package docs

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
)

// schemaPage is the schema a page presents on its document socket: version is C, the
// schema_version it reads, and creatable is R, the schema_creatable it may create up to. A page
// that sends no schema_creatable is a page from before the manifest, every element of which has
// since 1, so its R is 1. A page that presents no number, or a number that does not parse, is not
// valid, and is admitted read-only.
type schemaPage struct {
	version, creatable int
	valid              bool
}

func schemaPageOf(query url.Values) schemaPage {
	version, err := strconv.Atoi(query.Get("schema_version"))
	if err != nil {
		return schemaPage{}
	}
	creatable := 1
	if query.Has("schema_creatable") {
		if creatable, err = strconv.Atoi(query.Get("schema_creatable")); err != nil {
			return schemaPage{}
		}
	}
	return schemaPage{version: version, creatable: creatable, valid: true}
}

// schemaFleet is what the server processes serving the database read and create: the lowest
// schema version and the highest creatable version among this process and every live process
// that registers them. A process of a build that predates the fleet columns registers neither
// (processes.Process.PredatesFleetColumns) and is left out, as is a build that does not register
// at all.
type schemaFleet struct {
	minVersion, maxCreatable int
}

func readSchemaFleet(ctx context.Context, db processes.Querier) (schemaFleet, error) {
	fleet := schemaFleet{minVersion: pmdoc.SchemaVersion(), maxCreatable: pmdoc.SchemaCreatable()}
	live, err := processes.Live(ctx, db)
	if err != nil {
		return schemaFleet{}, err
	}
	for _, process := range live {
		if process.PredatesFleetColumns() {
			continue
		}
		fleet.minVersion = min(fleet.minVersion, process.SchemaVersion)
		fleet.maxCreatable = max(fleet.maxCreatable, process.SchemaCreatable)
	}
	return fleet, nil
}

// admits reports whether page may write a document whose elements need schema version
// documentSince (pmdoc.DocumentSince) while fleet serves it: every process reads what the page
// creates (R_page <= min V), some process creates what the page creates (R_page <= max R), and
// the page represents what every process creates and what the document holds (C_page >= max(max
// R, D)). A page that cannot represent an element drops it from its copy and writes the result
// back, and a process that cannot read an element the page wrote refuses the document. The second
// bound keeps every writable page's R at or under every other writable page's C, so no writable
// page creates what another drops: after a rollback, a page of the newer build is read-only, and
// the re-check closes its socket once no live process creates what it creates.
func (f schemaFleet) admits(page schemaPage, documentSince int) bool {
	return page.valid && page.creatable <= min(f.minVersion, f.maxCreatable) && page.version >= max(f.maxCreatable, documentSince)
}

// schemaAdmissionKey is the token a socket's page presents: its room, and the schema_version it
// sent on its query, which is what its provider sends as the Hocuspocus token.
type schemaAdmissionKey struct {
	room, token string
}

// schemaAdmission is one document socket's admission: what its page presented, the documentSince
// it was admitted against, and whether it was admitted writable. revoked is set once a re-check
// has closed its socket (revokeUnless).
type schemaAdmission struct {
	key           schemaAdmissionKey
	page          schemaPage
	documentSince int
	writable      bool
	revoked       bool
	conn          *admittedConn
}

// schemaAdmissions is the admission of every document socket this process serves, from authorize
// until ServeHTTP returns. Its zero value is ready to use.
//
// It holds what the re-check needs and nothing else: the live admissions, among which it closes
// every writable socket the fleet no longer admits (revokeUnless). A token's answer is the
// admission of the socket that sent it, which that socket's own upgrade request carries
// (authorizeSchemaVersion), so no socket here stands in for another: two sockets of one room
// presenting one schema_version are admitted apart and answered apart, in whatever order their
// tokens arrive, and a socket a re-check revoked answers nobody.
type schemaAdmissions struct {
	mu      sync.Mutex
	sockets map[*schemaAdmission]struct{}
}

// admit records admission, until its socket ends (remove). It refuses nothing: a socket's own
// decision is what its token is answered with, so a second socket of one room presenting the same
// schema_version is admitted beside the first however the two were decided.
func (a *schemaAdmissions) admit(admission *schemaAdmission) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sockets == nil {
		a.sockets = make(map[*schemaAdmission]struct{})
	}
	a.sockets[admission] = struct{}{}
}

// writable is the answer to a token that key names, sent by the socket admission belongs to: that
// socket was admitted writable, no re-check has revoked it, and key is the token its own page
// presented. A token naming another schema_version than its socket presented is read-only, as is
// a revoked socket's token, which reaches here when the socket's read loop read it before the
// revoke's past read deadline stopped its reads and its connection closed.
func (a *schemaAdmissions) writable(admission *schemaAdmission, key schemaAdmissionKey) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return admission.writable && !admission.revoked && admission.key == key
}

// remove forgets admission, once its socket has ended. Removing one already forgotten does
// nothing.
func (a *schemaAdmissions) remove(admission *schemaAdmission) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sockets, admission)
}

// revokeUnless closes every writable socket that fleet no longer admits: its read loop ends, and
// its connection closes with schemaRevokedCloseCode (admittedConn). A revoked socket's admission
// stays until its own ServeHTTP removes it, read-only from here on: a token of its own still on
// its way is answered read-only (writable), and no other socket's token reads it. A socket
// already revoked is left as it is: its connection is closing. It returns the sockets it closed.
func (a *schemaAdmissions) revokeUnless(fleet schemaFleet) []*schemaAdmission {
	a.mu.Lock()
	defer a.mu.Unlock()
	var revoked []*schemaAdmission
	for admission := range a.sockets {
		if admission.writable && !admission.revoked && !fleet.admits(admission.page, admission.documentSince) {
			revoked = append(revoked, admission)
		}
	}
	for _, admission := range revoked {
		admission.revoked = true
		if admission.conn != nil {
			admission.conn.revoke()
		}
	}
	return revoked
}

// RunSchemaAdmission checks, every live window of the registry (processes.LiveWindow), each
// writable document socket this process serves against the live fleet again, and closes each one
// the fleet no longer admits, until ctx ends (recheckSchemaAdmissions). A roll that follows the
// manifest's rule never changes a decision, so editors do not flip; a process that creates what an
// open page cannot represent, or that cannot read what it creates, ends that page's edits within
// a window.
func (s *Service) RunSchemaAdmission(ctx context.Context) {
	ticker := time.NewTicker(processes.LiveWindow)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.recheckSchemaAdmissions(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("dispatch: re-check the document sockets' schema admission", "error", err)
			}
		}
	}
}

// recheckSchemaAdmissions is one pass of RunSchemaAdmission. A socket it closes gets
// schemaRevokedCloseCode, on which the provider reconnects, and the reconnect's admission decides
// again: read-only, unless a process that admits the page serves it. A pass that cannot read the
// live processes closes nothing.
func (s *Service) recheckSchemaAdmissions(ctx context.Context) error {
	fleet, err := readSchemaFleet(ctx, s.store.Pool)
	if err != nil {
		return err
	}
	for _, admission := range s.schemaAdmissions.revokeUnless(fleet) {
		slog.Info("dispatch: close a writable document socket the live server processes no longer admit; the provider reconnects",
			"room", admission.key.room, "schema_version", admission.page.version, "schema_creatable", admission.page.creatable,
			"fleet_min_schema_version", fleet.minVersion, "fleet_max_schema_creatable", fleet.maxCreatable)
	}
	return nil
}

// schemaRevokedCloseCode closes a document socket whose page the fleet no longer admits writable:
// 1012, Service Restart, on which the provider reconnects (see documentNewerContentCloseCode), and
// the reconnect is admitted again.
const schemaRevokedCloseCode = gws.CloseServiceRestart

// finalCloseFrameByte is a close frame's first byte as a server writes it: FIN set, no reserved
// bit, opcode 8 (close), and no mask, which a server never applies (RFC 6455, 5.2).
const finalCloseFrameByte = 0x88

// controlFramePayloadMax is the longest payload a control frame may carry (RFC 6455, 5.5), and so
// the longest the one-byte length that follows finalCloseFrameByte can state: a longer payload
// would need an extended length the frame below does not write, and the byte would state a length
// that is not the payload's.
const controlFramePayloadMax = 125

// schemaRevokedCloseFrame is the close frame admittedConn writes, as a server writes it: final,
// opcode close, unmasked. A reason long enough to take the payload past controlFramePayloadMax
// panics here, as this package is loaded, rather than putting a wrong length on the wire.
var schemaRevokedCloseFrame = func() []byte {
	payload := gws.FormatCloseMessage(schemaRevokedCloseCode, "the server processes changed; reconnect")
	if len(payload) > controlFramePayloadMax {
		panic(fmt.Sprintf("docs: the schema-revoked close frame's payload is %d bytes, past the %d a control frame may carry", len(payload), controlFramePayloadMax))
	}
	return append([]byte{finalCloseFrameByte, byte(len(payload))}, payload...)
}()

// revokedDeadline is a read deadline already past: a read on the connection fails at once.
var revokedDeadline = time.Unix(1, 0)

// admittedWriter hands ygo's upgrade an admittedConn in place of the connection it hijacks.
type admittedWriter struct {
	http.ResponseWriter
	conn *admittedConn
}

func (w *admittedWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	raw, buffered, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	w.conn.attach(raw)
	return w.conn, buffered, nil
}

func (w *admittedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// admittedConn is the connection ygo serves a document socket on, which a re-check can close
// without a change to ygo (revoke). ygo writes on a socket from its writer goroutine alone, and
// only reading ends its read loop, so revoke sets a read deadline already past, which ends that
// loop; ygo then stops its writer and closes the connection, and that Close writes the close frame
// first. Until revoke, every call passes through.
type admittedConn struct {
	net.Conn
	mu sync.Mutex
	// keepAlive is Conn, the connection ygo's upgrade hijacked: the keep-alive's, which follows
	// what ygo's websocket writer puts on it, so the close frame can take the end of one of its
	// messages (keepAliveWriter.Hijack, keepAliveConn). A connection of any other kind follows
	// nothing, and its socket closes without the frame.
	keepAlive *keepAliveConn
	revoked   bool
}

// attach is the connection ygo's upgrade hijacked. A socket revoked before ygo hijacked it reads
// nothing.
func (c *admittedConn) attach(raw net.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Conn = raw
	c.keepAlive, _ = raw.(*keepAliveConn)
	if c.revoked {
		_ = raw.SetReadDeadline(revokedDeadline)
	}
}

func (c *admittedConn) revoke() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.revoked = true
	if c.Conn != nil {
		_ = c.Conn.SetReadDeadline(revokedDeadline)
	}
}

// SetReadDeadline keeps a revoked connection's past deadline: ygo clears its handshake deadline
// once the first message arrives, which would otherwise undo a revoke made before.
func (c *admittedConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.revoked {
		t = revokedDeadline
	}
	return c.Conn.SetReadDeadline(t)
}

func (c *admittedConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.revoked {
		return c.Conn.SetDeadline(t)
	}
	if err := c.Conn.SetWriteDeadline(t); err != nil {
		return err
	}
	return c.Conn.SetReadDeadline(revokedDeadline)
}

// Close writes, on a revoked connection, the close frame with schemaRevokedCloseCode, and then
// closes. The frame goes out between two of ygo's messages and never inside one, which a plain
// write could not promise, since gorilla writes a frame larger than its write buffer in more than
// one write (keepAliveConn.closeBetweenMessages). Close waits for that boundary for at most
// keepAliveWriteWait, the deadline ygo gives each of its own writes (provider/websocket's
// writeTimeout), so a message that holds the boundary has as long to go out as ygo allows it; past
// that, and once ygo's writer has sent a close frame of its own, the connection closes without the
// frame. Nothing goes on the connection after the frame: every later write is refused, the
// keep-alive's included.
//
// Two of ygo's goroutines close one connection (provider/websocket/server.go): its ServeHTTP
// teardown, once its writer has stopped (after handleDisconnect), and its context watcher, which
// the revoke itself wakes - gorilla reads through the buffered reader net/http hijacked with the
// connection, whose connReader reads the connection directly and cancels the request's context on
// the read error the past deadline gives it (net/http's server.go, connReader.Read calling
// handleReadError). Whichever arrives first writes the frame. The other writes no second frame and
// waits for the first, so it closes the connection after that frame rather than from under it: a
// close that beats the frame reaches the page as an abnormal closure, not as 1012. That wait is on
// the keep-alive's connection, not on this one's lock, which Close holds only to read what it
// needs: revoke takes it under schemaAdmissions.mu, where a wait would hold up every admission.
func (c *admittedConn) Close() error {
	c.mu.Lock()
	conn, keepAlive, revoked := c.Conn, c.keepAlive, c.revoked
	c.mu.Unlock()
	if revoked && keepAlive != nil {
		_ = keepAlive.closeBetweenMessages(schemaRevokedCloseFrame, keepAliveWriteWait)
	}
	return conn.Close()
}
