// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/controller"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func newSandboxBootstrapFixture(t *testing.T, typ string, enabled bool) *preparationFixture {
	t.Helper()
	f := newPreparationFixture(t, typ)
	ctx := context.Background()
	for _, object := range []client.Object{
		f.fixture.Input.Home, f.fixture.Input.Skills, f.fixture.ServiceAccount,
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: f.fixture.Agent.Spec.GatewayTokenSecretRef, Namespace: f.fixture.Namespace.Name}},
		f.fixture.Namespace,
	} {
		if err := f.client.Delete(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	agent := f.current(t)
	agent.Spec.Helm.Version = f.installer.chart.Metadata.Version
	agent.Spec.Agent.Image = ptr.To("registry.example.com/" + typ + "-gateway:" + f.installer.chart.Metadata.AppVersion)
	agent.Spec.Agent.Port = ptr.To(int32(18789))
	if typ == "hermes" {
		agent.Spec.Agent.Port = ptr.To(int32(8642))
	}
	if err := f.client.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{
		"enabled": true, "bootstrapOwnedPrerequisites": enabled,
		"platformConfigMapName": "sandbox-platform", "homeClaimName": f.fixture.Input.Home.Name,
		"skillsClaimName": f.fixture.Input.Skills.Name, "admissionServiceAccount": "cetirun-sandbox-admission",
		"helpers": f.fixture.Input.Helpers,
	})
	if err != nil {
		t.Fatal(err)
	}
	config, err := controller.ParseSandboxSourcesConfig(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.PlatformConfigMapName, Namespace: "cetirun-system"},
		Data:       map[string]string{controller.SandboxPlatformConfigurationKey: sandboxSourceDeclaration(t, f.config)},
	}
	if err := f.client.Create(ctx, cm); err != nil {
		t.Fatal(err)
	}
	preparation, err := controller.NewKubernetesSandboxPreparation(config, f.client, f.installer, "cetirun-system", f.installer.chart.Metadata.Version)
	if err != nil {
		t.Fatal(err)
	}
	f.reconciler.SandboxPreparation = preparation
	f.reconciler.ChartRegistry = "registry.example.com"
	return f
}

func bootstrapReceipt(t *testing.T, f *preparationFixture) *corev1.ConfigMap {
	t.Helper()
	var objects corev1.ConfigMapList
	if err := f.client.List(context.Background(), &objects, client.InNamespace("cetirun-system")); err != nil {
		t.Fatal(err)
	}
	for i := range objects.Items {
		if objects.Items[i].Data["bootstrap.json"] != "" {
			return &objects.Items[i]
		}
	}
	t.Fatal("bootstrap receipt not found")
	return nil
}

func assertNoBootstrap(t *testing.T, f *preparationFixture) {
	t.Helper()
	if err := f.client.Get(context.Background(), client.ObjectKeyFromObject(f.fixture.Namespace), &corev1.Namespace{}); !apierrors.IsNotFound(err) {
		t.Fatal("unexpected namespace side effect:", err)
	}
	var state corev1.ConfigMapList
	if err := f.client.List(context.Background(), &state, client.InNamespace("cetirun-system")); err != nil {
		t.Fatal(err)
	}
	if len(state.Items) != 1 || state.Items[0].Name != "sandbox-platform" ||
		slices.Contains(f.current(t).Finalizers, sandbox.BootstrapFinalizer) {
		t.Fatal("bootstrap persisted unexpected ownership obligations")
	}
}

func TestSandboxBootstrapOptInAndIntent(t *testing.T) {
	for _, test := range []struct {
		name    string
		enabled bool
		edit    func(*api.Agent)
	}{
		{name: "disabled"},
		{name: "suspended", enabled: true, edit: func(a *api.Agent) { a.Spec.Suspended = true }},
		{name: "custom", enabled: true, edit: func(a *api.Agent) { a.Spec.Agent.Type = api.AgentTypeCustom }},
		{name: "tailscale", enabled: true, edit: func(a *api.Agent) { a.Spec.Tailscale = &api.TailscaleSpec{} }},
		{name: "missing chart pin", enabled: true, edit: func(a *api.Agent) { a.Spec.Helm.Version = "" }},
		{name: "different chart pin", enabled: true, edit: func(a *api.Agent) { a.Spec.Helm.Version = "999.0.0" }},
		{name: "missing image pin", enabled: true, edit: func(a *api.Agent) { a.Spec.Agent.Image = nil }},
		{name: "missing port pin", enabled: true, edit: func(a *api.Agent) { a.Spec.Agent.Port = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newSandboxBootstrapFixture(t, "openclaw", test.enabled)
			agent := f.current(t)
			if test.edit != nil {
				test.edit(agent)
				if err := f.client.Update(context.Background(), agent); err != nil {
					t.Fatal(err)
				}
			}
			before := agent.Spec.DeepCopy()
			_ = f.reconcile()
			assertNoBootstrap(t, f)
			if !equality.Semantic.DeepEqual(f.current(t).Spec, *before) {
				t.Fatal("bootstrap changed Agent spec without authorized admission")
			}
			if agent.Spec.Suspended && f.current(t).Status.Phase != api.PhaseSuspended {
				t.Fatal("bootstrap lost explicit suspension")
			}
		})
	}
}

func TestSandboxBootstrapExistingNamespaceIsReadOnly(t *testing.T) {
	f := newSandboxBootstrapFixture(t, "openclaw", true)
	ns := f.fixture.Namespace.DeepCopy()
	ns.UID, ns.ResourceVersion = "", ""
	ns.Labels = map[string]string{"example.com/existing": "unchanged"}
	if err := f.client.Create(context.Background(), ns); err != nil {
		t.Fatal(err)
	}
	before := ns.DeepCopy()
	if err := f.reconcile(); err == nil {
		t.Fatal("unprotected existing namespace should still fail prerequisite validation")
	}
	if err := f.client.Get(context.Background(), client.ObjectKeyFromObject(ns), ns); err != nil {
		t.Fatal(err)
	}
	if !equality.Semantic.DeepEqual(before, ns) {
		t.Fatal("bootstrap adopted or modified an existing namespace")
	}
	if slices.Contains(f.current(t).Finalizers, sandbox.BootstrapFinalizer) {
		t.Fatal("bootstrap took ownership of externally supplied inputs")
	}
}

func TestSandboxBootstrapRetriesDoNotReadOrRotateSecrets(t *testing.T) {
	f := newSandboxBootstrapFixture(t, "hermes", true)
	if err := f.reconcile(); !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	var secret corev1.Secret
	key := client.ObjectKey{Namespace: f.fixture.Namespace.Name, Name: f.fixture.Agent.Spec.GatewayTokenSecretRef}
	if err := f.client.Get(context.Background(), key, &secret); err != nil {
		t.Fatal(err)
	}
	receipt := bootstrapReceipt(t, f)
	var secretReads, writes int
	spy := interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, o client.Object, opts ...client.GetOption) error {
			if _, ok := o.(*corev1.Secret); ok {
				secretReads++
				return fmt.Errorf("unexpected Secret contents read")
			}
			return c.Get(ctx, key, o, opts...)
		},
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			writes++
			return fmt.Errorf("unexpected bootstrap create")
		},
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			writes++
			return fmt.Errorf("unexpected bootstrap patch")
		},
		Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
			writes++
			return fmt.Errorf("unexpected bootstrap update")
		},
	})
	f.reconciler.Client, f.reconciler.APIReader = spy, spy
	f.reconciler.SandboxPreparation.Configuration.(*controller.KubernetesSandboxConfigurationSource).APIReader = spy
	f.reconciler.SandboxPreparation.Prerequisites.(*controller.KubernetesSandboxPrerequisites).APIReader = spy
	for range 2 {
		if err := f.reconcile(); !apierrors.IsNotFound(err) {
			t.Fatal("unchanged bootstrap failed for more than the missing skills claim:", err)
		}
	}
	if secretReads != 0 || writes != 0 {
		t.Fatal("retry read credentials or rewrote established ownership")
	}
	after := &corev1.Secret{}
	if err := f.client.Get(context.Background(), key, after); err != nil {
		t.Fatal(err)
	}
	if !equality.Semantic.DeepEqual(&secret, after) || bootstrapReceipt(t, f).ResourceVersion != receipt.ResourceVersion {
		t.Fatal("retry rotated credentials or changed the durable receipt")
	}
}

func bootstrapKind(o client.Object) string {
	switch o.(type) {
	case *corev1.ConfigMap:
		return "receipt"
	case *corev1.Namespace:
		return "namespace"
	case *rbacv1.Role:
		return "role"
	case *rbacv1.RoleBinding:
		return "binding"
	case *corev1.PersistentVolumeClaim:
		return "home"
	case *corev1.Secret:
		return "secret"
	default:
		return ""
	}
}

func TestSandboxBootstrapInterruptedCreates(t *testing.T) {
	for _, kind := range []string{"receipt", "namespace", "role", "binding", "home", "secret"} {
		for _, responseLost := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/response-lost=%t", kind, responseLost), func(t *testing.T) {
				f := newSandboxBootstrapFixture(t, "hermes", true)
				failure := errors.New("synthetic bootstrap response failure")
				var created client.Object
				f.reconciler.Client = interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
					Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
						if bootstrapKind(o) != kind {
							return c.Create(ctx, o, opts...)
						}
						if responseLost {
							if err := c.Create(ctx, o, opts...); err != nil {
								return err
							}
							created = o.DeepCopyObject().(client.Object)
						}
						return failure
					},
				})
				if err := f.reconcile(); !errors.Is(err, failure) {
					t.Fatal("bootstrap did not surface the injected failure:", err)
				}
				f.reconciler.Client = f.client
				if err := f.reconcile(); !apierrors.IsNotFound(err) {
					t.Fatal("bootstrap did not recover to the missing-skills boundary:", err)
				}
				if created != nil {
					actual := created.DeepCopyObject().(client.Object)
					if err := f.client.Get(context.Background(), client.ObjectKeyFromObject(created), actual); err != nil {
						t.Fatal(err)
					}
					if actual.GetUID() != created.GetUID() {
						t.Fatal("lost response caused a recorded create to be replaced")
					}
					if before, ok := created.(*corev1.Secret); ok && !equality.Semantic.DeepEqual(before.Data, actual.(*corev1.Secret).Data) {
						t.Fatal("lost Secret create response caused credential rotation")
					}
				}
				if f.installer.stages != 0 || !slices.Contains(f.current(t).Finalizers, sandbox.BootstrapFinalizer) {
					t.Fatal("interrupted bootstrap advanced to staging or lost cleanup protection")
				}
			})
		}
	}
}

func TestSandboxBootstrapColdCreate(t *testing.T) {
	for _, typ := range []string{"openclaw", "hermes"} {
		t.Run(typ, func(t *testing.T) {
			ctx := context.Background()
			f := newSandboxBootstrapFixture(t, typ, true)
			if err := f.reconcile(); !apierrors.IsNotFound(err) {
				t.Fatal("bootstrap should wait for the externally published skills claim:", err)
			}
			ns := &corev1.Namespace{}
			if err := f.client.Get(ctx, client.ObjectKeyFromObject(f.fixture.Namespace), ns); err != nil {
				t.Fatal("cold create did not provision the protected namespace:", err)
			}
			if ns.Labels[sandboxworkload.ProtectedNamespaceLabel] != "true" ||
				ns.Annotations[sandboxworkload.AgentUIDAnnotation] != string(f.fixture.Agent.UID) ||
				!equality.Semantic.DeepEqual(ns.OwnerReferences, []metav1.OwnerReference{sandboxworkload.AgentOwner(f.fixture.Agent.Name, f.fixture.Agent.UID)}) {
				t.Fatal("bootstrap namespace is not exactly owned and protected")
			}
			home := &corev1.PersistentVolumeClaim{}
			if err := f.client.Get(ctx, client.ObjectKeyFromObject(f.fixture.Input.Home), home); err != nil {
				t.Fatal("cold create did not provision HOME:", err)
			}
			if *home.Spec.StorageClassName != f.fixture.Sandbox.Spec.Storage.Home.StorageClassName ||
				!slices.Equal(home.Spec.AccessModes, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}) ||
				home.Annotations[sandboxworkload.AgentUIDAnnotation] != string(f.fixture.Agent.UID) {
				t.Fatal("HOME does not match the authorized profile")
			}
			secret := &corev1.Secret{}
			if err := f.client.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: f.fixture.Agent.Spec.GatewayTokenSecretRef}, secret); err != nil {
				t.Fatal("cold create did not provision native runtime authentication:", err)
			}
			if len(secret.Data["OPENCLAW_GATEWAY_TOKEN"]) == 0 || len(secret.Data["CETIRUN_AUTH_KEY"]) == 0 ||
				!equality.Semantic.DeepEqual(secret.OwnerReferences, []metav1.OwnerReference{sandboxworkload.AgentOwner(f.fixture.Agent.Name, f.fixture.Agent.UID)}) {
				t.Fatal("native runtime Secret lacks keys or exact ownership")
			}
			if typ == "hermes" && (len(secret.Data["API_SERVER_KEY"]) == 0 || len(secret.Data["hermes.env"]) == 0) {
				t.Fatal("Hermes runtime authentication was not initialized")
			}
			if err := f.client.Get(ctx, client.ObjectKeyFromObject(f.fixture.Input.Skills), &corev1.PersistentVolumeClaim{}); !apierrors.IsNotFound(err) {
				t.Fatal("bootstrap substituted an empty volume for published skills:", err)
			}
			skills := f.fixture.Input.Skills.DeepCopy()
			skills.UID, skills.ResourceVersion = "", ""
			skills.OwnerReferences = []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "Team", Name: f.fixture.Team.Name, UID: f.fixture.Team.UID}}
			if err := f.client.Create(ctx, skills); err != nil {
				t.Fatal(err)
			}
			if err := f.reconcile(); !errors.Is(err, sandbox.ErrObservationUnavailable) {
				t.Fatal("bootstrap did not reach the real missing-fence boundary:", err)
			}
			agent := f.current(t)
			if agent.Status.AccessURL != "" || agent.Status.Sandbox != nil || f.installer.stages != 0 ||
				!slices.Contains(agent.Finalizers, sandbox.HoldFinalizer) || slices.Contains(agent.Finalizers, sandbox.PreparationFinalizer) {
				t.Fatal("bootstrap was mistaken for execution or qualified preparation")
			}
			var workloads appsv1.StatefulSetList
			if err := f.client.List(ctx, &workloads, client.InNamespace(ns.Name)); err != nil || len(workloads.Items) != 0 {
				t.Fatal("bootstrap created a workload:", err)
			}
		})
	}
}
