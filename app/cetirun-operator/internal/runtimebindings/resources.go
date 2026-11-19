// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimebindings

import (
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
)

// ValidateResources runs before any lifecycle handoff. Quantity comparison is
// numeric, so equivalent encodings such as 1 and 1000m have the same meaning.
func ValidateResources(value any) (corev1.ResourceRequirements, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return corev1.ResourceRequirements{}, fmt.Errorf("invalid Custom resources")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return corev1.ResourceRequirements{}, fmt.Errorf("invalid Custom resources")
	}
	for key := range fields {
		if key != "requests" && key != "limits" {
			return corev1.ResourceRequirements{}, fmt.Errorf("Custom resources accepts only requests and limits")
		}
	}
	var resources corev1.ResourceRequirements
	if json.Unmarshal(raw, &resources) != nil {
		return resources, fmt.Errorf("Custom resource quantities must be valid")
	}
	hasHugePages, hasCPUOrMemory := false, false
	for _, list := range []corev1.ResourceList{resources.Requests, resources.Limits} {
		for name, quantity := range list {
			if !validResourceName(string(name)) || quantity.Sign() < 0 {
				return resources, fmt.Errorf("Custom resources must use supported names and non-negative quantities")
			}
			if name == corev1.ResourceCPU &&
				quantity.Cmp(*resource.NewMilliQuantity(quantity.MilliValue(), resource.DecimalSI)) != 0 {
				return resources, fmt.Errorf("Custom CPU precision must not exceed one millicpu")
			}
			if name == corev1.ResourceCPU || name == corev1.ResourceMemory {
				hasCPUOrMemory = true
			}
			if isExtendedResource(string(name)) && !quantity.IsZero() {
				if quantity.Cmp(*resource.NewQuantity(quantity.Value(), resource.DecimalSI)) != 0 {
					return resources, fmt.Errorf("Custom extended resources must be integers")
				}
			}
			if strings.HasPrefix(string(name), "hugepages-") {
				hasHugePages = true
				page, _ := resource.ParseQuantity(strings.TrimPrefix(string(name), "hugepages-"))
				pageBytes, bytes := page.Value(), quantity.Value()
				if pageBytes <= 0 || page.Cmp(*resource.NewQuantity(pageBytes, resource.DecimalSI)) != 0 ||
					quantity.Cmp(*resource.NewQuantity(bytes, resource.DecimalSI)) != 0 || bytes%pageBytes != 0 {
					return resources, fmt.Errorf("Custom hugepage quantities must be whole page multiples")
				}
			}
		}
	}
	if hasHugePages && !hasCPUOrMemory {
		return resources, fmt.Errorf("Custom hugepages require CPU or memory resources")
	}
	for name, requested := range resources.Requests {
		nonOvercommittable := isExtendedResource(string(name)) || strings.HasPrefix(string(name), "hugepages-")
		if _, exists := resources.Limits[name]; nonOvercommittable && !exists {
			return resources, fmt.Errorf("Custom non-overcommittable resources require a limit")
		}
		if limit, ok := resources.Limits[name]; ok {
			if requested.Cmp(limit) > 0 {
				return resources, fmt.Errorf("Custom resource requests must not exceed limits")
			}
			if nonOvercommittable && requested.Cmp(limit) != 0 {
				return resources, fmt.Errorf("Custom non-overcommittable resource requests must equal limits")
			}
		}
	}
	return resources, nil
}

func validResourceName(name string) bool {
	if len(validation.IsQualifiedName(name)) != 0 {
		return false
	}
	switch name {
	case "cpu", "memory", "ephemeral-storage":
		return true
	}
	if strings.HasPrefix(name, "hugepages-") {
		size, err := resource.ParseQuantity(strings.TrimPrefix(name, "hugepages-"))
		return err == nil && size.Sign() > 0
	}
	return strings.Contains(name, "/") &&
		(strings.Contains(name, "kubernetes.io/") || !strings.HasPrefix(name, "requests."))
}

func isExtendedResource(name string) bool {
	return strings.Contains(name, "/") && !strings.Contains(name, "kubernetes.io/")
}
