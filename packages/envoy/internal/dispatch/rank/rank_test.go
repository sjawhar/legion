package rank

import (
	"math/rand/v2"
	"strings"
	"testing"
)

func TestBetweenProducesBase62KeyStrictlyBetweenBounds(t *testing.T) {
	tests := []struct {
		name       string
		prev, next string
	}{
		{name: "empty range", prev: "", next: ""},
		{name: "before first", prev: "", next: "U"},
		{name: "after last", prev: "U", next: ""},
		{name: "wide digit gap", prev: "A", next: "z"},
		{name: "adjacent digits", prev: "U", next: "V"},
		{name: "common prefix", prev: "Ua", next: "Ub"},
		{name: "prefix with zero", prev: "U", next: "U0U"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Between(tt.prev, tt.next)
			if got == "" {
				t.Fatal("Between returned an empty key")
			}
			if tt.prev != "" && !(tt.prev < got) {
				t.Fatalf("prev %q must sort before key %q", tt.prev, got)
			}
			if tt.next != "" && !(got < tt.next) {
				t.Fatalf("key %q must sort before next %q", got, tt.next)
			}
			for _, char := range got {
				if !strings.ContainsRune("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz", char) {
					t.Fatalf("key %q contains non-base62 character %q", got, char)
				}
			}
		})
	}
}

func TestBetweenSupportsRepeatedHeadInsertion(t *testing.T) {
	first := Between("", "")
	for range 1_024 {
		candidate := Between("", first)
		if !(candidate < first) {
			t.Fatalf("head insertion %q must sort before %q", candidate, first)
		}
		first = candidate
	}
}

func TestBetweenRepeatedMidpointsStayOrderedBoundedAndAvoidSmallestTrailingDigit(t *testing.T) {
	const lower, upper = "U", "V"
	previous := lower
	for index := range 200 {
		got := Between(previous, upper)
		if !(previous < got && got < upper) {
			t.Fatalf("midpoint %d = %q, want strictly between %q and %q", index, got, previous, upper)
		}
		// With adjacent bounds "U"/"V" this always takes the no-midpoint branch
		// (prev[:common+1] + after(prev[common+1:])); the sequence is UU, UV, …,
		// Uz, UzU, …, so len(got) = 2 + index/32 (integer division), the exact
		// bound after's growth rate gives.
		if maxLen := 2 + index/32; len(got) > maxLen {
			t.Fatalf("midpoint %d = %q has length %d, want at most %d", index, got, len(got), maxLen)
		}
		if got[len(got)-1] == alphabet[0] {
			t.Fatalf("midpoint %d = %q ends in the smallest alphabet digit", index, got)
		}
		previous = got
	}
}

func TestBetweenTenThousandSuccessiveAppendsStayOrderedAndBounded(t *testing.T) {
	// after grows a key by one digit only once every 32 appends (see after's
	// doc comment): a fresh digit runs "U" through "z" (32 values) before it
	// saturates and the next append rolls over into a new digit. So after n
	// appends, no key can exceed 1 + n/32 characters (the leading "+1" is the
	// first key itself, produced before any append) — the exact bound the
	// growth rate gives.
	const appends = 10_000
	const maxLen = 1 + appends/32

	last := Between("", "")
	for range appends {
		next := Between(last, "")
		if !(last < next) {
			t.Fatalf("append must sort strictly after previous key: %q vs %q", last, next)
		}
		if len(next) > maxLen {
			t.Fatalf("key %q has length %d, want at most %d after %d appends", next, len(next), maxLen, appends)
		}
		last = next
	}
}

func TestBetweenAppendAfterLongKeyReturnsShortKey(t *testing.T) {
	tests := []struct {
		name string
		prev string
		want string
	}{
		{
			// after increments the first digit ("U", index 30) and drops
			// everything after it, however long the tail is.
			name: "run of the fallback digit",
			prev: strings.Repeat("U", 794),
			want: "V",
		},
		{
			// The leading "z" digits are already at the alphabet's maximum, so
			// after skips them, increments the first digit below it ("U"), and
			// drops everything after that digit, including the trailing "z"s.
			name: "leading run of the alphabet's largest digit",
			prev: "zzzzUzzzz",
			want: "zzzzV",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Between(tt.prev, "")
			if got != tt.want {
				t.Fatalf("Between(%q, \"\") = %q, want %q", tt.prev, got, tt.want)
			}
			if !(tt.prev < got) {
				t.Fatalf("key %q must sort after %q", got, tt.prev)
			}
		})
	}
}

func TestBetweenRandomizedInsertionsStayOrderedAndValid(t *testing.T) {
	rng := rand.New(rand.NewPCG(20260927, 20260927))
	var list []string

	for insertion := range 5_000 {
		pos := rng.IntN(len(list) + 1)
		prev, next := "", ""
		if pos > 0 {
			prev = list[pos-1]
		}
		if pos < len(list) {
			next = list[pos]
		}
		key := Between(prev, next)

		list = append(list, "")
		copy(list[pos+1:], list[pos:])
		list[pos] = key

		for index, got := range list {
			if index > 0 && !(list[index-1] < got) {
				t.Fatalf("after insertion %d, list not strictly sorted at index %d: %q >= %q", insertion, index, list[index-1], got)
			}
			for _, char := range got {
				if !strings.ContainsRune(alphabet, char) {
					t.Fatalf("after insertion %d, key %q contains non-base62 character %q", insertion, got, char)
				}
			}
			if got[len(got)-1] == alphabet[0] {
				t.Fatalf("after insertion %d, key %q ends in the smallest alphabet digit", insertion, got)
			}
		}
	}
}
