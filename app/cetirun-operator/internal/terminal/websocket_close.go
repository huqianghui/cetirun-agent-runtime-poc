// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package terminal

import (
	"encoding/binary"
	"io"
)

// ttyd 1.7.7 can observe PTY EOF before waitpid records a successful exit and
// send reserved close code 1006. Its child has ended, so require an explicit
// reopen just as for the child's normal exit. A lost connection with no close
// frame still reaches the browser as 1006 and remains eligible for reconnection.
type terminalWebSocket struct {
	io.ReadWriteCloser
	prefix    []byte
	remaining uint64
}

func (s *terminalWebSocket) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}
	if len(s.prefix) == 0 && s.remaining == 0 {
		if err := s.readFramePrefix(); err != nil {
			return 0, err
		}
	}
	if len(s.prefix) > 0 {
		n := copy(buffer, s.prefix)
		s.prefix = s.prefix[n:]
		return n, nil
	}
	n, err := s.ReadWriteCloser.Read(buffer[:min(uint64(len(buffer)), s.remaining)])
	s.remaining -= uint64(n)
	return n, err
}

func (s *terminalWebSocket) readFramePrefix() error {
	var header [14]byte
	if _, err := io.ReadFull(s.ReadWriteCloser, header[:2]); err != nil {
		return err
	}
	length, size := uint64(header[1]&0x7f), 2
	switch length {
	case 126:
		size += 2
	case 127:
		size += 8
	}
	if _, err := io.ReadFull(s.ReadWriteCloser, header[2:size]); err != nil {
		return err
	}
	switch length {
	case 126:
		length = uint64(binary.BigEndian.Uint16(header[2:size]))
	case 127:
		length = binary.BigEndian.Uint64(header[2:size])
	}
	if header[1]&0x80 != 0 {
		if _, err := io.ReadFull(s.ReadWriteCloser, header[size:size+4]); err != nil {
			return err
		}
		size += 4
	}
	s.prefix = append(s.prefix[:0], header[:size]...)
	s.remaining = length
	// Only an unmasked, final control frame may contain a server close code.
	if header[0] == 0x88 && header[1] >= 2 && header[1] <= 125 {
		var code [2]byte
		if _, err := io.ReadFull(s.ReadWriteCloser, code[:]); err != nil {
			return err
		}
		if binary.BigEndian.Uint16(code[:]) == 1006 {
			binary.BigEndian.PutUint16(code[:], 1000)
		}
		s.prefix = append(s.prefix, code[:]...)
		s.remaining -= 2
	}
	return nil
}
