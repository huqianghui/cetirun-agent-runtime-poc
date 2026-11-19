// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"sync"
	"sync/atomic"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
)

const completedRevisionLabel = "cetirun.microsoft.com/completed-helm-revision"

type releaseWriteState struct {
	mu                sync.Mutex
	seen              atomic.Int64
	completedRevision int
	store             *storage.Storage
}

func (c *Client) releaseWrite(namespace, name string) *releaseWriteState {
	c.writesMu.Lock()
	defer c.writesMu.Unlock()
	if c.writes == nil {
		c.writes = map[string]*releaseWriteState{}
	}
	key := namespace + "/" + name
	if c.writes[key] == nil {
		c.writes[key] = &releaseWriteState{}
	}
	return c.writes[key]
}

func (c *Client) beginReleaseWrite(namespace, name string) (*releaseWriteState, func() error, error) {
	state := c.releaseWrite(namespace, name)
	if !state.mu.TryLock() {
		return nil, nil, fmt.Errorf("a Helm writer for this release is still active")
	}
	state.seen.Store(0)
	state.store = nil
	return state, func() error {
		defer state.mu.Unlock()
		revision := int(state.seen.Load())
		if revision == 0 {
			return nil
		}
		state.completedRevision = revision
		rel, err := state.store.Get(name, revision)
		if err != nil {
			return fmt.Errorf("read completed Helm revision: %w", err)
		}
		if rel == nil || rel.Name != name || rel.Namespace != namespace || rel.Version != revision {
			return fmt.Errorf("completed Helm revision identity changed")
		}
		completed := *rel
		completed.Labels = maps.Clone(rel.Labels)
		if completed.Labels == nil {
			completed.Labels = make(map[string]string)
		}
		completed.Labels[completedRevisionLabel] = strconv.Itoa(revision)
		if err := state.store.Update(&completed); err != nil {
			return fmt.Errorf("record completed Helm revision: %w", err)
		}
		return nil
	}, nil
}

func trackReleaseWrites(cfg *action.Configuration, state *releaseWriteState) {
	state.store = cfg.Releases
	cfg.Releases = storage.Init(&releaseWriteDriver{Driver: cfg.Releases.Driver, state: state})
}

// MaintenanceQuiescent checks local writers and recovers completed releases
// from their terminal status or revision-bound completion receipt after restart.
func (c *Client) MaintenanceQuiescent(ctx context.Context, namespace, name string) error {
	return c.maintenanceQuiescent(ctx, namespace, name, c.CurrentRelease)
}

func (c *Client) maintenanceQuiescent(ctx context.Context, namespace, name string, read func(context.Context, string, string) (*release.Release, error)) error {
	state := c.releaseWrite(namespace, name)
	if !state.mu.TryLock() {
		return fmt.Errorf("the previous Helm writer is still active")
	}
	defer state.mu.Unlock()
	current, err := read(ctx, namespace, name)
	if err != nil {
		return err
	}
	if current == nil || current.Version < 1 {
		return fmt.Errorf("the current Helm revision is unavailable")
	}
	if state.completedRevision < 1 {
		terminal := current.Info != nil && (current.Info.Status == release.StatusDeployed || current.Info.Status == release.StatusFailed)
		if !terminal && current.Labels[completedRevisionLabel] != strconv.Itoa(current.Version) {
			return fmt.Errorf("the current revision has no confirmed completed writer")
		}
		state.completedRevision = current.Version
	}
	if current.Version != state.completedRevision {
		return fmt.Errorf("the Helm revision changed after its writer completed")
	}
	return nil
}

type releaseWriteDriver struct {
	driver.Driver
	state *releaseWriteState
}

func (d *releaseWriteDriver) Create(key string, rel *release.Release) error {
	if rel == nil || rel.Version < 1 {
		return fmt.Errorf("invalid Helm writer revision")
	}
	if err := d.Driver.Create(key, rel); err != nil {
		return err
	}
	d.state.seen.Store(int64(rel.Version))
	return nil
}
