package docs

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// The provider authenticates with its schema version alone. A token no socket of the room
// presented and is still waiting to authenticate is read-only: with no socket at all, and once the
// one socket that presented it has had its answer.
func TestATokenNoWaitingSocketPresentedIsReadOnly(t *testing.T) {
	service, artifactID, server := newAdmissionServer(t, false)
	version := strconv.Itoa(pmdoc.SchemaVersion())
	if config, err := service.authorizeSchemaVersion(artifactID, version); err != nil || !config.ReadOnly {
		t.Errorf("a token with no socket = %#v, %v; want read-only", config, err)
	}

	connection := mustDialPage(t, server.URL, artifactID, page(pmdoc.SchemaVersion(), pmdoc.SchemaCreatable()))
	if !tokenWritable(t, connection, artifactID, version) {
		t.Fatal("a current page's first token was answered read-only, want read-write")
	}
	if tokenWritable(t, connection, artifactID, version) {
		t.Error("the same token sent again, with no socket waiting for it, was answered read-write, want read-only")
	}
	other := mustDialPage(t, server.URL, artifactID, page(pmdoc.SchemaVersion(), pmdoc.SchemaCreatable()))
	if tokenWritable(t, other, artifactID, "1") {
		t.Error("a token naming a version no socket presented was answered read-write, want read-only")
	}
}

// Two sockets of one room presenting one schema version share one token, so the answer to a token
// is ambiguous while two waiting sockets were admitted differently. A second socket admitted
// otherwise than one still waiting for its token is refused, and its provider retries; once the
// first has its answer, the second is accepted.
func TestASecondSocketAdmittedOtherwiseWaitsForTheFirstSocketsToken(t *testing.T) {
	service, artifactID, server := newAdmissionServer(t, false)
	first := mustDialPage(t, server.URL, artifactID, page(1, 1))
	if !probeWritable(t, first, artifactID) {
		t.Fatal("an older page that represents everything the fleet creates was accepted read-only")
	}
	fleetProcess(t, service, 3, 2, 0)

	second, response, err := dialPage(server.URL, artifactID, page(1, 1))
	if err == nil {
		_ = second.Close()
		t.Fatal("a second socket admitted read-only beside a writable one waiting for the same token was accepted")
	}
	if !errors.Is(err, gws.ErrBadHandshake) || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the second socket's refusal = %v, response %#v; want a 401 handshake", err, response)
	}

	if !tokenWritable(t, first, artifactID, "1") {
		t.Error("the first socket's token was answered read-only, want its own read-write admission")
	}
	if socket, authenticated := admission(t, server.URL, artifactID, page(1, 1), "1"); socket || authenticated {
		t.Errorf("the second socket, once the first had its answer, was admitted writable=%t by its socket and %t by its token, want read-only",
			socket, authenticated)
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
