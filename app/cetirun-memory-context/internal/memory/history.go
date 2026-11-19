// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package memory

import (
	"context"
	"errors"
	"path"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

const (
	retainedHistory = 128
	// minimumHistory is the shortest window kept when the manifest would
	// otherwise exceed MaxManifest. Its receipts still resolve recent unknown
	// outcomes.
	minimumHistory = 32
)

// historyWindows are tried in order until the encoded manifest fits.
var historyWindows = []int64{retainedHistory, 96, 64, 48, minimumHistory}

var transactionName = regexp.MustCompile(`^[a-f0-9]{64}\.json$`)

func historyObject(id string) bool {
	return strings.HasPrefix(id, "use-") || strings.HasPrefix(id, "change-")
}

// compactHistory keeps the use/change records and write receipts of the
// latest retainedHistory generations, and history that a persistent object
// still references. The number of generations alone does not bound the
// manifest, and a rejected write never moves the window, so when the encoded
// manifest would exceed MaxManifest fewer generations are kept, down to
// minimumHistory. It returns the encoded manifest size; the caller rejects a
// manifest that still does not fit.
func (v *snapshot) compactHistory() (int, error) {
	generations := map[string]int64{}
	for id, ref := range v.m.Objects {
		if !historyObject(id) {
			continue
		}
		if strings.HasPrefix(id, "use-") {
			var use Use
			if err := v.read(ref, &use); err != nil {
				return 0, err
			}
			generations[id] = use.ManifestGeneration + 1
		} else {
			var change Change
			if err := v.read(ref, &change); err != nil {
				return 0, err
			}
			generations[id] = change.GenerationAfter
		}
	}
	size := 0
	for _, window := range historyWindows {
		v.retainHistory(window, generations)
		data, err := encode(v.m)
		if err != nil {
			return 0, err
		}
		if size = len(data); size <= MaxManifest {
			break
		}
	}
	return size, nil
}

// retainHistory drops receipts, and use/change records that nothing retained
// references, from generations older than the window.
func (v *snapshot) retainHistory(window int64, generations map[string]int64) {
	oldest := v.m.Generation - window
	for key, receipt := range v.m.Requests {
		if receipt.Generation <= oldest {
			delete(v.m.Requests, key)
		}
	}
	keep := map[string]bool{}
	for id := range v.m.Objects {
		if generation, history := generations[id]; !history || generation > oldest {
			keep[id] = true
		}
	}
	for _, receipt := range v.m.Requests {
		for _, ref := range receipt.Result.Refs {
			keep[ref.ID] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for id, refs := range v.m.Dependencies {
			if !keep[id] {
				continue
			}
			for _, ref := range refs {
				if !keep[ref.ID] {
					keep[ref.ID], changed = true, true
				}
			}
		}
	}
	for id := range v.m.Objects {
		if historyObject(id) && !keep[id] {
			delete(v.m.Objects, id)
			delete(v.m.Dependencies, id)
			v.m.HistoryCompacted = true
		}
	}
}

// removeAbandonedTransactions deletes journals that can no longer commit, so a
// failed attempt neither keeps its data on disk nor fills the root until every
// write fails. A journal that cannot be decoded can never be replayed, and one
// cut short by a crash has no objects yet, since objects are written after
// their journal; collectHistory would otherwise reject every write. The commit
// path rejects a journal whose manifest exceeds MaxManifest and replays a
// journal only at its own expected generation. An older journal whose receipt
// is retained has committed, so only the journal goes. Without a receipt, an
// attempt from the last minimumHistory generations never committed, so its
// object files go as well. For older journals, keep every revision of IDs still
// retained or referenced by the manifest; remove only unreferenced objects.
// Removal is best effort: a file that cannot be removed
// keeps its journal for a later write and does not fail this one. The caller
// holds the write lock.
func (v *snapshot) removeAbandonedTransactions(ctx context.Context) error {
	names, err := v.fs.names("transactions")
	if errors.Is(err, syscall.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	current := map[string]bool{}
	retained := map[string]bool{}
	for _, ref := range v.m.Objects {
		retained[ref.ID] = true
		if object, err := objectPath(ref); err == nil {
			current[object] = true
		}
	}
	for _, refs := range v.m.Dependencies {
		for _, ref := range refs {
			retained[ref.ID] = true
		}
	}
journals:
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !transactionName.MatchString(name) {
			continue
		}
		journal := "transactions/" + name
		data, err := v.fs.read(journal, MaxInput*4)
		if err != nil {
			continue
		}
		var tx transaction
		if Decode(data, &tx) != nil {
			_ = v.fs.remove(journal)
			continue
		}
		if tx.Manifest.Generation != tx.ExpectedGeneration+1 {
			continue
		}
		manifest, err := encode(tx.Manifest)
		if err != nil {
			continue
		}
		committed := false
		old := tx.Manifest.Generation <= v.m.Generation-minimumHistory
		if len(manifest) <= MaxManifest {
			if tx.ExpectedGeneration >= v.m.Generation {
				continue
			}
			receipt, ok := v.m.Requests[strings.TrimSuffix(name, ".json")]
			committed = ok && receipt.Generation == tx.Manifest.Generation
		}
		if !committed {
			staged := map[string]bool{}
			for _, ref := range tx.Manifest.Objects {
				if old && retained[ref.ID] {
					continue
				}
				if object, err := objectPath(ref); err == nil {
					staged[object] = true
				}
			}
			for _, file := range tx.Files {
				if !staged[file.Name] || current[file.Name] {
					continue
				}
				// An attempt can stop before writing an object, so its
				// directory may not exist.
				if v.fs.remove(file.Name) != nil {
					continue journals
				}
				if err := v.fs.removeDirectory(path.Dir(file.Name)); err != nil && !errors.Is(err, syscall.ENOENT) {
					continue journals
				}
			}
		}
		_ = v.fs.remove(journal)
	}
	return nil
}

// The caller holds the write lock; shared read locks pin files for active readers.
// Recompute garbage from the committed manifest so interrupted cleanup can resume.
func (v *snapshot) collectHistory(ctx context.Context) error {
	if !v.m.HistoryCompacted {
		return nil
	}
	files, _, err := v.fs.inventory()
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for id := range v.m.Objects {
		keep[id] = true
	}
	for _, refs := range v.m.Dependencies {
		for _, ref := range refs {
			keep[ref.ID] = true
		}
	}
	for _, file := range files {
		if !strings.HasPrefix(file.Name, "transactions/") {
			continue
		}
		data, err := v.fs.read(file.Name, MaxInput*4)
		if err != nil {
			return err
		}
		var tx transaction
		if Decode(data, &tx) != nil {
			return problem(6, "PendingDataUnresolved")
		}
		for id := range tx.Manifest.Objects {
			keep[id] = true
		}
	}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		parts := strings.Split(file.Name, "/")
		if len(parts) == 3 && parts[0] == "state" && parts[1] == "manifests" && strings.HasSuffix(parts[2], ".json") {
			generation, err := strconv.ParseInt(strings.TrimSuffix(parts[2], ".json"), 10, 64)
			if err == nil && generation > 0 && generation < v.m.Generation {
				if err := v.fs.remove(file.Name); err != nil {
					return err
				}
			}
		}
		if len(parts) != 4 || parts[0] != "objects" || (parts[1] != "use" && parts[1] != "change") || !objectID.MatchString(parts[2]) || !strings.HasPrefix(parts[2], parts[1]+"-") || parts[3] != "r1.json" || keep[parts[2]] {
			continue
		}
		data, err := v.fs.read(file.Name, 16<<10)
		if err != nil {
			return err
		}
		if parts[1] == "use" {
			var use Use
			if Decode(data, &use) != nil || use.ID != parts[2] || use.Revision != 1 {
				return problem(6, "IntegrityMismatch")
			}
		} else {
			var change Change
			if Decode(data, &change) != nil || change.ID != parts[2] || change.Revision != 1 {
				return problem(6, "IntegrityMismatch")
			}
		}
		if err := v.fs.remove(file.Name); err != nil {
			return err
		}
		if err := v.fs.removeDirectory(strings.Join(parts[:3], "/")); err != nil {
			return err
		}
	}
	return nil
}
