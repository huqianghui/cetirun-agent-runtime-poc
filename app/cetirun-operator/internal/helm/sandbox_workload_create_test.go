// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/action"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestReconciledSandboxSDKWorkloadCreateOutcomes(t *testing.T) {
	for _, outcome := range []string{"forbidden", "throttled", "timeout", "lost-response", "earlier-submission"} {
		t.Run(outcome, func(t *testing.T) {
			source, _, _ := sandboxSDKKubernetesAccessPlans(t)
			prepared := reconciledGrantSDKPlan(t, source, func(context.Context, unstructured.Unstructured) (func(context.Context) error, error) {
				return func(context.Context) error { return nil }, nil
			})
			cfg, server, _ := sandboxSDKConfig()
			submitted, cancelled, posts := false, false, 0
			prepared.sandboxWorkloadCreate = func(context.Context) (func(context.Context) error, error) {
				submitted = true
				if outcome == "earlier-submission" {
					return nil, nil
				}
				return func(context.Context) error { cancelled = true; return nil }, nil
			}
			server.respond = func(call sandboxSDKRequest) (*http.Response, error) {
				if call.method != http.MethodPost || !strings.HasSuffix(call.path, "/statefulsets") {
					return nil, nil
				}
				posts++
				if !submitted {
					t.Fatal("workload CREATE preceded its submission checkpoint")
				}
				if posts > 1 {
					return sandboxSDKStatus(http.StatusForbidden, metav1.StatusReasonForbidden)
				}
				switch outcome {
				case "lost-response":
					return nil, io.ErrUnexpectedEOF
				case "timeout":
					response, err := sandboxSDKStatus(http.StatusGatewayTimeout, metav1.StatusReasonTimeout)
					response.Header.Set("Retry-After", "0")
					return response, err
				case "throttled":
					response, err := sandboxSDKStatus(http.StatusTooManyRequests, metav1.StatusReasonTooManyRequests)
					response.Header.Set("Retry-After", "0")
					return response, err
				default:
					return sandboxSDKStatus(http.StatusForbidden, metav1.StatusReasonForbidden)
				}
			}
			err := (&Client{}).installOrUpgrade(prepared.sandboxReconcile.Namespace.Name, prepared.releaseName, prepared,
				func(string) (*action.Configuration, error) { return cfg, nil })
			wantCancellation := outcome == "forbidden" || outcome == "throttled"
			if err == nil || posts != 1 || cancelled != wantCancellation {
				t.Fatalf("incorrect first CREATE outcome: posts=%d cancelled=%t err=%v", posts, cancelled, err)
			}
		})
	}
}
