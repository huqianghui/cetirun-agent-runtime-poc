// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package agentconfig

import (
	"encoding/json"
	"reflect"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"k8s.io/utils/ptr"
)

func TestCreateAgentContract(t *testing.T) {
	for _, tc := range []struct {
		body string
		ok   bool
	}{
		{`{}`, true},
		{`{"type":"hermes"}`, true},
		{`{"type":"openclaw"}`, true},
		{`{"type":"custom","image":"registry.example.com/team/agent:1"}`, true},
		{`{"type":"custom","image":"registry.example.com/team/agent:1","username":"assistant","port":8080}`, true},
		{`{"type":"custom","image":"registry.example.com/team/agent:1","port":8080,"webUI":true}`, true},
		{`{"type":"custom","image":"registry.example.com/team/agent:1","port":8080,"webUI":false}`, true},
		{`{"type":"custom","image":"registry.example.com/team/agent:1","webUI":false}`, true},
		{`{"type":"custom","image":"registry.example.com/team/agent:1","webUI":null}`, true},
		{`{"webUI":false}`, true},
		{`{"webUI":true,"port":8080}`, false},
		{`{"type":"custom","image":"registry.example.com/team/agent:1","webUI":true}`, false},
		{`{"type":"custom","image":"registry.example.com/team/agent:1","port":0,"webUI":true}`, false},
		{`{"type":"custom","image":"registry.example.com/team/agent:1","port":65536,"webUI":true}`, false},
		{`{"type":"custom","image":"registry.example.com/team/agent:1","webUI":{"port":8080}}`, false},
		{`{"webUI":"true"}`, false},
		{`{"webUI":1}`, false},
		{`{"type":"custom"}`, false},
		{`{"type":""}`, false},
		{`{"type":null}`, false},
		{`{"type":"other"}`, false},
		{`{"username":""}`, false},
		{`{"username":"root"}`, false},
		{`{"username":"../other"}`, false},
		{`{"username":null}`, false},
		{`{"username":"other"}`, false},
		{`{"image":""}`, false},
		{`{"image":null}`, false},
		{`{"image":"https://registry.example.com/agent"}`, false},
		{`{"image":"repo/image with spaces"}`, false},
		{`{"port":0}`, false},
		{`{"port":65536}`, false},
		{`{"port":1.5}`, false},
		{`{"port":"8080"}`, false},
	} {
		t.Run(tc.body, func(t *testing.T) {
			var in Input
			err := json.Unmarshal([]byte(tc.body), &in)
			var agent api.HarnessSpec
			if err == nil {
				agent, err = in.Create()
			}
			if (err == nil) != tc.ok {
				t.Fatalf("Create() error=%v, want success=%v", err, tc.ok)
			}
			if tc.ok && (agent.Type == "" || agent.Username == nil) {
				t.Fatal("defaults were not applied")
			}
		})
	}
}

func TestPatchAgentTransitions(t *testing.T) {
	current := api.HarnessSpec{Type: api.AgentTypeCustom, Image: ptr.To("registry.example.com/agent:1"), Port: ptr.To(int32(8080))}
	for _, tc := range []struct {
		body string
		ok   bool
		port *int32
	}{
		{`{}`, true, ptr.To(int32(8080))},
		{`{"port":null}`, true, nil},
		{`{"port":9090}`, true, ptr.To(int32(9090))},
		{`{"type":"custom","username":"vibe"}`, true, ptr.To(int32(8080))},
		{`{"type":"hermes"}`, false, nil},
		{`{"username":"assistant"}`, false, nil},
		{`{"username":null}`, false, nil},
		{`{"image":null}`, false, nil},
		{`{"image":""}`, false, nil},
	} {
		t.Run(tc.body, func(t *testing.T) {
			var in Input
			if err := json.Unmarshal([]byte(tc.body), &in); err != nil {
				t.Fatal(err)
			}
			agent, err := in.Apply(current)
			if (err == nil) != tc.ok {
				t.Fatalf("Apply() error=%v, want success=%v", err, tc.ok)
			}
			if tc.ok && !ptr.Equal(agent.Port, tc.port) {
				t.Fatalf("wrong port: %v", agent.Port)
			}
			if current.Username != nil || *current.Port != 8080 {
				t.Fatal("patch modified the source object")
			}
		})
	}
}

func TestMarshalOmitsUnsetFields(t *testing.T) {
	in := Input{Port: Field[int32]{Set: true, Null: true}}
	data, err := json.Marshal(in)
	if err != nil || string(data) != `{"port":null}` {
		t.Fatalf("unexpected wire format: %s, %v", data, err)
	}
	data, err = json.Marshal(Input{WebUI: Value(false)})
	if err != nil || string(data) != `{"webUI":false}` {
		t.Fatalf("explicit false was omitted: %s, %v", data, err)
	}
}

func TestPatchWebUITransitionsPreserveOtherAgentFields(t *testing.T) {
	current := api.HarnessSpec{Type: api.AgentTypeCustom, Image: ptr.To("registry.example.com/agent:1"),
		RegistrySecretRef: &api.RegistrySecretRef{Name: "registry-example"},
		Port:              ptr.To[int32](8080), WebUI: true}
	for _, tc := range []struct {
		body string
		ok   bool
		ui   bool
		port *int32
	}{
		{`{}`, true, true, ptr.To[int32](8080)},
		{`{"image":"registry.example.com/agent:1"}`, true, true, ptr.To[int32](8080)},
		{`{"webUI":false}`, true, false, ptr.To[int32](8080)},
		{`{"webUI":null}`, true, false, ptr.To[int32](8080)},
		{`{"webUI":true,"port":9090}`, true, true, ptr.To[int32](9090)},
		{`{"port":9090}`, true, true, ptr.To[int32](9090)},
		{`{"port":null}`, false, true, nil},
		{`{"webUI":false,"port":null}`, true, false, nil},
	} {
		t.Run(tc.body, func(t *testing.T) {
			var in Input
			if err := json.Unmarshal([]byte(tc.body), &in); err != nil {
				t.Fatal(err)
			}
			if tc.body != "{}" && in.Empty() {
				t.Fatal("Web UI patch was treated as empty")
			}
			agent, err := in.Apply(current)
			if (err == nil) != tc.ok {
				t.Fatalf("Apply() error=%v, want success=%v", err, tc.ok)
			}
			if tc.ok && (agent.WebUI != tc.ui || !ptr.Equal(agent.Port, tc.port) ||
				!reflect.DeepEqual(agent.RegistrySecretRef, current.RegistrySecretRef)) {
				t.Fatalf("patch lost Web UI or Registry state: %#v", agent)
			}
			if current.Username != nil || !current.WebUI || *current.Port != 8080 {
				t.Fatal("patch modified its input")
			}
		})
	}
}
