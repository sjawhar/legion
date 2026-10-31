package docs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
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

// The provider authenticates with its schema version alone, so a token names the page that
// presented it and never the socket that sent it: the answer is the admission that socket's own
// request context carries (authorizeSchemaVersion). A context carrying no admission is read-only;
// a socket's own token is answered with that socket's admission, as often as it asks; and a token
// its page never presented is read-only.
func TestATokenIsAnsweredFromTheAdmissionItsSocketCarries(t *testing.T) {
	service, artifactID, server := newAdmissionServer(t, false)
	version := strconv.Itoa(pmdoc.SchemaVersion())
	if config, err := service.authorizeSchemaVersion(context.Background(), artifactID, version); err != nil || !config.ReadOnly {
		t.Errorf("a token on a context carrying no admission = %#v, %v; want read-only", config, err)
	}

	connection := mustDialPage(t, server.URL, artifactID, page(pmdoc.SchemaVersion(), pmdoc.SchemaCreatable()))
	if !tokenWritable(t, connection, artifactID, version) {
		t.Fatal("a current page's first token was answered read-only, want read-write")
	}
	if !tokenWritable(t, connection, artifactID, version) {
		t.Error("the same socket's token, sent again, was answered read-only; its admission has not changed")
	}
	other := mustDialPage(t, server.URL, artifactID, page(pmdoc.SchemaVersion(), pmdoc.SchemaCreatable()))
	if tokenWritable(t, other, artifactID, "1") {
		t.Error("a token naming a version the socket's page never presented was answered read-write, want read-only")
	}
}

// Every live window each process checks its writable document sockets against the fleet again.
// When a process that creates what an older page cannot represent joins, the older page's socket
// is closed with the admission-revoked code, on which the provider reconnects, and its reconnect
// is read-only; a current page's socket stays open and writable.
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

	requireRevokedClose(t, older, "the older page's socket")
	if socket, authenticated := admission(t, server.URL, artifactID, page(1, 1), "1"); socket || authenticated {
		t.Errorf("the older page's reconnect was admitted writable=%t by its socket and %t by its token, want read-only", socket, authenticated)
	}
	if !probeWritable(t, current, artifactID) {
		t.Error("the current page's socket no longer applies its updates after the re-check")
	}
}

// After a rollback no live process creates what a newer page creates, so the next re-check closes
// that page's socket with the admission-revoked code, and its reconnect is read-only. A current
// page's socket stays open and writable.
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

	requireRevokedClose(t, newer, "the newer page's socket")
	if socket, authenticated := admission(t, server.URL, artifactID, page(3, 2), "3"); socket || authenticated {
		t.Errorf("the newer page's reconnect was admitted writable=%t by its socket and %t by its token, want read-only", socket, authenticated)
	}
	if !probeWritable(t, current, artifactID) {
		t.Error("the current page's socket no longer applies its updates after the re-check")
	}
}

// requireRevokedClose reads connection until it ends, and fails the test unless the server closed
// it with the admission-revoked code within five seconds.
func requireRevokedClose(t *testing.T, connection *gws.Conn, what string) {
	t.Helper()
	_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, _, err := connection.ReadMessage()
		if err == nil {
			continue
		}
		var closed *gws.CloseError
		if !errors.As(err, &closed) || closed.Code != contracts.DocumentAdmissionRevokedCloseCode {
			t.Fatalf("%s ended with %v, want a close with %d", what, err, contracts.DocumentAdmissionRevokedCloseCode)
		}
		return
	}
}

// bigDocumentMarkdown is a document whose sync message is far past 64 KB, so the server writes it
// to a page in many writes and cannot hold it in the socket's buffers.
func bigDocumentMarkdown() string {
	var markdown strings.Builder
	markdown.WriteString("# First\n")
	for paragraph := range 4_000 {
		fmt.Fprintf(&markdown, "\nParagraph %d: %s\n", paragraph, strings.Repeat("document ", 16))
	}
	return markdown.String()
}

// dialSlowPage is dialPage on a connection whose receive buffer holds far less than the document's
// sync message, so the server's write of that message is still part way through it while the page
// reads nothing. The socket must be accepted, and closes when the test ends.
func dialSlowPage(t *testing.T, serverURL, artifactID string, query url.Values) *gws.Conn {
	t.Helper()
	dialer := gws.Dialer{NetDial: func(network, address string) (net.Conn, error) {
		connection, err := net.Dial(network, address)
		if err != nil {
			return nil, err
		}
		tcp, ok := connection.(*net.TCPConn)
		if !ok {
			return nil, fmt.Errorf("the page dialled a %T, want a TCP connection whose receive buffer this can set", connection)
		}
		if err := tcp.SetReadBuffer(4096); err != nil {
			_ = tcp.Close()
			return nil, err
		}
		return tcp, nil
	}}
	address := "ws" + strings.TrimPrefix(serverURL, "http") + "/ws/doc/" + artifactID + "?" + query.Encode()
	connection, response, err := dialer.Dial(address, http.Header{"X-Dispatch-User": []string{"alice"}})
	if err != nil {
		t.Fatalf("connect a slow page presenting %v: response=%#v err=%v", query, response, err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return connection
}

// A revoke ends the socket with a close frame of ygo's own, which gorilla's WriteControl puts on
// the connection between two of ygo's messages: it waits up to a second for a message already
// going out and never cuts into one. Here the document's sync message is far past 64 KB and the
// page reads nothing until the re-check has revoked its socket, so that message is part way out
// when the revoke arrives. The page then reads every message whole and the close with the
// admission-revoked code after them - never a protocol error from a frame cut in half, and never
// the bare 1006 of a connection closed without a frame.
func TestARevokeWaitsForTheMessageTheServerHasInFlight(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, bigDocumentMarkdown())
	server := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(server.Close)

	// A page creating up to 1 represents everything this fleet creates, so its socket is admitted
	// writable; it reads nothing, so the sync message stays part way out.
	reader := dialSlowPage(t, server.URL, artifactID, page(1, 1))
	time.Sleep(250 * time.Millisecond)

	// A process that creates what this page cannot represent joins, and the re-check revokes it.
	fleetProcess(t, service, 3, 2, 0)
	if err := service.recheckSchemaAdmissions(context.Background()); err != nil {
		t.Fatalf("re-check the sockets' admission: %v", err)
	}

	biggest := 0
	_ = reader.SetReadDeadline(time.Now().Add(30 * time.Second))
	for {
		_, message, err := reader.ReadMessage()
		if err != nil {
			var closed *gws.CloseError
			if !errors.As(err, &closed) || closed.Code != contracts.DocumentAdmissionRevokedCloseCode {
				t.Fatalf("the revoked page's socket ended with %v, want a close with %d after the messages in flight",
					err, contracts.DocumentAdmissionRevokedCloseCode)
			}
			break
		}
		biggest = max(biggest, len(message))
	}
	if biggest <= 64*1024 {
		t.Fatalf("the biggest message the page read whole is %d bytes; this test needs the revoke to arrive inside one past 64 KB", biggest)
	}
}

// ygoHandshakeTimeout is the deadline ygo's ServeHTTP sets on a new socket's first read, after
// which it ends a connection whose page has sent nothing of its own; Dispatch leaves it at ygo's
// default (sjawhar/ygo provider/websocket/server.go, defaultHandshakeTimeout and the
// SetReadDeadline ServeHTTP sets before the first message it reads). The slow page below sends
// nothing, so this deadline ends its socket by itself 30 s after it connects, revoke or no
// revoke.
const ygoHandshakeTimeout = 30 * time.Second

// ygoPeerWriteTimeout is the deadline ygo sets on each write to a peer, after which it gives up
// on that write and closes the peer (sjawhar/ygo provider/websocket/server.go, writeTimeout, set
// by peer.writeToConn). The slow page below reads nothing, so the server's write of the document
// to it is stalled from the moment it connects and this deadline runs from there.
const ygoPeerWriteTimeout = 10 * time.Second

// revokeAfterDialBound is how soon after the slow page connects the revoke must be taken, a
// quarter of the earlier of the two deadlines ygo would end that socket on by itself, and
// revokedEndBound is how long after the revoke the socket must end: the two seconds the page
// reads nothing for, and two more to drain what was in flight and read the end, which takes
// milliseconds on a loopback socket. Together they put the end at most 6.5 s after the page
// connected, well inside both, so an end this test accepts is the revoke's work and neither of
// theirs.
const (
	revokeAfterDialBound = min(ygoPeerWriteTimeout, ygoHandshakeTimeout) / 4
	revokedEndBound      = 4 * time.Second
)

// A page on a slow link takes nothing from its socket for longer than the revoked close waits, so
// it reads the connection's end as 1006 rather than 4412 and loses nothing by it; the rule is in
// packages/envoy/cmd/dispatch/README.md, "Two builds at once". This pins both of its halves on
// real sockets. A writer page sends the room eight whole-document replacements of about 1 MB
// each, and the slow page then connects to a room holding far more than its buffers can take and
// reads nothing: the server's write of all of it is part way out when the re-check revokes that
// socket, and the page starts reading two seconds later. The re-check must return that page's
// admission as one it closed; its socket must end within revokedEndBound of the revoke, with one
// of the two codes the provider reconnects on - never a protocol error from a frame cut in half,
// and never a socket that does not end; and the page's reconnect must hold the server's whole
// document once it has synced, the writes that were in flight included.
func TestARevokedSlowPageEndsOnACodeItReconnectsOnAndSyncsWhole(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, bigDocumentMarkdown())
	server := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(server.Close)

	// The writer presents this build's numbers, which the fleet below still admits writable, so
	// the re-check leaves its socket open. Its rounds leave the room holding megabytes of document
	// and history, which is what the slow page is then owed.
	writer := connectBrowser(t, server.URL, artifactID, "bob")
	for round := range 8 {
		editAsBrowser(t, service, artifactID, writer,
			strings.ReplaceAll(bigDocumentMarkdown(), "document ", fmt.Sprintf("round%d ", round)))
	}

	// The slow page connects last and reads nothing, so the server's write of all of that is still
	// going out when the revoke arrives, and every round is still owed to it.
	dialed := time.Now()
	reader := dialSlowPage(t, server.URL, artifactID, page(1, 1))
	time.Sleep(250 * time.Millisecond)
	fleetProcess(t, service, 3, 2, 0)
	fleet, err := readSchemaFleet(context.Background(), service.store.Pool)
	if err != nil {
		t.Fatalf("read the live fleet: %v", err)
	}
	// The re-check's own revoke (recheckSchemaAdmissions), taken here so the admissions it closed
	// are read: this fleet still admits the writer's page, so the slow page's is the only socket
	// it closes, and the end below is that revoke's and no other's.
	revokedAt := time.Now()
	revoked := service.schemaAdmissions.revokeUnless(fleet)
	if len(revoked) != 1 {
		t.Fatalf("the re-check revoked %d sockets, want the slow page's alone", len(revoked))
	}
	if got := revoked[0]; got.key.room != artifactID || got.page != (schemaPage{version: 1, creatable: 1, valid: true}) {
		t.Fatalf("the re-check revoked room %q presenting %+v, want room %q presenting version 1 and creating up to 1, the slow page's",
			got.key.room, got.page, artifactID)
	}
	if took := revokedAt.Sub(dialed); took > revokeAfterDialBound {
		t.Fatalf("the revoke came %v after the slow page connected, past %v: a run this slow cannot tell the revoke's close from ygo's own %v deadline on the stalled write",
			took, revokeAfterDialBound, ygoPeerWriteTimeout)
	}
	time.Sleep(2 * time.Second)

	// The end must reach the page within revokedEndBound of the revoke, which is what makes it
	// the revoke's: both deadlines ygo would end this socket on by itself fall after it.
	_ = reader.SetReadDeadline(revokedAt.Add(revokedEndBound))
	var ended error
	for ended == nil {
		_, _, ended = reader.ReadMessage()
	}
	requireReconnectableEnd(t, ended, fmt.Sprintf("the revoked page, reading two seconds after the revoke and within %v of it", revokedEndBound))

	want, err := service.Text(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read the server's document: %v", err)
	}
	// The reconnect is that page on a new socket of the same room, read-only now: the fleet that
	// revoked it admits it no other way. What it syncs must be the whole document.
	address := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/doc/" + artifactID + "?" + page(1, 1).Encode()
	reconnected := docstest.Dial(t, address, http.Header{"X-Dispatch-User": []string{"alice"}}, artifactID, crdt.New())
	waitForPeerDocument(t, reconnected, want)
}

// requireReconnectableEnd accepts the two ends a revoked socket may have, and only those: the
// close frame carrying the admission-revoked code, and the 1006 of a connection dropped with no
// close frame, which gorilla reports as a CloseError of 1006 when the drop lands inside a message
// and as the connection's own reset or EOF otherwise. A close with any other code, the protocol
// error gorilla raises on a frame cut in half, and a socket still open at the read deadline all
// fail.
func requireReconnectableEnd(t *testing.T, err error, what string) {
	t.Helper()
	var closed *gws.CloseError
	if errors.As(err, &closed) {
		if closed.Code == contracts.DocumentAdmissionRevokedCloseCode || closed.Code == gws.CloseAbnormalClosure {
			return
		}
	} else if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		// The drop reached the page as the connection's own end, with no close frame: 1006.
		return
	}
	t.Fatalf("%s ended with %v, want a close with %d or the %d of a connection dropped with no close frame",
		what, err, contracts.DocumentAdmissionRevokedCloseCode, gws.CloseAbnormalClosure)
}

// admitSocket is one document socket of room, admitted as presented and writable, on a request
// context of its own, as Service.ServeHTTP and authorize make it: the admission holds that
// context's cancel, and the context carries the admission.
func admitSocket(t *testing.T, service *Service, room, token string, presented schemaPage, writable bool) (context.Context, *schemaAdmission) {
	t.Helper()
	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	admission := &schemaAdmission{
		key:      schemaAdmissionKey{room: room, token: token},
		page:     presented,
		writable: writable,
		cancel:   cancel,
	}
	service.schemaAdmissions.admit(admission)
	return context.WithValue(ctx, connectionContextKey{}, &connectionState{admission: admission}), admission
}

// Two sockets of one room can present one token - the page's token is its schema_version alone -
// so a token's answer is the admission of the socket that sent it, which that socket's own request
// context carries, and never the admission of another socket of the same room and token. Here a
// page creating up to 1 and a page creating up to 2 are both admitted writable while the fleet
// reads version 2 and creates up to 2; the re-check under a fleet that creates up to 1 revokes the
// page creating up to 2, and the one creating up to 1 is still admitted. Each socket's token is
// then answered from its own admission - the live socket read-write and the revoked one read-only
// - whichever of the two asks first. Matching a token to a socket by its key, or by the order the
// two arrive in, answers one of these orders wrongly, since neither says which socket sent it.
func TestATokenIsAnsweredFromItsOwnSocketInEitherOrder(t *testing.T) {
	for _, order := range []struct {
		name         string
		revokedFirst bool
	}{
		{name: "the revoked socket's token first", revokedFirst: true},
		{name: "the live socket's token first"},
	} {
		t.Run(order.name, func(t *testing.T) {
			const room, token = "room", "2"
			admitting := schemaFleet{minVersion: 2, maxCreatable: 2}
			live := schemaPage{version: 2, creatable: 1, valid: true}
			losing := schemaPage{version: 2, creatable: 2, valid: true}
			for name, presented := range map[string]schemaPage{"creating up to 1": live, "creating up to 2": losing} {
				if !admitting.admits(presented, 0) {
					t.Fatalf("the page %s is not admitted writable by the fleet both sockets opened under", name)
				}
			}
			service := &Service{}
			liveCtx, liveAdmission := admitSocket(t, service, room, token, live, true)
			revokedCtx, revokedAdmission := admitSocket(t, service, room, token, losing, true)

			closed := service.schemaAdmissions.revokeUnless(schemaFleet{minVersion: 2, maxCreatable: 1})
			if len(closed) != 1 || closed[0] != revokedAdmission {
				t.Fatalf("the rollback revoked %d sockets, want the page creating up to 2 alone", len(closed))
			}
			if cause := context.Cause(revokedCtx); !errors.Is(cause, errSchemaRevoked) {
				t.Errorf("the revoked socket's context ended with %v, want the revoke's cause", cause)
			}
			if liveCtx.Err() != nil {
				t.Error("the socket the fleet still admits was closed with the revoked one")
			}

			answers := []struct {
				what string
				ctx  context.Context
				want bool
			}{
				{what: "the revoked socket's token", ctx: revokedCtx},
				{what: "the live socket's token", ctx: liveCtx, want: true},
			}
			if !order.revokedFirst {
				answers[0], answers[1] = answers[1], answers[0]
			}
			for _, answer := range answers {
				config, err := service.authorizeSchemaVersion(answer.ctx, room, token)
				if err != nil || config.ReadOnly == answer.want {
					t.Errorf("%s = %#v, %v; want read-write=%t", answer.what, config, err, answer.want)
				}
			}
			if liveAdmission.revoked {
				t.Error("answering the tokens revoked the socket the fleet still admits")
			}
		})
	}
}

// A revoked socket's page reconnects on the close, and that reconnect is admitted: nothing of the
// revoked socket stands in its way, however late the revoked socket's own token arrives. Here the
// revoked socket is still open and its token still unanswered, and the reconnect - read-only now,
// since the fleet no longer admits the page - is admitted and answered read-only, while the
// revoked socket's late token is answered read-only too.
func TestARevokedPagesReconnectIsAdmittedBesideItsLateToken(t *testing.T) {
	const room, token = "room", "2"
	service := &Service{}
	revokedCtx, revoked := admitSocket(t, service, room, token, schemaPage{version: 2, creatable: 2, valid: true}, true)
	if closed := service.schemaAdmissions.revokeUnless(schemaFleet{minVersion: 2, maxCreatable: 1}); len(closed) != 1 || closed[0] != revoked {
		t.Fatalf("the rollback revoked %d sockets, want the newer page's alone", len(closed))
	}

	reconnectCtx, _ := admitSocket(t, service, room, token, schemaPage{version: 2, creatable: 2, valid: true}, false)
	for _, answer := range []struct {
		what string
		ctx  context.Context
	}{
		{what: "the revoked socket's late token", ctx: revokedCtx},
		{what: "the read-only reconnect's token", ctx: reconnectCtx},
	} {
		if config, err := service.authorizeSchemaVersion(answer.ctx, room, token); err != nil || !config.ReadOnly {
			t.Errorf("%s = %#v, %v; want read-only", answer.what, config, err)
		}
	}
}
