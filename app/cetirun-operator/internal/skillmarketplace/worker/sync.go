// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package worker

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/skillmarketplace/gitsync"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/skillmarketplace/publish"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/skillmarketplace/skills"
	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Store holds the skills of the last synchronized commit.
type Store struct {
	mu     sync.RWMutex
	commit string
	skills map[string]skills.Skill
}

// Snapshot returns the commit and its valid skills.
func (s *Store) Snapshot() (string, map[string]skills.Skill) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.commit, s.skills
}

func (s *Store) set(commit string, valid []skills.Skill) {
	byName := make(map[string]skills.Skill, len(valid))
	for _, skill := range valid {
		byName[skill.Name] = skill
	}
	s.mu.Lock()
	s.commit, s.skills = commit, byName
	s.mu.Unlock()
}

// Syncer periodically synchronizes the repository.
type Syncer struct {
	Source   gitsync.Source
	Path     string
	Interval time.Duration
	Limits   skills.Limits
	Reserved []string
	// Fetch bounds the memory of one fetch.
	Fetch   gitsync.Limits
	Store   *Store
	Catalog *Catalog
	// Changed receives one signal after each new commit, to publish again.
	Changed chan<- struct{}

	// refused holds the hashes, as Resolve can return them, of the last
	// commit that could not be loaded because of its own content, and
	// refusal is its error. Such a commit stays refused, so later checks
	// report the error without reading the commit again.
	refused []string
	refusal error
	// loaded is the hash that the ref named when the fetch read the commit in
	// Store: the commit, or the tag object of an annotated tag.
	loaded string
}

// NeedLeaderElection reports that the synchronizer runs once per worker.
func (s *Syncer) NeedLeaderElection() bool { return true }

// Start runs the synchronization loop.
func (s *Syncer) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("sync")
	timer := time.NewTimer(0)
	defer timer.Stop()
	var failures failureLog
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		err := s.SyncOnce(ctx)
		if ctx.Err() != nil {
			// The worker stops; the check did not fail.
			return nil
		}
		failures.report(logger, err, gitsync.Redacted(s.Source.URL))
		if err != nil {
			s.Catalog.SetError(err.Error())
			timer.Reset(minDuration(s.Interval, time.Minute))
			continue
		}
		timer.Reset(s.Interval)
	}
}

// failureLog logs a failed check as an error when the failure starts or its
// error changes. While the same error repeats, it logs one short line for
// each check instead of the error and its stack trace. It logs once when the
// checks succeed again.
type failureLog struct {
	// message is the error of the last failed check.
	message string
	// failed counts the consecutive failed checks.
	failed int
}

func (f *failureLog) report(logger logr.Logger, err error, repository string) {
	if err == nil {
		if f.failed > 0 {
			logger.Info("the skills repository check succeeds again", "repository", repository, "failedChecks", f.failed)
		}
		*f = failureLog{}
		return
	}
	f.failed++
	if message := err.Error(); message != f.message {
		f.message = message
		logger.Error(err, "synchronize the skills repository", "repository", repository)
		return
	}
	logger.Info("the skills repository check failed again with the same error", "repository", repository, "failedChecks", f.failed)
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// SyncOnce checks the remote and loads a new commit. A commit that cannot be
// loaded because of its own content, such as a commit over the limits, is
// read once: while the remote names it, a check reports its error again.
//
// The remote names a commit by the hash that Resolve returns. For an annotated
// tag, a remote that does not advertise the tag's commit gives the hash of the
// tag object, which differs from the loaded commit: the hash that loaded the
// commit counts as that commit too. The ref can move between Resolve and
// Fetch, so the worker records the hashes that the fetch read, not the hash
// that Resolve returned before it.
func (s *Syncer) SyncOnce(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	ref, hash, err := s.Source.Resolve(ctx)
	if err != nil {
		return err
	}
	resolved := hash.String()
	current, _ := s.Store.Snapshot()
	if current == resolved || (current != "" && s.loaded == resolved) {
		s.Catalog.SetChecked()
		return nil
	}
	if slices.Contains(s.refused, resolved) {
		return s.refusal
	}
	snapshot, err := s.Source.Fetch(ctx, ref, s.Fetch)
	if err != nil {
		// A limit belongs to the commit. Another error, such as a failed
		// connection, can pass, so the next check reads the commit again.
		// A fetch that a limit stopped leaves no ref that names the commit
		// it read: the refusal counts for the resolved hash only while the
		// ref still names it after the fetch.
		if errors.Is(err, gitsync.ErrLimit) {
			if _, again, resolveErr := s.Source.Resolve(ctx); resolveErr == nil && again.String() == resolved {
				s.refused, s.refusal = []string{resolved}, err
			}
		}
		return err
	}
	valid, problems, err := skills.Discover(snapshot, s.Path, s.Limits, s.Reserved)
	if err != nil {
		// Discover reads only the commit, so its result does not change.
		s.refused, s.refusal = []string{snapshot.Ref, snapshot.Commit}, err
		return err
	}
	entries := make([]api.SkillMarketplaceSkill, 0, len(valid))
	for _, skill := range valid {
		entries = append(entries, api.SkillMarketplaceSkill{
			Name: skill.Name, Description: skill.Description, Version: skill.Version, Path: skill.Path,
			UpdatedAt: s.Catalog.FirstSeen(skill.Name, skill.Version), Files: len(skill.Files), Bytes: skill.Bytes,
		})
	}
	s.Store.set(snapshot.Commit, valid)
	s.loaded = snapshot.Ref
	s.Catalog.SetSync(snapshot.Commit, entries, problems)
	log.FromContext(ctx).Info("synchronized the skills repository", "commit", snapshot.Commit, "skills", len(valid), "rejected", len(problems))
	signal(s.Changed)
	return nil
}

// contentFor turns a repository skill into publishable content.
func contentFor(skill skills.Skill) publish.SkillContent {
	content := publish.SkillContent{Name: skill.Name, Version: skill.Version}
	for _, file := range skill.Files {
		content.Files = append(content.Files, publish.File{RelPath: file.RelPath, Executable: file.Executable, Open: file.Open})
	}
	return content
}

// errNotSynced is returned while no commit has been loaded yet.
var errNotSynced = fmt.Errorf("the skills repository has not been synchronized yet")
