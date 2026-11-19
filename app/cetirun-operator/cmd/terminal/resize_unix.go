// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// notifyResize delivers the PTY's window-size changes to changes.
func notifyResize(changes chan<- os.Signal) {
	signal.Notify(changes, syscall.SIGWINCH)
}
