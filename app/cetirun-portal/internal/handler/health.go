// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"net/http"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// HealthHandler provides liveness and readiness probes.
type HealthHandler struct {
	KubeClient client.Client
}

// Liveness always returns 200 — if the process is running, it's alive.
func (h *HealthHandler) Liveness(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

// Readiness pings the Kubernetes API server to verify connectivity.
func (h *HealthHandler) Readiness(w http.ResponseWriter, r *http.Request) {
	list := &cetirunv1alpha1.AgentList{}
	if err := h.KubeClient.List(r.Context(), list, client.Limit(1)); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("kubernetes API unavailable"))
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}
