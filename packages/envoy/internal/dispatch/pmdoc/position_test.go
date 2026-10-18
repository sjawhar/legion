package pmdoc

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf16"
)

// Characters outside the Basic Multilingual Plane whose UTF-16 pairs agree on one unit: pairA and
// pairB share a high surrogate (D83D), pairC and pairD share a low one (DE0A) under different high
// ones (D83D, D83E).
const (
	pairA = "\U0001F4CA" // D83D DCCA
	pairB = "\U0001F4CB" // D83D DCCB
	pairC = "\U0001F60A" // D83D DE0A
	pairD = "\U0001FA0A" // D83E DE0A
)

// simpleDiff's span replaces whole characters: applied to current's UTF-16 units it yields want,
// with neither end of it inside a surrogate pair, over every pair of texts of up to three
// characters drawn from the pairs above, an ASCII character and a non-ASCII character of one UTF-16
// unit.
func TestSimpleDiffNeverCutsAPair(t *testing.T) {
	alphabet := []string{pairA, pairB, pairC, pairD, "x", "\u26A1"}
	var texts []string
	var build func(prefix string, depth int)
	build = func(prefix string, depth int) {
		texts = append(texts, prefix)
		if depth == 0 {
			return
		}
		for _, next := range alphabet {
			build(prefix+next, depth-1)
		}
	}
	build("", 3)

	for _, current := range texts {
		for _, want := range texts {
			index, remove, insert := simpleDiff(current, want)
			left := utf16.Encode([]rune(current))
			if index < 0 || remove < 0 || index+remove > len(left) {
				t.Fatalf("simpleDiff(%q, %q) = %d, %d, %q: outside the %d units of the text", current, want, index, remove, insert, len(left))
			}
			if splitsPair(left, index) || splitsPair(left, index+remove) {
				t.Fatalf("simpleDiff(%q, %q) = %d, %d, %q: the span cuts a surrogate pair", current, want, index, remove, insert)
			}
			if strings.ContainsRune(insert, '\uFFFD') {
				t.Fatalf("simpleDiff(%q, %q) inserts %q, which holds U+FFFD", current, want, insert)
			}
			units := append(append(append([]uint16{}, left[:index]...), utf16.Encode([]rune(insert))...), left[index+remove:]...)
			if got := string(utf16.Decode(units)); got != want {
				t.Fatalf("simpleDiff(%q, %q) = %d, %d, %q, which turns the text into %q", current, want, index, remove, insert, got)
			}
		}
	}
}

// The diff still keeps what both texts share: only the characters they disagree on are rewritten.
func TestSimpleDiffKeepsWhatTheTextsShare(t *testing.T) {
	for name, test := range map[string]struct {
		current, want string
		index, remove int
		insert        string
	}{
		"equal":                        {current: "a" + pairA + "b", want: "a" + pairA + "b", index: 4, remove: 0},
		"a character in the middle":    {current: "a" + pairA + "b", want: "a" + pairB + "b", index: 1, remove: 2, insert: pairB},
		"a shared high surrogate":      {current: pairA, want: pairB, index: 0, remove: 2, insert: pairB},
		"a shared low surrogate":       {current: pairC, want: pairD, index: 0, remove: 2, insert: pairD},
		"a pair after a shared prefix": {current: "ab" + pairA, want: "ab" + pairB, index: 2, remove: 2, insert: pairB},
		"a pair before a shared tail":  {current: pairC + "ab", want: pairD + "ab", index: 0, remove: 2, insert: pairD},
		"a pair added":                 {current: "ab", want: "a" + pairA + "b", index: 1, remove: 0, insert: pairA},
		"a pair removed":               {current: "a" + pairA + "b", want: "ab", index: 1, remove: 2},
	} {
		t.Run(name, func(t *testing.T) {
			index, remove, insert := simpleDiff(test.current, test.want)
			if index != test.index || remove != test.remove || insert != test.insert {
				t.Fatalf("simpleDiff(%q, %q) = %d, %d, %q, want %d, %d, %q", test.current, test.want, index, remove, insert, test.index, test.remove, test.insert)
			}
		})
	}
}

// A splice cuts a text node at the edges of its range, which fall between characters. An edge
// inside a pair is a bug of this package's, not a fault in the caller's write, so it is an error
// that is not ErrSchema (a verdict on the write, answered 400): half a character cannot be kept, and
// a splice around it would write the other half twice or not at all.
func TestSlice16RefusesARangeInsideAPair(t *testing.T) {
	value := "a" + pairA + "b" // a, D83D, DCCA, b
	for name, test := range map[string]struct {
		from, to int
		want     string
	}{
		"the whole text":      {from: 0, to: 4, want: value},
		"the pair":            {from: 1, to: 3, want: pairA},
		"before the pair":     {from: 0, to: 1, want: "a"},
		"after the pair":      {from: 3, to: 4, want: "b"},
		"an empty cut beside": {from: 3, to: 3, want: ""},
	} {
		if got, err := slice16(value, test.from, test.to); err != nil || got != test.want {
			t.Errorf("%s: slice16(%q, %d, %d) = %q, %v, want %q", name, value, test.from, test.to, got, err, test.want)
		}
	}
	for name, edges := range map[string][2]int{
		"a start inside the pair": {2, 4},
		"an end inside the pair":  {0, 2},
		"an empty cut inside":     {2, 2},
	} {
		got, err := slice16(value, edges[0], edges[1])
		if err == nil || errors.Is(err, ErrSchema) {
			t.Errorf("%s: slice16(%q, %d, %d) = %q, %v, want an error that is not ErrSchema", name, value, edges[0], edges[1], got, err)
		}
	}
}

// A quote's context is cut at positions in UTF-16 units; a cut inside a pair keeps the whole pair.
func TestContext16KeepsEveryCharacterItTouchesWhole(t *testing.T) {
	value := "a" + pairA + "b" // a, D83D, DCCA, b
	for _, test := range []struct {
		from, to int
		want     string
	}{
		{0, 4, value},
		{1, 3, pairA},
		{2, 4, pairA + "b"},
		{1, 2, pairA},
		{0, 2, "a" + pairA},
		{3, 4, "b"},
	} {
		if got := context16(value, test.from, test.to); got != test.want {
			t.Errorf("context16(%q, %d, %d) = %q, want %q", value, test.from, test.to, got, test.want)
		}
	}
}
