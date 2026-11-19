// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	istioapiv1beta1 "istio.io/api/networking/v1beta1"
	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/agents/builtin"
	"github.com/Azure/CetiRun/app/internal/terminalpolicy"
)

const (
	// DefaultIstioGateway is the Istio Gateway resource that handles external traffic.
	DefaultIstioGateway = "aks-istio-ingress/cetirun-gateway"

	// openclawPort is the port the OpenClaw Gateway listens on.
	openclawPort = 18789

	// hermesAPIPort is the port the Hermes OpenAI-compatible API listens on.
	hermesAPIPort = 8642

	// hermesWebappPort is the port the native Hermes Webapp listens on.
	hermesWebappPort = 9120

	// ttydPort is the port ttyd web terminal listens on.
	ttydPort = 7681

	// novncPort is the port noVNC/websockify listens on.
	novncPort = 6080

	// Override via spec.helm.values.persistence.storageClass.
	defaultStorageClass = "default"
)

// instanceNamespace returns the Kubernetes namespace for a given alias.
func instanceNamespace(alias string) string {
	return fmt.Sprintf("cetirun-%s", alias)
}

// helmReleaseName returns the Helm release name for a given alias.
// TODO: rename to "cetirun-<alias>" once existing instances are migrated.
// Changing the release name requires migration logic (uninstall old release,
// rebind PVC, install new release) since Helm tracks releases by name.
func helmReleaseName(alias string) string {
	return fmt.Sprintf("openclaw-%s", alias)
}

// instanceServiceName returns the Kubernetes Service name created by the Helm chart.
// The chart fullname template is: if release contains chart name → release name,
// else → "<release>-<chart>". Release is "openclaw-<alias>", chart name is "cetirun"
// (they have no contains relationship), so fullname = "openclaw-<alias>-cetirun".
// TODO: will become "cetirun-<alias>" after helmReleaseName migration (since
// "cetirun-<alias>" contains "cetirun", fullname collapses to release name).
func instanceServiceName(alias string) string {
	name := fmt.Sprintf("openclaw-%s-cetirun", alias)
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.TrimRight(name, "-")
}

// accessURL returns the HTTPS URL for the given instance alias.
func accessURL(alias, domainSuffix string) string {
	return fmt.Sprintf("https://%s.%s", alias, domainSuffix)
}

// externalHostname returns the external hostname for the given alias.
func externalHostname(alias, domainSuffix string) string {
	return fmt.Sprintf("%s.%s", alias, domainSuffix)
}

// instanceKey returns the primary key used to derive all resource names for an instance.
// It is the instanceName from the CRD spec.
func instanceKey(instanceName string) string {
	return instanceName
}

// accessURLPathBased returns the canonical native UI URL for an instance.
// e.g. "https://cetirun.example.com/ui/alice-work/"
func accessURLPathBased(instanceName, domainSuffix string) string {
	return fmt.Sprintf("https://%s/ui/%s/", domainSuffix, instanceName)
}

// virtualServiceName returns the Istio VirtualService name for a given alias.
func virtualServiceName(alias string) string {
	return fmt.Sprintf("cetirun-%s", alias)
}

// operatorConfigHash returns a short hex hash of operator-level config fields
// that should trigger a Helm upgrade when changed. This includes the domain
// suffix, a hash of the gateway Secret contents, the skills storage account
// override, the chart registry, and operator-level helm values.
// Chart version is intentionally excluded: version changes are tracked via
// spec.helm.version (which bumps the CR generation).
func operatorConfigHash(domainSuffix, secretHash, skillsStorageAccount, cetirunHelmValuesHash, chartRegistry string, config ...string) string {
	payload := domainSuffix + "\x00" + secretHash
	if skillsStorageAccount != "" || cetirunHelmValuesHash != "" || chartRegistry != "" || len(config) > 0 {
		payload += "\x00" + skillsStorageAccount + "\x00" + cetirunHelmValuesHash + "\x00" + chartRegistry
		for _, v := range config {
			payload += "\x00" + v
		}
	}
	h := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(h[:8]) // 16-char prefix is plenty for this purpose
}

const agentIdentityRuntimeRevision = "native-mcp-v1"

func (r *AgentReconciler) configHash(instance *cetirunv1alpha1.Agent, team *cetirunv1alpha1.Team, gwSecretHash, teamRV string) string {
	config := []string{}
	if instance.Spec.Agent.Type == cetirunv1alpha1.AgentTypeHermes {
		harness, _ := builtin.NewRegistry().Lookup("hermes")
		profile := harness.Describe()
		config = append(config, "hermes-webapp/v1", profile.NativeVersion, profile.NativeCommit,
			strconv.Itoa(int(profile.DefaultPort)), strconv.Itoa(int(profile.WebappPort)))
	}
	if enabled, err := r.connectionsMCPEnabledFor(instance); err != nil {
		config = append(config, "invalid-connections-mcp-runtime-types", strings.Join(r.ConnectionsMCPRuntimeTypes, ","))
	} else if enabled {
		runtimeTypes, _ := normalizeConnectionsMCPRuntimeTypes(r.ConnectionsMCPRuntimeTypes)
		config = append(config, r.ConnectionsMCPURL, r.ConnectionsMCPAudience, r.ConnectionsMCPCASecretName,
			string(instance.Spec.Agent.Type), strings.Join(runtimeTypes, ","), agentIdentityRuntimeRevision)
	}
	// Avoid rolling unaffected workloads when Agent Identity remains disabled.
	if r.AgentIdentityEnabled || agentIdentityRequested(instance) || instance.Status.AgentIdentity != nil {
		config = append(config, r.AgentIdentityTenantID, r.AgentIdentityBlueprintClientID,
			r.AgentIdentityNamePrefix, r.AgentIdentityBrokerURL)
	}
	if agentIdentityRequested(instance) || instance.Status.AgentIdentity != nil {
		config = append(config, agentIdentityRuntimeRevision)
	}
	// A configured default affects instances without a per-instance model.
	// Include it only for those instances so an explicit selection does not
	// restart when the deployment default changes.
	if defaultModel := strings.TrimSpace(r.DefaultModel); defaultModel != "" && r.usesDeploymentDefaultModel(instance, team) {
		config = append(config, defaultModel)
	}
	if revision := r.aiGatewayRevision(instance); revision != "" {
		config = append(config, revision)
	}
	config = append(config, teamRV)
	if r.MemoryEnabled || r.MemoryImage != "" {
		config = append(config, "memory-context/v2", r.MemoryImage, r.MemoryRegistry)
	}
	// Older suspended-state caches must not skip the retention repair.
	if instance.Spec.Suspended && !sandboxRequested(instance, team) &&
		(instance.Spec.Agent.Type == cetirunv1alpha1.AgentTypeOpenClaw || instance.Spec.Agent.Type == cetirunv1alpha1.AgentTypeHermes) {
		config = append(config, "suspended-home-retention/v1")
	}
	// Bind both steady-state and Helm-apply caches to requested restart intent.
	if instance.Spec.RestartRequestID != "" {
		config = append(config, "restart-request/v1", instance.Spec.RestartRequestID)
	}
	if request := instance.Spec.PlannedMigration; request != nil {
		config = append(config, "planned-migration/v1", request.RequestID, request.Target.Name, string(request.Target.UID))
	}
	return operatorConfigHash(
		r.DomainSuffix, gwSecretHash, r.SkillsStorageAccount,
		hashCetiRunHelmValues(r.CetiRunHelmValues), r.ChartRegistry,
		config...,
	)
}

// usesDeploymentDefaultModel reports whether buildHelmValues will render the
// operator's default model when no per-Agent selection is present.
func (r *AgentReconciler) usesDeploymentDefaultModel(instance *cetirunv1alpha1.Agent, _ *cetirunv1alpha1.Team) bool {
	return strings.TrimSpace(instance.Spec.Model) == ""
}

// hashCetiRunHelmValues returns a stable hash string of the operator-level helm values map.
// Returns empty string for nil/empty maps.
func hashCetiRunHelmValues(values map[string]interface{}) string {
	if len(values) == 0 {
		return ""
	}
	data, err := json.Marshal(values)
	if err != nil {
		return "error"
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:8])
}

// secretContentHash returns a short hex hash of the Secret's Data map contents.
func secretContentHash(data map[string][]byte) string {
	if len(data) == 0 {
		return ""
	}
	h := sha256.New()
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write(data[k])
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// generateRandomToken generates a cryptographically random token of the given byte length,
// returned as a base64-encoded string.
func generateRandomToken(byteLen int) (string, error) {
	buf := make([]byte, byteLen)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate random token: %w", err)
	}
	return base64.StdEncoding.EncodeToString(buf), nil
}

// buildHelmValues constructs the Helm values map for installing/upgrading the openclaw chart.
// The optional final argument is the operator deployment default model. An
// Agent's spec.model takes precedence. The variadic form keeps the helper
// convenient for unit tests and callers that do not need a default.
// Merge order (later wins): controller defaults → cetirunHelmValues → team
// context/skills → per-instance spec.helm.values → controller-enforced model.
type AgentIdentityHelmConfig struct {
	Enabled                    bool
	BrokerURL                  string
	ConnectionsMCPURL          string
	ConnectionsMCPAudience     string
	ConnectionsMCPCASecretName string
	ConnectionsMCPRuntimeTypes []string
}

func buildHelmValues(instance *cetirunv1alpha1.Agent, team *cetirunv1alpha1.Team, domainSuffix string, skillsStorageAccount string, cetirunHelmValues map[string]interface{}, chartRegistry string, defaultModels ...string) (map[string]interface{}, error) {
	return buildHelmValuesCore(instance, team, domainSuffix, skillsStorageAccount, cetirunHelmValues, chartRegistry, AgentIdentityHelmConfig{}, false, defaultModels...)
}

func buildHelmValuesWithAgentIdentity(instance *cetirunv1alpha1.Agent, team *cetirunv1alpha1.Team, domainSuffix string, skillsStorageAccount string, cetirunHelmValues map[string]interface{}, chartRegistry, defaultModel string, agentIdentityConfig AgentIdentityHelmConfig) (map[string]interface{}, error) {
	return buildHelmValuesCore(instance, team, domainSuffix, skillsStorageAccount, cetirunHelmValues, chartRegistry, agentIdentityConfig, false, defaultModel)
}

// builtinHelmValues builds a built-in Agent's values from the reconciler's
// deployment configuration, including the Skill Marketplace switch.
// buildHelmValues and buildHelmValuesWithAgentIdentity build them without the
// marketplace.
func (r *AgentReconciler) builtinHelmValues(instance *cetirunv1alpha1.Agent, team *cetirunv1alpha1.Team, agentIdentityConfig AgentIdentityHelmConfig) (map[string]interface{}, error) {
	if err := skillsWithoutMarketplace(team, instance.Spec.SkillOverrides, r.SkillMarketplace); err != nil {
		return nil, err
	}
	return buildHelmValuesCore(instance, team, r.DomainSuffix, r.SkillsStorageAccount, r.CetiRunHelmValues, r.ChartRegistry,
		agentIdentityConfig, r.SkillMarketplace, r.DefaultModel)
}

func buildHelmValuesCore(instance *cetirunv1alpha1.Agent, team *cetirunv1alpha1.Team, domainSuffix string, skillsStorageAccount string, cetirunHelmValues map[string]interface{}, chartRegistry string, agentIdentityConfig AgentIdentityHelmConfig, skillMarketplace bool, defaultModels ...string) (map[string]interface{}, error) {
	if instance.Spec.Agent.Type == cetirunv1alpha1.AgentTypeCustom {
		return nil, fmt.Errorf("Custom requires its own platform binding and runtime plan")
	}
	if err := validateAgentRuntime(instance.Spec.Agent); err != nil {
		return nil, err
	}
	if err := validateRuntimeOverrides(cetirunHelmValues, string(instance.Spec.Agent.Type)); err != nil {
		return nil, err
	}
	alias := instance.Spec.User.Alias
	spec := instance.Spec
	agentType := spec.Agent.Type
	userTeam := spec.Team
	if !sandboxRequested(instance, team) && spec.User.Team != "" {
		userTeam = spec.User.Team
	}

	values := map[string]interface{}{
		"agentType": string(agentType),
		// Keep runtime/UI credentials separate from the AI Gateway model key.
		// The controller projects the Gateway endpoint and key before apply.
		"secrets": map[string]interface{}{
			"existingSecret": spec.GatewayTokenSecretRef,
			"provider":       "openai",
			"model":          "",
		},
		// User identity — templates USER.md in the ConfigMap.
		"user": map[string]interface{}{
			"alias":       alias,
			"displayName": spec.User.DisplayName,
			"team":        userTeam,
			"baseDomain":  domainSuffix,
		},
		// User identification via extra env vars.
		// Must be []interface{} (not []map[string]interface{}) so that Helm's JSON
		// schema validator accepts the value for array-typed fields.
		"extraEnv": []interface{}{
			map[string]interface{}{"name": "CETIRUN_USER_ALIAS", "value": alias},
			map[string]interface{}{"name": "CETIRUN_USER_DISPLAY_NAME", "value": spec.User.DisplayName},
			map[string]interface{}{"name": "CETIRUN_USER_TEAM", "value": userTeam},
		},
		// Persistent home volume.
		"persistence": map[string]interface{}{
			"storageClass": defaultStorageClass,
			"accessMode":   "ReadWriteOnce",
			"size":         "100Gi",
		},
	}
	for key, value := range platformRuntimeDefaults(instance.Name) {
		values[key] = value
	}

	// Operator-level Helm value overrides (from CETIRUN_HELM_VALUES).
	// Merged after hardcoded defaults but before controller-enforced values
	// (suspended, tailscale, teams) so that the controller always wins.
	for k, v := range cetirunHelmValues {
		values[k] = deepMerge(values[k], v)
	}

	// Apply Team defaults — higher priority than cetirunHelmValues
	// (deepMerge is called with team values as the override side, so they win
	// on key collisions), lower priority than per-instance spec.helm.values
	// which is applied at the bottom of this function.
	if team != nil {

		userPatch := map[string]interface{}{}
		if team.Spec.MemberContext.Role != "" {
			userPatch["role"] = team.Spec.MemberContext.Role
		}
		if len(team.Spec.MemberContext.Description) > 0 {
			userPatch["description"] = stringsToInterfaces(team.Spec.MemberContext.Description)
		}
		if len(userPatch) > 0 {
			values["user"] = deepMerge(values["user"], userPatch)
		}
	}

	if err := applySkillValues(values, team, instance.Spec.SkillOverrides, skillsStorageAccount, skillMarketplace); err != nil {
		return nil, err
	}

	// When Tailscale is enabled, turn on tailscale serve by default so that the
	// gateway is accessible via HTTPS on the tailnet. Gateway token and device
	// authentication still apply.
	if spec.Tailscale != nil {
		values["tailscale"] = map[string]interface{}{
			"serve": true,
		}
	}

	// Override image repositories to match the operator's chart registry.
	// This ensures both openclaw-gateway and cetirun-desktop images are
	// pulled from the same ACR that hosts the Helm chart.
	if chartRegistry != "" {
		values["openclaw"] = deepMerge(values["openclaw"], map[string]interface{}{
			"image": map[string]interface{}{
				"repository": chartRegistry + "/openclaw-gateway",
			},
		})
		values["hermes"] = deepMerge(values["hermes"], map[string]interface{}{
			"image": map[string]interface{}{
				"repository": chartRegistry + "/hermes-gateway",
			},
		})
		values["desktop"] = deepMerge(values["desktop"], map[string]interface{}{
			"image": map[string]interface{}{
				"repository": chartRegistry + "/cetirun-desktop",
			},
		})
	}

	// User-supplied Helm value overrides. Each key in spec.helm.values is a top-level
	// Helm value name; its JSON value is deep-merged into the operator defaults so that
	// nested keys (e.g. persistence.storageClass) can be overridden without silently
	// dropping sibling defaults (e.g. persistence.size). Slices are replaced entirely,
	// matching standard Helm --values merge semantics.
	for k, rawExt := range spec.Helm.Values {
		if rawExt.Raw == nil {
			continue
		}
		var v interface{}
		if err := json.Unmarshal(rawExt.Raw, &v); err != nil {
			return nil, fmt.Errorf("unmarshal helm.values[%s]: %w", k, err)
		}
		if k == "agentType" && v != string(agentType) {
			return nil, fmt.Errorf("helm.values.agentType must not override spec.agent.type")
		}
		if err := validateRuntimeOverrides(map[string]any{k: v}, string(agentType)); err != nil {
			return nil, err
		}
		values[k] = deepMerge(values[k], v)
	}
	if spec.Suspended {
		persistence, ok := values["persistence"].(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("helm values persistence must be an object")
		}
		if retention, present := persistence["retainPolicy"]; present && retention != "Retain" && retention != "Delete" {
			return nil, fmt.Errorf("helm values persistence.retainPolicy must be Retain or Delete")
		}
		if err := validateSuspendedHome(values); err != nil {
			return nil, err
		}
		// A retained stop must win over every override, without changing the
		// stored retention preference that will apply again after resume.
		values["replicaCount"] = 0
		values["persistence"] = deepMerge(persistence, map[string]interface{}{"retainPolicy": "Retain"})
	}
	// Deployment-level chart defaults cannot select a different integration.
	values["agentType"] = string(agentType)

	// The native OpenClaw Control UI must know the externally visible UI
	// prefix so it emits prefixed asset, API, media, and WebSocket URLs. This is
	// platform routing and must not be overridden by user Helm values.
	h, _ := builtin.NewRegistry().Lookup(string(agentType))
	if h.Describe().PreserveUIPrefix {
		values[string(agentType)] = deepMerge(values[string(agentType)], map[string]interface{}{
			"config": map[string]interface{}{
				"gateway": map[string]interface{}{
					"controlUi": map[string]interface{}{
						"basePath": "/ui/" + instance.Name,
					},
				},
			},
		})
	}
	if agentType == cetirunv1alpha1.AgentTypeHermes && domainSuffix != "" {
		values["hermes"] = deepMerge(values["hermes"], map[string]interface{}{
			"env": map[string]interface{}{
				"HERMES_WEBAPP_PUBLIC_URL": fmt.Sprintf("https://%s/ui/%s", domainSuffix, instance.Name),
				"API_SERVER_CORS_ORIGINS":  hermesCORSOrigins(domainSuffix),
			},
		})
	}

	// The AI Gateway projection validates the selected model against the
	// Agent grant before execution.
	secrets, ok := values["secrets"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("helm values secrets must be an object")
	}
	provider, _ := secrets["provider"].(string)
	provider = strings.TrimSpace(provider)
	if provider == "" {
		provider = "openai"
	}
	if provider == "openai" {
		model := strings.TrimSpace(spec.Model)
		if model == "" && len(defaultModels) > 0 {
			model = strings.TrimSpace(defaultModels[0])
		}
		if model != "" {
			values["secrets"] = deepMerge(secrets, map[string]interface{}{
				"model": model,
			})
		}
	}

	values["agentRequest"] = runtimeRequest(instance, chartRegistry)

	// Agent Identity values are controller-owned and applied after arbitrary
	// per-instance Helm overrides. The workload cannot select a Broker endpoint,
	// service account, Kubernetes role, or default token mount.
	values["agentIdentity"] = map[string]interface{}{"enabled": false}
	values["connectionsMCP"] = map[string]interface{}{"enabled": false, "url": "", "audience": "cetirun-connections-mcp", "namespace": "cetirun-tools"}
	if agentIdentityRequested(instance) && agentIdentityConfig.Enabled &&
		instance.Status.AgentIdentity != nil &&
		instance.Status.AgentIdentity.Phase == cetirunv1alpha1.AgentIdentityPhaseReady {
		values["agentIdentity"] = map[string]interface{}{
			"enabled": true,
		}
		values["serviceAccount"] = deepMerge(values["serviceAccount"], map[string]interface{}{
			"create":         true,
			"name":           instanceServiceName(instance.Name),
			"role":           "",
			"automountToken": false,
		})
	}
	connectionsMCPEnabled, err := connectionsMCPRuntimeAllowed(agentType, agentIdentityConfig.ConnectionsMCPRuntimeTypes)
	if err != nil {
		return nil, err
	}
	if agentIdentityConfig.ConnectionsMCPURL != "" && connectionsMCPEnabled && !sandboxRequested(instance, team) {
		namespace, err := connectionsMCPNamespace(agentIdentityConfig.ConnectionsMCPURL)
		if err != nil {
			return nil, err
		}
		audience := agentIdentityConfig.ConnectionsMCPAudience
		if audience == "" {
			audience = "cetirun-connections-mcp"
		}
		if agentIdentityConfig.ConnectionsMCPCASecretName == "" {
			return nil, fmt.Errorf("Connections MCP requires a workload-local CA trust Secret")
		}
		values["connectionsMCP"] = map[string]interface{}{"enabled": true, "url": agentIdentityConfig.ConnectionsMCPURL, "audience": audience, "namespace": namespace, "caSecretName": agentIdentityConfig.ConnectionsMCPCASecretName}
		values["serviceAccount"] = deepMerge(values["serviceAccount"], map[string]interface{}{"create": true, "name": instanceServiceName(instance.Name), "role": "", "automountToken": false})
	}
	if spec.RestartRequestID != "" {
		if !cetirunv1alpha1.ValidRestartRequestID(spec.RestartRequestID) {
			return nil, fmt.Errorf("invalid Agent restart request ID")
		}
		values["podAnnotations"] = deepMerge(values["podAnnotations"], map[string]any{
			cetirunv1alpha1.AnnotationRestartRequestID: spec.RestartRequestID,
		})
	}
	return values, nil
}

func validateSuspendedHome(values map[string]any) error {
	persistence, ok := values["persistence"].(map[string]any)
	if !ok {
		return fmt.Errorf("retained suspension requires a valid persistence configuration")
	}
	if enabled, explicit := persistence["enabled"].(bool); explicit && !enabled {
		if claim, _ := persistence["existingClaim"].(string); claim == "" {
			return fmt.Errorf("retained suspension cannot scale down ephemeral HOME; persistent storage is required")
		}
	}
	return nil
}

func connectionsMCPNamespace(raw string) (string, error) {
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Scheme != "https" || endpoint.Path != "/mcp" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return "", fmt.Errorf("Connections MCP URL must be a fixed in-cluster HTTPS /mcp endpoint")
	}
	parts := strings.Split(endpoint.Hostname(), ".")
	if len(parts) != 5 || parts[0] == "" || parts[1] == "" || parts[2] != "svc" || parts[3] != "cluster" || parts[4] != "local" {
		return "", fmt.Errorf("Connections MCP URL must use service.namespace.svc.cluster.local")
	}
	return parts[1], nil
}

func agentIdentityBrokerNamespace(raw string) (string, error) {
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Scheme != "http" || endpoint.Port() != "8080" ||
		endpoint.User != nil || endpoint.Path != "" || endpoint.RawQuery != "" ||
		endpoint.Fragment != "" {
		return "", fmt.Errorf("Agent Identity Broker URL must be an in-cluster HTTP service URL on port 8080")
	}
	parts := strings.Split(endpoint.Hostname(), ".")
	if len(parts) != 5 || parts[0] == "" || parts[1] == "" ||
		parts[2] != "svc" || parts[3] != "cluster" || parts[4] != "local" {
		return "", fmt.Errorf("Agent Identity Broker URL must use service.namespace.svc.cluster.local")
	}
	return parts[1], nil
}

// aiGatewayRoutedStatus reports whether the Agent's recorded routing sends
// model traffic through the AI Gateway. Such an Agent carries no in-agent
// sign-in helper, so its ingress has no route to one.
func aiGatewayRoutedStatus(instance *cetirunv1alpha1.Agent) bool {
	return instance.Status.AIGateway != nil && instance.Status.AIGateway.Routed
}

func platformRuntimeDefaults(name string) map[string]any {
	return map[string]any{
		"networkPolicy": map[string]any{
			"istioNamespace": "aks-istio-system", "istioIngressNamespace": "aks-istio-ingress",
		},
		"desktop": map[string]any{"novncPathPrefix": name + "/desktop/"},
	}
}

// deepMerge recursively merges override into base.
// - If both values are map[string]interface{}, keys are merged recursively.
// - For all other types (including slices), override replaces base entirely.
// This matches standard Helm --values merge semantics.
func deepMerge(base, override interface{}) interface{} {
	baseMap, baseOk := base.(map[string]interface{})
	overrideMap, overrideOk := override.(map[string]interface{})
	if !baseOk || !overrideOk {
		return override
	}
	merged := make(map[string]interface{}, len(baseMap))
	for k, v := range baseMap {
		merged[k] = v
	}
	for k, v := range overrideMap {
		if existing, ok := merged[k]; ok {
			merged[k] = deepMerge(existing, v)
		} else {
			merged[k] = v
		}
	}
	return merged
}

// normalizedTeamRuntimeInputs returns the Team fields that can affect an
// Agent's rendered runtime configuration. Compatibility is a short-lived
// diagnostic report, so its status refresh must neither requeue dependent
// Agents nor invalidate their Helm cache. Kubernetes transport bookkeeping is
// also excluded. All other metadata, spec, and status remain meaningful.
func normalizedTeamRuntimeInputs(team *cetirunv1alpha1.Team) *cetirunv1alpha1.Team {
	if team == nil {
		return nil
	}
	normalized := team.DeepCopy()
	normalized.TypeMeta = metav1.TypeMeta{}
	normalized.ResourceVersion = ""
	normalized.ManagedFields = nil
	normalized.Status.Compatibility = nil
	return normalized
}

// teamRuntimeInputFingerprint is a stable cache-invalidation token for the
// Team fields that can affect an Agent without changing the Agent generation.
func teamRuntimeInputFingerprint(team *cetirunv1alpha1.Team) string {
	normalized := normalizedTeamRuntimeInputs(team)
	if normalized == nil {
		return ""
	}
	data, err := json.Marshal(normalized)
	if err != nil {
		// Team's API fields are JSON serializable. Keep a conservative fallback
		// if that contract is ever violated: a transport update must still miss
		// the cache rather than sharing a constant invalid fingerprint.
		return "invalid-team-runtime-inputs:" + string(team.UID) + ":" + team.ResourceVersion
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:8])
}

// stringsToInterfaces converts a []string to []interface{} so that values
// passed to Helm's schema validator are treated as untyped JSON arrays.
// The Helm v3 SDK schema validator rejects strongly typed Go slices like
// []string with "invalid jsonType" errors; values.yaml-loaded arrays are
// always []interface{}, so this restores that shape.
func stringsToInterfaces(in []string) []interface{} {
	out := make([]interface{}, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

// computeEffectiveSkills returns (base ∪ add) \ remove as a sorted, deduplicated slice.
// team may be nil (treated as empty base).
func computeEffectiveSkills(team *cetirunv1alpha1.Team, add, remove []string) []string {
	set := map[string]struct{}{}
	if team != nil {
		for _, s := range team.Spec.Skills {
			set[s] = struct{}{}
		}
	}
	for _, s := range add {
		set[s] = struct{}{}
	}
	for _, s := range remove {
		delete(set, s)
	}
	result := make([]string, 0, len(set))
	for s := range set {
		result = append(result, s)
	}
	sort.Strings(result)
	return result
}

// classifySkills splits skills by prefix into npm and clawHub lists.
// npm:PKG → PKG (prefix stripped)
// other   → clawHub as-is
// "pvc:" prefix is rejected.
func classifySkills(skills []string) (npm []string, clawHub []string, err error) {
	for _, s := range skills {
		switch {
		case strings.HasPrefix(s, "pvc:"):
			return nil, nil, fmt.Errorf("invalid skill %q: pvc: prefix is not supported", s)
		case strings.HasPrefix(s, "npm:"):
			pkg := strings.TrimPrefix(s, "npm:")
			if pkg == "" {
				return nil, nil, fmt.Errorf("invalid skill %q: npm: prefix requires a package name", s)
			}
			npm = append(npm, pkg)
		default:
			clawHub = append(clawHub, s)
		}
	}
	return
}

// buildVirtualService builds the Istio VirtualService
// It exposes native agent UIs below /ui/<instanceName>. OpenClaw's native Control UI
// retains that UI prefix through its gateway.controlUi.basePath setting;
// the Terminal, Desktop, Hermes, and in-pod auth routes use their own rewrites.
//
// The instance name is not used as a top-level native UI route, which keeps it isolated
// from Portal SPA routes such as /instances/<name>.
func buildVirtualService(instance *cetirunv1alpha1.Agent, domainSuffix, gateway string, access ...customAccess) *istiov1beta1.VirtualService {
	if gateway == "" {
		gateway = DefaultIstioGateway
	}
	name := instance.Name
	ns := instanceNamespace(name)
	svcHost := fmt.Sprintf("%s.%s.svc.cluster.local", instanceServiceName(name), ns)
	instancePrefix := "/" + name
	uiPrefix := "/ui/" + name
	agentType := instance.Spec.Agent.Type
	h, err := builtin.NewRegistry().Lookup(string(agentType))
	if err != nil {
		// Unknown types never get an OpenClaw route.
		return nil
	}
	descriptor := h.Describe()
	primaryPort := uint32(descriptor.DefaultPort)
	if instance.Spec.Agent.Port != nil {
		primaryPort = uint32(*instance.Spec.Agent.Port)
	}

	makeRoute := func(matchPrefix, rewriteURI string, port uint32, timeoutSecs int64) *istioapiv1beta1.HTTPRoute {
		route := &istioapiv1beta1.HTTPRoute{
			Match: []*istioapiv1beta1.HTTPMatchRequest{
				{Uri: &istioapiv1beta1.StringMatch{MatchType: &istioapiv1beta1.StringMatch_Prefix{Prefix: matchPrefix}}},
			},
			Rewrite: &istioapiv1beta1.HTTPRewrite{Uri: rewriteURI},
			Route: []*istioapiv1beta1.HTTPRouteDestination{
				{Destination: &istioapiv1beta1.Destination{
					Host: svcHost,
					Port: &istioapiv1beta1.PortSelector{Number: port},
				}},
			},
		}
		if timeoutSecs > 0 {
			route.Timeout = durationpb.New(time.Duration(timeoutSecs) * time.Second)
			route.Retries = &istioapiv1beta1.HTTPRetry{Attempts: 3, PerTryTimeout: durationpb.New(100 * time.Second)}
		}
		return route
	}
	makeRedirect := func(exactPath string) *istioapiv1beta1.HTTPRoute {
		return &istioapiv1beta1.HTTPRoute{
			Match: []*istioapiv1beta1.HTTPMatchRequest{
				{Uri: &istioapiv1beta1.StringMatch{MatchType: &istioapiv1beta1.StringMatch_Exact{Exact: exactPath}}},
			},
			Redirect: &istioapiv1beta1.HTTPRedirect{
				Uri:          exactPath + "/",
				RedirectCode: 301,
			},
		}
	}
	makeWebSocketRoute := func(exactPath string, port uint32) *istioapiv1beta1.HTTPRoute {
		return &istioapiv1beta1.HTTPRoute{
			Match: []*istioapiv1beta1.HTTPMatchRequest{
				{
					Uri: &istioapiv1beta1.StringMatch{MatchType: &istioapiv1beta1.StringMatch_Exact{Exact: exactPath}},
					Headers: map[string]*istioapiv1beta1.StringMatch{
						"upgrade": {MatchType: &istioapiv1beta1.StringMatch_Exact{Exact: "websocket"}},
					},
				},
			},
			Route: []*istioapiv1beta1.HTTPRouteDestination{
				{Destination: &istioapiv1beta1.Destination{
					Host: svcHost,
					Port: &istioapiv1beta1.PortSelector{Number: port},
				}},
			},
			Timeout: durationpb.New(300 * time.Second),
			Retries: &istioapiv1beta1.HTTPRetry{Attempts: 3, PerTryTimeout: durationpb.New(100 * time.Second)},
		}
	}

	routes := []*istioapiv1beta1.HTTPRoute{
		makeRedirect(uiPrefix),
		makeRedirect(instancePrefix + "/desktop"),
		makeRedirect(instancePrefix + "/term"),
	}
	if agentType == cetirunv1alpha1.AgentTypeCustom {
		services := customAccess{desktop: true}
		if len(access) > 0 {
			services = access[0]
		}
		routes = nil
		if services.desktop {
			routes = append(routes,
				makeRedirect(instancePrefix+"/desktop"),
				makeRoute(instancePrefix+"/desktop/", "/desktop/", novncPort, 0))
		}
		if services.terminal {
			port := services.terminalPort
			if port == 0 {
				port = ttydPort
			}
			route := makeRoute(instancePrefix+"/term/", "/term/", port, 0)
			route.Route[0].Destination.Host = fmt.Sprintf("%s.%s.svc.cluster.local",
				terminalpolicy.WorkloadName(instanceServiceName(name)), ns)
			routes = append(routes,
				makeRedirect(instancePrefix+"/term"),
				route)
		}
		if instance.Spec.Agent.WebUI && instance.Spec.Agent.Port != nil {
			routes = append(routes, makeRedirect(uiPrefix), makeRoute(uiPrefix+"/", uiPrefix+"/", primaryPort, 0))
		}
	} else if descriptor.WebappPort != 0 {
		routes = append(routes,
			makeRoute(uiPrefix+"/v1", "/v1", primaryPort, 300),
			makeRoute(uiPrefix+descriptor.HealthPath, descriptor.HealthPath, primaryPort, 0),
		)
		routes = append(routes,
			makeRoute(instancePrefix+"/desktop", "/desktop", novncPort, 0),
			makeRoute(instancePrefix+"/term", "/term", ttydPort, 0),
		)
		webappRoute := makeRoute(uiPrefix+"/", "/", uint32(descriptor.WebappPort), 0)
		webappRoute.Headers = &istioapiv1beta1.Headers{
			Request: &istioapiv1beta1.Headers_HeaderOperations{
				Set: map[string]string{"x-forwarded-prefix": uiPrefix},
			},
		}
		routes = append(routes, webappRoute)
	} else {
		// OpenClaw's Control UI normalizes controlUi.basePath by removing the
		// trailing slash before constructing its WebSocket URL. Preserve the
		// canonical HTTP redirect for /ui/<instance>, but route an Upgrade request
		// on that exact path directly to the gateway because WebSockets cannot
		// follow a 301 redirect.
		routes = append([]*istioapiv1beta1.HTTPRoute{makeWebSocketRoute(uiPrefix, primaryPort)}, routes...)
		routes = append(routes,
			makeRoute(instancePrefix+"/desktop", "/desktop", novncPort, 0),
			makeRoute(instancePrefix+"/term", "/term", ttydPort, 0),
			&istioapiv1beta1.HTTPRoute{
				Match: []*istioapiv1beta1.HTTPMatchRequest{{
					Uri: &istioapiv1beta1.StringMatch{MatchType: &istioapiv1beta1.StringMatch_Prefix{Prefix: uiPrefix + "/"}},
				}},
				Route: []*istioapiv1beta1.HTTPRouteDestination{{Destination: &istioapiv1beta1.Destination{
					Host: svcHost,
					Port: &istioapiv1beta1.PortSelector{Number: primaryPort},
				}}},
				Timeout: durationpb.New(300 * time.Second),
				Retries: &istioapiv1beta1.HTTPRetry{Attempts: 3, PerTryTimeout: durationpb.New(100 * time.Second)},
			},
		)
	}

	desired := &istiov1beta1.VirtualService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      virtualServiceName(name),
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "cetirun-operator",
				"app.kubernetes.io/instance":   name,
			},
		},
		Spec: istioapiv1beta1.VirtualService{
			Hosts:    []string{domainSuffix},
			Gateways: []string{gateway},
			ExportTo: []string{"*"},
			Http:     routes,
		},
	}
	if agentType == cetirunv1alpha1.AgentTypeCustom {
		desired.Annotations = map[string]string{customAgentUID: string(instance.UID)}
		desired.OwnerReferences = []metav1.OwnerReference{
			*metav1.NewControllerRef(instance, cetirunv1alpha1.GroupVersion.WithKind("Agent")),
		}
	}
	return desired
}

// setCondition upserts a metav1.Condition into the conditions slice.
func setCondition(conditions *[]metav1.Condition, condType string, status metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	for i, c := range *conditions {
		if c.Type == condType {
			if c.Status == status && c.Reason == reason && c.Message == message {
				// No change.
				return
			}
			(*conditions)[i].Status = status
			(*conditions)[i].Reason = reason
			(*conditions)[i].Message = message
			(*conditions)[i].LastTransitionTime = now
			return
		}
	}
	*conditions = append(*conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	})
}

// isConditionTrue returns true if the named condition exists and has status True.
func isConditionTrue(conditions []metav1.Condition, condType string) bool {
	for _, c := range conditions {
		if c.Type == condType {
			return c.Status == metav1.ConditionTrue
		}
	}
	return false
}
