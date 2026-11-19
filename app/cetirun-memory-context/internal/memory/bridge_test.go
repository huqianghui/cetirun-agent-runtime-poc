// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func nativeBinding(f fixture) NativeBinding {
	return NativeBinding{Adapter: AdapterVersion, Entrypoint: "custom", NativeVersion: "fixture/1", SessionID: "fixture-session", OpportunityID: "fixture-task", CWD: f.home, Audience: "owner-private", Destination: Destination{Kind: "owner-private"}, Model: "fixture/model", ModelRevision: digest([]byte("fixture-route")), ModelEvaluation: true}
}
func nativeEvent(b NativeBinding, event, command string, payload any) NativeEvent {
	raw, _ := json.Marshal(payload)
	return NativeEvent{Binding: b, EventID: newID("event"), Event: event, Command: command, Payload: raw}
}
func (f fixture) native(t *testing.T, b NativeBinding, event, command string, payload any) Response {
	t.Helper()
	out, code := f.store.NativeEvent(context.Background(), nativeEvent(b, event, command, payload), "canonical")
	if code != 0 {
		t.Fatalf("%s/%s: %+v", event, command, out)
	}
	return out
}

func TestNativeContextSuppliesScopeAndRestrictions(t *testing.T) {
	for _, initialized := range []bool{false, true} {
		for _, noCapture := range []bool{false, true} {
			t.Run(fmt.Sprintf("initialized=%t/noCapture=%t", initialized, noCapture), func(t *testing.T) {
				home := t.TempDir()
				root := filepath.Join(home, ".cetirun", "memory")
				f := fixture{store: New(Config{Root: root}), home: home}
				f.store.now = func() time.Time { return time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC) }
				b := nativeBinding(f)
				if initialized {
					note := testNote("existing scoped note")
					note.Scope = Scope{"project": b.CWD}
					f.native(t, b, "tool", "capture", captureInput{Note: &note})
				}
				b.NoCapture = noCapture
				out := f.native(t, b, "context", "", map[string]any{})
				data, ok := out.Data.(map[string]any)
				if !ok {
					t.Fatalf("unexpected context data: %T", out.Data)
				}
				text, ok := data["context"].(string)
				if !ok {
					t.Fatal("missing context text")
				}
				scope, _ := json.Marshal(Scope{"project": b.CWD})
				for _, want := range []string{string(scope), fmt.Sprintf("noCapture=%t", noCapture), "2026-10-31T18:00:00Z"} {
					if !strings.Contains(text, want) {
						t.Fatalf("runtime context lacks %q: %s", want, text)
					}
				}
				if noCapture && strings.Contains(text, "read/write and bounded learning are enabled") {
					t.Fatal("restricted context incorrectly advertises writes")
				}
				if !initialized {
					if _, err := os.Stat(root); !os.IsNotExist(err) {
						t.Fatal("context read initialized the private store", err)
					}
				}
				blocked := b
				blocked.Audience = "shared"
				denied, code := f.store.NativeEvent(context.Background(), nativeEvent(blocked, "context", "", map[string]any{}), "canonical")
				if code != 4 || denied.Data != nil {
					t.Fatal("runtime metadata escaped the private context", denied)
				}
			})
		}
	}
}

func TestNativeHarnessesShareDefaultsAndObservedFeedback(t *testing.T) {
	for _, entrypoint := range []string{"openclaw", "hermes", "pi-http", "pi-tui", "custom"} {
		t.Run(entrypoint, func(t *testing.T) {
			f := newFixture(t)
			b := nativeBinding(f)
			b.Entrypoint = entrypoint
			note := testNote("scoped native fact")
			note.Scope = Scope{"project": b.CWD}
			f.native(t, b, "tool", "capture", captureInput{Note: &note})
			query := f.native(t, b, "tool", "query", Query{QueryTerms: []string{note.Title}}).Data.(QueryResult)
			if len(query.Hits) != 1 {
				t.Fatal(query)
			}
			f.native(t, b, "injected", "", map[string]any{"refs": []Ref{query.Hits[0].Ref}})
			f.native(t, b, "feedback", "", map[string]string{"tool": "fixture_tool", "category": "failure"})
			trace := f.native(t, b, "tool", "trace", refInput{Ref: query.Hits[0].Ref}).Data.(traceResult)
			if len(trace.Entries) < 1 {
				t.Fatal("missing observed injection")
			}
			v, _ := f.store.open()
			generation := v.m.Generation
			v.fs.close()
			for _, blocked := range []NativeBinding{
				func() NativeBinding { x := b; x.NoMemory = true; return x }(),
				func() NativeBinding { x := b; x.Audience = "shared"; return x }(),
				func() NativeBinding { x := b; x.ParentSessionID = "parent"; return x }(),
			} {
				out, code := f.store.NativeEvent(context.Background(), nativeEvent(blocked, "context", "", map[string]any{}), "canonical")
				if code != 4 || out.Data != nil {
					t.Fatal("private context not blocked", out)
				}
			}
			b.NoCapture = true
			if out, code := f.store.NativeEvent(context.Background(), nativeEvent(b, "feedback", "", map[string]string{"tool": "fixture_tool", "category": "failure"}), "canonical"); code != 4 {
				t.Fatal(out)
			}
			f.native(t, b, "context", "", map[string]any{})
			v, _ = f.store.open()
			defer v.fs.close()
			if v.m.Generation != generation {
				t.Fatal("noCapture context wrote history")
			}
		})
	}
}

func TestNativeSourceChecksExpireWithTheOpportunity(t *testing.T) {
	f := newFixture(t)
	b := nativeBinding(f)
	name := filepath.Join(f.home, "fixture.txt")
	data := []byte("Synthetic current source.")
	if err := os.WriteFile(name, data, 0600); err != nil {
		t.Fatal(err)
	}
	version := digest(data)
	note := testNote("current fixture")
	note.Scope = Scope{"project": b.CWD}
	note.TemporalClass = "live"
	note.Revalidation.Mode = "on-use"
	note.Sources = []Source{{Kind: "file", Ref: name, Version: &version, Origin: "tool-observed", Assurance: "reported"}}
	ref := f.native(t, b, "tool", "capture", captureInput{Note: &note}).Data.(writeResult).Refs[0]
	b.NoCapture = true
	check := f.native(t, b, "tool", "source", map[string]any{"ref": ref, "sourceRef": name}).Data.(CurrentSourceCheck)
	b.CurrentSourceChecks = []CurrentSourceCheck{check}
	query := f.native(t, b, "tool", "query", Query{QueryTerms: []string{note.Title}}).Data.(QueryResult)
	if query.Hits[0].Review.NeedsRecheck {
		t.Fatal("current native check was not used")
	}
	b.OpportunityID = "later-task"
	query = f.native(t, b, "tool", "query", Query{QueryTerms: []string{note.Title}}).Data.(QueryResult)
	if !query.Hits[0].Review.NeedsRecheck {
		t.Fatal("ephemeral check crossed task boundary")
	}
	r := f.request(t, Query{QueryTerms: []string{note.Title}})
	r.RequestContext.CurrentSourceChecks = []CurrentSourceCheck{check}
	if out, code := f.store.Execute(context.Background(), "query", r); code != 4 {
		t.Fatal("ordinary caller forged source assurance", out)
	}
}

func TestArtifactCaseExposureDoesNotResetWithCandidateIdentity(t *testing.T) {
	first, second := Ref{ID: "candidate-one"}, Ref{ID: "candidate-two"}
	set := artifactSetDigest()
	calls := map[string]ModelCall{"attempt": {Candidate: first, Case: 3, CaseSetDigest: set}}
	if artifactCaseExposed(calls, first, set, 3) || !artifactCaseExposed(calls, second, set, 3) || artifactCaseExposed(calls, second, set, 4) {
		t.Fatal("prospective case exposure was not tracked across candidate identities")
	}
	calls["attempt"] = ModelCall{Case: 3, CaseSetDigest: set, Outcome: "forgotten"}
	if !artifactCaseExposed(calls, second, set, 3) {
		t.Fatal("forgetting lineage made a public fixture unseen again")
	}
}

func TestArtifactProfileBudgetTrialAdoptionAndRegression(t *testing.T) {
	f := newFixture(t)
	b := nativeBinding(f)
	obs := f.native(t, b, "feedback", "", map[string]string{"tool": "fixture_tool", "category": "failure"}).Data.(writeResult).Refs[0]
	proc := Procedure{Kind: "playbook", Scope: Scope{"project": b.CWD}, Applicability: []string{"Only artifact-contract-v1 synthetic records"}, Exclusions: []string{"Live data and external actions"}, SourceObservationIDs: []string{obs.ID}, Hypothesis: "Read each field and check the conjunction before returning JSON.", Compatibility: map[string]string{"helperABI": "1", "adapter": AdapterVersion, "model": b.Model, "nativeVersion": b.NativeVersion, "modelRevision": b.ModelRevision}}
	// A fixture owner searches for a stable sampling branch to exercise three
	// selected prospective cases; this is branch coverage, not an effect claim.
	for seed := 0; seed < 1000; seed++ {
		proc.Payload, _ = encode(Playbook{Steps: []string{fmt.Sprintf("Read fields; synthetic variant %d.", seed)}, ExpectedEvidence: []string{"Exact JSON result"}, StopConditions: []string{"Unknown schema"}})
		count := 0
		groups := map[string]bool{}
		for i := 2; i < 34; i++ {
			if strings.HasSuffix(digest([]byte(fmt.Sprintf("%d/%s", i, procedureFingerprint(proc)))), "0") {
				count++
				_, _, group := artifactCase(i)
				groups[group] = true
				if count == 3 {
					break
				}
			}
		}
		if count >= 3 && len(groups) >= 2 {
			break
		}
		if seed == 999 {
			t.Fatal("no sampling fixture")
		}
	}
	ref := f.native(t, b, "tool", "propose", proposeInput{Procedure: proc}).Data.(writeResult).Refs[0]
	changed := b
	changed.ModelRevision = digest([]byte("a different configured route for the same model name"))
	for _, event := range []NativeEvent{nativeEvent(changed, "evaluation-next", "", refInput{Ref: ref}), nativeEvent(changed, "tool", "check", refInput{Ref: ref})} {
		if out, code := f.store.NativeEvent(context.Background(), event, "canonical"); code != 7 || out.Reason != "CompatibilityChanged" {
			t.Fatal("model route change reused old qualification", out)
		}
	}
	clock := f.store.now()
	dispatch := func(baselineFails bool) ModelCall {
		ticket := f.native(t, b, "evaluation-next", "", refInput{Ref: ref}).Data.(*artifactTicket)
		_, expected, _ := artifactCase(ticket.Call.Case)
		output := string(expected)
		if ticket.Call.Variant == "baseline" && baselineFails {
			output = `{}`
		}
		f.native(t, b, "evaluation-result", "", artifactRecord{ID: ticket.Call.ID, Output: output, Outcome: "completed", Model: b.Model, Requests: 1})
		return ticket.Call
	}
	for i := 0; i < 4; i++ {
		dispatch(true)
	}
	if out, code := f.store.NativeEvent(context.Background(), nativeEvent(b, "evaluation-next", "", refInput{Ref: ref}), "canonical"); code != 5 && code != 7 {
		t.Fatal("budget/finished selection did not stop work", out)
	}
	checked := f.native(t, b, "tool", "check", refInput{Ref: ref}).Data.(writeResult)
	if checked.State != "checked" {
		t.Fatal(checked)
	}
	f.native(t, b, "tool", "advance", refInput{Ref: ref})
	clock = clock.Add(25 * time.Hour)
	f.store.now = func() time.Time { return clock }
	for i := 0; i < 6; i++ {
		if i == 4 {
			clock = clock.Add(25 * time.Hour)
		}
		dispatch(false)
	}
	active := f.native(t, b, "tool", "advance", refInput{Ref: ref}).Data.(writeResult)
	if active.State != "active" {
		t.Fatal(active)
	}
	// A newer candidate does not displace the still-active revision or stop
	// its subsequent usage evaluation and result recording.
	proc.Parent = &ref
	f.native(t, b, "tool", "propose", proposeInput{Procedure: proc})
	ticket := f.native(t, b, "evaluation-next", "", refInput{Ref: ref}).Data.(*artifactTicket)
	if ticket.Call.Phase != "usage" || !strings.Contains(ticket.Prompt, "synthetic variant") {
		t.Fatal("active method not used in a later fixture")
	}
	f.native(t, b, "evaluation-result", "", artifactRecord{ID: ticket.Call.ID, Output: `{}`, Outcome: "completed", Model: b.Model, Requests: 1})
	v, _ := f.store.open()
	defer v.fs.close()
	if v.m.ProcedureStates[ref.Key()].State != "quarantined" || v.m.Slots[artifactSlot(proc.Scope)].Active != nil {
		t.Fatal("fixture regression did not stop reuse")
	}
}
