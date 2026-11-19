// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxplacement

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func nodeRequest(t *testing.T, old, updated *corev1.Node) admission.Request {
	t.Helper()
	old = old.DeepCopy()
	old.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Node"}
	request := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID: "request-uid", Operation: admissionv1.Delete, Name: old.Name,
		Resource:  metav1.GroupVersionResource{Version: "v1", Resource: "nodes"},
		Kind:      metav1.GroupVersionKind{Version: "v1", Kind: "Node"},
		OldObject: runtime.RawExtension{Raw: marshal(t, old)},
		UserInfo:  authenticationv1.UserInfo{Username: "example-user", Groups: []string{"system:authenticated"}},
	}}
	if updated != nil {
		updated = updated.DeepCopy()
		updated.TypeMeta = old.TypeMeta
		request.Operation = admissionv1.Update
		request.Object.Raw = marshal(t, updated)
	}
	return request
}

func TestReservedNodeDeletionHasNoCallerExemption(t *testing.T) {
	f := newFixture(t)
	for _, username := range []string{
		"example-user", "system:kube-controller-manager", "system:node:reserved-worker",
		"system:serviceaccount:platform-system:platform-daemon",
	} {
		t.Run(username, func(t *testing.T) {
			request := nodeRequest(t, f.reserved, nil)
			request.UserInfo.Username = username
			request.UserInfo.Groups = []string{"system:masters"}
			expectDenied(t, f.handler.Validate(context.Background(), request), "ReservedNodeDeletion")
			request.DryRun = ptr.To(true)
			expectDenied(t, f.handler.Validate(context.Background(), request), "ReservedNodeDeletion")
		})
	}
	replacement := f.reserved.DeepCopy()
	replacement.UID, replacement.Spec.ProviderID = "replacement-uid", "example://replacement"
	expectDenied(t, f.handler.Validate(context.Background(), nodeRequest(t, replacement, nil)), "ReservedNodeDeletion")
}

func TestNodeLifecycleDoesNotAuthorizeUnknownPlacement(t *testing.T) {
	f := newFixture(t)
	expectAllowed(t, f.handler.Validate(context.Background(), nodeRequest(t, f.ordinary, nil)))
	updated := f.ordinary.DeepCopy()
	updated.Labels = map[string]string{"example.com/pool": "sandbox"}
	expectAllowed(t, f.handler.Validate(context.Background(), nodeRequest(t, f.ordinary, updated)))
	terminating := f.ordinary.DeepCopy()
	terminating.DeletionTimestamp = ptr.To(metav1.Now())
	expectAllowed(t, f.handler.Validate(context.Background(), nodeRequest(t, terminating, nil)))
	unknown := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "new-worker", UID: "new-node-uid"},
		Spec:       corev1.NodeSpec{ProviderID: "example://new-worker"},
	}
	if err := f.client.Create(context.Background(), unknown); err != nil {
		t.Fatal(err)
	}
	expectAllowed(t, f.handler.Validate(context.Background(), nodeRequest(t, unknown, unknown)))
	expectAllowed(t, f.handler.Validate(context.Background(), nodeRequest(t, unknown, nil)))
	pod := ordinaryPod()
	pod.Spec.NodeName = unknown.Name
	expectDenied(t, f.handler.Validate(context.Background(), podRequest(t, pod, nil)), "NodeUnknown")
	pod.Spec.NodeName, pod.ResourceVersion = "", ""
	if err := f.client.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	expectDenied(t, f.handler.Validate(context.Background(), bindingRequest(t, pod, unknown.Name)), "NodeUnknown")
}

func TestReservedNodeLabelsAreNotReservationAuthority(t *testing.T) {
	f := newFixture(t)
	updated := f.reserved.DeepCopy()
	updated.Labels = nil
	updated.Spec.Unschedulable = true
	expectAllowed(t, f.handler.Validate(context.Background(), nodeRequest(t, f.reserved, updated)))
	if err := f.client.Update(context.Background(), updated); err != nil {
		t.Fatal(err)
	}
	pod := ordinaryPod()
	pod.Spec.NodeName = updated.Name
	expectDenied(t, f.handler.Validate(context.Background(), podRequest(t, pod, nil)), "ReservedNode")
	expectDenied(t, f.handler.Validate(context.Background(), nodeRequest(t, updated, nil)), "ReservedNodeDeletion")
}

func TestReservedNodeUpdatePinsLiveIdentity(t *testing.T) {
	for name, mutate := range map[string]func(*corev1.Node, *corev1.Node){
		"UID":              func(_, n *corev1.Node) { n.UID = "replacement-uid" },
		"provider":         func(_, n *corev1.Node) { n.Spec.ProviderID = "example://replacement" },
		"removed provider": func(_, n *corev1.Node) { n.Spec.ProviderID = "" },
		"name":             func(_, n *corev1.Node) { n.Name = "different-worker" },
		"namespace":        func(_, n *corev1.Node) { n.Namespace = "tenant-workloads" },
		"resource version": func(_, n *corev1.Node) { n.ResourceVersion = "changed-version" },
		"terminating":      func(_, n *corev1.Node) { n.DeletionTimestamp = ptr.To(metav1.Now()) },
		"termination adoption": func(old, _ *corev1.Node) {
			old.DeletionTimestamp = ptr.To(metav1.Now())
		},
		"forged previous provider": func(old, _ *corev1.Node) { old.Spec.ProviderID = "example://previous" },
		"forged previous UID": func(old, n *corev1.Node) {
			old.UID, n.UID = "not-the-persisted-uid", "not-the-persisted-uid"
		},
		"stale previous version": func(old, n *corev1.Node) {
			old.ResourceVersion, n.ResourceVersion = "old-version", "old-version"
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			old, updated := f.reserved.DeepCopy(), f.reserved.DeepCopy()
			mutate(old, updated)
			expectDenied(t, f.handler.Validate(context.Background(), nodeRequest(t, old, updated)), "")
		})
	}
	for name, mutate := range map[string]func(*corev1.Node){
		"actual replacement":  func(n *corev1.Node) { n.UID = "replacement-uid" },
		"actual provider":     func(n *corev1.Node) { n.Spec.ProviderID = "example://replacement" },
		"actual termination":  func(n *corev1.Node) { n.DeletionTimestamp = ptr.To(metav1.Now()) },
		"actual version lost": func(n *corev1.Node) { n.ResourceVersion = "" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.handler.reader = mutateReader{Reader: f.client, mutate: func(object client.Object) {
				if node, ok := object.(*corev1.Node); ok {
					mutate(node)
				}
			}}
			expectDenied(t, f.handler.Validate(context.Background(), nodeRequest(t, f.reserved, f.reserved)), "")
		})
	}
}

func TestNodeLifecycleRequiresCurrentRegistryAndSnapshots(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		f := newFixture(t)
		var updated *corev1.Node
		if !deleting {
			updated = f.ordinary
		}
		request := nodeRequest(t, f.ordinary, updated)
		var reads int
		f.handler.reader = mutateReader{Reader: f.client, mutate: func(object client.Object) {
			if state, ok := object.(*corev1.ConfigMap); ok && state.Name == f.state.Name {
				reads++
				if reads > 1 {
					state.ResourceVersion = "changed-version"
				}
			}
		}}
		expectDenied(t, f.handler.Validate(context.Background(), request), "RegistryChanged")
		f.handler.reader = f.client
		if err := f.client.Delete(context.Background(), f.state); err != nil {
			t.Fatal(err)
		}
		expectDenied(t, f.handler.Validate(context.Background(), request), "RegistryMissing")
	}
	f := newFixture(t)
	var reads int
	f.handler.reader = mutateReader{Reader: f.client, mutate: func(object client.Object) {
		if node, ok := object.(*corev1.Node); ok {
			reads++
			if reads > 1 {
				node.Spec.ProviderID = "example://changed-without-version"
			}
		}
	}}
	expectDenied(t, f.handler.Validate(context.Background(), nodeRequest(t, f.reserved, f.reserved)), "NodeChanged")
}

func TestNodeStatusAndUnsupportedRequests(t *testing.T) {
	f := newFixture(t)
	reader := &countingReader{Reader: f.client}
	f.handler.reader = reader
	status := nodeRequest(t, f.reserved, f.reserved)
	status.SubResource = "status"
	expectAllowed(t, f.handler.Validate(context.Background(), status))
	if reader.gets.Load() != 0 {
		t.Fatal("Node status depended on placement configuration")
	}
	for name, mutate := range map[string]func(*admission.Request){
		"CREATE outside matching": func(r *admission.Request) { r.Operation = admissionv1.Create },
		"namespace":               func(r *admission.Request) { r.Namespace = "tenant-workloads" },
		"missing name":            func(r *admission.Request) { r.Name = "" },
		"wrong kind":              func(r *admission.Request) { r.Kind.Kind = "Pod" },
		"wrong subresource":       func(r *admission.Request) { r.SubResource = "proxy" },
		"missing previous Node":   func(r *admission.Request) { r.OldObject.Raw = nil },
		"missing updated Node":    func(r *admission.Request) { r.Object.Raw = nil },
		"duplicate field": func(r *admission.Request) {
			r.OldObject.Raw = []byte(`{"apiVersion":"v1","kind":"Node","kind":"Node","metadata":{"name":"reserved-worker","uid":"reserved-node-uid","resourceVersion":"1"}}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			request := nodeRequest(t, f.reserved, f.reserved)
			mutate(&request)
			expectDenied(t, f.handler.Validate(context.Background(), request), "")
		})
	}
}

func TestNodeLifecycleSharesTotalReadBudget(t *testing.T) {
	f := newFixture(t)
	reader := &budgetReader{Reader: f.client}
	handler, err := NewHandler(reader, Config{Namespace: f.state.Namespace, ConfigMapName: f.state.Name, RequestTimeout: 25 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	expectDenied(t, handler.Validate(context.Background(), nodeRequest(t, f.reserved, f.reserved)), "RequestDeadlineExceeded")
	if reader.gets != 2 {
		t.Fatal("Node lifecycle did not share one cancellable registry/Node read budget")
	}
}

func TestSchedulerBindingDoesNotRequireTargetNodeUID(t *testing.T) {
	f := newFixture(t)
	request := bindingRequest(t, f.platformPod, f.reserved.Name)
	var binding corev1.Binding
	if err := json.Unmarshal(request.Object.Raw, &binding); err != nil {
		t.Fatal(err)
	}
	if binding.Target.UID != "" {
		t.Fatal("fixture unexpectedly supplied a scheduler target UID")
	}
	expectAllowed(t, f.handler.Validate(context.Background(), request))
}
