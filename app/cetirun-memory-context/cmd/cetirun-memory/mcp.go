// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/Azure/CetiRun/app/cetirun-memory-context/assets"
	"github.com/Azure/CetiRun/app/cetirun-memory-context/internal/memory"
)

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Stdio is an explicit owner-private client configuration. It exposes only the
// fixed file operations, never sampling, network access or model credentials.
func runMCP(ctx context.Context, in io.Reader, out io.Writer, store *memory.Store, binding memory.NativeBinding) int {
	done := make(chan struct{})
	defer close(done)
	if input, ok := in.(io.Closer); ok {
		go func() {
			select {
			case <-ctx.Done():
				_ = input.Close()
			case <-done:
			}
		}()
	}
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 4096), memory.MaxInput)
	var outputMu, pendingMu sync.Mutex
	var workers sync.WaitGroup
	pending := map[string]context.CancelFunc{}
	write := func(id json.RawMessage, result any, code int) {
		outputMu.Lock()
		defer outputMu.Unlock()
		message := map[string]any{"jsonrpc": "2.0", "id": id}
		if code != 0 {
			message["error"] = map[string]any{"code": code, "message": "Invalid or unavailable Memory request"}
		} else {
			message["result"] = result
		}
		_ = json.NewEncoder(out).Encode(message)
	}
	defer workers.Wait()
	initialized := false
	canonical, _ := assets.Files.ReadFile("AGENTS.md")
	payloadSchema, err := assets.Files.ReadFile("adapters/common/payload-schema.json")
	if err != nil || !json.Valid(payloadSchema) {
		write(json.RawMessage(`null`), nil, -32603)
		return 6
	}
	for scanner.Scan() {
		var message rpcMessage
		if memory.Decode(scanner.Bytes(), &message) != nil || message.JSONRPC != "2.0" {
			write(json.RawMessage(`null`), nil, -32600)
			continue
		}
		if ctx.Err() != nil {
			return 5
		}
		if message.Method == "notifications/cancelled" {
			var cancel struct {
				RequestID json.RawMessage `json:"requestId"`
				Reason    string          `json:"reason,omitempty"`
			}
			if memory.Decode(message.Params, &cancel) == nil {
				pendingMu.Lock()
				if stop := pending[string(cancel.RequestID)]; stop != nil {
					stop()
				}
				pendingMu.Unlock()
			}
			continue
		}
		if len(message.ID) == 0 {
			continue
		}
		switch message.Method {
		case "initialize":
			// Newer protocol revisions add descriptive client fields, such as
			// clientInfo.description and websiteUrl in 2025-11-25. Initialization
			// carries no authority, so fields beyond the version and client
			// name are ignored; tool calls keep strict decoding.
			var params struct {
				Protocol   string `json:"protocolVersion"`
				ClientInfo struct {
					Name string `json:"name"`
				} `json:"clientInfo"`
			}
			if json.Unmarshal(message.Params, &params) != nil || params.Protocol == "" || params.ClientInfo.Name == "" {
				write(message.ID, nil, -32602)
				continue
			}
			initialized = true
			write(message.ID, map[string]any{"protocolVersion": "2025-06-18", "serverInfo": map[string]string{"name": "cetirun-memory", "version": version}, "capabilities": map[string]any{"tools": map[string]any{}, "resources": map[string]any{}}, "instructions": string(canonical)}, 0)
		case "ping":
			write(message.ID, map[string]any{}, 0)
		case "tools/list":
			write(message.ID, map[string]any{"tools": []any{map[string]any{"name": "cetirun_memory", "description": "Agent-private Memory operations: look up earlier conversations and save what is worth keeping. Read the cetirun-memory skill. Do not use from shared contexts; request-level noCapture/noMemory restrictions remain in force. This stdio adapter does not execute model evaluations.", "inputSchema": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"command": map[string]any{"type": "string", "enum": []string{"status", "query", "capture", "prepare", "commit", "propose", "check", "advance", "select", "retire", "revise", "record-source-check", "record-use", "trace", "lint", "source"}}, "payload": json.RawMessage(payloadSchema)}, "required": []string{"command", "payload"}}}}}, 0)
		case "resources/list":
			write(message.ID, map[string]any{"resources": []any{map[string]string{"uri": "cetirun-memory://contract", "name": "Memory contract", "mimeType": "text/markdown"}}}, 0)
		case "resources/read":
			var params struct {
				URI string `json:"uri"`
			}
			if memory.Decode(message.Params, &params) != nil || params.URI != "cetirun-memory://contract" {
				write(message.ID, nil, -32602)
				continue
			}
			write(message.ID, map[string]any{"contents": []any{map[string]string{"uri": params.URI, "mimeType": "text/markdown", "text": string(canonical)}}}, 0)
		case "tools/call":
			var params struct {
				Name      string `json:"name"`
				Arguments struct {
					Command string          `json:"command"`
					Payload json.RawMessage `json:"payload"`
				} `json:"arguments"`
				Meta map[string]json.RawMessage `json:"_meta,omitempty"`
			}
			if !initialized || memory.Decode(message.Params, &params) != nil || params.Name != "cetirun_memory" {
				write(message.ID, nil, -32602)
				continue
			}
			bound := binding
			invalid := false
			for key, value := range params.Meta {
				if key == "cetirun/noCapture" || key == "cetirun/noMemory" {
					var flag bool
					if memory.Decode(value, &flag) != nil {
						invalid = true
						break
					}
					if key == "cetirun/noCapture" {
						bound.NoCapture = bound.NoCapture || flag
					} else {
						bound.NoMemory = bound.NoMemory || flag
					}
				}
			}
			if invalid {
				write(message.ID, nil, -32602)
				continue
			}
			pendingMu.Lock()
			key := string(message.ID)
			if len(pending) >= 8 || pending[key] != nil {
				pendingMu.Unlock()
				write(message.ID, nil, -32600)
				continue
			}
			callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			pending[key] = cancel
			pendingMu.Unlock()
			workers.Add(1)
			go func(id json.RawMessage, command string, payload json.RawMessage, bound memory.NativeBinding) {
				defer workers.Done()
				defer cancel()
				defer func() { pendingMu.Lock(); delete(pending, string(id)); pendingMu.Unlock() }()
				response, _ := store.NativeEvent(callCtx, memory.NativeEvent{Binding: bound, Event: "tool", Command: command, Payload: payload, EventID: "mcp-" + string(id)}, string(canonical))
				body, _ := json.Marshal(response)
				write(id, map[string]any{"content": []any{map[string]string{"type": "text", "text": string(body)}}, "isError": !response.OK}, 0)
			}(message.ID, params.Arguments.Command, params.Arguments.Payload, bound)
		default:
			write(message.ID, nil, -32601)
		}
	}
	if scanner.Err() != nil {
		return 2
	}
	return 0
}
