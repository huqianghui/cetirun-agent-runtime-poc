// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/Azure/CetiRun/app/cetirun-connections-mcp/internal/tools/providers"
)

type Spec struct {
	Name        string
	Description string
	InputSchema map[string]any
}

func Catalog() []Spec {
	empty := map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
	return []Spec{
		{"connections_status", "List this Agent's bound connections, executor identity, actual OAuth scopes and status. Credentials are never returned.", empty},
		{"connections_start_link", "Open the configured Portal to connect an account and explicitly associate it with this Agent.", empty},
		{"services_list", "Discover enabled services and their fixed origin/base path for this Agent. API knowledge comes from service skills.", empty},
		{"service_request", "Execute one JSON REST request with the bound connection. Provider RBAC/OAuth permissions decide access. Path starts with / and is relative to the discovered service base path; query is separate. No redirects or automatic retries. A correlation ID is for tracing, not idempotency. On execution_unknown, inspect provider state before considering another request. Pagination and async polling require separate calls with a validated relative path; never pass an absolute URL.", map[string]any{
			"type": "object", "additionalProperties": false, "required": []string{"connection_id", "service", "method", "path"},
			"properties": map[string]any{
				"connection_id": map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
				"service":       map[string]any{"type": "string"},
				"method":        map[string]any{"type": "string", "enum": []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}},
				"path":          map[string]any{"type": "string", "minLength": 1, "maxLength": 8192},
				"query":         map[string]any{"type": "object", "additionalProperties": map[string]any{"oneOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}}},
				"headers":       map[string]any{"type": "object", "description": "Only If-Match, If-None-Match, Prefer, ConsistencyLevel and X-GitHub-Api-Version are allowed.", "additionalProperties": map[string]any{"type": "string"}},
				"body":          map[string]any{"description": "Optional JSON value; not supported for GET or HEAD. Maximum 256 KiB."},
			},
		}},
	}
}
func Find(name string) (Spec, bool) {
	for _, s := range Catalog() {
		if s.Name == name {
			return s, true
		}
	}
	return Spec{}, false
}
func DecodeArguments(raw json.RawMessage, schema map[string]any) (map[string]any, error) {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	var input map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&input) != nil || input == nil || decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("invalid arguments")
	}
	properties, _ := schema["properties"].(map[string]any)
	for key := range input {
		if _, ok := properties[key]; !ok {
			return nil, errors.New("unknown argument")
		}
	}
	required, _ := schema["required"].([]string)
	for _, key := range required {
		if _, ok := input[key]; !ok {
			return nil, errors.New("missing argument")
		}
	}
	return input, nil
}
func ValidateToolArguments(name string, input map[string]any) error {
	if name == "service_request" {
		_, err := providers.Decode(input)
		return err
	}
	if _, ok := Find(name); !ok || len(input) != 0 {
		return errors.New("invalid arguments")
	}
	return nil
}
