// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package terminal

import (
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

func TestPTYInputIsNotReplayedAfterReconnect(t *testing.T) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = master.Close() })
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	number, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	state, err := term.MakeRaw(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = term.Restore(int(slave.Fd()), state) })
	gate := &InputGate{}
	gate.Bind(slave)
	go gate.Pump(slave)
	t.Cleanup(gate.Close)

	for i := 0; i < 10; i++ {
		gate.Stop()
		if _, err := master.Write([]byte("discard-this-command\n")); err != nil {
			t.Fatal(err)
		}
		reader := gate.Start()
		if _, err := master.Write([]byte("new")); err != nil {
			t.Fatal(err)
		}
		result := make(chan string, 1)
		go func() {
			data := make([]byte, 3)
			_, err := io.ReadFull(reader, data)
			if err != nil {
				result <- "read failed"
				return
			}
			result <- string(data)
		}()
		select {
		case value := <-result:
			if value != "new" {
				t.Fatalf("replayed input: %q", value)
			}
		case <-time.After(time.Second):
			_ = reader.Close()
			t.Fatal("new terminal did not receive input")
		}
		_ = reader.Close()
	}
}
