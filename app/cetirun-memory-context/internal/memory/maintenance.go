// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"
)

var atomicTemporaryName = regexp.MustCompile(`^\.tmp-tx-[a-f0-9]{32}$`)

func atomicTemporary(name string) bool {
	if !atomicTemporaryName.MatchString(filepath.Base(name)) {
		return false
	}
	return slices.Contains([]string{"knowledge", "lessons", "procedures", "state", "state/manifests"}, filepath.Dir(name))
}

type ForgetPlan struct {
	Generation int64    `json:"generation"`
	RootRefs   []Ref    `json:"rootRefs"`
	ObjectIDs  []string `json:"objectIds"`
	Files      []string `json:"files"`
	Digest     string   `json:"digest"`
}

func (p ForgetPlan) hash() string { p.Digest = ""; b, _ := encode(p); return digest(b) }

type forgetInput struct {
	Refs []Ref `json:"refs"`
}
type forgetApplyInput struct {
	Plan ForgetPlan `json:"plan"`
}

func dependencies(v *snapshot, ref Ref) ([]Ref, error) {
	refs := []Ref{}
	switch {
	case strings.HasPrefix(ref.ID, "note-"):
		var x Note
		if err := v.read(ref, &x); err != nil {
			return nil, err
		}
		for _, d := range x.DerivedFrom {
			refs = append(refs, d.Ref)
		}
		for _, link := range x.Relations {
			refs = append(refs, link.TargetRef)
		}
	case strings.HasPrefix(ref.ID, "proc-"):
		var x Procedure
		if err := v.read(ref, &x); err != nil {
			return nil, err
		}
		refs = append(refs, x.RequiredKnowledgeRefs...)
		for _, d := range x.DerivedFrom {
			refs = append(refs, d.Ref)
		}
		for _, id := range x.SourceObservationIDs {
			if r, ok := v.m.Objects[id]; ok {
				refs = append(refs, r)
			}
		}
		if x.Parent != nil {
			refs = append(refs, *x.Parent)
		}
	case strings.HasPrefix(ref.ID, "obs-"):
		var x Observation
		if err := v.read(ref, &x); err != nil {
			return nil, err
		}
		if x.SourceCheck != nil {
			refs = append(refs, x.SourceCheck.TargetRef)
		}
	case strings.HasPrefix(ref.ID, "eval-"):
		var x Evaluation
		if err := v.read(ref, &x); err != nil {
			return nil, err
		}
		refs = append(refs, x.CandidateRef)
		if x.BaselineRef != nil {
			refs = append(refs, *x.BaselineRef)
		}
	case strings.HasPrefix(ref.ID, "use-"):
		var x Use
		if err := v.read(ref, &x); err != nil {
			return nil, err
		}
		refs = append(refs, x.InjectedRefs...)
		refs = append(refs, x.CitedRefs...)
		if x.SelectedRef != nil {
			refs = append(refs, *x.SelectedRef)
		}
		if x.OutcomeRef != nil {
			refs = append(refs, *x.OutcomeRef)
		}
	case strings.HasPrefix(ref.ID, "change-"):
		var x Change
		if err := v.read(ref, &x); err != nil {
			return nil, err
		}
		refs = append(refs, x.AffectedRefs...)
	}
	return refs, nil
}
func (s *Store) forgetPlan(v *snapshot, r Request) (ForgetPlan, error) {
	var input forgetInput
	if err := Decode(r.Payload, &input); err != nil {
		return ForgetPlan{}, err
	}
	if len(input.Refs) == 0 || len(input.Refs) > 8 {
		return ForgetPlan{}, problem(2, "InvalidInput")
	}
	ids := map[string]bool{}
	for _, ref := range input.Refs {
		if err := v.current(ref); err != nil {
			return ForgetPlan{}, err
		}
		ids[ref.ID] = true
	}
	files, _, err := v.fs.inventory()
	if err != nil {
		return ForgetPlan{}, err
	}
	dependencies := map[string][]Ref{}
	for id := range v.m.Objects {
		dependencies[id] = slices.Clone(v.m.Dependencies[id])
	}
	transactions := map[string][]byte{}
	for _, file := range files {
		if !strings.HasPrefix(file.Name, "transactions/") {
			continue
		}
		data, err := v.fs.read(file.Name, MaxInput*4)
		if err != nil {
			return ForgetPlan{}, err
		}
		var tx transaction
		if Decode(data, &tx) != nil || tx.Manifest.Dependencies == nil {
			return ForgetPlan{}, problem(6, "PendingDataUnresolved")
		}
		transactions[file.Name] = data
		// Objects may be durable before HEAD. Include their registered lineage
		// so forgetting cannot leave an interrupted derivative on disk.
		for id, ref := range tx.Manifest.Objects {
			if id != ref.ID || !ref.Valid() {
				return ForgetPlan{}, problem(6, "PendingDataUnresolved")
			}
			dependencies[id] = append(dependencies[id], tx.Manifest.Dependencies[id]...)
		}
	}
	for changed := true; changed; {
		changed = false
		for id, refs := range dependencies {
			if ids[id] {
				continue
			}
			for _, dep := range refs {
				if ids[dep.ID] {
					ids[id] = true
					changed = true
					break
				}
			}
		}
	}
	plan := ForgetPlan{Generation: v.m.Generation, RootRefs: input.Refs, ObjectIDs: []string{}, Files: []string{}}
	for id := range ids {
		plan.ObjectIDs = append(plan.ObjectIDs, id)
	}
	sort.Strings(plan.ObjectIDs)
	for _, file := range files {
		// Atomic scratch is never a committed object or a recovery input. Old
		// unnamed copies cannot be attributed to one note after interruption.
		matched := atomicTemporary(file.Name)
		for id := range ids {
			kind, _, _ := strings.Cut(id, "-")
			if strings.HasPrefix(file.Name, "objects/"+kind+"/"+id+"/") || file.Name == "knowledge/"+id+".md" || file.Name == "lessons/"+id+".md" || file.Name == "procedures/"+id+".json" || strings.HasPrefix(file.Name, "knowledge/"+id+".md.conflict-") || strings.HasPrefix(file.Name, "lessons/"+id+".md.conflict-") || strings.HasPrefix(file.Name, "procedures/"+id+".json.conflict-") {
				matched = true
				break
			}
		}
		if b, pending := transactions[file.Name]; pending {
			for id := range ids {
				if bytes.Contains(b, []byte(id)) {
					matched = true
					break
				}
			}
		}
		if matched {
			plan.Files = append(plan.Files, file.Name)
		}
	}
	sort.Strings(plan.Files)
	plan.Digest = plan.hash()
	return plan, nil
}
func (s *Store) forgetApply(v *snapshot, r Request) (writeResult, error) {
	var input forgetApplyInput
	if err := Decode(r.Payload, &input); err != nil {
		return writeResult{}, err
	}
	plan := input.Plan
	if plan.Digest != plan.hash() || plan.Generation > v.m.Generation || plan.Generation < 1 {
		return writeResult{}, problem(3, "Conflict")
	}
	check := r
	check.Payload, _ = json.Marshal(forgetInput{plan.RootRefs})
	actual, err := s.forgetPlan(v, check)
	if err != nil {
		return writeResult{}, err
	}
	// Another turn may add injection/trace records, but not another private
	// note or method to the owner's approved deletion scope.
	for _, id := range actual.ObjectIDs {
		if !slices.Contains(plan.ObjectIDs, id) && !historyObject(id) {
			return writeResult{}, problem(3, "Conflict")
		}
	}
	for _, id := range plan.ObjectIDs {
		if !slices.Contains(actual.ObjectIDs, id) && !historyObject(id) {
			return writeResult{}, problem(3, "Conflict")
		}
	}
	for _, file := range actual.Files {
		if strings.HasPrefix(file, "objects/") && !strings.HasPrefix(file, "objects/use/") && !strings.HasPrefix(file, "objects/change/") && !slices.Contains(plan.Files, file) {
			return writeResult{}, problem(3, "Conflict")
		}
	}
	approvedDigest := plan.Digest
	plan = actual
	for _, id := range plan.ObjectIDs {
		if strings.HasPrefix(id, "note-") {
			v.notesChanged = true
		}
		v.m.Tombstones[id] = true
		delete(v.m.Objects, id)
		delete(v.m.Dependencies, id)
		delete(v.m.Processed, id)
		v.m.Pending = slices.DeleteFunc(v.m.Pending, func(x string) bool { return x == id })
		for key := range v.m.SourceChecks {
			if strings.HasPrefix(key, id+"/") {
				delete(v.m.SourceChecks, key)
			}
		}
		for key := range v.m.ProcedureStates {
			if strings.HasPrefix(key, id+"/") {
				if exposure := v.m.ArtifactExposure; exposure != nil {
					for i, owner := range exposure.Owners {
						if owner == digest([]byte(key)) {
							exposure.Owners[i] = "*"
						}
					}
				}
				delete(v.m.ProcedureStates, key)
			}
		}
	}
	for key, state := range v.m.ProcedureStates {
		if state.ArtifactEvidence != nil {
			for phase, evidence := range state.ArtifactEvidence.Phases {
				if evidence.Baseline != nil && v.m.Tombstones[evidence.Baseline.ID] {
					delete(state.ArtifactEvidence.Phases, phase)
				}
			}
			v.m.ProcedureStates[key] = state
		}
	}
	for key, current := range v.m.Slots {
		if current.Active != nil && v.m.Tombstones[current.Active.ID] {
			current.Active = nil
		}
		if current.Trial != nil && v.m.Tombstones[current.Trial.ID] {
			current.Trial = nil
		}
		if current.Baseline != nil && v.m.Tombstones[current.Baseline.ID] {
			current.Baseline = nil
		}
		v.m.Slots[key] = current
	}
	for key, refs := range v.m.SourceChecks {
		v.m.SourceChecks[key] = slices.DeleteFunc(refs, func(ref Ref) bool { return v.m.Tombstones[ref.ID] })
	}
	for id, call := range v.m.ExtraCalls {
		if v.m.Tombstones[call.Candidate.ID] || call.Baseline != nil && v.m.Tombstones[call.Baseline.ID] {
			// Retain spent quota and exposure of public fixtures, not private
			// lineage, model configuration, outcomes or content digests.
			v.m.ExtraCalls[id] = ModelCall{ID: id, ReservedAt: call.ReservedAt, Finished: true, Outcome: "forgotten", CaseSetDigest: call.CaseSetDigest, Case: call.Case, ProfileRevision: call.ProfileRevision}
		}
	}
	v.m.Purges[approvedDigest] = plan
	return writeResult{Refs: []Ref{}, PurgePending: true}, nil
}
func (s *Store) purge(ctx context.Context, planDigest string) bool {
	v, err := s.openSnapshot(ctx, syscall.LOCK_EX)
	if err != nil {
		return false
	}
	defer v.fs.close()
	plan, ok := v.m.Purges[planDigest]
	if !ok {
		// Only a committed forget reaches here. A later write may already
		// have completed and retired this purge before its receipt is replayed.
		return true
	}
	if plan.Digest != plan.hash() {
		return false
	}
	return v.purgeFiles(ctx, plan) == nil
}

func (v *snapshot) purgeFiles(ctx context.Context, plan ForgetPlan) error {
	if plan.Digest != plan.hash() {
		return problem(6, "IntegrityMismatch")
	}
	for _, file := range plan.Files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := v.fs.remove(file); err != nil {
			return err
		}
	}
	return nil
}

func (v *snapshot) resumePurges(ctx context.Context) error {
	for key, plan := range v.m.Purges {
		if err := v.purgeFiles(ctx, plan); err != nil {
			return err
		}
		delete(v.m.Purges, key)
	}
	return nil
}

func (v *snapshot) pendingPurges() ([]string, error) {
	keys := []string{}
	for key, plan := range v.m.Purges {
		if plan.Digest != plan.hash() {
			return nil, problem(6, "IntegrityMismatch")
		}
		for _, name := range plan.Files {
			f, err := v.fs.open(name, syscall.O_RDONLY, false)
			if errors.Is(err, syscall.ENOENT) {
				continue
			}
			if err != nil {
				return nil, err
			}
			f.Close()
			keys = append(keys, key)
			break
		}
	}
	sort.Strings(keys)
	return keys, nil
}

type traceEntry struct {
	Ref          Ref         `json:"ref"`
	Relationship string      `json:"relationship"`
	Outputs      []OutputRef `json:"outputs,omitempty"`
}
type traceResult struct {
	Entries []traceEntry `json:"entries"`
	Partial Partial      `json:"partial"`
}

func (s *Store) trace(v *snapshot, r Request) (traceResult, error) {
	var input refInput
	if err := Decode(r.Payload, &input); err != nil {
		return traceResult{}, err
	}
	if !input.Ref.Valid() {
		return traceResult{}, problem(2, "InvalidInput")
	}
	if !v.allowed(input.Ref, r) {
		return traceResult{}, problem(4, "DisclosureNotAllowed")
	}
	result := traceResult{Entries: []traceEntry{}, Partial: Partial{Reasons: []string{"RegisteredReferencesOnly", "UnobservedActivityUnknown"}}}
	if v.m.HistoryCompacted {
		result.Partial.LimitHit = true
		result.Partial.Reasons = append(result.Partial.Reasons, "HistoryCompacted")
	}
	ids := []string{}
	for id := range v.m.Objects {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for i, id := range ids {
		if i >= MaxScan || len(result.Entries) >= 32 {
			result.Partial.LimitHit = true
			result.Partial.Unscanned = len(ids) - i
			break
		}
		ref := v.m.Objects[id]
		if !v.allowed(ref, r) {
			continue
		}
		deps, err := dependencies(v, ref)
		if err != nil {
			return result, err
		}
		if slices.Contains(deps, input.Ref) {
			entry := traceEntry{Ref: ref, Relationship: "derived"}
			if strings.HasPrefix(id, "use-") {
				var use Use
				if err := v.read(ref, &use); err != nil {
					return result, err
				}
				entry.Relationship = "cited"
				if slices.Contains(use.InjectedRefs, input.Ref) {
					entry.Relationship = "injected"
				}
				entry.Outputs = use.OutputRefs
			}
			result.Entries = append(result.Entries, entry)
		}
	}
	return result, nil
}
func (s *Store) recordUse(v *snapshot, r Request) (writeResult, error) {
	var use Use
	if err := Decode(r.Payload, &use); err != nil {
		return writeResult{}, err
	}
	if use.ID != "" || use.Revision != 0 || len(use.InjectedRefs) > 0 || len(use.CitedRefs) > 16 || len(use.OutputRefs) > 8 || use.ProfileRef != nil || use.SelectedRef != nil || use.OutcomeRef != nil || use.TraceCoverage.Assurance != "" && use.TraceCoverage.Assurance != "reported" {
		return writeResult{}, problem(2, "InvalidInput")
	}
	for _, ref := range use.CitedRefs {
		if err := v.current(ref); err != nil {
			return writeResult{}, err
		}
		if !v.allowed(ref, r) {
			return writeResult{}, problem(4, "DisclosureNotAllowed")
		}
	}
	for _, ref := range use.OutputRefs {
		if !validSource(ref.Ref) || !identityPattern.MatchString(ref.Kind) || ref.Digest != nil && !digestPattern.MatchString(*ref.Digest) {
			return writeResult{}, problem(2, "InvalidInput")
		}
	}
	use.ID = newID("use")
	use.Revision = 1
	use.OpportunityID = r.RequestContext.OpportunityID
	use.ManifestGeneration = v.m.Generation
	use.Assignment = "none"
	use.Compatibility = nil
	use.TraceCoverage = TraceCoverage{Status: "partial", Reasons: []string{"SelfReportedReferences"}, Assurance: "reported"}
	ref, err := v.save(use.ID, 1, use)
	return writeResult{Refs: []Ref{ref}}, err
}

type ImportFile struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
}
type ImportPlan struct {
	Files  []ImportFile `json:"files"`
	Scope  Scope        `json:"scope"`
	Digest string       `json:"digest"`
}
type importInput struct {
	Files []string `json:"files"`
}
type importApplyInput struct {
	Plan ImportPlan `json:"plan"`
}

func (p ImportPlan) hash() string { p.Digest = ""; b, _ := encode(p); return digest(b) }

// Files in HOME that conventionally hold credentials, private keys or shell
// history, which import refuses whatever their content: their formats vary
// too much for the secret check alone.
var (
	credentialDirectories = []string{".ssh/", ".gnupg/", ".aws/", ".azure/", ".kube/", ".docker/", ".config/gh/",
		".config/gcloud/", ".config/github-copilot/", ".password-store/", ".local/share/keyrings/",
		".openclaw/credentials/"}
	credentialPaths = []string{".hermes/config.yaml"}
	credentialNames = []string{".env", ".netrc", "_netrc", ".git-credentials", ".npmrc", ".pypirc", ".pgpass", ".my.cnf",
		".boto", ".s3cfg", ".dockercfg", ".htpasswd", ".vault-token", ".history", ".viminfo", ".lesshst", "credentials",
		"credentials.json", ".credentials.json", "oauth_creds.json", "auth.json", "auth-profiles.json", "kubeconfig"}
	credentialNamePatterns = []string{".env.*", "*_history", "id_rsa*", "id_dsa*", "id_ecdsa*", "id_ed25519*", "*.pem", "*.key",
		"*.p12", "*.pfx", "*.jks", "*.keystore", "*.kdbx", "*.kubeconfig", "client_secret*.json", "openclaw.json*"}
)

// Match nested homes too, so changing the import boundary cannot hide these paths.
func credentialFile(rel string) bool {
	for suffix := rel; suffix != ""; {
		if slices.Contains(credentialPaths, suffix) || slices.ContainsFunc(credentialDirectories, func(dir string) bool { return strings.HasPrefix(suffix, dir) }) {
			return true
		}
		if parts := strings.Split(suffix, "/"); len(parts) > 4 && parts[0] == ".openclaw" && parts[1] == "agents" && parts[3] == "agent" {
			return true
		}
		_, suffix, _ = strings.Cut(suffix, "/")
	}
	name := strings.ToLower(filepath.Base(rel))
	if slices.Contains(credentialNames, name) {
		return true
	}
	return slices.ContainsFunc(credentialNamePatterns, func(pattern string) bool {
		matched, _ := filepath.Match(pattern, name)
		return matched
	})
}

func (s *Store) importFile(name string) ([]byte, error) {
	// Only individually selected files inside this instance's HOME are read.
	home := s.Config.Home
	rel, err := filepath.Rel(home, name)
	if err != nil || !filepath.IsAbs(name) || !relative(rel) {
		return nil, problem(2, "InvalidPath")
	}
	if rootRelative, err := filepath.Rel(s.Config.Root, name); err != nil || relative(rootRelative) {
		return nil, problem(2, "InvalidPath")
	}
	if credentialFile(rel) {
		return nil, problem(2, "SensitiveContent")
	}
	fs, err := openRoot(home, false)
	if err != nil {
		return nil, err
	}
	defer fs.close()
	return fs.read(rel, 32<<10)
}
func (s *Store) importPlan(r Request) (ImportPlan, error) {
	var input importInput
	if err := Decode(r.Payload, &input); err != nil {
		return ImportPlan{}, err
	}
	if len(input.Files) == 0 || len(input.Files) > 8 {
		return ImportPlan{}, problem(2, "InvalidInput")
	}
	plan := ImportPlan{Scope: r.RequestContext.Scope, Files: []ImportFile{}}
	for _, file := range input.Files {
		b, err := s.importFile(file)
		if err != nil {
			return plan, err
		}
		if !validText(string(b), 32<<10) {
			return plan, problem(2, "SensitiveContent")
		}
		plan.Files = append(plan.Files, ImportFile{file, digest(b)})
	}
	plan.Digest = plan.hash()
	return plan, nil
}
func (s *Store) importApply(v *snapshot, r Request) (writeResult, error) {
	var input importApplyInput
	if err := Decode(r.Payload, &input); err != nil {
		return writeResult{}, err
	}
	plan := input.Plan
	if plan.Digest != plan.hash() || len(plan.Files) < 1 || len(plan.Files) > 8 || scopeKey(plan.Scope) != scopeKey(r.RequestContext.Scope) {
		return writeResult{}, problem(3, "Conflict")
	}
	result := writeResult{Refs: []Ref{}}
	for _, file := range plan.Files {
		b, err := s.importFile(file.Path)
		if err != nil {
			return result, err
		}
		if digest(b) != file.Digest {
			return result, problem(3, "Conflict")
		}
		if !validText(string(b), 32<<10) {
			return result, problem(2, "SensitiveContent")
		}
		version := file.Digest
		note := Note{Kind: "fact", Title: filepath.Base(file.Path), Scope: plan.Scope, Status: "active", TemporalClass: "historical", LoadHint: "on-demand", Revalidation: Revalidation{Mode: "on-use"}, Sources: []Source{{Kind: "import", Ref: filepath.Base(file.Path), Version: &version, Origin: "external-claim", Assurance: "reported"}}, Body: string(b)}
		if err := note.validate(); err != nil {
			return result, err
		}
		note.ID = newID("note")
		note.Revision = 1
		note.UpdatedAt = timestamp(s.now())
		ref, err := v.save(note.ID, 1, note)
		if err != nil {
			return result, err
		}
		result.Refs = append(result.Refs, ref)
	}
	return result, nil
}
