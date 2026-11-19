// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package memory

import (
	"encoding/json"
	"slices"
	"strings"
	"time"
)

type Evaluation struct {
	ID              string       `json:"id"`
	Revision        int64        `json:"revision"`
	CandidateRef    Ref          `json:"candidateRef"`
	BaselineRef     *Ref         `json:"baselineRef"`
	BaselineDigest  string       `json:"baselineDigest"`
	ProfileID       string       `json:"profileId"`
	ProfileRevision string       `json:"profileRevision"`
	CaseSetDigest   string       `json:"caseSetDigest"`
	CorpusDigest    string       `json:"corpusDigest"`
	Phase           string       `json:"phase"`
	Independence    string       `json:"independence"`
	Integrity       string       `json:"integrity"`
	Results         []CaseResult `json:"results"`
	CompletedAt     string       `json:"completedAt"`
	Improved        bool         `json:"improved"`
}
type CaseResult struct {
	Key             string `json:"key"`
	Group           string `json:"group"`
	Correct         bool   `json:"correct"`
	BaselineCorrect bool   `json:"baselineCorrect"`
	Work            int    `json:"work"`
	BaselineWork    int    `json:"baselineWork"`
}
type retrievalCase struct {
	Terms    []string
	Scope    Scope
	Group    string
	Expected []string
}

const profileRevision = "memory-retrieval-v1/1"

func procedureFingerprint(proc Procedure) string {
	b, _ := encode(struct {
		Kind    string
		Scope   Scope
		Payload json.RawMessage
		Profile string
	}{proc.Kind, proc.Scope, proc.Payload, profileRevision})
	return digest(b)
}

func planValid(p RetrievalPlan) bool {
	return len(p.Stages) > 0 && textList(p.Stages, 3, 32) && textList(p.RankBy, 2, 32) && len(p.RankBy) > 0 && p.RankBy[0] == "scope-match" && p.MaxResults >= 1 && p.MaxResults <= 10 && allAllowed(p.Stages, []string{"exact-title", "alias", "keywords"}) && allAllowed(p.RankBy, []string{"scope-match", "evidence-recency"})
}
func allAllowed(values, allowed []string) bool {
	for _, v := range values {
		if !slices.Contains(allowed, v) {
			return false
		}
	}
	return true
}

type proposeInput struct {
	Procedure Procedure `json:"procedure"`
}

func (s *Store) propose(v *snapshot, r Request) (writeResult, error) {
	var input proposeInput
	if err := Decode(r.Payload, &input); err != nil {
		return writeResult{}, err
	}
	proc := input.Procedure
	if proc.ID != "" || proc.Revision != 0 || proc.Hypothesis == "" || !validText(proc.Hypothesis, 2048) || validateScope(proc.Scope) != nil || !scopeMatches(r.RequestContext.Scope, proc.Scope) || !textList(proc.Applicability, 8, 1024) || len(proc.Applicability) == 0 || !textList(proc.Exclusions, 8, 1024) || len(proc.SourceObservationIDs) < 1 || len(proc.SourceObservationIDs) > 8 || len(proc.DerivedFrom) > 8 || len(proc.RequiredKnowledgeRefs) > 8 {
		return writeResult{}, problem(2, "InvalidInput")
	}
	if proc.Compatibility["helperABI"] != "1" || len(proc.Compatibility) > 5 {
		return writeResult{}, problem(2, "InvalidInput")
	}
	for _, ref := range proc.RequiredKnowledgeRefs {
		if !strings.HasPrefix(ref.ID, "note-") {
			return writeResult{}, problem(2, "InvalidInput")
		}
	}
	switch proc.Kind {
	case "retrieval-plan":
		if len(proc.Compatibility) != 1 {
			return writeResult{}, problem(2, "InvalidInput")
		}
		var plan RetrievalPlan
		if Decode(proc.Payload, &plan) != nil || !planValid(plan) {
			return writeResult{}, problem(2, "InvalidInput")
		}
	case "playbook":
		if (len(proc.Compatibility) != 4 && len(proc.Compatibility) != 5) || proc.Compatibility["adapter"] != AdapterVersion || !validText(proc.Compatibility["model"], 256) || proc.Compatibility["model"] == "" || !validText(proc.Compatibility["nativeVersion"], 128) || proc.Compatibility["nativeVersion"] == "" || (len(proc.Compatibility) == 5 && !digestPattern.MatchString(proc.Compatibility["modelRevision"])) {
			return writeResult{}, problem(2, "InvalidInput")
		}
		var plan Playbook
		if Decode(proc.Payload, &plan) != nil || len(plan.Steps) == 0 || !textList(plan.Steps, 8, 1024) || len(plan.ExpectedEvidence) == 0 || !textList(plan.ExpectedEvidence, 8, 1024) || len(plan.StopConditions) == 0 || !textList(plan.StopConditions, 8, 1024) {
			return writeResult{}, problem(2, "InvalidInput")
		}
	default:
		return writeResult{}, problem(2, "InvalidInput")
	}
	if err := v.validateLinks(proc.DerivedFrom, proc.RequiredKnowledgeRefs, nil, r); err != nil {
		return writeResult{}, err
	}
	for _, id := range proc.SourceObservationIDs {
		ref, ok := v.m.Objects[id]
		if !ok || !strings.HasPrefix(id, "obs-") || !v.allowed(ref, r) {
			return writeResult{}, problem(7, "EvidenceInsufficient")
		}
		var o Observation
		if err := v.read(ref, &o); err != nil {
			return writeResult{}, err
		}
	}
	count := 0
	for _, state := range v.m.ProcedureStates {
		if slices.Contains([]string{"candidate", "checked", "trial"}, state.State) {
			count++
		}
	}
	if count >= 32 {
		return writeResult{}, problem(5, "LimitReached")
	}
	if proc.Parent != nil {
		if !v.allowed(*proc.Parent, r) {
			return writeResult{}, problem(4, "DisclosureNotAllowed")
		}
		if err := v.current(*proc.Parent); err != nil {
			return writeResult{}, err
		}
		var parent Procedure
		if err := v.read(*proc.Parent, &parent); err != nil {
			return writeResult{}, err
		}
		if parent.Kind != proc.Kind || scopeKey(parent.Scope) != scopeKey(proc.Scope) {
			return writeResult{}, problem(2, "ScopeMismatch")
		}
		revision, err := v.nextRevision(*proc.Parent)
		if err != nil {
			return writeResult{}, err
		}
		previous := v.m.ProcedureStates[proc.Parent.Key()]
		if slices.Contains([]string{"candidate", "checked", "trial"}, previous.State) {
			previous.State, previous.Reason = "retired", "superseded-candidate"
			v.m.ProcedureStates[proc.Parent.Key()] = previous
			for key, current := range v.m.Slots {
				if sameRef(current.Trial, proc.Parent) {
					current.Trial = nil
					v.m.Slots[key] = current
				}
			}
		}
		proc.ID = proc.Parent.ID
		proc.Revision = revision
	} else {
		proc.ID = newID("proc")
		proc.Revision = 1
	}
	ref, err := v.save(proc.ID, proc.Revision, proc)
	if err != nil {
		return writeResult{}, err
	}
	v.m.ProcedureStates[ref.Key()] = methodState{State: "candidate"}
	// The candidate is the response to the observations it cites.
	v.processObservations(proc.SourceObservationIDs)
	return writeResult{Refs: []Ref{ref}, State: "candidate"}, nil
}
func corpusDigest(corpus []corpusNote) string {
	refs := []Ref{}
	for _, item := range corpus {
		refs = append(refs, item.Ref)
	}
	b, _ := encode(refs)
	return digest(b)
}
func caseGroup(n Note) string {
	if len(n.Sources) == 0 {
		return ""
	}
	ref := n.Sources[0].Ref
	ref, _, _ = strings.Cut(ref, "#")
	return digest([]byte(ref))
}

// This oracle enumerates exact matches independently of the staged ranker.
func oracle(corpus []corpusNote, t []string, scope Scope) []string {
	exact, aliases, keywords := []string{}, []string{}, []string{}
	for _, item := range corpus {
		n := item.Note
		if !scopeMatches(n.Scope, scope) {
			continue
		}
		if len(t) == 1 && strings.ToLower(strings.TrimSpace(n.Title)) == t[0] {
			exact = append(exact, item.Ref.ID)
		}
		for _, alias := range n.Aliases {
			if len(t) == 1 && strings.ToLower(strings.TrimSpace(alias)) == t[0] {
				aliases = append(aliases, item.Ref.ID)
				break
			}
		}
		set := map[string]bool{}
		for _, field := range append(append(append([]string{n.Title}, n.Aliases...), n.Keywords...), n.Tags...) {
			set[strings.ToLower(strings.TrimSpace(field))] = true
		}
		all := true
		for _, term := range t {
			if !set[term] {
				all = false
			}
		}
		if all {
			keywords = append(keywords, item.Ref.ID)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	if len(aliases) > 0 {
		return aliases
	}
	return keywords
}
func selectionCases(corpus []corpusNote, scope Scope) []retrievalCase {
	cases := []retrievalCase{}
	seen := map[string]bool{}
	for _, item := range corpus {
		if !scopeMatches(item.Note.Scope, scope) {
			continue
		}
		inputs := []string{item.Note.Title}
		if len(item.Note.Aliases) > 0 {
			inputs = append(inputs, item.Note.Aliases[0])
		}
		if len(item.Note.Keywords) > 0 {
			inputs = append(inputs, item.Note.Keywords[0])
		}
		for _, input := range inputs {
			t := []string{strings.ToLower(strings.TrimSpace(input))}
			key := opportunityKey(t, scope)
			if seen[key] {
				continue
			}
			seen[key] = true
			cases = append(cases, retrievalCase{t, scope, caseGroup(item.Note), oracle(corpus, t, scope)})
		}
		if len(cases) >= 30 {
			break
		}
	}
	for _, word := range []string{"__memory_no_match_a__", "__memory_no_match_b__"} {
		t := []string{word}
		if len(oracle(corpus, t, scope)) == 0 {
			cases = append(cases, retrievalCase{t, scope, "negative", nil})
		}
	}
	return cases
}
func compare(candidate, base RetrievalPlan, corpus []corpusNote, c retrievalCase) CaseResult {
	a, _, work := rank(candidate, corpus, c.Terms, c.Scope)
	b, _, baseWork := rank(base, corpus, c.Terms, c.Scope)
	return CaseResult{opportunityKey(c.Terms, c.Scope), c.Group, retrievalCorrect(a, c.Expected), retrievalCorrect(b, c.Expected), work, baseWork}
}
func retrievalCorrect(results []corpusNote, expected []string) bool {
	if len(expected) == 0 {
		return len(results) == 0
	}
	return len(results) > 0 && slices.Contains(expected, results[0].Ref.ID)
}

type refInput struct {
	Ref    Ref    `json:"ref"`
	Reason string `json:"reason,omitempty"`
}

func (s *Store) check(v *snapshot, r Request) (writeResult, error) {
	var input refInput
	if err := Decode(r.Payload, &input); err != nil {
		return writeResult{}, err
	}
	if err := v.current(input.Ref); err != nil {
		return writeResult{}, err
	}
	var proc Procedure
	if err := v.read(input.Ref, &proc); err != nil {
		return writeResult{}, err
	}
	if proc.Kind == "playbook" {
		return s.checkArtifact(v, r, input.Ref, proc)
	}
	if proc.Kind != "retrieval-plan" {
		return writeResult{}, problem(7, "EvaluationUnavailable")
	}
	if !scopeMatches(proc.Scope, r.RequestContext.Scope) || scopeKey(proc.Scope) != scopeKey(r.RequestContext.Scope) {
		return writeResult{}, problem(2, "ScopeMismatch")
	}
	state := v.m.ProcedureStates[input.Ref.Key()]
	if !slices.Contains([]string{"candidate", "checked", "rejected"}, state.State) {
		return writeResult{}, problem(3, "Conflict")
	}
	for _, ref := range proc.RequiredKnowledgeRefs {
		if err := s.usable(v, ref, r, map[string]bool{}, 0); err != nil {
			return writeResult{}, err
		}
	}
	corpus, partial, err := v.corpus(r)
	if err != nil {
		return writeResult{}, err
	}
	if partial != nil {
		return writeResult{}, problem(7, "EvidenceInsufficient")
	}
	cases := selectionCases(corpus, proc.Scope)
	groups := map[string]bool{}
	for _, c := range cases {
		if c.Group != "negative" {
			groups[c.Group] = true
		}
	}
	if len(cases) < 8 || len(groups) < 3 {
		return writeResult{}, problem(7, "EvidenceInsufficient")
	}
	now := s.now()
	if last, err := time.Parse(time.RFC3339, v.m.LastWriteAt); err != nil || now.Before(last) {
		return writeResult{}, problem(5, "BudgetExhausted")
	}
	slotID := scopeKey(proc.Scope)
	lineage := slotID + "/" + proc.Kind
	slotCount, lineageCount := 0, 0
	kept := []budgetEntry{}
	for _, entry := range v.m.Budgets {
		at, err := time.Parse(time.RFC3339, entry.At)
		if err != nil {
			return writeResult{}, problem(6, "IntegrityMismatch")
		}
		if now.Sub(at) < 24*time.Hour {
			kept = append(kept, entry)
			if entry.Slot == slotID {
				slotCount++
			}
			if entry.Lineage == lineage {
				lineageCount++
			}
		}
	}
	if slotCount >= 8 || lineageCount >= 2 {
		return writeResult{}, problem(5, "BudgetExhausted")
	}
	v.m.Budgets = append(kept, budgetEntry{timestamp(now), slotID, lineage})
	var plan RetrievalPlan
	if err := Decode(proc.Payload, &plan); err != nil {
		return writeResult{}, err
	}
	base := Baseline()
	baseRef := v.m.Slots[slotID].Active
	if baseRef != nil {
		var previous Procedure
		if err := v.read(*baseRef, &previous); err != nil {
			return writeResult{}, err
		}
		if err := s.procedureUsable(v, *baseRef, previous, r); err != nil {
			return writeResult{}, err
		}
		if err := Decode(previous.Payload, &base); err != nil {
			return writeResult{}, err
		}
	}
	baseBytes, _ := encode(base)
	casesBytes, _ := encode(cases)
	eval := Evaluation{ID: newID("eval"), Revision: 1, CandidateRef: input.Ref, BaselineRef: baseRef, BaselineDigest: digest(baseBytes), ProfileID: RetrievalProfile, ProfileRevision: profileRevision, CaseSetDigest: digest(casesBytes), CorpusDigest: corpusDigest(corpus), Phase: "selection", Independence: "independent-criterion", Integrity: "adapter-observed", CompletedAt: timestamp(now)}
	correct, baseCorrect, work, baseWork := 0, 0, 0, 0
	regression := false
	for _, c := range cases {
		res := compare(plan, base, corpus, c)
		eval.Results = append(eval.Results, res)
		if res.Correct {
			correct++
		}
		if res.BaselineCorrect {
			baseCorrect++
		}
		if !res.Correct && res.BaselineCorrect {
			regression = true
		}
		work += res.Work
		baseWork += res.BaselineWork
	}
	eval.Improved = !regression && correct >= baseCorrect && (correct > baseCorrect || work*100 <= baseWork*80)
	ref, err := v.save(eval.ID, 1, eval)
	if err != nil {
		return writeResult{}, err
	}
	state.Evaluation = &ref
	state.State = "rejected"
	state.Reason = "NoImprovement"
	if eval.Improved {
		state.State = "checked"
		state.Reason = ""
	}
	v.m.ProcedureStates[input.Ref.Key()] = state
	return writeResult{Refs: []Ref{ref}, State: state.State}, nil
}
func (s *Store) procedureUsable(v *snapshot, ref Ref, p Procedure, r Request) error {
	if !v.allowed(ref, r) || !scopeMatches(p.Scope, r.RequestContext.Scope) {
		return problem(2, "ScopeMismatch")
	}
	state := v.m.ProcedureStates[ref.Key()]
	if !slices.Contains([]string{"active", "trial", "retired"}, state.State) || state.Reason == "owner-disabled" || state.Exhausted {
		return problem(7, "EvidenceInsufficient")
	}
	if p.Kind != "retrieval-plan" || p.Compatibility["helperABI"] != "1" {
		return problem(7, "EvaluationUnavailable")
	}
	for _, ref := range p.RequiredKnowledgeRefs {
		if err := s.usable(v, ref, r, map[string]bool{}, 0); err != nil {
			return err
		}
	}
	for _, d := range p.DerivedFrom {
		if v.m.Tombstones[d.Ref.ID] {
			return problem(6, "SourceForgotten")
		}
		if d.Role == "support" && strings.HasPrefix(d.Ref.ID, "note-") {
			if err := s.usable(v, d.Ref, r, map[string]bool{}, 0); err != nil {
				return err
			}
		}
	}
	return nil
}

// Only a current slot or its still-approved predecessor can select a method.
// Historical use records and retired candidates are not independent approvals.
func (s *Store) activeMethod(v *snapshot, r Request) (*Ref, error) {
	current := v.m.Slots[scopeKey(r.RequestContext.Scope)]
	for _, ref := range []*Ref{current.Active, current.Baseline} {
		if ref == nil || v.m.Tombstones[ref.ID] {
			continue
		}
		state := v.m.ProcedureStates[ref.Key()]
		if state.State != "active" && !(state.State == "retired" && state.Reason == "superseded") {
			continue
		}
		var proc Procedure
		if err := v.read(*ref, &proc); err != nil {
			return nil, err
		}
		if s.procedureUsable(v, *ref, proc, r) == nil {
			return ref, nil
		}
	}
	return nil, nil
}
func (s *Store) advance(v *snapshot, r Request) (writeResult, error) {
	var input refInput
	if err := Decode(r.Payload, &input); err != nil {
		return writeResult{}, err
	}
	if err := v.current(input.Ref); err != nil {
		return writeResult{}, err
	}
	var proc Procedure
	if err := v.read(input.Ref, &proc); err != nil {
		return writeResult{}, err
	}
	if proc.Kind == "playbook" {
		return s.advanceArtifact(v, r, input.Ref, proc)
	}
	if scopeKey(proc.Scope) != scopeKey(r.RequestContext.Scope) {
		return writeResult{}, problem(2, "ScopeMismatch")
	}
	state := v.m.ProcedureStates[input.Ref.Key()]
	if state.Evaluation == nil || state.Exhausted {
		return writeResult{}, problem(7, "EvidenceInsufficient")
	}
	var eval Evaluation
	if err := v.read(*state.Evaluation, &eval); err != nil {
		return writeResult{}, err
	}
	if eval.CandidateRef != input.Ref || !eval.Improved || eval.ProfileRevision != profileRevision || eval.Independence != "independent-criterion" {
		return writeResult{}, problem(7, "EvidenceInsufficient")
	}
	if v.m.ExhaustedTrials[procedureFingerprint(proc)+eval.CaseSetDigest] {
		return writeResult{}, problem(7, "EvidenceInsufficient")
	}
	corpus, partial, err := v.corpus(r)
	if err != nil {
		return writeResult{}, err
	}
	if partial != nil {
		return writeResult{}, problem(7, "EvidenceInsufficient")
	}
	if eval.CorpusDigest != corpusDigest(corpus) {
		key := scopeKey(proc.Scope)
		if sameRef(v.m.Slots[key].Trial, &input.Ref) {
			v.resetRetrievalTrial(key)
			return writeResult{Refs: []Ref{input.Ref}, State: "candidate"}, nil
		}
		return writeResult{}, problem(7, "EvidenceInsufficient")
	}
	for _, ref := range proc.RequiredKnowledgeRefs {
		if err := s.usable(v, ref, r, map[string]bool{}, 0); err != nil {
			return writeResult{}, err
		}
	}
	key := scopeKey(proc.Scope)
	current, exists := v.m.Slots[key]
	if !exists && len(v.m.Slots) >= 16 {
		return writeResult{}, problem(5, "LimitReached")
	}
	if !sameRef(current.Active, eval.BaselineRef) {
		return writeResult{}, problem(3, "Conflict")
	}
	baseline, err := s.activeMethod(v, r)
	if err != nil {
		return writeResult{}, err
	}
	if !sameRef(current.Active, baseline) {
		return writeResult{}, problem(7, "EvidenceInsufficient")
	}
	switch state.State {
	case "checked":
		if current.Trial != nil {
			return writeResult{}, problem(3, "Conflict")
		}
		current.Baseline = current.Active
		current.Trial = &input.Ref
		state.State = "trial"
		state.TrialSeen = map[string]bool{}
		state.TrialPassed = map[string]string{}
	case "trial":
		groups := map[string]bool{}
		for _, group := range state.TrialPassed {
			groups[group] = true
		}
		if len(state.TrialPassed) < 3 || len(groups) < 2 {
			return writeResult{}, problem(7, "EvidenceInsufficient")
		}
		if current.Active != nil {
			previous := v.m.ProcedureStates[current.Active.Key()]
			previous.State = "retired"
			previous.Reason = "superseded"
			v.m.ProcedureStates[current.Active.Key()] = previous
		}
		current.Active = &input.Ref
		current.Trial = nil
		state.State = "active"
	default:
		return writeResult{}, problem(3, "Conflict")
	}
	v.m.Slots[key] = current
	v.m.ProcedureStates[input.Ref.Key()] = state
	return writeResult{Refs: []Ref{input.Ref}, State: state.State}, nil
}
func sameRef(a, b *Ref) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func (v *snapshot) resetRetrievalTrial(key string) {
	current := v.m.Slots[key]
	if current.Trial == nil {
		return
	}
	state := v.m.ProcedureStates[current.Trial.Key()]
	state.State, state.Reason = "candidate", "CorpusChanged"
	state.Evaluation, state.TrialSeen, state.TrialPassed = nil, nil, nil
	v.m.ProcedureStates[current.Trial.Key()] = state
	current.Trial = nil
	v.m.Slots[key] = current
}

type selectInput struct {
	QueryTerms []string `json:"queryTerms"`
}

func (s *Store) selectMethod(v *snapshot, r Request, record bool) (writeResult, error) {
	var input selectInput
	if err := Decode(r.Payload, &input); err != nil {
		return writeResult{}, err
	}
	t, err := terms(input.QueryTerms)
	if err != nil {
		return writeResult{}, err
	}
	key := scopeKey(r.RequestContext.Scope)
	current := v.m.Slots[key]
	selected, err := s.activeMethod(v, r)
	if err != nil {
		return writeResult{}, err
	}
	assignment := "baseline"
	opportunity := opportunityKey(t, r.RequestContext.Scope)
	if selected != nil {
		var proc Procedure
		if err := v.read(*selected, &proc); err != nil {
			return writeResult{}, err
		}
		corpus, partial, err := v.corpus(r)
		if err != nil {
			return writeResult{}, err
		}
		assignment = "active"
		var plan RetrievalPlan
		if err := Decode(proc.Payload, &plan); err != nil {
			return writeResult{}, err
		}
		hits, _, _ := rank(plan, corpus, t, r.RequestContext.Scope)
		if partial != nil || !retrievalCorrect(hits, oracle(corpus, t, r.RequestContext.Scope)) {
			if record && partial == nil {
				state := v.m.ProcedureStates[selected.Key()]
				state.State, state.Reason = "quarantined", "Regression"
				v.m.ProcedureStates[selected.Key()] = state
			}
			selected, assignment = nil, "baseline"
		}
	}
	if record && current.Trial != nil && sameRef(selected, current.Active) {
		ref := *current.Trial
		state := v.m.ProcedureStates[ref.Key()]
		if state.TrialSeen == nil {
			state.TrialSeen = map[string]bool{}
		}
		if state.TrialPassed == nil {
			state.TrialPassed = map[string]string{}
		}
		var proc Procedure
		if err := v.read(ref, &proc); err != nil {
			return writeResult{}, err
		}
		if err := s.procedureUsable(v, ref, proc, r); err != nil {
			return writeResult{}, err
		}
		var eval Evaluation
		if state.Evaluation == nil {
			return writeResult{}, problem(7, "EvidenceInsufficient")
		}
		if err := v.read(*state.Evaluation, &eval); err != nil {
			return writeResult{}, err
		}
		if !sameRef(eval.BaselineRef, current.Active) || eval.ProfileRevision != profileRevision {
			return writeResult{}, problem(7, "EvidenceInsufficient")
		}
		corpus, partial, err := v.corpus(r)
		if err != nil {
			return writeResult{}, err
		}
		if partial != nil {
			return writeResult{}, problem(7, "EvidenceInsufficient")
		}
		if eval.CorpusDigest != corpusDigest(corpus) {
			v.resetRetrievalTrial(key)
		} else if !state.TrialSeen[opportunity] && len(state.TrialSeen) < 32 {
			state.TrialSeen[opportunity] = true
			base := Baseline()
			if current.Active != nil {
				var old Procedure
				if err := v.read(*current.Active, &old); err != nil {
					return writeResult{}, err
				}
				if err := Decode(old.Payload, &base); err != nil {
					return writeResult{}, err
				}
			}
			var plan RetrievalPlan
			if err := Decode(proc.Payload, &plan); err != nil {
				return writeResult{}, err
			}
			expected := oracle(corpus, t, r.RequestContext.Scope)
			group := ""
			if len(expected) > 0 {
				for _, n := range corpus {
					if n.Ref.ID == expected[0] {
						group = caseGroup(n.Note)
					}
				}
			}
			res := compare(plan, base, corpus, retrievalCase{t, r.RequestContext.Scope, group, expected})
			if !res.Correct && res.BaselineCorrect {
				state.State = "quarantined"
				state.Reason = "Regression"
				current.Trial = nil
			} else {
				// A fixed 1/16 sample stays within the profile's 10% ceiling.
				// Request IDs and re-created candidate IDs do not redraw it.
				hash := digest([]byte(opportunity + procedureFingerprint(proc)))
				trial := strings.HasSuffix(hash, "0")
				if trial {
					selected = &ref
					assignment = "trial"
					exposed := slices.ContainsFunc(eval.Results, func(c CaseResult) bool { return c.Key == opportunity })
					if !exposed && group != "" && res.Correct {
						state.TrialPassed[opportunity] = group
					}
				}
				groups := map[string]bool{}
				for _, group := range state.TrialPassed {
					groups[group] = true
				}
				if len(state.TrialSeen) >= 32 && (len(state.TrialPassed) < 3 || len(groups) < 2) {
					state.State = "checked"
					state.Exhausted = true
					v.m.ExhaustedTrials[procedureFingerprint(proc)+eval.CaseSetDigest] = true
					current.Trial = nil
					selected = current.Active
					assignment = "baseline"
					if selected != nil {
						assignment = "active"
					}
				}
			}
			v.m.ProcedureStates[ref.Key()] = state
			v.m.Slots[key] = current
		}
	}
	if !record {
		refs := []Ref{}
		if selected != nil {
			refs = append(refs, *selected)
		}
		return writeResult{Refs: refs, State: assignment}, nil
	}
	use := Use{ID: newID("use"), Revision: 1, OpportunityID: opportunity, SelectedRef: selected, ManifestGeneration: v.m.Generation, Assignment: assignment, Compatibility: map[string]string{"helperABI": "1", "profileRevision": profileRevision}, InjectedRefs: []Ref{}, CitedRefs: []Ref{}, OutputRefs: []OutputRef{}, TraceCoverage: TraceCoverage{Status: "partial", Reasons: []string{"SelectionOnly"}, Assurance: "adapter-observed"}}
	ref, err := v.save(use.ID, 1, use)
	if err != nil {
		return writeResult{}, err
	}
	return writeResult{Refs: []Ref{ref}, State: assignment}, nil
}
func (s *Store) retire(v *snapshot, r Request) (writeResult, error) {
	var input refInput
	if err := Decode(r.Payload, &input); err != nil {
		return writeResult{}, err
	}
	if !strings.HasPrefix(input.Ref.ID, "proc-") || !v.allowed(input.Ref, r) {
		return writeResult{}, problem(4, "DisclosureNotAllowed")
	}
	var procedure Procedure
	if err := v.read(input.Ref, &procedure); err != nil {
		return writeResult{}, err
	}
	state, ok := v.m.ProcedureStates[input.Ref.Key()]
	if !ok {
		return writeResult{}, problem(2, "InvalidInput")
	}
	state.State = "retired"
	state.Reason = "owner-disabled"
	v.m.ProcedureStates[input.Ref.Key()] = state
	for key, current := range v.m.Slots {
		if current.Active != nil && *current.Active == input.Ref {
			current.Active = nil
			if current.Trial != nil {
				trial := v.m.ProcedureStates[current.Trial.Key()]
				trial.State, trial.Reason = "checked", "BaselineChanged"
				v.m.ProcedureStates[current.Trial.Key()] = trial
				current.Trial = nil
			}
		}
		if current.Trial != nil && *current.Trial == input.Ref {
			current.Trial = nil
		}
		if current.Baseline != nil && *current.Baseline == input.Ref {
			current.Baseline = nil
		}
		v.m.Slots[key] = current
	}
	return writeResult{Refs: []Ref{input.Ref}, State: "retired"}, nil
}
