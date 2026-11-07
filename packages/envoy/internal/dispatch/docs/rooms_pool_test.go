package docs

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A read of a document takes no connection of the rooms pool, whether a room holds the document or
// none does: it reads through the pool its own caller reads through (readQuerier), and its fold of
// a stored history takes a request-pool connection of its own (foldContext). The rooms pool is for
// the loads a request's connection cannot serve - ygo's room load, which no context reaches, and a
// write's cold fork, which is inside its transaction - and a read that queued on it put every room
// load, a settlement's warm-up among them, behind the reads of requests (INFRA-592).
//
// Each read runs with every connection of the rooms pool held and that pool's acquire bound
// shortened, so a read that asked for one is refused ErrPoolBusy within the test rather than after
// production's three seconds, and with the pool's acquires counted before and after, so a read
// that took one is caught even where the pool has a connection free.
func TestADocumentIsReadWhileTheRoomsPoolIsFull(t *testing.T) {
	t.Run("the room is resident", func(t *testing.T) {
		service, artifactID := newTestService(t)
		service.settle = time.Hour
		shortenRoomsAcquireBound(service)
		seedServiceText(t, service, artifactID, "before\n")
		// A browser's edit leaves the room resident, and the settlement after it leaves the
		// room's cursor at the stored head, which is what a read of the live room reads.
		editLiveTree(t, service, artifactID, replaceRun("before", "after"))
		waitForLandedAppends(t, service, artifactID)
		settleCurrentGeneration(t, service, artifactID)
		if service.srv.GetDoc(artifactID) == nil {
			t.Fatal("the document's room is gone, want it resident for a read of the live room")
		}
		release := holdTheRoomsPool(t, service)
		defer release()

		acquires := roomsAcquires(t, service)
		text, err := service.Text(context.Background(), artifactID)
		if err != nil {
			t.Fatalf("read a resident document with the rooms pool full: %v", err)
		}
		if text != "after\n" {
			t.Fatalf("read = %q, want %q", text, "after\n")
		}
		if took := roomsAcquires(t, service) - acquires; took != 0 {
			t.Fatalf("the read took %d rooms-pool connections, want none", took)
		}
	})

	t.Run("no room holds the document", func(t *testing.T) {
		database := storetest.Open(t)
		versioned := NewPgVersioned(database)
		artifactID := createDocument(t, database, "cold")
		writer := newProofHistory(t, 101)
		appendUpdates(t, versioned, artifactID, writer.write("cold\n"))
		service := New(Deps{
			Store:       database,
			Persistence: versioned,
			Events:      events.NewBroker(),
			Identity:    headerIdentity(database),
			ServerURL:   "https://dispatch.example",
			Settle:      time.Hour,
		})
		t.Cleanup(func() {
			if err := service.Shutdown(context.Background()); err != nil {
				t.Errorf("shutdown document service: %v", err)
			}
		})
		if service.srv.GetDoc(artifactID) != nil {
			t.Fatal("the document is resident, want a cold read")
		}
		shortenRoomsAcquireBound(service)
		release := holdTheRoomsPool(t, service)
		defer release()

		acquires := roomsAcquires(t, service)
		text, err := service.Text(context.Background(), artifactID)
		if err != nil {
			t.Fatalf("read a document no room holds with the rooms pool full: %v", err)
		}
		if text != "cold\n" {
			t.Fatalf("read = %q, want %q", text, "cold\n")
		}
		if took := roomsAcquires(t, service) - acquires; took != 0 {
			t.Fatalf("the cold read took %d rooms-pool connections, want none", took)
		}
		if service.srv.GetDoc(artifactID) != nil {
			t.Fatal("the cold read loaded the document's room")
		}
	})
}

// shortenRoomsAcquireBound makes an acquire of the rooms pool that waits be refused inside the
// test rather than after production's three seconds. The pool takes the bound when it opens, so
// every caller sets it before the first load.
func shortenRoomsAcquireBound(service *Service) {
	service.store.Pool.SetAcquireBoundForTest(store.RoomsPool, 200*time.Millisecond)
}

// holdTheRoomsPool takes every connection of the rooms pool and returns their release.
func holdTheRoomsPool(t *testing.T, service *Service) func() {
	t.Helper()
	rooms, err := service.store.Pool.Rooms()
	if err != nil {
		t.Fatalf("open the rooms pool: %v", err)
	}
	var held []*pgxpool.Conn
	for range rooms.Stat().MaxConns() {
		conn, err := rooms.Acquire(context.Background())
		if err != nil {
			t.Fatalf("hold a rooms connection: %v", err)
		}
		held = append(held, conn)
	}
	return func() {
		for _, conn := range held {
			conn.Release()
		}
	}
}

// roomsAcquires is how many connections the rooms pool has handed out since it opened.
func roomsAcquires(t *testing.T, service *Service) uint64 {
	t.Helper()
	for _, pool := range service.store.Pool.Stats() {
		if pool.Name == store.RoomsPool {
			return pool.Acquire.Count
		}
	}
	t.Fatal("the rooms pool is not open")
	return 0
}
