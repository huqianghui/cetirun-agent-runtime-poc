// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package egresscapture

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

type fakeTable struct {
	Chain  bool
	Rules  []string
	Output []string
}

type fakeNetfilter struct {
	Tables    map[string]fakeTable
	Mutations int
}

func newFakeNetfilter() fakeNetfilter {
	return fakeNetfilter{Tables: map[string]fakeTable{
		"iptables":  {Output: []string{"-A OUTPUT -d 192.0.2.1/32 -j RETURN"}},
		"ip6tables": {Output: []string{"-A OUTPUT -d 2001:db8::1/128 -j RETURN"}},
	}}
}

// A subprocess of the test executable implements only the netfilter operations
// used by the generated script. Faults can occur after a mutation was committed.
func TestCaptureNetfilterProcess(t *testing.T) {
	if os.Getenv("CETIRUN_FAKE_NETFILTER") != "1" {
		t.Skip("subprocess helper")
	}
	tool := os.Getenv("CETIRUN_FAKE_TOOL")
	if tool == "curl" {
		os.Exit(0)
	}
	index := slices.Index(os.Args, "--")
	if index < 0 {
		os.Exit(80)
	}
	args := os.Args[index+1:]
	if len(args) < 5 || !slices.Equal(args[:4], []string{"-w", "5", "-t", "nat"}) {
		os.Exit(81)
	}
	file := os.Getenv("CETIRUN_FAKE_STATE")
	var state fakeNetfilter
	data, err := os.ReadFile(file)
	if err != nil || json.Unmarshal(data, &state) != nil {
		os.Exit(82)
	}
	table := state.Tables[tool]
	args = args[4:]
	switch args[0] {
	case "-S":
		if os.Getenv("CETIRUN_FAIL_READ") == "1" {
			os.Exit(23)
		}
		fmt.Println("-P OUTPUT ACCEPT")
		fmt.Println("-N CNI_UNRELATED")
		fmt.Println("-A CNI_UNRELATED -j RETURN")
		if table.Chain {
			fmt.Println("-N " + captureChain)
		}
		for _, line := range append(slices.Clone(table.Rules), table.Output...) {
			fmt.Println(line)
		}
		os.Exit(0)
	case "-N":
		if len(args) != 2 || args[1] != captureChain || table.Chain {
			os.Exit(24)
		}
		table.Chain = true
	case "-A":
		if len(args) < 3 || !table.Chain {
			os.Exit(25)
		}
		line := strings.Join(args, " ")
		switch args[1] {
		case captureChain:
			table.Rules = append(table.Rules, line)
		case "OUTPUT":
			table.Output = append(table.Output, line)
		default:
			os.Exit(26)
		}
	case "-D":
		if os.Getenv("CETIRUN_FAIL_DELETE") == "1" {
			os.Exit(27)
		}
		if len(args) < 3 || args[1] != "OUTPUT" {
			os.Exit(28)
		}
		line := "-A " + strings.Join(args[1:], " ")
		index := slices.Index(table.Output, line)
		if index < 0 {
			os.Exit(29)
		}
		table.Output = slices.Delete(table.Output, index, index+1)
	default:
		os.Exit(30)
	}
	state.Tables[tool] = table
	state.Mutations++
	data, err = json.Marshal(state)
	if err != nil || os.WriteFile(file, data, 0o600) != nil {
		os.Exit(31)
	}
	fail, _ := strconv.Atoi(os.Getenv("CETIRUN_FAIL_AFTER"))
	if fail == state.Mutations {
		os.Exit(32)
	}
	os.Exit(0)
}

type scriptFixture struct {
	dir, state, script, bin string
}

func newScriptFixture(t *testing.T, dual bool) scriptFixture {
	t.Helper()
	dir := t.TempDir()
	fixture := scriptFixture{dir: dir, state: filepath.Join(dir, "state.json"),
		script: filepath.Join(dir, "capture.sh"), bin: filepath.Join(dir, "bin")}
	if err := os.Mkdir(fixture.bin, 0o755); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"iptables", "ip6tables", "curl"} {
		body := "#!/bin/sh\nCETIRUN_FAKE_TOOL=" + tool + " exec '" +
			strings.ReplaceAll(executable, "'", "'\\''") + "' -test.run='^TestCaptureNetfilterProcess$' -- \"$@\"\n"
		if err := os.WriteFile(filepath.Join(fixture.bin, tool), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := runtimeRegistration().Resolve([]string{"blocked.example.com"}, true, dual)
	if err != nil {
		t.Fatal(err)
	}
	script, err := plan.Script(15440)
	if err != nil || os.WriteFile(fixture.script, []byte(script), 0o600) != nil {
		t.Fatal("write generated script:", err)
	}
	fixture.write(t, newFakeNetfilter())
	return fixture
}

func (f scriptFixture) write(t *testing.T, state fakeNetfilter) {
	t.Helper()
	data, err := json.Marshal(state)
	if err != nil || os.WriteFile(f.state, data, 0o600) != nil {
		t.Fatal("write fake state:", err)
	}
}

func (f scriptFixture) read(t *testing.T) fakeNetfilter {
	t.Helper()
	data, err := os.ReadFile(f.state)
	var state fakeNetfilter
	if err != nil || json.Unmarshal(data, &state) != nil {
		t.Fatal("read fake state:", err)
	}
	return state
}

func (f scriptFixture) run(t *testing.T, variables ...string) error {
	t.Helper()
	command := exec.Command("sh", f.script)
	command.Dir = f.dir
	command.Env = append(os.Environ(), "PATH="+f.bin+":"+os.Getenv("PATH"),
		"CETIRUN_FAKE_NETFILTER=1", "CETIRUN_FAKE_STATE="+f.state,
		"GORACE=atexit_sleep_ms=0",
		"CETIRUN_FAIL_AFTER=", "CETIRUN_FAIL_READ=", "CETIRUN_FAIL_DELETE=")
	command.Env = append(command.Env, variables...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Logf("expected or unexpected script failure: %v (%s)", err, strings.TrimSpace(string(output)))
	}
	return err
}

func assertInstalled(t *testing.T, state fakeNetfilter, dual bool) {
	t.Helper()
	for _, family := range []struct {
		tool, loopback string
		enabled        bool
	}{{"iptables", "127.0.0.0/8", true}, {"ip6tables", "::1/128", dual}} {
		table := state.Tables[family.tool]
		original := newFakeNetfilter().Tables[family.tool].Output
		if !family.enabled {
			if table.Chain || len(table.Rules) != 0 || !slices.Equal(table.Output, original) {
				t.Fatal("unselected family changed")
			}
			continue
		}
		expected := commands(family.loopback)
		wantRules := []string{}
		for _, args := range expected[1:4] {
			wantRules = append(wantRules, strings.Join(args[4:], " "))
		}
		hook := strings.Join(expected[4][4:], " ")
		if !table.Chain || !slices.Equal(table.Rules, wantRules) ||
			!slices.Equal(table.Output, append(original, hook)) {
			t.Fatalf("%s did not converge without changing unrelated rules: %+v", family.tool, table)
		}
	}
}

func TestGeneratedCaptureScriptRetriesEveryCommittedPrefix(t *testing.T) {
	for _, dual := range []bool{false, true} {
		mutations := 5
		if dual {
			mutations = 10
		}
		for fail := 0; fail <= mutations; fail++ {
			t.Run(fmt.Sprintf("dual=%t/after=%d", dual, fail), func(t *testing.T) {
				fixture := newScriptFixture(t, dual)
				err := fixture.run(t, "CETIRUN_FAIL_AFTER="+strconv.Itoa(fail))
				if (err != nil) != (fail != 0) {
					t.Fatal("committed mutation error was ignored or clean install failed:", err)
				}
				if err := fixture.run(t); err != nil {
					t.Fatal("same-namespace retry did not recover:", err)
				}
				state := fixture.read(t)
				assertInstalled(t, state, dual)
				if err := fixture.run(t); err != nil || !reflect.DeepEqual(state, fixture.read(t)) {
					t.Fatal("successful retry changed the installed state:", err)
				}
			})
		}
	}
}

func TestGeneratedCaptureScriptDeduplicatesOnlyOwnedHooks(t *testing.T) {
	fixture := newScriptFixture(t, true)
	if err := fixture.run(t); err != nil {
		t.Fatal(err)
	}
	state := fixture.read(t)
	for tool, table := range state.Tables {
		table.Output = append(table.Output, "-A OUTPUT -p tcp -j "+captureChain, "-A OUTPUT -p tcp -j "+captureChain)
		state.Tables[tool] = table
	}
	fixture.write(t, state)
	if err := fixture.run(t, "CETIRUN_FAIL_DELETE=1"); err == nil || !reflect.DeepEqual(state, fixture.read(t)) {
		t.Fatal("operational hook-delete failure was ignored")
	}
	if err := fixture.run(t, "CETIRUN_FAIL_AFTER="+strconv.Itoa(state.Mutations+1)); err == nil {
		t.Fatal("committed hook-delete error was ignored")
	}
	if err := fixture.run(t); err != nil {
		t.Fatal(err)
	}
	assertInstalled(t, fixture.read(t), true)
}

func TestGeneratedCaptureScriptRejectsForeignStateAndReadErrors(t *testing.T) {
	for _, scenario := range []string{"foreign rule", "reordered rules", "foreign hook", "read error"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newScriptFixture(t, false)
			state := fixture.read(t)
			table := state.Tables["iptables"]
			var variables []string
			switch scenario {
			case "foreign rule":
				table.Chain, table.Rules = true, []string{"-A " + captureChain + " -j ACCEPT"}
			case "reordered rules":
				table.Chain, table.Rules = true, []string{
					"-A " + captureChain + " -m owner --uid-owner 1337 -j RETURN",
					"-A " + captureChain + " -p tcp -j REDIRECT --to-ports 15001",
				}
			case "foreign hook":
				table.Chain = true
				table.Output = append(table.Output, "-A OUTPUT -p udp -j "+captureChain)
			case "read error":
				variables = []string{"CETIRUN_FAIL_READ=1"}
			}
			state.Tables["iptables"] = table
			fixture.write(t, state)
			if err := fixture.run(t, variables...); err == nil || !reflect.DeepEqual(state, fixture.read(t)) {
				t.Fatal("foreign state or failed reads were silently accepted or modified")
			}
		})
	}
}
