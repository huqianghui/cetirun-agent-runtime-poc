// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
)

// The same per-Agent proxy contract as the native chart, expressed as a typed
// platform contribution so Custom's final manifest validator checks every field.
func (p *PreparedRelease) customCopilotObjects(b runtimebindings.Custom) ([]map[string]any, error) {
	proxy := b.Copilot
	if proxy == nil {
		return nil, nil
	}
	if p.chart.Metadata.Annotations["cetirun.microsoft.com/custom-copilot"] != "v1" {
		return nil, fmt.Errorf("the selected chart does not support the Custom Copilot proxy; upgrade the pinned chart")
	}
	if object(p.Values["networkPolicy"])["enabled"] != true {
		return nil, fmt.Errorf("Custom Copilot requires NetworkPolicy")
	}
	name := p.releaseName + "-copilot-proxy"
	if len(name) > 63 {
		name = name[:63]
	}
	name = strings.TrimRight(name, "-")
	labels := map[string]string{"app.kubernetes.io/name": "cetirun-copilot-proxy", "app.kubernetes.io/instance": p.releaseName, "app.kubernetes.io/component": "copilot-proxy"}
	owner := []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: b.AgentName, UID: types.UID(b.AgentUID), Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true)}}
	metadata := metav1.ObjectMeta{Name: name, Namespace: b.Namespace.Name, Labels: labels, OwnerReferences: owner, Annotations: map[string]string{"cetirun.microsoft.com/agent-uid": b.AgentUID}}
	models := []any{}
	for _, model := range proxy.ResponsesModels {
		models = append(models, map[string]any{"model_name": model, "litellm_params": map[string]any{"model": "github_copilot/" + model}, "model_info": map[string]any{"mode": "responses"}})
	}
	models = append(models, map[string]any{"model_name": "*", "litellm_params": map[string]any{"model": "github_copilot/*"}})
	config, err := yaml.Marshal(map[string]any{"model_list": models, "litellm_settings": map[string]any{"num_retries": 0}, "general_settings": map[string]any{"master_key": "os.environ/LITELLM_MASTER_KEY"}})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(config)
	annotations := map[string]string{"checksum/config": hex.EncodeToString(sum[:]), "cetirun.microsoft.com/copilot-proxy-key-id": proxy.KeyID}
	if proxy.SignInGeneration != "" {
		annotations["cetirun.microsoft.com/copilot-sign-in-generation"] = proxy.SignInGeneration
	}
	objects := []any{
		&corev1.ServiceAccount{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"}, ObjectMeta: metadata, AutomountServiceAccountToken: ptr.To(false)},
		&corev1.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: metadata, Data: map[string]string{"config.yaml": string(config)}},
		&corev1.Service{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"}, ObjectMeta: metadata, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Selector: labels,
			Ports: []corev1.ServicePort{{Name: "http", Port: 4000, TargetPort: intstr.FromString("http"), Protocol: corev1.ProtocolTCP}}}},
		&appsv1.Deployment{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}, ObjectMeta: metadata, Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1), Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}, Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: annotations}, Spec: corev1.PodSpec{
				ServiceAccountName: name, AutomountServiceAccountToken: ptr.To(false), EnableServiceLinks: ptr.To(false),
				SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To[int64](65532), RunAsGroup: ptr.To[int64](65532), FSGroup: ptr.To[int64](65532), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
				Containers: []corev1.Container{{Name: "proxy", Image: proxy.Image, ImagePullPolicy: corev1.PullIfNotPresent, Args: []string{"copilot-proxy", "--config", "/etc/litellm/config.yaml", "--port", "4000"},
					SecurityContext: customSecurityContext(), Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 4000, Protocol: corev1.ProtocolTCP}},
					Env: []corev1.EnvVar{
						{Name: "LITELLM_MASTER_KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "ai-gateway-copilot-proxy"}, Key: "master-key"}}},
						{Name: "COPILOT_TOKEN_FILE", Value: "/var/run/copilot/access-token"}, {Name: "GITHUB_COPILOT_TOKEN_DIR", Value: "/var/cache/copilot"}},
					ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/health/liveliness", Port: intstr.FromString("http")}}, PeriodSeconds: 5, TimeoutSeconds: 5},
					Resources:      corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("256Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")}},
					VolumeMounts:   []corev1.VolumeMount{{Name: "config", MountPath: "/etc/litellm", ReadOnly: true}, {Name: "token", MountPath: "/var/run/copilot", ReadOnly: true}, {Name: "cache", MountPath: "/var/cache/copilot"}, {Name: "tmp", MountPath: "/tmp"}},
				}},
				Volumes: []corev1.Volume{
					{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: name}}}},
					{Name: "token", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: api.CopilotTokenSecretName, Optional: ptr.To(true)}}},
					{Name: "cache", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr.To(resource.MustParse("16Mi"))}}},
					{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr.To(resource.MustParse("256Mi"))}}}},
			}}}},
	}
	network := object(p.Values["networkPolicy"])
	peer := func(namespace string) networkv1.NetworkPolicyPeer {
		return networkv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": namespace}}}
	}
	tcp := func(port int32) networkv1.NetworkPolicyPort {
		return networkv1.NetworkPolicyPort{Protocol: ptr.To(corev1.ProtocolTCP), Port: ptr.To(intstr.FromInt32(port))}
	}
	caller := peer(b.Network.Gateway.Namespace)
	caller.PodSelector = &metav1.LabelSelector{MatchLabels: b.Network.Gateway.PodLabels}
	block := &networkv1.IPBlock{CIDR: "0.0.0.0/0"}
	for _, item := range array(network["blockedCIDRs"]) {
		block.Except = append(block.Except, text(item))
	}
	objects = append(objects, &networkv1.NetworkPolicy{TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"}, ObjectMeta: metadata,
		Spec: networkv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: labels}, PolicyTypes: []networkv1.PolicyType{networkv1.PolicyTypeIngress, networkv1.PolicyTypeEgress},
			Ingress: []networkv1.NetworkPolicyIngressRule{{From: []networkv1.NetworkPolicyPeer{caller}, Ports: []networkv1.NetworkPolicyPort{tcp(4000)}}},
			Egress: []networkv1.NetworkPolicyEgressRule{
				{To: []networkv1.NetworkPolicyPeer{peer(text(network["dnsNamespace"]))}, Ports: []networkv1.NetworkPolicyPort{{Protocol: ptr.To(corev1.ProtocolUDP), Port: ptr.To(intstr.FromInt32(53))}, tcp(53)}},
				{To: []networkv1.NetworkPolicyPeer{peer(text(network["istioNamespace"]))}, Ports: []networkv1.NetworkPolicyPort{tcp(15012)}},
				{To: []networkv1.NetworkPolicyPeer{{IPBlock: block}}, Ports: []networkv1.NetworkPolicyPort{tcp(443)}}}}})
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
		delete(object, "status")
		result = append(result, object)
	}
	return result, nil
}
