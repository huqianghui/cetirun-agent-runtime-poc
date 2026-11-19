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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestTeamUpdateRecoversStatusOnlySupersededReservation(t *testing.T) {
	for _, boundary := range []string{"before-authoritative-read", "before-update", "reserved-after-transition-failure"} {
		t.Run(boundary, func(t *testing.T) {
			for _, order := range []string{"fresh-first", "stale-then-fresh"} {
				t.Run(order, func(t *testing.T) {
					ctx := context.Background()
					authority := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
						Namespace: "cetirun-system", Name: "configuration-coordination", UID: "authority-uid",
						Finalizers: []string{"sandbox.cetirun.microsoft.com/coordination"},
					}, Data: map[string]string{"coordination.json": `{"version":1,"executions":[],"changes":[]}`}}
					team := &api.Team{ObjectMeta: metav1.ObjectMeta{Name: "engineering", UID: "team-uid"}, Spec: api.TeamSpec{
						AgentPolicyRef: "standard", SandboxRefs: []string{"standard"}, DefaultSandboxRef: "standard",
					}}
					storage := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(team).WithObjects(authority, team).Build()
					if err := storage.Get(ctx, client.ObjectKeyFromObject(team), team); err != nil {
						t.Fatal(err)
					}
					original := ConfigurationVersion{UID: team.UID, ResourceVersion: team.ResourceVersion}

					journalChanges := func() []struct {
						Phase string `json:"phase"`
					} {
						t.Helper()
						current := &corev1.ConfigMap{}
						if err := storage.Get(ctx, client.ObjectKeyFromObject(authority), current); err != nil {
							t.Fatal(err)
						}
						var state struct {
							Changes []struct {
								Phase string `json:"phase"`
							} `json:"changes"`
						}
						if err := json.Unmarshal([]byte(current.Data["coordination.json"]), &state); err != nil {
							t.Fatal(err)
						}
						return state.Changes
					}

					statusWrites, teamUpdateAttempts := 0, 0
					statusRefresh := func(c client.WithWatch) error {
						current := &api.Team{}
						if err := c.Get(ctx, client.ObjectKeyFromObject(team), current); err != nil {
							return err
						}
						current.Status.ObservedGeneration = current.Generation
						current.Status.Compatibility = &api.TeamCompatibilityStatus{
							State: api.CompatibilityCompatible,
							Team:  api.CompatibilityReference{Name: current.Name, UID: string(current.UID), Generation: current.Generation},
						}
						statusWrites++
						return c.Status().Update(ctx, current)
					}
					refreshed, transitionFailed := false, false
					writer := interceptor.NewClient(storage.(client.WithWatch), interceptor.Funcs{
						Update: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.UpdateOption) error {
							if configMap, ok := object.(*corev1.ConfigMap); ok {
								var state struct {
									Changes []struct {
										Phase string `json:"phase"`
									} `json:"changes"`
								}
								if err := json.Unmarshal([]byte(configMap.Data["coordination.json"]), &state); err != nil {
									t.Fatal(err)
								}
								if boundary == "before-authoritative-read" && !refreshed && len(state.Changes) == 1 && state.Changes[0].Phase == "changing" {
									if err := c.Update(ctx, object, opts...); err != nil {
										return err
									}
									refreshed = true
									return statusRefresh(c)
								}
								if boundary == "reserved-after-transition-failure" && !transitionFailed && len(state.Changes) == 1 && state.Changes[0].Phase == "changing" {
									transitionFailed = true
									return errors.New("synthetic failure before persisting Changing")
								}
							}
							if _, ok := object.(*api.Team); ok {
								teamUpdateAttempts++
								if boundary == "before-update" && !refreshed {
									refreshed = true
									if err := statusRefresh(c); err != nil {
										return err
									}
								}
							}
							return c.Update(ctx, object, opts...)
						},
					})
					newPortal := func() *Client {
						coordination, err := configurationcoordination.New(writer, storage, configurationcoordination.Authority{
							Namespace: authority.Namespace, Name: authority.Name, UID: authority.UID,
						})
						if err != nil {
							t.Fatal(err)
						}
						portal := NewClient(writer)
						portal.SetConfigurationCoordination(coordination)
						return portal
					}
					desired := *team.Spec.DeepCopy()
					desired.MemberContext.Role = "Engineer"
					if _, err := newPortal().UpdateTeamSpec(ctx, team.Name, desired, original); !IsConfigurationCoordinationUnavailable(err) {
						t.Fatalf("initial save did not retain an uncertain superseded write: %v", err)
					}
					if boundary == "reserved-after-transition-failure" {
						if !transitionFailed {
							t.Fatal("fixture did not leave the operation Reserved")
						}
						refreshed = true
						if err := statusRefresh(storage); err != nil {
							t.Fatal("status refresh after the Reserved operation:", err)
						}
					}
					if !refreshed || statusWrites != 1 {
						t.Fatalf("fixture did not inject exactly one status refresh: refreshed=%v statusWrites=%d", refreshed, statusWrites)
					}
					changes := journalChanges()
					expectedPhase := "changing"
					if boundary == "reserved-after-transition-failure" {
						expectedPhase = "reserved"
					}
					if len(changes) != 1 || changes[0].Phase != expectedPhase {
						t.Fatalf("initial conflict did not retain a %s reservation: %+v", expectedPhase, changes)
					}
					current := &api.Team{}
					if err := storage.Get(ctx, client.ObjectKeyFromObject(team), current); err != nil {
						t.Fatal(err)
					}
					if current.ResourceVersion == original.ResourceVersion || current.Spec.MemberContext.Role != "" || current.Status.Compatibility == nil {
						t.Fatalf("status-only refresh did not preserve the original writable Team: %+v", current)
					}

					fresh := ConfigurationVersion{UID: current.UID, ResourceVersion: current.ResourceVersion}
					if order == "stale-then-fresh" {
						attemptsBeforeRecovery := teamUpdateAttempts
						if _, err := newPortal().UpdateTeamSpec(ctx, current.Name, desired, original); !errors.Is(err, ErrConfigurationChanged) {
							t.Fatalf("stale caller was silently rebased while recovering the reservation: %v", err)
						}
						if teamUpdateAttempts != attemptsBeforeRecovery {
							t.Fatalf("stale recovery submitted %d unexpected Team updates", teamUpdateAttempts-attemptsBeforeRecovery)
						}
						if changes := journalChanges(); len(changes) != 0 {
							t.Fatalf("stale recovery did not release the safely superseded reservation: %+v", changes)
						}
					}
					updated, err := newPortal().UpdateTeamSpec(ctx, current.Name, desired, fresh)
					if err != nil {
						t.Fatal("fresh save did not recover and apply after the status-only race:", err)
					}
					if updated.Spec.MemberContext.Role != "Engineer" || updated.Status.Compatibility == nil || updated.Status.Compatibility.State != api.CompatibilityCompatible {
						t.Fatalf("fresh save did not preserve status or apply the requested spec: %+v", updated)
					}
					if changes := journalChanges(); len(changes) != 0 {
						t.Fatalf("safe supersession recovery retained reservations: %+v", changes)
					}
					expectedAttempts := 1
					if boundary == "before-update" {
						expectedAttempts = 2
					}
					if teamUpdateAttempts != expectedAttempts {
						t.Fatalf("got %d Team update attempts, want %d (only the original fenced request and fresh save)", teamUpdateAttempts, expectedAttempts)
					}

					if order == "fresh-first" {
						if _, err := newPortal().UpdateTeamSpec(ctx, updated.Name, desired, original); !errors.Is(err, ErrConfigurationChanged) {
							t.Fatalf("stale caller was silently rebased after recovery: %v", err)
						}
					}
				})
			}
		})
	}
}
