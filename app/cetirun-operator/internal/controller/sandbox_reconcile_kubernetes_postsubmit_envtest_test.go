// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package controller

import (
	"context"
	"errors"
	"testing"

	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestReconciledSandboxKubernetesPostSubmitRecoveryAPIServer(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		t.Run(kind, func(t *testing.T) {
			h := newUnreconciledKubernetesAccessAPIHarness(t, kind)
			originalClient, originalReader := h.r.Client, h.r.APIReader
			injected := errors.New("synthetic final apply authority read failure")
			var submittedKey client.ObjectKey
			var claimedVersion string
			rejected := false
			h.r.Client = interceptor.NewClient(originalClient.(client.WithWatch), interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if err := c.Patch(ctx, object, patch, opts...); err != nil {
						return err
					}
					if state, ok := object.(*corev1.ConfigMap); ok && !rejected &&
						state.Data[w.PodExpectationKey] != "" && state.Annotations[sandboxApplyAttemptAnnotation] != "" {
						submittedKey = client.ObjectKeyFromObject(state)
						claimedVersion = state.ResourceVersion
					}
					return nil
				},
			})
			h.r.APIReader = interceptor.NewClient(h.kube, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
					if submittedKey.Name != "" && key == submittedKey && !rejected {
						rejected = true
						return injected
					}
					return c.Get(ctx, key, object, opts...)
				},
			})
			var failed error
			for range 5 {
				_, failed = h.reconcile()
				if failed != nil {
					break
				}
			}
			if !errors.Is(failed, injected) || !rejected || h.installer.stages != 0 {
				t.Fatal("served fixture did not reject the final pre-write guard:", failed)
			}
			expected := reconciledExpectationForTest(t, h.nativeSandboxFixture)
			state := &corev1.ConfigMap{}
			if err := h.admin.Get(h.ctx, submittedKey, state); err != nil ||
				state.Annotations[sandboxApplyStateAnnotation] != sandboxApplyCancelled ||
				state.ResourceVersion == claimedVersion {
				t.Fatal("served API did not confirm a superseding cancellation:", err)
			}
			if _, found, err := h.r.SandboxNativeCoordination.ReadNative(h.ctx, h.agent.UID); err != nil || found {
				t.Fatal("served cancellation stranded its reservation:", err)
			}
			workloadKey := client.ObjectKey{Namespace: expected.Namespace.Name, Name: instanceServiceName(h.agent.Name)}
			if err := h.admin.Get(h.ctx, workloadKey, &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
				t.Fatal("rejected manifest created a workload:", err)
			}
			h.r.Client, h.r.APIReader = originalClient, originalReader
			for range 4 {
				h.reconcileOK(t)
			}
			assertKubernetesFixtureGrants(t, h.nativeSandboxFixture, true)
			current := reconciledExpectationForTest(t, h.nativeSandboxFixture)
			if current.Home != expected.Home || current.Agent != expected.Agent ||
				current.Namespace != expected.Namespace || h.installer.stages != 1 {
				t.Fatal("served recovery replaced the lifetime or repeated workload applies")
			}
			if err := h.admin.Get(h.ctx, workloadKey, &appsv1.StatefulSet{}); err != nil {
				t.Fatal("served recovery did not create the current workload:", err)
			}
		})
	}
}
