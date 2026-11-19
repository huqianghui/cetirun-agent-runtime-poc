// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build windows

package main

import "os"

// notifyResize does nothing: Windows has no SIGWINCH. The terminal runs only
// in the Linux image; this lets the package build and its tests run on a
// Windows development machine.
func notifyResize(chan<- os.Signal) {}
