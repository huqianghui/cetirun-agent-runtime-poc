// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type customAccess struct {
	desktop      bool
	terminal     bool
	terminalPort uint32
}

func (r *AgentReconciler) configureCustomAccess(ctx context.Context, instance *api.Agent, access customAccess) {
	webUI := instance.Spec.Agent.WebUI
	instance.Status.AccessURL = ""
	status := metav1.ConditionFalse
	reason, message := "PlatformAccessDisabled", "Platform desktop, terminal and Agent Web UI are disabled"
	if access.desktop || access.terminal || webUI {
		if _, err := r.ensureVirtualService(ctx, instance, access); err != nil {
			reason, message = "PlatformRouteUnavailable", err.Error()
		} else {
			status = metav1.ConditionTrue
			reason, message = "PlatformServicesConfigured", "Approved platform routes configured; no native Agent UI or public Chat route"
			if webUI {
				instance.Status.AccessURL = accessURLPathBased(instance.Name, r.DomainSuffix)
				message = "Approved platform and Agent Web UI routes configured; no public Chat route"
				if !access.desktop && !access.terminal {
					reason = "PlatformWebUIConfigured"
				}
			} else if !access.terminal {
				reason = "PlatformDesktopConfigured"
			} else if !access.desktop {
				reason = "PlatformTerminalConfigured"
			}
		}
	} else if err := r.removeCustomPlatformRoute(ctx, instance); err != nil {
		reason, message = "PlatformRouteRemovalFailed", err.Error()
	}
	setCondition(&instance.Status.Conditions, api.ConditionIngressReady, status, reason, message)
	setCustomServiceConditions(instance, access, status, reason)
	if access.terminal && status == metav1.ConditionTrue {
		ready, err := r.customTerminalReady(ctx, instance)
		if err != nil || !ready {
			setCondition(&instance.Status.Conditions, api.ConditionTerminalConfigured, metav1.ConditionFalse,
				"PlatformTerminalPending", "Waiting for the owned terminal Deployment and shell check to become ready")
		}
	}
}

func setCustomServiceConditions(instance *api.Agent, access customAccess, ingress metav1.ConditionStatus, reason string) {
	for _, service := range []struct {
		condition string
		enabled   bool
	}{
		{api.ConditionDesktopConfigured, access.desktop},
		{api.ConditionTerminalConfigured, access.terminal},
	} {
		status := metav1.ConditionFalse
		if service.enabled && ingress == metav1.ConditionTrue {
			status = metav1.ConditionTrue
		}
		setCondition(&instance.Status.Conditions, service.condition, status, reason,
			"Reports platform routing configuration, not an interactive or model qualification")
	}
}
