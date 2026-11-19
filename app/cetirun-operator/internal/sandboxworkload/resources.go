// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload

import corev1 "k8s.io/api/core/v1"

// AccountResources follows Kubernetes effective Pod accounting: restartable init
// containers overlap subsequent init containers and all regular containers;
// non-restartable init containers run sequentially. Runtime overhead is added
// once, outside the declared Pod-level CPU/memory envelope.
func AccountResources(spec corev1.PodSpec, overhead corev1.ResourceList) Accounting {
	requests, limits := effectiveList(spec, false), effectiveList(spec, true)
	pod := corev1.ResourceRequirements{Requests: corev1.ResourceList{}, Limits: corev1.ResourceList{}}
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		if value, ok := requests[name]; ok {
			pod.Requests[name] = value.DeepCopy()
		}
		if value, ok := limits[name]; ok {
			pod.Limits[name] = value.DeepCopy()
		}
	}
	addResources(requests, overhead)
	addResources(limits, overhead)
	return Accounting{Pod: pod, Effective: corev1.ResourceRequirements{Requests: requests, Limits: limits}}
}

func effectiveList(spec corev1.PodSpec, limit bool) corev1.ResourceList {
	resources := func(container corev1.Container) corev1.ResourceList {
		if limit {
			return container.Resources.Limits
		}
		return container.Resources.Requests
	}
	regular, restartable, initMax := corev1.ResourceList{}, corev1.ResourceList{}, corev1.ResourceList{}
	for _, container := range spec.Containers {
		addResources(regular, resources(container))
	}
	for _, container := range spec.InitContainers {
		current := resources(container).DeepCopy()
		if container.RestartPolicy != nil && *container.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			addResources(restartable, current)
			current = restartable.DeepCopy()
		} else {
			addResources(current, restartable)
		}
		maxResources(initMax, current)
	}
	addResources(regular, restartable)
	maxResources(regular, initMax)
	return regular
}

func addResources(target, values corev1.ResourceList) {
	for name, value := range values {
		total := target[name]
		total.Add(value)
		target[name] = total
	}
}

func maxResources(target, values corev1.ResourceList) {
	for name, value := range values {
		current, found := target[name]
		if !found || current.Cmp(value) < 0 {
			target[name] = value.DeepCopy()
		}
	}
}
