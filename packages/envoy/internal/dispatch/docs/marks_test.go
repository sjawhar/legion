package docs

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

func TestMarkQuoteWritesMarkAndReturnsCoveredText(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "The quick brown fox")
	spec := MarkSpec{Kind: MarkAsk, ID: "ask-1", By: model.Actor{Kind: "session", ID: "s1"}}

	quote, err := service.MarkQuote(context.Background(), artifactID, spec, "brown", nil)
	if err != nil || quote != "brown" {
		t.Fatalf("MarkQuote = %q, %v", quote, err)
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
	if _, err := service.MarkQuote(context.Background(), artifactID, spec, "o", nil); !errors.As(err, &ambiguous) {
		t.Fatalf("ambiguous quote err = %v", err)
	}
	if _, err := service.MarkQuote(context.Background(), artifactID, spec, "purple", nil); !errors.Is(err, pmdoc.ErrTargetNotFound) {
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
	if _, err := service.MarkQuote(WithTx(context.Background(), tx), artifactID, MarkSpec{
		Kind: MarkComment,
		ID:   "c1",
		By:   model.Actor{Kind: "user", ID: "alice"},
	}, "quick", nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Evict(context.Background(), artifactID); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := pmdoc.FindMark(liveTree(t, service, artifactID), "proofComment", "c1"); ok {
		t.Fatal("rolled-back mark survived reload")
	}
}

func TestVerifyMarkWaitsForTheBrowserUpdate(t *testing.T) {
	database := openTestStore(t)
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
	quote, err := service.VerifyMark(context.Background(), artifactID, MarkComment, "b1")
	if err != nil || quote != "brown" {
		t.Fatalf("VerifyMark = %q, %v", quote, err)
	}

	started := time.Now()
	if _, err := service.VerifyMark(context.Background(), artifactID, MarkComment, "never"); !errors.Is(err, ErrAnchorMissing) {
		t.Fatalf("missing mark err = %v", err)
	}
	if waited := time.Since(started); waited < 250*time.Millisecond || waited > time.Second {
		t.Fatalf("waited %v, want approximately MarkWait", waited)
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
	if _, err := service.MarkQuote(context.Background(), artifactID, spec, "brown", nil); err != nil {
		t.Fatal(err)
	}
	if err := service.AcceptSuggestion(context.Background(), artifactID, "s1", "*red*", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatal(err)
	}
	waitForDocumentText(t, service, artifactID, "The quick *red* fox\n")
	if _, _, ok := pmdoc.FindMark(liveTree(t, service, artifactID), "proofSuggestion", "s1"); ok {
		t.Fatal("mark survived accept")
	}
	if err := service.AcceptSuggestion(context.Background(), artifactID, "s1", "x", model.Actor{Kind: "user", ID: "alice"}); !errors.Is(err, ErrAnchorOrphaned) {
		t.Fatalf("second accept err = %v", err)
	}
}

func TestRejectSuggestionIsKindAware(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "keep this and drop that")
	if _, err := service.MarkQuote(context.Background(), artifactID, MarkSpec{
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
	if err := service.RejectSuggestion(context.Background(), artifactID, "rep", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := service.RejectSuggestion(context.Background(), artifactID, "ins", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatal(err)
	}
	waitForDocumentText(t, service, artifactID, "keep this and drop \n")
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
	if err := service.ProjectMark(context.Background(), artifactID, "c1", record); err != nil {
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

	editLiveTree(t, service, artifactID, replaceRun("brown", "browner"))
	waitForDocumentVersion(t, service.store, artifactID, 2)
	if anchor := loadAskAnchor(t, service, askID); anchor.Quote != "browner" || anchor.Orphaned {
		t.Fatalf("refreshed anchor = %#v, want browner and not orphaned", anchor)
	}

	editLiveTree(t, service, artifactID, deleteRun("browner"))
	waitForDocumentVersion(t, service.store, artifactID, 3)
	if anchor := loadAskAnchor(t, service, askID); !anchor.Orphaned || anchor.Version != 1 {
		t.Fatalf("orphaned anchor = %#v, want version-1 orphan", anchor)
	}
}

func TestSettleSweepsUnrecordedMarksAfterTTL(t *testing.T) {
	database := openTestStore(t)
	artifactID := createDocument(t, database, "")
	service := New(Deps{
		Store:             database,
		Events:            events.NewBroker(),
		Settle:            20 * time.Millisecond,
		UnrecordedMarkTTL: 200 * time.Millisecond,
	})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	seedServiceText(t, service, artifactID, "The quick brown fox")
	browserMark(t, service, artifactID, "proofComment", "dangling", "quick")
	browserMarkWithAttrs(t, service, artifactID, "proofAuthored", "fox", pmdoc.Attrs{"id": "auth", "by": "user:bob"})
	resolvedCommentID := insertAnchoredComment(t, service, artifactID, "brown")
	if _, err := database.Pool.Exec(context.Background(), `update comments set resolved = true where id = $1`, resolvedCommentID); err != nil {
		t.Fatalf("resolve recorded comment: %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	if _, _, found := pmdoc.FindMark(liveTree(t, service, artifactID), "proofComment", "dangling"); !found {
		t.Fatal("unrecorded mark swept before its TTL")
	}
	waitFor(t, time.Second, "unrecorded mark removed", func() bool {
		_, _, found := pmdoc.FindMark(liveTree(t, service, artifactID), "proofComment", "dangling")
		return !found
	})
	if _, _, found := pmdoc.FindMark(liveTree(t, service, artifactID), "proofAuthored", "auth"); !found {
		t.Fatal("proof-authored mark was swept")
	}
	if _, _, found := pmdoc.FindMark(liveTree(t, service, artifactID), "proofComment", resolvedCommentID); !found {
		t.Fatal("recorded resolved comment mark was swept")
	}
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
	if err := service.ProjectMark(WithTx(context.Background(), tx), artifactID, "c1", MarkRecord{
		Kind: "comment", By: "user:alice", CreatedAt: "2026-09-10T00:00:00Z", Text: "note",
	}); err != nil {
		t.Fatalf("project mark: %v", err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit projection transaction: %v", err)
	}
	if version := waitForDocumentVersion(t, service.store, artifactID, 2); version.Named {
		t.Fatalf("settled projection version = %#v, want unnamed browser edit version", version)
	}
}

func TestReplaceTextReanchorsOpenRows(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = 20 * time.Millisecond
	seedServiceText(t, service, artifactID, "The quick brown fox")
	askID := insertAnchoredAsk(t, service, artifactID, "brown")
	commentID := insertAnchoredComment(t, service, artifactID, "fox")

	if _, err := service.ReplaceText(context.Background(), artifactID, "A quick brown dog and a slow brown cat", model.Actor{Kind: "user", ID: "alice"}); err != nil {
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
	if _, quote, found := pmdoc.FindMark(liveTree(t, service, artifactID), "dispatchAsk", askID); !found || quote != "brown" {
		t.Fatalf("ask mark after replace = %q found=%t, want brown", quote, found)
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
	if _, err := service.MarkQuote(context.Background(), artifactID, MarkSpec{
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
	return id
}

func insertAnchoredComment(t *testing.T, service *Service, artifactID, quote string) string {
	t.Helper()
	const id = "00000000-0000-4000-8000-000000000002"
	if _, err := service.MarkQuote(context.Background(), artifactID, MarkSpec{
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
