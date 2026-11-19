// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload

import (
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
)

// NativeClaimDigest pins ownership and the storage request while permitting
// Kubernetes to complete delayed binding. Once bound, the API makes volumeName
// immutable; it is not a user-selected replacement volume for this lifetime.
func NativeClaimDigest(claim *corev1.PersistentVolumeClaim) (string, error) {
	if claim == nil {
		return "", fmt.Errorf("a native storage claim is required")
	}
	copy := claim.DeepCopy()
	copy.Spec.VolumeName = ""
	copy.Finalizers = slices.DeleteFunc(copy.Finalizers, func(value string) bool {
		return value == "kubernetes.io/pvc-protection"
	})
	if len(copy.Finalizers) == 0 {
		copy.Finalizers = nil
	}
	return ExecutionObjectDigest(copy)
}
