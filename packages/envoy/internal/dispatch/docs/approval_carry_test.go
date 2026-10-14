package docs

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
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

// A decision block written as an example inside a code fence is code, which the parser keeps as
// text: the cut leaves it in place and takes out only the real block, and an edit inside the
// example changes the cut text. The browser's line-based cut (`decision-blocks.test.ts`) reads
// the same fixture.
func TestCutDecisionBlocksKeepsADecisionBlockWrittenInsideACodeFence(t *testing.T) {
	fenced := decisionBlockFixture(t, "fenced-example.md")
	cut := cutDecisionBlocks(t, fenced)
	for _, kept := range []string{"```markdown\n:::ask{urgency=\"high\" multiple=\"false\"}\nWhich region goes first?", "- West: The busy one.\n:::\n```"} {
		if !strings.Contains(cut, kept) {
			t.Fatalf("cut lost the fenced example %q:\n%s", kept, cut)
		}
	}
	for _, gone := range []string{"How many regions take the first wave?", "#rollout"} {
		if strings.Contains(cut, gone) {
			t.Fatalf("cut kept the real block (%q):\n%s", gone, cut)
		}
	}
	edited := cutDecisionBlocks(t, strings.Replace(fenced, "Which region goes first?", "Which region goes last?", 1))
	if edited == cut || !strings.Contains(edited, "Which region goes last?") {
		t.Fatalf("an edit inside the fenced example did not change the cut:\n%s", edited)
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

// answerers names each person who answered a block of the version once, by canonical login, in
// sorted order: one answerer under two spellings is one, two people are two, and a version whose
// blocks are all open or resolved names nobody. The states are read from the fixtures' blocks
// wherever they stand.
func TestAnswerersNamesEachAnswererOfTheVersionOnce(t *testing.T) {
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
	if got := answerers(answered); !reflect.DeepEqual(got, []string{"alice"}) {
		t.Fatalf("answerers(answered) = %q, want alice, who answered both answered blocks", got)
	}
	if got := answerers(approved); !reflect.DeepEqual(got, []string{"alice"}) {
		t.Fatalf("answerers(approved) = %q, want alice, who answered its one answered block", got)
	}

	answeredBy := func(by, at string) askBlockState {
		return askBlockState{state: "answered", answeredBy: by, answeredAt: at}
	}
	twoAnswerers := map[string]askBlockState{
		"one":   answeredBy("Bob", "2026-10-02T10:00:00Z"),
		"two":   answeredBy("alice", "2026-10-02T10:01:00Z"),
		"three": answeredBy(" Alice ", "2026-10-02T10:02:00Z"),
		"four":  {state: "open"},
	}
	if got := answerers(twoAnswerers); !reflect.DeepEqual(got, []string{"alice", "bob"}) {
		t.Fatalf("answerers(two people, one under two spellings) = %q, want alice and bob, sorted", got)
	}
	if got := answerers(map[string]askBlockState{"one": {state: "open"}, "two": {state: "resolved"}}); got != nil {
		t.Fatalf("answerers(open and resolved) = %q, want nobody", got)
	}
	if got := answerers(nil); got != nil {
		t.Fatalf("answerers(no blocks) = %q, want nobody", got)
	}
}

// askNodeOf is the one ask node of the block markdown holds.
func askNodeOf(t *testing.T, markdown string) *pmdoc.Node {
	t.Helper()
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatalf("parse block %q: %v", markdown, err)
	}
	node := firstAsk(tree)
	if node == nil {
		t.Fatalf("block %q parsed to no ask node", markdown)
	}
	return node
}

// withAttr is node with the attribute name set to value, as a peer on the document socket or
// settlement's own flag (invalid, which no rendering writes) sets one on the live tree.
func withAttr(node *pmdoc.Node, name string, value any) *pmdoc.Node {
	node.Attrs[name] = value
	return node
}

// The version just written is trusted for nothing: each block's state is its row's, read as the
// attributes settlement writes from the row, so a block a stored version holds unchanged compares
// equal to its row; and the block itself must be as settlement's own writer leaves it from the
// row. A node no row of this document backs - forged on the socket, or not yet indexed - refuses
// the carry outright; so does a node whose server attributes the writer would change - a copy,
// one settlement could not read, a state, answerer, time, selection or answer other than the
// row's, with or without the answer - and the block of a decided row that asks other wording than
// the row was decided on or that settlement cannot read. A block holding the row's answer, and
// one without it, as settlement leaves a withheld answer, are both as the row has it. An open
// row's body is free.
func TestRowBackedStatesReadRowsAndRefuseAForgedACopiedAndAnInvalidNode(t *testing.T) {
	answeredAt := time.Date(2026, 10, 2, 10, 0, 0, 123456000, time.UTC)
	text := "The quiet one first."
	rows := map[string]model.Ask{
		"own": {
			State: "answered", Urgency: "low", Question: "Which region goes first?",
			Options: []model.AskOption{{Label: "East", Description: "The quiet one."}, {Label: "West", Description: "The busy one."}},
			Answer:  &model.AskAnswer{User: "alice", Selected: []string{"East"}, Text: &text, At: answeredAt},
		},
		"open": {State: "open", Urgency: "med", Question: "How many regions take the first wave?", Options: []model.AskOption{}},
	}
	const (
		ownAnswered   = `:::ask{#own urgency="low" multiple="false" state="answered" answered_by="alice" answered_at="2026-10-02T10:00:00.123456Z"`
		ownDecided    = ownAnswered + ` selected="[&#x22;East&#x22;]" answer="The quiet one first."}` + "\n"
		ownWithheld   = ownAnswered + "}\n"
		ownBody       = "Which region goes first?\n\n- East: The quiet one.\n- West: The busy one.\n:::\n"
		openDirective = `:::ask{#open urgency="med" multiple="false" state="open"}` + "\n"
		openBody      = "How many regions take the first wave?\n:::\n"
	)
	own := askNodeOf(t, ownDecided+ownBody)
	open := askNodeOf(t, openDirective+openBody)
	states, backed := rowBackedStates(map[string]*pmdoc.Node{"own": own, "open": open}, rows)
	want := map[string]askBlockState{
		"own":  {state: "answered", answeredBy: "alice", answeredAt: "2026-10-02T10:00:00.123456Z"},
		"open": {state: "open"},
	}
	if !backed || !reflect.DeepEqual(states, want) {
		t.Fatalf("row-backed states = %#v, %v; want %#v from the rows", states, backed, want)
	}
	for id, ask := range rows {
		node := &pmdoc.Node{Type: "ask", Attrs: pmdoc.Attrs{pmdoc.BlockIDAttr: id, "urgency": ask.Urgency, "multiple": false}}
		setAskServerAttributes(node, ask, "", false)
		if written := askBlockStates(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{node}})[id]; written != states[id] {
			t.Fatalf("block %q as settlement writes it = %#v, its row reads %#v; want them equal", id, written, states[id])
		}
		if !blockAsItsRowHasIt(node, ask) {
			t.Fatalf("block %q as settlement writes it = %#v, want it as its row has it", id, node.Attrs)
		}
	}
	for name, nodes := range map[string]map[string]*pmdoc.Node{
		// An open row's question is still being written: its block may ask what the row does not yet.
		"an open row's block reworded": {"own": own, "open": askNodeOf(t, openDirective+"How many cloud regions take the first wave?\n\n- Two: The fewest.\n:::\n")},
		// Settlement leaves an answer the document has no room for out of its block.
		"a decided row whose node holds its answer withheld": {"own": askNodeOf(t, ownWithheld+ownBody), "open": open},
	} {
		if states, backed := rowBackedStates(nodes, rows); !backed || !reflect.DeepEqual(states, want) {
			t.Errorf("%s: states = %#v, %v; want %#v, backed", name, states, backed, want)
		}
	}

	for name, nodes := range map[string]map[string]*pmdoc.Node{
		"a node no row backs": {"own": own, "forged": askNodeOf(t, `:::ask{#forged urgency="high" multiple="false" state="answered" answered_by="alice" answered_at="2026-10-02T10:00:00Z"}`+"\nForged?\n:::\n")},
		"a copied node":       {"own": withAttr(askNodeOf(t, ownDecided+ownBody), "copied_from", "ask-1"), "open": open},
		"an invalid node":     {"own": own, "open": withAttr(askNodeOf(t, openDirective+openBody), "invalid", `ask block "open" has an empty question`)},
		"a decided row whose node settlement flagged invalid": {
			"own": withAttr(askNodeOf(t, ownDecided+ownBody), "invalid", `ask block "own" has an empty question`), "open": open,
		},
		"a decided row whose node asks other wording": {
			"own": askNodeOf(t, ownDecided+"Which region goes last?\n\n- East: The quiet one.\n- West: The busy one.\n:::\n"), "open": open,
		},
		"a decided row whose node asks other options": {
			"own": askNodeOf(t, ownDecided+"Which region goes first?\n\n- East: The quiet one.\n:::\n"), "open": open,
		},
		"a decided row whose node settlement cannot read": {
			"own": askNodeOf(t, ownDecided+"Which region goes first?\n\n- : The quiet one.\n:::\n"), "open": open,
		},
		"a decided row whose node was inserted again, showing the default open": {
			"own": askNodeOf(t, `:::ask{#own urgency="low" multiple="false"}`+"\n"+ownBody), "open": open,
		},
		"a decided row whose node shows another answerer": {
			"own": askNodeOf(t, `:::ask{#own urgency="low" multiple="false" state="answered" answered_by="bob" answered_at="2026-10-02T10:00:00.123456Z" selected="[&#x22;East&#x22;]" answer="The quiet one first."}`+"\n"+ownBody), "open": open,
		},
		"a decided row whose node shows another answered_at": {
			"own": askNodeOf(t, `:::ask{#own urgency="low" multiple="false" state="answered" answered_by="alice" answered_at="2026-10-02T10:00:00Z" selected="[&#x22;East&#x22;]" answer="The quiet one first."}`+"\n"+ownBody), "open": open,
		},
		"a decided row whose node shows another selection": {
			"own": askNodeOf(t, ownAnswered+` selected="[&#x22;West&#x22;]" answer="The quiet one first."}`+"\n"+ownBody), "open": open,
		},
		"a decided row whose node shows other answer text": {
			"own": askNodeOf(t, ownAnswered+` selected="[&#x22;East&#x22;]" answer="The busy one first."}`+"\n"+ownBody), "open": open,
		},
		"a decided row whose node shows the selection without the text": {
			"own": askNodeOf(t, ownAnswered+` selected="[&#x22;East&#x22;]"}`+"\n"+ownBody), "open": open,
		},
		"a decided row whose node shows the text without the selection": {
			"own": askNodeOf(t, ownAnswered+` answer="The quiet one first."}`+"\n"+ownBody), "open": open,
		},
		"an open row whose node shows answered": {
			"own": own, "open": askNodeOf(t, `:::ask{#open urgency="med" multiple="false" state="answered" answered_by="alice" answered_at="2026-10-02T10:00:00Z"}`+"\n"+openBody),
		},
		"an open row whose node shows a selection": {
			"own": own, "open": askNodeOf(t, `:::ask{#open urgency="med" multiple="false" state="open" selected="[&#x22;Two&#x22;]"}`+"\n"+openBody),
		},
	} {
		if states, backed := rowBackedStates(nodes, rows); backed || states != nil {
			t.Errorf("%s: row-backed states = %#v, %v; want the carry refused", name, states, backed)
		}
	}
	if states, backed := rowBackedStates(nil, rows); !backed || len(states) != 0 {
		t.Fatalf("no nodes: states = %#v, %v; want none, backed", states, backed)
	}
}

// askNodes reads every ask node of the live tree wherever it stands, each id once; a tree that
// repeats a block id, which settlement reads no block under, is not read at all.
func TestAskNodesOfTheCarriedVersionReadEveryBlockOnceAndRefuseARepeatedID(t *testing.T) {
	tree, err := pmdoc.Parse(decisionBlockFixture(t, "answered.md"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]*pmdoc.Node{}
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type == "ask" {
			id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
			want[id] = node
		}
		return true
	})
	for _, id := range []string{"wave-size", "rollback", "regions", "budget", "second-wave"} {
		if _, held := want[id]; !held {
			t.Fatalf("the fixture holds no block %q", id)
		}
	}
	if got, readable := askNodes(tree); !readable || !reflect.DeepEqual(got, want) {
		t.Fatalf("askNodes = %#v, %v; want %#v, readable", got, readable, want)
	}
	if got, readable := askNodes(nil); got != nil || !readable {
		t.Fatalf("askNodes(nil) = %#v, %v; want none, readable", got, readable)
	}

	const wave = `:::ask{#wave urgency="med" multiple="false"}` + "\nHow many regions take the first wave?\n:::\n"
	repeated := &pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{
		askNodeOf(t, wave),
		{Type: "blockquote", Attrs: pmdoc.Attrs{}, Children: []*pmdoc.Node{askNodeOf(t, wave)}},
	}}
	if got, readable := askNodes(repeated); got != nil || readable {
		t.Fatalf("askNodes of a tree repeating a block id = %#v, %v; want none, not readable", got, readable)
	}
}

// The rule over in-memory states: N carries H's approval of A when the two cut to the same text,
// every block of N not as A held it is H's answer given after his approval, at least one is not
// as A held it, and every block of A still stands.
func TestCarryVerdictOverTheBlocksOfTheApprovedAndTheNewVersion(t *testing.T) {
	approvedAt := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	answered := func(by string, at time.Time) askBlockState {
		return askBlockState{state: "answered", answeredBy: by, answeredAt: at.Format(time.RFC3339Nano)}
	}
	before, after, later := approvedAt.Add(-time.Hour), approvedAt.Add(time.Minute), approvedAt.Add(2*time.Minute)
	open, resolved := askBlockState{state: "open"}, askBlockState{state: "resolved"}
	const text = "Context\n"
	for _, test := range []struct {
		name  string
		input carryInput
		carry bool
	}{
		{"his answer to a block added since his approval", carryInput{
			login: "alice", blocks: map[string]askBlockState{"wave": answered("alice", after)}, approvedCut: text, cut: text,
		}, true},
		{"his answer to a block the approved version held open", carryInput{
			login: "alice", approved: map[string]askBlockState{"wave": open}, blocks: map[string]askBlockState{"wave": answered("alice", after)}, approvedCut: text, cut: text,
		}, true},
		{"his changed answer to a block he answered before his approval", carryInput{
			login: "alice", approved: map[string]askBlockState{"wave": answered("alice", before)}, blocks: map[string]askBlockState{"wave": answered("alice", after)}, approvedCut: text, cut: text,
		}, true},
		{"another's answer he approved, unchanged, beside his own", carryInput{
			login: "alice", approved: map[string]askBlockState{"wave": answered("bob", before)},
			blocks: map[string]askBlockState{"wave": answered("bob", before), "region": answered("Alice", after)}, approvedCut: text, cut: text,
		}, true},
		{"another's answer since his approval to a block it held open", carryInput{
			login: "alice", approved: map[string]askBlockState{"wave": open},
			blocks: map[string]askBlockState{"wave": answered("bob", after), "region": answered("alice", later)}, approvedCut: text, cut: text,
		}, false},
		{"a block he approved resolved since", carryInput{
			login: "alice", approved: map[string]askBlockState{"wave": open},
			blocks: map[string]askBlockState{"wave": resolved, "region": answered("alice", after)}, approvedCut: text, cut: text,
		}, false},
		{"a block he approved removed", carryInput{
			login: "alice", approved: map[string]askBlockState{"wave": open, "region": open},
			blocks: map[string]askBlockState{"wave": answered("alice", after)}, approvedCut: text, cut: text,
		}, false},
		{"a block added since left open", carryInput{
			login: "alice", blocks: map[string]askBlockState{"wave": answered("alice", after), "region": open}, approvedCut: text, cut: text,
		}, false},
		{"a block added since answered by someone else", carryInput{
			login: "alice", blocks: map[string]askBlockState{"wave": answered("alice", after), "region": answered("bob", later)}, approvedCut: text, cut: text,
		}, false},
		{"an edit outside the blocks", carryInput{
			login: "alice", blocks: map[string]askBlockState{"wave": answered("alice", after)}, approvedCut: text, cut: "Context changed.\n",
		}, false},
		{"nothing of his changed: every block as the approved version held it", carryInput{
			login: "alice", approved: map[string]askBlockState{"wave": answered("alice", before), "region": answered("bob", before)},
			blocks: map[string]askBlockState{"wave": answered("alice", before), "region": answered("bob", before)}, approvedCut: text, cut: text,
		}, false},
		{"a block he answered before his approval, removed since and restored as it was", carryInput{
			login: "alice", approved: map[string]askBlockState{"wave": answered("alice", before)},
			blocks: map[string]askBlockState{"wave": answered("alice", before)}, approvedCut: text, cut: text,
		}, false},
		{"a block the approved version did not hold, back with an answer he gave before his approval", carryInput{
			login: "alice", blocks: map[string]askBlockState{"wave": answered("alice", before)}, approvedCut: text, cut: text,
		}, false},
		{"a restored block answered before his approval beside his answer since", carryInput{
			login: "alice", blocks: map[string]askBlockState{"wave": answered("alice", before), "region": answered("alice", after)}, approvedCut: text, cut: text,
		}, false},
		{"his answer at the approval's own instant", carryInput{
			login: "alice", blocks: map[string]askBlockState{"wave": answered("alice", approvedAt)}, approvedCut: text, cut: text,
		}, false},
		{"a changed block whose answered_at does not read as a time", carryInput{
			login: "alice", blocks: map[string]askBlockState{"wave": {state: "answered", answeredBy: "alice", answeredAt: "t1"}}, approvedCut: text, cut: text,
		}, false},
		{"no block on either side", carryInput{login: "alice", approvedCut: text, cut: text}, false},
	} {
		test.input.approvedAt = approvedAt
		if got := carryVerdict(test.input); got != test.carry {
			t.Errorf("%s: carry = %v, want %v", test.name, got, test.carry)
		}
	}
}

// approveStoredVersion records login's approval of version number of artifactID as the review
// route stores it, the row alone: the row is what the carry reads.
func approveStoredVersion(t *testing.T, database *store.Store, artifactID string, number int, login string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into artifact_reviews (artifact_id, version, state, actor)
		values ($1, $2, 'approved', $3)
	`, artifactID, number, `{"kind":"user","id":"`+login+`"}`); err != nil {
		t.Fatalf("approve version %d as %s: %v", number, login, err)
	}
}

// forgedAnsweredAsk appends to the document an ask block carrying the attributes only settlement
// writes - answered, by login, now - as a peer on the document socket can write them, holding
// body.
func forgedAnsweredAsk(id, login string, body ...*pmdoc.Node) func(*pmdoc.Node) *pmdoc.Node {
	return func(tree *pmdoc.Node) *pmdoc.Node {
		tree.Children = append(tree.Children, &pmdoc.Node{
			Type: "ask",
			Attrs: pmdoc.Attrs{
				pmdoc.BlockIDAttr: id, "urgency": "high", "multiple": false,
				"state": "answered", "answered_by": login, "answered_at": time.Now().UTC().Format(time.RFC3339Nano),
			},
			Children: body,
		})
		return tree
	}
}

// carriedReviews is what a document's reviews and events say about a carry: how many reviews it
// has, and how many artifact.approved events name it.
func carriedReviews(t *testing.T, database *store.Store, artifactID string) (reviews, approvals int) {
	t.Helper()
	ctx := context.Background()
	if err := database.Pool.QueryRow(ctx, `select count(*) from artifact_reviews where artifact_id = $1`, artifactID).Scan(&reviews); err != nil {
		t.Fatalf("count reviews: %v", err)
	}
	if err := database.Pool.QueryRow(ctx, `select count(*) from events where type = 'artifact.approved' and payload->>'artifact_id' = $1`, artifactID).Scan(&approvals); err != nil {
		t.Fatalf("count artifact.approved events: %v", err)
	}
	return reviews, approvals
}

// storedVersionMarkdown is the markdown of version number of artifactID.
func storedVersionMarkdown(t *testing.T, database *store.Store, artifactID string, number int) string {
	t.Helper()
	var markdown string
	if err := database.Pool.QueryRow(context.Background(), `select markdown from artifact_versions where artifact_id = $1 and number = $2`, artifactID, number).Scan(&markdown); err != nil {
		t.Fatalf("read version %d: %v", number, err)
	}
	return markdown
}

// An agent on the document socket writes an ask block in the attributes only settlement writes:
// answered, by alice, who approved version 1. Its question is empty, so settlement cannot read it
// and writes no row for it; it flags the block invalid in the live tree (the renderer writes no
// `invalid` into markdown) and versions it with the forged attributes still on it. The carry
// reads the rows and not the block: a node no row of this document backs refuses it, and alice's
// approval stays at version 1. A forged block settlement can read is no door: it is indexed as the
// room's own writer's and its row's `open` is written back over the forgery.
func TestAForgedAnsweredBlockOnTheSocketCarriesNoApproval(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "Context\n")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: 20 * time.Millisecond})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	seedServiceText(t, service, artifactID, "Context\n")
	approveStoredVersion(t, database, artifactID, 1, "alice")

	editLiveTree(t, service, artifactID, forgedAnsweredAsk("forged", "alice", &pmdoc.Node{Type: "paragraph", Attrs: pmdoc.Attrs{}}))
	waitForDocumentVersion(t, database, artifactID, 2)
	markdown := storedVersionMarkdown(t, database, artifactID, 2)
	if !strings.Contains(markdown, `:::ask{#forged urgency="high" multiple="false" state="answered" answered_by="alice" answered_at="`) {
		t.Fatalf("version 2 = %q, want the forged block with its forged attributes kept", markdown)
	}
	if invalid, _ := firstAsk(liveTree(t, service, artifactID)).Attrs["invalid"].(string); !strings.Contains(invalid, "empty question") {
		t.Fatalf("forged block invalid = %q, want settlement to have flagged its empty question", invalid)
	}
	var rows int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from asks where block_artifact_id = $1 and block_id = 'forged'`, artifactID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("rows for the forged block = %d, want none", rows)
	}
	if reviews, approvals := carriedReviews(t, database, artifactID); reviews != 1 || approvals != 0 {
		t.Fatalf("reviews = %d, artifact.approved events = %d; want alice's approval of version 1 alone and no event", reviews, approvals)
	}
}

// joined runs op inside a transaction joined to the service (Join), committed through its ledger,
// as the answer route and the named-version route run their writes.
func joined(t *testing.T, service *Service, op func(ctx context.Context, tx pgx.Tx) error) {
	t.Helper()
	tx, err := service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(context.Background())
	ctx, ledger := service.Join(context.Background(), tx)
	defer ledger.Discard()
	if err := op(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Commit(context.Background()); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// answerBlockAs writes login's answer to the block ask blockID of artifactID as the answer route
// does: the row's state and answer, and the block's attributes through the service's own live
// attribute write, in one joined transaction.
func answerBlockAs(t *testing.T, service *Service, artifactID, blockID, login string, selected []string) {
	t.Helper()
	at := time.Now().UTC()
	answer := model.AskAnswer{User: login, Selected: selected, At: at}
	encoded, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	joined(t, service, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `update asks set state = 'answered', answer = $3 where block_artifact_id = $1 and block_id = $2`, artifactID, blockID, encoded); err != nil {
			return err
		}
		return service.SetBlockAttributes(ctx, artifactID, blockID, map[string]any{
			"state": "answered", "answered_by": login, "answered_at": at.Format(time.RFC3339Nano), "selected": selected, "answer": nil,
		}, model.Actor{Kind: "user", ID: login})
	})
}

// A version named from the live tree while it holds two ask nodes under one decided block id - a
// second node a document-socket peer wrote with the id of the block the human answered - carries
// nothing: one node per id is the first thing the carry holds a version to, before any node is
// read. The same version without the repeat carries, so the repeat alone is what refuses.
func TestAVersionHoldingARepeatedBlockIDCarriesNothing(t *testing.T) {
	const spec = "Context\n\n:::ask{#region urgency=\"low\" multiple=\"false\"}\nWhich region goes first?\n\n- East: The quiet one.\n- West: The busy one.\n:::\n"
	run := func(t *testing.T, repeat bool) (reviews, approvals int) {
		database := storetest.Open(t)
		artifactID := createDocument(t, database, spec)
		service := New(Deps{Store: database, Events: events.NewBroker(), Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
		seedServiceText(t, service, artifactID, spec)
		// Settlement indexes the block as an ask of this document, the row the carry reads.
		service.ScheduleSettlement(artifactID)
		deadline := time.Now().Add(3 * time.Second)
		for {
			var rows int
			if err := database.Pool.QueryRow(context.Background(), `select count(*) from asks where block_artifact_id = $1 and block_id = 'region'`, artifactID).Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if rows == 1 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("settlement did not index the block")
			}
			time.Sleep(5 * time.Millisecond)
		}
		approveStoredVersion(t, database, artifactID, 1, "alice")

		answerBlockAs(t, service, artifactID, "region", "alice", []string{"East"})
		if repeat {
			editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
				second, err := pmdoc.Parse(":::ask{#region urgency=\"low\" multiple=\"false\"}\nWhich region goes last?\n\n- North: The cold one.\n:::\n")
				if err != nil {
					t.Fatal(err)
				}
				tree.Children = append(tree.Children, second.Children...)
				return tree
			})
		}
		joined(t, service, func(ctx context.Context, tx pgx.Tx) error {
			_, err := service.NamedVersion(ctx, artifactID, "Named by the agent", model.Actor{Kind: "session", ID: "session-0123456789abcdef"})
			return err
		})
		return carriedReviews(t, database, artifactID)
	}
	if reviews, approvals := run(t, false); reviews != 2 || approvals != 1 {
		t.Fatalf("without the repeat: reviews = %d, artifact.approved events = %d; want her answer to carry her approval", reviews, approvals)
	}
	if reviews, approvals := run(t, true); reviews != 1 || approvals != 0 {
		t.Fatalf("with the repeat: reviews = %d, artifact.approved events = %d; want alice's approval of version 1 alone and no event", reviews, approvals)
	}
}
