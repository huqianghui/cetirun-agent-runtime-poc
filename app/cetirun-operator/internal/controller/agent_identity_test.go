// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/azure"
)

const (
	testAgentIdentityTenantID    = "11111111-1111-1111-1111-111111111111"
	testAgentIdentityBlueprintID = "22222222-2222-2222-2222-222222222222"
	testAgentIdentityOwnerObject = "33333333-3333-3333-3333-333333333333"
	testAgentIdentityObjectID    = "44444444-4444-4444-4444-444444444444"
	testAgentIdentityClientID    = "55555555-5555-5555-5555-555555555555"
	testAgentIdentityInstanceUID = "66666666-6666-6666-6666-666666666666"
)

type fakeAgentIdentityDirectory struct {
	byObject              *azure.ServicePrincipal
	getObjectErr          error
	matches               []azure.ServicePrincipal
	getObjectCalls        int
	lookupCalls           int
	lastLookupDisplayName string
	createCalls           int
	deleteCalls           int
	deleteErr             error
	createResult          *azure.ServicePrincipal
	createErr             error
	onCreate              func()
	onDelete              func(string)
}

func (f *fakeAgentIdentityDirectory) CreateAgentIdentity(context.Context, string, string, string) (*azure.ServicePrincipal, error) {
	f.createCalls++
	if f.onCreate != nil {
		f.onCreate()
	}
	return f.createResult, f.createErr
}

type conflictOnceStatusClient struct {
	client.Client
	conflictsRemaining int
}

func (c *conflictOnceStatusClient) Status() client.SubResourceWriter {
	return &conflictOnceStatusWriter{SubResourceWriter: c.Client.Status(), parent: c}
}

type conflictOnceStatusWriter struct {
	client.SubResourceWriter
	parent *conflictOnceStatusClient
}

func (w *conflictOnceStatusWriter) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.SubResourcePatchOption) error {
	if w.parent.conflictsRemaining > 0 {
		w.parent.conflictsRemaining--
		return apierrors.NewConflict(schema.GroupResource{Group: cetirunv1alpha1.GroupVersion.Group, Resource: "agents"}, object.GetName(), errors.New("synthetic status conflict"))
	}
	return w.SubResourceWriter.Patch(ctx, object, patch, options...)
}

func (f *fakeAgentIdentityDirectory) DeleteServicePrincipal(_ context.Context, objectID string) error {
	f.deleteCalls++
	if f.onDelete != nil {
		f.onDelete(objectID)
	}
	return f.deleteErr
}

func (f *fakeAgentIdentityDirectory) GetAgentIdentity(context.Context, string) (*azure.ServicePrincipal, error) {
	f.getObjectCalls++
	return f.byObject, f.getObjectErr
}

func (f *fakeAgentIdentityDirectory) GetAgentIdentitiesByDisplayName(_ context.Context, displayName string) ([]azure.ServicePrincipal, error) {
	f.lookupCalls++
	f.lastLookupDisplayName = displayName
	return f.matches, nil
}

func agentIdentityTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := cetirunv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func newAgentIdentityTestInstance() *cetirunv1alpha1.Agent {
	return &cetirunv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "identity-test",
			UID:        types.UID(testAgentIdentityInstanceUID),
			Generation: 7,
			Annotations: map[string]string{
				cetirunv1alpha1.AnnotationOwnerTenantID: testAgentIdentityTenantID,
				cetirunv1alpha1.AnnotationOwnerObjectID: testAgentIdentityOwnerObject,
			},
		},
		Spec: cetirunv1alpha1.AgentSpec{AgentIdentity: &cetirunv1alpha1.AgentIdentitySpec{Enabled: true}},
	}
}

func testAgentIdentityDisplayName() string {
	return "cetirun-agent-" + testAgentIdentityInstanceUID
}

func TestAgentIdentityVerificationDue(t *testing.T) {
	now := time.Date(2026, time.September, 24, 12, 0, 0, 0, time.UTC)
	instance := newAgentIdentityTestInstance()

	if !agentIdentityVerificationDue(instance, now) {
		t.Fatal("missing status must require verification")
	}
	verifiedRecently := metav1.NewTime(now.Add(-9 * time.Minute))
	instance.Status.AgentIdentity = &cetirunv1alpha1.AgentIdentityStatus{
		Phase:            cetirunv1alpha1.AgentIdentityPhaseReady,
		LastVerifiedTime: &verifiedRecently,
	}
	if agentIdentityVerificationDue(instance, now) {
		t.Fatal("recent verification must remain eligible for the steady-state cache")
	}
	verifiedAtBoundary := metav1.NewTime(now.Add(-agentIdentityVerificationInterval))
	instance.Status.AgentIdentity.LastVerifiedTime = &verifiedAtBoundary
	if !agentIdentityVerificationDue(instance, now) {
		t.Fatal("verification at the interval boundary must be refreshed")
	}
	instance.Spec.AgentIdentity.Enabled = false
	if agentIdentityVerificationDue(instance, now) {
		t.Fatal("disabled Agent Identity must not trigger directory verification")
	}
}

func newAgentIdentityTestReconciler(t *testing.T, instance *cetirunv1alpha1.Agent, directory *fakeAgentIdentityDirectory) *AgentReconciler {
	t.Helper()
	scheme := agentIdentityTestScheme(t)
	builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(instance).WithStatusSubresource(instance)
	return &AgentReconciler{
		Client:                         builder.Build(),
		EventRecorder:                  record.NewFakeRecorder(10),
		AgentIdentityEnabled:           true,
		AgentIdentityTenantID:          testAgentIdentityTenantID,
		AgentIdentityBlueprintClientID: testAgentIdentityBlueprintID,
		agentIdentityGraphClientFactory: func() (agentIdentityDirectoryClient, error) {
			return directory, nil
		},
	}
}

func TestAgentIdentityGraphClientRemainsAvailableForCleanup(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("synthetic-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AZURE_FEDERATED_TOKEN_FILE", tokenFile)
	reconciler := &AgentReconciler{
		AgentIdentityEnabled:           false,
		AgentIdentityTenantID:          "11111111-1111-1111-1111-111111111111",
		AgentIdentityBlueprintClientID: "22222222-2222-2222-2222-222222222222",
	}
	if _, err := reconciler.agentIdentityGraphClient(); err != nil {
		t.Fatalf("cleanup credential was rejected after creation was disabled: %v", err)
	}
}

func TestDeleteAgentAuthorizationUsesBoundedOperatorEndpoint(t *testing.T) {
	const uid = "11111111-1111-1111-1111-111111111111"
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != http.MethodDelete || r.URL.Path != "/operator/v1/instances/"+uid+"/authorization" {
			t.Fatalf("unexpected cleanup request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	reconciler := &AgentReconciler{AgentIdentityBrokerURL: server.URL, AgentIdentityHTTPClient: server.Client()}
	if err := reconciler.deleteAgentAuthorization(context.Background(), uid); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("authorization cleanup endpoint was not called")
	}
}

func TestDeleteAgentAuthorizationFailsClosed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	reconciler := &AgentReconciler{AgentIdentityBrokerURL: server.URL, AgentIdentityHTTPClient: server.Client()}
	if err := reconciler.deleteAgentAuthorization(context.Background(), "11111111-1111-1111-1111-111111111111"); err == nil {
		t.Fatal("dependency failure must retain the Agent finalizer")
	}
}

func TestReconcileAgentIdentityUsesCanonicalObjectBinding(t *testing.T) {
	instance := newAgentIdentityTestInstance()
	instance.Status.AgentIdentity = &cetirunv1alpha1.AgentIdentityStatus{
		Phase:              cetirunv1alpha1.AgentIdentityPhaseProvisioning,
		ClientID:           testAgentIdentityClientID,
		ObjectID:           testAgentIdentityObjectID,
		ObservedGeneration: 6,
	}
	directory := &fakeAgentIdentityDirectory{byObject: &azure.ServicePrincipal{
		ID:                       testAgentIdentityObjectID,
		AppID:                    testAgentIdentityClientID,
		DisplayName:              testAgentIdentityDisplayName(),
		AgentIdentityBlueprintID: testAgentIdentityBlueprintID,
	}}
	reconciler := newAgentIdentityTestReconciler(t, instance, directory)

	if err := reconciler.reconcileAgentIdentity(context.Background(), instance); err != nil {
		t.Fatal(err)
	}
	if directory.getObjectCalls != 1 || directory.lookupCalls != 0 || directory.createCalls != 0 {
		t.Fatalf("canonical binding must use exactly one object lookup, got object=%d lookup=%d create=%d", directory.getObjectCalls, directory.lookupCalls, directory.createCalls)
	}
	if instance.Status.AgentIdentity.ClientID != testAgentIdentityClientID || instance.Status.AgentIdentity.ObjectID != testAgentIdentityObjectID {
		t.Fatalf("canonical binding was changed: %#v", instance.Status.AgentIdentity)
	}
	if instance.Status.AgentIdentity.Phase != cetirunv1alpha1.AgentIdentityPhaseReady {
		t.Fatalf("expected Ready, got %#v", instance.Status.AgentIdentity)
	}
	if instance.Status.AgentIdentity.DisplayName != testAgentIdentityDisplayName() {
		t.Fatalf("legacy canonical binding did not persist the immutable display name: %#v", instance.Status.AgentIdentity)
	}
	stored := &cetirunv1alpha1.Agent{}
	if err := reconciler.Get(context.Background(), types.NamespacedName{Name: instance.Name}, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Status.AgentIdentity == nil || stored.Status.AgentIdentity.DisplayName != testAgentIdentityDisplayName() {
		t.Fatalf("legacy display name migration was not persisted: %#v", stored.Status.AgentIdentity)
	}
}

func TestReconcileAgentIdentityFailsClosedWhenCanonicalIdentityWasDeleted(t *testing.T) {
	instance := newAgentIdentityTestInstance()
	instance.Status.AgentIdentity = &cetirunv1alpha1.AgentIdentityStatus{
		Phase:       cetirunv1alpha1.AgentIdentityPhaseReady,
		ClientID:    testAgentIdentityClientID,
		ObjectID:    testAgentIdentityObjectID,
		DisplayName: testAgentIdentityDisplayName(),
	}
	directory := &fakeAgentIdentityDirectory{}
	reconciler := newAgentIdentityTestReconciler(t, instance, directory)

	if err := reconciler.reconcileAgentIdentity(context.Background(), instance); err == nil {
		t.Fatal("an externally deleted canonical identity must fail closed")
	}
	if directory.getObjectCalls != 1 || directory.lookupCalls != 0 || directory.createCalls != 0 {
		t.Fatalf("deleted canonical identity must not fall back to adoption or creation: object=%d lookup=%d create=%d", directory.getObjectCalls, directory.lookupCalls, directory.createCalls)
	}
	if got := instance.Status.AgentIdentity.Reason; got != "BindingNotFound" {
		t.Fatalf("unexpected reason %q", got)
	}
}

func TestReconcileAgentIdentityFailsClosedOnCanonicalVerificationError(t *testing.T) {
	instance := newAgentIdentityTestInstance()
	instance.Status.AgentIdentity = &cetirunv1alpha1.AgentIdentityStatus{
		Phase:       cetirunv1alpha1.AgentIdentityPhaseReady,
		ClientID:    testAgentIdentityClientID,
		ObjectID:    testAgentIdentityObjectID,
		DisplayName: testAgentIdentityDisplayName(),
	}
	directory := &fakeAgentIdentityDirectory{getObjectErr: errors.New("synthetic Graph outage")}
	reconciler := newAgentIdentityTestReconciler(t, instance, directory)

	if err := reconciler.reconcileAgentIdentity(context.Background(), instance); err == nil {
		t.Fatal("a directory verification error must fail closed")
	}
	if directory.getObjectCalls != 1 || directory.lookupCalls != 0 || directory.createCalls != 0 {
		t.Fatalf("verification failure must not fall back to adoption or creation: object=%d lookup=%d create=%d", directory.getObjectCalls, directory.lookupCalls, directory.createCalls)
	}
	if got := instance.Status.AgentIdentity.Reason; got != "DirectoryLookupFailed" {
		t.Fatalf("unexpected reason %q", got)
	}
}

func TestReconcileAgentIdentityPersistsCreatedBindingAndReusesIt(t *testing.T) {
	instance := newAgentIdentityTestInstance()
	created := &azure.ServicePrincipal{
		ID:                       testAgentIdentityObjectID,
		AppID:                    testAgentIdentityClientID,
		DisplayName:              testAgentIdentityDisplayName(),
		AgentIdentityBlueprintID: testAgentIdentityBlueprintID,
	}
	directory := &fakeAgentIdentityDirectory{createResult: created, byObject: created}
	reconciler := newAgentIdentityTestReconciler(t, instance, directory)

	if err := reconciler.reconcileAgentIdentity(context.Background(), instance); err != nil {
		t.Fatal(err)
	}
	if directory.createCalls != 1 {
		t.Fatalf("expected one create, got %d", directory.createCalls)
	}
	stored := &cetirunv1alpha1.Agent{}
	if err := reconciler.Get(context.Background(), types.NamespacedName{Name: instance.Name}, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Status.AgentIdentity == nil || stored.Status.AgentIdentity.ClientID != testAgentIdentityClientID || stored.Status.AgentIdentity.ObjectID != testAgentIdentityObjectID || stored.Status.AgentIdentity.CreationAttempted {
		t.Fatalf("created binding was not persisted before returning: %#v", stored.Status.AgentIdentity)
	}

	if err := reconciler.reconcileAgentIdentity(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	if directory.createCalls != 1 || directory.getObjectCalls != 1 || directory.lookupCalls != 1 {
		t.Fatalf("repeat reconcile did not use persisted binding: create=%d object=%d lookup=%d", directory.createCalls, directory.getObjectCalls, directory.lookupCalls)
	}
}

func TestReconcileAgentIdentityAdoptsAfterCreatedBindingStatusConflict(t *testing.T) {
	instance := newAgentIdentityTestInstance()
	created := &azure.ServicePrincipal{
		ID:                       testAgentIdentityObjectID,
		AppID:                    testAgentIdentityClientID,
		DisplayName:              testAgentIdentityDisplayName(),
		AgentIdentityBlueprintID: testAgentIdentityBlueprintID,
	}
	directory := &fakeAgentIdentityDirectory{createResult: created}
	reconciler := newAgentIdentityTestReconciler(t, instance, directory)
	conflictingClient := &conflictOnceStatusClient{Client: reconciler.Client}
	reconciler.Client = conflictingClient
	directory.onCreate = func() {
		directory.matches = []azure.ServicePrincipal{*created}
		conflictingClient.conflictsRemaining = 1
	}

	if err := reconciler.reconcileAgentIdentity(context.Background(), instance); err == nil {
		t.Fatal("expected the created binding status update to conflict")
	}
	if directory.createCalls != 1 {
		t.Fatalf("expected exactly one create before the status conflict, got %d", directory.createCalls)
	}
	stored := &cetirunv1alpha1.Agent{}
	if err := reconciler.Get(context.Background(), types.NamespacedName{Name: instance.Name}, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Status.AgentIdentity == nil || !stored.Status.AgentIdentity.CreationAttempted || stored.Status.AgentIdentity.DisplayName != testAgentIdentityDisplayName() || stored.Status.AgentIdentity.ClientID != "" || stored.Status.AgentIdentity.ObjectID != "" {
		t.Fatalf("create checkpoint was not retained after the binding conflict: %#v", stored.Status.AgentIdentity)
	}

	if err := reconciler.reconcileAgentIdentity(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	if directory.createCalls != 1 || directory.lookupCalls != 2 {
		t.Fatalf("recovery did not adopt without another POST: create=%d lookup=%d", directory.createCalls, directory.lookupCalls)
	}
	if stored.Status.AgentIdentity == nil || stored.Status.AgentIdentity.ClientID != testAgentIdentityClientID || stored.Status.AgentIdentity.ObjectID != testAgentIdentityObjectID || stored.Status.AgentIdentity.CreationAttempted {
		t.Fatalf("recovered binding was not persisted: %#v", stored.Status.AgentIdentity)
	}
}

func TestAgentIdentityBindingSurvivesHelmFailureStatusUpdate(t *testing.T) {
	instance := newAgentIdentityTestInstance()
	created := &azure.ServicePrincipal{
		ID:                       testAgentIdentityObjectID,
		AppID:                    testAgentIdentityClientID,
		DisplayName:              testAgentIdentityDisplayName(),
		AgentIdentityBlueprintID: testAgentIdentityBlueprintID,
	}
	directory := &fakeAgentIdentityDirectory{createResult: created}
	reconciler := newAgentIdentityTestReconciler(t, instance, directory)

	if err := reconciler.reconcileAgentIdentity(context.Background(), instance); err != nil {
		t.Fatal(err)
	}
	_, _ = reconciler.failWith(context.Background(), instance, "HelmInstallFailed", "synthetic Helm failure")

	stored := &cetirunv1alpha1.Agent{}
	if err := reconciler.Get(context.Background(), types.NamespacedName{Name: instance.Name}, stored); err != nil {
		t.Fatal(err)
	}
	status := stored.Status.AgentIdentity
	if status == nil || status.ClientID != testAgentIdentityClientID || status.ObjectID != testAgentIdentityObjectID || status.CreationAttempted {
		t.Fatalf("Helm failure status update erased the persisted binding: %#v", status)
	}
}

func TestReconcileAgentIdentityUnknownCreateOutcomeDoesNotRetry(t *testing.T) {
	instance := newAgentIdentityTestInstance()
	instance.Status.AgentIdentity = &cetirunv1alpha1.AgentIdentityStatus{
		CreationAttempted: true,
		DisplayName:       testAgentIdentityDisplayName(),
	}
	directory := &fakeAgentIdentityDirectory{}
	reconciler := newAgentIdentityTestReconciler(t, instance, directory)

	if err := reconciler.reconcileAgentIdentity(context.Background(), instance); err == nil {
		t.Fatal("expected uncertain create outcome to block a duplicate create")
	}
	if directory.createCalls != 0 {
		t.Fatalf("unknown create outcome retried POST %d times", directory.createCalls)
	}
	if got := instance.Status.AgentIdentity.Reason; got != "CreateOutcomeUnknown" {
		t.Fatalf("unexpected reason %q", got)
	}
}

func TestReconcileAgentIdentityUnknownCreateUsesRecordedDisplayNameAfterPrefixChange(t *testing.T) {
	instance := newAgentIdentityTestInstance()
	historicalName := "historical-prefix-" + testAgentIdentityInstanceUID
	instance.Status.AgentIdentity = &cetirunv1alpha1.AgentIdentityStatus{
		CreationAttempted: true,
		DisplayName:       historicalName,
	}
	directory := &fakeAgentIdentityDirectory{}
	reconciler := newAgentIdentityTestReconciler(t, instance, directory)
	reconciler.AgentIdentityNamePrefix = "new-prefix"

	if err := reconciler.reconcileAgentIdentity(context.Background(), instance); err == nil {
		t.Fatal("expected unknown create outcome to block a duplicate create")
	}
	if directory.createCalls != 0 {
		t.Fatalf("unknown create outcome retried POST %d times", directory.createCalls)
	}
	if directory.lastLookupDisplayName != historicalName {
		t.Fatalf("lookup used %q, want recorded name %q", directory.lastLookupDisplayName, historicalName)
	}
	status := instance.Status.AgentIdentity
	if status == nil || !status.CreationAttempted || status.DisplayName != historicalName {
		t.Fatalf("unknown create checkpoint was changed: %#v", status)
	}
}

func TestReconcileAgentIdentityRequestNotSentClearsCreateCheckpoint(t *testing.T) {
	instance := newAgentIdentityTestInstance()
	directory := &fakeAgentIdentityDirectory{createErr: azure.NewRequestNotSentError(errors.New("synthetic token failure"))}
	reconciler := newAgentIdentityTestReconciler(t, instance, directory)
	conflictingClient := &conflictOnceStatusClient{Client: reconciler.Client}
	reconciler.Client = conflictingClient
	directory.onCreate = func() { conflictingClient.conflictsRemaining = 1 }

	if err := reconciler.reconcileAgentIdentity(context.Background(), instance); err == nil {
		t.Fatal("expected request-not-sent create failure")
	}
	status := instance.Status.AgentIdentity
	if status == nil || status.CreationAttempted || status.Reason != "CreateFailed" {
		t.Fatalf("safe-to-retry failure did not clear creation checkpoint: %#v", status)
	}
	stored := &cetirunv1alpha1.Agent{}
	if err := reconciler.Get(context.Background(), types.NamespacedName{Name: instance.Name}, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Status.AgentIdentity == nil || stored.Status.AgentIdentity.CreationAttempted || stored.Status.AgentIdentity.Reason != "CreateFailed" {
		t.Fatalf("conflict retry did not persist checkpoint reset: %#v", stored.Status.AgentIdentity)
	}
	if conflictingClient.conflictsRemaining != 0 {
		t.Fatal("status conflict injection was not consumed")
	}
}

func TestReconcileAgentIdentityUsesRecordedDisplayNameAfterPrefixChange(t *testing.T) {
	instance := newAgentIdentityTestInstance()
	instance.Status.AgentIdentity = &cetirunv1alpha1.AgentIdentityStatus{
		ClientID:    testAgentIdentityClientID,
		ObjectID:    testAgentIdentityObjectID,
		DisplayName: "historical-prefix-" + testAgentIdentityInstanceUID,
	}
	directory := &fakeAgentIdentityDirectory{byObject: &azure.ServicePrincipal{
		ID:                       testAgentIdentityObjectID,
		AppID:                    testAgentIdentityClientID,
		DisplayName:              "historical-prefix-" + testAgentIdentityInstanceUID,
		AgentIdentityBlueprintID: testAgentIdentityBlueprintID,
	}}
	reconciler := newAgentIdentityTestReconciler(t, instance, directory)
	reconciler.AgentIdentityNamePrefix = "new-prefix"

	if err := reconciler.reconcileAgentIdentity(context.Background(), instance); err != nil {
		t.Fatal(err)
	}
	if instance.Status.AgentIdentity.DisplayName != "historical-prefix-"+testAgentIdentityInstanceUID {
		t.Fatalf("prefix change replaced immutable display name: %#v", instance.Status.AgentIdentity)
	}
}

func TestReconcileAgentIdentityRejectsIncompleteBinding(t *testing.T) {
	instance := newAgentIdentityTestInstance()
	instance.Status.AgentIdentity = &cetirunv1alpha1.AgentIdentityStatus{ClientID: testAgentIdentityClientID}
	directory := &fakeAgentIdentityDirectory{}
	reconciler := newAgentIdentityTestReconciler(t, instance, directory)

	if err := reconciler.reconcileAgentIdentity(context.Background(), instance); err == nil {
		t.Fatal("expected incomplete binding to fail closed")
	}
	if directory.getObjectCalls != 0 || directory.lookupCalls != 0 || directory.createCalls != 0 {
		t.Fatalf("incomplete binding must not access an adoption or create path: object=%d lookup=%d create=%d", directory.getObjectCalls, directory.lookupCalls, directory.createCalls)
	}
	if got := instance.Status.AgentIdentity.Reason; got != "BindingIncomplete" {
		t.Fatalf("unexpected reason %q", got)
	}
}

func TestReconcileAgentIdentityRejectsCanonicalBindingWithUnexpectedDisplayName(t *testing.T) {
	instance := newAgentIdentityTestInstance()
	instance.Status.AgentIdentity = &cetirunv1alpha1.AgentIdentityStatus{ClientID: testAgentIdentityClientID, ObjectID: testAgentIdentityObjectID}
	directory := &fakeAgentIdentityDirectory{byObject: &azure.ServicePrincipal{
		ID:                       testAgentIdentityObjectID,
		AppID:                    testAgentIdentityClientID,
		DisplayName:              "cetirun-agent-another-instance",
		AgentIdentityBlueprintID: testAgentIdentityBlueprintID,
	}}
	reconciler := newAgentIdentityTestReconciler(t, instance, directory)

	if err := reconciler.reconcileAgentIdentity(context.Background(), instance); err == nil {
		t.Fatal("expected mismatched display name to fail closed")
	}
	if directory.getObjectCalls != 1 || directory.lookupCalls != 0 || directory.createCalls != 0 {
		t.Fatalf("canonical mismatch must not fall back to adoption or create: object=%d lookup=%d create=%d", directory.getObjectCalls, directory.lookupCalls, directory.createCalls)
	}
	if got := instance.Status.AgentIdentity.Reason; got != "BindingMismatch" {
		t.Fatalf("unexpected reason %q", got)
	}
}

func TestReconcileAgentIdentityAmbiguousAdoptionDoesNotCreate(t *testing.T) {
	instance := newAgentIdentityTestInstance()
	directory := &fakeAgentIdentityDirectory{matches: []azure.ServicePrincipal{
		{ID: testAgentIdentityObjectID, AppID: testAgentIdentityClientID, AgentIdentityBlueprintID: testAgentIdentityBlueprintID},
		{ID: "77777777-7777-7777-7777-777777777777", AppID: "88888888-8888-8888-8888-888888888888", AgentIdentityBlueprintID: testAgentIdentityBlueprintID},
	}}
	reconciler := newAgentIdentityTestReconciler(t, instance, directory)

	if err := reconciler.reconcileAgentIdentity(context.Background(), instance); err == nil {
		t.Fatal("expected ambiguous adoption to fail")
	}
	if directory.createCalls != 0 || directory.deleteCalls != 0 {
		t.Fatalf("ambiguous adoption must not mutate directory: create=%d delete=%d", directory.createCalls, directory.deleteCalls)
	}
	if got := instance.Status.AgentIdentity.Reason; got != "AdoptionAmbiguous" {
		t.Fatalf("unexpected reason %q", got)
	}
}

func TestDeleteAgentIdentityUnknownCreateZeroMatchRetainsCheckpoint(t *testing.T) {
	instance := newAgentIdentityTestInstance()
	historicalName := "historical-prefix-" + testAgentIdentityInstanceUID
	instance.Status.AgentIdentity = &cetirunv1alpha1.AgentIdentityStatus{
		CreationAttempted: true,
		DisplayName:       historicalName,
	}
	directory := &fakeAgentIdentityDirectory{}
	reconciler := newAgentIdentityTestReconciler(t, instance, directory)
	reconciler.AgentIdentityNamePrefix = "new-prefix"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Fatalf("unexpected authorization cleanup method %q", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	reconciler.AgentIdentityBrokerURL = server.URL
	reconciler.AgentIdentityHTTPClient = server.Client()

	if err := reconciler.deleteAgentIdentity(context.Background(), instance); err == nil {
		t.Fatal("expected cleanup to retain an unknown create checkpoint with no directory match")
	}
	if directory.lastLookupDisplayName != historicalName {
		t.Fatalf("cleanup lookup used %q, want recorded name %q", directory.lastLookupDisplayName, historicalName)
	}
	if directory.deleteCalls != 0 {
		t.Fatalf("zero-match cleanup deleted %d principals", directory.deleteCalls)
	}
	status := instance.Status.AgentIdentity
	if status == nil || status.Phase == cetirunv1alpha1.AgentIdentityPhaseDisabled || !status.CreationAttempted || status.DisplayName != historicalName {
		t.Fatalf("cleanup changed unknown create checkpoint: %#v", status)
	}
}

func TestDeleteAgentIdentityPersistsRecoveredBindingBeforeDelete(t *testing.T) {
	instance := newAgentIdentityTestInstance()
	historicalName := "historical-prefix-" + testAgentIdentityInstanceUID
	instance.Status.AgentIdentity = &cetirunv1alpha1.AgentIdentityStatus{
		CreationAttempted: true,
		DisplayName:       historicalName,
	}
	recovered := azure.ServicePrincipal{
		ID:                       testAgentIdentityObjectID,
		AppID:                    testAgentIdentityClientID,
		DisplayName:              historicalName,
		AgentIdentityBlueprintID: testAgentIdentityBlueprintID,
	}
	directory := &fakeAgentIdentityDirectory{
		matches:   []azure.ServicePrincipal{recovered},
		deleteErr: errors.New("synthetic lost delete response"),
	}
	reconciler := newAgentIdentityTestReconciler(t, instance, directory)
	reconciler.AgentIdentityBrokerURL = newSuccessfulAuthorizationCleanupServer(t, reconciler)
	directory.onDelete = func(objectID string) {
		if objectID != testAgentIdentityObjectID {
			t.Fatalf("deleted object %q, want %q", objectID, testAgentIdentityObjectID)
		}
		stored := &cetirunv1alpha1.Agent{}
		if err := reconciler.Get(context.Background(), types.NamespacedName{Name: instance.Name}, stored); err != nil {
			t.Fatal(err)
		}
		status := stored.Status.AgentIdentity
		if status == nil || status.ClientID != testAgentIdentityClientID || status.ObjectID != testAgentIdentityObjectID || status.DisplayName != historicalName || status.CreationAttempted {
			t.Fatalf("canonical binding was not durable before DELETE: %#v", status)
		}
	}

	if err := reconciler.deleteAgentIdentity(context.Background(), instance); err == nil {
		t.Fatal("expected the lost delete response to keep cleanup retryable")
	}
	if directory.deleteCalls != 1 {
		t.Fatalf("expected one delete attempt, got %d", directory.deleteCalls)
	}

	stored := &cetirunv1alpha1.Agent{}
	if err := reconciler.Get(context.Background(), types.NamespacedName{Name: instance.Name}, stored); err != nil {
		t.Fatal(err)
	}
	directory.matches = nil
	directory.byObject = nil
	directory.deleteErr = nil
	if err := reconciler.deleteAgentIdentity(context.Background(), stored); err != nil {
		t.Fatalf("retry did not treat the absent canonical principal as deleted: %v", err)
	}
	if directory.lookupCalls != 1 || directory.getObjectCalls != 1 || directory.deleteCalls != 1 {
		t.Fatalf("retry did not use the canonical object path: lookup=%d object=%d delete=%d", directory.lookupCalls, directory.getObjectCalls, directory.deleteCalls)
	}
}

func newSuccessfulAuthorizationCleanupServer(t *testing.T, reconciler *AgentReconciler) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("unexpected authorization cleanup method %q", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	reconciler.AgentIdentityHTTPClient = server.Client()
	return server.URL
}
