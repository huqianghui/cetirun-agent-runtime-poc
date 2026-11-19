// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestEntrypointExitStatus(t *testing.T) {
	if mode := os.Getenv("CETIRUN_TERMINAL_TEST_MODE"); mode != "" {
		os.Args = []string{"terminal", mode}
		main()
		os.Exit(0)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"serve", "exec"} {
		t.Run(mode, func(t *testing.T) {
			command := exec.Command(executable, "-test.run=^TestEntrypointExitStatus$")
			// Missing binding fails locally, before an in-cluster client is created.
			command.Env = []string{"CETIRUN_TERMINAL_TEST_MODE=" + mode}
			var stderr bytes.Buffer
			command.Stderr = &stderr
			err := command.Run()
			if (err != nil) != (mode == "serve") {
				t.Fatalf("%s exit: %v; stderr: %s", mode, err, stderr.String())
			}
			if !strings.Contains(stderr.String(), "Terminal ended.") {
				t.Fatalf("missing redacted diagnostic: %s", stderr.String())
			}
		})
	}
}
