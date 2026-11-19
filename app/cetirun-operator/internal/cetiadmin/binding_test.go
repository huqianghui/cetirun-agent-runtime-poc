// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cetiadmin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

func bindingFixture(t *testing.T) (Config, Record, ObservedInstallation) {
	t.Helper()
	config, err := ParseConfig(validConfigJSON)
	if err != nil {
		t.Fatal(err)
	}
	source := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-cetiadmin", Namespace: "platform-test", UID: "source-uid", ResourceVersion: "1"}, Data: map[string]string{ConfigKey: validConfigJSON}}
	annotations := map[string]string{AnnotationSourceUID: string(source.UID), api.AnnotationOwnerTenantID: config.Owner.TenantID, api.AnnotationOwnerObjectID: config.Owner.ObjectID}
	agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: config.InstanceName, UID: "agent-uid", Annotations: annotations}, Spec: api.AgentSpec{
		Agent: api.HarnessSpec{Type: api.AgentTypeHermes}, User: api.UserSpec{Alias: config.Owner.Alias, DisplayName: "CetiAdmin"},
		Team: config.InstanceName, GatewayTokenSecretRef: "gateway-token",
	}}
	team := &api.Team{ObjectMeta: metav1.ObjectMeta{Name: config.InstanceName, UID: "team-uid", Annotations: map[string]string{AnnotationSourceUID: string(source.UID)}}}
	owners := []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: agent.Name, UID: agent.UID, Controller: ptr.To(true)}}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-" + agent.Name, UID: "namespace-uid", OwnerReferences: owners}}
	account := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: "openclaw-cetiadmin-cetirun", Namespace: namespace.Name, UID: "account-uid", OwnerReferences: owners,
		Annotations: map[string]string{AnnotationSourceUID: string(source.UID)},
	}, AutomountServiceAccountToken: ptr.To(false)}
	hash, err := ConfigHash(config)
	if err != nil {
		t.Fatal(err)
	}
	record := Record{SchemaVersion: 1, ConfigHash: hash,
		Source: ObjectIdentity{Name: source.Name, Namespace: source.Namespace, UID: source.UID},
		Agent:  ObjectIdentity{Name: agent.Name, UID: agent.UID}, Team: ObjectIdentity{Name: team.Name, UID: team.UID},
		Namespace:      ObjectIdentity{Name: namespace.Name, UID: namespace.UID},
		ServiceAccount: ObjectIdentity{Name: account.Name, Namespace: account.Namespace, UID: account.UID},
	}
	return config, record, ObservedInstallation{Source: source, Agent: agent, Team: team, Namespace: namespace, ServiceAccount: account}
}

func TestResolveBindingUsesRecordedIdentities(t *testing.T) {
	config, record, observed := bindingFixture(t)
	binding, err := ResolveBinding(config, &record, observed)
	if err != nil {
		t.Fatal(err)
	}
	if binding.ServiceAccountUsername() != "system:serviceaccount:cetirun-cetiadmin:openclaw-cetiadmin-cetirun" || binding.Record().Agent.UID != types.UID("agent-uid") {
		t.Fatalf("wrong binding: %#v", binding.Record())
	}
	observed.Source.ResourceVersion = "2"
	if _, err := ResolveBinding(config, &record, observed); err != nil {
		t.Fatalf("unchanged source content with new metadata: %v", err)
	}
}

func TestResolveBindingRejectsUnconfirmedIdentity(t *testing.T) {
	for _, tt := range []struct {
		name string
		edit func(*Config, *Record, *ObservedInstallation)
	}{
		{"source UID", func(_ *Config, _ *Record, o *ObservedInstallation) { o.Source.UID = "replacement" }},
		{"source namespace", func(_ *Config, _ *Record, o *ObservedInstallation) { o.Source.Namespace = "other" }},
		{"source body", func(_ *Config, _ *Record, o *ObservedInstallation) { o.Source.Data[ConfigKey] = `{"enabled":false}` }},
		{"disabled", func(c *Config, _ *Record, _ *ObservedInstallation) { c.Enabled = false }},
		{"record version", func(_ *Config, r *Record, _ *ObservedInstallation) { r.SchemaVersion = 2 }},
		{"record hash", func(_ *Config, r *Record, _ *ObservedInstallation) { r.ConfigHash = "other" }},
		{"missing Agent", func(_ *Config, _ *Record, o *ObservedInstallation) { o.Agent = nil }},
		{"Agent UID", func(_ *Config, _ *Record, o *ObservedInstallation) { o.Agent.UID = "replacement" }},
		{"Agent name", func(_ *Config, _ *Record, o *ObservedInstallation) { o.Agent.Name = "copied-admin" }},
		{"Agent marker", func(_ *Config, _ *Record, o *ObservedInstallation) { delete(o.Agent.Annotations, AnnotationSourceUID) }},
		{"owner", func(_ *Config, _ *Record, o *ObservedInstallation) { o.Agent.Spec.User.Alias = "other-owner" }},
		{"owner object", func(_ *Config, _ *Record, o *ObservedInstallation) {
			o.Agent.Annotations[api.AnnotationOwnerObjectID] = "other"
		}},
		{"wrong runtime", func(_ *Config, _ *Record, o *ObservedInstallation) { o.Agent.Spec.Agent.Type = api.AgentTypeOpenClaw }},
		{"Sandbox", func(_ *Config, _ *Record, o *ObservedInstallation) { o.Agent.Spec.SandboxRef = "box" }},
		{"Team UID", func(_ *Config, _ *Record, o *ObservedInstallation) { o.Team.UID = "replacement" }},
		{"Team policy", func(_ *Config, _ *Record, o *ObservedInstallation) { o.Team.Spec.AgentPolicyRef = "policy" }},
		{"Team selection", func(_ *Config, _ *Record, o *ObservedInstallation) { o.Agent.Spec.Team = "other-team" }},
		{"namespace UID", func(_ *Config, _ *Record, o *ObservedInstallation) { o.Namespace.UID = "replacement" }},
		{"namespace owner", func(_ *Config, _ *Record, o *ObservedInstallation) { o.Namespace.OwnerReferences = nil }},
		{"account UID", func(_ *Config, _ *Record, o *ObservedInstallation) { o.ServiceAccount.UID = "replacement" }},
		{"account namespace", func(_ *Config, _ *Record, o *ObservedInstallation) { o.ServiceAccount.Namespace = "other" }},
		{"account owner", func(_ *Config, _ *Record, o *ObservedInstallation) { o.ServiceAccount.OwnerReferences = nil }},
		{"account automount", func(_ *Config, _ *Record, o *ObservedInstallation) {
			o.ServiceAccount.AutomountServiceAccountToken = ptr.To(true)
		}},
		{"account default automount", func(_ *Config, _ *Record, o *ObservedInstallation) {
			o.ServiceAccount.AutomountServiceAccountToken = nil
		}},
		{"terminating", func(_ *Config, _ *Record, o *ObservedInstallation) {
			now := metav1.Now()
			o.Namespace.DeletionTimestamp = &now
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			config, record, observed := bindingFixture(t)
			tt.edit(&config, &record, &observed)
			if _, err := ResolveBinding(config, &record, observed); err == nil {
				t.Fatal("unconfirmed identity became a management binding")
			}
		})
	}
}

func TestBindingCannotBeAdoptedByNameOrLabel(t *testing.T) {
	config, _, observed := bindingFixture(t)
	if _, err := ResolveBinding(config, nil, observed); err == nil {
		t.Fatal("existing named objects were adopted without a protected record")
	}
	if got := (Binding{}).ServiceAccountUsername(); got != "" {
		t.Fatalf("zero binding acquired a principal: %s", got)
	}
}

func TestBindingClonesPlatformReadNamespaces(t *testing.T) {
	config, record, observed := bindingFixture(t)
	config.PlatformReadNamespaces = []string{"platform-test", "kube-system"}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	observed.Source.Data[ConfigKey] = string(data)
	record.ConfigHash, err = ConfigHash(config)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := ResolveBinding(config, &record, observed)
	if err != nil {
		t.Fatal(err)
	}
	config.PlatformReadNamespaces[0] = "changed-input"
	config.Kubernetes.APIEgressCIDRs[0] = "192.0.2.55/32"
	copy := binding.Config()
	if !slices.Equal(copy.PlatformReadNamespaces, []string{"platform-test", "kube-system"}) ||
		!slices.Equal(copy.Kubernetes.APIEgressCIDRs, []string{"192.0.2.44/32"}) {
		t.Fatal("binding shares arrays with its input")
	}
	copy.PlatformReadNamespaces[0] = "changed-output"
	copy.Kubernetes.APIEgressCIDRs[0] = "192.0.2.66/32"
	current := binding.Config()
	if !slices.Equal(current.PlatformReadNamespaces, []string{"platform-test", "kube-system"}) ||
		!slices.Equal(current.Kubernetes.APIEgressCIDRs, []string{"192.0.2.44/32"}) {
		t.Fatal("binding shares arrays with its returned configuration")
	}
}

func TestConfigHashUsesCanonicalConfigJSON(t *testing.T) {
	canonical := `{"enabled":false,"instanceName":"cetiadmin","owner":{"alias":"","tenantID":"","objectID":""},` +
		`"kubernetes":{"apiServer":"https://kubernetes.default.svc","apiAudience":"","caConfigMapName":"kube-root-ca.crt","apiEgressCIDRs":null},` +
		`"model":{"default":"auto","initialMonthlyBudgetUSD":"","githubCopilot":false},"runtime":{},` +
		`"azureWorkloadIdentity":{"enabled":false},"sharedSkills":{"enabled":false},` +
		`"monitor":{"enabled":false,"timezone":"UTC","deliver":"local"}}`
	for _, test := range []struct{ name, raw, expected string }{
		{"default", `{"enabled":false}`, canonical},
		{"empty diagnostics", `{"enabled":false,"platformReadNamespaces":[]}`, canonical},
		{"null diagnostics", `{"enabled":false,"platformReadNamespaces":null}`, canonical},
		{"platform diagnostics", `{"platformReadNamespaces":["kube-system"],"enabled":false}`,
			strings.Replace(canonical, `"kubernetes":`, `"platformReadNamespaces":["kube-system"],"kubernetes":`, 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			config, err := ParseConfig(test.raw)
			if err != nil {
				t.Fatal(err)
			}
			actual, err := ConfigHash(config)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256([]byte(test.expected))
			if actual != hex.EncodeToString(sum[:]) {
				t.Fatalf("hash does not match canonical Config JSON: %s", actual)
			}
		})
	}
}
