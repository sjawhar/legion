package docs

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func lockOrderService(t *testing.T) (*store.Store, *Service, string) {
	t.Helper()
	database := storetest.Open(t)
	id := createDocument(t, database, "")
	service := New(Deps{Store: database, Settle: time.Hour})
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = service.Shutdown(stop)
	})
	return database, service, id
}

func waitForLockWait(t *testing.T, ctx context.Context, database *store.Store, like string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
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
	if _, err := service.ApplyOps(WithTx(ctx, txOne), id, []model.EditOp{{
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
		_, err := service.ApplyOps(WithTx(ctx, txTwo), id, []model.EditOp{{
			Op: "replace", Find: "second", With: "SECOND",
		}}, bob, &model.EditPrecondition{Document: token})
		secondDone <- err
	}()
	waitForLockWait(t, ctx, database, "%pg_advisory_xact_lock%")

	snapshotDone := make(chan error, 1)
	go func() {
		_, err := service.SnapshotVersion(WithTx(ctx, txOne), txOne, id, alice)
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
