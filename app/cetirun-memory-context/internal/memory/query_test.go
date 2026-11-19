// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestQueryResultLimit(t *testing.T) {
	for _, count := range []int{5, 6} {
		t.Run(fmt.Sprintf("%d_matches", count), func(t *testing.T) {
			f := newFixture(t)
			ids := []string{}
			for i := 0; i < count; i++ {
				note := testNote(fmt.Sprintf("matching-%d", i))
				note.Keywords = []string{"shared"}
				ids = append(ids, f.note(t, note).ID)
			}
			slices.Sort(ids)
			response := f.call(t, "query", Query{QueryTerms: []string{" Shared "}})
			result := response.Data.(QueryResult)
			if len(result.Hits) != 5 || result.Stage != "keywords" || result.WorkUnits != count*3 {
				t.Fatalf("hits=%d stage=%s work=%d", len(result.Hits), result.Stage, result.WorkUnits)
			}
			for i, hit := range result.Hits {
				if hit.Ref.ID != ids[i] {
					t.Fatalf("hit %d: got %s, want %s", i, hit.Ref.ID, ids[i])
				}
			}
			if response.Partial != result.Partial {
				t.Fatal("response and query partial differ")
			}
			if count == 5 {
				if result.Partial != nil {
					t.Fatalf("exactly the result limit is complete: %+v", result.Partial)
				}
			} else {
				assertQueryPartial(t, result.Partial, 0, 1, "ResultLimit")
			}
		})
	}
}

func TestQueryOutputLimit(t *testing.T) {
	for _, count := range []int{3, 6} {
		t.Run(fmt.Sprintf("%d_matches", count), func(t *testing.T) {
			f := newFixture(t)
			for i := 0; i < count; i++ {
				note := testNote(fmt.Sprintf("large-%d", i))
				note.Keywords = []string{"shared"}
				note.Body = strings.Repeat("x", 20<<10)
				f.note(t, note)
			}
			result := f.call(t, "query", Query{QueryTerms: []string{"shared"}}).Data.(QueryResult)
			if len(result.Hits) != 1 {
				t.Fatalf("got %d hits, want 1 within the output bound", len(result.Hits))
			}
			encoded, err := encode(result.Hits[0])
			if err != nil || len(encoded) > 32<<10 {
				t.Fatalf("returned hit exceeds output bound: bytes=%d err=%v", len(encoded), err)
			}
			reasons := []string{"OutputLimit"}
			if count > Baseline().MaxResults {
				reasons = []string{"ResultLimit", "OutputLimit"}
			}
			assertQueryPartial(t, result.Partial, 0, count-1, reasons...)
		})
	}
}

func TestQueryCombinedPartial(t *testing.T) {
	for _, tc := range []struct {
		name       string
		count      int
		matches    int
		largeHits  bool
		largeOther bool
		wantHits   int
		reasons    []string
	}{
		{"result_and_scan", MaxNoteScan + 3, 6, false, false, 5, []string{"ResultLimit"}},
		{"output_and_scan", MaxNoteScan + 3, 3, true, false, 1, []string{"OutputLimit"}},
		{"result_output_and_scan", MaxNoteScan + 3, 6, true, false, 1, []string{"ResultLimit", "OutputLimit"}},
		{"output_and_scan_bytes", MaxScanBytes/(20<<10) + 10, 3, true, true, 1, []string{"OutputLimit"}},
		{"scan_only", MaxNoteScan + 3, 5, false, false, 5, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			notes := []Note{}
			for i := 0; i < tc.count; i++ {
				note := testNote(fmt.Sprintf("bounded-%03d", i))
				if i < tc.matches {
					note.Keywords = []string{"shared"}
				}
				if i < tc.matches && tc.largeHits || i >= tc.matches && tc.largeOther {
					// Keep the byte bound without making each fixture a giant lexical token.
					note.Body = strings.Repeat("note ", 4<<10)
				}
				notes = append(notes, note)
			}
			f, v := newQuerySnapshot(t, notes)
			r := f.request(t, Query{QueryTerms: []string{"shared"}})
			corpus, partial, err := v.corpus(r)
			if err != nil || partial == nil || partial.Unscanned != tc.count-len(corpus) || partial.Unscanned <= 0 {
				t.Fatalf("fixture did not hit corpus bound: scanned=%d partial=%+v err=%v", len(corpus), partial, err)
			}
			if !tc.largeOther && len(corpus) != MaxNoteScan || tc.largeOther && len(corpus) >= MaxNoteScan {
				t.Fatalf("fixture hit the wrong corpus bound: scanned=%d", len(corpus))
			}
			result, err := f.store.query(context.Background(), v, r)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Hits) != tc.wantHits || result.Stage != "keywords" {
				t.Fatalf("hits=%d stage=%s, want %d keyword hits", len(result.Hits), result.Stage, tc.wantHits)
			}
			assertQueryPartial(t, result.Partial, partial.Unscanned, tc.matches-tc.wantHits, tc.reasons...)
		})
	}
}

func TestQueryIgnoresIrrelevantRecords(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 5; i++ {
		note := testNote(fmt.Sprintf("matching-%d", i))
		note.Keywords = []string{"shared"}
		f.note(t, note)
	}
	for i := 0; i < 6; i++ {
		note := testNote(fmt.Sprintf("other-scope-%d", i))
		note.Scope["topic"] = "other"
		note.Keywords = []string{"shared"}
		f.note(t, note)
	}
	note := testNote("shared substring is not an exact match")
	note.Keywords = []string{"shared-prefix"}
	note.Body = "shared"
	f.note(t, note)
	result := f.call(t, "query", Query{QueryTerms: []string{"shared"}}).Data.(QueryResult)
	if len(result.Hits) != 5 || result.Partial != nil {
		t.Fatalf("irrelevant records affected completeness: hits=%d partial=%+v", len(result.Hits), result.Partial)
	}
	for _, hit := range result.Hits {
		if !strings.HasPrefix(hit.Note.Title, "matching-") {
			t.Fatalf("returned irrelevant note: %s", hit.Note.Title)
		}
	}
	result = f.call(t, "query", Query{QueryTerms: []string{"absent"}}).Data.(QueryResult)
	if len(result.Hits) != 0 || result.Stage != "none" || result.Partial != nil {
		t.Fatalf("no-match query is not complete: hits=%d stage=%s partial=%+v", len(result.Hits), result.Stage, result.Partial)
	}
}

func TestQueryOnlyCountsAllowedMatches(t *testing.T) {
	f := newFixture(t)
	refs := []Ref{}
	for i := 0; i < 7; i++ {
		note := testNote(fmt.Sprintf("matching-%d", i))
		note.Keywords = []string{"shared"}
		refs = append(refs, f.note(t, note))
	}
	for _, allowed := range []int{5, 6} {
		t.Run(fmt.Sprintf("%d_allowed", allowed), func(t *testing.T) {
			r := f.request(t, Query{QueryTerms: []string{"shared"}})
			r.RequestContext.Delegation = &Delegation{ParentRequestID: "parent-fixture", Purpose: "scoped lookup", AllowedRefs: refs[:allowed], NoCapture: true}
			response, code := f.store.Execute(context.Background(), "query", r)
			if code != 0 {
				t.Fatalf("query: code=%d reason=%s", code, response.Reason)
			}
			result := response.Data.(QueryResult)
			if len(result.Hits) != 5 {
				t.Fatalf("got %d hits, want 5", len(result.Hits))
			}
			for _, hit := range result.Hits {
				if !slices.Contains(refs[:allowed], hit.Ref) {
					t.Fatal("returned a note outside the delegation")
				}
			}
			if allowed == 5 {
				if result.Partial != nil {
					t.Fatalf("unauthorized matches counted as omitted: %+v", result.Partial)
				}
			} else {
				assertQueryPartial(t, result.Partial, 0, 1, "ResultLimit")
			}
		})
	}
}

func TestQueryActivePlanLimitAndFallback(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprintf("fallback_%t", fallback), func(t *testing.T) {
			notes := []Note{}
			for i := 0; i < 6; i++ {
				note := testNote(fmt.Sprintf("matching-%d", i))
				note.Keywords = []string{"shared"}
				notes = append(notes, note)
			}
			f, v := newQuerySnapshot(t, notes)
			plan := Baseline()
			plan.MaxResults = 2
			wantHits := 2
			if fallback {
				plan.Stages = []string{"exact-title"}
				wantHits = 5
			}
			payload, err := encode(plan)
			if err != nil {
				t.Fatal(err)
			}
			proc := Procedure{ID: fmt.Sprintf("proc-%032x", 1), Revision: 1, Kind: "retrieval-plan", Scope: Scope{"project": "fixture"}, Compatibility: map[string]string{"helperABI": "1"}, Payload: payload}
			ref, err := v.save(proc.ID, proc.Revision, proc)
			if err != nil {
				t.Fatal(err)
			}
			v.m.ProcedureStates[ref.Key()] = methodState{State: "active"}
			v.m.Slots[scopeKey(proc.Scope)] = slot{Active: &ref}
			result, err := f.store.query(context.Background(), v, f.request(t, Query{QueryTerms: []string{"shared"}}))
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Hits) != wantHits || result.Stage != "keywords" || result.WorkUnits != 18 {
				t.Fatalf("hits=%d stage=%s work=%d, want %d keyword hits and 18 work units", len(result.Hits), result.Stage, result.WorkUnits, wantHits)
			}
			assertQueryPartial(t, result.Partial, 0, 6-wantHits, "ResultLimit")
		})
	}
}

func TestRankRetainsResultCap(t *testing.T) {
	corpus := []corpusNote{}
	for i := 6; i > 0; i-- {
		note := testNote(fmt.Sprintf("matching-%d", i))
		note.Keywords = []string{"shared"}
		corpus = append(corpus, corpusNote{Ref: Ref{ID: fmt.Sprintf("note-%032x", i)}, Note: note})
	}
	for _, limit := range []int{2, 5} {
		plan := Baseline()
		plan.MaxResults = limit
		hits, stage, work := rank(plan, corpus, []string{"shared"}, Scope{"project": "fixture"})
		if len(hits) != limit || stage != "keywords" || work != 18 {
			t.Fatalf("limit=%d: hits=%d stage=%s work=%d", limit, len(hits), stage, work)
		}
		for i, hit := range hits {
			if hit.Ref.ID != fmt.Sprintf("note-%032x", i+1) {
				t.Fatalf("rank order changed: hit %d is %s", i, hit.Ref.ID)
			}
		}
	}
}

func newQuerySnapshot(t *testing.T, notes []Note) (fixture, *snapshot) {
	t.Helper()
	f := newFixture(t)
	v, err := f.store.open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.fs.close() })
	for i, note := range notes {
		if err := note.validate(); err != nil {
			t.Fatal(err)
		}
		note.ID = fmt.Sprintf("note-%032x", i+1)
		note.Revision = 1
		note.UpdatedAt = timestamp(f.store.now())
		if _, err := v.save(note.ID, note.Revision, note); err != nil {
			t.Fatal(err)
		}
	}
	return f, v
}

func assertQueryPartial(t *testing.T, partial *Partial, unscanned, omitted int, reasons ...string) {
	t.Helper()
	if partial == nil {
		t.Fatal("partial is nil despite omitted matches")
	}
	if !partial.LimitHit || partial.Unscanned != unscanned || !slices.Equal(partial.Reasons, reasons) {
		t.Errorf("partial=%+v, want limitHit=true unscanned=%d reasons=%v", partial, unscanned, reasons)
	}
	encoded, err := json.Marshal(partial)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Omitted *int `json:"omitted"`
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if omitted == 0 {
		if wire.Omitted != nil {
			t.Errorf("zero omitted count should be absent: %s", encoded)
		}
	} else if wire.Omitted == nil || *wire.Omitted != omitted {
		t.Errorf("partial JSON=%s, want omitted=%d", encoded, omitted)
	}
}
