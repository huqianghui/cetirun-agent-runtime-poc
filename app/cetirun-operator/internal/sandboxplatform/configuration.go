// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxplatform

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/types"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
)

// ErrSourceChanged indicates that a captured source identity has changed.
var ErrSourceChanged = errors.New("Sandbox source identity changed")

type RuntimePrerequisite struct {
	UID     types.UID
	Handler string
}

type StoragePrerequisite struct {
	UID         types.UID
	Provisioner string
}

// Snapshot is trusted platform support inventory, not CR status. Empty or stale
// snapshots never imply readiness. It provides neither writer fencing,
// enforcement evidence nor start authorization.
type Snapshot struct {
	ObservedAt     time.Time
	Registrations  map[api.SandboxType]sandbox.Registration
	Platforms      map[api.SandboxType]sandboxpolicy.Platform
	RuntimeClasses map[string]RuntimePrerequisite
	StorageClasses map[string]StoragePrerequisite
}

type Source interface {
	// Snapshot returns an immutable, atomically captured support inventory.
	// Grant approvals bind role UID, resourceVersion and normalized rules.
	// Policy assessment and workload consumers must authoritatively verify only
	// their requested grants before use; unrelated roles do not gate capture.
	Snapshot(context.Context) (Snapshot, error)
}

// CheckFreshness rejects missing, future-dated or expired observations.
// Nonpositive maxAge values use a 30-second lifetime; the expiry bound is strict.
func CheckFreshness(observedAt, now time.Time, maxAge time.Duration) error {
	if maxAge <= 0 {
		maxAge = 30 * time.Second
	}
	if now.IsZero() || observedAt.IsZero() || observedAt.After(now) || now.Sub(observedAt) >= maxAge {
		return fmt.Errorf("%w: platform snapshot is stale or incomplete", sandboxpolicy.ErrUnavailable)
	}
	return nil
}
