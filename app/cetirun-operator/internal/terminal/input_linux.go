// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package terminal

import (
	"os"

	"golang.org/x/sys/unix"
)

func flushTerminalInput(fd int) error {
	return unix.IoctlSetInt(fd, unix.TCFLSH, unix.TCIFLUSH)
}

func (g *InputGate) pumpFile(file *os.File) {
	fd := int(file.Fd())
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	buffer := make([]byte, 4096)
	for {
		if _, err := unix.Poll(poll, 100); err != nil {
			if err == unix.EINTR {
				continue
			}
			break
		}
		g.mu.Lock()
		if g.closed {
			g.mu.Unlock()
			return
		}
		// Start can flush input after the first poll. Recheck under the same
		// lock before reading, so a blocking read cannot hold up a reconnect.
		ready, err := unix.Poll(poll, 0)
		if err != nil || ready == 0 {
			g.mu.Unlock()
			continue
		}
		n, err := unix.Read(fd, buffer)
		writer := g.writer
		g.mu.Unlock()
		if n > 0 && writer != nil {
			_, _ = writer.Write(buffer[:n])
		}
		if err == unix.EINTR || err == unix.EAGAIN {
			continue
		}
		if err != nil || n == 0 {
			break
		}
	}
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
	g.Stop()
}
