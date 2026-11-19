// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/aigateway"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
)

func TestPolicyGatewayRevisionFollowsModelNamesNotTheirConfiguration(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, _, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
	ctx := context.Background()

	first, err := r.resolvePolicy(ctx, agent)
	if err != nil {
		t.Fatal(err)
	}
	// A running Agent whose projections for the first revision are verified.
	running := &api.Agent{ObjectMeta: metav1.ObjectMeta{Generation: 3}, Status: api.AgentStatus{
		Phase: api.PhaseReady,
		Policy: &api.PolicyStatus{Enforcement: api.PolicyEnforcementStatus{
			Sandbox:    api.PolicyRevisionExpectation{DesiredRevision: "sandbox"},
			Gateway:    api.PolicyRevisionExpectation{DesiredRevision: first.gatewayRevision},
			Credential: api.PolicyRevisionExpectation{DesiredRevision: first.credentialRevision},
		}},
		Sandbox:    &api.SandboxStatus{AppliedRevision: "sandbox", TeamUID: first.team.UID, AgentPolicyUID: first.policy.UID},
		AIGateway:  &api.AIGatewayStatus{Phase: api.AIGatewayPhaseReady, Routed: true, PolicyRevision: first.gatewayRevision},
		Conditions: []metav1.Condition{{Type: api.ConditionSandboxReady, Status: metav1.ConditionTrue, ObservedGeneration: 3}},
	}}
	withSandbox := func(resolution *policyResolution) *policyResolution {
		current := *resolution
		current.sandboxRevision = "sandbox"
		return &current
	}
	if policyReprojectionMustHold(running, withSandbox(first)) {
		t.Fatal("test setup: the verified projection was held")
	}

	// An administrator edits the allowed model's configuration, which gives
	// it a new deployment ID, and adds a load-balanced entry of the same name.
	gateway.shared[0].ID = managedTestDeploymentID(testSharedModelOne, 7)
	gateway.shared = append(gateway.shared, aigateway.Deployment{
		ID: managedTestDeploymentID(testSharedModelOne, 8), ModelName: testSharedModelOne,
		AccessGroups: []string{testManagedGroup}, DatabaseSourceKnown: true,
	})
	edited, err := r.resolvePolicy(ctx, agent)
	if err != nil {
		t.Fatal(err)
	}
	if edited.gatewayRevision != first.gatewayRevision {
		t.Fatal("a configuration edit that keeps the model name changed the policy Gateway revision")
	}
	if policyReprojectionMustHold(running, withSandbox(edited)) {
		t.Fatal("a configuration edit that keeps the model name fenced a running policy-bound Agent")
	}

	// A model name the policy does not allow, added and removed again, changes
	// nothing for its Agents (measured on e2e: before, adding one stopped every
	// running native policy-bound Agent for good).
	gateway.shared = append(gateway.shared, aigateway.Deployment{
		ID: managedTestDeploymentID("new-shared", 1), ModelName: "new-shared",
		AccessGroups: []string{aiGatewayModelAccessGroupPrefix + aiGatewayModelBinding("new-shared")}, DatabaseSourceKnown: true,
	})
	added, err := r.resolvePolicy(ctx, agent)
	if err != nil {
		t.Fatal(err)
	}
	if added.gatewayRevision != first.gatewayRevision || policyReprojectionMustHold(running, withSandbox(added)) {
		t.Fatal("a model name the policy does not allow re-projected its Agent")
	}
	gateway.shared = gateway.shared[:len(gateway.shared)-1]
	removed, err := r.resolvePolicy(ctx, agent)
	if err != nil {
		t.Fatal(err)
	}
	if removed.gatewayRevision != first.gatewayRevision {
		t.Fatal("removing an unrelated model name changed the policy Gateway revision")
	}

	// The allowed name bound to other groups re-projects: a console copy of its
	// group makes the chart's group untrusted, which leaves the name unresolved.
	gateway.shared = append(gateway.shared, aigateway.Deployment{
		ID: "console-copy", ModelName: "elsewhere", AccessGroups: []string{testManagedGroup}, DatabaseSourceKnown: true, FromDatabase: true,
	})
	if _, err := r.resolvePolicy(ctx, agent); err == nil {
		t.Fatal("an allowed model whose group is no longer trusted must not resolve")
	}
}

func TestPolicyRevisionsAreScopedToTheirEnforcementDomains(t *testing.T) {
	ctx := context.Background()
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, kube, _ := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
	baseline, err := r.resolvePolicy(ctx, agent)
	if err != nil {
		t.Fatal(err)
	}

	team := &api.Team{}
	if err := kube.Get(ctx, types.NamespacedName{Name: testAIGatewayTeam}, team); err != nil {
		t.Fatal(err)
	}
	team.Spec.MemberContext.Role = "unrelated-role"
	team.Generation++
	if err := kube.Update(ctx, team); err != nil {
		t.Fatal(err)
	}
	team.Status.ObservedGeneration = team.Generation
	team.Status.Conditions[0].ObservedGeneration = team.Generation
	if err := kube.Status().Update(ctx, team); err != nil {
		t.Fatal(err)
	}
	afterTeam, err := r.resolvePolicy(ctx, agent)
	if err != nil {
		t.Fatal(err)
	}
	if afterTeam.gatewayRevision != baseline.gatewayRevision || afterTeam.credentialRevision != baseline.credentialRevision || afterTeam.sandboxRevision != baseline.sandboxRevision {
		t.Fatal("an unrelated Team default changed an enforcement revision")
	}

	policy := &api.AgentPolicy{}
	if err := kube.Get(ctx, types.NamespacedName{Name: testPolicyName}, policy); err != nil {
		t.Fatal(err)
	}
	policy.Spec.Network.BlockedPorts = []api.AgentPolicyPort{{Port: 443}}
	policy.Generation++
	if err := kube.Update(ctx, policy); err != nil {
		t.Fatal(err)
	}
	policy.Status.ObservedGeneration = policy.Generation
	policy.Status.Conditions[0].ObservedGeneration = policy.Generation
	if err := kube.Status().Update(ctx, policy); err != nil {
		t.Fatal(err)
	}
	afterNetwork, err := r.resolvePolicy(ctx, agent)
	if err != nil {
		t.Fatal(err)
	}
	if afterNetwork.gatewayRevision != baseline.gatewayRevision || afterNetwork.credentialRevision != baseline.credentialRevision {
		t.Fatal("Sandbox-only policy changed Gateway or credential revisions")
	}

	if err := kube.Get(ctx, types.NamespacedName{Name: testPolicyName}, policy); err != nil {
		t.Fatal(err)
	}
	policy.Spec.AgentIdentity = api.AgentPolicyAgentIdentity{Allowed: true, CredentialServices: []string{"azure-resource-manager"}}
	policy.Generation++
	if err := kube.Update(ctx, policy); err != nil {
		t.Fatal(err)
	}
	policy.Status.ObservedGeneration = policy.Generation
	policy.Status.Conditions[0].ObservedGeneration = policy.Generation
	if err := kube.Status().Update(ctx, policy); err != nil {
		t.Fatal(err)
	}
	afterIdentity, err := r.resolvePolicy(ctx, agent)
	if err != nil {
		t.Fatal(err)
	}
	if afterIdentity.gatewayRevision != baseline.gatewayRevision || afterIdentity.credentialRevision == baseline.credentialRevision {
		t.Fatal("credential policy did not remain isolated from the Gateway revision")
	}
}

func TestPolicyReprojectionHoldsOnlyActiveOldProjection(t *testing.T) {
	resolution := &policyResolution{
		team: &api.Team{ObjectMeta: metav1.ObjectMeta{UID: "team-uid"}}, policy: &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{UID: "policy-uid"}},
		gatewayRevision: "gateway-new", credentialRevision: "credential", sandboxRevision: "sandbox",
	}
	agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Generation: 3}, Status: api.AgentStatus{Policy: &api.PolicyStatus{
		Enforcement: api.PolicyEnforcementStatus{
			Sandbox:    api.PolicyRevisionExpectation{DesiredRevision: "sandbox"},
			Gateway:    api.PolicyRevisionExpectation{DesiredRevision: "gateway-old"},
			Credential: api.PolicyRevisionExpectation{DesiredRevision: "credential"},
		},
	}}}
	if policyReprojectionMustHold(agent, resolution) {
		t.Fatal("a non-running Agent must be allowed to create its first current projection")
	}
	agent.Status.Phase = api.PhaseReady
	if !policyReprojectionMustHold(agent, resolution) {
		t.Fatal("a running Agent retained an old Gateway projection")
	}
	// A matching verified projection has no hold transition.
	agent.Status.Policy.Enforcement.Gateway.DesiredRevision = resolution.gatewayRevision
	agent.Status.Sandbox = &api.SandboxStatus{AppliedRevision: resolution.sandboxRevision, TeamUID: resolution.team.UID,
		AgentPolicyUID: resolution.policy.UID}
	agent.Status.AIGateway = &api.AIGatewayStatus{Phase: api.AIGatewayPhaseReady, Routed: true, PolicyRevision: resolution.gatewayRevision}
	agent.Status.Conditions = []metav1.Condition{{Type: api.ConditionSandboxReady, Status: metav1.ConditionTrue, ObservedGeneration: agent.Generation}}
	if policyReprojectionMustHold(agent, resolution) {
		t.Fatal("a running Agent with all current projections verified was unnecessarily held")
	}
}

func TestPolicyWorkloadActivityDistinguishesDisabledFromPublishedIngress(t *testing.T) {
	agent := &api.Agent{Status: api.AgentStatus{Phase: api.PhasePending, Conditions: []metav1.Condition{{
		Type: api.ConditionIngressReady, Status: metav1.ConditionTrue, Reason: "IngressDisabled",
	}}}}
	if policyWorkloadMayBeActive(agent) {
		t.Fatal("an intentionally absent ingress route was treated as an active workload")
	}
	agent.Status.Conditions[0].Reason = "IngressPublished"
	if !policyWorkloadMayBeActive(agent) {
		t.Fatal("a published ingress route was not treated as a potentially active workload")
	}
}

func TestPolicyEnforcementRejectsStaleSandboxEvidence(t *testing.T) {
	resolution := &policyResolution{
		team:            &api.Team{ObjectMeta: metav1.ObjectMeta{UID: "team-uid"}},
		policy:          &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{UID: "policy-uid"}},
		sandboxRevision: "sandbox-current",
	}
	agent := &api.Agent{
		ObjectMeta: metav1.ObjectMeta{Generation: 7},
		Spec:       api.AgentSpec{SandboxRef: "standard"},
		Status: api.AgentStatus{
			Sandbox: &api.SandboxStatus{TeamUID: resolution.team.UID, AgentPolicyUID: resolution.policy.UID,
				AppliedRevision: resolution.sandboxRevision},
			Conditions: []metav1.Condition{{Type: api.ConditionSandboxReady, Status: metav1.ConditionTrue, ObservedGeneration: 7}},
		},
	}
	if !policyEnforcementVerified(agent, resolution) {
		t.Fatal("current Sandbox evidence was not accepted")
	}
	for name, mutate := range map[string]func(*api.Agent){
		"revision":   func(a *api.Agent) { a.Status.Sandbox.AppliedRevision = "sandbox-old" },
		"team":       func(a *api.Agent) { a.Status.Sandbox.TeamUID = "old-team" },
		"policy":     func(a *api.Agent) { a.Status.Sandbox.AgentPolicyUID = "old-policy" },
		"generation": func(a *api.Agent) { a.Status.Conditions[0].ObservedGeneration-- },
	} {
		t.Run(name, func(t *testing.T) {
			stale := agent.DeepCopy()
			mutate(stale)
			if policyEnforcementVerified(stale, resolution) {
				t.Fatal("stale Sandbox evidence was accepted")
			}
		})
	}
}

func TestDesiredSandboxRevisionDoesNotCopyHistoricalEvidenceWithoutCompiler(t *testing.T) {
	agent := &api.Agent{Status: api.AgentStatus{Sandbox: &api.SandboxStatus{AppliedRevision: "historical"}}}
	revision, err := (&AgentReconciler{}).desiredSandboxPolicyRevision(context.Background(), agent, &api.Team{})
	if err != nil {
		t.Fatal(err)
	}
	if revision != "" {
		t.Fatalf("desired revision = %q, want empty without a compiler", revision)
	}
}

func TestPolicyDenialRevokesKeyAndStopsOwnedBuiltinWorkload(t *testing.T) {
	ctx := context.Background()
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	agent.Status.Phase = api.PhaseReady
	namespace := aiGatewayNamespace()
	owner := metav1.OwnerReference{APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: agent.Name, UID: agent.UID}
	replicas := int32(1)
	workload := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: instanceServiceName(agent.Name), Namespace: namespace.Name, UID: types.UID("workload-uid"),
		OwnerReferences: []metav1.OwnerReference{owner},
		Annotations: map[string]string{
			customAgentUID:                   string(agent.UID),
			"meta.helm.sh/release-name":      helmReleaseName(agent.Name),
			"meta.helm.sh/release-namespace": namespace.Name,
		},
	}, Spec: appsv1.StatefulSetSpec{Replicas: &replicas}}
	route := &istiov1beta1.VirtualService{ObjectMeta: metav1.ObjectMeta{
		Name: virtualServiceName(agent.Name), Namespace: namespace.Name, UID: types.UID("route-uid"),
		OwnerReferences: []metav1.OwnerReference{owner},
		Annotations: map[string]string{
			customAgentUID:                   string(agent.UID),
			"meta.helm.sh/release-name":      helmReleaseName(agent.Name),
			"meta.helm.sh/release-namespace": namespace.Name,
		},
	}}
	r, kube, gateway := aiGatewayTestReconciler(t, agent, namespace, workload, route)
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	if len(gateway.keys) != 1 {
		t.Fatal("test setup did not create the governed virtual key")
	}

	team := &api.Team{}
	if err := kube.Get(ctx, types.NamespacedName{Name: testAIGatewayTeam}, team); err != nil {
		t.Fatal(err)
	}
	team.Spec.SandboxRefs = nil
	if err := kube.Update(ctx, team); err != nil {
		t.Fatal(err)
	}

	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: agent.Name}})
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("denied Agent must enter a retrying hold: result=%+v err=%v", result, err)
	}
	if len(gateway.keys) != 0 || len(gateway.deletedTokens) != 1 {
		t.Fatalf("denial did not revoke the virtual key: keys=%d deleted=%v", len(gateway.keys), gateway.deletedTokens)
	}
	stopped := &appsv1.StatefulSet{}
	if err := kube.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: workload.Name}, stopped); err != nil || !ptr.Equal(stopped.Spec.Replicas, ptr.To[int32](0)) {
		t.Fatalf("denial did not stop the owned workload: workload=%+v err=%v", stopped.Spec.Replicas, err)
	}
	if err := kube.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: route.Name}, &istiov1beta1.VirtualService{}); !apierrors.IsNotFound(err) {
		t.Fatalf("denial did not remove the owned route: %v", err)
	}
	current := &api.Agent{}
	if err := kube.Get(ctx, types.NamespacedName{Name: agent.Name}, current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Policy == nil || current.Status.Policy.Phase != "Denied" {
		t.Fatalf("denial status was not persisted: %+v", current.Status.Policy)
	}
}

func TestPolicyDependencyFailureBlocksButPreservesKeyAndRecovers(t *testing.T) {
	ctx := context.Background()
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	agent.Status.Phase = api.PhaseReady
	namespace := aiGatewayNamespace()
	owner := metav1.OwnerReference{APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: agent.Name, UID: agent.UID}
	replicas := int32(1)
	workload := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: instanceServiceName(agent.Name), Namespace: namespace.Name, UID: types.UID("workload-uid"), OwnerReferences: []metav1.OwnerReference{owner},
		Annotations: map[string]string{customAgentUID: string(agent.UID), "meta.helm.sh/release-name": helmReleaseName(agent.Name), "meta.helm.sh/release-namespace": namespace.Name},
	}, Spec: appsv1.StatefulSetSpec{Replicas: &replicas}}
	route := &istiov1beta1.VirtualService{ObjectMeta: metav1.ObjectMeta{
		Name: virtualServiceName(agent.Name), Namespace: namespace.Name, UID: types.UID("route-uid"), OwnerReferences: []metav1.OwnerReference{owner},
		Annotations: map[string]string{customAgentUID: string(agent.UID), "meta.helm.sh/release-name": helmReleaseName(agent.Name), "meta.helm.sh/release-namespace": namespace.Name},
	}}
	r, kube, gateway := aiGatewayTestReconciler(t, agent, namespace, workload, route)
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	if err := r.updateStatus(ctx, agent); err != nil {
		t.Fatal(err)
	}
	secret := readKeySecret(t, kube)
	token := aigateway.Token(string(secret.Data[aiGatewayKeySecretField]))
	if gateway.keys[token] == nil {
		t.Fatal("test setup did not create the governed key")
	}

	dependencyDown := true
	r.Client = interceptor.NewClient(kube.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
			if _, ok := object.(*api.Team); ok && dependencyDown {
				return errors.New("synthetic Team API outage")
			}
			return c.Get(ctx, key, object, options...)
		},
	})
	r.APIReader = r.Client
	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: agent.Name}})
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("dependency outage must enter a retrying hold: result=%+v err=%v", result, err)
	}
	if gateway.keys[token] == nil || !gateway.keys[token].Blocked || len(gateway.deletedTokens) != 0 || len(gateway.deletedAliases) != 0 {
		t.Fatalf("dependency outage deleted or failed to block the existing key: key=%+v tokens=%v aliases=%v", gateway.keys[token], gateway.deletedTokens, gateway.deletedAliases)
	}
	current := &api.Agent{}
	if err := kube.Get(ctx, types.NamespacedName{Name: agent.Name}, current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Policy == nil || current.Status.Policy.Phase != "Pending" {
		t.Fatalf("dependency outage was reported as a denial: %+v", current.Status.Policy)
	}

	dependencyDown = false
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: agent.Name}}); err != nil {
		t.Fatal(err)
	}
	if gateway.keys[token] == nil || gateway.keys[token].Blocked || len(gateway.deletedTokens) != 0 || len(gateway.deletedAliases) != 0 {
		t.Fatalf("recovery did not reuse and unblock the same key: key=%+v tokens=%v aliases=%v", gateway.keys[token], gateway.deletedTokens, gateway.deletedAliases)
	}
}

func TestPolicyGatewayNeverProjectsAKeyIntoAnUnverifiedNamespace(t *testing.T) {
	ctx := context.Background()
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
	namespace := &corev1.Namespace{}
	if err := kube.Get(ctx, types.NamespacedName{Name: instanceNamespace(agent.Name)}, namespace); err != nil {
		t.Fatal(err)
	}
	delete(namespace.Annotations, sandboxworkload.AgentUIDAnnotation)
	if err := kube.Update(ctx, namespace); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: agent.Name}}); err == nil {
		t.Fatal("an unverified namespace must keep the governed Agent held")
	}
	secret := &corev1.Secret{}
	err := kube.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: aiGatewayKeySecretName}, secret)
	if !apierrors.IsNotFound(err) || len(gateway.generated) != 0 {
		t.Fatalf("an unverified namespace received Gateway credentials: secretErr=%v generated=%d", err, len(gateway.generated))
	}
}

func TestPolicyUnknownAcceptancePreservesTheExistingGatewayKey(t *testing.T) {
	for _, kind := range []string{"Team", "AgentPolicy"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			agent := aiGatewayTestAgent(testAIGatewayTeam)
			r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
			if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
				t.Fatal(err)
			}
			if err := r.updateStatus(ctx, agent); err != nil {
				t.Fatal(err)
			}
			token := aigateway.Token(string(readKeySecret(t, kube).Data[aiGatewayKeySecretField]))

			switch kind {
			case "Team":
				team := &api.Team{}
				if err := kube.Get(ctx, types.NamespacedName{Name: testAIGatewayTeam}, team); err != nil {
					t.Fatal(err)
				}
				team.Status.Conditions[0].Status = metav1.ConditionUnknown
				if err := kube.Status().Update(ctx, team); err != nil {
					t.Fatal(err)
				}
			case "AgentPolicy":
				policy := &api.AgentPolicy{}
				if err := kube.Get(ctx, types.NamespacedName{Name: testPolicyName}, policy); err != nil {
					t.Fatal(err)
				}
				policy.Status.Conditions[0].Status = metav1.ConditionUnknown
				if err := kube.Status().Update(ctx, policy); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := r.resolvePolicy(ctx, agent); err == nil || !isPolicyDependencyError(err) {
				t.Fatalf("Accepted=Unknown must be a retryable dependency state: %v", err)
			}
			if _, err := r.holdPolicyDependencyUnavailable(ctx, agent); err != nil {
				t.Fatal(err)
			}
			if gateway.keys[token] == nil || !gateway.keys[token].Blocked || len(gateway.deletedTokens) != 0 || len(gateway.deletedAliases) != 0 {
				t.Fatalf("Accepted=Unknown deleted or failed to block the key: key=%+v tokens=%v aliases=%v", gateway.keys[token], gateway.deletedTokens, gateway.deletedAliases)
			}
		})
	}
}

func TestPolicyStaleConfigurationObservationPreservesKeyUntilAccepted(t *testing.T) {
	for _, kind := range []string{"Team", "AgentPolicy"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			agent := aiGatewayTestAgent(testAIGatewayTeam)
			namespace := aiGatewayNamespace()
			r, kube, gateway := aiGatewayTestReconciler(t, agent, namespace)
			if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
				t.Fatal(err)
			}
			if err := r.updateStatus(ctx, agent); err != nil {
				t.Fatal(err)
			}
			secret := readKeySecret(t, kube)
			token := aigateway.Token(string(secret.Data[aiGatewayKeySecretField]))

			switch kind {
			case "Team":
				team := &api.Team{}
				if err := kube.Get(ctx, types.NamespacedName{Name: testAIGatewayTeam}, team); err != nil {
					t.Fatal(err)
				}
				team.Generation++
				if err := kube.Update(ctx, team); err != nil {
					t.Fatal(err)
				}
			case "AgentPolicy":
				policy := &api.AgentPolicy{}
				if err := kube.Get(ctx, types.NamespacedName{Name: testPolicyName}, policy); err != nil {
					t.Fatal(err)
				}
				policy.Generation++
				if err := kube.Update(ctx, policy); err != nil {
					t.Fatal(err)
				}
			}
			r.APIReader = r.Client
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: agent.Name}}); err != nil {
				t.Fatalf("stale %s observation did not enter a controlled hold: %v", kind, err)
			}
			if gateway.keys[token] == nil || !gateway.keys[token].Blocked || len(gateway.deletedTokens) != 0 || len(gateway.deletedAliases) != 0 {
				t.Fatalf("stale %s observation deleted or failed to block the key: key=%+v tokens=%v aliases=%v", kind, gateway.keys[token], gateway.deletedTokens, gateway.deletedAliases)
			}

			switch kind {
			case "Team":
				team := &api.Team{}
				if err := kube.Get(ctx, types.NamespacedName{Name: testAIGatewayTeam}, team); err != nil {
					t.Fatal(err)
				}
				team.Status.ObservedGeneration = team.Generation
				team.Status.Conditions = []metav1.Condition{{Type: api.ConditionTeamAccepted, Status: metav1.ConditionTrue, ObservedGeneration: team.Generation, Reason: "Accepted"}}
				if err := kube.Status().Update(ctx, team); err != nil {
					t.Fatal(err)
				}
			case "AgentPolicy":
				policy := &api.AgentPolicy{}
				if err := kube.Get(ctx, types.NamespacedName{Name: testPolicyName}, policy); err != nil {
					t.Fatal(err)
				}
				policy.Status.ObservedGeneration = policy.Generation
				policy.Status.Conditions = []metav1.Condition{{Type: api.ConditionAgentPolicyAccepted, Status: metav1.ConditionTrue, ObservedGeneration: policy.Generation, Reason: "Accepted"}}
				if err := kube.Status().Update(ctx, policy); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: agent.Name}}); err != nil {
				t.Fatalf("accepted %s observation did not recover: %v", kind, err)
			}
			if gateway.keys[token] == nil || gateway.keys[token].Blocked || len(gateway.deletedTokens) != 0 || len(gateway.deletedAliases) != 0 {
				t.Fatalf("accepted %s observation did not reuse the same key: key=%+v tokens=%v aliases=%v", kind, gateway.keys[token], gateway.deletedTokens, gateway.deletedAliases)
			}
		})
	}
}

func TestSandboxHoldContinuesShutdownWhenFinalizerPatchFails(t *testing.T) {
	ctx := context.Background()
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	namespace := aiGatewayNamespace()
	owner := metav1.OwnerReference{APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: agent.Name, UID: agent.UID}
	replicas := int32(1)
	workload := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: instanceServiceName(agent.Name), Namespace: namespace.Name, UID: types.UID("workload-uid"), OwnerReferences: []metav1.OwnerReference{owner},
		Annotations: map[string]string{customAgentUID: string(agent.UID), "meta.helm.sh/release-name": helmReleaseName(agent.Name), "meta.helm.sh/release-namespace": namespace.Name},
	}, Spec: appsv1.StatefulSetSpec{Replicas: &replicas}}
	route := &istiov1beta1.VirtualService{ObjectMeta: metav1.ObjectMeta{
		Name: virtualServiceName(agent.Name), Namespace: namespace.Name, UID: types.UID("route-uid"), OwnerReferences: []metav1.OwnerReference{owner},
		Annotations: map[string]string{customAgentUID: string(agent.UID), "meta.helm.sh/release-name": helmReleaseName(agent.Name), "meta.helm.sh/release-namespace": namespace.Name},
	}}
	r, kube, _ := aiGatewayTestReconciler(t, agent, namespace, workload, route)
	injected := errors.New("synthetic finalizer patch failure")
	r.Client = interceptor.NewClient(kube.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			if _, ok := object.(*api.Agent); ok {
				return injected
			}
			return c.Patch(ctx, object, patch, options...)
		},
	})
	if _, err := r.holdSandbox(ctx, agent); !errors.Is(err, injected) {
		t.Fatalf("finalizer persistence failure was hidden: %v", err)
	}
	stopped := &appsv1.StatefulSet{}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(workload), stopped); err != nil || !ptr.Equal(stopped.Spec.Replicas, ptr.To[int32](0)) {
		t.Fatalf("finalizer failure skipped workload shutdown: replicas=%v err=%v", stopped.Spec.Replicas, err)
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(route), &istiov1beta1.VirtualService{}); !apierrors.IsNotFound(err) {
		t.Fatalf("finalizer failure skipped route removal: %v", err)
	}
	persisted := &api.Agent{}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(agent), persisted); err != nil {
		t.Fatal(err)
	}
	if controllerContainsFinalizer(persisted, sandboxHoldFinalizer) || controllerContainsFinalizer(agent, sandboxHoldFinalizer) {
		t.Fatal("an unpersisted hold finalizer was retained in local or stored state")
	}
}

func controllerContainsFinalizer(object client.Object, finalizer string) bool {
	for _, current := range object.GetFinalizers() {
		if current == finalizer {
			return true
		}
	}
	return false
}

func TestPolicyReprojectionBlockClearsOnlyAfterGrantReadback(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
	ctx := context.Background()
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	if err := r.updateStatus(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if err := r.beginAIGatewayPolicyReprojection(ctx, agent); err != nil {
		t.Fatal(err)
	}
	secret := readKeySecret(t, kube)
	token := aigateway.Token(string(secret.Data[aiGatewayKeySecretField]))
	if !gateway.keys[token].Blocked || agent.Status.AIGateway.Reason != aiGatewayReasonPolicyReprojection {
		t.Fatalf("old grant was not recorded as an operator-owned reprojection block: %+v", agent.Status.AIGateway)
	}
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	if gateway.keys[token].Blocked || agent.Status.AIGateway.Reason != "" {
		t.Fatalf("verified current grant did not clear the temporary reprojection block: %+v", agent.Status.AIGateway)
	}
}

func TestPolicyReprojectionConvertsSuspensionBlockAndResumesTheSameGrant(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
	ctx := context.Background()
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	if err := r.updateStatus(ctx, agent); err != nil {
		t.Fatal(err)
	}
	secret := readKeySecret(t, kube)
	token := aigateway.Token(string(secret.Data[aiGatewayKeySecretField]))
	spend := 3.25
	gateway.keys[token].Spend = &spend
	wantBudget := *gateway.keys[token].MaxBudget

	agent.Spec.Suspended = true
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	if err := r.updateStatus(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if !gateway.keys[token].Blocked || agent.Status.AIGateway.Reason != aiGatewayReasonSuspended {
		t.Fatalf("suspension did not establish an operator-owned block: key=%+v status=%+v", gateway.keys[token], agent.Status.AIGateway)
	}

	agent.Spec.Suspended = false
	if err := r.beginAIGatewayPolicyReprojection(ctx, agent); err != nil {
		t.Fatalf("suspension block could not become a policy-reprojection block: %v", err)
	}
	if !gateway.keys[token].Blocked || agent.Status.AIGateway.Reason != aiGatewayReasonPolicyReprojection {
		t.Fatalf("suspension block lost its operator ownership during reprojection: key=%+v status=%+v", gateway.keys[token], agent.Status.AIGateway)
	}
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	if len(gateway.generated) != 1 || gateway.keys[token].Blocked || gateway.keys[token].Spend == nil ||
		*gateway.keys[token].Spend != spend || gateway.keys[token].MaxBudget == nil || *gateway.keys[token].MaxBudget != wantBudget {
		t.Fatalf("resume did not preserve and unblock the same grant: generated=%d key=%+v", len(gateway.generated), gateway.keys[token])
	}
	if agent.Status.AIGateway.Reason != "" || agent.Status.AIGateway.Blocked || agent.Status.AIGateway.SpendUSD != "3.25" {
		t.Fatalf("verified grant did not clear the temporary block: %+v", agent.Status.AIGateway)
	}
}

func TestPolicyReprojectionDoesNotClaimAnAdministratorBlock(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
	ctx := context.Background()
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	if err := r.updateStatus(ctx, agent); err != nil {
		t.Fatal(err)
	}
	token := aigateway.Token(string(readKeySecret(t, kube).Data[aiGatewayKeySecretField]))
	gateway.keys[token].Blocked = true
	agent.Status.AIGateway.Reason = ""

	if err := r.beginAIGatewayPolicyReprojection(ctx, agent); err == nil {
		t.Fatal("policy reprojection claimed an administrator block")
	}
	if !gateway.keys[token].Blocked || agent.Status.AIGateway.Reason == aiGatewayReasonPolicyReprojection {
		t.Fatalf("administrator block lost its ownership boundary: key=%+v status=%+v", gateway.keys[token], agent.Status.AIGateway)
	}
}

func TestPolicyFenceDoesNotPrepareAReplacementSandbox(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	agent.Status.Phase = api.PhaseReady
	r, kube, _ := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
	// An incomplete preparation configuration fails immediately if the normal
	// held-preparation path is entered. A policy fence must not consult it.
	r.SandboxPreparation = &SandboxPreparation{}

	result, err := r.fenceSandboxForPolicyReprojection(context.Background(), agent)
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("policy fence entered Sandbox preparation: result=%+v err=%v", result, err)
	}
	current := &api.Agent{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(agent), current); err != nil {
		t.Fatal(err)
	}
	condition := findStatusCondition(current.Status.Conditions, api.ConditionSandboxReady)
	if current.Status.Phase != api.PhasePending || condition == nil || condition.Status != metav1.ConditionFalse ||
		condition.Reason != aiGatewayReasonPolicyReprojection {
		t.Fatalf("policy fence did not report a held replacement projection: phase=%s condition=%+v", current.Status.Phase, condition)
	}
}

func TestManagedGatewayGroupPollutionBlocksAnExistingGrant(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
	ctx := context.Background()
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	if err := r.updateStatus(ctx, agent); err != nil {
		t.Fatal(err)
	}
	secret := readKeySecret(t, kube)
	token := aigateway.Token(string(secret.Data[aiGatewayKeySecretField]))
	gateway.shared = append(gateway.shared, aigateway.Deployment{
		ID: "database-copy", ModelName: "unauthorized-model", AccessGroups: []string{testManagedGroup},
		FromDatabase: true, DatabaseSourceKnown: true,
	})
	if _, err := r.resolvePolicy(ctx, agent); err == nil || !isPolicyDependencyError(err) {
		t.Fatalf("a polluted managed group must invalidate current authorization evidence: %v", err)
	}
	// The Agent controller routes that dependency failure through this hold
	// path. Workload teardown may have no fixture resources here, but key
	// revocation must happen before it is attempted.
	_, _ = r.holdPolicyDependencyUnavailable(ctx, agent)
	if !gateway.keys[token].Blocked || agent.Status.AIGateway.Reason != aiGatewayReasonPolicyReprojection {
		t.Fatalf("the old grant remained usable after catalog pollution: key=%+v status=%+v", gateway.keys[token], agent.Status.AIGateway)
	}
	if err := r.reconcileAIGateway(ctx, agent, true); err == nil {
		t.Fatal("a polluted managed group unexpectedly produced a grant")
	}
	if !gateway.keys[token].Blocked || agent.Status.AIGateway.Reason != aiGatewayReasonPolicyReprojection {
		t.Fatalf("a failed replacement grant lost ownership of the temporary block: key=%+v status=%+v", gateway.keys[token], agent.Status.AIGateway)
	}
	gateway.shared = gateway.shared[:len(gateway.shared)-1]
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal("the same key did not recover after the collision was removed:", err)
	}
	if gateway.keys[token].Blocked || agent.Status.AIGateway.Reason != "" || agent.Status.AIGateway.KeyID != token[:aiGatewayKeyIDLength] {
		t.Fatalf("the verified grant did not restore the same key: key=%+v status=%+v", gateway.keys[token], agent.Status.AIGateway)
	}
}
