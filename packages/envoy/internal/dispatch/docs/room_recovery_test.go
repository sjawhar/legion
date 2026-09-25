package docs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/reearth/ygo/crdt"

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
