// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Azure/CetiRun/app/cetirun-operator/configurationcoordination"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestTeamAndSandboxChangesUseExactVersionsAndCoordination(t *testing.T) {
	for _, kind := range []string{"Team", "Sandbox"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			meta := metav1.ObjectMeta{Name: "example", UID: "configuration-uid", Labels: map[string]string{"example.com/keep": "yes"}}
			var object client.Object = &api.Team{ObjectMeta: meta, Spec: api.TeamSpec{AgentPolicyRef: "old", SandboxRefs: []string{"standard"}, DefaultSandboxRef: "standard"}}
			if kind == "Sandbox" {
				object = &api.Sandbox{ObjectMeta: meta, Spec: api.SandboxSpec{Type: api.SandboxKata}}
			}
			storage := newFakeClient(object)
			if err := storage.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
				t.Fatal(err)
			}
			original := ConfigurationVersion{UID: object.GetUID(), ResourceVersion: object.GetResourceVersion()}
			coordination := &recordingPolicyCoordination{storage: storage}
			portal := NewClient(storage)
			portal.SetConfigurationCoordination(coordination)
			update := func(version ConfigurationVersion) (client.Object, error) {
				if kind == "Team" {
					return portal.UpdateTeamSpec(ctx, object.GetName(), api.TeamSpec{AgentPolicyRef: "new", SandboxRefs: []string{"restricted"}, DefaultSandboxRef: "restricted"}, version)
				}
				return portal.UpdateSandboxSpec(ctx, object.GetName(), api.SandboxSpec{Type: api.SandboxGVisor}, version)
			}
			for _, invalid := range []ConfigurationVersion{{}, {UID: "replacement", ResourceVersion: original.ResourceVersion}, {UID: original.UID, ResourceVersion: "stale"}} {
				if _, err := update(invalid); !errors.Is(err, ErrConfigurationChanged) {
					t.Fatalf("invalid version accepted: %v", err)
				}
				if coordination.before != nil {
					t.Fatal("invalid version entered coordination")
				}
			}
			updated, err := update(original)
			if err != nil {
				t.Fatal(err)
			}
			if coordination.before == nil || coordination.after == nil || coordination.before.GetUID() != original.UID || coordination.before.GetResourceVersion() != original.ResourceVersion || coordination.after.GetResourceVersion() != original.ResourceVersion {
				t.Fatal("configuration update did not preserve the exact reviewed before/after identity")
			}
			if updated.GetLabels()["example.com/keep"] != "yes" {
				t.Fatal("configuration update dropped unrelated metadata")
			}
			current := ConfigurationVersion{UID: updated.GetUID(), ResourceVersion: updated.GetResourceVersion()}
			coordination.before, coordination.after = nil, nil
			if _, err := update(current); err != nil {
				t.Fatal(err)
			}
			if coordination.before != nil {
				t.Fatal("unchanged spec should not reserve a configuration operation")
			}
			remove := func(version ConfigurationVersion) error {
				if kind == "Team" {
					return portal.DeleteTeam(ctx, object.GetName(), version)
				}
				return portal.DeleteSandbox(ctx, object.GetName(), version)
			}
			if err := remove(original); !errors.Is(err, ErrConfigurationChanged) {
				t.Fatalf("stale deletion accepted: %v", err)
			}
			if coordination.deleted != nil {
				t.Fatal("stale deletion entered coordination")
			}
			if err := remove(current); err != nil {
				t.Fatal(err)
			}
			if coordination.deleted == nil || coordination.deleted.GetUID() != current.UID || coordination.deleted.GetResourceVersion() != current.ResourceVersion {
				t.Fatal("deletion was not bound to the reviewed object")
			}
		})
	}
}

func TestConfigurationUpdateRecoversAfterPortalRestart(t *testing.T) {
	for _, kind := range []string{"Team", "Sandbox", "AgentPolicy"} {
		for _, failure := range []string{"lost-update-reply", "failed-release"} {
			for _, retry := range []string{"old-version", "refreshed-noop"} {
				t.Run(kind+"/"+failure+"/"+retry, func(t *testing.T) {
					ctx := context.Background()
					authority := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
						Namespace: "cetirun-system", Name: "configuration-coordination", UID: "authority-uid",
						Finalizers: []string{"sandbox.cetirun.microsoft.com/coordination"}},
						Data: map[string]string{"coordination.json": `{"version":1,"executions":[],"changes":[]}`}}
					meta := metav1.ObjectMeta{Name: "example", UID: "configuration-uid"}
					var object client.Object = &api.Team{ObjectMeta: meta, Spec: api.TeamSpec{AgentPolicyRef: "standard", SandboxRefs: []string{"standard"}, DefaultSandboxRef: "standard"}}
					if kind == "Sandbox" {
						object = &api.Sandbox{ObjectMeta: meta, Spec: api.SandboxSpec{Type: api.SandboxKata}}
					}
					if kind == "AgentPolicy" {
						object = &api.AgentPolicy{ObjectMeta: meta, Spec: api.AgentPolicySpec{Harnesses: api.AgentPolicyHarnesses{Allowed: []api.AgentType{api.AgentTypeHermes}}}}
					}
					storage := newFakeClient(authority, object)
					if err := storage.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
						t.Fatal(err)
					}
					version := ConfigurationVersion{UID: object.GetUID(), ResourceVersion: object.GetResourceVersion()}
					writes := 0
					failing := true
					writer := interceptor.NewClient(storage.(client.WithWatch), interceptor.Funcs{
						Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
							if journal, ok := obj.(*corev1.ConfigMap); ok {
								var record struct {
									Changes []json.RawMessage `json:"changes"`
								}
								if err := json.Unmarshal([]byte(journal.Data["coordination.json"]), &record); err != nil {
									t.Fatal(err)
								}
								if failing && failure == "failed-release" && len(record.Changes) == 0 {
									return errors.New("synthetic failed release")
								}
								return c.Update(ctx, obj, opts...)
							}
							writes++
							if err := c.Update(ctx, obj, opts...); err != nil {
								return err
							}
							if failing && failure == "lost-update-reply" {
								return errors.New("synthetic lost reply after commit")
							}
							return nil
						},
					})
					newPortal := func() *Client {
						coordinator, err := configurationcoordination.New(writer, storage, configurationcoordination.Authority{Namespace: authority.Namespace, Name: authority.Name, UID: authority.UID})
						if err != nil {
							t.Fatal(err)
						}
						portal := NewClient(writer)
						portal.SetConfigurationCoordination(coordinator)
						return portal
					}
					update := func(portal *Client, version ConfigurationVersion) (client.Object, error) {
						switch current := object.(type) {
						case *api.Team:
							spec := *current.Spec.DeepCopy()
							spec.MemberContext.Role = "Engineer"
							return portal.UpdateTeamSpec(ctx, current.Name, spec, version)
						case *api.Sandbox:
							spec := *current.Spec.DeepCopy()
							spec.Type = api.SandboxGVisor
							return portal.UpdateSandboxSpec(ctx, current.Name, spec, version)
						case *api.AgentPolicy:
							spec := *current.Spec.DeepCopy()
							spec.Harnesses.Allowed = []api.AgentType{api.AgentTypeOpenClaw}
							return portal.UpdateAgentPolicySpec(ctx, current.Name, spec, AgentPolicyVersion(version))
						default:
							panic("unknown fixture kind")
						}
					}
					if _, err := update(newPortal(), version); !IsConfigurationCoordinationUnavailable(err) {
						t.Fatalf("expected retained uncertain update: %v", err)
					}
					if err := storage.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
						t.Fatal(err)
					}
					if object.GetResourceVersion() == version.ResourceVersion || writes != 1 {
						t.Fatal("fixture did not commit exactly once")
					}
					journal := &corev1.ConfigMap{}
					readChanges := func() int {
						if err := storage.Get(ctx, client.ObjectKeyFromObject(authority), journal); err != nil {
							t.Fatal(err)
						}
						var record struct {
							Changes []json.RawMessage `json:"changes"`
						}
						if err := json.Unmarshal([]byte(journal.Data["coordination.json"]), &record); err != nil {
							t.Fatal(err)
						}
						return len(record.Changes)
					}
					if readChanges() != 1 {
						t.Fatal("failure lost the durable reservation")
					}
					failing = false
					if retry == "refreshed-noop" {
						version.ResourceVersion = object.GetResourceVersion()
					}
					_, err := update(newPortal(), version)
					if retry == "old-version" {
						if !errors.Is(err, ErrConfigurationChanged) && !errors.Is(err, ErrAgentPolicyChanged) {
							t.Fatalf("stale retry bypassed version check: %v", err)
						}
					} else if err != nil {
						t.Fatal("same-spec recovery failed:", err)
					}
					if readChanges() != 0 || writes != 1 {
						t.Fatal("restart recovery failed to release or resubmitted the update")
					}
				})
			}
		}
	}
}
