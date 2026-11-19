// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"fmt"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/egresscapture"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/ingressmtls"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
)

func sandboxEgressSNIInputs(attempt *sandboxPreparationAttempt) error {
	if len(attempt.resolution.AgentPolicy.Network.BlockedFQDNs) == 0 {
		return nil
	}
	port := attempt.agent.Spec.Agent.Port
	if !attempt.reconciled || port == nil && attempt.agent.Spec.Agent.Type != api.AgentTypeCustom ||
		port != nil && (*port == egresscapture.CapturePort || *port == ingressmtls.AdminPort) {
		return fmt.Errorf("%w: observed-SNI capture requires a nonconflicting common-executor application port", sandboxpolicy.ErrUnsupportedControl)
	}
	return nil
}
