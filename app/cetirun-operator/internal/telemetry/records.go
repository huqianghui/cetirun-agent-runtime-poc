// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package telemetry turns CetiRun custom resources and Kubernetes events into
// JSON records, one per line, for a log collector such as ADX-Mon. The
// records come from the API server through the telemetry program's own
// read-only access, so a query can trust them more than workload output: they
// say which namespace belongs to which Agent, owner and Team, which Pods do
// not carry.
//
// Records hold only the fields below. An Agent's Helm values, model settings,
// Secrets and status details stay out.
package telemetry

import (
	"strings"
	"time"
	"unicode/utf8"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Log types of the records. Queries select records by log_type.
const (
	LogTypeEvent       = "cetirun.k8s.event"
	LogTypeAgent       = "cetirun.inventory.agent"
	LogTypeTeam        = "cetirun.inventory.team"
	LogTypeAgentPolicy = "cetirun.inventory.agentpolicy"
	LogTypeSandbox     = "cetirun.inventory.sandbox"

	// SchemaVersion changes when a field changes meaning or goes away.
	SchemaVersion = 1
)

// Actions of inventory records.
const (
	// ActionUpsert records an object that was created or changed.
	ActionUpsert = "upsert"
	// ActionDelete records an object that was deleted.
	ActionDelete = "delete"
	// ActionSnapshot repeats every object periodically, so the latest state
	// stays within the retention of the logs.
	ActionSnapshot = "snapshot"
)

// MaxTextLength bounds free text, such as an event message, in characters.
const MaxTextLength = 2048

// Record is one line of output.
type Record struct {
	LogType       string             `json:"log_type"`
	SchemaVersion int                `json:"schema_version"`
	Time          string             `json:"time"`
	Action        string             `json:"action,omitempty"`
	Event         *EventRecord       `json:"event,omitempty"`
	Agent         *AgentRecord       `json:"agent,omitempty"`
	Team          *TeamRecord        `json:"team,omitempty"`
	AgentPolicy   *AgentPolicyRecord `json:"agent_policy,omitempty"`
	Sandbox       *SandboxRecord     `json:"sandbox,omitempty"`
}

// ObjectReference names the object an event is about.
type ObjectReference struct {
	APIVersion string `json:"api_version,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name,omitempty"`
	UID        string `json:"uid,omitempty"`
}

// EventRecord is a Kubernetes event. A recurring event is written again with
// its new count; uid and resource_version identify a copy.
type EventRecord struct {
	UID             string          `json:"uid"`
	Namespace       string          `json:"namespace,omitempty"`
	Name            string          `json:"name"`
	ResourceVersion string          `json:"resource_version,omitempty"`
	Type            string          `json:"type,omitempty"`
	Reason          string          `json:"reason,omitempty"`
	Message         string          `json:"message,omitempty"`
	Action          string          `json:"action,omitempty"`
	Count           int32           `json:"count,omitempty"`
	FirstTime       string          `json:"first_time,omitempty"`
	LastTime        string          `json:"last_time,omitempty"`
	Source          string          `json:"source,omitempty"`
	Object          ObjectReference `json:"object"`
}

// Owner is the user an Agent belongs to, as the Agent records it.
type Owner struct {
	Alias       string `json:"alias,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Email       string `json:"email,omitempty"`
	TenantID    string `json:"tenant_id,omitempty"`
	ObjectID    string `json:"object_id,omitempty"`
}

// AgentRecord maps an Agent to its namespace, owner, Team and policy.
type AgentRecord struct {
	Name      string `json:"name"`
	UID       string `json:"uid"`
	Namespace string `json:"namespace"`
	Type      string `json:"type,omitempty"`
	Team      string `json:"team,omitempty"`
	Sandbox   string `json:"sandbox,omitempty"`
	Policy    string `json:"policy,omitempty"`
	Phase     string `json:"phase,omitempty"`
	Suspended bool   `json:"suspended,omitempty"`
	Owner     Owner  `json:"owner"`
	Created   string `json:"created,omitempty"`
	Deleting  bool   `json:"deleting,omitempty"`
}

// TeamRecord maps a Team to its AgentPolicy.
type TeamRecord struct {
	Name        string `json:"name"`
	UID         string `json:"uid"`
	AgentPolicy string `json:"agent_policy,omitempty"`
	Role        string `json:"role,omitempty"`
	Created     string `json:"created,omitempty"`
}

// AgentPolicyRecord identifies an AgentPolicy revision.
type AgentPolicyRecord struct {
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Generation int64  `json:"generation,omitempty"`
	Accepted   string `json:"accepted,omitempty"`
	Created    string `json:"created,omitempty"`
}

// SandboxRecord identifies a Sandbox and its runtime.
type SandboxRecord struct {
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Type       string `json:"type,omitempty"`
	Generation int64  `json:"generation,omitempty"`
	Ready      string `json:"ready,omitempty"`
	Created    string `json:"created,omitempty"`
}

// AgentNamespace is the namespace of an Agent's workload: the one its status
// names, or the operator's naming rule while it has none.
func AgentNamespace(agent *api.Agent) string {
	if agent.Status.Namespace != "" {
		return agent.Status.Namespace
	}
	return "cetirun-" + agent.Name
}

// AgentRecordOf returns the inventory record of an Agent.
func AgentRecordOf(agent *api.Agent) *AgentRecord {
	record := &AgentRecord{
		Name:      agent.Name,
		UID:       string(agent.UID),
		Namespace: AgentNamespace(agent),
		Type:      string(agent.Spec.Agent.Type),
		Team:      agent.Spec.Team,
		Sandbox:   agent.Spec.SandboxRef,
		Phase:     agent.Status.Phase,
		Suspended: agent.Spec.Suspended,
		Owner: Owner{
			Alias:       agent.Spec.User.Alias,
			DisplayName: Clip(agent.Spec.User.DisplayName, 256),
			Email:       agent.Spec.User.Email,
			TenantID:    agent.Annotations[api.AnnotationOwnerTenantID],
			ObjectID:    agent.Annotations[api.AnnotationOwnerObjectID],
		},
		Created:  timestamp(agent.CreationTimestamp),
		Deleting: agent.DeletionTimestamp != nil,
	}
	if record.Team == "" {
		record.Team = agent.Spec.User.Team
	}
	if policy := agent.Status.Policy; policy != nil && policy.EffectivePolicyRef != nil {
		record.Policy = policy.EffectivePolicyRef.Name
	}
	return record
}

// TeamRecordOf returns the inventory record of a Team.
func TeamRecordOf(team *api.Team) *TeamRecord {
	return &TeamRecord{
		Name:        team.Name,
		UID:         string(team.UID),
		AgentPolicy: team.Spec.AgentPolicyRef,
		Role:        Clip(team.Spec.MemberContext.Role, 256),
		Created:     timestamp(team.CreationTimestamp),
	}
}

// AgentPolicyRecordOf returns the inventory record of an AgentPolicy.
func AgentPolicyRecordOf(policy *api.AgentPolicy) *AgentPolicyRecord {
	return &AgentPolicyRecord{
		Name:       policy.Name,
		UID:        string(policy.UID),
		Generation: policy.Generation,
		Accepted:   conditionStatus(policy.Status.Conditions, "Accepted"),
		Created:    timestamp(policy.CreationTimestamp),
	}
}

// SandboxRecordOf returns the inventory record of a Sandbox.
func SandboxRecordOf(sandbox *api.Sandbox) *SandboxRecord {
	return &SandboxRecord{
		Name:       sandbox.Name,
		UID:        string(sandbox.UID),
		Type:       string(sandbox.Spec.Type),
		Generation: sandbox.Generation,
		Ready:      conditionStatus(sandbox.Status.Conditions, "Ready"),
		Created:    timestamp(sandbox.CreationTimestamp),
	}
}

// EventRecordOf returns the record of a Kubernetes event.
func EventRecordOf(event *corev1.Event) *EventRecord {
	record := &EventRecord{
		UID:             string(event.UID),
		Namespace:       event.Namespace,
		Name:            event.Name,
		ResourceVersion: event.ResourceVersion,
		Type:            event.Type,
		Reason:          Clip(event.Reason, 256),
		Message:         Clip(event.Message, MaxTextLength),
		Action:          Clip(event.Action, 256),
		Count:           event.Count,
		FirstTime:       timestamp(event.FirstTimestamp),
		LastTime:        timestamp(event.LastTimestamp),
		Source:          event.ReportingController,
		Object: ObjectReference{
			APIVersion: event.InvolvedObject.APIVersion,
			Kind:       event.InvolvedObject.Kind,
			Namespace:  event.InvolvedObject.Namespace,
			Name:       event.InvolvedObject.Name,
			UID:        string(event.InvolvedObject.UID),
		},
	}
	if record.Source == "" {
		record.Source = event.Source.Component
	}
	if series := event.Series; series != nil && series.Count > record.Count {
		record.Count = series.Count
		if !series.LastObservedTime.IsZero() {
			record.LastTime = series.LastObservedTime.UTC().Format(time.RFC3339Nano)
		}
	}
	if record.FirstTime == "" && !event.EventTime.IsZero() {
		record.FirstTime = event.EventTime.UTC().Format(time.RFC3339Nano)
	}
	if record.LastTime == "" {
		record.LastTime = record.FirstTime
	}
	return record
}

// EventFilter selects the events that are written.
type EventFilter string

const (
	// EventsAll writes every event of the cluster.
	EventsAll EventFilter = "all"
	// EventsWarningsAndCetiRun writes warnings, and every event about a
	// CetiRun resource or in a namespace whose name starts with cetirun-:
	// the platform's and the Agents'.
	EventsWarningsAndCetiRun EventFilter = "warnings-and-cetirun"
	// EventsNone writes no events.
	EventsNone EventFilter = "none"
)

// Valid reports whether the filter is one of the known ones.
func (f EventFilter) Valid() bool {
	return f == EventsAll || f == EventsWarningsAndCetiRun || f == EventsNone
}

// Keep reports whether the event passes the filter.
func (f EventFilter) Keep(event *corev1.Event) bool {
	switch f {
	case EventsAll:
		return true
	case EventsWarningsAndCetiRun:
		if event.Type == corev1.EventTypeWarning || strings.HasPrefix(event.Namespace, "cetirun-") ||
			strings.HasPrefix(event.InvolvedObject.Namespace, "cetirun-") {
			return true
		}
		group, err := schema.ParseGroupVersion(event.InvolvedObject.APIVersion)
		return err == nil && group.Group == api.GroupVersion.Group
	default:
		return false
	}
}

// Clip shortens text to at most limit characters, at a character boundary.
func Clip(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := 0
	for index := range text {
		if runes == limit {
			return text[:index]
		}
		runes++
	}
	return text
}

func timestamp(value metav1.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func conditionStatus(conditions []metav1.Condition, kind string) string {
	if condition := meta.FindStatusCondition(conditions, kind); condition != nil {
		return string(condition.Status)
	}
	return ""
}
