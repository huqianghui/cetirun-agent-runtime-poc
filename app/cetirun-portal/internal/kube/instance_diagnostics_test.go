// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"context"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestGetInstanceEventsPinsEventsToAgentUID(t *testing.T) {
	instance := sampleInstance()
	instance.UID = "current-agent"
	matching := &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "current", Namespace: "cetirun-alice"}, InvolvedObject: corev1.ObjectReference{APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: instance.Name, UID: instance.UID}}
	old := &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "cetirun-alice"}, InvolvedObject: corev1.ObjectReference{APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: instance.Name, UID: "previous-agent"}}
	wrongKind := &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "cetirun-alice"}, InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: instance.Name, UID: instance.UID}}
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(matching, old, wrongKind).
		WithIndex(&corev1.Event{}, "involvedObject.uid", func(obj client.Object) []string { return []string{string(obj.(*corev1.Event).InvolvedObject.UID)} }).
		WithIndex(&corev1.Event{}, "involvedObject.kind", func(obj client.Object) []string { return []string{obj.(*corev1.Event).InvolvedObject.Kind} }).Build()

	events, err := NewClient(fc).GetInstanceEvents(context.Background(), instance)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Name != matching.Name {
		t.Fatalf("events = %#v, want only current Agent event", events)
	}
	instance.UID = ""
	if _, err := NewClient(fc).GetInstanceEvents(context.Background(), instance); err == nil {
		t.Fatal("diagnostics accepted an Agent without a UID")
	}
}

func TestGetInstanceDiagnosticNamespaceRequiresMatchingOwnerOrLegacyNamespace(t *testing.T) {
	instance := sampleInstance()
	instance.UID = "agent-uid"
	instance.Status.Namespace = "cetirun-alice"
	owner := metav1.OwnerReference{APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: instance.Name, UID: instance.UID}
	for _, test := range []struct {
		name        string
		namespace   string
		owners      []metav1.OwnerReference
		annotations map[string]string
		want        string
		wantErr     bool
	}{
		{"matching owner", "cetirun-alice", []metav1.OwnerReference{owner}, nil, "cetirun-alice", false},
		{"legacy namespace", "cetirun-alice", nil, nil, "cetirun-alice", false},
		{"matching annotation", "cetirun-alice", nil, map[string]string{diagnosticAgentUIDAnnotation: string(instance.UID)}, "cetirun-alice", false},
		{"replacement owner", "cetirun-alice", []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: instance.Name, UID: "replacement-uid"}}, nil, "", true},
		{"mixed owners", "cetirun-alice", []metav1.OwnerReference{owner, {APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: "other", UID: "other-uid"}}, nil, "", true},
		{"contradictory annotation", "cetirun-alice", []metav1.OwnerReference{owner}, map[string]string{diagnosticAgentUIDAnnotation: "replacement-uid"}, "", true},
		{"unexpected namespace", "other-namespace", nil, nil, "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := instance.DeepCopy()
			candidate.Status.Namespace = test.namespace
			objects := []client.Object{}
			if test.namespace == "cetirun-alice" {
				objects = append(objects, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: test.namespace, UID: "namespace-uid", OwnerReferences: test.owners, Annotations: test.annotations}})
			}
			got, err := NewClient(newFakeClient(objects...)).GetInstanceDiagnosticNamespace(context.Background(), candidate)
			gotName := ""
			if got != nil {
				gotName = got.Name
			}
			if (err != nil) != test.wantErr || gotName != test.want {
				t.Fatalf("namespace=%q err=%v, want %q err=%v", gotName, err, test.want, test.wantErr)
			}
		})
	}
}

func TestDiagnosticNamespaceRequiresUIDAndRetainsOwnershipMetadata(t *testing.T) {
	instance := sampleInstance()
	instance.UID = "agent-uid"
	instance.Status.Namespace = "cetirun-alice"
	legacy := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: instance.Status.Namespace}}
	diagnosticClient := NewClient(newFakeClient(legacy))
	if _, err := diagnosticClient.GetInstanceDiagnosticNamespace(context.Background(), instance); err == nil {
		t.Fatal("diagnostics accepted a namespace without a UID")
	}

	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: instance.Status.Namespace, UID: "namespace-uid",
		OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: instance.Name, UID: instance.UID}},
		Annotations:     map[string]string{diagnosticAgentUIDAnnotation: string(instance.UID)},
	}}
	diagnosticClient = NewClient(newFakeClient(namespace))
	snapshot, err := diagnosticClient.GetInstanceDiagnosticNamespace(context.Background(), instance)
	if err != nil {
		t.Fatal(err)
	}
	current := &corev1.Namespace{}
	if err := diagnosticClient.client.Get(context.Background(), client.ObjectKey{Name: instance.Status.Namespace}, current); err != nil {
		t.Fatal(err)
	}
	current.OwnerReferences = nil
	delete(current.Annotations, diagnosticAgentUIDAnnotation)
	if err := diagnosticClient.client.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if err := diagnosticClient.VerifyInstanceDiagnosticNamespace(context.Background(), instance, snapshot); err == nil {
		t.Fatal("diagnostics accepted removed namespace ownership metadata")
	}
}
