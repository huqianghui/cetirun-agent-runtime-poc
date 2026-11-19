// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
)

func (p *PreparedRelease) validateReconciledObject(object unstructured.Unstructured) error {
	input := p.sandboxBundle.Identity()
	var workload *w.RenderedWorkload
	if object.GetKind() == "Deployment" || object.GetKind() == "StatefulSet" {
		annotations, _, err := unstructured.NestedStringMap(object.Object, "spec", "template", "metadata", "annotations")
		if err != nil {
			return err
		}
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[w.ReconciledAnnotation] = "true"
		if err := unstructured.SetNestedStringMap(object.Object, annotations, "spec", "template", "metadata", "annotations"); err != nil {
			return err
		}
	}
	switch object.GetAPIVersion() + "/" + object.GetKind() {
	case "apps/v1/StatefulSet":
		var stateful appsv1.StatefulSet
		if err := decodeObject(object.Object, &stateful); err != nil {
			return err
		}
		if stateful.Name != input.WorkloadName || len(stateful.Spec.VolumeClaimTemplates) != 0 ||
			stateful.Spec.ServiceName != stateful.Name || stateful.Spec.Selector == nil {
			return fmt.Errorf("reconciled Sandbox must use the owned Agent workload and existing HOME")
		}
		memory, _ := p.Values["memoryContext"].(map[string]any)
		if err := p.sandboxBundle.ValidateReconciledTemplate(&stateful.Spec.Template, p.Agent.Image, memory["enabled"] == true, p.Agent.Port); err != nil {
			return err
		}
		if err := p.validateSandboxOpenClawEnvironment(stateful.Spec.Template.Spec); err != nil {
			return err
		}
		if request := input.PlannedMigration; request != nil {
			if err := runtimebindings.PinMigrationNode(&stateful.Spec.Template.Spec, request.Target.Name); err != nil {
				return err
			}
			stateful.Spec.Template.Annotations[api.AnnotationMigrationRequest] = request.RequestID
		}
		helpers := input.Resolution.Sandbox.Resources.Helpers
		helperResources := corev1.ResourceRequirements{Requests: helpers.Requests, Limits: helpers.Limits}
		for index := range stateful.Spec.Template.Spec.InitContainers {
			stateful.Spec.Template.Spec.InitContainers[index].Resources = *helperResources.DeepCopy()
		}
		for index := 1; index < len(stateful.Spec.Template.Spec.Containers); index++ {
			stateful.Spec.Template.Spec.Containers[index].Resources = *helperResources.DeepCopy()
		}
		accounting := w.AccountResources(stateful.Spec.Template.Spec, p.sandboxBundle.Overhead())
		stateful.Spec.Template.Spec.Resources = &accounting.Pod
		template, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&stateful.Spec.Template)
		if err != nil {
			return err
		}
		if err := unstructured.SetNestedMap(object.Object, template, "spec", "template"); err != nil {
			return err
		}
		p.sandboxTemplate = stateful.Spec.Template.DeepCopy()
		workload = &w.RenderedWorkload{
			Kind: "StatefulSet", Identity: w.ObjectIdentity{Name: stateful.Name},
			Replicas: ptr.Deref(stateful.Spec.Replicas, 1), Template: stateful.Spec.Template,
			Overhead: p.sandboxBundle.Overhead(),
		}
	case "apps/v1/Deployment":
		var deployment appsv1.Deployment
		if err := decodeObject(object.Object, &deployment); err != nil {
			return err
		}
		spec := deployment.Spec.Template.Spec
		if spec.HostNetwork || spec.HostPID || spec.HostIPC || ptr.Deref(spec.ShareProcessNamespace, false) ||
			ptr.Deref(spec.AutomountServiceAccountToken, true) || spec.SecurityContext == nil ||
			len(spec.Containers) == 0 || deployment.Spec.Selector == nil {
			return fmt.Errorf("Sandbox auxiliary workload has unapproved host or identity access")
		}
		terminal := input.Terminal != nil && deployment.Name == input.Terminal.ServiceAccountName
		if terminal {
			if err := p.validateTerminalDeployment(&deployment); err != nil {
				return err
			}
		} else if err := w.ValidateReconciledVolumes(spec, nil, nil, input.Resolution.Sandbox.Storage.Scratch.SizeLimit); err != nil {
			return err
		}
		for _, source := range w.PodSourceReferences(spec, input.Namespace.Name) {
			if source.Kind == "Secret" && (slices.Contains(input.AuthorizedSecrets, source.Name) ||
				(input.ImagePullSecret != nil && source.Name == input.ImagePullSecret.Name)) {
				return fmt.Errorf("Sandbox auxiliary workload cannot receive Agent credentials")
			}
		}
		for _, containers := range [][]corev1.Container{spec.InitContainers, spec.Containers} {
			for _, container := range containers {
				sc := container.SecurityContext
				if sc == nil || ptr.Deref(sc.Privileged, false) || ptr.Deref(sc.AllowPrivilegeEscalation, true) ||
					!ptr.Deref(sc.RunAsNonRoot, ptr.Deref(spec.SecurityContext.RunAsNonRoot, false)) ||
					ptr.Deref(sc.RunAsUser, ptr.Deref(spec.SecurityContext.RunAsUser, 0)) <= 0 ||
					sc.Capabilities == nil || len(sc.Capabilities.Add) != 0 ||
					!slices.Contains(sc.Capabilities.Drop, corev1.Capability("ALL")) {
					return fmt.Errorf("Sandbox auxiliary workload privileges are unapproved")
				}
			}
		}
		workload = &w.RenderedWorkload{
			Kind: "Deployment", Identity: w.ObjectIdentity{Name: deployment.Name},
			Replicas: ptr.Deref(deployment.Spec.Replicas, 1), Template: deployment.Spec.Template,
		}
		if terminal {
			workload.Overhead = p.sandboxBundle.Overhead()
		}
	case "v1/Service":
		var service corev1.Service
		if err := decodeObject(object.Object, &service); err != nil {
			return err
		}
		if service.Spec.Type != corev1.ServiceTypeClusterIP || service.Spec.ExternalName != "" || len(service.Spec.ExternalIPs) != 0 {
			return fmt.Errorf("Sandbox services must remain private")
		}
		if service.Name != input.WorkloadName && (input.Terminal == nil || service.Name != input.Terminal.ServiceAccountName) {
			break
		}
		if err := p.sandboxBundle.ValidateIngressService(&service); err != nil {
			return err
		}
		if input.Controls.IngressMTLS != nil {
			spec, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&service.Spec)
			if err != nil {
				return err
			}
			if err := unstructured.SetNestedMap(object.Object, spec, "spec"); err != nil {
				return err
			}
		}
	case "rbac.authorization.k8s.io/v1/RoleBinding":
		if err := p.validateTerminalRoleBinding(object); err != nil {
			return err
		}
	case "v1/ConfigMap":
		if object.GetName() == input.WorkloadName+"-config" {
			var config corev1.ConfigMap
			if err := decodeObject(object.Object, &config); err != nil {
				return err
			}
			if err := p.validateSandboxOpenClawConfig(config); err != nil {
				return err
			}
		}
	case "v1/ServiceAccount", "networking.k8s.io/v1/NetworkPolicy",
		"networking.istio.io/v1beta1/Sidecar", "security.istio.io/v1beta1/AuthorizationPolicy",
		"security.istio.io/v1beta1/PeerAuthentication":
	default:
		return fmt.Errorf("unsupported chart-owned Sandbox resource %s", object.GetKind())
	}
	if workload != nil {
		if workload.Replicas < 0 || workload.Replicas > 1 {
			return fmt.Errorf("Sandbox workloads require at most one active replica")
		}
		if input.Suspended && workload.Replicas != 0 {
			return fmt.Errorf("suspended Sandbox components must remain at zero replicas")
		}
		p.sandboxReconcile.Workloads = append(p.sandboxReconcile.Workloads, *workload)
	}
	return nil
}

type reconciledSandboxRenderer struct {
	prepared *PreparedRelease
}

func (r reconciledSandboxRenderer) Run(_ *bytes.Buffer) (*bytes.Buffer, error) {
	if err := r.prepared.AuthorizeSandboxRender(context.Background()); err != nil {
		return nil, err
	}
	return bytes.NewBuffer(bytes.Clone(r.prepared.manifest)), nil
}

// CheckSandboxApply validates authority before Helm's read-only preparation
// without declaring that an apply has been submitted.
func (p *PreparedRelease) CheckSandboxApply(ctx context.Context) error {
	if p == nil || p.sandboxReconcile == nil || p.sandboxPreflight == nil || p.sealedInput != p.inputDigest() {
		return fmt.Errorf("Sandbox preflight requires unchanged current render and controller authority")
	}
	return p.sandboxPreflight(ctx)
}

// AuthorizeSandboxRender fences manifest delivery without declaring a primary
// workload submission.
func (p *PreparedRelease) AuthorizeSandboxRender(ctx context.Context) error {
	if p == nil || p.sandboxReconcile == nil || p.sandboxRenderGuard == nil || p.sealedInput != p.inputDigest() {
		return fmt.Errorf("Sandbox render requires unchanged current render and controller authority")
	}
	return p.sandboxRenderGuard(ctx)
}

// AuthorizeSandboxAuxiliaryApply fences an approved auxiliary without consuming
// the primary HOME writer's non-submission proof. Cleanup still waits for it.
func (p *PreparedRelease) AuthorizeSandboxAuxiliaryApply(ctx context.Context, object unstructured.Unstructured) error {
	if p == nil || p.sandboxReconcile == nil || object.GetAPIVersion() != "apps/v1" || object.GetKind() != "Deployment" ||
		object.GetNamespace() != p.sandboxReconcile.Namespace.Name ||
		!slices.ContainsFunc(p.sandboxReconcile.Workloads, func(workload w.RenderedWorkload) bool {
			return workload.Kind == "Deployment" && workload.Identity.Name == object.GetName()
		}) {
		return fmt.Errorf("Sandbox auxiliary apply requires its sealed workload identity")
	}
	return p.AuthorizeSandboxRender(ctx)
}

// AuthorizeSandboxApply rechecks authority and records submission intent before
// a primary or opaque workload apply. Earlier submissions remain submitted.
func (p *PreparedRelease) AuthorizeSandboxApply(ctx context.Context) error {
	if p == nil || p.sandboxReconcile == nil || p.sandboxGuard == nil || p.sealedInput != p.inputDigest() {
		return fmt.Errorf("Sandbox apply requires unchanged current render and controller authority")
	}
	return p.sandboxGuard(ctx)
}

// AuthorizeSandboxWorkloadCreate binds a definite primary non-creation result to
// this submission. Callers without a rejection recorder retain the opaque barrier.
func (p *PreparedRelease) AuthorizeSandboxWorkloadCreate(ctx context.Context) (func(error) error, error) {
	if p == nil || p.sandboxReconcile == nil || p.sandboxGuard == nil || p.sealedInput != p.inputDigest() {
		return nil, fmt.Errorf("Sandbox workload creation requires current render and controller authority")
	}
	if p.sandboxWorkloadCreate == nil {
		if err := p.AuthorizeSandboxApply(ctx); err != nil {
			return nil, err
		}
		return func(err error) error { return err }, nil
	}
	rejected, err := p.sandboxWorkloadCreate(ctx)
	if err != nil {
		return nil, err
	}
	return func(createErr error) error {
		if rejected == nil || !sandboxBindingCreateRejected(createErr) {
			return createErr
		}
		if err := rejected(ctx); err != nil {
			return errors.Join(createErr, fmt.Errorf("record rejected Sandbox workload creation: %w", err))
		}
		return createErr
	}, nil
}

// AuthorizeSandboxBindingCreate returns an outcome recorder bound to this
// submission. Call it with the CREATE result before returning to reconciliation.
func (p *PreparedRelease) AuthorizeSandboxBindingCreate(ctx context.Context, object unstructured.Unstructured) (func(error) error, error) {
	if p == nil || p.sandboxReconcile == nil || p.sandboxBindingCreate == nil || p.sealedInput != p.inputDigest() {
		return nil, fmt.Errorf("Kubernetes grant creation requires current per-binding authority")
	}
	if object.GetUID() != "" || object.GetResourceVersion() != "" ||
		!slices.ContainsFunc(p.sandboxReconcile.Controls, func(expected unstructured.Unstructured) bool {
			return w.KubernetesBindingResource(expected) && w.ReconciledObjectContentMatches(&object, &expected)
		}) {
		return nil, fmt.Errorf("Kubernetes grant creation differs from the sealed control inventory")
	}
	rejected, err := p.sandboxBindingCreate(ctx, object)
	if err != nil {
		return nil, err
	}
	if rejected == nil {
		return nil, fmt.Errorf("Kubernetes grant creation requires its rejection checkpoint")
	}
	return func(createErr error) error {
		if !sandboxBindingCreateRejected(createErr) {
			return createErr
		}
		if err := rejected(ctx); err != nil {
			return errors.Join(createErr, fmt.Errorf("record rejected Kubernetes grant creation: %w", err))
		}
		return createErr
	}, nil
}
