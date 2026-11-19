// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxwriter

import (
	"fmt"
	"reflect"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

func validateObservation(request Request, observed Observation, now time.Time, maxAge time.Duration) error {
	if !reflect.DeepEqual(observed.Request, request) || observed.Coverage != CompleteHistory {
		return fmt.Errorf("%w: observation must bind the exact journal, operation, enrollment, lifetime and complete history", ErrUnproven)
	}
	if err := validateObservationTime(observed.ObservedAt, now, maxAge); err != nil {
		return err
	}
	if observed.Barrier.Method != DurableStartBarrier || !boundedValue(observed.Barrier.ID, 256) {
		return fmt.Errorf("%w: a durable guest-external start barrier is required", ErrUnproven)
	}
	if len(observed.Writers) != len(request.Writers) {
		return fmt.Errorf("%w: every historical writer must have exclusion evidence", ErrUnproven)
	}
	writers := make(map[types.UID]Writer, len(request.Writers))
	volumes := make(map[StorageIdentity]bool)
	for _, writer := range request.Writers {
		writers[writer.Pod.UID] = writer
		volumes[writer.Storage] = true
	}
	seen := make(map[types.UID]bool, len(request.Writers))
	for _, proof := range observed.Writers {
		writer, present := writers[proof.Writer.Pod.UID]
		if !present || seen[proof.Writer.Pod.UID] || proof.Writer != writer || !boundedValue(proof.EvidenceID, 256) {
			return fmt.Errorf("%w: missing, duplicate or changed historical writer identity", ErrUnproven)
		}
		switch proof.Method {
		case RuntimeRevocation, InfrastructureDestruction:
		default:
			return fmt.Errorf("%w: writer exclusion method is not supported durable evidence", ErrUnproven)
		}
		seen[proof.Writer.Pod.UID] = true
	}
	if observed.Home.Home != request.Lifetime.Home || observed.Home.State != Released ||
		!boundedValue(observed.Home.EvidenceID, 256) || len(observed.Home.Volumes) != len(volumes) {
		return fmt.Errorf("%w: exact historical HOME and all storage releases are required", ErrUnproven)
	}
	released := make(map[StorageIdentity]bool, len(volumes))
	for _, proof := range observed.Home.Volumes {
		if !volumes[proof.Storage] || released[proof.Storage] || proof.State != Released || !boundedValue(proof.EvidenceID, 256) {
			return fmt.Errorf("%w: missing, pending, duplicate or changed historical storage release", ErrUnproven)
		}
		released[proof.Storage] = true
	}
	return nil
}

func validateObservationTime(observedAt, now time.Time, maxAge time.Duration) error {
	if maxAge <= 0 || maxAge > MaxObservationAge || !now.After(time.Unix(0, 0)) ||
		!observedAt.After(time.Unix(0, 0)) || observedAt.After(now) || now.Sub(observedAt) > maxAge {
		return fmt.Errorf("%w: observation timestamp is missing, stale or in the future", ErrUnproven)
	}
	return nil
}
