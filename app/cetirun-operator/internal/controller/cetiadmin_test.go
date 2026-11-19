// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"encoding/json"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestCetiAdminDesiredAgent(t *testing.T) {
	config := cetiadmin.DefaultConfig()
	config.Owner = cetiadmin.Owner{Alias: "platform-owner", TenantID: "tenant-id", ObjectID: "owner-id"}
	config.Model.InitialMonthlyBudgetUSD = "7.50"
	config.SharedSkills.Enabled = true
	source := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cetiadmin", Namespace: "platform-test", UID: "source-uid"}}
	agent, err := desiredCetiAdminAgent(config, source, "0.1.1-dev.cetiadmin")
	if err != nil {
		t.Fatal(err)
	}
	if agent.Name != "cetiadmin" || agent.Spec.User.Alias != "platform-owner" || agent.Spec.Team != "cetiadmin" || agent.Spec.Agent.Type != api.AgentTypeHermes {
		t.Fatalf("wrong instance identity: %#v", agent)
	}
	if !agent.Spec.Suspended || agent.Spec.SandboxRef != "" || agent.Spec.Helm.Version != "0.1.1-dev.cetiadmin" || *agent.Spec.Agent.Username != "vibe" {
		t.Fatal("bootstrap did not hold the ordinary Hermes runtime")
	}
	if agent.Spec.Agent.Image != nil || agent.Spec.Agent.Port != nil {
		t.Fatal("did not leave default image/port resolution to the selected profile")
	}
	if agent.Annotations[api.AnnotationOwnerTenantID] != "tenant-id" || agent.Annotations[api.AnnotationOwnerObjectID] != "owner-id" || agent.Annotations[cetiadmin.AnnotationSourceUID] != "source-uid" {
		t.Fatal("owner or installation source changed")
	}
	if !agent.Spec.SharedModelsEnabled() || agent.Spec.GitHubCopilotEnabled() || agent.Spec.Model != "auto" || agent.Spec.ModelAccess.MonthlyBudgetUSD != "" {
		t.Fatal("changed model access or treated initial key budget as an Agent policy budget")
	}
	if string(agent.Spec.Helm.Values["skills"].Raw) != `{"shared":true}` || len(agent.Spec.Helm.Values) != 1 {
		t.Fatal("shared skills were lost or privilege overrides were persisted")
	}
	encoded, _ := json.Marshal(agent)
	if strings.Contains(strings.ToLower(string(encoded)), "msteams") {
		t.Fatal("CetiAdmin restored Teams integration")
	}
	team, err := desiredCetiAdminTeam(config, source)
	if err != nil || team.Spec.PolicyConfigured() || team.Spec.MemberContext.Role == "" || team.Name != agent.Spec.Team {
		t.Fatalf("wrong ordinary Team: %#v, %v", team, err)
	}
}

func TestCetiAdminDesiredAgentKeepsExplicitRuntime(t *testing.T) {
	config := cetiadmin.DefaultConfig()
	config.Runtime = cetiadmin.RuntimeCoordinates{Version: "0.1.1-dev.pin", Image: "registry.example.com/hermes-gateway:pin", Port: ptr.To(int32(8642))}
	source := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "platform-test", UID: "source-uid"}}
	agent, err := desiredCetiAdminAgent(config, source, "0.1.1-dev.default")
	if err != nil || agent.Spec.Helm.Version != config.Runtime.Version || *agent.Spec.Agent.Image != config.Runtime.Image || *agent.Spec.Agent.Port != *config.Runtime.Port {
		t.Fatalf("explicit runtime was changed: %#v, %v", agent, err)
	}
	*config.Runtime.Port = 9000
	if *agent.Spec.Agent.Port != 8642 {
		t.Fatal("Agent port aliases the mutable input")
	}
}

func TestCetiAdminDesiredAgentRejectsUnboundSource(t *testing.T) {
	config := cetiadmin.DefaultConfig()
	for _, source := range []*corev1.ConfigMap{nil, {}, {ObjectMeta: metav1.ObjectMeta{Namespace: "platform-test"}}} {
		if _, err := desiredCetiAdminAgent(config, source, "0.1.1"); err == nil {
			t.Fatal("constructed an Agent without a source UID")
		}
	}
	config.Enabled = false
	if _, err := desiredCetiAdminAgent(config, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "platform-test", UID: "source-uid"}}, "0.1.1"); err == nil {
		t.Fatal("disabled installation constructed an Agent")
	}
}
