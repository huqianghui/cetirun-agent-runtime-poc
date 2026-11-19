// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxpolicy

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"slices"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/ingressmtls"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func validateCustomMTLS(input CompileInput, plan *ingressmtls.Plan) error {
	if plan.Harness != "custom" {
		return nil
	}
	custom := input.CustomIngress
	if custom == nil || plan.Port != custom.Port ||
		plan.Terminal != (input.TerminalIngress != nil) ||
		input.TerminalIngress != nil && input.TerminalIngress.Port != 7681 ||
		len(custom.ChatCallers) != len(plan.ChatClients) {
		return fmt.Errorf("%w: mTLS approval must match Custom HTTP, terminal and Chat callers", ErrConflict)
	}
	for _, caller := range custom.ChatCallers {
		if !slices.ContainsFunc(plan.ChatClients, func(client ingressmtls.ChatClient) bool {
			return client.Secret.Namespace == caller.Namespace && maps.Equal(client.PodLabels, caller.PodLabels)
		}) {
			return fmt.Errorf("%w: Custom Chat caller lacks its own approved mTLS identity", ErrConflict)
		}
	}
	return nil
}

func mtlsDestination(destination Destination, plan ingressmtls.Plan) (Destination, error) {
	if destination.Peer.Namespace != plan.Gateway.Namespace || len(destination.Peer.PodSelector) == 0 {
		return destination, fmt.Errorf("%w: mTLS requires the approved Gateway's explicit ingress Pod selector", ErrConflict)
	}
	ports := make([]api.AgentPolicyPort, 0, len(plan.Ports()))
	for _, port := range plan.Ports() {
		if !slices.ContainsFunc(destination.Ports, func(allowed api.AgentPolicyPort) bool {
			return allowed.Protocol == corev1.ProtocolTCP && port.Backend == allowed.Port
		}) {
			return destination, fmt.Errorf("%w: mTLS cannot add an unapproved application ingress port", ErrConflict)
		}
		ports = append(ports, api.AgentPolicyPort{Port: port.TLS, Protocol: corev1.ProtocolTCP})
	}
	destination.Ports = ports
	return destination, nil
}

func IngressMTLSResources(input CompileInput, plan ingressmtls.Plan, revision string) ([]unstructured.Unstructured, error) {
	metadata, err := ownedMetadata(input, ingressmtls.ConfigName, input.Namespace)
	if err != nil {
		return nil, err
	}
	metadata.Annotations[RevisionAnnotation] = revision
	config, err := plan.Bootstrap()
	if err != nil {
		return nil, err
	}
	data := map[string]string{"envoy.json": string(config)}
	if plan.Harness == "custom" && plan.Terminal {
		config, err := plan.TerminalEndpoint().Bootstrap()
		if err != nil {
			return nil, err
		}
		data["terminal-envoy.json"] = string(config)
	}
	cm, err := manifest(corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: metadata,
		Data: data,
	})
	if err != nil {
		return nil, err
	}
	destination := func(plan ingressmtls.Plan) unstructured.Unstructured {
		dr := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "networking.istio.io/v1beta1", "kind": "DestinationRule",
			"spec": map[string]any{
				"host": plan.ServiceHost, "exportTo": []any{plan.Gateway.Namespace},
				"trafficPolicy": map[string]any{
					"tls": map[string]any{
						"mode": "MUTUAL", "credentialName": plan.ClientSecret.Name,
						"sni": plan.ServerName, "subjectAltNames": []any{plan.ServerName},
						"insecureSkipVerify": false,
					},
					"connectionPool": map[string]any{"tcp": map[string]any{"maxConnectionDuration": "300s"}},
				},
			},
		}}
		dr.SetName(metadata.Name)
		dr.SetNamespace(metadata.Namespace)
		dr.SetLabels(metadata.Labels)
		dr.SetAnnotations(metadata.Annotations)
		dr.SetOwnerReferences(metadata.OwnerReferences)
		return *dr
	}
	result := []unstructured.Unstructured{cm, destination(plan)}
	if plan.Harness == "custom" && plan.Terminal {
		dr := destination(plan.TerminalEndpoint())
		dr.SetName(metadata.Name + "-terminal")
		result = append(result, dr)
	}
	for _, client := range plan.ChatClients {
		dr := destination(plan)
		hash := sha256.Sum256([]byte(plan.AgentUID))
		dr.SetName(fmt.Sprintf("%s-%x", metadata.Name, hash[:16]))
		dr.SetNamespace(client.Secret.Namespace)
		if err := unstructured.SetNestedStringSlice(dr.Object, []string{"."}, "spec", "exportTo"); err != nil {
			return nil, err
		}
		if err := unstructured.SetNestedStringMap(dr.Object, client.PodLabels, "spec", "workloadSelector", "matchLabels"); err != nil {
			return nil, err
		}
		if err := unstructured.SetNestedField(dr.Object, client.Secret.Name, "spec", "trafficPolicy", "tls", "credentialName"); err != nil {
			return nil, err
		}
		result = append(result, dr)
	}
	return result, nil
}
