// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	admissionv1 "k8s.io/api/admission/v1"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var (
	admissionDecisions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cetirun_sandbox_admission_decisions_total",
		Help: "Sandbox admission decisions by handler and bounded outcome, including denials returned over HTTP 200.",
	}, []string{"handler", "outcome"})
	admissionRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "cetirun_sandbox_admission_request_duration_seconds",
		Help:    "Time spent in completed Sandbox admission handlers, excluding transport and response serialization, by bounded handler, operation and outcome.",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	}, []string{"handler", "operation", "outcome"})
	podAdmissionDenials = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cetirun_sandbox_pod_admission_denials_total",
		Help: "Protected Pod admission denials by bounded reason code, without source identities or values.",
	}, []string{"reason"})
	servingCertificateExpiry = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "cetirun_sandbox_admission_certificate_not_after_seconds",
		Help: "Unix expiry time of the current serving certificate; zero when serving material cannot be read or parsed.",
	})
)

func init() {
	metrics.Registry.MustRegister(admissionDecisions, admissionRequestDuration, podAdmissionDenials, servingCertificateExpiry)
}

func instrumentAdmission(name string, handler admission.HandlerFunc) admission.HandlerFunc {
	switch name {
	case "validate", "default", "pod", "placement":
	default:
		name = "unknown"
	}
	return func(ctx context.Context, request admission.Request) admission.Response {
		started := time.Now()
		response := handler(ctx, request)
		duration := time.Since(started).Seconds()
		outcome := "denied"
		switch {
		case response.Allowed:
			outcome = "allowed"
		case response.Result != nil && response.Result.Code >= http.StatusInternalServerError:
			outcome = "dependency_error"
		case response.Result != nil && response.Result.Code == http.StatusBadRequest:
			outcome = "invalid_request"
		}
		admissionDecisions.WithLabelValues(name, outcome).Inc()
		operation := "unknown"
		switch request.Operation {
		case admissionv1.Create, admissionv1.Update, admissionv1.Delete, admissionv1.Connect:
			operation = string(request.Operation)
		}
		admissionRequestDuration.WithLabelValues(name, operation, outcome).Observe(duration)
		return response
	}
}
