// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/configurationcoordination"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestConfigurationReservedUpdateResumesExactRetryAfterRestart(t *testing.T) {
	for _, kind := range []string{"Team", "Sandbox", "AgentPolicy"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			authority := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
				Namespace: "cetirun-system", Name: "configuration-coordination", UID: "authority-uid",
				Finalizers: []string{"sandbox.cetirun.microsoft.com/coordination"}},
				Data: map[string]string{"coordination.json": `{"version":1,"executions":[],"changes":[]}`}}
			meta := metav1.ObjectMeta{Name: "example", UID: "configuration-uid"}
			var before client.Object
			switch kind {
			case "Team":
				before = &api.Team{ObjectMeta: meta, Spec: api.TeamSpec{AgentPolicyRef: "standard", SandboxRefs: []string{"standard"}, DefaultSandboxRef: "standard"}}
			case "Sandbox":
				before = &api.Sandbox{ObjectMeta: meta, Spec: api.SandboxSpec{Type: api.SandboxKata}}
			case "AgentPolicy":
				before = &api.AgentPolicy{ObjectMeta: meta, Spec: api.AgentPolicySpec{Harnesses: api.AgentPolicyHarnesses{Allowed: []api.AgentType{api.AgentTypeHermes}}}}
			}
			storage := newFakeClient(authority, before)
			if err := storage.Get(ctx, client.ObjectKeyFromObject(before), before); err != nil {
				t.Fatal(err)
			}
			version := ConfigurationVersion{UID: before.GetUID(), ResourceVersion: before.GetResourceVersion()}
			after := before.DeepCopyObject().(client.Object)
			different := before.DeepCopyObject().(client.Object)
			switch desired := after.(type) {
			case *api.Team:
				desired.Spec.MemberContext.Role = "Engineer"
				different.(*api.Team).Spec.MemberContext.Role = "Reviewer"
			case *api.Sandbox:
				desired.Spec.Type = api.SandboxGVisor
				different.(*api.Sandbox).Spec.Storage.Home.StorageClassName = "different-storage"
			case *api.AgentPolicy:
				desired.Spec.Harnesses.Allowed = []api.AgentType{api.AgentTypeOpenClaw}
				different.(*api.AgentPolicy).Spec.Harnesses.Allowed = []api.AgentType{api.AgentTypeCustom}
			}
			type change struct {
				ID    string `json:"id"`
				Phase string `json:"phase"`
			}
			readChanges := func(journal *corev1.ConfigMap) []change {
				t.Helper()
				var record struct {
					Changes []change `json:"changes"`
				}
				if err := json.Unmarshal([]byte(journal.Data["coordination.json"]), &record); err != nil {
					t.Fatal(err)
				}
				return record.Changes
			}
			writes, advances := 0, 0
			failing := true
			var operationID string
			writer := interceptor.NewClient(storage.(client.WithWatch), interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.UpdateOption) error {
					if journal, ok := object.(*corev1.ConfigMap); ok {
						changes := readChanges(journal)
						if len(changes) == 1 && changes[0].Phase == "changing" {
							if failing {
								return errors.New("synthetic failure before persisting Changing")
							}
							if changes[0].ID != operationID {
								t.Fatal("retry replaced the reserved operation")
							}
							advances++
						}
					} else {
						writes++
					}
					return c.Update(ctx, object, opts...)
				},
			})
			update := func(desired client.Object, reviewed ConfigurationVersion) (client.Object, error) {
				coordinator, err := configurationcoordination.New(writer, storage, configurationcoordination.Authority{Namespace: authority.Namespace, Name: authority.Name, UID: authority.UID})
				if err != nil {
					t.Fatal(err)
				}
				portal := NewClient(writer)
				portal.SetConfigurationCoordination(coordinator)
				switch object := desired.(type) {
				case *api.Team:
					return portal.UpdateTeamSpec(ctx, object.Name, object.Spec, reviewed)
				case *api.Sandbox:
					return portal.UpdateSandboxSpec(ctx, object.Name, object.Spec, reviewed)
				case *api.AgentPolicy:
					return portal.UpdateAgentPolicySpec(ctx, object.Name, object.Spec, AgentPolicyVersion(reviewed))
				default:
					panic("unknown fixture kind")
				}
			}
			if _, err := update(after, version); err == nil {
				t.Fatal("expected interrupted update")
			}
			journal := &corev1.ConfigMap{}
			if err := storage.Get(ctx, client.ObjectKeyFromObject(authority), journal); err != nil {
				t.Fatal(err)
			}
			changes := readChanges(journal)
			if len(changes) != 1 || changes[0].Phase != "reserved" || writes != 0 {
				t.Fatalf("fixture did not retain an unsubmitted reservation: changes=%+v writes=%d", changes, writes)
			}
			operationID = changes[0].ID
			reservedJournal := journal.Data["coordination.json"]
			failing = false
			for _, retry := range []struct {
				name    string
				desired client.Object
				version ConfigurationVersion
			}{
				{"changed-request", different, version},
				{"unchanged-save", before, version},
				{"missing-version", after, ConfigurationVersion{}},
				{"stale-version", after, ConfigurationVersion{UID: version.UID, ResourceVersion: "stale"}},
				{"wrong-uid", after, ConfigurationVersion{UID: "replacement", ResourceVersion: version.ResourceVersion}},
			} {
				t.Run(retry.name, func(t *testing.T) {
					if _, err := update(retry.desired, retry.version); err == nil {
						t.Fatal("inexact retry succeeded")
					}
					if err := storage.Get(ctx, client.ObjectKeyFromObject(authority), journal); err != nil {
						t.Fatal(err)
					}
					if journal.Data["coordination.json"] != reservedJournal || writes != 0 || advances != 0 {
						t.Fatal("inexact retry changed the reservation or submitted a write")
					}
				})
			}
			updated, err := update(after, version)
			if err != nil {
				t.Fatal("exact retry failed:", err)
			}
			if err := storage.Get(ctx, client.ObjectKeyFromObject(authority), journal); err != nil {
				t.Fatal(err)
			}
			if len(readChanges(journal)) != 0 || writes != 1 || advances != 1 || updated.GetResourceVersion() == version.ResourceVersion {
				t.Fatalf("exact retry did not commit once and release: writes=%d advances=%d", writes, advances)
			}
		})
	}
}
