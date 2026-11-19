// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package custom

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/agents"
	"k8s.io/utils/ptr"
)

func TestCustomWithoutNativeInputs(t *testing.T) {
	for _, port := range []*int32{nil, ptr.To[int32](8080)} {
		agent, err := agents.Resolve(agents.Request{Type: "custom", Image: "registry.example.com/agent:1", Username: "worker", Port: port}, agents.Profile{})
		if err != nil {
			t.Fatal(err)
		}
		bindings := agents.Bindings{Model: agents.ModelBinding{BaseURL: "http://gateway.example.com/v1",
			CredentialSlot: "model", Revision: "key-r1"}, ContextRevision: "ctx-r1"}
		c, err := (Harness{}).Build(agent, bindings)
		if err != nil {
			t.Fatal(err)
		}
		if len(c.ConfigFiles) != 0 || c.ProfileID != "" || (c.HTTP == nil) != (port == nil) {
			t.Fatalf("native assumption or optional HTTP lost: %+v", c)
		}
		expected := []agents.EnvironmentValue{
			{Name: "HOME", Value: "/home/worker"},
			{Name: "OPENAI_BASE_URL", Value: "http://gateway.example.com/v1"},
			{Name: "OPENAI_API_KEY", CredentialSlot: "model"},
		}
		if port != nil {
			expected = append(expected, agents.EnvironmentValue{Name: "PORT", Value: strconv.Itoa(int(*port))})
		}
		if !reflect.DeepEqual(c.Environment, expected) {
			t.Fatalf("Custom v1 environment changed: %#v", c.Environment)
		}
		raw, _ := json.Marshal(c)
		if strings.Contains(string(raw), "secretName") || strings.Contains(string(raw), "native") {
			t.Fatalf("unexpected privileged contribution: %s", raw)
		}
		if port == nil && (len(c.Ports) != 0 || strings.Contains(string(raw), `"PORT"`) || strings.Contains(string(raw), `"port":`)) {
			t.Fatal("headless received an HTTP port")
		}
		for _, bad := range []agents.Bindings{
			{}, {Model: bindings.Model}, {ContextRevision: "ctx-r1"},
			{Model: agents.ModelBinding{BaseURL: "https://token@gateway.example.com/v1", CredentialSlot: "model", Revision: "r1"}, ContextRevision: "ctx-r1"},
		} {
			if _, err := (Harness{}).Build(agent, bad); err == nil {
				t.Fatal("missing or unsafe binding accepted")
			}
		}
	}
}

func TestCustomResolutionRejectsInvalidInputs(t *testing.T) {
	for _, request := range []agents.Request{
		{Type: "custom"}, {Type: "unknown", Image: "example.com/image:1"},
		{Type: "custom", Image: "example.com/image:1", Username: "root"},
		{Type: "custom", Image: "example.com/image:1", Username: "../other"},
		{Type: "custom", Image: "example.com/image:1", Port: ptr.To[int32](0)},
		{Type: "custom", Image: "example.com/image:1", Port: ptr.To[int32](65536)},
		{Type: "custom", Image: "example.com/image:1\nother"},
	} {
		if _, err := agents.Resolve(request, agents.Profile{}); err == nil {
			t.Fatalf("accepted invalid request: %+v", request)
		}
	}
}
