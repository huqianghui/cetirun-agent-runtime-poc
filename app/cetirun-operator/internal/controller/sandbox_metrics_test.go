// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
)

const (
	sandboxMetricsAgentsHelp = "Agents with Sandbox intent or retained hold/history, by declared runtime and reported status; not runtime qualification or fresh enforcement evidence. Only populated groups are emitted."
	sandboxMetricsHealthHelp = "Whether all Sandbox reported-status cache reads completed successfully (1) or fleet counts were omitted (0)."
)

type sandboxMetricsCacheCall struct {
	kind string
	ctx  context.Context
	at   time.Time
}

type sandboxMetricsTestCache struct {
	// Unexpected Get, cache-start, or observer methods fail rather than hiding
	// live-read or lifecycle dependencies behind the fixture.
	cache.Cache
	mu               sync.Mutex
	agents           api.AgentList
	teams            api.TeamList
	sandboxes        api.SandboxList
	synced           bool
	failures         map[string]error
	block            string
	blockSuccess     bool
	calls            []sandboxMetricsCacheCall
	informers        map[string]bool
	informerCalls    []sandboxMetricsCacheCall
	informerFailures map[string]error
	informerBlock    string
	informerEntered  chan struct{}
	informerRelease  chan struct{}
}

func newSandboxMetricsTestCache(t *testing.T, objects ...client.Object) *sandboxMetricsTestCache {
	t.Helper()
	source := &sandboxMetricsTestCache{synced: true, informers: make(map[string]bool)}
	source.replace(t, objects...)
	return source
}

func (c *sandboxMetricsTestCache) replace(t *testing.T, objects ...client.Object) {
	t.Helper()
	var agents api.AgentList
	var teams api.TeamList
	var sandboxes api.SandboxList
	for _, object := range objects {
		switch object := object.(type) {
		case *api.Agent:
			agents.Items = append(agents.Items, *object.DeepCopy())
		case *api.Team:
			teams.Items = append(teams.Items, *object.DeepCopy())
		case *api.Sandbox:
			sandboxes.Items = append(sandboxes.Items, *object.DeepCopy())
		default:
			t.Fatalf("unexpected metrics fixture object %T", object)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.agents, c.teams, c.sandboxes = agents, teams, sandboxes
}

func (c *sandboxMetricsTestCache) objects() []client.Object {
	c.mu.Lock()
	defer c.mu.Unlock()
	var objects []client.Object
	for i := range c.agents.Items {
		objects = append(objects, c.agents.Items[i].DeepCopy())
	}
	for i := range c.teams.Items {
		objects = append(objects, c.teams.Items[i].DeepCopy())
	}
	for i := range c.sandboxes.Items {
		objects = append(objects, c.sandboxes.Items[i].DeepCopy())
	}
	return objects
}

func (c *sandboxMetricsTestCache) WaitForCacheSync(ctx context.Context) bool {
	c.mu.Lock()
	c.calls = append(c.calls, sandboxMetricsCacheCall{"sync", ctx, time.Now()})
	synced, block := c.synced, c.block == "sync"
	c.mu.Unlock()
	if block {
		<-ctx.Done()
		return false
	}
	return synced
}

func (c *sandboxMetricsTestCache) List(ctx context.Context, out client.ObjectList, opts ...client.ListOption) error {
	c.mu.Lock()
	var kind string
	// Return shared, immutable object contents so accidental collector mutations
	// are visible. Updates replace the population rather than modifying it.
	switch out := out.(type) {
	case *api.AgentList:
		kind, *out = "agents", c.agents
	case *api.TeamList:
		kind, *out = "teams", c.teams
	case *api.SandboxList:
		kind, *out = "sandboxes", c.sandboxes
	default:
		c.mu.Unlock()
		return fmt.Errorf("unexpected cached list %T", out)
	}
	c.calls = append(c.calls, sandboxMetricsCacheCall{kind, ctx, time.Now()})
	err, block, blockSuccess := c.failures[kind], c.block == kind, c.blockSuccess
	initialized := c.informers[kind]
	c.mu.Unlock()
	if !initialized {
		return fmt.Errorf("List would bootstrap a missing %s informer", kind)
	}
	if len(opts) != 0 {
		return fmt.Errorf("fleet metrics must not filter or limit cached lists")
	}
	if block {
		<-ctx.Done()
		if !blockSuccess {
			return ctx.Err()
		}
	}
	return err
}

func sandboxMetricsFixture(t *testing.T, name string, runtime api.SandboxType) (*api.Agent, *api.Team, *api.Sandbox) {
	t.Helper()
	box := &api.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-sandbox", UID: types.UID(name + "-sandbox-uid")},
		Spec:       api.SandboxSpec{Type: runtime},
	}
	policy := &api.AgentPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-policy", UID: types.UID(name + "-policy-uid")},
	}
	team := &api.Team{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-team", UID: types.UID(name + "-team-uid")},
		Spec: api.TeamSpec{
			SandboxRefs: []string{box.Name}, DefaultSandboxRef: box.Name, AgentPolicyRef: policy.Name,
		},
	}
	agent := &api.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, UID: types.UID(name + "-uid"), Generation: 5,
			Labels: map[string]string{"example.com/identity": name},
		},
		Spec: api.AgentSpec{Team: team.Name, SandboxRef: box.Name},
		Status: api.AgentStatus{
			ObservedGeneration: 2,
			AccessURL:          "https://" + name + ".example.com/",
			Conditions: []metav1.Condition{{
				Type: api.ConditionSandboxReady, Status: metav1.ConditionTrue, Reason: "PolicyApplied",
				Message: "Synthetic report for " + name, ObservedGeneration: 5,
				LastTransitionTime: metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
			}},
		},
	}
	record, err := sandboxbinding.Snapshot(agent, team, sandbox.Resources{
		Sandboxes: map[string]*api.Sandbox{box.Name: box}, AgentPolicies: map[string]*api.AgentPolicy{policy.Name: policy},
	})
	if err != nil {
		t.Fatal(err)
	}
	agent.Annotations = map[string]string{
		sandboxbinding.AuthorizationAnnotation: record,
		"example.com/opaque":                   name + "-opaque",
	}
	return agent, team, box
}

func sandboxMetricsRegistry(t *testing.T, source cache.Cache) *prometheus.Registry {
	t.Helper()
	registry, collector := sandboxMetricsRegisteredCollector(t, source)
	if err := collector.initializeCache(context.Background()); err != nil {
		t.Fatal(err)
	}
	return registry
}

func readSandboxMetrics(registry *prometheus.Registry) (map[string]float64, float64, error) {
	families, err := registry.Gather()
	if err != nil {
		return nil, 0, err
	}
	bounds := map[string][]string{
		"runtime": {"kata", "gvisor", "unknown"},
		"status":  {"True", "False", "Unknown"},
		"reason": {
			"PolicyApplied", "ObservationUnavailable", "EnforcementPending", "EnforcementLost",
			"RuntimeUnavailable", "UnsupportedControl", "ReferenceMissing", "UnauthorizedBinding",
			"PolicyConflict", "PolicyApplyFailed", "SandboxPreparationPending", "SandboxPrepared",
			"Suspended", "Deleting", "InstanceDeleting",
			"ConditionMissing", "ConditionStale", "ConditionMalformed", "Other",
		},
	}
	counts := make(map[string]float64)
	health := -1.0
	for _, family := range families {
		if family.GetType() != dto.MetricType_GAUGE {
			return nil, 0, fmt.Errorf("unexpected metric type: %v", family)
		}
		for _, metric := range family.Metric {
			if metric.Gauge == nil || metric.Gauge.Value == nil || metric.Counter != nil ||
				metric.Histogram != nil || metric.Summary != nil || metric.Untyped != nil || metric.TimestampMs != nil {
				return nil, 0, fmt.Errorf("unexpected gauge fields: %v", metric)
			}
		}
		switch family.GetName() {
		case "cetirun_sandbox_metrics_collection_success":
			if family.GetHelp() != sandboxMetricsHealthHelp || len(family.Metric) != 1 ||
				len(family.Metric[0].Label) != 0 || health != -1 {
				return nil, 0, fmt.Errorf("unexpected collection health shape: %v", family)
			}
			health = family.Metric[0].Gauge.GetValue()
			if health != 0 && health != 1 {
				return nil, 0, fmt.Errorf("invalid collection health: %v", health)
			}
		case "cetirun_sandbox_agents":
			if family.GetHelp() != sandboxMetricsAgentsHelp || len(family.Metric) == 0 {
				return nil, 0, fmt.Errorf("unexpected fleet metric shape: %v", family)
			}
			for _, metric := range family.Metric {
				if len(metric.Label) != 3 {
					return nil, 0, fmt.Errorf("unexpected label count: %v", metric)
				}
				labels := make(map[string]string)
				for _, label := range metric.Label {
					name, value := label.GetName(), label.GetValue()
					if _, exists := labels[name]; exists || !slices.Contains(bounds[name], value) {
						return nil, 0, fmt.Errorf("unbounded or duplicate label: %v", label)
					}
					labels[name] = value
				}
				key := strings.Join([]string{labels["runtime"], labels["status"], labels["reason"]}, "/")
				value := metric.Gauge.GetValue()
				if _, exists := counts[key]; exists || value <= 0 || math.Trunc(value) != value {
					return nil, 0, fmt.Errorf("duplicate or non-count fleet sample: %v", metric)
				}
				counts[key] = value
			}
		default:
			return nil, 0, fmt.Errorf("unexpected metric family: %s", family.GetName())
		}
	}
	if health == -1 || (health == 0 && len(counts) != 0) {
		return nil, 0, fmt.Errorf("missing health or partial fleet counts: %v, %v", health, counts)
	}
	return counts, health, nil
}

func assertSandboxMetrics(t *testing.T, registry *prometheus.Registry, want map[string]float64, health float64) {
	t.Helper()
	got, gotHealth, err := readSandboxMetrics(registry)
	if err != nil {
		t.Fatal(err)
	}
	if want == nil {
		want = map[string]float64{}
	}
	if gotHealth != health || !reflect.DeepEqual(got, want) {
		t.Fatalf("metrics = %v, health %v; want %v, health %v", got, gotHealth, want, health)
	}
}

func sandboxMetricsExposition(t *testing.T, registry *prometheus.Registry) string {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "http://metrics.example.com/metrics", nil)
	request.Header.Set("Accept", "text/plain; version=0.0.4")
	response := httptest.NewRecorder()
	promhttp.HandlerFor(registry, promhttp.HandlerOpts{DisableCompression: true}).ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.HasPrefix(response.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("unexpected metrics HTTP response: %d, %s", response.Code, response.Body.String())
	}
	return response.Body.String()
}

func TestSandboxMetricsExposition(t *testing.T) {
	var objects []client.Object
	for _, item := range []struct {
		name    string
		runtime api.SandboxType
		status  metav1.ConditionStatus
		reason  string
	}{
		{"example-one", api.SandboxKata, metav1.ConditionTrue, "PolicyApplied"},
		{"example-two", api.SandboxKata, metav1.ConditionTrue, "PolicyApplied"},
		{"example-three", api.SandboxKata, metav1.ConditionFalse, "EnforcementPending"},
		{"example-four", api.SandboxGVisor, metav1.ConditionUnknown, "ObservationUnavailable"},
		{"example-five", api.SandboxGVisor, metav1.ConditionFalse, "SandboxPrepared"},
	} {
		agent, team, box := sandboxMetricsFixture(t, item.name, item.runtime)
		agent.Status.Conditions[0].Status, agent.Status.Conditions[0].Reason = item.status, item.reason
		objects = append(objects, agent, team, box)
	}
	source := newSandboxMetricsTestCache(t, objects...)
	before := source.objects()
	registry := sandboxMetricsRegistry(t, source)
	assertSandboxMetrics(t, registry, map[string]float64{
		"kata/True/PolicyApplied":               2,
		"kata/False/EnforcementPending":         1,
		"gvisor/Unknown/ObservationUnavailable": 1,
		"gvisor/False/SandboxPrepared":          1,
	}, 1)
	want := `# HELP cetirun_sandbox_agents ` + sandboxMetricsAgentsHelp + `
# TYPE cetirun_sandbox_agents gauge
cetirun_sandbox_agents{reason="EnforcementPending",runtime="kata",status="False"} 1
cetirun_sandbox_agents{reason="ObservationUnavailable",runtime="gvisor",status="Unknown"} 1
cetirun_sandbox_agents{reason="PolicyApplied",runtime="kata",status="True"} 2
cetirun_sandbox_agents{reason="SandboxPrepared",runtime="gvisor",status="False"} 1
# HELP cetirun_sandbox_metrics_collection_success ` + sandboxMetricsHealthHelp + `
# TYPE cetirun_sandbox_metrics_collection_success gauge
cetirun_sandbox_metrics_collection_success 1
`
	if got := sandboxMetricsExposition(t, registry); got != want {
		t.Fatalf("exposition:\n%s\nwant:\n%s", got, want)
	}
	if !reflect.DeepEqual(before, source.objects()) {
		t.Fatal("collection modified cached objects")
	}
}

func TestSandboxMetricsPopulation(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		assertSandboxMetrics(t, sandboxMetricsRegistry(t, newSandboxMetricsTestCache(t)), nil, 1)
	})
	for _, tc := range []struct {
		name   string
		mutate func(*api.Agent, *api.Team)
		want   string
	}{
		{"legacy normal", func(*api.Agent, *api.Team) {}, ""},
		{"legacy suspended", func(a *api.Agent, _ *api.Team) { a.Spec.Suspended = true }, ""},
		{"legacy deleting", func(a *api.Agent, _ *api.Team) { a.DeletionTimestamp = &metav1.Time{Time: time.Now()} }, ""},
		{"missing Team", func(a *api.Agent, _ *api.Team) { a.Spec.Team = "example-absent" }, ""},
		{"Team policy intent", func(_ *api.Agent, team *api.Team) { team.Spec.AgentPolicyRef = "example-policy" }, "unknown/Unknown/ConditionMissing"},
		{"Team Sandbox list intent", func(_ *api.Agent, team *api.Team) { team.Spec.SandboxRefs = []string{"example-sandbox"} }, "unknown/Unknown/ConditionMissing"},
		{"Team default is not authorization", func(_ *api.Agent, team *api.Team) { team.Spec.DefaultSandboxRef = "example-sandbox" }, "unknown/Unknown/ConditionMissing"},
		{"unrelated governed Team", func(a *api.Agent, team *api.Team) {
			a.Spec.Team = "example-other-team"
			team.Spec.DefaultSandboxRef = "example-sandbox"
		}, ""},
		{"explicit Sandbox", func(a *api.Agent, _ *api.Team) { a.Spec.SandboxRef = "example-sandbox" }, "unknown/Unknown/ConditionMissing"},
		{"hold after cleared fields", func(a *api.Agent, _ *api.Team) {
			a.Spec = api.AgentSpec{}
			a.Finalizers = []string{sandbox.HoldFinalizer}
		}, "unknown/Unknown/ConditionMissing"},
		{"preparation after cleared fields", func(a *api.Agent, _ *api.Team) {
			a.Spec = api.AgentSpec{}
			a.Finalizers = []string{sandbox.PreparationFinalizer}
		}, "unknown/Unknown/ConditionMissing"},
		{"history after cleared fields", func(a *api.Agent, _ *api.Team) {
			a.Spec = api.AgentSpec{}
			a.Status.Sandbox = &api.SandboxStatus{
				SandboxUID: "example-sandbox-uid", AppliedRevision: "example-applied",
			}
		}, "unknown/Unknown/ConditionMissing"},
		{"condition after cleared fields", func(a *api.Agent, _ *api.Team) {
			a.Spec = api.AgentSpec{}
			a.Status.Conditions[0].Type = api.ConditionSandboxReady
			a.Status.Conditions[0].ObservedGeneration--
		}, "unknown/Unknown/ConditionStale"},
		{"empty retained authorization", func(a *api.Agent, _ *api.Team) {
			a.Annotations = map[string]string{sandboxbinding.AuthorizationAnnotation: ""}
		}, "unknown/Unknown/ConditionMissing"},
		{"retained reauthorization request", func(a *api.Agent, _ *api.Team) {
			a.Annotations = map[string]string{sandboxbinding.ReauthorizeAnnotation: "opaque-request"}
		}, "unknown/Unknown/ConditionMissing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent, team, box := sandboxMetricsFixture(t, "example", api.SandboxKata)
			agent.Spec.SandboxRef, agent.Annotations = "", nil
			agent.Status.Conditions[0].Type = api.ConditionReady
			team.Spec = api.TeamSpec{}
			tc.mutate(agent, team)
			source := newSandboxMetricsTestCache(t, agent, team, box)
			before := source.objects()
			want := map[string]float64{}
			if tc.want != "" {
				want[tc.want] = 1
			}
			assertSandboxMetrics(t, sandboxMetricsRegistry(t, source), want, 1)
			if !reflect.DeepEqual(before, source.objects()) {
				t.Fatal("population classification modified cached objects")
			}
		})
	}
}

func TestSandboxMetricsReportedStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*api.Agent)
		want   string
	}{
		{"current condition despite older overall observed generation", func(*api.Agent) {}, "True/PolicyApplied"},
		{"missing condition", func(a *api.Agent) { a.Status.Conditions = nil }, "Unknown/ConditionMissing"},
		{"other condition", func(a *api.Agent) { a.Status.Conditions[0].Type = api.ConditionReady }, "Unknown/ConditionMissing"},
		{"old generation", func(a *api.Agent) { a.Status.Conditions[0].ObservedGeneration-- }, "Unknown/ConditionStale"},
		{"future generation", func(a *api.Agent) { a.Status.Conditions[0].ObservedGeneration++ }, "Unknown/ConditionStale"},
		{"unobserved generation", func(a *api.Agent) { a.Status.Conditions[0].ObservedGeneration = 0 }, "Unknown/ConditionStale"},
		{"unspecified generations", func(a *api.Agent) {
			a.Generation, a.Status.Conditions[0].ObservedGeneration = 0, 0
		}, "Unknown/ConditionStale"},
		{"negative observed generation", func(a *api.Agent) { a.Status.Conditions[0].ObservedGeneration = -1 }, "Unknown/ConditionMalformed"},
		{"empty status", func(a *api.Agent) { a.Status.Conditions[0].Status = "" }, "Unknown/ConditionMalformed"},
		{"opaque status", func(a *api.Agent) { a.Status.Conditions[0].Status = "opaque-status" }, "Unknown/ConditionMalformed"},
		{"noncanonical status", func(a *api.Agent) { a.Status.Conditions[0].Status = "true" }, "Unknown/ConditionMalformed"},
		{"empty reason", func(a *api.Agent) { a.Status.Conditions[0].Reason = "" }, "Unknown/ConditionMalformed"},
		{"malformed reason", func(a *api.Agent) { a.Status.Conditions[0].Reason = "https://opaque.example.com/" }, "Unknown/ConditionMalformed"},
		{"opaque reason", func(a *api.Agent) { a.Status.Conditions[0].Reason = "OpaqueReason" }, "True/Other"},
		{"maximum valid reason", func(a *api.Agent) { a.Status.Conditions[0].Reason = strings.Repeat("R", 1024) }, "True/Other"},
		{"oversized reason", func(a *api.Agent) { a.Status.Conditions[0].Reason = strings.Repeat("R", 1025) }, "Unknown/ConditionMalformed"},
		{"maximum valid message", func(a *api.Agent) { a.Status.Conditions[0].Message = strings.Repeat("M", 32768) }, "True/PolicyApplied"},
		{"oversized message", func(a *api.Agent) { a.Status.Conditions[0].Message = strings.Repeat("M", 32769) }, "Unknown/ConditionMalformed"},
		{"missing transition time", func(a *api.Agent) { a.Status.Conditions[0].LastTransitionTime = metav1.Time{} }, "Unknown/ConditionMalformed"},
		{"old transition is not an observation age", func(a *api.Agent) {
			a.Status.Conditions[0].LastTransitionTime = metav1.NewTime(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
		}, "True/PolicyApplied"},
		{"duplicate identical conditions", func(a *api.Agent) {
			a.Status.Conditions = append(a.Status.Conditions, a.Status.Conditions[0])
		}, "Unknown/ConditionMalformed"},
		{"duplicate conflicting conditions", func(a *api.Agent) {
			other := a.Status.Conditions[0]
			other.Status, other.Reason = metav1.ConditionFalse, "PolicyApplyFailed"
			a.Status.Conditions = append(a.Status.Conditions, other)
		}, "Unknown/ConditionMalformed"},
		{"phase does not imply suspension", func(a *api.Agent) { a.Status.Phase = api.PhaseSuspended }, "True/PolicyApplied"},
		{"phase does not imply deletion", func(a *api.Agent) { a.Status.Phase = api.PhaseDeleting }, "True/PolicyApplied"},
		{"applied history is not readiness", func(a *api.Agent) {
			a.Status.Sandbox = &api.SandboxStatus{AppliedRevision: "example-applied"}
			a.Status.Conditions[0].Status, a.Status.Conditions[0].Reason = metav1.ConditionFalse, "EnforcementLost"
		}, "False/EnforcementLost"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent, team, box := sandboxMetricsFixture(t, "example", api.SandboxKata)
			tc.mutate(agent)
			source := newSandboxMetricsTestCache(t, agent, team, box)
			before := source.objects()
			assertSandboxMetrics(t, sandboxMetricsRegistry(t, source), map[string]float64{"kata/" + tc.want: 1}, 1)
			if !reflect.DeepEqual(before, source.objects()) {
				t.Fatal("reported-status classification modified cached objects")
			}
		})
	}
}

func TestSandboxMetricsReasonAllowlist(t *testing.T) {
	for _, tc := range []struct {
		reason string
		status metav1.ConditionStatus
	}{
		{"PolicyApplied", metav1.ConditionTrue},
		{"ObservationUnavailable", metav1.ConditionUnknown},
		{"EnforcementPending", metav1.ConditionFalse},
		{"EnforcementLost", metav1.ConditionFalse},
		{"RuntimeUnavailable", metav1.ConditionFalse},
		{"UnsupportedControl", metav1.ConditionFalse},
		{"ReferenceMissing", metav1.ConditionFalse},
		{"UnauthorizedBinding", metav1.ConditionFalse},
		{"PolicyConflict", metav1.ConditionFalse},
		{"PolicyApplyFailed", metav1.ConditionFalse},
		{"SandboxPreparationPending", metav1.ConditionFalse},
		{"SandboxPrepared", metav1.ConditionFalse},
		{"Suspended", metav1.ConditionFalse},
		{"Deleting", metav1.ConditionFalse},
		{"InstanceDeleting", metav1.ConditionFalse},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			agent, team, box := sandboxMetricsFixture(t, "example", api.SandboxKata)
			agent.Status.Conditions[0].Status, agent.Status.Conditions[0].Reason = tc.status, tc.reason
			registry := sandboxMetricsRegistry(t, newSandboxMetricsTestCache(t, agent, team, box))
			assertSandboxMetrics(t, registry, map[string]float64{"kata/" + string(tc.status) + "/" + tc.reason: 1}, 1)
		})
	}
}

func TestSandboxMetricsLifecyclePrecedence(t *testing.T) {
	for _, lifecycle := range []string{"suspended", "deleting", "both"} {
		for _, condition := range []string{"stale success", "stale failure", "missing", "malformed", "current success"} {
			t.Run(lifecycle+"/"+condition, func(t *testing.T) {
				agent, team, box := sandboxMetricsFixture(t, "example", api.SandboxKata)
				switch condition {
				case "stale success":
					agent.Status.Conditions[0].ObservedGeneration--
				case "stale failure":
					agent.Status.Conditions[0].ObservedGeneration--
					agent.Status.Conditions[0].Status, agent.Status.Conditions[0].Reason = metav1.ConditionFalse, "PolicyApplyFailed"
				case "missing":
					agent.Status.Conditions = nil
				case "malformed":
					agent.Status.Conditions[0].Status = "opaque-status"
				}
				agent.Spec.Suspended = lifecycle != "deleting"
				want := "kata/False/Suspended"
				if lifecycle != "suspended" {
					agent.DeletionTimestamp = &metav1.Time{Time: time.Now()}
					want = "unknown/False/Deleting"
				}
				source := newSandboxMetricsTestCache(t, agent, team, box)
				before := source.objects()
				assertSandboxMetrics(t, sandboxMetricsRegistry(t, source), map[string]float64{want: 1}, 1)
				if !reflect.DeepEqual(before, source.objects()) {
					t.Fatal("lifecycle classification modified cached objects")
				}
			})
		}
	}
}

func TestSandboxMetricsRuntimeBinding(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*api.Agent, *api.Sandbox)
		want   string
	}{
		{"current kata", func(*api.Agent, *api.Sandbox) {}, "kata"},
		{"current gvisor", func(_ *api.Agent, box *api.Sandbox) { box.Spec.Type = api.SandboxGVisor }, "gvisor"},
		{"declared runtime is not qualification", func(_ *api.Agent, box *api.Sandbox) {
			box.Status.Conditions = []metav1.Condition{{
				Type: api.ConditionReady, Status: metav1.ConditionFalse, Reason: "RuntimeUnavailable",
			}}
		}, "kata"},
		{"absent declaration has no default", func(_ *api.Agent, box *api.Sandbox) { box.Spec.Type = "" }, "unknown"},
		{"opaque declaration", func(_ *api.Agent, box *api.Sandbox) { box.Spec.Type = "OpaqueRuntime" }, "unknown"},
		{"noncanonical declaration", func(_ *api.Agent, box *api.Sandbox) { box.Spec.Type = "KATA" }, "unknown"},
		{"missing Sandbox", func(_ *api.Agent, box *api.Sandbox) { box.Name = "example-other-sandbox" }, "unknown"},
		{"missing Sandbox UID", func(_ *api.Agent, box *api.Sandbox) { box.UID = "" }, "unknown"},
		{"same-name replacement", func(_ *api.Agent, box *api.Sandbox) { box.UID = "replacement-sandbox-uid" }, "unknown"},
		{"history cannot authorize replacement", func(a *api.Agent, box *api.Sandbox) {
			box.UID = "replacement-sandbox-uid"
			a.Status.Sandbox = &api.SandboxStatus{SandboxUID: box.UID, AppliedRevision: "example-history"}
		}, "unknown"},
		{"terminating Sandbox", func(_ *api.Agent, box *api.Sandbox) { box.DeletionTimestamp = &metav1.Time{Time: time.Now()} }, "unknown"},
		{"namespaced Sandbox", func(_ *api.Agent, box *api.Sandbox) { box.Namespace = "example-namespace" }, "unknown"},
		{"missing authorization", func(a *api.Agent, _ *api.Sandbox) { a.Annotations = nil }, "unknown"},
		{"empty authorization", func(a *api.Agent, _ *api.Sandbox) { a.Annotations[sandboxbinding.AuthorizationAnnotation] = "" }, "unknown"},
		{"malformed authorization", func(a *api.Agent, _ *api.Sandbox) {
			a.Annotations[sandboxbinding.AuthorizationAnnotation] = "opaque-record"
		}, "unknown"},
		{"noncanonical authorization", func(a *api.Agent, _ *api.Sandbox) {
			a.Annotations[sandboxbinding.AuthorizationAnnotation] = " " + a.Annotations[sandboxbinding.AuthorizationAnnotation]
		}, "unknown"},
		{"reauthorization is not authorization", func(a *api.Agent, _ *api.Sandbox) {
			a.Annotations = map[string]string{sandboxbinding.ReauthorizeAnnotation: "opaque-request"}
		}, "unknown"},
		{"changed Team selection", func(a *api.Agent, _ *api.Sandbox) { a.Spec.Team = "example-other-team" }, "unknown"},
		{"changed Sandbox selection", func(a *api.Agent, box *api.Sandbox) {
			a.Spec.SandboxRef, box.Name = "example-other-sandbox", "example-other-sandbox"
		}, "unknown"},
		{"cleared Sandbox selection", func(a *api.Agent, _ *api.Sandbox) { a.Spec.SandboxRef = "" }, "unknown"},
		{"missing Agent UID", func(a *api.Agent, _ *api.Sandbox) { a.UID = "" }, "unknown"},
		{"namespaced Agent", func(a *api.Agent, _ *api.Sandbox) { a.Namespace = "example-namespace" }, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent, team, box := sandboxMetricsFixture(t, "example", api.SandboxKata)
			tc.mutate(agent, box)
			source := newSandboxMetricsTestCache(t, agent, team, box)
			before := source.objects()
			assertSandboxMetrics(t, sandboxMetricsRegistry(t, source), map[string]float64{tc.want + "/True/PolicyApplied": 1}, 1)
			if !reflect.DeepEqual(before, source.objects()) {
				t.Fatal("runtime classification modified cached objects")
			}
		})
	}
}

func TestSandboxMetricsBoundedLabels(t *testing.T) {
	var objects []client.Object
	var forbidden []string
	for i := range 240 {
		name := fmt.Sprintf("opaque-example-%03d", i)
		runtime := api.SandboxKata
		if i%4 == 1 {
			runtime = api.SandboxGVisor
		}
		agent, team, box := sandboxMetricsFixture(t, name, runtime)
		condition := &agent.Status.Conditions[0]
		condition.Reason = fmt.Sprintf("OpaqueReason%d", i)
		condition.Message = "Synthetic error for " + name + " at https://" + name + ".example.com/"
		agent.Status.Sandbox = &api.SandboxStatus{
			TeamUID: team.UID, SandboxUID: box.UID,
			AgentPolicyUID: types.UID(name + "-policy-uid"), AppliedRevision: name + "-revision",
		}
		switch i % 4 {
		case 1:
			condition.Status = metav1.ConditionFalse
		case 2:
			box.Spec.Type = api.SandboxType("OpaqueRuntime" + name)
			condition.Status = metav1.ConditionUnknown
		case 3:
			agent.Namespace = name + "-namespace"
			condition.Status = metav1.ConditionStatus("OpaqueStatus" + name)
		}
		forbidden = append(forbidden, name, team.Name, box.Name, string(agent.UID), string(team.UID), string(box.UID),
			team.Spec.AgentPolicyRef, string(agent.Status.Sandbox.AgentPolicyUID), agent.Status.Sandbox.AppliedRevision,
			condition.Reason, condition.Message, agent.Annotations[sandboxbinding.AuthorizationAnnotation], agent.Status.AccessURL)
		if i%4 == 2 {
			forbidden = append(forbidden, string(box.Spec.Type))
		}
		if i%4 == 3 {
			forbidden = append(forbidden, agent.Namespace, string(condition.Status))
		}
		objects = append(objects, agent, team, box)
	}
	source := newSandboxMetricsTestCache(t, objects...)
	before := source.objects()
	registry := sandboxMetricsRegistry(t, source)
	want := map[string]float64{
		"kata/True/Other":                    60,
		"gvisor/False/Other":                 60,
		"unknown/Unknown/Other":              60,
		"unknown/Unknown/ConditionMalformed": 60,
	}
	for range 3 {
		assertSandboxMetrics(t, registry, want, 1)
	}
	body := sandboxMetricsExposition(t, registry)
	for _, value := range forbidden {
		if strings.Contains(body, value) {
			t.Fatalf("opaque object data leaked into exposition: %q", value)
		}
	}
	if !reflect.DeepEqual(before, source.objects()) {
		t.Fatal("bounded-label collection modified cached objects")
	}
}

func TestSandboxMetricsUpdatesAndDeletes(t *testing.T) {
	first, team, box := sandboxMetricsFixture(t, "example", api.SandboxKata)
	second := first.DeepCopy()
	second.Name, second.UID = "example-second", "example-second-uid"
	source := newSandboxMetricsTestCache(t, first, second, team, box)
	registry := sandboxMetricsRegistry(t, source)
	for range 3 {
		assertSandboxMetrics(t, registry, map[string]float64{"kata/True/PolicyApplied": 2}, 1)
	}
	first.Generation++
	first.Status.Conditions[0].ObservedGeneration = first.Generation
	first.Status.Conditions[0].Status, first.Status.Conditions[0].Reason = metav1.ConditionFalse, "RuntimeUnavailable"
	source.replace(t, first, team, box)
	for range 3 {
		assertSandboxMetrics(t, registry, map[string]float64{"kata/False/RuntimeUnavailable": 1}, 1)
	}
	source.replace(t)
	for range 3 {
		assertSandboxMetrics(t, registry, nil, 1)
	}
	first.UID, first.Annotations, first.Status.Conditions = "example-replacement-uid", nil, nil
	source.replace(t, first, team, box)
	assertSandboxMetrics(t, registry, map[string]float64{"unknown/Unknown/ConditionMissing": 1}, 1)
}

func TestSandboxMetricsCollectionFailures(t *testing.T) {
	for _, stage := range []string{"nil cache", "unsynced", "agents", "teams", "sandboxes"} {
		t.Run(stage, func(t *testing.T) {
			agent, team, box := sandboxMetricsFixture(t, "example", api.SandboxKata)
			source := newSandboxMetricsTestCache(t, agent, team, box)
			collector := newSandboxMetricsCollector(source)
			var logMu sync.Mutex
			var logs []string
			collector.logger = funcr.New(func(prefix, message string) {
				logMu.Lock()
				defer logMu.Unlock()
				logs = append(logs, prefix+message)
			}, funcr.Options{})
			registry := prometheus.NewPedanticRegistry()
			if err := registry.Register(collector); err != nil {
				t.Fatal(err)
			}
			if err := collector.initializeCache(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertSandboxMetrics(t, registry, map[string]float64{"kata/True/PolicyApplied": 1}, 1)
			switch stage {
			case "nil cache":
				collector.cache = nil
			case "unsynced":
				source.synced = false
			default:
				// List fills the output before returning this error: even those
				// partial objects must never become fleet samples.
				source.failures = map[string]error{stage: errors.New("synthetic failure with opaque-example-identity")}
			}
			assertSandboxMetrics(t, registry, nil, 0)
			body := sandboxMetricsExposition(t, registry)
			if strings.Contains(body, "cetirun_sandbox_agents") || strings.Contains(body, "opaque-example-identity") ||
				strings.Contains(body, "synthetic failure") || !strings.Contains(body, "cetirun_sandbox_metrics_collection_success 0\n") {
				t.Fatalf("failed collection returned partial counts or error data:\n%s", body)
			}
			logMu.Lock()
			if len(logs) != 2 || !strings.Contains(logs[0], "Unable to collect Sandbox reported-status metrics") {
				t.Errorf("collection failures did not use repository logging: %v", logs)
			}
			logMu.Unlock()
			collector.cache, source.synced, source.failures = source, true, nil
			assertSandboxMetrics(t, registry, map[string]float64{"kata/True/PolicyApplied": 1}, 1)
		})
	}
}

func TestSandboxMetricsReadContext(t *testing.T) {
	agent, team, box := sandboxMetricsFixture(t, "example", api.SandboxKata)
	source := newSandboxMetricsTestCache(t, agent, team, box)
	registry := sandboxMetricsRegistry(t, source)
	if len(source.calls) != 0 {
		t.Fatal("registration read or waited for the cache")
	}
	assertSandboxMetrics(t, registry, map[string]float64{"kata/True/PolicyApplied": 1}, 1)
	var kinds []string
	for _, call := range source.calls {
		kinds = append(kinds, call.kind)
		deadline, ok := call.ctx.Deadline()
		if !ok || deadline.Sub(call.at) <= 0 || deadline.Sub(call.at) > sandboxMetricsCollectionTimeout ||
			call.ctx != source.calls[0].ctx || !errors.Is(call.ctx.Err(), context.Canceled) {
			t.Fatalf("read did not share one bounded, released context: %+v", call)
		}
	}
	if !reflect.DeepEqual(kinds, []string{"sync", "agents", "teams", "sandboxes"}) {
		t.Fatalf("unexpected cache reads: %v", kinds)
	}
}

func TestSandboxMetricsCollectionDeadline(t *testing.T) {
	for _, stage := range []string{"sync", "agents", "late successful sandboxes"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			agent, team, box := sandboxMetricsFixture(t, "example", api.SandboxKata)
			source := newSandboxMetricsTestCache(t, agent, team, box)
			source.block = stage
			if stage == "late successful sandboxes" {
				source.block, source.blockSuccess = "sandboxes", true
			}
			registry := sandboxMetricsRegistry(t, source)
			start := time.Now()
			assertSandboxMetrics(t, registry, nil, 0)
			if elapsed := time.Since(start); elapsed > sandboxMetricsCollectionTimeout+2*time.Second {
				t.Fatalf("collection exceeded its bounded read budget: %v", elapsed)
			}
			for _, call := range source.calls {
				if !errors.Is(call.ctx.Err(), context.DeadlineExceeded) || call.ctx != source.calls[0].ctx {
					t.Fatalf("collection did not expire one shared read context: %+v", call)
				}
			}
		})
	}
}

func TestSandboxMetricsRegistration(t *testing.T) {
	agent, team, box := sandboxMetricsFixture(t, "example", api.SandboxKata)
	first := newSandboxMetricsTestCache(t, agent, team, box)
	other := agent.DeepCopy()
	other.Name, other.UID = "example-other", "example-other-uid"
	second := newSandboxMetricsTestCache(t, agent, other, team, box)
	registry, collector := sandboxMetricsRegisteredCollector(t, first)
	secondManager := &sandboxMetricsTestManager{source: second}
	var alreadyRegistered prometheus.AlreadyRegisteredError
	if err := RegisterSandboxMetrics(registry, secondManager); !errors.As(err, &alreadyRegistered) {
		t.Fatalf("duplicate registration was swallowed or replaced: %v", err)
	}
	if len(first.calls) != 0 || len(second.calls) != 0 || len(first.informerCalls) != 0 ||
		len(second.informerCalls) != 0 || len(secondManager.runnables) != 0 {
		t.Fatal("descriptor registration accessed the cache")
	}
	if err := collector.initializeCache(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertSandboxMetrics(t, registry, map[string]float64{"kata/True/PolicyApplied": 1}, 1)
	assertSandboxMetrics(t, sandboxMetricsRegistry(t, second), map[string]float64{"kata/True/PolicyApplied": 2}, 1)
	if err := RegisterSandboxMetrics(nil, secondManager); err == nil {
		t.Fatal("missing registry did not return an error")
	}
	if err := RegisterSandboxMetrics(prometheus.NewPedanticRegistry(), nil); err == nil {
		t.Fatal("missing manager did not return an error")
	}
	emptyRegistry := prometheus.NewPedanticRegistry()
	nilCacheManager := &sandboxMetricsTestManager{}
	if err := RegisterSandboxMetrics(emptyRegistry, nilCacheManager); err == nil {
		t.Fatal("missing manager cache did not return a configuration error")
	}
	families, err := emptyRegistry.Gather()
	if err != nil || len(families) != 0 || len(nilCacheManager.runnables) != 0 {
		t.Fatalf("invalid cache configuration partially registered metrics: %v, %v", families, err)
	}
}

func TestSandboxMetricsConcurrentScrapes(t *testing.T) {
	agent, team, box := sandboxMetricsFixture(t, "example", api.SandboxKata)
	other := agent.DeepCopy()
	other.Name, other.UID = "example-other", "example-other-uid"
	source := newSandboxMetricsTestCache(t, agent, team, box)
	registry := sandboxMetricsRegistry(t, source)
	const workers = 8
	start := make(chan struct{})
	failures := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 20 {
				counts, health, err := readSandboxMetrics(registry)
				if err != nil {
					failures <- err
					return
				}
				value := counts["kata/True/PolicyApplied"]
				if health != 1 || len(counts) != 1 || (value != 1 && value != 2) {
					failures <- fmt.Errorf("concurrent scrape accumulated or lost counts: %v, health %v", counts, health)
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for range 40 {
			source.replace(t, agent, other, team, box)
			source.replace(t, agent, team, box)
		}
	}()
	close(start)
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	source.replace(t)
	assertSandboxMetrics(t, registry, nil, 1)
}

type sandboxMetricsCancellationContext struct {
	context.Context
	cancel   context.CancelFunc
	cancelAt int64
	checks   atomic.Int64
}

func (c *sandboxMetricsCancellationContext) Err() error {
	if c.checks.Add(1) == c.cancelAt {
		c.cancel()
	}
	return c.Context.Err()
}

type sandboxMetricsContextCollector struct {
	*sandboxMetricsCollector
	ctx context.Context
}

func (c *sandboxMetricsContextCollector) Collect(ch chan<- prometheus.Metric) {
	c.sandboxMetricsCollector.collect(c.ctx, ch)
}

func TestSandboxMetricsAggregationCancellation(t *testing.T) {
	for _, kind := range []string{"teams", "sandboxes", "agents"} {
		t.Run(kind, func(t *testing.T) {
			var objects []client.Object
			for i := range 4096 {
				meta := metav1.ObjectMeta{Name: fmt.Sprintf("example-%04d", i), Generation: 5}
				switch kind {
				case "teams":
					objects = append(objects, &api.Team{ObjectMeta: meta})
				case "sandboxes":
					objects = append(objects, &api.Sandbox{ObjectMeta: meta})
				case "agents":
					objects = append(objects, &api.Agent{ObjectMeta: meta, Spec: api.AgentSpec{SandboxRef: "example-sandbox"}})
				}
			}
			source := newSandboxMetricsTestCache(t, objects...)
			before := source.objects()
			collector := newSandboxMetricsCollector(source)
			if err := collector.initializeCache(context.Background()); err != nil {
				t.Fatal(err)
			}
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			// Cancel after the three list checks and part of this population,
			// without relying on wall-clock speed or allocating a huge fleet.
			ctx := &sandboxMetricsCancellationContext{Context: base, cancel: cancel, cancelAt: 3 + 512}
			registry := prometheus.NewPedanticRegistry()
			if err := registry.Register(&sandboxMetricsContextCollector{sandboxMetricsCollector: collector, ctx: ctx}); err != nil {
				t.Fatal(err)
			}
			assertSandboxMetrics(t, registry, nil, 0)
			if !errors.Is(base.Err(), context.Canceled) || ctx.checks.Load() != ctx.cancelAt || len(source.calls) != 4 {
				t.Fatalf("aggregation did not stop during %s: %d checks, %d reads, %v",
					kind, ctx.checks.Load(), len(source.calls), base.Err())
			}
			for _, call := range source.calls {
				if call.ctx != ctx {
					t.Fatal("aggregation did not share the cache-read context")
				}
			}
			if !reflect.DeepEqual(before, source.objects()) {
				t.Fatal("cancelled aggregation modified cached objects")
			}
		})
	}
}
