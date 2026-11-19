// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package sandboxbinding reads admission-owned authorization, never enforcement
// status. These records are trustworthy only with the configuration admission
// ownership checks enforced on every Agent create and main-resource update.
package sandboxbinding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	AuthorizationAnnotation = api.AnnotationSandboxAuthorization
	ReauthorizeAnnotation   = api.AnnotationSandboxReauthorize
	recordVersion           = "v1"
)

type reference struct {
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
}

// The record belongs to its containing Agent. The API server may assign both
// name and UID after mutation (including generateName), so neither is copied
// from the incoming create request. Admission forbids transplanting the record.
type authorization struct {
	Version     string    `json:"version"`
	Team        reference `json:"team"`
	Sandbox     reference `json:"sandbox"`
	AgentPolicy reference `json:"agentPolicy"`
}

// Reauthorization is explicit administrator intent, not an authorization record.
// The expected binding prevents an operation prepared for one UID from approving
// a same-name replacement. RequestID must be fresh for each operation.
type Reauthorization struct {
	RequestID string          `json:"requestID"`
	Binding   sandbox.Binding `json:"binding"`
}

// Present detects protected metadata, including malformed or empty records. It
// must not be used as evidence that a binding is valid or enforcement is active.
func Present(agent *api.Agent) bool {
	if agent == nil {
		return false
	}
	_, authorized := agent.Annotations[AuthorizationAnnotation]
	_, requested := agent.Annotations[ReauthorizeAnnotation]
	return authorized || requested
}

// Snapshot constructs a canonical record from already-read live references.
// Admission must separately validate complete Team compatibility and caller
// authority before persisting it atomically on the Agent.
func Snapshot(agent *api.Agent, team *api.Team, resources sandbox.Resources) (string, error) {
	record, err := current(agent, team, resources)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(record)
	return string(data), err
}

// CurrentBinding returns the current reference UIDs for an existing Agent. It
// does not authorize them; compare with an explicit request or AuthorizedBinding.
func CurrentBinding(agent *api.Agent, team *api.Team, resources sandbox.Resources) (sandbox.Binding, error) {
	record, err := current(agent, team, resources)
	if err != nil {
		return sandbox.Binding{}, err
	}
	if !persistedAgent(agent) {
		return sandbox.Binding{}, fmt.Errorf("authorization requires a persisted, non-terminating Agent")
	}
	return record.binding(agent.UID), nil
}

// AuthorizedBinding returns only the durable admission-owned snapshot. It never
// refreshes references by name or consults status.sandbox. Pass it to
// sandbox.Resolve with freshly read resources; Resolve rejects UID replacement.
func AuthorizedBinding(agent *api.Agent) (sandbox.Binding, error) {
	if !persistedAgent(agent) {
		return sandbox.Binding{}, fmt.Errorf("%w: authorization requires a persisted, non-terminating Agent", sandbox.ErrUnauthorizedBinding)
	}
	raw, found := agent.Annotations[AuthorizationAnnotation]
	if !found || raw == "" {
		return sandbox.Binding{}, fmt.Errorf("%w: Agent has no protected authorization record", sandbox.ErrUnauthorizedBinding)
	}
	var record authorization
	if err := decode([]byte(raw), &record); err != nil {
		return sandbox.Binding{}, fmt.Errorf("%w: invalid authorization record: %v", sandbox.ErrUnauthorizedBinding, err)
	}
	canonical, err := json.Marshal(record)
	if err != nil || !bytes.Equal(canonical, []byte(raw)) || record.Version != recordVersion ||
		!validReference(record.Team) || !validReference(record.Sandbox) || !validReference(record.AgentPolicy) {
		return sandbox.Binding{}, fmt.Errorf("%w: authorization record is not a canonical supported snapshot", sandbox.ErrUnauthorizedBinding)
	}
	if record.Team.Name != agent.Spec.Team || record.Sandbox.Name != agent.Spec.SandboxRef {
		return sandbox.Binding{}, fmt.Errorf("%w: Agent selection differs from its protected authorization", sandbox.ErrUnauthorizedBinding)
	}
	return record.binding(agent.UID), nil
}

// Read uses the supplied authoritative (uncached) reader to fetch the Agent and
// its authorization together. Reference and enforcement checks remain the
// caller's responsibility; a successful read must never release an execution hold.
func Read(ctx context.Context, reader client.Reader, key client.ObjectKey) (*api.Agent, sandbox.Binding, error) {
	if reader == nil || key.Namespace != "" || len(validation.IsDNS1123Subdomain(key.Name)) != 0 {
		return nil, sandbox.Binding{}, fmt.Errorf("authorization requires an authoritative reader and one cluster-scoped Agent name")
	}
	agent := &api.Agent{}
	if err := reader.Get(ctx, key, agent); err != nil {
		return nil, sandbox.Binding{}, err
	}
	binding, err := AuthorizedBinding(agent)
	return agent, binding, err
}

func FormatReauthorization(requestID string, binding sandbox.Binding) (string, error) {
	request := Reauthorization{RequestID: requestID, Binding: binding}
	if err := request.validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(request)
	return string(data), err
}

func ParseReauthorization(raw string) (Reauthorization, error) {
	var request Reauthorization
	if err := decode([]byte(raw), &request); err != nil {
		return request, fmt.Errorf("invalid explicit reauthorization request: %w", err)
	}
	return request, request.validate()
}

func (r Reauthorization) validate() error {
	if !exact(r.RequestID) || len(r.RequestID) > 128 ||
		!exact(string(r.Binding.AgentUID)) || !exact(string(r.Binding.TeamUID)) ||
		!exact(string(r.Binding.SandboxUID)) || !exact(string(r.Binding.AgentPolicyUID)) {
		return fmt.Errorf("explicit reauthorization requires a nonempty requestID and exact Agent, Team, Sandbox and AgentPolicy UIDs")
	}
	return nil
}

func current(agent *api.Agent, team *api.Team, resources sandbox.Resources) (authorization, error) {
	if agent == nil || team == nil || team.Name != agent.Spec.Team || !live(team) ||
		!slices.Contains(team.Spec.SandboxRefs, agent.Spec.SandboxRef) {
		return authorization{}, fmt.Errorf("authorization requires the selected live Team and permitted Sandbox")
	}
	policyName := team.Spec.AgentPolicyRef
	box, policy := resources.Sandboxes[agent.Spec.SandboxRef], resources.AgentPolicies[policyName]
	if box == nil || policy == nil || policyName == "" ||
		box.Name != agent.Spec.SandboxRef || policy.Name != policyName || !live(box) || !live(policy) {
		return authorization{}, fmt.Errorf("authorization requires the selected live Sandbox and AgentPolicy")
	}
	return authorization{
		Version: recordVersion, Team: reference{team.Name, team.UID}, Sandbox: reference{box.Name, box.UID},
		AgentPolicy: reference{policy.Name, policy.UID},
	}, nil
}

func (a authorization) binding(agentUID types.UID) sandbox.Binding {
	return sandbox.Binding{
		AgentUID: agentUID, TeamUID: a.Team.UID, SandboxUID: a.Sandbox.UID,
		AgentPolicyUID: a.AgentPolicy.UID,
	}
}

func persistedAgent(agent *api.Agent) bool {
	return agent != nil && agent.Namespace == "" && live(agent)
}

func live(object client.Object) bool {
	return object.GetNamespace() == "" && validReference(reference{object.GetName(), object.GetUID()}) && object.GetDeletionTimestamp().IsZero()
}

func validReference(ref reference) bool {
	return len(validation.IsDNS1123Subdomain(ref.Name)) == 0 && exact(string(ref.UID))
}

func exact(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "*\r\n\t")
}

func decode(raw []byte, object any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(object); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("expected exactly one JSON object")
	}
	return nil
}
