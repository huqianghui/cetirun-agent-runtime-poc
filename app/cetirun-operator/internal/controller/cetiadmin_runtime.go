// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strconv"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
)

func applyCetiAdminNetwork(values map[string]any, access cetiadmin.KubernetesAccess) error {
	server, err := url.Parse(access.APIServer)
	if err != nil {
		return fmt.Errorf("CetiAdmin API origin is invalid")
	}
	port := 443
	if server.Port() != "" {
		port, err = strconv.Atoi(server.Port())
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("CetiAdmin API port is invalid")
		}
	}
	object := func(key string) (map[string]any, error) {
		if values[key] == nil {
			values[key] = map[string]any{}
		}
		result, ok := values[key].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("CetiAdmin %s must be an object", key)
		}
		return result, nil
	}
	network, err := object("networkPolicy")
	if err != nil {
		return err
	}
	network["enabled"] = true
	var rules []any
	if existing, exists := network["allowedInternalCIDRs"]; exists {
		list, ok := existing.([]any)
		if !ok {
			return fmt.Errorf("CetiAdmin internal network rules must be a list")
		}
		for _, item := range list {
			rule, ok := item.(map[string]any)
			if !ok {
				return fmt.Errorf("CetiAdmin internal network rule must be an object")
			}
			cidr, _ := rule["cidr"].(string)
			if !slices.Contains(access.APIEgressCIDRs, cidr) {
				rules = append(rules, rule)
			}
		}
	}
	blocked, err := cetiAdminStringList(network["blockedCIDRs"])
	if err != nil {
		return err
	}
	for _, cidr := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.169.254/32"} {
		if !slices.Contains(blocked, cidr) {
			blocked = append(blocked, cidr)
		}
	}
	for _, cidr := range access.APIEgressCIDRs {
		rules = append(rules, map[string]any{"cidr": cidr, "ports": []any{map[string]any{"protocol": "TCP", "port": port}}})
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			return fmt.Errorf("CetiAdmin API CIDR is invalid")
		}
		if prefix.Addr().Is4() && !slices.Contains(blocked, cidr) {
			blocked = append(blocked, cidr)
		}
	}
	network["allowedInternalCIDRs"], network["blockedCIDRs"] = rules, cetiAdminValuesList(blocked)
	scope, err := object("sidecarScope")
	if err != nil {
		return err
	}
	scope["enabled"] = true
	hosts, err := cetiAdminStringList(scope["additionalEgressHosts"])
	if err != nil {
		return err
	}
	host := "*/" + server.Hostname()
	if server.Hostname() == "kubernetes.default.svc" || server.Hostname() == "kubernetes.default" || server.Hostname() == "kubernetes.default.svc.cluster.local" {
		host = "default/kubernetes.default.svc.cluster.local"
	}
	if !slices.Contains(hosts, host) {
		hosts = append(hosts, host)
	}
	scope["additionalEgressHosts"] = cetiAdminValuesList(hosts)
	return nil
}

func cetiAdminValuesList(input []string) []any {
	result := make([]any, len(input))
	for i, value := range input {
		result[i] = value
	}
	return result
}

func cetiAdminStringList(value any) ([]string, error) {
	switch input := value.(type) {
	case nil:
		return nil, nil
	case []string:
		return slices.Clone(input), nil
	case []any:
		result := make([]string, 0, len(input))
		for _, entry := range input {
			text, ok := entry.(string)
			if !ok {
				return nil, fmt.Errorf("CetiAdmin network list must contain strings")
			}
			result = append(result, text)
		}
		return result, nil
	default:
		return nil, fmt.Errorf("CetiAdmin network configuration must be a list")
	}
}
