// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package worker

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sync"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/skillmarketplace"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/skillmarketplace/publish"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// teamsPollInterval is how often the worker reads the Teams document.
	teamsPollInterval = 5 * time.Second
	// fullPassInterval is how often the worker publishes every Team, and
	// removes the releases of the Teams that left the document, without a
	// change.
	fullPassInterval = 10 * time.Minute
	// retryInterval is how soon the worker tries a Team again that waits for
	// the first synchronization or whose publication failed the first time.
	retryInterval = 10 * time.Second
	// maxRetryInterval bounds the growing wait before the worker tries again
	// a Team whose publication keeps failing with the same input.
	maxRetryInterval = 5 * time.Minute
)

// TeamsSource reads the Teams document that the operator writes.
type TeamsSource struct {
	Reader    client.Reader
	Namespace string
	// Changed receives one signal after the document changed.
	Changed chan<- struct{}

	mu       sync.Mutex
	known    bool
	version  string
	document skillmarketplace.TeamsDocument
}

// Document returns the last document, and whether one was read.
func (s *TeamsSource) Document() (skillmarketplace.TeamsDocument, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.document, s.known
}

// Set replaces the document (tests, and Poll).
func (s *TeamsSource) Set(document skillmarketplace.TeamsDocument) {
	s.mu.Lock()
	s.document, s.known = document, true
	s.mu.Unlock()
	signal(s.Changed)
}

// Poll reads the document when the ConfigMap changed.
func (s *TeamsSource) Poll(ctx context.Context) error {
	configMap := &corev1.ConfigMap{}
	err := s.Reader.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: skillmarketplace.TeamsConfigMap}, configMap)
	if apierrors.IsNotFound(err) {
		return errors.New("the operator has not written the Skill Marketplace Teams document yet")
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	unchanged := s.known && configMap.ResourceVersion == s.version
	s.mu.Unlock()
	if unchanged {
		return nil
	}
	document, err := skillmarketplace.DecodeTeams(configMap.Data[skillmarketplace.TeamsKey])
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.version = configMap.ResourceVersion
	s.mu.Unlock()
	s.Set(document)
	return nil
}

// Start polls the document. A read error keeps the last document.
func (s *TeamsSource) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("teams")
	ticker := time.NewTicker(teamsPollInterval)
	defer ticker.Stop()
	failing := false
	for {
		if err := s.Poll(ctx); err != nil {
			if !failing {
				logger.Error(err, "read the Skill Marketplace Teams document")
			}
			failing = true
		} else if failing {
			logger.Info("read the Skill Marketplace Teams document again")
			failing = false
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// signal sends one signal without blocking.
func signal(changed chan<- struct{}) {
	if changed == nil {
		return
	}
	select {
	case changed <- struct{}{}:
	default:
	}
}

// TeamPublisher publishes the selection of each Team of the Teams document.
type TeamPublisher struct {
	Publisher *publish.Publisher
	Store     *Store
	Catalog   *Catalog
	Teams     *TeamsSource
	Now       func() time.Time
	// GCGrace is how long the releases of a Team that left the document stay
	// after the worker found it gone.
	GCGrace time.Duration
	// Changed signals a new commit or a new Teams document.
	Changed <-chan struct{}

	// failures holds each Team whose last publication failed, by UID, and
	// waiting each Team that waits for the first synchronization.
	failures map[string]*teamFailure
	waiting  map[string]bool
	// gone holds when the worker first found each Team with releases
	// missing from the document, by UID.
	gone map[string]time.Time
}

// teamFailure is the failed publication of one Team.
type teamFailure struct {
	// input identifies what the worker tried to publish: the selection and
	// the synchronized commit.
	input string
	// message is the error of the last attempt.
	message string
	// failed counts the consecutive failed attempts, and attempts the ones
	// of the current input, which set the wait before the next try.
	failed, attempts int
	// next is when the worker tries the same input again.
	next time.Time
}

func (p *TeamPublisher) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Start publishes every Team after each change, and every ten minutes. In
// between, it tries again only the Teams that wait for the first
// synchronization, and the Teams whose publication failed, with a growing
// wait while the same input keeps failing.
func (p *TeamPublisher) Start(ctx context.Context) error {
	timer := time.NewTimer(0)
	defer timer.Stop()
	var nextFull time.Time
	for {
		full := false
		select {
		case <-ctx.Done():
			return nil
		case <-p.Changed:
			full = true
		case <-timer.C:
			full = !p.now().Before(nextFull)
		}
		var retry bool
		if full {
			retry = p.PublishAll(ctx)
			nextFull = p.now().Add(fullPassInterval)
		} else {
			retry = p.publishPending(ctx)
		}
		next := nextFull.Sub(p.now())
		if wait := p.nextRetry(); retry && wait < next {
			next = wait
		}
		timer.Reset(max(next, time.Second))
	}
}

// PublishAll publishes every Team of the document, and removes the releases
// of the Teams that left it. It reports whether a Team needs another try
// soon. Before the first document, it changes nothing.
func (p *TeamPublisher) PublishAll(ctx context.Context) (retry bool) {
	logger := log.FromContext(ctx).WithName("publish")
	document, known := p.Teams.Document()
	if !known {
		return true
	}
	live := map[string]bool{}
	for _, team := range document.Teams {
		live[team.UID] = true
		p.publishOne(ctx, logger, team)
	}
	p.forget(live)
	p.Catalog.RetainTeams(live)
	uids, err := p.Publisher.ListTeams()
	if err != nil {
		logger.Error(err, "list the Team releases")
		return true
	}
	now := p.now()
	if p.gone == nil {
		p.gone = map[string]time.Time{}
	}
	for uid := range p.gone {
		if live[uid] || !slices.Contains(uids, uid) {
			delete(p.gone, uid)
		}
	}
	for _, uid := range uids {
		if live[uid] {
			continue
		}
		// The grace period starts when this worker finds the Team gone, so
		// a Team that comes back within it keeps its releases, retained
		// skills included.
		since, ok := p.gone[uid]
		if !ok {
			p.gone[uid] = now
			continue
		}
		if now.Sub(since) <= p.GCGrace {
			continue
		}
		if err := p.Publisher.RemoveTeam(uid); err != nil {
			logger.Error(err, "remove the releases of a Team that no longer selects skills", "teamUID", uid)
		} else {
			logger.Info("removed the releases of a Team that no longer selects skills", "teamUID", uid)
			delete(p.gone, uid)
		}
	}
	return p.pending()
}

// publishPending tries again the Teams that wait for the first
// synchronization, and the failed Teams whose next try has come.
func (p *TeamPublisher) publishPending(ctx context.Context) (retry bool) {
	logger := log.FromContext(ctx).WithName("publish")
	document, known := p.Teams.Document()
	if !known {
		return true
	}
	live := map[string]bool{}
	for _, team := range document.Teams {
		live[team.UID] = true
		if failure := p.failures[team.UID]; p.waiting[team.UID] || (failure != nil && !p.now().Before(failure.next)) {
			p.publishOne(ctx, logger, team)
		}
	}
	p.forget(live)
	return p.pending()
}

// publishOne publishes one Team and logs the outcome. A failure is logged as
// an error when it starts or its error changes; a repeated failure logs one
// short line, and the end of the failures one line. While the same input
// keeps failing, the wait before the next try grows.
func (p *TeamPublisher) publishOne(ctx context.Context, logger logr.Logger, team skillmarketplace.TeamSelection) {
	commit, _ := p.Store.Snapshot()
	input := api.SkillMarketplaceSelectionDigest(team.Skills) + "@" + commit
	previous := p.failures[team.UID]
	again, err := p.publishTeam(ctx, team)
	if p.waiting == nil {
		p.waiting = map[string]bool{}
	}
	if again {
		p.waiting[team.UID] = true
	} else {
		delete(p.waiting, team.UID)
	}
	if p.failures == nil {
		p.failures = map[string]*teamFailure{}
	}
	if err == nil {
		if previous != nil {
			logger.Info("publishing the Team skills succeeds again", "team", team.Name, "failedAttempts", previous.failed)
			delete(p.failures, team.UID)
		}
		return
	}
	failure := &teamFailure{input: input, message: sameFailure(err), failed: 1}
	attempts := 1
	if previous != nil {
		failure.failed = previous.failed + 1
		if previous.input == input {
			attempts = previous.attempts + 1
		}
	}
	failure.attempts = attempts
	if previous == nil || previous.message != failure.message {
		logger.Error(err, "publish Team skills", "team", team.Name)
	} else {
		logger.Info("publishing the Team skills failed again with the same error", "team", team.Name, "failedAttempts", failure.failed)
	}
	wait := retryInterval
	for i := 1; i < attempts && wait < maxRetryInterval; i++ {
		wait *= 2
	}
	failure.next = p.now().Add(min(wait, maxRetryInterval))
	p.failures[team.UID] = failure
}

// temporaryName matches the temporary file or directory names that the
// publisher writes next to a file or release (".tmp-" and a random suffix).
var temporaryName = regexp.MustCompile(`\.tmp-[^/\s:]+`)

// sameFailure is the message that tells whether two failed publications
// failed the same way: the error without the random temporary names that
// each attempt uses.
func sameFailure(err error) string {
	return temporaryName.ReplaceAllString(err.Error(), ".tmp-*")
}

// forget drops the retry state of the Teams that left the document.
func (p *TeamPublisher) forget(live map[string]bool) {
	for uid := range p.failures {
		if !live[uid] {
			delete(p.failures, uid)
		}
	}
	for uid := range p.waiting {
		if !live[uid] {
			delete(p.waiting, uid)
		}
	}
}

// pending reports whether a Team waits for another try.
func (p *TeamPublisher) pending() bool {
	return len(p.waiting) > 0 || len(p.failures) > 0
}

// nextRetry returns how long until the next try of a waiting or failed Team.
func (p *TeamPublisher) nextRetry() time.Duration {
	next := maxRetryInterval
	if len(p.waiting) > 0 {
		next = retryInterval
	}
	for _, failure := range p.failures {
		next = min(next, failure.next.Sub(p.now()))
	}
	return next
}

// publishTeam publishes one Team. It reports whether the Team waits for the
// first synchronization.
func (p *TeamPublisher) publishTeam(ctx context.Context, team skillmarketplace.TeamSelection) (bool, error) {
	if err := p.Publisher.EnsureTeam(team.UID); err != nil {
		return false, err
	}
	selection := api.SkillMarketplaceSelection(team.Skills)
	digest := api.SkillMarketplaceSelectionDigest(team.Skills)
	state := api.SkillMarketplaceTeam{Name: team.Name, UID: team.UID, SelectionDigest: digest}
	commit, available := p.Store.Snapshot()
	if available == nil && len(selection) > 0 {
		state.Error = errNotSynced.Error()
		state = p.withCurrent(state)
		// A restarted worker keeps the entry of the earlier one while it
		// is for the Team's current selection and current release: it
		// still says what the Team's Agents copy.
		if previous, ok := p.Catalog.Team(team.UID); !ok || previous.SelectionDigest != digest || previous.Release != state.Release {
			p.Catalog.SetTeam(state)
		}
		return true, nil
	}
	// A selected skill that the repository no longer offers keeps the version
	// of the current release. The next release must not leave it out because
	// that release could not be read: retry instead, and keep CURRENT.
	var currentID string
	var current *api.SkillMarketplaceRelease
	if slices.ContainsFunc(selection, func(name string) bool { _, ok := available[name]; return !ok }) {
		var err error
		if currentID, current, err = p.Publisher.ReadCurrent(team.UID); err != nil {
			return false, p.publicationBlocked(state, err)
		}
	}
	var content []publish.SkillContent
	for _, name := range selection {
		if skill, ok := available[name]; ok {
			content = append(content, contentFor(skill))
			continue
		}
		if version := releaseVersion(current, name); version != "" {
			files, err := p.Publisher.ReleaseSkillFiles(team.UID, currentID, name)
			if err != nil {
				return false, p.publicationBlocked(state, fmt.Errorf("read the retained skill %s: %w", name, err))
			}
			content = append(content, publish.SkillContent{Name: name, Version: version, Files: files})
			state.Retained = append(state.Retained, name)
			continue
		}
		state.Missing = append(state.Missing, name)
	}
	release, changed, err := p.Publisher.Publish(publish.TeamRef{Name: team.Name, UID: team.UID}, digest, content, commit)
	if err != nil {
		state.Error = "publication failed: " + err.Error()
		p.Catalog.SetTeam(p.withCurrent(state))
		return false, err
	}
	state.Release, state.ReleaseSelectionDigest, state.PublishedAt = release.ID, release.SelectionDigest, release.CreatedAt
	state.Skills = release.Skills
	p.Catalog.SetTeam(state)
	if changed {
		log.FromContext(ctx).Info("published Team skills", "team", team.Name, "release", release.ID, "skills", len(release.Skills),
			"retained", len(state.Retained), "missing", len(state.Missing))
	}
	if err := p.Publisher.GC(team.UID, 2, 10*time.Minute); err != nil {
		log.FromContext(ctx).Error(err, "remove old Team releases", "team", team.Name)
	}
	return false, nil
}

// withCurrent fills a Team state from its existing release.
func (p *TeamPublisher) withCurrent(state api.SkillMarketplaceTeam) api.SkillMarketplaceTeam {
	if _, release, err := p.Publisher.ReadCurrent(state.UID); err == nil && release != nil {
		state.Release, state.ReleaseSelectionDigest, state.PublishedAt = release.ID, release.SelectionDigest, release.CreatedAt
		state.Skills = release.Skills
	}
	return state
}

// publicationBlocked records why a Team's publication waits, and returns the
// error, so that the Team is tried again soon. CURRENT and the Team's
// releases stay as they are.
func (p *TeamPublisher) publicationBlocked(state api.SkillMarketplaceTeam, err error) error {
	state.Retained, state.Missing = nil, nil
	state.Error = "publication waits for the current release, which could not be read: " + err.Error()
	p.Catalog.SetTeam(p.withCurrent(state))
	return fmt.Errorf("Team %s: %w", state.Name, err)
}

func releaseVersion(release *api.SkillMarketplaceRelease, name string) string {
	if release == nil {
		return ""
	}
	for _, skill := range release.Skills {
		if skill.Name == name {
			return skill.Version
		}
	}
	return ""
}
