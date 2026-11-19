// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package identity

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

type authenticationTransport func(*http.Request) (*http.Response, error)

func (f authenticationTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestAuthenticationDependencyFailures(t *testing.T) {
	steps := []struct {
		name, method, path, response string
	}{
		{"TokenReview", http.MethodPost, "/apis/authentication.k8s.io/v1/tokenreviews", `{"status":{"authenticated":true,"audiences":["cetirun-connections-mcp"],"user":{"username":"system:serviceaccount:cetirun-agent-a:openclaw-agent-a-cetirun","uid":"account-uid","extra":{"authentication.kubernetes.io/pod-name":["agent-pod"],"authentication.kubernetes.io/pod-uid":["pod-uid"]}}}}`},
		{"Pod", http.MethodGet, "/api/v1/namespaces/cetirun-agent-a/pods/agent-pod", `{"metadata":{"uid":"pod-uid","namespace":"cetirun-agent-a"},"spec":{"serviceAccountName":"openclaw-agent-a-cetirun"}}`},
		{"ServiceAccount", http.MethodGet, "/api/v1/namespaces/cetirun-agent-a/serviceaccounts/openclaw-agent-a-cetirun", `{"metadata":{"uid":"account-uid"}}`},
		{"Namespace", http.MethodGet, "/api/v1/namespaces/cetirun-agent-a", `{"metadata":{"name":"cetirun-agent-a","ownerReferences":[{"apiVersion":"cetirun.microsoft.com/v1alpha1","kind":"Agent","name":"agent-a","uid":"instance-uid"}]}}`},
		{"Agent", http.MethodGet, "/apis/cetirun.microsoft.com/v1alpha1/agents/agent-a", `{"metadata":{"uid":"instance-uid","name":"agent-a"},"status":{"phase":"Ready","namespace":"cetirun-agent-a"}}`},
	}
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("synthetic-server-proof"), 0600); err != nil {
		t.Fatal(err)
	}
	for failedIndex, step := range steps {
		for _, failure := range []struct {
			name      string
			status    int
			body      string
			err       error
			readError bool
		}{
			{name: "transport", err: errors.New("synthetic private transport detail")},
			{name: "timeout", err: context.DeadlineExceeded},
			{name: "canceled", err: context.Canceled},
			{name: "server credential rejected", status: http.StatusUnauthorized},
			{name: "server RBAC denied", status: http.StatusForbidden},
			{name: "not found", status: http.StatusNotFound},
			{name: "throttled", status: http.StatusTooManyRequests},
			{name: "server error", status: http.StatusInternalServerError},
			{name: "unavailable", status: http.StatusServiceUnavailable},
			{name: "malformed JSON", body: `{"invalid":`},
			{name: "empty response"},
			{name: "null response", body: "null"},
			{name: "oversized response", body: strings.Repeat("x", maxKubernetesResponse+1)},
			{name: "interrupted response", readError: true},
		} {
			t.Run(step.name+"/"+failure.name, func(t *testing.T) {
				calls := 0
				client := &Kubernetes{baseURL: "https://kubernetes.example.com", tokenFile: tokenPath, audience: "cetirun-connections-mcp", client: &http.Client{
					Transport: authenticationTransport(func(request *http.Request) (*http.Response, error) {
						index := calls
						calls++
						if index >= len(steps) {
							t.Fatal("unexpected extra Kubernetes request")
						}
						expected := steps[index]
						if request.Method != expected.method || request.URL.Path != expected.path {
							t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
						}
						status, body := http.StatusOK, expected.response
						if index == failedIndex {
							if failure.err != nil {
								return nil, failure.err
							}
							body = failure.body
							if failure.status != 0 {
								status, body = failure.status, "synthetic private Kubernetes diagnostic"
							}
						}
						var reader io.Reader = strings.NewReader(body)
						if index == failedIndex && failure.readError {
							reader = iotest.ErrReader(io.ErrUnexpectedEOF)
						}
						return &http.Response{StatusCode: status, Body: io.NopCloser(reader), Header: make(http.Header)}, nil
					}),
				}}
				caller, err := client.Authenticate(context.Background(), "synthetic-workload-proof")
				wantErr := ErrDependencyUnavailable
				if failure.status == http.StatusNotFound && step.method == http.MethodGet {
					wantErr = ErrUnauthenticated
				}
				if !errors.Is(err, wantErr) || err.Error() != wantErr.Error() || caller != (Caller{}) || calls != failedIndex+1 {
					t.Fatalf("authentication = %#v, %v, %d calls; want empty caller, %v, %d calls", caller, err, calls, wantErr, failedIndex+1)
				}
			})
		}
	}
}

func TestAuthenticationRequiresServerCredential(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "missing"}[missing], func(t *testing.T) {
			tokenPath := filepath.Join(t.TempDir(), "token")
			if !missing {
				if err := os.WriteFile(tokenPath, []byte(" \n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			client := &Kubernetes{baseURL: "https://kubernetes.example.com", tokenFile: tokenPath, client: &http.Client{
				Transport: authenticationTransport(func(*http.Request) (*http.Response, error) {
					t.Fatal("Kubernetes request sent without a server credential")
					return nil, nil
				}),
			}}
			for _, proof := range []string{"", "synthetic-workload-proof"} {
				wantErr := ErrDependencyUnavailable
				if proof == "" {
					wantErr = ErrUnauthenticated
				}
				caller, err := client.Authenticate(context.Background(), proof)
				if !errors.Is(err, wantErr) || caller != (Caller{}) {
					t.Fatalf("authentication = %#v, %v; want empty caller, %v", caller, err, wantErr)
				}
			}
		})
	}
}
