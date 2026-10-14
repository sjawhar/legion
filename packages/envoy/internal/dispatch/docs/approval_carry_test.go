package docs

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

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
// answered_at, and two people's answers in one version name nobody. The states are read from the
// fixtures' blocks wherever they stand.
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
}

// The version just written is trusted for nothing: each block's state is its row's, read as the
// attributes settlement writes from the row, so a block a stored version holds unchanged compares
// equal to its row; and a node no row of this document backs - forged on the socket, or not yet
// indexed - a copy, or one settlement could not read refuses the carry outright.
func TestRowBackedStatesReadRowsAndRefuseAForgedACopiedAndAnInvalidNode(t *testing.T) {
	answeredAt := time.Date(2026, 10, 2, 10, 0, 0, 123456000, time.UTC)
	rows := map[string]model.Ask{
		"own":  {State: "answered", Answer: &model.AskAnswer{User: "alice", Selected: []string{"Two"}, At: answeredAt}},
		"open": {State: "open"},
	}
	nodes := map[string]askNode{"own": {}, "open": {}}
	states, backed := rowBackedStates(nodes, rows)
	want := map[string]askBlockState{
		"own":  {state: "answered", answeredBy: "alice", answeredAt: "2026-10-02T10:00:00.123456Z"},
		"open": {state: "open"},
	}
	if !backed || !reflect.DeepEqual(states, want) {
		t.Fatalf("row-backed states = %#v, %v; want %#v from the rows", states, backed, want)
	}
	for id, ask := range rows {
		node := &pmdoc.Node{Type: "ask", Attrs: pmdoc.Attrs{pmdoc.BlockIDAttr: id, "urgency": "med", "multiple": false}}
		setAskServerAttributes(node, ask, "", false)
		if written := askBlockStates(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{node}})[id]; written != states[id] {
			t.Fatalf("block %q as settlement writes it = %#v, its row reads %#v; want them equal", id, written, states[id])
		}
	}

	for name, nodes := range map[string]map[string]askNode{
		"a node no row backs": {"own": {}, "forged": {}},
		"a copied node":       {"own": {}, "copy": {copied: true}},
		"an invalid node":     {"own": {}, "open": {invalid: true}},
	} {
		if states, backed := rowBackedStates(nodes, rows); backed || states != nil {
			t.Errorf("%s: row-backed states = %#v, %v; want the carry refused", name, states, backed)
		}
	}
	if states, backed := rowBackedStates(nil, rows); !backed || len(states) != 0 {
		t.Fatalf("no nodes: states = %#v, %v; want none, backed", states, backed)
	}
}

// askNodes reads every ask node of the live tree wherever it stands, with the flags the rule
// needs: copied_from makes a copy, invalid an unreadable block, and a block carrying neither is
// plain whatever else its attributes say.
func TestAskNodesOfTheCarriedVersionFlagCopiesAndInvalidBlocksWhereverTheyStand(t *testing.T) {
	tree, err := pmdoc.Parse(decisionBlockFixture(t, "answered.md"))
	if err != nil {
		t.Fatal(err)
	}
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type == "ask" {
			switch node.Attrs[pmdoc.BlockIDAttr] {
			case "rollback":
				node.Attrs["copied_from"] = "ask-1"
			case "budget":
				node.Attrs["invalid"] = `ask block "budget" has an empty question`
			}
		}
		return true
	})
	want := map[string]askNode{"wave-size": {}, "rollback": {copied: true}, "regions": {}, "budget": {invalid: true}, "second-wave": {}}
	if got := askNodes(tree); !reflect.DeepEqual(got, want) {
		t.Fatalf("askNodes = %#v, want %#v", got, want)
	}
	if got := askNodes(nil); got != nil {
		t.Fatalf("askNodes(nil) = %#v, want none", got)
	}
}

// The rule over in-memory states: N carries H's approval of A when the two cut to the same text,
// every block of N not as A held it is H's answer, and every block of A still stands.
func TestCarryVerdictOverTheBlocksOfTheApprovedAndTheNewVersion(t *testing.T) {
	answered := func(by, at string) askBlockState {
		return askBlockState{state: "answered", answeredBy: by, answeredAt: at}
	}
	open, resolved := askBlockState{state: "open"}, askBlockState{state: "resolved"}
	const text = "Context\n"
	for _, test := range []struct {
		name  string
		input carryInput
		carry bool
	}{
		{"his answer to a block added since his approval", carryInput{
			login: "alice", blocks: map[string]askBlockState{"wave": answered("alice", "t1")}, approvedCut: text, cut: text,
		}, true},
		{"his answer to a block the approved version held open", carryInput{
			login: "alice", approved: map[string]askBlockState{"wave": open}, blocks: map[string]askBlockState{"wave": answered("alice", "t1")}, approvedCut: text, cut: text,
		}, true},
		{"his changed answer to a block he answered before his approval", carryInput{
			login: "alice", approved: map[string]askBlockState{"wave": answered("alice", "t0")}, blocks: map[string]askBlockState{"wave": answered("alice", "t1")}, approvedCut: text, cut: text,
		}, true},
		{"another's answer he approved, unchanged, beside his own", carryInput{
			login: "alice", approved: map[string]askBlockState{"wave": answered("bob", "t0")},
			blocks: map[string]askBlockState{"wave": answered("bob", "t0"), "region": answered("Alice", "t1")}, approvedCut: text, cut: text,
		}, true},
		{"another's answer since his approval to a block it held open", carryInput{
			login: "alice", approved: map[string]askBlockState{"wave": open},
			blocks: map[string]askBlockState{"wave": answered("bob", "t1"), "region": answered("alice", "t2")}, approvedCut: text, cut: text,
		}, false},
		{"a block he approved resolved since", carryInput{
			login: "alice", approved: map[string]askBlockState{"wave": open},
			blocks: map[string]askBlockState{"wave": resolved, "region": answered("alice", "t1")}, approvedCut: text, cut: text,
		}, false},
		{"a block he approved removed", carryInput{
			login: "alice", approved: map[string]askBlockState{"wave": open, "region": open},
			blocks: map[string]askBlockState{"wave": answered("alice", "t1")}, approvedCut: text, cut: text,
		}, false},
		{"a block added since left open", carryInput{
			login: "alice", blocks: map[string]askBlockState{"wave": answered("alice", "t1"), "region": open}, approvedCut: text, cut: text,
		}, false},
		{"a block added since answered by someone else", carryInput{
			login: "alice", blocks: map[string]askBlockState{"wave": answered("alice", "t1"), "region": answered("bob", "t2")}, approvedCut: text, cut: text,
		}, false},
		{"an edit outside the blocks", carryInput{
			login: "alice", blocks: map[string]askBlockState{"wave": answered("alice", "t1")}, approvedCut: text, cut: "Context changed.\n",
		}, false},
	} {
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
