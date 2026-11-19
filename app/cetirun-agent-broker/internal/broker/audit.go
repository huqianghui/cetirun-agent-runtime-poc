// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Azure/CetiRun/app/internal/agenttools"
)

type auditKey struct{}
type managementAudit struct {
	owner     *Owner
	target    []any
	mutation  bool
	committed bool
	callback  bool
}
type auditWriter struct {
	http.ResponseWriter
	status int
}

func (w *auditWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *auditWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(data)
}
func auditState(r *http.Request) *managementAudit {
	state, _ := r.Context().Value(auditKey{}).(*managementAudit)
	return state
}
func auditOwner(r *http.Request, owner Owner) {
	if state := auditState(r); state != nil {
		state.owner = &owner
	}
}
func auditTarget(r *http.Request, connectionID, agentUID, bindingID string) {
	if state := auditState(r); state != nil {
		state.target = []any{"connection_id", agenttools.AuditText(connectionID, 128), "agent_uid", agenttools.AuditText(agentUID, 128), "binding_id", agenttools.AuditText(bindingID, 128)}
	}
}
func auditMutation(r *http.Request) {
	if state := auditState(r); state != nil {
		state.mutation = true
	}
}
func auditCommit(r *http.Request) {
	if state := auditState(r); state != nil {
		state.committed = true
	}
}

func managementOperation(r *http.Request) string {
	path := r.URL.Path
	switch {
	case path == "/internal/v1/service-authorizations/list":
		return "credential.list"
	case path == "/internal/v1/service-authorizations/check":
		return "credential.check"
	case path == "/internal/v1/service-credentials/issue":
		return "credential.issue"
	case strings.HasPrefix(path, "/operator/v1/instances/") && strings.HasSuffix(path, "/credential-policy"):
		switch r.Method {
		case http.MethodGet:
			return "credential.policy.readback"
		case http.MethodPut:
			return "credential.policy.apply"
		case http.MethodDelete:
			return "credential.policy.remove"
		}
	case path == "/api/v1/connections/callback":
		return "oauth.complete"
	case r.Method == "GET":
		return ""
	case strings.HasSuffix(path, "/connections/start"):
		return "oauth.start"
	case strings.HasSuffix(path, "/reconnect"):
		return "oauth.reauthorize"
	case strings.Contains(path, "/connection-bindings"):
		switch r.Method {
		case "POST":
			return "binding.connect"
		case "PUT":
			return "binding.update"
		case "DELETE":
			return "binding.disconnect"
		}
	case strings.HasSuffix(path, "/tool-identities"):
		return "identity.connect"
	case r.Method == "DELETE" && strings.HasPrefix(path, "/v1/connections/"):
		return "account.remove"
	case r.Method == "DELETE" && strings.HasPrefix(path, "/operator/"):
		return "agent.cleanup"
	}
	return ""
}

func (s *Server) auditManagement(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		operation := managementOperation(r)
		if operation == "" {
			next.ServeHTTP(w, r)
			return
		}
		state := &managementAudit{callback: operation == "oauth.complete"}
		r = r.WithContext(context.WithValue(r.Context(), auditKey{}, state))
		started := time.Now()
		logger := s.logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Info("connection management", "operation", operation, "phase", "started", "correlation_id", correlationID(r))
		wrapped := &auditWriter{ResponseWriter: w}
		next.ServeHTTP(wrapped, r)
		outcome := "rejected"
		if state.committed {
			outcome = "succeeded"
		} else if wrapped.status >= 500 {
			outcome = "dependency_unavailable"
			if state.mutation {
				outcome = "execution_unknown"
			}
		} else if state.callback && state.mutation {
			outcome = "execution_unknown"
		} else if wrapped.status >= 200 && wrapped.status < 300 && !state.callback {
			outcome = "succeeded"
		}
		attributes := []any{"operation", operation, "phase", "completed", "outcome", outcome, "http_status", wrapped.status, "correlation_id", correlationID(r), "duration_ms", time.Since(started).Milliseconds()}
		if state.owner != nil {
			attributes = append(attributes, "owner_tenant_id", state.owner.TenantID, "owner_object_id", state.owner.ObjectID)
		}
		attributes = append(attributes, state.target...)
		logger.Info("connection management", attributes...)
	})
}
