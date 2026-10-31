package docs

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// fleetProcess writes the server_processes row of another server process straight into the
// registry: one that reads schema version and creates up to creatable, whose last heartbeat is
// age before the database's now(). A version of 0 is the row of a build that predates the fleet
// columns.
func fleetProcess(t *testing.T, service *Service, version, creatable int, age time.Duration) {
	t.Helper()
	if _, err := service.store.Pool.Exec(context.Background(), `
		insert into server_processes (instance, signing_keys, schema_version, schema_creatable, heartbeat_at)
		values (gen_random_uuid(), '{fingerprint}', $1, $2, now() - make_interval(secs => $3))`,
		version, creatable, age.Seconds()); err != nil {
		t.Fatalf("add a server process reading schema version %d: %v", version, err)
	}
}

// page is the query a page presents on its document socket: schema_version, and schema_creatable
// unless creatable is negative, as a page that sends no schema_creatable does.
func page(version, creatable int) url.Values {
	query := url.Values{"schema_version": {strconv.Itoa(version)}}
	if creatable >= 0 {
		query.Set("schema_creatable", strconv.Itoa(creatable))
	}
	return query
}

// dialPage opens a document socket to artifactID's room as alice's page presenting query.
func dialPage(serverURL, artifactID string, query url.Values) (*gws.Conn, *http.Response, error) {
	address := "ws" + strings.TrimPrefix(serverURL, "http") + "/ws/doc/" + artifactID + "?" + query.Encode()
	return gws.DefaultDialer.Dial(address, http.Header{"X-Dispatch-User": []string{"alice"}})
}

// mustDialPage is dialPage for a socket that must be accepted. It closes when the test ends.
func mustDialPage(t *testing.T, serverURL, artifactID string, query url.Values) *gws.Conn {
	t.Helper()
	connection, response, err := dialPage(serverURL, artifactID, query)
	if err != nil {
		t.Fatalf("connect a page presenting %v: response=%#v err=%v", query, response, err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return connection
}

// sendProbe sends an update the room does not hold: a key of its own in a map no document uses.
// The room answers it with a SyncStatus, 1 when it applied it and 0 when the socket is read-only.
func sendProbe(t *testing.T, connection *gws.Conn, artifactID string) {
	t.Helper()
	doc := crdt.New()
	var update []byte
	doc.OnUpdate(func(u []byte, _ any) { update = append([]byte(nil), u...) })
	probe := doc.GetMap("admission-probe")
	doc.Transact(func(txn *crdt.Transaction) { probe.Set(txn, "probe", "value") })
	frame := encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarString(artifactID)
		encoder.WriteVarUint(0) // Hocuspocus sync message.
	})
	if err := connection.WriteMessage(gws.BinaryMessage, append(frame, ygsync.EncodeUpdate(update)...)); err != nil {
		t.Fatalf("send the probe update: %v", err)
	}
}

// sendToken sends token in a Hocuspocus authentication message, as the provider does once its
// socket opens.
func sendToken(t *testing.T, connection *gws.Conn, artifactID, token string) {
	t.Helper()
	frame := encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarString(artifactID)
		encoder.WriteVarUint(2) // Hocuspocus authentication message.
		encoder.WriteVarUint(0) // Token authentication payload.
		encoder.WriteVarString(token)
	})
	if err := connection.WriteMessage(gws.BinaryMessage, frame); err != nil {
		t.Fatalf("send the token: %v", err)
	}
}

// readUntil reads the room's messages, for up to 5 s, until it sends one of kind (Hocuspocus's
// message type), and returns the rest of that message.
func readUntil(t *testing.T, connection *gws.Conn, kind uint64) *encoding.Decoder {
	t.Helper()
	_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer func() { _ = connection.SetReadDeadline(time.Time{}) }()
	for {
		_, message, err := connection.ReadMessage()
		if err != nil {
			t.Fatalf("read the room's messages for one of type %d: %v", kind, err)
		}
		decoder := encoding.NewDecoder(message)
		if _, err := decoder.ReadVarString(); err != nil {
			t.Fatalf("read the Hocuspocus document name: %v", err)
		}
		got, err := decoder.ReadVarUint()
		if err != nil {
			t.Fatalf("read the Hocuspocus message type: %v", err)
		}
		if got == kind {
			return decoder
		}
	}
}

// probeWritable sends a probe update and reports whether the room applied it.
func probeWritable(t *testing.T, connection *gws.Conn, artifactID string) bool {
	t.Helper()
	sendProbe(t, connection, artifactID)
	flag, err := readUntil(t, connection, 8).ReadVarUint() // Hocuspocus SyncStatus.
	if err != nil {
		t.Fatalf("read the SyncStatus: %v", err)
	}
	return flag == 1
}

// tokenWritable sends token and reports whether the room authenticated the socket read-write.
func tokenWritable(t *testing.T, connection *gws.Conn, artifactID, token string) bool {
	t.Helper()
	sendToken(t, connection, artifactID, token)
	decoder := readUntil(t, connection, 2) // Hocuspocus authentication.
	subtype, err := decoder.ReadVarUint()
	if err != nil {
		t.Fatalf("read the authentication subtype: %v", err)
	}
	scope, err := decoder.ReadVarString()
	if err != nil || subtype != 2 {
		t.Fatalf("authentication = subtype %d scope %q (%v), want Authenticated", subtype, scope, err)
	}
	return scope == "read-write"
}

// admission is what a page presenting query, and token once its socket opens, is admitted as: by
// the socket's acceptance, which the room's answer to an update sent before the token shows, and
// by the token's answer.
func admission(t *testing.T, serverURL, artifactID string, query url.Values, token string) (socket, authenticated bool) {
	t.Helper()
	connection := mustDialPage(t, serverURL, artifactID, query)
	socket = probeWritable(t, connection, artifactID)
	authenticated = tokenWritable(t, connection, artifactID, token)
	_ = connection.Close()
	return socket, authenticated
}

// newAdmissionServer is a document service on its own database, with a document on an open issue,
// served over HTTP. With future, the document also holds a typed block "future", which builds
// read from schema version 2; this build reads it and does not create it, so its own creatable
// version stays 1.
func newAdmissionServer(t *testing.T, future bool) (*Service, string, *httptest.Server) {
	t.Helper()
	markdown := "# First\n"
	if future {
		restore, err := pmdoc.UseStagedBlockForTest("future", 2)
		if err != nil {
			t.Fatalf("stage the future block as creatable: %v", err)
		}
		t.Cleanup(restore)
		markdown += "\n:::future{#future-1}\nBody.\n:::\n"
	}
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, markdown)
	if future {
		restore, err := pmdoc.UseStagedBlockForTest("future", 3)
		if err != nil {
			t.Fatalf("stage the future block as not creatable: %v", err)
		}
		t.Cleanup(restore)
	}
	server := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(server.Close)
	return service, artifactID, server
}

// A page is writable on an open issue when every live server process reads what the page
// creates (its schema_creatable is at most each process's schema version), some live process
// creates what the page creates (its schema_creatable is at most the highest creatable version),
// and the page reads what every live process creates and what the document holds (its
// schema_version is at least each process's creatable version and the highest since in the
// document). This process counts as one of them; a process of a build that predates the fleet
// columns, which registers no schema numbers, and a process whose heartbeat lapsed do not. So
// after a rollback, when no live process creates what a newer page creates, that page is
// read-only and an older page stays writable. A page that sends no schema_creatable is a page
// from before the manifest, every element of which has since 1, so it creates up to 1. The
// socket's acceptance and its token's answer agree, since the provider takes the second as the
// socket's scope.
func TestSchemaAdmissionFollowsTheLiveFleet(t *testing.T) {
	v, r := pmdoc.SchemaVersion(), pmdoc.SchemaCreatable()
	if v != 2 || r != 1 {
		t.Fatalf("this build reads schema version %d and creates up to %d; the cases below assume 2 and 1", v, r)
	}
	type process struct {
		version, creatable int
		age                time.Duration
	}
	for _, c := range []struct {
		name     string
		future   bool
		fleet    []process
		query    url.Values
		token    string
		writable bool
	}{
		{name: "a page one version newer beside a live process of its build", fleet: []process{{3, 2, 0}}, query: page(3, 2), token: "3", writable: true},
		{name: "after a rollback, a page one version newer creating what no live process creates", query: page(3, 2), token: "3"},
		{name: "after a rollback, a page one version older that represents everything the fleet creates", query: page(1, 1), token: "1", writable: true},
		{name: "a page that cannot represent an element the document holds", future: true, query: page(1, 1), token: "1"},
		{name: "a page of this version beside the element it represents", future: true, query: page(2, 1), token: "2", writable: true},
		{name: "a live process creating what an older page cannot represent", fleet: []process{{3, 2, 0}}, query: page(1, 1), token: "1"},
		{name: "a live process that cannot read what a newer page creates", fleet: []process{{1, 1, 0}}, query: page(3, 2), token: "3"},
		{name: "a process that predates the fleet columns is left out", fleet: []process{{0, 0, 0}}, query: page(2, 1), token: "2", writable: true},
		{name: "a lapsed process is left out", fleet: []process{{3, 2, time.Hour}}, query: page(1, 1), token: "1", writable: true},
		{name: "a page that sends no schema_creatable on this version", query: page(2, -1), token: "2", writable: true},
		{name: "a page that sends no schema_creatable one version newer creates what the first version reads", query: page(3, -1), token: "3", writable: true},
		{name: "a page that sends no schema_creatable beside a live process that reads only the first version", fleet: []process{{1, 1, 0}}, query: page(2, -1), token: "2", writable: true},
		{name: "a page presenting no number", query: url.Values{"schema_version": {"two"}}, token: "two"},
	} {
		t.Run(c.name, func(t *testing.T) {
			service, artifactID, server := newAdmissionServer(t, c.future)
			for _, p := range c.fleet {
				fleetProcess(t, service, p.version, p.creatable, p.age)
			}
			socket, authenticated := admission(t, server.URL, artifactID, c.query, c.token)
			if socket != c.writable || authenticated != c.writable {
				t.Errorf("a page presenting %v and token %q is admitted writable=%t by its socket and writable=%t by its token, want %t for both",
					c.query, c.token, socket, authenticated, c.writable)
			}
		})
	}
}

// A closed issue's document is read-only to every page, by the socket and by its token alike.
func TestSchemaAdmissionOfAClosedIssueIsReadOnly(t *testing.T) {
	service, artifactID, server := newAdmissionServer(t, false)
	if _, err := service.store.Pool.Exec(context.Background(), `update issues set closed_at = now() where key = 'DOC-1'`); err != nil {
		t.Fatalf("close the document's issue: %v", err)
	}
	if socket, authenticated := admission(t, server.URL, artifactID, page(2, 1), "2"); socket || authenticated {
		t.Errorf("a current page on a closed issue is admitted writable=%t by its socket and %t by its token, want read-only", socket, authenticated)
	}
}

// The provider authenticates with its schema version alone, and the answer is the admission of
// the socket that sent the token: a token that reaches the service outside any socket - a
// context carrying no admission - is read-only, and so is one naming a schema_version its own
// socket did not present. A socket's own token is answered its own admission as often as it
// sends it.
func TestATokenNoWaitingSocketPresentedIsReadOnly(t *testing.T) {
	service, artifactID, server := newAdmissionServer(t, false)
	version := strconv.Itoa(pmdoc.SchemaVersion())
	if config, err := service.authorizeSchemaVersion(context.Background(), artifactID, version); err != nil || !config.ReadOnly {
		t.Errorf("a token with no socket = %#v, %v; want read-only", config, err)
	}

	connection := mustDialPage(t, server.URL, artifactID, page(pmdoc.SchemaVersion(), pmdoc.SchemaCreatable()))
	if !tokenWritable(t, connection, artifactID, version) {
		t.Fatal("a current page's first token was answered read-only, want read-write")
	}
	if !tokenWritable(t, connection, artifactID, version) {
		t.Error("the same socket's token sent again was answered read-only; its own admission is read-write until its socket ends")
	}
	other := mustDialPage(t, server.URL, artifactID, page(pmdoc.SchemaVersion(), pmdoc.SchemaCreatable()))
	if tokenWritable(t, other, artifactID, "1") {
		t.Error("a token naming a version its own socket did not present was answered read-write, want read-only")
	}
}

// Two sockets of one room presenting one schema version share one token, and each is answered
// its own admission, so neither waits on the other: a second socket admitted read-only beside a
// writable one - a process that creates what its page cannot represent joined between them - is
// accepted rather than refused, and the one token they share is answered read-write to the first
// and read-only to the second.
func TestASecondSocketAdmittedOtherwiseWaitsForTheFirstSocketsToken(t *testing.T) {
	service, artifactID, server := newAdmissionServer(t, false)
	first := mustDialPage(t, server.URL, artifactID, page(1, 1))
	if !probeWritable(t, first, artifactID) {
		t.Fatal("an older page that represents everything the fleet creates was accepted read-only")
	}
	fleetProcess(t, service, 3, 2, 0)

	second := mustDialPage(t, server.URL, artifactID, page(1, 1))
	if probeWritable(t, second, artifactID) {
		t.Fatal("a page that cannot represent what the newer process creates was accepted writable")
	}

	if !tokenWritable(t, first, artifactID, "1") {
		t.Error("the first socket's token was answered read-only, want its own read-write admission")
	}
	if tokenWritable(t, second, artifactID, "1") {
		t.Error("the second socket's token was answered read-write, want its own read-only admission")
	}
}

// Every live window each process checks its writable document sockets against the fleet again.
// When a process that creates what an older page cannot represent joins, the older page's
// socket is closed with 1012, a code the provider reconnects on, and its reconnect is read-only;
// a current page's socket stays open and writable.
func TestAdmissionRecheckClosesASocketTheFleetNoLongerAdmits(t *testing.T) {
	service, artifactID, server := newAdmissionServer(t, false)
	older := mustDialPage(t, server.URL, artifactID, page(1, 1))
	if !tokenWritable(t, older, artifactID, "1") {
		t.Fatal("an older page that represents everything the fleet creates was authenticated read-only")
	}
	current := mustDialPage(t, server.URL, artifactID, page(pmdoc.SchemaVersion(), pmdoc.SchemaCreatable()))
	if !tokenWritable(t, current, artifactID, strconv.Itoa(pmdoc.SchemaVersion())) {
		t.Fatal("a current page was authenticated read-only")
	}

	fleetProcess(t, service, 3, 2, 0)
	if err := service.recheckSchemaAdmissions(context.Background()); err != nil {
		t.Fatalf("re-check the sockets' admission: %v", err)
	}

	requireServiceRestartClose(t, older, "the older page's socket")
	if socket, authenticated := admission(t, server.URL, artifactID, page(1, 1), "1"); socket || authenticated {
		t.Errorf("the older page's reconnect was admitted writable=%t by its socket and %t by its token, want read-only", socket, authenticated)
	}
	if !probeWritable(t, current, artifactID) {
		t.Error("the current page's socket no longer applies its updates after the re-check")
	}
}

// After a rollback no live process creates what a newer page creates, so the next re-check closes
// that page's socket with 1012, and its reconnect is read-only. A current page's socket stays open
// and writable.
func TestAdmissionRecheckClosesANewerPageOnceNoLiveProcessCreatesWhatItCreates(t *testing.T) {
	service, artifactID, server := newAdmissionServer(t, false)
	fleetProcess(t, service, 3, 2, 0)
	newer := mustDialPage(t, server.URL, artifactID, page(3, 2))
	if !tokenWritable(t, newer, artifactID, "3") {
		t.Fatal("a newer page beside a live process of its build was authenticated read-only")
	}
	current := mustDialPage(t, server.URL, artifactID, page(pmdoc.SchemaVersion(), pmdoc.SchemaCreatable()))
	if !tokenWritable(t, current, artifactID, strconv.Itoa(pmdoc.SchemaVersion())) {
		t.Fatal("a current page was authenticated read-only")
	}

	// The rollback stops the newer build's process.
	if _, err := service.store.Pool.Exec(context.Background(), `delete from server_processes`); err != nil {
		t.Fatalf("stop the newer build's process: %v", err)
	}
	if err := service.recheckSchemaAdmissions(context.Background()); err != nil {
		t.Fatalf("re-check the sockets' admission: %v", err)
	}

	requireServiceRestartClose(t, newer, "the newer page's socket")
	if socket, authenticated := admission(t, server.URL, artifactID, page(3, 2), "3"); socket || authenticated {
		t.Errorf("the newer page's reconnect was admitted writable=%t by its socket and %t by its token, want read-only", socket, authenticated)
	}
	if !probeWritable(t, current, artifactID) {
		t.Error("the current page's socket no longer applies its updates after the re-check")
	}
}

// requireServiceRestartClose reads connection until it ends, and fails the test unless the server
// closed it with 1012 within five seconds.
func requireServiceRestartClose(t *testing.T, connection *gws.Conn, what string) {
	t.Helper()
	_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, _, err := connection.ReadMessage()
		if err == nil {
			continue
		}
		var closed *gws.CloseError
		if !errors.As(err, &closed) || closed.Code != gws.CloseServiceRestart {
			t.Fatalf("%s ended with %v, want a close with %d", what, err, gws.CloseServiceRestart)
		}
		return
	}
}

// documentSocket is one document socket served through the wrappers a document socket is served
// through (Service.ServeHTTP): the keep-alive's writer under the admitted connection, so what
// ygo's upgrade hijacks is a keepAliveConn held by an admittedConn. page is the other end.
type documentSocket struct {
	admitted  *admittedConn
	keepAlive *keepAliveConn
	page      *gws.Conn
}

// serveDocumentSocket serves one document socket through those wrappers and connects a page to
// it. The server, the socket and the page close when the test ends.
func serveDocumentSocket(t *testing.T) documentSocket {
	t.Helper()
	served, done := make(chan *admittedConn, 1), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		admitted := &admittedConn{}
		keepAlive := newKeepAliveWriter(w, "room", time.Hour)
		defer keepAlive.stop()
		var upgrader gws.Upgrader
		if _, err := upgrader.Upgrade(&admittedWriter{ResponseWriter: keepAlive, conn: admitted}, r, nil); err != nil {
			t.Errorf("upgrade the document socket: %v", err)
			return
		}
		served <- admitted
		<-done
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(done) })

	page, response, err := gws.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/ws/doc/room", nil)
	if err != nil {
		t.Fatalf("connect a page: response=%#v err=%v", response, err)
	}
	t.Cleanup(func() { _ = page.Close() })
	admitted := <-served
	keepAlive, ok := admitted.Conn.(*keepAliveConn)
	if !ok {
		t.Fatalf("ygo's upgrade hijacked a %T, want the keep-alive's connection", admitted.Conn)
	}
	return documentSocket{admitted: admitted, keepAlive: keepAlive, page: page}
}

// A revoked socket's close frame is a message of its own, so it must never go out inside one of
// ygo's. gorilla writes a frame larger than its write buffer as a header and the buffer's worth in
// one write and the rest in another, and the close of a revoked connection - ygo's context
// watcher, which the revoke wakes, and its ServeHTTP teardown, both of which call Close - can
// arrive between the two. Here the writer puts the header and the first 4 KB of a 10 KB binary
// message on the connection, both closers call Close while that message is part way out, and the
// rest of the message follows: the page reads that message whole and then the close with 1012.
// Nothing follows that frame (TestARevokedSocketTakesNoWriteAfterItsCloseFrame).
func TestARevokedSocketsCloseFrameWaitsForTheEndOfAMessage(t *testing.T) {
	connection := serveDocumentSocket(t)
	connection.admitted.revoke()

	// A binary message past gorilla's 4 KB write buffer, written as gorilla writes one.
	message := bytes.Repeat([]byte("page"), 2_500)
	frame := append([]byte{finalFrame | gws.BinaryMessage, 126, byte(len(message) >> 8), byte(len(message))}, message...)
	const firstWrite = 4 + 4096
	begun, release, rest := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		if _, err := connection.admitted.Write(frame[:firstWrite]); err != nil {
			close(begun)
			rest <- err
			return
		}
		close(begun)
		<-release
		_, err := connection.admitted.Write(frame[firstWrite:])
		rest <- err
	}()
	<-begun
	closed := make(chan error, 2)
	for range cap(closed) {
		go func() { closed <- connection.admitted.Close() }()
	}
	// Both closers reach the connection while the message is part way out, as ygo's two do.
	time.Sleep(50 * time.Millisecond)
	close(release)
	if err := <-rest; err != nil {
		t.Errorf("the rest of the message could not be written; the close cut it short: %v", err)
	}
	<-closed
	<-closed

	_ = connection.page.SetReadDeadline(time.Now().Add(5 * time.Second))
	kind, read, err := connection.page.ReadMessage()
	if err != nil || kind != gws.BinaryMessage || !bytes.Equal(read, message) {
		t.Fatalf("the page read a message of type %d and %d bytes (%v), want the %d-byte message whole before the close",
			kind, len(read), err, len(message))
	}
	requireServiceRestartClose(t, connection.page, "the revoked page's socket")
}

// Nothing goes on a revoked socket's connection after its close frame: keepAliveConn refuses
// every write once that frame has had its turn, the keep-alive's own among them, so the page
// reads the close with 1012 and nothing after it. Here the frame goes out as admittedConn.Close
// writes it (closeBetweenMessages), on a connection nothing has closed yet; a test that closed
// the connection first would show none of this, since a write on a closed socket fails whatever
// the connection does.
func TestARevokedSocketTakesNoWriteAfterItsCloseFrame(t *testing.T) {
	connection := serveDocumentSocket(t)
	connection.admitted.revoke()
	if err := connection.keepAlive.closeBetweenMessages(schemaRevokedCloseFrame, keepAliveWriteWait); err != nil {
		t.Fatalf("the revoked socket's close frame could not be written: %v", err)
	}
	requireServiceRestartClose(t, connection.page, "the revoked page's socket")

	message := append([]byte{finalFrame | gws.BinaryMessage, byte(len("page"))}, "page"...)
	if _, err := connection.admitted.Write(message); !errors.Is(err, net.ErrClosed) {
		t.Errorf("a write after the close frame = %v, want %v", err, net.ErrClosed)
	}
	if err := connection.keepAlive.writeBetweenMessages(keepAliveFrame("room"), time.Second); err != nil {
		t.Errorf("the keep-alive's write after the close frame failed with %v, want it refused and nothing written", err)
	}

	// The page's own connection, not its websocket reader, which goes no further than the close
	// frame: whatever those writes put on the connection is waiting to be read here.
	raw := connection.page.UnderlyingConn()
	_ = raw.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	var after [64]byte
	if read, err := raw.Read(after[:]); read != 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("the page read %d bytes (%v) after the close frame, want nothing until its deadline", read, err)
	}
}

// Two pages of one room presenting one schema_version share a token, and a re-check can revoke
// one of them while its token is still on its way: after a rollback the page creating up to
// version 2 is no longer admitted, and the page creating up to 1 stays writable. The older page
// was admitted first and the newer beside it. Each socket's token is answered that socket's own
// admission, so the one the fleet still admits is read-write whichever of the two tokens comes
// first, and the revoked socket's own token is read-only. The revoked page's read-only reconnect
// joins them rather than being refused: no socket's answer is another's to take.
func TestARevokedSocketKeepsItsPlaceUntilItsTokenIsAnswered(t *testing.T) {
	var admissions schemaAdmissions
	key := schemaAdmissionKey{room: "room", token: "2"}
	older := &schemaAdmission{key: key, page: schemaPage{version: 2, creatable: 1, valid: true}, writable: true}
	newer := &schemaAdmission{key: key, page: schemaPage{version: 2, creatable: 2, valid: true}, writable: true}
	for _, admission := range []*schemaAdmission{older, newer} {
		admissions.admit(admission)
	}

	revoked := admissions.revokeUnless(schemaFleet{minVersion: 2, maxCreatable: 1})
	if len(revoked) != 1 || revoked[0] != newer {
		t.Fatalf("the rollback revoked %d sockets, want the newer page's alone", len(revoked))
	}
	reconnect := &schemaAdmission{key: key, page: schemaPage{version: 2, creatable: 2, valid: true}}
	admissions.admit(reconnect)
	if admissions.writable(reconnect, key) {
		t.Error("the revoked page's read-only reconnect was answered read-write")
	}
	if admissions.writable(newer, key) {
		t.Fatal("the revoked socket's own token was answered read-write, want read-only")
	}
	if !admissions.writable(older, key) {
		t.Error("the socket that is still admitted was answered read-only; it holds a writable admission")
	}
}

// Here the newer page is admitted first and the older one beside it, and the rollback revokes
// the newer: the revoked socket's token, already read when the revoke landed, arrives first and
// is answered read-only, taking nothing from the older page. The revoked socket's ServeHTTP then
// returns and forgets its admission, and the older page's token - the only one left of that
// token - is still answered the read-write admission of its own socket. A read-only reconnect of
// the revoked page joins beside it.
func TestARevokedSocketsEntryAnswersBehindEverySocketStillOpen(t *testing.T) {
	var admissions schemaAdmissions
	key := schemaAdmissionKey{room: "room", token: "2"}
	newer := &schemaAdmission{key: key, page: schemaPage{version: 2, creatable: 2, valid: true}, writable: true}
	older := &schemaAdmission{key: key, page: schemaPage{version: 2, creatable: 1, valid: true}, writable: true}
	for _, admission := range []*schemaAdmission{newer, older} {
		admissions.admit(admission)
	}

	revoked := admissions.revokeUnless(schemaFleet{minVersion: 2, maxCreatable: 1})
	if len(revoked) != 1 || revoked[0] != newer {
		t.Fatalf("the rollback revoked %d sockets, want the newer page's alone", len(revoked))
	}
	if admissions.writable(newer, key) {
		t.Fatal("the revoked socket's own token was answered read-write, want read-only")
	}
	admissions.remove(newer)

	if !admissions.writable(older, key) {
		t.Error("the older page's token was answered read-only once the revoked socket was gone; its own admission is read-write")
	}
	reconnect := &schemaAdmission{key: key, page: schemaPage{version: 2, creatable: 2, valid: true}}
	admissions.admit(reconnect)
	if admissions.writable(reconnect, key) {
		t.Error("the revoked page's read-only reconnect was answered read-write")
	}
}

// A revoked socket answers nothing read-write afterwards, its own token included, and its
// ServeHTTP is what forgets it: the registry the re-check walks is empty once that socket has
// ended, so no later pass revokes it a second time.
func TestARevokedSocketWhoseTokenWasAnsweredIsForgotten(t *testing.T) {
	var admissions schemaAdmissions
	key := schemaAdmissionKey{room: "room", token: "3"}
	socket := &schemaAdmission{key: key, page: schemaPage{version: 3, creatable: 2, valid: true}, writable: true}
	admissions.admit(socket)
	if !admissions.writable(socket, key) {
		t.Fatal("the socket's token was answered read-only, want its writable admission")
	}

	if revoked := admissions.revokeUnless(schemaFleet{minVersion: 2, maxCreatable: 1}); len(revoked) != 1 || revoked[0] != socket {
		t.Fatalf("the rollback revoked %d sockets, want the newer page's", len(revoked))
	}
	if admissions.writable(socket, key) {
		t.Error("the revoked socket's token was answered read-write, want read-only")
	}
	if revoked := admissions.revokeUnless(schemaFleet{minVersion: 2, maxCreatable: 1}); len(revoked) != 0 {
		t.Errorf("a second pass revoked %d sockets, want the revoked one left as it is", len(revoked))
	}

	admissions.remove(socket)
	if held := len(admissions.sockets); held != 0 {
		t.Errorf("the registry holds %d sockets once the revoked one has ended, want none", held)
	}
}
