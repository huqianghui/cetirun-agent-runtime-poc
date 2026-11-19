// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package memory

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestForgetRecoveryAfterCommittedInterruption(t *testing.T) {
	for _, recovery := range []string{"write", "repair-index"} {
		t.Run(recovery, func(t *testing.T) {
			f := newFixture(t)
			n := testNote("interrupted forget")
			n.Body = "SYNTHETIC-FORGOTTEN-CONTENT"
			ref := f.note(t, n)
			plan := f.call(t, "forget-plan", forgetInput{Refs: []Ref{ref}}).Data.(ForgetPlan)
			f.store.fault = func(point string) error {
				if point == "head" {
					return context.Canceled
				}
				return nil
			}
			response, code := f.store.Execute(context.Background(), "forget-apply", f.request(t, forgetApplyInput{Plan: plan}))
			if code != 8 || response.Reason != "CommitOutcomeUnknown" {
				t.Fatalf("interruption: %+v", response)
			}
			f.store = New(f.store.Config)
			if len(filesContaining(t, f, n.Body)) == 0 {
				t.Fatal("fixture did not interrupt physical deletion")
			}
			status := f.call(t, "status", struct{}{}).Data.(map[string]any)
			if status["purgePending"] != true || !slices.Contains(status["pendingPurges"].([]string), plan.Digest) {
				t.Fatal("status lost the durable pending deletion", status)
			}
			if recovery == "write" {
				f.note(t, testNote("retained after restart"))
			} else {
				f.call(t, "repair-index", struct{}{})
			}
			if found := filesContaining(t, f, n.Body); len(found) != 0 {
				t.Fatalf("forgotten content survived recovery: %v", found)
			}
			if status := f.call(t, "status", struct{}{}).Data.(map[string]any); status["purgePending"] != false {
				t.Fatal("status still reports completed purge as pending", status)
			}
		})
	}
}

func TestForgetPlanSurvivesUnrelatedTurn(t *testing.T) {
	f := newFixture(t)
	ref := f.note(t, testNote("forget after discussion"))
	plan := f.call(t, "forget-plan", forgetInput{Refs: []Ref{ref}}).Data.(ForgetPlan)
	f.call(t, "record-use", Use{})
	f.call(t, "forget-apply", forgetApplyInput{Plan: plan})
	query := f.call(t, "query", Query{QueryTerms: []string{"forget after discussion"}}).Data.(QueryResult)
	if len(query.Hits) != 0 {
		t.Fatal("forgotten note is still queryable")
	}
}

func TestCompletedForgetReplayReportsPhysicalCompletion(t *testing.T) {
	f := newFixture(t)
	ref := f.note(t, testNote("forget completion receipt"))
	plan := f.call(t, "forget-plan", forgetInput{Refs: []Ref{ref}}).Data.(ForgetPlan)
	request := f.request(t, forgetApplyInput{Plan: plan})
	response, code := f.store.Execute(context.Background(), "forget-apply", request)
	if code != 0 || response.Data.(writeResult).PurgePending {
		t.Fatal("initial forget did not complete", response)
	}
	status := f.call(t, "status", map[string]string{"lookupRequestId": request.RequestID})
	if status.Data.(map[string]any)["result"].(writeResult).PurgePending {
		t.Fatal("the completion receipt still reports a pending purge")
	}
	f.note(t, testNote("later unrelated write"))
	response, code = f.store.Execute(context.Background(), "forget-apply", request)
	if code != 0 || response.Data.(writeResult).PurgePending {
		t.Fatal("replay lost the completed purge after journal retirement", response)
	}
}

func TestRepairEditedIndex(t *testing.T) {
	f := newFixture(t)
	ref := f.note(t, testNote("repair index"))
	index := filepath.Join(f.store.Config.Root, "index.md")
	if err := os.WriteFile(index, []byte("owner's local index edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.call(t, "repair-index", struct{}{})
	backups, err := filepath.Glob(index + ".conflict-*")
	if err != nil || len(backups) != 1 {
		t.Fatal("repair did not preserve exactly one conflicting view", backups, err)
	}
	if data, err := os.ReadFile(backups[0]); err != nil || string(data) != "owner's local index edit\n" {
		t.Fatal("repair lost the owner's edit", err)
	}
	f.note(t, testNote("capture after repair"))
	query := f.call(t, "query", Query{QueryTerms: []string{"repair index"}}).Data.(QueryResult)
	if len(query.Hits) != 1 || query.Hits[0].Ref != ref {
		t.Fatal("repair changed the authoritative note")
	}
}

func TestRepairReportsViewInstallationFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("filesystem permission check requires a non-root process")
	}
	f := newFixture(t)
	f.note(t, testNote("retained synthetic note"))
	request := f.request(t, struct{}{})
	if err := os.Chmod(f.store.Config.Root, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.store.Config.Root, 0700) })
	response, code := f.store.Execute(context.Background(), "repair-index", request)
	if code == 0 || response.OK {
		t.Fatalf("repair reported success without installing its view: %+v", response)
	}
	if err := os.Chmod(f.store.Config.Root, 0700); err != nil {
		t.Fatal(err)
	}
	replayed, code := f.store.Execute(context.Background(), "repair-index", request)
	if code != 0 || !replayed.OK || replayed.Generation != response.Generation {
		t.Fatalf("repair replay did not finish the committed generation: %+v", replayed)
	}
	assertInstalledViews(t, f)
}

func TestRepairReplayInstallsCommittedView(t *testing.T) {
	f := newFixture(t)
	f.note(t, testNote("retained synthetic note"))
	index := filepath.Join(f.store.Config.Root, "index.md")
	edit := []byte("synthetic owner edit\n")
	if err := os.WriteFile(index, edit, 0600); err != nil {
		t.Fatal(err)
	}
	request := f.request(t, struct{}{})
	f.store.fault = func(point string) error {
		if point == "head" {
			return context.Canceled
		}
		return nil
	}
	first, code := f.store.Execute(context.Background(), "repair-index", request)
	if code != 8 || first.Reason != "CommitOutcomeUnknown" {
		t.Fatalf("fixture did not stop after committed HEAD: %+v", first)
	}
	f.store = New(f.store.Config)
	response, code := f.store.Execute(context.Background(), "repair-index", request)
	if code != 0 || !response.OK || response.Generation != first.Generation {
		t.Fatalf("repair replay did not finish the committed generation: %+v", response)
	}
	assertInstalledViews(t, f)
	backups, err := filepath.Glob(index + ".conflict-*")
	if err != nil || len(backups) != 1 {
		t.Fatal("replay did not preserve exactly one conflicting view", backups, err)
	}
	if data, err := os.ReadFile(backups[0]); err != nil || !bytes.Equal(data, edit) {
		t.Fatal("replay lost the owner's edit", err)
	}
	f.note(t, testNote("capture after repair replay"))
}

func assertInstalledViews(t *testing.T, f fixture) {
	t.Helper()
	v, err := f.store.open()
	if err != nil {
		t.Fatal(err)
	}
	defer v.fs.close()
	for name, expected := range v.m.Views {
		data, err := os.ReadFile(filepath.Join(f.store.Config.Root, name))
		if err != nil || digest(data) != expected.Current {
			t.Fatalf("%s does not match the committed view: %v", name, err)
		}
	}
}

func TestForgetPlanRejectsChangedContent(t *testing.T) {
	for _, change := range []string{"root", "derivative", "new-derivative"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t)
			note := testNote("source before approval")
			root := f.note(t, note)
			child := testNote("derived before approval")
			child.DerivedFrom = []Derived{{Ref: root, Role: "counterexample"}}
			ref := f.note(t, child)
			plan := f.call(t, "forget-plan", forgetInput{Refs: []Ref{root}}).Data.(ForgetPlan)
			switch change {
			case "root":
				note.Body = "New source content."
				f.call(t, "revise", reviseInput{Ref: root, Note: note, Reason: "update"})
			case "derivative":
				child.Body = "New derivative content."
				f.call(t, "revise", reviseInput{Ref: ref, Note: child, Reason: "update"})
			case "new-derivative":
				f.note(t, child)
			}
			response, code := f.store.Execute(context.Background(), "forget-apply", f.request(t, forgetApplyInput{Plan: plan}))
			if code != 3 || response.Reason != "Conflict" {
				t.Fatal("deletion expanded past the approved content", response)
			}
		})
	}
}

func TestOldAbandonedCaptureIsCollectedWithoutLosingCommittedHistory(t *testing.T) {
	f := newFixture(t)
	stage := t.TempDir()
	for _, point := range []string{"objects", "head"} {
		note := testNote(point)
		note.Body = "SYNTHETIC-AGED-" + point
		f.store.fault = func(at string) error {
			if at == point {
				return context.Canceled
			}
			return nil
		}
		response, code := f.store.Execute(context.Background(), "capture", f.request(t, captureInput{Note: &note}))
		if code == 0 {
			t.Fatal("fixture did not interrupt capture", response)
		}
		f.store.fault = nil
		for _, name := range transactionFiles(t, f) {
			if err := os.Rename(filepath.Join(f.store.Config.Root, "transactions", name), filepath.Join(stage, name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	query := f.call(t, "query", Query{QueryTerms: []string{"head"}}).Data.(QueryResult)
	note := query.Hits[0].Note
	note.Body = "The current committed revision."
	f.call(t, "revise", reviseInput{Ref: query.Hits[0].Ref, Note: note, Reason: "update committed note"})
	for i := 0; i < retainedHistory+2; i++ {
		f.call(t, "record-use", Use{OpportunityID: fmt.Sprintf("later-%d", i)})
	}
	entries, err := os.ReadDir(stage)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if err := os.Rename(filepath.Join(stage, entry.Name()), filepath.Join(f.store.Config.Root, "transactions", entry.Name())); err != nil {
			t.Fatal(err)
		}
	}
	f.note(t, testNote("trigger old journal recovery"))
	if found := filesContaining(t, f, "SYNTHETIC-AGED-objects"); len(found) != 0 {
		t.Fatal("old abandoned capture survived collection", found)
	}
	if found := filesContaining(t, f, "SYNTHETIC-AGED-head"); len(found) != 1 {
		t.Fatal("committed immutable history was removed or its journal was retained", found)
	}
}

func TestDamagedNoteDoesNotStopHealthyMemory(t *testing.T) {
	for _, operation := range []string{"query", "capture", "repair-index", "forget-apply", "context", "lint"} {
		t.Run(operation, func(t *testing.T) {
			f := newFixture(t)
			binding := nativeBinding(f)
			capture := func(title string) Ref {
				note := testNote(title)
				if operation == "context" {
					note.Scope = Scope{"project": binding.CWD}
					return f.native(t, binding, "tool", "capture", captureInput{Note: &note}).Data.(writeResult).Refs[0]
				}
				return f.note(t, note)
			}
			broken := capture("damaged note")
			healthy := capture("healthy note")
			file, err := objectPath(broken)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.store.Config.Root, file), []byte("damaged object\n"), 0600); err != nil {
				t.Fatal(err)
			}
			switch operation {
			case "query":
				query := f.call(t, "query", Query{QueryTerms: []string{"healthy note"}}).Data.(QueryResult)
				if len(query.Hits) != 1 || query.Hits[0].Ref != healthy || query.Partial == nil {
					t.Fatalf("healthy lookup must succeed and disclose partial results: %+v", query)
				}
			case "capture":
				f.note(t, testNote("new healthy note"))
			case "repair-index":
				if response := f.call(t, operation, struct{}{}); response.Partial == nil || !slices.Contains(response.Partial.Reasons, "DamagedObject:"+broken.ID) {
					t.Fatal("repair did not identify the damaged object", response)
				}
				f.note(t, testNote("healthy capture after damaged view repair"))
			case "lint":
				if response := f.call(t, operation, struct{}{}); response.Partial == nil || !slices.Contains(response.Partial.Reasons, "DamagedObject:"+broken.ID) {
					t.Fatal("lint did not identify the damaged object", response)
				}
			case "context":
				response := f.native(t, binding, "context", "", struct{}{})
				refs := response.Data.(map[string]any)["refs"].([]Ref)
				if !slices.Contains(refs, healthy) || slices.Contains(refs, broken) || response.Partial == nil {
					t.Fatal("native context did not keep only healthy notes", response)
				}
			case "forget-apply":
				plan := f.call(t, "forget-plan", forgetInput{Refs: []Ref{broken}}).Data.(ForgetPlan)
				f.call(t, operation, forgetApplyInput{Plan: plan})
			}
		})
	}
}

func TestDamagedRelatedNoteKeepsHealthyQueryWithWarning(t *testing.T) {
	f := newFixture(t)
	healthy := f.note(t, testNote("healthy related note"))
	note := testNote("damaged counterclaim")
	note.Relations = []Relation{{Type: "contradicts", TargetRef: healthy, Reason: "synthetic disagreement"}}
	broken := f.note(t, note)
	file, err := objectPath(broken)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.store.Config.Root, file), []byte("damaged\n"), 0600); err != nil {
		t.Fatal(err)
	}
	query := f.call(t, "query", Query{QueryTerms: []string{"healthy related note"}}).Data.(QueryResult)
	if len(query.Hits) != 1 || query.Hits[0].Ref != healthy || !query.Hits[0].Review.NeedsRecheck || query.Partial == nil {
		t.Fatal("damaged counterclaim suppressed the healthy note or lost its warning", query)
	}
}
