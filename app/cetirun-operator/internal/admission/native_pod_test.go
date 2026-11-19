// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func nativePodFixture(t *testing.T) podFixture {
	t.Helper()
	return nativePodSkillsFixture(t, false)
}

func nativePodSkillsFixture(t *testing.T, emptySkills bool) podFixture {
	t.Helper()
	f := newPodSkillsFixture(t, emptySkills)
	f.expected.SinglePod = true
	f.expected.Template.Finalizers = []string{w.NativePodFinalizer}
	f.workload.Spec.Template.Finalizers = []string{w.NativePodFinalizer}
	f.pod.Finalizers = []string{w.NativePodFinalizer}
	if err := f.client.Update(context.Background(), f.workload); err != nil {
		t.Fatal(err)
	}
	f.expected.SourceDigests = map[string]string{}
	for _, source := range f.expected.Sources {
		if !w.EffectiveSource(source) {
			continue
		}
		for _, object := range f.objects {
			if object.GetName() != source.Name || object.GetNamespace() != source.Namespace || object.GetUID() != source.UID {
				continue
			}
			digest, err := w.ExecutionSourceDigest(object, f.fixture.Sandbox.Name)
			if claim, ok := object.(*corev1.PersistentVolumeClaim); ok {
				digest, err = w.NativeClaimDigest(claim)
			}
			if err != nil {
				t.Fatal(err)
			}
			f.expected.SourceDigests[w.SourceKey(source)] = digest
		}
	}
	saveNativePodExpectation(t, &f)
	return f
}

func TestNativeEmptySkillsAdmissionRejectsInconsistentExpectations(t *testing.T) {
	for name, mutate := range map[string]func(*podFixture){
		"unknown mode":           func(f *podFixture) { f.expected.SkillsMode = "unknown" },
		"implicit empty":         func(f *podFixture) { f.expected.SkillsMode = "" },
		"claim identity":         func(f *podFixture) { f.expected.Skills = w.ObjectIdentity{Name: "skills", UID: "skills-uid"} },
		"non-native expectation": func(f *podFixture) { f.expected.SinglePod = false },
		"missing lifetime":       func(f *podFixture) { f.expected.Template.Finalizers = nil },
		"extra publication source": func(f *podFixture) {
			f.expected.Sources = append(f.expected.Sources, w.SourceReference{
				APIVersion: "v1", Kind: "PersistentVolume", Name: "skills", UID: "skills-uid", ResourceVersion: "1",
			})
		},
		"writable template": func(f *podFixture) {
			for i := range f.expected.Template.Spec.Containers[0].VolumeMounts {
				if f.expected.Template.Spec.Containers[0].VolumeMounts[i].Name == "empty-skills" {
					f.expected.Template.Spec.Containers[0].VolumeMounts[i].ReadOnly = false
				}
			}
		},
		"populated template": func(f *podFixture) {
			for i := range f.expected.Template.Spec.Volumes {
				if f.expected.Template.Spec.Volumes[i].Name == "empty-skills" {
					f.expected.Template.Spec.Volumes[i].VolumeSource = corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "example-config"}},
					}
				}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := nativePodSkillsFixture(t, true)
			if response := f.handler.Validate(context.Background(), podRequest(t, f.pod, admissionv1.Create, nil)); !response.Allowed {
				t.Fatalf("actual empty chart fixture was rejected: %+v", response.Result)
			}
			mutate(&f)
			saveNativePodExpectation(t, &f)
			if response := f.handler.Validate(context.Background(), podRequest(t, f.pod, admissionv1.Create, nil)); response.Allowed {
				t.Fatal("inconsistent empty-skills expectation was admitted")
			}
		})
	}
}

func TestNativeEmptySkillsAdmissionPinsSelectionAndHome(t *testing.T) {
	for _, change := range []string{"Team skill", "Agent addition", "HOME digest"} {
		t.Run(change, func(t *testing.T) {
			f := nativePodSkillsFixture(t, true)
			ctx := context.Background()
			switch change {
			case "Team skill":
				team := &api.Team{}
				if err := f.client.Get(ctx, client.ObjectKeyFromObject(f.fixture.Team), team); err != nil {
					t.Fatal(err)
				}
				team.Spec.Skills = []string{"example-skill"}
				if err := f.client.Update(ctx, team); err != nil {
					t.Fatal(err)
				}
			case "Agent addition":
				agent := f.fixture.Agent.DeepCopy()
				agent.Spec.SkillOverrides.Add = []string{"example-skill"}
				if err := f.client.Update(ctx, agent); err != nil {
					t.Fatal(err)
				}
			case "HOME digest":
				for _, source := range f.expected.Sources {
					if source.Kind == "PersistentVolumeClaim" {
						delete(f.expected.SourceDigests, w.SourceKey(source))
					}
				}
				saveNativePodExpectation(t, &f)
			}
			response := f.handler.Validate(ctx, podRequest(t, f.pod, admissionv1.Create, nil))
			if response.Allowed || response.Result == nil || !strings.Contains(response.Result.Message, "SourceChanged") {
				t.Fatalf("changed empty selection or HOME pin was not rejected: %+v", response.Result)
			}
			if !slices.Contains(f.pod.Finalizers, w.NativePodFinalizer) {
				t.Fatal("empty native fixture lost initial Pod lifetime protection")
			}
		})
	}
}

func saveNativePodExpectation(t *testing.T, f *podFixture) {
	t.Helper()
	if err := f.client.Get(context.Background(), client.ObjectKeyFromObject(f.state), f.state); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(f.expected)
	if err != nil {
		t.Fatal(err)
	}
	f.state.Data[w.PodExpectationKey] = string(raw)
	if err := f.client.Update(context.Background(), f.state); err != nil {
		t.Fatal(err)
	}
}

func TestNativePodAdmissionToleratesOnlyObservedSourceChanges(t *testing.T) {
	for _, test := range []struct {
		name    string
		change  func(*testing.T, *podFixture)
		allowed bool
	}{
		{name: "initial", allowed: true},
		{name: "StatefulSet status", allowed: true, change: func(t *testing.T, f *podFixture) {
			workload := &appsv1.StatefulSet{}
			if err := f.client.Get(context.Background(), client.ObjectKeyFromObject(f.workload), workload); err != nil {
				t.Fatal(err)
			}
			workload.Status.ReadyReplicas = 1
			if err := f.client.Update(context.Background(), workload); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "effective input changed", change: func(t *testing.T, f *podFixture) {
			config := &corev1.ConfigMap{}
			key := client.ObjectKey{Namespace: f.pod.Namespace, Name: "example-config"}
			if err := f.client.Get(context.Background(), key, config); err != nil {
				t.Fatal(err)
			}
			config.Data = map[string]string{"setting": "changed"}
			if err := f.client.Update(context.Background(), config); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "missing captured inputs", change: func(t *testing.T, f *podFixture) {
			f.expected.SourceDigests = nil
			saveNativePodExpectation(t, f)
		}},
		{name: "workload template changed", change: func(t *testing.T, f *podFixture) {
			workload := &appsv1.StatefulSet{}
			if err := f.client.Get(context.Background(), client.ObjectKeyFromObject(f.workload), workload); err != nil {
				t.Fatal(err)
			}
			workload.Spec.Template.Spec.Containers[0].Image = "registry.example.com/unapproved:1"
			if err := f.client.Update(context.Background(), workload); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := nativePodFixture(t)
			if test.change != nil {
				test.change(t, &f)
			}
			response := f.handler.Validate(context.Background(), podRequest(t, f.pod, admissionv1.Create, nil))
			if response.Allowed != test.allowed {
				t.Fatalf("allowed=%t want=%t result=%+v", response.Allowed, test.allowed, response.Result)
			}
		})
	}
}

func TestNativePodAdmissionNeverRecreatesAPinnedPod(t *testing.T) {
	f := nativePodFixture(t)
	f.expected.PodUID = "first-pod-uid"
	saveNativePodExpectation(t, &f)
	for _, spoofUID := range []bool{false, true} {
		pod := f.pod.DeepCopy()
		if spoofUID {
			pod.UID = f.expected.PodUID
		}
		response := f.handler.Validate(context.Background(), podRequest(t, pod, admissionv1.Create, nil))
		if response.Allowed || response.Result == nil || !strings.Contains(response.Result.Message, "ReplacementUnsupported") {
			t.Fatalf("replacement was not rejected independently of caller-supplied UID: %+v", response.Result)
		}
	}
}

func TestNativePodPinKeepsOrdinaryDeletionAvailable(t *testing.T) {
	f := nativePodFixture(t)
	f.expected.PodUID = "first-pod-uid"
	f.expected.ExecutionHeld = true
	saveNativePodExpectation(t, &f)
	response := f.handler.Validate(context.Background(), podRequest(t, f.pod, admissionv1.Delete, nil))
	if !response.Allowed {
		t.Fatalf("native one-shot state blocked ordinary Pod cleanup: %+v", response.Result)
	}
}

func TestNativePodAdmissionAllowsClaimBindingBeforeFirstCreate(t *testing.T) {
	for _, name := range []string{"home", "skills"} {
		t.Run(name, func(t *testing.T) {
			f := nativePodFixture(t)
			identity := f.expected.Home
			if name == "skills" {
				identity = f.expected.Skills
			}
			claim := &corev1.PersistentVolumeClaim{}
			if err := f.client.Get(context.Background(), client.ObjectKey{Namespace: f.pod.Namespace, Name: identity.Name}, claim); err != nil {
				t.Fatal(err)
			}
			claim.Spec.VolumeName = "bound-example-volume"
			claim.Status.Phase = corev1.ClaimBound
			if claim.Annotations == nil {
				claim.Annotations = map[string]string{}
			}
			claim.Annotations["pv.kubernetes.io/bind-completed"] = "yes"
			claim.Finalizers = append(claim.Finalizers, "kubernetes.io/pvc-protection")
			if err := f.client.Update(context.Background(), claim); err != nil {
				t.Fatal(err)
			}
			claim.Status.Phase = corev1.ClaimBound
			if err := f.client.Status().Update(context.Background(), claim); err != nil {
				t.Fatal(err)
			}
			response := f.handler.Validate(context.Background(), podRequest(t, f.pod, admissionv1.Create, nil))
			if !response.Allowed {
				t.Fatalf("binding-only claim changes blocked the first Pod: %+v", response.Result)
			}
		})
	}
}

func TestNativePodAdmissionRejectsChangedClaimInputs(t *testing.T) {
	for _, change := range []struct {
		name   string
		mutate func(*corev1.PersistentVolumeClaim)
	}{
		{"owner", func(p *corev1.PersistentVolumeClaim) {
			p.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: "other", UID: "other-owner"}}
		}},
		{"storage class", func(p *corev1.PersistentVolumeClaim) { p.Spec.StorageClassName = ptr.To("other-storage") }},
		{"access mode", func(p *corev1.PersistentVolumeClaim) {
			p.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadOnlyMany}
		}},
		{"size", func(p *corev1.PersistentVolumeClaim) {
			p.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("99Gi")}
		}},
		{"lost", func(p *corev1.PersistentVolumeClaim) { p.Status.Phase = corev1.ClaimLost }},
	} {
		t.Run(change.name, func(t *testing.T) {
			f := nativePodFixture(t)
			claim := &corev1.PersistentVolumeClaim{}
			if err := f.client.Get(context.Background(), client.ObjectKey{Namespace: f.pod.Namespace, Name: f.expected.Home.Name}, claim); err != nil {
				t.Fatal(err)
			}
			change.mutate(claim)
			phase := claim.Status.Phase
			if err := f.client.Update(context.Background(), claim); err != nil {
				t.Fatal(err)
			}
			claim.Status.Phase = phase
			if err := f.client.Status().Update(context.Background(), claim); err != nil {
				t.Fatal(err)
			}
			if response := f.handler.Validate(context.Background(), podRequest(t, f.pod, admissionv1.Create, nil)); response.Allowed {
				t.Fatal("changed claim ownership or storage input was accepted")
			}
		})
	}
}
