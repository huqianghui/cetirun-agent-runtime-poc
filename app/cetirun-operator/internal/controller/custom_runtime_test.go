// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/controller"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"github.com/Azure/CetiRun/app/internal/agenttools"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"
)

type customProvider struct {
	bindings  runtimebindings.Custom
	applyErr  error
	applied   bool
	approve   func(*appsv1.StatefulSet) error
	leaseErr  error
	completed []controller.CustomApplyOutcome
}

type provisioningClient struct{ client.Client }

func (c provisioningClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if obj.GetUID() == "" {
		obj.SetUID(types.UID("synthetic-" + obj.GetName()))
	}
	return c.Client.Create(ctx, obj, opts...)
}

func TestCustomReconcileCreatesHomeInOneChartCall(t *testing.T) {
	requireLegacyCustomRuntime(t)
	ctx := context.Background()
	cr, _, _ := customFixture("disk-agent", nil)
	scheme := testScheme()
	sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Provisioner: "disk.csi.azure.com", VolumeBindingMode: ptr.To(storagev1.VolumeBindingWaitForFirstConsumer)}
	c := provisioningClient{fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&api.Agent{}, &corev1.PersistentVolumeClaim{}, &appsv1.StatefulSet{}).
		WithObjects(cr, sc).Build()}
	hc := &fakeHelmClient{useHarness: true}
	r := &controller.AgentReconciler{Client: c, APIReader: c, Scheme: scheme, HelmClient: hc,
		OperatorNamespace: "cetirun-system", OperatorSAName: "operator-test", IstioRevision: "asm-test",
		EventRecorder: record.NewFakeRecorder(100)}
	controller.ConfigureTestGateway(t, r)
	config := controller.CustomRuntimeConfig{
		Enabled: true, ApprovedImages: []string{*cr.Spec.Agent.Image},
		StorageClass: "default", StorageSize: "100Gi", AccessMode: "ReadWriteOnce", FSGroup: 1024,
		Context: map[string]string{"AGENTS.md": "approved context", "skills/cetirun-memory/SKILL.md": "approved memory"},
	}
	r.CustomBindings = controller.NewKubernetesCustomBindings(r, config)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name}}
	reconcile := func(allowConflict bool) {
		t.Helper()
		if _, err := r.Reconcile(ctx, request); err != nil && !(allowConflict && apierrors.IsConflict(err)) {
			t.Fatal(err)
		}
		if err := c.Get(ctx, request.NamespacedName, cr); err != nil {
			t.Fatal(err)
		}
	}
	reconcile(true)
	if hc.installCalled {
		t.Fatal("workload applied before the ownership receipt was observed")
	}
	oldWriter := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "old-writer", Namespace: "cetirun-disk-agent"},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-openclaw-disk-agent-cetirun-0"},
		}}}}}
	if err := c.Create(ctx, oldWriter); err != nil {
		t.Fatal(err)
	}
	reconcile(false)
	if hc.installCalled {
		t.Fatal("pending HOME bypassed old-writer shutdown")
	}
	if err := c.Delete(ctx, oldWriter); err != nil {
		t.Fatal(err)
	}
	reconcile(false)
	if !hc.installCalled || cr.Status.Phase != api.PhaseProvisioning {
		t.Fatalf("pending HOME did not reach consumer creation: applied=%v status=%+v", hc.installCalled, cr.Status)
	}
	home := &corev1.PersistentVolumeClaim{}
	homeKey := types.NamespacedName{Namespace: "cetirun-disk-agent", Name: "data-openclaw-disk-agent-cetirun-0"}
	if err := c.Get(ctx, homeKey, home); !apierrors.IsNotFound(err) {
		t.Fatalf("operator created a PVC before the StatefulSet controller: %v", err)
	}
	workloadHash := hc.lastValues["runtime"].(map[string]any)["workloadHash"].(string)
	chart, err := loader.Load("../../../cetirun-chart")
	if err != nil {
		t.Fatal(err)
	}
	renderWorkload := func() *appsv1.StatefulSet {
		t.Helper()
		values, err := chartutil.ToRenderValues(chart, hc.lastValues, chartutil.ReleaseOptions{
			Name: "openclaw-disk-agent", Namespace: homeKey.Namespace, IsInstall: true,
		}, chartutil.DefaultCapabilities)
		if err != nil {
			t.Fatal(err)
		}
		rendered, err := engine.Render(chart, values)
		if err != nil {
			t.Fatal(err)
		}
		workload := &appsv1.StatefulSet{}
		if err := yaml.Unmarshal([]byte(strings.Split(rendered["cetirun/templates/custom-runtime.yaml"], "\n---")[0]), workload); err != nil {
			t.Fatal(err)
		}
		workload.Namespace = homeKey.Namespace
		return workload
	}
	sts := renderWorkload()
	if len(sts.Spec.VolumeClaimTemplates) != 1 || sts.Spec.VolumeClaimTemplates[0].Name != "data" {
		t.Fatal("Chart did not declare exactly one HOME claim template")
	}
	if err := c.Create(ctx, sts); err != nil {
		t.Fatal(err)
	}
	sts.Status = appsv1.StatefulSetStatus{ObservedGeneration: sts.Generation, ReadyReplicas: 1, UpdatedReplicas: 1}
	if err := c.Status().Update(ctx, sts); err != nil {
		t.Fatal(err)
	}
	// Simulate StatefulSet/CSI controllers; the Operator must never create this PVC.
	home = sts.Spec.VolumeClaimTemplates[0].DeepCopy()
	home.Name, home.Namespace = "data-"+sts.Name+"-0", sts.Namespace
	if err := c.Create(ctx, home); err != nil {
		t.Fatal(err)
	}
	homeUID := home.UID
	hc.installCalled = false
	reconcile(false)
	if hc.installCalled || cr.Status.Phase == api.PhaseReady {
		t.Fatal("pending HOME was reapplied or reported Ready")
	}
	home.Spec.VolumeName = "synthetic-disk"
	if err := c.Update(ctx, home); err != nil {
		t.Fatal(err)
	}
	home.Status.Phase = corev1.ClaimBound
	if err := c.Status().Update(ctx, home); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "consumer", Namespace: home.Namespace},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: home.Name},
		}}}}}
	if err := c.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	reconcile(false)
	if cr.Status.Phase != api.PhaseReady {
		t.Fatalf("bound HOME did not become ready: %+v", cr.Status)
	}
	if hc.lastValues["runtime"].(map[string]any)["workloadHash"] != workloadHash {
		t.Fatal("CSI binding changed the workload plan")
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(sts), sts); err != nil || *sts.Spec.Replicas != 1 {
		t.Fatal("CSI binding stopped the first consumer")
	}
	if err := c.Get(ctx, homeKey, home); err != nil || home.UID != homeUID || len(home.OwnerReferences) != 0 {
		t.Fatal("CSI binding replaced or re-owned HOME")
	}
	hc.installCalled = false
	reconcile(false)
	if hc.installCalled || hc.installCalls != 1 {
		t.Fatalf("initial provisioning must use one Chart call, got %d", hc.installCalls)
	}
	// An existing Ready ordinary Custom Agent must gain and repair Broker record
	// storage permissions without another Helm apply. Its namespace intentionally
	// has no Agent OwnerReference, so the Broker uses the immutable receipt.
	r.ConnectionsMCPBrokerNamespace = "cetirun-credentials"
	r.ConnectionsMCPBrokerServiceAccount = "cetirun-agent-broker"
	reconcile(false)
	secretRole := &rbacv1.Role{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: homeKey.Namespace, Name: "cetirun-agent-tools-broker"}, secretRole); err != nil {
		t.Fatalf("Ready Custom Agent did not receive Broker Secret Role: %v", err)
	}
	receiptRole := &rbacv1.Role{}
	receiptRoleName := "cetirun-custom-ownership-" + strings.TrimPrefix(agenttools.CustomOwnershipRecordName(string(cr.UID)), "custom-ownership-")
	if err := c.Get(ctx, types.NamespacedName{Namespace: "cetirun-system", Name: receiptRoleName}, receiptRole); err != nil {
		t.Fatalf("Ready Custom Agent did not receive receipt read Role: %v", err)
	}
	brokerRBACCalls := hc.installCalls
	if err := c.Delete(ctx, receiptRole); err != nil {
		t.Fatal(err)
	}
	reconcile(false)
	if err := c.Get(ctx, types.NamespacedName{Namespace: "cetirun-system", Name: receiptRoleName}, receiptRole); err != nil {
		t.Fatalf("Custom receipt Role drift was not repaired: %v", err)
	}
	if hc.installCalls != brokerRBACCalls {
		t.Fatalf("Broker RBAC repair reapplied the Chart: %d -> %d", brokerRBACCalls, hc.installCalls)
	}

	originalHome := home.DeepCopy()
	originalClaims := sts.DeepCopy().Spec.VolumeClaimTemplates
	originalUID := sts.UID
	newImage := "registry.example.com/agent@sha256:" + strings.Repeat("b", 64)
	config.ApprovedImages = append(config.ApprovedImages, newImage)
	r.CustomBindings = controller.NewKubernetesCustomBindings(r, config)
	cr.Spec.Agent.Image, cr.Generation = ptr.To(newImage), cr.Generation+1
	if err := c.Update(ctx, cr); err != nil {
		t.Fatal(err)
	}
	reconcile(false)
	if hc.installCalls != 1 {
		t.Fatal("image upgrade applied before the old HOME consumer exited")
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(sts), sts); err != nil || *sts.Spec.Replicas != 0 {
		t.Fatalf("image upgrade did not stop the old StatefulSet: %v", err)
	}
	if err := c.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	reconcile(false)
	if hc.installCalls != 2 || cr.Status.Phase != api.PhaseProvisioning {
		t.Fatal("image upgrade was not applied after old Pod exit")
	}
	next := renderWorkload()
	if next.Spec.Template.Spec.Containers[0].Image != newImage || next.Spec.Template.Spec.InitContainers[0].Image != newImage ||
		*next.Spec.Replicas != 1 || !reflect.DeepEqual(next.Spec.VolumeClaimTemplates, originalClaims) ||
		next.Annotations["cetirun.microsoft.com/custom-plan-hash"] == workloadHash {
		t.Fatal("image upgrade did not update both containers while preserving the HOME claim template")
	}
	// Simulate the applied Helm template and StatefulSet/CSI observations.
	if err := c.Get(ctx, client.ObjectKeyFromObject(sts), sts); err != nil {
		t.Fatal(err)
	}
	sts.Spec, sts.Annotations = next.Spec, next.Annotations
	if err := c.Update(ctx, sts); err != nil {
		t.Fatal(err)
	}
	sts.Status = appsv1.StatefulSetStatus{ObservedGeneration: sts.Generation, ReadyReplicas: 1, UpdatedReplicas: 1}
	if err := c.Status().Update(ctx, sts); err != nil {
		t.Fatal(err)
	}
	pod.UID, pod.ResourceVersion = "replacement-consumer", ""
	pod.Spec = *next.Spec.Template.Spec.DeepCopy()
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: "data", VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: home.Name},
	}})
	if err := c.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		reconcile(false)
	}
	if hc.installCalls != 2 || cr.Status.Phase != api.PhaseReady || cr.Status.ObservedGeneration != cr.Generation {
		t.Fatal("image upgrade did not settle without repeated applications")
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(sts), sts); err != nil || sts.UID != originalUID {
		t.Fatalf("image upgrade replaced the StatefulSet identity: %v", err)
	}
	if err := c.Get(ctx, homeKey, home); err != nil || !reflect.DeepEqual(home, originalHome) {
		t.Fatalf("image upgrade changed the existing HOME: %v", err)
	}
}

func (p *customProvider) Resolve(context.Context, *api.Agent, *api.Team) (runtimebindings.Custom, error) {
	return p.bindings, nil
}

func (p *customProvider) ApproveApply(_ context.Context, _ *api.Agent, current *appsv1.StatefulSet, _ controller.CustomApplyOperation) (controller.CustomApplyLease, error) {
	p.applied = true
	if p.approve != nil {
		if err := p.approve(current); err != nil {
			return nil, err
		}
	}
	if p.applyErr != nil {
		return nil, p.applyErr
	}
	return p, nil
}

func (p *customProvider) ResumeApply(context.Context, *api.Agent, controller.CustomApplyOperation) (controller.CustomApplyLease, error) {
	return p, nil
}

func (p *customProvider) Check(context.Context) error { return p.leaseErr }

func (p *customProvider) Complete(_ context.Context, outcome controller.CustomApplyOutcome) error {
	p.completed = append(p.completed, outcome)
	return nil
}

func customFixture(name string, port *int32) (*api.Agent, runtimebindings.Custom, []client.Object) {
	cr := newCR(name, api.PhasePending, nil, 1, 0, "")
	cr.UID = types.UID(name + "-agent-uid")
	cr.Spec.User.Alias = "same-user"
	image := "registry.example.com/agent@sha256:" + strings.Repeat("a", 64)
	cr.Spec.Agent = api.HarnessSpec{Type: api.AgentTypeCustom, Image: ptr.To(image), Username: ptr.To("worker"), Port: port}
	cr.Spec.GatewayTokenSecretRef = ""
	cr.Spec.Helm.Values = map[string]runtime.RawExtension{"ttyd": {Raw: []byte(`{"enabled":false}`)}}
	ref := func(name string) runtimebindings.ObjectReference {
		return runtimebindings.ObjectReference{Name: name, UID: name + "-uid", Revision: "1"}
	}
	b := runtimebindings.Custom{
		AgentName: name, AgentUID: string(cr.UID), Generation: 1, Namespace: ref("cetirun-" + name),
		ApprovedType: "custom", ApprovedImage: image, PolicyRevision: "policy-1",
		Model: runtimebindings.ModelAccess{ObjectReference: ref(name + "-model"), Key: "api-key", BaseURL: "http://gateway.models.svc/v1"},
		Home: runtimebindings.Home{Name: "data-openclaw-" + name + "-cetirun-0", FSGroup: 2000,
			Claim: &runtimebindings.HomeClaim{StorageClass: "default", StorageSize: "100Gi", AccessMode: "ReadWriteOnce"}},
		Context: runtimebindings.Context{ObjectReference: ref(name + "-context"), Files: map[string]string{
			"canonical": "AGENTS.md", "memory": "skills/cetirun-memory/SKILL.md",
		}},
		Network: runtimebindings.Network{Revision: "network-1", GatewayPort: 8080,
			Gateway: runtimebindings.Peer{Namespace: "models", PodLabels: map[string]string{"app": "gateway"}},
			DNS:     runtimebindings.Peer{Namespace: "kube-system", PodLabels: map[string]string{"k8s-app": "kube-dns"}},
		},
	}
	meta := func(ref runtimebindings.ObjectReference) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: ref.Name, Namespace: b.Namespace.Name, UID: types.UID(ref.UID), ResourceVersion: ref.Revision}
	}
	nsMeta := meta(b.Namespace)
	nsMeta.Namespace = ""
	secret := &corev1.Secret{ObjectMeta: meta(b.Model.ObjectReference), Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"api-key": []byte("sk-dummy")}}
	secret.OwnerReferences = []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: cr.Name, UID: cr.UID}}
	home := &corev1.PersistentVolumeClaim{ObjectMeta: meta(ref(b.Home.Name)), Spec: b.Home.Claim.PVCSpec(),
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
	home.Annotations = map[string]string{"cetirun.microsoft.com/agent-uid": string(cr.UID)}
	home.Spec.VolumeName = name + "-volume"
	return cr, b, []client.Object{
		cr, &corev1.Namespace{ObjectMeta: nsMeta}, secret,
		home,
		&corev1.ConfigMap{ObjectMeta: meta(b.Context.ObjectReference), Immutable: ptr.To(true),
			Data: map[string]string{"canonical": "synthetic canonical contract", "memory": "synthetic memory skill"}},
	}
}

func TestCustomReconcileUsesBindingsNotNativeSecrets(t *testing.T) {
	requireLegacyCustomRuntime(t)
	for _, port := range []*int32{nil, ptr.To[int32](8080)} {
		cr, b, objects := customFixture("example", port)
		scheme := testScheme()
		c := provisioningClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.Agent{}).WithObjects(objects...).Build()}
		hc := &fakeHelmClient{useHarness: true}
		provider := &customProvider{bindings: b}
		r := &controller.AgentReconciler{Client: c, Scheme: scheme, HelmClient: hc, CustomBindings: provider, EventRecorder: record.NewFakeRecorder(100)}
		controller.ConfigureTestGateway(t, r)
		key := types.NamespacedName{Name: cr.Name}
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatal(err)
		}
		if !hc.installCalled || !provider.applied {
			t.Fatal("valid Custom plan did not reach approved application")
		}
		current := &api.Agent{}
		if err := c.Get(context.Background(), key, current); err != nil {
			t.Fatal(err)
		}
		if current.Status.AccessURL != "" || current.Spec.Agent.Port == nil != (port == nil) {
			t.Fatal("Custom received a native URL or default port")
		}
		secrets := &corev1.SecretList{}
		if err := c.List(context.Background(), secrets); err != nil || len(secrets.Items) != 2 {
			t.Fatal("Custom did not retain its binding Secret and add the Gateway key")
		}
		raw := fmt.Sprint(hc.lastValues["runtime"])
		if strings.Contains(raw, "sk-dummy") || strings.Contains(raw, "gateway-token") {
			t.Fatal("secret content/native reference leaked into release")
		}
		// Publish only a synthetic workload observation, not a running Pod.
		sts := statefulSet(cr.Name, 1)
		sts.Status.CurrentRevision = "revision-1"
		sts.Status.UpdateRevision = "revision-1"
		sts.OwnerReferences = []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: cr.Name, UID: cr.UID}}
		sts.Annotations = map[string]string{
			"cetirun.microsoft.com/agent-uid":        string(cr.UID),
			"cetirun.microsoft.com/custom-plan-hash": hc.lastValues["runtime"].(map[string]any)["workloadHash"].(string),
		}
		markGatewayCustomWorkload(sts, r.AIGateway.URL)
		if err := c.Create(context.Background(), sts); err != nil {
			t.Fatal(err)
		}
		hc.installCalled = false
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatal(err)
		}
		if hc.installCalled {
			t.Fatal("unchanged Custom plan was reapplied")
		}
		if err := c.Get(context.Background(), key, current); err != nil || current.Status.Phase != api.PhaseReady ||
			current.Status.AccessURL != "" {
			t.Fatal("Custom readiness observation failed")
		}
		steadyVersion := current.ResourceVersion
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatal(err)
		}
		if err := c.Get(context.Background(), key, current); err != nil || current.ResourceVersion != steadyVersion {
			t.Fatal("steady-state status caused another watch event")
		}
		// A Gateway binding rotation must still pass through the controlled
		// replacement gate, preserving the original writer-lifecycle regression.
		secret := &corev1.Secret{}
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: b.Namespace.Name, Name: current.Status.AIGateway.KeySecretName}, secret); err != nil {
			t.Fatal(err)
		}
		if secret.Annotations == nil {
			secret.Annotations = map[string]string{}
		}
		secret.Annotations["example.com/rotation"] = "next-binding-revision"
		if err := c.Update(context.Background(), secret); err != nil {
			t.Fatal(err)
		}
		provider.applyErr = fmt.Errorf("no handoff")
		provider.applied = false
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatal(err)
		}
		if !provider.applied || hc.installCalled {
			t.Fatal("rotation bypassed or did not reach the controlled replacement gate")
		}
		provider.applyErr = nil
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatal(err)
		}
		if hc.installCalled {
			t.Fatal("a replicas=1 workload can recreate its old writer during replacement")
		}
		provider.approve = func(current *appsv1.StatefulSet) error {
			current.Spec.Replicas = ptr.To[int32](0)
			return c.Update(context.Background(), current)
		}
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatal(err)
		}
		if !hc.installCalled {
			t.Fatal("approved stopped workload did not receive the rotated binding")
		}
	}
}

func TestCustomReconcileRejectsUnreadyOrForeignBindings(t *testing.T) {
	requireLegacyCustomRuntime(t)
	cases := []string{"no provider", "wrong UID", "wrong generation", "wrong namespace", "wrong secret owner", "wrong secret revision",
		"missing home", "mutable context", "missing canonical", "old writer", "foreign workload", "registry"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			cr, b, objects := customFixture("example", nil)
			provider := &customProvider{bindings: b}
			switch name {
			case "wrong UID":
				provider.bindings.AgentUID = "another-agent"
			case "wrong generation":
				provider.bindings.Generation++
			case "wrong namespace":
				provider.bindings.Namespace.Name = "cetirun-other"
			case "wrong secret owner":
				objects[2].(*corev1.Secret).OwnerReferences[0].UID = "different-uid"
			case "wrong secret revision":
				provider.bindings.Model.Revision = "2"
			case "missing home":
				provider.bindings.Home.Name = "absent-home"
			case "mutable context":
				objects[4].(*corev1.ConfigMap).Immutable = ptr.To(false)
			case "missing canonical":
				delete(objects[4].(*corev1.ConfigMap).Data, "canonical")
			case "old writer":
				objects = append(objects, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "old-writer", Namespace: b.Namespace.Name},
					Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "home", VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: b.Home.Name}}}}}})
			case "foreign workload":
				objects = append(objects, statefulSet(cr.Name, 1))
			case "registry":
				cr.Spec.Agent.RegistrySecretRef = &api.RegistrySecretRef{Name: "registry-source"}
			}
			scheme := testScheme()
			c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.Agent{}).WithObjects(objects...).Build()
			hc := &fakeHelmClient{useHarness: true}
			r := &controller.AgentReconciler{Client: c, Scheme: scheme, HelmClient: hc, CustomBindings: provider, EventRecorder: record.NewFakeRecorder(100)}
			if name == "no provider" {
				r.CustomBindings = nil
			}
			_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name}})
			if hc.installCalled {
				t.Fatal("unready or unowned bindings reached Helm")
			}
			home := &corev1.PersistentVolumeClaim{}
			if err := c.Get(context.Background(), types.NamespacedName{Namespace: b.Namespace.Name, Name: b.Home.Name}, home); err != nil ||
				string(home.UID) != b.Home.Name+"-uid" || len(home.OwnerReferences) != 0 {
				t.Fatal("rejected plan changed or claimed HOME")
			}
		})
	}
}

func TestCustomSameUserInstancesDoNotShareBindings(t *testing.T) {
	requireLegacyCustomRuntime(t)
	one, b, objects := customFixture("one", nil)
	two, _, second := customFixture("two", nil)
	objects = append(objects, second...)
	provider := &customProvider{bindings: b}
	scheme := testScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.Agent{}).WithObjects(objects...).Build()
	hc := &fakeHelmClient{useHarness: true}
	r := &controller.AgentReconciler{Client: c, Scheme: scheme, HelmClient: hc, CustomBindings: provider, EventRecorder: record.NewFakeRecorder(100)}
	if one.Spec.User.Alias != two.Spec.User.Alias {
		t.Fatal("fixture should use the same user")
	}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: two.Name}})
	if err != nil || hc.installCalled {
		t.Fatal("another instance's binding was accepted")
	}
}

func TestCustomSuspensionWorksWithoutBindingsAndWithInvalidConfig(t *testing.T) {
	requireLegacyCustomRuntime(t)
	cr, b, objects := customFixture("example", nil)
	cr.Spec.Suspended = true
	cr.Spec.Agent.RegistrySecretRef = &api.RegistrySecretRef{Name: "unsupported"}
	sts := statefulSet(cr.Name, 1)
	sts.Annotations = map[string]string{"cetirun.microsoft.com/agent-uid": string(cr.UID)}
	sts.OwnerReferences = []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: cr.Name, UID: cr.UID}}
	objects = append(objects, sts)
	scheme := testScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.Agent{}).WithObjects(objects...).Build()
	hc := &fakeHelmClient{useHarness: true}
	r := &controller.AgentReconciler{Client: c, Scheme: scheme, HelmClient: hc, EventRecorder: record.NewFakeRecorder(100)}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(sts), sts); err != nil || *sts.Spec.Replicas != 0 || hc.installCalled {
		t.Fatal("suspension was gated by Custom runtime configuration")
	}
	home := &corev1.PersistentVolumeClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: b.Namespace.Name, Name: b.Home.Name}, home); err != nil {
		t.Fatal("suspension removed HOME")
	}
}

func TestCustomRendererDriftDoesNotReplaceWorkload(t *testing.T) {
	requireLegacyCustomRuntime(t)
	cr, b, objects := customFixture("example", nil)
	scheme := testScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.Agent{}).WithObjects(objects...).Build()
	hc := &fakeHelmClient{useHarness: true}
	provider := &customProvider{bindings: b}
	r := &controller.AgentReconciler{Client: c, Scheme: scheme, HelmClient: hc, CustomBindings: provider, EventRecorder: record.NewFakeRecorder(100)}
	key := types.NamespacedName{Name: cr.Name}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), key, cr); err != nil {
		t.Fatal(err)
	}
	plan := strings.SplitN(cr.Status.ObservedOperatorHash, ":", 2)[0]
	cr.Status.ObservedOperatorHash = plan + ":previous-renderer"
	if err := c.Status().Update(context.Background(), cr); err != nil {
		t.Fatal(err)
	}
	sts := statefulSet(cr.Name, 1)
	sts.OwnerReferences = []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: cr.Name, UID: cr.UID}}
	sts.Annotations = map[string]string{"cetirun.microsoft.com/agent-uid": string(cr.UID),
		"cetirun.microsoft.com/custom-plan-hash": plan}
	if err := c.Create(context.Background(), sts); err != nil {
		t.Fatal(err)
	}
	hc.installCalled, provider.applied = false, false
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if hc.installCalled || provider.applied {
		t.Fatal("renderer drift triggered replacement")
	}
}
