// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"helm.sh/helm/v3/pkg/action"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func reconciledGrantSDKPlan(t *testing.T, source *PreparedRelease, beforeCreate func(context.Context, unstructured.Unstructured) (func(context.Context) error, error)) *PreparedRelease {
	t.Helper()
	input := source.sandboxBundle.Identity()
	input.Reconciled = true
	bundle, err := w.Build(input)
	if err != nil {
		t.Fatal(err)
	}
	guard := func(context.Context) error { return nil }
	values := sampleInput(input.AgentType, "openai", false)
	object(values["secrets"])["existingSecret"] = "runtime-secret"
	prepared, err := source.ReconcileSandboxWithPreflight(values, bundle, guard, guard, guard, beforeCreate, nil)
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func TestReconciledSandboxSDKGrantSubmissionPrecedesOnlyAttemptedCreates(t *testing.T) {
	for _, scenario := range []string{"success", "transport-failure", "guard-failure", "already-exists"} {
		for _, upgrade := range []bool{false, true} {
			t.Run(scenario+map[bool]string{false: "/install", true: "/upgrade"}[upgrade], func(t *testing.T) {
				source, _, _ := sandboxSDKKubernetesAccessPlans(t)
				injected := errors.New("synthetic grant request failure")
				var authorized []string
				prepared := reconciledGrantSDKPlan(t, source, func(_ context.Context, object unstructured.Unstructured) (func(context.Context) error, error) {
					if scenario == "guard-failure" {
						return nil, injected
					}
					authorized = append(authorized, object.GetName())
					return func(context.Context) error { return nil }, nil
				})
				cfg, api, _ := sandboxSDKConfig()
				if upgrade {
					initial := reconciledGrantSDKPlan(t, source, func(context.Context, unstructured.Unstructured) (func(context.Context) error, error) {
						return func(context.Context) error { return nil }, nil
					})
					if err := (&Client{}).installOrUpgrade(initial.sandboxReconcile.Namespace.Name, initial.releaseName, initial,
						func(string) (*action.Configuration, error) { return cfg, nil }); err != nil {
						t.Fatal(err)
					}
					for path, object := range api.objects {
						if w.KubernetesBindingResource(*object) {
							delete(api.objects, path)
						}
					}
				}
				posts := 0
				api.before = func(call sandboxSDKRequest) {
					if call.method != http.MethodPost || !strings.HasSuffix(call.path, "/clusterrolebindings") {
						return
					}
					var object unstructured.Unstructured
					if err := object.UnmarshalJSON(call.body); err != nil {
						t.Fatal(err)
					}
					posts++
					if !slices.Contains(authorized, object.GetName()) {
						t.Fatal("the real Helm client posted a binding before its submission checkpoint")
					}
					if scenario == "already-exists" {
						object.SetUID("existing-binding")
						object.SetResourceVersion("existing-version")
						api.objects[sandboxTestPath(&object)] = object.DeepCopy()
					}
				}
				api.fail = func(call sandboxSDKRequest) error {
					if scenario == "transport-failure" && call.method == http.MethodPost &&
						strings.HasSuffix(call.path, "/clusterrolebindings") {
						return injected
					}
					return nil
				}
				err := (&Client{}).installOrUpgrade(prepared.sandboxReconcile.Namespace.Name, prepared.releaseName, prepared,
					func(string) (*action.Configuration, error) { return cfg, nil })
				if scenario == "success" {
					if err != nil || posts != 2 || len(authorized) != 2 {
						t.Fatalf("complete grant install: posts=%d authorized=%d error=%v", posts, len(authorized), err)
					}
				} else {
					want := 1
					if scenario == "guard-failure" {
						want = 0
					}
					if err == nil || posts != want || len(authorized) != want {
						t.Fatalf("partial grant install: posts=%d authorized=%d error=%v", posts, len(authorized), err)
					}
				}
			})
		}
	}
}

func TestReconciledSandboxSDKGrantUpdatesPreserveConditionalOwnership(t *testing.T) {
	for _, race := range []bool{false, true} {
		t.Run(map[bool]string{false: "metadata-update", true: "replacement-race"}[race], func(t *testing.T) {
			old, next, retired := sandboxSDKKubernetesAccessPlans(t)
			authorize := func(context.Context, unstructured.Unstructured) (func(context.Context) error, error) {
				return func(context.Context) error { return nil }, nil
			}
			previous := reconciledGrantSDKPlan(t, old, authorize)
			desired := reconciledGrantSDKPlan(t, next, authorize)
			cfg, api, _ := sandboxSDKConfig()
			configure := func(string) (*action.Configuration, error) { return cfg, nil }
			if err := (&Client{}).installOrUpgrade(previous.sandboxReconcile.Namespace.Name, previous.releaseName, previous, configure); err != nil {
				t.Fatal(err)
			}
			retiredPath := sandboxTestPath(retired)
			retained := api.objects[retiredPath].DeepCopy()
			retained.SetUID("retired-name-replacement")
			retained.SetResourceVersion("retired-name-version")
			retained.SetOwnerReferences(nil)
			api.objects[retiredPath] = retained
			requests, patched := len(api.requests), false
			api.before = func(call sandboxSDKRequest) {
				if call.method != http.MethodPatch || !strings.Contains(call.path, "/clusterrolebindings/") {
					return
				}
				patched = true
				var patch struct {
					Metadata struct{ UID, ResourceVersion string }
				}
				if err := json.Unmarshal(call.body, &patch); err != nil || patch.Metadata.UID == "" || patch.Metadata.ResourceVersion == "" {
					t.Fatal("binding update omitted atomic identity/version preconditions:", err)
				}
				if race {
					replacement := api.objects[call.path].DeepCopy()
					replacement.SetUID("target-name-replacement")
					replacement.SetResourceVersion("target-name-version")
					api.objects[call.path] = replacement
				}
			}

			err := (&Client{}).installOrUpgrade(desired.sandboxReconcile.Namespace.Name, desired.releaseName, desired, configure)
			if !patched || (err != nil) != race {
				t.Fatalf("conditional grant update: patched=%t error=%v", patched, err)
			}
			if sandboxJSONHash(api.objects[retiredPath]) != sandboxJSONHash(retained) {
				t.Fatal("Helm changed a retired binding outside journal authority")
			}
			for _, call := range api.requests[requests:] {
				if call.method == http.MethodDelete && strings.Contains(call.path, "/clusterrolebindings/") {
					t.Fatal("Helm performed name-only pruning of a grant")
				}
			}
		})
	}
}

func TestSandboxBindingCreateRejectionClassification(t *testing.T) {
	for _, tc := range []struct {
		code     int32
		reason   metav1.StatusReason
		rejected bool
	}{
		{400, metav1.StatusReasonBadRequest, true},
		{401, metav1.StatusReasonUnauthorized, true},
		{403, metav1.StatusReasonForbidden, true},
		{404, metav1.StatusReasonNotFound, true},
		{409, metav1.StatusReasonAlreadyExists, true},
		{409, metav1.StatusReasonConflict, true},
		{422, metav1.StatusReasonInvalid, true},
		{405, metav1.StatusReasonMethodNotAllowed, true},
		{413, metav1.StatusReasonRequestEntityTooLarge, true},
		{415, metav1.StatusReasonUnsupportedMediaType, true},
		{429, metav1.StatusReasonTooManyRequests, true},
		{408, metav1.StatusReasonTimeout, false},
		{504, metav1.StatusReasonTimeout, false},
		{500, metav1.StatusReasonServerTimeout, false},
		{503, metav1.StatusReasonServiceUnavailable, false},
		{500, metav1.StatusReasonInternalError, false},
		{400, metav1.StatusReasonUnknown, false},
		{200, metav1.StatusReasonForbidden, false},
		{503, metav1.StatusReasonForbidden, false},
	} {
		t.Run(fmt.Sprintf("%s/%d", tc.reason, tc.code), func(t *testing.T) {
			err := fmt.Errorf("wrapped CREATE: %w", &apierrors.StatusError{
				ErrStatus: metav1.Status{Code: tc.code, Reason: tc.reason},
			})
			if sandboxBindingCreateRejected(err) != tc.rejected {
				t.Fatal("incorrect terminal CREATE classification:", err)
			}
		})
	}
	for _, err := range []error{nil, errors.New("unknown"), context.Canceled, context.DeadlineExceeded, io.ErrUnexpectedEOF} {
		if sandboxBindingCreateRejected(err) {
			t.Fatal("missing or uncertain result was classified as a rejection:", err)
		}
	}
}

func TestReconciledSandboxSDKGrantRejectionOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		code     int
		reason   metav1.StatusReason
		rejected bool
	}{
		{"forbidden", http.StatusForbidden, metav1.StatusReasonForbidden, true},
		{"invalid", http.StatusUnprocessableEntity, metav1.StatusReasonInvalid, true},
		{"throttled", http.StatusTooManyRequests, metav1.StatusReasonTooManyRequests, true},
		{"timeout", http.StatusGatewayTimeout, metav1.StatusReasonTimeout, false},
		{"server-timeout", http.StatusInternalServerError, metav1.StatusReasonServerTimeout, false},
		{"unavailable", http.StatusServiceUnavailable, metav1.StatusReasonServiceUnavailable, false},
		{"transport-failure", 0, "", false},
		{"lost-response", 0, "", false},
		{"receipt-failure", http.StatusForbidden, metav1.StatusReasonForbidden, true},
	} {
		for _, upgrade := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/install", true: "/upgrade"}[upgrade], func(t *testing.T) {
				source, _, _ := sandboxSDKKubernetesAccessPlans(t)
				var authorized, rejected []string
				receiptFailure := errors.New("synthetic rejection checkpoint failure")
				prepared := reconciledGrantSDKPlan(t, source,
					func(_ context.Context, object unstructured.Unstructured) (func(context.Context) error, error) {
						name := object.GetName()
						authorized = append(authorized, name)
						return func(context.Context) error {
							rejected = append(rejected, name)
							if tc.name == "receipt-failure" {
								return receiptFailure
							}
							return nil
						}, nil
					})
				cfg, api, _ := sandboxSDKConfig()
				configure := func(string) (*action.Configuration, error) { return cfg, nil }
				if upgrade {
					initial := reconciledGrantSDKPlan(t, source,
						func(context.Context, unstructured.Unstructured) (func(context.Context) error, error) {
							return func(context.Context) error { return nil }, nil
						})
					if err := (&Client{}).installOrUpgrade(initial.sandboxReconcile.Namespace.Name, initial.releaseName, initial, configure); err != nil {
						t.Fatal(err)
					}
					for path, object := range api.objects {
						if w.KubernetesBindingResource(*object) {
							delete(api.objects, path)
						}
					}
				}
				posts := 0
				api.respond = func(call sandboxSDKRequest) (*http.Response, error) {
					if call.method != http.MethodPost || !strings.HasSuffix(call.path, "/clusterrolebindings") {
						return nil, nil
					}
					posts++
					if posts > 1 {
						return sandboxSDKStatus(http.StatusForbidden, metav1.StatusReasonForbidden)
					}
					if tc.name == "transport-failure" {
						return nil, errors.New("synthetic transport failure")
					}
					if tc.name == "lost-response" {
						var object unstructured.Unstructured
						if err := object.UnmarshalJSON(call.body); err != nil {
							return nil, err
						}
						object.SetUID("completed-without-response")
						object.SetResourceVersion("lost-response-version")
						api.objects[sandboxTestPath(&object)] = object.DeepCopy()
						return nil, io.ErrUnexpectedEOF
					}
					response, err := sandboxSDKStatus(tc.code, tc.reason)
					if err == nil {
						response.Header.Set("Retry-After", "0")
					}
					return response, err
				}
				err := (&Client{}).installOrUpgrade(prepared.sandboxReconcile.Namespace.Name, prepared.releaseName, prepared, configure)
				wantRejected := 0
				if tc.rejected {
					wantRejected = 1
				}
				if err == nil || posts != 1 || len(authorized) != 1 || len(rejected) != wantRejected {
					t.Fatalf("grant outcome: posts=%d authorized=%v rejected=%v error=%v", posts, authorized, rejected, err)
				}
				if tc.rejected && rejected[0] != authorized[0] {
					t.Fatal("rejection was applied to a different grant")
				}
				if tc.name == "receipt-failure" && !errors.Is(err, receiptFailure) {
					t.Fatal("rejection checkpoint failure was hidden:", err)
				}
				if tc.name == "lost-response" {
					found := false
					for _, object := range api.objects {
						if object.GetUID() == "completed-without-response" {
							found = true
						}
					}
					if !found {
						t.Fatal("lost-response fixture did not commit its grant")
					}
				}
			})
		}
	}
}
