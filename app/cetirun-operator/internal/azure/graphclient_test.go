// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package azure

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

type staticTokenCredential struct{}

func (staticTokenCredential) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "synthetic-token"}, nil
}

type failingTokenCredential struct{}

func (failingTokenCredential) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{}, errors.New("synthetic token failure")
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestGraphErrorDoesNotExposeResponseBody(t *testing.T) {
	err := (&GraphError{StatusCode: 403, Code: safeGraphErrorCode("Authorization_RequestDenied")}).Error()
	if err != "graph API error (HTTP 403, code Authorization_RequestDenied)" {
		t.Fatalf("unexpected safe Graph error: %q", err)
	}
	if got := safeGraphErrorCode("private value: token"); got != "" {
		t.Fatalf("unsafe Graph error code was retained: %q", got)
	}
}

func TestGetAgentIdentityUsesObjectIDAndTreatsNotFoundAsAbsent(t *testing.T) {
	const objectID = "11111111-1111-1111-1111-111111111111"
	client := &GraphClient{
		cred: staticTokenCredential{},
		httpClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.Method != http.MethodGet {
				t.Fatalf("unexpected method: %s", request.Method)
			}
			if request.URL.Path != "/v1.0/servicePrincipals/"+objectID {
				t.Fatalf("unexpected path: %s", request.URL.Path)
			}
			if request.URL.Query().Get("$select") != "id,appId,displayName,agentIdentityBlueprintId" {
				t.Fatalf("unexpected select: %q", request.URL.Query().Get("$select"))
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"id":"11111111-1111-1111-1111-111111111111","appId":"22222222-2222-2222-2222-222222222222","agentIdentityBlueprintId":"33333333-3333-3333-3333-333333333333"}`)),
				Header:     make(http.Header),
			}, nil
		})},
	}

	identity, err := client.GetAgentIdentity(context.Background(), objectID)
	if err != nil {
		t.Fatal(err)
	}
	if identity == nil || identity.ID != objectID || identity.AppID == "" {
		t.Fatalf("unexpected identity: %#v", identity)
	}

	client.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"Request_ResourceNotFound"}}`)),
			Header:     make(http.Header),
		}, nil
	})
	identity, err = client.GetAgentIdentity(context.Background(), objectID)
	if err != nil {
		t.Fatal(err)
	}
	if identity != nil {
		t.Fatalf("not found identity must be nil, got %#v", identity)
	}
}

func TestCreateAgentIdentityClassifiesTokenFailureAsRequestNotSent(t *testing.T) {
	client := &GraphClient{
		cred: failingTokenCredential{},
		httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("HTTP transport must not run when token acquisition fails")
			return nil, nil
		})},
	}

	_, err := client.CreateAgentIdentity(context.Background(), "synthetic", "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222")
	if err == nil || !IsRequestNotSent(err) {
		t.Fatalf("token acquisition failure must be classified request-not-sent, got %v", err)
	}
}

func TestGetAgentIdentitiesByDisplayNameFollowsValidatedPagination(t *testing.T) {
	requests := 0
	client := &GraphClient{
		cred: staticTokenCredential{},
		httpClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			requests++
			var body string
			switch requests {
			case 1:
				if request.URL.Path != "/v1.0/servicePrincipals" || request.URL.Query().Get("$filter") == "" {
					t.Fatalf("unexpected first page request: %s", request.URL.String())
				}
				body = `{"value":[{"id":"11111111-1111-1111-1111-111111111111","appId":"22222222-2222-2222-2222-222222222222"}],"@odata.nextLink":"https://graph.microsoft.com/v1.0/servicePrincipals?$skiptoken=synthetic"}`
			case 2:
				if request.URL.Path != "/v1.0/servicePrincipals" || request.URL.Query().Get("$skiptoken") != "synthetic" {
					t.Fatalf("unexpected continuation request: %s", request.URL.String())
				}
				body = `{"value":[{"id":"33333333-3333-3333-3333-333333333333","appId":"44444444-4444-4444-4444-444444444444"}]}`
			default:
				t.Fatalf("unexpected request %d", requests)
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})},
	}

	identities, err := client.GetAgentIdentitiesByDisplayName(context.Background(), "immutable-name")
	if err != nil {
		t.Fatal(err)
	}
	if len(identities) != 2 || requests != 2 {
		t.Fatalf("pagination lost a candidate: identities=%#v requests=%d", identities, requests)
	}
}

func TestGetAgentIdentitiesByDisplayNameRejectsUnsafeContinuation(t *testing.T) {
	client := &GraphClient{
		cred: staticTokenCredential{},
		httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			body := `{"value":[],"@odata.nextLink":"https://example.invalid/v1.0/servicePrincipals?$skiptoken=synthetic"}`
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})},
	}

	if _, err := client.GetAgentIdentitiesByDisplayName(context.Background(), "immutable-name"); err == nil {
		t.Fatal("unsafe continuation must be rejected")
	}
}
