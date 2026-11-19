// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package skillmarketplace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	agentNameAnnotation = "cetirun.microsoft.com/agent-name"
	agentUIDAnnotation  = "cetirun.microsoft.com/agent-uid"
	protectedLabel      = "cetirun.microsoft.com/sandbox-protected"
	namespacePrefix     = "cetirun-"
)

// claimSize is the nominal size of each Agent's claim and volume. Blob NFS
// does not enforce it.
var claimSize = resource.MustParse("1Gi")

// StorageSource is the Blob NFS container that holds the Team releases.
type StorageSource struct {
	ResourceGroup    string
	StorageAccount   string
	ContainerName    string
	StorageClassName string
}

// ClaimReconciler gives each Agent of a policy-bound Team a read-only claim of
// the release storage, in the Agent's protected namespace. The Agent mounts
// only its Team's subtree. The prerequisites of the Agent wait for the claim.
//
// It replaces a volume that an earlier claim of the Agent left, and the claim
// and volume of an Agent whose volume names other storage than the configured
// one. While the Agent runs, its claim stays until the operator stops the
// Agent, which then starts again with the new claim.
type ClaimReconciler struct {
	// Client reads Agents and Teams from the cache and creates the objects.
	Client client.Client
	// Reader reads namespaces, claims and volumes, which the operator does
	// not cache.
	Reader  client.Reader
	Storage StorageSource
	// ClaimName is the operator's sandboxSources.skillsClaimName.
	ClaimName string

	// logged holds the messages that are logged once for each claim.
	logged sync.Map
}

// claimRetry is how soon the reconciler examines a claim again that is not
// bound yet, or whose earlier claim or volume is going away.
const claimRetry = 10 * time.Second

// VolumeName is the cluster-scoped PersistentVolume of one Agent.
func VolumeName(agentUID types.UID) string {
	sum := sha256.Sum256([]byte(agentUID))
	return "skills-mp-" + hex.EncodeToString(sum[:])[:20]
}

// logOnce logs a message about a claim once for each operator process.
func (r *ClaimReconciler) logOnce(logger logr.Logger, claim *corev1.PersistentVolumeClaim, message string, keysAndValues ...any) {
	if _, seen := r.logged.LoadOrStore(string(claim.UID)+"\x00"+message, true); !seen {
		logger.Info(message, keysAndValues...)
	}
}

// Reconcile ensures the claim of one Agent.
func (r *ClaimReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx).WithValues("agent", req.Name)
	agent := &api.Agent{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: req.Name}, agent); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if !agent.DeletionTimestamp.IsZero() || agent.Spec.Team == "" {
		return reconcile.Result{}, nil
	}
	team := &api.Team{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: agent.Spec.Team}, team); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if team.Spec.AgentPolicyRef == "" {
		return reconcile.Result{}, nil
	}
	namespace := &corev1.Namespace{}
	if err := r.Reader.Get(ctx, client.ObjectKey{Name: namespacePrefix + agent.Name}, namespace); err != nil {
		if apierrors.IsNotFound(err) {
			// The operator bootstraps the namespace; its next Agent update or
			// this retry creates the claim.
			return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
		}
		return reconcile.Result{}, err
	}
	if !namespace.DeletionTimestamp.IsZero() || namespace.Labels[protectedLabel] != "true" ||
		namespace.Annotations[agentUIDAnnotation] != string(agent.UID) {
		return reconcile.Result{}, nil
	}
	volume := &corev1.PersistentVolume{}
	err := r.Reader.Get(ctx, client.ObjectKey{Name: VolumeName(agent.UID)}, volume)
	if apierrors.IsNotFound(err) {
		volume = nil
	} else if err != nil {
		return reconcile.Result{}, err
	}
	claim := &corev1.PersistentVolumeClaim{}
	err = r.Reader.Get(ctx, client.ObjectKey{Namespace: namespace.Name, Name: r.ClaimName}, claim)
	switch {
	case apierrors.IsNotFound(err):
		claim = nil
	case err != nil:
		return reconcile.Result{}, err
	case claim.Labels[api.SkillMarketplaceManagedLabel] != "true":
		r.logOnce(logger, claim, "the namespace already has a skills claim that the Skill Marketplace did not create; leaving it", "namespace", namespace.Name)
		return reconcile.Result{}, nil
	case !claim.DeletionTimestamp.IsZero():
		// A running Agent keeps its claim until the operator stops it.
		return reconcile.Result{RequeueAfter: claimRetry}, nil
	}
	if volume != nil && volume.DeletionTimestamp.IsZero() && claim != nil && r.ownVolume(volume, claim) && !r.currentStorage(volume) {
		// The storage changed. The Agent moves to it with a new claim; the
		// earlier volume goes once the claim is gone.
		if err := r.Client.Delete(ctx, claim, client.Preconditions{UID: &claim.UID, ResourceVersion: &claim.ResourceVersion}); client.IgnoreNotFound(err) != nil {
			return reconcile.Result{}, fmt.Errorf("replace the skills claim of earlier storage: %w", err)
		}
		logger.Info("replacing the Team skills claim, whose volume uses earlier storage; the Agent restarts once with the configured storage",
			"namespace", namespace.Name, "volume", volume.Name)
		return reconcile.Result{RequeueAfter: claimRetry}, nil
	}
	if claim == nil {
		claim = r.desiredClaim(agent, team, namespace.Name)
		if err := r.Client.Create(ctx, claim); err != nil {
			return reconcile.Result{}, fmt.Errorf("create the skills claim: %w", err)
		}
		logger.Info("created the Team skills claim", "namespace", namespace.Name, "team", team.Name)
	}
	if claim.Annotations[api.SkillMarketplaceTeamUIDAnnotation] != string(team.UID) {
		r.logOnce(logger, claim, "the Agent's Team changed after its skills claim was created; the Agent needs recreation", "namespace", namespace.Name)
	}
	if volume != nil && !r.ownVolume(volume, claim) {
		// A volume of an earlier claim of the Agent. It can never bind the
		// current claim, and no Pod uses it: the namespace has no other
		// claim of that name. The container data stays.
		if volume.DeletionTimestamp.IsZero() {
			if err := r.Client.Delete(ctx, volume, client.Preconditions{UID: &volume.UID, ResourceVersion: &volume.ResourceVersion}); client.IgnoreNotFound(err) != nil {
				return reconcile.Result{}, fmt.Errorf("remove the skills volume of an earlier claim: %w", err)
			}
			logger.Info("removed the Team skills volume of an earlier claim", "volume", volume.Name)
		}
		return reconcile.Result{RequeueAfter: 2 * time.Second}, nil
	}
	if volume == nil {
		volume = r.desiredVolume(agent, claim)
		if err := r.Client.Create(ctx, volume); err != nil {
			return reconcile.Result{}, fmt.Errorf("create the skills volume: %w", err)
		}
		logger.Info("created the Team skills volume", "volume", volume.Name)
	}
	if claim.Status.Phase != corev1.ClaimBound {
		// Kubernetes binds the claim to its volume; nothing else reconciles
		// the Agent soon if the binding needs another pass.
		return reconcile.Result{RequeueAfter: claimRetry}, nil
	}
	return reconcile.Result{}, nil
}

// ownVolume reports whether the volume was created for the claim.
func (r *ClaimReconciler) ownVolume(volume *corev1.PersistentVolume, claim *corev1.PersistentVolumeClaim) bool {
	ref := volume.Spec.ClaimRef
	return ref != nil && ref.Namespace == claim.Namespace && ref.Name == claim.Name && ref.UID == claim.UID
}

// currentStorage reports whether the volume names the configured storage.
func (r *ClaimReconciler) currentStorage(volume *corev1.PersistentVolume) bool {
	csi := volume.Spec.CSI
	if csi == nil || volume.Spec.StorageClassName != r.Storage.StorageClassName {
		return false
	}
	attributes := csi.VolumeAttributes
	return attributes["resourceGroup"] == r.Storage.ResourceGroup && attributes["storageAccount"] == r.Storage.StorageAccount &&
		attributes["containerName"] == r.Storage.ContainerName
}

func labels() map[string]string {
	return map[string]string{api.SkillMarketplaceManagedLabel: "true", "app.kubernetes.io/managed-by": managedBy}
}

func (r *ClaimReconciler) annotations(agent *api.Agent, team *api.Team) map[string]string {
	return map[string]string{
		agentNameAnnotation: agent.Name, agentUIDAnnotation: string(agent.UID),
		api.SkillMarketplaceTeamUIDAnnotation: string(team.UID),
	}
}

func (r *ClaimReconciler) desiredClaim(agent *api.Agent, team *api.Team, namespace string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: r.ClaimName, Namespace: namespace, Labels: labels(), Annotations: r.annotations(agent, team),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: agent.Name, UID: agent.UID,
			}},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: claimSize}},
			StorageClassName: ptr.To(r.Storage.StorageClassName),
			VolumeMode:       ptr.To(corev1.PersistentVolumeFilesystem),
			VolumeName:       VolumeName(agent.UID),
		},
	}
}

func (r *ClaimReconciler) desiredVolume(agent *api.Agent, claim *corev1.PersistentVolumeClaim) *corev1.PersistentVolume {
	annotations := map[string]string{}
	for key, value := range claim.Annotations {
		annotations[key] = value
	}
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: VolumeName(agent.UID), Labels: labels(), Annotations: annotations},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: claimSize},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			StorageClassName:              r.Storage.StorageClassName,
			VolumeMode:                    ptr.To(corev1.PersistentVolumeFilesystem),
			MountOptions:                  []string{"ro"},
			ClaimRef: &corev1.ObjectReference{
				APIVersion: "v1", Kind: "PersistentVolumeClaim", Namespace: claim.Namespace, Name: claim.Name, UID: claim.UID,
			},
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
				Driver: "blob.csi.azure.com", VolumeHandle: VolumeName(agent.UID), ReadOnly: true,
				VolumeAttributes: map[string]string{
					"resourceGroup": r.Storage.ResourceGroup, "storageAccount": r.Storage.StorageAccount,
					"containerName": r.Storage.ContainerName, "protocol": "nfs",
				},
			}},
		},
	}
}

// SetupWithManager reconciles each Agent. A new Agent's namespace appears
// while the operator prepares the Agent, which updates the Agent's status.
func (r *ClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("skill-marketplace-claims").
		For(&api.Agent{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

// VolumeCollector deletes the marketplace volumes whose claim is gone.
type VolumeCollector struct {
	Client   client.Client
	Reader   client.Reader
	Interval time.Duration
}

// NeedLeaderElection keeps one collector.
func (c *VolumeCollector) NeedLeaderElection() bool { return true }

// Start runs the collection loop.
func (c *VolumeCollector) Start(ctx context.Context) error {
	ticker := time.NewTicker(c.Interval)
	defer ticker.Stop()
	for {
		if err := c.CollectOnce(ctx); err != nil {
			log.FromContext(ctx).Error(err, "remove unused skills volumes")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// CollectOnce deletes released or orphaned marketplace volumes.
func (c *VolumeCollector) CollectOnce(ctx context.Context) error {
	volumes := &corev1.PersistentVolumeList{}
	if err := c.Reader.List(ctx, volumes, client.MatchingLabels{api.SkillMarketplaceManagedLabel: "true"}); err != nil {
		return err
	}
	for i := range volumes.Items {
		volume := &volumes.Items[i]
		if !volume.DeletionTimestamp.IsZero() {
			continue
		}
		orphan := volume.Status.Phase == corev1.VolumeReleased
		if ref := volume.Spec.ClaimRef; !orphan && ref != nil {
			claim := &corev1.PersistentVolumeClaim{}
			err := c.Reader.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, claim)
			orphan = apierrors.IsNotFound(err) || (err == nil && ref.UID != "" && claim.UID != ref.UID)
		}
		if !orphan {
			continue
		}
		if err := c.Client.Delete(ctx, volume, client.Preconditions{UID: &volume.UID, ResourceVersion: &volume.ResourceVersion}); client.IgnoreNotFound(err) != nil {
			return err
		}
		log.FromContext(ctx).Info("removed an unused skills volume", "volume", volume.Name)
	}
	return nil
}
