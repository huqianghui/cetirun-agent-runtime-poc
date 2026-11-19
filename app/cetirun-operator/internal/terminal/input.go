// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package terminal

import (
	"io"
	"os"
	"sync"

	"golang.org/x/term"
)

// InputGate discards input while there is no active exec. A replacement Pod
// must never receive buffered keystrokes or commands from a previous session.
type InputGate struct {
	mu     sync.Mutex
	writer *io.PipeWriter
	closed bool
	file   *os.File
}

func (g *InputGate) Bind(source io.Reader) {
	if file, ok := source.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		g.file = file
	}
}

func (g *InputGate) Pump(source io.Reader) {
	if g.file != nil {
		g.pumpFile(g.file)
		return
	}
	g.pumpReader(source)
}

func (g *InputGate) pumpReader(source io.Reader) {
	buffer := make([]byte, 4096)
	for {
		n, err := source.Read(buffer)
		g.mu.Lock()
		writer := g.writer
		g.mu.Unlock()
		if n > 0 && writer != nil {
			_, _ = writer.Write(buffer[:n])
		}
		if err != nil {
			g.mu.Lock()
			g.closed = true
			g.mu.Unlock()
			g.Stop()
			return
		}
	}
}

func (g *InputGate) Start() *io.PipeReader {
	g.Stop()
	reader, writer := io.Pipe()
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		_ = writer.Close()
		return reader
	}
	if g.file != nil {
		if err := flushTerminalInput(int(g.file.Fd())); err != nil {
			g.mu.Unlock()
			_ = writer.CloseWithError(err)
			return reader
		}
	}
	g.writer = writer
	g.mu.Unlock()
	return reader
}

func (g *InputGate) Stop() {
	g.mu.Lock()
	writer := g.writer
	g.writer = nil
	g.mu.Unlock()
	if writer != nil {
		_ = writer.Close()
	}
}

func (g *InputGate) Close() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
	g.Stop()
}
