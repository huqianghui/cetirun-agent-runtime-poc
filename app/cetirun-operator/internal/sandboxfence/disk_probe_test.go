// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxfence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	strictjson "sigs.k8s.io/json"
)

const (
	diskProbeAPIVersion = "2025-01-02"
	fixtureDisk         = "/subscriptions/" + fixtureSubscription + "/resourceGroups/qualification-nodes/providers/Microsoft.Compute/disks/home"
	fixtureDiskUID      = "00000000-0000-4000-8000-000000000008"
	fixtureDiskBody     = `{
		"id": "` + fixtureDisk + `",
		"managedBy": "",
		"managedByExtended": [],
		"properties": {
			"uniqueId": "` + fixtureDiskUID + `",
			"diskState": "Unattached",
			"provisioningState": "Succeeded",
			"maxShares": 1,
			"shareInfo": []
		}
	}`
)

type diskProbeIdentity struct {
	ResourceID string
	UniqueID   string
}

// This test-only probe checks a necessary control-plane observation, not
// physical release, writer exclusion, complete enrollment or a start permit.
func probeUnattachedDisk(ctx context.Context, c *armClient, expected diskProbeIdentity) error {
	parts := strings.Split(expected.ResourceID, "/")
	if len(parts) != 9 || parts[0] != "" || !strings.EqualFold(parts[1], "subscriptions") ||
		!uuidRE.MatchString(parts[2]) || !strings.EqualFold(parts[3], "resourceGroups") ||
		!nameRE.MatchString(parts[4]) || !strings.EqualFold(parts[5], "providers") ||
		!strings.EqualFold(parts[6], "Microsoft.Compute") || !strings.EqualFold(parts[7], "disks") ||
		!nameRE.MatchString(parts[8]) || len(parts[8]) > 80 || !uuidRE.MatchString(expected.UniqueID) {
		return errors.New("disk probe requires a pinned managed-disk resource ID and uniqueId")
	}
	result, err := c.request(ctx, http.MethodGet, c.resourceURL(expected.ResourceID, diskProbeAPIVersion), "", "")
	if err != nil {
		return err
	}
	if result.status != http.StatusOK {
		return fmt.Errorf("disk observation unavailable: %w", unexpectedHTTP(result.status))
	}
	var disk struct {
		ID                string   `json:"id"`
		ManagedBy         string   `json:"managedBy"`
		ManagedByExtended []string `json:"managedByExtended"`
		Properties        struct {
			UniqueID          string            `json:"uniqueId"`
			DiskState         string            `json:"diskState"`
			ProvisioningState string            `json:"provisioningState"`
			MaxShares         *int              `json:"maxShares"`
			ShareInfo         []json.RawMessage `json:"shareInfo"`
		} `json:"properties"`
	}
	problems, err := strictjson.UnmarshalStrict(result.body, &disk, strictjson.DisallowDuplicateFields)
	if err != nil {
		return fmt.Errorf("decode disk observation: %w", err)
	}
	if len(problems) != 0 {
		return fmt.Errorf("ambiguous disk observation: %w", errors.Join(problems...))
	}
	if !strings.EqualFold(disk.ID, expected.ResourceID) || !strings.EqualFold(disk.Properties.UniqueID, expected.UniqueID) {
		return errors.New("disk observation does not match the pinned storage incarnation")
	}
	if disk.Properties.MaxShares == nil || *disk.Properties.MaxShares != 1 {
		return errors.New("disk probe requires explicit single-attachment capacity")
	}
	if disk.Properties.ProvisioningState != "Succeeded" || disk.Properties.DiskState != "Unattached" ||
		disk.ManagedBy != "" || len(disk.ManagedByExtended) != 0 || len(disk.Properties.ShareInfo) != 0 {
		return errors.New("disk is not independently observed unattached")
	}
	return ctx.Err()
}

func newDiskProbeClient(t *testing.T, status int, body string) (*armClient, *atomic.Int32) {
	t.Helper()
	requests := &atomic.Int32{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != fixtureDisk || r.URL.RawQuery != "api-version="+diskProbeAPIVersion {
			t.Errorf("unexpected disk request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status != http.StatusNoContent {
			if _, err := io.WriteString(w, body); err != nil {
				t.Error(err)
			}
		}
	}))
	t.Cleanup(server.Close)
	c, err := makeARMClient(fixtureCredential{}, server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return c, requests
}

func TestDiskProbeUnattachedObservation(t *testing.T) {
	c, requests := newDiskProbeClient(t, http.StatusOK, fixtureDiskBody)
	expected := diskProbeIdentity{fixtureDisk, fixtureDiskUID}
	for range 2 {
		if err := probeUnattachedDisk(context.Background(), c, expected); err != nil {
			t.Fatal(err)
		}
	}
	if requests.Load() != 2 {
		t.Fatal("a previous disk observation replaced a fresh read")
	}
}

func TestDiskProbeRejectsIncompleteOrConflictingEvidence(t *testing.T) {
	changes := []struct{ name, before, after string }{
		{"attached", `"Unattached"`, `"Attached"`},
		{"deallocated owner", `"Unattached"`, `"Reserved"`},
		{"hibernated owner", `"Unattached"`, `"Frozen"`},
		{"active export", `"Unattached"`, `"ActiveSAS"`},
		{"hibernated export", `"Unattached"`, `"ActiveSASFrozen"`},
		{"upload preparation", `"Unattached"`, `"ReadyToUpload"`},
		{"active upload", `"Unattached"`, `"ActiveUpload"`},
		{"unknown state", `"Unattached"`, `"FutureState"`},
		{"missing state", `"diskState": "Unattached",`, ``},
		{"provisioning pending", `"Succeeded"`, `"Updating"`},
		{"provisioning failed", `"Succeeded"`, `"Failed"`},
		{"missing provisioning", `"provisioningState": "Succeeded",`, ``},
		{"old owner remains", `"managedBy": ""`, `"managedBy": "` + fixtureVM + `"`},
		{"extended owner remains", `"managedByExtended": []`, `"managedByExtended": ["` + fixtureVM + `"]`},
		{"share owner remains", `"shareInfo": []`, `"shareInfo": [{"vmUri": "` + fixtureVM + `"}]`},
		{"shared disk", `"maxShares": 1`, `"maxShares": 2`},
		{"unknown sharing", `"maxShares": 1,`, ``},
		{"null sharing", `"maxShares": 1`, `"maxShares": null`},
		{"different resource", fixtureDisk, fixtureDisk + "-replacement"},
		{"recreated disk", fixtureDiskUID, fixtureReplacement},
		{"missing incarnation", `"uniqueId": "` + fixtureDiskUID + `",`, ``},
		{"duplicate state", `"diskState": "Unattached"`, `"diskState": "Attached", "diskState": "Unattached"`},
		{"duplicate owner", `"managedBy": ""`, `"managedBy": "` + fixtureVM + `", "managedBy": ""`},
		{"wrong field type", `"managedByExtended": []`, `"managedByExtended": false`},
	}
	for _, change := range changes {
		t.Run(change.name, func(t *testing.T) {
			body := strings.Replace(fixtureDiskBody, change.before, change.after, 1)
			c, requests := newDiskProbeClient(t, http.StatusOK, body)
			if err := probeUnattachedDisk(context.Background(), c, diskProbeIdentity{fixtureDisk, fixtureDiskUID}); err == nil {
				t.Fatal("incomplete or conflicting disk evidence was accepted")
			}
			if requests.Load() != 1 {
				t.Fatal("disk observation retried or did not read independently")
			}
		})
	}
	for _, body := range []string{`null`, `{`, `{"error":{"code":"ResourceNotFound"}}`} {
		t.Run("invalid document "+body, func(t *testing.T) {
			c, _ := newDiskProbeClient(t, http.StatusOK, body)
			if err := probeUnattachedDisk(context.Background(), c, diskProbeIdentity{fixtureDisk, fixtureDiskUID}); err == nil {
				t.Fatal("an invalid disk document became a release observation")
			}
		})
	}
}

func TestDiskProbeRequiresPositiveReadAndPinnedIdentity(t *testing.T) {
	for _, status := range []int{202, 204, 401, 403, 404, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			c, requests := newDiskProbeClient(t, status, fixtureDiskBody)
			if err := probeUnattachedDisk(context.Background(), c, diskProbeIdentity{fixtureDisk, fixtureDiskUID}); err == nil {
				t.Fatal("a non-200 disk read became a release observation")
			}
			if requests.Load() != 1 {
				t.Fatal("disk observation retried")
			}
		})
	}
	c, requests := newDiskProbeClient(t, http.StatusOK, fixtureDiskBody)
	for _, identity := range []diskProbeIdentity{
		{ResourceID: fixtureDisk},
		{UniqueID: fixtureDiskUID},
		{ResourceID: fixtureVM, UniqueID: fixtureDiskUID},
		{ResourceID: fixtureDisk + "?api-version=other", UniqueID: fixtureDiskUID},
	} {
		if err := probeUnattachedDisk(context.Background(), c, identity); err == nil {
			t.Fatal("an incomplete or unscoped disk identity was accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := probeUnattachedDisk(ctx, c, diskProbeIdentity{fixtureDisk, fixtureDiskUID}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled observation was not preserved: %v", err)
	}
	if requests.Load() != 0 {
		t.Fatal("invalid identity or canceled observation reached ARM")
	}
}

func TestDiskProbeRemainsIndependentOfPoolReceipt(t *testing.T) {
	f := newARMFixture(t)
	f.prepare(true)
	if _, err := f.invoke(context.Background(), Delete, true); !errors.Is(err, ErrPending) {
		t.Fatal(err)
	}
	journal, err := f.invoke(context.Background(), Observe, false)
	if err != nil || journal.Phase != Complete {
		t.Fatalf("synthetic pool operation did not complete: %v", err)
	}
	c, requests := newDiskProbeClient(t, http.StatusOK, strings.Replace(fixtureDiskBody, `"Unattached"`, `"Reserved"`, 1))
	if err := probeUnattachedDisk(context.Background(), c, diskProbeIdentity{fixtureDisk, fixtureDiskUID}); err == nil {
		t.Fatal("a completed empty-pool receipt substituted for independent disk evidence")
	}
	if requests.Load() != 1 || len(f.mutations()) != 2 {
		t.Fatal("disk observation skipped its read or repeated pool deletion")
	}
}

func TestDiskProbeCannotRepairLostDeletionReply(t *testing.T) {
	f := newARMFixture(t)
	f.prepare(true)
	f.loseDelete = true
	journal, err := f.invoke(context.Background(), Delete, true)
	if err == nil || journal.Phase != DeletePending || journal.Deletion.OperationURL != "" {
		t.Fatalf("lost reply was not retained as unknown: %v", err)
	}
	requestID := journal.Deletion.RequestID
	f.mu.Lock()
	f.poolExists, f.vmExists = false, false
	f.mu.Unlock()
	c, _ := newDiskProbeClient(t, http.StatusOK, fixtureDiskBody)
	if err := probeUnattachedDisk(context.Background(), c, diskProbeIdentity{fixtureDisk, fixtureDiskUID}); err != nil {
		t.Fatal(err)
	}
	// Each invocation reopens the durable journal, as after a process restart.
	for range 2 {
		journal, err = f.invoke(context.Background(), Observe, false)
		if !errors.Is(err, ErrPending) || journal.Phase != DeletePending || journal.Completion != nil ||
			journal.Deletion.RequestID != requestID {
			t.Fatalf("resource absence and an unattached disk repaired missing operation evidence: %v", err)
		}
	}
	if _, err := f.invoke(context.Background(), Delete, true); err == nil || len(f.mutations()) != 2 {
		t.Fatal("lost deletion reply authorized a destructive resubmission")
	}
}
