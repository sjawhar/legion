// Package docstest speaks the Hocuspocus websocket sync that docs.Service.ServeHTTP serves, and
// writes the deep trees a crafted client sends, for Dispatch tests in more than one package. It is
// an ordinary package rather than a _test.go file because api, docs and pmdoc cannot import one
// another's test helpers, and it imports none of them.
package docstest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ygsync "github.com/reearth/ygo/sync"
)

// The Hocuspocus message kinds a Peer reads and writes: the outer type after the document's name.
const (
	messageSync      = 0
	messageAwareness = 1
	messageAuth      = 2
)

// The auth message's sub-kinds: the client's token, and the room's two answers to it.
const (
	authToken            = 0
	authPermissionDenied = 1
	authAuthenticated    = 2
)

// Peer is a writable connection to a document's room that behaves as a browser's provider does: it
// applies every sync message the room sends to Doc, answers the room's sync step 1, and sends the
// updates its own transactions make. It keeps the awareness states the room sends it, and can
// announce awareness states and authenticate as a provider does.
type Peer struct {
	// Doc is the peer's copy of the document. The peer's reader applies the room's updates to it.
	Doc *crdt.Doc
	// Answers carries the content of each sync step 2 the room sends, once Doc holds it. One that
	// arrives while sixteen are waiting to be read is dropped.
	Answers <-chan []byte
	// Awareness carries the entries of each awareness update the room sends, in order, once
	// AwarenessStates holds them. One that arrives while sixty-four are waiting to be read is
	// dropped; AwarenessStates and WaitForAwareness miss none.
	Awareness <-chan []AwarenessEntry
	// Ended is closed once the connection has ended and the reader has stopped.
	Ended <-chan struct{}
	// UpgradeHeader is the header of the room's answer to the websocket upgrade.
	UpgradeHeader http.Header

	artifactID string
	connection *gws.Conn
	// writes holds one write at a time: gorilla/websocket panics on two writes at once, and the
	// reader answers the room's sync step 1 while a test sends.
	writes sync.Mutex
	// auth carries the room's answers to Authenticate.
	auth chan authReply
	// mu guards the fields below it.
	mu sync.Mutex
	// awareness is every client the room has sent a state of, a client that left included, as
	// y-protocols folds the updates (foldAwareness).
	awareness map[uint64]AwarenessEntry
	// awarenessChanged is closed, and replaced, each time awareness changes.
	awarenessChanged chan struct{}
	// clocks is the clock of the last state SetAwareness sent for each client.
	clocks map[uint64]uint64
	scope  string
	// endedAt and closeCode are written by the reader before Ended closes, and read after.
	endedAt   time.Time
	closeCode int
}

// AwarenessEntry is one client's state in an awareness update: its client id, its clock, and its
// state as JSON, which is null for a client that left.
type AwarenessEntry struct {
	Client uint64
	Clock  uint64
	State  json.RawMessage
}

// Removed reports whether the entry says its client left.
func (e AwarenessEntry) Removed() bool {
	return bytes.Equal(bytes.TrimSpace(e.State), []byte("null"))
}

// authReply is the room's answer to an auth token: the scope it granted, or the reason it refused.
type authReply struct {
	granted bool
	text    string
}

// Dial connects a peer to artifactID's room at url, the document websocket's full address with its
// schema_version, with header naming the caller, and starts its reader. doc is the peer's copy of
// the document: a new one, or the copy a reconnecting browser kept. The connection closes when t
// ends.
func Dial(t testing.TB, url string, header http.Header, artifactID string, doc *crdt.Doc) *Peer {
	t.Helper()
	connection, response, err := gws.DefaultDialer.Dial(url, header)
	if err != nil {
		t.Fatalf("connect a peer to document %s: response=%#v err=%v", artifactID, response, err)
	}
	answers, awareness, ended := make(chan []byte, 16), make(chan []AwarenessEntry, 64), make(chan struct{})
	peer := &Peer{
		Doc: doc, Answers: answers, Awareness: awareness, Ended: ended, UpgradeHeader: response.Header,
		artifactID: artifactID, connection: connection, auth: make(chan authReply, 4),
		awareness: make(map[uint64]AwarenessEntry), awarenessChanged: make(chan struct{}), clocks: make(map[uint64]uint64),
	}
	t.Cleanup(peer.Close)
	go func() {
		peer.read(answers, awareness)
		peer.endedAt = time.Now()
		close(ended)
	}()
	return peer
}

// Write sends syncMessage to the room as Hocuspocus carries it: the document's name, then the sync
// kind.
func (p *Peer) Write(syncMessage []byte) error {
	return p.writeFrame(messageSync, func(encoder *encoding.Encoder) { encoder.WriteRaw(syncMessage) })
}

// writeFrame sends the room one Hocuspocus frame: the document's name, kind, then what body writes.
func (p *Peer) writeFrame(kind uint64, body func(*encoding.Encoder)) error {
	frame := encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarString(p.artifactID)
		encoder.WriteVarUint(kind)
		body(encoder)
	})
	p.writes.Lock()
	defer p.writes.Unlock()
	return p.connection.WriteMessage(gws.BinaryMessage, frame)
}

// Send runs change in one transaction on Doc and sends the room the update it made, as a keystroke
// does, returning that update.
func (p *Peer) Send(change func(*crdt.Transaction)) ([]byte, error) {
	update := Transact(p.Doc, change)
	if update == nil {
		return nil, errors.New("the peer's transaction made no update")
	}
	return update, p.Write(ygsync.EncodeUpdate(update))
}

// AskForDocument asks the room for its whole document, a sync step 1 naming no state. The room
// answers in order on this connection, and the answer arrives on Answers once Doc holds it.
func (p *Peer) AskForDocument() error {
	return p.Write(ygsync.EncodeSyncStep1(crdt.New()))
}

// SetAwareness announces client's awareness state to the room, as y-protocols encodes an update of
// one client: state is marshalled to JSON, and nil announces that client gone. Each announcement
// carries a clock one past the last this peer sent or the room sent it for client, so the room
// takes it. The room relays it to its other peers, not back to this one.
func (p *Peer) SetAwareness(client uint64, state any) error {
	encoded, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode the awareness state of client %d: %w", client, err)
	}
	p.mu.Lock()
	clock := max(p.clocks[client], p.awareness[client].Clock) + 1
	p.clocks[client] = clock
	p.mu.Unlock()
	update := encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarUint(1)
		encoder.WriteVarUint(client)
		encoder.WriteVarUint(clock)
		encoder.WriteVarString(string(encoded))
	})
	return p.writeFrame(messageAwareness, func(encoder *encoding.Encoder) { encoder.WriteVarBytes(update) })
}

// AwarenessStates is every client the room has sent this peer a state of and has not since said
// left, each with its state's JSON.
func (p *Peer) AwarenessStates() map[uint64]json.RawMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.awarenessStatesLocked()
}

func (p *Peer) awarenessStatesLocked() map[uint64]json.RawMessage {
	states := make(map[uint64]json.RawMessage, len(p.awareness))
	for client, entry := range p.awareness {
		if !entry.Removed() {
			states[client] = entry.State
		}
	}
	return states
}

// WaitForAwareness waits up to within for the awareness states the room has sent this peer
// (AwarenessStates) to satisfy holds, which it calls each time they change. It fails naming the
// states it last saw when they do not, or when the connection ends first.
func (p *Peer) WaitForAwareness(within time.Duration, holds func(map[uint64]json.RawMessage) bool) error {
	deadline := time.NewTimer(within)
	defer deadline.Stop()
	for {
		p.mu.Lock()
		states, changed := p.awarenessStatesLocked(), p.awarenessChanged
		p.mu.Unlock()
		if holds(states) {
			return nil
		}
		select {
		case <-changed:
		case <-p.Ended:
			if final := p.AwarenessStates(); holds(final) {
				return nil
			}
			return fmt.Errorf("the peer's connection closed before its awareness states held what was awaited; they hold %s", describeStates(p.AwarenessStates()))
		case <-deadline.C:
			return fmt.Errorf("the peer's awareness states did not hold what was awaited within %v; they hold %s", within, describeStates(p.AwarenessStates()))
		}
	}
}

func describeStates(states map[uint64]json.RawMessage) string {
	described := make(map[uint64]string, len(states))
	for client, state := range states {
		described[client] = string(state)
	}
	return fmt.Sprint(described)
}

// WaitForText asks the room for its document until render, which renders the peer's copy, answers
// want, and fails when it does not within that time or the connection ends first. render is the
// caller's, since this package imports no document renderer (see the package comment).
func (p *Peer) WaitForText(render func(*Peer) string, want string, within time.Duration) error {
	deadline := time.NewTimer(within)
	defer deadline.Stop()
	for render(p) != want {
		if err := p.AskForDocument(); err != nil {
			return fmt.Errorf("ask the room for the document: %w", err)
		}
		select {
		case <-p.Answers:
		case <-p.Ended:
			return fmt.Errorf("the peer's connection closed before the room sent the document: its copy renders %q, want %q", render(p), want)
		case <-deadline.C:
			return fmt.Errorf("the peer's copy renders %q, want %q, after %v", render(p), want, within)
		}
	}
	return nil
}

// Authenticate sends the room token, as a Hocuspocus provider authenticates (the document room
// reads it as the editor's schema version), and waits up to within for the answer: the scope the
// room granted, "read-write" or "readonly", or an error naming its refusal.
func (p *Peer) Authenticate(token string, within time.Duration) (string, error) {
	if err := p.writeFrame(messageAuth, func(encoder *encoding.Encoder) {
		encoder.WriteVarUint(authToken)
		encoder.WriteVarString(token)
	}); err != nil {
		return "", fmt.Errorf("send the auth token: %w", err)
	}
	deadline := time.NewTimer(within)
	defer deadline.Stop()
	select {
	case reply := <-p.auth:
		if !reply.granted {
			return "", fmt.Errorf("the room refused the auth token: %s", reply.text)
		}
		return reply.text, nil
	case <-p.Ended:
		return "", errors.New("the peer's connection closed before the room answered the auth token")
	case <-deadline.C:
		return "", fmt.Errorf("the room did not answer the auth token within %v", within)
	}
}

// Scope is the scope the room's last authenticated answer granted, "read-write" or "readonly", or
// "" before any.
func (p *Peer) Scope() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.scope
}

// Close closes the connection and waits for the reader to stop.
func (p *Peer) Close() {
	_ = p.connection.Close()
	<-p.Ended
}

// EndedAt is when the connection ended. It is read once Ended is closed.
func (p *Peer) EndedAt() time.Time {
	<-p.Ended
	return p.endedAt
}

// CloseCode is the websocket close code the connection ended with: the room's close frame's, 1006
// for a connection that dropped without one, or 0 for one this side closed (Close). It waits for
// Ended.
func (p *Peer) CloseCode() int {
	<-p.Ended
	return p.closeCode
}

// read applies every sync frame the room sends and answers its sync step 1, keeps every awareness
// update and hands on every auth answer, until the connection closes, handing answers the content
// of each sync step 2 once it is applied and awareness each awareness update once it is kept.
func (p *Peer) read(answers chan<- []byte, awareness chan<- []AwarenessEntry) {
	for {
		_, message, err := p.connection.ReadMessage()
		if err != nil {
			if closed := (*gws.CloseError)(nil); errors.As(err, &closed) {
				p.closeCode = closed.Code
			}
			return
		}
		decoder := encoding.NewDecoder(message)
		if _, err := decoder.ReadVarString(); err != nil {
			return
		}
		kind, err := decoder.ReadVarUint()
		if err != nil {
			continue
		}
		switch kind {
		case messageSync:
			if err := p.applySync(decoder.RemainingBytes(), answers); err != nil {
				return
			}
		case messageAwareness:
			update, err := decoder.ReadVarBytes()
			if err != nil {
				return
			}
			entries, err := decodeAwareness(update)
			if err != nil {
				return
			}
			p.foldAwareness(entries)
			select {
			case awareness <- entries:
			default:
			}
		case messageAuth:
			p.readAuth(decoder)
		}
	}
}

// applySync applies one sync message the room sent and answers its sync step 1, handing answers
// the content of a sync step 2 once Doc holds it.
func (p *Peer) applySync(payload []byte, answers chan<- []byte) error {
	kind, content, err := ygsync.ReadSyncMessage(payload)
	if err != nil {
		return err
	}
	reply, err := ygsync.ApplySyncMessage(p.Doc, payload, nil)
	if err != nil {
		return err
	}
	if kind == ygsync.MsgSyncStep1 && reply != nil {
		if err := p.Write(reply); err != nil {
			return err
		}
	}
	if kind == ygsync.MsgSyncStep2 {
		select {
		case answers <- content:
		default:
		}
	}
	return nil
}

// readAuth reads the room's answer to an auth token, records the scope it granted, and hands the
// answer to Authenticate. An answer that arrives while four are waiting is dropped.
func (p *Peer) readAuth(decoder *encoding.Decoder) {
	kind, err := decoder.ReadVarUint()
	if err != nil {
		return
	}
	text, err := decoder.ReadVarString()
	if err != nil {
		return
	}
	var reply authReply
	switch kind {
	case authAuthenticated:
		reply = authReply{granted: true, text: text}
		p.mu.Lock()
		p.scope = text
		p.mu.Unlock()
	case authPermissionDenied:
		reply = authReply{text: text}
	default:
		return
	}
	select {
	case p.auth <- reply:
	default:
	}
}

// decodeAwareness reads a y-protocols awareness update: a count, then each client's id, clock and
// state JSON.
func decodeAwareness(update []byte) ([]AwarenessEntry, error) {
	decoder := encoding.NewDecoder(update)
	count, err := decoder.ReadVarUint()
	if err != nil {
		return nil, err
	}
	var entries []AwarenessEntry
	for range count {
		client, err := decoder.ReadVarUint()
		if err != nil {
			return nil, err
		}
		clock, err := decoder.ReadVarUint()
		if err != nil {
			return nil, err
		}
		state, err := decoder.ReadVarString()
		if err != nil {
			return nil, err
		}
		entries = append(entries, AwarenessEntry{Client: client, Clock: clock, State: json.RawMessage(state)})
	}
	return entries, nil
}

// foldAwareness keeps each entry as y-protocols' applyAwarenessUpdate does: one with a later clock
// than the client's last, or a removal at the same clock of a client still present.
func (p *Peer) foldAwareness(entries []AwarenessEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	changed := false
	for _, entry := range entries {
		current, known := p.awareness[entry.Client]
		present := known && !current.Removed()
		if current.Clock < entry.Clock || (current.Clock == entry.Clock && entry.Removed() && present) {
			p.awareness[entry.Client] = entry
			changed = true
		}
	}
	if changed {
		close(p.awarenessChanged)
		p.awarenessChanged = make(chan struct{})
	}
}

// Transact runs change in one transaction on document and returns the update it produced, as a
// browser's provider sends a keystroke's, or nil when it produced none.
func Transact(document *crdt.Doc, change func(*crdt.Transaction)) []byte {
	origin := &transactOrigin{}
	var update []byte
	unsubscribe := document.OnUpdate(func(encoded []byte, updateOrigin any) {
		if updateOrigin == origin {
			update = append([]byte(nil), encoded...)
		}
	})
	document.Transact(change, origin)
	unsubscribe()
	return update
}

// transactOrigin tags Transact's own transaction. Transact tells its update from others by
// comparing origins with ==, so it is not zero sized: two pointers to zero-sized values may be equal.
type transactOrigin struct{ _ byte }

// WriteDeepChain writes, as the fragment's first child, blockquotes nested one inside another
// around a paragraph whose text, text, stands textLevel levels below the document: the document is
// level 0, the blockquotes levels 1 through textLevel-2, and the paragraph level textLevel-1.
// textLevel is the unit pmdoc's tree-depth bound counts. It writes the elements itself, as a
// crafted client can, rather than through pmdoc.Update, which validates the tree first.
func WriteDeepChain(txn *crdt.Transaction, fragment *crdt.YXmlFragment, textLevel int, text string) {
	parent := crdt.NewYXmlElement("blockquote")
	fragment.InsertElement(txn, 0, parent)
	for range textLevel - 3 {
		child := crdt.NewYXmlElement("blockquote")
		parent.InsertElement(txn, 0, child)
		parent = child
	}
	paragraph := crdt.NewYXmlElement("paragraph")
	parent.InsertElement(txn, 0, paragraph)
	run := crdt.NewYXmlText()
	paragraph.InsertText(txn, 0, run)
	run.Insert(txn, 0, text, nil)
}

// NestedLinkUpdate marks every character of the first text in doc's prosemirror fragment with a
// link whose "nested" attribute is nested, and returns the update a crafted client sends for that
// change. ygo's own writers refuse a mark nested past the 100 levels YText takes, so the change is
// made with a placeholder and the update rewritten: a V1 update carries a mark's attributes as JSON
// text, which ygo reads back as deep as encoding/json does. doc keeps the placeholder, so it is a
// copy the caller throws away.
func NestedLinkUpdate(t testing.TB, doc *crdt.Doc, nested any) []byte {
	t.Helper()
	text := FirstText(doc.GetXmlFragment("prosemirror"))
	if text == nil {
		t.Fatal("the document holds no text to link")
	}
	link := func(nested any) map[string]any {
		return map[string]any{"href": "https://example.com", "nested": nested}
	}
	before := doc.StateVector()
	doc.Transact(func(txn *crdt.Transaction) {
		text.Format(txn, 0, text.Len(), crdt.Attributes{"link": link("placeholder")})
	})
	update := crdt.EncodeStateAsUpdateV1(doc, before)
	// ygo writes the JSON with sorted keys and, depending on its release, with or without escaping
	// <, > and &, none of which the link holds, so json.Marshal spells it as ygo wrote it.
	written := func(v any) []byte {
		text, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("encode %T as JSON: %v", v, err)
		}
		return encoding.EncodeBytes(func(encoder *encoding.Encoder) { encoder.WriteVarString(string(text)) })
	}
	placeholder := written(link("placeholder"))
	if count := bytes.Count(update, placeholder); count != 1 {
		t.Fatalf("the link's update holds its placeholder %d times, want once", count)
	}
	return bytes.Replace(update, placeholder, written(link(nested)), 1)
}

// FirstText is the first text node in document order under fragment, or nil for a fragment
// without one.
func FirstText(fragment *crdt.YXmlFragment) *crdt.YXmlText {
	for _, child := range fragment.Children() {
		switch node := child.(type) {
		case *crdt.YXmlText:
			return node
		case *crdt.YXmlElement:
			if text := FirstText(&node.YXmlFragment); text != nil {
				return text
			}
		}
	}
	return nil
}

// NestedValue is the string "x" inside depth arrays, each nested inside the next. It builds arrays
// only; pmdoc's attribute bound counts arrays and objects alike, so depth is the value's nesting as
// that bound counts it.
func NestedValue(depth int) any {
	var value any = "x"
	for range depth {
		value = []any{value}
	}
	return value
}
