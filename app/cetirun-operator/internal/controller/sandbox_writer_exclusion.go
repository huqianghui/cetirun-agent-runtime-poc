// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"slices"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxwriter"
	"k8s.io/apimachinery/pkg/util/validation"
)

// JournalSandboxWriterExclusion adapts historical preparation identities to the
// durable journal. It requires explicit enrollment and an independently
// qualified provider; it is deliberately not installed by default and grants
// no execution permission. Current policy, namespace and placement inventory
// are never consulted to retarget an old lifetime.
type JournalSandboxWriterExclusion struct {
	Manager *sandboxwriter.Manager
}

var _ SandboxWriterExclusionSource = (*JournalSandboxWriterExclusion)(nil)

func (s *JournalSandboxWriterExclusion) CheckQuiesced(ctx context.Context, agent *api.Agent, prerequisites SandboxPrerequisites) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.Manager == nil {
		return fmt.Errorf("%w: durable writer-exclusion manager is required", sandbox.ErrObservationUnavailable)
	}
	if agent == nil || agent.Namespace != "" || len(validation.IsDNS1123Subdomain(agent.Name)) != 0 ||
		len(validation.IsDNS1123Label(instanceNamespace(agent.Name))) != 0 ||
		agent.UID == "" || agent.Generation < 1 || prerequisites.AgentUID != agent.UID ||
		prerequisites.AgentGeneration < 1 || prerequisites.AgentGeneration > agent.Generation ||
		prerequisites.NamespaceUID == "" || prerequisites.Home.UID == "" ||
		len(validation.IsDNS1123Subdomain(prerequisites.Home.Name)) != 0 {
		return fmt.Errorf("%w: exact historical Agent, namespace and HOME identities are required", sandbox.ErrObservationUnavailable)
	}
	prerequisites = copySandboxPrerequisites(prerequisites)
	if err := validateSandboxPlacementTuple(agent, prerequisites); err != nil {
		return fmt.Errorf("%w: %w", sandbox.ErrObservationUnavailable, err)
	}
	lifetime := sandboxwriter.Lifetime{
		Agent:     sandboxworkload.ObjectIdentity{Name: agent.Name, UID: agent.UID},
		Namespace: sandboxworkload.ObjectIdentity{Name: instanceNamespace(agent.Name), UID: prerequisites.NamespaceUID},
		Home:      prerequisites.Home,
	}
	manager := *s.Manager
	pinned, err := manager.Store.Read(ctx, lifetime)
	if err != nil {
		return fmt.Errorf("%w: %w", sandbox.ErrObservationUnavailable, err)
	}
	if len(pinned.Writers) != 0 && prerequisites.Placement == nil {
		return fmt.Errorf("%w: nonempty writer history requires recorded lifetime placement", sandbox.ErrObservationUnavailable)
	}
	for _, writer := range pinned.Writers {
		if writer.Workload != prerequisites.Placement.Workload || !slices.Contains(prerequisites.Placement.Nodes, writer.Node) {
			return fmt.Errorf("%w: historical writer is outside the supplied lifetime workload/Node placement", sandbox.ErrObservationUnavailable)
		}
	}
	if _, err := manager.Quiesce(ctx, pinned); err != nil {
		return fmt.Errorf("%w: %w", sandbox.ErrObservationUnavailable, err)
	}
	return ctx.Err()
}
