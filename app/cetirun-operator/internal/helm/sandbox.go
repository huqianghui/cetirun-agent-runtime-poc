// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/agents"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kubeyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
)

// PrepareSandboxChart keeps override provenance until validation, applies the
// complete protected bundle last, and uses the existing native Helm renderer.
// The sealed zero-replica result can only be applied by guarded StageSandbox;
// InstallOrUpgrade explicitly refuses it, and staging never grants execution.
func PrepareSandboxChart(chrt *chart.Chart, platform, user map[string]any, bundle *sandboxworkload.Bundle) (*PreparedRelease, error) {
	return PrepareSandboxChartWithHarness(chrt, platform, user, bundle, nil)
}

// PrepareSandboxChartWithHarness uses the supplied harness with the same
// schema, protected bundle, render and seal validation. Nil selects the built-in registry.
func PrepareSandboxChartWithHarness(chrt *chart.Chart, platform, user map[string]any, bundle *sandboxworkload.Bundle, harness agents.Harness) (*PreparedRelease, error) {
	if bundle == nil || chrt == nil || chrt.Metadata == nil || chrt.Metadata.Name != "cetirun" || len(chrt.CRDObjects()) != 0 {
		return nil, fmt.Errorf("Sandbox requires a native chart and a resolved protected bundle")
	}
	if platform[sandboxworkload.ValuesKey] != nil || platform["runtime"] != nil {
		return nil, fmt.Errorf("protected runtime values cannot be supplied as Helm inputs")
	}
	input := bundle.Identity()
	if err := checkCustomSkillMarketplace(chrt, input); err != nil {
		return nil, err
	}
	if err := sandboxworkload.ValidateUserValues(user, input.AuthorizedSecrets); err != nil {
		return nil, err
	}
	var request AgentRequest
	if err := decodeObject(platform["agentRequest"], &request); err != nil || request.Type != input.AgentType {
		return nil, fmt.Errorf("native request does not match the Sandbox bundle")
	}
	if request.TailscaleEnabled || present(object(platform["secrets"])["tailscaleAuthKey"]) {
		return nil, fmt.Errorf("Tailscale is unsupported by the sidecar-free Sandbox contract")
	}
	raw, err := json.Marshal(platform)
	if err != nil {
		return nil, err
	}
	var merged map[string]any
	if err := json.Unmarshal(raw, &merged); err != nil {
		return nil, err
	}
	// CoalesceTables gives the already-validated user map priority; it is never
	// allowed to see an unvalidated user extension path.
	raw, err = json.Marshal(user)
	if err != nil {
		return nil, err
	}
	var overrides map[string]any
	if err := json.Unmarshal(raw, &overrides); err != nil {
		return nil, err
	}
	merged = chartutil.CoalesceTables(overrides, merged)
	merged, err = applySandboxChartValues(chrt, merged, bundle)
	if err != nil {
		return nil, err
	}
	if options, present := platform["memoryOptions"]; present {
		merged["memoryOptions"] = options
	}
	prepared, err := prepareRuntimeChart(chrt, merged, false, harness)
	if err != nil {
		return nil, configurationError(err)
	}
	prepared.Values, err = applySandboxChartValues(chrt, prepared.Values, bundle)
	if err != nil {
		return nil, err
	}
	prepared.sandboxBundle, prepared.releaseName = bundle, input.ReleaseName
	if err := prepared.prepareAndValidateSandboxRender(); err != nil {
		return nil, configurationError(err)
	}
	prepared.sealedInput = prepared.inputDigest()
	return prepared, nil
}

// ReconcileSandbox reuses the already loaded chart and normal feature values.
// The controller publishes its current admission expectation before applying it.
func (p *PreparedRelease) ReconcileSandbox(values map[string]any, bundle *sandboxworkload.Bundle,
	guard func(context.Context) error,
) (*PreparedRelease, error) {
	return p.ReconcileSandboxWithPreflight(values, bundle, guard, guard, guard, nil, nil)
}

// ReconcileSandboxWithPreflight separates preflight and manifest authority
// from the checkpoint recorded immediately before a workload write.
func (p *PreparedRelease) ReconcileSandboxWithPreflight(values map[string]any, bundle *sandboxworkload.Bundle,
	preflight, renderGuard, guard func(context.Context) error,
	bindingCreate func(context.Context, unstructured.Unstructured) (func(context.Context) error, error),
	workloadCreate func(context.Context) (func(context.Context) error, error),
) (*PreparedRelease, error) {
	if p == nil || p.chart == nil || bundle == nil || !bundle.Identity().Reconciled || preflight == nil || renderGuard == nil || guard == nil {
		return nil, fmt.Errorf("Sandbox reconciliation requires a current bundle and guarded apply")
	}
	prepared, err := PrepareSandboxChart(p.chart, values, nil, bundle)
	if err != nil {
		return nil, err
	}
	prepared.sandboxPreflight, prepared.sandboxRenderGuard, prepared.sandboxGuard = preflight, renderGuard, guard
	prepared.sandboxBindingCreate = bindingCreate
	prepared.sandboxWorkloadCreate = workloadCreate
	return prepared, nil
}

func (p *PreparedRelease) ReconcileExpectation() (sandboxworkload.ReconcileExpectation, error) {
	if p.sandboxReconcile == nil || p.sealedInput != p.inputDigest() {
		return sandboxworkload.ReconcileExpectation{}, fmt.Errorf("current Sandbox render expectation is unavailable")
	}
	raw, err := json.Marshal(p.sandboxReconcile)
	var result sandboxworkload.ReconcileExpectation
	if err == nil {
		err = json.Unmarshal(raw, &result)
	}
	return result, err
}

func applySandboxChartValues(chrt *chart.Chart, values map[string]any, bundle *sandboxworkload.Bundle) (map[string]any, error) {
	values, err := bundle.Apply(values)
	if err != nil {
		return nil, err
	}
	protected := object(values[sandboxworkload.ValuesKey])
	proxies, present := protected["trustedProxyIPs"]
	if !present {
		return values, nil
	}
	var schema any
	if len(chrt.Schema) != 0 {
		if err := json.Unmarshal(chrt.Schema, &schema); err != nil {
			return nil, fmt.Errorf("Sandbox chart has an invalid schema")
		}
	}
	properties := object(object(schema)["properties"])
	sandboxProperties := object(object(properties[sandboxworkload.ValuesKey])["properties"])
	if _, supported := sandboxProperties["trustedProxyIPs"]; !supported {
		// Older pinned charts have a closed protected-values schema, but their
		// native template already consumes this existing OpenClaw config field.
		config := ensureObject(ensureObject(values, "openclaw"), "config")
		ensureObject(config, "gateway")["trustedProxies"] = proxies
		delete(protected, "trustedProxyIPs")
	}
	return values, nil
}

// SandboxTemplate returns a copy of the chart-rendered template. It has no
// owner/Pod UID and cannot be used as an execution or enforcement observation.
func (p *PreparedRelease) SandboxTemplate() (*corev1.PodTemplateSpec, error) {
	if p.sandboxBundle == nil || p.sandboxTemplate == nil || p.sealedInput != p.inputDigest() {
		return nil, fmt.Errorf("Sandbox render is absent or changed after validation")
	}
	return p.sandboxTemplate.DeepCopy(), nil
}

func (p *PreparedRelease) SandboxManifest() ([]byte, error) {
	if _, err := p.SandboxTemplate(); err != nil {
		return nil, err
	}
	return slices.Clone(p.manifest), nil
}

// prepareAndValidateSandboxRender normalizes native namespace and ownership
// before validating and sealing the complete manifest.
func (p *PreparedRelease) prepareAndValidateSandboxRender() error {
	input := p.sandboxBundle.Identity()
	if input.Reconciled {
		p.sandboxReconcile = &sandboxworkload.ReconcileExpectation{
			Version:         sandboxworkload.ReconcileExpectationVersion,
			Agent:           sandboxworkload.ObjectIdentity{Name: input.AgentName, UID: input.Resolution.Binding.AgentUID},
			AgentGeneration: input.AgentGeneration, Namespace: input.Namespace,
			Binding: input.Controls.Binding, Revision: input.Controls.Revision,
			Home:                   sandboxworkload.ObjectIdentity{Name: input.Home.Name, UID: input.Home.UID},
			Controls:               slices.Clone(input.Controls.Objects),
			IdentityServiceAccount: input.Controls.ServiceAccountSource,
		}
		if input.Suspended {
			p.sandboxReconcile.Controls = slices.DeleteFunc(p.sandboxReconcile.Controls, sandboxworkload.KubernetesBindingResource)
		}
		if input.AgentType == "custom" {
			p.sandboxReconcile.CustomIngressTLS = input.Controls.IngressMTLS
		}
		terminalValues := object(object(p.Values[sandboxworkload.ValuesKey])["terminal"])
		if terminal := input.Terminal; terminal != nil && terminalValues["enabled"] == true {
			p.sandboxReconcile.Terminal = &sandboxworkload.TerminalTarget{
				ServiceAccountName: terminal.ServiceAccountName, TargetWorkload: input.WorkloadName, Container: "cetirun",
			}
		}
	}
	values, err := chartutil.ToRenderValues(p.chart, p.Values, chartutil.ReleaseOptions{
		Name: input.ReleaseName, Namespace: input.Namespace.Name, IsInstall: true, Revision: 1,
	}, chartutil.DefaultCapabilities)
	if err != nil {
		return err
	}
	rendered, err := engine.Render(p.chart, values)
	if err != nil {
		return fmt.Errorf("Sandbox chart render failed: %w", err)
	}
	compiled := map[string]unstructured.Unstructured{}
	for _, object := range input.Controls.Objects {
		compiled[sandboxObjectKey(object)] = object
	}
	files := make([]string, 0, len(rendered))
	for file := range rendered {
		if !strings.HasSuffix(file, "/NOTES.txt") {
			files = append(files, file)
		}
	}
	slices.Sort(files)
	seen, native := map[string]bool{}, map[string]bool{}
	var manifest bytes.Buffer
	for _, file := range files {
		reader := kubeyaml.NewYAMLReader(bufio.NewReader(strings.NewReader(rendered[file])))
		for {
			document, err := reader.Read()
			if err == io.EOF {
				break
			}
			if err != nil || validateYAMLKeys(document) != nil {
				return fmt.Errorf("invalid or ambiguous Sandbox manifest")
			}
			raw, err := yaml.YAMLToJSONStrict(document)
			if err != nil {
				return fmt.Errorf("invalid Sandbox YAML")
			}
			var object unstructured.Unstructured
			if err := json.Unmarshal(raw, &object.Object); err != nil {
				return err
			}
			if len(object.Object) == 0 {
				continue
			}
			_, exactCompilerObject := compiled[sandboxObjectKey(object)]
			if !exactCompilerObject && object.GetNamespace() == "" {
				object.SetNamespace(input.Namespace.Name)
			}
			key := sandboxObjectKey(object)
			if seen[key] {
				return fmt.Errorf("duplicate Sandbox object")
			}
			seen[key] = true
			for annotation := range object.GetAnnotations() {
				if strings.HasPrefix(annotation, "helm.sh/") {
					return fmt.Errorf("Sandbox rendering cannot contain lifecycle hooks")
				}
			}
			if expected, exists := compiled[key]; exists {
				want, wantErr := json.Marshal(expected.Object)
				got, gotErr := json.Marshal(object.Object)
				if wantErr != nil || gotErr != nil || !bytes.Equal(want, got) {
					return fmt.Errorf("rendered compiler resource differs from the approved output")
				}
				delete(compiled, key)
				if input.Suspended && sandboxworkload.KubernetesBindingResource(object) {
					// Validate the full compiler output, but never recreate
					// revoked workload grants in a zero-replica suspension.
					continue
				}
			} else {
				if object.GetNamespace() != input.Namespace.Name {
					return fmt.Errorf("foreign native Sandbox object")
				}
				if !input.Reconciled && native[object.GetKind()] {
					return fmt.Errorf("unexpected additional native Sandbox object")
				}
				owner := []metav1.OwnerReference{sandboxworkload.AgentOwner(input.AgentName, input.Resolution.Binding.AgentUID)}
				if input.Reconciled || object.GetKind() == "ConfigMap" || object.GetKind() == "Service" {
					if len(object.GetOwnerReferences()) != 0 && !equality.Semantic.DeepEqual(object.GetOwnerReferences(), owner) {
						return fmt.Errorf("native Sandbox source has foreign ownership")
					}
					object.SetOwnerReferences(owner)
				}
				validate := p.validateSandboxNative
				if input.Reconciled {
					validate = p.validateReconciledObject
				}
				if err := validate(object); err != nil {
					return err
				}
				if input.Reconciled && (object.GetKind() == "ConfigMap" || object.GetKind() == "ServiceAccount" || object.GetKind() == "NetworkPolicy" || object.GetKind() == "Service" || object.GetKind() == "RoleBinding") {
					p.sandboxReconcile.Controls = append(p.sandboxReconcile.Controls, *object.DeepCopy())
				}
				native[object.GetKind()] = true
			}
			raw, err = json.Marshal(object.Object)
			if err != nil {
				return err
			}
			manifest.WriteString("---\n")
			manifest.Write(raw)
			manifest.WriteByte('\n')
		}
	}
	if len(compiled) != 0 || !native["StatefulSet"] || !native["ConfigMap"] || !native["Service"] {
		return fmt.Errorf("Sandbox render omitted a required workload or compiler resource")
	}
	if p.sandboxOpenClawPortalSignIn() && !seen["v1/ConfigMap/"+input.Namespace.Name+"/"+input.WorkloadName+"-config"] {
		return fmt.Errorf("Sandbox Portal sign-in requires the validated OpenClaw configuration")
	}
	p.manifest = slices.Clone(manifest.Bytes())
	sum := sha256.Sum256(p.manifest)
	p.manifestHash, p.PlanHash = hex.EncodeToString(sum[:]), input.Controls.Revision
	p.WorkloadHash = p.manifestHash
	if p.sandboxReconcile != nil {
		p.sandboxReconcile.PlanHash = p.manifestHash
	}
	return nil
}

func sandboxObjectKey(object unstructured.Unstructured) string {
	return object.GetAPIVersion() + "/" + object.GetKind() + "/" + object.GetNamespace() + "/" + object.GetName()
}

func (p *PreparedRelease) validateSandboxNative(object unstructured.Unstructured) error {
	input := p.sandboxBundle.Identity()
	name := input.WorkloadName
	switch object.GetAPIVersion() + "/" + object.GetKind() {
	case "apps/v1/StatefulSet":
		var stateful appsv1.StatefulSet
		if err := decodeObject(object.Object, &stateful); err != nil {
			return err
		}
		owner := sandboxworkload.AgentOwner(input.AgentName, input.Resolution.Binding.AgentUID)
		if stateful.Name != name || ptr.Deref(stateful.Spec.Replicas, -1) != 0 ||
			stateful.Spec.ServiceName != name || len(stateful.Spec.VolumeClaimTemplates) != 0 ||
			!equality.Semantic.DeepEqual(stateful.OwnerReferences, []metav1.OwnerReference{owner}) ||
			stateful.Spec.Selector == nil || len(stateful.Spec.Selector.MatchExpressions) != 0 {
			return fmt.Errorf("Sandbox StatefulSet identity, hold or HOME was modified")
		}
		for key, value := range stateful.Spec.Selector.MatchLabels {
			if stateful.Spec.Template.Labels[key] != value {
				return fmt.Errorf("Sandbox selector differs from the Pod template")
			}
		}
		if len(stateful.Spec.Selector.MatchLabels) == 0 {
			return fmt.Errorf("Sandbox workload selector cannot be empty")
		}
		if err := p.sandboxBundle.ValidateTemplate(&stateful.Spec.Template, p.Agent.Image); err != nil {
			return err
		}
		if err := p.validateSandboxOpenClawEnvironment(stateful.Spec.Template.Spec); err != nil {
			return err
		}
		p.sandboxTemplate = stateful.Spec.Template.DeepCopy()
	case "v1/ConfigMap":
		var config corev1.ConfigMap
		if err := decodeObject(object.Object, &config); err != nil || config.Name != name+"-config" || len(config.BinaryData) != 0 {
			return fmt.Errorf("unexpected native Sandbox configuration")
		}
		if err := p.validateSandboxOpenClawConfig(config); err != nil {
			return err
		}
	case "v1/Service":
		var service corev1.Service
		if err := decodeObject(object.Object, &service); err != nil || service.Name != name ||
			service.Spec.Type != corev1.ServiceTypeClusterIP || len(service.Spec.ExternalIPs) != 0 ||
			service.Spec.ExternalName != "" || service.Spec.LoadBalancerIP != "" || service.Spec.LoadBalancerClass != nil {
			return fmt.Errorf("Sandbox Service must remain a private ClusterIP")
		}
		for _, port := range service.Spec.Ports {
			if port.NodePort != 0 {
				return fmt.Errorf("Sandbox cannot expose a NodePort")
			}
		}
	default:
		return fmt.Errorf("unexpected Sandbox object %s", object.GetKind())
	}
	return nil
}
