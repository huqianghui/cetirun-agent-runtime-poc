// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
)

func skillMarketplaceConfigMap(namespace string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: cetirunv1alpha1.SkillMarketplaceCatalogConfigMap, Namespace: namespace},
		Data:       data,
	}
}

func exampleSkillMarketplaceCatalog() cetirunv1alpha1.SkillMarketplaceCatalog {
	return cetirunv1alpha1.SkillMarketplaceCatalog{
		Version:    cetirunv1alpha1.SkillMarketplaceCatalogVersion,
		Repository: cetirunv1alpha1.SkillMarketplaceRepository{URL: "https://git.example.com/skills.git", Ref: "main", Path: "skills"},
		Commit:     strings.Repeat("a", 40),
		SyncedAt:   "2026-11-03T00:00:00Z",
		CheckedAt:  "2026-11-03T00:05:00Z",
		Skills: []cetirunv1alpha1.SkillMarketplaceSkill{{
			Name: "pdf-tools", Description: "Read and write PDF files", Version: "0123456789ab",
			Path: "skills/pdf-tools", UpdatedAt: "2026-11-02T00:00:00Z", Files: 2, Bytes: 2048,
		}},
		Invalid: []cetirunv1alpha1.SkillMarketplaceProblem{{Path: "skills/broken", Name: "broken", Reason: "SKILL.md has no description"}},
		Teams: []cetirunv1alpha1.SkillMarketplaceTeam{{
			Name: "research", UID: "team-uid", SelectionDigest: cetirunv1alpha1.SkillMarketplaceSelectionDigest([]string{"pdf-tools"}),
			Release: strings.Repeat("b", 20), ReleaseSelectionDigest: cetirunv1alpha1.SkillMarketplaceSelectionDigest([]string{"pdf-tools"}),
			PublishedAt: "2026-11-03T00:01:00Z", Skills: []cetirunv1alpha1.SkillMarketplaceReleaseSkill{{Name: "pdf-tools", Version: "0123456789ab"}},
		}},
	}
}

func TestGetSkillMarketplaceCatalog(t *testing.T) {
	valid, err := json.Marshal(exampleSkillMarketplaceCatalog())
	if err != nil {
		t.Fatal(err)
	}
	withFutureFields := strings.Replace(string(valid), `{"version":"v1",`, `{"version":"v1","futureField":{"x":1},`, 1)
	withFutureFields = strings.Replace(withFutureFields, `"files":2,`, `"files":2,"license":"MIT",`, 1)
	if withFutureFields == string(valid) {
		t.Fatal("the test did not add fields")
	}
	for _, tc := range []struct {
		name      string
		objects   []client.Object
		wantKnown bool
		wantError string
	}{
		{name: "not published"},
		{name: "published in another namespace", objects: []client.Object{skillMarketplaceConfigMap("other", map[string]string{cetirunv1alpha1.SkillMarketplaceCatalogKey: string(valid)})}},
		{name: "without the catalog key", objects: []client.Object{skillMarketplaceConfigMap("cetirun-system", map[string]string{"other.json": string(valid)})}},
		{name: "published", objects: []client.Object{skillMarketplaceConfigMap("cetirun-system", map[string]string{cetirunv1alpha1.SkillMarketplaceCatalogKey: string(valid)})}, wantKnown: true},
		{name: "fields of a newer marketplace", objects: []client.Object{skillMarketplaceConfigMap("cetirun-system", map[string]string{cetirunv1alpha1.SkillMarketplaceCatalogKey: withFutureFields})}, wantKnown: true},
		{name: "not JSON", objects: []client.Object{skillMarketplaceConfigMap("cetirun-system", map[string]string{cetirunv1alpha1.SkillMarketplaceCatalogKey: "{"})}, wantError: "decode the Skill Marketplace catalog"},
		{name: "empty", objects: []client.Object{skillMarketplaceConfigMap("cetirun-system", map[string]string{cetirunv1alpha1.SkillMarketplaceCatalogKey: ""})}, wantError: "decode the Skill Marketplace catalog"},
		{name: "trailing data", objects: []client.Object{skillMarketplaceConfigMap("cetirun-system", map[string]string{cetirunv1alpha1.SkillMarketplaceCatalogKey: string(valid) + "{}"})}, wantError: "decode the Skill Marketplace catalog"},
		{name: "duplicate field", objects: []client.Object{skillMarketplaceConfigMap("cetirun-system", map[string]string{cetirunv1alpha1.SkillMarketplaceCatalogKey: `{"version":"v1","skills":[],"skills":[]}`})}, wantError: "duplicate field"},
		{name: "wrong type", objects: []client.Object{skillMarketplaceConfigMap("cetirun-system", map[string]string{cetirunv1alpha1.SkillMarketplaceCatalogKey: `{"version":"v1","skills":{}}`})}, wantError: "decode the Skill Marketplace catalog"},
		{name: "null", objects: []client.Object{skillMarketplaceConfigMap("cetirun-system", map[string]string{cetirunv1alpha1.SkillMarketplaceCatalogKey: "null"})}, wantError: "not the supported version"},
		{name: "another version", objects: []client.Object{skillMarketplaceConfigMap("cetirun-system", map[string]string{cetirunv1alpha1.SkillMarketplaceCatalogKey: `{"version":"v2"}`})}, wantError: `version "v2"`},
		{name: "field names are case-sensitive", objects: []client.Object{skillMarketplaceConfigMap("cetirun-system", map[string]string{cetirunv1alpha1.SkillMarketplaceCatalogKey: `{"Version":"v1"}`})}, wantError: "not the supported version"},
		{name: "larger than 1 MiB", objects: []client.Object{skillMarketplaceConfigMap("cetirun-system", map[string]string{cetirunv1alpha1.SkillMarketplaceCatalogKey: `{"version":"v1","error":"` + strings.Repeat("x", 1<<20) + `"}`})}, wantError: "longer than 1048576 bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog, known, err := NewClient(newFakeClient(tc.objects...)).GetSkillMarketplaceCatalog(context.Background(), "cetirun-system")
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) || known {
					t.Fatalf("known=%v err=%v, want an error containing %q", known, err, tc.wantError)
				}
				if !reflect.DeepEqual(catalog, cetirunv1alpha1.SkillMarketplaceCatalog{}) {
					t.Fatalf("an unreadable catalog returned content: %+v", catalog)
				}
				return
			}
			if err != nil || known != tc.wantKnown {
				t.Fatalf("known=%v err=%v, want known=%v", known, err, tc.wantKnown)
			}
			want := cetirunv1alpha1.SkillMarketplaceCatalog{}
			if tc.wantKnown {
				want = exampleSkillMarketplaceCatalog()
			}
			if !reflect.DeepEqual(catalog, want) {
				t.Fatalf("catalog = %+v, want %+v", catalog, want)
			}
		})
	}
}

func TestGetSkillMarketplaceCatalogReportsReadFailures(t *testing.T) {
	denied := apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, cetirunv1alpha1.SkillMarketplaceCatalogConfigMap, errors.New("synthetic denial"))
	c := interceptor.NewClient(newFakeClient().(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			return denied
		},
	})
	_, known, err := NewClient(c).GetSkillMarketplaceCatalog(context.Background(), "cetirun-system")
	if known || !apierrors.IsForbidden(err) {
		t.Fatalf("known=%v err=%v, want the read failure", known, err)
	}
}

// Every Agent page polls the catalog. While the ConfigMap stays the same, a
// read transfers only its metadata and returns the catalog decoded before; a
// change of the ConfigMap is read at once.
func TestGetSkillMarketplaceCatalogReadsTheCatalogOnlyAfterAChange(t *testing.T) {
	valid, err := json.Marshal(exampleSkillMarketplaceCatalog())
	if err != nil {
		t.Fatal(err)
	}
	storage := newFakeClient(skillMarketplaceConfigMap("cetirun-system", map[string]string{cetirunv1alpha1.SkillMarketplaceCatalogKey: string(valid)}))
	var full, metadata int
	counting := interceptor.NewClient(storage.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			switch obj.(type) {
			case *corev1.ConfigMap:
				full++
			case *metav1.PartialObjectMetadata:
				metadata++
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	portal := NewClient(counting)
	ctx := context.Background()
	for range 5 {
		catalog, known, err := portal.GetSkillMarketplaceCatalog(ctx, "cetirun-system")
		if err != nil || !known || !reflect.DeepEqual(catalog, exampleSkillMarketplaceCatalog()) {
			t.Fatalf("catalog %+v known=%v err=%v", catalog, known, err)
		}
	}
	if full != 1 || metadata != 5 {
		t.Fatalf("five polls read the whole catalog %d times and its metadata %d times, want once and five times", full, metadata)
	}
	changed := exampleSkillMarketplaceCatalog()
	changed.Commit = strings.Repeat("c", 40)
	raw, err := json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	stored := &corev1.ConfigMap{}
	if err := storage.Get(ctx, client.ObjectKey{Namespace: "cetirun-system", Name: cetirunv1alpha1.SkillMarketplaceCatalogConfigMap}, stored); err != nil {
		t.Fatal(err)
	}
	stored.Data[cetirunv1alpha1.SkillMarketplaceCatalogKey] = string(raw)
	if err := storage.Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if catalog, _, err := portal.GetSkillMarketplaceCatalog(ctx, "cetirun-system"); err != nil || catalog.Commit != changed.Commit || full != 2 {
		t.Fatalf("a changed catalog must be read at once: commit %q, %d full reads, %v", catalog.Commit, full, err)
	}
	// A catalog that the Portal cannot decode is not kept.
	stored.Data[cetirunv1alpha1.SkillMarketplaceCatalogKey] = "{"
	if err := storage.Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, known, err := portal.GetSkillMarketplaceCatalog(ctx, "cetirun-system"); err == nil || known {
			t.Fatalf("a damaged catalog must be an error: known=%v err=%v", known, err)
		}
	}
	// Another namespace is another catalog.
	if _, known, err := portal.GetSkillMarketplaceCatalog(ctx, "other"); err != nil || known {
		t.Fatalf("another namespace: known=%v err=%v", known, err)
	}
}
