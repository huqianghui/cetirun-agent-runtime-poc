// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxplatform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	pathvalidation "k8s.io/apimachinery/pkg/api/validation/path"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	strictjson "sigs.k8s.io/json"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
)

const (
	ConfigurationKey = "platform.json"
	// MaxConfigurationBytes bounds the UTF-8 JSON document before decoding.
	MaxConfigurationBytes = 512 * 1024

	maxSandboxPlatformObjects    = 128
	maxSandboxPlatformJSONTokens = 16 * 1024
	maxSandboxPlatformJSONDepth  = 12
)

// Configuration is the v1 document in ConfigMap data[platform.json].
// All four maps are required and non-null; empty maps declare no support. The only
// registration/platform keys are kata, gvisor and container, with matching entries and pinned
// RuntimeClasses. Top-level fields use lower-camel JSON names. Inner values retain
// their existing case-sensitive field names: registrations use Capabilities and
// Available, runtime pins UID and Handler, and storage pins UID and Provisioner.
// Capabilities and Platform retain their existing JSON tags.
// Unknown/duplicate fields, null values, excessive nesting and oversized input
// are rejected. At most 128 distinct API objects, including the ConfigMap, may
// participate in a snapshot.
type Configuration struct {
	Version        string                                     `json:"version"`
	Registrations  map[api.SandboxType]sandbox.Registration   `json:"registrations"`
	Platforms      map[api.SandboxType]sandboxpolicy.Platform `json:"platforms"`
	RuntimeClasses map[string]RuntimePrerequisite             `json:"runtimeClasses"`
	StorageClasses map[string]StoragePrerequisite             `json:"storageClasses"`
}

// KubernetesSource reads one explicitly selected ConfigMap.
// APIReader must be an authoritative reader, such as manager.GetAPIReader.
// Namespace and Name must come from trusted process configuration; namespace
// write protection is a deployment prerequisite, not inferred from object labels.
// This source never discovers or qualifies runtimes, topology, identities or
// enforcement. Those declarations remain the platform owner's responsibility.
// It verifies pinned RuntimeClasses and StorageClasses; consumers verify only
// their requested ClusterRole approvals. It performs no writes and supplies
// neither enforcement evidence nor start permits.
type KubernetesSource struct {
	APIReader client.Reader
	Namespace string
	Name      string
	Now       func() time.Time
}

var _ Source = (*KubernetesSource)(nil)

func (s *KubernetesSource) Snapshot(ctx context.Context) (Snapshot, error) {
	empty := Snapshot{}
	if s == nil || s.APIReader == nil || len(validation.IsDNS1123Label(s.Namespace)) != 0 ||
		len(validation.IsDNS1123Subdomain(s.Name)) != 0 {
		return empty, fmt.Errorf("%w: an authoritative reader and explicit valid configuration namespace/name are required", sandboxpolicy.ErrUnavailable)
	}
	observedAt := configurationNow(s.Now)
	if observedAt.IsZero() {
		return empty, fmt.Errorf("%w: configuration observation time is required", sandboxpolicy.ErrUnavailable)
	}
	cm, err := s.readConfiguration(ctx)
	if err != nil {
		return empty, err
	}
	document, err := decodeSandboxPlatformConfiguration(cm.Data[ConfigurationKey])
	if err != nil {
		return empty, err
	}
	sources := map[string]sandboxworkload.SourceReference{}
	if err := addSandboxPlatformSource(sources, sandboxSource(cm, "v1", "ConfigMap")); err != nil {
		return empty, err
	}
	for _, name := range slices.Sorted(maps.Keys(document.RuntimeClasses)) {
		pin := document.RuntimeClasses[name]
		object := &nodev1.RuntimeClass{}
		if err := readSandboxPlatformObject(ctx, s.APIReader, client.ObjectKey{Name: name}, object); err != nil {
			return empty, err
		}
		if object.UID != pin.UID || object.Handler != pin.Handler {
			return empty, fmt.Errorf("%w: RuntimeClass no longer matches its configured UID/handler", sandboxpolicy.ErrUnavailable)
		}
		if err := addSandboxPlatformSource(sources, sandboxSource(object, nodev1.SchemeGroupVersion.String(), "RuntimeClass")); err != nil {
			return empty, err
		}
	}
	for _, name := range slices.Sorted(maps.Keys(document.StorageClasses)) {
		pin := document.StorageClasses[name]
		object := &storagev1.StorageClass{}
		if err := readSandboxPlatformObject(ctx, s.APIReader, client.ObjectKey{Name: name}, object); err != nil {
			return empty, err
		}
		if object.UID != pin.UID || object.Provisioner != pin.Provisioner {
			return empty, fmt.Errorf("%w: StorageClass no longer matches its configured UID/provisioner", sandboxpolicy.ErrUnavailable)
		}
		if err := addSandboxPlatformSource(sources, sandboxSource(object, storagev1.SchemeGroupVersion.String(), "StorageClass")); err != nil {
			return empty, err
		}
	}
	// A second collect detects changes across the initial read interval. This is
	// optimistic snapshot validation, not a lock or evidence for future execution.
	references := make([]sandboxworkload.SourceReference, 0, len(sources))
	for _, key := range slices.Sorted(maps.Keys(sources)) {
		source := sources[key]
		references = append(references, source)
		if source.Kind == "ConfigMap" {
			continue
		}
		object := &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: source.APIVersion, Kind: source.Kind}}
		if err := readSandboxPlatformObject(ctx, s.APIReader, client.ObjectKey{Namespace: source.Namespace, Name: source.Name}, object); err != nil {
			return empty, err
		}
		if sandboxSource(object, source.APIVersion, source.Kind) != source {
			return empty, fmt.Errorf("%w: platform dependency changed during capture", ErrSourceChanged)
		}
	}
	current, err := s.readConfiguration(ctx)
	if err != nil {
		return empty, err
	}
	if current.UID != cm.UID || current.ResourceVersion != cm.ResourceVersion ||
		current.Data[ConfigurationKey] != cm.Data[ConfigurationKey] {
		return empty, fmt.Errorf("%w: platform configuration changed during capture", ErrSourceChanged)
	}
	encoded, err := json.Marshal(struct {
		Version       string
		Configuration Configuration
		Sources       []sandboxworkload.SourceReference
	}{"sandbox-platform-snapshot-v1", document, references})
	if err != nil {
		return empty, fmt.Errorf("encode Sandbox platform snapshot revision: %w", err)
	}
	digest := sha256.Sum256(encoded)
	revision := hex.EncodeToString(digest[:])
	for kind, registration := range document.Registrations {
		registration.Capabilities.Revision = revision
		document.Registrations[kind] = registration
	}
	return Snapshot{
		ObservedAt: observedAt, Registrations: document.Registrations, Platforms: document.Platforms,
		RuntimeClasses: document.RuntimeClasses, StorageClasses: document.StorageClasses,
	}, nil
}

func (s *KubernetesSource) readConfiguration(ctx context.Context) (*corev1.ConfigMap, error) {
	cm := &corev1.ConfigMap{}
	if err := readSandboxPlatformObject(ctx, s.APIReader, client.ObjectKey{Namespace: s.Namespace, Name: s.Name}, cm); err != nil {
		return nil, err
	}
	for _, owner := range cm.OwnerReferences {
		if owner.Kind == "Agent" {
			return nil, fmt.Errorf("%w: Agent-owned platform configuration is not trusted", sandboxpolicy.ErrOwnership)
		}
	}
	for _, key := range []string{
		sandboxworkload.AgentNameAnnotation, sandboxworkload.AgentUIDAnnotation,
		sandboxpolicy.AgentUIDLabel, sandboxpolicy.BindingAnnotation, sandboxworkload.KindAnnotation,
	} {
		_, label := cm.Labels[key]
		_, annotation := cm.Annotations[key]
		if label || annotation {
			return nil, fmt.Errorf("%w: Agent-derived platform configuration is not trusted", sandboxpolicy.ErrOwnership)
		}
	}
	_, present := cm.Data[ConfigurationKey]
	_, binary := cm.BinaryData[ConfigurationKey]
	if !present || binary {
		return nil, fmt.Errorf("%w: platform.json must be supplied as ConfigMap text data", sandboxpolicy.ErrUnavailable)
	}
	return cm, nil
}

func readSandboxPlatformObject(ctx context.Context, reader client.Reader, key client.ObjectKey, object client.Object) error {
	if err := reader.Get(ctx, key, object); err != nil {
		return fmt.Errorf("read Sandbox platform configuration dependency: %w", err)
	}
	if client.ObjectKeyFromObject(object) != key || object.GetUID() == "" || object.GetResourceVersion() == "" ||
		!object.GetDeletionTimestamp().IsZero() {
		return fmt.Errorf("%w: platform configuration requires persisted, non-terminating objects at the configured names", sandboxpolicy.ErrUnavailable)
	}
	return nil
}

func decodeSandboxPlatformConfiguration(raw string) (Configuration, error) {
	var document Configuration
	if len(raw) == 0 || len(raw) > MaxConfigurationBytes || !utf8.ValidString(raw) {
		return document, fmt.Errorf("%w: platform.json must be nonempty UTF-8 JSON of at most 512 KiB", sandboxpolicy.ErrUnavailable)
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	depth, tokens := 0, 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil || token == nil {
			return document, fmt.Errorf("%w: malformed or null platform JSON", sandboxpolicy.ErrUnavailable)
		}
		tokens++
		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
		if depth > maxSandboxPlatformJSONDepth || tokens > maxSandboxPlatformJSONTokens {
			return document, fmt.Errorf("%w: platform JSON nesting or token budget exceeded", sandboxpolicy.ErrUnavailable)
		}
	}
	// Decoder diagnostics can contain configuration payloads; do not return them.
	problems, err := strictjson.UnmarshalStrict([]byte(raw), &document, strictjson.DisallowDuplicateFields, strictjson.DisallowUnknownFields)
	if err != nil || len(problems) != 0 {
		return document, fmt.Errorf("%w: malformed, unknown or duplicate platform JSON fields", sandboxpolicy.ErrUnavailable)
	}
	if err := validateSandboxPlatformConfiguration(document); err != nil {
		return document, err
	}
	return document, nil
}

func validateSandboxPlatformConfiguration(document Configuration) error {
	if document.Version != "v1" || document.Registrations == nil || document.Platforms == nil ||
		document.RuntimeClasses == nil || document.StorageClasses == nil {
		return fmt.Errorf("%w: platform version v1 and all four inventory maps are required", sandboxpolicy.ErrUnavailable)
	}
	roleNames := map[string]struct{}{}
	for kind, registration := range document.Registrations {
		if kind != api.SandboxKata && kind != api.SandboxGVisor && kind != api.SandboxContainer {
			return fmt.Errorf("%w: unsupported Sandbox registration key", sandboxpolicy.ErrUnsupportedControl)
		}
		capabilities := registration.Capabilities
		if len(sandbox.CheckRuntimeCapabilities(kind, capabilities)) != 0 {
			return fmt.Errorf("%w: unsupported Sandbox capability profile", sandboxpolicy.ErrUnsupportedControl)
		}
		_, runtimeExists := document.RuntimeClasses[capabilities.RuntimeClassName]
		if !runtimeExists || !sandboxPlatformValue(capabilities.Revision) ||
			(capabilities.RequirePerContainerResourceLimits && !capabilities.PerContainerResourceLimits) {
			return fmt.Errorf("%w: registration requires a revision, pinned RuntimeClass and consistent resource controls", sandboxpolicy.ErrUnavailable)
		}
		for _, mode := range capabilities.HomeAccessModes {
			if mode != corev1.ReadWriteOnce && mode != corev1.ReadWriteOncePod {
				return fmt.Errorf("%w: unsupported registered HOME access mode", sandboxpolicy.ErrUnsupportedControl)
			}
		}
		platform, exists := document.Platforms[kind]
		if !exists {
			return fmt.Errorf("%w: registration requires a matching platform declaration", sandboxpolicy.ErrUnavailable)
		}
		if err := sandboxpolicy.ValidatePlatform(platform, capabilities); err != nil {
			// Validators can quote topology values. Preserve the error category,
			// not payload-bearing diagnostics that callers might log.
			cause := sandboxpolicy.ErrUnavailable
			for _, candidate := range []error{sandboxpolicy.ErrUnsupportedControl, sandboxpolicy.ErrConflict, sandboxpolicy.ErrUnapprovedGrant} {
				if errors.Is(err, candidate) {
					cause = candidate
					break
				}
			}
			return fmt.Errorf("%w: invalid Sandbox platform declaration", cause)
		}
		if err := validateSandboxPlatformIdentities(platform, capabilities); err != nil {
			return err
		}
		if err := validateSandboxPlatformGrants(platform.Grants, roleNames); err != nil {
			return err
		}
	}
	if len(document.RuntimeClasses)+len(document.StorageClasses)+len(roleNames)+1 > maxSandboxPlatformObjects {
		return fmt.Errorf("%w: platform object inventory exceeds 128 objects", sandboxpolicy.ErrUnavailable)
	}
	for kind := range document.Platforms {
		if _, exists := document.Registrations[kind]; !exists {
			return fmt.Errorf("%w: platform has no matching Sandbox registration", sandboxpolicy.ErrUnavailable)
		}
	}
	for name, pin := range document.RuntimeClasses {
		if len(validation.IsDNS1123Subdomain(name)) != 0 || !sandboxPlatformUID(pin.UID) ||
			len(validation.IsDNS1123Label(pin.Handler)) != 0 {
			return fmt.Errorf("%w: RuntimeClass pins require a valid name, UID and handler", sandboxpolicy.ErrUnavailable)
		}
	}
	for name, pin := range document.StorageClasses {
		if len(validation.IsDNS1123Subdomain(name)) != 0 || !sandboxPlatformUID(pin.UID) || !sandboxPlatformValue(pin.Provisioner) {
			return fmt.Errorf("%w: StorageClass pins require a valid name, UID and provisioner", sandboxpolicy.ErrUnavailable)
		}
	}
	return nil
}

func validateSandboxPlatformIdentities(platform sandboxpolicy.Platform, capabilities sandbox.Capabilities) error {
	if err := sandboxpolicy.ValidateIdentityApprovals(platform.Identities); err != nil {
		return err
	}
	if len(platform.Grants) != 0 && !capabilities.KubernetesAccess {
		return fmt.Errorf("%w: grants require declared Kubernetes access support", sandboxpolicy.ErrUnapprovedGrant)
	}
	for _, identity := range platform.Identities {
		if identity.Kubernetes && !capabilities.KubernetesAccess {
			return fmt.Errorf("%w: identity approval requires declared Kubernetes access support", sandboxpolicy.ErrUnapprovedGrant)
		}
		if identity.Entra != nil && !capabilities.EntraID {
			return fmt.Errorf("%w: Entra identity approval requires declared support", sandboxpolicy.ErrUnapprovedGrant)
		}
	}
	if platform.TokenExpirationSeconds != 0 && (platform.TokenExpirationSeconds < 600 || platform.TokenExpirationSeconds > 3600) {
		return fmt.Errorf("%w: token lifetime must be between 600 and 3600 seconds", sandboxpolicy.ErrUnapprovedGrant)
	}
	if platform.KubernetesAudience != "" || len(platform.Grants) != 0 || len(platform.Identities) != 0 {
		if !sandboxPlatformValue(platform.KubernetesAudience) || platform.KubernetesAudience == sandboxpolicy.EntraAudience {
			return fmt.Errorf("%w: a distinct approved Kubernetes API audience is required", sandboxpolicy.ErrUnapprovedGrant)
		}
	}
	return nil
}

func validateSandboxPlatformGrants(grants []sandboxpolicy.ApprovedGrant, roleNames map[string]struct{}) error {
	for _, grant := range grants {
		if !sandboxPlatformUID(grant.PolicyUID) || !sandboxPlatformUID(grant.RoleUID) ||
			!sandboxPlatformValue(grant.RoleResourceVersion) || !sandboxPlatformValue(grant.Revision) || len(grant.Rules) == 0 ||
			grant.RoleRef.APIGroup != rbacv1.GroupName || grant.RoleRef.Kind != "ClusterRole" ||
			grant.RoleRef.Name == "" || len(pathvalidation.IsValidPathSegmentName(grant.RoleRef.Name)) != 0 ||
			(grant.Namespace != "" && len(validation.IsDNS1123Label(grant.Namespace)) != 0) {
			return fmt.Errorf("%w: grants require complete policy, ClusterRole UID/version/rules and scope pins", sandboxpolicy.ErrUnapprovedGrant)
		}
		roleNames[grant.RoleRef.Name] = struct{}{}
	}
	return nil
}

func addSandboxPlatformSource(sources map[string]sandboxworkload.SourceReference, source sandboxworkload.SourceReference) error {
	key := sandboxSourceKey(source)
	if !source.ValidIdentity() {
		return fmt.Errorf("%w: platform dependency identity is incomplete", sandboxpolicy.ErrUnavailable)
	}
	if previous, exists := sources[key]; exists {
		if previous != source {
			return fmt.Errorf("%w: platform dependency pins disagree", sandboxpolicy.ErrUnavailable)
		}
		return nil
	}
	if len(sources) == maxSandboxPlatformObjects {
		return fmt.Errorf("%w: platform object inventory exceeds 128 objects", sandboxpolicy.ErrUnavailable)
	}
	sources[key] = source
	return nil
}

func sandboxPlatformUID(uid types.UID) bool {
	return uid != "" && len(validation.IsValidLabelValue(string(uid))) == 0
}

func sandboxPlatformValue(value string) bool {
	return value != "" && strings.TrimSpace(value) == value
}

func sandboxSource(object client.Object, version, kind string) sandboxworkload.SourceReference {
	return sandboxworkload.SourceReference{
		APIVersion: version, Kind: kind, Namespace: object.GetNamespace(), Name: object.GetName(),
		UID: object.GetUID(), ResourceVersion: object.GetResourceVersion(),
	}
}

func sandboxSourceKey(source sandboxworkload.SourceReference) string {
	return source.APIVersion + "/" + source.Kind + "/" + source.Namespace + "/" + source.Name
}

func configurationNow(clock func() time.Time) time.Time {
	if clock != nil {
		return clock()
	}
	return time.Now()
}
