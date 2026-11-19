// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build !linux

package terminal

import "os"

// Runtime images are Linux; retain portable unit-test/build support.
func flushTerminalInput(int) error          { return nil }
func (g *InputGate) pumpFile(file *os.File) { g.pumpReader(file) }
