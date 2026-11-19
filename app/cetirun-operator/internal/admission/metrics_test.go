// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const admissionDurationMetric = "cetirun_sandbox_admission_request_duration_seconds"

func admissionDurationSnapshot(t *testing.T, handler, operation, outcome string) *dto.Histogram {
	t.Helper()
	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != admissionDurationMetric {
			continue
		}
		for _, metric := range family.Metric {
			labels := make(map[string]string)
			for _, label := range metric.Label {
				labels[label.GetName()] = label.GetValue()
			}
			if reflect.DeepEqual(labels, map[string]string{
				"handler": handler, "operation": operation, "outcome": outcome,
			}) {
				return metric.GetHistogram()
			}
		}
	}
	return &dto.Histogram{}
}

func TestAdmissionMetricsDurationPreservesResponses(t *testing.T) {
	patch := admission.PatchResponseFromRaw([]byte(`{"spec":{}}`), []byte(`{"spec":{"sandboxRef":"synthetic-sandbox"}}`))
	patch.UID = "synthetic-response"
	patch.Warnings = []string{"synthetic warning"}
	patch.AuditAnnotations = map[string]string{"example.com/test": "synthetic annotation"}
	for _, name := range []string{"validate", "default", "pod", "placement"} {
		for _, operation := range []admissionv1.Operation{
			admissionv1.Create, admissionv1.Update, admissionv1.Delete, admissionv1.Connect, "",
		} {
			for _, test := range []struct {
				outcome  string
				response admission.Response
			}{
				{"allowed", admission.Allowed("")},
				{"allowed", patch},
				{"denied", admission.Denied("synthetic denial")},
				{"invalid_request", admission.Errored(http.StatusBadRequest, fmt.Errorf("synthetic malformed input"))},
				{"dependency_error", admission.Errored(http.StatusServiceUnavailable, fmt.Errorf("synthetic dependency failure"))},
			} {
				t.Run(fmt.Sprintf("%s/%s/%s/patch=%t", name, operation, test.outcome, len(test.response.Patches) != 0), func(t *testing.T) {
					operationLabel := string(operation)
					if operationLabel == "" {
						operationLabel = "unknown"
					}
					before := admissionDurationSnapshot(t, name, operationLabel, test.outcome)
					request := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
						Operation: operation, UID: "synthetic-request", Name: "synthetic-agent",
					}}
					original := request.DeepCopy()
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					var calls int
					handler := instrumentAdmission(name, func(gotContext context.Context, gotRequest admission.Request) admission.Response {
						calls++
						if gotContext != ctx || !reflect.DeepEqual(gotRequest, request) {
							t.Fatal("instrumentation changed the context or request")
						}
						return test.response
					})
					response := handler(ctx, request)
					if calls != 1 || !reflect.DeepEqual(response, test.response) || !reflect.DeepEqual(request.DeepCopy(), original) {
						t.Fatal("instrumentation changed the handler call, request, or complete response")
					}
					after := admissionDurationSnapshot(t, name, operationLabel, test.outcome)
					if after.GetSampleCount() != before.GetSampleCount()+1 || after.GetSampleSum() <= before.GetSampleSum() {
						t.Fatal("a completed handler must contribute exactly one positive duration")
					}
					wantBounds := []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}
					if len(after.Bucket) != len(wantBounds) {
						t.Fatal("duration buckets do not cover short handlers and the maximum webhook deadline")
					}
					for i, bound := range wantBounds {
						if after.Bucket[i].GetUpperBound() != bound {
							t.Fatalf("unexpected duration bucket: %v", after.Bucket[i])
						}
					}
				})
			}
		}
	}
}

func TestAdmissionMetricsDurationIncludesHandlerWaitAndCancellation(t *testing.T) {
	before := admissionDurationSnapshot(t, "pod", "CREATE", "dependency_error")
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan time.Time, 1)
	held := make(chan time.Duration, 1)
	handler := instrumentAdmission("pod", func(ctx context.Context, _ admission.Request) admission.Response {
		start := time.Now()
		started <- start
		<-ctx.Done()
		held <- time.Since(start)
		return admission.Errored(http.StatusServiceUnavailable, ctx.Err())
	})
	done := make(chan admission.Response, 1)
	go func() {
		done <- handler(ctx, admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Operation: admissionv1.Create}})
	}()
	t.Cleanup(cancel)
	<-started
	cancel()
	response := <-done
	if response.Allowed || response.Result == nil || response.Result.Code != http.StatusServiceUnavailable {
		t.Fatal("instrumentation replaced the cancelled handler's response")
	}
	after := admissionDurationSnapshot(t, "pod", "CREATE", "dependency_error")
	if after.GetSampleCount() != before.GetSampleCount()+1 || after.GetSampleSum()-before.GetSampleSum() < (<-held).Seconds() {
		t.Fatal("duration omitted time spent inside the handler")
	}
}

func TestAdmissionMetricsDurationDoesNotRecoverPanics(t *testing.T) {
	before := admissionDurationSnapshot(t, "validate", "UPDATE", "allowed")
	sentinel := fmt.Errorf("synthetic handler panic")
	func() {
		defer func() {
			if recovered := recover(); recovered != sentinel {
				t.Fatalf("instrumentation changed panic propagation: %v", recovered)
			}
		}()
		instrumentAdmission("validate", func(context.Context, admission.Request) admission.Response {
			panic(sentinel)
		})(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Operation: admissionv1.Update}})
	}()
	after := admissionDurationSnapshot(t, "validate", "UPDATE", "allowed")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("a handler that did not return must not be recorded as an allowed decision")
	}
}

func TestAdmissionMetricsDurationBoundsLabelsAndHTTPExposition(t *testing.T) {
	const requests = 240
	before := admissionDurationSnapshot(t, "unknown", "unknown", "denied")
	for i := range requests {
		opaque := fmt.Sprintf("synthetic-private-value-%d", i)
		request := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Operation(opaque), UID: types.UID(opaque), Name: opaque, Namespace: opaque,
		}}
		request.UserInfo.Username = opaque
		handler := instrumentAdmission(opaque, func(_ context.Context, got admission.Request) admission.Response {
			if !reflect.DeepEqual(got, request) {
				t.Fatal("bounded telemetry rewrote the actual request")
			}
			return admission.Denied(opaque)
		})
		body, err := json.Marshal(admissionv1.AdmissionReview{
			TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
			Request:  &request.AdmissionRequest,
		})
		if err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		httpRequest := httptest.NewRequest(http.MethodPost, ValidationPath, bytes.NewReader(body))
		httpRequest.Header.Set("Content-Type", "application/json")
		(&admission.Webhook{Handler: handler}).ServeHTTP(recorder, httpRequest)
		var review admissionv1.AdmissionReview
		if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &review) != nil ||
			review.Response == nil || review.Response.Allowed || review.Response.UID != request.UID {
			t.Fatal("the real HTTP webhook changed the denial or response identity")
		}
	}
	after := admissionDurationSnapshot(t, "unknown", "unknown", "denied")
	if after.GetSampleCount() != before.GetSampleCount()+requests {
		t.Fatal("arbitrary operations or handler names did not collapse to bounded labels")
	}
	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]map[string]bool{
		"handler":   {"validate": true, "default": true, "pod": true, "placement": true, "unknown": true},
		"operation": {"CREATE": true, "UPDATE": true, "DELETE": true, "CONNECT": true, "unknown": true},
		"outcome":   {"allowed": true, "denied": true, "invalid_request": true, "dependency_error": true},
	}
	for _, family := range families {
		wantLabels := 3
		switch family.GetName() {
		case admissionDurationMetric:
			if len(family.Metric) > 5*5*4 {
				t.Fatal("duration label tuples exceeded the bounded domain")
			}
		case "cetirun_sandbox_admission_decisions_total":
			wantLabels = 2
		default:
			continue
		}
		for _, metric := range family.Metric {
			if len(metric.Label) != wantLabels {
				t.Fatal("metric label shape changed")
			}
			for _, label := range metric.Label {
				if !allowed[label.GetName()][label.GetValue()] {
					t.Fatalf("unbounded label in %s", family.GetName())
				}
			}
		}
	}
	recorder := httptest.NewRecorder()
	promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), admissionDurationMetric+"_bucket{") ||
		!strings.Contains(recorder.Body.String(), admissionDurationMetric+"_count{") ||
		!strings.Contains(recorder.Body.String(), admissionDurationMetric+"_sum{") ||
		strings.Contains(recorder.Body.String(), "synthetic-private-value") {
		t.Fatal("HTTP exposition omitted duration samples or exposed request/response values")
	}
}

func TestAdmissionMetricsPreserveLegacyAndCleanupPaths(t *testing.T) {
	f := newFixture(t)
	reader := &unavailablePodReader{}
	f.handler.reader = reader
	podHandler, err := NewPodHandler(reader, PodConfig{StateNamespace: "operator-state"})
	if err != nil {
		t.Fatal(err)
	}
	legacy := f.agent.DeepCopy()
	legacy.Name, legacy.GenerateName, legacy.UID, legacy.ResourceVersion = "", "generated-agent-", "", ""
	legacy.Spec.Team, legacy.Spec.SandboxRef = "", ""
	create := requestFor(t, admissionv1.Create, legacy, nil, "portal")
	team := f.team.DeepCopy()
	team.Spec.MemberContext.Role = "synthetic updated application context"
	old := f.agent.DeepCopy()
	old.Finalizers = []string{api.Finalizer, sandbox.HoldFinalizer, sandbox.PreparationFinalizer, "example.com/other"}
	deleting := metav1.NewTime(time.Unix(1700000000, 0))
	old.DeletionTimestamp = &deleting
	candidate := old.DeepCopy()
	candidate.Finalizers = []string{"example.com/other"}
	status := requestFor(t, admissionv1.Update, f.agent, f.agent, testController)
	status.SubResource = "status"
	pod, oldPod := podFinalizationPair()
	podCleanup := podRequest(t, pod, admissionv1.Update, oldPod)
	podCleanup.UserInfo.Username = "system:serviceaccount:kube-system:generic-garbage-collector"
	for _, test := range []struct {
		name    string
		handler admission.HandlerFunc
		request admission.Request
	}{
		{"default", f.handler.DefaultAgent, create},
		{"validate", f.handler.Validate, create},
		{"validate", f.handler.Validate, requestFor(t, admissionv1.Update, team, f.team, "portal")},
		{"validate", f.handler.Validate, requestFor(t, admissionv1.Delete, f.agent, nil, "member")},
		{"validate", f.handler.Validate, status},
		{"validate", f.handler.Validate, requestFor(t, admissionv1.Update, candidate, old, testController)},
		{"pod", podHandler.Validate, admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Operation: admissionv1.Delete}}},
		{"pod", podHandler.Validate, admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Operation: admissionv1.Update, SubResource: "status"}}},
		{"pod", podHandler.Validate, podCleanup},
	} {
		before := test.handler(context.Background(), test.request)
		after := instrumentAdmission(test.name, test.handler)(context.Background(), test.request)
		if !before.Allowed || !reflect.DeepEqual(before, after) {
			t.Fatalf("%s instrumentation changed a legacy/maintenance response", test.name)
		}
	}
	if reader.calls != 0 {
		t.Fatal("metrics made legacy/maintenance paths depend on resource reads")
	}
}

func TestAdmissionMetricsRequestsDoNotWaitForScrapes(t *testing.T) {
	scrapeStarted, releaseScrape, scrapeDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	slowRegistry := prometheus.NewRegistry()
	slowRegistry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "synthetic_slow_collector", Help: "Synthetic collector held until requests complete.",
	}, func() float64 {
		close(scrapeStarted)
		<-releaseScrape
		return 1
	}))
	recorder := httptest.NewRecorder()
	go func() {
		defer close(scrapeDone)
		promhttp.HandlerFor(prometheus.Gatherers{slowRegistry, metrics.Registry}, promhttp.HandlerOpts{}).
			ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	}()
	t.Cleanup(func() {
		close(releaseScrape)
		<-scrapeDone
	})
	select {
	case <-scrapeStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("synthetic scrape did not start")
	}
	before := admissionDurationSnapshot(t, "validate", "DELETE", "allowed")
	const requests = 32
	var group sync.WaitGroup
	handler := instrumentAdmission("validate", func(context.Context, admission.Request) admission.Response {
		return admission.Allowed("synthetic cleanup")
	})
	for range requests {
		group.Go(func() {
			if !handler(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Operation: admissionv1.Delete}}).Allowed {
				t.Error("concurrent instrumentation changed admission")
			}
		})
	}
	done := make(chan struct{})
	go func() { group.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("admission waited for a blocked scrape")
	}
	after := admissionDurationSnapshot(t, "validate", "DELETE", "allowed")
	if after.GetSampleCount() != before.GetSampleCount()+requests {
		t.Fatal("concurrent instrumentation lost or duplicated observations")
	}
}
