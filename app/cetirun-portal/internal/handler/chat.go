// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"context"
	"io"
	"mime"
	"net/http"
	"regexp"
	"time"

	"github.com/Azure/CetiRun/app/cetirun-portal/internal/auth"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/chatproxy"
	"github.com/Azure/CetiRun/app/internal/chataccess"
)

type ChatHandler struct {
	Instances *InstanceHandler
	Proxy     *chatproxy.Client
	limiter   chatproxy.Limiter
}

func NewChatHandler(instances *InstanceHandler) *ChatHandler {
	return &ChatHandler{Instances: instances, Proxy: chatproxy.NewClient()}
}

func (h *ChatHandler) target(w http.ResponseWriter, r *http.Request, binding bool) (chatproxy.Target, auth.Claims, bool) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-CetiRun-Chat-Dispatch", "not-sent")
	var empty chatproxy.Target
	if !h.Instances.Config.CustomChatEnabled {
		http.NotFound(w, r)
		return empty, auth.Claims{}, false
	}
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		chatproxy.Error(w, 401, "not_authenticated", "Sign in to use Chat.")
		return empty, claims, false
	}
	scope := chataccess.StorageScope(h.Instances.Config.ChatIdentityRealm, claims.Subject)
	if scope == "" || scope != r.Header.Get("X-CetiRun-Chat-Scope") {
		chatproxy.Error(w, 401, "identity_changed", "Refresh your sign-in before sending.")
		return empty, claims, false
	}
	if r.URL.RawQuery != "" {
		chatproxy.Error(w, 400, "invalid_chat_request", "Chat endpoints do not accept query parameters.")
		return empty, claims, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	agent, err := h.Instances.Kube.GetInstanceByName(ctx, r.PathValue("name"))
	if err != nil || !ownsInstance(agent, claims) {
		chatproxy.Error(w, 403, "forbidden", "This instance is not available to you.")
		return empty, claims, false
	}
	if r.Header.Get("X-CetiRun-Agent-UID") != string(agent.UID) {
		chatproxy.Error(w, 409, "agent_changed", "The instance changed. Reload before sending.")
		return empty, claims, false
	}
	target, err := h.Instances.Kube.ResolveChat(ctx, agent, h.Instances.Config.ChatCaller)
	if err != nil {
		chatproxy.Error(w, 503, "chat_dependency_unavailable", "Chat configuration cannot be verified.")
		return empty, claims, false
	}
	if binding && (!target.Available || target.Revision != r.Header.Get("X-CetiRun-Chat-Binding")) {
		chatproxy.Error(w, 409, "chat_binding_changed", "Chat is unavailable or changed. Refresh before sending.")
		return empty, claims, false
	}
	return target, claims, true
}

func (h *ChatHandler) Describe(w http.ResponseWriter, r *http.Request) {
	target, _, ok := h.target(w, r, false)
	if ok {
		writeJSON(w, 200, target)
	}
}

func (h *ChatHandler) Models(w http.ResponseWriter, r *http.Request) {
	target, claims, ok := h.target(w, r, true)
	if !ok {
		return
	}
	release, ok := h.limiter.Acquire(claims.Subject, target.AgentUID)
	if !ok {
		chatproxy.Error(w, 429, "chat_busy", "Chat is busy. Try again later.")
		return
	}
	defer release()
	models, err := h.Proxy.Models(r.Context(), target)
	if err != nil {
		chatproxy.Error(w, 502, "agent_protocol_error", "The Agent did not provide an available model list.")
		return
	}
	writeJSON(w, 200, models)
}

var chatRequestID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func (h *ChatHandler) Complete(w http.ResponseWriter, r *http.Request) {
	target, claims, ok := h.target(w, r, true)
	if !ok {
		return
	}
	if h.Instances.Config.PortalOrigin == "" || r.Header.Get("Origin") != h.Instances.Config.PortalOrigin || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		chatproxy.Error(w, 403, "invalid_origin", "Chat requires a same-origin request.")
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	id := r.Header.Get("X-CetiRun-Request-ID")
	if err != nil || media != "application/json" || !chatRequestID.MatchString(id) {
		chatproxy.Error(w, 400, "invalid_chat_request", "Send a JSON text request with a request ID.")
		return
	}
	release, ok := h.limiter.Acquire(claims.Subject, target.AgentUID)
	if !ok {
		chatproxy.Error(w, 429, "chat_busy", "Chat is busy. Try again later.")
		return
	}
	defer release()
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(15 * time.Second))
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, chatproxy.RequestBytes))
	_ = controller.SetReadDeadline(time.Time{})
	if err != nil {
		chatproxy.Error(w, 413, "chat_request_too_large", "The chat request is too large or could not be read.")
		return
	}
	request, err := chatproxy.DecodeRequest(raw)
	if err != nil {
		chatproxy.Error(w, 400, "invalid_chat_request", "Only bounded text messages and a model are supported.")
		return
	}
	switch r.Header.Get("X-CetiRun-Memory-Mode") {
	case "", "off", "no-capture":
		request.MemoryMode = r.Header.Get("X-CetiRun-Memory-Mode")
	default:
		chatproxy.Error(w, 400, "invalid_memory_mode", "Only off or no-capture restrictions are supported.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	models, err := h.Proxy.Models(ctx, target)
	if err != nil {
		chatproxy.Error(w, 502, "agent_protocol_error", "The Agent did not provide an available model list.")
		return
	}
	found := false
	for _, model := range models.Data {
		if model.ID == request.Model {
			found = true
		}
	}
	if !found {
		chatproxy.Error(w, 400, "invalid_model", "Choose a model currently offered by this Agent.")
		return
	}
	if !h.current(ctx, r.PathValue("name"), claims, target) {
		chatproxy.Error(w, 409, "chat_binding_changed", "The Agent changed before sending.")
		return
	}
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !h.current(ctx, r.PathValue("name"), claims, target) {
					cancel()
					return
				}
			}
		}
	}()
	h.Proxy.Complete(ctx, w, target, request, id)
}

func (h *ChatHandler) current(ctx context.Context, name string, claims auth.Claims, expected chatproxy.Target) bool {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	agent, err := h.Instances.Kube.GetInstanceByName(ctx, name)
	if err != nil || !ownsInstance(agent, claims) || string(agent.UID) != expected.AgentUID {
		return false
	}
	current, err := h.Instances.Kube.ObserveChat(ctx, agent, h.Instances.Config.ChatCaller)
	return err == nil && current.Available && current.Revision == expected.Revision
}
