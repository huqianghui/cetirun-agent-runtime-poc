// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestPlannedMigrationRejectsPreflightWithoutStopping(t *testing.T) {
	for _, fault := range []string{"disabled", "wrong UID", "zone", "taint", "CSI driver", "restart pending", "attacher missing"} {
		t.Run(fault, func(t *testing.T) {
			f := newMigrationFixture(t, "openclaw", false)
			ctx := context.Background()
			node := &corev1.Node{}
			if err := f.kube.Get(ctx, client.ObjectKey{Name: "node-b"}, node); err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "disabled":
				f.r.SandboxPreparation.PlannedMigration = false
			case "zone":
				node.Labels["topology.kubernetes.io/zone"] = "different-zone"
				if err := f.kube.Update(ctx, node); err != nil {
					t.Fatal(err)
				}
			case "taint":
				node.Spec.Taints = []corev1.Taint{{Key: "example.com/dedicated", Value: "other", Effect: corev1.TaintEffectNoSchedule}}
				if err := f.kube.Update(ctx, node); err != nil {
					t.Fatal(err)
				}
			case "CSI driver":
				csi := &storagev1.CSINode{}
				if err := f.kube.Get(ctx, client.ObjectKey{Name: node.Name}, csi); err != nil {
					t.Fatal(err)
				}
				csi.Spec.Drivers = nil
				if err := f.kube.Update(ctx, csi); err != nil {
					t.Fatal(err)
				}
			case "attacher missing":
				attachment := &storagev1.VolumeAttachment{}
				if err := f.kube.Get(ctx, client.ObjectKey{Name: "source-attachment"}, attachment); err != nil {
					t.Fatal(err)
				}
				attachment.Finalizers = nil
				if err := f.kube.Update(ctx, attachment); err != nil {
					t.Fatal(err)
				}
			}
			requestMigration(t, f, "migration-00000001", "node-b")
			agent := f.current(t)
			if fault == "wrong UID" {
				agent.Spec.PlannedMigration.Target.UID = "different-node"
			}
			if fault == "restart pending" {
				agent.Spec.RestartRequestID = "restart-00000001"
			}
			if err := f.kube.Update(ctx, agent); err != nil {
				t.Fatal(err)
			}
			stages := f.installer.stages
			if _, err := f.reconcile(); err == nil {
				t.Fatal("invalid migration was not rejected")
			}
			if f.current(t).Status.PlannedMigration != nil || f.installer.stages != stages ||
				ptr.Deref(f.workload(t).Spec.Replicas, 0) != 1 {
				t.Fatal("failed migration preflight changed the source lifetime")
			}
		})
	}
}

func TestPlannedMigrationDisabledStillRechecksAuthorization(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		t.Run(kind, func(t *testing.T) {
			f := newMigrationFixture(t, kind, false)
			ctx := context.Background()
			f.r.SandboxPreparation.PlannedMigration = false
			requestMigration(t, f, "migration-00000001", "node-b")
			request := *f.current(t).Spec.PlannedMigration
			stages := f.installer.stages
			if _, err := f.reconcile(); err == nil || ptr.Deref(f.workload(t).Spec.Replicas, -1) != 1 {
				t.Fatal("disabled migration must reject without stopping an authorized source:", err)
			}
			policy := &api.AgentPolicy{}
			if err := f.kube.Get(ctx, client.ObjectKey{Name: f.current(t).Status.Policy.EffectivePolicyRef.Name}, policy); err != nil {
				t.Fatal(err)
			}
			setCondition(&policy.Status.Conditions, api.ConditionAgentPolicyAccepted, metav1.ConditionFalse,
				"Denied", "synthetic policy revocation")
			if err := f.kube.Status().Update(ctx, policy); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if _, err := f.reconcile(); err == nil {
					t.Fatal("disabled request or source denial was hidden")
				}
				agent := f.current(t)
				if agent.Status.PlannedMigration != nil || *agent.Spec.PlannedMigration != request ||
					f.installer.stages != stages || ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 {
					t.Fatal("rejected migration prevented source revocation or changed the request")
				}
				for _, key := range f.gateway.keys {
					if !key.Blocked || ptr.Deref(key.Spend, 0) != 3.25 {
						t.Fatal("a disabled migration left revoked model access usable")
					}
				}
			}
		})
	}
}

func TestPlannedMigrationProtectingIsolatesCurrentAccess(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, control := range []string{"suspended", "blocked CIDR", "blocked port"} {
			t.Run(kind+"/"+control, func(t *testing.T) {
				f := newMigrationFixture(t, kind, false, func(r *AgentReconciler, c client.Client) {
					if control != "blocked port" {
						return
					}
					config := &corev1.ConfigMap{}
					ctx := context.Background()
					if err := c.Get(ctx, client.ObjectKey{Namespace: r.OperatorNamespace, Name: "sandbox-platform"}, config); err != nil {
						t.Fatal(err)
					}
					var platform SandboxPlatformConfiguration
					if err := json.Unmarshal([]byte(config.Data[SandboxPlatformConfigurationKey]), &platform); err != nil {
						t.Fatal(err)
					}
					registration := platform.Registrations[api.SandboxKata]
					registration.Capabilities.BlockedPorts = true
					platform.Registrations[api.SandboxKata] = registration
					raw, err := json.Marshal(platform)
					if err != nil {
						t.Fatal(err)
					}
					config.Data[SandboxPlatformConfigurationKey] = string(raw)
					if err := c.Update(ctx, config); err != nil {
						t.Fatal(err)
					}
				})
				ctx := context.Background()
				requestMigration(t, f, "migration-00000001", "node-b")
				f.reconcileOK(t)
				state := f.current(t).Status.PlannedMigration.DeepCopy()
				expected := reconciledExpectationForTest(t, f)
				agent := f.current(t)
				if control == "suspended" {
					agent.Spec.Suspended = true
					agent.Generation++
					if err := f.kube.Update(ctx, agent); err != nil {
						t.Fatal(err)
					}
				} else {
					policy := &api.AgentPolicy{}
					if err := f.kube.Get(ctx, client.ObjectKey{Name: agent.Status.Policy.EffectivePolicyRef.Name}, policy); err != nil {
						t.Fatal(err)
					}
					if control == "blocked CIDR" {
						policy.Spec.Network.BlockedCIDRs = []string{"203.0.113.0/24"}
					} else {
						policy.Spec.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 8443, Protocol: corev1.ProtocolTCP}}
					}
					if err := f.kube.Update(ctx, policy); err != nil {
						t.Fatal(err)
					}
					if _, err := f.r.resolvePolicy(ctx, f.current(t)); err != nil {
						t.Fatal("tightened network policy must remain a supported configuration:", err)
					}
				}
				protectionErr := errors.New("synthetic attachment protection failure")
				f.r.Client = interceptor.NewClient(f.kube, interceptor.Funcs{
					Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
						if _, attachment := object.(*storagev1.VolumeAttachment); attachment {
							return protectionErr
						}
						return c.Patch(ctx, object, patch, opts...)
					},
				})
				for range 2 {
					if _, err := f.reconcile(); !errors.Is(err, protectionErr) {
						t.Fatal("source protection failure was hidden:", err)
					}
					if !reflect.DeepEqual(f.current(t).Status.PlannedMigration, state) ||
						ptr.Deref(f.workload(t).Spec.Replicas, -1) != 1 {
						t.Fatal("isolation must preserve the unprotected source observations")
					}
					for _, key := range f.gateway.keys {
						if !key.Blocked || ptr.Deref(key.Spend, 0) != 3.25 {
							t.Error("model access was not independently revoked")
						}
					}
					route := &istiov1beta1.VirtualService{}
					if err := f.kube.Get(ctx, client.ObjectKey{Namespace: state.Namespace.Name, Name: virtualServiceName(agent.Name)}, route); !apierrors.IsNotFound(err) {
						t.Error("the source route remained usable:", err)
					}
					found := false
					for _, recorded := range expected.Controls {
						if recorded.GetKind() != "CiliumNetworkPolicy" {
							continue
						}
						found = true
						actual := &unstructured.Unstructured{}
						actual.SetGroupVersionKind(recorded.GroupVersionKind())
						if err := f.kube.Get(ctx, client.ObjectKeyFromObject(&recorded), actual); err != nil {
							t.Fatal(err)
						}
						for field, direction := range map[string]string{"ingressDeny": "fromEntities", "egressDeny": "toEntities"} {
							rules, _, err := unstructured.NestedSlice(actual.Object, "spec", field)
							want := []any{map[string]any{direction: []any{"all"}}}
							if err != nil || !reflect.DeepEqual(rules, want) {
								t.Errorf("source %s is not an all-traffic deny: %v", field, err)
							}
						}
						before, _, _ := unstructured.NestedMap(recorded.Object, "spec", "endpointSelector")
						after, _, _ := unstructured.NestedMap(actual.Object, "spec", "endpointSelector")
						if !reflect.DeepEqual(before, after) {
							t.Fatal("isolation changed the Agent/namespace selector")
						}
					}
					if !found {
						t.Fatal("fixture has no independently observed network policy")
					}
				}
			})
		}
	}
}

func TestPlannedMigrationIsolationFailuresStayIndependent(t *testing.T) {
	for _, fault := range []string{"foreign owner", "changed selector", "network patch", "network readback", "Gateway read"} {
		t.Run(fault, func(t *testing.T) {
			f := newMigrationFixture(t, "openclaw", false)
			ctx := context.Background()
			requestMigration(t, f, "migration-00000001", "node-b")
			f.reconcileOK(t)
			agent := f.current(t)
			state := agent.Status.PlannedMigration.DeepCopy()
			expected := reconciledExpectationForTest(t, f)
			var network *unstructured.Unstructured
			for _, object := range expected.Controls {
				if object.GetKind() == "CiliumNetworkPolicy" {
					network = object.DeepCopy()
				}
			}
			if network == nil {
				t.Fatal("fixture has no network control")
			}
			if err := f.kube.Get(ctx, client.ObjectKeyFromObject(network), network); err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "foreign owner":
				owners := network.GetOwnerReferences()
				owners[0].UID = "foreign-agent"
				network.SetOwnerReferences(owners)
			case "changed selector":
				if err := unstructured.SetNestedMap(network.Object, map[string]any{}, "spec", "endpointSelector"); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.kube.Update(ctx, network); err != nil {
				t.Fatal(err)
			}
			before := network.DeepCopy()
			injected := errors.New("synthetic isolation dependency failure")
			patched := false
			f.r.Client = interceptor.NewClient(f.kube, interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if object.GetObjectKind().GroupVersionKind().Kind == "CiliumNetworkPolicy" {
						if fault == "network patch" {
							return injected
						}
						patched = true
					}
					return c.Patch(ctx, object, patch, opts...)
				},
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
					if fault == "network readback" && patched && object.GetObjectKind().GroupVersionKind().Kind == "CiliumNetworkPolicy" {
						return injected
					}
					return c.Get(ctx, key, object, opts...)
				},
			})
			f.r.APIReader = f.r.Client
			if fault == "Gateway read" {
				f.gateway.getErr = injected
			}
			if err := f.r.isolateMigrationSourceAccess(ctx, agent); err == nil ||
				((fault == "network patch" || fault == "network readback" || fault == "Gateway read") && !errors.Is(err, injected)) {
				t.Fatal("independent isolation failure was hidden:", err)
			}
			if err := f.kube.Get(ctx, client.ObjectKeyFromObject(network), network); err != nil {
				t.Fatal(err)
			}
			if fault == "foreign owner" || fault == "changed selector" || fault == "network patch" {
				if patched || !reflect.DeepEqual(before.Object["spec"], network.Object["spec"]) {
					t.Fatal("isolation adopted or changed an unconfirmed policy")
				}
			} else {
				rules, _, err := unstructured.NestedSlice(network.Object, "spec", "egressDeny")
				if err != nil || !reflect.DeepEqual(rules, []any{map[string]any{"toEntities": []any{"all"}}}) {
					t.Fatal("Gateway/readback failure skipped the network isolation attempt")
				}
			}
			route := &istiov1beta1.VirtualService{}
			if err := f.kube.Get(ctx, client.ObjectKey{Namespace: state.Namespace.Name, Name: virtualServiceName(agent.Name)}, route); !apierrors.IsNotFound(err) {
				t.Fatal("an independent failure skipped route withdrawal:", err)
			}
			if ptr.Deref(f.workload(t).Spec.Replicas, -1) != 1 ||
				!reflect.DeepEqual(f.current(t).Status.PlannedMigration, state) {
				t.Fatal("failed isolation destroyed or advanced the unprotected source")
			}
		})
	}
}

func TestPlannedMigrationProtectionRetriesFailedAndLostWrites(t *testing.T) {
	for _, kind := range []string{"pod", "attachment"} {
		for _, committed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/committed=%t", kind, committed), func(t *testing.T) {
				f := newMigrationFixture(t, "openclaw", true)
				requestMigration(t, f, "migration-00000001", "node-b")
				f.reconcileOK(t)
				injected := errors.New("synthetic protection write failure")
				failed := false
				original := f.r.Client
				f.r.Client = interceptor.NewClient(f.kube, interceptor.Funcs{
					Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
						_, pod := object.(*corev1.Pod)
						_, attachment := object.(*storagev1.VolumeAttachment)
						if !failed && ((kind == "pod" && pod) || (kind == "attachment" && attachment)) {
							failed = true
							if committed {
								if err := c.Patch(ctx, object, patch, opts...); err != nil {
									return err
								}
							}
							return injected
						}
						return c.Patch(ctx, object, patch, opts...)
					},
				})
				if _, err := f.reconcile(); !errors.Is(err, injected) {
					t.Fatal("protection failure was hidden:", err)
				}
				migrationPhase(t, f, api.MigrationProtecting)
				if ptr.Deref(f.workload(t).Spec.Replicas, 0) != 1 {
					t.Fatal("partial protection stopped the unprotected source")
				}
				f.r.Client = original
				f.reconcileOK(t)
				migrationPhase(t, f, api.MigrationStopping)
			})
		}
	}
}

func deleteMigrationSourceOnScaleDown(ctx context.Context, c client.Client, object client.Object, state *api.PlannedMigrationStatus) (bool, error) {
	workload, ok := object.(*appsv1.StatefulSet)
	if !ok || ptr.Deref(workload.Spec.Replicas, -1) != 0 {
		return false, nil
	}
	pod := &corev1.Pod{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: state.Namespace.Name, Name: state.SourcePod.Name}, pod); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	attachment := &storagev1.VolumeAttachment{}
	if err := c.Get(ctx, client.ObjectKey{Name: state.Attachment.Name}, attachment); err != nil {
		return false, err
	}
	unprotected := !slices.Contains(pod.Finalizers, api.MigrationPodFinalizer) ||
		!slices.Contains(attachment.Finalizers, api.MigrationAttachmentFinalizer)
	return unprotected, c.Delete(ctx, pod)
}

func TestPlannedMigrationProtectingSurvivesEarlierHoldFailures(t *testing.T) {
	faults := []struct {
		name      string
		suspended bool
		committed bool
	}{
		{name: "bootstrap receipt"},
		{name: "bootstrap receipt", suspended: true},
		{name: "HOME", suspended: true},
		{name: "source Node", suspended: true},
		{name: "source Pod", suspended: true},
		{name: "attachment", suspended: true},
		{name: "hold write", suspended: true},
		{name: "pod write", suspended: true},
		{name: "pod write", suspended: true, committed: true},
		{name: "attachment write", suspended: true},
		{name: "attachment write", suspended: true, committed: true},
	}
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, fault := range faults {
			t.Run(fmt.Sprintf("%s/%s/suspended=%t/committed=%t", kind, fault.name, fault.suspended, fault.committed), func(t *testing.T) {
				ctx := context.Background()
				f := newMigrationFixture(t, kind, true)
				requestMigration(t, f, "migration-00000001", "node-b")
				f.reconcileOK(t)
				state := f.current(t).Status.PlannedMigration.DeepCopy()
				if fault.suspended {
					agent := f.current(t)
					agent.Spec.Suspended = true
					agent.Generation++
					if err := f.kube.Update(ctx, agent); err != nil {
						t.Fatal(err)
					}
				}
				down, unprotectedStop := true, false
				injected := errors.New("synthetic migration protection dependency failure")
				f.r.Client = interceptor.NewClient(f.kube, interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
						_, home := object.(*corev1.PersistentVolumeClaim)
						_, node := object.(*corev1.Node)
						_, pod := object.(*corev1.Pod)
						_, attachment := object.(*storagev1.VolumeAttachment)
						_, receipt := object.(*corev1.ConfigMap)
						if down && ((fault.name == "HOME" && home) || (fault.name == "source Node" && node) ||
							(fault.name == "source Pod" && pod) || (fault.name == "attachment" && attachment) ||
							(fault.name == "bootstrap receipt" && receipt && key == f.r.sandboxBootstrapKey(f.current(t)))) {
							return injected
						}
						return c.Get(ctx, key, object, opts...)
					},
					Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
						_, agent := object.(*api.Agent)
						_, pod := object.(*corev1.Pod)
						_, attachment := object.(*storagev1.VolumeAttachment)
						if down && ((fault.name == "hold write" && agent) || (fault.name == "pod write" && pod) ||
							(fault.name == "attachment write" && attachment)) {
							if fault.committed {
								if err := c.Patch(ctx, object, patch, opts...); err != nil {
									return err
								}
							}
							return injected
						}
						if err := c.Patch(ctx, object, patch, opts...); err != nil {
							return err
						}
						unsafe, err := deleteMigrationSourceOnScaleDown(ctx, c, object, state)
						unprotectedStop = unprotectedStop || unsafe
						return err
					},
				})
				f.r.APIReader = f.r.Client
				for attempt := range 2 {
					_, err := f.reconcile()
					if fault.name != "bootstrap receipt" && (!fault.committed || attempt == 0) && !errors.Is(err, injected) {
						t.Fatal("protection failure was not surfaced:", err)
					}
				}
				if unprotectedStop {
					t.Fatal("an earlier hold removed captured observations before both protections existed")
				}
				pod := &corev1.Pod{}
				if err := f.kube.Get(ctx, client.ObjectKey{Namespace: state.Namespace.Name, Name: state.SourcePod.Name}, pod); err != nil ||
					pod.UID != state.SourcePod.UID {
					t.Fatal("the accepted source Pod was lost:", err)
				}
				if fault.name != "bootstrap receipt" && !fault.committed {
					migrationPhase(t, f, api.MigrationProtecting)
					if ptr.Deref(f.workload(t).Spec.Replicas, -1) != 1 {
						t.Fatal("failed protection stopped the source")
					}
				}
				down = false
				f.reconcileOK(t)
				migrationPhase(t, f, api.MigrationStopping)
				if fault.suspended {
					if f.current(t).Status.Phase == api.PhaseSuspended {
						t.Fatal("suspension was acknowledged before source termination")
					}
					agent := f.current(t)
					agent.Spec.Suspended = false
					agent.Generation++
					if err := f.kube.Update(ctx, agent); err != nil {
						t.Fatal(err)
					}
				}
				finishMigration(t, f)
				current := f.current(t).Status.PlannedMigration
				if current.SourcePod != state.SourcePod || current.Attachment != state.Attachment ||
					current.PlannedMigrationRequest != state.PlannedMigrationRequest || !current.StartedAt.Equal(&state.StartedAt) {
					t.Fatal("recovery replaced the accepted source observations")
				}
			})
		}
	}
}

func TestPlannedMigrationProtectionFailureRechecksAccess(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, failure := range []string{"source read", "attachment write"} {
			for _, access := range []string{"unchanged", "models withdrawn", "budget tightened", "Team outage", "AgentPolicy outage", "catalog outage"} {
				t.Run(kind+"/"+failure+"/"+access, func(t *testing.T) {
					ctx := context.Background()
					f := newMigrationFixture(t, kind, true)
					requestMigration(t, f, "migration-00000001", "node-b")
					f.reconcileOK(t)
					state := f.current(t).Status.PlannedMigration.DeepCopy()
					keyID, stages := f.current(t).Status.AIGateway.KeyID, f.installer.stages
					policy := &api.AgentPolicy{}
					if err := f.kube.Get(ctx, client.ObjectKey{Name: f.current(t).Status.Policy.EffectivePolicyRef.Name}, policy); err != nil {
						t.Fatal(err)
					}
					originalPolicy := policy.Spec.DeepCopy()
					savePolicy := func() {
						t.Helper()
						policy.Generation++
						if err := f.kube.Update(ctx, policy); err != nil {
							t.Fatal(err)
						}
						policy.Status.ObservedGeneration = policy.Generation
						for index := range policy.Status.Conditions {
							policy.Status.Conditions[index].ObservedGeneration = policy.Generation
						}
						if err := f.kube.Status().Update(ctx, policy); err != nil {
							t.Fatal(err)
						}
					}
					protectionDown, dependencyDown, unprotectedStop := true, false, false
					protectionErr := errors.New("synthetic source protection failure")
					dependencyErr := errors.New("synthetic authorization read failure")
					f.r.Client = interceptor.NewClient(f.kube, interceptor.Funcs{
						Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
							_, pod := object.(*corev1.Pod)
							_, team := object.(*api.Team)
							_, policy := object.(*api.AgentPolicy)
							if protectionDown && failure == "source read" && pod {
								return protectionErr
							}
							if dependencyDown && ((access == "Team outage" && team) || (access == "AgentPolicy outage" && policy)) {
								return dependencyErr
							}
							return c.Get(ctx, key, object, opts...)
						},
						Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
							if _, attachment := object.(*storagev1.VolumeAttachment); attachment && protectionDown && failure == "attachment write" {
								return protectionErr
							}
							if err := c.Patch(ctx, object, patch, opts...); err != nil {
								return err
							}
							unsafe, err := deleteMigrationSourceOnScaleDown(ctx, c, object, state)
							unprotectedStop = unprotectedStop || unsafe
							return err
						},
					})
					f.r.APIReader = f.r.Client
					f.r.aiGatewayClientFactory = func(context.Context) (aiGatewayKeyClient, error) {
						if dependencyDown && access == "catalog outage" {
							return &failingListGateway{fakeAIGateway: f.gateway}, nil
						}
						return f.gateway, nil
					}
					if _, err := f.reconcile(); !errors.Is(err, protectionErr) {
						t.Fatal("initial protection failure was not surfaced:", err)
					}
					for _, key := range f.gateway.keys {
						if key.Blocked {
							t.Fatal("unchanged source authorization was unnecessarily blocked")
						}
					}
					switch access {
					case "models withdrawn":
						policy.Spec.ModelAccess.SharedModels.Allowed = nil
						savePolicy()
					case "budget tightened":
						policy.Spec.ModelAccess.GatewayBudget.DefaultMonthlyUSD = "5"
						savePolicy()
					default:
						dependencyDown = true
					}
					for range 2 {
						result, err := f.reconcile()
						if !errors.Is(err, protectionErr) || result.RequeueAfter == 0 {
							t.Fatalf("protection failure did not remain retryable: result=%+v err=%v", result, err)
						}
						if (access == "Team outage" || access == "AgentPolicy outage") && !errors.Is(err, dependencyErr) {
							t.Fatal("protection retry skipped the current authorization read:", err)
						}
						current := f.current(t)
						migrationPhase(t, f, api.MigrationProtecting)
						if unprotectedStop || ptr.Deref(f.workload(t).Spec.Replicas, -1) != 1 || f.installer.stages != stages {
							t.Fatal("key authorization stopped or replaced the unprotected source")
						}
						pod := &corev1.Pod{}
						if err := f.kube.Get(ctx, client.ObjectKey{Namespace: state.Namespace.Name, Name: state.SourcePod.Name}, pod); err != nil ||
							pod.UID != state.SourcePod.UID || !pod.DeletionTimestamp.IsZero() {
							t.Fatal("the captured source observation was lost:", err)
						}
						if !reflect.DeepEqual(current.Status.PlannedMigration, state) || current.Status.AIGateway.KeyID != keyID ||
							len(f.gateway.keys) != 1 || len(f.gateway.generated) != 1 ||
							len(f.gateway.deletedTokens) != 0 || len(f.gateway.deletedAliases) != 0 {
							t.Fatal("protection retry reset the migration or retained accounting identity")
						}
						wantBlocked := access != "unchanged"
						for _, key := range f.gateway.keys {
							if key.Blocked != wantBlocked || ptr.Deref(key.Spend, 0) != 3.25 {
								t.Fatal("current authorization did not independently control the retained key")
							}
						}
						if wantBlocked && (!current.Status.AIGateway.Blocked || current.Status.AIGateway.Routed ||
							current.Status.AIGateway.Reason != aiGatewayReasonPolicyReprojection) {
							t.Fatal("the independent key block was not recorded")
						}
					}
					if access == "models withdrawn" || access == "budget tightened" {
						policy.Spec = *originalPolicy
						savePolicy()
					}
					protectionDown, dependencyDown = false, false
					if _, err := f.reconcile(); (err != nil) != (access != "unchanged") {
						t.Fatal("protection recovery did not preserve the source authorization hold:", err)
					}
					migrationPhase(t, f, api.MigrationStopping)
					finishMigration(t, f)
					if unprotectedStop || f.current(t).Status.AIGateway.KeyID != keyID || len(f.gateway.generated) != 1 {
						t.Fatal("recovery changed the retained key or stopped before source protection")
					}
					for _, key := range f.gateway.keys {
						if key.Blocked || ptr.Deref(key.Spend, 0) != 3.25 {
							t.Fatal("completed authorized recovery did not retain and unblock the same key")
						}
					}
				})
			}
		}
	}
}

func TestPlannedMigrationProtectionAccessFailureRetries(t *testing.T) {
	for _, failure := range []string{"Gateway read", "block receipt", "administrator block"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			f := newMigrationFixture(t, "openclaw", false)
			requestMigration(t, f, "migration-00000001", "node-b")
			f.reconcileOK(t)
			state := f.current(t).Status.PlannedMigration.DeepCopy()
			policy := &api.AgentPolicy{}
			if err := f.kube.Get(ctx, client.ObjectKey{Name: f.current(t).Status.Policy.EffectivePolicyRef.Name}, policy); err != nil {
				t.Fatal(err)
			}
			policy.Spec.ModelAccess.SharedModels.Allowed = nil
			if err := f.kube.Update(ctx, policy); err != nil {
				t.Fatal(err)
			}
			protectionErr := errors.New("synthetic protection write failure")
			accessErr := errors.New("synthetic key access failure")
			down := true
			f.r.Client = interceptor.NewClient(f.kube, interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if _, pod := object.(*corev1.Pod); pod {
						return protectionErr
					}
					return c.Patch(ctx, object, patch, opts...)
				},
				SubResourcePatch: func(ctx context.Context, c client.Client, sub string, object client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
					agent, ok := object.(*api.Agent)
					if down && failure == "block receipt" && sub == "status" && ok &&
						agent.Status.AIGateway != nil && agent.Status.AIGateway.Blocked {
						return accessErr
					}
					return c.SubResource(sub).Patch(ctx, object, patch, opts...)
				},
			})
			if failure == "Gateway read" {
				f.gateway.getErr = accessErr
			}
			if failure == "administrator block" {
				for _, key := range f.gateway.keys {
					key.Blocked = true
				}
			}
			_, err := f.reconcile()
			if !errors.Is(err, protectionErr) || (failure != "administrator block" && !errors.Is(err, accessErr)) {
				t.Fatal("independent access/protection failures were not joined:", err)
			}
			if failure == "administrator block" && !strings.Contains(err.Error(), "already blocked outside") {
				t.Fatal("an administrator block was silently adopted:", err)
			}
			down, f.gateway.getErr = false, nil
			_, err = f.reconcile()
			if !errors.Is(err, protectionErr) || errors.Is(err, accessErr) {
				t.Fatal("key access did not retry independently of failed protection:", err)
			}
			current := f.current(t)
			if !reflect.DeepEqual(current.Status.PlannedMigration, state) || ptr.Deref(f.workload(t).Spec.Replicas, -1) != 1 ||
				len(f.gateway.keys) != 1 || len(f.gateway.generated) != 1 || len(f.gateway.deletedTokens) != 0 {
				t.Fatal("key access retry changed the source or accounting identity")
			}
			for _, key := range f.gateway.keys {
				if !key.Blocked || ptr.Deref(key.Spend, 0) != 3.25 {
					t.Fatal("key access recovery left withdrawn access usable")
				}
			}
			if failure == "administrator block" && current.Status.AIGateway.Reason == aiGatewayReasonPolicyReprojection {
				t.Fatal("an external block became controller-owned during protection retries")
			}
		})
	}
}

func TestPlannedMigrationStalePreparationDoesNotHoldNewRequest(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f := newMigrationFixture(t, kind, true)
			completeMigration(t, f, "migration-00000001", "node-b")
			stale := f.current(t)
			requestMigration(t, f, "migration-00000002", "node-a")
			current := f.current(t)
			workload := f.workload(t)
			before := workload.DeepCopy()
			keyID := current.Status.AIGateway.KeyID
			cause := f.r.checkMigrationApply(ctx, stale, &workload.Spec.Template.Spec)
			if cause == nil {
				t.Fatal("stale migration intent did not stop preparation")
			}
			if _, err := f.r.reconciledSandboxFailure(ctx, stale, cause); err == nil {
				t.Fatal("stale preparation error was hidden")
			}
			after := f.workload(t)
			if !reflect.DeepEqual(before.Spec, after.Spec) || after.ResourceVersion != before.ResourceVersion ||
				f.current(t).Generation != current.Generation || f.current(t).Status.AIGateway.KeyID != keyID {
				t.Fatal("stale preparation changed the newer request's source workload or identity")
			}
			for _, key := range f.gateway.keys {
				if key.Blocked || ptr.Deref(key.Spend, 0) != 3.25 {
					t.Fatal("stale preparation changed retained Gateway authority")
				}
			}
			f.reconcileOK(t)
			if migration := f.current(t).Status.PlannedMigration; migration.RequestID != "migration-00000002" ||
				migration.Phase != api.MigrationProtecting {
				t.Fatal("new migration could not capture its still-ready source")
			}
		})
	}
}

func TestPlannedMigrationLateIntentChangeDoesNotHoldNewRequest(t *testing.T) {
	for _, scenario := range []struct{ kind, backend string }{
		{"openclaw", "kata"}, {"hermes", "kata"}, {"custom", "kata"}, {"custom", "container"},
	} {
		t.Run(scenario.kind+"/"+scenario.backend, func(t *testing.T) {
			var setup []func(*AgentReconciler, client.Client)
			if scenario.kind == "custom" {
				setup = append(setup, withCommonCustom(t, "chat"))
			}
			if scenario.backend == "container" {
				setup = append(setup, withContainerRuntime(t))
			}
			f := newMigrationFixture(t, scenario.kind, true, setup...)
			completeMigration(t, f, "migration-00000001", "node-b")
			before := f.workload(t).DeepCopy()
			keyID := f.current(t).Status.AIGateway.KeyID
			changed := false
			f.r.APIReader = interceptor.NewClient(f.kube, interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if err := c.List(ctx, list, opts...); err != nil {
						return err
					}
					if _, attachments := list.(*storagev1.VolumeAttachmentList); attachments && !changed {
						changed = true
						requestMigration(t, f, "migration-00000002", "node-a")
					}
					return nil
				},
			})
			_, err := f.reconcile()
			if !changed || err == nil || !strings.Contains(err.Error(), "Sandbox intent changed before resource reconciliation") {
				t.Fatal("fixture did not change intent after the migration guard and before the Sandbox guard:", err)
			}
			after := f.workload(t)
			if !reflect.DeepEqual(before.Spec, after.Spec) || after.ResourceVersion != before.ResourceVersion ||
				f.current(t).Status.AIGateway.KeyID != keyID {
				t.Fatal("late stale preparation changed the newer request's source workload or identity")
			}
			for _, key := range f.gateway.keys {
				if key.Blocked || ptr.Deref(key.Spend, 0) != 3.25 {
					t.Fatal("late stale preparation changed retained Gateway authority")
				}
			}
			f.reconcileOK(t)
			if migration := f.current(t).Status.PlannedMigration; migration.RequestID != "migration-00000002" ||
				migration.Phase != api.MigrationProtecting {
				t.Fatal("new migration could not capture its still-ready source")
			}
		})
	}
}

func TestPlannedMigrationLateAgentSourceChangeDoesNotHoldNewRequest(t *testing.T) {
	for _, backend := range []string{"kata", "container"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			setup := []func(*AgentReconciler, client.Client){withCommonCustom(t, "chat")}
			if backend == "container" {
				setup = append(setup, withContainerRuntime(t))
			}
			f := newMigrationFixture(t, "custom", true, setup...)
			completeMigration(t, f, "migration-00000001", "node-b")
			stale := f.current(t)
			source, err := w.AgentSourceReference(stale)
			if err != nil {
				t.Fatal(err)
			}
			before := f.workload(t).DeepCopy()
			requestMigration(t, f, "migration-00000002", "node-a")
			_, cause := f.r.readSandboxSource(ctx, source)
			if !errors.Is(cause, errSandboxSourceChanged) {
				t.Fatal("late Agent source guard did not reject the old generation:", cause)
			}
			if _, err := f.r.reconciledSandboxFailure(ctx, stale, cause); err == nil {
				t.Fatal("late Agent source failure was hidden")
			}
			after := f.workload(t)
			if !reflect.DeepEqual(before.Spec, after.Spec) || after.ResourceVersion != before.ResourceVersion ||
				f.current(t).Status.AIGateway.KeyID != stale.Status.AIGateway.KeyID {
				t.Fatal("late Agent source failure changed the newer request's source")
			}
			for _, key := range f.gateway.keys {
				if key.Blocked || ptr.Deref(key.Spend, 0) != 3.25 {
					t.Fatal("late Agent source failure changed retained Gateway authority")
				}
			}
			f.reconcileOK(t)
			if migration := f.current(t).Status.PlannedMigration; migration.RequestID != "migration-00000002" ||
				migration.Phase != api.MigrationProtecting {
				t.Fatal("new migration could not capture its still-ready source")
			}
		})
	}
}

func TestPlannedMigrationLateBindingChangeStillHoldsSource(t *testing.T) {
	for _, backend := range []string{"kata", "container"} {
		t.Run(backend, func(t *testing.T) {
			setup := []func(*AgentReconciler, client.Client){withCommonCustom(t, "chat")}
			if backend == "container" {
				setup = append(setup, withContainerRuntime(t))
			}
			f := newMigrationFixture(t, "custom", true, setup...)
			completeMigration(t, f, "migration-00000001", "node-b")
			keyID := f.current(t).Status.AIGateway.KeyID
			changed := false
			f.r.APIReader = interceptor.NewClient(f.kube, interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if err := c.List(ctx, list, opts...); err != nil {
						return err
					}
					if _, attachments := list.(*storagev1.VolumeAttachmentList); attachments && !changed {
						changed = true
						policy := &api.AgentPolicy{}
						if err := f.kube.Get(ctx, client.ObjectKey{Name: "policy-example"}, policy); err != nil {
							return err
						}
						if err := f.kube.Delete(ctx, policy); err != nil {
							return err
						}
						policy.UID, policy.ResourceVersion = "replacement-policy-uid", ""
						return f.kube.Create(ctx, policy)
					}
					return nil
				},
			})
			_, err := f.reconcile()
			if !changed || err == nil || errors.Is(err, errSandboxIntentChanged) ||
				!errors.Is(err, errSandboxSourceChanged) {
				t.Fatal("a changed current authorization binding was not guarded as a current source failure:", err)
			}
			if ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 || f.current(t).Status.AIGateway.KeyID != keyID {
				t.Fatal("the current binding change did not hold the source with its retained key")
			}
			for _, key := range f.gateway.keys {
				if !key.Blocked || ptr.Deref(key.Spend, 0) != 3.25 {
					t.Fatal("the current binding change did not block access while retaining spend")
				}
			}
		})
	}
}

func TestPlannedMigrationPreCaptureHoldRecoversSource(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, dependency := range []string{"Team", "AgentPolicy", "catalog"} {
			for _, previous := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/previous=%t", kind, dependency, previous), func(t *testing.T) {
					ctx := context.Background()
					f := newMigrationFixture(t, kind, true)
					source, target, id := "node-a", "node-b", "migration-00000001"
					if previous {
						completeMigration(t, f, id, target)
						source, target, id = target, source, "migration-00000002"
					}
					initial := reconciledExpectationForTest(t, f)
					recorded := f.current(t).Status.PlannedMigration.DeepCopy()
					keyID, stages := f.current(t).Status.AIGateway.KeyID, f.installer.stages
					requestMigration(t, f, id, target)
					request := *f.current(t).Spec.PlannedMigration
					down := true
					injected := errors.New("synthetic pre-capture authorization outage")
					f.r.Client = interceptor.NewClient(f.kube, interceptor.Funcs{
						Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
							_, team := object.(*api.Team)
							_, policy := object.(*api.AgentPolicy)
							if down && ((dependency == "Team" && team) || (dependency == "AgentPolicy" && policy)) {
								return injected
							}
							return c.Get(ctx, key, object, opts...)
						},
					})
					f.r.APIReader = f.r.Client
					f.r.aiGatewayClientFactory = func(context.Context) (aiGatewayKeyClient, error) {
						if down && dependency == "catalog" {
							return &failingListGateway{fakeAIGateway: f.gateway}, nil
						}
						return f.gateway, nil
					}
					if _, err := f.reconcile(); err == nil {
						t.Fatal("the pre-capture authorization failure was hidden")
					}
					if !reflect.DeepEqual(recorded, f.current(t).Status.PlannedMigration) ||
						ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 ||
						isConditionTrue(f.current(t).Status.Conditions, api.ConditionSandboxReady) {
						t.Fatal("the pre-capture outage did not hold the uncaptured source")
					}
					workload := f.workload(t)
					pod := &corev1.Pod{}
					if err := f.kube.Get(ctx, client.ObjectKey{Namespace: workload.Namespace, Name: workload.Name + "-0"}, pod); err != nil {
						t.Fatal(err)
					}
					if err := f.kube.Delete(ctx, pod); err != nil {
						t.Fatal(err)
					}
					attachments, err := f.r.migrationAttachments(ctx, "synthetic-home-volume")
					if err != nil || len(attachments) != 1 {
						t.Fatal("read ordinary source attachment:", err)
					}
					attachment := &attachments[0]
					if err := f.kube.Delete(ctx, attachment); err != nil {
						t.Fatal(err)
					}
					if err := f.kube.Get(ctx, client.ObjectKeyFromObject(attachment), attachment); err != nil {
						t.Fatal(err)
					}
					attachment.Finalizers = nil
					if err := f.kube.Update(ctx, attachment); err != nil {
						t.Fatal(err)
					}
					migrationWorkloadStatus(t, f, 0)
					for range 2 {
						result, err := f.reconcile()
						current := f.current(t)
						if err != nil || result.RequeueAfter == 0 || current.Status.Policy == nil ||
							current.Status.Policy.Phase != "Pending" {
							t.Fatalf("source dependency outage did not remain a reported retrying hold: result=%+v err=%v", result, err)
						}
						if f.installer.stages != stages || ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 {
							t.Fatal("an unavailable dependency allowed pre-capture execution")
						}
						for _, key := range f.gateway.keys {
							if !key.Blocked || ptr.Deref(key.Spend, 0) != 3.25 {
								t.Fatal("pre-capture outage did not retain the blocked accounting identity")
							}
						}
					}
					down = false
					for range 6 {
						f.reconcileOK(t)
						if ptr.Deref(f.workload(t).Spec.Replicas, -1) == 1 {
							break
						}
					}
					workload = f.workload(t)
					wantAnnotation := ""
					if recorded != nil {
						wantAnnotation = recorded.RequestID
					}
					if f.installer.stages != stages+1 || ptr.Deref(workload.Spec.Replicas, -1) != 1 ||
						workload.Spec.Template.Annotations[api.AnnotationMigrationRequest] != wantAnnotation ||
						!reflect.DeepEqual(recorded, f.current(t).Status.PlannedMigration) {
						t.Fatal("recovery did not restore only the authorized source projection")
					}
					migrationReadyPod(t, f, "recovered-source-pod", source)
					migrationAttachment(t, f, "recovered-source-attachment", source)
					migrationWorkloadStatus(t, f, 1)
					f.reconcileOK(t)
					if f.current(t).Status.Phase != api.PhaseReady {
						t.Fatal("source recovery never restored readiness")
					}
					f.reconcileOK(t)
					migrationPhase(t, f, api.MigrationProtecting)
					state := f.current(t).Status.PlannedMigration
					if state.SourcePod.UID != "recovered-source-pod" || state.Source.Name != source ||
						state.Attachment.Name != "recovered-source-attachment" || state.PlannedMigrationRequest != request {
						t.Fatal("capture did not use the recovered source and original request")
					}
					finishMigration(t, f)
					current := f.current(t)
					expected := reconciledExpectationForTest(t, f)
					if current.Status.AIGateway.KeyID != keyID || len(f.gateway.generated) != 1 ||
						expected.Home != initial.Home || expected.Namespace != initial.Namespace ||
						slices.Contains(current.Finalizers, sandbox.HoldFinalizer) {
						t.Fatal("pre-capture recovery changed a retained identity or left execution held")
					}
					for _, key := range f.gateway.keys {
						if key.Blocked || ptr.Deref(key.Spend, 0) != 3.25 {
							t.Fatal("pre-capture recovery reset spending or left the key blocked")
						}
					}
				})
			}
		}
	}
}

func TestPlannedMigrationPreCaptureRecoveryStillHonorsHolds(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, hold := range []string{"policy denied", "suspended", "migration disabled"} {
			t.Run(kind+"/"+hold, func(t *testing.T) {
				ctx := context.Background()
				f := newMigrationFixture(t, kind, false)
				requestMigration(t, f, "migration-00000001", "node-b")
				injected := errors.New("synthetic pre-capture Team read failure")
				f.r.Client = interceptor.NewClient(f.kube, interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
						if _, team := object.(*api.Team); team {
							return injected
						}
						return c.Get(ctx, key, object, opts...)
					},
				})
				if _, err := f.reconcile(); !errors.Is(err, injected) {
					t.Fatal("the pre-capture dependency failure was not surfaced:", err)
				}
				f.r.Client = f.kube
				switch hold {
				case "policy denied":
					policy := &api.AgentPolicy{}
					if err := f.kube.Get(ctx, client.ObjectKey{Name: f.current(t).Status.Policy.EffectivePolicyRef.Name}, policy); err != nil {
						t.Fatal(err)
					}
					setCondition(&policy.Status.Conditions, api.ConditionAgentPolicyAccepted, metav1.ConditionFalse,
						"Denied", "synthetic current policy denial")
					if err := f.kube.Status().Update(ctx, policy); err != nil {
						t.Fatal(err)
					}
				case "suspended":
					agent := f.current(t)
					agent.Spec.Suspended = true
					agent.Generation++
					if err := f.kube.Update(ctx, agent); err != nil {
						t.Fatal(err)
					}
				case "migration disabled":
					f.r.SandboxPreparation.PlannedMigration = false
				}
				stages := f.installer.stages
				for range 2 {
					result, err := f.reconcile()
					current := f.current(t)
					if result.RequeueAfter == 0 && err == nil {
						t.Fatal("the continuing hold was not surfaced")
					}
					if hold == "policy denied" && (err != nil || current.Status.Policy.Phase != "Denied") {
						t.Fatal("recovery did not recheck the denied current policy:", err)
					}
					if current.Status.PlannedMigration != nil || f.installer.stages != stages ||
						ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 ||
						isConditionTrue(current.Status.Conditions, api.ConditionReady) {
						t.Fatal("pre-capture recovery bypassed an independent execution hold")
					}
				}
			})
		}
	}
}

func TestPlannedMigrationProtectingRecoversAuthorizationOutage(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, dependency := range []string{"Team", "AgentPolicy", "catalog"} {
			for _, protected := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/protected=%t", kind, dependency, protected), func(t *testing.T) {
					ctx := context.Background()
					f := newMigrationFixture(t, kind, true)
					initial := reconciledExpectationForTest(t, f)
					requestMigration(t, f, "migration-00000001", "node-b")
					f.reconcileOK(t)
					state := f.current(t).Status.PlannedMigration.DeepCopy()
					keyID, stages := f.current(t).Status.AIGateway.KeyID, f.installer.stages
					podKey := client.ObjectKey{Namespace: state.Namespace.Name, Name: state.SourcePod.Name}
					if protected {
						for _, item := range []struct {
							object    client.Object
							key       client.ObjectKey
							finalizer string
						}{
							{&corev1.Pod{}, podKey, api.MigrationPodFinalizer},
							{&storagev1.VolumeAttachment{}, client.ObjectKey{Name: state.Attachment.Name}, api.MigrationAttachmentFinalizer},
						} {
							if err := f.kube.Get(ctx, item.key, item.object); err != nil {
								t.Fatal(err)
							}
							if err := f.r.migrationFinalizer(ctx, f.current(t), item.object, item.finalizer, true); err != nil {
								t.Fatal(err)
							}
						}
					}
					unavailable := errors.New("synthetic migration authorization dependency outage")
					down := true
					f.r.aiGatewayClientFactory = func(context.Context) (aiGatewayKeyClient, error) {
						if down && dependency == "catalog" {
							return &failingListGateway{fakeAIGateway: f.gateway}, nil
						}
						return f.gateway, nil
					}
					unprotectedStop := false
					f.r.Client = interceptor.NewClient(f.kube, interceptor.Funcs{
						Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
							_, team := object.(*api.Team)
							_, policy := object.(*api.AgentPolicy)
							if down && ((dependency == "Team" && team) || (dependency == "AgentPolicy" && policy)) {
								return unavailable
							}
							return c.Get(ctx, key, object, opts...)
						},
						Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
							if err := c.Patch(ctx, object, patch, opts...); err != nil {
								return err
							}
							// Model the StatefulSet controller deleting the source immediately on scale-down.
							unsafe, err := deleteMigrationSourceOnScaleDown(ctx, c, object, state)
							unprotectedStop = unprotectedStop || unsafe
							return err
						},
					})
					f.r.APIReader = f.r.Client
					if _, err := f.reconcile(); err == nil {
						t.Fatal("authorization outage was not surfaced")
					}
					pod := &corev1.Pod{}
					if unprotectedStop {
						t.Error("authorization failure stopped the source before protecting its observations")
					}
					if err := f.kube.Get(ctx, podKey, pod); err != nil {
						t.Fatal("authorization outage lost the captured source Pod:", err)
					}
					if !slices.Contains(pod.Finalizers, api.MigrationPodFinalizer) ||
						ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 {
						t.Fatal("outage did not retain source protection while stopping execution")
					}
					if isConditionTrue(f.current(t).Status.Conditions, api.ConditionSandboxReady) {
						t.Fatal("authorization outage retained a stale SandboxReady observation")
					}

					down = false
					f.reconcileOK(t)
					migrationPhase(t, f, api.MigrationStopping)
					acknowledgeMigrationSource(t, f)
					f.reconcileOK(t)
					f.reconcileOK(t)
					acknowledgeMigrationDetach(t, f)
					f.reconcileOK(t)
					f.reconcileOK(t)
					f.reconcileOK(t)
					migrationPhase(t, f, api.MigrationStarting)

					down = true
					for range 2 {
						result, err := f.reconcile()
						current := f.current(t)
						if err != nil || result.RequeueAfter == 0 || current.Status.Policy == nil ||
							current.Status.Policy.Phase != "Pending" ||
							isConditionTrue(current.Status.Conditions, api.ConditionPolicyResolved) {
							t.Fatalf("destination authorization outage was not reported as a retrying hold: result=%+v err=%v", result, err)
						}
						if f.installer.stages != stages || ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 {
							t.Fatal("migration restarted without fresh destination authorization")
						}
						for _, key := range f.gateway.keys {
							if !key.Blocked || ptr.Deref(key.Spend, 0) != 3.25 {
								t.Fatal("authorization outage lost the blocked accounting identity")
							}
						}
					}
					down = false
					for range 4 {
						f.reconcileOK(t)
					}
					if f.installer.stages != stages+1 {
						t.Fatal("recovered dependencies did not allow exactly one replacement apply")
					}
					migrationReadyPod(t, f, "replacement-pod", "node-b")
					migrationAttachment(t, f, "replacement-attachment", "node-b")
					migrationWorkloadStatus(t, f, 1)
					f.reconcileOK(t)
					migrationPhase(t, f, api.MigrationCompleted)
					current := f.current(t)
					observed := current.Status.PlannedMigration
					expected := reconciledExpectationForTest(t, f)
					if observed.PlannedMigrationRequest != state.PlannedMigrationRequest ||
						observed.SourcePod != state.SourcePod || observed.Attachment != state.Attachment ||
						!observed.StartedAt.Equal(&state.StartedAt) || expected.Home != initial.Home ||
						expected.Namespace != initial.Namespace || current.Status.AIGateway.KeyID != keyID ||
						current.Status.Phase != api.PhaseReady || len(f.gateway.generated) != 1 {
						t.Fatal("authorization recovery reset the captured migration or retained identity")
					}
				})
			}
		}
	}
}

func TestPlannedMigrationProtectionCheckpointPrecedesAuthorizationHold(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("committed=%t", committed), func(t *testing.T) {
			ctx := context.Background()
			f := newMigrationFixture(t, "openclaw", true)
			requestMigration(t, f, "migration-00000001", "node-b")
			f.reconcileOK(t)
			state := f.current(t).Status.PlannedMigration.DeepCopy()
			lost := errors.New("synthetic stop checkpoint acknowledgement failure")
			unavailable := errors.New("synthetic Team lookup failure")
			failCheckpoint := true
			f.r.Client = interceptor.NewClient(f.kube, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
					if _, team := object.(*api.Team); team {
						return unavailable
					}
					return c.Get(ctx, key, object, opts...)
				},
				SubResourcePatch: func(ctx context.Context, c client.Client, sub string, object client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
					agent, ok := object.(*api.Agent)
					if failCheckpoint && sub == "status" && ok && agent.Status.PlannedMigration != nil &&
						agent.Status.PlannedMigration.Phase == api.MigrationStopping {
						if committed {
							if err := c.SubResource(sub).Patch(ctx, object, patch, opts...); err != nil {
								return err
							}
						}
						return lost
					}
					return c.SubResource(sub).Patch(ctx, object, patch, opts...)
				},
			})
			if _, err := f.reconcile(); !errors.Is(err, lost) || !errors.Is(err, unavailable) {
				t.Fatal("checkpoint failure skipped independent source authorization:", err)
			}
			wantPhase := api.MigrationProtecting
			if committed {
				wantPhase = api.MigrationStopping
			}
			migrationPhase(t, f, wantPhase)
			if ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 {
				t.Fatal("a failed checkpoint left the protected unauthorized source running")
			}
			failCheckpoint = false
			_, err := f.reconcile()
			if (err != nil) != !committed {
				t.Fatal("checkpoint recovery did not resume its recorded transition:", err)
			}
			migrationPhase(t, f, api.MigrationStopping)
			pod := &corev1.Pod{}
			if err := f.kube.Get(ctx, client.ObjectKey{Namespace: state.Namespace.Name, Name: state.SourcePod.Name}, pod); err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(pod.Finalizers, api.MigrationPodFinalizer) ||
				ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 {
				t.Fatal("recovery did not stop the protected source")
			}
		})
	}
}

func TestPlannedMigrationRecoveredDependencyStillChecksPolicy(t *testing.T) {
	f := newMigrationFixture(t, "hermes", true)
	ctx := context.Background()
	requestMigration(t, f, "migration-00000001", "node-b")
	f.reconcileOK(t)
	original := f.r.Client
	f.r.Client = interceptor.NewClient(f.kube, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
			if _, policy := object.(*api.AgentPolicy); policy {
				return errors.New("synthetic AgentPolicy lookup failure")
			}
			return c.Get(ctx, key, object, opts...)
		},
	})
	if _, err := f.reconcile(); err == nil {
		t.Fatal("source authorization failure was hidden")
	}
	f.r.Client = original
	acknowledgeMigrationSource(t, f)
	f.reconcileOK(t)
	f.reconcileOK(t)
	acknowledgeMigrationDetach(t, f)
	f.reconcileOK(t)
	f.reconcileOK(t)
	f.reconcileOK(t)
	migrationPhase(t, f, api.MigrationStarting)

	policy := &api.AgentPolicy{}
	if err := f.kube.Get(ctx, client.ObjectKey{Name: f.current(t).Status.Policy.EffectivePolicyRef.Name}, policy); err != nil {
		t.Fatal(err)
	}
	setCondition(&policy.Status.Conditions, api.ConditionAgentPolicyAccepted, metav1.ConditionFalse,
		"Denied", "synthetic current policy denial")
	if err := f.kube.Status().Update(ctx, policy); err != nil {
		t.Fatal(err)
	}
	stages := f.installer.stages
	for range 2 {
		f.reconcileOK(t)
		current := f.current(t)
		if current.Status.Policy == nil || current.Status.Policy.Phase != "Denied" ||
			isConditionTrue(current.Status.Conditions, api.ConditionReady) ||
			f.installer.stages != stages || ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 {
			t.Fatal("a recovered dependency permitted a destination denied by current policy")
		}
		migrationPhase(t, f, api.MigrationStarting)
	}
}

func TestPlannedMigrationSuspensionAndDisabledGateRetainProgress(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, captured := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/captured=%t", kind, captured), func(t *testing.T) {
				f := newMigrationFixture(t, kind, true)
				initial := reconciledExpectationForTest(t, f)
				requestMigration(t, f, "migration-00000001", "node-b")
				if captured {
					f.reconcileOK(t)
				}
				agent := f.current(t)
				request := *agent.Spec.PlannedMigration
				keyID := agent.Status.AIGateway.KeyID
				agent.Spec.Suspended = true
				agent.Generation++
				if err := f.kube.Update(context.Background(), agent); err != nil {
					t.Fatal(err)
				}
				if !captured {
					f.reconcileOK(t)
					migrationPhase(t, f, api.MigrationProtecting)
				}
				f.r.SandboxPreparation.PlannedMigration = false
				f.reconcileOK(t)
				migrationPhase(t, f, api.MigrationStopping)
				if ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 {
					t.Fatal("suspension waited for migration completion")
				}
				if f.current(t).Status.Phase == api.PhaseSuspended {
					t.Fatal("a stop request acknowledged suspension before source termination")
				}
				acknowledgeMigrationSource(t, f)
				f.reconcileOK(t)
				f.reconcileOK(t)
				acknowledgeMigrationDetach(t, f)
				f.reconcileOK(t)
				f.reconcileOK(t)
				f.reconcileOK(t)
				migrationPhase(t, f, api.MigrationStarting)
				stages := f.installer.stages
				for range 2 {
					f.reconcileOK(t)
				}
				if f.installer.stages != stages || ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 {
					t.Fatal("a suspended Agent started the migration destination")
				}
				agent = f.current(t)
				agent.Spec.Suspended = false
				agent.Generation++
				if err := f.kube.Update(context.Background(), agent); err != nil {
					t.Fatal(err)
				}
				for range 3 {
					f.reconcileOK(t)
				}
				if f.installer.stages != stages+1 {
					t.Fatal("disabling new requests stranded an accepted migration")
				}
				migrationReadyPod(t, f, "replacement-pod", "node-b")
				migrationAttachment(t, f, "replacement-attachment", "node-b")
				migrationWorkloadStatus(t, f, 1)
				f.reconcileOK(t)
				migrationPhase(t, f, api.MigrationCompleted)
				current := f.current(t)
				expected := reconciledExpectationForTest(t, f)
				if current.Status.Phase != api.PhaseReady || *current.Spec.PlannedMigration != request ||
					current.UID != initial.Agent.UID || expected.Home != initial.Home || expected.Namespace != initial.Namespace ||
					current.Status.AIGateway.KeyID != keyID || len(f.gateway.generated) != 1 {
					t.Fatal("resume did not complete the original migration with retained identities")
				}
			})
		}
	}
}

func TestPlannedMigrationRejectedPreflightStillSuspends(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, fault := range []string{"disabled", "wrong UID"} {
			t.Run(kind+"/"+fault, func(t *testing.T) {
				f := newMigrationFixture(t, kind, true)
				requestMigration(t, f, "migration-00000001", "node-b")
				agent := f.current(t)
				agent.Spec.Suspended = true
				agent.Generation++
				if fault == "disabled" {
					f.r.SandboxPreparation.PlannedMigration = false
				} else {
					agent.Spec.PlannedMigration.Target.UID = "different-node"
				}
				if err := f.kube.Update(context.Background(), agent); err != nil {
					t.Fatal(err)
				}
				request := *agent.Spec.PlannedMigration
				keyID := agent.Status.AIGateway.KeyID
				stages := f.installer.stages
				for range 2 {
					if _, err := f.reconcile(); err == nil {
						t.Fatal("migration preflight rejection was hidden")
					}
					current := f.current(t)
					if ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 ||
						current.Status.Phase != api.PhasePending || current.Status.AccessURL != "" ||
						isConditionTrue(current.Status.Conditions, api.ConditionReady) {
						t.Fatal("migration preflight prevented suspension or reported shutdown complete")
					}
					if current.Status.PlannedMigration != nil || *current.Spec.PlannedMigration != request ||
						f.installer.stages != stages || current.Status.AIGateway.KeyID != keyID || len(f.gateway.generated) != 1 {
						t.Fatal("rejected migration changed the retained request or execution identity")
					}
					for _, key := range f.gateway.keys {
						if !key.Blocked || ptr.Deref(key.Spend, 0) != 3.25 {
							t.Fatal("suspension did not block and retain the existing Gateway key")
						}
					}
				}
			})
		}
	}
}

func TestPlannedMigrationRecoversDetachStatusWriteLoss(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, cleanup := range []bool{false, true} {
			for _, staleError := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/cleanup=%t/staleError=%t", kind, cleanup, staleError), func(t *testing.T) {
					f := newMigrationFixture(t, kind, true)
					initial := reconciledExpectationForTest(t, f)
					ctx := context.Background()
					requestMigration(t, f, "migration-00000001", "node-b")
					f.reconcileOK(t)
					f.reconcileOK(t)
					f.reconcileOK(t)
					acknowledgeMigrationSource(t, f)
					f.reconcileOK(t)
					f.reconcileOK(t)
					migrationPhase(t, f, api.MigrationSourceTerminated)
					state := f.current(t).Status.PlannedMigration
					stages := f.installer.stages
					attachment := &storagev1.VolumeAttachment{}
					key := client.ObjectKey{Name: state.Attachment.Name}
					if err := f.kube.Get(ctx, key, attachment); err != nil {
						t.Fatal(err)
					}
					if staleError {
						attachment.Status.DetachError = &storagev1.VolumeError{Message: "synthetic earlier detach error"}
						if err := f.kube.Status().Update(ctx, attachment); err != nil {
							t.Fatal(err)
						}
					}
					acknowledgeMigrationDetachFinalizer(t, f)
					if cleanup {
						if err := f.kube.Delete(ctx, f.current(t)); err != nil {
							t.Fatal(err)
						}
						f.reconcileOK(t)
						if done, err := f.r.cleanupMigration(ctx, f.current(t)); err != nil || !done {
							t.Fatalf("completed CSI detach stranded cleanup: done=%v err=%v", done, err)
						}
					} else {
						f.reconcileOK(t)
						migrationPhase(t, f, api.MigrationDetached)
						if err := f.kube.Get(ctx, key, attachment); err != nil {
							t.Fatal("detach checkpoint must precede protection release:", err)
						}
						if !attachment.Status.Attached || (attachment.Status.DetachError != nil) != staleError ||
							!slices.Contains(attachment.Finalizers, api.MigrationAttachmentFinalizer) {
							t.Fatal("migration rewrote CSI status or released protection before its checkpoint")
						}
						f.reconcileOK(t)
					}
					if err := f.kube.Get(ctx, key, attachment); !apierrors.IsNotFound(err) {
						t.Fatal("completed CSI detach retained migration protection:", err)
					}
					if f.installer.stages != stages || ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 {
						t.Fatal("detach acknowledgement prematurely started a replacement")
					}
					if cleanup {
						return
					}
					for range 4 {
						f.reconcileOK(t)
					}
					migrationReadyPod(t, f, "replacement-pod", "node-b")
					migrationAttachment(t, f, "replacement-attachment", "node-b")
					migrationWorkloadStatus(t, f, 1)
					f.reconcileOK(t)
					migrationPhase(t, f, api.MigrationCompleted)
					expected := reconciledExpectationForTest(t, f)
					if f.installer.stages != stages+1 || expected.Home != initial.Home || expected.Agent != initial.Agent ||
						expected.Namespace != initial.Namespace || len(f.gateway.generated) != 1 {
						t.Fatal("detach recovery changed retained identities or repeated the replacement")
					}
				})
			}
		}
	}
}

func TestPlannedMigrationDetachStillRequiresCapturedEvidence(t *testing.T) {
	for _, cleanup := range []bool{false, true} {
		for _, fault := range []string{"not deleting", "attacher pending", "detach failed", "UID", "node", "driver", "volume"} {
			t.Run(fmt.Sprintf("cleanup=%t/%s", cleanup, fault), func(t *testing.T) {
				f := newMigrationFixture(t, "hermes", true)
				ctx := context.Background()
				requestMigration(t, f, "migration-00000001", "node-b")
				f.reconcileOK(t)
				f.reconcileOK(t)
				f.reconcileOK(t)
				acknowledgeMigrationSource(t, f)
				f.reconcileOK(t)
				f.reconcileOK(t)
				state := f.current(t).Status.PlannedMigration
				stages := f.installer.stages
				if cleanup {
					if err := f.kube.Delete(ctx, f.current(t)); err != nil {
						t.Fatal(err)
					}
				}
				f.r.APIReader = interceptor.NewClient(f.kube, interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
						if err := c.Get(ctx, key, object, opts...); err != nil {
							return err
						}
						attachment, ok := object.(*storagev1.VolumeAttachment)
						if !ok || attachment.Name != state.Attachment.Name {
							return nil
						}
						attachment.DeletionTimestamp = ptr.To(metav1.Now())
						attachment.Status.Attached = false
						attachment.Finalizers = []string{api.MigrationAttachmentFinalizer}
						switch fault {
						case "not deleting":
							attachment.DeletionTimestamp = nil
						case "attacher pending", "detach failed":
							attachment.Finalizers = append(attachment.Finalizers, migrationAttacherFinalizer(state.Driver))
							if fault == "detach failed" {
								attachment.Status.DetachError = &storagev1.VolumeError{Message: "synthetic detach rejection"}
							}
						case "UID":
							attachment.UID = "replacement-attachment"
						case "node":
							attachment.Spec.NodeName = "node-b"
						case "driver":
							attachment.Spec.Attacher = "different.example.com"
						case "volume":
							attachment.Spec.Source.PersistentVolumeName = ptr.To("different-volume")
						}
						return nil
					},
				})
				_, err := f.reconcile()
				if (err != nil) != (fault != "not deleting" && fault != "attacher pending") {
					t.Fatal("detach evidence produced the wrong failure result:", err)
				}
				migrationPhase(t, f, api.MigrationSourceTerminated)
				attachment := &storagev1.VolumeAttachment{}
				if err := f.kube.Get(ctx, client.ObjectKey{Name: state.Attachment.Name}, attachment); err != nil {
					t.Fatal(err)
				}
				if !slices.Contains(attachment.Finalizers, api.MigrationAttachmentFinalizer) || f.installer.stages != stages ||
					ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 {
					t.Fatal("incomplete or foreign detach evidence released protection or started a replacement")
				}
			})
		}
	}
}

func TestPlannedMigrationDeletionReleasesItsProtectedObjects(t *testing.T) {
	f := newMigrationFixture(t, "openclaw", true)
	requestMigration(t, f, "migration-00000001", "node-b")
	f.reconcileOK(t)
	f.reconcileOK(t)
	f.reconcileOK(t)
	ctx := context.Background()
	agent := f.current(t)
	if err := f.kube.Delete(ctx, agent); err != nil {
		t.Fatal(err)
	}
	acknowledgeMigrationSource(t, f)
	f.reconcileOK(t)
	migrationPhase(t, f, api.MigrationSourceTerminated)
	f.reconcileOK(t)
	acknowledgeMigrationDetach(t, f)
	f.reconcileOK(t)
	if done, err := f.r.cleanupMigration(ctx, f.current(t)); err != nil || !done {
		t.Fatalf("migration protections survived ordinary deletion: done=%v err=%v", done, err)
	}
	attachments := &storagev1.VolumeAttachmentList{}
	if err := f.kube.List(ctx, attachments); err != nil {
		t.Fatal(err)
	}
	for _, attachment := range attachments.Items {
		if slices.Contains(attachment.Finalizers, api.MigrationAttachmentFinalizer) {
			t.Fatal("migration left an attachment finalizer")
		}
	}
}

func TestPlannedMigrationDeletionAfterDestinationSubmissionUsesNormalCleanup(t *testing.T) {
	f := newMigrationFixture(t, "openclaw", true)
	completeMigration(t, f, "migration-00000001", "node-b")
	ctx := context.Background()
	agent := f.current(t)
	agent.Status.PlannedMigration.Phase = api.MigrationStarting
	if err := f.kube.Status().Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Delete(ctx, agent); err != nil {
		t.Fatal(err)
	}
	agent = f.current(t)
	if done, err := f.r.cleanupMigration(ctx, agent); err != nil || !done {
		t.Fatalf("destination submission re-entered old source cleanup: done=%v err=%v", done, err)
	}
	_, err := f.reconcile()
	if err != nil && !strings.Contains(err.Error(), "common Sandbox source reservation waits for acknowledged controller shutdown") {
		t.Fatal("normal deletion failed outside the expected controller-shutdown wait:", err)
	}
	if ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 {
		t.Fatal("normal deletion did not stop the replacement workload")
	}
}

func TestMigrationLeaseFreshnessBounds(t *testing.T) {
	f := newMigrationFixture(t, "openclaw", false)
	now := time.Now().Truncate(time.Microsecond)
	f.r.SandboxPreparation.Now = func() time.Time { return now }
	for _, test := range []struct {
		duration int32
		age      time.Duration
		allowed  bool
	}{
		{40, 40 * time.Second, true},
		{40, 40*time.Second + time.Microsecond, false},
		{40, -5 * time.Second, true},
		{40, -5*time.Second - time.Microsecond, false},
		{120, 60 * time.Second, true},
		{120, 60*time.Second + time.Microsecond, false},
		{0, 0, false},
	} {
		lease := &coordinationv1.Lease{}
		if err := f.kube.Get(context.Background(), client.ObjectKey{Namespace: corev1.NamespaceNodeLease, Name: "node-a"}, lease); err != nil {
			t.Fatal(err)
		}
		lease.Spec.LeaseDurationSeconds = ptr.To(test.duration)
		lease.Spec.RenewTime = &metav1.MicroTime{Time: now.Add(-test.age)}
		if err := f.kube.Update(context.Background(), lease); err != nil {
			t.Fatal(err)
		}
		_, _, err := f.r.migrationNode(context.Background(), api.MigrationObject{Name: "node-a", UID: "node-a-uid"}, migrationTestDriver)
		if (err == nil) != test.allowed {
			t.Fatalf("duration=%d age=%s allowed=%v err=%v", test.duration, test.age, test.allowed, err)
		}
	}
}

func TestMigrationTerminationRequiresNamedContainerAcknowledgements(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: ptr.To(metav1.Now())},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}, {Name: "helper"}}},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{
			{Name: "agent", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{FinishedAt: metav1.Now()}}},
			{Name: "helper", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{FinishedAt: metav1.Now()}}},
		}}}
	if !migrationPodTerminated(pod) {
		t.Fatal("complete named termination was rejected")
	}
	pod.Status.ContainerStatuses[1].Name = "agent"
	if migrationPodTerminated(pod) {
		t.Fatal("duplicate status hid an unacknowledged container")
	}
}
