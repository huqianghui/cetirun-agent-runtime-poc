// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/agents"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/observability"
	"github.com/Azure/CetiRun/app/internal/registryauth"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

func runtimeRequest(instance *api.Agent, registry string) map[string]any {
	agent := instance.Spec.Agent
	request := map[string]any{
		"type": string(agent.Type), "registry": registry, "uiPrefix": "/ui/" + instance.Name,
		"tailscaleEnabled": instance.Spec.Tailscale != nil,
		"releaseName":      helmReleaseName(instance.Name),
	}
	if agent.Image != nil {
		request["image"] = *agent.Image
	}
	if agent.Username != nil {
		request["username"] = *agent.Username
	}
	if agent.Port != nil {
		request["port"] = *agent.Port
	}
	if agent.RegistrySecretRef != nil {
		name, _ := registryauth.PullSecretName(instance.UID)
		request["pullSecretName"] = name
	}
	if agent.WebUI {
		request["webUI"] = agent.WebUI
	}
	return request
}

func (r *AgentReconciler) rejectAgentConfig(ctx context.Context, instance *api.Agent, err error) (ctrl.Result, error) {
	message := agentConfigMessage(ctx, err)
	setCondition(&instance.Status.Conditions, api.ConditionAgentConfigReady,
		metav1.ConditionFalse, "AgentConfigInvalid", message)
	return r.failWith(ctx, instance, "AgentConfigInvalid", message)
}

// agentConfigMessage is what the Agent's conditions and Warning event say
// about err. The cause of a chart download failure comes from the registry
// and gives the Agent's owner nothing to act on, so only the operator log
// records it.
func agentConfigMessage(ctx context.Context, err error) string {
	var download *helm.ChartDownloadError
	if !errors.As(err, &download) {
		return err.Error()
	}
	log.FromContext(ctx).Error(err, "Agent chart download failed")
	return fmt.Sprintf("Agent chart %s could not be downloaded from the registry; the operator will retry", download.Version)
}

// User and deployment overrides cannot inject a prepared plan or replace its
// primary image/port. Unrelated deployment settings keep their existing scope.
func validateRuntimeOverrides(values map[string]any, agentType string) error {
	for _, key := range []string{"runtime", "agentRequest", "imagePullSecrets", "customBindings", "memoryContext", "cetiadmin"} {
		if _, ok := values[key]; ok {
			return fmt.Errorf("helm.values.%s is reserved for the platform", key)
		}
	}
	// Gateway routing is a controller projection of Policy. Permitting a
	// Helm value to replace any part of it would let an Agent bypass the
	// virtual key, select an unapproved provider/model, or send traffic around
	// the Gateway. Keep these paths reserved even while the Gateway is disabled
	// so a value cannot become an escape hatch after it is enabled.
	if _, ok := values["aiGateway"]; ok {
		return fmt.Errorf("helm.values.aiGateway is reserved for the platform")
	}
	if _, ok := values["connectionsMCP"]; ok {
		return fmt.Errorf("helm.values.connectionsMCP is reserved for the platform")
	}
	if secrets, ok := values["secrets"].(map[string]any); ok {
		for _, field := range []string{"provider", "authMode", "apiBase", "apiKey", "model"} {
			if _, present := secrets[field]; present {
				return fmt.Errorf("helm.values.secrets.%s is reserved for AI Gateway routing", field)
			}
		}
	}
	if annotations, ok := values["podAnnotations"].(map[string]any); ok {
		for _, field := range []string{"cetirun.microsoft.com/registry-binding-only", "cetirun.microsoft.com/connections-ca-hash", aiGatewayKeyIDPodAnnotation} {
			if _, present := annotations[field]; present {
				return fmt.Errorf("platform rollout annotations are platform-owned")
			}
		}
		for key := range annotations {
			if observability.CollectorAnnotation(key) {
				return fmt.Errorf("pod annotation %s is reserved for the platform's log and metrics collection", key)
			}
		}
	}
	if _, ok := values["image"]; ok {
		return fmt.Errorf("helm.values.image must use spec.agent.image")
	}
	if agentType == "hermes" {
		if sources, ok := values["extraEnvFrom"].([]any); ok && len(sources) != 0 {
			return fmt.Errorf("Hermes bulk environment sources cannot override platform configuration")
		}
	}
	if extraEnv, ok := values["extraEnv"].([]any); ok {
		for _, item := range extraEnv {
			entry, _ := item.(map[string]any)
			name, _ := entry["name"].(string)
			if agentType == "hermes" && hermesReservedEnvironment(name) || name == "CETIRUN_AGENT_BROKER_URL" || name == "CETIRUN_CONNECTIONS_MCP_URL" || name == "SSL_CERT_FILE" || name == "NODE_EXTRA_CA_CERTS" || strings.HasPrefix(name, "CETIRUN_MEMORY_") || strings.HasPrefix(name, "CETIADMIN_") || name == "KUBECONFIG" {
				return fmt.Errorf("helm.values.extraEnv contains a platform-reserved environment variable")
			}
		}
	}
	if native, ok := values[agentType].(map[string]any); ok {
		if _, ok := native["image"]; ok {
			return fmt.Errorf("native image overrides must use spec.agent.image")
		}
		if agentType == "hermes" {
			if err := validateHermesOverrides(native); err != nil {
				return err
			}
		}
		if env, ok := native["env"].(map[string]any); ok {
			if _, ok := env["API_SERVER_PORT"]; ok {
				return fmt.Errorf("native port overrides must use spec.agent.port")
			}
		}
		if config, ok := native["config"].(map[string]any); ok {
			if _, ok := config["model"]; ok {
				return fmt.Errorf("native model overrides must use spec.model and Policy policy")
			}
			if gateway, ok := config["gateway"].(map[string]any); ok {
				if _, ok := gateway["port"]; ok {
					return fmt.Errorf("native port overrides must use spec.agent.port")
				}
			}
		}
	}
	if service, ok := values["service"].(map[string]any); ok {
		for _, field := range []string{"port", "targetPort"} {
			if _, ok := service[field]; ok {
				return fmt.Errorf("service.%s must use spec.agent.port", field)
			}
		}
	}
	return nil
}

func hermesReservedEnvironment(name string) bool {
	for _, prefix := range []string{"HERMES_WEBAPP_", "HERMES_DASHBOARD_", "HERMES_CONFIG_", "HERMES_ENV_", "HERMES_INSTALL_", "HERMES_PROFILE", "API_SERVER_", "CETIRUN_GATEWAY_"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	switch name {
	case "HERMES_HOME", "HERMES_MANAGED_DIR", "HERMES_CONFIG", "HERMES_CONFIG_FILE", "HERMES_CONFIG_PATH", "HERMES_ENV", "HERMES_ENV_FILE", "HERMES_PROFILE", "HERMES_IGNORE_USER_CONFIG", "HERMES_GATEWAY_CONFIG", "HERMES_RUNTIME_DIR", "HERMES_PYTHON", "HERMES_INSTALL_DIR", "CETIRUN_AUTH_KEY", "CETIRUN_AI_GATEWAY", "CETIRUN_INGRESS_MTLS", "PYTHONPATH", "PYTHONHOME", "NODE_PATH", "NODE_OPTIONS", "PATH", "HOME":
		return true
	}
	return false
}

func validateHermesOverrides(native map[string]any) error {
	if env, ok := native["env"].(map[string]any); ok {
		for name := range env {
			if hermesReservedEnvironment(name) {
				return fmt.Errorf("Hermes listener, authentication and runtime environment are platform-owned")
			}
		}
	}
	config, _ := native["config"].(map[string]any)
	if sources, ok := config["secrets"].(map[string]any); ok {
		for name, value := range sources {
			if name == "sources" || name == "preserve_existing" || name == "profile_alias" {
				continue
			}
			settings, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("Hermes secret source configuration must be an object")
			}
			if name != "onepassword" && settings["enabled"] != nil && settings["enabled"] != false {
				return fmt.Errorf("Hermes secret sources must declare their output names")
			}
			if name == "onepassword" {
				bindings, _ := settings["env"].(map[string]any)
				for variable := range bindings {
					if hermesReservedEnvironment(variable) || strings.HasPrefix(variable, "CETIRUN_") {
						return fmt.Errorf("Hermes secret sources must not replace platform environment")
					}
				}
				variable, _ := settings["service_account_token_env"].(string)
				if hermesReservedEnvironment(variable) || strings.HasPrefix(variable, "CETIRUN_") {
					return fmt.Errorf("Hermes secret sources must not consume platform credentials")
				}
			}
		}
	}
	for _, name := range []string{"webapp", "dashboard", "api_server", "dashboard_auth"} {
		if _, present := config[name]; present {
			return fmt.Errorf("Hermes listener and authentication configuration are platform-owned")
		}
	}
	if platforms, ok := config["platforms"].(map[string]any); ok {
		if _, present := platforms["api_server"]; present {
			return fmt.Errorf("Hermes API listener configuration is platform-owned")
		}
	}
	if gateway, ok := config["gateway"].(map[string]any); ok {
		if err := validateHermesOverrides(map[string]any{"config": gateway}); err != nil {
			return err
		}
	}
	return nil
}

func (r *AgentReconciler) prepareAgent(instance *api.Agent, team *api.Team) (*helm.PreparedRelease, error) {
	values, err := r.agentPreparationValues(instance, team)
	if err != nil {
		return nil, err
	}
	return r.HelmClient.Prepare(instanceNamespace(instance.Name), values, instance.Spec.Helm.Version)
}

func (r *AgentReconciler) prepareRuntimeAgent(ctx context.Context, instance *api.Agent, team *api.Team) (*helm.PreparedRelease, error) {
	values, err := r.agentPreparationValues(instance, team)
	if err != nil {
		return nil, err
	}
	if err := r.applyCetiAdminValues(ctx, instance, values); err != nil {
		return nil, err
	}
	return r.HelmClient.Prepare(instanceNamespace(instance.Name), values, instance.Spec.Helm.Version)
}

func (r *AgentReconciler) agentPreparationValues(instance *api.Agent, team *api.Team) (map[string]any, error) {
	values, err := r.builtinHelmValues(instance, team, AgentIdentityHelmConfig{})
	if err != nil {
		return nil, err
	}
	// Prepare only establishes the pinned chart contract. The first Gateway
	// projection is reconciled later in this pass, so Hermes still needs a
	// syntactically complete Gateway binding to render its native profile here.
	// The probe is never installed: the release is rebuilt from the verified
	// projection before any Sandbox or Helm apply.
	if r.AIGateway.Enabled {
		if r.aiGatewayRouted(instance) {
			if err := r.applyAIGatewayValues(values, instance); err != nil {
				return nil, err
			}
		} else if err := r.applyAIGatewayPreflightValues(values); err != nil {
			return nil, err
		}
	}
	values["memoryOptions"] = r.memoryOptions()
	return values, nil
}

func (r *AgentReconciler) applyCetiAdminValues(ctx context.Context, instance *api.Agent, values map[string]any) error {
	if r.CetiAdmin == nil {
		return nil
	}
	binding, managed, err := r.CetiAdmin.BindingForAgent(ctx, instance)
	if !managed {
		return nil
	}
	if err != nil {
		return err
	}
	projection, err := binding.RuntimeProjection(r.CetiAdmin.OperatorImageReference(), r.ConnectionsMCPAudience)
	if err != nil {
		return err
	}
	request, ok := values["agentRequest"].(map[string]any)
	if !ok {
		return fmt.Errorf("CetiAdmin requires a prepared Agent request")
	}
	request["cetiadmin"] = projection
	return applyCetiAdminNetwork(values, projection.Kubernetes)
}

// pinAgentDefaults changes only omitted fields. The reconciler writes this with
// resourceVersion and returns before acting on the new generation.
func pinAgentDefaults(spec *api.HarnessSpec, resolved agents.ResolvedAgent) bool {
	changed := false
	if spec.Image == nil && resolved.Image != "" {
		spec.Image = &resolved.Image
		changed = true
	}
	if spec.Port == nil && resolved.Port != nil {
		port := *resolved.Port
		spec.Port = &port
		changed = true
	}
	return changed
}

func (r *AgentReconciler) upgradeDefaultImage(ctx context.Context, instance *api.Agent) (bool, error) {
	if instance.Spec.Agent.Type == api.AgentTypeCustom || instance.Spec.Agent.Image == nil ||
		instance.Status.HelmReleaseName != helmReleaseName(instance.Name) || instance.Status.ObservedGeneration == instance.Generation {
		return false, nil
	}
	image, err := r.HelmClient.ResolveImageUpgrade(instanceNamespace(instance.Name), helmReleaseName(instance.Name),
		instance.Spec.Helm.Version, string(instance.Spec.Agent.Type), *instance.Spec.Agent.Image, r.ChartRegistry)
	if err != nil {
		_, err = r.preparationFailed(ctx, instance, err)
		return false, err
	}
	if image == *instance.Spec.Agent.Image {
		return false, nil
	}
	before := instance.DeepCopy()
	instance.Spec.Agent.Image = &image
	if err := r.Patch(ctx, instance, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return false, fmt.Errorf("upgrade default agent image: %w", err)
	}
	return true, nil
}
