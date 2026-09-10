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

func TestSeedTextStoresTreeAndReturnsCanonicalMarkdown(t *testing.T) {
	database := openTestStore(t)
	artifactID := createDocument(t, database, "")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	tx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := service.SeedText(context.Background(), tx, artifactID, "## Database\nUse SQLite")
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
	database := openTestStore(t)
	artifactID := createDocument(t, database, "")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: 20 * time.Millisecond})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })

	tx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin seed transaction: %v", err)
	}
	if _, err := service.SeedText(context.Background(), tx, artifactID, "# Rolled back"); err != nil {
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
	if _, err := service.ReplaceText(WithTx(ctx, tx), artifactID, "# Rolled back", actor); err != nil {
		t.Fatalf("replace text: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback replace transaction: %v", err)
	}
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
func TestTransactionalApplyDoesNotScheduleSettlement(t *testing.T) {
	service, artifactID := newTestService(t)
	const settleInterval = 50 * time.Millisecond
	service.settle = settleInterval
	seedServiceText(t, service, artifactID, "before")
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin transactional edit: %v", err)
	}
	if _, err := service.ApplyOps(WithTx(ctx, tx), artifactID, []model.EditOp{{Op: "replace", Find: "before", With: "after"}}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"}); err != nil {
		t.Fatalf("apply transactional edit: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit transactional edit: %v", err)
	}
	time.Sleep(3 * settleInterval)
	var versions int
	if err := service.store.Pool.QueryRow(ctx, `
		select count(*) from artifact_versions where artifact_id = $1
	`, artifactID).Scan(&versions); err != nil {
		t.Fatalf("count transactional edit versions: %v", err)
	}
	if versions != 1 {
		t.Fatalf("versions after transactional edit = %d, want 1", versions)
	}
	if got, err := service.Text(ctx, artifactID); err != nil || got != "after\n" {
		t.Fatalf("document after transactional edit = %q (%v), want after", got, err)
	}
}

func TestTransactionalApplyReresolvesAnchoredComment(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "target")
	anchorJSON, err := json.Marshal(model.Anchor{
		ArtifactID: artifactID,
		Version:    1,
		Quote:      "target",
		From:       0,
		To:         len("target"),
	})
	if err != nil {
		t.Fatalf("encode comment anchor: %v", err)
	}
	if _, err := service.store.Pool.Exec(context.Background(), `
		insert into comments (issue_key, author, body, anchor)
		values ('DOC-1', '{"kind":"user","id":"alice"}', 'Anchored comment', $1)
	`, anchorJSON); err != nil {
		t.Fatalf("create anchored comment: %v", err)
	}

	tx, err := service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin transactional edit: %v", err)
	}
	defer tx.Rollback(context.Background())
	if _, err := service.ApplyOps(WithTx(context.Background(), tx), artifactID, []model.EditOp{{
		Op: "insert", Markdown: "before ", Before: "target",
	}}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"}); err != nil {
		t.Fatalf("apply transactional edit: %v", err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit transactional edit: %v", err)
	}

	var stored []byte
	if err := service.store.Pool.QueryRow(context.Background(), `select anchor from comments where body = 'Anchored comment'`).Scan(&stored); err != nil {
		t.Fatalf("load re-resolved comment anchor: %v", err)
	}
	var anchor model.Anchor
	if err := json.Unmarshal(stored, &anchor); err != nil {
		t.Fatalf("decode re-resolved comment anchor: %v", err)
	}
	if anchor.From != len("before ") || anchor.To != len("before target") || anchor.Quote != "target" || anchor.Orphaned {
		t.Fatalf("transactional edit anchor = %#v, want target at [7,13)", anchor)
	}
	var versions int
	if err := service.store.Pool.QueryRow(context.Background(), `select count(*) from artifact_versions where artifact_id = $1`, artifactID).Scan(&versions); err != nil {
		t.Fatalf("count transactional edit versions: %v", err)
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
	_, err := service.ApplyOps(context.Background(), artifactID, []model.EditOp{{Op: "replace", Find: "before", With: "after"}}, actor)
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
	version, err := service.NamedVersion(context.Background(), artifactID, "checkpoint", actor)
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
	version, wrote, err := service.SnapshotVersion(context.Background(), tx, artifactID, snapshotter)
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
	version, wrote, err := service.SnapshotVersion(context.Background(), tx, artifactID, snapshotter)
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
	version, err = service.NamedVersion(WithTx(context.Background(), tx), artifactID, "checkpoint", snapshotter)
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
	version, wrote, err := service.SnapshotVersion(context.Background(), tx, artifactID, first)
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
	version, err = service.NamedVersion(WithTx(context.Background(), tx), artifactID, "checkpoint", first)
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
	database := openTestStore(t)
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
	version, wrote, err := service.SnapshotVersion(context.Background(), tx, artifactID, snapshotter)
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
func TestApplyReplaceResolvesAgainstDocumentInsideApply(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "# First")
	if _, err := service.ReplaceText(context.Background(), artifactID, "base", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("prepare document: %v", err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	service.srv.OnInject = func(ctx context.Context, info ygws.InjectInfo) error {
		select {
		case <-entered:
		default:
			close(entered)
			<-release
		}
		return service.allowInject(ctx, info)
	}

	result := make(chan error, 1)
	go func() {
		result <- service.ApplyReplace(context.Background(), artifactID, model.Anchor{ArtifactID: artifactID, Quote: "base", To: 4}, "server", model.Actor{Kind: "session", ID: "session-0123456789abcdef"})
	}()
	<-entered
	editLiveTree(t, service, artifactID, replaceRun("base", "base browser"))
	close(release)
	if err := <-result; err != nil {
		t.Fatalf("apply replacement: %v", err)
	}
	waitForDocumentText(t, service, artifactID, "server browser\n")
}

func TestReplaceTextAcceptsUnchangedEmptyDocument(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "")
	if _, err := service.ReplaceText(context.Background(), artifactID, "", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("replace unchanged empty document: %v", err)
	}
}

func TestReresolveAnchorsClosesRowsBeforeUpdating(t *testing.T) {
	service, artifactID := newTestService(t)
	askAnchor, err := json.Marshal(model.Anchor{ArtifactID: artifactID, Version: 1, Quote: "after", From: 0, To: len("after")})
	if err != nil {
		t.Fatalf("encode ask anchor: %v", err)
	}
	commentAnchor, err := json.Marshal(model.Anchor{ArtifactID: artifactID, Version: 1, Quote: "before", From: 0, To: len("before")})
	if err != nil {
		t.Fatalf("encode comment anchor: %v", err)
	}
	tx, err := service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin anchor transaction: %v", err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(context.Background(), `
		insert into asks (issue_key, author, question, anchor)
		values ('DOC-1', '{"kind":"user","id":"alice"}', 'Moved ask', $1)
	`, askAnchor); err != nil {
		t.Fatalf("create anchored ask: %v", err)
	}
	if _, err := tx.Exec(context.Background(), `
		insert into comments (issue_key, author, body, anchor)
		values ('DOC-1', '{"kind":"user","id":"alice"}', 'Orphaned comment', $1)
	`, commentAnchor); err != nil {
		t.Fatalf("create anchored comment: %v", err)
	}
	if err := service.reresolveAnchors(context.Background(), tx, artifactID, "prefix after\n"); err != nil {
		t.Fatalf("reresolve anchors after closing rows: %v", err)
	}

	var encoded []byte
	if err := tx.QueryRow(context.Background(), `select anchor from asks where question = 'Moved ask'`).Scan(&encoded); err != nil {
		t.Fatalf("read re-resolved ask: %v", err)
	}
	var moved model.Anchor
	if err := json.Unmarshal(encoded, &moved); err != nil {
		t.Fatalf("decode re-resolved ask: %v", err)
	}
	if moved.From != len("prefix ") || moved.To != len("prefix after") || moved.Orphaned {
		t.Fatalf("re-resolved ask = %#v, want after at [7,12)", moved)
	}
	if err := tx.QueryRow(context.Background(), `select anchor from comments where body = 'Orphaned comment'`).Scan(&encoded); err != nil {
		t.Fatalf("read re-resolved comment: %v", err)
	}
	var orphaned model.Anchor
	if err := json.Unmarshal(encoded, &orphaned); err != nil {
		t.Fatalf("decode re-resolved comment: %v", err)
	}
	if !orphaned.Orphaned {
		t.Fatalf("re-resolved comment = %#v, want orphaned", orphaned)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit re-resolved anchors: %v", err)
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
