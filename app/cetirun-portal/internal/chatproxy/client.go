// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package chatproxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Client struct{ HTTP *http.Client }

type idleConn struct{ net.Conn }

func (c idleConn) Read(p []byte) (int, error) {
	_ = c.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	return c.Conn.Read(p)
}

func NewClient() *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DisableCompression = true
	transport.DisableKeepAlives = true
	transport.ResponseHeaderTimeout = 60 * time.Second
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return idleConn{conn}, nil
	}
	return &Client{HTTP: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (c *Client) do(ctx context.Context, target Target, method, route string, body io.Reader, memoryMode string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target.BaseURL+route, body)
	if err != nil {
		return nil, ErrProtocol
	}
	req.GetBody = nil
	req.Header.Set("Accept-Encoding", "identity")
	if method == http.MethodPost {
		// Extra hint only; the owned mesh route enforces zero retries.
		req.Header.Set("X-Envoy-Max-Retries", "0")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream, application/json")
		if memoryMode == "off" || memoryMode == "no-capture" {
			req.Header.Set("X-CetiRun-Memory-Mode", memoryMode)
		}
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	return res, nil
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, ErrProtocol
	}
	return raw, nil
}

func (c *Client) Models(ctx context.Context, target Target) (Models, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res, err := c.do(ctx, target, http.MethodGet, "/v1/models", nil, "")
	if err != nil {
		return Models{}, err
	}
	defer res.Body.Close()
	media, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if res.StatusCode != 200 || media != "application/json" || res.Header.Get("Content-Encoding") != "" {
		return Models{}, ErrProtocol
	}
	raw, err := readBounded(res.Body, ModelBytes)
	if err != nil {
		return Models{}, err
	}
	return DecodeModels(raw)
}

func Error(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func (c *Client) Complete(ctx context.Context, w http.ResponseWriter, target Target, request Request, requestID string) {
	raw, _ := json.Marshal(request)
	w.Header().Set("X-CetiRun-Chat-Dispatch", "started")
	res, err := c.do(ctx, target, http.MethodPost, "/v1/chat/completions", strings.NewReader(string(raw)), request.MemoryMode)
	if err != nil {
		Error(w, 502, "agent_request_failed", "The connection ended. The Agent may have executed this request.")
		return
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		status, code := 502, "agent_reported_error"
		if res.StatusCode == 429 {
			status, code = 429, "chat_busy"
		}
		Error(w, status, code, "The Agent did not complete this request. It will not be retried.")
		return
	}
	media, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if res.Header.Get("Content-Encoding") != "" {
		Error(w, 502, "agent_protocol_error", ErrProtocol.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if !request.Stream {
		if media != "application/json" {
			Error(w, 502, "agent_protocol_error", ErrProtocol.Error())
			return
		}
		body, err := readBounded(res.Body, OutputBytes)
		if err != nil {
			Error(w, 502, "agent_protocol_error", ErrProtocol.Error())
			return
		}
		result, _, err := normalize(body, "chatcmpl-"+requestID, request.Model, false)
		if err != nil {
			code := "agent_protocol_error"
			if errors.Is(err, ErrAgent) {
				code = "agent_reported_error"
			}
			Error(w, 502, code, "The Agent did not return a completed response.")
			return
		}
		encoded, _ := json.Marshal(result)
		if len(encoded) > OutputBytes {
			Error(w, 502, "agent_protocol_error", ErrProtocol.Error())
			return
		}
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(15 * time.Second))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(encoded)
		return
	}
	if media != "text/event-stream" {
		Error(w, 502, "agent_protocol_error", ErrProtocol.Error())
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	writer := &eventWriter{w: w}
	if writer.write(": connected\n\n") != nil {
		return
	}
	if err := stream(res.Body, writer, requestID, request.Model); err != nil {
		code := "agent_stream_interrupted"
		message := "The response did not complete. The Agent may still be running."
		if errors.Is(err, ErrAgent) {
			code = "agent_reported_error"
			message = "The Agent reported a failed response. Earlier tool actions may have completed; the request will not be replayed."
		}
		_ = writer.write("data: {\"error\":{\"code\":\"" + code + "\",\"message\":\"" + message + "\"}}\n\n")
	}
}

type eventWriter struct {
	w     http.ResponseWriter
	bytes int
}

func (w *eventWriter) write(value string) error {
	if w.bytes+len(value) > OutputBytes {
		return ErrProtocol
	}
	controller := http.NewResponseController(w.w)
	_ = controller.SetWriteDeadline(time.Now().Add(15 * time.Second))
	if _, err := io.WriteString(w.w, value); err != nil {
		return err
	}
	w.bytes += len(value)
	return controller.Flush()
}

func stream(reader io.Reader, writer *eventWriter, id, model string) error {
	bounded := &io.LimitedReader{R: reader, N: OutputBytes + 1}
	scanner := bufio.NewScanner(bounded)
	scanner.Buffer(make([]byte, 4096), FrameBytes)
	scanner.Split(splitEventLine)
	var data []string
	size := 0
	finished := false
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		size += len(line) + 1
		if size > FrameBytes || bounded.N <= 0 {
			return ErrProtocol
		}
		if line != "" {
			if strings.HasPrefix(line, "data:") {
				data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			}
			continue
		}
		size = 0
		if len(data) == 0 {
			if err := writer.write(": keepalive\n\n"); err != nil {
				return err
			}
			continue
		}
		payload := strings.Join(data, "\n")
		data = nil
		if payload == "[DONE]" {
			if !finished {
				return ErrProtocol
			}
			return writer.write("data: [DONE]\n\n")
		}
		value, ignored, err := normalize([]byte(payload), "chatcmpl-"+id, model, true)
		if err != nil {
			return err
		}
		if ignored {
			continue
		}
		if finished {
			return ErrProtocol
		}
		if value.Choices[0].FinishReason != nil {
			finished = true
		}
		raw, _ := json.Marshal(value)
		if err := writer.write("data: " + string(raw) + "\n\n"); err != nil {
			return err
		}
	}
	return fmt.Errorf("incomplete Agent stream")
}

func splitEventLine(data []byte, atEOF bool) (int, []byte, error) {
	for i, b := range data {
		if b == '\n' {
			return i + 1, data[:i], nil
		}
		if b == '\r' {
			if i+1 == len(data) && !atEOF {
				return 0, nil, nil
			}
			advance := i + 1
			if advance < len(data) && data[advance] == '\n' {
				advance++
			}
			return advance, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

type Limiter struct {
	mu            sync.Mutex
	total         int
	users, agents map[string]int
}

func (l *Limiter) Acquire(user, agent string) (func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.users == nil {
		l.users = map[string]int{}
		l.agents = map[string]int{}
	}
	if l.total >= 16 || l.users[user] >= 4 || l.agents[agent] >= 1 {
		return nil, false
	}
	l.total++
	l.users[user]++
	l.agents[agent]++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.total--
			l.users[user]--
			l.agents[agent]--
			if l.users[user] == 0 {
				delete(l.users, user)
			}
			if l.agents[agent] == 0 {
				delete(l.agents, agent)
			}
		})
	}, true
}
