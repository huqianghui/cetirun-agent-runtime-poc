// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	"github.com/go-logr/logr/funcr"
	"helm.sh/helm/v3/pkg/chart/loader"
	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

type podFixture struct {
	fixture  testfixture.Fixture
	scheme   *runtime.Scheme
	client   client.Client
	handler  *PodHandler
	pod      *corev1.Pod
	expected w.Expectation
	state    *corev1.ConfigMap
	workload *appsv1.StatefulSet
	objects  []client.Object
}

func newPodFixture(t *testing.T) podFixture {
	t.Helper()
	return newPodSkillsFixture(t, false)
}

func newPodSkillsFixture(t *testing.T, emptySkills bool) podFixture {
	t.Helper()
	typ := "openclaw"
	fixture := testfixture.New(typ)
	values := map[string]any{
		"agentType": typ, "agentRequest": map[string]any{"type": typ},
		"secrets": map[string]any{"existingSecret": "runtime-secret", "provider": "openai", "model": "example-model"},
	}
	fixture.Input.GatewayKey = &w.ObjectIdentity{Name: "gateway-key", UID: "gateway-key-uid"}
	fixture.Input.AuthorizedSecrets = append(fixture.Input.AuthorizedSecrets, "gateway-key")
	values["aiGateway"] = map[string]any{
		"enabled": true, "keySecretName": "gateway-key", "keySecretKey": "api-key",
		"models": []any{map[string]any{"id": "example-model", "name": "Example"}},
	}
	values["secrets"].(map[string]any)["apiBase"] = "http://gateway.example-platform.svc.cluster.local:4000/v1"
	if emptySkills {
		fixture.Input.Skills, fixture.Input.SkillsMode = nil, w.SkillsModeEmpty
		fixture.Input.Helpers.Desktop = false
	}
	bundle, err := w.Build(fixture.Input)
	if err != nil {
		t.Fatal(err)
	}
	chrt, err := loader.Load("../../../cetirun-chart")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := helm.PrepareSandboxChart(chrt, values, nil, bundle)
	if err != nil {
		t.Fatal(err)
	}
	template, err := prepared.SandboxTemplate()
	if err != nil {
		t.Fatal(err)
	}
	workload := testfixture.Workload(fixture.Input, *template)
	expected, err := bundle.Expect(*template, workload, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Models a future controller-approved state using persisted API objects.
	// Production rendering remains held and cannot construct an active state.
	expected.ExecutionHeld, expected.ControllerRevision = false, workload.Status.UpdateRevision
	expected.SinglePod = emptySkills
	if !emptySkills {
		// Preserve compatibility coverage for persisted records from before
		// native Pod lifetime protection. This fixture does not authorize a start.
		expected.Template.Finalizers = nil
		workload.Spec.Template.Finalizers = nil
	}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{api.AddToScheme, appsv1.AddToScheme, corev1.AddToScheme, nodev1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	objects := []client.Object{
		fixture.Agent, fixture.Team, fixture.Sandbox, fixture.Policy, fixture.Namespace, fixture.ServiceAccount,
		fixture.Input.RuntimeClass, fixture.Input.Home,
	}
	if fixture.Input.Skills != nil {
		objects = append(objects, fixture.Input.Skills)
	}
	objects = append(objects, workload,
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "example-config", Namespace: fixture.Namespace.Name, UID: "config-uid", ResourceVersion: "1"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "runtime-secret", Namespace: fixture.Namespace.Name, UID: "secret-uid", ResourceVersion: "1"}},
	)
	objects = append(objects, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "gateway-key", Namespace: fixture.Namespace.Name, UID: "gateway-key-uid", ResourceVersion: "1",
	}})
	for _, object := range objects {
		if agent, ok := object.(*api.Agent); ok {
			source, err := w.AgentSourceReference(agent)
			if err != nil {
				t.Fatal(err)
			}
			expected.Sources = append(expected.Sources, source)
			continue
		}
		kinds, _, err := scheme.ObjectKinds(object)
		if err != nil {
			t.Fatal(err)
		}
		expected.Sources = append(expected.Sources, w.SourceReference{
			APIVersion: kinds[0].GroupVersion().String(), Kind: kinds[0].Kind, Namespace: object.GetNamespace(), Name: object.GetName(),
			UID: object.GetUID(), ResourceVersion: object.GetResourceVersion(),
		})
	}
	state, err := PodExpectationObject("operator-state", expected)
	if err != nil {
		t.Fatal(err)
	}
	state.UID, state.ResourceVersion = "state-uid", "1"
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(fixture.Agent).
		WithObjects(append(objects, state)...).Build()
	handler, err := NewPodHandler(cl, PodConfig{StateNamespace: "operator-state"})
	if err != nil {
		t.Fatal(err)
	}
	return podFixture{fixture: fixture, scheme: scheme, client: cl, handler: handler, pod: testfixture.Pod(expected),
		expected: expected, state: state, workload: workload, objects: objects}
}

func podRequest(t *testing.T, pod *corev1.Pod, operation admissionv1.Operation, old *corev1.Pod) admission.Request {
	t.Helper()
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatal(err)
	}
	request := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID: "request-uid", Operation: operation, Namespace: pod.Namespace, Name: pod.Name,
		Resource: metav1.GroupVersionResource{Version: "v1", Resource: "pods"},
		Kind:     metav1.GroupVersionKind{Version: "v1", Kind: "Pod"}, Object: runtime.RawExtension{Raw: raw},
	}}
	if old != nil {
		request.OldObject.Raw, err = json.Marshal(old)
		if err != nil {
			t.Fatal(err)
		}
	}
	return request
}

func TestPodAdmissionRequiresPersistedCurrentAuthority(t *testing.T) {
	f := newPodFixture(t)
	request := podRequest(t, f.pod, admissionv1.Create, nil)
	if response := f.handler.Validate(context.Background(), request); !response.Allowed {
		t.Fatalf("valid persisted future-contract state rejected: %+v", response.Result)
	}
	if err := f.client.Delete(context.Background(), f.state); err != nil {
		t.Fatal(err)
	}
	if response := f.handler.Validate(context.Background(), request); response.Allowed {
		t.Fatal("an in-memory expected template authorized a Pod without persisted controller state")
	}
	foreign := f.state.DeepCopy()
	foreign.Namespace = f.pod.Namespace
	foreign.ResourceVersion = ""
	if err := f.client.Create(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}
	if response := f.handler.Validate(context.Background(), request); response.Allowed {
		t.Fatal("tenant namespace state substituted for protected controller state")
	}
}

func TestPodNamespaceTupleMustMatchAuthoritativeExpectation(t *testing.T) {
	for name, mutate := range map[string]func(*corev1.Namespace){
		"marker removed":       func(ns *corev1.Namespace) { delete(ns.Labels, w.ProtectedNamespaceLabel) },
		"marker false":         func(ns *corev1.Namespace) { ns.Labels[w.ProtectedNamespaceLabel] = "false" },
		"Agent name changed":   func(ns *corev1.Namespace) { ns.Annotations[w.AgentNameAnnotation] = "foreign" },
		"Agent UID changed":    func(ns *corev1.Namespace) { ns.Annotations[w.AgentUIDAnnotation] = "foreign" },
		"owner removed":        func(ns *corev1.Namespace) { ns.OwnerReferences = nil },
		"owner changed":        func(ns *corev1.Namespace) { ns.OwnerReferences[0].UID = "foreign" },
		"non-controller owner": func(ns *corev1.Namespace) { ns.OwnerReferences[0].Controller = ptr.To(false) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newPodFixture(t)
			mutate(f.fixture.Namespace)
			if err := f.client.Update(context.Background(), f.fixture.Namespace); err != nil {
				t.Fatal(err)
			}
			// Keep source RVs current so this tests the ownership tuple itself,
			// not merely the stale-source check exercised elsewhere.
			for i := range f.expected.Sources {
				if f.expected.Sources[i].Kind == "Namespace" {
					f.expected.Sources[i].ResourceVersion = f.fixture.Namespace.ResourceVersion
				}
			}
			raw, err := json.Marshal(f.expected)
			if err != nil {
				t.Fatal(err)
			}
			f.state.Data[PodExpectationKey] = string(raw)
			if err := f.client.Update(context.Background(), f.state); err != nil {
				t.Fatal(err)
			}
			if response := f.handler.Validate(context.Background(), podRequest(t, f.pod, admissionv1.Create, nil)); response.Allowed {
				t.Fatal("current expected-state RVs authorized an unowned namespace")
			}
		})
	}
}

func TestPodAdmissionRejectsDNSMutations(t *testing.T) {
	f := newPodFixture(t)
	for name, mutate := range map[string]func(*corev1.Pod){
		"cluster defaults": func(p *corev1.Pod) { p.Spec.DNSPolicy = corev1.DNSClusterFirst },
		"node defaults":    func(p *corev1.Pod) { p.Spec.DNSPolicy = corev1.DNSDefault },
		"missing config":   func(p *corev1.Pod) { p.Spec.DNSConfig = nil },
		"foreign server":   func(p *corev1.Pod) { p.Spec.DNSConfig.Nameservers[0] = "192.0.2.53" },
		"fallback server": func(p *corev1.Pod) {
			p.Spec.DNSConfig.Nameservers = append(p.Spec.DNSConfig.Nameservers, "192.0.2.53")
		},
		"search domains": func(p *corev1.Pod) { p.Spec.DNSConfig.Searches = []string{"example.com"} },
	} {
		t.Run(name, func(t *testing.T) {
			pod := f.pod.DeepCopy()
			mutate(pod)
			if response := f.handler.Validate(context.Background(), podRequest(t, pod, admissionv1.Create, nil)); response.Allowed {
				t.Fatal("Pod admission accepted mutated DNS despite a current protected expectation")
			}
		})
	}
}

func TestPodAdmissionRejectsStaleHoldAndSpoofedState(t *testing.T) {
	cases := map[string]func(*podFixture){
		"Agent hold": func(f *podFixture) {
			f.fixture.Agent.Finalizers = append(f.fixture.Agent.Finalizers, sandbox.HoldFinalizer)
		},
		"suspension":            func(f *podFixture) { f.fixture.Agent.Spec.Suspended = true },
		"binding removed":       func(f *podFixture) { delete(f.fixture.Agent.Annotations, sandboxbinding.AuthorizationAnnotation) },
		"Agent generation":      func(f *podFixture) { f.fixture.Agent.Generation++ },
		"policy revision":       func(f *podFixture) { f.fixture.Policy.Spec.Security.ReadOnlyRootFilesystem = ptr.To(false) },
		"HOME ownership":        func(f *podFixture) { f.fixture.Input.Home.Annotations[w.AgentUIDAnnotation] = "foreign" },
		"namespace replacement": func(f *podFixture) { f.fixture.Namespace.UID = "replaced" },
		"workload owner":        func(f *podFixture) { f.workload.OwnerReferences[0].UID = "foreign" },
		"workload stopped":      func(f *podFixture) { f.workload.Spec.Replicas = ptr.To(int32(0)) },
		"controller revision":   func(f *podFixture) { f.workload.Status.UpdateRevision = "new" },
		"helper state":          func(f *podFixture) { f.expected.WorkloadKind = "copilot-helper" },
		"held expectation":      func(f *podFixture) { f.expected.ExecutionHeld = true },
		"missing sources":       func(f *podFixture) { f.expected.Sources = f.expected.Sources[:len(f.expected.Sources)-1] },
		"state owner":           func(f *podFixture) { f.state.OwnerReferences[0].UID = "foreign" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := newPodFixture(t)
			if response := f.handler.Validate(context.Background(), podRequest(t, f.pod, admissionv1.Create, nil)); !response.Allowed {
				t.Fatalf("test starts without valid authority: %+v", response.Result)
			}
			before := make([]client.Object, len(f.objects))
			for i, object := range f.objects {
				before[i] = object.DeepCopyObject().(client.Object)
			}
			change(&f)
			for i, object := range f.objects {
				if equality.Semantic.DeepEqual(before[i], object) {
					continue
				}
				if err := f.client.Update(context.Background(), object); err != nil {
					t.Fatal(err)
				}
			}
			raw, err := json.Marshal(f.expected)
			if err != nil {
				t.Fatal(err)
			}
			f.state.Data[PodExpectationKey] = string(raw)
			if err := f.client.Update(context.Background(), f.state); err != nil {
				t.Fatal(err)
			}
			if response := f.handler.Validate(context.Background(), podRequest(t, f.pod, admissionv1.Create, nil)); response.Allowed {
				t.Fatal("stale, held or spoofed controller state accepted")
			}
		})
	}
}

func TestPodAdmissionUpdateAndExtraContainerBoundary(t *testing.T) {
	f := newPodFixture(t)
	f.pod.UID, f.pod.ResourceVersion, f.pod.Spec.NodeName = "pod-uid", "", "scheduled-node"
	if err := f.client.Create(context.Background(), f.pod); err != nil {
		t.Fatal(err)
	}
	current := &corev1.Pod{}
	if err := f.client.Get(context.Background(), client.ObjectKeyFromObject(f.pod), current); err != nil {
		t.Fatal(err)
	}
	current.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}
	request := podRequest(t, current.DeepCopy(), admissionv1.Update, current)
	if response := f.handler.Validate(context.Background(), request); !response.Allowed {
		t.Fatalf("unchanged scheduled Pod update denied: %+v", response.Result)
	}
	changed := current.DeepCopy()
	changed.Spec.NodeName = "injected-node"
	if response := f.handler.Validate(context.Background(), podRequest(t, changed, admissionv1.Update, current)); response.Allowed {
		t.Fatal("an update changed authoritative placement")
	}
	changed = current.DeepCopy()
	changed.Spec.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug", Image: "example.com/debug:v1"}}}
	request = podRequest(t, changed, admissionv1.Update, current)
	request.SubResource = "ephemeralcontainers"
	if response := f.handler.Validate(context.Background(), request); response.Allowed {
		t.Fatal("ephemeral container injection accepted")
	}
}

func TestPodAdmissionAgentStatusAndUnrelatedMetadataDoNotInvalidateIntent(t *testing.T) {
	f := newPodFixture(t)
	ctx := context.Background()
	agent := &api.Agent{}
	if err := f.client.Get(ctx, client.ObjectKeyFromObject(f.fixture.Agent), agent); err != nil {
		t.Fatal(err)
	}
	oldVersion := agent.ResourceVersion
	agent.Status.Phase = api.PhaseFailed
	if err := f.client.Status().Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	agent.Labels = map[string]string{"example.com/note": "updated"}
	agent.Annotations["example.com/note"] = "updated"
	agent.Finalizers = append(agent.Finalizers, "example.com/unrelated")
	if err := f.client.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if agent.ResourceVersion == oldVersion {
		t.Fatal("fixture did not change the Agent resourceVersion")
	}
	if response := f.handler.Validate(ctx, podRequest(t, f.pod, admissionv1.Create, nil)); !response.Allowed {
		t.Fatalf("status-only or unrelated metadata changes denied an otherwise authorized Pod: %+v", response.Result)
	}
}

func TestPodAdmissionMetadataOnlyReauthorizationInvalidatesIntent(t *testing.T) {
	f := newPodFixture(t)
	ctx := context.Background()
	agent := &api.Agent{}
	if err := f.client.Get(ctx, client.ObjectKeyFromObject(f.fixture.Agent), agent); err != nil {
		t.Fatal(err)
	}
	reauthorize, err := sandboxbinding.FormatReauthorization("fresh-request", f.expected.Binding)
	if err != nil {
		t.Fatal(err)
	}
	agent.Annotations[sandboxbinding.ReauthorizeAnnotation] = reauthorize
	if err := f.client.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	binding, err := sandboxbinding.AuthorizedBinding(agent)
	if err != nil || binding != f.expected.Binding || agent.Generation != f.expected.AgentGeneration {
		t.Fatal("fixture must retain generation and bound UIDs")
	}
	response := f.handler.Validate(ctx, podRequest(t, f.pod, admissionv1.Create, nil))
	if response.Allowed || !strings.HasPrefix(response.Result.Message, "SourceChanged:") {
		t.Fatalf("metadata-only authorization intent reused an old expectation: %+v", response.Result)
	}
}

func TestPodAdmissionRejectsUnsupportedExpectationOrInvalidAgentSource(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		version string
	}{
		{"status-sensitive Agent source", ""},
		{"unsupported v2", "v2"},
		{"unsupported v3", "v3"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newPodFixture(t)
			if scenario.version != "" {
				fixture.expected.Version = scenario.version
			} else {
				for sourceIndex := range fixture.expected.Sources {
					if fixture.expected.Sources[sourceIndex].IsAgent() {
						fixture.expected.Sources[sourceIndex].Generation, fixture.expected.Sources[sourceIndex].IntentDigest = 0, ""
						fixture.expected.Sources[sourceIndex].ResourceVersion = fixture.fixture.Agent.ResourceVersion
					}
				}
			}
			raw, err := json.Marshal(fixture.expected)
			if err != nil {
				t.Fatal(err)
			}
			fixture.state.Data[PodExpectationKey] = string(raw)
			if err := fixture.client.Update(context.Background(), fixture.state); err != nil {
				t.Fatal(err)
			}
			if response := fixture.handler.Validate(context.Background(), podRequest(t, fixture.pod, admissionv1.Create, nil)); response.Allowed {
				t.Fatal("unsupported version or status-sensitive Agent source was accepted")
			}
		})
	}
}

type unavailablePodReader struct{ calls int }

func (r *unavailablePodReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	r.calls++
	return fmt.Errorf("synthetic API outage")
}
func (r *unavailablePodReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	r.calls++
	return fmt.Errorf("synthetic API outage")
}

func TestPodCleanupDoesNotReadDependencies(t *testing.T) {
	reader := &unavailablePodReader{}
	handler, err := NewPodHandler(reader, PodConfig{StateNamespace: "operator-state"})
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []admission.Request{
		{AdmissionRequest: admissionv1.AdmissionRequest{Operation: admissionv1.Delete}},
		{AdmissionRequest: admissionv1.AdmissionRequest{Operation: admissionv1.Update, SubResource: "status"}},
	} {
		if response := handler.Validate(context.Background(), request); !response.Allowed {
			t.Fatal("Pod cleanup/status depended on the webhook's resource reader")
		}
	}
	for _, username := range []string{
		"system:kube-controller-manager",
		"system:serviceaccount:kube-system:generic-garbage-collector",
	} {
		pod, old := podFinalizationPair()
		request := podRequest(t, pod, admissionv1.Update, old)
		request.UserInfo.Username = username
		if response := handler.Validate(context.Background(), request); !response.Allowed {
			t.Fatalf("GC finalization consulted unavailable dependencies: %+v", response.Result)
		}
	}
	if reader.calls != 0 {
		t.Fatal("cleanup/status/GC finalization consulted dependencies")
	}
}

func podFinalizationPair() (*corev1.Pod, *corev1.Pod) {
	old := &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "cleanup-0", Namespace: "protected-example", UID: "pod-uid", ResourceVersion: "7",
			DeletionTimestamp: ptr.To(metav1.NewTime(time.Unix(1700000000, 0))),
			Finalizers:        []string{"example.com/first", metav1.FinalizerDeleteDependents, "example.com/last"},
			Labels:            map[string]string{"example.com/workload": "cleanup"},
			Annotations:       map[string]string{"example.com/revision": "original"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "agent", Image: "example.com/agent:v1"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	pod := old.DeepCopy()
	pod.Finalizers = []string{"example.com/first", "example.com/last"}
	return pod, old
}

type countingPodReader struct {
	client.Reader
	calls atomic.Int64
}

func (r *countingPodReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	r.calls.Add(1)
	return r.Reader.Get(ctx, key, object, options...)
}

func (r *countingPodReader) List(ctx context.Context, objects client.ObjectList, options ...client.ListOption) error {
	r.calls.Add(1)
	return r.Reader.List(ctx, objects, options...)
}

func TestPodGarbageCollectorCleanupIgnoresBrokenAuthority(t *testing.T) {
	for _, state := range []string{"missing expectation", "stale expectation", "held expectation", "reader outage"} {
		t.Run(state, func(t *testing.T) {
			f := newPodFixture(t)
			ctx := context.Background()
			switch state {
			case "missing expectation":
				if err := f.client.Delete(ctx, f.state); err != nil {
					t.Fatal(err)
				}
			case "stale expectation", "held expectation":
				if state == "stale expectation" {
					f.expected.AgentGeneration++
				} else {
					f.expected.ExecutionHeld = true
				}
				raw, err := json.Marshal(f.expected)
				if err != nil {
					t.Fatal(err)
				}
				f.state.Data[PodExpectationKey] = string(raw)
				if err := f.client.Update(ctx, f.state); err != nil {
					t.Fatal(err)
				}
			case "reader outage":
				f.handler.reader = &unavailablePodReader{}
			}
			for _, username := range []string{
				"system:kube-controller-manager",
				"system:serviceaccount:kube-system:generic-garbage-collector",
			} {
				request := podRequest(t, f.pod, admissionv1.Create, nil)
				request.UserInfo.Username = username
				if response := f.handler.Validate(ctx, request); response.Allowed {
					t.Fatal("GC identity bypassed broken authority for Pod creation")
				}
				reader := &countingPodReader{Reader: f.handler.reader}
				handler, err := NewPodHandler(reader, f.handler.config)
				if err != nil {
					t.Fatal(err)
				}
				for _, retainOthers := range []bool{false, true} {
					pod, old := podFinalizationPair()
					pod.Namespace, old.Namespace = f.pod.Namespace, f.pod.Namespace
					if !retainOthers {
						old.Finalizers, pod.Finalizers = []string{metav1.FinalizerDeleteDependents}, nil
					}
					// API-server field ownership changes are intentionally ignored.
					pod.ManagedFields = []metav1.ManagedFieldsEntry{{
						Manager: "garbage-collector", Operation: metav1.ManagedFieldsOperationUpdate,
						APIVersion: "v1", FieldsType: "FieldsV1",
						FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:metadata":{"f:finalizers":{}}}`)},
					}}
					request = podRequest(t, pod, admissionv1.Update, old)
					request.UserInfo.Username = username
					request.RequestKind, request.RequestResource = &request.Kind, &request.Resource
					if response := handler.Validate(ctx, request); !response.Allowed {
						t.Fatalf("GC cleanup depended on %s: %+v", state, response.Result)
					}
				}
				if reader.calls.Load() != 0 {
					t.Fatalf("GC cleanup read %s dependencies", state)
				}
			}
		})
	}
}

func podCleanupObjectMutations() map[string]func(*corev1.Pod, *corev1.Pod) {
	return map[string]func(*corev1.Pod, *corev1.Pod){
		"spec":                 func(p, _ *corev1.Pod) { p.Spec.Containers[0].Image = "example.com/other:v1" },
		"status":               func(p, _ *corev1.Pod) { p.Status.Phase = corev1.PodRunning },
		"labels":               func(p, _ *corev1.Pod) { p.Labels["example.com/workload"] = "changed" },
		"annotations":          func(p, _ *corev1.Pod) { p.Annotations["example.com/revision"] = "changed" },
		"owner references":     func(p, _ *corev1.Pod) { p.OwnerReferences = []metav1.OwnerReference{{UID: "other"}} },
		"old name":             func(_, old *corev1.Pod) { old.Name = "other" },
		"old namespace":        func(_, old *corev1.Pod) { old.Namespace = "other" },
		"UID":                  func(p, _ *corev1.Pod) { p.UID = "other" },
		"resource version":     func(p, _ *corev1.Pod) { p.ResourceVersion = "8" },
		"generation":           func(p, _ *corev1.Pod) { p.Generation++ },
		"generate name":        func(p, _ *corev1.Pod) { p.GenerateName = "other-" },
		"self link":            func(p, _ *corev1.Pod) { p.SelfLink = "/other" },
		"creation timestamp":   func(p, _ *corev1.Pod) { p.CreationTimestamp = metav1.NewTime(time.Unix(1600000000, 0)) },
		"deletion timestamp":   func(p, _ *corev1.Pod) { p.DeletionTimestamp = ptr.To(metav1.NewTime(time.Unix(1700000001, 0))) },
		"deletion grace":       func(p, _ *corev1.Pod) { p.DeletionGracePeriodSeconds = ptr.To[int64](0) },
		"not terminating":      func(p, old *corev1.Pod) { p.DeletionTimestamp, old.DeletionTimestamp = nil, nil },
		"missing UID":          func(p, old *corev1.Pod) { p.UID, old.UID = "", "" },
		"missing revision":     func(p, old *corev1.Pod) { p.ResourceVersion, old.ResourceVersion = "", "" },
		"no finalizer removal": func(p, old *corev1.Pod) { p.Finalizers = slices.Clone(old.Finalizers) },
		"remove other":         func(p, _ *corev1.Pod) { p.Finalizers = p.Finalizers[:1] },
		"add other":            func(p, _ *corev1.Pod) { p.Finalizers = append(p.Finalizers, "example.com/extra") },
		"reorder others":       func(p, _ *corev1.Pod) { slices.Reverse(p.Finalizers) },
		"replace other":        func(p, _ *corev1.Pod) { p.Finalizers[0] = "example.com/replacement" },
		"duplicate foreground": func(_, old *corev1.Pod) { old.Finalizers = append(old.Finalizers, metav1.FinalizerDeleteDependents) },
		"no foreground":        func(_, old *corev1.Pod) { old.Finalizers = []string{"example.com/first", "example.com/last"} },
	}
}

func TestPodGarbageCollectorCleanupRejectsOtherChanges(t *testing.T) {
	for name, mutate := range podCleanupObjectMutations() {
		t.Run(name, func(t *testing.T) {
			pod, old := podFinalizationPair()
			mutate(pod, old)
			request := podRequest(t, pod, admissionv1.Update, old)
			request.UserInfo.Username = "system:kube-controller-manager"
			handler, err := NewPodHandler(&unavailablePodReader{}, PodConfig{StateNamespace: "operator-state"})
			if err != nil {
				t.Fatal(err)
			}
			if response := handler.Validate(context.Background(), request); response.Allowed {
				t.Fatalf("%s bypassed Pod policy as GC cleanup", name)
			}
		})
	}
}

func TestPodGarbageCollectorCleanupRejectsOtherCallersAndRequestShapes(t *testing.T) {
	pod, old := podFinalizationPair()
	handler, err := NewPodHandler(&unavailablePodReader{}, PodConfig{StateNamespace: "operator-state"})
	if err != nil {
		t.Fatal(err)
	}
	for _, username := range []string{
		"", "member@example.com", "admin@example.com",
		"system:serviceaccount:cetirun-system:cetirun-operator-controller-manager",
		"system:kube-controller-manager-extra", "System:kube-controller-manager",
		"system:serviceaccount:other:generic-garbage-collector",
		"system:serviceaccount:kube-system:generic-garbage-collector-extra",
	} {
		t.Run(username, func(t *testing.T) {
			request := podRequest(t, pod, admissionv1.Update, old)
			request.UserInfo = authenticationv1.UserInfo{Username: username, Groups: []string{"system:masters", "system:serviceaccounts"}}
			if response := handler.Validate(context.Background(), request); response.Allowed {
				t.Fatal("non-GC caller bypassed normal Pod admission")
			}
		})
	}
	for name, mutate := range podCleanupRequestMutations() {
		t.Run(name, func(t *testing.T) {
			request := podRequest(t, pod, admissionv1.Update, old)
			request.UserInfo.Username = "system:kube-controller-manager"
			mutate(&request)
			if response := handler.Validate(context.Background(), request); response.Allowed {
				t.Fatal("invalid or non-main UPDATE request bypassed normal admission")
			}
		})
	}
	for _, subresource := range []string{"status", "finalize"} {
		request := podRequest(t, pod, admissionv1.Update, old)
		request.UserInfo.Username, request.SubResource = "system:kube-controller-manager", subresource
		if podGarbageCollectorFinalizerUpdate(request, pod) {
			t.Fatal("GC exception matched a subresource")
		}
	}
}

func podCleanupRequestMutations() map[string]func(*admission.Request) {
	return map[string]func(*admission.Request){
		"create":               func(r *admission.Request) { r.Operation = admissionv1.Create },
		"connect":              func(r *admission.Request) { r.Operation, r.SubResource = admissionv1.Connect, "exec" },
		"unknown operation":    func(r *admission.Request) { r.Operation = "UNKNOWN" },
		"ephemeral containers": func(r *admission.Request) { r.SubResource = "ephemeralcontainers" },
		"resize":               func(r *admission.Request) { r.SubResource = "resize" },
		"unknown subresource":  func(r *admission.Request) { r.SubResource = "finalize" },
		"original subresource": func(r *admission.Request) { r.RequestSubResource = "resize" },
		"resource":             func(r *admission.Request) { r.Resource.Resource = "secrets" },
		"resource group":       func(r *admission.Request) { r.Resource.Group = "other.example.com" },
		"resource version":     func(r *admission.Request) { r.Resource.Version = "v2" },
		"kind":                 func(r *admission.Request) { r.Kind.Kind = "Secret" },
		"kind group":           func(r *admission.Request) { r.Kind.Group = "other.example.com" },
		"kind version":         func(r *admission.Request) { r.Kind.Version = "v2" },
		"original resource": func(r *admission.Request) {
			r.RequestResource = &metav1.GroupVersionResource{Version: "v1", Resource: "secrets"}
		},
		"original kind":        func(r *admission.Request) { r.RequestKind = &metav1.GroupVersionKind{Version: "v1", Kind: "Secret"} },
		"missing request name": func(r *admission.Request) { r.Name = "" },
		"request name":         func(r *admission.Request) { r.Name = "other" },
		"missing namespace":    func(r *admission.Request) { r.Namespace = "" },
		"request namespace":    func(r *admission.Request) { r.Namespace = "other" },
		"missing previous Pod": func(r *admission.Request) { r.OldObject.Raw = nil },
		"null previous Pod":    func(r *admission.Request) { r.OldObject.Raw = []byte("null") },
		"missing current Pod":  func(r *admission.Request) { r.Object.Raw = nil },
		"null current Pod":     func(r *admission.Request) { r.Object.Raw = []byte("null") },
	}
}

func TestPodGarbageCollectorCleanupDecodesBothPodsStrictly(t *testing.T) {
	for _, previous := range []bool{false, true} {
		for name, mutate := range map[string]func([]byte) []byte{
			"unknown top level": func(raw []byte) []byte {
				return bytes.Replace(raw, []byte(`"spec":`), []byte(`"futureField":true,"spec":`), 1)
			},
			"unknown spec": func(raw []byte) []byte {
				return bytes.Replace(raw, []byte(`"spec":{`), []byte(`"spec":{"futureField":true,`), 1)
			},
			"unknown metadata": func(raw []byte) []byte {
				return bytes.Replace(raw, []byte(`"metadata":{`), []byte(`"metadata":{"futureField":true,`), 1)
			},
			"unknown status": func(raw []byte) []byte {
				return bytes.Replace(raw, []byte(`"status":{`), []byte(`"status":{"futureField":true,`), 1)
			},
			"duplicate field": func(raw []byte) []byte {
				return bytes.Replace(raw, []byte(`"kind":"Pod"`), []byte(`"kind":"Pod","kind":"Pod"`), 1)
			},
			"wrong kind": func(raw []byte) []byte {
				return bytes.Replace(raw, []byte(`"kind":"Pod"`), []byte(`"kind":"Secret"`), 1)
			},
			"wrong api version": func(raw []byte) []byte {
				return bytes.Replace(raw, []byte(`"apiVersion":"v1"`), []byte(`"apiVersion":"v2"`), 1)
			},
			"trailing object":  func(raw []byte) []byte { return append(raw, []byte(`{}`)...) },
			"malformed object": func(raw []byte) []byte { return raw[:len(raw)-1] },
			"array":            func([]byte) []byte { return []byte(`[]`) },
		} {
			t.Run(fmt.Sprintf("previous=%t/%s", previous, name), func(t *testing.T) {
				pod, old := podFinalizationPair()
				request := podRequest(t, pod, admissionv1.Update, old)
				request.UserInfo.Username = "system:kube-controller-manager"
				if previous {
					request.OldObject.Raw = mutate(request.OldObject.Raw)
				} else {
					request.Object.Raw = mutate(request.Object.Raw)
				}
				handler, err := NewPodHandler(&unavailablePodReader{}, PodConfig{StateNamespace: "operator-state"})
				if err != nil {
					t.Fatal(err)
				}
				if response := handler.Validate(context.Background(), request); response.Allowed {
					t.Fatal("invalid raw Pod representation received the GC exemption")
				}
			})
		}
	}
}

func TestPodConnectHasNoCallerOrStateExemption(t *testing.T) {
	reader := &unavailablePodReader{}
	handler, err := NewPodHandler(reader, PodConfig{StateNamespace: "operator-state"})
	if err != nil {
		t.Fatal(err)
	}
	for _, caller := range []string{"member@example.com", "admin@example.com",
		"system:serviceaccount:cetirun-system:cetirun-operator-controller-manager",
		"system:serviceaccount:cetirun-admin:openclaw-admin-cetirun",
		"system:kube-controller-manager",
		"system:serviceaccount:kube-system:generic-garbage-collector"} {
		for _, subresource := range []string{"exec", "attach", "portforward"} {
			request := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
				Operation: admissionv1.Connect, Namespace: "protected-example", Name: "example-0",
				Resource: metav1.GroupVersionResource{Version: "v1", Resource: "pods"}, SubResource: subresource,
				UserInfo: authenticationv1.UserInfo{Username: caller, Groups: []string{"system:masters"}},
				Object:   runtime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"PodExecOptions","command":["true"]}`)},
			}}
			response := handler.Validate(context.Background(), request)
			if response.Allowed || response.Result.Code != http.StatusForbidden || !strings.Contains(response.Result.Message, "ConnectForbidden") {
				t.Fatalf("CONNECT %s for %s did not fail closed: %+v", subresource, caller, response.Result)
			}
		}
	}
	if reader.calls != 0 {
		t.Fatal("CONNECT protection consulted expectations or API dependencies")
	}
}

type blockingPodReader struct {
	client.Reader
	entered     chan struct{}
	release     chan struct{}
	sourcesOnly bool
	failName    string
	active      atomic.Int32
	maximum     atomic.Int32
	canceled    atomic.Int32
}

func (r *blockingPodReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if r.sourcesOnly {
		_, metadata := object.(*metav1.PartialObjectMetadata)
		_, runtimeClass := object.(*nodev1.RuntimeClass)
		if !metadata && !runtimeClass {
			return r.Reader.Get(ctx, key, object, options...)
		}
	}
	active := r.active.Add(1)
	defer r.active.Add(-1)
	for maximum := r.maximum.Load(); active > maximum; maximum = r.maximum.Load() {
		if r.maximum.CompareAndSwap(maximum, active) {
			break
		}
	}
	r.entered <- struct{}{}
	if r.failName != "" && key.Name != r.failName {
		<-ctx.Done()
		r.canceled.Add(1)
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		r.canceled.Add(1)
		return ctx.Err()
	case <-r.release:
	}
	if r.failName != "" {
		return fmt.Errorf("synthetic-sensitive-reader-error")
	}
	return r.Reader.Get(ctx, key, object, options...)
}

func TestPodSourcesUseBoundedConcurrencyAndCancelOnFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("failure=%t", fail), func(t *testing.T) {
			f := newPodFixture(t)
			reader := &blockingPodReader{Reader: f.client, entered: make(chan struct{}, 128), release: make(chan struct{})}
			if fail {
				reader.failName = f.expected.Sources[0].Name
			}
			f.handler.reader = reader
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			done := make(chan *podFailure, 1)
			go func() { done <- f.handler.validatePodSources(ctx, f.expected, f.fixture.Agent) }()
			for range podSourceReadConcurrency {
				select {
				case <-reader.entered:
				case <-ctx.Done():
					t.Fatal("source reads did not run concurrently")
				}
			}
			if reader.active.Load() != podSourceReadConcurrency {
				t.Fatal("source reads exceeded or did not fill the concurrency bound")
			}
			close(reader.release)
			select {
			case failure := <-done:
				if fail {
					if failure == nil || failure.reason != "SourceReadFailed" || strings.Contains(failure.Error(), "sensitive") {
						t.Fatalf("source read failed without a safe reason: %v", failure)
					}
					if reader.canceled.Load() != podSourceReadConcurrency-1 {
						t.Fatal("source failure did not cancel outstanding sibling reads")
					}
				} else if failure != nil {
					t.Fatal(failure)
				}
			case <-ctx.Done():
				t.Fatal("source workers did not finish after success or cancellation")
			}
			if reader.maximum.Load() != podSourceReadConcurrency || reader.active.Load() != 0 {
				t.Fatal("source reads exceeded the bound or left workers running")
			}
		})
	}
}

func TestPodValidationDeadlineCoversAllReadStages(t *testing.T) {
	for _, sourcesOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("sources-only=%t", sourcesOnly), func(t *testing.T) {
			f := newPodFixture(t)
			reader := &blockingPodReader{
				Reader: f.client, entered: make(chan struct{}, 128), release: make(chan struct{}), sourcesOnly: sourcesOnly,
			}
			handler, err := NewPodHandler(reader, PodConfig{StateNamespace: "operator-state", RequestTimeout: 30 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			response := handler.Validate(context.Background(), podRequest(t, f.pod, admissionv1.Create, nil))
			if response.Allowed || response.Result.Code != http.StatusGatewayTimeout ||
				!strings.Contains(response.Result.Message, "RequestDeadlineExceeded") {
				t.Fatalf("request did not honor its total validation budget: %+v", response.Result)
			}
			if reader.canceled.Load() == 0 || reader.active.Load() != 0 {
				t.Fatal("deadline did not cancel and join in-flight reads")
			}
			if sourcesOnly && reader.maximum.Load() != podSourceReadConcurrency {
				t.Fatal("deadline fixture did not exercise concurrent source reads")
			}
		})
	}
	for _, timeout := range []time.Duration{-1, 30 * time.Second, time.Minute} {
		if _, err := NewPodHandler(&unavailablePodReader{}, PodConfig{StateNamespace: "operator-state", RequestTimeout: timeout}); err == nil {
			t.Fatal("invalid request timeout accepted")
		}
	}
}

func TestPodDiagnosticsExposePathsWithoutValues(t *testing.T) {
	f := newPodFixture(t)
	f.pod.Spec.Containers[0].Env = append(f.pod.Spec.Containers[0].Env,
		corev1.EnvVar{Name: "SYNTHETIC_PRIVATE_KEY_NAME", Value: "synthetic-sensitive-env-value"})
	var output strings.Builder
	ctx := log.IntoContext(context.Background(), funcr.New(func(prefix, message string) {
		output.WriteString(prefix + message)
	}, funcr.Options{}))
	response := f.handler.Validate(ctx, podRequest(t, f.pod, admissionv1.Create, nil))
	if response.Allowed || !strings.Contains(response.Result.Message, "TemplateMismatch") ||
		!strings.Contains(response.Result.Message, "spec.containers[0].env") {
		t.Fatalf("missing safe field-path diagnostic: %+v", response.Result)
	}
	if !strings.Contains(output.String(), "TemplateMismatch") || !strings.Contains(output.String(), "spec.containers[0].env") {
		t.Fatal("structured rejection log is missing the reason and field path")
	}
	for _, sensitive := range []string{"SYNTHETIC_PRIVATE_KEY_NAME", "synthetic-sensitive-env-value"} {
		if strings.Contains(output.String()+response.Result.Message, sensitive) {
			t.Fatal("diagnostic disclosed a field value")
		}
	}
	if err := f.client.Delete(ctx, f.state); err != nil {
		t.Fatal(err)
	}
	response = f.handler.Validate(ctx, podRequest(t, f.pod, admissionv1.Create, nil))
	if response.Allowed || !strings.Contains(response.Result.Message, "ExpectationMissing") {
		t.Fatalf("missing expectation was not distinguishable: %+v", response.Result)
	}
	response = rejectPod(ctx, podRequest(t, f.pod, admissionv1.Create, nil),
		podReadFailure("Source", fmt.Errorf("synthetic-sensitive-reader-error")))
	if response.Allowed || response.Result.Code != http.StatusServiceUnavailable ||
		strings.Contains(output.String()+response.Result.Message, "synthetic-sensitive-reader-error") {
		t.Fatal("raw API error was not replaced with a safe read-failure diagnostic")
	}
}

type changingPodExpectationReader struct {
	client.Reader
	stateReads atomic.Int32
}

func (r *changingPodExpectationReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if err := r.Reader.Get(ctx, key, object, options...); err != nil {
		return err
	}
	if key.Namespace == "operator-state" {
		if _, ok := object.(*corev1.ConfigMap); ok && r.stateReads.Add(1) == 2 {
			object.SetResourceVersion("changed-during-validation")
		}
	}
	return nil
}

func TestPodValidationRetainsFinalExpectationFence(t *testing.T) {
	f := newPodFixture(t)
	reader := &changingPodExpectationReader{Reader: f.client}
	f.handler.reader = reader
	response := f.handler.Validate(context.Background(), podRequest(t, f.pod, admissionv1.Create, nil))
	if response.Allowed || reader.stateReads.Load() != 2 || !strings.Contains(response.Result.Message, "ExpectationChanged") {
		t.Fatalf("changed expectation bypassed the final state fence: %+v", response.Result)
	}
}

func TestPodHTTPWebhookAndStrictDecoding(t *testing.T) {
	f := newPodFixture(t)
	request := podRequest(t, f.pod, admissionv1.Create, nil)
	review := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"}, Request: &request.AdmissionRequest,
	}
	body, err := json.Marshal(review)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	httpRequest := httptest.NewRequest(http.MethodPost, PodValidationPath, bytes.NewReader(body))
	httpRequest.Header.Set("Content-Type", "application/json")
	(&admission.Webhook{Handler: f.handler}).ServeHTTP(recorder, httpRequest)
	var result admissionv1.AdmissionReview
	if json.Unmarshal(recorder.Body.Bytes(), &result) != nil || result.Response == nil || !result.Response.Allowed {
		t.Fatalf("real AdmissionReview handler failed: %s", recorder.Body.String())
	}
	request.Object.Raw = bytes.Replace(request.Object.Raw, []byte(`"spec":{`), []byte(`"spec":{"futurePrivilege":true,`), 1)
	if response := f.handler.Validate(context.Background(), request); response.Allowed {
		t.Fatal("unknown final Pod field was ignored")
	}
}
