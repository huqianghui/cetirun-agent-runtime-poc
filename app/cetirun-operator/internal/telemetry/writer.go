// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package telemetry

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// Writer writes records as JSON lines. It is safe for concurrent use: each
// record is one write of one line, so lines never interleave.
type Writer struct {
	mu  sync.Mutex
	out io.Writer
	now func() time.Time
}

// NewWriter returns a Writer to out.
func NewWriter(out io.Writer) *Writer {
	return &Writer{out: out, now: time.Now}
}

// Write stamps the record with the log type, schema version and current time,
// and writes it as one line.
func (w *Writer) Write(logType string, record Record) error {
	record.LogType = logType
	record.SchemaVersion = SchemaVersion
	record.Time = w.now().UTC().Format(time.RFC3339Nano)
	line, err := json.Marshal(record)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err = w.out.Write(line)
	return err
}
