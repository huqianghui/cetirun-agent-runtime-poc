// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
package tools

import (
	"encoding/json"
	"testing"
)

func TestGenericRequestSchemaAndStrictArguments(t *testing.T) {
	spec, ok := Find("service_request")
	if !ok {
		t.Fatal("missing generic request")
	}
	for _, raw := range []string{`{"connection_id":"c","service":"azure_arm","method":"POST","path":"/providers/Microsoft.ResourceGraph/resources","body":{"query":"Resources"}}`, `{"connection_id":"c","service":"azure_arm","method":"GET","path":"/subscriptions","query":{"api-version":"2022-12-01"}}`} {
		args, err := DecodeArguments(json.RawMessage(raw), spec.InputSchema)
		if err != nil || ValidateToolArguments(spec.Name, args) != nil {
			t.Fatalf("valid generic API rejected: %v", err)
		}
	}
	for _, raw := range []string{`{"connection_id":"c","service":"azure_arm","method":"GET","path":"/","audience":"evil"}`, `{"connection_id":"c"}`, `null`, `{} {}`} {
		if _, err := DecodeArguments(json.RawMessage(raw), spec.InputSchema); err == nil {
			t.Fatalf("invalid arguments accepted: %s", raw)
		}
	}
	if _, ok := Find("azure_resource_groups_create"); ok {
		t.Fatal("fixed operation catalog retained")
	}
	empty, _ := Find("services_list")
	if _, err := DecodeArguments(nil, empty.InputSchema); err != nil {
		t.Fatal(err)
	}
}
