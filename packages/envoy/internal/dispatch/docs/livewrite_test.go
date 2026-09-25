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
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
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

// A browser connected when a transaction changes its document is credited on the version the
// transaction writes, as it is when a write reaches the room directly, and not again on a
// version settled after it left.
func TestAJoinedWriteCreditsConnectedBrowsersOnItsOwnVersionOnly(t *testing.T) {
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
	if want := []model.Actor{browser, writer}; !reflect.DeepEqual(snapshot.Version.Authors, want) {
		t.Fatalf("joined edit's version authors = %#v, want %v", snapshot.Version.Authors, want)
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

// A write that starts while its room is failing waits for the room to recover and opens on the
// recovered room, whose settlement it then holds off. A browser types while the write is open;
// a settlement between the write's commit and its publish must not version that typing past the
// committed write, which the room does not hold yet.
func TestAWriteOpenedDuringRoomRecoveryHoldsOffTheRecoveredRoom(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	ctx := context.Background()
	if err := service.warmLiveDocument(ctx, artifactID); err != nil {
		t.Fatalf("load live document: %v", err)
	}
	// An outside transaction holds the document's advisory lock, so the failed room's eviction,
	// which compacts the document, cannot finish until the test releases it.
	holder, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock holder: %v", err)
	}
	defer holder.Rollback(ctx)
	if err := lockDocumentRoom(ctx, holder, artifactID); err != nil {
		t.Fatalf("hold document lock: %v", err)
	}
	service.failRoom(artifactID, errors.New("injected room failure"))

	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin edit transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	edited := make(chan error, 1)
	go func() {
		_, err := service.ReplaceText(joined, artifactID, "after", model.Actor{Kind: "user", ID: "alice"})
		edited <- err
	}()
	// Give the edit time to reach the failed room before its eviction can finish.
	time.Sleep(100 * time.Millisecond)
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release document lock: %v", err)
	}
	select {
	case err := <-edited:
		if err != nil {
			t.Fatalf("replace text in the transaction: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("joined edit did not finish once the room could recover")
	}
	editLiveTree(t, service, artifactID, appendBlocks(t, "typed"))
	if err := ledger.commit(ctx); err != nil {
		t.Fatalf("commit edit transaction: %v", err)
	}
	settleCurrentGeneration(t, service, artifactID)
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

// forkBeforeRoomReload opens a joined write whose first operation forks the room while the room
// holds a browser paragraph, "typed", that is not durable yet. That paragraph's append then
// fails, and the failed room is evicted and reloaded without it before the write appends its
// own update. It returns with the write's transaction still open.
func forkBeforeRoomReload(t *testing.T) (*Service, string, pgx.Tx, context.Context, *Ledger) {
	t.Helper()
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
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	joinedCtx, ledger := service.Join(ctx, tx)
	t.Cleanup(ledger.Discard)

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
	alice := model.Actor{Kind: "user", ID: "alice"}
	if _, err := service.ApplyOps(joinedCtx, artifactID, []model.EditOp{{Op: "replace", Find: "before", With: "after"}}, alice, nil); err != nil {
		t.Fatalf("joined edit: %v", err)
	}
	return service, artifactID, tx, joinedCtx, ledger
}

// A write keeps its fork of the room between operations. When the room reloads underneath it,
// the fork still holds what the failed room had and the reloaded one dropped, so the write must
// fork again: a version it snapshots is the document its commit publishes.
func TestAWriteWhoseRoomReloadsSnapshotsTheReloadedDocument(t *testing.T) {
	service, artifactID, tx, joinedCtx, ledger := forkBeforeRoomReload(t)
	ctx := context.Background()
	snapshot, err := service.SnapshotVersion(joinedCtx, tx, artifactID, model.Actor{Kind: "user", ID: "alice"})
	if err != nil {
		t.Fatalf("snapshot version: %v", err)
	}
	var versioned string
	if err := tx.QueryRow(ctx, `select markdown from artifact_versions where artifact_id = $1 and number = $2`, artifactID, snapshot.Version.Number).Scan(&versioned); err != nil {
		t.Fatalf("read snapshot version: %v", err)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit transactional edit: %v", err)
	}
	published, err := service.Text(ctx, artifactID)
	if err != nil {
		t.Fatalf("read published document: %v", err)
	}
	if published != "after\n" || versioned != published {
		t.Fatalf("version %d = %q, published document = %q, want both %q", snapshot.Version.Number, versioned, published, "after\n")
	}
}

// The paragraph the reloaded room dropped is gone for the write too: an edit that quotes it is
// refused rather than reported applied and lost at the publish.
func TestAWriteWhoseRoomReloadsCannotEditWhatTheReloadDropped(t *testing.T) {
	service, artifactID, _, joinedCtx, _ := forkBeforeRoomReload(t)
	applied, err := service.ApplyOps(joinedCtx, artifactID, []model.EditOp{{Op: "replace", Find: "typed", With: "TYPED"}}, model.Actor{Kind: "user", ID: "alice"}, nil)
	if !errors.Is(err, pmdoc.ErrTargetNotFound) {
		t.Fatalf("edit of the dropped paragraph = %d applied, %v; want %v", applied, err, pmdoc.ErrTargetNotFound)
	}
}
