package docs

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestSeedTextStoresTreeAndReturnsCanonicalMarkdown(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	tx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := service.SeedText(context.Background(), tx, artifactID, "## Database\nUse SQLite", model.Actor{Kind: "user", ID: "seed"})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if canonical != "## Database\n\nUse SQLite\n" {
		t.Fatalf("canonical = %q", canonical)
	}
	if got, err := service.Text(context.Background(), artifactID); err != nil || got != canonical {
		t.Fatalf("Text = %q (%v)", got, err)
	}
	loaded, err := service.persistence.Load(context.Background(), artifactID)
	if err != nil {
		t.Fatal(err)
	}
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, loaded.Update, nil); err != nil {
		t.Fatal(err)
	}
	if doc.GetText("content").Len() != 0 {
		t.Fatal("legacy content text must stay empty")
	}
	tree, err := pmdoc.Read(doc.GetXmlFragment(fragmentName))
	if err != nil || len(tree.Children) != 2 || tree.Children[0].Type != "heading" {
		t.Fatalf("tree = %#v (%v)", tree, err)
	}
}

func TestSeedTextRollsBackWithCreatingTransaction(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: 20 * time.Millisecond})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })

	tx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin seed transaction: %v", err)
	}
	if _, err := service.SeedText(context.Background(), tx, artifactID, "# Rolled back", model.Actor{Kind: "user", ID: "seed"}); err != nil {
		t.Fatalf("seed text: %v", err)
	}
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatalf("rollback seed transaction: %v", err)
	}

	var updates int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from doc_updates where artifact_id = $1`, artifactID).Scan(&updates); err != nil {
		t.Fatalf("count document updates: %v", err)
	}
	if updates != 0 {
		t.Fatalf("rolled-back seed left %d document updates", updates)
	}
}

func TestReplaceTextUpdatesLiveDocumentAndSettlesVersion(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "# First")
	actor := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	if _, err := service.ReplaceText(context.Background(), artifactID, "# Replaced", actor); err != nil {
		t.Fatalf("replace text: %v", err)
	}
	waitForDocumentText(t, service, artifactID, "# Replaced\n")
	version := waitForDocumentVersion(t, service.store, artifactID, 2)
	if version.Named || len(version.Authors) != 1 || version.Authors[0] != actor {
		t.Fatalf("settled version = %#v, want unnamed version authored by %v", version, actor)
	}
	var eventType string
	var notify bool
	if err := service.store.Pool.QueryRow(context.Background(), `
		select type, notify from events where issue_key = 'DOC-1' order by seq desc limit 1
	`).Scan(&eventType, &notify); err != nil {
		t.Fatalf("read settle event: %v", err)
	}
	if eventType != "artifact.version" || notify {
		t.Fatalf("settle event = %q notify=%t, want non-notifying artifact.version", eventType, notify)
	}
}

func TestReplaceTextWithTransactionRollsBackUpdate(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "# First")
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin replace transaction: %v", err)
	}
	actor := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	joined, collector := joinTx(ctx, tx)
	if _, err := service.ReplaceText(joined, artifactID, "# Rolled back", actor); err != nil {
		t.Fatalf("replace text: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback replace transaction: %v", err)
	}
	service.DiscardLiveWrites(collector)
	if err := service.srv.CloseRoom(artifactID, true); err != nil {
		t.Fatalf("close live document: %v", err)
	}
	waitForNoLiveDocument(t, service, artifactID)

	got, err := service.Text(ctx, artifactID)
	if err != nil {
		t.Fatalf("read durable text: %v", err)
	}
	if got != "# First\n" {
		t.Fatalf("durable text = %q, want %q", got, "# First")
	}
	var updates int
	if err := service.store.Pool.QueryRow(ctx, "select count(*) from doc_updates where artifact_id = $1", artifactID).Scan(&updates); err != nil {
		t.Fatalf("count document updates: %v", err)
	}
	if updates != 1 {
		t.Fatalf("rolled-back replace left %d document updates, want 1", updates)
	}
}
func TestTransactionalApplySchedulesSettlementAfterCommit(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = 20 * time.Millisecond
	seedServiceText(t, service, artifactID, "before")
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin transactional edit: %v", err)
	}
	joined, collector := joinTx(ctx, tx)
	if _, err := service.ApplyOps(joined, artifactID, []model.EditOp{{Op: "replace", Find: "before", With: "after"}}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"}, nil); err != nil {
		t.Fatalf("apply transactional edit: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit transactional edit: %v", err)
	}
	service.ScheduleSettlement(artifactID)
	service.PublishLiveWrites(collector)
	version := waitForDocumentVersion(t, service.store, artifactID, 2)
	if len(version.Authors) != 1 || version.Authors[0].ID != "session-0123456789abcdef" {
		t.Fatalf("settled transactional version authors = %#v", version.Authors)
	}
}

func TestTransactionalApplyRefreshesAnchoredComment(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "target")
	const commentID = "00000000-0000-4000-8000-000000000003"
	if _, err := service.MarkQuote(context.Background(), artifactID, MarkSpec{
		Kind: MarkComment,
		ID:   commentID,
		By:   model.Actor{Kind: "user", ID: "alice"},
	}, "target", nil); err != nil {
		t.Fatalf("mark anchored comment: %v", err)
	}
	anchorJSON, err := json.Marshal(model.Anchor{
		ArtifactID: artifactID,
		MarkID:     commentID,
		Version:    1,
		Quote:      "target",
	})
	if err != nil {
		t.Fatalf("encode comment anchor: %v", err)
	}
	if _, err := service.store.Pool.Exec(context.Background(), `
		insert into comments (id, issue_key, author, body, anchor)
		values ($1, 'DOC-1', '{"kind":"user","id":"alice"}', 'Anchored comment', $2)
	`, commentID, anchorJSON); err != nil {
		t.Fatalf("create anchored comment: %v", err)
	}

	tx, err := service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin transactional edit: %v", err)
	}
	defer tx.Rollback(context.Background())
	joined, collector := joinTx(context.Background(), tx)
	if _, err := service.ApplyOps(joined, artifactID, []model.EditOp{
		{Op: "insert", Markdown: "before ", Before: "target"},
	}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"}, nil); err != nil {
		t.Fatalf("apply transactional edit: %v", err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit transactional edit: %v", err)
	}
	service.PublishLiveWrites(collector)

	var stored []byte
	if err := service.store.Pool.QueryRow(context.Background(), `select anchor from comments where id = $1`, commentID).Scan(&stored); err != nil {
		t.Fatalf("load refreshed comment anchor: %v", err)
	}
	var anchor model.Anchor
	if err := json.Unmarshal(stored, &anchor); err != nil {
		t.Fatalf("decode refreshed comment anchor: %v", err)
	}
	if anchor.MarkID != commentID || anchor.Quote != "target" || anchor.Orphaned {
		t.Fatalf("transactional edit anchor = %#v, want live target mark", anchor)
	}
	var versions int
	if err := service.store.Pool.QueryRow(context.Background(), `select count(*) from artifact_versions where artifact_id = $1`, artifactID).Scan(&versions); err != nil {
		t.Fatalf("count document versions: %v", err)
	}
	if versions != 1 {
		t.Fatalf("transactional edit wrote %d versions, want no live-client settle", versions)
	}
}
func TestClosedIssueRejectsLiveEditsAndNamedVersions(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	if _, err := service.store.Pool.Exec(context.Background(), `update issues set closed_at = now() where key = 'DOC-1'`); err != nil {
		t.Fatalf("close document issue: %v", err)
	}
	actor := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	_, err := service.ApplyOps(context.Background(), artifactID, []model.EditOp{{Op: "replace", Find: "before", With: "after"}}, actor, nil)
	if !errors.Is(err, ErrIssueClosed) {
		t.Fatalf("edit closed document error = %v, want ErrIssueClosed", err)
	}
	if _, err := service.NamedVersion(context.Background(), artifactID, "checkpoint", actor); !errors.Is(err, ErrIssueClosed) {
		t.Fatalf("version closed document error = %v, want ErrIssueClosed", err)
	}
	waitForDocumentText(t, service, artifactID, "before\n")
}

func TestNamedVersionIncludesTrackedActorsAndResetsRoom(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	connected := model.Actor{Kind: "user", ID: "alice"}
	actor := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	service.recordActor(artifactID, connected)
	namedResult, err := service.NamedVersion(context.Background(), artifactID, "checkpoint", actor)
	version := namedResult.Version
	if err != nil {
		t.Fatalf("name document version: %v", err)
	}
	if !version.Named || version.Summary == nil || *version.Summary != "checkpoint" {
		t.Fatalf("named version = %#v, want named checkpoint", version)
	}
	if len(version.Authors) != 2 || version.Authors[0] != actor || version.Authors[1] != connected {
		t.Fatalf("named version authors = %#v, want %v and %v", version.Authors, actor, connected)
	}
	state := service.room(artifactID)
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.pending) != 0 {
		t.Fatalf("pending authors after named version = %#v, want empty", state.pending)
	}
}
func TestSnapshotVersionDoesNotAttributeUnchangedDocument(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "# First")
	snapshotter := model.Actor{Kind: "user", ID: "alice"}
	editor := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}

	tx, err := service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin snapshot transaction: %v", err)
	}
	defer tx.Rollback(context.Background())
	versionResult, err := service.SnapshotVersion(context.Background(), tx, artifactID, snapshotter)
	version, wrote := versionResult.Version, versionResult.Wrote
	if err != nil {
		t.Fatalf("snapshot unchanged document: %v", err)
	}
	if wrote || version.Number != 1 {
		t.Fatalf("unchanged snapshot = %#v wrote=%t, want version 1 without a write", version, wrote)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit snapshot transaction: %v", err)
	}

	if _, err := service.ReplaceText(context.Background(), artifactID, "after", editor); err != nil {
		t.Fatalf("replace document: %v", err)
	}
	version = waitForDocumentVersion(t, service.store, artifactID, 2)
	if len(version.Authors) != 1 || version.Authors[0] != editor {
		t.Fatalf("settled version authors = %#v, want only %v", version.Authors, editor)
	}
}
func TestCommittedSnapshotAndNamedVersionsClearPendingAuthors(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	editor := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	snapshotter := model.Actor{Kind: "user", ID: "alice"}
	if _, err := service.ReplaceText(context.Background(), artifactID, "after", editor); err != nil {
		t.Fatalf("edit before snapshot: %v", err)
	}

	tx, err := service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin snapshot transaction: %v", err)
	}
	versionResult, err := service.SnapshotVersion(context.Background(), tx, artifactID, snapshotter)
	version, wrote := versionResult.Version, versionResult.Wrote
	if err != nil {
		t.Fatalf("snapshot version: %v", err)
	}
	if !wrote {
		t.Fatal("snapshot did not write dirty document")
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit snapshot transaction: %v", err)
	}
	service.CommitVersion(artifactID, version)
	state := service.room(artifactID)
	state.mu.Lock()
	pending := len(state.pending)
	state.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending authors after committed snapshot = %d, want 0", pending)
	}

	tx, err = service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin named version transaction: %v", err)
	}
	namedResult, err := service.NamedVersion(WithTx(context.Background(), tx), artifactID, "checkpoint", snapshotter)
	version = namedResult.Version
	if err != nil {
		t.Fatalf("named version: %v", err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit named version transaction: %v", err)
	}
	service.CommitVersion(artifactID, version)
	state.mu.Lock()
	pending = len(state.pending)
	state.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending authors after committed named version = %d, want 0", pending)
	}
}

func TestVersionCaptureDoesNotClearAuthorsFromLaterEdits(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	first := model.Actor{Kind: "user", ID: "alice"}
	second := model.Actor{Kind: "user", ID: "bob"}
	if _, err := service.ReplaceText(context.Background(), artifactID, "first", first); err != nil {
		t.Fatalf("first edit: %v", err)
	}
	tx, err := service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin snapshot transaction: %v", err)
	}
	versionResult, err := service.SnapshotVersion(context.Background(), tx, artifactID, first)
	version, wrote := versionResult.Version, versionResult.Wrote
	if err != nil || !wrote {
		t.Fatalf("snapshot dirty document = %#v, wrote=%t, err=%v", version, wrote, err)
	}
	if _, err := service.ReplaceText(context.Background(), artifactID, "second", second); err != nil {
		t.Fatalf("later edit: %v", err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit snapshot transaction: %v", err)
	}
	service.CommitVersion(artifactID, version)
	state := service.room(artifactID)
	state.mu.Lock()
	_, retained := state.pending[actorKey(second)]
	state.mu.Unlock()
	if !retained {
		t.Fatal("committed snapshot cleared author from later edit")
	}

	tx, err = service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin named transaction: %v", err)
	}
	namedResult, err := service.NamedVersion(WithTx(context.Background(), tx), artifactID, "checkpoint", first)
	version = namedResult.Version
	if err != nil {
		t.Fatalf("name document version: %v", err)
	}
	if _, err := service.ReplaceText(context.Background(), artifactID, "third", second); err != nil {
		t.Fatalf("later named-version edit: %v", err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit named transaction: %v", err)
	}
	service.CommitVersion(artifactID, version)
	state.mu.Lock()
	_, retained = state.pending[actorKey(second)]
	state.mu.Unlock()
	if !retained {
		t.Fatal("committed named version cleared author from later edit")
	}
}

func TestColdSnapshotCapturesFirstEditAfterWarm(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	seedServiceText(t, service, artifactID, "before")
	if err := service.srv.Apply(context.Background(), artifactID, func(_ *crdt.Doc, _ func(func(*crdt.Transaction))) {}); err != nil && !errors.Is(err, ygws.ErrNoChanges) {
		t.Fatalf("warm cold document: %v", err)
	}
	editor := model.Actor{Kind: "user", ID: "alice"}
	snapshotter := model.Actor{Kind: "user", ID: "bob"}
	editDone := make(chan error, 1)
	go func() {
		editDone <- func() error {
			_, err := service.ReplaceText(context.Background(), artifactID, "after", editor)
			return err
		}()
	}()
	if err := <-editDone; err != nil {
		t.Fatalf("first edit: %v", err)
	}
	tx, err := service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin snapshot transaction: %v", err)
	}
	versionResult, err := service.SnapshotVersion(context.Background(), tx, artifactID, snapshotter)
	version, wrote := versionResult.Version, versionResult.Wrote
	if err != nil || !wrote {
		t.Fatalf("cold snapshot = %#v, wrote=%t, err=%v; want a version after first edit", version, wrote, err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit snapshot transaction: %v", err)
	}
	service.CommitVersion(artifactID, version)
	var markdown string
	if err := service.store.Pool.QueryRow(context.Background(), `
		select markdown from artifact_versions where artifact_id = $1 and number = $2
	`, artifactID, version.Number).Scan(&markdown); err != nil {
		t.Fatalf("read snapshot markdown: %v", err)
	}
	if markdown != "after\n" {
		t.Fatalf("snapshot markdown = %q, want post-edit text", markdown)
	}
	if len(version.Authors) != 2 || version.Authors[0] != editor || version.Authors[1] != snapshotter {
		t.Fatalf("snapshot authors = %#v, want editor and snapshotter", version.Authors)
	}
}

func TestReplaceTextAcceptsUnchangedEmptyDocument(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "")
	if _, err := service.ReplaceText(context.Background(), artifactID, "", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("replace unchanged empty document: %v", err)
	}
}

func TestRefreshAnchorsClosesRowsBeforeUpdating(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before after")
	const askID = "00000000-0000-4000-8000-000000000004"
	if _, err := service.MarkQuote(context.Background(), artifactID, MarkSpec{
		Kind: MarkAsk,
		ID:   askID,
		By:   model.Actor{Kind: "user", ID: "alice"},
	}, "after", nil); err != nil {
		t.Fatalf("mark ask: %v", err)
	}
	askAnchor, err := json.Marshal(model.Anchor{ArtifactID: artifactID, MarkID: askID, Version: 1, Quote: "after"})
	if err != nil {
		t.Fatalf("encode ask anchor: %v", err)
	}
	commentAnchor, err := json.Marshal(model.Anchor{ArtifactID: artifactID, MarkID: "missing", Version: 1, Quote: "before"})
	if err != nil {
		t.Fatalf("encode comment anchor: %v", err)
	}
	tx, err := service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin anchor transaction: %v", err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(context.Background(), `
		insert into asks (id, issue_key, author, question, anchor)
		values ($1, 'DOC-1', '{"kind":"user","id":"alice"}', 'Marked ask', $2)
	`, askID, askAnchor); err != nil {
		t.Fatalf("create anchored ask: %v", err)
	}
	if _, err := service.events.Append(context.Background(), tx, model.Event{
		IssueKey: new("DOC-1"),
		Type:     "ask.opened",
		Actor:    model.Actor{Kind: "user", ID: "alice"},
		Payload:  model.NewAskEventPayload(model.Ask{ID: askID}, model.ReferenceChanges{}),
	}); err != nil {
		t.Fatalf("record opened ask event: %v", err)
	}
	if _, err := tx.Exec(context.Background(), `
		insert into comments (id, issue_key, author, body, anchor)
		values ($1, 'DOC-1', '{"kind":"user","id":"alice"}', 'Missing mark comment', $2)
	`, "00000000-0000-4000-8000-000000000005", commentAnchor); err != nil {
		t.Fatalf("create anchored comment: %v", err)
	}
	if err := service.refreshAnchors(context.Background(), tx, artifactID, liveTree(t, service, artifactID), model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("refresh anchors after closing rows: %v", err)
	}

	var encoded []byte
	if err := tx.QueryRow(context.Background(), `select anchor from asks where id = $1`, askID).Scan(&encoded); err != nil {
		t.Fatalf("read refreshed ask: %v", err)
	}
	var marked model.Anchor
	if err := json.Unmarshal(encoded, &marked); err != nil {
		t.Fatalf("decode refreshed ask: %v", err)
	}
	if marked.Quote != "after" || marked.Orphaned {
		t.Fatalf("refreshed ask = %#v, want marked after", marked)
	}
	if err := tx.QueryRow(context.Background(), `select anchor from comments where body = 'Missing mark comment'`).Scan(&encoded); err != nil {
		t.Fatalf("read orphaned comment: %v", err)
	}
	var orphaned model.Anchor
	if err := json.Unmarshal(encoded, &orphaned); err != nil {
		t.Fatalf("decode orphaned comment: %v", err)
	}
	if !orphaned.Orphaned {
		t.Fatalf("refreshed comment = %#v, want orphaned", orphaned)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit refreshed anchors: %v", err)
	}
}
func TestNamedVersionIndexesDocumentReferences(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	if _, err := service.ReplaceText(context.Background(), artifactID, "See dispatch://DOC-1/artifact/spec.", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("replace document text: %v", err)
	}
	if _, err := service.NamedVersion(context.Background(), artifactID, "reference", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("write named version: %v", err)
	}
	var references int
	if err := service.store.Pool.QueryRow(context.Background(), `
		select count(*) from refs
		where from_kind = 'artifact' and from_id = $1 and to_kind = 'artifact' and to_id = 'DOC-1/spec'
	`, artifactID).Scan(&references); err != nil {
		t.Fatalf("count document references: %v", err)
	}
	if references != 1 {
		t.Fatalf("document references = %d, want 1", references)
	}
}

func TestNamedVersionIndexesServerURLDocumentReferences(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	if _, err := service.ReplaceText(context.Background(), artifactID, "See https://dispatch.example/issues/DOC-1/spec.", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("replace document text: %v", err)
	}
	if _, err := service.NamedVersion(context.Background(), artifactID, "reference", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("write named version: %v", err)
	}
	var references int
	if err := service.store.Pool.QueryRow(context.Background(), `
		select count(*) from refs
		where from_kind = 'artifact' and from_id = $1 and to_kind = 'artifact' and to_id = 'DOC-1/spec'
	`, artifactID).Scan(&references); err != nil {
		t.Fatalf("count browser-url document references: %v", err)
	}
	if references != 1 {
		t.Fatalf("browser-url document references = %d, want 1", references)
	}
}

// A live document edit is a reference write like any other: the settle event names the counted
// nodes its new markdown cites, so a reader holding those nodes' batched backlink counts — an
// Inbox row's ask — refreshes without reading the graph.
func TestSettledVersionNamesChangedReferenceTargets(t *testing.T) {
	service, artifactID := newTestService(t)
	askID := uuid.NewString()
	if _, err := service.store.Pool.Exec(context.Background(), `
		insert into asks (id, issue_key, author, question)
		values ($1, 'DOC-1', '{"kind":"user","id":"alice"}', 'Cited by a document')
	`, askID); err != nil {
		t.Fatalf("create cited ask: %v", err)
	}
	seedServiceText(t, service, artifactID, "# First")
	actor := model.Actor{Kind: "user", ID: "alice"}
	if _, err := service.ReplaceText(
		context.Background(), artifactID, "# First\n\nWaiting on dispatch://DOC-1/ask/"+askID+".", actor,
	); err != nil {
		t.Fatalf("replace text: %v", err)
	}
	waitForDocumentVersion(t, service.store, artifactID, 2)

	var payload []byte
	if err := service.store.Pool.QueryRow(context.Background(), `
		select payload from events where issue_key = 'DOC-1' and type = 'artifact.version'
		order by seq desc limit 1
	`).Scan(&payload); err != nil {
		t.Fatalf("read settle event payload: %v", err)
	}
	var named struct {
		ReferencesChanged []model.ChangedReference `json:"references_changed"`
	}
	if err := json.Unmarshal(payload, &named); err != nil {
		t.Fatalf("decode settle event payload: %v", err)
	}
	issueKey := "DOC-1"
	want := []model.ChangedReference{{Kind: "ask", ID: askID, IssueKey: &issueKey}}
	if !reflect.DeepEqual(named.ReferencesChanged, want) {
		t.Fatalf("settle event references_changed = %#v, want %#v", named.ReferencesChanged, want)
	}
}

// SnapshotVersion and NamedVersion are the two version writers the API's document handlers call,
// and each reports what its markdown moved in the reference graph: the caller names those targets
// on the artifact event it appends, so a batched backlink count refreshes without a reload.
func TestVersionWritersReportChangedReferenceTargets(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "# First\n\nPending.")
	actor := model.Actor{Kind: "user", ID: "alice"}
	issueKey := "DOC-1"
	askIDs := make([]string, 0, 2)
	for _, question := range []string{"Snapshot cites this?", "Named version cites this?"} {
		id := uuid.NewString()
		if _, err := service.store.Pool.Exec(context.Background(), `
			insert into asks (id, issue_key, author, question)
			values ($1, 'DOC-1', '{"kind":"user","id":"alice"}', $2)
		`, id, question); err != nil {
			t.Fatalf("create cited ask: %v", err)
		}
		askIDs = append(askIDs, id)
	}

	if _, err := service.ApplyOps(context.Background(), artifactID, []model.EditOp{
		{Op: "replace", Find: "Pending.", With: "Waiting on dispatch://DOC-1/ask/" + askIDs[0] + "."},
	}, actor, nil); err != nil {
		t.Fatalf("apply the citing edit: %v", err)
	}
	tx, err := service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin snapshot transaction: %v", err)
	}
	defer tx.Rollback(context.Background())
	snapshot, err := service.SnapshotVersion(context.Background(), tx, artifactID, actor)
	if err != nil {
		t.Fatalf("snapshot version: %v", err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit snapshot: %v", err)
	}
	want := []model.ChangedReference{{Kind: "ask", ID: askIDs[0], IssueKey: &issueKey}}
	if !snapshot.Wrote || !reflect.DeepEqual(snapshot.Changes.Targets, want) {
		t.Fatalf("snapshot changes = %#v (wrote=%t), want %#v", snapshot.Changes.Targets, snapshot.Wrote, want)
	}

	if _, err := service.ApplyOps(context.Background(), artifactID, []model.EditOp{
		{Op: "replace", Find: "Waiting on", With: "Also dispatch://DOC-1/ask/" + askIDs[1] + ", waiting on"},
	}, actor, nil); err != nil {
		t.Fatalf("apply the second citing edit: %v", err)
	}
	named, err := service.NamedVersion(context.Background(), artifactID, "checkpoint", actor)
	if err != nil {
		t.Fatalf("name version: %v", err)
	}
	want = []model.ChangedReference{{Kind: "ask", ID: askIDs[1], IssueKey: &issueKey}}
	if !reflect.DeepEqual(named.Changes.Targets, want) {
		t.Fatalf("named version changes = %#v, want %#v", named.Changes.Targets, want)
	}
}
