package docs

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
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
	unfoldable, _ := unfoldableHistory()
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
				// The log is built for a small pending queue, which the other fixtures above
				// park more than: shrink the store only here, and only once they are read.
				shrinkPendingItems(t, nil, versioned)
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

// Buffered folds are bounded across the process (foldSlots): with one slot, a second load whose
// rows it reads into memory waits for the slot, holding its read transaction, while the first is
// inside its fold, and folds once the first gives the slot back; a waiter whose context ends
// returns that context's error without folding.
func TestABufferedFoldWaitsForASlotWithinItsContext(t *testing.T) {
	database := storetest.Open(t)
	versioned := NewPgVersioned(database)
	versioned.foldSlots = make(chan struct{}, 1)
	writer := newProofHistory(t, 101)
	rows := [][]byte{writer.write("First.\n"), writer.write("First.\n\nSecond.\n")}
	holding, waiting := createDocument(t, database, "holding"), createDocument(t, database, "waiting")
	appendUpdates(t, versioned, holding, rows...)
	appendUpdates(t, versioned, waiting, rows...)
	held, released := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(released) })
	t.Cleanup(release)
	var waitedFolds atomic.Int32
	versioned.beforeFold = func(room string) {
		switch room {
		case holding:
			close(held)
			<-released
		case waiting:
			waitedFolds.Add(1)
		}
	}
	ctx := context.Background()

	first := make(chan error, 1)
	go func() {
		_, err := versioned.Load(ctx, holding)
		first <- err
	}()
	select {
	case <-held:
	case <-time.After(30 * time.Second):
		t.Fatal("the first load never reached its fold")
	}
	type coldRead struct {
		loaded LoadedDocument
		err    error
	}
	second := make(chan coldRead, 1)
	go func() {
		loaded, err := versioned.LoadDocument(ctx, waiting)
		second <- coldRead{loaded, err}
	}()
	cancelled, cancel := context.WithCancel(ctx)
	defer cancel()
	third := make(chan error, 1)
	go func() {
		_, err := versioned.Load(cancelled, waiting)
		third <- err
	}()
	// The holder committed its read before its fold, so the two transactions left open are the
	// waiters', each past its read of the stored log and waiting for the slot.
	waitFor(t, 30*time.Second, "both later loads to wait for the slot inside their read transactions", func() bool {
		return idleInTransaction(t, database) == 2
	})
	select {
	case read := <-second:
		t.Fatalf("the second load returned (%v) while the only slot was held", read.err)
	case err := <-third:
		t.Fatalf("the third load returned (%v) while the only slot was held and its context was live", err)
	case <-time.After(200 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-third:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the waiter whose context ended returned %v, want %v", err, context.Canceled)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the waiter whose context ended is still waiting for the slot")
	}
	select {
	case read := <-second:
		t.Fatalf("the second load returned (%v) while the only slot was held", read.err)
	default:
	}
	if folds := waitedFolds.Load(); folds != 0 {
		t.Fatalf("the waiting document folded %d times while the only slot was held, want none", folds)
	}

	release()
	select {
	case err := <-first:
		if err != nil {
			t.Fatalf("the first load: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the first load did not return once released")
	}
	select {
	case read := <-second:
		if read.err != nil {
			t.Fatalf("the second load: %v", read.err)
		}
		if got := docMarkdown(read.loaded.Doc); got != "First.\n\nSecond.\n" {
			t.Fatalf("the second load reads %q, want the stored document", got)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the second load did not proceed once the slot was given back")
	}
	if folds := waitedFolds.Load(); folds != 1 {
		t.Fatalf("the waiting document folded %d times, want the second load's one fold", folds)
	}
}

// idleInTransaction counts the sessions of the test's database that hold a transaction open
// without running a statement in it.
func idleInTransaction(t *testing.T, database *store.Store) int {
	t.Helper()
	var sessions int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from pg_stat_activity
		where datname = current_database() and state = 'idle in transaction'
	`).Scan(&sessions); err != nil {
		t.Fatalf("count the sessions idle in a transaction: %v", err)
	}
	return sessions
}

// A fold of rows read into memory ends with its context, as a fold that streams them does: a load
// or a cold read whose context ends once its read transaction has committed, before its fold
// starts, returns the context's error and no state, and a fold whose context ends between two
// updates applies none after it and returns that error.
func TestABufferedFoldEndsWithItsContext(t *testing.T) {
	database := storetest.Open(t)
	versioned := NewPgVersioned(database)
	writer := newProofHistory(t, 101)
	rows := [][]byte{
		writer.write("First.\n"),
		writer.write("First.\n\nSecond.\n"),
		writer.write("First.\n\nSecond.\n\nThird.\n"),
	}
	artifactID := createDocument(t, database, "cancelled")
	appendUpdates(t, versioned, artifactID, rows...)

	t.Run("before the fold starts", func(t *testing.T) {
		loadCtx, cancelLoad := context.WithCancel(context.Background())
		defer cancelLoad()
		versioned.beforeFold = func(string) { cancelLoad() }
		loaded, err := versioned.Load(loadCtx, artifactID)
		if !errors.Is(err, context.Canceled) || loaded.Update != nil || loaded.Version != 0 {
			t.Fatalf("the load whose context ended before its fold returned %d bytes at version %d (%v), want none and %v",
				len(loaded.Update), loaded.Version, err, context.Canceled)
		}
		readCtx, cancelRead := context.WithCancel(context.Background())
		defer cancelRead()
		versioned.beforeFold = func(string) { cancelRead() }
		document, err := versioned.LoadDocument(readCtx, artifactID)
		if !errors.Is(err, context.Canceled) || document.Doc != nil || document.Stamp != (DocumentStamp{}) {
			t.Fatalf("the cold read whose context ended before its fold returned %+v (%v), want no document and %v",
				document.Stamp, err, context.Canceled)
		}
	})
	t.Run("between updates", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		replay := memoryUpdates(ctx, rows)
		applied := 0
		state, err := stateThrough(artifactID, 3, func(use func([]byte) error) error {
			return replay(func(update []byte) error {
				applied++
				if applied == 2 {
					cancel()
				}
				return use(update)
			})
		}, maxUpdateItems)
		if !errors.Is(err, context.Canceled) || state != nil || applied != 2 {
			t.Fatalf("the fold whose context ended after its second update returned %d bytes (%v) after %d updates, want none, %v and two",
				len(state), err, applied, context.Canceled)
		}
	})
}

// roomLoadService is a service on a database of its own whose room loads record each stored state
// their load hook decodes (bootstrapLoadedState), by room.
func roomLoadService(t *testing.T) (*Service, *PgVersioned, *roomDecodes) {
	t.Helper()
	database := storetest.Open(t)
	versioned := NewPgVersioned(database)
	service := New(Deps{Store: database, Persistence: versioned, Events: events.NewBroker(), Settle: time.Hour})
	decodes := &roomDecodes{states: make(map[string][][]byte)}
	service.bootstrapping = decodes.decoded
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), recoveryBound)
		defer cancel()
		_ = service.Shutdown(stop)
	})
	return service, versioned, decodes
}

// roomDecodes is every stored state a room's load hook decoded, by room, in order.
type roomDecodes struct {
	mu     sync.Mutex
	states map[string][][]byte
}

func (d *roomDecodes) decoded(room string, state []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.states[room] = append(d.states[room], state)
}

func (d *roomDecodes) of(room string) [][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([][]byte(nil), d.states[room]...)
}

// A room's load decodes the state its store read once, in the room's own load hook: ygo is handed
// no bytes to decode. A single stored row is that decode alone, of the row as stored; a folded log
// is decoded by the fold's read-back too, and then once in the room. Either room reads the text
// its history wrote.
func TestARoomLoadDecodesItsStoredStateOnceInTheRoom(t *testing.T) {
	service, versioned, decodes := roomLoadService(t)
	ctx := context.Background()
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

	for _, test := range []struct {
		name string
		rows [][]byte
		text string
	}{
		{"one stored row", folded[:1], "base"},
		{"a folded log", folded, "base kept"},
	} {
		t.Run(test.name, func(t *testing.T) {
			artifactID := createDocument(t, service.store, "history")
			appendUpdates(t, versioned, artifactID, test.rows...)
			stored, err := versioned.Load(ctx, artifactID)
			if err != nil {
				t.Fatalf("load the stored state: %v", err)
			}
			if len(test.rows) == 1 && !bytes.Equal(stored.Update, test.rows[0]) {
				t.Fatalf("the store loaded %d bytes of one row, want the %d-byte row as stored", len(stored.Update), len(test.rows[0]))
			}
			// What ygo itself decodes: nothing.
			adapter := &servicePersistenceAdapter{store: versioned, service: service}
			if handed, err := adapter.LoadDoc(artifactID); err != nil || len(handed) != 0 {
				t.Fatalf("LoadDoc handed ygo %d bytes (%v), want none", len(handed), err)
			}
			if kept := service.takeLoadedState(artifactID); !bytes.Equal(kept, stored.Update) {
				t.Fatalf("LoadDoc kept %d bytes for the room's load hook, want the %d-byte stored state", len(kept), len(stored.Update))
			}

			if err := service.warmLiveDocument(ctx, artifactID); err != nil {
				t.Fatalf("load the room: %v", err)
			}
			live := service.srv.GetDoc(artifactID)
			if live == nil {
				t.Fatal("the room loaded no document")
			}
			if got := live.GetText("t").ToString(); got != test.text {
				t.Fatalf("the room reads %q, want %q", got, test.text)
			}
			if states := decodes.of(artifactID); len(states) != 1 || !bytes.Equal(states[0], stored.Update) {
				t.Fatalf("the room's load decoded %d states, want the stored state decoded once", len(states))
			}
		})
	}
}

// A history that does not decode in the room fails the room as the check before the room's
// decode (validateUpdate) failed it: the caller gets the error that check gives for the same bytes,
// the room's failure holds it, its failed-room eviction runs, no room is published, and the next
// access loads the history afresh and fails the same way. A single stored row that parks more
// than the server's pending cap does not load, nor does a log the fold cannot apply, served
// merged.
func TestARoomWhoseHistoryDoesNotDecodeFailsFromTheRoomsOwnDecode(t *testing.T) {
	service, versioned, decodes := roomLoadService(t)
	// Before any room opens: ygo reads the server's cap as each room loads, and the log below is
	// built to park more than that cap holds.
	shrinkPendingItems(t, service, versioned)
	ctx := context.Background()
	evicting := make(chan string, 1)
	release := make(chan struct{}, 1)
	finished := make(chan struct{})
	service.beforeFailedEviction = func(room string) {
		evicting <- room
		select {
		case <-release:
		case <-finished:
		}
	}
	t.Cleanup(func() { close(finished) })
	unfoldable, _ := unfoldableHistory()
	merged, err := crdt.MergeUpdatesV1(unfoldable...)
	if err != nil {
		t.Fatalf("merge the unfoldable log: %v", err)
	}

	for _, test := range []struct {
		name  string
		store func(t *testing.T, artifactID string)
	}{
		{"one stored row", func(t *testing.T, artifactID string) {
			// A row no append takes (appendUpdate decodes it under the same cap), stored as it is.
			if _, err := service.store.Pool.Exec(ctx, `
				insert into doc_updates (artifact_id, version, update, content_changed) values ($1, 1, $2, true)
			`, artifactID, merged); err != nil {
				t.Fatalf("store the row: %v", err)
			}
		}},
		{"a log served merged", func(t *testing.T, artifactID string) {
			appendUpdates(t, versioned, artifactID, unfoldable...)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			artifactID := createDocument(t, service.store, "unloadable")
			test.store(t, artifactID)
			stored, err := versioned.Load(ctx, artifactID)
			if err != nil {
				t.Fatalf("load the stored state: %v", err)
			}
			if !bytes.Equal(stored.Update, merged) {
				t.Fatalf("the store loaded %d bytes, want the %d-byte merge", len(stored.Update), len(merged))
			}
			want := validateUpdate(stored.Update, service.maxPendingItems)
			if want == nil {
				t.Fatal("the stored state decodes, so nothing here is tested")
			}

			for access := 1; access <= 2; access++ {
				err := service.warmLiveDocument(ctx, artifactID)
				if !wrapsDecodeError(err, want) {
					t.Fatalf("access %d: the room's load = %v, want the decode's error %q", access, err, want)
				}
				select {
				case room := <-evicting:
					if room != artifactID {
						t.Fatalf("access %d: failed room %s, want %s", access, room, artifactID)
					}
				case <-time.After(recoveryBound):
					t.Fatalf("access %d: the room that cannot load never failed", access)
				}
				// The eviction waits here, so the failure is the one this access recorded.
				state := service.lockExistingState(artifactID)
				if state == nil {
					t.Fatalf("access %d: the failed room has no record", access)
				}
				cause := state.failed
				service.unlockState(artifactID, state)
				if cause == nil || cause.Error() != want.Error() || errors.Is(cause, crdt.ErrInvalidUpdate) != errors.Is(want, crdt.ErrInvalidUpdate) {
					t.Fatalf("access %d: the room failed for %v, want the decode's error %q", access, cause, want)
				}
				if !errors.Is(err, cause) {
					t.Fatalf("access %d: the caller got %v, not the failure the room recorded", access, err)
				}
				if failure := service.roomFailure(artifactID); !errors.Is(failure, ErrServiceUnavailable) || !errors.Is(failure, cause) {
					t.Fatalf("access %d: the room's failure = %v, want ErrServiceUnavailable for the decode's error", access, failure)
				}
				if service.srv.GetDoc(artifactID) != nil {
					t.Fatalf("access %d: a room that did not load is served", access)
				}
				release <- struct{}{}
				recovered, cancel := context.WithTimeout(ctx, recoveryBound)
				err = service.awaitRoomRecovery(recovered, artifactID)
				cancel()
				if err != nil {
					t.Fatalf("access %d: the failed room's eviction: %v", access, err)
				}
				if service.srv.GetDoc(artifactID) != nil {
					t.Fatalf("access %d: a room that did not load is served after its eviction", access)
				}
				if states := decodes.of(artifactID); len(states) != access || !bytes.Equal(states[access-1], stored.Update) {
					t.Fatalf("access %d: the room's loads decoded %d states, want the stored state once per access", access, len(states))
				}
			}
		})
	}
}

// wrapsDecodeError reports whether err carries want, a decode's error, as the same error: one in
// its chain that says what want says and is the sentinel want is.
func wrapsDecodeError(err, want error) bool {
	if err == nil {
		return false
	}
	if err.Error() == want.Error() {
		return errors.Is(err, crdt.ErrInvalidUpdate) == errors.Is(want, crdt.ErrInvalidUpdate)
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() error }:
		return wrapsDecodeError(wrapped.Unwrap(), want)
	case interface{ Unwrap() []error }:
		for _, each := range wrapped.Unwrap() {
			if wrapsDecodeError(each, want) {
				return true
			}
		}
	}
	return false
}
