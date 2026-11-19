// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const testController = "system:serviceaccount:operator-test:controller"

func testConfig() Config {
	return Config{ValidationMode: ValidationStrict, AdminUsers: []string{"administrator", "system:serviceaccount:test:portal"}, AdminGroups: []string{"policy-test-admins"}, ControllerUsername: testController}
}

type fixture struct {
	handler *Handler
	client  client.Client
	agent   *api.Agent
	team    *api.Team
	box     *api.Sandbox
	policy  *api.AgentPolicy
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	resources := api.SandboxContainerResources{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("1Gi")},
		Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("2Gi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("2Gi")},
	}
	box := &api.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "standard", UID: "sandbox-uid", ResourceVersion: "1"},
		Spec: api.SandboxSpec{
			Resources: api.SandboxResources{Agent: resources, Helpers: resources},
			Storage: api.SandboxStorage{
				Home:    api.SandboxHomeStorage{StorageClassName: "test-home", Size: resource.MustParse("10Gi"), AccessMode: corev1.ReadWriteOncePod},
				Scratch: api.SandboxScratchStorage{SizeLimit: resource.MustParse("1Gi")},
			},
		},
	}
	policy := &api.AgentPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "normal", UID: "policy-uid", ResourceVersion: "1"},
		Spec:       api.AgentPolicySpec{Harnesses: api.AgentPolicyHarnesses{Allowed: []api.AgentType{api.AgentTypeHermes}}},
	}
	team := &api.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "engineering", UID: "team-uid", ResourceVersion: "1"},
		Spec: api.TeamSpec{
			AgentPolicyRef:    policy.Name,
			SandboxRefs:       []string{box.Name},
			DefaultSandboxRef: box.Name,
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(box, policy, team).Build()
	handler, err := NewHandler(cl, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	handler.registrations = map[api.SandboxType]sandbox.Registration{
		api.SandboxKata: {Available: true, Capabilities: sandbox.Capabilities{
			RuntimeClassName: "test-kata", Revision: "synthetic-test-only",
			HostResourceLimits: true, ReadOnlyRootFilesystem: true,
			HomeAccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod},
			IPFamilies:      []corev1.IPFamily{corev1.IPv4Protocol}, Protocols: []corev1.Protocol{corev1.ProtocolTCP},
			GuestExternalNetwork: true, DenyPrecedence: true, NodeTraffic: true, DNSIPRestrictions: true,
			BlockedPorts: true, AuthenticatedIngress: true, DisabledIngress: true, EnforcementObservation: true, RestrictiveFailure: true,
		}},
	}
	agent := &api.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name: "alice-work", UID: "agent-uid", ResourceVersion: "1", Finalizers: []string{api.Finalizer},
			Annotations: map[string]string{},
		},
		Spec: api.AgentSpec{
			Agent: api.HarnessSpec{Type: api.AgentTypeHermes}, User: api.UserSpec{Alias: "alice"},
			GatewayTokenSecretRef: "gateway-token", Team: team.Name, SandboxRef: box.Name,
		},
	}
	return fixture{handler, cl, agent, team, box, policy}
}

func requestFor(t *testing.T, operation admissionv1.Operation, object, old client.Object, username string) admission.Request {
	t.Helper()
	var resourceName, kind string
	switch object.(type) {
	case *api.Agent:
		resourceName, kind = "agents", "Agent"
	case *api.Team:
		resourceName, kind = "teams", "Team"
	case *api.Sandbox:
		resourceName, kind = "sandboxes", "Sandbox"
	case *api.AgentPolicy:
		resourceName, kind = "agentpolicies", "AgentPolicy"
	default:
		t.Fatal("unsupported fixture")
	}
	raw := func(value client.Object) runtime.RawExtension {
		if value == nil {
			return runtime.RawExtension{}
		}
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return runtime.RawExtension{Raw: data}
	}
	request := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID: "request-uid", Name: object.GetName(), Operation: operation,
		Resource: metav1.GroupVersionResource{Group: api.GroupVersion.Group, Version: api.GroupVersion.Version, Resource: resourceName},
		Kind:     metav1.GroupVersionKind{Group: api.GroupVersion.Group, Version: api.GroupVersion.Version, Kind: kind},
		UserInfo: authenticationv1.UserInfo{Username: username}, Object: raw(object), OldObject: raw(old),
	}}
	if operation == admissionv1.Delete {
		request.OldObject, request.Object = request.Object, runtime.RawExtension{}
	}
	return request
}

func requireResponse(t *testing.T, response admission.Response, allowed bool, message string) {
	t.Helper()
	if response.Allowed != allowed || (message != "" && (response.Result == nil || !strings.Contains(response.Result.Message, message))) {
		t.Fatalf("want allowed=%v containing %q, got %#v (status: %+v)", allowed, message, response.AdmissionResponse, response.Result)
	}
}

func TestExplicitAdmissionAuthority(t *testing.T) {
	for _, resourceName := range []string{"agent", "team", "sandbox", "policy"} {
		t.Run(resourceName, func(t *testing.T) {
			f := newFixture(t)
			objects := map[string]client.Object{"agent": f.agent, "team": f.team, "sandbox": f.box, "policy": f.policy}
			for _, user := range []string{"member", testController, "system:anonymous"} {
				req := requestFor(t, admissionv1.Create, objects[resourceName], nil, user)
				requireResponse(t, f.handler.Validate(context.Background(), req), false, "administrator")
			}
			for _, user := range []string{"administrator", "group-administrator"} {
				if resourceName == "agent" {
					stampFixtureAuthorization(t, f)
				}
				req := requestFor(t, admissionv1.Create, objects[resourceName], nil, user)
				if user == "group-administrator" {
					req.UserInfo.Groups = []string{"policy-test-admins"}
				}
				requireResponse(t, f.handler.Validate(context.Background(), req), true, "")
			}
		})
	}
}

func TestCreationDefaultsNeverPromoteAdministrators(t *testing.T) {
	f := newFixture(t)
	f.agent.Spec.SandboxRef = ""
	req := requestFor(t, admissionv1.Create, f.agent, nil, "administrator")
	response := f.handler.DefaultAgent(context.Background(), req)
	requireResponse(t, response, true, "")
	if len(response.Patches) != 2 || response.Patches[0].Path != "/spec/sandboxRef" || response.Patches[0].Value != "standard" {
		t.Fatalf("unexpected selection patch: %#v", response.Patches)
	}
	req = requestFor(t, admissionv1.Update, f.agent, f.agent.DeepCopy(), "administrator")
	requireResponse(t, f.handler.DefaultAgent(context.Background(), req), true, "never re-default")
	requireResponse(t, f.handler.Validate(context.Background(), req), false, "never re-defaulted")
}

func TestGeneratedNameCreatesRemainValidBeforeAndAfterNameAssignment(t *testing.T) {
	for _, governed := range []bool{false, true} {
		t.Run(fmt.Sprintf("governed=%v", governed), func(t *testing.T) {
			f := newFixture(t)
			agent := f.agent.DeepCopy()
			agent.Name, agent.GenerateName, agent.UID, agent.ResourceVersion = "", "generated-agent-", "", ""
			agent.Spec.SandboxRef = ""
			if !governed {
				agent.Spec.Team = ""
			}
			req := requestFor(t, admissionv1.Create, agent, nil, "administrator")
			response := f.handler.DefaultAgent(context.Background(), req)
			requireResponse(t, response, true, "")
			if governed {
				if len(response.Patches) != 2 {
					t.Fatalf("governed generated-name creation did not receive defaults: %#v", response.Patches)
				}
				agent, _ = applyAgentPatches(t, req, response)
			} else if len(response.Patches) != 0 {
				t.Fatalf("legacy generated-name creation acquired defaults: %#v", response.Patches)
			}
			requireResponse(t, f.handler.Validate(context.Background(), requestFor(t, admissionv1.Create, agent, nil, "administrator")), true, "")

			agent.Name = "generated-agent-abcde"
			requireResponse(t, f.handler.Validate(context.Background(), requestFor(t, admissionv1.Create, agent, nil, "administrator")), true, "")
			mismatch := requestFor(t, admissionv1.Create, agent, nil, "administrator")
			mismatch.Name = "different-name"
			requireResponse(t, f.handler.Validate(context.Background(), mismatch), false, "identity")

			agent.Name, agent.GenerateName = "", ""
			requireResponse(t, f.handler.Validate(context.Background(), requestFor(t, admissionv1.Create, agent, nil, "administrator")), false, "identity")
		})
	}
}

func TestLegacyBehaviorAndTeamEdits(t *testing.T) {
	f := newFixture(t)
	f.agent.Spec.Team, f.agent.Spec.SandboxRef = "", ""
	req := requestFor(t, admissionv1.Create, f.agent, nil, "portal")
	requireResponse(t, f.handler.Validate(context.Background(), req), true, "legacy")
	response := f.handler.DefaultAgent(context.Background(), req)
	requireResponse(t, response, true, "legacy")
	if len(response.Patches) != 0 {
		t.Fatal("legacy Agent must not acquire Sandbox bindings")
	}
	team := f.team.DeepCopy()
	team.Spec.MemberContext.Role = "updated application context"
	f.handler.registrations = nil
	req = requestFor(t, admissionv1.Update, team, f.team, "portal")
	requireResponse(t, f.handler.Validate(context.Background(), req), true, "unchanged")
	team.Spec.DefaultSandboxRef = "different"
	req = requestFor(t, admissionv1.Update, team, f.team, "portal")
	requireResponse(t, f.handler.Validate(context.Background(), req), false, "administrator")
}

func TestGarbageCollectorCanOnlyRemoveForegroundDeletionMetadata(t *testing.T) {
	for _, username := range []string{
		"system:kube-controller-manager",
		"system:serviceaccount:kube-system:generic-garbage-collector",
	} {
		for _, fixtureObject := range []string{"Agent", "Sandbox", "AgentPolicy"} {
			t.Run(username+"/"+fixtureObject, func(t *testing.T) {
				f := newFixture(t)
				objects := map[string]client.Object{"Agent": f.agent, "Sandbox": f.box, "AgentPolicy": f.policy}
				old := objects[fixtureObject].DeepCopyObject().(client.Object)
				now := metav1.Now()
				old.SetDeletionTimestamp(&now)
				old.SetFinalizers(append(old.GetFinalizers(), metav1.FinalizerDeleteDependents))
				object := old.DeepCopyObject().(client.Object)
				object.SetFinalizers(slices.DeleteFunc(object.GetFinalizers(), func(value string) bool {
					return value == metav1.FinalizerDeleteDependents
				}))
				req := requestFor(t, admissionv1.Update, object, old, username)
				requireResponse(t, f.handler.Validate(context.Background(), req), true, "garbage collector")
			})
		}
	}

	f := newFixture(t)
	now := metav1.Now()
	oldPolicy := f.policy.DeepCopy()
	oldPolicy.SetDeletionTimestamp(&now)
	oldPolicy.SetFinalizers([]string{metav1.FinalizerDeleteDependents})
	changedPolicy := oldPolicy.DeepCopy()
	changedPolicy.Finalizers = nil
	changedPolicy.Spec.Network.BlockedFQDNs = []string{"blocked.example.com"}
	req := requestFor(t, admissionv1.Update, changedPolicy, oldPolicy, "system:kube-controller-manager")
	requireResponse(t, f.handler.Validate(context.Background(), req), false, "administrator")

	nonCollector := oldPolicy.DeepCopy()
	nonCollector.Finalizers = nil
	req = requestFor(t, admissionv1.Update, nonCollector, oldPolicy, "member")
	requireResponse(t, f.handler.Validate(context.Background(), req), false, "administrator")

	oldAgent := f.agent.DeepCopy()
	oldAgent.SetDeletionTimestamp(&now)
	oldAgent.SetFinalizers([]string{api.Finalizer, metav1.FinalizerDeleteDependents})
	removedApplicationFinalizer := oldAgent.DeepCopy()
	removedApplicationFinalizer.Finalizers = nil
	req = requestFor(t, admissionv1.Update, removedApplicationFinalizer, oldAgent, "system:kube-controller-manager")
	requireResponse(t, f.handler.Validate(context.Background(), req), false, "controller lifecycle finalizers")
}

func TestGarbageCollectorRejectsOtherFinalizerUpdates(t *testing.T) {
	for _, test := range []struct {
		name string
		old  []string
		new  []string
		live bool
	}{
		{name: "no finalizers"},
		{name: "no foreground finalizer", old: []string{"example.com/keep"}, new: []string{"example.com/keep"}},
		{name: "unchanged", old: []string{metav1.FinalizerDeleteDependents}, new: []string{metav1.FinalizerDeleteDependents}},
		{name: "not terminating", old: []string{metav1.FinalizerDeleteDependents}, live: true},
		{name: "added finalizer", old: []string{metav1.FinalizerDeleteDependents}, new: []string{"example.com/keep"}},
		{name: "removed another finalizer", old: []string{metav1.FinalizerDeleteDependents, "example.com/keep"}},
		{name: "reordered finalizers", old: []string{"example.com/first", metav1.FinalizerDeleteDependents, "example.com/second"},
			new: []string{"example.com/second", "example.com/first"}},
		{name: "duplicate foreground finalizers", old: []string{metav1.FinalizerDeleteDependents, metav1.FinalizerDeleteDependents}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			old := f.policy.DeepCopy()
			old.Finalizers = test.old
			if !test.live {
				now := metav1.Now()
				old.DeletionTimestamp = &now
			}
			object := old.DeepCopy()
			object.Finalizers = test.new
			req := requestFor(t, admissionv1.Update, object, old, "system:kube-controller-manager")
			requireResponse(t, f.handler.Validate(context.Background(), req), false, "administrator")
		})
	}
}

func TestLegacyTeamDeletionPreservesExistingAuthorization(t *testing.T) {
	for _, user := range []string{"portal", testController, "administrator"} {
		t.Run(user, func(t *testing.T) {
			f := newFixture(t)
			f.team.Spec = api.TeamSpec{}
			f.agent.Spec.SandboxRef = ""
			f.agent.ResourceVersion = ""
			if err := f.client.Create(context.Background(), f.agent); err != nil {
				t.Fatal(err)
			}
			req := requestFor(t, admissionv1.Delete, f.team, nil, user)
			requireResponse(t, f.handler.Validate(context.Background(), req), true, "legacy")
			f.handler.reader = unavailableReader{}
			requireResponse(t, f.handler.Validate(context.Background(), req), true, "legacy")
		})
	}
}

func TestTeamRemovalProtectsInheritedPolicy(t *testing.T) {
	f := newFixture(t)
	f.agent.Spec.SandboxRef = ""
	f.agent.ResourceVersion = ""
	f.agent.Finalizers = nil
	if err := f.client.Create(context.Background(), f.agent); err != nil {
		t.Fatal(err)
	}
	ungoverned := f.team.DeepCopy()
	ungoverned.Spec = api.TeamSpec{}
	for _, req := range []admission.Request{
		requestFor(t, admissionv1.Delete, f.team, nil, "administrator"),
		requestFor(t, admissionv1.Update, ungoverned, f.team, "administrator"),
	} {
		requireResponse(t, f.handler.Validate(context.Background(), req), false, "inherited policy")
		requireResponse(t, f.handler.Validate(context.Background(), req), false, f.agent.Name)
	}
	if err := f.client.Delete(context.Background(), f.agent); err != nil {
		t.Fatal(err)
	}
	req := requestFor(t, admissionv1.Update, ungoverned, f.team, "administrator")
	requireResponse(t, f.handler.Validate(context.Background(), req), true, "unreferenced")
}

func TestMissingCreateDefaultNamesTheMutatingWebhook(t *testing.T) {
	f := newFixture(t)
	f.agent.Spec.SandboxRef = ""
	req := requestFor(t, admissionv1.Create, f.agent, nil, "administrator")
	requireResponse(t, f.handler.Validate(context.Background(), req), false, "creation-defaulting webhook")
}

func TestUnknownVersionFieldsDoNotBypassValidationOrBlockKnownObjects(t *testing.T) {
	f := newFixture(t)
	stampFixtureAuthorization(t, f)
	for _, object := range []client.Object{f.agent, f.team, f.box, f.policy} {
		t.Run(fmt.Sprintf("%T", object), func(t *testing.T) {
			known := requestFor(t, admissionv1.Create, object, nil, "administrator")
			req := known
			var raw map[string]any
			if err := json.Unmarshal(req.Object.Raw, &raw); err != nil {
				t.Fatal(err)
			}
			spec, ok := raw["spec"].(map[string]any)
			if !ok {
				t.Fatal("fixture omitted spec")
			}
			spec["futurePolicy"] = true
			var err error
			req.Object.Raw, err = json.Marshal(raw)
			if err != nil {
				t.Fatal(err)
			}
			response := f.handler.Validate(context.Background(), req)
			requireResponse(t, response, false, `unknown field "futurePolicy"`)
			if response.Result.Code != 400 {
				t.Fatal("unrecognized policy fields must explicitly fail closed")
			}
			requireResponse(t, f.handler.Validate(context.Background(), known), true, "")
		})
	}
}

type observedReader struct {
	client.Reader
	gets []string
	list func(context.Context, client.ObjectList, ...client.ListOption) error
}

func (r *observedReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	r.gets = append(r.gets, fmt.Sprintf("%T/%s", object, key.Name))
	return r.Reader.Get(ctx, key, object, options...)
}

func (r *observedReader) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	return r.list(ctx, list, options...)
}

func TestAgentValidationReadsOnlyNamedReferencesAndRefreshesEachRequest(t *testing.T) {
	f := newFixture(t)
	stampFixtureAuthorization(t, f)
	reader := &observedReader{Reader: f.client, list: func(context.Context, client.ObjectList, ...client.ListOption) error {
		t.Fatal("Agent validation must not list any collection")
		return nil
	}}
	f.handler.reader = reader
	req := requestFor(t, admissionv1.Create, f.agent, nil, "administrator")
	requireResponse(t, f.handler.Validate(context.Background(), req), true, "")
	if !slices.Equal(reader.gets, []string{"*v1alpha1.Team/engineering", "*v1alpha1.Sandbox/standard", "*v1alpha1.AgentPolicy/normal"}) {
		t.Fatalf("unexpected reads: %v", reader.gets)
	}
	f.policy.Spec.Network.BlockedFQDNs = []string{"blocked.example.com"}
	if err := f.client.Update(context.Background(), f.policy); err != nil {
		t.Fatal(err)
	}
	requireResponse(t, f.handler.Validate(context.Background(), req), false, "incompatible")
	if len(reader.gets) != 6 {
		t.Fatal("validation reused a previous request's references")
	}
}

func TestTeamCompatibilityRejectionReturnsSafeStructuredCauses(t *testing.T) {
	f := newFixture(t)
	f.policy.Spec.Network.BlockedFQDNs = []string{"blocked.example.com"}
	if err := f.client.Update(context.Background(), f.policy); err != nil {
		t.Fatal(err)
	}
	response := f.handler.Validate(context.Background(), requestFor(t, admissionv1.Create, f.team, nil, "administrator"))
	if response.Allowed || response.Result == nil || response.Result.Details == nil {
		t.Fatalf("compatibility response = %+v", response)
	}
	var found bool
	for _, cause := range response.Result.Details.Causes {
		if cause.Type != metav1.CauseType("CetiRunCompatibility") {
			continue
		}
		var issue api.CompatibilityIssue
		if err := json.Unmarshal([]byte(cause.Message), &issue); err != nil {
			t.Fatalf("compatibility cause is not JSON: %v", err)
		}
		if !issue.Valid() || issue.Code != "network_control_unsupported" || issue.Field != "registration.blockedFQDNs" || issue.Team == nil || issue.Policy == nil || issue.Sandbox == nil {
			t.Fatalf("unsafe compatibility issue = %+v", issue)
		}
		found = true
	}
	if !found {
		t.Fatalf("compatibility causes = %+v", response.Result.Details.Causes)
	}
}

func TestConsumerPaginationChecksLaterPagesAndErrors(t *testing.T) {
	for _, scenario := range []string{"team consumers", "policy references", "sandbox selections", "shared validation", "expired page"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			pages := 0
			reader := &observedReader{Reader: f.client}
			reader.list = func(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
				opts := (&client.ListOptions{}).ApplyOptions(options)
				if opts.Limit != referencePageSize {
					t.Fatalf("unbounded reference list: %#v", opts)
				}
				if scenario == "sandbox selections" {
					if teams, ok := list.(*api.TeamList); ok {
						*teams = api.TeamList{}
						return nil
					}
				}
				pages++
				switch opts.Continue {
				case "":
					switch page := list.(type) {
					case *api.AgentList:
						*page = api.AgentList{}
					case *api.TeamList:
						*page = api.TeamList{}
					default:
						t.Fatalf("unexpected collection read: %T", list)
					}
					list.SetContinue("next-page")
				case "next-page":
					if scenario == "expired page" {
						return apierrors.NewResourceExpired("synthetic continuation expiration")
					}
					switch page := list.(type) {
					case *api.AgentList:
						*page = api.AgentList{Items: []api.Agent{*f.agent.DeepCopy()}}
					case *api.TeamList:
						*page = api.TeamList{Items: []api.Team{*f.team.DeepCopy()}}
					}
				default:
					t.Fatalf("unexpected continuation: %q", opts.Continue)
				}
				return nil
			}
			f.handler.reader = reader
			object := client.Object(f.team)
			operation := admissionv1.Delete
			var old client.Object
			want := f.agent.Name
			switch scenario {
			case "policy references", "expired page":
				object, want = f.policy, f.team.Name
			case "sandbox selections":
				object = f.box
			case "shared validation":
				old = f.policy.DeepCopy()
				f.policy.Spec.Network.BlockedFQDNs = []string{"blocked.example.com"}
				object, operation, want = f.policy, admissionv1.Update, f.team.Name
			}
			if scenario == "expired page" {
				want = "dependencies are unavailable"
			}
			req := requestFor(t, operation, object, old, "administrator")
			response := f.handler.Validate(context.Background(), req)
			requireResponse(t, response, false, want)
			if scenario == "expired page" && response.Result.Code != 503 {
				t.Fatal("expired continuation must not produce partial success")
			}
			if pages != 2 {
				t.Fatalf("expected both bounded pages, got %d", pages)
			}
		})
	}
}

func TestSharedValidationReusesOnlyRequestLocalReferences(t *testing.T) {
	f := newFixture(t)
	reader := &observedReader{Reader: f.client}
	reader.list = func(_ context.Context, list client.ObjectList, options ...client.ListOption) error {
		page, ok := list.(*api.TeamList)
		if !ok {
			t.Fatalf("unexpected collection read: %T", list)
		}
		opts := (&client.ListOptions{}).ApplyOptions(options)
		*page = api.TeamList{Items: []api.Team{*f.team.DeepCopy()}}
		if opts.Continue == "" {
			page.Continue = "next"
		} else {
			page.Items[0].Name = "second-team"
		}
		return nil
	}
	f.handler.reader = reader
	req := requestFor(t, admissionv1.Update, f.policy, f.policy.DeepCopy(), "administrator")
	requireResponse(t, f.handler.Validate(context.Background(), req), true, "")
	if !slices.Equal(reader.gets, []string{"*v1alpha1.Sandbox/standard"}) {
		t.Fatalf("shared references were not deduplicated, or candidate was fetched instead: %v", reader.gets)
	}
}

func TestAbsentQualificationFailsClosed(t *testing.T) {
	f := newFixture(t)
	handler, err := NewHandler(f.client, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range []client.Object{f.agent, f.team} {
		req := requestFor(t, admissionv1.Create, object, nil, "administrator")
		requireResponse(t, handler.Validate(context.Background(), req), false, "registrations")
	}
}

func TestTeamReferenceAndCompatibilityValidation(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*testing.T, *fixture)
		want string
	}{
		{"missing policy", func(_ *testing.T, f *fixture) { f.team.Spec.AgentPolicyRef = "missing" }, "Not found"},
		{"missing default", func(_ *testing.T, f *fixture) { f.team.Spec.DefaultSandboxRef = "missing" }, "sandboxRefs"},
		{"duplicate selections", func(_ *testing.T, f *fixture) { f.team.Spec.SandboxRefs = append(f.team.Spec.SandboxRefs, "standard") }, "Duplicate"},
		{"multiple denied by either policy", func(t *testing.T, f *fixture) {
			box := f.box.DeepCopy()
			box.Name, box.UID, box.ResourceVersion = "second", "second-uid", ""
			if err := f.client.Create(context.Background(), box); err != nil {
				t.Fatal(err)
			}
			f.team.Spec.SandboxRefs = append(f.team.Spec.SandboxRefs, box.Name)
		}, "only one"},
		{"normal grants forbidden", func(t *testing.T, f *fixture) {
			f.policy.Spec.KubernetesAccess = &api.AgentPolicyAdminKubernetesAccess{
				ServiceAccountName: "agent-admin", ClusterRoleBindings: []api.AgentPolicyClusterRoleBinding{{ClusterRoleName: "read-only"}},
			}
			if err := f.client.Update(context.Background(), f.policy); err != nil {
				t.Fatal(err)
			}
		}, "administrative Kubernetes access"},
		{"unsupported optional FQDN", func(t *testing.T, f *fixture) {
			f.policy.Spec.Network.BlockedFQDNs = []string{"blocked.example.com"}
			if err := f.client.Update(context.Background(), f.policy); err != nil {
				t.Fatal(err)
			}
		}, "incompatible"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			test.edit(t, &f)
			req := requestFor(t, admissionv1.Create, f.team, nil, "administrator")
			requireResponse(t, f.handler.Validate(context.Background(), req), false, test.want)
		})
	}
	t.Run("candidate Team has no UID yet", func(t *testing.T) {
		f := newFixture(t)
		f.team.UID, f.team.ResourceVersion = "", ""
		req := requestFor(t, admissionv1.Create, f.team, nil, "administrator")
		requireResponse(t, f.handler.Validate(context.Background(), req), true, "")
	})
}

func TestTeamPolicyReferenceBlocksDeletionWithoutCoordination(t *testing.T) {
	fixture := newFixture(t)
	if fixture.handler.coordination != nil {
		t.Fatal("test requires configuration coordination to be disabled")
	}
	var agents api.AgentList
	if err := fixture.client.List(context.Background(), &agents); err != nil {
		t.Fatal(err)
	}
	if len(agents.Items) != 0 {
		t.Fatal("Team policy protection must not depend on Agent consumers")
	}
	request := requestFor(t, admissionv1.Delete, fixture.policy, nil, "administrator")
	requireResponse(t, fixture.handler.Validate(context.Background(), request), false, "still referenced by Team")
}

func TestReferencingConsumersBlockInvalidation(t *testing.T) {
	f := newFixture(t)
	f.agent.ResourceVersion = ""
	if err := f.client.Create(context.Background(), f.agent); err != nil {
		t.Fatal(err)
	}
	for _, object := range []client.Object{f.team, f.box, f.policy} {
		req := requestFor(t, admissionv1.Delete, object, nil, "administrator")
		requireResponse(t, f.handler.Validate(context.Background(), req), false, "")
	}
	oldPolicy := f.policy.DeepCopy()
	f.policy.Spec.Network.BlockedFQDNs = []string{"blocked.example.com"}
	req := requestFor(t, admissionv1.Update, f.policy, oldPolicy, "administrator")
	requireResponse(t, f.handler.Validate(context.Background(), req), false, "referencing Team")

	team := f.team.DeepCopy()
	team.Spec = api.TeamSpec{}
	req = requestFor(t, admissionv1.Update, team, f.team, "administrator")
	requireResponse(t, f.handler.Validate(context.Background(), req), false, "referencing Agent")
}

func TestConfigurationUpdatesCheckEveryReferencingTeam(t *testing.T) {
	f := newFixture(t)
	other := f.team.DeepCopy()
	other.Name, other.UID, other.ResourceVersion = "other-team", "other-team-uid", ""
	other.Spec.AgentPolicyRef = f.policy.Name
	if err := f.client.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	candidate := f.policy.DeepCopy()
	candidate.Spec.KubernetesAccess = &api.AgentPolicyAdminKubernetesAccess{
		ServiceAccountName: "agent-admin", ClusterRoleBindings: []api.AgentPolicyClusterRoleBinding{{ClusterRoleName: "read-only"}},
	}
	req := requestFor(t, admissionv1.Update, candidate, f.policy, "administrator")
	requireResponse(t, f.handler.Validate(context.Background(), req), false, "referencing Team")
}

func TestPolicyCannotBeClearedIntoLegacyExecution(t *testing.T) {
	for _, held := range []bool{false, true} {
		t.Run(fmt.Sprintf("held=%v", held), func(t *testing.T) {
			f := newFixture(t)
			old := f.agent.DeepCopy()
			old.Spec.SandboxRef = ""
			if held {
				old.Finalizers = append(old.Finalizers, sandbox.HoldFinalizer)
			}
			candidate := old.DeepCopy()
			candidate.Spec.Team = ""
			req := requestFor(t, admissionv1.Update, candidate, old, "administrator")
			requireResponse(t, f.handler.Validate(context.Background(), req), false, "")
		})
	}
}

type unavailableReader struct{}

func (unavailableReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return fmt.Errorf("synthetic API unavailability")
}

func (unavailableReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return fmt.Errorf("synthetic API unavailability")
}

func TestDependencyFailureAndCleanupBoundary(t *testing.T) {
	f := newFixture(t)
	f.handler.reader = unavailableReader{}
	req := requestFor(t, admissionv1.Create, f.agent, nil, "administrator")
	response := f.handler.Validate(context.Background(), req)
	requireResponse(t, response, false, "dependencies are unavailable")
	if response.Result.Code != 503 {
		t.Fatal("dependency failures must not look like configuration validity")
	}
	old := f.agent.DeepCopy()
	old.Finalizers = append(old.Finalizers, sandbox.HoldFinalizer, sandbox.PreparationFinalizer, sandbox.BootstrapFinalizer, "example.com/other-controller")
	now := metav1.Now()
	old.DeletionTimestamp = &now
	candidate := old.DeepCopy()
	candidate.Finalizers = []string{"example.com/other-controller"}
	req = requestFor(t, admissionv1.Update, candidate, old, testController)
	requireResponse(t, f.handler.Validate(context.Background(), req), true, "lifecycle metadata")
	for _, edit := range []func(*api.Agent){
		func(a *api.Agent) { a.Spec.Model = "changed" },
		func(a *api.Agent) { a.Labels = map[string]string{"changed": "true"} },
		func(a *api.Agent) { a.Annotations = map[string]string{"changed": "true"} },
		func(a *api.Agent) { a.Status.Phase = api.PhaseReady },
		func(a *api.Agent) { a.Finalizers = nil },
		func(a *api.Agent) {
			a.Finalizers = append(a.Finalizers, "cetirun.microsoft.com/custom-runtime-ownership")
		},
	} {
		changed := candidate.DeepCopy()
		edit(changed)
		req = requestFor(t, admissionv1.Update, changed, old, testController)
		requireResponse(t, f.handler.Validate(context.Background(), req), false, "")
	}
	req = requestFor(t, admissionv1.Update, candidate, old, "administrator")
	requireResponse(t, f.handler.Validate(context.Background(), req), false, "finalizers")
	req = requestFor(t, admissionv1.Delete, old, nil, "member")
	requireResponse(t, f.handler.Validate(context.Background(), req), true, "deletion")
	req = requestFor(t, admissionv1.Update, candidate, old, testController)
	req.SubResource = "status"
	requireResponse(t, f.handler.Validate(context.Background(), req), true, "RBAC")
}

func TestControllerCanPersistHoldWithoutReferenceLookups(t *testing.T) {
	f := newFixture(t)
	f.handler.reader = unavailableReader{}
	candidate := f.agent.DeepCopy()
	candidate.Finalizers = append(candidate.Finalizers, sandbox.HoldFinalizer)
	req := requestFor(t, admissionv1.Update, candidate, f.agent, testController)
	requireResponse(t, f.handler.Validate(context.Background(), req), true, "")
	req = requestFor(t, admissionv1.Update, f.agent, candidate, testController)
	requireResponse(t, f.handler.Validate(context.Background(), req), false, "finalizers")
}

func TestControllerPreparationFinalizerBoundary(t *testing.T) {
	for _, finalizer := range []string{sandbox.PreparationFinalizer, sandbox.BootstrapFinalizer} {
		t.Run(finalizer, func(t *testing.T) {
			f := newFixture(t)
			f.handler.reader = unavailableReader{}
			candidate := f.agent.DeepCopy()
			candidate.Finalizers = append(candidate.Finalizers, finalizer)
			requireResponse(t, f.handler.Validate(context.Background(),
				requestFor(t, admissionv1.Update, candidate, f.agent, testController)), true, "lifecycle metadata")
			for _, user := range []string{"administrator", "member"} {
				requireResponse(t, f.handler.Validate(context.Background(),
					requestFor(t, admissionv1.Update, candidate, f.agent, user)), false, "finalizers")
			}
			requireResponse(t, f.handler.Validate(context.Background(),
				requestFor(t, admissionv1.Update, f.agent, candidate, testController)), false, "finalizers")
			changed := candidate.DeepCopy()
			changed.Spec.Model = "different-model"
			requireResponse(t, f.handler.Validate(context.Background(),
				requestFor(t, admissionv1.Update, changed, f.agent, testController)), false, "")
			requireResponse(t, f.handler.Validate(context.Background(),
				requestFor(t, admissionv1.Create, candidate, nil, "administrator")), false, "markers")
		})
	}
}

func TestMalformedAdmissionRequestsAreRejected(t *testing.T) {
	f := newFixture(t)
	for _, edit := range []func(*admission.Request){
		func(r *admission.Request) { r.Resource.Resource = "unknown" },
		func(r *admission.Request) { r.Namespace = "unexpected" },
		func(r *admission.Request) { r.Kind.Kind = "Team" },
		func(r *admission.Request) { r.Object.Raw = []byte(`{"metadata":{"name":"wrong"}}`) },
		func(r *admission.Request) {
			r.Object.Raw = []byte(`{"metadata":{"name":"alice-work"},"spec":{"unknown":true}}`)
		},
		func(r *admission.Request) { r.Object.Raw = []byte(`{} {}`) },
		func(r *admission.Request) { r.Operation = admissionv1.Connect },
	} {
		req := requestFor(t, admissionv1.Create, f.agent, nil, "administrator")
		edit(&req)
		requireResponse(t, f.handler.Validate(context.Background(), req), false, "")
	}
}

func TestAdmissionConfigValidation(t *testing.T) {
	for _, raw := range []string{
		`{}`, `null`, `{"adminUsers":["administrator"],"controllerUsername":"*","unknown":1}`,
		`{"adminUsers":["administrator"],"controllerUsername":"controller"}`,
		`{"adminGroups":["system:authenticated"],"controllerUsername":"system:serviceaccount:test:controller"}`,
		`{"adminUsers":[""],"controllerUsername":"system:serviceaccount:test:controller"}`,
		`{"adminUsers":["administrator"],"controllerUsername":"system:serviceaccount:test:controller"} {}`,
	} {
		if _, err := ParseConfig(raw); err == nil {
			t.Fatalf("accepted invalid config: %s", raw)
		}
	}
	config, err := ParseConfig(`{"adminUsers":["administrator","system:serviceaccount:test:portal"],"controllerUsername":"system:serviceaccount:test:controller"}`)
	if err != nil || !slices.Equal(config.AdminUsers, []string{"administrator", "system:serviceaccount:test:portal"}) {
		t.Fatalf("valid config rejected: %v", err)
	}
}
