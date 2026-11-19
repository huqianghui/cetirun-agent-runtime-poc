// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func transactionFiles(t *testing.T, f fixture) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(f.store.Config.Root, "transactions"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func filesContaining(t *testing.T, f fixture, marker string) []string {
	t.Helper()
	found := []string{}
	err := filepath.WalkDir(f.store.Config.Root, func(name string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(name)
		if err == nil && strings.Contains(string(data), marker) {
			found = append(found, name)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// Every native request records the refs it injected. Large records must not
// let the history window outgrow the manifest, which would reject every later
// write.
func TestInjectionHistoryKeepsTheStoreWritable(t *testing.T) {
	for _, injected := range []int{8, MaxQueryLimit} {
		t.Run(fmt.Sprintf("refs=%d", injected), func(t *testing.T) {
			f := newFixture(t)
			binding := nativeBinding(f)
			capture := func(title string) Ref {
				note := testNote(title)
				note.Scope = Scope{"project": binding.CWD}
				return f.native(t, binding, "tool", "capture", captureInput{Note: &note}).Data.(writeResult).Refs[0]
			}
			refs := []Ref{}
			for i := 0; i < injected; i++ {
				refs = append(refs, capture(fmt.Sprintf("injected note %d", i)))
			}
			var last NativeEvent
			var saved Response
			for i := 0; i < 200; i++ {
				last = nativeEvent(binding, "injected", "", map[string]any{"refs": refs})
				var code int
				if saved, code = f.store.NativeEvent(context.Background(), last, "contract"); code != 0 {
					t.Fatalf("injection %d was rejected: %+v", i+1, saved)
				}
				if i%10 == 9 {
					capture(fmt.Sprintf("later note %d", i))
				}
			}
			if replay, code := f.store.NativeEvent(context.Background(), last, "contract"); code != 0 || replay.Generation != saved.Generation {
				t.Fatal("the latest injection could not replay", replay)
			}
			v, err := f.store.open()
			if err != nil {
				t.Fatal(err)
			}
			defer v.fs.close()
			if retained := len(v.m.Requests); retained < minimumHistory || retained >= retainedHistory {
				t.Fatalf("%d receipts retained; the window did not shrink within its bounds", retained)
			}
			if names := transactionFiles(t, f); len(names) != 0 {
				t.Fatal("committed writes left journals", names)
			}
		})
	}
}

// A write that cannot fit the manifest fails before anything durable is
// written, instead of leaving its journal and objects behind.
func TestFullManifestRejectsWritesBeforeTheJournal(t *testing.T) {
	f := newFixture(t)
	// Synthetic tombstones leave less room than one more note needs.
	_, _, err := f.store.write(context.Background(), f.request(t, struct{}{}), "fixture-fill", false, func(v *snapshot) (writeResult, error) {
		for {
			data, err := encode(v.m)
			if err != nil {
				return writeResult{}, err
			}
			remaining := MaxManifest - 1024 - len(data)
			if remaining <= 0 {
				return writeResult{Refs: []Ref{}}, nil
			}
			for i := 0; i < max(1, remaining/64); i++ {
				v.m.Tombstones[newID("note")] = true
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	generation := f.request(t, struct{}{}).ExpectedGeneration
	rejected := testNote("does not fit")
	rejected.Body = "SYNTHETIC-REJECTED-BODY"
	if response, code := f.store.Execute(context.Background(), "capture", f.request(t, captureInput{Note: &rejected})); code != 5 || response.Reason != "LimitReached" {
		t.Fatal("full manifest accepted another note", response)
	}
	if names := transactionFiles(t, f); len(names) != 0 {
		t.Fatal("a rejected write left a journal", names)
	}
	if found := filesContaining(t, f, rejected.Body); len(found) != 0 {
		t.Fatal("a rejected write left its data on disk", found)
	}
	if after := f.request(t, struct{}{}).ExpectedGeneration; *after != *generation {
		t.Fatal("a rejected write changed the generation")
	}
}

func TestAbandonedTransactionsAreRemoved(t *testing.T) {
	f := newFixture(t)
	failAt := func(point string) {
		f.store.fault = func(at string) error {
			if at == point {
				return problem(8, "InjectedFailure")
			}
			return nil
		}
	}
	// Interrupted after its objects were written: the same request may still
	// commit at this generation, so its journal stays until another commits.
	abandoned := testNote("interrupted capture")
	abandoned.Body = "SYNTHETIC-ABANDONED-BODY"
	retry := f.request(t, captureInput{Note: &abandoned})
	failAt("objects")
	if response, code := f.store.Execute(context.Background(), "capture", retry); code != 8 {
		t.Fatal("fixture did not interrupt the write", response)
	}
	f.store.fault = nil
	if len(transactionFiles(t, f)) != 1 || len(filesContaining(t, f, abandoned.Body)) == 0 {
		t.Fatal("fixture left no interrupted attempt")
	}
	f.note(t, testNote("next generation"))
	if len(transactionFiles(t, f)) != 1 {
		t.Fatal("a journal that could still commit was removed")
	}
	f.note(t, testNote("later generation"))
	if names := transactionFiles(t, f); len(names) != 0 {
		t.Fatal("abandoned journal was kept", names)
	}
	if found := filesContaining(t, f, abandoned.Body); len(found) != 0 {
		t.Fatal("abandoned objects were kept", found)
	}
	if response, code := f.store.Execute(context.Background(), "capture", retry); code != 3 {
		t.Fatal("an abandoned request committed at a later generation", response)
	}

	// Interrupted before its first object: the object's directories may not
	// exist, which must not keep the journal.
	observation := Observation{SourceKind: "fixture", SourceRef: "interrupted-observation", Outcome: "failure"}
	failAt("transaction")
	if response, code := f.store.Execute(context.Background(), "capture", f.request(t, captureInput{Observation: &observation})); code != 8 {
		t.Fatal("fixture did not interrupt the write", response)
	}
	f.store.fault = nil
	if _, err := os.Stat(filepath.Join(f.store.Config.Root, "objects", "obs")); !os.IsNotExist(err) {
		t.Fatal("fixture expected no observation directory", err)
	}
	f.note(t, testNote("next generation after the observation"))
	f.note(t, testNote("later generation after the observation"))
	if names := transactionFiles(t, f); len(names) != 0 {
		t.Fatal("journal without objects was kept", names)
	}

	// Interrupted after HEAD moved: the write committed, so only the journal
	// goes and a retry still returns the receipt.
	committed := testNote("committed before interruption")
	request := f.request(t, captureInput{Note: &committed})
	failAt("head")
	if response, code := f.store.Execute(context.Background(), "capture", request); code != 8 || response.Reason != "CommitOutcomeUnknown" {
		t.Fatal("fixture did not interrupt after the commit", response)
	}
	f.store.fault = nil
	f.note(t, testNote("after the interruption"))
	if names := transactionFiles(t, f); len(names) != 0 {
		t.Fatal("committed journal was kept", names)
	}
	replay, code := f.store.Execute(context.Background(), "capture", request)
	if code != 0 || !replay.Data.(writeResult).Saved {
		t.Fatal("committed request lost its receipt", replay)
	}
	query := f.call(t, "query", Query{QueryTerms: []string{committed.Title}}).Data.(QueryResult)
	if len(query.Hits) != 1 || query.Hits[0].Ref != replay.Data.(writeResult).Refs[0] {
		t.Fatal("removing the journal removed committed data", query)
	}
}

// Earlier versions checked the manifest size only after writing the journal
// and objects, so rejected attempts stayed on disk at the current generation
// until the root reached its high-water mark and every write failed.
func TestOversizedJournalsDoNotBlockLaterWrites(t *testing.T) {
	f := newFixture(t)
	root := f.store.Config.Root
	v, err := f.store.open()
	if err != nil {
		t.Fatal(err)
	}
	m := v.m
	v.fs.close()
	ref := Ref{ID: newID("note"), Revision: 1, Digest: digest([]byte("SYNTHETIC-STUCK-BODY"))}
	name, err := objectPath(ref)
	if err != nil {
		t.Fatal(err)
	}
	m.Generation++
	m.Objects[ref.ID] = ref
	for {
		data, _ := encode(m)
		if len(data) > MaxManifest {
			break
		}
		for i := 0; i < 256; i++ {
			m.Tombstones[newID("note")] = true
		}
	}
	tx := transaction{RequestDigest: digest([]byte("stuck")), ExpectedGeneration: m.Generation - 1, Manifest: m,
		Files: []pendingFile{{name, []byte("SYNTHETIC-STUCK-BODY")}}, Result: writeResult{Saved: true, Refs: []Ref{ref}}}
	data, err := encode(tx)
	if err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(root, "transactions", strings.TrimPrefix(digest([]byte("stuck")), "sha256:")+".json")
	if err := os.MkdirAll(filepath.Dir(journal), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journal, data, 0600); err != nil {
		t.Fatal(err)
	}
	// The attempt's object fills the root up to the high-water mark.
	object := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(object), 0700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(object, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(HighWaterBytes); err != nil {
		t.Fatal(err)
	}
	file.Close()
	f.note(t, testNote("written after the stuck attempt"))
	if names := transactionFiles(t, f); len(names) != 0 {
		t.Fatal("oversized journal was kept", names)
	}
	if _, err := os.Stat(filepath.Dir(object)); !os.IsNotExist(err) {
		t.Fatal("oversized attempt's object was kept", err)
	}
}

// A helper killed while writing its journal leaves a journal that cannot be
// decoded. After history has been compacted, history collection reads every
// journal, so such a journal used to reject every later write.
func TestTornJournalDoesNotBlockWrites(t *testing.T) {
	f := newFixture(t)
	if _, _, err := f.store.write(context.Background(), f.request(t, struct{}{}), "fixture-compacted", false, func(v *snapshot) (writeResult, error) {
		v.m.HistoryCompacted = true
		return writeResult{Refs: []Ref{}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(f.store.Config.Root, "transactions", strings.Repeat("a", 64)+".json")
	if err := os.MkdirAll(filepath.Dir(journal), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journal, []byte(`{"requestDigest":"sha256:`), 0600); err != nil {
		t.Fatal(err)
	}
	f.note(t, testNote("written after a torn journal"))
	if names := transactionFiles(t, f); len(names) != 0 {
		t.Fatal("torn journal was kept", names)
	}
}
