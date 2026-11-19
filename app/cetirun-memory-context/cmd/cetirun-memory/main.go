// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/Azure/CetiRun/app/cetirun-memory-context/assets"
	"github.com/Azure/CetiRun/app/cetirun-memory-context/internal/memory"
)

var version = "dev"
var commit = "unknown"

func validMemoryRoot(root string) bool {
	return filepath.IsAbs(root) && filepath.Clean(root) == root && root != "/"
}

func run(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) int {
	if slices.Contains(args, "--help") || len(args) == 0 {
		fmt.Fprintln(out, "Usage: cetirun-memory [--no-memory] [--no-capture] COMMAND < request.json\nCommands: status init query capture revise prepare commit propose check advance select retire record-source-check record-use trace lint repair-index forget plan|apply import plan|apply\nAsset installer: install-assets DIRECTORY\nNative adapter: native-event (memory-adapter/v1)\nOwner-private MCP: mcp --stdio --owner-private\nLegacy context adapter: native-context hermes|pi\nRestrictions: --no-memory / CETIRUN_MEMORY_NO_MEMORY; --no-capture / CETIRUN_MEMORY_NO_CAPTURE. Environment values: true/false or 1/0. Restrictions are additive; request payloads cannot relax them.\nBinding: CETIRUN_MEMORY_ROOT (explicit canonical absolute path). Import sources: files under the managed root's HOME, or process HOME for other layouts.")
		guide, err := assets.Files.ReadFile("skills/cetirun-memory/references/requests.md")
		if err != nil {
			return 8
		}
		_, err = out.Write(guide)
		if err != nil {
			return 8
		}
		return 0
	}
	noCapture, noMemory := false, false
	for _, option := range []struct {
		name  string
		value *bool
	}{{"CETIRUN_MEMORY_NO_CAPTURE", &noCapture}, {"CETIRUN_MEMORY_NO_MEMORY", &noMemory}} {
		switch strings.ToLower(os.Getenv(option.name)) {
		case "", "false", "0":
		case "true", "1":
			*option.value = true
		default:
			fmt.Fprintln(errOut, "MemoryRestrictionsInvalid")
			return 2
		}
	}
	commandsWithOptions := args
	args = nil
	for _, arg := range commandsWithOptions {
		switch arg {
		case "--no-capture":
			noCapture = true
		case "--no-memory":
			noMemory = true
		default:
			args = append(args, arg)
		}
	}
	if len(args) == 2 && args[0] == "native-context" && (args[1] == "hermes" || args[1] == "pi") {
		data, err := io.ReadAll(io.LimitReader(in, memory.MaxInput+1))
		if err != nil || len(data) > memory.MaxInput {
			return 2
		}
		if noCapture || noMemory {
			var input memory.NativeInput
			if json.Unmarshal(data, &input) != nil {
				return 2
			}
			input.NoCapture = input.NoCapture || noCapture
			input.NoMemory = input.NoMemory || noMemory
			data, err = json.Marshal(input)
			if err != nil {
				return 2
			}
		}
		canonical, _ := assets.Files.ReadFile("AGENTS.md")
		store := memory.New(memory.Config{Root: os.Getenv("CETIRUN_MEMORY_ROOT")})
		text := store.NativeContext(ctx, args[1], data, string(canonical))
		if len(text) > 8192 {
			return 2
		}
		if err := json.NewEncoder(out).Encode(map[string]string{"context": text}); err != nil {
			return 8
		}
		return 0
	}
	if len(args) == 1 && args[0] == "native-event" {
		data, err := io.ReadAll(io.LimitReader(in, memory.MaxInput+1))
		if err != nil || len(data) > memory.MaxInput {
			return 2
		}
		var event memory.NativeEvent
		if memory.Decode(data, &event) != nil {
			return 2
		}
		event.Binding.NoCapture = event.Binding.NoCapture || noCapture
		event.Binding.NoMemory = event.Binding.NoMemory || noMemory
		canonical, _ := assets.Files.ReadFile("AGENTS.md")
		store := memory.New(memory.Config{Root: os.Getenv("CETIRUN_MEMORY_ROOT")})
		response, code := store.NativeEvent(ctx, event, string(canonical))
		if err := json.NewEncoder(out).Encode(response); err != nil {
			return 8
		}
		return code
	}
	if slices.Equal(args, []string{"mcp", "--stdio", "--owner-private"}) {
		root := os.Getenv("CETIRUN_MEMORY_ROOT")
		cwd, err := os.Getwd()
		if err != nil || !validMemoryRoot(root) {
			return 4
		}
		id := rand.Text()
		binding := memory.NativeBinding{Adapter: memory.AdapterVersion, Entrypoint: "custom", NativeVersion: "mcp/2025-06-18", SessionID: id, OpportunityID: id, CWD: cwd, Audience: "owner-private", Destination: memory.Destination{Kind: "owner-private"}, NoCapture: noCapture, NoMemory: noMemory}
		return runMCP(ctx, in, out, memory.New(memory.Config{Root: root}), binding)
	}
	if len(args) == 2 && args[0] == "install-assets" {
		executable, err := os.Executable()
		if err != nil {
			return 8
		}
		binary, err := os.ReadFile(executable)
		if err != nil {
			return 8
		}
		if err := memory.InstallAssets(args[1], assets.Files, binary); err != nil {
			fmt.Fprintln(errOut, "AssetInstallationFailed")
			return 8
		}
		return 0
	}
	commands := []string{"status", "init", "query", "capture", "prepare", "commit", "propose", "check", "advance", "select", "lint", "repair-index", "revise", "retire", "record-source-check", "record-use", "trace", "forget-plan", "forget-apply", "import-plan", "import-apply"}
	if len(args) == 1 && args[0] == "--version" {
		fmt.Fprintf(out, "%s %s %s ABI %d\n", version, commit, memory.ContractVersion, memory.HelperABI)
		return 0
	}
	if len(args) == 2 && (args[0] == "forget" || args[0] == "import") {
		args = []string{args[0] + "-" + args[1]}
	}
	if len(args) != 1 || !slices.Contains(commands, args[0]) {
		fmt.Fprintln(errOut, "InvalidCommand")
		return 2
	}
	data, err := io.ReadAll(io.LimitReader(in, memory.MaxInput+1))
	if err != nil || len(data) > memory.MaxInput {
		fmt.Fprintln(errOut, "InvalidInput")
		return 2
	}
	var request memory.Request
	if memory.Decode(data, &request) != nil {
		fmt.Fprintln(errOut, "InvalidInput")
		return 2
	}
	request.RequestContext.NoCapture = request.RequestContext.NoCapture || noCapture
	request.RequestContext.NoMemory = request.RequestContext.NoMemory || noMemory
	root := os.Getenv("CETIRUN_MEMORY_ROOT")
	if !validMemoryRoot(root) {
		fmt.Fprintln(errOut, "BindingUnavailable")
		return 4
	}
	store := memory.New(memory.Config{Root: root})
	response, code := store.Execute(ctx, args[0], request)
	if err := json.NewEncoder(out).Encode(response); err != nil {
		return 8
	}
	return code
}
func main() {
	syscall.Umask(0077)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
