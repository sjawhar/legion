package docs

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func newTestService(t *testing.T) (*Service, string) {
	t.Helper()
	database := openTestStore(t)
	artifactID := createDocument(t, database, "# First")
	service := New(Deps{
		Store:     database,
		Events:    events.NewBroker(),
		Identity:  identity.HeaderIdentity{Header: "X-Dispatch-User", AllowedLogins: map[string]struct{}{"alice": {}}},
		ServerURL: "https://dispatch.example",
		Settle:    20 * time.Millisecond,
	})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	return service, artifactID
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

func TestSettleRetriesTransientVersionWriteFailure(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = 10 * time.Millisecond
	seedServiceText(t, service, artifactID, "before")
	if _, err := service.store.Pool.Exec(context.Background(), `create sequence dispatch_test_settle_failure`); err != nil {
		t.Fatalf("create settlement failure sequence: %v", err)
	}
	if _, err := service.store.Pool.Exec(context.Background(), `
		create function dispatch_test_fail_first_settlement() returns trigger language plpgsql as $$
		begin
			if new.number = 2 and nextval('dispatch_test_settle_failure') = 1 then
				raise exception 'transient settlement write failure';
			end if;
			return new;
		end;
		$$
	`); err != nil {
		t.Fatalf("create settlement failure function: %v", err)
	}
	if _, err := service.store.Pool.Exec(context.Background(), `
		create trigger dispatch_test_fail_first_settlement
		before insert on artifact_versions for each row
		execute function dispatch_test_fail_first_settlement()
	`); err != nil {
		t.Fatalf("create settlement failure trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = service.store.Pool.Exec(context.Background(), `drop trigger if exists dispatch_test_fail_first_settlement on artifact_versions`)
		_, _ = service.store.Pool.Exec(context.Background(), `drop function if exists dispatch_test_fail_first_settlement()`)
		_, _ = service.store.Pool.Exec(context.Background(), `drop sequence if exists dispatch_test_settle_failure`)
	})

	if err := service.ReplaceText(context.Background(), artifactID, "after", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("change document before transient settle failure: %v", err)
	}
	time.Sleep(20 * service.settle)
	var versions int
	if err := service.store.Pool.QueryRow(context.Background(), `select count(*) from artifact_versions where artifact_id = $1`, artifactID).Scan(&versions); err != nil {
		t.Fatalf("count versions after transient settlement failure: %v", err)
	}
	if versions != 2 {
		t.Fatalf("versions after transient settlement failure = %d, want 2 after retry", versions)
	}
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
	if _, err := blocker.Exec(context.Background(), `
		select 1 from issues where key = (select issue_key from artifacts where id = $1) for update
	`, artifactID); err != nil {
		t.Fatalf("lock document issue: %v", err)
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
	if _, err := blocker.Exec(context.Background(), `
		select 1 from issues where key = (select issue_key from artifacts where id = $1) for update
	`, artifactID); err != nil {
		t.Fatalf("lock document issue: %v", err)
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
