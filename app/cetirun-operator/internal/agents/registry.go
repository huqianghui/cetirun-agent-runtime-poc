// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package agents

import "fmt"

type Registry struct {
	harnesses map[string]Harness
}

func NewRegistry(harnesses ...Harness) (*Registry, error) {
	r := &Registry{harnesses: make(map[string]Harness, len(harnesses))}
	for _, h := range harnesses {
		if h == nil || h.Describe().Type == "" {
			return nil, fmt.Errorf("harness must declare a type")
		}
		name := h.Describe().Type
		if _, exists := r.harnesses[name]; exists {
			return nil, fmt.Errorf("duplicate harness type %q", name)
		}
		r.harnesses[name] = h
	}
	return r, nil
}

func (r *Registry) Lookup(name string) (Harness, error) {
	h, ok := r.harnesses[name]
	if !ok {
		return nil, fmt.Errorf("unsupported harness type %q", name)
	}
	return h, nil
}
