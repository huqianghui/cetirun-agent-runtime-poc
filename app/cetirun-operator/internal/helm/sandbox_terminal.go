// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"fmt"
	"slices"
	"strconv"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/ingressmtls"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/terminal"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

func (p *PreparedRelease) validateTerminalRoleBinding(object unstructured.Unstructured) error {
	input := p.sandboxBundle.Identity()
	terminalInput := input.Terminal
	if terminalInput == nil {
		return fmt.Errorf("Sandbox RoleBindings require a platform terminal plan")
	}
	var binding rbacv1.RoleBinding
	if err := decodeObject(object.Object, &binding); err != nil {
		return err
	}
	account := terminalInput.ServiceAccountName
	if binding.Namespace != input.Namespace.Name || len(binding.Subjects) != 1 ||
		binding.Subjects[0] != (rbacv1.Subject{Kind: "ServiceAccount", Name: account, Namespace: input.Namespace.Name}) {
		return fmt.Errorf("Sandbox terminal RoleBinding subject differs from the platform plan")
	}
	want := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: account}
	switch binding.Name {
	case account:
	case account + "-exec":
		want = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: terminal.ExecRole}
	default:
		return fmt.Errorf("Sandbox RoleBinding is not an approved terminal binding")
	}
	if binding.RoleRef != want {
		return fmt.Errorf("Sandbox terminal RoleBinding role differs from the platform plan")
	}
	return nil
}

func (p *PreparedRelease) validateTerminalDeployment(deployment *appsv1.Deployment) error {
	input := p.sandboxBundle.Identity()
	settings := input.Terminal
	if settings == nil || deployment.Name != settings.ServiceAccountName || deployment.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType ||
		deployment.Spec.Selector == nil || ptr.Deref(deployment.Spec.Replicas, 0) != 1 {
		return fmt.Errorf("Sandbox terminal deployment differs from its platform plan")
	}
	labels := map[string]string{
		"app.kubernetes.io/name": "cetirun-terminal", "app.kubernetes.io/instance": p.releaseName,
		"cetirun.microsoft.com/terminal":          "true",
		"sandbox.cetirun.microsoft.com/agent-uid": string(input.Resolution.Binding.AgentUID),
	}
	spec := deployment.Spec.Template.Spec
	tlsContainer, tlsVolumes := p.sandboxBundle.TerminalTLS()
	containerCount := 1
	if tlsContainer != nil {
		containerCount++
	}
	var selectors map[string]string
	var tolerations []corev1.Toleration
	if input.RuntimeClass.Scheduling != nil {
		selectors, tolerations = input.RuntimeClass.Scheduling.NodeSelector, input.RuntimeClass.Scheduling.Tolerations
	}
	podSecurity := &corev1.PodSecurityContext{
		RunAsUser: ptr.To[int64](65532), RunAsGroup: ptr.To[int64](65532), RunAsNonRoot: ptr.To(true),
		FSGroup: ptr.To[int64](65532), FSGroupChangePolicy: ptr.To(corev1.FSGroupChangeOnRootMismatch),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	if !equality.Semantic.DeepEqual(deployment.Spec.Selector.MatchLabels, labels) || !equality.Semantic.DeepEqual(deployment.Spec.Template.Labels, labels) ||
		deployment.Spec.Selector.MatchExpressions != nil || spec.ServiceAccountName != settings.ServiceAccountName ||
		ptr.Deref(spec.AutomountServiceAccountToken, true) || ptr.Deref(spec.EnableServiceLinks, true) ||
		ptr.Deref(spec.RuntimeClassName, "") != input.RuntimeClass.Name || spec.DNSPolicy != corev1.DNSNone ||
		!equality.Semantic.DeepEqual(spec.DNSConfig, p.sandboxBundle.DNSConfig()) || len(spec.InitContainers) != 0 || len(spec.Containers) != containerCount ||
		len(spec.ImagePullSecrets) != 0 || !equality.Semantic.DeepEqual(spec.NodeSelector, selectors) ||
		!equality.Semantic.DeepEqual(spec.Tolerations, tolerations) || !equality.Semantic.DeepEqual(spec.SecurityContext, podSecurity) {
		return fmt.Errorf("Sandbox terminal runtime, identity or scheduling differs from the platform plan")
	}
	container := spec.Containers[0]
	resources := corev1.ResourceRequirements{Requests: input.Resolution.Sandbox.Resources.Helpers.Requests, Limits: input.Resolution.Sandbox.Resources.Helpers.Limits}
	security := &corev1.SecurityContext{RunAsUser: ptr.To[int64](65532), RunAsGroup: ptr.To[int64](65532), RunAsNonRoot: ptr.To(true),
		ReadOnlyRootFilesystem: ptr.To(true), AllowPrivilegeEscalation: ptr.To(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}
	env := []corev1.EnvVar{
		{Name: "PORT", Value: strconv.Itoa(int(settings.Port))},
		{Name: "CETIRUN_TERMINAL_AGENT_NAME", Value: input.AgentName},
		{Name: "CETIRUN_TERMINAL_AGENT_UID", Value: string(input.Resolution.Binding.AgentUID)},
		{Name: "CETIRUN_TERMINAL_NAMESPACE", Value: input.Namespace.Name},
		{Name: "CETIRUN_TERMINAL_STATEFULSET", Value: input.WorkloadName},
		{Name: "CETIRUN_TERMINAL_CONTAINER", Value: "cetirun"},
		{Name: "CETIRUN_TERMINAL_SESSION_SECONDS", Value: strconv.Itoa(int(settings.SessionSeconds))},
	}
	serverName := ""
	if tlsContainer != nil {
		serverName = input.Controls.IngressMTLS.ServerName
		env = append(env, corev1.EnvVar{Name: "CETIRUN_BIND_HOST", Value: "127.0.0.1"},
			corev1.EnvVar{Name: "CETIRUN_INGRESS_MTLS", Value: "true"},
			corev1.EnvVar{Name: "CETIRUN_INGRESS_SERVER_NAME", Value: input.Controls.IngressMTLS.ServerName})
		digest, err := input.Controls.IngressMTLS.ConfigDigest()
		if err != nil || deployment.Spec.Template.Annotations[ingressmtls.ConfigDigestAnnotation] != digest ||
			!equality.Semantic.DeepEqual(spec.Containers[1], *tlsContainer) {
			return fmt.Errorf("Custom terminal TLS helper differs from its approved endpoint")
		}
	}
	if container.Name != "terminal" || container.Image != settings.Image || container.ImagePullPolicy != corev1.PullIfNotPresent ||
		!slices.Equal(container.Command, []string{"/terminal"}) || !slices.Equal(container.Args, []string{"serve"}) ||
		!equality.Semantic.DeepEqual(container.SecurityContext, security) || !equality.Semantic.DeepEqual(container.Resources, resources) ||
		!equality.Semantic.DeepEqual(container.Env, env) || !equality.Semantic.DeepEqual(container.Ports, []corev1.ContainerPort{{Name: "terminal", ContainerPort: settings.Port, Protocol: corev1.ProtocolTCP}}) ||
		len(container.EnvFrom) != 0 || len(container.VolumeDevices) != 0 || !terminalProbesMatch(container, serverName, settings.Port) {
		return fmt.Errorf("Sandbox terminal container differs from the platform plan")
	}
	volumes := []corev1.Volume{
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr.To(input.Resolution.Sandbox.Storage.Scratch.SizeLimit.DeepCopy())}}},
		{Name: "api-access", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{DefaultMode: ptr.To[int32](0440), Sources: []corev1.VolumeProjection{
			// Kubernetes supplies its configured API audience when it is omitted.
			{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", ExpirationSeconds: ptr.To[int64](3600)}},
			{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"}, Items: []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}}}},
			{DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{{Path: "namespace", FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"}}}}},
		}}}},
	}
	mounts := []corev1.VolumeMount{
		{Name: "tmp", MountPath: "/tmp"}, {Name: "api-access", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true},
	}
	if tlsContainer != nil {
		volumes = append(volumes, tlsVolumes...)
		mounts = append(mounts, corev1.VolumeMount{Name: "ingress-public", MountPath: ingressmtls.PublicPath, ReadOnly: true})
	}
	if !equality.Semantic.DeepEqual(spec.Volumes, volumes) || !equality.Semantic.DeepEqual(container.VolumeMounts, mounts) {
		return fmt.Errorf("Sandbox terminal must only mount its scratch and Kubernetes API projections")
	}
	return nil
}

func terminalProbesMatch(container corev1.Container, serverName string, port int32) bool {
	probe := func(path string) *corev1.Probe {
		if serverName != "" {
			return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{
				"/loopback-probe", "--url", fmt.Sprintf("http://127.0.0.1:%d%s", port, path),
				"--mtls-server-name", serverName,
			}}}, PeriodSeconds: 5, TimeoutSeconds: 2, FailureThreshold: 3}
		}
		return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: path, Port: intstr.FromString("terminal")}},
			PeriodSeconds: 5, TimeoutSeconds: 2, FailureThreshold: 3}
	}
	return equality.Semantic.DeepEqual(container.ReadinessProbe, probe("/healthz")) &&
		equality.Semantic.DeepEqual(container.LivenessProbe, probe("/livez")) && container.StartupProbe == nil
}
