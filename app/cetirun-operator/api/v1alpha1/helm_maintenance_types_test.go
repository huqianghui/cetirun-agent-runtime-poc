// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

const maintenanceTestID = "maintenance-example-0001"

func TestHelmMaintenanceGeneratedSchema(t *testing.T) {
	root := apiContractCRD(t, "agents").Spec.Versions[0].Schema.OpenAPIV3Schema
	spec, status := root.Properties["spec"], root.Properties["status"]
	helm := spec.Properties["helm"]
	request, found := helm.Properties["maintenance"]
	if !found || slices.Contains(helm.Required, "maintenance") || request.Default != nil || request.Type != "object" {
		t.Fatal("Helm maintenance must be an optional object without a default")
	}
	observation, found := status.Properties["helmMaintenance"]
	if !found || slices.Contains(status.Required, "helmMaintenance") || observation.Default != nil || observation.Type != "object" {
		t.Fatal("Helm maintenance status must remain optional without a default")
	}
	if !slices.Equal(request.Required, []string{"expectedRevision", "requestID"}) ||
		!slices.Equal(observation.Required, []string{"observedGeneration", "phase", "requestID", "revision"}) {
		t.Fatalf("incomplete request/status required fields: %v / %v", request.Required, observation.Required)
	}
	if helm.Properties["values"].XPreserveUnknownFields == nil || !*helm.Properties["values"].XPreserveUnknownFields {
		t.Fatal("maintenance changed the existing Helm values schema")
	}
	if slices.Contains(spec.Required, "suspended") {
		t.Fatal("ordinary Agents must not gain a required suspension field")
	}
}

func TestHelmMaintenanceRequestValidation(t *testing.T) {
	for _, directory := range []string{"config/crd/bases", "chart/templates/crds"} {
		t.Run(directory, func(t *testing.T) {
			validator := readHarnessSchema(t, directory, "agents", "spec", "helm", "maintenance")
			for _, test := range []struct {
				name, requestID string
				revision        any
				valid           bool
			}{
				{"normal", maintenanceTestID, int64(1), true},
				{"minimum ID", strings.Repeat("a", 16), int64(2), true},
				{"maximum ID", strings.Repeat("Z", 128), int64(2147483648), true},
				{"ASCII alphabet", "Az09_-Az09_-Az09_-", int64(3), true},
				{"empty ID", "", int64(1), false},
				{"short ID", strings.Repeat("a", 15), int64(1), false},
				{"long ID", strings.Repeat("a", 129), int64(1), false},
				{"Unicode ID", strings.Repeat("界", 16), int64(1), false},
				{"space", "maintenance example-0001", int64(1), false},
				{"slash", "maintenance/example-0001", int64(1), false},
				{"newline", maintenanceTestID + "\n", int64(1), false},
				{"zero revision", maintenanceTestID, int64(0), false},
				{"negative revision", maintenanceTestID, int64(-1), false},
				{"fractional revision", maintenanceTestID, 1.5, false},
				{"string revision", maintenanceTestID, "1", false},
			} {
				t.Run(test.name, func(t *testing.T) {
					errs := validator.Validate(map[string]any{"requestID": test.requestID, "expectedRevision": test.revision}).Errors
					if (len(errs) == 0) != test.valid {
						t.Fatalf("validation errors = %v, want valid=%v", errs, test.valid)
					}
				})
			}
			for _, missing := range []string{"requestID", "expectedRevision"} {
				value := map[string]any{"requestID": maintenanceTestID, "expectedRevision": int64(1)}
				delete(value, missing)
				if len(validator.Validate(value).Errors) == 0 {
					t.Fatalf("accepted request without %s", missing)
				}
			}
		})
	}
}

func TestHelmMaintenanceStatusValidation(t *testing.T) {
	for _, directory := range []string{"config/crd/bases", "chart/templates/crds"} {
		t.Run(directory, func(t *testing.T) {
			validator := readHarnessSchema(t, directory, "agents", "status", "helmMaintenance")
			valid := func() map[string]any {
				return map[string]any{"requestID": maintenanceTestID, "observedGeneration": int64(3), "revision": int64(2), "phase": "Requested"}
			}
			for _, phase := range []string{"Requested", "Held", "Reapplying", "Released", "Blocked"} {
				value := valid()
				value["phase"] = phase
				value["reason"], value["message"] = "ExampleReason", "Bounded status detail"
				if errs := validator.Validate(value).Errors; len(errs) != 0 {
					t.Fatalf("phase %s rejected: %v", phase, errs)
				}
			}
			unobserved := valid()
			unobserved["observedGeneration"], unobserved["revision"] = int64(0), int64(0)
			if errs := validator.Validate(unobserved).Errors; len(errs) != 0 {
				t.Fatalf("unobserved counters rejected: %v", errs)
			}
			for _, missing := range []string{"requestID", "observedGeneration", "revision", "phase"} {
				value := valid()
				delete(value, missing)
				if len(validator.Validate(value).Errors) == 0 {
					t.Fatalf("accepted status without %s", missing)
				}
			}
			for _, test := range []struct {
				field string
				value any
			}{
				{"requestID", "short"}, {"requestID", strings.Repeat("a", 129)}, {"requestID", strings.Repeat("界", 16)},
				{"observedGeneration", int64(-1)}, {"revision", int64(-1)}, {"revision", 1.5},
				{"phase", "Ready"}, {"phase", "held"}, {"phase", ""},
			} {
				value := valid()
				value[test.field] = test.value
				if len(validator.Validate(value).Errors) == 0 {
					t.Fatalf("accepted invalid %s=%v", test.field, test.value)
				}
			}
		})
	}
}

func maintenanceSpec(requestID string, revision int64, suspended *bool) map[string]any {
	value := map[string]any{
		"agent":                 map[string]any{"type": "hermes", "username": "vibe", "webUI": false},
		"user":                  map[string]any{"alias": "example-agent", "displayName": "Example agent"},
		"gatewayTokenSecretRef": "gateway-token",
	}
	if requestID != "" {
		value["helm"] = map[string]any{"maintenance": map[string]any{"requestID": requestID, "expectedRevision": revision}}
	}
	if suspended != nil {
		value["suspended"] = *suspended
	}
	return value
}

// Like the existing API CEL tests, evaluate the generated rules directly.
// This is not an API-server, admission, or Helm-writer integration test.
func maintenanceRulesAllow(t *testing.T, schema apiextensionsv1.JSONSchemaProps, current, previous map[string]any) bool {
	t.Helper()
	env, err := cel.NewEnv(cel.Variable("self", cel.DynType), cel.Variable("oldSelf", cel.DynType))
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range schema.XValidations {
		if previous == nil && strings.Contains(rule.Rule, "oldSelf") {
			continue
		}
		ast, issues := env.Compile(rule.Rule)
		if issues != nil && issues.Err() != nil {
			t.Fatalf("compile %q: %v", rule.Rule, issues.Err())
		}
		program, err := env.Program(ast)
		if err != nil {
			t.Fatal(err)
		}
		value, _, err := program.ContextEval(context.Background(), map[string]any{"self": current, "oldSelf": previous})
		if err != nil {
			t.Fatalf("evaluate %q: %v", rule.Rule, err)
		}
		if value.Value() != true {
			return false
		}
	}
	for name, child := range schema.Properties {
		next, ok := current[name].(map[string]any)
		if !ok {
			continue
		}
		old, _ := previous[name].(map[string]any)
		if !maintenanceRulesAllow(t, child, next, old) {
			return false
		}
	}
	return true
}

func TestHelmMaintenanceTransitions(t *testing.T) {
	spec := apiContractCRD(t, "agents").Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	yes, no := true, false
	requested := maintenanceSpec(maintenanceTestID, 2, &yes)
	cleared := maintenanceSpec("", 0, &yes)
	emptyHelm := maintenanceSpec("", 0, &yes)
	emptyHelm["helm"] = map[string]any{"version": "1.2.3"}
	nestedResume := maintenanceSpec("", 0, &no)
	nestedResume["helm"] = map[string]any{"version": "1.2.3"}
	nestedOmittedSuspension := maintenanceSpec("", 0, nil)
	nestedOmittedSuspension["helm"] = map[string]any{"version": "1.2.3"}
	for _, test := range []struct {
		name              string
		previous, current map[string]any
		valid             bool
	}{
		{"legacy omission", nil, maintenanceSpec("", 0, nil), true},
		{"create request while suspended", nil, requested, true},
		{"create request without suspension", nil, maintenanceSpec(maintenanceTestID, 2, nil), false},
		{"create request while running", nil, maintenanceSpec(maintenanceTestID, 2, &no), false},
		{"add request and suspension", maintenanceSpec("", 0, nil), requested, true},
		{"same request and revision", requested, maintenanceSpec(maintenanceTestID, 2, &yes), true},
		{"same ID changes revision", requested, maintenanceSpec(maintenanceTestID, 3, &yes), false},
		{"new request may select a revision", requested, maintenanceSpec("maintenance-example-0002", 3, &yes), true},
		{"held request cannot resume", requested, maintenanceSpec(maintenanceTestID, 2, &no), false},
		{"held request cannot drop suspension", requested, maintenanceSpec(maintenanceTestID, 2, nil), false},
		{"clear whole Helm while suspended", requested, cleared, true},
		{"clear nested request while suspended", requested, emptyHelm, true},
		{"clear and resume together", requested, maintenanceSpec("", 0, &no), false},
		{"clear and drop suspension together", requested, maintenanceSpec("", 0, nil), false},
		{"clear nested request and resume", requested, nestedResume, false},
		{"clear nested request and drop suspension", requested, nestedOmittedSuspension, false},
		// Released and caller authority are checked by admission/controller, not this schema.
		{"later resume is schema legal", cleared, maintenanceSpec("", 0, &no), true},
		{"ordinary suspend remains valid", maintenanceSpec("", 0, nil), cleared, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := maintenanceRulesAllow(t, spec, test.current, test.previous); got != test.valid {
				t.Fatalf("transition valid=%v, want %v", got, test.valid)
			}
		})
	}
}

func TestHelmMaintenancePreservesLifecycleRules(t *testing.T) {
	spec := apiContractCRD(t, "agents").Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	before := maintenanceSpec("", 0, nil)
	before["restartRequestID"] = "restart-example-0001"
	if maintenanceRulesAllow(t, spec, maintenanceSpec("", 0, nil), before) {
		t.Fatal("existing restart ID became removable")
	}
	yes := true
	after := maintenanceSpec("", 0, &yes)
	after["restartRequestID"] = "restart-example-0002"
	if maintenanceRulesAllow(t, spec, after, before) {
		t.Fatal("suspended Agent accepted a new restart")
	}
	before = maintenanceSpec("", 0, nil)
	before["plannedMigration"] = map[string]any{"requestID": "migration-example-0001", "target": map[string]any{"name": "node-a", "uid": "uid-a"}}
	after = maintenanceSpec("", 0, nil)
	after["plannedMigration"] = map[string]any{"requestID": "migration-example-0001", "target": map[string]any{"name": "node-b", "uid": "uid-b"}}
	if maintenanceRulesAllow(t, spec, after, before) {
		t.Fatal("existing migration request changed its target")
	}
}

func TestHelmMaintenanceLegacyOmission(t *testing.T) {
	agent := Agent{Spec: AgentSpec{Agent: HarnessSpec{Type: AgentTypeHermes}, User: UserSpec{Alias: "example-agent", DisplayName: "Example agent"}, GatewayTokenSecretRef: "gateway-token"}}
	wire, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "maintenance") || strings.Contains(string(wire), "helmMaintenance") {
		t.Fatal("legacy serialization gained maintenance state")
	}
	var decoded Agent
	if err := json.Unmarshal(wire, &decoded); err != nil || !reflect.DeepEqual(agent, decoded) {
		t.Fatalf("legacy Agent round trip changed: %v", err)
	}
}

func TestHelmMaintenanceWireFormatAndCopy(t *testing.T) {
	agent := &Agent{
		Spec: AgentSpec{Suspended: true, Helm: HelmSpec{
			Version: "1.2.3", Maintenance: &HelmMaintenanceRequest{RequestID: maintenanceTestID, ExpectedRevision: 2},
		}},
		Status: AgentStatus{HelmMaintenance: &HelmMaintenanceStatus{
			RequestID: maintenanceTestID, ObservedGeneration: 4, Revision: 2, Phase: HelmMaintenancePhaseHeld,
			Reason: "WritersStopped", Message: "The controller observed a stopped runtime.",
		}},
	}
	wire := apiJSONMap(t, agent)
	request := wire["spec"].(map[string]any)["helm"].(map[string]any)["maintenance"]
	status := wire["status"].(map[string]any)["helmMaintenance"]
	if !reflect.DeepEqual(request, map[string]any{"requestID": maintenanceTestID, "expectedRevision": int64(2)}) {
		t.Fatalf("request wire format changed: %v", request)
	}
	if !reflect.DeepEqual(status, map[string]any{
		"requestID": maintenanceTestID, "observedGeneration": int64(4), "revision": int64(2),
		"phase": "Held", "reason": "WritersStopped", "message": "The controller observed a stopped runtime.",
	}) || ConditionHelmMaintenanceReady != "HelmMaintenanceReady" {
		t.Fatalf("status/condition wire format changed: %v", status)
	}
	encoded, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Agent
	if err := json.Unmarshal(encoded, &decoded); err != nil || !reflect.DeepEqual(agent, &decoded) {
		t.Fatalf("maintenance round trip changed: %v", err)
	}
	copy := agent.DeepCopy()
	copy.Spec.Helm.Maintenance.ExpectedRevision = 9
	copy.Status.HelmMaintenance.Phase = HelmMaintenancePhaseBlocked
	copy.Status.HelmMaintenance.Message = "Changed copy"
	listCopy := (&AgentList{Items: []Agent{*agent}}).DeepCopy()
	listCopy.Items[0].Spec.Helm.Maintenance.RequestID = "maintenance-example-0002"
	listCopy.Items[0].Status.HelmMaintenance.ObservedGeneration = 10
	if agent.Spec.Helm.Maintenance.ExpectedRevision != 2 || agent.Spec.Helm.Maintenance.RequestID != maintenanceTestID ||
		agent.Status.HelmMaintenance.Phase != HelmMaintenancePhaseHeld || agent.Status.HelmMaintenance.ObservedGeneration != 4 ||
		agent.Status.HelmMaintenance.Message != "The controller observed a stopped runtime." {
		t.Fatal("Agent or AgentList DeepCopy shares maintenance pointers")
	}
	if omitted := (&Agent{}).DeepCopy(); omitted.Spec.Helm.Maintenance != nil || omitted.Status.HelmMaintenance != nil {
		t.Fatal("DeepCopy materialized an omitted maintenance object")
	}
	minimal := apiJSONMap(t, HelmMaintenanceStatus{RequestID: maintenanceTestID, Phase: HelmMaintenancePhaseRequested})
	if _, present := minimal["reason"]; present {
		t.Fatal("empty reason was not omitted")
	}
	if _, present := minimal["message"]; present {
		t.Fatal("empty message was not omitted")
	}
	if minimal["observedGeneration"] != int64(0) || minimal["revision"] != int64(0) {
		t.Fatal("required unobserved counters were omitted")
	}
}

func TestHelmMaintenanceStrictWireTypes(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	decoder := serializer.NewCodecFactory(scheme, serializer.EnableStrict).UniversalDeserializer()
	for _, fragment := range []string{
		`"spec":{"helm":{"maintenance":{"requestID":12,"expectedRevision":1}}}`,
		`"spec":{"helm":{"maintenance":{"requestID":"maintenance-example-0001","expectedRevision":"1"}}}`,
		`"spec":{"helm":{"maintenance":{"requestID":"maintenance-example-0001","expectedRevision":1.5}}}`,
		`"status":{"helmMaintenance":{"requestID":"maintenance-example-0001","observedGeneration":1,"revision":1,"phase":true}}`,
		`"status":{"helmMaintenance":{"requestID":"maintenance-example-0001","observedGeneration":1,"revision":1,"phase":"Held","approved":true}}`,
	} {
		raw := `{"apiVersion":"cetirun.microsoft.com/v1alpha1","kind":"Agent",` + fragment + `}`
		if _, _, err := decoder.Decode([]byte(raw), nil, nil); err == nil {
			t.Fatalf("strict decoder accepted malformed maintenance: %s", fragment)
		}
	}
}
