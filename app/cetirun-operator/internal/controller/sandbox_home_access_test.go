// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func withNativeHomeAccessMode(t *testing.T, mode corev1.PersistentVolumeAccessMode) func(*AgentReconciler, client.Client) {
	t.Helper()
	return func(r *AgentReconciler, kube client.Client) {
		ctx := context.Background()
		var boxes api.SandboxList
		if err := kube.List(ctx, &boxes); err != nil {
			t.Fatal(err)
		}
		if len(boxes.Items) != 1 {
			t.Fatal("HOME fixture requires exactly one Sandbox")
		}
		box := &boxes.Items[0]
		box.Spec.Storage.Home.AccessMode = mode
		if err := kube.Update(ctx, box); err != nil {
			t.Fatal(err)
		}
		object := &corev1.ConfigMap{}
		if err := kube.Get(ctx, client.ObjectKey{Namespace: r.OperatorNamespace, Name: "sandbox-platform"}, object); err != nil {
			t.Fatal(err)
		}
		var platform SandboxPlatformConfiguration
		if err := json.Unmarshal([]byte(object.Data[SandboxPlatformConfigurationKey]), &platform); err != nil {
			t.Fatal(err)
		}
		registration, found := platform.Registrations[box.Spec.Type]
		if !found {
			t.Fatal("HOME fixture requires the selected runtime registration")
		}
		registration.Capabilities.HomeAccessModes = []corev1.PersistentVolumeAccessMode{mode}
		platform.Registrations[box.Spec.Type] = registration
		raw, err := json.Marshal(platform)
		if err != nil {
			t.Fatal(err)
		}
		object.Data[SandboxPlatformConfigurationKey] = string(raw)
		if err := kube.Update(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
}

func assertNativeHomeAccessMode(t *testing.T, f *nativeSandboxFixture, mode corev1.PersistentVolumeAccessMode) {
	t.Helper()
	expected := reconciledExpectationForTest(t, f)
	home := &corev1.PersistentVolumeClaim{}
	if err := f.kube.Get(context.Background(), client.ObjectKey{Namespace: expected.Namespace.Name, Name: expected.Home.Name}, home); err != nil {
		t.Fatal(err)
	}
	if home.UID != expected.Home.UID || !slices.Equal(home.Spec.AccessModes, []corev1.PersistentVolumeAccessMode{mode}) ||
		!slices.ContainsFunc(f.workload(t).Spec.Template.Spec.Volumes, func(v corev1.Volume) bool {
			return v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == home.Name
		}) {
		t.Fatal("common workload did not retain the selected HOME mode and claim")
	}
}

func TestRetainedNativeSandboxStillRejectsRWO(t *testing.T) {
	attempt := &sandboxPreparationAttempt{
		agent: &api.Agent{},
		resolution: &sandbox.Resolution{Sandbox: api.SandboxSpec{
			Type: api.SandboxKata, Storage: api.SandboxStorage{Home: api.SandboxHomeStorage{AccessMode: corev1.ReadWriteOnce}},
		}},
	}
	attempt.agent.Spec.Agent.Type = api.AgentTypeOpenClaw
	if err := validateNativeSandboxProfile(attempt); !errors.Is(err, sandboxpolicy.ErrUnsupportedControl) {
		t.Fatal("common RWO support changed the retained single-start executor:", err)
	}
}

func TestNativeSandboxQuiescenceHomeModes(t *testing.T) {
	for _, reconciled := range []bool{false, true} {
		for _, tc := range []struct {
			name  string
			modes []corev1.PersistentVolumeAccessMode
		}{
			{"RWOP", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod}},
			{"RWO", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}},
			{"shared", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}},
			{"multiple", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce, corev1.ReadWriteOncePod}},
			{"missing", nil},
		} {
			t.Run(tc.name+"/reconciled="+strconv.FormatBool(reconciled), func(t *testing.T) {
				agent, inputs, namespace, home, workload := nativeQuiescenceObjects()
				home.Spec.AccessModes = tc.modes
				kube := fake.NewClientBuilder().WithObjects(namespace, home, workload).Build()
				source := &KubernetesNativeQuiescence{APIReader: kube, Reconciled: reconciled}
				allowed := tc.name == "RWOP" || (tc.name == "RWO" && reconciled)
				if err := source.CheckQuiesced(context.Background(), agent, inputs); (err == nil) != allowed {
					t.Fatal("quiescence did not honor the lifetime's HOME modes:", err)
				}
				if !allowed {
					return
				}
				pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "remaining-home-user", Namespace: namespace.Name}}
				if err := kube.Create(context.Background(), pod); err != nil {
					t.Fatal(err)
				}
				if err := source.CheckQuiesced(context.Background(), agent, inputs); err == nil {
					t.Fatal("HOME access mode skipped ordinary Pod termination")
				}
			})
		}
	}
}

func TestMigrationHomeAccessModes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		modes   []corev1.PersistentVolumeAccessMode
		block   bool
		allowed bool
	}{
		{"RWOP", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod}, false, true},
		{"RWO", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, false, true},
		{"shared", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, false, false},
		{"read only", []corev1.PersistentVolumeAccessMode{corev1.ReadOnlyMany}, false, false},
		{"multiple", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce, corev1.ReadWriteOncePod}, false, false},
		{"missing", nil, false, false},
		{"block", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent, _, namespace, home, _ := nativeQuiescenceObjects()
			home.OwnerReferences = slices.Clone(namespace.OwnerReferences)
			home.Spec.AccessModes, home.Spec.VolumeName, home.Status.Phase = tc.modes, "home-volume", corev1.ClaimBound
			if tc.block {
				home.Spec.VolumeMode = ptr.To(corev1.PersistentVolumeBlock)
			}
			volume := &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{Name: home.Spec.VolumeName, UID: "home-volume-uid"},
				Spec: corev1.PersistentVolumeSpec{
					ClaimRef: &corev1.ObjectReference{Name: home.Name, Namespace: home.Namespace, UID: home.UID},
					PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
						Driver: migrationTestDriver, VolumeHandle: "synthetic-home",
					}},
				},
				Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeBound},
			}
			state := &api.PlannedMigrationStatus{
				Namespace: migrationIdentity(namespace), Home: migrationIdentity(home), Volume: migrationIdentity(volume),
				Driver: migrationTestDriver, VolumeHandle: volume.Spec.CSI.VolumeHandle,
			}
			kube := fake.NewClientBuilder().WithObjects(namespace, home, volume).Build()
			r := &AgentReconciler{APIReader: kube}
			if _, err := r.migrationHome(context.Background(), agent, state); (err == nil) != tc.allowed {
				t.Fatal("migration did not enforce the HOME storage contract:", err)
			}
		})
	}
}
