package pmdoc

import (
	"reflect"
	"slices"
	"testing"
)

// The renderer reads a run by CommonMark's flanking rules (flankingOnlyMarkdown) only where its
// read by the editor's rules judged a delimiter run otherwise (parseInlineWithDefinitions), so
// wherever that read reports no divergence the flanking-only read must be the same read: the same
// refusal, or the same nodes.
func FuzzFlankingDivergence(f *testing.F) {
	for _, seed := range []string{
		"\\**x",
		"b_*a**",
		"***a.****&#32;b*",
		"a_b*a_b*",
		"**[a](u)**_b_",
		"**a** b",
		"*a* _b_",
		"a[^a] *b*",
	} {
		f.Add(seed, false)
		f.Add(seed, true)
	}
	f.Fuzz(func(t *testing.T, markdown string, withLabel bool) {
		var labels []string
		if withLabel {
			labels = []string{"a"}
		}
		nodes, diverged, err := parseInlineWithDefinitions(markdown, labels, inlineMarkdown)
		if diverged {
			return
		}
		flanking, flankingDiverged, flankingErr := parseInlineWithDefinitions(markdown, labels, flankingOnlyMarkdown)
		if flankingDiverged {
			t.Fatalf("%q read by the flanking rules alone reports a divergence", markdown)
		}
		if (err == nil) != (flankingErr == nil) || err != nil && err.Error() != flankingErr.Error() {
			t.Fatalf("%q with labels %q, no divergence reported: read by the editor's rules %v, by the flanking rules %v", markdown, labels, err, flankingErr)
		}
		if err != nil {
			return
		}
		if got, want := inlineSignature(flanking), inlineSignature(nodes); !slices.Equal(got, want) {
			t.Fatalf("%q with labels %q, no divergence reported: read by the flanking rules as %q, by the editor's rules as %q", markdown, labels, got, want)
		}
		if !reflect.DeepEqual(flanking, nodes) {
			t.Fatalf("%q with labels %q, no divergence reported: the flanking rules' nodes differ from the editor's", markdown, labels)
		}
	})
}

// The divergence is reported only where a delimiter run is judged otherwise by the editor's rules:
// not for emphasis whose runs both rules judge alike, and for a run that opens or closes only
// beside another `*` or `_`.
func TestFlankingDivergenceIsReportedOnlyWhereTheRulesDiffer(t *testing.T) {
	for _, test := range []struct {
		markdown string
		diverged bool
	}{
		{"**bold** and *em* and _u_", false},
		{"b_*a**", true},
		{"\\**x", true},
	} {
		for _, labels := range [][]string{nil, {"a"}} {
			_, diverged, err := parseInlineWithDefinitions(test.markdown, labels, inlineMarkdown)
			if err != nil {
				t.Fatalf("%q with labels %q: %v", test.markdown, labels, err)
			}
			if diverged != test.diverged {
				t.Errorf("%q with labels %q: diverged %v, want %v", test.markdown, labels, diverged, test.diverged)
			}
		}
	}
}

// An emphasis run with `_` just inside and a letter just outside opens or closes by the editor's
// rules, which let a run beside another `*` or `_` do so, and not by CommonMark's flanking rules
// alone. Its first writing reads back by the editor's rules only, the read that reports the
// divergence, so the renderer reads it by the flanking rules too and writes the letter as a
// character reference, which both rules read back.
func TestRenderWritesARunBesideAnUnderscoreSoBothRulesReadIt(t *testing.T) {
	for _, test := range []struct{ markdown, want string }{
		{"a*_*", "&#97;*_*\n"},
		{"*_*a", "*_*&#97;\n"},
	} {
		doc, err := ParseRendering(test.markdown)
		if err != nil {
			t.Fatalf("parse %q: %v", test.markdown, err)
		}
		got, err := Render(doc)
		if err != nil {
			t.Fatalf("render %q: %v", test.markdown, err)
		}
		if got != test.want {
			t.Errorf("render %q = %q, want %q", test.markdown, got, test.want)
		}
	}
}
