// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxfence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

const (
	fixtureSubscription = "00000000-0000-4000-8000-000000000001"
	fixtureTenant       = "00000000-0000-4000-8000-000000000002"
	fixtureRun          = "00000000-0000-4000-8000-000000000003"
	fixtureVMID         = "00000000-0000-4000-8000-000000000004"
	fixtureReplacement  = "00000000-0000-4000-8000-000000000005"
	fixtureCluster      = "/subscriptions/" + fixtureSubscription + "/resourceGroups/qualification/providers/Microsoft.ContainerService/managedClusters/qualification"
	fixturePool         = fixtureCluster + "/agentPools/pilot"
	fixtureVM           = "/subscriptions/" + fixtureSubscription + "/resourceGroups/qualification-nodes/providers/Microsoft.Compute/virtualMachineScaleSets/pilot-vmss/virtualMachines/0"
	fixtureOperation    = "/subscriptions/" + fixtureSubscription + "/providers/Microsoft.ContainerService/locations/westus3/operationresults/00000000-0000-4000-8000-000000000006"
)

type fixtureCredential struct{}

func (fixtureCredential) GetToken(_ context.Context, options policy.TokenRequestOptions) (azcore.AccessToken, error) {
	if !slices.Equal(options.Scopes, []string{"https://management.azure.com/.default"}) {
		return azcore.AccessToken{}, errors.New("SDK requested the wrong ARM audience")
	}
	return azcore.AccessToken{Token: "synthetic-arm-token", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

type wireRequest struct {
	Method, Path, IfMatch, RequestID string
}

type armFixture struct {
	t                   *testing.T
	mu                  sync.Mutex
	server              *httptest.Server
	client              *armClient
	dir                 string
	poolExists          bool
	vmExists            bool
	bareNotFound        bool
	missingPoolResponse *response
	etag                string
	mode                string
	vmID                string
	clusterUID          string
	runTag              string
	pollKind            string
	pollStatus          string
	pollHTTP            int
	monitorVersion      string
	monitorQuery        string
	polledQuery         string
	deleteHTTP          int
	largeDelete         bool
	keepVM              bool
	replaceVM           bool
	ignoreIfMatch       bool
	loseDelete          bool
	badPollURL          bool
	challenge           bool
	duplicateVM         bool
	extraUserPool       bool
	readFailure         string
	failAfterProbe      bool
	probeRejected       bool
	beforeDelete        func(*armFixture)
	parentSync          func(string) error
	requests            []wireRequest
}

func newARMFixture(t *testing.T) *armFixture {
	t.Helper()
	f := &armFixture{t: t, dir: filepath.Join(t.TempDir(), "journal"), etag: `"pool-version-1"`,
		mode: "User", vmID: fixtureVMID, clusterUID: "synthetic-cluster-incarnation",
		runTag: fixtureRun, pollKind: "location", pollStatus: "Succeeded"}
	f.server = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	var err error
	f.client, err = makeARMClient(fixtureCredential{}, f.server.URL, f.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *armFixture) reservation() Reservation {
	return Reservation{TenantID: fixtureTenant, PoolID: fixturePool, ClusterResourceUID: "synthetic-cluster-incarnation", RunID: fixtureRun}
}

func (f *armFixture) invoke(ctx context.Context, action Action, confirm bool) (*Journal, error) {
	f.t.Helper()
	options := Options{Action: action, JournalDirectory: f.dir}
	if action == Reserve {
		options.Reservation = f.reservation()
	}
	if confirm {
		options.ConfirmDeletePool = fixturePool
	}
	syncParent := syncJournalParent
	if f.parentSync != nil {
		syncParent = f.parentSync
	}
	return run(ctx, options, func(Reservation) (*armClient, error) { return f.client, nil }, syncParent)
}

func (f *armFixture) prepare(qualified bool) *Journal {
	f.t.Helper()
	if _, err := f.invoke(context.Background(), Reserve, false); err != nil {
		f.t.Fatal(err)
	}
	f.mu.Lock()
	f.poolExists, f.vmExists = true, true
	f.mu.Unlock()
	journal, err := f.invoke(context.Background(), Register, false)
	if err != nil {
		f.t.Fatal(err)
	}
	if qualified {
		journal, err = f.invoke(context.Background(), TestCondition, true)
		if err != nil {
			f.t.Fatal(err)
		}
	}
	return journal
}

func (f *armFixture) mutations() []wireRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []wireRequest
	for _, request := range f.requests {
		if request.Method != http.MethodGet {
			result = append(result, request)
		}
	}
	return result
}

func (f *armFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, wireRequest{r.Method, r.URL.Path, r.Header.Get("If-Match"), r.Header.Get("x-ms-client-request-id")})
	version := APIVersion
	if r.URL.Path == fixtureVM {
		version = ComputeAPIVersion
	}
	if r.URL.Path == fixtureOperation && f.monitorVersion != "" {
		version = f.monitorVersion
	}
	if r.URL.Query().Get("api-version") != version {
		f.t.Errorf("unexpected API version for %s", r.URL.Path)
		f.fail(w, 400, "InvalidAPIVersion")
		return
	}
	if r.Header.Get("Authorization") != "Bearer synthetic-arm-token" {
		f.t.Error("SDK did not use the synthetic credential")
		f.fail(w, 401, "Unauthorized")
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == f.readFailure {
		f.fail(w, 503, "Unavailable")
		return
	}
	if f.failAfterProbe && f.probeRejected && r.Method == http.MethodGet && r.URL.Path == fixturePool {
		f.failAfterProbe = false
		f.fail(w, 503, "Unavailable")
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == fixtureCluster:
		profiles := []map[string]string{{"name": "system", "mode": "System"}}
		if f.poolExists {
			profiles = append(profiles, map[string]string{"name": "pilot", "mode": f.mode})
		}
		if f.extraUserPool {
			profiles = append(profiles, map[string]string{"name": "unrelated", "mode": "User"})
		}
		f.json(w, 200, map[string]any{"id": fixtureCluster, "properties": map[string]any{
			"resourceUID": f.clusterUID, "nodeResourceGroup": "qualification-nodes", "provisioningState": "Succeeded",
			"agentPoolProfiles": profiles,
		}})
	case r.Method == http.MethodGet && r.URL.Path == fixturePool:
		if !f.poolExists {
			if f.missingPoolResponse != nil {
				w.WriteHeader(f.missingPoolResponse.status)
				_, _ = w.Write(f.missingPoolResponse.body)
				return
			}
			f.notFound(w)
			return
		}
		f.json(w, 200, map[string]any{"id": fixturePool, "name": "pilot", "properties": map[string]any{
			"eTag": f.etag, "mode": f.mode, "type": "VirtualMachineScaleSets", "count": 1,
			"enableAutoScaling": false, "provisioningState": "Succeeded", "tags": map[string]string{RunTag: f.runTag},
		}})
	case r.Method == http.MethodGet && r.URL.Path == fixturePool+"/machines":
		machines := []map[string]any{{"name": "pilot-vmss000000", "properties": map[string]string{"resourceId": fixtureVM}}}
		if f.duplicateVM {
			machines = append(machines, machines[0])
		}
		f.json(w, 200, map[string]any{"value": machines})
	case r.Method == http.MethodGet && r.URL.Path == fixtureVM:
		if !f.vmExists {
			f.notFound(w)
			return
		}
		f.json(w, 200, map[string]any{"id": fixtureVM, "properties": map[string]any{
			"vmId": f.vmID, "instanceView": map[string]any{"statuses": []map[string]string{{"code": "PowerState/deallocated"}}},
		}})
	case r.Method == http.MethodDelete && r.URL.Path == fixturePool:
		f.checkDurableIntent(r)
		if f.beforeDelete != nil {
			f.beforeDelete(f)
		}
		if r.Header.Get("If-Match") != f.etag && !f.ignoreIfMatch {
			f.probeRejected = true
			f.fail(w, 412, "PreconditionFailed")
			return
		}
		if f.largeDelete {
			status := f.deleteHTTP
			if status == 0 {
				status = http.StatusAccepted
				w.Header().Set("Location", f.server.URL+fixtureOperation+"?api-version="+APIVersion)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"padding":"`+strings.Repeat("x", maxResponseBytes)+`"}`)
			return
		}
		if f.deleteHTTP != 0 {
			if f.challenge {
				w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_claims", claims="e30="`)
			}
			f.fail(w, f.deleteHTTP, "SyntheticFailure")
			return
		}
		if f.loseDelete {
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				f.t.Error(err)
				return
			}
			connection.Close()
			return
		}
		uri := f.server.URL + fixtureOperation + "?api-version=" + APIVersion
		if f.monitorVersion != "" {
			uri = f.server.URL + fixtureOperation + "?api-version=" + f.monitorVersion + f.monitorQuery
		}
		if f.badPollURL {
			uri = "https://example.com/untrusted-operation"
		}
		if f.pollKind == "azure-async-operation" {
			w.Header().Set("Azure-AsyncOperation", uri)
		} else {
			w.Header().Set("Location", uri)
		}
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodGet && r.URL.Path == fixtureOperation:
		f.polledQuery = r.URL.RawQuery
		if f.pollHTTP != 0 {
			f.fail(w, f.pollHTTP, "OperationUnavailable")
			return
		}
		if f.pollStatus == "Succeeded" {
			f.poolExists = false
			f.vmExists = f.keepVM || f.replaceVM
			if f.replaceVM {
				f.vmID = fixtureReplacement
			}
		}
		if f.pollKind == "location" && f.pollStatus == "Succeeded" {
			w.WriteHeader(http.StatusNoContent)
		} else if f.pollKind == "location" && f.pollStatus == "InProgress" {
			w.WriteHeader(http.StatusAccepted)
		} else {
			f.json(w, 200, map[string]string{"status": f.pollStatus})
		}
	default:
		f.t.Errorf("unexpected ARM request: %s %s", r.Method, r.URL.Path)
		f.fail(w, 400, "UnexpectedRequest")
	}
}

func (f *armFixture) checkDurableIntent(r *http.Request) {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, "journal.json"))
	if err != nil {
		f.t.Errorf("DELETE happened without readable durable intent: %v", err)
		return
	}
	var journal Journal
	if err := json.Unmarshal(data, &journal); err != nil {
		f.t.Error(err)
		return
	}
	attempt := journal.Probe
	if journal.Phase == DeletePending {
		attempt = journal.Deletion
	} else if journal.Phase != ProbePending {
		f.t.Errorf("DELETE sent from unsafe persisted phase %q", journal.Phase)
	}
	if attempt == nil || attempt.RequestID != r.Header.Get("x-ms-client-request-id") ||
		attempt.IfMatch != r.Header.Get("If-Match") || attempt.IntentAt.IsZero() || journal.Completion != nil {
		f.t.Error("DELETE request was not bound to its previously persisted intent")
	}
}

func (f *armFixture) json(w http.ResponseWriter, status int, value any) {
	f.t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if status == http.StatusNoContent {
		return
	}
	if err := json.NewEncoder(w).Encode(value); err != nil {
		f.t.Error(err)
	}
}

func (f *armFixture) fail(w http.ResponseWriter, status int, code string) {
	f.json(w, status, map[string]any{"error": map[string]string{"code": code}})
}

func TestConditionalPoolDeletionAtHTTPBoundary(t *testing.T) {
	for _, kind := range []string{"location", "azure-async-operation"} {
		t.Run(kind, func(t *testing.T) {
			f := newARMFixture(t)
			f.pollKind = kind
			f.etag = "00000000-0000-4000-8000-000000000007"
			journal := f.prepare(true)
			if journal.Phase != Qualified || journal.ETag != f.etag {
				t.Fatalf("registration was not pinned: %+v", journal)
			}
			journal, err := f.invoke(context.Background(), Delete, true)
			if !errors.Is(err, ErrPending) || journal.Phase != Deleting || journal.Completion != nil {
				t.Fatalf("accepted submission was mistaken for completion: %v, %+v", err, journal)
			}
			journal, err = f.invoke(context.Background(), Observe, false)
			if err != nil || journal.Phase != Complete || journal.Completion == nil ||
				!slices.Equal(journal.Completion.AbsentVMs, journal.Machines) {
				t.Fatalf("operation and VM destruction were not observed: %v, %+v", err, journal)
			}
			mutations := f.mutations()
			if len(mutations) != 2 || mutations[0].IfMatch == f.etag || mutations[1].IfMatch != f.etag ||
				mutations[0].Path != fixturePool || mutations[1].Path != fixturePool ||
				mutations[0].RequestID == mutations[1].RequestID {
				t.Fatalf("unexpected destructive wire requests: %+v", mutations)
			}
			for _, request := range f.requests {
				if request.Method == http.MethodDelete && strings.Contains(request.Path, "Microsoft.Compute") {
					t.Fatal("unsupported Compute mutation")
				}
			}
			info, err := os.Stat(filepath.Join(f.dir, "journal.json"))
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("journal is not private: %v", err)
			}
		})
	}
}

func TestExistingPoolCannotBeAdopted(t *testing.T) {
	f := newARMFixture(t)
	f.poolExists, f.vmExists = true, true
	if _, err := f.invoke(context.Background(), Reserve, false); err == nil {
		t.Fatal("existing pool adopted")
	}

	if len(f.mutations()) != 0 {
		t.Fatal("reservation sent a destructive request")
	}
	if _, err := os.Stat(filepath.Join(f.dir, "journal.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed reservation created an ownership journal")
	}
}

func (f *armFixture) notFound(w http.ResponseWriter) {
	if f.bareNotFound {
		f.json(w, 404, map[string]any{"code": "NotFound", "message": "Synthetic resource absent",
			"details": []map[string]string{{"code": "Unspecified"}}})
		return
	}
	f.fail(w, 404, "ResourceNotFound")
}

func TestBareAKSAbsenceAtHTTPBoundary(t *testing.T) {
	f := newARMFixture(t)
	f.bareNotFound = true
	f.etag = "00000000-0000-4000-8000-000000000007"
	journal := f.prepare(true)
	if journal.Phase != Qualified {
		t.Fatal("bare AKS absence did not permit reservation and conditional verification")
	}
	journal, err := f.invoke(context.Background(), Delete, true)
	if !errors.Is(err, ErrPending) || journal.Phase != Deleting {
		t.Fatalf("DELETE acceptance was not kept separate from completion: %v", err)
	}
	journal, err = f.invoke(context.Background(), Observe, false)
	if err != nil || journal.Phase != Complete || journal.Completion == nil {
		t.Fatalf("bare AKS/Compute absence was not accepted after successful operation: %v", err)
	}
	if len(f.mutations()) != 2 {
		t.Fatal("absence handling must not add mutation attempts")
	}
}

func TestOpaqueOperationHandlePreservedAcrossObservationRecovery(t *testing.T) {
	for _, kind := range []string{"azure-async-operation", "location"} {
		t.Run(kind, func(t *testing.T) {
			f := newARMFixture(t)
			f.pollKind, f.monitorVersion = kind, "2025-03-01"
			f.monitorQuery = "&c=synthetic-correlation&h=synthetic%2Bsignature%2Fcursor%3D&s=synthetic-scope&t=synthetic-time"
			f.prepare(true)
			journal, err := f.invoke(context.Background(), Delete, true)
			if !errors.Is(err, ErrPending) || journal.Phase != Deleting {
				t.Fatalf("independent monitor version was not accepted: %v", err)
			}
			wantQuery := "api-version=" + f.monitorVersion + f.monitorQuery
			if journal.Deletion.OperationURL != f.server.URL+fixtureOperation+"?"+wantQuery {
				t.Fatal("provider operation handle was rewritten")
			}
			// Reproduce a prior client's durable 202 receipt without its final
			// phase transition. Recovery may observe, never resubmit DELETE.
			store, err := openJournal(f.dir, false)
			if err != nil {
				t.Fatal(err)
			}
			journal.Phase = DeletePending
			err = store.save(journal)
			err = errors.Join(err, store.close())
			if err != nil {
				t.Fatal(err)
			}
			journal, err = f.invoke(context.Background(), Observe, false)
			if err != nil || journal.Phase != Complete || f.polledQuery != wantQuery || len(f.mutations()) != 2 {
				t.Fatalf("observation did not safely recover the recorded handle: phase=%s error=%v", journal.Phase, err)
			}
		})
	}
}

func TestObservationRecoveryRequiresRecordedAcceptedHandle(t *testing.T) {
	for _, change := range []struct {
		name   string
		mutate func(*Journal)
	}{
		{"no HTTP response", func(j *Journal) { j.Deletion.HTTPStatus = 0 }},
		{"no operation handle", func(j *Journal) { j.Deletion.OperationURL = "" }},
		{"unknown poll kind", func(j *Journal) { j.Deletion.PollKind = "unknown" }},
		{"foreign monitor", func(j *Journal) { j.Deletion.OperationURL = "https://example.com/untrusted" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			f := newARMFixture(t)
			f.prepare(true)
			journal, err := f.invoke(context.Background(), Delete, true)
			if !errors.Is(err, ErrPending) {
				t.Fatal(err)
			}
			store, err := openJournal(f.dir, false)
			if err != nil {
				t.Fatal(err)
			}
			journal.Phase = DeletePending
			change.mutate(journal)
			err = store.save(journal)
			err = errors.Join(err, store.close())
			if err != nil {
				t.Fatal(err)
			}
			journal, err = f.invoke(context.Background(), Observe, false)
			if err == nil || journal.Phase != DeletePending || journal.Completion != nil ||
				f.polledQuery != "" || len(f.mutations()) != 2 {
				t.Fatalf("uncertain receipt authorized recovery or mutation: %v", err)
			}
		})
	}
}

func TestUntrustedAbsenceCannotReserveAtHTTPBoundary(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{"HTML", 404, `<html>Not found</html>`},
		{"empty", 404, ``},
		{"malformed", 404, `{"code":`},
		{"bare authorization error", 404, `{"code":"AuthorizationFailed"}`},
		{"enveloped authorization error", 404, `{"error":{"code":"Forbidden"}}`},
		{"authentication status", 401, `{"code":"NotFound"}`},
		{"authorization status", 403, `{"code":"NotFound"}`},
		{"success status", 200, `{"code":"NotFound"}`},
		{"ambiguous shapes", 404, `{"code":"NotFound","error":{"code":"NotFound"}}`},
		{"duplicate bare code", 404, `{"code":"Forbidden","code":"NotFound"}`},
		{"duplicate nested code", 404, `{"error":{"code":"Forbidden","code":"NotFound"}}`},
		{"trailing document", 404, `{"code":"NotFound"}{"code":"Forbidden"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newARMFixture(t)
			f.missingPoolResponse = &response{status: test.status, body: []byte(test.body)}
			journal, err := f.invoke(context.Background(), Reserve, false)
			if err == nil || journal != nil || len(f.mutations()) != 0 {
				t.Fatalf("untrusted absence became a reservation: %v", err)
			}
			if _, err := os.Stat(filepath.Join(f.dir, "journal.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("untrusted absence persisted an ownership journal")
			}
		})
	}
}

func TestBareAbsenceStillRequiresRegisteredClusterIncarnation(t *testing.T) {
	f := newARMFixture(t)
	f.bareNotFound = true
	f.clusterUID = "different-synthetic-cluster-incarnation"
	if journal, err := f.invoke(context.Background(), Reserve, false); err == nil || journal != nil || len(f.mutations()) != 0 {
		t.Fatal("bare absence bypassed the registered cluster incarnation")
	}
}

func TestUnsafeTargetsFailBeforeDeletion(t *testing.T) {
	for _, change := range []struct {
		name string
		run  func(*armFixture)
	}{
		{"pool ETag changed", func(f *armFixture) { f.etag = `"replacement-pool"` }},
		{"cluster reused", func(f *armFixture) { f.clusterUID = "replacement-cluster" }},
		{"VM identity swapped", func(f *armFixture) { f.vmID = fixtureReplacement }},
		{"system pool", func(f *armFixture) { f.mode = "System" }},
		{"shared cluster", func(f *armFixture) { f.extraUserPool = true }},
		{"wrong ownership run", func(f *armFixture) { f.runTag = fixtureReplacement }},
		{"ambiguous machine", func(f *armFixture) { f.duplicateVM = true }},
		{"VM missing", func(f *armFixture) { f.vmExists = false }},
		{"VM read error", func(f *armFixture) { f.readFailure = fixtureVM }},
		{"pool read error", func(f *armFixture) { f.readFailure = fixturePool }},
		{"missing ETag", func(f *armFixture) { f.etag = "" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			f := newARMFixture(t)
			f.prepare(true)
			f.mu.Lock()
			change.run(f)
			f.mu.Unlock()
			journal, err := f.invoke(context.Background(), Delete, true)
			if err == nil || journal.Completion != nil || len(f.mutations()) != 1 {
				t.Fatalf("unsafe target caused deletion: %v, %+v", err, journal)
			}
		})
	}
}

func TestSwapAfterLastReadIsConditionallyRejected(t *testing.T) {
	f := newARMFixture(t)
	f.prepare(true)
	f.beforeDelete = func(f *armFixture) { f.etag = `"replacement-after-read"` }
	journal, err := f.invoke(context.Background(), Delete, true)
	if err == nil || journal.Phase != Failed || journal.Deletion.HTTPStatus != 412 || !f.poolExists {
		t.Fatalf("replacement was not preserved: %v, %+v", err, journal)
	}
	_, _ = f.invoke(context.Background(), Delete, true)
	if len(f.mutations()) != 2 {
		t.Fatal("conditional failure was retried")
	}
}

func TestDestructionRequiresExplicitConfirmation(t *testing.T) {
	f := newARMFixture(t)
	f.prepare(false)
	if _, err := f.invoke(context.Background(), TestCondition, false); err == nil || len(f.mutations()) != 0 {
		t.Fatal("probe ignored missing destruction confirmation")
	}
	if _, err := f.invoke(context.Background(), TestCondition, true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.invoke(context.Background(), Delete, false); err == nil || len(f.mutations()) != 1 {
		t.Fatal("delete ignored missing destruction confirmation")
	}
}

func TestConditionProbeFailsClosedWhenNotEnforced(t *testing.T) {
	f := newARMFixture(t)
	f.prepare(false)
	f.ignoreIfMatch = true
	journal, err := f.invoke(context.Background(), TestCondition, true)
	if err == nil || journal.Phase != ProbeUnexpected || journal.Completion != nil {
		t.Fatalf("accepted bad condition qualified deletion: %v, %+v", err, journal)
	}
	_, _ = f.invoke(context.Background(), Delete, true)
	_, _ = f.invoke(context.Background(), TestCondition, true)
	_, _ = f.invoke(context.Background(), Observe, false)
	if len(f.mutations()) != 1 {
		t.Fatal("broken condition probe was replayed")
	}
}

func TestRejectedProbeReadFailureCanResumeWithoutMutation(t *testing.T) {
	f := newARMFixture(t)
	f.prepare(false)
	f.failAfterProbe = true
	journal, err := f.invoke(context.Background(), TestCondition, true)
	if err == nil || journal.Phase != ProbeRejected {
		t.Fatalf("read failure was silently accepted: %v, %+v", err, journal)
	}
	journal, err = f.invoke(context.Background(), Observe, false)
	if err != nil || journal.Phase != Qualified || len(f.mutations()) != 1 {
		t.Fatalf("probe observation was not safely resumed: %v, %+v", err, journal)
	}
}

func TestUncertainSubmissionIsNeverReplayed(t *testing.T) {
	for _, change := range []struct {
		name string
		run  func(*armFixture)
	}{
		{"lost response", func(f *armFixture) { f.loseDelete = true }},
		{"server error", func(f *armFixture) { f.deleteHTTP = 503 }},
		{"oversized accepted response", func(f *armFixture) { f.largeDelete = true }},
		{"oversized error response", func(f *armFixture) { f.largeDelete, f.deleteHTTP = true, 503 }},
		{"request timeout", func(f *armFixture) { f.deleteHTTP = 408 }},
		{"throttled", func(f *armFixture) { f.deleteHTTP = 429 }},
		{"already absent", func(f *armFixture) { f.deleteHTTP = 204 }},
		{"untrusted operation URL", func(f *armFixture) { f.badPollURL = true }},
		{"authentication replay", func(f *armFixture) { f.deleteHTTP, f.challenge = 401, true }},
	} {
		t.Run(change.name, func(t *testing.T) {
			f := newARMFixture(t)
			f.prepare(true)
			change.run(f)
			journal, err := f.invoke(context.Background(), Delete, true)
			if err == nil || journal.Completion != nil || journal.Phase != DeletePending {
				t.Fatalf("unknown outcome became success: %v, %+v", err, journal)
			}
			store, err := openJournal(f.dir, false)
			if err != nil {
				t.Fatal(err)
			}
			persisted, loadErr := store.load()
			closeErr := store.close()
			if loadErr != nil || closeErr != nil {
				t.Fatalf("read persisted unknown outcome: %v", errors.Join(loadErr, closeErr))
			}
			if persisted.Phase != DeletePending || persisted.Completion != nil || persisted.Deletion == nil ||
				persisted.Deletion.RequestID != journal.Deletion.RequestID {
				t.Fatal("unknown deletion outcome was not durably preserved")
			}
			f.mu.Lock()
			f.etag, f.vmID = `"later-replacement"`, fixtureReplacement
			f.mu.Unlock()
			_, _ = f.invoke(context.Background(), Delete, true)
			_, _ = f.invoke(context.Background(), Observe, false)
			if len(f.mutations()) != 2 || !f.poolExists {
				t.Fatal("uncertain request was replayed onto a replacement")
			}
		})
	}
}

func TestCancellationDoesNotAuthorizeReplay(t *testing.T) {
	f := newARMFixture(t)
	f.prepare(true)
	ctx, cancel := context.WithCancel(context.Background())
	f.beforeDelete = func(*armFixture) { cancel() }
	journal, err := f.invoke(ctx, Delete, true)
	if err == nil || journal.Completion != nil {
		t.Fatalf("canceled submission succeeded: %+v", journal)
	}
	_, _ = f.invoke(context.Background(), Delete, true)
	if len(f.mutations()) != 2 {
		t.Fatal("canceled submission was replayed")
	}
}

func TestOperationAndOldVMDestructionAreBothRequired(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(*armFixture)
	}{
		{"operation running despite absent VM", func(f *armFixture) {
			f.pollStatus, f.poolExists, f.vmExists = "InProgress", false, false
		}},
		{"operation read error", func(f *armFixture) { f.pollHTTP = 503 }},
		{"operation record missing", func(f *armFixture) { f.pollHTTP = 404 }},
		{"terminal failed", func(f *armFixture) { f.pollStatus = "Failed" }},
		{"terminal canceled", func(f *armFixture) { f.pollStatus = "Canceled" }},
		{"deallocated VM remains", func(f *armFixture) { f.keepVM = true }},
		{"VM replacement remains", func(f *armFixture) { f.replaceVM = true }},
		{"VM read failed", func(f *armFixture) { f.readFailure = fixtureVM }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newARMFixture(t)
			f.prepare(true)
			if _, err := f.invoke(context.Background(), Delete, true); !errors.Is(err, ErrPending) {
				t.Fatal(err)
			}
			test.run(f)
			journal, err := f.invoke(context.Background(), Observe, false)
			if err == nil || journal.Completion != nil || journal.Phase == Complete || len(f.mutations()) != 2 {
				t.Fatalf("incomplete evidence became a completion receipt: %v, %+v", err, journal)
			}
		})
	}
}

func TestObservationReadFailureAndCancellationAreRetriable(t *testing.T) {
	f := newARMFixture(t)
	f.prepare(true)
	if _, err := f.invoke(context.Background(), Delete, true); !errors.Is(err, ErrPending) {
		t.Fatal(err)
	}
	f.pollHTTP = 503
	if _, err := f.invoke(context.Background(), Observe, false); err == nil {
		t.Fatal("read error was ignored")
	}
	f.pollHTTP = 0
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.invoke(ctx, Observe, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	journal, err := f.invoke(context.Background(), Observe, false)
	if err != nil || journal.Phase != Complete || len(f.mutations()) != 2 {
		t.Fatalf("read-only recovery failed: %v, %+v", err, journal)
	}
}

func TestCompletedJournalPreservesLaterReplacement(t *testing.T) {
	f := newARMFixture(t)
	f.prepare(true)
	_, _ = f.invoke(context.Background(), Delete, true)
	first, err := f.invoke(context.Background(), Observe, false)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.poolExists, f.vmExists = true, true
	f.etag, f.vmID = `"replacement-pool"`, fixtureReplacement
	f.mu.Unlock()
	if _, err := f.invoke(context.Background(), Delete, true); err == nil {
		t.Fatal("completed deletion resubmitted")
	}
	second, err := f.invoke(context.Background(), Observe, false)
	if err != nil || !second.Completion.ObservedAt.Equal(first.Completion.ObservedAt) || len(f.mutations()) != 2 || !f.poolExists {
		t.Fatalf("historical completion modified a replacement: %v", err)
	}
}

func TestNoDeleteWhenDurableIntentCannotBeWritten(t *testing.T) {
	f := newARMFixture(t)
	journal := f.prepare(true)
	store, err := openJournal(f.dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.lock.Close()
	if err := store.directory.Close(); err != nil {
		t.Fatal(err)
	}
	if err := deletePool(context.Background(), store, f.client, journal); err == nil {
		t.Fatal("deletion proceeded without durable intent")
	}
	if len(f.mutations()) != 1 {
		t.Fatal("failed journal transaction still submitted DELETE")
	}
}

func TestJournalLockAndStrictLoading(t *testing.T) {
	t.Run("concurrent access", func(t *testing.T) {
		f := newARMFixture(t)
		f.prepare(true)
		store, err := openJournal(f.dir, false)
		if err != nil {
			t.Fatal(err)
		}
		defer store.close()
		if _, err := f.invoke(context.Background(), Delete, true); err == nil || len(f.mutations()) != 1 {
			t.Fatal("concurrent journal user could submit deletion")
		}
	})
	for _, malformed := range []string{
		`{}`, `{"version":1,"unknown":true}`, `null`, `{} {}`,
	} {
		t.Run(fmt.Sprintf("malformed-%q", malformed), func(t *testing.T) {
			f := newARMFixture(t)
			f.prepare(true)
			if err := os.WriteFile(filepath.Join(f.dir, "journal.json"), []byte(malformed), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := f.invoke(context.Background(), Delete, true); err == nil || len(f.mutations()) != 1 {
				t.Fatal("invalid journal authorized deletion")
			}
		})
	}
	t.Run("attempt phase rollback", func(t *testing.T) {
		f := newARMFixture(t)
		journal := f.prepare(true)
		journal.Phase = Registered
		if journal.validate() == nil {
			t.Fatal("journal accepted a phase rollback that could replay the probe")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		f := newARMFixture(t)
		f.prepare(true)
		path := filepath.Join(f.dir, "journal.json")
		if err := os.Rename(path, path+".saved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path+".saved", path); err != nil {
			t.Fatal(err)
		}
		if _, err := f.invoke(context.Background(), Delete, true); err == nil || len(f.mutations()) != 1 {
			t.Fatal("symlink journal was trusted")
		}
	})
}
