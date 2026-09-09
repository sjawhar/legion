package docs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"

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

func TestWebsocketRejectsUnauthenticatedConnection(t *testing.T) {
	service, _ := newTestService(t)
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/room"
	connection, response, err := websocket.DefaultDialer.Dial(wsURL, nil)
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
