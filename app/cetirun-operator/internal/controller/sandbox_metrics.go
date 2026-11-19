// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
)

const (
	sandboxMetricsCollectionTimeout        = 5 * time.Second
	sandboxMetricsInitializationRetryDelay = 5 * time.Second
)

type sandboxMetricLabels struct {
	runtime, status, reason string
}

type sandboxMetricsCollector struct {
	cache                 cache.Cache
	initialized           atomic.Bool
	logger                logr.Logger
	agentsDesc            *prometheus.Desc
	collectionSuccessDesc *prometheus.Desc
}

// RegisterSandboxMetrics registers descriptors without cache reads and arranges
// startup initialization of the manager's informers on every replica.
func RegisterSandboxMetrics(registry prometheus.Registerer, mgr manager.Manager) error {
	if registry == nil {
		return fmt.Errorf("register Sandbox metrics: registry is required")
	}
	if mgr == nil {
		return fmt.Errorf("register Sandbox metrics: manager is required")
	}
	source := mgr.GetCache()
	if source == nil {
		return fmt.Errorf("register Sandbox metrics: manager cache is required")
	}
	collector := newSandboxMetricsCollector(source)
	if err := registry.Register(collector); err != nil {
		return fmt.Errorf("register Sandbox metrics: %w", err)
	}
	if err := mgr.Add(collector); err != nil {
		return fmt.Errorf("register Sandbox metrics cache initializer: %w", err)
	}
	return nil
}

func newSandboxMetricsCollector(source cache.Cache) *sandboxMetricsCollector {
	return &sandboxMetricsCollector{
		cache:  source,
		logger: log.Log.WithName("sandbox-metrics"),
		agentsDesc: prometheus.NewDesc(
			"cetirun_sandbox_agents",
			"Agents with Sandbox intent or retained hold/history, by declared runtime and reported status; not runtime qualification or fresh enforcement evidence. Only populated groups are emitted.",
			[]string{"runtime", "status", "reason"}, nil,
		),
		collectionSuccessDesc: prometheus.NewDesc(
			"cetirun_sandbox_metrics_collection_success",
			"Whether all Sandbox reported-status cache reads completed successfully (1) or fleet counts were omitted (0).",
			nil, nil,
		),
	}
}

func (c *sandboxMetricsCollector) NeedLeaderElection() bool { return false }

func (c *sandboxMetricsCollector) Start(ctx context.Context) error {
	if c.cache == nil {
		return fmt.Errorf("Sandbox metrics manager cache is required")
	}
	defer c.initialized.Store(false)
	for ctx.Err() == nil {
		err := c.initializeCache(ctx)
		if err == nil {
			<-ctx.Done()
			return nil
		}
		if ctx.Err() != nil {
			return nil
		}
		c.logger.Error(err, "Unable to initialize Sandbox metrics cache; retrying",
			"retryAfter", sandboxMetricsInitializationRetryDelay)
		timer := time.NewTimer(sandboxMetricsInitializationRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
	return nil
}

func (c *sandboxMetricsCollector) initializeCache(ctx context.Context) error {
	if c.cache == nil {
		return fmt.Errorf("Sandbox metrics manager cache is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, sandboxMetricsCollectionTimeout)
	defer cancel()
	// Controller watches may never start on a nonleader. Initialize these shared
	// informers here, never via List during collection; the manager owns their
	// subsequent list/watch traffic and lifetime.
	for _, object := range []client.Object{&api.Agent{}, &api.Team{}, &api.Sandbox{}} {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("initialize Sandbox metrics cache: %w", err)
		}
		informer, err := c.cache.GetInformer(ctx, object, cache.BlockUntilSynced(false))
		if err != nil {
			return fmt.Errorf("initialize cached %T for Sandbox metrics: %w", object, err)
		}
		if informer == nil {
			return fmt.Errorf("initialize cached %T for Sandbox metrics: informer is unavailable", object)
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("initialize Sandbox metrics cache: %w", err)
	}
	c.initialized.Store(true)
	return nil
}

func (c *sandboxMetricsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.agentsDesc
	ch <- c.collectionSuccessDesc
}

func (c *sandboxMetricsCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), sandboxMetricsCollectionTimeout)
	defer cancel()
	c.collect(ctx, ch)
}

func (c *sandboxMetricsCollector) collect(ctx context.Context, ch chan<- prometheus.Metric) {
	groups, err := c.collectSnapshot(ctx)
	if err != nil {
		c.logger.Error(err, "Unable to collect Sandbox reported-status metrics")
		ch <- prometheus.MustNewConstMetric(c.collectionSuccessDesc, prometheus.GaugeValue, 0)
		return
	}
	for labels, count := range groups {
		ch <- prometheus.MustNewConstMetric(c.agentsDesc, prometheus.GaugeValue, float64(count),
			labels.runtime, labels.status, labels.reason)
	}
	ch <- prometheus.MustNewConstMetric(c.collectionSuccessDesc, prometheus.GaugeValue, 1)
}

func (c *sandboxMetricsCollector) collectSnapshot(ctx context.Context) (map[sandboxMetricLabels]uint64, error) {
	if c.cache == nil {
		return nil, fmt.Errorf("manager cache is unavailable")
	}
	if !c.initialized.Load() {
		return nil, fmt.Errorf("Sandbox metrics cache initialization is unavailable")
	}
	if !c.cache.WaitForCacheSync(ctx) {
		return nil, fmt.Errorf("manager cache is not synchronized")
	}
	var agents api.AgentList
	var teams api.TeamList
	var sandboxes api.SandboxList
	for _, list := range []client.ObjectList{&agents, &teams, &sandboxes} {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("Sandbox metrics collection deadline: %w", err)
		}
		if err := c.cache.List(ctx, list); err != nil {
			return nil, fmt.Errorf("list cached %T: %w", list, err)
		}
	}

	// These lists are independently cached reports, not an atomic cross-resource
	// observation. Keep all identity lookups and counts local to this scrape.
	teamsByName := make(map[string]*api.Team, len(teams.Items))
	for i := range teams.Items {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("Sandbox metrics collection deadline: %w", err)
		}
		team := &teams.Items[i]
		if team.Namespace == "" {
			teamsByName[team.Name] = team
		}
	}
	sandboxesByName := make(map[string]*api.Sandbox, len(sandboxes.Items))
	for i := range sandboxes.Items {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("Sandbox metrics collection deadline: %w", err)
		}
		box := &sandboxes.Items[i]
		if box.Namespace == "" {
			sandboxesByName[box.Name] = box
		}
	}
	groups := make(map[sandboxMetricLabels]uint64)
	for i := range agents.Items {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("Sandbox metrics collection deadline: %w", err)
		}
		agent := &agents.Items[i]
		// A Team's policy fields imply intent only. They never select a
		// runtime or authorize an otherwise unbound Agent for these metrics.
		if !sandboxRequested(agent, teamsByName[agent.Spec.Team]) {
			continue
		}
		status, reason := sandboxMetricReportedStatus(agent)
		labels := sandboxMetricLabels{
			runtime: sandboxMetricRuntime(agent, sandboxesByName[agent.Spec.SandboxRef]),
			status:  string(status),
			reason:  reason,
		}
		groups[labels]++
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("Sandbox metrics collection deadline: %w", err)
	}
	if !c.initialized.Load() {
		return nil, fmt.Errorf("Sandbox metrics cache initialization is unavailable")
	}
	return groups, nil
}

func sandboxMetricRuntime(agent *api.Agent, box *api.Sandbox) string {
	binding, err := sandboxbinding.AuthorizedBinding(agent)
	if err != nil || box == nil || box.UID == "" || box.UID != binding.SandboxUID ||
		box.Name != agent.Spec.SandboxRef || !box.DeletionTimestamp.IsZero() {
		return "unknown"
	}
	switch box.Spec.Type {
	case api.SandboxKata, api.SandboxGVisor, api.SandboxContainer:
		return string(box.Spec.Type)
	default:
		return "unknown"
	}
}

func sandboxMetricReportedStatus(agent *api.Agent) (metav1.ConditionStatus, string) {
	switch {
	case !agent.DeletionTimestamp.IsZero():
		return metav1.ConditionFalse, "Deleting"
	case agent.Spec.Suspended:
		return metav1.ConditionFalse, "Suspended"
	}

	var ready *metav1.Condition
	for i := range agent.Status.Conditions {
		condition := &agent.Status.Conditions[i]
		if condition.Type == api.ConditionSandboxReady {
			if ready != nil {
				return metav1.ConditionUnknown, "ConditionMalformed"
			}
			ready = condition
		}
	}
	if ready == nil {
		return metav1.ConditionUnknown, "ConditionMissing"
	}
	if len(metav1validation.ValidateCondition(*ready, field.NewPath("status", "conditions"))) != 0 {
		return metav1.ConditionUnknown, "ConditionMalformed"
	}
	if agent.Generation <= 0 || ready.ObservedGeneration != agent.Generation {
		return metav1.ConditionUnknown, "ConditionStale"
	}
	// LastTransitionTime describes a condition transition, not an enforcement
	// observation clock. Applied binding history is not consulted for readiness.
	return ready.Status, sandboxMetricReason(ready.Reason)
}

func sandboxMetricReason(reason string) string {
	switch reason {
	case "PolicyApplied", "ObservationUnavailable", "EnforcementPending", "EnforcementLost",
		"RuntimeUnavailable", "UnsupportedControl", "ReferenceMissing", "UnauthorizedBinding",
		"PolicyConflict", "PolicyApplyFailed", "SandboxPreparationPending", "SandboxPrepared",
		"Suspended", "Deleting", "InstanceDeleting":
		return reason
	default:
		return "Other"
	}
}
