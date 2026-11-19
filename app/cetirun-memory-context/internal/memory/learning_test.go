// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// learningPlaybook returns a playbook candidate whose prospective sample
// (cases 2 to 33) has exactly want cases.
func learningPlaybook(t *testing.T, b NativeBinding, observation string, want int, tag string) Procedure {
	t.Helper()
	proc := Procedure{Kind: "playbook", Scope: Scope{"project": b.CWD}, Applicability: []string{"Only artifact-contract-v1 synthetic records"},
		Exclusions: []string{"Live data and external actions"}, SourceObservationIDs: []string{observation}, Hypothesis: "Read each field before returning JSON.",
		Compatibility: map[string]string{"helperABI": "1", "adapter": AdapterVersion, "model": b.Model, "nativeVersion": b.NativeVersion, "modelRevision": b.ModelRevision}}
	for seed := 0; seed < 20000; seed++ {
		proc.Payload, _ = encode(Playbook{Steps: []string{fmt.Sprintf("Read fields; %s variant %d.", tag, seed)}, ExpectedEvidence: []string{"Exact JSON result"}, StopConditions: []string{"Unknown schema"}})
		count := 0
		for i := 2; i < 34; i++ {
			if strings.HasSuffix(digest([]byte(fmt.Sprintf("%d/%s", i, procedureFingerprint(proc)))), "0") {
				count++
			}
		}
		if want < 0 || count == want {
			return proc
		}
	}
	t.Fatal("no sampling fixture")
	return proc
}

// evaluate reserves and records the next fixture call of ref. Baselines fail
// and candidates pass when the call completes.
func evaluate(t *testing.T, f fixture, b NativeBinding, ref Ref, outcome string) (Response, *artifactTicket) {
	t.Helper()
	next, code := f.store.NativeEvent(context.Background(), nativeEvent(b, "evaluation-next", "", refInput{Ref: ref}), "canonical")
	ticket, ok := next.Data.(*artifactTicket)
	if code != 0 || !ok {
		return next, nil
	}
	_, expected, _ := artifactCase(ticket.Call.Case)
	output := string(expected)
	if ticket.Call.Variant == "baseline" {
		output = `{}`
	}
	if outcome != "completed" {
		output = ""
	}
	f.native(t, b, "evaluation-result", "", artifactRecord{ID: ticket.Call.ID, Output: output, Outcome: outcome, Model: b.Model, Requests: 1})
	return next, ticket
}

func procedureState(t *testing.T, f fixture, ref Ref) (methodState, slot) {
	t.Helper()
	v, err := f.store.open()
	if err != nil {
		t.Fatal(err)
	}
	defer v.fs.close()
	var proc Procedure
	if err := v.read(ref, &proc); err != nil {
		t.Fatal(err)
	}
	return v.m.ProcedureStates[ref.Key()], v.m.Slots[artifactSlot(proc.Scope)]
}

func TestIncompleteSelectionRejectsTheCandidate(t *testing.T) {
	for _, outcome := range []string{"unknown", "error", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			f := newFixture(t)
			b := nativeBinding(f)
			obs := f.native(t, b, "feedback", "", map[string]string{"tool": "fixture_tool", "category": "failure"}).Data.(writeResult).Refs[0]
			ref := f.native(t, b, "tool", "propose", proposeInput{Procedure: learningPlaybook(t, b, obs.ID, -1, "selection")}).Data.(writeResult).Refs[0]
			if _, ticket := evaluate(t, f, b, ref, outcome); ticket == nil {
				t.Fatal("no first selection ticket")
			}
			// The next reservation ends the evaluation instead of spending
			// the remaining daily requests on a candidate that cannot pass.
			next, ticket := evaluate(t, f, b, ref, "completed")
			if ticket != nil || !next.OK || next.Data.(writeResult).State != "EvaluationIncomplete" {
				t.Fatalf("incomplete selection kept dispatching: %+v", next)
			}
			if state, _ := procedureState(t, f, ref); state.State != "rejected" || state.Reason != "EvaluationIncomplete" {
				t.Fatalf("candidate state %+v", state)
			}
			v, err := f.store.open()
			if err != nil {
				t.Fatal(err)
			}
			if len(v.m.ExtraCalls) != 1 {
				t.Fatalf("%d reservations after an incomplete selection", len(v.m.ExtraCalls))
			}
			v.fs.close()
			// A new revision can be evaluated again.
			proc := learningPlaybook(t, b, obs.ID, -1, "selection")
			proc.Parent = &ref
			retry := f.native(t, b, "tool", "propose", proposeInput{Procedure: proc}).Data.(writeResult).Refs[0]
			if _, ticket := evaluate(t, f, b, retry, "completed"); ticket == nil || ticket.Call.Phase != "selection" {
				t.Fatal("new revision was not evaluated", ticket)
			}
		})
	}
}

func TestCheckRejectsACandidateWithAnIncompleteSelection(t *testing.T) {
	f := newFixture(t)
	b := nativeBinding(f)
	obs := f.native(t, b, "feedback", "", map[string]string{"tool": "fixture_tool", "category": "failure"}).Data.(writeResult).Refs[0]
	ref := f.native(t, b, "tool", "propose", proposeInput{Procedure: learningPlaybook(t, b, obs.ID, -1, "check")}).Data.(writeResult).Refs[0]
	evaluate(t, f, b, ref, "completed")
	evaluate(t, f, b, ref, "unknown")
	checked := f.native(t, b, "tool", "check", refInput{Ref: ref}).Data.(writeResult)
	if checked.State != "rejected" {
		t.Fatalf("check of an incomplete selection: %+v", checked)
	}
	if state, _ := procedureState(t, f, ref); state.Reason != "EvaluationIncomplete" {
		t.Fatalf("candidate state %+v", state)
	}
}

func TestIncompleteTrialReleasesItsSlot(t *testing.T) {
	f := newFixture(t)
	b := nativeBinding(f)
	clock := f.store.now()
	nextDay := func() {
		clock = clock.Add(25 * time.Hour)
		now := clock
		f.store.now = func() time.Time { return now }
	}
	obs := f.native(t, b, "feedback", "", map[string]string{"tool": "fixture_tool", "category": "failure"}).Data.(writeResult).Refs[0]
	ref := f.native(t, b, "tool", "propose", proposeInput{Procedure: learningPlaybook(t, b, obs.ID, 1, "trial")}).Data.(writeResult).Refs[0]
	for i := 0; i < 4; i++ {
		evaluate(t, f, b, ref, "completed")
	}
	if checked := f.native(t, b, "tool", "check", refInput{Ref: ref}).Data.(writeResult); checked.State != "checked" {
		t.Fatal(checked)
	}
	if advanced := f.native(t, b, "tool", "advance", refInput{Ref: ref}).Data.(writeResult); advanced.State != "trial" {
		t.Fatal(advanced)
	}
	nextDay()
	// The one sampled case: the baseline's call does not complete.
	evaluate(t, f, b, ref, "unknown")
	evaluate(t, f, b, ref, "completed")
	next, ticket := evaluate(t, f, b, ref, "completed")
	if ticket != nil || !next.OK || next.Data.(writeResult).State != "TrialExhausted" {
		t.Fatalf("incomplete trial did not end: %+v", next)
	}
	if _, current := procedureState(t, f, ref); current.Trial != nil {
		t.Fatal("incomplete trial kept its slot")
	}
	// Another improved candidate can start its trial.
	nextDay()
	second := f.native(t, b, "tool", "propose", proposeInput{Procedure: learningPlaybook(t, b, obs.ID, -1, "second")}).Data.(writeResult).Refs[0]
	for i := 0; i < 4; i++ {
		evaluate(t, f, b, second, "completed")
	}
	f.native(t, b, "tool", "check", refInput{Ref: second})
	if advanced := f.native(t, b, "tool", "advance", refInput{Ref: second}).Data.(writeResult); advanced.State != "trial" {
		t.Fatalf("second candidate could not start its trial: %+v", advanced)
	}
}

func contextText(t *testing.T, f fixture, b NativeBinding) string {
	t.Helper()
	return f.native(t, b, "context", "", map[string]any{}).Data.(map[string]any)["context"].(string)
}

const feedbackNudge = "Pending observed feedback is available"

func TestFeedbackReminderEndsWhenHandledOrStale(t *testing.T) {
	t.Run("propose", func(t *testing.T) {
		f := newFixture(t)
		b := nativeBinding(f)
		obs := f.native(t, b, "feedback", "", map[string]string{"tool": "fixture_tool", "category": "failure"}).Data.(writeResult).Refs[0]
		if !strings.Contains(contextText(t, f, b), feedbackNudge) {
			t.Fatal("a fresh failure was not offered for learning")
		}
		f.native(t, b, "tool", "propose", proposeInput{Procedure: learningPlaybook(t, b, obs.ID, -1, "reminder")})
		if strings.Contains(contextText(t, f, b), feedbackNudge) {
			t.Fatal("the reminder stayed after a proposal cited the observation")
		}
	})
	t.Run("dismiss", func(t *testing.T) {
		f := newFixture(t)
		b := nativeBinding(f)
		obs := f.native(t, b, "feedback", "", map[string]string{"tool": "fixture_tool", "category": "failure"}).Data.(writeResult).Refs[0]
		f.native(t, b, "tool", "commit", map[string]any{"operations": []any{}, "processedObservationIds": []string{obs.ID}})
		if strings.Contains(contextText(t, f, b), feedbackNudge) {
			t.Fatal("dismissed feedback was still offered")
		}
		if out, code := f.store.NativeEvent(context.Background(), nativeEvent(b, "tool", "commit", map[string]any{"operations": []any{}, "processedObservationIds": []string{}}), "canonical"); code != 2 {
			t.Fatal("an empty batch was accepted", out)
		}
	})
	t.Run("stale", func(t *testing.T) {
		f := newFixture(t)
		b := nativeBinding(f)
		f.native(t, b, "feedback", "", map[string]string{"tool": "fixture_tool", "category": "failure"})
		later := f.store.now().Add(feedbackReminder + time.Minute)
		f.store.now = func() time.Time { return later }
		if strings.Contains(contextText(t, f, b), feedbackNudge) {
			t.Fatal("the reminder outlived its window")
		}
		prepared := f.native(t, b, "tool", "prepare", map[string]any{}).Data.(map[string]any)["observations"].([]Observation)
		if len(prepared) != 1 {
			t.Fatal("stale feedback was not kept for prepare", prepared)
		}
	})
}

func TestBatchMayListObservationsItsProposalProcessed(t *testing.T) {
	f := newFixture(t)
	obs := Observation{SourceKind: "fixture", SourceRef: "batch-feedback", Outcome: "failure"}
	obsRef := f.call(t, "capture", captureInput{Observation: &obs}).Data.(writeResult).Refs[0]
	payload, _ := json.Marshal(RetrievalPlan{Stages: []string{"exact-title", "alias"}, RankBy: []string{"scope-match", "evidence-recency"}, MaxResults: 5})
	proc := Procedure{Kind: "retrieval-plan", Scope: Scope{"project": "fixture"}, Applicability: []string{"title and alias lookups"}, Hypothesis: "Skip unused keyword comparisons.",
		SourceObservationIDs: []string{obsRef.ID}, Compatibility: map[string]string{"helperABI": "1"}, Payload: payload}
	operation, _ := json.Marshal(map[string]any{"type": "add-procedure-version", "payload": proposeInput{Procedure: proc}})
	f.call(t, "commit", map[string]any{"operations": []json.RawMessage{operation}, "processedObservationIds": []string{obsRef.ID}})
	v, err := f.store.open()
	if err != nil {
		t.Fatal(err)
	}
	defer v.fs.close()
	if len(v.m.Pending) != 0 || !v.m.Processed[obsRef.ID] {
		t.Fatal("the batch did not process its observation")
	}
	if response, code := f.store.Execute(context.Background(), "commit", f.request(t, map[string]any{"operations": []any{}, "processedObservationIds": []string{obsRef.ID}})); code != 3 {
		t.Fatal("an already processed observation was processed again", response)
	}
}
