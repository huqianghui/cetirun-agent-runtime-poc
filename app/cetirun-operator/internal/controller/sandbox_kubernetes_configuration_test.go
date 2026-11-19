// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/controller"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
)

const emptyKubernetesPlatformJSON = `{"version":"v1","registrations":{},"platforms":{},"runtimeClasses":{},"storageClasses":{}}`

func kubernetesPlatformJSON(t *testing.T, document controller.SandboxPlatformConfiguration) string {
	t.Helper()
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func kubernetesPlatformFixture(t *testing.T) (controller.SandboxPlatformConfiguration, *corev1.ConfigMap, []client.Object) {
	t.Helper()
	_, _, objects := sandboxConfigurationFixture()
	_, snapshot, role := configurationGrantFixture()
	for i, object := range objects {
		object.SetResourceVersion(fmt.Sprint(i + 11))
	}
	document := controller.SandboxPlatformConfiguration{
		Version: "v1", Registrations: snapshot.Registrations, Platforms: snapshot.Platforms,
		RuntimeClasses: snapshot.RuntimeClasses, StorageClasses: snapshot.StorageClasses,
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "platform-system", Name: "sandbox-platform", UID: "example-configuration-uid", ResourceVersion: "3"},
		Data:       map[string]string{controller.SandboxPlatformConfigurationKey: kubernetesPlatformJSON(t, document)},
	}
	return document, cm, append(objects, role)
}

type kubernetesPlatformRead struct {
	kind string
	key  client.ObjectKey
}

type kubernetesPlatformReader struct {
	client.Reader
	calls map[kubernetesPlatformRead]int
	lists int
	after func(client.Object, int) error
}

func (r *kubernetesPlatformReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	kind := object.GetObjectKind().GroupVersionKind().Kind
	switch object.(type) {
	case *corev1.ConfigMap:
		kind = "ConfigMap"
	case *nodev1.RuntimeClass:
		kind = "RuntimeClass"
	case *storagev1.StorageClass:
		kind = "StorageClass"
	case *rbacv1.ClusterRole:
		kind = "ClusterRole"
	}
	read := kubernetesPlatformRead{kind, key}
	r.calls[read]++
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.Reader.Get(ctx, key, object, opts...); err != nil {
		return err
	}
	if r.after != nil {
		return r.after(object, r.calls[read])
	}
	return nil
}

func (r *kubernetesPlatformReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	r.lists++
	return errors.New("unexpected platform configuration discovery")
}

func kubernetesPlatformSource(t *testing.T, cm *corev1.ConfigMap, objects []client.Object) (*controller.KubernetesSandboxConfigurationSource, *kubernetesPlatformReader) {
	t.Helper()
	scheme := configurationScheme(t)
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	direct := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append([]client.Object{cm}, objects...)...).Build()
	reader := &kubernetesPlatformReader{Reader: direct, calls: map[kubernetesPlatformRead]int{}}
	source := &controller.KubernetesSandboxConfigurationSource{
		APIReader: reader, Namespace: cm.Namespace, Name: cm.Name,
		Now: func() time.Time { return time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC) },
	}
	return source, reader
}

func requireKubernetesPlatformFailure(t *testing.T, source *controller.KubernetesSandboxConfigurationSource) error {
	t.Helper()
	snapshot, err := source.Snapshot(context.Background())
	if err == nil || !reflect.DeepEqual(snapshot, controller.SandboxConfigurationSnapshot{}) {
		t.Fatalf("expected an error and no partial snapshot; got %+v, %v", snapshot, err)
	}
	return err
}

func TestKubernetesSandboxConfigurationSourceSnapshot(t *testing.T) {
	document, cm, objects := kubernetesPlatformFixture(t)
	source, reader := kubernetesPlatformSource(t, cm, objects)
	begin := source.Now()
	clock := begin
	source.Now = func() time.Time { return clock }
	reader.after = func(client.Object, int) error {
		clock = clock.Add(time.Second)
		return nil
	}
	snapshot, err := source.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	registration := snapshot.Registrations[api.SandboxKata]
	if snapshot.ObservedAt != begin || len(registration.Capabilities.Revision) != 64 ||
		registration.Capabilities.Revision == document.Registrations[api.SandboxKata].Capabilities.Revision {
		t.Fatalf("snapshot must bind its read beginning and effective revision: %+v", snapshot)
	}
	_, expected, _ := configurationGrantFixture()
	expectedRegistration := expected.Registrations[api.SandboxKata]
	expectedRegistration.Capabilities.Revision = registration.Capabilities.Revision
	expected.Registrations[api.SandboxKata] = expectedRegistration
	expected.ObservedAt = begin
	if !reflect.DeepEqual(expected, snapshot) {
		t.Fatal("capture invented or changed declared support")
	}
	wantReads := map[kubernetesPlatformRead]int{
		{"ConfigMap", client.ObjectKeyFromObject(cm)}:            2,
		{"RuntimeClass", client.ObjectKey{Name: "example-kata"}}: 2,
		{"StorageClass", client.ObjectKey{Name: "example-home"}}: 2,
	}
	if !reflect.DeepEqual(reader.calls, wantReads) || reader.lists != 0 {
		t.Fatalf("capture did not use only explicit authoritative reads and rechecks: %+v, lists=%d", reader.calls, reader.lists)
	}
}

func TestKubernetesNativePlatformDeclaration(t *testing.T) {
	for _, scenario := range []string{"bounded", "unknown profile", "observer claim", "missing baseline", "wrong runtime", "qualified adapter"} {
		t.Run(scenario, func(t *testing.T) {
			_, snapshot, objects := nativeConfigurationSnapshot()
			document := controller.SandboxPlatformConfiguration{
				Version: "v1", Registrations: snapshot.Registrations, Platforms: snapshot.Platforms,
				RuntimeClasses: snapshot.RuntimeClasses, StorageClasses: snapshot.StorageClasses,
			}
			registration := document.Registrations[api.SandboxKata]
			platform := document.Platforms[api.SandboxKata]
			switch scenario {
			case "unknown profile":
				registration.Capabilities.Profile = "unknown"
			case "observer claim":
				registration.Capabilities.EnforcementObservation = true
			case "missing baseline":
				registration.Capabilities.NodeTraffic = false
			case "qualified adapter":
				platform.Adapter.Qualified = true
			}
			document.Registrations[api.SandboxKata], document.Platforms[api.SandboxKata] = registration, platform
			if scenario == "wrong runtime" {
				document.Registrations[api.SandboxGVisor], document.Platforms[api.SandboxGVisor] = registration, platform
				delete(document.Registrations, api.SandboxKata)
				delete(document.Platforms, api.SandboxKata)
			}
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Namespace: "platform-system", Name: "sandbox-platform", UID: "native-platform-uid", ResourceVersion: "1"},
				Data:       map[string]string{controller.SandboxPlatformConfigurationKey: kubernetesPlatformJSON(t, document)},
			}
			source, _ := kubernetesPlatformSource(t, cm, objects)
			if scenario != "bounded" {
				if err := requireKubernetesPlatformFailure(t, source); !errors.Is(err, sandboxpolicy.ErrUnsupportedControl) {
					t.Fatalf("invalid profile lost the unsupported category: %v", err)
				}
				return
			}
			actual, err := source.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			capabilities := actual.Registrations[api.SandboxKata].Capabilities
			adapter := actual.Platforms[api.SandboxKata].Adapter
			if capabilities.Profile != sandbox.ProfileNativeSingleStart ||
				capabilities.EnforcementObservation || capabilities.RestrictiveFailure ||
				adapter.Name != sandboxpolicy.Cilium || adapter.Qualified {
				t.Fatal("Kubernetes source replaced bounded declarations with stronger or Azure guarantees")
			}
		})
	}
}

func TestKubernetesSandboxConfigurationSourceScopesRoleFailuresToRequestedGrants(t *testing.T) {
	failure := errors.New("synthetic role read failure")
	for _, change := range []string{"missing", "UID", "resourceVersion", "rules", "terminating", "read failure", "duplicate approval"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			document, cm, objects := kubernetesPlatformFixture(t)
			policy, _, _ := configurationGrantFixture()
			box, _, _ := sandboxConfigurationFixture()
			normal := &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{
				Name: "example-normal", UID: "example-normal-uid", Generation: 1,
			}, Spec: api.AgentPolicySpec{Harnesses: nativePolicyHarnesses()}}
			other := policy.DeepCopy()
			other.Name, other.UID = "example-other-policy", "example-other-policy-uid"
			role := objects[2].(*rbacv1.ClusterRole)
			otherRole := role.DeepCopy()
			otherRole.Name, otherRole.UID = "example-other-role", "example-other-role-uid"
			other.Spec.KubernetesAccess.RoleBindings[0].ClusterRoleName = otherRole.Name
			platform := document.Platforms[api.SandboxKata]
			grant := platform.Grants[0]
			grant.PolicyUID = other.UID
			platform.Grants = append(platform.Grants, grant)
			grant.RoleRef.Name, grant.RoleUID = otherRole.Name, otherRole.UID
			platform.Grants = append(platform.Grants, grant)
			if change == "duplicate approval" {
				platform.Grants = append(platform.Grants, platform.Grants[0])
			}
			document.Platforms[api.SandboxKata] = platform
			cm.Data[controller.SandboxPlatformConfigurationKey] = kubernetesPlatformJSON(t, document)
			if change == "missing" {
				objects = objects[:2]
			}
			source, reader := kubernetesPlatformSource(t, cm, append(objects, otherRole))
			reader.after = func(object client.Object, _ int) error {
				current, ok := object.(*rbacv1.ClusterRole)
				if !ok || current.Name != role.Name {
					return nil
				}
				switch change {
				case "UID":
					current.UID = "replacement-role"
				case "resourceVersion":
					current.ResourceVersion = "8"
				case "rules":
					current.Rules[0].Resources = []string{"secrets"}
				case "terminating":
					stamp := metav1.Now()
					current.DeletionTimestamp = &stamp
				case "read failure":
					return failure
				}
				return nil
			}
			cached := fake.NewClientBuilder().WithScheme(configurationScheme(t)).
				WithStatusSubresource(&api.Sandbox{}, &api.AgentPolicy{}).
				WithObjects(box, normal, other, policy, role.DeepCopy()).Build()
			reconciler := &controller.AgentPolicyReconciler{
				Client: cached, APIReader: reader, Source: source, Now: source.Now,
			}
			for _, requested := range []*api.AgentPolicy{normal, other, policy} {
				result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(requested)})
				status, reason := metav1.ConditionTrue, "PolicyAccepted"
				if requested == policy {
					status, reason = metav1.ConditionFalse, "UnapprovedGrant"
				}
				if requested == policy && change == "read failure" {
					status, reason = metav1.ConditionUnknown, "PrerequisitesUnavailable"
					if !errors.Is(err, failure) || !errors.Is(err, sandboxpolicy.ErrUnavailable) || result.RequeueAfter != 0 {
						t.Fatalf("requested role failure lost its cause or backoff: %+v, %v", result, err)
					}
				} else if err != nil || result.RequeueAfter <= 0 {
					t.Fatalf("policy %s was affected by an unrelated role or lost its refresh: %+v, %v", requested.Name, result, err)
				}
				current := &api.AgentPolicy{}
				if err := cached.Get(ctx, client.ObjectKeyFromObject(requested), current); err != nil {
					t.Fatal(err)
				}
				condition := apimeta.FindStatusCondition(current.Status.Conditions, api.ConditionAgentPolicyAccepted)
				if condition == nil || condition.Status != status || condition.Reason != reason {
					t.Fatalf("policy %s has the wrong scoped assessment: %+v", requested.Name, condition)
				}
			}
			sandboxReconciler := &controller.SandboxReconciler{
				Client: cached, APIReader: reader, Source: source, Now: source.Now,
			}
			if _, err := sandboxReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(box)}); err != nil {
				t.Fatal("role failure prevented Sandbox prerequisite assessment:", err)
			}
			if err := cached.Get(ctx, client.ObjectKeyFromObject(box), box); err != nil {
				t.Fatal(err)
			}
			if !apimeta.IsStatusConditionTrue(box.Status.Conditions, api.ConditionReady) {
				t.Fatal("role failure changed Sandbox prerequisite readiness")
			}
			wantRoleReads := 1
			if change == "duplicate approval" {
				wantRoleReads = 0
			}
			if reader.calls[kubernetesPlatformRead{"ClusterRole", client.ObjectKeyFromObject(role)}] != wantRoleReads ||
				reader.calls[kubernetesPlatformRead{"ClusterRole", client.ObjectKeyFromObject(otherRole)}] != 1 {
				t.Fatal("authoritative role reads were not limited to each policy's requested grants:", reader.calls)
			}
		})
	}
}

func TestKubernetesSandboxConfigurationSourceRequiresExplicitSelection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*controller.KubernetesSandboxConfigurationSource)
	}{
		{"reader", func(s *controller.KubernetesSandboxConfigurationSource) { s.APIReader = nil }},
		{"namespace", func(s *controller.KubernetesSandboxConfigurationSource) { s.Namespace = "" }},
		{"name", func(s *controller.KubernetesSandboxConfigurationSource) { s.Name = "" }},
		{"invalid namespace", func(s *controller.KubernetesSandboxConfigurationSource) { s.Namespace = "tenant/namespace" }},
		{"invalid name", func(s *controller.KubernetesSandboxConfigurationSource) { s.Name = "../platform" }},
		{"zero clock", func(s *controller.KubernetesSandboxConfigurationSource) {
			s.Now = func() time.Time { return time.Time{} }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, cm, objects := kubernetesPlatformFixture(t)
			source, reader := kubernetesPlatformSource(t, cm, objects)
			tc.mutate(source)
			requireKubernetesPlatformFailure(t, source)
			if len(reader.calls) != 0 || reader.lists != 0 {
				t.Fatal("invalid selection attempted discovery")
			}
		})
	}
	requireKubernetesPlatformFailure(t, nil)
	_, cm, objects := kubernetesPlatformFixture(t)
	source, reader := kubernetesPlatformSource(t, cm, objects)
	source.Namespace = "another-platform"
	if err := requireKubernetesPlatformFailure(t, source); !apierrors.IsNotFound(err) {
		t.Fatal("missing explicitly selected ConfigMap lost its cause", err)
	}
	if len(reader.calls) != 1 || reader.lists != 0 {
		t.Fatal("missing configuration fell back to another namespace or discovery")
	}
}

func TestKubernetesSandboxConfigurationSourceRejectsUntrustedConfigMaps(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*corev1.ConfigMap)
	}{
		{"unpersisted UID", func(cm *corev1.ConfigMap) { cm.UID = "" }},
		{"unpersisted version", func(cm *corev1.ConfigMap) { cm.ResourceVersion = "" }},
		{"terminating", func(cm *corev1.ConfigMap) { stamp := metav1.Now(); cm.DeletionTimestamp = &stamp }},
		{"wrong namespace", func(cm *corev1.ConfigMap) { cm.Namespace = "tenant-space" }},
		{"wrong name", func(cm *corev1.ConfigMap) { cm.Name = "tenant-configuration" }},
		{"Agent owner", func(cm *corev1.ConfigMap) {
			cm.OwnerReferences = []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: "example-agent", UID: "example-agent-uid"}}
		}},
		{"Agent label", func(cm *corev1.ConfigMap) {
			cm.Labels = map[string]string{sandboxpolicy.AgentUIDLabel: "example-agent-uid"}
		}},
		{"Agent annotation", func(cm *corev1.ConfigMap) {
			cm.Annotations = map[string]string{sandboxworkload.AgentUIDAnnotation: "example-agent-uid"}
		}},
		{"empty Agent marker", func(cm *corev1.ConfigMap) {
			cm.Annotations = map[string]string{sandboxworkload.AgentNameAnnotation: ""}
		}},
		{"workload marker", func(cm *corev1.ConfigMap) { cm.Labels = map[string]string{sandboxworkload.KindAnnotation: "agent"} }},
		{"missing key", func(cm *corev1.ConfigMap) { cm.Data = map[string]string{"another.json": emptyKubernetesPlatformJSON} }},
		{"binary key", func(cm *corev1.ConfigMap) {
			cm.BinaryData = map[string][]byte{controller.SandboxPlatformConfigurationKey: []byte(emptyKubernetesPlatformJSON)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, cm, objects := kubernetesPlatformFixture(t)
			source, reader := kubernetesPlatformSource(t, cm, objects)
			reader.after = func(object client.Object, _ int) error {
				if cm, ok := object.(*corev1.ConfigMap); ok {
					tc.mutate(cm)
				}
				return nil
			}
			requireKubernetesPlatformFailure(t, source)
			if len(reader.calls) != 1 {
				t.Fatal("untrusted configuration was used to resolve dependencies")
			}
		})
	}
}

func TestKubernetesSandboxConfigurationSourceDocumentFieldNames(t *testing.T) {
	document, _, _ := kubernetesPlatformFixture(t)
	assertFields := func(raw json.RawMessage, names ...string) map[string]json.RawMessage {
		t.Helper()
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if len(fields) != len(names) {
			t.Fatalf("document has %d fields, want %d", len(fields), len(names))
		}
		for _, name := range names {
			if _, present := fields[name]; !present {
				t.Fatalf("document is missing case-sensitive field %s", name)
			}
		}
		return fields
	}
	fields := assertFields([]byte(kubernetesPlatformJSON(t, document)),
		"version", "registrations", "platforms", "runtimeClasses", "storageClasses")
	for _, tc := range []struct {
		group, key string
		fields     []string
	}{
		{"registrations", "kata", []string{"Capabilities", "Available"}},
		{"runtimeClasses", "example-kata", []string{"UID", "Handler"}},
		{"storageClasses", "example-home", []string{"UID", "Provisioner"}},
	} {
		var records map[string]json.RawMessage
		if err := json.Unmarshal(fields[tc.group], &records); err != nil {
			t.Fatal(err)
		}
		assertFields(records[tc.key], tc.fields...)
	}
}

func TestKubernetesSandboxConfigurationSourceStrictJSON(t *testing.T) {
	document, _, _ := kubernetesPlatformFixture(t)
	valid := kubernetesPlatformJSON(t, document)
	for _, tc := range []struct{ name, raw string }{
		{"empty", ""},
		{"whitespace", " \n\t"},
		{"malformed", `{"version":"v1",`},
		{"trailing document", emptyKubernetesPlatformJSON + emptyKubernetesPlatformJSON},
		{"root array", `[]`},
		{"root null", `null`},
		{"unknown version", strings.Replace(emptyKubernetesPlatformJSON, `"v1"`, `"v2"`, 1)},
		{"missing version", strings.Replace(emptyKubernetesPlatformJSON, `"version":"v1",`, "", 1)},
		{"missing map", strings.Replace(emptyKubernetesPlatformJSON, `,"storageClasses":{}`, "", 1)},
		{"null map", strings.Replace(emptyKubernetesPlatformJSON, `"registrations":{}`, `"registrations":null`, 1)},
		{"null scalar", strings.Replace(valid, `"Available":true`, `"Available":null`, 1)},
		{"null pin", strings.Replace(emptyKubernetesPlatformJSON, `"runtimeClasses":{}`, `"runtimeClasses":{"example":null}`, 1)},
		{"wrong scalar type", strings.Replace(valid, `"Available":true`, `"Available":"example-payload-marker"`, 1)},
		{"wrong map type", strings.Replace(emptyKubernetesPlatformJSON, `"registrations":{}`, `"registrations":[]`, 1)},
		{"unknown field", strings.Replace(emptyKubernetesPlatformJSON, `"version":`, `"example-payload-marker":"example-data","version":`, 1)},
		{"unknown nested field", strings.Replace(valid, `"runtimeClassName":`, `"example-payload-marker":`, 1)},
		{"top level case", strings.Replace(emptyKubernetesPlatformJSON, `"version":`, `"Version":`, 1)},
		{"registration case", strings.Replace(valid, `"Capabilities":`, `"capabilities":`, 1)},
		{"availability case", strings.Replace(valid, `"Available":`, `"available":`, 1)},
		{"UID case", strings.Replace(valid, `"UID":`, `"uid":`, 1)},
		{"handler case", strings.Replace(valid, `"Handler":`, `"handler":`, 1)},
		{"provisioner case", strings.Replace(valid, `"Provisioner":`, `"provisioner":`, 1)},
		{"duplicate field", strings.Replace(emptyKubernetesPlatformJSON, `"version":`, `"version":"v1","version":`, 1)},
		{"duplicate nested field", strings.Replace(valid, `"Handler":"kata"`, `"Handler":"kata","Handler":"kata"`, 1)},
		{"duplicate map key", strings.Replace(valid, `"example-kata":{`, `"example-kata":{},"example-kata":{`, 1)},
		{"invalid UTF-8", strings.Replace(emptyKubernetesPlatformJSON, "v1", string([]byte{0xff}), 1)},
		{"excessive nesting", strings.Repeat("[", 13) + "0" + strings.Repeat("]", 13)},
		{"excessive tokens", "[" + strings.Repeat("0,", 16*1024) + "0]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, cm, objects := kubernetesPlatformFixture(t)
			cm.Data[controller.SandboxPlatformConfigurationKey] = tc.raw
			source, reader := kubernetesPlatformSource(t, cm, objects)
			err := requireKubernetesPlatformFailure(t, source)
			if strings.Contains(err.Error(), "example-payload-marker") || strings.Contains(err.Error(), "example-data") {
				t.Fatal("configuration diagnostics exposed payload data", err)
			}
			if len(reader.calls) != 1 {
				t.Fatal("malformed document triggered dependency reads")
			}
		})
	}
}

func TestKubernetesSandboxConfigurationSourceRejectsInconsistentRecords(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*controller.SandboxPlatformConfiguration)
	}{
		{"unknown runtime type", func(d *controller.SandboxPlatformConfiguration) {
			d.Registrations["unknown"] = d.Registrations[api.SandboxKata]
		}},
		{"unregistered platform", func(d *controller.SandboxPlatformConfiguration) {
			d.Platforms[api.SandboxGVisor] = d.Platforms[api.SandboxKata]
		}},
		{"missing platform", func(d *controller.SandboxPlatformConfiguration) { delete(d.Platforms, api.SandboxKata) }},
		{"missing runtime pin", func(d *controller.SandboxPlatformConfiguration) { delete(d.RuntimeClasses, "example-kata") }},
		{"missing owner revision", func(d *controller.SandboxPlatformConfiguration) {
			r := d.Registrations[api.SandboxKata]
			r.Capabilities.Revision = ""
			d.Registrations[api.SandboxKata] = r
		}},
		{"contradictory resource limits", func(d *controller.SandboxPlatformConfiguration) {
			r := d.Registrations[api.SandboxKata]
			r.Capabilities.RequirePerContainerResourceLimits = true
			d.Registrations[api.SandboxKata] = r
		}},
		{"unsupported home mode", func(d *controller.SandboxPlatformConfiguration) {
			d.Registrations[api.SandboxKata].Capabilities.HomeAccessModes[0] = corev1.ReadWriteMany
		}},
		{"unsupported IP family", func(d *controller.SandboxPlatformConfiguration) {
			d.Registrations[api.SandboxKata].Capabilities.IPFamilies[0] = "unknown"
		}},
		{"unsupported protocol", func(d *controller.SandboxPlatformConfiguration) {
			d.Registrations[api.SandboxKata].Capabilities.Protocols[0] = "unknown"
		}},
		{"invalid runtime key", func(d *controller.SandboxPlatformConfiguration) {
			d.RuntimeClasses["../runtime"] = d.RuntimeClasses["example-kata"]
		}},
		{"missing runtime UID", func(d *controller.SandboxPlatformConfiguration) {
			d.RuntimeClasses["example-kata"] = controller.SandboxRuntimePrerequisite{Handler: "kata"}
		}},
		{"missing handler", func(d *controller.SandboxPlatformConfiguration) {
			d.RuntimeClasses["example-kata"] = controller.SandboxRuntimePrerequisite{UID: "example-runtime-uid"}
		}},
		{"invalid storage key", func(d *controller.SandboxPlatformConfiguration) {
			d.StorageClasses["../storage"] = d.StorageClasses["example-home"]
		}},
		{"missing storage UID", func(d *controller.SandboxPlatformConfiguration) {
			d.StorageClasses["example-home"] = controller.SandboxStoragePrerequisite{Provisioner: "csi.example.com"}
		}},
		{"missing provisioner", func(d *controller.SandboxPlatformConfiguration) {
			d.StorageClasses["example-home"] = controller.SandboxStoragePrerequisite{UID: "example-storage-uid"}
		}},
		{"unqualified adapter", func(d *controller.SandboxPlatformConfiguration) {
			p := d.Platforms[api.SandboxKata]
			p.Adapter.Qualified = false
			d.Platforms[api.SandboxKata] = p
		}},
		{"unavailable adapter", func(d *controller.SandboxPlatformConfiguration) {
			p := d.Platforms[api.SandboxKata]
			p.Adapter.Available = false
			d.Platforms[api.SandboxKata] = p
		}},
		{"invalid topology", func(d *controller.SandboxPlatformConfiguration) {
			d.Platforms[api.SandboxKata].Topology.PodCIDRs[0] = "example-payload-marker"
		}},
		{"Role is not ClusterRole", func(d *controller.SandboxPlatformConfiguration) {
			d.Platforms[api.SandboxKata].Grants[0].RoleRef.Kind = "Role"
		}},
		{"wrong role API group", func(d *controller.SandboxPlatformConfiguration) {
			d.Platforms[api.SandboxKata].Grants[0].RoleRef.APIGroup = "example.com"
		}},
		{"invalid role name", func(d *controller.SandboxPlatformConfiguration) {
			d.Platforms[api.SandboxKata].Grants[0].RoleRef.Name = "../role"
		}},
		{"invalid grant namespace", func(d *controller.SandboxPlatformConfiguration) {
			d.Platforms[api.SandboxKata].Grants[0].Namespace = "invalid/namespace"
		}},
		{"missing policy UID", func(d *controller.SandboxPlatformConfiguration) {
			d.Platforms[api.SandboxKata].Grants[0].PolicyUID = ""
		}},
		{"missing role UID", func(d *controller.SandboxPlatformConfiguration) { d.Platforms[api.SandboxKata].Grants[0].RoleUID = "" }},
		{"missing role version", func(d *controller.SandboxPlatformConfiguration) {
			d.Platforms[api.SandboxKata].Grants[0].RoleResourceVersion = ""
		}},
		{"missing approval revision", func(d *controller.SandboxPlatformConfiguration) { d.Platforms[api.SandboxKata].Grants[0].Revision = "" }},
		{"missing approved rules", func(d *controller.SandboxPlatformConfiguration) {
			d.Platforms[api.SandboxKata].Grants[0].Rules = []rbacv1.PolicyRule{}
		}},
		{"grants without capabilities", func(d *controller.SandboxPlatformConfiguration) {
			r := d.Registrations[api.SandboxKata]
			r.Capabilities.KubernetesAccess = false
			d.Registrations[api.SandboxKata] = r
		}},
		{"missing audience", func(d *controller.SandboxPlatformConfiguration) {
			p := d.Platforms[api.SandboxKata]
			p.KubernetesAudience = ""
			d.Platforms[api.SandboxKata] = p
		}},
		{"shared Entra audience", func(d *controller.SandboxPlatformConfiguration) {
			p := d.Platforms[api.SandboxKata]
			p.KubernetesAudience = sandboxpolicy.EntraAudience
			d.Platforms[api.SandboxKata] = p
		}},
		{"short token lifetime", func(d *controller.SandboxPlatformConfiguration) {
			p := d.Platforms[api.SandboxKata]
			p.TokenExpirationSeconds = 599
			d.Platforms[api.SandboxKata] = p
		}},
		{"long token lifetime", func(d *controller.SandboxPlatformConfiguration) {
			p := d.Platforms[api.SandboxKata]
			p.TokenExpirationSeconds = 3601
			d.Platforms[api.SandboxKata] = p
		}},
		{"incomplete identity", func(d *controller.SandboxPlatformConfiguration) {
			p := d.Platforms[api.SandboxKata]
			p.Identities = []sandboxpolicy.ApprovedIdentity{{AgentUID: "example-agent"}}
			d.Platforms[api.SandboxKata] = p
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			document, cm, objects := kubernetesPlatformFixture(t)
			tc.mutate(&document)
			cm.Data[controller.SandboxPlatformConfigurationKey] = kubernetesPlatformJSON(t, document)
			source, _ := kubernetesPlatformSource(t, cm, objects)
			if err := requireKubernetesPlatformFailure(t, source); strings.Contains(err.Error(), "example-payload-marker") {
				t.Fatal("semantic validation exposed configuration payload data", err)
			}
		})
	}
}

func TestKubernetesSandboxConfigurationSourceRejectsInconsistentIdentities(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*sandboxpolicy.Platform)
	}{
		{"missing Agent UID", func(p *sandboxpolicy.Platform) { p.Identities[0].AgentUID = "" }},
		{"missing namespace", func(p *sandboxpolicy.Platform) { p.Identities[0].Namespace = "" }},
		{"default service account", func(p *sandboxpolicy.Platform) { p.Identities[0].ServiceAccountName = "default" }},
		{"invalid service account", func(p *sandboxpolicy.Platform) { p.Identities[0].ServiceAccountName = "invalid/account" }},
		{"missing revision", func(p *sandboxpolicy.Platform) { p.Identities[0].Revision = "" }},
		{"missing client", func(p *sandboxpolicy.Platform) { p.Identities[0].Entra.ClientID = "" }},
		{"missing directory", func(p *sandboxpolicy.Platform) { p.Identities[0].Entra.TenantID = "" }},
		{"duplicate Agent", func(p *sandboxpolicy.Platform) { p.Identities = append(p.Identities, p.Identities[0]) }},
		{"borrowed ServiceAccount", func(p *sandboxpolicy.Platform) {
			other := p.Identities[0]
			other.AgentUID = "another-agent-uid"
			p.Identities = append(p.Identities, other)
		}},
		{"control character in identity", func(p *sandboxpolicy.Platform) { p.Identities[0].Entra.ClientID = "example\nclient" }},
		{"no identity authorization", func(p *sandboxpolicy.Platform) {
			p.Identities[0].Entra = nil
			p.Identities[0].Kubernetes = false
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			document, cm, objects := kubernetesPlatformFixture(t)
			platform := document.Platforms[api.SandboxKata]
			platform.Identities = []sandboxpolicy.ApprovedIdentity{{
				AgentUID: "example-agent-uid", Namespace: "example-workload", ServiceAccountName: "example-admin",
				Revision: "example-identity", Kubernetes: true,
				Entra: &sandboxpolicy.EntraIdentity{ClientID: "example-client", TenantID: "example-directory"},
			}}
			tc.mutate(&platform)
			document.Platforms[api.SandboxKata] = platform
			cm.Data[controller.SandboxPlatformConfigurationKey] = kubernetesPlatformJSON(t, document)
			source, _ := kubernetesPlatformSource(t, cm, objects)
			requireKubernetesPlatformFailure(t, source)
		})
	}
}

func TestKubernetesSandboxConfigurationSourceVerifiesLiveRuntimeAndStoragePins(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(client.Object)
	}{
		{"RuntimeClass replacement", func(o client.Object) {
			if _, ok := o.(*nodev1.RuntimeClass); ok {
				o.SetUID("replacement-runtime")
			}
		}},
		{"RuntimeClass handler", func(o client.Object) {
			if r, ok := o.(*nodev1.RuntimeClass); ok {
				r.Handler = "another-handler"
			}
		}},
		{"StorageClass replacement", func(o client.Object) {
			if _, ok := o.(*storagev1.StorageClass); ok {
				o.SetUID("replacement-storage")
			}
		}},
		{"StorageClass provisioner", func(o client.Object) {
			if s, ok := o.(*storagev1.StorageClass); ok {
				s.Provisioner = "another.example.com"
			}
		}},
		{"unpersisted dependency", func(o client.Object) {
			if _, ok := o.(*nodev1.RuntimeClass); ok {
				o.SetResourceVersion("")
			}
		}},
		{"terminating dependency", func(o client.Object) {
			if _, ok := o.(*storagev1.StorageClass); ok {
				stamp := metav1.Now()
				o.SetDeletionTimestamp(&stamp)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			document, cm, objects := kubernetesPlatformFixture(t)
			original := kubernetesPlatformJSON(t, document)
			source, reader := kubernetesPlatformSource(t, cm, objects)
			reader.after = func(object client.Object, _ int) error { tc.mutate(object); return nil }
			requireKubernetesPlatformFailure(t, source)
			if kubernetesPlatformJSON(t, document) != original {
				t.Fatal("runtime or storage pins were silently changed")
			}
		})
	}
	for i := range 2 {
		t.Run(fmt.Sprintf("missing dependency %d", i), func(t *testing.T) {
			_, cm, objects := kubernetesPlatformFixture(t)
			objects = append(objects[:i:i], objects[i+1:]...)
			source, _ := kubernetesPlatformSource(t, cm, objects)
			if err := requireKubernetesPlatformFailure(t, source); !apierrors.IsNotFound(err) {
				t.Fatal("missing dependency lost its cause", err)
			}
		})
	}
}

func TestKubernetesSandboxConfigurationSourceRejectsConcurrentChanges(t *testing.T) {
	for _, name := range []string{"sandbox-platform", "example-kata", "example-home"} {
		for _, change := range []string{"replacement", "version", "terminating", "read failure"} {
			t.Run(name+"/"+change, func(t *testing.T) {
				_, cm, objects := kubernetesPlatformFixture(t)
				source, reader := kubernetesPlatformSource(t, cm, objects)
				failure := errors.New("synthetic recheck failure")
				reader.after = func(object client.Object, count int) error {
					if object.GetName() != name || count != 2 {
						return nil
					}
					switch change {
					case "replacement":
						object.SetUID("concurrent-replacement")
					case "version":
						object.SetResourceVersion("concurrent-version")
					case "terminating":
						stamp := metav1.Now()
						object.SetDeletionTimestamp(&stamp)
					case "read failure":
						return failure
					}
					return nil
				}
				err := requireKubernetesPlatformFailure(t, source)
				if change == "read failure" && !errors.Is(err, failure) {
					t.Fatal("recheck lost its API failure", err)
				}
			})
		}
	}
	t.Run("configuration payload", func(t *testing.T) {
		_, cm, objects := kubernetesPlatformFixture(t)
		source, reader := kubernetesPlatformSource(t, cm, objects)
		reader.after = func(object client.Object, count int) error {
			if cm, ok := object.(*corev1.ConfigMap); ok && count == 2 {
				cm.Data[controller.SandboxPlatformConfigurationKey] = emptyKubernetesPlatformJSON
			}
			return nil
		}
		requireKubernetesPlatformFailure(t, source)
	})
}

func TestKubernetesSandboxConfigurationSourcePreservesReadErrors(t *testing.T) {
	for _, name := range []string{"sandbox-platform", "example-kata", "example-home"} {
		t.Run(name, func(t *testing.T) {
			_, cm, objects := kubernetesPlatformFixture(t)
			source, reader := kubernetesPlatformSource(t, cm, objects)
			failure := errors.New("synthetic API failure")
			reader.after = func(object client.Object, _ int) error {
				if object.GetName() == name {
					return failure
				}
				return nil
			}
			if err := requireKubernetesPlatformFailure(t, source); !errors.Is(err, failure) {
				t.Fatal("source hid an API failure", err)
			}
		})
	}
	_, cm, objects := kubernetesPlatformFixture(t)
	source, _ := kubernetesPlatformSource(t, cm, objects)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	snapshot, err := source.Snapshot(ctx)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(snapshot, controller.SandboxConfigurationSnapshot{}) {
		t.Fatal("cancelled capture returned success or lost cancellation", err)
	}
}

func TestKubernetesSandboxConfigurationSourceNoImplicitQualification(t *testing.T) {
	document, cm, objects := kubernetesPlatformFixture(t)
	cm.Data[controller.SandboxPlatformConfigurationKey] = emptyKubernetesPlatformJSON
	source, reader := kubernetesPlatformSource(t, cm, objects)
	snapshot, err := source.Snapshot(context.Background())
	if err != nil || snapshot.ObservedAt.IsZero() || len(snapshot.Registrations) != 0 || len(snapshot.Platforms) != 0 ||
		len(snapshot.RuntimeClasses) != 0 || len(snapshot.StorageClasses) != 0 || len(reader.calls) != 1 {
		t.Fatalf("empty inventory implicitly discovered or qualified support: %+v, %v", snapshot, err)
	}
	registration := document.Registrations[api.SandboxKata]
	registration.Available = false
	registration.Capabilities.HostResourceLimits = false
	registration.Capabilities.EnforcementObservation = false
	document.Registrations[api.SandboxKata] = registration
	cm.Data[controller.SandboxPlatformConfigurationKey] = kubernetesPlatformJSON(t, document)
	source, _ = kubernetesPlatformSource(t, cm, objects)
	snapshot, err = source.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	captured := snapshot.Registrations[api.SandboxKata]
	if captured.Available || captured.Capabilities.HostResourceLimits || captured.Capabilities.EnforcementObservation {
		t.Fatal("explicitly absent support was promoted into qualification")
	}
}

func TestKubernetesSandboxConfigurationSourceStableAndIdentityBoundRevisions(t *testing.T) {
	for _, tc := range []struct{ name, field string }{
		{"sandbox-platform", "UID"}, {"sandbox-platform", "resourceVersion"},
		{"example-kata", "resourceVersion"}, {"example-home", "resourceVersion"},
	} {
		t.Run(tc.name+"/"+tc.field, func(t *testing.T) {
			_, cm, objects := kubernetesPlatformFixture(t)
			source, reader := kubernetesPlatformSource(t, cm, objects)
			first, err := source.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			firstRevision := first.Registrations[api.SandboxKata].Capabilities.Revision
			source.Now = func() time.Time { return first.ObservedAt.Add(time.Second) }
			stable, err := source.Snapshot(context.Background())
			if err != nil || stable.ObservedAt == first.ObservedAt || stable.Registrations[api.SandboxKata].Capabilities.Revision != firstRevision {
				t.Fatal("timestamp refresh invalidated stable snapshot revision", err)
			}
			reader.after = func(object client.Object, _ int) error {
				if object.GetName() == tc.name {
					if tc.field == "UID" {
						object.SetUID("replaced-configuration")
					} else {
						object.SetResourceVersion("next-version")
					}
				}
				return nil
			}
			changed, err := source.Snapshot(context.Background())
			if err != nil || changed.Registrations[api.SandboxKata].Capabilities.Revision == firstRevision {
				t.Fatal("configuration/dependency identity change did not invalidate reuse", err)
			}
			repeated, err := source.Snapshot(context.Background())
			if err != nil || repeated.Registrations[api.SandboxKata].Capabilities.Revision != changed.Registrations[api.SandboxKata].Capabilities.Revision {
				t.Fatal("unchanged updated inventory did not have a stable revision", err)
			}
		})
	}
}

func TestKubernetesSandboxConfigurationSourceRequiresExplicitRoleReapproval(t *testing.T) {
	document, cm, objects := kubernetesPlatformFixture(t)
	policy, _, _ := configurationGrantFixture()
	source, reader := kubernetesPlatformSource(t, cm, objects)
	first, err := source.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	approved := false
	document.Platforms[api.SandboxKata].Grants[0].RoleResourceVersion = "8"
	updated := kubernetesPlatformJSON(t, document)
	reader.after = func(object client.Object, _ int) error {
		if object.GetName() == "example-view" {
			object.SetResourceVersion("8")
		}
		if cm, ok := object.(*corev1.ConfigMap); ok && approved {
			cm.ResourceVersion = "4"
			cm.Data[controller.SandboxPlatformConfigurationKey] = updated
		}
		return nil
	}
	drifted, err := source.Snapshot(context.Background())
	if err != nil || !reflect.DeepEqual(drifted, first) {
		t.Fatal("live role drift invalidated or silently changed the shared approvals:", err)
	}
	if err := sandboxpolicy.VerifyPolicyGrants(context.Background(), reader, policy, drifted.Platforms[api.SandboxKata].Grants); !errors.Is(err, sandboxpolicy.ErrUnapprovedGrant) {
		t.Fatal("changed live role was not held for explicit reapproval", err)
	}
	approved = true
	next, err := source.Snapshot(context.Background())
	if err != nil || next.ObservedAt != first.ObservedAt ||
		next.Registrations[api.SandboxKata].Capabilities.Revision == first.Registrations[api.SandboxKata].Capabilities.Revision {
		t.Fatal("explicit role pin update did not invalidate reuse independently of timestamps", err)
	}
	if err := sandboxpolicy.VerifyPolicyGrants(context.Background(), reader, policy, next.Platforms[api.SandboxKata].Grants); err != nil {
		t.Fatal("explicitly reapproved requested role was rejected:", err)
	}
}

func TestKubernetesSandboxConfigurationSourceScopesAndNormalizesRoleVerification(t *testing.T) {
	document, cm, objects := kubernetesPlatformFixture(t)
	platform := document.Platforms[api.SandboxKata]
	grant := platform.Grants[0]
	grant.Rules[0].Verbs = []string{"list", "get", "get"}
	grant.RoleRef.Name = "example:" + strings.Repeat("view", 70)
	platform.Grants = []sandboxpolicy.ApprovedGrant{grant}
	grant.Namespace = ""
	platform.Grants = append(platform.Grants, grant)
	grant.PolicyUID = "another-policy-uid"
	grant.RoleUID, grant.RoleResourceVersion = "stale-role-uid", "stale-role-version"
	platform.Grants = append(platform.Grants, grant)
	document.Platforms[api.SandboxKata] = platform
	role := objects[2].(*rbacv1.ClusterRole)
	role.Name = grant.RoleRef.Name
	role.Rules[0].Verbs = []string{"get", "list"}
	cm.Data[controller.SandboxPlatformConfigurationKey] = kubernetesPlatformJSON(t, document)
	source, reader := kubernetesPlatformSource(t, cm, objects)
	snapshot, err := source.Snapshot(context.Background())
	if err != nil || !reflect.DeepEqual(snapshot.Platforms, document.Platforms) {
		t.Fatal("normalized role rules, multiple exact scopes, or valid long ClusterRole name were rejected or mutated", err)
	}
	policy, _, _ := configurationGrantFixture()
	policy.Spec.KubernetesAccess.RoleBindings[0].ClusterRoleName = role.Name
	policy.Spec.KubernetesAccess.ClusterRoleBindings = []api.AgentPolicyClusterRoleBinding{{ClusterRoleName: role.Name}}
	if err := sandboxpolicy.VerifyPolicyGrants(context.Background(), reader, policy, snapshot.Platforms[api.SandboxKata].Grants); err != nil {
		t.Fatal("another policy's stale pins affected normalized requested grants:", err)
	}
	if reader.calls[kubernetesPlatformRead{"ClusterRole", client.ObjectKeyFromObject(role)}] != 1 {
		t.Fatal("multiple requested scopes did not share one authoritative role read")
	}
	policy.UID = grant.PolicyUID
	policy.Spec.KubernetesAccess.RoleBindings = nil
	if err := sandboxpolicy.VerifyPolicyGrants(context.Background(), reader, policy, snapshot.Platforms[api.SandboxKata].Grants); !errors.Is(err, sandboxpolicy.ErrUnapprovedGrant) {
		t.Fatal("another policy's current pins concealed a requested stale approval:", err)
	}
}

func TestKubernetesSandboxConfigurationSourceDoesNotMutateOrShareState(t *testing.T) {
	document, cm, objects := kubernetesPlatformFixture(t)
	platform := document.Platforms[api.SandboxKata]
	platform.Identities = []sandboxpolicy.ApprovedIdentity{{
		AgentUID: "example-agent-uid", Namespace: "example-workload", ServiceAccountName: "example-admin",
		Revision: "example-identity-revision", Kubernetes: true,
		Entra: &sandboxpolicy.EntraIdentity{ClientID: "example-client", TenantID: "example-directory"},
	}}
	document.Platforms[api.SandboxKata] = platform
	original := kubernetesPlatformJSON(t, document)
	cm.Data[controller.SandboxPlatformConfigurationKey] = original
	source, reader := kubernetesPlatformSource(t, cm, objects)
	first, err := source.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := source.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	registration := first.Registrations[api.SandboxKata]
	registration.Available = false
	registration.Capabilities.HomeAccessModes[0] = corev1.ReadWriteMany
	registration.Capabilities.IPFamilies[0] = corev1.IPv6Protocol
	registration.Capabilities.Protocols[0] = corev1.ProtocolSCTP
	first.Registrations[api.SandboxKata] = registration
	p := first.Platforms[api.SandboxKata]
	p.Topology.PodCIDRs[0] = "172.16.0.0/16"
	p.Topology.DNSResolverIPs[0] = "203.0.113.1"
	p.Topology.DNS[0].IPs[0] = "203.0.113.2"
	p.Topology.Gateway[0].Ports[0].Port = 9999
	p.Grants[0].Rules[0].Resources[0] = "secrets"
	p.Identities[0].Entra.ClientID = "changed-client"
	p.Identities[0].ServiceAccountName = "changed-account"
	delete(first.RuntimeClasses, "example-kata")
	delete(first.StorageClasses, "example-home")
	delete(first.Platforms, api.SandboxKata)
	third, err := source.Snapshot(context.Background())
	if err != nil || !reflect.DeepEqual(second, third) || kubernetesPlatformJSON(t, document) != original {
		t.Fatal("returned maps, nested slices or identity pointers escaped across captures", err)
	}
	current := &corev1.ConfigMap{}
	if err := reader.Reader.Get(context.Background(), client.ObjectKeyFromObject(cm), current); err != nil ||
		current.Data[controller.SandboxPlatformConfigurationKey] != original || current.ResourceVersion != cm.ResourceVersion {
		t.Fatal("capture changed persisted configuration", err)
	}
	for _, original := range objects {
		current := original.DeepCopyObject().(client.Object)
		if err := reader.Reader.Get(context.Background(), client.ObjectKeyFromObject(original), current); err != nil {
			t.Fatal(err)
		}
		current.GetObjectKind().SetGroupVersionKind(original.GetObjectKind().GroupVersionKind())
		if !reflect.DeepEqual(original, current) {
			t.Fatal("capture mutated a dependency", original.GetName())
		}
	}
}

func TestKubernetesSandboxConfigurationSourceBounds(t *testing.T) {
	t.Run("byte bound", func(t *testing.T) {
		for _, excess := range []int{0, 1} {
			_, cm, _ := kubernetesPlatformFixture(t)
			cm.Data[controller.SandboxPlatformConfigurationKey] = emptyKubernetesPlatformJSON +
				strings.Repeat(" ", controller.MaxSandboxPlatformConfigurationBytes-len(emptyKubernetesPlatformJSON)+excess)
			source, _ := kubernetesPlatformSource(t, cm, nil)
			if excess == 0 {
				if _, err := source.Snapshot(context.Background()); err != nil {
					t.Fatal("exact byte bound was rejected", err)
				}
			} else {
				requireKubernetesPlatformFailure(t, source)
			}
		}
	})
	t.Run("object bound", func(t *testing.T) {
		for _, count := range []int{127, 128} {
			_, cm, _ := kubernetesPlatformFixture(t)
			document := controller.SandboxPlatformConfiguration{
				Version: "v1", Registrations: map[api.SandboxType]sandbox.Registration{},
				Platforms: map[api.SandboxType]sandboxpolicy.Platform{}, RuntimeClasses: map[string]controller.SandboxRuntimePrerequisite{},
				StorageClasses: map[string]controller.SandboxStoragePrerequisite{},
			}
			var objects []client.Object
			for i := range count {
				name, uid := fmt.Sprintf("example-storage-%d", i), types.UID(fmt.Sprintf("example-storage-uid-%d", i))
				document.StorageClasses[name] = controller.SandboxStoragePrerequisite{UID: uid, Provisioner: "csi.example.com"}
				objects = append(objects, &storagev1.StorageClass{
					ObjectMeta: metav1.ObjectMeta{Name: name, UID: uid, ResourceVersion: "1"}, Provisioner: "csi.example.com",
				})
			}
			cm.Data[controller.SandboxPlatformConfigurationKey] = kubernetesPlatformJSON(t, document)
			source, _ := kubernetesPlatformSource(t, cm, objects)
			if count == 127 {
				if _, err := source.Snapshot(context.Background()); err != nil {
					t.Fatal("exact object bound was rejected", err)
				}
			} else {
				requireKubernetesPlatformFailure(t, source)
			}
		}
	})
	t.Run("declared role object bound", func(t *testing.T) {
		for _, count := range []int{125, 126} {
			document, cm, objects := kubernetesPlatformFixture(t)
			platform := document.Platforms[api.SandboxKata]
			grant := platform.Grants[0]
			platform.Grants = nil
			for i := range count {
				grant.RoleRef.Name = fmt.Sprintf("example-role-%d", i)
				platform.Grants = append(platform.Grants, grant)
			}
			document.Platforms[api.SandboxKata] = platform
			cm.Data[controller.SandboxPlatformConfigurationKey] = kubernetesPlatformJSON(t, document)
			source, _ := kubernetesPlatformSource(t, cm, objects[:2])
			if count == 125 {
				if _, err := source.Snapshot(context.Background()); err != nil {
					t.Fatal("exact declared role object bound was rejected:", err)
				}
			} else {
				requireKubernetesPlatformFailure(t, source)
			}
		}
	})
}
