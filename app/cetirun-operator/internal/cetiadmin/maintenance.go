// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cetiadmin

import api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"

const (
	MaintenanceAgentName = "cetirun.microsoft.com/helm-maintenance-agent"
	MaintenanceAgentUID  = "cetirun.microsoft.com/helm-maintenance-agent-uid"
	MaintenanceRequestID = "cetirun.microsoft.com/helm-maintenance-request-id"
)

func MaintenanceHeld(agent *api.Agent) bool {
	request, status := agent.Spec.Helm.Maintenance, agent.Status.HelmMaintenance
	return agent.UID != "" && agent.DeletionTimestamp == nil && agent.Spec.Suspended && request != nil && status != nil &&
		status.Phase == api.HelmMaintenancePhaseHeld && status.RequestID == request.RequestID &&
		status.ObservedGeneration == agent.Generation && status.Revision == request.ExpectedRevision && status.Revision > 0
}

func MaintenanceUnfinished(agent *api.Agent) bool {
	return agent.Spec.Helm.Maintenance != nil || (agent.Status.HelmMaintenance != nil && agent.Status.HelmMaintenance.Phase != api.HelmMaintenancePhaseReleased)
}
