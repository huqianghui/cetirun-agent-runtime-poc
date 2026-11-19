// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/aigateway"
)

const (
	// aiGatewayRolloutRecheck is how soon the publisher lists the Gateway
	// again while a catalog change waits for a rollout.
	aiGatewayRolloutRecheck = 15 * time.Second
	// aiGatewayRolloutHoldLimit bounds the wait: a rollout seen for longer
	// no longer holds catalog changes back.
	aiGatewayRolloutHoldLimit = 10 * time.Minute
	// aiGatewayRolloutCheckReuse lets the reconciles that list the Gateway
	// at about the same time share one look at its pods.
	aiGatewayRolloutCheckReuse = 5 * time.Second
)

// aiGatewayRollout remembers a Gateway rollout during which listings that
// change the catalog are held back.
//
// The operator lists the catalog through the Gateway Service. While the
// Gateway's pods are being replaced, consecutive listings come from old and
// new pods, so a model that the new configuration adds, removes or renames
// appears and disappears from one listing to the next. Adopting each listing
// changed the access groups of every Agent key and the model lists of the
// Agents, which restarted them several times during one rollout. Holding the
// previous view until a single pod template serves the Gateway applies the
// change once.
type aiGatewayRollout struct {
	mu sync.Mutex
	// since is when the current rollout was first seen; zero when none is.
	since         time.Time
	held          bool
	limitReported bool
	// unreadable is set once a failure to look at the Gateway's pods has
	// been logged, until a look succeeds again.
	unreadable bool
	checkedAt  time.Time
	rolling    bool
	checkErr   error
}

// waiting reports whether a rollout is being followed and has not reached
// the hold limit, so the publisher should list the Gateway again soon.
func (h *aiGatewayRollout) waiting() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.since.IsZero() && !h.limitReported
}

// aiGatewayServiceKey returns the Service a Gateway URL of the form
// http://<service>.<namespace>.svc[.cluster.local]:<port> names.
func aiGatewayServiceKey(raw string) (types.NamespacedName, error) {
	endpoint, err := url.Parse(raw)
	if err != nil {
		return types.NamespacedName{}, fmt.Errorf("the AI Gateway URL cannot be parsed")
	}
	parts := strings.Split(endpoint.Hostname(), ".")
	inCluster := len(parts) == 3 || (len(parts) == 5 && parts[3] == "cluster" && parts[4] == "local")
	if !inCluster || parts[0] == "" || parts[1] == "" || parts[2] != "svc" {
		return types.NamespacedName{}, fmt.Errorf("the AI Gateway URL does not name an in-cluster Service")
	}
	return types.NamespacedName{Namespace: parts[1], Name: parts[0]}, nil
}

// aiGatewayRollingOut reports whether the Gateway Service sends requests to
// pods of more than one pod template: the ready pods it selects that are not
// terminating carry different pod-template-hash labels (Deployments) or,
// without those, different controller-revision-hash labels (StatefulSets and
// DaemonSets).
func (r *AgentReconciler) aiGatewayRollingOut(ctx context.Context) (bool, error) {
	key, err := aiGatewayServiceKey(r.AIGateway.URL)
	if err != nil {
		return false, err
	}
	var reader client.Reader = r.Client
	if r.APIReader != nil {
		// Read directly: a cached read would start a Pod informer.
		reader = r.APIReader
	}
	service := &corev1.Service{}
	if err := reader.Get(ctx, key, service); err != nil {
		return false, fmt.Errorf("read the AI Gateway Service %s: %w", key, err)
	}
	if len(service.Spec.Selector) == 0 {
		return false, fmt.Errorf("the AI Gateway Service %s selects no pods", key)
	}
	pods := &corev1.PodList{}
	if err := reader.List(ctx, pods, client.InNamespace(key.Namespace), client.MatchingLabels(service.Spec.Selector)); err != nil {
		return false, fmt.Errorf("list the AI Gateway pods: %w", err)
	}
	templates := map[string]bool{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil || !aiGatewayPodReady(pod) {
			continue
		}
		template := pod.Labels["pod-template-hash"]
		if template == "" {
			template = pod.Labels["controller-revision-hash"]
		}
		templates[template] = true
	}
	return len(templates) > 1, nil
}

func aiGatewayPodReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// holdAIGatewayCatalog reports whether a listing must not replace the
// catalog view yet: it would change a view taken from an earlier listing
// while the Gateway is rolling out, for less than the hold limit. Without an
// earlier view there is nothing to keep, and when the Gateway's pods cannot
// be read the listing is adopted, as before rollouts were detected. Call it
// with the catalog's refreshMu held.
func (r *AgentReconciler) holdAIGatewayCatalog(ctx context.Context, deployments []aigateway.Deployment, blockUnpriced bool, now time.Time) bool {
	catalog := &r.aiGatewayCatalog
	changes := catalog.changedBy(deployments, blockUnpriced)
	hold := &catalog.rollout
	hold.mu.Lock()
	defer hold.mu.Unlock()
	if !changes && hold.since.IsZero() {
		hold.held = false
		return false
	}
	logger := log.FromContext(ctx).WithName("ai-gateway-catalog")
	if hold.checkedAt.IsZero() || now.Sub(hold.checkedAt) >= aiGatewayRolloutCheckReuse || now.Before(hold.checkedAt) {
		hold.rolling, hold.checkErr = r.aiGatewayRollingOut(ctx)
		hold.checkedAt = now
	}
	switch {
	case hold.checkErr != nil:
		if !hold.unreadable {
			logger.Info("cannot tell whether the AI Gateway is rolling out; catalog changes are applied without waiting for it",
				"error", hold.checkErr.Error())
			hold.unreadable = true
		}
		hold.since, hold.held, hold.limitReported = time.Time{}, false, false
		return false
	case !hold.rolling:
		if hold.held {
			logger.Info("the AI Gateway rollout is over; applying the current catalog", "waited", now.Sub(hold.since).Round(time.Second).String())
		}
		hold.unreadable = false
		hold.since, hold.held, hold.limitReported = time.Time{}, false, false
		return false
	}
	hold.unreadable = false
	if hold.since.IsZero() {
		hold.since = now
	}
	if !changes {
		hold.held = false
		return false
	}
	if now.Sub(hold.since) >= aiGatewayRolloutHoldLimit {
		if !hold.limitReported {
			logger.Info("the AI Gateway has been rolling out for longer than the hold limit; applying catalog changes without waiting",
				"limit", aiGatewayRolloutHoldLimit.String())
			hold.limitReported = true
		}
		hold.held = false
		return false
	}
	if !hold.held {
		logger.Info("the AI Gateway is rolling out; catalog changes wait until one pod template serves it",
			"limit", aiGatewayRolloutHoldLimit.String())
	}
	hold.held = true
	return true
}
