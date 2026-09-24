package docs

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A settlement already past its entry check when a transaction opens its write can reach the
// room after the transaction commits and before its write is published. The room then lacks
// the committed write while the durable cursor includes it, so a version written then would
// stand without the write. It must settle again after the publish instead.
func TestSettlementBetweenCommitAndPublishWaitsForTheWrite(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	paused := make(chan struct{})
	resume := make(chan struct{})
	var pausedOnce atomic.Bool
	service.afterSettleWarm = func(room string) {
		if room == artifactID && pausedOnce.CompareAndSwap(false, true) {
			close(paused)
			<-resume
		}
	}
	state := service.room(artifactID)
	state.mu.Lock()
	generation := state.gen
	state.mu.Unlock()
	settled := make(chan struct{})
	go func() {
		defer close(settled)
		service.settleRoom(artifactID, generation)
	}()
	t.Cleanup(func() {
		select {
		case <-resume:
		default:
			close(resume)
		}
		<-settled
	})
	select {
	case <-paused:
	case <-time.After(5 * time.Second):
		t.Fatal("settlement never reached its warm hook")
	}

	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin edit transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, _, err := lockArtifactOwner(ctx, tx, artifactID); err != nil {
		t.Fatalf("lock document owner: %v", err)
	}
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	alice := model.Actor{Kind: "user", ID: "alice"}
	if _, err := service.ApplyOps(joined, artifactID, []model.EditOp{{Op: "replace", Find: "before", With: "after"}}, alice, nil); err != nil {
		t.Fatalf("apply joined edit: %v", err)
	}
	if _, err := service.SnapshotVersion(joined, tx, artifactID, alice); err != nil {
		t.Fatalf("snapshot joined edit: %v", err)
	}
	// A browser types while the edit's transaction is open.
	editLiveTree(t, service, artifactID, appendBlocks(t, "typed"))
	if err := ledger.commit(ctx); err != nil {
		t.Fatalf("commit edit transaction: %v", err)
	}
	close(resume)
	select {
	case <-settled:
	case <-time.After(10 * time.Second):
		t.Fatal("paused settlement did not finish")
	}
	ledger.publish()
	settleCurrentGeneration(t, service, artifactID)

	var markdown string
	if err := service.store.Pool.QueryRow(ctx, `
		select markdown from artifact_versions where artifact_id = $1 order by number desc limit 1
	`, artifactID).Scan(&markdown); err != nil {
		t.Fatalf("read latest version: %v", err)
	}
	if markdown != "after\n\ntyped\n" {
		t.Fatalf("latest version = %q, want the committed edit and the browser's paragraph", markdown)
	}
}

// A commit that returns an error may still have committed. Failing the room instead of
// discarding its write makes the room reload whatever the durable document holds.
func TestFailedLiveWritesReloadTheDurableDocument(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin edit transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	if _, err := service.ApplyOps(joined, artifactID, []model.EditOp{{Op: "replace", Find: "before", With: "after"}}, model.Actor{Kind: "user", ID: "alice"}, nil); err != nil {
		t.Fatalf("apply joined edit: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit edit transaction: %v", err)
	}
	ledger.fail(errors.New("commit outcome unknown"))
	if got, err := service.Text(ctx, artifactID); err != nil || got != "after\n" {
		t.Fatalf("document after failing its live write = %q (%v), want the committed text", got, err)
	}
}

// A transaction that does not commit credits no one: the next settled version names only the
// actor whose write reached the document, and so does the version's event.
func TestARolledBackWriteIsNoAuthorOfTheNextVersion(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin edit transaction: %v", err)
	}
	joined, ledger := service.Join(ctx, tx)
	rolledBack := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	if _, err := service.ReplaceText(joined, artifactID, "rolled back", rolledBack); err != nil {
		t.Fatalf("replace text in the transaction: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll back edit transaction: %v", err)
	}
	ledger.Discard()

	alice := model.Actor{Kind: "user", ID: "alice"}
	if _, err := service.ReplaceText(ctx, artifactID, "after", alice); err != nil {
		t.Fatalf("replace text: %v", err)
	}
	version := waitForDocumentVersion(t, service.store, artifactID, 2)
	if !reflect.DeepEqual(version.Authors, []model.Actor{alice}) {
		t.Fatalf("version 2 authors = %#v, want only %v", version.Authors, alice)
	}
	var eventActor model.Actor
	if err := service.store.Pool.QueryRow(ctx, `
		select actor from events where issue_key = 'DOC-1' and type = 'artifact.version' order by seq desc limit 1
	`).Scan(&eventActor); err != nil {
		t.Fatalf("read version event: %v", err)
	}
	if eventActor != alice {
		t.Fatalf("version event actor = %v, want %v", eventActor, alice)
	}
}

// A browser connected while a transaction changes its document made none of the change: the
// version the transaction writes names only its writer, as a write that reaches the room
// directly does, and a version settled after the browser left names only its own editor.
func TestAJoinedWritesVersionNamesItsWriterNotAConnectedBrowser(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	browser := model.Actor{Kind: "user", ID: "alice"}
	connectionID := service.nextConnection.Add(1)
	service.addConnection(artifactID, connectionID, browser)

	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin edit transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, _, err := lockArtifactOwner(ctx, tx, artifactID); err != nil {
		t.Fatalf("lock document owner: %v", err)
	}
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	writer := model.Actor{Kind: "user", ID: "bob"}
	if _, err := service.ApplyOps(joined, artifactID, []model.EditOp{{Op: "replace", Find: "before", With: "during"}}, writer, nil); err != nil {
		t.Fatalf("apply joined edit: %v", err)
	}
	snapshot, err := service.SnapshotVersion(joined, tx, artifactID, writer)
	if err != nil {
		t.Fatalf("snapshot joined edit: %v", err)
	}
	if err := ledger.commit(ctx); err != nil {
		t.Fatalf("commit edit transaction: %v", err)
	}
	ledger.publish()
	if want := []model.Actor{writer}; !reflect.DeepEqual(snapshot.Version.Authors, want) {
		t.Fatalf("joined edit's version authors = %#v, want only the writer %v (browser %v only watched)", snapshot.Version.Authors, want, browser)
	}

	service.removeConnection(artifactID, connectionID)
	later := model.Actor{Kind: "user", ID: "carol"}
	if _, err := service.ReplaceText(ctx, artifactID, "after", later); err != nil {
		t.Fatalf("replace text: %v", err)
	}
	settleCurrentGeneration(t, service, artifactID)
	version := waitForDocumentVersion(t, service.store, artifactID, 3)
	if !reflect.DeepEqual(version.Authors, []model.Actor{later}) {
		t.Fatalf("version 3 authors = %#v, want only %v", version.Authors, later)
	}
}

// failingBrowserAppendStore fails every browser update's durable append while failing is set,
// holding the first such append until release closes, and runs beforeTx once, just before the
// next transactional append and so before that append takes the document's advisory lock.
type failingBrowserAppendStore struct {
	VersionedStore
	failing  atomic.Bool
	held     atomic.Bool
	entered  chan struct{}
	release  chan struct{}
	beforeTx atomic.Pointer[func()]
}

func (s *failingBrowserAppendStore) fail() error {
	if !s.failing.Load() {
		return nil
	}
	if s.held.CompareAndSwap(false, true) {
		close(s.entered)
		<-s.release
	}
	return errors.New("injected browser append failure")
}

func (s *failingBrowserAppendStore) AppendUpdate(ctx context.Context, room string, update []byte) (persistence.Version, error) {
	if err := s.fail(); err != nil {
		return 0, err
	}
	return s.VersionedStore.AppendUpdate(ctx, room, update)
}

func (s *failingBrowserAppendStore) AppendUpdateWithClass(ctx context.Context, room string, update []byte, contentChanged bool) (persistence.Version, error) {
	if err := s.fail(); err != nil {
		return 0, err
	}
	return s.VersionedStore.(classifiedUpdateStore).AppendUpdateWithClass(ctx, room, update, contentChanged)
}

func (s *failingBrowserAppendStore) AppendUpdateTx(ctx context.Context, tx pgx.Tx, room string, update []byte, contentChanged bool) (persistence.Version, error) {
	if hook := s.beforeTx.Swap(nil); hook != nil {
		(*hook)()
	}
	return s.VersionedStore.AppendUpdateTx(ctx, tx, room, update, contentChanged)
}

// A joined write's first operation forks the room while the room holds a browser paragraph,
// "typed", that is not durable yet. That paragraph's append then fails, and the failed room is
// evicted and reloaded without it before the write appends its own update. The write's fork
// still holds the paragraph and its slot is on the failed room, so the write fails at its append
// rather than versioning or publishing a document the room never held, and its transaction rolls
// back to the durable document.
func TestAWriteWhoseRoomReloadsBeforeItsFirstAppendFailsFast(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	store := &failingBrowserAppendStore{
		VersionedStore: NewPgVersioned(database),
		entered:        make(chan struct{}),
		release:        make(chan struct{}),
	}
	service := New(Deps{Store: database, Persistence: store, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	seedServiceText(t, service, artifactID, "before")
	liveTree(t, service, artifactID)

	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin transactional edit: %v", err)
	}
	defer tx.Rollback(ctx)
	joinedCtx, ledger := service.Join(ctx, tx)
	defer ledger.Discard()

	store.failing.Store(true)
	editLiveTree(t, service, artifactID, appendBlocks(t, "typed"))
	<-store.entered
	reload := func() {
		close(store.release)
		waitForRoomFailure(t, service, artifactID)
		if err := service.awaitRoomRecovery(ctx, artifactID); err != nil {
			t.Errorf("await room recovery: %v", err)
		}
		store.failing.Store(false)
	}
	store.beforeTx.Store(&reload)
	failsFast(t, "a joined write whose room reloaded before its first append", func() error {
		_, err := service.ApplyOps(joinedCtx, artifactID, []model.EditOp{{Op: "replace", Find: "before", With: "after"}}, model.Actor{Kind: "user", ID: "alice"}, nil)
		return err
	})
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll back: %v", err)
	}
	ledger.Discard()
	requireText(t, service, artifactID, "before\n")
	var latest int
	if err := database.Pool.QueryRow(ctx, `select max(number) from artifact_versions where artifact_id = $1`, artifactID).Scan(&latest); err != nil {
		t.Fatalf("read latest version: %v", err)
	}
	if latest != 1 {
		t.Fatalf("latest version = %d, want the seeded version only", latest)
	}
}
