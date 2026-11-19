// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import "testing"

func TestRuntimeRegistryPreservesPublicationPrefix(t *testing.T) {
	for _, test := range []struct {
		name, repo, want string
	}{
		{
			name: "MCR publication prefix",
			repo: "oci://mcr.microsoft.com/aks/ai-runtime/cetirun/helm/cetirun",
			want: "mcr.microsoft.com/aks/ai-runtime/cetirun",
		},
		{
			name: "default publication path",
			repo: "oci://registry.example.com/helm/cetirun",
			want: "registry.example.com",
		},
		{
			name: "publication prefix with port and nested paths",
			repo: "oci://registry.example.com:5000/project/staging/123/helm/cetirun",
			want: "registry.example.com:5000/project/staging/123",
		},
		{
			name: "nonstandard chart path retains hostname fallback",
			repo: "oci://registry.example.com/other/runtime",
			want: "registry.example.com",
		},
		{
			name: "suffix must match the complete chart path",
			repo: "oci://registry.example.com/project/helm/cetirun-extra",
			want: "registry.example.com",
		},
		{
			name: "missing registry is invalid",
			repo: "oci:///helm/cetirun",
			want: "",
		},
		{
			name: "malformed registry is invalid",
			repo: "oci://registry.example.com:invalid/project/helm/cetirun",
			want: "",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := runtimeRegistry(test.repo); got != test.want {
				t.Errorf("runtimeRegistry(%q) = %q, want %q", test.repo, got, test.want)
			}
		})
	}
}
