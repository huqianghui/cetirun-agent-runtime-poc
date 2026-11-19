// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/controller"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func configureBootstrap(t *testing.T, f *preparationCase) {
	t.Helper()
	configureBootstrapSkillsCatalog(t, f, "")
}

func configureBootstrapSkillsCatalog(t *testing.T, f *preparationCase, catalogName string) {
	t.Helper()
	snapshot := f.source.configuration
	data, err := json.Marshal(controller.SandboxPlatformConfiguration{
		Version: "v1", Registrations: snapshot.Registrations, Platforms: snapshot.Platforms,
		RuntimeClasses: snapshot.RuntimeClasses, StorageClasses: snapshot.StorageClasses,
	})
	if err != nil {
		t.Fatal(err)
	}
	name := f.fixture.Agent.Name + "-platform"
	f.h.create(t, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: preparationStateNamespace},
		Data:       map[string]string{controller.SandboxPlatformConfigurationKey: string(data)},
	})
	preparation, err := controller.NewKubernetesSandboxPreparation(controller.SandboxSourcesConfig{
		Enabled: true, BootstrapOwnedPrerequisites: true, PlatformConfigMapName: name,
		HomeClaimName: "home", SkillsClaimName: "skills", Helpers: f.fixture.Input.Helpers,
		SkillsCatalogConfigMapName: catalogName,
		AdmissionServiceAccount:    preparationAdmissionAccount,
	}, f.h.client, f.installer, preparationStateNamespace, f.h.chart.Metadata.Version)
	if err != nil {
		t.Fatal(err)
	}
	f.reconciler.SandboxPreparation = preparation
	f.reconciler.OperatorSAName = "cetirun-operator"
	f.reconciler.ChartRegistry = "registry.example.com"
}

// The API server, identities, admission and resource lifecycle here are real.
// Runtime/storage declarations remain synthetic; no execution or fencing is qualified.
func TestSandboxBootstrap(t *testing.T) {
	h := newPreparationHarness(t)
	for _, agentType := range []string{"openclaw", "hermes"} {
		t.Run(agentType, func(t *testing.T) {
			f := h.newCaseWithPrerequisites(t, agentType, false)
			configureBootstrap(t, f)
			if _, err := f.reconciler.Reconcile(h.ctx, f.request()); !apierrors.IsNotFound(err) {
				t.Fatal("bootstrap should wait for published skills:", err)
			}
			namespace := &corev1.Namespace{}
			h.get(t, client.ObjectKey{Name: f.fixture.Namespace.Name}, namespace)
			if namespace.Labels[w.ProtectedNamespaceLabel] != "true" || !slices.Contains(namespace.Finalizers, sandbox.BootstrapFinalizer) {
				t.Fatal("new namespace is not protected throughout bootstrap")
			}
			home := &corev1.PersistentVolumeClaim{}
			h.get(t, client.ObjectKey{Namespace: namespace.Name, Name: "home"}, home)
			secret := &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}}
			h.get(t, client.ObjectKey{Namespace: namespace.Name, Name: f.fixture.Agent.Spec.GatewayTokenSecretRef}, secret)
			if home.Annotations[w.AgentUIDAnnotation] != string(f.fixture.Agent.UID) ||
				secret.Annotations[w.AgentUIDAnnotation] != string(f.fixture.Agent.UID) {
				t.Fatal("created storage or native credentials lack exact Agent ownership")
			}
			original := home.DeepCopy()
			f.h.create(t, f.fixture.ServiceAccount)
			f.fixture.Input.Skills.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: api.GroupVersion.String(), Kind: "Team", Name: f.fixture.Team.Name, UID: f.fixture.Team.UID,
			}}
			h.create(t, f.fixture.Input.Skills)
			if _, err := f.reconciler.Reconcile(h.ctx, f.request()); !errors.Is(err, sandbox.ErrObservationUnavailable) {
				t.Fatal("real Kubernetes bootstrap bypassed missing writer evidence:", err)
			}
			before := h.webhookCalls.Load()
			probe := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "bootstrap-probe", Namespace: namespace.Name},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: ptr.To(false),
					Containers:                   []corev1.Container{{Name: "probe", Image: "registry.example.com/unused:v1"}},
				},
			}
			if err := wait.PollUntilContextTimeout(h.ctx, 50*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
				err := h.client.Create(ctx, probe.DeepCopy(), client.DryRunAll)
				return err != nil && h.webhookCalls.Load() > before, nil
			}); err != nil {
				t.Fatal("protected namespace did not reject a pre-start Pod through admission:", err)
			}
			h.assertUnselectedNamespace(t)
			preparation := f.reconciler.SandboxPreparation
			f.restart()
			f.reconciler.SandboxPreparation = preparation
			f.reconciler.OperatorSAName = "cetirun-operator"
			f.reconciler.ChartRegistry = "registry.example.com"
			if _, err := f.reconciler.Reconcile(h.ctx, f.request()); !errors.Is(err, sandbox.ErrObservationUnavailable) {
				t.Fatal("restart failed to recover bootstrapped inputs:", err)
			}
			h.get(t, client.ObjectKeyFromObject(home), home)
			if home.UID != original.UID || home.ResourceVersion != original.ResourceVersion {
				t.Fatal("restart replaced or rewrote HOME")
			}
			deleteBootstrapCase(t, f)
		})
	}
}

func bootstrapCaseReceipt(t *testing.T, f *preparationCase) *corev1.ConfigMap {
	t.Helper()
	var state corev1.ConfigMapList
	if err := f.h.client.List(f.h.ctx, &state, client.InNamespace(preparationStateNamespace)); err != nil {
		t.Fatal(err)
	}
	for i := range state.Items {
		if state.Items[i].Annotations[w.AgentUIDAnnotation] == string(f.fixture.Agent.UID) &&
			state.Items[i].Data["bootstrap.json"] != "" {
			return state.Items[i].DeepCopy()
		}
	}
	t.Fatal("bootstrap receipt is missing")
	return nil
}

func deleteBootstrapCase(t *testing.T, f *preparationCase) {
	t.Helper()
	h := f.h
	if err := h.client.Delete(h.ctx, f.agent(t)); err != nil {
		t.Fatal(err)
	}
	f.reconciler.SandboxPreparation = nil
	if _, err := f.reconciler.Reconcile(h.ctx, f.request()); err != nil {
		t.Fatal("bootstrap-only deletion required missing sources or fencing:", err)
	}
	namespace := &corev1.Namespace{}
	h.get(t, client.ObjectKey{Name: f.fixture.Namespace.Name}, namespace)
	if namespace.DeletionTimestamp.IsZero() || slices.Contains(namespace.Finalizers, sandbox.BootstrapFinalizer) {
		t.Fatal("cleanup did not authorize the exact namespace for deletion")
	}
	// envtest has no namespace controller. Complete only its native finalization
	// after the tested reconciler has released its own bootstrap protection.
	namespace.Spec.Finalizers = nil
	if _, err := h.kubernetes.CoreV1().Namespaces().Finalize(h.ctx, namespace, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reconciler.Reconcile(h.ctx, f.request()); err != nil {
		t.Fatal("bootstrap finalizer cleanup did not complete:", err)
	}
	if err := h.client.Get(h.ctx, client.ObjectKeyFromObject(f.fixture.Agent), &api.Agent{}); !apierrors.IsNotFound(err) {
		t.Fatal("Agent still has bootstrap ownership obligations:", err)
	}
}

type bootstrapNamespaceRaceClient struct {
	client.Client
	beforeRole func() error
}

func (c *bootstrapNamespaceRaceClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	if _, ok := object.(*rbacv1.Role); ok && c.beforeRole != nil {
		if err := c.beforeRole(); err != nil {
			return err
		}
		c.beforeRole = nil
	}
	return c.Client.Create(ctx, object, options...)
}

func TestSandboxBootstrapNamespaceLifetime(t *testing.T) {
	h := newPreparationHarness(t)
	f := h.newCaseWithPrerequisites(t, "hermes", false)
	configureBootstrap(t, f)
	var namespace corev1.Namespace
	var attempted bool
	f.reconciler.Client = &bootstrapNamespaceRaceClient{
		Client: h.client,
		beforeRole: func() error {
			attempted = true
			h.get(t, client.ObjectKey{Name: f.fixture.Namespace.Name}, &namespace)
			if err := h.client.Delete(h.ctx, &namespace); err != nil {
				return err
			}
			err := h.client.Create(h.ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace.Name}})
			if !apierrors.IsAlreadyExists(err) {
				t.Fatal("namespace lifetime protection allowed replacement between check and create:", err)
			}
			return nil
		},
	}
	if _, err := f.reconciler.Reconcile(h.ctx, f.request()); err == nil || !attempted {
		t.Fatal("namespace termination did not stop the checked child create:", err)
	}
	current := &corev1.Namespace{}
	h.get(t, client.ObjectKey{Name: namespace.Name}, current)
	if current.UID != namespace.UID || current.DeletionTimestamp.IsZero() ||
		!slices.Contains(current.Finalizers, sandbox.BootstrapFinalizer) {
		t.Fatal("interrupted bootstrap lost namespace identity protection")
	}
	deleteBootstrapCase(t, f)
}

func TestSandboxBootstrapForegroundDeletionRetainsDependents(t *testing.T) {
	h := newPreparationHarness(t)
	for _, agentType := range []string{"openclaw", "hermes"} {
		t.Run(agentType, func(t *testing.T) {
			f := h.newCaseWithPrerequisites(t, agentType, false)
			configureBootstrap(t, f)
			if _, err := f.reconciler.Reconcile(h.ctx, f.request()); !apierrors.IsNotFound(err) {
				t.Fatal(err)
			}
			receipt := bootstrapCaseReceipt(t, f)
			originalData := receipt.DeepCopy().Data
			receipt.Data["bootstrap.json"] = `{"invalid":"synthetic-receipt"}`
			h.update(t, receipt)
			if err := h.client.Delete(h.ctx, f.agent(t), client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(f.agent(t).Finalizers, metav1.FinalizerDeleteDependents) {
				t.Fatal("the API server did not initiate foreground deletion")
			}
			namespace := f.fixture.Namespace.Name
			dependents := []client.Object{
				&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-operator-ns-role", Namespace: namespace}},
				&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-operator-ns-rolebinding", Namespace: namespace}},
				&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "home", Namespace: namespace}},
				&metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
					ObjectMeta: metav1.ObjectMeta{Name: f.fixture.Agent.Spec.GatewayTokenSecretRef, Namespace: namespace}},
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}},
				receipt,
			}
			// envtest has no GC or namespace controller. Send their dependent
			// DELETEs through the real API before the controller can authorize cleanup.
			for _, object := range dependents {
				h.get(t, client.ObjectKeyFromObject(object), object)
				if err := h.client.Delete(h.ctx, object, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.reconciler.Reconcile(h.ctx, f.request()); err == nil {
				t.Fatal("corrupt ownership receipt did not block cleanup")
			}
			for _, object := range dependents {
				uid := object.GetUID()
				h.get(t, client.ObjectKeyFromObject(object), object)
				if object.GetUID() != uid || object.GetDeletionTimestamp().IsZero() ||
					!slices.Contains(object.GetFinalizers(), sandbox.BootstrapFinalizer) {
					t.Fatalf("%T was not retained through its dependent DELETE", object)
				}
			}
			receipt.Data = originalData
			h.update(t, receipt)
			f.reconciler.SandboxPreparation = nil
			if _, err := f.reconciler.Reconcile(h.ctx, f.request()); err != nil {
				t.Fatal("bootstrap-only cleanup did not recover:", err)
			}
			for _, object := range dependents[:4] {
				err := h.client.Get(h.ctx, client.ObjectKeyFromObject(object), object)
				if apierrors.IsNotFound(err) {
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				if slices.Contains(object.GetFinalizers(), sandbox.BootstrapFinalizer) {
					t.Fatalf("%T kept bootstrap protection after cleanup authorization", object)
				}
				claim, ok := object.(*corev1.PersistentVolumeClaim)
				if !ok || !slices.Equal(claim.Finalizers, []string{"kubernetes.io/pvc-protection"}) {
					t.Fatalf("%T has unexpected pending finalization", object)
				}
				// No Pods exist here; explicitly simulate the absent PVC
				// protection controller after checking our finalizer is gone.
				base := claim.DeepCopy()
				claim.Finalizers = nil
				if err := h.client.Patch(h.ctx, claim, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
					t.Fatal(err)
				}
			}
			ns := &corev1.Namespace{}
			h.get(t, client.ObjectKey{Name: namespace}, ns)
			if slices.Contains(ns.Finalizers, sandbox.BootstrapFinalizer) {
				t.Fatal("namespace retained bootstrap protection after dependent release")
			}
			ns.Spec.Finalizers = nil
			if _, err := h.kubernetes.CoreV1().Namespaces().Finalize(h.ctx, ns, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.reconciler.Reconcile(h.ctx, f.request()); err != nil {
				t.Fatal("retained receipt cleanup did not finish:", err)
			}
			if err := h.client.Get(h.ctx, client.ObjectKeyFromObject(receipt), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
				t.Fatal("bootstrap receipt outlived successful cleanup:", err)
			}
			agent := f.agent(t)
			if !slices.Equal(agent.Finalizers, []string{metav1.FinalizerDeleteDependents}) {
				t.Fatal("controller did not release only its own Agent finalizers")
			}
			// Simulate GC completing foreground deletion after its retained
			// dependents are gone; the operator must not remove GC's finalizer.
			base := agent.DeepCopy()
			agent.Finalizers = nil
			if err := h.client.Patch(h.ctx, agent, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				t.Fatal(err)
			}
			if err := h.client.Get(h.ctx, client.ObjectKeyFromObject(agent), &api.Agent{}); !apierrors.IsNotFound(err) {
				t.Fatal("foreground Agent deletion did not finish:", err)
			}
		})
	}
}
