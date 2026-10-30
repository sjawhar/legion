package docstest

import (
	"fmt"
	"math/rand/v2"
	"strings"
)

// LoadSpec is the load harness's spec (specMarkdown in packages/envoy/loadtest/seed.go, not table
// heavy): a title, the "Revision token r0" paragraph, then sections of two prose paragraphs and a
// six-column table of four rows until it holds target bytes. It returns the spec, each row's
// "Requirement <key>-R<n> holds" cell, and the prose paragraphs, each without its closing period.
// One seed of r gives one spec.
func LoadSpec(r *rand.Rand, key string, target int) (spec string, quotes, paragraphs []string) {
	var b strings.Builder
	fmt.Fprintf(&b, "# Spec for %s\n\nRevision token r0\n\n", key)
	row, section := 0, 0
	for b.Len() < target {
		section++
		p1, p2 := SpecSentence(r, 40), SpecSentence(r, 30)
		paragraphs = append(paragraphs, p1, p2)
		fmt.Fprintf(&b, "## Section %d\n\n%s.\n\n%s.\n\n", section, p1, p2)
		b.WriteString("| Id | Requirement | Owner | Status | Evidence | Notes |\n|---|---|---|---|---|---|\n")
		for range 4 {
			row++
			quote := fmt.Sprintf("Requirement %s-R%d holds", key, row)
			quotes = append(quotes, quote)
			fmt.Fprintf(&b, "| R%d | %s | team-%d | open | %s | %s |\n", row, quote, r.IntN(9), SpecSentence(r, 6), SpecSentence(r, 5))
		}
		b.WriteString("\n")
	}
	return b.String(), quotes, paragraphs
}

var specWords = strings.Fields(`the outbox publishes each committed event to its issue topic and every session that
watches the issue receives it within a second while the listener keeps interests in a key value bucket
dispatch reads issues documents comments and asks from postgres through a shared pool of connections
a reviewer anchors a comment to a line of the specification and the author answers with a new version
latency throughput memory goroutines profile scaling knee saturation connection queue lock contention`)

// SpecSentence is words of the load harness's vocabulary drawn from r, separated by spaces.
func SpecSentence(r *rand.Rand, words int) string {
	var b strings.Builder
	for i := range words {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(specWords[r.IntN(len(specWords))])
	}
	return b.String()
}
