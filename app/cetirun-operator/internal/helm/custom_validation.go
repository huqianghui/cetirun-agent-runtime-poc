// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strconv"
	"strings"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/agents"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubeyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/utils/ptr"
	strictjson "sigs.k8s.io/json"
	"sigs.k8s.io/yaml"
)

// ValidateCustom examines every rendered object, including hooks, before Helm
// gets any opportunity to act. It also runs in offline preparation tests.
func (p *PreparedRelease) ValidateCustom(namespace, release string) error {
	if !p.custom && p.Agent.Type != "custom" {
		return nil
	}
	if p.chart == nil || p.Values["agentType"] != "custom" {
		return fmt.Errorf("custom release is not prepared")
	}
	runtime := object(p.Values["runtime"])
	var bindings runtimebindings.Custom
	if err := decodeObject(runtime["bindings"], &bindings); err != nil {
		return fmt.Errorf("invalid custom bindings")
	}
	if err := bindings.Validate(p.Agent); err != nil {
		return err
	}
	if namespace != bindings.Namespace.Name || release != p.releaseName {
		return fmt.Errorf("custom namespace or release mismatch")
	}
	if p.sealedInput != "" {
		if p.inputDigest() != p.sealedInput {
			return fmt.Errorf("Custom plan inputs changed after sealing")
		}
		return p.validateCustomManifest(namespace, release, p.manifest)
	}
	p.setCustomCapabilities(chartutil.DefaultCapabilities)
	values, err := chartutil.ToRenderValues(p.chart, p.Values, chartutil.ReleaseOptions{
		Name: release, Namespace: namespace, IsUpgrade: true, Revision: 1,
	}, chartutil.DefaultCapabilities)
	if err != nil {
		return fmt.Errorf("invalid custom render values")
	}
	rendered, err := engine.Render(p.chart, values)
	if err != nil {
		return fmt.Errorf("custom chart rendering failed: %w", err)
	}
	names := make([]string, 0, len(rendered))
	for filename := range rendered {
		if !strings.HasSuffix(filename, "/NOTES.txt") {
			names = append(names, filename)
		}
	}
	slices.Sort(names)
	var documents bytes.Buffer
	for _, filename := range names {
		if strings.TrimSpace(rendered[filename]) != "" {
			documents.WriteString("---\n")
			documents.WriteString(rendered[filename])
			documents.WriteByte('\n')
		}
	}
	return p.validateCustomManifest(namespace, release, documents.Bytes())
}

func (p *PreparedRelease) validateCustomManifest(namespace, release string, manifest []byte) error {
	if _, err := p.customContribution(); err != nil {
		return err
	}
	var bindings runtimebindings.Custom
	if p.Agent.Type != "custom" || p.Values["agentType"] != "custom" ||
		decodeObject(object(p.Values["runtime"])["bindings"], &bindings) != nil ||
		namespace != bindings.Namespace.Name || release != p.releaseName {
		return fmt.Errorf("Custom manifest identity mismatch")
	}
	if err := bindings.Validate(p.Agent); err != nil {
		return err
	}
	if err := validateCustomPlatformValues(p.Values, p.Agent, bindings.RestartRequestID); err != nil {
		return err
	}
	name := text(p.Values["fullnameOverride"])
	if name == "" {
		return fmt.Errorf("custom requires a platform resource name")
	}
	seen := map[string]bool{}
	platformObjects := p.customPlatformObjects(namespace)
	terminalObjects, err := p.terminalObjects(bindings)
	if err != nil {
		return err
	}
	copilotObjects, err := p.customCopilotObjects(bindings)
	if err != nil {
		return err
	}
	terminalObjects = append(terminalObjects, copilotObjects...)
	expectedTerminal := map[string]map[string]any{}
	seenTerminal := map[string]bool{}
	for _, object := range terminalObjects {
		key := text(object["kind"]) + "/" + text(object["metadata"].(map[string]any)["name"])
		expectedTerminal[key] = object
	}
	manifests := map[string]any{}
	var canonical bytes.Buffer
	decoder := kubeyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(manifest)))
	for {
		var obj map[string]any
		document, err := decoder.Read()
		if err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("invalid custom manifest")
		}
		if err := validateYAMLKeys(document); err != nil {
			return fmt.Errorf("ambiguous Custom manifest: %w", err)
		}
		raw, err := yaml.YAMLToJSONStrict(document)
		if err != nil {
			return fmt.Errorf("invalid or duplicate Custom YAML fields: %w", err)
		}
		strictErrors, err := strictjson.UnmarshalStrict(raw, &obj)
		if err != nil || len(strictErrors) != 0 {
			return fmt.Errorf("invalid or duplicate Custom JSON fields")
		}
		if len(obj) == 0 {
			continue
		}
		kind := text(obj["kind"])
		meta := object(obj["metadata"])
		key := kind + "/" + text(meta["name"])
		if object(meta["annotations"])["helm.sh/hook"] != nil ||
			(text(meta["namespace"]) != "" && meta["namespace"] != namespace) {
			return fmt.Errorf("unexpected duplicate, hook or namespace in custom manifest")
		}
		if expected, ok := expectedTerminal[key]; ok {
			if seenTerminal[key] || !sameJSON(obj, expected) {
				return fmt.Errorf("Custom terminal %s violates the platform plan", key)
			}
			seenTerminal[key] = true
		} else {
			if seen[kind] {
				return fmt.Errorf("unexpected duplicate kind in custom manifest")
			}
			seen[kind] = true
			wantName := name
			if kind == "ConfigMap" {
				wantName += "-config"
			}
			if expected, ok := platformObjects[kind]; ok {
				wantName = expected.name
			}
			if meta["name"] != wantName {
				return fmt.Errorf("unexpected custom resource name")
			}
			if err := p.validateCustomObject(obj, kind, name, bindings); err != nil {
				return err
			}
		}
		manifests[key] = obj
		encoded, err := json.Marshal(obj)
		if err != nil {
			return err
		}
		canonical.WriteString("---\n")
		canonical.Write(encoded)
		canonical.WriteByte('\n')
	}
	for _, kind := range []string{"StatefulSet", "ConfigMap", "ServiceAccount"} {
		if !seen[kind] {
			return fmt.Errorf("custom manifest missing %s", kind)
		}
	}
	for kind := range platformObjects {
		if !seen[kind] {
			return fmt.Errorf("custom manifest missing platform %s", kind)
		}
	}
	for key := range expectedTerminal {
		if !seenTerminal[key] {
			return fmt.Errorf("Custom manifest missing terminal %s", key)
		}
	}
	if seen["NetworkPolicy"] != (object(p.Values["networkPolicy"])["enabled"] == true) {
		return fmt.Errorf("custom network policy does not match platform configuration")
	}
	if seen["Service"] != (len(p.customServicePorts()) > 0) {
		return fmt.Errorf("custom HTTP Service does not match optional port")
	}
	raw, err := json.Marshal(manifests)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	if p.manifestHash != "" && p.manifestHash != digest {
		return fmt.Errorf("prepared Custom manifest changed after validation")
	}
	p.manifestHash = digest
	// Apply the representation that was checked, not YAML that a downstream
	// parser could interpret differently.
	p.manifest = append([]byte(nil), canonical.Bytes()...)
	return nil
}

func (p *PreparedRelease) inputDigest() string {
	raw, err := json.Marshal(struct {
		Values       map[string]any
		Agent        any
		PlanHash     string
		WorkloadHash string
	}{p.Values, p.Agent, p.PlanHash, p.WorkloadHash})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func decodeObject(value any, target any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	strictErrors, err := strictjson.UnmarshalStrict(raw, target)
	if err != nil {
		return err
	}
	if len(strictErrors) != 0 {
		return fmt.Errorf("invalid Custom fields: %v", strictErrors)
	}
	return nil
}

func onlyKeys(obj map[string]any, allowed ...string) bool {
	for key := range obj {
		if !slices.Contains(allowed, key) {
			return false
		}
	}
	return true
}

func customSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsNonRoot: ptr.To(true), AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true),
		Capabilities:   &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func (p *PreparedRelease) validateCustomObject(obj map[string]any, kind, name string, b runtimebindings.Custom) error {
	fail := func() error { return fmt.Errorf("custom %s violates the platform runtime plan", kind) }
	if !onlyKeys(obj, "apiVersion", "kind", "metadata", "spec", "data", "automountServiceAccountToken") {
		return fail()
	}
	metadata := object(obj["metadata"])
	if !onlyKeys(metadata, "name", "namespace", "labels", "annotations", "ownerReferences") {
		return fail()
	}
	var objectMeta metav1.ObjectMeta
	if decodeObject(metadata, &objectMeta) != nil {
		return fail()
	}
	for annotation := range objectMeta.Annotations {
		if strings.HasPrefix(annotation, "helm.sh/") {
			return fail()
		}
	}
	if kind != "StatefulSet" && kind != "ValidatingAdmissionPolicy" && kind != "ValidatingAdmissionPolicyBinding" &&
		len(objectMeta.OwnerReferences) != 0 {
		return fail()
	}
	if expected, ok := p.customPlatformObjects(b.Namespace.Name)[kind]; ok {
		if obj["apiVersion"] != expected.apiVersion || !sameJSON(obj["spec"], expected.spec) {
			return fail()
		}
		if kind == "ValidatingAdmissionPolicy" || kind == "ValidatingAdmissionPolicyBinding" {
			var meta metav1.ObjectMeta
			if decodeObject(obj["metadata"], &meta) != nil || !reflect.DeepEqual(meta.OwnerReferences, []metav1.OwnerReference{{
				APIVersion: "cetirun.microsoft.com/v1alpha1", Kind: "Agent", Name: b.AgentName, UID: types.UID(b.AgentUID),
				Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
			}}) {
				return fail()
			}
		}
		if kind == "PersistentVolume" && (objectMeta.Namespace != "" ||
			objectMeta.Annotations["pv.kubernetes.io/provisioned-by"] != "blob.csi.azure.com") {
			return fail()
		}
		return nil
	}
	switch kind {
	case "ConfigMap":
		data := object(obj["data"])
		expected := map[string]any{
			"init-home-common.sh": string(asset(p.chart, "configs/init-home-common.sh")),
			"custom-init-home.sh": string(asset(p.chart, "agents/custom/bootstrap/init-home.sh")),
		}
		if obj["apiVersion"] != "v1" || !sameJSON(data, expected) || data["custom-init-home.sh"] == "" {
			return fail()
		}
	case "ServiceAccount":
		if obj["apiVersion"] != "v1" || obj["automountServiceAccountToken"] != false ||
			len(object(object(obj["metadata"])["annotations"])) != 0 {
			return fail()
		}
	case "Service":
		if obj["apiVersion"] != "v1" || len(p.customServicePorts()) == 0 || !onlyKeys(object(obj["spec"]), "type", "ports", "selector") {
			return fail()
		}
		var service corev1.Service
		if decodeObject(obj, &service) != nil || service.Spec.Type != corev1.ServiceTypeClusterIP ||
			!reflect.DeepEqual(service.Spec.Selector, customLabels(p.chart.Name(), p.releaseName)) ||
			!reflect.DeepEqual(service.Spec.Ports, p.customServicePorts()) {
			return fail()
		}
	case "NetworkPolicy":
		if obj["apiVersion"] != "networking.k8s.io/v1" {
			return fail()
		}
		var policy networkv1.NetworkPolicy
		if decodeObject(obj, &policy) != nil ||
			!sameJSON(policy.Spec, p.customNetwork(b.Network)) {
			return fail()
		}
	case "StatefulSet":
		if obj["apiVersion"] != "apps/v1" || !onlyKeys(object(obj["spec"]), "replicas", "serviceName", "selector", "template", "updateStrategy", "volumeClaimTemplates", "persistentVolumeClaimRetentionPolicy") {
			return fail()
		}
		claims := []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{
			Name: "data", Labels: customLabels(p.chart.Name(), p.releaseName),
			Annotations: map[string]string{"cetirun.microsoft.com/agent-uid": b.AgentUID},
		}, Spec: b.Home.Claim.PVCSpec()}}
		var retention *appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy
		if p.homeRetention {
			retention = &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
				WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			}
		}
		var sts appsv1.StatefulSet
		if decodeObject(obj, &sts) != nil || sts.Spec.Replicas == nil ||
			*sts.Spec.Replicas != number(p.Values["replicaCount"]) ||
			sts.Spec.ServiceName != name || sts.Spec.UpdateStrategy.Type != appsv1.OnDeleteStatefulSetStrategyType ||
			sts.Spec.Selector == nil || !reflect.DeepEqual(sts.Spec.Selector.MatchLabels, customLabels(p.chart.Name(), p.releaseName)) {
			return fail()
		}
		if !equality.Semantic.DeepEqual(sts.Spec.VolumeClaimTemplates, claims) ||
			!reflect.DeepEqual(sts.Spec.PersistentVolumeClaimRetentionPolicy, retention) {
			return fail()
		}
		if !reflect.DeepEqual(sts.OwnerReferences, []metav1.OwnerReference{{
			APIVersion: "cetirun.microsoft.com/v1alpha1", Kind: "Agent", Name: b.AgentName, UID: types.UID(b.AgentUID),
			Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
		}}) || sts.Annotations["cetirun.microsoft.com/agent-uid"] != b.AgentUID ||
			sts.Annotations["cetirun.microsoft.com/custom-plan-hash"] != p.WorkloadHash {
			return fail()
		}
		annotations := sts.Spec.Template.Annotations
		annotationCount := 4 + len(object(p.Values["podAnnotations"]))
		if b.RestartRequestID != "" {
			if annotations[api.AnnotationRestartRequestID] != b.RestartRequestID {
				return fail()
			}
		}
		if !onlyKeys(object(object(object(obj["spec"])["template"])["metadata"]), "labels", "annotations") {
			return fail()
		}
		if !reflect.DeepEqual(sts.Spec.Template.Labels, customLabels(p.chart.Name(), p.releaseName)) ||
			len(annotations) != annotationCount ||
			annotations["cetirun.microsoft.com/custom-plan-hash"] != p.WorkloadHash ||
			annotations["cetirun.microsoft.com/model-revision"] != b.Model.Revision ||
			annotations["cetirun.microsoft.com/context-revision"] != b.Context.Revision ||
			annotations["checksum/config"] == "" {
			return fail()
		}
		for key, value := range object(p.Values["podAnnotations"]) {
			if actual, ok := annotations[key]; !ok || actual != value {
				return fail()
			}
		}
		podMap := object(object(object(obj["spec"])["template"])["spec"])
		if !onlyKeys(podMap, "serviceAccountName", "automountServiceAccountToken", "runtimeClassName",
			"nodeSelector", "affinity", "tolerations", "topologySpreadConstraints",
			"securityContext", "initContainers", "containers", "volumes", "imagePullSecrets") {
			return fail()
		}
		pod := sts.Spec.Template.Spec
		var pullSecrets []corev1.LocalObjectReference
		if b.PullSecretName != "" {
			pullSecrets = []corev1.LocalObjectReference{{Name: b.PullSecretName}}
		}
		if !reflect.DeepEqual(pod.ImagePullSecrets, pullSecrets) {
			return fail()
		}
		var scheduling corev1.PodSpec
		schedulingValues := map[string]any{}
		if len(object(p.Values["affinity"])) > 0 {
			schedulingValues["affinity"] = p.Values["affinity"]
		}
		for _, key := range []string{"tolerations", "topologySpreadConstraints"} {
			if len(array(p.Values[key])) > 0 {
				schedulingValues[key] = p.Values[key]
			}
		}
		if decodeObject(schedulingValues, &scheduling) != nil ||
			!equality.Semantic.DeepEqual(pod.Affinity, scheduling.Affinity) ||
			!equality.Semantic.DeepEqual(pod.Tolerations, scheduling.Tolerations) ||
			!equality.Semantic.DeepEqual(pod.TopologySpreadConstraints, scheduling.TopologySpreadConstraints) {
			return fail()
		}
		var expectedNodes map[string]string
		if err := decodeObject(p.Values["nodeSelector"], &expectedNodes); err != nil {
			return fail()
		}
		expectedClass := text(p.Values["runtimeClassName"])
		if (expectedClass == "" && pod.RuntimeClassName != nil) ||
			ptr.Deref(pod.RuntimeClassName, "") != expectedClass ||
			len(pod.NodeSelector) != len(expectedNodes) {
			return fail()
		}
		for key, value := range expectedNodes {
			if actual, exists := pod.NodeSelector[key]; !exists || actual != value {
				return fail()
			}
		}
		containerCount := 1
		if p.desktopEnabled() {
			containerCount++
		}
		initCount := 1
		if b.Memory != nil {
			initCount++
		}
		if len(pod.Containers) != containerCount || len(pod.InitContainers) != initCount ||
			pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken || pod.ServiceAccountName != name ||
			!reflect.DeepEqual(pod.SecurityContext, &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true),
				FSGroup: ptr.To(b.Home.FSGroup), FSGroupChangePolicy: ptr.To(corev1.FSGroupChangeOnRootMismatch),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}) {
			return fail()
		}
		if p.desktopEnabled() && !equality.Semantic.DeepEqual(pod.Containers[1], p.desktopContainer()) {
			return fail()
		}
		if b.Memory != nil && !equality.Semantic.DeepEqual(pod.InitContainers[0], memoryInit(b.Memory)) {
			return fail()
		}
		for _, key := range []string{"initContainers", "containers"} {
			for _, c := range array(podMap[key]) {
				if !onlyKeys(object(c), "name", "image", "imagePullPolicy", "command", "securityContext",
					"env", "ports", "startupProbe", "readinessProbe", "resources", "volumeMounts") {
					return fail()
				}
				if !onlyKeys(object(object(c)["resources"]), "requests", "limits") {
					return fail()
				}
			}
		}
		main, init := pod.Containers[0], pod.InitContainers[0]
		if b.Memory != nil {
			init = pod.InitContainers[1]
		}
		expectedResources, err := runtimebindings.ValidateResources(p.Values["resources"])
		if err != nil || !equality.Semantic.DeepEqual(main.Resources, expectedResources) {
			return fail()
		}
		for _, c := range array(podMap["containers"]) {
			if _, err := runtimebindings.ValidateResources(object(c)["resources"]); err != nil {
				return fail()
			}
		}
		env := []corev1.EnvVar{{Name: "HOME", Value: p.Agent.HomeDir},
			{Name: "OPENAI_BASE_URL", Value: b.Model.BaseURL},
			{Name: "OPENAI_API_KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: b.Model.Name}, Key: b.Model.Key,
			}}}}
		if p.Agent.Port != nil {
			env = append(env, corev1.EnvVar{Name: "PORT", Value: strconv.Itoa(int(*p.Agent.Port))})
		}
		if p.Agent.WebUI {
			env = append(env,
				corev1.EnvVar{Name: "CETIRUN_WEB_BASE_PATH", Value: "/ui/" + b.AgentName + "/"},
				corev1.EnvVar{Name: "CETIRUN_WEB_INSTANCE", Value: b.AgentUID})
		}
		if b.Memory != nil {
			env = append(env, memoryEnvironment(p.Agent.HomeDir)...)
		}
		if main.Name != p.chart.Name() || init.Name != "init-home-data" || len(main.Command) != 0 ||
			!reflect.DeepEqual(init.Command, []string{"/bin/sh", "/etc/cetirun/custom-init-home.sh"}) ||
			!reflect.DeepEqual(main.Env, env) || !reflect.DeepEqual(init.Env, p.customInitEnv()) {
			return fail()
		}
		for _, c := range []corev1.Container{main, init} {
			if c.Image != p.Agent.Image || c.ImagePullPolicy != corev1.PullIfNotPresent ||
				!reflect.DeepEqual(c.SecurityContext, customSecurityContext()) {
				return fail()
			}
		}
		volumes := customVolumes(name, b)
		if p.desktopEnabled() {
			volumes = append(volumes, corev1.Volume{Name: "tmp-desktop", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
		}
		if p.sharedSkillsEnabled() {
			_, pvc := p.sharedSkillsNames(b.Namespace.Name)
			volumes = append(volumes,
				corev1.Volume{Name: "shared-skills", VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc, ReadOnly: true}}})
		}
		if p.sharedSkillsEnabled() || b.Memory != nil {
			volumes = append(volumes, runtimebindings.ContextVolume())
		}
		mounts := p.customMainMounts()
		if b.Memory != nil {
			mounts = append(mounts, memoryMounts()...)
			volumes = append(volumes, memoryVolumes()...)
		}
		if !reflect.DeepEqual(main.VolumeMounts, mounts) ||
			!reflect.DeepEqual(init.VolumeMounts, p.customInitMounts()) || !reflect.DeepEqual(pod.Volumes, volumes) {
			return fail()
		}
		contribution, err := p.customContribution()
		if err != nil {
			return fail()
		}
		var ports []corev1.ContainerPort
		for _, port := range contribution.Ports {
			ports = append(ports, corev1.ContainerPort{Name: port.Name, ContainerPort: port.Number, Protocol: corev1.ProtocolTCP})
		}
		if !reflect.DeepEqual(main.Ports, ports) {
			return fail()
		}
		http := contribution.HTTP
		if http == nil {
			if main.StartupProbe != nil || main.ReadinessProbe != nil {
				return fail()
			}
		} else if !reflect.DeepEqual(main.StartupProbe, customProbe(http.StartupPath, http.PortName, 5, 60)) ||
			!reflect.DeepEqual(main.ReadinessProbe, customProbe(http.HealthPath, http.PortName, 10, 0)) {
			return fail()
		}
	default:
		return fmt.Errorf("unexpected Custom resource kind %s", kind)
	}
	return nil
}

func customLabels(chartName, release string) map[string]string {
	return map[string]string{"app.kubernetes.io/name": chartName, "app.kubernetes.io/instance": release}
}

func customProbe(path, port string, period, failures int32) *corev1.Probe {
	return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
		Path: path, Port: intstr.FromString(port),
	}}, PeriodSeconds: period, FailureThreshold: failures}
}

func (p *PreparedRelease) customContribution() (agents.Contribution, error) {
	var value struct {
		agents.Contribution
		Bindings          runtimebindings.Custom `json:"bindings"`
		ContractVersion   string                 `json:"contractVersion"`
		PlanHash          string                 `json:"planHash"`
		WorkloadHash      string                 `json:"workloadHash"`
		TerminalResources []map[string]any       `json:"terminalResources,omitempty"`
		CopilotResources  []map[string]any       `json:"copilotResources,omitempty"`
		TerminalExecRule  string                 `json:"terminalExecRule,omitempty"`
	}
	if err := decodeObject(p.Values["runtime"], &value); err != nil {
		return agents.Contribution{}, err
	}
	valid := value.ContractVersion == "v1" && reflect.DeepEqual(value.Agent, p.Agent) &&
		len(value.ConfigFiles) == 0 && value.ProfileID == "" && (!p.Agent.WebUI || p.Agent.Port != nil)
	expectedPorts := []agents.PortBinding{}
	var expectedHTTP *agents.HTTPBinding
	if p.Agent.Port != nil {
		expectedPorts = append(expectedPorts, agents.PortBinding{Name: "http", Number: *p.Agent.Port})
		expectedHTTP = &agents.HTTPBinding{PortName: "http", HealthPath: "/healthz", ReadinessPath: "/healthz", StartupPath: "/healthz"}
		if !p.Agent.WebUI {
			expectedHTTP.ChatPath = "/v1/chat/completions"
			expectedHTTP.ModelsPath = "/v1/models"
		}
	}
	valid = valid && reflect.DeepEqual(value.Ports, expectedPorts) && reflect.DeepEqual(value.HTTP, expectedHTTP)
	if !valid {
		return agents.Contribution{}, fmt.Errorf("Custom contribution violates the v1 runtime contract")
	}
	return value.Contribution, nil
}

func customVolumes(name string, b runtimebindings.Custom) []corev1.Volume {
	keys := make([]string, 0, len(b.Context.Files))
	for key := range b.Context.Files {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	items := []corev1.KeyToPath{}
	for _, key := range keys {
		items = append(items, corev1.KeyToPath{Key: key, Path: b.Context.Files[key]})
	}
	contextSource := corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: b.Context.Name}, DefaultMode: ptr.To[int32](0444), Items: items,
	}}
	return []corev1.Volume{
		{Name: "context", VolumeSource: contextSource},
		{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: name + "-config"}, DefaultMode: ptr.To[int32](0444),
		}}},
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
}
