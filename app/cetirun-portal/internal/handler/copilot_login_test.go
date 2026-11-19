// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/config"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/copilot"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/kube"
)

// fakeGitHubServer answers the device flow, the Copilot token exchange and
// the model list.
type fakeGitHubServer struct {
	polls        int
	answers      []map[string]any
	exchangeCode int
	// onPoll runs once while a token poll is waiting on GitHub, before it is
	// answered: what the owner does in another tab meanwhile.
	onPoll func()
	// onDeviceCode runs once while a start is waiting on GitHub for its
	// device code: what happens meanwhile.
	onDeviceCode func()
	// models is the model list GitHub returns, when not the default one.
	models string
}

func (f *fakeGitHubServer) start(t *testing.T) *copilot.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/device/code", func(w http.ResponseWriter, r *http.Request) {
		if hook := f.onDeviceCode; hook != nil {
			f.onDeviceCode = nil
			hook()
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code": "dev-secret", "user_code": "WXYZ-9876",
			"verification_uri": "https://github.com/login/device", "expires_in": 900, "interval": 5,
		})
	})
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		f.polls++
		if hook := f.onPoll; hook != nil {
			f.onPoll = nil
			hook()
		}
		answer := map[string]any{"error": "authorization_pending"}
		if len(f.answers) > 0 {
			answer, f.answers = f.answers[0], f.answers[1:]
		}
		_ = json.NewEncoder(w).Encode(answer)
	})
	server := httptest.NewServer(mux)
	mux.HandleFunc("GET /copilot_internal/v2/token", func(w http.ResponseWriter, r *http.Request) {
		if f.exchangeCode != 0 {
			w.WriteHeader(f.exchangeCode)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "copilot-token", "endpoints": map[string]string{"api": server.URL}})
	})
	mux.HandleFunc("GET /models", func(w http.ResponseWriter, r *http.Request) {
		if f.models != "" {
			_, _ = w.Write([]byte(f.models))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5.5","name":"GPT-5.5","capabilities":{"type":"chat","limits":{"max_context_window_tokens":400000,"max_output_tokens":128000}}},{"id":"claude-sonnet-4.5","capabilities":{"type":"chat"}}]}`))
	})
	t.Cleanup(server.Close)
	return &copilot.Client{
		HTTP: server.Client(), GitHubURL: server.URL, GitHubAPIURL: server.URL,
		TrustAPIBase: func(u *url.URL) bool { return "http://"+u.Host == server.URL },
	}
}

type loginFixture struct {
	t     *testing.T
	h     *InstanceHandler
	fc    client.Client
	gh    *fakeGitHubServer
	clock time.Time
}

func copilotTokenSecret(uid string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: cetirunv1alpha1.CopilotTokenSecretName, Namespace: "cetirun-alice-gw",
		Labels: map[string]string{kube.AgentUIDLabel: uid},
	}}
}

func newLoginFixture(t *testing.T, objs ...client.Object) *loginFixture {
	t.Helper()
	f := &loginFixture{t: t, gh: &fakeGitHubServer{}, clock: time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)}
	if objs == nil {
		objs = []client.Object{routedAgent(), copilotTokenSecret("uid-1")}
	}
	f.fc = newFakeClient(objs...)
	f.h = &InstanceHandler{Kube: kube.NewClient(f.fc), Config: gatewayConfig(), Copilot: f.gh.start(t), Now: func() time.Time { return f.clock }}
	return f
}

func (f *loginFixture) call(method, alias string) (*httptest.ResponseRecorder, CopilotLoginResponse) {
	f.t.Helper()
	body := ""
	if method != http.MethodGet {
		body = `{"uid":"uid-1"}`
	}
	return f.callWith(method, alias, body, nil)
}

// callWith sends body, if any, as the request's JSON body with the headers.
func (f *loginFixture) callWith(method, alias, body string, headers map[string]string) (*httptest.ResponseRecorder, CopilotLoginResponse) {
	f.t.Helper()
	return f.send(method, body, headers, func(r *http.Request) *http.Request {
		return withClaims(r, alias, alias, alias+"@example.com")
	})
}

// send makes the request as the principal that identify attaches.
func (f *loginFixture) send(method, body string, headers map[string]string, identify func(*http.Request) *http.Request) (*httptest.ResponseRecorder, CopilotLoginResponse) {
	f.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, "/api/v1/instances/alice-gw/copilot/login", reader)
	req.SetPathValue("name", "alice-gw")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	req = identify(req)
	w := httptest.NewRecorder()
	switch method {
	case http.MethodPost:
		f.h.StartCopilotLogin(w, req)
	case http.MethodGet:
		f.h.PollCopilotLogin(w, req)
	case http.MethodDelete:
		f.h.CancelCopilotLogin(w, req)
	}
	var resp CopilotLoginResponse
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			f.t.Fatalf("decode %s: %v", w.Body.String(), err)
		}
	}
	return w, resp
}

func (f *loginFixture) secret(name string) *corev1.Secret {
	f.t.Helper()
	secret := &corev1.Secret{}
	if err := f.fc.Get(context.Background(), client.ObjectKey{Namespace: "cetirun-alice-gw", Name: name}, secret); err != nil {
		f.t.Fatalf("get Secret %s: %v", name, err)
	}
	return secret
}

func (f *loginFixture) agent() *cetirunv1alpha1.Agent {
	f.t.Helper()
	agent := &cetirunv1alpha1.Agent{}
	if err := f.fc.Get(context.Background(), client.ObjectKey{Name: "alice-gw"}, agent); err != nil {
		f.t.Fatal(err)
	}
	return agent
}

// stallAfter pauses the first write that match accepts right after it is
// stored, while meanwhile runs: the replica that made it stalls there.
func (f *loginFixture) stallAfter(match func(client.Object) bool, meanwhile func()) {
	f.t.Helper()
	stalled := false
	wrapped := interceptor.NewClient(f.fc.(client.WithWatch), interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if err := c.Update(ctx, obj, opts...); err != nil || stalled || !match(obj) {
				return err
			}
			stalled = true
			meanwhile()
			return nil
		},
	})
	f.fc = wrapped
	f.h.Kube = kube.NewClient(wrapped)
}

// afterGet runs meanwhile once, right after the first read that match accepts
// returned: what another replica does just then.
func (f *loginFixture) afterGet(match func(client.Object) bool, meanwhile func()) {
	f.t.Helper()
	done := false
	wrapped := interceptor.NewClient(f.fc.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			err := c.Get(ctx, key, obj, opts...)
			if err == nil && !done && match(obj) {
				done = true
				meanwhile()
			}
			return err
		},
	})
	f.fc = wrapped
	f.h.Kube = kube.NewClient(wrapped)
}

// agentReads counts the reads of the Agent. hook runs right before read
// number at; once replaced is set, the Agent reads with another UID, as after
// it was deleted and created again under the same name.
type agentReads struct {
	count, at int
	hook      func()
	replaced  bool
}

func (f *loginFixture) watchAgentReads() *agentReads {
	f.t.Helper()
	reads := &agentReads{}
	wrapped := interceptor.NewClient(f.fc.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			agent, ok := obj.(*cetirunv1alpha1.Agent)
			if !ok {
				return c.Get(ctx, key, obj, opts...)
			}
			reads.count++
			if hook := reads.hook; hook != nil && reads.count == reads.at {
				reads.hook = nil
				hook()
			}
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if reads.replaced {
				agent.UID = "uid-2"
			}
			return nil
		},
	})
	f.fc = wrapped
	f.h.Kube = kube.NewClient(wrapped)
	return reads
}

func isLoginSecret(obj client.Object) bool {
	secret, ok := obj.(*corev1.Secret)
	return ok && secret.Name == copilotLoginSecretName
}

// commitCompletion does what a poll does when GitHub has answered: it reads
// the token Secret, then commits its sign-in, which takes the lease. It
// returns the token Secret as read, for the completion's later token write.
func (f *loginFixture) commitCompletion() *corev1.Secret {
	f.t.Helper()
	read := f.secret(cetirunv1alpha1.CopilotTokenSecretName)
	login := f.secret(copilotLoginSecretName)
	login.Data[loginState] = []byte(copilotLoginCompleting)
	login.Data[loginCompletingUntil] = []byte(f.clock.Add(copilotLoginLease).Format(time.RFC3339))
	if err := f.fc.Update(context.Background(), login); err != nil {
		f.t.Fatal(err)
	}
	return read
}

func TestCopilotLoginStoresTokenAndModels(t *testing.T) {
	f := newLoginFixture(t)
	w, resp := f.call(http.MethodPost, "alice")
	if w.Code != http.StatusOK || resp.State != "pending" || resp.UserCode != "WXYZ-9876" ||
		resp.VerificationURI != "https://github.com/login/device" || resp.Interval != 5 {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "dev-secret") {
		t.Fatal("the device code must not reach the browser")
	}
	login := f.secret(copilotLoginSecretName)
	if login.Labels[kube.AgentUIDLabel] != "uid-1" || string(login.Data[loginDeviceCode]) != "dev-secret" {
		t.Fatalf("login Secret = %+v", login)
	}

	// Within the interval, polls do not reach GitHub.
	if _, resp = f.call(http.MethodGet, "alice"); resp.State != "pending" || f.gh.polls != 0 {
		t.Fatalf("early poll: %+v, GitHub polls %d", resp, f.gh.polls)
	}
	f.clock = f.clock.Add(6 * time.Second)
	if _, resp = f.call(http.MethodGet, "alice"); resp.State != "pending" || f.gh.polls != 1 {
		t.Fatalf("pending poll: %+v, GitHub polls %d", resp, f.gh.polls)
	}
	// The same second, from another replica or tab: no second GitHub poll.
	if _, resp = f.call(http.MethodGet, "alice"); resp.State != "pending" || f.gh.polls != 1 {
		t.Fatalf("repeated poll: %+v, GitHub polls %d", resp, f.gh.polls)
	}
	f.gh.answers = []map[string]any{{"error": "slow_down", "interval": 10}}
	f.clock = f.clock.Add(6 * time.Second)
	if _, resp = f.call(http.MethodGet, "alice"); resp.State != "pending" || resp.Interval != 10 {
		t.Fatalf("slow_down: %+v", resp)
	}
	f.gh.answers = []map[string]any{{"access_token": "gho_alice", "token_type": "bearer"}}
	f.clock = f.clock.Add(6 * time.Second)
	if _, resp = f.call(http.MethodGet, "alice"); resp.State != "pending" || f.gh.polls != 2 {
		t.Fatalf("slowed poll must wait 10s: %+v, GitHub polls %d", resp, f.gh.polls)
	}
	f.clock = f.clock.Add(5 * time.Second)
	_, resp = f.call(http.MethodGet, "alice")
	if resp.State != "succeeded" || !strings.Contains(resp.Message, "2 models") || resp.UserCode != "" {
		t.Fatalf("success: %+v", resp)
	}
	if token := f.secret(cetirunv1alpha1.CopilotTokenSecretName); string(token.Data[cetirunv1alpha1.CopilotTokenSecretKey]) != "gho_alice" ||
		token.Labels[kube.AgentUIDLabel] != "uid-1" {
		t.Fatalf("token Secret = %+v", token)
	}
	var models []cetirunv1alpha1.CopilotModel
	if err := json.Unmarshal([]byte(f.agent().Annotations[cetirunv1alpha1.AnnotationCopilotModels]), &models); err != nil ||
		len(models) != 2 || models[0].ID != "gpt-5.5" || models[0].ContextWindow != 400000 || models[1].ID != "claude-sonnet-4.5" {
		t.Fatalf("annotation models = %+v (%v)", models, err)
	}
	if f.agent().Annotations[cetirunv1alpha1.AnnotationCopilotSignedInAt] == "" {
		t.Fatal("the sign-in time must be recorded for the operator")
	}
	// The token and the models carry the same sign-in generation, so the
	// operator can tell models recorded for another sign-in.
	token := f.secret(cetirunv1alpha1.CopilotTokenSecretName)
	generation := token.Annotations[cetirunv1alpha1.AnnotationCopilotSignInGeneration]
	if generation == "" || f.agent().Annotations[cetirunv1alpha1.AnnotationCopilotSignInGeneration] != generation {
		t.Fatalf("token generation %q, Agent generation %q", generation, f.agent().Annotations[cetirunv1alpha1.AnnotationCopilotSignInGeneration])
	}
	if _, err := time.Parse(time.RFC3339, token.Annotations[cetirunv1alpha1.AnnotationCopilotTokenStoredAt]); err != nil {
		t.Fatalf("the token's storage time must be recorded: %v", err)
	}
	// The sign-in is completed last, which is what the operator waits for.
	if completed := token.Annotations[cetirunv1alpha1.AnnotationCopilotSignInCompleted]; completed != generation {
		t.Fatalf("the sign-in must be completed: %q, generation %q", completed, generation)
	}
	login = f.secret(copilotLoginSecretName)
	if _, ok := login.Data[loginDeviceCode]; ok || string(login.Data[loginState]) != "succeeded" {
		t.Fatalf("finished login Secret = %v", login.Data)
	}
	// The outcome stays readable without calling GitHub again.
	if _, resp = f.call(http.MethodGet, "alice"); resp.State != "succeeded" || f.gh.polls != 3 {
		t.Fatalf("after success: %+v, GitHub polls %d", resp, f.gh.polls)
	}
}

func TestCopilotLoginOutcomes(t *testing.T) {
	t.Run("account without Copilot", func(t *testing.T) {
		f := newLoginFixture(t)
		f.gh.exchangeCode = http.StatusNotFound
		f.gh.answers = []map[string]any{{"access_token": "gho_nocopilot"}}
		f.call(http.MethodPost, "alice")
		f.clock = f.clock.Add(6 * time.Second)
		if _, resp := f.call(http.MethodGet, "alice"); resp.State != "failed" || !strings.Contains(resp.Message, "cannot use GitHub Copilot") {
			t.Fatalf("resp = %+v", resp)
		}
		if token := f.secret(cetirunv1alpha1.CopilotTokenSecretName); len(token.Data) != 0 {
			t.Fatal("a token without Copilot access must not be stored")
		}
		if _, ok := f.agent().Annotations[cetirunv1alpha1.AnnotationCopilotModels]; ok {
			t.Fatal("no models may be recorded")
		}
	})
	t.Run("denied", func(t *testing.T) {
		f := newLoginFixture(t)
		f.gh.answers = []map[string]any{{"error": "access_denied"}}
		f.call(http.MethodPost, "alice")
		f.clock = f.clock.Add(6 * time.Second)
		if _, resp := f.call(http.MethodGet, "alice"); resp.State != "denied" {
			t.Fatalf("resp = %+v", resp)
		}
	})
	t.Run("expired", func(t *testing.T) {
		f := newLoginFixture(t)
		f.call(http.MethodPost, "alice")
		f.clock = f.clock.Add(16 * time.Minute)
		if _, resp := f.call(http.MethodGet, "alice"); resp.State != "expired" || f.gh.polls != 0 {
			t.Fatalf("resp = %+v, GitHub polls %d", resp, f.gh.polls)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		f := newLoginFixture(t)
		f.call(http.MethodPost, "alice")
		if w, _ := f.call(http.MethodDelete, "alice"); w.Code != http.StatusNoContent {
			t.Fatalf("cancel: %d", w.Code)
		}
		f.clock = f.clock.Add(6 * time.Second)
		if _, resp := f.call(http.MethodGet, "alice"); resp.State != "none" || f.gh.polls != 0 {
			t.Fatalf("after cancel: %+v, GitHub polls %d", resp, f.gh.polls)
		}
	})
	t.Run("token Secret replaced by the operator meanwhile", func(t *testing.T) {
		f := newLoginFixture(t)
		f.gh.answers = []map[string]any{{"access_token": "gho_alice"}}
		f.call(http.MethodPost, "alice")
		token := f.secret(cetirunv1alpha1.CopilotTokenSecretName)
		token.Labels[kube.AgentUIDLabel] = "uid-other"
		if err := f.fc.Update(context.Background(), token); err != nil {
			t.Fatal(err)
		}
		f.clock = f.clock.Add(6 * time.Second)
		if _, resp := f.call(http.MethodGet, "alice"); resp.State != "failed" {
			t.Fatalf("resp = %+v", resp)
		}
		if token := f.secret(cetirunv1alpha1.CopilotTokenSecretName); len(token.Data) != 0 {
			t.Fatal("a Secret of another Agent UID must not receive the token")
		}
	})
}

func TestCopilotLoginPreconditions(t *testing.T) {
	notRouted := routedAgent()
	notRouted.Status.AIGateway.Routed = false
	noCopilot := routedAgent()
	noCopilot.Status.AIGateway.Copilot = nil
	deleting := routedAgent()
	deleting.Finalizers = []string{"test"}
	deleting.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	cases := []struct {
		name   string
		objs   []client.Object
		cfg    *config.Config
		alias  string
		status int
	}{
		{name: "feature off", cfg: &config.Config{}, alias: "alice", status: http.StatusNotFound},
		{name: "another user", alias: "bob", status: http.StatusForbidden},
		{name: "not routed", objs: []client.Object{notRouted, copilotTokenSecret("uid-1")}, alias: "alice", status: http.StatusConflict},
		{name: "copilot off", objs: []client.Object{noCopilot, copilotTokenSecret("uid-1")}, alias: "alice", status: http.StatusConflict},
		{name: "deleting", objs: []client.Object{deleting, copilotTokenSecret("uid-1")}, alias: "alice", status: http.StatusConflict},
		{name: "no token Secret yet", objs: []client.Object{routedAgent()}, alias: "alice", status: http.StatusConflict},
		{name: "token Secret of another Agent", objs: []client.Object{routedAgent(), copilotTokenSecret("uid-old")}, alias: "alice", status: http.StatusConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLoginFixture(t, tc.objs...)
			if tc.cfg != nil {
				f.h.Config = *tc.cfg
			}
			if w, _ := f.call(http.MethodPost, tc.alias); w.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
		})
	}
}

// An Agent bound to a verified owner accepts sign-in requests only from that
// tenant and object ID: another principal with the same alias can neither
// start, read nor cancel its sign-in.
func TestCopilotLoginRequiresTheBoundOwner(t *testing.T) {
	const tenant, owner, other = "11111111-1111-1111-1111-111111111111",
		"22222222-2222-2222-2222-222222222222", "33333333-3333-3333-3333-333333333333"
	bound := routedAgent()
	if bound.Annotations == nil {
		bound.Annotations = map[string]string{}
	}
	bound.Annotations[cetirunv1alpha1.AnnotationOwnerTenantID] = tenant
	bound.Annotations[cetirunv1alpha1.AnnotationOwnerObjectID] = owner
	f := newLoginFixture(t, bound, copilotTokenSecret("uid-1"))
	as := func(objectID string) func(*http.Request) *http.Request {
		return func(r *http.Request) *http.Request { return withAgentIdentityClaims(r, "alice", tenant, objectID) }
	}
	bodyFor := func(method string) string {
		if method == http.MethodGet {
			return ""
		}
		return `{"uid":"uid-1"}`
	}
	exists := func() bool {
		t.Helper()
		return f.fc.Get(context.Background(), client.ObjectKey{Namespace: "cetirun-alice-gw", Name: copilotLoginSecretName}, &corev1.Secret{}) == nil
	}

	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
		if w, _ := f.send(method, bodyFor(method), nil, as(other)); w.Code != http.StatusForbidden {
			t.Fatalf("%s by a same-alias principal: status %d, want 403", method, w.Code)
		}
	}
	if exists() {
		t.Fatal("a refused start must not create a sign-in")
	}

	// The bound owner starts a sign-in; the other principal still cannot read
	// or cancel it.
	if w, resp := f.send(http.MethodPost, bodyFor(http.MethodPost), nil, as(owner)); w.Code != http.StatusOK || resp.State != copilotLoginPending {
		t.Fatalf("start by the owner: status %d %s", w.Code, w.Body.String())
	}
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		if w, _ := f.send(method, bodyFor(method), nil, as(other)); w.Code != http.StatusForbidden {
			t.Fatalf("%s of the owner's sign-in by a same-alias principal: status %d, want 403", method, w.Code)
		}
	}
	if state := string(f.secret(copilotLoginSecretName).Data[loginState]); state != copilotLoginPending {
		t.Fatalf("the owner's sign-in must stay pending, got %q", state)
	}
}

// A poll waiting on GitHub when the owner cancels must not store the token it
// then receives, nor record that account's models.
func TestCopilotLoginCancelledWhilePolledStoresNothing(t *testing.T) {
	f := newLoginFixture(t)
	f.call(http.MethodPost, "alice")
	f.gh.answers = []map[string]any{{"access_token": "gho_cancelled"}}
	cancelled := 0
	f.gh.onPoll = func() {
		w, _ := f.call(http.MethodDelete, "alice")
		cancelled = w.Code
	}
	f.clock = f.clock.Add(6 * time.Second)
	_, resp := f.call(http.MethodGet, "alice")
	if cancelled != http.StatusNoContent {
		t.Fatalf("cancel during the poll: status %d", cancelled)
	}
	if resp.State != copilotLoginNone {
		t.Fatalf("the cancelled sign-in must stay cancelled, got %+v", resp)
	}
	if token := f.secret(cetirunv1alpha1.CopilotTokenSecretName); len(token.Data) != 0 {
		t.Fatal("a cancelled sign-in must not store its token")
	}
	if _, ok := f.agent().Annotations[cetirunv1alpha1.AnnotationCopilotModels]; ok {
		t.Fatal("a cancelled sign-in must not record models")
	}
}

// A poll of an older sign-in that GitHub answers after the owner started a
// new one must not store the older account's token.
func TestCopilotLoginReplacedWhilePolledStoresNothing(t *testing.T) {
	f := newLoginFixture(t)
	f.call(http.MethodPost, "alice")
	f.gh.answers = []map[string]any{{"access_token": "gho_older_account"}}
	restarted := 0
	f.gh.onPoll = func() {
		w, _ := f.call(http.MethodPost, "alice")
		restarted = w.Code
	}
	f.clock = f.clock.Add(6 * time.Second)
	_, resp := f.call(http.MethodGet, "alice")
	if restarted != http.StatusOK {
		t.Fatalf("start during the poll: status %d", restarted)
	}
	if resp.State != copilotLoginPending || resp.UserCode == "" {
		t.Fatalf("the new sign-in must stay pending, got %+v", resp)
	}
	if token := f.secret(cetirunv1alpha1.CopilotTokenSecretName); len(token.Data) != 0 {
		t.Fatal("the replaced sign-in must not store its token")
	}
	if _, ok := f.agent().Annotations[cetirunv1alpha1.AnnotationCopilotModels]; ok {
		t.Fatal("the replaced sign-in must not record models")
	}
	if login := f.secret(copilotLoginSecretName); string(login.Data[loginState]) != copilotLoginPending || len(login.Data[loginDeviceCode]) == 0 {
		t.Fatalf("the new sign-in must be intact, got %v", login.Data)
	}
}

// Once a poll has committed its sign-in, a start or a cancel waits for the
// outcome; if the replica storing the token stops, the lease ends it.
func TestCopilotLoginBeingCompletedHoldsOffStartAndCancel(t *testing.T) {
	f := newLoginFixture(t)
	f.call(http.MethodPost, "alice")
	login := f.secret(copilotLoginSecretName)
	login.Data[loginState] = []byte(copilotLoginCompleting)
	login.Data[loginCompletingUntil] = []byte(f.clock.Add(copilotLoginLease).Format(time.RFC3339))
	delete(login.Data, loginDeviceCode)
	if err := f.fc.Update(context.Background(), login); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		if w, _ := f.call(method, "alice"); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "copilot_login_completing") {
			t.Fatalf("%s while completing: status %d %s", method, w.Code, w.Body.String())
		}
	}
	if _, resp := f.call(http.MethodGet, "alice"); resp.State != copilotLoginPending || resp.UserCode != "WXYZ-9876" || f.gh.polls != 0 {
		t.Fatalf("the browser keeps waiting without a new GitHub poll: %+v, GitHub polls %d", resp, f.gh.polls)
	}

	f.clock = f.clock.Add(copilotLoginLease + time.Second)
	if _, resp := f.call(http.MethodGet, "alice"); resp.State != copilotLoginFailed || !strings.Contains(resp.Message, "did not finish") {
		t.Fatalf("an expired completion must be reported as failed: %+v", resp)
	}
	if w, resp := f.call(http.MethodPost, "alice"); w.Code != http.StatusOK || resp.State != copilotLoginPending {
		t.Fatalf("a new sign-in after the lease: status %d %+v", w.Code, resp)
	}
}

// A replica completing a sign-in can stall past its lease, after which the
// owner can start, cancel or end the sign-in. Whatever the owner did then must
// stand when the stalled replica resumes: its token and models are fenced at
// the point they would be stored, not by the lease.
func TestCopilotLoginStalledCompletionIsFenced(t *testing.T) {
	committed := func(obj client.Object) bool {
		secret, ok := obj.(*corev1.Secret)
		return ok && secret.Name == copilotLoginSecretName && string(secret.Data[loginState]) == copilotLoginCompleting
	}
	storedToken := func(obj client.Object) bool {
		secret, ok := obj.(*corev1.Secret)
		return ok && secret.Name == cetirunv1alpha1.CopilotTokenSecretName &&
			string(secret.Data[cetirunv1alpha1.CopilotTokenSecretKey]) == "gho_alice"
	}
	const bobModels = `{"data":[{"id":"bob-model","capabilities":{"type":"chat"}}]}`
	signInAsBob := func(f *loginFixture) {
		f.t.Helper()
		if w, _ := f.call(http.MethodPost, "alice"); w.Code != http.StatusOK {
			f.t.Fatalf("a new sign-in after the lease: status %d %s", w.Code, w.Body.String())
		}
		f.gh.answers, f.gh.models = []map[string]any{{"access_token": "gho_bob"}}, bobModels
		f.clock = f.clock.Add(6 * time.Second)
		if _, resp := f.call(http.MethodGet, "alice"); resp.State != copilotLoginSucceeded {
			f.t.Fatalf("the new sign-in must complete: %+v", resp)
		}
	}
	for _, tc := range []struct {
		name       string
		stallAfter func(client.Object) bool
		meanwhile  func(*loginFixture)
		wantToken  string
		wantModels string
		wantState  string
	}{
		{name: "a newer sign-in after the commit", stallAfter: committed, meanwhile: signInAsBob,
			wantToken: "gho_bob", wantModels: "bob-model", wantState: copilotLoginSucceeded},
		{name: "a newer sign-in after the token was stored", stallAfter: storedToken, meanwhile: signInAsBob,
			wantToken: "gho_bob", wantModels: "bob-model", wantState: copilotLoginSucceeded},
		{name: "a new start after the commit", stallAfter: committed, wantState: copilotLoginPending,
			meanwhile: func(f *loginFixture) {
				if w, _ := f.call(http.MethodPost, "alice"); w.Code != http.StatusOK {
					f.t.Fatalf("a new sign-in after the lease: status %d", w.Code)
				}
			}},
		{name: "a cancel after the commit", stallAfter: committed, wantState: copilotLoginNone,
			meanwhile: func(f *loginFixture) {
				if w, _ := f.call(http.MethodDelete, "alice"); w.Code != http.StatusNoContent {
					f.t.Fatalf("cancel after the lease: status %d", w.Code)
				}
			}},
		{name: "the sign-in reported as failed", stallAfter: committed, wantState: copilotLoginFailed,
			meanwhile: func(f *loginFixture) {
				if _, resp := f.call(http.MethodGet, "alice"); resp.State != copilotLoginFailed {
					f.t.Fatalf("the abandoned sign-in must be reported as failed: %+v", resp)
				}
			}},
		// Ended after it stored its token: the token stays, as written, but
		// the models of a sign-in cancelled or reported as failed are not
		// recorded, so it never takes effect.
		{name: "a cancel after the token was stored", stallAfter: storedToken, wantToken: "gho_alice", wantState: copilotLoginNone,
			meanwhile: func(f *loginFixture) {
				if w, _ := f.call(http.MethodDelete, "alice"); w.Code != http.StatusNoContent {
					f.t.Fatalf("cancel after the lease: status %d", w.Code)
				}
			}},
		{name: "the sign-in reported as failed after the token was stored", stallAfter: storedToken, wantToken: "gho_alice", wantState: copilotLoginFailed,
			meanwhile: func(f *loginFixture) {
				if _, resp := f.call(http.MethodGet, "alice"); resp.State != copilotLoginFailed {
					f.t.Fatalf("the abandoned sign-in must be reported as failed: %+v", resp)
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLoginFixture(t)
			f.call(http.MethodPost, "alice")
			f.gh.answers = []map[string]any{{"access_token": "gho_alice"}}
			f.stallAfter(tc.stallAfter, func() {
				f.clock = f.clock.Add(copilotLoginLease + time.Second)
				tc.meanwhile(f)
			})
			f.clock = f.clock.Add(6 * time.Second)
			_, resp := f.call(http.MethodGet, "alice")
			if resp.State != tc.wantState {
				t.Fatalf("the stalled completion must report what happened meanwhile, %q, got %+v", tc.wantState, resp)
			}
			if token := string(f.secret(cetirunv1alpha1.CopilotTokenSecretName).Data[cetirunv1alpha1.CopilotTokenSecretKey]); token != tc.wantToken {
				t.Fatalf("stored token %q, want %q", token, tc.wantToken)
			}
			models := f.agent().Annotations[cetirunv1alpha1.AnnotationCopilotModels]
			if tc.wantModels == "" && models != "" || !strings.Contains(models, tc.wantModels) || strings.Contains(models, "gpt-5.5") {
				t.Fatalf("recorded models %q, want %q only", models, tc.wantModels)
			}
		})
	}
}

// A completion checks the token Secret just before it records the models,
// and its replica can stall right after that check. A cancel once the lease
// has run out raises the floor, but leaves the Agent as it was, so the
// stalled write of the models still lands when the replica resumes. The
// completion that would let them take effect is written under the token
// Secret's version and finds the floor past the sign-in: the sign-in never
// completes, and the operator never uses those models.
func TestCopilotLoginFencedAfterItsLastCheckNeverCompletes(t *testing.T) {
	f := newLoginFixture(t)
	f.call(http.MethodPost, "alice")
	f.gh.answers = []map[string]any{{"access_token": "gho_alice"}}
	storedToken := func(obj client.Object) bool {
		secret, ok := obj.(*corev1.Secret)
		return ok && secret.Name == cetirunv1alpha1.CopilotTokenSecretName &&
			string(secret.Data[cetirunv1alpha1.CopilotTokenSecretKey]) == "gho_alice"
	}
	cancelled := 0
	// The first read that finds the stored token is the check before the
	// models are written.
	f.afterGet(storedToken, func() {
		f.clock = f.clock.Add(copilotLoginLease + time.Second)
		w, _ := f.call(http.MethodDelete, "alice")
		cancelled = w.Code
	})
	f.clock = f.clock.Add(6 * time.Second)
	_, resp := f.call(http.MethodGet, "alice")
	if cancelled != http.StatusNoContent {
		t.Fatalf("a cancel after the lease: status %d", cancelled)
	}
	if resp.State != copilotLoginNone {
		t.Fatalf("the cancel must stand: %+v", resp)
	}
	token := f.secret(cetirunv1alpha1.CopilotTokenSecretName)
	generation := token.Annotations[cetirunv1alpha1.AnnotationCopilotSignInGeneration]
	if recorded := f.agent().Annotations[cetirunv1alpha1.AnnotationCopilotSignInGeneration]; generation == "" || recorded != generation {
		t.Fatalf("setup: the stalled write of the models must land: token generation %q, Agent generation %q", generation, recorded)
	}
	if completed := token.Annotations[cetirunv1alpha1.AnnotationCopilotSignInCompleted]; completed != "" {
		t.Fatalf("a sign-in cancelled before it completed must never complete, got %q", completed)
	}
}

// A completion can stall after it completed the sign-in and before it
// reported that. A cancel or a poll once its lease has run out finds the
// sign-in completed and in effect, and reports it as succeeded instead of
// fencing it and reporting it as cancelled or failed.
func TestCopilotLoginCompletedBeforeItsEndIsReportedAsSucceeded(t *testing.T) {
	completed := func(obj client.Object) bool {
		secret, ok := obj.(*corev1.Secret)
		return ok && secret.Name == cetirunv1alpha1.CopilotTokenSecretName &&
			secret.Annotations[cetirunv1alpha1.AnnotationCopilotSignInCompleted] != ""
	}
	for _, tc := range []struct {
		name string
		step func(*loginFixture)
	}{
		{name: "a cancel", step: func(f *loginFixture) {
			if w, _ := f.call(http.MethodDelete, "alice"); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "copilot_login_completed") {
				f.t.Fatalf("a cancel of a completed sign-in: status %d %s", w.Code, w.Body.String())
			}
		}},
		{name: "the end of the expired completion", step: func(f *loginFixture) {
			if _, resp := f.call(http.MethodGet, "alice"); resp.State != copilotLoginSucceeded {
				f.t.Fatalf("a completed sign-in must be reported as succeeded: %+v", resp)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLoginFixture(t)
			f.call(http.MethodPost, "alice")
			f.gh.answers = []map[string]any{{"access_token": "gho_alice"}}
			f.stallAfter(completed, func() {
				f.clock = f.clock.Add(copilotLoginLease + time.Second)
				tc.step(f)
			})
			f.clock = f.clock.Add(6 * time.Second)
			if _, resp := f.call(http.MethodGet, "alice"); resp.State != copilotLoginSucceeded {
				t.Fatalf("the completed sign-in must stay reported as succeeded: %+v", resp)
			}
			token := f.secret(cetirunv1alpha1.CopilotTokenSecretName)
			if generation := token.Annotations[cetirunv1alpha1.AnnotationCopilotSignInGeneration]; generation == "" ||
				token.Annotations[cetirunv1alpha1.AnnotationCopilotSignInCompleted] != generation {
				t.Fatalf("the completion must stand: %v", token.Annotations)
			}
			if floor := token.Annotations[copilotSignInFloorAnnotation]; floor != "1" {
				t.Fatalf("a completed sign-in must not be fenced: floor %q", floor)
			}
		})
	}
}

// A cancel or the end of an expired completion records the sign-in as ending
// first and fences only then, so the completion can complete in between.
// Checking for the completion is part of the fence's update of the token
// Secret: the step finds it, leaves the floor, and reports the sign-in as
// succeeded.
func TestCopilotLoginCompletedWhileEndedIsReportedAsSucceeded(t *testing.T) {
	loginWith := func(state string) func(client.Object) bool {
		return func(obj client.Object) bool {
			secret, ok := obj.(*corev1.Secret)
			return ok && secret.Name == copilotLoginSecretName && string(secret.Data[loginState]) == state &&
				string(secret.Data[loginNumber]) == "1"
		}
	}
	for _, tc := range []struct {
		name     string
		accepted func(client.Object) bool
		step     func(*loginFixture)
	}{
		{name: "a cancel", accepted: loginWith(copilotLoginEnding), step: func(f *loginFixture) {
			if w, _ := f.call(http.MethodDelete, "alice"); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "copilot_login_completed") {
				f.t.Fatalf("a cancel of a sign-in completed meanwhile: status %d %s", w.Code, w.Body.String())
			}
		}},
		{name: "the end of the expired completion", accepted: loginWith(copilotLoginEnding), step: func(f *loginFixture) {
			if _, resp := f.call(http.MethodGet, "alice"); resp.State != copilotLoginSucceeded {
				f.t.Fatalf("a sign-in completed meanwhile must be reported as succeeded: %+v", resp)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLoginFixture(t)
			f.call(http.MethodPost, "alice")
			// A completion committed sign-in 1 with its generation and
			// stored the token; its replica stalled past the lease.
			const generation = "0a1b2c3d"
			login := f.secret(copilotLoginSecretName)
			login.Data[loginState] = []byte(copilotLoginCompleting)
			login.Data[loginCompletingUntil] = []byte(f.clock.Add(copilotLoginLease).Format(time.RFC3339))
			login.Data[loginGeneration] = []byte(generation)
			if err := f.fc.Update(context.Background(), login); err != nil {
				t.Fatal(err)
			}
			token := f.secret(cetirunv1alpha1.CopilotTokenSecretName)
			token.Data = map[string][]byte{cetirunv1alpha1.CopilotTokenSecretKey: []byte("gho_alice")}
			token.Annotations[cetirunv1alpha1.AnnotationCopilotSignInGeneration] = generation
			if err := f.fc.Update(context.Background(), token); err != nil {
				t.Fatal(err)
			}
			f.clock = f.clock.Add(copilotLoginLease + time.Second)
			// The replica resumes and completes the sign-in right after the
			// step recorded its outcome.
			f.stallAfter(tc.accepted, func() {
				if err := f.h.completeCopilotSignIn(context.Background(), f.agent(), 1, generation); err != nil {
					t.Fatalf("setup: the completion must be accepted: %v", err)
				}
			})
			tc.step(f)
			if state := string(f.secret(copilotLoginSecretName).Data[loginState]); state != copilotLoginSucceeded {
				t.Fatalf("the sign-in must be recorded as succeeded, got %q", state)
			}
			token = f.secret(cetirunv1alpha1.CopilotTokenSecretName)
			if token.Annotations[cetirunv1alpha1.AnnotationCopilotSignInCompleted] != generation || token.Annotations[copilotSignInFloorAnnotation] != "1" {
				t.Fatalf("the completion must stand unfenced: %v", token.Annotations)
			}
		})
	}
}

// The write that completes a sign-in can be applied with only its response
// lost, and the operator then uses the sign-in. The completion reads it back
// before recording an outcome, and reports it as succeeded; while it cannot be
// read, the sign-in keeps its commit, and a poll after the lease reports what
// the token Secret holds then. A completion that failed without completing is
// reported as failed, and fenced, so that it cannot complete later.
func TestCopilotLoginCompletionWhoseResponseIsLost(t *testing.T) {
	isCompletion := func(obj client.Object) bool {
		secret, ok := obj.(*corev1.Secret)
		return ok && secret.Name == cetirunv1alpha1.CopilotTokenSecretName &&
			secret.Annotations[cetirunv1alpha1.AnnotationCopilotSignInCompleted] != ""
	}
	for _, tc := range []struct {
		name string
		// applied: the completion's write is applied before its error.
		// readFails: the read back after the error fails once.
		applied, readFails bool
		wantState          string
		wantCompleted      bool
		wantFloor          string
	}{
		{name: "applied, read back", applied: true, wantState: copilotLoginSucceeded, wantCompleted: true, wantFloor: "1"},
		{name: "applied, read back fails", applied: true, readFails: true, wantState: copilotLoginPending, wantCompleted: true, wantFloor: "1"},
		{name: "not applied", wantState: copilotLoginFailed, wantFloor: "2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLoginFixture(t)
			f.call(http.MethodPost, "alice")
			f.gh.answers = []map[string]any{{"access_token": "gho_alice"}}
			failed, readFailed := false, false
			wrapped := interceptor.NewClient(f.fc.(client.WithWatch), interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if failed || !isCompletion(obj) {
						return c.Update(ctx, obj, opts...)
					}
					failed = true
					if tc.applied {
						if err := c.Update(ctx, obj, opts...); err != nil {
							return err
						}
					}
					return errors.New("connection reset by peer")
				},
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if tc.readFails && failed && !readFailed && key.Name == cetirunv1alpha1.CopilotTokenSecretName {
						readFailed = true
						return errors.New("connection reset by peer")
					}
					return c.Get(ctx, key, obj, opts...)
				},
			})
			f.fc = wrapped
			f.h.Kube = kube.NewClient(wrapped)
			f.clock = f.clock.Add(6 * time.Second)
			if _, resp := f.call(http.MethodGet, "alice"); resp.State != tc.wantState {
				t.Fatalf("the completion must report %q, got %+v", tc.wantState, resp)
			}
			if tc.readFails {
				if state := string(f.secret(copilotLoginSecretName).Data[loginState]); state != copilotLoginCompleting {
					t.Fatalf("the sign-in must keep its commit, got %q", state)
				}
				f.clock = f.clock.Add(copilotLoginLease + time.Second)
				if _, resp := f.call(http.MethodGet, "alice"); resp.State != copilotLoginSucceeded {
					t.Fatalf("after the lease, the completed sign-in must be reported as succeeded: %+v", resp)
				}
			}
			token := f.secret(cetirunv1alpha1.CopilotTokenSecretName)
			generation := token.Annotations[cetirunv1alpha1.AnnotationCopilotSignInGeneration]
			if completed := generation != "" && token.Annotations[cetirunv1alpha1.AnnotationCopilotSignInCompleted] == generation; completed != tc.wantCompleted {
				t.Fatalf("completed %v, want %v: %v", completed, tc.wantCompleted, token.Annotations)
			}
			if floor := token.Annotations[copilotSignInFloorAnnotation]; floor != tc.wantFloor {
				t.Fatalf("floor %q, want %q", floor, tc.wantFloor)
			}
		})
	}
}

// A sign-in being ended keeps its generation, and the browser sees it as
// pending, until whether it completed is known; only then is the outcome
// published. Here the sign-in completed, but its replica stopped before
// reporting that, and its lease ran out. A step ending it that cannot read
// the token Secret leaves it ending for the next poll; a poll that comes
// while another settles it never sees a failure.
func TestCopilotLoginEndingKeepsItsOutcomeOpenUntilSettled(t *testing.T) {
	const generation = "0a1b2c3d"
	setup := func(t *testing.T) *loginFixture {
		f := newLoginFixture(t)
		f.call(http.MethodPost, "alice")
		login := f.secret(copilotLoginSecretName)
		login.Data[loginState] = []byte(copilotLoginCompleting)
		login.Data[loginCompletingUntil] = []byte(f.clock.Add(copilotLoginLease).Format(time.RFC3339))
		login.Data[loginGeneration] = []byte(generation)
		if err := f.fc.Update(context.Background(), login); err != nil {
			t.Fatal(err)
		}
		token := f.secret(cetirunv1alpha1.CopilotTokenSecretName)
		token.Data = map[string][]byte{cetirunv1alpha1.CopilotTokenSecretKey: []byte("gho_alice")}
		token.Annotations[cetirunv1alpha1.AnnotationCopilotSignInGeneration] = generation
		token.Annotations[cetirunv1alpha1.AnnotationCopilotSignInCompleted] = generation
		if err := f.fc.Update(context.Background(), token); err != nil {
			t.Fatal(err)
		}
		f.clock = f.clock.Add(copilotLoginLease + time.Second)
		return f
	}
	// The first write of a step that ends sign-in 1: ending, or, before it
	// existed, the failure or the cleared sign-in recorded first.
	endsIt := func(obj client.Object) bool {
		secret, ok := obj.(*corev1.Secret)
		state := string(secret.Data[loginState])
		return ok && secret.Name == copilotLoginSecretName && string(secret.Data[loginNumber]) == "1" &&
			(state == copilotLoginEnding || state == copilotLoginFailed || state == "")
	}
	// failTokenReadAfterEnding fails the first read of the token Secret after
	// the sign-in is recorded as ending.
	failTokenReadAfterEnding := func(f *loginFixture) {
		ended, failed := false, false
		wrapped := interceptor.NewClient(f.fc.(client.WithWatch), interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if err := c.Update(ctx, obj, opts...); err != nil {
					return err
				}
				ended = ended || endsIt(obj)
				return nil
			},
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if ended && !failed && key.Name == cetirunv1alpha1.CopilotTokenSecretName {
					failed = true
					return errors.New("connection reset by peer")
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
		f.fc = wrapped
		f.h.Kube = kube.NewClient(wrapped)
	}
	settled := func(f *loginFixture) {
		f.t.Helper()
		if _, resp := f.call(http.MethodGet, "alice"); resp.State != copilotLoginSucceeded {
			f.t.Fatalf("the next poll must settle the completed sign-in as succeeded: %+v", resp)
		}
		if floor := f.secret(cetirunv1alpha1.CopilotTokenSecretName).Annotations[copilotSignInFloorAnnotation]; floor != "1" {
			f.t.Fatalf("a completed sign-in must not be fenced: floor %q", floor)
		}
	}
	stillEnding := func(f *loginFixture) {
		f.t.Helper()
		login := f.secret(copilotLoginSecretName)
		if string(login.Data[loginState]) != copilotLoginEnding || string(login.Data[loginGeneration]) != generation {
			f.t.Fatalf("the sign-in must stay ending with its generation: %v", login.Data)
		}
	}

	t.Run("the end of the lease cannot read the token Secret", func(t *testing.T) {
		f := setup(t)
		failTokenReadAfterEnding(f)
		if _, resp := f.call(http.MethodGet, "alice"); resp.State != copilotLoginPending {
			t.Fatalf("an outcome not yet known must be reported as pending: %+v", resp)
		}
		stillEnding(f)
		settled(f)
	})
	t.Run("a cancel cannot read the token Secret", func(t *testing.T) {
		f := setup(t)
		failTokenReadAfterEnding(f)
		if w, _ := f.call(http.MethodDelete, "alice"); w.Code != http.StatusInternalServerError {
			t.Fatalf("a cancel whose outcome is not known: status %d %s", w.Code, w.Body.String())
		}
		stillEnding(f)
		settled(f)
	})
	t.Run("a poll while another settles it", func(t *testing.T) {
		f := setup(t)
		var during CopilotLoginResponse
		f.stallAfter(endsIt, func() { _, during = f.call(http.MethodGet, "alice") })
		_, resp := f.call(http.MethodGet, "alice")
		if during.State != copilotLoginSucceeded || resp.State != copilotLoginSucceeded {
			t.Fatalf("polls during the settlement must not see a failure: during %+v, then %+v", during, resp)
		}
		settled(f)
	})
}

// A start refused because a completion took its lease while GitHub answered
// the start leaves that completion able to store its token: the fence moves
// only once a start has replaced the sign-in.
func TestCopilotLoginRefusedStartLeavesTheCompletionUnfenced(t *testing.T) {
	f := newLoginFixture(t)
	f.call(http.MethodPost, "alice")
	var read *corev1.Secret
	f.gh.onDeviceCode = func() { read = f.commitCompletion() }
	if w, _ := f.call(http.MethodPost, "alice"); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "copilot_login_completing") {
		t.Fatalf("a start during a completion: status %d %s", w.Code, w.Body.String())
	}
	read.Data = map[string][]byte{cetirunv1alpha1.CopilotTokenSecretKey: []byte("gho_completing")}
	if err := f.fc.Update(context.Background(), read); err != nil {
		t.Fatalf("the refused start must not fence the completion: %v", err)
	}
}

// The same for a cancel: a completion that commits after the cancel read the
// sign-in makes the cancel conflict and then refuse, and the completion still
// stores its token.
func TestCopilotLoginRefusedCancelLeavesTheCompletionUnfenced(t *testing.T) {
	f := newLoginFixture(t)
	f.call(http.MethodPost, "alice")
	var read *corev1.Secret
	reads := f.watchAgentReads()
	// The cancel's second read of the Agent comes after it read the sign-in.
	reads.at, reads.hook = 2, func() { read = f.commitCompletion() }
	if w, _ := f.call(http.MethodDelete, "alice"); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "copilot_login_completing") {
		t.Fatalf("a cancel during a completion: status %d %s", w.Code, w.Body.String())
	}
	read.Data = map[string][]byte{cetirunv1alpha1.CopilotTokenSecretKey: []byte("gho_completing")}
	if err := f.fc.Update(context.Background(), read); err != nil {
		t.Fatalf("the refused cancel must not fence the completion: %v", err)
	}
}

// A start that waited on GitHub while its Agent was deleted and created again
// under the same name changes nothing, not even the replacement's sign-in.
func TestCopilotLoginStartForAReplacedAgentChangesNothing(t *testing.T) {
	f := newLoginFixture(t)
	reads := f.watchAgentReads()
	f.gh.onDeviceCode = func() {
		reads.replaced = true
		// The replacement has already started its own sign-in.
		if err := f.fc.Create(context.Background(), &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: copilotLoginSecretName, Namespace: "cetirun-alice-gw", Labels: map[string]string{kube.AgentUIDLabel: "uid-2"}},
			Data:       map[string][]byte{loginState: []byte(copilotLoginPending), loginUserCode: []byte("NEW2-0000")},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if w, _ := f.call(http.MethodPost, "alice"); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "instance_changed") {
		t.Fatalf("a start for a replaced Agent: status %d %s", w.Code, w.Body.String())
	}
	login := f.secret(copilotLoginSecretName)
	if login.Labels[kube.AgentUIDLabel] != "uid-2" || string(login.Data[loginUserCode]) != "NEW2-0000" {
		t.Fatalf("the replacement's sign-in must be left alone: %v %v", login.Labels, login.Data)
	}
	if token := f.secret(cetirunv1alpha1.CopilotTokenSecretName); token.Annotations[copilotSignInFloorAnnotation] != "" {
		t.Fatal("a refused start must not fence completions")
	}
}

// A cancel for an Agent replaced after the cancel read the sign-in clears
// nothing.
func TestCopilotLoginCancelForAReplacedAgentChangesNothing(t *testing.T) {
	f := newLoginFixture(t)
	f.call(http.MethodPost, "alice")
	reads := f.watchAgentReads()
	reads.at, reads.hook = 2, func() { reads.replaced = true }
	if w, _ := f.call(http.MethodDelete, "alice"); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "instance_changed") {
		t.Fatalf("a cancel for a replaced Agent: status %d %s", w.Code, w.Body.String())
	}
	if login := f.secret(copilotLoginSecretName); string(login.Data[loginState]) != copilotLoginPending || len(login.Data[loginDeviceCode]) == 0 {
		t.Fatalf("the sign-in must be left alone: %v", login.Data)
	}
}

// A sign-in Secret labelled for another UID was left by an Agent of the same
// name that no longer exists: a cancel leaves it alone, and the current
// Agent's start replaces it.
func TestCopilotLoginLeftoverOfAnEarlierAgent(t *testing.T) {
	leftover := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: copilotLoginSecretName, Namespace: "cetirun-alice-gw", Labels: map[string]string{kube.AgentUIDLabel: "uid-0"}},
		Data:       map[string][]byte{loginState: []byte(copilotLoginPending), loginDeviceCode: []byte("earlier-agent")},
	}
	f := newLoginFixture(t, routedAgent(), copilotTokenSecret("uid-1"), leftover)
	if w, _ := f.call(http.MethodDelete, "alice"); w.Code != http.StatusNoContent {
		t.Fatalf("cancel: status %d %s", w.Code, w.Body.String())
	}
	if login := f.secret(copilotLoginSecretName); string(login.Data[loginDeviceCode]) != "earlier-agent" {
		t.Fatalf("a cancel must not clear another Agent's sign-in: %v", login.Data)
	}
	if w, resp := f.call(http.MethodPost, "alice"); w.Code != http.StatusOK || resp.State != copilotLoginPending {
		t.Fatalf("start: status %d %+v", w.Code, resp)
	}
	if login := f.secret(copilotLoginSecretName); login.Labels[kube.AgentUIDLabel] != "uid-1" || string(login.Data[loginDeviceCode]) != "dev-secret" {
		t.Fatalf("the start must replace the leftover: %v %v", login.Labels, login.Data)
	}
}

// A poll that finds a completion past its lease records it as failed, and
// fences only once that is accepted: if a newer sign-in replaced it meanwhile,
// and its completion has committed, that completion still stores its token.
func TestCopilotLoginRefusedExpiryLeavesANewerCompletionUnfenced(t *testing.T) {
	f := newLoginFixture(t)
	f.call(http.MethodPost, "alice")
	f.commitCompletion()
	f.clock = f.clock.Add(copilotLoginLease + time.Second)
	var read *corev1.Secret
	f.afterGet(isLoginSecret, func() {
		// Right after the poll read the expired sign-in, a newer start
		// replaced it, and its completion committed.
		login := f.secret(copilotLoginSecretName)
		login.Data = map[string][]byte{loginState: []byte(copilotLoginPending), loginDeviceCode: []byte("newer")}
		if err := f.fc.Update(context.Background(), login); err != nil {
			t.Fatal(err)
		}
		read = f.commitCompletion()
	})
	if _, resp := f.call(http.MethodGet, "alice"); resp.State != copilotLoginPending {
		t.Fatalf("the newer sign-in must be reported: %+v", resp)
	}
	read.Data = map[string][]byte{cetirunv1alpha1.CopilotTokenSecretKey: []byte("gho_newer")}
	if err := f.fc.Update(context.Background(), read); err != nil {
		t.Fatalf("a refused expiry must not fence the newer completion: %v", err)
	}
}

// A start, a cancel, or a poll ending an expired completion fences only after
// its own change was accepted, so a replica can stall in between. A newer
// sign-in can start meanwhile and its completion commit: when the stalled
// step resumes, the floor is already past the sign-in it ended, and nothing
// is written, so the newer completion still stores its token.
func TestCopilotLoginLateFenceLeavesANewerCompletionUnfenced(t *testing.T) {
	withNumber := func(state, number string) func(client.Object) bool {
		return func(obj client.Object) bool {
			secret, ok := obj.(*corev1.Secret)
			return ok && secret.Name == copilotLoginSecretName &&
				string(secret.Data[loginState]) == state && string(secret.Data[loginNumber]) == number
		}
	}
	for _, tc := range []struct {
		name    string
		prepare func(*loginFixture)
		// accepted matches the stalled step's accepted write, and step
		// makes the request that writes it.
		accepted func(client.Object) bool
		step     func(*loginFixture)
	}{
		{
			name:     "a cancel",
			prepare:  func(f *loginFixture) { f.call(http.MethodPost, "alice") },
			accepted: withNumber("", "1"),
			step: func(f *loginFixture) {
				if w, _ := f.call(http.MethodDelete, "alice"); w.Code != http.StatusNoContent {
					f.t.Fatalf("cancel: status %d %s", w.Code, w.Body.String())
				}
			},
		},
		{
			name:     "a start",
			prepare:  func(f *loginFixture) { f.call(http.MethodPost, "alice") },
			accepted: withNumber(copilotLoginPending, "2"),
			step: func(f *loginFixture) {
				if w, _ := f.call(http.MethodPost, "alice"); w.Code != http.StatusOK {
					f.t.Fatalf("start: status %d %s", w.Code, w.Body.String())
				}
			},
		},
		{
			name: "the end of an expired completion",
			prepare: func(f *loginFixture) {
				f.call(http.MethodPost, "alice")
				f.commitCompletion()
				f.clock = f.clock.Add(copilotLoginLease + time.Second)
			},
			accepted: withNumber(copilotLoginEnding, "1"),
			step: func(f *loginFixture) {
				// The newer sign-in replaced the expired one meanwhile,
				// and is what the poll reports.
				if _, resp := f.call(http.MethodGet, "alice"); resp.State != copilotLoginPending {
					f.t.Fatalf("the newer sign-in must be reported: %+v", resp)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLoginFixture(t)
			tc.prepare(f)
			var read *corev1.Secret
			f.stallAfter(tc.accepted, func() {
				if w, _ := f.call(http.MethodPost, "alice"); w.Code != http.StatusOK {
					t.Fatalf("a newer sign-in: status %d %s", w.Code, w.Body.String())
				}
				read = f.commitCompletion()
			})
			tc.step(f)
			if read == nil {
				t.Fatal("the step did not stall")
			}
			read.Data = map[string][]byte{cetirunv1alpha1.CopilotTokenSecretKey: []byte("gho_newer")}
			if err := f.fc.Update(context.Background(), read); err != nil {
				t.Fatalf("the late fence must not fence the newer completion: %v", err)
			}
		})
	}
}

// A completion stores its token under the version of the token Secret it
// read before committing. Another write since then that neither stored a
// token nor fenced this sign-in is retried past; one that stored another
// completion's token is not overwritten.
func TestCopilotLoginTokenWriteAfterAnotherWrite(t *testing.T) {
	committed := func(obj client.Object) bool {
		secret, ok := obj.(*corev1.Secret)
		return ok && secret.Name == copilotLoginSecretName && string(secret.Data[loginState]) == copilotLoginCompleting
	}
	for _, tc := range []struct {
		name      string
		write     func(*corev1.Secret)
		wantToken string
		wantState string
	}{
		{name: "an annotation of another writer", wantToken: "gho_alice", wantState: copilotLoginSucceeded,
			write: func(secret *corev1.Secret) { secret.Annotations = map[string]string{"example.com/note": "touched"} }},
		{name: "another completion's token", wantToken: "gho_other", wantState: copilotLoginFailed,
			write: func(secret *corev1.Secret) {
				secret.Data = map[string][]byte{cetirunv1alpha1.CopilotTokenSecretKey: []byte("gho_other")}
				secret.Annotations = map[string]string{cetirunv1alpha1.AnnotationCopilotSignInGeneration: "other"}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLoginFixture(t)
			f.call(http.MethodPost, "alice")
			f.gh.answers = []map[string]any{{"access_token": "gho_alice"}}
			f.stallAfter(committed, func() {
				token := f.secret(cetirunv1alpha1.CopilotTokenSecretName)
				tc.write(token)
				if err := f.fc.Update(context.Background(), token); err != nil {
					t.Fatal(err)
				}
			})
			f.clock = f.clock.Add(6 * time.Second)
			if _, resp := f.call(http.MethodGet, "alice"); resp.State != tc.wantState {
				t.Fatalf("state %+v, want %q", resp, tc.wantState)
			}
			if token := string(f.secret(cetirunv1alpha1.CopilotTokenSecretName).Data[cetirunv1alpha1.CopilotTokenSecretKey]); token != tc.wantToken {
				t.Fatalf("stored token %q, want %q", token, tc.wantToken)
			}
		})
	}
}

// A start numbers its sign-in past the token Secret's floor too, so a token
// Secret that kept the floor of earlier sign-ins does not fence new ones.
func TestCopilotLoginNumbersContinuePastTheFloor(t *testing.T) {
	token := copilotTokenSecret("uid-1")
	token.Annotations = map[string]string{copilotSignInFloorAnnotation: "5"}
	f := newLoginFixture(t, routedAgent(), token)
	if w, _ := f.call(http.MethodPost, "alice"); w.Code != http.StatusOK {
		t.Fatalf("start: status %d %s", w.Code, w.Body.String())
	}
	if number := string(f.secret(copilotLoginSecretName).Data[loginNumber]); number != "6" {
		t.Fatalf("sign-in number %q, want 6", number)
	}
	f.gh.answers = []map[string]any{{"access_token": "gho_alice"}}
	f.clock = f.clock.Add(6 * time.Second)
	if _, resp := f.call(http.MethodGet, "alice"); resp.State != copilotLoginSucceeded {
		t.Fatalf("the sign-in must complete: %+v", resp)
	}
	if floor := f.secret(cetirunv1alpha1.CopilotTokenSecretName).Annotations[copilotSignInFloorAnnotation]; floor != "6" {
		t.Fatalf("floor %q, want 6", floor)
	}
}

// A page left open while its Agent was deleted and created again under the
// same name names the old Agent's UID: it must not start or cancel the new
// Agent's sign-in.
func TestCopilotLoginChangesOnlyTheAgentItWasMadeFor(t *testing.T) {
	f := newLoginFixture(t)
	errorCodeOf := func(w *httptest.ResponseRecorder) string {
		t.Helper()
		var resp ErrorResponse
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		return resp.Error.Code
	}
	for _, tc := range []struct {
		name, body string
		headers    map[string]string
		status     int
		code       string
	}{
		{name: "no body", status: http.StatusBadRequest, code: "invalid_body"},
		{name: "no UID", body: `{}`, status: http.StatusPreconditionRequired, code: "version_required"},
		{name: "unknown field", body: `{"uid":"uid-1","name":"alice-gw"}`, status: http.StatusBadRequest, code: "invalid_body"},
		{name: "the replaced Agent", body: `{"uid":"uid-0"}`, status: http.StatusConflict, code: "instance_changed"},
		{name: "another site", body: `{"uid":"uid-1"}`, headers: map[string]string{"Sec-Fetch-Site": "cross-site"}, status: http.StatusForbidden, code: "invalid_origin"},
	} {
		t.Run("start: "+tc.name, func(t *testing.T) {
			w, _ := f.callWith(http.MethodPost, "alice", tc.body, tc.headers)
			if w.Code != tc.status || errorCodeOf(w) != tc.code {
				t.Fatalf("status %d %s, want %d %s", w.Code, w.Body.String(), tc.status, tc.code)
			}
			if err := f.fc.Get(context.Background(), client.ObjectKey{Namespace: "cetirun-alice-gw", Name: copilotLoginSecretName}, &corev1.Secret{}); err == nil {
				t.Fatal("a refused start must not create a sign-in")
			}
		})
	}

	// The current Agent's own page starts a sign-in.
	if w, resp := f.call(http.MethodPost, "alice"); w.Code != http.StatusOK || resp.State != copilotLoginPending {
		t.Fatalf("start: status %d %s", w.Code, w.Body.String())
	}
	if w, _ := f.callWith(http.MethodDelete, "alice", `{"uid":"uid-0"}`, nil); w.Code != http.StatusConflict {
		t.Fatalf("cancel for the replaced Agent: status %d, want 409", w.Code)
	}
	if w, _ := f.callWith(http.MethodDelete, "alice", `{}`, nil); w.Code != http.StatusPreconditionRequired {
		t.Fatalf("cancel without a UID: status %d, want 428", w.Code)
	}
	if state := string(f.secret(copilotLoginSecretName).Data[loginState]); state != copilotLoginPending {
		t.Fatalf("a refused cancel must leave the sign-in pending, got %q", state)
	}
	if w, _ := f.call(http.MethodDelete, "alice"); w.Code != http.StatusNoContent {
		t.Fatalf("cancel: status %d", w.Code)
	}
	// The sign-in is cleared; its number stays for the next start.
	if data := f.secret(copilotLoginSecretName).Data; len(data) != 1 || string(data[loginNumber]) != "1" {
		t.Fatalf("cancel must clear the sign-in, got %v", data)
	}
}

// A page that still shows a sign-in pending can cancel it after the sign-in
// succeeded, for example from another tab or while the poll that completed
// it is still on its way to the browser. The sign-in is in effect: the
// cancel is refused as too late, changes nothing, and a later sign-in starts
// as usual.
func TestCopilotLoginCancelAfterItSucceededIsRefused(t *testing.T) {
	for _, agentType := range []cetirunv1alpha1.AgentType{cetirunv1alpha1.AgentTypeOpenClaw, cetirunv1alpha1.AgentTypeCustom} {
		t.Run(string(agentType), func(t *testing.T) {
			agent := routedAgent()
			agent.Spec.Agent.Type = agentType
			f := newLoginFixture(t, agent, copilotTokenSecret("uid-1"))
			f.gh.answers = []map[string]any{{"access_token": "gho_alice"}}
			if w, _ := f.call(http.MethodPost, "alice"); w.Code != http.StatusOK {
				t.Fatalf("start: status %d %s", w.Code, w.Body.String())
			}
			f.clock = f.clock.Add(6 * time.Second)
			if _, resp := f.call(http.MethodGet, "alice"); resp.State != copilotLoginSucceeded {
				t.Fatalf("poll: %+v", resp)
			}
			login := f.secret(copilotLoginSecretName)
			token := f.secret(cetirunv1alpha1.CopilotTokenSecretName)

			w, _ := f.call(http.MethodDelete, "alice")
			if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "copilot_login_completed") {
				t.Fatalf("a cancel after the sign-in succeeded: status %d %s", w.Code, w.Body.String())
			}
			if after := f.secret(copilotLoginSecretName); after.ResourceVersion != login.ResourceVersion {
				t.Fatalf("the refused cancel changed the sign-in: %v", after.Data)
			}
			if after := f.secret(cetirunv1alpha1.CopilotTokenSecretName); after.ResourceVersion != token.ResourceVersion {
				t.Fatalf("the refused cancel changed the token Secret: %v", after.Annotations)
			}
			if _, resp := f.call(http.MethodGet, "alice"); resp.State != copilotLoginSucceeded {
				t.Fatalf("after the refused cancel: %+v", resp)
			}
			if w, resp := f.call(http.MethodPost, "alice"); w.Code != http.StatusOK || resp.State != copilotLoginPending {
				t.Fatalf("a new sign-in after the refused cancel: status %d %s", w.Code, w.Body.String())
			}
		})
	}
}

// A sign-in that ends without taking effect says why in the Portal's log, for
// an administrator looking into it, without the account's token or the
// sign-in's codes.
func TestCopilotLoginLogsWhyASignInDidNotComplete(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(*loginFixture)
		wait   time.Duration
		state  string
		reason string
	}{
		{name: "account without Copilot", state: copilotLoginFailed, reason: "account_cannot_use_copilot", setup: func(f *loginFixture) {
			f.gh.answers = []map[string]any{{"access_token": "gho_alice"}}
			f.gh.exchangeCode = http.StatusNotFound
		}},
		{name: "no chat models", state: copilotLoginFailed, reason: "no_chat_models", setup: func(f *loginFixture) {
			f.gh.answers = []map[string]any{{"access_token": "gho_alice"}}
			f.gh.models = `{"data":[{"id":"text-embedding-3-small","capabilities":{"type":"embeddings"}}]}`
		}},
		{name: "denied at GitHub", state: copilotLoginDenied, reason: "denied_at_github", setup: func(f *loginFixture) {
			f.gh.answers = []map[string]any{{"error": "access_denied"}}
		}},
		{name: "expired at GitHub", state: copilotLoginExpired, reason: "code_expired", setup: func(f *loginFixture) {
			f.gh.answers = []map[string]any{{"error": "expired_token"}}
		}},
		{name: "expired before a poll", state: copilotLoginExpired, reason: "code_expired", wait: 16 * time.Minute, setup: func(*loginFixture) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })
			f := newLoginFixture(t)
			tc.setup(f)
			if w, _ := f.call(http.MethodPost, "alice"); w.Code != http.StatusOK {
				t.Fatalf("start: status %d %s", w.Code, w.Body.String())
			}
			f.clock = f.clock.Add(max(tc.wait, 6*time.Second))
			if _, resp := f.call(http.MethodGet, "alice"); resp.State != tc.state {
				t.Fatalf("poll: %+v", resp)
			}
			found := 0
			decoder := json.NewDecoder(bytes.NewReader(logs.Bytes()))
			for decoder.More() {
				var entry map[string]any
				if err := decoder.Decode(&entry); err != nil {
					t.Fatalf("decode log: %v", err)
				}
				if entry["msg"] != "GitHub Copilot sign-in did not complete" {
					continue
				}
				found++
				if entry["level"] != "INFO" || entry["instance"] != "alice-gw" || entry["reason"] != tc.reason {
					t.Errorf("log entry %v", entry)
				}
			}
			if found != 1 {
				t.Errorf("%d entries say why the sign-in did not complete, want 1: %s", found, logs.String())
			}
			for _, value := range []string{"gho_alice", "dev-secret", "WXYZ-9876", "copilot-token"} {
				if strings.Contains(logs.String(), value) {
					t.Errorf("the log contains %q", value)
				}
			}
		})
	}
}
