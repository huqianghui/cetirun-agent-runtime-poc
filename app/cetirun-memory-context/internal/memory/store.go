// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"
)

type Config struct {
	Root string
	Home string // Import source boundary; managed layout uses the root's HOME.
}
type Store struct {
	Config Config
	now    func() time.Time
	fault  func(string) error
}

func New(config Config) *Store {
	if config.Home == "" {
		if filepath.Base(config.Root) == "memory" && filepath.Base(filepath.Dir(config.Root)) == ".cetirun" {
			config.Home = filepath.Dir(filepath.Dir(config.Root))
		} else {
			config.Home, _ = os.UserHomeDir()
		}
	}
	return &Store{Config: config, now: time.Now}
}

type meta struct {
	SchemaVersion string `json:"schemaVersion"`
	RootID        string `json:"rootId"`
	InitRequestID string `json:"initRequestId"`
	InitializedAt string `json:"initializedAt"`
}
type head struct {
	SchemaVersion  string `json:"schemaVersion"`
	Generation     int64  `json:"generation"`
	ManifestDigest string `json:"manifestDigest"`
}
type viewHash struct {
	Current  string `json:"current"`
	Previous string `json:"previous,omitempty"`
}
type writeResult struct {
	Saved        bool   `json:"saved"`
	Refs         []Ref  `json:"refs"`
	State        string `json:"state,omitempty"`
	PurgePending bool   `json:"purgePending,omitempty"`
}
type writeReceipt struct {
	Digest     string      `json:"digest"`
	Generation int64       `json:"generation"`
	Result     writeResult `json:"result"`
}
type slot struct {
	Active   *Ref `json:"active"`
	Trial    *Ref `json:"trial"`
	Baseline *Ref `json:"baseline"`
}
type methodState struct {
	State            string            `json:"state"`
	Reason           string            `json:"reason,omitempty"`
	Evaluation       *Ref              `json:"evaluation,omitempty"`
	TrialSeen        map[string]bool   `json:"trialSeen,omitempty"`
	TrialPassed      map[string]string `json:"trialPassed,omitempty"`
	Exhausted        bool              `json:"exhausted,omitempty"`
	ArtifactEvidence *artifactEvidence `json:"artifactEvidence,omitempty"`
}
type budgetEntry struct {
	At      string `json:"at"`
	Slot    string `json:"slot"`
	Lineage string `json:"lineage"`
}
type manifest struct {
	SchemaVersion    string                  `json:"schemaVersion"`
	Generation       int64                   `json:"generation"`
	Objects          map[string]Ref          `json:"objects"`
	Dependencies     map[string][]Ref        `json:"dependencies"`
	ProcedureStates  map[string]methodState  `json:"procedureStates"`
	Slots            map[string]slot         `json:"activeSlots"`
	SourceChecks     map[string][]Ref        `json:"sourceChecks"`
	Pending          []string                `json:"pendingObservationIds"`
	Processed        map[string]bool         `json:"processedSources"`
	Requests         map[string]writeReceipt `json:"requests"`
	Views            map[string]viewHash     `json:"views"`
	Tombstones       map[string]bool         `json:"tombstones"`
	Purges           map[string]ForgetPlan   `json:"purges"`
	Budgets          []budgetEntry           `json:"budgets"`
	ExhaustedTrials  map[string]bool         `json:"exhaustedTrials"`
	LastWriteAt      string                  `json:"lastWriteAt"`
	ExtraCalls       map[string]ModelCall    `json:"extraCalls,omitempty"`
	ArtifactExposure *artifactExposure       `json:"artifactExposure,omitempty"`
	HistoryCompacted bool                    `json:"historyCompacted,omitempty"`
}

func emptyManifest() manifest {
	return manifest{
		SchemaVersion: SchemaVersion,
		Objects:       map[string]Ref{}, Dependencies: map[string][]Ref{},
		ProcedureStates: map[string]methodState{}, Slots: map[string]slot{},
		SourceChecks: map[string][]Ref{}, Processed: map[string]bool{},
		Requests: map[string]writeReceipt{}, Views: map[string]viewHash{},
		Tombstones: map[string]bool{}, Purges: map[string]ForgetPlan{},
		ExhaustedTrials: map[string]bool{},
		ExtraCalls:      map[string]ModelCall{},
	}
}

type pendingFile struct {
	Name string `json:"name"`
	Data []byte `json:"data"`
}
type transaction struct {
	RequestDigest      string        `json:"requestDigest"`
	ExpectedGeneration int64         `json:"expectedGeneration"`
	Manifest           manifest      `json:"manifest"`
	Files              []pendingFile `json:"files"`
	Result             writeResult   `json:"result"`
}
type snapshot struct {
	fs           *safeFS
	m            manifest
	added        map[string][]byte
	files        []pendingFile
	notesChanged bool
	damaged      []Ref
}

func (s *Store) point(name string) error {
	if s.fault != nil {
		return s.fault(name)
	}
	return nil
}
func (s *Store) openRead(ctx context.Context) (*snapshot, error) {
	return s.openSnapshot(ctx, syscall.LOCK_SH)
}
func (s *Store) openSnapshot(ctx context.Context, mode int) (*snapshot, error) {
	f, err := openRoot(s.Config.Root, false)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*snapshot, error) { f.close(); return nil, err }
	b, err := f.read("meta.json", 4096)
	if err != nil {
		return fail(err)
	}
	var metadata meta
	if err := Decode(b, &metadata); err != nil {
		return fail(problem(6, "RootInvalid"))
	}
	if metadata.SchemaVersion != SchemaVersion {
		return fail(problem(6, "SchemaUnsupported"))
	}
	if mode != 0 {
		f.unlock, err = f.lockMode(ctx, mode)
		if err != nil {
			return fail(err)
		}
	}
	m, err := readManifest(f)
	if err != nil {
		return fail(err)
	}
	return &snapshot{fs: f, m: m, added: map[string][]byte{}}, nil
}
func readManifest(f *safeFS) (manifest, error) {
	var h head
	b, err := f.read("state/HEAD.json", 4096)
	if err != nil {
		return manifest{}, err
	}
	if err := Decode(b, &h); err != nil || h.SchemaVersion != SchemaVersion || h.Generation < 1 || !digestPattern.MatchString(h.ManifestDigest) {
		return manifest{}, problem(6, "IntegrityMismatch")
	}
	b, err = f.read(fmt.Sprintf("state/manifests/%d.json", h.Generation), MaxManifest)
	if err != nil {
		return manifest{}, problem(6, "IntegrityMismatch")
	}
	if digest(b) != h.ManifestDigest {
		return manifest{}, problem(6, "IntegrityMismatch")
	}
	var m manifest
	if err := Decode(b, &m); err != nil || m.Generation != h.Generation || m.SchemaVersion != SchemaVersion {
		return m, problem(6, "IntegrityMismatch")
	}
	if m.Objects == nil || m.Requests == nil || m.Tombstones == nil || m.Views == nil || m.Slots == nil || m.ProcedureStates == nil || m.SourceChecks == nil || m.Purges == nil || m.Processed == nil || m.ExhaustedTrials == nil {
		return m, problem(6, "IntegrityMismatch")
	}
	if m.Dependencies == nil {
		return m, problem(6, "IntegrityMismatch")
	}
	if m.ExtraCalls == nil {
		m.ExtraCalls = map[string]ModelCall{}
	}
	if exposure := m.ArtifactExposure; exposure != nil {
		if !digestPattern.MatchString(exposure.CaseSetDigest) || len(exposure.Owners) != 66 {
			return m, problem(6, "IntegrityMismatch")
		}
		for _, owner := range exposure.Owners {
			if owner != "" && owner != "*" && !digestPattern.MatchString(owner) {
				return m, problem(6, "IntegrityMismatch")
			}
		}
	}
	for id, ref := range m.Objects {
		if id != ref.ID || !ref.Valid() || m.Tombstones[id] {
			return m, problem(6, "IntegrityMismatch")
		}
	}
	return m, nil
}
func objectPath(ref Ref) (string, error) {
	if !ref.Valid() {
		return "", problem(2, "InvalidInput")
	}
	kind, _, _ := strings.Cut(ref.ID, "-")
	ext := "json"
	if kind == "note" {
		ext = "md"
	}
	return fmt.Sprintf("objects/%s/%s/r%d.%s", kind, ref.ID, ref.Revision, ext), nil
}
func (v *snapshot) read(ref Ref, target any) error {
	name, err := objectPath(ref)
	if err != nil {
		return err
	}
	if v.m.Tombstones[ref.ID] {
		return problem(6, "SourceForgotten")
	}
	b, ok := v.added[ref.Key()]
	if !ok {
		b, err = v.fs.read(name, 32<<10)
		if err != nil {
			return err
		}
		v.added[ref.Key()] = b
	}
	if digest(b) != ref.Digest {
		return problem(6, "IntegrityMismatch")
	}
	if strings.HasPrefix(ref.ID, "note-") {
		note, ok := target.(*Note)
		if !ok {
			return problem(2, "InvalidInput")
		}
		parts := bytes.SplitN(b, []byte("\n---\n"), 2)
		if len(parts) != 2 || !bytes.HasPrefix(parts[0], []byte("---\n")) {
			return problem(6, "IntegrityMismatch")
		}
		if err := Decode(parts[0][4:], note); err != nil {
			return problem(6, "IntegrityMismatch")
		}
		note.Body = string(parts[1])
		return nil
	}
	if err := Decode(b, target); err != nil {
		return problem(6, "IntegrityMismatch")
	}
	return nil
}
func (v *snapshot) current(ref Ref) error {
	if !ref.Valid() || v.m.Objects[ref.ID] != ref {
		return problem(3, "Conflict")
	}
	if v.m.Tombstones[ref.ID] {
		return problem(6, "SourceForgotten")
	}
	return nil
}

func (v *snapshot) damagedObject(ref Ref, err error) bool {
	var typed *Error
	if !errors.Is(err, syscall.ENOENT) && !(errors.As(err, &typed) && (typed.Reason == "IntegrityMismatch" || typed.Reason == "LimitReached")) {
		return false
	}
	if !slices.Contains(v.damaged, ref) {
		v.damaged = append(v.damaged, ref)
	}
	return true
}

func (v *snapshot) damageReport() *Partial {
	if len(v.damaged) == 0 {
		return nil
	}
	reasons := []string{}
	for _, ref := range v.damaged {
		reasons = append(reasons, "DamagedObject:"+ref.ID)
	}
	sort.Strings(reasons)
	return &Partial{Omitted: len(reasons), Reasons: reasons}
}

// Unpublished files can survive a cancelled commit. Never overwrite them or
// a published immutable revision when a different request revises this object.
func (v *snapshot) nextRevision(ref Ref) (int64, error) {
	for revision := ref.Revision + 1; revision > ref.Revision && revision-ref.Revision <= MaxScan; revision++ {
		next := Ref{ID: ref.ID, Revision: revision, Digest: ref.Digest}
		if _, staged := v.added[next.Key()]; staged {
			continue
		}
		name, err := objectPath(next)
		if err != nil {
			return 0, err
		}
		file, err := v.fs.open(name, syscall.O_RDONLY, false)
		if errors.Is(err, syscall.ENOENT) {
			return revision, nil
		}
		if err != nil {
			return 0, err
		}
		file.Close()
	}
	return 0, problem(5, "LimitReached")
}

func (v *snapshot) save(id string, revision int64, value any) (Ref, error) {
	var b []byte
	var err error
	if note, ok := value.(Note); ok {
		body := strings.ReplaceAll(note.Body, "\r\n", "\n")
		note.Body = ""
		b, err = encode(note)
		b = append(append(append([]byte("---\n"), b...), []byte("---\n")...), []byte(body)...)
	} else {
		b, err = encode(value)
	}
	if err != nil {
		return Ref{}, err
	}
	limit := 32 << 10
	if strings.HasPrefix(id, "obs-") {
		limit = 2 << 10
	}
	if strings.HasPrefix(id, "proc-") || strings.HasPrefix(id, "use-") {
		limit = 16 << 10
	}
	if len(b) > limit {
		return Ref{}, problem(5, "LimitReached")
	}
	ref := Ref{id, revision, digest(b)}
	name, err := objectPath(ref)
	if err != nil {
		return Ref{}, err
	}
	if v.m.Tombstones[id] {
		return Ref{}, problem(6, "SourceForgotten")
	}
	v.added[ref.Key()] = b
	v.files = append(v.files, pendingFile{name, b})
	v.m.Objects[id] = ref
	if strings.HasPrefix(id, "note-") {
		v.notesChanged = true
	}
	dependencies, err := dependencies(v, ref)
	if err != nil {
		return Ref{}, err
	}
	for _, dependency := range dependencies {
		if !slices.Contains(v.m.Dependencies[id], dependency) {
			v.m.Dependencies[id] = append(v.m.Dependencies[id], dependency)
		}
	}
	return ref, nil
}

func (s *Store) init(ctx context.Context, r Request) (writeResult, int64, error) {
	if err := ctx.Err(); err != nil {
		return writeResult{}, 0, err
	}
	if err := validateContext(r, true, false); err != nil {
		return writeResult{}, 0, err
	}
	if *r.ExpectedGeneration != 0 {
		return writeResult{}, 0, problem(3, "Conflict")
	}
	f, err := openRoot(s.Config.Root, true)
	if err != nil {
		return writeResult{}, 0, err
	}
	defer f.close()
	unlockRoot, err := f.lockRoot(ctx)
	if err != nil {
		return writeResult{}, 0, err
	}
	defer unlockRoot()
	b, err := f.read("meta.json", 4096)
	if errors.Is(err, syscall.ENOENT) {
		dirFD, openErr := syscall.Openat(int(f.root.Fd()), ".", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
		if openErr != nil {
			return writeResult{}, 0, openErr
		}
		dir := os.NewFile(uintptr(dirFD), ".")
		entries, err := dir.ReadDir(1)
		dir.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			return writeResult{}, 0, err
		}
		if len(entries) > 0 {
			return writeResult{}, 0, problem(6, "RootInvalid")
		}
		b, _ = encode(meta{SchemaVersion: SchemaVersion, RootID: newID("root"), InitRequestID: r.RequestID, InitializedAt: timestamp(s.now())})
		if err = f.create("meta.json", b); err != nil {
			return writeResult{}, 0, err
		}
	} else if err != nil {
		return writeResult{}, 0, err
	}
	var metadata meta
	if Decode(b, &metadata) != nil || metadata.SchemaVersion != SchemaVersion {
		return writeResult{}, 0, problem(6, "RootInvalid")
	}
	unlock, err := f.lock(ctx)
	if err != nil {
		return writeResult{}, 0, err
	}
	defer unlock()
	if _, headErr := f.read("state/HEAD.json", 4096); headErr == nil {
		m, err := readManifest(f)
		if err != nil {
			return writeResult{}, 0, err
		}
		return writeResult{Saved: true, Refs: []Ref{}}, m.Generation, nil
	} else if !errors.Is(headErr, syscall.ENOENT) {
		return writeResult{}, 0, headErr
	}
	if metadata.InitRequestID != r.RequestID {
		return writeResult{}, 0, problem(3, "Conflict")
	}
	m := emptyManifest()
	m.Generation = 1
	m.LastWriteAt = metadata.InitializedAt
	data, _ := encode(m)
	if err := f.create("state/manifests/1.json", data); err != nil && !errors.Is(err, syscall.EEXIST) {
		return writeResult{}, 0, err
	}
	if prior, err := f.read("state/manifests/1.json", MaxManifest); err != nil || !bytes.Equal(prior, data) {
		return writeResult{}, 0, problem(6, "IntegrityMismatch")
	}
	h, _ := encode(head{SchemaVersion, 1, digest(data)})
	if err := ctx.Err(); err != nil {
		return writeResult{}, 0, err
	}
	if err := f.atomic("state/HEAD.json", h); err != nil {
		return writeResult{}, 0, problem(8, "CommitOutcomeUnknown")
	}
	return writeResult{Saved: true, Refs: []Ref{}}, 1, nil
}

func requestHash(command string, r Request) string {
	if r.native {
		// The adapter binds generation at dispatch time, not as part of the
		// native event's identity. Explicit CLI callers retain generation CAS.
		r.ExpectedGeneration = nil
	}
	b, _ := encode(struct {
		Command string  `json:"command"`
		Request Request `json:"request"`
	}{command, r})
	return digest(b)
}
func requestKey(id string) string { return strings.TrimPrefix(digest([]byte(id)), "sha256:") }
func (s *Store) write(ctx context.Context, r Request, command string, maintenance bool, fn func(*snapshot) (writeResult, error)) (writeResult, int64, error) {
	if err := validateContext(r, true, maintenance); err != nil {
		return writeResult{}, 0, err
	}
	v, err := s.openSnapshot(ctx, syscall.LOCK_EX)
	if err != nil {
		return writeResult{}, 0, err
	}
	defer v.fs.close()
	if err := v.resumePurges(ctx); err != nil {
		return writeResult{}, v.m.Generation, err
	}
	key, hash := requestKey(r.RequestID), requestHash(command, r)
	if receipt, ok := v.m.Requests[key]; ok {
		if !requestDigestMatches(receipt.Digest, command, r, receipt.Generation-1) {
			return writeResult{}, v.m.Generation, problem(3, "Conflict")
		}
		for _, ref := range receipt.Result.Refs {
			if v.m.Tombstones[ref.ID] {
				return writeResult{}, v.m.Generation, problem(6, "SourceForgotten")
			}
		}
		return receipt.Result, receipt.Generation, nil
	}
	if *r.ExpectedGeneration != v.m.Generation {
		return writeResult{}, v.m.Generation, problem(3, "Conflict")
	}
	if err := ctx.Err(); err != nil {
		return writeResult{}, v.m.Generation, err
	}
	if command == "repair-index" {
		if err := v.preserveEditedViews(); err != nil {
			return writeResult{}, v.m.Generation, err
		}
	}
	// Finish the previous HEAD's derived files before another generation can
	// replace their recovery hashes. An interrupted commit may leave old views.
	if len(v.m.Views) > 0 {
		if err := v.installViews(); err != nil {
			return writeResult{}, v.m.Generation, err
		}
	}
	if err := v.removeAbandonedTransactions(ctx); err != nil {
		return writeResult{}, v.m.Generation, err
	}
	if err := v.collectHistory(ctx); err != nil {
		return writeResult{}, v.m.Generation, err
	}
	txPath := "transactions/" + key + ".json"
	var tx transaction
	if data, readErr := v.fs.read(txPath, MaxInput*4); readErr == nil {
		if Decode(data, &tx) != nil || !requestDigestMatches(tx.RequestDigest, command, r, tx.ExpectedGeneration) || tx.ExpectedGeneration != v.m.Generation {
			return writeResult{}, v.m.Generation, problem(3, "Conflict")
		}
	} else {
		if !errors.Is(readErr, syscall.ENOENT) {
			return writeResult{}, v.m.Generation, readErr
		}
		result, err := fn(v)
		if err != nil {
			return writeResult{}, v.m.Generation, err
		}
		if v.notesChanged {
			for key, current := range v.m.Slots {
				if current.Trial != nil && !strings.HasPrefix(key, ArtifactProfile+"/") {
					v.resetRetrievalTrial(key)
				}
			}
		}
		before := v.m.Generation
		v.m.Generation++
		v.m.LastWriteAt = timestamp(s.now())
		change := Change{newID("change"), 1, before, v.m.Generation, command, result.Refs}
		if _, err := v.save(change.ID, 1, change); err != nil {
			return writeResult{}, before, err
		}
		result.Saved = true
		v.m.Requests[key] = writeReceipt{hash, v.m.Generation, result}
		for len(v.m.Requests) > retainedHistory {
			oldest := ""
			var age int64
			for k, x := range v.m.Requests {
				if oldest == "" || x.Generation < age || x.Generation == age && k < oldest {
					age = x.Generation
					oldest = k
				}
			}
			delete(v.m.Requests, oldest)
		}
		views, err := v.renderViews()
		if err != nil {
			return writeResult{}, before, err
		}
		for name, data := range views {
			v.m.Views[name] = viewHash{Current: digest(data), Previous: v.m.Views[name].Current}
		}
		for name, hash := range v.m.Views {
			if _, exists := views[name]; !exists {
				v.m.Views[name] = viewHash{Previous: hash.Current}
			}
		}
		// Views first, so the size checked here is the manifest that commits.
		// An oversized write fails before anything durable is written.
		manifestSize, err := v.compactHistory()
		if err != nil {
			return writeResult{}, before, err
		}
		if manifestSize > MaxManifest {
			return writeResult{}, before, problem(5, "LimitReached")
		}
		tx = transaction{hash, before, v.m, v.files, result}
		data, err := encode(tx)
		if err != nil || len(data) > MaxInput*4 {
			return writeResult{}, before, problem(5, "LimitReached")
		}
		_, size, err := v.fs.inventory()
		if err != nil {
			return writeResult{}, before, err
		}
		limit := int64(HighWaterBytes)
		if maintenance {
			limit = MaxRootBytes
		}
		if size+int64(len(data)*2)+MaxManifest > limit {
			return writeResult{}, before, problem(5, "LimitReached")
		}
		if err := v.fs.create(txPath, data); err != nil {
			return writeResult{}, before, err
		}
	}
	if err := s.point("transaction"); err != nil {
		return writeResult{}, tx.ExpectedGeneration, err
	}
	for _, file := range tx.Files {
		if err := v.fs.create(file.Name, file.Data); err != nil {
			if !errors.Is(err, syscall.EEXIST) {
				return writeResult{}, tx.ExpectedGeneration, err
			}
			b, err := v.fs.read(file.Name, MaxInput)
			if err != nil || !bytes.Equal(b, file.Data) {
				return writeResult{}, tx.ExpectedGeneration, problem(6, "IntegrityMismatch")
			}
		}
	}
	if err := s.point("objects"); err != nil {
		return writeResult{}, tx.ExpectedGeneration, err
	}
	data, err := encode(tx.Manifest)
	if err != nil || len(data) > MaxManifest {
		return writeResult{}, tx.ExpectedGeneration, problem(5, "LimitReached")
	}
	name := fmt.Sprintf("state/manifests/%d.json", tx.Manifest.Generation)
	// An abandoned, uncommitted generation may be replaced only while HEAD
	// still matches the expected generation and this request owns the lock.
	if err := v.fs.atomic(name, data); err != nil {
		return writeResult{}, tx.ExpectedGeneration, err
	}
	if err := s.point("manifest"); err != nil {
		return writeResult{}, tx.ExpectedGeneration, err
	}
	if err := ctx.Err(); err != nil {
		return writeResult{}, tx.ExpectedGeneration, err
	}
	h, _ := encode(head{SchemaVersion, tx.Manifest.Generation, digest(data)})
	if err := v.fs.atomic("state/HEAD.json", h); err != nil {
		return writeResult{}, tx.ExpectedGeneration, problem(8, "CommitOutcomeUnknown")
	}
	if err := s.point("head"); err != nil {
		return writeResult{}, tx.Manifest.Generation, problem(8, "CommitOutcomeUnknown")
	}
	v.m = tx.Manifest
	v.added = map[string][]byte{}
	if err := v.installViews(); err != nil {
		tx.Result.State = "IndexStale"
	}
	_ = v.fs.remove(txPath)
	return tx.Result, tx.Manifest.Generation, nil
}

func requestDigestMatches(saved, command string, r Request, generation int64) bool {
	if saved == requestHash(command, r) {
		return true
	}
	if !r.native {
		return false
	}
	// Read receipts written before native event identity excluded the
	// adapter-generated generation, without changing the persisted schema.
	r.native = false
	r.ExpectedGeneration = &generation
	return saved == requestHash(command, r)
}

func (v *snapshot) checkViews() error {
	for name, expected := range v.m.Views {
		if !validViewPath(name) {
			return problem(6, "IntegrityMismatch")
		}
		b, err := v.fs.read(name, MaxInput)
		if errors.Is(err, syscall.ENOENT) {
			continue
		}
		if err != nil {
			return err
		}
		if h := digest(b); h != expected.Current && h != expected.Previous {
			return problem(3, "DataModifiedOutsideHelper")
		}
	}
	return nil
}
func (v *snapshot) renderViews() (map[string][]byte, error) {
	views := map[string][]byte{}
	var index strings.Builder
	fmt.Fprintf(&index, "# Private memory\n\nGeneration: %d\n\n", v.m.Generation)
	ids := make([]string, 0, len(v.m.Objects))
	for id := range v.m.Objects {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	count := 0
	for _, id := range ids {
		ref := v.m.Objects[id]
		if strings.HasPrefix(id, "note-") {
			var note Note
			if err := v.read(ref, &note); err != nil {
				if v.damagedObject(ref, err) {
					continue
				}
				return nil, err
			}
			name := "knowledge/" + id + ".md"
			if note.Kind == "lesson" {
				name = "lessons/" + id + ".md"
			}
			file, _ := objectPath(ref)
			b, ok := v.added[ref.Key()]
			if !ok {
				var err error
				b, err = v.fs.read(file, 32<<10)
				if err != nil {
					return nil, err
				}
			}
			views[name] = b
			if index.Len() < 15000 && count < MaxScan {
				fmt.Fprintf(&index, "- [%s](%s) (%s)\n", id, name, note.Status)
				count++
			}
		}
		if strings.HasPrefix(id, "proc-") {
			var proc Procedure
			if err := v.read(ref, &proc); err != nil {
				if v.damagedObject(ref, err) {
					continue
				}
				return nil, err
			}
			b, _ := encode(proc)
			views["procedures/"+id+".json"] = b
		}
	}
	views["index.md"] = []byte(index.String())
	return views, nil
}
func (v *snapshot) installViews() error {
	if err := v.checkViews(); err != nil {
		return err
	}
	views, err := v.renderViews()
	if err != nil {
		return err
	}
	for name, b := range views {
		// Damage can change a derived index without changing HEAD. Publish
		// that new view only once a write has committed its expected hash.
		if v.m.Views[name].Current != digest(b) {
			continue
		}
		current, err := v.fs.read(name, MaxInput)
		if err != nil && !errors.Is(err, syscall.ENOENT) {
			return err
		}
		if bytes.Equal(current, b) {
			continue
		}
		if err := v.fs.atomic(name, b); err != nil {
			return err
		}
	}
	for name, hash := range v.m.Views {
		if hash.Current == "" {
			if err := v.fs.remove(name); err != nil {
				return err
			}
		}
	}
	return nil
}

func validViewPath(name string) bool {
	if name == "index.md" {
		return true
	}
	parts := strings.Split(name, "/")
	if len(parts) != 2 {
		return false
	}
	if parts[0] == "procedures" {
		return objectID.MatchString(strings.TrimSuffix(parts[1], ".json")) && strings.HasPrefix(parts[1], "proc-") && strings.HasSuffix(parts[1], ".json")
	}
	return (parts[0] == "knowledge" || parts[0] == "lessons") && objectID.MatchString(strings.TrimSuffix(parts[1], ".md")) && strings.HasPrefix(parts[1], "note-") && strings.HasSuffix(parts[1], ".md")
}

func (v *snapshot) refs(kind string) []Ref {
	refs := []Ref{}
	for id, r := range v.m.Objects {
		if strings.HasPrefix(id, kind+"-") {
			refs = append(refs, r)
		}
	}
	slices.SortFunc(refs, func(a, b Ref) int { return strings.Compare(a.ID, b.ID) })
	return refs
}
func (v *snapshot) allowed(ref Ref, r Request) bool {
	if d := r.RequestContext.Delegation; d != nil {
		return slices.Contains(d.AllowedRefs, ref)
	}
	return true
}

func (v *snapshot) preserveEditedViews() error {
	for name, expected := range v.m.Views {
		if !validViewPath(name) {
			return problem(6, "IntegrityMismatch")
		}
		data, err := v.fs.read(name, MaxInput)
		if errors.Is(err, syscall.ENOENT) {
			continue
		}
		if err != nil {
			return err
		}
		hash := digest(data)
		if hash == expected.Current || hash == expected.Previous {
			continue
		}
		backup := name + ".conflict-" + strings.TrimPrefix(hash, "sha256:")
		if err := v.fs.create(backup, data); err != nil {
			if !errors.Is(err, syscall.EEXIST) {
				return err
			}
			prior, err := v.fs.read(backup, MaxInput)
			if err != nil || !bytes.Equal(prior, data) {
				return problem(6, "IntegrityMismatch")
			}
		}
		if err := v.fs.remove(name); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) repair(ctx context.Context, r Request) (int64, *Partial, error) {
	_, generation, err := s.write(ctx, r, "repair-index", false, func(v *snapshot) (writeResult, error) {
		return writeResult{Refs: []Ref{}}, nil
	})
	if err != nil {
		return generation, nil, err
	}
	// A committed receipt can precede view installation. Finish that work on
	// replay too, and report I/O failures instead of claiming the repair worked.
	v, err := s.openSnapshot(ctx, syscall.LOCK_EX)
	if err != nil {
		return generation, nil, err
	}
	defer v.fs.close()
	if err := v.preserveEditedViews(); err != nil {
		return generation, nil, err
	}
	err = v.installViews()
	return generation, v.damageReport(), err
}
