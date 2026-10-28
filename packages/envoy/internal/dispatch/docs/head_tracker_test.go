package docs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
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

// A cold read on a process whose document LISTEN connection has gone half-open - its socket still
// up, delivering nothing and reporting no error, as a dropped NAT entry or a failover leaves it -
// still sees another process's write within the notification delay the test above allows, and
// does not serve its cached copy until the listener's idle ping finds the connection dead. The head
// tracker trusts itself while the listener has a connection; a half-open one is a connection that
// has stopped proving anything. On a real Dispatch pair, with the reader's document LISTEN backend
// stopped and every other connection of it working, the reader served the old text for 31.4 s
// after the other process's edit committed, where the build before this change read it at once.
func TestAColdReadSeesAnotherServicesWriteWhileItsListenConnectionIsHalfOpen(t *testing.T) {
	const bound = 10 * time.Second
	ctx := context.Background()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	writer, _ := newTestServiceInstance(t, database)
	readerStore := storetest.SecondProcess(t, database)
	reader := newReadCacheService(t, readerStore, NewPgVersioned(readerStore))
	seedServiceText(t, writer, artifactID, "Write 0.\n")
	connConfig := readerStore.Pool.Config().ConnConfig
	proxy := newSilencingProxy(t, net.JoinHostPort(connConfig.Host, strconv.Itoa(int(connConfig.Port))))
	runProbedEvictor(t, reader, func(evictor *staleRoomEvictor) {
		evictor.dial = func(ctx context.Context) (*pgx.Conn, error) {
			config := connConfig.Copy()
			config.Host, config.Port, config.Fallbacks = "127.0.0.1", proxy.port, nil
			return pgx.ConnectConfig(ctx, config)
		}
	})
	if text, err := reader.Text(ctx, artifactID); err != nil || text != "Write 0.\n" {
		t.Fatalf("the first cold read = %q (%v), want the seeded text", text, err)
	}
	if cacheEntry(t, reader, artifactID) == nil {
		t.Fatal("the cold read cached nothing, so nothing here is tested")
	}

	proxy.silence()
	if _, err := joinedApplyOps(writer, artifactID, []model.EditOp{
		{Op: "replace", Find: "Write 0.", With: "Write 1."},
	}, model.Actor{Kind: "session", ID: "agent"}, nil); err != nil {
		t.Fatalf("write on the other service: %v", err)
	}
	committed := time.Now()
	for {
		text, err := reader.Text(ctx, artifactID)
		if err != nil {
			t.Fatalf("cold read on the reader: %v", err)
		}
		if text == "Write 1.\n" {
			return
		}
		if time.Since(committed) > bound {
			t.Fatalf("the reader still reads %q %s after the other service's write committed while its LISTEN connection was half-open, want %q within %s: the head tracker kept trusting a connection that had stopped delivering", text, time.Since(committed).Round(time.Millisecond), "Write 1.\n", bound)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The evictor's liveness ping keeps the head tracker trusting a healthy LISTEN connection, and the
// tracker's trust protects only what this process answers without the database: a resident room,
// a cached cold read, a preload. A process holding none of those has nothing the ping protects, so
// it sends none. On a real Dispatch pair left idle with no room, no cached read and no tracker
// entry, the two processes sent 118 pings a minute (240 with four processes), where the build
// before the ping sent none.
func TestAServiceWithNothingTheTrackerProtectsSendsNoPing(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service, _ := newTestServiceInstance(t, database)
	listen, err := pgx.ConnectConfig(ctx, database.Pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatalf("connect the ping listener: %v", err)
	}
	t.Cleanup(func() { _ = listen.Close(context.Background()) })
	if _, err := listen.Exec(ctx, "listen "+documentPingChannel); err != nil {
		t.Fatalf("listen on %s: %v", documentPingChannel, err)
	}
	runProbedEvictor(t, service, nil)
	if live := len(service.srv.Rooms()); live != 0 {
		t.Fatalf("%d rooms are live, so the service holds something and nothing here is tested", live)
	}

	window := headTrustWindow + 2*headPingAfter
	deadline := time.Now().Add(window)
	pings := 0
	for {
		waitCtx, cancel := context.WithDeadline(ctx, deadline)
		_, err := listen.WaitForNotification(waitCtx)
		cancel()
		if err != nil {
			break
		}
		pings++
	}
	if figures := service.RoomFigures(); figures.ResidentRooms != 0 || figures.ReadCache.Entries != 0 || figures.Preloads != 0 || figures.HeadTracker.Entries != 0 {
		t.Fatalf("the service came to hold something during the window (%+v), so nothing here is tested", figures)
	}
	if pings != 0 {
		t.Errorf("a service with no room, no cached read, no preload and no tracker entry sent %d pings on %s in %s, want none: the ping protects nothing there and costs a shared-pool connection and a NOTIFY to every listener each second", pings, documentPingChannel, window)
	}
}

// silencingProxy forwards each TCP connection it accepts to target until silence, after which the
// connections it already holds forward nothing and neither end of them is closed: each looks open
// to both ends and stays silent, a half-open connection. A connection accepted after silence
// forwards as usual, so a listener that redials gets a working connection.
type silencingProxy struct {
	listener net.Listener
	port     uint16
	target   string
	mu       sync.Mutex
	gates    []*atomic.Bool
	conns    []net.Conn
}

func newSilencingProxy(t *testing.T, target string) *silencingProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the proxy: %v", err)
	}
	proxy := &silencingProxy{listener: listener, port: uint16(listener.Addr().(*net.TCPAddr).Port), target: target}
	go proxy.serve()
	t.Cleanup(func() {
		_ = listener.Close()
		proxy.mu.Lock()
		defer proxy.mu.Unlock()
		for _, conn := range proxy.conns {
			_ = conn.Close()
		}
	})
	return proxy
}

func (p *silencingProxy) serve() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		server, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = client.Close()
			continue
		}
		gate := &atomic.Bool{}
		p.mu.Lock()
		p.gates = append(p.gates, gate)
		p.conns = append(p.conns, client, server)
		p.mu.Unlock()
		go func() { _, _ = io.Copy(silencedWriter{to: server, gate: gate}, client) }()
		go func() { _, _ = io.Copy(silencedWriter{to: client, gate: gate}, server) }()
	}
}

// silence stops every connection the proxy holds from forwarding, closing neither end.
func (p *silencingProxy) silence() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, gate := range p.gates {
		gate.Store(true)
	}
}

// silencedWriter writes to to until gate is set, and from then on drops what it is given.
type silencedWriter struct {
	to   net.Conn
	gate *atomic.Bool
}

func (w silencedWriter) Write(p []byte) (int, error) {
	if w.gate.Load() {
		return len(p), nil
	}
	return w.to.Write(p)
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

// The head tracker trusts itself only while its LISTEN connection goes on delivering: a connection
// that stands and carries nothing - half-open - says nothing of the writes committed since. Trust
// lapses headTrustWindow after the last delivery, with no event to end it, and returns with the
// next delivery, which arrives after every notification committed before it. No database: the
// tracker's clock is the test's.
func TestTheHeadTrackerStopsTrustingAListenConnectionThatHasDeliveredNothingForTheTrustWindow(t *testing.T) {
	const room, head = "room", int64(7)
	clock := time.Unix(1_000_000, 0)
	tracker := newHeadTracker()
	tracker.now = func() time.Time { return clock }
	tracker.connected()
	tracker.delivered()
	tracker.observe(room, head)
	trusts := func(when string, want bool) {
		t.Helper()
		if got, known := tracker.head(room); known != want || (want && got != head) {
			t.Errorf("%s: head = %d, %v, want %d, %v", when, got, known, head, want)
		}
		if token := tracker.token(room); token.trusted != want {
			t.Errorf("%s: token trusted = %v, want %v", when, token.trusted, want)
		}
		if figures := tracker.figures(); figures.Trusted != want {
			t.Errorf("%s: figures trusted = %v, want %v", when, figures.Trusted, want)
		}
	}

	trusts("at the delivery", true)
	clock = clock.Add(headTrustWindow - time.Millisecond)
	trusts("just inside the trust window", true)
	if quiet, listening := tracker.quiet(); quiet != headTrustWindow-time.Millisecond || !listening {
		t.Errorf("quiet = %s, %v, want %s, true", quiet, listening, headTrustWindow-time.Millisecond)
	}
	token := tracker.token(room)

	clock = clock.Add(time.Millisecond)
	trusts("the trust window after the last delivery", false)
	if tracker.unchanged(room, token) {
		t.Error("a token taken while trusted is unchanged once the trust window has passed with nothing delivered")
	}

	tracker.delivered()
	trusts("at the next delivery", true)
	if !tracker.unchanged(room, token) {
		t.Error("a token taken while trusted is not unchanged at the next delivery, though nothing wrote the document")
	}
	clock = clock.Add(headTrustWindow)
	trusts("the trust window after that delivery", false)
}
