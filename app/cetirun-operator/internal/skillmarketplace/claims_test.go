// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package skillmarketplace

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/go-logr/logr/funcr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func claimFixture(t *testing.T, protected bool, policy string) (*ClaimReconciler, client.Client) {
	t.Helper()
	agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "alice", UID: "agent-uid"}, Spec: api.AgentSpec{Team: "research"}}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-alice",
		Annotations: map[string]string{agentUIDAnnotation: "agent-uid", agentNameAnnotation: "alice"}}}
	if protected {
		ns.Labels = map[string]string{protectedLabel: "true"}
	}
	kube := newClient(t, agent, ns, team("research", "00000000-0000-4000-8000-000000000001", policy))
	return &ClaimReconciler{Client: kube, Reader: kube, ClaimName: "skills",
		Storage: StorageSource{ResourceGroup: "rg", StorageAccount: "account", ContainerName: "marketplace", StorageClassName: "azureblob-nfs-premium"}}, kube
}

var alice = reconcile.Request{NamespacedName: types.NamespacedName{Name: "alice"}}

func TestClaimIsCreatedForProtectedAgentNamespaces(t *testing.T) {
	r, kube := claimFixture(t, true, "policy")
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, alice); err != nil {
		t.Fatal(err)
	}
	claim := &corev1.PersistentVolumeClaim{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: "cetirun-alice", Name: "skills"}, claim); err != nil {
		t.Fatal(err)
	}
	if claim.Spec.VolumeName != VolumeName("agent-uid") || claim.OwnerReferences[0].Kind != "Agent" ||
		claim.Annotations[agentUIDAnnotation] != "agent-uid" || claim.Labels[api.SkillMarketplaceManagedLabel] != "true" ||
		claim.Annotations[api.SkillMarketplaceTeamUIDAnnotation] != "00000000-0000-4000-8000-000000000001" {
		t.Fatalf("claim %+v", claim)
	}
	volume := &corev1.PersistentVolume{}
	if err := kube.Get(ctx, client.ObjectKey{Name: VolumeName("agent-uid")}, volume); err != nil {
		t.Fatal(err)
	}
	csi := volume.Spec.CSI
	if !csi.ReadOnly || volume.Spec.MountOptions[0] != "ro" || csi.VolumeAttributes["containerName"] != "marketplace" ||
		csi.VolumeAttributes["storageAccount"] != "account" || csi.VolumeAttributes["protocol"] != "nfs" ||
		volume.Spec.ClaimRef.Name != "skills" || volume.Spec.ClaimRef.Namespace != "cetirun-alice" ||
		volume.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		t.Fatalf("volume %+v", volume.Spec)
	}
	// A second pass changes nothing.
	if _, err := r.Reconcile(ctx, alice); err != nil {
		t.Fatal(err)
	}
}

func TestClaimWaitsForTheAgentNamespace(t *testing.T) {
	agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "alice", UID: "agent-uid"}, Spec: api.AgentSpec{Team: "research"}}
	kube := newClient(t, agent, team("research", "00000000-0000-4000-8000-000000000001", "policy"))
	r := &ClaimReconciler{Client: kube, Reader: kube, ClaimName: "skills"}
	result, err := r.Reconcile(context.Background(), alice)
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("must try again until the namespace exists: %+v %v", result, err)
	}
}

func TestClaimSkipsOtherAgents(t *testing.T) {
	for name, fixture := range map[string]struct {
		protected bool
		policy    string
	}{
		"unprotected namespace": {false, "policy"},
		"Team without a policy": {true, ""},
	} {
		t.Run(name, func(t *testing.T) {
			r, kube := claimFixture(t, fixture.protected, fixture.policy)
			if _, err := r.Reconcile(context.Background(), alice); err != nil {
				t.Fatal(err)
			}
			claims := &corev1.PersistentVolumeClaimList{}
			if err := kube.List(context.Background(), claims); err != nil || len(claims.Items) != 0 {
				t.Fatalf("no claim may be created: %+v", claims.Items)
			}
		})
	}
}

func TestClaimLeavesAForeignClaimAlone(t *testing.T) {
	r, kube := claimFixture(t, true, "policy")
	ctx := context.Background()
	foreign := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "cetirun-alice", Name: "skills"}}
	if err := kube.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, alice); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(ctx, client.ObjectKey{Name: VolumeName("agent-uid")}, &corev1.PersistentVolume{}); err == nil {
		t.Fatal("no volume may be created for a claim that the marketplace did not create")
	}
}

func TestVolumeCollectorRemovesOrphans(t *testing.T) {
	r, kube := claimFixture(t, true, "policy")
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, alice); err != nil {
		t.Fatal(err)
	}
	collector := &VolumeCollector{Client: kube, Reader: kube}
	if err := collector.CollectOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(ctx, client.ObjectKey{Name: VolumeName("agent-uid")}, &corev1.PersistentVolume{}); err != nil {
		t.Fatalf("a bound volume must stay: %v", err)
	}
	if err := kube.Delete(ctx, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "cetirun-alice", Name: "skills"}}); err != nil {
		t.Fatal(err)
	}
	if err := collector.CollectOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(ctx, client.ObjectKey{Name: VolumeName("agent-uid")}, &corev1.PersistentVolume{}); err == nil {
		t.Fatalf("an orphaned volume must be removed")
	}
}

// uidFixture is claimFixture with a client that gives each created object a
// UID, as the API server does.
func uidFixture(t *testing.T) (*ClaimReconciler, client.Client) {
	t.Helper()
	r, base := claimFixture(t, true, "policy")
	var created atomic.Int32
	kube := interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if obj.GetUID() == "" {
				obj.SetUID(types.UID(fmt.Sprintf("uid-%d", created.Add(1))))
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	r.Client, r.Reader = kube, kube
	return r, kube
}

func agentVolume(t *testing.T, kube client.Client) *corev1.PersistentVolume {
	t.Helper()
	volume := &corev1.PersistentVolume{}
	if err := kube.Get(context.Background(), client.ObjectKey{Name: VolumeName("agent-uid")}, volume); err != nil {
		t.Fatal(err)
	}
	return volume
}

func agentClaim(t *testing.T, kube client.Client) *corev1.PersistentVolumeClaim {
	t.Helper()
	claim := &corev1.PersistentVolumeClaim{}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "cetirun-alice", Name: "skills"}, claim); err != nil {
		t.Fatal(err)
	}
	return claim
}

// reconcileUntilStable reconciles the Agent until a pass changes nothing, at
// most ten times, and returns the last result.
func reconcileUntilStable(t *testing.T, r *ClaimReconciler) reconcile.Result {
	t.Helper()
	state := func() string {
		claims := &corev1.PersistentVolumeClaimList{}
		volumes := &corev1.PersistentVolumeList{}
		if err := r.Reader.List(context.Background(), claims); err != nil {
			t.Fatal(err)
		}
		if err := r.Reader.List(context.Background(), volumes); err != nil {
			t.Fatal(err)
		}
		var objects []string
		for _, claim := range claims.Items {
			objects = append(objects, "claim/"+claim.Name+"/"+string(claim.UID)+"/"+claim.ResourceVersion)
		}
		for _, volume := range volumes.Items {
			objects = append(objects, "volume/"+volume.Name+"/"+string(volume.UID)+"/"+volume.ResourceVersion)
		}
		return strings.Join(objects, " ")
	}
	var result reconcile.Result
	for range 10 {
		before := state()
		var err error
		if result, err = r.Reconcile(context.Background(), alice); err != nil {
			t.Fatal(err)
		}
		if state() == before {
			return result
		}
	}
	t.Fatal("the claim did not settle")
	return result
}

// The volume of an earlier claim of the Agent can never bind its current
// claim. The reconciler replaces it, and examines the claim again until it is
// bound, instead of waiting for another Agent event.
func TestClaimReplacesTheVolumeOfAnEarlierClaim(t *testing.T) {
	r, kube := uidFixture(t)
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, alice); err != nil {
		t.Fatal(err)
	}
	earlier := agentClaim(t, kube)
	if err := kube.Delete(ctx, earlier); err != nil {
		t.Fatal(err)
	}
	result := reconcileUntilStable(t, r)
	claim, volume := agentClaim(t, kube), agentVolume(t, kube)
	if claim.UID == earlier.UID || volume.Spec.ClaimRef.UID != claim.UID {
		t.Fatalf("the volume must name the current claim %q: %q (earlier claim %q)", claim.UID, volume.Spec.ClaimRef.UID, earlier.UID)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("an unbound claim must be examined again")
	}
	claim.Status.Phase = corev1.ClaimBound
	if err := kube.Status().Update(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if result, err := r.Reconcile(ctx, alice); err != nil || result.RequeueAfter != 0 {
		t.Fatalf("a bound claim needs no further pass: %+v %v", result, err)
	}
}

// A claim that is being deleted, for example while its Pod still runs, stays
// until it is gone: the reconciler examines it again later.
func TestClaimWaitsWhileItsEarlierClaimIsDeleted(t *testing.T) {
	r, kube := uidFixture(t)
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, alice); err != nil {
		t.Fatal(err)
	}
	claim := agentClaim(t, kube)
	claim.Finalizers = []string{"kubernetes.io/pvc-protection"}
	if err := kube.Update(ctx, claim); err != nil {
		t.Fatal(err)
	}
	claim.Status.Phase = corev1.ClaimBound
	if err := kube.Status().Update(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if err := kube.Delete(ctx, claim); err != nil {
		t.Fatal(err)
	}
	volume := agentVolume(t, kube)
	result, err := r.Reconcile(ctx, alice)
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("a terminating claim must be examined again: %+v %v", result, err)
	}
	if got := agentVolume(t, kube); got.ResourceVersion != volume.ResourceVersion {
		t.Fatal("the volume must stay while its claim is terminating")
	}
}

// When the configured storage changes, an Agent's claim and volume move to
// it: the reconciler deletes the claim of the earlier storage, and creates a
// claim and volume of the configured one.
func TestClaimMovesToTheConfiguredStorage(t *testing.T) {
	r, kube := uidFixture(t)
	if _, err := r.Reconcile(context.Background(), alice); err != nil {
		t.Fatal(err)
	}
	earlier := agentClaim(t, kube)
	r.Storage.ContainerName, r.Storage.StorageAccount = "moved", "otheraccount"
	reconcileUntilStable(t, r)
	claim, volume := agentClaim(t, kube), agentVolume(t, kube)
	attributes := volume.Spec.CSI.VolumeAttributes
	if claim.UID == earlier.UID || volume.Spec.ClaimRef.UID != claim.UID ||
		attributes["containerName"] != "moved" || attributes["storageAccount"] != "otheraccount" {
		t.Fatalf("the Agent must move to the configured storage: claim %q (earlier %q), volume %+v", claim.UID, earlier.UID, volume.Spec)
	}
	r.Storage.StorageClassName = "azureblob-nfs-standard"
	reconcileUntilStable(t, r)
	if claim := agentClaim(t, kube); *claim.Spec.StorageClassName != "azureblob-nfs-standard" ||
		agentVolume(t, kube).Spec.StorageClassName != "azureblob-nfs-standard" {
		t.Fatalf("the Agent must move to the configured storage class: %+v", claim.Spec)
	}
}

// The messages about a claim that the reconciler leaves alone appear once,
// not at each Agent event.
func TestClaimMessagesAreLoggedOnce(t *testing.T) {
	r, kube := claimFixture(t, true, "policy")
	var lines []string
	ctx := log.IntoContext(context.Background(), funcr.New(func(_, args string) { lines = append(lines, args) }, funcr.Options{}))
	foreign := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "cetirun-alice", Name: "skills", UID: "foreign"}}
	if err := kube.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := r.Reconcile(ctx, alice); err != nil {
			t.Fatal(err)
		}
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "did not create; leaving it") {
		t.Fatalf("the foreign claim must be logged once: %q", lines)
	}
}
