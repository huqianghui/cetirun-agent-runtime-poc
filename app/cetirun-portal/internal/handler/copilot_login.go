// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/auth"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/copilot"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/kube"
)

// The GitHub Copilot sign-in runs in the Portal. The Portal has more than one
// replica, so a sign-in in progress lives in a short-lived Secret in the
// Agent's namespace rather than in memory: any replica can poll it, and the
// device code never reaches the browser.
const (
	copilotLoginSecretName = "github-copilot-login"

	loginState           = "state"
	loginDeviceCode      = "device-code"
	loginUserCode        = "user-code"
	loginVerificationURI = "verification-uri"
	loginExpiresAt       = "expires-at"
	loginInterval        = "interval"
	loginNextPollAt      = "next-poll-at"
	loginMessage         = "message"
	loginCompletingUntil = "completing-until"
	// loginGeneration is the generation a completion stores its token and
	// models with. It is recorded when the completion commits, so that a
	// cancel or the end of the completion's lease can tell whether the
	// sign-in completed after all.
	loginGeneration = "generation"
	// loginEndAs and loginEndMessage hold the outcome a sign-in being ended
	// gets if it did not complete: failed with its message, or, for a
	// cancel, none.
	loginEndAs      = "end-as"
	loginEndMessage = "end-message"

	// Sign-in states reported to the browser.
	copilotLoginNone      = "none"
	copilotLoginPending   = "pending"
	copilotLoginSucceeded = "succeeded"
	copilotLoginExpired   = "expired"
	copilotLoginDenied    = "denied"
	copilotLoginFailed    = "failed"
	// copilotLoginCompleting marks a sign-in whose token is being stored. It
	// is committed before anything is written, so a cancel or a new start
	// that came first makes the commit conflict and the token is dropped;
	// once committed, it holds off both until the outcome is recorded. The
	// browser sees it as pending.
	copilotLoginCompleting = "completing"
	// copilotLoginEnding marks a committed sign-in that a cancel, the end of
	// its lease or its failed completion ends, while its outcome waits on
	// whether it completed before the floor rose past it. It keeps the
	// generation, and the browser sees it as pending: a step that stops
	// before the outcome is known leaves it to the next poll to settle,
	// rather than leaving an outcome that may be wrong.
	copilotLoginEnding = "ending"

	// copilotLoginLease bounds how long a sign-in being completed holds off
	// a new start or a cancel. Storing the token takes a few API calls; the
	// lease only matters if the replica completing it stops on the way.
	copilotLoginLease = 2 * time.Minute

	// loginNumber numbers the Agent's sign-ins from 1 up. A start takes the
	// next number, and the sign-in Secret keeps it through the outcome, so
	// the next start continues from it.
	loginNumber = "number"

	// copilotSignInFloorAnnotation on the token Secret is the lowest sign-in
	// number that may still store its token or complete. It fences
	// completions, since a lease cannot: a replica that stalls past it may
	// still resume. Once a start has replaced a sign-in, or a cancel or the
	// end of a completion whose lease ran out has ended one, the floor rises
	// past it. The floor only ever rises, so a step that resumes after a
	// newer one finds it already there and writes nothing: it cannot fence a
	// newer sign-in. A completion reads the token Secret before it commits,
	// stores the token under that version, and checks the floor again when
	// the write conflicts, before it records the models, and when it
	// records its completion, which the operator waits for. That last check
	// and an end step's raise each read the token Secret and write under the
	// version read, so the one that comes second conflicts and reads again:
	// a sign-in fenced first never completes, and an end step that comes
	// second finds the completion and reports it.
	copilotSignInFloorAnnotation = "cetirun.microsoft.com/copilot-sign-in-floor"
)

// copilotSignedInMessage reports a sign-in that completed, to a step that
// found it so rather than completed it.
const copilotSignedInMessage = "Signed in to GitHub Copilot. The models appear on the agent once its proxy has started."

// errCopilotLoginCompleting reports a sign-in that is storing its token.
var errCopilotLoginCompleting = errors.New("the GitHub Copilot sign-in is being completed")

// errCopilotLoginChanged reports a sign-in Secret that kept changing while a
// start or a cancel tried to replace it.
var errCopilotLoginChanged = errors.New("the GitHub Copilot sign-in changed meanwhile")

// errCopilotLoginSuperseded reports a completion that a newer sign-in
// replaced, or that a cancel or the end of its lease ended, before it stored
// its token or recorded its models.
var errCopilotLoginSuperseded = errors.New("the GitHub Copilot sign-in was replaced or ended meanwhile")

// CopilotLoginResponse reports a GitHub Copilot sign-in. It carries the code
// the user enters at GitHub, never the device code or a token.
type CopilotLoginResponse struct {
	State           string `json:"state"`
	UserCode        string `json:"userCode,omitempty"`
	VerificationURI string `json:"verificationUri,omitempty"`
	ExpiresAt       string `json:"expiresAt,omitempty"`
	Interval        int    `json:"interval,omitempty"`
	Message         string `json:"message,omitempty"`
}

var errCopilotProxyNotReady = errors.New("the agent's GitHub Copilot proxy is not set up yet")

func (h *InstanceHandler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *InstanceHandler) copilotClient() *copilot.Client {
	if h.Copilot != nil {
		return h.Copilot
	}
	return &copilot.Client{}
}

// copilotAgent returns the caller's Agent when it runs a GitHub Copilot proxy
// through the AI Gateway, or writes the error response. A request that
// changes the sign-in names the Agent it was made for with expectedUID, so a
// page left open while the name was deleted and created again cannot change
// the new Agent's sign-in; reads pass nil.
func (h *InstanceHandler) copilotAgent(w http.ResponseWriter, r *http.Request, expectedUID *string) (*cetirunv1alpha1.Agent, bool) {
	if !h.Config.AIGatewayEnabled {
		writeError(w, http.StatusNotFound, "feature_disabled", "The AI Gateway is not enabled for this deployment")
		return nil, false
	}
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
		return nil, false
	}
	instance, err := h.Kube.GetInstanceByName(r.Context(), r.PathValue("name"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Agent not found")
		return nil, false
	}
	// The same ownership rule as the other instance endpoints: an Agent bound
	// to a verified owner accepts only that tenant and object ID, whatever
	// alias another principal shares with it.
	if !ownsInstance(instance, claims) {
		writeError(w, http.StatusForbidden, "forbidden", "forbidden")
		return nil, false
	}
	if expectedUID != nil {
		if *expectedUID == "" {
			writeError(w, http.StatusPreconditionRequired, "version_required", "uid from the current Agent is required")
			return nil, false
		}
		if *expectedUID != string(instance.UID) {
			writeManagementError(w, kube.ErrInstanceChanged)
			return nil, false
		}
	}
	if instance.DeletionTimestamp != nil {
		writeError(w, http.StatusConflict, "agent_deleting", "The agent is being deleted")
		return nil, false
	}
	if !aiGatewayRouted(instance) || instance.Status.AIGateway.Copilot == nil {
		writeError(w, http.StatusConflict, "copilot_not_enabled", "GitHub Copilot is not enabled for this agent")
		return nil, false
	}
	return instance, true
}

// copilotLoginRequest is the JSON body that starts or cancels a sign-in.
type copilotLoginRequest struct {
	UID string `json:"uid"`
}

// copilotLoginUID reads the Agent UID a start or cancel request was made for,
// with the origin check of the other management requests, or writes the
// error response.
func (h *InstanceHandler) copilotLoginUID(w http.ResponseWriter, r *http.Request) (string, bool) {
	if !h.validManagementOrigin(w, r) {
		return "", false
	}
	var req copilotLoginRequest
	if _, ok := decodeManagementRequest(w, r, &req); !ok {
		return "", false
	}
	return req.UID, true
}

// copilotTokenSecret returns the Secret the operator created for the Agent's
// Copilot token. A Secret labelled for another Agent UID is never written.
func (h *InstanceHandler) copilotTokenSecret(ctx context.Context, instance *cetirunv1alpha1.Agent) (*corev1.Secret, error) {
	secret, err := h.Kube.GetSecret(ctx, kube.InstanceNamespace(instance.Name), cetirunv1alpha1.CopilotTokenSecretName)
	if kube.IsNotFound(err) {
		return nil, errCopilotProxyNotReady
	}
	if err != nil {
		return nil, err
	}
	if secret.Labels[kube.AgentUIDLabel] != string(instance.UID) {
		return nil, errCopilotProxyNotReady
	}
	return secret, nil
}

// copilotLoginNumberOf returns the number of the sign-in in a sign-in
// Secret's data, 0 for one an earlier Portal version wrote.
func copilotLoginNumberOf(data map[string][]byte) int64 {
	number, err := strconv.ParseInt(string(data[loginNumber]), 10, 64)
	if err != nil || number < 0 {
		return 0
	}
	return number
}

// withCopilotLoginNumber adds the sign-in's number to the data that replaces
// a sign-in Secret's, so the next start continues from it.
func withCopilotLoginNumber(data map[string][]byte, number int64) map[string][]byte {
	if number > 0 {
		data[loginNumber] = []byte(strconv.FormatInt(number, 10))
	}
	return data
}

// copilotSignInFloor returns the lowest sign-in number the token Secret
// still accepts a token or models from.
func copilotSignInFloor(secret *corev1.Secret) int64 {
	floor, err := strconv.ParseInt(secret.Annotations[copilotSignInFloorAnnotation], 10, 64)
	if err != nil || floor < 0 {
		return 0
	}
	return floor
}

// copilotSignInCompleted reports whether the token Secret records the sign-in
// of generation as completed. A sign-in without a generation never is.
func copilotSignInCompleted(secret *corev1.Secret, generation string) bool {
	return generation != "" && secret.Annotations[cetirunv1alpha1.AnnotationCopilotSignInCompleted] == generation
}

// raiseCopilotSignInFloor fences the completions of sign-ins numbered below
// floor. A floor already there or higher is left alone: the step raising it
// resumed after a newer one, and must not fence that one's completion. An
// Agent without its token Secret has no completion to fence. A step that ends
// a committed sign-in names its generation: when the Secret records that
// sign-in as completed, the step found it finished, and nothing is written.
// The check and the raise read and write the Secret at one version, so a
// completion recorded in between makes the raise conflict, and the retry
// finds it.
func (h *InstanceHandler) raiseCopilotSignInFloor(ctx context.Context, instance *cetirunv1alpha1.Agent, floor int64, generation string) (completed bool, err error) {
	for attempt := 0; attempt < 3; attempt++ {
		secret, err := h.copilotTokenSecret(ctx, instance)
		if errors.Is(err, errCopilotProxyNotReady) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if copilotSignInCompleted(secret, generation) {
			return true, nil
		}
		if copilotSignInFloor(secret) >= floor {
			return false, nil
		}
		if secret.Annotations == nil {
			secret.Annotations = map[string]string{}
		}
		secret.Annotations[copilotSignInFloorAnnotation] = strconv.FormatInt(floor, 10)
		if err := h.Kube.UpdateSecret(ctx, secret); !kube.IsConflict(err) {
			return false, err
		}
	}
	return false, errCopilotLoginChanged
}

// newCopilotSignInGeneration identifies a sign-in whose token and models are
// being stored.
func newCopilotSignInGeneration() (string, error) {
	return randomHex()
}

func randomHex() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func loginResponse(data map[string][]byte) CopilotLoginResponse {
	state := string(data[loginState])
	if state == "" {
		return CopilotLoginResponse{State: copilotLoginNone}
	}
	// The browser keeps waiting while the token is being stored, and while
	// the outcome of a sign-in being ended is settled.
	if state == copilotLoginCompleting || state == copilotLoginEnding {
		state = copilotLoginPending
	}
	resp := CopilotLoginResponse{State: state, Message: string(data[loginMessage])}
	if state == copilotLoginPending {
		resp.UserCode = string(data[loginUserCode])
		resp.VerificationURI = string(data[loginVerificationURI])
		resp.ExpiresAt = string(data[loginExpiresAt])
		resp.Interval, _ = strconv.Atoi(string(data[loginInterval]))
	}
	return resp
}

// copilotLoginBeingCompleted reports a sign-in of this Agent that committed
// its outcome and is storing its token, within its lease.
func copilotLoginBeingCompleted(secret *corev1.Secret, instance *cetirunv1alpha1.Agent, now time.Time) bool {
	if secret == nil || secret.Labels[kube.AgentUIDLabel] != string(instance.UID) ||
		string(secret.Data[loginState]) != copilotLoginCompleting {
		return false
	}
	until, err := time.Parse(time.RFC3339, string(secret.Data[loginCompletingUntil]))
	return err == nil && now.Before(until)
}

func writeCopilotLoginCompleting(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, "copilot_login_completing", "The GitHub Copilot sign-in is finishing. Try again in a moment.")
}

// logCopilotLoginNotCompleted records why a sign-in ended without taking
// effect, for an administrator looking into a failed sign-in. It names neither
// the GitHub account nor the sign-in's codes.
func logCopilotLoginNotCompleted(instance *cetirunv1alpha1.Agent, reason string) {
	slog.Info("GitHub Copilot sign-in did not complete", "instance", instance.Name, "reason", reason)
}

// writeCopilotLoginCompleted refuses a cancel of a sign-in that is in effect.
func writeCopilotLoginCompleted(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, "copilot_login_completed", "The GitHub Copilot sign-in had already finished.")
}

// writeCopilotFenceError reports a failure to fence earlier completions.
func writeCopilotFenceError(w http.ResponseWriter, instance *cetirunv1alpha1.Agent, err error, message string) {
	if errors.Is(err, errCopilotLoginChanged) {
		writeError(w, http.StatusConflict, "copilot_login_changed", "The sign-in changed meanwhile. Try again.")
		return
	}
	slog.Error("Failed to fence earlier GitHub Copilot sign-ins", "instance", instance.Name, "error", err)
	writeError(w, http.StatusInternalServerError, "internal_error", message)
}

// StartCopilotLogin handles POST /api/v1/instances/{name}/copilot/login with
// the body {"uid": "<Agent UID>"}. It starts a GitHub device-code sign-in,
// replacing one in progress, but not one that is storing its token.
func (h *InstanceHandler) StartCopilotLogin(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.copilotLoginUID(w, r)
	if !ok {
		return
	}
	instance, ok := h.copilotAgent(w, r, &uid)
	if !ok {
		return
	}
	ctx := r.Context()
	if _, err := h.copilotTokenSecret(ctx, instance); err != nil {
		if !errors.Is(err, errCopilotProxyNotReady) {
			slog.Error("Failed to read the Copilot token Secret", "instance", instance.Name, "error", err)
		}
		writeError(w, http.StatusConflict, "copilot_not_ready", "The agent's GitHub Copilot proxy is not set up yet. Try again in a minute.")
		return
	}
	// Refuse before asking GitHub for a code that could not be saved.
	if current, err := h.Kube.GetSecret(ctx, kube.InstanceNamespace(instance.Name), copilotLoginSecretName); err == nil &&
		copilotLoginBeingCompleted(current, instance, h.now()) {
		writeCopilotLoginCompleting(w)
		return
	}
	code, err := h.copilotClient().StartDeviceFlow(ctx)
	if err == nil {
		if parsed, parseErr := url.Parse(code.VerificationURI); parseErr != nil || parsed.Scheme != "https" || parsed.Host == "" {
			err = fmt.Errorf("GitHub returned an unexpected verification address")
		}
	}
	if err != nil {
		slog.Error("Failed to start the GitHub Copilot sign-in", "instance", instance.Name, "error", err)
		writeError(w, http.StatusBadGateway, "github_unavailable", "GitHub did not start the sign-in. Try again.")
		return
	}
	now := h.now().UTC()
	data := map[string][]byte{
		loginState:           []byte(copilotLoginPending),
		loginDeviceCode:      []byte(code.DeviceCode),
		loginUserCode:        []byte(code.UserCode),
		loginVerificationURI: []byte(code.VerificationURI),
		loginExpiresAt:       []byte(now.Add(time.Duration(code.ExpiresIn) * time.Second).Format(time.RFC3339)),
		loginInterval:        []byte(strconv.Itoa(code.Interval)),
		loginNextPollAt:      []byte(now.Add(time.Duration(code.Interval) * time.Second).Format(time.RFC3339)),
	}
	number, err := h.saveCopilotLogin(ctx, instance, data)
	if err != nil {
		switch {
		case errors.Is(err, errCopilotLoginCompleting):
			writeCopilotLoginCompleting(w)
		case errors.Is(err, errCopilotLoginChanged):
			writeError(w, http.StatusConflict, "copilot_login_changed", "The sign-in changed meanwhile. Try again.")
		case errors.Is(err, kube.ErrInstanceChanged):
			writeManagementError(w, err)
		case errors.Is(err, errCopilotProxyNotReady):
			writeError(w, http.StatusConflict, "copilot_not_ready", "The agent's GitHub Copilot proxy is not set up yet. Try again in a minute.")
		default:
			slog.Error("Failed to save the GitHub Copilot sign-in", "instance", instance.Name, "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to start the sign-in")
		}
		return
	}
	// Only once this sign-in has replaced the earlier ones: a completion of
	// an earlier sign-in that outlived its lease must not store its token
	// now. Fencing before the replacement was accepted would fence a
	// completion that holds its lease, and the start is then refused too.
	// An earlier sign-in that completed stays in effect until this one
	// stores its token, so the start has nothing to report of it.
	if _, err := h.raiseCopilotSignInFloor(ctx, instance, number, ""); err != nil {
		writeCopilotFenceError(w, instance, err, "Failed to start the sign-in")
		return
	}
	slog.Info("GitHub Copilot sign-in started", "instance", instance.Name)
	writeJSON(w, http.StatusOK, loginResponse(data))
}

// copilotAgentCurrent rereads the Agent and reports kube.ErrInstanceChanged
// when it is no longer the incarnation instance was read as: an Agent deleted
// and created again under the same name has another UID, and its sign-in is
// not a request for the earlier one's to change.
func (h *InstanceHandler) copilotAgentCurrent(ctx context.Context, instance *cetirunv1alpha1.Agent) error {
	current, err := h.Kube.GetInstanceByName(ctx, instance.Name)
	if kube.IsNotFound(err) {
		return kube.ErrInstanceChanged
	}
	if err != nil {
		return err
	}
	if current.UID != instance.UID || current.DeletionTimestamp != nil {
		return kube.ErrInstanceChanged
	}
	return nil
}

// saveCopilotLogin creates or replaces the sign-in Secret, with the version
// it read: it never replaces a sign-in that is storing its token, and it
// retries when a poll updated the Secret meanwhile. Between reading the
// Secret and writing it, it checks that the Agent is still the one the
// request was made for, so a start that waited on GitHub while the Agent was
// replaced changes nothing. A Secret labelled for another UID is then left
// over from an Agent that no longer exists, and is replaced. The new sign-in
// takes the number after both the replaced one's and the token Secret's
// floor, which it returns.
func (h *InstanceHandler) saveCopilotLogin(ctx context.Context, instance *cetirunv1alpha1.Agent, data map[string][]byte) (int64, error) {
	namespace := kube.InstanceNamespace(instance.Name)
	labels := map[string]string{
		"app.kubernetes.io/name":       "cetirun",
		"app.kubernetes.io/managed-by": "cetirun-portal",
		kube.AgentUIDLabel:             string(instance.UID),
	}
	for attempt := 0; attempt < 3; attempt++ {
		secret, err := h.Kube.GetSecret(ctx, namespace, copilotLoginSecretName)
		if err != nil && !kube.IsNotFound(err) {
			return 0, err
		}
		missing := err != nil
		if err := h.copilotAgentCurrent(ctx, instance); err != nil {
			return 0, err
		}
		token, err := h.copilotTokenSecret(ctx, instance)
		if err != nil {
			return 0, err
		}
		number := copilotSignInFloor(token)
		if !missing && secret.Labels[kube.AgentUIDLabel] == string(instance.UID) {
			number = max(number, copilotLoginNumberOf(secret.Data))
		}
		number++
		withCopilotLoginNumber(data, number)
		if missing {
			err = h.Kube.CreateSecret(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: copilotLoginSecretName, Namespace: namespace, Labels: labels},
				Type:       corev1.SecretTypeOpaque,
				Data:       data,
			})
			if kube.IsAlreadyExists(err) {
				continue
			}
			return number, err
		}
		if copilotLoginBeingCompleted(secret, instance, h.now()) {
			return 0, errCopilotLoginCompleting
		}
		if secret.Labels == nil {
			secret.Labels = map[string]string{}
		}
		for k, v := range labels {
			secret.Labels[k] = v
		}
		secret.Data = data
		err = h.Kube.UpdateSecret(ctx, secret)
		if kube.IsConflict(err) {
			continue
		}
		return number, err
	}
	return 0, errCopilotLoginChanged
}

// PollCopilotLogin handles GET /api/v1/instances/{name}/copilot/login. While
// a sign-in is pending it asks GitHub at most once per polling interval,
// across replicas and browser tabs; once the user has authorized, it stores
// the token for the Agent's proxy and records the account's models. It needs
// no Agent UID: it only advances a sign-in that a start for the current Agent
// created, which the Secret's Agent UID label proves.
func (h *InstanceHandler) PollCopilotLogin(w http.ResponseWriter, r *http.Request) {
	instance, ok := h.copilotAgent(w, r, nil)
	if !ok {
		return
	}
	ctx := r.Context()
	secret, err := h.Kube.GetSecret(ctx, kube.InstanceNamespace(instance.Name), copilotLoginSecretName)
	if kube.IsNotFound(err) {
		writeJSON(w, http.StatusOK, CopilotLoginResponse{State: copilotLoginNone})
		return
	}
	if err != nil {
		slog.Error("Failed to read the GitHub Copilot sign-in", "instance", instance.Name, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to read the sign-in")
		return
	}
	if secret.Labels[kube.AgentUIDLabel] != string(instance.UID) {
		writeJSON(w, http.StatusOK, CopilotLoginResponse{State: copilotLoginNone})
		return
	}
	if string(secret.Data[loginState]) == copilotLoginCompleting {
		if copilotLoginBeingCompleted(secret, instance, h.now()) {
			writeJSON(w, http.StatusOK, loginResponse(secret.Data))
			return
		}
		// The replica completing the sign-in did not report its outcome
		// within its lease.
		h.endUnfinishedCopilotLogin(ctx, w, instance, secret, "The sign-in did not finish. Start it again.")
		return
	}
	if string(secret.Data[loginState]) == copilotLoginEnding {
		// A step ending the sign-in stopped before its outcome was settled.
		h.settleCopilotLogin(ctx, w, instance, secret)
		return
	}
	if string(secret.Data[loginState]) != copilotLoginPending {
		writeJSON(w, http.StatusOK, loginResponse(secret.Data))
		return
	}
	now := h.now().UTC()
	expiresAt, err := time.Parse(time.RFC3339, string(secret.Data[loginExpiresAt]))
	if err != nil || !now.Before(expiresAt) {
		logCopilotLoginNotCompleted(instance, "code_expired")
		h.endCopilotLogin(ctx, w, instance, secret, copilotLoginExpired, "The code expired. Start the sign-in again.")
		return
	}
	if nextPoll, err := time.Parse(time.RFC3339, string(secret.Data[loginNextPollAt])); err == nil && now.Before(nextPoll) {
		writeJSON(w, http.StatusOK, loginResponse(secret.Data))
		return
	}
	// Claim this poll before calling GitHub; a replica or tab that loses the
	// race waits for the next interval instead of polling too.
	interval, _ := strconv.Atoi(string(secret.Data[loginInterval]))
	interval = max(interval, 5)
	secret.Data[loginNextPollAt] = []byte(now.Add(time.Duration(interval) * time.Second).Format(time.RFC3339))
	if err := h.Kube.UpdateSecret(ctx, secret); err != nil {
		if !kube.IsConflict(err) {
			slog.Error("Failed to update the GitHub Copilot sign-in", "instance", instance.Name, "error", err)
		}
		writeJSON(w, http.StatusOK, loginResponse(secret.Data))
		return
	}
	result, err := h.copilotClient().PollAccessToken(ctx, string(secret.Data[loginDeviceCode]))
	if err != nil {
		slog.Warn("GitHub Copilot sign-in poll failed", "instance", instance.Name, "error", err)
		writeJSON(w, http.StatusOK, loginResponse(secret.Data))
		return
	}
	switch {
	case result.AccessToken != "":
		h.finishCopilotLogin(ctx, w, instance, secret, result.AccessToken)
	case result.Error == "authorization_pending":
		writeJSON(w, http.StatusOK, loginResponse(secret.Data))
	case result.Error == "slow_down":
		if result.Interval > interval {
			interval = result.Interval
		} else {
			interval += 5
		}
		secret.Data[loginInterval] = []byte(strconv.Itoa(interval))
		secret.Data[loginNextPollAt] = []byte(now.Add(time.Duration(interval) * time.Second).Format(time.RFC3339))
		if err := h.Kube.UpdateSecret(ctx, secret); err != nil && !kube.IsConflict(err) {
			slog.Error("Failed to update the GitHub Copilot sign-in", "instance", instance.Name, "error", err)
		}
		writeJSON(w, http.StatusOK, loginResponse(secret.Data))
	case result.Error == "expired_token":
		logCopilotLoginNotCompleted(instance, "code_expired")
		h.endCopilotLogin(ctx, w, instance, secret, copilotLoginExpired, "The code expired. Start the sign-in again.")
	case result.Error == "access_denied":
		logCopilotLoginNotCompleted(instance, "denied_at_github")
		h.endCopilotLogin(ctx, w, instance, secret, copilotLoginDenied, "The sign-in was cancelled at GitHub.")
	default:
		slog.Warn("GitHub Copilot sign-in failed", "instance", instance.Name, "error", result.Error)
		h.endCopilotLogin(ctx, w, instance, secret, copilotLoginFailed, "GitHub refused the sign-in. Start it again.")
	}
}

// finishCopilotLogin checks that the account can use GitHub Copilot, commits
// the sign-in, stores the token for the proxy, records the account's models
// on the Agent, and then completes the sign-in on the token Secret. The
// commit comes first: a cancel or a new start since this poll claimed the
// sign-in makes it conflict, and the token is then dropped instead of taking
// effect. The token Secret is read before the commit and the token stored
// under that version; a start, a cancel or the end of this completion after
// its lease ran out raises the Secret's floor past this sign-in once
// accepted, and the write then conflicts and finds the floor past it, so the
// token is dropped too. A conflict that leaves the floor at or below this
// sign-in, from a step that resumed late or another writer, is retried. The
// token and the models carry the same new sign-in generation, so the
// operator never uses models with another sign-in's token. The models go on
// the Agent, which the floor does not guard: a check just before the write
// drops them once the sign-in was replaced or ended, but a replica that
// stalls after that check still writes them when it resumes. They take
// effect only through the completion, written last under the token Secret's
// version while the floor admits this sign-in: the operator uses the models
// of a completed sign-in only, and a sign-in replaced or ended first never
// completes. The annotations on the Agent are what trigger the operator; it
// waits for the completion that follows them, and looks again meanwhile.
func (h *InstanceHandler) finishCopilotLogin(ctx context.Context, w http.ResponseWriter, instance *cetirunv1alpha1.Agent, login *corev1.Secret, token string) {
	models, err := h.copilotClient().ListModels(ctx, token)
	switch {
	case errors.Is(err, copilot.ErrNoCopilot):
		logCopilotLoginNotCompleted(instance, "account_cannot_use_copilot")
		h.endCopilotLogin(ctx, w, instance, login, copilotLoginFailed, "This GitHub account cannot use GitHub Copilot.")
		return
	case err != nil:
		slog.Error("Failed to list the GitHub Copilot models", "instance", instance.Name, "error", err)
		h.endCopilotLogin(ctx, w, instance, login, copilotLoginFailed, "Signed in to GitHub, but GitHub Copilot's model list could not be read. Start the sign-in again.")
		return
	case len(models) == 0:
		logCopilotLoginNotCompleted(instance, "no_chat_models")
		h.endCopilotLogin(ctx, w, instance, login, copilotLoginFailed, "This GitHub Copilot account offers no chat models.")
		return
	}
	generation, err := newCopilotSignInGeneration()
	if err != nil {
		slog.Error("Failed to create a GitHub Copilot sign-in generation", "instance", instance.Name, "error", err)
		h.endCopilotLogin(ctx, w, instance, login, copilotLoginFailed, "Signed in to GitHub, but the token could not be stored for the agent. Start the sign-in again.")
		return
	}
	number := copilotLoginNumberOf(login.Data)
	secret, err := h.copilotTokenSecret(ctx, instance)
	if err != nil {
		slog.Error("Failed to read the GitHub Copilot token Secret", "instance", instance.Name, "error", err)
		h.endCopilotLogin(ctx, w, instance, login, copilotLoginFailed, "Signed in to GitHub, but the token could not be stored for the agent. Start the sign-in again.")
		return
	}
	if !h.commitCopilotLogin(ctx, instance, login, generation) {
		slog.Info("GitHub Copilot sign-in was cancelled or replaced while it was polled; its token is dropped", "instance", instance.Name)
		h.writeCurrentCopilotLogin(ctx, w, instance)
		return
	}
	if err := h.storeCopilotToken(ctx, instance, secret, number, token, generation); err != nil {
		if errors.Is(err, errCopilotLoginSuperseded) {
			slog.Info("GitHub Copilot sign-in was replaced or ended before its token was stored; the token is dropped", "instance", instance.Name)
			h.endCopilotLogin(ctx, w, instance, login, copilotLoginFailed, "The sign-in changed meanwhile. Start it again.")
			return
		}
		slog.Error("Failed to store the GitHub Copilot token", "instance", instance.Name, "error", err)
		h.endCopilotLogin(ctx, w, instance, login, copilotLoginFailed, "Signed in to GitHub, but the token could not be stored for the agent. Start the sign-in again.")
		return
	}
	stillStored := func(ctx context.Context) error {
		current, err := h.copilotTokenSecret(ctx, instance)
		if err != nil {
			return err
		}
		if current.Annotations[cetirunv1alpha1.AnnotationCopilotSignInGeneration] != generation || copilotSignInFloor(current) > number {
			return errCopilotLoginSuperseded
		}
		return nil
	}
	if err := h.Kube.SetCopilotModels(ctx, instance.Name, instance.UID, models, generation, stillStored); err != nil {
		if errors.Is(err, errCopilotLoginSuperseded) {
			slog.Info("GitHub Copilot sign-in was replaced or ended before its models were recorded; they are dropped", "instance", instance.Name)
			h.endCopilotLogin(ctx, w, instance, login, copilotLoginFailed, "The sign-in changed meanwhile. Start it again.")
			return
		}
		slog.Error("Failed to record the GitHub Copilot models", "instance", instance.Name, "error", err)
		h.endCopilotLogin(ctx, w, instance, login, copilotLoginFailed, "Signed in, but the model list could not be saved. Start the sign-in again.")
		return
	}
	succeeded := fmt.Sprintf("Signed in to GitHub Copilot: %d models. They appear on the agent once its proxy has started.", len(models))
	if err := h.completeCopilotSignIn(ctx, instance, number, generation); err != nil {
		if errors.Is(err, errCopilotLoginSuperseded) {
			slog.Info("GitHub Copilot sign-in was replaced or ended before it completed; its models are not used", "instance", instance.Name)
			h.endCopilotLogin(ctx, w, instance, login, copilotLoginFailed, "The sign-in changed meanwhile. Start it again.")
			return
		}
		slog.Error("Failed to complete the GitHub Copilot sign-in", "instance", instance.Name, "error", err)
		// The write may have been applied with only its response lost, and
		// the operator then uses the sign-in: read the completion back before
		// recording an outcome. While it cannot be read, the sign-in keeps its
		// commit, and the end of its lease settles the outcome from what the
		// token Secret holds then.
		token, readErr := h.copilotTokenSecret(ctx, instance)
		switch {
		case readErr != nil:
			slog.Error("Failed to read whether the GitHub Copilot sign-in completed; the end of its lease settles it", "instance", instance.Name, "error", readErr)
			writeJSON(w, http.StatusOK, loginResponse(login.Data))
		case copilotSignInCompleted(token, generation):
			slog.Info("GitHub Copilot sign-in completed although its write reported an error", "instance", instance.Name, "models", len(models))
			h.endCopilotLogin(ctx, w, instance, login, copilotLoginSucceeded, succeeded)
		default:
			h.endUnfinishedCopilotLogin(ctx, w, instance, login, "Signed in, but the sign-in could not be completed. Start it again.")
		}
		return
	}
	slog.Info("GitHub Copilot sign-in completed", "instance", instance.Name, "models", len(models))
	h.endCopilotLogin(ctx, w, instance, login, copilotLoginSucceeded, succeeded)
}

// storeCopilotToken writes the token and its generation into the token
// Secret, under the version read before the sign-in was committed, and drops
// the completion of the sign-in whose token it replaces. It reports
// errCopilotLoginSuperseded once the Secret's floor is past this sign-in, or
// once another completion has stored its token since that read. A conflict
// from anything else, such as a step that resumed late and raised the floor
// only up to this sign-in, is retried.
func (h *InstanceHandler) storeCopilotToken(ctx context.Context, instance *cetirunv1alpha1.Agent, secret *corev1.Secret, number int64, token, generation string) error {
	stored := secret.Annotations[cetirunv1alpha1.AnnotationCopilotSignInGeneration]
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			current, err := h.copilotTokenSecret(ctx, instance)
			if err != nil {
				return err
			}
			if current.Annotations[cetirunv1alpha1.AnnotationCopilotSignInGeneration] != stored {
				return errCopilotLoginSuperseded
			}
			secret = current
		}
		if copilotSignInFloor(secret) > number {
			return errCopilotLoginSuperseded
		}
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data[cetirunv1alpha1.CopilotTokenSecretKey] = []byte(token)
		if secret.Annotations == nil {
			secret.Annotations = map[string]string{}
		}
		secret.Annotations[cetirunv1alpha1.AnnotationCopilotSignInGeneration] = generation
		secret.Annotations[cetirunv1alpha1.AnnotationCopilotTokenStoredAt] = h.now().UTC().Format(time.RFC3339)
		delete(secret.Annotations, cetirunv1alpha1.AnnotationCopilotSignInCompleted)
		err := h.Kube.UpdateSecret(ctx, secret)
		if !kube.IsConflict(err) {
			return err
		}
	}
	return errCopilotLoginSuperseded
}

// completeCopilotSignIn records on the token Secret that the sign-in of
// generation completed: its token is stored and its models recorded. The
// operator uses the models of a completed sign-in only, and a cancel or the
// end of an expired completion that finds it completed reports it as such.
// It writes under the Secret's version, and only while the Secret holds this
// sign-in's token and the floor admits it: a step that fenced the sign-in
// first makes it refuse with errCopilotLoginSuperseded, and one that fences
// later conflicts with it and then finds it completed.
func (h *InstanceHandler) completeCopilotSignIn(ctx context.Context, instance *cetirunv1alpha1.Agent, number int64, generation string) error {
	for attempt := 0; attempt < 3; attempt++ {
		secret, err := h.copilotTokenSecret(ctx, instance)
		if err != nil {
			return err
		}
		if generation == "" || secret.Annotations[cetirunv1alpha1.AnnotationCopilotSignInGeneration] != generation || copilotSignInFloor(secret) > number {
			return errCopilotLoginSuperseded
		}
		secret.Annotations[cetirunv1alpha1.AnnotationCopilotSignInCompleted] = generation
		err = h.Kube.UpdateSecret(ctx, secret)
		if !kube.IsConflict(err) {
			return err
		}
	}
	return errCopilotLoginSuperseded
}

// commitCopilotLogin marks the sign-in as being completed, with the version
// the poll claimed, and records the generation the completion stores its
// token with. It reports false when the sign-in changed meanwhile.
func (h *InstanceHandler) commitCopilotLogin(ctx context.Context, instance *cetirunv1alpha1.Agent, login *corev1.Secret, generation string) bool {
	data := map[string][]byte{
		loginState:           []byte(copilotLoginCompleting),
		loginCompletingUntil: []byte(h.now().UTC().Add(copilotLoginLease).Format(time.RFC3339)),
		loginGeneration:      []byte(generation),
	}
	// The browser keeps showing the code until the outcome is recorded; the
	// device code is no longer needed.
	for _, key := range []string{loginUserCode, loginVerificationURI, loginExpiresAt, loginInterval, loginNumber} {
		if value, ok := login.Data[key]; ok {
			data[key] = value
		}
	}
	login.Data = data
	if err := h.Kube.UpdateSecret(ctx, login); err != nil {
		if !kube.IsConflict(err) && !kube.IsNotFound(err) {
			slog.Error("Failed to commit the GitHub Copilot sign-in", "instance", instance.Name, "error", err)
		}
		return false
	}
	return true
}

// writeCurrentCopilotLogin reports the sign-in as it is now stored.
func (h *InstanceHandler) writeCurrentCopilotLogin(ctx context.Context, w http.ResponseWriter, instance *cetirunv1alpha1.Agent) {
	secret, err := h.Kube.GetSecret(ctx, kube.InstanceNamespace(instance.Name), copilotLoginSecretName)
	if err != nil || secret.Labels[kube.AgentUIDLabel] != string(instance.UID) {
		writeJSON(w, http.StatusOK, CopilotLoginResponse{State: copilotLoginNone})
		return
	}
	writeJSON(w, http.StatusOK, loginResponse(secret.Data))
}

// endCopilotLogin records the outcome in place of the sign-in it was polled
// for and reports it. A sign-in cancelled or started meanwhile is left alone
// and reported as it is.
func (h *InstanceHandler) endCopilotLogin(ctx context.Context, w http.ResponseWriter, instance *cetirunv1alpha1.Agent, login *corev1.Secret, state, message string) {
	login.Data = withCopilotLoginNumber(map[string][]byte{loginState: []byte(state), loginMessage: []byte(message)}, copilotLoginNumberOf(login.Data))
	if err := h.Kube.UpdateSecret(ctx, login); err != nil {
		if kube.IsConflict(err) || kube.IsNotFound(err) {
			h.writeCurrentCopilotLogin(ctx, w, instance)
			return
		}
		slog.Error("Failed to record the GitHub Copilot sign-in outcome", "instance", instance.Name, "error", err)
	}
	writeJSON(w, http.StatusOK, loginResponse(login.Data))
}

// endUnfinishedCopilotLogin ends a committed sign-in that did not report an
// outcome: one whose replica did not report it within its lease, or whose
// completion failed without completing. It records the sign-in as ending
// first, keeping its generation, and settles the outcome only once that is
// accepted: fencing first would fence a newer sign-in's completion if the
// ending were then refused.
func (h *InstanceHandler) endUnfinishedCopilotLogin(ctx context.Context, w http.ResponseWriter, instance *cetirunv1alpha1.Agent, login *corev1.Secret, message string) {
	login.Data = endingCopilotLoginData(login.Data, copilotLoginFailed, message)
	if err := h.Kube.UpdateSecret(ctx, login); err != nil {
		if !kube.IsConflict(err) && !kube.IsNotFound(err) {
			slog.Error("Failed to record the end of a GitHub Copilot sign-in", "instance", instance.Name, "error", err)
		}
		h.writeCurrentCopilotLogin(ctx, w, instance)
		return
	}
	h.settleCopilotLogin(ctx, w, instance, login)
}

// endingCopilotLoginData is the sign-in Secret's data for ending the
// committed sign-in in data: its number, generation and code, and the outcome
// it gets if it did not complete, none when endAs is empty.
func endingCopilotLoginData(data map[string][]byte, endAs, message string) map[string][]byte {
	ending := map[string][]byte{loginState: []byte(copilotLoginEnding)}
	for _, key := range []string{loginUserCode, loginVerificationURI, loginExpiresAt, loginInterval, loginNumber, loginGeneration} {
		if value, ok := data[key]; ok {
			ending[key] = value
		}
	}
	if endAs != "" {
		ending[loginEndAs] = []byte(endAs)
		ending[loginEndMessage] = []byte(message)
	}
	return ending
}

// settleEndingCopilotLogin settles the outcome of a sign-in being ended. It
// raises the floor past the sign-in, or finds that it completed first: the
// check and the raise are one versioned write of the token Secret, so the
// outcome is known once that returns. Only then does it publish the outcome,
// succeeded for a sign-in that completed, else the one it is ended with, in a
// versioned write of the sign-in Secret; errCopilotLoginChanged reports that
// another step changed the sign-in first. On any error the sign-in stays
// ending, keeping its generation, for the next poll to settle.
func (h *InstanceHandler) settleEndingCopilotLogin(ctx context.Context, instance *cetirunv1alpha1.Agent, login *corev1.Secret) (completed bool, err error) {
	number := copilotLoginNumberOf(login.Data)
	completed, err = h.raiseCopilotSignInFloor(ctx, instance, number+1, string(login.Data[loginGeneration]))
	if err != nil {
		return false, err
	}
	data := map[string][]byte{}
	switch {
	case completed:
		data[loginState], data[loginMessage] = []byte(copilotLoginSucceeded), []byte(copilotSignedInMessage)
	case len(login.Data[loginEndAs]) > 0:
		data[loginState], data[loginMessage] = login.Data[loginEndAs], login.Data[loginEndMessage]
	}
	published := login.DeepCopy()
	published.Data = withCopilotLoginNumber(data, number)
	if err := h.Kube.UpdateSecret(ctx, published); err != nil {
		if kube.IsConflict(err) || kube.IsNotFound(err) {
			return completed, errCopilotLoginChanged
		}
		return completed, err
	}
	*login = *published
	return completed, nil
}

// settleCopilotLogin settles the outcome of a sign-in being ended and reports
// it. While it cannot be settled, the sign-in stays ending and is reported as
// pending, and a later poll settles it.
func (h *InstanceHandler) settleCopilotLogin(ctx context.Context, w http.ResponseWriter, instance *cetirunv1alpha1.Agent, login *corev1.Secret) {
	if _, err := h.settleEndingCopilotLogin(ctx, instance, login); err != nil {
		if errors.Is(err, errCopilotLoginChanged) {
			h.writeCurrentCopilotLogin(ctx, w, instance)
			return
		}
		slog.Error("Failed to settle the outcome of a GitHub Copilot sign-in; a later poll settles it", "instance", instance.Name, "error", err)
	}
	writeJSON(w, http.StatusOK, loginResponse(login.Data))
}

// CancelCopilotLogin handles DELETE /api/v1/instances/{name}/copilot/login
// with the body {"uid": "<Agent UID>"}. A sign-in that is storing its token
// is not cancelled. A committed sign-in past its lease is ended rather than
// cleared, and one that completed first is reported with
// copilot_login_completed; while that cannot be settled, the sign-in stays
// ending for a poll to settle. A sign-in that has succeeded is in effect, and
// a cancel that comes after it, from a page that still showed it pending, is
// reported with copilot_login_completed too. Between reading the sign-in
// Secret and changing it, the cancel checks that the Agent is still the one
// it was made for; a Secret labelled for another UID, left over from an Agent
// that no longer exists, is not this Agent's sign-in and is left alone.
func (h *InstanceHandler) CancelCopilotLogin(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.copilotLoginUID(w, r)
	if !ok {
		return
	}
	instance, ok := h.copilotAgent(w, r, &uid)
	if !ok {
		return
	}
	ctx := r.Context()
	namespace := kube.InstanceNamespace(instance.Name)
	for attempt := 0; attempt < 3; attempt++ {
		secret, err := h.Kube.GetSecret(ctx, namespace, copilotLoginSecretName)
		if kube.IsNotFound(err) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err != nil {
			slog.Error("Failed to read the GitHub Copilot sign-in", "instance", instance.Name, "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to cancel the sign-in")
			return
		}
		if err := h.copilotAgentCurrent(ctx, instance); err != nil {
			writeManagementError(w, err)
			return
		}
		if secret.Labels[kube.AgentUIDLabel] != string(instance.UID) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if copilotLoginBeingCompleted(secret, instance, h.now()) {
			writeCopilotLoginCompleting(w)
			return
		}
		// Clearing a succeeded sign-in would not undo it: its token and
		// models are in effect. Reporting the cancel as done would tell the
		// page that it was cancelled.
		if string(secret.Data[loginState]) == copilotLoginSucceeded {
			writeCopilotLoginCompleted(w)
			return
		}
		// With the version just read: a poll that claimed the sign-in
		// meanwhile makes this conflict, and the cancel is retried. The
		// number stays for the next start. A committed sign-in, whose
		// completion may have finished, is ended rather than cleared: its
		// outcome is published only once settled.
		number := copilotLoginNumberOf(secret.Data)
		committed := len(secret.Data[loginGeneration]) > 0
		if committed {
			secret.Data = endingCopilotLoginData(secret.Data, "", "")
		} else {
			secret.Data = withCopilotLoginNumber(map[string][]byte{}, number)
		}
		err = h.Kube.UpdateSecret(ctx, secret)
		if kube.IsConflict(err) {
			continue
		}
		if kube.IsNotFound(err) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err != nil {
			slog.Error("Failed to cancel the GitHub Copilot sign-in", "instance", instance.Name, "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to cancel the sign-in")
			return
		}
		// Only once the cancel is accepted: a completion that outlived its
		// lease must not store its token, record its models or complete
		// after it. Fencing before the cancel was accepted would fence a
		// completion that holds its lease, and the cancel is then refused
		// too. Should this cancel resume only after a newer sign-in started,
		// the floor is already past the cancelled one and stays as it is. A
		// sign-in that completed before the floor rose is in effect: the
		// cancel came too late, and that is published and reported instead.
		if !committed {
			if _, err := h.raiseCopilotSignInFloor(ctx, instance, number+1, ""); err != nil {
				writeCopilotFenceError(w, instance, err, "Failed to cancel the sign-in")
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		completed, err := h.settleEndingCopilotLogin(ctx, instance, secret)
		switch {
		case err != nil:
			// The sign-in stays ending, and a poll settles it.
			writeCopilotFenceError(w, instance, err, "Failed to cancel the sign-in")
		case completed:
			writeCopilotLoginCompleted(w)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
		return
	}
	writeError(w, http.StatusConflict, "copilot_login_changed", "The sign-in changed meanwhile. Try again.")
}
