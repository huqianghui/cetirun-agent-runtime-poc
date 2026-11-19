// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"context"
	"slices"

	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func nativePodFinalizerRemoval(request admission.Request, pod *corev1.Pod) bool {
	if request.Operation != admissionv1.Update || request.SubResource != "" ||
		request.RequestSubResource != "" || pod.UID == "" || pod.ResourceVersion == "" {
		return false
	}
	old := &corev1.Pod{}
	if decodePod(request.OldObject.Raw, old) != nil || !slices.Contains(old.Finalizers, w.NativePodFinalizer) ||
		slices.Contains(pod.Finalizers, w.NativePodFinalizer) {
		return false
	}
	old.Finalizers = slices.DeleteFunc(old.Finalizers, func(value string) bool { return value == w.NativePodFinalizer })
	if len(old.Finalizers) == 0 {
		old.Finalizers = nil
	}
	current := pod.DeepCopy()
	if len(current.Finalizers) == 0 {
		current.Finalizers = nil
	}
	old.ManagedFields, current.ManagedFields = nil, nil
	return equality.Semantic.DeepEqual(old, current)
}

// The finalizer can be removed only after the controller durably consumes the
// Pod allowance. Admission remains read-only, including retries and dry runs.
func (h *PodHandler) releaseNativePodFinalizer(ctx context.Context, request admission.Request, pod *corev1.Pod,
	expected w.Expectation, state *corev1.ConfigMap,
) admission.Response {
	if !expected.SinglePod || expected.PodUID == "" || expected.PodUID != pod.UID ||
		pod.Name != expected.Workload.Name+"-0" || pod.Namespace != expected.Namespace.Name {
		return rejectPod(ctx, request, podDenied("LifetimeUnrecorded", "the initial Pod lifetime must be recorded before finalization"))
	}
	current := &corev1.Pod{}
	if err := h.reader.Get(ctx, client.ObjectKeyFromObject(pod), current); err != nil {
		return rejectPod(ctx, request, podReadFailure("PreviousPod", err))
	}
	if current.UID != pod.UID || current.ResourceVersion != pod.ResourceVersion ||
		!slices.Contains(current.Finalizers, w.NativePodFinalizer) {
		return rejectPod(ctx, request, podDenied("PreviousPodMismatch", "native finalization requires the current recorded Pod"))
	}
	confirmed := &corev1.ConfigMap{}
	if err := h.reader.Get(ctx, client.ObjectKeyFromObject(state), confirmed); err != nil {
		return rejectPod(ctx, request, podReadFailure("Expectation", err))
	}
	if confirmed.UID != state.UID || confirmed.ResourceVersion != state.ResourceVersion {
		return rejectPod(ctx, request, podDenied("ExpectationChanged", "native Pod lifetime changed during finalization"))
	}
	if ctx.Err() != nil {
		return rejectPod(ctx, request, podReadFailure("Expectation", ctx.Err()))
	}
	return admission.Allowed("the recorded native Pod lifetime remains consumed after finalization")
}
