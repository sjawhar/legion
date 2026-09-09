package text

import (
	"errors"
	"reflect"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func TestResolveUsesUTF16Offsets(t *testing.T) {
	from, to, err := Resolve("😀 The quick brown fox", "brown", nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if from != 13 || to != 18 {
		t.Fatalf("Resolve offsets = %d,%d; want 13,18", from, to)
	}
	if got := Slice16("😀 The quick brown fox", from, to); got != "brown" {
		t.Fatalf("Slice16 resolved range = %q; want brown", got)
	}
}

func TestResolveReportsEveryAmbiguousCandidate(t *testing.T) {
	_, _, err := Resolve("the quick the fox", "the", nil)
	var ambiguous *ErrTargetAmbiguous
	if !errors.As(err, &ambiguous) {
		t.Fatalf("Resolve error = %v; want ErrTargetAmbiguous", err)
	}
	want := []Candidate{
		{From: 0, To: 3, Context: "the quick the fox"},
		{From: 10, To: 13, Context: "the quick the fox"},
	}
	if !reflect.DeepEqual(ambiguous.Candidates, want) {
		t.Fatalf("candidates = %#v; want %#v", ambiguous.Candidates, want)
	}
}

func TestResolveUsesZeroBasedOccurrence(t *testing.T) {
	occurrence := 1
	from, to, err := Resolve("the quick the fox", "the", &occurrence)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if from != 10 || to != 13 {
		t.Fatalf("Resolve occurrence one = %d,%d; want 10,13", from, to)
	}
}

func TestReresolveUsesNearestExactThenNormalizedWhitespace(t *testing.T) {
	exact := Reresolve("the red fox and the red fox", model.Anchor{Quote: "red", From: 18, To: 21})
	if exact.From != 20 || exact.To != 23 || exact.Orphaned {
		t.Fatalf("nearest exact anchor = %#v; want range 20,23 and not orphaned", exact)
	}

	normalized := Reresolve("the quick\n\tbrown fox", model.Anchor{Quote: "quick brown", From: 4, To: 15})
	if normalized.From != 4 || normalized.To != 16 || normalized.Orphaned {
		t.Fatalf("normalized anchor = %#v; want range 4,16 and not orphaned", normalized)
	}

	normalizedNearest := Reresolve("red\tfox 😀😀😀 red\nfox", model.Anchor{Quote: "red fox", From: 7, To: 14})
	if normalizedNearest.From != 0 || normalizedNearest.To != 7 || normalizedNearest.Orphaned {
		t.Fatalf("normalized UTF-16 nearest anchor = %#v; want range 0,7 and not orphaned", normalizedNearest)
	}

	orphaned := Reresolve("the red fox", model.Anchor{Quote: "brown", From: 4, To: 9})
	if !orphaned.Orphaned || orphaned.From != 4 || orphaned.To != 9 {
		t.Fatalf("missing quote anchor = %#v; want original orphaned anchor", orphaned)
	}
}

func TestLenAndSlice16(t *testing.T) {
	const value = "a😀b"
	if got := Len16(value); got != 4 {
		t.Fatalf("Len16 = %d; want 4", got)
	}
	if got := Slice16(value, 1, 3); got != "😀" {
		t.Fatalf("Slice16 = %q; want 😀", got)
	}
}
