// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	"slices"
	"strings"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	sandboxadmission "github.com/Azure/CetiRun/app/cetirun-operator/internal/admission"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	"github.com/go-logr/logr"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// Exercises retained single-start state. The API server, RBAC, both Agent webhooks,
// Pod admission, controller writes,
// chart rendering and Helm SDK persistence are real. Platform declarations and
// Gateway/model responses are synthetic. Envtest has no StatefulSet controller,
// scheduler, kubelet, ServiceAccount controller or namespace/volume cleanup
// controllers; their ordinary API observations are supplied explicitly below.
// Nothing here runs OpenClaw, Kata, Cilium, a Gateway proxy or a useful task.
func TestNativeSandboxStartAPIServer(t *testing.T) {
	ctrl.SetLogger(logr.Discard())
	for _, enabled := range []bool{false, true} {
		name := "admission-disabled"
		if enabled {
			name = "served-hold-release-and-cleanup"
		}
		t.Run(name, func(t *testing.T) {
			h := newNativeStartAPIHarness(t, enabled, false)
			f := h.nativeSandboxFixture
			f.seedRetainedBootstrap(t)
			f.assertNotReady(t)
			agent := f.current(t)
			if !slices.Contains(agent.Finalizers, sandbox.HoldFinalizer) {
				t.Fatal("retained bootstrap did not persist the execution hold")
			}
			if err := h.admin.Get(h.ctx, client.ObjectKey{
				Namespace: instanceNamespace(agent.Name), Name: instanceServiceName(agent.Name),
			}, &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
				t.Fatalf("cold bootstrap started a workload before Gateway preparation: %v", err)
			}
			// An API-only environment does not create the namespace's default SA.
			h.create(t, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
				Namespace: instanceNamespace(agent.Name), Name: "default",
			}})
			f.prepare(t)
			state, held := f.expectation(t)
			receipt, _, err := f.r.readSandboxPreparationReceipt(h.ctx, f.current(t))
			if err != nil {
				t.Fatal(err)
			}
			if !held.ExecutionHeld || held.SinglePod || held.PodUID != "" ||
				ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 || receipt.Annotations[nativeStartAnnotation] != "" {
				t.Fatal("held preparation granted execution or consumed the one-shot marker")
			}
			f.assertNotReady(t)
			h.assertNoRoute(t)
			h.assertPodHeld(t, *held)

			_, startErr := f.reconcile()
			if !enabled {
				if startErr == nil || !strings.Contains(startErr.Error(), "release native Sandbox hold") ||
					!strings.Contains(startErr.Error(), "denied the request") {
					t.Fatalf("expected a served Agent admission denial at the hold-release PATCH, got: %v", startErr)
				}
				h.assertHoldAdmission(t, held.Revision, false)
				_, stillHeld := f.expectation(t)
				if !slices.Contains(f.current(t).Finalizers, sandbox.HoldFinalizer) ||
					!stillHeld.ExecutionHeld || stillHeld.SinglePod || ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 {
					t.Fatal("denied hold release enabled compute")
				}
				f.assertNotReady(t)
				h.assertNoRoute(t)
				h.assertPodHeld(t, *stillHeld)
				recorded, _, err := f.r.readSandboxPreparationReceipt(h.ctx, f.current(t))
				if err != nil || recorded.Annotations[nativeStartAnnotation] != held.Revision {
					t.Fatalf("denial lost the durable no-replay boundary: %v", err)
				}
				if _, err := f.reconcile(); err != nil && !strings.Contains(err.Error(), "single-start") {
					t.Fatalf("retry after denied native start failed outside its no-replay guard: %v", err)
				}
				if len(h.holdRequests) != 0 || ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 {
					t.Fatal("controller replayed a denied one-shot start")
				}
				f.assertNotReady(t)
				h.assertNoRoute(t)
				return
			}
			if startErr != nil {
				t.Fatalf("real AgentReconciler startup failed: %v", startErr)
			}
			h.assertHoldAdmission(t, held.Revision, true)
			runningState, running := f.expectation(t)
			if runningState.UID != state.UID || running.ExecutionHeld || !running.SinglePod || running.PodUID != "" ||
				ptr.Deref(f.workload(t).Spec.Replicas, -1) != 1 ||
				slices.Contains(f.current(t).Finalizers, sandbox.HoldFinalizer) {
				t.Fatal("served startup did not release the existing preparation for exactly one Pod")
			}
			f.assertNotReady(t)
			h.assertNoRoute(t)
			assertNativeStartDigests(t, running)

			pod := testfixture.Pod(*running)
			pod.Spec.Overhead = nil // RuntimeClass admission, not this fixture, adds overhead.
			bad := pod.DeepCopy()
			bad.Spec.Containers[0].Image = "registry.example.com/unapproved:1"
			if err := h.admin.Create(h.ctx, bad, client.DryRunAll); err == nil || !strings.Contains(err.Error(), "TemplateMismatch") {
				t.Fatalf("served Pod admission accepted a different image: %v", err)
			}
			h.create(t, pod) // The absent StatefulSet controller creates its first Pod.
			f.reconcileOK(t)
			f.assertNotReady(t)
			h.assertNoRoute(t)
			_, observed := f.expectation(t)
			if observed.PodUID != pod.UID || slices.Contains(f.pod(t).Finalizers, w.NativePodFinalizer) {
				t.Fatal("controller did not persist and release the real first-Pod lifetime")
			}
			claimBound := changeNativeClaimDuringRecheck(f, running.Home, func(ctx context.Context, key client.ObjectKey) error {
				return bindNativeTestClaim(ctx, h.admin, key)
			})
			h.observeReadyPod(t, pod)
			f.reconcileOK(t)
			if !claimBound() {
				t.Fatal("served startup did not exercise concurrent HOME binding")
			}
			f.assertReady(t)
			ready := f.current(t)
			if ready.Status.Namespace != running.Namespace.Name || ready.Status.Namespace != pod.Namespace ||
				!slices.ContainsFunc(running.Sources, func(source w.SourceReference) bool {
					return source.APIVersion == "v1" && source.Kind == "Secret" && source.UID != "" &&
						source.Namespace == ready.Status.Namespace && source.Name == ready.Spec.GatewayTokenSecretRef
				}) {
				t.Fatal("served Ready status cannot locate the pinned runtime Secret through the existing namespace/ref contract")
			}
			pods := &corev1.PodList{}
			if err := h.admin.List(h.ctx, pods, client.InNamespace(pod.Namespace)); err != nil || len(pods.Items) != 1 {
				t.Fatalf("native start must have exactly one admitted Pod: count=%d error=%v", len(pods.Items), err)
			}
			f.reconcileOK(t)
			if len(h.holdRequests) != 0 {
				t.Fatal("an unchanged running reconciliation submitted another hold release")
			}
			f.assertReady(t)
			h.cleanupAgent(t, pod, state, receipt)
		})
	}
}

type nativeStartAdmissionObservation struct {
	stage, username, revision, marker string
	allowed                           bool
	expected                          *w.Expectation
	replicas                          int32
	err                               error
}

type nativeStartAPIHarness struct {
	*nativeSandboxFixture
	ctx          context.Context
	admin        client.WithWatch
	member       client.Client
	holdRequests chan nativeStartAdmissionObservation
	localHelm    *nativeStartLocalHelm
	sources      SandboxSourcesConfig
}

func newNativeStartAPIHarness(t *testing.T, admissionNative, coordinated bool, network ...api.AgentPolicyNetwork) *nativeStartAPIHarness {
	t.Helper()
	return newNativeStartAPIHarnessWithHome(t, admissionNative, coordinated, corev1.ReadWriteOncePod, network...)
}

func newNativeStartAPIHarnessWithHome(t *testing.T, admissionNative, coordinated bool, homeMode corev1.PersistentVolumeAccessMode, network ...api.AgentPolicyNetwork) *nativeStartAPIHarness {
	t.Helper()
	return newNativeStartAPIHarnessForAgent(t, admissionNative, coordinated, homeMode, "openclaw", network...)
}

func newNativeStartAPIHarnessForAgent(t *testing.T, admissionNative, coordinated bool, homeMode corev1.PersistentVolumeAccessMode, kind string, network ...api.AgentPolicyNetwork) *nativeStartAPIHarness {
	t.Helper()
	return newNativeStartAPIHarnessConfigured(t, admissionNative, coordinated, homeMode, kind, nil, network...)
}

func newNativeStartAPIHarnessWithRegistration(t *testing.T, admissionNative, coordinated bool, homeMode corev1.PersistentVolumeAccessMode,
	configure func(*sandbox.Capabilities), network ...api.AgentPolicyNetwork,
) *nativeStartAPIHarness {
	t.Helper()
	return newNativeStartAPIHarnessConfigured(t, admissionNative, coordinated, homeMode, "openclaw", configure, network...)
}

func newNativeStartAPIHarnessConfigured(t *testing.T, admissionNative, coordinated bool, homeMode corev1.PersistentVolumeAccessMode,
	kind string, configure func(*sandbox.Capabilities), network ...api.AgentPolicyNetwork,
) *nativeStartAPIHarness {
	t.Helper()
	return newNativeStartAPIHarnessWithRuntime(t, admissionNative, coordinated, homeMode, kind, api.SandboxKata, configure, network...)
}

func newNativeStartAPIHarnessWithRuntime(t *testing.T, admissionNative, coordinated bool, homeMode corev1.PersistentVolumeAccessMode,
	kind string, runtimeType api.SandboxType, configure func(*sandbox.Capabilities), network ...api.AgentPolicyNetwork,
) *nativeStartAPIHarness {
	t.Helper()
	if len(network) > 1 {
		t.Fatal("native API fixture accepts at most one network policy")
	}
	if os.Getenv("USE_EXISTING_CLUSTER") == "true" {
		t.Fatal("native start regression refuses an existing cluster")
	}
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	for _, binary := range []string{"kube-apiserver", "etcd"} {
		info, err := os.Stat(filepath.Join(assets, binary))
		if assets == "" || err != nil || info.IsDir() || info.Mode()&0111 == 0 {
			t.Fatal("set KUBEBUILDER_ASSETS to preinstalled Kubernetes 1.37 envtest binaries")
		}
	}
	environment := &envtest.Environment{
		BinaryAssetsDirectory: assets, UseExistingCluster: ptr.To(false),
		CRDDirectoryPaths: []string{"../../config/crd/bases"}, ErrorIfCRDPathMissing: true,
		CRDs: []*apiextensionsv1.CustomResourceDefinition{
			nativeStartExternalCRD("cilium.io", "v2", "CiliumNetworkPolicy", "ciliumnetworkpolicies"),
			nativeStartExternalCRD("networking.istio.io", "v1beta1", "VirtualService", "virtualservices"),
			nativeStartExternalCRD("networking.istio.io", "v1beta1", "Gateway", "gateways"),
			nativeStartExternalCRD("networking.istio.io", "v1beta1", "DestinationRule", "destinationrules"),
		},
		ControlPlaneStartTimeout: time.Minute, ControlPlaneStopTimeout: 30 * time.Second,
	}
	environment.ControlPlane.GetAPIServer().Configure().Set("authorization-mode", "RBAC")
	environment.ControlPlane.GetAPIServer().Configure().Set("enable-admission-plugins", "PodTopologyLabels")
	cfg, err := environment.Start()
	if err != nil {
		t.Fatal(errors.Join(err, environment.Stop()))
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	cfg.QPS, cfg.Burst = 100, 200
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, api.AddToScheme, istiov1beta1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	cilium := schema.GroupVersion{Group: "cilium.io", Version: "v2"}
	scheme.AddKnownTypeWithName(cilium.WithKind("CiliumNetworkPolicy"), &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(cilium.WithKind("CiliumNetworkPolicyList"), &unstructured.UnstructuredList{})
	newClient := func(config *rest.Config) client.WithWatch {
		result, err := client.NewWithWatch(config, client.Options{Scheme: scheme})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	h := &nativeStartAPIHarness{admin: newClient(cfg), holdRequests: make(chan nativeStartAdmissionObservation, 8)}
	var cancel context.CancelFunc
	h.ctx, cancel = context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)
	const namespace, controllerSA, memberName = "cetirun-system", "cetirun-operator", "native-member"
	h.create(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})
	var coordinationName, coordinationUID string
	if coordinated {
		authority, err := sandboxcoordination.NewState(namespace, "native-start-coordination")
		if err != nil {
			t.Fatal(err)
		}
		h.create(t, authority)
		coordinationName, coordinationUID = authority.Name, string(authority.UID)
	}

	mux := http.NewServeMux()
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	operatorChart, err := loader.Load("../../chart")
	if err != nil {
		t.Fatal(err)
	}
	capabilities := chartutil.DefaultCapabilities.Copy()
	capabilities.KubeVersion = chartutil.KubeVersion{Version: "v1.37.0", Major: "1", Minor: "37"}
	values, err := chartutil.ToRenderValues(operatorChart, map[string]any{
		"cetiadmin": map[string]any{"enabled": false},
		"namespace": namespace, "serviceAccount": map[string]any{"name": controllerSA},
		"sandboxSources": map[string]any{
			"enabled": true, "nativeExecution": true, "bootstrapOwnedPrerequisites": true,
			"platformConfigMapName": "sandbox-platform", "skillsClaimName": "",
		},
		"sandboxAdmission": map[string]any{
			"enabled": true, "enforce": true, "podsEnabled": true, "validationMode": "strict",
			"tlsSecretName": "synthetic-admission-tls", "caBundle": base64.StdEncoding.EncodeToString(ca),
			"adminGroups":               []any{"system:masters"},
			"teamAssignments":           []any{map[string]any{"team": "team-example", "users": []any{memberName}}},
			"coordinationConfigMapName": coordinationName,
			"coordinationConfigMapUID":  coordinationUID,
		},
	}, chartutil.ReleaseOptions{Name: "native-start", Namespace: namespace}, capabilities)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := engine.Render(operatorChart, values)
	if err != nil {
		t.Fatal(err)
	}
	var admissionConfig sandboxadmission.Config
	var sourcesConfig SandboxSourcesConfig
	var webhooks []client.Object
	var files []string
	for file := range rendered {
		files = append(files, file)
	}
	slices.Sort(files)
	for _, file := range files {
		if strings.HasSuffix(file, "/NOTES.txt") {
			continue
		}
		for _, object := range nativeStartObjects(t, []byte(rendered[file])) {
			switch object.GetKind() {
			case "ServiceAccount", "ClusterRole", "ClusterRoleBinding", "Role", "RoleBinding":
				h.create(t, object)
			case "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding":
				h.create(t, object)
			case "ValidatingWebhookConfiguration":
				var rules admissionregistrationv1.ValidatingWebhookConfiguration
				if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &rules); err != nil {
					t.Fatal(err)
				}
				for i := range rules.Webhooks {
					config := &rules.Webhooks[i].ClientConfig
					if config.Service == nil || config.Service.Path == nil {
						t.Fatal("rendered validation webhook has no serving path")
					}
					config.URL, config.Service = ptr.To(server.URL+*config.Service.Path), nil
				}
				webhooks = append(webhooks, &rules)
			case "MutatingWebhookConfiguration":
				var rules admissionregistrationv1.MutatingWebhookConfiguration
				if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &rules); err != nil {
					t.Fatal(err)
				}
				for i := range rules.Webhooks {
					config := &rules.Webhooks[i].ClientConfig
					if config.Service == nil || config.Service.Path == nil {
						t.Fatal("rendered defaulting webhook has no serving path")
					}
					config.URL, config.Service = ptr.To(server.URL+*config.Service.Path), nil
				}
				webhooks = append(webhooks, &rules)
			case "Deployment":
				var deployment appsv1.Deployment
				if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &deployment); err != nil {
					t.Fatal(err)
				}
				for _, container := range deployment.Spec.Template.Spec.Containers {
					for _, variable := range container.Env {
						switch variable.Name {
						case "CETIRUN_SANDBOX_ADMISSION":
							admissionConfig, err = sandboxadmission.ParseConfig(variable.Value)
						case "CETIRUN_SANDBOX_SOURCES":
							sourcesConfig, err = ParseSandboxSourcesConfig(variable.Value)
						}
						if err != nil {
							t.Fatal(err)
						}
					}
				}
			}
		}
	}
	if !sourcesConfig.NativeExecution || !admissionConfig.NativeExecution || len(webhooks) != 3 {
		t.Fatal("rendered deployment did not wire native execution and both Agent webhooks plus Pod admission")
	}
	h.sources = sourcesConfig
	serviceAccountClient := func(name string) (client.WithWatch, *rest.Config) {
		scoped := rest.CopyConfig(cfg)
		scoped.Impersonate = rest.ImpersonationConfig{
			UserName: "system:serviceaccount:" + namespace + ":" + name,
			Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + namespace, "system:authenticated"},
		}
		return newClient(scoped), scoped
	}
	controllerClient, controllerConfig := serviceAccountClient(controllerSA)
	admissionClient, _ := serviceAccountClient("cetirun-sandbox-admission")
	h.assertAccess(t, controllerClient, authorizationv1.ResourceAttributes{
		Group: "apps", Resource: "statefulsets", Verb: "create", Namespace: namespace,
	}, true)
	h.assertAccess(t, controllerClient, authorizationv1.ResourceAttributes{
		Resource: "pods", Verb: "create", Namespace: namespace,
	}, false)
	h.assertAccess(t, admissionClient, authorizationv1.ResourceAttributes{
		Resource: "secrets", Verb: "get", Namespace: "cetirun-example", Name: "runtime-secret",
	}, false)
	user, err := environment.AddUser(envtest.User{Name: memberName, Groups: []string{"system:authenticated"}}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.member = newClient(user.Config())
	h.create(t, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "native-start-member"}, Rules: []rbacv1.PolicyRule{{
		APIGroups: []string{api.GroupVersion.Group}, Resources: []string{"agents"}, Verbs: []string{"get", "create", "patch", "delete"},
	}}})
	h.create(t, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "native-start-member"},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "native-start-member"},
		Subjects: []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: memberName}},
	})
	h.assertAccess(t, h.member, authorizationv1.ResourceAttributes{
		Group: api.GroupVersion.Group, Resource: "agents", Verb: "create",
	}, true)
	runtimeChart, err := loader.Load("../../../cetirun-chart")
	if err != nil {
		t.Fatal(err)
	}
	gateway := newFakeAIGateway()
	r := &AgentReconciler{
		Client: controllerClient, APIReader: controllerClient, Scheme: scheme, EventRecorder: record.NewFakeRecorder(100),
		OperatorNamespace: namespace, OperatorSAName: controllerSA, ChartVersion: runtimeChart.Metadata.Version,
		ChartRegistry: "registry.example.com", DomainSuffix: "example.com", DefaultModel: testSharedModelOne,
		IstioGateway: "platform/ingress", IstioRevision: "synthetic",
		AIGateway: aiGatewayTestConfig(), aiGatewayClientFactory: func(context.Context) (aiGatewayKeyClient, error) { return gateway, nil },
	}
	h.localHelm = &nativeStartLocalHelm{chart: runtimeChart, config: controllerConfig, admin: h.admin}
	r.SandboxPreparation, err = NewKubernetesSandboxPreparation(sourcesConfig, controllerClient, h.localHelm, namespace, r.ChartVersion)
	if err != nil {
		t.Fatal(err)
	}
	r.SandboxNativeCoordination, err = NewNativeSandboxCoordination(sourcesConfig, controllerClient, controllerClient, namespace)
	if err != nil {
		t.Fatal(err)
	}
	input := testfixture.New(kind)
	if runtimeType == api.SandboxContainer {
		input.Sandbox.Spec.Type = runtimeType
		input.Input.RuntimeClass = containerRuntimeClassForTest()
		input.Input.Resolution.Capabilities.RuntimeClassName = input.Input.RuntimeClass.Name
		input.Input.Resolution.Capabilities.PerContainerResourceLimits = true
		input.Input.Resolution.Capabilities.RequirePerContainerResourceLimits = true
	} else if runtimeType != api.SandboxKata {
		t.Fatal("unsupported native API fixture runtime")
	}
	h.nativeSandboxFixture = &nativeSandboxFixture{r: r, kube: controllerClient, gateway: gateway, agent: input.Agent}
	h.installPlatform(t, input, admissionNative, homeMode, configure, network...)
	// Disabled admission must still deny release of the retained execution hold.
	admissionConfig.NativeExecution = admissionNative
	handler, err := sandboxadmission.NewHandler(admissionClient, admissionConfig)
	if err != nil {
		t.Fatal(err)
	}
	podHandler, err := sandboxadmission.NewPodHandler(admissionClient, sandboxadmission.PodConfig{
		StateNamespace: namespace, CoordinationConfigMapName: coordinationName, CoordinationConfigMapUID: coordinationUID,
	})
	if err != nil {
		t.Fatal(err)
	}
	mux.Handle(sandboxadmission.DefaultingPath, &admission.Webhook{
		Handler: h.observeHoldAdmission("defaulting", handler.DefaultAgent),
	})
	mux.Handle(sandboxadmission.ValidationPath, &admission.Webhook{
		Handler: h.observeHoldAdmission("validation", handler.Validate),
	})
	mux.Handle(sandboxadmission.PodValidationPath, &admission.Webhook{Handler: podHandler})
	for _, object := range webhooks {
		h.create(t, object)
	}

	agent := input.Agent.DeepCopy()
	agent.ObjectMeta = metav1.ObjectMeta{Name: input.Agent.Name}
	agent.Spec.Team, agent.Spec.SandboxRef = "", ""
	agent.Spec.ModelAccess = modelAccess(true, false)
	agent.Spec.Helm.Version = runtimeChart.Metadata.Version
	agent.Spec.Agent.Image = ptr.To("registry.example.com/" + kind + "-gateway:" + runtimeChart.Metadata.AppVersion)
	agent.Spec.Agent.Port = ptr.To[int32](18789)
	if kind == "hermes" {
		agent.Spec.Agent.Port = ptr.To[int32](8642)
	}
	if kind == "custom" {
		agent.Spec.GatewayTokenSecretRef = ""
		agent.Spec.Agent.Image = ptr.To("registry.example.com/custom-agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		agent.Spec.Agent.Port = ptr.To[int32](8080)
		r.CustomBindings = NewKubernetesCustomBindings(r, CustomRuntimeConfig{
			Enabled: true, ApprovedImages: []string{*agent.Spec.Agent.Image},
		})
		r.CetiRunHelmValues = map[string]any{"ttyd": map[string]any{
			"enabled": false, "image": map[string]any{
				"repository": "registry.example.com/cetirun-terminal",
				"digest":     "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			},
		}}
	}
	var lastErr error
	if err := wait.PollUntilContextTimeout(h.ctx, 100*time.Millisecond, 20*time.Second, true, func(ctx context.Context) (bool, error) {
		probe := agent.DeepCopy()
		lastErr = h.member.Create(ctx, probe, client.DryRunAll)
		return lastErr == nil && sandboxbinding.Present(probe) && probe.Spec.Team == input.Team.Name &&
			probe.Spec.SandboxRef == input.Sandbox.Name, nil
	}); err != nil {
		t.Fatalf("served Agent defaulting/validation did not become ready: %v; last create: %v", err, lastErr)
	}
	if err := h.member.Create(h.ctx, agent); err != nil {
		t.Fatalf("create native Agent through served admission: %v", err)
	}
	if agent.UID == "" || !sandboxbinding.Present(agent) || agent.Spec.Team != input.Team.Name ||
		agent.Spec.SandboxRef != input.Sandbox.Name {
		t.Fatal("API creation did not persist the server-authorized default selection")
	}
	h.agent = agent
	return h
}

func (h *nativeStartAPIHarness) installPlatform(t *testing.T, input testfixture.Fixture, nativeProfile bool, homeMode corev1.PersistentVolumeAccessMode,
	configure func(*sandbox.Capabilities), network ...api.AgentPolicyNetwork,
) {
	t.Helper()
	runtimeClass := input.Input.RuntimeClass.DeepCopy()
	runtimeClass.ObjectMeta = metav1.ObjectMeta{Name: runtimeClass.Name}
	storageClass := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "example-csi"}, Provisioner: "csi.example.com"}
	h.create(t, runtimeClass)
	h.create(t, storageClass)
	capabilities := input.Input.Resolution.Capabilities
	capabilities.HomeAccessModes = []corev1.PersistentVolumeAccessMode{homeMode}
	capabilities.AuthenticatedIngress = true
	platform := sandboxpolicy.Platform{
		Adapter: sandboxpolicy.AdapterRegistration{Name: sandboxpolicy.AzureCNIOverlayCilium, Qualified: true, Available: true, Revision: "synthetic-only"},
		Topology: sandboxpolicy.Topology{
			Revision: "synthetic-topology", ManagedPeersComplete: true,
			PodCIDRs: []string{"10.244.0.0/16"}, ServiceCIDRs: []string{"10.96.0.0/12"},
			NodeCIDRs: []string{"192.168.0.0/16"}, ManagementCIDRs: []string{"10.1.0.0/16"},
			MetadataCIDRs: []string{"169.254.169.254/32"}, DNSResolverIPs: []string{"10.96.0.10"},
			DNS: []sandboxpolicy.Peer{{Name: "dns", Namespace: "kube-system", UID: "synthetic-dns", IPs: []string{"10.96.0.10"}}},
			Gateway: []sandboxpolicy.Destination{{
				Peer:  sandboxpolicy.Peer{Name: "gateway", Namespace: h.r.OperatorNamespace, UID: "synthetic-gateway", IPs: []string{"10.96.1.50"}},
				Ports: []api.AgentPolicyPort{{Port: 4000, Protocol: corev1.ProtocolTCP}},
			}},
			Ingress: []sandboxpolicy.Destination{{
				Peer:  sandboxpolicy.Peer{Name: "ingress", Namespace: "platform", UID: "synthetic-ingress", IPs: []string{"10.96.1.60"}},
				Ports: []api.AgentPolicyPort{{Port: 18789, Protocol: corev1.ProtocolTCP}},
			}},
		},
	}
	if nativeProfile {
		capabilities.Profile = sandbox.ProfileNativeSingleStart
		capabilities.EnforcementObservation, capabilities.RestrictiveFailure = false, false
		platform.Adapter.Name, platform.Adapter.Qualified = sandboxpolicy.Cilium, false
		input.Policy.Spec.Harnesses.Allowed = []api.AgentType{input.Agent.Spec.Agent.Type}
	}
	if len(network) == 1 {
		input.Policy.Spec.Network = *network[0].DeepCopy()
		capabilities.BlockedPorts = len(input.Policy.Spec.Network.BlockedPorts) != 0
	}
	if configure != nil {
		configure(&capabilities)
	}
	document := SandboxPlatformConfiguration{
		Version: "v1", Registrations: map[api.SandboxType]sandbox.Registration{input.Sandbox.Spec.Type: {Capabilities: capabilities, Available: true}},
		Platforms: map[api.SandboxType]sandboxpolicy.Platform{input.Sandbox.Spec.Type: platform},
		RuntimeClasses: map[string]SandboxRuntimePrerequisite{
			runtimeClass.Name: {UID: runtimeClass.UID, Handler: runtimeClass.Handler},
		},
		StorageClasses: map[string]SandboxStoragePrerequisite{
			storageClass.Name: {UID: storageClass.UID, Provisioner: storageClass.Provisioner},
		},
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	h.create(t, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Namespace: h.r.OperatorNamespace, Name: "sandbox-platform",
	}, Data: map[string]string{SandboxPlatformConfigurationKey: string(raw)}})
	input.Sandbox.Spec.Storage.Home.AccessMode = homeMode
	input.Policy.Spec.ModelAccess = aiGatewayTestPolicy().Spec.ModelAccess
	input.Policy.Spec.ModelAccess.Copilot.Allowed = false
	input.Policy.Spec.Network.Ingress.Mode = api.AgentPolicyIngressAuthenticated
	for _, object := range []client.Object{input.Sandbox, input.Policy, input.Team} {
		object.SetUID("")
		object.SetResourceVersion("")
		object.SetGeneration(0)
		h.create(t, object)
	}
	for _, step := range []struct {
		name string
		run  func(context.Context, ctrl.Request) (ctrl.Result, error)
	}{
		{input.Sandbox.Name, (&SandboxReconciler{Client: h.kube, APIReader: h.kube, Source: h.r.SandboxPreparation.Configuration, NativeExecution: true}).Reconcile},
		{input.Policy.Name, (&AgentPolicyReconciler{Client: h.kube, APIReader: h.kube, Source: h.r.SandboxPreparation.Configuration, NativeExecution: true}).Reconcile},
		{input.Team.Name, (&TeamReconciler{Client: h.kube, APIReader: h.kube}).Reconcile},
	} {
		if _, err := step.run(h.ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: step.name}}); err != nil {
			t.Fatalf("reconcile native platform configuration %s: %v", step.name, err)
		}
	}
}

func (h *nativeStartAPIHarness) create(t *testing.T, object client.Object) {
	t.Helper()
	if err := h.admin.Create(h.ctx, object); err != nil {
		t.Fatalf("create %T %s: %v", object, client.ObjectKeyFromObject(object), err)
	}
}

func (h *nativeStartAPIHarness) assertAccess(t *testing.T, caller client.Client, attributes authorizationv1.ResourceAttributes, allowed bool) {
	t.Helper()
	var last authorizationv1.SubjectAccessReviewStatus
	if err := wait.PollUntilContextTimeout(h.ctx, 100*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		review := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &attributes}}
		if err := caller.Create(ctx, review); err != nil {
			return false, err
		}
		last = review.Status
		return last.Allowed == allowed, nil
	}); err != nil {
		t.Fatalf("rendered RBAC for %+v: wanted allowed=%v, got %+v: %v", attributes, allowed, last, err)
	}
}

func (h *nativeStartAPIHarness) observeHoldAdmission(stage string, handle admission.HandlerFunc) admission.HandlerFunc {
	return func(ctx context.Context, request admission.Request) admission.Response {
		response := handle(ctx, request)
		var old, next api.Agent
		if request.Resource.Resource != "agents" || json.Unmarshal(request.OldObject.Raw, &old) != nil ||
			json.Unmarshal(request.Object.Raw, &next) != nil || !next.DeletionTimestamp.IsZero() ||
			!slices.Contains(old.Finalizers, sandbox.HoldFinalizer) || slices.Contains(next.Finalizers, sandbox.HoldFinalizer) {
			return response
		}
		observed := nativeStartAdmissionObservation{stage: stage, username: request.UserInfo.Username, allowed: response.Allowed}
		_, observed.expected, observed.err = h.r.readKubernetesSandboxExpectation(ctx, &old)
		if observed.err == nil {
			observed.revision = observed.expected.Revision
			receipt, _, err := h.r.readSandboxPreparationReceipt(ctx, &old)
			observed.err = err
			if err == nil {
				observed.marker = receipt.Annotations[nativeStartAnnotation]
			}
			workload := &appsv1.StatefulSet{}
			err = h.kube.Get(ctx, client.ObjectKey{Namespace: observed.expected.Namespace.Name, Name: observed.expected.Workload.Name}, workload)
			observed.err = errors.Join(observed.err, err)
			observed.replicas = ptr.Deref(workload.Spec.Replicas, -1)
		}
		h.holdRequests <- observed
		return response
	}
}

func (h *nativeStartAPIHarness) assertHoldAdmission(t *testing.T, revision string, allowed bool) {
	t.Helper()
	stages := map[string]bool{}
	for len(h.holdRequests) != 0 {
		observed := <-h.holdRequests
		if observed.err != nil || observed.expected == nil {
			t.Fatalf("observe %s hold-release admission: %v", observed.stage, observed.err)
		}
		if observed.username != "system:serviceaccount:"+h.r.OperatorNamespace+":"+h.r.OperatorSAName ||
			observed.revision != revision || observed.marker != revision || !observed.expected.ExecutionHeld ||
			observed.expected.SinglePod || observed.expected.PodUID != "" || observed.replicas != 0 {
			t.Fatalf("served %s hold release did not see the current still-held preparation and durable start marker: %+v", observed.stage, observed)
		}
		assertNativeStartDigests(t, observed.expected)
		if observed.stage == "validation" && observed.allowed != allowed {
			t.Fatalf("served hold-release validation allowed=%v, want %v", observed.allowed, allowed)
		}
		stages[observed.stage] = true
	}
	if !stages["defaulting"] || !stages["validation"] {
		t.Fatalf("real finalizer PATCH did not traverse both Agent webhooks: %v", stages)
	}
}

func assertNativeStartDigests(t *testing.T, expected *w.Expectation) {
	t.Helper()
	count := 0
	for _, source := range expected.Sources {
		if w.EffectiveSource(source) {
			count++
			if !w.ValidInputDigest(expected.SourceDigests[w.SourceKey(source)]) {
				t.Fatalf("native start omitted the effective digest for %s", w.SourceKey(source))
			}
		}
	}
	if count == 0 || len(expected.SourceDigests) != count {
		t.Fatal("native start digests do not exactly cover the selected effective sources")
	}
}

func (h *nativeStartAPIHarness) assertPodHeld(t *testing.T, expected w.Expectation) {
	t.Helper()
	pod := testfixture.Pod(expected)
	pod.Spec.Overhead = nil
	var lastErr error
	if err := wait.PollUntilContextTimeout(h.ctx, 100*time.Millisecond, 20*time.Second, true, func(ctx context.Context) (bool, error) {
		lastErr = h.admin.Create(ctx, pod.DeepCopy(), client.DryRunAll)
		return lastErr != nil && strings.Contains(lastErr.Error(), "ExecutionHeld"), nil
	}); err != nil {
		t.Fatalf("the actual Pod webhook did not enforce the execution hold: %v; last create: %v", err, lastErr)
	}
}

func (h *nativeStartAPIHarness) assertNoRoute(t *testing.T) {
	t.Helper()
	route := &istiov1beta1.VirtualService{}
	if err := h.admin.Get(h.ctx, client.ObjectKey{
		Namespace: instanceNamespace(h.agent.Name), Name: virtualServiceName(h.agent.Name),
	}, route); !apierrors.IsNotFound(err) {
		t.Fatalf("unready native Agent retained an external route: %v", err)
	}
}

func (h *nativeStartAPIHarness) observeReadyPod(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "synthetic-" + h.agent.Name + "-node"}}
	h.create(t, node)
	node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	if err := h.admin.Status().Update(h.ctx, node); err != nil {
		t.Fatal(err)
	}
	binding := &corev1.Binding{ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace, UID: pod.UID},
		Target: corev1.ObjectReference{Kind: "Node", Name: node.Name}}
	if err := h.admin.SubResource("binding").Create(h.ctx, pod, binding); err != nil {
		t.Fatalf("supply the absent scheduler's Pod binding: %v", err)
	}
	pod = h.pod(t)
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if err := h.admin.Status().Update(h.ctx, pod); err != nil {
		t.Fatalf("supply the absent kubelet's readiness observation: %v", err)
	}
}

func (h *nativeStartAPIHarness) cleanupAgent(t *testing.T, pod *corev1.Pod, state, receipt *corev1.ConfigMap) {
	t.Helper()
	agent := h.current(t)
	if err := h.member.Delete(h.ctx, agent, client.Preconditions{UID: &agent.UID, ResourceVersion: &agent.ResourceVersion}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.reconcile(); err == nil || !strings.Contains(err.Error(), "ordinary Pod termination") {
		t.Fatalf("cleanup did not wait for the existing Pod's ordinary termination: %v", err)
	}
	h.assertNotReady(t)
	h.assertNoRoute(t)
	if ptr.Deref(h.workload(t).Spec.Replicas, -1) != 0 || len(h.gateway.deletedTokens) == 0 {
		t.Fatal("cleanup did not stop compute and revoke Gateway access before waiting")
	}
	pod = h.pod(t)
	pod.Status.Phase = corev1.PodSucceeded
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	if err := h.admin.Status().Update(h.ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := h.admin.Delete(h.ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := wait.PollUntilContextTimeout(h.ctx, 100*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		err := h.admin.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{})
		return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
	}); err != nil {
		t.Fatalf("ordinary terminal Pod deletion did not complete: %v", err)
	}
	h.reconcileOK(t)
	namespace := &corev1.Namespace{}
	if err := h.admin.Get(h.ctx, client.ObjectKey{Name: pod.Namespace}, namespace); err != nil {
		t.Fatal(err)
	}
	if namespace.DeletionTimestamp.IsZero() || len(namespace.Finalizers) != 0 {
		t.Fatal("controller did not release its namespace protection before requesting deletion")
	}
	// Only the missing built-in namespace/volume controllers are represented
	// here. All CetiRun finalizers must already have been released by Reconcile.
	for _, list := range []client.ObjectList{
		&appsv1.StatefulSetList{}, &appsv1.ControllerRevisionList{}, &corev1.ServiceList{},
		&corev1.SecretList{}, &corev1.ConfigMapList{}, &corev1.PersistentVolumeClaimList{},
		&corev1.ServiceAccountList{}, &rbacv1.RoleList{}, &rbacv1.RoleBindingList{},
		&networkingv1.NetworkPolicyList{},
		&unstructured.UnstructuredList{Object: map[string]any{"apiVersion": "cilium.io/v2", "kind": "CiliumNetworkPolicyList"}},
	} {
		if err := h.admin.List(h.ctx, list, client.InNamespace(namespace.Name)); err != nil {
			t.Fatal(err)
		}
		if err := apimeta.EachListItem(list, func(item runtime.Object) error {
			object, ok := item.(client.Object)
			if !ok {
				return fmt.Errorf("namespace resource %T is not a client object", item)
			}
			for _, finalizer := range object.GetFinalizers() {
				if finalizer != "kubernetes.io/pvc-protection" {
					return fmt.Errorf("controller left %s on %T %s", finalizer, object, object.GetName())
				}
			}
			if len(object.GetFinalizers()) != 0 {
				before := object.DeepCopyObject().(client.Object)
				object.SetFinalizers(nil)
				if err := h.admin.Patch(h.ctx, object, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
					return err
				}
			}
			return h.admin.Delete(h.ctx, object, client.Preconditions{UID: ptr.To(object.GetUID()), ResourceVersion: ptr.To(object.GetResourceVersion())})
		}); err != nil {
			t.Fatal(err)
		}
	}
	namespace.Spec.Finalizers = nil
	if err := h.admin.SubResource("finalize").Update(h.ctx, namespace); err != nil {
		t.Fatalf("supply ordinary namespace finalization after dependent cleanup: %v", err)
	}
	h.reconcileOK(t)
	for _, object := range []client.Object{
		&api.Agent{ObjectMeta: metav1.ObjectMeta{Name: agent.Name}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace.Name}},
		state, receipt,
	} {
		if err := h.admin.Get(h.ctx, client.ObjectKeyFromObject(object), object); !apierrors.IsNotFound(err) {
			t.Fatalf("normal controller cleanup retained %T %s: %v", object, client.ObjectKeyFromObject(object), err)
		}
	}
}

func nativeStartExternalCRD(group, version, kind, plural string) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: plural + "." + group},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: group, Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{Kind: kind, ListKind: kind + "List", Plural: plural, Singular: strings.ToLower(kind)},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{Name: version, Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"spec": {Type: "object", XPreserveUnknownFields: ptr.To(true)},
					},
				}},
			}},
		},
	}
}

func nativeStartObjects(t *testing.T, manifest []byte) []*unstructured.Unstructured {
	t.Helper()
	var objects []*unstructured.Unstructured
	decoder := kubeyaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifest), 4096)
	for {
		object := &unstructured.Unstructured{}
		if err := decoder.Decode(object); errors.Is(err, io.EOF) {
			return objects
		} else if err != nil {
			t.Fatal(err)
		}
		if len(object.Object) != 0 {
			objects = append(objects, object)
		}
	}
}

// The production Client requires in-cluster credentials even for StageSandbox.
// This local-chart adapter uses real Helm actions/storage with the same sealed
// zero-replica manifest and an authoritative guard on every Helm mutation. It
// does not replace, wrap or allow any Agent or Pod admission decision.
type nativeStartLocalHelm struct {
	chart  *chart.Chart
	config *rest.Config
	admin  client.Client
}

func (h *nativeStartLocalHelm) PrepareSandbox(_ string, platform, user map[string]any, version string, bundle *w.Bundle) (*helm.PreparedRelease, error) {
	if version != h.chart.Metadata.Version {
		return nil, fmt.Errorf("local native chart version differs from the requested version")
	}
	return helm.PrepareSandboxChart(h.chart, platform, user, bundle)
}

func (h *nativeStartLocalHelm) actionConfig(namespace string, guard func(context.Context) error) (*action.Configuration, error) {
	config := rest.CopyConfig(h.config)
	if guard != nil {
		config.Wrap(func(base http.RoundTripper) http.RoundTripper {
			return nativeStartGuardedTransport{base: base, guard: guard}
		})
	}
	result := &action.Configuration{}
	err := result.Init(&nativeStartRESTGetter{config: config, namespace: namespace}, namespace, "secret", func(string, ...any) {})
	return result, err
}

func (h *nativeStartLocalHelm) StageSandbox(ctx context.Context, namespace, name string, prepared *helm.PreparedRelease, guard func(context.Context) error) error {
	if err := guard(ctx); err != nil {
		return err
	}
	manifest, err := prepared.SandboxManifest()
	if err != nil {
		return err
	}
	config, err := h.actionConfig(namespace, guard)
	if err != nil {
		return err
	}
	current, err := config.Releases.Last(name)
	if err == nil {
		if current.Info == nil || current.Info.Status != release.StatusDeployed || current.Manifest != string(manifest) {
			return fmt.Errorf("local native test only permits reuse of its exact deployed held release")
		}
		return guard(ctx)
	}
	if !errors.Is(err, driver.ErrReleaseNotFound) {
		return err
	}
	install := action.NewInstall(config)
	install.Namespace, install.ReleaseName = namespace, name
	install.DisableHooks, install.SkipCRDs = true, true
	install.PostRenderer = nativeStartSealedManifest(manifest)
	if _, err := install.RunWithContext(ctx, h.chart, prepared.Values); err != nil {
		return err
	}
	workload := &appsv1.StatefulSet{}
	alias, validNamespace := strings.CutPrefix(namespace, "cetirun-")
	if !validNamespace || name != helmReleaseName(alias) {
		return fmt.Errorf("local native release differs from its Agent namespace")
	}
	key := client.ObjectKey{Namespace: namespace, Name: instanceServiceName(alias)}
	if err := h.admin.Get(ctx, key, workload); err != nil {
		return err
	}
	if ptr.Deref(workload.Spec.Replicas, -1) != 0 {
		return fmt.Errorf("local Helm staging enabled a replica")
	}
	// StatefulSet history/status is the only observation synthesized at staging.
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
		Namespace: namespace, Name: workload.Name + "-envtest-revision",
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(workload, appsv1.SchemeGroupVersion.WithKind("StatefulSet"))},
	}, Revision: 1, Data: runtime.RawExtension{Raw: raw}}
	if err := h.admin.Create(ctx, revision); err != nil {
		return err
	}
	workload.Status.UpdateRevision, workload.Status.ObservedGeneration = revision.Name, workload.Generation
	if err := h.admin.Status().Update(ctx, workload); err != nil {
		return err
	}
	return guard(ctx)
}

func (h *nativeStartLocalHelm) SandboxStageDigest(ctx context.Context, namespace, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	config, err := h.actionConfig(namespace, nil)
	if err != nil {
		return "", err
	}
	current, err := config.Releases.Last(name)
	if err != nil {
		return "", err
	}
	if current.Info == nil || current.Info.Status != release.StatusDeployed {
		return "", fmt.Errorf("local native release is not durably deployed")
	}
	raw, err := json.Marshal(current)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw)), ctx.Err()
}

type nativeStartSealedManifest []byte

func (m nativeStartSealedManifest) Run(*bytes.Buffer) (*bytes.Buffer, error) {
	return bytes.NewBuffer(bytes.Clone(m)), nil
}

type nativeStartGuardedTransport struct {
	base  http.RoundTripper
	guard func(context.Context) error
}

func (r nativeStartGuardedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		if err := r.guard(request.Context()); err != nil {
			return nil, err
		}
	}
	return r.base.RoundTrip(request)
}

type nativeStartRESTGetter struct {
	config    *rest.Config
	namespace string
}

func (g *nativeStartRESTGetter) ToRESTConfig() (*rest.Config, error) {
	return rest.CopyConfig(g.config), nil
}

func (g *nativeStartRESTGetter) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	result, err := discovery.NewDiscoveryClientForConfig(g.config)
	if err != nil {
		return nil, err
	}
	return memory.NewMemCacheClient(result), nil
}

func (g *nativeStartRESTGetter) ToRESTMapper() (apimeta.RESTMapper, error) {
	discovery, err := g.ToDiscoveryClient()
	if err != nil {
		return nil, err
	}
	return restmapper.NewDeferredDiscoveryRESTMapper(discovery), nil
}

func (g *nativeStartRESTGetter) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	return clientcmd.NewDefaultClientConfig(clientcmdapi.Config{}, &clientcmd.ConfigOverrides{
		Context: clientcmdapi.Context{Namespace: g.namespace},
	})
}
