package docs

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A load of a log within foldBufferBytes reads its rows, ends its transaction and folds them
// holding no connection of the rooms pool, which every room load and cold read waits on; a load of
// a larger log folds it while it streams it inside the transaction, holding that one connection.
// Either way a room's load (Load) and a cold read (LoadDocument) read the state the log makes: a
// single row as stored, a folded log as its merge reads, a log with parked items keeping them, and
// a log the fold cannot apply served merged, which a cold read refuses as ErrDocumentUnloadable.
func TestALoadReadsOneStateWhetherItFoldsTheLogInMemoryOrStreamed(t *testing.T) {
	fixtures := readHistoryFixtures(t)
	parked, lost, gone := parkedHistory(t)
	unfoldable := unfoldableHistory()
	unfoldableMerged, err := crdt.MergeUpdatesV1(unfoldable...)
	if err != nil {
		t.Fatalf("merge the unfoldable log: %v", err)
	}
	database := storetest.Open(t)
	rooms, err := database.Pool.Rooms()
	if err != nil {
		t.Fatalf("open the rooms pool: %v", err)
	}
	ctx := context.Background()
	for _, mode := range []struct {
		name  string
		limit int64
		held  int32
	}{
		{name: "in memory", held: 0},
		{name: "streamed", limit: 1, held: 1},
	} {
		t.Run(mode.name, func(t *testing.T) {
			versioned := NewPgVersioned(database)
			versioned.foldBufferLimit = mode.limit
			var held []int32
			versioned.beforeFold = func(string) { held = append(held, rooms.Stat().AcquiredConns()) }
			requireFoldHeld := func(t *testing.T, what string) {
				t.Helper()
				if len(held) != 1 || held[0] != mode.held {
					t.Fatalf("%s folded holding %v connections of the rooms pool, want one fold holding %d", what, held, mode.held)
				}
				held = held[:0]
			}
			stored := func(t *testing.T, rows [][]byte) string {
				t.Helper()
				artifactID := createDocument(t, database, "history")
				appendUpdates(t, versioned, artifactID, rows...)
				return artifactID
			}
			load := func(t *testing.T, artifactID string) []byte {
				t.Helper()
				loaded, err := versioned.Load(ctx, artifactID)
				if err != nil {
					t.Fatalf("load: %v", err)
				}
				requireFoldHeld(t, "the load")
				return loaded.Update
			}
			coldRead := func(t *testing.T, artifactID string) []byte {
				t.Helper()
				loaded, err := versioned.LoadDocument(ctx, artifactID)
				if err != nil {
					t.Fatalf("cold read: %v", err)
				}
				requireFoldHeld(t, "the cold read")
				return crdt.EncodeStateAsUpdateV1(loaded.Doc, nil)
			}

			t.Run("one row", func(t *testing.T) {
				row := fixtures[0].update
				artifactID := stored(t, [][]byte{row})
				if got := load(t, artifactID); !bytes.Equal(got, row) {
					t.Fatalf("the load returned %d bytes, want the %d-byte row as stored", len(got), len(row))
				}
				requireReadsAs(t, "the cold read", coldRead(t, artifactID), readMerged(t, [][]byte{row}))
			})
			t.Run("many rows", func(t *testing.T) {
				for index, fixture := range fixtures {
					history := documentHistory(t, fixture.update, fixtures[(index+1)%len(fixtures)].update)
					artifactID := stored(t, history)
					want := readMerged(t, history)
					requireReadsAs(t, fixture.Name+": the load", load(t, artifactID), want)
					requireReadsAs(t, fixture.Name+": the cold read", coldRead(t, artifactID), want)
				}
			})
			t.Run("parked items", func(t *testing.T) {
				artifactID := stored(t, parked)
				requireParkedArrive(t, "the load", load(t, artifactID), lost, gone)
				requireParkedArrive(t, "the cold read", coldRead(t, artifactID), lost, gone)
			})
			t.Run("a log the fold cannot apply", func(t *testing.T) {
				artifactID := stored(t, unfoldable)
				if got := load(t, artifactID); !bytes.Equal(got, unfoldableMerged) {
					t.Fatalf("the load returned %d bytes, want the %d-byte merge of its stored updates", len(got), len(unfoldableMerged))
				}
				if _, err := versioned.LoadDocument(ctx, artifactID); !errors.Is(err, ErrDocumentUnloadable) {
					t.Fatalf("cold read of a log the fold cannot apply: %v, want ErrDocumentUnloadable", err)
				}
				requireFoldHeld(t, "the cold read")
			})
		})
	}
}

// copyingLoadStore answers a room's load with a copy of the state the store below it loaded.
type copyingLoadStore struct {
	VersionedStore
}

func (s copyingLoadStore) Load(ctx context.Context, room string) (persistence.LoadResult, error) {
	result, err := s.VersionedStore.Load(ctx, room)
	result.Update = bytes.Clone(result.Update)
	return result, err
}

// A room's load decodes a folded log's state no further before the room does: the fold applied the
// rows and read the state back into the document a room decodes it into, so the load does not
// check it again (validateUpdate). It checks a single row, which the store returns as stored, and
// a state a store wrapping PgVersioned answers with in place of the one the fold read back. A log
// the fold cannot apply, served merged, is checked too, and that check fails the room, as ygo's
// own load of the room meets it.
func TestARoomLoadChecksOnlyAStateTheFoldDidNotReadBack(t *testing.T) {
	database := storetest.Open(t)
	versioned := NewPgVersioned(database)
	service := New(Deps{Store: database, Persistence: versioned, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), recoveryBound)
		defer cancel()
		_ = service.Shutdown(stop)
	})
	failed := make(chan string, 4)
	service.beforeFailedEviction = func(room string) { failed <- room }
	requireFailed := func(t *testing.T, artifactID string) {
		t.Helper()
		select {
		case room := <-failed:
			if room != artifactID {
				t.Fatalf("failed room %s, want %s", room, artifactID)
			}
		case <-time.After(recoveryBound):
			t.Fatal("the room that cannot load never failed")
		}
	}
	var checks atomic.Int32
	loadRoom := func(t *testing.T, store VersionedStore, rows [][]byte) ([]byte, string, int32, error) {
		t.Helper()
		artifactID := createDocument(t, database, "history")
		appendUpdates(t, versioned, artifactID, rows...)
		adapter := &servicePersistenceAdapter{store: store, service: service, validating: func(string) { checks.Add(1) }}
		checks.Store(0)
		served, err := adapter.LoadDoc(artifactID)
		return served, artifactID, checks.Load(), err
	}
	text := func(t *testing.T, state []byte) string {
		t.Helper()
		doc := crdt.New()
		if err := crdt.ApplyUpdateV1(doc, state, nil); err != nil {
			t.Fatalf("decode the served state: %v", err)
		}
		return doc.GetText("t").ToString()
	}
	writer := crdt.New(crdt.WithClientID(7))
	written := writer.GetText("t")
	var folded [][]byte
	for _, edit := range []func(*crdt.Transaction){
		func(txn *crdt.Transaction) { written.Insert(txn, 0, "base", nil) },
		func(txn *crdt.Transaction) { written.Insert(txn, 4, " gone", nil) },
		func(txn *crdt.Transaction) { written.Delete(txn, 4, 5) },
		func(txn *crdt.Transaction) { written.Insert(txn, 4, " kept", nil) },
	} {
		before := writer.StateVector()
		writer.Transact(edit)
		folded = append(folded, crdt.EncodeStateAsUpdateV1(writer, before))
	}

	served, _, checked, err := loadRoom(t, versioned, folded)
	if err != nil || checked != 0 || text(t, served) != "base kept" {
		t.Fatalf("room load of a folded log: %q, %d checks (%v), want %q and no check", text(t, served), checked, err, "base kept")
	}
	served, _, checked, err = loadRoom(t, versioned, folded[:1])
	if err != nil || checked != 1 || !bytes.Equal(served, folded[0]) {
		t.Fatalf("room load of one row: %d bytes, %d checks (%v), want the row as stored and one check", len(served), checked, err)
	}
	served, _, checked, err = loadRoom(t, copyingLoadStore{VersionedStore: versioned}, folded)
	if err != nil || checked != 1 || text(t, served) != "base kept" {
		t.Fatalf("room load of a wrapping store's copy: %q, %d checks (%v), want %q and one check", text(t, served), checked, err, "base kept")
	}
	_, artifactID, checked, err := loadRoom(t, versioned, unfoldableHistory())
	if err == nil || checked != 1 {
		t.Fatalf("room load of a log the fold cannot apply: %d checks (%v), want one check that fails", checked, err)
	}
	requireFailed(t, artifactID)

	artifactID = createDocument(t, database, "unfoldable")
	appendUpdates(t, versioned, artifactID, unfoldableHistory()...)
	if err := service.warmLiveDocument(context.Background(), artifactID); err == nil {
		t.Fatal("a room of a log the fold cannot apply loaded")
	}
	requireFailed(t, artifactID)
}
