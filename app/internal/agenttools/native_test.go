// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package agenttools_test

import (
	"context"
	"crypto/x509"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/Azure/CetiRun/app/internal/agenttools"
	"github.com/Azure/CetiRun/app/internal/agenttools/testfixture"
)

func TestNativeVerifierAuthenticatesDocumentedServiceName(t *testing.T) {
	for _, scenario := range []string{"documented-SAN-only", "wrong-service-SAN", "untrusted-CA"} {
		t.Run(scenario, func(t *testing.T) {
			f := testfixture.NewNative(t)
			certificate := f.Server.Certificate()
			if !slices.Equal(certificate.DNSNames, []string{"cetirun-sandbox-admission.operator.svc"}) ||
				len(certificate.IPAddresses) != 0 {
				t.Fatal("fixture must present only the documented Service DNS SAN")
			}
			transport := f.Transport.(*http.Transport)
			if transport.TLSClientConfig.InsecureSkipVerify || transport.TLSClientConfig.ServerName != "" {
				t.Fatal("fixture must verify TLS against the unchanged request hostname")
			}
			namespace := "operator"
			if scenario == "wrong-service-SAN" {
				namespace = "another-operator"
				configuration := f.Objects["/apis/admissionregistration.k8s.io/v1/validatingwebhookconfigurations/"+agenttools.NativePodWebhookConfiguration].(map[string]any)
				hook := configuration["webhooks"].([]any)[0].(map[string]any)
				hook["clientConfig"].(map[string]any)["service"].(map[string]any)["namespace"] = namespace
			} else if scenario == "untrusted-CA" {
				transport.TLSClientConfig.RootCAs = x509.NewCertPool()
			}
			verifier := agenttools.NativeVerifier{Get: f.Get, OperatorNamespace: namespace,
				Audience: "cetirun-connections-mcp", Transport: transport}
			err := verifier.Verify(context.Background(), f.Identity)
			if scenario == "documented-SAN-only" {
				if err != nil || f.Calls != 1 {
					t.Fatalf("documented admission certificate was rejected: %v; calls=%d", err, f.Calls)
				}
			} else if !errors.Is(err, agenttools.ErrNativeUnavailable) || f.Calls != 0 {
				t.Fatalf("invalid TLS identity reached admission: %v; calls=%d", err, f.Calls)
			}
		})
	}
}

func TestNativeVerifierChecksCurrentPurposeAndAuthority(t *testing.T) {
	for _, scenario := range []string{"valid", "trusted-init", "denied", "missing-authority", "recreated-sa", "recreated-pod", "recreated-agent",
		"foreign-namespace-owner", "foreign-sa-namespace", "suspended", "deleting-agent", "deleting-pod",
		"held", "deleting-namespace", "wrong-audience", "helper-mount"} {
		t.Run(scenario, func(t *testing.T) {
			f := testfixture.NewNative(t)
			pod := f.Objects["/api/v1/namespaces/agents/pods/"+f.Identity.PodName].(map[string]any)
			spec := pod["spec"].(map[string]any)
			switch scenario {
			case "trusted-init":
				spec["initContainers"] = []any{
					map[string]any{"name": "managed-egress", "restartPolicy": "Always"},
					map[string]any{"name": "managed-capture"},
				}
			case "denied":
				f.Allowed = false
			case "missing-authority":
				delete(f.Objects, "/apis/admissionregistration.k8s.io/v1/validatingwebhookconfigurations/"+agenttools.NativePodWebhookConfiguration)
			case "recreated-sa":
				f.Objects["/api/v1/namespaces/agents/serviceaccounts/default"].(map[string]any)["metadata"].(map[string]any)["uid"] = "replacement-sa"
			case "recreated-pod":
				pod["metadata"].(map[string]any)["uid"] = "replacement-pod"
			case "recreated-agent":
				f.Objects["/apis/cetirun.microsoft.com/v1alpha1/agents/agent"].(map[string]any)["metadata"].(map[string]any)["uid"] = "replacement-agent"
			case "foreign-namespace-owner":
				f.Objects["/api/v1/namespaces/agents"].(map[string]any)["metadata"].(map[string]any)["ownerReferences"] = []any{}
			case "foreign-sa-namespace":
				f.Objects["/api/v1/namespaces/agents/serviceaccounts/default"].(map[string]any)["metadata"].(map[string]any)["namespace"] = "another-agent"
			case "deleting-agent":
				f.Objects["/apis/cetirun.microsoft.com/v1alpha1/agents/agent"].(map[string]any)["metadata"].(map[string]any)["deletionTimestamp"] = "2026-10-28T00:00:00Z"
			case "deleting-pod":
				pod["metadata"].(map[string]any)["deletionTimestamp"] = "2026-10-28T00:00:00Z"
			case "held":
				f.Objects["/apis/cetirun.microsoft.com/v1alpha1/agents/agent"].(map[string]any)["metadata"].(map[string]any)["finalizers"] = []string{"cetirun.microsoft.com/sandbox-hold"}
			case "suspended":
				f.Objects["/apis/cetirun.microsoft.com/v1alpha1/agents/agent"].(map[string]any)["spec"].(map[string]any)["suspended"] = true
			case "deleting-namespace":
				f.Objects["/api/v1/namespaces/agents"].(map[string]any)["metadata"].(map[string]any)["deletionTimestamp"] = "2026-10-28T00:00:00Z"
			case "wrong-audience":
				spec["volumes"].([]any)[0].(map[string]any)["projected"].(map[string]any)["sources"].([]any)[0].(map[string]any)["serviceAccountToken"].(map[string]any)["audience"] = "api://AzureADTokenExchange"
			case "helper-mount":
				spec["initContainers"] = spec["containers"]
			}
			verifier := agenttools.NativeVerifier{Get: f.Get, OperatorNamespace: "operator",
				Audience: "cetirun-connections-mcp", Transport: f.Transport}
			err := verifier.Verify(context.Background(), f.Identity)
			if scenario == "valid" || scenario == "trusted-init" {
				if err != nil || f.Calls != 1 {
					t.Fatalf("current native proof rejected: %v, calls=%d", err, f.Calls)
				}
			} else if err == nil {
				t.Fatal("invalid native proof was accepted")
			}
			if scenario == "missing-authority" && !errors.Is(err, agenttools.ErrNativeUnavailable) {
				t.Fatal("authority failure was not classified unavailable")
			}
		})
	}
}

type nativeRoundTrip func(*http.Request) (*http.Response, error)

func (f nativeRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNativeVerifierRejectsChangesDuringAuthorityReadback(t *testing.T) {
	f := testfixture.NewNative(t)
	verifier := agenttools.NativeVerifier{Get: f.Get, OperatorNamespace: "operator", Audience: "cetirun-connections-mcp"}
	verifier.Transport = nativeRoundTrip(func(r *http.Request) (*http.Response, error) {
		response, err := f.Transport.RoundTrip(r)
		f.Objects["/api/v1/namespaces/agents/serviceaccounts/default"].(map[string]any)["metadata"].(map[string]any)["uid"] = "replacement-sa"
		return response, err
	})
	if err := verifier.Verify(context.Background(), f.Identity); !errors.Is(err, agenttools.ErrNativeProofInvalid) {
		t.Fatalf("concurrent SA replacement = %v", err)
	}
}

func TestNativeVerifierFencesAgentAuthorizationChangesNotStatusResourceVersions(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		change func(map[string]any)
		valid  bool
	}{
		{
			name: "status-resource-version",
			change: func(agent map[string]any) {
				agent["metadata"].(map[string]any)["resourceVersion"] = "2"
			},
			valid: true,
		},
		{
			name: "generation",
			change: func(agent map[string]any) {
				agent["metadata"].(map[string]any)["generation"] = int64(2)
			},
		},
		{
			name: "missing-resource-version",
			change: func(agent map[string]any) {
				agent["metadata"].(map[string]any)["resourceVersion"] = ""
			},
		},
		{
			name: "missing-generation",
			change: func(agent map[string]any) {
				agent["metadata"].(map[string]any)["generation"] = int64(0)
			},
		},
		{
			name: "agent-identity",
			change: func(agent map[string]any) {
				agent["metadata"].(map[string]any)["uid"] = "replacement-agent"
			},
		},
		{
			name: "deletion",
			change: func(agent map[string]any) {
				agent["metadata"].(map[string]any)["deletionTimestamp"] = "2026-11-06T00:00:00Z"
			},
		},
		{
			name: "hold",
			change: func(agent map[string]any) {
				agent["metadata"].(map[string]any)["finalizers"] = []string{"cetirun.microsoft.com/sandbox-hold"}
			},
		},
		{
			name: "metadata-owner",
			change: func(agent map[string]any) {
				agent["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{"uid": "replacement-owner"}}
			},
		},
		{
			name: "block-owner-deletion",
			change: func(agent map[string]any) {
				agent["metadata"].(map[string]any)["ownerReferences"].([]any)[0].(map[string]any)["blockOwnerDeletion"] = false
			},
		},
		{
			name: "suspended",
			change: func(agent map[string]any) {
				agent["spec"].(map[string]any)["suspended"] = true
			},
		},
		{
			name: "namespace",
			change: func(agent map[string]any) {
				agent["status"].(map[string]any)["namespace"] = "another-agent"
			},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := testfixture.NewNative(t)
			verifier := agenttools.NativeVerifier{Get: f.Get, OperatorNamespace: "operator", Audience: "cetirun-connections-mcp"}
			verifier.Transport = nativeRoundTrip(func(request *http.Request) (*http.Response, error) {
				response, err := f.Transport.RoundTrip(request)
				scenario.change(f.Objects["/apis/cetirun.microsoft.com/v1alpha1/agents/agent"].(map[string]any))
				return response, err
			})
			err := verifier.Verify(context.Background(), f.Identity)
			if scenario.valid {
				if err != nil {
					t.Fatalf("status-only resourceVersion change rejected native proof: %v", err)
				}
				return
			}
			if !errors.Is(err, agenttools.ErrNativeProofInvalid) {
				t.Fatalf("Agent authorization change = %v", err)
			}
		})
	}
}

func TestNativeVerifierRequiresInitialAgentVersionedObservation(t *testing.T) {
	for _, field := range []string{"resourceVersion", "generation"} {
		t.Run(field, func(t *testing.T) {
			f := testfixture.NewNative(t)
			metadata := f.Objects["/apis/cetirun.microsoft.com/v1alpha1/agents/agent"].(map[string]any)["metadata"].(map[string]any)
			if field == "generation" {
				metadata[field] = int64(0)
			} else {
				metadata[field] = ""
			}
			verifier := agenttools.NativeVerifier{Get: f.Get, OperatorNamespace: "operator", Audience: "cetirun-connections-mcp", Transport: f.Transport}
			if err := verifier.Verify(context.Background(), f.Identity); !errors.Is(err, agenttools.ErrNativeProofInvalid) {
				t.Fatalf("initial Agent without %s = %v", field, err)
			}
		})
	}
}
