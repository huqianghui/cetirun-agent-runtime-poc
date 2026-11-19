// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"fmt"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// protectedPolicyBudget prevents ordinary Kubernetes writers from
// changing a policy-bounded spend limit on an active Agent. Team selection is
// direct: any caller otherwise authorized to create an Agent may select a Team.
func (h *Handler) protectedPolicyBudget(request admission.Request, agent, old *api.Agent) error {
	if old == nil {
		return nil
	}
	if monthlyBudgetUSD(old) != monthlyBudgetUSD(agent) {
		if !h.config.administrator(request.UserInfo) || !old.Spec.Suspended || !agent.Spec.Suspended {
			return fmt.Errorf("policy budget changes require an authorized administrator and a suspended Agent")
		}
	}
	return nil
}

func monthlyBudgetUSD(agent *api.Agent) string {
	if agent == nil || agent.Spec.ModelAccess == nil {
		return ""
	}
	return agent.Spec.ModelAccess.MonthlyBudgetUSD
}
