// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package memory

import "strings"

// maxStemLength leaves longer tokens unstemmed. They are identifiers or noise
// rather than inflected words, still match themselves exactly, and stay cheap.
const maxStemLength = 40

// stem applies the Porter (1980) suffix-stripping algorithm to a lower-case
// ASCII word, so that forms such as bikes/bike, booked/booking/book and
// hopefulness/hopeful share one search key. Other words are returned as is.
// Keys need not be dictionary words: queries and notes use the same function.
func stem(w string) string {
	if len(w) <= 2 || len(w) > maxStemLength {
		return w
	}
	for i := 0; i < len(w); i++ {
		if w[i] < 'a' || w[i] > 'z' {
			return w
		}
	}
	b := []byte(w)
	b = step1a(b)
	b = step1b(b)
	b = step1c(b)
	b = step2(b)
	b = step3(b)
	b = step4(b)
	b = step5(b)
	return string(b)
}

// consonants classifies every letter in one pass. A y is a consonant at the
// start of a word or after a vowel.
func consonants(b []byte) []bool {
	c := make([]bool, len(b))
	for i, ch := range b {
		switch ch {
		case 'a', 'e', 'i', 'o', 'u':
		case 'y':
			c[i] = i == 0 || !c[i-1]
		default:
			c[i] = true
		}
	}
	return c
}

// measure counts VC sequences in b, the m of [C](VC)^m[V].
func measure(b []byte) int {
	c := consonants(b)
	m, i, n := 0, 0, len(b)
	for i < n && c[i] {
		i++
	}
	for i < n {
		for i < n && !c[i] {
			i++
		}
		if i >= n {
			break
		}
		for i < n && c[i] {
			i++
		}
		m++
	}
	return m
}

func containsVowel(b []byte) bool {
	for _, consonant := range consonants(b) {
		if !consonant {
			return true
		}
	}
	return false
}

func doubleConsonant(b []byte) bool {
	n := len(b)
	return n >= 2 && b[n-1] == b[n-2] && consonants(b)[n-1]
}

// cvc reports a consonant-vowel-consonant ending whose last letter is not
// w, x or y, as in hop or fil.
func cvc(b []byte) bool {
	n := len(b)
	if n < 3 {
		return false
	}
	c := consonants(b)
	if !c[n-1] || c[n-2] || !c[n-3] {
		return false
	}
	return b[n-1] != 'w' && b[n-1] != 'x' && b[n-1] != 'y'
}

func hasSuffix(b []byte, s string) bool { return strings.HasSuffix(string(b), s) }

// replace swaps suffix s for r when the remaining stem has measure > m.
func replace(b []byte, s, r string, m int) ([]byte, bool) {
	if !hasSuffix(b, s) {
		return b, false
	}
	base := b[:len(b)-len(s)]
	if measure(base) > m {
		return append(base[:len(base):len(base)], r...), true
	}
	return b, true
}

func step1a(b []byte) []byte {
	switch {
	case hasSuffix(b, "sses"), hasSuffix(b, "ies"):
		return b[:len(b)-2]
	case hasSuffix(b, "ss"):
		return b
	case hasSuffix(b, "s"):
		return b[:len(b)-1]
	}
	return b
}

func step1b(b []byte) []byte {
	if hasSuffix(b, "eed") {
		if measure(b[:len(b)-3]) > 0 {
			return b[:len(b)-1]
		}
		return b
	}
	for _, s := range []string{"ed", "ing"} {
		if !hasSuffix(b, s) || !containsVowel(b[:len(b)-len(s)]) {
			continue
		}
		b = b[:len(b)-len(s)]
		switch {
		case hasSuffix(b, "at"), hasSuffix(b, "bl"), hasSuffix(b, "iz"):
			return append(b[:len(b):len(b)], 'e')
		case doubleConsonant(b) && !hasSuffix(b, "l") && !hasSuffix(b, "s") && !hasSuffix(b, "z"):
			return b[:len(b)-1]
		case measure(b) == 1 && cvc(b):
			return append(b[:len(b):len(b)], 'e')
		}
		return b
	}
	return b
}

func step1c(b []byte) []byte {
	if hasSuffix(b, "y") && containsVowel(b[:len(b)-1]) {
		out := append([]byte{}, b...)
		out[len(out)-1] = 'i'
		return out
	}
	return b
}

var step2Rules = [][2]string{
	{"ational", "ate"}, {"tional", "tion"}, {"enci", "ence"}, {"anci", "ance"}, {"izer", "ize"},
	{"abli", "able"}, {"alli", "al"}, {"entli", "ent"}, {"eli", "e"}, {"ousli", "ous"},
	{"ization", "ize"}, {"ation", "ate"}, {"ator", "ate"}, {"alism", "al"}, {"iveness", "ive"},
	{"fulness", "ful"}, {"ousness", "ous"}, {"aliti", "al"}, {"iviti", "ive"}, {"biliti", "ble"},
}

var step3Rules = [][2]string{
	{"icate", "ic"}, {"ative", ""}, {"alize", "al"}, {"iciti", "ic"}, {"ical", "ic"}, {"ful", ""}, {"ness", ""},
}

func step2(b []byte) []byte {
	for _, rule := range step2Rules {
		if out, matched := replace(b, rule[0], rule[1], 0); matched {
			return out
		}
	}
	return b
}

func step3(b []byte) []byte {
	for _, rule := range step3Rules {
		if out, matched := replace(b, rule[0], rule[1], 0); matched {
			return out
		}
	}
	return b
}

var step4Suffixes = []string{"al", "ance", "ence", "er", "ic", "able", "ible", "ant", "ement", "ment", "ent", "ion", "ou", "ism", "ate", "iti", "ous", "ive", "ize"}

func step4(b []byte) []byte {
	for _, s := range step4Suffixes {
		if !hasSuffix(b, s) {
			continue
		}
		base := b[:len(b)-len(s)]
		if measure(base) <= 1 {
			return b
		}
		if s == "ion" && !hasSuffix(base, "s") && !hasSuffix(base, "t") {
			return b
		}
		return base
	}
	return b
}

func step5(b []byte) []byte {
	if hasSuffix(b, "e") {
		base := b[:len(b)-1]
		if m := measure(base); m > 1 || m == 1 && !cvc(base) {
			b = base
		}
	}
	if measure(b) > 1 && doubleConsonant(b) && hasSuffix(b, "l") {
		b = b[:len(b)-1]
	}
	return b
}
