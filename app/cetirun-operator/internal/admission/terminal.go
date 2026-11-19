// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/internal/terminalpolicy"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// Reject unrelated callers before any authoritative API reads.
func terminalConnectPreflight(request admission.Request) bool {
	workload, matched := strings.CutSuffix(request.Name, "-0")
	if request.Namespace == "" || !matched || workload == "" ||
		(request.RequestSubResource != "" && request.RequestSubResource != "exec") ||
		request.UserInfo.Username != "system:serviceaccount:"+request.Namespace+":"+terminalpolicy.WorkloadName(workload) {
		return false
	}
	var options corev1.PodExecOptions
	return json.Unmarshal(request.Object.Raw, &options) == nil && options.Container == "cetirun" && terminalCommandPermitted(options)
}

func terminalCommandPermitted(options corev1.PodExecOptions) bool {
	if !options.Stdout {
		return false
	}
	for _, candidate := range terminalpolicy.ShellCandidates() {
		if slices.Equal(options.Command, candidate.Probe) {
			return !options.Stdin && !options.TTY
		}
		command, err := terminalpolicy.ShellCommand(candidate.Name)
		if err == nil && slices.Equal(options.Command, command) {
			return options.Stdin && options.TTY
		}
	}
	return false
}

func (h *PodHandler) validateTerminalConnect(ctx context.Context, request admission.Request) admission.Response {
	ctx, cancel := context.WithTimeout(ctx, h.config.RequestTimeout)
	defer cancel()
	deny := func(reason, message string) admission.Response {
		return rejectPod(ctx, request, podDenied(reason, message))
	}
	expected, state, handled, failure := h.reconciledExpectation(ctx, request.Namespace)
	if failure != nil {
		return rejectPod(ctx, request, failure)
	}
	if !handled || expected.Terminal == nil {
		return deny("TerminalUnavailable", "terminal access is unavailable for this Sandbox")
	}
	terminal := expected.Terminal
	if terminal.ServiceAccountName != terminalpolicy.WorkloadName(terminal.TargetWorkload) ||
		request.UserInfo.Username != "system:serviceaccount:"+request.Namespace+":"+terminal.ServiceAccountName ||
		request.Name != terminal.TargetWorkload+"-0" {
		return deny("TerminalIdentityUnauthorized", "terminal caller or target differs from the current Sandbox plan")
	}
	var options corev1.PodExecOptions
	if err := json.Unmarshal(request.Object.Raw, &options); err != nil || options.Container != terminal.Container ||
		!terminalCommandPermitted(options) {
		return deny("TerminalCommandUnauthorized", "terminal exec command differs from the approved shell protocol")
	}
	agent, binding, err := sandboxbinding.Read(ctx, h.reader, client.ObjectKey{Name: expected.Agent.Name})
	if err != nil || agent.UID != expected.Agent.UID || agent.Generation != expected.AgentGeneration || binding != expected.Binding ||
		agent.Spec.Agent.Type != api.AgentTypeCustom || agent.Spec.Suspended || !agent.DeletionTimestamp.IsZero() ||
		slices.Contains(agent.Finalizers, sandbox.HoldFinalizer) {
		return deny("BindingUnauthorized", "terminal binding or lifecycle state is not current")
	}
	if failure := h.validateReconciledSources(ctx, expected); failure != nil {
		return rejectPod(ctx, request, failure)
	}
	checkReservation := func() *podFailure {
		if h.coordination == nil {
			return nil
		}
		intent, err := sandboxcoordination.ReconciledIntentFor(expected)
		if err != nil {
			return podDenied("NativeReservationRequired", "terminal requires the current Sandbox reservation")
		}
		if err := h.coordination.AuthorizeNativeStart(ctx, intent); err != nil {
			if errors.Is(err, sandboxcoordination.ErrConflict) {
				return podDenied("NativeReservationRequired", "terminal requires the current non-stopping Sandbox reservation")
			}
			return podReadFailure("NativeReservation", err)
		}
		return nil
	}
	if failure := checkReservation(); failure != nil {
		return rejectPod(ctx, request, failure)
	}
	if failure := h.validateTerminalAuthority(ctx, expected, agent); failure != nil {
		return rejectPod(ctx, request, failure)
	}
	confirmed := &corev1.ConfigMap{}
	if err := h.reader.Get(ctx, client.ObjectKeyFromObject(state), confirmed); err != nil {
		return rejectPod(ctx, request, podReadFailure("Expectation", err))
	}
	if confirmed.UID != state.UID || confirmed.ResourceVersion != state.ResourceVersion {
		return deny("ExpectationChanged", "terminal expectation changed during authorization")
	}
	if failure := h.validateReconciledSources(ctx, expected); failure != nil {
		return rejectPod(ctx, request, failure)
	}
	if ctx.Err() != nil {
		return rejectPod(ctx, request, podReadFailure("Expectation", ctx.Err()))
	}
	// A lifecycle stop may have fenced the reservation during the API reads.
	if failure := checkReservation(); failure != nil {
		return rejectPod(ctx, request, failure)
	}
	return admission.Allowed("terminal exec matches the current common Sandbox expectation")
}

func (h *PodHandler) validateTerminalAuthority(ctx context.Context, expected w.ReconcileExpectation, agent *api.Agent) *podFailure {
	terminal := expected.Terminal
	serviceAccount := &corev1.ServiceAccount{}
	if err := h.reader.Get(ctx, client.ObjectKey{Namespace: expected.Namespace.Name, Name: terminal.ServiceAccountName}, serviceAccount); err != nil {
		return podReadFailure("TerminalServiceAccount", err)
	}
	owner := w.AgentOwner(agent.Name, agent.UID)
	if !serviceAccount.DeletionTimestamp.IsZero() || ptr.Deref(serviceAccount.AutomountServiceAccountToken, true) ||
		!equality.Semantic.DeepEqual(serviceAccount.OwnerReferences, []metav1.OwnerReference{owner}) {
		return podDenied("TerminalServiceAccountMismatch", "terminal service account differs from the current Sandbox plan")
	}
	main := &appsv1.StatefulSet{}
	if err := h.reader.Get(ctx, client.ObjectKey{Namespace: expected.Namespace.Name, Name: terminal.TargetWorkload}, main); err != nil {
		return podReadFailure("TerminalTarget", err)
	}
	index := slices.IndexFunc(expected.Workloads, func(workload w.RenderedWorkload) bool {
		return workload.Kind == "StatefulSet" && workload.Identity.Name == terminal.TargetWorkload
	})
	if index < 0 || expected.Workloads[index].Identity.UID == "" || main.UID != expected.Workloads[index].Identity.UID ||
		!main.DeletionTimestamp.IsZero() || ptr.Deref(main.Spec.Replicas, 0) != 1 || main.Status.ObservedGeneration < main.Generation ||
		main.Status.ReadyReplicas < 1 || !equality.Semantic.DeepEqual(main.OwnerReferences, []metav1.OwnerReference{owner}) ||
		!w.SamePodTemplate(main.Spec.Template, expected.Workloads[index].Template) {
		return podDenied("TerminalTargetUnavailable", "terminal target is not the current ready Sandbox workload")
	}
	pod := &corev1.Pod{}
	if err := h.reader.Get(ctx, client.ObjectKey{Namespace: expected.Namespace.Name, Name: terminal.TargetWorkload + "-0"}, pod); err != nil {
		return podReadFailure("TerminalTargetPod", err)
	}
	containerReady := slices.ContainsFunc(pod.Status.ContainerStatuses, func(status corev1.ContainerStatus) bool {
		return status.Name == terminal.Container && status.Ready && status.State.Running != nil && status.ContainerID != ""
	})
	podOwner := metav1.OwnerReference{APIVersion: "apps/v1", Kind: "StatefulSet", Name: main.Name, UID: main.UID,
		Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true)}
	if pod.UID == "" || !pod.DeletionTimestamp.IsZero() || pod.Status.Phase != corev1.PodRunning || !containerReady ||
		!equality.Semantic.DeepEqual(pod.OwnerReferences, []metav1.OwnerReference{podOwner}) {
		return podDenied("TerminalTargetUnavailable", "terminal target Pod is not the current ready workload Pod")
	}
	wanted := expected.Workloads[index]
	node, failure := h.readAssignedNode(ctx, pod)
	if failure != nil {
		return failure
	}
	if err := w.ValidateRenderedPodOnNode(pod, node, wanted, podOwner, main.Status.UpdateRevision); err != nil {
		return podDenied("TerminalTargetUnavailable", "terminal target Pod differs from the current Sandbox template")
	}
	return nil
}
