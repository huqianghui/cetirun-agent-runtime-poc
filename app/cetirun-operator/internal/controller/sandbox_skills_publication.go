// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	strictjson "sigs.k8s.io/json"
)

const (
	SandboxSkillsCatalogKey      = "skills.json"
	MaxSandboxSkillsCatalogBytes = 64 * 1024
)

type sandboxSkillsCatalog struct {
	Version string                              `json:"version"`
	Teams   map[string]sandboxSkillsPublication `json:"teams"`
}

type sandboxSkillsPublication struct {
	TeamUID  types.UID              `json:"teamUID"`
	Revision string                 `json:"revision"`
	Source   sandboxSkillsNFSSource `json:"source"`
}

// Only already-published Blob NFS content is supported, without credentials,
// caller-selected PVs, dynamic provisioning or configurable mount commands.
type sandboxSkillsNFSSource struct {
	ResourceGroup    string `json:"resourceGroup"`
	StorageAccount   string `json:"storageAccount"`
	ContainerName    string `json:"containerName"`
	StorageClassName string `json:"storageClassName"`
	StorageSize      string `json:"storageSize"`
}

type sandboxBootstrapSkills struct {
	Catalog     sandboxworkload.SourceReference `json:"catalog"`
	TeamName    string                          `json:"teamName"`
	Publication sandboxSkillsPublication        `json:"publication"`
}

var (
	sandboxPublicationToken = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
	sandboxResourceGroup    = regexp.MustCompile(`^[a-zA-Z0-9_().-]{1,90}$`)
	sandboxStorageAccount   = regexp.MustCompile(`^[a-z0-9]{3,24}$`)
)

func decodeSandboxSkillsCatalog(raw string) (sandboxSkillsCatalog, error) {
	var document sandboxSkillsCatalog
	if len(raw) == 0 || len(raw) > MaxSandboxSkillsCatalogBytes || !utf8.ValidString(raw) {
		return document, fmt.Errorf("%w: skills.json must be nonempty UTF-8 JSON of at most 64 KiB", sandboxpolicy.ErrUnavailable)
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	depth, tokens := 0, 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil || token == nil {
			return document, fmt.Errorf("%w: malformed or null skills catalog JSON", sandboxpolicy.ErrUnavailable)
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
		if depth > 5 || tokens > 8192 {
			return document, fmt.Errorf("%w: skills catalog nesting or token budget exceeded", sandboxpolicy.ErrUnavailable)
		}
	}
	problems, err := strictjson.UnmarshalStrict([]byte(raw), &document)
	if err != nil || len(problems) != 0 || document.Version != "v1" || document.Teams == nil || len(document.Teams) > 128 {
		return document, fmt.Errorf("%w: skills catalog requires version v1 and at most 128 unique Teams, without unknown or duplicate fields", sandboxpolicy.ErrUnavailable)
	}
	for name, publication := range document.Teams {
		if len(validation.IsDNS1123Subdomain(name)) != 0 {
			return document, fmt.Errorf("%w: skills catalog Team name is invalid", sandboxpolicy.ErrUnavailable)
		}
		if err := validateSandboxSkillsPublication(publication); err != nil {
			return document, err
		}
	}
	return document, nil
}

func validateSandboxSkillsPublication(publication sandboxSkillsPublication) error {
	source := publication.Source
	size, err := resource.ParseQuantity(source.StorageSize)
	if !sandboxPublicationToken.MatchString(string(publication.TeamUID)) || !sandboxPublicationToken.MatchString(publication.Revision) ||
		!sandboxResourceGroup.MatchString(source.ResourceGroup) || strings.HasSuffix(source.ResourceGroup, ".") ||
		!sandboxStorageAccount.MatchString(source.StorageAccount) ||
		len(source.ContainerName) < 3 || len(validation.IsDNS1123Label(source.ContainerName)) != 0 ||
		strings.Contains(source.ContainerName, "--") || len(validation.IsDNS1123Subdomain(source.StorageClassName)) != 0 ||
		err != nil || size.Sign() <= 0 {
		return fmt.Errorf("%w: skills publication requires an exact Team UID, revision and valid non-secret Blob NFS storage descriptor", sandboxpolicy.ErrUnavailable)
	}
	return nil
}

func (r *AgentReconciler) readSandboxSkillsPublication(ctx context.Context, name string, team *api.Team) (*sandboxBootstrapSkills, error) {
	if r.APIReader == nil || len(validation.IsDNS1123Label(r.OperatorNamespace)) != 0 ||
		len(validation.IsDNS1123Subdomain(name)) != 0 || team == nil || team.UID == "" {
		return nil, fmt.Errorf("%w: skills publication requires a trusted catalog location and current Team identity", sandboxpolicy.ErrUnavailable)
	}
	catalog := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: r.OperatorNamespace, Name: name}
	if err := r.APIReader.Get(ctx, key, catalog); err != nil {
		return nil, fmt.Errorf("read Sandbox skills publication catalog: %w", err)
	}
	if client.ObjectKeyFromObject(catalog) != key || catalog.UID == "" || catalog.ResourceVersion == "" ||
		!catalog.DeletionTimestamp.IsZero() || len(catalog.Data) != 1 || len(catalog.BinaryData) != 0 {
		return nil, fmt.Errorf("%w: skills catalog must be a persisted non-terminating ConfigMap containing only skills.json", sandboxpolicy.ErrUnavailable)
	}
	document, err := decodeSandboxSkillsCatalog(catalog.Data[SandboxSkillsCatalogKey])
	if err != nil {
		return nil, err
	}
	publication, found := document.Teams[team.Name]
	if !found || publication.TeamUID != team.UID {
		return nil, fmt.Errorf("%w: no skills publication is approved for the exact Team name and UID", sandboxpolicy.ErrUnavailable)
	}
	return &sandboxBootstrapSkills{
		Catalog: sandboxSource(catalog, "v1", "ConfigMap"), TeamName: team.Name, Publication: publication,
	}, nil
}

func sameSandboxSkillsPublication(a, b sandboxBootstrapSkills) bool {
	// Other Teams and catalog metadata may change; the selected publication and
	// catalog UID cannot. Each attempt still guards the current resourceVersion.
	a.Catalog.ResourceVersion, b.Catalog.ResourceVersion = "", ""
	return a == b
}

func sandboxSkillsVolumeName(agentUID types.UID) string {
	sum := sha256.Sum256([]byte(agentUID))
	return "sandbox-skills-" + hex.EncodeToString(sum[:20])
}

func sandboxSkillsBootstrapObjects(receipt *sandboxBootstrapReceipt) []client.Object {
	source := receipt.Skills.Publication.Source
	size := resource.MustParse(source.StorageSize) // Validated before recording or reading the receipt.
	claim, volume := receipt.Objects[5], receipt.Objects[6]
	// Match the existing static Blob NFS skills renderer, with enforced
	// read-only CSI/mount options and a UID-prebound claim instead of name adoption.
	return []client.Object{
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: claim.Name, Namespace: receipt.Namespace},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
				Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceStorage: size,
				}},
				StorageClassName: ptr.To(source.StorageClassName), VolumeMode: ptr.To(corev1.PersistentVolumeFilesystem),
				VolumeName: volume.Name,
			},
		},
		&corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: volume.Name},
			Spec: corev1.PersistentVolumeSpec{
				Capacity:                      corev1.ResourceList{corev1.ResourceStorage: size},
				AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
				PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
				StorageClassName:              source.StorageClassName, VolumeMode: ptr.To(corev1.PersistentVolumeFilesystem),
				MountOptions: []string{"ro"},
				ClaimRef: &corev1.ObjectReference{
					APIVersion: "v1", Kind: "PersistentVolumeClaim", Namespace: receipt.Namespace, Name: claim.Name, UID: claim.UID,
				},
				PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
					Driver: "blob.csi.azure.com", VolumeHandle: volume.Name, ReadOnly: true,
					VolumeAttributes: map[string]string{
						"resourceGroup": source.ResourceGroup, "storageAccount": source.StorageAccount,
						"containerName": source.ContainerName, "protocol": "nfs",
					},
				}},
			},
		},
	}
}

func validateSandboxSkillsVolume(current, desired *corev1.PersistentVolume) error {
	actual := current.Spec.DeepCopy()
	if actual.ClaimRef != nil {
		actual.ClaimRef.ResourceVersion = ""
	}
	if desired.Spec.ClaimRef.UID == "" || !equality.Semantic.DeepEqual(*actual, desired.Spec) {
		return fmt.Errorf("%w: published skills PV no longer matches its retained read-only source and exact claim UID; recovery is required", sandboxworkload.ErrStorageTransitionRequired)
	}
	return nil
}

func (p *KubernetesSandboxPrerequisites) publishedSkillsSources(ctx context.Context, agent *api.Agent, team *api.Team,
	namespace *corev1.Namespace, home, skills *corev1.PersistentVolumeClaim, secret client.Object,
) ([]sandboxworkload.SourceReference, error) {
	if namespace.Annotations[sandboxBootstrapAnnotation] == "" &&
		!slices.Contains(agent.Finalizers, sandbox.BootstrapFinalizer) &&
		(skills == nil || skills.Annotations[sandboxBootstrapAnnotation] == "") {
		return nil, nil
	}
	if len(validation.IsDNS1123Label(p.OperatorNamespace)) != 0 {
		return nil, fmt.Errorf("%w: bootstrap prerequisites require their configured operator state namespace", sandboxpolicy.ErrUnavailable)
	}
	reader := &AgentReconciler{APIReader: p.APIReader, OperatorNamespace: p.OperatorNamespace}
	object, receipt, err := reader.readSandboxBootstrap(ctx, agent)
	if err != nil {
		return nil, err
	}
	if receipt.Skills == nil {
		return nil, nil
	}
	if skills == nil {
		return nil, fmt.Errorf("%w: recorded skills publication requires its skills PVC; empty skills cannot replace it", sandboxworkload.ErrStorageTransitionRequired)
	}
	if p.SkillsCatalogConfigMapName != receipt.Skills.Catalog.Name {
		return nil, fmt.Errorf("%w: recorded skills catalog selection changed; recovery is required", sandboxworkload.ErrStorageTransitionRequired)
	}
	publication, err := reader.readSandboxSkillsPublication(ctx, p.SkillsCatalogConfigMapName, team)
	if err != nil {
		return nil, fmt.Errorf("recorded skills publication is unavailable; recovery is required: %w", err)
	}
	if !sameSandboxSkillsPublication(*publication, *receipt.Skills) || skills.Name != receipt.Objects[5].Name {
		return nil, fmt.Errorf("%w: recorded skills publication changed; recovery is required", sandboxworkload.ErrStorageTransitionRequired)
	}
	volume := &corev1.PersistentVolume{}
	if err := p.readPrerequisite(ctx, client.ObjectKey{Name: receipt.Objects[6].Name}, volume); err != nil {
		return nil, err
	}
	desired := reader.sandboxBootstrapObjects(agent, object, receipt)
	for _, item := range []struct {
		index  int
		object client.Object
	}{{0, namespace}, {3, home}, {4, secret}, {5, skills}, {6, volume}} {
		if receipt.Objects[item.index].UID == "" {
			return nil, fmt.Errorf("Sandbox published skills bootstrap identities are incomplete")
		}
		if err := validateSandboxBootstrapObject(item.object, desired[item.index], receipt.Objects[item.index].UID, false); err != nil {
			return nil, err
		}
	}
	return []sandboxworkload.SourceReference{
		publication.Catalog, sandboxSource(object, "v1", "ConfigMap"), sandboxSource(volume, "v1", "PersistentVolume"),
	}, nil
}
