// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package config

import (
	"errors"
	"net/url"
	"os"
	"strings"
)

const DefaultAudience = "cetirun-connections-mcp"

type Config struct {
	OperatorNamespace     string
	ListenAddress         string
	TLSCertFile           string
	TLSKeyFile            string
	BrokerBaseURL         *url.URL
	WorkloadTokenAudience string
	PortalBaseURL         *url.URL
	KubernetesTokenFile   string
	KubernetesCAFile      string
}

func Load() (Config, error) {
	broker, err := httpsOrigin(os.Getenv("AGENT_BROKER_BASE_URL"))
	if err != nil {
		return Config{}, errors.New("AGENT_BROKER_BASE_URL must be an absolute HTTPS origin")
	}
	var portal *url.URL
	if raw := strings.TrimSpace(os.Getenv("PORTAL_BASE_URL")); raw != "" {
		portal, err = httpsOrigin(raw)
		if err != nil {
			return Config{}, errors.New("PORTAL_BASE_URL must be an absolute HTTPS origin when configured")
		}
	}
	audience := strings.TrimSpace(os.Getenv("WORKLOAD_TOKEN_AUDIENCE"))
	if audience == "" {
		audience = DefaultAudience
	}
	listen := strings.TrimSpace(os.Getenv("LISTEN_ADDRESS"))
	if listen == "" {
		listen = ":8443"
	}
	certFile, keyFile := strings.TrimSpace(os.Getenv("TLS_CERT_FILE")), strings.TrimSpace(os.Getenv("TLS_KEY_FILE"))
	if certFile == "" || keyFile == "" {
		return Config{}, errors.New("TLS_CERT_FILE and TLS_KEY_FILE are required")
	}
	return Config{
		OperatorNamespace:     os.Getenv("OPERATOR_NAMESPACE"),
		ListenAddress:         listen,
		TLSCertFile:           certFile,
		TLSKeyFile:            keyFile,
		BrokerBaseURL:         broker,
		WorkloadTokenAudience: audience,
		PortalBaseURL:         portal,
		KubernetesTokenFile:   valueOr("KUBERNETES_TOKEN_FILE", "/var/run/cetirun/kubernetes/token"),
		KubernetesCAFile:      valueOr("KUBERNETES_CA_FILE", "/var/run/cetirun/kubernetes/ca.crt"),
	}, nil
}

func httpsOrigin(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("not an HTTPS origin")
	}
	return u, nil
}

func valueOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
