// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload

import (
	"fmt"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/utils/ptr"
)

// ValidateTemplate checks the complete container and mount inventory before a
// rendered template can become an admission expectation.
func (b *Bundle) ValidateTemplate(template *corev1.PodTemplateSpec, image string) error {
	if template == nil || image == "" {
		return fmt.Errorf("a rendered native Pod template is required")
	}
	var finalizers []string
	if b.input.GatewayKey != nil {
		finalizers = []string{NativePodFinalizer}
	}
	if !slices.Equal(template.Finalizers, finalizers) {
		return fmt.Errorf("native Pod lifetime protection differs from the bundle")
	}
	spec := &template.Spec
	if err := b.ValidateIdentityProjection(*spec); err != nil {
		return err
	}
	for key, value := range b.input.Controls.PodLabels {
		if template.Labels[key] != value {
			return fmt.Errorf("Pod labels do not match the compiler's Agent UID selector")
		}
	}
	for key, value := range map[string]string{
		"sidecar.istio.io/inject": "false", RevisionAnnotation: b.input.Controls.Revision,
		AgentUIDAnnotation: string(b.input.Resolution.Binding.AgentUID), KindAnnotation: AgentWorkload,
	} {
		if template.Annotations[key] != value {
			return fmt.Errorf("protected Pod annotation %s differs from the bundle", key)
		}
	}
	if ptr.Deref(spec.RuntimeClassName, "") != b.input.RuntimeClass.Name ||
		spec.DNSPolicy != corev1.DNSNone || !equality.Semantic.DeepEqual(spec.DNSConfig, b.DNSConfig()) ||
		spec.ServiceAccountName != b.input.Controls.ServiceAccountName ||
		spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken ||
		spec.EnableServiceLinks == nil || *spec.EnableServiceLinks ||
		!equality.Semantic.DeepEqual(spec.SecurityContext, b.PodSecurityContext()) ||
		!equality.Semantic.DeepEqual(spec.Resources, &b.accounting.Pod) {
		return fmt.Errorf("Pod runtime, DNS, identity, security or resources differ from the bundle")
	}
	permitted := spec.DeepCopy()
	permitted.Containers, permitted.InitContainers, permitted.Volumes = nil, nil, nil
	permitted.RuntimeClassName, permitted.AutomountServiceAccountToken = nil, nil
	permitted.ServiceAccountName, permitted.SecurityContext = "", nil
	permitted.Resources, permitted.EnableServiceLinks, permitted.ShareProcessNamespace = nil, nil, nil
	permitted.NodeSelector, permitted.Tolerations = nil, nil
	permitted.DNSPolicy, permitted.DNSConfig = "", nil
	if !equality.Semantic.DeepEqual(*permitted, corev1.PodSpec{}) || ptr.Deref(spec.ShareProcessNamespace, false) {
		return fmt.Errorf("Pod contains unsupported scheduling, host namespace or extension fields")
	}
	var selectors map[string]string
	var tolerations []corev1.Toleration
	if b.input.RuntimeClass.Scheduling != nil {
		selectors, tolerations = b.input.RuntimeClass.Scheduling.NodeSelector, b.input.RuntimeClass.Scheduling.Tolerations
	}
	if !equalJSON(spec.NodeSelector, selectors) || !equalJSON(spec.Tolerations, tolerations) {
		return fmt.Errorf("Pod scheduling differs from the registered RuntimeClass")
	}
	names := []string{"cetirun"}
	if b.input.Helpers.Desktop {
		names = append(names, "desktop")
	}
	if b.input.Helpers.Logger {
		names = append(names, "logger")
	}
	initNames := []string{"init-home-data"}
	if b.input.Helpers.Doctor {
		initNames = append(initNames, "doctor")
	}
	if len(spec.Containers) != len(names) || len(spec.InitContainers) != len(initNames) {
		return fmt.Errorf("unexpected native container or helper count")
	}
	for _, group := range []struct {
		containers []corev1.Container
		names      []string
	}{
		{spec.Containers, names}, {spec.InitContainers, initNames},
	} {
		for i, container := range group.containers {
			if container.Name != group.names[i] {
				return fmt.Errorf("unapproved or reordered helper container")
			}
			if err := b.validateContainer(container, image); err != nil {
				return err
			}
		}
	}
	if err := b.validateVolumes(spec.Volumes); err != nil {
		return err
	}
	accounting := AccountResources(*spec, b.Overhead())
	if !equality.Semantic.DeepEqual(accounting, b.accounting) {
		return fmt.Errorf("effective Pod resources do not match all containers and runtime overhead")
	}
	return nil
}

func (b *Bundle) validateContainer(container corev1.Container, image string) error {
	resources := b.helperResources()
	if container.Name == "cetirun" {
		resources = b.agentResources()
	}
	if !equality.Semantic.DeepEqual(container.Resources, resources) ||
		!equality.Semantic.DeepEqual(container.SecurityContext, b.SecurityContext()) ||
		len(container.VolumeDevices) != 0 || len(container.EnvFrom) != 0 ||
		container.RestartPolicy != nil || len(container.ResizePolicy) != 0 {
		return fmt.Errorf("%s resources, security or extensions violate the bundle", container.Name)
	}
	if container.Image == "" || (container.Name != "desktop" && container.Image != image) {
		return fmt.Errorf("%s image differs from the native profile", container.Name)
	}
	for _, port := range container.Ports {
		if port.HostPort != 0 || port.HostIP != "" {
			return fmt.Errorf("host container ports are forbidden")
		}
	}
	seen := map[string]bool{}
	for _, env := range container.Env {
		if seen[env.Name] {
			return fmt.Errorf("%s has a duplicate environment name", container.Name)
		}
		seen[env.Name] = true
		if env.ValueFrom != nil {
			source := env.ValueFrom
			if env.Value != "" || source.SecretKeyRef == nil || source.ConfigMapKeyRef != nil ||
				source.FieldRef != nil || source.ResourceFieldRef != nil {
				return fmt.Errorf("%s contains an unsupported environment source", container.Name)
			}
			if !slices.Contains(b.input.AuthorizedSecrets, source.SecretKeyRef.Name) {
				return fmt.Errorf("%s references an unauthorized Secret", container.Name)
			}
		}
	}
	mounts := []corev1.VolumeMount{
		{Name: "data", MountPath: HomePath}, {Name: "tmp", MountPath: "/tmp"},
	}
	switch container.Name {
	case "cetirun":
		mounts = append(mounts,
			corev1.VolumeMount{Name: "config", MountPath: "/etc/cetirun", ReadOnly: true},
			corev1.VolumeMount{Name: b.input.SkillsMode.volume(skillsIdentity(b.input.Skills)).Name, MountPath: SkillsPath, ReadOnly: true})
		mounts = append(mounts, b.input.Controls.VolumeMounts...)
		for _, expected := range b.input.Controls.Environment {
			found := false
			for _, actual := range container.Env {
				found = found || equality.Semantic.DeepEqual(expected, actual)
			}
			if !found {
				return fmt.Errorf("main container is missing compiler identity environment")
			}
		}
	case "init-home-data":
		mounts[0].MountPath = "/home-data"
		mounts = append(mounts, corev1.VolumeMount{Name: "config", MountPath: "/etc/cetirun", ReadOnly: true})
		script := "/etc/cetirun/init-home.sh"
		if b.input.AgentType == "hermes" {
			script = "/etc/cetirun/hermes-init-home.sh"
			mounts = append(mounts, corev1.VolumeMount{Name: "secrets", MountPath: "/etc/cetirun-secrets", ReadOnly: true})
		}
		if !b.matchesInitCommand(container.Command, []string{"/bin/sh", script}) || len(container.Args) != 0 {
			return fmt.Errorf("unapproved HOME initializer")
		}
	case "doctor":
		if !b.matchesInitCommand(container.Command, []string{"openclaw", "doctor", "--fix", "--non-interactive", "--yes"}) ||
			len(container.Args) != 0 {
			return fmt.Errorf("unapproved doctor helper")
		}
	case "retire-copilot-sign-in", "retire-direct-model-auth":
		if b.input.GatewayKey == nil || b.input.AgentType != "openclaw" ||
			!b.matchesInitCommand(container.Command, []string{"/bin/sh", "/etc/cetirun/" + container.Name + ".sh"}) ||
			len(container.Args) != 0 {
			return fmt.Errorf("unapproved native Gateway initializer")
		}
		mounts = append(mounts, corev1.VolumeMount{Name: "config", MountPath: "/etc/cetirun", ReadOnly: true})
	case "logger":
		mounts[0].MountPath, mounts[0].SubPath, mounts[0].ReadOnly = HomePath+"/."+b.input.AgentType, "."+b.input.AgentType, true
		mounts = append(mounts, corev1.VolumeMount{Name: "config", MountPath: "/etc/cetirun", ReadOnly: true})
		if !slices.Equal(container.Command, []string{"/bin/sh", "/etc/cetirun/session-logger.sh"}) || len(container.Args) != 0 {
			return fmt.Errorf("unapproved logger helper")
		}
	case "desktop":
		mounts[1].Name = "tmp-desktop"
		if len(container.Command) != 0 || len(container.Args) != 0 {
			return fmt.Errorf("unapproved desktop entrypoint")
		}
	default:
		return fmt.Errorf("unapproved helper classification")
	}
	actual := slices.Clone(container.VolumeMounts)
	sortMounts(actual)
	sortMounts(mounts)
	if !equality.Semantic.DeepEqual(actual, mounts) {
		return fmt.Errorf("%s has unapproved HOME, skills, scratch or identity mounts", container.Name)
	}
	return nil
}

func (b *Bundle) matchesInitCommand(actual, expected []string) bool {
	// Existing chart preparations retain their exact original entrypoints.
	if slices.Equal(actual, expected) {
		return true
	}
	return b.input.AgentType == "openclaw" && len(actual) == len(expected)+3 &&
		slices.Equal(actual[:3], []string{"/usr/bin/tini", "-g", "--"}) &&
		slices.Equal(actual[3:], expected)
}

func sortMounts(mounts []corev1.VolumeMount) {
	slices.SortFunc(mounts, func(a, b corev1.VolumeMount) int {
		if a.MountPath < b.MountPath {
			return -1
		}
		if a.MountPath > b.MountPath {
			return 1
		}
		return 0
	})
}

func (b *Bundle) validateVolumes(volumes []corev1.Volume) error {
	wanted := map[string]corev1.Volume{
		"tmp": {Name: "tmp", VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr.To(b.input.Resolution.Sandbox.Storage.Scratch.SizeLimit)},
		}},
		"data": {Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: b.input.Home.Name}}},
		"config": {Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: b.input.WorkloadName + "-config"},
		}}},
	}
	skills := b.input.SkillsMode.volume(skillsIdentity(b.input.Skills))
	wanted[skills.Name] = skills
	if b.input.Helpers.Desktop {
		wanted["tmp-desktop"] = corev1.Volume{Name: "tmp-desktop", VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr.To(b.input.Resolution.Sandbox.Storage.Scratch.SizeLimit)},
		}}
	}
	for _, volume := range b.input.Controls.Volumes {
		wanted[volume.Name] = volume
	}
	expectedCount := len(wanted) + 1
	for _, volume := range volumes {
		if volume.Name == "secrets" {
			if volume.Secret == nil || !slices.Contains(b.input.AuthorizedSecrets, volume.Secret.SecretName) {
				return fmt.Errorf("runtime Secret volume is not authorized")
			}
			expected := corev1.Volume{Name: "secrets", VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: volume.Secret.SecretName, Optional: ptr.To(true)},
			}}
			if !equality.Semantic.DeepEqual(volume, expected) {
				return fmt.Errorf("runtime Secret volume was modified")
			}
		} else if expected, ok := wanted[volume.Name]; !ok || !equality.Semantic.DeepEqual(volume, expected) {
			return fmt.Errorf("unexpected or modified Pod volume %q", volume.Name)
		}
		delete(wanted, volume.Name)
	}
	if len(wanted) != 0 || len(volumes) != expectedCount {
		return fmt.Errorf("Pod volume inventory is incomplete or contains duplicates")
	}
	return nil
}

func equalJSON[T any](a, b T) bool {
	// Kubernetes omitempty removes empty map/list differences. Quantity
	// equality remains Kubernetes-aware instead of comparing their strings.
	a, _ = clone(a)
	b, _ = clone(b)
	return equality.Semantic.DeepEqual(a, b) || (isEmptyJSON(a) && isEmptyJSON(b))
}

func isEmptyJSON(value any) bool {
	data, _ := jsonValue(value)
	switch v := data.(type) {
	case nil:
		return true
	case map[string]any:
		return len(v) == 0
	case []any:
		return len(v) == 0
	}
	return false
}

// Expect creates a held snapshot; it never converts a render into permission to
// execute. The controller must persist it only after verifying all live sources.
func (b *Bundle) Expect(template corev1.PodTemplateSpec, workload *appsv1.StatefulSet, sources []SourceReference) (Expectation, error) {
	if workload == nil || workload.UID == "" || workload.Name != b.input.WorkloadName || workload.Namespace != b.input.Namespace.Name {
		return Expectation{}, fmt.Errorf("an authoritative StatefulSet identity is required")
	}
	if len(template.Spec.Containers) == 0 {
		return Expectation{}, fmt.Errorf("a protected native template is required")
	}
	if err := b.ValidateTemplate(&template, template.Spec.Containers[0].Image); err != nil {
		return Expectation{}, err
	}
	expected := Expectation{
		Version: ExpectationVersion, Binding: b.input.Resolution.Binding, Revision: b.input.Controls.Revision,
		Agent: ObjectIdentity{Name: b.input.AgentName, UID: b.input.Resolution.Binding.AgentUID}, AgentGeneration: b.input.AgentGeneration,
		Namespace: b.input.Namespace, Workload: ObjectIdentity{Name: workload.Name, UID: workload.UID},
		WorkloadKind: AgentWorkload, ExecutionHeld: true, Template: *template.DeepCopy(), Overhead: b.Overhead(),
		Sources: slices.Clone(sources), Home: ObjectIdentity{Name: b.input.Home.Name, UID: b.input.Home.UID},
		Skills: skillsIdentity(b.input.Skills), SkillsMode: b.input.SkillsMode,
	}
	if err := expected.ValidateSkills(); err != nil {
		return Expectation{}, err
	}
	return expected, nil
}
