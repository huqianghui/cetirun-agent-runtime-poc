// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package controller

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	sandboxadmission "github.com/Azure/CetiRun/app/cetirun-operator/internal/admission"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/internal/registryauth"
	"github.com/go-logr/logr"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	kyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	"sigs.k8s.io/yaml"
)

// CRDs, authentication, authorization, admission, UID/defaulting and controller
// writes are served by an isolated API server. Only runtime Helm observations
// and scheduler/kubelet/Deployment status are synthetic; no containers run.
func TestCetiAdminInstallationAPIServer(t *testing.T) {
	ctrl.SetLogger(logr.Discard())
	t.Run("operator-role-delegation", func(t *testing.T) {
		h := newCetiAdminInstallationAPI(t, true)
		_, namespace := h.target(t, "delegation", "engineering", api.AgentTypeOpenClaw, true)
		cluster, namespaced, err := h.installer.diagnosticRules()
		if err != nil {
			t.Fatal(err)
		}
		// These unbound probe roles test Kubernetes' privilege-escalation checks.
		// They do not authorize a machine or substitute for cold installation.
		for _, object := range []client.Object{
			&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "delegation-probe"}, Rules: append(cluster, cetiadmin.ClusterOperationRules()...)},
			&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "delegation-probe", Namespace: namespace.Name}, Rules: append(namespaced, cetiadmin.NamespaceOperationRules(false)...)},
		} {
			if err := h.operator.Create(h.ctx, object); err != nil {
				t.Errorf("Chart Operator cannot delegate the production %T rules: %v", object, err)
			}
		}
		h.assertPermission(t, h.operator, "escalate", rbacv1.GroupName, "clusterroles", "", "", "unrelated", false)
		h.assertPermission(t, h.operator, "bind", rbacv1.GroupName, "clusterroles", "", "", "cluster-admin", false)
	})
	t.Run("readiness-api-defaulting", func(t *testing.T) {
		h := newCetiAdminInstallationAPI(t, true)
		stored := &admissionregistrationv1.ValidatingWebhookConfiguration{}
		if err := h.operator.Get(h.ctx, client.ObjectKey{Name: cetiAdminAdmissionName}, stored); err != nil {
			t.Fatal(err)
		}
		for _, hook := range stored.Webhooks {
			selector, err := json.Marshal(hook.NamespaceSelector)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("API-persisted %s namespaceSelector=%s", hook.Name, selector)
		}
		if err := h.installer.AdmissionReady(h.ctx); err != nil {
			t.Fatal("production readiness rejected the API-defaulted Chart contract:", err)
		}
	})
	t.Run("aks-namespace-exclusions", func(t *testing.T) {
		h := newCetiAdminInstallationAPI(t, true)
		exclusions := []metav1.LabelSelectorRequirement{
			{Key: "control-plane", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"true"}},
			{Key: "kubernetes.azure.com/managedby", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"aks"}},
		}
		validation := &admissionregistrationv1.ValidatingWebhookConfiguration{}
		if err := h.admin.Get(h.ctx, client.ObjectKey{Name: cetiAdminAdmissionName}, validation); err != nil {
			t.Fatal(err)
		}
		for i := range validation.Webhooks {
			if validation.Webhooks[i].NamespaceSelector == nil {
				validation.Webhooks[i].NamespaceSelector = &metav1.LabelSelector{}
			}
			validation.Webhooks[i].NamespaceSelector.MatchExpressions = exclusions
		}
		if err := h.admin.Update(h.ctx, validation); err != nil {
			t.Fatal(err)
		}
		mutating := &admissionregistrationv1.MutatingWebhookConfiguration{}
		if err := h.admin.Get(h.ctx, client.ObjectKey{Name: cetiAdminAdmissionName}, mutating); err != nil {
			t.Fatal(err)
		}
		mutating.Webhooks[0].NamespaceSelector = &metav1.LabelSelector{MatchExpressions: exclusions}
		if err := h.admin.Update(h.ctx, mutating); err != nil {
			t.Fatal(err)
		}
		var readinessErr error
		if err := wait.PollUntilContextTimeout(h.ctx, 50*time.Millisecond, 15*time.Second, true, func(context.Context) (bool, error) {
			readinessErr = h.installer.AdmissionReady(h.ctx)
			if readinessErr != nil && !strings.Contains(readinessErr.Error(), "has not confirmed rejection") {
				return false, readinessErr
			}
			return readinessErr == nil, nil
		}); err != nil {
			t.Fatal("guarded AKS namespace selectors were rejected:", err, readinessErr)
		}
		_, target := h.target(t, "aks-guard", "engineering", api.AgentTypeOpenClaw, true)
		if target.Labels == nil {
			target.Labels = map[string]string{}
		}
		target.Labels["app.kubernetes.io/managed-by"] = "cetirun-operator"
		if err := h.operator.Update(h.ctx, target); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{h.source.Namespace, target.Name} {
			for _, label := range []string{"control-plane", "kubernetes.azure.com/managedby"} {
				namespace := &corev1.Namespace{}
				if err := h.admin.Get(h.ctx, client.ObjectKey{Name: name}, namespace); err != nil {
					t.Fatal(err)
				}
				if namespace.Labels == nil {
					namespace.Labels = map[string]string{}
				}
				delete(namespace.Labels, "app.kubernetes.io/managed-by")
				namespace.Labels[label] = map[string]string{"control-plane": "true", "kubernetes.azure.com/managedby": "aks"}[label]
				err := h.admin.Update(h.ctx, namespace, client.DryRunAll)
				if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), cetiadmin.NamespaceAdmissionGuardName) {
					t.Fatalf("namespace bypass %s/%s was not rejected by the independent guard: %v", name, label, err)
				}
			}
		}
		newAdminNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-cetiadmin", Labels: map[string]string{"control-plane": "true"}}}
		if err := h.admin.Create(h.ctx, newAdminNamespace, client.DryRunAll); !apierrors.IsForbidden(err) {
			t.Fatalf("excluded CetiAdmin namespace creation was accepted: %v", err)
		}
		unrelated := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "unrelated-aks-system", Labels: map[string]string{"control-plane": "true", "kubernetes.azure.com/managedby": "aks"}}}
		if err := h.admin.Create(h.ctx, unrelated, client.DryRunAll); err != nil {
			t.Fatal("unrelated AKS namespace was changed by CetiAdmin protection:", err)
		}
		h.converge(t)
		self := h.agent(t)
		self.Status.Phase, self.Status.ObservedGeneration = api.PhaseReady, self.Generation
		setCondition(&self.Status.Conditions, api.ConditionReady, metav1.ConditionTrue, "SyntheticRuntimeReady", "Isolated API test runtime observation")
		if err := h.operator.Status().Update(h.ctx, self); err != nil {
			t.Fatal(err)
		}
		h.reconcile(t)
		if state := h.state(t); state.Phase != cetiadmin.PhaseReady {
			t.Fatal("guarded AKS installation did not reach Ready")
		}
		binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{ObjectMeta: metav1.ObjectMeta{Name: cetiadmin.NamespaceAdmissionGuardName}}
		if err := h.admin.Delete(h.ctx, binding); err != nil {
			t.Fatal(err)
		}
		if err := h.installer.AdmissionReady(h.ctx); err == nil {
			t.Fatal("AKS exclusions remained qualified after losing the namespace guard")
		}
	})
	t.Run("disabled-cold-install", func(t *testing.T) {
		h := newCetiAdminInstallationAPI(t, false)
		for range 3 {
			if _, err := h.installer.Reconcile(h.ctx, ctrl.Request{}); err != nil {
				t.Fatal(err)
			}
		}
		state := h.state(t)
		checkpoint := &corev1.ConfigMap{}
		if err := h.operator.Get(h.ctx, h.installer.stateKey(), checkpoint); err != nil || state.RecordUID == "" || state.RecordUID != checkpoint.UID {
			t.Fatalf("disabled checkpoint did not persist its actual record UID: %v", err)
		}
		config, err := cetiadmin.ParseConfig(h.source.Data[cetiadmin.ConfigKey])
		if err != nil {
			t.Fatal(err)
		}
		hash, err := cetiadmin.ConfigHash(config)
		if err != nil {
			t.Fatal(err)
		}
		if state.Phase != cetiadmin.PhaseDisabled || state.ConfigHash != hash || state.Configuration.Enabled || state.CanRun() ||
			state.Agent.UID != "" || state.Team.UID != "" || len(state.Resources) != 0 {
			t.Fatal("disabled cold install did not publish an exact zero-instance checkpoint")
		}
		for _, object := range []client.Object{&api.Agent{}, &api.Team{}, &corev1.Namespace{}} {
			name := config.InstanceName
			if _, ok := object.(*corev1.Namespace); ok {
				name = instanceNamespace(name)
			}
			if err := h.admin.Get(h.ctx, client.ObjectKey{Name: name}, object); !apierrors.IsNotFound(err) {
				t.Fatalf("disabled installation created %T: %v", object, err)
			}
		}
		if h.helm.installCalled {
			t.Fatal("disabled installation invoked runtime Helm")
		}
	})
	t.Run("admission-unready-grants-nothing", func(t *testing.T) {
		h := newCetiAdminInstallationAPI(t, true)
		deployment := &appsv1.Deployment{}
		if err := h.admin.Get(h.ctx, client.ObjectKey{Name: "cetirun-sandbox-admission", Namespace: h.source.Namespace}, deployment); err != nil {
			t.Fatal(err)
		}
		deployment.Status.ReadyReplicas, deployment.Status.AvailableReplicas = 0, 0
		if err := h.admin.Status().Update(h.ctx, deployment); err != nil {
			t.Fatal(err)
		}
		if _, err := h.installer.Reconcile(h.ctx, ctrl.Request{}); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			_, err := h.installer.Reconcile(h.ctx, ctrl.Request{})
			if err == nil || !strings.Contains(err.Error(), "has not completed its current rollout") {
				t.Fatalf("unready admission was not the installation blocker: %v", err)
			}
			state := h.state(t)
			if state.CanRun() || len(state.Resources) != 0 || state.Agent.UID != "" {
				t.Fatal("unready admission allowed Agent creation or API grants")
			}
		}
	})
	t.Run("cold-install-and-delegation", func(t *testing.T) {
		h := newCetiAdminInstallationAPI(t, true)
		target, namespace := h.target(t, "ordinary", "engineering", api.AgentTypeHermes, true)
		manifestObjects := h.targetManifest(t, target, namespace)
		const payload = "synthetic-namespace-secret-not-installation-state"
		h.create(t, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "namespace-secret", Namespace: namespace.Name}, Data: map[string][]byte{"fixture": []byte(payload)}})
		h.create(t, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "controller-credentials", Namespace: h.source.Namespace}, Data: map[string][]byte{"fixture": []byte("synthetic-platform-only")}})
		h.converge(t)
		h.runtimeAccess(t, target.Name)
		state := h.state(t)
		if !state.Bootstrapped || !state.CanRun() || state.BootstrapReleasePending {
			t.Fatalf("cold controller installation did not release its runtime: phase=%s bootstrapped=%v", state.Phase, state.Bootstrapped)
		}
		h.assertRecordedUIDs(t, state)
		self := h.agent(t)
		if self.Spec.Suspended || self.Spec.Agent.Type != api.AgentTypeHermes || self.Spec.Agent.Image == nil || self.Spec.Agent.Port == nil {
			t.Fatal("bootstrap did not release only the initial suspension with paired Hermes coordinates")
		}
		if _, managed, err := h.installer.BindingForAgent(h.ctx, self); !managed || err != nil {
			t.Fatalf("fresh installation does not authorize its runtime: managed=%v err=%v", managed, err)
		}
		machine := h.machine(t)
		if err := machine.List(h.ctx, &corev1.PodList{}, client.InNamespace(namespace.Name)); err != nil {
			t.Fatal("installed roles did not allow target Pod diagnostics:", err)
		}
		secrets := &corev1.SecretList{}
		if err := machine.List(h.ctx, secrets, client.InNamespace(namespace.Name)); err != nil {
			t.Fatal("installed roles did not allow the explicitly accepted namespace Secret-list scope:", err)
		}
		if len(secrets.Items) != 1 || string(secrets.Items[0].Data["fixture"]) != payload {
			t.Fatal("test did not exercise the documented namespace Secret-list disclosure")
		}
		for _, caller := range []client.Client{machine, h.admission} {
			if err := caller.Get(h.ctx, client.ObjectKey{Name: "controller-credentials", Namespace: h.source.Namespace}, &corev1.Secret{}); !apierrors.IsForbidden(err) {
				t.Fatalf("runtime access disclosed a non-Agent platform Secret: %v", err)
			}
		}
		if err := machine.List(h.ctx, &corev1.SecretList{}, client.InNamespace(h.source.Namespace)); !apierrors.IsForbidden(err) {
			t.Fatalf("platform diagnostics disclosed namespace Secrets: %v", err)
		}
		if err := machine.List(h.ctx, &corev1.PodList{}, client.InNamespace(h.source.Namespace)); err != nil {
			t.Fatal("platform read namespace lost non-secret diagnostics:", err)
		}
		before := target.DeepCopy()
		target.Spec.Suspended = true
		if err := machine.Patch(h.ctx, target, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			t.Fatal("installed machine cannot perform the approved ordinary Agent lifecycle mutation:", err)
		}
		h.assertPermission(t, machine, "get", "", "pods", "", namespace.Name, "", true)
		h.assertPermission(t, machine, "create", "", "pods", "exec", namespace.Name, "ordinary-0", true)
		h.assertPermission(t, machine, "delete", "", "secrets", "", namespace.Name, "native-auth", false)
		h.assertPermission(t, machine, "delete", api.GroupVersion.Group, "agents", "", "", target.Name, false)
		h.assertPermission(t, machine, "delete", "", "namespaces", "", "", namespace.Name, false)
		h.assertPermission(t, machine, "delete", "", "persistentvolumeclaims", "", namespace.Name, "home", false)
		h.assertPermission(t, machine, "create", rbacv1.GroupName, "roles", "", namespace.Name, "", false)
		h.assertPermission(t, machine, "create", "", "serviceaccounts", "token", namespace.Name, "default", false)
		h.assertPermission(t, machine, "get", "", "secrets", "", h.source.Namespace, "controller-credentials", false)
		h.assertPermission(t, machine, "patch", "", "configmaps", "", h.source.Namespace, h.installer.stateKey().Name, false)
		h.assertPermission(t, machine, "impersonate", "", "serviceaccounts", "", h.source.Namespace, "controller", false)
		h.assertPermission(t, machine, "update", api.GroupVersion.Group, "agents", "", "", self.Name, true)
		selfEdit := self.DeepCopy()
		selfEdit.Spec.Suspended = true
		if err := machine.Update(h.ctx, selfEdit, client.DryRunAll); !apierrors.IsForbidden(err) {
			t.Fatalf("general Agent RBAC bypassed CetiAdmin self-identity protection: %v", err)
		}
		if h.machineUID.Load() != string(state.ServiceAccount.UID) {
			t.Fatal("served mutation did not authenticate the API-issued ServiceAccount UID")
		}
		created := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "machine-created", Annotations: map[string]string{
			api.AnnotationOwnerTenantID: self.Annotations[api.AnnotationOwnerTenantID], api.AnnotationOwnerObjectID: self.Annotations[api.AnnotationOwnerObjectID],
		}}, Spec: api.AgentSpec{Agent: api.HarnessSpec{Type: api.AgentTypeHermes}, User: self.Spec.User, Team: target.Spec.Team, GatewayTokenSecretRef: "native-auth"}}
		if err := machine.Create(h.ctx, created); err != nil || created.UID == "" {
			t.Fatalf("installed machine cannot create an ordinary Agent for a known owner and Team: %v", err)
		}
		if err := h.admin.Get(h.ctx, client.ObjectKey{Name: "cetiadmin-targets", Namespace: state.Namespace.Name}, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
			t.Fatalf("installation still publishes an authorization inventory: %v", err)
		}
		checkpoint := &corev1.ConfigMap{}
		if err := h.operator.Get(h.ctx, h.installer.stateKey(), checkpoint); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(checkpoint.Data[cetiadmin.BindingKey], payload) ||
			strings.Contains(self.Annotations[cetiadmin.AnnotationRecovery], payload) {
			t.Fatal("Secret contents entered installation or recovery state")
		}
		var document map[string]json.RawMessage
		if err := json.Unmarshal([]byte(checkpoint.Data[cetiadmin.BindingKey]), &document); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"targets", "readNamespaces", "accessHash"} {
			if _, present := document[field]; present {
				t.Fatalf("installation persisted the obsolete %s authorization snapshot", field)
			}
		}
		for _, object := range manifestObjects {
			stored := object.DeepCopyObject().(client.Object)
			if err := h.admission.Get(h.ctx, client.ObjectKeyFromObject(object), stored); err != nil || stored.GetUID() != object.GetUID() {
				t.Fatalf("namespace admission reader cannot verify the served runtime UID: %v", err)
			}
		}
		resolvedAgent, resolved, err := cetiadmin.ResolveRuntimeTarget(h.ctx, h.admission, state.Configuration, state.Record, target.Name, namespace.Name)
		if err != nil || resolvedAgent == nil || resolvedAgent.UID != target.UID || resolved.Agent != identityOf(target) ||
			resolved.Namespace != identityOf(namespace) || len(resolved.Resources) != 0 {
			t.Fatalf("current API ownership did not resolve without a persistent resource inventory: %v", err)
		}
		unrelated := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "not-in-manifest", Namespace: namespace.Name}, Data: map[string]string{"fixture": "unrelated"}}
		h.create(t, unrelated)
		if err := h.admission.Get(h.ctx, client.ObjectKeyFromObject(unrelated), &corev1.ConfigMap{}); err != nil {
			t.Fatalf("current namespace ownership checks cannot read an unregistered object: %v", err)
		}
		unrelated.Data["fixture"] = "unauthorized-change"
		if err := machine.Update(h.ctx, unrelated, client.DryRunAll); !apierrors.IsForbidden(err) {
			t.Fatalf("namespace permission allowed a write without runtime ownership or maintenance: %v", err)
		}
		self.Status.Phase, self.Status.ObservedGeneration = api.PhaseReady, self.Generation
		if err := h.operator.Status().Update(h.ctx, self); err != nil {
			t.Fatal("Operator could not save the synthetic runtime Ready observation:", err)
		}
		h.reconcile(t)
		if h.state(t).Phase != cetiadmin.PhaseReady {
			t.Fatal("installation ignored the current served runtime status")
		}
	})
	for _, kind := range []string{"Agent", "Team", "Namespace", "ServiceAccount"} {
		t.Run("foreign-same-name-"+kind, func(t *testing.T) {
			h := newCetiAdminInstallationAPI(t, true)
			var foreign client.Object
			for attempt := 0; attempt < 16 && foreign == nil; attempt++ {
				if kind == "Agent" || kind == "Team" || kind == "Namespace" || (attempt > 0 && h.state(t).Namespace.UID != "") {
					switch kind {
					case "Agent":
						foreign = &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "cetiadmin"}, Spec: api.AgentSpec{Agent: api.HarnessSpec{Type: api.AgentTypeHermes}, User: api.UserSpec{Alias: "unrelated-owner"}, GatewayTokenSecretRef: "foreign-auth"}}
					case "Team":
						foreign = &api.Team{ObjectMeta: metav1.ObjectMeta{Name: "cetiadmin"}}
					case "Namespace":
						foreign = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-cetiadmin"}}
					case "ServiceAccount":
						foreign = &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "openclaw-cetiadmin-cetirun", Namespace: "cetirun-cetiadmin"}, AutomountServiceAccountToken: ptr.To(false)}
					}
					foreign.SetAnnotations(map[string]string{"example.com/preserved": "foreign"})
					// The dedicated operator may create this name, but this object
					// deliberately has no installation or creation receipt.
					if err := h.operator.Create(h.ctx, foreign); err != nil {
						t.Fatal(err)
					}
					break
				}
				h.reconcile(t)
			}
			if foreign == nil {
				t.Fatal("controller did not reach the namespace bootstrap stage")
			}
			before := foreign.DeepCopyObject().(client.Object)
			var conflict error
			for range 16 {
				_, conflict = h.installer.Reconcile(h.ctx, ctrl.Request{})
				if conflict != nil {
					break
				}
			}
			if !errors.Is(conflict, cetiadmin.ErrConflict) {
				t.Fatalf("same-name %s did not stop on an ownership conflict: %v", kind, conflict)
			}
			if err := h.admin.Get(h.ctx, client.ObjectKeyFromObject(foreign), foreign); err != nil || !reflect.DeepEqual(before, foreign) {
				t.Fatalf("foreign %s was adopted, modified or deleted: %v", kind, err)
			}
			state := h.state(t)
			if state.CanRun() || len(state.Resources) != 0 {
				t.Fatal("conflicted installation retained API authority")
			}
		})
	}
	t.Run("bootstrap-rejects-user-lifecycle", func(t *testing.T) {
		h := newCetiAdminInstallationAPI(t, true)
		for attempt := 0; attempt < 16; attempt++ {
			state := h.reconcile(t)
			if state.Agent.UID == "" {
				continue
			}
			if state.Bootstrapped || state.CanRun() {
				t.Fatal("test missed the controller-created bootstrap hold")
			}
			agent := h.agent(t)
			for _, edit := range []func(*api.Agent){
				func(a *api.Agent) { a.Spec.Suspended = false },
				func(a *api.Agent) { a.Spec.RestartRequestID = "bootstrap-restart-12345678" },
			} {
				candidate := agent.DeepCopy()
				edit(candidate)
				err := h.admin.Patch(h.ctx, candidate, client.MergeFromWithOptions(agent, client.MergeFromWithOptimisticLock{}), client.DryRunAll)
				if !apierrors.IsForbidden(err) && !(apierrors.IsInvalid(err) && strings.Contains(err.Error(), "a suspended Agent cannot request a new restart")) {
					t.Errorf("bootstrap lifecycle was not rejected by its admission or schema rule: %v", err)
				}
			}
			h.converge(t)
			return
		}
		t.Fatal("controller did not create its suspended bootstrap Agent")
	})
	t.Run("lost-agent-create-resumes-defaulted-object", func(t *testing.T) {
		h := newCetiAdminInstallationAPI(t, true)
		lost := &cetiAdminLostCreateClient{Client: h.operator}
		h.installer.Client = lost
		var last error
		for range 32 {
			_, last = h.installer.Reconcile(h.ctx, ctrl.Request{})
			state := h.state(t)
			if last == nil && state.Bootstrapped && state.CanRun() {
				if lost.createdUID == "" || string(state.Agent.UID) != lost.createdUID {
					t.Fatal("lost CREATE retry did not preserve the actual created Agent UID")
				}
				h.assertRecordedUIDs(t, state)
				return
			}
			if last != nil && !lost.lost {
				t.Fatalf("installation failed before the injected response loss: %v", last)
			}
		}
		state := h.state(t)
		t.Fatalf("lost API CREATE could not resume the defaulted object: phase=%s pending=%s recordedUID=%s createdUID=%s err=%v", state.Phase, state.PendingCreate, state.Agent.UID, lost.createdUID, last)
	})
	t.Run("new-agents-and-teams-preserve-running-authority", func(t *testing.T) {
		h := newCetiAdminInstallationAPI(t, true)
		existing, existingNS := h.target(t, "existing", "engineering", api.AgentTypeHermes, true)
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "existing-secret", Namespace: existingNS.Name}, Data: map[string][]byte{"fixture": []byte("retained-access")}}
		h.create(t, secret)
		h.converge(t)
		h.runtimeAccess(t, existing.Name)
		original := h.state(t)
		self := h.agent(t)
		projection := h.runtimeProjection(t)
		machine := h.machine(t)
		clusterBindings := &rbacv1.ClusterRoleBindingList{}
		if err := h.operator.List(h.ctx, clusterBindings); err != nil {
			t.Fatal(err)
		}
		var retainedClusterBindings []rbacv1.ClusterRoleBinding
		for _, binding := range clusterBindings.Items {
			if binding.Annotations[cetiadmin.AnnotationInstallationUID] == string(original.RecordUID) {
				retainedClusterBindings = append(retainedClusterBindings, binding)
			}
		}
		namespaceBindings := &rbacv1.RoleBindingList{}
		if err := h.operator.List(h.ctx, namespaceBindings, client.InNamespace(existingNS.Name)); err != nil {
			t.Fatal(err)
		}
		if len(retainedClusterBindings) == 0 || len(namespaceBindings.Items) == 0 {
			t.Fatal("test requires API-created cluster and existing namespace bindings")
		}
		assertRunning := func(stage string) {
			t.Helper()
			current, currentAgent := h.state(t), h.agent(t)
			if !current.CanRun() || current.Record != original.Record || current.RecordUID != original.RecordUID ||
				currentAgent.UID != self.UID || currentAgent.Generation != self.Generation || !reflect.DeepEqual(currentAgent.Spec, self.Spec) ||
				!reflect.DeepEqual(h.runtimeProjection(t), projection) {
				t.Fatalf("%s changed the Admin runtime identity, intent or binding", stage)
			}
			for _, before := range retainedClusterBindings {
				after := &rbacv1.ClusterRoleBinding{}
				if err := h.operator.Get(h.ctx, client.ObjectKeyFromObject(&before), after); err != nil || after.UID != before.UID || after.RoleRef != before.RoleRef || !reflect.DeepEqual(after.Subjects, before.Subjects) {
					t.Fatalf("%s replaced or withdrew an existing ClusterRoleBinding %s: %v", stage, before.Name, err)
				}
			}
			for _, before := range namespaceBindings.Items {
				after := &rbacv1.RoleBinding{}
				if err := h.operator.Get(h.ctx, client.ObjectKeyFromObject(&before), after); err != nil || after.UID != before.UID || after.RoleRef != before.RoleRef || !reflect.DeepEqual(after.Subjects, before.Subjects) {
					t.Fatalf("%s replaced or withdrew the existing namespace grant %s: %v", stage, before.Name, err)
				}
			}
			stored := &corev1.Secret{}
			if err := machine.Get(h.ctx, client.ObjectKeyFromObject(secret), stored); err != nil || !bytes.Equal(stored.Data["fixture"], secret.Data["fixture"]) {
				t.Fatalf("%s revoked existing served Secret access: %v", stage, err)
			}
			pod := &corev1.Pod{}
			if err := h.operator.Get(h.ctx, client.ObjectKeyFromObject(h.machinePod), pod); err != nil || pod.UID != h.machinePod.UID || pod.DeletionTimestamp != nil {
				t.Fatalf("%s stopped or replaced the Admin runtime Pod: %v", stage, err)
			}
		}
		assertRunning("initial installation")
		h.assertPermission(t, machine, "update", api.GroupVersion.Group, "agents", "", "", "late-arrival", true)
		late, lateNS := h.target(t, "late-arrival", "research", api.AgentTypeOpenClaw, true)
		pending := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "not-started"}, Spec: api.AgentSpec{
			Agent: api.HarnessSpec{Type: api.AgentTypeHermes}, User: api.UserSpec{Alias: "target-owner"}, Team: "research", GatewayTokenSecretRef: "native-auth"}}
		h.create(t, pending)
		// Neither the installer nor namespace-access reconciliation has seen these Agents.
		for _, target := range []*api.Agent{late, pending} {
			current := &api.Agent{}
			if err := machine.Get(h.ctx, client.ObjectKeyFromObject(target), current); err != nil {
				t.Fatal(err)
			}
			current.Spec.Model = "corrected-model"
			if err := machine.Update(h.ctx, current); err != nil {
				t.Fatalf("unregistered Agent %s cannot be updated through served RBAC and admission: %v", target.Name, err)
			}
			stored := &api.Agent{}
			if err := h.operator.Get(h.ctx, client.ObjectKeyFromObject(target), stored); err != nil || stored.UID != target.UID || stored.Spec.Model != "corrected-model" {
				t.Fatalf("unregistered Agent intent was not persisted: %v", err)
			}
		}
		if err := h.admin.Get(h.ctx, client.ObjectKey{Name: instanceNamespace(pending.Name)}, &corev1.Namespace{}); !apierrors.IsNotFound(err) {
			t.Fatalf("incomplete-runtime regression unexpectedly acquired a namespace: %v", err)
		}
		assertRunning("Agent creation and intent updates in another Team")
		lateSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "new-secret", Namespace: lateNS.Name}, Data: map[string][]byte{"fixture": []byte("new-access")}}
		h.create(t, lateSecret)
		if err := machine.Get(h.ctx, client.ObjectKeyFromObject(lateSecret), &corev1.Secret{}); !apierrors.IsForbidden(err) {
			t.Fatalf("unreconciled namespace already had a cluster-wide Secret grant: %v", err)
		}
		h.runtimeAccess(t, late.Name, pending.Name)
		storedSecret := &corev1.Secret{}
		if err := machine.Get(h.ctx, client.ObjectKeyFromObject(lateSecret), storedSecret); err != nil || !bytes.Equal(storedSecret.Data["fixture"], lateSecret.Data["fixture"]) {
			t.Fatalf("ordinary Agent reconciliation did not grant the new runtime's Secret access: %v", err)
		}
		h.assertPermission(t, machine, "create", "", "pods", "exec", lateNS.Name, "runtime-pod", true)
		assertRunning("new namespace grant without an installer refresh")
		current := &api.Agent{}
		if err := h.operator.Get(h.ctx, client.ObjectKeyFromObject(existing), current); err != nil {
			t.Fatal(err)
		}
		before := current.DeepCopy()
		current.Spec.Team = "research"
		if err := h.operator.Patch(h.ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			t.Fatal(err)
		}
		h.runtimeAccess(t, existing.Name)
		for range 2 {
			h.reconcile(t)
			assertRunning("Team reassignment and installer reconciliation")
		}
		if err := h.admin.Delete(h.ctx, late, client.Preconditions{UID: &late.UID}); err != nil {
			t.Fatal(err)
		}
		h.runtimeAccess(t, late.Name)
		if err := machine.Get(h.ctx, client.ObjectKeyFromObject(lateSecret), &corev1.Secret{}); !apierrors.IsForbidden(err) {
			t.Fatalf("deleted Agent retained its namespace Secret access: %v", err)
		}
		h.reconcile(t)
		assertRunning("another Agent deletion and grant cleanup")
	})
	for _, paused := range []bool{false, true} {
		t.Run(fmt.Sprintf("disable-reenable-user-paused-%v", paused), func(t *testing.T) {
			h := newCetiAdminInstallationAPI(t, true)
			target, targetNS := h.target(t, "ordinary", "engineering", api.AgentTypeHermes, true)
			h.converge(t)
			h.runtimeAccess(t, target.Name)
			original := h.state(t)
			machine := h.machine(t)
			h.assertPermission(t, machine, "list", "", "pods", "", targetNS.Name, "", true)
			home := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "retained-home", Namespace: original.Namespace.Name}, Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
			}}
			h.create(t, home)
			if paused {
				agent := h.agent(t)
				before := agent.DeepCopy()
				agent.Spec.Suspended = true
				if err := h.admin.Patch(h.ctx, agent, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
					t.Fatal("owner pause was rejected after bootstrap:", err)
				}
			}
			h.changeConfig(t, func(config *cetiadmin.Config) { config.Enabled = false })
			state := h.reconcile(t)
			if state.CanRun() || len(state.Resources) != 0 {
				t.Fatal("disable did not withdraw authority before waiting for compute to stop")
			}
			if state.Phase == cetiadmin.PhaseDisabled {
				t.Fatal("disable accepted a still-present unobserved Pod as stopped")
			}
			h.assertPermission(t, machine, "list", "", "pods", "", targetNS.Name, "", false)
			h.observeStoppedPod(t)
			h.until(t, func(s *cetiadmin.Installation) bool {
				return s.Phase == cetiadmin.PhaseDisabled && !s.Configuration.Enabled
			})
			h.changeConfig(t, func(config *cetiadmin.Config) { config.Enabled = true })
			if paused {
				h.until(t, func(s *cetiadmin.Installation) bool {
					return s.Phase == cetiadmin.PhaseDisabled && s.Configuration.Enabled
				})
				h.assertPermission(t, machine, "list", "", "pods", "", targetNS.Name, "", false)
			} else {
				h.converge(t)
				h.runtimeAccess(t, target.Name)
				h.assertPermission(t, machine, "list", "", "pods", "", targetNS.Name, "", true)
			}
			current := h.state(t)
			if h.agent(t).Spec.Suspended != paused || current.Agent != original.Agent || current.Namespace != original.Namespace || current.Team != original.Team || current.ServiceAccount != original.ServiceAccount {
				t.Fatal("component disable/reenable changed user suspension or retained identities")
			}
			retained := &corev1.PersistentVolumeClaim{}
			if err := h.admin.Get(h.ctx, client.ObjectKeyFromObject(home), retained); err != nil || retained.UID != home.UID || retained.DeletionTimestamp != nil {
				t.Fatalf("disable/reenable did not preserve HOME: %v", err)
			}
		})
	}
	t.Run("custom-receipt-and-foreign-namespace", func(t *testing.T) {
		h := newCetiAdminInstallationAPI(t, true)
		custom, namespace := h.target(t, "custom", "engineering", api.AgentTypeCustom, false)
		h.customReceipt(t, custom, namespace)
		foreign, foreignNS := h.target(t, "foreign", "engineering", api.AgentTypeHermes, false)
		unbound, unboundNS := h.target(t, "custom-unbound", "engineering", api.AgentTypeCustom, true)
		mismatched, mismatchedNS := h.target(t, "custom-wrong-receipt", "engineering", api.AgentTypeCustom, false)
		h.customReceipt(t, mismatched, namespace)
		h.converge(t)
		h.runtimeAccess(t, custom.Name)
		machine := h.machine(t)
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "custom-secret", Namespace: namespace.Name}, Data: map[string][]byte{"fixture": []byte("custom-owned")}}
		h.create(t, secret)
		stored := &corev1.Secret{}
		if err := machine.Get(h.ctx, client.ObjectKeyFromObject(secret), stored); err != nil || !bytes.Equal(stored.Data["fixture"], secret.Data["fixture"]) {
			t.Fatalf("current Custom receipt did not authorize its namespace: %v", err)
		}
		event := &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "custom-management", Namespace: namespace.Name},
			InvolvedObject: corev1.ObjectReference{APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: custom.Name, Namespace: namespace.Name, UID: custom.UID}, Reason: "Diagnostic", Message: "Synthetic management observation", Type: corev1.EventTypeNormal}
		if err := machine.Create(h.ctx, event); err != nil || event.UID == "" {
			t.Fatalf("served admission did not resolve current Custom ownership: %v", err)
		}
		for _, rejected := range []struct {
			agent     *api.Agent
			namespace *corev1.Namespace
		}{{foreign, foreignNS}, {unbound, unboundNS}, {mismatched, mismatchedNS}} {
			if err := h.installer.Runtime.reconcileCetiAdminAccess(h.ctx, rejected.agent.Name); err != nil &&
				!errors.Is(err, cetiadmin.ErrConflict) && !errors.Is(err, cetiadmin.ErrUnbound) && !apierrors.IsNotFound(err) {
				t.Fatalf("unexpected namespace reconciliation failure: %v", err)
			}
			h.create(t, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "foreign-secret", Namespace: rejected.namespace.Name}})
			if err := machine.Get(h.ctx, client.ObjectKey{Name: "foreign-secret", Namespace: rejected.namespace.Name}, &corev1.Secret{}); !apierrors.IsForbidden(err) {
				t.Fatalf("unowned runtime %s disclosed a namespace Secret: %v", rejected.agent.Name, err)
			}
			h.assertPermission(t, machine, "create", "", "pods", "exec", rejected.namespace.Name, "runtime-pod", false)
		}
		current := &corev1.Namespace{}
		if err := h.admin.Get(h.ctx, client.ObjectKeyFromObject(namespace), current); err != nil {
			t.Fatal(err)
		}
		if len(current.OwnerReferences) != 0 || current.Labels["app.kubernetes.io/managed-by"] != "cetirun-operator" {
			t.Fatal("Custom target was adopted or admission selector was not installed")
		}
		receipt := &corev1.ConfigMap{}
		if err := h.admission.Get(h.ctx, h.installer.Runtime.ownershipKey(custom), receipt); err != nil {
			t.Fatal("admission was not delegated exact Custom receipt read permission:", err)
		}
		h.reconcile(t)
		h.runtimeProjection(t)
	})
	t.Run("protected-runtime-keeps-diagnostics-without-execution-or-secrets", func(t *testing.T) {
		h := newCetiAdminInstallationAPI(t, true)
		h.converge(t)
		machine := h.machine(t)
		original := h.runtimeProjection(t)
		for _, protection := range []string{"team", "namespace", "sandbox-reference", "retained-sandbox-status"} {
			t.Run(protection, func(t *testing.T) {
				kind := api.AgentTypeHermes
				if protection == "sandbox-reference" || protection == "retained-sandbox-status" {
					kind = api.AgentTypeCustom
				}
				agent, namespace := h.target(t, "protected-"+protection, "guarded-"+protection, kind, true)
				secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "runtime-secret", Namespace: namespace.Name}, Data: map[string][]byte{"fixture": []byte("synthetic")}}
				h.create(t, secret)
				if kind != api.AgentTypeCustom {
					h.runtimeAccess(t, agent.Name)
					h.assertPermission(t, machine, "create", "", "pods", "exec", namespace.Name, "runtime-pod", true)
					h.assertPermission(t, machine, "update", "", "secrets", "", namespace.Name, secret.Name, true)
					for _, verb := range []string{"get", "list", "watch"} {
						name := ""
						if verb == "get" {
							name = secret.Name
						}
						h.assertPermission(t, machine, verb, "", "secrets", "", namespace.Name, name, true)
					}
				}
				switch protection {
				case "team":
					team := &api.Team{}
					if err := h.operator.Get(h.ctx, client.ObjectKey{Name: agent.Spec.Team}, team); err != nil {
						t.Fatal(err)
					}
					team.Spec.AgentPolicyRef = "protected-policy"
					if err := h.admin.Update(h.ctx, team); err != nil {
						t.Fatal(err)
					}
				case "namespace":
					current := &corev1.Namespace{}
					if err := h.operator.Get(h.ctx, client.ObjectKeyFromObject(namespace), current); err != nil {
						t.Fatal(err)
					}
					before := current.DeepCopy()
					current.Labels["cetirun.microsoft.com/sandbox-protected"] = "true"
					if err := h.operator.Patch(h.ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
						t.Fatal(err)
					}
				case "sandbox-reference", "retained-sandbox-status":
					if protection == "sandbox-reference" {
						agent.Spec.SandboxRef = "native-sandbox"
						if err := h.admin.Update(h.ctx, agent); err != nil {
							t.Fatal(err)
						}
					} else {
						agent.Status.Sandbox = &api.SandboxStatus{
							TeamUID: "historical-team", SandboxUID: "historical-sandbox", AgentPolicyUID: "historical-policy",
							AppliedRevision: "historical-revision",
						}
						if err := h.operator.Status().Update(h.ctx, agent); err != nil {
							t.Fatal(err)
						}
					}
					namespace.Labels = map[string]string{sandboxworkload.ProtectedNamespaceLabel: "true"}
					namespace.Annotations = map[string]string{
						sandboxworkload.AgentUIDAnnotation:  string(agent.UID),
						sandboxworkload.AgentNameAnnotation: agent.Name,
					}
					namespace.OwnerReferences = []metav1.OwnerReference{sandboxworkload.AgentOwner(agent.Name, agent.UID)}
					if err := h.admin.Update(h.ctx, namespace); err != nil {
						t.Fatal(err)
					}
				}
				h.runtimeAccess(t, agent.Name)
				h.assertPermission(t, machine, "create", "", "pods", "exec", namespace.Name, "runtime-pod", false)
				h.assertPermission(t, machine, "delete", "", "pods", "", namespace.Name, "runtime-pod", false)
				h.assertPermission(t, machine, "update", "", "secrets", "", namespace.Name, secret.Name, false)
				h.assertPermission(t, machine, "get", "", "pods", "log", namespace.Name, "runtime-pod", true)
				h.assertPermission(t, machine, "list", "", "pods", "", namespace.Name, "", true)
				for _, verb := range []string{"get", "list", "watch"} {
					name := ""
					if verb == "get" {
						name = secret.Name
					}
					h.assertPermission(t, machine, verb, "", "secrets", "", namespace.Name, name, false)
				}
				if err := machine.Get(h.ctx, client.ObjectKeyFromObject(secret), &corev1.Secret{}); !apierrors.IsForbidden(err) {
					t.Fatalf("protected runtime accepted a Secret read: %v", err)
				}
				if err := machine.List(h.ctx, &corev1.SecretList{}, client.InNamespace(namespace.Name)); !apierrors.IsForbidden(err) {
					t.Fatalf("protected runtime accepted a Secret list: %v", err)
				}
				stored := secret.DeepCopy()
				stored.Data["fixture"] = []byte("forbidden-write")
				if err := machine.Update(h.ctx, stored, client.DryRunAll); !apierrors.IsForbidden(err) {
					t.Fatalf("protected runtime accepted a Secret write: %v", err)
				}
				event := &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "protected-management", Namespace: namespace.Name},
					InvolvedObject: corev1.ObjectReference{APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: agent.Name, Namespace: namespace.Name, UID: agent.UID}, Reason: "Diagnostic", Message: "Synthetic protected observation", Type: corev1.EventTypeNormal}
				if err := machine.Create(h.ctx, event); err != nil || event.UID == "" {
					t.Fatalf("protected runtime rejected an allowed diagnostic Event: %v", err)
				}
				wrong := event.DeepCopy()
				wrong.Name, wrong.UID, wrong.ResourceVersion = "wrong-target-event", "", ""
				wrong.InvolvedObject.UID = h.state(t).Agent.UID
				if err := machine.Create(h.ctx, wrong, client.DryRunAll); !apierrors.IsForbidden(err) {
					t.Fatalf("protected namespace accepted an Event for another Agent UID: %v", err)
				}
				h.reconcile(t)
				if !reflect.DeepEqual(h.runtimeProjection(t), original) {
					t.Fatal("protecting another runtime changed the Admin runtime binding")
				}
			})
		}
	})
}

type cetiAdminInstallationAPI struct {
	ctx         context.Context
	config      *rest.Config
	admin       client.Client
	operator    client.Client
	admission   client.Client
	installer   *CetiAdminReconciler
	source      *corev1.ConfigMap
	helm        *cetiAdminInstallationHelm
	unavailable atomic.Bool
	probeCalls  atomic.Int64
	machineUID  atomic.Value
	loopbackURL string
	loopbackCA  []byte
	machinePod  *corev1.Pod
}

type cetiAdminInstallationHelm struct {
	suspensionHelmClient
	releases map[string]*release.Release
}

func (h *cetiAdminInstallationHelm) Prepare(_ string, values map[string]any, version string) (*helm.PreparedRelease, error) {
	chart, err := loader.Load("../../../cetirun-chart")
	if err != nil {
		return nil, err
	}
	if chart.Metadata.Version != version {
		return nil, fmt.Errorf("test uses the actual local runtime chart: requested %s, available %s", version, chart.Metadata.Version)
	}
	return helm.PrepareChart(chart, values)
}

func (h *cetiAdminInstallationHelm) CurrentRelease(_ context.Context, namespace, name string) (*release.Release, error) {
	if current := h.releases[namespace+"/"+name]; current != nil {
		return current, nil
	}
	return nil, driver.ErrReleaseNotFound
}

func newCetiAdminInstallationAPI(t *testing.T, enabled bool) *cetiAdminInstallationAPI {
	t.Helper()
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	for _, binary := range []string{"kube-apiserver", "etcd", "kubectl"} {
		info, err := os.Stat(filepath.Join(assets, binary))
		if assets == "" || err != nil || info.IsDir() || info.Mode()&0111 == 0 {
			t.Fatal("KUBEBUILDER_ASSETS must contain the pinned, executable local envtest binaries")
		}
	}
	environment := &envtest.Environment{
		BinaryAssetsDirectory: assets, UseExistingCluster: ptr.To(false),
		CRDDirectoryPaths: []string{"../../chart/templates/crds"}, ErrorIfCRDPathMissing: true,
		ControlPlaneStartTimeout: 45 * time.Second, ControlPlaneStopTimeout: 30 * time.Second,
	}
	environment.ControlPlane.GetAPIServer().Configure().Set("authorization-mode", "RBAC").Set("api-audiences", "cetiadmin-envtest")
	config, err := environment.Start()
	if err != nil {
		_ = environment.Stop()
		t.Fatal(err)
	}
	config.QPS, config.Burst = 200, 400
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	h := &cetiAdminInstallationAPI{ctx: ctx, config: config, helm: &cetiAdminInstallationHelm{releases: map[string]*release.Release{}}}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, api.AddToScheme, istiov1beta1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	h.admin = h.newClient(t, config, scheme)
	h.create(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "operator-test"}})
	mux := http.NewServeMux()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.unavailable.Load() {
			http.Error(w, "synthetic admission outage", http.StatusServiceUnavailable)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	h.loopbackURL = server.URL
	h.loopbackCA = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	objects := h.renderChart(t, enabled)
	var hooks []client.Object
	var deployment *appsv1.Deployment
	for _, object := range objects {
		switch value := object.(type) {
		case *admissionregistrationv1.ValidatingWebhookConfiguration:
			for i := range value.Webhooks {
				hook := &value.Webhooks[i]
				url := server.URL + *hook.ClientConfig.Service.Path
				hook.ClientConfig.Service, hook.ClientConfig.URL = nil, &url
			}
			hooks = append(hooks, value)
			continue
		case *admissionregistrationv1.MutatingWebhookConfiguration:
			for i := range value.Webhooks {
				hook := &value.Webhooks[i]
				url := server.URL + *hook.ClientConfig.Service.Path
				hook.ClientConfig.Service, hook.ClientConfig.URL = nil, &url
			}
			hooks = append(hooks, value)
			continue
		case *corev1.ConfigMap:
			if value.Name == "cetirun-cetiadmin-config" {
				h.source = value
			}
		case *appsv1.Deployment:
			if value.Name == "cetirun-sandbox-admission" {
				deployment = value
			}
		}
		h.create(t, object)
	}
	if h.source == nil {
		t.Fatal("chart did not provide its installation source")
	}
	operatorConfig := h.accountConfig(t, "operator-test", "controller", nil)
	h.operator = h.newClient(t, operatorConfig, scheme)
	configuration, err := cetiadmin.ParseConfig(h.source.Data[cetiadmin.ConfigKey])
	if err != nil {
		t.Fatal(err)
	}
	if !enabled {
		if deployment != nil || len(hooks) != 0 || configuration.Owner.Alias != "" {
			t.Fatal("minimal disabled chart unexpectedly required an owner or admission")
		}
		manager := &appsv1.Deployment{}
		if err := h.admin.Get(ctx, client.ObjectKey{Name: "cetirun-operator", Namespace: h.source.Namespace}, manager); err != nil {
			t.Fatal(err)
		}
		h.configureInstaller(manager.Spec.Template.Spec.Containers[0].Image, configuration.Runtime.Version)
		h.installer.AdmissionReady = func(context.Context) error { return fmt.Errorf("minimal disabled chart has no admission server") }
		return h
	}
	if deployment == nil || len(hooks) != 2 {
		t.Fatal("chart did not provide its source, shared admission Deployment and two webhook configurations")
	}
	var settings sandboxadmission.Config
	var operatorImage string
	for _, container := range deployment.Spec.Template.Spec.Containers {
		for _, value := range container.Env {
			if value.Name == "CETIRUN_SANDBOX_ADMISSION" {
				settings, err = sandboxadmission.ParseConfig(value.Value)
				if err != nil {
					t.Fatal(err)
				}
				operatorImage = container.Image
			}
		}
	}
	h.admission = h.newClient(t, h.accountConfig(t, "operator-test", "cetirun-sandbox-admission", nil), scheme)
	guard, err := sandboxadmission.NewCetiAdminHandler(h.admission, settings)
	if err != nil {
		t.Fatal(err)
	}
	governance, err := sandboxadmission.NewHandler(h.admission, settings)
	if err != nil {
		t.Fatal(err)
	}
	mux.Handle(cetiadmin.AdmissionValidationPath, &admission.Webhook{Handler: admission.HandlerFunc(func(ctx context.Context, request admission.Request) admission.Response {
		response := guard.Validate(ctx, request)
		if response.Allowed && request.DryRun != nil && *request.DryRun && request.Namespace == h.source.Namespace &&
			request.Resource.Resource == "configmaps" && strings.HasPrefix(request.Name, "cetiadmin-admission-probe-") && request.UserInfo.Username == settings.ControllerUsername {
			h.probeCalls.Add(1)
		}
		if strings.HasPrefix(request.UserInfo.Username, "system:serviceaccount:cetirun-") {
			h.machineUID.Store(request.UserInfo.UID)
		}
		return response
	})})
	mux.Handle(cetiadmin.AgentDefaultingPath, &admission.Webhook{Handler: admission.HandlerFunc(governance.DefaultAgent)})
	for _, object := range hooks {
		h.create(t, object)
	}
	h.configureInstaller(operatorImage, configuration.Runtime.Version)
	// Only readiness sees the reverse URL-to-Service adapter. All reads still
	// use the chart-bound Operator identity, and all writes reach served handlers.
	probeReconciler := *h.installer
	probeReconciler.APIReader = cetiAdminLoopbackReadinessReader{Reader: h.operator, url: server.URL, ca: h.loopbackCA, namespace: h.source.Namespace}
	probe, err := probeReconciler.NewAdmissionProbe(operatorConfig)
	if err != nil {
		t.Fatal(err)
	}
	h.installer.AdmissionReady = func(ctx context.Context) error {
		before := h.probeCalls.Load()
		if err := probe(ctx); err != nil {
			return err
		}
		if h.probeCalls.Load() == before {
			return fmt.Errorf("production readiness dry-run did not reach the loopback handler")
		}
		return nil
	}
	h.deploymentReady(t, deployment)
	// Observe the actual handler independently of the install-readiness contract,
	// so a production readiness defect does not hide disabled/bootstrap evidence.
	var last error
	if err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		probe := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{GenerateName: "protected-probe-", Namespace: h.source.Namespace,
			Annotations: map[string]string{cetiadmin.AnnotationSourceUID: string(h.source.UID)}}}
		last = h.admin.Create(ctx, probe, client.DryRunAll)
		return apierrors.IsForbidden(last) && strings.Contains(last.Error(), "Operator-owned"), nil
	}); err != nil {
		t.Fatalf("Chart webhook did not reach the production source-protection handler: %v; last=%v", err, last)
	}
	return h
}

func (h *cetiAdminInstallationAPI) configureInstaller(image, version string) {
	runtimeController := &AgentReconciler{Client: h.operator, APIReader: h.operator, Scheme: h.operator.Scheme(), HelmClient: h.helm,
		OperatorNamespace: h.source.Namespace, OperatorSAName: "controller", ChartVersion: version,
		ChartRegistry: "registry.example.com", DomainSuffix: "agents.example.com", IstioRevision: "test-istio",
		ConnectionsMCPAudience: "connections-envtest", RegistryCredentialNamespace: "registry-test", ConnectionsMCPBrokerNamespace: "broker-test",
		AIGateway: AIGatewayConfig{Enabled: true, URL: "http://gateway.operator-test.svc.cluster.local:4000"}}
	runtimeController.aiGatewayClientFactory = func(context.Context) (aiGatewayKeyClient, error) {
		return nil, fmt.Errorf("unexpected Gateway access in API-only installation test")
	}
	h.installer = &CetiAdminReconciler{Client: h.operator, APIReader: h.operator, Source: client.ObjectKeyFromObject(h.source), Runtime: runtimeController,
		OperatorImage: image, ChartVersion: version}
	h.installer.GrantAccess, h.installer.RevokeAccess = h.installer.GrantInstallationAccess, h.installer.RevokeInstallationAccess
	runtimeController.CetiAdmin = h.installer
}

func (h *cetiAdminInstallationAPI) renderChart(t *testing.T, enabled bool) []client.Object {
	t.Helper()
	chart, err := loader.Load("../../chart")
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]any{"namespace": "operator-test", "domainSuffix": "agents.example.com", "serviceAccount": map[string]any{"name": "controller"},
		"image": map[string]any{"repository": "registry.example.com/operator"},
		"cetiadmin": map[string]any{"enabled": enabled, "owner": map[string]any{"alias": "owner", "tenantID": "11111111-1111-1111-1111-111111111111", "objectID": "22222222-2222-2222-2222-222222222222"},
			"platformReadNamespaces": []any{"operator-test"}, "model": map[string]any{"initialMonthlyBudgetUSD": "10.25"},
			"kubernetes": map[string]any{"apiAudience": "cetiadmin-envtest", "apiEgressCIDRs": []any{"192.0.2.44/32"}}},
		"sandboxAdmission": map[string]any{"tlsSecretName": "envtest-tls", "caBundle": base64.StdEncoding.EncodeToString(h.loopbackCA), "adminGroups": []any{"system:masters"}}}
	if !enabled {
		values["cetiadmin"] = map[string]any{"enabled": false}
	}
	capabilities := chartutil.DefaultCapabilities.Copy()
	capabilities.KubeVersion.Version = "v1.37.0"
	renderValues, err := chartutil.ToRenderValues(chart, values, chartutil.ReleaseOptions{Name: "operator", Namespace: "operator-test", IsInstall: true}, capabilities)
	if err != nil {
		t.Fatal(err)
	}
	manifests, err := engine.Render(chart, renderValues)
	if err != nil {
		t.Fatal(err)
	}
	var objects []client.Object
	for _, name := range []string{"serviceaccount.yaml", "clusterrole.yaml", "clusterrolebinding.yaml", "cetiadmin-delegation.yaml", "cetiadmin-config.yaml", "sandbox-admission.yaml", "cetiadmin-admission.yaml", "deployment.yaml"} {
		decoder := kyaml.NewYAMLOrJSONDecoder(strings.NewReader(manifests[chart.Metadata.Name+"/templates/"+name]), 4096)
		for {
			object := &unstructured.Unstructured{}
			if err := decoder.Decode(object); err == io.EOF {
				break
			} else if err != nil {
				t.Fatal(err)
			}
			if len(object.Object) == 0 {
				continue
			}
			typed, err := h.admin.Scheme().New(object.GroupVersionKind())
			if err != nil {
				t.Fatal(err)
			}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, typed); err != nil {
				t.Fatal(err)
			}
			objects = append(objects, typed.(client.Object))
		}
	}
	return objects
}

// Envtest has no Service routing. Require the exact test URL and TLS trust
// before restoring only endpoint coordinates for the production contract check.
type cetiAdminLoopbackReadinessReader struct {
	client.Reader
	url, namespace string
	ca             []byte
}

func (r cetiAdminLoopbackReadinessReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if err := r.Reader.Get(ctx, key, object, options...); err != nil {
		return err
	}
	adapt := func(config *admissionregistrationv1.WebhookClientConfig, path string) error {
		if config.Service != nil || config.URL == nil || *config.URL != r.url+path || !bytes.Equal(config.CABundle, r.ca) {
			return fmt.Errorf("unexpected loopback admission endpoint or CA")
		}
		config.URL = nil
		config.Service = &admissionregistrationv1.ServiceReference{Name: "cetirun-sandbox-admission", Namespace: r.namespace, Path: ptr.To(path), Port: ptr.To(int32(443))}
		return nil
	}
	switch value := object.(type) {
	case *admissionregistrationv1.ValidatingWebhookConfiguration:
		for i := range value.Webhooks {
			if err := adapt(&value.Webhooks[i].ClientConfig, cetiadmin.AdmissionValidationPath); err != nil {
				return err
			}
		}
	case *admissionregistrationv1.MutatingWebhookConfiguration:
		for i := range value.Webhooks {
			if err := adapt(&value.Webhooks[i].ClientConfig, cetiadmin.AgentDefaultingPath); err != nil {
				return err
			}
		}
	}
	return nil
}

func (h *cetiAdminInstallationAPI) newClient(t *testing.T, config *rest.Config, scheme *runtime.Scheme) client.Client {
	t.Helper()
	result, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func (h *cetiAdminInstallationAPI) accountConfig(t *testing.T, namespace, name string, pod *corev1.Pod) *rest.Config {
	t.Helper()
	request := &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{Audiences: []string{"cetiadmin-envtest"}, ExpirationSeconds: ptr.To(int64(600))}}
	if pod != nil {
		request.Spec.BoundObjectRef = &authenticationv1.BoundObjectReference{APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID}
	}
	account := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	if err := h.admin.SubResource("token").Create(h.ctx, account, request); err != nil {
		t.Fatal(err)
	}
	config := rest.AnonymousClientConfig(h.config)
	config.BearerToken = request.Status.Token
	return config
}

func (h *cetiAdminInstallationAPI) create(t *testing.T, object client.Object) {
	t.Helper()
	if err := h.admin.Create(h.ctx, object); err != nil {
		t.Fatalf("create %T %s: %v", object, client.ObjectKeyFromObject(object), err)
	}
}

func (h *cetiAdminInstallationAPI) deploymentReady(t *testing.T, deployment *appsv1.Deployment) {
	t.Helper()
	replicas := ptr.Deref(deployment.Spec.Replicas, int32(1))
	deployment.Status = appsv1.DeploymentStatus{ObservedGeneration: deployment.Generation, Replicas: replicas, UpdatedReplicas: replicas, ReadyReplicas: replicas, AvailableReplicas: replicas}
	if err := h.admin.Status().Update(h.ctx, deployment); err != nil {
		t.Fatal(err)
	}
}

func (h *cetiAdminInstallationAPI) state(t *testing.T) *cetiadmin.Installation {
	t.Helper()
	checkpoint := &corev1.ConfigMap{}
	if err := h.operator.Get(h.ctx, h.installer.stateKey(), checkpoint); err != nil {
		t.Fatal(err)
	}
	state, err := cetiadmin.ParseInstallation(checkpoint.Data[cetiadmin.BindingKey])
	if err != nil {
		t.Fatal(err)
	}
	return &state
}

func (h *cetiAdminInstallationAPI) agent(t *testing.T) *api.Agent {
	t.Helper()
	state := h.state(t)
	agent := &api.Agent{}
	if err := h.admin.Get(h.ctx, client.ObjectKey{Name: state.Agent.Name}, agent); err != nil {
		t.Fatal(err)
	}
	return agent
}

func (h *cetiAdminInstallationAPI) converge(t *testing.T) {
	t.Helper()
	h.until(t, func(state *cetiadmin.Installation) bool { return state.Bootstrapped && state.CanRun() })
}

func (h *cetiAdminInstallationAPI) runtimeAccess(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if err := h.installer.Runtime.reconcileCetiAdminAccess(h.ctx, name); err != nil {
			t.Fatalf("ordinary Agent management-access reconciliation for %s: %v", name, err)
		}
	}
}

func (h *cetiAdminInstallationAPI) runtimeProjection(t *testing.T) cetiadmin.RuntimeProjection {
	t.Helper()
	binding, managed, err := h.installer.BindingForAgent(h.ctx, h.agent(t))
	if err != nil || !managed {
		t.Fatalf("Admin runtime binding is unavailable: managed=%v err=%v", managed, err)
	}
	projection, err := binding.RuntimeProjection(h.installer.OperatorImage, h.installer.Runtime.ConnectionsMCPAudience)
	if err != nil {
		t.Fatal(err)
	}
	return projection
}

func (h *cetiAdminInstallationAPI) reconcile(t *testing.T) *cetiadmin.Installation {
	t.Helper()
	_, err := h.installer.Reconcile(h.ctx, ctrl.Request{NamespacedName: h.installer.Source})
	state := h.state(t)
	if err != nil {
		t.Fatalf("installation reconcile: phase=%s pending=%s reason=%s: %v", state.Phase, state.PendingCreate, state.Reason, err)
	}
	return state
}

func (h *cetiAdminInstallationAPI) until(t *testing.T, done func(*cetiadmin.Installation) bool) {
	t.Helper()
	for attempt := range 32 {
		_, err := h.installer.Reconcile(h.ctx, ctrl.Request{NamespacedName: h.installer.Source})
		state := h.state(t)
		if err != nil {
			t.Fatalf("installation reconcile %d: phase=%s pending=%s reason=%s: %v", attempt+1, state.Phase, state.PendingCreate, state.Reason, err)
		}
		if done(state) {
			return
		}
	}
	state := h.state(t)
	t.Fatalf("installation did not converge: phase=%s pending=%s reason=%s", state.Phase, state.PendingCreate, state.Reason)
}

func (h *cetiAdminInstallationAPI) target(t *testing.T, name, team string, kind api.AgentType, owned bool) (*api.Agent, *corev1.Namespace) {
	t.Helper()
	if err := h.admin.Get(h.ctx, client.ObjectKey{Name: team}, &api.Team{}); apierrors.IsNotFound(err) {
		h.create(t, &api.Team{ObjectMeta: metav1.ObjectMeta{Name: team}})
	} else if err != nil {
		t.Fatal(err)
	}
	agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: api.AgentSpec{Agent: api.HarnessSpec{Type: kind}, User: api.UserSpec{Alias: "target-owner"}, Team: team, GatewayTokenSecretRef: "native-auth"}}
	if kind == api.AgentTypeCustom {
		agent.Spec.Agent.Image = ptr.To("registry.example.com/custom:1")
		agent.Spec.GatewayTokenSecretRef = ""
	}
	h.create(t, agent)
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: instanceNamespace(name)}}
	if owned {
		namespace.OwnerReferences = []metav1.OwnerReference{registryauth.Owner(agent)}
	}
	h.create(t, namespace)
	agent.Status = api.AgentStatus{Namespace: namespace.Name, HelmReleaseName: helmReleaseName(name), Phase: api.PhaseReady, ObservedGeneration: agent.Generation}
	if err := h.operator.Status().Update(h.ctx, agent); err != nil {
		t.Fatal(err)
	}
	return agent, namespace
}

func (h *cetiAdminInstallationAPI) targetManifest(t *testing.T, agent *api.Agent, namespace *corev1.Namespace) []client.Object {
	t.Helper()
	objects := []client.Object{
		&corev1.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: metav1.ObjectMeta{Name: "runtime-config", Namespace: namespace.Name}, Data: map[string]string{"setting": "synthetic"}},
		&corev1.ServiceAccount{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"}, ObjectMeta: metav1.ObjectMeta{Name: "runtime-account", Namespace: namespace.Name}, AutomountServiceAccountToken: ptr.To(false)},
	}
	var manifest strings.Builder
	for _, object := range objects {
		object.SetOwnerReferences([]metav1.OwnerReference{registryauth.Owner(agent)})
		body, err := yaml.Marshal(object)
		if err != nil {
			t.Fatal(err)
		}
		manifest.WriteString("---\n")
		manifest.Write(body)
		h.create(t, object)
	}
	name := helmReleaseName(agent.Name)
	h.helm.releases[namespace.Name+"/"+name] = &release.Release{Name: name, Namespace: namespace.Name, Version: 1,
		Info: &release.Info{Status: release.StatusDeployed}, Manifest: manifest.String()}
	return objects
}

func (h *cetiAdminInstallationAPI) customReceipt(t *testing.T, agent *api.Agent, namespace *corev1.Namespace) {
	t.Helper()
	key := h.installer.Runtime.ownershipKey(agent)
	data, err := json.Marshal(customOwnership{AgentName: agent.Name, AgentUID: agent.UID, Namespace: namespace.Name, NamespaceUID: namespace.UID,
		Release: helmReleaseName(agent.Name), Home: "custom-home", HomeClaim: &runtimebindings.HomeClaim{StorageClass: "synthetic-storage", StorageSize: "1Gi", AccessMode: "ReadWriteOnce"}})
	if err != nil {
		t.Fatal(err)
	}
	h.create(t, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, OwnerReferences: []metav1.OwnerReference{registryauth.Owner(agent)}},
		Immutable: ptr.To(true), Data: map[string]string{"ownership": string(data)}})
}

func (h *cetiAdminInstallationAPI) changeConfig(t *testing.T, edit func(*cetiadmin.Config)) {
	t.Helper()
	source := &corev1.ConfigMap{}
	if err := h.admin.Get(h.ctx, client.ObjectKeyFromObject(h.source), source); err != nil {
		t.Fatal(err)
	}
	config, err := cetiadmin.ParseConfig(source.Data[cetiadmin.ConfigKey])
	if err != nil {
		t.Fatal(err)
	}
	edit(&config)
	body, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	before := source.DeepCopy()
	source.Data[cetiadmin.ConfigKey] = string(body)
	if err := h.admin.Patch(h.ctx, source, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		t.Fatal(err)
	}
}

func (h *cetiAdminInstallationAPI) observeStoppedPod(t *testing.T) {
	t.Helper()
	if h.machinePod == nil {
		t.Fatal("no synthetic runtime Pod to observe")
	}
	pod := &corev1.Pod{}
	if err := h.admin.Get(h.ctx, client.ObjectKeyFromObject(h.machinePod), pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = corev1.PodSucceeded
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "hermes", Image: "registry.example.com/synthetic:1", ImageID: "synthetic", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}}}
	if err := h.admin.Status().Update(h.ctx, pod); err != nil {
		t.Fatal(err)
	}
	// No scheduler, kubelet, model call or HOME write occurred. This supplies the
	// stop observation only after the installer has withdrawn its real RBAC.
	h.machinePod = pod
}

func (h *cetiAdminInstallationAPI) machine(t *testing.T) client.Client {
	t.Helper()
	state := h.state(t)
	if h.machinePod == nil {
		h.machinePod = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "synthetic-hermes", Namespace: state.Namespace.Name}, Spec: corev1.PodSpec{
			ServiceAccountName: state.ServiceAccount.Name, AutomountServiceAccountToken: ptr.To(false),
			RestartPolicy: corev1.RestartPolicyNever,
			Containers:    []corev1.Container{{Name: "hermes", Image: "registry.example.com/synthetic:1"}},
		}}
		h.create(t, h.machinePod)
	}
	return h.newClient(t, h.accountConfig(t, state.ServiceAccount.Namespace, state.ServiceAccount.Name, h.machinePod), h.admin.Scheme())
}

type cetiAdminLostCreateClient struct {
	client.Client
	lost       bool
	createdUID string
}

func (c *cetiAdminLostCreateClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	if _, ok := object.(*api.Agent); ok && !c.lost {
		served := object.DeepCopyObject().(client.Object)
		if err := c.Client.Create(ctx, served, options...); err != nil {
			return err
		}
		c.lost, c.createdUID = true, string(served.GetUID())
		// Keep the caller's pre-defaulted request intact, as when the response
		// body was lost after the API server had persisted the CREATE.
		return fmt.Errorf("synthetic lost Agent CREATE response")
	}
	return c.Client.Create(ctx, object, options...)
}

func (h *cetiAdminInstallationAPI) assertPermission(t *testing.T, caller client.Client, verb, group, resource, subresource, namespace, name string, allowed bool) {
	t.Helper()
	review := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authorizationv1.ResourceAttributes{
		Verb: verb, Group: group, Resource: resource, Subresource: subresource, Namespace: namespace, Name: name,
	}}}
	if err := caller.Create(h.ctx, review); err != nil {
		t.Fatal(err)
	}
	if review.Status.Allowed != allowed {
		t.Fatalf("RBAC %s %s/%s/%s namespace=%s name=%s: allowed=%v want=%v reason=%s", verb, group, resource, subresource, namespace, name, review.Status.Allowed, allowed, review.Status.Reason)
	}
}

func (h *cetiAdminInstallationAPI) assertRecordedUIDs(t *testing.T, state *cetiadmin.Installation) {
	t.Helper()
	checkpoint := &corev1.ConfigMap{}
	if err := h.admin.Get(h.ctx, h.installer.stateKey(), checkpoint); err != nil || checkpoint.UID != state.RecordUID || state.RecordUID == "" {
		t.Fatalf("record UID was not persisted: %v", err)
	}
	for _, ref := range append([]cetiadmin.ManagedResource{
		{Kind: "Agent", ObjectIdentity: state.Agent}, {Kind: "Team", ObjectIdentity: state.Team},
		{Kind: "Namespace", ObjectIdentity: state.Namespace}, {Kind: "ServiceAccount", ObjectIdentity: state.ServiceAccount},
	}, state.Resources...) {
		object, _, err := installationObject(ref.Kind)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.admin.Get(h.ctx, client.ObjectKey{Name: ref.Name, Namespace: ref.Namespace}, object); err != nil {
			t.Fatal(err)
		}
		if ref.UID == "" || identityOf(object) != ref.ObjectIdentity {
			t.Fatalf("%s identity is not its API-assigned UID", ref.Kind)
		}
	}
}
