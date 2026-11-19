// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxpolicy

import (
	"encoding/json"
	"fmt"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/egresscapture"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/ingressmtls"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ManagedProxyBootstrap composes outbound capture into the same controlled
// Envoy. A FQDN-only plan has no TLS transport, certificate reference or route.
func ManagedProxyBootstrap(ingress *ingressmtls.Plan, egress *egresscapture.Plan) ([]byte, error) {
	if egress == nil {
		if ingress == nil {
			return nil, nil
		}
		return ingress.Bootstrap()
	}
	config, err := egress.Config()
	if err != nil {
		return nil, err
	}
	bootstrap := map[string]any{}
	if ingress != nil {
		if ingress.Image != egress.EnvoyImage {
			return nil, fmt.Errorf("%w: ingress and egress must approve the same controlled Envoy image", ErrConflict)
		}
		raw, err := ingress.Bootstrap()
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &bootstrap); err != nil {
			return nil, err
		}
	}
	var outbound struct {
		Listeners []any `json:"listeners"`
		Clusters  []any `json:"clusters"`
	}
	if err := json.Unmarshal(config.StaticResources, &outbound); err != nil {
		return nil, err
	}
	static, _ := bootstrap["static_resources"].(map[string]any)
	if static == nil {
		static = map[string]any{"listeners": []any{}, "clusters": []any{}}
	}
	static["listeners"] = append(static["listeners"].([]any), outbound.Listeners...)
	static["clusters"] = append(static["clusters"].([]any), outbound.Clusters...)
	// Applications can observe readiness/certificates/stats, but cannot invoke
	// mutating admin handlers. The actual admin socket is proxy-only.
	bootstrap["admin"] = map[string]any{"address": map[string]any{"pipe": map[string]any{
		"path": egresscapture.AdminSocketPath, "mode": 0600,
	}}}
	static["clusters"] = append(static["clusters"].([]any), map[string]any{
		"name": "cetirun_proxy_admin", "type": "STATIC", "connect_timeout": "1s",
		"load_assignment": map[string]any{"cluster_name": "cetirun_proxy_admin",
			"endpoints": []any{map[string]any{"lb_endpoints": []any{map[string]any{
				"endpoint": map[string]any{"address": map[string]any{"pipe": map[string]any{"path": egresscapture.AdminSocketPath}}},
			}}}}},
	})
	routes := []any{}
	for _, path := range []string{"/ready", "/certs", "/stats"} {
		routes = append(routes, map[string]any{
			"match": map[string]any{"path": path, "headers": []any{map[string]any{
				"name": ":method", "string_match": map[string]any{"exact": "GET"},
			}}},
			"route": map[string]any{"cluster": "cetirun_proxy_admin", "timeout": "2s"},
		})
	}
	static["listeners"] = append(static["listeners"].([]any), map[string]any{
		"name": "cetirun_proxy_health",
		"address": map[string]any{"socket_address": map[string]any{
			"address": "127.0.0.1", "port_value": ingressmtls.AdminPort,
		}},
		"filter_chains": []any{map[string]any{"filters": []any{map[string]any{
			"name": "envoy.filters.network.http_connection_manager",
			"typed_config": map[string]any{
				"@type":       "type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager",
				"stat_prefix": "cetirun_proxy_health", "codec_type": "AUTO",
				"route_config": map[string]any{"name": "health", "virtual_hosts": []any{map[string]any{
					"name": "health", "domains": []any{"*"}, "routes": routes,
				}}},
				"http_filters": []any{map[string]any{
					"name": "envoy.filters.http.router", "typed_config": map[string]any{
						"@type": "type.googleapis.com/envoy.extensions.filters.http.router.v3.Router",
					},
				}},
			},
		}}}},
	})
	bootstrap["static_resources"] = static
	return json.Marshal(bootstrap)
}

func ManagedProxyDigest(ingress *ingressmtls.Plan, egress *egresscapture.Plan) (string, error) {
	if egress == nil {
		if ingress == nil {
			return "", nil
		}
		return ingress.ConfigDigest()
	}
	bootstrap, err := ManagedProxyBootstrap(ingress, egress)
	if err != nil {
		return "", err
	}
	script, err := egress.Script(ingressmtls.AdminPort)
	if err != nil {
		return "", err
	}
	return digest(struct {
		Bootstrap json.RawMessage
		Script    string
		Egress    *egresscapture.Plan
	}{bootstrap, script, egress})
}

func ManagedProxyResources(input CompileInput, ingress *ingressmtls.Plan, egress *egresscapture.Plan, revision string) ([]unstructured.Unstructured, error) {
	if egress == nil {
		if ingress == nil {
			return nil, nil
		}
		return IngressMTLSResources(input, *ingress, revision)
	}
	var objects []unstructured.Unstructured
	if ingress != nil {
		var err error
		objects, err = IngressMTLSResources(input, *ingress, revision)
		if err != nil {
			return nil, err
		}
	} else {
		metadata, err := ownedMetadata(input, ingressmtls.ConfigName, input.Namespace)
		if err != nil {
			return nil, err
		}
		metadata.Annotations[RevisionAnnotation] = revision
		cm, err := manifest(corev1.ConfigMap{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: metadata,
		})
		if err != nil {
			return nil, err
		}
		objects = []unstructured.Unstructured{cm}
	}
	bootstrap, err := ManagedProxyBootstrap(ingress, egress)
	if err != nil {
		return nil, err
	}
	script, err := egress.Script(ingressmtls.AdminPort)
	if err != nil {
		return nil, err
	}
	data, _, err := unstructured.NestedStringMap(objects[0].Object, "data")
	if err != nil {
		return nil, err
	}
	if data == nil {
		data = map[string]string{}
	}
	data["envoy.json"], data["capture.sh"] = string(bootstrap), script
	if err := unstructured.SetNestedStringMap(objects[0].Object, data, "data"); err != nil {
		return nil, err
	}
	return objects, nil
}
