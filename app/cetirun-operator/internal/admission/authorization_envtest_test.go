// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package sandboxadmission

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	operatorcontroller "github.com/Azure/CetiRun/app/cetirun-operator/internal/controller"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
	admissionv1 "k8s.io/api/admission/v1"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// This harness exercises the rendered chart's create/update authorization
// protocol with real authentication, RBAC, admission ordering and persistence.
// Only the webhook transport is redirected to its loopback test server.
func TestAuthorizationAPIServerBindings(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	for _, binary := range []string{"kube-apiserver", "etcd"} {
		info, err := os.Stat(filepath.Join(assets, binary))
		if assets == "" || err != nil || info.IsDir() || info.Mode()&0111 == 0 {
			t.Fatal("set KUBEBUILDER_ASSETS to preinstalled Kubernetes 1.37 envtest binaries")
		}
	}
	env := &envtest.Environment{
		BinaryAssetsDirectory: assets, UseExistingCluster: ptr.To(false),
		CRDDirectoryPaths: []string{"../../config/crd/bases"}, ErrorIfCRDPathMissing: true,
		ControlPlaneStartTimeout: 45 * time.Second, ControlPlaneStopTimeout: 30 * time.Second,
	}
	env.ControlPlane.GetAPIServer().Configure().Set("authorization-mode", "RBAC")
	cfg, err := env.Start()
	if err != nil {
		_ = env.Stop()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{api.AddToScheme, corev1.AddToScheme, rbacv1.AddToScheme, admissionregistrationv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	newClient := func(config *rest.Config) client.Client {
		cl, err := client.New(config, client.Options{Scheme: scheme})
		if err != nil {
			t.Fatal(err)
		}
		return cl
	}
	admin := newClient(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	create := func(object client.Object) {
		t.Helper()
		if err := admin.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	userClient := func(name string, groups ...string) client.Client {
		user, err := env.AddUser(envtest.User{Name: name, Groups: groups}, cfg)
		if err != nil {
			t.Fatal(err)
		}
		role := &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "authorization-test-"},
			Rules: []rbacv1.PolicyRule{{
				APIGroups: []string{api.GroupVersion.Group}, Resources: []string{"agents"},
				Verbs: []string{"get", "list", "create", "update", "patch", "delete"},
			}},
		}
		if name == testController {
			role.Rules = append(role.Rules,
				rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"namespaces"}, Verbs: []string{"get"}},
				rbacv1.PolicyRule{APIGroups: []string{api.GroupVersion.Group}, Resources: []string{"agents/status"}, Verbs: []string{"get", "update", "patch"}},
			)
		}
		create(role)
		create(&rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: role.Name},
			Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: name}},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		})
		return newClient(user.Config())
	}
	member := userClient("assigned-member")
	groupMember := userClient("assigned-group-member", "engineering-members")
	outsider := userClient("unassigned-member")
	conflicted := userClient("conflicting-member", "engineering-members", "other-members")
	reauthorizer := userClient("administrator")
	controller := userClient(testController)
	f := newFixture(t)
	for _, object := range []client.Object{f.box, f.policy, f.team} {
		object.SetUID("")
		object.SetResourceVersion("")
		create(object)
	}
	recoveries := []struct {
		name     string
		missing  []string
		reselect bool
		fixture
	}{
		{name: "replace-all", missing: []string{"Team", "Sandbox", "AgentPolicy"}},
		{name: "restore-sandbox", missing: []string{"Sandbox"}},
		{name: "reselect-team", missing: []string{"Team"}, reselect: true},
		{name: "reselect-policy", missing: []string{"AgentPolicy"}, reselect: true},
	}
	// Seed a restore state before admission enrollment. Active admission forbids
	// deleting referenced resources; every repair below uses the rendered hooks.
	for i := range recoveries {
		recovery := &recoveries[i]
		recovery.box = f.box.DeepCopy()
		recovery.box.ObjectMeta = metav1.ObjectMeta{Name: recovery.name + "-box"}
		recovery.policy = f.policy.DeepCopy()
		recovery.policy.ObjectMeta = metav1.ObjectMeta{Name: recovery.name + "-policy"}
		recovery.team = f.team.DeepCopy()
		recovery.team.ObjectMeta = metav1.ObjectMeta{Name: recovery.name + "-team"}
		recovery.team.Spec.SandboxRefs = []string{recovery.box.Name}
		recovery.team.Spec.DefaultSandboxRef = recovery.box.Name
		recovery.team.Spec.AgentPolicyRef = recovery.policy.Name
		for _, object := range []client.Object{recovery.box, recovery.policy, recovery.team} {
			create(object)
		}
		recovery.agent = f.agent.DeepCopy()
		recovery.agent.ObjectMeta = metav1.ObjectMeta{Name: recovery.name + "-agent", Finalizers: []string{api.Finalizer}}
		recovery.agent.Spec.Team, recovery.agent.Spec.SandboxRef = recovery.team.Name, recovery.box.Name
		stampFixtureAuthorization(t, recovery.fixture)
		create(recovery.agent)
		binding, err := sandboxbinding.AuthorizedBinding(recovery.agent)
		if err != nil {
			t.Fatal(err)
		}
		request, err := sandboxbinding.FormatReauthorization("before-restore", binding)
		if err != nil {
			t.Fatal(err)
		}
		recovery.agent.Annotations[sandboxbinding.ReauthorizeAnnotation] = request
		if err := admin.Update(ctx, recovery.agent, client.FieldOwner("restored-authorization")); err != nil {
			t.Fatal(err)
		}
		objects := map[string]client.Object{"Team": recovery.team, "Sandbox": recovery.box, "AgentPolicy": recovery.policy}
		for _, kind := range recovery.missing {
			object := objects[kind]
			if err := admin.Delete(ctx, object); err != nil {
				t.Fatal(err)
			}
			if err := admin.Get(ctx, client.ObjectKeyFromObject(object), object); !apierrors.IsNotFound(err) {
				t.Fatalf("restore fixture still has its original %s: %v", kind, err)
			}
		}
	}
	config := testConfig()
	config.AdminGroups = []string{"system:masters"}
	config.TeamAssignments = []TeamAssignment{
		{Team: f.team.Name, Users: []string{"assigned-member"}, Groups: []string{"engineering-members"}},
		{Team: "other-team", Groups: []string{"other-members"}},
	}
	handler, err := NewHandler(admin, config)
	if err != nil {
		t.Fatal(err)
	}
	handler.registrations = f.handler.registrations
	var updateMutations atomic.Int64
	mux := http.NewServeMux()
	mux.Handle(DefaultingPath, &admission.Webhook{Handler: admission.HandlerFunc(func(ctx context.Context, request admission.Request) admission.Response {
		if request.Operation == admissionv1.Update {
			updateMutations.Add(1)
		}
		return handler.DefaultAgent(ctx, request)
	})})
	mux.Handle(ValidationPath, &admission.Webhook{Handler: admission.HandlerFunc(handler.Validate)})
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	validating, mutating := renderedWebhooks(t, ca)
	redirect := func(config *admissionregistrationv1.WebhookClientConfig) {
		t.Helper()
		if config.Service == nil || config.Service.Path == nil {
			t.Fatal("rendered admission webhook omitted its Service path")
		}
		config.URL = ptr.To(server.URL + *config.Service.Path)
		config.Service = nil
	}
	for i := range validating.Webhooks {
		redirect(&validating.Webhooks[i].ClientConfig)
	}
	for i := range mutating.Webhooks {
		hook := &mutating.Webhooks[i]
		if hook.ReinvocationPolicy == nil || *hook.ReinvocationPolicy != admissionregistrationv1.NeverReinvocationPolicy {
			t.Fatal("packaged authorization mutation must explicitly disable reinvocation")
		}
		redirect(&hook.ClientConfig)
	}
	create(validating)
	create(mutating)
	newAgent := func(name string) *api.Agent {
		agent := f.agent.DeepCopy()
		agent.ObjectMeta = metav1.ObjectMeta{Name: name, Finalizers: []string{api.Finalizer}}
		agent.Spec.Team, agent.Spec.SandboxRef = "", ""
		return agent
	}
	if err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		probe := newAgent("authorization-ready")
		err := member.Create(ctx, probe, client.DryRunAll)
		return err == nil && probe.Annotations[sandboxbinding.AuthorizationAnnotation] != "", nil
	}); err != nil {
		t.Fatal("authorization admission did not become active:", err)
	}
	denied := func(err error) {
		t.Helper()
		if !apierrors.IsForbidden(err) {
			t.Fatalf("expected admission denial despite broad main-resource RBAC, got %v", err)
		}
	}
	t.Run("trusted user and group creation persists normal authorization with generated names", func(t *testing.T) {
		for _, writer := range []client.Client{member, groupMember} {
			agent := newAgent("")
			agent.GenerateName = "assigned-agent-"
			agent.Labels = map[string]string{"role": "admin", "team": "caller-team"}
			if err := writer.Create(ctx, agent); err != nil {
				t.Fatal(err)
			}
			stored, binding, err := sandboxbinding.Read(ctx, admin, client.ObjectKeyFromObject(agent))
			if err != nil || !strings.HasPrefix(agent.Name, agent.GenerateName) || binding.AgentUID != agent.UID ||
				binding.TeamUID != f.team.UID || binding.SandboxUID != f.box.UID || binding.AgentPolicyUID != f.policy.UID {
				t.Fatalf("API-assigned identity or current reference authorization was not durable: %#v, %v", binding, err)
			}
			if stored.Spec.Team != f.team.Name ||
				stored.Spec.SandboxRef != f.box.Name || stored.Status.Sandbox != nil {
				t.Fatal("creation defaulting promoted the workload or claimed enforcement")
			}
			stored.Spec.Model = "member-application-model"
			if err := writer.Update(ctx, stored); err != nil {
				t.Fatal("assigned member lost ordinary application updates:", err)
			}
			for _, edit := range []func(*api.Agent){
				func(a *api.Agent) { a.Spec.Team = "other-team" },
				func(a *api.Agent) { a.Spec.SandboxRef = "other-box" },
				func(a *api.Agent) { delete(a.Annotations, sandboxbinding.AuthorizationAnnotation) },
				func(a *api.Agent) { a.Annotations[sandboxbinding.AuthorizationAnnotation] = "forged" },
				func(a *api.Agent) {
					a.Annotations[sandboxbinding.ReauthorizeAnnotation] = `{"requestID":"member-request"}`
				},
			} {
				candidate := stored.DeepCopy()
				edit(candidate)
				denied(writer.Update(ctx, candidate))
			}
			denied(outsider.Update(ctx, stored.DeepCopy()))
		}
	})
	t.Run("caller context and ambiguous memberships cannot choose authority", func(t *testing.T) {
		for name, writer := range map[string]client.Client{"outsider": outsider, "conflicted": conflicted} {
			agent := newAgent("denied-" + name)
			agent.Spec.Team = f.team.Name
			agent.Labels = map[string]string{"team": f.team.Name, "role": "admin"}
			err := writer.Create(ctx, agent)
			denied(err)
			if name == "conflicted" && !strings.Contains(err.Error(), errTeamAssignmentConflict.Error()) {
				t.Fatalf("API-server denial omitted the stable assignment guidance: %v", err)
			}
		}
		for name, edit := range map[string]func(*api.Agent){
			"team":  func(a *api.Agent) { a.Spec.Team = "caller-team" },
			"record": func(a *api.Agent) {
				a.Annotations = map[string]string{sandboxbinding.AuthorizationAnnotation: fixtureAuthorization(t, f)}
			},
			"request": func(a *api.Agent) { a.Annotations = map[string]string{sandboxbinding.ReauthorizeAnnotation: ""} },
		} {
			agent := newAgent("forged-" + name)
			edit(agent)
			denied(member.Create(ctx, agent))
		}
	})
	t.Run("creation dry run persists nothing and cannot be replayed", func(t *testing.T) {
		agent := newAgent("dry-run-authorization")
		if err := member.Create(ctx, agent, client.DryRunAll); err != nil {
			t.Fatal(err)
		}
		if _, _, err := sandboxbinding.Read(ctx, admin, client.ObjectKeyFromObject(agent)); !apierrors.IsNotFound(err) {
			t.Fatalf("dry-run authorization became durable: %v", err)
		}
		agent.UID, agent.ResourceVersion = "", ""
		denied(member.Create(ctx, agent))
		if err := member.Create(ctx, newAgent(agent.Name)); err != nil {
			t.Fatal("fresh create after dry run failed:", err)
		}
	})
	t.Run("same-name policy replacement before first reconcile requires exact admin reauthorization", func(t *testing.T) {
		policy, alternate := f.policy.DeepCopy(), f.policy.DeepCopy()
		policy.ObjectMeta = metav1.ObjectMeta{Name: "replacement-policy"}
		alternate.ObjectMeta = metav1.ObjectMeta{Name: "alternate-policy"}
		create(policy)
		create(alternate)
		team := f.team.DeepCopy()
		team.ObjectMeta = metav1.ObjectMeta{Name: "replacement-team"}
		team.Spec.AgentPolicyRef = policy.Name
		create(team)
		agent := newAgent("replacement-agent")
		agent.Spec.Team = team.Name
		create(agent)
		original, err := sandboxbinding.AuthorizedBinding(agent)
		if err != nil || agent.Status.Sandbox != nil {
			t.Fatalf("initial authorization missing before first apply: %v", err)
		}
		denied(admin.Delete(ctx, team.DeepCopy()))
		denied(admin.Delete(ctx, f.box.DeepCopy()))
		denied(admin.Delete(ctx, policy.DeepCopy()))
		// An authorized slot change permits deleting the now-unreferenced policy.
		// Re-selecting its same-name replacement must not renew Agent authority.
		team.Spec.AgentPolicyRef = alternate.Name
		if err := admin.Update(ctx, team); err != nil {
			t.Fatal(err)
		}
		if err := admin.Delete(ctx, policy); err != nil {
			t.Fatal(err)
		}
		policy.ObjectMeta = metav1.ObjectMeta{Name: policy.Name}
		create(policy)
		if policy.UID == original.AgentPolicyUID {
			t.Fatal("API server did not assign a replacement policy UID")
		}
		team.Spec.AgentPolicyRef = policy.Name
		if err := admin.Update(ctx, team); err != nil {
			t.Fatal(err)
		}
		candidate := agent.DeepCopy()
		candidate.Spec.Model = "unrelated-update"
		denied(admin.Update(ctx, candidate))
		unchanged, binding, err := sandboxbinding.Read(ctx, admin, client.ObjectKeyFromObject(agent))
		if err != nil || binding != original || unchanged.Status.Sandbox != nil {
			t.Fatalf("replacement silently renewed pending authorization: %v, %v", binding, err)
		}
		target := sandbox.Binding{AgentUID: agent.UID, TeamUID: team.UID, SandboxUID: f.box.UID, AgentPolicyUID: policy.UID}
		request, err := sandboxbinding.FormatReauthorization("replace-policy-1", target)
		if err != nil {
			t.Fatal(err)
		}
		candidate = unchanged.DeepCopy()
		candidate.Annotations[sandboxbinding.ReauthorizeAnnotation] = request
		denied(member.Update(ctx, candidate.DeepCopy()))
		stale := candidate.DeepCopy()
		stale.Annotations[sandboxbinding.ReauthorizeAnnotation] = strings.Replace(request, string(policy.UID), string(original.AgentPolicyUID), 1)
		denied(admin.Update(ctx, stale))
		wrongAgent := candidate.DeepCopy()
		wrongAgent.Annotations[sandboxbinding.ReauthorizeAnnotation] = strings.Replace(request, string(agent.UID), "another-agent-uid", 1)
		denied(admin.Update(ctx, wrongAgent))
		dryRun := candidate.DeepCopy()
		if err := admin.Update(ctx, dryRun, client.DryRunAll); err != nil {
			t.Fatal("explicit dry-run reauthorization failed:", err)
		}
		_, afterDryRun, err := sandboxbinding.Read(ctx, admin, client.ObjectKeyFromObject(agent))
		if err != nil || afterDryRun != original {
			t.Fatal("dry-run reauthorization changed durable authority")
		}
		beforeMutation := updateMutations.Load()
		if err := admin.Update(ctx, candidate); err != nil {
			t.Fatal("explicit reauthorization failed:", err)
		}
		if updateMutations.Load() != beforeMutation+1 {
			t.Fatal("metadata-only administrator reauthorization did not invoke the packaged mutating webhook exactly once")
		}
		stored, actual, err := sandboxbinding.Read(ctx, admin, client.ObjectKeyFromObject(agent))
		if err != nil || actual != target || stored.Status.Sandbox != nil || stored.Generation != agent.Generation {
			t.Fatalf("metadata-only reauthorization was not persisted independently of status/generation: %v, %v", actual, err)
		}
		replay := stored.DeepCopy()
		replay.Annotations[sandboxbinding.ReauthorizeAnnotation] += " "
		denied(admin.Update(ctx, replay))
		stored.Spec.Model = "ordinary-admin-update"
		if err := admin.Update(ctx, stored); err != nil {
			t.Fatal("reauthorized Agent could not accept an ordinary update:", err)
		}
	})
	t.Run("changed Team defaults never reselect an existing Agent", func(t *testing.T) {
		box := f.box.DeepCopy()
		box.ObjectMeta = metav1.ObjectMeta{Name: "second-default"}
		create(box)
		policy := f.policy.DeepCopy()
		policy.ObjectMeta = metav1.ObjectMeta{Name: "multiple-selection"}
		policy.Spec.SandboxSelection.AllowMultiple = true
		create(policy)
		team := f.team.DeepCopy()
		team.ObjectMeta = metav1.ObjectMeta{Name: "changing-default-team"}
		team.Spec.SandboxRefs = []string{f.box.Name, box.Name}
		team.Spec.AgentPolicyRef = policy.Name
		create(team)
		agent := newAgent("persisted-default-agent")
		agent.Spec.Team = team.Name
		create(agent)
		before, err := sandboxbinding.AuthorizedBinding(agent)
		if err != nil {
			t.Fatal(err)
		}
		team.Spec.DefaultSandboxRef = box.Name
		if err := admin.Update(ctx, team); err != nil {
			t.Fatal(err)
		}
		agent.Spec.Model = "default-preserving-update"
		if err := admin.Update(ctx, agent); err != nil {
			t.Fatal(err)
		}
		after, err := sandboxbinding.AuthorizedBinding(agent)
		if err != nil || before != after || agent.Spec.SandboxRef != f.box.Name {
			t.Fatal("an update re-applied a changed Team default")
		}
		fresh := newAgent("new-default-agent")
		fresh.Spec.Team = team.Name
		create(fresh)
		if fresh.Spec.SandboxRef != box.Name {
			t.Fatal("new creation did not use the new Team default")
		}
		agent.Spec.SandboxRef = ""
		if err := admin.Update(ctx, agent); !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "existing Agents are never re-defaulted") {
			t.Fatalf("served admission did not protect the existing Sandbox binding: %v", err)
		}
	})
	t.Run("missing references recover without clearing protected authority or the execution hold", func(t *testing.T) {
		for _, recovery := range recoveries {
			t.Run(recovery.name, func(t *testing.T) {
				key := client.ObjectKeyFromObject(recovery.agent)
				original, err := sandboxbinding.AuthorizedBinding(recovery.agent)
				if err != nil {
					t.Fatal(err)
				}
				readHeld := func(expected sandbox.Binding, record, request string) *api.Agent {
					t.Helper()
					stored, binding, err := sandboxbinding.Read(ctx, admin, key)
					if err != nil || binding != expected || stored.Annotations[sandboxbinding.AuthorizationAnnotation] != record ||
						stored.Annotations[sandboxbinding.ReauthorizeAnnotation] != request {
						t.Fatalf("durable authorization changed unexpectedly: binding=%v, err=%v", binding, err)
					}
					if !slices.Contains(stored.Finalizers, sandbox.HoldFinalizer) || !sandbox.Requested(stored, nil) || stored.Status.Sandbox != nil {
						t.Fatal("recovery lost the sticky hold or claimed enforcement")
					}
					return stored
				}
				held := recovery.agent.DeepCopy()
				held.Finalizers = append(held.Finalizers, sandbox.HoldFinalizer)
				requireAuthorizationDenied(t, reauthorizer.Update(ctx, held.DeepCopy()))
				requireAuthorizationDenied(t, member.Update(ctx, held.DeepCopy()))
				smuggled := held.DeepCopy()
				delete(smuggled.Annotations, sandboxbinding.AuthorizationAnnotation)
				requireAuthorizationDenied(t, controller.Update(ctx, smuggled))
				if err := controller.Update(ctx, held); err != nil {
					t.Fatal("exact controller finalizer-only update could not persist a hold with missing references:", err)
				}
				oldRecord, oldRequest := held.Annotations[sandboxbinding.AuthorizationAnnotation], held.Annotations[sandboxbinding.ReauthorizeAnnotation]
				before := readHeld(original, oldRecord, oldRequest)
				team, err := handler.team(ctx, before.Spec.Team)
				if err != nil {
					t.Fatal(err)
				}
				resources := sandbox.Resources{}
				if team != nil {
					resources, err = handler.resources(ctx, team.Spec, resources)
					if err != nil {
						t.Fatal(err)
					}
				}
				if _, err := sandbox.Resolve(sandbox.ResolveInput{
					Agent: before, Team: team, Resources: resources, Registrations: handler.registrations, AuthorizedBinding: original,
				}); err == nil {
					t.Fatal("missing references resolved as an executable configuration")
				}
				request, err := sandboxbinding.FormatReauthorization("repair-before-references", original)
				if err != nil {
					t.Fatal(err)
				}
				for _, edit := range []func(*api.Agent){
					func(a *api.Agent) { a.Spec.Model = "ordinary-update" },
					func(a *api.Agent) { a.Annotations[sandboxbinding.ReauthorizeAnnotation] = request },
					func(a *api.Agent) { delete(a.Annotations, sandboxbinding.AuthorizationAnnotation) },
					func(a *api.Agent) { a.Annotations[sandboxbinding.AuthorizationAnnotation] = "caller-authored" },
					func(a *api.Agent) { delete(a.Annotations, sandboxbinding.ReauthorizeAnnotation) },
					func(a *api.Agent) { a.Annotations = nil },
					func(a *api.Agent) {
						delete(a.Annotations, sandboxbinding.AuthorizationAnnotation)
						a.Annotations[sandboxbinding.ReauthorizeAnnotation] = request
					},
				} {
					candidate := before.DeepCopy()
					edit(candidate)
					requireAuthorizationDenied(t, reauthorizer.Update(ctx, candidate))
					readHeld(original, oldRecord, oldRequest)
				}
				candidate := before.DeepCopy()
				if recovery.reselect {
					if slices.Contains(recovery.missing, "Team") {
						candidate.Spec.Team, candidate.Spec.SandboxRef = f.team.Name, f.box.Name
					} else {
						recovery.team.Spec.AgentPolicyRef = f.policy.Name
						if err := admin.Update(ctx, recovery.team); err != nil {
							t.Fatal("could not repair the missing policy by selecting a live policy:", err)
						}
					}
				} else {
					for _, item := range []struct {
						kind   string
						object client.Object
					}{{"Sandbox", recovery.box}, {"AgentPolicy", recovery.policy}, {"Team", recovery.team}} {
						if !slices.Contains(recovery.missing, item.kind) {
							continue
						}
						item.object.SetUID("")
						item.object.SetResourceVersion("")
						item.object.SetManagedFields(nil)
						create(item.object)
					}
				}
				requireAuthorizationDenied(t, reauthorizer.Update(ctx, candidate.DeepCopy()))
				readHeld(original, oldRecord, oldRequest)
				requestReauthorization(t, handler, candidate, "repair-"+recovery.name)
				intent, err := sandboxbinding.ParseReauthorization(candidate.Annotations[sandboxbinding.ReauthorizeAnnotation])
				if err != nil {
					t.Fatal(err)
				}
				target := intent.Binding
				if target == original || target.AgentUID != original.AgentUID {
					t.Fatal("repair must change reference identity without replacing the Agent")
				}
				if recovery.name == "replace-all" &&
					(target.TeamUID == original.TeamUID || target.SandboxUID == original.SandboxUID || target.AgentPolicyUID == original.AgentPolicyUID) {
					t.Fatal("simultaneous replacement did not assign new Team, Sandbox and AgentPolicy UIDs")
				}
				team, err = handler.team(ctx, candidate.Spec.Team)
				if err != nil {
					t.Fatal(err)
				}
				resources, err = handler.resources(ctx, team.Spec, sandbox.Resources{})
				if err != nil {
					t.Fatal(err)
				}
				input := sandbox.ResolveInput{
					Agent: candidate, Team: team, Resources: resources, Registrations: handler.registrations, AuthorizedBinding: original,
				}
				if _, err := sandbox.Resolve(input); !errors.Is(err, sandbox.ErrUnauthorizedBinding) {
					t.Fatalf("restoring or reselecting references implicitly renewed authorization: %v", err)
				}
				requireAuthorizationDenied(t, member.Update(ctx, candidate.DeepCopy()))
				requireAuthorizationDenied(t, controller.Update(ctx, candidate.DeepCopy()))
				stale := candidate.DeepCopy()
				stale.Annotations[sandboxbinding.ReauthorizeAnnotation] = request
				requireAuthorizationDenied(t, reauthorizer.Update(ctx, stale))
				readHeld(original, oldRecord, oldRequest)
				if recovery.name == "replace-all" {
					patch, marshalErr := json.Marshal(map[string]any{
						"apiVersion": api.GroupVersion.String(), "kind": "Agent",
						"metadata": map[string]any{"name": candidate.Name, "annotations": map[string]string{
							sandboxbinding.ReauthorizeAnnotation: candidate.Annotations[sandboxbinding.ReauthorizeAnnotation],
						}},
					})
					if marshalErr != nil {
						t.Fatal(marshalErr)
					}
					err = reauthorizer.Patch(ctx, candidate.DeepCopy(), client.RawPatch(types.ApplyPatchType, patch), client.FieldOwner("binding-recovery"))
					if !apierrors.IsConflict(err) {
						t.Fatalf("reauthorization should initially conflict with the restored request's field manager: %v", err)
					}
					requireAuthorizationDenied(t, member.Patch(ctx, candidate.DeepCopy(), client.RawPatch(types.ApplyPatchType, patch),
						client.FieldOwner("binding-recovery"), client.ForceOwnership))
					readHeld(original, oldRecord, oldRequest)
					err = reauthorizer.Patch(ctx, candidate, client.RawPatch(types.ApplyPatchType, patch),
						client.FieldOwner("binding-recovery"), client.ForceOwnership)
				} else {
					err = reauthorizer.Update(ctx, candidate)
				}
				if err != nil {
					t.Fatal("explicit current-UID administrator reauthorization failed:", err)
				}
				stored := readHeld(target, candidate.Annotations[sandboxbinding.AuthorizationAnnotation], candidate.Annotations[sandboxbinding.ReauthorizeAnnotation])
				if sameAgentSelection(stored, before) && stored.Generation != before.Generation {
					t.Fatal("metadata-only recovery unexpectedly changed the Agent generation")
				}
				input.Agent, input.AuthorizedBinding = stored, target
				if _, err := sandbox.Resolve(input); err != nil {
					t.Fatal("repaired authorization did not resolve with current references:", err)
				}
				for _, edit := range []func(*api.Agent){
					func(a *api.Agent) { a.Annotations[sandboxbinding.AuthorizationAnnotation] = oldRecord },
					func(a *api.Agent) { a.Annotations[sandboxbinding.ReauthorizeAnnotation] = oldRequest },
					func(a *api.Agent) { a.Annotations[sandboxbinding.ReauthorizeAnnotation] += " " },
					func(a *api.Agent) { delete(a.Annotations, sandboxbinding.ReauthorizeAnnotation) },
					func(a *api.Agent) { a.Annotations = nil },
				} {
					rollback := stored.DeepCopy()
					edit(rollback)
					requireAuthorizationDenied(t, reauthorizer.Update(ctx, rollback))
				}
				stored.Spec.Model = "recovered-application"
				if err := reauthorizer.Update(ctx, stored); err != nil {
					t.Fatal("repaired Agent could not accept an ordinary update:", err)
				}
				readHeld(target, stored.Annotations[sandboxbinding.AuthorizationAnnotation], stored.Annotations[sandboxbinding.ReauthorizeAnnotation])
			})
		}
	})
	t.Run("server-side apply owns annotation keys rather than the whole map", func(t *testing.T) {
		for _, mode := range []string{"create-defaults", "create-explicit", "adopt-existing"} {
			t.Run(mode, func(t *testing.T) {
				name := "apply-" + mode
				annotations := map[string]any{"example.com/gitops-note": "initial"}
				spec := map[string]any{
					"agent": map[string]any{"type": api.AgentTypeHermes}, "user": map[string]any{"alias": "alice", "displayName": ""},
					"gatewayTokenSecretRef": "gateway-token",
				}
				manifest := map[string]any{
					"apiVersion": api.GroupVersion.String(), "kind": "Agent",
					"metadata": map[string]any{"name": name, "annotations": annotations, "finalizers": []string{api.Finalizer}},
					"spec":     spec,
				}
				if mode == "create-explicit" {
					spec["team"], spec["sandboxRef"] = f.team.Name, f.box.Name
				}
				if mode == "adopt-existing" {
					if err := member.Create(ctx, newAgent(name), client.FieldOwner("agent-bootstrap")); err != nil {
						t.Fatal(err)
					}
				}
				apply := func(writer client.Client, manager string, body map[string]any, force bool) (*api.Agent, error) {
					t.Helper()
					raw, err := json.Marshal(body)
					if err != nil {
						t.Fatal(err)
					}
					agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: name}}
					options := []client.PatchOption{client.FieldOwner(manager)}
					if force {
						options = append(options, client.ForceOwnership)
					}
					err = writer.Patch(ctx, agent, client.RawPatch(types.ApplyPatchType, raw), options...)
					return agent, err
				}
				metadataApply := func(values map[string]any) map[string]any {
					metadata := map[string]any{"name": name}
					if values != nil {
						metadata["annotations"] = values
					}
					return map[string]any{
						"apiVersion": api.GroupVersion.String(), "kind": "Agent",
						"metadata": metadata,
					}
				}
				applied, err := apply(member, "gitops", manifest, false)
				if err != nil {
					t.Fatal("initial server-side apply failed:", err)
				}
				binding, err := sandboxbinding.AuthorizedBinding(applied)
				if err != nil {
					t.Fatal(err)
				}
				record := applied.Annotations[sandboxbinding.AuthorizationAnnotation]
				if owners := authorizationAnnotationManagers(t, applied, sandboxbinding.AuthorizationAnnotation); slices.Contains(owners, "gitops:Apply") {
					t.Fatalf("the applying manager unexpectedly owns an admission-injected annotation: %v", owners)
				}
				if owners := authorizationAnnotationManagers(t, applied, "example.com/gitops-note"); !slices.Equal(owners, []string{"gitops:Apply"}) {
					t.Fatalf("declared annotation was not owned by its applying manager: %v", owners)
				}
				for range 2 {
					applied, err = apply(member, "gitops", manifest, false)
					if err != nil {
						t.Fatal("repeating the same manifest removed admission-owned fields:", err)
					}
					if applied.Annotations[sandboxbinding.AuthorizationAnnotation] != record {
						t.Fatal("repeated apply regenerated authorization")
					}
				}
				reconciler := &operatorcontroller.AgentReconciler{Client: controller, APIReader: controller}
				if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: applied.Name}}); err != nil {
					t.Fatal("controller could not hold a sparse server-side-applied Agent:", err)
				}
				if err := admin.Get(ctx, client.ObjectKeyFromObject(applied), applied); err != nil {
					t.Fatal(err)
				}
				if !slices.Contains(applied.Finalizers, sandbox.HoldFinalizer) {
					t.Fatal("controller did not retain the execution hold")
				}
				annotations["example.com/gitops-note"], spec["model"] = "updated", "gitops-application-model"
				applied, err = apply(member, "gitops", manifest, false)
				if err != nil {
					t.Fatal("applying application-owned fields failed:", err)
				}
				observer := metadataApply(map[string]any{"example.com/observer": "preserve"})
				if _, err := apply(groupMember, "observer", observer, false); err != nil {
					t.Fatal(err)
				}
				delete(annotations, "example.com/gitops-note")
				applied, err = apply(member, "gitops", manifest, false)
				if err != nil {
					t.Fatal("omitting an owned ordinary annotation removed protected or separately owned keys:", err)
				}
				if _, found := applied.Annotations["example.com/gitops-note"]; found ||
					applied.Annotations["example.com/observer"] != "preserve" ||
					applied.Annotations[sandboxbinding.AuthorizationAnnotation] != record {
					t.Fatal("server-side apply did not reconcile annotation keys independently")
				}
				annotations["example.com/observer"] = "conflicting-value"
				_, err = apply(member, "gitops", manifest, false)
				delete(annotations, "example.com/observer")
				if !apierrors.IsConflict(err) {
					t.Fatalf("changing another manager's annotation should conflict before admission: %v", err)
				}
				intent, err := sandboxbinding.FormatReauthorization("apply-authorization-1", binding)
				if err != nil {
					t.Fatal(err)
				}
				adminManifest := metadataApply(map[string]any{sandboxbinding.ReauthorizeAnnotation: intent})
				if _, err := apply(member, "binding-administrator", adminManifest, false); err == nil {
					t.Fatal("a field-manager name conferred administrator authority")
				} else {
					requireAuthorizationDenied(t, err)
				}
				applied, err = apply(reauthorizer, "binding-administrator", adminManifest, false)
				if err != nil {
					t.Fatal("explicit administrator SSA reauthorization failed:", err)
				}
				if owners := authorizationAnnotationManagers(t, applied, sandboxbinding.ReauthorizeAnnotation); !slices.Equal(owners, []string{"binding-administrator:Apply"}) {
					t.Fatalf("reauthorization intent was not owned by its applying administrator manager: %v", owners)
				}
				for range 2 {
					if _, err := apply(reauthorizer, "binding-administrator", adminManifest, false); err != nil {
						t.Fatal("unchanged explicit intent was not an idempotent apply:", err)
					}
					applied, err = apply(member, "gitops", manifest, false)
					if err != nil {
						t.Fatal("ordinary GitOps apply attempted to remove another manager's authorization request:", err)
					}
				}
				if applied.Annotations[sandboxbinding.AuthorizationAnnotation] != record ||
					applied.Annotations[sandboxbinding.ReauthorizeAnnotation] != intent ||
					!slices.Contains(applied.Finalizers, sandbox.HoldFinalizer) || applied.Status.Sandbox != nil {
					t.Fatal("repeated apply changed authorization or released the execution hold")
				}
				wrongBinding := binding
				wrongBinding.AgentUID = "another-agent-uid"
				wrongIntent, err := sandboxbinding.FormatReauthorization("apply-wrong-target", wrongBinding)
				if err != nil {
					t.Fatal(err)
				}
				// Explicit takeover and deletion remain subject to admission even
				// with force ownership; field-manager names are not identities.
				for _, values := range []map[string]any{
					{sandboxbinding.AuthorizationAnnotation: nil},
					{sandboxbinding.AuthorizationAnnotation: "caller-authored"},
					{sandboxbinding.ReauthorizeAnnotation: nil},
					{sandboxbinding.AuthorizationAnnotation: nil, sandboxbinding.ReauthorizeAnnotation: nil},
					{sandboxbinding.ReauthorizeAnnotation: intent + " "},
					{sandboxbinding.ReauthorizeAnnotation: wrongIntent},
				} {
					for _, writer := range []client.Client{member, reauthorizer, controller} {
						_, err := apply(writer, "forced-takeover", metadataApply(values), true)
						requireAuthorizationDenied(t, err)
					}
				}
				_, err = apply(reauthorizer, "binding-administrator", metadataApply(nil), false)
				requireAuthorizationDenied(t, err)
				// Exporting live annotations into desired state claims those keys.
				// Omitting a solely owned protected key then attempts deletion.
				annotations[sandboxbinding.AuthorizationAnnotation], annotations[sandboxbinding.ReauthorizeAnnotation] = record, intent
				applied, err = apply(member, "gitops", manifest, false)
				if err != nil {
					t.Fatal("applying unchanged live metadata could not share ownership:", err)
				}
				for _, key := range []string{sandboxbinding.AuthorizationAnnotation, sandboxbinding.ReauthorizeAnnotation} {
					if owners := authorizationAnnotationManagers(t, applied, key); !slices.Contains(owners, "gitops:Apply") {
						t.Fatalf("declaring protected key %q did not claim it: %v", key, owners)
					}
					delete(annotations, key)
				}
				for _, force := range []bool{false, true} {
					_, err := apply(member, "gitops", manifest, force)
					requireAuthorizationDenied(t, err)
				}
				// An administrator can establish a separate owner for the exact
				// current values before GitOps relinquishes them, without deleting
				// authority, replay state, or the hold.
				custodian := metadataApply(map[string]any{
					sandboxbinding.AuthorizationAnnotation: record, sandboxbinding.ReauthorizeAnnotation: intent,
				})
				if _, err := apply(reauthorizer, "binding-custodian", custodian, false); err != nil {
					t.Fatal("protected annotation ownership could not be handed off unchanged:", err)
				}
				for range 2 {
					applied, err = apply(member, "gitops", manifest, false)
					if err != nil {
						t.Fatal("GitOps could not relinquish shared protected fields:", err)
					}
				}
				for _, key := range []string{sandboxbinding.AuthorizationAnnotation, sandboxbinding.ReauthorizeAnnotation} {
					owners := authorizationAnnotationManagers(t, applied, key)
					if slices.Contains(owners, "gitops:Apply") || !slices.Contains(owners, "binding-custodian:Apply") {
						t.Fatalf("protected field %q ownership did not transfer without removal: %v", key, owners)
					}
				}
				stored, actual, err := sandboxbinding.Read(ctx, admin, client.ObjectKey{Name: name})
				if err != nil || actual != binding || stored.Annotations[sandboxbinding.AuthorizationAnnotation] != record ||
					stored.Annotations[sandboxbinding.ReauthorizeAnnotation] != intent ||
					!slices.Contains(stored.Finalizers, sandbox.HoldFinalizer) {
					t.Fatalf("denied SSA changed protected state: binding=%v, err=%v", actual, err)
				}
				if err := admin.Delete(ctx, stored); err != nil {
					t.Fatal(err)
				}
				if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
					t.Fatal("controller could not finalize a sparse server-side-applied Agent:", err)
				}
				if err := admin.Get(ctx, client.ObjectKey{Name: name}, &api.Agent{}); !apierrors.IsNotFound(err) {
					t.Fatalf("metadata-only controller cleanup left the sparse Agent terminating: %v", err)
				}
			})
		}
	})
}

func requireAuthorizationDenied(t *testing.T, err error) {
	t.Helper()
	if !apierrors.IsForbidden(err) {
		t.Fatalf("expected admission denial despite Agent write RBAC, got %v", err)
	}
}

func authorizationAnnotationManagers(t *testing.T, agent *api.Agent, key string) []string {
	t.Helper()
	var managers []string
	for _, entry := range agent.ManagedFields {
		if entry.FieldsV1 == nil {
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal(entry.FieldsV1.Raw, &fields); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"f:metadata", "f:annotations", "f:" + key} {
			fields, _ = fields[field].(map[string]any)
		}
		if fields != nil {
			managers = append(managers, entry.Manager+":"+string(entry.Operation))
		}
	}
	slices.Sort(managers)
	return managers
}
