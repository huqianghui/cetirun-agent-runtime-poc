// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"github.com/Azure/CetiRun/app/internal/chataccess"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (r *AgentReconciler) configureReconciledCustomRuntime(ctx context.Context, instance *api.Agent, prepared *helm.PreparedRelease) error {
	access, err := reconciledCustomAccess(instance, prepared)
	if err != nil {
		return err
	}
	resolution, err := r.resolvePolicy(ctx, instance)
	if err != nil {
		return err
	}
	disabled := resolution.policy.Spec.Network.Ingress.Mode == api.AgentPolicyIngressDisabled
	chat := !disabled && r.ChatAccess.Enabled && len(r.ChatAccess.Callers) != 0 &&
		instance.Spec.Agent.Port != nil && !instance.Spec.Agent.WebUI
	if chat && !prepared.SupportsPortalChat() {
		return fmt.Errorf("the Custom chart does not support private Portal Chat")
	}
	if err := r.reconcileChatRoute(ctx, instance, chat); err != nil {
		setCondition(&instance.Status.Conditions, chataccess.Condition, metav1.ConditionFalse,
			"ChatRouteUnavailable", "The private Chat route could not be reconciled")
		return err
	}
	status, reason := metav1.ConditionFalse, "ChatAccessDisabled"
	if chat {
		status, reason = metav1.ConditionTrue, "ChatAccessConfigured"
	}
	setCondition(&instance.Status.Conditions, chataccess.Condition, status, reason,
		"Reports private Chat configuration, not model or network qualification")
	if disabled {
		if err := r.withdrawKubernetesSandboxRoute(ctx, instance); err != nil {
			return err
		}
		instance.Status.AccessURL = ""
		setCondition(&instance.Status.Conditions, api.ConditionIngressReady, metav1.ConditionTrue,
			"IngressDisabled", "Sandbox ingress is disabled")
		setCustomServiceConditions(instance, access, metav1.ConditionFalse, "IngressDisabled")
		return nil
	}
	if !access.terminal && !instance.Spec.Agent.WebUI {
		if err := r.removeCustomPlatformRoute(ctx, instance); err != nil {
			return err
		}
		instance.Status.AccessURL = ""
		setCondition(&instance.Status.Conditions, api.ConditionIngressReady, metav1.ConditionFalse,
			"PlatformAccessDisabled", "Custom terminal and Web UI are disabled")
		setCustomServiceConditions(instance, access, metav1.ConditionFalse, "PlatformAccessDisabled")
		return nil
	}
	if _, err := r.ensureVirtualService(ctx, instance, access); err != nil {
		return err
	}
	instance.Status.AccessURL = ""
	if instance.Spec.Agent.WebUI {
		instance.Status.AccessURL = accessURLPathBased(instance.Name, r.DomainSuffix)
	}
	setCondition(&instance.Status.Conditions, api.ConditionIngressReady, metav1.ConditionTrue,
		"PlatformServicesConfigured", "Approved Custom Web UI and terminal routes are configured")
	setCustomServiceConditions(instance, access, metav1.ConditionTrue, "PlatformServicesConfigured")
	if access.terminal {
		ready, err := r.customTerminalReady(ctx, instance)
		if err != nil {
			return err
		}
		if !ready {
			setCondition(&instance.Status.Conditions, api.ConditionTerminalConfigured, metav1.ConditionFalse,
				"PlatformTerminalPending", "Waiting for the owned terminal Deployment")
			return fmt.Errorf("common Custom terminal is not ready")
		}
	}
	return nil
}

func reconciledCustomAccess(instance *api.Agent, prepared *helm.PreparedRelease) (customAccess, error) {
	if prepared == nil {
		return customAccess{}, fmt.Errorf("common Custom release is unavailable")
	}
	runtime, _ := prepared.Values["runtime"].(map[string]any)
	agent, _ := runtime["agent"].(map[string]any)
	if agent["type"] != "custom" || agent["webUI"] != instance.Spec.Agent.WebUI {
		return customAccess{}, fmt.Errorf("common Custom runtime differs from its Harness contract")
	}
	access := customAccess{}
	control, _ := prepared.Values["sandboxWorkload"].(map[string]any)
	terminal, _ := control["terminal"].(map[string]any)
	if terminal["enabled"] == true {
		port, err := customSandboxNumber(terminal["port"], 0, 1024, 65535)
		if err != nil || port == 0 {
			return customAccess{}, fmt.Errorf("common Custom terminal contract has an invalid port")
		}
		access.terminal, access.terminalPort = true, uint32(port)
	}
	return access, nil
}
