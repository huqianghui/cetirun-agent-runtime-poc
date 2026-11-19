// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-memory-context/internal/memory"
)

func TestHelpWorksWithoutBindingsAndDocumentsPayloads(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"capture", "--help"}, {"install-assets", "--help"}, {"forget", "plan", "--help"}} {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), args, strings.NewReader(""), &out, &errOut); code != 0 {
			t.Fatalf("%v: %s", args, errOut.String())
		}
		for _, word := range []string{"expectedGeneration", "requestContext", "temporalClass", "queryTerms"} {
			if !strings.Contains(out.String(), word) {
				t.Fatalf("help missing %s", word)
			}
		}
	}
}

func TestCLIAndMCPUseConfiguredMemoryRoots(t *testing.T) {
	for _, relativeRoot := range []string{".cetirun/memory", ".hermes/memories/memory", "custom-store", "../external-store"} {
		t.Run(relativeRoot, func(t *testing.T) {
			home := t.TempDir()
			root := filepath.Join(home, relativeRoot)
			t.Setenv("HOME", home)
			t.Setenv("CETIRUN_MEMORY_ROOT", root)
			t.Setenv("CETIRUN_MEMORY_NO_MEMORY", "")
			zero := int64(0)
			request := memory.Request{ContractVersion: memory.ContractVersion, RequestID: "initialize", ExpectedGeneration: &zero, RequestContext: memory.RequestContext{Audience: "owner-private", Destination: memory.Destination{Kind: "owner-private"}, OpportunityID: "fixture-task", Scope: memory.Scope{"project": "fixture"}}, Payload: json.RawMessage(`{}`)}
			data, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			var out, errOut bytes.Buffer
			if code := run(context.Background(), []string{"init"}, bytes.NewReader(data), &out, &errOut); code != 0 {
				t.Fatalf("init: %d, %s, %s", code, out.String(), errOut.String())
			}
			if _, err := os.Stat(filepath.Join(root, "meta.json")); err != nil {
				t.Fatal("store was not created at configured root", err)
			}
			out.Reset()
			if code := run(context.Background(), []string{"status"}, bytes.NewReader(data), &out, &errOut); code != 0 || !strings.Contains(out.String(), `"initialized":true`) {
				t.Fatalf("status: %d, %s, %s", code, out.String(), errOut.String())
			}
			out.Reset()
			input := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"fixture"}}}` + "\n" +
				`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"cetirun_memory","arguments":{"command":"status","payload":{}}}}` + "\n"
			if code := run(context.Background(), []string{"mcp", "--stdio", "--owner-private"}, strings.NewReader(input), &out, &errOut); code != 0 {
				t.Fatalf("mcp: %d, %s", code, out.String())
			}
			var reply struct {
				Result struct {
					IsError bool `json:"isError"`
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"result"`
			}
			lines := strings.Split(strings.TrimSpace(out.String()), "\n")
			if len(lines) != 2 || json.Unmarshal([]byte(lines[1]), &reply) != nil || reply.Result.IsError || len(reply.Result.Content) != 1 || !strings.Contains(reply.Result.Content[0].Text, `"initialized":true`) {
				t.Fatalf("MCP did not use configured store: %s", out.String())
			}
		})
	}
}

func TestCLIAndMCPRejectInvalidRootBindings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, root := range []string{"", "relative/memory", "/", home + "/memory/../other", home + "/memory/"} {
		t.Run(root, func(t *testing.T) {
			t.Setenv("CETIRUN_MEMORY_ROOT", root)
			for _, args := range [][]string{{"status"}, {"mcp", "--stdio", "--owner-private"}} {
				var out, errOut bytes.Buffer
				if code := run(context.Background(), args, strings.NewReader(`{}`), &out, &errOut); code != 4 {
					t.Errorf("%v accepted invalid binding %q: %d, %s, %s", args, root, code, out.String(), errOut.String())
				}
			}
		})
	}
}

func TestRuntimeRestrictions(t *testing.T) {
	for _, mode := range []string{"no-memory", "no-capture"} {
		for _, source := range []string{"environment", "flag-before", "flag-after"} {
			t.Run(mode+"/"+source, func(t *testing.T) {
				home := t.TempDir()
				t.Chdir(home)
				t.Setenv("HOME", home)
				t.Setenv("CETIRUN_MEMORY_ROOT", filepath.Join(home, "memory"))
				t.Setenv("CETIRUN_MEMORY_NO_CAPTURE", "false")
				t.Setenv("CETIRUN_MEMORY_NO_MEMORY", "false")
				binding := memory.NativeBinding{Adapter: memory.AdapterVersion, Entrypoint: "hermes", NativeVersion: "fixture/1", SessionID: "fixture-session", OpportunityID: "fixture-turn", CWD: home, Audience: "owner-private", Destination: memory.Destination{Kind: "owner-private"}}
				invoke := func(args []string, input any) (int, string) {
					t.Helper()
					raw, err := json.Marshal(input)
					if err != nil {
						t.Fatal(err)
					}
					var out, errOut bytes.Buffer
					code := run(context.Background(), args, bytes.NewReader(raw), &out, &errOut)
					return code, out.String()
				}
				preference := memory.NativeEvent{Binding: binding, EventID: "preference", Event: "owner-preference", Payload: json.RawMessage(`{"message":"Remember my preference: CLI permission fixture"}`)}
				if code, out := invoke([]string{"native-event"}, preference); code != 0 {
					t.Fatalf("seed: %d %s", code, out)
				}
				if source == "environment" {
					t.Setenv("CETIRUN_MEMORY_"+strings.ToUpper(strings.ReplaceAll(mode, "-", "_")), "1")
				}
				arguments := func(args ...string) []string {
					if source == "flag-before" {
						return append([]string{"--" + mode}, args...)
					}
					if source == "flag-after" {
						return append(args, "--"+mode)
					}
					return args
				}
				wantReason := "CaptureProhibited"
				if mode == "no-memory" {
					wantReason = "MemoryDisabled"
				}
				if code, out := invoke(arguments("native-event"), preference); code != 4 || !strings.Contains(out, wantReason) {
					t.Fatalf("native write: %d %s", code, out)
				}
				query := memory.NativeEvent{Binding: binding, EventID: "read", Event: "tool", Command: "query", Payload: json.RawMessage(`{"queryTerms":["CLI permission fixture"]}`)}
				wantReadCode := 0
				if mode == "no-memory" {
					wantReadCode = 4
				}
				if code, out := invoke(arguments("native-event"), query); code != wantReadCode || (code == 0) != strings.Contains(out, "CLI permission fixture") {
					t.Fatalf("native read: %d %s", code, out)
				}
				zero := int64(0)
				request := memory.Request{ContractVersion: memory.ContractVersion, RequestID: "direct-read", ExpectedGeneration: &zero, RequestContext: memory.RequestContext{Audience: "owner-private", Destination: memory.Destination{Kind: "owner-private"}, Scope: memory.Scope{"project": home}}, Payload: query.Payload}
				if code, out := invoke(arguments("query"), request); code != wantReadCode {
					t.Fatalf("direct read: %d %s", code, out)
				}
				request.Payload = json.RawMessage(`{}`)
				if code, out := invoke(arguments("init"), request); code != 4 || !strings.Contains(out, wantReason) {
					t.Fatalf("direct write: %d %s", code, out)
				}
				legacy := memory.NativeInput{Platform: "cli", CWD: home, UserMessage: "Remember my preference: forbidden legacy write", SessionID: "legacy"}
				if code, out := invoke(arguments("native-context", "hermes"), legacy); code != 0 || strings.Contains(out, "forbidden legacy write") || (mode == "no-capture") != strings.Contains(out, "CLI permission fixture") {
					t.Fatalf("legacy context: %d %s", code, out)
				}
				// MCP metadata cannot override the process's restrictions with false.
				input := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"fixture"}}}` + "\n" +
					`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"cetirun_memory","arguments":{"command":"capture","payload":{}},"_meta":{"cetirun/noCapture":false,"cetirun/noMemory":false}}}` + "\n"
				var out, errOut bytes.Buffer
				if code := run(context.Background(), arguments("mcp", "--stdio", "--owner-private"), strings.NewReader(input), &out, &errOut); code != 0 || !strings.Contains(out.String(), wantReason) {
					t.Fatalf("MCP write: %d %s %s", code, out.String(), errOut.String())
				}
			})
		}
	}
}

func TestRestrictionInputsCannotSilentlyEnableMemory(t *testing.T) {
	t.Setenv("CETIRUN_MEMORY_NO_CAPTURE", "false")
	t.Setenv("CETIRUN_MEMORY_NO_MEMORY", "false")
	for _, name := range []string{"CETIRUN_MEMORY_NO_CAPTURE", "CETIRUN_MEMORY_NO_MEMORY"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "tru")
			var out, errOut bytes.Buffer
			if code := run(context.Background(), []string{"native-event"}, strings.NewReader(`{}`), &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "MemoryRestrictionsInvalid") || out.Len() != 0 {
				t.Fatalf("invalid restriction accepted: %d %s %s", code, out.String(), errOut.String())
			}
		})
	}
	t.Setenv("CETIRUN_MEMORY_NO_MEMORY", "true")
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"native-event", "--no-memory=false"}, strings.NewReader(`{}`), &out, &errOut); code != 2 {
		t.Fatalf("restriction reset accepted: %d %s %s", code, out.String(), errOut.String())
	}
}

func TestCustomMemoryRootRejectsSymlink(t *testing.T) {
	home := t.TempDir()
	target := t.TempDir()
	root := filepath.Join(home, "memory")
	if err := os.Symlink(target, root); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CETIRUN_MEMORY_ROOT", root)
	var out, errOut bytes.Buffer
	input := fmt.Sprintf(`{"contractVersion":%q,"requestId":"initialize","expectedGeneration":0,"requestContext":{"audience":"owner-private","destination":{"kind":"owner-private"},"opportunityId":"fixture-task","scope":{"project":"fixture"}},"payload":{}}`, memory.ContractVersion)
	if code := run(context.Background(), []string{"init"}, strings.NewReader(input), &out, &errOut); code != 8 || !strings.Contains(out.String(), `"reason":"StorageError"`) {
		t.Fatalf("symlink root: code %d, %s, %s", code, out.String(), errOut.String())
	}
	entries, err := os.ReadDir(target)
	if err != nil || len(entries) != 0 {
		t.Fatal("symlink target was changed", err)
	}
}

func TestMCPInitializeAcceptsNewerClientInfo(t *testing.T) {
	store := memory.New(memory.Config{Root: t.TempDir()})
	input := strings.Join([]string{
		`{"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"roots":{"listChanged":true},"elicitation":{"form":{},"url":{}}},"clientInfo":{"name":"example-client","title":"Example","version":"9.1.0","description":"A synthetic MCP client","websiteUrl":"https://example.com/client"}},"jsonrpc":"2.0","id":0}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"capabilities":{}}}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	if code := runMCP(context.Background(), strings.NewReader(input), &out, store, memory.NativeBinding{}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	replies := map[string]map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var reply map[string]any
		if err := json.Unmarshal([]byte(line), &reply); err != nil {
			t.Fatal(err)
		}
		replies[fmt.Sprint(reply["id"])] = reply
	}
	if replies["0"]["result"] == nil || replies["1"]["result"] == nil {
		t.Fatalf("newer client could not initialize: %s", out.String())
	}
	if replies["2"]["error"] == nil {
		t.Fatalf("initialize without a protocol version or client was accepted: %s", out.String())
	}
}

func TestMCPDescribesMemoryPayloads(t *testing.T) {
	store := memory.New(memory.Config{Root: t.TempDir()})
	var out bytes.Buffer
	input := "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\"}\n"
	if code := runMCP(context.Background(), strings.NewReader(input), &out, store, memory.NativeBinding{}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var reply struct {
		Result struct {
			Tools []struct {
				InputSchema struct {
					Properties struct {
						Payload struct {
							Properties map[string]json.RawMessage `json:"properties"`
						} `json:"payload"`
					} `json:"properties"`
				} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	if len(reply.Result.Tools) != 1 {
		t.Fatalf("unexpected tool list: %s", out.String())
	}
	properties := reply.Result.Tools[0].InputSchema.Properties.Payload.Properties
	for _, field := range []string{"queryTerms", "note", "observation", "procedure", "ref"} {
		if len(properties[field]) == 0 {
			t.Fatalf("tool payload does not describe %s", field)
		}
	}
}
