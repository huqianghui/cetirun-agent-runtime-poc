// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConcurrentInitializationOfSameRequest(t *testing.T) {
	for attempt := 0; attempt < 12; attempt++ {
		s := New(Config{Root: filepath.Join(t.TempDir(), "memory")})
		zero := int64(0)
		r := Request{ContractVersion: ContractVersion, RequestID: "same-initialization", ExpectedGeneration: &zero,
			RequestContext: RequestContext{Audience: "owner-private", Destination: Destination{Kind: "owner-private"}, Scope: Scope{"project": "fixture"}, OpportunityID: "fixture-task"}, Payload: json.RawMessage(`{}`)}
		start := make(chan struct{})
		results := make(chan Response, 8)
		var workers sync.WaitGroup
		for worker := 0; worker < cap(results); worker++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-start
				response, _ := s.Execute(context.Background(), "init", r)
				results <- response
			}()
		}
		close(start)
		workers.Wait()
		close(results)
		for result := range results {
			if !result.OK || result.Generation != 1 {
				t.Fatalf("same init request was not idempotent: %+v", result)
			}
		}
	}
}

func TestNativeLookupBookkeepingDoesNotExhaustWrites(t *testing.T) {
	f := newFixture(t)
	binding := nativeBinding(f)
	note := testNote("long-lived lookup")
	note.Scope = Scope{"project": binding.CWD}
	ref := f.native(t, binding, "tool", "capture", captureInput{Note: &note}).Data.(writeResult).Refs[0]
	pinned := f.native(t, binding, "injected", "", map[string]any{"refs": []Ref{ref}}).Data.(writeResult).Refs[0]
	dependent := testNote("retained provenance")
	dependent.Scope, dependent.DerivedFrom = note.Scope, []Derived{{Ref: pinned, Role: "context"}}
	f.native(t, binding, "tool", "capture", captureInput{Note: &dependent})
	usage := func() (int64, int) {
		v, err := f.store.open()
		if err != nil {
			t.Fatal(err)
		}
		_, size, err := v.fs.inventory()
		v.fs.close()
		if err != nil {
			t.Fatal(err)
		}
		entries := 0
		if err := filepath.WalkDir(f.store.Config.Root, func(_ string, _ fs.DirEntry, err error) error {
			entries++
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return size, entries
	}
	var midwayBytes int64
	var midwayEntries int
	var last NativeEvent
	var saved Response
	for i := 0; i < 400; i++ {
		query := f.native(t, binding, "tool", "query", Query{QueryTerms: []string{note.Title}}).Data.(QueryResult)
		if len(query.Hits) != 1 || query.Hits[0].Ref != ref {
			t.Fatal("repeated lookup lost its note", query)
		}
		last = nativeEvent(binding, "injected", "", map[string]any{"refs": []Ref{ref}})
		var code int
		saved, code = f.store.NativeEvent(context.Background(), last, "contract")
		if code != 0 {
			t.Fatalf("lookup %d exhausted bookkeeping: %+v", i+1, saved)
		}
		if i == 199 {
			midwayBytes, midwayEntries = usage()
		}
	}
	if size, entries := usage(); size > midwayBytes+(32<<10) || entries > midwayEntries+4 {
		t.Fatalf("bookkeeping kept growing: bytes %d -> %d; entries %d -> %d", midwayBytes, size, midwayEntries, entries)
	}
	replay, code := f.store.NativeEvent(context.Background(), last, "contract")
	if code != 0 || replay.Generation != saved.Generation || !reflect.DeepEqual(replay.Data, saved.Data) {
		t.Fatal("recent injection could not replay", replay)
	}
	other := testNote("captured after many reads")
	other.Scope = note.Scope
	f.native(t, binding, "tool", "capture", captureInput{Note: &other})
	note.Body = "Updated after many lookups."
	f.native(t, binding, "tool", "revise", reviseInput{Ref: ref, Note: note, Reason: "fixture correction"})
	query := f.native(t, binding, "tool", "query", Query{QueryTerms: []string{note.Title}}).Data.(QueryResult)
	if len(query.Hits) != 1 || query.Hits[0].Note.Body != note.Body {
		t.Fatal("later revision was not retrievable", query)
	}
	trace := f.native(t, binding, "tool", "trace", refInput{Ref: ref}).Data.(traceResult)
	if !trace.Partial.LimitHit || !slices.Contains(trace.Partial.Reasons, "HistoryCompacted") {
		t.Fatal("trace hid its retention limit", trace)
	}
	v, err := f.store.open()
	if err != nil {
		t.Fatal(err)
	}
	if v.m.Objects[pinned.ID] != pinned {
		t.Fatal("compaction removed live provenance")
	}
	var use Use
	if err := v.read(pinned, &use); err != nil {
		t.Fatal("compaction deleted pinned history", err)
	}
	v.fs.close()
	readOnly := binding
	readOnly.NoCapture = true
	generation := f.native(t, readOnly, "tool", "status", struct{}{}).Generation
	f.native(t, readOnly, "tool", "query", Query{QueryTerms: []string{note.Title}})
	if after := f.native(t, readOnly, "tool", "status", struct{}{}).Generation; after != generation {
		t.Fatal("read-only lookup performed retention writes")
	}

	f.store.fault = func(point string) error {
		if point == "head" {
			return problem(8, "InjectedFailure")
		}
		return nil
	}
	other.Title = "interrupted after compaction"
	interrupted := nativeEvent(binding, "tool", "capture", captureInput{Note: &other})
	if result, code := f.store.NativeEvent(context.Background(), interrupted, "contract"); code != 8 || result.Reason != "CommitOutcomeUnknown" {
		t.Fatal("fixture did not interrupt a committed write", result)
	}
	f.store.fault = nil
	if result, code := f.store.NativeEvent(context.Background(), interrupted, "contract"); code != 0 || !result.Data.(writeResult).Saved {
		t.Fatal("compaction broke unknown-commit recovery", result)
	}
}

func TestInitializationPreservesUnknownRoot(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "owner-file")
	if err := os.WriteFile(marker, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	f := fixture{store: New(Config{Root: root})}
	if result, code := f.store.Execute(context.Background(), "init", f.request(t, struct{}{})); code != 6 || result.Reason != "RootInvalid" {
		t.Fatal("initialization claimed an unknown directory", result)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "owner-file" {
		t.Fatal("initialization changed the unknown directory", entries, err)
	}
}

func TestReadSnapshotPinsFilesUntilClosed(t *testing.T) {
	f := newFixture(t)
	v, err := f.store.openRead(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	note := testNote("writer waits for reader")
	r := f.request(t, captureInput{Note: &note})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	result, code := f.store.Execute(ctx, "capture", r)
	v.fs.close()
	if code != 5 || result.Reason != "Cancelled" {
		t.Fatal("writer bypassed a pinned read snapshot", result)
	}
	f.call(t, "capture", captureInput{Note: &note})
}

func TestCorpusChangesReleaseRetrievalTrial(t *testing.T) {
	for _, mutation := range []string{"capture", "revise"} {
		for _, replacement := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/replacement=%t", mutation, replacement), func(t *testing.T) {
				f := newFixture(t)
				var first Ref
				for i := 0; i < 3; i++ {
					note := testNote(fmt.Sprintf("trial-topic-%d", i))
					note.Aliases = []string{fmt.Sprintf("trial-alias-%d", i)}
					ref := f.note(t, note)
					if i == 0 {
						first = ref
					}
				}
				obs := Observation{SourceKind: "fixture", SourceRef: "trial-feedback", Outcome: "failure"}
				obsRef := f.call(t, "capture", captureInput{Observation: &obs}).Data.(writeResult).Refs[0]
				payload, _ := encode(RetrievalPlan{Stages: []string{"exact-title", "alias"}, RankBy: []string{"scope-match", "evidence-recency"}, MaxResults: 5})
				proc := Procedure{Kind: "retrieval-plan", Scope: Scope{"project": "fixture"}, Applicability: []string{"title and alias lookups"}, Hypothesis: "Skip unused keyword comparisons.", SourceObservationIDs: []string{obsRef.ID}, Compatibility: map[string]string{"helperABI": "1"}, Payload: payload}
				ref := f.call(t, "propose", proposeInput{Procedure: proc}).Data.(writeResult).Refs[0]
				if f.call(t, "check", refInput{Ref: ref}).Data.(writeResult).State != "checked" || f.call(t, "advance", refInput{Ref: ref}).Data.(writeResult).State != "trial" {
					t.Fatal("fixture did not enter a trial")
				}
				if mutation == "capture" {
					f.note(t, testNote("new trial topic"))
				} else {
					note := testNote("trial-topic-0")
					note.Aliases = []string{"trial-alias-0"}
					note.Body = "Updated synthetic note."
					f.call(t, "revise", reviseInput{Ref: first, Note: note, Reason: "fixture correction"})
				}
				selected := f.call(t, "select", selectInput{QueryTerms: []string{"trial-topic-0"}}).Data.(writeResult)
				if selected.State != "baseline" {
					t.Fatal("stale trial displaced the baseline", selected)
				}
				if replacement {
					ref = f.call(t, "propose", proposeInput{Procedure: proc}).Data.(writeResult).Refs[0]
				}
				if f.call(t, "check", refInput{Ref: ref}).Data.(writeResult).State != "checked" || f.call(t, "advance", refInput{Ref: ref}).Data.(writeResult).State != "trial" {
					t.Fatal("fresh evaluation could not restart the trial")
				}
			})
		}
	}
}

func TestExpiredEvaluationCallsDoNotImposeLifetimeBudget(t *testing.T) {
	f := newFixture(t)
	binding := nativeBinding(f)
	_, ref := fixturePlaybook(t, f, binding)
	_, _, err := f.store.write(context.Background(), f.request(t, struct{}{}), "fixture-history", false, func(v *snapshot) (writeResult, error) {
		for i := 0; i < 144; i++ {
			id := newID("call")
			v.m.ExtraCalls[id] = ModelCall{ID: id, ReservedAt: timestamp(f.store.now().Add(-25 * time.Hour)), Finished: true, Outcome: "forgotten", ProfileRevision: artifactRevision, CaseSetDigest: artifactSetDigest(), Case: i % 66}
		}
		return writeResult{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ticket := f.native(t, binding, "evaluation-next", "", refInput{Ref: ref}).Data.(*artifactTicket)
	if ticket.Call.Phase != "selection" || !strings.HasPrefix(ticket.Call.ID, "call-") {
		t.Fatal("expired history prevented a new reservation", ticket)
	}
	v, err := f.store.open()
	if err != nil {
		t.Fatal(err)
	}
	defer v.fs.close()
	if len(v.m.ExtraCalls) != 1 || !v.artifactExposed(ref, artifactSetDigest(), 3) {
		t.Fatal("expired records were retained or fixture exposure was lost")
	}
}

func fixturePlaybook(t *testing.T, f fixture, binding NativeBinding) (Procedure, Ref) {
	t.Helper()
	obs := f.native(t, binding, "feedback", "", map[string]string{"tool": "fixture", "category": "failure"}).Data.(writeResult).Refs[0]
	payload, _ := encode(Playbook{Steps: []string{"Read the fixture fields."}, ExpectedEvidence: []string{"JSON"}, StopConditions: []string{"Unknown schema"}})
	proc := Procedure{Kind: "playbook", Scope: Scope{"project": binding.CWD}, Applicability: []string{"synthetic records"}, Hypothesis: "Check fixture fields.", SourceObservationIDs: []string{obs.ID}, Payload: payload,
		Compatibility: map[string]string{"helperABI": "1", "adapter": AdapterVersion, "model": binding.Model, "nativeVersion": binding.NativeVersion, "modelRevision": binding.ModelRevision}}
	ref := f.native(t, binding, "tool", "propose", proposeInput{Procedure: proc}).Data.(writeResult).Refs[0]
	return proc, ref
}

func TestEvaluationExpiryPreservesAttemptsAndDailyBudget(t *testing.T) {
	f := newFixture(t)
	binding := nativeBinding(f)
	proc, first := fixturePlaybook(t, f, binding)
	for i := 0; i < MaxExtraTurnsPer24h; i++ {
		f.native(t, binding, "evaluation-next", "", refInput{Ref: first})
	}
	second := f.native(t, binding, "tool", "propose", proposeInput{Procedure: proc}).Data.(writeResult).Refs[0]
	if result, code := f.store.NativeEvent(context.Background(), nativeEvent(binding, "evaluation-next", "", refInput{Ref: second}), "contract"); code != 5 || result.Reason != "BudgetExhausted" {
		t.Fatal("unfinished calls escaped the daily budget", result)
	}
	nextDay := f.store.now().Add(25 * time.Hour)
	f.store.now = func() time.Time { return nextDay }
	f.native(t, binding, "evaluation-next", "", refInput{Ref: second})
	// The expired calls count as unknown results: the candidate's selection can
	// no longer complete, so it ends without dispatching the case again.
	result, code := f.store.NativeEvent(context.Background(), nativeEvent(binding, "evaluation-next", "", refInput{Ref: first}), "contract")
	if ended, ok := result.Data.(writeResult); code != 0 || !ok || ended.State != "EvaluationIncomplete" {
		t.Fatal("expired unknown dispatch was repeated", result)
	}
	v, err := f.store.open()
	if err != nil {
		t.Fatal(err)
	}
	if len(v.m.ExtraCalls) != 1 || v.m.ProcedureStates[first.Key()].ArtifactEvidence == nil {
		t.Fatal("expired attempts were not compacted")
	}
	v.fs.close()
}

func TestForgottenCompactedEvaluationKeepsFixtureExposure(t *testing.T) {
	f := newFixture(t)
	binding := nativeBinding(f)
	_, ref := fixturePlaybook(t, f, binding)
	_, _, err := f.store.write(context.Background(), f.request(t, struct{}{}), "fixture-exposure", false, func(v *snapshot) (writeResult, error) {
		call := ModelCall{ID: newID("call"), Candidate: ref, CaseSetDigest: artifactSetDigest(), ProfileRevision: artifactRevision, Phase: "prospective", Variant: "candidate", Case: 3, ReservedAt: timestamp(f.store.now().Add(-25 * time.Hour))}
		v.m.ExtraCalls[call.ID] = call
		return writeResult{}, v.compactArtifactCalls(f.store.now())
	})
	if err != nil {
		t.Fatal(err)
	}
	plan := f.call(t, "forget-plan", forgetInput{Refs: []Ref{ref}}).Data.(ForgetPlan)
	f.call(t, "forget-apply", forgetApplyInput{Plan: plan})
	v, err := f.store.open()
	if err != nil {
		t.Fatal(err)
	}
	defer v.fs.close()
	if !v.artifactExposed(ref, artifactSetDigest(), 3) || v.m.ArtifactExposure.Owners[3] != "*" || v.m.ProcedureStates[ref.Key()].ArtifactEvidence != nil {
		t.Fatal("forgetting reset exposure or retained private evaluation state")
	}
}
