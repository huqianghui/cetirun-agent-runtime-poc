// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package memory

import (
	"context"
	"math"
	"sort"
	"strings"
	"unicode"
)

// Text search ranks notes that share words with the query when no title,
// alias or keyword equals a query term. It reads only notes already in the
// bounded corpus and keeps nothing on disk, so a forgotten note leaves no
// searchable copy behind.

var stopWords = map[string]bool{}

// Frequent function characters are not searched on their own. They still
// count inside the character pairs around them.
var stopCharacters = map[rune]bool{}

func init() {
	for _, w := range strings.Fields(`a an and are as at be been but by can could did do does for from had has have how i if in into is it its me my of on or our so than that the their them then there these they this those to us was we were what when where which who whom why will with would you your about any some just also very`) {
		stopWords[w] = true
	}
	for _, r := range "的了着吗吧呢啊呀之和与及或也就都而是在这那个们一我你他她它은는이가을를의에도와과로" {
		stopCharacters[r] = true
	}
}

// latinFold maps lower-case Latin letters with diacritics to their base
// letters, so that "café" and "cafe" share a search key.
var latinFold = func() map[rune]string {
	fold := map[rune]string{}
	for base, letters := range map[string]string{
		"a": "àáâãäåāăą", "ae": "æ", "c": "çćĉċč", "d": "ďđð", "e": "èéêëēĕėęě", "g": "ĝğġģ", "h": "ĥħ",
		"i": "ìíîïĩīĭįı", "ij": "ĳ", "j": "ĵ", "k": "ķ", "l": "ĺļľŀł", "n": "ñńņňŉŋ", "o": "òóôõöøōŏő",
		"oe": "œ", "r": "ŕŗř", "s": "śŝşšſ", "ss": "ß", "t": "ţťŧ", "th": "þ", "u": "ùúûüũūŭůűų",
		"w": "ŵ", "y": "ýÿŷ", "z": "źżž",
	} {
		for _, r := range letters {
			fold[r] = base
		}
	}
	return fold
}()

// Search classes of a rune.
const (
	separator = iota
	wordRune
	cjkRune    // Han, hiragana and katakana: no spaces between words
	hangulRune // Korean: particles attach to the word
)

func runeClass(r rune) int {
	switch {
	case unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || r == 'ー':
		return cjkRune
	case unicode.Is(unicode.Hangul, r):
		return hangulRune
	case unicode.IsLetter(r) || unicode.IsDigit(r):
		return wordRune
	}
	return separator
}

// pieces splits a run of CJK or Hangul characters into overlapping pairs,
// plus single Han characters and Hangul syllables. A single kana is mostly an
// inflection or particle and is only searched within its pairs.
func pieces(run []rune) []string {
	out := []string{}
	for i, r := range run {
		single := (unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hangul, r)) && !stopCharacters[r]
		if single {
			out = append(out, string(r))
		}
		if i+1 < len(run) {
			out = append(out, string(run[i:i+2]))
		}
	}
	return out
}

// words splits text into lower-case search words. Letter and digit runs fold
// full-width forms and Latin diacritics, drop combining marks and fold common
// English inflections; function words are dropped. Text written without spaces
// (Chinese, Japanese) and Korean is searched by single characters and
// overlapping character pairs.
func words(text string) []string {
	out := []string{}
	var run []rune
	class := separator
	flush := func() {
		if len(run) == 0 {
			return
		}
		if class == cjkRune || class == hangulRune {
			out = append(out, pieces(run)...)
		} else if w := string(run); !stopWords[w] && (len(run) > 1 || unicode.IsDigit(run[0])) {
			out = append(out, stem(w))
		}
		run, class = run[:0], separator
	}
	for _, r := range strings.ToLower(text) {
		if r >= 0xFF01 && r <= 0xFF5E {
			r -= 0xFEE0 // full-width ASCII
		}
		if unicode.Is(unicode.Mn, r) {
			continue // composed and decomposed forms match
		}
		if unicode.Is(unicode.Mc, r) && class != separator {
			run = append(run, r)
			continue
		}
		next := runeClass(r)
		if next != class {
			flush()
		}
		if next == separator {
			continue
		}
		class = next
		if base, ok := latinFold[r]; ok {
			run = append(run, []rune(base)...)
		} else {
			run = append(run, r)
		}
	}
	flush()
	return out
}

type textHit struct {
	item    corpusNote
	score   float64
	matched []string
}

// Field weights make a word in a title or alias count more than the same word
// in the body.
var fieldWeights = []float64{3, 2.5, 2, 1}

func noteFields(n Note) [4][]string {
	var fields [4][]string
	fields[0] = words(n.Title)
	for _, alias := range n.Aliases {
		fields[1] = append(fields[1], words(alias)...)
	}
	for _, tag := range append(append([]string{}, n.Keywords...), n.Tags...) {
		fields[2] = append(fields[2], words(tag)...)
	}
	fields[3] = words(n.Body)
	return fields
}

// rankText scores in-scope notes with BM25 over weighted fields. A note must
// share at least one query word. Ties fall back to the exact-stage order:
// scope specificity, evidence recency, then ID. It stops when ctx ends.
func rankText(ctx context.Context, corpus []corpusNote, query []string, scope Scope) ([]textHit, int, error) {
	wanted := map[string]bool{}
	for _, term := range query {
		for _, w := range words(term) {
			wanted[w] = true
		}
	}
	if len(wanted) == 0 {
		return nil, 0, nil
	}
	type doc struct {
		item   corpusNote
		tf     map[string]float64
		length float64
	}
	docs := []doc{}
	df := map[string]int{}
	total, work := 0.0, 0
	for _, item := range corpus {
		if err := ctx.Err(); err != nil {
			return nil, work, err
		}
		if !scopeMatches(item.Note.Scope, scope) {
			continue
		}
		d := doc{item: item, tf: map[string]float64{}}
		for i, field := range noteFields(item.Note) {
			for _, w := range field {
				work++
				d.length += fieldWeights[i]
				if wanted[w] {
					d.tf[w] += fieldWeights[i]
				}
			}
		}
		for w := range d.tf {
			df[w]++
		}
		total += d.length
		docs = append(docs, d)
	}
	if len(docs) == 0 {
		return nil, work, nil
	}
	const k1, b = 1.2, 0.75
	average := total / float64(len(docs))
	if average == 0 {
		average = 1
	}
	hits := []textHit{}
	for _, d := range docs {
		if len(d.tf) == 0 {
			continue
		}
		hit := textHit{item: d.item}
		for w := range d.tf {
			hit.matched = append(hit.matched, w)
		}
		// Summing in a fixed word order gives notes with equal statistics equal
		// scores, so they reach the tie-break instead of differing by rounding.
		sort.Strings(hit.matched)
		for _, w := range hit.matched {
			tf := d.tf[w]
			idf := math.Log(1 + (float64(len(docs))-float64(df[w])+0.5)/(float64(df[w])+0.5))
			hit.score += idf * tf * (k1 + 1) / (tf + k1*(1-b+b*d.length/average))
		}
		hits = append(hits, hit)
	}
	sort.SliceStable(hits, func(i, j int) bool {
		a, c := hits[i], hits[j]
		if a.score != c.score {
			return a.score > c.score
		}
		if len(a.item.Note.Scope) != len(c.item.Note.Scope) {
			return len(a.item.Note.Scope) > len(c.item.Note.Scope)
		}
		if evidenceTime(a.item.Note) != evidenceTime(c.item.Note) {
			return evidenceTime(a.item.Note) > evidenceTime(c.item.Note)
		}
		return a.item.Ref.ID < c.item.Ref.ID
	})
	return hits, work, nil
}
