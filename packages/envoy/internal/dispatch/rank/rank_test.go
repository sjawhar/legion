package rank

import (
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

func TestBetweenSupportsRepeatedHeadAndTailInsertion(t *testing.T) {
	first := Between("", "")
	for range 1_024 {
		before := Between("", first)
		if !(before < first) {
			t.Fatalf("head insertion %q must sort before %q", before, first)
		}
		first = before
	}

	last := Between("", "")
	for range 1_024 {
		after := Between(last, "")
		if !(last < after) {
			t.Fatalf("tail insertion %q must sort after %q", after, last)
		}
		last = after
	}
}

func TestBetweenRepeatedMidpointsStayOrderedAndAvoidSmallestTrailingDigit(t *testing.T) {
	const lower, upper = "U", "V"
	previous := lower
	for index := range 200 {
		got := Between(previous, upper)
		if !(previous < got && got < upper) {
			t.Fatalf("midpoint %d = %q, want strictly between %q and %q", index, got, previous, upper)
		}
		if len(got) <= len(previous) {
			t.Fatalf("midpoint %d = %q must grow past %q", index, got, previous)
		}
		if got[len(got)-1] == alphabet[0] {
			t.Fatalf("midpoint %d = %q ends in the smallest alphabet digit", index, got)
		}
		previous = got
	}
}
