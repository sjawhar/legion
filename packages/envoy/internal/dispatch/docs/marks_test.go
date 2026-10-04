package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// An unrecorded comment mark - one no comment, ask or suggestion row records - stays until the
// first settlement that saw it is UnrecordedMarkTTL old, and the first settlement after that
// sweeps it; a recorded mark, resolved or not, and a proof-authored mark stay. The sweep ages
// marks by the service's clock, which the test holds and moves, so a mark's age is what the test
// sets and never how long a loaded runner took. The sweep re-arms settlement for the mark's
// remaining TTL in real time; any such settlement reads the held clock and cannot change the
// result this test asserts.
func TestSettleSweepsUnrecordedMarksAfterTTL(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "")
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
	seedServiceText(t, service, artifactID, "The quick brown fox")
	browserMark(t, service, artifactID, "proofComment", "dangling", "quick")
	browserMarkWithAttrs(t, service, artifactID, "proofAuthored", "fox", pmdoc.Attrs{"id": "auth", "by": "user:bob"})
	resolvedCommentID := insertAnchoredComment(t, service, artifactID, "brown")
	if _, err := database.Pool.Exec(context.Background(), `update comments set resolved = true where id = $1`, resolvedCommentID); err != nil {
		t.Fatalf("resolve recorded comment: %v", err)
	}
	dangling := func() bool {
		_, _, found := pmdoc.FindMark(liveTree(t, service, artifactID), "proofComment", "dangling")
		return found
	}

	settleCurrentGeneration(t, service, artifactID)
	aged.Store(int64(ttl / 2))
	settleCurrentGeneration(t, service, artifactID)
	if !dangling() {
		t.Fatal("unrecorded mark swept before its TTL")
	}
	aged.Store(int64(ttl))
	settleCurrentGeneration(t, service, artifactID)
	if dangling() {
		t.Fatal("unrecorded mark not swept once its TTL passed")
	}
	if _, _, found := pmdoc.FindMark(liveTree(t, service, artifactID), "proofAuthored", "auth"); !found {
		t.Fatal("proof-authored mark was swept")
	}
	if _, _, found := pmdoc.FindMark(liveTree(t, service, artifactID), "proofComment", resolvedCommentID); !found {
		t.Fatal("recorded resolved comment mark was swept")
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
	for index := 0; index <= 500; index++ {
		browserMarkWithAttrs(t, service, artifactID, "proofAuthored", "one|two", pmdoc.Attrs{
			"id": fmt.Sprintf("covered-mark-%d", index), "by": "user:alice",
		})
	}
	waitForPersistedUpdates(t, service, artifactID, 502)
	persist := service.persistence.(*PgVersioned)
	if _, err := persist.Compact(context.Background(), artifactID, 500); err != nil {
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

func TestCompactionRetainsUncoveredContentBeyondVersionCursor(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	alignLatestVersionWithUpdates(t, service, artifactID)
	for index := range 249 {
		browserMarkWithAttrs(t, service, artifactID, "proofAuthored", "before", pmdoc.Attrs{
			"id": fmt.Sprintf("before-mark-%d", index), "by": "user:alice",
		})
	}
	waitForPersistedUpdates(t, service, artifactID, 250)
	if _, err := joinedReplaceText(service, artifactID, "after", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("write uncovered content update: %v", err)
	}
	waitForPersistedProofText(t, service.store, artifactID, "after\n")
	for index := range 251 {
		browserMarkWithAttrs(t, service, artifactID, "proofAuthored", "after", pmdoc.Attrs{
			"id": fmt.Sprintf("after-mark-%d", index), "by": "user:alice",
		})
	}
	waitForPersistedUpdates(t, service, artifactID, 502)
	persist := service.persistence.(*PgVersioned)
	if _, err := persist.Compact(context.Background(), artifactID, 500); err != nil {
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

func insertAnchoredAsk(t *testing.T, service *Service, artifactID, quote string) string {
	t.Helper()
	const id = "00000000-0000-4000-8000-000000000001"
	if _, err := joinedMarkQuote(service, artifactID, MarkSpec{
		Kind: MarkAsk,
		ID:   id,
		By:   model.Actor{Kind: "user", ID: "alice"},
	}, quote, nil); err != nil {
		t.Fatalf("mark ask: %v", err)
	}
	anchor, err := json.Marshal(storedAnchor{ArtifactID: artifactID, MarkID: id, Version: 1, Quote: quote})
	if err != nil {
		t.Fatalf("encode ask anchor: %v", err)
	}
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
	if _, err := joinedMarkQuote(service, artifactID, MarkSpec{
		Kind: MarkComment,
		ID:   id,
		By:   model.Actor{Kind: "user", ID: "alice"},
	}, quote, nil); err != nil {
		t.Fatalf("mark comment: %v", err)
	}
	anchor, err := json.Marshal(storedAnchor{ArtifactID: artifactID, MarkID: id, Version: 1, Quote: quote})
	if err != nil {
		t.Fatalf("encode comment anchor: %v", err)
	}
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
			// Minted in preorder: the original's paragraph takes minted-1, the copy minted-2.
			copied := strings.LastIndex(before, "{#note ")
			return before[:copied] + "{#minted-2 " + before[copied+len("{#note "):] + "\nAn added line.\n"
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
