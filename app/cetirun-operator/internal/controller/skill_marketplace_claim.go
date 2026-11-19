// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// errSkillsClaimPending reports that the skills claim of a common-executor
// Agent does not exist yet while the Skill Marketplace is enabled. The
// operator's Skill Marketplace claim controller creates that claim shortly
// after the operator bootstraps the Agent's protected namespace, so this is a
// short wait, not a configuration error or an unavailable dependency.
var errSkillsClaimPending = errors.New("the Skill Marketplace has not provided the Team skills claim yet")

const (
	skillsClaimPendingReason  = "SkillsMaterializationPending"
	skillsClaimPendingMessage = "Waiting for the Skill Marketplace to provide the Team skills claim"
	skillsClaimPendingRetry   = 5 * time.Second
)

// skillsClaimPending reports whether a failed read of the skills claim is the
// wait for the Skill Marketplace: the claim of a common-executor Agent, built-in
// or Custom, is absent. A terminating or foreign claim, any other read failure
// and the held native paths keep their existing errors.
func (p *KubernetesSandboxPrerequisites) skillsClaimPending(err error, reconciled bool) bool {
	return p.SkillMarketplace && reconciled && apierrors.IsNotFound(err)
}

// waitForSkillsClaim handles errSkillsClaimPending. It only records the wait
// and retries soon: the Agent keeps its phase and is neither held nor
// stopped, so its key, route and any running workload stay as they are, and
// nothing is installed until the claim exists.
func (r *AgentReconciler) waitForSkillsClaim(ctx context.Context, instance *api.Agent, cause error) (ctrl.Result, error) {
	log.FromContext(ctx).V(1).Info(skillsClaimPendingMessage, "cause", cause.Error(), "retryAfter", skillsClaimPendingRetry)
	previous := instance.Status.DeepCopy()
	// Prerequisite readiness carries the wait. Ready reports it too, unless a
	// running workload already made the Agent Ready: that is unchanged here.
	setCondition(&instance.Status.Conditions, builtInPrerequisitesReady, metav1.ConditionFalse,
		skillsClaimPendingReason, skillsClaimPendingMessage)
	if !isConditionTrue(instance.Status.Conditions, api.ConditionReady) {
		setCondition(&instance.Status.Conditions, api.ConditionReady, metav1.ConditionFalse,
			skillsClaimPendingReason, skillsClaimPendingMessage)
	}
	// Write only a change: each status write starts another reconcile.
	if !equality.Semantic.DeepEqual(*previous, instance.Status) {
		if err := r.updateStatus(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: skillsClaimPendingRetry}, nil
}

// endSkillsClaimWait replaces the conditions of a finished wait once the
// claim exists. The rest of the pass reports readiness as usual.
func endSkillsClaimWait(instance *api.Agent) {
	for _, kind := range []string{builtInPrerequisitesReady, api.ConditionReady} {
		if condition := meta.FindStatusCondition(instance.Status.Conditions, kind); condition != nil && condition.Reason == skillsClaimPendingReason {
			setCondition(&instance.Status.Conditions, kind, metav1.ConditionFalse, "PrerequisitesPending", "Agent prerequisites did not complete")
		}
	}
}
