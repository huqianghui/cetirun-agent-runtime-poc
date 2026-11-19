// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller_test

import (
	"context"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/controller"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func containerConfigurationSnapshot() (*api.Sandbox, controller.SandboxConfigurationSnapshot, []client.Object) {
	box, snapshot, objects := nativeConfigurationSnapshot()
	box.Spec.Type = api.SandboxContainer
	registration := snapshot.Registrations[api.SandboxKata]
	registration.Capabilities.RuntimeClassName = "example-container"
	registration.Capabilities.PerContainerResourceLimits = true
	snapshot.Registrations = map[api.SandboxType]sandbox.Registration{api.SandboxContainer: registration}
	snapshot.Platforms[api.SandboxContainer] = snapshot.Platforms[api.SandboxKata]
	delete(snapshot.Platforms, api.SandboxKata)
	snapshot.RuntimeClasses = map[string]controller.SandboxRuntimePrerequisite{
		"example-container": {UID: "container-runtime-uid", Handler: "runc"},
	}
	objects[0] = &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "example-container", UID: "container-runtime-uid", ResourceVersion: "1"}, Handler: "runc"}
	return box, snapshot, objects
}

func TestContainerSandboxConfigurationPrerequisites(t *testing.T) {
	for _, fault := range []string{"ready", "native disabled", "unavailable", "unregistered", "handler drift", "missing class", "no container limits"} {
		t.Run(fault, func(t *testing.T) {
			box, snapshot, objects := containerConfigurationSnapshot()
			registration := snapshot.Registrations[api.SandboxContainer]
			switch fault {
			case "unavailable":
				registration.Available = false
			case "handler drift":
				objects[0].(*nodev1.RuntimeClass).Handler = "unverified-handler"
			case "missing class":
				objects = objects[1:]
			case "no container limits":
				registration.Capabilities.PerContainerResourceLimits = false
			}
			snapshot.Registrations[api.SandboxContainer] = registration
			if fault == "unregistered" {
				delete(snapshot.Registrations, api.SandboxContainer)
			}
			kube := fake.NewClientBuilder().WithScheme(configurationScheme(t)).WithStatusSubresource(box).
				WithObjects(append(objects, box)...).Build()
			r := &controller.SandboxReconciler{
				Client: kube, APIReader: kube, NativeExecution: fault != "native disabled",
				Now: func() time.Time { return snapshot.ObservedAt },
				Source: controller.SandboxConfigurationSourceFunc(func(context.Context) (controller.SandboxConfigurationSnapshot, error) {
					return snapshot, nil
				}),
			}
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(box)}); err != nil {
				t.Fatal(err)
			}
			if err := kube.Get(context.Background(), client.ObjectKeyFromObject(box), box); err != nil {
				t.Fatal(err)
			}
			if len(box.Status.Conditions) != 1 || (box.Status.Conditions[0].Status == metav1.ConditionTrue) != (fault == "ready") {
				t.Fatal("container readiness did not verify the runtime prerequisites:", box.Status)
			}
		})
	}
}

func TestContainerSandboxKubernetesRegistration(t *testing.T) {
	for _, fault := range []string{"registered", "unknown handler pin", "empty handler pin", "missing RuntimeClass pin", "unknown field", "overstated isolation", "gvisor native"} {
		t.Run(fault, func(t *testing.T) {
			_, snapshot, objects := containerConfigurationSnapshot()
			document := controller.SandboxPlatformConfiguration{
				Version: "v1", Registrations: snapshot.Registrations, Platforms: snapshot.Platforms,
				RuntimeClasses: snapshot.RuntimeClasses, StorageClasses: snapshot.StorageClasses,
			}
			pin := document.RuntimeClasses["example-container"]
			switch fault {
			case "unknown handler pin":
				pin.Handler = "unverified-handler"
				document.RuntimeClasses["example-container"] = pin
			case "empty handler pin":
				pin.Handler = ""
				document.RuntimeClasses["example-container"] = pin
			case "missing RuntimeClass pin":
				delete(document.RuntimeClasses, "example-container")
			case "overstated isolation":
				registration := document.Registrations[api.SandboxContainer]
				registration.Capabilities.RestrictiveFailure = true
				document.Registrations[api.SandboxContainer] = registration
			case "gvisor native":
				document.Registrations[api.SandboxGVisor] = document.Registrations[api.SandboxContainer]
				document.Platforms[api.SandboxGVisor] = document.Platforms[api.SandboxContainer]
				delete(document.Registrations, api.SandboxContainer)
				delete(document.Platforms, api.SandboxContainer)
			}
			raw := kubernetesPlatformJSON(t, document)
			if fault == "unknown field" {
				raw = `{"ignored":true,` + raw[1:]
			}
			cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
				Name: "sandbox-platform", Namespace: "platform-system", UID: "container-platform-uid", ResourceVersion: "1",
			}, Data: map[string]string{controller.SandboxPlatformConfigurationKey: raw}}
			source, reader := kubernetesPlatformSource(t, cm, objects)
			if fault != "registered" {
				requireKubernetesPlatformFailure(t, source)
				return
			}
			actual, err := source.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			registration := actual.Registrations[api.SandboxContainer]
			if !registration.Available || registration.Capabilities.RuntimeClassName != "example-container" ||
				!registration.Capabilities.PerContainerResourceLimits || len(registration.Capabilities.Revision) != 64 ||
				actual.RuntimeClasses["example-container"].Handler != "runc" || reader.lists != 0 {
				t.Fatal("container registration invented or lost its trusted identity")
			}
		})
	}
}
