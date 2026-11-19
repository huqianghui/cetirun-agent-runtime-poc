// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"helm.sh/helm/v3/pkg/chart/loader"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type providerTestClient struct{ client.Client }

type namespaceLagClient struct{ client.Client }

func (c namespaceLagClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, options ...client.GetOption) error {
	if _, ok := obj.(*corev1.Namespace); ok {
		return apierrors.NewNotFound(corev1.Resource("namespaces"), key.Name)
	}
	return c.Client.Get(ctx, key, obj, options...)
}

func (c providerTestClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if obj.GetUID() == "" {
		obj.SetUID(types.UID("synthetic-" + obj.GetName()))
	}
	return c.Client.Create(ctx, obj, opts...)
}

func providerFixture(t *testing.T) (*KubernetesCustomBindings, *api.Agent) {
	t.Helper()
	image := "registry.example.com/pi@sha256:" + strings.Repeat("a", 64)
	cfg := CustomRuntimeConfig{Enabled: true, ApprovedImages: []string{image},
		StorageClass: "test-storage", StorageSize: "20Gi", AccessMode: "ReadWriteMany", FSGroup: 1024,
		Context: map[string]string{"AGENTS.md": "approved context", "skills/cetirun-memory/SKILL.md": "approved memory"}}
	data, _ := json.Marshal(cfg)
	parsed, err := ParseCustomRuntimeConfig(string(data))
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{api.AddToScheme, corev1.AddToScheme, appsv1.AddToScheme, rbacv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	instance := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "pi-test", UID: "pi-agent-uid", Generation: 1, Finalizers: []string{api.Finalizer}},
		Spec: api.AgentSpec{Agent: api.HarnessSpec{Type: api.AgentTypeCustom, Image: &image},
			User: api.UserSpec{Alias: "owner", DisplayName: "Test"}, Helm: api.HelmSpec{Version: "0.1.1"}}}
	cl := providerTestClient{fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&api.Agent{}, &corev1.PersistentVolumeClaim{}).WithObjects(instance).Build()}
	r := &AgentReconciler{Client: cl, APIReader: cl, Scheme: scheme, OperatorNamespace: "cetirun-system",
		OperatorSAName: "operator-test", IstioRevision: "asm-test"}
	return NewKubernetesCustomBindings(r, parsed), instance
}

func readyProvider(t *testing.T) (*KubernetesCustomBindings, *api.Agent) {
	t.Helper()
	p, instance := providerFixture(t)
	ctx := context.Background()
	if _, err := p.Resolve(ctx, instance, nil); err == nil {
		t.Fatal("namespace ownership was not recorded first")
	}
	if err := p.reconciler.Get(ctx, client.ObjectKeyFromObject(instance), instance); err != nil {
		t.Fatal(err)
	}
	b, err := p.Resolve(ctx, instance, nil)
	if err != nil {
		t.Fatal(err)
	}
	simulateChartHome(t, p, instance, b, true)
	b, err = p.Resolve(ctx, instance, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b.Model.Name != "" || b.Network.GatewayPort != 0 {
		t.Fatal("provider must wait for the controller Gateway binding")
	}
	secrets := &corev1.SecretList{}
	if err := p.reconciler.List(ctx, secrets); err != nil || len(secrets.Items) != 0 {
		t.Fatal("binding resolution must not create a model Secret")
	}
	return p, instance
}

func simulateChartHome(t *testing.T, p *KubernetesCustomBindings, instance *api.Agent, b runtimebindings.Custom, bound bool) *corev1.PersistentVolumeClaim {
	t.Helper()
	// The fake client has no StatefulSet or CSI controller; simulate their output.
	home := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: b.Home.Name, Namespace: b.Namespace.Name,
		Annotations: map[string]string{customAgentUID: string(instance.UID)}}, Spec: b.Home.Claim.PVCSpec()}
	if bound {
		home.Spec.VolumeName = "synthetic-bound-volume"
	}
	if err := p.reconciler.Create(context.Background(), home); err != nil {
		t.Fatal(err)
	}
	home.Status.Phase = corev1.ClaimPending
	if bound {
		home.Status.Phase = corev1.ClaimBound
	}
	if err := p.reconciler.Status().Update(context.Background(), home); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestKubernetesCustomProvisioning(t *testing.T) {
	p, instance := readyProvider(t)
	ctx := context.Background()
	first, err := p.Resolve(ctx, instance, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.Resolve(ctx, instance, nil)
	if err != nil || first.Context.ObjectReference != second.Context.ObjectReference || !reflect.DeepEqual(first.Home, second.Home) {
		t.Fatal("unchanged provisioning replaced HOME or context")
	}
}

func pendingHomeProvider(t *testing.T) (*KubernetesCustomBindings, *api.Agent) {
	t.Helper()
	p, instance := providerFixture(t)
	chart, err := loader.Load("../../chart")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(chart.Values["customRuntime"])
	if err != nil {
		t.Fatal(err)
	}
	var defaults CustomRuntimeConfig
	if err := json.Unmarshal(raw, &defaults); err != nil {
		t.Fatal(err)
	}
	if !defaults.Enabled || defaults.StorageClass != "default" || defaults.StorageSize != "100Gi" || defaults.AccessMode != "ReadWriteOnce" {
		t.Fatalf("unexpected Custom HOME defaults: %+v", defaults)
	}
	defaults.ApprovedImages = p.config.ApprovedImages
	p = NewKubernetesCustomBindings(p.reconciler, defaults)
	ctx := context.Background()
	if _, err := p.Resolve(ctx, instance, nil); err == nil || !strings.Contains(err.Error(), "ownership recorded") {
		t.Fatalf("first resolve must persist ownership before approving HOME: %v", err)
	}
	if err := p.reconciler.Get(ctx, client.ObjectKeyFromObject(instance), instance); err != nil {
		t.Fatal(err)
	}
	return p, instance
}

func TestKubernetesCustomHomeIsCreatedByChart(t *testing.T) {
	p, instance := pendingHomeProvider(t)
	ctx := context.Background()
	b, err := p.Resolve(ctx, instance, nil)
	if err != nil {
		t.Fatal(err)
	}
	bindProviderTestModel(t, p, instance, &b)
	if bound, err := p.reconciler.validateCustomObjects(ctx, instance, b); err != nil || bound {
		t.Fatalf("owned pending HOME should allow a consumer, but not report bound: %v %v", bound, err)
	}
	claims := &corev1.PersistentVolumeClaimList{}
	if err := p.reconciler.List(ctx, claims); err != nil || len(claims.Items) != 0 {
		t.Fatal("operator pre-created a HOME PVC")
	}
	if b.Home.Claim == nil ||
		b.Home.Claim.StorageClass != "default" || b.Home.Claim.StorageSize != "100Gi" || b.Home.Claim.AccessMode != "ReadWriteOnce" {
		t.Fatalf("unexpected HOME plan: %+v", b.Home)
	}
	home := simulateChartHome(t, p, instance, b, false)
	if ready, err := p.reconciler.validateCustomObjects(ctx, instance, b); err != nil || ready {
		t.Fatalf("pending HOME was rejected or marked ready: %v %v", ready, err)
	}
	home.Spec.VolumeName = "synthetic-disk"
	if err := p.reconciler.Update(ctx, home); err != nil {
		t.Fatal(err)
	}
	home.Status.Phase = corev1.ClaimBound
	if err := p.reconciler.Status().Update(ctx, home); err != nil {
		t.Fatal(err)
	}
	bound, err := p.Resolve(ctx, instance, nil)
	if err != nil {
		t.Fatal(err)
	}
	bound.Model = b.Model
	if !reflect.DeepEqual(bound.Home, b.Home) || bound.Context.ObjectReference != b.Context.ObjectReference {
		t.Fatal("CSI binding replaced HOME or context")
	}
	if ready, err := p.reconciler.validateCustomObjects(ctx, instance, bound); err != nil || !ready {
		t.Fatalf("bound HOME was not ready: %v %v", ready, err)
	}
	if ready, err := p.reconciler.validateCustomObjects(ctx, instance, b); err != nil || !ready {
		t.Fatal("PVC binding should not invalidate the name-based plan")
	}
}

func TestKubernetesCustomRejectsDifferentStorageProfile(t *testing.T) {
	p, instance := readyProvider(t)
	ctx := context.Background()
	before, err := p.Resolve(ctx, instance, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := p.config
	cfg.StorageClass, cfg.StorageSize, cfg.AccessMode = "default", "100Gi", "ReadWriteOnce"
	updated := NewKubernetesCustomBindings(p.reconciler, cfg)
	if _, err := updated.Resolve(ctx, instance, nil); err == nil {
		t.Fatal("HOME with a different storage profile was accepted")
	}
	home := &corev1.PersistentVolumeClaim{}
	if err := p.reconciler.Get(ctx, types.NamespacedName{Namespace: before.Namespace.Name, Name: before.Home.Name}, home); err != nil {
		t.Fatal(err)
	}
	if !before.Home.Claim.Matches(home) {
		t.Fatal("rejected storage profile modified HOME")
	}
	if err := p.reconciler.Delete(ctx, home); err != nil {
		t.Fatal(err)
	}
	if _, err := updated.Resolve(ctx, instance, nil); err == nil {
		t.Fatal("missing HOME permitted an outdated storage profile")
	}
	if err := p.reconciler.Get(ctx, client.ObjectKeyFromObject(home), home); !apierrors.IsNotFound(err) {
		t.Fatal("rejected storage profile created a replacement PVC")
	}
}

func TestCustomHomeRejectsForeignOrChangedStorage(t *testing.T) {
	for _, name := range []string{"wrong class", "foreign annotation", "owner reference", "lost", "deleting"} {
		t.Run(name, func(t *testing.T) {
			p, instance := pendingHomeProvider(t)
			ctx := context.Background()
			b, err := p.Resolve(ctx, instance, nil)
			if err != nil {
				t.Fatal(err)
			}
			bindProviderTestModel(t, p, instance, &b)
			home := simulateChartHome(t, p, instance, b, false)
			switch name {
			case "wrong class":
				home.Spec.StorageClassName = ptr.To("missing")
			case "foreign annotation":
				home.Annotations[customAgentUID] = "another-agent"
			case "owner reference":
				home.OwnerReferences = []metav1.OwnerReference{{Kind: "Agent", UID: "another-agent"}}
			case "lost":
				home.Status.Phase = corev1.ClaimLost
			case "deleting":
				home.Finalizers = []string{"test/retain"}
			}
			if name == "lost" {
				if err := p.reconciler.Status().Update(ctx, home); err != nil {
					t.Fatal(err)
				}
			} else if err := p.reconciler.Update(ctx, home); err != nil {
				t.Fatal(err)
			}
			if name == "deleting" {
				if err := p.reconciler.Delete(ctx, home); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := p.reconciler.validateCustomObjects(ctx, instance, b); err == nil {
				t.Fatal("unverified HOME was approved")
			}
		})
	}
}

func TestKubernetesCustomWaitsForObservedMeshLabels(t *testing.T) {
	p, instance := providerFixture(t)
	p.reconciler.Client = namespaceLagClient{Client: p.reconciler.Client}
	if _, err := p.Resolve(context.Background(), instance, nil); err == nil || !strings.Contains(err.Error(), "mesh preparation") {
		t.Fatalf("unobserved namespace mesh settings were accepted: %v", err)
	}
	claims := &corev1.PersistentVolumeClaimList{}
	if err := p.reconciler.List(context.Background(), claims); err != nil || len(claims.Items) != 0 {
		t.Fatal("provisioning advanced before namespace setup was observed")
	}
}

func TestKubernetesCustomRejectsUnapprovedAndForeignResources(t *testing.T) {
	p, instance := providerFixture(t)
	instance.Spec.Agent.Image = ptr.To("registry.example.com/other@sha256:" + strings.Repeat("b", 64))
	if _, err := p.Resolve(context.Background(), instance, nil); err == nil {
		t.Fatal("unapproved image accepted")
	}
	namespaces := &corev1.NamespaceList{}
	_ = p.reconciler.List(context.Background(), namespaces)
	if len(namespaces.Items) != 0 {
		t.Fatal("unapproved image created a namespace")
	}
	p, instance = providerFixture(t)
	foreign := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: instanceNamespace(instance.Name)}}
	if err := p.reconciler.Create(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Resolve(context.Background(), instance, nil); err == nil {
		t.Fatal("foreign namespace adopted")
	}
}

func TestKubernetesCustomLeavesMissingChartHomeToStatefulSet(t *testing.T) {
	p, instance := readyProvider(t)
	ctx := context.Background()
	receipt, err := p.reconciler.readCustomOwnership(ctx, instance)
	if err != nil {
		t.Fatal(err)
	}
	home := &corev1.PersistentVolumeClaim{}
	key := types.NamespacedName{Namespace: receipt.Namespace, Name: receipt.Home}
	if err := p.reconciler.Get(ctx, key, home); err != nil {
		t.Fatal(err)
	}
	if err := p.reconciler.Delete(ctx, home); err != nil {
		t.Fatal(err)
	}
	if bindings, err := p.Resolve(ctx, instance, nil); err != nil || bindings.Home.Claim == nil {
		t.Fatalf("missing chart-owned HOME lost its claim template: %v", err)
	}
	claims := &corev1.PersistentVolumeClaimList{}
	_ = p.reconciler.List(ctx, claims)
	if len(claims.Items) != 0 {
		t.Fatal("recovery allocated a replacement claim")
	}
}

func TestKubernetesCustomHomeIgnoresPVCUID(t *testing.T) {
	p, instance := readyProvider(t)
	ctx := context.Background()
	first, err := p.Resolve(ctx, instance, nil)
	if err != nil {
		t.Fatal(err)
	}
	home := &corev1.PersistentVolumeClaim{}
	if err := p.reconciler.Get(ctx, types.NamespacedName{Namespace: first.Namespace.Name, Name: first.Home.Name}, home); err != nil {
		t.Fatal(err)
	}
	if err := p.reconciler.Delete(ctx, home); err != nil {
		t.Fatal(err)
	}
	home.UID, home.ResourceVersion = "replacement-home", ""
	if err := p.reconciler.Create(ctx, home); err != nil {
		t.Fatal(err)
	}
	second, err := p.Resolve(ctx, instance, nil)
	if err != nil {
		t.Fatalf("same-name PVC was rejected by its new UID: %v", err)
	}
	bindProviderTestModel(t, p, instance, &first)
	if bound, err := p.reconciler.validateCustomObjects(ctx, instance, first); err != nil || !bound {
		t.Fatalf("PVC UID is still enforced: %v", err)
	}
	if !reflect.DeepEqual(second.Home, first.Home) {
		t.Fatal("PVC UID or resourceVersion changed the HOME binding")
	}
}

func TestKubernetesCustomRejectsOwnershipWithoutClaim(t *testing.T) {
	p, instance := providerFixture(t)
	receipt := customOwnership{AgentName: instance.Name, AgentUID: instance.UID,
		Namespace: instanceNamespace(instance.Name), NamespaceUID: "namespace-uid",
		Release: helmReleaseName(instance.Name), Home: "data-" + instanceServiceName(instance.Name) + "-0"}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	key := p.reconciler.ownershipKey(instance)
	record := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace,
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(instance, api.GroupVersion.WithKind("Agent"))}},
		Immutable: ptr.To(true), Data: map[string]string{"ownership": string(raw)}}
	if err := p.reconciler.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Resolve(context.Background(), instance, nil); err == nil {
		t.Fatal("ownership without a HOME claim template was accepted")
	}
}

func TestKubernetesCustomLeasePersistsAcrossRestart(t *testing.T) {
	p, instance := readyProvider(t)
	ctx := context.Background()
	b, _ := p.Resolve(ctx, instance, nil)
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: instanceServiceName(instance.Name), Namespace: b.Namespace.Name,
		Annotations:     map[string]string{customAgentUID: string(instance.UID)},
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(instance, api.GroupVersion.WithKind("Agent"))}},
		Spec: appsv1.StatefulSetSpec{Replicas: ptr.To[int32](1), Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "cetirun", Image: *instance.Spec.Agent.Image}},
		}}}}
	if err := p.reconciler.Create(ctx, sts); err != nil {
		t.Fatal(err)
	}
	op := CustomApplyOperation{ID: "one", PlanKey: "plan:manifest", Generation: instance.Generation}
	lease, err := p.ApproveApply(ctx, instance, sts, op)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.reconciler.Get(ctx, client.ObjectKeyFromObject(sts), sts); err != nil || *sts.Spec.Replicas != 0 {
		t.Fatal("provider did not stop the old writer")
	}
	other := op
	other.ID = "two"
	if _, err := p.ApproveApply(ctx, instance, sts, other); err == nil {
		t.Fatal("concurrent operation acquired the same HOME")
	}
	restarted := NewKubernetesCustomBindings(p.reconciler, p.config)
	resumed, err := restarted.ResumeApply(ctx, instance, op)
	if err != nil {
		t.Fatal(err)
	}
	if err := resumed.Complete(ctx, CustomApplyUnknown); err == nil {
		t.Fatal("unknown apply result was acknowledged")
	}
	if err := resumed.Complete(ctx, CustomApplySucceeded); err != nil {
		t.Fatal(err)
	}
	if err := resumed.Complete(ctx, CustomApplySucceeded); err != nil {
		t.Fatal("completion was not idempotent")
	}
	if err := lease.Check(ctx); err == nil {
		t.Fatal("completed lease remained valid")
	}
}

func TestKubernetesCustomImageUpgradeRetainsHome(t *testing.T) {
	for _, name := range []string{"unapproved", "approved"} {
		t.Run(name, func(t *testing.T) {
			p, instance := readyProvider(t)
			ctx := context.Background()
			bindings, err := p.Resolve(ctx, instance, nil)
			if err != nil {
				t.Fatal(err)
			}
			home := &corev1.PersistentVolumeClaim{}
			homeKey := client.ObjectKey{Namespace: bindings.Namespace.Name, Name: bindings.Home.Name}
			if err := p.reconciler.Get(ctx, homeKey, home); err != nil {
				t.Fatal(err)
			}
			originalHome := home.DeepCopy()
			oldImage := *instance.Spec.Agent.Image
			workload := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
				Name: instanceServiceName(instance.Name), Namespace: bindings.Namespace.Name,
				Annotations:     map[string]string{customAgentUID: string(instance.UID)},
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(instance, api.GroupVersion.WithKind("Agent"))},
			}, Spec: appsv1.StatefulSetSpec{Replicas: ptr.To[int32](1),
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "cetirun", Image: oldImage}},
				}},
			}}
			if err := p.reconciler.Create(ctx, workload); err != nil {
				t.Fatal(err)
			}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: workload.Name + "-0", Namespace: workload.Namespace},
				Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: home.Name},
				}}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
			if err := p.reconciler.Create(ctx, pod); err != nil {
				t.Fatal(err)
			}
			newImage := "registry.example.com/pi@sha256:" + strings.Repeat("b", 64)
			instance.Spec.Agent.Image, instance.Generation = ptr.To(newImage), instance.Generation+1
			if err := p.reconciler.Update(ctx, instance); err != nil {
				t.Fatal(err)
			}
			if name == "approved" {
				cfg := p.config
				cfg.ApprovedImages = []string{oldImage, newImage}
				p = NewKubernetesCustomBindings(p.reconciler, cfg)
			}
			operation := CustomApplyOperation{ID: "image-upgrade", PlanKey: "new-image:manifest", Generation: instance.Generation}
			lease, err := p.ApproveApply(ctx, instance, workload, operation)
			if lease != nil || err == nil {
				t.Fatal("image upgrade was approved before the old Pod exited")
			}
			if err := p.reconciler.Get(ctx, client.ObjectKeyFromObject(workload), workload); err != nil {
				t.Fatal(err)
			}
			if name == "unapproved" {
				if *workload.Spec.Replicas != 1 {
					t.Fatal("unapproved image stopped the running workload")
				}
				return
			}
			if *workload.Spec.Replicas != 0 || workload.Spec.Template.Spec.Containers[0].Image != oldImage {
				t.Fatal("approved upgrade did not stop the old workload before changing its image")
			}
			if err := p.reconciler.Delete(ctx, pod); err != nil {
				t.Fatal(err)
			}
			lease, err = p.ApproveApply(ctx, instance, workload, operation)
			if err != nil || lease == nil {
				t.Fatalf("approved image upgrade remained blocked after Pod exit: %v", err)
			}
			if err := lease.Check(ctx); err != nil {
				t.Fatal(err)
			}
			next, err := p.Resolve(ctx, instance, nil)
			if err != nil || next.ApprovedImage != newImage || !reflect.DeepEqual(next.Home, bindings.Home) {
				t.Fatalf("image upgrade changed the HOME binding: %v", err)
			}
			if err := p.reconciler.Get(ctx, homeKey, home); err != nil || !reflect.DeepEqual(home, originalHome) {
				t.Fatalf("image upgrade changed the retained PVC: %v", err)
			}
			if err := lease.Complete(ctx, CustomApplySucceeded); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCustomRuntimeDefaultsRequireImageApproval(t *testing.T) {
	chart, err := loader.Load("../../chart")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(chart.Values["customRuntime"])
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseCustomRuntimeConfig(string(raw))
	if err != nil || !cfg.Enabled || len(cfg.ApprovedImages) != 0 {
		t.Fatalf("default Custom provider must start without image approvals: %v", err)
	}
	p, instance := providerFixture(t)
	p = NewKubernetesCustomBindings(p.reconciler, cfg)
	if _, err := p.Resolve(context.Background(), instance, nil); err == nil || !strings.Contains(err.Error(), "not approved") {
		t.Fatalf("unapproved image was not rejected: %v", err)
	}
	if p.UsesAIGateway(instance) {
		t.Fatal("default configuration authorized model access for an unapproved image")
	}
	namespaces := &corev1.NamespaceList{}
	if err := p.reconciler.List(context.Background(), namespaces); err != nil || len(namespaces.Items) != 0 {
		t.Fatalf("unapproved image created a namespace: %v", err)
	}
	cfg.Enabled = false
	cfg.ApprovedImages = []string{*instance.Spec.Agent.Image}
	p = NewKubernetesCustomBindings(p.reconciler, cfg)
	if err := p.authorize(instance); err == nil || p.UsesAIGateway(instance) {
		t.Fatal("explicitly disabled provider authorized an approved image")
	}
}

func TestCustomRuntimeConfigRequiresExplicitApproval(t *testing.T) {
	p, _ := providerFixture(t)
	for _, edit := range []func(*CustomRuntimeConfig){
		func(c *CustomRuntimeConfig) { c.ApprovedImages = []string{"registry.example.com/pi:latest"} },
		func(c *CustomRuntimeConfig) { c.ApprovedImages = slices.Repeat(c.ApprovedImages, 65) },
		func(c *CustomRuntimeConfig) { c.FSGroup = 0 },
		func(c *CustomRuntimeConfig) { c.StorageSize = "0" },
		func(c *CustomRuntimeConfig) { c.AccessMode = "ReadOnlyMany" },
		func(c *CustomRuntimeConfig) { c.Context["../private"] = "bad" },
	} {
		data, _ := json.Marshal(p.config)
		var cfg CustomRuntimeConfig
		_ = json.Unmarshal(data, &cfg)
		edit(&cfg)
		data, _ = json.Marshal(cfg)
		if _, err := ParseCustomRuntimeConfig(string(data)); err == nil {
			t.Fatal("invalid provisioning configuration accepted")
		}
	}
}

// Storage-provider tests use a synthetic Gateway Secret; management API behavior
// is covered independently in custom_gateway_test.go.
func bindProviderTestModel(t *testing.T, p *KubernetesCustomBindings, instance *api.Agent, b *runtimebindings.Custom) {
	t.Helper()
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: aiGatewayKeySecretName, Namespace: b.Namespace.Name,
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(instance, api.GroupVersion.WithKind("Agent"))}},
		Type: corev1.SecretTypeOpaque, Data: map[string][]byte{aiGatewayKeySecretField: []byte("sk-dummy")}}
	if err := p.reconciler.Create(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	b.Model = runtimebindings.ModelAccess{ObjectReference: objectReference(secret), Key: aiGatewayKeySecretField, BaseURL: testAIGatewayURL + "/v1"}
}
