// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package skillmarketplace

import (
	"context"
	"strings"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const namespace = "cetirun-system"

func newClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func team(name, uid, policy string, skills ...string) *api.Team {
	return &api.Team{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid)},
		Spec: api.TeamSpec{AgentPolicyRef: policy, Skills: skills}}
}

func TestTeamsForListsThePolicyBoundTeams(t *testing.T) {
	deleting := team("archive", "u4", "policy", "pdf-tools")
	deleting.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	document := TeamsFor([]api.Team{
		*team("support", "u2", "policy", "zh-notes", "npm:@example/pkg", "pdf-tools", "zh-notes"),
		*team("legacy", "u3", "", "pdf-tools"),
		*deleting,
		*team("research", "u1", "policy"),
	})
	want := []TeamSelection{{Name: "research", UID: "u1"}, {Name: "support", UID: "u2", Skills: []string{"pdf-tools", "zh-notes"}}}
	if document.Version != TeamsVersion || len(document.Teams) != 2 ||
		document.Teams[0].Name != want[0].Name || document.Teams[0].Skills != nil ||
		document.Teams[1].Name != want[1].Name || strings.Join(document.Teams[1].Skills, ",") != "pdf-tools,zh-notes" {
		t.Fatalf("document %+v", document)
	}
	if empty := TeamsFor(nil); empty.Teams == nil || len(empty.Teams) != 0 {
		t.Fatalf("no Teams must still be a document with an empty list: %+v", empty)
	}
}

func TestDecodeTeamsIsStrict(t *testing.T) {
	if document, err := DecodeTeams(`{"version":"v1","teams":[{"name":"research","uid":"u1","skills":["pdf-tools"]}]}`); err != nil ||
		len(document.Teams) != 1 {
		t.Fatalf("valid document: %+v %v", document, err)
	}
	for name, raw := range map[string]string{
		"unknown field":   `{"version":"v1","teams":[],"extra":1}`,
		"duplicate field": `{"version":"v1","version":"v1","teams":[]}`,
		"version":         `{"version":"v2","teams":[]}`,
		"no list":         `{"version":"v1"}`,
		"no UID":          `{"version":"v1","teams":[{"name":"research"}]}`,
		"no name":         `{"version":"v1","teams":[{"uid":"u1"}]}`,
		"not JSON":        `teams`,
		"too long":        `{"version":"v1","teams":[]}` + strings.Repeat(" ", MaxTeamsBytes),
	} {
		if _, err := DecodeTeams(raw); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
}

func TestTeamsReconcilerWritesTheDocumentAndTheCatalog(t *testing.T) {
	ctx := context.Background()
	research := team("research", "u1", "policy", "pdf-tools")
	kube := newClient(t, research, team("legacy", "u3", "", "pdf-tools"))
	r := &TeamsReconciler{Client: kube, APIReader: kube, Namespace: namespace}
	result, err := r.Reconcile(ctx, reconcile.Request{})
	if err != nil || result.RequeueAfter != resync {
		t.Fatalf("reconcile: %+v %v", result, err)
	}
	document := func() (TeamsDocument, string) {
		t.Helper()
		stored := &corev1.ConfigMap{}
		if err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: TeamsConfigMap}, stored); err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodeTeams(stored.Data[TeamsKey])
		if err != nil {
			t.Fatal(err)
		}
		return decoded, stored.ResourceVersion
	}
	written, version := document()
	if len(written.Teams) != 1 || written.Teams[0].UID != "u1" || written.Teams[0].Skills[0] != "pdf-tools" {
		t.Fatalf("document %+v", written)
	}
	catalog := &corev1.ConfigMap{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: api.SkillMarketplaceCatalogConfigMap}, catalog); err != nil ||
		len(catalog.Data) != 0 {
		t.Fatalf("the operator creates an empty catalog for the worker: %+v %v", catalog.Data, err)
	}
	// Unchanged Teams write nothing; the worker's catalog content stays.
	catalog.Data = map[string]string{api.SkillMarketplaceCatalogKey: `{"version":"v1"}`}
	if err := kube.Update(ctx, catalog); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	if _, again := document(); again != version {
		t.Fatal("an unchanged document must not be written again")
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(catalog), catalog); err != nil || catalog.Data[api.SkillMarketplaceCatalogKey] == "" {
		t.Fatalf("the operator must leave the worker's catalog alone: %+v %v", catalog.Data, err)
	}
	// A changed selection is written.
	research.Spec.Skills = []string{"pdf-tools", "web"}
	if err := kube.Update(ctx, research); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	if changed, _ := document(); strings.Join(changed.Teams[0].Skills, ",") != "pdf-tools,web" {
		t.Fatalf("document %+v", changed)
	}
}

func TestTeamsReconcilerRefusesADocumentThatDoesNotFit(t *testing.T) {
	var objects []client.Object
	for i := 0; i < 200; i++ {
		var skills []string
		for j := 0; j < 100; j++ {
			skills = append(skills, strings.Repeat("s", 50)+"-"+string(rune('a'+j%26))+string(rune('a'+j/26)))
		}
		objects = append(objects, team(strings.Repeat("t", 40)+string(rune('a'+i%26))+string(rune('a'+i/26)), "uid-"+strings.Repeat("0", 30)+string(rune('a'+i%26))+string(rune('a'+i/26)), "policy", skills...))
	}
	kube := newClient(t, objects...)
	r := &TeamsReconciler{Client: kube, APIReader: kube, Namespace: namespace}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{}); err == nil || !strings.Contains(err.Error(), "select fewer skills") {
		t.Fatalf("an oversized document must be refused: %v", err)
	}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: TeamsConfigMap}, &corev1.ConfigMap{}); err == nil {
		t.Fatal("no part of an oversized document may be written")
	}
}
