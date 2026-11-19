// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package controller

import (
	"reflect"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

func TestCetiAdminDiagnosticNamespaceAvailabilityAPIServer(t *testing.T) {
	h := newCetiAdminInstallationAPI(t, true)
	h.changeConfig(t, func(config *cetiadmin.Config) {
		config.PlatformReadNamespaces = append(config.PlatformReadNamespaces, "observability")
	})
	var diagnosticError error
	for range 32 {
		_, diagnosticError = h.installer.Reconcile(h.ctx, ctrl.Request{})
		if h.state(t).CanRun() {
			break
		}
		if diagnosticError != nil {
			t.Fatal("core installation failed:", diagnosticError)
		}
	}
	if !h.state(t).CanRun() || !apierrors.IsNotFound(diagnosticError) {
		t.Fatalf("missing optional namespace prevented installation or lost its error: %v", diagnosticError)
	}
	before := h.runtimeProjection(t)
	machine := h.machine(t)
	h.assertPermission(t, machine, "get", "", "pods", "", h.source.Namespace, "platform-pod", true)
	h.assertPermission(t, machine, "get", "", "secrets", "", h.source.Namespace, "provider", false)
	h.assertPermission(t, machine, "get", "", "pods", "", "observability", "diagnostic-pod", false)
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "observability"}}
	h.create(t, namespace)
	h.reconcile(t)
	h.assertPermission(t, machine, "get", "", "pods", "", namespace.Name, "diagnostic-pod", true)
	h.assertPermission(t, machine, "get", "", "secrets", "", namespace.Name, "private", false)
	if !reflect.DeepEqual(before, h.runtimeProjection(t)) {
		t.Fatal("diagnostic availability changed the Admin runtime binding")
	}
	if err := h.admin.Delete(h.ctx, namespace); err != nil {
		t.Fatal(err)
	}
	if _, err := h.installer.Reconcile(h.ctx, ctrl.Request{}); err == nil {
		t.Fatal("terminating diagnostic namespace was not reported")
	}
	if !h.state(t).CanRun() || !reflect.DeepEqual(before, h.runtimeProjection(t)) {
		t.Fatal("diagnostic namespace removal stopped or rebound the Admin")
	}
	h.assertPermission(t, machine, "get", "", "pods", "", namespace.Name, "diagnostic-pod", false)
	h.assertPermission(t, machine, "get", "", "pods", "", h.source.Namespace, "platform-pod", true)
}
