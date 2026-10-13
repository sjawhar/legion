package docs

import (
	"os"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// decisionBlockFixture reads one of the decision-block fixtures the browser's line-based cut and
// this cut share (packages/contracts/fixtures/decision-blocks), and checks it is written as
// pmdoc.Render writes it, so a stored version of it reads back byte for byte.
func decisionBlockFixture(t *testing.T, name string) string {
	t.Helper()
	content, err := os.ReadFile("../../../../contracts/fixtures/decision-blocks/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	markdown := string(content)
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
	rendered, err := pmdoc.Render(tree)
	if err != nil {
		t.Fatalf("render fixture %s: %v", name, err)
	}
	if rendered != markdown {
		t.Fatalf("fixture %s is not written as pmdoc.Render writes it; rendered:\n%s", name, rendered)
	}
	return markdown
}

// cutDecisionBlocks is CutDecisionBlocks failing t on an error.
func cutDecisionBlocks(t *testing.T, markdown string) string {
	t.Helper()
	cut, err := CutDecisionBlocks(markdown)
	if err != nil {
		t.Fatalf("cut decision blocks: %v", err)
	}
	return cut
}

// A re-answered block and a new answered block are the whole difference between the approved
// fixture and the answered one, so the two cut to the same text; a changed paragraph does not.
func TestCutDecisionBlocksEqualsAcrossAnswersAndDiffersOnAParagraph(t *testing.T) {
	approved := cutDecisionBlocks(t, decisionBlockFixture(t, "approved.md"))
	answered := cutDecisionBlocks(t, decisionBlockFixture(t, "answered.md"))
	paragraphChanged := cutDecisionBlocks(t, decisionBlockFixture(t, "paragraph-changed.md"))
	if approved != answered {
		t.Fatalf("cut(approved) != cut(answered):\n%s\n----\n%s", approved, answered)
	}
	if approved == paragraphChanged {
		t.Fatalf("cut(approved) == cut(paragraph-changed), want a changed paragraph to show:\n%s", approved)
	}
	if !strings.Contains(paragraphChanged, "The service ships behind a flag and a canary.") {
		t.Fatalf("cut(paragraph-changed) lost the changed paragraph:\n%s", paragraphChanged)
	}
}

// The cut leaves out every ask block wherever it stands - at the top level, in a blockquote, in a
// list item, inside a callout - and keeps the container that held it.
func TestCutDecisionBlocksLeavesOutEveryAskBlockAndKeepsItsContainer(t *testing.T) {
	cut := cutDecisionBlocks(t, decisionBlockFixture(t, "answered.md"))
	if strings.Contains(cut, ":::ask") || strings.Contains(cut, "ask{") {
		t.Fatalf("cut holds an ask block:\n%s", cut)
	}
	for _, kept := range []string{
		"# Rollout plan\n",
		"The service ships behind a flag.\n",
		"## Decisions\n",
		"> The platform team asked for a rollback path.\n",
		"- Announce the dates.\n- Pick the regions.\n",
		":::callout{#budget-note kind=\"note\" title=\"Open question\"}\nThe budget is not settled.\n:::\n",
		"The last paragraph closes the plan.\n",
	} {
		if !strings.Contains(cut, kept) {
			t.Fatalf("cut lost %q:\n%s", kept, cut)
		}
	}
	for _, gone := range []string{"How many regions", "rollback path?", "Which regions go first?", "Who pays", "second wave follow?"} {
		if strings.Contains(cut, gone) {
			t.Fatalf("cut kept a block's text %q:\n%s", gone, cut)
		}
	}
}

// A document with no ask block cuts to its own rendering, and one that is nothing but ask blocks
// cuts to nothing; a container holding only a block is kept, holding an empty paragraph.
func TestCutDecisionBlocksOfADocumentWithoutBlocksIsItsRendering(t *testing.T) {
	plain := "# Title\n\nA paragraph.\n\n> A quote.\n\n- One.\n- Two.\n"
	tree, err := pmdoc.Parse(plain)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := pmdoc.Render(tree)
	if err != nil {
		t.Fatal(err)
	}
	if cut := cutDecisionBlocks(t, plain); cut != rendered {
		t.Fatalf("cut(plain) = %q, want its own rendering %q", cut, rendered)
	}
	if cut := cutDecisionBlocks(t, ":::ask{#only urgency=\"med\" multiple=\"false\"}\nOnly?\n:::\n"); cut != "" {
		t.Fatalf("cut(a document that is one block) = %q, want nothing", cut)
	}
	quoted := cutDecisionBlocks(t, "Intro.\n\n> :::ask{#quoted urgency=\"med\" multiple=\"false\"}\n> Quoted?\n> :::\n\nAfter.\n")
	if !strings.HasPrefix(quoted, "Intro.\n\n>") || !strings.HasSuffix(quoted, "\n\nAfter.\n") || strings.Contains(quoted, "Quoted?") {
		t.Fatalf("cut(a quote holding one block) = %q, want the quote kept empty", quoted)
	}
}

// A block is newly answered when the version before lacks it or holds it with another
// answered_at; copies count on neither side, and two people's answers in one version name
// nobody. The states are read from the fixtures' blocks wherever they stand.
func TestNewlyAnsweredBlocksNamesTheOneAnswererOfWhatChanged(t *testing.T) {
	approvedTree, err := pmdoc.Parse(decisionBlockFixture(t, "approved.md"))
	if err != nil {
		t.Fatal(err)
	}
	answeredTree, err := pmdoc.Parse(decisionBlockFixture(t, "answered.md"))
	if err != nil {
		t.Fatal(err)
	}
	approved, answered := askBlockStates(approvedTree), askBlockStates(answeredTree)
	for _, id := range []string{"wave-size", "rollback", "regions", "budget"} {
		if _, held := approved[id]; !held {
			t.Fatalf("approved fixture states = %#v, want block %q read wherever it stands", approved, id)
		}
	}
	if approved["wave-size"].state != "answered" || approved["wave-size"].answeredBy != "alice" || approved["rollback"].state != "open" {
		t.Fatalf("approved fixture states = %#v", approved)
	}

	if login, newly := newlyAnsweredBlocks(answered, approved); !newly || login != "alice" {
		t.Fatalf("answered since approved = %q, %v; want alice's re-answer and new block", login, newly)
	}
	if login, newly := newlyAnsweredBlocks(approved, approved); newly || login != "" {
		t.Fatalf("approved since itself = %q, %v; want nothing newly answered", login, newly)
	}
	if login, newly := newlyAnsweredBlocks(approved, map[string]askBlockState{}); !newly || login != "alice" {
		t.Fatalf("approved since no version = %q, %v; want its answered block newly answered", login, newly)
	}

	twoAnswerers := map[string]askBlockState{
		"one": {state: "answered", answeredBy: "Alice", answeredAt: "2026-10-02T10:00:00Z"},
		"two": {state: "answered", answeredBy: "bob", answeredAt: "2026-10-02T10:01:00Z"},
	}
	if login, newly := newlyAnsweredBlocks(twoAnswerers, nil); newly || login != "" {
		t.Fatalf("two answerers = %q, %v; want no one", login, newly)
	}
	oneAnswererTwice := map[string]askBlockState{
		"one": {state: "answered", answeredBy: "Alice", answeredAt: "2026-10-02T10:00:00Z"},
		"two": {state: "answered", answeredBy: " alice ", answeredAt: "2026-10-02T10:01:00Z"},
	}
	if login, newly := newlyAnsweredBlocks(oneAnswererTwice, nil); !newly || login != "alice" {
		t.Fatalf("one answerer under two spellings = %q, %v; want alice", login, newly)
	}
	copied := map[string]askBlockState{
		"copy": {state: "answered", answeredBy: "bob", answeredAt: "2026-10-02T10:01:00Z", copied: true},
		"own":  {state: "answered", answeredBy: "alice", answeredAt: "2026-10-02T10:00:00Z"},
	}
	if login, newly := newlyAnsweredBlocks(copied, nil); !newly || login != "alice" {
		t.Fatalf("an answered copy beside an answer = %q, %v; want the copy ignored", login, newly)
	}
	if holdsAnsweredBlock(map[string]askBlockState{"copy": copied["copy"]}) {
		t.Fatal("a version holding only an answered copy holds an answered block of its own")
	}
}
