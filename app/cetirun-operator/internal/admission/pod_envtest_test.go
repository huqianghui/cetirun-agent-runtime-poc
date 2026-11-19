// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package sandboxadmission

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxplacement"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	"helm.sh/helm/v3/pkg/chart/loader"
	admissionv1 "k8s.io/api/admission/v1"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestPodGarbageCollectorAPIServerBoundaries(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	for _, name := range []string{"kube-apiserver", "etcd"} {
		if info, err := os.Stat(filepath.Join(assets, name)); assets == "" || err != nil || info.Mode()&0111 == 0 {
			t.Fatal("preinstalled Kubernetes envtest assets are required")
		}
	}
	env := &envtest.Environment{
		BinaryAssetsDirectory: assets, UseExistingCluster: ptr.To(false),
		ControlPlaneStartTimeout: time.Minute, ControlPlaneStopTimeout: 30 * time.Second,
	}
	env.ControlPlane.GetAPIServer().Configure().Set("authorization-mode", "RBAC")
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
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, rbacv1.AddToScheme, admissionregistrationv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	newClient := func(config *rest.Config) client.Client {
		t.Helper()
		result, err := client.New(config, client.Options{Scheme: scheme})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	cl := newClient(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	create := func(object client.Object) {
		t.Helper()
		if err := cl.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	namespace := "gc-protected"
	create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: namespace, Labels: map[string]string{w.ProtectedNamespaceLabel: "true"},
	}})
	create(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "gc-runner", Namespace: namespace}})
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "gc-fixture", Namespace: namespace},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""}, Resources: []string{"pods", "pods/status"},
			Verbs: []string{"get", "create", "update", "patch", "delete"},
		}},
	}
	create(role)
	usernames := []string{
		"system:kube-controller-manager",
		"system:serviceaccount:kube-system:generic-garbage-collector",
		"member@example.com", "admin@example.com",
		"system:kube-controller-manager-extra",
		"system:serviceaccount:other:generic-garbage-collector",
		"system:serviceaccount:kube-system:generic-garbage-collector-extra",
		"system:serviceaccount:cetirun-system:cetirun-operator-controller-manager",
	}
	callers := make([]client.Client, len(usernames))
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: role.Name, Namespace: namespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
	}
	for i, username := range usernames {
		user := envtest.User{Name: username}
		if username == "admin@example.com" {
			user.Groups = []string{"system:masters"}
		}
		authenticated, err := env.AddUser(user, cfg)
		if err != nil {
			t.Fatal(err)
		}
		callers[i] = newClient(authenticated.Config())
		binding.Subjects = append(binding.Subjects, rbacv1.Subject{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: username})
	}
	create(binding)
	seed := func(name string, finalizers []string, terminating bool) *corev1.Pod {
		t.Helper()
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Finalizers: finalizers},
			Spec: corev1.PodSpec{
				ServiceAccountName: "gc-runner", AutomountServiceAccountToken: ptr.To(false),
				Containers: []corev1.Container{{Name: "agent", Image: "example.com/agent:v1"}},
			},
		}
		create(pod)
		if terminating {
			if err := cl.Delete(ctx, pod, client.GracePeriodSeconds(0), client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil {
				t.Fatal(err)
			}
			if err := cl.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
				t.Fatal(err)
			}
			if pod.DeletionTimestamp.IsZero() || !slices.Contains(pod.Finalizers, metav1.FinalizerDeleteDependents) {
				t.Fatal("foreground deletion did not persist the built-in finalizer")
			}
		}
		return pod
	}
	// Seed historical Pods before enforcement. Envtest runs no garbage collector;
	// authenticated clients below perform its main-resource UPDATE and PATCH.
	others := []string{"example.com/first", "example.com/last"}
	guarded := seed("guarded", slices.Clone(others), true)
	active := seed("not-terminating", []string{metav1.FinalizerDeleteDependents}, false)
	cleanup := map[string]*corev1.Pod{}
	for _, phase := range []string{"reader-outage", "webhook-outage"} {
		for i := range 2 {
			for _, retainOthers := range []bool{false, true} {
				name := fmt.Sprintf("%s-%d-%t", phase, i, retainOthers)
				var finalizers []string
				if retainOthers {
					finalizers = slices.Clone(others)
				}
				cleanup[name] = seed(name, finalizers, true)
			}
		}
	}
	reader := &countingPodReader{Reader: &unavailablePodReader{}}
	handler, err := NewPodHandler(reader, PodConfig{StateNamespace: "operator-state"})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	webhook := &admission.Webhook{Handler: handler}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		webhook.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	_, _, rules := renderedPodAdmission(t, ca)
	rules.Webhooks[0].ClientConfig.Service = nil
	rules.Webhooks[0].ClientConfig.URL = ptr.To(server.URL + PodValidationPath)
	rules.Webhooks[0].TimeoutSeconds = ptr.To(int32(1))
	create(rules)
	var probeError error
	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 20*time.Second, true, func(ctx context.Context) (bool, error) {
		candidate := guarded.DeepCopy()
		err := cl.Update(ctx, candidate, client.DryRunAll)
		probeError = err
		return calls.Load() > 0 && err != nil && strings.Contains(err.Error(), "NamespaceReadFailed"), nil
	}); err != nil {
		t.Fatalf("rendered Pod webhook did not become effective: %v; calls=%d; last=%v", err, calls.Load(), probeError)
	}
	t.Run("rendered CEL parity", func(t *testing.T) {
		testPodCleanupCELParity(t, ctx, cfg, cl, namespace, rules.Webhooks[0].MatchConditions[0].Expression)
	})
	removeForeground := func(pod *corev1.Pod) *corev1.Pod {
		candidate := pod.DeepCopy()
		candidate.Finalizers = slices.DeleteFunc(candidate.Finalizers, func(value string) bool {
			return value == metav1.FinalizerDeleteDependents
		})
		return candidate
	}
	for _, phase := range []string{"reader-outage", "webhook-outage"} {
		t.Run(phase, func(t *testing.T) {
			if phase == "webhook-outage" {
				server.Close()
			}
			assertBlocked := func(caller client.Client, pod *corev1.Pod) {
				t.Helper()
				before := calls.Load()
				err := caller.Update(ctx, pod, client.DryRunAll)
				if phase == "reader-outage" {
					if err == nil || !strings.Contains(err.Error(), "NamespaceReadFailed") || calls.Load() <= before {
						t.Fatalf("non-cleanup UPDATE bypassed the rendered webhook: %v", err)
					}
				} else if err == nil || !strings.Contains(err.Error(), "failed calling webhook") {
					t.Fatalf("non-cleanup UPDATE did not fail closed during serving outage: %v", err)
				}
			}
			for i := 2; i < len(callers); i++ {
				t.Run(usernames[i], func(t *testing.T) {
					assertBlocked(callers[i], removeForeground(guarded))
				})
			}
			for name, mutate := range map[string]func(*corev1.Pod){
				"image":          func(p *corev1.Pod) { p.Spec.Containers[0].Image = "example.com/other:v1" },
				"labels":         func(p *corev1.Pod) { p.Labels = map[string]string{"example.com/changed": "true"} },
				"annotations":    func(p *corev1.Pod) { p.Annotations = map[string]string{"example.com/changed": "true"} },
				"remove other":   func(p *corev1.Pod) { p.Finalizers = p.Finalizers[:1] },
				"reorder others": func(p *corev1.Pod) { slices.Reverse(p.Finalizers) },
				"keep foreground": func(p *corev1.Pod) {
					p.Finalizers = slices.Clone(guarded.Finalizers)
				},
			} {
				t.Run(name, func(t *testing.T) {
					candidate := removeForeground(guarded)
					mutate(candidate)
					for _, collector := range callers[:2] {
						assertBlocked(collector, candidate.DeepCopy())
					}
				})
			}
			for _, collector := range callers[:2] {
				assertBlocked(collector, removeForeground(active))
				for _, field := range []string{"spec", "metadata", "status"} {
					pod := removeForeground(guarded)
					pod.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}
					candidate, err := runtime.DefaultUnstructuredConverter.ToUnstructured(pod)
					if err != nil {
						t.Fatal(err)
					}
					if err := unstructured.SetNestedField(candidate, true, field, "futureField"); err != nil {
						t.Fatal(err)
					}
					err = collector.Update(ctx, &unstructured.Unstructured{Object: candidate},
						client.DryRunAll, client.FieldValidation(metav1.FieldValidationStrict))
					if !apierrors.IsBadRequest(err) || !strings.Contains(err.Error(), "unknown field") {
						t.Fatalf("strict API decoding did not reject unknown %s field: %v", field, err)
					}
				}
			}
			for i, collector := range callers[:2] {
				for _, retainOthers := range []bool{false, true} {
					pod := cleanup[fmt.Sprintf("%s-%d-%t", phase, i, retainOthers)]
					candidate := removeForeground(pod)
					beforeCalls, beforeReads := calls.Load(), reader.calls.Load()
					if retainOthers {
						err = collector.Patch(ctx, candidate, client.MergeFromWithOptions(pod, client.MergeFromWithOptimisticLock{}),
							client.FieldOwner("gc-finalization"))
					} else {
						err = collector.Update(ctx, candidate, client.FieldOwner("gc-finalization"))
					}
					if err != nil {
						t.Fatalf("%s GC finalization failed for %s: %v", phase, usernames[i], err)
					}
					if calls.Load() != beforeCalls || reader.calls.Load() != beforeReads {
						t.Fatal("GC finalization reached the webhook or read dependencies")
					}
					persisted := &corev1.Pod{}
					err := cl.Get(ctx, client.ObjectKeyFromObject(pod), persisted)
					if retainOthers {
						if err != nil || !slices.Equal(persisted.Finalizers, others) ||
							persisted.Spec.Containers[0].Image != pod.Spec.Containers[0].Image {
							t.Fatalf("GC finalization changed another finalizer or the Pod: %v", err)
						}
					} else if !apierrors.IsNotFound(err) {
						t.Fatalf("foreground Pod deletion did not complete: %v", err)
					}
				}
			}
		})
	}
}

func testPodCleanupCELParity(t *testing.T, ctx context.Context, cfg *rest.Config, cl client.Client, namespace, expression string) {
	t.Helper()
	// The Pod REST strategy restores status and drops unknown fields before
	// admission. Preserve synthetic admission inputs in a CRD so the API server
	// can also evaluate the exact rendered CEL against those negative cases.
	_, err := envtest.InstallCRDs(cfg, envtest.CRDInstallOptions{CRDs: []*apiextensionsv1.CustomResourceDefinition{{
		ObjectMeta: metav1.ObjectMeta{Name: "podcleanupchecks.example.com"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "example.com", Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: "podcleanupchecks", Singular: "podcleanupcheck", Kind: "PodCleanupCheck"},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: "v1", Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"spec": {Type: "object", XPreserveUnknownFields: ptr.To(true)},
					},
				}},
			}},
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	variables := map[string]string{
		"request": "variables.requestFixture", "object": "variables.podFixture", "oldObject": "variables.previousPodFixture",
	}
	// Rebind only the three API-server inputs; do not rewrite the predicate.
	expression = regexp.MustCompile(`\b(request|object|oldObject)\b`).ReplaceAllStringFunc(expression, func(name string) string {
		return variables[name]
	})
	const mismatch = "synthetic Pod cleanup CEL mismatch"
	policy := &admissionregistrationv1.ValidatingAdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-cleanup-cel-parity"},
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			FailurePolicy: ptr.To(admissionregistrationv1.Fail),
			MatchConstraints: &admissionregistrationv1.MatchResources{ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
				RuleWithOperations: admissionregistrationv1.RuleWithOperations{
					Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create},
					Rule:       admissionregistrationv1.Rule{APIGroups: []string{"example.com"}, APIVersions: []string{"v1"}, Resources: []string{"podcleanupchecks"}},
				},
			}}},
			Variables: []admissionregistrationv1.Variable{
				{Name: "requestFixture", Expression: "object.spec.request"},
				{Name: "podFixture", Expression: "object.spec.pod"},
				{Name: "previousPodFixture", Expression: "object.spec.oldPod"},
			},
			Validations: []admissionregistrationv1.Validation{{Expression: "(" + expression + ") == object.spec.expectWebhook", Message: mismatch}},
		},
	}
	if err := cl.Create(ctx, policy); err != nil {
		t.Fatal(err)
	}
	if err := cl.Create(ctx, &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{Name: policy.Name},
		Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName: policy.Name, ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny},
		},
	}); err != nil {
		t.Fatal(err)
	}
	fixture := func(request admission.Request, expectWebhook bool) *unstructured.Unstructured {
		t.Helper()
		raw, err := json.Marshal(request.AdmissionRequest)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		pod, old := fields["object"], fields["oldObject"]
		delete(fields, "object")
		delete(fields, "oldObject")
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "example.com/v1", "kind": "PodCleanupCheck",
			"metadata": map[string]any{"namespace": namespace, "generateName": "pod-cleanup-"},
			"spec":     map[string]any{"request": fields, "pod": pod, "oldPod": old, "expectWebhook": expectWebhook},
		}}
	}
	pod, old := podFinalizationPair()
	probe := podRequest(t, pod, admissionv1.Update, old)
	probe.UserInfo.Username = "system:kube-controller-manager"
	var probeError error
	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 20*time.Second, true, func(ctx context.Context) (bool, error) {
		probeError = cl.Create(ctx, fixture(probe, true), client.DryRunAll)
		return probeError != nil && strings.Contains(probeError.Error(), mismatch), nil
	}); err != nil {
		t.Fatalf("CEL fixture policy did not become effective: %v; last=%v", err, probeError)
	}
	check := func(t *testing.T, request admission.Request, bypass bool) {
		t.Helper()
		var pod corev1.Pod
		goBypass := decodePod(request.Object.Raw, &pod) == nil && podGarbageCollectorFinalizerUpdate(request, &pod)
		if goBypass != bypass {
			t.Fatalf("Go cleanup predicate returned %t, want %t", goBypass, bypass)
		}
		if err := cl.Create(ctx, fixture(request, !bypass), client.DryRunAll); err != nil {
			t.Fatalf("rendered cleanup CEL disagrees with the Go predicate: %v", err)
		}
	}
	for _, username := range []string{"system:kube-controller-manager", "system:serviceaccount:kube-system:generic-garbage-collector"} {
		t.Run(username, func(t *testing.T) {
			pod, old := podFinalizationPair()
			request := podRequest(t, pod, admissionv1.Update, old)
			request.UserInfo.Username = username
			check(t, request, true)
			old.Finalizers, pod.Finalizers = []string{metav1.FinalizerDeleteDependents}, nil
			pod.ManagedFields = []metav1.ManagedFieldsEntry{{
				Manager: "garbage-collector", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "v1",
			}}
			request = podRequest(t, pod, admissionv1.Update, old)
			request.UserInfo.Username = username
			request.RequestKind, request.RequestResource = &request.Kind, &request.Resource
			check(t, request, true)
		})
	}
	for name, mutate := range podCleanupObjectMutations() {
		t.Run(name, func(t *testing.T) {
			pod, old := podFinalizationPair()
			mutate(pod, old)
			request := podRequest(t, pod, admissionv1.Update, old)
			request.UserInfo.Username = "system:kube-controller-manager"
			check(t, request, false)
		})
	}
	for name, mutate := range podCleanupRequestMutations() {
		t.Run(name, func(t *testing.T) {
			pod, old := podFinalizationPair()
			request := podRequest(t, pod, admissionv1.Update, old)
			request.UserInfo.Username = "system:kube-controller-manager"
			mutate(&request)
			check(t, request, false)
		})
	}
	for name, mutate := range map[string]func(*admission.Request){
		"delete": func(r *admission.Request) { r.Operation = admissionv1.Delete },
		"status": func(r *admission.Request) { r.SubResource = "status" },
		"non-GC admin": func(r *admission.Request) {
			r.UserInfo.Username, r.UserInfo.Groups = "admin@example.com", []string{"system:masters"}
		},
		"lookalike user": func(r *admission.Request) { r.UserInfo.Username = "system:kube-controller-manager-extra" },
		"lookalike SA": func(r *admission.Request) {
			r.UserInfo.Username = "system:serviceaccount:other:generic-garbage-collector"
		},
		"old kind": func(r *admission.Request) {
			r.OldObject.Raw = []byte(strings.Replace(string(r.OldObject.Raw), `"kind":"Pod"`, `"kind":"Secret"`, 1))
		},
		"old apiVersion": func(r *admission.Request) {
			r.OldObject.Raw = []byte(strings.Replace(string(r.OldObject.Raw), `"apiVersion":"v1"`, `"apiVersion":"v2"`, 1))
		},
	} {
		t.Run(name, func(t *testing.T) {
			pod, old := podFinalizationPair()
			request := podRequest(t, pod, admissionv1.Update, old)
			request.UserInfo.Username = "system:kube-controller-manager"
			mutate(&request)
			check(t, request, false)
		})
	}
	for _, field := range []string{"", "spec", "status", "metadata"} {
		for _, previous := range []bool{false, true} {
			t.Run(fmt.Sprintf("unknown %s/previous=%t", field, previous), func(t *testing.T) {
				pod, old := podFinalizationPair()
				request := podRequest(t, pod, admissionv1.Update, old)
				request.UserInfo.Username = "system:kube-controller-manager"
				raw := &request.Object.Raw
				if previous {
					raw = &request.OldObject.Raw
				}
				var object map[string]any
				if err := json.Unmarshal(*raw, &object); err != nil {
					t.Fatal(err)
				}
				path := []string{"futureField"}
				if field != "" {
					path = append([]string{field}, path...)
				}
				if err := unstructured.SetNestedField(object, true, path...); err != nil {
					t.Fatal(err)
				}
				*raw, err = json.Marshal(object)
				if err != nil {
					t.Fatal(err)
				}
				check(t, request, false)
			})
		}
	}
}

func TestPodAdmissionAPIServerBoundaries(t *testing.T) {
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
	env.ControlPlane.GetAPIServer().Configure().Set("enable-admission-plugins", "PodTopologyLabels")
	env.ControlPlane.GetAPIServer().Configure().Set("authorization-mode", "RBAC")
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
	f := newPodFixture(t)
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
	f.fixture.Namespace.Labels, f.fixture.Namespace.Annotations, f.fixture.Namespace.OwnerReferences = nil, nil, nil
	create(f.fixture.Namespace)
	create(f.fixture.Sandbox)
	create(f.fixture.Policy)
	create(f.fixture.Team)
	create(f.fixture.Agent)
	f.fixture.Namespace.Labels = map[string]string{w.ProtectedNamespaceLabel: "true"}
	f.fixture.Namespace.Annotations = map[string]string{
		w.AgentNameAnnotation: f.fixture.Agent.Name, w.AgentUIDAnnotation: string(f.fixture.Agent.UID),
	}
	f.fixture.Namespace.OwnerReferences = []metav1.OwnerReference{w.AgentOwner(f.fixture.Agent.Name, f.fixture.Agent.UID)}
	if err := cl.Update(ctx, f.fixture.Namespace); err != nil {
		t.Fatal(err)
	}
	create(f.fixture.ServiceAccount)
	create(f.fixture.Input.RuntimeClass)
	f.fixture.Input.Home.Annotations[w.AgentUIDAnnotation] = string(f.fixture.Agent.UID)
	create(f.fixture.Input.Home)
	f.fixture.Input.Skills.Spec = corev1.PersistentVolumeClaimSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, StorageClassName: ptr.To("example-csi"),
		Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
	}
	create(f.fixture.Input.Skills)
	create(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "example-config", Namespace: f.fixture.Namespace.Name}})
	create(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "runtime-secret", Namespace: f.fixture.Namespace.Name}})
	modelKey := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "gateway-key", Namespace: f.fixture.Namespace.Name}}
	create(modelKey)
	f.fixture.Input.GatewayKey.UID = modelKey.UID
	resources := sandbox.Resources{
		Sandboxes:     map[string]*api.Sandbox{f.fixture.Sandbox.Name: f.fixture.Sandbox},
		AgentPolicies: map[string]*api.AgentPolicy{f.fixture.Policy.Name: f.fixture.Policy},
	}
	record, err := sandboxbinding.Snapshot(f.fixture.Agent, f.fixture.Team, resources)
	if err != nil {
		t.Fatal(err)
	}
	f.fixture.Agent.Annotations[sandboxbinding.AuthorizationAnnotation] = record
	if err := cl.Update(ctx, f.fixture.Agent); err != nil {
		t.Fatal(err)
	}
	binding, err := sandboxbinding.AuthorizedBinding(f.fixture.Agent)
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := sandbox.Resolve(sandbox.ResolveInput{
		Agent: f.fixture.Agent, Team: f.fixture.Team, Resources: resources, AuthorizedBinding: binding,
		Registrations: map[api.SandboxType]sandbox.Registration{api.SandboxKata: {
			Capabilities: f.fixture.Input.Resolution.Capabilities, Available: true,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := f.fixture.Input
	input.Resolution, input.AgentGeneration, input.Namespace.UID = resolution, f.fixture.Agent.Generation, f.fixture.Namespace.UID
	input.Controls.Binding, input.Controls.ResolutionRevision = binding, resolution.Revision
	bindingJSON, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	for key := range input.Controls.PodLabels {
		input.Controls.PodLabels[key] = string(binding.AgentUID)
	}
	for i := range input.Controls.Objects {
		object := &input.Controls.Objects[i]
		object.SetOwnerReferences([]metav1.OwnerReference{w.AgentOwner(input.AgentName, binding.AgentUID)})
		object.SetLabels(map[string]string{sandboxpolicy.AgentUIDLabel: string(binding.AgentUID)})
		annotations := object.GetAnnotations()
		annotations[sandboxpolicy.BindingAnnotation] = string(bindingJSON)
		object.SetAnnotations(annotations)
		if object.GetKind() == "CiliumNetworkPolicy" {
			object.Object["spec"].(map[string]any)["endpointSelector"] = map[string]any{"matchLabels": map[string]any{
				"k8s:" + sandboxpolicy.AgentUIDLabel: string(binding.AgentUID), "k8s:io.kubernetes.pod.namespace": input.Namespace.Name,
			}}
		}
	}
	bundle, err := w.Build(input)
	if err != nil {
		t.Fatal(err)
	}
	chrt, err := loader.Load("../../../cetirun-chart")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := helm.PrepareSandboxChart(chrt, map[string]any{
		"agentType": input.AgentType, "agentRequest": map[string]any{"type": input.AgentType},
		"secrets":   map[string]any{"existingSecret": "runtime-secret", "provider": "openai", "model": "example-model", "apiBase": "http://gateway.example-platform.svc.cluster.local:4000/v1"},
		"aiGateway": map[string]any{"enabled": true, "keySecretName": input.GatewayKey.Name, "keySecretKey": "api-key", "models": []any{map[string]any{"id": "example-model", "name": "Example"}}},
	}, nil, bundle)
	if err != nil {
		t.Fatal(err)
	}
	template, err := prepared.SandboxTemplate()
	if err != nil {
		t.Fatal(err)
	}
	workload := testfixture.Workload(input, *template)
	create(workload)
	workload.Status.UpdateRevision = "example-revision"
	if err := cl.Status().Update(ctx, workload); err != nil {
		t.Fatal(err)
	}
	expected, err := bundle.Expect(*template, workload, nil)
	if err != nil {
		t.Fatal(err)
	}
	expected.ExecutionHeld, expected.ControllerRevision = false, workload.Status.UpdateRevision
	expected.Template.Finalizers = nil
	workload.Spec.Template.Finalizers = nil
	if err := cl.Update(ctx, workload); err != nil {
		t.Fatal(err)
	}
	// Refresh persisted identities; the Agent pins intent, other sources pin
	// metadata versions. No Secret contents or external cluster are read.
	for _, source := range f.expected.Sources {
		if source.IsAgent() {
			agent := &api.Agent{}
			if err := cl.Get(ctx, client.ObjectKey{Name: source.Name}, agent); err != nil {
				t.Fatal(err)
			}
			current, err := w.AgentSourceReference(agent)
			if err != nil {
				t.Fatal(err)
			}
			expected.Sources = append(expected.Sources, current)
			continue
		}
		object := &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: source.APIVersion, Kind: source.Kind}}
		if err := cl.Get(ctx, client.ObjectKey{Namespace: source.Namespace, Name: source.Name}, object); err != nil {
			t.Fatal(err)
		}
		source.UID, source.ResourceVersion = object.UID, object.ResourceVersion
		expected.Sources = append(expected.Sources, source)
	}
	state, err := PodExpectationObject("operator-state", expected)
	if err != nil {
		t.Fatal(err)
	}
	create(state)
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "reserved-worker", Labels: map[string]string{
			corev1.LabelTopologyRegion: "example-region", corev1.LabelTopologyZone: "example-zone",
		}},
		Spec: corev1.NodeSpec{ProviderID: "example://reserved-worker"},
	}
	create(node)
	placementRegistry := placementRegistryForPod(t, state.Namespace, expected, node)
	create(placementRegistry)
	handler, err := NewPodHandler(cl, PodConfig{StateNamespace: state.Namespace, PlacementConfigMapName: placementRegistry.Name})
	if err != nil {
		t.Fatal(err)
	}
	placement, err := sandboxplacement.NewHandler(cl, sandboxplacement.Config{Namespace: state.Namespace, ConfigMapName: placementRegistry.Name})
	if err != nil {
		t.Fatal(err)
	}
	var unavailable atomic.Bool
	var calls atomic.Int64
	webhook := &admission.Webhook{Handler: handler}
	placementWebhook := &admission.Webhook{Handler: placement}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if unavailable.Load() {
			http.Error(w, "synthetic outage", http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path == PlacementValidationPath {
			placementWebhook.ServeHTTP(w, r)
			return
		}
		webhook.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	_, _, rules := renderedPodAdmission(t, ca)
	rules.Webhooks[0].ClientConfig.Service = nil
	rules.Webhooks[0].ClientConfig.URL = ptr.To(server.URL + PodValidationPath)
	rules.Webhooks[0].TimeoutSeconds = ptr.To(int32(2))
	create(rules)
	var probeError error
	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 20*time.Second, true, func(ctx context.Context) (bool, error) {
		probe := testfixture.Pod(expected)
		probe.Spec.Overhead = nil
		probe.Annotations[w.KindAnnotation] = "unapproved"
		err := cl.Create(ctx, probe)
		probeError = err
		if err == nil {
			return false, cl.Delete(ctx, probe, client.GracePeriodSeconds(0))
		}
		return calls.Load() > 0 && strings.Contains(err.Error(), "denied"), nil
	}); err != nil {
		t.Fatalf("webhook did not reject probe: %v; calls=%d; last=%v", err, calls.Load(), probeError)
	}
	var inject atomic.Bool
	inject.Store(true)
	var mutationCalls atomic.Int64
	mutator := &admission.Webhook{Handler: admission.HandlerFunc(func(_ context.Context, request admission.Request) admission.Response {
		mutationCalls.Add(1)
		if !inject.Load() {
			return admission.Allowed("synthetic mutator excluded")
		}
		var pod corev1.Pod
		if err := json.Unmarshal(request.Object.Raw, &pod); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations["example.com/injected"] = "synthetic-mutator-value"
		raw, err := json.Marshal(pod)
		if err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		}
		return admission.PatchResponseFromRaw(request.Object.Raw, raw)
	})}
	mutatorServer := httptest.NewTLSServer(mutator)
	t.Cleanup(mutatorServer.Close)
	create(&admissionregistrationv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "synthetic-pod-mutator"},
		Webhooks: []admissionregistrationv1.MutatingWebhook{{
			Name: "synthetic-pod-mutator.example.com", AdmissionReviewVersions: []string{"v1"},
			ClientConfig: admissionregistrationv1.WebhookClientConfig{
				URL: ptr.To(mutatorServer.URL), CABundle: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: mutatorServer.Certificate().Raw}),
			},
			FailurePolicy: ptr.To(admissionregistrationv1.Fail), SideEffects: ptr.To(admissionregistrationv1.SideEffectClassNone),
			TimeoutSeconds: ptr.To(int32(2)), NamespaceSelector: rules.Webhooks[0].NamespaceSelector.DeepCopy(),
			Rules: []admissionregistrationv1.RuleWithOperations{{
				Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create},
				Rule:       admissionregistrationv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods"}},
			}},
		}},
	})
	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 20*time.Second, true, func(ctx context.Context) (bool, error) {
		probe := testfixture.Pod(expected)
		probe.Spec.Overhead = nil
		err := cl.Create(ctx, probe, client.DryRunAll)
		return mutationCalls.Load() > 0 && err != nil && strings.Contains(err.Error(), "TemplateMismatch") &&
			strings.Contains(err.Error(), "metadata.annotations") && !strings.Contains(err.Error(), "synthetic-mutator-value"), nil
	}); err != nil {
		t.Fatalf("third-party mutation was not safely diagnosed: %v", err)
	}
	inject.Store(false)
	foreignDNS := testfixture.Pod(expected)
	foreignDNS.Spec.Overhead = nil
	foreignDNS.Spec.DNSConfig.Nameservers = []string{"192.0.2.53"}
	beforeDNS := calls.Load()
	if err := cl.Create(ctx, foreignDNS); err == nil || !strings.Contains(err.Error(), "denied") || calls.Load() <= beforeDNS {
		t.Fatalf("final-Pod webhook did not reject a schema-valid DNS substitution: %v", err)
	}
	pod := testfixture.Pod(expected)
	pod.Spec.Overhead = nil // RuntimeClass admission supplies the registered overhead.
	if err := cl.Create(ctx, pod); err != nil {
		t.Fatalf("API-defaulted protected Pod was rejected: %v", err)
	}
	if pod.Spec.DNSPolicy != corev1.DNSNone || pod.Spec.DNSConfig == nil ||
		len(pod.Spec.DNSConfig.Nameservers) != 1 || pod.Spec.DNSConfig.Nameservers[0] != "10.96.0.10" {
		t.Fatal("API defaulting replaced the protected DNS configuration")
	}
	t.Run("node-topology-binding-and-protected-update", func(t *testing.T) {
		bindingRules := renderedPlacementWebhook(t, state.Namespace, placementRegistry.Name, ca)
		bindingRules.Webhooks[0].ClientConfig.Service = nil
		bindingRules.Webhooks[0].ClientConfig.URL = ptr.To(server.URL + PlacementValidationPath)
		create(bindingRules)
		t.Cleanup(func() {
			if err := cl.Delete(context.Background(), bindingRules); err != nil {
				t.Error(err)
			}
		})
		kube, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		binding := &corev1.Binding{
			ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace, UID: pod.UID},
			Target:     corev1.ObjectReference{Kind: "Node", Name: node.Name},
		}
		stale := binding.DeepCopy()
		stale.UID = "synthetic-stale-pod"
		if err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 15*time.Second, true, func(ctx context.Context) (bool, error) {
			err := kube.CoreV1().Pods(pod.Namespace).Bind(ctx, stale, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
			return err != nil && strings.Contains(err.Error(), "BindingPodMismatch"), nil
		}); err != nil {
			t.Fatal("placement binding webhook did not activate:", err)
		}
		if err := kube.CoreV1().Pods(pod.Namespace).Bind(ctx, binding, metav1.CreateOptions{}); err != nil {
			t.Fatal("API-server topology additions blocked a qualified binding:", err)
		}
		if err := cl.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{corev1.LabelTopologyRegion, corev1.LabelTopologyZone} {
			if pod.Labels[key] != node.Labels[key] {
				t.Fatal("PodTopologyLabels did not copy the Node topology at binding")
			}
		}
		if err := cl.Update(ctx, pod.DeepCopy(), client.DryRunAll); err != nil {
			t.Fatal("both validators must accept the unchanged bound Pod:", err)
		}
		forged := pod.DeepCopy()
		forged.Labels[corev1.LabelTopologyZone] = "forged-zone"
		if err := cl.Update(ctx, forged, client.DryRunAll); err == nil {
			t.Fatal("forged bound topology was accepted")
		}
	})
	t.Run("status-churn-and-metadata-reauthorization", func(t *testing.T) {
		agent := &api.Agent{}
		if err := cl.Get(ctx, client.ObjectKeyFromObject(f.fixture.Agent), agent); err != nil {
			t.Fatal(err)
		}
		oldVersion, generation, expectationVersion := agent.ResourceVersion, agent.Generation, state.ResourceVersion
		agent.Status.Phase = api.PhasePending
		if err := cl.Status().Update(ctx, agent); err != nil {
			t.Fatal(err)
		}
		if agent.ResourceVersion == oldVersion || agent.Generation != generation {
			t.Fatal("status update did not preserve generation while changing resourceVersion")
		}
		if err := cl.Update(ctx, pod.DeepCopy(), client.DryRunAll); err != nil {
			t.Fatalf("status-only change invalidated the protected Pod: %v", err)
		}
		if err := cl.Get(ctx, client.ObjectKeyFromObject(state), state); err != nil {
			t.Fatal(err)
		}
		if state.ResourceVersion != expectationVersion {
			t.Fatal("status-only admission required republishing the expectation")
		}
		reauthorize, err := sandboxbinding.FormatReauthorization("synthetic-new-intent", expected.Binding)
		if err != nil {
			t.Fatal(err)
		}
		agent.Annotations[sandboxbinding.ReauthorizeAnnotation] = reauthorize
		if err := cl.Update(ctx, agent); err != nil {
			t.Fatal(err)
		}
		if agent.Generation != generation {
			t.Fatal("reauthorization fixture changed generation")
		}
		if err := cl.Update(ctx, pod.DeepCopy(), client.DryRunAll); err == nil || !strings.Contains(err.Error(), "SourceChanged") {
			t.Fatalf("metadata-only reauthorization reused stale intent: %v", err)
		}
		current, err := w.AgentSourceReference(agent)
		if err != nil {
			t.Fatal(err)
		}
		for i := range expected.Sources {
			if expected.Sources[i].IsAgent() {
				expected.Sources[i] = current
			}
		}
		raw, err := json.Marshal(expected)
		if err != nil {
			t.Fatal(err)
		}
		state.Data[PodExpectationKey] = string(raw)
		if err := cl.Update(ctx, state); err != nil {
			t.Fatal(err)
		}
		if err := cl.Update(ctx, pod.DeepCopy(), client.DryRunAll); err != nil {
			t.Fatalf("fresh explicit intent did not restore admission: %v", err)
		}
	})
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		for _, subresource := range []string{"exec", "attach", "portforward"} {
			before := calls.Load()
			request := clientset.CoreV1().RESTClient().Verb(method).Namespace(pod.Namespace).
				Resource("pods").Name(pod.Name).SubResource(subresource)
			switch subresource {
			case "exec":
				request.Param("command", "true").Param("container", "cetirun").Param("stdout", "true")
			case "attach":
				request.Param("container", "cetirun").Param("stdout", "true")
			case "portforward":
				request.Param("ports", "8080")
			}
			err := request.Do(ctx).Error()
			if err == nil || !strings.Contains(err.Error(), "ConnectForbidden") || calls.Load() <= before {
				t.Fatalf("%s %s bypassed the rendered CONNECT guard: %v", method, subresource, err)
			}
		}
	}
	t.Run("API defaulting fixed point", func(t *testing.T) { testPodAPIDefaulting(t, ctx, cl) })
	helper := testfixture.Pod(expected)
	helper.Name, helper.Spec.Overhead = "copilot-helper", nil
	helper.Annotations[w.KindAnnotation] = "copilot-helper"
	if err := cl.Create(ctx, helper); err == nil {
		t.Fatal("undeclared helper bypassed final-Pod admission")
	}

	unavailable.Store(true)
	rejected := testfixture.Pod(expected)
	rejected.Name, rejected.Spec.Overhead = "outage-probe", nil
	if err := cl.Create(ctx, rejected); err == nil || apierrors.IsAlreadyExists(err) {
		t.Fatalf("protected creation did not fail closed during outage: %v", err)
	}
	before := calls.Load()
	pod.Status.Phase = corev1.PodPending
	if err := cl.Status().Update(ctx, pod); err != nil {
		t.Fatalf("Pod status depended on unavailable webhook: %v", err)
	}
	if err := cl.Delete(ctx, pod, client.GracePeriodSeconds(0)); err != nil {
		t.Fatalf("Pod deletion depended on unavailable webhook: %v", err)
	}
	if calls.Load() != before {
		t.Fatal("status/delete reached protected-write webhook")
	}
	unavailable.Store(false)
	t.Run("native-single-pod-and-effective-source-pins", func(t *testing.T) {
		expected.SinglePod, expected.PodUID = true, ""
		expected.Template.Finalizers = []string{w.NativePodFinalizer}
		expected.SourceDigests = map[string]string{}
		for _, source := range expected.Sources {
			if !w.EffectiveSource(source) {
				continue
			}
			value := &unstructured.Unstructured{}
			value.SetAPIVersion(source.APIVersion)
			value.SetKind(source.Kind)
			var object client.Object = value
			if source.APIVersion == api.GroupVersion.String() && source.Kind == "Team" {
				object = &api.Team{}
			}
			if source.APIVersion == "v1" && source.Kind == "PersistentVolumeClaim" {
				object = &corev1.PersistentVolumeClaim{}
			}
			if err := cl.Get(ctx, client.ObjectKey{Namespace: source.Namespace, Name: source.Name}, object); err != nil {
				t.Fatal(err)
			}
			digest, err := w.ExecutionSourceDigest(object, f.fixture.Sandbox.Name)
			if claim, ok := object.(*corev1.PersistentVolumeClaim); ok {
				digest, err = w.NativeClaimDigest(claim)
			}
			if err != nil {
				t.Fatal(err)
			}
			expected.SourceDigests[w.SourceKey(source)] = digest
		}
		save := func() {
			t.Helper()
			if err := cl.Get(ctx, client.ObjectKeyFromObject(state), state); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(expected)
			if err != nil {
				t.Fatal(err)
			}
			state.Data[PodExpectationKey] = string(raw)
			if err := cl.Update(ctx, state); err != nil {
				t.Fatal(err)
			}
		}
		save()
		if err := cl.Get(ctx, client.ObjectKeyFromObject(workload), workload); err != nil {
			t.Fatal(err)
		}
		workload.Spec.Template.Finalizers = []string{w.NativePodFinalizer}
		if err := cl.Update(ctx, workload); err != nil {
			t.Fatal(err)
		}
		workload.Status.Replicas, workload.Status.ReadyReplicas = 1, 1
		if err := cl.Status().Update(ctx, workload); err != nil {
			t.Fatal(err)
		}
		for _, identity := range []w.ObjectIdentity{expected.Home, expected.Skills} {
			claim := &corev1.PersistentVolumeClaim{}
			if err := cl.Get(ctx, client.ObjectKey{Namespace: expected.Namespace.Name, Name: identity.Name}, claim); err != nil {
				t.Fatal(err)
			}
			claim.Spec.VolumeName = "bound-" + claim.Name
			if err := cl.Update(ctx, claim); err != nil {
				t.Fatal(err)
			}
			claim.Status.Phase = corev1.ClaimBound
			if err := cl.Status().Update(ctx, claim); err != nil {
				t.Fatal(err)
			}
		}
		native := testfixture.Pod(expected)
		native.Spec.Overhead = nil
		if err := cl.Create(ctx, native); err != nil {
			t.Fatal("native admission rejected status-only or claim-binding source changes:", err)
		}
		if err := cl.Delete(ctx, native, client.GracePeriodSeconds(0)); err != nil {
			t.Fatal(err)
		}
		if err := cl.Get(ctx, client.ObjectKeyFromObject(native), native); err != nil {
			t.Fatal("initial Pod disappeared before its lifetime was recorded:", err)
		}
		withoutProtection := native.DeepCopy()
		withoutProtection.Finalizers = nil
		if err := cl.Update(ctx, withoutProtection, client.DryRunAll); err == nil {
			t.Fatal("unrecorded Pod protection was removable")
		}
		expected.PodUID = native.UID
		expected.ExecutionHeld = true
		save()
		cleanup := nativeDisabledCleanupClient(t, ctx, cl, cfg, f.scheme)
		beforeFinalize := native.DeepCopy()
		native.Finalizers = nil
		if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
			err := cleanup.Patch(ctx, native, client.MergeFromWithOptions(beforeFinalize, client.MergeFromWithOptimisticLock{}), client.DryRunAll)
			if apierrors.IsForbidden(err) {
				return false, nil
			}
			return err == nil, err
		}); err != nil {
			t.Fatal("disabled manager role could not finalize the held native Pod:", err)
		}
		agent := &api.Agent{}
		if err := cl.Get(ctx, client.ObjectKeyFromObject(f.fixture.Agent), agent); err != nil {
			t.Fatal(err)
		}
		agent.Finalizers = append(agent.Finalizers, api.Finalizer)
		if err := cl.Update(ctx, agent); err != nil {
			t.Fatal(err)
		}
		if err := cl.Delete(ctx, agent); err != nil {
			t.Fatal(err)
		}
		if err := cl.Get(ctx, client.ObjectKeyFromObject(native), native); err != nil {
			t.Fatal(err)
		}
		beforeFinalize = native.DeepCopy()
		native.Finalizers = nil
		if err := cleanup.Patch(ctx, native, client.MergeFromWithOptions(beforeFinalize, client.MergeFromWithOptimisticLock{})); err != nil {
			t.Fatal(err)
		}
		replacement := testfixture.Pod(expected)
		replacement.Spec.Overhead = nil
		if err := cl.Create(ctx, replacement); err == nil {
			t.Fatalf("native admission did not enforce the recorded Pod lifetime: %v", err)
		}
	})
	if err := cl.Delete(ctx, state); err != nil {
		t.Fatal(err)
	}
	missing := testfixture.Pod(expected)
	missing.Spec.Overhead = nil
	if err := cl.Create(ctx, missing); err == nil {
		t.Fatal("missing persisted expectations allowed creation")
	}
}

func testPodAPIDefaulting(t *testing.T, ctx context.Context, cl client.Client) {
	t.Helper()
	if err := cl.Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "defaulting-runner", Namespace: "operator-state"}}); err != nil {
		t.Fatal(err)
	}
	items := []corev1.DownwardAPIVolumeFile{
		{Path: "name", FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}},
		{Path: "cpu", ResourceFieldRef: &corev1.ResourceFieldSelector{ContainerName: "example", Resource: "limits.cpu"}},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "defaulting-example", Namespace: "operator-state"},
		Spec: corev1.PodSpec{
			ServiceAccountName: "defaulting-runner", AutomountServiceAccountToken: ptr.To(false), EnableServiceLinks: ptr.To(false),
			SecurityContext: &corev1.PodSecurityContext{},
			Containers: []corev1.Container{{
				Name: "example", Image: "example.com/agent:v1",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
				},
				Env: []corev1.EnvVar{
					{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
					{Name: "CPU", ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{Resource: "limits.cpu"}}},
				},
			}},
			Volumes: []corev1.Volume{
				{Name: "downward", VolumeSource: corev1.VolumeSource{DownwardAPI: &corev1.DownwardAPIVolumeSource{Items: items}}},
				{Name: "projected", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
					Sources: []corev1.VolumeProjection{{DownwardAPI: &corev1.DownwardAPIProjection{Items: items}}},
				}}},
			},
		},
	}
	expected := corev1.PodTemplateSpec{Spec: *pod.Spec.DeepCopy()}
	if err := cl.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	actual := corev1.PodTemplateSpec{ObjectMeta: pod.ObjectMeta, Spec: pod.Spec}
	if !w.SamePodTemplate(actual, expected) {
		t.Fatal("representative API-defaulted Pod does not match the normalized template")
	}
	if !w.SamePodTemplate(actual, actual) {
		t.Fatal("API-defaulted template normalization is not a fixed point")
	}
	actual.Spec.Volumes[0].DownwardAPI.DefaultMode = ptr.To(int32(0400))
	if w.SamePodTemplate(actual, expected) {
		t.Fatal("normalization ignored an explicit volume-mode change")
	}
}
