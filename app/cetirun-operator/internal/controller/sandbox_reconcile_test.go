// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	kubeyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type sharedSandboxInstaller struct {
	*nativeSandboxInstaller
	beforeApply func(*helm.PreparedRelease) error
}

func TestReconciledSandboxSuspendsBeforeBootstrap(t *testing.T) {
	ctx := context.Background()
	f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, "openclaw", true, false)
	agent := f.current(t)
	agent.Spec.Suspended = true
	agent.Generation++
	if err := f.kube.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		f.reconcileOK(t)
	}
	current := f.current(t)
	if current.Status.Phase != api.PhaseSuspended || current.Status.ObservedGeneration != current.Generation ||
		current.Status.AccessURL != "" {
		t.Fatalf("pre-bootstrap suspension did not preserve the stopped lifecycle: %#v", current.Status)
	}
	if f.installer.stages != 0 {
		t.Fatal("pre-bootstrap suspension rendered or applied a workload")
	}
	if err := f.kube.Get(ctx, client.ObjectKey{Name: instanceNamespace(current.Name)}, &corev1.Namespace{}); !apierrors.IsNotFound(err) {
		t.Fatalf("pre-bootstrap suspension created a namespace: %v", err)
	}
	if _, _, err := f.r.readSandboxBootstrap(ctx, current); !apierrors.IsNotFound(err) {
		t.Fatalf("pre-bootstrap suspension created a bootstrap receipt: %v", err)
	}
	current.Spec.Suspended = false
	current.Generation++
	if err := f.kube.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	f.reconcileOK(t)
	if err := f.kube.Get(ctx, client.ObjectKey{Name: instanceNamespace(current.Name)}, &corev1.Namespace{}); err != nil {
		t.Fatalf("resume did not restart normal bootstrap: %v", err)
	}
}

func TestReconciledSandboxSuspensionAfterReceiptBeforeFinalizerStaysPending(t *testing.T) {
	ctx := context.Background()
	f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, "openclaw", true, false)
	bootstrap := f.r.sandboxBootstrapKey(f.current(t))
	suspended := false
	f.r.Client = interceptor.NewClient(f.kube, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.CreateOption) error {
			if err := c.Create(ctx, object, options...); err != nil {
				return err
			}
			configMap, isReceipt := object.(*corev1.ConfigMap)
			if !isReceipt || suspended || client.ObjectKeyFromObject(configMap) != bootstrap {
				return nil
			}
			current := &api.Agent{}
			if err := c.Get(ctx, client.ObjectKey{Name: f.agent.Name}, current); err != nil {
				return err
			}
			current.Spec.Suspended = true
			current.Generation++
			if err := c.Update(ctx, current); err != nil {
				return err
			}
			suspended = true
			return nil
		},
	})
	if _, err := f.reconcile(); err == nil {
		t.Fatal("receipt-before-finalizer suspension race did not interrupt bootstrap")
	}
	if !suspended {
		t.Fatal("bootstrap receipt was not created before the injected suspension")
	}
	f.reconcileOK(t)
	current := f.current(t)
	if current.Status.Phase != api.PhasePending || current.Status.AccessURL != "" {
		t.Fatalf("interrupted pre-bootstrap suspension was not held pending: %#v", current.Status)
	}
	condition := meta.FindStatusCondition(current.Status.Conditions, api.ConditionSandboxReady)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "SuspensionPending" {
		t.Fatalf("interrupted pre-bootstrap suspension did not report pending evidence: %#v", current.Status.Conditions)
	}
	if err := f.kube.Get(ctx, client.ObjectKey{Name: instanceNamespace(current.Name)}, &corev1.Namespace{}); !apierrors.IsNotFound(err) {
		t.Fatalf("interrupted pre-bootstrap suspension created a namespace: %v", err)
	}
	if f.installer.stages != 0 {
		t.Fatal("interrupted pre-bootstrap suspension rendered or applied a workload")
	}
}

func TestReconciledSandboxSuspensionAfterFinalizerBeforeNamespaceStaysPending(t *testing.T) {
	ctx := context.Background()
	f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, "openclaw", true, false)
	suspended := false
	f.r.Client = interceptor.NewClient(f.kube, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			agent, isAgent := object.(*api.Agent)
			if err := c.Patch(ctx, object, patch, options...); err != nil {
				return err
			}
			if !isAgent || suspended || !controllerutil.ContainsFinalizer(agent, sandbox.BootstrapFinalizer) {
				return nil
			}
			current := &api.Agent{}
			if err := c.Get(ctx, client.ObjectKey{Name: f.agent.Name}, current); err != nil {
				return err
			}
			current.Spec.Suspended = true
			current.Generation++
			if err := c.Update(ctx, current); err != nil {
				return err
			}
			suspended = true
			return nil
		},
	})
	if _, err := f.reconcile(); err == nil {
		t.Fatal("finalizer-before-namespace suspension race did not interrupt bootstrap")
	}
	if !suspended {
		t.Fatal("bootstrap finalizer was not persisted before the injected suspension")
	}
	current := f.current(t)
	if !controllerutil.ContainsFinalizer(current, sandbox.BootstrapFinalizer) {
		t.Fatal("bootstrap finalizer was lost during the interrupted bootstrap")
	}
	if err := f.kube.Get(ctx, client.ObjectKey{Name: instanceNamespace(current.Name)}, &corev1.Namespace{}); !apierrors.IsNotFound(err) {
		t.Fatalf("finalizer-before-namespace suspension created a namespace: %v", err)
	}
	if f.installer.stages != 0 {
		t.Fatal("finalizer-before-namespace suspension rendered or applied a workload")
	}
}

func TestReconciledSandboxSuspensionDoesNotAdoptReplacedNamespace(t *testing.T) {
	ctx := context.Background()
	f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, "openclaw", true, false)
	current := f.current(t)
	namespace := &corev1.Namespace{}
	if err := f.kube.Get(ctx, client.ObjectKey{Name: instanceNamespace(current.Name)}, namespace); err != nil {
		t.Fatal(err)
	}
	namespace.Finalizers = nil
	if err := f.kube.Update(ctx, namespace); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Delete(ctx, namespace); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Delete(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: namespace.Name}}); err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	if err := f.kube.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace.Name, UID: "replacement-namespace"}}); err != nil {
		t.Fatal(err)
	}
	current.Spec.Suspended = true
	current.Generation++
	if err := f.kube.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	stages, writes := f.installer.stages, 0
	f.r.Client = interceptor.NewClient(f.kube, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.CreateOption) error {
			if _, isAgent := object.(*api.Agent); !isAgent {
				writes++
			}
			return c.Create(ctx, object, options...)
		},
		Update: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.UpdateOption) error {
			if _, isAgent := object.(*api.Agent); !isAgent {
				writes++
			}
			return c.Update(ctx, object, options...)
		},
	})
	f.reconcileOK(t)
	current = f.current(t)
	keyBlocked := false
	for _, key := range f.gateway.keys {
		keyBlocked = keyBlocked || key.Blocked
	}
	if current.Status.Phase != api.PhasePending || current.Status.AccessURL != "" || writes != 0 || f.installer.stages != stages || keyBlocked {
		t.Fatalf("suspension adopted or repaired a replaced namespace: status=%#v writes=%d stages=%d", current.Status, writes, f.installer.stages-stages)
	}
}

func TestReconciledSandboxSuspensionWithHistoricalEvidenceStaysPending(t *testing.T) {
	ctx := context.Background()
	f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, "openclaw", true, false)
	agent := f.current(t)
	agent.Spec.Suspended = true
	agent.Generation++
	if err := f.kube.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	agent = f.current(t)
	agent.Status.Sandbox = &api.SandboxStatus{}
	if err := f.kube.Status().Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	f.reconcileOK(t)
	current := f.current(t)
	if current.Status.Phase != api.PhasePending || current.Status.AccessURL != "" {
		t.Fatalf("historical execution evidence did not hold suspension pending: %#v", current.Status)
	}
	if err := f.kube.Get(ctx, client.ObjectKey{Name: instanceNamespace(current.Name)}, &corev1.Namespace{}); !apierrors.IsNotFound(err) {
		t.Fatalf("historical execution evidence bootstrapped a namespace: %v", err)
	}
	if f.installer.stages != 0 {
		t.Fatal("historical execution evidence rendered or applied a workload")
	}
}

func TestReconciledSandboxSuspendsAfterPreBootstrapSkillsFailure(t *testing.T) {
	ctx := context.Background()
	f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, "openclaw", true, false,
		func(r *AgentReconciler, kube client.Client) {
			teams := &api.TeamList{}
			if err := kube.List(ctx, teams); err != nil || len(teams.Items) != 1 {
				t.Fatal("pre-bootstrap skills fixture requires one Team:", err)
			}
			team := &teams.Items[0]
			team.Spec.SkillStorage = &api.SkillStorageSpec{ResourceGroup: "skills-example", StorageAccount: "publishedskills", ContainerName: "skills-v1"}
			if err := kube.Update(ctx, team); err != nil {
				t.Fatal(err)
			}
			catalog := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "skills-catalog", Namespace: r.OperatorNamespace},
				Data: map[string]string{SandboxSkillsCatalogKey: `{"version":"v1","teams":{"other-team":{"teamUID":"other-team-uid","revision":"v1","source":{"resourceGroup":"skills-example","storageAccount":"publishedskills","containerName":"skills-v1","storageClassName":"azureblob-nfs-premium","storageSize":"1Gi"}}}}`}}
			if err := kube.Create(ctx, catalog); err != nil {
				t.Fatal(err)
			}
			r.SandboxPreparation.bootstrap.SkillsCatalogConfigMapName = catalog.Name
			r.SandboxPreparation.Prerequisites.(*KubernetesSandboxPrerequisites).SkillsCatalogConfigMapName = catalog.Name
		},
	)
	if _, err := f.reconcile(); err == nil {
		t.Fatal("missing Team skills publication did not fail before bootstrap")
	}
	failed := f.current(t)
	if !controllerutil.ContainsFinalizer(failed, sandbox.HoldFinalizer) {
		t.Fatalf("pre-bootstrap skills failure did not retain the hold: %#v", failed.Finalizers)
	}
	if err := f.kube.Get(ctx, client.ObjectKey{Name: instanceNamespace(failed.Name)}, &corev1.Namespace{}); !apierrors.IsNotFound(err) {
		t.Fatalf("pre-bootstrap skills failure created a namespace: %v", err)
	}
	if _, _, err := f.r.readSandboxBootstrap(ctx, failed); !apierrors.IsNotFound(err) {
		t.Fatalf("pre-bootstrap skills failure created a bootstrap receipt: %v", err)
	}
	if condition := meta.FindStatusCondition(failed.Status.Conditions, api.ConditionHelmDeployed); condition == nil ||
		condition.Status != metav1.ConditionFalse || condition.Reason != "ChartNotInstalled" {
		t.Fatalf("first pre-bootstrap failure did not preserve creation progress: %#v", failed.Status.Conditions)
	}
	// An unchanged missing publication must not cycle the Agent through a
	// persisted Provisioning phase.  That would continuously invalidate the
	// Portal's suspension confirmation version despite no intent changing.
	before := failed.DeepCopy()
	resourceWrites, statusWrites := 0, 0
	f.r.Client = interceptor.NewClient(f.kube, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.CreateOption) error {
			if _, isAgent := object.(*api.Agent); !isAgent {
				resourceWrites++
			}
			return c.Create(ctx, object, options...)
		},
		Update: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.UpdateOption) error {
			if _, isAgent := object.(*api.Agent); !isAgent {
				resourceWrites++
			}
			return c.Update(ctx, object, options...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			if _, isAgent := object.(*api.Agent); !isAgent {
				resourceWrites++
			}
			return c.Patch(ctx, object, patch, options...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.DeleteOption) error {
			if _, isAgent := object.(*api.Agent); !isAgent {
				resourceWrites++
			}
			return c.Delete(ctx, object, options...)
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, subresource string, object client.Object,
			patch client.Patch, options ...client.SubResourcePatchOption,
		) error {
			if _, isAgent := object.(*api.Agent); isAgent && subresource == "status" {
				statusWrites++
			}
			return c.SubResource(subresource).Patch(ctx, object, patch, options...)
		},
	})
	if _, err := f.reconcile(); err == nil {
		t.Fatal("unchanged missing Team skills publication unexpectedly succeeded")
	}
	held := f.current(t)
	if held.UID != before.UID || held.Generation != before.Generation || !reflect.DeepEqual(held.Spec, before.Spec) ||
		held.ResourceVersion != before.ResourceVersion || !reflect.DeepEqual(held.Status, before.Status) ||
		resourceWrites != 0 || statusWrites != 0 {
		t.Fatalf("unchanged pre-bootstrap hold churned state: resourceWrites=%d statusWrites=%d before=%#v after=%#v", resourceWrites, statusWrites, before, held)
	}
	// This is the same persisted intent a successful Portal suspension request
	// writes after its independent no-lifetime proof.
	failed.Spec.Suspended = true
	failed.Generation++
	if err := f.kube.Update(ctx, failed); err != nil {
		t.Fatal(err)
	}
	f.reconcileOK(t)
	current := f.current(t)
	if current.Status.Phase != api.PhaseSuspended || current.Status.AccessURL != "" {
		t.Fatalf("pre-bootstrap skills failure was not safely suspended: %#v", current.Status)
	}
	if err := f.kube.Get(ctx, client.ObjectKey{Name: instanceNamespace(current.Name)}, &corev1.Namespace{}); !apierrors.IsNotFound(err) {
		t.Fatalf("suspending the failed pre-bootstrap Agent created a namespace: %v", err)
	}
	if f.installer.stages != 0 {
		t.Fatal("suspending the failed pre-bootstrap Agent rendered or applied a workload")
	}
}

func (i *sharedSandboxInstaller) ResolveImageUpgrade(_, _, _, _, image, _ string) (string, error) {
	return image, nil
}

func (i *sharedSandboxInstaller) Prepare(_ string, values map[string]any, version string) (*helm.PreparedRelease, error) {
	if version != i.chart.Metadata.Version {
		return nil, fmt.Errorf("unexpected chart version")
	}
	return helm.PrepareChart(i.chart, values)
}

func (i *sharedSandboxInstaller) Refresh(prepared *helm.PreparedRelease, values map[string]any) (*helm.PreparedRelease, error) {
	return prepared.Rebuild(values)
}

func (i *sharedSandboxInstaller) InstallOrUpgrade(namespace, releaseName string, prepared *helm.PreparedRelease) error {
	if i.beforeApply != nil {
		if err := i.beforeApply(prepared); err != nil {
			return err
		}
	}
	if err := prepared.AuthorizeSandboxRender(context.Background()); err != nil {
		return err
	}
	manifest, err := prepared.SandboxManifest()
	if err != nil {
		return err
	}
	ctx := context.Background()
	decoder := kubeyaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifest), 4096)
	for {
		object := &unstructured.Unstructured{}
		if err := decoder.Decode(object); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return err
		}
		if len(object.Object) == 0 {
			continue
		}
		if object.GetNamespace() != namespace && !w.KubernetesBindingResource(*object) {
			expected, err := prepared.ReconcileExpectation()
			if err != nil || !slices.ContainsFunc(expected.Controls, func(control unstructured.Unstructured) bool {
				return control.GetAPIVersion() == "networking.istio.io/v1beta1" && control.GetKind() == "DestinationRule" &&
					reflect.DeepEqual(control.Object, object.Object)
			}) {
				return fmt.Errorf("unexpected rendered namespace")
			}
		}
		annotations := object.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations["meta.helm.sh/release-name"] = releaseName
		annotations["meta.helm.sh/release-namespace"] = namespace
		object.SetAnnotations(annotations)
		workload := object.GetAPIVersion() == "apps/v1" && (object.GetKind() == "StatefulSet" || object.GetKind() == "Deployment")
		current := &unstructured.Unstructured{}
		current.SetGroupVersionKind(object.GroupVersionKind())
		if err := i.kube.Get(ctx, client.ObjectKeyFromObject(object), current); apierrors.IsNotFound(err) {
			var complete func(error) error
			var authorizeErr error
			if w.KubernetesBindingResource(*object) {
				complete, authorizeErr = prepared.AuthorizeSandboxBindingCreate(ctx, *object)
			} else if workload && object.GetKind() == "Deployment" {
				authorizeErr = prepared.AuthorizeSandboxAuxiliaryApply(ctx, *object)
			} else if workload {
				complete, authorizeErr = prepared.AuthorizeSandboxWorkloadCreate(ctx)
			}
			if authorizeErr != nil {
				return authorizeErr
			}
			err := i.kube.Create(ctx, object)
			if complete != nil {
				err = complete(err)
			}
			if err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else {
			if workload {
				var err error
				if object.GetKind() == "Deployment" {
					err = prepared.AuthorizeSandboxAuxiliaryApply(ctx, *object)
				} else {
					err = prepared.AuthorizeSandboxApply(ctx)
				}
				if err != nil {
					return err
				}
			}
			object.SetUID(current.GetUID())
			object.SetResourceVersion(current.GetResourceVersion())
			if err := i.kube.Update(ctx, object); err != nil {
				return err
			}
		}
	}
	i.prepared = prepared
	i.stages++
	return nil
}

func (i *sharedSandboxInstaller) CustomApplied(_, _ string, _ *helm.PreparedRelease) (bool, error) {
	return false, fmt.Errorf("fixture is not a Custom runtime")
}

func (i *sharedSandboxInstaller) Uninstall(namespace, _ string) error {
	if i.prepared == nil {
		return nil
	}
	expected, err := i.prepared.ReconcileExpectation()
	if err != nil {
		return err
	}
	for _, workload := range expected.Workloads {
		object := &unstructured.Unstructured{}
		object.SetAPIVersion("apps/v1")
		object.SetKind(workload.Kind)
		object.SetNamespace(namespace)
		object.SetName(workload.Identity.Name)
		if err := i.kube.Delete(context.Background(), object); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

func TestReconciledSandboxUsesCommonApplyWithoutOneShotState(t *testing.T) {
	f := newSandboxRuntimeFixture(t, true, nil, true)
	ctx := context.Background()
	for range 3 {
		f.reconcileOK(t)
	}
	if f.installer.stages != 1 {
		t.Fatalf("expected one common apply, got %d", f.installer.stages)
	}
	agent := f.current(t)
	workload := &appsv1.StatefulSet{}
	if err := f.kube.Get(ctx, client.ObjectKey{Namespace: instanceNamespace(agent.Name), Name: instanceServiceName(agent.Name)}, workload); err != nil {
		t.Fatal(err)
	}
	if ptr.Deref(workload.Spec.Replicas, 0) != 1 || len(workload.Spec.Template.Finalizers) != 0 {
		t.Fatal("new shared workload retained the legacy zero-replica/single-Pod hold")
	}
	namespace := &corev1.Namespace{}
	if err := f.kube.Get(ctx, client.ObjectKey{Name: workload.Namespace}, namespace); err != nil {
		t.Fatal(err)
	}
	state := &corev1.ConfigMap{}
	if err := f.kube.Get(ctx, client.ObjectKey{Namespace: f.r.OperatorNamespace,
		Name: w.PodExpectationName(namespace.Name, namespace.UID)}, state); err != nil {
		t.Fatal(err)
	}
	expected, err := w.ReadReconcileExpectation(state)
	if err != nil {
		t.Fatal(err)
	}
	if expected.Agent.UID != agent.UID || len(expected.Workloads) != 1 {
		t.Fatal("common expectation did not bind the current owned workload")
	}
	homeUID := expected.Home.UID
	oldPlan := expected.PlanHash
	secret := &corev1.Secret{}
	if err := f.kube.Get(ctx, client.ObjectKey{Namespace: workload.Namespace, Name: agent.Spec.GatewayTokenSecretRef}, secret); err != nil {
		t.Fatal(err)
	}
	secret.Data["CETIRUN_AUTH_KEY"] = []byte("synthetic-replacement-auth-key")
	if err := f.kube.Update(ctx, secret); err != nil {
		t.Fatal(err)
	}
	f.reconcileOK(t)
	if err := f.kube.Get(ctx, client.ObjectKeyFromObject(state), state); err != nil {
		t.Fatal(err)
	}
	expected, err = w.ReadReconcileExpectation(state)
	if err != nil {
		t.Fatal(err)
	}
	if expected.Home.UID != homeUID || expected.PlanHash == oldPlan || f.installer.stages != 2 {
		t.Fatal("common credential update failed to preserve HOME or reconcile the new template")
	}
	if _, _, err := f.r.readSandboxPreparationReceipt(ctx, agent); !apierrors.IsNotFound(err) {
		t.Fatalf("new common flow unexpectedly created a legacy single-start receipt: %v", err)
	}
}

func TestReconciledSandboxRequiresReadableLifetimeMode(t *testing.T) {
	for _, readErr := range []error{
		errors.New("synthetic bootstrap read outage"),
		apierrors.NewNotFound(corev1.Resource("configmaps"), "bootstrap"),
	} {
		t.Run(readErr.Error(), func(t *testing.T) {
			f := newSandboxRuntimeFixture(t, true, nil, true)
			agent := f.current(t)
			stages := f.installer.stages
			f.r.APIReader = interceptor.NewClient(f.kube.(client.WithWatch), interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
					if key == f.r.sandboxBootstrapKey(agent) {
						return readErr
					}
					return c.Get(ctx, key, object, opts...)
				},
			})
			if _, err := f.reconcile(); !errors.Is(err, readErr) {
				t.Fatal("reconciliation did not preserve the lifetime read error:", err)
			}
			if _, err := f.r.ensureVirtualService(context.Background(), agent); !errors.Is(err, readErr) {
				t.Fatal("route publication did not preserve the lifetime read error:", err)
			}
			if f.installer.stages != stages {
				t.Fatal("unreadable lifetime mode permitted a workload update")
			}
			if ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 ||
				!slices.Contains(f.current(t).Finalizers, sandbox.HoldFinalizer) {
				t.Fatal("unreadable lifetime mode skipped independent workload shutdown")
			}
			f.assertNotReady(t)
		})
	}
}

func TestNativeExecutionUsesCommonReconciliationWithoutUpgradeGate(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		t.Run(kind, func(t *testing.T) {
			f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, kind, true, false)
			config, err := ParseSandboxSourcesConfig(nativeSandboxSourcesJSON)
			if err != nil {
				t.Fatal(err)
			}
			config.NativeExecution = true
			f.r.SandboxPreparation, err = NewKubernetesSandboxPreparation(config, f.kube, f.installer,
				f.r.OperatorNamespace, f.r.ChartVersion)
			if err != nil {
				t.Fatal(err)
			}
			for range 3 {
				f.reconcileOK(t)
			}
			agent := f.current(t)
			_, receipt, err := f.r.readSandboxBootstrap(context.Background(), agent)
			if err != nil || !receipt.Reconciled {
				t.Fatal("native execution did not select the common lifetime:", err)
			}
			expected := reconciledExpectationForTest(t, f)
			if len(expected.Workloads) != 1 || expected.Workloads[0].Replicas != 1 ||
				slices.Contains(agent.Finalizers, sandbox.PreparationFinalizer) || f.installer.stages != 1 {
				t.Fatal("new native Agent retained the single-start preparation path")
			}
		})
	}
}

func TestReconciledSandboxDoesNotAdoptForeignWorkload(t *testing.T) {
	f := newSandboxRuntimeFixture(t, true, nil, true)
	workload := &appsv1.StatefulSet{}
	key := client.ObjectKey{Namespace: instanceNamespace(f.agent.Name), Name: instanceServiceName(f.agent.Name)}
	if err := f.kube.Get(context.Background(), key, workload); err != nil {
		t.Fatal(err)
	}
	workload.OwnerReferences[0].UID = types.UID("another-agent")
	if err := f.kube.Update(context.Background(), workload); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reconcile(); err == nil {
		t.Fatal("common reconciliation adopted a foreign workload")
	}
}

func TestReconciledSandboxStatusFailureIsNotAuthorizationDenial(t *testing.T) {
	f := newSandboxRuntimeFixture(t, true, nil, true)
	ctx := context.Background()
	agent := f.current(t)
	agent.Spec.Model = testSharedModelOne
	agent.Generation++
	if err := f.kube.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	watch, ok := f.kube.(client.WithWatch)
	if !ok {
		t.Fatal("fixture requires watch support")
	}
	f.r.Client = interceptor.NewClient(watch, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, subresource string, object client.Object,
			patch client.Patch, options ...client.SubResourcePatchOption,
		) error {
			if subresource == "status" {
				return apierrors.NewConflict(api.GroupVersion.WithResource("agents").GroupResource(), object.GetName(), errors.New("synthetic observation conflict"))
			}
			return c.SubResource(subresource).Patch(ctx, object, patch, options...)
		},
	})
	result, err := f.r.reconcilePolicy(ctx, agent)
	if err == nil || !result.authorized || !result.dependencyUnavailable {
		t.Fatalf("valid policy observation failure became a destructive authorization denial: result=%+v error=%v", result, err)
	}
}

func TestReconciledSandboxRouteOwnershipSurvivesHold(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprintf("foreign=%v", foreign), func(t *testing.T) {
			f := newSandboxRuntimeFixture(t, true, nil, true)
			ctx := context.Background()
			agent := f.current(t)
			if _, err := f.r.ensureVirtualService(ctx, agent); err != nil {
				t.Fatal(err)
			}
			route := &istiov1beta1.VirtualService{}
			key := client.ObjectKey{Namespace: instanceNamespace(agent.Name), Name: virtualServiceName(agent.Name)}
			if err := f.kube.Get(ctx, key, route); err != nil {
				t.Fatal(err)
			}
			if !sandboxBuiltinOwned(route, agent) {
				t.Fatal("the common route cannot be safely withdrawn by its own controller")
			}
			if foreign {
				route.OwnerReferences[0].UID = "another-agent"
				if err := f.kube.Update(ctx, route); err != nil {
					t.Fatal(err)
				}
				if err := f.r.stopSandboxBuiltin(ctx, agent); err == nil {
					t.Fatal("hold cleanup accepted a foreign route")
				}
				if _, err := f.r.ensureVirtualService(ctx, agent); err == nil {
					t.Fatal("normal reconciliation adopted a foreign route")
				}
			} else {
				if err := f.r.stopSandboxBuiltin(ctx, agent); err != nil {
					t.Fatal("the common route blocked its own normal hold:", err)
				}
				if err := f.kube.Get(ctx, key, route); !apierrors.IsNotFound(err) {
					t.Fatal("owned route was not withdrawn:", err)
				}
			}
		})
	}
}

func TestReconciledSandboxRepairsCurrentStateDrift(t *testing.T) {
	f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, "openclaw", true, true)
	ctx := context.Background()
	expected := reconciledExpectationForTest(t, f)
	before := f.installer.stages
	for _, workload := range expected.Workloads {
		if workload.Kind == "StatefulSet" {
			object := &appsv1.StatefulSet{}
			if err := f.kube.Get(ctx, client.ObjectKey{Namespace: expected.Namespace.Name, Name: workload.Identity.Name}, object); err != nil {
				t.Fatal(err)
			}
			object.Spec.Replicas = ptr.To[int32](0)
			if err := f.kube.Update(ctx, object); err != nil {
				t.Fatal(err)
			}
		}
	}
	f.reconcileOK(t)
	if f.installer.stages != before+1 {
		t.Fatal("the shared apply cache concealed a stopped workload")
	}
	after := reconciledExpectationForTest(t, f)
	if after.Home != expected.Home || after.PlanHash != expected.PlanHash {
		t.Fatal("repair changed the authorized plan or retained HOME")
	}
}

func TestReconciledSandboxSteadyStateDoesNotRewriteStatus(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, suspended := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/suspended=%t", kind, suspended), func(t *testing.T) {
				ctx := context.Background()
				f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, kind, true, false)
				if suspended {
					agent := f.current(t)
					agent.Spec.Suspended = true
					agent.Generation++
					if err := f.kube.Update(ctx, agent); err != nil {
						t.Fatal(err)
					}
				} else {
					workload := f.workload(t)
					workload.Status.ObservedGeneration = workload.Generation
					workload.Status.ReadyReplicas, workload.Status.UpdatedReplicas = 1, 1
					if err := f.kube.Status().Update(ctx, workload); err != nil {
						t.Fatal(err)
					}
				}
				f.reconcileOK(t)
				agent := f.current(t)
				wantPhase := api.PhaseReady
				if suspended {
					wantPhase = api.PhaseSuspended
				}
				if agent.Status.Phase != wantPhase {
					t.Fatalf("fixture phase = %s, want %s", agent.Status.Phase, wantPhase)
				}
				previousTime := metav1.NewTime(time.Now().Add(-time.Minute).Truncate(time.Second))
				agent.Status.LastReconcileTime = &previousTime
				if err := f.kube.Status().Update(ctx, agent); err != nil {
					t.Fatal(err)
				}
				// Settle source resourceVersions after the synthetic status change.
				f.reconcileOK(t)
				agent = f.current(t)
				stages, statusWrites := f.installer.stages, 0
				f.r.Client = interceptor.NewClient(f.kube.(client.WithWatch), interceptor.Funcs{
					SubResourcePatch: func(ctx context.Context, c client.Client, subresource string, object client.Object,
						patch client.Patch, opts ...client.SubResourcePatchOption,
					) error {
						if _, ok := object.(*api.Agent); ok && subresource == "status" {
							statusWrites++
						}
						return c.SubResource(subresource).Patch(ctx, object, patch, opts...)
					},
				})
				for range 3 {
					result, err := f.reconcile()
					if err != nil || result.Requeue || result.RequeueAfter != 10*time.Minute {
						t.Fatalf("steady reconciliation did not retain its periodic requeue: %+v, %v", result, err)
					}
				}
				current := f.current(t)
				if statusWrites != 0 || f.installer.stages != stages || current.ResourceVersion != agent.ResourceVersion ||
					!current.Status.LastReconcileTime.Equal(&previousTime) || current.Status.Phase != wantPhase {
					t.Fatalf("unchanged lifetime generated status/watch churn: status writes=%d, applies=%d",
						statusWrites, f.installer.stages-stages)
				}
				stale := metav1.NewTime(time.Now().Add(-11 * time.Minute))
				current.Status.LastReconcileTime = &stale
				if err := f.kube.Status().Update(ctx, current); err != nil {
					t.Fatal(err)
				}
				for range 3 {
					f.reconcileOK(t)
				}
				if statusWrites != 1 || f.installer.stages != stages ||
					!f.current(t).Status.LastReconcileTime.After(stale.Time) {
					t.Fatalf("periodic status refresh did not settle after one write: %d", statusWrites)
				}
				secret := &corev1.Secret{}
				if err := f.kube.Get(ctx, client.ObjectKey{Namespace: instanceNamespace(agent.Name), Name: agent.Spec.GatewayTokenSecretRef}, secret); err != nil {
					t.Fatal(err)
				}
				secret.Data["CETIRUN_AUTH_KEY"] = []byte("synthetic-steady-state-rotation")
				if err := f.kube.Update(ctx, secret); err != nil {
					t.Fatal(err)
				}
				f.reconcileOK(t)
				if f.installer.stages != stages+1 {
					t.Fatal("stable status suppressed a current source change")
				}
			})
		}
	}
}

func TestReconciledSandboxSuspensionKeepsHomeAndKey(t *testing.T) {
	testReconciledSandboxSuspension(t)
}

func testReconciledSandboxSuspension(t *testing.T, setup ...func(*AgentReconciler, client.Client)) {
	testReconciledSandboxSuspensionForHarnesses(t, []string{"openclaw", "hermes"}, setup...)
}

func testReconciledSandboxSuspensionForHarnesses(t *testing.T, harnesses []string, setup ...func(*AgentReconciler, client.Client)) {
	for _, kind := range harnesses {
		for _, mode := range []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod, corev1.ReadWriteOnce} {
			t.Run(fmt.Sprintf("%s/%s", kind, mode), func(t *testing.T) {
				f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, kind, true, true,
					append([]func(*AgentReconciler, client.Client){withNativeHomeAccessMode(t, mode)}, setup...)...)
				ctx := context.Background()
				initial := reconciledExpectationForTest(t, f)
				keyID := f.current(t).Status.AIGateway.KeyID
				for _, key := range f.gateway.keys {
					key.Spend = ptr.To(3.25)
				}
				for range 2 {
					for _, suspended := range []bool{true, false} {
						agent := f.current(t)
						agent.Spec.Suspended = suspended
						agent.Generation++
						if err := f.kube.Update(ctx, agent); err != nil {
							t.Fatal(err)
						}
						f.reconcileOK(t)
						current := f.current(t)
						expected := reconciledExpectationForTest(t, f)
						if expected.Home != initial.Home || current.Status.AIGateway.KeyID != keyID || len(f.gateway.generated) != 1 {
							t.Fatal("suspension/reconciliation replaced HOME or reset the Gateway key")
						}
						want := int32(1)
						if suspended {
							want = 0
							if current.Status.Phase != api.PhaseSuspended || !current.Status.AIGateway.Blocked {
								t.Fatal("suspension did not stop access before acknowledging completion")
							}
						} else if current.Status.AIGateway.Blocked {
							t.Fatal("authorized reconciliation did not release its own suspension block")
						}
						for _, workload := range expected.Workloads {
							if workload.Replicas != want {
								t.Fatal("Agent and auxiliary suspension states differ")
							}
						}
						for _, key := range f.gateway.keys {
							if ptr.Deref(key.Spend, 0) != 3.25 {
								t.Fatal("suspension or resume reset retained spending")
							}
						}
						assertNativeHomeAccessMode(t, f, mode)
					}
				}
			})
		}
	}
}

func TestReconciledSandboxRetainedRestartUsesCommonApply(t *testing.T) {
	testReconciledSandboxRetainedRestart(t)
}

func testReconciledSandboxRetainedRestart(t *testing.T, setup ...func(*AgentReconciler, client.Client)) {
	testReconciledSandboxRetainedRestartForHarnesses(t, []string{"openclaw", "hermes"}, setup...)
}

func testReconciledSandboxRetainedRestartForHarnesses(t *testing.T, harnesses []string, setup ...func(*AgentReconciler, client.Client)) {
	for _, kind := range harnesses {
		for _, mode := range []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod, corev1.ReadWriteOnce} {
			t.Run(fmt.Sprintf("%s/%s", kind, mode), func(t *testing.T) {
				f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, kind, true, false,
					append([]func(*AgentReconciler, client.Client){withNativeHomeAccessMode(t, mode)}, setup...)...)
				assertNativeHomeAccessMode(t, f, mode)
				ctx := context.Background()
				initial := reconciledExpectationForTest(t, f)
				keyID := f.current(t).Status.AIGateway.KeyID
				var previousPod *corev1.Pod
				for _, requestID := range []string{"request-00000001", "request-00000002"} {
					agent := f.current(t)
					agent.Spec.RestartRequestID = requestID
					agent.Generation++
					if err := f.kube.Update(ctx, agent); err != nil {
						t.Fatal(err)
					}
					for range 3 {
						f.reconcileOK(t)
					}
					workload := f.workload(t)
					if workload.Spec.Template.Annotations[api.AnnotationRestartRequestID] != requestID {
						t.Fatal("common reconciliation did not apply the durable restart request")
					}
					if f.current(t).Status.ObservedRestartRequestID == requestID {
						t.Fatal("template application or the previous Pod completed a restart")
					}
					expected := reconciledExpectationForTest(t, f)
					if expected.Home != initial.Home || expected.Namespace != initial.Namespace ||
						f.current(t).UID != initial.Agent.UID || f.current(t).Status.AIGateway.KeyID != keyID || len(f.gateway.generated) != 1 {
						t.Fatal("restart replaced HOME, namespace, Agent or Gateway spending identity")
					}
					// The fixture supplies Kubernetes observations, not a live kubelet.
					if previousPod != nil {
						if err := f.kube.Delete(ctx, previousPod); err != nil {
							t.Fatal(err)
						}
					}
					pod := &corev1.Pod{ObjectMeta: *workload.Spec.Template.ObjectMeta.DeepCopy(), Spec: *workload.Spec.Template.Spec.DeepCopy()}
					pod.Name, pod.Namespace, pod.UID = workload.Name+"-0", workload.Namespace, types.UID(requestID)
					pod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(workload, appsv1.SchemeGroupVersion.WithKind("StatefulSet"))}
					pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
					if err := f.kube.Create(ctx, pod); err != nil {
						t.Fatal(err)
					}
					workload.Status = appsv1.StatefulSetStatus{ObservedGeneration: workload.Generation, ReadyReplicas: 1, UpdatedReplicas: 1}
					if err := f.kube.Status().Update(ctx, workload); err != nil {
						t.Fatal(err)
					}
					f.reconcileOK(t)
					if f.current(t).Status.ObservedRestartRequestID != requestID {
						t.Fatal("current ready Pod did not complete the retained restart")
					}
					stages := f.installer.stages
					f.reconcileOK(t)
					if f.installer.stages != stages {
						t.Fatal("an unchanged restart request repeated workload application")
					}
					assertNativeHomeAccessMode(t, f, mode)
					previousPod = pod
				}
			})
		}
	}
}

func TestReconciledSandboxSuspensionStopsBeforePrerequisites(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, unavailable := range []string{"runtime", "chart"} {
			for _, coordinated := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/coordination=%t", kind, unavailable, coordinated), func(t *testing.T) {
					ctx := context.Background()
					var setup []func(*AgentReconciler, client.Client)
					if coordinated {
						setup = append(setup, withReconciledCoordination(t))
					}
					f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, kind, true, true, setup...)
					main := f.workload(t)
					main.Status.ObservedGeneration = main.Generation
					main.Status.Replicas, main.Status.ReadyReplicas, main.Status.UpdatedReplicas = 1, 1, 1
					if err := f.kube.Status().Update(ctx, main); err != nil {
						t.Fatal(err)
					}
					f.reconcileOK(t)
					agent := f.current(t)
					if agent.Status.Phase != api.PhaseReady || agent.Status.AccessURL == "" {
						t.Fatal("fixture must have an active owned workload and route")
					}
					expected := reconciledExpectationForTest(t, f)
					keyID, stages := agent.Status.AIGateway.KeyID, f.installer.stages
					agent.Spec.Suspended = true
					agent.Generation++
					if err := f.kube.Update(ctx, agent); err != nil {
						t.Fatal(err)
					}
					// Keep the projection observation current so an unrelated
					// policy-reprojection hold cannot mask suspension ordering.
					for index := range agent.Status.Conditions {
						if agent.Status.Conditions[index].Type == api.ConditionSandboxReady {
							agent.Status.Conditions[index].ObservedGeneration = agent.Generation
						}
					}
					if err := f.kube.Status().Update(ctx, agent); err != nil {
						t.Fatal(err)
					}
					if unavailable == "runtime" {
						source := f.r.SandboxPreparation.Configuration
						f.r.SandboxPreparation.Configuration = SandboxConfigurationSourceFunc(func(ctx context.Context) (SandboxConfigurationSnapshot, error) {
							snapshot, err := source.Snapshot(ctx)
							if err != nil {
								return snapshot, err
							}
							registration := snapshot.Registrations[api.SandboxKata]
							registration.Available = false
							snapshot.Registrations[api.SandboxKata] = registration
							return snapshot, nil
						})
					} else {
						f.installer.chart.Metadata.Version = "999.0.0"
					}
					resolution, err := f.r.resolvePolicy(ctx, agent)
					if err != nil || policyReprojectionMustHold(agent, resolution) {
						t.Fatal("fixture must reach suspension without a policy-reprojection hold:", err)
					}
					if _, err := f.reconcile(); err == nil {
						t.Fatal("the unavailable prerequisite or unacknowledged stop must remain visible")
					}
					for _, workload := range expected.Workloads {
						object := &unstructured.Unstructured{}
						object.SetAPIVersion("apps/v1")
						object.SetKind(workload.Kind)
						if err := f.kube.Get(ctx, client.ObjectKey{Namespace: expected.Namespace.Name, Name: workload.Identity.Name}, object); err != nil {
							t.Fatal(err)
						}
						replicas, _, err := unstructured.NestedInt64(object.Object, "spec", "replicas")
						if err != nil || replicas != 0 {
							t.Fatalf("unavailable %s left %s running: replicas=%d, error=%v", unavailable, workload.Kind, replicas, err)
						}
					}
					route := &istiov1beta1.VirtualService{}
					if err := f.kube.Get(ctx, client.ObjectKey{Namespace: expected.Namespace.Name, Name: virtualServiceName(agent.Name)}, route); !apierrors.IsNotFound(err) {
						t.Fatal("unavailable prerequisite left the route published:", err)
					}
					current := f.current(t)
					if current.Status.AccessURL != "" || current.Status.AIGateway.KeyID != keyID || len(f.gateway.generated) != 1 {
						t.Fatal("suspension lost access state or replaced the retained Gateway identity")
					}
					for _, key := range f.gateway.keys {
						if !key.Blocked {
							t.Fatal("unavailable prerequisite prevented suspension from blocking the Gateway key")
						}
					}
					home := &corev1.PersistentVolumeClaim{}
					if err := f.kube.Get(ctx, client.ObjectKey{Namespace: expected.Namespace.Name, Name: expected.Home.Name}, home); err != nil || home.UID != expected.Home.UID {
						t.Fatal("suspension changed the retained HOME:", err)
					}
					if f.installer.stages != stages {
						t.Fatal("suspension with unavailable prerequisites attempted Helm application")
					}
					if coordinated {
						ticket, found, err := f.r.SandboxNativeCoordination.ReadNative(ctx, agent.UID)
						if err != nil || !found || ticket.Reservation.Phase != sandboxcoordination.Stopping {
							t.Fatal("unacknowledged shutdown released its source reservation:", err)
						}
					}
				})
			}
		}
	}
}

func TestReconciledSandboxSuspensionAttemptsIndependentCleanup(t *testing.T) {
	for _, failure := range []string{"gateway", "status", "main", "auxiliary"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, "openclaw", true, true)
			agent := f.current(t)
			if _, err := f.r.ensureVirtualService(ctx, agent); err != nil {
				t.Fatal(err)
			}
			expected := reconciledExpectationForTest(t, f)
			stages := f.installer.stages
			agent.Spec.Suspended = true
			agent.Generation++
			if err := f.kube.Update(ctx, agent); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("synthetic suspension dependency failure")
			if failure == "gateway" {
				f.gateway.getErr = injected
			}
			f.r.Client = interceptor.NewClient(f.kube, interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
					_, main := object.(*appsv1.StatefulSet)
					_, auxiliary := object.(*appsv1.Deployment)
					if failure == "main" && main || failure == "auxiliary" && auxiliary {
						return injected
					}
					return c.Patch(ctx, object, patch, opts...)
				},
				SubResourcePatch: func(ctx context.Context, c client.Client, subresource string, object client.Object,
					patch client.Patch, opts ...client.SubResourcePatchOption,
				) error {
					if _, ok := object.(*api.Agent); ok && failure == "status" && subresource == "status" {
						return injected
					}
					return c.SubResource(subresource).Patch(ctx, object, patch, opts...)
				},
			})
			if _, err := f.reconcile(); !errors.Is(err, injected) {
				t.Fatal("suspension did not report the independent failure:", err)
			}
			for _, workload := range expected.Workloads {
				object := &unstructured.Unstructured{}
				object.SetAPIVersion("apps/v1")
				object.SetKind(workload.Kind)
				if err := f.kube.Get(ctx, client.ObjectKey{Namespace: expected.Namespace.Name, Name: workload.Identity.Name}, object); err != nil {
					t.Fatal(err)
				}
				replicas, _, err := unstructured.NestedInt64(object.Object, "spec", "replicas")
				want := int64(0)
				if failure == "main" && workload.Kind == "StatefulSet" || failure == "auxiliary" && workload.Kind == "Deployment" {
					want = 1
				}
				if err != nil || replicas != want {
					t.Fatalf("%s failure skipped independent %s shutdown: got %d, want %d, error=%v",
						failure, workload.Kind, replicas, want, err)
				}
			}
			if err := f.kube.Get(ctx, client.ObjectKey{Namespace: expected.Namespace.Name, Name: virtualServiceName(agent.Name)},
				&istiov1beta1.VirtualService{}); !apierrors.IsNotFound(err) {
				t.Fatal("another cleanup failure skipped route withdrawal:", err)
			}
			if f.installer.stages != stages {
				t.Fatal("incomplete suspension continued into Helm application")
			}
			f.r.Client, f.gateway.getErr = f.kube, nil
			for range 3 {
				f.reconcileOK(t)
			}
			if f.current(t).Status.Phase != api.PhaseSuspended || len(f.gateway.generated) != 1 {
				t.Fatal("suspension did not recover with its existing Gateway key")
			}
		})
	}
}

func TestReconciledSandboxRuntimeUnavailableStopsActiveWorkloads(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, coordinated := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/coordination=%t", kind, coordinated), func(t *testing.T) {
				ctx := context.Background()
				var setup []func(*AgentReconciler, client.Client)
				if coordinated {
					setup = append(setup, withReconciledCoordination(t))
				}
				f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, kind, true, true, setup...)
				workload := f.workload(t)
				workload.Status.ObservedGeneration = workload.Generation
				workload.Status.Replicas, workload.Status.ReadyReplicas, workload.Status.UpdatedReplicas = 1, 1, 1
				if err := f.kube.Status().Update(ctx, workload); err != nil {
					t.Fatal(err)
				}
				f.reconcileOK(t)
				agent := f.current(t)
				if agent.Spec.Suspended || agent.Status.Phase != api.PhaseReady || agent.Status.AccessURL == "" {
					t.Fatal("fixture must be an active, unsuspended reconciled Agent")
				}
				expected := reconciledExpectationForTest(t, f)
				keyID, stages := agent.Status.AIGateway.KeyID, f.installer.stages
				for _, key := range f.gateway.keys {
					key.Spend = ptr.To(3.25)
				}
				source := f.r.SandboxPreparation.Configuration
				f.r.SandboxPreparation.Configuration = SandboxConfigurationSourceFunc(func(ctx context.Context) (SandboxConfigurationSnapshot, error) {
					snapshot, err := source.Snapshot(ctx)
					if err != nil {
						return snapshot, err
					}
					registration := snapshot.Registrations[api.SandboxKata]
					registration.Available = false
					snapshot.Registrations[api.SandboxKata] = registration
					return snapshot, nil
				})
				resolution, err := f.r.resolvePolicy(ctx, agent)
				if err != nil || policyReprojectionMustHold(agent, resolution) {
					t.Fatal("runtime availability must not be hidden by a policy-revision hold:", err)
				}
				if _, err := f.reconcile(); err == nil {
					t.Fatal("unavailable runtime was not reported")
				}
				for _, desired := range expected.Workloads {
					current := &unstructured.Unstructured{}
					current.SetAPIVersion("apps/v1")
					current.SetKind(desired.Kind)
					if err := f.kube.Get(ctx, client.ObjectKey{Namespace: expected.Namespace.Name, Name: desired.Identity.Name}, current); err != nil {
						t.Fatal(err)
					}
					replicas, _, err := unstructured.NestedInt64(current.Object, "spec", "replicas")
					if err != nil || replicas != 0 {
						t.Fatalf("runtime outage left %s running: replicas=%d, error=%v", desired.Kind, replicas, err)
					}
				}
				if err := f.kube.Get(ctx, client.ObjectKey{Namespace: expected.Namespace.Name, Name: virtualServiceName(agent.Name)},
					&istiov1beta1.VirtualService{}); !apierrors.IsNotFound(err) {
					t.Fatal("runtime outage left an active route:", err)
				}
				current := f.current(t)
				if !slices.Contains(current.Finalizers, sandbox.HoldFinalizer) || current.Status.AccessURL != "" ||
					current.Status.AIGateway.KeyID != keyID || len(f.gateway.generated) != 1 || f.installer.stages != stages {
					t.Fatal("runtime hold lost its intent, key identity or performed another apply")
				}
				for _, key := range f.gateway.keys {
					if !key.Blocked || ptr.Deref(key.Spend, 0) != 3.25 {
						t.Fatal("runtime hold failed to block the existing key while preserving spend")
					}
				}
				home := &corev1.PersistentVolumeClaim{}
				if err := f.kube.Get(ctx, client.ObjectKey{Namespace: expected.Namespace.Name, Name: expected.Home.Name}, home); err != nil ||
					home.UID != expected.Home.UID || !home.DeletionTimestamp.IsZero() {
					t.Fatal("runtime hold changed the retained HOME:", err)
				}
				f.assertNotReady(t)
				if coordinated {
					ticket, found, err := f.r.SandboxNativeCoordination.ReadNative(ctx, agent.UID)
					if err != nil || !found || ticket.Reservation.Phase != sandboxcoordination.Stopping {
						t.Fatal("unacknowledged runtime shutdown released its source reservation:", err)
					}
				}
				workload = f.workload(t)
				workload.Status.ObservedGeneration, workload.Status.Replicas = workload.Generation, 0
				if err := f.kube.Status().Update(ctx, workload); err != nil {
					t.Fatal(err)
				}
				f.r.SandboxPreparation.Configuration = source
				for range 3 {
					f.reconcileOK(t)
				}
				current = f.current(t)
				if slices.Contains(current.Finalizers, sandbox.HoldFinalizer) || current.Status.AIGateway.KeyID != keyID ||
					len(f.gateway.generated) != 1 || reconciledExpectationForTest(t, f).Home != expected.Home ||
					ptr.Deref(f.workload(t).Spec.Replicas, -1) != 1 {
					t.Fatal("runtime recovery did not resume the same owned HOME and key lifetime")
				}
				for _, key := range f.gateway.keys {
					if key.Blocked || ptr.Deref(key.Spend, 0) != 3.25 {
						t.Fatal("runtime recovery lost spend or failed to release its own temporary block")
					}
				}
			})
		}
	}
}

func TestReconciledSandboxRuntimeHoldAttemptsCleanupAfterErrors(t *testing.T) {
	for _, failure := range []string{"gateway", "status"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, "openclaw", true, true)
			workload := f.workload(t)
			workload.Status.ObservedGeneration = workload.Generation
			workload.Status.Replicas, workload.Status.ReadyReplicas, workload.Status.UpdatedReplicas = 1, 1, 1
			if err := f.kube.Status().Update(ctx, workload); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				f.reconcileOK(t)
			}
			expected := reconciledExpectationForTest(t, f)
			source := f.r.SandboxPreparation.Configuration
			f.r.SandboxPreparation.Configuration = SandboxConfigurationSourceFunc(func(ctx context.Context) (SandboxConfigurationSnapshot, error) {
				snapshot, err := source.Snapshot(ctx)
				if err != nil {
					return snapshot, err
				}
				registration := snapshot.Registrations[api.SandboxKata]
				registration.Available = false
				snapshot.Registrations[api.SandboxKata] = registration
				return snapshot, nil
			})
			injected := errors.New("synthetic runtime-hold cleanup failure")
			if failure == "gateway" {
				f.gateway.getErr = injected
			} else {
				f.r.Client = interceptor.NewClient(f.kube, interceptor.Funcs{
					SubResourcePatch: func(ctx context.Context, c client.Client, subresource string, object client.Object,
						patch client.Patch, opts ...client.SubResourcePatchOption,
					) error {
						if _, ok := object.(*api.Agent); ok && subresource == "status" {
							return injected
						}
						return c.SubResource(subresource).Patch(ctx, object, patch, opts...)
					},
				})
			}
			if _, err := f.reconcile(); !errors.Is(err, injected) {
				t.Fatal("runtime cleanup failure was not surfaced:", err)
			}
			for _, desired := range expected.Workloads {
				object := &unstructured.Unstructured{}
				object.SetAPIVersion("apps/v1")
				object.SetKind(desired.Kind)
				if err := f.kube.Get(ctx, client.ObjectKey{Namespace: expected.Namespace.Name, Name: desired.Identity.Name}, object); err != nil {
					t.Fatal(err)
				}
				replicas, _, err := unstructured.NestedInt64(object.Object, "spec", "replicas")
				if err != nil || replicas != 0 {
					t.Fatal("runtime hold skipped independent shutdown:", err)
				}
			}
			if err := f.kube.Get(ctx, client.ObjectKey{Namespace: expected.Namespace.Name, Name: virtualServiceName(f.agent.Name)},
				&istiov1beta1.VirtualService{}); !apierrors.IsNotFound(err) {
				t.Fatal("runtime hold skipped route withdrawal:", err)
			}
		})
	}
}

func TestReconciledSandboxFinishesStoppingReservationBeforeResume(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, acknowledged := range []bool{false, true} {
			for _, mode := range []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod, corev1.ReadWriteOnce} {
				t.Run(fmt.Sprintf("%s/acknowledged=%v/%s", kind, acknowledged, mode), func(t *testing.T) {
					ctx := context.Background()
					f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, kind, true, true,
						withReconciledCoordination(t), withNativeHomeAccessMode(t, mode))
					agent := f.current(t)
					if agent.Spec.Suspended || slices.Contains(agent.Finalizers, sandboxHoldFinalizer) {
						t.Fatal("fixture must start with current resume intent and no hold marker")
					}
					expected := reconciledExpectationForTest(t, f)
					keyID, stages := agent.Status.AIGateway.KeyID, f.installer.stages
					store := f.r.SandboxNativeCoordination
					ticket, found, err := store.ReadNative(ctx, agent.UID)
					if err != nil || !found {
						t.Fatal("missing initial source reservation:", err)
					}
					if _, err := store.BeginNativeStop(ctx, ticket); err != nil {
						t.Fatal(err)
					}
					for _, desired := range expected.Workloads {
						key := client.ObjectKey{Namespace: expected.Namespace.Name, Name: desired.Identity.Name}
						if desired.Kind == "StatefulSet" {
							workload := &appsv1.StatefulSet{}
							if err := f.kube.Get(ctx, key, workload); err != nil {
								t.Fatal(err)
							}
							workload.Spec.Replicas = ptr.To[int32](0)
							if err := f.kube.Update(ctx, workload); err != nil {
								t.Fatal(err)
							}
							workload.Status.ObservedGeneration = workload.Generation
							workload.Status.Replicas = 0
							if !acknowledged {
								workload.Status.Replicas = 1
							}
							if err := f.kube.Status().Update(ctx, workload); err != nil {
								t.Fatal(err)
							}
						} else {
							workload := &appsv1.Deployment{}
							if err := f.kube.Get(ctx, key, workload); err != nil {
								t.Fatal(err)
							}
							workload.Spec.Replicas = ptr.To[int32](0)
							if err := f.kube.Update(ctx, workload); err != nil {
								t.Fatal(err)
							}
							workload.Status.ObservedGeneration = workload.Generation
							workload.Status.Replicas = 0
							if err := f.kube.Status().Update(ctx, workload); err != nil {
								t.Fatal(err)
							}
						}
					}
					_, reconcileErr := f.reconcile()
					if !slices.Contains(f.current(t).Finalizers, sandboxHoldFinalizer) || f.installer.stages != stages {
						t.Fatal("pending stop did not restore the hold before any new workload apply")
					}
					current, found, err := store.ReadNative(ctx, agent.UID)
					if err != nil {
						t.Fatal(err)
					}
					if !acknowledged {
						if reconcileErr == nil || !found || current.Reservation.ID != ticket.Reservation.ID ||
							current.Reservation.Phase != sandboxcoordination.Stopping {
							t.Fatal("unacknowledged shutdown released or replaced its stopping reservation:", reconcileErr)
						}
						return
					}
					if reconcileErr != nil || found {
						t.Fatal("acknowledged stop did not finish before resume:", reconcileErr)
					}
					for range 3 {
						f.reconcileOK(t)
					}
					current, found, err = store.ReadNative(ctx, agent.UID)
					updated := reconciledExpectationForTest(t, f)
					if err != nil || !found || current.Reservation.Phase != sandboxcoordination.Reserved ||
						current.Reservation.ID == ticket.Reservation.ID || updated.Home != expected.Home ||
						f.current(t).Status.AIGateway.KeyID != keyID || len(f.gateway.generated) != 1 ||
						slices.Contains(f.current(t).Finalizers, sandboxHoldFinalizer) {
						t.Fatal("resume did not reacquire current authority while preserving HOME and key identity:", err)
					}
					for _, workload := range updated.Workloads {
						if workload.Replicas != 1 {
							t.Fatal("resume did not restore the Agent and auxiliary workloads")
						}
					}
					assertNativeHomeAccessMode(t, f, mode)
				})
			}
		}
	}
}

func withReconciledCoordination(t *testing.T) func(*AgentReconciler, client.Client) {
	t.Helper()
	return func(r *AgentReconciler, kube client.Client) {
		authority, err := sandboxcoordination.NewState(r.OperatorNamespace, "resume-coordination")
		if err != nil {
			t.Fatal(err)
		}
		if err := kube.Create(context.Background(), authority); err != nil {
			t.Fatal(err)
		}
		r.SandboxNativeCoordination = &sandboxcoordination.Store{
			Client: kube, APIReader: kube, Namespace: authority.Namespace,
			Identity: w.ObjectIdentity{Name: authority.Name, UID: authority.UID},
		}
	}
}

func TestReconciledSandboxDeletionCancelsBeforeFirstApply(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, kind, true, false,
				withReconciledCoordination(t))
			if f.installer.stages != 0 {
				t.Fatal("fixture already submitted a workload")
			}
			original := f.r.Client
			injected := errors.New("synthetic final preparation guard failure")
			published := false
			var stateKey client.ObjectKey
			f.r.Client = interceptor.NewClient(f.kube.(client.WithWatch), interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
					if err := c.Create(ctx, object, opts...); err != nil {
						return err
					}
					if state, ok := object.(*corev1.ConfigMap); ok && state.Data[w.PodExpectationKey] != "" {
						published = true
						stateKey = client.ObjectKeyFromObject(state)
					}
					return nil
				},
			})
			f.r.APIReader = interceptor.NewClient(f.kube.(client.WithWatch), interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
					if published && key == stateKey {
						_, reserved, err := f.r.SandboxNativeCoordination.ReadNative(ctx, f.agent.UID)
						if err != nil {
							return err
						}
						if reserved {
							return injected
						}
					}
					return c.Get(ctx, key, object, opts...)
				},
			})
			if _, err := f.reconcile(); err == nil || !published || f.installer.stages != 0 {
				t.Fatalf("fixture did not fail after publication and before submission: published=%t, applies=%d, error=%v",
					published, f.installer.stages, err)
			}
			f.r.Client, f.r.APIReader = original, f.kube
			agent := f.current(t)
			if _, found, err := f.r.SandboxNativeCoordination.ReadNative(ctx, agent.UID); err != nil || !found {
				t.Fatal("pre-submit fixture has no source reservation:", err)
			}
			if err := f.kube.Get(ctx, client.ObjectKey{Namespace: instanceNamespace(agent.Name), Name: instanceServiceName(agent.Name)}, &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
				t.Fatal("pre-submit fixture has a main workload:", err)
			}
			if err := f.kube.Delete(ctx, agent); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				f.reconcileOK(t)
			}
			state := &corev1.ConfigMap{}
			if err := f.kube.Get(ctx, stateKey, state); err != nil ||
				state.Annotations[sandboxApplyStateAnnotation] != sandboxApplyCancelled {
				t.Fatal("pre-submit cancellation was not durably recorded:", err)
			}
			if _, found, err := f.r.SandboxNativeCoordination.ReadNative(ctx, agent.UID); err != nil || found {
				t.Fatal("never-submitted deletion retained its source reservation:", err)
			}
			if err := f.kube.Get(ctx, client.ObjectKeyFromObject(agent), &api.Agent{}); !apierrors.IsNotFound(err) {
				t.Fatal("never-submitted deletion retained its Agent finalizer:", err)
			}
		})
	}
}

func TestReconciledSandboxDeletionReleasesGrantJournal(t *testing.T) {
	testReconciledSandboxDeletionForHarnesses(t, []string{"openclaw", "hermes"})
}

func testReconciledSandboxDeletionForHarnesses(t *testing.T, harnesses []string, setup ...func(*AgentReconciler, client.Client)) {
	for _, kind := range harnesses {
		for _, foreground := range []bool{false, true} {
			for _, sourcesEnabled := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/foreground=%t/sources=%t", kind, foreground, sourcesEnabled), func(t *testing.T) {
					ctx := context.Background()
					f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, kind, true, true, setup...)
					agent := f.current(t)
					journal := &corev1.ConfigMap{}
					journalKey := client.ObjectKey{Namespace: f.r.OperatorNamespace, Name: sandboxpolicy.GrantJournalName(agent.UID)}
					if err := f.kube.Get(ctx, journalKey, journal); err != nil {
						t.Fatal(err)
					}
					bindings, err := f.r.sandboxGrantJournal().Read(ctx, agent.Name, agent.UID)
					if err != nil || len(bindings) == 0 {
						t.Fatal("fixture must retain a protected grant inventory:", err)
					}
					if !sourcesEnabled {
						f.r.SandboxPreparation = nil
					}
					propagation := metav1.DeletePropagationBackground
					if foreground {
						propagation = metav1.DeletePropagationForeground
					}
					if err := f.kube.Delete(ctx, agent, client.PropagationPolicy(propagation)); err != nil {
						t.Fatal(err)
					}
					if foreground {
						// The fake API has no GC; model its dependent deletion
						// while the journal's own finalizer still protects it.
						if err := f.kube.Delete(ctx, journal); err != nil {
							t.Fatal(err)
						}
					}
					for range 3 {
						f.reconcileOK(t)
					}
					if err := f.kube.Get(ctx, journalKey, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
						t.Fatal("normal deletion left the protected journal in the Operator namespace:", err)
					}
					for _, binding := range bindings {
						if err := f.kube.Get(ctx, client.ObjectKey{Namespace: binding.Namespace, Name: binding.Name}, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
							t.Fatal("normal deletion did not revoke its recorded source grant:", err)
						}
					}
					if err := f.kube.Get(ctx, client.ObjectKeyFromObject(agent), &api.Agent{}); !apierrors.IsNotFound(err) {
						t.Fatal("completed journal cleanup retained the Agent:", err)
					}
				})
			}
		}
	}
}

func TestReconciledSandboxGrantCleanupFailureRetainsFinalizers(t *testing.T) {
	for _, failure := range []string{"revoke", "release-conflict", "delete", "delete-lost-ack", "foreign-owner"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, "hermes", true, true)
			agent := f.current(t)
			if _, err := f.r.ensureVirtualService(ctx, agent); err != nil {
				t.Fatal(err)
			}
			journalKey := client.ObjectKey{Namespace: f.r.OperatorNamespace, Name: sandboxpolicy.GrantJournalName(agent.UID)}
			original := &corev1.ConfigMap{}
			if err := f.kube.Get(ctx, journalKey, original); err != nil {
				t.Fatal(err)
			}
			if failure == "foreign-owner" {
				foreign := original.DeepCopy()
				foreign.OwnerReferences[0].UID = "another-agent"
				if err := f.kube.Update(ctx, foreign); err != nil {
					t.Fatal(err)
				}
			}
			injected := errors.New("synthetic grant cleanup failure")
			f.r.Client = interceptor.NewClient(f.kube, interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.UpdateOption) error {
					if state, ok := object.(*corev1.ConfigMap); ok && client.ObjectKeyFromObject(state) == journalKey &&
						failure == "release-conflict" && !slices.Contains(state.Finalizers, sandboxpolicy.GrantJournalFinalizer) {
						concurrent := &corev1.ConfigMap{}
						if err := c.Get(ctx, journalKey, concurrent); err != nil {
							return err
						}
						concurrent.Annotations["example.com/concurrent"] = "retained"
						if err := c.Update(ctx, concurrent); err != nil {
							return err
						}
					}
					return c.Update(ctx, object, opts...)
				},
				Delete: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.DeleteOption) error {
					if _, binding := object.(*rbacv1.RoleBinding); binding && failure == "revoke" {
						return injected
					}
					if _, journal := object.(*corev1.ConfigMap); journal && client.ObjectKeyFromObject(object) == journalKey &&
						(failure == "delete" || failure == "delete-lost-ack") {
						if failure == "delete-lost-ack" {
							if err := c.Delete(ctx, object, opts...); err != nil {
								return err
							}
						}
						return injected
					}
					return c.Delete(ctx, object, opts...)
				},
			})
			if err := f.kube.Delete(ctx, agent); err != nil {
				t.Fatal(err)
			}
			var cleanupErr error
			for range 3 {
				if _, cleanupErr = f.reconcile(); cleanupErr != nil {
					break
				}
			}
			switch failure {
			case "release-conflict":
				if !apierrors.IsConflict(cleanupErr) {
					t.Fatal("journal release did not preserve the optimistic-lock conflict:", cleanupErr)
				}
			case "foreign-owner":
				if !errors.Is(cleanupErr, sandboxpolicy.ErrOwnership) {
					t.Fatal("foreign journal ownership did not block finalization:", cleanupErr)
				}
			default:
				if !errors.Is(cleanupErr, injected) {
					t.Fatal("journal cleanup failure was not surfaced:", cleanupErr)
				}
			}
			current := f.current(t)
			if !slices.Contains(current.Finalizers, api.Finalizer) || !slices.Contains(current.Finalizers, sandbox.BootstrapFinalizer) {
				t.Fatal("grant cleanup failure discarded Agent or bootstrap protection")
			}
			workload := &appsv1.StatefulSet{}
			workloadErr := f.kube.Get(ctx, client.ObjectKey{
				Namespace: instanceNamespace(agent.Name), Name: instanceServiceName(agent.Name),
			}, workload)
			if workloadErr != nil && !apierrors.IsNotFound(workloadErr) ||
				workloadErr == nil && ptr.Deref(workload.Spec.Replicas, -1) != 0 {
				t.Fatal("grant cleanup failure skipped independent workload shutdown")
			}
			if err := f.kube.Get(ctx, client.ObjectKey{Namespace: instanceNamespace(agent.Name), Name: virtualServiceName(agent.Name)},
				&istiov1beta1.VirtualService{}); !apierrors.IsNotFound(err) {
				t.Fatal("grant cleanup failure skipped route withdrawal:", err)
			}
			if failure != "delete-lost-ack" {
				journal := &corev1.ConfigMap{}
				if err := f.kube.Get(ctx, journalKey, journal); err != nil || journal.UID != original.UID ||
					!slices.Contains(journal.Finalizers, sandboxpolicy.GrantJournalFinalizer) {
					t.Fatal("failed cleanup lost journal ownership or protection:", err)
				}
				if failure == "revoke" || failure == "foreign-owner" {
					if journal.Data[sandboxpolicy.GrantJournalDataKey] != original.Data[sandboxpolicy.GrantJournalDataKey] {
						t.Fatal("failed revocation or foreign ownership discarded recorded grant history")
					}
				}
				if failure == "foreign-owner" {
					journal.OwnerReferences = original.OwnerReferences
					if err := f.kube.Update(ctx, journal); err != nil {
						t.Fatal(err)
					}
				}
			}
			f.r.Client = f.kube
			for range 3 {
				f.reconcileOK(t)
			}
			if err := f.kube.Get(ctx, journalKey, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
				t.Fatal("recovered cleanup retained the grant journal:", err)
			}
			if err := f.kube.Get(ctx, client.ObjectKeyFromObject(agent), &api.Agent{}); !apierrors.IsNotFound(err) {
				t.Fatal("recovered cleanup retained the Agent:", err)
			}
		})
	}
}

func TestReconciledSandboxSubmissionOutcomeControlsCancellation(t *testing.T) {
	for _, scenario := range []string{"submitted", "legacy", "lost-expectation", "lost-submit-ack", "failed-submit-write", "cancellation-conflict", "remaining-pod"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, "openclaw", true, false,
				withReconciledCoordination(t))
			installer := f.r.HelmClient.(*sharedSandboxInstaller)
			injected := errors.New("synthetic interrupted apply")
			var prepared *helm.PreparedRelease
			installer.beforeApply = func(value *helm.PreparedRelease) error {
				prepared = value
				if scenario != "cancellation-conflict" && scenario != "remaining-pod" {
					if err := value.AuthorizeSandboxApply(ctx); err != nil {
						return err
					}
				}
				return injected
			}
			if scenario == "lost-submit-ack" || scenario == "failed-submit-write" {
				f.r.Client = interceptor.NewClient(f.kube, interceptor.Funcs{
					Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
						if state, ok := object.(*corev1.ConfigMap); ok && state.Annotations[sandboxApplyStateAnnotation] == sandboxApplySubmitted {
							if scenario == "failed-submit-write" {
								return injected
							}
							if err := c.Patch(ctx, object, patch, opts...); err != nil {
								return err
							}
							return injected
						}
						return c.Patch(ctx, object, patch, opts...)
					},
				})
			}
			if _, err := f.reconcile(); !errors.Is(err, injected) || prepared == nil || f.installer.stages != 0 {
				t.Fatal("fixture did not interrupt the intended submission boundary:", err)
			}
			f.r.Client = f.kube
			agent := f.current(t)
			expected := reconciledExpectationForTest(t, f)
			state := &corev1.ConfigMap{}
			stateKey := client.ObjectKey{Namespace: f.r.OperatorNamespace, Name: w.PodExpectationName(expected.Namespace.Name, expected.Namespace.UID)}
			if err := f.kube.Get(ctx, stateKey, state); err != nil {
				t.Fatal(err)
			}
			if scenario == "legacy" {
				delete(state.Annotations, sandboxApplyStateAnnotation)
				if err := f.kube.Update(ctx, state); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "lost-expectation" {
				if err := f.kube.Delete(ctx, state); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "submitted" || scenario == "legacy" || scenario == "lost-expectation" || scenario == "lost-submit-ack" {
				previousState := state.Annotations[sandboxApplyStateAnnotation]
				if scenario == "lost-expectation" {
					previousState = ""
				}
				installer.beforeApply = func(value *helm.PreparedRelease) error {
					prepared = value
					return injected
				}
				if _, err := f.reconcile(); !errors.Is(err, injected) {
					t.Fatal("retry did not reach the intended apply boundary:", err)
				}
				if err := f.kube.Get(ctx, stateKey, state); err != nil ||
					state.Annotations[sandboxApplyStateAnnotation] != previousState {
					t.Fatal("retry downgraded an uncertain submission to pending:", err)
				}
			}
			if scenario == "cancellation-conflict" {
				f.r.Client = interceptor.NewClient(f.kube, interceptor.Funcs{
					Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
						if state, ok := object.(*corev1.ConfigMap); ok && state.Annotations[sandboxApplyStateAnnotation] == sandboxApplyCancelled {
							concurrent := &corev1.ConfigMap{}
							if err := c.Get(ctx, client.ObjectKeyFromObject(state), concurrent); err != nil {
								return err
							}
							// Model a submission that passed its source guard before
							// Stopping and won the expectation CAS during cancellation.
							concurrent.Annotations[sandboxApplyStateAnnotation] = sandboxApplySubmitted
							if err := c.Update(ctx, concurrent); err != nil {
								return err
							}
						}
						return c.Patch(ctx, object, patch, opts...)
					},
				})
			}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "synthetic-unexpected-pod", Namespace: expected.Namespace.Name,
			}}
			if scenario == "remaining-pod" {
				if err := f.kube.Create(ctx, pod); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.kube.Delete(ctx, agent); err != nil {
				t.Fatal(err)
			}
			_, err := f.reconcile()
			ticket, found, readErr := f.r.SandboxNativeCoordination.ReadNative(ctx, agent.UID)
			if scenario == "failed-submit-write" {
				if err != nil || readErr != nil || found {
					t.Fatal("confirmed unsubmitted apply could not cancel:", err, readErr)
				}
			} else if err == nil || readErr != nil || !found || ticket.Reservation.Phase != sandboxcoordination.Stopping {
				t.Fatal("uncertain submission or incomplete quiescence released the reservation:", err, readErr)
			}
			if scenario == "cancellation-conflict" && !apierrors.IsConflict(err) {
				t.Fatal("cancellation failed to enforce its resource-version CAS:", err)
			}
			if err := prepared.AuthorizeSandboxApply(ctx); err == nil {
				t.Fatal("stopped or cancelled preparation still authorized an apply")
			}
			if scenario == "remaining-pod" {
				if err := f.kube.Delete(ctx, pod); err != nil {
					t.Fatal(err)
				}
				for range 3 {
					f.reconcileOK(t)
				}
				if _, found, err := f.r.SandboxNativeCoordination.ReadNative(ctx, agent.UID); err != nil || found {
					t.Fatal("confirmed cancellation did not finish after ordinary Pod cleanup:", err)
				}
			}
		})
	}
}

func TestReconciledSandboxPublicationPrecedesSourceReservation(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, written := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/write-ack-lost-%t", kind, written), func(t *testing.T) {
				ctx := context.Background()
				f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, kind, true, false,
					withReconciledCoordination(t))
				attempted := false
				f.r.Client = interceptor.NewClient(f.kube, interceptor.Funcs{
					Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
						if state, ok := object.(*corev1.ConfigMap); ok && state.Data[w.PodExpectationKey] != "" {
							attempted = true
							if written {
								if err := c.Create(ctx, object, opts...); err != nil {
									return err
								}
							}
							return errors.New("synthetic publication failure")
						}
						return c.Create(ctx, object, opts...)
					},
				})
				if _, err := f.reconcile(); err == nil || !attempted || f.installer.stages != 0 {
					t.Fatal("fixture did not fail at expectation publication:", err)
				}
				if _, found, err := f.r.SandboxNativeCoordination.ReadNative(ctx, f.agent.UID); err != nil || found {
					t.Fatal("failed publication acquired a reservation without confirmed cleanup state:", err)
				}
				f.r.Client = f.kube
				if err := f.kube.Delete(ctx, f.current(t)); err != nil {
					t.Fatal(err)
				}
				for range 3 {
					f.reconcileOK(t)
				}
				if err := f.kube.Get(ctx, client.ObjectKeyFromObject(f.agent), &api.Agent{}); !apierrors.IsNotFound(err) {
					t.Fatal("failed publication blocked ordinary deletion:", err)
				}
			})
		}
	}
}

func TestReconciledSandboxPublicationRetryRetainsCancellation(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, checkpoint := range []string{"expectation", "journal", "initialization"} {
			for _, written := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/write-ack-lost-%t", kind, checkpoint, written), func(t *testing.T) {
					ctx := context.Background()
					f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, kind, true, false,
						withReconciledCoordination(t))
					attempted := false
					injected := errors.New("synthetic initial publication failure")
					f.r.Client = interceptor.NewClient(f.kube, interceptor.Funcs{
						Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
							state, ok := object.(*corev1.ConfigMap)
							if ok && (checkpoint == "expectation" && state.Data[w.PodExpectationKey] != "" ||
								checkpoint == "journal" && state.Data[sandboxpolicy.GrantJournalDataKey] != "") {
								attempted = true
								if written {
									if err := c.Create(ctx, object, opts...); err != nil {
										return err
									}
								}
								return injected
							}
							return c.Create(ctx, object, opts...)
						},
						Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
							if state, ok := object.(*corev1.ConfigMap); ok && checkpoint == "initialization" &&
								state.Data[w.PodExpectationKey] != "" && state.Annotations[sandboxApplyStateAnnotation] == sandboxApplyPending {
								attempted = true
								if written {
									if err := c.Patch(ctx, object, patch, opts...); err != nil {
										return err
									}
								}
								return injected
							}
							return c.Patch(ctx, object, patch, opts...)
						},
					})
					if _, err := f.reconcile(); err == nil || !attempted || f.installer.stages != 0 {
						t.Fatal("fixture did not fail before its initial workload submission:", err)
					}
					f.r.Client = f.kube
					recorded, err := f.r.sandboxGrantJournal().Read(ctx, f.agent.Name, f.agent.UID)
					if err != nil && !apierrors.IsNotFound(err) {
						t.Fatal(err)
					}
					if len(recorded) != 0 || checkpoint == "expectation" && !apierrors.IsNotFound(err) {
						t.Fatal("journal or grants advanced before expectation initialization completed")
					}
					installer := f.r.HelmClient.(*sharedSandboxInstaller)
					installer.beforeApply = func(*helm.PreparedRelease) error {
						return injected
					}
					for range 2 {
						if _, err := f.reconcile(); !errors.Is(err, injected) || f.installer.stages != 0 {
							t.Fatal("publication retry did not reach the pre-submit boundary:", err)
						}
					}
					agent := f.current(t)
					expected := reconciledExpectationForTest(t, f)
					state := &corev1.ConfigMap{}
					key := client.ObjectKey{Namespace: f.r.OperatorNamespace, Name: w.PodExpectationName(expected.Namespace.Name, expected.Namespace.UID)}
					if err := f.kube.Get(ctx, key, state); err != nil {
						t.Fatal(err)
					}
					if state.Annotations[sandboxApplyStateAnnotation] != sandboxApplyPending {
						t.Fatal("retry lost the durable pre-submit checkpoint")
					}
					if _, found, err := f.r.SandboxNativeCoordination.ReadNative(ctx, agent.UID); err != nil || !found {
						t.Fatal("retry did not reserve its selected sources:", err)
					}
					if err := f.kube.Delete(ctx, agent); err != nil {
						t.Fatal(err)
					}
					for range 3 {
						f.reconcileOK(t)
					}
					if _, found, err := f.r.SandboxNativeCoordination.ReadNative(ctx, agent.UID); err != nil || found {
						t.Fatal("cancelled publication retry retained its source reservation:", err)
					}
					if err := f.kube.Get(ctx, client.ObjectKeyFromObject(agent), &api.Agent{}); !apierrors.IsNotFound(err) {
						t.Fatal("cancelled publication retry retained its Agent finalizer:", err)
					}
				})
			}
		}
	}
}

func TestReconciledSandboxDoesNotRecreateMissingJournalAfterInitialization(t *testing.T) {
	ctx := context.Background()
	f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, "openclaw", true, false,
		withReconciledCoordination(t))
	installer := f.r.HelmClient.(*sharedSandboxInstaller)
	injected := errors.New("synthetic pause before submission")
	installer.beforeApply = func(*helm.PreparedRelease) error { return injected }
	if _, err := f.reconcile(); !errors.Is(err, injected) {
		t.Fatal("fixture did not reach the initialized pre-submit checkpoint:", err)
	}
	key := client.ObjectKey{Namespace: f.r.OperatorNamespace, Name: sandboxpolicy.GrantJournalName(f.agent.UID)}
	journal := &corev1.ConfigMap{}
	if err := f.kube.Get(ctx, key, journal); err != nil {
		t.Fatal(err)
	}
	journal.Finalizers = nil
	if err := f.kube.Update(ctx, journal); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Delete(ctx, journal); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reconcile(); err == nil || f.installer.stages != 0 {
		t.Fatal("missing grant history permitted progress:", err)
	}
	if err := f.kube.Get(ctx, key, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Fatal("initialized lifetime recreated missing grant history:", err)
	}
}

func TestReconciledSandboxCancelledPreparationCannotReplayAfterResume(t *testing.T) {
	ctx := context.Background()
	f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, "hermes", true, false,
		withReconciledCoordination(t))
	installer := f.r.HelmClient.(*sharedSandboxInstaller)
	var old *helm.PreparedRelease
	injected := errors.New("synthetic pause before first apply")
	installer.beforeApply = func(prepared *helm.PreparedRelease) error {
		old = prepared
		return injected
	}
	if _, err := f.reconcile(); !errors.Is(err, injected) || old == nil {
		t.Fatal("fixture did not pause at its pre-submit checkpoint:", err)
	}
	expected := reconciledExpectationForTest(t, f)
	agent := f.current(t)
	ticket, found, err := f.r.SandboxNativeCoordination.ReadNative(ctx, agent.UID)
	if err != nil || !found {
		t.Fatal("missing initial reservation:", err)
	}
	agent.Finalizers = append(agent.Finalizers, sandbox.HoldFinalizer)
	if err := f.kube.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if err := f.r.stopNativeSandboxReservation(ctx, agent); err != nil {
		t.Fatal("confirmed pre-submit checkpoint could not be cancelled:", err)
	}
	installer.beforeApply = nil
	for range 3 {
		f.reconcileOK(t)
	}
	next, found, err := f.r.SandboxNativeCoordination.ReadNative(ctx, agent.UID)
	if err != nil || !found || next.Reservation.ID == ticket.Reservation.ID || f.installer.stages != 1 ||
		reconciledExpectationForTest(t, f).Home != expected.Home {
		t.Fatal("cancelled checkpoint could not resume with new authority and the same HOME:", err)
	}
	if err := old.AuthorizeSandboxApply(ctx); err == nil {
		t.Fatal("an old prepared apply reused authority acquired after cancellation")
	}
}

func TestReconciledSandboxStopWaitsForMissingAuxiliaryChildren(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, scenario := range []string{"unobserved-controller", "removed-controller", "remaining-replicaset", "remaining-pod"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				ctx := context.Background()
				f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, kind, false, true,
					withReconciledCoordination(t))
				if scenario != "unobserved-controller" {
					f.reconcileOK(t)
				}
				expected := reconciledExpectationForTest(t, f)
				if scenario == "unobserved-controller" {
					for index := range expected.Workloads {
						if expected.Workloads[index].Kind == "Deployment" {
							expected.Workloads[index].Identity.UID = ""
						}
					}
					unobserved, err := expected.Object(f.r.OperatorNamespace)
					if err != nil {
						t.Fatal(err)
					}
					state := &corev1.ConfigMap{}
					if err := f.kube.Get(ctx, client.ObjectKeyFromObject(unobserved), state); err != nil {
						t.Fatal(err)
					}
					state.Data = unobserved.Data
					if err := f.kube.Update(ctx, state); err != nil {
						t.Fatal(err)
					}
				}
				var auxiliary w.RenderedWorkload
				for _, workload := range expected.Workloads {
					if workload.Kind == "Deployment" {
						auxiliary = workload
					}
				}
				if auxiliary.Identity.Name == "" ||
					(auxiliary.Identity.UID == "") != (scenario == "unobserved-controller") {
					t.Fatal("fixture did not retain the requested controller-observation boundary")
				}
				agent := f.current(t)
				keyID := agent.Status.AIGateway.KeyID
				store := f.r.SandboxNativeCoordination
				ticket, found, err := store.ReadNative(ctx, agent.UID)
				if err != nil || !found {
					t.Fatal("missing source reservation:", err)
				}
				if _, err := store.BeginNativeStop(ctx, ticket); err != nil {
					t.Fatal(err)
				}
				main := f.workload(t)
				main.Spec.Replicas = ptr.To[int32](0)
				if err := f.kube.Update(ctx, main); err != nil {
					t.Fatal(err)
				}
				main.Status.ObservedGeneration, main.Status.Replicas = main.Generation, 0
				if err := f.kube.Status().Update(ctx, main); err != nil {
					t.Fatal(err)
				}
				proxy := &appsv1.Deployment{}
				if err := f.kube.Get(ctx, client.ObjectKey{
					Namespace: expected.Namespace.Name, Name: auxiliary.Identity.Name,
				}, proxy); err != nil {
					t.Fatal(err)
				}
				if err := f.kube.Delete(ctx, proxy); err != nil {
					t.Fatal(err)
				}
				var child client.Object
				switch scenario {
				case "remaining-replicaset":
					child = &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
						Name: "old-proxy-replicaset", Namespace: expected.Namespace.Name,
						OwnerReferences: []metav1.OwnerReference{{
							APIVersion: "apps/v1", Kind: "Deployment", Name: proxy.Name, UID: proxy.UID, Controller: ptr.To(true),
						}},
					}, Spec: appsv1.ReplicaSetSpec{Replicas: ptr.To[int32](1)}}
				case "remaining-pod":
					child = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
						Name: "late-proxy-pod", Namespace: expected.Namespace.Name,
					}, Status: corev1.PodStatus{Phase: corev1.PodPending}}
				}
				if child != nil {
					if err := f.kube.Create(ctx, child); err != nil {
						t.Fatal(err)
					}
					if _, err := f.reconcile(); err == nil {
						t.Fatal("controller absence concealed a remaining ReplicaSet or Pod")
					}
					current, found, err := store.ReadNative(ctx, agent.UID)
					if err != nil || !found || current.Reservation.Phase != sandboxcoordination.Stopping ||
						current.Reservation.ID != ticket.Reservation.ID {
						t.Fatal("remaining child released or replaced its source reservation:", err)
					}
					if err := f.kube.Delete(ctx, child); err != nil {
						t.Fatal(err)
					}
				}
				for range 4 {
					f.reconcileOK(t)
				}
				current, found, err := store.ReadNative(ctx, agent.UID)
				if err != nil || !found || current.Reservation.Phase != sandboxcoordination.Reserved ||
					current.Reservation.ID == ticket.Reservation.ID ||
					reconciledExpectationForTest(t, f).Home != expected.Home ||
					f.current(t).Status.AIGateway.KeyID != keyID || len(f.gateway.generated) != 1 {
					t.Fatal("observed auxiliary removal could not resume with retained identity:", err)
				}
			})
		}
	}
}

func reconciledExpectationForTest(t *testing.T, f *nativeSandboxFixture) w.ReconcileExpectation {
	t.Helper()
	ctx := context.Background()
	ns := &corev1.Namespace{}
	if err := f.kube.Get(ctx, client.ObjectKey{Name: instanceNamespace(f.agent.Name)}, ns); err != nil {
		t.Fatal(err)
	}
	state := &corev1.ConfigMap{}
	if err := f.kube.Get(ctx, client.ObjectKey{Namespace: f.r.OperatorNamespace, Name: w.PodExpectationName(ns.Name, ns.UID)}, state); err != nil {
		t.Fatal(err)
	}
	expected, err := w.ReadReconcileExpectation(state)
	if err != nil {
		t.Fatal(err)
	}
	return expected
}

func TestReconciledSandboxCommonModelCombinations(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, models := range []struct {
			name            string
			shared, copilot bool
		}{{"shared", true, false}, {"copilot", false, true}, {"mixed", true, true}} {
			t.Run(kind+"/"+models.name, func(t *testing.T) {
				f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, kind, models.shared, models.copilot)
				ctx := context.Background()
				for range 3 {
					f.reconcileOK(t)
				}
				expected := reconciledExpectationForTest(t, f)
				count := 1
				if models.copilot {
					count++
				}
				if len(expected.Workloads) != count {
					t.Fatalf("common inventory contains %d workloads, want %d", len(expected.Workloads), count)
				}
				for _, workload := range expected.Workloads {
					if workload.Replicas != 1 || len(workload.Template.Finalizers) != 0 {
						t.Fatalf("%s was unnecessarily held before model sign-in", workload.Kind)
					}
					if workload.Kind == "StatefulSet" {
						container := workload.Template.Spec.Containers[0]
						for _, probe := range []*corev1.Probe{container.StartupProbe, container.ReadinessProbe, container.LivenessProbe} {
							if probe == nil || probe.Exec == nil || probe.HTTPGet != nil {
								t.Fatal("protected readiness must not require forbidden host-to-Pod ingress")
							}
							if kind == "hermes" {
								command := []string{"node", "/etc/cetirun/hermes-health-probe.cjs", "--timeout"}
								if probe == container.ReadinessProbe {
									command = []string{"/usr/local/bin/hermes-python", "/etc/cetirun/hermes-runtime.py", "readiness", "--timeout"}
								}
								if len(probe.Exec.Command) != len(command)+1 || !slices.Equal(probe.Exec.Command[:len(command)], command) {
									t.Fatal("protected Hermes probe did not use the managed API/Webapp health check")
								}
							} else if len(probe.Exec.Command) != 5 || probe.Exec.Command[2] != fmt.Sprint(*f.current(t).Spec.Agent.Port) {
								t.Fatal("protected readiness did not use the actual resolved runtime port")
							}
						}
					}
				}
				if !models.copilot {
					return
				}
				agent := f.current(t)
				if agent.Status.AIGateway.Copilot == nil || agent.Status.AIGateway.Copilot.Phase != api.AIGatewayCopilotPhaseSignInRequired {
					t.Fatal("pre-login state did not preserve the common Portal sign-in workflow")
				}
				token := &corev1.Secret{}
				if err := f.kube.Get(ctx, client.ObjectKey{Namespace: expected.Namespace.Name, Name: api.CopilotTokenSecretName}, token); err != nil {
					t.Fatal(err)
				}
				token.Data = map[string][]byte{api.CopilotTokenSecretKey: []byte("gho_synthetic")}
				token.Annotations = map[string]string{
					api.AnnotationCopilotSignInGeneration: "0a1b2c3d",
					api.AnnotationCopilotSignInCompleted:  "0a1b2c3d",
				}
				if err := f.kube.Update(ctx, token); err != nil {
					t.Fatal(err)
				}
				agent.Annotations[api.AnnotationCopilotModels] = testCopilotModels
				agent.Annotations[api.AnnotationCopilotSignInGeneration] = "0a1b2c3d"
				if err := f.kube.Update(ctx, agent); err != nil {
					t.Fatal(err)
				}
				for _, workload := range expected.Workloads {
					if workload.Kind != "Deployment" {
						continue
					}
					proxy := &appsv1.Deployment{}
					if err := f.kube.Get(ctx, client.ObjectKey{Namespace: expected.Namespace.Name, Name: workload.Identity.Name}, proxy); err != nil {
						t.Fatal(err)
					}
					proxy.Status.ReadyReplicas = 1
					if err := f.kube.Status().Update(ctx, proxy); err != nil {
						t.Fatal(err)
					}
				}
				f.reconcileOK(t)
				updated := reconciledExpectationForTest(t, f)
				if updated.Home != expected.Home || updated.PlanHash == expected.PlanHash {
					t.Fatal("completed sign-in did not update the common deployment while preserving HOME")
				}
				if f.current(t).Status.AIGateway.Copilot.Phase != api.AIGatewayCopilotPhaseReady {
					t.Fatal("completed synthetic sign-in did not register Copilot through the common Gateway reconciler")
				}
				for _, workload := range updated.Workloads {
					if workload.Kind == "Deployment" && workload.Template.Annotations[api.AnnotationCopilotSignInGeneration] != "0a1b2c3d" {
						t.Fatal("common proxy template lost the sign-in generation")
					}
				}
			})
		}
	}
}
