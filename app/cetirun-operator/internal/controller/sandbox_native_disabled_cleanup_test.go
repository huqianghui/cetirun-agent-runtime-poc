// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func disableNativeStarts(t *testing.T, f *nativeSandboxFixture, disableSources bool) {
	t.Helper()
	config, err := ParseSandboxSourcesConfig(nativeSandboxSourcesJSON)
	if err != nil {
		t.Fatal(err)
	}
	if disableSources {
		config = SandboxSourcesConfig{}
	}
	// Reconstruct the production factory, rather than retaining the old
	// in-memory quiescence implementation while flipping a boolean.
	f.r.SandboxPreparation, err = NewKubernetesSandboxPreparation(config, f.kube, f.installer,
		f.r.OperatorNamespace, f.r.ChartVersion)
	if err != nil {
		t.Fatal(err)
	}
	if f.r.SandboxPreparation != nil && f.r.SandboxPreparation.NativeExecution {
		t.Fatal("restart unexpectedly enabled new native starts")
	}
}

func TestNativeCleanupSurvivesDisablingStarts(t *testing.T) {
	for _, phase := range []string{"prepared", "unobserved Pod", "ready", "legacy started receipt"} {
		for _, disableSources := range []bool{false, true} {
			t.Run(phase+map[bool]string{false: "/starts-disabled", true: "/sources-disabled"}[disableSources], func(t *testing.T) {
				f := newNativeSandboxSkillsFixture(t, true, nil)
				f.prepare(t)
				ctx := context.Background()
				var pod *corev1.Pod
				if phase != "prepared" {
					f.reconcileOK(t)
					pod = f.createPod(t, phase != "unobserved Pod")
					if phase != "unobserved Pod" {
						f.reconcileOK(t)
						f.assertReady(t)
					}
				}
				if phase == "legacy started receipt" {
					object, _, err := f.r.readSandboxPreparationReceipt(ctx, f.current(t))
					if err != nil {
						t.Fatal(err)
					}
					var receipt map[string]json.RawMessage
					if err := json.Unmarshal([]byte(object.Data[sandboxPreparationReceiptKey]), &receipt); err != nil {
						t.Fatal(err)
					}
					delete(receipt, "nativeExecution")
					raw, err := json.Marshal(receipt)
					if err != nil {
						t.Fatal(err)
					}
					object.Data[sandboxPreparationReceiptKey] = string(raw)
					if err := f.kube.Update(ctx, object); err != nil {
						t.Fatal(err)
					}
				}
				stages := f.installer.stages
				agent := f.current(t)
				disableNativeStarts(t, f, disableSources)
				if err := f.kube.Delete(ctx, agent); err != nil {
					t.Fatal(err)
				}
				if pod != nil {
					if _, err := f.reconcile(); err == nil || !strings.Contains(err.Error(), "ordinary Pod termination") {
						t.Fatal("disabled-start cleanup did not wait for ordinary Pod termination:", err)
					}
					f.assertNotReady(t)
					if ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 || f.pod(t).UID != pod.UID {
						t.Fatal("disabled-start cleanup skipped stopping the owned workload")
					}
					if len(f.gateway.deletedTokens) == 0 {
						t.Fatal("Gateway revocation waited for Pod termination")
					}
					if err := f.kube.Delete(ctx, f.pod(t)); err != nil {
						t.Fatal(err)
					}
				}
				f.reconcileOK(t)
				f.reconcileOK(t)
				for _, object := range []client.Object{&api.Agent{}, &corev1.Namespace{}} {
					name := agent.Name
					if _, namespace := object.(*corev1.Namespace); namespace {
						name = instanceNamespace(agent.Name)
					}
					if err := f.kube.Get(ctx, client.ObjectKey{Name: name}, object); !apierrors.IsNotFound(err) {
						t.Fatalf("disabled-start cleanup retained %T: %v", object, err)
					}
				}
				if f.installer.stages != stages {
					t.Fatal("cleanup staged another native workload")
				}
			})
		}
	}
}

func TestNativeCleanupDoesNotInferModeFromCurrentConfiguration(t *testing.T) {
	for _, failure := range []string{"non-native receipt", "malformed legacy marker", "foreign receipt"} {
		t.Run(failure, func(t *testing.T) {
			f := newNativeSandboxSkillsFixture(t, true, nil)
			f.prepare(t)
			ctx := context.Background()
			agent := f.current(t)
			object, receipt, err := f.r.readSandboxPreparationReceipt(ctx, agent)
			if err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "non-native receipt":
				receipt.NativeExecution = false
			case "malformed legacy marker":
				receipt.NativeExecution = false
				object.Annotations[nativeStartAnnotation] = "not-a-recorded-revision"
			case "foreign receipt":
				object.OwnerReferences[0].UID = "different-agent"
			}
			raw, err := json.Marshal(receipt)
			if err != nil {
				t.Fatal(err)
			}
			object.Data[sandboxPreparationReceiptKey] = string(raw)
			if err := f.kube.Update(ctx, object); err != nil {
				t.Fatal(err)
			}
			if err := f.kube.Delete(ctx, agent); err != nil {
				t.Fatal(err)
			}
			disableNativeStarts(t, f, false)
			if err := f.r.checkSandboxPreparationCleanup(ctx, f.current(t)); err == nil {
				t.Fatal("cleanup accepted unknown or unowned native authority")
			}
		})
	}
}

func TestNativeCleanupAfterDisableReleasesRecordedReservation(t *testing.T) {
	f, _ := nativeCoordinationFixture(t)
	ctx := context.Background()
	if _, err := f.r.startKubernetesSandbox(ctx, f.current(t)); err != nil {
		t.Fatal(err)
	}
	pod := f.createPod(t, false)
	original := f.r.SandboxNativeCoordination
	config, err := ParseSandboxSourcesConfig(nativeSandboxSourcesJSON)
	if err != nil {
		t.Fatal(err)
	}
	config.CoordinationConfigMapName, config.CoordinationConfigMapUID = original.Identity.Name, string(original.Identity.UID)
	disableNativeStarts(t, f, false)
	f.r.SandboxNativeCoordination, err = NewNativeSandboxCoordination(config, f.kube, f.kube, f.r.OperatorNamespace)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Delete(ctx, f.current(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reconcile(); err == nil {
		t.Fatal("native reservation released before ordinary termination")
	}
	if _, found, err := original.ReadNative(ctx, f.agent.UID); err != nil || !found {
		t.Fatal("disabling starts discarded the active configuration reservation:", err)
	}
	if err := f.kube.Delete(ctx, f.pod(t)); err != nil {
		t.Fatal(err)
	}
	f.finishNamespaceGC(t)
	f.reconcileOK(t)
	if _, found, err := original.ReadNative(ctx, f.agent.UID); err != nil || found {
		t.Fatal("recorded reservation did not finish cleanup after disablement:", err)
	}
	if err := f.kube.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatal("initial Pod finalization was blocked after disablement:", err)
	}
}
