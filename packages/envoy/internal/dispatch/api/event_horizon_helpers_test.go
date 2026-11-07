package api

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// appendOutOfOrder appends two events through broker so they commit out of id order: tx A
// appends on issue first (the lower id, low) and stays open, then tx B appends on issue second
// (high) and commits. commitLow commits A and rollbackLow rolls it back, which notifies nothing
// and leaves low a gap for good; until one of them runs, low is an id below a committed one that
// is not yet visible. A that the test ends neither way is rolled back when the test ends.
func appendOutOfOrder(
	t *testing.T, database *store.Store, broker *events.Broker, first, second string,
) (low, high int64, commitLow, rollbackLow func()) {
	t.Helper()
	ctx := t.Context()
	open, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the lower id's transaction: %v", err)
	}
	t.Cleanup(func() { _ = open.Rollback(context.Background()) })
	lowEvent, err := broker.Append(ctx, open, model.Event{
		IssueKey: new(first),
		Type:     "test.concurrent",
		Actor:    model.Actor{Kind: "session", ID: "seed"},
		Payload:  map[string]any{},
	})
	if err != nil {
		t.Fatalf("append the lower id: %v", err)
	}
	committed, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the higher id's transaction: %v", err)
	}
	defer func() { _ = committed.Rollback(context.Background()) }()
	highEvent, err := broker.Append(ctx, committed, model.Event{
		IssueKey: new(second),
		Type:     "test.concurrent",
		Actor:    model.Actor{Kind: "session", ID: "seed"},
		Payload:  map[string]any{},
	})
	if err != nil {
		t.Fatalf("append the higher id while the lower one is open: %v", err)
	}
	if err := committed.Commit(ctx); err != nil {
		t.Fatalf("commit the higher id: %v", err)
	}
	if highEvent.ID <= lowEvent.ID {
		t.Fatalf("event ids %d (open) and %d (committed) are not out of commit order, so this test proves nothing", lowEvent.ID, highEvent.ID)
	}
	commitLow = func() {
		t.Helper()
		if err := open.Commit(ctx); err != nil {
			t.Fatalf("commit the lower id: %v", err)
		}
	}
	rollbackLow = func() {
		t.Helper()
		if err := open.Rollback(ctx); err != nil {
			t.Fatalf("roll back the lower id: %v", err)
		}
	}
	return lowEvent.ID, highEvent.ID, commitLow, rollbackLow
}

// holdClusterTransactionID opens a transaction in the test cluster's postgres database and takes
// a transaction id, as a job in another database or another package's test does. Snapshots are
// instance-wide, so until it ends no horizon on the cluster proves an observation taken after it
// began. It commits when release runs or after hold, whichever comes first, and by the time the
// test ends; release returns once it has.
func holdClusterTransactionID(t *testing.T, hold time.Duration) (release func()) {
	t.Helper()
	config, err := pgx.ParseConfig(os.Getenv(storetest.DatabaseURLEnv))
	if err != nil {
		t.Fatalf("parse %s: %v", storetest.DatabaseURLEnv, err)
	}
	config.Database = "postgres"
	conn, err := pgx.ConnectConfig(t.Context(), config)
	if err != nil {
		t.Fatalf("connect to the cluster's postgres database: %v", err)
	}
	tx, err := conn.Begin(t.Context())
	if err != nil {
		_ = conn.Close(context.Background())
		t.Fatalf("begin the transaction that holds the horizon: %v", err)
	}
	var xid string
	if err := tx.QueryRow(t.Context(), `select pg_current_xact_id()::text`).Scan(&xid); err != nil {
		_ = conn.Close(context.Background())
		t.Fatalf("take a transaction id: %v", err)
	}
	released := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		timer := time.NewTimer(hold)
		defer timer.Stop()
		select {
		case <-released:
		case <-timer.C:
		}
		_ = tx.Commit(context.Background())
		_ = conn.Close(context.Background())
	}()
	var once sync.Once
	release = func() {
		once.Do(func() { close(released) })
		<-done
	}
	t.Cleanup(release)
	return release
}
