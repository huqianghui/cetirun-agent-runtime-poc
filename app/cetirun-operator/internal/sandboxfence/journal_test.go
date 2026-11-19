// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxfence

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestParentSyncPrecedesReservationSuccess(t *testing.T) {
	for _, retry := range []bool{false, true} {
		name := "new-directory"
		if retry {
			name = "existing-directory-after-interrupted-creation"
		}
		t.Run(name, func(t *testing.T) {
			f := newARMFixture(t)
			if retry {
				if err := os.Mkdir(f.dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			syncs := 0
			f.parentSync = func(parentDirectory string) error {
				syncs++
				if parentDirectory != filepath.Dir(f.dir) {
					t.Fatal("the containing parent directory was not selected for sync")
				}
				if info, err := os.Stat(f.dir); err != nil || !info.IsDir() {
					t.Fatal("parent sync must follow creation of the journal directory")
				}
				f.mu.Lock()
				requests := len(f.requests)
				f.mu.Unlock()
				if requests != 0 {
					t.Fatal("ARM was contacted before journal parent synchronization")
				}
				return syncJournalParent(parentDirectory)
			}
			journal, err := f.invoke(context.Background(), Reserve, false)
			if err != nil || journal == nil || journal.Phase != Reserved || syncs != 1 {
				t.Fatalf("reservation bypassed successful parent sync: error=%v syncs=%d", err, syncs)
			}
			if len(f.mutations()) != 0 {
				t.Fatal("reservation changed a cloud resource")
			}
		})
	}
}

func TestParentSyncFailureBlocksARMAndJournalSuccess(t *testing.T) {
	for _, action := range []Action{Reserve, Register, Inspect, TestCondition, Delete, Observe} {
		t.Run(string(action), func(t *testing.T) {
			f := newARMFixture(t)
			switch action {
			case Reserve:
				// Simulate a previous process creating the directory but not syncing its parent.
				if err := os.Mkdir(f.dir, 0700); err != nil {
					t.Fatal(err)
				}
			case Register:
				if _, err := f.invoke(context.Background(), Reserve, false); err != nil {
					t.Fatal(err)
				}
				f.poolExists, f.vmExists = true, true
			case Inspect, TestCondition:
				f.prepare(false)
			case Delete, Observe:
				f.prepare(true)
				if action == Observe {
					if _, err := f.invoke(context.Background(), Delete, true); !errors.Is(err, ErrPending) {
						t.Fatal(err)
					}
				}
			}
			path := filepath.Join(f.dir, "journal.json")
			before, beforeErr := os.ReadFile(path)
			if beforeErr != nil && !errors.Is(beforeErr, os.ErrNotExist) {
				t.Fatal(beforeErr)
			}
			f.mu.Lock()
			beforeRequests := len(f.requests)
			f.mu.Unlock()
			syncFailure := errors.New("synthetic parent-directory fsync failure")
			syncs := 0
			f.parentSync = func(parentDirectory string) error {
				syncs++
				if parentDirectory != filepath.Dir(f.dir) {
					t.Fatal("wrong parent directory")
				}
				return syncFailure
			}
			journal, err := f.invoke(context.Background(), action, action == TestCondition || action == Delete)
			if !errors.Is(err, syncFailure) || journal != nil || syncs != 1 {
				t.Fatalf("parent-sync failure was not propagated: error=%v syncs=%d", err, syncs)
			}
			f.mu.Lock()
			afterRequests := len(f.requests)
			f.mu.Unlock()
			if afterRequests != beforeRequests {
				t.Fatal("failed parent sync allowed an ARM request")
			}
			after, afterErr := os.ReadFile(path)
			if !bytes.Equal(before, after) ||
				errors.Is(beforeErr, os.ErrNotExist) != errors.Is(afterErr, os.ErrNotExist) ||
				afterErr != nil && !errors.Is(afterErr, os.ErrNotExist) {
				t.Fatal("failed parent sync created or changed successful journal state")
			}
		})
	}
}
