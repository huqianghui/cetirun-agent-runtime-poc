// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package terminal

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
)

var (
	ErrDenied       = errors.New("terminal access is not authorized")
	ErrWaiting      = errors.New("agent is not ready")
	ErrRestarted    = errors.New("agent container restarted")
	ErrSessionLimit = errors.New("terminal connection limit reached")
)

type Identity struct {
	Value             string `json:"value"`
	PreferredUsername string `json:"preferredUsername,omitempty"`
}

func (i Identity) Valid() bool {
	valid := func(value string) bool {
		return value != "" && len(value) <= 256 && !strings.ContainsAny(value, "\r\n\t ,")
	}
	return valid(i.Value) && (i.PreferredUsername == "" || valid(i.PreferredUsername))
}

func (i Identity) Owns(agent *api.Agent) bool {
	if !i.Valid() {
		return false
	}
	username := i.PreferredUsername
	if username == "" {
		username = i.Value
	}
	alias := strings.SplitN(username, "@", 2)[0]
	return (agent.Spec.User.Alias != "" && strings.EqualFold(alias, agent.Spec.User.Alias)) ||
		(agent.Spec.User.Email != "" && strings.EqualFold(i.Value, agent.Spec.User.Email))
}

type AgentReader interface {
	Agent(context.Context) (*api.Agent, error)
}

func Authorize(ctx context.Context, reader AgentReader, cfg Config, identity Identity) error {
	agent, err := reader.Agent(ctx)
	if err != nil {
		return err
	}
	if agent == nil {
		return ErrDenied
	}
	if string(agent.UID) != cfg.AgentUID || agent.Name != cfg.AgentName ||
		agent.Spec.Agent.Type != api.AgentTypeCustom || !identity.Owns(agent) ||
		(agent.Status.Namespace != "" && agent.Status.Namespace != cfg.Namespace) ||
		agent.Spec.Suspended || !agent.DeletionTimestamp.IsZero() ||
		agent.Status.Phase == api.PhaseSuspended || agent.Status.Phase == api.PhaseDeleting ||
		agent.Status.Phase == api.PhaseFailed {
		return ErrDenied
	}
	return nil
}

type Session struct {
	Identity Identity  `json:"identity"`
	AgentUID string    `json:"agentUID"`
	Expires  time.Time `json:"expires"`
}

// ttyd 1.7.7 limits TTYD_USER to a short string. Pass an opaque 22-character
// one-use handle, not an email, bearer credential or serialized session.
type SessionStore struct {
	mu       sync.Mutex
	sessions map[string]Session
	now      func() time.Time
	limit    int
}

func NewSessionStore() *SessionStore {
	return &SessionStore{sessions: map[string]Session{}, now: time.Now, limit: 32}
}

func (s *SessionStore) Create(session Session) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, value := range s.sessions {
		if !s.now().Before(value.Expires) {
			delete(s.sessions, key)
		}
	}
	if len(s.sessions) >= s.limit {
		return "", ErrSessionLimit
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	key := base64.RawURLEncoding.EncodeToString(nonce[:])
	s.sessions[key] = session
	return key, nil
}

func (s *SessionStore) Claim(key string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.sessions[key]
	delete(s.sessions, key)
	if !ok || len(key) != 22 || !s.now().Before(value.Expires) {
		return Session{}, ErrDenied
	}
	return value, nil
}

func (s *SessionStore) Delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, key)
}
