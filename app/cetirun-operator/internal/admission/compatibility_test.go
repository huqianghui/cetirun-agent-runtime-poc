// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func addUnknownField(t *testing.T, raw *runtime.RawExtension, section, name string, value any) {
	t.Helper()
	var object map[string]any
	if err := decodeJSON(raw.Raw, &object, false); err != nil {
		t.Fatal(err)
	}
	fields, ok := object[section].(map[string]any)
	if !ok {
		fields = map[string]any{}
		object[section] = fields
	}
	fields[name] = value
	var err error
	raw.Raw, err = json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
}

func TestCompatibleAdmissionAcceptsFutureStatusWithoutPatchingIt(t *testing.T) {
	f := newFixture(t)
	agent := f.agent.DeepCopy()
	agent.Spec.Team, agent.Spec.SandboxRef = "", ""
	updated := agent.DeepCopy()
	updated.Labels = map[string]string{"example.com/note": "updated"}
	request := requestFor(t, admissionv1.Update, updated, agent, "member")
	for _, raw := range []*runtime.RawExtension{&request.Object, &request.OldObject} {
		addUnknownField(t, raw, "status", "futureRegistryCredentials", map[string]any{"sourceUID": "synthetic-source"})
	}
	requireResponse(t, f.handler.Validate(context.Background(), request), false, "unknown field")
	for _, mode := range []string{"", ValidationCompatible} {
		f.handler.config.ValidationMode = mode
		requireResponse(t, f.handler.Validate(context.Background(), request), true, "")
		mutation := f.handler.DefaultAgent(context.Background(), request)
		requireResponse(t, mutation, true, "")
		if len(mutation.Patches) != 0 {
			t.Fatal("an ordinary update must not reserialize or remove future fields")
		}
	}
}

func TestCompatibleAdmissionPreservesExistingChecks(t *testing.T) {
	f := newFixture(t)
	f.handler.config.ValidationMode = ValidationCompatible
	request := requestFor(t, admissionv1.Create, f.policy, nil, "member")
	addUnknownField(t, &request.Object, "status", "futureObservation", true)
	requireResponse(t, f.handler.Validate(context.Background(), request), false, "administrator")
	request = requestFor(t, admissionv1.Create, f.agent, nil, "administrator")
	addUnknownField(t, &request.Object, "spec", "suspended", "not-a-boolean")
	requireResponse(t, f.handler.Validate(context.Background(), request), false, "cannot unmarshal")
	request = requestFor(t, admissionv1.Create, f.policy, nil, "administrator")
	request.Object.Raw = append(request.Object.Raw, []byte(" {}")...)
	requireResponse(t, f.handler.Validate(context.Background(), request), false, "exactly one JSON object")
}

func TestCompatibleUnknownSpecChangesCannotUseLegacyAuthority(t *testing.T) {
	f := newFixture(t)
	f.handler.config.ValidationMode = ValidationCompatible
	agent := f.agent.DeepCopy()
	agent.Spec.Team, agent.Spec.SandboxRef = "", ""
	team := &api.Team{ObjectMeta: metav1.ObjectMeta{Name: "legacy-team"}}
	for _, object := range []client.Object{agent, team} {
		t.Run(object.GetObjectKind().GroupVersionKind().Kind+"/"+object.GetName(), func(t *testing.T) {
			request := requestFor(t, admissionv1.Update, object, object, "member")
			for _, raw := range []*runtime.RawExtension{&request.Object, &request.OldObject} {
				addUnknownField(t, raw, "spec", "futureConfiguration", "unchanged")
			}
			requireResponse(t, f.handler.Validate(context.Background(), request), true, "")
			addUnknownField(t, &request.Object, "spec", "futureConfiguration", "changed")
			requireResponse(t, f.handler.Validate(context.Background(), request), false, "unrecognized")
			request = requestFor(t, admissionv1.Create, object, nil, "member")
			addUnknownField(t, &request.Object, "spec", "futureConfiguration", "new")
			requireResponse(t, f.handler.Validate(context.Background(), request), false, "unrecognized")
		})
	}
}

func TestCompatibleLegacyTeamDeletionWithFutureSpec(t *testing.T) {
	for _, user := range []string{"member", "portal", testController, "administrator"} {
		t.Run(user, func(t *testing.T) {
			f := newFixture(t)
			team := &api.Team{ObjectMeta: metav1.ObjectMeta{Name: "legacy-team"}}
			request := requestFor(t, admissionv1.Delete, team, nil, user)
			addUnknownField(t, &request.OldObject, "spec", "futureConfiguration", "preserve-me")
			for _, mode := range []string{"", ValidationCompatible} {
				f.handler.config.ValidationMode = mode
				requireResponse(t, f.handler.Validate(context.Background(), request), true, "legacy")
			}
			f.handler.reader = unavailableReader{}
			requireResponse(t, f.handler.Validate(context.Background(), request), true, "legacy")
			f.handler.config.ValidationMode = ValidationStrict
			requireResponse(t, f.handler.Validate(context.Background(), request), false, "unknown field")
		})
	}
}

func TestCompatibleDeletionWithFutureSpecKeepsPolicyChecks(t *testing.T) {
	f := newFixture(t)
	f.handler.config.ValidationMode = ValidationCompatible
	f.agent.ResourceVersion = ""
	if err := f.client.Create(context.Background(), f.agent); err != nil {
		t.Fatal(err)
	}
	for _, object := range []client.Object{f.team, f.box, f.policy} {
		t.Run(object.GetName(), func(t *testing.T) {
			for _, user := range []string{"member", "portal", testController, "administrator"} {
				request := requestFor(t, admissionv1.Delete, object, nil, user)
				addUnknownField(t, &request.OldObject, "spec", "futureConfiguration", "preserve-me")
				message := "administrator"
				if user == "administrator" {
					message = "referenced"
					if object == f.team {
						message = "inherited policy"
					}
				}
				requireResponse(t, f.handler.Validate(context.Background(), request), false, message)
			}
		})
	}
}

func TestCompatibleCleanupComparesRawFieldsWithoutRounding(t *testing.T) {
	f := newFixture(t)
	f.handler.config.ValidationMode = ValidationCompatible
	old := f.agent.DeepCopy()
	stamp := metav1.NewTime(time.Now())
	old.DeletionTimestamp = &stamp
	old.Finalizers = []string{metav1.FinalizerDeleteDependents, api.Finalizer}
	updated := old.DeepCopy()
	updated.Finalizers = []string{api.Finalizer}
	request := requestFor(t, admissionv1.Update, updated, old, "system:kube-controller-manager")
	for _, raw := range []*runtime.RawExtension{&request.Object, &request.OldObject} {
		addUnknownField(t, raw, "status", "futureCounter", json.Number("9007199254740992"))
	}
	if !rawMetadataOnly(request) {
		t.Fatal("unchanged unknown fields should permit metadata-only cleanup")
	}
	requireResponse(t, f.handler.Validate(context.Background(), request), true, "")
	addUnknownField(t, &request.Object, "status", "futureCounter", json.Number("9007199254740993"))
	if rawMetadataOnly(request) {
		t.Fatal("large JSON numbers were rounded into an apparent metadata-only change")
	}
	if f.handler.Validate(context.Background(), request).Allowed {
		t.Fatal("garbage collector bypass accepted changes hidden by typed decoding")
	}
}

func TestValidationModeDoesNotRelaxServerConfiguration(t *testing.T) {
	for _, mode := range []string{"", ValidationCompatible, ValidationStrict} {
		config := testConfig()
		config.ValidationMode = mode
		data, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParseConfig(string(data)); err != nil {
			t.Fatal(err)
		}
		unknown := strings.TrimSuffix(string(data), "}") + `,"typo":true}`
		if _, err := ParseConfig(unknown); err == nil {
			t.Fatal("unknown server configuration was accepted")
		}
	}
	config := testConfig()
	config.ValidationMode = "ignore-everything"
	if config.Validate() == nil {
		t.Fatal("unsupported validation mode was accepted")
	}
}
