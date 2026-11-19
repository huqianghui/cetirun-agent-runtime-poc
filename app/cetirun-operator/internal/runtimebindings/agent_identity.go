// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimebindings

import (
	"fmt"
	"net/url"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// AgentIdentity is supplied by the identity controller after the instance's
// canonical identity is ready. It contains no Azure credential or principal ID.
type AgentIdentity struct {
	BrokerURL string `json:"brokerURL"`
}

func AgentIdentityBrokerNamespace(raw string) (string, error) {
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Scheme != "http" || endpoint.User != nil || endpoint.Port() != "8080" ||
		endpoint.Path != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.ForceQuery {
		return "", fmt.Errorf("AgentIdentityBindingInvalid: a fixed in-cluster Broker URL is required")
	}
	labels := strings.Split(endpoint.Hostname(), ".")
	if len(labels) != 5 || strings.Join(labels[2:], ".") != "svc.cluster.local" ||
		len(validation.IsDNS1123Label(labels[0])) != 0 || len(validation.IsDNS1123Label(labels[1])) != 0 {
		return "", fmt.Errorf("AgentIdentityBindingInvalid: a fixed in-cluster Broker URL is required")
	}
	return labels[1], nil
}
