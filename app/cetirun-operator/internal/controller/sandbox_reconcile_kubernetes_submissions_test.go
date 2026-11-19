// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestReconciledKubernetesGrantSubmissionInventory(t *testing.T) {
	ref := sandboxpolicy.BindingReference{Kind: "ClusterRoleBinding", Name: "example-grant", AgentName: "example", AgentUID: "agent-uid"}
	ref.RoleRef.Kind = "ClusterRole"
	digest, err := reconciledGrantDigest(ref)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal([]string{digest})
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name, state, submissions string
		present, invalid         bool
		want                     int
	}{
		{name: "legacy pending", state: sandboxApplyPending},
		{name: "legacy cancelled", state: sandboxApplyCancelled},
		{name: "legacy submitted", state: sandboxApplySubmitted, want: 1},
		{name: "legacy unknown", want: 1},
		{name: "unsubmitted retained grant", state: sandboxApplySubmitted, submissions: "[]", present: true},
		{name: "submitted retained grant", state: sandboxApplySubmitted, submissions: string(encoded), present: true, want: 1},
		{name: "null", submissions: "null", present: true, invalid: true},
		{name: "malformed", submissions: "{", present: true, invalid: true},
		{name: "invalid digest", submissions: `["invalid"]`, present: true, invalid: true},
		{name: "duplicate", submissions: `["` + digest + `","` + digest + `"]`, present: true, invalid: true},
		{name: "oversized", submissions: strings.Repeat(" ", sandboxpolicy.MaxGrantJournalReferences*68+1), present: true, invalid: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			state := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				sandboxApplyStateAnnotation: scenario.state,
			}}}
			if scenario.present {
				state.Annotations[sandboxGrantSubmissionsAnnotation] = scenario.submissions
			}
			got, err := submittedReconciledGrants(state, []sandboxpolicy.BindingReference{ref})
			if (err != nil) != scenario.invalid || (!scenario.invalid && len(got) != scenario.want) {
				t.Fatalf("submission classification: count=%d, error=%v", len(got), err)
			}
		})
	}
}

func TestReconciledSandboxKubernetesUnsubmittedResumeGrantRecovery(t *testing.T) {
	f := newReconciledKubernetesFixture(t, "openclaw", corev1.ReadWriteOnce)
	ctx := context.Background()
	initial := reconciledExpectationForTest(t, f)
	agent := f.current(t)
	agent.Spec.Suspended = true
	agent.Generation++
	if err := f.kube.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		f.reconcileOK(t)
	}
	installer := f.r.HelmClient.(*sharedSandboxInstaller)
	injected := errors.New("synthetic retained preflight failure")
	var attempted *helm.PreparedRelease
	installer.beforeApply = func(prepared *helm.PreparedRelease) error {
		attempted = prepared
		if err := prepared.CheckSandboxApply(ctx); err != nil {
			return err
		}
		return injected
	}
	agent = f.current(t)
	agent.Spec.Suspended = false
	agent.Generation++
	if err := f.kube.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reconcile(); !errors.Is(err, injected) || attempted == nil {
		t.Fatal("fixture did not reject retained preflight:", err)
	}
	refs, err := f.r.sandboxGrantJournal().Read(ctx, agent.Name, agent.UID)
	if err != nil || len(refs) != 0 {
		t.Fatal("unsubmitted retained grant intents were stranded:", err)
	}
	if err := attempted.AuthorizeSandboxApply(ctx); err == nil {
		t.Fatal("revoked unsubmitted grants permitted a stale prepared apply")
	}
	installer.beforeApply = nil
	for range 4 {
		f.reconcileOK(t)
	}
	expected := reconciledExpectationForTest(t, f)
	if expected.Agent != initial.Agent || expected.Namespace != initial.Namespace || expected.Home != initial.Home {
		t.Fatal("retained preflight recovery replaced the lifetime")
	}
	assertKubernetesFixtureGrants(t, f, true)
}
