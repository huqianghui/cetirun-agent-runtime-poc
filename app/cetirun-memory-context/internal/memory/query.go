// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package memory

import (
	"context"
	"slices"
	"sort"
	"strings"
)

type Query struct {
	View         string   `json:"view,omitempty"`
	QueryTerms   []string `json:"queryTerms"`
	SelectionRef *Ref     `json:"selectionRef,omitempty"`
	Limit        int      `json:"limit,omitempty"`
}
type Hit struct {
	Ref          Ref      `json:"ref"`
	Note         Note     `json:"note"`
	Review       review   `json:"review"`
	ConflictRefs []Ref    `json:"conflictRefs,omitempty"`
	MatchedWords []string `json:"matchedWords,omitempty"`
}
type QueryResult struct {
	Hits        []Hit    `json:"hits"`
	Stage       string   `json:"stage"`
	WorkUnits   int      `json:"workUnits"`
	TextMatches int      `json:"textMatches,omitempty"`
	Partial     *Partial `json:"partial"`
}

// MaxQueryLimit bounds a caller-chosen result count; the output byte budget
// still applies.
const MaxQueryLimit = 20

type corpusNote struct {
	Ref  Ref
	Note Note
}

func terms(values []string) ([]string, error) {
	if len(values) < 1 || len(values) > 8 {
		return nil, problem(2, "InvalidInput")
	}
	result := []string{}
	for _, value := range values {
		v := strings.ToLower(strings.TrimSpace(value))
		if v == "" || !validText(v, 256) {
			return nil, problem(2, "InvalidInput")
		}
		if !slices.Contains(result, v) {
			result = append(result, v)
		}
	}
	return result, nil
}
func (v *snapshot) corpus(r Request) ([]corpusNote, *Partial, error) {
	items := []corpusNote{}
	refs := v.refs("note")
	bytesRead := 0
	for i, ref := range refs {
		if i >= MaxNoteScan || bytesRead >= MaxScanBytes {
			return items, &Partial{LimitHit: true, Unscanned: len(refs) - i}, nil
		}
		if !v.allowed(ref, r) {
			continue
		}
		var note Note
		if err := v.read(ref, &note); err != nil {
			if v.damagedObject(ref, err) {
				continue
			}
			return nil, nil, err
		}
		b, _ := encode(note)
		bytesRead += len(b)
		if bytesRead > MaxScanBytes {
			return items, &Partial{LimitHit: true, Unscanned: len(refs) - i}, nil
		}
		items = append(items, corpusNote{ref, note})
	}
	return items, v.damageReport(), nil
}
func evidenceTime(n Note) string {
	best := ""
	for _, source := range n.Sources {
		if source.ObservedAt != nil && *source.ObservedAt > best {
			best = *source.ObservedAt
		}
	}
	return best
}
func rank(plan RetrievalPlan, corpus []corpusNote, query []string, scope Scope) ([]corpusNote, string, int) {
	hits, stage, work := rankMatches(plan, corpus, query, scope)
	if len(hits) > 0 {
		hits = hits[:min(len(hits), plan.MaxResults)]
	}
	return hits, stage, work
}
func rankMatches(plan RetrievalPlan, corpus []corpusNote, query []string, scope Scope) ([]corpusNote, string, int) {
	work := 0
	for _, stage := range plan.Stages {
		hits := []corpusNote{}
		for _, item := range corpus {
			if !scopeMatches(item.Note.Scope, scope) {
				continue
			}
			match := false
			switch stage {
			case "exact-title":
				work++
				match = len(query) == 1 && strings.ToLower(strings.TrimSpace(item.Note.Title)) == query[0]
			case "alias":
				for _, alias := range item.Note.Aliases {
					work++
					if len(query) == 1 && strings.ToLower(strings.TrimSpace(alias)) == query[0] {
						match = true
					}
				}
			case "keywords":
				fields := append([]string{item.Note.Title}, item.Note.Aliases...)
				fields = append(fields, item.Note.Keywords...)
				fields = append(fields, item.Note.Tags...)
				match = true
				for _, term := range query {
					found := false
					for _, field := range fields {
						work++
						if strings.ToLower(strings.TrimSpace(field)) == term {
							found = true
						}
					}
					if !found {
						match = false
					}
				}
			}
			if match {
				hits = append(hits, item)
			}
		}
		if len(hits) > 0 {
			sort.Slice(hits, func(i, j int) bool {
				a, b := hits[i], hits[j]
				if len(a.Note.Scope) != len(b.Note.Scope) {
					return len(a.Note.Scope) > len(b.Note.Scope)
				}
				if slices.Contains(plan.RankBy, "evidence-recency") && evidenceTime(a.Note) != evidenceTime(b.Note) {
					return evidenceTime(a.Note) > evidenceTime(b.Note)
				}
				return a.Ref.ID < b.Ref.ID
			})
			return hits, stage, work
		}
	}
	return []corpusNote{}, "none", work
}
func (s *Store) query(ctx context.Context, v *snapshot, r Request) (QueryResult, error) {
	if err := ctx.Err(); err != nil {
		return QueryResult{}, err
	}
	var q Query
	if err := Decode(r.Payload, &q); err != nil {
		return QueryResult{}, err
	}
	if q.View != "" && q.View != "search" && q.View != "core" || q.Limit < 0 || q.Limit > MaxQueryLimit {
		return QueryResult{}, problem(2, "InvalidInput")
	}
	corpus, partial, err := v.corpus(r)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return QueryResult{}, err
	}
	out := QueryResult{Hits: []Hit{}, Partial: partial}
	if q.View == "core" {
		out.Stage = "core"
		size := 0
		for _, item := range corpus {
			n := item.Note
			observed := slices.ContainsFunc(n.Sources, func(source Source) bool {
				return source.Kind == "native-owner" && source.Origin == "user-stated" && source.Assurance == "adapter-observed" && source.Ref == nativePreferenceRef(n)
			})
			if n.Kind != "preference" || n.TemporalClass != "stable" || n.LoadHint != "core" || !observed || !scopeMatches(n.Scope, r.RequestContext.Scope) {
				continue
			}
			if err := s.usable(v, item.Ref, r, map[string]bool{}, 0); err != nil {
				continue
			}
			review, err := s.reviewForRequest(v, item.Ref, n, r)
			if err != nil {
				return out, err
			}
			hit := Hit{Ref: item.Ref, Note: n, Review: review}
			b, _ := encode(hit)
			if len(out.Hits) >= 8 || size+len(b) > 4096 {
				out.Partial = &Partial{LimitHit: true, Reasons: []string{"CoreLimit"}}
				break
			}
			size += len(b)
			out.Hits = append(out.Hits, hit)
		}
		return out, nil
	}
	t, err := terms(q.QueryTerms)
	if err != nil {
		return out, err
	}
	plan := Baseline()
	var chosen *Ref
	if partial == nil {
		chosen, err = s.activeMethod(v, r)
		if err != nil {
			return out, err
		}
		if q.SelectionRef != nil {
			if err := v.current(*q.SelectionRef); err != nil {
				return out, err
			}
			if !v.allowed(*q.SelectionRef, r) {
				return out, problem(4, "DisclosureNotAllowed")
			}
			var use Use
			if err := v.read(*q.SelectionRef, &use); err != nil {
				return out, err
			}
			if use.OpportunityID != opportunityKey(t, r.RequestContext.Scope) || use.Compatibility["helperABI"] != "1" || use.Compatibility["profileRevision"] != profileRevision {
				return out, problem(3, "Conflict")
			}
			switch use.Assignment {
			case "trial":
				if r.RequestContext.NoCapture || r.RequestContext.Delegation != nil && r.RequestContext.Delegation.NoCapture {
					return out, problem(4, "CaptureProhibited")
				}
				current := v.m.Slots[scopeKey(r.RequestContext.Scope)]
				if use.SelectedRef == nil || !sameRef(use.SelectedRef, current.Trial) {
					return out, problem(3, "Conflict")
				}
				state := v.m.ProcedureStates[use.SelectedRef.Key()]
				if state.State != "trial" || state.Evaluation == nil || state.Exhausted {
					return out, problem(7, "EvidenceInsufficient")
				}
				var eval Evaluation
				if err := v.read(*state.Evaluation, &eval); err != nil {
					return out, err
				}
				if !sameRef(eval.BaselineRef, current.Active) || eval.CorpusDigest != corpusDigest(corpus) || eval.ProfileRevision != profileRevision {
					return out, problem(7, "EvidenceInsufficient")
				}
			case "active", "baseline":
				if !(use.Assignment == "baseline" && use.SelectedRef == nil) && !sameRef(use.SelectedRef, chosen) {
					return out, problem(3, "Conflict")
				}
			default:
				return out, problem(3, "Conflict")
			}
			chosen = use.SelectedRef
		}
		if chosen != nil {
			var proc Procedure
			if err := v.read(*chosen, &proc); err != nil {
				return out, err
			}
			if s.procedureUsable(v, *chosen, proc, r) == nil {
				if err := Decode(proc.Payload, &plan); err != nil {
					return out, err
				}
			}
		}
	}
	matches, stage, work := rankMatches(plan, corpus, t, r.RequestContext.Scope)
	// Only the first match decides correctness, so the full list is checked.
	if chosen != nil && !retrievalCorrect(matches, oracle(corpus, t, r.RequestContext.Scope)) {
		// A read-only lookup can fall back without writing evaluation evidence.
		plan = Baseline()
		matches, stage, work = rankMatches(plan, corpus, t, r.RequestContext.Scope)
	}
	limit := plan.MaxResults
	if q.Limit > 0 {
		limit = q.Limit
	}
	hits := matches[:min(len(matches), limit)]
	out.Stage = stage
	out.WorkUnits = work
	if len(matches) > len(hits) {
		out.addLimit("ResultLimit", len(matches)-len(hits))
	}
	// Only when no title, alias or keyword equals the query does the helper
	// rank notes by shared words. Lower-ranked word matches are counted in
	// textMatches, not reported as omitted.
	matched := map[string][]string{}
	if stage == "none" {
		ranked, textWork, err := rankText(ctx, corpus, t, r.RequestContext.Scope)
		if err != nil {
			return out, err
		}
		out.WorkUnits += textWork
		out.TextMatches = len(ranked)
		for _, hit := range ranked[:min(len(ranked), limit)] {
			hits = append(hits, hit.item)
			matched[hit.item.Ref.ID] = hit.matched
		}
		if len(hits) > 0 {
			out.Stage = "text"
		}
	}
	total := 0
	outputLimit := 32 << 10
	if q.Limit == 1 {
		// A stored 32 KiB note can expand sixfold when JSON escapes its body.
		// Explicit single-hit lookup is the bounded full-detail path.
		outputLimit = 224 << 10
	}
	for _, item := range hits {
		review, err := s.reviewForRequest(v, item.Ref, item.Note, r)
		if err != nil {
			return out, err
		}
		conflicts := []Ref{}
		for _, other := range corpus {
			for _, rel := range other.Note.Relations {
				if rel.TargetRef.ID == item.Ref.ID {
					conflicts = append(conflicts, other.Ref)
				}
			}
		}
		for _, rel := range item.Note.Relations {
			if rel.Type == "contradicts" {
				conflicts = append(conflicts, rel.TargetRef)
			}
		}
		hit := Hit{item.Ref, item.Note, review, conflicts, matched[item.Ref.ID]}
		b, _ := encode(hit)
		if total+len(b) > outputLimit {
			out.addLimit("OutputLimit", len(hits)-len(out.Hits))
			break
		}
		total += len(b)
		out.Hits = append(out.Hits, hit)
	}
	return out, nil
}

func (out *QueryResult) addLimit(reason string, omitted int) {
	if out.Partial == nil {
		out.Partial = &Partial{}
	}
	out.Partial.LimitHit = true
	out.Partial.Omitted += omitted
	out.Partial.Reasons = append(out.Partial.Reasons, reason)
}

func opportunityKey(t []string, scope Scope) string {
	copyTerms := slices.Clone(t)
	slices.Sort(copyTerms)
	b, _ := encode(struct {
		Terms []string
		Scope Scope
	}{copyTerms, scope})
	return digest(b)
}
