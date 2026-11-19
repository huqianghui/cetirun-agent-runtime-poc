// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package memory

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestStemMatchesPorterReference(t *testing.T) {
	// Expected values from Porter's published vocabulary/output lists.
	for word, want := range map[string]string{
		"caresses": "caress", "ponies": "poni", "ties": "ti", "cats": "cat", "feed": "feed", "agreed": "agre",
		"plastered": "plaster", "motoring": "motor", "sing": "sing", "conflated": "conflat", "troubled": "troubl",
		"sized": "size", "hopping": "hop", "tanned": "tan", "falling": "fall", "hissing": "hiss", "fizzed": "fizz",
		"failing": "fail", "filing": "file", "happy": "happi", "relational": "relat", "conditional": "condit",
		"rational": "ration", "valenci": "valenc", "digitizer": "digit", "conformabli": "conform", "radicalli": "radic",
		"differentli": "differ", "vileli": "vile", "analogousli": "analog", "vietnamization": "vietnam",
		"predication": "predic", "operator": "oper", "feudalism": "feudal", "decisiveness": "decis",
		"hopefulness": "hope", "callousness": "callous", "formaliti": "formal", "sensitiviti": "sensit",
		"sensibiliti": "sensibl", "triplicate": "triplic", "formative": "form", "formalize": "formal",
		"electriciti": "electr", "electrical": "electr", "hopeful": "hope", "goodness": "good", "revival": "reviv",
		"allowance": "allow", "inference": "infer", "airliner": "airlin", "gyroscopic": "gyroscop",
		"adjustable": "adjust", "defensible": "defens", "irritant": "irrit", "replacement": "replac",
		"adjustment": "adjust", "dependent": "depend", "adoption": "adopt", "homologou": "homolog",
		"communism": "commun", "activate": "activ", "angulariti": "angular", "homologous": "homolog",
		"effective": "effect", "bowdlerize": "bowdler", "probate": "probat", "rate": "rate", "cease": "ceas",
		"controll": "control", "roll": "roll", "generalizations": "gener", "oscillators": "oscil",
		"bikes": "bike", "booking": "book", "booked": "book",
	} {
		if got := stem(word); got != want {
			t.Errorf("stem(%q) = %q, want %q", word, got, want)
		}
	}
	for _, word := range []string{"a1", "über", "x", "id"} {
		if stem(word) != word {
			t.Errorf("stem changed non-applicable word %q", word)
		}
	}
}

func TestWordsSplitsFoldsAndFiltersText(t *testing.T) {
	got := words("The user's 2 spare Bikes were stored at Rootcraft.example; 记住偏好")
	want := []string{"user", "2", "spare", "bike", "store", "rootcraft", "exampl", "记", "记住", "住", "住偏", "偏", "偏好", "好"}
	if !slices.Equal(got, want) {
		t.Fatalf("words = %q, want %q", got, want)
	}
	for text, want := range map[string][]string{
		"我的猫":              {"我的", "的猫", "猫"},
		"ブラックコーヒーが好き":      {"ブラ", "ラッ", "ック", "クコ", "コー", "ーヒ", "ヒー", "ーが", "が好", "好", "好き"},
		"커피를":              {"커", "커피", "피", "피를"},
		"Café ＶＰＮ Ærø":     {"cafe", "vpn", "aero"},
		"cafe\u0301 naïve": {"cafe", "naiv"},
	} {
		if got := words(text); !slices.Equal(got, want) {
			t.Errorf("words(%q) = %q, want %q", text, got, want)
		}
	}
}

func TestTextSearchFindsWordsInTextWithoutSpaces(t *testing.T) {
	f := newFixture(t)
	for title, body := range map[string]string{
		"咖啡偏好":    "我每天早上喝美式咖啡，不加糖，也不喝茶。",
		"宠物信息":    "家里养了一只猫，叫小白。",
		"飲み物の好み":  "ブラックコーヒーが好きです。",
		"음료 취향":   "아침에 커피를 마십니다.",
		"Lunch":   "The team meets at the café on Fridays.",
		"Network": "Use the office ＶＰＮ for the build server.",
	} {
		note := testNote(title)
		note.Body = body
		f.note(t, note)
	}
	for query, want := range map[string]string{
		"茶": "咖啡偏好", "猫": "宠物信息", "小白": "宠物信息", "コーヒー": "飲み物の好み", "커피": "음료 취향",
		"cafe": "Lunch", "cafe\u0301": "Lunch", "VPN": "Network", "ＶＰＮ": "Network",
	} {
		result := f.call(t, "query", Query{QueryTerms: []string{query}}).Data.(QueryResult)
		if result.Stage != "text" || len(result.Hits) == 0 || result.Hits[0].Note.Title != want {
			t.Errorf("%q: stage=%s hits=%v, want %q first", query, result.Stage, titles(result.Hits), want)
		}
	}
	if result := f.call(t, "query", Query{QueryTerms: []string{"的"}}).Data.(QueryResult); len(result.Hits) != 0 {
		t.Errorf("a function character alone matched: %v", titles(result.Hits))
	}
}

func TestQueryRanksSharedWordsWhenNoExactField(t *testing.T) {
	f := newFixture(t)
	bikes := testNote("Bike storage limits")
	bikes.Body = "The owner asked how many spare bikes fit in the hallway."
	bikes.Keywords = []string{"bikes"}
	f.note(t, bikes)
	resource := testNote("Gardening resources")
	resource.Body = "I recommended rootcraft.example for soil testing; the owner had not tried it yet."
	f.note(t, resource)
	other := testNote("Travel insurance")
	other.Body = "Annual policy renewed in March."
	f.note(t, other)
	outside := testNote("Second bike in another project")
	outside.Scope["topic"] = "other"
	outside.Body = "A second bike was requested."
	f.note(t, outside)

	result := f.call(t, "query", Query{QueryTerms: []string{"second bike"}}).Data.(QueryResult)
	if result.Stage != "text" || len(result.Hits) != 1 || result.Hits[0].Note.Title != "Bike storage limits" || result.TextMatches != 1 {
		t.Fatalf("stage=%s textMatches=%d hits=%v", result.Stage, result.TextMatches, titles(result.Hits))
	}
	if !slices.Equal(result.Hits[0].MatchedWords, []string{"bike"}) || result.Partial != nil {
		t.Fatalf("matched=%v partial=%+v", result.Hits[0].MatchedWords, result.Partial)
	}
	result = f.call(t, "query", Query{QueryTerms: []string{"gardener website"}}).Data.(QueryResult)
	if result.Stage != "text" || len(result.Hits) != 1 || result.Hits[0].Note.Title != "Gardening resources" {
		t.Fatalf("resource lookup: stage=%s hits=%v", result.Stage, titles(result.Hits))
	}
	// An exact keyword still wins and keeps the earlier stage semantics.
	result = f.call(t, "query", Query{QueryTerms: []string{"Bikes"}}).Data.(QueryResult)
	if result.Stage != "keywords" || len(result.Hits) != 1 || result.TextMatches != 0 || result.Hits[0].MatchedWords != nil {
		t.Fatalf("exact keyword: stage=%s hits=%v textMatches=%d", result.Stage, titles(result.Hits), result.TextMatches)
	}
	for _, query := range []string{"the and of", "absent"} {
		result = f.call(t, "query", Query{QueryTerms: []string{query}}).Data.(QueryResult)
		if result.Stage != "none" || len(result.Hits) != 0 || result.TextMatches != 0 {
			t.Fatalf("%q: stage=%s hits=%v", query, result.Stage, titles(result.Hits))
		}
	}
}

func TestQueryTextOrderAndLimit(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 8; i++ {
		note := testNote(fmt.Sprintf("garden note %d", i))
		note.Body = strings.Repeat("tomato ", i+1) + "seedlings were planted."
		f.note(t, note)
	}
	result := f.call(t, "query", Query{QueryTerms: []string{"tomatoes"}}).Data.(QueryResult)
	if result.Stage != "text" || len(result.Hits) != Baseline().MaxResults || result.TextMatches != 8 || result.Partial != nil {
		t.Fatalf("stage=%s hits=%d textMatches=%d partial=%+v", result.Stage, len(result.Hits), result.TextMatches, result.Partial)
	}
	if result.Hits[0].Note.Title != "garden note 7" {
		t.Fatalf("highest term frequency should rank first: %v", titles(result.Hits))
	}
	result = f.call(t, "query", Query{QueryTerms: []string{"tomato"}, Limit: 8}).Data.(QueryResult)
	if len(result.Hits) != 8 {
		t.Fatalf("limit 8 returned %d hits", len(result.Hits))
	}
	for _, limit := range []int{-1, MaxQueryLimit + 1} {
		r := f.request(t, Query{QueryTerms: []string{"tomato"}, Limit: limit})
		if response, code := f.store.Execute(context.Background(), "query", r); code != 2 {
			t.Fatalf("limit %d accepted: %+v", limit, response)
		}
	}
}

func TestQueryTextRespectsDelegation(t *testing.T) {
	f := newFixture(t)
	allowed := testNote("Allowed packing list")
	allowed.Body = "Pack the blue tent."
	allowedRef := f.note(t, allowed)
	hidden := testNote("Hidden packing list")
	hidden.Body = "Pack the blue tent and the stove."
	f.note(t, hidden)
	r := f.request(t, Query{QueryTerms: []string{"tent"}})
	r.RequestContext.Delegation = &Delegation{ParentRequestID: "parent-fixture", Purpose: "scoped lookup", AllowedRefs: []Ref{allowedRef}, NoCapture: true}
	response, code := f.store.Execute(context.Background(), "query", r)
	if code != 0 {
		t.Fatal(response.Reason)
	}
	result := response.Data.(QueryResult)
	if len(result.Hits) != 1 || result.Hits[0].Ref != allowedRef || result.TextMatches != 1 {
		t.Fatalf("delegated text search leaked: %v textMatches=%d", titles(result.Hits), result.TextMatches)
	}
}

func TestLargeStoreStaysSearchableAndReviewed(t *testing.T) {
	notes := []Note{}
	for i := 0; i < 600; i++ {
		note := testNote(fmt.Sprintf("bulk note %03d", i))
		notes = append(notes, note)
	}
	notes[599].Body = "The rare marker zephyrine appears only here."
	f, v := newQuerySnapshot(t, notes)
	target := v.m.Objects[fmt.Sprintf("note-%032x", 600)]
	conflict := testNote("later correction")
	conflict.Relations = []Relation{{Type: "contradicts", TargetRef: target, Reason: "newer measurement"}}
	if err := conflict.validate(); err != nil {
		t.Fatal(err)
	}
	conflict.ID, conflict.Revision, conflict.UpdatedAt = fmt.Sprintf("note-%032x", 601), 1, timestamp(f.store.now())
	if _, err := v.save(conflict.ID, 1, conflict); err != nil {
		t.Fatal(err)
	}
	r := f.request(t, Query{QueryTerms: []string{"zephyrine"}})
	result, err := f.store.query(context.Background(), v, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Hits) != 1 || result.Hits[0].Ref != target || result.Partial != nil {
		t.Fatalf("note beyond 256 records not found: hits=%v partial=%+v", titles(result.Hits), result.Partial)
	}
	if review := result.Hits[0].Review; !review.NeedsRecheck || review.Reason != "Conflicted" {
		t.Fatalf("conflict from a note beyond the old scan bound was missed: %+v", review)
	}
	plain, err := f.store.review(v, v.m.Objects[fmt.Sprintf("note-%032x", 1)], notes[0])
	if err != nil || plain.NeedsRecheck {
		t.Fatalf("large store marked an unrelated note for recheck: %+v err=%v", plain, err)
	}
}

func TestNoteScanCoversEveryRegisteredNote(t *testing.T) {
	ref := Ref{ID: fmt.Sprintf("note-%032x", 1), Revision: 1, Digest: "sha256:" + strings.Repeat("0", 64)}
	entry, _ := encode(map[string]Ref{ref.ID: ref})
	// A manifest object entry is the smallest per-note cost; views add more.
	if perNote := len(entry) - 2; MaxNoteScan*perNote < MaxManifest {
		t.Fatalf("a %d-byte manifest can register %d notes, more than the %d-note scan", MaxManifest, MaxManifest/perNote, MaxNoteScan)
	}
}

func titles(hits []Hit) []string {
	out := []string{}
	for _, hit := range hits {
		out = append(out, hit.Note.Title)
	}
	return out
}

func TestAdaptersCanRecordEveryHitOfALargeQuery(t *testing.T) {
	f := newFixture(t)
	b := nativeBinding(f)
	for i := 0; i < MaxQueryLimit+1; i++ {
		note := testNote(fmt.Sprintf("trip note %02d", i))
		note.Scope = Scope{"project": b.CWD}
		note.Body = "Packing list for the coastal walking trip."
		f.native(t, b, "tool", "capture", captureInput{Note: &note})
	}
	query := f.native(t, b, "tool", "query", Query{QueryTerms: []string{"walking trip"}, Limit: MaxQueryLimit}).Data.(QueryResult)
	if len(query.Hits) != MaxQueryLimit || query.Stage != "text" {
		t.Fatalf("hits=%d stage=%s", len(query.Hits), query.Stage)
	}
	refs := []Ref{}
	for _, hit := range query.Hits {
		refs = append(refs, hit.Ref)
	}
	f.native(t, b, "injected", "", map[string]any{"refs": refs})
	extra := f.native(t, b, "tool", "query", Query{QueryTerms: []string{"trip note 20"}}).Data.(QueryResult)
	refs = append(refs, extra.Hits[0].Ref)
	if out, code := f.store.NativeEvent(context.Background(), nativeEvent(b, "injected", "", map[string]any{"refs": refs}), "canonical"); code != 5 || out.Reason != "LimitReached" {
		t.Fatalf("injection above the query limit: code=%d reason=%s", code, out.Reason)
	}
}

func TestLongTokensStayCheapAndSearchable(t *testing.T) {
	long := strings.Repeat("y", 30000)
	if stem(long) != long {
		t.Fatal("a token longer than maxStemLength was stemmed")
	}
	f := newFixture(t)
	for i := 0; i < 8; i++ {
		note := testNote(fmt.Sprintf("long token %d", i))
		note.Body = long
		f.note(t, note)
	}
	identifier := "orderreference" + strings.Repeat("q", 50)
	holder := testNote("Reference holder")
	holder.Body = "Quote " + identifier + " when calling the shop."
	f.note(t, holder)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := time.Now()
	response, code := f.store.Execute(ctx, "query", f.request(t, Query{QueryTerms: []string{identifier}}))
	if code != 0 || time.Since(started) > 2*time.Second {
		t.Fatalf("query over long stored tokens: code=%d reason=%s elapsed=%v", code, response.Reason, time.Since(started))
	}
	if hits := response.Data.(QueryResult).Hits; len(hits) != 1 || hits[0].Note.Title != "Reference holder" {
		t.Fatalf("long identifier is not searchable: %v", titles(hits))
	}
}

func TestQueryStopsWhenTheContextEnds(t *testing.T) {
	f := newFixture(t)
	note := testNote("Tent checklist")
	note.Body = "Pack the blue tent."
	f.note(t, note)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	for name, ctx := range map[string]context.Context{"cancelled": cancelled, "expired": expired} {
		for _, q := range []Query{{QueryTerms: []string{"Tent checklist"}}, {QueryTerms: []string{"blue tent"}}} {
			response, code := f.store.Execute(ctx, "query", f.request(t, q))
			if code != 5 || response.OK || response.Reason != "Cancelled" {
				t.Fatalf("%s %v: code=%d ok=%v reason=%s", name, q.QueryTerms, code, response.OK, response.Reason)
			}
		}
	}
	_, v := newQuerySnapshot(t, []Note{note})
	corpus, _, err := v.corpus(f.request(t, Query{QueryTerms: []string{"blue tent"}}))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := rankText(cancelled, corpus, []string{"blue tent"}, Scope{"project": "fixture"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("word ranking ignored cancellation: %v", err)
	}
}

func TestTextRankingIsRepeatable(t *testing.T) {
	vocabulary := strings.Fields("alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima")
	notes := []Note{}
	for i := 0; i < 10; i++ {
		note := testNote("Same list")
		note.Body = strings.Join(vocabulary, " ")
		notes = append(notes, note)
	}
	// Background notes give each query word a different document frequency.
	for i := range vocabulary {
		note := testNote(fmt.Sprintf("Background %02d", i))
		note.Body = strings.Join(vocabulary[:i+1], " ")
		notes = append(notes, note)
	}
	f, v := newQuerySnapshot(t, notes)
	query := []string{strings.Join(vocabulary, " ")}
	corpus, _, err := v.corpus(f.request(t, Query{QueryTerms: query}))
	if err != nil {
		t.Fatal(err)
	}
	order := func() []string {
		hits, _, err := rankText(context.Background(), corpus, query, Scope{"project": "fixture"})
		if err != nil {
			t.Fatal(err)
		}
		return hitIDs(hits)
	}
	first := order()
	if want := fmt.Sprintf("note-%032x", 1); first[0] != want || !slices.IsSorted(first[:10]) {
		t.Fatalf("equal scores did not fall back to ID order: %v", first[:10])
	}
	for i := 0; i < 50; i++ {
		if next := order(); !slices.Equal(next, first) {
			t.Fatalf("run %d changed the order:\nfirst=%v\nnext=%v", i, first, next)
		}
	}
}

func TestTextTiesPreferScopeThenRecencyThenID(t *testing.T) {
	notes := []Note{}
	for i := 0; i < 4; i++ {
		note := testNote("Shared wording")
		note.Body = "Pottery class schedule."
		notes = append(notes, note)
	}
	notes[2].Scope = Scope{"project": "fixture", "topic": "studio"}
	newer := "2026-10-08T00:00:00Z"
	notes[3].Sources[0].ObservedAt = &newer
	f, v := newQuerySnapshot(t, notes)
	scope := Scope{"project": "fixture", "topic": "studio"}
	r := f.request(t, Query{QueryTerms: []string{"pottery schedule"}})
	r.RequestContext.Scope = scope
	corpus, _, err := v.corpus(r)
	if err != nil {
		t.Fatal(err)
	}
	hits, _, err := rankText(context.Background(), corpus, []string{"pottery schedule"}, scope)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{fmt.Sprintf("note-%032x", 3), fmt.Sprintf("note-%032x", 4), fmt.Sprintf("note-%032x", 1), fmt.Sprintf("note-%032x", 2)}
	if got := hitIDs(hits); !slices.Equal(got, want) {
		t.Fatalf("tie order = %v, want %v", got, want)
	}
}

func hitIDs(hits []textHit) []string {
	ids := []string{}
	for _, hit := range hits {
		ids = append(ids, hit.item.Ref.ID)
	}
	return ids
}
