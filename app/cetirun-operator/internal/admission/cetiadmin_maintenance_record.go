// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type cetiAdminMaintenanceRecord struct {
	Version           int                        `json:"version"`
	Target            cetiadmin.TargetIdentity   `json:"target"`
	Request           api.HelmMaintenanceRequest `json:"request"`
	RequestGeneration int64                      `json:"requestGeneration"`
	IntentHash        string                     `json:"intentHash"`
	Phase             api.HelmMaintenancePhase   `json:"phase"`
	ApplyRevision     int                        `json:"applyRevision"`
	PreparedHash      string                     `json:"preparedHash"`
	Resumed           bool                       `json:"resumed"`
	Restoration       *struct {
		Revision     int    `json:"revision"`
		ManifestHash string `json:"manifestHash"`
		Objects      []struct {
			cetiadmin.ManagedResource
			PreviousUID types.UID `json:"previousUID,omitempty"`
		} `json:"objects"`
	} `json:"restoration"`
}

func (h *CetiAdminHandler) readCetiAdminMaintenance(ctx context.Context, agent *api.Agent) (cetiAdminMaintenanceRecord, error) {
	var record cetiAdminMaintenanceRecord
	hash := sha256.Sum256([]byte(agent.UID))
	checkpoint := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: h.config.CetiAdmin.Namespace, Name: "cetiadmin-maintenance-" + hex.EncodeToString(hash[:16])}
	if err := h.reader.Get(ctx, key, checkpoint); err != nil {
		return record, err
	}
	if checkpoint.UID == "" || checkpoint.DeletionTimestamp != nil || !cetiAdminAgentOwner(checkpoint, agent) ||
		len(checkpoint.Data) != 1 || len(checkpoint.BinaryData) != 0 || len(checkpoint.Data["maintenance.json"]) > 2<<20 ||
		decodeJSON([]byte(checkpoint.Data["maintenance.json"]), &record, false) != nil || record.Version != 1 ||
		record.Target.Agent != (cetiadmin.ObjectIdentity{Name: agent.Name, UID: agent.UID}) || record.Target.Namespace.UID == "" ||
		record.Target.Release != "openclaw-"+agent.Name || record.Request.RequestID == "" || record.Request.ExpectedRevision < 1 ||
		record.RequestGeneration < 1 || record.IntentHash == "" {
		return record, fmt.Errorf("Helm maintenance checkpoint is unbound or invalid")
	}
	return record, nil
}

func sameCetiAdminMaintenanceTarget(saved, current cetiadmin.TargetIdentity) bool {
	saved.Resources, current.Resources = nil, nil
	return reflect.DeepEqual(saved, current)
}

func (h *CetiAdminHandler) heldMaintenanceTarget(ctx context.Context, agent *api.Agent, target cetiadmin.TargetIdentity) (cetiadmin.TargetIdentity, error) {
	if target.Protected || !cetiadmin.MaintenanceHeld(agent) {
		return cetiadmin.TargetIdentity{}, fmt.Errorf("Helm writes require a current Held ordinary runtime")
	}
	record, err := h.readCetiAdminMaintenance(ctx, agent)
	if err != nil {
		return cetiadmin.TargetIdentity{}, err
	}
	if record.Phase != api.HelmMaintenancePhaseHeld || record.Resumed || record.Request != *agent.Spec.Helm.Maintenance ||
		record.RequestGeneration != agent.Generation || !sameCetiAdminMaintenanceTarget(record.Target, target) {
		return cetiadmin.TargetIdentity{}, fmt.Errorf("Helm maintenance snapshot does not match the current request and target")
	}
	namespace := &corev1.Namespace{}
	if err := h.reader.Get(ctx, client.ObjectKey{Name: target.Namespace.Name}, namespace); err != nil {
		return cetiadmin.TargetIdentity{}, err
	}
	if namespace.UID != target.Namespace.UID || namespace.DeletionTimestamp != nil ||
		namespace.Labels["app.kubernetes.io/managed-by"] != "cetirun-operator" ||
		namespace.Annotations[cetiadmin.MaintenanceAgentName] != agent.Name ||
		namespace.Annotations[cetiadmin.MaintenanceAgentUID] != string(agent.UID) ||
		namespace.Annotations[cetiadmin.MaintenanceRequestID] != record.Request.RequestID {
		return cetiadmin.TargetIdentity{}, fmt.Errorf("Helm maintenance startup markers do not match the snapshot")
	}
	return record.Target, nil
}
