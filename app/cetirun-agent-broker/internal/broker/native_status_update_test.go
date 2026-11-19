// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestNativeProviderAuthorizationSurvivesAgentStatusUpdates(t *testing.T) {
	f := newAuthorizationFixture(t, "github", "github")
	policy := json.RawMessage(`{"phase":"Applied"}`)
	f.agent.Status.Policy = &policy
	native := enableNativeProofForServer(t, f.server, &f.agent)
	previous := f.server.store.client.Transport
	reads := 0
	f.server.store.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		response, err := previous.RoundTrip(request)
		if err != nil || request.URL.Path != "/apis/cetirun.microsoft.com/v1alpha1/agents/agent" {
			return response, err
		}
		var body map[string]any
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		reads++
		metadata := body["metadata"].(map[string]any)
		metadata["resourceVersion"] = fmt.Sprintf("status-update-%d", reads)
		metadata["generation"] = 1
		body["status"].(map[string]any)["agentIdentity"] = map[string]any{
			"phase": "Ready", "clientId": "synthetic-client", "objectId": "synthetic-object", "observedGeneration": 1,
			"lastVerifiedTime": fmt.Sprintf("2026-01-01T00:00:%02dZ", reads),
		}
		return securityJSONResponse(t, response.StatusCode, body), nil
	})
	for _, path := range []string{
		"/internal/v1/service-authorizations/list",
		"/internal/v1/service-authorizations/check",
		"/internal/v1/service-credentials/issue",
	} {
		response := f.request(t, path)
		if response.Code != http.StatusOK {
			t.Fatalf("status-only update rejected %s: %d %s", path, response.Code, response.Body)
		}
		if strings.Contains(response.Body.String(), "synthetic-provider-token") != strings.HasSuffix(path, "/issue") {
			t.Fatalf("unexpected credential disclosure from %s", path)
		}
	}
	if native.Calls < 4 || reads < 8 {
		t.Fatalf("expected repeated native verification, got admission=%d Agent reads=%d", native.Calls, reads)
	}
}
