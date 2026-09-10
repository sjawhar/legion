package docs

import (
	"context"
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
