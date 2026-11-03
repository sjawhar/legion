package pmdoc

import (
	"reflect"
	"strings"
	"testing"
)

// The renderer skips a read-back that is the same parse as one it made (readsBackUnder):
// storedMarkdown where the source holds no `~`, since it then reads as inlineMarkdown does, and
// flankingMarkdown where the stored parse reported no `*` or `_` run that could open or close
// otherwise under flankingRules (flankingDivergenceKey). Each skipped read must give the nodes and
// the error the read it stands for gives, and the divergence it reports.
func FuzzReadBackSkips(f *testing.F) {
	seeds := []string{
		`\**x`, `b_*a**`, `***a.****&#32;b*`, `a_b*a_b*`, `**[a](u)**_b_`, `**a~~b**c~~`, `*~~**b~~*`,
		`~a~ *b*`, `a~~*b*~~`, `\~~a~~`, `[^a] *b*`, `**a** b`,
		`~~a ~b~~ c~`, `~a~~b~`, `a\~~~b~~`, `x ~\~~ y ~\~~`, `[~\~~](u)`, `&#126;~a~`,
		`a*_*`, `*_*a`, `a*~*`, `~a~~`,
	}
	for _, seed := range seeds {
		f.Add(seed, false)
		f.Add(seed, true)
	}
	f.Fuzz(func(t *testing.T, markdown string, withLabel bool) {
		var labels []string
		if withLabel {
			labels = []string{"a"}
		}
		editorNodes, editorDiverged, editorErr := parseInlineWithDefinitions(markdown, labels, inlineMarkdown)
		if !strings.Contains(markdown, "~") {
			storedNodes, storedDiverged, storedErr := parseInlineWithDefinitions(markdown, labels, storedMarkdown)
			sameRead(t, markdown, labels, "inlineMarkdown", editorNodes, editorErr, "storedMarkdown", storedNodes, storedErr)
			if storedDiverged != editorDiverged {
				t.Fatalf("%q with labels %q: storedMarkdown reports divergence %v, inlineMarkdown %v", markdown, labels, storedDiverged, editorDiverged)
			}
		}
		storedNodes, storedDiverged, storedErr := parseInlineWithDefinitions(markdown, labels, storedMarkdown)
		if storedDiverged {
			return
		}
		flankingNodes, flankingDiverged, flankingErr := parseInlineWithDefinitions(markdown, labels, flankingMarkdown)
		sameRead(t, markdown, labels, "storedMarkdown", storedNodes, storedErr, "flankingMarkdown", flankingNodes, flankingErr)
		if flankingDiverged {
			t.Fatalf("%q with labels %q: flankingMarkdown reports divergence", markdown, labels)
		}
	})
}

// sameRead fails t unless two readers read markdown to equal nodes and errors of the same text.
func sameRead(t *testing.T, markdown string, labels []string, name string, nodes []*Node, err error, otherName string, otherNodes []*Node, otherErr error) {
	t.Helper()
	if errorText(err) != errorText(otherErr) {
		t.Fatalf("%q with labels %q: %s errs %q, %s %q", markdown, labels, name, errorText(err), otherName, errorText(otherErr))
	}
	if !reflect.DeepEqual(nodes, otherNodes) {
		t.Fatalf("%q with labels %q: %s reads %s, %s %s", markdown, labels, name, markedSummary(nodes), otherName, markedSummary(otherNodes))
	}
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// A parse reports flanking divergence only where a `*` or `_` run beside another could open or
// close otherwise under the flanking rules alone, under both readers that report it and with or
// without footnote definitions after the run.
func TestFlankingDivergenceIsReportedOnlyWhereTheRulesDiffer(t *testing.T) {
	tests := []struct {
		markdown string
		diverged bool
	}{
		{`**bold** and *em* and _u_`, false},
		{`b_*a**`, true},
		{`\**x`, true},
	}
	for _, reader := range []struct {
		name   string
		reader inlineReader
	}{{"inlineMarkdown", inlineMarkdown}, {"storedMarkdown", storedMarkdown}} {
		for _, labels := range [][]string{nil, {"a"}} {
			for _, test := range tests {
				if _, diverged, err := parseInlineWithDefinitions(test.markdown, labels, reader.reader); err != nil || diverged != test.diverged {
					t.Errorf("%s reads %q with labels %q: diverged %v (%v), want %v", reader.name, test.markdown, labels, diverged, err, test.diverged)
				}
			}
		}
	}
}

// Each reading here renders otherwise if the read-back it needs were skipped: `a*_*` and `*_*a`
// read back under inlineMarkdown and storedMarkdown as written, but under flankingMarkdown the `*`
// beside the letter opens or closes nothing, so the letter is written as a character reference; a
// source holding `~` is read back under storedMarkdown, which pairs `~` runs of either length and
// reads no `*` run as opening or closing beside a `~`.
func TestRenderReadsBackUnderEveryReaderASkipCouldMiss(t *testing.T) {
	tests := []struct {
		name string
		doc  *Node
		want string
	}{
		{"flanking read of a*_*", parsedDoc(t, "a*_*"), "&#97;*_*\n"},
		{"flanking read of *_*a", parsedDoc(t, "*_*a"), "*_*&#97;\n"},
		{"stored read of a*~*", parsedDoc(t, "a*~*"), "&#97;*~*\n"},
		{"stored read of *~*a", parsedDoc(t, "*~*a"), "*~*&#97;\n"},
		{"stored read of the text ~a~~", &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{{Type: "text", Text: "~a~~"}}}}}, "&#126;a&#126;&#126;\n"},
	}
	for _, test := range tests {
		if got, err := Render(test.doc); err != nil || got != test.want {
			t.Errorf("%s: Render = %q (%v), want %q", test.name, got, err, test.want)
		}
	}
}

func parsedDoc(t *testing.T, markdown string) *Node {
	t.Helper()
	doc, err := Parse(markdown)
	if err != nil {
		t.Fatalf("Parse(%q): %v", markdown, err)
	}
	return doc
}
