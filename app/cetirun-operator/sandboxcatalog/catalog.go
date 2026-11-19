// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package sandboxcatalog exposes the Portal's read-only Sandbox selection
// inventory without exposing the Operator's internal platform implementation.
package sandboxcatalog

import (
	"context"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxplatform"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// StorageClass is an approved current StorageClass that may be selected for
// Sandbox HOME. It is not a readiness guarantee for a subsequent write.
type StorageClass struct {
	Name                 string `json:"name"`
	Provisioner          string `json:"provisioner"`
	AllowVolumeExpansion bool   `json:"allowVolumeExpansion"`
}

// Options contains approved HOME storage choices and runtime-declared HOME
// access modes. Empty collections are always represented as empty collections.
type Options struct {
	StorageClasses  []StorageClass                                          `json:"storageClasses"`
	HomeAccessModes map[api.SandboxType][]corev1.PersistentVolumeAccessMode `json:"homeAccessModes"`
}

// Reader reads one explicit platform configuration ConfigMap.
type Reader struct {
	APIReader client.Reader
	Namespace string
	Name      string
}

// StorageOptions returns the currently usable approved StorageClasses.
func (r Reader) StorageOptions(ctx context.Context) (Options, error) {
	options, err := (&sandboxplatform.KubernetesSource{APIReader: r.APIReader, Namespace: r.Namespace, Name: r.Name}).StorageOptions(ctx)
	if err != nil {
		return Options{StorageClasses: []StorageClass{}, HomeAccessModes: map[api.SandboxType][]corev1.PersistentVolumeAccessMode{}}, err
	}
	result := Options{
		StorageClasses:  make([]StorageClass, 0, len(options.StorageClasses)),
		HomeAccessModes: make(map[api.SandboxType][]corev1.PersistentVolumeAccessMode, len(options.HomeAccessModes)),
	}
	for _, option := range options.StorageClasses {
		result.StorageClasses = append(result.StorageClasses, StorageClass{Name: option.Name, Provisioner: option.Provisioner, AllowVolumeExpansion: option.AllowVolumeExpansion})
	}
	for kind, modes := range options.HomeAccessModes {
		result.HomeAccessModes[kind] = make([]corev1.PersistentVolumeAccessMode, len(modes))
		copy(result.HomeAccessModes[kind], modes)
	}
	return result, nil
}
