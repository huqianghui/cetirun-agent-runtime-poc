// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func coordinatedDeleteRequest(t *testing.T, before client.Object, caller string) admission.Request {
	t.Helper()
	request := requestFor(t, admissionv1.Delete, before, nil, caller)
	raw, err := json.Marshal(metav1.DeleteOptions{
		Preconditions:     &metav1.Preconditions{UID: ptr.To(before.GetUID()), ResourceVersion: ptr.To(before.GetResourceVersion())},
		PropagationPolicy: ptr.To(metav1.DeletePropagationBackground),
	})
	if err != nil {
		t.Fatal(err)
	}
	request.Options = runtime.RawExtension{Raw: raw}
	return request
}

func TestConfigurationDeletionRequiresSubmittedReservation(t *testing.T) {
	for _, kind := range []string{"Team", "Sandbox", "AgentPolicy"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, store := coordinatedFixture(t)
			var before client.Object
			switch kind {
			case "Team":
				before = f.team.DeepCopy()
			case "Sandbox":
				before = f.box.DeepCopy()
			case "AgentPolicy":
				before = f.policy.DeepCopy()
			}
			before.SetName(before.GetName() + "-unused")
			before.SetUID(before.GetUID() + "-unused")
			before.SetResourceVersion("")
			if err := f.client.Create(ctx, before); err != nil {
				t.Fatal(err)
			}
			request := coordinatedDeleteRequest(t, before, "administrator")
			requireResponse(t, f.handler.Validate(ctx, request), false, "CoordinationRequired")
			ticket, err := store.AcquireConfigurationDeletion(ctx, before)
			if err != nil {
				t.Fatal(err)
			}
			requireResponse(t, f.handler.Validate(ctx, request), false, "CoordinationRequired")
			ticket, err = store.BeginChange(ctx, ticket)
			if err != nil {
				t.Fatal(err)
			}
			stateKey := client.ObjectKey{Namespace: store.Namespace, Name: store.Identity.Name}
			original := &corev1.ConfigMap{}
			if err := f.client.Get(ctx, stateKey, original); err != nil {
				t.Fatal(err)
			}
			for _, dryRun := range []bool{false, true} {
				request.DryRun = ptr.To(dryRun)
				requireResponse(t, f.handler.Validate(ctx, request), true, "")
			}
			for _, caller := range []string{"member", testController, "system:anonymous"} {
				requireResponse(t, f.handler.Validate(ctx, coordinatedDeleteRequest(t, before, caller)), false, "administrator")
			}
			for _, raw := range []string{
				`{}`, `null`, `{"unexpected":"field"}`, `{"preconditions":{},"preconditions":{}}`,
				`{"preconditions":{"uid":"wrong","resourceVersion":"wrong"},"propagationPolicy":"Background"}`,
			} {
				invalid := request
				invalid.Options = runtime.RawExtension{Raw: []byte(raw)}
				requireResponse(t, f.handler.Validate(ctx, invalid), false, "CoordinationRequired")
			}
			current := &corev1.ConfigMap{}
			if err := f.client.Get(ctx, stateKey, current); err != nil || !reflect.DeepEqual(original, current) {
				t.Fatal("deletion admission changed authority:", err)
			}
			if err := store.CheckChange(ctx, ticket); err != nil {
				t.Fatal("admission released the operation:", err)
			}
			f.handler.coordination.Identity.UID = "missing-authority"
			requireResponse(t, f.handler.Validate(ctx, request), false, "dependencies are unavailable")
		})
	}
}

func TestConfigurationDeletionRetainsReferenceAndLegacyRules(t *testing.T) {
	ctx := context.Background()
	f, store := coordinatedFixture(t)
	ticket, err := store.AcquireConfigurationDeletion(ctx, f.policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginChange(ctx, ticket); err != nil {
		t.Fatal(err)
	}
	requireResponse(t, f.handler.Validate(ctx, coordinatedDeleteRequest(t, f.policy, "administrator")), false, "")

	legacy := &api.Team{ObjectMeta: metav1.ObjectMeta{Name: "legacy", UID: "legacy-uid"}}
	if err := f.client.Create(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	request := coordinatedDeleteRequest(t, legacy, "member")
	requireResponse(t, f.handler.Validate(ctx, request), false, "CoordinationRequired")
	ticket, err = store.AcquireConfigurationDeletion(ctx, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginChange(ctx, ticket); err != nil {
		t.Fatal(err)
	}
	requireResponse(t, f.handler.Validate(ctx, request), true, "")
	requireResponse(t, f.handler.Validate(ctx, requestFor(t, admissionv1.Delete, f.agent, nil, "member")), true, "")

	defaults := newFixture(t)
	requireResponse(t, defaults.handler.Validate(ctx,
		requestFor(t, admissionv1.Delete, legacy, nil, "member")), true, "")
	if defaults.handler.coordination != nil {
		t.Fatal("deletion coordination became default-on")
	}
}

func TestConfigurationDeletionCannotUseUpdateTicket(t *testing.T) {
	ctx := context.Background()
	f, store := coordinatedFixture(t)
	before := &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "unused", UID: "unused-uid"}}
	if err := f.client.Create(ctx, before); err != nil {
		t.Fatal(err)
	}
	after := before.DeepCopy()
	after.Annotations = map[string]string{"example.com/description": "updated"}
	ticket, err := store.AcquireConfigurationChange(ctx, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginChange(ctx, ticket); err != nil {
		t.Fatal(err)
	}
	requireResponse(t, f.handler.Validate(ctx, coordinatedDeleteRequest(t, before, "administrator")), false, "CoordinationRequired")
	if _, err := store.ReadConfigurationDeletion(ctx, before); err == nil {
		t.Fatal("deletion adopted an update operation")
	}
	if _, err := sandboxcoordination.NewKubernetesDeleteAdapter(nil, nil, before); err == nil {
		t.Fatal("deletion observation accepted a missing reader")
	}
}
