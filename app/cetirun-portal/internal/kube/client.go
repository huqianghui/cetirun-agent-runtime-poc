// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"context"
	cryptorand "crypto/rand"
	stderrors "errors"
	"fmt"
	"maps"
	"net/url"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/configurationcoordination"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/agentconfig"
)

var ErrUnsupportedCustomUpdate = stderrors.New("Custom does not support Gateway Token, Tailscale, or Agent Identity without a governed Sandbox")
var ErrInstanceChanged = stderrors.New("Agent changed or was replaced; reload before saving")
var ErrInstanceDeleting = stderrors.New("Agent is being deleted")
var ErrOperatorConfigurationUnavailable = stderrors.New("Operator release configuration is unavailable")
var ErrAgentPolicyChanged = stderrors.New("AgentPolicy changed or was replaced; reload before saving")

func IsConfigurationCoordinationConflict(err error) bool {
	return configurationcoordination.IsConflict(err)
}

func IsConfigurationCoordinationUnavailable(err error) bool {
	return configurationcoordination.IsUnavailable(err)
}

func IsUnsupportedCustomUpdate(err error) bool {
	return stderrors.Is(err, ErrUnsupportedCustomUpdate)
}

func validateInstanceUpdate(instance *cetirunv1alpha1.Agent, opts UpdateInstanceOpts) error {
	if !instance.DeletionTimestamp.IsZero() {
		return ErrInstanceDeleting
	}
	if opts.ExpectedUID != "" && instance.UID != opts.ExpectedUID ||
		opts.ExpectedResourceVersion != "" && instance.ResourceVersion != opts.ExpectedResourceVersion {
		return ErrInstanceChanged
	}
	if opts.Authorize != nil {
		if err := opts.Authorize(instance); err != nil {
			return err
		}
	}
	if instance.Spec.Agent.Type == cetirunv1alpha1.AgentTypeCustom &&
		(opts.GatewayToken != nil || opts.TailscaleAuthKey != nil) {
		return ErrUnsupportedCustomUpdate
	}
	if opts.Agent != nil {
		if err := agentconfig.Validate(*opts.Agent); err != nil {
			return err
		}
		previous := agentconfig.Defaults(instance.Spec.Agent)
		if opts.Agent.Type != previous.Type || *opts.Agent.Username != *previous.Username ||
			previous.Image != nil && opts.Agent.Image == nil {
			return fmt.Errorf("agent identity and pinned image must be preserved")
		}
	}
	return nil
}

// AnnotationSecretUpdatedAt is set on the CR when a Secret is updated,
// triggering a reconcile even when the CR spec (generation) hasn't changed.
const AnnotationSecretUpdatedAt = "cetirun.microsoft.com/secret-updated-at"

// UpdateInstanceOpts specifies which fields to update on an existing instance.
// nil pointer = no change; non-nil = apply the pointed-to value.
type UpdateInstanceOpts struct {
	Agent                   *cetirunv1alpha1.HarnessSpec
	ExpectedUID             types.UID
	ExpectedResourceVersion string
	Authorize               func(*cetirunv1alpha1.Agent) error
	DisplayName             *string // nil = no change
	Team                    *string // nil = no change
	SandboxRef              *string // nil = no change; set with Team from the selected Team default
	GatewayToken            *string // nil = no change, non-empty = update
	TailscaleAuthKey        *string // nil = no change, "" = disable tailscale, non-empty = update/enable
	Model                   *string // nil = no change, "" = use deployment default
}

type InstanceVersion struct {
	UID             types.UID `json:"uid"`
	ResourceVersion string    `json:"resourceVersion"`
	// Generation is optional for compatibility with existing lifecycle
	// clients. Suspension uses it to distinguish a status-only write from a
	// concurrent specification change.
	Generation int64 `json:"-"`
}

// CreateInstanceOptions contains security-sensitive platform-derived fields.
// Callers must never populate owner identifiers from request JSON.
type CreateInstanceOptions struct {
	AgentIdentityEnabled bool
	OwnerTenantID        string
	OwnerObjectID        string
	SandboxRef           string
	ChartVersion         string
	Annotations          map[string]string
	// ModelAccess is written only when set: an Agent without it has both
	// model sources on.
	ModelAccess *cetirunv1alpha1.ModelAccessSpec
}

const chartVersionTTL = 5 * time.Minute

// Client wraps controller-runtime client for Agent operations.
type Client struct {
	client                      client.Client
	configurationCoordination   ConfigurationCoordination
	chartVersionMu              sync.RWMutex
	chartVersionVal             string
	chartVersionErr             error
	chartVersionExp             time.Time
	operatorDomain              string
	registryCredentialNamespace string
	sandboxStateNamespace       string
	skillMarketplaceCatalog     skillMarketplaceCatalogCache
}

// ConfigurationCoordination serializes writes protected by the optional
// UID-pinned Sandbox configuration authority.
type ConfigurationCoordination interface {
	Update(context.Context, client.Object, client.Object) error
	RecoverUpdate(context.Context, client.Object, client.Object) error
	Delete(context.Context, client.Object) error
}

// NewClient creates a new kube Client.
func NewClient(c client.Client) *Client {
	return &Client{client: c, sandboxStateNamespace: "cetirun-system"}
}

// SetSandboxStateNamespace selects the operator-owned common execution state.
func (c *Client) SetSandboxStateNamespace(namespace string) {
	if namespace != "" {
		c.sandboxStateNamespace = namespace
	}
}

// SetConfigurationCoordination enables coordinated Team, Sandbox and
// AgentPolicy writes for operations that opt into the protocol.
func (c *Client) SetConfigurationCoordination(coordination ConfigurationCoordination) {
	c.configurationCoordination = coordination
}

// GetInstance retrieves an Agent by alias (cluster-scoped).
func (c *Client) GetInstance(ctx context.Context, alias string) (*cetirunv1alpha1.Agent, error) {
	instance := &cetirunv1alpha1.Agent{}
	key := client.ObjectKey{Name: alias}
	if err := c.client.Get(ctx, key, instance); err != nil {
		return nil, err
	}
	return instance, nil
}

// CreateInstance creates a new Agent CR.
// instanceName is metadata.name; an empty value uses the caller's alias.
// If gatewayToken or tailscaleAuthKey are non-empty, it pre-creates the
// corresponding Secrets in the instance namespace before creating the CR.
func (c *Client) CreateInstance(ctx context.Context, alias, displayName, team, email, instanceName string, agent cetirunv1alpha1.HarnessSpec, gatewayToken, tailscaleAuthKey, model string, optional ...CreateInstanceOptions) (*cetirunv1alpha1.Agent, error) {
	if instanceName == "" {
		instanceName = alias
	}
	agent = agentconfig.Defaults(agent)
	if err := agentconfig.Validate(agent); err != nil {
		return nil, err
	}
	identityEnabled := len(optional) > 0 && optional[0].AgentIdentityEnabled
	if agent.Type == cetirunv1alpha1.AgentTypeCustom &&
		(gatewayToken != "" || tailscaleAuthKey != "" || identityEnabled && optional[0].SandboxRef == "") {
		return nil, ErrUnsupportedCustomUpdate
	}
	if identityEnabled && (optional[0].OwnerTenantID == "" || optional[0].OwnerObjectID == "") {
		return nil, fmt.Errorf("verified owner identity is required for Agent Identity")
	}
	sandboxRef := ""
	if len(optional) > 0 {
		sandboxRef = optional[0].SandboxRef
	}
	ns := fmt.Sprintf("cetirun-%s", instanceName)
	var createdNamespace *corev1.Namespace

	// Pre-create secrets if provided.
	if gatewayToken != "" || tailscaleAuthKey != "" {
		var err error
		createdNamespace, err = c.createNamespace(ctx, ns, alias)
		if err != nil {
			return nil, fmt.Errorf("ensure namespace %s: %w", ns, err)
		}
	}
	if gatewayToken != "" {
		if err := c.createOrUpdateSecret(ctx, ns, "gateway-token", map[string][]byte{
			"OPENCLAW_GATEWAY_TOKEN": []byte(gatewayToken),
		}); err != nil {
			return nil, fmt.Errorf("create gateway-token secret: %w", err)
		}
	}
	if tailscaleAuthKey != "" {
		if err := c.createOrUpdateSecret(ctx, ns, "tailscale-auth", map[string][]byte{
			"TAILSCALE_AUTH_KEY": []byte(tailscaleAuthKey),
		}); err != nil {
			return nil, fmt.Errorf("create tailscale-auth secret: %w", err)
		}
	}

	instance := &cetirunv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:       instanceName,
			Finalizers: []string{cetirunv1alpha1.Finalizer},
		},
		Spec: cetirunv1alpha1.AgentSpec{
			Agent:      agent,
			Model:      strings.TrimSpace(model),
			Team:       team,
			SandboxRef: sandboxRef,
			User: cetirunv1alpha1.UserSpec{
				Alias:       alias,
				DisplayName: displayName,
				Email:       email,
			},
		},
	}
	if agent.Type != cetirunv1alpha1.AgentTypeCustom {
		instance.Spec.GatewayTokenSecretRef = "gateway-token"
	}
	if len(optional) > 0 {
		instance.Annotations = maps.Clone(optional[0].Annotations)
		instance.Spec.Helm.Version = optional[0].ChartVersion
	}
	if len(optional) > 0 && optional[0].OwnerTenantID != "" && optional[0].OwnerObjectID != "" {
		if instance.Annotations == nil {
			instance.Annotations = map[string]string{}
		}
		instance.Annotations[cetirunv1alpha1.AnnotationOwnerTenantID] = optional[0].OwnerTenantID
		instance.Annotations[cetirunv1alpha1.AnnotationOwnerObjectID] = optional[0].OwnerObjectID
	}
	if identityEnabled {
		instance.Spec.AgentIdentity = &cetirunv1alpha1.AgentIdentitySpec{Enabled: true}
	}
	if tailscaleAuthKey != "" {
		instance.Spec.Tailscale = &cetirunv1alpha1.TailscaleSpec{
			AuthKeySecretRef: "tailscale-auth",
		}
	}
	if len(optional) > 0 && optional[0].ModelAccess != nil {
		instance.Spec.ModelAccess = optional[0].ModelAccess.DeepCopy()
	}
	if err := c.client.Create(ctx, instance, client.FieldValidation(metav1.FieldValidationStrict)); err != nil {
		return nil, err
	}
	if createdNamespace != nil {
		if err := c.bindCreatedNamespace(ctx, createdNamespace, instance); err != nil {
			return nil, fmt.Errorf("Agent created but namespace ownership could not be recorded; cleanup requires recovery: %w", err)
		}
	}
	return instance, nil
}

// DeleteInstance deletes an Agent by alias (CR object name = alias).
func (c *Client) DeleteInstance(ctx context.Context, alias string) error {
	instance := &cetirunv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name: alias,
		},
	}
	return c.client.Delete(ctx, instance)
}

// DeleteInstanceByName deletes an Agent by metadata.name with object preconditions.
func (c *Client) DeleteInstanceByName(ctx context.Context, instanceName string, versions ...InstanceVersion) error {
	instance, err := c.GetInstanceByName(ctx, instanceName)
	if err != nil {
		return err
	}
	if len(versions) > 0 {
		if err := validateInstanceUpdate(instance, UpdateInstanceOpts{ExpectedUID: versions[0].UID, ExpectedResourceVersion: versions[0].ResourceVersion}); err != nil {
			return err
		}
	}
	return c.client.Delete(ctx, instance, client.Preconditions{UID: &instance.UID, ResourceVersion: &instance.ResourceVersion})
}

// IsNotFound returns true if the error is a Kubernetes not-found error.
func IsNotFound(err error) bool {
	return errors.IsNotFound(err)
}

// IsAlreadyExists returns true if the error is a Kubernetes already-exists error.
func IsAlreadyExists(err error) bool {
	return errors.IsAlreadyExists(err)
}

// IsForbidden returns true if the error is a Kubernetes forbidden error
// (typically RBAC denial).
func IsForbidden(err error) bool {
	return errors.IsForbidden(err)
}

// GetGatewayToken retrieves the gateway token from the instance's Secret.
func (c *Client) GetGatewayToken(ctx context.Context, namespace, secretName string) (string, error) {
	if secretName == "" {
		return "", fmt.Errorf("secret name is empty for namespace %q", namespace)
	}

	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: namespace, Name: secretName}
	if err := c.client.Get(ctx, key, secret); err != nil {
		return "", fmt.Errorf("get secret %s/%s: %w", namespace, secretName, err)
	}
	tokenBytes, ok := secret.Data["OPENCLAW_GATEWAY_TOKEN"]
	if !ok || len(tokenBytes) == 0 {
		return "", fmt.Errorf("missing key OPENCLAW_GATEWAY_TOKEN in secret %s/%s", namespace, secretName)
	}
	return string(tokenBytes), nil
}

// GetAPIServerKey retrieves the Hermes API server key from the instance's Secret.
func (c *Client) GetAPIServerKey(ctx context.Context, namespace, secretName string) (string, error) {
	if secretName == "" {
		return "", fmt.Errorf("secret name is empty for namespace %q", namespace)
	}

	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: namespace, Name: secretName}
	if err := c.client.Get(ctx, key, secret); err != nil {
		return "", fmt.Errorf("get secret %s/%s: %w", namespace, secretName, err)
	}
	keyBytes, ok := secret.Data["API_SERVER_KEY"]
	if !ok || len(keyBytes) == 0 {
		return "", fmt.Errorf("missing key API_SERVER_KEY in secret %s/%s", namespace, secretName)
	}
	return string(keyBytes), nil
}

// GetTailscaleAuthKey retrieves the Tailscale auth key from the instance's Secret.
func (c *Client) GetTailscaleAuthKey(ctx context.Context, namespace, secretName string) (string, error) {
	if secretName == "" {
		return "", fmt.Errorf("secret name is empty for namespace %q", namespace)
	}

	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: namespace, Name: secretName}
	if err := c.client.Get(ctx, key, secret); err != nil {
		return "", fmt.Errorf("get secret %s/%s: %w", namespace, secretName, err)
	}
	keyBytes, ok := secret.Data["TAILSCALE_AUTH_KEY"]
	if !ok || len(keyBytes) == 0 {
		return "", fmt.Errorf("missing key TAILSCALE_AUTH_KEY in secret %s/%s", namespace, secretName)
	}
	return string(keyBytes), nil
}

// ensureNamespace creates the instance namespace if it doesn't already exist.
func (c *Client) ensureNamespace(ctx context.Context, ns, alias string) error {
	_, err := c.createNamespace(ctx, ns, alias)
	return err
}

// createNamespace returns only a namespace created by this call, never an
// existing namespace that a same-named Agent might otherwise adopt.
func (c *Client) createNamespace(ctx context.Context, ns, alias string) (*corev1.Namespace, error) {
	nsObj := &corev1.Namespace{}
	err := c.client.Get(ctx, client.ObjectKey{Name: ns}, nsObj)
	if err == nil {
		return nil, nil
	}
	if !errors.IsNotFound(err) {
		return nil, fmt.Errorf("get namespace %s: %w", ns, err)
	}
	nsObj = &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: ns,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "cetirun",
				"app.kubernetes.io/instance":   alias,
				"app.kubernetes.io/managed-by": "cetirun-operator",
			},
		},
	}
	if err := c.client.Create(ctx, nsObj); err != nil {
		return nil, client.IgnoreAlreadyExists(err)
	}
	return nsObj, nil
}

func (c *Client) bindCreatedNamespace(ctx context.Context, created *corev1.Namespace, agent *cetirunv1alpha1.Agent) error {
	if created.UID == "" || agent.UID == "" {
		return fmt.Errorf("namespace and Agent must have persisted UIDs")
	}
	owner := metav1.OwnerReference{APIVersion: cetirunv1alpha1.GroupVersion.String(), Kind: "Agent", Name: agent.Name, UID: agent.UID}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &corev1.Namespace{}
		if err := c.client.Get(ctx, client.ObjectKeyFromObject(created), current); err != nil {
			return err
		}
		if current.UID != created.UID || !current.DeletionTimestamp.IsZero() {
			return fmt.Errorf("newly created namespace was replaced or is deleting")
		}
		if len(current.OwnerReferences) != 0 {
			if len(current.OwnerReferences) == 1 && current.OwnerReferences[0] == owner {
				return nil
			}
			return fmt.Errorf("newly created namespace already has another owner")
		}
		before := current.DeepCopy()
		current.OwnerReferences = []metav1.OwnerReference{owner}
		return c.client.Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	})
}

// UpdateInstance updates an existing Agent CR and its associated Secrets.
// Secrets are updated first (idempotent), then the CR is updated inside a
// RetryOnConflict loop to handle races with operator status updates.
func (c *Client) UpdateInstance(ctx context.Context, alias string, opts UpdateInstanceOpts) (*cetirunv1alpha1.Agent, error) {
	return c.UpdateInstanceByName(ctx, alias, opts)
}

// UpdateInstanceByName updates an Agent looked up by metadata.name.
// Unlike UpdateInstance (which looks up by alias==metadata.name), this uses
// the actual CR name so it works for instances with custom names (multi-instance).
func (c *Client) UpdateInstanceByName(ctx context.Context, instanceName string, opts UpdateInstanceOpts) (*cetirunv1alpha1.Agent, error) {
	// Pre-flight: verify the instance exists and get the alias for namespace/secret ops.
	existing, err := c.GetInstanceByName(ctx, instanceName)
	if err != nil {
		return nil, err
	}
	if err := validateInstanceUpdate(existing, opts); err != nil {
		return nil, err
	}
	if opts.ExpectedUID == "" {
		opts.ExpectedUID = existing.UID
	}
	alias := existing.Spec.User.Alias
	ns := fmt.Sprintf("cetirun-%s", instanceName)
	secretsUpdated := false

	if opts.GatewayToken != nil && *opts.GatewayToken != "" {
		if err := c.ensureNamespace(ctx, ns, alias); err != nil {
			return nil, fmt.Errorf("ensure namespace %s: %w", ns, err)
		}
		if err := c.createOrUpdateSecret(ctx, ns, "gateway-token", map[string][]byte{
			"OPENCLAW_GATEWAY_TOKEN": []byte(*opts.GatewayToken),
		}); err != nil {
			return nil, fmt.Errorf("update gateway-token secret: %w", err)
		}
		secretsUpdated = true
	}
	if opts.TailscaleAuthKey != nil && *opts.TailscaleAuthKey != "" {
		if err := c.ensureNamespace(ctx, ns, alias); err != nil {
			return nil, fmt.Errorf("ensure namespace %s: %w", ns, err)
		}
		if err := c.createOrUpdateSecret(ctx, ns, "tailscale-auth", map[string][]byte{
			"TAILSCALE_AUTH_KEY": []byte(*opts.TailscaleAuthKey),
		}); err != nil {
			return nil, fmt.Errorf("update tailscale-auth secret: %w", err)
		}
		secretsUpdated = true
	}

	var result *cetirunv1alpha1.Agent
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		instance, err := c.GetInstanceByName(ctx, instanceName)
		if err != nil {
			return err
		}
		if err := validateInstanceUpdate(instance, opts); err != nil {
			return err
		}
		before := instance.DeepCopy()
		crChanged := false
		if opts.Agent != nil {
			instance.Spec.Agent = *opts.Agent.DeepCopy()
			crChanged = true
		}

		if opts.DisplayName != nil {
			instance.Spec.User.DisplayName = *opts.DisplayName
			crChanged = true
		}
		if opts.Team != nil {
			instance.Spec.Team = *opts.Team
			crChanged = true
		}
		if opts.SandboxRef != nil {
			instance.Spec.SandboxRef = *opts.SandboxRef
			crChanged = true
		}
		if opts.GatewayToken != nil && *opts.GatewayToken != "" {
			touchSecretAnnotation(instance)
			crChanged = true
		}
		if opts.TailscaleAuthKey != nil {
			if *opts.TailscaleAuthKey != "" {
				if instance.Spec.Tailscale == nil {
					instance.Spec.Tailscale = &cetirunv1alpha1.TailscaleSpec{
						AuthKeySecretRef: "tailscale-auth",
					}
				}
				touchSecretAnnotation(instance)
				crChanged = true
			} else {
				instance.Spec.Tailscale = nil
				crChanged = true
			}
		}
		if opts.Model != nil {
			instance.Spec.Model = strings.TrimSpace(*opts.Model)
			crChanged = true
		}
		if secretsUpdated && !crChanged {
			touchSecretAnnotation(instance)
			crChanged = true
		}

		if crChanged {
			if err := c.client.Patch(ctx, instance, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}), client.FieldValidation(metav1.FieldValidationStrict)); err != nil {
				return fmt.Errorf("update instance: %w", err)
			}
		}
		result = instance
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// UpgradeInstanceByName upgrades an Agent looked up by metadata.name.
func (c *Client) UpgradeInstanceByName(ctx context.Context, instanceName string, versions ...InstanceVersion) (*cetirunv1alpha1.Agent, error) {
	latestVersion, err := c.GetOperatorChartVersion(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOperatorConfigurationUnavailable, err)
	}
	var result *cetirunv1alpha1.Agent
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		instance, err := c.GetInstanceByName(ctx, instanceName)
		if err != nil {
			return err
		}
		if len(versions) > 0 {
			if err := validateInstanceUpdate(instance, UpdateInstanceOpts{ExpectedUID: versions[0].UID, ExpectedResourceVersion: versions[0].ResourceVersion}); err != nil {
				return err
			}
		}
		if instance.Spec.Helm.Version == latestVersion {
			result = instance
			return nil
		}
		before := instance.DeepCopy()
		instance.Spec.Helm.Version = latestVersion
		if err := c.client.Patch(ctx, instance, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}), client.FieldValidation(metav1.FieldValidationStrict)); err != nil {
			return fmt.Errorf("update instance: %w", err)
		}
		result = instance
		return nil
	})
	return result, err
}

// touchSecretAnnotation sets a timestamp annotation on the CR so that
// a watch event fires even when spec.generation doesn't change (secret-only updates).
func touchSecretAnnotation(instance *cetirunv1alpha1.Agent) {
	if instance.Annotations == nil {
		instance.Annotations = make(map[string]string)
	}
	instance.Annotations[AnnotationSecretUpdatedAt] = time.Now().UTC().Format(time.RFC3339Nano)
}

// createOrUpdateSecret creates an Opaque Secret or updates it if it already exists.
//
// For the "gateway-token" secret, this also ensures an CETIRUN_AUTH_KEY field
// is present (auto-generated 48-char alphanumeric, preserved across updates).
// CETIRUN_AUTH_KEY signs Hermes Dashboard sessions. The chart cannot manage
// this field itself because Portal passes the secret in as
// secrets.existingSecret, which skips the chart's Secret template.
func (c *Client) createOrUpdateSecret(ctx context.Context, ns, name string, data map[string][]byte) error {
	labels := map[string]string{
		"app.kubernetes.io/name":       "cetirun",
		"app.kubernetes.io/managed-by": "cetirun-portal",
	}
	secret := &corev1.Secret{}
	err := c.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, secret)
	if err == nil {
		// Preserve CETIRUN_AUTH_KEY (and any other chart-managed fields the
		// caller didn't supply) by merging caller-supplied data over the
		// in-cluster Secret. Replacing secret.Data outright would rotate the
		// Dashboard session key on every update, since env-var-mounted secrets
		// do not refresh in running Pods.
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		for k, v := range data {
			secret.Data[k] = v
		}
		if err := ensureCetiRunAuthKey(secret, name); err != nil {
			return err
		}
		return c.client.Update(ctx, secret)
	}
	if !errors.IsNotFound(err) {
		return fmt.Errorf("get secret %s/%s: %w", ns, name, err)
	}
	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
			Labels:    labels,
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
	if err := ensureCetiRunAuthKey(secret, name); err != nil {
		return err
	}
	return c.client.Create(ctx, secret)
}

// ensureCetiRunAuthKey injects an CETIRUN_AUTH_KEY into the gateway-token
// secret if one isn't already present. Preserves any existing value so that
// subsequent CR/secret updates do not invalidate active Dashboard sessions.
func ensureCetiRunAuthKey(secret *corev1.Secret, name string) error {
	if name != "gateway-token" {
		return nil
	}
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	// Preserve existing key set on the in-cluster object.
	if existing, ok := secret.Data["CETIRUN_AUTH_KEY"]; ok && len(existing) > 0 {
		return nil
	}
	key, err := randAlphaNum(48)
	if err != nil {
		return fmt.Errorf("generate CETIRUN_AUTH_KEY: %w", err)
	}
	secret.Data["CETIRUN_AUTH_KEY"] = []byte(key)
	return nil
}

// randAlphaNum returns a cryptographically random alphanumeric string of
// length n, matching the chart-side randAlphaNum helper used elsewhere.
// Returns an error when the system CSPRNG fails — callers must surface that
// rather than fall back to a predictable value, since this string ends up as
// the CETIRUN_AUTH_KEY bearer credential.
func randAlphaNum(n int) (string, error) {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	buf := make([]byte, n)
	if _, err := cryptorand.Read(buf); err != nil {
		return "", err
	}
	for i, b := range buf {
		buf[i] = charset[int(b)%len(charset)]
	}
	return string(buf), nil
}

// GetInstanceByName returns the Agent with the given metadata.name.
// Since Agent is cluster-scoped and metadata.name is globally unique,
// this is a direct Get rather than a list scan.
func (c *Client) GetInstanceByName(ctx context.Context, instanceName string) (*cetirunv1alpha1.Agent, error) {
	instance := &cetirunv1alpha1.Agent{}
	if err := c.client.Get(ctx, client.ObjectKey{Name: instanceName}, instance); err != nil {
		return nil, err
	}
	return instance, nil
}

// GetInstanceByAlias returns the first Agent whose spec.user.alias matches.
// Used in multi-instance mode where metadata.name != alias.
func (c *Client) GetInstanceByAlias(ctx context.Context, alias string) (*cetirunv1alpha1.Agent, error) {
	list := &cetirunv1alpha1.AgentList{}
	if err := c.client.List(ctx, list); err != nil {
		return nil, err
	}
	for i := range list.Items {
		if list.Items[i].Spec.User.Alias == alias {
			return &list.Items[i], nil
		}
	}
	return nil, errors.NewNotFound(cetirunv1alpha1.GroupVersion.WithResource("agents").GroupResource(), alias)
}

// ListAllInstances returns all Agent CRs in the cluster.
func (c *Client) ListAllInstances(ctx context.Context) ([]cetirunv1alpha1.Agent, error) {
	list := &cetirunv1alpha1.AgentList{}
	if err := c.client.List(ctx, list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// GetOperatorChartVersion returns the --chart-version from the operator Deployment.
// The result (including errors) is cached for 5 minutes to avoid repeated API calls.
func (c *Client) GetOperatorChartVersion(ctx context.Context) (string, error) {
	c.chartVersionMu.RLock()
	if time.Now().Before(c.chartVersionExp) {
		v, err := c.chartVersionVal, c.chartVersionErr
		c.chartVersionMu.RUnlock()
		return v, err
	}
	c.chartVersionMu.RUnlock()

	c.chartVersionMu.Lock()
	defer c.chartVersionMu.Unlock()
	// double-check after acquiring write lock
	if time.Now().Before(c.chartVersionExp) {
		return c.chartVersionVal, c.chartVersionErr
	}

	v, err := c.fetchOperatorChartVersion(ctx)
	c.chartVersionVal = v
	c.chartVersionErr = err
	c.chartVersionExp = time.Now().Add(chartVersionTTL)
	return v, err
}

func (c *Client) fetchOperatorChartVersion(ctx context.Context) (string, error) {
	c.operatorDomain = ""
	deploy := &appsv1.Deployment{}
	key := client.ObjectKey{Namespace: "cetirun-system", Name: "cetirun-operator"}
	if err := c.client.Get(ctx, key, deploy); err != nil {
		return "", fmt.Errorf("get operator deployment: %w", err)
	}
	version := ""
	for _, container := range deploy.Spec.Template.Spec.Containers {
		if container.Name != "manager" {
			continue
		}
		for _, arg := range container.Args {
			if strings.HasPrefix(arg, "--chart-version=") {
				version = strings.TrimPrefix(arg, "--chart-version=")
			}
			if strings.HasPrefix(arg, "--domain-suffix=") {
				c.operatorDomain = strings.TrimPrefix(arg, "--domain-suffix=")
			}
		}
	}
	if version != "" {
		return version, nil
	}
	return "", fmt.Errorf("--chart-version not found in operator deployment args")
}

// Reuse the same cached deployment observation as chart-version discovery.
// The gateway may have a different origin from the Portal itself.
func (c *Client) GetOperatorDomainSuffix(ctx context.Context) (string, error) {
	if _, err := c.GetOperatorChartVersion(ctx); err != nil {
		return "", err
	}
	c.chartVersionMu.RLock()
	domain := c.operatorDomain
	c.chartVersionMu.RUnlock()
	parsed, err := url.Parse("https://" + domain)
	if err != nil || domain == "" || parsed.Host != domain || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("operator domain-suffix is unavailable or invalid")
	}
	return domain, nil
}

// GetEvents returns K8s Events related to an Agent CR.
// Agent is cluster-scoped, so we search all namespaces using a
// raw field selector that the API server natively supports for Events.
func (c *Client) GetEvents(ctx context.Context, alias string) ([]corev1.Event, error) {
	eventList := &corev1.EventList{}
	if err := c.client.List(ctx, eventList, client.MatchingFields{
		"involvedObject.name": alias,
		"involvedObject.kind": "Agent",
	}); err != nil {
		return nil, fmt.Errorf("list events for CR %s: %w", alias, err)
	}
	return eventList.Items, nil
}

// GetNamespaceEvents returns all Events in the given namespace.
func (c *Client) GetNamespaceEvents(ctx context.Context, namespace string) ([]corev1.Event, error) {
	eventList := &corev1.EventList{}
	if err := c.client.List(ctx, eventList, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list events in namespace %s: %w", namespace, err)
	}
	return eventList.Items, nil
}

// GetPods returns all Pods in the given namespace.
func (c *Client) GetPods(ctx context.Context, namespace string) ([]corev1.Pod, error) {
	podList := &corev1.PodList{}
	if err := c.client.List(ctx, podList, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list pods in namespace %s: %w", namespace, err)
	}
	return podList.Items, nil
}

// UpgradeInstance sets the instance's spec.helm.version to the operator's latest chart version.
func (c *Client) UpgradeInstance(ctx context.Context, alias string) (*cetirunv1alpha1.Agent, error) {
	latestVersion, err := c.GetOperatorChartVersion(ctx)
	if err != nil {
		return nil, fmt.Errorf("get latest chart version: %w", err)
	}
	var result *cetirunv1alpha1.Agent
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		instance, err := c.GetInstance(ctx, alias)
		if err != nil {
			return err
		}
		if instance.Spec.Helm.Version == latestVersion {
			result = instance
			return nil
		}
		before := instance.DeepCopy()
		instance.Spec.Helm.Version = latestVersion
		if err := c.client.Patch(ctx, instance, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return fmt.Errorf("update instance: %w", err)
		}
		result = instance
		return nil
	})
	return result, err
}

// ListTeams returns all Team CRs (cluster-scoped).
func (c *Client) ListTeams(ctx context.Context) ([]cetirunv1alpha1.Team, error) {
	list := &cetirunv1alpha1.TeamList{}
	if err := c.client.List(ctx, list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// GetTeam retrieves a single Team by name (cluster-scoped).
func (c *Client) GetTeam(ctx context.Context, name string) (*cetirunv1alpha1.Team, error) {
	team := &cetirunv1alpha1.Team{}
	if err := c.client.Get(ctx, client.ObjectKey{Name: name}, team); err != nil {
		return nil, err
	}
	return team, nil
}

// CreateTeam creates a new Team.
func (c *Client) CreateTeam(ctx context.Context, team *cetirunv1alpha1.Team) (*cetirunv1alpha1.Team, error) {
	if err := c.client.Create(ctx, team); err != nil {
		return nil, err
	}
	return team, nil
}

// ListAgentPolicies returns all cluster-scoped AgentPolicy resources.
func (c *Client) ListAgentPolicies(ctx context.Context) ([]cetirunv1alpha1.AgentPolicy, error) {
	list := &cetirunv1alpha1.AgentPolicyList{}
	if err := c.client.List(ctx, list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// GetAgentPolicy retrieves a cluster-scoped AgentPolicy by name.
func (c *Client) GetAgentPolicy(ctx context.Context, name string) (*cetirunv1alpha1.AgentPolicy, error) {
	policy := &cetirunv1alpha1.AgentPolicy{}
	if err := c.client.Get(ctx, client.ObjectKey{Name: name}, policy); err != nil {
		return nil, err
	}
	return policy, nil
}

// CreateAgentPolicy creates a new cluster-scoped AgentPolicy.
func (c *Client) CreateAgentPolicy(ctx context.Context, policy *cetirunv1alpha1.AgentPolicy) (*cetirunv1alpha1.AgentPolicy, error) {
	if err := c.client.Create(ctx, policy); err != nil {
		return nil, err
	}
	return policy, nil
}

// AgentPolicyVersion identifies the exact AgentPolicy an administrator read.
type AgentPolicyVersion struct {
	UID             types.UID
	ResourceVersion string
}

func validateAgentPolicyVersion(policy *cetirunv1alpha1.AgentPolicy, version AgentPolicyVersion) error {
	if version.UID == "" || version.ResourceVersion == "" || policy.UID != version.UID || policy.ResourceVersion != version.ResourceVersion {
		return ErrAgentPolicyChanged
	}
	return nil
}

// UpdateAgentPolicySpec updates the Portal-managed policy fields while
// preserving runtime-owned controls. The handler supplies a spec already
// merged from the current resource for controls that the Portal does not own.
func (c *Client) UpdateAgentPolicySpec(ctx context.Context, name string, spec cetirunv1alpha1.AgentPolicySpec, version AgentPolicyVersion) (*cetirunv1alpha1.AgentPolicy, error) {
	policy, err := c.GetAgentPolicy(ctx, name)
	if err != nil {
		return nil, err
	}
	before := policy.DeepCopy()
	managed := spec.DeepCopy()
	policy.Spec.Harnesses = managed.Harnesses
	policy.Spec.Skills = managed.Skills
	policy.Spec.ModelAccess = managed.ModelAccess
	policy.Spec.AgentIdentity = managed.AgentIdentity
	policy.Spec.EntraID = managed.EntraID
	policy.Spec.SandboxSelection = managed.SandboxSelection
	policy.Spec.Security = managed.Security
	policy.Spec.Network = managed.Network
	policy.Spec.KubernetesAccess = managed.KubernetesAccess
	if err := c.recoverConfigurationUpdate(ctx, before, policy); err != nil {
		return nil, err
	}
	if err := validateAgentPolicyVersion(policy, version); err != nil {
		return nil, err
	}
	if equality.Semantic.DeepEqual(before.Spec, policy.Spec) {
		return policy, nil
	}
	if c.configurationCoordination != nil {
		if err := c.configurationCoordination.Update(ctx, before, policy); err != nil {
			return nil, fmt.Errorf("update agent policy through configuration coordination: %w", err)
		}
		updated, err := c.GetAgentPolicy(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("read coordinated agent policy update: %w", err)
		}
		return updated, nil
	}
	if err := c.client.Patch(ctx, policy, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}), client.FieldValidation(metav1.FieldValidationStrict)); err != nil {
		return nil, fmt.Errorf("update agent policy: %w", err)
	}
	return policy, nil
}

// DeleteAgentPolicy deletes the exact AgentPolicy the caller read.
func (c *Client) DeleteAgentPolicy(ctx context.Context, name string, version AgentPolicyVersion) error {
	policy, err := c.GetAgentPolicy(ctx, name)
	if err != nil {
		return err
	}
	if err := validateAgentPolicyVersion(policy, version); err != nil {
		return err
	}
	if c.configurationCoordination != nil {
		if err := c.configurationCoordination.Delete(ctx, policy); err != nil {
			return fmt.Errorf("delete agent policy through configuration coordination: %w", err)
		}
		return nil
	}
	return c.client.Delete(ctx, policy,
		client.Preconditions{UID: &policy.UID, ResourceVersion: &policy.ResourceVersion},
		client.PropagationPolicy(metav1.DeletePropagationBackground))
}
