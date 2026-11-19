// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	sandboxadmission "github.com/Azure/CetiRun/app/cetirun-operator/internal/admission"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestNativePublishedSkillsRejectsRuntimePackageRequests(t *testing.T) {
	for _, selection := range []string{"example-skill", "npm:example-package"} {
		t.Run(selection, func(t *testing.T) {
			f := newSandboxRuntimeFixtureForAgent(t, true, []string{selection}, false, "openclaw", true, false,
				withNativePublishedSkills(t))
			f.projectGateway(t)
			err := f.r.prepareSandboxHeld(context.Background(), f.current(t))
			if err == nil || !strings.Contains(err.Error(), "SkillsMaterializationPending") {
				t.Fatal("published PVC silently accepted an unmaterialized package request:", err)
			}
			f.assertNotReady(t)
			if f.installer.stages != 0 {
				t.Fatal("unsupported package request staged a workload")
			}
		})
	}
}

func TestNativeSandboxEmptySkillsLifecycle(t *testing.T) {
	for _, catalog := range []string{"", "skills-catalog"} {
		t.Run("catalog="+catalog, func(t *testing.T) { testNativeSandboxEmptySkillsLifecycle(t, catalog) })
	}
}

func testNativeSandboxEmptySkillsLifecycle(t *testing.T, catalog string) {
	f := newNativeSandboxSkillsFixture(t, true, nil)
	ctx := context.Background()
	source := f.r.SandboxPreparation.Prerequisites.(*KubernetesSandboxPrerequisites)
	if catalog == "" {
		source.SkillsClaimName, f.r.SandboxPreparation.bootstrap.SkillsClaimName = "", ""
	}
	source.SkillsCatalogConfigMapName, f.r.SandboxPreparation.bootstrap.SkillsCatalogConfigMapName = catalog, catalog
	if catalog != "" {
		if err := f.kube.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: catalog, Namespace: f.r.OperatorNamespace},
			Data: map[string]string{SandboxSkillsCatalogKey: `{"version":"v1","teams":{"other-team":{"teamUID":"other-team-uid","revision":"v1","source":{"resourceGroup":"example","storageAccount":"exampleskills","containerName":"skills","storageClassName":"azureblob-nfs-premium","storageSize":"1Gi"}}}}`}}); err != nil {
			t.Fatal(err)
		}
	}
	reader := interceptor.NewClient(f.kube, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
			if _, ok := object.(*corev1.ConfigMap); ok && catalog != "" && key.Name == catalog {
				return fmt.Errorf("empty skills unexpectedly read the global publication catalog")
			}
			if _, ok := object.(*corev1.PersistentVolume); ok {
				return fmt.Errorf("empty skills unexpectedly read a publication volume")
			}
			if _, ok := object.(*corev1.PersistentVolumeClaim); ok && key.Name != source.HomeClaimName {
				return fmt.Errorf("empty skills unexpectedly read a skills PVC")
			}
			return c.Get(ctx, key, object, opts...)
		},
	})
	source.APIReader, source.NativeQuiescence.APIReader, f.r.APIReader = reader, reader, reader
	f.r.SandboxPreparation.Configuration.(*KubernetesSandboxConfigurationSource).APIReader = reader
	f.prepare(t)
	state, held := f.expectation(t)
	if !held.ExecutionHeld || held.SinglePod || held.SkillsMode != w.SkillsModeEmpty || held.Skills != (w.ObjectIdentity{}) ||
		ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 ||
		!slices.Equal(held.Template.Finalizers, []string{w.NativePodFinalizer}) {
		t.Fatal("empty selection did not produce an explicitly held native empty-skills expectation")
	}
	bootstrap, inventory, err := f.r.readSandboxBootstrap(ctx, f.current(t))
	if err != nil {
		t.Fatal(err)
	}
	if inventory.Version != "v1" || inventory.Skills != nil || len(inventory.Objects) != 5 {
		t.Fatal("empty skills invented a publication receipt or storage objects")
	}
	receipt, preparation, err := f.r.readSandboxPreparationReceipt(ctx, f.current(t))
	if err != nil {
		t.Fatal(err)
	}
	prerequisites := preparation.Prerequisites
	if prerequisites.SkillsMode != w.SkillsModeEmpty || prerequisites.Skills != (w.ObjectIdentity{}) ||
		len(prerequisites.PublicationSources) != 0 || receipt.Annotations[nativeStartAnnotation] != "" {
		t.Fatal("empty prerequisites acquired a publication, PVC identity or early start marker")
	}
	handler, err := sandboxadmission.NewPodHandler(reader, sandboxadmission.PodConfig{StateNamespace: f.r.OperatorNamespace})
	if err != nil {
		t.Fatal(err)
	}
	admit := func(pod *corev1.Pod) admission.Response {
		t.Helper()
		raw, err := json.Marshal(pod)
		if err != nil {
			t.Fatal(err)
		}
		return handler.Validate(ctx, admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Create, Namespace: pod.Namespace, Name: pod.Name,
			Kind:     metav1.GroupVersionKind{Version: "v1", Kind: "Pod"},
			Resource: metav1.GroupVersionResource{Version: "v1", Resource: "pods"},
			Object:   runtime.RawExtension{Raw: raw},
		}})
	}
	if response := admit(testfixture.Pod(*held)); response.Allowed || !strings.Contains(response.Result.Message, "ExecutionHeld") {
		t.Fatalf("empty skills lifted admission's preparation hold: %+v", response.Result)
	}
	f.reconcileOK(t)
	runningState, running := f.expectation(t)
	currentReceipt, _, err := f.r.readSandboxPreparationReceipt(ctx, f.current(t))
	if err != nil {
		t.Fatal(err)
	}
	if runningState.UID != state.UID || !slices.Contains(runningState.Finalizers, w.NativePodFinalizer) ||
		running.ExecutionHeld || !running.SinglePod || running.SkillsMode != w.SkillsModeEmpty ||
		slices.Contains(f.current(t).Finalizers, sandbox.HoldFinalizer) ||
		currentReceipt.Annotations[nativeStartAnnotation] != held.Revision {
		t.Fatal("native start lost its one-shot marker, retained expectation or empty source")
	}

	claims := 0
	for _, reference := range running.Sources {
		if reference.Kind == "PersistentVolume" {
			t.Fatal("empty skills pinned a publication volume")
		}
		if reference.Kind == "PersistentVolumeClaim" {
			claims++
			if reference.Name != running.Home.Name || reference.UID != running.Home.UID ||
				!w.ValidInputDigest(running.SourceDigests[w.SourceKey(reference)]) {
				t.Fatal("native HOME binding digest was lost or a skills claim was invented")
			}
		}
	}
	if claims != 1 {
		t.Fatal("empty skills changed the required HOME source inventory")
	}
	if err := running.ValidateSkills(); err != nil {
		t.Fatal(err)
	}
	pod := testfixture.Pod(*running)
	if ptr.Deref(pod.Spec.AutomountServiceAccountToken, true) || pod.Spec.ServiceAccountName != "default" {
		t.Fatal("empty skills enabled a workload identity grant")
	}
	for _, volume := range pod.Spec.Volumes {
		if volume.Projected != nil {
			t.Fatal("empty skills added an identity projection")
		}
	}
	f.assertGatewayProjection(t, running)
	config := &corev1.ConfigMap{}
	if err := f.kube.Get(ctx, client.ObjectKey{Namespace: running.Namespace.Name, Name: running.Workload.Name + "-config"}, config); err != nil {
		t.Fatal(err)
	}
	var native struct {
		Skills struct {
			Load struct {
				ExtraDirs []string `json:"extraDirs"`
			} `json:"load"`
		} `json:"skills"`
	}
	if err := json.Unmarshal([]byte(config.Data["openclaw.json"]), &native); err != nil ||
		!slices.Equal(native.Skills.Load.ExtraDirs, []string{w.SkillsPath}) {
		t.Fatal("Go native rendering lost the read-only empty skills directory:", err)
	}
	if response := admit(pod); !response.Allowed {
		t.Fatalf("actual native empty chart was not admitted: %+v", response.Result)
	}
	afterAdmission, _ := f.expectation(t)
	if afterAdmission.ResourceVersion != runningState.ResourceVersion {
		t.Fatal("read-only admission mutated the native expectation")
	}
	for name, mutate := range map[string]func(*corev1.Pod){
		"writable skills": func(p *corev1.Pod) {
			for i := range p.Spec.Containers[0].VolumeMounts {
				if p.Spec.Containers[0].VolumeMounts[i].Name == "empty-skills" {
					p.Spec.Containers[0].VolumeMounts[i].ReadOnly = false
				}
			}
		},
		"skills PVC substitution": func(p *corev1.Pod) {
			for i := range p.Spec.Volumes {
				if p.Spec.Volumes[i].Name == "empty-skills" {
					p.Spec.Volumes[i].VolumeSource = corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "unapproved", ReadOnly: true},
					}
				}
			}
		},
		"helper writer": func(p *corev1.Pod) {
			p.Spec.InitContainers[0].VolumeMounts = append(p.Spec.InitContainers[0].VolumeMounts,
				corev1.VolumeMount{Name: "empty-skills", MountPath: "/skills"})
		},
		"token grant":      func(p *corev1.Pod) { p.Spec.AutomountServiceAccountToken = ptr.To(true) },
		"missing lifetime": func(p *corev1.Pod) { p.Finalizers = nil },
	} {
		t.Run(name, func(t *testing.T) {
			changed := pod.DeepCopy()
			mutate(changed)
			if response := admit(changed); response.Allowed {
				t.Fatal("unapproved native empty-skills mutation was admitted")
			}
		})
	}
	initial := f.createPod(t, false)
	f.reconcileOK(t)
	f.assertNotReady(t)
	_, observed := f.expectation(t)
	if observed.PodUID != initial.UID {
		t.Fatal("empty skills bypassed initial Pod lifetime recording")
	}
	current := f.pod(t)
	current.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if err := f.kube.Status().Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	f.reconcileOK(t)
	f.assertReady(t)
	var volumes corev1.PersistentVolumeList
	var storage corev1.PersistentVolumeClaimList
	if err := f.kube.List(ctx, &volumes); err != nil || len(volumes.Items) != 0 {
		t.Fatal("empty skills created a publication PV:", err)
	}
	if err := f.kube.List(ctx, &storage); err != nil || len(storage.Items) != 1 || storage.Items[0].Name != running.Home.Name {
		t.Fatal("empty skills created a skills PVC:", err)
	}
	if err := f.kube.Delete(ctx, f.current(t)); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Delete(ctx, f.pod(t)); err != nil {
		t.Fatal(err)
	}
	f.reconcileOK(t)
	f.reconcileOK(t)
	for _, object := range []client.Object{
		&api.Agent{ObjectMeta: metav1.ObjectMeta{Name: f.agent.Name}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: running.Namespace.Name}},
		state, bootstrap, receipt,
	} {
		if err := f.kube.Get(ctx, client.ObjectKeyFromObject(object), object); !apierrors.IsNotFound(err) {
			t.Fatalf("native deletion retained %T or its lifecycle state: %v", object, err)
		}
	}
}

func TestNativeSandboxEmptySkillsUsesConfigurationReservation(t *testing.T) {
	f := newNativeSandboxSkillsFixture(t, true, nil)
	ctx := context.Background()
	authority, err := sandboxcoordination.NewState(f.r.OperatorNamespace, "empty-skills-coordination")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Create(ctx, authority); err != nil {
		t.Fatal(err)
	}
	store := &sandboxcoordination.Store{
		Client: f.kube, APIReader: f.kube, Namespace: authority.Namespace,
		Identity: w.ObjectIdentity{Name: authority.Name, UID: authority.UID},
	}
	f.r.SandboxNativeCoordination = store
	f.ready(t)
	_, expected := f.expectation(t)
	if expected.SkillsMode != w.SkillsModeEmpty || expected.Skills != (w.ObjectIdentity{}) {
		t.Fatal("coordinated startup reintroduced a skills claim")
	}
	if _, found, err := store.ReadNative(ctx, f.agent.UID); err != nil || !found {
		t.Fatal("empty-skills startup did not reserve its configuration:", err)
	}
	before, after := nativePolicyChange(t, f, expected)
	if _, err := store.AcquireConfigurationChange(ctx, before, after); !errors.Is(err, sandboxcoordination.ErrConflict) {
		t.Fatal("configuration changed while the empty-skills Agent ran:", err)
	}
	pod := f.pod(t)
	if err := f.kube.Delete(ctx, f.current(t)); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	f.reconcileOK(t)
	f.reconcileOK(t)
	if _, found, err := store.ReadNative(ctx, f.agent.UID); err != nil || found {
		t.Fatal("ordinary empty-skills cleanup retained its reservation:", err)
	}
}

func TestNativeSandboxEmptySkillsDeletionBeforeFirstObservation(t *testing.T) {
	f := newNativeSandboxSkillsFixture(t, true, nil)
	f.prepare(t)
	f.reconcileOK(t)
	pod := f.createPod(t, false)
	ctx := context.Background()
	state, _ := f.expectation(t)
	for _, object := range []client.Object{f.current(t), state, pod} {
		if err := f.kube.Delete(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.kube.Get(ctx, client.ObjectKeyFromObject(state), &corev1.ConfigMap{}); err != nil {
		t.Fatal("foreground deletion lost the empty-skills lifetime record:", err)
	}
	f.reconcileOK(t)
	f.reconcileOK(t)
	for _, object := range []client.Object{state, pod} {
		if err := f.kube.Get(ctx, client.ObjectKeyFromObject(object), object); !apierrors.IsNotFound(err) {
			t.Fatal("empty-skills early deletion could not finish native finalization:", err)
		}
	}
}

func TestNativeSandboxEmptySkillsRejectsInconsistentPrerequisites(t *testing.T) {
	for name, change := range map[string]func(*nativeSandboxFixture, *sandboxPreparationAttempt){
		"unknown mode": func(_ *nativeSandboxFixture, a *sandboxPreparationAttempt) {
			a.prerequisites.SkillsMode = "unsupported"
		},
		"PVC and empty": func(_ *nativeSandboxFixture, a *sandboxPreparationAttempt) {
			a.prerequisites.Skills = w.ObjectIdentity{Name: "skills-example", UID: "skills-uid"}
		},
		"publication and empty": func(_ *nativeSandboxFixture, a *sandboxPreparationAttempt) {
			a.prerequisites.PublicationSources = []w.SourceReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "catalog"}}
		},
		"requested skill": func(_ *nativeSandboxFixture, a *sandboxPreparationAttempt) {
			a.agent.Spec.SkillOverrides.Add = []string{"example-skill"}
		},
		"shared skill storage": func(_ *nativeSandboxFixture, a *sandboxPreparationAttempt) {
			a.team.Spec.SkillStorage = &api.SkillStorageSpec{ResourceGroup: "example", StorageAccount: "exampleskills", ContainerName: "skills"}
		},
		"default held path": func(f *nativeSandboxFixture, _ *sandboxPreparationAttempt) {
			f.r.SandboxPreparation.NativeExecution = false
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newNativeSandboxSkillsFixture(t, true, nil)
			f.projectGateway(t)
			attempt, err := f.r.resolveSandboxPreparation(context.Background(), f.current(t))
			if err != nil {
				t.Fatal(err)
			}
			attempt.prerequisites, err = f.r.SandboxPreparation.Prerequisites.Resolve(context.Background(), attempt.agent, attempt.team)
			if err != nil {
				t.Fatal(err)
			}
			change(f, attempt)
			if err := f.r.sandboxPreparationInput(context.Background(), attempt); err == nil {
				t.Fatal("inconsistent internal empty-skills prerequisite was accepted")
			}
		})
	}
}

func TestNativeEmptySkillsBootstrapModeCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, marker                string
		claim, wantEmpty, wantError bool
	}{
		{name: "new empty receipt survives catalog enablement", marker: "empty", wantEmpty: true},
		{name: "legacy receipt retains supplied claim", claim: true},
		{name: "legacy receipt cannot lose supplied claim", wantError: true},
		{name: "unknown marker is rejected", marker: "unknown", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newNativeSandboxSkillsFixture(t, true, nil)
			f.projectGateway(t)
			agent := f.current(t)
			record, receipt, err := f.r.readSandboxBootstrap(ctx, agent)
			if err != nil {
				t.Fatal(err)
			}
			if receipt.Version != "v1" || record.Annotations[sandboxBootstrapSkillsModeAnnotation] != string(w.SkillsModeEmpty) {
				t.Fatal("new empty bootstrap did not explicitly record its storage selection")
			}
			if tc.marker == "" {
				delete(record.Annotations, sandboxBootstrapSkillsModeAnnotation)
			} else {
				record.Annotations[sandboxBootstrapSkillsModeAnnotation] = tc.marker
			}
			if err := f.kube.Update(ctx, record); err != nil {
				t.Fatal(err)
			}
			source := f.r.SandboxPreparation.Prerequisites.(*KubernetesSandboxPrerequisites)
			source.SkillsCatalogConfigMapName = "later-global-catalog"
			var claim *corev1.PersistentVolumeClaim
			if tc.claim {
				claim = testfixture.New("openclaw").Input.Skills.DeepCopy()
				claim.Name, claim.Namespace = source.SkillsClaimName, instanceNamespace(agent.Name)
				claim.UID, claim.ResourceVersion = "", ""
				claim.OwnerReferences = []metav1.OwnerReference{w.AgentOwner(agent.Name, agent.UID)}
				if err := f.kube.Create(ctx, claim); err != nil {
					t.Fatal(err)
				}
			}
			team := &api.Team{}
			if err := f.kube.Get(ctx, client.ObjectKey{Name: agent.Spec.Team}, team); err != nil {
				t.Fatal(err)
			}
			got, err := source.Resolve(ctx, agent, team)
			if tc.wantError {
				if err == nil {
					t.Fatal("invalid or missing retained skills authority was accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantEmpty {
				if got.SkillsMode != w.SkillsModeEmpty || got.Skills.UID != "" {
					t.Fatal("catalog enablement replaced explicit empty skills")
				}
			} else if got.SkillsMode != "" || claim == nil || got.Skills.UID != claim.UID {
				t.Fatal("legacy external claim was replaced with empty skills")
			}
		})
	}
}
