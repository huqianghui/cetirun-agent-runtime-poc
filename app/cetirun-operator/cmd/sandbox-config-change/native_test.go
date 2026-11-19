// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestCLIConfigurationChangesHonorNativeReservations(t *testing.T) {
	for _, kind := range []string{"Team", "Sandbox", "AgentPolicy", "unselected Team sandbox"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			args, store, storage, team, _ := cliFixture(t)
			fixture := testfixture.New("openclaw")
			box, policy := fixture.Sandbox, fixture.Policy
			box.Name, box.UID, box.ResourceVersion = "sandbox", "sandbox-uid", ""
			policy.Name, policy.UID, policy.ResourceVersion = "policy", "policy-uid", ""
			for _, object := range []client.Object{box, policy} {
				if err := storage.Create(ctx, object); err != nil {
					t.Fatal(err)
				}
			}
			intent := sandboxcoordination.NativeIntent{
				Agent: w.ObjectIdentity{Name: "native-agent", UID: "native-agent-uid"}, AgentGeneration: 1,
				Namespace: w.ObjectIdentity{Name: "cetirun-native-agent", UID: "native-namespace-uid"},
				Home:      w.ObjectIdentity{Name: "home", UID: "native-home-uid"},
				Workload:  w.ObjectIdentity{Name: "native-workload", UID: "native-workload-uid"},
				Binding: sandbox.Binding{AgentUID: "native-agent-uid", TeamUID: team.UID, SandboxUID: box.UID,
					AgentPolicyUID: policy.UID},
				Revision: "native-revision", SourceDigests: map[string]string{},
			}
			for _, selected := range []struct {
				kind   string
				object client.Object
			}{{"Team", team}, {"Sandbox", box}, {"AgentPolicy", policy}} {
				reference := w.SourceReference{APIVersion: api.GroupVersion.String(), Kind: selected.kind,
					Name: selected.object.GetName(), UID: selected.object.GetUID(), ResourceVersion: selected.object.GetResourceVersion()}
				intent.Sources = append(intent.Sources, reference)
				value, err := w.ExecutionSourceDigest(selected.object, box.Name)
				if err != nil {
					t.Fatal(err)
				}
				intent.SourceDigests[w.SourceKey(reference)] = value
			}
			ticket, err := store.AcquireNative(ctx, intent)
			if err != nil {
				t.Fatal(err)
			}
			var before, after client.Object
			switch kind {
			case "Team", "unselected Team sandbox":
				before = team
				desired := team.DeepCopy()
				if kind == "Team" {
					desired.Spec.AgentPolicyRef = "replacement-policy"
				} else {
					desired.Spec.SandboxRefs = append(desired.Spec.SandboxRefs, "another-spare")
				}
				after = desired
			case "Sandbox":
				before = box
				desired := box.DeepCopy()
				desired.Spec.Storage.Home.Size = resource.MustParse("64Gi")
				after = desired
			case "AgentPolicy":
				before = policy
				desired := policy.DeepCopy()
				desired.Spec.Network.BlockedFQDNs = []string{"blocked.example.com"}
				after = desired
			}
			gvk := api.GroupVersion.WithKind(kind)
			if kind == "unselected Team sandbox" {
				gvk = api.GroupVersion.WithKind("Team")
			}
			before.GetObjectKind().SetGroupVersionKind(gvk)
			after.GetObjectKind().SetGroupVersionKind(gvk)
			writeObjectFile(t, args[len(args)-3], before)
			writeObjectFile(t, args[len(args)-1], after)
			args = append(args, "--action", "update")
			writes := 0
			writer := interceptor.NewClient(storage, interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.UpdateOption) error {
					switch object.(type) {
					case *api.Team, *api.Sandbox, *api.AgentPolicy:
						writes++
					}
					return c.Update(ctx, object, opts...)
				},
			})
			code, status, message := invoke(t, args, injectedClients(writer))
			if kind == "unselected Team sandbox" {
				if code != 0 || status.Status != "completed" || writes != 1 {
					t.Fatalf("unselected Team Sandbox was blocked: code=%d status=%+v writes=%d message=%s", code, status, writes, message)
				}
				return
			}
			if code == 0 || writes != 0 || status.Status != "unavailable" {
				t.Fatalf("selected mutation overlapped native execution: code=%d status=%+v writes=%d", code, status, writes)
			}
			ticket, err = store.BeginNativeStop(ctx, ticket)
			if err != nil {
				t.Fatal(err)
			}
			if code, _, _ := invoke(t, args, injectedClients(writer)); code == 0 || writes != 0 {
				t.Fatal("entering native Stopping released a selected mutation")
			}
			// Models the controller's release after its quiescence check, not a
			// stop/fence capability of this configuration command.
			if err := store.ReleaseNative(ctx, ticket); err != nil {
				t.Fatal(err)
			}
			code, status, message = invoke(t, args, injectedClients(writer))
			if code != 0 || status.Status != "completed" || writes != 1 {
				t.Fatalf("quiescent release did not unblock the command: code=%d status=%+v writes=%d message=%s", code, status, writes, message)
			}
		})
	}
}
