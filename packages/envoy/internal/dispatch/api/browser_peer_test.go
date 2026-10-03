package api

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// servedSockets lets a test wait until the document server is done with one of its
// connections. ygo runs a peer's disconnect before its handler returns: the room's eviction when
// the peer was its last, and the last-peer settlement.
type servedSockets struct {
	mu       sync.Mutex
	finished map[string]chan struct{}
	next     atomic.Int64
}

// socketHeader names a connection so servedSockets can say when the server let it go.
const socketHeader = "X-Test-Socket"

func (s *servedSockets) serve(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		done := make(chan struct{})
		s.mu.Lock()
		s.finished[r.Header.Get(socketHeader)] = done
		s.mu.Unlock()
		defer close(done)
		next(w, r)
	}
}

func (s *servedSockets) done(id string) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finished[id]
}

// syncedPeer is a writable document connection that behaves as the SPA's provider does: it
// applies every update the room sends, and reconnects with the document it kept.
type syncedPeer struct {
	wsURL      string
	sockets    *servedSockets
	headers    http.Header
	artifactID string
	doc        *crdt.Doc

	mu         sync.Mutex
	connection *gws.Conn
	socketID   string
	readerDone chan struct{}
	// answers carries the content of each sync step 2 the room sends, once read has applied it.
	answers chan []byte
}

// connectBrowserPeer connects alice's browser to artifactID's room, through a document server of
// its own over documentService.
func connectBrowserPeer(t *testing.T, documentService *docs.Service, artifactID string) *syncedPeer {
	t.Helper()
	sockets := &servedSockets{finished: make(map[string]chan struct{})}
	server := httptest.NewServer(sockets.serve(documentService.ServeHTTP))
	t.Cleanup(server.Close)
	peer := &syncedPeer{
		wsURL: "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/doc/" + artifactID, sockets: sockets,
		headers: http.Header{"X-Dispatch-User": []string{"alice"}}, artifactID: artifactID, doc: crdt.New(),
	}
	peer.connect(t)
	return peer
}

func (p *syncedPeer) connect(t *testing.T) {
	t.Helper()
	socketID := strconv.FormatInt(p.sockets.next.Add(1), 10)
	headers := p.headers.Clone()
	headers.Set(socketHeader, socketID)
	connection, response, err := gws.DefaultDialer.Dial(p.wsURL+"?schema_version="+strconv.Itoa(pmdoc.SchemaVersion()), headers)
	if err != nil {
		t.Fatalf("connect browser peer: response=%#v err=%v", response, err)
	}
	p.mu.Lock()
	p.connection = connection
	p.socketID = socketID
	p.readerDone = make(chan struct{})
	p.answers = make(chan []byte, 16)
	readerDone, answers := p.readerDone, p.answers
	p.mu.Unlock()
	go p.read(connection, readerDone, answers)
	p.barrier(t)
}

// read applies every sync frame the room sends. It answers the room's sync step 1 with the
// updates the room lacks, and hands barrier the content of each sync step 2 it applies.
func (p *syncedPeer) read(connection *gws.Conn, done chan<- struct{}, answers chan<- []byte) {
	defer close(done)
	docstest.Drain(connection, p.doc, func(syncMessage []byte) error {
		return p.write(connection, syncMessage)
	}, func(content []byte) {
		select {
		case answers <- content:
		default:
		}
	})
}

func (p *syncedPeer) write(connection *gws.Conn, syncMessage []byte) error {
	return docstest.WriteFrame(&p.mu, connection, p.artifactID, syncMessage)
}

// barrier returns once the room holds everything the peer sent and has answered a request sent
// now. It drops the answers already queued, then asks for the room's whole state (a sync step 1
// naming no state) until an answer holds the peer's document. The room answers one connection in
// order, and each answer is applied to the peer before barrier sees it, so the peer then holds
// everything the room sent before that answer. The document check covers a sync step 2 that was
// already on its way when barrier drained: another peer's that the room relays can arrive after
// the request and predate an update this peer sent just before it.
func (p *syncedPeer) barrier(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	connection, answers, done := p.connection, p.answers, p.readerDone
	p.mu.Unlock()
	for len(answers) > 0 {
		<-answers
	}
	deadline := time.After(5 * time.Second)
	for {
		if err := p.write(connection, ygsync.EncodeSyncStep1(crdt.New())); err != nil {
			t.Fatalf("send browser peer sync step 1: %v", err)
		}
		select {
		case state := <-answers:
			if p.roomHolds(t, state) {
				return
			}
		case <-done:
			t.Fatal("browser peer connection closed before the room answered its sync")
		case <-deadline:
			t.Fatal("the room never answered with a state holding everything the browser peer sent")
		}
	}
}

// roomHolds reports whether state, the content of a sync step 2, holds everything in the peer's
// document: applying the peer's document on top of it changes nothing.
func (p *syncedPeer) roomHolds(t *testing.T, state []byte) bool {
	t.Helper()
	room := crdt.New()
	if err := crdt.ApplyUpdateV1(room, state, nil); err != nil {
		t.Fatalf("decode the room's sync step 2: %v", err)
	}
	before := crdt.EncodeStateAsUpdateV1(room, nil)
	if err := crdt.ApplyUpdateV1(room, crdt.EncodeStateAsUpdateV1(p.doc, nil), nil); err != nil {
		t.Fatalf("apply the browser peer's document to the room's state: %v", err)
	}
	return bytes.Equal(before, crdt.EncodeStateAsUpdateV1(room, nil))
}

func (p *syncedPeer) reconnect(t *testing.T) {
	t.Helper()
	// A browser's reconnect reaches a server that has let its old connection go, and with it the
	// room that connection emptied. A dial before then races that teardown, and the server can
	// drop the new connection in the window.
	p.closeAndWait(t)
	p.connect(t)
}

// closeAndWait closes the peer's connection and returns once the document server has let it go,
// which is after the room's eviction and last-peer settlement when the peer was the room's last.
func (p *syncedPeer) closeAndWait(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	socketID := p.socketID
	p.mu.Unlock()
	p.close()
	select {
	case <-p.sockets.done(socketID):
	case <-time.After(30 * time.Second):
		t.Fatal("the document server did not let go of the browser peer's closed connection")
	}
}

func (p *syncedPeer) close() {
	p.mu.Lock()
	connection, done := p.connection, p.readerDone
	p.mu.Unlock()
	if connection == nil {
		return
	}
	_ = connection.Close()
	<-done
}

// edit runs change on the peer's document tree in one local transaction and sends the update it
// produced, as a keystroke does.
func (p *syncedPeer) edit(t *testing.T, change func(*pmdoc.Node) error) {
	t.Helper()
	p.transact(t, func(txn *crdt.Transaction, fragment *crdt.YXmlFragment) error {
		tree, err := pmdoc.ReadInTransaction(txn, fragment)
		if err != nil {
			return err
		}
		if err := change(tree); err != nil {
			return err
		}
		return pmdoc.Update(txn, fragment, tree)
	})
}

// transact runs change on the peer's live fragment in one local transaction and sends the update
// it produced. A change that writes the fragment itself rather than through pmdoc.Update can write
// what no server route would, as a crafted client can.
func (p *syncedPeer) transact(t *testing.T, change func(*crdt.Transaction, *crdt.YXmlFragment) error) {
	t.Helper()
	fragment := p.doc.GetXmlFragment("prosemirror")
	var changeErr error
	update := docstest.Transact(p.doc, func(txn *crdt.Transaction) {
		changeErr = change(txn, fragment)
	})
	if changeErr != nil || update == nil {
		t.Fatalf("browser peer edit: update=%d bytes err=%v", len(update), changeErr)
	}
	p.mu.Lock()
	connection := p.connection
	p.mu.Unlock()
	if err := p.write(connection, ygsync.EncodeUpdate(update)); err != nil {
		t.Fatalf("send browser peer update: %v", err)
	}
}

func (p *syncedPeer) appendParagraph(t *testing.T, text string) {
	t.Helper()
	typed, err := pmdoc.Parse(text)
	if err != nil {
		t.Fatalf("parse browser paragraph: %v", err)
	}
	p.edit(t, func(tree *pmdoc.Node) error {
		tree.Children = append(tree.Children, typed.Children...)
		return nil
	})
}

// appendToFirstParagraph types text at the end of the first paragraph's last text run.
func (p *syncedPeer) appendToFirstParagraph(t *testing.T, text string) {
	t.Helper()
	p.edit(t, func(tree *pmdoc.Node) error {
		if len(tree.Children) == 0 || len(tree.Children[0].Children) == 0 {
			return errors.New("browser peer document has no text to type after")
		}
		runs := tree.Children[0].Children
		runs[len(runs)-1].Text += text
		return nil
	})
}

// assertProjection checks the peer's copy of markID's margin projection against want.
func (p *syncedPeer) assertProjection(t *testing.T, markID string, want map[string]any) {
	t.Helper()
	got, _ := p.doc.GetMap("marks").Get(markID)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("browser peer projection of %s = %#v, want %#v", markID, got, want)
	}
}

func (p *syncedPeer) assertTextLacks(t *testing.T, text string) {
	t.Helper()
	if got := renderDocument(t, p.doc); strings.Contains(got, text) {
		t.Fatalf("browser peer document = %q, holds %q", got, text)
	}
}
