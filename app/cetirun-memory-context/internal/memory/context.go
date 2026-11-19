// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package memory

import (
	"regexp"
	"strings"
)

const MaxExtraTurnsPer24h = 4

var identityPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

func defaultLearning() Learning {
	return Learning{Mode: "bounded-auto", Profiles: []string{RetrievalProfile, ArtifactProfile}, MaxExtraTurnsPer24h: MaxExtraTurnsPer24h, AllowModelEvaluation: true}
}

// Request restrictions come from the current entrypoint, not a persisted policy.
func validateContext(r Request, write bool, maintenance bool) error {
	if r.ContractVersion != ContractVersion {
		return problem(6, "SchemaUnsupported")
	}
	if !identityPattern.MatchString(r.RequestID) {
		return problem(2, "InvalidInput")
	}
	c := r.RequestContext
	if len(c.CurrentSourceChecks) > 8 || len(c.CurrentSourceChecks) > 0 && !r.native {
		return problem(4, "SourceAssuranceUnavailable")
	}
	if len(c.OpportunityID) > 256 || validateScope(c.Scope) != nil {
		return problem(2, "InvalidInput")
	}
	if c.NoMemory && !maintenance {
		return problem(4, "MemoryDisabled")
	}
	if c.Audience != "owner-private" {
		return problem(4, "AudienceUnknown")
	}
	if c.Destination.Kind != "owner-private" && c.Destination.Kind != "delegated-task" {
		return problem(4, "DisclosureNotAllowed")
	}
	if c.Destination.Ref != nil && len(*c.Destination.Ref) > 256 {
		return problem(2, "InvalidInput")
	}
	if c.Destination.Kind == "delegated-task" && c.Delegation == nil {
		return problem(4, "AudienceUnknown")
	}
	if d := c.Delegation; d != nil {
		if maintenance || !identityPattern.MatchString(d.ParentRequestID) || d.Purpose == "" || len(d.Purpose) > 512 || len(d.AllowedRefs) > 16 {
			return problem(4, "DisclosureNotAllowed")
		}
		if d.NoMemory || write && d.NoCapture {
			return problem(4, "CaptureProhibited")
		}
		for _, ref := range d.AllowedRefs {
			if !ref.Valid() {
				return problem(2, "InvalidInput")
			}
		}
	}
	if write && c.NoCapture && !maintenance {
		return problem(4, "CaptureProhibited")
	}
	if write && r.ExpectedGeneration == nil {
		return problem(2, "InvalidInput")
	}
	return nil
}

func validateScope(s Scope) error {
	if len(s) > 8 {
		return problem(2, "InvalidInput")
	}
	for k, v := range s {
		if !identityPattern.MatchString(k) || len(v) > 256 || strings.TrimSpace(v) == "" || strings.ContainsAny(v, "\x00\r\n") {
			return problem(2, "InvalidInput")
		}
	}
	return nil
}

func scopeMatches(required, actual Scope) bool {
	for k, v := range required {
		if actual[k] != v {
			return false
		}
	}
	return true
}

func scopeKey(s Scope) string { b, _ := encode(s); return digest(b) }
