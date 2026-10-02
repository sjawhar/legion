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
	"github.com/reearth/ygo/encoding"
	ygws "github.com/reearth/ygo/provider/websocket"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestIssueCloseClosesOpenDocumentConnection(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	headers := http.Header{"X-Dispatch-User": []string{"alice"}}
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID
	connection, response, err := gws.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("connect live document: response=%#v err=%v", response, err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	if _, err := service.store.Pool.Exec(context.Background(), `update issues set closed_at = now() where key = 'DOC-1'`); err != nil {
		t.Fatalf("close document issue: %v", err)
	}
	service.SetIssueClosed(context.Background(), "DOC-1", true)
	waitForRoomClosed(t, service, artifactID)
	waitForNoLiveDocument(t, service, artifactID)
	connection.SetReadDeadline(time.Now().Add(time.Second))
	for {
		if _, _, err := connection.ReadMessage(); err != nil {
			break
		}
	}
	if err := service.srv.Apply(context.Background(), artifactID, func(_ *crdt.Doc, _ func(func(*crdt.Transaction))) {}); !errors.Is(err, ErrIssueClosed) {
		t.Fatalf("server write after issue close = %v, want ErrIssueClosed", err)
	}
}

func TestClosedColdRoomAuthorizesReadOnly(t *testing.T) {
	service, artifactID := newTestService(t)
	if _, err := service.store.Pool.Exec(context.Background(), `update issues set closed_at = now() where key = 'DOC-1'`); err != nil {
		t.Fatalf("close document issue: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/ws/doc/"+artifactID, nil)
	request.Header.Set("X-Dispatch-User", "alice")
	request.SetPathValue("room", artifactID)
	request = request.WithContext(context.WithValue(request.Context(), connectionContextKey{}, &connectionState{}))
	config, ok := service.authorize(request)
	if !ok || !config.ReadOnly {
		t.Fatalf("cold closed room authorization = %#v, %t; want read-only acceptance", config, ok)
	}
}

func TestSchemaVersionAdmissionAuthorizesMismatchAndVersionlessClientsReadOnly(t *testing.T) {
	for _, schemaVersion := range []string{"0", ""} {
		t.Run(schemaVersion, func(t *testing.T) {
			service, artifactID := newTestService(t)
			request := httptest.NewRequest(
				http.MethodGet,
				"/ws/doc/"+artifactID+"?schema_version="+schemaVersion,
				nil,
			)
			request.Header.Set("X-Dispatch-User", "alice")
			request.SetPathValue("room", artifactID)
			request = request.WithContext(
				context.WithValue(request.Context(), connectionContextKey{}, &connectionState{}),
			)

			config, ok := service.authorize(request)
			if !ok || !config.ReadOnly {
				t.Fatalf(
					"schema version %q authorization = %#v, %t; want read-only acceptance",
					schemaVersion,
					config,
					ok,
				)
			}
		})
	}
}

func TestSchemaVersionAdmissionCommunicatesReadOnlyScope(t *testing.T) {
	service, artifactID := newTestService(t)

	mismatched, err := service.authorizeSchemaVersion(artifactID, "0")
	if err != nil {
		t.Fatalf("authorize mismatched schema version: %v", err)
	}
	if !mismatched.ReadOnly {
		t.Fatalf("mismatched schema admission = %#v, want read-only", mismatched)
	}

	current, err := service.authorizeSchemaVersion(artifactID, fmt.Sprintf("%d", pmdoc.SchemaVersion()))
	if err != nil {
		t.Fatalf("authorize current schema version: %v", err)
	}
	if current.ReadOnly {
		t.Fatalf("current schema admission = %#v, want read-write", current)
	}
}

func TestHocuspocusAuthenticationCommunicatesSchemaReadOnly(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	headers := http.Header{"X-Dispatch-User": []string{"alice"}}
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID
	connection, response, err := gws.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("connect live document: response=%#v err=%v", response, err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	auth := encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarString(artifactID)
		encoder.WriteVarUint(2) // Hocuspocus authentication message.
		encoder.WriteVarUint(0) // Token authentication payload.
		encoder.WriteVarString("0")
	})
	if err := connection.WriteMessage(gws.BinaryMessage, auth); err != nil {
		t.Fatalf("send schema authentication: %v", err)
	}

	connection.SetReadDeadline(time.Now().Add(time.Second))
	for {
		_, message, err := connection.ReadMessage()
		if err != nil {
			t.Fatalf("read schema authentication reply: %v", err)
		}
		decoder := encoding.NewDecoder(message)
		if _, err := decoder.ReadVarString(); err != nil {
			t.Fatalf("read Hocuspocus document name: %v", err)
		}
		kind, err := decoder.ReadVarUint()
		if err != nil {
			t.Fatalf("read Hocuspocus message kind: %v", err)
		}
		if kind != 2 {
			continue
		}
		subtype, err := decoder.ReadVarUint()
		if err != nil {
			t.Fatalf("read Hocuspocus authentication subtype: %v", err)
		}
		scope, err := decoder.ReadVarString()
		if err != nil {
			t.Fatalf("read Hocuspocus authentication scope: %v", err)
		}
		if subtype != 2 || scope != "readonly" {
			t.Fatalf("schema authentication = subtype %d scope %q, want authenticated readonly", subtype, scope)
		}
		return
	}
}

func TestLoadFailureMakesDocumentServiceUnavailable(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service := New(Deps{
		Store:       database,
		Persistence: failingVersionedStore{VersionedStore: NewPgVersioned(database), loadErr: errors.New("load failed")},
		Events:      events.NewBroker(),
		Identity:    identity.HeaderIdentity{Header: "X-Dispatch-User", AllowedLogins: map[string]struct{}{"alice": {}}},
	})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })

	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	headers := http.Header{"X-Dispatch-User": []string{"alice"}}
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID
	connection, response, err := gws.DefaultDialer.Dial(wsURL, headers)
	if connection != nil {
		_ = connection.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("failed-room connection: response=%#v err=%v, want HTTP 503", response, err)
	}
	if _, err := service.Text(context.Background(), artifactID); !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("load failure = %v, want ErrServiceUnavailable", err)
	}
}

// A history that does not decode refuses the socket before any upgrade, and a read of it is the
// state a rebuild repairs (ErrDocumentUnloadable) rather than any failed room's ErrServiceUnavailable.
func TestCorruptLoadRefusesTheSocketAndReadsAsUnloadable(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service := New(Deps{
		Store:       database,
		Persistence: failingVersionedStore{VersionedStore: NewPgVersioned(database), loadUpdate: []byte{0xff}},
		Events:      events.NewBroker(),
		Identity:    identity.HeaderIdentity{Header: "X-Dispatch-User", AllowedLogins: map[string]struct{}{"alice": {}}},
	})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })

	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	headers := http.Header{"X-Dispatch-User": []string{"alice"}}
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID
	connection, response, err := gws.DefaultDialer.Dial(wsURL, headers)
	if connection != nil {
		_ = connection.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("corrupt-room connection: response=%#v err=%v, want HTTP 503", response, err)
	}
	if _, err := service.Text(context.Background(), artifactID); !errors.Is(err, ErrDocumentUnloadable) || errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("corrupt load = %v, want ErrDocumentUnloadable", err)
	}
}

// A cold socket's admission check hands the room's load the history it decoded, so the history
// is read once - but only while it is still the stored one. An update appended in between raises
// the head, and the load reads the store again rather than open the room without that update.
func TestASocketsPreloadIsServedOnlyWhileTheHistoryHasNotMoved(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	adapter := &servicePersistenceAdapter{store: service.persistence, service: service}
	preload := func(t *testing.T) []byte {
		t.Helper()
		_, loaded, err := service.loadDocument(context.Background(), artifactID)
		if err != nil || loaded == nil || len(loaded.Update) == 0 {
			t.Fatalf("load cold document: loaded=%v err=%v, want its durable history", loaded, err)
		}
		service.preloads.Store(artifactID, &preloadedDocument{loaded: *loaded})
		return loaded.Update
	}

	held := preload(t)
	served, err := adapter.LoadDoc(artifactID)
	if err != nil || &served[0] != &held[0] {
		t.Fatalf("load with a current preload served %d bytes (%v), want the preloaded history itself", len(served), err)
	}
	if _, kept := service.preloads.Load(artifactID); kept {
		t.Fatal("a served preload stayed behind for a later load")
	}

	preload(t)
	moved, err := encodeDocumentTree(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{{
		Type: "paragraph", Children: []*pmdoc.Node{{Type: "text", Text: "appended"}},
	}}})
	if err != nil {
		t.Fatalf("encode appended update: %v", err)
	}
	if _, err := service.persistence.AppendUpdate(context.Background(), artifactID, moved); err != nil {
		t.Fatalf("append update after the check: %v", err)
	}
	served, err = adapter.LoadDoc(artifactID)
	if err != nil {
		t.Fatalf("load after the history moved: %v", err)
	}
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, served, nil); err != nil {
		t.Fatalf("decode the loaded history: %v", err)
	}
	if markdown, err := renderDocument(doc); err != nil || !strings.Contains(markdown, "appended") {
		t.Fatalf("load after the history moved served %q (%v), want the appended update", markdown, err)
	}
}

// A browser editor normalizes a tree it cannot represent and writes the result back, so the
// document websocket admits no connection to a room outside the Proof schema: resident or only
// stored, refused by the tree reader or only by the renderer. It completes the upgrade only to
// close with documentSchemaCloseCode before any sync, a code a browser can read, and neither loads
// nor fails the room.
func TestDocumentSocketRefusesARoomOutsideTheSchema(t *testing.T) {
	for _, test := range []struct {
		name    string
		corrupt func(*testing.T, *Service, string)
	}{
		{"a resident room the reader refuses", writeSchemaInvalidElement},
		{"a stored history only the renderer refuses", func(t *testing.T, service *Service, artifactID string) {
			appendRenderOnlySchemaViolation(t, service.store, artifactID)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			seedServiceText(t, service, artifactID, "before")
			test.corrupt(t, service, artifactID)
			httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
			t.Cleanup(httpServer.Close)
			headers := http.Header{"X-Dispatch-User": []string{"alice"}}
			wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID
			connection, response, err := gws.DefaultDialer.Dial(wsURL, headers)
			if err != nil {
				t.Fatalf("connect outside-schema document: response=%#v err=%v, want an upgrade the server closes", response, err)
			}
			t.Cleanup(func() { _ = connection.Close() })
			connection.SetReadDeadline(time.Now().Add(time.Second))
			_, message, err := connection.ReadMessage()
			var closed *gws.CloseError
			if !errors.As(err, &closed) || closed.Code != documentSchemaCloseCode {
				t.Fatalf("first frame = %q (%v), want close %d before any sync", message, err, documentSchemaCloseCode)
			}
			if _, err := service.Text(context.Background(), artifactID); !errors.Is(err, ErrDocOutsideSchema) {
				t.Fatalf("read after refused socket: %v, want ErrDocOutsideSchema", err)
			}
		})
	}
}

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

func TestShutdownClosesDocumentPeersBeforeDrain(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service := New(Deps{
		Store: database, Events: events.NewBroker(),
		Identity: identity.HeaderIdentity{Header: "X-Dispatch-User", AllowedLogins: map[string]struct{}{"alice": {}}},
	})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	seedServiceText(t, service, artifactID, "before")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	headers := http.Header{"X-Dispatch-User": []string{"alice"}}
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID
	connection, response, err := gws.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("connect live document: response=%#v err=%v", response, err)
	}
	defer connection.Close()
	waitFor(t, time.Second, "document peer connection", func() bool {
		state := service.room(artifactID)
		state.mu.Lock()
		defer state.mu.Unlock()
		return len(state.connected) == 1
	})
	shutdown := make(chan error, 1)
	go func() { shutdown <- service.Shutdown(context.Background()) }()
	connection.SetReadDeadline(time.Now().Add(time.Second))
	for {
		if _, _, err := connection.ReadMessage(); err != nil {
			break
		}
	}
	if err := <-shutdown; err != nil {
		t.Fatalf("shutdown with document peer: %v", err)
	}
	if got, err := service.Text(context.Background(), artifactID); err != nil || got != "before\n" {
		t.Fatalf("text after closing document peer = %q (%v), want before", got, err)
	}
}

func TestShutdownBoundsPeerCloseDuringLockedAppend(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service := New(Deps{
		Store: database, Events: events.NewBroker(),
		Identity: identity.HeaderIdentity{Header: "X-Dispatch-User", AllowedLogins: map[string]struct{}{"alice": {}}},
		Settle:   time.Hour,
	})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	seedServiceText(t, service, artifactID, "before")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	headers := http.Header{"X-Dispatch-User": []string{"alice"}}
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID
	connection, response, err := gws.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("connect live document: response=%#v err=%v", response, err)
	}
	defer connection.Close()

	locker, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin append lock transaction: %v", err)
	}
	defer locker.Rollback(context.Background())
	if _, err := locker.Exec(context.Background(), `select pg_advisory_xact_lock(hashtext($1))`, artifactID); err != nil {
		t.Fatalf("lock document append: %v", err)
	}
	if _, err := service.ReplaceText(context.Background(), artifactID, "after", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("write delayed document: %v", err)
	}
	if !service.hasDurableAppend(artifactID) {
		t.Fatal("durable append finished while its advisory lock was held")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if err := service.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown with peer and append lock = %v, want deadline exceeded", err)
	}
	if err := locker.Commit(context.Background()); err != nil {
		t.Fatalf("release append lock: %v", err)
	}
	waitForPersistedUpdates(t, service, artifactID, 2)
	reloaded := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	defer reloaded.Shutdown(context.Background())
	if got, err := reloaded.Text(context.Background(), artifactID); err != nil || got != "after\n" {
		t.Fatalf("text after peer-bounded shutdown = %q (%v), want after", got, err)
	}
}

func TestAppendFailureClosesDocumentConnectionAndReloadsRoom(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service := New(Deps{
		Store:       database,
		Persistence: failingVersionedStore{VersionedStore: NewPgVersioned(database), appendErr: errors.New("append failed")},
		Events:      events.NewBroker(),
		Identity:    identity.HeaderIdentity{Header: "X-Dispatch-User", AllowedLogins: map[string]struct{}{"alice": {}}},
		Settle:      time.Hour,
	})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	seedServiceText(t, service, artifactID, "before")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	headers := http.Header{"X-Dispatch-User": []string{"alice"}}
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID
	connection, response, err := gws.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("connect live document: response=%#v err=%v", response, err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	if _, err := service.ReplaceText(context.Background(), artifactID, "after", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("replace text before persistence failure: %v", err)
	}
	connection.SetReadDeadline(time.Now().Add(time.Second))
	for {
		if _, _, err := connection.ReadMessage(); err != nil {
			break
		}
	}
	_ = connection.Close()
	waitForNoLiveDocument(t, service, artifactID)
	if got, err := service.Text(context.Background(), artifactID); err != nil || got != "before\n" {
		t.Fatalf("reloaded text after append failure = %q (%v), want persisted text before", got, err)
	}
}

func TestFailedRoomEvictsAndReloadsOnNextAccess(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service := New(Deps{
		Store:       database,
		Persistence: &failingOnceVersionedStore{VersionedStore: NewPgVersioned(database)},
		Events:      events.NewBroker(),
		Identity:    identity.HeaderIdentity{Header: "X-Dispatch-User", AllowedLogins: map[string]struct{}{"alice": {}}},
	})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	seedServiceText(t, service, artifactID, "before")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	headers := http.Header{"X-Dispatch-User": []string{"alice"}}
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID

	connection, response, err := gws.DefaultDialer.Dial(wsURL, headers)
	if connection != nil {
		_ = connection.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("first failed-room access: response=%#v err=%v, want HTTP 503", response, err)
	}
	connection, response, err = gws.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("reloaded room access: response=%#v err=%v", response, err)
	}
	_ = connection.Close()
}

// TestDocumentBearerCannotForgeVerifiedServiceSubject: Actor.Service means "the
// server verified this Kubernetes subject from a projected token", and both
// renderers show it as that. The document websocket takes its actor from a
// caller-supplied header, so a shared-token holder must not be able to put one
// there — nor an owner, which seeds the default assignee.
func TestDocumentBearerCannotForgeVerifiedServiceSubject(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	service := New(Deps{
		Store:      database,
		Events:     events.NewBroker(),
		Identity:   identity.HeaderIdentity{Header: "X-Dispatch-User", AllowedLogins: map[string]struct{}{"alice": {}}},
		AgentToken: "doc-agent-token",
		ServerURL:  "https://dispatch.example",
		Settle:     20 * time.Millisecond,
	})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")

	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	headers := http.Header{
		"Authorization": []string{"Bearer doc-agent-token"},
		"X-Dispatch-Actor": []string{`{"kind":"session","id":"session-0123456789abcdef",` +
			`"origin":{"host":"forge"},` +
			`"service":"system:serviceaccount:legion:legion-worker","owner":"mallory"}`},
	}
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID
	connection, response, err := gws.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("connect as document bearer: response=%#v err=%v", response, err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	// A connection is credited with the browser edits made while it is open, and a raw room edit is
	// one, so this change puts the connection's actor on the next version, as the server persists it.
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))

	named, err := namedVersion(t, service, artifactID,
		"checkpoint", model.Actor{Kind: "user", ID: "alice"})
	if err != nil {
		t.Fatalf("name document version: %v", err)
	}
	version := named.Version
	var session *model.Actor
	for index, author := range version.Authors {
		if author.Kind == "session" {
			session = &version.Authors[index]
		}
	}
	if session == nil {
		t.Fatalf("version authors = %#v, want the bearer connection's session actor among them", version.Authors)
	}
	if session.Service != nil {
		t.Fatalf("the header's service reached the persisted author: %q", *session.Service)
	}
	if session.Owner != nil {
		t.Fatalf("the header's owner reached the persisted author: %q", *session.Owner)
	}
	if session.ID != "session-0123456789abcdef" {
		t.Fatalf("author id = %q, want the header's session id", session.ID)
	}
	if session.Origin == nil || session.Origin.Host != "forge" {
		t.Fatalf("author origin = %#v, want the header's origin preserved", session.Origin)
	}
}

// The document websocket takes a bearer's session actor only when the bearer is the shared agent
// token; one byte off, the same actor is refused.
func TestDocumentBearerMustBeTheSharedToken(t *testing.T) {
	service := &Service{agentToken: "doc-agent-token"}
	for _, test := range []struct {
		authorization string
		admitted      bool
	}{
		{authorization: "Bearer doc-agent-token", admitted: true},
		{authorization: "Bearer doc-agent-tokem", admitted: false},
	} {
		request := httptest.NewRequest(http.MethodGet, "/ws/doc/room", nil)
		request.Header.Set("Authorization", test.authorization)
		request.Header.Set("X-Dispatch-Actor", `{"kind":"session","id":"session-0123456789abcdef"}`)
		if _, err := service.requestActor(request); (err == nil) != test.admitted {
			t.Errorf("requestActor(Authorization %q) err = %v, want admitted %t", test.authorization, err, test.admitted)
		}
	}
}

func TestWebsocketRejectsUnauthenticatedConnection(t *testing.T) {
	service, _ := newTestService(t)
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/room"
	connection, response, err := gws.DefaultDialer.Dial(wsURL, nil)
	if connection != nil {
		_ = connection.Close()
	}
	if err == nil {
		t.Fatal("unauthenticated websocket connection succeeded")
	}
	if response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated websocket response = %#v, want HTTP 401", response)
	}
}

func TestUnauthenticatedDocumentRequestsDoNotAllocateRooms(t *testing.T) {
	service, _ := newTestService(t)

	for number := range 1000 {
		response := httptest.NewRecorder()
		service.ServeHTTP(response, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/ws/doc/random-%d", number), nil))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("request %d status = %d, want %d", number, response.Code, http.StatusUnauthorized)
		}
	}

	rooms := 0
	service.rooms.Range(func(_, _ any) bool {
		rooms++
		return true
	})
	if rooms != 0 {
		t.Fatalf("unauthenticated requests left %d room states, want 0", rooms)
	}
}
