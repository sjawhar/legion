package docs

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A transaction renders its document once. The edit's own rendering is what the version
// snapshot writes, so a snapshot that walks and renders the tree again is doing the whole
// document's work twice while it holds the issue row.
func TestSnapshotReusesTheTransactionsRenderedDocument(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")

	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin transactional edit: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	joinedCtx, ledger := service.Join(ctx, tx)
	t.Cleanup(ledger.Discard)

	alice := model.Actor{Kind: "user", ID: "alice"}
	if _, err := service.ApplyOps(joinedCtx, artifactID, []model.EditOp{{
		Op: "replace", Find: "before", With: "after",
	}}, alice, nil); err != nil {
		t.Fatalf("joined edit: %v", err)
	}
	write := ledger.liveWriteFor(artifactID)
	if write == nil {
		t.Fatal("the edit opened no live write")
	}
	if write.tree == nil || write.markdown == "" {
		t.Fatalf("the edit recorded no rendering: tree=%v markdown=%q", write.tree, write.markdown)
	}

	tree, markdown, _, _, err := service.captureLiveTextAndAuthors(joinedCtx, artifactID, nil)
	if err != nil {
		t.Fatalf("capture live text: %v", err)
	}
	if tree != write.tree {
		t.Fatal("the snapshot walked the document again instead of reusing the edit's tree")
	}
	if markdown != write.markdown {
		t.Fatalf("the snapshot rendered the document again: %q, want the edit's %q", markdown, write.markdown)
	}
}

// The reuse is only sound while the fork it was taken from still describes the document. A
// browser update the room gains under the transaction moves that fork, and the snapshot must
// render the moved document, not the rendering it cached before.
func TestAForkThatGainsRoomContentIsRenderedAgain(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")

	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin transactional edit: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	joinedCtx, ledger := service.Join(ctx, tx)
	t.Cleanup(ledger.Discard)

	alice := model.Actor{Kind: "user", ID: "alice"}
	if _, err := service.ApplyOps(joinedCtx, artifactID, []model.EditOp{{
		Op: "replace", Find: "before", With: "after",
	}}, alice, nil); err != nil {
		t.Fatalf("joined edit: %v", err)
	}
	editLiveTree(t, service, artifactID, appendBlocks(t, "gained in the room"))

	_, markdown, _, _, err := service.captureLiveTextAndAuthors(joinedCtx, artifactID, nil)
	if err != nil {
		t.Fatalf("capture live text: %v", err)
	}
	if !strings.Contains(markdown, "gained in the room") {
		t.Fatalf("snapshot markdown = %q, want it to carry what the room gained", markdown)
	}
	if !strings.Contains(markdown, "after") {
		t.Fatalf("snapshot markdown = %q, want it to carry the transaction's own edit", markdown)
	}
}

// anchorQueryCounter counts the statement openAnchoredMarks runs, which is one per anchor
// refresh pass per table. It is how a second refresh of the same tree in one transaction is
// visible from outside the package under test.
type anchorQueryCounter struct{ count atomic.Int64 }

func (c *anchorQueryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "anchor->>'artifact_id' = $1") {
		c.count.Add(1)
	}
	return ctx
}

func (c *anchorQueryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// countedService is newTestService on a pool whose anchor reads are counted.
func countedService(t *testing.T) (*Service, string, *anchorQueryCounter) {
	t.Helper()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	config := database.Pool.Config().Copy()
	counter := &anchorQueryCounter{}
	config.ConnConfig.Tracer = counter
	counted, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open counted pool: %v", err)
	}
	t.Cleanup(counted.Close)
	service := New(Deps{
		Store:  &store.Store{Pool: store.NewPool(counted)},
		Events: events.NewBroker(),
		Settle: time.Hour,
	})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	return service, artifactID, counter
}

// seedAnchoredAsk marks quote in the document and commits an open ask anchored to that mark,
// carrying storedQuote as the quote its row remembers.
func seedAnchoredAsk(t *testing.T, service *Service, artifactID, askID, quote, storedQuote string) {
	t.Helper()
	alice := model.Actor{Kind: "user", ID: "alice"}
	if _, err := service.MarkQuote(context.Background(), artifactID, MarkSpec{
		Kind: MarkAsk, ID: askID, By: alice,
	}, quote, nil); err != nil {
		t.Fatalf("mark the ask's quote: %v", err)
	}
	anchor, err := json.Marshal(model.Anchor{
		ArtifactID: artifactID, MarkID: askID, Version: 1, Quote: storedQuote,
	})
	if err != nil {
		t.Fatalf("encode ask anchor: %v", err)
	}
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin anchored ask: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		insert into asks (id, issue_key, author, question, anchor)
		values ($1, 'DOC-1', '{"kind":"user","id":"alice"}', 'Anchored ask', $2)
	`, askID, anchor); err != nil {
		t.Fatalf("create anchored ask: %v", err)
	}
	if _, err := service.events.Append(ctx, tx, model.Event{
		IssueKey: new("DOC-1"),
		Type:     "ask.opened",
		Actor:    alice,
		Payload:  model.NewAskEventPayload(model.Ask{ID: askID}, model.ReferenceChanges{}),
	}); err != nil {
		t.Fatalf("record opened ask event: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit anchored ask: %v", err)
	}
}

// anchorRefreshPasses is how many refresh passes count anchor reads represent: openAnchoredMarks
// reads asks and comments, so one pass is two statements.
const anchorRefreshPasses = 2

// One edit refreshes each anchor once. Both the edit's own mutation and its version snapshot
// refresh anchors from the same tree; refreshing twice is a whole extra pass over every open
// anchor, under the issue row, for no change.
func TestAnEditRefreshesItsAnchorsOncePerTransaction(t *testing.T) {
	service, artifactID, counter := countedService(t)
	seedServiceText(t, service, artifactID, "before the anchored quote and after")

	askID := "00000000-0000-4000-8000-0000000000a1"
	alice := model.Actor{Kind: "user", ID: "alice"}
	seedAnchoredAsk(t, service, artifactID, askID, "anchored quote", "anchored quote")

	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin transactional edit: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	joinedCtx, ledger := service.Join(ctx, tx)
	t.Cleanup(ledger.Discard)

	counter.count.Store(0)
	if _, err := service.ApplyOps(joinedCtx, artifactID, []model.EditOp{{
		Op: "replace", Find: "before", With: "BEFORE",
	}}, alice, nil); err != nil {
		t.Fatalf("joined edit: %v", err)
	}
	if _, err := service.SnapshotVersion(joinedCtx, tx, artifactID, alice); err != nil {
		t.Fatalf("snapshot version: %v", err)
	}
	if passes := counter.count.Load(); passes != anchorRefreshPasses {
		t.Fatalf("anchor reads = %d, want %d: one refresh pass per edit", passes, anchorRefreshPasses)
	}

	refreshes := 0
	for _, event := range ledger.Events() {
		if event.Type == "ask.anchor_refreshed" {
			refreshes++
		}
	}
	if refreshes != 0 {
		t.Fatalf("ask.anchor_refreshed events = %d, want none: the edit left the quote alone", refreshes)
	}
}

// An edit that moves an anchored quote refreshes that anchor exactly once, whichever pass does
// it: the row carries the new quote and the change is announced one time.
func TestAnEditAnnouncesEachChangedAnchorOnce(t *testing.T) {
	service, artifactID, _ := countedService(t)
	seedServiceText(t, service, artifactID, "before the anchored quote and after")

	askID := "00000000-0000-4000-8000-0000000000a2"
	alice := model.Actor{Kind: "user", ID: "alice"}
	seedAnchoredAsk(t, service, artifactID, askID, "anchored quote", "stale quote")

	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin transactional edit: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	joinedCtx, ledger := service.Join(ctx, tx)
	t.Cleanup(ledger.Discard)

	if _, err := service.ApplyOps(joinedCtx, artifactID, []model.EditOp{{
		Op: "replace", Find: "before", With: "BEFORE",
	}}, alice, nil); err != nil {
		t.Fatalf("joined edit: %v", err)
	}
	if _, err := service.SnapshotVersion(joinedCtx, tx, artifactID, alice); err != nil {
		t.Fatalf("snapshot version: %v", err)
	}

	refreshes := 0
	for _, event := range ledger.Events() {
		if event.Type == "ask.anchor_refreshed" {
			refreshes++
		}
	}
	if refreshes != 1 {
		t.Fatalf("ask.anchor_refreshed events = %d, want exactly 1", refreshes)
	}
	var encoded []byte
	if err := tx.QueryRow(ctx, `select anchor from asks where id = $1`, askID).Scan(&encoded); err != nil {
		t.Fatalf("read refreshed ask: %v", err)
	}
	var refreshed model.Anchor
	if err := json.Unmarshal(encoded, &refreshed); err != nil {
		t.Fatalf("decode refreshed ask: %v", err)
	}
	if refreshed.Quote != "anchored quote" || refreshed.Orphaned {
		t.Fatalf("refreshed anchor = %#v, want the current quote", refreshed)
	}
}

// A version written without a live write of its own - a named version, and settlement - is the
// only refresh its anchors get, so that pass must still run.
func TestANamedVersionRefreshesAnchorsWithNoLiveWriteOfItsOwn(t *testing.T) {
	service, artifactID, counter := countedService(t)
	seedServiceText(t, service, artifactID, "before the anchored quote and after")

	askID := "00000000-0000-4000-8000-0000000000a3"
	alice := model.Actor{Kind: "user", ID: "alice"}
	seedAnchoredAsk(t, service, artifactID, askID, "anchored quote", "anchored quote")

	// The browser rewrites the document, dropping the marked text. Nothing else refreshes the
	// ask's anchor: the named version's own pass is it.
	if _, err := service.ReplaceText(context.Background(), artifactID, "the quote is gone", alice); err != nil {
		t.Fatalf("replace document text: %v", err)
	}
	counter.count.Store(0)
	if _, err := service.NamedVersion(context.Background(), artifactID, "rewritten", alice); err != nil {
		t.Fatalf("write named version: %v", err)
	}
	if passes := counter.count.Load(); passes != anchorRefreshPasses {
		t.Fatalf("anchor reads = %d, want %d: a named version refreshes anchors", passes, anchorRefreshPasses)
	}

	var encoded []byte
	if err := service.store.Pool.QueryRow(context.Background(), `select anchor from asks where id = $1`, askID).Scan(&encoded); err != nil {
		t.Fatalf("read refreshed ask: %v", err)
	}
	var refreshed model.Anchor
	if err := json.Unmarshal(encoded, &refreshed); err != nil {
		t.Fatalf("decode refreshed ask: %v", err)
	}
	if !refreshed.Orphaned {
		t.Fatalf("refreshed anchor = %#v, want it orphaned by the rewrite", refreshed)
	}
}

// boldBlock returns a live edit that marks a paragraph's text strong without changing a
// character of it, as a browser does when it selects a line and hits bold.
func boldBlock(t *testing.T, text string) func(*pmdoc.Node) *pmdoc.Node {
	t.Helper()
	return func(tree *pmdoc.Node) *pmdoc.Node {
		marked := false
		var walk func(node *pmdoc.Node)
		walk = func(node *pmdoc.Node) {
			if node.Type == "text" && node.Text == text {
				node.Marks = append(node.Marks, pmdoc.Mark{Type: "strong"})
				marked = true
				return
			}
			for _, child := range node.Children {
				walk(child)
			}
		}
		walk(tree)
		if !marked {
			t.Fatalf("no text node carries %q", text)
		}
		return tree
	}
}

// A formatting change carries no new text, but it does change the canonical markdown the
// version records, so the rendering cached from the fork before it no longer describes the
// document.
func TestAForkThatGainsRoomFormattingIsRenderedAgain(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before\n\nplain paragraph")

	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin transactional edit: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	joinedCtx, ledger := service.Join(ctx, tx)
	t.Cleanup(ledger.Discard)

	alice := model.Actor{Kind: "user", ID: "alice"}
	if _, err := service.ApplyOps(joinedCtx, artifactID, []model.EditOp{{
		Op: "replace", Find: "before", With: "after",
	}}, alice, nil); err != nil {
		t.Fatalf("joined edit: %v", err)
	}
	editLiveTree(t, service, artifactID, boldBlock(t, "plain paragraph"))

	_, markdown, _, _, err := service.captureLiveTextAndAuthors(joinedCtx, artifactID, nil)
	if err != nil {
		t.Fatalf("capture live text: %v", err)
	}
	if !strings.Contains(markdown, "**plain paragraph**") {
		t.Fatalf("snapshot markdown = %q, want the browser's formatting", markdown)
	}
	if !strings.Contains(markdown, "after") {
		t.Fatalf("snapshot markdown = %q, want it to carry the transaction's own edit", markdown)
	}
}
