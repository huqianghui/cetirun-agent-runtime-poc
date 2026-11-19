// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxplacement

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	"helm.sh/helm/v3/pkg/chart/loader"
	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/storage/names"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

type placementFixture struct {
	client          client.Client
	handler         *Handler
	registry        registry
	state           *corev1.ConfigMap
	reserved        *corev1.Node
	ordinary        *corev1.Node
	platformNS      *corev1.Namespace
	platformAccount *corev1.ServiceAccount
	daemon          *appsv1.DaemonSet
	platformPod     *corev1.Pod
	agent           testfixture.Fixture
	agentPod        *corev1.Pod
	workload        *appsv1.StatefulSet
	expected        sandboxworkload.Expectation
	expectation     *corev1.ConfigMap
}

func newFixture(t *testing.T) *placementFixture {
	t.Helper()
	f := &placementFixture{agent: testfixture.New("openclaw")}
	f.reserved = &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "reserved-worker", UID: "reserved-node-uid", ResourceVersion: "1",
		Labels: map[string]string{"example.com/pool": "sandbox", "example.com/platform": "true"},
	}, Spec: corev1.NodeSpec{ProviderID: "example://reserved-worker"}}
	f.ordinary = &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "ordinary-worker", UID: "ordinary-node-uid", ResourceVersion: "1",
		Labels: map[string]string{"example.com/platform": "true"},
	}, Spec: corev1.NodeSpec{ProviderID: "example://ordinary-worker"}}
	f.agent.Input.GatewayKey = &sandboxworkload.ObjectIdentity{Name: "gateway-key", UID: "gateway-key-uid"}
	f.agent.Input.AuthorizedSecrets = append(f.agent.Input.AuthorizedSecrets, "gateway-key")
	bundle, err := sandboxworkload.Build(f.agent.Input)
	if err != nil {
		t.Fatal(err)
	}
	chart, err := loader.Load("../../../cetirun-chart")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := helm.PrepareSandboxChart(chart, map[string]any{
		"agentType": "openclaw", "agentRequest": map[string]any{"type": "openclaw"},
		"image":     map[string]any{"repository": "registry.example.com/agent/runtime", "tag": "1.0"},
		"secrets":   map[string]any{"existingSecret": "runtime-secret", "provider": "openai", "model": "example-model", "apiBase": "http://gateway.example-platform.svc.cluster.local:4000/v1"},
		"aiGateway": map[string]any{"enabled": true, "keySecretName": "gateway-key", "keySecretKey": "api-key", "models": []any{map[string]any{"id": "example-model", "name": "Example"}}},
	}, nil, bundle)
	if err != nil {
		t.Fatal(err)
	}
	template, err := prepared.SandboxTemplate()
	if err != nil {
		t.Fatal(err)
	}
	f.workload = testfixture.Workload(f.agent.Input, *template)
	f.expected, err = bundle.Expect(*template, f.workload, nil)
	if err != nil {
		t.Fatal(err)
	}
	// These generic admission records predate native Pod lifetime protection;
	// preserve their compatibility checks without authorizing new execution.
	f.expected.Template.Finalizers = nil
	f.workload.Spec.Template.Finalizers = nil
	f.expected.ControllerRevision = f.workload.Status.UpdateRevision
	f.agentPod = testfixture.Pod(f.expected)
	f.agentPod.UID, f.agentPod.ResourceVersion = "agent-pod-uid", "1"
	f.platformNS = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "platform-system", UID: "platform-namespace-uid", ResourceVersion: "1"}}
	f.platformAccount = &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: "platform-daemon", Namespace: f.platformNS.Name, UID: "platform-account-uid", ResourceVersion: "1",
	}}
	f.daemon = &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "platform-daemon", Namespace: f.platformNS.Name, UID: "daemon-uid", ResourceVersion: "1", Generation: 1},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "platform-daemon"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "platform-daemon"}},
				Spec: corev1.PodSpec{
					Containers:         []corev1.Container{{Name: "daemon", Image: "registry.example.com/platform/daemon:1.0"}},
					ServiceAccountName: f.platformAccount.Name, AutomountServiceAccountToken: ptr.To(false),
					NodeSelector: map[string]string{"example.com/platform": "true"},
				},
			},
		},
	}
	f.platformPod = daemonPod(f.daemon, f.reserved.Name)
	digest, err := CanonicalDaemonSetDigest(f.daemon)
	if err != nil {
		t.Fatal(err)
	}
	f.registry = registry{
		Version: RegistryVersion,
		Nodes: []registeredNode{
			{Name: f.reserved.Name, UID: f.reserved.UID, ProviderID: f.reserved.Spec.ProviderID, Reservation: &nodeReservation{
				Agent: f.expected.Agent, Namespace: f.expected.Namespace, StatefulSet: f.expected.Workload,
			}},
			{Name: f.ordinary.Name, UID: f.ordinary.UID, ProviderID: f.ordinary.Spec.ProviderID},
		},
		PlatformDaemonSets: []registeredDaemonSet{{
			Name: f.daemon.Name, UID: f.daemon.UID,
			Namespace:      sandboxworkload.ObjectIdentity{Name: f.platformNS.Name, UID: f.platformNS.UID},
			TemplateDigest: digest, TrustedCreator: "system:kube-controller-manager",
			BootstrapPods: []sandboxworkload.ObjectIdentity{{Name: f.platformPod.Name, UID: f.platformPod.UID}},
		}},
	}
	f.state = &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-placement", Namespace: "operator-state", UID: "registry-uid", ResourceVersion: "1"},
		Data:       map[string]string{ConfigMapKey: string(marshal(t, f.registry))},
	}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, api.AddToScheme, nodev1.AddToScheme, schedulingv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	sources := []client.Object{
		f.agent.Agent, f.agent.Team, f.agent.Sandbox, f.agent.Policy, f.agent.Namespace,
		f.agent.ServiceAccount, f.agent.Input.Home, f.agent.Input.Skills, f.agent.Input.RuntimeClass, f.workload,
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "example-config", Namespace: f.agent.Namespace.Name, UID: "config-uid", ResourceVersion: "1"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "runtime-secret", Namespace: f.agent.Namespace.Name, UID: "secret-uid", ResourceVersion: "1"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "gateway-key", Namespace: f.agent.Namespace.Name, UID: "gateway-key-uid", ResourceVersion: "1"}},
	}
	for _, object := range sources {
		if agent, ok := object.(*api.Agent); ok {
			source, err := sandboxworkload.AgentSourceReference(agent)
			if err != nil {
				t.Fatal(err)
			}
			f.expected.Sources = append(f.expected.Sources, source)
			continue
		}
		kinds, _, err := scheme.ObjectKinds(object)
		if err != nil {
			t.Fatal(err)
		}
		f.expected.Sources = append(f.expected.Sources, sandboxworkload.SourceReference{
			APIVersion: kinds[0].GroupVersion().String(), Kind: kinds[0].Kind, Namespace: object.GetNamespace(),
			Name: object.GetName(), UID: object.GetUID(), ResourceVersion: object.GetResourceVersion(),
		})
	}
	f.expectation, err = sandboxworkload.PodExpectationObject(f.state.Namespace, f.expected)
	if err != nil {
		t.Fatal(err)
	}
	f.expectation.UID, f.expectation.ResourceVersion = "expectation-uid", "1"
	objects := append(sources, f.reserved, f.ordinary, f.platformNS, f.platformAccount,
		f.daemon, f.platformPod, f.agentPod, f.state, f.expectation,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ordinary-workloads", UID: "ordinary-namespace-uid", ResourceVersion: "1"}})
	f.client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	f.handler, err = NewHandler(f.client, Config{Namespace: f.state.Namespace, ConfigMapName: f.state.Name})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func daemonPod(daemon *appsv1.DaemonSet, node string) *corev1.Pod {
	prefix := daemonPodGenerateName(daemon.Name)
	pod := &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Name: prefix[:min(len(prefix), names.MaxGeneratedNameLength)] + "bcdfg", GenerateName: prefix, Namespace: daemon.Namespace,
			UID: "platform-pod-uid", ResourceVersion: "1",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "DaemonSet", Name: daemon.Name, UID: daemon.UID,
				Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
			}},
			Labels: daemon.Spec.Template.DeepCopy().Labels, Annotations: daemon.Spec.Template.DeepCopy().Annotations,
		},
		Spec: *daemon.Spec.Template.Spec.DeepCopy(),
	}
	pod.Labels[appsv1.ControllerRevisionHashLabelKey] = "synthetic-revision"
	pod.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: targetAffinity(node)}}
	addDaemonTolerations(&pod.Spec)
	return pod
}

func ordinaryPod() *corev1.Pod {
	return &corev1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: "ordinary-pod", Namespace: "ordinary-workloads", UID: "ordinary-pod-uid", ResourceVersion: "1"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "registry.example.com/app:1.0"}}},
	}
}

func podRequest(t *testing.T, pod *corev1.Pod, old *corev1.Pod) admission.Request {
	t.Helper()
	pod = pod.DeepCopy()
	pod.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}
	operation := admissionv1.Create
	if old != nil {
		old = old.DeepCopy()
		old.TypeMeta = pod.TypeMeta
		operation = admissionv1.Update
	}
	request := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID: "request-uid", Operation: operation, Namespace: pod.Namespace, Name: pod.Name,
		Resource: metav1.GroupVersionResource{Version: "v1", Resource: "pods"},
		Kind:     metav1.GroupVersionKind{Version: "v1", Kind: "Pod"}, Object: runtime.RawExtension{Raw: marshal(t, pod)},
		UserInfo: authenticationv1.UserInfo{Username: "example-user", Groups: []string{"system:authenticated"}},
	}}
	if old != nil {
		request.OldObject.Raw = marshal(t, old)
	}
	return request
}

func bindingRequest(t *testing.T, pod *corev1.Pod, node string) admission.Request {
	t.Helper()
	binding := &corev1.Binding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Binding"},
		ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace, UID: pod.UID},
		Target:     corev1.ObjectReference{Kind: "Node", Name: node},
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID: "request-uid", Operation: admissionv1.Create, Namespace: pod.Namespace, Name: pod.Name, SubResource: "binding",
		Resource: metav1.GroupVersionResource{Version: "v1", Resource: "pods"},
		Kind:     metav1.GroupVersionKind{Version: "v1", Kind: "Binding"}, Object: runtime.RawExtension{Raw: marshal(t, binding)},
		UserInfo: authenticationv1.UserInfo{Username: "system:kube-scheduler"},
	}}
}

func marshal(t *testing.T, object any) []byte {
	t.Helper()
	raw, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (f *placementFixture) saveRegistry(t *testing.T) {
	t.Helper()
	current := &corev1.ConfigMap{}
	if err := f.client.Get(context.Background(), client.ObjectKeyFromObject(f.state), current); err != nil {
		t.Fatal(err)
	}
	current.Data[ConfigMapKey] = string(marshal(t, f.registry))
	if err := f.client.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	f.state = current
}

func expectAllowed(t *testing.T, response admission.Response) {
	t.Helper()
	if !response.Allowed || len(response.Patches) != 0 || len(response.Patch) != 0 {
		t.Fatalf("expected read-only placement allowance, got %+v", response)
	}
}

func expectDenied(t *testing.T, response admission.Response, reason string) {
	t.Helper()
	if response.Allowed || response.Result == nil || (reason != "" && !strings.Contains(response.Result.Message, reason)) {
		t.Fatalf("expected denial %q, got %+v", reason, response)
	}
}
