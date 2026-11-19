// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"context"
	"log/slog"
	"time"
)

// RunMaintenance expires only OAuth transactions. Connection references and
// revocation tombstones have separate lifecycles and are never swept here.
func (s *Server) RunMaintenance(ctx context.Context) {
	interval := s.cfg.AuthorizationCleanupInterval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	for {
		sweepCtx, cancel := context.WithTimeout(ctx, time.Minute)
		err := s.store.pruneAuthorizationRecords(sweepCtx, nil, s.currentTime().UTC())
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Warn("OAuth transaction cleanup failed")
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
