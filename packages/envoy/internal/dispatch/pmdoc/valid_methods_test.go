package pmdoc

import (
	"fmt"
	"testing"
)

// The reads an edit makes over a tree it has already validated have one form, the method on
// ValidDocument. What is worth holding about them is what no other code says: that the zero value,
// which holds no tree, refuses with the words a read of a tree that is no document always gave,
// and that a quote finder made from a validated tree answers what a finder that validates for
// itself answers. The operations' own behaviour is held by the tests of this package that drive
// them through a plain tree (plain_tree_test.go), and base-against-head by docs' write corpus.

// validOf is tree as a ValidDocument, which every corpus document is.
func validOf(t *testing.T, tree *Node) ValidDocument {
	t.Helper()
	valid, err := ValidateDocument(tree, "test")
	if err != nil {
		t.Fatalf("ValidateDocument: %v", err)
	}
	return valid
}

// sameAnswer reports how two answers differ, or "" when they do not: the values as %#v prints
// them, and the errors by their text, since every caller serves an error's own words.
func sameAnswer(wantValue, gotValue any, wantErr, gotErr error) string {
	if (wantErr == nil) != (gotErr == nil) {
		return fmt.Sprintf("errors: want %v, got %v", wantErr, gotErr)
	}
	if wantErr != nil {
		if wantErr.Error() != gotErr.Error() {
			return fmt.Sprintf("errors: want %q, got %q", wantErr, gotErr)
		}
		return ""
	}
	if want, got := fmt.Sprintf("%#v", wantValue), fmt.Sprintf("%#v", gotValue); want != got {
		return fmt.Sprintf("values: want %s, got %s", want, got)
	}
	return ""
}

// quoteProbes are the quotes each document is searched for: its own first words, which match; a
// single space, which is ambiguous in most documents; text no document holds; and the empty quote.
func quoteProbes(tree *Node) []string {
	probes := []string{"", " ", "no document holds this text \u2014 zzqq", "the"}
	text := TextContent(tree)
	for _, width := range []int{5, 17, 40} {
		if len(text) > width {
			probes = append(probes, text[:width])
		}
	}
	return probes
}

// A finder over a validated tree skips the validation and answers what a finder that validates
// answers: the same range for a quote that matches, and the same refusal, with its candidates and
// its nearest blocks, for one that does not.
func TestValidDocumentQuoteFinderAnswersFindQuote(t *testing.T) {
	for _, document := range corpusMarkdown(t) {
		t.Run(document.name, func(t *testing.T) {
			tree, err := Parse(document.markdown)
			if err != nil {
				t.Fatal(err)
			}
			finder := validOf(t, tree).QuoteFinder()
			for _, quote := range quoteProbes(tree) {
				wantRange, wantErr := FindQuote(tree, quote, nil, nil)
				gotRange, gotErr := finder.Find(quote, nil, nil)
				if diff := sameAnswer(wantRange, gotRange, wantErr, gotErr); diff != "" {
					t.Fatalf("QuoteFinder().Find(%q) %s", quote, diff)
				}
			}
		})
	}
}

// The zero ValidDocument holds no tree, so every read of it refuses with the words a read of a
// tree that is no document gives, and none of them reaches a nil dereference.
func TestTheZeroValidDocumentWantsADocument(t *testing.T) {
	zero := ValidDocument{}
	for _, test := range []struct {
		name string
		got  func() error
	}{
		{"BlockIndexes", func() error { _, _, err := zero.BlockIndexes(Range{}); return err }},
		{"BlockBoundary", func() error { _, err := zero.BlockBoundary(Range{}, true); return err }},
		{"BlockRange", func() error { _, err := zero.BlockRange("b"); return err }},
		{"BlockDescendantIDs", func() error { _, err := zero.BlockDescendantIDs("b"); return err }},
		{"DeleteBlock", func() error { _, err := zero.DeleteBlock("b"); return err }},
		{"DeleteTextblock", func() error { _, _, err := zero.DeleteTextblock(Range{}); return err }},
		{"InsertTableRows", func() error {
			_, _, err := zero.InsertTableRows(Range{}, "| a |", true, NewWriteBudget())
			return err
		}},
		{"TableDescendantIDs", func() error { _, err := zero.TableDescendantIDs(); return err }},
	} {
		t.Run(test.name, func(t *testing.T) {
			want := wantsDocument(test.name)
			if diff := sameAnswer(nil, nil, want, test.got()); diff != "" {
				t.Fatalf("the zero ValidDocument's %s %s", test.name, diff)
			}
		})
	}
	// Splice has a shape of its own to read before any tree, so its zero-value refusal is that
	// shape's, not wantsDocument's.
	if _, err := zero.Splice(Range{}, &Node{Type: "doc"}); err == nil || !isSchemaRefusal(err, "invalid splice") {
		t.Fatalf("the zero ValidDocument's Splice = %v, want the invalid-splice refusal", err)
	}
	// The zero value's quote finder answers FindQuote's refusal for a nil document.
	_, wantErr := FindQuote(nil, "x", nil, nil)
	_, gotErr := zero.QuoteFinder().Find("x", nil, nil)
	if diff := sameAnswer(nil, nil, wantErr, gotErr); diff != "" {
		t.Fatalf("the zero ValidDocument's finder %s", diff)
	}
}

// isSchemaRefusal reports whether err is this package's schema refusal carrying reason.
func isSchemaRefusal(err error, reason string) bool {
	return err != nil && err.Error() == fmt.Sprintf("%v: %s", ErrSchema, reason)
}
