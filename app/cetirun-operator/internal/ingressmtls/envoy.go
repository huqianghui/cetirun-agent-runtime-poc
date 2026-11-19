// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ingressmtls

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

func (p Plan) ConfigDigest() (string, error) {
	raw, err := p.Bootstrap()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// Bootstrap terminates TLS inside the Agent Pod. Every application connection
// stays on loopback; no fallback, transparent capture or outbound proxy exists.
func (p Plan) Bootstrap() ([]byte, error) {
	listeners, clusters := []any{}, []any{}
	clientNames := []any{map[string]any{"san_type": "URI", "matcher": map[string]string{"exact": p.ClientURI}}}
	for _, client := range p.ChatClients {
		clientNames = append(clientNames, map[string]any{"san_type": "URI", "matcher": map[string]string{"exact": client.URI}})
	}
	for _, port := range p.Ports() {
		clusters = append(clusters, map[string]any{
			"name": port.Name, "type": "STATIC", "connect_timeout": "5s",
			"load_assignment": map[string]any{
				"cluster_name": port.Name,
				"endpoints": []any{map[string]any{"lb_endpoints": []any{map[string]any{
					"endpoint": map[string]any{"address": socket("127.0.0.1", port.Backend)},
				}}}},
			},
		})
		address := socket("::", port.TLS)
		address["socket_address"].(map[string]any)["ipv4_compat"] = true
		listeners = append(listeners, map[string]any{
			"name": port.Name, "address": address,
			"filter_chains": []any{map[string]any{
				"transport_socket": map[string]any{
					"name": "envoy.transport_sockets.tls",
					"typed_config": map[string]any{
						"@type":                                "type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.DownstreamTlsContext",
						"require_client_certificate":           true,
						"disable_stateless_session_resumption": true,
						"disable_stateful_session_resumption":  true,
						"common_tls_context": map[string]any{
							"tls_params": map[string]any{"tls_minimum_protocol_version": "TLSv1_3"},
							"tls_certificates": []any{map[string]any{
								"certificate_chain": map[string]string{"filename": TLSPath + "/tls.crt"},
								"private_key":       map[string]string{"filename": TLSPath + "/tls.key"},
							}},
							"validation_context": map[string]any{
								"trusted_ca":                    map[string]string{"filename": TLSPath + "/ca.crt"},
								"match_typed_subject_alt_names": clientNames,
							},
						},
					},
				},
				"filters": []any{map[string]any{
					"name": "envoy.filters.network.tcp_proxy",
					"typed_config": map[string]any{
						"@type":       "type.googleapis.com/envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy",
						"stat_prefix": port.Name, "cluster": port.Name,
						"idle_timeout": "300s", "max_downstream_connection_duration": "300s",
					},
				}},
			}},
		})
	}
	return json.Marshal(map[string]any{
		"admin":            map[string]any{"address": socket("127.0.0.1", AdminPort)},
		"static_resources": map[string]any{"listeners": listeners, "clusters": clusters},
	})
}

func socket(address string, port int32) map[string]any {
	return map[string]any{"socket_address": map[string]any{"address": address, "port_value": port}}
}
