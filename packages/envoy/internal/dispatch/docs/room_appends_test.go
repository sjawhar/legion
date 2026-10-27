package docs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A browser's keystrokes reach the store through the room's persistence, one call per update
// (PersistCoalesceWindow = -1). The tests here are about what those calls cost the database and
// what they leave in it when a room's updates queue up behind each other: the store takes the
// updates a room has queued together, in one transaction on connections of the rooms' own, and
// leaves exactly the rows the same updates stored one at a time leave.

// statementTracer records every round trip the connections of one store make to Postgres: each
// statement sent on its own (pgx.QueryTracer) and each pipeline (pgx.BatchTracer), a pipeline
// being one round trip however many statements it carries. It is installed on the store's
// connection configuration (tracedStore), and every pool the store opens later is opened from a
// copy of that configuration (store.Pool.separate), so it sees the shared pool's connections and
// those of the rooms pool and of any pool of the rooms' own alike.
//
// A test asks it two things. How many round trips a browser append makes holding its document's
// lock (lockedAppends): the span, on one connection, from the session pg_advisory_lock of the
// document to its pg_advisory_unlock, both included, for each span that wrote a row of doc_updates
// - one span per append transaction, which is how the store takes a room's updates. And how many
// statements of some text the store sent for one document (statements), such as a lock statement.
//
// What it does not count is a statement's preparation. pgx describes a statement it has not seen
// on a connection before in a round trip of its own, once per connection and statement text, so
// that round trip is a property of a connection's first use, not of what the code sends each time.
// The count is of the statements and pipelines the code sends, which is what the code decides.
type statementTracer struct {
	mu    sync.Mutex
	trips []roundTrip
}

// roundTrip is one statement, or one pipeline of statements, a connection sent.
type roundTrip struct {
	conn       *pgx.Conn
	statements []tracedStatement
}

type tracedStatement struct {
	sql  string
	args []any
}

// names reports whether the statement's first argument is room, as every statement of a
// document's lock and rows passes it.
func (statement tracedStatement) names(room string) bool {
	return len(statement.args) > 0 && fmt.Sprint(statement.args[0]) == room
}

func (t *statementTracer) record(conn *pgx.Conn, statements []tracedStatement) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.trips = append(t.trips, roundTrip{conn: conn, statements: statements})
}

func (t *statementTracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	t.record(conn, []tracedStatement{{sql: data.SQL, args: data.Args}})
	return ctx
}

func (t *statementTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (t *statementTracer) TraceBatchStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	statements := make([]tracedStatement, 0, len(data.Batch.QueuedQueries))
	for _, queued := range data.Batch.QueuedQueries {
		statements = append(statements, tracedStatement{sql: queued.SQL, args: queued.Arguments})
	}
	t.record(conn, statements)
	return ctx
}

func (t *statementTracer) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

func (t *statementTracer) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// mark is where the round trips recorded from now on start, for a later count since it.
func (t *statementTracer) mark() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.trips)
}

// lockedAppends is, for each append transaction of room's document since mark, how many round trips
// its connection made from its session lock of the document to that lock's release, both included:
// each span between a pg_advisory_lock and a pg_advisory_unlock of the document, on one connection,
// that wrote a row of doc_updates.
func (t *statementTracer) lockedAppends(room string, since int) []int {
	t.mu.Lock()
	defer t.mu.Unlock()
	type span struct {
		trips   int
		appends bool
	}
	open := make(map[*pgx.Conn]*span)
	var appends []int
	for _, trip := range t.trips[since:] {
		current := open[trip.conn]
		if current == nil {
			for _, statement := range trip.statements {
				if strings.Contains(statement.sql, "pg_advisory_lock(hashtext(") && statement.names(room) {
					current = &span{}
					open[trip.conn] = current
					break
				}
			}
			if current == nil {
				continue
			}
		}
		current.trips++
		for _, statement := range trip.statements {
			if strings.Contains(statement.sql, "insert into doc_updates") {
				current.appends = true
			}
		}
		for _, statement := range trip.statements {
			if strings.Contains(statement.sql, "pg_advisory_unlock(hashtext(") && statement.names(room) {
				if current.appends {
					appends = append(appends, current.trips)
				}
				delete(open, trip.conn)
				break
			}
		}
	}
	return appends
}

// statements is how many statements naming room, whose text contains text, were sent since mark,
// a pipeline's counted one by one.
func (t *statementTracer) statements(room, text string, since int) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	count := 0
	for _, trip := range t.trips[since:] {
		for _, statement := range trip.statements {
			if strings.Contains(statement.sql, text) && statement.names(room) {
				count++
			}
		}
	}
	return count
}

// tracedStore is a store of its own on database's database whose every connection, and every
// connection of every pool it opens later, reports to tracer.
func tracedStore(t *testing.T, database *store.Store, tracer *statementTracer) *store.Store {
	t.Helper()
	config := database.Pool.Config()
	config.ConnConfig.Tracer = tracer
	opened, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open a traced pool on the test database: %v", err)
	}
	pool := store.NewPool(opened)
	t.Cleanup(pool.Close)
	return &store.Store{Pool: pool}
}

// storedVersionsAfter is every version of artifactID's stored updates past after, in order.
func storedVersionsAfter(t *testing.T, database *store.Store, artifactID string, after int64) []int64 {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select version from doc_updates where artifact_id = $1 and version > $2 order by version
	`, artifactID, after)
	if err != nil {
		t.Fatalf("read the document's stored versions: %v", err)
	}
	versions, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		t.Fatalf("read the document's stored versions: %v", err)
	}
	return versions
}

// typeInRoom is a keystroke of actor's browser, connected on connection, in artifactID's resident
// room: the room applies it with actor connected, so its update observer credits actor alone, and
// ygo's persistence takes it from there. Unlike editAsConnectedPeer it does not warm the room
// first - a warm is a Server.Apply, whose injection check reads the issue through the shared pool
// (allowInject) - so the store's own work for the keystroke is all the keystroke costs.
func typeInRoom(t *testing.T, service *Service, artifactID string, connection uint64, actor model.Actor, edit func(*pmdoc.Node) *pmdoc.Node) *crdt.Doc {
	t.Helper()
	service.addConnection(artifactID, connection, actor)
	defer service.removeConnection(artifactID, connection)
	return editAsPeer(t, service, artifactID, edit)
}

// A room whose browser types twenty keystrokes while its first append waits for the document's
// lock - another writer holds it, as a settlement or a live write does - queues the other nineteen
// behind that append. Stored one at a time, each of them takes a connection of the shared request
// pool and about ten round trips under the document's lock, so a person typing costs the API's
// requests a connection per keystroke. The queued updates are stored together instead: in fewer
// transactions than updates, as consecutive rows in the order the room applied them, each
// transaction in at most three round trips, and on connections of the rooms' own, so the shared
// pool hands the room none.
func TestQueuedBrowserUpdatesOfOneRoomCommitInOneTransactionOffTheSharedPool(t *testing.T) {
	const queued = 20
	ctx := context.Background()
	database := storetest.Open(t)
	tracer := &statementTracer{}
	traced := tracedStore(t, database, tracer)
	// The test's own reads and its hold of the document's lock go through a store of their own,
	// so the traced store's shared pool counts the room's acquisitions alone.
	observer := storetest.SecondProcess(t, database)
	artifactID := createDocument(t, database, "# First")
	service, _ := newTestServiceInstance(t, traced)
	seedServiceText(t, service, artifactID, "First.\n")
	settleCurrentGeneration(t, service, artifactID)
	if err := service.warmLiveDocument(ctx, artifactID); err != nil {
		t.Fatalf("load the document's room: %v", err)
	}
	waitForLandedAppends(t, service, artifactID)
	alice := model.Actor{Kind: "user", ID: "alice"}
	before := storedHead(t, observer, artifactID)

	unlock := holdDocumentLock(t, observer, artifactID)
	since := tracer.mark()
	sharedBefore := traced.Pool.Stat().AcquireCount()
	for keystroke := range queued {
		typeInRoom(t, service, artifactID, 1, alice, appendBlocks(t, fmt.Sprintf("Keystroke %d.\n", keystroke)))
	}
	waitFor(t, roomWait(t), "the room's first append to wait for the document's lock", func() bool {
		return waitingAdvisoryLocks(t, observer) > 0
	})
	unlock()
	waitFor(t, roomWait(t), "every queued update to be stored", func() bool {
		return storedHead(t, observer, artifactID) >= before+queued
	})
	waitForLandedAppends(t, service, artifactID)
	sharedAcquired := traced.Pool.Stat().AcquireCount() - sharedBefore

	versions := storedVersionsAfter(t, observer, artifactID, before)
	want := make([]int64, 0, queued)
	for version := before + 1; version <= before+queued; version++ {
		want = append(want, version)
	}
	if !slices.Equal(versions, want) {
		t.Errorf("the queued updates are stored at versions %v, want the consecutive %v", versions, want)
	}
	transactions := tracer.lockedAppends(artifactID, since)
	switch {
	case len(transactions) == 0:
		t.Errorf("no append transaction of the document was seen under its session lock, so nothing here counts what it says")
	case len(transactions) >= queued:
		t.Errorf("the %d queued updates committed in %d transactions, want fewer than %d", queued, len(transactions), queued)
	}
	for index, trips := range transactions {
		if trips > 3 {
			t.Errorf("append transaction %d of %d made %d round trips under the document's lock (all of them: %v), want at most 3",
				index+1, len(transactions), trips, transactions)
			break
		}
	}
	if sharedAcquired != 0 {
		t.Errorf("the room took %d connections of the shared request pool to store its %d updates, want none", sharedAcquired, queued)
	}
}

// creditedStep is one browser update of a credit scenario: who was connected while the room
// applied it, nobody when author is nil, and whether a committed version listed its credit before
// its append took it (UpdateCredit.take answers include=false then).
type creditedStep struct {
	author   *model.Actor
	consumed bool
}

// storeCreditedUpdates makes steps' updates in artifactID's room and returns the head the store
// stood at before them. together queues every update behind the document's lock, held by another
// connection until all of them are in the room, so the store takes them as one queue; otherwise
// each is stored before the next is made. A consumed step's credit is marked consumed while its
// append waits for that lock, as a version that listed it and committed first would leave it.
func storeCreditedUpdates(t *testing.T, service *Service, observer *store.Store, artifactID string, steps []creditedStep, together bool) int64 {
	t.Helper()
	if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
		t.Fatalf("load the document's room: %v", err)
	}
	waitForLandedAppends(t, service, artifactID)
	base := storedHead(t, observer, artifactID)
	var unlock func()
	if together {
		unlock = holdDocumentLock(t, observer, artifactID)
	}
	for index, step := range steps {
		if !together {
			unlock = holdDocumentLock(t, observer, artifactID)
		}
		edit := appendBlocks(t, fmt.Sprintf("Update %d.\n", index))
		if step.author != nil {
			typeInRoom(t, service, artifactID, uint64(index+1), *step.author, edit)
		} else {
			editAsPeer(t, service, artifactID, edit)
		}
		if step.consumed {
			state := service.lockState(artifactID)
			record := state.inflight[state.creditSeq.Load()]
			if record != nil {
				record.consumed = true
			}
			service.unlockState(artifactID, state)
			if record == nil {
				t.Fatalf("update %d's credit is not in flight while its append waits for the document's lock", index)
			}
		}
		if !together {
			unlock()
			waitForLandedAppends(t, service, artifactID)
		}
	}
	if together {
		waitFor(t, roomWait(t), "the room's first append to wait for the document's lock", func() bool {
			return waitingAdvisoryLocks(t, observer) > 0
		})
		unlock()
		waitForLandedAppends(t, service, artifactID)
	}
	return base
}

// creditRows is what a document's updates left of their credit: its pending authors, each with the
// update that recorded it counted from base, and its pending settlement's latest edit source.
type creditRows struct {
	authors   []string
	owed      bool
	lastActor string
}

func creditRowsSince(t *testing.T, database *store.Store, artifactID string, base int64) creditRows {
	t.Helper()
	ctx := context.Background()
	rows, err := database.Pool.Query(ctx, `
		select actor_kind || '/' || actor_id || ' at update ' || (written_through - $2)
		from doc_pending_authors where artifact_id = $1 order by 1
	`, artifactID, base)
	if err != nil {
		t.Fatalf("read the document's pending authors: %v", err)
	}
	authors, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the document's pending authors: %v", err)
	}
	var lastActor *string
	err = database.Pool.QueryRow(ctx, `
		select last_actor::text from doc_settlements_pending where artifact_id = $1
	`, artifactID).Scan(&lastActor)
	if errors.Is(err, pgx.ErrNoRows) {
		return creditRows{authors: authors}
	}
	if err != nil {
		t.Fatalf("read the document's pending settlement: %v", err)
	}
	found := creditRows{authors: authors, owed: true}
	if lastActor != nil {
		found.lastActor = *lastActor
	}
	return found
}

// The rows a room's queued updates leave are the rows the same updates leave stored one at a time:
// a pending author per credited update, recorded at that update's own version, none for a credit a
// committed version already listed, and a pending settlement whose latest edit source is the last
// credited update's actor when that update names one, and is kept otherwise (markUpdateOwedBy,
// markUpdateOwed). Each scenario is made twice on two documents, queued and one at a time.
func TestABatchLeavesThePendingAuthorRowsOneAtATimeAppendsLeave(t *testing.T) {
	alice := model.Actor{Kind: "user", ID: "alice"}
	bob := model.Actor{Kind: "user", ID: "bob"}
	carol := model.Actor{Kind: "session", ID: "carol"}
	cases := []struct {
		name  string
		steps []creditedStep
	}{
		{name: "credits not consumed", steps: []creditedStep{{author: &alice}, {author: &bob}, {author: &alice}, {author: &carol}}},
		{name: "credits consumed", steps: []creditedStep{{author: &alice, consumed: true}, {author: &bob}, {author: &carol, consumed: true}, {author: &alice}}},
		{name: "the last credit consumed", steps: []creditedStep{{author: &alice}, {author: &bob, consumed: true}}},
		{name: "no connected author", steps: []creditedStep{{author: &alice}, {}, {author: &bob}, {}}},
	}
	database := storetest.Open(t)
	observer := storetest.SecondProcess(t, database)
	service, _ := newTestServiceInstance(t, database)
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			queuedDoc := createDocument(t, database, "# First")
			oneByOneDoc := createDocument(t, database, "# First")
			for _, artifactID := range []string{queuedDoc, oneByOneDoc} {
				seedServiceText(t, service, artifactID, "First.\n")
				settleCurrentGeneration(t, service, artifactID)
			}
			queuedBase := storeCreditedUpdates(t, service, observer, queuedDoc, test.steps, true)
			oneByOneBase := storeCreditedUpdates(t, service, observer, oneByOneDoc, test.steps, false)
			queued := creditRowsSince(t, observer, queuedDoc, queuedBase)
			oneByOne := creditRowsSince(t, observer, oneByOneDoc, oneByOneBase)
			if !slices.Equal(queued.authors, oneByOne.authors) || queued.owed != oneByOne.owed || queued.lastActor != oneByOne.lastActor {
				t.Errorf("the queued updates left pending authors %q and a pending settlement %t naming %q; one at a time the same updates left %q, %t, %q",
					queued.authors, queued.owed, queued.lastActor, oneByOne.authors, oneByOne.owed, oneByOne.lastActor)
			}
			if got := storedHead(t, observer, queuedDoc) - queuedBase; got != int64(len(test.steps)) {
				t.Errorf("the queued updates are stored as %d rows, want %d", got, len(test.steps))
			}
		})
	}
}

// failDocumentCommits makes every commit that wrote a row of artifactID's doc_updates fail at
// commit, as a commit the database refuses does: the trigger is deferred to the commit, so every
// statement of the transaction succeeds and the commit is what errors.
func failDocumentCommits(t *testing.T, database *store.Store, artifactID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		create function fail_document_commit() returns trigger language plpgsql as $$
		begin
			raise exception 'the test fails this document''s commit';
		end
		$$
	`); err != nil {
		t.Fatalf("create the commit-failing trigger function: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, fmt.Sprintf(`
		create constraint trigger fail_document_commit after insert on doc_updates
		deferrable initially deferred for each row
		when (new.artifact_id = '%s')
		execute function fail_document_commit()
	`, artifactID)); err != nil {
		t.Fatalf("create the commit-failing trigger: %v", err)
	}
}

// A room whose queued updates the store cannot take - the gate refuses them, the room standing
// behind the head another process's append moved, or their commit fails - stores none of them, and
// every one of them ends exactly once: the first one's call fails the room and returns the error,
// and each other meets the failed room on its own call and is dropped, its credit landed, its
// pending and durable counts ended. That holds for the one ygo hands on late, after the failed
// room's eviction has handed its record over to the one registered in its place: it is still
// gated on the instance it was made in, so it is refused rather than stored with no gate at all.
// The three updates are recorded and handed to the room's persistence here as the room's own
// observer and ygo's worker do, so the test decides when each call is made.
func TestAGateRefusalOrFailedCommitStoresNothingOfTheBatchAndEachUpdateEndsOnce(t *testing.T) {
	cases := []struct {
		name string
		// fail makes the store refuse the room's next append.
		fail func(t *testing.T, database *store.Store, artifactID string)
	}{
		{name: "the gate refuses the batch", fail: func(t *testing.T, database *store.Store, artifactID string) {
			other := NewPgVersioned(storetest.SecondProcess(t, database))
			if _, err := other.AppendUpdate(context.Background(), artifactID, paragraphUpdate()); err != nil {
				t.Fatalf("append from the other process: %v", err)
			}
		}},
		{name: "the batch's commit fails", fail: failDocumentCommits},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			database := storetest.Open(t)
			artifactID := createDocument(t, database, "# First")
			service, _ := newTestServiceInstance(t, database)
			seedServiceText(t, service, artifactID, "First.\n")
			settleCurrentGeneration(t, service, artifactID)
			if err := service.warmLiveDocument(ctx, artifactID); err != nil {
				t.Fatalf("load the document's room: %v", err)
			}
			waitForLandedAppends(t, service, artifactID)
			live := service.srv.GetDoc(artifactID)
			instance := service.instanceOf(live)
			if instance == nil {
				t.Fatal("the resident room lists no instance")
			}
			test.fail(t, database, artifactID)
			before := storedHead(t, database, artifactID)

			// The failed room's eviction waits until the update the failure drops has been handed
			// on, so the last update is handed on after the hand-over, as a late call of ygo's is.
			evicting := make(chan struct{})
			letEviction := make(chan struct{})
			var evictOnce, letOnce sync.Once
			release := func() { letOnce.Do(func() { close(letEviction) }) }
			t.Cleanup(release)
			service.beforeFailedEviction = func(room string) {
				if room != artifactID {
					return
				}
				evictOnce.Do(func() { close(evicting) })
				<-letEviction
			}

			bob := model.Actor{Kind: "user", ID: "bob"}
			service.addConnection(artifactID, 11, bob)
			t.Cleanup(func() { service.removeConnection(artifactID, 11) })
			updates := make([][]byte, 3)
			credits := make([]*UpdateCredit, 3)
			for index := range updates {
				_, updates[index] = peerEdit(t, live, appendBlocks(t, fmt.Sprintf("Queued %d.\n", index)))
				credits[index] = service.creditContentChange(artifactID, nil, &instance.hold)
				service.recordUpdateClass(artifactID, updates[index],
					documentUpdateClass{contentChanged: true, durable: true, credit: credits[index], cursor: &instance.cursor})
			}
			failed := service.room(artifactID)
			adapter := &servicePersistenceAdapter{store: service.persistence, service: service}

			if err := adapter.StoreUpdateContext(ctx, artifactID, updates[0]); err == nil {
				t.Error("the first queued update's call returned no error, want the store's refusal")
			}
			if failed.failure() == nil {
				t.Error("the room did not fail on the refused updates")
			}
			if err := adapter.StoreUpdateContext(ctx, artifactID, updates[1]); err != nil {
				t.Errorf("the second queued update's call on the failed room = %v, want it dropped", err)
			}
			select {
			case <-evicting:
			case <-time.After(roomWait(t)):
				t.Fatal("the failed room's eviction never started")
			}
			release()
			if err := service.awaitRoomRecovery(ctx, artifactID); err != nil {
				t.Fatalf("wait for the failed room's eviction: %v", err)
			}
			successor := service.room(artifactID)
			if successor == failed {
				t.Fatal("the failed room's record was kept, so no late call is made after a hand-over")
			}
			// ygo's late call, after the hand-over: refused, its room's failure ending as the first.
			_ = adapter.StoreUpdateContext(ctx, artifactID, updates[2])
			if err := service.awaitRoomRecovery(ctx, artifactID); err != nil {
				t.Fatalf("wait for the record's own eviction: %v", err)
			}

			if head := storedHead(t, database, artifactID); head != before {
				t.Errorf("the store's head is %d after the refused updates, want %d: something of them was stored", head, before)
			}
			for index, credit := range credits {
				credit.state.mu.Lock()
				done := credit.done
				credit.state.mu.Unlock()
				if !done {
					t.Errorf("queued update %d's credit never landed", index)
				}
			}
			for _, record := range []struct {
				name  string
				state *roomState
			}{
				{"the failed room's record", failed},
				{"the record that replaced it", successor},
				{"the document's record now", service.room(artifactID)},
			} {
				record.state.mu.Lock()
				pending, appends, inflight := record.state.pendingUpdates, record.state.durableAppends.Load(), len(record.state.inflight)
				record.state.mu.Unlock()
				if pending != 0 || appends != 0 || inflight != 0 {
					t.Errorf("%s counts %d pending updates, %d durable appends and %d in-flight credits, want none", record.name, pending, appends, inflight)
				}
			}
		})
	}
}

// A room's queue holds more than one room's updates at times: a published live write's update,
// already durable, which the room's persistence must skip by its suppression slot, and an update a
// retired instance of the room made, gated on that instance and not the resident one. Two peers
// type around both, while the first peer's append is held so the rest queue behind it. Every
// update is stored once and the live write's never a second time, and the stored history decodes
// with nothing left pending, every time over many rounds (run it with -race).
func TestABatchStopsAtASuppressedUpdateAndAtAnotherRoomInstance(t *testing.T) {
	// Each round's edits grow the document's history, so a round costs more than the one before:
	// under -race 150 rounds run in about 57 s and 200 in about 110 s.
	const rounds = 150
	ctx := context.Background()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	appends := &burstStore{VersionedStore: NewPgVersioned(database), room: artifactID}
	appends.armed.Store(true)
	service := newShutdownTestService(t, database, appends)
	// Compaction folds the stored rows this test counts; ygo's count-based one is off here.
	service.srv.CompactEvery = 0
	seedServiceText(t, service, artifactID, "Alice 0.\n\nBob 0.\n\nCarol 0.\n\nAgent 0.\n\nRetired 0.\n")
	settleCurrentGeneration(t, service, artifactID)
	alice := model.Actor{Kind: "user", ID: "alice"}
	bob := model.Actor{Kind: "user", ID: "bob"}
	carol := model.Actor{Kind: "user", ID: "carol"}
	agent := model.Actor{Kind: "session", ID: "agent"}
	adapter := &servicePersistenceAdapter{store: service.persistence, service: service}
	step := func(name string, round int) func(*pmdoc.Node) *pmdoc.Node {
		return replaceRun(fmt.Sprintf("%s %d.", name, round), fmt.Sprintf("%s %d.", name, round+1))
	}
	started := time.Now()
	for round := range rounds {
		if err := service.warmLiveDocument(ctx, artifactID); err != nil {
			t.Fatalf("round %d: load the document's room: %v", round, err)
		}
		waitForLandedAppends(t, service, artifactID)
		live := service.srv.GetDoc(artifactID)
		before := storedHead(t, database, artifactID)
		gate := &heldAppendGate{entered: make(chan struct{}), release: make(chan struct{})}
		t.Cleanup(gate.let)
		appends.mu.Lock()
		appends.gates = append(appends.gates, gate)
		appends.mu.Unlock()

		first := typeInRoom(t, service, artifactID, 1, alice, step("Alice", 2*round))
		select {
		case <-gate.entered:
		case <-time.After(roomWait(t)):
			t.Fatalf("round %d: the first peer's append never reached the store", round)
		}
		second := typeInRoom(t, service, artifactID, 2, bob, step("Bob", round))
		if _, err := joinedApplyOps(service, artifactID, []model.EditOp{
			{Op: "replace", Find: fmt.Sprintf("Agent %d.", round), With: fmt.Sprintf("Agent %d.", round+1)},
		}, agent, nil); err != nil {
			t.Fatalf("round %d: the live write: %v", round, err)
		}
		liveRows := storedHead(t, database, artifactID) - before
		third := typeInRoom(t, service, artifactID, 3, carol, step("Carol", round))
		// A retired instance's update: made on the room's document, recorded for an instance
		// that is not the resident one, as one ygo's stranded persistence hands on.
		retiredPeer, retiredUpdate := peerEdit(t, live, step("Retired", round))
		retired := &roomCursor{}
		service.recordUpdateClass(artifactID, retiredUpdate, documentUpdateClass{contentChanged: true, durable: true, cursor: retired})
		fourth := typeInRoom(t, service, artifactID, 1, alice, step("Alice", 2*round+1))
		gate.let()

		state := service.room(artifactID)
		waitFor(t, roomWait(t), "every update but the retired instance's to be stored", func() bool {
			state.mu.Lock()
			defer state.mu.Unlock()
			return len(state.updateClasses) == 1 && state.durableAppends.Load() == 1
		})
		retired.mu.Lock()
		retired.through = storedHead(t, database, artifactID)
		retired.mu.Unlock()
		if err := adapter.StoreUpdateContext(ctx, artifactID, retiredUpdate); err != nil {
			t.Fatalf("round %d: store the retired instance's update: %v", round, err)
		}
		waitForLandedAppends(t, service, artifactID)

		if failure := service.roomFailure(artifactID); failure != nil {
			t.Fatalf("round %d: the room failed: %v", round, failure)
		}
		if got, want := storedHead(t, database, artifactID)-before, liveRows+5; got != want {
			t.Fatalf("round %d: %d rows were stored, want %d: the live write's %d and one for each of the five other updates", round, got, want, liveRows)
		}
		var duplicated int
		if err := database.Pool.QueryRow(ctx, `
			select count(*) from (
				select 1 from doc_updates where artifact_id = $1 and version > $2 group by update having count(*) > 1
			) repeated
		`, artifactID, before).Scan(&duplicated); err != nil {
			t.Fatalf("round %d: read the stored updates: %v", round, err)
		}
		if duplicated != 0 {
			t.Fatalf("round %d: %d updates are stored more than once", round, duplicated)
		}
		loaded, err := NewPgVersioned(database).Load(ctx, artifactID)
		if err != nil {
			t.Fatalf("round %d: load the stored document: %v", round, err)
		}
		stored := newDocumentCopy()
		if err := crdt.ApplyUpdateV1(stored, loaded.Update, nil); err != nil {
			t.Fatalf("round %d: decode the stored document: %v", round, err)
		}
		if pending := stored.PendingStats(); pending.Items != 0 || pending.DeleteRanges != 0 {
			t.Fatalf("round %d: the stored history leaves %d items and %d delete ranges pending", round, pending.Items, pending.DeleteRanges)
		}
		held := stored.StateVector()
		for index, peer := range []*crdt.Doc{first, second, third, retiredPeer, fourth} {
			for client, clock := range peer.StateVector() {
				if held[client] < clock {
					t.Fatalf("round %d: the store does not hold update %d", round, index)
				}
			}
		}
		// The resident room never took the retired instance's update, so it stands behind the head
		// until it reloads, which the next round does.
		if err := service.Evict(ctx, artifactID); err != nil {
			t.Fatalf("round %d: evict the room: %v", round, err)
		}
		waitForNoLiveDocument(t, service, artifactID)
	}
	t.Logf("%d rounds in %s", rounds, time.Since(started).Round(time.Millisecond))
}
