// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/azure"
	"github.com/Azure/CetiRun/app/internal/agenttools"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type credentialPolicyTestBroker struct {
	readback   *agenttools.CredentialPolicyReadback
	readCode   int
	writeCode  int
	deleteCode int
	ignorePut  bool
	puts       int
	gets       int
	deletes    int
}

func (b *credentialPolicyTestBroker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		b.gets++
		if b.readCode != 0 {
			w.WriteHeader(b.readCode)
		} else if b.readback == nil {
			w.WriteHeader(http.StatusNotFound)
		} else {
			_ = json.NewEncoder(w).Encode(b.readback)
		}
	case http.MethodPut:
		b.puts++
		var policy agenttools.CredentialPolicy
		if json.NewDecoder(r.Body).Decode(&policy) != nil || !policy.ValidApplied() {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if b.writeCode != 0 {
			w.WriteHeader(b.writeCode)
			return
		}
		if !b.ignorePut {
			b.readback = &agenttools.CredentialPolicyReadback{
				Policy: policy, Revision: policy.Revision(), RecordUID: "synthetic-executor-record", ResourceVersion: "12",
			}
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		b.deletes++
		if b.deleteCode != 0 {
			w.WriteHeader(b.deleteCode)
		} else {
			b.readback = nil
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

func newCredentialPolicyOperatorFixture(t *testing.T) (*AgentReconciler, *api.Agent, *policyResolution, *credentialPolicyTestBroker, *fakeAgentIdentityDirectory) {
	t.Helper()
	agent := newAgentIdentityTestInstance()
	agent.Spec.Agent.Type = api.AgentTypeOpenClaw
	agent.Spec.SandboxRef = "standard"
	agent.Status.AgentIdentity = &api.AgentIdentityStatus{
		Phase: api.AgentIdentityPhaseReady, ClientID: testAgentIdentityClientID, ObjectID: testAgentIdentityObjectID,
		DisplayName: testAgentIdentityDisplayName(), ObservedGeneration: agent.Generation,
	}
	team := &api.Team{ObjectMeta: metav1.ObjectMeta{Name: "example-team", UID: "team-uid"}}
	policy := &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: "example-policy", UID: "policy-uid"},
		Spec: api.AgentPolicySpec{AgentIdentity: api.AgentPolicyAgentIdentity{Allowed: true, Required: true, CredentialServices: []string{agenttools.CredentialServiceARM}}}}
	resolution := &policyResolution{team: team, policy: policy, sandboxRevision: "sandbox-current"}
	resolution.credentialRevision = credentialPolicyProjection(agent, team, policy).Revision()
	agent.Status.Policy = &api.PolicyStatus{}
	agent.Status.Sandbox = &api.SandboxStatus{AppliedRevision: resolution.sandboxRevision, TeamUID: team.UID, AgentPolicyUID: policy.UID}
	agent.Status.Conditions = []metav1.Condition{
		{Type: api.ConditionSandboxReady, Status: metav1.ConditionTrue, ObservedGeneration: agent.Generation},
		{Type: api.ConditionAgentIdentityReady, Status: metav1.ConditionTrue, ObservedGeneration: agent.Generation},
	}
	directory := &fakeAgentIdentityDirectory{byObject: &azure.ServicePrincipal{
		ID: testAgentIdentityObjectID, AppID: testAgentIdentityClientID,
		DisplayName: testAgentIdentityDisplayName(), AgentIdentityBlueprintID: testAgentIdentityBlueprintID,
	}}
	reconciler := newAgentIdentityTestReconciler(t, agent, directory)
	broker := &credentialPolicyTestBroker{}
	server := httptest.NewServer(broker)
	t.Cleanup(server.Close)
	reconciler.AgentIdentityBrokerURL, reconciler.AgentIdentityHTTPClient = server.URL, server.Client()
	if err := reconciler.Get(context.Background(), client.ObjectKeyFromObject(agent), agent); err != nil {
		t.Fatal(err)
	}
	return reconciler, agent, resolution, broker, directory
}

func TestCredentialPolicyOperatorConsumesOnlyVerifiedReadback(t *testing.T) {
	r, agent, resolution, broker, _ := newCredentialPolicyOperatorFixture(t)
	ctx := context.Background()
	if policyEnforcementVerified(agent, resolution) {
		t.Fatal("canonical identity and desired state alone satisfied PolicyApplied")
	}
	if err := r.reconcileCredentialPolicy(ctx, agent, resolution); err != nil {
		t.Fatal(err)
	}
	if broker.puts != 1 || broker.gets != 2 || !policyEnforcementVerified(agent, resolution) {
		t.Fatal("apply/readback did not supply current component evidence")
	}
	if err := r.recordPolicy(ctx, agent, resolution, nil); err != nil {
		t.Fatal(err)
	}
	if agent.Status.Policy.Phase != "Applied" || !meta.IsStatusConditionTrue(agent.Status.Conditions, api.ConditionPolicyApplied) {
		t.Fatal("aggregate policy did not consume the synthetic executor's evidence")
	}
	broker.readCode = http.StatusServiceUnavailable
	if err := r.reconcileCredentialPolicy(ctx, agent, resolution); err == nil {
		t.Fatal("unavailable readback was ignored")
	}
	if err := r.recordPolicy(ctx, agent, resolution, nil); err != nil {
		t.Fatal(err)
	}
	var persisted api.Agent
	if err := r.Get(ctx, client.ObjectKeyFromObject(agent), &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Status.AgentIdentity.CredentialPolicyRevision != "" || persisted.Status.Policy.Phase != "Pending" ||
		meta.IsStatusConditionTrue(persisted.Status.Conditions, api.ConditionPolicyApplied) {
		t.Fatal("failed readback retained applied evidence")
	}
}

func TestCredentialPolicyOperatorRejectsUncertainUnauthorizedAndStaleEvidence(t *testing.T) {
	for name, configure := range map[string]func(*api.Agent, *credentialPolicyTestBroker){
		"unavailable":   func(_ *api.Agent, b *credentialPolicyTestBroker) { b.readCode = 503 },
		"unauthorized":  func(_ *api.Agent, b *credentialPolicyTestBroker) { b.readCode = 403 },
		"write failed":  func(_ *api.Agent, b *credentialPolicyTestBroker) { b.writeCode = 503 },
		"ack only":      func(_ *api.Agent, b *credentialPolicyTestBroker) { b.ignorePut = true },
		"legacy issuer": func(_ *api.Agent, b *credentialPolicyTestBroker) { b.readback = &agenttools.CredentialPolicyReadback{} },
		"unprovisioned": func(a *api.Agent, _ *credentialPolicyTestBroker) { a.Status.AgentIdentity.ClientID = "" },
		"stale identity": func(a *api.Agent, _ *credentialPolicyTestBroker) {
			a.Status.AgentIdentity.ObservedGeneration--
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, agent, resolution, broker, _ := newCredentialPolicyOperatorFixture(t)
			agent.Status.AgentIdentity.CredentialPolicyRevision = resolution.credentialRevision
			setCredentialPolicyCondition(agent, metav1.ConditionTrue, "CredentialEnforcementVerified")
			configure(agent, broker)
			if err := r.updateStatus(context.Background(), agent); err != nil {
				t.Fatal(err)
			}
			_ = r.reconcileCredentialPolicy(context.Background(), agent, resolution)
			if agent.Status.AgentIdentity.CredentialPolicyRevision != "" || policyEnforcementVerified(agent, resolution) {
				t.Fatal("unverified evidence remained ready")
			}
		})
	}
}

type credentialPolicyRoundTrip func(*http.Request) (*http.Response, error)

func (f credentialPolicyRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCredentialPolicyOperatorLostReplyRetriesReadNotWrite(t *testing.T) {
	r, agent, resolution, broker, _ := newCredentialPolicyOperatorFixture(t)
	transport := r.AgentIdentityHTTPClient.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	r.AgentIdentityHTTPClient.Transport = credentialPolicyRoundTrip(func(request *http.Request) (*http.Response, error) {
		response, err := transport.RoundTrip(request)
		if err == nil && request.Method == http.MethodPut {
			response.Body.Close()
			return nil, io.ErrUnexpectedEOF
		}
		return response, err
	})
	if err := r.reconcileCredentialPolicy(context.Background(), agent, resolution); err == nil || policyEnforcementVerified(agent, resolution) {
		t.Fatal("lost write reply became applied evidence")
	}
	// Reconstruct the reconciler's observed Agent after its failed turn.
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(agent), agent); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileCredentialPolicy(context.Background(), agent, resolution); err != nil {
		t.Fatal(err)
	}
	if broker.puts != 1 || !policyEnforcementVerified(agent, resolution) {
		t.Fatal("recovery resubmitted a known stored application")
	}
}

func TestCredentialPolicyAggregateRejectsEveryStaleScope(t *testing.T) {
	r, current, resolution, _, _ := newCredentialPolicyOperatorFixture(t)
	if err := r.reconcileCredentialPolicy(context.Background(), current, resolution); err != nil {
		t.Fatal(err)
	}
	if !policyEnforcementVerified(current, resolution) {
		t.Fatal("synthetic executor fixture is not current")
	}
	for name, mutate := range map[string]func(*api.Agent){
		"owner":     func(a *api.Agent) { a.Annotations[api.AnnotationOwnerObjectID] = testAgentIdentityClientID },
		"tenant":    func(a *api.Agent) { a.Annotations[api.AnnotationOwnerTenantID] = testAgentIdentityClientID },
		"agent":     func(a *api.Agent) { a.UID = types.UID(testAgentIdentityClientID) },
		"client":    func(a *api.Agent) { a.Status.AgentIdentity.ClientID = testAgentIdentityObjectID },
		"principal": func(a *api.Agent) { a.Status.AgentIdentity.ObjectID = testAgentIdentityClientID },
		"stale generation": func(a *api.Agent) {
			a.Generation++
		},
		"missing condition": func(a *api.Agent) { meta.RemoveStatusCondition(&a.Status.Conditions, credentialPolicyReady) },
		"unverified":        func(a *api.Agent) { a.Status.AgentIdentity.Phase = api.AgentIdentityPhaseDegraded },
		"old revision":      func(a *api.Agent) { a.Status.AgentIdentity.CredentialPolicyRevision = "old-revision" },
		"suspended":         func(a *api.Agent) { a.Spec.Suspended = true },
		"deleting":          func(a *api.Agent) { now := metav1.Now(); a.DeletionTimestamp = &now },
	} {
		t.Run(name, func(t *testing.T) {
			agent := current.DeepCopy()
			mutate(agent)
			if policyEnforcementVerified(agent, resolution) {
				t.Fatal("stale scope satisfied aggregate policy")
			}
		})
	}
}

func TestCredentialPolicyRemovalRevokesBeforeDirectoryDeletion(t *testing.T) {
	r, agent, resolution, broker, directory := newCredentialPolicyOperatorFixture(t)
	ctx := context.Background()
	if err := r.reconcileCredentialPolicy(ctx, agent, resolution); err != nil {
		t.Fatal(err)
	}
	agent.Spec.AgentIdentity.Enabled = false
	resolution.policy.Spec.AgentIdentity.Required = false
	resolution.credentialRevision = credentialPolicyProjection(agent, resolution.team, resolution.policy).Revision()
	if err := r.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileCredentialPolicy(ctx, agent, resolution); err != nil {
		t.Fatal(err)
	}
	if policyEnforcementVerified(agent, resolution) {
		t.Fatal("removing the request hid a still-provisioned credential identity")
	}
	broker.deleteCode = http.StatusServiceUnavailable
	if err := r.reconcileAgentIdentity(ctx, agent); err == nil || directory.deleteCalls != 0 {
		t.Fatal("unconfirmed Broker cleanup allowed directory deletion")
	}
	if agent.Status.AgentIdentity.CredentialPolicyRevision != "" || policyEnforcementVerified(agent, resolution) {
		t.Fatal("failed cleanup remained policy-ready")
	}
	broker.deleteCode = 0
	if err := r.reconcileAgentIdentity(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if err := r.updateStatus(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if directory.deleteCalls != 1 || broker.deletes != 2 || !policyEnforcementVerified(agent, resolution) {
		t.Fatal("confirmed cleanup did not allow the identity-free policy")
	}
}

func TestCredentialPolicyStatusConflictDoesNotPersistEvidence(t *testing.T) {
	r, agent, resolution, _, _ := newCredentialPolicyOperatorFixture(t)
	r.Client = &conflictOnceStatusClient{Client: r.Client, conflictsRemaining: 1}
	if err := r.reconcileCredentialPolicy(context.Background(), agent, resolution); err == nil || policyEnforcementVerified(agent, resolution) {
		t.Fatal("failed status write was acknowledged")
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(agent), agent); err != nil {
		t.Fatal(err)
	}
	if policyEnforcementVerified(agent, resolution) {
		t.Fatal("uncommitted readback survived a status conflict")
	}
	if err := r.reconcileCredentialPolicy(context.Background(), agent, resolution); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialPolicyConsumerRejectsRedirectAndMalformedSuccess(t *testing.T) {
	for _, response := range []string{"redirect", "null", "{}", `{"policy":{},"extra":"unsupported"}`, strings.Repeat("x", (16<<10)+1)} {
		t.Run(response[:min(12, len(response))], func(t *testing.T) {
			r, agent, resolution, _, _ := newCredentialPolicyOperatorFixture(t)
			calls := 0
			r.AgentIdentityHTTPClient.Transport = credentialPolicyRoundTrip(func(request *http.Request) (*http.Response, error) {
				calls++
				status, body := 200, response
				headers := http.Header{}
				if response == "redirect" {
					status, body = 307, ""
					headers.Set("Location", "https://broker.example.com/other")
				}
				return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			if err := r.reconcileCredentialPolicy(context.Background(), agent, resolution); err == nil ||
				policyEnforcementVerified(agent, resolution) || calls != 1 {
				t.Fatal("invalid success or redirect was consumed")
			}
		})
	}
}

func TestCredentialPolicyPolicyReconcilerPersistsDependencyFailure(t *testing.T) {
	ctx := context.Background()
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	agent.Spec.AgentIdentity = &api.AgentIdentitySpec{Enabled: true}
	agent.Annotations[api.AnnotationOwnerTenantID] = testAgentIdentityTenantID
	agent.Annotations[api.AnnotationOwnerObjectID] = testAgentIdentityOwnerObject
	agent.Status.AgentIdentity = &api.AgentIdentityStatus{Phase: api.AgentIdentityPhaseReady,
		ClientID: testAgentIdentityClientID, ObjectID: testAgentIdentityObjectID, ObservedGeneration: agent.Generation}
	setCondition(&agent.Status.Conditions, api.ConditionAgentIdentityReady, metav1.ConditionTrue, "Ready", "Synthetic identity")
	r, kube, _ := aiGatewayTestReconciler(t, agent)
	policy := &api.AgentPolicy{}
	team := &api.Team{}
	if err := kube.Get(ctx, client.ObjectKey{Name: testPolicyName}, policy); err != nil {
		t.Fatal(err)
	}
	policy.UID = "policy-uid"
	policy.Spec.AgentIdentity = api.AgentPolicyAgentIdentity{Allowed: true, CredentialServices: []string{agenttools.CredentialServiceARM}}
	if err := kube.Update(ctx, policy); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(ctx, client.ObjectKey{Name: testAIGatewayTeam}, team); err != nil {
		t.Fatal(err)
	}
	team.UID = "team-uid"
	if err := kube.Update(ctx, team); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(agent), agent); err != nil {
		t.Fatal(err)
	}
	r.AgentIdentityBrokerURL = "https://broker.example.com"
	r.AgentIdentityEnabled = true
	r.AgentIdentityHTTPClient = &http.Client{Transport: credentialPolicyRoundTrip(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("synthetic Broker unavailable")
	})}
	result, err := r.reconcilePolicy(ctx, agent)
	if err == nil || !result.authorized || !result.dependencyUnavailable || result.projectionsVerified ||
		agent.Status.Policy.Phase != "Pending" || agent.Status.AgentIdentity.CredentialPolicyRevision != "" {
		t.Fatalf("credential dependency was not held: %+v, %v", result, err)
	}
}
