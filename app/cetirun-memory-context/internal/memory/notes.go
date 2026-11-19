// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package memory

import (
	"encoding/json"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// secretPattern matches text that carries a credential: a private key, a
// bearer or API token in a well-known format, a JSON Web Token, credentials
// in a URL, or a credential URL parameter. Token formats are matched with
// their case.
var secretPattern = regexp.MustCompile(`(?i)(-----BEGIN [A-Z ]*PRIVATE KEY-----|\bBearer\s+[A-Za-z0-9._~+/-]{8,}|\bsk-(?:proj-)?[A-Za-z0-9_-]{20,}|[?&](?:access_token|api_key|password|secret|token|sig)=|` +
	`(?-i:\bgh[pousr]_[A-Za-z0-9]{30,}|\bgithub_pat_[A-Za-z0-9_]{22,}|\bglpat-[A-Za-z0-9_-]{20,}|\bxox[abposr]-[A-Za-z0-9-]{10,}|\b(?:AKIA|ASIA)[0-9A-Z]{16}\b|\bAIza[0-9A-Za-z_-]{35}|\bnpm_[A-Za-z0-9]{36}|` +
	`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,})|` +
	`\b[a-z][a-z0-9+.-]*://[^/\s:@]+:[^/\s@]+@)`)

// credentialAssignment matches a value assigned to a name that ends like a
// credential: API_KEY=value, "token": "value", password: value.
var credentialAssignment = regexp.MustCompile(`(?i)[a-z0-9_.-]*(?:api[_-]?key|access[_-]?key|account[_-]?key|auth[_-]?key|private[_-]?key|secret[_-]?key|signing[_-]?key|secret|token|passw(?:or)?d|credentials?)["']?\s*[:=]\s*["']?([^\s"'<>,;]+)`)

func validText(text string, max int) bool {
	return len(text) <= max && utf8.ValidString(text) && !strings.ContainsRune(text, 0) && !secretPattern.MatchString(text) && !assignsCredential(text)
}

// assignsCredential reports whether text assigns a token-like value to a
// credential name. Values that are short, read as words, numbers or dates, or
// refer elsewhere (a path, a URL, a variable or a template) do not count.
func assignsCredential(text string) bool {
	for _, match := range credentialAssignment.FindAllStringSubmatch(text, -1) {
		if tokenLike(match[1]) {
			return true
		}
	}
	return false
}

func tokenLike(value string) bool {
	if len(value) < 16 || strings.ContainsAny(value[:1], "/~.$%{[(") || strings.Contains(value, "://") {
		return false
	}
	letters, digits := 0, 0
	for _, c := range value {
		switch {
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			letters++
		case c >= '0' && c <= '9':
			digits++
		}
	}
	return letters >= 4 && digits >= 2
}
func validTime(value *string) bool {
	if value == nil {
		return true
	}
	parsed, err := time.Parse(time.RFC3339, *value)
	return err == nil && parsed.Location() == time.UTC
}
func validSource(value string) bool {
	if strings.TrimSpace(value) == "" || !validText(value, 2048) || strings.ContainsAny(value, "\r\n") {
		return false
	}
	u, err := url.Parse(value)
	if err != nil {
		return false
	}
	return u.User == nil && (u.Scheme == "" || u.Scheme == "https" || u.Scheme == "http")
}
func textList(values []string, maxCount, maxLength int) bool {
	if len(values) > maxCount {
		return false
	}
	seen := map[string]bool{}
	for _, v := range values {
		if strings.TrimSpace(v) == "" || !validText(v, maxLength) || seen[v] {
			return false
		}
		seen[v] = true
	}
	return true
}
func (n *Note) validate(preservedSources ...Source) error {
	if !slices.Contains([]string{"fact", "preference", "decision", "lesson"}, n.Kind) || n.Title == "" || !validText(n.Title, 160) || !validText(n.Body, 32<<10) || strings.TrimSpace(n.Body) == "" || validateScope(n.Scope) != nil {
		return problem(2, "InvalidInput")
	}
	if n.Status == "" {
		n.Status = "active"
	}
	if !slices.Contains([]string{"active", "contested", "deprecated"}, n.Status) {
		return problem(2, "InvalidInput")
	}
	if n.SchemaVersion != "" && n.SchemaVersion != SchemaVersion {
		return problem(6, "SchemaUnsupported")
	}
	n.SchemaVersion = SchemaVersion
	if !slices.Contains([]string{"stable", "historical", "live"}, n.TemporalClass) {
		return problem(2, "InvalidInput")
	}
	if n.LoadHint == "" {
		n.LoadHint = "on-demand"
	}
	if n.LoadHint != "on-demand" && (n.LoadHint != "core" || n.Kind != "preference" || n.TemporalClass != "stable") {
		return problem(2, "InvalidInput")
	}
	if !slices.Contains([]string{"on-use", "on-change", "after-date"}, n.Revalidation.Mode) || !validTime(n.Revalidation.ReviewAfter) {
		return problem(2, "InvalidInput")
	}
	if (n.Revalidation.Mode == "after-date") != (n.Revalidation.ReviewAfter != nil) || n.TemporalClass == "live" && n.Revalidation.Mode != "on-use" {
		return problem(2, "InvalidInput")
	}
	if (n.Kind == "decision" && !slices.Contains([]string{"proposed", "confirmed", "exception", "superseded"}, n.DecisionState)) || (n.Kind != "decision" && n.DecisionState != "") {
		return problem(2, "InvalidInput")
	}
	if len(n.Sources) < 1 || len(n.Sources) > 8 || len(n.DerivedFrom) > 8 || len(n.Relations) > 8 {
		return problem(2, "InvalidInput")
	}
	for i := range n.Sources {
		s := &n.Sources[i]
		// Only the complete, unchanged source read from the existing note may
		// retain text accepted before the secret checks were tightened. Revise
		// requires this provenance to survive, so rechecking it would prevent
		// even harmless body/status corrections. New or edited sources still
		// pass every content check; callers cannot supply preservedSources.
		preserved := slices.ContainsFunc(preservedSources, func(original Source) bool { return reflect.DeepEqual(original, *s) })
		if !identityPattern.MatchString(s.Kind) || !validTime(s.ObservedAt) {
			return problem(2, "InvalidInput")
		}
		if !preserved && (!validSource(s.Ref) || s.CurrentRef != nil && !validSource(*s.CurrentRef) || s.Version != nil && !validText(*s.Version, 256)) {
			return problem(2, "InvalidInput")
		}
		if !slices.Contains([]string{"user-stated", "tool-observed", "external-claim", "agent-inferred"}, s.Origin) {
			return problem(2, "InvalidInput")
		}
		if s.Assurance != "" && s.Assurance != "reported" && !preserved {
			return problem(2, "InvalidInput")
		}
		if !preserved {
			s.Assurance = "reported"
		}
		if s.Origin == "external-claim" && s.ObservedAt == nil && n.Revalidation.Mode != "on-use" {
			return problem(2, "InvalidInput")
		}
	}
	for _, d := range n.DerivedFrom {
		if !d.Ref.Valid() || !slices.Contains([]string{"support", "counterexample", "context"}, d.Role) {
			return problem(2, "InvalidInput")
		}
	}
	for _, r := range n.Relations {
		if !r.TargetRef.Valid() || !strings.HasPrefix(r.TargetRef.ID, "note-") || !slices.Contains([]string{"contradicts", "supersedes"}, r.Type) || strings.TrimSpace(r.Reason) == "" || !validText(r.Reason, 1024) {
			return problem(2, "InvalidInput")
		}
	}
	if !textList(n.Aliases, 16, 64) || !textList(n.Keywords, 16, 64) || !textList(n.Tags, 16, 64) {
		return problem(2, "InvalidInput")
	}
	return nil
}

type captureInput struct {
	Note        *Note        `json:"note,omitempty"`
	Observation *Observation `json:"observation,omitempty"`
}

func (s *Store) capture(v *snapshot, r Request) (writeResult, error) {
	var input captureInput
	if err := Decode(r.Payload, &input); err != nil {
		return writeResult{}, err
	}
	if (input.Note == nil) == (input.Observation == nil) {
		return writeResult{}, problem(2, "InvalidInput")
	}
	if input.Note != nil {
		n := *input.Note
		if n.ID != "" || n.Revision != 0 || n.UpdatedAt != "" {
			return writeResult{}, problem(2, "InvalidInput")
		}
		if err := n.validate(); err != nil {
			return writeResult{}, err
		}
		if !scopeMatches(r.RequestContext.Scope, n.Scope) {
			return writeResult{}, problem(2, "ScopeMismatch")
		}
		if err := v.validateLinks(n.DerivedFrom, nil, n.Relations, r); err != nil {
			return writeResult{}, err
		}
		n.ID = newID("note")
		n.Revision = 1
		n.UpdatedAt = timestamp(s.now())
		ref, err := v.save(n.ID, 1, n)
		if err != nil {
			return writeResult{}, err
		}
		return writeResult{Refs: []Ref{ref}}, nil
	}
	o := *input.Observation
	if o.ID != "" || o.Revision != 0 || o.SourceCheck != nil || o.ProfileRef != nil || o.Assurance != "" && o.Assurance != "reported" {
		return writeResult{}, problem(2, "InvalidInput")
	}
	if !validText(o.RedactedSummary, 1024) || !validText(o.Rationale, 512) || !validSource(o.SourceRef) || !identityPattern.MatchString(o.SourceKind) || !slices.Contains([]string{"success", "failure", "correction", "unknown"}, o.Outcome) {
		return writeResult{}, problem(2, "InvalidInput")
	}
	if len(v.m.Pending) >= 256 {
		return writeResult{}, problem(5, "LimitReached")
	}
	o.ID = newID("obs")
	o.Revision = 1
	o.Assurance = "reported"
	o.OpportunityID = r.RequestContext.OpportunityID
	o.ObservedAt = timestamp(s.now())
	ref, err := v.save(o.ID, 1, o)
	if err != nil {
		return writeResult{}, err
	}
	v.m.Pending = append(v.m.Pending, o.ID)
	return writeResult{Refs: []Ref{ref}}, nil
}
func (v *snapshot) validateLinks(derived []Derived, required []Ref, relations []Relation, r Request) error {
	refs := []Ref{}
	for _, d := range derived {
		if !slices.Contains([]string{"support", "counterexample", "context"}, d.Role) {
			return problem(2, "InvalidInput")
		}
		refs = append(refs, d.Ref)
	}
	refs = append(refs, required...)
	for _, x := range relations {
		refs = append(refs, x.TargetRef)
	}
	for _, ref := range refs {
		if !v.allowed(ref, r) {
			return problem(4, "DisclosureNotAllowed")
		}
		if v.m.Tombstones[ref.ID] {
			return problem(6, "SourceForgotten")
		}
		if !ref.Valid() {
			return problem(2, "InvalidInput")
		}
		name, _ := objectPath(ref)
		b, ok := v.added[ref.Key()]
		var err error
		if !ok {
			b, err = v.fs.read(name, 32<<10)
		}
		if err != nil {
			return err
		}
		if digest(b) != ref.Digest {
			return problem(6, "IntegrityMismatch")
		}
	}
	return nil
}

type reviseInput struct {
	Ref          Ref    `json:"ref"`
	Note         Note   `json:"note"`
	Reason       string `json:"reason"`
	EvidenceRefs []Ref  `json:"evidenceRefs"`
}

func (s *Store) revise(v *snapshot, r Request) (writeResult, error) {
	var input reviseInput
	if err := Decode(r.Payload, &input); err != nil {
		return writeResult{}, err
	}
	if err := v.current(input.Ref); err != nil {
		return writeResult{}, err
	}
	if !v.allowed(input.Ref, r) {
		return writeResult{}, problem(4, "DisclosureNotAllowed")
	}
	var old Note
	if err := v.read(input.Ref, &old); err != nil {
		return writeResult{}, err
	}
	n := input.Note
	if n.ID != "" && n.ID != old.ID || n.Revision != 0 && n.Revision != old.Revision || n.UpdatedAt != "" && n.UpdatedAt != old.UpdatedAt {
		return writeResult{}, problem(3, "Conflict")
	}
	if err := n.validate(old.Sources...); err != nil {
		return writeResult{}, err
	}
	if !scopeMatches(r.RequestContext.Scope, n.Scope) || len(input.EvidenceRefs) > 8 || !validText(input.Reason, 1024) || strings.TrimSpace(input.Reason) == "" {
		return writeResult{}, problem(2, "InvalidInput")
	}
	if old.Status == "contested" && n.Status != "contested" && len(input.EvidenceRefs) == 0 {
		return writeResult{}, problem(7, "EvidenceInsufficient")
	}
	for _, source := range old.Sources {
		found := false
		for _, next := range n.Sources {
			if reflect.DeepEqual(source, next) {
				found = true
				break
			}
		}
		if !found {
			return writeResult{}, problem(2, "SourceLineageRequired")
		}
	}
	if err := v.validateLinks(n.DerivedFrom, input.EvidenceRefs, n.Relations, r); err != nil {
		return writeResult{}, err
	}
	for _, ref := range input.EvidenceRefs {
		if !slices.ContainsFunc(n.DerivedFrom, func(d Derived) bool { return d.Ref == ref }) {
			n.DerivedFrom = append(n.DerivedFrom, Derived{Ref: ref, Role: "support"})
		}
	}
	if len(n.DerivedFrom) > 8 {
		return writeResult{}, problem(5, "LimitReached")
	}
	n.ID = old.ID
	revision, err := v.nextRevision(input.Ref)
	if err != nil {
		return writeResult{}, err
	}
	n.Revision = revision
	n.UpdatedAt = timestamp(s.now())
	ref, err := v.save(n.ID, n.Revision, n)
	if err != nil {
		return writeResult{}, err
	}
	return writeResult{Refs: []Ref{ref}}, nil
}

type sourceCheckInput struct {
	Check   SourceCheck `json:"check"`
	Summary string      `json:"summary"`
}

func (s *Store) recordSourceCheck(v *snapshot, r Request) (writeResult, error) {
	var input sourceCheckInput
	if err := Decode(r.Payload, &input); err != nil {
		return writeResult{}, err
	}
	c := input.Check
	if err := v.current(c.TargetRef); err != nil {
		return writeResult{}, err
	}
	if !v.allowed(c.TargetRef, r) {
		return writeResult{}, problem(4, "DisclosureNotAllowed")
	}
	var note Note
	if err := v.read(c.TargetRef, &note); err != nil {
		return writeResult{}, err
	}
	found := false
	for _, source := range note.Sources {
		if source.Ref == c.SourceRef {
			found = true
		}
	}
	if !found || !slices.Contains([]string{"matches", "changed", "unavailable", "unresolved"}, c.Result) || !validText(input.Summary, 1024) || !validSource(c.SourceRef) || c.EvidenceRef != nil && !validSource(*c.EvidenceRef) || c.CurrentVersion != nil && !validText(*c.CurrentVersion, 256) {
		return writeResult{}, problem(2, "InvalidInput")
	}
	// An ordinary caller cannot extend a review deadline or mint an observed
	// result. This path records a report; a qualified verifier is separate.
	if c.NextReviewAfter != nil {
		return writeResult{}, problem(7, "EvidenceInsufficient")
	}
	o := Observation{ID: newID("obs"), Revision: 1, OpportunityID: r.RequestContext.OpportunityID, ObservedAt: timestamp(s.now()), SourceKind: "source-check", Assurance: "reported", Outcome: "unknown", SourceRef: c.SourceRef, RedactedSummary: input.Summary, SourceCheck: &c}
	ref, err := v.save(o.ID, 1, o)
	if err != nil {
		return writeResult{}, err
	}
	key := c.TargetRef.Key()
	if len(v.m.SourceChecks[key]) >= 64 {
		return writeResult{}, problem(5, "LimitReached")
	}
	v.m.SourceChecks[key] = append(v.m.SourceChecks[key], ref)
	return writeResult{Refs: []Ref{ref}}, nil
}

type review struct {
	NeedsRecheck  bool    `json:"needsRecheck"`
	Reason        string  `json:"reviewReason"`
	LastCheckedAt *string `json:"lastCheckedAt"`
	LastResult    *string `json:"lastResult"`
	Assurance     string  `json:"assurance"`
}

func (s *Store) review(v *snapshot, ref Ref, n Note) (review, error) {
	out := review{Assurance: "reported"}
	if n.Status != "active" || n.DecisionState == "proposed" || n.DecisionState == "exception" || n.DecisionState == "superseded" {
		out.NeedsRecheck = true
		out.Reason = "Conflicted"
	}
	if n.TemporalClass == "live" || n.Revalidation.Mode == "on-use" {
		out.NeedsRecheck = true
		if out.Reason == "" {
			out.Reason = "OnUse"
		}
	}
	if n.Revalidation.ReviewAfter != nil {
		due, err := time.Parse(time.RFC3339, *n.Revalidation.ReviewAfter)
		if err != nil {
			return out, problem(6, "IntegrityMismatch")
		}
		if !s.now().Before(due) {
			out.NeedsRecheck = true
			if out.Reason == "" {
				out.Reason = "ReviewDue"
			}
		}
	}
	checks := []Ref{}
	keys := []string{}
	for key := range v.m.SourceChecks {
		if strings.HasPrefix(key, ref.ID+"/") {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	for _, key := range keys {
		checks = append(checks, v.m.SourceChecks[key]...)
	}
	for _, oRef := range checks {
		var o Observation
		if err := v.read(oRef, &o); err != nil {
			return out, err
		}
		if o.SourceCheck == nil || o.SourceCheck.TargetRef.ID != ref.ID {
			return out, problem(6, "IntegrityMismatch")
		}
		// Inherit invalidation through preserved source lineage, not a past
		// successful check. Keep the on-disk revision/check index unchanged.
		if o.SourceCheck.TargetRef.Revision > ref.Revision || o.SourceCheck.TargetRef != ref && o.SourceCheck.Result == "matches" {
			continue
		}
		if out.LastCheckedAt == nil || o.ObservedAt >= *out.LastCheckedAt {
			at, result := o.ObservedAt, o.SourceCheck.Result
			out.LastCheckedAt = &at
			out.LastResult = &result
			out.Assurance = o.Assurance
		}
		switch o.SourceCheck.Result {
		case "changed", "unresolved":
			out.NeedsRecheck = true
			out.Reason = "SourceChanged"
		case "unavailable":
			out.NeedsRecheck = true
			if out.Reason == "" {
				out.Reason = "SourceUnavailable"
			}
		}
	}
	for _, link := range n.Relations {
		if link.Type == "contradicts" {
			out.NeedsRecheck, out.Reason = true, "Conflicted"
		}
	}
	// The manifest records every note's relation targets, so only notes that
	// may point at this one are read, however large the store is.
	for id, targets := range v.m.Dependencies {
		otherRef, current := v.m.Objects[id]
		if !current || !strings.HasPrefix(id, "note-") || !slices.ContainsFunc(targets, func(target Ref) bool { return target.ID == ref.ID }) {
			continue
		}
		var other Note
		if err := v.read(otherRef, &other); err != nil {
			if v.damagedObject(otherRef, err) {
				out.NeedsRecheck, out.Reason = true, "RelatedMemoryDamaged"
				continue
			}
			return out, err
		}
		for _, link := range other.Relations {
			if link.TargetRef.ID == ref.ID && (link.Type == "contradicts" || link.Type == "supersedes") {
				out.NeedsRecheck = true
				out.Reason = "Conflicted"
			}
		}
	}
	return out, nil
}

func (s *Store) usable(v *snapshot, ref Ref, r Request, seen map[string]bool, depth int) error {
	if depth > 8 || len(seen) >= MaxScan {
		return problem(7, "EvidenceInsufficient")
	}
	if seen[ref.Key()] {
		return problem(6, "DependencyCycle")
	}
	seen[ref.Key()] = true
	defer delete(seen, ref.Key())
	if err := v.current(ref); err != nil {
		return err
	}
	if !v.allowed(ref, r) {
		return problem(4, "DisclosureNotAllowed")
	}
	var n Note
	if err := v.read(ref, &n); err != nil {
		return err
	}
	if !scopeMatches(n.Scope, r.RequestContext.Scope) {
		return problem(2, "ScopeMismatch")
	}
	review, err := s.reviewForRequest(v, ref, n, r)
	if err != nil {
		return err
	}
	if review.NeedsRecheck {
		return problem(7, "KnowledgeNeedsRecheck")
	}
	for _, d := range n.DerivedFrom {
		if v.m.Tombstones[d.Ref.ID] {
			return problem(6, "SourceForgotten")
		}
		if d.Role == "support" && strings.HasPrefix(d.Ref.ID, "note-") {
			if err := s.usable(v, d.Ref, r, seen, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) reviewForRequest(v *snapshot, ref Ref, n Note, r Request) (review, error) {
	out, err := s.review(v, ref, n)
	if err != nil || !r.native {
		return out, err
	}
	allMatched := true
	for _, source := range n.Sources {
		var latest *CurrentSourceCheck
		for i := range r.RequestContext.CurrentSourceChecks {
			current := &r.RequestContext.CurrentSourceChecks[i]
			at, err := time.Parse(time.RFC3339, current.ObservedAt)
			if err != nil || s.now().Before(at) || s.now().Sub(at) > 5*time.Minute {
				continue
			}
			check := current.Check
			if current.ContextKey == r.nativeContextKey && current.Assurance == "adapter-observed" && check.TargetRef == ref && check.SourceRef == source.Ref &&
				(latest == nil || current.ObservedAt >= latest.ObservedAt) {
				latest = current
			}
		}
		if latest == nil || source.Kind != "file" || source.Version == nil || latest.Check.CurrentVersion == nil || *source.Version != *latest.Check.CurrentVersion || latest.Check.Result != "matches" {
			allMatched = false
			if latest != nil && latest.Check.Result != "matches" {
				out.NeedsRecheck = true
				if out.Reason != "Conflicted" && out.Reason != "SourceChanged" {
					out.Reason = "SourceChanged"
					if latest.Check.Result == "unavailable" {
						out.Reason = "SourceUnavailable"
					}
				}
				out.LastCheckedAt, out.LastResult = &latest.ObservedAt, &latest.Check.Result
			}
		}
	}
	if allMatched && out.NeedsRecheck && slices.Contains([]string{"OnUse", "ReviewDue"}, out.Reason) {
		out.NeedsRecheck, out.Reason, out.Assurance = false, "", "adapter-observed"
	}
	return out, nil
}

type batchInput struct {
	Operations              []json.RawMessage `json:"operations"`
	ProcessedObservationIDs []string          `json:"processedObservationIds"`
}
type operation struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// feedbackReminder bounds how long the latest pending observation prompts the
// model to prepare and propose. Pending observations stay available to
// prepare afterwards.
const feedbackReminder = 24 * time.Hour

// freshFeedback reports whether the newest pending observation is recent
// enough to remind the model of it.
func (v *snapshot) freshFeedback(now time.Time) bool {
	if len(v.m.Pending) == 0 {
		return false
	}
	ref, ok := v.m.Objects[v.m.Pending[len(v.m.Pending)-1]]
	if !ok {
		return false
	}
	var o Observation
	if v.read(ref, &o) != nil {
		return false
	}
	at, err := time.Parse(time.RFC3339, o.ObservedAt)
	return err == nil && now.Sub(at) < feedbackReminder
}

// processObservations removes observations from the pending list.
func (v *snapshot) processObservations(ids []string) {
	for _, id := range ids {
		if slices.Contains(v.m.Pending, id) {
			v.m.Processed[id] = true
			v.m.Pending = slices.DeleteFunc(v.m.Pending, func(x string) bool { return x == id })
		}
	}
}

func (s *Store) commitBatch(v *snapshot, r Request) (writeResult, error) {
	var input batchInput
	if err := Decode(r.Payload, &input); err != nil {
		return writeResult{}, err
	}
	// A batch without operations only marks observations as processed, for
	// feedback that needs no change.
	if len(input.Operations) > 8 || len(input.ProcessedObservationIDs) > 8 || len(input.Operations) == 0 && len(input.ProcessedObservationIDs) == 0 {
		return writeResult{}, problem(2, "InvalidInput")
	}
	pending := slices.Clone(v.m.Pending)
	result := writeResult{Refs: []Ref{}}
	for _, raw := range input.Operations {
		var op operation
		if err := Decode(raw, &op); err != nil {
			return result, err
		}
		child := r
		child.Payload = op.Payload
		var next writeResult
		var err error
		switch op.Type {
		case "add-note":
			next, err = s.capture(v, child)
		case "revise-note", "mark-note-state":
			next, err = s.revise(v, child)
		case "add-procedure-version":
			next, err = s.propose(v, child)
		default:
			err = problem(2, "InvalidInput")
		}
		if err != nil {
			return result, err
		}
		result.Refs = append(result.Refs, next.Refs...)
	}
	for _, id := range input.ProcessedObservationIDs {
		// A proposal in this batch may already have processed the observation.
		if !slices.Contains(pending, id) && !slices.Contains(v.m.Pending, id) || v.m.Tombstones[id] {
			return result, problem(3, "Conflict")
		}
		if !v.allowed(v.m.Objects[id], r) {
			return result, problem(4, "DisclosureNotAllowed")
		}
		v.processObservations([]string{id})
	}
	return result, nil
}
