// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package skillmarketplace is the operator's part of the Skill Marketplace.
// The operator reconciles the Teams and Agents: it lists the policy-bound
// Teams and their marketplace selections for the publisher, and it gives each
// Agent of such a Team a read-only claim of the release storage.
//
// The publisher is a separate process of the operator image
// (cmd/skill-marketplace). It reads the Git repository, writes the Team
// releases to the storage and updates the catalog. It has no access to Teams,
// Agents, claims or volumes: its input is the Teams document, which this
// package writes.
package skillmarketplace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	strictjson "sigs.k8s.io/json"
)

const (
	// TeamsConfigMap holds the Teams document, in the operator namespace.
	TeamsConfigMap = "cetirun-skill-marketplace-teams"
	// TeamsKey holds a JSON TeamsDocument.
	TeamsKey = "teams.json"
	// TeamsVersion is the Teams document version.
	TeamsVersion = "v1"
	// MaxTeamsBytes keeps the Teams document well below the ConfigMap limit.
	MaxTeamsBytes = 900 << 10

	managedBy = "cetirun-operator"
	// resync is how often the operator writes the documents again without a
	// Team change, for example after someone deleted one.
	resync = 10 * time.Minute
)

// TeamsDocument lists the Teams whose selections the publisher publishes.
type TeamsDocument struct {
	// Version is TeamsVersion.
	Version string `json:"version"`
	// Teams are the policy-bound Teams that are not being deleted, sorted by
	// name. The publisher removes the releases of a Team that leaves the list.
	Teams []TeamSelection `json:"teams"`
}

// TeamSelection is one policy-bound Team and its marketplace selection.
type TeamSelection struct {
	Name string `json:"name"`
	UID  string `json:"uid"`
	// Skills is api.SkillMarketplaceSelection of the Team's spec.skills.
	Skills []string `json:"skills,omitempty"`
}

// TeamsFor returns the Teams document of the Teams.
func TeamsFor(teams []api.Team) TeamsDocument {
	document := TeamsDocument{Version: TeamsVersion, Teams: []TeamSelection{}}
	for i := range teams {
		team := &teams[i]
		if team.Spec.AgentPolicyRef == "" || !team.DeletionTimestamp.IsZero() {
			continue
		}
		selection := TeamSelection{Name: team.Name, UID: string(team.UID)}
		if skills := api.SkillMarketplaceSelection(team.Spec.Skills); len(skills) > 0 {
			selection.Skills = skills
		}
		document.Teams = append(document.Teams, selection)
	}
	sort.Slice(document.Teams, func(i, j int) bool { return document.Teams[i].Name < document.Teams[j].Name })
	return document
}

// DecodeTeams reads a Teams document: one JSON object of the supported
// version, with case-sensitive field names and no unknown or duplicate fields.
func DecodeTeams(raw string) (TeamsDocument, error) {
	var document TeamsDocument
	if len(raw) > MaxTeamsBytes {
		return document, fmt.Errorf("the Teams document is longer than %d bytes", MaxTeamsBytes)
	}
	strict, err := strictjson.UnmarshalStrict([]byte(raw), &document, strictjson.DisallowDuplicateFields, strictjson.DisallowUnknownFields)
	if err == nil {
		err = errors.Join(strict...)
	}
	if err != nil {
		return TeamsDocument{}, fmt.Errorf("decode the Teams document: %w", err)
	}
	if document.Version != TeamsVersion || document.Teams == nil {
		return TeamsDocument{}, fmt.Errorf("the Teams document has version %q, not %q", document.Version, TeamsVersion)
	}
	for _, team := range document.Teams {
		if team.Name == "" || team.UID == "" {
			return TeamsDocument{}, errors.New("the Teams document has a Team without a name or UID")
		}
	}
	return document, nil
}

// TeamsReconciler writes the Teams document, and creates the empty catalog
// ConfigMap that the publisher updates. The publisher's permissions name both
// ConfigMaps, and it cannot create ConfigMaps.
type TeamsReconciler struct {
	// Client lists Teams from the cache and writes the ConfigMaps.
	Client client.Client
	// APIReader reads the ConfigMaps, which the operator does not cache.
	APIReader client.Reader
	// Namespace is the operator namespace.
	Namespace string
}

// Reconcile writes the Teams document of every Team.
func (r *TeamsReconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	teams := &api.TeamList{}
	if err := r.Client.List(ctx, teams); err != nil {
		return reconcile.Result{}, err
	}
	raw, err := json.Marshal(TeamsFor(teams.Items))
	if err != nil {
		return reconcile.Result{}, err
	}
	if len(raw) > MaxTeamsBytes {
		return reconcile.Result{}, fmt.Errorf("the Skill Marketplace Teams document needs %d bytes, more than %d: "+
			"select fewer skills on the policy-bound Teams", len(raw), MaxTeamsBytes)
	}
	if err := r.write(ctx, TeamsConfigMap, map[string]string{TeamsKey: string(raw)}, true); err != nil {
		return reconcile.Result{}, fmt.Errorf("write the Skill Marketplace Teams document: %w", err)
	}
	if err := r.write(ctx, api.SkillMarketplaceCatalogConfigMap, nil, false); err != nil {
		return reconcile.Result{}, fmt.Errorf("create the Skill Marketplace catalog: %w", err)
	}
	return reconcile.Result{RequeueAfter: resync}, nil
}

// write creates a ConfigMap of the operator namespace with data, or, when
// replace is set, gives an existing one that data.
func (r *TeamsReconciler) write(ctx context.Context, name string, data map[string]string, replace bool) error {
	key := client.ObjectKey{Namespace: r.Namespace, Name: name}
	existing := &corev1.ConfigMap{}
	err := r.APIReader.Get(ctx, key, existing)
	switch {
	case apierrors.IsNotFound(err):
		created := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.Namespace,
				Labels: map[string]string{"app.kubernetes.io/managed-by": managedBy}},
			Data: data,
		}
		return client.IgnoreAlreadyExists(r.Client.Create(ctx, created))
	case err != nil:
		return err
	case !replace || (maps.Equal(existing.Data, data) && len(existing.BinaryData) == 0):
		return nil
	}
	existing.Data, existing.BinaryData = data, nil
	return r.Client.Update(ctx, existing)
}

// SetupWithManager writes the documents when the operator starts, after each
// Team change, and every ten minutes.
func (r *TeamsReconciler) SetupWithManager(mgr ctrl.Manager) error {
	one := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: r.Namespace, Name: TeamsConfigMap}}}
	})
	start := make(chan event.GenericEvent, 1)
	start <- event.GenericEvent{Object: &corev1.ConfigMap{}}
	return ctrl.NewControllerManagedBy(mgr).
		Named("skill-marketplace-teams").
		Watches(&api.Team{}, one).
		WatchesRawSource(source.Channel(start, one)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}
