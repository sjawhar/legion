package docs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A process hears every document write over LISTEN, its own included, and what it does with each
// notification is the question here: a notification that tells it nothing new costs no query, and
// a cold read whose cached copy nothing has changed since needs no database read at all, while a
// write of another process, or of an older build that sends only the bare notification, still
// reaches every reader within the notification's delivery.

// evictorProbe watches one service's stale-room evictor (newStaleRoomEvictor): headReads is how
// many times its decisions read a document's head (staleRoomEvictor.readingHead), and delivered is
// the payload of each notification as the listener hands it over (staleRoomEvictor.delivering),
// which hold can keep it from deciding until release.
type evictorProbe struct {
	headReads atomic.Int64
	delivered chan string
	held      atomic.Pointer[chan struct{}]
}

// runProbedEvictor runs service's stale-room evictor until the test ends and returns once its
// LISTEN stands, since a notification sent before that reaches nobody. configure, when set, adjusts
// the evictor before it runs.
func runProbedEvictor(t *testing.T, service *Service, configure func(*staleRoomEvictor)) *evictorProbe {
	t.Helper()
	probe := &evictorProbe{delivered: make(chan string, 4096)}
	evictor := newStaleRoomEvictor(service)
	evictor.readingHead = func(string) { probe.headReads.Add(1) }
	evictor.delivering = func(_, payload string) {
		select {
		case probe.delivered <- payload:
		default:
		}
		if held := probe.held.Load(); held != nil {
			<-*held
		}
	}
	listening := make(chan struct{}, 4)
	evictor.listening = func() {
		select {
		case listening <- struct{}{}:
		default:
		}
	}
	if configure != nil {
		configure(evictor)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		evictor.run(ctx)
	}()
	t.Cleanup(func() {
		probe.release()
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the stale-room evictor did not return within 10s of its context ending")
		}
	})
	select {
	case <-listening:
	case <-time.After(roomWait(t)):
		t.Fatal("the stale-room evictor never listened")
	}
	return probe
}

// hold keeps every notification delivered from now on from being decided until release.
func (p *evictorProbe) hold() {
	held := make(chan struct{})
	p.held.Store(&held)
}

// release lets the notifications hold kept back be decided, and the ones after them.
func (p *evictorProbe) release() {
	if held := p.held.Swap(nil); held != nil {
		close(*held)
	}
}

// awaitDelivered returns once a notification naming artifactID has been handed to the evictor.
func (p *evictorProbe) awaitDelivered(t *testing.T, artifactID string) {
	t.Helper()
	deadline := time.After(roomWait(t))
	for {
		select {
		case payload := <-p.delivered:
			if strings.Contains(payload, artifactID) {
				return
			}
		case <-deadline:
			t.Fatalf("no notification for %s reached the evictor", artifactID)
		}
	}
}

// awaitDecided returns once every notification committed before it has been decided. It sends one
// of its own, for a document nobody has, which Postgres delivers after every notification
// committed before it, and the listener's one goroutine hands it to the evictor only once every
// notification before it has been decided.
func (p *evictorProbe) awaitDecided(t *testing.T, database *store.Store) {
	t.Helper()
	sentinel := uuid.NewString()
	if _, err := database.Pool.Exec(context.Background(), notifyDocumentChangedStatement, sentinel); err != nil {
		t.Fatalf("send the test's own notification: %v", err)
	}
	p.awaitDelivered(t, sentinel)
}

// oldBuildAppend appends update to artifactID as a build of Dispatch that predates every channel
// but documentChangedChannel does: the row, and in the same transaction the bare notification.
func oldBuildAppend(t *testing.T, database *store.Store, artifactID string, update []byte) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the old build's append: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := lockDocumentRoom(ctx, tx, artifactID); err != nil {
		t.Fatalf("lock the document for the old build's append: %v", err)
	}
	var head int64
	if err := tx.QueryRow(ctx, `select coalesce(max(version), 0) from doc_updates where artifact_id = $1`, artifactID).Scan(&head); err != nil {
		t.Fatalf("read the head for the old build's append: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into doc_updates (artifact_id, version, update, content_changed) values ($1, $2, $3, true)
	`, artifactID, head+1, update); err != nil {
		t.Fatalf("append as the old build: %v", err)
	}
	if _, err := tx.Exec(ctx, notifyDocumentChangedStatement, artifactID); err != nil {
		t.Fatalf("notify as the old build: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the old build's append: %v", err)
	}
}

// A process's own writes - a browser typing in its room, an agent's live writes - each notify every
// process, this one included, and this one already knows what each of them wrote: its room's cursor
// took every one of them in. Answering each with a read of the document's head is a query per
// keystroke that can only find the room where its own write left it.
func TestAProcessesOwnNotificationsTriggerNoQuery(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	service, _ := newTestServiceInstance(t, database)
	seedServiceText(t, service, artifactID, "Typing 0.\n\nAgent 0.\n")
	settleCurrentGeneration(t, service, artifactID)
	if err := service.warmLiveDocument(ctx, artifactID); err != nil {
		t.Fatalf("load the document's room: %v", err)
	}
	waitForLandedAppends(t, service, artifactID)
	room := service.srv.GetDoc(artifactID)
	probe := runProbedEvictor(t, service, nil)
	probe.awaitDecided(t, database)
	probe.headReads.Store(0)

	alice := model.Actor{Kind: "user", ID: "alice"}
	agent := model.Actor{Kind: "session", ID: "agent"}
	for round := range 10 {
		for keystroke := range 10 {
			typed := round*10 + keystroke
			typeInRoom(t, service, artifactID, 1, alice, replaceRun(fmt.Sprintf("Typing %d.", typed), fmt.Sprintf("Typing %d.", typed+1)))
		}
		waitForLandedAppends(t, service, artifactID)
		if _, err := joinedApplyOps(service, artifactID, []model.EditOp{
			{Op: "replace", Find: fmt.Sprintf("Agent %d.", round), With: fmt.Sprintf("Agent %d.", round+1)},
		}, agent, nil); err != nil {
			t.Fatalf("live write %d: %v", round, err)
		}
	}
	waitForLandedAppends(t, service, artifactID)
	probe.awaitDecided(t, database)
	// A decision run off the listener's goroutine has had its moment too.
	time.Sleep(100 * time.Millisecond)

	if reads := probe.headReads.Load(); reads != 0 {
		t.Errorf("the evictor read the document's head %d times for this process's own 100 browser updates and 10 live writes, want 0", reads)
	}
	if now := service.srv.GetDoc(artifactID); now != room {
		t.Error("the room was replaced while only this process wrote the document")
	}
}

// A room that already holds the write another process announces - it loaded after that write
// committed and before its notification was decided - stands at the version the notification
// names, and that is a room nothing is wrong with: no query decides it. A write the room does not
// hold still closes it, as the evictor always has.
func TestAnotherProcessesNotificationAtTheRoomsCursorTriggersNoQuery(t *testing.T) {
	ctx := context.Background()
	pair := newProcessPair(t)
	reader, writer := pair.taskA, pair.taskB
	if err := reader.Evict(ctx, pair.artifactID); err != nil {
		t.Fatalf("evict the reader's room: %v", err)
	}
	waitForNoLiveDocument(t, reader, pair.artifactID)
	probe := runProbedEvictor(t, reader, nil)
	probe.awaitDecided(t, pair.database)
	agent := model.Actor{Kind: "session", ID: "agent"}

	probe.hold()
	if _, err := joinedApplyOps(writer, pair.artifactID, []model.EditOp{
		{Op: "insert", After: "end", Markdown: "Written on the other process."},
	}, agent, nil); err != nil {
		t.Fatalf("the other process's write: %v", err)
	}
	probe.awaitDelivered(t, pair.artifactID)
	if err := reader.warmLiveDocument(ctx, pair.artifactID); err != nil {
		t.Fatalf("load the reader's room: %v", err)
	}
	if roomBehind(t, reader, pair.artifactID) {
		t.Fatal("the room loaded after the other process's write stands behind the head, so nothing here is tested")
	}
	room := reader.srv.GetDoc(pair.artifactID)
	probe.headReads.Store(0)
	probe.release()
	probe.awaitDecided(t, pair.database)
	time.Sleep(100 * time.Millisecond)
	if reads := probe.headReads.Load(); reads != 0 {
		t.Errorf("the evictor read the document's head %d times for a write the room already holds, want 0", reads)
	}
	if now := reader.srv.GetDoc(pair.artifactID); now != room {
		t.Error("the room that holds the other process's write was closed for it")
	}

	if _, err := joinedApplyOps(writer, pair.artifactID, []model.EditOp{
		{Op: "insert", After: "end", Markdown: "Written again on the other process."},
	}, agent, nil); err != nil {
		t.Fatalf("the other process's second write: %v", err)
	}
	waitFor(t, roomWait(t), "the room the other process's second write left behind to close", func() bool {
		return reader.srv.GetDoc(pair.artifactID) != room
	})
}

// During a roll a build from before the head channel writes documents too, and it announces each
// write on the bare channel alone. A process that cached a cold read of such a document must not
// go on serving it: the bare notification invalidates the cached read, so the next read checks the
// store and returns the new text - and the notification costs the evictor no read of its own.
func TestAnOldBuildsBareNotificationInvalidatesTheCachedRead(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "one")
	oldBuild := storetest.SecondProcess(t, database)
	writer := newProofHistory(t, 101)
	oldBuildAppend(t, oldBuild, artifactID, writer.write("one\n"))
	oldBuildAppend(t, oldBuild, artifactID, writer.write("two\n"))
	reader := newReadCacheService(t, database, NewPgVersioned(database))
	probe := runProbedEvictor(t, reader, nil)
	if text, err := reader.Text(ctx, artifactID); err != nil || text != "two\n" {
		t.Fatalf("the first cold read = %q (%v), want two", text, err)
	}
	if cacheEntry(t, reader, artifactID) == nil {
		t.Fatal("the cold read cached nothing, so nothing here is tested")
	}
	if reader.srv.GetDoc(artifactID) != nil {
		t.Fatal("the reader holds a room for the document, so its reads are not cold")
	}
	probe.awaitDecided(t, oldBuild)
	probe.headReads.Store(0)

	oldBuildAppend(t, oldBuild, artifactID, writer.write("three\n"))
	probe.awaitDecided(t, oldBuild)
	if text, err := reader.Text(ctx, artifactID); err != nil || text != "three\n" {
		t.Errorf("the cold read after the old build's write = %q (%v), want three", text, err)
	}
	if reads := probe.headReads.Load(); reads != 0 {
		t.Errorf("the evictor read the document's head %d times for the old build's notification, want 0", reads)
	}
}

// A cold read of a document nothing has written since its rendering was cached is served from the
// cache with no database read at all while LISTEN stands: every write that could change it would
// have notified. Writes from another process - a prune to two versions back and two appends of
// other text, which bring the head back to the very version the cache was folded at - reach the
// next read as the new text. With LISTEN down nothing would announce such writes, so every read
// then reads the document's stamp.
func TestACachedColdReadNeedsNoDatabaseReadWhileTheTrackerTrustsIt(t *testing.T) {
	const reads = 100
	ctx := context.Background()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "one")
	secondStore := storetest.SecondProcess(t, database)
	second := newReadCacheService(t, secondStore, NewPgVersioned(secondStore))
	secondVersioned := second.persistence.(*PgVersioned)
	writer := newProofHistory(t, 101)
	one, two, three := writer.write("one\n"), writer.write("two\n"), writer.write("three\n")
	appendUpdates(t, secondVersioned, artifactID, one, two, three)
	reader := newReadCacheService(t, database, NewPgVersioned(database))

	var listenerPID atomic.Uint32
	var listenerDown atomic.Bool
	dials := make(chan struct{}, 64)
	runProbedEvictor(t, reader, func(evictor *staleRoomEvictor) {
		dial := evictor.dial
		evictor.backoff = func(int) time.Duration { return 10 * time.Millisecond }
		evictor.dial = func(ctx context.Context) (*pgx.Conn, error) {
			select {
			case dials <- struct{}{}:
			default:
			}
			if listenerDown.Load() {
				return nil, errors.New("the test keeps the listener down")
			}
			conn, err := dial(ctx)
			if err == nil {
				listenerPID.Store(conn.PgConn().PID())
			}
			return conn, err
		}
	})
	readText := func(want string) {
		t.Helper()
		if text, err := reader.Text(ctx, artifactID); err != nil || text != want {
			t.Fatalf("cold read = %q (%v), want %q", text, err, want)
		}
	}
	rooms, err := database.Pool.Rooms()
	if err != nil {
		t.Fatalf("open the rooms pool: %v", err)
	}

	readText("three\n")
	before := rooms.Stat().AcquireCount()
	for range reads {
		readText("three\n")
	}
	if acquired := rooms.Stat().AcquireCount() - before; acquired != 0 {
		t.Errorf("%d cold reads of an unchanged document took %d connections of the rooms pool after the first read, want 0", reads, acquired)
	}

	if err := secondVersioned.PruneAfter(ctx, artifactID, 1, nil); err != nil {
		t.Fatalf("prune to version 1 from the second service: %v", err)
	}
	rewriter := newProofHistory(t, 202, one)
	appendUpdates(t, secondVersioned, artifactID, rewriter.write("four\n"), rewriter.write("five\n"))
	if head := storedHead(t, database, artifactID); head != 3 {
		t.Fatalf("the history stands at %d after the prune and the appends, want 3, the version the cached read was folded at", head)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		text, err := reader.Text(ctx, artifactID)
		if err != nil {
			t.Fatalf("cold read after the second service's writes: %v", err)
		}
		if text == "five\n" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cold reads still return %q 10 s after the second service's prune and appends, want five", text)
		}
		time.Sleep(time.Millisecond)
	}

	listenerDown.Store(true)
	for len(dials) > 0 {
		<-dials
	}
	if _, err := database.Pool.Exec(ctx, `select pg_terminate_backend($1)`, int64(listenerPID.Load())); err != nil {
		t.Fatalf("kill the evictor's LISTEN connection: %v", err)
	}
	select {
	case <-dials:
	case <-time.After(roomWait(t)):
		t.Fatal("the evictor never redialled once its LISTEN connection was killed")
	}
	before = rooms.Stat().AcquireCount()
	for range reads {
		readText("five\n")
	}
	if acquired := rooms.Stat().AcquireCount() - before; acquired < reads {
		t.Errorf("%d cold reads with LISTEN down took %d connections of the rooms pool, want one stamp read each", reads, acquired)
	}
}

// A cold read on one process sees a write another process committed within the notification's
// delivery: each of 200 writes on one service is read on the other until it shows, and the delay
// from the write's commit to the first read that shows it is the bound that freshness rests on.
func TestAColdReadOnOneServiceSeesAnotherServicesWriteWithinTheNotificationDelay(t *testing.T) {
	const writes = 200
	ctx := context.Background()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	writer, _ := newTestServiceInstance(t, database)
	readerStore := storetest.SecondProcess(t, database)
	reader := newReadCacheService(t, readerStore, NewPgVersioned(readerStore))
	seedServiceText(t, writer, artifactID, "Write 0.\n")
	runProbedEvictor(t, reader, nil)
	if text, err := reader.Text(ctx, artifactID); err != nil || text != "Write 0.\n" {
		t.Fatalf("the first cold read = %q (%v), want the seeded text", text, err)
	}
	agent := model.Actor{Kind: "session", ID: "agent"}

	delays := make([]time.Duration, 0, writes)
	for write := range writes {
		if _, err := joinedApplyOps(writer, artifactID, []model.EditOp{
			{Op: "replace", Find: fmt.Sprintf("Write %d.", write), With: fmt.Sprintf("Write %d.", write+1)},
		}, agent, nil); err != nil {
			t.Fatalf("write %d: %v", write, err)
		}
		committed := time.Now()
		want := fmt.Sprintf("Write %d.\n", write+1)
		for {
			text, err := reader.Text(ctx, artifactID)
			if err != nil {
				t.Fatalf("write %d: cold read on the other service: %v", write, err)
			}
			if text == want {
				break
			}
			if time.Since(committed) > 10*time.Second {
				t.Fatalf("write %d: the other service still reads %q 10 s after the write committed, want %q", write, text, want)
			}
			time.Sleep(time.Millisecond)
		}
		delays = append(delays, time.Since(committed))
	}
	slices.Sort(delays)
	t.Logf("delay from a write's commit to the other service's cold read of it over %d writes: p50 %s, p99 %s, max %s",
		writes, delays[len(delays)/2], delays[len(delays)*99/100], delays[len(delays)-1])
}

// A process reads its own write through the read cache at once, without waiting for the
// notification that announces it: a live write committed, and a cold read on the same service
// straight after it returns the written text, every time, while every notification is held back
// from the service's evictor.
func TestAProcessReadsItsOwnWriteThroughTheReadCache(t *testing.T) {
	const writes = 200
	ctx := context.Background()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	service, _ := newTestServiceInstance(t, database)
	seedServiceText(t, service, artifactID, "Write 0.\n")
	probe := runProbedEvictor(t, service, nil)
	probe.awaitDecided(t, database)
	probe.hold()
	coldRead := func(want string) {
		t.Helper()
		if err := service.Evict(ctx, artifactID); err != nil {
			t.Fatalf("evict the room the write published into: %v", err)
		}
		waitForNoLiveDocument(t, service, artifactID)
		if text, err := service.Text(ctx, artifactID); err != nil || text != want {
			t.Fatalf("cold read = %q (%v), want %q", text, err, want)
		}
	}
	coldRead("Write 0.\n")
	if cacheEntry(t, service, artifactID) == nil {
		t.Fatal("the cold read cached nothing, so nothing here is tested")
	}
	agent := model.Actor{Kind: "session", ID: "agent"}
	for write := range writes {
		if _, err := joinedApplyOps(service, artifactID, []model.EditOp{
			{Op: "replace", Find: fmt.Sprintf("Write %d.", write), With: fmt.Sprintf("Write %d.", write+1)},
		}, agent, nil); err != nil {
			t.Fatalf("write %d: %v", write, err)
		}
		coldRead(fmt.Sprintf("Write %d.\n", write+1))
	}
}
