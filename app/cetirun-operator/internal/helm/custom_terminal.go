// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/terminal"
	"github.com/Azure/CetiRun/app/internal/terminalpolicy"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
)

var ErrTerminalAPIBindingPending = errors.New("TerminalAPIBindingPending: API server network endpoints are required")

func (p *PreparedRelease) CustomTerminalPort() int32 {
	if !p.terminalEnabled() {
		return 0
	}
	return number(object(p.Values["ttyd"])["port"])
}

func (p *PreparedRelease) terminalImage() string {
	image := object(object(p.Values["ttyd"])["image"])
	repository := text(image["repository"])
	global := object(p.Values["global"])
	if registry := strings.TrimSuffix(text(global["registry"]), "/"); registry != "" {
		repository = registry + "/cetirun-terminal"
	}
	tag := text(global["version"])
	if tag == "" {
		tag = text(image["tag"])
	}
	if tag == "" {
		tag = p.chart.Metadata.AppVersion
	}
	reference := repository + ":" + tag
	if digest := text(image["digest"]); digest != "" {
		reference += "@" + digest
	}
	return reference
}

func validateTerminalValues(values map[string]any) error {
	configuration := object(values["ttyd"])
	if !onlyKeys(configuration, "enabled", "port", "shell", "basePath", "image", "resources", "sessionSeconds", "gatewayServiceAccount") {
		return fmt.Errorf("unsupported Custom terminal setting")
	}
	if configuration["enabled"] != true {
		return nil
	}
	if _, err := runtimebindings.ValidateResources(configuration["resources"]); err != nil {
		return fmt.Errorf("invalid terminal resources: %w", err)
	}
	if len(validation.IsDNS1123Label(text(configuration["gatewayServiceAccount"]))) != 0 ||
		number(configuration["sessionSeconds"]) < 60 || number(configuration["sessionSeconds"]) > 3600 {
		return fmt.Errorf("terminal requires a gateway service account and a 60–3600 second session limit")
	}
	image := object(configuration["image"])
	if !onlyKeys(image, "repository", "tag", "digest", "pullPolicy") ||
		text(image["repository"]) == "" || text(image["pullPolicy"]) == "" {
		return fmt.Errorf("invalid terminal image configuration")
	}
	return nil
}

// These typed resources are platform contributions. The Chart emits them, and
// final validation recreates them from the bindings rather than trusting the
// Chart to choose a target, service account or permission.
func (p *PreparedRelease) terminalObjects(b runtimebindings.Custom) ([]map[string]any, error) {
	if !p.terminalEnabled() {
		return nil, nil
	}
	if len(b.Network.APIServer) == 0 {
		return nil, ErrTerminalAPIBindingPending
	}
	fullname := text(p.Values["fullnameOverride"])
	name := terminalpolicy.WorkloadName(fullname)
	readerName := terminalpolicy.AgentReaderName(b.AgentUID)
	labels := map[string]string{
		"app.kubernetes.io/name":     "cetirun-terminal",
		"app.kubernetes.io/instance": p.releaseName,
	}
	owner := []metav1.OwnerReference{{
		APIVersion: "cetirun.microsoft.com/v1alpha1", Kind: "Agent", Name: b.AgentName, UID: types.UID(b.AgentUID),
		Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
	}}
	meta := func(name, namespace string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels, OwnerReferences: owner,
			Annotations: map[string]string{"cetirun.microsoft.com/agent-uid": b.AgentUID}}
	}
	subject := []rbacv1.Subject{{Kind: "ServiceAccount", Name: name, Namespace: b.Namespace.Name}}
	settings := object(p.Values["ttyd"])
	port := p.CustomTerminalPort()
	resources, _ := runtimebindings.ValidateResources(settings["resources"])
	probe := func(path string) *corev1.Probe {
		return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: path, Port: intstr.FromString("ttyd"),
		}}, PeriodSeconds: 5, TimeoutSeconds: 2, FailureThreshold: 3}
	}
	objects := []any{
		&corev1.ServiceAccount{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
			ObjectMeta: meta(name, b.Namespace.Name), AutomountServiceAccountToken: ptr.To(false)},
		&rbacv1.RoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
			ObjectMeta: meta(name, b.Namespace.Name), Subjects: subject,
			RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name}},
		&rbacv1.RoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
			ObjectMeta: meta(name+"-exec", b.Namespace.Name), Subjects: subject,
			RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: terminal.ExecRole}},
		&rbacv1.ClusterRoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
			ObjectMeta: meta(readerName, ""), Subjects: subject,
			RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: readerName}},
		&corev1.Service{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
			ObjectMeta: meta(name, b.Namespace.Name), Spec: corev1.ServiceSpec{
				Type: corev1.ServiceTypeClusterIP, Selector: labels,
				Ports: []corev1.ServicePort{{Name: "ttyd", Port: port, TargetPort: intstr.FromString("ttyd"), Protocol: corev1.ProtocolTCP}},
			}},
		&appsv1.Deployment{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
			ObjectMeta: meta(name, b.Namespace.Name), Spec: appsv1.DeploymentSpec{
				Replicas: ptr.To[int32](1), Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
				Selector: &metav1.LabelSelector{MatchLabels: labels},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: map[string]string{"sidecar.istio.io/inject": "true"}},
					Spec: corev1.PodSpec{
						ServiceAccountName: name, AutomountServiceAccountToken: ptr.To(false), EnableServiceLinks: ptr.To(false),
						TerminationGracePeriodSeconds: ptr.To[int64](10),
						SecurityContext: &corev1.PodSecurityContext{
							RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To[int64](65532), RunAsGroup: ptr.To[int64](65532),
							FSGroup: ptr.To[int64](65532), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
						},
						Containers: []corev1.Container{{
							Name: "terminal", Image: p.terminalImage(), ImagePullPolicy: corev1.PullPolicy(text(object(settings["image"])["pullPolicy"])),
							Command: []string{"/terminal"}, Args: []string{"serve"}, SecurityContext: customSecurityContext(),
							Ports: []corev1.ContainerPort{{Name: "ttyd", ContainerPort: port, Protocol: corev1.ProtocolTCP}},
							Env: []corev1.EnvVar{
								{Name: "PORT", Value: strconv.Itoa(int(port))},
								{Name: "CETIRUN_TERMINAL_AGENT_NAME", Value: b.AgentName},
								{Name: "CETIRUN_TERMINAL_AGENT_UID", Value: b.AgentUID},
								{Name: "CETIRUN_TERMINAL_NAMESPACE", Value: b.Namespace.Name},
								{Name: "CETIRUN_TERMINAL_STATEFULSET", Value: fullname},
								{Name: "CETIRUN_TERMINAL_CONTAINER", Value: p.chart.Name()},
								{Name: "CETIRUN_TERMINAL_SESSION_SECONDS", Value: strconv.Itoa(int(number(settings["sessionSeconds"])))},
							},
							Resources: resources, ReadinessProbe: probe("/healthz"), LivenessProbe: probe("/livez"),
							VolumeMounts: []corev1.VolumeMount{
								{Name: "tmp", MountPath: "/tmp"},
								{Name: "api-access", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true},
							},
						}},
						Volumes: []corev1.Volume{
							{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
							{Name: "api-access", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
								DefaultMode: ptr.To[int32](0440),
								Sources: []corev1.VolumeProjection{
									{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", ExpirationSeconds: ptr.To[int64](3600)}},
									{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"},
										Items: []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}}}},
									{DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{{
										Path: "namespace", FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"},
									}}}},
								},
							}}},
						},
					},
				},
			}},
	}
	network := object(p.Values["networkPolicy"])
	gatewayNamespace := text(network["istioIngressNamespace"])
	if gatewayNamespace == "" {
		gatewayNamespace = text(network["istioNamespace"])
	}
	peer := func(namespace string) networkv1.NetworkPolicyPeer {
		return networkv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"kubernetes.io/metadata.name": namespace},
		}}
	}
	tcp := func(port int32) networkv1.NetworkPolicyPort {
		return networkv1.NetworkPolicyPort{Protocol: ptr.To(corev1.ProtocolTCP), Port: ptr.To(intstr.FromInt32(port))}
	}
	egress := []networkv1.NetworkPolicyEgressRule{
		{To: []networkv1.NetworkPolicyPeer{peer(text(network["dnsNamespace"]))}, Ports: []networkv1.NetworkPolicyPort{
			{Protocol: ptr.To(corev1.ProtocolUDP), Port: ptr.To(intstr.FromInt32(53))}, tcp(53),
		}},
		{To: []networkv1.NetworkPolicyPeer{peer(text(network["istioNamespace"]))}, Ports: []networkv1.NetworkPolicyPort{tcp(15012)}},
	}
	for _, endpoint := range b.Network.APIServer {
		address, _ := netip.ParseAddr(endpoint.IP)
		cidr := netip.PrefixFrom(address, address.BitLen()).String()
		egress = append(egress, networkv1.NetworkPolicyEgressRule{
			To:    []networkv1.NetworkPolicyPeer{{IPBlock: &networkv1.IPBlock{CIDR: cidr}}},
			Ports: []networkv1.NetworkPolicyPort{tcp(endpoint.Port)},
		})
	}
	objects = append(objects, &networkv1.NetworkPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: meta(name, b.Namespace.Name), Spec: networkv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: labels},
			PolicyTypes: []networkv1.PolicyType{networkv1.PolicyTypeIngress, networkv1.PolicyTypeEgress},
			Ingress: []networkv1.NetworkPolicyIngressRule{{From: []networkv1.NetworkPolicyPeer{peer(gatewayNamespace)},
				Ports: []networkv1.NetworkPolicyPort{tcp(port)}}},
			Egress: egress,
		},
	})
	objects = append(objects,
		map[string]any{"apiVersion": "security.istio.io/v1", "kind": "PeerAuthentication", "metadata": meta(name, b.Namespace.Name),
			"spec": map[string]any{"selector": map[string]any{"matchLabels": labels}, "mtls": map[string]any{"mode": "STRICT"}}},
		map[string]any{"apiVersion": "security.istio.io/v1", "kind": "AuthorizationPolicy", "metadata": meta(name, b.Namespace.Name),
			"spec": map[string]any{
				"selector": map[string]any{"matchLabels": labels}, "action": "ALLOW",
				"rules": []any{map[string]any{"from": []any{map[string]any{"source": map[string]any{
					"principals": []string{"cluster.local/ns/" + gatewayNamespace + "/sa/" + text(settings["gatewayServiceAccount"])},
				}}}, "to": []any{map[string]any{"operation": map[string]any{"ports": []string{strconv.Itoa(int(port))}}}}}},
			}},
	)
	result := make([]map[string]any, 0, len(objects))
	for _, value := range objects {
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		var object map[string]any
		if err := json.Unmarshal(raw, &object); err != nil {
			return nil, err
		}
		// creationTimestamp:null is encoding boilerplate, not a desired field.
		delete(object["metadata"].(map[string]any), "creationTimestamp")
		delete(object, "status")
		result = append(result, object)
	}
	return result, nil
}
