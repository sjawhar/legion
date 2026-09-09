package docs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
	ygws "github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

func newTestService(t *testing.T) (*Service, string) {
	t.Helper()
	database := openTestStore(t)
	artifactID := createDocument(t, database, "# First")
	service := New(Deps{
		Store:    database,
		Events:   events.NewBroker(),
		Identity: identity.HeaderIdentity{Header: "X-Dispatch-User", AllowedLogins: map[string]struct{}{"alice": {}}},
		Settle:   20 * time.Millisecond,
	})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	return service, artifactID
}

func TestSeedTextPersistsWithCreatingTransaction(t *testing.T) {
	database := openTestStore(t)
	artifactID := createDocument(t, database, "")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: 20 * time.Millisecond})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })

	tx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin seed transaction: %v", err)
	}
	if err := service.SeedText(context.Background(), tx, artifactID, "# Seeded"); err != nil {
		t.Fatalf("seed text: %v", err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit seed transaction: %v", err)
	}

	got, err := service.Text(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read seeded text: %v", err)
	}
	if got != "# Seeded" {
		t.Fatalf("seeded text = %q, want %q", got, "# Seeded")
	}
	var versions, updates int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from artifact_versions where artifact_id = $1
	`, artifactID).Scan(&versions); err != nil {
		t.Fatalf("count artifact versions: %v", err)
	}
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from doc_updates where artifact_id = $1
	`, artifactID).Scan(&updates); err != nil {
		t.Fatalf("count document updates: %v", err)
	}
	if versions != 1 || updates != 1 {
		t.Fatalf("seed rows = versions:%d updates:%d, want 1 and 1", versions, updates)
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
	if err := service.SeedText(context.Background(), tx, artifactID, "# Rolled back"); err != nil {
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
	if err := service.ReplaceText(context.Background(), artifactID, "# Replaced", actor); err != nil {
		t.Fatalf("replace text: %v", err)
	}
	waitForDocumentText(t, service, artifactID, "# Replaced")
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
	if err := service.ReplaceText(WithTx(ctx, tx), artifactID, "# Rolled back", actor); err != nil {
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
	if got != "# First" {
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

func TestEvictDropsUnconnectedRoomAndCancelsSettlement(t *testing.T) {
	service, artifactID := newTestService(t)
	const settleInterval = 50 * time.Millisecond
	service.settle = settleInterval
	seedServiceText(t, service, artifactID, "before")
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin transactional edit: %v", err)
	}
	if err := service.ReplaceText(WithTx(ctx, tx), artifactID, "after", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("replace text before eviction: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll back document mutation: %v", err)
	}
	if err := service.Evict(ctx, artifactID); err != nil {
		t.Fatalf("evict unconnected document: %v", err)
	}
	waitForNoLiveDocument(t, service, artifactID)
	if got, err := service.Text(ctx, artifactID); err != nil || got != "before" {
		t.Fatalf("document after eviction = %q (%v), want persisted text before", got, err)
	}
	time.Sleep(3 * settleInterval)
	var versions int
	if err := service.store.Pool.QueryRow(context.Background(), `
		select count(*) from artifact_versions where artifact_id = $1
	`, artifactID).Scan(&versions); err != nil {
		t.Fatalf("count versions after eviction: %v", err)
	}
	if versions != 1 {
		t.Fatalf("versions after eviction = %d, want 1", versions)
	}
}

func TestApplyOpsEditsLiveDocumentAndSettlesVersion(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "one two one")
	actor := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	first := 0
	applied, err := service.ApplyOps(context.Background(), artifactID, []model.EditOp{
		{Op: "replace", Find: "two", With: "TWO"},
		{Op: "insert", Markdown: "!", After: "end"},
		{Op: "delete", Find: "one", Occurrence: &first},
	}, actor)
	if err != nil {
		t.Fatalf("apply document operations: %v", err)
	}
	if applied != 3 {
		t.Fatalf("applied operations = %d, want 3", applied)
	}
	waitForDocumentText(t, service, artifactID, " TWO one!")
	if version := waitForDocumentVersion(t, service.store, artifactID, 2); len(version.Authors) != 1 || version.Authors[0] != actor {
		t.Fatalf("settled version authors = %#v, want %v", version.Authors, actor)
	}
}

func TestApplyOpsRejectsAmbiguousTargetWithoutChangingDocument(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "same same")
	_, err := service.ApplyOps(context.Background(), artifactID, []model.EditOp{{Op: "replace", Find: "same", With: "changed"}}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"})
	var ambiguous *text.ErrTargetAmbiguous
	if !errors.As(err, &ambiguous) {
		t.Fatalf("ambiguous edit error = %v, want ErrTargetAmbiguous", err)
	}
	if len(ambiguous.Candidates) != 2 {
		t.Fatalf("ambiguous candidates = %#v, want two candidates", ambiguous.Candidates)
	}
	waitForDocumentText(t, service, artifactID, "same same")
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
	waitForDocumentText(t, service, artifactID, "before")
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

func TestSettlePersistsPendingAuthorAfterDisconnect(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	actor := model.Actor{Kind: "user", ID: "alice"}
	connectionID := service.nextConnection.Add(1)
	service.addConnection(artifactID, connectionID, actor)

	if err := service.srv.Apply(context.Background(), artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		content := doc.GetText("content")
		transact(func(tx *crdt.Transaction) {
			content.Delete(tx, 0, content.Len())
			content.Insert(tx, 0, "after", nil)
		})
	}); err != nil {
		t.Fatalf("apply connected client update: %v", err)
	}
	service.removeConnection(artifactID, connectionID)

	version := waitForDocumentVersion(t, service.store, artifactID, 2)
	if len(version.Authors) != 1 || version.Authors[0] != actor {
		t.Fatalf("settled version authors = %#v, want %v", version.Authors, actor)
	}
}

func TestSettleWritesDirtyVersionWithoutPendingAuthors(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")

	if err := service.srv.Apply(context.Background(), artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		content := doc.GetText("content")
		transact(func(tx *crdt.Transaction) {
			content.Delete(tx, 0, content.Len())
			content.Insert(tx, 0, "after", nil)
		})
	}); err != nil {
		t.Fatalf("apply unattributed update: %v", err)
	}

	version := waitForDocumentVersion(t, service.store, artifactID, 2)
	if len(version.Authors) != 0 {
		t.Fatalf("settled version authors = %#v, want none", version.Authors)
	}
}

func TestConnectedActorIsPendingAfterEachDocumentUpdate(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	actor := model.Actor{Kind: "user", ID: "alice"}
	connectionID := service.nextConnection.Add(1)
	service.addConnection(artifactID, connectionID, actor)

	for number, markdown := range []string{"after first update", "after second update"} {
		if err := service.srv.Apply(context.Background(), artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
			content := doc.GetText("content")
			transact(func(tx *crdt.Transaction) {
				content.Delete(tx, 0, content.Len())
				content.Insert(tx, 0, markdown, nil)
			})
		}); err != nil {
			t.Fatalf("apply connected update %d: %v", number+1, err)
		}
		version := waitForDocumentVersion(t, service.store, artifactID, number+2)
		if len(version.Authors) != 1 || version.Authors[0] != actor {
			t.Fatalf("version %d authors = %#v, want %v", number+2, version.Authors, actor)
		}
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

	if err := service.ReplaceText(context.Background(), artifactID, "after", editor); err != nil {
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
	if err := service.ReplaceText(context.Background(), artifactID, "after", editor); err != nil {
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
	if err := service.ReplaceText(context.Background(), artifactID, "first", first); err != nil {
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
	if err := service.ReplaceText(context.Background(), artifactID, "second", second); err != nil {
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
	if err := service.ReplaceText(context.Background(), artifactID, "third", second); err != nil {
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
		editDone <- service.ReplaceText(context.Background(), artifactID, "after", editor)
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
	if markdown != "after" {
		t.Fatalf("snapshot markdown = %q, want post-edit text", markdown)
	}
	if len(version.Authors) != 2 || version.Authors[0] != editor || version.Authors[1] != snapshotter {
		t.Fatalf("snapshot authors = %#v, want editor and snapshotter", version.Authors)
	}
}

func TestApplyOpsResolvesAgainstDocumentInsideApply(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "# First")
	if err := service.ReplaceText(context.Background(), artifactID, "base", model.Actor{Kind: "user", ID: "alice"}); err != nil {
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
		_, err := service.ApplyOps(context.Background(), artifactID, []model.EditOp{{Op: "insert", Markdown: "!", After: "end"}}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"})
		result <- err
	}()
	<-entered
	if err := service.srv.Apply(context.Background(), artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		content := doc.GetText("content")
		transact(func(tx *crdt.Transaction) {
			content.Insert(tx, content.Len(), " browser", nil)
		})
	}); err != nil {
		t.Fatalf("apply concurrent browser update: %v", err)
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatalf("apply operation: %v", err)
	}
	waitForDocumentText(t, service, artifactID, "base browser!")
}

func TestApplyReplaceResolvesAgainstDocumentInsideApply(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "# First")
	if err := service.ReplaceText(context.Background(), artifactID, "base", model.Actor{Kind: "user", ID: "alice"}); err != nil {
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
	if err := service.srv.Apply(context.Background(), artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		content := doc.GetText("content")
		transact(func(tx *crdt.Transaction) {
			content.Insert(tx, content.Len(), " browser", nil)
		})
	}); err != nil {
		t.Fatalf("apply concurrent browser update: %v", err)
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatalf("apply replacement: %v", err)
	}
	waitForDocumentText(t, service, artifactID, "server browser")
}

func TestReplaceTextAcceptsUnchangedEmptyDocument(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "")
	if err := service.ReplaceText(context.Background(), artifactID, "", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("replace unchanged empty document: %v", err)
	}
}

func TestReresolveAnchorsClosesRowsBeforeUpdating(t *testing.T) {
	service, artifactID := newTestService(t)
	anchorJSON, err := json.Marshal(model.Anchor{ArtifactID: artifactID, Version: 1, Quote: "before", To: len("before")})
	if err != nil {
		t.Fatalf("encode anchor: %v", err)
	}
	tx, err := service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin anchor transaction: %v", err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(context.Background(), `
		insert into asks (issue_key, author, question, anchor)
		values ('DOC-1', '{"kind":"user","id":"alice"}', 'Question?', $1)
	`, anchorJSON); err != nil {
		t.Fatalf("create anchored ask: %v", err)
	}
	if err := service.reresolveAnchors(context.Background(), tx, artifactID, "after"); err != nil {
		t.Fatalf("reresolve anchors: %v", err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit anchor transaction: %v", err)
	}
}

func TestSupersededSettleGenerationDoesNotWrite(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")

	if err := service.srv.Apply(context.Background(), artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		content := doc.GetText("content")
		transact(func(tx *crdt.Transaction) {
			content.Delete(tx, 0, content.Len())
			content.Insert(tx, 0, "after", nil)
		})
	}); err != nil {
		t.Fatalf("apply first update: %v", err)
	}
	state := service.room(artifactID)
	state.mu.Lock()
	stale := state.gen
	state.mu.Unlock()
	service.scheduleSettle(artifactID)
	state.mu.Lock()
	current := state.gen
	state.mu.Unlock()

	service.settleRoom(artifactID, stale)
	var versions int
	if err := service.store.Pool.QueryRow(context.Background(), `select count(*) from artifact_versions where artifact_id = $1`, artifactID).Scan(&versions); err != nil {
		t.Fatalf("count versions after stale settle: %v", err)
	}
	if versions != 1 {
		t.Fatalf("versions after stale settle = %d, want 1", versions)
	}
	service.settleRoom(artifactID, current)
	waitForDocumentVersion(t, service.store, artifactID, 2)
}

func TestShutdownContextDoesNotWaitForBlockedSettlement(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = 5 * time.Millisecond
	seedServiceText(t, service, artifactID, "before")

	blocker, err := service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin blocker transaction: %v", err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(context.Background(), `select 1 from artifacts where id = $1 for update`, artifactID); err != nil {
		t.Fatalf("lock document artifact: %v", err)
	}
	if err := service.srv.Apply(context.Background(), artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		content := doc.GetText("content")
		transact(func(tx *crdt.Transaction) {
			content.Delete(tx, 0, content.Len())
			content.Insert(tx, 0, "after", nil)
		})
	}); err != nil {
		t.Fatalf("apply document update: %v", err)
	}
	locked := false
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := service.store.Pool.QueryRow(context.Background(), `
			select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock'
		`).Scan(&waiting); err != nil {
			t.Fatalf("inspect database locks: %v", err)
		}
		if waiting > 0 {
			locked = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !locked {
		t.Fatal("settlement did not wait on the document lock")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- service.Shutdown(ctx) }()
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("shutdown error = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown waited on the blocked settlement")
	}
}

func TestIssueCloseClosesOpenDocumentConnection(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	headers := http.Header{"X-Dispatch-User": []string{"alice"}}
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID
	connection, response, err := gws.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("connect live document: response=%#v err=%v", response, err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	if _, err := service.store.Pool.Exec(context.Background(), `update issues set closed_at = now() where key = 'DOC-1'`); err != nil {
		t.Fatalf("close document issue: %v", err)
	}
	service.SetIssueClosed("DOC-1", true)
	waitForRoomClosed(t, service, artifactID)
	waitForNoLiveDocument(t, service, artifactID)
	connection.SetReadDeadline(time.Now().Add(time.Second))
	for {
		if _, _, err := connection.ReadMessage(); err != nil {
			break
		}
	}
	if err := service.srv.Apply(context.Background(), artifactID, func(_ *crdt.Doc, _ func(func(*crdt.Transaction))) {}); !errors.Is(err, ErrIssueClosed) {
		t.Fatalf("server write after issue close = %v, want ErrIssueClosed", err)
	}
}

func TestIssueReopenRestoresLiveWrites(t *testing.T) {
	service, artifactID := newTestService(t)
	if got := service.events.SubscriberCount(); got != 0 {
		t.Fatalf("document service registered %d event subscriptions, want none", got)
	}
	seedServiceText(t, service, artifactID, "before")
	if _, err := service.store.Pool.Exec(context.Background(), `update issues set closed_at = now() where key = 'DOC-1'`); err != nil {
		t.Fatalf("close document issue: %v", err)
	}
	service.SetIssueClosed("DOC-1", true)
	if err := service.ReplaceText(context.Background(), artifactID, "closed", model.Actor{Kind: "user", ID: "alice"}); !errors.Is(err, ErrIssueClosed) {
		t.Fatalf("write to closed issue = %v, want ErrIssueClosed", err)
	}
	if _, err := service.store.Pool.Exec(context.Background(), `update issues set closed_at = null where key = 'DOC-1'`); err != nil {
		t.Fatalf("reopen document issue: %v", err)
	}
	service.SetIssueClosed("DOC-1", false)
	service.events.Publish(model.Event{IssueKey: "DOC-1", Type: "issue.closed"})
	if got := service.events.SubscriberCount(); got != 0 {
		t.Fatalf("stale issue.closed event gained %d subscriptions, want none", got)
	}
	if err := service.ReplaceText(context.Background(), artifactID, "reopened", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("write to reopened issue: %v", err)
	}
	waitForDocumentText(t, service, artifactID, "reopened")
}

func TestClosedColdRoomAuthorizesReadOnly(t *testing.T) {
	service, artifactID := newTestService(t)
	if _, err := service.store.Pool.Exec(context.Background(), `update issues set closed_at = now() where key = 'DOC-1'`); err != nil {
		t.Fatalf("close document issue: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/ws/doc/"+artifactID, nil)
	request.Header.Set("X-Dispatch-User", "alice")
	request.SetPathValue("room", artifactID)
	request = request.WithContext(context.WithValue(request.Context(), connectionContextKey{}, &connectionState{}))
	config, ok := service.authorize(request)
	if !ok || !config.ReadOnly {
		t.Fatalf("cold closed room authorization = %#v, %t; want read-only acceptance", config, ok)
	}
}

func TestLoadFailureMakesDocumentServiceUnavailable(t *testing.T) {
	database := openTestStore(t)
	artifactID := createDocument(t, database, "before")
	service := New(Deps{
		Store:       database,
		Persistence: failingVersionedStore{VersionedStore: NewPgVersioned(database), loadErr: errors.New("load failed")},
		Events:      events.NewBroker(),
		Identity:    identity.HeaderIdentity{Header: "X-Dispatch-User", AllowedLogins: map[string]struct{}{"alice": {}}},
	})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })

	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	headers := http.Header{"X-Dispatch-User": []string{"alice"}}
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID
	connection, response, err := gws.DefaultDialer.Dial(wsURL, headers)
	if connection != nil {
		_ = connection.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("failed-room connection: response=%#v err=%v, want HTTP 503", response, err)
	}
	if _, err := service.Text(context.Background(), artifactID); !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("load failure = %v, want ErrServiceUnavailable", err)
	}
}

func TestCorruptLoadMakesDocumentServiceUnavailable(t *testing.T) {
	database := openTestStore(t)
	artifactID := createDocument(t, database, "before")
	service := New(Deps{
		Store:       database,
		Persistence: failingVersionedStore{VersionedStore: NewPgVersioned(database), loadUpdate: []byte{0xff}},
		Events:      events.NewBroker(),
		Identity:    identity.HeaderIdentity{Header: "X-Dispatch-User", AllowedLogins: map[string]struct{}{"alice": {}}},
	})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })

	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	headers := http.Header{"X-Dispatch-User": []string{"alice"}}
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID
	connection, response, err := gws.DefaultDialer.Dial(wsURL, headers)
	if connection != nil {
		_ = connection.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("corrupt-room connection: response=%#v err=%v, want HTTP 503", response, err)
	}
	if _, err := service.Text(context.Background(), artifactID); !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("corrupt load = %v, want ErrServiceUnavailable", err)
	}
}

func TestCancelledTextLoadDoesNotQuarantineRoom(t *testing.T) {
	database := openTestStore(t)
	artifactID := createDocument(t, database, "before")
	service := New(Deps{
		Store:       database,
		Persistence: failingVersionedStore{VersionedStore: NewPgVersioned(database), respectContext: true},
		Events:      events.NewBroker(),
	})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.Text(ctx, artifactID); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled text load = %v, want context.Canceled", err)
	}
	if _, err := service.Text(context.Background(), artifactID); err != nil {
		t.Fatalf("text after cancelled load = %v, want room to remain available", err)
	}
}

func TestAppendFailureClosesDocumentConnectionAndReloadsRoom(t *testing.T) {
	database := openTestStore(t)
	artifactID := createDocument(t, database, "before")
	service := New(Deps{
		Store:       database,
		Persistence: failingVersionedStore{VersionedStore: NewPgVersioned(database), appendErr: errors.New("append failed")},
		Events:      events.NewBroker(),
		Identity:    identity.HeaderIdentity{Header: "X-Dispatch-User", AllowedLogins: map[string]struct{}{"alice": {}}},
		Settle:      time.Hour,
	})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	seedServiceText(t, service, artifactID, "before")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	headers := http.Header{"X-Dispatch-User": []string{"alice"}}
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID
	connection, response, err := gws.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("connect live document: response=%#v err=%v", response, err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	if err := service.ReplaceText(context.Background(), artifactID, "after", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("replace text before persistence failure: %v", err)
	}
	connection.SetReadDeadline(time.Now().Add(time.Second))
	for {
		if _, _, err := connection.ReadMessage(); err != nil {
			break
		}
	}
	_ = connection.Close()
	waitForNoLiveDocument(t, service, artifactID)
	if got, err := service.Text(context.Background(), artifactID); err != nil || got != "before" {
		t.Fatalf("reloaded text after append failure = %q (%v), want persisted text before", got, err)
	}
}

func TestFailedRoomEvictsAndReloadsOnNextAccess(t *testing.T) {
	database := openTestStore(t)
	artifactID := createDocument(t, database, "before")
	service := New(Deps{
		Store:       database,
		Persistence: &failingOnceVersionedStore{VersionedStore: NewPgVersioned(database)},
		Events:      events.NewBroker(),
		Identity:    identity.HeaderIdentity{Header: "X-Dispatch-User", AllowedLogins: map[string]struct{}{"alice": {}}},
	})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	seedServiceText(t, service, artifactID, "before")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	headers := http.Header{"X-Dispatch-User": []string{"alice"}}
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID

	connection, response, err := gws.DefaultDialer.Dial(wsURL, headers)
	if connection != nil {
		_ = connection.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("first failed-room access: response=%#v err=%v, want HTTP 503", response, err)
	}
	connection, response, err = gws.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("reloaded room access: response=%#v err=%v", response, err)
	}
	_ = connection.Close()
}

func TestNamedVersionIndexesDocumentReferences(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	if err := service.ReplaceText(context.Background(), artifactID, "See dispatch://DOC-1/artifact/spec.", model.Actor{Kind: "user", ID: "alice"}); err != nil {
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

func TestSettleCapturesAuthorsAtSnapshotTime(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	first := model.Actor{Kind: "user", ID: "alice"}
	second := model.Actor{Kind: "user", ID: "bob"}
	if err := service.ReplaceText(context.Background(), artifactID, "after", first); err != nil {
		t.Fatalf("replace document text: %v", err)
	}
	state := service.room(artifactID)
	state.mu.Lock()
	generation := state.gen
	state.mu.Unlock()
	blocker, err := service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin blocker transaction: %v", err)
	}
	if _, err := blocker.Exec(context.Background(), `select 1 from artifacts where id = $1 for update`, artifactID); err != nil {
		t.Fatalf("lock document artifact: %v", err)
	}
	settled := make(chan struct{})
	go func() {
		service.settleRoom(artifactID, generation)
		close(settled)
	}()
	waitForDatabaseLock(t, service.store)
	service.recordActor(artifactID, second)
	if err := blocker.Commit(context.Background()); err != nil {
		t.Fatalf("release document lock: %v", err)
	}
	select {
	case <-settled:
	case <-time.After(time.Second):
		t.Fatal("settlement did not complete")
	}
	version := waitForDocumentVersion(t, service.store, artifactID, 2)
	if len(version.Authors) != 2 || version.Authors[0] != first || version.Authors[1] != second {
		t.Fatalf("settled version authors = %#v, want %v and %v", version.Authors, first, second)
	}
}

func waitForDatabaseLock(t *testing.T, database *store.Store) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := database.Pool.QueryRow(context.Background(), `
			select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock'
		`).Scan(&waiting); err != nil {
			t.Fatalf("inspect database locks: %v", err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("settlement did not wait on the document lock")
}

type failingOnceVersionedStore struct {
	VersionedStore
	loads atomic.Int32
}

func (s *failingOnceVersionedStore) Load(ctx context.Context, room string) (persistence.LoadResult, error) {
	if s.loads.Add(1) == 1 {
		return persistence.LoadResult{}, errors.New("transient load failure")
	}
	return s.VersionedStore.Load(ctx, room)
}

func TestWebsocketRejectsUnauthenticatedConnection(t *testing.T) {
	service, _ := newTestService(t)
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/room"
	connection, response, err := gws.DefaultDialer.Dial(wsURL, nil)
	if connection != nil {
		_ = connection.Close()
	}
	if err == nil {
		t.Fatal("unauthenticated websocket connection succeeded")
	}
	if response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated websocket response = %#v, want HTTP 401", response)
	}
}

type failingVersionedStore struct {
	VersionedStore
	loadErr        error
	loadUpdate     []byte
	appendErr      error
	respectContext bool
}

func (s failingVersionedStore) Load(ctx context.Context, room string) (persistence.LoadResult, error) {
	if s.respectContext && ctx.Err() != nil {
		return persistence.LoadResult{}, ctx.Err()
	}
	if s.loadErr != nil {
		return persistence.LoadResult{}, s.loadErr
	}
	if s.loadUpdate != nil {
		return persistence.LoadResult{Update: s.loadUpdate, Version: 1}, nil
	}
	return s.VersionedStore.Load(ctx, room)
}

func (s failingVersionedStore) AppendUpdate(ctx context.Context, room string, update []byte) (persistence.Version, error) {
	if s.appendErr != nil {
		return 0, s.appendErr
	}
	return s.VersionedStore.AppendUpdate(ctx, room, update)
}

func seedServiceText(t *testing.T, service *Service, artifactID, markdown string) {
	t.Helper()
	tx, err := service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin seed text: %v", err)
	}
	defer tx.Rollback(context.Background())
	if err := service.SeedText(context.Background(), tx, artifactID, markdown); err != nil {
		t.Fatalf("seed service text: %v", err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit seed text: %v", err)
	}
}
func waitForRoomClosed(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if service.roomClosed(artifactID) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("document room did not close after issue event")
}
func waitForRoomFailure(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if service.roomFailure(artifactID) != nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("document room did not become unavailable after append failure")
}

func waitForNoLiveDocument(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if service.srv.GetDoc(artifactID) == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("document room remained resident after closure")
}
func waitForDocumentText(t *testing.T, service *Service, artifactID, want string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		got, err := service.Text(context.Background(), artifactID)
		if err == nil && got == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	got, err := service.Text(context.Background(), artifactID)
	t.Fatalf("document text = %q (%v), want %q", got, err, want)
}

func waitForDocumentVersion(t *testing.T, database *store.Store, artifactID string, number int) model.Version {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		version, ok, err := loadDocumentVersion(context.Background(), database, artifactID, number)
		if err != nil {
			t.Fatalf("load document version: %v", err)
		}
		if ok {
			return version
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("document version did not settle")
	return model.Version{}
}

func loadDocumentVersion(ctx context.Context, database *store.Store, artifactID string, number int) (model.Version, bool, error) {
	var version model.Version
	var authors []byte
	err := database.Pool.QueryRow(ctx, `
		select number, named, summary, authors, created_at
		from artifact_versions where artifact_id = $1 and number = $2
	`, artifactID, number).Scan(&version.Number, &version.Named, &version.Summary, &authors, &version.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Version{}, false, nil
	}
	if err != nil {
		return model.Version{}, false, err
	}
	if err := json.Unmarshal(authors, &version.Authors); err != nil {
		return model.Version{}, false, err
	}
	return version, true, nil
}
