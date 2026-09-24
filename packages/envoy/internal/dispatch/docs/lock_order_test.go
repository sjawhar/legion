package docs

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func lockOrderServiceFor(t *testing.T, database *store.Store) *Service {
	t.Helper()
	service := New(Deps{Store: database, Settle: time.Hour})
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = service.Shutdown(stop)
	})
	return service
}

func lockOrderService(t *testing.T) (*store.Store, *Service, string) {
	t.Helper()
	database := storetest.Open(t)
	id := createDocument(t, database, "")
	return database, lockOrderServiceFor(t, database), id
}

// waitForLockWait returns once a statement matching like waits on a lock, and fails at once with
// the waiting operation's result if it returns first.
func waitForLockWait(t *testing.T, ctx context.Context, database *store.Store, like string, returned <-chan error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-returned:
			t.Fatalf("the operation returned (%v) before any statement matching %s waited on a lock", err, like)
		default:
		}
		var waiting int
		// Poll from the pool, never from a transaction: a repeatable-read snapshot freezes
		// pg_stat_activity and the loop spins until it times out.
		if err := database.Pool.QueryRow(ctx, `
			select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock' and query like $1
		`, like).Scan(&waiting); err != nil {
			t.Fatalf("inspect database locks: %v", err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("no statement waiting on a lock matching %s", like)
}

func TestConditionalEditDoesNotInvertTheRoomLockOrder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	database, service, id := lockOrderService(t)
	seedServiceText(t, service, id, "first\n\nsecond")
	alice := model.Actor{Kind: "user", ID: "alice"}
	bob := model.Actor{Kind: "user", ID: "bob"}

	_, token, err := service.TextWithToken(ctx, id)
	if err != nil {
		t.Fatalf("read token: %v", err)
	}

	txOne, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin first transaction: %v", err)
	}
	defer txOne.Rollback(context.Background())
	joinedOne, collectorOne := joinTx(ctx, txOne)
	defer service.DiscardLiveWrites(collectorOne)
	if _, err := service.ApplyOps(joinedOne, id, []model.EditOp{{
		Op: "replace", Find: "first", With: "FIRST",
	}}, alice, &model.EditPrecondition{Document: token}); err != nil {
		t.Fatalf("first conditional edit: %v", err)
	}

	txTwo, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin second transaction: %v", err)
	}
	defer txTwo.Rollback(context.Background())
	secondDone := make(chan error, 1)
	go func() {
		joinedTwo, collectorTwo := joinTx(ctx, txTwo)
		defer service.DiscardLiveWrites(collectorTwo)
		_, err := service.ApplyOps(joinedTwo, id, []model.EditOp{{
			Op: "replace", Find: "second", With: "SECOND",
		}}, bob, &model.EditPrecondition{Document: token})
		secondDone <- err
	}()
	// The second edit takes the document's owner row first (see liveWrite), so it waits there,
	// in the database, holding nothing of the document.
	waitForLockWait(t, ctx, database, "%from issues where key = $1 for %", secondDone)

	snapshotDone := make(chan error, 1)
	go func() {
		_, err := service.SnapshotVersion(joinedOne, txOne, id, alice)
		snapshotDone <- err
	}()

	select {
	case err := <-snapshotDone:
		if err != nil {
			t.Fatalf("snapshot while second edit waits: %v", err)
		}
	case err := <-secondDone:
		t.Fatalf("second edit returned before first transaction released its lock: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("lock-order inversion: a conditional edit holds the document mutex while waiting for the advisory lock another writer holds")
	}
}

// ownerLockedTransaction holds the document's owner row for the duration of one test.
func ownerLockedTransaction(t *testing.T, ctx context.Context, database *store.Store, id string) pgx.Tx {
	t.Helper()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the owner-locked transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, _, err := lockArtifactOwner(ctx, tx, id); err != nil {
		t.Fatalf("lock the document owner: %v", err)
	}
	return tx
}

func lockOrderDocument(t *testing.T) (*store.Store, *Service, string) {
	t.Helper()
	database := storetest.Open(t)
	id := createProjectDocument(t, database, "first")
	return database, lockOrderServiceFor(t, database), id
}

// awaitUnblocked runs op and fails if it has not returned within the window a blocked writer
// would exceed. subject names the writer in the lock-order-inversion message.
func awaitUnblocked(t *testing.T, subject string, op func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- op() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run %s while the owner row is held: %v", subject, err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("lock-order inversion: %s is blocked by the document's owner lock", subject)
	}
}

// An owner-locked writer and a room-locked writer must not block each other. A settlement, an
// event append and a comment write all lock the document's owner row - for a project document
// that is the artifact row itself - while the durable writers take the room lock and then
// reach that same row through doc_updates', doc_snapshots' and doc_checkpoints' foreign keys.
// While the owner lock was `for update` it conflicted with those key-share checks, so the two
// closed a cycle and Postgres broke it with `deadlock detected` - the 500 an upload returned
// when it raced the settlement its own previous write had armed. The owner lock is
// `for no key update` now: it still serialises the writers that take it, including the event
// sequence allocation, and no longer blocks a foreign key.
func TestDurableAppendIsNotBlockedByTheOwnerLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	database, service, id := lockOrderDocument(t)
	settling := ownerLockedTransaction(t, ctx, database, id)

	appending, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the appending transaction: %v", err)
	}
	defer appending.Rollback(context.Background())
	doc := crdt.New()
	doc.GetXmlFragment(fragmentName)
	awaitUnblocked(t, "a durable append", func() error {
		_, err := service.persistence.AppendUpdateTx(
			ctx, appending, id, crdt.EncodeStateAsUpdateV1(doc, nil), true,
		)
		return err
	})
	if err := appending.Commit(context.Background()); err != nil {
		t.Fatalf("commit the append: %v", err)
	}
	// And the owner-locked transaction can still take the room lock behind it.
	if err := lockDocumentRoom(ctx, settling, id); err != nil {
		t.Fatalf("take the room lock behind the append: %v", err)
	}
}

func TestSnapshotIsNotBlockedByTheOwnerLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	database, service, id := lockOrderDocument(t)
	settling := ownerLockedTransaction(t, ctx, database, id)

	// CaptureSnapshot owns its transaction and takes the room lock on its own connection
	// before it can reach the artifact row, so it is the writer the owner lock could only
	// ever deadlock with rather than merely delay.
	awaitUnblocked(t, "a snapshot", func() error {
		_, err := service.persistence.(*PgVersioned).CaptureSnapshot(ctx, id, "live", []byte{0})
		return err
	})
	if err := lockDocumentRoom(ctx, settling, id); err != nil {
		t.Fatalf("take the room lock behind the snapshot: %v", err)
	}
}

// The live-document path is the same cycle with the worst loser. A browser edit reaches
// AppendUpdateWithClass through websocket.go, which takes the room lock on its own connection
// and then the artifact row through doc_updates' foreign key; when the owner lock was
// `for update` and an event append held it, Postgres killed one of them, and on this path the
// loser is failRoom - it evicts the room and drops the in-flight update rather than returning
// an error to anyone.
func TestLiveUpdateIsNotBlockedByTheOwnerLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	database, service, id := lockOrderDocument(t)
	settling := ownerLockedTransaction(t, ctx, database, id)

	doc := crdt.New()
	doc.GetXmlFragment(fragmentName)
	awaitUnblocked(t, "a live document update", func() error {
		_, err := service.persistence.(*PgVersioned).AppendUpdateWithClass(
			ctx, id, crdt.EncodeStateAsUpdateV1(doc, nil), true,
		)
		return err
	})
	if err := lockDocumentRoom(ctx, settling, id); err != nil {
		t.Fatalf("take the room lock behind the live update: %v", err)
	}
}
