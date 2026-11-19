// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package terminal

import (
	"bytes"
	"encoding/binary"
	"io"
	"net/http"
	"testing"
	"testing/iotest"
)

type testWebSocketStream struct {
	io.Reader
	io.Writer
	closed bool
}

func (s *testWebSocketStream) Close() error {
	s.closed = true
	return nil
}

func TestTerminalWebSocketNormalizesOnlyReservedChildClose(t *testing.T) {
	for _, test := range []struct {
		name     string
		input    []byte
		expected []byte
	}{
		{"reserved close", []byte{0x88, 2, 0x03, 0xee}, []byte{0x88, 2, 0x03, 0xe8}},
		{"normal close", []byte{0x88, 2, 0x03, 0xe8}, []byte{0x88, 2, 0x03, 0xe8}},
		{"policy close", []byte{0x88, 2, 0x03, 0xf0}, []byte{0x88, 2, 0x03, 0xf0}},
		{"terminal output", []byte{0x82, 4, 0x88, 2, 0x03, 0xee}, []byte{0x82, 4, 0x88, 2, 0x03, 0xee}},
		{"fragment", []byte{0x02, 2, 0x03, 0xee, 0x80, 0}, []byte{0x02, 2, 0x03, 0xee, 0x80, 0}},
		{"ping", []byte{0x89, 2, 0x03, 0xee}, []byte{0x89, 2, 0x03, 0xee}},
		{"close reason", []byte{0x88, 5, 0x03, 0xee, 'e', 'n', 'd'}, []byte{0x88, 5, 0x03, 0xe8, 'e', 'n', 'd'}},
	} {
		t.Run(test.name, func(t *testing.T) {
			written := &bytes.Buffer{}
			source := &testWebSocketStream{Reader: iotest.OneByteReader(bytes.NewReader(test.input)), Writer: written}
			stream := &terminalWebSocket{ReadWriteCloser: source}
			if n, err := stream.Read(nil); n != 0 || err != nil {
				t.Fatal("zero-length read consumed a frame")
			}
			got, err := io.ReadAll(stream)
			if err != nil || !bytes.Equal(got, test.expected) {
				t.Fatalf("got %x, %v; want %x", got, err, test.expected)
			}
			if _, err := stream.Write([]byte("client input")); err != nil || written.String() != "client input" {
				t.Fatal("browser input was changed")
			}
			if err := stream.Close(); err != nil || !source.closed {
				t.Fatal("upstream was not closed")
			}
		})
	}
}

func TestTerminalWebSocketStreamsExtendedFrames(t *testing.T) {
	for _, size := range []int{126, 65536} {
		var header [10]byte
		header[0] = 0x82
		headerSize := 4
		if size < 65536 {
			header[1] = 126
			binary.BigEndian.PutUint16(header[2:4], uint16(size))
		} else {
			header[1] = 127
			binary.BigEndian.PutUint64(header[2:10], uint64(size))
			headerSize = 10
		}
		frame := append(header[:headerSize], bytes.Repeat([]byte{0x88}, size)...)
		input := append(bytes.Clone(frame), 0x88, 2, 0x03, 0xee)
		source := &testWebSocketStream{Reader: bytes.NewReader(input), Writer: io.Discard}
		got, err := io.ReadAll(&terminalWebSocket{ReadWriteCloser: source})
		want := append(frame, 0x88, 2, 0x03, 0xe8)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("extended frame of length %d was not preserved: %v", size, err)
		}
	}
}

func TestTerminalProxyNormalizesTheUpgradedStream(t *testing.T) {
	source := &testWebSocketStream{Reader: bytes.NewReader([]byte{0x88, 2, 0x03, 0xee}), Writer: io.Discard}
	response := &http.Response{StatusCode: http.StatusSwitchingProtocols,
		Header: http.Header{"Upgrade": {"websocket"}}, Body: source}
	server := NewServer(testConfig(), nil, "/unused-ttyd.sock")
	if err := server.Proxy.ModifyResponse(response); err != nil {
		t.Fatal(err)
	}
	if _, ok := response.Body.(io.ReadWriteCloser); !ok {
		t.Fatal("upgrade lost its duplex stream")
	}
	got, err := io.ReadAll(response.Body)
	if err != nil || !bytes.Equal(got, []byte{0x88, 2, 0x03, 0xe8}) {
		t.Fatalf("proxy did not normalize the child close: %x, %v", got, err)
	}
}
