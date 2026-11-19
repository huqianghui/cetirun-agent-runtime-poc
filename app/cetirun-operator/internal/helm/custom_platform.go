// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/agents"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/observability"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/terminal"
	"github.com/Azure/CetiRun/app/internal/terminalpolicy"
	"github.com/Masterminds/semver/v3"
	"helm.sh/helm/v3/pkg/chartutil"
	corev1 "k8s.io/api/core/v1"
	networkv1 "k8s.io/api/networking/v1"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
)

func (p *PreparedRelease) setCustomCapabilities(capabilities *chartutil.Capabilities) {
	defer func() {
		if p.basePlanHash == "" {
			return
		}
		selected := map[string]string{"inputs": p.basePlanHash}
		if object(p.Values["sidecarScope"])["enabled"] == true {
			selected["sidecar"] = p.sidecarAPIVersion
		}
		if object(p.Values["podDisruptionBudget"])["enabled"] == true {
			selected["pdb"] = p.pdbAPIVersion
		}
		selected["homeRetention"] = strconv.FormatBool(p.homeRetention)
		raw, _ := json.Marshal(selected)
		sum := sha256.Sum256(raw)
		p.PlanHash = hex.EncodeToString(sum[:])
		object(p.Values["runtime"])["planHash"] = p.PlanHash
	}()
	p.sidecarAPIVersion = ""
	p.pdbAPIVersion = "policy/v1beta1"
	p.homeRetention = false
	if capabilities == nil {
		return
	}
	if version, err := semver.NewVersion(capabilities.KubeVersion.Version); err == nil {
		if !version.LessThan(semver.MustParse("1.21.0")) {
			p.pdbAPIVersion = "policy/v1"
		}
		p.homeRetention = !version.LessThan(semver.MustParse("1.25.0"))
	}
	for _, version := range []string{"networking.istio.io/v1", "networking.istio.io/v1beta1"} {
		if capabilities.APIVersions.Has(version + "/Sidecar") {
			p.sidecarAPIVersion = version
			return
		}
	}
}

func validateCustomPlatformValues(values map[string]any, agent agents.ResolvedAgent, restartRequestID string) error {
	if err := validateTerminalValues(values); err != nil {
		return err
	}
	if err := validateCustomSkillValues(values); err != nil {
		return err
	}
	var annotations map[string]string
	if err := decodeObject(values["podAnnotations"], &annotations); err != nil {
		return fmt.Errorf("podAnnotations must contain string values")
	}
	if problems := apivalidation.ValidateAnnotations(annotations, field.NewPath("podAnnotations")); len(problems) != 0 {
		return fmt.Errorf("invalid podAnnotations: %s", problems.ToAggregate())
	}
	network := object(values["networkPolicy"])
	if agent.Port != nil && network["enabled"] != true {
		return fmt.Errorf("Custom HTTP requires networkPolicy.enabled=true: its Agent endpoint has no application token")
	}
	if object(values["ttyd"])["enabled"] == true {
		if network["enabled"] != true {
			return fmt.Errorf("Custom terminal requires networkPolicy.enabled=true")
		}
		if object(values["execRestriction"])["enabled"] != true {
			return fmt.Errorf("Custom terminal requires execRestriction.enabled=true")
		}
		if text(object(values["ttyd"])["basePath"]) != terminal.BasePath {
			return fmt.Errorf("Custom terminal must use the platform base path")
		}
	}
	if network["enabled"] == true {
		for _, entry := range array(network["blockedCIDRs"]) {
			if problems := validation.IsValidCIDRForLegacyField(field.NewPath("networkPolicy", "blockedCIDRs"), text(entry), false, nil); len(problems) != 0 {
				return fmt.Errorf("invalid networkPolicy.blockedCIDRs: %s", problems.ToAggregate())
			}
		}
		for _, entry := range array(network["allowedInternalCIDRs"]) {
			if problems := validation.IsValidCIDRForLegacyField(field.NewPath("networkPolicy", "allowedInternalCIDRs"), text(object(entry)["cidr"]), false, nil); len(problems) != 0 {
				return fmt.Errorf("invalid networkPolicy.allowedInternalCIDRs: %s", problems.ToAggregate())
			}
		}
	}
	pdb := object(values["podDisruptionBudget"])
	if pdb["enabled"] == true {
		if templateTruthy(pdb["minAvailable"]) && templateTruthy(pdb["maxUnavailable"]) {
			return fmt.Errorf("podDisruptionBudget cannot set both minAvailable and maxUnavailable")
		}
		for _, key := range []string{"minAvailable", "maxUnavailable"} {
			if templateTruthy(pdb[key]) {
				if _, err := pdbValue(pdb[key]); err != nil {
					return fmt.Errorf("invalid podDisruptionBudget.%s: %w", key, err)
				}
			}
		}
	}
	desktop := object(values["desktop"])
	if !onlyKeys(desktop, "enabled", "image", "resources", "screenResolution", "novncPathPrefix", "logging") {
		return fmt.Errorf("unsupported desktop configuration")
	}
	if desktop["enabled"] == true {
		if _, err := runtimebindings.ValidateResources(desktop["resources"]); err != nil {
			return fmt.Errorf("invalid desktop resources: %w", err)
		}
		for _, port := range []int32{5900, 6080, 18800} {
			if agent.Port != nil && *agent.Port == port {
				return fmt.Errorf("Custom Agent port conflicts with the platform desktop")
			}
		}
	}
	autoscaling := object(values["autoscaling"])
	if autoscaling["enabled"] == true && (number(autoscaling["minReplicas"]) != 1 || number(autoscaling["maxReplicas"]) != 1) {
		return fmt.Errorf("autoscaling.enabled requires minReplicas=1 and maxReplicas=1 for CetiRun agents")
	}
	for key, value := range object(values["podAnnotations"]) {
		if key == api.AnnotationRestartRequestID && restartRequestID != "" && value == restartRequestID {
			continue
		}
		if key == "checksum/config" || strings.HasPrefix(key, "cetirun.microsoft.com/") || observability.CollectorAnnotation(key) {
			return fmt.Errorf("Custom pod annotation %s is platform-owned", key)
		}
	}
	return nil
}

func (p *PreparedRelease) desktopEnabled() bool {
	return object(p.Values["desktop"])["enabled"] == true
}

func (p *PreparedRelease) terminalEnabled() bool {
	return object(p.Values["ttyd"])["enabled"] == true
}

func (p *PreparedRelease) desktopContainer() corev1.Container {
	desktop := object(p.Values["desktop"])
	image := object(desktop["image"])
	global := object(p.Values["global"])
	repository := text(image["repository"])
	if registry := strings.TrimSuffix(text(global["registry"]), "/"); registry != "" {
		repository = registry + "/cetirun-desktop"
	}
	tag := text(global["version"])
	if tag == "" {
		tag = text(image["tag"])
	}
	if tag == "" {
		tag = p.chart.Metadata.AppVersion
	}
	imageRef := repository + ":" + tag
	if digest := text(image["digest"]); digest != "" {
		imageRef += "@" + digest
	}
	resources, _ := runtimebindings.ValidateResources(desktop["resources"])
	env := []corev1.EnvVar{
		{Name: "DISPLAY_NUM", Value: "99"},
		{Name: "SCREEN_RESOLUTION", Value: text(desktop["screenResolution"])},
		{Name: "VNC_PORT", Value: "5900"},
		{Name: "NOVNC_PORT", Value: "6080"},
		{Name: "NOVNC_PATH_PREFIX", Value: text(desktop["novncPathPrefix"])},
		{Name: "DESKTOP_LOGGING", Value: fmt.Sprint(desktop["logging"])},
	}
	return corev1.Container{
		Name: "desktop", Image: imageRef, ImagePullPolicy: corev1.PullPolicy(text(image["pullPolicy"])),
		SecurityContext: &corev1.SecurityContext{
			RunAsUser: ptr.To[int64](1024), RunAsNonRoot: ptr.To(true), ReadOnlyRootFilesystem: ptr.To(false),
			AllowPrivilegeEscalation: ptr.To(false),
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Ports: []corev1.ContainerPort{
			{Name: "novnc", ContainerPort: 6080, Protocol: corev1.ProtocolTCP},
			{Name: "chrome-cdp", ContainerPort: 18800, Protocol: corev1.ProtocolTCP},
		},
		Env: env, Resources: resources,
		VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/home/vibe"}, {Name: "tmp-desktop", MountPath: "/tmp"}},
	}
}

func (p *PreparedRelease) customServicePorts() []corev1.ServicePort {
	var ports []corev1.ServicePort
	contribution, err := p.customContribution()
	if err == nil {
		for _, port := range contribution.Ports {
			ports = append(ports, corev1.ServicePort{Name: port.Name, Port: port.Number, TargetPort: intstr.FromString(port.Name), Protocol: corev1.ProtocolTCP})
		}
	}
	if p.desktopEnabled() {
		ports = append(ports, corev1.ServicePort{Name: "novnc", Port: 6080, TargetPort: intstr.FromInt32(6080), Protocol: corev1.ProtocolTCP})
	}
	return ports
}

type platformObject struct {
	apiVersion string
	name       string
	spec       map[string]any
}

// These expectations describe platform controls, independently of Chart source.
// Re-enabling a shared template must not turn final validation into a kind-only allowlist.
func (p *PreparedRelease) customPlatformObjects(namespace string) map[string]platformObject {
	name := text(p.Values["fullnameOverride"])
	objects := p.customSkillsObjects(namespace)
	if object(p.Values["execRestriction"])["enabled"] == true {
		policyName := name + "-" + namespace + "-deny-exec"
		validation := map[string]any{"expression": "false", "message": "kubectl exec is not allowed on openclaw pods"}
		if p.terminalEnabled() {
			bindings := object(object(p.Values["runtime"])["bindings"])
			validation["expression"] = terminalpolicy.AdmissionExpression(namespace, name+"-0", p.chart.Name(),
				terminalpolicy.WorkloadName(name), text(bindings["agentUID"]))
			validation["reason"] = "Forbidden"
		}
		objects["ValidatingAdmissionPolicy"] = platformObject{"admissionregistration.k8s.io/v1", policyName, map[string]any{
			"failurePolicy": "Fail",
			"matchConstraints": map[string]any{
				"resourceRules": []any{map[string]any{"apiGroups": []string{""}, "apiVersions": []string{"v1"},
					"operations": []string{"CONNECT"}, "resources": []string{"pods/exec"}}},
				"namespaceSelector": map[string]any{"matchLabels": map[string]string{"kubernetes.io/metadata.name": namespace}},
			},
			"validations": []any{validation},
		}}
		objects["ValidatingAdmissionPolicyBinding"] = platformObject{"admissionregistration.k8s.io/v1", policyName,
			map[string]any{"policyName": policyName, "validationActions": []string{"Deny"}}}
	}
	pdb := object(p.Values["podDisruptionBudget"])
	if pdb["enabled"] == true {
		spec := map[string]any{"selector": map[string]any{"matchLabels": customLabels(p.chart.Name(), p.releaseName)}}
		for _, key := range []string{"minAvailable", "maxUnavailable"} {
			if templateTruthy(pdb[key]) {
				spec[key], _ = pdbValue(pdb[key])
			}
		}
		objects["PodDisruptionBudget"] = platformObject{p.pdbAPIVersion, name, spec}
	}
	hpa := object(p.Values["autoscaling"])
	if hpa["enabled"] == true {
		var metrics []any
		for _, resource := range []struct{ name, key string }{{"cpu", "targetCPUUtilizationPercentage"}, {"memory", "targetMemoryUtilizationPercentage"}} {
			if target := number(hpa[resource.key]); target != 0 {
				metrics = append(metrics, map[string]any{"type": "Resource", "resource": map[string]any{
					"name": resource.name, "target": map[string]any{"type": "Utilization", "averageUtilization": target},
				}})
			}
		}
		metrics = append(metrics, array(hpa["metrics"])...)
		spec := map[string]any{
			"scaleTargetRef": map[string]any{"apiVersion": "apps/v1", "kind": "StatefulSet", "name": name},
			"minReplicas":    hpa["minReplicas"], "maxReplicas": hpa["maxReplicas"], "metrics": metrics,
		}
		if behavior := object(hpa["behavior"]); len(behavior) > 0 {
			spec["behavior"] = behavior
		}
		objects["HorizontalPodAutoscaler"] = platformObject{"autoscaling/v2", name, spec}
	}
	scope := object(p.Values["sidecarScope"])
	if scope["enabled"] == true && p.sidecarAPIVersion != "" {
		network := object(p.Values["networkPolicy"])
		hosts := []any{"./*", text(network["istioNamespace"]) + "/*"}
		if ingress := text(network["istioIngressNamespace"]); ingress != "" {
			hosts = append(hosts, ingress+"/*")
		}
		hosts = append(hosts, array(scope["additionalEgressHosts"])...)
		bindings := object(object(p.Values["runtime"])["bindings"])
		hosts = append(hosts, text(object(object(bindings["network"])["gateway"])["namespace"])+"/*")
		objects["Sidecar"] = platformObject{p.sidecarAPIVersion, name, map[string]any{
			"workloadSelector": map[string]any{"labels": customLabels(p.chart.Name(), p.releaseName)},
			"egress":           []any{map[string]any{"hosts": hosts}},
		}}
	}
	return objects
}

func templateTruthy(value any) bool {
	if value == nil {
		return false
	}
	switch v := value.(type) {
	case string:
		return v != ""
	case bool:
		return v
	default:
		return fmt.Sprint(value) != "0"
	}
}

func pdbValue(value any) (intstr.IntOrString, error) {
	raw := fmt.Sprint(value)
	if strings.ContainsAny(raw, "\r\n") {
		return intstr.IntOrString{}, fmt.Errorf("must be a single integer or percentage")
	}
	// The shared template emits a plain YAML scalar, so normalize its actual
	// rendering (including numeric strings) before comparing with the API form.
	var scalar any
	var parsed intstr.IntOrString
	if yaml.UnmarshalStrict([]byte(raw), &scalar) != nil || scalar == nil || decodeObject(scalar, &parsed) != nil {
		return parsed, fmt.Errorf("must be a non-negative integer or percentage")
	}
	if parsed.Type == intstr.Int {
		if parsed.IntVal >= 0 {
			return parsed, nil
		}
	} else if strings.HasSuffix(parsed.StrVal, "%") {
		n, err := strconv.Atoi(strings.TrimSuffix(parsed.StrVal, "%"))
		if err == nil && n >= 0 {
			return parsed, nil
		}
	}
	return intstr.IntOrString{}, fmt.Errorf("must be a non-negative integer or percentage")
}

func sameJSON(a, b any) bool {
	left, leftErr := json.Marshal(a)
	right, rightErr := json.Marshal(b)
	return leftErr == nil && rightErr == nil && string(left) == string(right)
}

func (p *PreparedRelease) customNetwork(n runtimebindings.Network) networkv1.NetworkPolicySpec {
	config := object(p.Values["networkPolicy"])
	peer := func(namespace string) networkv1.NetworkPolicyPeer {
		return networkv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"kubernetes.io/metadata.name": namespace},
		}}
	}
	boundPeer := func(value runtimebindings.Peer) networkv1.NetworkPolicyPeer {
		result := peer(value.Namespace)
		result.PodSelector = &metav1.LabelSelector{MatchLabels: value.PodLabels}
		return result
	}
	tcp := func(port int32) networkv1.NetworkPolicyPort {
		return networkv1.NetworkPolicyPort{Protocol: ptr.To(corev1.ProtocolTCP), Port: ptr.To(intstr.FromInt32(port))}
	}
	spec := networkv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{MatchLabels: customLabels(p.chart.Name(), p.releaseName)},
		PolicyTypes: []networkv1.PolicyType{networkv1.PolicyTypeIngress, networkv1.PolicyTypeEgress},
		Ingress:     []networkv1.NetworkPolicyIngressRule{},
		Egress: []networkv1.NetworkPolicyEgressRule{
			{To: []networkv1.NetworkPolicyPeer{peer(text(config["dnsNamespace"]))}, Ports: []networkv1.NetworkPolicyPort{
				{Protocol: ptr.To(corev1.ProtocolUDP), Port: ptr.To(intstr.FromInt32(53))}, tcp(53),
			}},
			{To: []networkv1.NetworkPolicyPeer{peer(text(config["istioNamespace"]))}, Ports: []networkv1.NetworkPolicyPort{tcp(15012)}},
		},
	}
	if n.Gateway.Namespace != "" {
		spec.Egress = append([]networkv1.NetworkPolicyEgressRule{{
			To: []networkv1.NetworkPolicyPeer{boundPeer(n.Gateway)}, Ports: []networkv1.NetworkPolicyPort{tcp(n.GatewayPort)},
		}}, spec.Egress...)
	}
	if p.desktopEnabled() || p.Agent.WebUI {
		namespace := text(config["istioIngressNamespace"])
		if namespace == "" {
			namespace = text(config["istioNamespace"])
		}
		ports := []networkv1.NetworkPolicyPort{}
		if p.desktopEnabled() {
			ports = append(ports, tcp(6080))
		}
		if p.Agent.WebUI {
			ports = append(ports, tcp(*p.Agent.Port))
		}
		spec.Ingress = append(spec.Ingress, networkv1.NetworkPolicyIngressRule{From: []networkv1.NetworkPolicyPeer{peer(namespace)}, Ports: ports})
	}
	if p.Agent.Port != nil && !p.Agent.WebUI && len(n.Callers) > 0 {
		rule := networkv1.NetworkPolicyIngressRule{Ports: []networkv1.NetworkPolicyPort{tcp(*p.Agent.Port)}}
		for _, caller := range n.Callers {
			rule.From = append(rule.From, boundPeer(caller))
		}
		spec.Ingress = append(spec.Ingress, rule)
	}
	for _, entry := range array(config["allowedInternalCIDRs"]) {
		value := object(entry)
		rule := networkv1.NetworkPolicyEgressRule{To: []networkv1.NetworkPolicyPeer{{IPBlock: &networkv1.IPBlock{CIDR: text(value["cidr"])}}}}
		for _, entry := range array(value["ports"]) {
			value := object(entry)
			port := tcp(number(value["port"]))
			if protocol := text(value["protocol"]); protocol != "" {
				port.Protocol = ptr.To(corev1.Protocol(protocol))
			}
			rule.Ports = append(rule.Ports, port)
		}
		spec.Egress = append(spec.Egress, rule)
	}
	block := &networkv1.IPBlock{CIDR: "0.0.0.0/0"}
	for _, cidr := range array(config["blockedCIDRs"]) {
		block.Except = append(block.Except, text(cidr))
	}
	spec.Egress = append(spec.Egress, networkv1.NetworkPolicyEgressRule{To: []networkv1.NetworkPolicyPeer{{IPBlock: block}}})
	return spec
}
