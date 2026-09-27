package docs

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A failed room's eviction flushes and compacts the document under its advisory lock. An
// operation inside a database transaction that waited for that eviction could be waiting on its
// own transaction, which Postgres cannot see, so these tests bound every such wait.
const recoveryBound = 10 * time.Second

// failsFast runs op and requires ErrServiceUnavailable within the bound.
func failsFast(t *testing.T, what string, op func() error) {
	t.Helper()
	returned := make(chan error, 1)
	go func() { returned <- op() }()
	select {
	case err := <-returned:
		if !errors.Is(err, ErrServiceUnavailable) {
			t.Fatalf("%s on a failed room = %v, want ErrServiceUnavailable", what, err)
		}
	case <-time.After(recoveryBound):
		t.Fatalf("%s waited for the failed room's recovery, which waits on a lock its transaction holds", what)
	}
}

// awaitRecovered waits, outside any transaction, for the failed room's eviction to finish.
func awaitRecovered(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), recoveryBound)
	defer cancel()
	if err := service.awaitRoomRecovery(ctx, artifactID); err != nil {
		t.Fatalf("the failed room did not recover: %v", err)
	}
}

func requireText(t *testing.T, service *Service, artifactID, want string) {
	t.Helper()
	if got, err := service.Text(context.Background(), artifactID); err != nil || got != want {
		t.Fatalf("document = %q (%v), want %q", got, err, want)
	}
}

// failRoomDuringLoadStore fails a room from inside its load: the durable read ygo makes just
// before it calls OnLoadDocument, with the room's ready barrier still open. It stands for any
// failure that lands in that window - a committed write's publish, a browser update's append,
// a commit whose outcome is unknown - which a test cannot time by hand. It fails the first load
// that runs after it is armed, so a test can choose which load meets the failure.
type failRoomDuringLoadStore struct {
	VersionedStore
	service atomic.Pointer[Service]
	room    string
	armed   atomic.Bool
	failed  chan struct{}
	once    sync.Once
}

func (s *failRoomDuringLoadStore) Load(ctx context.Context, room string) (persistence.LoadResult, error) {
	result, err := s.VersionedStore.Load(ctx, room)
	if err != nil || room != s.room || !s.armed.Load() {
		return result, err
	}
	s.once.Do(func() {
		s.service.Load().failRoom(room, errors.New("injected room failure"))
		close(s.failed)
	})
	return result, nil
}

// A room that fails while it is still loading recovers. The failure's eviction waits in ygo's
// CloseRoom for the load's ready barrier and closes the recovery's channel only afterwards, so
// a load that waited for that recovery held the eviction that would end its wait: the room
// stayed failed, and every later write to the document answered 503 until the server was
// restarted (LEGION-282).
func TestARoomThatFailsWhileItIsLoadingRecovers(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "")
	persist := &failRoomDuringLoadStore{
		VersionedStore: NewPgVersioned(database),
		room:           artifactID,
		failed:         make(chan struct{}),
	}
	service := New(Deps{Store: database, Persistence: persist, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), recoveryBound)
		defer cancel()
		_ = service.Shutdown(stop)
	})
	persist.service.Store(service)
	seedServiceText(t, service, artifactID, "before")
	persist.armed.Store(true)

	// The load runs on context.Background(), as the settlement warm-up and a committed write's
	// publish do, so nothing but the fix ends it.
	loaded := make(chan error, 1)
	go func() { loaded <- service.warmLiveDocument(context.Background(), artifactID) }()
	<-persist.failed
	select {
	case err := <-loaded:
		if !errors.Is(err, ErrServiceUnavailable) {
			t.Fatalf("a load that meets its own room's failure = %v, want ErrServiceUnavailable", err)
		}
	case <-time.After(recoveryBound):
		t.Fatal("the load never returned: it waited for the recovery that waits for it")
	}
	awaitRecovered(t, service, artifactID)

	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin write transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	if _, err := service.ReplaceText(joined, artifactID, "after", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("joined write after the failed load recovered: %v", err)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the write: %v", err)
	}
	requireText(t, service, artifactID, "after\n")
}

// A committed write's publish that its room refuses must not fail the room it finds by name. The
// refusal says the room had already failed, and by the time it is handled that failure's
// recovery can have finished and registered a replacement - which holds this write, whose append
// committed. Failing the replacement refused every write to the document until it too recovered.
func TestAPublishRefusedByAFailedRoomLeavesTheReplacementAlone(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "")
	persist := &failRoomDuringLoadStore{
		VersionedStore: NewPgVersioned(database),
		room:           artifactID,
		failed:         make(chan struct{}),
	}
	service := New(Deps{Store: database, Persistence: persist, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), recoveryBound)
		defer cancel()
		_ = service.Shutdown(stop)
	})
	persist.service.Store(service)
	seedServiceText(t, service, artifactID, "before")
	alice := model.Actor{Kind: "user", ID: "alice"}

	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin write transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	if _, err := service.ReplaceText(joined, artifactID, "after", alice); err != nil {
		t.Fatalf("joined write: %v", err)
	}
	// Commit without publishing, the window a room can be replaced in, and leave the publish a
	// room to load: the write's own update is durable from its append.
	if err := ledger.commit(ctx); err != nil {
		t.Fatalf("commit the write: %v", err)
	}
	if err := service.Evict(ctx, artifactID); err != nil {
		t.Fatalf("evict the room: %v", err)
	}
	persist.armed.Store(true)
	// The publish's own load meets a failure, and the refusal is handled only once that
	// failure's recovery has finished - the window in which the publish would fail the
	// replacement room instead of the failed one.
	service.afterPublishRefused = func(room string) { awaitRecovered(t, service, room) }
	ledger.publish()
	<-persist.failed

	if service.roomFailed(artifactID) {
		t.Fatal("the refused publish failed the room that replaced the one it was refused by")
	}
	retry, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin retry transaction: %v", err)
	}
	defer retry.Rollback(ctx)
	retryJoined, retryLedger := service.Join(ctx, retry)
	defer retryLedger.Discard()
	if _, err := service.ReplaceText(retryJoined, artifactID, "later", alice); err != nil {
		t.Fatalf("joined write after the refused publish: %v", err)
	}
	if err := retryLedger.Commit(ctx); err != nil {
		t.Fatalf("commit the retry: %v", err)
	}
	requireText(t, service, artifactID, "later\n")
}

// A failure drops the room's settlement: the queued one is stopped, a running one refuses, and
// its retry stops because the generation moved. The reloaded room arms one on its next update
// alone, so the ask blocks the dropped settlement would have indexed stayed out of the open
// asks until someone edited the document. The replacement settles once instead.
func TestAFailedRoomsReplacementSettlesWithoutAnotherEdit(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID,
		":::ask{#ask-block urgency=\"high\" multiple=\"false\" state=\"open\"}\nWhich transport?\n:::\n")
	if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
		t.Fatalf("load live document: %v", err)
	}
	if asks := indexedAsks(t, service, artifactID); asks != 0 {
		t.Fatalf("indexed asks before any settlement = %d, want 0", asks)
	}

	service.failRoom(artifactID, errors.New("injected room failure"))
	awaitRecovered(t, service, artifactID)
	// Nothing else reads the delay now: no settlement is armed and the reload below runs on
	// this goroutine.
	service.settle = 10 * time.Millisecond
	if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
		t.Fatalf("load the replacement room: %v", err)
	}

	deadline := time.Now().Add(recoveryBound)
	for indexedAsks(t, service, artifactID) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the failed room's replacement never settled: its ask block is unindexed")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func indexedAsks(t *testing.T, service *Service, artifactID string) int {
	t.Helper()
	var count int
	if err := service.store.Pool.QueryRow(context.Background(), `
		select count(*) from asks where block_artifact_id = $1
	`, artifactID).Scan(&count); err != nil {
		t.Fatalf("count indexed asks: %v", err)
	}
	return count
}

func TestATransactionHoldingTheRoomLockFailsFastOnItsFailedRoom(t *testing.T) {
	for name, create := range map[string]func(*testing.T, *store.Store, string) string{
		"issue document":   createDocument,
		"project document": createProjectDocument,
	} {
		t.Run(name, func(t *testing.T) {
			database := storetest.Open(t)
			artifactID := create(t, database, "")
			service := lockOrderServiceFor(t, database)
			seedServiceText(t, service, artifactID, "before")
			ctx := context.Background()
			if err := service.warmLiveDocument(ctx, artifactID); err != nil {
				t.Fatalf("load live document: %v", err)
			}
			tx, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin transaction: %v", err)
			}
			defer tx.Rollback(ctx)
			joined, ledger := service.Join(ctx, tx)
			defer ledger.Discard()
			if err := lockDocumentRoom(ctx, tx, artifactID); err != nil {
				t.Fatalf("take the document's advisory lock: %v", err)
			}
			service.failRoom(artifactID, errors.New("injected room failure"))

			failsFast(t, "a joined write", func() error {
				_, err := service.ReplaceText(joined, artifactID, "after", model.Actor{Kind: "user", ID: "alice"})
				return err
			})
			if err := tx.Rollback(ctx); err != nil {
				t.Fatalf("roll back: %v", err)
			}
			ledger.Discard()
			awaitRecovered(t, service, artifactID)
			requireText(t, service, artifactID, "before\n")
		})
	}
}

// A write that meets a failed room fails at once even when its own transaction holds none of the
// document's locks: the transaction holding them may be waiting on a lock this one holds. Once
// the room has recovered, a write runs as ever.
func TestAWriteMeetingAFailedRoomFailsFastWhoeverHoldsItsLock(t *testing.T) {
	database, service, artifactID := lockOrderService(t)
	seedServiceText(t, service, artifactID, "before")
	ctx := context.Background()
	if err := service.warmLiveDocument(ctx, artifactID); err != nil {
		t.Fatalf("load live document: %v", err)
	}
	// Another transaction holds the document's advisory lock, so the failed room's eviction,
	// which compacts the document, cannot finish until the test releases it.
	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock holder: %v", err)
	}
	defer holder.Rollback(ctx)
	if err := lockDocumentRoom(ctx, holder, artifactID); err != nil {
		t.Fatalf("hold document lock: %v", err)
	}
	service.failRoom(artifactID, errors.New("injected room failure"))

	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin edit transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	alice := model.Actor{Kind: "user", ID: "alice"}
	failsFast(t, "a joined write", func() error {
		_, err := service.ReplaceText(joined, artifactID, "during", alice)
		return err
	})
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll back: %v", err)
	}
	ledger.Discard()
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release document lock: %v", err)
	}
	awaitRecovered(t, service, artifactID)

	retry, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin retry transaction: %v", err)
	}
	defer retry.Rollback(ctx)
	retryJoined, retryLedger := service.Join(ctx, retry)
	defer retryLedger.Discard()
	if _, err := service.ReplaceText(retryJoined, artifactID, "after", alice); err != nil {
		t.Fatalf("joined write on the recovered room: %v", err)
	}
	if err := retryLedger.Commit(ctx); err != nil {
		t.Fatalf("commit retry transaction: %v", err)
	}
	requireText(t, service, artifactID, "after\n")
}

func TestARoomFailingBetweenTwoJoinedOperationsFailsTheSecondFast(t *testing.T) {
	database, service, artifactID := lockOrderService(t)
	seedServiceText(t, service, artifactID, "before")
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	alice := model.Actor{Kind: "user", ID: "alice"}
	if _, err := service.ReplaceText(joined, artifactID, "during", alice); err != nil {
		t.Fatalf("first joined write: %v", err)
	}
	service.failRoom(artifactID, errors.New("injected room failure"))

	failsFast(t, "the second joined write", func() error {
		_, err := service.ReplaceText(joined, artifactID, "after", alice)
		return err
	})
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll back: %v", err)
	}
	ledger.Discard()
	awaitRecovered(t, service, artifactID)
	requireText(t, service, artifactID, "before\n")
}

// A room that fails while a joined write runs on its fork cannot take the write coherently: the
// room reloads from the durable document, which may not hold the write, while the writer slot
// that holds off every other writer and the settlement is on the failed room. The write fails,
// and its transaction rolls back.
func TestARoomFailingDuringAJoinedWriteFailsTheWrite(t *testing.T) {
	database, service, artifactID := lockOrderService(t)
	seedServiceText(t, service, artifactID, "before")
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()

	failsFast(t, "a joined write whose room fails under it", func() error {
		return service.applyLive(joined, artifactID, model.Actor{Kind: "user", ID: "alice"}, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) error {
			service.failRoom(artifactID, errors.New("injected room failure"))
			tree, err := treeOf(doc)
			if err != nil {
				return err
			}
			fragment := doc.GetXmlFragment(fragmentName)
			next := replaceRun("before", "after")(tree)
			var updateErr error
			transact(func(txn *crdt.Transaction) {
				updateErr = pmdoc.Update(txn, fragment, next)
			})
			return updateErr
		})
	})
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll back: %v", err)
	}
	ledger.Discard()
	awaitRecovered(t, service, artifactID)
	requireText(t, service, artifactID, "before\n")
}

// A committed write that has not been published yet holds the writer slot while a second write
// waits for it. The room fails: the first write's publish waits for the recovery, which needs
// no lock either transaction holds, and the second write then either runs on the reloaded room
// or, had it reached the failed room first, fails fast. It never waits for good.
func TestAWriterWaitingForTheSlotFinishesWhenTheRoomFails(t *testing.T) {
	database, service, artifactID := lockOrderService(t)
	seedServiceText(t, service, artifactID, "before")
	ctx := context.Background()

	first, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin first transaction: %v", err)
	}
	defer first.Rollback(ctx)
	firstJoined, firstLedger := service.Join(ctx, first)
	defer firstLedger.Discard()
	if _, err := service.ReplaceText(firstJoined, artifactID, "first", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("first joined write: %v", err)
	}

	second, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin second transaction: %v", err)
	}
	defer second.Rollback(ctx)
	secondJoined, secondLedger := service.Join(ctx, second)
	defer secondLedger.Discard()
	returned := make(chan error, 1)
	go func() {
		_, err := service.ReplaceText(secondJoined, artifactID, "second", model.Actor{Kind: "user", ID: "bob"})
		returned <- err
	}()
	// The second write waits on the document's owner row while the first transaction is open;
	// once that commits, it takes the row and goes on to wait for the first write's slot.
	waitForLockWait(t, ctx, database, "%from issues where key = $1 for %", returned)
	if err := firstLedger.commit(ctx); err != nil {
		t.Fatalf("commit first transaction: %v", err)
	}
	awaitOwnerRowTaken(t, ctx, database, returned)
	service.failRoom(artifactID, errors.New("injected room failure"))

	published := make(chan struct{})
	go func() {
		firstLedger.publish()
		close(published)
	}()
	select {
	case <-published:
	case <-time.After(recoveryBound):
		t.Fatal("the first write's publish did not finish once its room could recover")
	}
	var secondErr error
	select {
	case secondErr = <-returned:
	case <-time.After(recoveryBound):
		t.Fatal("the second write waited for good behind a slot whose room failed")
	}
	switch {
	case secondErr == nil:
		t.Log("the second write ran on the reloaded room")
		if err := secondLedger.Commit(ctx); err != nil {
			t.Fatalf("commit second transaction: %v", err)
		}
		requireText(t, service, artifactID, "second\n")
	case errors.Is(secondErr, ErrServiceUnavailable):
		t.Log("the second write reached the failed room before the slot and failed fast")
		requireText(t, service, artifactID, "first\n")
	default:
		t.Fatalf("second joined write: %v", secondErr)
	}
}

// awaitOwnerRowTaken returns once another transaction holds DOC-1's owner row, and fails at once
// with the waiting operation's result if it returns first.
func awaitOwnerRowTaken(t *testing.T, ctx context.Context, database *store.Store, returned <-chan error) {
	t.Helper()
	deadline := time.Now().Add(recoveryBound)
	for time.Now().Before(deadline) {
		select {
		case err := <-returned:
			t.Fatalf("the operation returned (%v) before it took the owner row", err)
		default:
		}
		probe, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin owner row probe: %v", err)
		}
		_, err = probe.Exec(ctx, `select 1 from issues where key = 'DOC-1' for no key update nowait`)
		_ = probe.Rollback(ctx)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
			return
		}
		if err != nil {
			t.Fatalf("probe the owner row: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no transaction took the owner row")
}

// Settlement is a transaction too: it holds the document's advisory lock while it stamps block
// ids into the room, so a room that fails in that window fails the settlement rather than
// holding it until an eviction that waits for its lock. The reloaded room settles as ever.
func TestASettlementHoldingTheRoomLockFailsFastOnItsFailedRoom(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	seedUnidentifiedProofDocument(t, database, artifactID, "before")
	service := lockOrderServiceFor(t, database)
	service.afterSettleLock = func(room string) {
		service.failRoom(room, errors.New("injected room failure"))
	}
	settled := make(chan struct{})
	go func() {
		service.settleRoom(artifactID, 0)
		close(settled)
	}()
	select {
	case <-settled:
	case <-time.After(recoveryBound):
		t.Fatal("a settlement holding the document's lock waited for its failed room's recovery")
	}
	awaitRecovered(t, service, artifactID)

	service.afterSettleLock = nil
	settleCurrentGeneration(t, service, artifactID)
	loaded, err := NewPgVersioned(database).Load(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("load settled document: %v", err)
	}
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, loaded.Update, nil); err != nil {
		t.Fatalf("decode settled document: %v", err)
	}
	tree, err := treeOf(doc)
	if err != nil {
		t.Fatalf("read settled document: %v", err)
	}
	if repairs := pmdoc.BlockIDRepairCount(tree); repairs != 0 {
		t.Fatalf("the reloaded room's settlement left %d unstamped blocks", repairs)
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
