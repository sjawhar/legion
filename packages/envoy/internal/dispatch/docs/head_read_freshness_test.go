package docs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A cold read served from the read cache is as fresh as the head tracker says: it needs no
// database read while the tracker trusts its LISTEN connection, and it still shows another
// process's write within the notification's delivery - with the connection down or half-open, and
// with the notice heard while the read folds - while a process reads its own write at once.

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

// foldGateStore holds a cold read's fold back after it has loaded the stored state and before the
// rendering is cached, once gate is set, and counts the stamp reads cold reads make.
type foldGateStore struct {
	VersionedStore
	gate    atomic.Bool
	folded  chan struct{}
	release chan struct{}
	stamps  atomic.Int64
}

func (s *foldGateStore) LoadDocument(ctx context.Context, room string) (LoadedDocument, error) {
	loaded, err := s.VersionedStore.LoadDocument(ctx, room)
	if s.gate.CompareAndSwap(true, false) {
		s.folded <- struct{}{}
		<-s.release
	}
	return loaded, err
}

func (s *foldGateStore) DocumentStamp(ctx context.Context, room string) (DocumentStamp, error) {
	s.stamps.Add(1)
	return s.VersionedStore.DocumentStamp(ctx, room)
}

// A notice heard while a cold read folds - after the read took its token and loaded the store,
// before it cached the rendering - leaves that rendering to be checked against the store on the
// next read: the read's own hold kept the document's entry, so the notice moved the generation
// on. The next read reads the stamp and returns the other process's write, and the read after it
// is served from the cache again with no stamp read.
func TestANoticeDuringAColdReadsFoldMakesTheNextReadCheckTheStore(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	writer, _ := newTestServiceInstance(t, database)
	seedServiceText(t, writer, artifactID, "Write 0.\n")
	readerStore := storetest.SecondProcess(t, database)
	persistence := &foldGateStore{
		VersionedStore: NewPgVersioned(readerStore),
		folded:         make(chan struct{}),
		release:        make(chan struct{}),
	}
	reader := newReadCacheService(t, readerStore, persistence)
	probe := runProbedEvictor(t, reader, nil)
	probe.awaitDecided(t, database)

	persistence.gate.Store(true)
	type reading struct {
		text string
		err  error
	}
	first := make(chan reading, 1)
	go func() {
		text, err := reader.Text(ctx, artifactID)
		first <- reading{text, err}
	}()
	select {
	case <-persistence.folded:
	case <-time.After(roomWait(t)):
		t.Fatal("the cold read never reached its fold")
	}
	if entries := reader.RoomFigures().HeadTracker.Entries; entries != 1 {
		t.Errorf("%d tracker entries while the cold read folds, want 1: the read holds its document", entries)
	}
	if _, err := joinedApplyOps(writer, artifactID, []model.EditOp{
		{Op: "replace", Find: "Write 0.", With: "Write 1."},
	}, model.Actor{Kind: "session", ID: "agent"}, nil); err != nil {
		t.Fatalf("write on the other service: %v", err)
	}
	probe.awaitDecided(t, database)
	close(persistence.release)
	if got := <-first; got.err != nil || got.text != "Write 0.\n" {
		t.Fatalf("the cold read held in its fold = %q (%v), want the text it loaded before the write", got.text, got.err)
	}
	cached := cacheEntry(t, reader, artifactID)
	if cached == nil {
		t.Fatal("the cold read cached nothing, so nothing here is tested")
	}
	if reader.heads.unchanged(artifactID, cached.heads) {
		t.Fatal("the rendering cached after a notice heard during its fold is still trusted")
	}

	stamps := persistence.stamps.Load()
	if text, err := reader.Text(ctx, artifactID); err != nil || text != "Write 1.\n" {
		t.Fatalf("the read after the fold = %q (%v), want the other service's write", text, err)
	}
	if read := persistence.stamps.Load() - stamps; read != 1 {
		t.Errorf("the read after the fold read the stamp %d times, want once", read)
	}
	stamps = persistence.stamps.Load()
	if text, err := reader.Text(ctx, artifactID); err != nil || text != "Write 1.\n" {
		t.Fatalf("the read after that = %q (%v), want the other service's write", text, err)
	}
	if read := persistence.stamps.Load() - stamps; read != 0 {
		t.Errorf("the read after the store was checked read the stamp %d times, want none: the cache is trusted again", read)
	}
}
