// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestRetrievalEvolutionAdoptsAcrossNewOpportunities(t *testing.T) {
	f := newFixture(t)
	payload, _ := json.Marshal(RetrievalPlan{Stages: []string{"exact-title", "alias"}, RankBy: []string{"scope-match", "evidence-recency"}, MaxResults: 5})
	proc := Procedure{Kind: "retrieval-plan", Scope: Scope{"project": "fixture"}, Applicability: []string{"Metadata title and alias lookups"}, Exclusions: []string{"Queries requiring keyword-only matches"}, Hypothesis: "Skip the unused keyword stage to reduce comparisons without losing a match.", Compatibility: map[string]string{"helperABI": "1"}, Payload: payload}
	fingerprint := procedureFingerprint(proc)
	prospective := []string{}
	for group := 0; group < 3; group++ {
		n := testNote(fmt.Sprintf("topic-%d", group))
		n.Aliases = []string{fmt.Sprintf("selection-%d", group)}
		for i := 0; len(n.Aliases) < 16; i++ {
			alias := fmt.Sprintf("future-%d-%d", group, i)
			n.Aliases = append(n.Aliases, alias)
		}
		// An external fixture owner chooses an opportunity that exercises the
		// trial branch; production callers cannot alter the helper assignment.
		for i := 0; ; i++ {
			alias := fmt.Sprintf("prospective-%d-%d", group, i)
			if strings.HasSuffix(digest([]byte(opportunityKey([]string{alias}, proc.Scope)+fingerprint)), "0") {
				n.Aliases[15] = alias
				prospective = append(prospective, alias)
				break
			}
		}
		f.note(t, n)
	}
	observation := Observation{SourceKind: "fixture", SourceRef: "lookup-feedback", Outcome: "failure", RedactedSummary: "Repeated lookups compare unused keyword metadata."}
	obsRef := f.call(t, "capture", captureInput{Observation: &observation}).Data.(writeResult).Refs[0]
	proc.SourceObservationIDs = []string{obsRef.ID}
	ref := f.call(t, "propose", proposeInput{Procedure: proc}).Data.(writeResult).Refs[0]
	checked := f.call(t, "check", refInput{Ref: ref}).Data.(writeResult)
	if checked.State != "checked" {
		t.Fatalf("candidate did not improve: %+v", checked)
	}
	trial := f.call(t, "advance", refInput{Ref: ref}).Data.(writeResult)
	if trial.State != "trial" {
		t.Fatal(trial)
	}
	var trialUse Ref
	for _, alias := range prospective {
		selected := f.call(t, "select", selectInput{QueryTerms: []string{alias}}).Data.(writeResult)
		if selected.State != "trial" {
			t.Fatalf("fixture did not exercise trial: %+v", selected)
		}
		q := f.call(t, "query", Query{QueryTerms: []string{alias}, SelectionRef: &selected.Refs[0]}).Data.(QueryResult)
		if len(q.Hits) != 1 || q.Stage != "alias" {
			t.Fatal(q)
		}
		trialUse = selected.Refs[0]
		request := f.request(t, Query{QueryTerms: []string{alias}, SelectionRef: &trialUse})
		request.RequestContext.Delegation = &Delegation{ParentRequestID: "parent-fixture", Purpose: "narrow read-only lookup", AllowedRefs: []Ref{trialUse, ref}, NoCapture: true}
		if response, code := f.store.Execute(context.Background(), "query", request); code != 4 {
			t.Fatalf("inherited noCapture resumed a trial: %+v", response)
		}
	}
	active := f.call(t, "advance", refInput{Ref: ref}).Data.(writeResult)
	if active.State != "active" {
		t.Fatal(active)
	}
	if response, code := f.store.Execute(context.Background(), "query", f.request(t, Query{QueryTerms: []string{prospective[2]}, SelectionRef: &trialUse})); code != 3 {
		t.Fatalf("old trial record reused after adoption: %+v", response)
	}
	// A new candidate must neither replace the active version nor prevent the
	// owner from retiring that older active version later.
	proc.Parent = &ref
	f.call(t, "propose", proposeInput{Procedure: proc})
	selected := f.call(t, "select", selectInput{QueryTerms: []string{"topic-0"}}).Data.(writeResult)
	if selected.State != "active" {
		t.Fatal("proposing a revision displaced the approved method")
	}
	activeUse := selected.Refs[0]
	note := testNote("new-keyword-source")
	note.Keywords = []string{"keyword-only"}
	f.note(t, note)
	r := f.request(t, selectInput{QueryTerms: []string{"keyword-only"}})
	r.RequestContext.NoCapture = true
	readOnly, code := f.store.Execute(context.Background(), "select", r)
	if code != 0 || readOnly.Data.(writeResult).State != "baseline" || readOnly.Generation != *r.ExpectedGeneration {
		t.Fatalf("read-only regression did not use the unchanged baseline: %+v", readOnly)
	}
	query := f.call(t, "query", Query{QueryTerms: []string{"keyword-only"}}).Data.(QueryResult)
	if len(query.Hits) != 1 || query.Stage != "keywords" {
		t.Fatalf("active regression did not fall back: %+v", query)
	}
	selected = f.call(t, "select", selectInput{QueryTerms: []string{"keyword-only"}}).Data.(writeResult)
	if selected.State != "baseline" {
		t.Fatal("regression did not quarantine the active method")
	}
	f.call(t, "query", Query{QueryTerms: []string{"keyword-only"}, SelectionRef: &selected.Refs[0]})
	if response, code := f.store.Execute(context.Background(), "query", f.request(t, Query{QueryTerms: []string{"topic-0"}, SelectionRef: &activeUse})); code != 3 {
		t.Fatalf("old active selection survived quarantine: %+v", response)
	}
	f.call(t, "retire", refInput{Ref: ref})
	r = f.request(t, selectInput{QueryTerms: []string{"topic-0"}})
	r.RequestContext.NoCapture = true
	retired, code := f.store.Execute(context.Background(), "select", r)
	if code != 0 || retired.Data.(writeResult).State != "baseline" {
		t.Fatalf("retired method reused: %+v", retired)
	}
}

func TestBaselineSelectionIsAUsableReadReceipt(t *testing.T) {
	f := newFixture(t)
	f.note(t, testNote("ordinary-lookup"))
	selected := f.call(t, "select", selectInput{QueryTerms: []string{"ordinary-lookup"}}).Data.(writeResult)
	if selected.State != "baseline" {
		t.Fatal(selected)
	}
	q := f.call(t, "query", Query{QueryTerms: []string{"ordinary-lookup"}, SelectionRef: &selected.Refs[0]}).Data.(QueryResult)
	if len(q.Hits) != 1 {
		t.Fatal(q)
	}
}

func TestCandidateCannotSetEvaluationOrActiveState(t *testing.T) {
	f := newFixture(t)
	for _, command := range []string{"propose", "advance", "check"} {
		r := f.request(t, map[string]any{"validated": true, "state": "active"})
		if response, code := f.store.Execute(context.Background(), command, r); code != 2 {
			t.Fatalf("%s accepted forged authority: %+v", command, response)
		}
	}
}

func TestProcedureLinksRequireKnownRoles(t *testing.T) {
	f := newFixture(t)
	ref := f.note(t, testNote("source"))
	v, err := f.store.open()
	if err != nil {
		t.Fatal(err)
	}
	defer v.fs.close()
	if err := v.validateLinks([]Derived{{Ref: ref, Role: "SUPPORT"}}, nil, nil, f.request(t, struct{}{})); err == nil {
		t.Fatal("an unknown derivation role bypassed support checks")
	}
}

func TestDuplicateTermsDoNotCreateAnotherLearningOpportunity(t *testing.T) {
	query, err := terms([]string{" Fixture topic ", "fixture topic", "FIXTURE TOPIC"})
	if err != nil || len(query) != 1 || query[0] != "fixture topic" {
		t.Fatal("duplicate terms did not normalize", query, err)
	}
	scope := Scope{"project": "fixture"}
	if opportunityKey(query, scope) != opportunityKey([]string{"fixture topic"}, scope) {
		t.Fatal("equivalent lookup counted as independent evidence")
	}
}
