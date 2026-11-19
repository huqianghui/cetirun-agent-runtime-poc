// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package egresscapture builds observed-SNI controls for registered runtimes.
// Builders do not themselves install capture or authorize a Pod.
package egresscapture

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	ProxyUID          = 1337
	CapturePort       = 15001
	InspectionTimeout = time.Second
	MaxHostnames      = 1024
	MaxConfigBytes    = 512 * 1024

	IPv4TimeoutCounter = "listener.cetirun_egress_ipv4.downstream_pre_cx_timeout"
	IPv6TimeoutCounter = "listener.cetirun_egress_ipv6.downstream_pre_cx_timeout"

	captureChain = "CETIRUN_FQDN_OUT"
)

// Config contains egress-only static_resources and argv for a controlled init.
// The caller owns the shared Envoy bootstrap, admin/metrics endpoint and lifecycle.
// Commands must run with the corresponding iptables/ip6tables binary in the
// workload network namespace, never on the host. They do not flush existing rules.
type Config struct {
	StaticResources json.RawMessage `json:"staticResources"`
	IPv4Commands    [][]string      `json:"ipv4Commands"`
	IPv6Commands    [][]string      `json:"ipv6Commands"`
}

type object = map[string]any

// Build returns nil for an empty list: no listener, cluster or capture commands.
// Names follow AgentPolicy's exact lowercase ASCII hostname syntax. Matching is
// Envoy's exact comparison against its lowercase-normalized requested_server_name,
// not DNS resolution or suffix matching. A timeout continues uninspected.
func Build(blockedFQDNs []string) (*Config, error) {
	if len(blockedFQDNs) == 0 {
		return nil, nil
	}
	if len(blockedFQDNs) > MaxHostnames {
		return nil, fmt.Errorf("blockedFQDNs exceeds %d input entries", MaxHostnames)
	}
	names := slices.Clone(blockedFQDNs)
	for i, name := range names {
		if len(name) > 253 {
			return nil, fmt.Errorf("blockedFQDNs[%d] exceeds 253 bytes", i)
		}
		valid := len(validation.IsDNS1123Subdomain(name)) == 0
		for _, label := range strings.Split(name, ".") {
			valid = valid && len(validation.IsDNS1123Label(label)) == 0
		}
		if _, err := netip.ParseAddr(name); err == nil {
			valid = false
		}
		if !valid {
			return nil, fmt.Errorf("blockedFQDNs[%d] must be an exact lowercase ASCII hostname, not an IP, URL or wildcard", i)
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	permissions := make([]object, 0, len(names))
	for _, name := range names {
		permissions = append(permissions, object{"requested_server_name": object{"exact": name}})
	}
	filters := []object{
		filter("envoy.filters.network.rbac", "envoy.extensions.filters.network.rbac.v3.RBAC", object{
			"stat_prefix": "cetirun_observed_sni",
			"rules": object{"action": "DENY", "policies": object{
				"blocked_observed_sni": object{
					"permissions": permissions, "principals": []object{{"any": true}},
				},
			}},
		}),
		filter("envoy.filters.network.tcp_proxy", "envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy", object{
			"stat_prefix": "cetirun_original_destination", "cluster": "cetirun_original_destination",
		}),
	}
	listeners := make([]object, 0, 2)
	for _, family := range []struct {
		name, address, loopback string
		prefixLen               int
	}{
		{"ipv4", "127.0.0.1", "127.0.0.0", 8},
		{"ipv6", "::1", "::1", 128},
	} {
		listeners = append(listeners, object{
			"name": "cetirun_egress_" + family.name, "stat_prefix": "cetirun_egress_" + family.name,
			"address":           object{"socket_address": object{"address": family.address, "port_value": CapturePort}},
			"traffic_direction": "OUTBOUND",
			"listener_filters": []object{
				filter("envoy.filters.listener.original_dst", "envoy.extensions.filters.listener.original_dst.v3.OriginalDst", object{}),
				filter("envoy.filters.listener.tls_inspector", "envoy.extensions.filters.listener.tls_inspector.v3.TlsInspector", object{}),
			},
			"listener_filters_timeout":             InspectionTimeout.String(),
			"continue_on_listener_filters_timeout": true,
			// Match after original_dst restoration, before tcp_proxy can connect.
			// A first-data RBAC guard is too late for a silent direct self socket.
			"filter_chains": []object{{
				"filter_chain_match": object{
					"destination_port": CapturePort,
					"prefix_ranges": []object{{
						"address_prefix": family.loopback, "prefix_len": family.prefixLen,
					}},
				},
				"filters": []object{
					filter("envoy.filters.network.direct_response", "envoy.extensions.filters.network.direct_response.v3.Config",
						object{"response": object{"inline_string": ""}}),
				},
			}},
			"default_filter_chain": object{"filters": filters},
		})
	}
	resources, err := json.Marshal(object{
		"listeners": listeners,
		"clusters": []object{{
			"name": "cetirun_original_destination", "type": "ORIGINAL_DST",
			"lb_policy": "CLUSTER_PROVIDED", "connect_timeout": "2s",
			"original_dst_lb_config": object{"use_http_header": false},
		}},
	})
	if err != nil {
		return nil, err
	}
	if len(resources) > MaxConfigBytes {
		return nil, fmt.Errorf("observed-SNI config exceeds %d bytes", MaxConfigBytes)
	}
	return &Config{
		StaticResources: resources,
		IPv4Commands:    commands("127.0.0.0/8"),
		IPv6Commands:    commands("::1/128"),
	}, nil
}

func filter(name, typeName string, config object) object {
	config["@type"] = "type.googleapis.com/" + typeName
	return object{"name": name, "typed_config": config}
}

func commands(loopback string) [][]string {
	return [][]string{
		{"-w", "5", "-t", "nat", "-N", captureChain},
		{"-w", "5", "-t", "nat", "-A", captureChain, "-m", "owner", "--uid-owner", strconv.Itoa(ProxyUID), "-j", "RETURN"},
		{"-w", "5", "-t", "nat", "-A", captureChain, "-d", loopback, "-j", "RETURN"},
		{"-w", "5", "-t", "nat", "-A", captureChain, "-p", "tcp", "-j", "REDIRECT", "--to-ports", strconv.Itoa(CapturePort)},
		{"-w", "5", "-t", "nat", "-A", "OUTPUT", "-p", "tcp", "-j", captureChain},
	}
}
