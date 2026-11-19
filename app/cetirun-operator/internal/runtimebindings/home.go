// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimebindings

import (
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
)

// HomeClaim describes the platform-approved HOME claim template.
type HomeClaim struct {
	StorageClass string `json:"storageClass"`
	StorageSize  string `json:"storageSize"`
	AccessMode   string `json:"accessMode"`
}

func (s HomeClaim) Validate() error {
	if len(validation.IsDNS1123Subdomain(s.StorageClass)) != 0 ||
		(s.AccessMode != "ReadWriteMany" && s.AccessMode != "ReadWriteOnce") {
		return fmt.Errorf("Custom HOME requires an explicit StorageClass and access mode")
	}
	size, err := resource.ParseQuantity(s.StorageSize)
	if err != nil || size.Sign() <= 0 {
		return fmt.Errorf("invalid Custom HOME storageSize")
	}
	return nil
}

func (s HomeClaim) Matches(home *corev1.PersistentVolumeClaim) bool {
	size, err := resource.ParseQuantity(s.StorageSize)
	actual := home.Spec.Resources.Requests[corev1.ResourceStorage]
	return err == nil && actual.Cmp(size) == 0 && ptr.Deref(home.Spec.StorageClassName, "") == s.StorageClass &&
		slices.Equal(home.Spec.AccessModes, []corev1.PersistentVolumeAccessMode{corev1.PersistentVolumeAccessMode(s.AccessMode)})
}

func (s HomeClaim) PVCSpec() corev1.PersistentVolumeClaimSpec {
	return corev1.PersistentVolumeClaimSpec{
		StorageClassName: ptr.To(s.StorageClass),
		AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.PersistentVolumeAccessMode(s.AccessMode)},
		Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceStorage: resource.MustParse(s.StorageSize),
		}},
	}
}
