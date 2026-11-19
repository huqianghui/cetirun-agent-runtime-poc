// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/auth"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/kube"
)

const (
	internalOwnerTenant = "11111111-1111-1111-1111-111111111111"
	internalOwnerObject = "22222222-2222-2222-2222-222222222222"
)

func TestInternalAuthBoundOwnerIdentity(t *testing.T) {
	ownerIdentity := auth.VerifiedIdentity{TenantID: internalOwnerTenant, ObjectID: internalOwnerObject}
	wrongObject := auth.VerifiedIdentity{TenantID: internalOwnerTenant, ObjectID: "33333333-3333-3333-3333-333333333333"}
	wrongTenant := auth.VerifiedIdentity{TenantID: "44444444-4444-4444-4444-444444444444", ObjectID: internalOwnerObject}
	boundOwner := map[string]string{
		cetirunv1alpha1.AnnotationOwnerTenantID: internalOwnerTenant,
		cetirunv1alpha1.AnnotationOwnerObjectID: internalOwnerObject,
	}

	tests := []struct {
		name                  string
		annotations           map[string]string
		alias                 string
		email                 string
		portalToken           string
		malformedPortalHeader bool
		authorization         string
		verifier              func(context.Context, string) (auth.VerifiedIdentity, error)
		wantStatus            int
		wantCall              bool
		wantToken             string
		wantBody              string
	}{
		{
			name:        "same owner object allows different display identity",
			annotations: boundOwner,
			alias:       "display-owner",
			email:       "renamed-owner@example.com",
			portalToken: "owner-token",
			verifier:    func(context.Context, string) (auth.VerifiedIdentity, error) { return ownerIdentity, nil },
			wantStatus:  http.StatusOK,
			wantCall:    true,
			wantToken:   "owner-token",
		},
		{
			name:        "bound owner allows identity token without display headers",
			annotations: boundOwner,
			portalToken: "owner-token",
			verifier:    func(context.Context, string) (auth.VerifiedIdentity, error) { return ownerIdentity, nil },
			wantStatus:  http.StatusOK,
			wantCall:    true,
			wantToken:   "owner-token",
		},
		{
			name:        "same alias wrong object is denied",
			annotations: boundOwner,
			alias:       "renamed-owner",
			email:       "owner@example.com",
			portalToken: "wrong-object",
			verifier:    func(context.Context, string) (auth.VerifiedIdentity, error) { return wrongObject, nil },
			wantStatus:  http.StatusForbidden,
			wantCall:    true,
			wantToken:   "wrong-object",
		},
		{
			name:        "wrong tenant is denied",
			annotations: boundOwner,
			alias:       "renamed-owner",
			email:       "owner@example.com",
			portalToken: "wrong-tenant",
			verifier:    func(context.Context, string) (auth.VerifiedIdentity, error) { return wrongTenant, nil },
			wantStatus:  http.StatusForbidden,
			wantCall:    true,
			wantToken:   "wrong-tenant",
		},
		{
			name:        "missing token is denied",
			annotations: boundOwner,
			verifier:    func(context.Context, string) (auth.VerifiedIdentity, error) { return ownerIdentity, nil },
			wantStatus:  http.StatusForbidden,
		},
		{
			name:          "authorization header alone is denied",
			annotations:   boundOwner,
			authorization: "Bearer owner-token",
			verifier:      func(context.Context, string) (auth.VerifiedIdentity, error) { return ownerIdentity, nil },
			wantStatus:    http.StatusForbidden,
		},
		{
			name:                  "malformed portal identity header is denied",
			annotations:           boundOwner,
			portalToken:           "not-a-bearer-token",
			malformedPortalHeader: true,
			verifier:              func(context.Context, string) (auth.VerifiedIdentity, error) { return ownerIdentity, nil },
			wantStatus:            http.StatusForbidden,
		},
		{
			name:        "invalid token is denied",
			annotations: boundOwner,
			portalToken: "expired-synthetic-token",
			verifier: func(context.Context, string) (auth.VerifiedIdentity, error) {
				return auth.VerifiedIdentity{}, errors.New("expired-synthetic-token")
			},
			wantStatus: http.StatusForbidden,
			wantCall:   true,
			wantToken:  "expired-synthetic-token",
			wantBody:   "expired-synthetic-token",
		},
		{
			name:        "unconfigured verifier is denied",
			annotations: boundOwner,
			portalToken: "owner-token",
			wantStatus:  http.StatusForbidden,
		},
		{
			name: "tenant-only owner annotation is denied",
			annotations: map[string]string{
				cetirunv1alpha1.AnnotationOwnerTenantID: internalOwnerTenant,
			},
			portalToken: "owner-token",
			verifier:    func(context.Context, string) (auth.VerifiedIdentity, error) { return ownerIdentity, nil },
			wantStatus:  http.StatusForbidden,
			wantCall:    true,
			wantToken:   "owner-token",
		},
		{
			name: "object-only owner annotation is denied",
			annotations: map[string]string{
				cetirunv1alpha1.AnnotationOwnerObjectID: internalOwnerObject,
			},
			portalToken: "owner-token",
			verifier:    func(context.Context, string) (auth.VerifiedIdentity, error) { return ownerIdentity, nil },
			wantStatus:  http.StatusForbidden,
			wantCall:    true,
			wantToken:   "owner-token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := sampleInstance()
			instance.Spec.User.Alias = "renamed-owner"
			instance.Spec.User.Email = "owner@example.com"
			instance.Annotations = tt.annotations
			called := false
			actualToken := ""
			h := &InstanceHandler{Kube: kube.NewClient(newFakeClientWith(instance))}
			if tt.verifier != nil {
				h.VerifyInternalIdentity = func(ctx context.Context, token string) (auth.VerifiedIdentity, error) {
					called = true
					actualToken = token
					return tt.verifier(ctx, token)
				}
			}

			req := httptest.NewRequest(http.MethodGet, "/internal/auth", nil)
			req.Header.Set("X-Internal-Instance", instance.Name)
			if tt.alias != "" {
				req.Header.Set("X-Internal-Alias", tt.alias)
			}
			if tt.email != "" {
				req.Header.Set("X-Internal-Email", tt.email)
			}
			if tt.portalToken != "" {
				portalHeader := "Bearer " + tt.portalToken
				if tt.malformedPortalHeader {
					portalHeader = tt.portalToken
				}
				req.Header.Set("X-CetiRun-Portal-ID-Token", portalHeader)
			}
			if tt.authorization != "" {
				req.Header.Set("Authorization", tt.authorization)
			}
			w := httptest.NewRecorder()
			h.InternalAuth(w, req)
			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", w.Code, tt.wantStatus)
			}
			if called != tt.wantCall {
				t.Fatalf("verifier called = %v, want %v", called, tt.wantCall)
			}
			if actualToken != tt.wantToken {
				t.Fatalf("verifier token = %q, want %q", actualToken, tt.wantToken)
			}
			if tt.wantBody != "" && strings.Contains(w.Body.String(), tt.wantBody) {
				t.Fatalf("response body leaked verifier detail: %q", w.Body.String())
			}
		})
	}
}

func TestInternalAuthLegacyAliasAndEmail(t *testing.T) {
	tests := []struct {
		name       string
		alias      string
		email      string
		wantStatus int
	}{
		{name: "matching alias", alias: "legacy-owner", wantStatus: http.StatusOK},
		{name: "matching email", alias: "other", email: "legacy-owner@example.com", wantStatus: http.StatusOK},
		{name: "different identity", alias: "other", email: "other@example.com", wantStatus: http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := sampleInstance()
			instance.Spec.User.Alias = "legacy-owner"
			instance.Spec.User.Email = "legacy-owner@example.com"
			h := &InstanceHandler{Kube: kube.NewClient(newFakeClientWith(instance))}
			req := httptest.NewRequest(http.MethodGet, "/internal/auth", nil)
			req.Header.Set("X-Internal-Instance", instance.Name)
			req.Header.Set("X-Internal-Alias", tt.alias)
			req.Header.Set("X-Internal-Email", tt.email)
			w := httptest.NewRecorder()
			h.InternalAuth(w, req)
			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", w.Code, tt.wantStatus)
			}
		})
	}
}

func TestInternalAuthVerifierContextDeadlineAndCancellation(t *testing.T) {
	instance := sampleInstance()
	instance.Annotations = map[string]string{
		cetirunv1alpha1.AnnotationOwnerTenantID: internalOwnerTenant,
		cetirunv1alpha1.AnnotationOwnerObjectID: internalOwnerObject,
	}
	deadlineObserved := make(chan bool, 1)
	verifierStarted := make(chan struct{})
	h := &InstanceHandler{
		Kube: kube.NewClient(newFakeClientWith(instance)),
		VerifyInternalIdentity: func(ctx context.Context, token string) (auth.VerifiedIdentity, error) {
			deadline, ok := ctx.Deadline()
			deadlineObserved <- ok && time.Until(deadline) > 3*time.Second && time.Until(deadline) <= 4*time.Second
			close(verifierStarted)
			<-ctx.Done()
			return auth.VerifiedIdentity{}, ctx.Err()
		},
	}
	requestContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/internal/auth", nil).WithContext(requestContext)
	req.Header.Set("X-Internal-Instance", instance.Name)
	req.Header.Set("X-CetiRun-Portal-ID-Token", "Bearer cancellation-test-token")
	w := httptest.NewRecorder()
	completed := make(chan struct{})
	go func() {
		h.InternalAuth(w, req)
		close(completed)
	}()

	select {
	case <-verifierStarted:
	case <-time.After(time.Second):
		t.Fatal("verifier did not start")
	}
	if !<-deadlineObserved {
		t.Fatal("verifier did not receive the internal authentication deadline")
	}
	cancel()
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("request cancellation did not reach verifier")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}
