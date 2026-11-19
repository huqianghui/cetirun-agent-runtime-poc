// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxplatform"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func platformFixtureDocument(f fixture, runtimeUID, storageUID types.UID) sandboxplatform.Configuration {
	registration := f.handler.registrations[api.SandboxKata]
	registration.Capabilities.Protocols = []corev1.Protocol{corev1.ProtocolTCP, corev1.ProtocolUDP}
	return sandboxplatform.Configuration{
		Version: "v1", Registrations: map[api.SandboxType]sandbox.Registration{api.SandboxKata: registration},
		RuntimeClasses: map[string]sandboxplatform.RuntimePrerequisite{
			"test-kata": {UID: runtimeUID, Handler: "kata"},
		},
		StorageClasses: map[string]sandboxplatform.StoragePrerequisite{
			"test-home": {UID: storageUID, Provisioner: "storage.example.com/csi"},
		},
		Platforms: map[api.SandboxType]sandboxpolicy.Platform{
			api.SandboxKata: {
				Adapter: sandboxpolicy.AdapterRegistration{
					Name: sandboxpolicy.AzureCNIOverlayCilium, Revision: "synthetic-only", Qualified: true, Available: true,
				},
				Topology: sandboxpolicy.Topology{
					Revision: "synthetic-topology", ManagedPeersComplete: true,
					PodCIDRs: []string{"10.244.0.0/16"}, ServiceCIDRs: []string{"10.96.0.0/12"},
					NodeCIDRs: []string{"192.168.0.0/16"}, ManagementCIDRs: []string{"10.1.0.0/16"},
					MetadataCIDRs: []string{"169.254.169.254/32"}, DNSResolverIPs: []string{"10.96.0.10"},
					DNS: []sandboxpolicy.Peer{{Name: "dns", Namespace: "kube-system", UID: "synthetic-dns", IPs: []string{"10.96.0.10"}}},
					Gateway: []sandboxpolicy.Destination{{
						Peer:  sandboxpolicy.Peer{Name: "gateway", Namespace: "platform", UID: "synthetic-gateway", IPs: []string{"10.96.1.50"}},
						Ports: []api.AgentPolicyPort{{Port: 8443, Protocol: corev1.ProtocolTCP}},
					}},
					Ingress: []sandboxpolicy.Destination{{
						Peer:  sandboxpolicy.Peer{Name: "ingress", Namespace: "platform", UID: "synthetic-ingress", IPs: []string{"10.96.1.60"}},
						Ports: []api.AgentPolicyPort{{Port: 8080, Protocol: corev1.ProtocolTCP}},
					}},
				},
			},
		},
	}
}

func platformFixtureConfig() Config {
	config := testConfig()
	config.PlatformNamespace, config.PlatformConfigMapName = "operator-test", "sandbox-platform"
	config.TeamAssignments = []TeamAssignment{{Team: "engineering", Users: []string{"member"}}}
	return config
}

func newPlatformFixture(t *testing.T) (fixture, *corev1.ConfigMap) {
	t.Helper()
	f := newFixture(t)
	for _, add := range []func() error{
		func() error { return corev1.AddToScheme(f.client.Scheme()) },
		func() error { return nodev1.AddToScheme(f.client.Scheme()) },
		func() error { return storagev1.AddToScheme(f.client.Scheme()) },
	} {
		if err := add(); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := json.Marshal(platformFixtureDocument(f, "runtime-uid", "storage-uid"))
	if err != nil {
		t.Fatal(err)
	}
	config := platformFixtureConfig()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.PlatformConfigMapName, Namespace: config.PlatformNamespace, UID: "platform-uid"},
		Data:       map[string]string{sandboxplatform.ConfigurationKey: string(raw)},
	}
	for _, object := range []client.Object{
		cm,
		&nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "test-kata", UID: "runtime-uid"}, Handler: "kata"},
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "test-home", UID: "storage-uid"}, Provisioner: "storage.example.com/csi"},
	} {
		if err := f.client.Create(context.Background(), object); err != nil {
			t.Fatal(err)
		}
	}
	f.handler, err = NewHandler(f.client, config)
	if err != nil {
		t.Fatal(err)
	}
	return f, cm
}

func TestPlatformAdmissionProductionFactory(t *testing.T) {
	f, _ := newPlatformFixture(t)
	for _, object := range []client.Object{f.team, f.box, f.policy} {
		requireResponse(t, f.handler.Validate(context.Background(), requestFor(t, admissionv1.Create, object, nil, "administrator")), true, "")
	}
	f.agent.Spec.SandboxRef, f.agent.Spec.Team = "", ""
	response := f.handler.DefaultAgent(context.Background(), requestFor(t, admissionv1.Create, f.agent, nil, "member"))
	requireResponse(t, response, true, "")
	if len(response.Patches) == 0 || f.handler.platformSnapshot != nil || f.handler.registrations != nil {
		t.Fatal("production source must serve request-local decisions without persisting registration state")
	}
	requireResponse(t, f.handler.DefaultAgent(context.Background(), requestFor(t, admissionv1.Create, f.agent, nil, "outsider")), true, "legacy")
	f.agent.Spec.Team = f.team.Name
	requireResponse(t, f.handler.DefaultAgent(context.Background(), requestFor(t, admissionv1.Create, f.agent, nil, "outsider")), false, "assigned Team")
}

func TestNativePlatformAdmissionRequiresIndependentOptIn(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprintf("native-%t", native), func(t *testing.T) {
			f, cm := newPlatformFixture(t)
			var document sandboxplatform.Configuration
			if err := json.Unmarshal([]byte(cm.Data[sandboxplatform.ConfigurationKey]), &document); err != nil {
				t.Fatal(err)
			}
			registration := document.Registrations[api.SandboxKata]
			registration.Capabilities.Profile = sandbox.ProfileNativeSingleStart
			registration.Capabilities.EnforcementObservation, registration.Capabilities.RestrictiveFailure = false, false
			registration.Capabilities.BlockedPorts = false
			document.Registrations[api.SandboxKata] = registration
			platform := document.Platforms[api.SandboxKata]
			platform.Adapter.Name, platform.Adapter.Qualified = sandboxpolicy.Cilium, false
			document.Platforms[api.SandboxKata] = platform
			raw, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			cm.Data[sandboxplatform.ConfigurationKey] = string(raw)
			ctx := context.Background()
			if err := f.client.Update(ctx, cm); err != nil {
				t.Fatal(err)
			}
			f.policy.Spec.Harnesses.Allowed = []api.AgentType{api.AgentTypeOpenClaw}
			if err := f.client.Update(ctx, f.policy); err != nil {
				t.Fatal(err)
			}
			config := platformFixtureConfig()
			config.NativeExecution = native
			f.handler, err = NewHandler(f.client, config)
			if err != nil {
				t.Fatal(err)
			}
			request := requestFor(t, admissionv1.Create, f.team, nil, "administrator")
			requireResponse(t, f.handler.Validate(ctx, request), native, "")
			f.agent.Spec.Agent.Type = api.AgentTypeOpenClaw
			f.agent.Spec.Team, f.agent.Spec.SandboxRef = "", ""
			request = requestFor(t, admissionv1.Create, f.agent, nil, "member")
			requireResponse(t, f.handler.DefaultAgent(ctx, request), native, "")
			if native {
				candidate := f.policy.DeepCopy()
				candidate.Spec.Network.BlockedCIDRs = []string{"203.0.113.0/24"}
				request = requestFor(t, admissionv1.Update, candidate, f.policy, "administrator")
				requireResponse(t, f.handler.Validate(ctx, request), true, "")
				candidate.Spec.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 18080, Protocol: corev1.ProtocolTCP}}
				request = requestFor(t, admissionv1.Update, candidate, f.policy, "administrator")
				requireResponse(t, f.handler.Validate(ctx, request), false, "blockedPorts")
				registration.Capabilities.BlockedPorts = true
				document.Registrations[api.SandboxKata] = registration
				raw, err = json.Marshal(document)
				if err != nil {
					t.Fatal(err)
				}
				cm.Data[sandboxplatform.ConfigurationKey] = string(raw)
				if err := f.client.Update(ctx, cm); err != nil {
					t.Fatal(err)
				}
				requireResponse(t, f.handler.Validate(ctx, request), true, "")
				candidate.Spec.Network.BlockedPorts[0].Protocol = corev1.ProtocolUDP
				request = requestFor(t, admissionv1.Update, candidate, f.policy, "administrator")
				requireResponse(t, f.handler.Validate(ctx, request), true, "")
				candidate = f.policy.DeepCopy()
				candidate.Spec.Network.AllowedInternalCIDRs = []api.AgentPolicyCIDRRule{
					{CIDR: "10.20.0.0/16", Ports: []api.AgentPolicyPort{{Port: 443}}},
				}
				request = requestFor(t, admissionv1.Update, candidate, f.policy, "administrator")
				requireResponse(t, f.handler.Validate(ctx, request), true, "")
				injected := *f.handler
				injected.platformSource, injected.registrations = nil, document.Registrations
				requireResponse(t, injected.Validate(ctx, request), false, "current platform topology")
				candidate.Spec.Network.AllowedInternalCIDRs[0].CIDR = "192.168.0.0/16"
				request = requestFor(t, admissionv1.Update, candidate, f.policy, "administrator")
				requireResponse(t, f.handler.Validate(ctx, request), false, "internal exceptions")
				candidate = f.policy.DeepCopy()
				candidate.Spec.KubernetesAccess = &api.AgentPolicyAdminKubernetesAccess{
					ServiceAccountName: "operations",
					RoleBindings:       []api.AgentPolicyRoleBinding{{Namespace: "operations", ClusterRoleName: "view"}},
				}
				request = requestFor(t, admissionv1.Update, candidate, f.policy, "administrator")
				requireResponse(t, f.handler.Validate(ctx, request), false, "kubernetesAccess")
				if err := f.client.Update(ctx, candidate); err != nil {
					t.Fatal(err)
				}
				request = requestFor(t, admissionv1.Create, f.agent, nil, "member")
				requireResponse(t, f.handler.DefaultAgent(ctx, request), false, "kubernetesAccess")
			}
			// Deleting a recorded Agent must not require fresh native authorization.
			f.handler.config.NativeExecution = false
			request = requestFor(t, admissionv1.Delete, f.agent, nil, "administrator")
			requireResponse(t, f.handler.Validate(ctx, request), true, "")
		})
	}
}

func TestNativeTopologyFailureReturnsSafeCompatibilityCause(t *testing.T) {
	f, cm := newPlatformFixture(t)
	var document sandboxplatform.Configuration
	if err := json.Unmarshal([]byte(cm.Data[sandboxplatform.ConfigurationKey]), &document); err != nil {
		t.Fatal(err)
	}
	registration := document.Registrations[api.SandboxKata]
	registration.Capabilities.Profile = sandbox.ProfileNativeSingleStart
	registration.Capabilities.EnforcementObservation, registration.Capabilities.RestrictiveFailure = false, false
	document.Registrations[api.SandboxKata] = registration
	platform := document.Platforms[api.SandboxKata]
	platform.Adapter.Name, platform.Adapter.Qualified = sandboxpolicy.Cilium, false
	document.Platforms[api.SandboxKata] = platform
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	cm.Data[sandboxplatform.ConfigurationKey] = string(raw)
	if err := f.client.Update(context.Background(), cm); err != nil {
		t.Fatal(err)
	}
	config := platformFixtureConfig()
	config.NativeExecution = true
	f.handler, err = NewHandler(f.client, config)
	if err != nil {
		t.Fatal(err)
	}
	f.policy.Spec.Network.AllowedInternalCIDRs = []api.AgentPolicyCIDRRule{{CIDR: "192.168.0.0/16", Ports: []api.AgentPolicyPort{{Port: 443, Protocol: corev1.ProtocolTCP}}}}
	if err := f.client.Update(context.Background(), f.policy); err != nil {
		t.Fatal(err)
	}
	if _, err := f.handler.platformSource.Snapshot(context.Background()); err != nil {
		t.Fatalf("platform snapshot: %v", err)
	}
	response := f.handler.Validate(context.Background(), requestFor(t, admissionv1.Create, f.team, nil, "administrator"))
	if response.Allowed || response.Result == nil || response.Result.Details == nil {
		t.Fatalf("native grant response = %+v", response)
	}
	for _, cause := range response.Result.Details.Causes {
		if cause.Type != metav1.CauseType("CetiRunCompatibility") {
			continue
		}
		var issue api.CompatibilityIssue
		if err := json.Unmarshal([]byte(cause.Message), &issue); err != nil {
			t.Fatal(err)
		}
		if issue.Valid() && issue.Code == "network_control_unsupported" {
			return
		}
	}
	t.Fatalf("native grant compatibility causes = %+v", response.Result.Details.Causes)
}

func TestPlatformAdmissionScopesExecutionProfiles(t *testing.T) {
	for _, source := range []string{"platform", "injected"} {
		t.Run(source, func(t *testing.T) {
			f, cm := newPlatformFixture(t)
			var document sandboxplatform.Configuration
			if err := json.Unmarshal([]byte(cm.Data[sandboxplatform.ConfigurationKey]), &document); err != nil {
				t.Fatal(err)
			}
			qualified := document.Registrations[api.SandboxKata]
			qualified.Capabilities.RuntimeClassName = "test-gvisor"
			document.Registrations[api.SandboxGVisor] = qualified
			document.RuntimeClasses["test-gvisor"] = sandboxplatform.RuntimePrerequisite{UID: "gvisor-runtime-uid", Handler: "gvisor"}
			document.Platforms[api.SandboxGVisor] = document.Platforms[api.SandboxKata]
			native := document.Registrations[api.SandboxKata]
			native.Capabilities.Profile = sandbox.ProfileNativeSingleStart
			native.Capabilities.EnforcementObservation, native.Capabilities.RestrictiveFailure = false, false
			native.Capabilities.BlockedPorts = false
			document.Registrations[api.SandboxKata] = native
			platform := document.Platforms[api.SandboxKata]
			platform.Adapter.Name, platform.Adapter.Qualified = sandboxpolicy.Cilium, false
			document.Platforms[api.SandboxKata] = platform
			raw, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			cm.Data[sandboxplatform.ConfigurationKey] = string(raw)
			f.box.Spec.Type = api.SandboxGVisor
			f.policy.Spec.Harnesses.Allowed = []api.AgentType{api.AgentTypeOpenClaw}
			ctx := context.Background()
			for _, object := range []client.Object{cm, f.box, f.policy} {
				if err := f.client.Update(ctx, object); err != nil {
					t.Fatal(err)
				}
			}
			runtimeClass := &nodev1.RuntimeClass{
				ObjectMeta: metav1.ObjectMeta{Name: "test-gvisor", UID: "gvisor-runtime-uid"}, Handler: "gvisor",
			}
			if err := f.client.Create(ctx, runtimeClass); err != nil {
				t.Fatal(err)
			}
			if source == "injected" {
				f.handler.platformSource = nil
				f.handler.registrations = document.Registrations
			}
			for _, object := range []client.Object{f.team, f.box, f.policy} {
				requireResponse(t, f.handler.Validate(ctx, requestFor(t, admissionv1.Create, object, nil, "administrator")), true, "")
			}
			alternate := f.policy.DeepCopy()
			alternate.Name, alternate.UID, alternate.ResourceVersion = "alternate-policy", "alternate-policy-uid", ""
			if err := f.client.Create(ctx, alternate); err != nil {
				t.Fatal(err)
			}
			team := f.team.DeepCopy()
			team.Spec.AgentPolicyRef = alternate.Name
			requireResponse(t, f.handler.Validate(ctx, requestFor(t, admissionv1.Update, team, f.team, "administrator")), true, "")

			f.agent.Spec.Agent.Type = api.AgentTypeOpenClaw
			old := admitAgent(t, f.handler, requestFor(t, admissionv1.Create, f.agent, nil, "member"))
			updated := old.DeepCopy()
			requestReauthorization(t, f.handler, updated, "qualified-reauthorization")
			admitAgent(t, f.handler, requestFor(t, admissionv1.Update, updated, old, "administrator"))

			box := f.box.DeepCopy()
			box.Name, box.UID, box.ResourceVersion, box.Spec.Type = "native", "native-sandbox-uid", "", api.SandboxKata
			if err := f.client.Create(ctx, box); err != nil {
				t.Fatal(err)
			}
			f.policy.Spec.SandboxSelection.AllowMultiple = true
			if err := f.client.Update(ctx, f.policy); err != nil {
				t.Fatal(err)
			}
			team = f.team.DeepCopy()
			team.Spec.SandboxRefs = append(team.Spec.SandboxRefs, box.Name)
			requireResponse(t, f.handler.Validate(ctx, requestFor(t, admissionv1.Update, team, f.team, "administrator")), false, "nativeExecution")
			if err := f.client.Update(ctx, team); err != nil {
				t.Fatal(err)
			}
			admitAgent(t, f.handler, requestFor(t, admissionv1.Create, f.agent, nil, "member"))
			requestReauthorization(t, f.handler, updated, "qualified-mixed-team")
			admitAgent(t, f.handler, requestFor(t, admissionv1.Update, updated, old, "administrator"))
			for _, object := range []client.Object{box, f.policy} {
				requireResponse(t, f.handler.Validate(ctx, requestFor(t, admissionv1.Create, object, nil, "administrator")), false, "nativeExecution")
			}
			previous := team.DeepCopy()
			team.Spec.SandboxRefs, team.Spec.DefaultSandboxRef = []string{box.Name}, box.Name
			requireResponse(t, f.handler.Validate(ctx, requestFor(t, admissionv1.Update, team, previous, "administrator")), false, "nativeExecution")
			if err := f.client.Update(ctx, team); err != nil {
				t.Fatal(err)
			}
			f.agent.Spec.SandboxRef = box.Name
			request := requestFor(t, admissionv1.Create, f.agent, nil, "member")
			requireResponse(t, f.handler.DefaultAgent(ctx, request), false, "nativeExecution")
			updated = old.DeepCopy()
			updated.Spec.SandboxRef = box.Name
			requestReauthorization(t, f.handler, updated, "native-reauthorization")
			request = requestFor(t, admissionv1.Update, updated, old, "administrator")
			requireResponse(t, f.handler.DefaultAgent(ctx, request), false, "nativeExecution")
			requireResponse(t, f.handler.Validate(ctx, request), false, "nativeExecution")

			f.handler.config.NativeExecution = true
			admitAgent(t, f.handler, request)
		})
	}
}

func TestPlatformAdmissionNoLastKnownGoodFallback(t *testing.T) {
	for _, failure := range []string{"missing", "malformed", "unqualified", "runtime replaced", "storage replaced"} {
		t.Run(failure, func(t *testing.T) {
			f, cm := newPlatformFixture(t)
			request := requestFor(t, admissionv1.Create, f.team, nil, "administrator")
			requireResponse(t, f.handler.Validate(context.Background(), request), true, "")
			switch failure {
			case "missing":
				if err := f.client.Delete(context.Background(), cm); err != nil {
					t.Fatal(err)
				}
			case "malformed", "unqualified":
				if failure == "malformed" {
					cm.Data[sandboxplatform.ConfigurationKey] = `{"invalid":"do-not-log-this-platform-value"}`
				} else {
					cm.Data[sandboxplatform.ConfigurationKey] = strings.ReplaceAll(cm.Data[sandboxplatform.ConfigurationKey], `"qualified":true`, `"qualified":false`)
				}
				if err := f.client.Update(context.Background(), cm); err != nil {
					t.Fatal(err)
				}
			case "runtime replaced":
				current := &nodev1.RuntimeClass{}
				key := client.ObjectKey{Name: "test-kata"}
				if err := f.client.Get(context.Background(), key, current); err != nil {
					t.Fatal(err)
				}
				current.UID = "replacement-runtime"
				if err := f.client.Update(context.Background(), current); err != nil {
					t.Fatal(err)
				}
			case "storage replaced":
				current := &storagev1.StorageClass{}
				if err := f.client.Get(context.Background(), client.ObjectKey{Name: "test-home"}, current); err != nil {
					t.Fatal(err)
				}
				current.UID = "replacement-storage"
				if err := f.client.Update(context.Background(), current); err != nil {
					t.Fatal(err)
				}
			}
			response := f.handler.Validate(context.Background(), request)
			requireResponse(t, response, false, "dependencies are unavailable")
			if response.Result.Code != 503 || strings.Contains(response.Result.Message, "do-not-log") {
				t.Fatal("dependency failure must be explicit and payload-free")
			}
		})
	}
}

type platformInterceptReader struct {
	client.Reader
	before func(context.Context, client.Object) error
}

func (r platformInterceptReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if r.before != nil {
		if err := r.before(ctx, object); err != nil {
			return err
		}
	}
	return r.Reader.Get(ctx, key, object, options...)
}

func TestPlatformAdmissionRechecksBeforeAuthorizing(t *testing.T) {
	f, cm := newPlatformFixture(t)
	reads := 0
	reader := platformInterceptReader{Reader: f.client, before: func(_ context.Context, object client.Object) error {
		if _, ok := object.(*corev1.ConfigMap); !ok {
			return nil
		}
		reads++
		if reads == 3 {
			cm.Annotations = map[string]string{"example.com/revision": "changed"}
			return f.client.Update(context.Background(), cm)
		}
		return nil
	}}
	var err error
	f.handler, err = NewHandler(reader, platformFixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	requireResponse(t, f.handler.Validate(context.Background(), requestFor(t, admissionv1.Create, f.team, nil, "administrator")), false, "dependencies")
	requireResponse(t, f.handler.Validate(context.Background(), requestFor(t, admissionv1.Create, f.team, nil, "administrator")), true, "")
}

func TestPlatformAdmissionOutageDoesNotBlockIndependentRequests(t *testing.T) {
	f, cm := newPlatformFixture(t)
	if err := f.client.Delete(context.Background(), cm); err != nil {
		t.Fatal(err)
	}
	legacy := f.agent.DeepCopy()
	legacy.Spec.Team, legacy.Spec.SandboxRef = "", ""
	plain := f.policy.DeepCopy()
	plain.Name = "unreferenced"
	requests := []admission.Request{
		requestFor(t, admissionv1.Create, legacy, nil, "unassigned-member"),
		requestFor(t, admissionv1.Delete, f.agent, nil, "member"),
		requestFor(t, admissionv1.Create, plain, nil, "administrator"),
		requestFor(t, admissionv1.Delete, plain, nil, "administrator"),
		requestFor(t, admissionv1.Update, f.team, f.team.DeepCopy(), "member"),
	}
	status := requestFor(t, admissionv1.Update, f.agent, f.agent.DeepCopy(), testController)
	status.SubResource = "status"
	requests = append(requests, status)
	held := f.agent.DeepCopy()
	now := metav1.Now()
	held.DeletionTimestamp = &now
	held.Finalizers = append(held.Finalizers, sandbox.HoldFinalizer)
	deleting := f.agent.DeepCopy()
	deleting.DeletionTimestamp = &now
	cleanup := requestFor(t, admissionv1.Update, deleting, held, testController)
	requests = append(requests, cleanup)
	for i, request := range requests {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			requireResponse(t, f.handler.Validate(context.Background(), request), true, "")
		})
	}
	requireResponse(t, f.handler.DefaultAgent(context.Background(), requests[0]), true, "legacy")
	requireResponse(t, f.handler.Validate(context.Background(), requestFor(t, admissionv1.Delete, f.policy, nil, "administrator")), false, "referenced")
}

func TestPlatformAdmissionSuspensionOnly(t *testing.T) {
	for _, mode := range []string{ValidationStrict, ValidationCompatible} {
		for _, failure := range []string{"missing", "malformed"} {
			t.Run(mode+"/"+failure, func(t *testing.T) {
				f, cm := newPlatformFixture(t)
				f.handler.config.ValidationMode = mode
				stampFixtureAuthorization(t, f)
				f.agent.Finalizers = append(f.agent.Finalizers, sandbox.HoldFinalizer, sandbox.PreparationFinalizer)
				suspended := f.agent.DeepCopy()
				suspended.Spec.Suspended = true
				suspended.Generation++
				suspended.ManagedFields = []metav1.ManagedFieldsEntry{{
					Manager: "synthetic-client", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: api.GroupVersion.String(),
				}}
				reauthorized := suspended.DeepCopy()
				requestReauthorization(t, f.handler, reauthorized, "synthetic-suspension-request")
				if failure == "missing" {
					if err := f.client.Delete(context.Background(), cm); err != nil {
						t.Fatal(err)
					}
				} else {
					cm.Data[sandboxplatform.ConfigurationKey] = `{"invalid":"synthetic-invalid-platform"}`
					if err := f.client.Update(context.Background(), cm); err != nil {
						t.Fatal(err)
					}
				}
				source, reads := f.handler.platformSource, 0
				f.handler.platformSource = platformSnapshotFunc(func(ctx context.Context) (sandboxplatform.Snapshot, error) {
					reads++
					return source.Snapshot(ctx)
				})
				for _, user := range []string{"administrator", "member", "outsider", testController, "system:anonymous"} {
					for _, explicitFalse := range []bool{false, true} {
						t.Run(fmt.Sprintf("%s/explicit-false=%v", user, explicitFalse), func(t *testing.T) {
							request := requestFor(t, admissionv1.Update, suspended, f.agent, user)
							if explicitFalse {
								addUnknownField(t, &request.OldObject, "spec", "suspended", false)
							}
							if mode == ValidationCompatible {
								for _, raw := range []*runtime.RawExtension{&request.Object, &request.OldObject} {
									for _, section := range []string{"spec", "status"} {
										addUnknownField(t, raw, section, "futureCounter", json.Number("9007199254740992"))
									}
								}
							}
							allowed := user == "administrator" || user == "member"
							message := "authenticated assigned Team member"
							if allowed {
								message = "suspension-only"
								mutation := f.handler.DefaultAgent(context.Background(), request)
								requireResponse(t, mutation, true, "")
								if len(mutation.Patches) != 0 {
									t.Fatal("suspension must not change defaults or authorization")
								}
							}
							before := reads
							requireResponse(t, f.handler.Validate(context.Background(), request), allowed, message)
							if reads != before {
								t.Fatal("suspension or caller-authority checks consulted platform inventory")
							}
						})
					}
				}
				for _, test := range []struct {
					name string
					edit func(*admission.Request)
				}{
					{"creation", func(r *admission.Request) {
						r.Operation, r.OldObject = admissionv1.Create, runtime.RawExtension{}
					}},
					{"resume", func(r *admission.Request) {
						addUnknownField(t, &r.OldObject, "spec", "suspended", true)
						addUnknownField(t, &r.Object, "spec", "suspended", false)
					}},
					{"already suspended", func(r *admission.Request) {
						addUnknownField(t, &r.OldObject, "spec", "suspended", true)
					}},
					{"no transition", func(r *admission.Request) {
						addUnknownField(t, &r.Object, "spec", "suspended", false)
					}},
					{"Team binding", func(r *admission.Request) {
						addUnknownField(t, &r.Object, "spec", "team", "another-team")
					}},
					{"Sandbox binding", func(r *admission.Request) {
						addUnknownField(t, &r.Object, "spec", "sandboxRef", "another-sandbox")
					}},
					{"model", func(r *admission.Request) {
						addUnknownField(t, &r.Object, "spec", "model", "another-model")
					}},
					{"Secret reference", func(r *admission.Request) {
						addUnknownField(t, &r.Object, "spec", "gatewayTokenSecretRef", "another-secret")
					}},
					{"labels", func(r *admission.Request) {
						addUnknownField(t, &r.Object, "metadata", "labels", map[string]string{"example.com/note": "changed"})
					}},
					{"annotations", func(r *admission.Request) {
						addUnknownField(t, &r.Object, "metadata", "annotations", map[string]string{
							sandboxbinding.AuthorizationAnnotation: f.agent.Annotations[sandboxbinding.AuthorizationAnnotation],
							"example.com/note":                     "changed",
						})
					}},
					{"finalizers", func(r *admission.Request) {
						addUnknownField(t, &r.Object, "metadata", "finalizers", append(suspended.GetFinalizers(), "example.com/other"))
					}},
					{"owner references", func(r *admission.Request) {
						addUnknownField(t, &r.Object, "metadata", "ownerReferences", []metav1.OwnerReference{{
							APIVersion: api.GroupVersion.String(), Kind: "Team", Name: f.team.Name, UID: f.team.UID,
						}})
					}},
					{"authorization record", func(r *admission.Request) {
						addUnknownField(t, &r.Object, "metadata", "annotations", map[string]string{
							sandboxbinding.AuthorizationAnnotation: "synthetic-invalid-authorization",
						})
					}},
					{"reauthorization", func(r *admission.Request) {
						addUnknownField(t, &r.Object, "metadata", "annotations", reauthorized.Annotations)
					}},
					{"UID", func(r *admission.Request) {
						addUnknownField(t, &r.Object, "metadata", "uid", "replacement-agent")
					}},
					{"resourceVersion", func(r *admission.Request) {
						addUnknownField(t, &r.Object, "metadata", "resourceVersion", "2")
					}},
					{"status", func(r *admission.Request) {
						addUnknownField(t, &r.Object, "status", "phase", api.PhaseReady)
					}},
					{"future nested spec", func(r *admission.Request) {
						addUnknownField(t, &r.Object, "spec", "agent", map[string]any{
							"type": api.AgentTypeHermes, "futureCapability": true,
						})
					}},
				} {
					t.Run(test.name, func(t *testing.T) {
						request := requestFor(t, admissionv1.Update, suspended, f.agent, "administrator")
						test.edit(&request)
						requireResponse(t, f.handler.Validate(context.Background(), request), false, "")
					})
				}
				for _, section := range []string{"spec", "metadata", "status", "futureSection"} {
					t.Run("changed future field/"+section, func(t *testing.T) {
						request := requestFor(t, admissionv1.Update, suspended, f.agent, "administrator")
						addUnknownField(t, &request.OldObject, section, "futureCounter", json.Number("9007199254740992"))
						addUnknownField(t, &request.Object, section, "futureCounter", json.Number("9007199254740993"))
						requireResponse(t, f.handler.Validate(context.Background(), request), false, "")
					})
				}
			})
		}
	}
}

func TestPlatformAdmissionReadsRespectTotalDeadline(t *testing.T) {
	f, _ := newPlatformFixture(t)
	reader := platformInterceptReader{Reader: f.client, before: func(ctx context.Context, object client.Object) error {
		if _, ok := object.(*corev1.ConfigMap); ok {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}}
	handler, err := NewHandler(reader, platformFixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	handler.requestTimeout = 10 * time.Millisecond
	begin := time.Now()
	requireResponse(t, handler.Validate(context.Background(), requestFor(t, admissionv1.Create, f.team, nil, "administrator")), false, "dependencies")
	if time.Since(begin) > time.Second {
		t.Fatal("source reads exceeded the request budget")
	}
}

type platformSnapshotFunc func(context.Context) (sandboxplatform.Snapshot, error)

func (f platformSnapshotFunc) Snapshot(ctx context.Context) (sandboxplatform.Snapshot, error) {
	return f(ctx)
}

func TestPlatformAdmissionRejectsStaleAndFutureSnapshots(t *testing.T) {
	for _, observed := range []time.Time{{}, time.Now().Add(-time.Hour), time.Now().Add(time.Hour)} {
		f, _ := newPlatformFixture(t)
		source := f.handler.platformSource
		f.handler.platformSource = platformSnapshotFunc(func(ctx context.Context) (sandboxplatform.Snapshot, error) {
			snapshot, err := source.Snapshot(ctx)
			snapshot.ObservedAt = observed
			return snapshot, err
		})
		requireResponse(t, f.handler.Validate(context.Background(), requestFor(t, admissionv1.Create, f.team, nil, "administrator")), false, "dependencies")
	}
}

func TestPlatformAdmissionConcurrentRequestsDoNotShareSnapshots(t *testing.T) {
	f, _ := newPlatformFixture(t)
	request := requestFor(t, admissionv1.Create, f.team, nil, "administrator")
	var group sync.WaitGroup
	responses := make(chan admission.Response, 12)
	for range cap(responses) {
		group.Go(func() { responses <- f.handler.Validate(context.Background(), request) })
	}
	group.Wait()
	close(responses)
	for response := range responses {
		requireResponse(t, response, true, "")
	}
	if f.handler.platformSnapshot != nil {
		t.Fatal("request snapshot escaped to the shared handler")
	}
}
