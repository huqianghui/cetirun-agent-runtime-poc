// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package sandboxadmission

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestNativeEmptySkillsAPIServerAdmission(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	for _, name := range []string{"kube-apiserver", "etcd"} {
		if info, err := os.Stat(filepath.Join(assets, name)); assets == "" || err != nil || info.Mode()&0111 == 0 {
			t.Fatal("preinstalled Kubernetes envtest assets are required")
		}
	}
	env := &envtest.Environment{
		BinaryAssetsDirectory: assets, UseExistingCluster: ptr.To(false),
		CRDDirectoryPaths: []string{"../../config/crd/bases"}, ErrorIfCRDPathMissing: true,
		ControlPlaneStartTimeout: time.Minute, ControlPlaneStopTimeout: 30 * time.Second,
	}
	cfg, err := env.Start()
	if err != nil {
		_ = env.Stop()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	})
	f := newPodSkillsFixture(t, true)
	if err := admissionregistrationv1.AddToScheme(f.scheme); err != nil {
		t.Fatal(err)
	}
	cl, err := client.New(cfg, client.Options{Scheme: f.scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	create := func(object client.Object) {
		t.Helper()
		object.SetUID("")
		object.SetResourceVersion("")
		object.SetGeneration(0)
		if err := cl.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "operator-state"}})
	// Keep the real chart's Pod shape, replacing synthetic identities with
	// persisted API identities. There is no scheduler, kubelet or skills source.
	for _, object := range f.objects {
		if object == f.workload {
			continue
		}
		switch object {
		case f.fixture.Namespace:
			object.SetOwnerReferences([]metav1.OwnerReference{w.AgentOwner(f.fixture.Agent.Name, f.fixture.Agent.UID)})
			object.GetAnnotations()[w.AgentUIDAnnotation] = string(f.fixture.Agent.UID)
		case f.fixture.Input.Home:
			object.GetAnnotations()[w.AgentUIDAnnotation] = string(f.fixture.Agent.UID)
		}
		create(object)
	}
	record, err := sandboxbinding.Snapshot(f.fixture.Agent, f.fixture.Team, sandbox.Resources{
		Sandboxes:     map[string]*api.Sandbox{f.fixture.Sandbox.Name: f.fixture.Sandbox},
		AgentPolicies: map[string]*api.AgentPolicy{f.fixture.Policy.Name: f.fixture.Policy},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.fixture.Agent.Annotations[sandboxbinding.AuthorizationAnnotation] = record
	if err := cl.Update(ctx, f.fixture.Agent); err != nil {
		t.Fatal(err)
	}
	expected := f.expected
	expected.Binding, err = sandboxbinding.AuthorizedBinding(f.fixture.Agent)
	if err != nil {
		t.Fatal(err)
	}
	expected.Agent.UID, expected.AgentGeneration = f.fixture.Agent.UID, f.fixture.Agent.Generation
	expected.Namespace.UID, expected.Home.UID = f.fixture.Namespace.UID, f.fixture.Input.Home.UID
	expected.Template.Annotations[w.AgentUIDAnnotation] = string(expected.Agent.UID)
	expected.Template.Labels[sandboxpolicy.AgentUIDLabel] = string(expected.Agent.UID)
	f.workload.OwnerReferences = []metav1.OwnerReference{w.AgentOwner(expected.Agent.Name, expected.Agent.UID)}
	f.workload.Annotations[w.AgentUIDAnnotation] = string(expected.Agent.UID)
	f.workload.Spec.Template = *expected.Template.DeepCopy()
	f.workload.Spec.Selector = &metav1.LabelSelector{MatchLabels: expected.Template.Labels}
	create(f.workload)
	f.workload.Status.UpdateRevision = expected.ControllerRevision
	if err := cl.Status().Update(ctx, f.workload); err != nil {
		t.Fatal(err)
	}
	expected.Workload.UID = f.workload.UID
	expected.Sources, expected.SourceDigests = nil, map[string]string{}
	for _, object := range f.objects {
		if agent, ok := object.(*api.Agent); ok {
			source, err := w.AgentSourceReference(agent)
			if err != nil {
				t.Fatal(err)
			}
			expected.Sources = append(expected.Sources, source)
			continue
		}
		kinds, _, err := f.scheme.ObjectKinds(object)
		if err != nil {
			t.Fatal(err)
		}
		source := w.SourceReference{
			APIVersion: kinds[0].GroupVersion().String(), Kind: kinds[0].Kind,
			Namespace: object.GetNamespace(), Name: object.GetName(), UID: object.GetUID(), ResourceVersion: object.GetResourceVersion(),
		}
		expected.Sources = append(expected.Sources, source)
		if w.EffectiveSource(source) {
			digest, err := w.ExecutionSourceDigest(object, f.fixture.Sandbox.Name)
			if claim, ok := object.(*corev1.PersistentVolumeClaim); ok {
				digest, err = w.NativeClaimDigest(claim)
			}
			if err != nil {
				t.Fatal(err)
			}
			expected.SourceDigests[w.SourceKey(source)] = digest
		}
	}
	state, err := PodExpectationObject("operator-state", expected)
	if err != nil {
		t.Fatal(err)
	}
	state.Finalizers = []string{w.NativePodFinalizer}
	create(state)
	handler, err := NewPodHandler(cl, PodConfig{StateNamespace: state.Namespace})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(&admission.Webhook{Handler: handler})
	t.Cleanup(server.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	_, _, rules := renderedPodAdmission(t, ca)
	rules.Webhooks[0].ClientConfig.Service = nil
	rules.Webhooks[0].ClientConfig.URL = ptr.To(server.URL + PodValidationPath)
	create(rules)
	pod := testfixture.Pod(expected)
	pod.Spec.Overhead = nil
	bad := pod.DeepCopy()
	bad.Annotations[w.KindAnnotation] = "unapproved"
	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 20*time.Second, true, func(ctx context.Context) (bool, error) {
		err := cl.Create(ctx, bad.DeepCopy(), client.DryRunAll)
		return err != nil && strings.Contains(err.Error(), "TemplateMismatch"), nil
	}); err != nil {
		t.Fatal("rendered Pod webhook did not become active:", err)
	}
	for _, volumeSource := range []bool{false, true} {
		changed := pod.DeepCopy()
		if volumeSource {
			for i := range changed.Spec.Volumes {
				if changed.Spec.Volumes[i].Name == "empty-skills" {
					changed.Spec.Volumes[i].VolumeSource = corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "unapproved", ReadOnly: true},
					}
				}
			}
		} else {
			for i := range changed.Spec.Containers[0].VolumeMounts {
				if changed.Spec.Containers[0].VolumeMounts[i].Name == "empty-skills" {
					changed.Spec.Containers[0].VolumeMounts[i].ReadOnly = false
				}
			}
		}
		if err := cl.Create(ctx, changed, client.DryRunAll); err == nil || !strings.Contains(err.Error(), "TemplateMismatch") {
			t.Fatal("served admission did not reject changed empty-skills storage:", err)
		}
	}
	if err := cl.Create(ctx, pod); err != nil {
		t.Fatal("real API-defaulted native empty-skills Pod was rejected:", err)
	}
	if err := w.ValidatePod(pod, expected); err != nil {
		t.Fatal("API defaulting changed the native empty-skills contract:", err)
	}
	var claims corev1.PersistentVolumeClaimList
	if err := cl.List(ctx, &claims, client.InNamespace(pod.Namespace)); err != nil || len(claims.Items) != 1 ||
		claims.Items[0].UID != expected.Home.UID {
		t.Fatal("native empty skills needed more than HOME storage:", err)
	}
	unchanged := &corev1.ConfigMap{}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(state), unchanged); err != nil || unchanged.ResourceVersion != state.ResourceVersion {
		t.Fatal("served admission mutated its expectation:", err)
	}
	if err := cl.Delete(ctx, pod, client.GracePeriodSeconds(0)); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil || pod.DeletionTimestamp.IsZero() ||
		!slices.Contains(pod.Finalizers, w.NativePodFinalizer) {
		t.Fatal("early deletion lost the initial native lifetime:", err)
	}
	var persisted w.Expectation
	if err := json.Unmarshal([]byte(unchanged.Data[w.PodExpectationKey]), &persisted); err != nil ||
		persisted.SkillsMode != w.SkillsModeEmpty || persisted.PodUID != "" {
		t.Fatal("read-only admission consumed or lost the empty-skills lifetime:", err)
	}
}
