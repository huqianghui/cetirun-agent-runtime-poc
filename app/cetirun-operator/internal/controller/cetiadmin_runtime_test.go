// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
)

type cetiAdminTestProvider struct {
	binding cetiadmin.Binding
	err     error
}

func (p cetiAdminTestProvider) BindingForAgent(_ context.Context, agent *api.Agent) (cetiadmin.Binding, bool, error) {
	return p.binding, p.binding.Record().Agent.UID == agent.UID, p.err
}

func (cetiAdminTestProvider) OperatorImageReference() string {
	return "registry.example.com/cetirun-operator:dev@sha256:" + strings.Repeat("a", 64)
}

func boundCetiAdminForTest(t *testing.T, agent *api.Agent, budget string) cetiAdminTestProvider {
	t.Helper()
	config := cetiadmin.DefaultConfig()
	config.InstanceName = agent.Name
	config.Owner = cetiadmin.Owner{Alias: agent.Spec.User.Alias, TenantID: "11111111-1111-1111-1111-111111111111", ObjectID: "22222222-2222-2222-2222-222222222222"}
	config.Kubernetes.APIAudience = "kubernetes-api"
	config.Kubernetes.APIEgressCIDRs = []string{"192.0.2.10/32"}
	config.Model.Default, config.Model.InitialMonthlyBudgetUSD = agent.Spec.Model, budget
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	source := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cetiadmin-config", Namespace: "cetirun-system", UID: "source-uid"}, Data: map[string]string{cetiadmin.ConfigKey: string(raw)}}
	if agent.Annotations == nil {
		agent.Annotations = map[string]string{}
	}
	agent.Annotations[cetiadmin.AnnotationSourceUID] = string(source.UID)
	agent.Annotations[api.AnnotationOwnerTenantID], agent.Annotations[api.AnnotationOwnerObjectID] = config.Owner.TenantID, config.Owner.ObjectID
	owner := metav1.OwnerReference{APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: agent.Name, UID: agent.UID, Controller: ptr.To(true)}
	team := &api.Team{ObjectMeta: metav1.ObjectMeta{Name: agent.Name, UID: "team-uid", Annotations: map[string]string{cetiadmin.AnnotationSourceUID: string(source.UID)}}}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: instanceNamespace(agent.Name), UID: "namespace-uid", OwnerReferences: []metav1.OwnerReference{owner}}}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: instanceServiceName(agent.Name), Namespace: namespace.Name, UID: "account-uid", OwnerReferences: []metav1.OwnerReference{owner}, Annotations: map[string]string{cetiadmin.AnnotationSourceUID: string(source.UID)}}, AutomountServiceAccountToken: ptr.To(false)}
	identity := func(o metav1.Object) cetiadmin.ObjectIdentity {
		return cetiadmin.ObjectIdentity{Name: o.GetName(), Namespace: o.GetNamespace(), UID: o.GetUID()}
	}
	hash, _ := cetiadmin.ConfigHash(config)
	record := cetiadmin.Record{SchemaVersion: 1, Source: identity(source), ConfigHash: hash, Agent: identity(agent), Team: identity(team), Namespace: identity(namespace), ServiceAccount: identity(sa)}
	binding, err := cetiadmin.ResolveBinding(config, &record, cetiadmin.ObservedInstallation{Source: source, Agent: agent, Team: team, Namespace: namespace, ServiceAccount: sa})
	if err != nil {
		t.Fatal(err)
	}
	return cetiAdminTestProvider{binding: binding}
}

func TestCetiAdminNetworkProjectionIsScopedAndIdempotent(t *testing.T) {
	access := cetiadmin.KubernetesAccess{APIServer: "https://kubernetes.default.svc", APIEgressCIDRs: []string{"192.0.2.10/32", "2001:db8::10/128"}}
	values := map[string]any{"networkPolicy": map[string]any{"allowedInternalCIDRs": []any{
		map[string]any{"cidr": "198.51.100.20/32", "ports": []any{map[string]any{"port": 8443}}},
		map[string]any{"cidr": "192.0.2.10/32", "ports": []any{map[string]any{"port": 1234}}},
	}}, "sidecarScope": map[string]any{"additionalEgressHosts": []any{"tools/mcp.tools.svc.cluster.local"}}}
	if err := applyCetiAdminNetwork(values, access); err != nil {
		t.Fatal(err)
	}
	first, _ := json.Marshal(values)
	if err := applyCetiAdminNetwork(values, access); err != nil {
		t.Fatal(err)
	}
	second, _ := json.Marshal(values)
	if string(first) != string(second) {
		t.Fatal("network projection accumulated rules")
	}
	network := values["networkPolicy"].(map[string]any)
	rules := network["allowedInternalCIDRs"].([]any)
	if len(rules) != 3 || rules[0].(map[string]any)["cidr"] != "198.51.100.20/32" {
		t.Fatal("unrelated network rule was removed")
	}
	for _, rule := range rules[1:] {
		if !reflect.DeepEqual(rule.(map[string]any)["ports"], []any{map[string]any{"protocol": "TCP", "port": 443}}) {
			t.Fatal("API rule grants the wrong port")
		}
	}
	if !strings.Contains(string(first), "169.254.169.254/32") || !strings.Contains(string(first), "default/kubernetes.default.svc.cluster.local") {
		t.Fatal("IMDS or mesh contract missing")
	}
}

func TestCetiAdminRuntimeUsesExistingHelmProducer(t *testing.T) {
	agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "cetiadmin", UID: "admin-uid"}, Spec: api.AgentSpec{
		Agent: api.HarnessSpec{Type: api.AgentTypeHermes, Username: ptr.To("vibe")}, User: api.UserSpec{Alias: "owner"}, Team: "cetiadmin", Model: "auto", GatewayTokenSecretRef: "native-auth", Helm: api.HelmSpec{Version: "0.1.1"}}}
	provider := boundCetiAdminForTest(t, agent, "100")
	r := &AgentReconciler{CetiAdmin: provider, HelmClient: &chartHelm{}, DomainSuffix: "example.com", DefaultModel: "auto",
		ChartRegistry: "registry.example.com", AIGateway: AIGatewayConfig{Enabled: true, URL: "http://gateway.system.svc.cluster.local:4000"}}
	prepared, err := r.prepareRuntimeAgent(context.Background(), agent, &api.Team{ObjectMeta: metav1.ObjectMeta{Name: agent.Spec.Team}})
	if err != nil {
		input, _ := r.agentPreparationValues(agent, &api.Team{})
		_ = r.applyCetiAdminValues(context.Background(), agent, input)
		projection, _ := provider.binding.RuntimeProjection(provider.OperatorImageReference(), "")
		encoded, _ := json.Marshal(projection)
		var fields map[string]any
		_ = json.Unmarshal(encoded, &fields)
		input["cetiadmin"] = fields
		chrt, _ := loader.Load("../../../cetirun-chart")
		values, _ := chartutil.CoalesceValues(chrt, input)
		t.Logf("synthetic input schema diagnostic: %v", chartutil.ValidateAgainstSchema(chrt, values))
		t.Fatal(err)
	}
	projection := prepared.Values["cetiadmin"].(map[string]any)
	if projection["operatorImage"] != provider.OperatorImageReference() || projection["agentUID"] != string(agent.UID) {
		t.Fatal("producer changed the binding or asset image")
	}
	files := prepared.Values["runtime"].(map[string]any)["configFiles"].(map[string]any)
	var native struct {
		Skills struct {
			ExternalDirs []string `json:"external_dirs"`
		} `json:"skills"`
	}
	if err := yaml.Unmarshal([]byte(files["hermes-config.yaml"].(string)), &native); err != nil {
		t.Fatal(err)
	}
	for _, directory := range native.Skills.ExternalDirs {
		if directory == "/home/vibe/.aks/skills" {
			t.Fatal("CetiAdmin with shared skills disabled declares an unmounted default skill source")
		}
	}
	account := prepared.Values["serviceAccount"].(map[string]any)
	if account["create"] != false || account["automountToken"] != false || account["name"] != instanceServiceName(agent.Name) {
		t.Fatal("identity synthesis replaced the managed ServiceAccount")
	}
	if _, exists := prepared.Values["agentRequest"]; exists {
		t.Fatal("internal authority request reached the Helm chart")
	}
	chrt, err := loader.Load("../../../cetirun-chart")
	if err != nil {
		t.Fatal(err)
	}
	capabilities := chartutil.DefaultCapabilities.Copy()
	capabilities.KubeVersion.Version = "v1.37.0"
	capabilities.APIVersions = append(capabilities.APIVersions, "networking.istio.io/v1/Sidecar")
	renderValues, err := chartutil.ToRenderValues(chrt, prepared.Values, chartutil.ReleaseOptions{Name: helmReleaseName(agent.Name), Namespace: instanceNamespace(agent.Name), Revision: 1, IsInstall: true}, capabilities)
	if err != nil {
		t.Fatal(err)
	}
	manifests, err := engine.Render(chrt, renderValues)
	if err != nil {
		t.Fatal(err)
	}
	var workload struct {
		Spec struct {
			Template struct {
				Spec corev1.PodSpec `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(manifests["cetirun/templates/statefulset.yaml"]), &workload); err != nil {
		t.Fatal(err)
	}
	if workload.Spec.Template.Spec.ServiceAccountName != instanceServiceName(agent.Name) || len(workload.Spec.Template.Spec.Containers) == 0 {
		t.Fatal("generated StatefulSet did not consume the bound runtime")
	}
	exported := false
	for _, container := range workload.Spec.Template.Spec.InitContainers {
		if container.Name == "cetiadmin-assets" {
			exported = container.Image == provider.OperatorImageReference() && len(container.VolumeMounts) == 1 && container.VolumeMounts[0].Name == "cetiadmin-assets"
		}
	}
	if !exported {
		t.Fatal("actual runtime manifest did not isolate the same-image exporter")
	}
	r.CetiAdmin = nil
	ordinary, err := r.prepareRuntimeAgent(context.Background(), agent, &api.Team{ObjectMeta: metav1.ObjectMeta{Name: agent.Spec.Team}})
	if err != nil {
		t.Fatal(err)
	}
	if ordinary.Values["cetiadmin"].(map[string]any)["enabled"] != false {
		t.Fatal("ordinary Hermes received admin assets")
	}
}

func TestCetiAdminSkillPathsKeepMountedAndExplicitSources(t *testing.T) {
	for _, test := range []struct {
		name       string
		shared     bool
		memory     bool
		mount      string
		explicit   []any
		wantLegacy bool
	}{
		{name: "disabled", mount: "/home/vibe/.aks/skills"},
		{name: "disabled-with-memory", memory: true, mount: "/home/vibe/.aks/skills"},
		{name: "shared-default", shared: true, mount: "/home/vibe/.aks/skills", wantLegacy: true},
		{name: "shared-with-memory", shared: true, memory: true, mount: "/home/vibe/.aks/skills", wantLegacy: true},
		{name: "shared-other-mount", shared: true, mount: "/home/vibe/shared-skills"},
		{name: "explicit-user-source", mount: "/home/vibe/.aks/skills", explicit: []any{"/home/vibe/user-skills"}},
		{name: "explicit-legacy-source", mount: "/home/vibe/.aks/skills", explicit: []any{"/home/vibe/.aks/skills"}, wantLegacy: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "cetiadmin", UID: "admin-uid"}, Spec: api.AgentSpec{
				Agent: api.HarnessSpec{Type: api.AgentTypeHermes, Username: ptr.To("vibe")}, User: api.UserSpec{Alias: "owner"}, Team: "cetiadmin", Model: "auto", GatewayTokenSecretRef: "native-auth", Helm: api.HelmSpec{Version: "0.1.1"}}}
			r := &AgentReconciler{CetiAdmin: boundCetiAdminForTest(t, agent, "100"), HelmClient: &chartHelm{}, DomainSuffix: "example.com", DefaultModel: "auto",
				ChartRegistry: "registry.example.com", AIGateway: AIGatewayConfig{Enabled: true, URL: "http://gateway.system.svc.cluster.local:4000"}}
			input, err := r.agentPreparationValues(agent, &api.Team{ObjectMeta: metav1.ObjectMeta{Name: agent.Spec.Team}})
			if err != nil {
				t.Fatal(err)
			}
			if err := r.applyCetiAdminValues(context.Background(), agent, input); err != nil {
				t.Fatal(err)
			}
			input["skills"] = map[string]any{"shared": test.shared, "mountPath": test.mount, "resourceGroup": "fixture", "storageAccount": "fixture", "containerName": "skills"}
			input["memoryOptions"] = helm.MemoryOptions{Enabled: test.memory, Image: "registry.example.com/memory:1.2.3"}
			if test.explicit != nil {
				input["hermes"] = map[string]any{"config": map[string]any{"skills": map[string]any{"external_dirs": test.explicit}}}
			}
			prepared, err := r.HelmClient.Prepare(instanceNamespace(agent.Name), input, agent.Spec.Helm.Version)
			if err != nil {
				t.Fatal(err)
			}
			files := prepared.Values["runtime"].(map[string]any)["configFiles"].(map[string]any)
			var native struct {
				Skills struct {
					ExternalDirs []string `json:"external_dirs"`
				} `json:"skills"`
			}
			if err := yaml.Unmarshal([]byte(files["hermes-config.yaml"].(string)), &native); err != nil {
				t.Fatal(err)
			}
			contains := func(want string) bool {
				for _, actual := range native.Skills.ExternalDirs {
					if actual == want {
						return true
					}
				}
				return false
			}
			if contains("/home/vibe/.aks/skills") != test.wantLegacy || !contains(cetiadmin.AssetsMountPath+"/skills") || contains("/home/vibe/.agents/skills") != test.memory {
				t.Fatalf("skill roots disagree with current bindings: %v", native.Skills.ExternalDirs)
			}
			if test.shared && !contains(test.mount) {
				t.Fatalf("active shared skill source is not registered: %s", test.mount)
			}
			for _, expected := range test.explicit {
				if !contains(expected.(string)) {
					t.Fatal("explicit source was discarded")
				}
			}
		})
	}
}

func TestCetiAdminGatewayInitialBudgetDoesNotResetExistingKey(t *testing.T) {
	agent := aiGatewayTestAgent("alice")
	agent.Spec.Agent.Type, agent.Spec.SandboxRef, agent.Status.Policy = api.AgentTypeHermes, "", nil
	agent.Spec.Model, agent.Spec.ModelAccess = testSharedModelOne, modelAccess(true, false)
	provider := boundCetiAdminForTest(t, agent, "27.50")
	team := &api.Team{ObjectMeta: metav1.ObjectMeta{Name: agent.Name}}
	r, kube, gateway := aiGatewayTestReconciler(t, agent, team, aiGatewayNamespace())
	r.CetiAdmin = provider
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	if len(gateway.generated) != 1 || gateway.generated[0].MaxBudget != 27.5 || r.AIGateway.DefaultBudget != 10 {
		t.Fatal("CetiAdmin did not initialize its own budget independently")
	}
	keyID := agent.Status.AIGateway.KeyID
	for _, key := range gateway.keys {
		key.MaxBudget = ptr.To(71.0)
		key.Spend = ptr.To(4.75)
	}
	r.CetiAdmin = boundCetiAdminForTest(t, agent, "99")
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	if agent.Status.AIGateway.BudgetUSD != "71" || agent.Status.AIGateway.SpendUSD != "4.75" || agent.Status.AIGateway.KeyID != keyID || len(gateway.generated) != 1 {
		t.Fatal("a config refresh replaced existing spend, key or budget")
	}
	for _, key := range gateway.keys {
		key.Blocked = true
	}
	if err := r.reconcileAIGateway(context.Background(), agent, true); err == nil {
		t.Fatal("external key block was ignored")
	}
	for _, key := range gateway.keys {
		if !key.Blocked {
			t.Fatal("external key block was cleared")
		}
	}
	current := &api.Agent{}
	if err := kube.Get(context.Background(), types.NamespacedName{Name: agent.Name}, current); err != nil {
		t.Fatal(err)
	}
}

func TestCetiAdminSuspensionPreservesExternalKeyBlock(t *testing.T) {
	agent := aiGatewayTestAgent("alice")
	agent.Spec.Agent.Type, agent.Spec.SandboxRef, agent.Status.Policy = api.AgentTypeHermes, "", nil
	agent.Spec.Model, agent.Spec.ModelAccess = testSharedModelOne, modelAccess(true, false)
	provider := boundCetiAdminForTest(t, agent, "20")
	r, _, gateway := aiGatewayTestReconciler(t, agent, &api.Team{ObjectMeta: metav1.ObjectMeta{Name: agent.Name}}, aiGatewayNamespace())
	r.CetiAdmin = provider
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	for _, key := range gateway.keys {
		key.Blocked = true
	}
	reason := agent.Status.AIGateway.Reason
	if err := r.suspendGatewayKey(context.Background(), agent); err != nil {
		t.Fatalf("already blocked key prevented safe suspension: %v", err)
	}
	if !agent.Status.AIGateway.Blocked || agent.Status.AIGateway.Routed || agent.Status.AIGateway.Reason != reason || aiGatewayOperatorOwnedBlock(agent.Status.AIGateway.Reason) {
		t.Fatal("suspension took ownership of an external block")
	}
	if err := r.reconcileAIGateway(context.Background(), agent, true); err == nil {
		t.Fatal("resume cleared or ignored the external key block")
	}
	for _, key := range gateway.keys {
		if !key.Blocked {
			t.Fatal("external key block was cleared")
		}
	}
}
