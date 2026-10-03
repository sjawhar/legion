package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
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

// A document only the API touches - an agent's edit, a read outside any transaction - has no
// browser to leave its room. ygo stamps such a room idle when the Server.Apply that touched it
// returns, so its idle sweep evicts the room within the idle timeout and a sweep of the API's last
// touch, as it evicts a room its last browser left (roomIdleTimeout), and the document reads back
// what the API wrote.
func TestARoomOnlyTheAPITouchesLeavesWithinTheIdleTimeout(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	// Production waits roomIdleTimeout; two seconds keeps the test short, and ygo then sweeps
	// every second. No room is loaded yet, so ygo's sweeper has not read it.
	service.srv.RoomIdleTimeout = 2 * time.Second
	seedServiceText(t, service, artifactID, "before")
	ctx := context.Background()
	agent := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}

	resident := func() bool { return slices.Contains(service.srv.Rooms(), artifactID) }
	leaves := func(touch string) {
		t.Helper()
		if !resident() {
			t.Fatalf("the %s did not load the document's room", touch)
		}
		waitFor(t, 10*time.Second, fmt.Sprintf("ygo's idle sweep to evict the document's room after the %s, the API's last touch (idle timeout %v)", touch, service.srv.RoomIdleTimeout), func() bool {
			return !resident()
		})
	}

	written, err := service.ReplaceText(ctx, artifactID, "Edited by an agent.\n", agent)
	if err != nil {
		t.Fatalf("agent edit: %v", err)
	}
	leaves("agent's edit")

	if _, err := service.currentToken(ctx, artifactID); err != nil {
		t.Fatalf("read outside any transaction: %v", err)
	}
	leaves("read outside any transaction")

	if text, err := service.Text(ctx, artifactID); err != nil || text != written {
		t.Errorf("the document after its room was evicted twice = %q (%v), want the agent's edit, %q", text, err, written)
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

// newRetiringRoomService is newTestService on its own database, with a Shutdown cleanup bounded by
// a deadline: a publish wedged on its own room would hold an unbounded cleanup, and the test binary
// with it, for good.
func newRetiringRoomService(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	database := storetest.Open(t)
	service := New(Deps{
		Store:     database,
		Events:    events.NewBroker(),
		Identity:  identity.HeaderIdentity{Header: "X-Dispatch-User", AllowedLogins: map[string]struct{}{"alice": {}}},
		ServerURL: "https://dispatch.example",
		Settle:    time.Hour,
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := service.Shutdown(ctx); err != nil {
			t.Logf("shutdown document service: %v", err)
		}
	})
	return service, database
}

// A committed write's publish (publishLiveUpdate) applies the write to its room inside a
// Server.Apply. CloseRoom ignores an Apply in flight - SetIssueClosed's, Shutdown's for a room with
// a connected editor, Evict's - and retires the room's persistence worker under it, so ygo hands the
// commit's update to stranded persistence on the publishing goroutine itself, whose StoreUpdate
// consumes the publish's suppression slot. The room's own update observer finishes that slot before
// ygo's persistence observer runs; a slot finished only once Apply returned is one the publishing
// goroutine waits on for good, holding the request that committed the write, the document's writer
// slot, and every later room's persistence of the document, which waits behind the stranded slot.
// The test pauses the commit in an observer between those two, retires the worker there, and lets
// the commit go on. A failed room's eviction, which cancels the room's slots first, is a control.
func TestAPublishSurvivesItsRoomsWorkerRetiringUnderIt(t *testing.T) {
	for _, test := range []struct {
		name   string
		peer   bool
		retire func(service *Service, artifactID string) error
	}{
		{"control: nothing closes the room", false, func(*Service, string) error { return nil }},
		{"CloseRoom closes the room", false, func(service *Service, artifactID string) error {
			return service.srv.CloseRoom(artifactID, true)
		}},
		{"SetIssueClosed closes the room", false, func(service *Service, _ string) error {
			service.SetIssueClosed(context.Background(), "DOC-1", true)
			return nil
		}},
		{"Evict closes the room", false, func(service *Service, artifactID string) error {
			return service.Evict(context.Background(), artifactID)
		}},
		{"control: a failed room is evicted", false, func(service *Service, artifactID string) error {
			service.failRoom(artifactID, errors.New("test: room failed"))
			return service.awaitRoomRecovery(context.Background(), artifactID)
		}},
		// Shutdown closes a room with a connected editor first (CloseRoom), then waits on the paused
		// write's durable append, which this test holds, until its budget ends.
		{"Shutdown with a connected editor", true, func(service *Service, _ string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			_ = service.Shutdown(ctx)
			return nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, database := newRetiringRoomService(t)
			artifactID := createDocument(t, database, "# First")
			// Pause the publish's commit in the room's update observers: after the service's own,
			// which OnLoadDocument registers, and before ygo's persistence observer, registered after.
			committed, proceed := make(chan struct{}), make(chan struct{})
			var once sync.Once
			load := service.srv.OnLoadDocument
			service.srv.OnLoadDocument = func(ctx context.Context, room string, doc *crdt.Doc) error {
				if err := load(ctx, room, doc); err != nil {
					return err
				}
				doc.OnUpdate(func(_ []byte, origin any) {
					if _, published := origin.(*liveWriteOrigin); published {
						once.Do(func() {
							close(committed)
							<-proceed
						})
					}
				})
				return nil
			}
			seedServiceText(t, service, artifactID, "before")
			if test.peer {
				httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
				t.Cleanup(httpServer.Close)
				peer, response, err := gws.DefaultDialer.Dial("ws"+strings.TrimPrefix(httpServer.URL, "http")+"/ws/doc/"+artifactID, http.Header{"X-Dispatch-User": []string{"alice"}})
				if err != nil {
					t.Fatalf("connect peer: response=%#v err=%v", response, err)
				}
				t.Cleanup(func() { _ = peer.Close() })
				peer.SetReadDeadline(time.Now().Add(5 * time.Second))
				if _, _, err := peer.ReadMessage(); err != nil {
					t.Fatalf("peer's first frame: %v", err)
				}
				go func() {
					for {
						if _, _, err := peer.ReadMessage(); err != nil {
							return
						}
					}
				}()
			}

			written := make(chan error, 1)
			go func() {
				ctx := context.Background()
				tx, err := service.store.Pool.Begin(ctx)
				if err != nil {
					written <- err
					return
				}
				defer tx.Rollback(ctx)
				joined, ledger := service.Join(ctx, tx)
				defer ledger.Discard()
				if _, err := service.ReplaceText(joined, artifactID, "after", model.Actor{Kind: "user", ID: "alice"}); err != nil {
					written <- err
					return
				}
				written <- ledger.Commit(ctx)
			}()
			select {
			case <-committed:
			case err := <-written:
				t.Fatalf("the write finished (%v) without publishing through the room", err)
			case <-time.After(10 * time.Second):
				t.Fatal("the write's publish never reached the room")
			}
			retired := make(chan error, 1)
			go func() { retired <- test.retire(service, artifactID) }()
			select {
			case err := <-retired:
				if err != nil {
					close(proceed)
					t.Fatalf("retire the room: %v", err)
				}
			case <-time.After(10 * time.Second):
				close(proceed)
				t.Fatal("closing the room did not return while the publish waited")
			}
			close(proceed)
			select {
			case err := <-written:
				if err != nil {
					t.Fatalf("write: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("the committed write's publish never returned once its room's persistence worker retired under it (%d goroutine(s) in consumeSuppressedPersistence under persistStranded)",
					goroutinesIn("consumeSuppressedPersistence", "persistStranded"))
			}
			// The publish's slot does not outlive it: every later room of the document persists
			// behind the slots queued under its name.
			for deadline := time.Now().Add(5 * time.Second); ; {
				service.suppressMu.Lock()
				queued := len(service.suppressed[artifactID])
				service.suppressMu.Unlock()
				if queued == 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("%d persistence-suppression slot(s) still queued for the document after its publish returned", queued)
				}
				time.Sleep(5 * time.Millisecond)
			}
		})
	}
}

// No hooks: a write holds its issue's row, a close of the same issue waits on that row, commits once
// the write has, and then runs SetIssueClosed, as the issue route does after its own commit
// (api/issue_patch.go), while the write's Ledger.Commit publishes. The close's CloseRoom can retire
// the room's persistence worker under the publish's Apply; the commit returns either way. The longer
// the document, the longer its room's update observer renders, and the wider that window.
func TestAWriteSurvivesItsIssueClosingAsItPublishes(t *testing.T) {
	markdown := func(paragraphs int, tag string) string {
		var text strings.Builder
		for index := range paragraphs {
			fmt.Fprintf(&text, "## Section %d\n\nParagraph %d %s: the quick brown fox jumps over the lazy dog, and a spec says what the tree will do.\n\n", index, index, tag)
		}
		return text.String()
	}
	alice := model.Actor{Kind: "user", ID: "alice"}
	for _, paragraphs := range []int{20, 50, 200} {
		t.Run(fmt.Sprintf("%d paragraphs", paragraphs), func(t *testing.T) {
			service, database := newRetiringRoomService(t)
			ctx := context.Background()
			const trials = 40
			hung := 0
			for trial := range trials {
				number := 100 + trial
				artifactID := createIssueDocument(t, database, number, markdown(paragraphs, "before"))
				seedServiceText(t, service, artifactID, markdown(paragraphs, "before"))
				tx, err := database.Pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				joined, ledger := service.Join(ctx, tx)
				if _, err := service.ReplaceText(joined, artifactID, markdown(paragraphs, "after"), alice); err != nil {
					t.Fatalf("trial %d: replace: %v", trial, err)
				}
				closed := make(chan error, 1)
				go func() {
					issueKey := fmt.Sprintf("DOC-%d", number)
					// The close waits on the issue row the write holds until the write commits.
					if _, err := database.Pool.Exec(ctx, `update issues set closed_at = now() where key = $1`, issueKey); err != nil {
						closed <- err
						return
					}
					service.SetIssueClosed(ctx, issueKey, true)
					closed <- nil
				}()
				time.Sleep(30 * time.Millisecond)
				committed := make(chan error, 1)
				go func() { committed <- ledger.Commit(ctx) }()
				select {
				case err := <-committed:
					if err != nil {
						t.Errorf("trial %d: commit: %v", trial, err)
					}
				case <-time.After(10 * time.Second):
					hung++
					continue
				}
				if err := <-closed; err != nil {
					t.Fatalf("trial %d: close the issue: %v", trial, err)
				}
			}
			if hung > 0 {
				t.Errorf("%d of %d writes never returned from their commit once their issue closed as they published (%d goroutine(s) in consumeSuppressedPersistence under persistStranded)",
					hung, trials, goroutinesIn("consumeSuppressedPersistence", "persistStranded"))
			}
		})
	}
}

// goroutinesIn counts the goroutines whose stacks name every one of functions.
func goroutinesIn(functions ...string) int {
	buffer := make([]byte, 1<<24)
	count := 0
	for _, stack := range strings.Split(string(buffer[:runtime.Stack(buffer, true)]), "\n\n") {
		matches := true
		for _, function := range functions {
			matches = matches && strings.Contains(stack, function)
		}
		if matches {
			count++
		}
	}
	return count
}
