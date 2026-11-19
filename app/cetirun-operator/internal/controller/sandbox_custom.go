// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/agents"
	customharness "github.com/Azure/CetiRun/app/cetirun-operator/internal/agents/custom"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/internal/terminalpolicy"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func sandboxRuntimeSecretName(agent *api.Agent) string {
	if agent.Spec.Agent.Type == api.AgentTypeCustom {
		return "custom-runtime"
	}
	return agent.Spec.GatewayTokenSecretRef
}

func sandboxGatewayProjectionReadyForLifecycle(agent *api.Agent) bool {
	return sandboxGatewayProjectionReady(agent) || agent.Spec.Suspended && agent.Status.AIGateway != nil &&
		agent.Status.Policy != nil && agent.Status.AIGateway.Phase == api.AIGatewayPhaseReady &&
		agent.Status.AIGateway.Blocked && agent.Status.AIGateway.Reason == aiGatewayReasonSuspended &&
		agent.Status.AIGateway.PolicyRevision == agent.Status.Policy.Enforcement.Gateway.DesiredRevision
}

func (r *AgentReconciler) ensureCommonCustomRuntimeAuthentication(ctx context.Context, agent *api.Agent, logger logr.Logger) error {
	state, receipt, err := r.readSandboxBootstrap(ctx, agent)
	if err != nil {
		return fmt.Errorf("read Custom bootstrap receipt: %w", err)
	}
	if agent.Spec.Agent.Type != api.AgentTypeCustom || !receipt.Reconciled || len(receipt.Objects) < 5 ||
		receipt.Objects[4].Kind != "Secret" || receipt.Objects[4].Name != sandboxRuntimeSecretName(agent) || receipt.Objects[4].UID == "" {
		return fmt.Errorf("Custom bootstrap receipt does not identify its runtime Secret")
	}
	desired := r.sandboxBootstrapObjects(agent, state, receipt)
	secret := &corev1.Secret{}
	if err := r.customReader().Get(ctx, client.ObjectKeyFromObject(desired[4]), secret); err != nil {
		return err
	}
	if err := validateSandboxBootstrapObject(secret, desired[4], receipt.Objects[4].UID, false); err != nil {
		return err
	}
	return r.ensureCetiRunAuthKey(ctx, logger, secret)
}

func (r *AgentReconciler) configureCustomSandboxInput(ctx context.Context, attempt *sandboxPreparationAttempt) error {
	agent := attempt.agent
	if !attempt.reconciled || !r.customGatewaySelected(agent) || agent.Spec.Agent.Image == nil {
		return fmt.Errorf("Custom Sandbox execution requires an explicitly Gateway-qualified image and common execution")
	}
	request := agents.Request{Type: "custom", Image: *agent.Spec.Agent.Image, Port: agent.Spec.Agent.Port, WebUI: agent.Spec.Agent.WebUI}
	if agent.Spec.Agent.Username != nil {
		request.Username = *agent.Spec.Agent.Username
	}
	resolved, err := agents.ResolveCustom(request)
	if err != nil {
		return err
	}
	baseURL := strings.TrimSuffix(r.AIGateway.URL, "/") + "/v1"
	contribution, err := (customharness.Harness{}).Build(resolved, agents.Bindings{
		ContextRevision: attempt.prerequisites.Revision,
		Model: agents.ModelBinding{BaseURL: baseURL, CredentialSlot: "model",
			Revision: string(attempt.prerequisites.RuntimeSecret.UID)},
		WebBasePath: "/ui/" + agent.Name + "/", WebInstance: string(agent.UID),
	})
	if err != nil {
		return err
	}
	custom := &w.CustomAgent{
		Image: resolved.Image, Username: resolved.Username, Home: resolved.HomeDir, Port: resolved.Port, WebUI: resolved.WebUI,
		WebBasePath: "/ui/" + agent.Name + "/", WebInstance: string(agent.UID),
	}
	if len(attempt.resolution.AgentPolicy.Network.BlockedFQDNs) != 0 {
		provider, ok := r.CustomBindings.(interface {
			ImageUser(*api.Agent) (CustomImageUser, error)
		})
		if !ok {
			return fmt.Errorf("Custom FQDN capture requires a qualified image user")
		}
		user, err := provider.ImageUser(agent)
		if err != nil {
			return err
		}
		custom.RunAsUser, custom.RunAsGroup = ptr.To(user.UID), ptr.To(user.GID)
	}
	for _, value := range contribution.Environment {
		custom.Environment = append(custom.Environment, w.CustomEnvironment{
			Name: value.Name, Value: value.Value, CredentialSlot: value.CredentialSlot,
		})
	}
	for _, value := range contribution.Ports {
		custom.Ports = append(custom.Ports, w.CustomPort{Name: value.Name, Number: value.Number})
	}
	if h := contribution.HTTP; h != nil {
		custom.HTTP = &w.CustomHTTP{PortName: h.PortName, HealthPath: h.HealthPath, ReadinessPath: h.ReadinessPath,
			StartupPath: h.StartupPath, ChatPath: h.ChatPath, ModelsPath: h.ModelsPath}
	}
	attempt.input.CustomAgent, attempt.input.GatewayAPIBase = custom, baseURL
	snapshot, err := r.ensureCustomSandboxContext(ctx, agent, attempt.team, attempt.input.Namespace.Name)
	if err != nil {
		return err
	}
	attempt.input.Context = &w.Context{ObjectIdentity: w.ObjectIdentity{Name: snapshot.Name, UID: snapshot.UID},
		Files: map[string]string{"AGENTS.md": "AGENTS.md"}}
	attempt.sources = append(attempt.sources, sandboxSource(snapshot, "v1", "ConfigMap"))
	platform, user, err := r.sandboxHelmValues(agent, attempt.team)
	if err != nil {
		return err
	}
	attempt.input.Terminal, err = customSandboxTerminalInput(platform, user, attempt.input.WorkloadName)
	if err != nil {
		return err
	}
	if custom.HTTP != nil || len(attempt.resolution.AgentPolicy.Network.BlockedFQDNs) != 0 {
		image, err := customSandboxHelperImage(platform)
		if err != nil {
			return err
		}
		attempt.input.LoopbackProbe = &w.LoopbackProbe{Image: image}
	}
	return nil
}

func (r *AgentReconciler) ensureCustomSandboxContext(ctx context.Context, agent *api.Agent, team *api.Team, namespace string) (*corev1.ConfigMap, error) {
	if team == nil {
		return nil, fmt.Errorf("Custom Sandbox requires a resolved Team context")
	}
	data := map[string]string{"AGENTS.md": "# CetiRun Team Context\n\n## Role\n" + team.Spec.MemberContext.Role +
		"\n\n## Context\n" + strings.Join(team.Spec.MemberContext.Description, "\n") + "\n"}
	raw, err := json.Marshal(struct {
		AgentUID string            `json:"agentUID"`
		TeamUID  string            `json:"teamUID"`
		Files    map[string]string `json:"files"`
	}{string(agent.UID), string(team.UID), data})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	name := "custom-context-" + hex.EncodeToString(sum[:16])
	owner := []metav1.OwnerReference{w.AgentOwner(agent.Name, agent.UID)}
	snapshot := &corev1.ConfigMap{}
	err = r.customReader().Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, snapshot)
	if apierrors.IsNotFound(err) {
		snapshot = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, OwnerReferences: owner},
			Immutable: ptr.To(true), Data: data}
		if err := r.Create(ctx, snapshot); err != nil {
			return nil, err
		}
		if err := r.customReader().Get(ctx, client.ObjectKeyFromObject(snapshot), snapshot); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if snapshot.UID == "" || snapshot.ResourceVersion == "" || !snapshot.DeletionTimestamp.IsZero() ||
		!ptr.Deref(snapshot.Immutable, false) || !equality.Semantic.DeepEqual(snapshot.OwnerReferences, owner) ||
		!equality.Semantic.DeepEqual(snapshot.Data, data) || len(snapshot.BinaryData) != 0 {
		return nil, fmt.Errorf("Custom context snapshot has changed or has foreign ownership")
	}
	return snapshot, nil
}

func customSandboxHelperImage(platform map[string]any) (string, error) {
	settings, _ := platform["ttyd"].(map[string]any)
	image, _ := settings["image"].(map[string]any)
	repository, _ := image["repository"].(string)
	if global, ok := platform["global"].(map[string]any); ok {
		if registry, ok := global["registry"].(string); ok && strings.TrimSuffix(registry, "/") != "" {
			repository = strings.TrimSuffix(registry, "/") + "/cetirun-terminal"
		}
	}
	digest, _ := image["digest"].(string)
	if repository == "" || len(digest) != len("sha256:")+64 || !strings.HasPrefix(digest, "sha256:") {
		return "", fmt.Errorf("Custom terminal and HTTP probes require an immutable platform terminal image")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(digest, "sha256:")); err != nil {
		return "", fmt.Errorf("Custom platform terminal image digest is invalid")
	}
	return repository + "@" + digest, nil
}

func customSandboxTerminalInput(platform, user map[string]any, workload string) (*w.Terminal, error) {
	settings, _ := platform["ttyd"].(map[string]any)
	requested, _ := user["ttyd"].(map[string]any)
	if settings["enabled"] == false || requested["enabled"] == false {
		return nil, nil
	}
	image, err := customSandboxHelperImage(platform)
	if err != nil {
		return nil, err
	}
	port, err := customSandboxNumber(settings["port"], 7681, 1024, 65535)
	if err != nil {
		return nil, fmt.Errorf("Custom terminal port: %w", err)
	}
	session, err := customSandboxNumber(settings["sessionSeconds"], 1800, 60, 3600)
	if err != nil {
		return nil, fmt.Errorf("Custom terminal session: %w", err)
	}
	return &w.Terminal{Image: image, Port: port, SessionSeconds: session, ServiceAccountName: terminalpolicy.WorkloadName(workload)}, nil
}

func customSandboxNumber(value any, defaultValue, minimum, maximum int32) (int32, error) {
	if value == nil {
		return defaultValue, nil
	}
	var number float64
	switch value := value.(type) {
	case int:
		number = float64(value)
	case int32:
		number = float64(value)
	case int64:
		number = float64(value)
	case float64:
		number = value
	default:
		return 0, fmt.Errorf("must be an integer between %d and %d", minimum, maximum)
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || number < float64(minimum) || number > float64(maximum) || math.Trunc(number) != number {
		return 0, fmt.Errorf("must be an integer between %d and %d", minimum, maximum)
	}
	return int32(number), nil
}

func (r *AgentReconciler) customSandboxIngress(agent *api.Agent) *sandboxpolicy.CustomIngress {
	if agent.Spec.Agent.Port == nil {
		return nil
	}
	ingress := &sandboxpolicy.CustomIngress{Port: *agent.Spec.Agent.Port, WebUI: agent.Spec.Agent.WebUI}
	if !ingress.WebUI && r.ChatAccess.Enabled {
		for _, caller := range r.ChatAccess.Callers {
			ingress.ChatCallers = append(ingress.ChatCallers, sandboxpolicy.IngressCaller{Namespace: caller.Namespace, PodLabels: caller.PodLabels})
		}
	}
	if !ingress.WebUI && len(ingress.ChatCallers) == 0 {
		return nil
	}
	return ingress
}

func terminalSandboxIngress(terminal *w.Terminal) *sandboxpolicy.TerminalIngress {
	if terminal == nil {
		return nil
	}
	return &sandboxpolicy.TerminalIngress{Port: terminal.Port}
}
