// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxplatform

import (
	"context"
	"fmt"
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
)

// StorageOption is one current StorageClass from the platform's approved
// inventory. It is an input-selection aid only; it is not runtime readiness
// evidence and does not replace the admission checks for Sandbox writes.
type StorageOption struct {
	Name                 string
	Provisioner          string
	AllowVolumeExpansion bool
}

// StorageOptions is the current approved HOME storage inventory and the HOME
// modes declared by available Sandbox runtime registrations.
type StorageOptions struct {
	StorageClasses  []StorageOption
	HomeAccessModes map[api.SandboxType][]corev1.PersistentVolumeAccessMode
}

// StorageOptions reads the selected platform configuration without discovering
// storage resources. Missing, terminating, or identity-mismatched approved
// classes are omitted. API read failures retain their fail-closed behavior.
func (s *KubernetesSource) StorageOptions(ctx context.Context) (StorageOptions, error) {
	empty := StorageOptions{StorageClasses: []StorageOption{}, HomeAccessModes: map[api.SandboxType][]corev1.PersistentVolumeAccessMode{}}
	if s == nil || s.APIReader == nil || len(validation.IsDNS1123Label(s.Namespace)) != 0 ||
		len(validation.IsDNS1123Subdomain(s.Name)) != 0 {
		return empty, fmt.Errorf("%w: an authoritative reader and explicit valid configuration namespace/name are required", sandboxpolicy.ErrUnavailable)
	}
	configuration, err := s.readConfiguration(ctx)
	if err != nil {
		return empty, err
	}
	document, err := decodeSandboxPlatformConfiguration(configuration.Data[ConfigurationKey])
	if err != nil {
		return empty, err
	}
	options := storageOptionsFromConfiguration(document)
	current := make(map[string]storagev1.StorageClass, len(document.StorageClasses))
	for _, name := range slices.Sorted(maps.Keys(document.StorageClasses)) {
		pin := document.StorageClasses[name]
		storageClass := storagev1.StorageClass{}
		if err := s.APIReader.Get(ctx, client.ObjectKey{Name: name}, &storageClass); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return empty, fmt.Errorf("read approved StorageClass: %w", err)
		}
		if storageClass.Name != name || storageClass.UID != pin.UID || storageClass.ResourceVersion == "" ||
			storageClass.Provisioner != pin.Provisioner || !storageClass.DeletionTimestamp.IsZero() {
			continue
		}
		current[name] = storageClass
		options.StorageClasses = append(options.StorageClasses, StorageOption{
			Name: name, Provisioner: storageClass.Provisioner, AllowVolumeExpansion: storageClass.AllowVolumeExpansion != nil && *storageClass.AllowVolumeExpansion,
		})
	}
	// Repeat the direct reads for each returned option so a result cannot combine
	// a previously observed identity with a replacement made during this capture.
	for _, option := range options.StorageClasses {
		before := current[option.Name]
		after := storagev1.StorageClass{}
		if err := s.APIReader.Get(ctx, client.ObjectKey{Name: option.Name}, &after); err != nil {
			return empty, fmt.Errorf("recheck approved StorageClass: %w", err)
		}
		if after.UID != before.UID || after.ResourceVersion != before.ResourceVersion ||
			after.Provisioner != before.Provisioner || !after.DeletionTimestamp.IsZero() {
			return empty, fmt.Errorf("%w: approved StorageClass changed during capture", ErrSourceChanged)
		}
	}
	latest, err := s.readConfiguration(ctx)
	if err != nil {
		return empty, err
	}
	if latest.UID != configuration.UID || latest.ResourceVersion != configuration.ResourceVersion ||
		latest.Data[ConfigurationKey] != configuration.Data[ConfigurationKey] {
		return empty, fmt.Errorf("%w: platform configuration changed during capture", ErrSourceChanged)
	}
	return options, nil
}

func storageOptionsFromConfiguration(document Configuration) StorageOptions {
	options := StorageOptions{StorageClasses: []StorageOption{}, HomeAccessModes: make(map[api.SandboxType][]corev1.PersistentVolumeAccessMode)}
	for _, kind := range []api.SandboxType{api.SandboxKata, api.SandboxGVisor, api.SandboxContainer} {
		registration, exists := document.Registrations[kind]
		if !exists || !registration.Available {
			continue
		}
		modes := make([]corev1.PersistentVolumeAccessMode, 0, len(registration.Capabilities.HomeAccessModes))
		for _, mode := range registration.Capabilities.HomeAccessModes {
			if mode == corev1.ReadWriteOnce || mode == corev1.ReadWriteOncePod {
				modes = append(modes, mode)
			}
		}
		options.HomeAccessModes[kind] = modes
	}
	return options
}
