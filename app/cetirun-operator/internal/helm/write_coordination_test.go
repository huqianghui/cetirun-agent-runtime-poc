// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"context"
	"testing"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
)

func TestMaintenanceQuiescenceNeedsItsCompletedWriter(t *testing.T) {
	c := &Client{}
	reads := 0
	current := &release.Release{Name: "example", Namespace: "test", Version: 7, Info: &release.Info{Status: release.StatusPendingInstall}}
	read := func(context.Context, string, string) (*release.Release, error) { reads++; return current, nil }
	if c.maintenanceQuiescent(context.Background(), "test", "example", read) == nil || reads != 1 {
		t.Fatal("an unobserved writer was accepted")
	}
	state, done, err := c.beginReleaseWrite("test", "example")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.beginReleaseWrite("test", "example"); err == nil {
		t.Fatal("two writers acquired the same release")
	}
	if c.maintenanceQuiescent(context.Background(), "test", "example", read) == nil || reads != 1 {
		t.Fatal("running SDK writer was treated as complete")
	}
	cfg := &action.Configuration{Releases: storage.Init(driver.NewMemory())}
	trackReleaseWrites(cfg, state)
	if err := cfg.Releases.Create(current); err != nil {
		t.Fatal(err)
	}
	if c.maintenanceQuiescent(context.Background(), "test", "example", read) == nil {
		t.Fatal("storage publication alone authorized handoff before SDK return")
	}
	if err := done(); err != nil {
		t.Fatal(err)
	}
	if err := c.maintenanceQuiescent(context.Background(), "test", "example", read); err != nil {
		t.Fatal(err)
	}
	current.Version = 8
	if c.maintenanceQuiescent(context.Background(), "test", "example", read) == nil {
		t.Fatal("a replacement revision inherited the completion proof")
	}
	other := &Client{}
	if other.maintenanceQuiescent(context.Background(), "test", "example", read) == nil {
		t.Fatal("a new process adopted an unknown prior writer")
	}
}

func TestMaintenanceQuiescenceKeepsReleasesIndependent(t *testing.T) {
	c := &Client{}
	_, first, err := c.beginReleaseWrite("one", "example")
	if err != nil {
		t.Fatal(err)
	}
	defer first()
	_, second, err := c.beginReleaseWrite("two", "example")
	if err != nil {
		t.Fatal("unrelated namespace writer was blocked")
	}
	defer second()
	_, third, err := c.beginReleaseWrite("one", "another")
	if err != nil {
		t.Fatal("unrelated release writer was blocked")
	}
	defer third()
}

func TestMaintenanceQuiescenceRecoversTerminalReleaseForFreshClient(t *testing.T) {
	for _, status := range []release.Status{release.StatusDeployed, release.StatusFailed} {
		t.Run(string(status), func(t *testing.T) {
			stored := storage.Init(driver.NewMemory())
			current := &release.Release{Name: "example", Namespace: "test", Version: 7, Info: &release.Info{Status: status}}
			if err := stored.Create(current); err != nil {
				t.Fatal(err)
			}
			read := func(_ context.Context, _, name string) (*release.Release, error) { return stored.Last(name) }
			c := &Client{}
			if err := c.maintenanceQuiescent(context.Background(), "test", "example", read); err != nil {
				t.Fatalf("first maintenance after restart cannot recover a completed release: %v", err)
			}
			_, done, err := c.beginReleaseWrite("test", "example")
			if err != nil {
				t.Fatal(err)
			}
			if err := c.maintenanceQuiescent(context.Background(), "test", "example", read); err == nil {
				t.Fatal("terminal storage status bypassed an active SDK writer")
			}
			if err := done(); err != nil {
				t.Fatal(err)
			}
			next := &release.Release{Name: "example", Namespace: "test", Version: 8, Info: &release.Info{Status: status}}
			if err := stored.Create(next); err != nil {
				t.Fatal(err)
			}
			if err := c.maintenanceQuiescent(context.Background(), "test", "example", read); err == nil {
				t.Fatal("a replacement revision inherited the recovered completion proof")
			}
		})
	}
}

func TestMaintenanceQuiescenceRejectsUnconfirmedPersistedRelease(t *testing.T) {
	for _, status := range []release.Status{release.StatusPendingInstall, release.StatusPendingUpgrade,
		release.StatusPendingRollback, release.StatusUnknown, release.StatusUninstalling} {
		t.Run(string(status), func(t *testing.T) {
			c := &Client{}
			current := &release.Release{Name: "example", Namespace: "test", Version: 7, Info: &release.Info{Status: status}}
			read := func(context.Context, string, string) (*release.Release, error) { return current, nil }
			if err := c.maintenanceQuiescent(context.Background(), "test", "example", read); err == nil {
				t.Fatal("a fresh client accepted an unfinished or unknown release")
			}
		})
	}
}

func TestMaintenanceQuiescenceRecoversCompletedPendingWriter(t *testing.T) {
	c := &Client{}
	state, done, err := c.beginReleaseWrite("test", "example")
	if err != nil {
		t.Fatal(err)
	}
	stored := storage.Init(driver.NewMemory())
	cfg := &action.Configuration{Releases: stored}
	trackReleaseWrites(cfg, state)
	current := &release.Release{Name: "example", Namespace: "test", Version: 7, Info: &release.Info{Status: release.StatusPendingInstall}}
	if err := cfg.Releases.Create(current); err != nil {
		t.Fatal(err)
	}
	read := func(_ context.Context, _, name string) (*release.Release, error) { return stored.Last(name) }
	fresh := &Client{}
	if err := fresh.maintenanceQuiescent(context.Background(), "test", "example", read); err == nil {
		t.Fatal("storage publication authorized handoff before SDK completion")
	}
	if err := done(); err != nil {
		t.Fatal(err)
	}
	if err := fresh.maintenanceQuiescent(context.Background(), "test", "example", read); err != nil {
		t.Fatalf("restart lost the completed pending-install writer: %v", err)
	}
	current, err = stored.Last("example")
	if err != nil {
		t.Fatal(err)
	}
	next := *current
	next.Version = 8
	if err := stored.Create(&next); err != nil {
		t.Fatal(err)
	}
	other := &Client{}
	if err := other.maintenanceQuiescent(context.Background(), "test", "example", read); err == nil {
		t.Fatal("a pending revision inherited an older revision's completion receipt")
	}
}
