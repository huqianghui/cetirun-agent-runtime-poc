// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimebindings

import (
	"strings"
	"testing"
)

func TestDefaultMemoryImage(t *testing.T) {
	for _, test := range []struct{ registry, version, want string }{
		{"", "0.1.1", "mcr.microsoft.com/aks/ai-runtime/cetirun/cetirun-memory-context:0.1.1"},
		{"registry.example.com/team/", "2.3.4-dev.2", "registry.example.com/team/cetirun-memory-context:2.3.4-dev.2"},
	} {
		image := DefaultMemoryImage(test.registry, test.version)
		if image != test.want {
			t.Fatalf("default image = %q, want %q", image, test.want)
		}
		if err := (&Memory{Image: image, Home: "/home/vibe"}).Validate("/home/vibe"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMemoryImageValidation(t *testing.T) {
	for _, image := range []string{
		"registry.example.com/memory:1.2.3", "registry.example.com/memory:1.2.3-dev.1",
		"registry.example.com/memory@sha256:" + strings.Repeat("a", 64),
	} {
		binding := &Memory{Image: image, Home: "/home/worker"}
		if err := binding.Validate("/home/worker"); err != nil {
			t.Fatal(err)
		}
		if binding.Validate("/home/other") == nil {
			t.Fatal("a different HOME was accepted")
		}
	}
	for _, image := range []string{"", "registry.example.com/memory", "registry.example.com/memory:latest", "registry.example.com/memory:1.2", "registry.example.com/memory@sha256:bad", "registry.example.com/memory:1.2.3\n"} {
		if (&Memory{Image: image, Home: "/home/vibe"}).Validate("/home/vibe") == nil {
			t.Fatalf("invalid image %q was accepted", image)
		}
	}
}
