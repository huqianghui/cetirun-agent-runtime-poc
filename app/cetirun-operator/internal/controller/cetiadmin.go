// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"encoding/json"
	"fmt"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
)

func desiredCetiAdminAgent(config cetiadmin.Config, source *corev1.ConfigMap, chartVersion string) (*api.Agent, error) {
	if !config.Enabled || source == nil || source.UID == "" || source.Namespace == "" {
		return nil, fmt.Errorf("enabled CetiAdmin requires its installation source identity")
	}
	if config.Runtime.Version != "" {
		chartVersion = config.Runtime.Version
	}
	if chartVersion == "" {
		return nil, fmt.Errorf("CetiAdmin runtime chart version is required")
	}
	skills, err := json.Marshal(map[string]bool{"shared": config.SharedSkills.Enabled})
	if err != nil {
		return nil, err
	}
	agent := &api.Agent{
		TypeMeta: metav1.TypeMeta{APIVersion: api.GroupVersion.String(), Kind: "Agent"},
		ObjectMeta: metav1.ObjectMeta{Name: config.InstanceName, Annotations: map[string]string{
			cetiadmin.AnnotationSourceUID: string(source.UID),
			api.AnnotationOwnerTenantID:   config.Owner.TenantID,
			api.AnnotationOwnerObjectID:   config.Owner.ObjectID,
		}},
		Spec: api.AgentSpec{
			Agent:                 api.HarnessSpec{Type: api.AgentTypeHermes, Username: ptr.To("vibe")},
			User:                  api.UserSpec{Alias: config.Owner.Alias, DisplayName: "CetiAdmin"},
			Team:                  config.InstanceName,
			Model:                 config.Model.Default,
			ModelAccess:           &api.ModelAccessSpec{SharedModels: ptr.To(true), GitHubCopilot: ptr.To(config.Model.GitHubCopilot)},
			GatewayTokenSecretRef: "gateway-token",
			Helm:                  api.HelmSpec{Version: chartVersion, Values: map[string]runtime.RawExtension{"skills": {Raw: skills}}},
			Suspended:             true,
		},
	}
	if config.Runtime.Image != "" {
		agent.Spec.Agent.Image = ptr.To(config.Runtime.Image)
	}
	if config.Runtime.Port != nil {
		agent.Spec.Agent.Port = ptr.To(*config.Runtime.Port)
	}
	return agent, nil
}

func desiredCetiAdminTeam(config cetiadmin.Config, source *corev1.ConfigMap) (*api.Team, error) {
	if !config.Enabled || source == nil || source.UID == "" {
		return nil, fmt.Errorf("enabled CetiAdmin requires its installation source identity")
	}
	return &api.Team{
		TypeMeta: metav1.TypeMeta{APIVersion: api.GroupVersion.String(), Kind: "Team"},
		ObjectMeta: metav1.ObjectMeta{Name: config.InstanceName, Annotations: map[string]string{
			cetiadmin.AnnotationSourceUID: string(source.UID),
		}},
		Spec: api.TeamSpec{MemberContext: api.TeamContextSpec{
			Role: "CetiAdmin platform operations",
			Description: []string{
				"Use the cetiadmin skill for platform diagnostics and authorized maintenance.",
				"Kubernetes RBAC and admission determine access; this context grants no permissions.",
			},
		}},
	}, nil
}
