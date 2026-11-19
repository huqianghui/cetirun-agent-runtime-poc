// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package egresscapture

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestEmptyDisablesCapture(t *testing.T) {
	for _, names := range [][]string{nil, {}} {
		config, err := Build(names)
		if err != nil || config != nil {
			t.Fatalf("empty list must produce no capture: config=%v err=%v", config, err)
		}
	}
}

func TestHostnameValidationAndBudgets(t *testing.T) {
	for _, name := range []string{
		"", "Example.com", "*.example.com", ".example.com", "example.com.",
		"https://example.com", "example.com:443", "192.0.2.1", "::1",
		"example..com", "-example.com", "example-.com", "exämple.com",
		strings.Repeat("a", 64) + ".com", strings.Repeat("a.", 127) + "a",
	} {
		t.Run(name, func(t *testing.T) {
			if config, err := Build([]string{name}); err == nil || config != nil {
				t.Fatalf("accepted invalid hostname: config=%v err=%v", config, err)
			}
		})
	}
	for _, name := range []string{"example.com", "localhost", "xn--bcher-kva.example", "a-b.example"} {
		if _, err := Build([]string{name}); err != nil {
			t.Fatalf("valid hostname %q: %v", name, err)
		}
	}
	duplicates := make([]string, MaxHostnames+1)
	for i := range duplicates {
		duplicates[i] = "example.com"
	}
	if config, err := Build(duplicates); err == nil || config != nil {
		t.Fatal("input budget must count duplicates before canonicalization")
	}
	if _, err := Build(duplicates[:MaxHostnames]); err != nil {
		t.Fatal(err)
	}
	large := make([]string, MaxHostnames)
	for i := range large {
		large[i] = strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." +
			strings.Repeat("c", 63) + "." + strconv.Itoa(i) + strings.Repeat("d", 49) + ".example"
	}
	if config, err := Build(large); err == nil || config != nil || !strings.Contains(err.Error(), "bytes") {
		t.Fatalf("expanded JSON byte budget was not enforced: %v", err)
	}
}

func TestCanonicalConfigDoesNotAliasInputs(t *testing.T) {
	names := []string{"z.example.com", "blocked.example.com", "blocked.example.com"}
	before := slices.Clone(names)
	first := mustBuild(t, names)
	second := mustBuild(t, []string{"blocked.example.com", "z.example.com"})
	if !slices.Equal(names, before) || !reflect.DeepEqual(first, second) {
		t.Fatal("canonicalization mutated input or produced unstable output")
	}
	first.StaticResources[0] = '!'
	first.IPv4Commands[0][0] = "mutated"
	if !reflect.DeepEqual(mustBuild(t, names), second) {
		t.Fatal("independent outputs share mutable state")
	}
}

func TestObservedSNIResourcesAndSelfDestinationGuard(t *testing.T) {
	config := mustBuild(t, []string{"blocked.example.com", "other.example.com"})
	var resources struct {
		Listeners []struct {
			Name            string          `json:"name"`
			StatPrefix      string          `json:"stat_prefix"`
			Address         json.RawMessage `json:"address"`
			Timeout         string          `json:"listener_filters_timeout"`
			Continue        bool            `json:"continue_on_listener_filters_timeout"`
			ListenerFilters []object        `json:"listener_filters"`
			FilterChains    []struct {
				Match   object   `json:"filter_chain_match"`
				Filters []object `json:"filters"`
			} `json:"filter_chains"`
			Default struct {
				Filters []object `json:"filters"`
			} `json:"default_filter_chain"`
		} `json:"listeners"`
		Clusters []object `json:"clusters"`
	}
	if err := json.Unmarshal(config.StaticResources, &resources); err != nil {
		t.Fatal(err)
	}
	if len(resources.Listeners) != 2 || len(resources.Clusters) != 1 {
		t.Fatal("unexpected listener or cluster count")
	}
	for i, listener := range resources.Listeners {
		family, address, timeoutCounter := "ipv4", "127.0.0.1", IPv4TimeoutCounter
		if i == 1 {
			family, address, timeoutCounter = "ipv6", "::1", IPv6TimeoutCounter
		}
		if listener.Name != "cetirun_egress_"+family || listener.Timeout != "1s" || !listener.Continue {
			t.Fatalf("inspection contract drift: %+v", listener)
		}
		if timeoutCounter != "listener."+listener.StatPrefix+".downstream_pre_cx_timeout" {
			t.Fatal("uninspected-timeout counter and listener prefix disagree")
		}
		assertJSON(t, listener.Address, object{"socket_address": object{"address": address, "port_value": CapturePort}})
		if len(listener.ListenerFilters) != 2 ||
			listener.ListenerFilters[0]["name"] != "envoy.filters.listener.original_dst" ||
			listener.ListenerFilters[1]["name"] != "envoy.filters.listener.tls_inspector" {
			t.Fatal("original destination must be restored before TLS inspection")
		}
		if len(listener.FilterChains) != 1 || len(listener.FilterChains[0].Filters) != 1 {
			t.Fatal("self-destination guard must have its own terminal filter chain")
		}
		guard := listener.FilterChains[0]
		prefix, bits := "127.0.0.0", 8
		if i == 1 {
			prefix, bits = "::1", 128
		}
		assertObject(t, guard.Match, object{
			"destination_port": CapturePort,
			"prefix_ranges":    []object{{"address_prefix": prefix, "prefix_len": bits}},
		})
		assertObject(t, guard.Filters[0], filter("envoy.filters.network.direct_response",
			"envoy.extensions.filters.network.direct_response.v3.Config", object{"response": object{"inline_string": ""}}))
		if len(listener.Default.Filters) != 2 || listener.Default.Filters[1]["name"] != "envoy.filters.network.tcp_proxy" {
			t.Fatal("default chain must retain the original-destination TCP proxy")
		}
		rbac := listener.Default.Filters[0]["typed_config"].(map[string]any)
		assertObject(t, rbac["rules"], object{
			"action": "DENY",
			"policies": object{"blocked_observed_sni": object{
				"permissions": []object{
					{"requested_server_name": object{"exact": "blocked.example.com"}},
					{"requested_server_name": object{"exact": "other.example.com"}},
				},
				"principals": []object{{"any": true}},
			}},
		})
	}
	assertObject(t, resources.Clusters[0], object{
		"name": "cetirun_original_destination", "type": "ORIGINAL_DST",
		"lb_policy": "CLUSTER_PROVIDED", "connect_timeout": "2s",
		"original_dst_lb_config": object{"use_http_header": false},
	})
}

func TestCaptureCommandsAreScopedAndTCPOnly(t *testing.T) {
	config := mustBuild(t, []string{"blocked.example.com"})
	for _, family := range []struct {
		commands [][]string
		loopback string
	}{
		{config.IPv4Commands, "127.0.0.0/8"},
		{config.IPv6Commands, "::1/128"},
	} {
		want := [][]string{
			{"-w", "5", "-t", "nat", "-N", "CETIRUN_FQDN_OUT"},
			{"-w", "5", "-t", "nat", "-A", "CETIRUN_FQDN_OUT", "-m", "owner", "--uid-owner", "1337", "-j", "RETURN"},
			{"-w", "5", "-t", "nat", "-A", "CETIRUN_FQDN_OUT", "-d", family.loopback, "-j", "RETURN"},
			{"-w", "5", "-t", "nat", "-A", "CETIRUN_FQDN_OUT", "-p", "tcp", "-j", "REDIRECT", "--to-ports", "15001"},
			{"-w", "5", "-t", "nat", "-A", "OUTPUT", "-p", "tcp", "-j", "CETIRUN_FQDN_OUT"},
		}
		if !reflect.DeepEqual(family.commands, want) {
			t.Fatalf("unsafe capture command drift: %v", family.commands)
		}
	}
}

func mustBuild(t *testing.T, names []string) *Config {
	t.Helper()
	config, err := Build(names)
	if err != nil || config == nil {
		t.Fatalf("build: %v", err)
	}
	return config
}

func assertJSON(t *testing.T, actual []byte, expected any) {
	t.Helper()
	var decoded any
	if err := json.Unmarshal(actual, &decoded); err != nil {
		t.Fatal(err)
	}
	assertObject(t, decoded, expected)
}

func assertObject(t *testing.T, actual, expected any) {
	t.Helper()
	a, err := json.Marshal(actual)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("got %s, want %s", a, b)
	}
}
