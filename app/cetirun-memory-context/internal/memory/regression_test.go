// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNativeWriteReplayAfterGenerationChanges(t *testing.T) {
	f := newFixture(t)
	binding := nativeBinding(f)
	note := testNote("native replay")
	note.Scope = Scope{"project": binding.CWD}
	event := nativeEvent(binding, "tool", "capture", captureInput{Note: &note})
	first, code := f.store.NativeEvent(context.Background(), event, "contract")
	if code != 0 {
		t.Fatal(first)
	}
	f.native(t, binding, "feedback", "", map[string]string{"tool": "fixture", "category": "failure"})
	replayed, code := f.store.NativeEvent(context.Background(), event, "contract")
	if code != 0 || replayed.Generation != first.Generation || !reflect.DeepEqual(replayed.Data, first.Data) {
		t.Fatalf("native retry did not replay the committed result: %+v", replayed)
	}
	event.Binding.NoCapture = true
	if denied, code := f.store.NativeEvent(context.Background(), event, "contract"); code != 4 || denied.Reason != "CaptureProhibited" {
		t.Fatal("idempotent replay bypassed current restrictions", denied)
	}
}

func TestNativeReplayAcceptsLegacyGenerationBoundReceipt(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		f := newFixture(t)
		binding := nativeBinding(f)
		note := testNote("legacy native receipt")
		note.Scope = Scope{"project": binding.CWD}
		event := nativeEvent(binding, "tool", "capture", captureInput{Note: &note})
		request, err := binding.request(event)
		if err != nil {
			t.Fatal(err)
		}
		request.ExpectedGeneration = f.request(t, nil).ExpectedGeneration
		request.native = false // the old writer included this generated generation
		if interrupted {
			f.store.fault = func(point string) error {
				if point == "objects" {
					return problem(8, "InjectedFailure")
				}
				return nil
			}
		}
		_, code := f.store.Execute(context.Background(), "capture", request)
		if interrupted && code != 8 || !interrupted && code != 0 {
			t.Fatalf("legacy fixture did not reach expected state: %d", code)
		}
		f.store.fault = nil
		if replay, code := f.store.NativeEvent(context.Background(), event, "contract"); code != 0 || len(replay.Data.(writeResult).Refs) != 1 {
			t.Fatal("legacy native retry was lost", replay)
		}
	}
}

func TestMetadataRevisionPreservesSourceWarning(t *testing.T) {
	f := newFixture(t)
	note := testNote("changed source")
	ref := f.note(t, note)
	f.call(t, "record-source-check", sourceCheckInput{Check: SourceCheck{TargetRef: ref, SourceRef: note.Sources[0].Ref, Result: "changed"}})
	note.Keywords = []string{"new keyword"}
	next := f.call(t, "revise", reviseInput{Ref: ref, Note: note, Reason: "change keywords only"}).Data.(writeResult).Refs[0]
	query := f.call(t, "query", Query{QueryTerms: []string{note.Title}}).Data.(QueryResult)
	if len(query.Hits) != 1 || query.Hits[0].Ref != next || !query.Hits[0].Review.NeedsRecheck || query.Hits[0].Review.Reason != "SourceChanged" {
		t.Fatalf("revision discarded source invalidation: %+v", query)
	}
	v, err := f.store.open()
	if err != nil {
		t.Fatal(err)
	}
	defer v.fs.close()
	if len(v.m.SourceChecks[next.Key()]) != 0 {
		t.Fatal("revision rewrote the source-check index instead of deriving inherited invalidation")
	}
}

func TestContradictionRequiresReviewAtBothEnds(t *testing.T) {
	f := newFixture(t)
	a := testNote("contradiction a")
	a.Keywords = []string{"contradiction"}
	aRef := f.note(t, a)
	b := testNote("contradiction b")
	b.Keywords = []string{"contradiction"}
	b.Relations = []Relation{{Type: "contradicts", TargetRef: aRef, Reason: "synthetic disagreement"}}
	bRef := f.note(t, b)
	query := f.call(t, "query", Query{QueryTerms: []string{"contradiction"}}).Data.(QueryResult)
	if len(query.Hits) != 2 {
		t.Fatal(query)
	}
	for _, hit := range query.Hits {
		if !hit.Review.NeedsRecheck || hit.Review.Reason != "Conflicted" {
			t.Fatalf("one side escaped contradiction review: %+v", hit)
		}
	}
	a.Keywords = append(a.Keywords, "copy-edit")
	f.call(t, "revise", reviseInput{Ref: aRef, Note: a, Reason: "metadata only"})
	query = f.call(t, "query", Query{QueryTerms: []string{a.Title}}).Data.(QueryResult)
	if !query.Hits[0].Review.NeedsRecheck || query.Hits[0].Review.Reason != "Conflicted" || !reflect.DeepEqual(query.Hits[0].ConflictRefs, []Ref{bRef}) {
		t.Fatal("metadata edit escaped an incoming contradiction")
	}
}

func TestLatestNativeSourceCheckCanOnlyNarrowEarlierSuccess(t *testing.T) {
	for _, temporalClass := range []string{"live", "stable"} {
		f := newFixture(t)
		binding := nativeBinding(f)
		note := testNote("current native source")
		note.Scope, note.TemporalClass = Scope{"project": binding.CWD}, temporalClass
		if temporalClass == "live" {
			note.Revalidation.Mode = "on-use"
		}
		name := filepath.Join(f.home, "synthetic.txt")
		if err := os.WriteFile(name, []byte("original"), 0600); err != nil {
			t.Fatal(err)
		}
		version := digest([]byte("original"))
		note.Sources = []Source{{Kind: "file", Ref: name, Version: &version, Origin: "tool-observed", Assurance: "reported"}}
		ref := f.native(t, binding, "tool", "capture", captureInput{Note: &note}).Data.(writeResult).Refs[0]
		payload := map[string]any{"ref": ref, "sourceRef": name}
		good := f.native(t, binding, "tool", "source", payload).Data.(CurrentSourceCheck)
		if err := os.WriteFile(name, []byte("changed"), 0600); err != nil {
			t.Fatal(err)
		}
		bad := f.native(t, binding, "tool", "source", payload).Data.(CurrentSourceCheck)
		binding.NoCapture, binding.CurrentSourceChecks = true, []CurrentSourceCheck{good, bad}
		query := f.native(t, binding, "tool", "query", Query{QueryTerms: []string{note.Title}}).Data.(QueryResult)
		if !query.Hits[0].Review.NeedsRecheck || query.Hits[0].Review.Reason != "SourceChanged" {
			t.Fatalf("new failure was masked by earlier success for %s", temporalClass)
		}
	}
}

func TestLegacyNotesRequireExplicitImportAndRemainUnchanged(t *testing.T) {
	f := newFixture(t)
	name := filepath.Join(f.home, "memory", "legacy.md")
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	const body = "Synthetic legacy private note."
	if err := os.WriteFile(name, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if query := f.call(t, "query", Query{QueryTerms: []string{"legacy.md"}}).Data.(QueryResult); len(query.Hits) != 0 {
		t.Fatal("legacy files were imported implicitly")
	}
	plan := f.call(t, "import-plan", importInput{Files: []string{name}}).Data.(ImportPlan)
	f.call(t, "import-apply", importApplyInput{Plan: plan})
	query := f.call(t, "query", Query{QueryTerms: []string{"legacy.md"}}).Data.(QueryResult)
	if len(query.Hits) != 1 || query.Hits[0].Note.Body != body || !query.Hits[0].Review.NeedsRecheck {
		t.Fatal("explicit import lost body or asserted verification")
	}
	if data, err := os.ReadFile(name); err != nil || string(data) != body {
		t.Fatal("import changed the legacy file", err)
	}
}

func TestSingleNoteLookupHandlesJSONEscaping(t *testing.T) {
	for _, body := range []string{strings.Repeat("x\n", 12<<10), strings.Repeat("\x01", 24<<10)} {
		f := newFixture(t)
		note := testNote("large escaped note")
		note.Body = body
		f.note(t, note)
		query := f.call(t, "query", Query{QueryTerms: []string{note.Title}, Limit: 1}).Data.(QueryResult)
		if len(query.Hits) != 1 || query.Hits[0].Note.Body != note.Body || query.Partial != nil {
			t.Fatalf("admitted note cannot be retrieved intact: hits=%d partial=%+v", len(query.Hits), query.Partial)
		}
	}
}

func TestDelegationMustIncludeProcedureParent(t *testing.T) {
	f := newFixture(t)
	obs := Observation{SourceKind: "fixture", SourceRef: "delegation-fixture", Outcome: "failure"}
	obsRef := f.call(t, "capture", captureInput{Observation: &obs}).Data.(writeResult).Refs[0]
	payload, _ := json.Marshal(Baseline())
	proc := Procedure{Kind: "retrieval-plan", Scope: Scope{"project": "fixture"}, Applicability: []string{"fixture"}, Hypothesis: "bounded fixture",
		Compatibility: map[string]string{"helperABI": "1"}, Payload: payload, SourceObservationIDs: []string{obsRef.ID}}
	parent := f.call(t, "propose", proposeInput{Procedure: proc}).Data.(writeResult).Refs[0]
	proc.Parent = &parent
	r := f.request(t, proposeInput{Procedure: proc})
	r.RequestContext.Delegation = &Delegation{ParentRequestID: "parent", Purpose: "use only the observation", AllowedRefs: []Ref{obsRef}}
	if result, code := f.store.Execute(context.Background(), "propose", r); code != 4 || result.Reason != "DisclosureNotAllowed" {
		t.Fatalf("delegation replaced an ungranted procedure: %+v", result)
	}
	v, err := f.store.open()
	if err != nil {
		t.Fatal(err)
	}
	defer v.fs.close()
	if v.m.Objects[parent.ID] != parent || v.m.ProcedureStates[parent.Key()].State != "candidate" {
		t.Fatal("denied request changed the parent")
	}
}

func TestDifferentRevisionAfterAbandonedObjectWrite(t *testing.T) {
	f := newFixture(t)
	note := testNote("revision recovery")
	ref := f.note(t, note)
	note.Body = "Unpublished synthetic correction"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.store.fault = func(point string) error {
		if point == "objects" {
			cancel()
		}
		return nil
	}
	request := f.request(t, reviseInput{Ref: ref, Note: note, Reason: "interrupted change"})
	if result, code := f.store.Execute(ctx, "revise", request); code != 5 || result.Reason != "Cancelled" {
		t.Fatal(result)
	}
	f.store.fault = nil
	note.Body = "A different synthetic correction"
	changed := f.call(t, "revise", reviseInput{Ref: ref, Note: note, Reason: "new change"}).Data.(writeResult).Refs[0]
	if changed.Revision <= ref.Revision {
		t.Fatal("revision did not advance")
	}
	query := f.call(t, "query", Query{QueryTerms: []string{note.Title}}).Data.(QueryResult)
	if query.Hits[0].Note.Body != note.Body {
		t.Fatal("abandoned body was committed")
	}
}

func TestForgetPurgesLegacyAtomicTemporaryCopies(t *testing.T) {
	f := newFixture(t)
	note := testNote("forget temporary")
	note.Body = "SYNTHETIC-TEMPORARY-COPY"
	ref := f.note(t, note)
	for _, dir := range []string{"knowledge", "lessons", "procedures", "state/manifests"} {
		if err := os.MkdirAll(filepath.Join(f.store.Config.Root, dir), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.store.Config.Root, dir, ".tmp-"+newID("tx")), []byte(note.Body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	plan := f.call(t, "forget-plan", forgetInput{Refs: []Ref{ref}}).Data.(ForgetPlan)
	result := f.call(t, "forget-apply", forgetApplyInput{Plan: plan}).Data.(writeResult)
	if result.PurgePending {
		t.Fatal("purge did not finish")
	}
	err := filepath.WalkDir(f.store.Config.Root, func(name string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(name)
		if err == nil && strings.Contains(string(data), note.Body) {
			t.Errorf("forgotten body retained in %s", name)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
