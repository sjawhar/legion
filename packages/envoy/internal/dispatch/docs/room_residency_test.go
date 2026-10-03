package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A room that only ever receives valid writes is never refused and never loses one while readers,
// versions and peers come and go around it.
//
// Reads: the socket's admission check, `/text`, a version's capture (`POST /versions`) and a read
// outside any transaction (docView) read a copy of the resident room taken under its lock
// (snapshotDocument), never its live tree halfway through a write, which they would read as a tree
// outside the schema - the socket closed with documentSchemaCloseCode, the read or the version
// answered 409 with the repair. Under -race the direct walk of the live tree is also a data race
// with the writer.
//
// Writes: a peer joins and leaves over and over, so the room's last peer leaves while the writer
// is inside its Server.Apply. The room must not be evicted under that write (roomIdleTimeout):
// eagerly evicted, the write lands on the evicted room, the next access loads the store without
// it, and the next write is made from a document missing the first. The two writes then merge into
// a document neither wrote, which reads back with no block at all once each kept a block the other
// replaced. Every load of the room is checked against the writes its earlier instances took
// (watchRoomLoads), and the document the store holds at the end is the last one written.
func TestAHealthyRoomUnderWritesIsNeverRefused(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	loads := watchRoomLoads(service)
	seedServiceText(t, service, artifactID, "before")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	headers := http.Header{"X-Dispatch-User": []string{"alice"}}
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID

	stop := make(chan struct{})
	var peers sync.WaitGroup
	peers.Add(1)
	go func() {
		defer peers.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			connection, _, err := gws.DefaultDialer.Dial(wsURL, headers)
			if err != nil {
				continue
			}
			connection.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			for {
				if _, _, err := connection.ReadMessage(); err != nil {
					break
				}
			}
			_ = connection.Close()
		}
	}()

	bodies := []string{
		"- one\n- two\n  - nested\n\n> quoted\n",
		"> a quote\n>\n> - with a list\n\n1. first\n2. second\n",
		"# Title\n\n- [ ] task\n- [x] done\n\nTail.\n",
	}
	alice := model.Actor{Kind: "user", ID: "alice"}
	type writerResult struct {
		writes int
		last   string
		err    error
	}
	written := make(chan writerResult, 1)
	go func() {
		var result writerResult
		for round := 0; ; round++ {
			select {
			case <-stop:
				written <- result
				return
			default:
			}
			canonical, err := service.ReplaceText(context.Background(), artifactID, bodies[round%len(bodies)], alice)
			if err != nil {
				result.err = fmt.Errorf("write round %d: %w", round, err)
				written <- result
				return
			}
			result.writes++
			result.last = canonical
		}
	}()

	version := func() error {
		ctx := context.Background()
		tx, err := service.store.Pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		joined, ledger := service.Join(ctx, tx)
		defer ledger.Discard()
		_, err = service.SnapshotVersion(joined, artifactID, alice)
		return err
	}
	var rounds, sockets atomic.Int64
	var refusalsMu sync.Mutex
	var refusals []string
	refuse := func(format string, args ...any) {
		refusalsMu.Lock()
		defer refusalsMu.Unlock()
		refusals = append(refusals, fmt.Sprintf(format, args...))
	}
	// Three seconds and forty rounds, whichever comes later, within thirty seconds: a loaded machine
	// takes longer over each round.
	start := time.Now()
	running := func() bool {
		elapsed := time.Since(start)
		return elapsed < 30*time.Second && (elapsed < 3*time.Second || rounds.Load() < 40)
	}
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for running() {
				round := rounds.Add(1)
				if _, _, err := service.TextWithToken(context.Background(), artifactID); err != nil {
					refuse("read %d: %v", round, err)
				}
				if _, err := service.currentToken(context.Background(), artifactID); err != nil {
					refuse("unjoined read %d: %v", round, err)
				}
				if err := version(); err != nil {
					refuse("version %d: %v", round, err)
				}
				connection, response, err := gws.DefaultDialer.Dial(wsURL, headers)
				if err != nil {
					refuse("socket %d: response=%#v err=%v", round, response, err)
					continue
				}
				connection.SetReadDeadline(time.Now().Add(time.Second))
				if _, _, err := connection.ReadMessage(); err != nil {
					refuse("socket %d: first frame %v", round, err)
				}
				_ = connection.Close()
				sockets.Add(1)
			}
		}()
	}
	readers.Wait()
	close(stop)
	peers.Wait()
	result := <-written
	if result.err != nil {
		t.Fatal(result.err)
	}
	if len(refusals) != 0 {
		t.Errorf("a healthy room under writes refused %d of its reads, versions and sockets: %s", len(refusals), strings.Join(refusals, "; "))
	}
	if missed := loads.missedWrites(); len(missed) != 0 {
		t.Errorf("%d of %d loads of the room started without a write another instance of it had taken: %s", len(missed), loads.count(), strings.Join(missed, "; "))
	}
	if rounds.Load() < 20 || result.writes < 20 {
		t.Fatalf("%d rounds of reads and versions, %d sockets and %d writes, too few to say anything", rounds.Load(), sockets.Load(), result.writes)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := service.waitForPendingUpdates(ctx, artifactID); err != nil {
		t.Fatalf("wait for the room's updates to reach persistence: %v", err)
	}
	if err := service.waitForDurableAppends(ctx, artifactID); err != nil {
		t.Fatalf("wait for the room's durable appends: %v", err)
	}
	stored, err := service.persistence.Load(ctx, artifactID)
	if err != nil {
		t.Fatal(err)
	}
	durable := crdt.New()
	if err := crdt.ApplyUpdateV1(durable, stored.Update, nil); err != nil {
		t.Fatal(err)
	}
	if markdown, err := renderDocument(durable); err != nil || markdown != result.last {
		t.Errorf("the stored document after %d writes = %q (%v), want the last one written, %q", result.writes, markdown, err, result.last)
	}
	t.Logf("%d rounds of reads, unjoined reads, versions and sockets and %d writes on a room whose peers came and went, over %d loads of it", rounds.Load(), result.writes, loads.count())
}

// A write inside a Server.Apply when the room's last peer leaves stays in the room the next write
// is made against. Evicted eagerly under it, the room takes the write after its successor has
// loaded the store without it; the next write is then a diff of the document the first replaced,
// and the store holds both writes' text. The write here waits inside its Apply while the peer
// leaves and a read loads the room again (roomIdleTimeout).
func TestAWriteInsideApplyOutlivesTheRoomsLastPeer(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	peer, response, err := gws.DefaultDialer.Dial("ws"+strings.TrimPrefix(httpServer.URL, "http")+"/ws/doc/"+artifactID, http.Header{"X-Dispatch-User": []string{"alice"}})
	if err != nil {
		t.Fatalf("connect peer: response=%#v err=%v", response, err)
	}
	peer.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := peer.ReadMessage(); err != nil {
		t.Fatalf("peer's first frame: %v", err)
	}
	write := func(markdown string, inside func()) error {
		tree, err := parseInput(markdown)
		if err != nil {
			return err
		}
		var updateErr error
		err = service.srv.Apply(context.Background(), artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
			inside()
			fragment := doc.GetXmlFragment(fragmentName)
			transact(func(txn *crdt.Transaction) { updateErr = pmdoc.Update(txn, fragment, tree) })
		})
		return errors.Join(updateErr, err)
	}

	entered, proceed := make(chan struct{}), make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- write("first", func() {
			close(entered)
			<-proceed
		})
	}()
	<-entered
	if err := peer.Close(); err != nil {
		t.Fatal(err)
	}
	// The server has handled the departure, and whatever it does when a room's last peer leaves,
	// once the socket's handler returns (Service.ServeHTTP removes the connection after that).
	state := service.room(artifactID)
	for deadline := time.Now().Add(5 * time.Second); ; {
		state.mu.Lock()
		connected := len(state.connected)
		state.mu.Unlock()
		if connected == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the peer's departure was not handled")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := service.currentToken(context.Background(), artifactID); err != nil {
		t.Fatalf("read the room while the write waits: %v", err)
	}
	close(proceed)
	if err := <-first; err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := write("second", func() {}); err != nil {
		t.Fatalf("second write: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := service.waitForPendingUpdates(ctx, artifactID); err != nil {
		t.Fatalf("wait for the room's updates to reach persistence: %v", err)
	}
	if err := service.waitForDurableAppends(ctx, artifactID); err != nil {
		t.Fatalf("wait for the room's durable appends: %v", err)
	}
	if text, err := service.Text(ctx, artifactID); err != nil || text != "second\n" {
		t.Errorf("the room after both writes = %q (%v), want \"second\\n\"", text, err)
	}
	stored, err := service.persistence.Load(ctx, artifactID)
	if err != nil {
		t.Fatal(err)
	}
	durable := crdt.New()
	if err := crdt.ApplyUpdateV1(durable, stored.Update, nil); err != nil {
		t.Fatal(err)
	}
	if markdown, err := renderDocument(durable); err != nil || markdown != "second\n" {
		t.Errorf("the stored document after both writes = %q (%v), want \"second\\n\"", markdown, err)
	}
}

// roomLoads checks every load of one room's document against the writes its earlier instances
// took: a load whose state lacks one of them started from a document another instance had
// already moved past.
type roomLoads struct {
	mu     sync.Mutex
	taken  map[crdt.ClientID]uint64 // past the highest clock any instance's update inserted
	loads  int
	missed []string
}

// watchRoomLoads records each document service loads from here on, and each update a loaded
// document then takes.
func watchRoomLoads(service *Service) *roomLoads {
	watch := &roomLoads{taken: make(map[crdt.ClientID]uint64)}
	load := service.srv.OnLoadDocument
	service.srv.OnLoadDocument = func(ctx context.Context, room string, doc *crdt.Doc) error {
		if err := load(ctx, room, doc); err != nil {
			return err
		}
		watch.loaded(doc)
		doc.OnUpdate(func(update []byte, _ any) { watch.took(update) })
		return nil
	}
	return watch
}

func (w *roomLoads) loaded(doc *crdt.Doc) {
	state := doc.StateVector()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.loads++
	for client, end := range w.taken {
		if state[client] < end {
			w.missed = append(w.missed, fmt.Sprintf("load %d lacked clocks %d-%d of client %d", w.loads, state[client], end, client))
		}
	}
}

func (w *roomLoads) took(update []byte) {
	ids, err := crdt.ContentIDsFromUpdateV1(update)
	if err != nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, client := range ids.Inserts.Clients() {
		for _, inserted := range ids.Inserts.Ranges(client) {
			if end := inserted.Clock + inserted.Len; end > w.taken[client] {
				w.taken[client] = end
			}
		}
	}
}

func (w *roomLoads) missedWrites() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.missed...)
}

func (w *roomLoads) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.loads
}

// The room's update observer renders a copy of the room brought up to date under the room's lock
// (renderedReplica), never the live tree. ygo fires the observer once the transaction has released
// the document's lock, so another writer can be integrating into the live tree while a render walks
// it: the torn walk reads a healthy document as one outside the schema, logs a false "updated
// document outside Proof schema" WARN, and counts the update as a content change it may not have
// been. Two writers stand in for two browser peers (ygo applies a peer's update in that peer's
// goroutine and fires the observer there), and a third is the service projecting comment records.
// Under -race the walk of the live tree is a data race with the other writers.
func TestTheUpdateObserverNeverRendersAWriteHalfWay(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	logs := &lockedLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	var trees []*pmdoc.Node
	for _, body := range []string{
		"- one\n- two\n  - nested\n\n> quoted\n",
		"> a quote\n>\n> - with a list\n\n1. first\n2. second\n",
		"# Title\n\n- [ ] task\n- [x] done\n\nTail.\n",
	} {
		tree, err := parseInput(body)
		if err != nil {
			t.Fatal(err)
		}
		trees = append(trees, tree)
	}
	ctx := context.Background()
	bob := model.Actor{Kind: "user", ID: "bob"}
	deadline := time.Now().Add(2 * time.Second)
	failures := make(chan error, 3)
	var writes, projections atomic.Int64
	var writers sync.WaitGroup
	peer := func(first int) {
		defer writers.Done()
		for round := first; time.Now().Before(deadline); round++ {
			tree := trees[round%len(trees)]
			var updateErr error
			err := service.srv.Apply(ctx, artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
				fragment := doc.GetXmlFragment(fragmentName)
				transact(func(txn *crdt.Transaction) { updateErr = pmdoc.Update(txn, fragment, tree) })
			})
			if err = errors.Join(updateErr, err); err != nil && !errors.Is(err, ygws.ErrNoChanges) {
				failures <- fmt.Errorf("peer write %d: %w", round, err)
				return
			}
			writes.Add(1)
		}
	}
	writers.Add(3)
	go peer(0)
	go peer(1)
	go func() {
		defer writers.Done()
		for round := 0; time.Now().Before(deadline); round++ {
			if err := service.ProjectMark(ctx, artifactID, fmt.Sprintf("projection-%d", round), MarkRecord{
				Kind: "comment", By: "user:bob", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Text: "note",
			}, bob); err != nil {
				failures <- fmt.Errorf("projection %d: %w", round, err)
				return
			}
			projections.Add(1)
		}
	}()
	writers.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	if writes.Load() < 20 || projections.Load() < 5 {
		t.Fatalf("%d peer writes and %d projections, too few to say anything", writes.Load(), projections.Load())
	}

	if logged := logs.String(); logged != "" {
		t.Errorf("a healthy room under concurrent writes logged:\n%s", logged)
	}
	state := service.room(artifactID)
	state.mu.Lock()
	content := state.contentMarkdown
	state.mu.Unlock()
	live, err := snapshotDocument(service.srv.GetDoc(artifactID))
	if err != nil {
		t.Fatal(err)
	}
	want, err := renderDocument(live)
	if err != nil {
		t.Fatal(err)
	}
	if content == nil {
		t.Errorf("the observer's last rendering is unset, want the room's %q", want)
	} else if *content != want {
		t.Errorf("the observer's last rendering = %q, want the room's %q", *content, want)
	}
	t.Logf("%d peer writes and %d projections", writes.Load(), projections.Load())
}

// lockedLog collects what every goroutine logs.
type lockedLog struct {
	mu      sync.Mutex
	written strings.Builder
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.written.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.written.String()
}
