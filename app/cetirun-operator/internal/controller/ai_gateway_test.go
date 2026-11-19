// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	sigsyaml "sigs.k8s.io/yaml"

	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/aigateway"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
)

// testManagedGroup and testManagedDeploymentID are the Gateway chart's binding
// of testSharedModelOne: the first 16 hexadecimal characters of
// SHA-256("gpt-6-sol"), followed in the deployment ID by a synthetic
// configuration hash.
const (
	testAIGatewayUID        = "3f0b5a8e-1c2d-4e5f-8a9b-0c1d2e3f4a5b"
	testAIGatewayURL        = "http://cetirun-ai-gateway.cetirun-system.svc.cluster.local:4000"
	testAIGatewayTeam       = "platform-admins"
	testProxyImage          = "registry.example.com/cetirun-ai-gateway:0.1.1"
	testCopilotModels       = `[{"id":"gpt-5.5","name":"GPT-5.5","contextWindow":400000,"maxOutputTokens":128000},{"id":"claude-sonnet-4.5","name":"Claude Sonnet 4.5"},{"id":"grok-code-fast-1"},{"id":"bad/id"}]`
	testSharedModelOne      = "gpt-6-sol"
	testManagedGroup        = "cetirun-model-7388db16debdff68"
	testManagedDeploymentID = "cetirun-shared-7388db16debdff68-0123456789abcdef"
	testPolicyName          = "gateway-test-policy"
	testSandboxName         = "gateway-test-sandbox"
)

// fakeAIGateway mirrors the LiteLLM behaviour the operator relies on: keys
// are stored by the SHA-256 of their value and aliases are unique; teams,
// users and memberships are created once; model IDs are unique.
type fakeAIGateway struct {
	// deleteModelErr fails DeleteModel.
	deleteModelErr    error
	keys              map[string]*aigateway.Key
	getErr            error
	deleteErr         error
	addModelErr       error
	generated         []aigateway.GenerateRequest
	updates           [][]string
	ownerUpdates      [][2]string
	ignoreOwnerUpdate bool
	deletedTokens     []string
	deletedAliases    []string
	beforeGenerate    func(aigateway.GenerateRequest)
	teams             map[string]bool
	users             map[string]bool
	members           map[string]bool
	models            map[string]aigateway.ModelRequest
	added             []string
	deletedModels     []string
	shared            []aigateway.Deployment
	priced            map[string]bool
	// vision are the price table's names whose models accept images.
	vision    map[string]bool
	listCalls int
	// listErr fails ListModels, and priceErr fails PriceTable.
	listErr  error
	priceErr error
	// blockUnpriced is off so that tests about other behaviour need no
	// prices; the tests about prices turn it on.
	blockUnpriced bool
	blockErr      error
	aliasUpdates  []map[string]string
	aliasErr      error
	routeUpdates  [][]string
	// limitUpdates records UpdateKeyLimits calls; ignoreLimitUpdate makes
	// the Gateway accept them without storing the limits.
	limitUpdates      []aigateway.KeyLimits
	ignoreLimitUpdate bool
}

func newFakeAIGateway() *fakeAIGateway {
	return &fakeAIGateway{
		keys: map[string]*aigateway.Key{}, teams: map[string]bool{}, users: map[string]bool{}, members: map[string]bool{},
		models: map[string]aigateway.ModelRequest{},
		shared: []aigateway.Deployment{
			{ID: testManagedDeploymentID, ModelName: testSharedModelOne, AccessGroups: []string{aiGatewaySharedAccessGroup, testManagedGroup}, DatabaseSourceKnown: true, MaxInputTokens: 1000000, MaxOutputTokens: 32768},
			{ID: "cfg-2", ModelName: "admin-only", AccessGroups: []string{"cetirun-admin"}},
		},
		priced: map[string]bool{"gpt-5.5": true, "claude-sonnet-4-5": true, "gpt-4.1": true},
	}
}

func (f *fakeAIGateway) GetKey(_ context.Context, token string) (*aigateway.Key, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	key, ok := f.keys[token]
	if !ok {
		return nil, nil
	}
	copied := *key
	copied.Models = slices.Clone(key.Models)
	copied.Aliases = maps.Clone(key.Aliases)
	copied.AllowedRoutes = slices.Clone(key.AllowedRoutes)
	copied.Limits = cloneKeyLimits(key.Limits)
	return &copied, nil
}

func (f *fakeAIGateway) GenerateKey(_ context.Context, req aigateway.GenerateRequest) error {
	if f.beforeGenerate != nil {
		f.beforeGenerate(req)
	}
	for _, key := range f.keys {
		if key.Alias == req.Alias {
			return aigateway.ErrAliasExists
		}
	}
	f.generated = append(f.generated, req)
	budget := req.MaxBudget
	f.keys[aigateway.Token(req.Key)] = &aigateway.Key{
		Alias: req.Alias, Models: slices.Clone(req.Models), MaxBudget: &budget, BudgetDuration: req.BudgetDuration,
		Blocked: req.Blocked, TeamID: req.TeamID, UserID: req.UserID, Aliases: maps.Clone(req.Aliases),
		AllowedRoutes: slices.Clone(aigateway.KeyRoutes),
		Limits:        cloneKeyLimits(req.Limits),
	}
	return nil
}

func (f *fakeAIGateway) UpdateKeyRoutes(_ context.Context, token string, routes []string) error {
	key, ok := f.keys[token]
	if !ok {
		return &aigateway.APIError{StatusCode: 404, Message: "not found"}
	}
	key.AllowedRoutes = slices.Clone(routes)
	f.routeUpdates = append(f.routeUpdates, slices.Clone(routes))
	return nil
}

func (f *fakeAIGateway) UpdateKeyAliases(_ context.Context, token string, aliases map[string]string) error {
	if f.aliasErr != nil {
		return f.aliasErr
	}
	key, ok := f.keys[token]
	if !ok {
		return &aigateway.APIError{StatusCode: 404, Message: "not found"}
	}
	key.Aliases = maps.Clone(aliases)
	if len(aliases) == 0 {
		key.Aliases = nil
	}
	f.aliasUpdates = append(f.aliasUpdates, maps.Clone(aliases))
	return nil
}

func (f *fakeAIGateway) UpdateKeyModels(_ context.Context, token string, models []string) error {
	key, ok := f.keys[token]
	if !ok {
		return &aigateway.APIError{StatusCode: 404, Message: "not found"}
	}
	key.Models = slices.Clone(models)
	f.updates = append(f.updates, slices.Clone(models))
	return nil
}

func (f *fakeAIGateway) UpdateKeyBudget(_ context.Context, token string, budget float64, duration string) error {
	key, ok := f.keys[token]
	if !ok {
		return &aigateway.APIError{StatusCode: 404, Message: "not found"}
	}
	key.MaxBudget, key.BudgetDuration = &budget, duration
	return nil
}

func (f *fakeAIGateway) SetKeyBlocked(_ context.Context, token string, blocked bool) error {
	key, ok := f.keys[token]
	if !ok {
		return &aigateway.APIError{StatusCode: 404, Message: "not found"}
	}
	key.Blocked = blocked
	return nil
}

func (f *fakeAIGateway) UpdateKeyOwner(_ context.Context, token, teamID, userID string) error {
	key, ok := f.keys[token]
	if !ok {
		return &aigateway.APIError{StatusCode: 404, Message: "not found"}
	}
	if teamID != "" && !f.members[teamID+"/"+userID] {
		return &aigateway.APIError{StatusCode: 403, Message: "User is not a member of the team"}
	}
	f.ownerUpdates = append(f.ownerUpdates, [2]string{teamID, userID})
	if !f.ignoreOwnerUpdate {
		key.TeamID, key.UserID = teamID, userID
	}
	return nil
}

func (f *fakeAIGateway) DeleteKeys(_ context.Context, tokens []string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	for _, token := range tokens {
		f.deletedTokens = append(f.deletedTokens, token)
		delete(f.keys, token)
	}
	return nil
}

func (f *fakeAIGateway) DeleteKeyByAlias(_ context.Context, alias string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deletedAliases = append(f.deletedAliases, alias)
	for token, key := range f.keys {
		if key.Alias == alias {
			delete(f.keys, token)
		}
	}
	return nil
}

func (f *fakeAIGateway) EnsureTeam(_ context.Context, teamID, _ string, _ map[string]string) error {
	f.teams[teamID] = true
	return nil
}

func (f *fakeAIGateway) EnsureUser(_ context.Context, userID string, _ map[string]string) error {
	f.users[userID] = true
	return nil
}

func (f *fakeAIGateway) EnsureTeamMember(_ context.Context, teamID, userID string) error {
	if !f.teams[teamID] {
		return &aigateway.APIError{StatusCode: 400, Message: "team does not exist"}
	}
	f.members[teamID+"/"+userID] = true
	return nil
}

func (f *fakeAIGateway) ListModels(context.Context) ([]aigateway.Deployment, error) {
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	deployments := slices.Clone(f.shared)
	for id, model := range f.models {
		deployments = append(deployments, aigateway.Deployment{
			ID: id, ModelName: model.ModelName, AccessGroups: model.AccessGroups,
			FromDatabase: true, DatabaseSourceKnown: true, Priced: true,
			Model: model.Model, BaseModel: model.BaseModel, Routing: model.Routing,
			// LiteLLM reports the price table's value for the base model.
			Vision: f.vision[model.BaseModel],
		})
	}
	return deployments, nil
}

func (f *fakeAIGateway) AddModel(_ context.Context, req aigateway.ModelRequest) error {
	if f.addModelErr != nil {
		return f.addModelErr
	}
	if _, ok := f.models[req.ID]; ok {
		return &aigateway.APIError{StatusCode: 500, Message: "Failed to add model to db"}
	}
	f.models[req.ID] = req
	f.added = append(f.added, req.ID)
	return nil
}

func (f *fakeAIGateway) DeleteModel(_ context.Context, id string) error {
	if f.deleteModelErr != nil {
		return f.deleteModelErr
	}
	f.deletedModels = append(f.deletedModels, id)
	delete(f.models, id)
	return nil
}

func (f *fakeAIGateway) PriceTable(context.Context) (aigateway.PriceTable, error) {
	if f.priceErr != nil {
		return aigateway.PriceTable{}, f.priceErr
	}
	return aigateway.PriceTable{Priced: f.priced, Vision: f.vision}, nil
}

func (f *fakeAIGateway) BlockUnpricedModels(context.Context) (bool, error) {
	if f.blockErr != nil {
		return false, f.blockErr
	}
	return f.blockUnpriced, nil
}

func (f *fakeAIGateway) modelNames() []string {
	var names []string
	for _, model := range f.models {
		names = append(names, model.ModelName)
	}
	slices.Sort(names)
	return names
}

func aiGatewayTestConfig() AIGatewayConfig {
	return AIGatewayConfig{
		Enabled: true, URL: testAIGatewayURL, MasterKeySecretName: "cetirun-ai-gateway-auth", MasterKeySecretKey: "master-key",
		DefaultBudget: 10, CopilotProxyImage: testProxyImage,
	}
}

func aiGatewayTestAgent(team string) *cetirunv1alpha1.Agent {
	shared, copilot := true, true
	return &cetirunv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name: "alice", UID: types.UID(testAIGatewayUID), Generation: 1,
			Annotations: map[string]string{}, Finalizers: []string{cetirunv1alpha1.Finalizer},
		},
		Spec: cetirunv1alpha1.AgentSpec{
			Agent:                 cetirunv1alpha1.HarnessSpec{Type: cetirunv1alpha1.AgentTypeOpenClaw},
			User:                  cetirunv1alpha1.UserSpec{Alias: "alice", DisplayName: "Alice"},
			Team:                  team,
			SandboxRef:            testSandboxName,
			ModelAccess:           &cetirunv1alpha1.ModelAccessSpec{SharedModels: &shared, GitHubCopilot: &copilot},
			GatewayTokenSecretRef: "gateway-token",
			Helm:                  cetirunv1alpha1.HelmSpec{Version: "0.1.28", Values: map[string]runtime.RawExtension{}},
		},
		Status: cetirunv1alpha1.AgentStatus{Policy: &cetirunv1alpha1.PolicyStatus{}},
	}
}

func modelAccess(shared, copilot bool) *cetirunv1alpha1.ModelAccessSpec {
	return &cetirunv1alpha1.ModelAccessSpec{SharedModels: &shared, GitHubCopilot: &copilot}
}

func aiGatewayTestTeam(name string) *cetirunv1alpha1.Team {
	return &cetirunv1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Name: name, Generation: 1}, Spec: cetirunv1alpha1.TeamSpec{
		AgentPolicyRef: testPolicyName,
		SandboxRefs:    []string{testSandboxName}, DefaultSandboxRef: testSandboxName,
	}, Status: cetirunv1alpha1.TeamStatus{ObservedGeneration: 1, Conditions: []metav1.Condition{{Type: cetirunv1alpha1.ConditionTeamAccepted, Status: metav1.ConditionTrue, ObservedGeneration: 1}}}}
}

func aiGatewayTestPolicy() *cetirunv1alpha1.AgentPolicy {
	return &cetirunv1alpha1.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: testPolicyName, Generation: 1}, Spec: cetirunv1alpha1.AgentPolicySpec{
		Harnesses: cetirunv1alpha1.AgentPolicyHarnesses{Allowed: []cetirunv1alpha1.AgentType{cetirunv1alpha1.AgentTypeOpenClaw, cetirunv1alpha1.AgentTypeHermes}},
		ModelAccess: cetirunv1alpha1.AgentPolicyModelAccess{
			SharedModels: cetirunv1alpha1.AgentPolicySharedModelsAccess{Allowed: []string{testSharedModelOne}}, Copilot: cetirunv1alpha1.AgentPolicyCopilotAccess{Allowed: true},
			GatewayBudget: &cetirunv1alpha1.AgentPolicyGatewayBudget{DefaultMonthlyUSD: "10", MaxMonthlyUSD: "100"},
		},
	}, Status: cetirunv1alpha1.AgentPolicyStatus{ObservedGeneration: 1, Conditions: []metav1.Condition{{Type: cetirunv1alpha1.ConditionAgentPolicyAccepted, Status: metav1.ConditionTrue, ObservedGeneration: 1}}}}
}

func allowTestPolicyModels(t *testing.T, kube client.Client, models ...string) {
	t.Helper()
	policy := &cetirunv1alpha1.AgentPolicy{}
	if err := kube.Get(context.Background(), types.NamespacedName{Name: testPolicyName}, policy); err != nil {
		t.Fatal(err)
	}
	policy.Spec.ModelAccess.SharedModels.Allowed = append(policy.Spec.ModelAccess.SharedModels.Allowed, models...)
	if err := kube.Update(context.Background(), policy); err != nil {
		t.Fatal(err)
	}
	policy.Status.ObservedGeneration = policy.Generation
	if err := kube.Status().Update(context.Background(), policy); err != nil {
		t.Fatal(err)
	}
}

// disallowTestPolicyModels removes shared models from the test policy's
// allowed list, as allowTestPolicyModels adds them.
func disallowTestPolicyModels(t *testing.T, kube client.Client, models ...string) {
	t.Helper()
	policy := &cetirunv1alpha1.AgentPolicy{}
	if err := kube.Get(context.Background(), types.NamespacedName{Name: testPolicyName}, policy); err != nil {
		t.Fatal(err)
	}
	policy.Spec.ModelAccess.SharedModels.Allowed = slices.DeleteFunc(policy.Spec.ModelAccess.SharedModels.Allowed,
		func(name string) bool { return slices.Contains(models, name) })
	if err := kube.Update(context.Background(), policy); err != nil {
		t.Fatal(err)
	}
	policy.Status.ObservedGeneration = policy.Generation
	if err := kube.Status().Update(context.Background(), policy); err != nil {
		t.Fatal(err)
	}
}

func aiGatewayTestReconciler(t *testing.T, objects ...client.Object) (*AgentReconciler, client.Client, *fakeAIGateway) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, cetirunv1alpha1.AddToScheme, istiov1beta1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	// Gateway tests exercise the real Policy resolver. Add the accepted
	// Team and policy backing every test Agent unless the test supplied them.
	teams := map[string]bool{}
	for _, object := range objects {
		if team, ok := object.(*cetirunv1alpha1.Team); ok {
			teams[team.Name] = true
		}
	}
	for _, object := range objects {
		if agent, ok := object.(*cetirunv1alpha1.Agent); ok && !teams[agent.Spec.Team] {
			objects = append(objects, aiGatewayTestTeam(agent.Spec.Team))
			teams[agent.Spec.Team] = true
		}
	}
	objects = append(objects, aiGatewayTestPolicy())
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(&cetirunv1alpha1.Agent{}, &cetirunv1alpha1.Team{}, &cetirunv1alpha1.AgentPolicy{}).Build()
	gateway := newFakeAIGateway()
	r := &AgentReconciler{
		Client: kube, Scheme: scheme, EventRecorder: record.NewFakeRecorder(100),
		OperatorNamespace: "cetirun-system", ChartVersion: "0.1.28", DomainSuffix: "example.com", IstioRevision: "test",
		DefaultModel: "copilot-model",
		AIGateway:    aiGatewayTestConfig(),
		aiGatewayClientFactory: func(context.Context) (aiGatewayKeyClient, error) {
			return gateway, nil
		},
	}
	for _, object := range objects {
		if agent, ok := object.(*cetirunv1alpha1.Agent); ok {
			installAIGatewayTestBootstrap(t, r, kube, agent)
		}
	}
	return r, kube, gateway
}

func aiGatewayNamespace() *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: instanceNamespace("alice"), UID: "test-agent-namespace-uid"}}
}

func installAIGatewayTestBootstrap(t *testing.T, r *AgentReconciler, kube client.Client, agent *cetirunv1alpha1.Agent) {
	t.Helper()
	namespace := &corev1.Namespace{}
	if err := kube.Get(context.Background(), types.NamespacedName{Name: instanceNamespace(agent.Name)}, namespace); err != nil {
		if apierrors.IsNotFound(err) {
			return
		}
		t.Fatal(err)
	}
	receipt := &sandboxBootstrapReceipt{
		Version: "v1", AgentName: agent.Name, AgentUID: agent.UID, AgentType: agent.Spec.Agent.Type,
		Namespace: namespace.Name, ChartVersion: agent.Spec.Helm.Version,
		Home: runtimebindings.HomeClaim{StorageClass: "test-storage", StorageSize: "1Gi", AccessMode: string(corev1.ReadWriteOnce)},
		Objects: []sandboxBootstrapObject{
			{Kind: "Namespace", Name: namespace.Name, UID: namespace.UID},
			{Kind: "Role", Name: "cetirun-operator-ns-role"},
			{Kind: "RoleBinding", Name: "cetirun-operator-ns-rolebinding"},
			{Kind: "PersistentVolumeClaim", Name: "agent-home"},
			{Kind: "Secret", Name: agent.Spec.GatewayTokenSecretRef},
		},
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	receiptObject := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: r.sandboxBootstrapKey(agent).Name, Namespace: r.OperatorNamespace, UID: types.UID("test-bootstrap-" + agent.Name),
		OwnerReferences: []metav1.OwnerReference{sandboxworkload.AgentOwner(agent.Name, agent.UID)},
		Annotations:     map[string]string{sandboxworkload.AgentUIDAnnotation: string(agent.UID)},
		Finalizers:      []string{sandbox.BootstrapFinalizer},
	}, Data: map[string]string{sandboxBootstrapReceiptKey: string(raw)}}
	if err := kube.Create(context.Background(), receiptObject); err != nil {
		t.Fatal(err)
	}
	desired := r.sandboxBootstrapObjects(agent, receiptObject, receipt)[0].(*corev1.Namespace)
	desired.UID, desired.ResourceVersion = namespace.UID, namespace.ResourceVersion
	if err := kube.Update(context.Background(), desired); err != nil {
		t.Fatal(err)
	}
}

func readSecret(t *testing.T, kube client.Client, name string) *corev1.Secret {
	t.Helper()
	secret := &corev1.Secret{}
	if err := kube.Get(context.Background(), types.NamespacedName{Namespace: instanceNamespace("alice"), Name: name}, secret); err != nil {
		t.Fatalf("read Secret %s: %v", name, err)
	}
	return secret
}

func readKeySecret(t *testing.T, kube client.Client) *corev1.Secret {
	t.Helper()
	return readSecret(t, kube, aiGatewayKeySecretName)
}

func keySecret(uid, value string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: aiGatewayKeySecretName, Namespace: instanceNamespace("alice"),
			Labels: map[string]string{aiGatewayAgentUIDLabel: uid},
		},
		Data: map[string][]byte{aiGatewayKeySecretField: []byte(value)},
	}
}

func mustKeyValue(t *testing.T) string {
	t.Helper()
	value, err := aigateway.NewKeyValue()
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func copilotGroup() string { return aiGatewayCopilotGroupPrefix + testAIGatewayUID }

func TestReconcileAIGatewayCreatesKeyAfterPersistingSecret(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
	gateway.beforeGenerate = func(req aigateway.GenerateRequest) {
		if string(readKeySecret(t, kube).Data[aiGatewayKeySecretField]) != req.Key {
			t.Fatal("the key value must be persisted before the Gateway create request")
		}
	}
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	secret := readKeySecret(t, kube)
	value := string(secret.Data[aiGatewayKeySecretField])
	if !aigateway.ValidKeyValue(value) || secret.Labels[aiGatewayAgentUIDLabel] != testAIGatewayUID {
		t.Fatalf("unexpected key Secret labels=%v", secret.Labels)
	}
	if len(secret.OwnerReferences) != 1 || !metav1.IsControlledBy(secret, agent) ||
		secret.OwnerReferences[0].BlockOwnerDeletion == nil || !*secret.OwnerReferences[0].BlockOwnerDeletion {
		t.Fatalf("key Secret is not controlled by its Agent: %+v", secret.OwnerReferences)
	}
	if len(gateway.generated) != 1 {
		t.Fatalf("generated %d keys, want 1", len(gateway.generated))
	}
	req := gateway.generated[0]
	wantGroups := sortedCopy([]string{testManagedGroup, copilotGroup()})
	if req.Key != value || req.Alias != "cetirun-alice-"+testAIGatewayUID || !slices.Equal(req.Models, wantGroups) ||
		req.MaxBudget != 10 || req.BudgetDuration != "1mo" || req.Metadata["cetirun_agent_uid"] != testAIGatewayUID ||
		req.TeamID != aiGatewayTeamPrefix+testAIGatewayTeam || req.UserID != "alice" {
		t.Fatalf("unexpected create request %+v", req)
	}
	if !gateway.teams[aiGatewayTeamPrefix+testAIGatewayTeam] || !gateway.users["alice"] ||
		!gateway.members[aiGatewayTeamPrefix+testAIGatewayTeam+"/alice"] {
		t.Fatalf("the Gateway team, user and membership must exist before the key: %v %v %v", gateway.teams, gateway.users, gateway.members)
	}
	status := agent.Status.AIGateway
	if status.Phase != cetirunv1alpha1.AIGatewayPhaseReady || status.KeyID != aigateway.Token(value)[:aiGatewayKeyIDLength] ||
		!slices.Equal(status.AccessGroups, wantGroups) || !status.Routed || status.KeySecretName != aiGatewayKeySecretName ||
		status.Team != aiGatewayTeamPrefix+testAIGatewayTeam {
		t.Fatalf("unexpected status %+v", status)
	}
	if !slices.Equal(status.Models, []string{testSharedModelOne}) || status.DefaultModel != testSharedModelOne {
		t.Fatalf("the Agent must be configured with the shared models: %v default %q", status.Models, status.DefaultModel)
	}
	if strings.Contains(status.KeyID, value) || strings.Contains(status.KeyAlias, value) {
		t.Fatal("status must not contain the key value")
	}
	if !meta.IsStatusConditionTrue(agent.Status.Conditions, cetirunv1alpha1.ConditionAIGatewayReady) {
		t.Fatal("AIGatewayReady is not true")
	}

	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	if len(gateway.generated) != 1 || len(gateway.updates) != 0 || len(gateway.ownerUpdates) != 0 {
		t.Fatalf("a steady reconcile must not create or update: generated=%d updates=%d owner=%d",
			len(gateway.generated), len(gateway.updates), len(gateway.ownerUpdates))
	}
}

func TestReconcileAIGatewaySuspensionBlocksAndResumesTheSameKey(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	agent.Spec.Suspended = true
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	value := string(readKeySecret(t, kube).Data[aiGatewayKeySecretField])
	token := aigateway.Token(value)
	if len(gateway.generated) != 1 || !gateway.generated[0].Blocked || !gateway.keys[token].Blocked {
		t.Fatalf("a suspended Agent must create a blocked key: request=%+v key=%+v", gateway.generated, gateway.keys[token])
	}
	spend := 3.25
	gateway.keys[token].Spend = &spend
	agent.Spec.Suspended = false
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	if len(gateway.generated) != 1 || gateway.keys[token].Blocked || gateway.keys[token].Spend == nil || *gateway.keys[token].Spend != spend {
		t.Fatalf("resume must unblock the existing key without losing spend: generated=%d key=%+v", len(gateway.generated), gateway.keys[token])
	}
	if agent.Status.AIGateway.Blocked || agent.Status.AIGateway.SpendUSD != "3.25" {
		t.Fatalf("status must read back the resumed key observations: %+v", agent.Status.AIGateway)
	}
}

func TestReconcileAIGatewayNeverClaimsOrUnblocksAnExternalSuspendBlock(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	token := aigateway.Token(string(readKeySecret(t, kube).Data[aiGatewayKeySecretField]))
	gateway.keys[token].Blocked = true // administrator emergency block
	agent.Spec.Suspended = true
	agent.Status.AIGateway.Reason = ""
	if err := r.reconcileAIGateway(context.Background(), agent, true); err == nil {
		t.Fatal("suspension must not claim an externally blocked key")
	}
	if !gateway.keys[token].Blocked || agent.Status.AIGateway.Reason == aiGatewayReasonSuspended {
		t.Fatalf("external block must remain distinguishable: key=%+v status=%+v", gateway.keys[token], agent.Status.AIGateway)
	}
	agent.Spec.Suspended = false
	if err := r.reconcileAIGateway(context.Background(), agent, true); err == nil || !gateway.keys[token].Blocked {
		t.Fatal("resume must never unblock an externally blocked key")
	}
}

func TestReconcileAIGatewayNoEntitlementBlocksExistingKeyWithoutReplacingIt(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	token := aigateway.Token(string(readKeySecret(t, kube).Data[aiGatewayKeySecretField]))
	spend := 4.5
	gateway.keys[token].Spend = &spend
	agent.Spec.ModelAccess = modelAccess(false, false)
	if err := r.reconcileAIGateway(context.Background(), agent, true); err == nil {
		t.Fatal("no entitlement must stop the workload")
	}
	if len(gateway.generated) != 1 || !gateway.keys[token].Blocked || gateway.keys[token].Spend == nil || *gateway.keys[token].Spend != spend {
		t.Fatalf("no entitlement must block the same key without losing spend: generated=%d key=%+v", len(gateway.generated), gateway.keys[token])
	}
	if agent.Status.AIGateway.Routed || !agent.Status.AIGateway.Blocked || agent.Status.AIGateway.Reason != aiGatewayReasonNoModels {
		t.Fatalf("no entitlement must report a blocked, unrouted hold: %+v", agent.Status.AIGateway)
	}
}

func TestReconcileAIGatewayNoEntitlementNeverClaimsAnAdministratorBlock(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	token := aigateway.Token(string(readKeySecret(t, kube).Data[aiGatewayKeySecretField]))
	gateway.keys[token].Blocked = true
	agent.Status.AIGateway.Reason = ""
	agent.Spec.ModelAccess = modelAccess(false, false)
	if err := r.reconcileAIGateway(context.Background(), agent, true); err == nil {
		t.Fatal("no-entitlement reconciliation claimed an administrator block")
	}
	if !gateway.keys[token].Blocked || agent.Status.AIGateway.Reason == aiGatewayReasonNoModels {
		t.Fatalf("administrator block lost its ownership boundary: key=%+v status=%+v", gateway.keys[token], agent.Status.AIGateway)
	}
	agent.Spec.ModelAccess = modelAccess(true, true)
	if err := r.reconcileAIGateway(context.Background(), agent, true); err == nil || !gateway.keys[token].Blocked {
		t.Fatal("restoring entitlement must not clear an administrator block")
	}
}

func TestReconcileAIGatewayNoEntitlementCannotClaimBlockWhenRecordedSecretIsMissing(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	agent.Status.AIGateway = &cetirunv1alpha1.AIGatewayStatus{KeyID: "previous-key", KeyAlias: aiGatewayKeyAlias(agent)}
	agent.Spec.ModelAccess = modelAccess(false, false)
	r, _, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
	if err := r.reconcileAIGateway(context.Background(), agent, true); err == nil {
		t.Fatal("a missing Secret for a recorded key must not be treated as blocked")
	}
	if len(gateway.generated) != 0 || agent.Status.AIGateway.Reason != "KeyBlockFailed" || agent.Status.AIGateway.Blocked {
		t.Fatalf("block evidence must not be fabricated: generated=%d status=%+v", len(gateway.generated), agent.Status.AIGateway)
	}
}

func TestReconcileAIGatewayGroupsFollowModelAccessSwitches(t *testing.T) {
	for name, tc := range map[string]struct {
		access   *cetirunv1alpha1.ModelAccessSpec
		want     []string
		inactive bool
	}{
		"both requested": {modelAccess(true, true), sortedCopy([]string{testManagedGroup, copilotGroup()}), false},
		"shared only":    {modelAccess(true, false), []string{testManagedGroup}, false},
		"copilot only":   {modelAccess(false, true), []string{copilotGroup()}, false},
		"neither":        {modelAccess(false, false), nil, true},
	} {
		t.Run(name, func(t *testing.T) {
			agent := aiGatewayTestAgent(testAIGatewayTeam)
			agent.Spec.ModelAccess = tc.access
			r, _, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
			if tc.inactive {
				if err := r.reconcileAIGateway(context.Background(), agent, true); err == nil {
					t.Fatal("no entitlement must hold the workload before a direct-provider render")
				}
				if len(gateway.generated) != 0 || agent.Status.AIGateway.Reason != aiGatewayReasonNoModels {
					t.Fatalf("no entitlement must not create a key: generated=%d status=%+v", len(gateway.generated), agent.Status.AIGateway)
				}
				return
			}
			if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
				t.Fatal(err)
			}
			if got := gateway.generated[0].Models; !slices.Equal(got, tc.want) {
				t.Fatalf("key models %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReconcileAIGatewayWithoutProxyImageRejectsRequestedCopilot(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, _, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
	r.AIGateway.CopilotProxyImage = ""
	if err := r.reconcileAIGateway(context.Background(), agent, true); err == nil {
		t.Fatal("a requested policy-allowed Copilot source must fail when the deployment offers no proxy")
	}
	if len(gateway.generated) != 0 {
		t.Fatal("a rejected Copilot request must not create a partial key")
	}
}

func TestReconcileAIGatewayMovesKeyWhenTeamChanges(t *testing.T) {
	agent := aiGatewayTestAgent("team-a")
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	if err := kube.Create(context.Background(), aiGatewayTestTeam("team-b")); err != nil {
		t.Fatal(err)
	}
	agent.Spec.Team = "team-b"
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	if len(gateway.ownerUpdates) != 1 || gateway.ownerUpdates[0] != [2]string{aiGatewayTeamPrefix + "team-b", "alice"} {
		t.Fatalf("the key must move to the new team after the user joins it: %v", gateway.ownerUpdates)
	}
	for _, key := range gateway.keys {
		if key.MaxBudget == nil || *key.MaxBudget != 10 {
			t.Fatalf("the budget must be left as created, got %v", key.MaxBudget)
		}
	}
	if agent.Status.AIGateway.Team != aiGatewayTeamPrefix+"team-b" {
		t.Fatalf("status team %q", agent.Status.AIGateway.Team)
	}
}

func TestReconcileAIGatewayLetsKeysIssuedBeforeTheModelLookupLookUpModels(t *testing.T) {
	agent := aiGatewayTestAgent("team-a")
	r, _, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	if len(gateway.routeUpdates) != 0 {
		t.Fatalf("a new key already has the routes: %v", gateway.routeUpdates)
	}
	// A key issued by an earlier operator may call the inference routes only.
	for _, key := range gateway.keys {
		key.AllowedRoutes = []string{aigateway.LLMAPIRoutes}
	}
	for range 2 {
		if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
			t.Fatal(err)
		}
	}
	if len(gateway.routeUpdates) != 1 || !slices.Equal(gateway.routeUpdates[0], aigateway.KeyRoutes) {
		t.Fatalf("the key must get the model lookup once: %v", gateway.routeUpdates)
	}
	if len(gateway.generated) != 1 {
		t.Fatalf("the key must be updated in place, not replaced: %d keys generated", len(gateway.generated))
	}
}

func TestReconcileAIGatewayRejectsOwnerUpdateWithoutMatchingReadback(t *testing.T) {
	agent := aiGatewayTestAgent("team-a")
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	if err := kube.Create(context.Background(), aiGatewayTestTeam("team-b")); err != nil {
		t.Fatal(err)
	}
	gateway.ignoreOwnerUpdate = true
	agent.Spec.Team = "team-b"
	if err := r.reconcileAIGateway(context.Background(), agent, true); err == nil {
		t.Fatal("an owner update that the Gateway did not store must fail closed")
	}
	if agent.Status.AIGateway.Reason != "KeyReadbackMismatch" || agent.Status.AIGateway.Phase != cetirunv1alpha1.AIGatewayPhaseDegraded {
		t.Fatalf("unexpected status after owner read-back mismatch: %+v", agent.Status.AIGateway)
	}
	if agent.Status.AIGateway.Team == aiGatewayTeamPrefix+"team-b" {
		t.Fatal("status must not claim the requested owner without Gateway evidence")
	}
}

func TestReconcileAIGatewayAdoptsKeyCreatedBeforeStatusWasLost(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	value := mustKeyValue(t)
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace(), keySecret(testAIGatewayUID, value))
	budget := 25.0
	gateway.keys[aigateway.Token(value)] = &aigateway.Key{
		Alias: aiGatewayKeyAlias(agent), Models: sortedCopy([]string{testManagedGroup, copilotGroup()}), MaxBudget: &budget,
		TeamID: aiGatewayTeamPrefix + testAIGatewayTeam, UserID: "alice",
	}
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	if len(gateway.generated) != 0 || len(gateway.deletedAliases) != 0 || len(gateway.updates) != 0 {
		t.Fatal("an existing key for the persisted value must be adopted, not recreated")
	}
	if agent.Status.AIGateway.KeyID != aigateway.Token(value)[:aiGatewayKeyIDLength] {
		t.Fatal("adopted key is not recorded")
	}
	secret := readKeySecret(t, kube)
	if len(secret.OwnerReferences) != 1 || !metav1.IsControlledBy(secret, agent) ||
		secret.Annotations[aiGatewayKeyAliasAnnotation] != aiGatewayKeyAlias(agent) {
		t.Fatalf("adopted key Secret ownership was not repaired: owners=%+v annotations=%v",
			secret.OwnerReferences, secret.Annotations)
	}
}

func TestEnsureAIGatewayKeySecretRepairsIssuedSecretOwnership(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	value := mustKeyValue(t)
	agent.Status.AIGateway = &cetirunv1alpha1.AIGatewayStatus{KeyID: aigateway.Token(value)[:aiGatewayKeyIDLength]}
	r, kube, _ := aiGatewayTestReconciler(t, agent, aiGatewayNamespace(), keySecret(testAIGatewayUID, value))

	got, err := r.ensureAIGatewayKeySecret(context.Background(), agent, false)
	if err != nil || got != value {
		t.Fatalf("repair issued key Secret: value matched=%t error=%v", got == value, err)
	}
	secret := readKeySecret(t, kube)
	if len(secret.OwnerReferences) != 1 || !metav1.IsControlledBy(secret, agent) ||
		secret.Annotations[aiGatewayKeyAliasAnnotation] != aiGatewayKeyAlias(agent) {
		t.Fatalf("issued key Secret ownership was not repaired: owners=%+v annotations=%v",
			secret.OwnerReferences, secret.Annotations)
	}
}

// When the key Secret no longer holds the value of this Agent's existing key,
// replacing the key would reset its spend and budget (and unblock it), so the
// operator reports the state and leaves the key alone until an administrator
// restores the Secret or deletes the key.
func TestReconcileAIGatewayKeepsOwnKeyWhenItsValueIsLost(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	value := mustKeyValue(t)
	r, _, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace(), keySecret(testAIGatewayUID, value))
	budget := 25.0
	gateway.keys["token-of-a-value-no-longer-held"] = &aigateway.Key{
		Alias: aiGatewayKeyAlias(agent), Models: []string{aiGatewaySharedAccessGroup}, MaxBudget: &budget, Blocked: true,
	}
	for pass := 1; pass <= 2; pass++ {
		if err := r.reconcileAIGateway(context.Background(), agent, true); err == nil {
			t.Fatalf("pass %d: a lost key value must be reported", pass)
		}
	}
	status := agent.Status.AIGateway
	if status.Phase != cetirunv1alpha1.AIGatewayPhaseDegraded || status.Reason != aiGatewayReasonKeySecretLost {
		t.Fatalf("status = %s/%s", status.Phase, status.Reason)
	}
	if len(gateway.deletedAliases) != 0 || len(gateway.deletedTokens) != 0 || len(gateway.generated) != 0 {
		t.Fatalf("the existing key must be left alone: deleted=%v/%v generated=%d", gateway.deletedAliases, gateway.deletedTokens, len(gateway.generated))
	}
	if key, ok := gateway.keys["token-of-a-value-no-longer-held"]; !ok || !key.Blocked || *key.MaxBudget != budget {
		t.Fatal("the existing key and its budget state must survive")
	}
	events := r.EventRecorder.(*record.FakeRecorder).Events
	lost := 0
	for len(events) > 0 {
		if strings.Contains(<-events, "AIGatewayKeySecretLost") {
			lost++
		}
	}
	if lost != 1 {
		t.Fatalf("the lost value must be reported once, got %d events", lost)
	}

	// An administrator deletes the old key: the next pass creates this one.
	delete(gateway.keys, "token-of-a-value-no-longer-held")
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	if len(gateway.generated) != 1 || gateway.generated[0].Key != value || agent.Status.AIGateway.Phase != cetirunv1alpha1.AIGatewayPhaseReady {
		t.Fatalf("after the administrator's decision the key must be created: generated=%d phase=%s", len(gateway.generated), agent.Status.AIGateway.Phase)
	}
}

func TestReconcileAIGatewayDoesNotReplaceRecordedKeyWhenSecretIsMissing(t *testing.T) {
	for name, aliasExists := range map[string]bool{"alias exists": true, "alias temporarily absent": false} {
		t.Run(name, func(t *testing.T) {
			agent := aiGatewayTestAgent(testAIGatewayTeam)
			agent.Status.AIGateway = &cetirunv1alpha1.AIGatewayStatus{KeyID: "previous-key", KeyAlias: aiGatewayKeyAlias(agent)}
			r, _, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
			if aliasExists {
				budget := 10.0
				gateway.keys["not-the-lost-token"] = &aigateway.Key{Alias: aiGatewayKeyAlias(agent), Models: []string{testManagedGroup}, MaxBudget: &budget, BudgetDuration: aiGatewayBudgetDuration}
			}
			if err := r.reconcileAIGateway(context.Background(), agent, true); err == nil {
				t.Fatal("a missing Secret for a recorded key must fail closed")
			}
			if len(gateway.generated) != 0 || agent.Status.AIGateway.Reason != aiGatewayReasonKeySecretLost {
				t.Fatalf("the recorded key must not be replaced: generated=%d status=%+v", len(gateway.generated), agent.Status.AIGateway)
			}
		})
	}
}

func TestReconcileAIGatewayDoesNotRecreateRecordedKeyMissingFromGateway(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	value := mustKeyValue(t)
	agent.Status.AIGateway = &cetirunv1alpha1.AIGatewayStatus{
		KeyID: value[:aiGatewayKeyIDLength], KeyAlias: aiGatewayKeyAlias(agent),
		KeySecretName: aiGatewayKeySecretName, Routed: true,
	}
	r, _, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace(), keySecret(testAIGatewayUID, value))
	if err := r.reconcileAIGateway(context.Background(), agent, true); err == nil {
		t.Fatal("a missing Gateway record for a previously issued key must fail closed")
	}
	if len(gateway.generated) != 0 || agent.Status.AIGateway.Reason != aiGatewayReasonKeyRecordLost {
		t.Fatalf("the missing record must not reset spend or block state: generated=%d status=%+v", len(gateway.generated), agent.Status.AIGateway)
	}
}

func TestReconcileAIGatewayReplacesSecretOfAnotherAgent(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	stale := mustKeyValue(t)
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace(), keySecret("00000000-0000-0000-0000-000000000000", stale))
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	secret := readKeySecret(t, kube)
	if string(secret.Data[aiGatewayKeySecretField]) == stale || secret.Labels[aiGatewayAgentUIDLabel] != testAIGatewayUID {
		t.Fatal("a Secret written for another Agent UID must get a new value")
	}
	if gateway.generated[0].Key == stale {
		t.Fatal("the stale value must not be registered")
	}
}

func TestReconcileAIGatewayNeverAdoptsAnotherAlias(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	value := mustKeyValue(t)
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace(), keySecret(testAIGatewayUID, value))
	foreign := &aigateway.Key{Alias: "someone-else", Models: []string{"model-x"}}
	gateway.keys[aigateway.Token(value)] = foreign
	err := r.reconcileAIGateway(context.Background(), agent, true)
	if err == nil || agent.Status.AIGateway.Reason != "KeyAliasMismatch" {
		t.Fatalf("want KeyAliasMismatch, got %v / %+v", err, agent.Status.AIGateway)
	}
	if string(readKeySecret(t, kube).Data[aiGatewayKeySecretField]) == value {
		t.Fatal("the Secret must get a new value")
	}
	if len(gateway.updates) != 0 || len(gateway.deletedTokens) != 0 || len(gateway.deletedAliases) != 0 ||
		!slices.Equal(foreign.Models, []string{"model-x"}) {
		t.Fatal("a key with another alias must not be changed or deleted")
	}
}

func TestReconcileAIGatewayOutageKeepsCheckpoint(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
	gateway.getErr = errors.New("synthetic outage")
	err := r.reconcileAIGateway(context.Background(), agent, true)
	if err == nil || agent.Status.AIGateway.Phase != cetirunv1alpha1.AIGatewayPhaseDegraded || agent.Status.AIGateway.Reason != "GatewayUnavailable" {
		t.Fatalf("want Degraded/GatewayUnavailable, got %v %+v", err, agent.Status.AIGateway)
	}
	if agent.Status.AIGateway.Routed {
		t.Fatal("an Agent must not be routed before its key exists")
	}
	first := string(readKeySecret(t, kube).Data[aiGatewayKeySecretField])
	gateway.getErr = nil
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	if gateway.generated[0].Key != first {
		t.Fatal("the persisted value must be reused after the outage")
	}
}

func TestReconcileAIGatewayRequiresChartSupportForRouting(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, _, _ := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
	if err := r.reconcileAIGateway(context.Background(), agent, false); err != nil {
		t.Fatal(err)
	}
	condition := meta.FindStatusCondition(agent.Status.Conditions, cetirunv1alpha1.ConditionAIGatewayReady)
	if agent.Status.AIGateway.Routed || condition == nil || !strings.Contains(condition.Message, "chart version") {
		t.Fatalf("an Agent on a chart without AI Gateway support must not be routed: %+v %+v", agent.Status.AIGateway, condition)
	}
	// A governed virtual key can still contain the Copilot group while its
	// pinned chart is unable to consume the routing values. The Agent
	// controller, not this key reconciler, holds that workload fail-closed.
}

// Hermes takes its models from the Gateway like OpenClaw on a chart that
// carries Hermes' Gateway provider: both model sources and a Copilot proxy.
// On a chart that routes only OpenClaw it gets a key and keeps its provider.
func TestReconcileAIGatewayRoutesHermesOnAChartThatSupportsIt(t *testing.T) {
	for _, chartSupported := range []bool{true, false} {
		t.Run(fmt.Sprintf("chart support %v", chartSupported), func(t *testing.T) {
			agent := aiGatewayTestAgent(testAIGatewayTeam)
			agent.Spec.Agent.Type = cetirunv1alpha1.AgentTypeHermes
			r, _, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
			if err := r.reconcileAIGateway(context.Background(), agent, chartSupported); err != nil {
				t.Fatal(err)
			}
			if len(gateway.generated) != 1 || agent.Status.AIGateway.Routed != chartSupported {
				t.Fatalf("generated=%d status=%+v", len(gateway.generated), agent.Status.AIGateway)
			}
			want := sortedCopy([]string{testManagedGroup, copilotGroup()})
			if got := gateway.generated[0].Models; !slices.Equal(got, want) {
				t.Fatalf("Hermes key models %v, want %v", got, want)
			}
			if (agent.Status.AIGateway.Copilot != nil) != chartSupported {
				t.Fatalf("Copilot proxy status %+v", agent.Status.AIGateway.Copilot)
			}
		})
	}
}

// The chart decides whether a harness can be routed: every harness needs the
// model list and the Copilot proxy, and Hermes the variable that carries its
// key, which charts routing only OpenClaw do not declare.
func TestAIGatewayChartSupports(t *testing.T) {
	gateway := [][]string{{aiGatewayChartValue, aiGatewayChartModelsValue}, {aiGatewayChartValue, aiGatewayChartCopilotValue}}
	hermes := []string{aiGatewayChartValue, aiGatewayChartHermesValue, aiGatewayChartHermesKeyEnvValue}
	for _, tc := range []struct {
		name      string
		declared  [][]string
		agentType cetirunv1alpha1.AgentType
		want      bool
	}{
		{"OpenClaw on a Gateway chart", gateway, cetirunv1alpha1.AgentTypeOpenClaw, true},
		{"Hermes on a chart routing only OpenClaw", gateway, cetirunv1alpha1.AgentTypeHermes, false},
		{"Hermes on a chart with its key variable", append(slices.Clone(gateway), hermes), cetirunv1alpha1.AgentTypeHermes, true},
		{"OpenClaw on a chart before the Gateway", nil, cetirunv1alpha1.AgentTypeOpenClaw, false},
		{"Hermes with its key variable but no proxy", [][]string{gateway[0], hermes}, cetirunv1alpha1.AgentTypeHermes, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := aiGatewayChartSupports(declaredValues(tc.declared), tc.agentType); got != tc.want {
				t.Fatalf("aiGatewayChartSupports = %v, want %v", got, tc.want)
			}
		})
	}
}

// declaredValues is a chart that declares exactly the given value paths.
type declaredValues [][]string

func (d declaredValues) SupportsValuePath(path ...string) bool {
	for _, declared := range d {
		if slices.Equal(declared, path) {
			return true
		}
	}
	return false
}

func readyProxy(ready int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "openclaw-alice-copilot-proxy", Namespace: instanceNamespace("alice")},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: ready},
	}
}

// signIn stores a completed sign-in as the Portal does: the token, its
// generation and its completion on the token Secret, and the models with the
// same generation on the Agent.
func signIn(t *testing.T, kube client.Client, agent *cetirunv1alpha1.Agent, models string) {
	t.Helper()
	const generation = "0a1b2c3d"
	secret := readSecret(t, kube, cetirunv1alpha1.CopilotTokenSecretName)
	secret.Data = map[string][]byte{cetirunv1alpha1.CopilotTokenSecretKey: []byte("gho_synthetic")}
	secret.Annotations = map[string]string{
		cetirunv1alpha1.AnnotationCopilotSignInGeneration: generation,
		cetirunv1alpha1.AnnotationCopilotSignInCompleted:  generation,
	}
	if err := kube.Update(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	if agent.Annotations == nil {
		agent.Annotations = map[string]string{}
	}
	agent.Annotations[cetirunv1alpha1.AnnotationCopilotModels] = models
	agent.Annotations[cetirunv1alpha1.AnnotationCopilotSignInGeneration] = generation
}

func TestReconcileAIGatewayCopilotSignInRegistersPricedModels(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	copilot := agent.Status.AIGateway.Copilot
	if copilot == nil || copilot.Phase != cetirunv1alpha1.AIGatewayCopilotPhaseSignInRequired || copilot.AccessGroup != copilotGroup() {
		t.Fatalf("before sign-in the proxy waits: %+v", copilot)
	}
	proxyKey := string(readSecret(t, kube, aiGatewayProxySecretName).Data[aiGatewayProxySecretField])
	if !aigateway.ValidKeyValue(proxyKey) {
		t.Fatal("the proxy master key Secret was not created")
	}
	if token := readSecret(t, kube, cetirunv1alpha1.CopilotTokenSecretName); len(token.Data) != 0 || token.Labels[aiGatewayAgentUIDLabel] != testAIGatewayUID {
		t.Fatalf("the token Secret must exist empty for the proxy to mount: %+v", token)
	}
	if agent.Status.AIGateway.DefaultModel != testSharedModelOne {
		t.Fatalf("before sign-in the shared model is the default, got %q", agent.Status.AIGateway.DefaultModel)
	}

	signIn(t, kube, agent, testCopilotModels)
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	if agent.Status.AIGateway.Copilot.Phase != cetirunv1alpha1.AIGatewayCopilotPhaseStarting || len(gateway.models) != 0 {
		t.Fatalf("routes wait for the proxy to be ready: %+v models=%d", agent.Status.AIGateway.Copilot, len(gateway.models))
	}

	if err := kube.Create(context.Background(), readyProxy(1)); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	copilot = agent.Status.AIGateway.Copilot
	if copilot.Phase != cetirunv1alpha1.AIGatewayCopilotPhaseReady {
		t.Fatalf("copilot status %+v", copilot)
	}
	// A Copilot model that is no shared model's real model keeps its own ID.
	wantNames := []string{"claude-sonnet-4.5", "gpt-5.5"}
	if !slices.Equal(copilot.Models, wantNames) || !slices.Equal(gateway.modelNames(), wantNames) {
		t.Fatalf("registered %v, gateway %v", copilot.Models, gateway.modelNames())
	}
	if !slices.Equal(copilot.Unpriced, []string{"grok-code-fast-1"}) {
		t.Fatalf("an unpriced model must be reported, not registered: %v", copilot.Unpriced)
	}
	for id, model := range gateway.models {
		if !strings.HasPrefix(id, copilotGroup()+"-") || !slices.Equal(model.AccessGroups, []string{copilotGroup()}) ||
			model.APIBase != "http://openclaw-alice-copilot-proxy.cetirun-alice.svc.cluster.local:4000" || model.APIKey != proxyKey ||
			model.Model != "litellm_proxy/"+strings.TrimPrefix(model.ModelName, "copilot/") {
			t.Fatalf("unexpected route %s: %+v", id, model)
		}
		if !model.NoCooldown || !model.Routing.IsZero() {
			t.Fatalf("a route of its own is never cooled down and copies no routing: %+v", model)
		}
		if model.ModelName == "claude-sonnet-4.5" && model.BaseModel != "claude-sonnet-4-5" {
			t.Fatalf("Copilot's dotted Claude name must be priced by the dashed entry, got %q", model.BaseModel)
		}
	}
	wantModels := []string{testSharedModelOne, "claude-sonnet-4.5", "gpt-5.5"}
	if !slices.Equal(agent.Status.AIGateway.Models, wantModels) {
		t.Fatalf("agent models %v, want %v", agent.Status.AIGateway.Models, wantModels)
	}

	// Steady state: nothing is re-registered.
	added := len(gateway.added)
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	if len(gateway.added) != added {
		t.Fatal("a steady reconcile must not re-register routes")
	}

	// The account loses a model: its route goes.
	agent.Annotations[cetirunv1alpha1.AnnotationCopilotModels] = `[{"id":"gpt-5.5"}]`
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(gateway.modelNames(), []string{"gpt-5.5"}) || !slices.Equal(agent.Status.AIGateway.Copilot.Models, []string{"gpt-5.5"}) {
		t.Fatalf("stale routes must be removed: gateway %v status %v", gateway.modelNames(), agent.Status.AIGateway.Copilot.Models)
	}

	// The Gateway lost a route: the catalog shows it gone and it comes back.
	for id := range gateway.models {
		delete(gateway.models, id)
	}
	r.aiGatewayCatalog = aiGatewayCatalog{}
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(gateway.modelNames(), []string{"gpt-5.5"}) {
		t.Fatalf("a lost route must be re-added, gateway %v", gateway.modelNames())
	}

	// The owner turns Copilot off: routes, token and proxy key go.
	agent.Spec.ModelAccess = modelAccess(true, false)
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	if len(gateway.models) != 0 || agent.Status.AIGateway.Copilot != nil {
		t.Fatalf("turning Copilot off removes its routes: %v %+v", gateway.modelNames(), agent.Status.AIGateway.Copilot)
	}
	for _, name := range []string{cetirunv1alpha1.CopilotTokenSecretName, aiGatewayProxySecretName} {
		if err := kube.Get(context.Background(), types.NamespacedName{Namespace: instanceNamespace("alice"), Name: name}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
			t.Fatalf("Secret %s must be deleted: %v", name, err)
		}
	}
	if slices.Contains(gateway.generated[0].Models, copilotGroup()) && slices.Contains(lastKeyModels(gateway), copilotGroup()) {
		t.Fatal("the key must lose the Copilot group")
	}
}

// A route registered just before the operator stopped, before the status
// recorded it, is known only to the Gateway. Once the proxy key or the price
// table changes, its revision is obsolete, and it would keep serving the
// Agent's model name with the old key. A reconcile removes it as soon as the
// catalog has listed it; other Agents' routes stay.
func TestReconcileAIGatewayCopilotRemovesUnrecordedObsoleteRoutes(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace(), readyProxy(1))
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	signIn(t, kube, agent, `[{"id":"gpt-5.5"}]`)
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(gateway.modelNames(), []string{"gpt-5.5"}) {
		t.Fatalf("setup: gateway %v", gateway.modelNames())
	}
	obsolete := copilotRegistrationID(agent, "gpt-5.5", "0badc0de")
	gateway.models[obsolete] = aigateway.ModelRequest{ID: obsolete, ModelName: "gpt-5.5", APIKey: "sk-obsolete", AccessGroups: []string{copilotGroup()}}
	other := aiGatewayCopilotGroupPrefix + "another-agent-uid-gpt-5.5-0badc0de"
	gateway.models[other] = aigateway.ModelRequest{ID: other, ModelName: "gpt-5.5", AccessGroups: []string{aiGatewayCopilotGroupPrefix + "another-agent-uid"}}
	if slices.Contains(agent.Status.AIGateway.Copilot.Registered, obsolete) {
		t.Fatal("setup: the obsolete route must not be recorded")
	}

	// Policy grant resolution always obtains a live catalog. It can judge
	// and remove an unrecorded obsolete route in this first post-restart pass.
	r.aiGatewayCatalog = aiGatewayCatalog{}
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	if _, ok := gateway.models[obsolete]; ok {
		t.Fatalf("the unrecorded obsolete route must be removed: %v", gateway.deletedModels)
	}
	if _, ok := gateway.models[other]; !ok {
		t.Fatal("another Agent's route must stay")
	}
	if names := gateway.modelNames(); !slices.Equal(names, []string{"gpt-5.5", "gpt-5.5"}) {
		t.Fatalf("this Agent's current route must stay: %v", names)
	}
	if copilot := agent.Status.AIGateway.Copilot; copilot.Phase != cetirunv1alpha1.AIGatewayCopilotPhaseReady ||
		slices.Contains(copilot.Registered, obsolete) || len(copilot.Registered) != 1 {
		t.Fatalf("copilot status %+v", copilot)
	}
}

func lastKeyModels(gateway *fakeAIGateway) []string {
	for _, key := range gateway.keys {
		return key.Models
	}
	return nil
}

func TestReconcileAIGatewayCopilotSignedInWithoutModelListIsDegraded(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, kube, _ := aiGatewayTestReconciler(t, agent, aiGatewayNamespace(), readyProxy(1))
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	signIn(t, kube, agent, "")
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	if copilot := agent.Status.AIGateway.Copilot; copilot.Phase != cetirunv1alpha1.AIGatewayCopilotPhaseDegraded || copilot.Reason != "ModelListMissing" {
		t.Fatalf("copilot status %+v", copilot)
	}
}

func TestReconcileAIGatewayClearsTokenSecretOfAnotherAgent(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	stale := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: cetirunv1alpha1.CopilotTokenSecretName, Namespace: instanceNamespace("alice"),
			Labels: map[string]string{aiGatewayAgentUIDLabel: "00000000-0000-0000-0000-000000000000"}},
		Data: map[string][]byte{cetirunv1alpha1.CopilotTokenSecretKey: []byte("gho_previous_agent")},
	}
	r, kube, _ := aiGatewayTestReconciler(t, agent, aiGatewayNamespace(), stale)
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	secret := readSecret(t, kube, cetirunv1alpha1.CopilotTokenSecretName)
	if len(secret.Data) != 0 || secret.Labels[aiGatewayAgentUIDLabel] != testAIGatewayUID {
		t.Fatal("a same-name replacement must not inherit the previous Agent's sign-in")
	}
	if agent.Status.AIGateway.Copilot.Phase != cetirunv1alpha1.AIGatewayCopilotPhaseSignInRequired {
		t.Fatalf("copilot status %+v", agent.Status.AIGateway.Copilot)
	}
}

func TestAIGatewayModelEntriesChooseDefault(t *testing.T) {
	r, _, gateway := aiGatewayTestReconciler(t)
	r.refreshAIGatewayCatalogWithin(context.Background(), gateway, 0)
	agent := routedAgent()
	agent.Status.AIGateway.Copilot = &cetirunv1alpha1.AIGatewayCopilotStatus{Models: []string{"copilot/gpt-5.5"}}
	agent.Annotations = map[string]string{cetirunv1alpha1.AnnotationCopilotModels: testCopilotModels}

	entries, def, _ := r.aiGatewayModelEntries(agent)
	if def != testSharedModelOne || len(entries) != 2 || entries[1].ID != "copilot/gpt-5.5" ||
		entries[1].ContextWindow != 400000 || entries[1].MaxTokens != 128000 || entries[0].ContextWindow != 1000000 {
		t.Fatalf("entries %+v default %q", entries, def)
	}
	agent.Spec.Model = "copilot/gpt-5.5"
	if _, def, _ := r.aiGatewayModelEntries(agent); def != "copilot/gpt-5.5" {
		t.Fatalf("the Agent's own available choice wins, got %q", def)
	}
	agent.Spec.Model = "not-available"
	r.AIGateway.DefaultModel = "copilot/gpt-5.5"
	if _, def, _ := r.aiGatewayModelEntries(agent); def != "copilot/gpt-5.5" {
		t.Fatalf("an unavailable choice falls back to the deployment default, got %q", def)
	}
	agent.Spec.ModelAccess = modelAccess(false, true)
	agent.Status.AIGateway.Copilot.Models = nil
	if entries, def, _ := r.aiGatewayModelEntries(agent); def != aiGatewaySignInRequiredModel || len(entries) != 1 {
		t.Fatalf("a Copilot-only Agent before sign-in gets the placeholder: %+v %q", entries, def)
	}
	agent.Spec.ModelAccess = modelAccess(false, false)
	if _, def, _ := r.aiGatewayModelEntries(agent); def != aiGatewayNoModelsModel {
		t.Fatalf("an Agent without sources gets the no-models placeholder, got %q", def)
	}
}

// manySharedModels adds count shared models, model-000 onwards, to the Gateway.
func manySharedModels(gateway *fakeAIGateway, count int) []string {
	groups := make([]string, 0, count)
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("model-%03d", i)
		group := aiGatewayModelAccessGroupPrefix + aiGatewayModelBinding(name)
		gateway.shared = append(gateway.shared, aigateway.Deployment{
			ID: managedTestDeploymentID(name, i), ModelName: name,
			AccessGroups: []string{aiGatewaySharedAccessGroup, group}, DatabaseSourceKnown: true,
		})
		groups = append(groups, group)
	}
	return groups
}

// managedTestDeploymentID is a deployment ID of the form the Gateway chart
// renders for name, with a synthetic configuration hash taken from entry.
func managedTestDeploymentID(name string, entry int) string {
	return fmt.Sprintf("%s%s-%016x", aiGatewaySharedDeploymentIDPrefix, aiGatewayModelBinding(name), entry)
}

func TestAIGatewayModelEntriesKeepTheConfigurationWithinTheChartLimit(t *testing.T) {
	r, _, gateway := aiGatewayTestReconciler(t)
	groups := manySharedModels(gateway, 300)
	r.refreshAIGatewayCatalogWithin(context.Background(), gateway, 0)
	agent := routedAgent()
	agent.Status.AIGateway.AccessGroups = append(agent.Status.AIGateway.AccessGroups, groups...)
	copilot := []string{"copilot/claude-sonnet-4.5", "copilot/gpt-5.5"}
	agent.Status.AIGateway.Copilot = &cetirunv1alpha1.AIGatewayCopilotStatus{Models: copilot}
	agent.Spec.Model = "model-299"
	r.AIGateway.DefaultModel = "model-298"

	entries, def, omitted := r.aiGatewayModelEntries(agent)
	if len(entries) != aiGatewayMaxModels {
		t.Fatalf("%d entries, want the limit %d", len(entries), aiGatewayMaxModels)
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.ID)
	}
	// The owner's Copilot models all stay; the shared models fill the rest,
	// the Agent's own choice and the deployment default first, then by name.
	for _, want := range append([]string{"model-299", "model-298", "model-000", testSharedModelOne}, copilot...) {
		if !slices.Contains(ids, want) {
			t.Errorf("the configuration lacks %s", want)
		}
	}
	if def != "model-299" {
		t.Fatalf("the Agent's own choice stays its default, got %q", def)
	}
	if want := 301 + len(copilot) - aiGatewayMaxModels; len(omitted) != want {
		t.Fatalf("%d shared models left out, want %d", len(omitted), want)
	}
	for _, name := range omitted {
		if slices.Contains(ids, name) || name == "model-299" || name == "model-298" {
			t.Fatalf("%s is both listed and left out", name)
		}
	}
	if omitted[0] != "model-251" || omitted[len(omitted)-1] != "model-297" {
		t.Fatalf("the shared models past the limit in name order are left out, got %s..%s", omitted[0], omitted[len(omitted)-1])
	}
}

func TestAIGatewayMaxModelsMatchesTheAgentChart(t *testing.T) {
	raw, err := os.ReadFile("../../../cetirun-chart/values.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			AIGateway struct {
				Properties struct {
					Models struct {
						MaxItems int `json:"maxItems"`
					} `json:"models"`
				} `json:"properties"`
			} `json:"aiGateway"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	if got := schema.Properties.AIGateway.Properties.Models.MaxItems; got != aiGatewayMaxModels {
		t.Fatalf("the chart accepts %d models, the operator lists up to %d", got, aiGatewayMaxModels)
	}
}

// More shared models than the chart accepts must not block the Agent.
func legacyReconcileInstallsAnAgentWithMoreSharedModelsThanTheChartAccepts(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayTestTeam(testAIGatewayTeam))
	manySharedModels(gateway, 300)
	allowed := make([]string, 0, 300)
	for i := 0; i < 300; i++ {
		allowed = append(allowed, fmt.Sprintf("model-%03d", i))
	}
	allowTestPolicyModels(t, kube, allowed...)
	chart := &chartHelm{}
	r.HelmClient = chart
	recorder := record.NewFakeRecorder(100)
	r.EventRecorder = recorder
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: agent.Name}}
	for i := 0; i < 3 && chart.installs == 0; i++ {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	current := &cetirunv1alpha1.Agent{}
	if err := kube.Get(context.Background(), req.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	if chart.installs == 0 {
		t.Fatalf("the Agent was never installed: %+v", current.Status.Conditions)
	}
	if models := chart.lastValues["aiGateway"].(map[string]any)["models"].([]any); len(models) != aiGatewayMaxModels {
		t.Fatalf("%d models configured, want %d", len(models), aiGatewayMaxModels)
	}
	limited := false
	for len(recorder.Events) > 0 {
		if event := <-recorder.Events; strings.Contains(event, "AIGatewayModelsLimited") && strings.Contains(event, "45 shared models") {
			limited = true
		}
	}
	if !limited {
		t.Fatal("the Agent must report the shared models left out")
	}
}

func TestAIGatewayClientReadsMasterKeySecret(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, kube, _ := aiGatewayTestReconciler(t, agent)
	r.aiGatewayClientFactory = nil
	if _, err := r.aiGatewayClient(context.Background()); err == nil || !strings.Contains(err.Error(), "master key Secret") {
		t.Fatalf("missing master key Secret must be reported, got %v", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cetirun-ai-gateway-auth", Namespace: "cetirun-system"},
		Data:       map[string][]byte{"master-key": []byte(" ")},
	}
	if err := kube.Create(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	if _, err := r.aiGatewayClient(context.Background()); err == nil {
		t.Fatal("an empty master key must be rejected")
	}
	secret.Data["master-key"] = []byte("sk-synthetic-master")
	if err := kube.Update(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	if client, err := r.aiGatewayClient(context.Background()); err != nil || client == nil {
		t.Fatalf("client from a valid master key Secret: %v", err)
	}
}

func routedAgent() *cetirunv1alpha1.Agent {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	agent.Status.AIGateway = &cetirunv1alpha1.AIGatewayStatus{
		Phase: cetirunv1alpha1.AIGatewayPhaseReady, KeyID: "0123456789ab", KeySecretName: aiGatewayKeySecretName, Routed: true,
		Models: []string{testSharedModelOne}, DefaultModel: testSharedModelOne, AccessGroups: []string{testManagedGroup},
	}
	return agent
}

func TestApplyAIGatewayValuesOverridesProviderForRoutedAgent(t *testing.T) {
	r, _, gateway := aiGatewayTestReconciler(t)
	r.refreshAIGatewayCatalogWithin(context.Background(), gateway, 0)
	agent := routedAgent()
	agent.Status.AIGateway.Copilot = &cetirunv1alpha1.AIGatewayCopilotStatus{Phase: cetirunv1alpha1.AIGatewayCopilotPhaseReady, Models: []string{"copilot/gpt-5.5"}}
	values := map[string]any{
		"secrets":       map[string]any{"existingSecret": "gateway-token", "provider": "anthropic", "model": "user-choice"},
		"networkPolicy": map[string]any{"istioNamespace": "aks-istio-system"},
	}
	if err := r.applyAIGatewayValues(values, agent); err != nil {
		t.Fatal(err)
	}
	secrets := values["secrets"].(map[string]any)
	if secrets["provider"] != "openai" || secrets["apiBase"] != testAIGatewayURL+"/v1" || secrets["model"] != testSharedModelOne ||
		secrets["authMode"] != "api_key" || secrets["existingSecret"] != "gateway-token" {
		t.Fatalf("unexpected secrets values %v", secrets)
	}
	gatewayValues := values["aiGateway"].(map[string]any)
	if gatewayValues["enabled"] != true || gatewayValues["keySecretName"] != aiGatewayKeySecretName {
		t.Fatalf("unexpected aiGateway values %v", gatewayValues)
	}
	models := gatewayValues["models"].([]any)
	if len(models) != 2 || models[0].(map[string]any)["id"] != testSharedModelOne || models[1].(map[string]any)["id"] != "copilot/gpt-5.5" {
		t.Fatalf("unexpected models %v", models)
	}
	proxy := gatewayValues["copilotProxy"].(map[string]any)
	if proxy["enabled"] != true || proxy["image"] != testProxyImage || proxy["tokenSecretName"] != cetirunv1alpha1.CopilotTokenSecretName ||
		proxy["masterKeySecretName"] != aiGatewayProxySecretName {
		t.Fatalf("unexpected proxy values %v", proxy)
	}
	policy := values["networkPolicy"].(map[string]any)
	if policy["gatewayNamespace"] != "cetirun-system" || policy["gatewayPort"] != 4000 || policy["istioNamespace"] != "aks-istio-system" {
		t.Fatalf("unexpected networkPolicy values %v", policy)
	}
	if values["podAnnotations"].(map[string]any)[aiGatewayKeyIDPodAnnotation] != "0123456789ab" {
		t.Fatal("a key change must roll the Pod")
	}
}

func TestApplyAIGatewayValuesRejectsUnroutedAgent(t *testing.T) {
	for name, mutate := range map[string]func(*AgentReconciler, *cetirunv1alpha1.Agent){
		"not routed":       func(_ *AgentReconciler, a *cetirunv1alpha1.Agent) { a.Status.AIGateway.Routed = false },
		"feature disabled": func(r *AgentReconciler, _ *cetirunv1alpha1.Agent) { r.AIGateway.Enabled = false },
		"no key":           func(_ *AgentReconciler, a *cetirunv1alpha1.Agent) { a.Status.AIGateway.KeyID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			r := &AgentReconciler{AIGateway: aiGatewayTestConfig()}
			agent := routedAgent()
			mutate(r, agent)
			values := map[string]any{"secrets": map[string]any{"provider": "github_copilot", "model": "copilot-model"}}
			if err := r.applyAIGatewayValues(values, agent); err == nil {
				t.Fatal("unrouted Agent must not produce an applicable release")
			}
		})
	}
}

func TestAIGatewayRevision(t *testing.T) {
	disabled := &AgentReconciler{}
	if got := disabled.aiGatewayRevision(aiGatewayTestAgent(testAIGatewayTeam)); got != "" {
		t.Fatalf("a deployment without the feature must keep its hash, got %q", got)
	}
	r, _, gateway := aiGatewayTestReconciler(t)
	agent := routedAgent()
	routed := r.aiGatewayRevision(agent)
	agent.Status.AIGateway.KeyID = "ba9876543210"
	rotated := r.aiGatewayRevision(agent)
	if rotated == routed {
		t.Fatal("a new key must change the revision")
	}
	agent.Annotations = map[string]string{cetirunv1alpha1.AnnotationCopilotModels: `[{"id":"gpt-5.5"}]`}
	signedIn := r.aiGatewayRevision(agent)
	if signedIn == rotated {
		t.Fatal("a new Copilot model list must change the revision, so the next reconcile registers it")
	}
	// A Copilot model that joins a shared model leaves the Agent's model list
	// as it is; the revision must still change, since the proxy's
	// Responses-only models follow the routes.
	agent.Status.AIGateway.Copilot = &cetirunv1alpha1.AIGatewayCopilotStatus{Phase: cetirunv1alpha1.AIGatewayCopilotPhaseReady}
	if !r.aiGatewayCopilotActive(agent) {
		t.Fatal("the test Agent must have GitHub Copilot active")
	}
	beforeJoin := r.aiGatewayRevision(agent)
	agent.Status.AIGateway.Copilot.Routes = []cetirunv1alpha1.AIGatewayCopilotRoute{{Model: "gpt-5.5", Name: testSharedModelOne, Shared: true}}
	if r.aiGatewayRevision(agent) == beforeJoin {
		t.Fatal("a Copilot route joining a shared model must change the revision")
	}
	agent.Status.AIGateway.Copilot = nil
	r.refreshAIGatewayCatalogWithin(context.Background(), gateway, 0)
	withCatalog := r.aiGatewayRevision(agent)
	gateway.shared = append(gateway.shared, aigateway.Deployment{ID: "cfg-3", ModelName: "new-shared", AccessGroups: []string{aiGatewaySharedAccessGroup}})
	r.refreshAIGatewayCatalogWithin(context.Background(), gateway, 0)
	if r.aiGatewayRevision(agent) == withCatalog {
		t.Fatal("a new shared model must change the revision")
	}
	withNewModel := r.aiGatewayRevision(agent)
	gateway.shared[0].MaxInputTokens /= 2
	r.refreshAIGatewayCatalogWithin(context.Background(), gateway, 0)
	if r.aiGatewayRevision(agent) == withNewModel {
		t.Fatal("new limits for a shared model must change the revision, so they reach the Agent")
	}
	withNewLimits := r.aiGatewayRevision(agent)
	gateway.shared[0].MaxOutputTokens /= 2
	r.refreshAIGatewayCatalogWithin(context.Background(), gateway, 0)
	if r.aiGatewayRevision(agent) == withNewLimits {
		t.Fatal("a new output limit for a shared model must change the revision")
	}
	agent.Spec.ModelAccess = modelAccess(true, false)
	if r.aiGatewayRevision(agent) == withCatalog {
		t.Fatal("turning Copilot off must change the revision")
	}
}

func TestDeleteAIGatewayKeyRevokesKeyAndCopilotRoutes(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	value := mustKeyValue(t)
	agent.Status.AIGateway = &cetirunv1alpha1.AIGatewayStatus{
		Phase: cetirunv1alpha1.AIGatewayPhaseReady, KeyAlias: aiGatewayKeyAlias(agent),
		Copilot: &cetirunv1alpha1.AIGatewayCopilotStatus{Registered: []string{"recorded-route"}},
	}
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace(), keySecret(testAIGatewayUID, value))
	gateway.keys[aigateway.Token(value)] = &aigateway.Key{Alias: "renamed-by-an-administrator"}
	gateway.models["recorded-route"] = aigateway.ModelRequest{ID: "recorded-route", ModelName: "copilot/a", AccessGroups: []string{copilotGroup()}}
	gateway.models["unrecorded-route"] = aigateway.ModelRequest{ID: "unrecorded-route", ModelName: "copilot/b", AccessGroups: []string{copilotGroup()}}
	r.refreshAIGatewayCatalogWithin(context.Background(), gateway, 0)
	if err := r.deleteAIGatewayKey(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(gateway.deletedTokens, []string{aigateway.Token(value)}) || !slices.Equal(gateway.deletedAliases, []string{aiGatewayKeyAlias(agent)}) {
		t.Fatalf("revocation must use the key and the alias: tokens=%v aliases=%v", gateway.deletedTokens, gateway.deletedAliases)
	}
	if len(gateway.keys) != 0 || len(gateway.models) != 0 {
		t.Fatalf("the key or a Copilot route survived: keys=%d routes=%v", len(gateway.keys), gateway.modelNames())
	}
	persisted := &cetirunv1alpha1.Agent{}
	if err := kube.Get(context.Background(), types.NamespacedName{Name: agent.Name}, persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Status.AIGateway == nil || persisted.Status.AIGateway.Phase != cetirunv1alpha1.AIGatewayPhaseDeleting {
		t.Fatal("the deletion gate was not persisted before revocation")
	}
}

type aiGatewayStatusErrorClient struct {
	client.Client
	err error
}

func (c aiGatewayStatusErrorClient) Status() client.SubResourceWriter {
	return aiGatewayStatusErrorWriter{SubResourceWriter: c.Client.Status(), err: c.err}
}

type aiGatewayStatusErrorWriter struct {
	client.SubResourceWriter
	err error
}

type aiGatewayNthStatusErrorClient struct {
	client.Client
	err     error
	failAt  int
	patches int
}

func (c *aiGatewayNthStatusErrorClient) Status() client.SubResourceWriter {
	return aiGatewayNthStatusErrorWriter{SubResourceWriter: c.Client.Status(), parent: c}
}

type aiGatewayNthStatusErrorWriter struct {
	client.SubResourceWriter
	parent *aiGatewayNthStatusErrorClient
}

func (w aiGatewayNthStatusErrorWriter) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.SubResourcePatchOption) error {
	w.parent.patches++
	if w.parent.patches == w.parent.failAt {
		return w.parent.err
	}
	return w.SubResourceWriter.Patch(ctx, object, patch, options...)
}

func (w aiGatewayStatusErrorWriter) Patch(context.Context, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
	return w.err
}

func TestDeleteAIGatewayKeyRevokesWhenStatusWriteFails(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	value := mustKeyValue(t)
	agent.Status.AIGateway = &cetirunv1alpha1.AIGatewayStatus{Phase: cetirunv1alpha1.AIGatewayPhaseReady, KeyAlias: aiGatewayKeyAlias(agent)}
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace(), keySecret(testAIGatewayUID, value))
	gateway.keys[aigateway.Token(value)] = &aigateway.Key{Alias: aiGatewayKeyAlias(agent)}
	injected := errors.New("synthetic status failure")
	r.Client = aiGatewayStatusErrorClient{Client: kube, err: injected}

	err := r.deleteAIGatewayKey(context.Background(), agent)
	if !errors.Is(err, injected) {
		t.Fatalf("status failure was not returned after revocation: %v", err)
	}
	if !slices.Equal(gateway.deletedTokens, []string{aigateway.Token(value)}) ||
		!slices.Equal(gateway.deletedAliases, []string{aiGatewayKeyAlias(agent)}) || len(gateway.keys) != 0 {
		t.Fatalf("status failure skipped revocation: tokens=%v aliases=%v keys=%v", gateway.deletedTokens, gateway.deletedAliases, gateway.keys)
	}
}

func TestPolicyReprojectionRecoversWhenBlockReceiptWriteFails(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	value := mustKeyValue(t)
	agent.Status.AIGateway = &cetirunv1alpha1.AIGatewayStatus{
		Phase: cetirunv1alpha1.AIGatewayPhaseReady, KeyAlias: aiGatewayKeyAlias(agent),
		KeyID: value[:aiGatewayKeyIDLength], KeySecretName: aiGatewayKeySecretName, Routed: true,
	}
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace(), keySecret(testAIGatewayUID, value))
	token := aigateway.Token(value)
	budget := 10.0
	gateway.keys[token] = &aigateway.Key{Alias: aiGatewayKeyAlias(agent), Models: []string{testManagedGroup, copilotGroup()}, MaxBudget: &budget, BudgetDuration: aiGatewayBudgetDuration}
	injected := errors.New("synthetic block receipt status failure")
	failing := &aiGatewayNthStatusErrorClient{Client: kube, err: injected, failAt: 2}
	r.Client = failing

	err := r.beginAIGatewayPolicyReprojection(context.Background(), agent)
	if !errors.Is(err, injected) || !gateway.keys[token].Blocked {
		t.Fatalf("test did not reach a blocked key followed by receipt failure: blocked=%v err=%v", gateway.keys[token].Blocked, err)
	}
	persisted := &cetirunv1alpha1.Agent{}
	if err := kube.Get(context.Background(), types.NamespacedName{Name: agent.Name}, persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Status.AIGateway == nil || persisted.Status.AIGateway.Reason != aiGatewayReasonPolicyReprojection || persisted.Status.AIGateway.Blocked {
		t.Fatalf("durable intent was not recorded before the external block: %+v", persisted.Status.AIGateway)
	}

	if err := r.beginAIGatewayPolicyReprojection(context.Background(), persisted); err != nil {
		t.Fatalf("persisted intent could not recover the block receipt: %v", err)
	}
	if err := r.reconcileAIGateway(context.Background(), persisted, true); err != nil {
		t.Fatalf("current grant could not clear the recovered operator block: %v", err)
	}
	if gateway.keys[token].Blocked {
		t.Fatal("recovered policy-reprojection block remained permanent")
	}
}

func TestSuspensionBlockRecoversWhenReceiptWriteFails(t *testing.T) {
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
	agent.Spec.Suspended = true
	if err := kube.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(agent), agent); err != nil {
		t.Fatal(err)
	}

	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	if !gateway.keys[token].Blocked {
		t.Fatal("suspension did not block the key")
	}
	injected := errors.New("synthetic suspension receipt failure")
	r.Client = aiGatewayStatusErrorClient{Client: kube, err: injected}
	if err := r.updateStatus(ctx, agent); !errors.Is(err, injected) {
		t.Fatalf("receipt failure was not returned: %v", err)
	}
	persisted := &cetirunv1alpha1.Agent{}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(agent), persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Status.AIGateway == nil || persisted.Status.AIGateway.Reason != aiGatewayReasonSuspended || persisted.Status.AIGateway.Blocked {
		t.Fatalf("durable suspension intent was not recorded before blocking: %+v", persisted.Status.AIGateway)
	}

	persisted.Spec.Suspended = false
	if err := kube.Update(ctx, persisted); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(persisted), persisted); err != nil {
		t.Fatal(err)
	}
	r.Client = kube
	if err := r.reconcileAIGateway(ctx, persisted, true); err != nil {
		t.Fatalf("persisted suspension intent could not recover: %v", err)
	}
	if gateway.keys[token].Blocked {
		t.Fatal("recovered suspension block remained permanent")
	}
}

// A route registered just before the operator stopped is in neither the
// status nor, right after a restart, the operator's catalog. Deletion lists
// the Gateway itself, and holds the Agent while it cannot.
func TestDeleteAIGatewayKeyRemovesCopilotRoutesTheStatusMissed(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	agent.Status.AIGateway = &cetirunv1alpha1.AIGatewayStatus{Phase: cetirunv1alpha1.AIGatewayPhaseReady, KeyAlias: aiGatewayKeyAlias(agent)}
	r, _, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
	gateway.models["unrecorded-route"] = aigateway.ModelRequest{ID: "unrecorded-route", ModelName: "copilot/a", AccessGroups: []string{copilotGroup()}}
	gateway.models["other-agent-route"] = aigateway.ModelRequest{ID: "other-agent-route", ModelName: "copilot/a", AccessGroups: []string{"cetirun-copilot-another-agent"}}
	if _, known := r.aiGatewayCatalog.has("unrecorded-route"); known {
		t.Fatal("the test needs a catalog that was never listed")
	}
	if err := r.deleteAIGatewayKey(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(gateway.deletedModels, []string{"unrecorded-route"}) {
		t.Fatalf("deleted routes %v, want only this Agent's unrecorded route", gateway.deletedModels)
	}

	held := aiGatewayTestAgent(testAIGatewayTeam)
	held.Status.AIGateway = &cetirunv1alpha1.AIGatewayStatus{Phase: cetirunv1alpha1.AIGatewayPhaseReady}
	r2, _, _ := aiGatewayTestReconciler(t, held, aiGatewayNamespace())
	failing := &failingListGateway{fakeAIGateway: newFakeAIGateway()}
	r2.aiGatewayClientFactory = func(context.Context) (aiGatewayKeyClient, error) { return failing, nil }
	if err := r2.deleteAIGatewayKey(context.Background(), held); err == nil {
		t.Fatal("deletion must wait while the Gateway cannot be listed")
	}
}

func TestDeleteAIGatewayKeyBlocksOnFailureAndProceedsOnlyWithoutAGateway(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	agent.Status.AIGateway = &cetirunv1alpha1.AIGatewayStatus{Phase: cetirunv1alpha1.AIGatewayPhaseReady}
	r, _, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
	gateway.deleteErr = errors.New("synthetic outage")
	if err := r.deleteAIGatewayKey(context.Background(), agent); err == nil {
		t.Fatal("a failed revocation must block deletion")
	}

	// Nothing proves that an Agent without status never had a key, so with a
	// Gateway configured its alias is revoked, which is idempotent.
	unused := aiGatewayTestAgent(testAIGatewayTeam)
	r2, _, gateway2 := aiGatewayTestReconciler(t, unused)
	r2.AIGateway.Enabled = false
	if err := r2.deleteAIGatewayKey(context.Background(), unused); err != nil ||
		!slices.Equal(gateway2.deletedAliases, []string{aiGatewayKeyAlias(unused)}) {
		t.Fatalf("an Agent without status must still have its alias revoked: %v %v", err, gateway2.deletedAliases)
	}

	unconfigured := aiGatewayTestAgent(testAIGatewayTeam)
	unconfigured.Status.AIGateway = &cetirunv1alpha1.AIGatewayStatus{Phase: cetirunv1alpha1.AIGatewayPhaseReady}
	r3, _, _ := aiGatewayTestReconciler(t, unconfigured)
	r3.AIGateway = AIGatewayConfig{}
	if err := r3.deleteAIGatewayKey(context.Background(), unconfigured); err != nil {
		t.Fatalf("without any Gateway configuration deletion must proceed: %v", err)
	}
	select {
	case event := <-r3.EventRecorder.(*record.FakeRecorder).Events:
		if !strings.Contains(event, "AIGatewayKeyNotRevoked") {
			t.Fatalf("unexpected event %q", event)
		}
	default:
		t.Fatal("an unrevoked key must be reported")
	}
	// An Agent that never recorded a key has nothing to report.
	never := aiGatewayTestAgent(testAIGatewayTeam)
	r4, _, _ := aiGatewayTestReconciler(t, never)
	r4.AIGateway = AIGatewayConfig{}
	if err := r4.deleteAIGatewayKey(context.Background(), never); err != nil {
		t.Fatalf("without any Gateway configuration deletion must proceed: %v", err)
	}
	select {
	case event := <-r4.EventRecorder.(*record.FakeRecorder).Events:
		t.Fatalf("unexpected event %q", event)
	default:
	}
}

// The operator writes the key Secret and creates the Gateway key before it
// saves the Agent's status. A deletion that finds no status must still
// revoke that key while a Gateway is configured, even after the deployment
// turned the feature off: the namespace deletion removes the only record.
func TestDeleteAIGatewayKeyRevokesAKeyCreatedBeforeItsStatus(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	value := mustKeyValue(t)
	r, _, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace(), keySecret(testAIGatewayUID, value))
	r.AIGateway.Enabled = false
	gateway.keys[aigateway.Token(value)] = &aigateway.Key{Alias: aiGatewayKeyAlias(agent)}
	if err := r.deleteAIGatewayKey(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(gateway.deletedTokens, []string{aigateway.Token(value)}) || len(gateway.keys) != 0 {
		t.Fatalf("the key must be revoked: tokens=%v keys=%d", gateway.deletedTokens, len(gateway.keys))
	}

	// A Secret of another Agent under the same name is not this Agent's key:
	// only this Agent's own alias is revoked.
	other := aiGatewayTestAgent(testAIGatewayTeam)
	r2, _, gateway2 := aiGatewayTestReconciler(t, other, aiGatewayNamespace(), keySecret("uid-of-another-agent", mustKeyValue(t)))
	r2.AIGateway.Enabled = false
	if err := r2.deleteAIGatewayKey(context.Background(), other); err != nil || len(gateway2.deletedTokens) != 0 ||
		!slices.Equal(gateway2.deletedAliases, []string{aiGatewayKeyAlias(other)}) {
		t.Fatalf("another Agent's Secret must not be revoked: %v %v %v", err, gateway2.deletedAliases, gateway2.deletedTokens)
	}

	// An Agent that was never provisioned has no namespace: finalizing it
	// must not need Secret access, which a restricted controller lacks. Its
	// alias is still revoked, which needs no Secret.
	sparse := aiGatewayTestAgent(testAIGatewayTeam)
	r3, _, gateway3 := aiGatewayTestReconciler(t, sparse)
	r3.AIGateway.Enabled = false
	r3.Client = interceptor.NewClient(r3.Client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Secret); ok {
				return apierrors.NewForbidden(corev1.Resource("secrets"), key.Name, errors.New("no Secret access"))
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	if err := r3.deleteAIGatewayKey(context.Background(), sparse); err != nil ||
		!slices.Equal(gateway3.deletedAliases, []string{aiGatewayKeyAlias(sparse)}) {
		t.Fatalf("an Agent without a namespace must be finalized without reading Secrets: %v %v", err, gateway3.deletedAliases)
	}
}

// Neither the status nor the key Secret proves that the Gateway holds no key:
// the operator creates the key before it saves the status, and the Secret can
// be lost. The UID-derived alias is enough to revoke it, with key creation
// turned off too.
func TestDeleteAIGatewayKeyRevokesByAliasWithoutStatusOrSecret(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, _, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
	r.AIGateway.Enabled = false
	gateway.keys[aigateway.Token(mustKeyValue(t))] = &aigateway.Key{Alias: aiGatewayKeyAlias(agent)}
	if err := r.deleteAIGatewayKey(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	if len(gateway.keys) != 0 || !slices.Equal(gateway.deletedAliases, []string{aiGatewayKeyAlias(agent)}) {
		t.Fatalf("the key must be revoked by its alias: keys=%d aliases=%v", len(gateway.keys), gateway.deletedAliases)
	}
}

func TestParseAIGatewayConfig(t *testing.T) {
	env := func(values map[string]string) func(string) string {
		return func(key string) string { return values[key] }
	}
	cfg, err := ParseAIGatewayConfig(env(nil))
	if err != nil || cfg.Enabled || cfg.configured() || cfg.CopilotHealthChecks {
		t.Fatalf("empty configuration: %+v %v", cfg, err)
	}
	valid := map[string]string{
		"AI_GATEWAY_ENABLED": "true", "AI_GATEWAY_URL": testAIGatewayURL + "/", "AI_GATEWAY_MASTER_KEY_SECRET": "cetirun-ai-gateway-auth",
		"AI_GATEWAY_DEFAULT_BUDGET": "12.5", "AI_GATEWAY_DEFAULT_MODEL": "shared-model",
		"AI_GATEWAY_COPILOT_PROXY_IMAGE": "registry.example.com/cetirun-ai-gateway:0.1.1@sha256:" + strings.Repeat("a", 64),
	}
	cfg, err = ParseAIGatewayConfig(env(valid))
	if err != nil || !cfg.Enabled || cfg.URL != testAIGatewayURL || cfg.MasterKeySecretKey != "master-key" || cfg.DefaultBudget != 12.5 ||
		!strings.HasPrefix(cfg.CopilotProxyImage, "registry.example.com/") || cfg.CopilotHealthChecks {
		t.Fatalf("valid configuration: %+v %v", cfg, err)
	}
	// The Gateway probes the Agents' Copilot routes only when the setting is
	// true, in any letter case and with spaces around it.
	for value, want := range map[string]bool{"": false, "false": false, "yes": false, "true": true, " TRUE ": true} {
		values := maps.Clone(valid)
		values["AI_GATEWAY_COPILOT_HEALTH_CHECKS"] = value
		if cfg, err := ParseAIGatewayConfig(env(values)); err != nil || cfg.CopilotHealthChecks != want {
			t.Errorf("AI_GATEWAY_COPILOT_HEALTH_CHECKS=%q: probes of Copilot routes %v, want %v: %v", value, cfg.CopilotHealthChecks, want, err)
		}
	}
	if cfg, err := ParseAIGatewayConfig(env(map[string]string{"AI_GATEWAY_COPILOT_HEALTH_CHECKS": "true"})); err != nil || !cfg.CopilotHealthChecks {
		t.Errorf("the setting is read without a Gateway to call too: %+v %v", cfg, err)
	}
	for name, override := range map[string][2]string{
		"external URL":       {"AI_GATEWAY_URL", "http://gateway.example.com:4000"},
		"https URL":          {"AI_GATEWAY_URL", "https://cetirun-ai-gateway.cetirun-system.svc.cluster.local:4000"},
		"URL with path":      {"AI_GATEWAY_URL", testAIGatewayURL + "/v1"},
		"URL without port":   {"AI_GATEWAY_URL", "http://cetirun-ai-gateway.cetirun-system.svc.cluster.local"},
		"no Secret":          {"AI_GATEWAY_MASTER_KEY_SECRET", ""},
		"no budget":          {"AI_GATEWAY_DEFAULT_BUDGET", ""},
		"negative budget":    {"AI_GATEWAY_DEFAULT_BUDGET", "-1"},
		"untagged image":     {"AI_GATEWAY_COPILOT_PROXY_IMAGE", "registry.example.com/cetirun-ai-gateway"},
		"image with a space": {"AI_GATEWAY_COPILOT_PROXY_IMAGE", "registry.example.com/x:1 --privileged"},
		"bad digest":         {"AI_GATEWAY_COPILOT_PROXY_IMAGE", "registry.example.com/x@sha256:123"},
		// NaN passes every comparison, so a key created with it would get no
		// budget at all; an infinite budget is no cap either.
		"NaN budget":      {"AI_GATEWAY_DEFAULT_BUDGET", "NaN"},
		"infinite budget": {"AI_GATEWAY_DEFAULT_BUDGET", "+Inf"},
		"Infinity budget": {"AI_GATEWAY_DEFAULT_BUDGET", "Infinity"},
	} {
		values := map[string]string{}
		for key, value := range valid {
			values[key] = value
		}
		values[override[0]] = override[1]
		if _, err := ParseAIGatewayConfig(env(values)); err == nil {
			t.Errorf("%s: invalid configuration accepted", name)
		}
	}
	registryPort := map[string]string{}
	for key, value := range valid {
		registryPort[key] = value
	}
	registryPort["AI_GATEWAY_COPILOT_PROXY_IMAGE"] = "registry.example.com:5000/cetirun-ai-gateway:0.1.1"
	if _, err := ParseAIGatewayConfig(env(registryPort)); err != nil {
		t.Errorf("a registry with a port is valid: %v", err)
	}
}

func TestCopilotBaseModel(t *testing.T) {
	priced := map[string]bool{"gpt-5.5": true, "claude-sonnet-4-5": true, "github_copilot/gpt-9": true,
		"xai/grok-code-fast-1": true, "vertex_ai/grok-code-fast-1": true, "aihubmix/grok-code-fast-1": true,
		"openrouter/x-ai/grok-9": true, "aihubmix/grok-9": true, "gemini/gemini-9-flash": true}
	for id, want := range map[string]string{
		"gpt-5.5": "gpt-5.5", "claude-sonnet-4.5": "claude-sonnet-4-5", "gpt-9": "github_copilot/gpt-9",
		// The vendor's own entry, never a reseller's, whatever sorts first.
		"grok-code-fast-1": "xai/grok-code-fast-1", "gemini-9-flash": "gemini/gemini-9-flash",
		// Priced only by resellers: not a list price.
		"grok-9": "", "unknown-1": "",
	} {
		got, ok := copilotBaseModel(id, priced)
		if got != want || ok != (want != "") {
			t.Errorf("%s: got %q %v, want %q", id, got, ok, want)
		}
	}
}

func TestParseCopilotModels(t *testing.T) {
	models, err := parseCopilotModels(`[{"id":"b"},{"id":"a","contextWindow":-5},{"id":"a"},{"id":"../x"},{"id":""}]`)
	if err != nil || len(models) != 2 || models[0].ID != "a" || models[0].ContextWindow != 0 || models[1].ID != "b" {
		t.Fatalf("models %+v err %v", models, err)
	}
	if _, err := parseCopilotModels("not json"); err == nil {
		t.Fatal("invalid JSON must be reported")
	}
	var many []cetirunv1alpha1.CopilotModel
	for i := 0; i < aiGatewayMaxCopilotModels+10; i++ {
		many = append(many, cetirunv1alpha1.CopilotModel{ID: fmt.Sprintf("m%03d", i)})
	}
	raw, _ := json.Marshal(many)
	if models, _ := parseCopilotModels(string(raw)); len(models) != aiGatewayMaxCopilotModels {
		t.Fatalf("the list must be bounded, got %d", len(models))
	}
}

func TestAIGatewayCatalogPublishesSharedModels(t *testing.T) {
	r, kube, gateway := aiGatewayTestReconciler(t)
	gateway.shared = append(gateway.shared,
		aigateway.Deployment{ID: "cfg-3", ModelName: testSharedModelOne, AccessGroups: []string{aiGatewaySharedAccessGroup}, MaxInputTokens: 200000},
	)
	r.publishAIGatewayCatalog(context.Background(), 0)
	configMap := &corev1.ConfigMap{}
	if err := kube.Get(context.Background(), types.NamespacedName{Namespace: "cetirun-system", Name: cetirunv1alpha1.AIGatewayCatalogConfigMap}, configMap); err != nil {
		t.Fatal(err)
	}
	var catalog cetirunv1alpha1.AIGatewayCatalog
	if err := json.Unmarshal([]byte(configMap.Data[cetirunv1alpha1.AIGatewayCatalogKey]), &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.SharedModels) != 1 || catalog.SharedModels[0].Name != testSharedModelOne || catalog.SharedModels[0].ContextWindow != 200000 {
		t.Fatalf("the catalog lists each shared name once with the smallest limits: %+v", catalog)
	}
	resourceVersion := configMap.ResourceVersion
	r.publishAIGatewayCatalog(context.Background(), 0)
	if err := kube.Get(context.Background(), types.NamespacedName{Namespace: "cetirun-system", Name: cetirunv1alpha1.AIGatewayCatalogConfigMap}, configMap); err != nil {
		t.Fatal(err)
	}
	if configMap.ResourceVersion != resourceVersion {
		t.Fatal("an unchanged catalog must not be rewritten")
	}
}

func TestAIGatewayCatalogQueuesAgentsWhoseCopilotRoutesAreGone(t *testing.T) {
	copilotOnly := func(name, uid string) (*cetirunv1alpha1.Agent, string) {
		agent := routedAgent()
		agent.Name, agent.UID = name, types.UID(uid)
		shared, copilot := false, true
		agent.Spec.ModelAccess = &cetirunv1alpha1.ModelAccessSpec{SharedModels: &shared, GitHubCopilot: &copilot}
		route := copilotRegistrationID(agent, "gpt-5.5", "0123abcd")
		agent.Status.AIGateway.Models = []string{"copilot/gpt-5.5"}
		agent.Status.AIGateway.DefaultModel = "copilot/gpt-5.5"
		agent.Status.AIGateway.Copilot = &cetirunv1alpha1.AIGatewayCopilotStatus{
			Phase: cetirunv1alpha1.AIGatewayCopilotPhaseReady, AccessGroup: copilotAccessGroup(agent),
			Registered: []string{route}, Models: []string{"copilot/gpt-5.5"},
		}
		return agent, route
	}
	lost, lostRoute := copilotOnly("lost", "uid-lost")
	kept, keptRoute := copilotOnly("kept", "uid-kept")
	r, _, gateway := aiGatewayTestReconciler(t, lost, kept, routedAgent())
	gateway.models[keptRoute] = aigateway.ModelRequest{ID: keptRoute, ModelName: "copilot/gpt-5.5", AccessGroups: []string{copilotAccessGroup(kept)}}
	r.aiGatewayEvents = make(chan event.GenericEvent, 8)
	queued := func() []string {
		var names []string
		for {
			select {
			case queuedEvent := <-r.aiGatewayEvents:
				names = append(names, queuedEvent.Object.GetName())
			default:
				slices.Sort(names)
				return names
			}
		}
	}

	// The first listing is a catalog change, which queues the Agent with
	// shared models, and it shows that one route is gone.
	r.publishAIGatewayCatalog(context.Background(), 0)
	if got := queued(); !slices.Equal(got, []string{"alice", "lost"}) {
		t.Fatalf("first pass queued %v, want the shared-model Agent and the Agent that lost its route", got)
	}
	// The catalog is unchanged now: only the Agent without its route is
	// queued, again on every pass until the route is back.
	r.publishAIGatewayCatalog(context.Background(), 0)
	if got := queued(); !slices.Equal(got, []string{"lost"}) {
		t.Fatalf("second pass queued %v, want only the Agent that lost its route", got)
	}
	gateway.models[lostRoute] = aigateway.ModelRequest{ID: lostRoute, ModelName: "copilot/gpt-5.5", AccessGroups: []string{copilotAccessGroup(lost)}}
	r.publishAIGatewayCatalog(context.Background(), 0)
	if got := queued(); len(got) != 0 {
		t.Fatalf("queued %v once the route was back", got)
	}
}

// chartHelm renders the real agent chart so the test covers chart support
// detection and the rendered wiring, not just the values map.
type chartHelm struct {
	installs   int
	lastValues map[string]any
	uninstalls int
}

func (h *chartHelm) ResolveImageUpgrade(_, _, _, _, image, _ string) (string, error) {
	return image, nil
}

func (h *chartHelm) Prepare(_ string, values map[string]any, chartVersion string) (*helm.PreparedRelease, error) {
	chrt, err := loader.Load("../../../cetirun-chart")
	if err != nil {
		return nil, err
	}
	chrt.Metadata.Version = chartVersion
	return helm.PrepareChart(chrt, values)
}

func (h *chartHelm) Refresh(prepared *helm.PreparedRelease, values map[string]any) (*helm.PreparedRelease, error) {
	return prepared.Rebuild(values)
}

func (h *chartHelm) InstallOrUpgrade(_, _ string, prepared *helm.PreparedRelease) error {
	h.installs++
	h.lastValues = prepared.Values
	return nil
}

func (*chartHelm) CustomApplied(string, string, *helm.PreparedRelease) (bool, error) {
	return false, nil
}

func (h *chartHelm) Uninstall(string, string) error {
	h.uninstalls++
	return nil
}

// hasSignInHelperRoute reports whether the Agent's VirtualService routes to
// an in-agent sign-in helper.
func hasSignInHelperRoute(agent *cetirunv1alpha1.Agent) bool {
	for _, route := range buildVirtualService(agent, "example.com", "").Spec.Http {
		for _, match := range route.Match {
			if strings.Contains(match.GetUri().GetPrefix(), "cetirun-auth") {
				return true
			}
		}
	}
	return false
}

func TestSignInHelperRouteIsRemovedForEveryAgent(t *testing.T) {
	for _, kind := range []cetirunv1alpha1.AgentType{cetirunv1alpha1.AgentTypeOpenClaw, cetirunv1alpha1.AgentTypeHermes, cetirunv1alpha1.AgentTypeCustom} {
		agent := aiGatewayTestAgent(testAIGatewayTeam)
		agent.Spec.Agent.Type = kind
		if hasSignInHelperRoute(agent) {
			t.Fatalf("%s must not route in-Agent model login", kind)
		}
	}
}

func legacyReconcileRoutesAgentWithProxyAndRevokesOnDelete(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayTestTeam(testAIGatewayTeam))
	chart := &chartHelm{}
	r.HelmClient = chart
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: agent.Name}}
	for i := 0; i < 3 && chart.installs == 0; i++ {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	if chart.installs == 0 {
		t.Fatal("the Agent was never installed")
	}
	if len(gateway.generated) != 1 {
		t.Fatalf("generated %d keys, want 1", len(gateway.generated))
	}
	secrets := chart.lastValues["secrets"].(map[string]any)
	if secrets["provider"] != "openai" || secrets["apiBase"] != testAIGatewayURL+"/v1" || secrets["model"] != testSharedModelOne {
		t.Fatalf("installed values are not routed through the Gateway: %v", secrets)
	}
	gatewayValues := chart.lastValues["aiGateway"].(map[string]any)
	if gatewayValues["enabled"] != true || gatewayValues["copilotProxy"].(map[string]any)["enabled"] != true {
		t.Fatalf("chart wiring for the key and proxy is off: %v", gatewayValues)
	}
	runtimeValues := chart.lastValues["runtime"].(map[string]any)
	for _, port := range runtimeValues["ports"].([]any) {
		if port.(map[string]any)["name"] == "cetirun-auth" {
			t.Fatal("an Agent on the Gateway carries no in-agent sign-in helper port")
		}
	}
	config := runtimeValues["configFiles"].(map[string]any)["openclaw.json"].(string)
	if !strings.Contains(config, `"openai/`+testSharedModelOne+`"`) || !strings.Contains(config, `"contextWindow": 1000000`) {
		t.Fatalf("the native configuration lacks the Gateway models:\n%s", config)
	}
	current := &cetirunv1alpha1.Agent{}
	if err := kube.Get(context.Background(), req.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	if current.Status.AIGateway == nil || !current.Status.AIGateway.Routed || current.Status.AIGateway.Phase != cetirunv1alpha1.AIGatewayPhaseReady {
		t.Fatalf("persisted status %+v", current.Status.AIGateway)
	}
	vs := buildVirtualService(current, "example.com", "")
	for _, route := range vs.Spec.Http {
		for _, match := range route.Match {
			if prefix := match.GetUri().GetPrefix(); strings.Contains(prefix, "cetirun-auth") {
				t.Fatalf("an Agent on the Gateway must not route to an in-agent helper: %s", prefix)
			}
		}
	}

	if err := kube.Delete(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(gateway.keys) != 0 || !slices.Contains(gateway.deletedAliases, aiGatewayKeyAlias(agent)) {
		t.Fatalf("the key was not revoked on delete: keys=%d aliases=%v", len(gateway.keys), gateway.deletedAliases)
	}
	if chart.uninstalls != 1 {
		t.Fatal("the release was not uninstalled after revocation")
	}
	if err := kube.Get(context.Background(), req.NamespacedName, current); !apierrors.IsNotFound(err) {
		t.Fatalf("the Agent was not released after revocation: %v", err)
	}
}

func legacyReconcileDoesNotStartRoutedAgentWithoutKey(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, _, gateway := aiGatewayTestReconciler(t, agent, aiGatewayTestTeam(testAIGatewayTeam))
	gateway.getErr = errors.New("synthetic outage")
	chart := &chartHelm{}
	r.HelmClient = chart
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: agent.Name}}
	var result ctrl.Result
	for i := 0; i < 3; i++ {
		var err error
		if result, err = r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	if chart.installs != 0 || result.RequeueAfter != 30*time.Second {
		t.Fatalf("a routed Agent must wait for its key: installs=%d result=%+v", chart.installs, result)
	}
}

func TestAIGatewayVerificationTimeMovesOnlyWhenDue(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, _, _ := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	first := agent.Status.AIGateway.LastVerifiedTime
	if first == nil {
		t.Fatal("the first pass records the verification")
	}
	// A pass that was not due, such as one while the Agent is still
	// provisioning, must not change the status: a changed status re-queues
	// the Agent at once.
	earlier := metav1.NewTime(time.Now().Add(-time.Minute).Truncate(time.Second))
	agent.Status.AIGateway.LastVerifiedTime = &earlier
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	if !agent.Status.AIGateway.LastVerifiedTime.Equal(&earlier) {
		t.Fatalf("verification time moved on a pass that was not due: %v", agent.Status.AIGateway.LastVerifiedTime)
	}
	stale := metav1.NewTime(time.Now().Add(-2 * aiGatewayVerificationInterval))
	agent.Status.AIGateway.LastVerifiedTime = &stale
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	if !agent.Status.AIGateway.LastVerifiedTime.After(stale.Time) {
		t.Fatal("a due verification must record its time")
	}
}

// An Agent queued because the Gateway lost its Copilot routes must not take
// the lightweight pass, which skips the Gateway until the next verification.
func TestLostCopilotRouteMakesTheFullReconcileDue(t *testing.T) {
	agent := routedAgent()
	now := metav1.Now()
	route := copilotRegistrationID(agent, "gpt-5.5", "0123abcd")
	agent.Status.AIGateway.LastVerifiedTime = &now
	agent.Status.AIGateway.Copilot = &cetirunv1alpha1.AIGatewayCopilotStatus{
		Phase: cetirunv1alpha1.AIGatewayCopilotPhaseReady, AccessGroup: copilotAccessGroup(agent),
		Registered: []string{route}, Models: []string{"copilot/gpt-5.5"},
	}
	r, _, gateway := aiGatewayTestReconciler(t, agent)
	if r.aiGatewayReconcileDue(agent, time.Now()) {
		t.Fatal("a fresh verification is not due while nothing is known about the routes")
	}
	gateway.models[route] = aigateway.ModelRequest{ID: route, ModelName: "copilot/gpt-5.5", AccessGroups: []string{copilotAccessGroup(agent)}}
	r.refreshAIGatewayCatalogWithin(context.Background(), gateway, 0)
	// As recorded by the key reconcile that granted the current groups.
	agent.Status.AIGateway.PricingRevision, _ = r.aiGatewayCatalog.managedFingerprint()
	if r.aiGatewayReconcileDue(agent, time.Now()) {
		t.Fatal("an Agent whose routes the Gateway has waits for its verification")
	}
	delete(gateway.models, route)
	r.refreshAIGatewayCatalogWithin(context.Background(), gateway, 0)
	if !r.aiGatewayReconcileDue(agent, time.Now()) {
		t.Fatal("an Agent whose route the Gateway lost must get the full reconcile now")
	}
	r.AIGateway.Enabled = false
	if r.aiGatewayReconcileDue(agent, time.Now()) {
		t.Fatal("nothing is due with the Gateway off")
	}
}

// When the groups of the shared model names change, the key gets the new
// groups at the Agent's next reconcile, without waiting for its verification
// and without changing its configuration hash, so no Helm upgrade restarts
// it. That covers the one-time change from the per-configuration groups of
// earlier Gateway charts, and new model names. A configuration edit that
// keeps every name changes no group and makes nothing due.
func TestChangedModelBindingsUpdateTheKeyBeforeTheVerification(t *testing.T) {
	r, agent, gateway := legacyGatewayFixture(t)
	ctx := context.Background()
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	if len(gateway.generated) != 1 || r.aiGatewayReconcileDue(agent, time.Now()) {
		t.Fatalf("a freshly granted key is not due: generated=%d", len(gateway.generated))
	}
	token := aigateway.Token(gateway.generated[0].Key)
	verified := agent.Status.AIGateway.LastVerifiedTime
	revision := r.aiGatewayRevision(agent)

	// As granted by an operator that trusted the groups an earlier chart
	// derived from the complete model configuration.
	gateway.keys[token].Models = sortedCopy([]string{aiGatewaySharedAccessGroup, aiGatewayModelAccessGroupPrefix + "0123456789abcdef"})
	agent.Status.AIGateway.PricingRevision = "sha256:" + strings.Repeat("0", 64)
	agent.Status.AIGateway.AccessGroups = slices.Clone(gateway.keys[token].Models)
	if !r.aiGatewayReconcileDue(agent, time.Now()) {
		t.Fatal("a key granted under other model groups must get the full reconcile now")
	}
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	want := sortedCopy([]string{aiGatewaySharedAccessGroup, testManagedGroup})
	if !slices.Equal(gateway.keys[token].Models, want) || len(gateway.generated) != 1 ||
		!slices.Equal(agent.Status.AIGateway.AccessGroups, want) {
		t.Fatalf("the same key did not get the current groups: key=%v status=%v generated=%d",
			gateway.keys[token].Models, agent.Status.AIGateway.AccessGroups, len(gateway.generated))
	}
	if after := agent.Status.AIGateway.LastVerifiedTime; !after.Equal(verified) {
		t.Fatalf("updating the groups is not a verification, yet its time moved: %v -> %v", verified, after)
	}
	if r.aiGatewayRevision(agent) != revision {
		t.Fatal("updating the key's groups must not change the Agent's configuration hash")
	}
	if r.aiGatewayReconcileDue(agent, time.Now()) {
		t.Fatal("a key with the current groups is not due")
	}

	// An administrator edits the model: new endpoint or price, same name.
	updates := len(gateway.updates)
	gateway.shared[0].ID = managedTestDeploymentID(testSharedModelOne, 7)
	if r.refreshAIGatewayCatalogWithin(ctx, gateway, 0) || r.aiGatewayReconcileDue(agent, time.Now()) {
		t.Fatal("a configuration edit that keeps the model name must neither change the catalog nor make the reconcile due")
	}
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	if len(gateway.updates) != updates || !slices.Equal(gateway.keys[token].Models, want) {
		t.Fatalf("a configuration edit changed the key: %v", gateway.keys[token].Models)
	}

	// A new shared model name reaches the key before the verification too.
	added := aiGatewayModelAccessGroupPrefix + aiGatewayModelBinding("new-shared")
	gateway.shared = append(gateway.shared, aigateway.Deployment{
		ID: managedTestDeploymentID("new-shared", 1), ModelName: "new-shared",
		AccessGroups: []string{added}, DatabaseSourceKnown: true,
	})
	if !r.refreshAIGatewayCatalogWithin(ctx, gateway, 0) || !r.aiGatewayReconcileDue(agent, time.Now()) {
		t.Fatal("a new shared model name must change the catalog and make the reconcile due")
	}
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(gateway.keys[token].Models, added) || r.aiGatewayReconcileDue(agent, time.Now()) {
		t.Fatalf("the new model's group did not reach the key: %v", gateway.keys[token].Models)
	}

	// Agents without shared models do not depend on the shared groups.
	agent.Spec.ModelAccess = modelAccess(false, true)
	agent.Status.AIGateway.PricingRevision = ""
	if r.aiGatewayReconcileDue(agent, time.Now()) {
		t.Fatal("an Agent without shared models is not due for a shared-model change")
	}
}

// The whole path of a lost route: the catalog refresh shows it gone, and the
// Agent's next reconcile adds it back although its verification is not due.
func legacyReconcileRestoresLostCopilotRoutesBeforeTheVerification(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayTestTeam(testAIGatewayTeam))
	r.HelmClient = &chartHelm{}
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: agent.Name}}
	current := func() *cetirunv1alpha1.Agent {
		t.Helper()
		got := &cetirunv1alpha1.Agent{}
		if err := kube.Get(ctx, req.NamespacedName, got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	reconcile := func(times int) {
		t.Helper()
		for i := 0; i < times; i++ {
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
		}
	}
	reconcile(3)
	signedIn := current()
	signIn(t, kube, signedIn, testCopilotModels)
	if err := kube.Update(ctx, signedIn); err != nil {
		t.Fatal(err)
	}
	if err := kube.Create(ctx, readyProxy(1)); err != nil {
		t.Fatal(err)
	}
	reconcile(3)
	routes := len(gateway.models)
	if routes == 0 {
		t.Fatalf("no Copilot routes registered: %+v", current().Status.AIGateway.Copilot)
	}
	// Only a Ready Agent qualifies for the lightweight pass; the fake Helm
	// client starts no Pods, so mark it Ready as the workload would.
	ready := current()
	ready.Status.Phase = cetirunv1alpha1.PhaseReady
	if err := kube.Status().Update(ctx, ready); err != nil {
		t.Fatal(err)
	}
	reconcile(1)
	verified := current().Status.AIGateway.LastVerifiedTime

	// The Gateway loses the routes, and the catalog refresh sees it.
	gateway.models = map[string]aigateway.ModelRequest{}
	r.refreshAIGatewayCatalogWithin(ctx, gateway, 0)
	reconcile(1)
	if len(gateway.models) != routes {
		t.Fatalf("the reconcile restored %d of %d routes", len(gateway.models), routes)
	}
	if after := current().Status.AIGateway.LastVerifiedTime; !after.Equal(verified) {
		t.Fatalf("restoring the routes is not a verification, yet its time moved: %v -> %v", verified, after)
	}
}

// A lost proxy key Secret gets a new key, which the Gateway's routes carry at
// once. The running proxy read the old key when it started, so the Agent's
// chart must restart it.
func legacyReconcileRestartsTheCopilotProxyWithANewMasterKey(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayTestTeam(testAIGatewayTeam))
	chart := &chartHelm{}
	r.HelmClient = chart
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: agent.Name}}
	current := func() *cetirunv1alpha1.Agent {
		t.Helper()
		got := &cetirunv1alpha1.Agent{}
		if err := kube.Get(ctx, req.NamespacedName, got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	reconcile := func(times int) {
		t.Helper()
		for i := 0; i < times; i++ {
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
		}
	}
	proxyKeyID := func() any {
		t.Helper()
		return chart.lastValues["aiGateway"].(map[string]any)["copilotProxy"].(map[string]any)[aiGatewayChartProxyKeyValue]
	}
	reconcile(3)
	signedIn := current()
	signIn(t, kube, signedIn, testCopilotModels)
	if err := kube.Update(ctx, signedIn); err != nil {
		t.Fatal(err)
	}
	if err := kube.Create(ctx, readyProxy(1)); err != nil {
		t.Fatal(err)
	}
	reconcile(3)
	before := current().Status.AIGateway.Copilot.ProxyKeyID
	if before == "" || proxyKeyID() != before || len(gateway.models) == 0 {
		t.Fatalf("the chart must get the key ID once the routes exist: status %q values %v routes %d", before, proxyKeyID(), len(gateway.models))
	}
	installs := chart.installs

	// The proxy's key Secret is lost; the next verification finds it.
	if err := kube.Delete(ctx, readSecret(t, kube, aiGatewayProxySecretName)); err != nil {
		t.Fatal(err)
	}
	due := current()
	stale := metav1.NewTime(time.Now().Add(-2 * aiGatewayVerificationInterval))
	due.Status.AIGateway.LastVerifiedTime = &stale
	if err := kube.Status().Update(ctx, due); err != nil {
		t.Fatal(err)
	}
	reconcile(1)
	after := current().Status.AIGateway.Copilot.ProxyKeyID
	if after == "" || after == before {
		t.Fatalf("a new key must get a new ID: %q -> %q", before, after)
	}
	if chart.installs == installs || proxyKeyID() != after {
		t.Fatalf("the proxy must be restarted with the new key: installs %d -> %d, values carry %v", installs, chart.installs, proxyKeyID())
	}
	key := string(readSecret(t, kube, aiGatewayProxySecretName).Data[aiGatewayProxySecretField])
	if aigateway.Token(key)[:aiGatewayKeyIDLength] != after {
		t.Fatal("the ID must identify the key in the Secret")
	}
	for id, route := range gateway.models {
		if route.APIKey != key {
			t.Fatalf("route %s still carries the old key", id)
		}
	}
}

// Signing in again may switch to another GitHub account, with the same model
// list. The proxy keeps the Copilot token it exchanged until that token
// expires, without reading the new GitHub token, so the chart restarts it
// for every new sign-in the Agent takes the models of.
func legacyReconcileRestartsTheCopilotProxyForANewSignIn(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayTestTeam(testAIGatewayTeam))
	chart := &chartHelm{}
	r.HelmClient = chart
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: agent.Name}}
	reconcile := func(times int) {
		t.Helper()
		for i := 0; i < times; i++ {
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
		}
	}
	signInAs := func(generation, at string) {
		t.Helper()
		secret := readSecret(t, kube, cetirunv1alpha1.CopilotTokenSecretName)
		secret.Data = map[string][]byte{cetirunv1alpha1.CopilotTokenSecretKey: []byte("gho_" + generation)}
		secret.Annotations = map[string]string{
			cetirunv1alpha1.AnnotationCopilotSignInGeneration: generation,
			cetirunv1alpha1.AnnotationCopilotSignInCompleted:  generation,
			cetirunv1alpha1.AnnotationCopilotTokenStoredAt:    time.Now().UTC().Format(time.RFC3339),
		}
		if err := kube.Update(ctx, secret); err != nil {
			t.Fatal(err)
		}
		current := &cetirunv1alpha1.Agent{}
		if err := kube.Get(ctx, req.NamespacedName, current); err != nil {
			t.Fatal(err)
		}
		if current.Annotations == nil {
			current.Annotations = map[string]string{}
		}
		current.Annotations[cetirunv1alpha1.AnnotationCopilotModels] = testCopilotModels
		current.Annotations[cetirunv1alpha1.AnnotationCopilotSignInGeneration] = generation
		current.Annotations[cetirunv1alpha1.AnnotationCopilotSignedInAt] = at
		if err := kube.Update(ctx, current); err != nil {
			t.Fatal(err)
		}
	}
	signInValue := func() any {
		t.Helper()
		return chart.lastValues["aiGateway"].(map[string]any)["copilotProxy"].(map[string]any)[aiGatewayChartSignInValue]
	}
	reconcile(3)
	if err := kube.Create(ctx, readyProxy(1)); err != nil {
		t.Fatal(err)
	}
	signInAs("0a1b2c", "2026-10-07T08:00:00Z")
	reconcile(3)
	routes := gateway.modelNames()
	if len(routes) == 0 || signInValue() != "0a1b2c" {
		t.Fatalf("the chart must get the sign-in's generation: values %v, routes %v", signInValue(), routes)
	}
	installs := chart.installs

	// Another account, with the same models: the routes stay, the proxy is
	// restarted.
	signInAs("3d4e5f", "2026-10-07T08:30:00Z")
	reconcile(2)
	if chart.installs == installs || signInValue() != "3d4e5f" {
		t.Fatalf("the proxy must be restarted for the new sign-in: installs %d -> %d, values carry %v", installs, chart.installs, signInValue())
	}
	if !slices.Equal(gateway.modelNames(), routes) {
		t.Fatalf("the same models keep their routes: %v -> %v", routes, gateway.modelNames())
	}
}

func TestApplyCopilotSignInGenerationOnlyForTheProxy(t *testing.T) {
	for name, tc := range map[string]struct {
		generation string
		enabled    bool
		want       any
	}{
		"a sign-in":          {generation: "0123abcd", enabled: true, want: "0123abcd"},
		"before generations": {generation: "", enabled: true, want: nil},
		"not a generation":   {generation: "not hex", enabled: true, want: nil},
		"no proxy":           {generation: "0123abcd", enabled: false, want: nil},
	} {
		t.Run(name, func(t *testing.T) {
			agent := routedAgent()
			agent.Annotations = map[string]string{cetirunv1alpha1.AnnotationCopilotSignInGeneration: tc.generation}
			proxy := map[string]any{"enabled": tc.enabled}
			applyCopilotSignInGeneration(map[string]any{"aiGateway": map[string]any{"copilotProxy": proxy}}, agent)
			if proxy[aiGatewayChartSignInValue] != tc.want {
				t.Fatalf("signInGeneration %v, want %v", proxy[aiGatewayChartSignInValue], tc.want)
			}
		})
	}
}

// New limits for a shared model reach the Agent's configuration although the
// model keeps its name.
func legacyReconcileRollsTheAgentWhenASharedModelsLimitsChange(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayTestTeam(testAIGatewayTeam))
	chart := &chartHelm{}
	r.HelmClient = chart
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: agent.Name}}
	reconcile := func(times int) {
		t.Helper()
		for i := 0; i < times; i++ {
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
		}
	}
	contextWindow := func() int64 {
		t.Helper()
		for _, item := range chart.lastValues["aiGateway"].(map[string]any)["models"].([]any) {
			if model := item.(map[string]any); model["id"] == testSharedModelOne {
				// Helm's coalescing may turn the number into a float.
				switch value := model["contextWindow"].(type) {
				case int64:
					return value
				case float64:
					return int64(value)
				}
				t.Fatalf("context window %v (%T)", model["contextWindow"], model["contextWindow"])
			}
		}
		t.Fatal("the shared model is not configured")
		return 0
	}
	reconcile(3)
	// Only a Ready Agent qualifies for the lightweight pass; the fake Helm
	// client starts no Pods, so mark it Ready as the workload would.
	ready := &cetirunv1alpha1.Agent{}
	if err := kube.Get(ctx, req.NamespacedName, ready); err != nil {
		t.Fatal(err)
	}
	ready.Status.Phase = cetirunv1alpha1.PhaseReady
	if err := kube.Status().Update(ctx, ready); err != nil {
		t.Fatal(err)
	}
	reconcile(1)
	if got := contextWindow(); got != 1000000 {
		t.Fatalf("context window %d, want the catalog's", got)
	}
	installs := chart.installs

	// An administrator lowers the model's context window in the Gateway.
	gateway.shared[0].MaxInputTokens = 400000
	r.refreshAIGatewayCatalogWithin(ctx, gateway, 0)
	reconcile(1)
	if chart.installs == installs || contextWindow() != 400000 {
		t.Fatalf("the new limit must reach the Agent: installs %d -> %d, context window %d", installs, chart.installs, contextWindow())
	}
}

func TestAIGatewayCopilotPendingKeepsReconciling(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	now := metav1.Now()
	agent.Status.AIGateway = &cetirunv1alpha1.AIGatewayStatus{Phase: cetirunv1alpha1.AIGatewayPhaseReady, LastVerifiedTime: &now}
	if aiGatewayVerificationDue(agent, true, time.Now()) {
		t.Fatal("a fresh key verification is not due")
	}
	for phase, want := range map[string]bool{
		cetirunv1alpha1.AIGatewayCopilotPhaseSignInRequired: false,
		cetirunv1alpha1.AIGatewayCopilotPhaseStarting:       true,
		cetirunv1alpha1.AIGatewayCopilotPhaseDegraded:       true,
		cetirunv1alpha1.AIGatewayCopilotPhaseReady:          false,
	} {
		agent.Status.AIGateway.Copilot = &cetirunv1alpha1.AIGatewayCopilotStatus{Phase: phase}
		if got := aiGatewayCopilotPending(agent, true); got != want {
			t.Errorf("phase %s: pending=%v, want %v", phase, got, want)
		}
		if aiGatewayCopilotPending(agent, false) {
			t.Errorf("phase %s: nothing is pending with the Gateway off", phase)
		}
	}
}

// Changing only the deployment's default model must reach the Agents whose
// default it decides, rather than wait for the next key verification, and
// must leave Agents with their own usable model alone.
func TestAIGatewayRevisionFollowsTheDeploymentDefaultModel(t *testing.T) {
	r, _, _ := aiGatewayTestReconciler(t)
	revisionWith := func(agent *cetirunv1alpha1.Agent, defaultModel string) string {
		r.AIGateway.DefaultModel = defaultModel
		return r.aiGatewayRevision(agent)
	}
	unnamed := routedAgent()
	if revisionWith(unnamed, "model-a") == revisionWith(unnamed, "model-b") {
		t.Fatal("an Agent that names no model must follow the deployment default")
	}
	unavailable := routedAgent()
	unavailable.Spec.Model = "no-longer-offered"
	if revisionWith(unavailable, "model-a") == revisionWith(unavailable, "model-b") {
		t.Fatal("an Agent whose own model is unavailable must follow the deployment default")
	}
	chosen := routedAgent()
	chosen.Spec.Model = testSharedModelOne
	if revisionWith(chosen, "model-a") != revisionWith(chosen, "model-b") {
		t.Fatal("an Agent with its own usable model must not be reconciled for the deployment default")
	}
}

func TestAIGatewayRevisionFollowsEverySignIn(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, _, _ := aiGatewayTestReconciler(t, agent)
	agent.Annotations = map[string]string{cetirunv1alpha1.AnnotationCopilotModels: testCopilotModels}
	before := r.aiGatewayRevision(agent)
	agent.Annotations[cetirunv1alpha1.AnnotationCopilotSignedInAt] = "2026-10-01T08:00:00Z"
	first := r.aiGatewayRevision(agent)
	agent.Annotations[cetirunv1alpha1.AnnotationCopilotSignedInAt] = "2026-10-01T08:05:00Z"
	second := r.aiGatewayRevision(agent)
	if before == first || first == second {
		t.Fatal("a sign-in with an unchanged model list must still change the revision")
	}
}

// A Copilot model list is used only with the token of its sign-in: the
// Portal records the same generation with both. Models of another sign-in are
// awaited while a sign-in completes, then no longer used, and the routes go,
// so the proxy is never asked for models its token's account may not have.
func TestReconcileAIGatewayCopilotUsesModelsOnlyWithTheirSignIn(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace(), readyProxy(1))
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	// Each token is stored completed here; the next test covers sign-ins
	// that are not.
	storeToken := func(generation string, storedAt time.Time) {
		t.Helper()
		secret := readSecret(t, kube, cetirunv1alpha1.CopilotTokenSecretName)
		secret.Data = map[string][]byte{cetirunv1alpha1.CopilotTokenSecretKey: []byte("gho_" + generation)}
		secret.Annotations = map[string]string{
			cetirunv1alpha1.AnnotationCopilotSignInGeneration: generation,
			cetirunv1alpha1.AnnotationCopilotSignInCompleted:  generation,
			cetirunv1alpha1.AnnotationCopilotTokenStoredAt:    storedAt.UTC().Format(time.RFC3339),
		}
		if err := kube.Update(context.Background(), secret); err != nil {
			t.Fatal(err)
		}
	}
	recordModels := func(generation, models string) {
		// Preserve immutable ownership evidence while simulating the
		// Copilot-proxy-owned discovery annotations.
		agent.Annotations[cetirunv1alpha1.AnnotationCopilotModels] = models
		agent.Annotations[cetirunv1alpha1.AnnotationCopilotSignInGeneration] = generation
	}
	reconcile := func() *cetirunv1alpha1.AIGatewayCopilotStatus {
		t.Helper()
		if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
			t.Fatal(err)
		}
		return agent.Status.AIGateway.Copilot
	}

	storeToken("first", time.Now())
	recordModels("first", `[{"id":"gpt-5.5"}]`)
	if copilot := reconcile(); copilot.Phase != cetirunv1alpha1.AIGatewayCopilotPhaseReady || !slices.Equal(gateway.modelNames(), []string{"gpt-5.5"}) {
		t.Fatalf("a consistent sign-in registers its models: %+v %v", copilot, gateway.modelNames())
	}

	// A newer sign-in stored its token; its models follow. The routes stay
	// as they are meanwhile.
	storeToken("second", time.Now())
	if copilot := reconcile(); copilot.Phase != cetirunv1alpha1.AIGatewayCopilotPhaseStarting || copilot.Reason != "SignInCompleting" ||
		!slices.Equal(gateway.modelNames(), []string{"gpt-5.5"}) {
		t.Fatalf("models of the previous sign-in are awaited, not dropped yet: %+v %v", copilot, gateway.modelNames())
	}

	// They never came: the models on the Agent belong to another sign-in
	// than the stored token, and are not used.
	storeToken("second", time.Now().Add(-copilotSignInGrace-time.Minute))
	if copilot := reconcile(); copilot.Phase != cetirunv1alpha1.AIGatewayCopilotPhaseDegraded || copilot.Reason != "SignInIncomplete" ||
		len(gateway.models) != 0 || len(copilot.Models) != 0 {
		t.Fatalf("models of another sign-in must not be used: %+v %v", copilot, gateway.modelNames())
	}

	// The second sign-in's models arrive.
	recordModels("second", `[{"id":"claude-sonnet-4.5"}]`)
	if copilot := reconcile(); copilot.Phase != cetirunv1alpha1.AIGatewayCopilotPhaseReady ||
		!slices.Equal(gateway.modelNames(), []string{"claude-sonnet-4.5"}) {
		t.Fatalf("the models of the stored sign-in are registered: %+v %v", copilot, gateway.modelNames())
	}
}

// The models of a sign-in are used only once the Portal completed it on the
// token Secret, under the fence that a cancel, a newer start or the end of an
// expired completion raises. A completion fenced after its last check can
// still write the token and the models with the same generation, as when its
// replica stalled there; it never completes, and its models are never used.
func TestReconcileAIGatewayCopilotUsesOnlyCompletedSignIns(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace(), readyProxy(1))
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	storeToken := func(completed string, storedAt time.Time) {
		t.Helper()
		secret := readSecret(t, kube, cetirunv1alpha1.CopilotTokenSecretName)
		secret.Data = map[string][]byte{cetirunv1alpha1.CopilotTokenSecretKey: []byte("gho_fenced")}
		secret.Annotations = map[string]string{
			cetirunv1alpha1.AnnotationCopilotSignInGeneration: "fenced",
			cetirunv1alpha1.AnnotationCopilotTokenStoredAt:    storedAt.UTC().Format(time.RFC3339),
		}
		if completed != "" {
			secret.Annotations[cetirunv1alpha1.AnnotationCopilotSignInCompleted] = completed
		}
		if err := kube.Update(context.Background(), secret); err != nil {
			t.Fatal(err)
		}
	}
	agent.Annotations[cetirunv1alpha1.AnnotationCopilotModels] = `[{"id":"gpt-5.5"}]`
	agent.Annotations[cetirunv1alpha1.AnnotationCopilotSignInGeneration] = "fenced"
	reconcile := func() *cetirunv1alpha1.AIGatewayCopilotStatus {
		t.Helper()
		if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
			t.Fatal(err)
		}
		return agent.Status.AIGateway.Copilot
	}

	// The token and the models of one sign-in, not completed yet: awaited.
	storeToken("", time.Now())
	if copilot := reconcile(); copilot.Phase != cetirunv1alpha1.AIGatewayCopilotPhaseStarting || copilot.Reason != "SignInCompleting" ||
		len(gateway.models) != 0 {
		t.Fatalf("a sign-in not completed must not be used: %+v %v", copilot, gateway.modelNames())
	}
	// It never completes, or an earlier sign-in's completion is all the
	// Secret records: its models are not used.
	for _, completed := range []string{"", "earlier"} {
		storeToken(completed, time.Now().Add(-copilotSignInGrace-time.Minute))
		if copilot := reconcile(); copilot.Phase != cetirunv1alpha1.AIGatewayCopilotPhaseDegraded || copilot.Reason != "SignInIncomplete" ||
			len(gateway.models) != 0 || len(copilot.Models) != 0 {
			t.Fatalf("completed %q: the models of a sign-in that never completed must not be used: %+v %v", completed, copilot, gateway.modelNames())
		}
	}
	// Completed, it is used.
	storeToken("fenced", time.Now().Add(-copilotSignInGrace-time.Minute))
	if copilot := reconcile(); copilot.Phase != cetirunv1alpha1.AIGatewayCopilotPhaseReady ||
		!slices.Equal(gateway.modelNames(), []string{"gpt-5.5"}) {
		t.Fatalf("a completed sign-in's models are registered: %+v %v", copilot, gateway.modelNames())
	}

	// A token stored before generations, with models recorded without one,
	// is used as before: the Portal now stores every token with one.
	legacy := readSecret(t, kube, cetirunv1alpha1.CopilotTokenSecretName)
	legacy.Annotations = nil
	if err := kube.Update(context.Background(), legacy); err != nil {
		t.Fatal(err)
	}
	delete(agent.Annotations, cetirunv1alpha1.AnnotationCopilotSignInGeneration)
	agent.Annotations[cetirunv1alpha1.AnnotationCopilotModels] = `[{"id":"claude-sonnet-4.5"}]`
	if copilot := reconcile(); copilot.Phase != cetirunv1alpha1.AIGatewayCopilotPhaseReady ||
		!slices.Equal(gateway.modelNames(), []string{"claude-sonnet-4.5"}) {
		t.Fatalf("a sign-in from before generations keeps its models: %+v %v", copilot, gateway.modelNames())
	}
}

func TestCopilotTokenIsReadPastTheCache(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, kube, _ := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
	if signIn, err := r.ensureCopilotTokenSecret(context.Background(), agent); err != nil || signIn.signedIn {
		t.Fatalf("first pass creates an empty Secret: signIn=%+v err=%v", signIn, err)
	}
	// The cache still shows the empty Secret; the API server has the token.
	fresh := readSecret(t, kube, cetirunv1alpha1.CopilotTokenSecretName).DeepCopy()
	fresh.Data = map[string][]byte{cetirunv1alpha1.CopilotTokenSecretKey: []byte("gho_synthetic")}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	r.APIReader = fake.NewClientBuilder().WithScheme(scheme).WithObjects(fresh).Build()
	if signIn, err := r.ensureCopilotTokenSecret(context.Background(), agent); err != nil || !signIn.signedIn {
		t.Fatalf("the token written by the Portal must be seen: signIn=%+v err=%v", signIn, err)
	}
}

func TestAIGatewayCatalogLeavesOutUnusableSharedNames(t *testing.T) {
	var catalog aiGatewayCatalog
	atLimit := strings.Repeat("m", aiGatewayModelNameLimit)
	// The chart counts characters, not bytes.
	multiByteAtLimit := strings.Repeat("é", aiGatewayModelNameLimit)
	changed, skipped, _ := catalog.update([]aigateway.Deployment{
		{ID: "a", ModelName: "gpt-6", AccessGroups: []string{aiGatewaySharedAccessGroup}},
		{ID: "b", ModelName: "openai/*", AccessGroups: []string{aiGatewaySharedAccessGroup}},
		{ID: "c", ModelName: "copilot/gpt-5.5", AccessGroups: []string{aiGatewaySharedAccessGroup}},
		{ID: "d", ModelName: "has space", AccessGroups: []string{aiGatewaySharedAccessGroup}},
		{ID: "e", ModelName: atLimit, AccessGroups: []string{aiGatewaySharedAccessGroup}},
		{ID: "f", ModelName: multiByteAtLimit, AccessGroups: []string{aiGatewaySharedAccessGroup}},
		{ID: "g", ModelName: atLimit + "m", AccessGroups: []string{aiGatewaySharedAccessGroup}},
	}, false, time.Now())
	shared, _ := catalog.shared()
	if !changed || len(shared) != 3 || shared[0].Name != "gpt-6" || shared[1].Name != atLimit ||
		shared[2].Name != multiByteAtLimit || len(skipped) != 4 || !slices.Contains(skipped, atLimit+"m") {
		t.Fatalf("shared=%d %v skipped=%d changed=%v", len(shared), shared, len(skipped), changed)
	}
}

func TestAIGatewayCatalogBindsModelNamesAndIgnoresConfigurationEdits(t *testing.T) {
	// The Gateway chart's group for "gpt": SHA-256("gpt") begins 053ea4804ef1bb33.
	const group = "cetirun-model-053ea4804ef1bb33"
	var catalog aiGatewayCatalog
	first := []aigateway.Deployment{
		{ID: managedTestDeploymentID("gpt", 1), ModelName: "gpt", AccessGroups: []string{aiGatewaySharedAccessGroup, group}, DatabaseSourceKnown: true},
		{ID: managedTestDeploymentID("gpt", 2), ModelName: "gpt", AccessGroups: []string{group}, DatabaseSourceKnown: true},
	}
	if changed, _, _ := catalog.update(first, false, time.Now()); !changed {
		t.Fatal("first catalog listing must change")
	}
	groups, ok := catalog.resolvedGroups([]string{"gpt"})
	if !ok || !slices.Equal(groups, []string{group}) {
		t.Fatalf("load-balanced entries of one name must share one trusted group: groups=%v known=%v", groups, ok)
	}
	firstFingerprint, ok := catalog.managedFingerprint()
	if !ok || firstFingerprint == "" {
		t.Fatal("live managed catalog must have a fingerprint")
	}
	// An edit to a configured entry (endpoint, provider parameters, price)
	// changes its deployment ID but neither its name nor its group. Agents
	// keep the model: neither the catalog nor its fingerprint changes.
	edited := slices.Clone(first)
	edited[0].ID = managedTestDeploymentID("gpt", 3)
	if changed, _, _ := catalog.update(edited, false, time.Now()); changed {
		t.Fatal("a configuration edit that keeps the model name must not change the managed catalog")
	}
	if fingerprint, _ := catalog.managedFingerprint(); fingerprint != firstFingerprint {
		t.Fatal("a configuration edit that keeps the model name must not change the managed fingerprint")
	}
	if groups, _ := catalog.resolvedGroups([]string{"gpt"}); !slices.Equal(groups, []string{group}) {
		t.Fatalf("edited groups=%v", groups)
	}
	// Adding or removing a model name still changes the fingerprint.
	added := append(slices.Clone(edited), aigateway.Deployment{
		ID: managedTestDeploymentID("other", 1), ModelName: "other",
		AccessGroups: []string{"cetirun-model-d9298a10d1b07358"}, DatabaseSourceKnown: true,
	})
	if changed, _, _ := catalog.update(added, false, time.Now()); !changed {
		t.Fatal("a new model name must change the managed catalog")
	}
	if fingerprint, _ := catalog.managedFingerprint(); fingerprint == firstFingerprint {
		t.Fatal("a new model name must change the managed fingerprint")
	}
	if changed, _, _ := catalog.update(edited, false, time.Now()); !changed {
		t.Fatal("a removed model name must change the managed catalog")
	}
	if fingerprint, _ := catalog.managedFingerprint(); fingerprint != firstFingerprint {
		t.Fatal("removing the added name must restore the managed fingerprint")
	}
}

func TestAIGatewayCatalogRejectsForgedOrCollidingManagedBindings(t *testing.T) {
	trusted := aigateway.Deployment{
		ID: testManagedDeploymentID, ModelName: testSharedModelOne,
		AccessGroups: []string{aiGatewaySharedAccessGroup, testManagedGroup}, DatabaseSourceKnown: true,
	}
	balanced := trusted
	balanced.ID = managedTestDeploymentID(testSharedModelOne, 2)
	for name, deployments := range map[string][]aigateway.Deployment{
		"database model copies trusted group": {
			trusted,
			{ID: "db-copy", ModelName: "other", AccessGroups: []string{testManagedGroup}, FromDatabase: true, DatabaseSourceKnown: true},
		},
		"database copy of the configured model": {
			trusted, balanced,
			{ID: managedTestDeploymentID(testSharedModelOne, 3), ModelName: testSharedModelOne, AccessGroups: []string{testManagedGroup}, FromDatabase: true, DatabaseSourceKnown: true},
		},
		"static model of another name reuses trusted group": {
			trusted,
			{ID: managedTestDeploymentID(testSharedModelOne, 2), ModelName: "other", AccessGroups: []string{testManagedGroup}, DatabaseSourceKnown: true},
		},
		"database model forges reserved tuple": {
			{ID: testManagedDeploymentID, ModelName: testSharedModelOne, AccessGroups: []string{testManagedGroup}, FromDatabase: true, DatabaseSourceKnown: true},
		},
		"unknown source forges reserved tuple": {
			{ID: testManagedDeploymentID, ModelName: testSharedModelOne, AccessGroups: []string{testManagedGroup}},
		},
		"deployment ID does not match group": {
			{ID: "different", ModelName: testSharedModelOne, AccessGroups: []string{testManagedGroup}, DatabaseSourceKnown: true},
		},
		"deployment ID of another binding": {
			{ID: managedTestDeploymentID("other", 1), ModelName: testSharedModelOne, AccessGroups: []string{testManagedGroup}, DatabaseSourceKnown: true},
		},
		"deployment ID without a configuration hash": {
			{ID: aiGatewaySharedDeploymentIDPrefix + "7388db16debdff68", ModelName: testSharedModelOne, AccessGroups: []string{testManagedGroup}, DatabaseSourceKnown: true},
		},
		"deployment ID with an uppercase configuration hash": {
			{ID: aiGatewaySharedDeploymentIDPrefix + "7388db16debdff68-0123456789ABCDEF", ModelName: testSharedModelOne, AccessGroups: []string{testManagedGroup}, DatabaseSourceKnown: true},
		},
		"group not derived from the model name": {
			{ID: aiGatewaySharedDeploymentIDPrefix + "0123456789abcdef-0123456789abcdef", ModelName: testSharedModelOne, AccessGroups: []string{aiGatewayModelAccessGroupPrefix + "0123456789abcdef"}, DatabaseSourceKnown: true},
		},
		"binding from a configuration hash, as earlier charts rendered it": {
			{ID: aiGatewaySharedDeploymentIDPrefix + "0123456789abcdef", ModelName: testSharedModelOne, AccessGroups: []string{aiGatewayModelAccessGroupPrefix + "0123456789abcdef"}, DatabaseSourceKnown: true},
		},
		"group with uppercase hexadecimal": {
			{ID: aiGatewaySharedDeploymentIDPrefix + "7388DB16DEBDFF68-0123456789abcdef", ModelName: testSharedModelOne, AccessGroups: []string{aiGatewayModelAccessGroupPrefix + "7388DB16DEBDFF68"}, DatabaseSourceKnown: true},
		},
		"deployment ID is duplicated": {
			trusted,
			{ID: testManagedDeploymentID, ModelName: "other", AccessGroups: []string{"cetirun-admin"}, DatabaseSourceKnown: true},
		},
		"load-balanced entries share a deployment ID": {
			trusted, trusted,
		},
	} {
		t.Run(name, func(t *testing.T) {
			var catalog aiGatewayCatalog
			catalog.update(deployments, false, time.Now())
			if groups, complete := catalog.resolvedGroups([]string{testSharedModelOne}); complete || len(groups) != 0 {
				t.Fatalf("untrusted binding resolved to %v, complete=%v", groups, complete)
			}
		})
	}
}

// gatewayChartDeployments renders the AI Gateway chart with operatorIntegration
// and lists its model_list the way LiteLLM reports configured models.
func gatewayChartDeployments(t *testing.T, models []any) []aigateway.Deployment {
	t.Helper()
	chrt, err := loader.Load(filepath.Join("..", "..", "..", "cetirun-ai-gateway", "chart"))
	if err != nil {
		t.Fatal(err)
	}
	values, err := chartutil.ToRenderValues(chrt, map[string]any{"operatorIntegration": true, "models": models},
		chartutil.ReleaseOptions{Name: "gateway", Namespace: "cetirun-system", IsInstall: true}, chartutil.DefaultCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := engine.Render(chrt, values)
	if err != nil {
		t.Fatal(err)
	}
	configMap := &corev1.ConfigMap{}
	if err := sigsyaml.Unmarshal([]byte(rendered["cetirun-ai-gateway/templates/configmap.yaml"]), configMap); err != nil {
		t.Fatal(err)
	}
	var config struct {
		ModelList []struct {
			ModelName string `json:"model_name"`
			ModelInfo struct {
				ID           string   `json:"id"`
				AccessGroups []string `json:"access_groups"`
			} `json:"model_info"`
		} `json:"model_list"`
	}
	if err := sigsyaml.Unmarshal([]byte(configMap.Data["config.yaml"]), &config); err != nil {
		t.Fatal(err)
	}
	deployments := make([]aigateway.Deployment, 0, len(config.ModelList))
	for _, model := range config.ModelList {
		deployments = append(deployments, aigateway.Deployment{
			ID: model.ModelInfo.ID, ModelName: model.ModelName, AccessGroups: model.ModelInfo.AccessGroups,
			DatabaseSourceKnown: true, Priced: true,
		})
	}
	return deployments
}

// The Gateway chart and the operator must derive the same binding: the
// operator trusts the groups the chart renders, for load-balanced entries of
// one name and for names beyond ASCII, and an edit to an entry keeps them.
func TestAIGatewayCatalogTrustsTheGatewayChartsBindings(t *testing.T) {
	entry := func(name, model, apiBase string, price float64) map[string]any {
		return map[string]any{
			"model_name":     name,
			"litellm_params": map[string]any{"model": model, "api_base": apiBase},
			"model_info":     map[string]any{"input_cost_per_token": price, "access_groups": []any{aiGatewaySharedAccessGroup}},
		}
	}
	const other = "modèle-example"
	models := []any{
		entry(testSharedModelOne, "azure/first", "https://first.example.com/", 0.001),
		entry(testSharedModelOne, "azure/second", "https://second.example.com/", 0.001),
		entry(other, "openai/example", "https://models.example.com/v1", 0.002),
	}
	rendered := gatewayChartDeployments(t, models)
	if len(rendered) != 3 || rendered[0].ID == rendered[1].ID ||
		!slices.Equal(rendered[0].AccessGroups, []string{aiGatewaySharedAccessGroup, testManagedGroup}) ||
		!slices.Equal(rendered[1].AccessGroups, rendered[0].AccessGroups) {
		t.Fatalf("unexpected chart bindings: %+v", rendered)
	}
	var catalog aiGatewayCatalog
	catalog.update(rendered, true, time.Now())
	otherGroup := aiGatewayModelAccessGroupPrefix + aiGatewayModelBinding(other)
	groups, complete := catalog.resolvedGroups([]string{testSharedModelOne, other})
	if !complete || !slices.Equal(groups, sortedCopy([]string{testManagedGroup, otherGroup})) {
		t.Fatalf("the chart's bindings are not trusted: groups=%v complete=%v", groups, complete)
	}
	fingerprint, _ := catalog.managedFingerprint()

	edited := slices.Clone(models)
	edited[0] = entry(testSharedModelOne, "azure/replacement", "https://replacement.example.com/", 0.003)
	editedDeployments := gatewayChartDeployments(t, edited)
	if editedDeployments[0].ID == rendered[0].ID {
		t.Fatal("an edited entry must get a new deployment ID")
	}
	if changed, _, _ := catalog.update(editedDeployments, true, time.Now()); changed {
		t.Fatal("an edit to a chart entry changed the managed catalog")
	}
	if after, _ := catalog.managedFingerprint(); after != fingerprint {
		t.Fatal("an edit to a chart entry changed the managed fingerprint")
	}
	if groups, complete := catalog.resolvedGroups([]string{testSharedModelOne, other}); !complete ||
		!slices.Equal(groups, sortedCopy([]string{testManagedGroup, otherGroup})) {
		t.Fatalf("an edit to a chart entry changed its trusted groups: %v", groups)
	}
}

// The configured entries of one name are load-balanced deployments: each of
// them is a member of the name's group, and the group stays trusted.
func TestAIGatewayCatalogTrustsEveryConfiguredEntryOfOneName(t *testing.T) {
	balanced := aigateway.Deployment{
		ID: managedTestDeploymentID(testSharedModelOne, 2), ModelName: testSharedModelOne,
		AccessGroups: []string{testManagedGroup}, DatabaseSourceKnown: true, MaxInputTokens: 500000,
	}
	var catalog aiGatewayCatalog
	catalog.update(append(newFakeAIGateway().shared, balanced), false, time.Now())
	groups, complete := catalog.resolvedGroups([]string{testSharedModelOne})
	if !complete || !slices.Equal(groups, []string{testManagedGroup}) {
		t.Fatalf("groups=%v complete=%v", groups, complete)
	}
	shared, _ := catalog.shared()
	if len(shared) != 1 || shared[0].Name != testSharedModelOne || shared[0].ContextWindow != 500000 {
		t.Fatalf("the name must be listed once with the smallest limits of its entries: %+v", shared)
	}
	if got := aiGatewayModelBinding(testSharedModelOne); aiGatewayModelAccessGroupPrefix+got != testManagedGroup {
		t.Fatalf("binding %q does not match the Gateway chart's group %q", got, testManagedGroup)
	}
}

func TestAIGatewayCatalogFingerprintIsIndependentOfListingOrder(t *testing.T) {
	deployments := []aigateway.Deployment{
		{ID: managedTestDeploymentID("same", 1), ModelName: "same", AccessGroups: []string{"cetirun-model-0967115f2813a354"}, DatabaseSourceKnown: true},
		{ID: managedTestDeploymentID("same", 2), ModelName: "same", AccessGroups: []string{"cetirun-model-0967115f2813a354"}, DatabaseSourceKnown: true},
		{ID: managedTestDeploymentID("other", 1), ModelName: "other", AccessGroups: []string{"cetirun-model-d9298a10d1b07358"}, DatabaseSourceKnown: true},
		{ID: "duplicate", ModelName: "untrusted", AccessGroups: []string{"cetirun-model-a"}, DatabaseSourceKnown: true},
		{ID: "duplicate", ModelName: "untrusted", AccessGroups: []string{"cetirun-model-b"}, FromDatabase: true, DatabaseSourceKnown: true},
	}
	var first, reversed aiGatewayCatalog
	first.update(deployments, false, time.Now())
	slices.Reverse(deployments)
	reversed.update(deployments, false, time.Now())
	firstFingerprint, _ := first.managedFingerprint()
	reversedFingerprint, _ := reversed.managedFingerprint()
	if firstFingerprint != reversedFingerprint {
		t.Fatalf("catalog fingerprint depends on listing order: %s != %s", firstFingerprint, reversedFingerprint)
	}
	if groups, complete := first.resolvedGroups([]string{"same", "other"}); !complete || len(groups) != 2 {
		t.Fatalf("groups=%v complete=%v", groups, complete)
	}
}

func catalogNames(models []aiGatewayCatalogModel) []string {
	names := make([]string, 0, len(models))
	for _, model := range models {
		names = append(names, model.Name)
	}
	return names
}

// The Gateway refuses every request for an unpriced model while it blocks
// them, so the catalog does not offer such a model then, and offers it
// otherwise. Like LiteLLM, a name counts as priced when any deployment
// behind it is.
func TestAIGatewayCatalogLeavesOutUnpricedSharedModelsWhileTheGatewayBlocksThem(t *testing.T) {
	deployments := []aigateway.Deployment{
		{ID: "a", ModelName: "priced", AccessGroups: []string{aiGatewaySharedAccessGroup}, Priced: true},
		{ID: "b", ModelName: "unpriced", AccessGroups: []string{aiGatewaySharedAccessGroup}},
		{ID: "c", ModelName: "unpriced", AccessGroups: []string{aiGatewaySharedAccessGroup}},
		{ID: "d", ModelName: "mixed", AccessGroups: []string{aiGatewaySharedAccessGroup}},
		{ID: "e", ModelName: "mixed", AccessGroups: []string{"cetirun-admin"}, Priced: true},
	}
	var blocking aiGatewayCatalog
	_, _, unpriced := blocking.update(deployments, true, time.Now())
	shared, _ := blocking.shared()
	if names := catalogNames(shared); !slices.Equal(names, []string{"mixed", "priced"}) || !slices.Equal(unpriced, []string{"unpriced"}) {
		t.Fatalf("while the Gateway blocks unpriced models: shared %v, left out %v", names, unpriced)
	}
	var allowing aiGatewayCatalog
	_, _, unpriced = allowing.update(deployments, false, time.Now())
	shared, _ = allowing.shared()
	if names := catalogNames(shared); !slices.Equal(names, []string{"mixed", "priced", "unpriced"}) || len(unpriced) != 0 {
		t.Fatalf("while the Gateway serves unpriced models: shared %v, left out %v", names, unpriced)
	}
}

// The console accepts the names the Gateway chart reserves, in any letter
// case; the operator leaves them out. Other provider prefixes are not reserved.
func TestAIGatewayCatalogLeavesOutReservedSharedNames(t *testing.T) {
	var catalog aiGatewayCatalog
	_, skipped, _ := catalog.update([]aigateway.Deployment{
		{ID: "a", ModelName: "cetirun-console-model", AccessGroups: []string{aiGatewaySharedAccessGroup}},
		{ID: "b", ModelName: "CetiRun-Console-Model", AccessGroups: []string{aiGatewaySharedAccessGroup}},
		{ID: "c", ModelName: "Copilot/gpt-5.5", AccessGroups: []string{aiGatewaySharedAccessGroup}},
		{ID: "d", ModelName: "azure/gpt-4o", AccessGroups: []string{aiGatewaySharedAccessGroup}},
		{ID: "e", ModelName: "my-cetirun-model", AccessGroups: []string{aiGatewaySharedAccessGroup}},
	}, false, time.Now())
	shared, _ := catalog.shared()
	if names := catalogNames(shared); !slices.Equal(names, []string{"azure/gpt-4o", "my-cetirun-model"}) || len(skipped) != 3 {
		t.Fatalf("shared=%v skipped=%v", names, skipped)
	}
}

// An administrator can change the setting in the Gateway's console at any
// time, so every listing reads it again.
func TestAIGatewayCatalogFollowsTheGatewaysUnpricedSetting(t *testing.T) {
	r, _, gateway := aiGatewayTestReconciler(t)
	gateway.shared[0].Priced = true
	gateway.shared = append(gateway.shared, aigateway.Deployment{ID: "cfg-3", ModelName: "unpriced", AccessGroups: []string{aiGatewaySharedAccessGroup}})
	ctx := context.Background()
	names := func() []string {
		shared, _ := r.aiGatewayCatalog.shared()
		return catalogNames(shared)
	}
	pricedOnly, both := []string{testSharedModelOne}, []string{testSharedModelOne, "unpriced"}

	// Before the setting was ever read, the Gateway's default holds.
	gateway.blockErr = errors.New("not reported")
	r.refreshAIGatewayCatalogWithin(ctx, gateway, 0)
	if got := names(); !slices.Equal(got, pricedOnly) {
		t.Fatalf("before the first read: %v, want %v", got, pricedOnly)
	}
	gateway.blockErr, gateway.blockUnpriced = nil, false
	if !r.refreshAIGatewayCatalogWithin(ctx, gateway, 0) || !slices.Equal(names(), both) {
		t.Fatalf("once the Gateway serves unpriced models the catalog must change to %v, got %v", both, names())
	}
	gateway.blockErr = errors.New("unavailable")
	if r.refreshAIGatewayCatalogWithin(ctx, gateway, 0) || !slices.Equal(names(), both) {
		t.Fatalf("a failed read keeps the last setting: %v", names())
	}
	gateway.blockErr, gateway.blockUnpriced = nil, true
	if !r.refreshAIGatewayCatalogWithin(ctx, gateway, 0) || !slices.Equal(names(), pricedOnly) {
		t.Fatalf("once the Gateway blocks unpriced models again the catalog must change to %v, got %v", pricedOnly, names())
	}
}

func TestAIGatewayCatalogPublishesWhetherGitHubCopilotIsOffered(t *testing.T) {
	for _, image := range []string{testProxyImage, ""} {
		r, kube, _ := aiGatewayTestReconciler(t)
		r.AIGateway.CopilotProxyImage = image
		r.publishAIGatewayCatalog(context.Background(), 0)
		configMap := &corev1.ConfigMap{}
		if err := kube.Get(context.Background(), types.NamespacedName{Namespace: "cetirun-system", Name: cetirunv1alpha1.AIGatewayCatalogConfigMap}, configMap); err != nil {
			t.Fatal(err)
		}
		var catalog cetirunv1alpha1.AIGatewayCatalog
		if err := json.Unmarshal([]byte(configMap.Data[cetirunv1alpha1.AIGatewayCatalogKey]), &catalog); err != nil {
			t.Fatal(err)
		}
		if catalog.GitHubCopilot != (image != "") || len(catalog.SharedModels) != 1 {
			t.Fatalf("proxy image %q: published %+v", image, catalog)
		}
	}
}

// inspectingHelm reads the default chart's values for the catalog; the
// reconciler's other Helm operations are not used.
type inspectingHelm struct {
	HelmInstaller
	values   *helm.ChartValues
	err      error
	versions []string
}

func (f *inspectingHelm) ChartIdentity(version string) string {
	return "oci://registry.example.com/helm/cetirun:" + version
}

func (f *inspectingHelm) InspectChart(_ context.Context, version string) (*helm.ChartValues, error) {
	f.versions = append(f.versions, version)
	return f.values, f.err
}

// stallingHelm reads charts from a registry that never answers: a read
// returns only when its context is done.
type stallingHelm struct{ inspectingHelm }

func (f *stallingHelm) InspectChart(ctx context.Context, _ string) (*helm.ChartValues, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// A chart read that stalls ends with the publisher's context, so the catalog
// is still published, with the agent types unknown, instead of the publisher
// waiting forever and the Portal refusing every native Agent for want of a
// catalog.
func TestAIGatewayCatalogDoesNotWaitForAStalledChartRead(t *testing.T) {
	r, kube, _ := aiGatewayTestReconciler(t)
	r.HelmClient = &stallingHelm{}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		r.publishAIGatewayCatalog(ctx, 0)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the publisher must not wait for a stalled chart read")
	}
	if got := publishedRouting(t, kube); got == nil || got.AgentTypes != nil || len(publishedCatalogModels(t, kube)) != 1 {
		t.Fatalf("the catalog must be published with unknown agent types, got routing %+v", got)
	}
}

func publishedCatalogModels(t *testing.T, kube client.Client) []cetirunv1alpha1.AIGatewayCatalogModel {
	t.Helper()
	configMap := &corev1.ConfigMap{}
	if err := kube.Get(context.Background(), types.NamespacedName{Namespace: "cetirun-system", Name: cetirunv1alpha1.AIGatewayCatalogConfigMap}, configMap); err != nil {
		t.Fatal(err)
	}
	var catalog cetirunv1alpha1.AIGatewayCatalog
	if err := json.Unmarshal([]byte(configMap.Data[cetirunv1alpha1.AIGatewayCatalogKey]), &catalog); err != nil {
		t.Fatal(err)
	}
	return catalog.SharedModels
}

func publishedRouting(t *testing.T, kube client.Client) *cetirunv1alpha1.AIGatewayRouting {
	t.Helper()
	configMap := &corev1.ConfigMap{}
	if err := kube.Get(context.Background(), types.NamespacedName{Namespace: "cetirun-system", Name: cetirunv1alpha1.AIGatewayCatalogConfigMap}, configMap); err != nil {
		t.Fatal(err)
	}
	var catalog cetirunv1alpha1.AIGatewayCatalog
	if err := json.Unmarshal([]byte(configMap.Data[cetirunv1alpha1.AIGatewayCatalogKey]), &catalog); err != nil {
		t.Fatal(err)
	}
	return catalog.Routing
}

func catalogWithRouting(routing string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: cetirunv1alpha1.AIGatewayCatalogConfigMap, Namespace: "cetirun-system"},
		Data: map[string]string{cetirunv1alpha1.AIGatewayCatalogKey: `{"sharedModels":[],"githubCopilot":true,"routing":` +
			routing + `}`},
	}
}

// The catalog lists the agent types the default agent chart routes through
// the Gateway, with that chart's identity, so the Portal offers the Gateway's
// models to those only: a chart without aiGateway.hermes.keyEnv routes
// OpenClaw alone, one older than the Gateway none. The chart is read once.
func TestAIGatewayCatalogPublishesTheAgentTypesTheDefaultChartRoutes(t *testing.T) {
	for name, tc := range map[string]struct {
		strip []string
		want  []cetirunv1alpha1.AgentType
	}{
		"current chart":   {want: []cetirunv1alpha1.AgentType{cetirunv1alpha1.AgentTypeOpenClaw, cetirunv1alpha1.AgentTypeHermes, cetirunv1alpha1.AgentTypeCustom}},
		"OpenClaw only":   {strip: []string{"aiGateway", "hermes"}, want: []cetirunv1alpha1.AgentType{cetirunv1alpha1.AgentTypeOpenClaw, cetirunv1alpha1.AgentTypeCustom}},
		"before Gateway":  {strip: []string{"aiGateway"}, want: []cetirunv1alpha1.AgentType{cetirunv1alpha1.AgentTypeCustom}},
		"without a proxy": {strip: []string{"aiGateway", "copilotProxy"}, want: []cetirunv1alpha1.AgentType{cetirunv1alpha1.AgentTypeCustom}},
	} {
		t.Run(name, func(t *testing.T) {
			chrt, err := loader.Load("../../../cetirun-chart")
			if err != nil {
				t.Fatal(err)
			}
			if len(tc.strip) == 1 {
				delete(chrt.Values, tc.strip[0])
			} else if len(tc.strip) == 2 {
				delete(chrt.Values[tc.strip[0]].(map[string]any), tc.strip[1])
			}
			r, kube, _ := aiGatewayTestReconciler(t)
			inspector := &inspectingHelm{values: helm.NewChartValues(chrt)}
			r.HelmClient = inspector
			r.publishAIGatewayCatalog(context.Background(), 0)
			r.publishAIGatewayCatalog(context.Background(), 0)
			got := publishedRouting(t, kube)
			if got == nil || got.Chart != inspector.ChartIdentity(r.ChartVersion) || got.AgentTypes == nil || !slices.Equal(got.AgentTypes, tc.want) {
				t.Fatalf("published routing %+v, want %v for %s", got, tc.want, inspector.ChartIdentity(r.ChartVersion))
			}
			if !slices.Equal(inspector.versions, []string{r.ChartVersion}) {
				t.Fatalf("the default chart must be read once, read %v", inspector.versions)
			}
		})
	}
}

// While the default chart cannot be read, the catalog keeps the agent types
// it published for that chart. Those of another chart, published before a
// restart with a new default, are not reused: the agent types are unknown
// (null) until the chart can be read, and so are they when nothing was
// published.
func TestAIGatewayCatalogKeepsItsAgentTypesWhileTheChartCannotBeRead(t *testing.T) {
	unreadable := &inspectingHelm{err: errors.New("registry unavailable")}
	identity := unreadable.ChartIdentity("0.1.28")
	both := []cetirunv1alpha1.AgentType{cetirunv1alpha1.AgentTypeOpenClaw, cetirunv1alpha1.AgentTypeHermes, cetirunv1alpha1.AgentTypeCustom}

	r, kube, _ := aiGatewayTestReconciler(t, catalogWithRouting(`{"chart":"`+identity+`","agentTypes":["openclaw","hermes","custom"]}`))
	r.HelmClient = unreadable
	r.publishAIGatewayCatalog(context.Background(), 0)
	if got := publishedRouting(t, kube); got == nil || got.Chart != identity || !slices.Equal(got.AgentTypes, both) {
		t.Fatalf("the agent types published for this chart must stay, got %+v", got)
	}

	r, kube, _ = aiGatewayTestReconciler(t, catalogWithRouting(`{"chart":"`+unreadable.ChartIdentity("0.1.27")+`","agentTypes":["openclaw","hermes"]}`))
	r.HelmClient = unreadable
	r.publishAIGatewayCatalog(context.Background(), 0)
	if got := publishedRouting(t, kube); got == nil || got.Chart != identity || got.AgentTypes != nil {
		t.Fatalf("another chart's agent types must not be reused, got %+v", got)
	}

	r, kube, _ = aiGatewayTestReconciler(t)
	r.HelmClient = unreadable
	r.publishAIGatewayCatalog(context.Background(), 0)
	if got := publishedRouting(t, kube); got == nil || got.Chart != identity || got.AgentTypes != nil {
		t.Fatalf("no agent types are known yet, got %+v", got)
	}
	// Once the chart can be read, its agent types are published.
	chrt, err := loader.Load("../../../cetirun-chart")
	if err != nil {
		t.Fatal(err)
	}
	r.HelmClient = &inspectingHelm{values: helm.NewChartValues(chrt)}
	r.publishAIGatewayCatalog(context.Background(), 0)
	if got := publishedRouting(t, kube); got == nil || !slices.Equal(got.AgentTypes, both) {
		t.Fatalf("the chart's agent types must be published once it can be read, got %+v", got)
	}
}

// A Copilot model is named like every other model, by its Gateway name, never
// by GitHub's display name, which the operator does not control and which
// would tell the Agent which backend serves the model.
func TestAIGatewayModelEntriesNameCopilotModelsLikeEveryOtherModel(t *testing.T) {
	r, _, gateway := aiGatewayTestReconciler(t)
	r.refreshAIGatewayCatalogWithin(context.Background(), gateway, 0)
	agent := routedAgent()
	raw, err := json.Marshal([]cetirunv1alpha1.CopilotModel{{ID: "gpt-5.5", Name: strings.Repeat("n", 300) + " (GitHub Copilot)"}})
	if err != nil {
		t.Fatal(err)
	}
	agent.Annotations = map[string]string{cetirunv1alpha1.AnnotationCopilotModels: string(raw)}
	agent.Status.AIGateway.Copilot = &cetirunv1alpha1.AIGatewayCopilotStatus{Models: []string{"copilot/gpt-5.5"}}
	entries, _, _ := r.aiGatewayModelEntries(agent)
	found := false
	for _, entry := range entries {
		if utf8.RuneCountInString(entry.ID) > aiGatewayModelNameLimit || utf8.RuneCountInString(entry.Name) > aiGatewayModelNameLimit {
			t.Fatalf("entry %q exceeds the chart's limit: name has %d characters", entry.ID, utf8.RuneCountInString(entry.Name))
		}
		if strings.Contains(entry.Name, "Copilot") || strings.Contains(entry.Name, "nnn") {
			t.Fatalf("entry %q reveals its backend: %q", entry.ID, entry.Name)
		}
		if entry.ID == "copilot/gpt-5.5" {
			found = entry.Name == entry.ID
		}
	}
	if !found {
		t.Fatalf("the Copilot model must stay listed under its own name: %+v", entries)
	}
}

func TestAIGatewayCatalogSeedsFromThePublishedCatalogDuringAnOutage(t *testing.T) {
	published := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: cetirunv1alpha1.AIGatewayCatalogConfigMap, Namespace: "cetirun-system"},
		Data: map[string]string{cetirunv1alpha1.AIGatewayCatalogKey: `{"sharedModels":[{"name":"` + testSharedModelOne +
			`","contextWindow":1000000,"maxOutputTokens":32768}]}`},
	}
	r, _, _ := aiGatewayTestReconciler(t, published)
	failing := &failingListGateway{fakeAIGateway: newFakeAIGateway()}
	if r.refreshAIGatewayCatalogWithin(context.Background(), failing, 0) {
		t.Fatal("a failed listing reports no change")
	}
	shared, known := r.aiGatewayCatalog.shared()
	if !known || len(shared) != 1 || shared[0].ContextWindow != 1000000 {
		t.Fatalf("the published catalog stands in, with its limits: %+v known=%v", shared, known)
	}
	if _, known := r.aiGatewayCatalog.has("cfg-1"); known {
		t.Fatal("a seeded catalog does not claim to know the Gateway's deployments")
	}
	// A live listing establishes the concrete managed deployment fingerprint.
	// Even with the same display models, Policy must re-evaluate grants
	// rather than treating a seeded catalog as enforcement evidence.
	if !r.refreshAIGatewayCatalogWithin(context.Background(), failing.fakeAIGateway, 0) {
		t.Fatal("recovery from a seeded catalog must re-evaluate Policy grants")
	}
}

type failingListGateway struct{ *fakeAIGateway }

func (f *failingListGateway) ListModels(context.Context) ([]aigateway.Deployment, error) {
	return nil, errors.New("gateway unavailable")
}

// Turning key creation off removes no key the Gateway holds: while a Gateway
// is configured, a failed revocation holds the deletion either way. Removing
// the Gateway configuration is how a deployment abandons revocation.
func TestDeleteAIGatewayKeyHoldsDeletionWhileAConfiguredGatewayFails(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	agent.Status.AIGateway = &cetirunv1alpha1.AIGatewayStatus{Phase: cetirunv1alpha1.AIGatewayPhaseReady}
	r, _, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
	gateway.deleteErr = errors.New("synthetic outage")
	for _, enabled := range []bool{false, true} {
		r.AIGateway.Enabled = enabled
		if err := r.deleteAIGatewayKey(context.Background(), agent); err == nil {
			t.Fatalf("enabled=%v: a failed revocation must hold the deletion while the Gateway is configured", enabled)
		}
	}
	r.AIGateway = AIGatewayConfig{}
	if err := r.deleteAIGatewayKey(context.Background(), agent); err != nil {
		t.Fatalf("without a Gateway configured deletion must proceed: %v", err)
	}
}

func TestCopilotResponsesModelsReachTheProxy(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	agent.Annotations = map[string]string{cetirunv1alpha1.AnnotationCopilotModels: `[
		{"id":"gpt-5.5","responsesOnly":true},{"id":"claude-sonnet-4.5"},{"id":"gpt-6-sol","responsesOnly":true}]`}
	agent.Status.AIGateway = &cetirunv1alpha1.AIGatewayStatus{Copilot: &cetirunv1alpha1.AIGatewayCopilotStatus{
		// gpt-6-sol is not registered, for example for lack of a price.
		Models: []string{"copilot/claude-sonnet-4.5", "copilot/gpt-5.5"},
	}}
	r, _, _ := aiGatewayTestReconciler(t, agent)
	values := map[string]any{"aiGateway": map[string]any{"copilotProxy": map[string]any{"enabled": true}}}
	r.applyCopilotResponsesModels(values, agent)
	got := values["aiGateway"].(map[string]any)["copilotProxy"].(map[string]any)["responsesModels"]
	if !slices.Equal(got.([]any), []any{"gpt-5.5"}) {
		t.Fatalf("responsesModels = %v, want the registered responses-only models", got)
	}
	off := map[string]any{"aiGateway": map[string]any{"copilotProxy": map[string]any{"enabled": false}}}
	r.applyCopilotResponsesModels(off, agent)
	if _, set := off["aiGateway"].(map[string]any)["copilotProxy"].(map[string]any)["responsesModels"]; set {
		t.Fatal("an Agent without a proxy gets no proxy models")
	}
}

func TestReconcileAIGatewayRequiresRecoveryWhenTheGatewayLostItsData(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, _, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	team := aiGatewayTeamPrefix + testAIGatewayTeam
	if !gateway.teams[team] || !gateway.users["alice"] || !gateway.members[team+"/alice"] {
		t.Fatalf("first pass must create the owner: teams=%v users=%v members=%v", gateway.teams, gateway.users, gateway.members)
	}
	// The Gateway's database is replaced: keys, teams and users are gone,
	// while the operator still remembers having created the owner.
	gateway.keys = map[string]*aigateway.Key{}
	gateway.teams, gateway.users, gateway.members = map[string]bool{}, map[string]bool{}, map[string]bool{}
	stale := metav1.NewTime(time.Now().Add(-2 * aiGatewayVerificationInterval))
	agent.Status.AIGateway.LastVerifiedTime = &stale
	if err := r.reconcileAIGateway(context.Background(), agent, true); err == nil || agent.Status.AIGateway.Reason != aiGatewayReasonKeyRecordLost {
		t.Fatalf("lost key record must require explicit recovery: reason=%q err=%v", agent.Status.AIGateway.Reason, err)
	}
	if len(gateway.keys) != 0 || len(gateway.teams) != 0 || len(gateway.users) != 0 {
		t.Fatalf("recovery must not silently recreate a key or its owner: keys=%d teams=%v users=%v", len(gateway.keys), gateway.teams, gateway.users)
	}
}
