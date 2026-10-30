package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMarkQuoteWritesMarkAndReturnsCoveredText(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "The quick brown fox")
	spec := MarkSpec{Kind: MarkAsk, ID: "ask-1", By: model.Actor{Kind: "session", ID: "s1"}}

	anchored, err := joinedMarkQuote(service, artifactID, spec, "brown", nil)
	if err != nil || anchored.Quote != "brown" {
		t.Fatalf("MarkQuote = %q, %v", anchored.Quote, err)
	}

	tree := liveTree(t, service, artifactID)
	r, got, ok := pmdoc.FindMark(tree, "dispatchAsk", "ask-1")
	if !ok || got != "brown" || r != (pmdoc.Range{From: 11, To: 16}) {
		t.Fatalf("FindMark = %v %q %v", r, got, ok)
	}
	attrs, _ := pmdoc.MarkAttrs(tree, "dispatchAsk", "ask-1")
	if attrs["by"] != "session:s1" {
		t.Fatalf("by = %v", attrs["by"])
	}
	if text, _ := service.Text(context.Background(), artifactID); text != "The quick brown fox\n" {
		t.Fatalf("marks must not render: %q", text)
	}

	var ambiguous *pmdoc.ErrTargetAmbiguous
	if _, err := joinedMarkQuote(service, artifactID, spec, "o", nil); !errors.As(err, &ambiguous) {
		t.Fatalf("ambiguous quote err = %v", err)
	}
	if _, err := joinedMarkQuote(service, artifactID, spec, "purple", nil); !errors.Is(err, pmdoc.ErrTargetNotFound) {
		t.Fatalf("missing quote err = %v", err)
	}
}

func TestMarkQuoteJoinsTheCallerTransaction(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "The quick brown fox")

	tx, err := service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	joined, ledger := service.Join(context.Background(), tx)
	if _, err := service.MarkQuote(joined, artifactID, MarkSpec{
		Kind: MarkComment,
		ID:   "c1",
		By:   model.Actor{Kind: "user", ID: "alice"},
	}, "quick", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := pmdoc.FindMark(liveTree(t, service, artifactID), "proofComment", "c1"); ok {
		t.Fatal("the room holds a mark whose transaction has not committed")
	}
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	ledger.Discard()
	if _, _, ok := pmdoc.FindMark(liveTree(t, service, artifactID), "proofComment", "c1"); ok {
		t.Fatal("rolled-back mark reached the room")
	}
}

func TestVerifyMarkWaitsForTheBrowserUpdate(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "")
	service := New(Deps{
		Store:    database,
		Events:   events.NewBroker(),
		Settle:   time.Hour,
		MarkWait: 300 * time.Millisecond,
	})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	seedServiceText(t, service, artifactID, "The quick brown fox")

	go func() {
		time.Sleep(100 * time.Millisecond)
		browserMark(t, service, artifactID, "proofComment", "b1", "brown")
	}()
	anchored, err := service.VerifyMark(context.Background(), artifactID, MarkComment, "b1")
	if err != nil || anchored.Quote != "brown" {
		t.Fatalf("VerifyMark = %q, %v", anchored.Quote, err)
	}

	started := time.Now()
	if _, err := service.VerifyMark(context.Background(), artifactID, MarkComment, "never"); !errors.Is(err, ErrAnchorMissing) {
		t.Fatalf("missing mark err = %v", err)
	}
	if waited := time.Since(started); waited < 250*time.Millisecond || waited > time.Second {
		t.Fatalf("waited %v, want approximately MarkWait", waited)
	}
}

// Two processes serving one document can have one evict the room instance a verification
// subscribed to, so no update of that instance ever announces the mark the browser writes next.
// The wait ends with a read of the document as it then is, which finds the mark, rather than
// refusing an anchor the document holds.
func TestVerifyMarkReadsTheDocumentAgainWhenItsWaitEnds(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "")
	service := New(Deps{
		Store:    database,
		Events:   events.NewBroker(),
		Settle:   time.Hour,
		MarkWait: 300 * time.Millisecond,
	})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	seedServiceText(t, service, artifactID, "The quick brown fox")

	written := make(chan struct{})
	var replaced atomic.Int32
	service.afterReadWarm = func(room string) {
		if room != artifactID || !replaced.CompareAndSwap(0, 1) {
			return
		}
		// The verification subscribes to this instance as the hook returns, so evicting it here
		// leaves the mark to land in the instance that replaces it, announced to nobody. The
		// write waits out the verification's first read, which must find nothing.
		if err := service.Evict(context.Background(), room); err != nil {
			t.Errorf("evict the warm room: %v", err)
		}
		go func() {
			defer close(written)
			time.Sleep(100 * time.Millisecond)
			browserMark(t, service, artifactID, "proofComment", "b1", "brown")
		}()
	}
	anchored, err := service.VerifyMark(context.Background(), artifactID, MarkComment, "b1")
	<-written
	if err != nil || anchored.Quote != "brown" {
		t.Fatalf("VerifyMark of a mark written into the room that replaced the subscribed one = %q, %v, want brown", anchored.Quote, err)
	}
}

// Bob's browser comments on "quick brown", Alice's on "brown": the server reads each comment's own
// whole text, so neither Send is refused as a missing anchor and neither quote is cut.
func TestVerifyMarkFindsEachOfTwoCommentsOverOneWord(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "The quick brown fox")
	browserMarkWithAttrs(t, service, artifactID, "proofComment", "quick brown", pmdoc.Attrs{"id": "c1", "by": "user:bob"})
	browserMarkWithAttrs(t, service, artifactID, "proofComment", "brown", pmdoc.Attrs{"id": "c2", "by": "user:alice"})
	for id, want := range map[string]string{"c1": "quick brown", "c2": "brown"} {
		anchored, err := service.VerifyMark(context.Background(), artifactID, MarkComment, id)
		if err != nil || anchored.Quote != want {
			t.Fatalf("VerifyMark(%s) = %q, %v; want %q", id, anchored.Quote, err, want)
		}
	}
}

// An API caller's quote anchor over text another comment covers leaves that comment whole.
func TestMarkQuoteKeepsAnotherRecordOfTheKindOnTheSameText(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "The quick brown fox")
	for _, step := range []struct{ id, quote string }{{"c1", "quick brown"}, {"c2", "brown"}} {
		anchored, err := joinedMarkQuote(service, artifactID, MarkSpec{
			Kind: MarkComment, ID: step.id, By: model.Actor{Kind: "session", ID: "s1"},
		}, step.quote, nil)
		if err != nil || anchored.Quote != step.quote {
			t.Fatalf("MarkQuote(%s) = %q, %v", step.id, anchored.Quote, err)
		}
	}
	tree := liveTree(t, service, artifactID)
	for id, want := range map[string]string{"c1": "quick brown", "c2": "brown"} {
		if _, quote, ok := pmdoc.FindMark(tree, "proofComment", id); !ok || quote != want {
			t.Fatalf("FindMark(%s) = %q %t, want %q", id, quote, ok, want)
		}
	}
}

func TestSuggestionKindReadsBrowserMark(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "The quick brown fox")
	browserMarkWithAttrs(t, service, artifactID, "proofSuggestion", "quick", pmdoc.Attrs{
		"id": "insert-1", "by": "user:alice", "kind": "insert",
	})
	kind, err := service.SuggestionKind(context.Background(), artifactID, "insert-1")
	if err != nil || kind != "insert" {
		t.Fatalf("SuggestionKind = %q, %v, want insert", kind, err)
	}
}

func TestAcceptSuggestionReplacesMarkedTextAndRemovesTheMark(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "The quick brown fox")
	spec := MarkSpec{Kind: MarkSuggestion, ID: "s1", By: model.Actor{Kind: "session", ID: "s1"}}
	if _, err := joinedMarkQuote(service, artifactID, spec, "brown", nil); err != nil {
		t.Fatal(err)
	}
	if err := joinedAcceptSuggestion(service, artifactID, "s1", "*red*", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatal(err)
	}
	waitForDocumentText(t, service, artifactID, "The quick *red* fox\n")
	if _, _, ok := pmdoc.FindMark(liveTree(t, service, artifactID), "proofSuggestion", "s1"); ok {
		t.Fatal("mark survived accept")
	}
	if err := joinedAcceptSuggestion(service, artifactID, "s1", "x", model.Actor{Kind: "user", ID: "alice"}); !errors.Is(err, ErrAnchorOrphaned) {
		t.Fatalf("second accept err = %v", err)
	}
}

// A web address a person types in the browser editor, which has no autolink input rule, is plain
// text in the document's tree, though a reader of the markdown may take it for a link: here after
// a letter and `;`, seeded as a tree. A doc-edit replacing other text of its paragraph, and a
// suggestion accepted elsewhere in it, are not refused for how that address reads (INFRA-531).
func TestEditsBesideAPlainWebAddressInTheirParagraphAreAccepted(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "Placeholder.\n")
	editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		tree.Children[0].Children = []*pmdoc.Node{{Type: "text", Text: "The quick fox x;https://x.test jumps over the lazy dog"}}
		return tree
	})
	alice := model.Actor{Kind: "user", ID: "alice"}
	if _, err := joinedApplyOps(service, artifactID, []model.EditOp{{Op: "replace", Find: "quick", With: "slow"}}, alice, nil); err != nil {
		t.Fatalf("replace beside the address: %v", err)
	}
	spec := MarkSpec{Kind: MarkSuggestion, ID: "s1", By: model.Actor{Kind: "session", ID: "s1"}}
	if _, err := joinedMarkQuote(service, artifactID, spec, "lazy", nil); err != nil {
		t.Fatalf("suggest beside the address: %v", err)
	}
	if err := joinedAcceptSuggestion(service, artifactID, "s1", "sleepy", alice); err != nil {
		t.Fatalf("accept beside the address: %v", err)
	}
	const want = "The slow fox x;https://x.test jumps over the sleepy dog"
	if got := pmdoc.TextContent(liveTree(t, service, artifactID).Children[0]); got != want {
		t.Fatalf("the paragraph holds %q, want %q", got, want)
	}
}

// Rejecting a browser insert in code gives back the code the insert started from, in a typed block
// as anywhere: a reject is not read back.
func TestRejectSuggestionInCodeGivesBackTheCode(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, ":::callout{#c1 kind=\"note\" title=\"T\"}\n```\nabc added\n```\n:::\n")
	browserMarkWithAttrs(t, service, artifactID, "proofSuggestion", " added", pmdoc.Attrs{
		"id": "ins", "by": "user:bob", "kind": "insert",
	})
	if err := joinedRejectSuggestion(service, artifactID, "ins", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("reject: %v", err)
	}
	waitForDocumentText(t, service, artifactID, ":::callout{#c1 kind=\"note\" title=\"T\"}\n```\nabc\n```\n:::\n")
}

// Rejecting a browser insert deletes its text as the browser editor's reject does, and the
// document reads back as the live tree with no mark left. Runs of it that meet across a block
// boundary, nothing but the boundary between them, are one range, so the blocks join, which undoes
// the split an insert made (Enter typed while suggesting), and a table the range cuts keeps its
// width, its short row padded as the browser pads it. Runs with other text between them are each
// deleted where they stand, keeping that text, as the browser's reject does too: the pinned fork
// carries the split-mark fix (EveryInc/proof-sdk#83). Each want is the browser editor's result,
// written as this renderer writes it.
func TestRejectSuggestionDeletesTheInsertsText(t *testing.T) {
	const (
		table    = "\n\n| ZZNext | b |\n| --- | --- |\n| c | d |\n"
		narrowed = "\n\n|  | b |\n| --- | --- |\n| c | d |\n"
	)
	for _, test := range []struct {
		name, spec string
		runs       []string
		want       string
	}{
		{"code into a table cell", "Intro.\n\n```\nabcQQ\n```" + table, []string{"QQ", "ZZ"}, "Intro.\n\n```\nabcNext\n```" + narrowed},
		{"code in a callout into a table cell", ":::callout{#c1 kind=\"note\" title=\"T\"}\n```\nabcQQ\n```\n:::" + table, []string{"QQ", "ZZ"}, ":::callout{#c1 kind=\"note\" title=\"T\"}\n```\nabcNext\n```\n:::" + narrowed},
		{"code into a whole table cell", "Intro.\n\n```\nabcQQ\n```\n\n| ZZ | b |\n| --- | --- |\n| c | d |\n", []string{"QQ", "ZZ"}, "Intro.\n\n```\nabc\n```" + narrowed},
		{"a paragraph into a table cell", "Intro QQ" + table, []string{"QQ", "ZZ"}, "Intro Next" + narrowed},
		{"a paragraph split by the insert", "HelloQQ\n\nZZ world.\n", []string{"QQ", "ZZ"}, "Hello world.\n"},
		{"a list item split by the insert", "- HelloQQ\n- ZZ world.\n", []string{"QQ", "ZZ"}, "- Hello world.\n"},
		{"a heading split by the insert", "# HelloQQ\n\n# ZZ world.\n", []string{"QQ", "ZZ"}, "# Hello world.\n"},
		{"a heading into a paragraph", "# HelloQQ\n\nZZ world.\n", []string{"QQ", "ZZ"}, "# Hello world.\n"},
		{"two runs in one paragraph, text between them", "keep QQ this ZZ drop\n", []string{"QQ", "ZZ"}, "keep  this  drop\n"},
		{"a paragraph into a one-column table's header cell", "Intro QQ\n\n| ZZNext |\n| --- |\n| c |\n", []string{"QQ", "ZZ"}, "Intro Next\n\n|  |\n| --- |\n| c |\n"},
		{"a paragraph into an aligned table's first header cell", "abcQQ\n\n| ZZa | b | e |\n| :---: | :--- | ---: |\n| c | d | f |\n", []string{"QQ", "ZZ"}, "abca\n\n|  | b | e |\n| :---: | :--- | ---: |\n| c | d | f |\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			seedServiceText(t, service, artifactID, test.spec)
			for _, run := range test.runs {
				browserMarkWithAttrs(t, service, artifactID, "proofSuggestion", run, pmdoc.Attrs{
					"id": "ins", "by": "user:bob", "kind": "insert",
				})
			}
			if err := joinedRejectSuggestion(service, artifactID, "ins", model.Actor{Kind: "user", ID: "alice"}); err != nil {
				t.Fatalf("reject: %v", err)
			}
			waitForDocumentText(t, service, artifactID, test.want)
			live := liveTree(t, service, artifactID)
			for _, block := range live.Children {
				if err := pmdoc.BlockShapeError(block); err != nil {
					t.Errorf("after the reject a %s reads back otherwise: %v", block.Type, err)
				}
			}
			if len(pmdoc.ListMarks(live)) != 0 {
				t.Fatal("the insert's mark survived the reject")
			}
		})
	}
}

// A reject whose removal would join blocks the document cannot hold together is refused in the
// words of a reject, naming the suggestion's anchor, and the document stays as it was: an insert
// running from the text before into a callout or an ask, which the join would leave empty, and one
// running from one table into the next, which the browser joins into one table, and one whose
// removal leaves a task item the renderer cannot write.
func TestRejectSuggestionRefusesARemovalTheDocumentCannotHold(t *testing.T) {
	const callout = ":::callout{#c1 kind=\"note\" title=\"T\"}\nZZ world.\n:::\n"
	for _, test := range []struct {
		name, spec string
		runs       []string
		says       string
	}{
		{"a paragraph into a callout", "HelloQQ\n\n" + callout, nil, "runs into an ask or callout"},
		{"a list item into a callout in it", "- HelloQQ\n\n  :::callout{#c1 kind=\"note\" title=\"T\"}\n  ZZ world.\n  :::\n", nil, "runs into an ask or callout"},
		{"a paragraph into an ask", "HelloQQ\n\n:::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nZZ Which?\n:::\n", nil, "runs into an ask or callout"},
		{"a callout into a callout", ":::callout{#c0 kind=\"note\" title=\"U\"}\nHelloQQ\n:::\n\n" + callout, nil, "runs into an ask or callout"},
		{"code into a callout", "```\nabcQQ\n```\n\n" + callout, nil, "runs into an ask or callout"},
		{"one table into the next", "| a |\n| --- |\n| bQQ |\n\n| ZZc |\n| --- |\n| d |\n", nil, "from one table into the next"},
		{"a task item's whole text, before a nested list", "- [ ] QQ\n  - child\n", []string{"QQ"}, "a task item whose first paragraph is empty"},
		{"a task item's whole text, before a second paragraph", "- [ ] QQ\n\n  more\n", []string{"QQ"}, "a task item whose first paragraph is empty"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			seedServiceText(t, service, artifactID, test.spec)
			runs := test.runs
			if runs == nil {
				runs = []string{"QQ", "ZZ"}
			}
			for _, run := range runs {
				browserMarkWithAttrs(t, service, artifactID, "proofSuggestion", run, pmdoc.Attrs{
					"id": "ins", "by": "user:bob", "kind": "insert",
				})
			}
			before, err := service.Text(context.Background(), artifactID)
			if err != nil {
				t.Fatal(err)
			}
			err = joinedRejectSuggestion(service, artifactID, "ins", model.Actor{Kind: "user", ID: "alice"})
			var invalid *ErrInvalidOp
			if !errors.As(err, &invalid) || invalid.Field != "anchor" || !strings.Contains(invalid.Reason, test.says) || !strings.Contains(invalid.Reason, "accept the suggestion") {
				t.Fatalf("reject: %v, want an invalid anchor saying %q and offering to accept the suggestion", err, test.says)
			}
			if after, err := service.Text(context.Background(), artifactID); err != nil || after != before {
				t.Fatalf("after the refused reject = %q (%v), want unchanged %q", after, err, before)
			}
		})
	}
}

func TestRejectSuggestionIsKindAware(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "keep this and drop that")
	if _, err := joinedMarkQuote(service, artifactID, MarkSpec{
		Kind: MarkSuggestion,
		ID:   "rep",
		By:   model.Actor{Kind: "session", ID: "s1"},
	}, "this", nil); err != nil {
		t.Fatal(err)
	}
	browserMarkWithAttrs(t, service, artifactID, "proofSuggestion", "that", pmdoc.Attrs{
		"id":   "ins",
		"by":   "user:bob",
		"kind": "insert",
	})
	if err := joinedRejectSuggestion(service, artifactID, "rep", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := joinedRejectSuggestion(service, artifactID, "ins", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatal(err)
	}
	// The space the rejected insert leaves at the paragraph's end is kept, as a reference.
	waitForDocumentText(t, service, artifactID, "keep this and drop&#32;\n")
	if len(pmdoc.ListMarks(liveTree(t, service, artifactID))) != 0 {
		t.Fatal("marks survived reject")
	}
}

func TestProjectMarkWritesProofStoredMark(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "The quick brown fox")
	record := MarkRecord{
		Kind:      "comment",
		By:        "user:alice",
		CreatedAt: "2026-09-10T00:00:00Z",
		Text:      "why?",
		Replies: []MarkReply{{
			By:   "session:s1",
			Text: "because",
			At:   "2026-09-10T00:01:00Z",
		}},
	}
	if err := joinedProjectMark(service, artifactID, "c1", record, model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := service.srv.Apply(context.Background(), artifactID, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
		value, _ := doc.GetMap(marksMapName).Get("c1")
		got, _ = value.(map[string]any)
	}); err != nil && !errors.Is(err, ygws.ErrNoChanges) {
		t.Fatal(err)
	}
	if got["kind"] != "comment" || got["text"] != "why?" || got["resolved"] != false || len(got["replies"].([]any)) != 1 {
		t.Fatalf("projection = %#v", got)
	}
}

func browserMark(t *testing.T, service *Service, artifactID, markType, id, quote string) {
	t.Helper()
	browserMarkWithAttrs(t, service, artifactID, markType, quote, pmdoc.Attrs{"id": id, "by": "user:bob"})
}

func browserMarkWithAttrs(t *testing.T, service *Service, artifactID, markType, quote string, attrs pmdoc.Attrs) {
	t.Helper()
	err := service.srv.Apply(context.Background(), artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		fragment := doc.GetXmlFragment(fragmentName)
		tree, err := treeOf(doc)
		if err != nil {
			t.Errorf("read browser tree: %v", err)
			return
		}
		range_, err := pmdoc.FindQuote(tree, quote, nil, nil)
		if err != nil {
			t.Errorf("find browser quote: %v", err)
			return
		}
		transact(func(txn *crdt.Transaction) {
			if err := pmdoc.MarkRange(txn, fragment, range_, pmdoc.Mark{Type: markType, Attrs: attrs}); err != nil {
				t.Errorf("mark browser quote: %v", err)
			}
		})
	})
	if err != nil {
		t.Errorf("apply browser mark: %v", err)
	}
}

func TestVersionWriteRefreshesAnchorsByMark(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = 20 * time.Millisecond
	seedServiceText(t, service, artifactID, "The quick brown fox")
	askID := insertAnchoredAsk(t, service, artifactID, "brown")
	outerID := insertAnchoredCommentWithID(t, service, artifactID, "00000000-0000-4000-8000-000000000003", "quick brown")
	innerID := insertAnchoredCommentWithID(t, service, artifactID, "00000000-0000-4000-8000-000000000004", "brown")

	editLiveTree(t, service, artifactID, replaceRun("brown", "browner"))
	waitFor(t, time.Second, "anchor refresh to browner", func() bool {
		anchor := loadAskAnchor(t, service, askID)
		return anchor.Quote == "browner" && !anchor.Orphaned && loadCommentAnchor(t, service, innerID).Quote == "browner"
	})
	if anchor := loadAskAnchor(t, service, askID); anchor.Quote != "browner" || anchor.Orphaned {
		t.Fatalf("refreshed anchor = %#v, want browner and not orphaned", anchor)
	}
	// The run both comments share was rewritten: each comment's refresh reads its own whole text.
	for id, want := range map[string]string{outerID: "quick browner", innerID: "browner"} {
		if anchor := loadCommentAnchor(t, service, id); anchor.Quote != want || anchor.Orphaned {
			t.Fatalf("comment %s anchor = %#v, want %q and not orphaned", id, anchor, want)
		}
	}

	editLiveTree(t, service, artifactID, deleteRun("browner"))
	waitFor(t, time.Second, "anchor orphaning after browner deletion", func() bool {
		anchor := loadAskAnchor(t, service, askID)
		return anchor.Orphaned && anchor.Version == 1
	})
	if anchor := loadAskAnchor(t, service, askID); !anchor.Orphaned || anchor.Version != 1 {
		t.Fatalf("orphaned anchor = %#v, want version-1 orphan", anchor)
	}
}

// markSweepTTL is the TTL the sweep ages a mark by in every test that holds the clock below.
const markSweepTTL = time.Minute

// newUnrecordedMarkService is a service whose clock the tests of the unrecorded-mark sweep hold:
// a mark's age is what the test sets with the returned age function and never how long a loaded
// runner took. Settlement is armed by hand (settleCurrentGeneration); a sweep the re-arm runs in
// real time reads the held clock and so decides as the test's own settlements do.
func newUnrecordedMarkService(t *testing.T) (*Service, string, func(age time.Duration)) {
	t.Helper()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "")
	service := New(Deps{
		Store:             database,
		Events:            events.NewBroker(),
		Settle:            time.Hour,
		UnrecordedMarkTTL: markSweepTTL,
	})
	started := time.Now()
	var aged atomic.Int64
	service.now = func() time.Time { return started.Add(time.Duration(aged.Load())) }
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	return service, artifactID, func(age time.Duration) { aged.Store(int64(age)) }
}

// markIsInTheDocument reports whether the live document still carries a comment mark.
func markIsInTheDocument(t *testing.T, service *Service, artifactID, id string) bool {
	t.Helper()
	_, _, found := pmdoc.FindMark(liveTree(t, service, artifactID), "proofComment", id)
	return found
}

// An unrecorded comment mark - one no comment, ask or suggestion row records - stays until the
// first settlement that saw it is UnrecordedMarkTTL old, and the first settlement after that
// sweeps it; a recorded mark, resolved or not, and a proof-authored mark stay. Bob, who wrote the
// dangling mark, has no browser on the room, which is what lets it age at all: carol watches the
// document throughout, and a mark is kept for its own author's presence alone.
func TestSettleSweepsUnrecordedMarksAfterTTL(t *testing.T) {
	service, artifactID, age := newUnrecordedMarkService(t)
	seedServiceText(t, service, artifactID, "The quick brown fox")
	browserMark(t, service, artifactID, "proofComment", "dangling", "quick")
	browserMarkWithAttrs(t, service, artifactID, "proofAuthored", "fox", pmdoc.Attrs{"id": "auth", "by": "user:bob"})
	service.addConnection(artifactID, 1, model.Actor{Kind: "user", ID: "carol"})
	resolvedCommentID := insertAnchoredComment(t, service, artifactID, "brown")
	if _, err := service.store.Pool.Exec(context.Background(), `update comments set resolved = true where id = $1`, resolvedCommentID); err != nil {
		t.Fatalf("resolve recorded comment: %v", err)
	}

	settleCurrentGeneration(t, service, artifactID)
	age(markSweepTTL / 2)
	settleCurrentGeneration(t, service, artifactID)
	if !markIsInTheDocument(t, service, artifactID, "dangling") {
		t.Fatal("unrecorded mark swept before its TTL")
	}
	age(markSweepTTL)
	settleCurrentGeneration(t, service, artifactID)
	if markIsInTheDocument(t, service, artifactID, "dangling") {
		t.Fatal("unrecorded mark not swept once its TTL passed")
	}
	if _, _, found := pmdoc.FindMark(liveTree(t, service, artifactID), "proofAuthored", "auth"); !found {
		t.Fatal("proof-authored mark was swept")
	}
	if _, _, found := pmdoc.FindMark(liveTree(t, service, artifactID), "proofComment", resolvedCommentID); !found {
		t.Fatal("recorded resolved comment mark was swept")
	}
}

// A reader with a composer open holds the provisional mark it is anchored to: an unrecorded mark
// whose author has a browser on the room is kept however long the room settles, whichever
// spelling of the author the mark carries - the bare login the browser writes, or the
// user:<login> of a mark the server wrote.
func TestAnUnrecordedMarkOfAConnectedAuthorIsKept(t *testing.T) {
	for _, test := range []struct{ name, by string }{
		{"the bare login a browser writes", "bob"},
		{"the actor reference a server write carries", "user:bob"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID, age := newUnrecordedMarkService(t)
			seedServiceText(t, service, artifactID, "The quick brown fox")
			browserMarkWithAttrs(t, service, artifactID, "proofComment", "quick", pmdoc.Attrs{"id": "composing", "by": test.by})
			service.addConnection(artifactID, 1, model.Actor{Kind: "user", ID: "bob"})

			settleCurrentGeneration(t, service, artifactID)
			for _, elapsed := range []time.Duration{markSweepTTL, 2 * markSweepTTL, 3 * markSweepTTL} {
				age(elapsed)
				settleCurrentGeneration(t, service, artifactID)
				if !markIsInTheDocument(t, service, artifactID, "composing") {
					t.Fatalf("the open composer's mark was swept %v after it was written, with its author connected", elapsed)
				}
			}
		})
	}
}

// The tab that held the composer closes: the mark it left behind ages from the author's last
// presence, so it survives half a TTL after the disconnection and is swept a TTL after it.
func TestAnUnrecordedMarkIsSweptOneTTLAfterItsAuthorDisconnects(t *testing.T) {
	service, artifactID, age := newUnrecordedMarkService(t)
	seedServiceText(t, service, artifactID, "The quick brown fox")
	browserMarkWithAttrs(t, service, artifactID, "proofComment", "quick", pmdoc.Attrs{"id": "abandoned", "by": "bob"})
	service.addConnection(artifactID, 1, model.Actor{Kind: "user", ID: "bob"})
	settleCurrentGeneration(t, service, artifactID)
	age(2 * markSweepTTL)
	settleCurrentGeneration(t, service, artifactID)
	service.removeConnection(artifactID, 1)

	age(2*markSweepTTL + markSweepTTL/2)
	settleCurrentGeneration(t, service, artifactID)
	if !markIsInTheDocument(t, service, artifactID, "abandoned") {
		t.Fatal("the mark was swept half a TTL after its author's browser went")
	}
	age(3 * markSweepTTL)
	settleCurrentGeneration(t, service, artifactID)
	if markIsInTheDocument(t, service, artifactID, "abandoned") {
		t.Fatal("the mark outlived a TTL of its author's absence")
	}
}

// The minute runs from the author's last presence, not from the sweep that last saw them. On a
// quiet document the sweeps are a TTL apart, so bob's browser dropping a second before one - an
// idle reconnect - must not cost him the composer he is writing in: the sweep that first misses
// him keeps the mark, and the mark goes a TTL after the drop (at three TTL less a second), a
// whole TTL after the sweep that last saw him (at one TTL).
func TestAnUnrecordedMarkOutlivesASweepRunJustAfterItsAuthorDropped(t *testing.T) {
	service, artifactID, age := newUnrecordedMarkService(t)
	seedServiceText(t, service, artifactID, "The quick brown fox")
	browserMarkWithAttrs(t, service, artifactID, "proofComment", "quick", pmdoc.Attrs{"id": "composing", "by": "bob"})
	service.addConnection(artifactID, 1, model.Actor{Kind: "user", ID: "bob"})
	age(markSweepTTL)
	settleCurrentGeneration(t, service, artifactID)

	age(2*markSweepTTL - time.Second)
	service.removeConnection(artifactID, 1)
	age(2 * markSweepTTL)
	settleCurrentGeneration(t, service, artifactID)
	if !markIsInTheDocument(t, service, artifactID, "composing") {
		t.Fatal("the mark went a second after its author's browser dropped, a TTL after the sweep that last saw him")
	}
	age(3*markSweepTTL - 2*time.Second)
	settleCurrentGeneration(t, service, artifactID)
	if !markIsInTheDocument(t, service, artifactID, "composing") {
		t.Fatal("the mark went a second short of a TTL after its author's browser dropped")
	}
	age(3*markSweepTTL - time.Second)
	settleCurrentGeneration(t, service, artifactID)
	if markIsInTheDocument(t, service, artifactID, "composing") {
		t.Fatal("the mark outlived a TTL of its author's absence")
	}
}

// The dropped browser comes back: bob is writing in the composer again, so the mark lives past
// the deadline the drop set and is kept however long the room settles after it.
func TestAnUnrecordedMarkOutlivesItsDropDeadlineWhenItsAuthorComesBack(t *testing.T) {
	service, artifactID, age := newUnrecordedMarkService(t)
	seedServiceText(t, service, artifactID, "The quick brown fox")
	browserMarkWithAttrs(t, service, artifactID, "proofComment", "quick", pmdoc.Attrs{"id": "composing", "by": "bob"})
	service.addConnection(artifactID, 1, model.Actor{Kind: "user", ID: "bob"})
	settleCurrentGeneration(t, service, artifactID)

	age(markSweepTTL / 2)
	service.removeConnection(artifactID, 1)
	age(markSweepTTL)
	service.addConnection(artifactID, 2, model.Actor{Kind: "user", ID: "bob"})
	for _, elapsed := range []time.Duration{2 * markSweepTTL, 3 * markSweepTTL} {
		age(elapsed)
		settleCurrentGeneration(t, service, artifactID)
		if !markIsInTheDocument(t, service, artifactID, "composing") {
			t.Fatalf("the mark was swept %v after it was written, its author back since half a TTL in", elapsed)
		}
	}
}

// Bob has the document open in two tabs. Closing one leaves the composer open in the other, so
// the mark's minute starts only when the second goes: no settlement runs between the two closes,
// so a minute counted from the first would have taken the mark by the check below.
func TestAnUnrecordedMarkAgesFromItsAuthorsLastBrowserNotTheFirstToClose(t *testing.T) {
	service, artifactID, age := newUnrecordedMarkService(t)
	seedServiceText(t, service, artifactID, "The quick brown fox")
	browserMarkWithAttrs(t, service, artifactID, "proofComment", "quick", pmdoc.Attrs{"id": "composing", "by": "bob"})
	service.addConnection(artifactID, 1, model.Actor{Kind: "user", ID: "bob"})
	service.addConnection(artifactID, 2, model.Actor{Kind: "user", ID: "bob"})
	settleCurrentGeneration(t, service, artifactID)

	age(markSweepTTL / 2)
	service.removeConnection(artifactID, 1)
	age(2 * markSweepTTL)
	service.removeConnection(artifactID, 2)

	age(2*markSweepTTL + markSweepTTL/2)
	settleCurrentGeneration(t, service, artifactID)
	if !markIsInTheDocument(t, service, artifactID, "composing") {
		t.Fatal("the mark's minute ran from the first of its author's two tabs to close")
	}
	age(3 * markSweepTTL)
	settleCurrentGeneration(t, service, artifactID)
	if markIsInTheDocument(t, service, artifactID, "composing") {
		t.Fatal("the mark outlived a TTL of its author's absence")
	}
}

// A mark naming no author is nobody's open composer: it is swept at its TTL, whoever is in the
// room.
func TestAnUnrecordedMarkWithNoAuthorIsSweptAtItsTTL(t *testing.T) {
	service, artifactID, age := newUnrecordedMarkService(t)
	seedServiceText(t, service, artifactID, "The quick brown fox")
	browserMarkWithAttrs(t, service, artifactID, "proofComment", "quick", pmdoc.Attrs{"id": "anonymous"})
	service.addConnection(artifactID, 1, model.Actor{Kind: "user", ID: "bob"})

	settleCurrentGeneration(t, service, artifactID)
	age(markSweepTTL)
	settleCurrentGeneration(t, service, artifactID)
	if markIsInTheDocument(t, service, artifactID, "anonymous") {
		t.Fatal("a mark naming no author outlived its TTL")
	}
}

// An ask whose anchor names document A and whose block_artifact_id names document B records its
// mark on A, so the mark outlives UnrecordedMarkTTL there while an unrecorded mark on A is swept.
func TestSettleKeepsTheAnchoredMarkOfAnAskThatAlsoNamesABlock(t *testing.T) {
	database := storetest.Open(t)
	documentA := createDocument(t, database, "")
	documentB := createDocument(t, database, "")
	const ttl = time.Minute
	service := New(Deps{
		Store:             database,
		Events:            events.NewBroker(),
		Settle:            time.Hour,
		UnrecordedMarkTTL: ttl,
	})
	started := time.Now()
	var aged atomic.Int64
	service.now = func() time.Time { return started.Add(time.Duration(aged.Load())) }
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	seedServiceText(t, service, documentA, "The quick brown fox")
	seedServiceText(t, service, documentB, "The quick brown fox")
	askID := insertAnchoredAsk(t, service, documentA, "brown")
	if _, err := database.Pool.Exec(context.Background(), `update asks set block_artifact_id = $1 where id = $2`, documentB, askID); err != nil {
		t.Fatalf("name the ask's block document: %v", err)
	}
	browserMark(t, service, documentA, "proofComment", "dangling", "quick")

	settleCurrentGeneration(t, service, documentA)
	aged.Store(int64(ttl))
	settleCurrentGeneration(t, service, documentA)
	tree := liveTree(t, service, documentA)
	if _, _, found := pmdoc.FindMark(tree, "proofComment", "dangling"); found {
		t.Fatal("unrecorded mark not swept once its TTL passed")
	}
	if _, _, found := pmdoc.FindMark(tree, "dispatchAsk", askID); !found {
		t.Fatal("anchored mark of an ask that also names a block on another document was swept")
	}
}

func TestMarkOnlyUpdateSweepsUnrecordedMarksWithoutCanonicalizingLegacyTable(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = 10 * time.Millisecond
	service.unrecordedMarkTTL = 40 * time.Millisecond
	seedServiceText(t, service, artifactID, "| header |\n| :--- |\n| `one\\|two` |\n")
	if _, err := service.store.Pool.Exec(context.Background(), `
		update artifact_versions set markdown = $2 where artifact_id = $1 and number = 1
	`, artifactID, "| header |\n| :--- |\n| `one|two` |\n"); err != nil {
		t.Fatalf("seed legacy canonical markdown: %v", err)
	}
	alignLatestVersionWithUpdates(t, service, artifactID)
	browserMark(t, service, artifactID, "proofComment", "dangling", "one|two")
	waitFor(t, time.Second, "unrecorded mark removed", func() bool {
		_, _, found := pmdoc.FindMark(liveTree(t, service, artifactID), "proofComment", "dangling")
		return !found
	})
	assertTableCellPipeVersionAndEventCounts(t, service.store, artifactID, 1, 0)
}

func TestCompactionRetainsContentClassificationAcrossMarkUpdates(t *testing.T) {
	for _, keep := range []int{1, 500} {
		t.Run(fmt.Sprintf("keep_%d", keep), func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			seedServiceText(t, service, artifactID, "before")
			if _, err := joinedReplaceText(service, artifactID, "after", model.Actor{Kind: "user", ID: "alice"}); err != nil {
				t.Fatalf("write content update: %v", err)
			}
			waitForPersistedProofText(t, service.store, artifactID, "after\n")
			for index := 0; index <= keep; index++ {
				browserMarkWithAttrs(t, service, artifactID, "proofAuthored", "after", pmdoc.Attrs{
					"id": fmt.Sprintf("mark-%d", index), "by": "user:alice",
				})
			}
			waitForPersistedUpdates(t, service, artifactID, keep+2)
			persist := service.persistence.(*PgVersioned)
			if _, err := persist.Compact(context.Background(), artifactID, keep); err != nil {
				t.Fatalf("compact updates: %v", err)
			}
			if err := service.Evict(context.Background(), artifactID); err != nil {
				t.Fatalf("evict compacted document: %v", err)
			}
			if got, err := service.Text(context.Background(), artifactID); err != nil || got != "after\n" {
				t.Fatalf("reload compacted text = %q (%v), want after", got, err)
			}
			settleCurrentGeneration(t, service, artifactID)
			assertTableCellPipeVersionAndEventCounts(t, service.store, artifactID, 2, 1)
		})
	}
}

func TestCompactionDoesNotCarryCoveredContentClassificationPastCursor(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "| header |\n| :--- |\n| `one\\|two` |\n")
	if _, err := service.store.Pool.Exec(context.Background(), `
		update artifact_versions set markdown = $2 where artifact_id = $1 and number = 1
	`, artifactID, "| header |\n| :--- |\n| `one|two` |\n"); err != nil {
		t.Fatalf("seed legacy canonical markdown: %v", err)
	}
	alignLatestVersionWithUpdates(t, service, artifactID)
	for index := range compactKeep + 1 {
		browserMarkWithAttrs(t, service, artifactID, "proofAuthored", "one|two", pmdoc.Attrs{
			"id": fmt.Sprintf("covered-mark-%d", index), "by": "user:alice",
		})
	}
	waitForPersistedUpdates(t, service, artifactID, compactKeep+2)
	persist := service.persistence.(*PgVersioned)
	if _, err := persist.Compact(context.Background(), artifactID, compactKeep); err != nil {
		t.Fatalf("compact updates: %v", err)
	}
	if err := service.Evict(context.Background(), artifactID); err != nil {
		t.Fatalf("evict compacted document: %v", err)
	}
	if got, err := service.Text(context.Background(), artifactID); err != nil || got != "| header |\n| :--- |\n| `one\\|two` |\n" {
		t.Fatalf("reload compacted text = %q (%v)", got, err)
	}
	settleCurrentGeneration(t, service, artifactID)
	assertTableCellPipeVersionAndEventCounts(t, service.store, artifactID, 1, 0)
}

// The fold covers a content update past the latest version's cursor, between mark-only updates,
// and the folded row keeps its content_changed, so settlement versions the content.
func TestCompactionRetainsUncoveredContentBeyondVersionCursor(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	alignLatestVersionWithUpdates(t, service, artifactID)
	for index := range 2 {
		browserMarkWithAttrs(t, service, artifactID, "proofAuthored", "before", pmdoc.Attrs{
			"id": fmt.Sprintf("before-mark-%d", index), "by": "user:alice",
		})
	}
	waitForPersistedUpdates(t, service, artifactID, 3)
	if _, err := joinedReplaceText(service, artifactID, "after", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("write uncovered content update: %v", err)
	}
	waitForPersistedProofText(t, service.store, artifactID, "after\n")
	for index := range 2 {
		browserMarkWithAttrs(t, service, artifactID, "proofAuthored", "after", pmdoc.Attrs{
			"id": fmt.Sprintf("after-mark-%d", index), "by": "user:alice",
		})
	}
	waitForPersistedUpdates(t, service, artifactID, 6)
	persist := service.persistence.(*PgVersioned)
	if _, err := persist.Compact(context.Background(), artifactID, compactKeep); err != nil {
		t.Fatalf("compact updates: %v", err)
	}
	if stored := storedUpdateCount(t, service.store, artifactID); stored != compactKeep {
		t.Fatalf("compaction left %d stored updates, want %d", stored, compactKeep)
	}
	if err := service.Evict(context.Background(), artifactID); err != nil {
		t.Fatalf("evict compacted document: %v", err)
	}
	if got, err := service.Text(context.Background(), artifactID); err != nil || got != "after\n" {
		t.Fatalf("reload compacted text = %q (%v), want after", got, err)
	}
	settleCurrentGeneration(t, service, artifactID)
	assertTableCellPipeVersionAndEventCounts(t, service.store, artifactID, 2, 1)
}

func TestProjectMarkRearmsPendingSettlement(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = 100 * time.Millisecond
	seedServiceText(t, service, artifactID, "before")
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))

	tx, err := service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin projection transaction: %v", err)
	}
	joined, ledger := service.Join(context.Background(), tx)
	if err := service.ProjectMark(joined, artifactID, "c1", MarkRecord{
		Kind: "comment", By: "user:alice", CreatedAt: "2026-09-10T00:00:00Z", Text: "note",
	}, model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("project mark: %v", err)
	}
	if err := ledger.commit(context.Background()); err != nil {
		t.Fatalf("commit projection transaction: %v", err)
	}
	ledger.publish()
	waitFor(t, time.Second, "settled projection version for after", func() bool {
		var markdown string
		var named bool
		if err := service.store.Pool.QueryRow(context.Background(), `
			select markdown, named from artifact_versions
			where artifact_id = $1
			order by number desc limit 1
		`, artifactID).Scan(&markdown, &named); err != nil {
			t.Fatalf("read settled projection version: %v", err)
		}
		return markdown == "after\n" && !named
	})
}

// An anchored comment writes its quote mark and its margin projection inside the comment's own
// transaction. Neither changes the document's content, so the settlement after them records no
// version: a version no edit produced would stale an approval pinned to the latest one.
func TestTransactionalMarkWritesSettleWithoutAVersion(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "The quick brown fox")
	alignLatestVersionWithUpdates(t, service, artifactID)
	tx, err := service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin comment transaction: %v", err)
	}
	defer tx.Rollback(context.Background())
	ctx, ledger := service.Join(context.Background(), tx)
	alice := model.Actor{Kind: "user", ID: "alice"}
	if _, err := service.MarkQuote(ctx, artifactID, MarkSpec{Kind: MarkComment, ID: "c1", By: alice}, "quick", nil); err != nil {
		t.Fatalf("mark quote: %v", err)
	}
	if err := service.ProjectMark(ctx, artifactID, "c1", MarkRecord{
		Kind: "comment", By: "user:alice", CreatedAt: "2026-09-23T00:00:00Z", Text: "note",
	}, alice); err != nil {
		t.Fatalf("project mark: %v", err)
	}
	if err := ledger.commit(context.Background()); err != nil {
		t.Fatalf("commit comment transaction: %v", err)
	}
	ledger.publish()
	settleCurrentGeneration(t, service, artifactID)
	assertTableCellPipeVersionAndEventCounts(t, service.store, artifactID, 1, 0)
}

func TestReplaceTextReanchorsOpenRows(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = 20 * time.Millisecond
	seedServiceText(t, service, artifactID, "The quick brown fox")
	askID := insertAnchoredAsk(t, service, artifactID, "brown")
	commentID := insertAnchoredComment(t, service, artifactID, "fox")
	// Two comments of which one covers part of the other: re-anchoring the second must not cut
	// the first.
	outerID := insertAnchoredCommentWithID(t, service, artifactID, "00000000-0000-4000-8000-000000000003", "quick brown")
	innerID := insertAnchoredCommentWithID(t, service, artifactID, "00000000-0000-4000-8000-000000000004", "brown")

	if _, err := joinedReplaceText(service, artifactID, "A quick brown dog and a slow brown cat", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("replace text: %v", err)
	}
	// The anchored ask and comment above schedule their own settlements, so version 2 can be the
	// pre-replace snapshot; wait for the replacement's settled state, not a version number.
	waitFor(t, time.Second, "the deleted quote's comment to orphan", func() bool {
		return loadCommentAnchor(t, service, commentID).Orphaned
	})
	if anchor := loadAskAnchor(t, service, askID); anchor.Orphaned || anchor.Quote != "brown" {
		t.Fatalf("ask anchor after replace = %#v, want first brown mark", anchor)
	}
	tree := liveTree(t, service, artifactID)
	if _, quote, found := pmdoc.FindMark(tree, "dispatchAsk", askID); !found || quote != "brown" {
		t.Fatalf("ask mark after replace = %q found=%t, want brown", quote, found)
	}
	for id, want := range map[string]string{outerID: "quick brown", innerID: "brown"} {
		if _, quote, found := pmdoc.FindMark(tree, "proofComment", id); !found || quote != want {
			t.Fatalf("comment %s after replace = %q found=%t, want %q", id, quote, found, want)
		}
		if anchor := loadCommentAnchor(t, service, id); anchor.Orphaned || anchor.Quote != want {
			t.Fatalf("comment %s anchor after replace = %#v, want %q", id, anchor, want)
		}
	}
}

// A replace through the edit API keeps an open anchor over the text it wrote wherever that text
// lands inside the anchor, so the refreshed quote is the anchor's whole current extent rather than
// the run of the mark before the edit, which would read "quick " after "brown" became "red". A
// replace that runs past the anchor's edge rewrote text outside it as well, so the anchor keeps
// only the text the replace left alone.
func TestAReplaceKeepsTheAnchorOverItsWholeCurrentExtent(t *testing.T) {
	const text = "The quick brown fox jumps over the lazy dog"
	const quote = "quick brown fox jumps"
	for _, test := range []struct {
		name, find, with, want string
		orphaned               bool
	}{
		{name: "a word inside", find: "brown", with: "red", want: "quick red fox jumps"},
		{name: "words inside", find: "brown fox", with: "red fox", want: "quick red fox jumps"},
		{name: "the first word", find: "quick", with: "slow", want: "slow brown fox jumps"},
		{name: "the last word", find: "jumps", with: "leaps", want: "quick brown fox leaps"},
		{name: "the whole quote", find: quote, with: "sleepy cat naps", want: "sleepy cat naps"},
		{name: "a deletion inside", find: "brown ", with: "", want: "quick fox jumps"},
		{name: "an insertion inside", find: "brown", with: "very brown", want: "quick very brown fox jumps"},
		{name: "across the end", find: "jumps over", with: "leaps across", want: "quick brown fox "},
		{name: "across the start", find: "The quick", with: "A slow", want: " brown fox jumps"},
		{name: "over the whole quote and more", find: "The " + quote, with: "A cat", orphaned: true, want: quote},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			seedServiceText(t, service, artifactID, text)
			askID := insertAnchoredAsk(t, service, artifactID, quote)
			commentID := insertAnchoredComment(t, service, artifactID, quote)

			ctx := context.Background()
			tx, err := service.store.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin edit: %v", err)
			}
			defer tx.Rollback(ctx)
			joined, ledger := service.Join(ctx, tx)
			defer ledger.Discard()
			if _, err := service.ApplyOps(joined, artifactID, []model.EditOp{{
				Op: "replace", Find: test.find, With: test.with,
			}}, model.Actor{Kind: "user", ID: "alice"}, nil); err != nil {
				t.Fatalf("replace %q with %q: %v", test.find, test.with, err)
			}
			if err := ledger.Commit(ctx); err != nil {
				t.Fatalf("commit edit: %v", err)
			}

			for _, anchor := range []struct {
				kind   string
				stored storedAnchor
			}{
				{kind: "ask", stored: loadAskAnchor(t, service, askID)},
				{kind: "comment", stored: loadCommentAnchor(t, service, commentID)},
			} {
				if anchor.stored.Quote != test.want || anchor.stored.Orphaned != test.orphaned {
					t.Errorf("%s anchor after replacing %q with %q = quote %q orphaned %t, want quote %q orphaned %t",
						anchor.kind, test.find, test.with, anchor.stored.Quote, anchor.stored.Orphaned, test.want, test.orphaned)
				}
			}
		})
	}
}

// Accepting a suggestion inside a comment's quote writes its text inside that comment too, so the
// comment's quote is its whole current extent; the accepted suggestion's own mark goes with the
// text it replaced.
func TestAnAcceptedSuggestionStaysInsideTheCommentAroundIt(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "The quick brown fox jumps over the lazy dog")
	commentID := insertAnchoredComment(t, service, artifactID, "quick brown fox jumps")
	if _, err := joinedMarkQuote(service, artifactID, MarkSpec{
		Kind: MarkSuggestion, ID: "s1", By: model.Actor{Kind: "session", ID: "s1"},
	}, "brown", nil); err != nil {
		t.Fatalf("mark the suggestion: %v", err)
	}
	if err := joinedAcceptSuggestion(service, artifactID, "s1", "red", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("accept the suggestion: %v", err)
	}
	tree := liveTree(t, service, artifactID)
	if _, quote, found := pmdoc.FindMark(tree, string(MarkComment), commentID); !found || quote != "quick red fox jumps" {
		t.Fatalf("comment mark after the accept = %q found=%t, want %q", quote, found, "quick red fox jumps")
	}
	if _, _, found := pmdoc.FindMark(tree, string(MarkSuggestion), "s1"); found {
		t.Fatal("the accepted suggestion's mark survived the accept")
	}
}

// Two suggestions over the same word are two marks of one type on it. Accepting one writes its text
// inside every mark that covers all of the word, the other suggestion's included, so that one stays
// anchored on the new text rather than losing its anchor to the first one's mark.
func TestAcceptingOneOfTwoSuggestionsOverAWordKeepsTheOther(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "The quick brown fox")
	for _, id := range []string{"earlier", "later"} {
		if _, err := joinedMarkQuote(service, artifactID, MarkSpec{
			Kind: MarkSuggestion, ID: id, By: model.Actor{Kind: "session", ID: "s1"},
		}, "fox", nil); err != nil {
			t.Fatalf("mark suggestion %s: %v", id, err)
		}
	}
	if err := joinedAcceptSuggestion(service, artifactID, "later", "dog", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("accept the later suggestion: %v", err)
	}
	tree := liveTree(t, service, artifactID)
	if _, quote, found := pmdoc.FindMark(tree, string(MarkSuggestion), "earlier"); !found || quote != "dog" {
		t.Fatalf("earlier suggestion after the accept = %q found=%t, want %q", quote, found, "dog")
	}
	if _, _, found := pmdoc.FindMark(tree, string(MarkSuggestion), "later"); found {
		t.Fatal("the accepted suggestion's mark is still in the document")
	}
}

type storedAnchor struct {
	ArtifactID string `json:"artifact_id"`
	MarkID     string `json:"mark_id"`
	Version    int    `json:"version"`
	Quote      string `json:"quote"`
	Orphaned   bool   `json:"orphaned"`
}

// markedAnchor marks quote in artifactID's document with a kind mark of id and returns the stored
// anchor of a row that records that mark.
func markedAnchor(t *testing.T, service *Service, artifactID string, kind MarkKind, id, quote string) []byte {
	t.Helper()
	if _, err := joinedMarkQuote(service, artifactID, MarkSpec{
		Kind: kind,
		ID:   id,
		By:   model.Actor{Kind: "user", ID: "alice"},
	}, quote, nil); err != nil {
		t.Fatalf("mark %q with %s mark %s: %v", quote, kind, id, err)
	}
	anchor, err := json.Marshal(storedAnchor{ArtifactID: artifactID, MarkID: id, Version: 1, Quote: quote})
	if err != nil {
		t.Fatalf("encode the anchor of %s mark %s: %v", kind, id, err)
	}
	return anchor
}

func insertAnchoredAsk(t *testing.T, service *Service, artifactID, quote string) string {
	t.Helper()
	const id = "00000000-0000-4000-8000-000000000001"
	anchor := markedAnchor(t, service, artifactID, MarkAsk, id, quote)
	if _, err := service.store.Pool.Exec(context.Background(), `
		insert into asks (id, issue_key, author, question, anchor)
		values ($1, 'DOC-1', '{"kind":"user","id":"alice"}', 'Anchored ask', $2)
	`, id, anchor); err != nil {
		t.Fatalf("insert anchored ask: %v", err)
	}
	tx, err := service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin opened ask event: %v", err)
	}
	if _, err := service.events.Append(context.Background(), tx, model.Event{
		IssueKey: new("DOC-1"), Type: "ask.opened", Actor: model.Actor{Kind: "user", ID: "alice"}, Payload: model.NewAskEventPayload(model.Ask{ID: id}, model.ReferenceChanges{}),
	}); err != nil {
		_ = tx.Rollback(context.Background())
		t.Fatalf("append opened ask event: %v", err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit opened ask event: %v", err)
	}
	return id
}

func insertAnchoredComment(t *testing.T, service *Service, artifactID, quote string) string {
	t.Helper()
	return insertAnchoredCommentWithID(t, service, artifactID, "00000000-0000-4000-8000-000000000002", quote)
}

func insertAnchoredCommentWithID(t *testing.T, service *Service, artifactID, id, quote string) string {
	t.Helper()
	anchor := markedAnchor(t, service, artifactID, MarkComment, id, quote)
	if _, err := service.store.Pool.Exec(context.Background(), `
		insert into comments (id, issue_key, author, body, anchor)
		values ($1, 'DOC-1', '{"kind":"user","id":"alice"}', 'Anchored comment', $2)
	`, id, anchor); err != nil {
		t.Fatalf("insert anchored comment: %v", err)
	}
	return id
}

func loadAskAnchor(t *testing.T, service *Service, id string) storedAnchor {
	t.Helper()
	return loadAnchor(t, service, "asks", id)
}

func loadCommentAnchor(t *testing.T, service *Service, id string) storedAnchor {
	t.Helper()
	return loadAnchor(t, service, "comments", id)
}

func loadAnchor(t *testing.T, service *Service, table, id string) storedAnchor {
	t.Helper()
	var encoded []byte
	if err := service.store.Pool.QueryRow(context.Background(), "select anchor from "+table+" where id = $1", id).Scan(&encoded); err != nil {
		t.Fatalf("load %s anchor: %v", table, err)
	}
	var anchor storedAnchor
	if err := json.Unmarshal(encoded, &anchor); err != nil {
		t.Fatalf("decode %s anchor: %v", table, err)
	}
	return anchor
}

// waitForPersistedUpdates waits for every update the live document has queued to reach
// doc_updates, then checks that at least want rows landed. The service counts its own queue
// (durableAppends, decremented after each append commits), so this waits on that signal rather
// than polling the row count against a stopwatch: the drain takes as long as the machine needs -
// it runs at roughly a hundred appends a second, so five hundred marks is seconds of work on an
// idle box and longer on a loaded one - while a drain that never finishes still fails, on the
// context deadline rather than on how busy the box was.
func waitForPersistedUpdates(t *testing.T, service *Service, artifactID string, want int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := service.waitForDurableAppends(ctx, artifactID); err != nil {
		t.Fatalf("waiting for queued document updates to persist: %v", err)
	}
	var count int
	if err := service.store.Pool.QueryRow(context.Background(),
		`select count(*) from doc_updates where artifact_id = $1`, artifactID).Scan(&count); err != nil {
		t.Fatalf("count persisted document updates: %v", err)
	}
	if count < want {
		t.Fatalf("persisted %d document updates, want at least %d", count, want)
	}
}

func waitFor(t *testing.T, timeout time.Duration, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

// askTree parses markdown and gives its ask blocks the ids named, in document order, repeats
// included: a browser write can leave two asks under one id until settlement's repair runs, and
// parsing alone never produces that document.
func askTree(t *testing.T, markdown string, ids ...string) *pmdoc.Node {
	t.Helper()
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatal(err)
	}
	next := 0
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type == "ask" {
			node.Attrs[pmdoc.BlockIDAttr] = ids[next]
			next++
		}
		return true
	})
	if next != len(ids) {
		t.Fatalf("%d ask blocks, want %d", next, len(ids))
	}
	return tree
}

// refuseBrokenAsks judges each ask by its id, so an id the document already held unreadable,
// repeated included, neither switches the check off for another ask nor refuses a write that
// leaves it alone.
func TestRefuseBrokenAsksJudgesEachAskByItsID(t *testing.T) {
	const (
		ask       = ":::ask{urgency=\"med\" multiple=\"false\"}\n%s\n:::\n\n"
		malformed = "Which one?\n\n```\ncode\n```"
	)
	askDoc := func(questions ...string) string {
		markdown := ""
		for _, question := range questions {
			markdown += fmt.Sprintf(ask, question)
		}
		return markdown
	}
	for _, test := range []struct {
		name          string
		before, after *pmdoc.Node
		want          string
	}{
		{name: "an ask broken beside a repeated id elsewhere",
			before: askTree(t, askDoc("Which one?", "Q2?", "Q3?"), "a1", "a2", "a2"),
			after:  askTree(t, askDoc(malformed, "Q2?", "Q3?"), "a1", "a2", "a2"),
			want:   `ask block "a1" has unsupported body node "code_block"`},
		{name: "a repeat removed beside an ask already malformed",
			before: askTree(t, askDoc(malformed, "Q2?", "Q3?"), "a1", "a2", "a2"),
			after:  askTree(t, askDoc(malformed, "Q2?"), "a1", "a2")},
		{name: "a repeat written under an id held malformed, ahead of it",
			before: askTree(t, askDoc(malformed), "a1"),
			after:  askTree(t, askDoc("Other?", malformed), "a1", "a1"),
			want:   `duplicate ask block id "a1"`},
		{name: "a repeat written under a readable ask's id",
			before: askTree(t, askDoc("Which one?"), "a1"),
			after:  askTree(t, askDoc("Other?", "Which one?"), "a1", "a1"),
			want:   `duplicate ask block id "a1"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := refuseBrokenAsks(test.before, test.after)
			if test.want == "" {
				if err != nil {
					t.Fatalf("refuseBrokenAsks = %v, want nil", err)
				}
				return
			}
			var invalid *ErrInvalidAskBlock
			if !errors.As(err, &invalid) || invalid.Reason.Error() != test.want {
				t.Fatalf("refuseBrokenAsks = %v, want the ask refused with %q", err, test.want)
			}
		})
	}
}

// repeatLiveBlockID gives the live document's second paragraph the first one's block id, as a
// browser write can before settlement's id repair runs.
func repeatLiveBlockID(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	err := service.srv.Apply(context.Background(), artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		fragment := doc.GetXmlFragment(fragmentName)
		tree, err := treeOf(doc)
		if err != nil {
			t.Errorf("read live tree: %v", err)
			return
		}
		tree.Children[1].Attrs[pmdoc.BlockIDAttr] = tree.Children[0].Attrs[pmdoc.BlockIDAttr]
		transact(func(txn *crdt.Transaction) {
			if err := pmdoc.Update(txn, fragment, tree); err != nil {
				t.Errorf("repeat a live block id: %v", err)
			}
		})
	})
	if err != nil {
		t.Fatalf("apply the repeated id: %v", err)
	}
}

// A document a browser edit left unreadable, here with a footnote definition moved into a callout
// in a list item, is not an accept's to refuse elsewhere: the accept stores and the callout stays as
// it was.
func TestAcceptSuggestionBesideABlockTheParserAlreadyRefuses(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "- Lead.\n\n  :::callout{#c1 kind=\"note\" title=\"T\"}\n  Gone.\n  :::\n\nIntro.\n\nBody.\n\nAfter.\n")
	editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
			if node.Type == "callout" {
				node.Children = []*pmdoc.Node{{Type: "footnote_definition", Attrs: pmdoc.Attrs{pmdoc.BlockIDAttr: "moved", "label": "n"}, Children: node.Children}}
				return false
			}
			return true
		})
		return tree
	})
	before := liveTree(t, service, artifactID)
	if _, err := pmdoc.ReadBack(before); err == nil {
		t.Fatal("the footnote definition in a callout reads back; the test needs a document the parser refuses")
	}
	spec := MarkSpec{Kind: MarkSuggestion, ID: "s1", By: model.Actor{Kind: "session", ID: "s1"}}
	if _, err := joinedMarkQuote(service, artifactID, spec, "Body.", nil); err != nil {
		t.Fatal(err)
	}
	if err := joinedAcceptSuggestion(service, artifactID, "s1", "Changed.", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("accept beside a block the parser already refuses: %v", err)
	}
	after := liveTree(t, service, artifactID)
	if len(after.Children) != 4 || pmdoc.StripAnchorMarks(after.Children[2]).Children[0].Text != "Changed." {
		t.Fatalf("after the accept the document holds %#v, want Changed. in place of Body.", after.Children)
	}
	if !after.Children[0].Equal(before.Children[0]) {
		t.Fatal("the accept changed the list holding the emptied callout")
	}
}

// A footnote definition whose reference a browser edit removed reads back as itself, as the
// browser editor keeps a definition nothing refers to, and an accept inside it stores.
func TestAcceptSuggestionInAFootnoteDefinitionWhoseReferenceIsGone(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "Intro[^1].\n\n[^1]: Body.\n")
	editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		intro := tree.Children[0]
		kept := intro.Children[:0]
		for _, child := range intro.Children {
			if child.Type != "footnote_reference" {
				kept = append(kept, child)
			}
		}
		intro.Children = kept
		return tree
	})
	if back, err := pmdoc.ReadBack(liveTree(t, service, artifactID)); err != nil || len(back.Children) != 2 || back.Children[1].Type != "footnote_definition" {
		t.Fatalf("the unreferenced definition reads back as %#v (%v), want the paragraph and the definition", back, err)
	}
	spec := MarkSpec{Kind: MarkSuggestion, ID: "s1", By: model.Actor{Kind: "session", ID: "s1"}}
	if _, err := joinedMarkQuote(service, artifactID, spec, "Body.", nil); err != nil {
		t.Fatal(err)
	}
	if err := joinedAcceptSuggestion(service, artifactID, "s1", "Changed.", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("accept in a footnote definition whose reference is gone: %v", err)
	}
	definition := liveTree(t, service, artifactID).Children[1]
	if definition.Type != "footnote_definition" || pmdoc.StripAnchorMarks(definition.Children[0]).Children[0].Text != "Changed." {
		t.Fatalf("after the accept the definition is %#v, want it holding Changed.", definition)
	}
}

// An accept changes only the text it writes. Code beside it keeps the line break that ends it and
// the comment anchored on that break, and code whose ending breaks a mark splits takes an accept
// over its text, with or without a break of its own.
func TestAcceptSuggestionLeavesCodeItDoesNotWrite(t *testing.T) {
	codeWithMarkedBreak := func(text string) func(*pmdoc.Node) *pmdoc.Node {
		return func(tree *pmdoc.Node) *pmdoc.Node {
			pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
				if node.Type == "code_block" {
					node.Children = []*pmdoc.Node{{Type: "text", Text: text}, {Type: "text", Text: "\n", Marks: []pmdoc.Mark{{Type: "proofComment", Attrs: pmdoc.Attrs{"id": "c1"}}}}}
					return false
				}
				return true
			})
			return tree
		}
	}
	for _, test := range []struct{ name, spec, code, quote, with string }{
		{"text beside code in the same list", "- Body.\n- ```\n  c\n  ```\n", "c", "Body.", "Changed."},
		{"text over code whose ending breaks a mark splits", "Intro.\n\n```\nabc\n```\n", "abc\n", "abc", "xyz"},
		{"text ending in a break over code a marked break ends", "Intro.\n\n```\nabc\n```\n", "abc", "abc", "xyz\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			seedServiceText(t, service, artifactID, test.spec)
			editLiveTree(t, service, artifactID, codeWithMarkedBreak(test.code))
			spec := MarkSpec{Kind: MarkSuggestion, ID: "s1", By: model.Actor{Kind: "session", ID: "s1"}}
			if _, err := joinedMarkQuote(service, artifactID, spec, test.quote, nil); err != nil {
				t.Fatal(err)
			}
			if err := joinedAcceptSuggestion(service, artifactID, "s1", test.with, model.Actor{Kind: "user", ID: "alice"}); err != nil {
				t.Fatalf("accept: %v", err)
			}
			if _, _, ok := pmdoc.FindMark(liveTree(t, service, artifactID), "proofComment", "c1"); !ok {
				t.Fatal("the comment anchored on the code's ending line break is gone after the accept")
			}
		})
	}
}

// An accept leaves a list item beside the text it writes as it was, spread its markdown does not
// carry included: the accept settles only what it wrote.
func TestAcceptSuggestionLeavesASiblingItemsSpread(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "- Body.\n- two\n")
	sibling := func(tree *pmdoc.Node) *pmdoc.Node {
		var found *pmdoc.Node
		pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
			if node.Type == "list_item" {
				found = node
			}
			return true
		})
		return found
	}
	editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		sibling(tree).Attrs["spread"] = true
		return tree
	})
	spec := MarkSpec{Kind: MarkSuggestion, ID: "s1", By: model.Actor{Kind: "session", ID: "s1"}}
	if _, err := joinedMarkQuote(service, artifactID, spec, "Body.", nil); err != nil {
		t.Fatal(err)
	}
	if err := joinedAcceptSuggestion(service, artifactID, "s1", "Changed.", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if spread := sibling(liveTree(t, service, artifactID)).Attrs["spread"]; spread != true {
		t.Fatalf("the item beside the accepted text holds spread %v after the accept, want the true it held", spread)
	}
}

// An accepted suggestion's text is read as a paragraph's, where `\|` in code is the code's own text;
// in a table cell no markdown writes such code (pmdoc.TableCodePipes), so the accept is refused
// naming the cell and what the person accepting can do, nothing is written, and the suggestion
// stays open. Code holding a `|` with no backslash before it is accepted. Beside such code a browser
// edit left, an accept that adds none is taken, and one that adds the same code again is refused.
func TestAcceptSuggestionRefusesCodeWithAPipeAfterABackslashInATableCell(t *testing.T) {
	for _, test := range []struct {
		name, with string
		// browser puts code holding `a\|b` in the body row's first cell first, as a browser edit can.
		browser bool
		// says is what the refusal says, or "" when the accept is taken; stored is then the body
		// row the document is stored with.
		says, stored string
	}{
		{name: "code holding a backslash before a pipe", with: "`git log \\| wc -l`",
			says: `holds code in row 1, column 1 (under the header "Note") whose text has a | right after a backslash, "git log \| wc -l"`},
		{name: "code holding a pipe", with: "`git log | wc -l`", stored: "| run | `git log \\| wc -l` |"},
		{name: "text beside code a browser edit left", with: "y", browser: true, stored: "| `a\\`\\|`b` | y |"},
		{name: "a code pipe beside code a browser edit left", with: "`a\\|b`", browser: true,
			says: `holds code in row 1, column 1 (under the header "Note") whose text has a | right after a backslash, "a\|b"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			seedServiceText(t, service, artifactID, "| Cmd | Note |\n| --- | --- |\n| run | Body. |\n")
			if test.browser {
				editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
					tree.Children[0].Children[1].Children[0].Children[0].Children = []*pmdoc.Node{{Type: "text", Text: "a\\|b", Marks: []pmdoc.Mark{{Type: "inlineCode"}}}}
					return tree
				})
			}
			spec := MarkSpec{Kind: MarkSuggestion, ID: "s1", By: model.Actor{Kind: "session", ID: "s1"}}
			if _, err := joinedMarkQuote(service, artifactID, spec, "Body.", nil); err != nil {
				t.Fatal(err)
			}
			before := liveTree(t, service, artifactID)
			err := joinedAcceptSuggestion(service, artifactID, "s1", test.with, model.Actor{Kind: "user", ID: "alice"})
			if test.says == "" {
				if err != nil {
					t.Fatalf("accept: %v", err)
				}
				text, err := service.Text(context.Background(), artifactID)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(text, test.stored+"\n") {
					t.Fatalf("after the accept the document is %q, want the row %q", text, test.stored)
				}
				return
			}
			var invalid *ErrInvalidOp
			if !errors.As(err, &invalid) || invalid.Field != "replace_with" {
				t.Fatalf("accept = %v, want an invalid op on replace_with", err)
			}
			for _, says := range []string{fmt.Sprintf("table %q %s", blockID(before.Children[0]), test.says), "reject the suggestion, or reply asking for text the cell can hold"} {
				if !strings.Contains(invalid.Reason, says) {
					t.Errorf("accept refused with %q, want it to say %q", invalid.Reason, says)
				}
			}
			if after := liveTree(t, service, artifactID); !after.Equal(before) {
				t.Fatal("the refused accept changed the document")
			}
		})
	}
}

// A repeat the live document already holds is settlement's to repair, not a write's to refuse or
// to repair: beside one, an accept whose replacement names no held id (inline, or a typed block
// with an id of its own), the reject of a browser insert, and an upload of the document's own text
// are taken as they were before the repeated-id check, and the accept and the reject leave the
// repeat as they found it. The upload stores what the base did: the id repair every upload runs
// keeps the id for the first block in document order.
func TestWritesBesideALiveRepeatedBlockIDAreTaken(t *testing.T) {
	const seed = ":::callout{#note}\nOriginal.\n:::\n\n:::callout{kind=\"warning\"}\nCopy.\n:::\n\nThe quick brown fox\n"
	alice := model.Actor{Kind: "user", ID: "alice"}
	accept := func(quote, replacement string) func(*testing.T, *Service, string) error {
		return func(t *testing.T, service *Service, artifactID string) error {
			if _, err := joinedMarkQuote(service, artifactID, MarkSpec{
				Kind: MarkSuggestion, ID: "rep", By: model.Actor{Kind: "session", ID: "s1"},
			}, quote, nil); err != nil {
				return err
			}
			return joinedAcceptSuggestion(service, artifactID, "rep", replacement, alice)
		}
	}
	for _, test := range []struct {
		name string
		act  func(*testing.T, *Service, string) error
		want func(before string) string
	}{
		{name: "an inline accept", act: accept("brown", "red"),
			want: func(before string) string { return strings.Replace(before, "quick brown", "quick red", 1) }},
		{name: "an accept writing a typed block with an id of its own",
			act: accept("The quick brown fox", ":::callout{#fresh}\nA note.\n:::\n"),
			want: func(before string) string {
				return strings.Replace(before, "The quick brown fox\n", ":::callout{#fresh kind=\"note\" title=\"\"}\nA note.\n:::\n", 1)
			}},
		{name: "the reject of a browser insert", act: func(t *testing.T, service *Service, artifactID string) error {
			browserMarkWithAttrs(t, service, artifactID, "proofSuggestion", "quick ", pmdoc.Attrs{
				"id": "ins", "by": "user:bob", "kind": "insert",
			})
			return joinedRejectSuggestion(service, artifactID, "ins", alice)
		}, want: func(before string) string { return strings.Replace(before, "quick brown", "brown", 1) }},
		{name: "an upload of the document's own text", act: func(t *testing.T, service *Service, artifactID string) error {
			current, err := service.Text(context.Background(), artifactID)
			if err != nil {
				return err
			}
			installCounterIDs(t, "minted")
			_, err = joinedReplaceText(service, artifactID, current+"\nAn added line.\n", alice)
			return err
		}, want: func(before string) string {
			// The original's paragraph, unchanged, keeps its id, so the copy takes the first id minted.
			copied := strings.LastIndex(before, "{#note ")
			return before[:copied] + "{#minted-1 " + before[copied+len("{#note "):] + "\nAn added line.\n"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			seedServiceText(t, service, artifactID, seed)
			repeatLiveBlockID(t, service, artifactID)
			before, err := service.Text(context.Background(), artifactID)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(before, "{#note ") != 2 {
				t.Fatalf("the live document does not repeat the id: %q", before)
			}
			if err := test.act(t, service, artifactID); err != nil {
				t.Fatalf("%s beside a live repeated id: %v", test.name, err)
			}
			waitForDocumentText(t, service, artifactID, test.want(before))
		})
	}
}

// installCounterIDs mints prefix-1, prefix-2, ... for the rest of the test.
func installCounterIDs(t *testing.T, prefix string) {
	t.Helper()
	n := 0
	pmdoc.SetBlockIDGenerator(func() string {
		n++
		return fmt.Sprintf("%s-%d", prefix, n)
	})
	t.Cleanup(func() { pmdoc.SetBlockIDGenerator(nil) })
}

// insertAnchorRow records an open row of kind "ask", "comment" or "suggestion" with the given id,
// anchored to markID with quote. It writes no mark, so each test decides what the tree holds, and
// it gives an ask the opened event an ask's refresh event reads.
func insertAnchorRow(t *testing.T, service *Service, artifactID, kind, id, markID, quote string) {
	t.Helper()
	ctx := context.Background()
	anchor, err := json.Marshal(model.Anchor{ArtifactID: artifactID, MarkID: markID, Version: 1, Quote: quote})
	if err != nil {
		t.Fatalf("encode %s anchor: %v", kind, err)
	}
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin %s row: %v", kind, err)
	}
	defer tx.Rollback(ctx)
	switch kind {
	case "ask":
		if _, err := tx.Exec(ctx, `
			insert into asks (id, issue_key, author, question, anchor)
			values ($1, 'DOC-1', '{"kind":"user","id":"alice"}', 'Anchored ask', $2)
		`, id, anchor); err != nil {
			t.Fatalf("insert anchored ask: %v", err)
		}
		if _, err := service.events.Append(ctx, tx, model.Event{
			IssueKey: new("DOC-1"), Type: "ask.opened", Actor: model.Actor{Kind: "user", ID: "alice"},
			Payload: model.NewAskEventPayload(model.Ask{ID: id}, model.ReferenceChanges{}),
		}); err != nil {
			t.Fatalf("append opened ask event: %v", err)
		}
	case "comment", "suggestion":
		var suggestion *string
		if kind == "suggestion" {
			suggestion = new(`{"replace_with":"red","accepted":null}`)
		}
		if _, err := tx.Exec(ctx, `
			insert into comments (id, issue_key, author, body, anchor, suggestion)
			values ($1, 'DOC-1', '{"kind":"user","id":"alice"}', 'Anchored comment', $2, $3::jsonb)
		`, id, anchor, suggestion); err != nil {
			t.Fatalf("insert anchored %s: %v", kind, err)
		}
	default:
		t.Fatalf("unknown anchor row kind %q", kind)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit %s row: %v", kind, err)
	}
}

// refreshLiveAnchors runs one refresh of the live tree inside a joined transaction that it commits.
// It returns the open rows as the refresh's own query lists them, read in that transaction just
// before it, and the events the refresh collected, in the order it appended them.
func refreshLiveAnchors(t *testing.T, service *Service, artifactID string) ([]anchoredMark, []model.Event) {
	t.Helper()
	tree := liveTree(t, service, artifactID)
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin refresh: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	rows, err := service.openAnchoredMarks(joined, tx, artifactID)
	if err != nil {
		t.Fatalf("list open anchors: %v", err)
	}
	if err := service.refreshAnchors(joined, tx, artifactID, tree, model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("refresh anchors: %v", err)
	}
	refreshed := slices.Clone(ledger.events)
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit refresh: %v", err)
	}
	return rows, refreshed
}

// refreshedAnchor is the row id and the anchor an anchor refresh event announces.
func refreshedAnchor(t *testing.T, event model.Event) (string, model.Anchor) {
	t.Helper()
	switch payload := event.Payload.(type) {
	case model.AskEventPayload:
		if event.Type == "ask.anchor_refreshed" && payload.Ask.Anchor != nil {
			return payload.Ask.ID, *payload.Ask.Anchor
		}
	case model.CommentEventPayload:
		if event.Type == "comment.anchor_refreshed" && payload.Comment.Anchor != nil {
			return payload.Comment.ID, *payload.Comment.Anchor
		}
	}
	t.Fatalf("event %s with payload %T is not an anchor refresh carrying an anchor", event.Type, event.Payload)
	return "", model.Anchor{}
}

// Two rows naming one mark are two rows to refresh: each gets its own event and its own stored
// quote, so a refresh that walks the found marks, or one row per mark, drops one of them.
func TestRefreshAnchorsRefreshesEveryRowSharingAMark(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "The quick brown fox")
	if _, err := joinedMarkQuote(service, artifactID, MarkSpec{
		Kind: MarkComment, ID: "shared", By: model.Actor{Kind: "user", ID: "alice"},
	}, "brown", nil); err != nil {
		t.Fatalf("mark the shared comment: %v", err)
	}
	rows := []string{"00000000-0000-4000-8000-0000000000a1", "00000000-0000-4000-8000-0000000000a2"}
	for _, id := range rows {
		insertAnchorRow(t, service, artifactID, "comment", id, "shared", "stale")
	}

	_, refreshed := refreshLiveAnchors(t, service, artifactID)
	announced := map[string]model.Anchor{}
	for _, event := range refreshed {
		id, anchor := refreshedAnchor(t, event)
		announced[id] = anchor
	}
	if len(refreshed) != len(rows) || len(announced) != len(rows) {
		t.Fatalf("refresh announced %d events for rows %v, want one for each of %v", len(refreshed), slices.Collect(maps.Keys(announced)), rows)
	}
	for _, id := range rows {
		if anchor := announced[id]; anchor.Quote != "brown" || anchor.Orphaned {
			t.Errorf("row %s announced quote %q orphaned %t, want brown", id, anchor.Quote, anchor.Orphaned)
		}
		if anchor := loadCommentAnchor(t, service, id); anchor.Quote != "brown" || anchor.Orphaned {
			t.Errorf("row %s stored quote %q orphaned %t, want brown", id, anchor.Quote, anchor.Orphaned)
		}
	}
}

// An accept leaves every row anchored to the accepted suggestion's mark id as it was, a suggestion
// row and an ask row alike, and still refreshes the rest: AcceptSuggestion skips by mark id, not by
// mark type and id.
func TestAcceptSuggestionSkipsItsMarkIDForRowsOfEveryType(t *testing.T) {
	service, artifactID := newTestService(t)
	// No settlement may run: it refreshes without the accept's skip.
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "The quick brown fox jumps")
	by := model.Actor{Kind: "session", ID: "s1"}
	// The ask's mark shares the suggestion's id, so the ask row would read "quick" if refreshed.
	for _, mark := range []struct {
		spec  MarkSpec
		quote string
	}{
		{MarkSpec{Kind: MarkSuggestion, ID: "s1", By: by}, "brown"},
		{MarkSpec{Kind: MarkAsk, ID: "s1", By: by}, "quick"},
		{MarkSpec{Kind: MarkComment, ID: "control", By: by}, "fox"},
	} {
		if _, err := joinedMarkQuote(service, artifactID, mark.spec, mark.quote, nil); err != nil {
			t.Fatalf("mark %s %s: %v", mark.spec.Kind, mark.spec.ID, err)
		}
	}
	const (
		suggestionID = "00000000-0000-4000-8000-0000000000b1"
		askID        = "00000000-0000-4000-8000-0000000000b2"
		controlID    = "00000000-0000-4000-8000-0000000000b3"
	)
	insertAnchorRow(t, service, artifactID, "suggestion", suggestionID, "s1", "stale suggestion")
	insertAnchorRow(t, service, artifactID, "ask", askID, "s1", "stale ask")
	insertAnchorRow(t, service, artifactID, "comment", controlID, "control", "stale control")

	if err := joinedAcceptSuggestion(service, artifactID, "s1", "red", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("accept the suggestion: %v", err)
	}
	for _, row := range []struct {
		name   string
		stored storedAnchor
		want   string
	}{
		{"suggestion row on the accepted mark id", loadCommentAnchor(t, service, suggestionID), "stale suggestion"},
		{"ask row on the accepted mark id", loadAskAnchor(t, service, askID), "stale ask"},
		{"control comment on another mark", loadCommentAnchor(t, service, controlID), "fox"},
	} {
		if row.stored.Quote != row.want || row.stored.Orphaned {
			t.Errorf("%s after the accept = quote %q orphaned %t, want quote %q not orphaned", row.name, row.stored.Quote, row.stored.Orphaned, row.want)
		}
	}
}

// One row follows its mark through removal, restoration over new text and removal again: each
// change persists the quote and orphaned flag and announces itself once, an orphan keeps the quote
// it last had, and a refresh that changes nothing writes and announces nothing.
func TestRefreshAnchorsFollowsAMarkRemovedRestoredAndRemovedAgain(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "The quick brown fox")
	if _, err := joinedMarkQuote(service, artifactID, MarkSpec{
		Kind: MarkComment, ID: "c1", By: model.Actor{Kind: "user", ID: "alice"},
	}, "brown", nil); err != nil {
		t.Fatalf("mark the comment: %v", err)
	}
	const rowID = "00000000-0000-4000-8000-0000000000c1"
	insertAnchorRow(t, service, artifactID, "comment", rowID, "c1", "brown")
	restore := func(tree *pmdoc.Node) *pmdoc.Node {
		paragraph := tree.Children[0]
		paragraph.Children = append(paragraph.Children, &pmdoc.Node{Type: "text", Text: "lazy", Marks: []pmdoc.Mark{{
			Type: string(MarkComment), Attrs: pmdoc.Attrs{"id": "c1", "by": "user:alice"},
		}}})
		return tree
	}
	for _, step := range []struct {
		name     string
		edit     func(*pmdoc.Node) *pmdoc.Node
		quote    string
		orphaned bool
		events   int
	}{
		{name: "the marked text deleted", edit: deleteRun("brown"), quote: "brown", orphaned: true, events: 1},
		{name: "nothing changed while orphaned", quote: "brown", orphaned: true},
		{name: "the mark written over new text", edit: restore, quote: "lazy", events: 1},
		{name: "nothing changed while found", quote: "lazy"},
		{name: "the marked text deleted again", edit: deleteRun("lazy"), quote: "lazy", orphaned: true, events: 1},
	} {
		if step.edit != nil {
			editLiveTree(t, service, artifactID, step.edit)
		}
		_, refreshed := refreshLiveAnchors(t, service, artifactID)
		if len(refreshed) != step.events {
			t.Fatalf("%s: refresh announced %d events, want %d", step.name, len(refreshed), step.events)
		}
		for _, event := range refreshed {
			if id, anchor := refreshedAnchor(t, event); id != rowID || anchor.Quote != step.quote || anchor.Orphaned != step.orphaned {
				t.Fatalf("%s: refresh announced row %s quote %q orphaned %t, want row %s quote %q orphaned %t",
					step.name, id, anchor.Quote, anchor.Orphaned, rowID, step.quote, step.orphaned)
			}
		}
		if stored := loadCommentAnchor(t, service, rowID); stored.Quote != step.quote || stored.Orphaned != step.orphaned {
			t.Fatalf("%s: stored quote %q orphaned %t, want quote %q orphaned %t",
				step.name, stored.Quote, stored.Orphaned, step.quote, step.orphaned)
		}
	}
}

// One refresh of asks, comments and a suggestion announces the asks, then the comments, each by
// creation time and then id, with each row's own event type and quote. The creation times are set
// so that order is neither the ids' nor the inserts'. The rows go stale and are refreshed again
// for several rounds, and every round announces them in that same order: a listing with no stable
// order, or a refresh in a map's order, matches five rows by chance in about one round of five.
func TestRefreshAnchorsAnnouncesEveryKindInRowOrder(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "alpha bravo charlie delta echo")
	rows := []struct{ kind, id, quote string }{
		{"suggestion", "00000000-0000-4000-8000-0000000000d1", "echo"},
		{"ask", "00000000-0000-4000-8000-0000000000d2", "alpha"},
		{"comment", "00000000-0000-4000-8000-0000000000d3", "charlie"},
		{"ask", "00000000-0000-4000-8000-0000000000d4", "bravo"},
		{"comment", "00000000-0000-4000-8000-0000000000d5", "delta"},
	}
	quotes := map[string]string{}
	for _, row := range rows {
		kind := map[string]MarkKind{"ask": MarkAsk, "comment": MarkComment, "suggestion": MarkSuggestion}[row.kind]
		if _, err := joinedMarkQuote(service, artifactID, MarkSpec{
			Kind: kind, ID: row.id, By: model.Actor{Kind: "user", ID: "alice"},
		}, row.quote, nil); err != nil {
			t.Fatalf("mark %s %s: %v", row.kind, row.id, err)
		}
		quotes[row.id] = row.quote
	}
	for _, row := range rows {
		insertAnchorRow(t, service, artifactID, row.kind, row.id, row.id, "stale")
	}
	// Each update writes a new row version at the end of its table, so updating in the reverse of
	// the wanted order leaves the tables' physical order the opposite of it: a listing that leans
	// on that order announces the rows backwards.
	created := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		id     string
		offset time.Duration
	}{
		{"00000000-0000-4000-8000-0000000000d3", time.Second},
		{"00000000-0000-4000-8000-0000000000d1", time.Second},
		{"00000000-0000-4000-8000-0000000000d5", 0},
		{"00000000-0000-4000-8000-0000000000d2", time.Second},
		{"00000000-0000-4000-8000-0000000000d4", 0},
	} {
		for _, table := range []string{"asks", "comments"} {
			if _, err := service.store.Pool.Exec(context.Background(),
				"update "+table+" set created_at = $2 where id = $1::uuid", row.id, created.Add(row.offset)); err != nil {
				t.Fatalf("set %s %s created_at: %v", table, row.id, err)
			}
		}
	}
	// Asks by creation time, then comments by creation time and, for d1 and d3 created together, id.
	want := []string{
		"00000000-0000-4000-8000-0000000000d4", "00000000-0000-4000-8000-0000000000d2",
		"00000000-0000-4000-8000-0000000000d5", "00000000-0000-4000-8000-0000000000d1",
		"00000000-0000-4000-8000-0000000000d3",
	}

	ids := slices.Collect(maps.Keys(quotes))
	for round := range 4 {
		if round > 0 {
			for _, table := range []string{"asks", "comments"} {
				if _, err := service.store.Pool.Exec(context.Background(),
					"update "+table+` set anchor = jsonb_set(anchor, '{quote}', '"stale"') where id = any($1::uuid[])`, ids); err != nil {
					t.Fatalf("round %d: make the %s stale: %v", round, table, err)
				}
			}
		}
		listed, refreshed := refreshLiveAnchors(t, service, artifactID)
		if len(listed) != len(want) || len(refreshed) != len(want) {
			t.Fatalf("round %d: refresh listed %d rows and announced %d events, want %d of each", round, len(listed), len(refreshed), len(want))
		}
		for index, wantID := range want {
			wantType := "comment.anchor_refreshed"
			if strings.HasSuffix(wantID, "d2") || strings.HasSuffix(wantID, "d4") {
				wantType = "ask.anchor_refreshed"
			}
			id, anchor := refreshedAnchor(t, refreshed[index])
			if listed[index].id != wantID || id != wantID || refreshed[index].Type != wantType || anchor.Quote != quotes[wantID] || anchor.Orphaned {
				t.Fatalf("round %d: row %d listed %s, event %s for row %s quote %q orphaned %t, want %s for row %s quote %q",
					round, index, listed[index].id, refreshed[index].Type, id, anchor.Quote, anchor.Orphaned, wantType, wantID, quotes[wantID])
			}
		}
	}
}

// MarkQuote returns the anchor of the mark it wrote, as the committed document reads it: a quote
// crossing two top-level paragraphs reads joined by a space under no block, a quote's leading space
// stays under its paragraph, and a quote in a list item names the item's paragraph.
func TestMarkQuoteReturnsTheWrittenMarksAnchor(t *testing.T) {
	idOf := func(node *pmdoc.Node) string {
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		if id == "" {
			t.Fatalf("%s carries no block id", node.Type)
		}
		return id
	}
	for _, test := range []struct {
		name, text, quote, want string
		block                   func(tree *pmdoc.Node) string
	}{
		{
			name: "a quote across two paragraphs", text: "First paragraph ends here.\n\nSecond paragraph starts here.\n",
			quote: "ends here. Second paragraph", want: "ends here. Second paragraph",
			block: func(*pmdoc.Node) string { return "" },
		},
		{
			name: "a quote with a leading space", text: "The quick brown fox\n",
			quote: " brown", want: " brown",
			block: func(tree *pmdoc.Node) string { return idOf(tree.Children[0]) },
		},
		{
			name: "a quote in a list item", text: "Intro.\n\n- first item\n- second item text\n",
			quote: "second item", want: "second item",
			block: func(tree *pmdoc.Node) string { return idOf(tree.Children[1].Children[1].Children[0]) },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			seedServiceText(t, service, artifactID, test.text)
			spec := MarkSpec{Kind: MarkComment, ID: "c1", By: model.Actor{Kind: "user", ID: "alice"}}
			anchored, err := joinedMarkQuote(service, artifactID, spec, test.quote, nil)
			if err != nil {
				t.Fatalf("MarkQuote(%q): %v", test.quote, err)
			}
			tree := liveTree(t, service, artifactID)
			committed, err := readAnchored(tree, spec.Kind, spec.ID)
			if err != nil {
				t.Fatalf("read the committed mark: %v", err)
			}
			if anchored != committed {
				t.Fatalf("MarkQuote returned %#v, the committed document reads %#v", anchored, committed)
			}
			if want := (Anchored{Quote: test.want, BlockID: test.block(tree)}); anchored != want {
				t.Fatalf("MarkQuote returned %#v, want %#v", anchored, want)
			}
		})
	}
}
