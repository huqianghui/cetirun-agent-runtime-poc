// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/aigateway"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kubeyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const nativeSandboxSourcesJSON = `{
	"enabled":true,"bootstrapOwnedPrerequisites":true,
	"platformConfigMapName":"sandbox-platform","homeClaimName":"data-example-0",
	"skillsClaimName":"skills-example","admissionServiceAccount":"sandbox-admission"
}`

func TestNativeSandboxSourcesRequireExplicitOptIn(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		native    bool
		wantError bool
	}{
		{name: "unset"},
		{name: "empty", raw: "{}"},
		{name: "sources alone", raw: nativeSandboxSourcesJSON},
		{name: "explicit false", raw: strings.Replace(nativeSandboxSourcesJSON, `"enabled":true`, `"enabled":true,"nativeExecution":false`, 1)},
		{name: "explicit true", raw: strings.Replace(nativeSandboxSourcesJSON, `"enabled":true`, `"enabled":true,"nativeExecution":true`, 1), native: true},
		{name: "removed gate false", raw: strings.Replace(nativeSandboxSourcesJSON, `"enabled":true`, `"enabled":true,"nativeExecution":true,"reconciledExecution":false`, 1), wantError: true},
		{name: "removed gate true", raw: strings.Replace(nativeSandboxSourcesJSON, `"enabled":true`, `"enabled":true,"nativeExecution":true,"reconciledExecution":true`, 1), wantError: true},
		{name: "removed gate without native execution", raw: strings.Replace(nativeSandboxSourcesJSON, `"enabled":true`, `"enabled":true,"reconciledExecution":true`, 1), wantError: true},
		{name: "reconciliation rejects string", raw: `{"reconciledExecution":"true"}`, wantError: true},
		{name: "reconciliation rejects duplicate gate", raw: `{"reconciledExecution":false,"reconciledExecution":true}`, wantError: true},
		{name: "native without claim name", raw: strings.Replace(strings.Replace(nativeSandboxSourcesJSON, `"enabled":true`, `"enabled":true,"nativeExecution":true`, 1), `"skills-example"`, `""`, 1), native: true},
		{name: "disabled starts retain empty claim setting", raw: strings.Replace(nativeSandboxSourcesJSON, `"skills-example"`, `""`, 1)},
		{name: "catalog still needs claim name", raw: strings.Replace(strings.Replace(nativeSandboxSourcesJSON, `"enabled":true`, `"enabled":true,"nativeExecution":true,"skillsCatalogConfigMapName":"skills-catalog"`, 1), `"skills-example"`, `""`, 1), wantError: true},
		{name: "requires sources", raw: `{"nativeExecution":true,"bootstrapOwnedPrerequisites":true}`, wantError: true},
		{name: "requires bootstrap", raw: strings.Replace(nativeSandboxSourcesJSON, `"bootstrapOwnedPrerequisites":true`, `"nativeExecution":true`, 1), wantError: true},
		{name: "rejects desktop", raw: strings.Replace(nativeSandboxSourcesJSON, `"enabled":true`, `"enabled":true,"nativeExecution":true,"helpers":{"desktop":true}`, 1), wantError: true},
		{name: "rejects string", raw: `{"nativeExecution":"true"}`, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, err := ParseSandboxSourcesConfig(tc.raw)
			if (err != nil) != tc.wantError {
				t.Fatalf("ParseSandboxSourcesConfig error = %v, want error %v", err, tc.wantError)
			}
			if err != nil {
				return
			}
			if config.NativeExecution != tc.native {
				t.Fatalf("nativeExecution = %v, want %v", config.NativeExecution, tc.native)
			}
			_, kube, _ := aiGatewayTestReconciler(t)
			preparation, err := NewKubernetesSandboxPreparation(config, kube, &nativeSandboxInstaller{}, "cetirun-system", "0.1.1")
			if err != nil {
				t.Fatal(err)
			}
			if !config.Enabled {
				if preparation != nil {
					t.Fatal("disabled sources installed a preparation path")
				}
				return
			}
			source, ok := preparation.Prerequisites.(*KubernetesSandboxPrerequisites)
			if !ok || preparation.NativeExecution != tc.native ||
				source.NativeExecution != tc.native || (source.NativeQuiescence != nil) != tc.native ||
				source.WriterExclusion != nil {
				t.Fatal("factory did not preserve explicit opt-in or invented a writer-exclusion provider")
			}
		})
	}
}

func TestNativeSandboxProfileRejectsUnsupportedOverrides(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*sandboxPreparationAttempt)
	}{
		{"Hermes", func(a *sandboxPreparationAttempt) { a.agent.Spec.Agent.Type = api.AgentTypeHermes }},
		{"non-Kata", func(a *sandboxPreparationAttempt) { a.resolution.Sandbox.Type = api.SandboxType("other") }},
		{"RWO HOME", func(a *sandboxPreparationAttempt) {
			a.resolution.Sandbox.Storage.Home.AccessMode = corev1.ReadWriteOnce
		}},
		{"no shared models", func(a *sandboxPreparationAttempt) { a.agent.Spec.ModelAccess.SharedModels = ptr.To(false) }},
		{"Copilot", func(a *sandboxPreparationAttempt) { a.agent.Spec.ModelAccess.GitHubCopilot = ptr.To(true) }},
		{"Tailscale", func(a *sandboxPreparationAttempt) { a.agent.Spec.Tailscale = &api.TailscaleSpec{} }},
		{"Agent Identity", func(a *sandboxPreparationAttempt) { a.agent.Spec.AgentIdentity = &api.AgentIdentitySpec{Enabled: true} }},
		{"Entra grant", func(a *sandboxPreparationAttempt) { a.resolution.AgentPolicy.EntraID.Enabled = true }},
		{"Kubernetes grant", func(a *sandboxPreparationAttempt) {
			a.resolution.AgentPolicy.KubernetesAccess = &api.AgentPolicyAdminKubernetesAccess{ServiceAccountName: "admin"}
		}},
		{"Helm", func(a *sandboxPreparationAttempt) {
			a.agent.Spec.Helm.Values = map[string]runtime.RawExtension{"replicaCount": {Raw: []byte("1")}}
		}},
		{"blocked FQDN", func(a *sandboxPreparationAttempt) {
			a.resolution.AgentPolicy.Network.BlockedFQDNs = []string{"blocked.example.com"}
		}},
		{"blocked SCTP port", func(a *sandboxPreparationAttempt) {
			a.resolution.Capabilities.BlockedPorts = true
			a.resolution.Capabilities.Protocols = append(a.resolution.Capabilities.Protocols, corev1.ProtocolSCTP)
			a.resolution.AgentPolicy.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 443, Protocol: corev1.ProtocolSCTP}}
		}},
		{"internal CIDR", func(a *sandboxPreparationAttempt) {
			a.resolution.AgentPolicy.Network.AllowedInternalCIDRs = []api.AgentPolicyCIDRRule{{CIDR: "10.20.0.0/16", Ports: []api.AgentPolicyPort{{Port: 443}}}}
		}},
		{"custom mTLS", func(a *sandboxPreparationAttempt) { a.resolution.AgentPolicy.Network.Ingress.RequireMTLS = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := testfixture.New("openclaw")
			fixture.Agent.Spec.ModelAccess = modelAccess(true, false)
			fixture.Input.Resolution.Sandbox.Storage.Home.AccessMode = corev1.ReadWriteOncePod
			attempt := &sandboxPreparationAttempt{agent: fixture.Agent, resolution: fixture.Input.Resolution}
			if err := validateNativeSandboxProfile(attempt); err != nil {
				t.Fatal("supported baseline rejected:", err)
			}
			tc.mutate(attempt)
			if err := validateNativeSandboxProfile(attempt); !errors.Is(err, sandboxpolicy.ErrUnsupportedControl) {
				t.Fatalf("unsupported native profile was accepted: %v", err)
			}
		})
	}
}

func TestNativeSandboxStartRequiresOwnedCurrentGatewayProjection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*api.Agent, *corev1.Secret)
	}{
		{"foreign Secret owner", func(_ *api.Agent, s *corev1.Secret) {
			s.OwnerReferences = []metav1.OwnerReference{w.AgentOwner("foreign", "foreign-agent-uid")}
		}},
		{"missing Secret owner", func(_ *api.Agent, s *corev1.Secret) { s.OwnerReferences = nil }},
		{"foreign Secret label", func(_ *api.Agent, s *corev1.Secret) { s.Labels[aiGatewayAgentUIDLabel] = "foreign-agent-uid" }},
		{"invalid projected key", func(_ *api.Agent, s *corev1.Secret) {
			s.Data[aiGatewayKeySecretField] = []byte("synthetic-invalid-key")
		}},
		{"stale policy generation", func(a *api.Agent, _ *corev1.Secret) { a.Status.Policy.ObservedGeneration-- }},
		{"stale Gateway revision", func(a *api.Agent, _ *corev1.Secret) { a.Status.AIGateway.PolicyRevision = "previous-revision" }},
		{"blocked Gateway", func(a *api.Agent, _ *corev1.Secret) { a.Status.AIGateway.Blocked = true }},
		{"unrouted Gateway", func(a *api.Agent, _ *corev1.Secret) { a.Status.AIGateway.Routed = false }},
		{"missing key identity", func(a *api.Agent, _ *corev1.Secret) { a.Status.AIGateway.KeyID = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNativeSandboxFixture(t, true)
			f.projectGateway(t)
			agent := f.current(t)
			secret := &corev1.Secret{}
			if err := f.kube.Get(context.Background(), client.ObjectKey{
				Namespace: instanceNamespace(agent.Name), Name: aiGatewayKeySecretName,
			}, secret); err != nil {
				t.Fatal(err)
			}
			tc.mutate(agent, secret)
			if err := f.kube.Update(context.Background(), secret); err != nil {
				t.Fatal(err)
			}
			if err := f.kube.Status().Update(context.Background(), agent); err != nil {
				t.Fatal(err)
			}
			if _, err := f.r.startKubernetesSandbox(context.Background(), f.current(t)); err == nil {
				t.Fatal("native start accepted an unowned or stale Gateway projection")
			}
			f.assertNotReady(t)
			if !slices.Contains(f.current(t).Finalizers, sandbox.HoldFinalizer) || f.installer.stages != 0 {
				t.Fatal("invalid Gateway projection advanced to chart staging or release")
			}
			if _, _, err := f.r.readSandboxPreparationReceipt(context.Background(), f.current(t)); !apierrors.IsNotFound(err) {
				t.Fatal("invalid Gateway projection recorded preparation or a native start:", err)
			}
		})
	}
}

func TestNativeSandboxHeldToRunningRequiresCurrentPodReadiness(t *testing.T) {
	f := newNativeSandboxFixture(t, true)
	f.prepare(t)
	state, held := f.expectation(t)
	if !held.ExecutionHeld || held.SinglePod || held.PodUID != "" || ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 {
		t.Fatal("preparation granted execution")
	}
	f.assertGatewayProjection(t, held)
	receipt, _, err := f.r.readSandboxPreparationReceipt(context.Background(), f.current(t))
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Annotations[nativeStartAnnotation] != "" || len(receipt.Data) != 1 {
		t.Fatal("held preparation already recorded a start or introduced another receipt")
	}
	var order []string
	f.r.Client = interceptor.NewClient(f.kube, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
			step := ""
			switch object := object.(type) {
			case *corev1.ConfigMap:
				if object.Name == receipt.Name && object.Annotations[nativeStartAnnotation] != "" {
					step = "marker"
				} else if object.Name == state.Name {
					var next w.Expectation
					if err := json.Unmarshal([]byte(object.Data[w.PodExpectationKey]), &next); err != nil {
						return err
					}
					if !next.ExecutionHeld {
						step = "expectation"
					}
				}
			case *api.Agent:
				if nativeSandboxReleasesHold(t, object, patch) {
					step = "release"
				}
			case *appsv1.StatefulSet:
				if ptr.Deref(object.Spec.Replicas, -1) == 1 {
					step = "scale"
				}
			}
			if step != "" && step != "marker" {
				persisted, _, err := f.r.readSandboxPreparationReceipt(ctx, f.current(t))
				if err != nil || persisted.UID != receipt.UID || persisted.Annotations[nativeStartAnnotation] != held.Revision {
					t.Fatalf("%s preceded the durable one-shot start marker: %v", step, err)
				}
			}
			if err := c.Patch(ctx, object, patch, opts...); err != nil {
				return err
			}
			if step != "" {
				order = append(order, step)
			}
			return nil
		},
	})
	f.reconcileOK(t)
	f.r.Client = f.kube
	if !slices.Equal(order, []string{"marker", "release", "scale", "expectation"}) {
		t.Fatalf("start write ordering = %v", order)
	}
	runningState, running := f.expectation(t)
	if runningState.UID != state.UID || running.ExecutionHeld || !running.SinglePod || running.PodUID != "" ||
		ptr.Deref(f.workload(t).Spec.Replicas, -1) != 1 || slices.Contains(f.current(t).Finalizers, sandbox.HoldFinalizer) {
		t.Fatal("start did not release the existing expectation for exactly one Pod")
	}
	digests := 0
	for _, source := range running.Sources {
		if !w.EffectiveSource(source) {
			continue
		}
		digests++
		if !w.ValidInputDigest(running.SourceDigests[w.SourceKey(source)]) {
			t.Fatalf("native start did not pin effective source %s", w.SourceKey(source))
		}
	}
	if digests == 0 || len(running.SourceDigests) != digests {
		t.Fatal("native start has missing or unexpected source digests")
	}
	workload := f.workload(t)
	workload.Status.ReadyReplicas = 1
	if err := f.kube.Status().Update(context.Background(), workload); err != nil {
		t.Fatal(err)
	}
	f.reconcileOK(t)
	f.assertNotReady(t)
	_, running = f.expectation(t)
	if running.PodUID != "" {
		t.Fatal("absent Pod was assigned an identity")
	}
	pod := f.createPod(t, false)
	f.reconcileOK(t)
	f.assertNotReady(t)
	_, running = f.expectation(t)
	if running.PodUID != pod.UID {
		t.Fatal("the first observed Pod UID was not durably pinned")
	}
	pod = f.pod(t)
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if err := f.kube.Status().Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	f.reconcileOK(t)
	f.assertReady(t)
	route := &istiov1beta1.VirtualService{}
	if err := f.kube.Get(context.Background(), client.ObjectKey{Namespace: pod.Namespace, Name: virtualServiceName(f.agent.Name)}, route); err != nil {
		t.Fatal(err)
	}
	if !sandboxBuiltinOwned(route, f.current(t)) {
		t.Fatal("Ready published an unowned route")
	}
}

func TestNativeSandboxReadyRepublishesVerifiedNamespace(t *testing.T) {
	f := newNativeSandboxFixture(t, true)
	f.ready(t)
	before := f.current(t)
	source, err := w.AgentSourceReference(before)
	if err != nil {
		t.Fatal(err)
	}
	workload, stages := f.workload(t), f.installer.stages
	for _, namespace := range []string{"", "untrusted-status-namespace"} {
		agent := f.current(t)
		agent.Status.Namespace = namespace
		if err := f.kube.Status().Update(context.Background(), agent); err != nil {
			t.Fatal(err)
		}
		f.reconcileOK(t)
		f.assertReady(t)
		after := f.current(t)
		currentSource, err := w.AgentSourceReference(after)
		if err != nil || currentSource != source || after.Generation != before.Generation {
			t.Fatal("namespace status publication changed Agent authorization intent:", err)
		}
		currentWorkload := f.workload(t)
		if currentWorkload.UID != workload.UID || !w.SamePodTemplate(currentWorkload.Spec.Template, workload.Spec.Template) ||
			f.installer.stages != stages {
			t.Fatal("namespace status repair changed or restaged the protected workload")
		}
	}
}

func TestNativeSandboxDoesNotPublishUnverifiedNamespace(t *testing.T) {
	f := newNativeSandboxFixture(t, true)
	f.prepare(t)
	f.reconcileOK(t)
	f.createPod(t, true)
	_, expected := f.expectation(t)
	namespace := &corev1.Namespace{}
	ctx := context.Background()
	if err := f.kube.Get(ctx, client.ObjectKey{Name: expected.Namespace.Name}, namespace); err != nil {
		t.Fatal(err)
	}
	namespace.OwnerReferences = []metav1.OwnerReference{w.AgentOwner("other-agent", "other-agent-uid")}
	if err := f.kube.Update(ctx, namespace); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reconcile(); err == nil {
		t.Fatal("native readiness accepted a namespace whose ownership changed")
	}
	f.assertNotReady(t)
	if f.current(t).Status.Namespace != "" {
		t.Fatal("unverified namespace was published for owner access")
	}
}

func TestNativeSandboxReadinessToleratesAPIBindingAndStatusChurn(t *testing.T) {
	f := newNativeSandboxFixture(t, true)
	f.prepare(t)
	f.reconcileOK(t)
	_, before := f.expectation(t)
	claims, controls := 0, 0
	for _, source := range before.Sources {
		key := client.ObjectKey{Namespace: source.Namespace, Name: source.Name}
		switch source.Kind {
		case "PersistentVolumeClaim":
			claim := &corev1.PersistentVolumeClaim{}
			if err := bindNativeTestClaim(context.Background(), f.kube, key); err != nil {
				t.Fatal(err)
			}
			if err := f.kube.Get(context.Background(), key, claim); err != nil {
				t.Fatal(err)
			}
			if claim.ResourceVersion == source.ResourceVersion {
				t.Fatal("PVC binding fixture did not change the API resource version")
			}
			claims++
		case "CiliumNetworkPolicy":
			control := &unstructured.Unstructured{}
			control.SetAPIVersion(source.APIVersion)
			control.SetKind(source.Kind)
			if err := f.kube.Get(context.Background(), key, control); err != nil {
				t.Fatal(err)
			}
			if err := unstructured.SetNestedField(control.Object, int64(1), "status", "observedGeneration"); err != nil {
				t.Fatal(err)
			}
			if err := f.kube.Update(context.Background(), control); err != nil {
				t.Fatal(err)
			}
			controls++
		}
	}
	if claims != 2 || controls == 0 {
		t.Fatal("the prepared fixture did not exercise HOME, skills and control status churn")
	}
	f.reconcileOK(t)
	f.assertNotReady(t)
	f.createPod(t, true)
	f.reconcileOK(t)
	f.assertReady(t)
	_, after := f.expectation(t)
	if !maps.Equal(before.SourceDigests, after.SourceDigests) {
		t.Fatal("normal API status churn rebound the immutable execution inputs")
	}
}

func TestNativeSandboxRunningFailuresHoldWithoutReplay(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, *nativeSandboxFixture)
	}{
		{"Pod readiness lost", func(t *testing.T, f *nativeSandboxFixture) {
			pod := f.pod(t)
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
			if err := f.kube.Status().Update(context.Background(), pod); err != nil {
				t.Fatal(err)
			}
		}},
		{"Pod deleted", func(t *testing.T, f *nativeSandboxFixture) {
			if err := f.kube.Delete(context.Background(), f.pod(t)); err != nil {
				t.Fatal(err)
			}
		}},
		{"Pod replaced", func(t *testing.T, f *nativeSandboxFixture) {
			pod := f.pod(t)
			if err := f.kube.Delete(context.Background(), pod); err != nil {
				t.Fatal(err)
			}
			pod.UID, pod.ResourceVersion = "replacement-pod-uid", ""
			if err := f.kube.Create(context.Background(), pod); err != nil {
				t.Fatal(err)
			}
		}},
		{"Node readiness lost", func(t *testing.T, f *nativeSandboxFixture) {
			node := &corev1.Node{}
			if err := f.kube.Get(context.Background(), client.ObjectKey{Name: "native-test-node"}, node); err != nil {
				t.Fatal(err)
			}
			node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}
			if err := f.kube.Status().Update(context.Background(), node); err != nil {
				t.Fatal(err)
			}
		}},
		{"Gateway unavailable", func(_ *testing.T, f *nativeSandboxFixture) { f.gateway.getErr = errors.New("synthetic Gateway outage") }},
		{"HOME lost", func(t *testing.T, f *nativeSandboxFixture) {
			_, receipt, err := f.r.readSandboxPreparationReceipt(context.Background(), f.current(t))
			if err != nil {
				t.Fatal(err)
			}
			home := &corev1.PersistentVolumeClaim{}
			if err := f.kube.Get(context.Background(), client.ObjectKey{
				Namespace: instanceNamespace(f.agent.Name), Name: receipt.Prerequisites.Home.Name,
			}, home); err != nil {
				t.Fatal(err)
			}
			home.Status.Phase = corev1.ClaimLost
			if err := f.kube.Status().Update(context.Background(), home); err != nil {
				t.Fatal(err)
			}
		}},
		{"source digests missing", func(t *testing.T, f *nativeSandboxFixture) {
			state, expected := f.expectation(t)
			expected.SourceDigests = nil
			if err := f.r.saveNativeExpectation(context.Background(), state, expected); err != nil {
				t.Fatal(err)
			}
		}},
		{"platform source lost", func(t *testing.T, f *nativeSandboxFixture) {
			if err := f.kube.Delete(context.Background(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
				Namespace: f.r.OperatorNamespace, Name: "sandbox-platform",
			}}); err != nil {
				t.Fatal(err)
			}
		}},
		{"rendered config source changed", func(t *testing.T, f *nativeSandboxFixture) {
			_, expected := f.expectation(t)
			for _, source := range expected.Sources {
				if source.Kind == "ConfigMap" && source.Namespace == expected.Namespace.Name {
					object := &corev1.ConfigMap{}
					if err := f.kube.Get(context.Background(), client.ObjectKey{Namespace: source.Namespace, Name: source.Name}, object); err != nil {
						t.Fatal(err)
					}
					if object.Data == nil {
						object.Data = map[string]string{}
					}
					object.Data["synthetic-changed-input"] = "changed"
					if err := f.kube.Update(context.Background(), object); err != nil {
						t.Fatal(err)
					}
					return
				}
			}
			t.Fatal("prepared chart had no ConfigMap source to exercise")
		}},
		{"ControllerRevision lost", func(t *testing.T, f *nativeSandboxFixture) {
			_, expected := f.expectation(t)
			if err := f.kube.Delete(context.Background(), &appsv1.ControllerRevision{ObjectMeta: metav1.ObjectMeta{
				Namespace: expected.Namespace.Name, Name: expected.ControllerRevision,
			}}); err != nil {
				t.Fatal(err)
			}
		}},
		{"control lost", func(t *testing.T, f *nativeSandboxFixture) {
			_, expected := f.expectation(t)
			for _, source := range expected.Sources {
				if source.Kind == "CiliumNetworkPolicy" {
					object := &unstructured.Unstructured{}
					object.SetAPIVersion(source.APIVersion)
					object.SetKind(source.Kind)
					object.SetNamespace(source.Namespace)
					object.SetName(source.Name)
					if err := f.kube.Delete(context.Background(), object); err != nil {
						t.Fatal(err)
					}
					return
				}
			}
			t.Fatal("prepared chart had no network control")
		}},
		{"foreign route", func(t *testing.T, f *nativeSandboxFixture) {
			route := &istiov1beta1.VirtualService{}
			if err := f.kube.Get(context.Background(), client.ObjectKey{
				Namespace: instanceNamespace(f.agent.Name), Name: virtualServiceName(f.agent.Name),
			}, route); err != nil {
				t.Fatal(err)
			}
			route.OwnerReferences = []metav1.OwnerReference{w.AgentOwner("foreign", "foreign-agent-uid")}
			if err := f.kube.Update(context.Background(), route); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNativeSandboxFixture(t, true)
			f.ready(t)
			_, before := f.expectation(t)
			tc.mutate(t, f)
			_, err := f.reconcile()
			if err == nil {
				t.Error("current native execution failure was not reported")
			}
			f.assertNotReady(t)
			if !slices.Contains(f.current(t).Finalizers, sandbox.HoldFinalizer) || ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 {
				t.Fatal("failed observation left workload execution enabled")
			}
			stages := f.installer.stages
			if _, err := f.r.startKubernetesSandbox(context.Background(), f.current(t)); err == nil {
				t.Fatal("a previously started Agent automatically replayed")
			}
			receipt, _, err := f.r.readSandboxPreparationReceipt(context.Background(), f.current(t))
			if err != nil || receipt.Annotations[nativeStartAnnotation] != before.Revision ||
				f.installer.stages != stages || ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 {
				t.Fatal("replay changed the one-shot receipt or staged another workload:", err)
			}
			if tc.name == "foreign route" {
				route := &istiov1beta1.VirtualService{}
				if err := f.kube.Get(context.Background(), client.ObjectKey{
					Namespace: instanceNamespace(f.agent.Name), Name: virtualServiceName(f.agent.Name),
				}, route); err != nil || route.OwnerReferences[0].UID != "foreign-agent-uid" {
					t.Fatal("hold adopted or removed another Agent's route:", err)
				}
			}
		})
	}
}

func TestNativeSandboxStartFailureRetainsSingleStartBoundary(t *testing.T) {
	for _, failAt := range []string{"marker", "release"} {
		t.Run(failAt, func(t *testing.T) {
			f := newNativeSandboxFixture(t, true)
			f.prepare(t)
			receipt, _, err := f.r.readSandboxPreparationReceipt(context.Background(), f.current(t))
			if err != nil {
				t.Fatal(err)
			}
			failed := false
			f.r.Client = interceptor.NewClient(f.kube, interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
					isMarker := object.GetName() == receipt.Name && object.GetAnnotations()[nativeStartAnnotation] != ""
					agent, isAgent := object.(*api.Agent)
					isRelease := isAgent && nativeSandboxReleasesHold(t, agent, patch)
					if (failAt == "marker" && isMarker) || (failAt == "release" && isRelease) {
						failed = true
						return errors.New("synthetic native start write failure")
					}
					return c.Patch(ctx, object, patch, opts...)
				},
			})
			if _, err := f.reconcile(); err == nil || !failed {
				t.Fatal("start write failure was not exercised and reported:", err)
			}
			f.r.Client = f.kube
			f.assertNotReady(t)
			if ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 || !slices.Contains(f.current(t).Finalizers, sandbox.HoldFinalizer) {
				t.Fatal("failed start released compute")
			}
			persisted, _, err := f.r.readSandboxPreparationReceipt(context.Background(), f.current(t))
			if err != nil || persisted.UID != receipt.UID {
				t.Fatal("start replaced its preparation receipt:", err)
			}
			if failAt == "marker" {
				if persisted.Annotations[nativeStartAnnotation] != "" {
					t.Fatal("failed marker write was treated as persisted")
				}
				return
			}
			if persisted.Annotations[nativeStartAnnotation] == "" {
				t.Fatal("uncertain release lost its durable start marker")
			}
			if _, err := f.r.reconcileKubernetesSandbox(context.Background(), f.current(t)); err == nil {
				t.Fatal("restart automatically replayed an uncertain start")
			}
			if ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 {
				t.Fatal("restart scaled an already submitted start")
			}
		})
	}
}

func TestNativeSandboxSourcesDefaultRemainsHeld(t *testing.T) {
	f := newNativeSandboxFixture(t, false)
	f.reconcileOK(t)
	f.assertNotReady(t)
	if !slices.Contains(f.current(t).Finalizers, sandbox.HoldFinalizer) || f.installer.stages != 0 {
		t.Fatal("enabling sources without nativeExecution permitted execution")
	}
	var workloads appsv1.StatefulSetList
	if err := f.kube.List(context.Background(), &workloads, client.InNamespace(instanceNamespace(f.agent.Name))); err != nil || len(workloads.Items) != 0 {
		t.Fatal("default held path created native compute:", err)
	}
}

func nativeSandboxReleasesHold(t *testing.T, agent *api.Agent, patch client.Patch) bool {
	t.Helper()
	raw, err := patch.Data(agent)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Metadata struct {
			Finalizers []string `json:"finalizers"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	return body.Metadata.Finalizers != nil && !slices.Contains(body.Metadata.Finalizers, sandbox.HoldFinalizer)
}

// Only API object identities and StatefulSet-controller output are simulated.
// Preparation, chart rendering, Gateway projection, start and observation are real
// controller routines; these tests contain no guest-external enforcement evidence.
type nativeSandboxFixture struct {
	r         *AgentReconciler
	kube      client.WithWatch
	gateway   *fakeAIGateway
	installer *nativeSandboxInstaller
	agent     *api.Agent
}

func newNativeSandboxFixture(t *testing.T, native bool) *nativeSandboxFixture {
	t.Helper()
	return newSandboxRuntimeFixtureForAgent(t, native, nil, false, "openclaw", true, false,
		withNativePublishedSkills(t))
}

func withNativePublishedSkills(t *testing.T) func(*AgentReconciler, client.Client) {
	t.Helper()
	return func(r *AgentReconciler, kube client.Client) {
		t.Helper()
		teams := &api.TeamList{}
		if err := kube.List(context.Background(), teams); err != nil || len(teams.Items) != 1 {
			t.Fatal("publication requires exactly one fixture Team:", err)
		}
		team := &teams.Items[0]
		// This fixture intentionally consumes published storage even with an
		// empty package selection; a catalog alone no longer selects it.
		team.Spec.SkillStorage = &api.SkillStorageSpec{ResourceGroup: "skills-example", StorageAccount: "publishedskills", ContainerName: "skills-v1"}
		if err := kube.Update(context.Background(), team); err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(map[string]any{
			"version": "v1", "teams": map[string]any{team.Name: map[string]any{
				"teamUID": team.UID, "revision": "published-v1",
				"source": map[string]any{
					"resourceGroup": "skills-example", "storageAccount": "publishedskills",
					"containerName": "skills-v1", "storageClassName": "azureblob-nfs-premium", "storageSize": "1Gi",
				},
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		catalog := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "skills-catalog", Namespace: r.OperatorNamespace},
			Data:       map[string]string{SandboxSkillsCatalogKey: string(raw)},
		}
		if err := kube.Create(context.Background(), catalog); err != nil {
			t.Fatal(err)
		}
		r.SandboxPreparation.bootstrap.SkillsCatalogConfigMapName = catalog.Name
		r.SandboxPreparation.Prerequisites.(*KubernetesSandboxPrerequisites).SkillsCatalogConfigMapName = catalog.Name
	}
}

func newNativeSandboxSkillsFixture(t *testing.T, native bool, skills []string) *nativeSandboxFixture {
	return newSandboxRuntimeFixture(t, native, skills, false)
}

func newSandboxRuntimeFixture(t *testing.T, native bool, skills []string, reconciled bool) *nativeSandboxFixture {
	return newSandboxRuntimeFixtureForAgent(t, native, skills, reconciled, "openclaw", true, false)
}

func newSandboxRuntimeFixtureForAgent(t *testing.T, native bool, skills []string, reconciled bool, kind string, shared, copilot bool,
	setup ...func(*AgentReconciler, client.Client),
) *nativeSandboxFixture {
	t.Helper()
	f, input := newUnreconciledSandboxRuntimeFixture(t, native, skills, reconciled, kind, shared, copilot, setup...)
	if native && !reconciled {
		f.seedRetainedBootstrap(t)
	} else {
		f.reconcileOK(t)
	}
	namespace := &corev1.Namespace{}
	if err := f.kube.Get(context.Background(), client.ObjectKey{Name: instanceNamespace(input.Agent.Name)}, namespace); err != nil {
		t.Fatal("cold reconciliation did not bootstrap the namespace:", err)
	}
	f.assertNotReady(t)
	// The fake API has no namespace controller to create the default account.
	serviceAccount := input.ServiceAccount.DeepCopy()
	serviceAccount.UID, serviceAccount.ResourceVersion = "", ""
	if err := f.kube.Create(context.Background(), serviceAccount); err != nil && !(reconciled && apierrors.IsAlreadyExists(err)) {
		t.Fatal(err)
	}
	if len(skills) != 0 && f.r.SandboxPreparation.bootstrap.SkillsCatalogConfigMapName == "" {
		claim := input.Input.Skills.DeepCopy()
		claim.UID, claim.ResourceVersion = "", ""
		claim.OwnerReferences = []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "Team", Name: input.Team.Name, UID: input.Team.UID}}
		if err := f.kube.Create(context.Background(), claim); err != nil {
			t.Fatal(err)
		}
	}
	if reconciled {
		f.reconcileOK(t)
	}
	return f
}

func newUnreconciledSandboxRuntimeFixture(t *testing.T, native bool, skills []string, reconciled bool, kind string, shared, copilot bool,
	setup ...func(*AgentReconciler, client.Client),
) (*nativeSandboxFixture, testfixture.Fixture) {
	t.Helper()
	input := testfixture.New(kind)
	input.Team.Spec.Skills = skills
	input.Policy.Spec.Skills.Allowed = slices.Clone(skills)
	chrt, err := loader.Load(filepath.Join("..", "..", "..", "cetirun-chart"))
	if err != nil {
		t.Fatal(err)
	}
	input.Agent.Finalizers = []string{api.Finalizer}
	input.Agent.Spec.ModelAccess = modelAccess(shared, copilot)
	input.Agent.Spec.Helm.Version = chrt.Metadata.Version
	input.Agent.Spec.Agent.Image = ptr.To("registry.example.com/" + kind + "-gateway:" + chrt.Metadata.AppVersion)
	input.Agent.Spec.Agent.Port = ptr.To[int32](18789)
	if kind == "hermes" {
		input.Agent.Spec.Agent.Port = ptr.To[int32](8642)
	}
	if kind == "custom" {
		input.Agent.Spec.Agent.Image = ptr.To("registry.example.com/custom-agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		input.Agent.Spec.Agent.Port = ptr.To[int32](8080)
		input.Agent.Spec.GatewayTokenSecretRef = ""
	}
	input.Agent.Status.Policy = &api.PolicyStatus{}
	input.Team.Status = api.TeamStatus{ObservedGeneration: 1, Conditions: []metav1.Condition{{
		Type: api.ConditionTeamAccepted, Status: metav1.ConditionTrue, ObservedGeneration: 1,
	}}}
	input.Policy.Spec.ModelAccess = aiGatewayTestPolicy().Spec.ModelAccess
	input.Policy.Spec.ModelAccess.Copilot.Allowed = copilot
	input.Policy.Spec.Network.Ingress.Mode = api.AgentPolicyIngressAuthenticated
	if native {
		input.Policy.Spec.Harnesses.Allowed = []api.AgentType{api.AgentType(kind)}
	}
	input.Policy.Status = api.AgentPolicyStatus{ObservedGeneration: 1, Conditions: []metav1.Condition{{
		Type: api.ConditionAgentPolicyAccepted, Status: metav1.ConditionTrue, ObservedGeneration: 1,
	}}}
	input.Sandbox.Spec.Storage.Home.AccessMode = corev1.ReadWriteOncePod
	sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "example-csi", UID: "storage-uid"}, Provisioner: "csi.example.com"}
	r, base, gateway := aiGatewayTestReconciler(t, input.Agent, input.Team, input.Policy, input.Sandbox, input.Input.RuntimeClass, sc)
	cilium := schema.GroupVersion{Group: "cilium.io", Version: "v2"}
	r.Scheme.AddKnownTypeWithName(cilium.WithKind("CiliumNetworkPolicy"), &unstructured.Unstructured{})
	r.Scheme.AddKnownTypeWithName(cilium.WithKind("CiliumNetworkPolicyList"), &unstructured.UnstructuredList{})
	var creates int
	kube := interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
			if object.GetUID() == "" {
				creates++
				object.SetUID(types.UID(fmt.Sprintf("native-synthetic-%d", creates)))
			}
			if err := c.Create(ctx, object, opts...); err != nil {
				return err
			}
			if namespace, ok := object.(*corev1.Namespace); reconciled && ok {
				return c.Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
					Name: "default", Namespace: namespace.Name, UID: types.UID("synthetic-default-" + namespace.Name),
				}})
			}
			return nil
		},
	})
	r.Client, r.APIReader = kube, kube
	r.OperatorSAName, r.ChartRegistry, r.ChartVersion = "cetirun-operator", "registry.example.com", chrt.Metadata.Version
	r.DefaultModel, r.IstioGateway = testSharedModelOne, "platform/ingress"
	capabilities := input.Input.Resolution.Capabilities
	capabilities.HomeAccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod}
	capabilities.AuthenticatedIngress = true
	platform := sandboxpolicy.Platform{
		Adapter: sandboxpolicy.AdapterRegistration{Name: sandboxpolicy.AzureCNIOverlayCilium, Qualified: true, Available: true, Revision: "synthetic-only"},
		Topology: sandboxpolicy.Topology{
			Revision: "synthetic-topology", ManagedPeersComplete: true,
			PodCIDRs: []string{"10.244.0.0/16"}, ServiceCIDRs: []string{"10.96.0.0/12"},
			NodeCIDRs: []string{"192.168.0.0/16"}, ManagementCIDRs: []string{"10.1.0.0/16"},
			MetadataCIDRs: []string{"169.254.169.254/32"}, DNSResolverIPs: []string{"10.96.0.10"},
			DNS: []sandboxpolicy.Peer{{Name: "dns", Namespace: "kube-system", UID: "dns-uid", IPs: []string{"10.96.0.10"}}},
			Gateway: []sandboxpolicy.Destination{{
				Peer:  sandboxpolicy.Peer{Name: "gateway", Namespace: r.OperatorNamespace, UID: "gateway-uid", IPs: []string{"10.96.1.50"}},
				Ports: []api.AgentPolicyPort{{Port: 4000, Protocol: corev1.ProtocolTCP}},
			}},
			Ingress: []sandboxpolicy.Destination{{
				Peer:  sandboxpolicy.Peer{Name: "ingress", Namespace: "platform", UID: "ingress-uid", IPs: []string{"10.96.1.60"}},
				Ports: []api.AgentPolicyPort{{Port: 18789, Protocol: corev1.ProtocolTCP}},
			}},
		},
	}
	if native {
		capabilities.Profile = sandbox.ProfileNativeSingleStart
		capabilities.EnforcementObservation, capabilities.RestrictiveFailure = false, false
		platform.Adapter.Name, platform.Adapter.Qualified = sandboxpolicy.Cilium, false
	}
	document := SandboxPlatformConfiguration{
		Version: "v1", Registrations: map[api.SandboxType]sandbox.Registration{api.SandboxKata: {Capabilities: capabilities, Available: true}},
		Platforms: map[api.SandboxType]sandboxpolicy.Platform{api.SandboxKata: platform},
		RuntimeClasses: map[string]SandboxRuntimePrerequisite{
			input.Input.RuntimeClass.Name: {UID: input.Input.RuntimeClass.UID, Handler: input.Input.RuntimeClass.Handler},
		},
		StorageClasses: map[string]SandboxStoragePrerequisite{sc.Name: {UID: sc.UID, Provisioner: sc.Provisioner}},
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := kube.Create(context.Background(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Namespace: r.OperatorNamespace, Name: "sandbox-platform",
	}, Data: map[string]string{SandboxPlatformConfigurationKey: string(raw)}}); err != nil {
		t.Fatal(err)
	}
	config, err := ParseSandboxSourcesConfig(nativeSandboxSourcesJSON)
	if err != nil {
		t.Fatal(err)
	}
	config.NativeExecution = native
	installer := &nativeSandboxInstaller{kube: kube, chart: chrt}
	if reconciled {
		r.HelmClient = &sharedSandboxInstaller{nativeSandboxInstaller: installer}
	}
	r.SandboxPreparation, err = NewKubernetesSandboxPreparation(config, kube, installer, r.OperatorNamespace, chrt.Metadata.Version)
	if err != nil {
		t.Fatal(err)
	}
	for _, configure := range setup {
		configure(r, kube)
	}
	f := &nativeSandboxFixture{r: r, kube: kube, gateway: gateway, installer: installer, agent: input.Agent}
	return f, input
}

func (f *nativeSandboxFixture) seedRetainedBootstrap(t *testing.T) {
	t.Helper()
	// Retained-state tests cannot use the new common-lifetime creation path.
	ctx := context.Background()
	policy, err := f.r.reconcilePolicy(ctx, f.current(t))
	if err != nil || !policy.authorized {
		t.Fatal("retained fixture requires an authorized policy:", err)
	}
	if _, err := f.r.holdSandboxWithPolicyFence(ctx, f.current(t), nil); err != nil {
		t.Fatal("seed retained Sandbox bootstrap:", err)
	}
}

func (f *nativeSandboxFixture) reconcile() (ctrl.Result, error) {
	return f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: f.agent.Name}})
}

func (f *nativeSandboxFixture) reconcileOK(t *testing.T) {
	t.Helper()
	if _, err := f.reconcile(); err != nil {
		t.Fatal("reconcile native Sandbox:", err)
	}
}

func (f *nativeSandboxFixture) finishNamespaceGC(t *testing.T) {
	t.Helper()
	f.reconcileOK(t)
	if _, err := f.reconcile(); err == nil || !strings.Contains(err.Error(), "Sandbox skills claim must be absent") {
		t.Fatal("publication cleanup did not wait for namespace garbage collection:", err)
	}
	ctx := context.Background()
	namespace := instanceNamespace(f.agent.Name)
	if err := f.kube.Get(ctx, client.ObjectKey{Name: namespace}, &corev1.Namespace{}); !apierrors.IsNotFound(err) {
		t.Fatal("cannot complete namespace GC before ordinary namespace deletion:", err)
	}
	pods := &corev1.PodList{}
	if err := f.kube.List(ctx, pods, client.InNamespace(namespace)); err != nil || len(pods.Items) != 0 {
		t.Fatal("cannot complete namespace GC while Pods remain:", err)
	}
	// The fake API has no namespace controller to remove namespaced claims.
	claims := &corev1.PersistentVolumeClaimList{}
	if err := f.kube.List(ctx, claims, client.InNamespace(namespace)); err != nil {
		t.Fatal(err)
	}
	for i := range claims.Items {
		if err := f.kube.Delete(ctx, &claims.Items[i]); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *nativeSandboxFixture) current(t *testing.T) *api.Agent {
	t.Helper()
	agent := &api.Agent{}
	if err := f.kube.Get(context.Background(), client.ObjectKey{Name: f.agent.Name}, agent); err != nil {
		t.Fatal(err)
	}
	return agent
}

func (f *nativeSandboxFixture) workload(t *testing.T) *appsv1.StatefulSet {
	t.Helper()
	workload := &appsv1.StatefulSet{}
	if err := f.kube.Get(context.Background(), client.ObjectKey{
		Namespace: instanceNamespace(f.agent.Name), Name: instanceServiceName(f.agent.Name),
	}, workload); err != nil {
		t.Fatal(err)
	}
	return workload
}

func (f *nativeSandboxFixture) expectation(t *testing.T) (*corev1.ConfigMap, *w.Expectation) {
	t.Helper()
	state, expected, err := f.r.readKubernetesSandboxExpectation(context.Background(), f.current(t))
	if err != nil {
		t.Fatal(err)
	}
	return state, expected
}

func (f *nativeSandboxFixture) prepare(t *testing.T) {
	t.Helper()
	f.projectGateway(t)
	if err := f.r.prepareSandboxHeld(context.Background(), f.current(t)); err != nil {
		t.Fatal("prepare actual held chart:", err)
	}
}

func (f *nativeSandboxFixture) projectGateway(t *testing.T) {
	t.Helper()
	agent := f.current(t)
	if err := f.r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal("project typed Gateway:", err)
	}
	if err := f.r.updateStatus(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	if !sandboxGatewayProjectionReady(f.current(t)) {
		t.Fatal("fake Gateway did not produce a current typed projection")
	}
}

func (f *nativeSandboxFixture) ready(t *testing.T) {
	t.Helper()
	f.prepare(t)
	f.reconcileOK(t)
	f.createPod(t, true)
	f.reconcileOK(t)
	f.assertReady(t)
}

func (f *nativeSandboxFixture) createPod(t *testing.T, ready bool) *corev1.Pod {
	t.Helper()
	_, expected := f.expectation(t)
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "native-test-node", UID: "native-node-uid"},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	}
	if err := f.kube.Create(context.Background(), node); err != nil {
		t.Fatal(err)
	}
	pod := testfixture.Pod(*expected)
	pod.UID, pod.Spec.NodeName, pod.Status.Phase = "native-pod-uid", node.Name, corev1.PodRunning
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}
	if err := f.kube.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	return pod
}

func (f *nativeSandboxFixture) pod(t *testing.T) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{}
	if err := f.kube.Get(context.Background(), client.ObjectKey{
		Namespace: instanceNamespace(f.agent.Name), Name: instanceServiceName(f.agent.Name) + "-0",
	}, pod); err != nil {
		t.Fatal(err)
	}
	return pod
}

func (f *nativeSandboxFixture) assertNotReady(t *testing.T) {
	t.Helper()
	agent := f.current(t)
	if agent.Status.Phase == api.PhaseReady || agent.Status.AccessURL != "" ||
		apimeta.IsStatusConditionTrue(agent.Status.Conditions, api.ConditionReady) ||
		apimeta.IsStatusConditionTrue(agent.Status.Conditions, api.ConditionSandboxReady) {
		t.Fatal("unverified native workload retained Ready or external access")
	}
}

func (f *nativeSandboxFixture) assertReady(t *testing.T) {
	t.Helper()
	agent := f.current(t)
	if agent.Status.Phase != api.PhaseReady || agent.Status.AccessURL == "" || agent.Status.Sandbox == nil ||
		!apimeta.IsStatusConditionTrue(agent.Status.Conditions, api.ConditionReady) ||
		!apimeta.IsStatusConditionTrue(agent.Status.Conditions, api.ConditionSandboxReady) ||
		!apimeta.IsStatusConditionTrue(agent.Status.Conditions, api.ConditionIngressReady) {
		t.Fatalf("current native Pod did not become Ready: phase=%s conditions=%v", agent.Status.Phase, agent.Status.Conditions)
	}
	_, expected := f.expectation(t)
	if agent.Status.Namespace != expected.Namespace.Name {
		t.Fatalf("Ready namespace = %q, want verified namespace %q", agent.Status.Namespace, expected.Namespace.Name)
	}
	if expected.PodUID != f.pod(t).UID || !expected.SinglePod || expected.ExecutionHeld {
		t.Fatal("Ready was not tied to the admitted single Pod UID")
	}
}

func (f *nativeSandboxFixture) assertGatewayProjection(t *testing.T, expected *w.Expectation) {
	t.Helper()
	agent := f.current(t)
	secret := &corev1.Secret{}
	if err := f.kube.Get(context.Background(), client.ObjectKey{
		Namespace: expected.Namespace.Name, Name: aiGatewayKeySecretName,
	}, secret); err != nil {
		t.Fatal(err)
	}
	key := f.gateway.keys[aigateway.Token(string(secret.Data[aiGatewayKeySecretField]))]
	if !sandboxBuiltinOwned(secret, agent) || secret.Labels[aiGatewayAgentUIDLabel] != string(agent.UID) ||
		!slices.Contains(expected.Sources, sandboxSource(secret, "v1", "Secret")) ||
		key == nil || key.Blocked || len(f.gateway.generated) != 1 {
		t.Fatal("prepared Gateway key is not owned, pinned and verified with the fake Gateway")
	}
	projected := false
	for _, container := range expected.Template.Spec.Containers {
		if container.Name != f.installer.chart.Metadata.Name {
			continue
		}
		if container.ReadinessProbe == nil {
			t.Fatal("rendered native application has no readiness probe")
		}
		for _, env := range container.Env {
			if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil && env.ValueFrom.SecretKeyRef.Name == secret.Name {
				if env.Name != "OPENAI_API_KEY" || env.Value != "" ||
					env.ValueFrom.SecretKeyRef.Key != aiGatewayKeySecretField || ptr.Deref(env.ValueFrom.SecretKeyRef.Optional, false) {
					t.Fatal("Gateway key projection is not a required typed Secret reference")
				}
				projected = true
			}
		}
	}
	manifest, err := f.installer.prepared.SandboxManifest()
	if err != nil {
		t.Fatal(err)
	}
	if !projected || !bytes.Contains(manifest, []byte(testSharedModelOne)) ||
		bytes.Contains(manifest, secret.Data[aiGatewayKeySecretField]) {
		t.Fatal("real prepared chart did not use the shared model and separate typed key Secret")
	}
	var rawConfig string
	for _, volume := range expected.Template.Spec.Volumes {
		if volume.Name != "config" || volume.ConfigMap == nil {
			continue
		}
		configMap := &corev1.ConfigMap{}
		if err := f.kube.Get(context.Background(), client.ObjectKey{
			Namespace: expected.Namespace.Name, Name: volume.ConfigMap.Name,
		}, configMap); err != nil {
			t.Fatal(err)
		}
		rawConfig = configMap.Data["openclaw.json"]
	}
	var nativeConfig struct {
		Gateway struct {
			TrustedProxies []string `json:"trustedProxies"`
		} `json:"gateway"`
		Auth struct {
			Profiles map[string]json.RawMessage `json:"profiles"`
		} `json:"auth"`
		Models struct {
			Providers map[string]struct {
				BaseURL string `json:"baseUrl"`
				APIKey  struct {
					Source   string `json:"source"`
					Provider string `json:"provider"`
					ID       string `json:"id"`
				} `json:"apiKey"`
				Models []struct {
					ID string `json:"id"`
				} `json:"models"`
			} `json:"providers"`
		} `json:"models"`
	}
	if err := json.Unmarshal([]byte(rawConfig), &nativeConfig); err != nil {
		t.Fatal("prepared OpenClaw config is not the typed native Gateway shape:", err)
	}
	if !slices.Equal(nativeConfig.Gateway.TrustedProxies, []string{"10.96.1.60"}) {
		t.Fatal("native OpenClaw proxy trust differs from the approved ingress peer IPs")
	}
	provider, exists := nativeConfig.Models.Providers["openai"]
	if nativeConfig.Auth.Profiles == nil || len(nativeConfig.Auth.Profiles) != 0 ||
		!exists || len(nativeConfig.Models.Providers) != 1 || provider.BaseURL != testAIGatewayURL+"/v1" ||
		provider.APIKey.Source != "env" || provider.APIKey.Provider != "default" || provider.APIKey.ID != "OPENAI_API_KEY" ||
		len(provider.Models) != 1 || provider.Models[0].ID != testSharedModelOne {
		t.Fatal("prepared native config retained direct model auth or differs from the typed Gateway projection")
	}
}

type nativeSandboxInstaller struct {
	kube     client.Client
	chart    *chart.Chart
	prepared *helm.PreparedRelease
	digest   string
	stages   int
}

func (i *nativeSandboxInstaller) PrepareSandbox(_ string, platform, user map[string]any, version string, bundle *w.Bundle) (*helm.PreparedRelease, error) {
	if version != i.chart.Metadata.Version {
		return nil, fmt.Errorf("test chart version mismatch")
	}
	return helm.PrepareSandboxChart(i.chart, platform, user, bundle)
}

func (i *nativeSandboxInstaller) StageSandbox(ctx context.Context, namespace, _ string, prepared *helm.PreparedRelease, guard func(context.Context) error) error {
	if err := guard(ctx); err != nil {
		return err
	}
	manifest, err := prepared.SandboxManifest()
	if err != nil {
		return err
	}
	decoder := kubeyaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifest), 4096)
	var workloadKey client.ObjectKey
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
		if object.GetKind() == "StatefulSet" {
			replicas, found, err := unstructured.NestedInt64(object.Object, "spec", "replicas")
			if err != nil || !found || replicas != 0 {
				return fmt.Errorf("test installer only stages held workloads")
			}
			if workloadKey.Name != "" {
				return fmt.Errorf("test installer requires exactly one rendered StatefulSet")
			}
			workloadKey = client.ObjectKeyFromObject(object)
		}
		current := &unstructured.Unstructured{}
		current.SetGroupVersionKind(object.GroupVersionKind())
		if err := i.kube.Get(ctx, client.ObjectKeyFromObject(object), current); apierrors.IsNotFound(err) {
			if err := i.kube.Create(ctx, object); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else {
			object.SetUID(current.GetUID())
			object.SetResourceVersion(current.GetResourceVersion())
			if err := i.kube.Update(ctx, object); err != nil {
				return err
			}
		}
	}
	if workloadKey.Name == "" || workloadKey.Namespace != namespace {
		return fmt.Errorf("test installer requires a StatefulSet in the release namespace")
	}
	workload := &appsv1.StatefulSet{}
	if err := i.kube.Get(ctx, workloadKey, workload); err != nil {
		return err
	}
	template, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&workload.Spec.Template)
	if err != nil {
		return err
	}
	template["$patch"] = "replace"
	raw, err := json.Marshal(map[string]any{"spec": map[string]any{"template": template}})
	if err != nil {
		return err
	}
	revision := &appsv1.ControllerRevision{ObjectMeta: metav1.ObjectMeta{
		Namespace: namespace, Name: workload.Name + "-native-revision",
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(workload, appsv1.SchemeGroupVersion.WithKind("StatefulSet"))},
	}, Revision: 1, Data: runtime.RawExtension{Raw: raw}}
	if err := i.kube.Create(ctx, revision); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	workload.Status.UpdateRevision, workload.Status.ObservedGeneration = revision.Name, workload.Generation
	if err := i.kube.Status().Update(ctx, workload); err != nil {
		return err
	}
	if err := guard(ctx); err != nil {
		return err
	}
	i.prepared, i.digest = prepared, fmt.Sprintf("%x", sha256.Sum256(manifest))
	i.stages++
	return nil
}

func (i *nativeSandboxInstaller) SandboxStageDigest(ctx context.Context, _, _ string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if i.digest == "" {
		return "", errors.New("synthetic staged release is absent")
	}
	return i.digest, nil
}
