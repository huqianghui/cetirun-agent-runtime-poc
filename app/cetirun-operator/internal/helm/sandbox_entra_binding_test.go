// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"strings"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
)

func entraBindingObjects(t *testing.T, bundle *w.Bundle) (*corev1.Namespace, *corev1.ServiceAccount, w.ObjectIdentity) {
	t.Helper()
	input := bundle.Identity()
	namespace := testfixture.New(input.AgentType).Namespace
	for _, object := range input.Controls.Objects {
		if object.GetKind() != "ServiceAccount" {
			continue
		}
		account := &corev1.ServiceAccount{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, account); err != nil {
			t.Fatal(err)
		}
		account.UID, account.ResourceVersion = "recorded-service-account", "1"
		return namespace, account, w.ObjectIdentity{Name: account.Name, UID: account.UID}
	}
	t.Fatal("compiler fixture has no identity ServiceAccount")
	return nil, nil, w.ObjectIdentity{}
}

func TestEntraRuntimeBindingRejectsStaleOrBorrowedIdentity(t *testing.T) {
	bundle := entraRenderBundle(t, "hermes", true)
	namespace, account, pin := entraBindingObjects(t, bundle)
	input := bundle.Identity()
	expected := w.ReconcileExpectation{
		Version:   w.ReconcileExpectationVersion,
		Agent:     w.ObjectIdentity{Name: input.AgentName, UID: input.Resolution.Binding.AgentUID},
		Namespace: input.Namespace, Binding: input.Resolution.Binding,
		Revision: input.Controls.Revision, Controls: input.Controls.Objects,
	}
	if err := bundle.ValidateIdentityBinding(namespace, account, pin); err != nil {
		t.Fatal(err)
	}
	if err := expected.ValidateIdentityBinding(namespace, account, pin); err != nil {
		t.Fatal("protected expectation rejected matching identity:", err)
	}
	for name, mutate := range map[string]func(*corev1.Namespace, *corev1.ServiceAccount, *w.ObjectIdentity){
		"missing pin":         func(_ *corev1.Namespace, _ *corev1.ServiceAccount, p *w.ObjectIdentity) { p.UID = "" },
		"wrong pin":           func(_ *corev1.Namespace, _ *corev1.ServiceAccount, p *w.ObjectIdentity) { p.Name = "borrowed" },
		"namespace recreated": func(n *corev1.Namespace, _ *corev1.ServiceAccount, _ *w.ObjectIdentity) { n.UID = "replacement" },
		"namespace moved":     func(n *corev1.Namespace, _ *corev1.ServiceAccount, _ *w.ObjectIdentity) { n.Name = "other" },
		"namespace deleted": func(n *corev1.Namespace, _ *corev1.ServiceAccount, _ *w.ObjectIdentity) {
			now := metav1.Now()
			n.DeletionTimestamp = &now
		},
		"namespace unowned": func(n *corev1.Namespace, _ *corev1.ServiceAccount, _ *w.ObjectIdentity) { n.OwnerReferences = nil },
		"wrong Agent": func(n *corev1.Namespace, _ *corev1.ServiceAccount, _ *w.ObjectIdentity) {
			n.Annotations[w.AgentUIDAnnotation] = "other-agent"
		},
		"unprotected namespace": func(n *corev1.Namespace, _ *corev1.ServiceAccount, _ *w.ObjectIdentity) {
			delete(n.Labels, w.ProtectedNamespaceLabel)
		},
		"account recreated": func(_ *corev1.Namespace, a *corev1.ServiceAccount, _ *w.ObjectIdentity) {
			a.UID = "replacement-account"
		},
		"account moved": func(_ *corev1.Namespace, a *corev1.ServiceAccount, _ *w.ObjectIdentity) { a.Namespace = "other" },
		"account deleted": func(_ *corev1.Namespace, a *corev1.ServiceAccount, _ *w.ObjectIdentity) {
			now := metav1.Now()
			a.DeletionTimestamp = &now
		},
		"account unowned": func(_ *corev1.Namespace, a *corev1.ServiceAccount, _ *w.ObjectIdentity) { a.OwnerReferences = nil },
		"account borrowed": func(_ *corev1.Namespace, a *corev1.ServiceAccount, _ *w.ObjectIdentity) {
			a.OwnerReferences[0].UID = "other-agent"
		},
		"unpersisted read": func(_ *corev1.Namespace, a *corev1.ServiceAccount, _ *w.ObjectIdentity) { a.ResourceVersion = "" },
		"stale approval": func(_ *corev1.Namespace, a *corev1.ServiceAccount, _ *w.ObjectIdentity) {
			a.Annotations[sandboxpolicy.RevisionAnnotation] = "stale"
		},
		"client drift": func(_ *corev1.Namespace, a *corev1.ServiceAccount, _ *w.ObjectIdentity) {
			a.Annotations["azure.workload.identity/client-id"] = "borrowed"
		},
		"tenant drift": func(_ *corev1.Namespace, a *corev1.ServiceAccount, _ *w.ObjectIdentity) {
			a.Annotations["azure.workload.identity/tenant-id"] = "borrowed"
		},
		"extra Azure setting": func(_ *corev1.Namespace, a *corev1.ServiceAccount, _ *w.ObjectIdentity) {
			a.Annotations["azure.workload.identity/extra"] = ""
		},
		"automount": func(_ *corev1.Namespace, a *corev1.ServiceAccount, _ *w.ObjectIdentity) {
			a.AutomountServiceAccountToken = ptr.To(true)
		},
		"secret inheritance": func(_ *corev1.Namespace, a *corev1.ServiceAccount, _ *w.ObjectIdentity) {
			a.Secrets = []corev1.ObjectReference{{Name: "unrequested"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			ns, sa, currentPin := namespace.DeepCopy(), account.DeepCopy(), pin
			mutate(ns, sa, &currentPin)
			if err := bundle.ValidateIdentityBinding(ns, sa, currentPin); err == nil {
				t.Fatal("stale or borrowed identity readback accepted")
			}
			if err := expected.ValidateIdentityBinding(ns, sa, currentPin); err == nil {
				t.Fatal("protected expectation accepted stale or borrowed identity readback")
			}
		})
	}
	if err := bundle.ValidateIdentityBinding(nil, account, pin); err == nil {
		t.Fatal("missing namespace accepted")
	}
	if err := bundle.ValidateIdentityBinding(namespace, nil, pin); err == nil {
		t.Fatal("missing ServiceAccount accepted")
	}
	currentApproval := entraRenderBundleAtRevision(t, "hermes", true, "synthetic-approval-v2")
	if err := currentApproval.ValidateIdentityBinding(namespace, account, pin); err == nil {
		t.Fatal("new approval reused old ServiceAccount readback")
	}
	disabled := entraRenderBundle(t, "hermes", false)
	if err := disabled.ValidateIdentityBinding(namespace, account, pin); err == nil {
		t.Fatal("disabled Entra reused its prior binding")
	}
	changed := expected
	changed.Binding.AgentPolicyUID = "replacement-policy"
	if err := changed.ValidateIdentityBinding(namespace, account, pin); err == nil {
		t.Fatal("replaced policy reused prior identity metadata")
	}
	changed = expected
	changed.Revision = strings.Repeat("0", 64)
	if err := changed.ValidateIdentityBinding(namespace, account, pin); err == nil {
		t.Fatal("changed expectation reused stale identity controls")
	}
	namespace.ResourceVersion, account.ResourceVersion = "2", "2"
	namespace.Annotations["example.com/note"], account.Annotations["example.com/note"] = "metadata", "metadata"
	if err := bundle.ValidateIdentityBinding(namespace, account, pin); err != nil {
		t.Fatal("unrelated metadata-only refresh invalidated the approved UID binding:", err)
	}
}
