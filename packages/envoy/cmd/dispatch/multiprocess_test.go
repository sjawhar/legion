//go:build multiprocess

// Two envoy-dispatch processes serving one Postgres and one NATS, which is what production runs
// and what INFRA-66 made correct. Every row here starts real binaries and drives the real surface
// - HTTP, the processes' own logs, NATS's own ConsumerInfo and stream reads - because each
// mechanism the issue added is a property of two processes together and of nothing one process
// can show on its own.
//
// Its own build tag, and its own CI job (dispatch-multiprocess), for the reason the memory tests
// have theirs: a row here waits for a durable hand-over or for a maintenance loop's tick, tens of
// seconds each, and go test's default ten-minute deadline is one package's whole run.

package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	natsgo "github.com/nats-io/nats.go"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/docs/crafted"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/redeliver"
	"github.com/sjawhar/envoy/internal/dispatch/routes"
	"github.com/sjawhar/envoy/internal/dispatch/startup"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/session"
	envoystore "github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// multiprocessNATS is the NATS server this file's processes share: one of this test's own, with
// JetStream for Dispatch's outbox stream, running in this test process and ending with the test.
func multiprocessNATS(t *testing.T) string {
	t.Helper()
	_, url := testnats.Start(t)
	return url
}

// multiprocessSigningKey is the one DISPATCH_SIGNING_KEY every process of a pair carries: a
// session cookie minted by one process is presented to the other on the next request, and a
// process whose key set a live peer does not share refuses to start (processes.Register).
const multiprocessSigningKey = "multiprocess-signing-key"

// dispatchPair is two envoy-dispatch processes on one database and one NATS server, each reaching
// NATS through a proxy of its own (proxies), so a row can cut one process off NATS alone.
type dispatchPair struct {
	a, b     *dispatchProcess
	database *store.Store
	nats     string
	proxies  map[*dispatchProcess]*testnats.Proxy
}

// newPair opens the database and takes the NATS server the pair will share, and starts nothing:
// a row that must seed durable state the processes read at boot (the delivery settings their
// intakes wait for) or hold a lock their first pass will wait on does that between this and
// start.
func newPair(t *testing.T) *dispatchPair {
	t.Helper()
	return &dispatchPair{database: storetest.Open(t), nats: multiprocessNATS(t), proxies: map[*dispatchProcess]*testnats.Proxy{}}
}

// start starts both processes on the pair's database and NATS, each on a port held for it from the
// moment the kernel chose it (startDispatchProcessWith), in a home of its own and behind a NATS
// proxy of its own, both carrying multiprocessSigningKey, and returns once each answers /healthz.
// The two are started together rather than one after the other: several rows are about what the
// two do when their loops fire at the same moment, which a staggered start would hide.
func (p *dispatchPair) start(t *testing.T, env map[string]string) {
	t.Helper()
	p.startSettling(t, env, 0, 0)
}

// startSettling is start with each process's own settle delay (DISPATCH_TEST_SETTLE_DELAY): how
// long A, then B, waits after a live change before it settles the document. Zero keeps the
// server's default. Both run with the test hooks (DISPATCH_TEST_HOOKS=1), as every process
// startDispatchProcessWith starts does.
func (p *dispatchPair) startSettling(t *testing.T, env map[string]string, settleA, settleB time.Duration) {
	t.Helper()
	p.a = p.startProcessSettling(t, env, settleA)
	p.b = p.startProcessSettling(t, env, settleB)
	p.a.waitHealthy(t)
	p.b.waitHealthy(t)
}

// kill ends the process with SIGKILL (os.Process.Kill), as a task replaced mid-deploy or killed
// for memory ends, with no chance to finish anything, and returns once it has exited.
func (p *dispatchProcess) kill(t *testing.T) {
	t.Helper()
	if err := p.Cmd.Process.Kill(); err != nil {
		t.Fatalf("kill the process: %v", err)
	}
	<-p.Exited
}

// startProcess starts one process of the pair behind a NATS proxy of its own, and returns without
// waiting for it to be healthy.
func (p *dispatchPair) startProcess(t *testing.T, env map[string]string) *dispatchProcess {
	t.Helper()
	return p.startProcessSettling(t, env, 0)
}

// startProcessSettling is startProcess with a settle delay of the process's own
// (DISPATCH_TEST_SETTLE_DELAY): how long it waits after a live change before it settles the
// document. Zero keeps the server's default.
func (p *dispatchPair) startProcessSettling(t *testing.T, env map[string]string, settle time.Duration) *dispatchProcess {
	t.Helper()
	proxy := testnats.StartProxy(t, p.nats)
	process := startDispatchProcessWith(t, p.databaseURL(t),
		dispatchOptions{NATSURL: proxy.URL(), SigningKey: multiprocessSigningKey, Env: env, SettleDelay: settle})
	p.proxies[process] = proxy
	return process
}

// proxy is the NATS proxy the process reaches NATS through.
func (p *dispatchPair) proxy(one *dispatchProcess) *testnats.Proxy {
	return p.proxies[one]
}

// startPair is newPair and start together, for a row that seeds nothing first.
func startPair(t *testing.T, env map[string]string) *dispatchPair {
	t.Helper()
	pair := newPair(t)
	pair.start(t, env)
	return pair
}

// databaseURL is the connection string a process is started with: the database storetest cloned
// for this test (databaseURLOf).
func (p *dispatchPair) databaseURL(t *testing.T) string {
	t.Helper()
	return databaseURLOf(t, p.database)
}

// databaseURLOf is the connection string of database as its pool's configuration names it. The
// credential is written through url.UserPassword when the pool carries a password, since CI's
// Postgres takes one (DISPATCH_TEST_DATABASE_URL) where a trust-authenticated development server
// does not, and a URL built without it is refused with SQLSTATE 28P01 before the process can serve
// anything.
func databaseURLOf(t *testing.T, database *store.Store) string {
	t.Helper()
	config := database.Pool.Config().ConnConfig
	credential := url.User(config.User).String()
	if config.Password != "" {
		credential = url.UserPassword(config.User, config.Password).String()
	}
	return fmt.Sprintf("postgres://%s@%s:%d/%s?sslmode=disable",
		credential, config.Host, config.Port, config.Database)
}

// other is the process of the pair that is not one.
func (p *dispatchPair) other(one *dispatchProcess) *dispatchProcess {
	if one == p.a {
		return p.b
	}
	return p.a
}

// name is what a failure calls a process of the pair.
func (p *dispatchPair) name(one *dispatchProcess) string {
	if one == p.a {
		return "A"
	}
	return "B"
}

// ───── the live socket's event feed ─────────────────────────────────────────

// feedEvent is one event the event feed sent: its type and the event's JSON.
type feedEvent struct {
	eventType string
	data      string
}

// openEventFeed holds GET /ws/live open as a dashboard tab does, subscribed to the event feed, and
// reports every event it is sent on the returned channel, until the test ends or the socket does.
// One subscription, read to the end: a row about the live feed carrying the other process's event
// has to show that event arriving on the subscription the page already had, never on a
// resubscribe's catch-up.
func (p *dispatchProcess) openEventFeed(t *testing.T) <-chan feedEvent {
	t.Helper()
	conn := p.openLiveEvents(t)
	events := make(chan feedEvent, 256)
	go func() {
		defer close(events)
		for {
			var frame struct {
				Type  string          `json:"type"`
				Event json.RawMessage `json:"event"`
			}
			if err := conn.ReadJSON(&frame); err != nil {
				return
			}
			if frame.Type != "event" {
				continue
			}
			var event struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(frame.Event, &event); err != nil {
				return
			}
			events <- feedEvent{eventType: event.Type, data: string(frame.Event)}
		}
	}()
	return events
}

// ───── the API a row drives ─────────────────────────────────────────────────

// uniqueProjectKey is prefix with six random characters after it, within the project key's own
// grammar (contracts.ProjectKeyPattern). Every topic a row publishes on carries its project key,
// and it has to be this run's alone: a NATS server the harness keeps running outlives the run,
// and the notification stream recognises a repeat by its dedupe key and topic for
// DeliveryDuplicateWindow, three days. A fresh database always numbers its events from one, so a
// second run against a fixed project key would publish the very dedupe keys the stream still
// holds, have every JetStream publish suppressed as a duplicate, and read back nothing.
func uniqueProjectKey(t *testing.T, prefix string) string {
	t.Helper()
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("pick a project key: %v", err)
	}
	for i, b := range suffix {
		suffix[i] = alphabet[int(b)%len(alphabet)]
	}
	return prefix + string(suffix)
}

// createProject creates the project key, which the rows that make issues need first.
func (p *dispatchProcess) createProject(t *testing.T, key, name string) {
	t.Helper()
	p.call(t, http.MethodPost, "/api/v1/projects",
		fmt.Sprintf(`{"key":%q,"name":%q}`, key, name), http.StatusCreated)
}

// createIssueIn creates an issue in project with title and spec, and returns its key and its spec
// document's id.
func (p *dispatchProcess) createIssueIn(t *testing.T, project, title, spec string) (key, artifactID string) {
	t.Helper()
	body := fmt.Sprintf(`{"project":%q,"title":%q,"spec":%q}`, project, title, spec)
	created := p.call(t, http.MethodPost, "/api/v1/issues", body, http.StatusCreated)
	var issue struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}
	if err := json.Unmarshal(created, &issue); err != nil || issue.Key == "" {
		t.Fatalf("create issue: %v: %s", err, created)
	}
	return issue.Key, issue.PrimaryArtifactID
}

// documentText is the markdown GET /api/v1/artifacts/{id}/text answers: the document as this
// process renders it now.
func (p *dispatchProcess) documentText(t *testing.T, artifactID string) string {
	t.Helper()
	answer := p.call(t, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/text", "", http.StatusOK)
	var document struct {
		Markdown string `json:"markdown"`
	}
	if err := json.Unmarshal(answer, &document); err != nil {
		t.Fatalf("decode the document's text: %v: %s", err, answer)
	}
	return document.Markdown
}

// ───── document editors and the document test hooks ───────────────────────

// servingProcess is the X-Dispatch-Process header of the editor's websocket upgrade: the process
// that served its socket, or "" when the answer carried none.
func servingProcess(editor *docstest.Peer) string {
	return editor.UpgradeHeader.Get("X-Dispatch-Process")
}

// authenticateEditor authenticates the editor at the schema version the dashboard presents, as
// its provider does, and answers the scope the room granted: "read-write" or "readonly".
func authenticateEditor(t *testing.T, editor *docstest.Peer) string {
	t.Helper()
	scope, err := editor.Authenticate(strconv.Itoa(pmdoc.SchemaVersion()), 10*time.Second)
	if err != nil {
		t.Fatalf("authenticate the editor: %v", err)
	}
	return scope
}

// residentRooms lists the documents the process holds a room of now
// (GET /api/v1/artifacts/_test/rooms).
func (p *dispatchProcess) residentRooms(t *testing.T) []string {
	t.Helper()
	answer := p.call(t, http.MethodGet, "/api/v1/artifacts/_test/rooms", "", http.StatusOK)
	var listed struct {
		Rooms []string `json:"rooms"`
	}
	if err := json.Unmarshal(answer, &listed); err != nil {
		t.Fatalf("decode the resident rooms: %v: %s", err, answer)
	}
	return listed.Rooms
}

// roomCounters is what the process did with the document's rooms and owed settlement since it
// started: its room failures, lock-waiting compactions, room closes and settlement resumptions
// (GET /api/v1/artifacts/{id}/_test/room-counters).
func (p *dispatchProcess) roomCounters(t *testing.T, artifactID string) docs.RoomCounters {
	t.Helper()
	answer := p.call(t, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/_test/room-counters", "", http.StatusOK)
	var counters docs.RoomCounters
	if err := json.Unmarshal(answer, &counters); err != nil {
		t.Fatalf("decode the room counters: %v: %s", err, answer)
	}
	return counters
}

// holdPersistence holds, for duration from now, every append of a browser update the process's
// rooms of the document make (POST /api/v1/artifacts/{id}/_test/hold-persistence).
func (p *dispatchProcess) holdPersistence(t *testing.T, artifactID string, duration time.Duration) {
	t.Helper()
	p.call(t, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/_test/hold-persistence",
		fmt.Sprintf(`{"duration":%q}`, duration.String()), http.StatusNoContent)
}

// liveWritePausePath is the test hook route of the pause of the document's live writes at point.
func liveWritePausePath(artifactID string, point docs.LiveWritePausePoint) string {
	return "/api/v1/artifacts/" + artifactID + "/_test/live-write-pauses/" + string(point)
}

// armLiveWritePause makes the next live write of the document on this process park at point
// until releaseLiveWritePause. Arm it before the write is sent, then awaitLiveWriteParked.
func (p *dispatchProcess) armLiveWritePause(t *testing.T, artifactID string, point docs.LiveWritePausePoint) {
	t.Helper()
	p.call(t, http.MethodPut, liveWritePausePath(artifactID, point), "", http.StatusNoContent)
}

// awaitLiveWriteParked waits up to ten seconds for a write of the document to park at the pause
// armed at point, and fails the test when none does.
func (p *dispatchProcess) awaitLiveWriteParked(t *testing.T, artifactID string, point docs.LiveWritePausePoint) {
	t.Helper()
	answer := p.call(t, http.MethodGet, liveWritePausePath(artifactID, point)+"?wait=10s", "", http.StatusOK)
	var parked struct {
		Parked bool `json:"parked"`
	}
	if err := json.Unmarshal(answer, &parked); err != nil {
		t.Fatalf("decode the live write pause: %v: %s", err, answer)
	}
	if !parked.Parked {
		t.Fatalf("no live write of %s parked at %s within ten seconds:\n%s", artifactID, point, p.Output.String())
	}
}

// releaseLiveWritePause lets the write parked at point go on and disarms the pause, answering
// whether a write had parked there.
func (p *dispatchProcess) releaseLiveWritePause(t *testing.T, artifactID string, point docs.LiveWritePausePoint) bool {
	t.Helper()
	answer := p.call(t, http.MethodDelete, liveWritePausePath(artifactID, point), "", http.StatusOK)
	var released struct {
		Parked bool `json:"parked"`
	}
	if err := json.Unmarshal(answer, &released); err != nil {
		t.Fatalf("decode the live write release: %v: %s", err, answer)
	}
	return released.Parked
}

// ───── the live feed carries the other process's events ─────────────────────

// A dashboard tab is open on one process and the work happens on the other: the event of an issue
// created through B's API has to reach the event feed A's socket holds, on the subscription it
// already has. The live source of an open feed is not this process's in-memory broker, which hears
// only its own writes, but a relay over `listen dispatch_events`, so what either process commits
// every process publishes.
func TestALiveStreamOnOneProcessCarriesTheOtherProcessesEvent(t *testing.T) {
	pair := startPair(t, nil)
	project := uniqueProjectKey(t, "XPR")
	pair.a.createProject(t, project, "Cross process")

	events := pair.a.openEventFeed(t)
	key, _ := pair.b.createIssueIn(t, project, "Created on the other process", "spec")

	deadline := time.After(30 * time.Second)
	for {
		select {
		case event, open := <-events:
			if !open {
				t.Fatalf("the feed A held closed before B's event arrived:\n%s", pair.a.Output.String())
			}
			if event.eventType == "issue.created" && strings.Contains(event.data, `"`+key+`"`) {
				return
			}
		case <-deadline:
			t.Fatalf("the feed A held never carried the issue B created (%s):\nA:\n%s\nB:\n%s",
				key, pair.a.Output.String(), pair.b.Output.String())
		}
	}
}

// ───── test sessions ────────────────────────────────────────────────────────

const (
	// writesInFlight bounds the comment writes a row keeps waiting on an answer, and is how many
	// idle connections the rig's HTTP client keeps to each process.
	writesInFlight = 32
	// holderMachineID is the machine every session a row registers is on.
	holderMachineID = "multiprocess-sessions"
	// sessionHeartbeat is how often a registered session's entry is written again, as a session
	// plugin's heartbeat writes it: well inside session.ClaimStaleAfter and the session bucket's
	// TTL, so a role's holder stays fresh however long its row runs.
	sessionHeartbeat = 30 * time.Second
)

// sessionRig is the test's own NATS connection, the role-claim and session buckets it registers
// sessions in, and the HTTP client it writes comments through.
type sessionRig struct {
	natsURL  string
	conn     *natsgo.Conn
	roles    *envoystore.Registry
	sessions *session.SessionRegistry
	client   *http.Client
}

func newSessionRig(t *testing.T, natsURL string) *sessionRig {
	t.Helper()
	conn := testnats.Connect(t, natsURL)
	t.Cleanup(conn.Close)
	roles, err := envoystore.Open(conn)
	if err != nil {
		t.Fatalf("open the role registry: %v", err)
	}
	t.Cleanup(roles.StopWatch)
	sessions, err := session.OpenSessionRegistry(conn)
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	t.Cleanup(sessions.StopWatch)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = writesInFlight
	t.Cleanup(transport.CloseIdleConnections)
	return &sessionRig{
		natsURL:  natsURL,
		conn:     conn,
		roles:    roles,
		sessions: sessions,
		client:   &http.Client{Transport: transport, Timeout: 30 * time.Second},
	}
}

// registerSession registers id in the session bucket as a session plugin's own registration does:
// self-subscribed, with no port, updated now. It writes the entry again every sessionHeartbeat
// until the test ends.
func (r *sessionRig) registerSession(t *testing.T, id string) {
	t.Helper()
	entry := session.SessionEntry{
		MachineID:      holderMachineID,
		Title:          "Multiprocess session " + id,
		UpdatedAt:      time.Now().UnixMilli(),
		SelfSubscribed: true,
	}
	if err := r.sessions.Put(id, entry); err != nil {
		t.Fatalf("register session %s: %v", id, err)
	}
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		heartbeat := time.NewTicker(sessionHeartbeat)
		defer heartbeat.Stop()
		for {
			select {
			case <-stop:
				return
			case <-heartbeat.C:
				entry.UpdatedAt = time.Now().UnixMilli()
				_ = r.sessions.Put(id, entry)
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-stopped
	})
}

// registerHolder registers holder as a session (registerSession) and makes it role's holder.
func (r *sessionRig) registerHolder(t *testing.T, role, holder string) {
	t.Helper()
	r.registerSession(t, holder)
	if err := r.claimRole(role, holder); err != nil {
		t.Fatalf("claim role %s for %s: %v", role, holder, err)
	}
}

// claimRole writes holder's claim of role in the role-claim bucket, a hard claim that takes the
// role from whoever held it.
func (r *sessionRig) claimRole(role, holder string) error {
	_, err := r.roles.SetRole(holder, holderMachineID, role, false)
	return err
}

// commentWrite is one comment written: its id, or the error that kept it from being written.
type commentWrite struct {
	commentID string
	err       error
}

// postComment writes one comment with body on issue key through p, as the test's login, and
// returns its id.
func (r *sessionRig) postComment(p *dispatchProcess, key, body string) commentWrite {
	return r.writeComment(p, key, map[string]any{"body": body}, "")
}

// writeComment writes one comment on issue key through p, whose request body is fields, and returns
// its id. It writes as the test's login, or, when as names a session, as that session: with the
// shared agent token, as a session plugin calls Dispatch, and the session named as the actor.
func (r *sessionRig) writeComment(p *dispatchProcess, key string, fields map[string]any, as string) commentWrite {
	if as != "" {
		fields["actor"] = map[string]string{"kind": "session", "id": as}
	}
	payload, err := json.Marshal(fields)
	if err != nil {
		return commentWrite{err: err}
	}
	request, err := http.NewRequest(http.MethodPost, p.url("/api/v1/issues/"+key+"/comments"), bytes.NewReader(payload))
	if err != nil {
		return commentWrite{err: err}
	}
	request.Header.Set("Content-Type", "application/json")
	if as != "" {
		request.Header.Set("Authorization", "Bearer "+dispatchTestAgentToken)
	} else {
		request.Header.Set("X-Dispatch-User", dispatchTestLogin)
	}
	response, err := r.client.Do(request)
	if err != nil {
		return commentWrite{err: err}
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusCreated {
		return commentWrite{err: fmt.Errorf("comment on %s: status %d: %s", key, response.StatusCode, answer)}
	}
	var comment struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(answer, &comment); err != nil || comment.ID == "" {
		return commentWrite{err: fmt.Errorf("comment on %s: no id in %s: %v", key, answer, err)}
	}
	return commentWrite{commentID: comment.ID}
}

// written is the ids of the writes that succeeded; the failures are reported as errors on t,
// counted and with the first of them.
func written(t *testing.T, what string, writes []commentWrite) (ids []string, failures int) {
	t.Helper()
	var first error
	for _, write := range writes {
		if write.err != nil {
			failures++
			if first == nil {
				first = write.err
			}
			continue
		}
		ids = append(ids, write.commentID)
	}
	if failures > 0 {
		t.Errorf("%s: %d of %d writes failed; the first: %v", what, failures, len(writes), first)
	}
	return ids, failures
}

// expectsLaneReceipt reports whether msg is a role forward waiting for its receipt, by the rule a
// session plugin applies (expectsLaneReceipt in packages/envoy-client): a frame with a reply subject
// on an agent subject whose envelope's topic is another subject. Every other frame there, and every
// frame of a topic family, is a JetStream publish whose reply subject is the stream
// acknowledgement's, which an empty receipt would answer in the stream's place.
func expectsLaneReceipt(msg *natsgo.Msg, item contracts.Envelope) bool {
	return msg.Reply != "" && strings.HasPrefix(msg.Subject, contracts.AgentTopicPrefix) &&
		item.Topic != "" && item.Topic != msg.Subject
}

// testSession is one agent session as a host plugin runs it: a NATS connection of its own,
// subscribed to its agent subject or to the topic families it follows, that answers a role forward
// with the empty receipt (expectsLaneReceipt) and drops a repeat as the host does (createDeliveryDedupe
// in packages/envoy-client): a frame whose event id it has taken, or whose dedupe key names an event
// it has taken, is not taken again. A role forward's dedupe key is contracts.RoleForwardDedupePrefix
// and its event's own key, so that prefix is what maps it to its event. Every frame is recorded in
// arrival order.
type testSession struct {
	id       string
	natsURL  string
	subjects []string

	mu   sync.Mutex
	conn *natsgo.Conn
	// onForward, when set, decides each role forward before its receipt: false withholds the
	// receipt, and the session does not take the forward, as a host whose connection dropped with
	// it never had it.
	onForward func(sessionFrame) bool
	frames    []sessionFrame
	taken     map[string]bool
}

// sessionFrame is one frame as it reached a session.
type sessionFrame struct {
	at      time.Time
	subject string
	// event is the envelope's event id, dispatch-<id>; kind, owner, seq and commentID are read from
	// the event it carries. owner is the event's document, or else its issue: each has a seq of its
	// own.
	event     string
	kind      string
	owner     string
	seq       int
	commentID string
	// forward is a role forward; withheld one the session gave no receipt for and did not take;
	// repeat a frame of an event the session had already taken.
	forward, withheld, repeat bool
}

// eventRow is the events row the frame's event is.
func (f sessionFrame) eventRow(t *testing.T) int64 {
	t.Helper()
	id, err := strconv.ParseInt(strings.TrimPrefix(f.event, "dispatch-"), 10, 64)
	if err != nil {
		t.Fatalf("read the event row of event id %q: %v", f.event, err)
	}
	return id
}

// newTestSession connects session id to the rig's NATS and subscribes it to subjects, until the
// test ends.
func newTestSession(t *testing.T, rig *sessionRig, id string, subjects ...string) *testSession {
	t.Helper()
	s := &testSession{id: id, natsURL: rig.natsURL, subjects: subjects, taken: map[string]bool{}}
	if err := s.connect(); err != nil {
		t.Fatalf("connect session %s: %v", id, err)
	}
	t.Cleanup(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.conn.Close()
	})
	return s
}

// connect opens a connection with the session's subscriptions and makes it the session's.
func (s *testSession) connect() error {
	conn, err := natsgo.Connect(s.natsURL, natsgo.Timeout(time.Second))
	if err != nil {
		return err
	}
	for _, subject := range s.subjects {
		if _, err := conn.Subscribe(subject, s.receive); err != nil {
			conn.Close()
			return fmt.Errorf("subscribe to %s: %w", subject, err)
		}
	}
	if err := conn.Flush(); err != nil {
		conn.Close()
		return fmt.Errorf("flush the subscriptions: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conn = conn
	return nil
}

// reconnect drops the session's connection and opens a new one with the same subscriptions, as a
// host whose connection broke does. A frame sent between the two reaches neither.
func (s *testSession) reconnect() error {
	s.mu.Lock()
	old := s.conn
	s.mu.Unlock()
	old.Close()
	return s.connect()
}

func (s *testSession) receive(msg *natsgo.Msg) {
	at := time.Now()
	var item contracts.Envelope
	if err := json.Unmarshal(msg.Data, &item); err != nil {
		return
	}
	var event struct {
		Type       string  `json:"type"`
		IssueKey   *string `json:"issue_key"`
		ArtifactID *string `json:"artifact_id"`
		Seq        int     `json:"seq"`
		Payload    struct {
			ID string `json:"id"`
		} `json:"payload"`
	}
	_ = json.Unmarshal([]byte(item.Payload), &event)
	frame := sessionFrame{
		at: at, subject: msg.Subject, event: item.EventID, kind: event.Type, seq: event.Seq,
		forward: expectsLaneReceipt(msg, item),
	}
	switch {
	case event.ArtifactID != nil:
		frame.owner = "document " + *event.ArtifactID
	case event.IssueKey != nil:
		frame.owner = "issue " + *event.IssueKey
	}
	if event.Type == "comment.created" {
		frame.commentID = event.Payload.ID
	}
	if frame.forward {
		s.mu.Lock()
		decide := s.onForward
		s.mu.Unlock()
		if decide != nil && !decide(frame) {
			frame.withheld = true
		} else {
			_ = msg.Respond(nil)
		}
	}
	names := []string{item.EventID, strings.TrimPrefix(item.DedupeKey, contracts.RoleForwardDedupePrefix)}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !frame.withheld {
		for _, name := range names {
			frame.repeat = frame.repeat || (name != "" && s.taken[name])
		}
		if !frame.repeat {
			for _, name := range names {
				if name != "" {
					s.taken[name] = true
				}
			}
		}
	}
	s.frames = append(s.frames, frame)
}

// setOnForward sets the session's decision on each role forward (onForward).
func (s *testSession) setOnForward(decide func(sessionFrame) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onForward = decide
}

// allFrames is every frame that reached the session, in arrival order.
func (s *testSession) allFrames() []sessionFrame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.frames)
}

// takenFrames is the frames the session took, in arrival order: every frame but its repeats and
// the forwards it withheld a receipt for.
func (s *testSession) takenFrames() []sessionFrame {
	var taken []sessionFrame
	for _, frame := range s.allFrames() {
		if !frame.repeat && !frame.withheld {
			taken = append(taken, frame)
		}
	}
	return taken
}

// takenComments is when the session took each comment's comment.created event, by comment id.
func (s *testSession) takenComments() map[string]sessionFrame {
	taken := map[string]sessionFrame{}
	for _, frame := range s.takenFrames() {
		if frame.commentID != "" {
			taken[frame.commentID] = frame
		}
	}
	return taken
}

// requireTakenOnce fails the test for each comment of ids whose event the session has not taken.
// The session takes an event at most once, so each one it has is taken exactly once.
func (s *testSession) requireTakenOnce(t *testing.T, ids []string) {
	t.Helper()
	taken := s.takenComments()
	var missing []string
	for _, id := range ids {
		if _, ok := taken[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		t.Errorf("session %s has not taken %d of the %d comments addressed to it: %v", s.id, len(missing), len(ids), missing)
	}
}

// requireInOrder fails the test for every event the session took out of its owner's seq order:
// each document's and each issue's events must reach it in the order they were written.
func (s *testSession) requireInOrder(t *testing.T) {
	t.Helper()
	last := map[string]sessionFrame{}
	for _, frame := range s.takenFrames() {
		if frame.owner == "" {
			continue
		}
		if previous, ok := last[frame.owner]; ok && frame.seq <= previous.seq {
			t.Errorf("session %s took %s's event %s (seq %d, %s) after event %s (seq %d, %s)",
				s.id, frame.owner, frame.event, frame.seq, frame.kind, previous.event, previous.seq, previous.kind)
		}
		last[frame.owner] = frame
	}
}

// ───── the outbox ───────────────────────────────────────────────────────────

// outboxRoleLane is the role the outbox row's issue is routed to, and outboxRoleHolder its holder:
// each event is forwarded to the holder's agent subject with a Request, which a core subscriber of
// that subject is handed every time it is sent, whatever the stream's duplicate window keeps. It
// is where a second publish of one event shows up.
const (
	outboxRoleLane   = "multiprocess-outbox"
	outboxRoleHolder = "ses_multiprocess_outbox"
)

// Both processes run an outbox on one events table, and one of them at a time holds the publisher
// hold and publishes, so one event is published once however many processes run: every row
// reaches published_at, the stream holds one retained message per (dedupe key, topic), and the
// role's holder - a core subscriber that answers every forward with its receipt, as a session
// plugin does - takes exactly one forward per event. Nothing crosses the role's own topic.
func TestTheOutboxPublishesEachEventOnceAcrossBothProcesses(t *testing.T) {
	pair := startPair(t, nil)
	rig := newSessionRig(t, pair.nats)
	conn := rig.conn
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("JetStream context: %v", err)
	}
	// This row counts only what it publishes itself: readStreamMessagesFrom starts past this head.
	head := streamHead(t, js)

	forwards := make(chan contracts.Envelope, 256)
	holderSub, err := conn.Subscribe(contracts.AgentSubject(outboxRoleHolder), func(msg *natsgo.Msg) {
		var item contracts.Envelope
		if err := json.Unmarshal(msg.Data, &item); err == nil && strings.HasPrefix(item.DedupeKey, contracts.RoleForwardDedupePrefix) {
			forwards <- item
			_ = msg.Respond(nil)
		}
	})
	if err != nil {
		t.Fatalf("subscribe the role's holder: %v", err)
	}
	t.Cleanup(func() { _ = holderSub.Unsubscribe() })
	lane := make(chan contracts.Envelope, 256)
	laneSub, err := conn.Subscribe(contracts.RoleTopicPrefix+outboxRoleLane, func(msg *natsgo.Msg) {
		var item contracts.Envelope
		if err := json.Unmarshal(msg.Data, &item); err == nil {
			lane <- item
		}
	})
	if err != nil {
		t.Fatalf("subscribe to the role's topic: %v", err)
	}
	t.Cleanup(func() { _ = laneSub.Unsubscribe() })
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the subscriptions: %v", err)
	}
	rig.registerHolder(t, outboxRoleLane, outboxRoleHolder)

	project := uniqueProjectKey(t, "OBX")
	pair.a.createProject(t, project, "Outbox")
	key, _ := pair.a.createIssueIn(t, project, "Published once", "spec")
	pair.a.call(t, http.MethodPatch, "/api/v1/issues/"+key,
		fmt.Sprintf(`{"route":"role:%s"}`, outboxRoleLane), http.StatusOK)

	// Writes through both processes' APIs, interleaved, so both outboxes are scanning the same
	// unpublished rows while they are being written.
	const comments = 8
	for i := range comments {
		writer := pair.a
		if i%2 == 1 {
			writer = pair.b
		}
		writer.call(t, http.MethodPost, "/api/v1/issues/"+key+"/comments",
			fmt.Sprintf(`{"body":"comment %d"}`, i), http.StatusCreated)
	}

	waitForEveryEventPublished(t, pair.database, 60*time.Second)

	// The holder's forwards are read after every event is marked published, plus a settling read:
	// a second publish of an already published event would arrive after the mark, and this is
	// where it would be seen.
	received := drainEnvelopes(t, forwards, 3*time.Second)
	perEvent := map[string]int{}
	for _, item := range received {
		if item.Topic != contracts.RoleTopicPrefix+outboxRoleLane {
			t.Errorf("the holder received a forward of event %s on topic %q, want the role's", item.SourceEventID, item.Topic)
		}
		perEvent[item.SourceEventID]++
	}
	if len(perEvent) < comments {
		t.Errorf("the role's holder took %d events, want at least the %d comments:\n%v", len(perEvent), comments, perEvent)
	}
	for eventID, count := range perEvent {
		if count != 1 {
			t.Errorf("event %s reached the role's holder %d times; two processes publishing one event is what the publisher hold prevents:\n%v",
				eventID, count, perEvent)
		}
	}
	if carried := len(lane); carried != 0 {
		t.Errorf("the role's topic carried %d envelopes, want none: the outbox forwards to the holder itself", carried)
	}

	perMessage := map[string]int{}
	for _, message := range readStreamMessagesFrom(t, js, head) {
		perMessage[message.item.DedupeKey+" on "+message.item.Topic]++
	}
	if len(perMessage) == 0 {
		t.Fatalf("the notification stream holds nothing this row published")
	}
	for message, count := range perMessage {
		if count != 1 {
			t.Errorf("the stream holds %d retained messages for %s; want one", count, message)
		}
	}
}

// waitForEveryEventPublished waits for every event row the outbox publishes to carry published_at.
// A row whose payload says `pending` is a subscription command the outbox never publishes, so it
// is not waited for; the predicate is spelled as the outbox's own (outbox.readBatchSQL).
func waitForEveryEventPublished(t *testing.T, database *store.Store, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		var unpublished int
		if err := database.Pool.QueryRow(context.Background(), `
			select count(*) from events
			where published_at is null and (payload ->> 'pending') is distinct from 'true'
		`).Scan(&unpublished); err != nil {
			t.Fatalf("count unpublished events: %v", err)
		}
		if unpublished == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d events were still unpublished after %v", unpublished, within)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// streamHead is the last sequence the notification stream holds when a row begins, read before
// that row publishes anything: readStreamFrom reads past it, so nothing already on the stream is
// counted as the row's. A row starts a NATS server of its own, whose stream begins empty, so the
// head it reads is zero; reading it is what keeps that an assumption the row does not make.
func streamHead(t *testing.T, js natsgo.JetStreamContext) uint64 {
	t.Helper()
	info, err := js.StreamInfo(bus.Stream)
	if err != nil {
		if errors.Is(err, natsgo.ErrStreamNotFound) {
			return 0
		}
		t.Fatalf("read the notification stream: %v", err)
	}
	return info.State.LastSeq
}

// drainEnvelopes reads what a subscription has collected and keeps reading until quiet is up with
// nothing more arriving.
func drainEnvelopes(t *testing.T, from <-chan contracts.Envelope, quiet time.Duration) []contracts.Envelope {
	t.Helper()
	var items []contracts.Envelope
	for {
		select {
		case item := <-from:
			items = append(items, item)
		case <-time.After(quiet):
			return items
		}
	}
}

// ───── the outbox's publisher across two processes ──────────────────────────

const (
	// takeOverWithin bounds a take-over: from the moment the publisher is killed, cut off NATS or
	// frozen, the other process takes the publisher hold, publishes its first batch and a session
	// takes a comment written since, each within it.
	takeOverWithin = 10 * time.Second
	// outboxDeliveryWithin bounds the wait for what a row wrote to reach every session it is
	// addressed to.
	outboxDeliveryWithin = 60 * time.Second
	// frozenFor is how long the frozen-publisher row holds the publisher stopped.
	frozenFor = 10 * time.Second
	// resumedQuiet is how long the frozen-publisher row watches the sessions once the resumed
	// process has logged that it lost the hold: anything it sent on resuming arrives in this.
	resumedQuiet = 3 * time.Second
	// sceneWritesAtOnce is how many comments writeAll keeps waiting on an answer at once, so both
	// processes commit events of one issue at the same moment.
	sceneWritesAtOnce = 8

	// outboxLogPrefix starts every message the outbox logs; the messages below follow it, each line
	// carrying the process's instance.
	outboxLogPrefix = "dispatch outbox: "
	tookHold        = "took the publisher hold"
	releasedHold    = "released the publisher hold"
	lostHold        = "lost the publisher hold"
	sentBatch       = "sent batch"
)

// healthReport is what a process's /healthz says of NATS and of the outbox. OutboxPublisher is nil
// when the body has no such field.
type healthReport struct {
	NATS            *bool `json:"nats"`
	OutboxPublisher *bool `json:"outbox_publisher"`
}

// publishing reports whether the report says the process publishes the outbox.
func (r healthReport) publishing() bool { return r.OutboxPublisher != nil && *r.OutboxPublisher }

func (r healthReport) String() string {
	if r.OutboxPublisher == nil {
		return "no outbox_publisher field"
	}
	return fmt.Sprintf("outbox_publisher %v", *r.OutboxPublisher)
}

// healthClient is the client /healthz is read with: a process that does not answer within its
// timeout is a failed read, not a stuck row.
var healthClient = &http.Client{Timeout: 2 * time.Second}

// readHealthReport reads the process's /healthz, whose body a 503 carries too.
func (p *dispatchProcess) readHealthReport() (healthReport, error) {
	response, err := healthClient.Get(p.url("/healthz"))
	if err != nil {
		return healthReport{}, err
	}
	defer response.Body.Close()
	var report healthReport
	if err := json.NewDecoder(response.Body).Decode(&report); err != nil {
		return healthReport{}, fmt.Errorf("decode /healthz (status %d): %w", response.StatusCode, err)
	}
	return report, nil
}

// waitForPublisher waits up to within for exactly one process of the pair to report
// outbox_publisher true, and answers which.
func waitForPublisher(t *testing.T, pair *dispatchPair, within time.Duration) *dispatchProcess {
	t.Helper()
	deadline := time.Now().Add(within)
	var last string
	for {
		a, errA := pair.a.readHealthReport()
		b, errB := pair.b.readHealthReport()
		if errA == nil && errB == nil && a.publishing() != b.publishing() {
			if a.publishing() {
				return pair.a
			}
			return pair.b
		}
		last = fmt.Sprintf("A: %v %v; B: %v %v", a, errA, b, errB)
		if time.Now().After(deadline) {
			t.Fatalf("no one process of the pair reported outbox_publisher true within %v; the last reads: %s", within, last)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// signal sends the process sig and returns when it was sent.
func (p *dispatchProcess) signal(t *testing.T, sig syscall.Signal) time.Time {
	t.Helper()
	at := time.Now()
	if err := p.Cmd.Process.Signal(sig); err != nil {
		t.Fatalf("send %v to the process: %v", sig, err)
	}
	return at
}

// outboxLine is one line a process's outbox logged about its publisher hold or a batch: when, its
// message after outboxLogPrefix, its instance, a release's reason and a batch's first send.
type outboxLine struct {
	at       time.Time
	message  string
	instance string
	reason   string
	started  time.Time
	text     string
}

// logAttribute is one key=value of a slog text line, its value quoted or bare.
var logAttribute = regexp.MustCompile(`(\w+)=("(?:[^"\\]|\\.)*"|\S*)`)

// outboxLines is every line the process's outbox has logged, in order.
func (p *dispatchProcess) outboxLines(t *testing.T) []outboxLine {
	t.Helper()
	var lines []outboxLine
	for _, text := range p.logLines(`msg="` + outboxLogPrefix) {
		attributes := map[string]string{}
		for _, match := range logAttribute.FindAllStringSubmatch(text, -1) {
			value := match[2]
			if unquoted, err := strconv.Unquote(value); err == nil {
				value = unquoted
			}
			if _, ok := attributes[match[1]]; !ok {
				attributes[match[1]] = value
			}
		}
		at, err := time.Parse(time.RFC3339Nano, attributes["time"])
		if err != nil {
			t.Fatalf("read the time of the line %q: %v", text, err)
		}
		line := outboxLine{
			at: at, message: strings.TrimPrefix(attributes["msg"], outboxLogPrefix),
			instance: attributes["instance"], reason: attributes["reason"], text: text,
		}
		if started, ok := attributes["started"]; ok {
			if line.started, err = time.Parse(time.RFC3339Nano, started); err != nil {
				t.Fatalf("read the batch start of the line %q: %v", text, err)
			}
		}
		lines = append(lines, line)
	}
	return lines
}

// waitForOutboxLine waits up to within for the process to log message at or after since, and
// returns the first such line. A line's time is in milliseconds, so since is too.
func (p *dispatchProcess) waitForOutboxLine(t *testing.T, message string, since time.Time, within time.Duration) outboxLine {
	t.Helper()
	from := since.Truncate(time.Millisecond)
	deadline := time.Now().Add(within)
	for {
		for _, line := range p.outboxLines(t) {
			if line.message == message && !line.at.Before(from) {
				return line
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the process logged no %q at or after %s within %v", outboxLogPrefix+message, since.Format(time.RFC3339Nano), within)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// publishingPeriod is one span over which a process held the publisher hold: from its took line to
// the released or lost line after it, or still open.
type publishingPeriod struct {
	instance string
	from, to time.Time
	open     bool
}

// publishingPeriods is every period the lines, one process's, show it holding the publisher hold.
func publishingPeriods(lines []outboxLine) []publishingPeriod {
	var periods []publishingPeriod
	for _, line := range lines {
		switch line.message {
		case tookHold:
			periods = append(periods, publishingPeriod{instance: line.instance, from: line.at, open: true})
		case releasedHold, lostHold:
			if last := len(periods) - 1; last >= 0 && periods[last].open {
				periods[last].to, periods[last].open = line.at, false
			}
		}
	}
	return periods
}

// end is when the period ended, or now while it is open.
func (p publishingPeriod) end(now time.Time) time.Time {
	if p.open {
		return now
	}
	return p.to
}

// holds reports whether at falls inside the period.
func (p publishingPeriod) holds(at, now time.Time) bool {
	return !at.Before(p.from) && !at.After(p.end(now))
}

// outboxScene is a project whose issues a row writes comments on through the pair's processes, and
// the three sessions those comments are addressed to:
//
//   - watcher follows every issue's topic family;
//   - holder holds role, to which the role issues are routed, and takes each forward with its
//     receipt;
//   - author wrote the root comment on each reply issue, and every comment the row writes there
//     replies to it, which reaches the author on its agent subject (an author route).
type outboxScene struct {
	pair       *dispatchPair
	rig        *sessionRig
	role       string
	issues     []string
	roleIssues map[string]bool
	// roots is each reply issue's root comment.
	roots                   map[string]string
	watcher, holder, author *testSession

	mu      sync.Mutex
	written []sceneWrite
}

// sceneWrite is one comment a row wrote, on issue, the write started at started.
type sceneWrite struct {
	commentID string
	issue     string
	started   time.Time
}

// newOutboxScene creates a project of issues issues through A: the first roleIssues routed to the
// scene's role, whose holder is the holder session, the next replyIssues carrying a root comment
// the author session wrote, the rest neither. It returns once every event of the setup is
// published.
func newOutboxScene(t *testing.T, pair *dispatchPair, issues, roleIssues, replyIssues int) *outboxScene {
	t.Helper()
	rig := newSessionRig(t, pair.nats)
	project := uniqueProjectKey(t, "OBX")
	name := strings.ToLower(project)
	scene := &outboxScene{pair: pair, rig: rig, role: "outbox-" + name, roleIssues: map[string]bool{}, roots: map[string]string{}}
	pair.a.createProject(t, project, "Outbox "+project)
	var families []string
	for index := range issues {
		key, _ := pair.a.createIssueIn(t, project, fmt.Sprintf("Outbox %d", index+1), "Outbox.")
		scene.issues = append(scene.issues, key)
		families = append(families, "notifications.dispatch.issue."+key+".>")
	}
	scene.watcher = newTestSession(t, rig, "ses_"+name+"_watcher", families...)
	holder, author := "ses_"+name+"_holder", "ses_"+name+"_author"
	scene.holder = newTestSession(t, rig, holder, contracts.AgentSubject(holder))
	scene.author = newTestSession(t, rig, author, contracts.AgentSubject(author))
	rig.registerHolder(t, scene.role, holder)
	for index, key := range scene.issues {
		switch {
		case index < roleIssues:
			scene.roleIssues[key] = true
			pair.a.call(t, http.MethodPatch, "/api/v1/issues/"+key,
				fmt.Sprintf(`{"route":"role:%s"}`, scene.role), http.StatusOK)
		case index < roleIssues+replyIssues:
			root := rig.writeComment(pair.a, key, map[string]any{"body": "The thread every comment here replies to."}, author)
			if root.err != nil {
				t.Fatalf("write the root comment on %s as %s: %v", key, author, root.err)
			}
			scene.roots[key] = root.commentID
		}
	}
	waitForEveryEventPublished(t, pair.database, outboxDeliveryWithin)
	return scene
}

// sessions is the scene's sessions.
func (s *outboxScene) sessions() []*testSession {
	return []*testSession{s.watcher, s.holder, s.author}
}

// write writes one comment on issue through p - a reply to the root comment on a reply issue, a
// comment of its own elsewhere - and records it.
func (s *outboxScene) write(p *dispatchProcess, issue, body string) error {
	fields := map[string]any{"body": body}
	if root, ok := s.roots[issue]; ok {
		fields["reply_to"] = root
	}
	started := time.Now()
	write := s.rig.writeComment(p, issue, fields, "")
	if write.err != nil {
		return write.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.written = append(s.written, sceneWrite{commentID: write.commentID, issue: issue, started: started})
	return nil
}

// writerFor is the process the index-th write of a round robin over issues goes through:
// consecutive writes alternate between writers, and so do each issue's.
func writerFor(index, issues int, writers []*dispatchProcess) *dispatchProcess {
	return writers[(index/issues+index%issues)%len(writers)]
}

// writeAll writes count comments, round robin over the scene's issues, through writers in turn
// (writerFor), sceneWritesAtOnce at a time, and fails the test for each write that failed.
func (s *outboxScene) writeAll(t *testing.T, count int, writers ...*dispatchProcess) {
	t.Helper()
	failures := make(chan error, count)
	slots := make(chan struct{}, sceneWritesAtOnce)
	var writes sync.WaitGroup
	for index := range count {
		slots <- struct{}{}
		writes.Go(func() {
			defer func() { <-slots }()
			issue := s.issues[index%len(s.issues)]
			if err := s.write(writerFor(index, len(s.issues), writers), issue, fmt.Sprintf("Comment %d.", index)); err != nil {
				failures <- err
			}
		})
	}
	writes.Wait()
	close(failures)
	for err := range failures {
		t.Errorf("write a comment: %v", err)
	}
}

// writeAtRate writes a comment rate times a second, round robin over the scene's issues, through
// writers in turn (writerFor), until the returned stop is called. stop returns once the writing
// has stopped and fails the test for each write that failed; calling it again does nothing.
func (s *outboxScene) writeAtRate(t *testing.T, rate int, writers ...*dispatchProcess) (stop func()) {
	t.Helper()
	done, stopped := make(chan struct{}), make(chan struct{})
	var failures []error
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(time.Second / time.Duration(rate))
		defer ticker.Stop()
		for index := 0; ; index++ {
			issue := s.issues[index%len(s.issues)]
			if err := s.write(writerFor(index, len(s.issues), writers), issue, fmt.Sprintf("Comment %d at %d a second.", index, rate)); err != nil {
				failures = append(failures, err)
			}
			select {
			case <-done:
				return
			case <-ticker.C:
			}
		}
	}()
	return sync.OnceFunc(func() {
		close(done)
		<-stopped
		for _, err := range failures {
			t.Errorf("write a comment: %v", err)
		}
	})
}

// writtenCount is how many comments the scene has written.
func (s *outboxScene) writtenCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.written)
}

// addressed is each session's share of the written comments: every one for the watcher, those on
// a role issue for the holder, those on a reply issue for the author.
func (s *outboxScene) addressed() map[*testSession][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	addressed := map[*testSession][]string{}
	for _, write := range s.written {
		addressed[s.watcher] = append(addressed[s.watcher], write.commentID)
		if s.roleIssues[write.issue] {
			addressed[s.holder] = append(addressed[s.holder], write.commentID)
		}
		if _, ok := s.roots[write.issue]; ok {
			addressed[s.author] = append(addressed[s.author], write.commentID)
		}
	}
	return addressed
}

// deliveredCount is how many of the written comments the watcher has taken.
func (s *outboxScene) deliveredCount() int {
	taken := s.watcher.takenComments()
	count := 0
	for _, id := range s.addressed()[s.watcher] {
		if _, ok := taken[id]; ok {
			count++
		}
	}
	return count
}

// delivered reports whether every session has taken every written comment addressed to it.
func (s *outboxScene) delivered() bool {
	for session, ids := range s.addressed() {
		taken := session.takenComments()
		for _, id := range ids {
			if _, ok := taken[id]; !ok {
				return false
			}
		}
	}
	return true
}

// waitDelivered waits up to outboxDeliveryWithin for every session to take every written comment
// addressed to it, and fails the test naming what each has not taken when they do not.
func (s *outboxScene) waitDelivered(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(outboxDeliveryWithin)
	for !s.delivered() {
		if time.Now().After(deadline) {
			addressed := s.addressed()
			for _, session := range s.sessions() {
				session.requireTakenOnce(t, addressed[session])
			}
			t.Fatalf("the written comments did not reach every session they are addressed to within %v", outboxDeliveryWithin)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// requireOnceAndInOrder requires each session to have taken each written comment addressed to it,
// once, and every event it took in its owner's seq order.
func (s *outboxScene) requireOnceAndInOrder(t *testing.T) {
	t.Helper()
	addressed := s.addressed()
	for _, session := range s.sessions() {
		session.requireTakenOnce(t, addressed[session])
		session.requireInOrder(t)
	}
}

// firstArrivalSince is the first time any session took a comment whose write started at or after
// at, and false while none has.
func (s *outboxScene) firstArrivalSince(at time.Time) (time.Time, bool) {
	s.mu.Lock()
	since := map[string]bool{}
	for _, write := range s.written {
		if !write.started.Before(at) {
			since[write.commentID] = true
		}
	}
	s.mu.Unlock()
	var first time.Time
	for _, session := range s.sessions() {
		for id, frame := range session.takenComments() {
			if since[id] && (first.IsZero() || frame.at.Before(first)) {
				first = frame.at
			}
		}
	}
	return first, !first.IsZero()
}

// framesSince is every frame, repeats and withheld forwards included, that reached a session at or
// after at.
func (s *outboxScene) framesSince(at time.Time) map[*testSession][]sessionFrame {
	frames := map[*testSession][]sessionFrame{}
	for _, session := range s.sessions() {
		for _, frame := range session.allFrames() {
			if !frame.at.Before(at) {
				frames[session] = append(frames[session], frame)
			}
		}
	}
	return frames
}

// requireTakeOver waits for p to take the publisher hold at or after since, then to log its first
// batch, and for a session to take a comment written since since; and requires each within
// takeOverWithin of since, the moment what happened to the publisher before p.
func (s *outboxScene) requireTakeOver(t *testing.T, p *dispatchProcess, since time.Time, what string) {
	t.Helper()
	took := p.waitForOutboxLine(t, tookHold, since, 2*takeOverWithin)
	batch := p.waitForOutboxLine(t, sentBatch, took.at, 2*takeOverWithin)
	var arrived time.Time
	waitUntil(t, 2*takeOverWithin, "a session taking a comment written after "+what, func() bool {
		var ok bool
		arrived, ok = s.firstArrivalSince(since)
		return ok
	})
	t.Logf("after %s, the other process took the publisher hold in %v and logged its first batch in %v, and a session took a comment written since in %v",
		what, took.at.Sub(since).Round(time.Millisecond), batch.at.Sub(since).Round(time.Millisecond), arrived.Sub(since).Round(time.Millisecond))
	for _, step := range []struct {
		what string
		at   time.Time
	}{
		{"the other process took the publisher hold", took.at},
		{"the other process logged its first batch", batch.at},
		{"a session took a comment written since", arrived},
	} {
		if after := step.at.Sub(since); after > takeOverWithin {
			t.Errorf("%s %v after %s, past the %v bound", step.what, after.Round(time.Millisecond), what, takeOverWithin)
		}
	}
}

// requireRunning fails the test for each process of the pair that has exited.
func (p *dispatchPair) requireRunning(t *testing.T) {
	t.Helper()
	for _, process := range []*dispatchProcess{p.a, p.b} {
		select {
		case <-process.Exited:
			t.Errorf("process %s exited:\n%s", p.name(process), process.Output.String())
		default:
		}
	}
}

// publisherSample is one read of both processes' /healthz, after its time into the sampling.
type publisherSample struct {
	after time.Duration
	a, b  healthReport
	err   error
}

// sampleOutboxPublisher reads both processes' /healthz every 100 ms, the two at once, for span, and
// returns the func that waits for the last read and answers them all.
func sampleOutboxPublisher(pair *dispatchPair, span time.Duration) func() []publisherSample {
	started := time.Now()
	done := make(chan []publisherSample, 1)
	go func() {
		var samples []publisherSample
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for range tick.C {
			after := time.Since(started)
			if after > span {
				break
			}
			sample := publisherSample{after: after}
			var errA, errB error
			var reads sync.WaitGroup
			reads.Go(func() { sample.a, errA = pair.a.readHealthReport() })
			reads.Go(func() { sample.b, errB = pair.b.readHealthReport() })
			reads.Wait()
			sample.err = errors.Join(errA, errB)
			samples = append(samples, sample)
		}
		done <- samples
	}()
	return func() []publisherSample { return <-done }
}

// startOutboxPair is startPair for a row about the outbox's publisher, which logs both processes'
// output when the row fails: a hand-over is told from both sides.
func startOutboxPair(t *testing.T) *dispatchPair {
	t.Helper()
	pair := startPair(t, nil)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("A's output:\n%s\nB's output:\n%s", pair.a.Output.String(), pair.b.Output.String())
		}
	})
	return pair
}

// Two processes run an outbox each, and one of them at a time holds the publisher hold: /healthz,
// read on both every 100 ms, never shows outbox_publisher true on both, and from 10 s after the
// start shows it on exactly one. Each process's outbox lines name its own instance, the periods
// over which the two held the hold (from a took line to the released or lost line after it) never
// overlap, and every batch either logged was started and finished inside one of its own periods.
// The sessions take every comment written meanwhile once and in order.
func TestOneProcessPublishesTheOutboxAtATime(t *testing.T) {
	pair := startOutboxPair(t)
	samples := sampleOutboxPublisher(pair, 12*time.Second)
	scene := newOutboxScene(t, pair, 3, 1, 1)
	stop := scene.writeAtRate(t, 4, pair.a, pair.b)
	sampled := samples()
	stop()

	late := 0
	for _, sample := range sampled {
		if sample.err != nil {
			t.Errorf("the read of /healthz %v in failed: %v", sample.after.Round(time.Millisecond), sample.err)
			continue
		}
		publishing := 0
		for _, report := range []healthReport{sample.a, sample.b} {
			if report.publishing() {
				publishing++
			}
		}
		if publishing > 1 {
			t.Errorf("/healthz %v in: both processes report outbox_publisher true", sample.after.Round(time.Millisecond))
		}
		if sample.after >= 10*time.Second {
			late++
			if publishing != 1 {
				t.Errorf("/healthz %v in: A %v, B %v; want exactly one publishing from 10 s on",
					sample.after.Round(time.Millisecond), sample.a, sample.b)
			}
		}
	}
	if late == 0 {
		t.Errorf("none of the %d reads of /healthz was 10 s in or later", len(sampled))
	}

	now := time.Now()
	type processPeriod struct {
		process *dispatchProcess
		publishingPeriod
	}
	var periods []processPeriod
	owners := map[string]*dispatchProcess{}
	batches := 0
	var holder *dispatchProcess
	for _, process := range []*dispatchProcess{pair.a, pair.b} {
		lines := process.outboxLines(t)
		instances := map[string]bool{}
		for _, line := range lines {
			instances[line.instance] = true
			if owner, ok := owners[line.instance]; ok && owner != process {
				t.Errorf("both processes logged lines of instance %q", line.instance)
			}
			owners[line.instance] = process
		}
		if len(instances) > 1 || instances[""] {
			t.Errorf("process %s's outbox lines name the instances %v, want its one", pair.name(process), slices.Collect(maps.Keys(instances)))
		}
		own := publishingPeriods(lines)
		for _, line := range lines {
			if line.message != sentBatch {
				continue
			}
			batches++
			inside := slices.ContainsFunc(own, func(period publishingPeriod) bool {
				return period.instance == line.instance && period.holds(line.started, now) && period.holds(line.at, now)
			})
			if line.started.IsZero() || !inside {
				t.Errorf("process %s logged a batch outside every period it held the publisher hold: %s", pair.name(process), line.text)
			}
		}
		for _, period := range own {
			periods = append(periods, processPeriod{process, period})
			if period.open {
				holder = process
			}
		}
	}
	for index, one := range periods {
		for _, other := range periods[index+1:] {
			if one.process != other.process && one.from.Before(other.end(now)) && other.from.Before(one.end(now)) {
				t.Errorf("process %s held the publisher hold from %s to %s, and process %s from %s to %s",
					pair.name(one.process), one.from.Format(time.RFC3339Nano), one.end(now).Format(time.RFC3339Nano),
					pair.name(other.process), other.from.Format(time.RFC3339Nano), other.end(now).Format(time.RFC3339Nano))
			}
		}
	}
	if batches == 0 {
		t.Errorf("neither process logged %q", outboxLogPrefix+sentBatch)
	}
	if last := sampled[len(sampled)-1]; holder == nil || !map[*dispatchProcess]healthReport{pair.a: last.a, pair.b: last.b}[holder].publishing() {
		t.Errorf("the process whose publisher hold is open in the logs is not the one the last /healthz read reports publishing (A %v, B %v)", last.a, last.b)
	}
	scene.waitDelivered(t)
	scene.requireOnceAndInOrder(t)
}

// 200 comments on 10 issues, written through both processes in turn and several at a time: four
// issues routed to a role, three replying to a session's thread, three neither. Whichever process
// publishes, every session takes each comment addressed to it once and each issue's events in seq
// order: the watcher on the issues' topic families, the role's holder through the outbox's forward
// with its receipt, the thread's author on its agent subject.
func TestEachSessionTakesEachEventOnceAndInOrderAcrossTwoProcesses(t *testing.T) {
	pair := startOutboxPair(t)
	scene := newOutboxScene(t, pair, 10, 4, 3)
	scene.writeAll(t, 200, pair.a, pair.b)
	scene.waitDelivered(t)
	scene.requireOnceAndInOrder(t)
	addressed := scene.addressed()
	t.Logf("the watcher took %d comments, the role's holder %d, the thread's author %d",
		len(addressed[scene.watcher]), len(addressed[scene.holder]), len(addressed[scene.author]))
}

// The publisher is killed while comments are written through the other process at 4 a second: the
// other process takes the publisher hold, logs its first batch, and a session takes a comment
// written after the kill, each within 10 s of it, and every session still takes each comment once
// and in order across the hand-over.
func TestAKilledPublisherIsReplacedWithin10Seconds(t *testing.T) {
	pair := startOutboxPair(t)
	scene := newOutboxScene(t, pair, 3, 1, 1)
	publisher := waitForPublisher(t, pair, takeOverWithin)
	survivor := pair.other(publisher)
	stop := scene.writeAtRate(t, 4, survivor)
	defer stop()
	waitUntil(t, outboxDeliveryWithin, "the publisher publishing the first comments", func() bool { return scene.deliveredCount() >= 4 })

	killed := publisher.signal(t, syscall.SIGKILL)
	publisher.WaitExit(t, "SIGKILL")
	scene.requireTakeOver(t, survivor, killed, "the publisher was killed")
	written := scene.writtenCount()
	waitUntil(t, outboxDeliveryWithin, "eight more comments written", func() bool { return scene.writtenCount() >= written+8 })
	stop()
	scene.waitDelivered(t)
	scene.requireOnceAndInOrder(t)
}

// restartedPublishesWithin bounds how long a restarted process takes, from the moment it is
// healthy, to publish the events the process killed before it left unpublished.
const restartedPublishesWithin = 10 * time.Second

// The only process is killed while its batch carrying comment 1 of an issue is in flight - its NATS
// proxy frozen, so the envelope has left the process and reached nothing - with comment 2 of the
// same issue written behind it. Restarted on the same database and NATS, the process publishes
// both, comment 1 first: on the issue's topic family the stream holds comment 1 at a lower sequence
// than comment 2, a session following the issue takes comment 1 before comment 2, and both are
// marked published within 10 s of the restarted process becoming healthy. What the killed process
// had in flight is dropped with its connection, as a dead host's sends that never reached the
// server are, so nothing of comment 1 reaches the stream but what the restarted process publishes.
func TestAPublisherKilledMidBatchAndRestartedPublishesTheIssuesCommentsInOrderWithin10Seconds(t *testing.T) {
	pair := newPair(t)
	proxy := testnats.StartProxy(t, pair.nats)
	tap := startNATSTap(t, proxy.URL())
	killed := startDispatchProcessWith(t, pair.databaseURL(t),
		dispatchOptions{NATSURL: tap.url, SigningKey: multiprocessSigningKey})
	var restarted *dispatchProcess
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		t.Logf("the killed process's output:\n%s", killed.Output.String())
		if restarted != nil {
			t.Logf("the restarted process's output:\n%s", restarted.Output.String())
		}
	})
	killed.waitHealthy(t)

	rig := newSessionRig(t, pair.nats)
	js, err := rig.conn.JetStream()
	if err != nil {
		t.Fatalf("JetStream context: %v", err)
	}
	head := streamHead(t, js)
	project := uniqueProjectKey(t, "KMB")
	killed.createProject(t, project, "Killed mid-batch "+project)
	key, _ := killed.createIssueIn(t, project, "Killed mid-batch", "Killed mid-batch.")
	family := "notifications.dispatch.issue." + key + "."
	watcher := newTestSession(t, rig, "ses_"+strings.ToLower(project)+"_watcher", family+">")
	// The process publishes, and has nothing left to publish, when its NATS freezes.
	waitForEveryEventPublished(t, pair.database, outboxDeliveryWithin)

	proxy.Freeze()
	first := rig.postComment(killed, key, "Comment 1.")
	if first.err != nil {
		t.Fatalf("write comment 1: %v", first.err)
	}
	firstEvent := commentEventID(t, pair.database, first.commentID)
	waitUntil(t, 3*time.Second, "the process sending comment 1's envelope", func() bool {
		return tap.hasSent(fmt.Sprintf(`"event_id":%q`, firstEvent))
	})
	second := rig.postComment(killed, key, "Comment 2.")
	if second.err != nil {
		t.Fatalf("write comment 2: %v", second.err)
	}
	secondEvent := commentEventID(t, pair.database, second.commentID)
	killedAt := killed.signal(t, syscall.SIGKILL)
	killed.WaitExit(t, "SIGKILL")
	proxy.Cut()
	proxy.Restore()

	restarted = pair.startProcess(t, nil)
	restarted.waitHealthy(t)
	healthy := time.Now()
	t.Logf("the restarted process was healthy %v after the kill", healthy.Sub(killedAt).Round(time.Millisecond))

	published := map[string]time.Time{}
	waitUntil(t, 2*outboxDeliveryWithin, "both comments marked published and taken by the session following the issue", func() bool {
		for _, event := range []string{firstEvent, secondEvent} {
			if at, ok := eventPublishedAt(t, pair.database, event); ok {
				published[event] = at
			}
		}
		taken := watcher.takenComments()
		_, hasFirst := taken[first.commentID]
		_, hasSecond := taken[second.commentID]
		return len(published) == 2 && hasFirst && hasSecond
	})

	sequences := map[string]uint64{}
	for _, message := range readStreamMessagesFrom(t, js, head) {
		if _, seen := sequences[message.item.EventID]; !seen && strings.HasPrefix(message.item.Topic, family) {
			sequences[message.item.EventID] = message.seq
		}
	}
	firstSeq, secondSeq := sequences[firstEvent], sequences[secondEvent]
	t.Logf("on the issue's topic family the stream holds comment 1 (%s) at sequence %d and comment 2 (%s) at %d",
		firstEvent, firstSeq, secondEvent, secondSeq)
	switch {
	case firstSeq == 0 || secondSeq == 0:
		t.Errorf("the stream does not hold both comments on the issue's topic family: comment 1 at sequence %d, comment 2 at %d", firstSeq, secondSeq)
	case secondSeq < firstSeq:
		t.Errorf("the stream holds comment 2 at sequence %d and comment 1 at %d: the restarted process published the issue's comments out of the order they were written",
			secondSeq, firstSeq)
	}

	taken := watcher.takenComments()
	firstTaken, secondTaken := taken[first.commentID], taken[second.commentID]
	if !secondTaken.at.After(firstTaken.at) {
		t.Errorf("the session following the issue took comment 2 (%s) %v after the restarted process was healthy and comment 1 (%s) %v after: want comment 1 first",
			secondEvent, secondTaken.at.Sub(healthy).Round(time.Millisecond), firstEvent, firstTaken.at.Sub(healthy).Round(time.Millisecond))
	}
	watcher.requireInOrder(t)

	for _, comment := range []struct {
		name, event string
	}{{"comment 1", firstEvent}, {"comment 2", secondEvent}} {
		after := published[comment.event].Sub(healthy)
		t.Logf("%s (%s) was marked published %v after the restarted process was healthy", comment.name, comment.event, after.Round(time.Millisecond))
		if after > restartedPublishesWithin {
			t.Errorf("%s (%s) was marked published %v after the restarted process was healthy, past the %v bound",
				comment.name, comment.event, after.Round(time.Millisecond), restartedPublishesWithin)
		}
	}
}

// commentEventID is the event id, dispatch-<row>, of comment id's comment.created event.
func commentEventID(t *testing.T, database *store.Store, commentID string) string {
	t.Helper()
	var row int64
	if err := database.Pool.QueryRow(context.Background(),
		`select id from events where type = 'comment.created' and payload ->> 'id' = $1`, commentID).Scan(&row); err != nil {
		t.Fatalf("read the event row of comment %s: %v", commentID, err)
	}
	return fmt.Sprintf("dispatch-%d", row)
}

// eventPublishedAt is when the event of event id dispatch-<row> was marked published, and false
// while it is not.
func eventPublishedAt(t *testing.T, database *store.Store, eventID string) (time.Time, bool) {
	t.Helper()
	row, err := strconv.ParseInt(strings.TrimPrefix(eventID, "dispatch-"), 10, 64)
	if err != nil {
		t.Fatalf("read the event row of event id %q: %v", eventID, err)
	}
	var published *time.Time
	if err := database.Pool.QueryRow(context.Background(),
		`select published_at from events where id = $1`, row).Scan(&published); err != nil {
		t.Fatalf("read event %s's published_at: %v", eventID, err)
	}
	if published == nil {
		return time.Time{}, false
	}
	return *published, true
}

// natsTap relays a process's NATS connections to the NATS URL it fronts and keeps every byte the
// process sent, so a row sees an envelope leave the process while a proxy behind the tap holds it
// back (testnats.Proxy.Freeze).
type natsTap struct {
	listener net.Listener
	target   string
	// url is the NATS URL of the tap's address: the target's, with the host and port the tap
	// listens on.
	url string

	mu   sync.Mutex
	sent []byte
}

// startNATSTap relays to the NATS URL target on a loopback port of its own until the test ends.
func startNATSTap(t *testing.T, target string) *natsTap {
	t.Helper()
	server, err := url.Parse(target)
	if err != nil || server.Host == "" {
		t.Fatalf("read the NATS URL %q the tap relays to: %v", target, err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the NATS tap: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	tapped := *server
	tapped.Host = listener.Addr().String()
	tap := &natsTap{listener: listener, target: server.Host, url: tapped.String()}
	go tap.serve()
	return tap
}

// serve relays each connection the tap accepts until the tap's listener closes.
func (tap *natsTap) serve() {
	for {
		client, err := tap.listener.Accept()
		if err != nil {
			return
		}
		go tap.relay(client)
	}
}

// relay copies client to the target, keeping what it reads, and the target back to client, until
// either side ends; then it closes both.
func (tap *natsTap) relay(client net.Conn) {
	defer client.Close()
	server, err := net.Dial("tcp", tap.target)
	if err != nil {
		return
	}
	defer server.Close()
	go func() {
		_, _ = io.Copy(client, server)
		_ = client.Close()
	}()
	_, _ = io.Copy(server, io.TeeReader(client, tap))
}

// Write keeps what the process sent.
func (tap *natsTap) Write(data []byte) (int, error) {
	tap.mu.Lock()
	defer tap.mu.Unlock()
	tap.sent = append(tap.sent, data...)
	return len(data), nil
}

// hasSent reports whether the process has sent text on any of its connections.
func (tap *natsTap) hasSent(text string) bool {
	tap.mu.Lock()
	defer tap.mu.Unlock()
	return bytes.Contains(tap.sent, []byte(text))
}

// The publisher loses NATS while comments are written through both processes at 4 a second, and
// the other process, whose NATS works, takes the publisher hold, logs its first batch and has a
// comment written since reach a session, each within 10 s. First its proxy cuts it off, which
// closes its connection; then, with the roles swapped, the new publisher's proxy freezes, which
// leaves its connection open and silent. Either way the publisher releases the hold for NATS lost,
// and every session takes each comment once and in order.
func TestAPublisherCutFromNATSHandsOverWithin10Seconds(t *testing.T) {
	pair := startOutboxPair(t)
	scene := newOutboxScene(t, pair, 3, 1, 1)
	t.Run("cut", func(t *testing.T) {
		publisher := waitForPublisher(t, pair, takeOverWithin)
		proxy := pair.proxy(publisher)
		defer proxy.Restore()
		scene.handOver(t, publisher, "its NATS connection was cut", proxy.Cut)
	})
	if t.Failed() {
		return
	}
	t.Run("silent cut", func(t *testing.T) {
		publisher := waitForPublisher(t, pair, takeOverWithin)
		standby := pair.other(publisher)
		waitUntil(t, 30*time.Second, "the standby reconnecting to NATS", func() bool {
			report, err := standby.readHealthReport()
			return err == nil && report.NATS != nil && *report.NATS
		})
		proxy := pair.proxy(publisher)
		defer proxy.Restore()
		scene.handOver(t, publisher, "its NATS connection went silent", proxy.Freeze)
	})
}

// handOver writes comments at 4 a second through both processes and, once they flow, runs cut on
// the publisher's NATS. It requires the other process to take over within takeOverWithin
// (requireTakeOver), the publisher to release the hold for NATS lost, and every session to take
// each comment once and in order.
func (s *outboxScene) handOver(t *testing.T, publisher *dispatchProcess, what string, cut func()) {
	t.Helper()
	before := s.deliveredCount()
	stop := s.writeAtRate(t, 4, s.pair.a, s.pair.b)
	defer stop()
	waitUntil(t, outboxDeliveryWithin, "the publisher publishing comments", func() bool { return s.deliveredCount() >= before+4 })

	at := time.Now()
	cut()
	s.requireTakeOver(t, s.pair.other(publisher), at, what)
	released := publisher.waitForOutboxLine(t, releasedHold, at, 2*takeOverWithin)
	if released.reason != "NATS lost" {
		t.Errorf("the publisher released the hold for %q after %s, want NATS lost: %s", released.reason, what, released.text)
	}
	written := s.writtenCount()
	waitUntil(t, outboxDeliveryWithin, "eight more comments written", func() bool { return s.writtenCount() >= written+8 })
	stop()
	s.waitDelivered(t)
	s.requireOnceAndInOrder(t)
}

// The publisher is stopped (SIGSTOP) for 10 s while comments are written through the other
// process, which takes the publisher hold and publishes them. Once they are all delivered, the
// stopped process is continued: its hold's deadline passed while it was stopped, so it logs that it
// lost the hold and sends nothing - no session receives anything after it resumes, it logs no batch
// started after it was stopped, and it neither takes the hold again nor releases a hold that is no
// longer its own.
func TestAFrozenPublisherSendsNothingWhenItResumes(t *testing.T) {
	pair := startOutboxPair(t)
	scene := newOutboxScene(t, pair, 3, 1, 1)
	publisher := waitForPublisher(t, pair, takeOverWithin)
	standby := pair.other(publisher)
	for index := range 4 {
		if err := scene.write(standby, scene.issues[index%len(scene.issues)], fmt.Sprintf("Before the stop %d.", index)); err != nil {
			t.Fatalf("write a comment: %v", err)
		}
	}
	// The publisher is idle when it is stopped: everything written so far has reached its sessions.
	scene.waitDelivered(t)

	stopped := publisher.signal(t, syscall.SIGSTOP)
	stop := scene.writeAtRate(t, 4, standby)
	defer stop()
	scene.requireTakeOver(t, standby, stopped, "the publisher was stopped")
	time.Sleep(time.Until(stopped.Add(frozenFor)))
	stop()
	scene.waitDelivered(t)

	resumed := publisher.signal(t, syscall.SIGCONT)
	lost := publisher.waitForOutboxLine(t, lostHold, resumed, takeOverWithin)
	t.Logf("the resumed process logged that it lost the hold %v after it resumed", lost.at.Sub(resumed).Round(time.Millisecond))
	time.Sleep(resumedQuiet)

	for session, frames := range scene.framesSince(resumed) {
		for _, frame := range frames {
			t.Errorf("session %s received %s's event %s (%s) %v after the stopped publisher resumed, with nothing left to send but what the resumed process sent",
				session.id, frame.owner, frame.event, frame.kind, frame.at.Sub(resumed).Round(time.Millisecond))
		}
	}
	for _, line := range publisher.outboxLines(t) {
		switch {
		case line.message == sentBatch && line.started.After(stopped):
			t.Errorf("the stopped publisher logged a batch started after it was stopped: %s", line.text)
		case (line.message == tookHold || line.message == releasedHold) && !line.at.Before(resumed.Truncate(time.Millisecond)):
			t.Errorf("the resumed process logged %q: %s", line.message, line.text)
		}
	}
	for process, want := range map[*dispatchProcess]bool{publisher: false, standby: true} {
		report, err := process.readHealthReport()
		if err != nil || report.publishing() != want {
			t.Errorf("process %s's /healthz reads %v (%v), want outbox_publisher %v", pair.name(process), report, err, want)
		}
	}
	scene.requireOnceAndInOrder(t)
}

// A role's holder loses the first forward of event N, and the outbox, with both processes running,
// keeps N unpublished and N+1 held back from the role's holder until a holder's receipt of N
// arrives: N is published only after that receipt, and N+1 reaches the holder after N. In the first
// case the holder's connection drops as the forward arrives and comes back; in the second the role
// moves to another session while the forward waits for its receipt, and N and N+1 reach the new
// holder, the old one taking neither.
func TestARoleHolderWhoseConnectionMovesGetsTheEventOnceAndInOrder(t *testing.T) {
	pair := startOutboxPair(t)
	rig := newSessionRig(t, pair.nats)
	project := uniqueProjectKey(t, "RHM")
	name := strings.ToLower(project)
	pair.a.createProject(t, project, "Role holder "+project)
	routedIssue := func(t *testing.T, role string) string {
		t.Helper()
		key, _ := pair.a.createIssueIn(t, project, "Routed to "+role, "Routed.")
		pair.a.call(t, http.MethodPatch, "/api/v1/issues/"+key, fmt.Sprintf(`{"route":"role:%s"}`, role), http.StatusOK)
		waitForEveryEventPublished(t, pair.database, outboxDeliveryWithin)
		return key
	}
	// withholdFirstComment has session withhold its receipt of the first comment's forward, and run
	// then; every other forward it answers. It returns that forward's frame once it has arrived.
	withholdFirstComment := func(session *testSession, then func()) func() sessionFrame {
		var mu sync.Mutex
		var withheld sessionFrame
		session.setOnForward(func(frame sessionFrame) bool {
			mu.Lock()
			defer mu.Unlock()
			if frame.kind != "comment.created" || withheld.event != "" {
				return true
			}
			withheld = frame
			then()
			return false
		})
		return func() sessionFrame {
			mu.Lock()
			defer mu.Unlock()
			return withheld
		}
	}
	writeTwo := func(t *testing.T, key string) (first, second string) {
		t.Helper()
		n := rig.postComment(pair.a, key, "Event N.")
		next := rig.postComment(pair.b, key, "Event N+1.")
		if n.err != nil || next.err != nil {
			t.Fatalf("write the two comments: %v, %v", n.err, next.err)
		}
		return n.commentID, next.commentID
	}
	takesBoth := func(session *testSession, first, second string) func() bool {
		return func() bool {
			taken := session.takenComments()
			_, hasFirst := taken[first]
			_, hasSecond := taken[second]
			return hasFirst && hasSecond
		}
	}

	t.Run("the holder's connection drops", func(t *testing.T) {
		role := "drop-" + name
		holder := newTestSession(t, rig, "ses_"+name+"_dropping", contracts.AgentSubject("ses_"+name+"_dropping"))
		rig.registerHolder(t, role, holder.id)
		key := routedIssue(t, role)
		reconnected := make(chan error, 1)
		withheld := withholdFirstComment(holder, func() { go func() { reconnected <- holder.reconnect() }() })
		first, second := writeTwo(t, key)
		waitUntil(t, outboxDeliveryWithin, "the holder taking both comments", takesBoth(holder, first, second))
		if err := receiveWithin(t, reconnected, "the holder's reconnect"); err != nil {
			t.Fatalf("reconnect the holder: %v", err)
		}
		requirePublishedAfterReceipt(t, pair, holder, withheld(), first, second)
	})

	t.Run("the role moves to another session", func(t *testing.T) {
		role := "move-" + name
		leaving := newTestSession(t, rig, "ses_"+name+"_leaving", contracts.AgentSubject("ses_"+name+"_leaving"))
		arriving := newTestSession(t, rig, "ses_"+name+"_arriving", contracts.AgentSubject("ses_"+name+"_arriving"))
		rig.registerHolder(t, role, leaving.id)
		rig.registerSession(t, arriving.id)
		key := routedIssue(t, role)
		moved := make(chan error, 1)
		withheld := withholdFirstComment(leaving, func() { moved <- rig.claimRole(role, arriving.id) })
		first, second := writeTwo(t, key)
		waitUntil(t, outboxDeliveryWithin, "the new holder taking both comments", takesBoth(arriving, first, second))
		if err := receiveWithin(t, moved, "the role's move"); err != nil {
			t.Fatalf("move the role: %v", err)
		}
		for id := range leaving.takenComments() {
			if id == first || id == second {
				t.Errorf("the session the role moved from took comment %s", id)
			}
		}
		requirePublishedAfterReceipt(t, pair, arriving, withheld(), first, second)
	})
}

// receiveWithin is what a row's hook sent on done, waiting up to 10 s for it, and fails the test
// naming what when nothing comes.
func receiveWithin(t *testing.T, done <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never happened", what)
		return nil
	}
}

// requirePublishedAfterReceipt requires the forward of the first comment withheld to have been the
// first comment's, the holder to have taken the first comment after that and the second after the
// first, the first comment's event to have been marked published only after the holder took it -
// after its receipt - and both processes of the pair to be running. The batch that took the
// receipts records them once every answer of the batch is in, which can land after the holder has
// taken both comments, so event N's record is waited for, up to outboxDeliveryWithin, before its
// time is judged.
func requirePublishedAfterReceipt(t *testing.T, pair *dispatchPair, holder *testSession, withheld sessionFrame, first, second string) {
	t.Helper()
	taken := holder.takenComments()
	n, next := taken[first], taken[second]
	if withheld.commentID != first {
		t.Fatalf("the withheld forward was of comment %q, want the first comment, %s", withheld.commentID, first)
	}
	if !n.at.After(withheld.at) {
		t.Errorf("the holder took event N at %s, not after its withheld forward at %s", n.at.Format(time.RFC3339Nano), withheld.at.Format(time.RFC3339Nano))
	}
	if !next.at.After(n.at) {
		t.Errorf("the holder took event N+1 (%s) at %s, not after event N (%s) at %s",
			next.event, next.at.Format(time.RFC3339Nano), n.event, n.at.Format(time.RFC3339Nano))
	}
	holder.requireInOrder(t)
	row := n.eventRow(t)
	var published *time.Time
	waitUntil(t, outboxDeliveryWithin, fmt.Sprintf("event N (%s) being marked published once its holder took it", n.event), func() bool {
		if err := pair.database.Pool.QueryRow(context.Background(),
			`select published_at from events where id = $1`, row).Scan(&published); err != nil {
			t.Fatalf("read event N's published_at: %v", err)
		}
		return published != nil
	})
	if published.Before(n.at) {
		t.Errorf("event N (%s) was marked published at %s, before its holder took it at %s and sent its receipt",
			n.event, published.Format(time.RFC3339Nano), n.at.Format(time.RFC3339Nano))
	} else {
		t.Logf("event N was forwarded at %s with no receipt, taken at %s, marked published at %s",
			withheld.at.Format(time.RFC3339Nano), n.at.Format(time.RFC3339Nano), published.Format(time.RFC3339Nano))
	}
	pair.requireRunning(t)
}

// ───── the delivery durables ────────────────────────────────────────────────

// githubFake is the loopback GitHub the test's processes send their App calls to, so no call
// leaves the machine. It answers each path the processes read in the shape GitHub's API gives it
// (githubFakeAnswer): an empty webhook delivery list, the App's one installation on acme/deploy
// granting the reads the delivery reconcile checks for, an installation token, that
// installation's one repository, an empty pull-request search and empty workflow run and job listings, and GitHub's 404 for any other
// path. It holds the first webhook delivery listing - the webhook redelivery sweep's own first
// request, made at boot - until release is closed, which is how a row makes one process hold that
// sweep's lease long enough for the other to find it held. It counts those listings too: the
// listing is the call the webhook-redelivery lease exists to make once however many processes
// sweep, so a row about that lease reads the count rather than the log alone.
type githubFake struct {
	server  *httptest.Server
	release chan struct{}
	once    sync.Once

	mu     sync.Mutex
	listed int
}

// startGitHubFake serves on loopback until the test ends.
func startGitHubFake(t *testing.T) *githubFake {
	t.Helper()
	fake := &githubFake{release: make(chan struct{})}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/hook/deliveries") {
			fake.countDeliveryListing()
			fake.holdFirstDeliveryListing(r.Context())
		}
		status, body := githubFakeAnswer(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(func() {
		fake.Release()
		fake.server.Close()
	})
	return fake
}

// githubFakeInstallation is GitHub's installation object for the App on acme/deploy (the deploy
// repository seedDeliverySettings names), with the Actions and Pull requests reads the delivery
// reconcile's permission check requires (githubapp.Client.DeliveryPermissions).
const githubFakeInstallation = `{"id":1,"app_slug":"multiprocess","account":{"login":"acme"},` +
	`"permissions":{"actions":"read","pull_requests":"read","contents":"read","metadata":"read"}}`

// githubFakeAnswer is GitHub's status and body for r, in the shape its API gives each path the
// processes read: the webhook redelivery sweep's listings, the installation lookups and token
// mint every installation-token call makes, the installation's repository list and the
// reconcile's GraphQL pull-request search over it, and its workflow run and job listings. Every listing is empty, so a pass reads its whole sequence and
// finds nothing to import.
func githubFakeAnswer(r *http.Request) (int, string) {
	path := r.URL.Path
	switch {
	case strings.HasPrefix(path, "/app/hook/deliveries"):
		return http.StatusOK, "[]"
	case path == "/app/installations":
		return http.StatusOK, "[" + githubFakeInstallation + "]"
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/app/installations/") && strings.HasSuffix(path, "/access_tokens"):
		return http.StatusCreated, fmt.Sprintf(`{"token":"ghs_multiprocess","expires_at":%q}`,
			time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	case strings.HasPrefix(path, "/repos/") && strings.HasSuffix(path, "/installation"):
		return http.StatusOK, githubFakeInstallation
	case path == "/installation/repositories":
		return http.StatusOK, `{"total_count":1,"repositories":[{"id":1,"name":"deploy","full_name":"acme/deploy","private":true,"owner":{"login":"acme"}}]}`
	case r.Method == http.MethodPost && path == "/graphql":
		return http.StatusOK, `{"data":{"search":{"issueCount":0,"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[]}}}`
	case strings.Contains(path, "/actions/workflows/") && strings.HasSuffix(path, "/runs"):
		return http.StatusOK, `{"total_count":0,"workflow_runs":[]}`
	case strings.Contains(path, "/actions/runs/") && strings.HasSuffix(path, "/jobs"):
		return http.StatusOK, `{"total_count":0,"jobs":[]}`
	}
	return http.StatusNotFound, `{"message":"Not Found","documentation_url":"https://docs.github.com/rest"}`
}

// holdFirstDeliveryListing blocks the first listing until Release, the caller gives up, or
// githubHoldCap passes, whichever comes first; every later listing is answered at once.
func (f *githubFake) holdFirstDeliveryListing(ctx context.Context) {
	select {
	case <-f.release:
		return
	default:
	}
	select {
	case <-f.release:
	case <-ctx.Done():
	case <-time.After(githubHoldCap):
	}
}

// countDeliveryListing records one webhook delivery listing, whichever process made it.
func (f *githubFake) countDeliveryListing() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listed++
}

// deliveryListings is how many webhook delivery listings the processes have made between them.
func (f *githubFake) deliveryListings() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listed
}

// githubHoldCap bounds the hold whatever the test does, well inside githubapp.RequestTimeout (20
// s), so a row that fails before it releases leaves no request timing out.
const githubHoldCap = 10 * time.Second

// Release lets every held listing answer.
func (f *githubFake) Release() { f.once.Do(func() { close(f.release) }) }

// URL is the origin DISPATCH_GITHUB_API_BASE names.
func (f *githubFake) URL() string { return f.server.URL }

// Each delivery durable admits one subscriber. With two processes reading one NATS, exactly one
// of them binds each durable and the other stands by - an ordinary state, logged at INFO and
// never as a failure - polling every two seconds, so the work resumes within seconds of the
// holder dying rather than waiting for a deploy.
//
// The delivery reconcile reads GitHub from whichever process takes its lease. Its first pass is
// jittered over its first five minutes, so without a knob it lands inside this row's window
// only now and then; DISPATCH_TEST_RECONCILE_AT_START runs it as each process starts, so every
// run of this row meets it, and neither process may log a delivery error for it.
func TestOneProcessBindsEachDeliveryDurableAndTheOtherStandsBy(t *testing.T) {
	pair := newPair(t)
	seedDeliverySettings(t, pair.database)
	fake := startGitHubFake(t)
	fake.Release()
	env := githubAppEnv(t, fake.URL())
	env["DISPATCH_TEST_RECONCILE_AT_START"] = "1"
	pair.start(t, env)

	conn := testnats.Connect(t, pair.nats)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("JetStream context: %v", err)
	}

	holders := map[string]*dispatchProcess{}
	for _, durable := range deliveryDurables {
		holder := waitForDurableHolder(t, pair, durable, 60*time.Second)
		holders[durable] = holder
		standby := pair.other(holder)
		standby.waitForLog(t, 30*time.Second,
			`msg="dispatch delivery: NATS consumer held by another process; standing by"`, "name="+durable)
		if errorLines := standby.logLines("level=ERROR", "dispatch delivery:"); len(errorLines) > 0 {
			t.Errorf("the process standing by for %s logged a delivery error; standing by is not a failure:\n%s",
				durable, strings.Join(errorLines, "\n"))
		}
		if bound := standby.logLines(`msg="dispatch delivery: NATS consumer bound"`, "name="+durable); len(bound) > 0 {
			t.Errorf("both processes bound %s, which admits one subscriber:\n%s", durable, strings.Join(bound, "\n"))
		}
		if info, err := js.ConsumerInfo(bus.Stream, durable); err != nil {
			t.Fatalf("read %s: %v", durable, err)
		} else if !info.PushBound {
			t.Errorf("NATS reports nothing bound to %s, though %s logged that it bound it", durable, pair.name(holder))
		}
	}

	// The reconcile's pass at start has read GitHub as a deployment's does: its permission check,
	// its searches and its run listings, answered in GitHub's own shapes. The lease logs its run
	// once the pass has returned, so every line it wrote is in the log by then.
	waitForLease(t, pair, leaseRan, "delivery-reconcile", 60*time.Second)
	for _, process := range []*dispatchProcess{pair.a, pair.b} {
		if errorLines := process.logLines("level=ERROR", "dispatch delivery:"); len(errorLines) > 0 {
			t.Errorf("%s logged a delivery error while the reconcile read GitHub:\n%s",
				pair.name(process), strings.Join(errorLines, "\n"))
		}
	}

	// The holder dies the way a task replaced mid-deploy or killed for memory does, with no
	// chance to unsubscribe, and the process that was standing by takes the durable over.
	dying := holders["delivery-events"]
	survivor := pair.other(dying)
	dying.kill(t)
	handedOver := time.Now()
	for _, durable := range deliveryDurables {
		if holders[durable] != dying {
			continue
		}
		survivor.waitForLog(t, 10*time.Second, `msg="dispatch delivery: NATS consumer bound"`, "name="+durable)
	}
	t.Logf("the standby bound the dead holder's durables %v after it died", time.Since(handedOver))
}

// waitForDurableHolder waits for exactly one of the pair to log that it bound durable, and
// answers which.
func waitForDurableHolder(t *testing.T, pair *dispatchPair, durable string, within time.Duration) *dispatchProcess {
	t.Helper()
	bound := `msg="dispatch delivery: NATS consumer bound"`
	deadline := time.Now().Add(within)
	for {
		switch {
		case pair.a.logged(bound, "name="+durable):
			return pair.a
		case pair.b.logged(bound, "name="+durable):
			return pair.b
		case time.Now().After(deadline):
			t.Fatalf("neither process bound %s within %v:\nA:\n%s\nB:\n%s",
				durable, within, pair.a.Output.String(), pair.b.Output.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ───── documents ───────────────────────────────────────────────────────────

// A spec tab is open on one process while the other writes the document. The write announces
// itself over Postgres (dispatch_doc_head, with dispatch_doc_changed beside it for older builds)
// and A catches the room it holds up from the stored rows: the editor sees the write in its open
// socket, nobody is disconnected, A closes no room, and the reader that comes next reads the
// current document.
func TestADocumentWriteOnOneProcessReachesTheOpenEditorOnTheOther(t *testing.T) {
	pair := startPair(t, nil)
	project := uniqueProjectKey(t, "DOC")
	pair.a.createProject(t, project, "Documents")
	_, artifactID := pair.a.createIssueIn(t, project, "Open here, written there", "before")

	editor := pair.a.openEditor(t, artifactID, "before\n")
	wrote := time.Now()
	pair.b.call(t, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits",
		`{"ops":[{"op":"replace","find":"before","with":"after"}]}`, http.StatusOK)
	if err := editor.WaitForText(markdown, "after\n", 10*time.Second); err != nil {
		t.Fatalf("the editor on A never saw B's write: %v; %s\nA:\n%s", err, pair.roomCloses(t, artifactID), pair.a.Output.String())
	}
	t.Logf("B's write reached the editor on A %v after it began", time.Since(wrote))
	pair.requireConnected(t, artifactID, "the editor on A", editor, "after B's write")
	pair.requireNoRoomCloses(t, artifactID)
	if text := pair.a.documentText(t, artifactID); text != "after\n" {
		t.Errorf("A serves %q after B's write; want the current document, %q", text, "after\n")
	}
}

// storedHead is the version of the document's latest durable update.
func storedHead(t *testing.T, pair *dispatchPair, artifactID string) int64 {
	t.Helper()
	var head int64
	if err := pair.database.Pool.QueryRow(context.Background(),
		`select coalesce(max(version), 0) from doc_updates where artifact_id = $1`, artifactID).Scan(&head); err != nil {
		t.Fatalf("read the stored head of %s: %v", artifactID, err)
	}
	return head
}

// ───── the leased maintenance loops ────────────────────────────────────────

// The two lines duty.Exclusive writes, which every leased loop reports its pass through.
const (
	leaseRan     = `msg="dispatch duty: leased pass ran"`
	leaseSkipped = `msg="dispatch duty: leased pass skipped"`
)

// countLease is how many of message's lines the process wrote for lease.
func countLease(p *dispatchProcess, message, lease string) int {
	return len(p.logLines(message, "lease="+lease))
}

// waitForLease waits for either process to write message for lease.
func waitForLease(t *testing.T, pair *dispatchPair, message, lease string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for countLease(pair.a, message, lease)+countLease(pair.b, message, lease) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("neither process wrote %s for the %s lease within %v:\nA:\n%s\nB:\n%s",
				message, lease, within, pair.a.Output.String(), pair.b.Output.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitForLeaseInEach waits for both processes to have written message for lease.
func waitForLeaseInEach(t *testing.T, pair *dispatchPair, message, lease string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for countLease(pair.a, message, lease) == 0 || countLease(pair.b, message, lease) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("both processes did not each write %s for the %s lease within %v:\nA:\n%s\nB:\n%s",
				message, lease, within, pair.a.Output.String(), pair.b.Output.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// requireLeasedOnce is what a leased loop's window must show: the pass ran once across the two
// processes, which is what one process alone would have done, and the other process found the
// lease held and skipped rather than running the same work beside it.
func requireLeasedOnce(t *testing.T, pair *dispatchPair, lease string) {
	t.Helper()
	ranA, ranB := countLease(pair.a, leaseRan, lease), countLease(pair.b, leaseRan, lease)
	skippedA, skippedB := countLease(pair.a, leaseSkipped, lease), countLease(pair.b, leaseSkipped, lease)
	t.Logf("%s: A ran %d skipped %d, B ran %d skipped %d", lease, ranA, skippedA, ranB, skippedB)
	if ranA+ranB != 1 {
		t.Errorf("the %s pass ran %d times across the two processes; one process alone would have run it once",
			lease, ranA+ranB)
	}
	if ranA == 1 && skippedB == 0 {
		t.Errorf("A ran the %s pass and B never reported finding the lease held", lease)
	}
	if ranB == 1 && skippedA == 0 {
		t.Errorf("B ran the %s pass and A never reported finding the lease held", lease)
	}
}

// lockTables holds every table named in mode (`exclusive`, or `access exclusive`) in a transaction
// of the test's own, as a writer that is taking its time would, and rolls it back when the test
// ends if the test has not. A maintenance pass whose first statement writes one of them - or, in
// access exclusive mode, reads one - waits there, inside its lease, which is the moment the lease
// is about: both processes' loops firing while one is still in its pass.
func lockTables(t *testing.T, database *store.Store, mode string, tables ...string) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	gate, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("open the transaction holding %v: %v", tables, err)
	}
	t.Cleanup(func() { _ = gate.Rollback(context.Background()) })
	for _, table := range tables {
		if _, err := gate.Exec(ctx, "lock table "+table+" in "+mode+" mode"); err != nil {
			t.Fatalf("hold %s: %v", table, err)
		}
	}
	return gate
}

// Every leased maintenance loop takes a named lease for the length of its pass, so the second
// process's attempt finds it held and skips instead of doing the same work beside it. Each loop
// observed here is held inside its own first statement while the other process's attempt fires -
// by a table lock this test takes, or by the GitHub listing the fake holds - which is the only way
// two processes' passes are made to overlap on purpose rather than by luck.
//
// Four of the twelve leases are observable here inside a window a test can wait out, and they
// keep different rules once the hold lets go. webhook-redelivery, block-index and
// search-term-stats are timer loops whose first pass is immediate: each window is served once
// across the two processes, by whichever took the lease. reference-excerpts is a woken loop
// (docs.RunReferenceExcerptReconciliation) that runs a pass at start and again on the
// pglisten.Hub wake after its first LISTEN: the process that found the lease held retries (every
// two seconds, its MinGap) until it runs a pass of its own, because the other's pass may have
// read before whatever woke it, so after the release each process runs at least one pass and no
// count is held to one. The other eight are not observable, and this row says so rather than
// pretending: document-compaction's interval is 24 h; issue-ask-repair's first leased pass is one
// IssueAskRepairInterval (10 min) in, the boot's repair taking no lease; suggestion-outcomes is
// held only by the keyed writes' maintenance sweep now (api.RunMaintenanceSweep, the suggestion
// outcome sweep taking no lease), which no longer touches write_suggestions, the table this row
// once held it by, and whose first leased pass is one MaintenanceSweepInterval (1 min) in on a
// phase of each process's own (api.TestTheMaintenanceLoopSweepsOncePerPeriodAcrossTwoRunners
// holds what its lease is for); architecture-import's and
// delivery-reconcile's first pass is jittered uniformly over SyncInterval/ReconcileInterval
// (5 min) and each process draws its own, so whether their passes ever overlap is a coin toss no
// test should turn into a flake; edit-notices is a woken loop whose passes here, with nothing
// due, take milliseconds, so whether the two processes' boot passes meet is luck too
// (docs.TestEditNoticeSweepRetriesAHeldLease holds its retry, and
// docs.TestEditNoticeSweepOfTwoServicesSendsEachNoticeOnce what its lease is for);
// settlement-resumption's period is SettlementResumeInterval (1 min) and each pass is over in
// milliseconds, so this window holds several of its periods, each served once; and
// server-processes' prune is one delete on server_processes, the table every process's heartbeat
// writes every five seconds, so a lock that held the prune would hold the registry itself.
func TestALeasedLoopsPassesNeverOverlapAcrossBothProcesses(t *testing.T) {
	pair := newPair(t)
	fake := startGitHubFake(t)
	// The term-statistics refresh writes nothing for a database with no project, so one exists
	// before either process starts.
	if _, err := pair.database.Pool.Exec(context.Background(), `insert into projects (key, name) values ('LEASE', 'Lease')`); err != nil {
		t.Fatalf("create the project the term-statistics refresh rebuilds: %v", err)
	}
	// refs is where the reference-excerpt reconciliation's first statement writes
	// (refs.MarkStaleExcerpts), and search_term_totals where the term-statistics refresh's first
	// write for a project goes (its first statement only reads projects, which an exclusive lock
	// does not hold).
	gate := lockTables(t, pair.database, "exclusive", "refs", "search_term_totals")
	// The block-index reconciliation's first statements read document_block_versions and
	// block_grammar_owed, which only access exclusive mode holds back. They are held in a gate of
	// their own, released as soon as the other process has found the lease held: that pass's
	// interval is 30 s, so released later with the gate above, the other process's own next tick
	// could land beside the release and make the next period's pass before this row counts the
	// first.
	blockGate := lockTables(t, pair.database, "access exclusive", "document_block_versions", "block_grammar_owed")
	pair.start(t, githubAppEnv(t, fake.URL()))

	if waiting := storetest.WaitForLockWaiters(t, gate, 1, 60*time.Second); waiting < 1 {
		t.Fatalf("no reference-excerpt pass reached the refs lock, so neither process took the lease:\nA:\n%s\nB:\n%s",
			pair.a.Output.String(), pair.b.Output.String())
	}
	waitForLease(t, pair, leaseSkipped, "reference-excerpts", 60*time.Second)
	if ran := countLease(pair.a, leaseRan, "reference-excerpts") + countLease(pair.b, leaseRan, "reference-excerpts"); ran != 0 {
		t.Errorf("a reference-excerpt pass reported running %d times while the one holding the lease is still inside the refs lock", ran)
	}
	waitForLease(t, pair, leaseSkipped, "search-term-stats", 60*time.Second)
	if ran := countLease(pair.a, leaseRan, "search-term-stats") + countLease(pair.b, leaseRan, "search-term-stats"); ran != 0 {
		t.Errorf("a term-statistics pass reported running %d times while the one holding the lease is still inside the search_term_totals lock", ran)
	}

	if waiting := storetest.WaitForLockWaiters(t, blockGate, 1, 60*time.Second); waiting < 1 {
		t.Fatalf("no block-index pass reached the document_block_versions lock, so neither process took the lease:\nA:\n%s\nB:\n%s",
			pair.a.Output.String(), pair.b.Output.String())
	}
	waitForLease(t, pair, leaseSkipped, "block-index", 60*time.Second)
	if ran := countLease(pair.a, leaseRan, "block-index") + countLease(pair.b, leaseRan, "block-index"); ran != 0 {
		t.Errorf("a block-index pass reported running %d times while the one holding the lease is still inside the document_block_versions lock", ran)
	}
	if err := blockGate.Rollback(context.Background()); err != nil {
		t.Fatalf("release the tables the block-index pass is waiting on: %v", err)
	}
	waitForLease(t, pair, leaseRan, "block-index", 60*time.Second)
	requireLeasedOnce(t, pair, "block-index")

	// The webhook redelivery sweep's first pass is held in its first GitHub call instead: it
	// lists the App webhook's failed deliveries before it writes anything.
	waitForLease(t, pair, leaseSkipped, "webhook-redelivery", 60*time.Second)
	fake.Release()
	waitForLease(t, pair, leaseRan, "webhook-redelivery", 60*time.Second)
	requireLeasedOnce(t, pair, "webhook-redelivery")

	if err := gate.Rollback(context.Background()); err != nil {
		t.Fatalf("release the table the passes are waiting on: %v", err)
	}
	// The process that skipped retries a little over two seconds after each skip; a minute is
	// room for the held pass to finish and for that retry to run a pass of its own.
	waitForLeaseInEach(t, pair, leaseRan, "reference-excerpts", 60*time.Second)
	waitForLease(t, pair, leaseRan, "search-term-stats", 60*time.Second)
	requireLeasedOnce(t, pair, "search-term-stats")
	for _, unobserved := range []struct{ lease, why string }{
		{"document-compaction", "its interval is 24 h, so no pass is due inside any window a test can wait out"},
		{"issue-ask-repair", "its first leased pass is one IssueAskRepairInterval (10 min) after each process started"},
		{"suggestion-outcomes", "only the keyed writes' maintenance sweep holds it, which no longer touches write_suggestions; api.TestTheMaintenanceLoopSweepsOncePerPeriodAcrossTwoRunners holds its lease"},
		{"architecture-import", "its first pass is jittered over SyncInterval (5 min) and each process draws its own"},
		{"delivery-reconcile", "its first pass is jittered over ReconcileInterval (5 min) and each process draws its own"},
		{"edit-notices", "a woken loop whose passes take milliseconds here, so its boot passes meet by luck"},
		{"settlement-resumption", "its period is SettlementResumeInterval (1 min), so the window holds several periods, each served once"},
		{"server-processes", "its pass is one delete on server_processes, which every process's heartbeat writes, so holding it would hold the heartbeats too"},
	} {
		if ran := countLease(pair.a, leaseRan, unobserved.lease) + countLease(pair.b, leaseRan, unobserved.lease); ran > 0 {
			t.Logf("%s ran %d times in the window, which this row does not hold to a count: %s",
				unobserved.lease, ran, unobserved.why)
			continue
		}
		t.Logf("%s was not observed: %s", unobserved.lease, unobserved.why)
	}
}

// The suggestion-outcome sweep (api.RunSuggestionOutcomeSweep) is a woken loop that takes no
// lease: each of its pages runs in a transaction holding the suggestion_outcome_cursor row `for
// update`, which is what keeps two processes from judging one page twice. Each process runs a
// pass at start, and the two start together, so their boot passes meet. Both reach that row - a
// lease would have left one of them outside it, which is the change this row is about - and this
// test holds the row with a lock of its own to see both arrive. Once the lock lets go, one pass
// judges the page and advances the cursor and the other, which was waiting on the row, reads the
// cursor that pass advanced: no wake is lost to another process's pass, and neither process
// judges what the other judged.
func TestTheSuggestionOutcomeSweepSerialisesOnTheCursorAcrossBothProcesses(t *testing.T) {
	pair := newPair(t)
	gate := lockTables(t, pair.database, "exclusive", "suggestion_outcome_cursor")
	pair.start(t, nil)

	if waiting := storetest.WaitForLockWaiters(t, gate, 2, 60*time.Second); waiting < 2 {
		t.Fatalf("%d of the two processes' suggestion-outcome passes reached the cursor lock, want both; the loop takes no lease, so neither waits outside the row:\nA:\n%s\nB:\n%s",
			waiting, pair.a.Output.String(), pair.b.Output.String())
	}
	if err := gate.Rollback(context.Background()); err != nil {
		t.Fatalf("release the cursor the passes are waiting on: %v", err)
	}
	// Both passes come out of the row in turn: the first judges every pending suggestion and sets
	// the cursor, the second reads it set and finds its page empty.
	waitUntil(t, 60*time.Second, "the cursor the first pass out of the lock advanced", func() bool {
		var cursor *int64
		if err := pair.database.Pool.QueryRow(context.Background(), `select event_id from suggestion_outcome_cursor`).Scan(&cursor); err != nil {
			t.Fatalf("read the cursor: %v", err)
		}
		return cursor != nil
	})
	for name, process := range map[string]*dispatchProcess{"A": pair.a, "B": pair.b} {
		if failed := strings.Count(process.Output.String(), "suggestion outcome sweep failed"); failed != 0 {
			t.Errorf("process %s logged %d failed suggestion-outcome passes:\n%s", name, failed, process.Output.String())
		}
	}
}

// A rolling update starts the second process minutes after the first, and at a steady two
// replicas each process keeps a timer of its own: two ticks of one leased loop almost never
// overlap, because a pass is seconds and its interval is minutes. This row is that shape - the
// second process is started only after the first has finished a pass and given the lease back -
// and it is where "runs once overall" has to hold, not only while a pass is in flight.
//
// The webhook redelivery sweep is the loop driven here because its cost is countable from
// outside: every pass lists the App webhook's failed deliveries from GitHub before it writes
// anything, and its lease exists for exactly that reason ("N processes sweeping is N times those
// list calls for one result", redeliver/sweep.go). So the GitHub fake's count of those listings
// is what one sweep window cost the installation's rate limit, whichever process paid it.
//
// A period is a bucket of the tick's due time (duty.Exclusive, migration 0115), so both
// processes are started inside one bucket on purpose: a second process whose first tick lands
// past a bucket boundary is entitled to sweep, and this row is about the one that lands inside
// the bucket its peer has already served. waitOutABucketBoundary is what keeps which of the two
// this run gets from being luck.
func TestALeasedLoopRunsOnceAcrossAStaggeredStart(t *testing.T) {
	pair := newPair(t)
	waitOutABucketBoundary(t, redeliver.Interval)
	fake := startGitHubFake(t)
	// Nothing is held here: this row is about two passes that never meet, so the first one must
	// finish and give the lease back before the second process exists.
	fake.Release()
	env := githubAppEnv(t, fake.URL())

	pair.a = startDispatchProcessWith(t, pair.databaseURL(t), dispatchOptions{
		NATSURL: pair.nats, SigningKey: multiprocessSigningKey, Env: env,
	})
	pair.a.waitHealthy(t)
	pair.a.waitForLog(t, 60*time.Second, leaseRan, "lease=webhook-redelivery")
	listedByA := fake.deliveryListings()
	if listedByA == 0 {
		t.Fatalf("the first process's sweep listed no webhook deliveries, so this row would measure nothing:\n%s",
			pair.a.Output.String())
	}

	pair.b = startDispatchProcessWith(t, pair.databaseURL(t), dispatchOptions{
		NATSURL: pair.nats, SigningKey: multiprocessSigningKey, Env: env,
	})
	pair.b.waitHealthy(t)
	deadline := time.Now().Add(60 * time.Second)
	for countLease(pair.b, leaseRan, "webhook-redelivery")+countLease(pair.b, leaseSkipped, "webhook-redelivery") == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the second process reported no webhook-redelivery pass within a minute:\n%s",
				pair.b.Output.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	requireLeasedOnce(t, pair, "webhook-redelivery")
	if listed := fake.deliveryListings(); listed != listedByA {
		t.Errorf("the two processes listed the webhook's deliveries %d times for one sweep window; the first alone listed them %d times, and the lease is what keeps N processes from being N times those list calls",
			listed, listedByA)
	}
}

// waitOutABucketBoundary returns with enough of period's current bucket left for a row to start
// both processes and read their first ticks inside it. A period is floor(epoch / period) of a
// tick's due time, on the clock of the process that ticks, so a row that straddles a boundary
// watches a second process legitimately sweep rather than skip; this waits the boundary out
// instead, on the clock both processes share with this test (one host), and waits at most the
// margin it needs.
func waitOutABucketBoundary(t *testing.T, period time.Duration) {
	t.Helper()
	// What one run of the row needs inside the bucket: two process starts and their first ticks.
	const margin = 45 * time.Second
	left := period - time.Duration(time.Now().UnixNano()%period.Nanoseconds())
	if left >= margin {
		return
	}
	t.Logf("waiting %v for the next bucket of the %v period, so both processes tick inside one", left, period)
	time.Sleep(left + time.Second)
}

// ───── a sign-in across the processes ───────────────────────────────────────

// cookieSignIn is the environment that has a process sign people in through issuer's code flow
// under the group rule, on a plain-HTTP cookie, as the multiprocess sign-in rows run it.
func cookieSignIn(issuer *oidctest.Issuer) map[string]string {
	return map[string]string{
		"DISPATCH_IDENTITY":             "cookie",
		"DISPATCH_SIGNIN_ISSUER":        issuer.URL(),
		"DISPATCH_SIGNIN_CLIENT_ID":     cookieClientID,
		"DISPATCH_SIGNIN_CLIENT_SECRET": cookieClientSecret,
		"DISPATCH_SIGNIN_GROUP":         cookieGroup,
		"DISPATCH_INSECURE_COOKIE":      "1",
	}
}

// multiprocessServerURL is the one public origin a pair behind a load balancer is reached at,
// DISPATCH_SERVER_URL on both: each process builds the sign-in's redirect_uri from it, so the
// process that exchanges the code names the redirect_uri the process that started the sign-in
// sent the issuer, which the issuer requires. Without it each process would build its own from
// the request's Host, and the issuer would refuse the exchange on the other process. Nothing here
// resolves the name: the test carries every redirect itself.
const multiprocessServerURL = "http://dispatch.multiprocess.test"

// A person starts signing in on A and the issuer's redirect back lands on B, which is what a load
// balancer does when it sends the browser's two requests to different tasks. The sign-in in
// flight rides the browser's state cookie, signed under the pair's key, so B completes it: the
// callback answers 302 to the page the sign-in started from, with a session cookie, and both
// processes name the person on that cookie. Before INFRA-555 the state was held by the process
// that started the sign-in, and B answered 400 "invalid or expired state".
func TestASignInStartedOnOneProcessCompletesOnTheOther(t *testing.T) {
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), cookieClientID, cookieClientSecret)
	env := cookieSignIn(issuer)
	env["DISPATCH_SERVER_URL"] = multiprocessServerURL
	pair := startPair(t, env)
	issuer.SignInAs(map[string]any{
		"sub":              "alice-subject",
		"cognito:username": "ExampleIdP_" + cookiePerson,
		"cognito:groups":   []string{cookieGroup},
		"identities":       []map[string]any{{"providerName": "ExampleIdP"}},
	})

	const next = "/issues/XPS-1?tab=spec"
	started := browserGet(t, pair.a.url("/auth/start?next="+url.QueryEscape(next)), nil)
	_ = started.Body.Close()
	if started.StatusCode != http.StatusFound {
		t.Fatalf("/auth/start on A answered %d, want a redirect to the issuer:\n%s", started.StatusCode, pair.a.Output.String())
	}
	var state *http.Cookie
	for _, cookie := range started.Cookies() {
		if cookie.Name == "dispatch_oauth_state" && cookie.Value != "" {
			state = cookie
		}
	}
	if state == nil {
		t.Fatalf("/auth/start on A set no sign-in state cookie: %v", started.Cookies())
	}
	authorize, err := url.Parse(started.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse the authorization URL A answered: %v", err)
	}
	if got, want := authorize.Query().Get("redirect_uri"), multiprocessServerURL+"/auth/callback"; got != want {
		t.Fatalf("A sent the issuer redirect_uri %q, want %q, the pair's public origin", got, want)
	}
	code, returned := issuer.Authorize(t, authorize.String())

	callback := browserGet(t, pair.b.url("/auth/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(returned)), state)
	body, _ := io.ReadAll(callback.Body)
	_ = callback.Body.Close()
	if callback.StatusCode != http.StatusFound {
		t.Fatalf("/auth/callback on B answered %d, want 302: %s\nB:\n%s", callback.StatusCode, body, pair.b.Output.String())
	}
	if location := callback.Header.Get("Location"); location != next {
		t.Errorf("/auth/callback on B redirected to %q, want the page the sign-in started from, %q", location, next)
	}
	var session *http.Cookie
	for _, cookie := range callback.Cookies() {
		if cookie.Name == "dsession" && cookie.Value != "" {
			session = cookie
		}
	}
	if session == nil {
		t.Fatalf("/auth/callback on B set no session cookie: %v", callback.Cookies())
	}
	for _, process := range []*dispatchProcess{pair.a, pair.b} {
		if answer, status := whoami(t, process, session); status != http.StatusOK || !strings.Contains(answer, cookiePerson) {
			t.Errorf("%s answered whoami %d %s, want 200 naming %s", pair.name(process), status, answer, cookiePerson)
		}
	}
}

// ───── one signing key across the processes ────────────────────────────────

// A person signs in on one process and their next request lands on the other, which is what a
// load balancer in front of two tasks does on every page load. The session itself is in Postgres,
// which both read; what each process needs of its own is DISPATCH_SIGNING_KEY. A process carrying
// a key no live peer shares refuses to start (TestAProcessWhoseKeySetALivePeerDoesNotShareRefusesToStart),
// so no third process with a key of its own serves beside these two.
func TestASessionMintedOnOneProcessAuthenticatesOnTheOtherUnderOneSigningKey(t *testing.T) {
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), cookieClientID, cookieClientSecret)
	pair := newPair(t)
	pair.start(t, cookieSignIn(issuer))

	session := signInThrough(t, pair.a, issuer)
	for _, target := range []struct {
		name    string
		process *dispatchProcess
		status  int
	}{
		{"the process it was minted on", pair.a, http.StatusOK},
		{"the other process of the pair", pair.b, http.StatusOK},
	} {
		answer, status := whoami(t, target.process, session)
		if status != target.status {
			t.Errorf("%s answered whoami %d, want %d: %s", target.name, status, target.status, answer)
			continue
		}
		if status == http.StatusOK && !strings.Contains(answer, cookiePerson) {
			t.Errorf("%s answered whoami %s, want the person the session names (%s)", target.name, answer, cookiePerson)
		}
	}
}

// The registry of live server processes refuses a process whose signing key set a live peer does
// not share both ways (processes.Register): with a pair live under one key, a process carrying a
// key of its own - a task rolled with a changed secret - and one with no DISPATCH_SIGNING_KEY,
// which signs with a key it minted in its own data dir, each exit 1 with the refusal and never
// answer /healthz, since each would sign people out of the pair or be signed out by it. Each
// exits once it has waited a live window and a heartbeat for the pair to stop heartbeating, which
// the pair never does. A process given the pair's key first and another second, the first step of
// a rotation, starts beside them.
func TestAProcessWhoseKeySetALivePeerDoesNotShareRefusesToStart(t *testing.T) {
	pair := startPair(t, nil)
	for _, refused := range []struct{ name, key string }{
		{"a process carrying a key of its own", "a-key-of-its-own"},
		{"a process with no key, which minted one in its data dir", ""},
	} {
		process := startDispatchProcessWith(t, pair.databaseURL(t), dispatchOptions{NATSURL: pair.nats, SigningKey: refused.key})
		requireRefusedToStart(t, process, refused.name)
	}

	rotating := startDispatchProcessWith(t, pair.databaseURL(t), dispatchOptions{
		NATSURL: pair.nats, SigningKey: multiprocessSigningKey + " a-key-of-its-own",
	})
	rotating.waitHealthy(t)
	var registered int
	if err := pair.database.Pool.QueryRow(context.Background(), `select count(*) from server_processes`).Scan(&registered); err != nil {
		t.Fatalf("count the registered server processes: %v", err)
	}
	if registered != 3 {
		t.Errorf("%d server processes registered, want the pair and the process given the pair's key first", registered)
	}
}

// A process whose heartbeat stops for longer than the registry's live window drops out of Live
// while it still runs: a pod frozen by its node, or one cut from the database for that long. A
// process started meanwhile with a key set the first does not share is not refused, since no live
// row disagrees with it. Once the first process resumes, the two must not both serve: each refuses
// the cookies the other mints, which signs people out of whichever one their next request reaches,
// the very thing the boot refusal is there to prevent. Whichever of the two stops - exits, or stops
// answering /healthz 200 so the Service takes it out - the pair must not stay serving side by side.
func TestAProcessThatResumesBesideAPeerWhoseKeySetItDoesNotShareDoesNotServeBesideIt(t *testing.T) {
	pair := newPair(t)
	frozen := startDispatchProcessWith(t, pair.databaseURL(t), dispatchOptions{NATSURL: pair.nats, SigningKey: multiprocessSigningKey, OwnProcessGroup: true})
	frozen.waitHealthy(t)
	if err := frozen.Cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatalf("freeze the first process: %v", err)
	}
	resumed := false
	t.Cleanup(func() {
		if !resumed {
			_ = frozen.Cmd.Process.Signal(syscall.SIGCONT)
		}
	})
	time.Sleep(processes.LiveWindow + 2*time.Second)

	other := startDispatchProcessWith(t, pair.databaseURL(t), dispatchOptions{NATSURL: pair.nats, SigningKey: "a-key-of-its-own"})
	other.waitHealthy(t)
	if err := frozen.Cmd.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatalf("resume the first process: %v", err)
	}
	resumed = true

	serving := func(p *dispatchProcess) bool {
		select {
		case <-p.Exited:
			return false
		default:
		}
		response, err := (&http.Client{Timeout: time.Second}).Get(p.url("/healthz"))
		if err != nil {
			return false
		}
		_ = response.Body.Close()
		return response.StatusCode == http.StatusOK
	}
	deadline := time.Now().Add(3*processes.HeartbeatInterval + 5*time.Second)
	for time.Now().Before(deadline) {
		if !serving(frozen) || !serving(other) {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("a process that resumed beside a live peer whose key set it does not share still serves beside it %s after it resumed, and so does the peer: each refuses the other's cookies\nresumed process:\n%s\npeer:\n%s",
		3*processes.HeartbeatInterval+5*time.Second, frozen.Output.String(), other.Output.String())
}

// requireRefusedToStart waits up to a minute for the process to exit, asking its /healthz all the
// while, and requires that it exited 1 with the registry's refusal, never having answered /healthz
// nor logged that it serves. A refused process waits processes.LiveWindow and one
// processes.HeartbeatInterval (20 s) for its refusers to lapse before it exits, well inside the
// minute.
func requireRefusedToStart(t *testing.T, p *dispatchProcess, name string) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(time.Minute)
	answered := 0
	for exited := false; !exited; {
		select {
		case <-p.Exited:
			exited = true
			continue
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was still running a minute after it started:\n%s", name, p.Output.String())
		}
		if response, err := client.Get(p.url("/healthz")); err == nil {
			_ = response.Body.Close()
			answered++
		}
		time.Sleep(100 * time.Millisecond)
	}
	output := p.Output.String()
	if code := p.Cmd.ProcessState.ExitCode(); code != 1 {
		t.Errorf("%s exited %d, want 1:\n%s", name, code, output)
	}
	if answered > 0 {
		t.Errorf("%s answered /healthz %d times before it exited", name, answered)
	}
	if !p.logged(`msg="dispatch: register this server process"`, "signing key set does not match a live server process", "DISPATCH_SIGNING_KEY") {
		t.Errorf("%s did not log the registry's refusal:\n%s", name, output)
	}
	if p.logged(`msg="dispatch: listening"`) {
		t.Errorf("%s logged that it serves:\n%s", name, output)
	}
}

// The two keys TestAKeyRotationKeepsPeopleSignedInAcrossTwoProcesses rotates between: K0, the key
// the pair ran under, and K1, the key it moves to.
const (
	rotationK0 = "multiprocess-rotation-k0"
	rotationK1 = "multiprocess-rotation-k1"
)

// A rotation halfway rolled: A carries the new key first ("K1 K0") while B still signs with the
// old one ("K0 K1"), each holding both, which the registry admits. Nobody is signed out on either
// process. A person signed in on B holds a cookie signed and a refresh token sealed under K0, and
// A answers that cookie. Once the confirmation lapses, A's request refreshes it, opening the K0
// seal and sealing the token again under K1; once it lapses again, B's request refreshes it,
// which opens that K1 seal. A cookie minted on A, under K1, is answered on B. Before INFRA-555 a
// DISPATCH_SIGNING_KEY of two keys was one key, so each process refused the other's cookie.
func TestAKeyRotationKeepsPeopleSignedInAcrossTwoProcesses(t *testing.T) {
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), cookieClientID, cookieClientSecret)
	pair := newPair(t)
	pair.a = startDispatchProcessWith(t, pair.databaseURL(t), dispatchOptions{
		NATSURL: pair.nats, SigningKey: rotationK1 + " " + rotationK0, Env: cookieSignIn(issuer),
	})
	pair.b = startDispatchProcessWith(t, pair.databaseURL(t), dispatchOptions{
		NATSURL: pair.nats, SigningKey: rotationK0 + " " + rotationK1, Env: cookieSignIn(issuer),
	})
	pair.a.waitHealthy(t)
	pair.b.waitHealthy(t)

	signedInOnB := signInThrough(t, pair.b, issuer)
	requireWhoami(t, pair, pair.a, signedInOnB, "the cookie B minted under K0")
	requireSealedUnder(t, pair.database, rotationK0, "the refresh token B's sign-in stored")

	lapseConfirmation(t, pair.database)
	refreshes := issuer.Refreshes()
	requireWhoami(t, pair, pair.a, signedInOnB, "the cookie B minted, once the confirmation lapsed")
	if got := issuer.Refreshes(); got != refreshes+1 {
		t.Fatalf("A answered a lapsed confirmation with %d refreshes at the issuer, want 1", got-refreshes)
	}
	requireSealedUnder(t, pair.database, rotationK1, "the refresh token after A's refresh")

	lapseConfirmation(t, pair.database)
	refreshes = issuer.Refreshes()
	requireWhoami(t, pair, pair.b, signedInOnB, "its own cookie, once the confirmation A renewed lapsed")
	if got := issuer.Refreshes(); got != refreshes+1 {
		t.Fatalf("B answered a lapsed confirmation with %d refreshes at the issuer, want 1: it did not refresh with the token A sealed", got-refreshes)
	}
	requireSealedUnder(t, pair.database, rotationK0, "the refresh token after B's refresh")

	signedInOnA := signInThrough(t, pair.a, issuer)
	requireWhoami(t, pair, pair.b, signedInOnA, "the cookie A minted under K1")
}

// requireWhoami requires that p answers /auth/whoami 200 naming cookiePerson on session.
func requireWhoami(t *testing.T, pair *dispatchPair, p *dispatchProcess, session *http.Cookie, what string) {
	t.Helper()
	if answer, status := whoami(t, p, session); status != http.StatusOK || !strings.Contains(answer, cookiePerson) {
		t.Fatalf("%s answered whoami on %s with %d %s, want 200 naming %s:\n%s",
			pair.name(p), what, status, answer, cookiePerson, p.Output.String())
	}
}

// lapseConfirmation dates cookiePerson's last confirmation two hours back, past the hour a
// confirmation stands, so the next request either process answers for them refreshes it.
func lapseConfirmation(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(),
		`update people set confirmed_at = now() - interval '2 hours' where email = $1`, cookiePerson); err != nil {
		t.Fatalf("date the confirmation two hours back: %v", err)
	}
}

// requireSealedUnder requires that the refresh token people holds for cookiePerson opens under
// signingKey alone. It opens the value as the v1 seal format is pinned to stay (an AES-256-GCM key
// derived from the signing key with HKDF-SHA256, a random nonce leading the ciphertext, the email
// as additional data), so it tells which key of the two sealed it, which a process holding both
// cannot show.
func requireSealedUnder(t *testing.T, database *store.Store, signingKey, what string) {
	t.Helper()
	var sealed string
	if err := database.Pool.QueryRow(context.Background(),
		`select refresh_token from people where email = $1`, cookiePerson).Scan(&sealed); err != nil {
		t.Fatalf("read %s: %v", what, err)
	}
	encoded, ok := strings.CutPrefix(sealed, "v1:")
	if !ok {
		t.Fatalf("%s is not in the v1 sealed format", what)
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("%s does not decode: %v", what, err)
	}
	key, err := hkdf.Key(sha256.New, []byte(signingKey), nil, "dispatch people.refresh_token v1", 32)
	if err != nil {
		t.Fatalf("derive the refresh-token key: %v", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("refresh-token cipher: %v", err)
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		t.Fatalf("refresh-token AEAD: %v", err)
	}
	if _, err := aead.Open(nil, nil, ciphertext, []byte(cookiePerson)); err != nil {
		t.Fatalf("%s does not open under %s alone", what, signingKey)
	}
}

// ───── the component's three-apply key rotation ─────────────────────────────

// The two keys the staged-rotation rows walk: generation 0's, the key the deployment already
// runs under, and generation 1's, the key a rotation adds. They are a pair of their own so that
// TestAKeyRotationKeepsPeopleSignedInAcrossTwoProcesses keeps rotationK0 and rotationK1 to
// itself, and they stand for the keys envoy-infra:gcp:Dispatch generates, one per generation of
// its signingKeyGenerations input.
const (
	stagedK0 = "multiprocess-staged-rotation-k0"
	stagedK1 = "multiprocess-staged-rotation-k1"
)

// The four DISPATCH_SIGNING_KEY settings the component renders along the rotation it is built
// for, [0] → [0, 1] → [1, 0] → [1]: one generation renders its key alone, several render the
// generations' keys in the list's order with exactly one space between them. Each is written as
// that join rather than as one literal, so what a reader checks against the component is the
// setting's shape and the generations' order rather than this file's spelling of two keys.
const (
	stagedGeneration0   = stagedK0
	stagedGenerations01 = stagedK0 + " " + stagedK1
	stagedGenerations10 = stagedK1 + " " + stagedK0
	stagedGeneration1   = stagedK1
)

// The whole rotation signingKeyGenerations is built for, three applies against two real
// processes: [0], the deployment before it starts, then [0, 1], which adds generation 1's key
// second, then [1, 0], which promotes it, then [1], which drops generation 0's. Each apply is a
// rolling update, one process replaced at a time with a live peer beside it, which is the pairing
// processes.keySetsShared judges at every boot: no step of the rotation is refused, and nobody
// holding a cookie is signed out at any of them. Every cookie is read on both processes and never
// only on the one that minted it, because a fleet halfway through a roll answers whichever
// request the load balancer sends to either task.
//
// The last apply is the one step that does sign someone out, and by design: a cookie signed under
// generation 0's key is refused once that key is gone, which is what dropping a generation means.
// Only the cookies signed under generation 1 survive it, which is why the component's README
// makes "no sooner than 30 days after the promote" - a session cookie's whole life - the
// precondition of that apply: run it then, and every cookie signed under generation 0 has already
// expired on its own, so the drop refuses nobody who is still signed in.
func TestAStagedKeyRotationAcrossThreeAppliesKeepsPeopleSignedIn(t *testing.T) {
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), cookieClientID, cookieClientSecret)
	pair := newPair(t)
	// Every process of the walk runs the same unstamped build, and only its DISPATCH_SIGNING_KEY
	// differs, so the binary is built once rather than once per start.
	binary := cmdtest.Build(t, "envoy-dispatch")
	start := func(setting string) *dispatchProcess {
		t.Helper()
		process := startDispatchProcessWith(t, pair.databaseURL(t), dispatchOptions{
			NATSURL: pair.nats, SigningKey: setting, Env: cookieSignIn(issuer), Binary: binary,
		})
		process.waitHealthy(t)
		return process
	}
	// apply is what one `pulumi up` of a changed signingKeyGenerations does to the app
	// Deployment: a rolling update that replaces one pod at a time, so the process coming up
	// always has a live peer still carrying the setting of the apply before it. That pairing,
	// not the setting on its own, is what the registry admits or refuses at a boot.
	apply := func(setting string) {
		t.Helper()
		pair.a.Terminate(t)
		pair.a.WaitExit(t, "SIGTERM to A")
		pair.a = start(setting)
		pair.b.Terminate(t)
		pair.b.WaitExit(t, "SIGTERM to B")
		pair.b = start(setting)
	}
	// readOnBoth requires both processes to name the person on the cookie, which is what "nobody
	// is signed out" means for a fleet: the browser's next request reaches either task.
	readOnBoth := func(session *http.Cookie, what string) {
		t.Helper()
		requireWhoami(t, pair, pair.a, session, what)
		requireWhoami(t, pair, pair.b, session, what)
	}

	// [0]: the deployment as it runs before the rotation, both processes on generation 0's key
	// alone. The person signed in here is the one every later apply must not sign out.
	pair.a, pair.b = start(stagedGeneration0), start(stagedGeneration0)
	underK0 := signInThrough(t, pair.b, issuer)
	readOnBoth(underK0, "the cookie B minted under generation 0")
	requireSealedUnder(t, pair.database, stagedK0, "the refresh token B's sign-in stored")

	// Apply 1, [0] → [0, 1]: generation 1's key is added second, so generation 0's still signs
	// every new cookie and seals every new refresh token while the roll brings both processes to
	// hold both keys. A sign-in during this apply is as good as the one before it, and the person
	// signed in at [0] carries on, which is the whole point of adding a key before promoting it.
	apply(stagedGenerations01)
	readOnBoth(underK0, "the cookie minted at [0], once both processes held both keys")
	alsoUnderK0 := signInThrough(t, pair.a, issuer)
	readOnBoth(alsoUnderK0, "the cookie A minted at [0, 1], which generation 0 still signs")
	requireSealedUnder(t, pair.database, stagedK0, "the refresh token after the add")

	// Apply 2, [0, 1] → [1, 0]: generation 1's key moves first, so it signs every cookie minted
	// from here on while generation 0's still verifies every cookie minted before it.
	apply(stagedGenerations10)
	readOnBoth(underK0, "the cookie minted at [0], after the promote")
	readOnBoth(alsoUnderK0, "the cookie minted at [0, 1], after the promote")

	// The seal follows the promote at the person's next hourly confirmation rather than at the
	// roll: the request that finds the confirmation lapsed opens the token under generation 0's
	// key and seals it again under generation 1's, so the drop two applies later leaves it
	// readable. Nothing in the roll itself rewrites a seal, which is why this is worth showing.
	lapseConfirmation(t, pair.database)
	refreshes := issuer.Refreshes()
	requireWhoami(t, pair, pair.a, underK0, "the cookie minted at [0], once the confirmation lapsed")
	if got := issuer.Refreshes(); got != refreshes+1 {
		t.Fatalf("A answered a lapsed confirmation with %d refreshes at the issuer, want 1", got-refreshes)
	}
	requireSealedUnder(t, pair.database, stagedK1, "the refresh token after the refresh that followed the promote")

	underK1 := signInThrough(t, pair.b, issuer)
	readOnBoth(underK1, "the cookie B minted at [1, 0], which generation 1 signs")

	// Apply 3, [1, 0] → [1]: generation 0's key is dropped. Everything signed under generation 1
	// - the cookies minted since the promote, and the seal the confirmation above moved - carries
	// on across the roll, and the fleet signs people in as it did before the rotation began.
	apply(stagedGeneration1)
	readOnBoth(underK1, "the cookie minted at [1, 0], after the drop")
	afterTheDrop := signInThrough(t, pair.a, issuer)
	readOnBoth(afterTheDrop, "the cookie A minted at [1], the rotation's last setting")
	requireSealedUnder(t, pair.database, stagedK1, "the refresh token after the drop")

	// The cookies signed under generation 0's key are what the drop signs out: no process holds
	// the key that verifies them any more, so each answers 401 rather than the person's name.
	// This is the loss the README's 30 days prevent - waited out, no such cookie is left to
	// refuse - and never something the component's inputs can guarantee, so the row asserts the
	// refusal rather than pretending the drop is free.
	for _, signedOut := range []struct {
		what    string
		session *http.Cookie
	}{
		{"the cookie minted at [0]", underK0},
		{"the cookie minted at [0, 1]", alsoUnderK0},
	} {
		for _, process := range []*dispatchProcess{pair.a, pair.b} {
			answer, status := whoami(t, process, signedOut.session)
			if status != http.StatusUnauthorized {
				t.Errorf("%s answered whoami on %s with %d %s, want 401: generation 0's key, which signed it, is gone\n%s",
					pair.name(process), signedOut.what, status, answer, process.Output.String())
			}
		}
	}
}

// ───── the fleet's build ────────────────────────────────────────────────────

// The two builds TestTwoProcessesAnnounceTheirBuildOnce rolls between, stamped into its binaries
// as the image build stamps a commit.
const (
	fleetBuildC1 = "c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1"
	fleetBuildC2 = "c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2"
)

// fleetQuiet is how long a step that must announce nothing is watched: two passes of the
// announcer (startup.Interval, five seconds) and a second more, so every live process has passed
// at least once since the step and a pass that would announce has had its chance to.
const fleetQuiet = 2*5*time.Second + time.Second

// server.started goes out once each time every live process comes to serve one build, never per
// process start: two processes stamped C1 put one envelope on the stream; B restarted on C1 puts
// none; B replaced by a C2 binary while A still runs C1 is a roll halfway through and puts none;
// A replaced by C2 completes it and puts one for C2. Each process's /healthz fleet_commit follows
// the announcements, whichever build that process itself runs.
func TestTwoProcessesAnnounceTheirBuildOnce(t *testing.T) {
	pair := newPair(t)
	c1 := cmdtest.BuildStamped(t, "envoy-dispatch-c1", fleetBuildC1)
	c2 := cmdtest.BuildStamped(t, "envoy-dispatch-c2", fleetBuildC2)
	start := func(binary string) *dispatchProcess {
		t.Helper()
		process := startDispatchProcessWith(t, pair.databaseURL(t), dispatchOptions{
			NATSURL: pair.nats, SigningKey: multiprocessSigningKey, Binary: binary,
		})
		process.waitHealthy(t)
		return process
	}
	replace := func(process *dispatchProcess, name, binary string) *dispatchProcess {
		t.Helper()
		process.Terminate(t)
		process.WaitExit(t, "SIGTERM to "+name)
		return start(binary)
	}
	conn := testnats.Connect(t, pair.nats)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("JetStream context: %v", err)
	}
	head := streamHead(t, js)
	announced := func() []contracts.Envelope {
		var starts []contracts.Envelope
		for _, item := range readStreamFrom(t, js, head) {
			if item.Topic == startup.Subject {
				starts = append(starts, item)
			}
		}
		return starts
	}

	a, b := start(c1), start(c1)
	waitForAnnouncements(t, announced, fleetBuildC1)
	waitForFleetCommit(t, fleetBuildC1, a, b)

	b = replace(b, "B", c1)
	time.Sleep(fleetQuiet)
	requireAnnouncements(t, announced(), fleetBuildC1)

	b = replace(b, "B", c2)
	time.Sleep(fleetQuiet)
	requireAnnouncements(t, announced(), fleetBuildC1)
	waitForFleetCommit(t, fleetBuildC1, a, b)

	a = replace(a, "A", c2)
	waitForAnnouncements(t, announced, fleetBuildC1, fleetBuildC2)
	waitForFleetCommit(t, fleetBuildC2, a, b)
	time.Sleep(fleetQuiet)
	requireAnnouncements(t, announced(), fleetBuildC1, fleetBuildC2)
}

// waitForAnnouncements waits up to thirty seconds for the stream to hold as many server.started
// envelopes as commits names, then requires them to be those (requireAnnouncements).
func waitForAnnouncements(t *testing.T, announced func() []contracts.Envelope, commits ...string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for len(announced()) < len(commits) && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	requireAnnouncements(t, announced(), commits...)
}

// requireAnnouncements requires got to be one server.started envelope per commit of commits, in
// that order, each from Dispatch, naming its commit in its payload and in its event id.
func requireAnnouncements(t *testing.T, got []contracts.Envelope, commits ...string) {
	t.Helper()
	if len(got) != len(commits) {
		t.Fatalf("the stream holds %d server.started envelopes, want %d naming %v: %+v", len(got), len(commits), commits, got)
	}
	for i, item := range got {
		var payload startup.Payload
		if err := json.Unmarshal([]byte(item.Payload), &payload); err != nil {
			t.Fatalf("decode server.started payload %q: %v", item.Payload, err)
		}
		if item.Source != startup.Source || payload.Commit != commits[i] ||
			!strings.HasPrefix(item.EventID, "dispatch-server-started-"+commits[i]+"-") {
			t.Fatalf("server.started envelope %d = %+v, want one from dispatch announcing %s", i, item, commits[i])
		}
	}
}

// waitForFleetCommit waits up to fleetQuiet for every one of processes to report want as its
// /healthz fleet_commit: each refreshes it on its announcer's pass.
func waitForFleetCommit(t *testing.T, want string, processes ...*dispatchProcess) {
	t.Helper()
	deadline := time.Now().Add(fleetQuiet)
	for _, process := range processes {
		for {
			response, err := http.Get(process.url("/healthz"))
			if err != nil {
				t.Fatalf("GET /healthz: %v", err)
			}
			var health struct {
				FleetCommit *string `json:"fleet_commit"`
			}
			err = json.NewDecoder(response.Body).Decode(&health)
			_ = response.Body.Close()
			if err != nil {
				t.Fatalf("decode /healthz: %v", err)
			}
			if health.FleetCommit != nil && *health.FleetCommit == want {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("/healthz fleet_commit = %v, want %s", health.FleetCommit, want)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
}

// ───── one document, readers on two processes ─────────────────────────────
//
// The rows below are pull request #2414's: two people, or a person and an agent, on one
// document whose readers are on different processes. Each drives real editors (docstest.Peer
// speaks the dashboard's Hocuspocus framing) and the processes' own API, hooks and logs, and
// arranges each race it is about with a test hook (DISPATCH_TEST_HOOKS) - a held persistence, a
// parked live write - or a lock of its own, never with a sleep that hopes.

// liveWithin is how soon a change on one process must reach a reader on the other: the spec's
// "at once", which the plan pins at one second.
const liveWithin = time.Second

// multiprocessAgentSession is the session an agent's writes name (actor.kind session) when they
// come through the agent token (dispatchTestAgentToken).
const multiprocessAgentSession = "multiprocess-agent"

// asAgent is the header an agent's request carries: the agent token and no person.
func asAgent() http.Header {
	return http.Header{"Authorization": {"Bearer " + dispatchTestAgentToken}}
}

func editPath(artifactID string) string {
	return "/api/v1/artifacts/" + artifactID + "/edits"
}

// editBody is the body of an edit replacing find with with: an agent's when agent is set (a bearer
// names its session in actor), and conditional on the document's token when token is not "".
func editBody(t *testing.T, find, with, token string, agent bool) string {
	t.Helper()
	body := struct {
		Ops          []model.EditOp          `json:"ops"`
		Precondition *model.EditPrecondition `json:"precondition,omitempty"`
		Actor        *model.Actor            `json:"actor,omitempty"`
	}{Ops: []model.EditOp{{Op: "replace", Find: find, With: with}}}
	if token != "" {
		body.Precondition = &model.EditPrecondition{Document: token}
	}
	if agent {
		body.Actor = &model.Actor{Kind: "session", ID: multiprocessAgentSession}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode the edit: %v", err)
	}
	return string(encoded)
}

// textAndToken is the document as the process reads it now and the token a conditional edit
// against that reading passes (GET /api/v1/artifacts/{id}/text).
func (p *dispatchProcess) textAndToken(t *testing.T, artifactID string) (string, string) {
	t.Helper()
	answer := p.call(t, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/text", "", http.StatusOK)
	var document struct {
		Markdown string `json:"markdown"`
		Token    string `json:"token"`
	}
	if err := json.Unmarshal(answer, &document); err != nil || document.Token == "" {
		t.Fatalf("decode the document's text and token: %v: %s", err, answer)
	}
	return document.Markdown, document.Token
}

// holdsRoom reports whether the process holds a room of the document now (the rooms test hook).
func (p *dispatchProcess) holdsRoom(t *testing.T, artifactID string) bool {
	t.Helper()
	return slices.Contains(p.residentRooms(t), artifactID)
}

// roomCloses names what the room-counters hook counted for the document on either process so far
// - the rooms the service closed and the room failures - for a failure to show beside what it saw.
func (p *dispatchPair) roomCloses(t *testing.T, artifactID string) string {
	t.Helper()
	countersA, countersB := p.a.roomCounters(t, artifactID), p.b.roomCounters(t, artifactID)
	return fmt.Sprintf("the service closed rooms of the document %d times on A and %d on B; rooms of it failed %d times on A and %d on B",
		countersA.RoomCloses, countersB.RoomCloses, countersA.RoomFailures, countersB.RoomFailures)
}

// requireNoRoomCloses fails the test when either process's service closed a room of the document
// (the room-counters hook's room_closes): a process that keeps its rooms live closes none for a
// write, a cursor or a departure, on its own process or the other. The count rises once the close
// has returned, so a close still under way when this reads is missed.
func (p *dispatchPair) requireNoRoomCloses(t *testing.T, artifactID string) {
	t.Helper()
	countersA, countersB := p.a.roomCounters(t, artifactID), p.b.roomCounters(t, artifactID)
	if countersA.RoomCloses+countersB.RoomCloses > 0 {
		t.Errorf("the service closed a room of the document %d times on A and %d on B, want none on either: a process closed a room it should have kept live (rooms of the document failed %d times on A and %d on B)",
			countersA.RoomCloses, countersB.RoomCloses, countersA.RoomFailures, countersB.RoomFailures)
	}
}

// ───── editors that reconnect as a browser does ─────────────────────────────

func peerEnded(editor *docstest.Peer) bool {
	select {
	case <-editor.Ended:
		return true
	default:
		return false
	}
}

// requireConnected fails the test when the editor's connection has ended, naming who and when.
func (p *dispatchPair) requireConnected(t *testing.T, artifactID, who string, editor *docstest.Peer, when string) {
	t.Helper()
	if peerEnded(editor) {
		t.Fatalf("%s was disconnected %s (close code %d): the process closed the room it held rather than keeping it live; %s",
			who, when, editor.CloseCode(), p.roomCloses(t, artifactID))
	}
}

// lenientT hands a dial's refusal back to dialEditor instead of ending the test: a browser's
// provider dials again after a refused reconnect (ygo refuses a dial between a closing room's
// hang-up and its removal), and so do the rows that reconnect. docstest.Dial reports a refusal
// through Fatalf, which here unwinds to dialEditor's recover.
type lenientT struct{ *testing.T }

type dialRefused struct{ err error }

func (t lenientT) Fatalf(format string, args ...any) {
	panic(dialRefused{fmt.Errorf(format, args...)})
}

// dialEditor connects login's tab to the document's socket at url, answering a refusal as an
// error.
func dialEditor(t *testing.T, url, login, artifactID string) (editor *docstest.Peer, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			refused, ok := recovered.(dialRefused)
			if !ok {
				panic(recovered)
			}
			err = refused.err
		}
	}()
	return docstest.Dial(lenientT{t}, url, asUser(login), artifactID, crdt.New()), nil
}

// awaitDocument asks the room for its document until the editor's copy renders one.
func awaitDocument(editor *docstest.Peer, within time.Duration) error {
	deadline := time.NewTimer(within)
	defer deadline.Stop()
	for markdown(editor) == "" {
		if err := editor.AskForDocument(); err != nil {
			return fmt.Errorf("ask the room for the document: %w", err)
		}
		select {
		case <-editor.Answers:
		case <-editor.Ended:
			return fmt.Errorf("the connection closed (code %d) before the room sent the document", editor.CloseCode())
		case <-deadline.C:
			return fmt.Errorf("the room sent no document within %v", within)
		}
	}
	return nil
}

// reconnectEditor connects login's tab to the document on this process again, as the dashboard's
// provider does once its socket closed: it dials again after a refusal, up to twenty times a
// tenth of a second apart (a browser backs off too), and returns once the room has sent the tab
// a document. It answers an error rather than failing the test, so a row counting disconnects
// can report what it counted.
func (p *dispatchProcess) reconnectEditor(t *testing.T, login, artifactID string) (*docstest.Peer, error) {
	t.Helper()
	var last error
	for range 20 {
		editor, err := dialEditor(t, p.documentURL(artifactID), login, artifactID)
		if err == nil {
			if err = awaitDocument(editor, 10*time.Second); err == nil {
				return editor, nil
			}
			editor.Close()
		}
		last = err
		time.Sleep(100 * time.Millisecond)
	}
	return nil, fmt.Errorf("reconnect %s's tab: %w", login, last)
}

// openStoredEditor is openEditorAs that also waits, up to ten seconds, for the update the tab's
// sync appends to be stored - a browser's provider answers the room's sync step 1 with its own
// state, and the room appends that update even when it changes nothing, since ygo runs the room's
// update observer for every transaction it commits - so the next thing the row does is the only
// thing that moves the document.
func (p *dispatchPair) openStoredEditor(t *testing.T, process *dispatchProcess, login, artifactID, want string) *docstest.Peer {
	t.Helper()
	before := storedHead(t, p, artifactID)
	editor := process.openEditorAs(t, login, artifactID, want)
	p.waitStoredPast(t, artifactID, before, 10*time.Second, login+"'s tab's sync update on "+p.name(process))
	return editor
}

// waitStoredPast waits up to within for the document's stored head to pass head, and fails the
// test naming what was to be stored when it does not.
func (p *dispatchPair) waitStoredPast(t *testing.T, artifactID string, head int64, within time.Duration, what string) {
	t.Helper()
	if err := p.storedPast(t, artifactID, head, within, what); err != nil {
		t.Fatal(err)
	}
}

// storedPast is waitStoredPast for a caller with something of its own to report before the test
// fails - the typists' row reports the room counters and each disconnect (keystroke) - so it
// answers an error rather than failing the test where the head does not move.
func (p *dispatchPair) storedPast(t *testing.T, artifactID string, head int64, within time.Duration, what string) error {
	t.Helper()
	deadline := time.Now().Add(within)
	for storedHead(t, p, artifactID) <= head {
		if time.Now().After(deadline) {
			return fmt.Errorf("%s was not stored within %v: the document still stands at update %d", what, within, head)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return nil
}

// storedText renders the document the store holds: its updates folded in order, which is what
// every reload of it reads, whatever any room holds.
func storedText(t *testing.T, pair *dispatchPair, artifactID string) string {
	t.Helper()
	rows, err := pair.database.Pool.Query(context.Background(),
		`select update from doc_updates where artifact_id = $1 order by version`, artifactID)
	if err != nil {
		t.Fatalf("read the stored history of %s: %v", artifactID, err)
	}
	defer rows.Close()
	document := crdt.New()
	for rows.Next() {
		var update []byte
		if err := rows.Scan(&update); err != nil {
			t.Fatalf("scan a stored update of %s: %v", artifactID, err)
		}
		if err := crdt.ApplyUpdateV1(document, update, nil); err != nil {
			t.Fatalf("apply a stored update of %s: %v", artifactID, err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the stored history of %s: %v", artifactID, err)
	}
	return renderDocument(document)
}

// waitStoredHolds waits up to within for the stored document to hold want, and fails the test
// naming what was to be stored when it does not. It is waitStoredPast for a row that typed
// something: a browser's connect hands the room the state of its own copy, which the room appends
// as it does any other update of that browser, so a head read taken right after a connect can be
// passed by that append alone - this one passes only on the text.
func (p *dispatchPair) waitStoredHolds(t *testing.T, artifactID, want string, within time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		stored := storedText(t, p, artifactID)
		if strings.Contains(stored, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was not stored within %v: the stored document holds no %q, it reads:\n%s", what, within, want, stored)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// typeAtEnd types text at the end of the editor's first paragraph in one keystroke's transaction,
// on the paragraph's own Yjs text as ProseMirror's binding does, and sends the room the update.
func typeAtEnd(editor *docstest.Peer, text string) error {
	fragment := editor.Doc.GetXmlFragment("prosemirror")
	missing := false
	if _, err := editor.Send(func(txn *crdt.Transaction) {
		paragraph := crafted.FirstText(fragment)
		if paragraph == nil {
			missing = true
			return
		}
		paragraph.Insert(txn, paragraph.Len(), text, nil)
	}); err != nil {
		return err
	}
	if missing {
		return errors.New("the editor's copy holds no paragraph text to type into")
	}
	return nil
}

// rewriteDocument changes the editor's document as one transaction - change edits the tree it
// reads, and pmdoc writes the difference back - and sends the room the update.
func rewriteDocument(editor *docstest.Peer, change func(*pmdoc.Node) error) error {
	fragment := editor.Doc.GetXmlFragment("prosemirror")
	var changeErr error
	if _, err := editor.Send(func(txn *crdt.Transaction) {
		tree, err := pmdoc.ReadInTransaction(txn, fragment)
		if err != nil {
			changeErr = err
			return
		}
		if changeErr = change(tree); changeErr != nil {
			return
		}
		changeErr = pmdoc.Update(txn, fragment, tree)
	}); err != nil {
		return err
	}
	return changeErr
}

// deleteParagraph deletes, in the editor, the top-level block whose text holds word, as a person
// selecting the paragraph and pressing delete does.
func deleteParagraph(t *testing.T, editor *docstest.Peer, word string) {
	t.Helper()
	if err := rewriteDocument(editor, func(tree *pmdoc.Node) error {
		for index, block := range tree.Children {
			if strings.Contains(pmdoc.TextContent(block), word) {
				tree.Children = slices.Delete(tree.Children, index, index+1)
				return nil
			}
		}
		return fmt.Errorf("no paragraph holds %q", word)
	}); err != nil {
		t.Fatalf("delete the paragraph holding %q: %v", word, err)
	}
}

// appendMarkdown appends the blocks markdown parses to at the end of the editor's document, as a
// person typing them (an ask block's slash command included) does.
func appendMarkdown(t *testing.T, editor *docstest.Peer, markdown string) {
	t.Helper()
	parsed, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatalf("parse %q: %v", markdown, err)
	}
	if err := rewriteDocument(editor, func(tree *pmdoc.Node) error {
		tree.Children = append(tree.Children, parsed.Children...)
		return nil
	}); err != nil {
		t.Fatalf("append %q: %v", markdown, err)
	}
}

// placeMark marks quote in the editor's document with a comment mark carrying id, as the
// dashboard does before it posts the comment that mark anchors.
func placeMark(t *testing.T, editor *docstest.Peer, login, quote, id string) {
	t.Helper()
	fragment := editor.Doc.GetXmlFragment("prosemirror")
	var markErr error
	if _, err := editor.Send(func(txn *crdt.Transaction) {
		tree, err := pmdoc.ReadInTransaction(txn, fragment)
		if err != nil {
			markErr = err
			return
		}
		quoted, err := pmdoc.FindQuote(tree, quote, nil, nil)
		if err != nil {
			markErr = err
			return
		}
		markErr = pmdoc.MarkRange(txn, fragment, quoted, pmdoc.Mark{
			Type:  string(docs.MarkComment),
			Attrs: pmdoc.Attrs{"id": id, "by": "user:" + login},
		})
	}); err != nil || markErr != nil {
		t.Fatalf("mark %q in the editor: %v %v", quote, err, markErr)
	}
}

// roomRendering asks the room for its whole document over the editor's socket and renders the
// answer on its own: what the room holds, not what the editor's copy holds. The room answers a
// sync step 1 after every message the editor sent before it, so this is also a barrier.
func roomRendering(editor *docstest.Peer, within time.Duration) (string, error) {
	for drained := false; !drained; {
		select {
		case <-editor.Answers:
		default:
			drained = true
		}
	}
	if err := editor.AskForDocument(); err != nil {
		return "", fmt.Errorf("ask the room for its document: %w", err)
	}
	select {
	case state := <-editor.Answers:
		doc := crdt.New()
		if err := crdt.ApplyUpdateV1(doc, state, nil); err != nil {
			return "", fmt.Errorf("apply the room's document: %w", err)
		}
		return renderDocument(doc), nil
	case <-editor.Ended:
		return "", fmt.Errorf("the connection closed (code %d) before the room sent its document", editor.CloseCode())
	case <-time.After(within):
		return "", fmt.Errorf("the room sent no document within %v", within)
	}
}

// typist is one person typing into a document in a row that counts what the session cost them:
// they reconnect whenever their socket closes, as the dashboard's provider does, and each
// disconnect's close code is noted.
type typist struct {
	who         string
	login       string
	process     *dispatchProcess
	editor      *docstest.Peer
	text        string
	disconnects []int
}

// disconnected reports, as an error message's clause, how often the typist was disconnected and
// with which close codes, or "" when never.
func (typing *typist) disconnected() string {
	if len(typing.disconnects) == 0 {
		return ""
	}
	codes := map[int]int{}
	for _, code := range typing.disconnects {
		codes[code]++
	}
	return fmt.Sprintf("%s was disconnected %d times (close code: times %v)", typing.who, len(typing.disconnects), codes)
}

// keystroke types the typist's text once and waits until the store has moved past where it stood
// before the keystroke, or until the typist's socket closed, which is a browser's lot when the
// room it typed into was closed. A closed socket is noted and reconnected before the next
// keystroke. It answers an error when the typist cannot be reconnected or the keystroke goes
// nowhere at all.
func (p *dispatchPair) keystroke(t *testing.T, artifactID string, typing *typist) error {
	t.Helper()
	for attempt := 1; ; attempt++ {
		if peerEnded(typing.editor) {
			typing.disconnects = append(typing.disconnects, typing.editor.CloseCode())
			reconnected := storedHead(t, p, artifactID)
			editor, err := typing.process.reconnectEditor(t, typing.login, artifactID)
			if err != nil {
				return fmt.Errorf("%s: %w", typing.who, err)
			}
			typing.editor = editor
			// The room appends the state the tab sent on reconnecting, content or no content, so
			// that append is waited for here: left in flight, it would pass the wait below on its
			// own and the keystroke would be taken for stored.
			if err := p.storedPast(t, artifactID, reconnected, 10*time.Second, typing.who+"'s reconnected tab's sync update"); err != nil {
				return err
			}
		}
		before := storedHead(t, p, artifactID)
		if err := typeAtEnd(typing.editor, typing.text); err != nil {
			if peerEnded(typing.editor) && attempt < 5 {
				continue
			}
			return fmt.Errorf("%s's keystroke: %w", typing.who, err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for storedHead(t, p, artifactID) <= before && !peerEnded(typing.editor) {
			if time.Now().After(deadline) {
				return fmt.Errorf("%s's keystroke was neither stored nor ended with its socket within 5 s", typing.who)
			}
			time.Sleep(2 * time.Millisecond)
		}
		return nil
	}
}

// latestSeq is the issue's latest event sequence.
func latestSeq(t *testing.T, pair *dispatchPair, issueKey string) int {
	t.Helper()
	var seq int
	if err := pair.database.Pool.QueryRow(context.Background(),
		`select coalesce(max(seq), 0) from events where issue_key = $1`, issueKey).Scan(&seq); err != nil {
		t.Fatalf("read the latest event of %s: %v", issueKey, err)
	}
	return seq
}

// versionActorsSince is the actor of each artifact.version event of the issue after seq, in order:
// one per version a settlement or a write minted.
func versionActorsSince(t *testing.T, pair *dispatchPair, issueKey string, seq int) []model.Actor {
	t.Helper()
	rows, err := pair.database.Pool.Query(context.Background(), `
		select actor::text from events
		where issue_key = $1 and type = 'artifact.version' and seq > $2
		order by seq
	`, issueKey, seq)
	if err != nil {
		t.Fatalf("read the versions of %s: %v", issueKey, err)
	}
	defer rows.Close()
	var actors []model.Actor
	for rows.Next() {
		var encoded string
		var actor model.Actor
		if err := rows.Scan(&encoded); err != nil {
			t.Fatalf("scan a version's actor: %v", err)
		}
		if err := json.Unmarshal([]byte(encoded), &actor); err != nil {
			t.Fatalf("decode a version's actor %s: %v", encoded, err)
		}
		actors = append(actors, actor)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the versions of %s: %v", issueKey, err)
	}
	return actors
}

// typeAndSee types text in typist's editor and requires reader's editor, on the other process, to
// show it within liveWithin of the keystroke.
func (p *dispatchPair) typeAndSee(t *testing.T, artifactID, typistName string, typist *docstest.Peer, text, readerName string, reader *docstest.Peer, keystroke int) {
	t.Helper()
	p.requireConnected(t, artifactID, typistName, typist, fmt.Sprintf("before keystroke %d", keystroke))
	if err := typeAtEnd(typist, text); err != nil {
		t.Fatalf("%s's keystroke %d: %v", typistName, keystroke, err)
	}
	typed := time.Now()
	want := markdown(typist)
	if err := reader.WaitForText(markdown, want, liveWithin); err != nil {
		t.Fatalf("%s's keystroke %d did not reach %s live within %v: %v; %s",
			typistName, keystroke, readerName, liveWithin, err, p.roomCloses(t, artifactID))
	}
	t.Logf("%s's keystroke %d reached %s %v after it was typed", typistName, keystroke, readerName, time.Since(typed))
}

// cursorState is the awareness state a dashboard tab announces: who it is and where its cursor is.
func cursorState(login string, at int) map[string]any {
	return map[string]any{"user": map[string]any{"name": login}, "cursor": map[string]any{"anchor": at, "head": at}}
}

// holdsState is a WaitForAwareness condition: client's state is want, compared as JSON values.
func holdsState(client uint64, want any) func(map[uint64]json.RawMessage) bool {
	encoded, _ := json.Marshal(want)
	var wanted any
	_ = json.Unmarshal(encoded, &wanted)
	return func(states map[uint64]json.RawMessage) bool {
		raw, present := states[client]
		if !present {
			return false
		}
		var got any
		return json.Unmarshal(raw, &got) == nil && reflect.DeepEqual(got, wanted)
	}
}

// The awareness client ids the tabs below announce, as a Yjs provider announces its document's.
const (
	aliceAwarenessClient uint64 = 1001
	bobAwarenessClient   uint64 = 2002
)

// dialTabSocket connects a document socket of its own to process, as a browser tab's provider
// does, and drains what the room sends it so the connection never stalls on an unread message.
// The socket closes with the test; a test whose tab must leave sooner closes it itself.
func dialTabSocket(t *testing.T, process *dispatchProcess, artifactID, user string) *gws.Conn {
	t.Helper()
	socket, response, err := gws.DefaultDialer.Dial(process.documentURL(artifactID), asUser(user))
	if err != nil {
		t.Fatalf("connect %s's document socket to the document: response=%#v err=%v", user, response, err)
	}
	t.Cleanup(func() { _ = socket.Close() })
	go func() {
		for {
			if _, _, err := socket.ReadMessage(); err != nil {
				return
			}
		}
	}()
	return socket
}

// announceAwarenessClients has socket announce count awareness clients from first, each at clock
// with the state its id is given, in one awareness frame under artifactID: what a tab's provider
// sends for its own client, re-sent at a later clock as its heartbeat, and what a flooding tab
// sends for the ids it invents.
func announceAwarenessClients(socket *gws.Conn, artifactID string, first, count, clock uint64, state func(client uint64) string) error {
	update := encoding.EncodeBytes(func(e *encoding.Encoder) {
		e.WriteVarUint(count)
		for client := first; client < first+count; client++ {
			e.WriteVarUint(client)
			e.WriteVarUint(clock)
			e.WriteVarString(state(client))
		}
	})
	frame := encoding.EncodeBytes(func(e *encoding.Encoder) {
		e.WriteVarString(artifactID)
		e.WriteVarUint(1) // awareness
		e.WriteVarBytes(update)
	})
	return socket.WriteMessage(gws.BinaryMessage, frame)
}

// Spec item 1: two people on one document, each connected to a different process, see each
// other's typing and cursors within a second, and nobody is disconnected. Each subtest has a
// document of its own, so one behaviour failing does not decide another's.
func TestTwoEditorsOnTwoProcessesSeeEachOthersEditsAndCursorsLive(t *testing.T) {
	pair := startPair(t, nil)
	project := uniqueProjectKey(t, "LIVE")
	pair.a.createProject(t, project, "Live across processes")

	t.Run("each keystroke reaches the editor on the other process within a second", func(t *testing.T) {
		_, artifactID := pair.a.createIssueIn(t, project, "Typed on both", "x")
		alice := pair.a.openEditorAs(t, "alice", artifactID, "x\n")
		bob := pair.b.openEditorAs(t, "bob", artifactID, "x\n")
		for keystroke := 1; keystroke <= 3; keystroke++ {
			pair.typeAndSee(t, artifactID, "alice on A", alice, "a", "bob on B", bob, keystroke)
			pair.typeAndSee(t, artifactID, "bob on B", bob, "b", "alice on A", alice, keystroke)
		}
		pair.requireConnected(t, artifactID, "alice on A", alice, "after the keystrokes")
		pair.requireConnected(t, artifactID, "bob on B", bob, "after the keystrokes")
		pair.requireNoRoomCloses(t, artifactID)
	})

	t.Run("cursors and presence cross processes within a second", func(t *testing.T) {
		_, artifactID := pair.a.createIssueIn(t, project, "Cursors on both", "x")
		alice := pair.a.openEditorAs(t, "alice", artifactID, "x\n")
		if err := alice.SetAwareness(aliceAwarenessClient, cursorState("alice", 0)); err != nil {
			t.Fatalf("announce alice's cursor: %v", err)
		}
		// Bob opens the document second, on the other process: alice's cursor must be there for
		// him at once, not after her tab's next heartbeat.
		bob := pair.b.openEditorAs(t, "bob", artifactID, "x\n")
		if err := bob.WaitForAwareness(liveWithin, holdsState(aliceAwarenessClient, cursorState("alice", 0))); err != nil {
			t.Fatalf("bob, who opened the document second and on B, did not see the cursor alice set on A within %v: %v",
				liveWithin, err)
		}
		if err := bob.SetAwareness(bobAwarenessClient, cursorState("bob", 1)); err != nil {
			t.Fatalf("announce bob's cursor: %v", err)
		}
		if err := alice.WaitForAwareness(liveWithin, holdsState(bobAwarenessClient, cursorState("bob", 1))); err != nil {
			t.Fatalf("the cursor bob set on B did not reach alice on A within %v: %v", liveWithin, err)
		}
		if err := alice.SetAwareness(aliceAwarenessClient, cursorState("alice", 1)); err != nil {
			t.Fatalf("move alice's cursor: %v", err)
		}
		if err := bob.WaitForAwareness(liveWithin, holdsState(aliceAwarenessClient, cursorState("alice", 1))); err != nil {
			t.Fatalf("alice's cursor moving on A did not reach bob on B within %v: %v", liveWithin, err)
		}
		pair.requireConnected(t, artifactID, "alice on A", alice, "while the cursors moved")
		pair.requireConnected(t, artifactID, "bob on B", bob, "while the cursors moved")
		alice.Close()
		if err := bob.WaitForAwareness(liveWithin, func(states map[uint64]json.RawMessage) bool {
			_, present := states[aliceAwarenessClient]
			return !present
		}); err != nil {
			t.Fatalf("alice's tab closing on A did not remove her presence from bob's tab on B within %v: %v", liveWithin, err)
		}
		pair.requireConnected(t, artifactID, "bob on B", bob, "after alice left")
		pair.requireNoRoomCloses(t, artifactID)
	})

	t.Run("an edit through B's API reaches an editor on A within a second", func(t *testing.T) {
		_, artifactID := pair.a.createIssueIn(t, project, "Written through the other API", "x")
		alice := pair.a.openEditorAs(t, "alice", artifactID, "x\n")
		if answer := pair.b.send(t, http.MethodPost, editPath(artifactID), editBody(t, "x", "y", "", false), asUser("bob")); answer.status != http.StatusOK {
			t.Fatalf("bob's edit through B: %v", answer)
		}
		if err := alice.WaitForText(markdown, "y\n", liveWithin); err != nil {
			t.Fatalf("bob's edit through B's API did not reach alice's editor on A live within %v: %v; %s",
				liveWithin, err, pair.roomCloses(t, artifactID))
		}
		pair.requireConnected(t, artifactID, "alice on A", alice, "after bob's edit")
		pair.requireNoRoomCloses(t, artifactID)
	})

	// Finding 7 of the audit: a room whose append another process's write had left behind was
	// failed, and a failed room compacts while it waits for the document lock - the one
	// compaction that does. Two people taking turns, each keystroke stored before the next,
	// must fail no room and run no such compaction on either process (the room-counters hook).
	// A typist whose socket closes reconnects as the dashboard does, so a process that fails
	// rooms shows it in the counters rather than stopping the session at its first casualty.
	t.Run("two hundred alternating keystrokes fail no room and run no lock-waiting compaction", func(t *testing.T) {
		_, artifactID := pair.a.createIssueIn(t, project, "Taking turns", "x")
		typists := []*typist{
			{who: "alice on A", login: "alice", process: pair.a, text: "a"},
			{who: "bob on B", login: "bob", process: pair.b, text: "b"},
		}
		typists[0].editor = pair.openStoredEditor(t, pair.a, "alice", artifactID, "x\n")
		typists[1].editor = pair.openStoredEditor(t, pair.b, "bob", artifactID, "x\n")
		const keystrokes = 200
		var stopped error
		typed := 0
		for ; typed < keystrokes && stopped == nil; typed++ {
			stopped = pair.keystroke(t, artifactID, typists[typed%2])
		}
		countersA, countersB := pair.a.roomCounters(t, artifactID), pair.b.roomCounters(t, artifactID)
		t.Logf("after %d keystrokes: A %+v, B %+v", typed, countersA, countersB)
		if countersA.RoomFailures+countersA.LockWaitingCompactions+countersB.RoomFailures+countersB.LockWaitingCompactions != 0 {
			t.Errorf("finding 7: %d alternating keystrokes failed rooms of the document and ran lock-waiting compactions: on A %d room failures and %d lock-waiting compactions, on B %d and %d; want none on either",
				typed, countersA.RoomFailures, countersA.LockWaitingCompactions, countersB.RoomFailures, countersB.LockWaitingCompactions)
		}
		for _, typing := range typists {
			if disconnected := typing.disconnected(); disconnected != "" {
				t.Errorf("%s during the session and had to reconnect", disconnected)
			}
		}
		if stopped != nil {
			t.Errorf("the session stopped after %d keystrokes: %v", typed, stopped)
		}
		pair.requireNoRoomCloses(t, artifactID)
		if t.Failed() {
			return
		}
		alice, bob := typists[0].editor, typists[1].editor
		deadline := time.Now().Add(5 * time.Second)
		for {
			seenByAlice, seenByBob := markdown(alice), markdown(bob)
			if seenByAlice == seenByBob && strings.Count(seenByAlice, "a") == keystrokes/2 && strings.Count(seenByAlice, "b") == keystrokes/2 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("five seconds after the last keystroke the editors disagree or lost keystrokes: alice on A has %q, bob on B has %q; want both to hold %d of each", seenByAlice, seenByBob, keystrokes/2)
			}
			if err := alice.AskForDocument(); err != nil {
				t.Fatalf("ask A's room for the document: %v", err)
			}
			if err := bob.AskForDocument(); err != nil {
				t.Fatalf("ask B's room for the document: %v", err)
			}
			time.Sleep(20 * time.Millisecond)
		}
	})

	// One person types, then the other, each within the settle delay of the last: on one process
	// that is one burst and one version, credited to whoever typed last. Across two processes it
	// must be too (plan D3.7), whichever process wins the settlement.
	t.Run("alternating typists within the debounce make one version credited to the last typist", func(t *testing.T) {
		issueKey, artifactID := pair.a.createIssueIn(t, project, "One burst", "x")
		waitForSettlementOwed(t, pair.database, artifactID, false)
		baseline := latestSeq(t, pair, issueKey)
		typists := []*typist{
			{who: "alice on A", login: "alice", process: pair.a, text: "a"},
			{who: "bob on B", login: "bob", process: pair.b, text: "b"},
		}
		typists[0].editor = pair.openStoredEditor(t, pair.a, "alice", artifactID, "x\n")
		typists[1].editor = pair.openStoredEditor(t, pair.b, "bob", artifactID, "x\n")
		for keystroke := range 6 {
			if err := pair.keystroke(t, artifactID, typists[keystroke%2]); err != nil {
				t.Fatalf("keystroke %d: %v", keystroke+1, err)
			}
		}
		lastTypist := typists[1]
		burst := time.Now()
		// The default settle delay is two seconds on each process; fifteen is room for a
		// settlement that retried, and the wait ends as soon as the burst has settled.
		deadline := burst.Add(15 * time.Second)
		for (settlementOwed(t, pair.database, artifactID) || len(versionActorsSince(t, pair, issueKey, baseline)) == 0) && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		actors := versionActorsSince(t, pair, issueKey, baseline)
		t.Logf("%v after the burst: versions credited to %+v; settlement owed: %t", time.Since(burst), actors, settlementOwed(t, pair.database, artifactID))
		if len(actors) != 1 || actors[0].Kind != "user" || actors[0].ID != lastTypist.login {
			t.Errorf("six alternating keystrokes within the settle delay made %d versions, credited to %+v; want one version, credited to %s, the last typist",
				len(actors), actors, lastTypist.login)
		}
		for _, typing := range typists {
			if disconnected := typing.disconnected(); disconnected != "" {
				t.Errorf("%s during the burst", disconnected)
			}
		}
	})
}

// patchIssueStatus sets the issue's status through the process's API: "done" closes it, any other
// status reopens it.
func (p *dispatchProcess) patchIssueStatus(t *testing.T, issueKey, status string) {
	t.Helper()
	p.call(t, http.MethodPatch, "/api/v1/issues/"+issueKey, fmt.Sprintf(`{"status":%q}`, status), http.StatusOK)
}

// requireEndsWithin waits up to within for the editor's socket to close, and fails the test with
// what the process logged when it stays open.
func requireEndsWithin(t *testing.T, editor *docstest.Peer, within time.Duration, what string, process *dispatchProcess) {
	t.Helper()
	select {
	case <-editor.Ended:
	case <-time.After(within):
		t.Fatalf("%s: the socket stayed open %v later.\nThat process's output:\n%s", what, within, process.Output.String())
	}
}

// reconnectWithScope reconnects login's tab to the document on process, authenticates it as the
// dashboard does, and requires the scope the room grants it.
func reconnectWithScope(t *testing.T, process *dispatchProcess, login, artifactID, scope, what string) *docstest.Peer {
	t.Helper()
	editor, err := process.reconnectEditor(t, login, artifactID)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if granted := authenticateEditor(t, editor); granted != scope {
		t.Fatalf("%s: the room granted %q, want %q", what, granted, scope)
	}
	return editor
}

// Spec item 2: closing or reopening an issue changes every process's copy of its documents at
// once. A tab on the process that did not take the request is closed with the others and comes
// back with the scope the issue now allows; an update sent on the closed issue is not stored.
//
// Bob opens his tab on B before alice opens hers on A, so the room alice is in stands at the
// stored head when the issue closes: what closes her socket, if anything does, is the close.
func TestClosingAndReopeningAnIssueChangesEveryProcessesRooms(t *testing.T) {
	pair := startPair(t, nil)
	project := uniqueProjectKey(t, "SHUT")
	pair.a.createProject(t, project, "Closed across processes")
	issueKey, artifactID := pair.b.createIssueIn(t, project, "Closed on the other process", "before")

	bob := pair.openStoredEditor(t, pair.b, "bob", artifactID, "before\n")
	alice := pair.openStoredEditor(t, pair.a, "alice", artifactID, "before\n")
	if scope := authenticateEditor(t, alice); scope != "read-write" {
		t.Fatalf("alice's tab on A on an open issue was granted %q, want read-write", scope)
	}
	bobEndedBeforeClose := peerEnded(bob)

	pair.b.patchIssueStatus(t, issueKey, "done")
	requireEndsWithin(t, alice, liveWithin,
		"closing the issue through B left alice's tab on A connected to a room that still takes edits on the closed issue", pair.a)
	if bobEndedBeforeClose {
		t.Fatalf("bob's tab on B was disconnected before the close (close code %d), so the close cannot be shown closing it; %s",
			bob.CloseCode(), pair.roomCloses(t, artifactID))
	}
	requireEndsWithin(t, bob, liveWithin, "closing the issue through B left bob's tab on B connected", pair.b)

	alice = reconnectWithScope(t, pair.a, "alice", artifactID, "readonly", "alice's tab on A, reconnected after the close")
	bob = reconnectWithScope(t, pair.b, "bob", artifactID, "readonly", "bob's tab on B, reconnected after the close")
	closedHead := storedHead(t, pair, artifactID)
	if err := typeAtEnd(alice, "zzz"); err != nil {
		t.Fatalf("alice's keystroke on the closed issue: %v", err)
	}
	held, err := roomRendering(alice, 5*time.Second)
	if err != nil {
		t.Fatalf("read A's room after alice's keystroke on the closed issue: %v", err)
	}
	if strings.Contains(held, "zzz") {
		t.Errorf("A's room took alice's keystroke on the closed issue: it holds %q", held)
	}
	if head := storedHead(t, pair, artifactID); head != closedHead {
		t.Errorf("the document moved from update %d to %d after the close: an update on a closed issue was stored", closedHead, head)
	}

	pair.a.patchIssueStatus(t, issueKey, "todo")
	requireEndsWithin(t, alice, liveWithin, "reopening the issue through A left alice's read-only tab on A connected", pair.a)
	requireEndsWithin(t, bob, liveWithin,
		"reopening the issue through A left bob's read-only tab on B connected, so it cannot edit the reopened issue", pair.b)
	alice = reconnectWithScope(t, pair.a, "alice", artifactID, "read-write", "alice's tab on A, reconnected after the reopen")
	bob = reconnectWithScope(t, pair.b, "bob", artifactID, "read-write", "bob's tab on B, reconnected after the reopen")
	for _, edit := range []struct {
		who    string
		editor *docstest.Peer
		text   string
	}{{"alice on A", alice, "typed-a"}, {"bob on B", bob, "typed-b"}} {
		if err := typeAtEnd(edit.editor, edit.text); err != nil {
			t.Fatalf("%s's keystroke on the reopened issue: %v", edit.who, err)
		}
		pair.waitStoredHolds(t, artifactID, edit.text, 5*time.Second, edit.who+"'s keystroke on the reopened issue")
	}
}

// documentAsk is an ask a document's settlement indexed: its id, its block and its author.
type documentAsk struct {
	ID      string      `json:"id"`
	BlockID *string     `json:"block_id"`
	Author  model.Actor `json:"author"`
}

// askOfBlock is the ask a settlement made of blockID on the issue, read through the process's API
// (GET /api/v1/issues/{key}/asks), or false while there is none.
func (p *dispatchProcess) askOfBlock(t *testing.T, issueKey, blockID string) (documentAsk, bool) {
	t.Helper()
	answer := p.call(t, http.MethodGet, "/api/v1/issues/"+issueKey+"/asks", "", http.StatusOK)
	var asks []documentAsk
	if err := json.Unmarshal(answer, &asks); err != nil {
		t.Fatalf("decode the document's asks: %v: %s", err, answer)
	}
	for _, ask := range asks {
		if ask.BlockID != nil && *ask.BlockID == blockID {
			return ask, true
		}
	}
	return documentAsk{}, false
}

// readAsk is the ask as the process's API reads it (GET /api/v1/asks/{id}).
func (p *dispatchProcess) readAsk(t *testing.T, askID string) model.Ask {
	t.Helper()
	answer := p.call(t, http.MethodGet, "/api/v1/asks/"+askID, "", http.StatusOK)
	var read struct {
		Ask model.Ask `json:"ask"`
	}
	if err := json.Unmarshal(answer, &read); err != nil {
		t.Fatalf("decode the ask: %v: %s", err, answer)
	}
	return read.Ask
}

// requireAskWithdrawn fails the test unless the process reads the ask as the close of issueKey
// withdrew it: resolved, its resolution of kind withdrawn naming that close.
func requireAskWithdrawn(t *testing.T, p *dispatchProcess, issueKey, askID, when string) {
	t.Helper()
	ask := p.readAsk(t, askID)
	if ask.State != "resolved" || ask.Resolution == nil || ask.Resolution.Kind != "withdrawn" ||
		ask.Resolution.Reason != "Withdrawn by the close of "+issueKey+"." {
		t.Fatalf("%s: the ask reads %s with resolution %+v, want it withdrawn by the close of %s", when, ask.State, ask.Resolution, issueKey)
	}
}

// closingAskSpec is the spec of the issue below: a paragraph to type into and an open decision.
const closingAskSpec = "Context\n\n:::ask{#ship-it urgency=\"med\" multiple=\"false\"}\nShip it?\n\n- Yes: Now.\n- No: Later.\n:::\n"

// The issue's close withdraws its open ask and freezes every process's room of its spec, and
// neither undoes the other; its reopen brings both back.
//
// Bob's tab on B holds the spec, whose decision block is an open ask, and his keystroke there is
// stored, which arms B's settlement of it. The issue closes through A. B's room closes and bob
// reconnects read-only; the ask reads withdrawn and the block resolved, and both still do once
// the settlement B had armed was due, with no version written on the closed issue. The reopen goes
// through B: the ask reads open again, bob's read-only tab is closed and reconnects writable, his
// keystroke is stored, and the settlement the reopened document owes leaves the ask open.
func TestClosingAnIssueOnOneProcessWithdrawsItsAskAndFreezesTheOtherProcessesRoomUntilItsReopen(t *testing.T) {
	const settleDelay = 2 * time.Second
	pair := newPair(t)
	pair.startSettling(t, nil, settleDelay, settleDelay)
	project := uniqueProjectKey(t, "WDRN")
	pair.a.createProject(t, project, "Asks of a closed issue across processes")
	issueKey, artifactID := pair.a.createIssueIn(t, project, "Closed with an open ask", closingAskSpec)
	var askID string
	for deadline := time.Now().Add(10 * time.Second); askID == ""; time.Sleep(50 * time.Millisecond) {
		if ask, found := pair.a.askOfBlock(t, issueKey, "ship-it"); found {
			askID = ask.ID
		} else if time.Now().After(deadline) {
			t.Fatalf("A's settlement never made an ask of the spec's decision block:\nA:\n%s", pair.a.Output.String())
		}
	}
	waitForSettlementOwed(t, pair.database, artifactID, false)
	if ask := pair.b.readAsk(t, askID); ask.State != "open" {
		t.Fatalf("the decision's ask reads %s before the close, want open", ask.State)
	}

	bob := pair.openStoredEditor(t, pair.b, "bob", artifactID, pair.b.documentText(t, artifactID))
	if scope := authenticateEditor(t, bob); scope != "read-write" {
		t.Fatalf("bob's tab on B on an open issue was granted %q, want read-write", scope)
	}
	typed := time.Now()
	if err := typeAtEnd(bob, " late"); err != nil {
		t.Fatalf("bob's keystroke before the close: %v", err)
	}
	pair.waitStoredHolds(t, artifactID, "Context late", 5*time.Second, "bob's keystroke before the close")
	if peerEnded(bob) {
		t.Fatalf("bob's tab on B was disconnected before the close (close code %d), so the close cannot be shown closing it; %s",
			bob.CloseCode(), pair.roomCloses(t, artifactID))
	}

	pair.a.patchIssueStatus(t, issueKey, "done")
	// Whatever settled before the close has committed; nothing may write a version from here.
	versionBefore, _, _ := latestVersion(t, pair.database, artifactID)
	requireEndsWithin(t, bob, liveWithin, "closing the issue through A left bob's tab on B connected", pair.b)
	bob = reconnectWithScope(t, pair.b, "bob", artifactID, "readonly", "bob's tab on B, reconnected after the close")
	requireAskWithdrawn(t, pair.b, issueKey, askID, "after the close")
	const resolvedBlock = `#ship-it urgency="med" multiple="false" state="resolved"`
	requireClosedText := func(what, text string) {
		t.Helper()
		if !strings.Contains(text, "Context late") || !strings.Contains(text, resolvedBlock) {
			t.Fatalf("%s of the closed issue does not hold bob's keystroke and the decision block resolved:\n%s", what, text)
		}
	}
	room, err := roomRendering(bob, 5*time.Second)
	if err != nil {
		t.Fatalf("read B's room after the close: %v", err)
	}
	requireClosedText("B's room", room)
	// B armed its settlement of bob's keystroke a settle delay after its append, and the close
	// stopped it; were it running, it would settle by then. The ask, the stored document and the
	// versions are read once it was due, so nothing B's room did after the close is missed.
	time.Sleep(time.Until(typed.Add(settleDelay + time.Second)))
	requireAskWithdrawn(t, pair.a, issueKey, askID, "once B's settlement of bob's keystroke was due")
	requireClosedText("the stored document", pair.a.documentText(t, artifactID))
	if version, _, _ := latestVersion(t, pair.database, artifactID); version != versionBefore {
		t.Fatalf("the document's latest version moved from %d to %d while its issue was closed: a settlement ran on the closed issue", versionBefore, version)
	}

	pair.b.patchIssueStatus(t, issueKey, "todo")
	if ask := pair.a.readAsk(t, askID); ask.State != "open" || ask.Resolution != nil {
		t.Fatalf("the reopen left the ask %s with resolution %+v, want it open again with none", ask.State, ask.Resolution)
	}
	requireEndsWithin(t, bob, liveWithin, "reopening the issue through B left bob's read-only tab on B connected", pair.b)
	bob = reconnectWithScope(t, pair.b, "bob", artifactID, "read-write", "bob's tab on B, reconnected after the reopen")
	if reopened, err := roomRendering(bob, 5*time.Second); err != nil || !strings.Contains(reopened, `#ship-it urgency="med" multiple="false" state="open"`) {
		t.Fatalf("B's room of the reopened issue does not hold the decision block open (%v):\n%s", err, reopened)
	}
	if err := typeAtEnd(bob, " again"); err != nil {
		t.Fatalf("bob's keystroke on the reopened issue: %v", err)
	}
	pair.waitStoredHolds(t, artifactID, "Context late again", 5*time.Second, "bob's keystroke on the reopened issue")
	waitForSettlementOwed(t, pair.database, artifactID, false)
	if ask := pair.b.readAsk(t, askID); ask.State != "open" {
		t.Fatalf("the reopened document's settlement left the ask %s, want open", ask.State)
	}
}

// Spec item 3: an ask block keeps the author who wrote it, whichever process settles it.
//
// Bob writes an ask block in his tab on B, and B is killed (SIGKILL) before it settles - B's
// settle delay is ten minutes - so the settlement the document owes is left to the resumption on
// A. The pending row is made older than the resumption age (one minute) by SQL rather than by
// waiting it out, so A's next pass (one a minute) arms it.
//
// Alice edits the document through B after bob's block and before the kill. Without her, the
// latest edit source the pending row names is bob's, and at this row's first commit a settlement
// that does not know who introduced a block credits the block to that latest source (the
// fallback in askBlockSources.author), so the row would pass by coincidence with the author
// living only in B's memory. With her, that fallback names alice, and only an author recorded
// where A can read it names bob.
func TestAnAskBlockKeepsItsAuthorWhenAnotherProcessSettlesIt(t *testing.T) {
	pair := newPair(t)
	pair.startSettling(t, nil, 0, 10*time.Minute)
	project := uniqueProjectKey(t, "ASKA")
	pair.b.createProject(t, project, "Ask authors across processes")
	// Created through B, whose own settlement is ten minutes away, so A never loads this
	// document before its resumption does.
	issueKey, artifactID := pair.b.createIssueIn(t, project, "Bob asks on B", "Intro.")

	bob := pair.openStoredEditor(t, pair.b, "bob", artifactID, "Intro.\n")
	const blockID = "ask-bob-region"
	head := storedHead(t, pair, artifactID)
	appendMarkdown(t, bob, ":::ask{#"+blockID+` urgency="med" multiple="false"}`+"\nWhich region goes first?\n:::\n")
	pair.waitStoredPast(t, artifactID, head, 10*time.Second, "bob's ask block")
	if answer := pair.b.send(t, http.MethodPost, editPath(artifactID), editBody(t, "Intro.", "Intro, edited.", "", false), asUser("alice")); answer.status != http.StatusOK {
		t.Fatalf("alice's edit through B after bob's ask block: %v", answer)
	}
	if _, found := pair.b.askOfBlock(t, issueKey, blockID); found {
		t.Fatalf("B already settled bob's ask block, so no other process is left to settle it")
	}
	pair.b.kill(t)

	backdated, err := pair.database.Pool.Exec(context.Background(), `
		update doc_settlements_pending set marked_at = now() - interval '2 minutes' where artifact_id = $1
	`, artifactID)
	if err != nil {
		t.Fatalf("age the document's pending settlement: %v", err)
	}
	if backdated.RowsAffected() != 1 {
		t.Fatalf("B left no pending settlement for the document, so there is nothing for A to resume")
	}
	// A resumption pass runs once a minute; two and a half minutes is two passes and the settle
	// delay after the second.
	const within = 150 * time.Second
	deadline := time.Now().Add(within)
	for {
		ask, found := pair.a.askOfBlock(t, issueKey, blockID)
		if found {
			if ask.Author.Kind != "user" || ask.Author.ID != "bob" {
				t.Fatalf("A's resumed settlement made the ask of bob's block with author %+v, want bob (user), who wrote it on B; A's resumptions armed the document's settlement at %v",
					ask.Author, pair.a.roomCounters(t, artifactID).SettlementResumptions)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("A made no ask of bob's block within %v of the settlement becoming due for resumption:\n%s", within, pair.a.Output.String())
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// backgroundRequest is a request sent from a goroutine of its own, for a request a row parks
// with a hook while it does something else.
type backgroundRequest struct {
	answered chan httpAnswer
	failed   chan error
}

// requestInBackground sends the request from a goroutine of its own and returns at once.
func (p *dispatchProcess) requestInBackground(method, path, body string, header http.Header) backgroundRequest {
	pending := backgroundRequest{answered: make(chan httpAnswer, 1), failed: make(chan error, 1)}
	go func() {
		answer, err := p.trySend(method, path, body, header)
		if err != nil {
			pending.failed <- err
			return
		}
		pending.answered <- answer
	}()
	return pending
}

// await waits up to within for the request's answer, and fails the test when it does not come.
func (pending backgroundRequest) await(t *testing.T, within time.Duration, what string, process *dispatchProcess) httpAnswer {
	t.Helper()
	select {
	case answer := <-pending.answered:
		return answer
	case err := <-pending.failed:
		t.Fatalf("%s: %v", what, err)
	case <-time.After(within):
		t.Fatalf("%s: no answer within %v:\n%s", what, within, process.Output.String())
	}
	return httpAnswer{}
}

// The document item 4's cases write into: P, the middle paragraph, is the one bob deletes and the
// agent writes into.
const (
	agentRowSpec     = "Intro.\n\nThe pear is ripe.\n\nOutro."
	agentRowWithoutP = "Intro.\n\nOutro.\n"
)

// lostEditAnswer is the body of a lost-edit refusal or of an edit's 200: the code and
// participants of the first, the lost_ops of either.
type lostEditAnswer struct {
	Code         string        `json:"code"`
	Participants []model.Actor `json:"participants"`
	LostOps      *[]int        `json:"lost_ops"`
}

func decodeLostEdit(t *testing.T, answer httpAnswer) lostEditAnswer {
	t.Helper()
	var decoded lostEditAnswer
	if err := json.Unmarshal(answer.body, &decoded); err != nil {
		t.Fatalf("decode the edit's answer: %v: %v", err, answer)
	}
	return decoded
}

// Spec item 4: an agent's write through one process sees what a person on the other process
// typed before it, and its lost-edit check judges against every process. Bob's tab is on B, with a
// second tab of his there that shows when B's room has applied his delete; A holds no room of
// the document, so the agent's write forks it from the store. Each case has its own document.
func TestAnAgentWriteSeesEditsInFlightOnTheOtherProcess(t *testing.T) {
	pair := startPair(t, nil)
	project := uniqueProjectKey(t, "AGNT")
	pair.b.createProject(t, project, "Agent writes across processes")

	// open makes the case's document through B, opens bob's two tabs on it there, and requires
	// that A holds no room of it.
	open := func(t *testing.T, title string) (artifactID string, bob, bobsOtherTab *docstest.Peer) {
		t.Helper()
		_, artifactID = pair.b.createIssueIn(t, project, title, agentRowSpec)
		waitForSettlementOwed(t, pair.database, artifactID, false)
		want := pair.b.documentText(t, artifactID)
		bob = pair.openStoredEditor(t, pair.b, "bob", artifactID, want)
		bobsOtherTab = pair.openStoredEditor(t, pair.b, "bob", artifactID, want)
		if pair.a.holdsRoom(t, artifactID) {
			t.Fatalf("A holds a room of the document before the agent writes, so its write would not be the cold fork this case is about")
		}
		return artifactID, bob, bobsOtherTab
	}
	// deleteP deletes P in bob's tab and waits for B's room to show his other tab the delete,
	// leaving want.
	deleteP := func(t *testing.T, artifactID string, bob, bobsOtherTab *docstest.Peer, want string) {
		t.Helper()
		deleteParagraph(t, bob, "pear")
		if err := bobsOtherTab.WaitForText(markdown, want, liveWithin); err != nil {
			t.Fatalf("B's room never showed bob's delete of P to his other tab: %v; %s", err, pair.roomCloses(t, artifactID))
		}
	}

	t.Run("before the fork: an edit into a paragraph deleted on the other process is refused", func(t *testing.T) {
		artifactID, bob, bobsOtherTab := open(t, "Deleted before the agent forks")
		// B applies and broadcasts bob's delete at once and stores it 300 ms later.
		pair.b.holdPersistence(t, artifactID, 300*time.Millisecond)
		deleted := time.Now()
		deleteP(t, artifactID, bob, bobsOtherTab, agentRowWithoutP)
		answer := pair.a.send(t, http.MethodPost, editPath(artifactID), editBody(t, "pear", "plum", "", true), asAgent())
		t.Logf("the agent's edit through A answered %v after bob's delete", time.Since(deleted))
		if answer.status == http.StatusOK {
			t.Fatalf("the agent's edit through A of a paragraph bob had already deleted on B answered 200: A forked the document from a store that did not hold the delete B's room had applied (its append was held 300 ms), so the write landed in a deleted paragraph; want 404 TARGET_NOT_FOUND. Answer: %v", answer)
		}
		if answer.status != http.StatusNotFound || answer.code() != "TARGET_NOT_FOUND" {
			t.Fatalf("the agent's edit of a paragraph bob had deleted on B answered %v, want 404 TARGET_NOT_FOUND", answer)
		}
	})

	t.Run("while the write is open: a delete on the other process refuses it with its participants", func(t *testing.T) {
		artifactID, bob, bobsOtherTab := open(t, "Deleted while the agent writes")
		head := storedHead(t, pair, artifactID)
		pair.a.armLiveWritePause(t, artifactID, docs.PauseAfterFirstAppend)
		write := pair.a.requestInBackground(http.MethodPost, editPath(artifactID), editBody(t, "pear", "pear tart", "", true), asAgent())
		// Parked after its first append, the write holds the document's advisory lock, so B's
		// append of bob's delete waits on it: the delete is in B's room and not stored.
		pair.a.awaitLiveWriteParked(t, artifactID, docs.PauseAfterFirstAppend)
		deleteP(t, artifactID, bob, bobsOtherTab, agentRowWithoutP)
		if moved := storedHead(t, pair, artifactID); moved != head {
			t.Fatalf("the document moved from update %d to %d while the agent's write held its lock, so bob's delete is not the update in flight this case is about", head, moved)
		}
		if !pair.a.releaseLiveWritePause(t, artifactID, docs.PauseAfterFirstAppend) {
			t.Fatalf("the release found no write parked")
		}
		answer := write.await(t, 30*time.Second, "the agent's edit through A, after its release", pair.a)
		decoded := decodeLostEdit(t, answer)
		if answer.status != http.StatusConflict || decoded.Code != "EDIT_LOST_TO_CONCURRENT_CHANGE" {
			t.Fatalf("the agent's write into P through A, which bob deleted on B while the write was open, answered %v; want 409 EDIT_LOST_TO_CONCURRENT_CHANGE naming bob: the write's check judged a fork that never saw B's room, so the paragraph it wrote into reads as alive", answer)
		}
		if !slices.ContainsFunc(decoded.Participants, func(actor model.Actor) bool { return actor.Kind == "user" && actor.ID == "bob" }) {
			t.Errorf("the lost-edit refusal names participants %+v; want bob, connected on B, among them", decoded.Participants)
		}
		deadline := time.Now().Add(5 * time.Second)
		for strings.Contains(pair.a.documentText(t, artifactID), "pear") {
			if time.Now().After(deadline) {
				t.Fatalf("bob's delete was not stored within 5 s of the refused write rolling back: the document reads %q", pair.a.documentText(t, artifactID))
			}
			time.Sleep(20 * time.Millisecond)
		}
	})

	// The write that is judged here first commits and then publishes, so B learns of its commit
	// before bob deletes. An earlier agent write through A, which this case does not judge, is
	// what makes that hold at this row's first commit too: a commit on A closes B's room there
	// unless an eviction of that room inside the last two seconds holds it off, so where the
	// earlier write closes B's room, bob's tabs reconnect as a browser's do, and the judged write
	// lands inside the window that close opened - B's room stays open, and behind.
	t.Run("after the commit: a delete on the other process is reported in lost_ops", func(t *testing.T) {
		artifactID, bob, bobsOtherTab := open(t, "Deleted before the agent publishes")
		const earlier = "Intro, first.\n\nThe pear is ripe.\n\nOutro.\n"
		if answer := pair.a.send(t, http.MethodPost, editPath(artifactID), editBody(t, "Intro.", "Intro, first.", "", true), asAgent()); answer.status != http.StatusOK {
			t.Fatalf("the agent's earlier write through A: %v", answer)
		}
		var evicted time.Time
		if err := bobsOtherTab.WaitForText(markdown, earlier, liveWithin); err != nil {
			if !peerEnded(bobsOtherTab) {
				t.Fatalf("the agent's earlier write through A reached neither bob's tab on B nor closed it within %v: %v", liveWithin, err)
			}
			evicted = time.Now()
			t.Logf("the earlier write closed B's room (bob's tab: close code %d); bob's tabs reconnect", bobsOtherTab.CloseCode())
			bob = pair.reconnectStored(t, pair.b, "bob", artifactID)
			bobsOtherTab = pair.reconnectStored(t, pair.b, "bob", artifactID)
			// Bob's reconnects are writes on B, which close the room the earlier write's
			// publish left on A: the judged write is to fork the store, as a cold write does.
			deadline := time.Now().Add(5 * time.Second)
			for pair.a.holdsRoom(t, artifactID) {
				if time.Now().After(deadline) {
					t.Fatalf("A still holds the room the earlier write's publish loaded, so the judged write would not fork cold")
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		pair.a.armLiveWritePause(t, artifactID, docs.PauseAfterCommit)
		write := pair.a.requestInBackground(http.MethodPost, editPath(artifactID), editBody(t, "pear", "pear tart", "", true), asAgent())
		pair.a.awaitLiveWriteParked(t, artifactID, docs.PauseAfterCommit)
		// The write is committed; bob's delete reaches B's room now, and B stores it 300 ms on.
		pair.b.holdPersistence(t, artifactID, 300*time.Millisecond)
		deleteP(t, artifactID, bob, bobsOtherTab, "Intro, first.\n\nOutro.\n")
		if !evicted.IsZero() {
			if spent := time.Since(evicted); spent >= 1500*time.Millisecond {
				t.Fatalf("the judged write and bob's delete took %v of B's two-second eviction window, so B's room may close before the write is judged; this case measured nothing", spent)
			}
		}
		if !pair.a.releaseLiveWritePause(t, artifactID, docs.PauseAfterCommit) {
			t.Fatalf("the release found no write parked")
		}
		answer := write.await(t, 30*time.Second, "the agent's edit through A, after its release", pair.a)
		if answer.status != http.StatusOK {
			t.Fatalf("the agent's committed write answered %v, want 200 with lost_ops", answer)
		}
		decoded := decodeLostEdit(t, answer)
		if decoded.LostOps == nil || !slices.Equal(*decoded.LostOps, []int{0}) {
			got := "null"
			if decoded.LostOps != nil {
				got = fmt.Sprint(*decoded.LostOps)
			}
			t.Fatalf("the agent's write into P, which bob deleted on B after it committed and before it published, answered 200 with lost_ops %s; want [0], its one operation: the loss was judged on A's room alone, which never receives B's delete. Answer: %v", got, answer)
		}
	})
}

// reconnectStored reconnects login's tab on process and waits for the update its sync appends to
// be stored, as openStoredEditor does for a first connection.
func (p *dispatchPair) reconnectStored(t *testing.T, process *dispatchProcess, login, artifactID string) *docstest.Peer {
	t.Helper()
	head := storedHead(t, p, artifactID)
	editor, err := process.reconnectEditor(t, login, artifactID)
	if err != nil {
		t.Fatalf("%v:\n%s", err, process.Output.String())
	}
	p.waitStoredPast(t, artifactID, head, 10*time.Second, login+"'s reconnected tab's sync update on "+p.name(process))
	return editor
}

// Spec item 5: a read never returns text older than the store holds, whichever process serves it.
func TestAReadOnTheOtherProcessIsNeverOlderThanTheStore(t *testing.T) {
	pair := startPair(t, nil)
	project := uniqueProjectKey(t, "READ")
	pair.a.createProject(t, project, "Reads across processes")

	t.Run("a read on B right after each write on A shows it and its token passes the next conditional write", func(t *testing.T) {
		_, artifactID := pair.a.createIssueIn(t, project, "Written on A, read on B", "v0")
		bob := pair.openStoredEditor(t, pair.b, "bob", artifactID, "v0\n")
		_, token := pair.a.textAndToken(t, artifactID)
		reconnects := 0
		for write := 1; write <= 50; write++ {
			before, after := fmt.Sprintf("v%d", write-1), fmt.Sprintf("v%d", write)
			if peerEnded(bob) {
				reconnects++
				bob = pair.reconnectStored(t, pair.b, "bob", artifactID)
			}
			answer := pair.a.send(t, http.MethodPost, editPath(artifactID), editBody(t, before, after, token, false), asUser("alice"))
			if answer.status != http.StatusOK {
				t.Fatalf("write %d through A, conditional on the token of the read B served after write %d, answered %v: that read's token was not the stored document's",
					write, write-1, answer)
			}
			read, readToken := pair.b.textAndToken(t, artifactID)
			if read != after+"\n" {
				t.Fatalf("read %d on B, sent the moment write %d through A answered 200, answered %q: older than the store, which holds %q. Bob's tab on B, which keeps B's room resident, reconnected %d times by then, as a browser whose room was closed does; %s",
					write, write, read, after+"\n", reconnects, pair.roomCloses(t, artifactID))
			}
			token = readToken
		}
	})

	// A comment anchored to a mark a browser placed is verified against the document (VerifyMark),
	// which waits up to a second (MarkWait) for the mark to arrive. Alice places the mark on A;
	// the comment goes through B, where bob's tab keeps a room of the document.
	//
	// While alice connects and marks, a write through B is parked after its commit, in its
	// publish (the live-write pause hook), and released once the mark is stored: a process that
	// keeps its rooms current shows the mark whatever that write does.
	t.Run("a mark placed by an editor on A is verified by a comment anchored through B", func(t *testing.T) {
		issueKey, artifactID := pair.b.createIssueIn(t, project, "Marked on A, anchored through B", "The quick brown fox")
		bob := pair.openStoredEditor(t, pair.b, "bob", artifactID, "The quick brown fox\n")
		pair.b.armLiveWritePause(t, artifactID, docs.PauseAfterCommit)
		write := pair.b.requestInBackground(http.MethodPost, editPath(artifactID), editBody(t, "brown", "red", "", false), asUser("carol"))
		pair.b.awaitLiveWriteParked(t, artifactID, docs.PauseAfterCommit)
		alice := pair.openStoredEditor(t, pair.a, "alice", artifactID, "The quick red fox\n")
		markID := "mark-" + uniqueProjectKey(t, "M")
		head := storedHead(t, pair, artifactID)
		placeMark(t, alice, "alice", "quick", markID)
		pair.waitStoredPast(t, artifactID, head, 5*time.Second, "alice's mark")
		if !pair.b.releaseLiveWritePause(t, artifactID, docs.PauseAfterCommit) {
			t.Fatalf("the release found no write parked")
		}
		if answer := write.await(t, 30*time.Second, "carol's write through B, after its release", pair.b); answer.status != http.StatusOK {
			t.Fatalf("carol's write through B: %v", answer)
		}
		pair.requireConnected(t, artifactID, "bob on B", bob, "before the comment")
		body := fmt.Sprintf(`{"body":"why","anchor":{"artifact":%q,"mark_id":%q}}`, artifactID, markID)
		answer := pair.b.send(t, http.MethodPost, "/api/v1/issues/"+issueKey+"/comments", body, asUser("bob"))
		if answer.status != http.StatusCreated {
			t.Fatalf("bob's comment through B, anchored on the mark alice placed on A and the store holds, answered %v; want 201: B verified the mark against a room older than the store", answer)
		}
		var comment struct {
			Anchor struct {
				Quote string `json:"quote"`
			} `json:"anchor"`
		}
		if err := json.Unmarshal(answer.body, &comment); err != nil || comment.Anchor.Quote != "quick" {
			t.Fatalf("the comment's anchor quotes %q (%v), want %q: %v", comment.Anchor.Quote, err, "quick", answer)
		}
	})
}

// corruptHistory makes the document's stored history one update ygo cannot decode, at the head
// it stood at, so a rebuild has something to rebuild and no room's cursor moves.
func corruptHistory(t *testing.T, pair *dispatchPair, artifactID string) {
	t.Helper()
	ctx := context.Background()
	tx, err := pair.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the corruption: %v", err)
	}
	defer tx.Rollback(ctx)
	for _, statement := range []string{
		`delete from doc_checkpoints where artifact_id = $1`,
		`delete from doc_updates where artifact_id = $1 and version < (select max(version) from doc_updates where artifact_id = $1)`,
		`update doc_updates set update = decode('deadbeef', 'hex') where artifact_id = $1`,
	} {
		if _, err := tx.Exec(ctx, statement, artifactID); err != nil {
			t.Fatalf("corrupt the document's history: %s: %v", statement, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the corruption: %v", err)
	}
}

func rebuildPath(artifactID string) string {
	return "/api/v1/artifacts/" + artifactID + "/rebuild"
}

// resumedBetween is every time in [from, to) at which either process's settlement resumption armed
// the document's owed settlement (the room-counters hook's settlement_resumptions), each named by
// its process.
func (p *dispatchPair) resumedBetween(t *testing.T, artifactID string, from, to time.Time) []string {
	t.Helper()
	var resumed []string
	for _, process := range []*dispatchProcess{p.a, p.b} {
		for _, at := range process.roomCounters(t, artifactID).SettlementResumptions {
			if !at.Before(from) && at.Before(to) {
				resumed = append(resumed, p.name(process)+" at "+at.Format(time.RFC3339Nano))
			}
		}
	}
	return resumed
}

// Spec item 6: a rebuild, the settlement resumption, the conditional-edit queue and a write to a
// document no room holds behave across processes as on one. Every process here has a ten-minute
// settle delay, so no settlement runs inside the row: an owed settlement stays owed for the
// resumption to find, and no settlement loads a room.
func TestRebuildResumptionQueueAndColdWritesHoldAcrossProcesses(t *testing.T) {
	pair := newPair(t)
	pair.startSettling(t, nil, 10*time.Minute, 10*time.Minute)
	project := uniqueProjectKey(t, "MAIN")
	pair.a.createProject(t, project, "Maintenance across processes")

	t.Run("a rebuild through A refuses a document live on B", func(t *testing.T) {
		_, artifactID := pair.b.createIssueIn(t, project, "Live on B, rebuilt through A", "before")
		pair.openStoredEditor(t, pair.b, "bob", artifactID, "before\n")
		corruptHistory(t, pair, artifactID)
		answer := pair.a.send(t, http.MethodPost, rebuildPath(artifactID), `{}`, asUser("alice"))
		if answer.status != http.StatusConflict || answer.code() != "DOCUMENT_LIVE" {
			t.Fatalf("a rebuild through A while bob's tab holds a room of the document on B answered %v; want 409 DOCUMENT_LIVE: A asked only its own rooms whether the document is live", answer)
		}
	})

	t.Run("two rebuilds at once through A and B: one runs and the other is refused", func(t *testing.T) {
		issueKey, artifactID := pair.a.createIssueIn(t, project, "Rebuilt twice", "before")
		if pair.a.holdsRoom(t, artifactID) || pair.b.holdsRoom(t, artifactID) {
			t.Fatalf("a process holds a room of the document, so a rebuild would be refused as live rather than meet the other rebuild")
		}
		corruptHistory(t, pair, artifactID)
		// Both rebuilds validate the history they loaded and then wait here, on the issue's row
		// (lockArtifactOwner), so each has loaded the corrupt history before either replaces it.
		gate := lockIssue(t, pair.database, issueKey)
		throughA := pair.a.requestInBackground(http.MethodPost, rebuildPath(artifactID), `{}`, asUser("alice"))
		throughB := pair.b.requestInBackground(http.MethodPost, rebuildPath(artifactID), `{}`, asUser("alice"))
		if waiting := storetest.WaitForLockWaiters(t, gate, 2, 30*time.Second); waiting < 2 {
			t.Fatalf("%d of the two rebuilds reached the issue's row within 30 s:\nA:\n%s\nB:\n%s", waiting, pair.a.Output.String(), pair.b.Output.String())
		}
		if err := gate.Rollback(context.Background()); err != nil {
			t.Fatalf("release the issue's row: %v", err)
		}
		answers := []httpAnswer{
			throughA.await(t, 60*time.Second, "the rebuild through A", pair.a),
			throughB.await(t, 60*time.Second, "the rebuild through B", pair.b),
		}
		succeeded := 0
		for _, answer := range answers {
			if answer.status == http.StatusOK {
				succeeded++
			}
		}
		if succeeded != 1 || (answers[0].status != http.StatusConflict && answers[1].status != http.StatusConflict) {
			t.Fatalf("two rebuilds of one document at once answered %v through A and %v through B; want one 200 and one 409: the second replaced the history the first had just rebuilt, judged on a load from before it", answers[0], answers[1])
		}
	})

	t.Run("an owed settlement is resumed by one process per period", func(t *testing.T) {
		_, artifactID := pair.a.createIssueIn(t, project, "Owed and resumed", "before")
		pair.a.call(t, http.MethodPost, editPath(artifactID), editBody(t, "before", "after", "", false), http.StatusOK)
		waitForSettlementOwed(t, pair.database, artifactID, true)
		if _, err := pair.database.Pool.Exec(context.Background(), `
			update doc_settlements_pending set marked_at = now() - interval '2 minutes' where artifact_id = $1
		`, artifactID); err != nil {
			t.Fatalf("age the document's pending settlement: %v", err)
		}
		// A period is a bucket of the clock (duty.Exclusive, migration 0115), and the row is owed
		// through the whole of the first bucket that starts after it aged: no settlement runs
		// inside the row. Each process's resumption ticks once a minute from its own start, so
		// each meets that bucket once; what this counts is how many of those meetings resumed it.
		period := docs.SettlementResumeInterval
		start := time.Now().Truncate(period).Add(period)
		end := start.Add(period)
		t.Logf("counting the resumptions of the document in the period from %s to %s", start.Format(time.TimeOnly), end.Format(time.TimeOnly))
		time.Sleep(time.Until(end) + 2*time.Second)
		resumed := pair.resumedBetween(t, artifactID, start, end)
		if len(resumed) != 1 {
			t.Fatalf("the processes' settlement resumptions armed the document's owed settlement %d times in one %v period, want once across both processes:\n%s",
				len(resumed), period, strings.Join(resumed, "\n"))
		}
	})

	t.Run("33 conditional edits across A and B: 32 queue and one answers 429 EDIT_QUEUE_FULL", func(t *testing.T) {
		_, artifactID := pair.a.createIssueIn(t, project, "Queued on both", "alpha")
		_, token := pair.a.textAndToken(t, artifactID)
		// The first edit parks after its first append, holding the document; every later
		// conditional edit of it waits behind it, on A in A's queue and on B in B's.
		pair.a.armLiveWritePause(t, artifactID, docs.PauseAfterFirstAppend)
		first := pair.a.requestInBackground(http.MethodPost, editPath(artifactID), editBody(t, "alpha", "beta", token, false), asUser("alice"))
		pair.a.awaitLiveWriteParked(t, artifactID, docs.PauseAfterFirstAppend)
		const queued = 32
		later := make([]backgroundRequest, queued)
		for index := range later {
			process := pair.a
			if index%2 == 1 {
				process = pair.b
			}
			later[index] = process.requestInBackground(http.MethodPost, editPath(artifactID), editBody(t, "alpha", "gamma", token, false), asUser("alice"))
		}
		answered := make([]*httpAnswer, queued)
		refused := 0
		deadline := time.Now().Add(15 * time.Second)
		for refused == 0 && time.Now().Before(deadline) {
			for index, request := range later {
				if answered[index] != nil {
					continue
				}
				select {
				case answer := <-request.answered:
					answered[index] = &answer
					if answer.status == http.StatusTooManyRequests {
						refused++
					}
				case err := <-request.failed:
					t.Fatalf("conditional edit %d: %v", index+2, err)
				default:
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		if refused == 0 {
			t.Errorf("33 conditional edits of one document were in flight at once - the first parked holding it, 16 queued behind it on A and 16 on B - and none answered 429 EDIT_QUEUE_FULL within 15 s: each process held its own queue to 32, so the two together queued 33")
		}
		if !pair.a.releaseLiveWritePause(t, artifactID, docs.PauseAfterFirstAppend) {
			t.Fatalf("the release found no edit parked")
		}
		first.await(t, 60*time.Second, "the first conditional edit", pair.a)
		refused = 0
		for index, request := range later {
			answer := answered[index]
			if answer == nil {
				awaited := request.await(t, 60*time.Second, fmt.Sprintf("conditional edit %d", index+2), pair.a)
				answer = &awaited
			}
			if answer.status == http.StatusTooManyRequests {
				if answer.code() != "EDIT_QUEUE_FULL" {
					t.Errorf("conditional edit %d answered %v, want the 429 to be EDIT_QUEUE_FULL", index+2, answer)
				}
				refused++
			}
		}
		if refused != 1 {
			t.Errorf("%d of the 33 conditional edits answered 429 EDIT_QUEUE_FULL, want exactly one: 32 may queue across both processes", refused)
		}
	})

	t.Run("an agent write through A, which holds no room, leaves none resident there", func(t *testing.T) {
		_, artifactID := pair.b.createIssueIn(t, project, "Written cold", "before")
		if pair.a.holdsRoom(t, artifactID) {
			t.Fatalf("A holds a room of the document before the write, so the write would not be cold")
		}
		if answer := pair.a.send(t, http.MethodPost, editPath(artifactID), editBody(t, "before", "after", "", true), asAgent()); answer.status != http.StatusOK {
			t.Fatalf("the agent's write through A: %v", answer)
		}
		if pair.a.holdsRoom(t, artifactID) {
			t.Fatalf("an agent's write through A, which held no room of the document, left a room of it resident on A with no reader in it: the write's publish loaded the room from the store (audit finding 11); A's rooms: %q", pair.a.residentRooms(t))
		}
	})
}

// Plan D10: the answer to a document socket's upgrade names the process that serves it
// (X-Dispatch-Process, the pod name in production), which is how a two-pod production check
// records where each of its sockets landed. Both processes run on this host, and the binary
// reads its name from the host (no setting names it otherwise), so each must answer this host's
// name.
func TestEachDocumentSocketNamesItsServingProcess(t *testing.T) {
	pair := startPair(t, nil)
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		t.Fatalf("read this host's name: %q %v", hostname, err)
	}
	project := uniqueProjectKey(t, "HOST")
	pair.a.createProject(t, project, "Serving processes")
	_, artifactID := pair.a.createIssueIn(t, project, "Served by both", "x")
	for _, process := range []*dispatchProcess{pair.a, pair.b} {
		editor := process.openEditorAs(t, "alice", artifactID, "x\n")
		switch served := servingProcess(editor); served {
		case hostname:
		case "":
			t.Errorf("the document socket's upgrade on %s carries no X-Dispatch-Process header; want the serving process's host name, %q. Upgrade header: %v",
				pair.name(process), hostname, editor.UpgradeHeader)
		default:
			t.Errorf("the document socket's upgrade on %s names %q as its serving process, want %q", pair.name(process), served, hostname)
		}
	}
}

// ───── deliveries a request or a process left behind ───────────────────────

// listenerRoute is one of the Envoy listener routes Dispatch calls, which a row can hold.
type listenerRoute string

const (
	listenerRegistry listenerRoute = "GET /v1/registry/{session_id}"
	listenerSessions listenerRoute = "GET /v1/sessions"
	listenerSend     listenerRoute = "POST /v1/messages/send"
)

// listenerSession is a live session's record as the listener answers it, on its registry read and
// on its live list alike.
type listenerSession struct {
	SessionID    string   `json:"session_id"`
	Title        string   `json:"title"`
	Roles        []string `json:"roles"`
	Capabilities []string `json:"capabilities"`
}

// listenerSendRecord is one send the fake listener answered: the key and the session it was sent
// under, its urgency, and whether the listener answered it as a repeat of a send it had already
// answered.
type listenerSendRecord struct {
	Key, Recipient, Urgency string
	Duplicate               bool
}

// heldRequest is one request a hold keeps unanswered: its route, what it names - the session id a
// registry read asks about, the idempotency key a send carries - and a channel closed once its
// caller has given up on it, a cancelled call or a killed process alike.
type heldRequest struct {
	route   listenerRoute
	subject string
	gone    <-chan struct{}
}

// listenerHold keeps every request on its route unanswered from when it is taken until Release,
// and reports each one on arrived as it comes in.
type listenerHold struct {
	arrived chan heldRequest
	release chan struct{}
	once    sync.Once
}

// Release answers every request the hold keeps and lets every later one through.
func (h *listenerHold) Release() { h.once.Do(func() { close(h.release) }) }

func (h *listenerHold) released() bool {
	select {
	case <-h.release:
		return true
	default:
		return false
	}
}

// next waits up to within for the next request the hold keeps, and fails the test, naming what was
// awaited, when none arrives.
func (h *listenerHold) next(t *testing.T, within time.Duration, what string) heldRequest {
	t.Helper()
	select {
	case held := <-h.arrived:
		return held
	case <-time.After(within):
		t.Fatalf("%s never reached the listener within %v", what, within)
		return heldRequest{}
	}
}

// requireAbandoned waits up to ten seconds for each held request's caller to have gone away, as
// every request a killed process made has, so a release that follows answers none of them: a
// send the listener answers to a dead process would be one the row did not mean to land.
func requireAbandoned(t *testing.T, held ...heldRequest) {
	t.Helper()
	for _, request := range held {
		select {
		case <-request.gone:
		case <-time.After(10 * time.Second):
			t.Fatalf("the caller of %s %s never went away", request.route, request.subject)
		}
	}
}

// fakeListener is the Envoy listener both processes of a pair deliver through (ENVOY_URL): an
// httptest server in the test process that knows a set of live sessions (a row may replace it,
// setSessions), serves the three routes a delivery calls (the registry read, the live list and
// the send), counts every live-list read it answers, and records every send it answers. A send
// whose key and recipient it has already answered is answered as a duplicate, as the real
// listener answers a repeat inside contracts.DeliveryDuplicateWindow, and recorded as one. A row
// holds a route (hold) to keep a request unanswered while it acts on the processes;
// a held request whose caller goes away is never answered and never recorded, so a send a killed
// process made is not one that reached the session. Every hold is released when the test ends,
// before the server closes, since a close waits for every request still in flight.
//
// Each route's answer is decided in serve, in one place per route. A row that needs the listener
// to misbehave for a while starts a take-over (takeOver): for its first phase the listener refuses
// every connection, for its second it answers 503 "service starting" on every route as a starting
// listener does, for its third the registry read is answered with the session's record while the
// send still meets that 503, and for its fourth it holds no session yet - the registry read and
// the send answer "no live session", the live list is empty - and after that it answers as before.
type fakeListener struct {
	server *httptest.Server

	mu       sync.Mutex
	sessions []listenerSession
	// sessionLists counts the GET /v1/sessions reads the listener answered.
	sessionLists int
	answered     []listenerSendRecord
	holds        map[listenerRoute]*listenerHold
	// takeOverFrom is when the take-over began, zero when there is none; takeOverPhases is how
	// long it refuses connections, answers 503 on every route, answers 503 on the send alone, and
	// holds no session, in that order.
	takeOverFrom   time.Time
	takeOverPhases [4]time.Duration
	// takeOverReads counts, by session id, the registry reads a take-over answered other than
	// as the session's record.
	takeOverReads map[string]int
	// gatedSends counts, by idempotency key, the sends a take-over answered 503 "service
	// starting".
	gatedSends map[string]int
}

// startFakeListener serves the sessions on loopback until the test ends.
func startFakeListener(t *testing.T, sessions ...listenerSession) *fakeListener {
	t.Helper()
	f := &fakeListener{
		sessions: sessions, holds: map[listenerRoute]*listenerHold{},
		takeOverReads: map[string]int{}, gatedSends: map[string]int{},
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(func() {
		f.mu.Lock()
		for _, hold := range f.holds {
			hold.Release()
		}
		f.mu.Unlock()
		f.server.Close()
	})
	return f
}

// processEnv is what a Dispatch process delivering through this listener is started with: the
// listener's URL. Nothing here shortens the delivery sweep's period: it runs at production's
// SuggestionSweepInterval (a minute), as every pass of the maintenance loop does.
func (f *fakeListener) processEnv() map[string]string {
	return map[string]string{"ENVOY_URL": f.server.URL}
}

// setSessions replaces the sessions the listener knows: from now on its registry read, its live
// list and its send answer as though exactly these were live.
func (f *fakeListener) setSessions(sessions ...listenerSession) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions = sessions
}

// sessionListings is how many GET /v1/sessions reads the listener has answered.
func (f *fakeListener) sessionListings() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessionLists
}

// hold keeps every request on route unanswered from now until the returned hold is released. A
// hold the route already had is released first, so nothing it keeps is left waiting for the end
// of the test.
func (f *fakeListener) hold(route listenerRoute) *listenerHold {
	hold := &listenerHold{arrived: make(chan heldRequest, 64), release: make(chan struct{})}
	f.mu.Lock()
	defer f.mu.Unlock()
	if previous := f.holds[route]; previous != nil {
		previous.Release()
	}
	f.holds[route] = hold
	return hold
}

// sendsUnder is every send the listener answered under key, in the order it answered them.
func (f *fakeListener) sendsUnder(key string) []listenerSendRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	var sends []listenerSendRecord
	for _, send := range f.answered {
		if send.Key == key {
			sends = append(sends, send)
		}
	}
	return sends
}

// listenerTakeOverPhase is what a take-over has the fake listener do with a request right now.
type listenerTakeOverPhase int

const (
	takeOverOver listenerTakeOverPhase = iota
	takeOverRefusing
	takeOverStarting
	takeOverHandingOver
	takeOverUnknown
)

// takeOver begins a take-over now: the listener refuses connections for refuse, then answers 503
// on every route for starting, then on the send alone for handingOver, then holds no session for
// unknown.
//
// The handing-over phase is a replacement in which both listeners take requests behind the one
// address: the registry read reaches the outgoing listener, which still holds the session, and
// the send reaches its successor, still behind its starting gate (cmd/listener/main.go
// startingGate). It is the one phase in which a send itself meets that gate, because every try
// reads the registry before it sends.
func (f *fakeListener) takeOver(refuse, starting, handingOver, unknown time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.takeOverFrom = time.Now()
	f.takeOverPhases = [4]time.Duration{refuse, starting, handingOver, unknown}
}

// endTakeOver ends the take-over at once, whatever phase it is in.
func (f *fakeListener) endTakeOver() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.takeOverFrom = time.Time{}
}

// takeOverPhase is the phase the take-over is in now.
func (f *fakeListener) takeOverPhase() listenerTakeOverPhase {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.takeOverFrom.IsZero() {
		return takeOverOver
	}
	elapsed := time.Since(f.takeOverFrom)
	for index, phase := range []listenerTakeOverPhase{takeOverRefusing, takeOverStarting, takeOverHandingOver, takeOverUnknown} {
		if elapsed -= f.takeOverPhases[index]; elapsed < 0 {
			return phase
		}
	}
	return takeOverOver
}

// readsDuringTakeOver is how many registry reads of sessionID a take-over answered other than as
// the session's record.
func (f *fakeListener) readsDuringTakeOver(sessionID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.takeOverReads[sessionID]
}

// sendsGatedUnder is how many sends under key a take-over answered 503 "service starting".
func (f *fakeListener) sendsGatedUnder(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gatedSends[key]
}

// answerStarting answers as the real listener's starting gate does (cmd/listener/main.go
// startingGate).
func answerStarting(w http.ResponseWriter) {
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "service starting"})
}

func (f *fakeListener) serve(w http.ResponseWriter, r *http.Request) {
	// The whole body is read before anything waits, so the server sees the request end and
	// notices its caller going away while the request is held.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	phase := f.takeOverPhase()
	if phase != takeOverOver && phase != takeOverHandingOver &&
		r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/") {
		f.mu.Lock()
		f.takeOverReads[strings.TrimPrefix(r.URL.Path, "/v1/registry/")]++
		f.mu.Unlock()
	}
	switch phase {
	case takeOverRefusing:
		// The connection is dropped unanswered, which the client meets as a listener that is not
		// there: what a deleted listener pod, or its successor not yet listening, gives.
		if connection, _, err := http.NewResponseController(w).Hijack(); err == nil {
			_ = connection.Close()
		}
		return
	case takeOverStarting:
		answerStarting(w)
		return
	case takeOverHandingOver:
		if r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send" {
			var send struct {
				IdempotencyKey string `json:"idempotency_key"`
			}
			_ = json.Unmarshal(body, &send)
			f.mu.Lock()
			f.gatedSends[send.IdempotencyKey]++
			f.mu.Unlock()
			answerStarting(w)
			return
		}
	}
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/"):
		sessionID := strings.TrimPrefix(r.URL.Path, "/v1/registry/")
		if !f.await(listenerRegistry, sessionID, r) {
			return
		}
		session, ok := f.session(sessionID)
		if !ok || phase == takeOverUnknown {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "no live session " + sessionID})
			return
		}
		_ = json.NewEncoder(w).Encode(session)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
		if !f.await(listenerSessions, "", r) {
			return
		}
		f.mu.Lock()
		f.sessionLists++
		sessions := f.sessions
		f.mu.Unlock()
		if phase == takeOverUnknown || sessions == nil {
			sessions = []listenerSession{}
		}
		_ = json.NewEncoder(w).Encode(sessions)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
		var send struct {
			TargetSession  string `json:"target_session"`
			IdempotencyKey string `json:"idempotency_key"`
			Urgency        string `json:"urgency"`
		}
		if err := json.Unmarshal(body, &send); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if !f.await(listenerSend, send.IdempotencyKey, r) {
			return
		}
		if _, ok := f.session(send.TargetSession); !ok || phase == takeOverUnknown {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "no live session " + send.TargetSession})
			return
		}
		f.mu.Lock()
		duplicate := false
		for _, prior := range f.answered {
			duplicate = duplicate || (prior.Key == send.IdempotencyKey && prior.Recipient == send.TargetSession)
		}
		f.answered = append(f.answered, listenerSendRecord{
			Key: send.IdempotencyKey, Recipient: send.TargetSession, Urgency: send.Urgency, Duplicate: duplicate,
		})
		envelope := fmt.Sprintf("envelope-%d", len(f.answered))
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"event_id": envelope, "recipient": send.TargetSession, "duplicate": duplicate,
		})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// await keeps r unanswered while route is held, and reports whether it is still to be answered:
// false when its caller went away first.
func (f *fakeListener) await(route listenerRoute, subject string, r *http.Request) bool {
	f.mu.Lock()
	hold := f.holds[route]
	f.mu.Unlock()
	if hold == nil || hold.released() {
		return true
	}
	hold.arrived <- heldRequest{route: route, subject: subject, gone: r.Context().Done()}
	select {
	case <-hold.release:
		return r.Context().Err() == nil
	case <-r.Context().Done():
		return false
	}
}

func (f *fakeListener) session(sessionID string) (listenerSession, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, session := range f.sessions {
		if session.SessionID == sessionID {
			return session, true
		}
	}
	return listenerSession{}, false
}

// resumeSession is the one live session the delivery rows send to; it takes every mode.
var resumeSession = listenerSession{
	SessionID: "resume-session", Title: "resume", Roles: []string{}, Capabilities: []string{"btw", "aside", "steer"},
}

// deliveryUnderTest is one delivery a row follows: what a failure calls it, the attempt table its
// rows are in, and the idempotency key its send carries, which begins with the id of its message
// or comment.
type deliveryUnderTest struct {
	what, table, key string
}

func messageDelivery(what, key string) deliveryUnderTest {
	return deliveryUnderTest{what: what, table: "message_deliveries", key: key}
}

func mentionDelivery(what, key string) deliveryUnderTest {
	return deliveryUnderTest{what: what, table: "comment_deliveries", key: key}
}

// owner is the column naming the message or comment, the receipt event type and the payload
// field naming it there, and its id.
func (d deliveryUnderTest) owner() (column, receipt, field, id string) {
	id, _, _ = strings.Cut(d.key, ":")
	if d.table == "message_deliveries" {
		return "message_id", "message.delivery", "message_id", id
	}
	return "comment_id", "comment.delivery", "comment_id", id
}

// attempts is every attempt of the delivery's message or comment, as "<attempt>:<state>".
func (d deliveryUnderTest) attempts(t *testing.T, database *store.Store) []string {
	t.Helper()
	column, _, _, id := d.owner()
	rows, err := database.Pool.Query(context.Background(),
		`select attempt::text || ':' || state from `+d.table+` where `+column+` = $1 order by attempt`, id)
	if err != nil {
		t.Fatalf("read the attempts of %s: %v", d.what, err)
	}
	states, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the attempts of %s: %v", d.what, err)
	}
	return states
}

// receipts is how many delivery receipts the events hold for attempt 1 of the delivery.
func (d deliveryUnderTest) receipts(t *testing.T, database *store.Store) int {
	t.Helper()
	_, receipt, field, id := d.owner()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from events
		where type = $1 and payload ->> $2 = $3 and (payload ->> 'attempt')::int = 1
	`, receipt, field, id).Scan(&count); err != nil {
		t.Fatalf("count the receipts of %s: %v", d.what, err)
	}
	return count
}

// requireDeliveredOnce waits up to within for every delivery to settle and requires each to have
// reached its session exactly once under its own key: attempt 1 alone, sent, with one receipt,
// and one send the listener answered as new. A delivery still unsettled at the deadline is
// reported, and so is every one after it, with the processes' output once.
func requireDeliveredOnce(t *testing.T, pair *dispatchPair, listener *fakeListener, within time.Duration, deliveries ...deliveryUnderTest) {
	t.Helper()
	deadline := time.Now().Add(within)
	unsettled := false
	for _, delivery := range deliveries {
		states := delivery.attempts(t, pair.database)
		for len(states) == 0 || slices.ContainsFunc(states, func(state string) bool {
			return strings.HasSuffix(state, ":pending")
		}) {
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(100 * time.Millisecond)
			states = delivery.attempts(t, pair.database)
		}
		if !slices.Equal(states, []string{"1:sent"}) {
			t.Errorf("%s reads %v %v after the row began waiting; want attempt 1 alone, sent", delivery.what, states, within)
			unsettled = true
			continue
		}
		if sends := listener.sendsUnder(delivery.key); len(sends) != 1 || sends[0].Duplicate || sends[0].Recipient != resumeSession.SessionID {
			t.Errorf("%s: the listener answered %+v under %s; want one new send to %s",
				delivery.what, sends, delivery.key, resumeSession.SessionID)
		}
		if receipts := delivery.receipts(t, pair.database); receipts != 1 {
			t.Errorf("%s: attempt 1 has %d delivery receipts, want one", delivery.what, receipts)
		}
	}
	if unsettled {
		t.Fatalf("the processes' output:\nA:\n%s\nB:\n%s", pair.a.Output.String(), pair.b.Output.String())
	}
}

// sendInBackground sends body to path on p without waiting for the answer, for a request the row
// expects never to answer: the process is killed while it is held.
func (p *dispatchProcess) sendInBackground(method, path, body string) {
	go func() { _, _ = p.trySend(method, path, body, asUser(dispatchTestLogin)) }()
}

// backDatePendingAttempts moves every pending attempt's claim and creation two minutes back, past
// both ages the delivery sweep judges an attempt owed by (api/delivery_sweep.go,
// owedMessageAttempt): the one-minute claim lease, and the minute a row must have been open for,
// so a row does not wait real minutes for its attempts to be owed.
func backDatePendingAttempts(t *testing.T, database *store.Store) {
	t.Helper()
	for _, table := range []string{"message_deliveries", "comment_deliveries"} {
		if _, err := database.Pool.Exec(context.Background(), `
			update `+table+`
			set claimed_at = claimed_at - interval '2 minutes', created_at = created_at - interval '2 minutes'
			where state = 'pending'
		`); err != nil {
			t.Fatalf("back-date the pending attempts of %s: %v", table, err)
		}
	}
}

// sweepWait is how long a row waits for the surviving process's delivery sweep to send what a
// killed process left owed. The sweep is a pass of the maintenance loop
// (api.RunSuggestionOutcomeSweep), which these processes run at production's
// api.SuggestionSweepInterval (a minute) with no test override: attempts back-dated just after a
// tick wait that tick out, a minute, and when the killed process had already served that tick's
// period - one pass per period across the processes, duty.Exclusive - the survivor's next tick
// serves instead, a second minute. Half a minute on top covers the pass itself: its suggestion
// and request-key sweeps, and the sends, each bounded by the listener client's five-second
// window.
const sweepWait = 150 * time.Second

// A process killed while it delivers leaves its attempts pending: a direct message and a comment
// mention whose own request was sending them, and a broadcast recipient a background worker was
// sending. The other process's delivery sweep sends each one once its claim has lapsed, under the
// attempt and the idempotency key the killed process used, so the session gets each once. The
// subtest kills the process earlier, after the message is stored and before anything is resolved
// or claimed: the attempt the storing transaction wrote is what the survivor sends.
func TestADeliveryAKilledProcessLeftPendingIsSentByTheOther(t *testing.T) {
	t.Run("killed while sending", func(t *testing.T) {
		listener := startFakeListener(t, resumeSession)
		sends := listener.hold(listenerSend)
		pair := startPair(t, listener.processEnv())
		project := uniqueProjectKey(t, "RSM")
		pair.a.createProject(t, project, "Resumed deliveries")
		issue, _ := pair.a.createIssueIn(t, project, "Mentioned while sending", "spec")

		pair.a.sendInBackground(http.MethodPost, "/api/v1/agents/"+resumeSession.SessionID+"/messages",
			`{"body":"A direct message","delivery":"steer"}`)
		direct := sends.next(t, 30*time.Second, "the direct message's send")
		pair.a.sendInBackground(http.MethodPost, "/api/v1/issues/"+issue+"/comments",
			fmt.Sprintf(`{"body":"A mention","mentions":[{"target":"session:%s"}]}`, resumeSession.SessionID))
		mention := sends.next(t, 30*time.Second, "the mention's send")
		pair.a.call(t, http.MethodPost, "/api/v1/broadcasts", fmt.Sprintf(
			`{"body":"A broadcast","delivery":"steer","session_ids":[%q]}`,
			resumeSession.SessionID), http.StatusCreated)
		broadcast := sends.next(t, 30*time.Second, "the broadcast recipient's send")

		pair.a.kill(t)
		requireAbandoned(t, direct, mention, broadcast)
		sends.Release()
		backDatePendingAttempts(t, pair.database)
		requireDeliveredOnce(t, pair, listener, sweepWait,
			messageDelivery("the direct message", direct.subject),
			mentionDelivery("the mention", mention.subject),
			messageDelivery("the broadcast recipient's message", broadcast.subject),
		)
	})

	t.Run("killed before the claim", func(t *testing.T) {
		listener := startFakeListener(t, resumeSession)
		lookups := listener.hold(listenerRegistry)
		pair := startPair(t, listener.processEnv())
		pair.a.sendInBackground(http.MethodPost, "/api/v1/agents/"+resumeSession.SessionID+"/messages",
			`{"body":"Stored, then killed","delivery":"steer"}`)
		lookup := lookups.next(t, 30*time.Second, "the direct message's registry read")
		pair.a.kill(t)
		requireAbandoned(t, lookup)
		lookups.Release()
		var messageID string
		if err := pair.database.Pool.QueryRow(context.Background(),
			`select id::text from messages where target = $1`, "session:"+resumeSession.SessionID,
		).Scan(&messageID); err != nil {
			t.Fatalf("read the stored message: %v", err)
		}
		backDatePendingAttempts(t, pair.database)
		requireDeliveredOnce(t, pair, listener, sweepWait,
			messageDelivery("the message stored before the kill", messageID+":steer"))
	})
}

// cancelGrace is how long cancelOnceHeld gives Dispatch to act on a cancelled request before the
// listener answers the send that request made, when Dispatch does not abandon that send itself:
// well inside the listener client's five-second window, so the answer is still awaited.
const cancelGrace = 2 * time.Second

// cancelOnceHeld sends body to path on p with a request the test cancels as soon as the listener
// holds the send it made, which is after its commit, and waits for the client to return. The send
// is then answered once Dispatch has had the cancellation: at once if Dispatch gave up on the send
// with its request, after cancelGrace if it did not. It answers the send's idempotency key.
func cancelOnceHeld(t *testing.T, p *dispatchProcess, listener *fakeListener, path, body string) string {
	t.Helper()
	sends := listener.hold(listenerSend)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url(path), strings.NewReader(body))
	if err != nil {
		t.Fatalf("build POST %s: %v", path, err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", dispatchTestLogin)
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		if response, err := http.DefaultClient.Do(request); err == nil {
			_ = response.Body.Close()
		}
	}()
	held := sends.next(t, 30*time.Second, "the send of POST "+path)
	cancel()
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatalf("POST %s did not return within 10s of its cancellation", path)
	}
	select {
	case <-held.gone:
	case <-time.After(cancelGrace):
	}
	sends.Release()
	return held.subject
}

// A request cut off after its commit - a closed tab, a front door's pod roll - must not take the
// send it was making with it: the direct message and the mention each reach their session once
// and read sent, with no Retry. No attempt here is a minute old, and nothing back-dates one, so
// the delivery sweep owes none of them: only the request's own send can deliver them.
func TestASendWhoseRequestIsCancelledAfterItsCommitReachesItsRecipient(t *testing.T) {
	listener := startFakeListener(t, resumeSession)
	pair := startPair(t, listener.processEnv())
	project := uniqueProjectKey(t, "CNL")
	pair.a.createProject(t, project, "Cancelled requests")
	issue, _ := pair.a.createIssueIn(t, project, "Mentioned, then cut off", "spec")

	direct := cancelOnceHeld(t, pair.a, listener, "/api/v1/agents/"+resumeSession.SessionID+"/messages",
		`{"body":"Cut off after its commit","delivery":"steer"}`)
	mention := cancelOnceHeld(t, pair.a, listener, "/api/v1/issues/"+issue+"/comments",
		fmt.Sprintf(`{"body":"A mention cut off after its commit","mentions":[{"target":"session:%s"}]}`, resumeSession.SessionID))
	requireDeliveredOnce(t, pair, listener, 15*time.Second,
		messageDelivery("the direct message", direct),
		mentionDelivery("the mention", mention),
	)
}

// takeOverDeliveries waits up to ten seconds for the direct message and the mentioning comment a
// row sent, by their bodies, to be stored, and answers each as the delivery it follows.
func takeOverDeliveries(t *testing.T, database *store.Store, messageBody, commentBody string) (direct, mention deliveryUnderTest) {
	t.Helper()
	storedID := func(table, body string) string {
		deadline := time.Now().Add(10 * time.Second)
		for {
			var id string
			err := database.Pool.QueryRow(context.Background(),
				`select id::text from `+table+` where body = $1`, body).Scan(&id)
			if err == nil {
				return id
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("read the stored %s %q: %v", table, body, err)
			}
			if time.Now().After(deadline) {
				t.Fatalf("%q was not stored in %s within 10s", body, table)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	direct = messageDelivery("the direct message", storedID("messages", messageBody)+":steer")
	mention = mentionDelivery("the mention",
		storedID("comments", commentBody)+":session:"+resumeSession.SessionID+":steer")
	return direct, mention
}

// sendDuringTakeOver sends, on p, a direct message to the live session and a comment mentioning
// it, each without waiting for its answer, and returns the two deliveries once both are stored.
func sendDuringTakeOver(t *testing.T, p *dispatchProcess, database *store.Store, issue, what string) (direct, mention deliveryUnderTest) {
	t.Helper()
	messageBody, commentBody := "A direct message "+what, "A mention "+what
	p.sendInBackground(http.MethodPost, "/api/v1/agents/"+resumeSession.SessionID+"/messages",
		fmt.Sprintf(`{"body":%q,"delivery":"steer"}`, messageBody))
	p.sendInBackground(http.MethodPost, "/api/v1/issues/"+issue+"/comments",
		fmt.Sprintf(`{"body":%q,"mentions":[{"target":"session:%s"}]}`, commentBody, resumeSession.SessionID))
	return takeOverDeliveries(t, database, messageBody, commentBody)
}

// requireWaitingOutTheTakeOver waits up to ten seconds for both deliveries' senders to be waiting
// out the take-over: each attempt is still pending under a claim, and the session has been read
// at least five times during the take-over - the two first resolutions and at least three tries
// after them.
func requireWaitingOutTheTakeOver(t *testing.T, database *store.Store, listener *fakeListener, deliveries ...deliveryUnderTest) {
	t.Helper()
	claimed := func(delivery deliveryUnderTest) bool {
		column, _, _, id := delivery.owner()
		var held bool
		if err := database.Pool.QueryRow(context.Background(), `
			select count(*) = 1 and bool_and(state = 'pending' and claimed_at is not null)
			from `+delivery.table+` where `+column+` = $1
		`, id).Scan(&held); err != nil {
			t.Fatalf("read the attempts of %s: %v", delivery.what, err)
		}
		return held
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		waiting := listener.readsDuringTakeOver(resumeSession.SessionID) >= 5
		for _, delivery := range deliveries {
			waiting = waiting && claimed(delivery)
		}
		if waiting {
			return
		}
		if time.Now().After(deadline) {
			for _, delivery := range deliveries {
				t.Errorf("%s reads %v", delivery.what, delivery.attempts(t, database))
			}
			t.Fatalf("the senders were not waiting out the take-over within 10s: %d reads of the session during it, "+
				"want at least 5 with every attempt pending under its claim", listener.readsDuringTakeOver(resumeSession.SessionID))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// While the active listener is replaced, its successor refuses connections, then answers 503
// "service starting" - on every route, then on the send alone while the outgoing listener still
// answers the registry read - then does not yet hold the session, for a few seconds. A direct
// message and a mention sent then wait it out under the attempt each holds and reach the session
// once it answers: attempt 1 sent, one receipt, one send. A session the listener never knows still
// reads failed, "no live session", once the take-over window has passed. And a process killed
// while it waits leaves its attempts pending under the claim it renewed last, which the other
// process's delivery sweep sends.
func TestASendDuringAListenerTakeOverReachesItsSession(t *testing.T) {
	t.Run("sent during the take-over", func(t *testing.T) {
		listener := startFakeListener(t, resumeSession)
		// No attempt here is back-dated, so the delivery sweep owes none and only each request's
		// own sender can deliver.
		pair := startPair(t, listener.processEnv())
		project := uniqueProjectKey(t, "TKO")
		pair.a.createProject(t, project, "Listener take-over")
		issue, _ := pair.a.createIssueIn(t, project, "Mentioned during a take-over", "spec")

		// A sender tries at once, then 0.25, 0.75, 1.75, 3.75, 5.75 and 7.75 s after its first try
		// (takeoverRetry): from 1.75 s on its tries are two seconds apart plus each try's own time.
		// The send-only 503 lasts 2.5 s, longer than that spacing, so at least one try of each
		// sender meets it however late a loaded machine starts the tries; the take-over ends at
		// 6 s, so the try that lands comes before the row's 10 s bound.
		listener.takeOver(time.Second, time.Second, 2500*time.Millisecond, 1500*time.Millisecond)
		direct, mention := sendDuringTakeOver(t, pair.a, pair.database, issue, "sent during a take-over")
		requireDeliveredOnce(t, pair, listener, 10*time.Second, direct, mention)
		for _, delivery := range []deliveryUnderTest{direct, mention} {
			if gated := listener.sendsGatedUnder(delivery.key); gated == 0 {
				t.Errorf("%s: no send under %s met the starting gate; the row must cover a send answered 503 "+
					"\"service starting\"", delivery.what, delivery.key)
			}
		}
	})

	t.Run("sent to a session the listener never knows", func(t *testing.T) {
		listener := startFakeListener(t, resumeSession)
		pair := startPair(t, listener.processEnv())
		started := time.Now()
		var message model.Message
		if err := json.Unmarshal(pair.a.call(t, http.MethodPost, "/api/v1/agents/never-known/messages",
			`{"body":"To nobody","delivery":"steer"}`, http.StatusCreated), &message); err != nil {
			t.Fatalf("decode the message: %v", err)
		}
		took := time.Since(started)
		if len(message.Deliveries) != 1 || message.Deliveries[0].State != "failed" ||
			message.Deliveries[0].Error == nil || *message.Deliveries[0].Error != "no live session never-known" {
			t.Fatalf("deliveries = %#v, want attempt 1 failed with %q", message.Deliveries, "no live session never-known")
		}
		if took > 15*time.Second {
			t.Fatalf("the send to a session the listener never knows answered after %v, want within 15s", took)
		}
		gone := messageDelivery("the message to nobody", message.ID+":steer")
		if states := gone.attempts(t, pair.database); !slices.Equal(states, []string{"1:failed"}) {
			t.Fatalf("%s reads %v, want attempt 1 alone, failed", gone.what, states)
		}
		if receipts := gone.receipts(t, pair.database); receipts != 1 {
			t.Fatalf("%s: attempt 1 has %d delivery receipts, want one", gone.what, receipts)
		}
	})

	t.Run("killed during the wait", func(t *testing.T) {
		listener := startFakeListener(t, resumeSession)
		pair := startPair(t, listener.processEnv())
		project := uniqueProjectKey(t, "TKK")
		pair.a.createProject(t, project, "Killed during a take-over")
		issue, _ := pair.a.createIssueIn(t, project, "Mentioned, then killed", "spec")

		listener.takeOver(time.Hour, 0, 0, 0)
		direct, mention := sendDuringTakeOver(t, pair.a, pair.database, issue, "killed during a take-over")
		requireWaitingOutTheTakeOver(t, pair.database, listener, direct, mention)
		pair.a.kill(t)
		listener.endTakeOver()
		backDatePendingAttempts(t, pair.database)
		requireDeliveredOnce(t, pair, listener, sweepWait, direct, mention)
	})
}

// ───── webhook redelivery ───────────────────────────────────────────────────

// heldRedeliveryProxy stands between the redelivery command and the GitHub webhook fake. It
// forwards every call but the first redelivery request, which it holds and never forwards: held is
// closed when that request arrives, and the hold ends when its caller goes away (a killed process)
// or after githubHoldCap, answered 502 so a process that outlived it records a refusal.
type heldRedeliveryProxy struct {
	server *httptest.Server
	held   chan struct{}
	once   sync.Once
}

func startHeldRedeliveryProxy(t *testing.T, upstream string) *heldRedeliveryProxy {
	t.Helper()
	target, err := url.Parse(upstream)
	if err != nil {
		t.Fatalf("parse the GitHub fake's URL: %v", err)
	}
	forward := httputil.NewSingleHostReverseProxy(target)
	proxy := &heldRedeliveryProxy{held: make(chan struct{})}
	proxy.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/attempts") {
			first := false
			proxy.once.Do(func() {
				first = true
				close(proxy.held)
			})
			if first {
				select {
				case <-r.Context().Done():
				case <-time.After(githubHoldCap):
					http.Error(w, `{"message":"held by the test"}`, http.StatusBadGateway)
				}
				return
			}
		}
		forward.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.server.Close)
	return proxy
}

// startRedeliverWebhooks starts `envoy-dispatch redeliver-webhooks --since 1h` with env, in a home
// of its own and nothing from the caller's environment.
func startRedeliverWebhooks(t *testing.T, binary string, env map[string]string) *cmdtest.Process {
	t.Helper()
	home := t.TempDir()
	cmd := exec.Command(binary, "redeliver-webhooks", "--since", "1h")
	cmd.Dir = home
	environment := map[string]string{"HOME": home, "XDG_CONFIG_HOME": home, "XDG_DATA_HOME": home}
	maps.Copy(environment, env)
	for name, value := range environment {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	slices.Sort(cmd.Env)
	return cmdtest.Start(t, "redeliver-webhooks", cmd)
}

// backDateRedeliveryClaim moves the claimed_at of guid's record in the redelivery KV bucket back
// by, through the test's own NATS client, so a row does not wait out a claim's deadline.
func backDateRedeliveryClaim(t *testing.T, natsURL, guid string, by time.Duration) {
	t.Helper()
	nc, err := natsgo.Connect(natsURL)
	if err != nil {
		t.Fatalf("connect NATS: %v", err)
	}
	defer nc.Close()
	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	// One copy, as the single test NATS server holds and the runs under test open.
	state, err := redeliver.OpenState(js, 1)
	if err != nil {
		t.Fatalf("open the redelivery bucket: %v", err)
	}
	key := "guid." + guid
	entry, err := state.Get(key)
	if err != nil {
		t.Fatalf("read the redelivery record of %s: %v", guid, err)
	}
	var stored map[string]any
	if err := json.Unmarshal(entry.Value(), &stored); err != nil {
		t.Fatalf("decode the redelivery record of %s: %v", guid, err)
	}
	stored["claimed_at"] = time.Now().Add(-by).UTC().Format(time.RFC3339Nano)
	data, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.Update(key, data, entry.Revision()); err != nil {
		t.Fatalf("back-date the redelivery claim of %s: %v", guid, err)
	}
}

// A webhook redelivery whose process was killed between claiming the delivery and recording
// GitHub's answer is requested again by the next sweep once the claim's deadline (three GitHub
// request timeouts, a minute) has passed. Two `envoy-dispatch redeliver-webhooks --since 1h` runs
// share one NATS, one database (the GitHub rate limits every process obeys) and one GitHub; the
// first is SIGKILLed while its redelivery request is held on the way to GitHub, the test moves the
// claim's time back past the deadline rather than waiting it out, and the second run asks GitHub
// for the delivery, once.
func TestAWebhookRedeliveryWhoseProcessDiedAfterItsClaimIsRequestedAgain(t *testing.T) {
	natsURL := multiprocessNATS(t)
	key, pemText := githubapptest.Key(t)
	const clientID = "Iv1.multiprocess"
	webhook := githubapptest.NewWebhook(t, &key.PublicKey, clientID, nil)
	failed := webhook.Record(githubapptest.Attempt{GUID: "guid-orphaned-claim", Event: "push", Payload: []byte(`{}`)},
		"", 7, http.StatusServiceUnavailable, time.Now().Add(-time.Minute))
	proxy := startHeldRedeliveryProxy(t, webhook.URL())
	env := githubAppEnv(t, proxy.server.URL)
	env["DISPATCH_APP_CLIENT_ID"] = clientID
	env["DISPATCH_APP_PEM_B64"] = base64.StdEncoding.EncodeToString([]byte(pemText))
	env["NATS_URLS"] = natsURL
	env["DATABASE_URL"] = databaseURLOf(t, storetest.Open(t))
	binary := cmdtest.Build(t, "envoy-dispatch")

	first := startRedeliverWebhooks(t, binary, env)
	select {
	case <-proxy.held:
	case <-first.Exited:
		t.Fatalf("the first run exited before it asked GitHub for the delivery:\n%s", first.Output.String())
	case <-time.After(60 * time.Second):
		t.Fatalf("the first run asked GitHub for no delivery within a minute:\n%s", first.Output.String())
	}
	if err := first.Cmd.Process.Kill(); err != nil {
		t.Fatalf("kill the first run: %v", err)
	}
	<-first.Exited
	if got := webhook.Requests(); len(got) != 0 {
		t.Fatalf("the killed run's held request reached GitHub: %v", got)
	}

	backDateRedeliveryClaim(t, natsURL, failed.GUID, 2*3*githubapp.RequestTimeout)
	second := startRedeliverWebhooks(t, binary, env)
	second.WaitExit(t, "it started")
	if code := second.Cmd.ProcessState.ExitCode(); code != 0 {
		t.Fatalf("the second run exited %d:\n%s", code, second.Output.String())
	}
	if got, want := webhook.Requests(), []int64{failed.ID}; !slices.Equal(got, want) {
		t.Fatalf("GitHub got redelivery requests %v, want %v once:\n%s", got, want, second.Output.String())
	}
	if !strings.Contains(second.Output.String(), " redelivered=1") {
		t.Fatalf("the second run does not report the delivery redelivered:\n%s", second.Output.String())
	}
}

// ───── one GitHub rate limit across the processes ──────────────────────────

// rateLimitGitHub is the GitHub both processes' proxy reads go to: acme/web, the installation
// covering it (77), a token mint answering rateLimitToken, and every read under that token, which
// it records. A read of rateLimitedRead answers as GitHub answers an exhausted rate limit: 403,
// X-RateLimit-Remaining: 0 and Retry-After. Every other App call answers an empty list, as
// githubFake's do.
type rateLimitGitHub struct {
	server *httptest.Server

	mu         sync.Mutex
	tokenReads []string
}

const (
	rateLimitToken  = "ghs_rate_limit_77"
	rateLimitedRead = "/repos/acme/web/pulls/1"
)

func startRateLimitGitHub(t *testing.T) *rateLimitGitHub {
	t.Helper()
	fake := &rateLimitGitHub{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":77,"app_slug":"multiprocess","permissions":{"pull_requests":"read"}}`)
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"token":%q,"expires_at":%q}`, rateLimitToken, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+rateLimitToken {
			_, _ = io.WriteString(w, "[]")
			return
		}
		fake.mu.Lock()
		fake.tokenReads = append(fake.tokenReads, r.Method+" "+r.URL.RequestURI())
		fake.mu.Unlock()
		if r.URL.Path == rateLimitedRead {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"message":"API rate limit exceeded for installation ID 77."}`)
			return
		}
		_, _ = io.WriteString(w, `{"state":"open","title":"Ship it"}`)
	})
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

// reads is every request GitHub has been sent under the installation's token, by either process.
func (f *rateLimitGitHub) reads() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.tokenReads)
}

// proxyRead sends GET path to p's GitHub proxy as the test's login and returns the status, the
// Retry-After header and the error code of the answer.
func (p *dispatchProcess) proxyRead(t *testing.T, path string) (status int, retryAfter, code string) {
	t.Helper()
	answered, err := p.trySend(http.MethodGet, path, "", asUser(dispatchTestLogin))
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	var body struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(answered.body, &body)
	return answered.status, answered.header.Get("Retry-After"), body.Code
}

// GitHub budgets an installation, not a process, so a rate limit one process meets holds every
// process. A meets an exhausted limit reading acme/web as its installation; B's read of another
// path under the same installation answers 503 GITHUB_RATE_LIMITED with a Retry-After, and GitHub
// is sent nothing more under the installation's token. B's installation lookup and token mint are
// the App's calls, under the App's own limit, and may still reach GitHub.
func TestARateLimitOneProcessMetStopsTheOtherFromCallingGitHub(t *testing.T) {
	github := startRateLimitGitHub(t)
	pair := startPair(t, githubAppEnv(t, github.server.URL))

	statusA, retryA, codeA := pair.a.proxyRead(t, "/api/github/rest"+rateLimitedRead)
	if reads := github.reads(); len(reads) != 1 {
		t.Fatalf("GitHub got %v under the installation's token, want A's one read", reads)
	}
	statusB, retryB, codeB := pair.b.proxyRead(t, "/api/github/rest/repos/acme/web/issues/2")
	if reads := github.reads(); len(reads) != 1 {
		t.Errorf("GitHub got %v under the installation's token: B asked it inside the limit A met", reads)
	}
	if statusB != http.StatusServiceUnavailable || codeB != "GITHUB_RATE_LIMITED" {
		t.Errorf("B's read inside the limit: status %d code %q, want 503 GITHUB_RATE_LIMITED", statusB, codeB)
	}
	if seconds, err := strconv.Atoi(retryB); err != nil || seconds < 1 || seconds > 60 {
		t.Errorf("B's Retry-After %q, want the seconds left of GitHub's 60", retryB)
	}
	if statusA != http.StatusServiceUnavailable || codeA != "GITHUB_RATE_LIMITED" || retryA != "60" {
		t.Errorf("A's limited read: status %d code %q Retry-After %q, want 503 GITHUB_RATE_LIMITED after 60", statusA, codeA, retryA)
	}
}

// ───── the database's clock ────────────────────────────────────────────────

// Each process's host keeps a clock of its own, so a time Dispatch stores is the database's: the
// writing transaction's now(), which every column that transaction stamps shares to the
// microsecond. A claim made on A answers a claim.at equal to the updated_at its own update
// stamped, and its issue.claimed event was created at that time; an answer given on B to an ask
// opened on A carries the `at` its ask.answered event was created at. No process's clock can
// produce either equality.
func TestClaimAndAnswerTimesComeFromTheDatabaseOnEitherProcess(t *testing.T) {
	pair := startPair(t, nil)
	ctx := context.Background()
	project := uniqueProjectKey(t, "CLK")
	pair.a.createProject(t, project, "Database clock")
	key, _ := pair.a.createIssueIn(t, project, "Claimed on A, answered on B", "spec")

	claimed := pair.a.call(t, http.MethodPost, "/api/v1/issues/"+key+"/claim", `{}`, http.StatusOK)
	var issue struct {
		UpdatedAt time.Time `json:"updated_at"`
		Claim     *struct {
			At time.Time `json:"at"`
		} `json:"claim"`
	}
	if err := json.Unmarshal(claimed, &issue); err != nil || issue.Claim == nil {
		t.Fatalf("decode the claim on A: %v: %s", err, claimed)
	}
	if !issue.Claim.At.Equal(issue.UpdatedAt) {
		t.Errorf("A's claim: claim.at %s, updated_at %s, want the one transaction time",
			issue.Claim.At.Format(time.RFC3339Nano), issue.UpdatedAt.Format(time.RFC3339Nano))
	}
	var claimedEvent time.Time
	if err := pair.database.Pool.QueryRow(ctx, `select created_at from events where issue_key = $1 and type = 'issue.claimed'`, key).Scan(&claimedEvent); err != nil {
		t.Fatalf("read the issue.claimed event: %v", err)
	}
	if !claimedEvent.Equal(issue.Claim.At) {
		t.Errorf("A's claim: claim.at %s, issue.claimed created at %s, want the one transaction time",
			issue.Claim.At.Format(time.RFC3339Nano), claimedEvent.Format(time.RFC3339Nano))
	}

	opened := pair.a.call(t, http.MethodPost, "/api/v1/issues/"+key+"/asks",
		`{"question":"Ship it?","options":[{"label":"Ship"},{"label":"Hold"}]}`, http.StatusCreated)
	var ask struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(opened, &ask); err != nil || ask.ID == "" {
		t.Fatalf("decode the ask opened on A: %v: %s", err, opened)
	}
	answered := pair.b.call(t, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer",
		`{"selected":["Ship"],"expected_edited_at":null}`, http.StatusOK)
	var answer struct {
		Answer *struct {
			At time.Time `json:"at"`
		} `json:"answer"`
	}
	if err := json.Unmarshal(answered, &answer); err != nil || answer.Answer == nil {
		t.Fatalf("decode the answer given on B: %v: %s", err, answered)
	}
	var answeredEvent time.Time
	if err := pair.database.Pool.QueryRow(ctx, `
		select created_at from events where issue_key = $1 and type = 'ask.answered' and payload->>'id' = $2
	`, key, ask.ID).Scan(&answeredEvent); err != nil {
		t.Fatalf("read the ask.answered event: %v", err)
	}
	if !answeredEvent.Equal(answer.Answer.At) {
		t.Errorf("B's answer: at %s, ask.answered created at %s, want the one transaction time",
			answer.Answer.At.Format(time.RFC3339Nano), answeredEvent.Format(time.RFC3339Nano))
	}
}

// ───── one configuration warning across the processes ──────────────────────

// misconfiguredRun is the deploy run the warning row seeds: a successful run on main whose one job
// is none of the production job names seedDeliverySettings configures.
const misconfiguredRun = int64(38041790209)

// A misconfigured production job name is named in the log once, not once per process: the Delivery
// page judges every deploy run on each read, on whichever process the read reached. The settings
// name a production job no job of the seeded run holds; the Delivery page is read on A, then on B,
// and the two processes' logs hold exactly one warning for that run between them. Each process's
// access line for its read is written after the read's warning, so once both are in, a warning
// either process wrote is in too.
func TestAConfigurationWarningIsLoggedOnceAcrossTheProcesses(t *testing.T) {
	pair := newPair(t)
	seedDeliverySettings(t, pair.database)
	ctx := context.Background()
	start := time.Date(2026, 10, 10, 9, 47, 17, 0, time.UTC)
	success := delivery.DeliveryRunConclusionSuccess
	run := strconv.FormatInt(misconfiguredRun, 10)
	if err := delivery.UpsertRun(ctx, pair.database.Pool, delivery.DeliveryRun{
		Repo: "acme/deploy", RunID: misconfiguredRun, Kind: delivery.DeliveryRunKindDeploy, HeadSHA: "sha" + run,
		HeadCommitAt: start.Add(-time.Hour), StartedAt: start, CompletedAt: new(start.Add(15 * time.Minute)), Conclusion: &success,
		URL: "https://github.com/acme/deploy/actions/runs/" + run, HeadBranch: new("main"), Event: new("workflow_dispatch"),
	}); err != nil {
		t.Fatalf("seed the run: %v", err)
	}
	if err := delivery.UpsertRunJobs(ctx, pair.database.Pool, "acme/deploy", misconfiguredRun, []delivery.DeliveryRunJob{{
		Repo: "acme/deploy", RunID: misconfiguredRun, Name: "prod-apply / prod-apply",
		Conclusion: new(delivery.DeliveryJobConclusionSuccess), CompletedAt: new(start.Add(10 * time.Minute)),
	}}, true); err != nil {
		t.Fatalf("seed the run's job: %v", err)
	}
	pair.start(t, nil)

	const timeline = "/api/v1/delivery/timeline?from=2026-10-10T00:00:00Z&to=2026-10-11T00:00:00Z"
	read := []string{`msg="dispatch: request"`, "route=/api/v1/delivery/timeline", "status=200"}
	pair.a.call(t, http.MethodGet, timeline, "", http.StatusOK)
	pair.a.waitForLog(t, 10*time.Second, read...)
	pair.b.call(t, http.MethodGet, timeline, "", http.StatusOK)
	pair.b.waitForLog(t, 10*time.Second, read...)

	warning := []string{"no job matching the configured production_job_names", "run_id=" + run}
	onA, onB := pair.a.logLines(warning...), pair.b.logLines(warning...)
	if len(onA)+len(onB) != 1 {
		t.Fatalf("warnings for run %s: %d on A, %d on B, want one between them:\nA:\n%s\nB:\n%s",
			run, len(onA), len(onB), strings.Join(onA, "\n"), strings.Join(onB, "\n"))
	}
}

// ───── two processes syncing one architecture source ───────────────────────

// architectureGitHub is the GitHub both processes' architecture syncs read: one repository whose
// branch head the test moves, each commit answering its own architecture files, and every blob
// read of one commit held until the test releases it, the caller gives up, or githubHoldCap
// passes. Any other App call answers an empty list, as githubFake's do.
type architectureGitHub struct {
	server      *httptest.Server
	held        chan struct{}
	release     chan struct{}
	heldOnce    sync.Once
	releaseOnce sync.Once

	mu          sync.Mutex
	head        string
	commits     map[string]map[string]string
	holdBlobsOf string
}

// startArchitectureGitHub serves on loopback until the test ends, its branch at head.
func startArchitectureGitHub(t *testing.T, head string, commits map[string]map[string]string) *architectureGitHub {
	t.Helper()
	fake := &architectureGitHub{held: make(chan struct{}), release: make(chan struct{}), head: head, commits: commits}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":77,"app_slug":"multiprocess","permissions":{"contents":"read"}}`)
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"token":"ghs_multiprocess","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{branch}", func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		head := fake.head
		fake.mu.Unlock()
		// As GitHub does: the bare sha for Accept: application/vnd.github.sha, JSON otherwise.
		if r.Header.Get("Accept") == "application/vnd.github.sha" {
			_, _ = io.WriteString(w, head)
			return
		}
		_, _ = fmt.Fprintf(w, `{"sha":%q}`, head)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/trees/{ref}", func(w http.ResponseWriter, r *http.Request) {
		commit, _, _ := strings.Cut(r.PathValue("ref"), ":")
		fake.mu.Lock()
		files := fake.commits[commit]
		fake.mu.Unlock()
		entries := []map[string]any{}
		for _, name := range slices.Sorted(maps.Keys(files)) {
			entries = append(entries, map[string]any{
				"path": name, "mode": "100644", "type": "blob", "sha": commit + "." + name, "size": len(files[name]),
			})
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"sha": "tree-" + commit, "truncated": false, "tree": entries}); err != nil {
			t.Errorf("encode tree: %v", err)
		}
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/blobs/{sha}", func(w http.ResponseWriter, r *http.Request) {
		commit, name, _ := strings.Cut(r.PathValue("sha"), ".")
		fake.mu.Lock()
		content, ok := fake.commits[commit][name]
		hold := commit == fake.holdBlobsOf
		fake.mu.Unlock()
		if hold {
			fake.heldOnce.Do(func() { close(fake.held) })
			select {
			case <-fake.release:
			case <-r.Context().Done():
			case <-time.After(githubHoldCap):
			}
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{
			"content": base64.StdEncoding.EncodeToString([]byte(content)), "encoding": "base64",
		}); err != nil {
			t.Errorf("encode blob: %v", err)
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "[]")
	})
	fake.server = httptest.NewServer(mux)
	t.Cleanup(func() {
		fake.releaseHeld()
		fake.server.Close()
	})
	return fake
}

// holdBlobs holds every blob read of commit; moveHead pushes commit as the branch's new head.
func (f *architectureGitHub) holdBlobs(commit string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.holdBlobsOf = commit
}

func (f *architectureGitHub) moveHead(commit string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.head = commit
}

func (f *architectureGitHub) releaseHeld() { f.releaseOnce.Do(func() { close(f.release) }) }

// Two processes sync one project's architecture source through the Refresh route: A is held
// fetching the files of head X, the branch moves to Y, and B imports Y. Once A is released it
// must not record X over Y: the source stays at Y with Y's components, the only synced event is
// Y's, and A answers the row at Y. The source is disabled so the import ticker adds no sync of
// its own.
func TestTwoProcessesSyncingOneProjectNeverWriteTheOlderCommit(t *testing.T) {
	const older, newer = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	github := startArchitectureGitHub(t, older, map[string]map[string]string{
		older: {"api.md": "---\ntitle: API\n---\nThe API.\n"},
		newer: {"queue.md": "---\ntitle: Queue\n---\nThe queue.\n", "store.md": "---\ntitle: Store\n---\nThe store.\n"},
	})
	github.holdBlobs(older)
	pair := startPair(t, githubAppEnv(t, github.server.URL))
	project := uniqueProjectKey(t, "ARC")
	pair.a.createProject(t, project, "Architecture")
	if _, err := pair.database.Pool.Exec(context.Background(), `
		insert into architecture_sources (project_key, repo, branch, enabled, installation_id, created_by)
		values ($1, 'acme/arch', 'main', false, 77, '{"kind":"user","id":"alice"}')
	`, project); err != nil {
		t.Fatalf("seed the architecture source: %v", err)
	}
	path := "/api/v1/projects/" + project + "/architecture-sources/acme/arch/sync"

	type answer struct {
		status int
		body   []byte
		err    error
	}
	held := make(chan answer, 1)
	go func() {
		answered, err := pair.a.trySend(http.MethodPost, path, "", asUser(dispatchTestLogin))
		if err != nil {
			held <- answer{err: err}
			return
		}
		held <- answer{status: answered.status, body: answered.body}
	}()
	select {
	case <-github.held:
	case <-time.After(30 * time.Second):
		t.Fatalf("A's sync never fetched the files of %s:\n%s", older, pair.a.Output.String())
	}
	github.moveHead(newer)
	pair.b.call(t, http.MethodPost, path, "", http.StatusOK)
	github.releaseHeld()

	var fromA answer
	select {
	case fromA = <-held:
	case <-time.After(30 * time.Second):
		t.Fatalf("A's sync never answered:\n%s", pair.a.Output.String())
	}
	if fromA.err != nil || fromA.status != http.StatusOK {
		t.Fatalf("A's sync: status %d, err %v: %s", fromA.status, fromA.err, fromA.body)
	}
	var row struct {
		LastCommit *string `json:"last_commit"`
		LastError  *string `json:"last_error"`
	}
	if err := json.Unmarshal(fromA.body, &row); err != nil {
		t.Fatalf("decode A's answer: %v: %s", err, fromA.body)
	}
	if row.LastCommit == nil || *row.LastCommit != newer || row.LastError != nil {
		t.Errorf("A answered %s, want the row at %s with no error", fromA.body, newer)
	}

	var lastCommit string
	var components, synced []string
	if err := pair.database.Pool.QueryRow(context.Background(), `
		select last_commit,
		       array(select id from components where project_key = $1 order by id),
		       array(select payload->>'commit' from events where project_key = $1 and type = 'architecture.synced' order by id)
		from architecture_sources where project_key = $1
	`, project).Scan(&lastCommit, &components, &synced); err != nil {
		t.Fatalf("read the source: %v", err)
	}
	if lastCommit != newer {
		t.Errorf("last_commit = %s, want %s", lastCommit, newer)
	}
	if want := []string{"queue", "store"}; !slices.Equal(components, want) {
		t.Errorf("components = %v, want %s's %v", components, newer, want)
	}
	if want := []string{newer}; !slices.Equal(synced, want) {
		t.Errorf("architecture.synced events name %v, want %v once", synced, want)
	}
}

// ───── route status from one stored listener answer ────────────────────────

// rosterEnv is what both processes of a route-status row are started with: the fake listener as
// ENVOY_URL, and a listener answer that serves issue reads for a minute
// (DISPATCH_TEST_ROUTE_ROSTER_TTL, under the test hooks every process here runs with), so the row
// reads both processes inside one answer's life however slow the machine, with no timing window.
func rosterEnv(listener *fakeListener) map[string]string {
	env := listener.processEnv()
	env["DISPATCH_TEST_ROUTE_ROSTER_TTL"] = time.Minute.String()
	return env
}

// routeIssue creates an issue in project titled title and routes it to route, returning its key.
func (p *dispatchProcess) routeIssue(t *testing.T, project, title, route string) string {
	t.Helper()
	created := p.call(t, http.MethodPost, "/api/v1/issues",
		fmt.Sprintf(`{"project":%q,"title":%q,"force":true}`, project, title), http.StatusCreated)
	var issue struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(created, &issue); err != nil || issue.Key == "" {
		t.Fatalf("create issue: %v: %s", err, created)
	}
	p.call(t, http.MethodPatch, "/api/v1/issues/"+issue.Key, fmt.Sprintf(`{"route":%q}`, route), http.StatusOK)
	return issue.Key
}

// issueReach is the issue's route_status and route_holder as GET /api/v1/issues/{key} on this
// process answers them, "<status> <holder>", each "null" when absent.
func (p *dispatchProcess) issueReach(t *testing.T, key string) string {
	t.Helper()
	var issue struct {
		RouteStatus *string `json:"route_status"`
		RouteHolder *string `json:"route_holder"`
	}
	answer := p.call(t, http.MethodGet, "/api/v1/issues/"+key, "", http.StatusOK)
	if err := json.Unmarshal(answer, &issue); err != nil {
		t.Fatalf("decode %s: %v: %s", key, err, answer)
	}
	orNull := func(value *string) string {
		if value == nil {
			return "null"
		}
		return *value
	}
	return orNull(issue.RouteStatus) + " " + orNull(issue.RouteHolder)
}

// liveRoutesPage is one page of project's open issues whose route reaches a live session, as GET
// /api/v1/issues?route_status=live on this process answers it: the page's keys and its total.
func (p *dispatchProcess) liveRoutesPage(t *testing.T, project string, limit, offset int) (keys []string, total int) {
	t.Helper()
	var page struct {
		Issues []struct {
			Key string `json:"key"`
		} `json:"issues"`
		Total int `json:"total"`
	}
	answer := p.call(t, http.MethodGet,
		fmt.Sprintf("/api/v1/issues?project=%s&route_status=live&limit=%d&offset=%d", project, limit, offset), "", http.StatusOK)
	if err := json.Unmarshal(answer, &page); err != nil {
		t.Fatalf("decode the page at offset %d: %v: %s", offset, err, answer)
	}
	for _, issue := range page.Issues {
		keys = append(keys, issue.Key)
	}
	return keys, page.Total
}

// Both processes judge a route against one stored listener answer. A route A reads live reads the
// same on B, and again on A, after the listener stops listing its holder, inside the answer's life:
// B does not ask the listener for a copy of its own, so the listener answers one live-list read
// in all. A filtered listing paged across the two processes is cut from that one answer too: a
// page read on B after the listener's list has flipped holds the rest of the issues A's page was
// cut from, never issues judged against B's own later copy.
func TestBothProcessesJudgeARouteAgainstOneRoster(t *testing.T) {
	sre := listenerSession{SessionID: "sre-holder", Title: "sre", Roles: []string{"sre"}, Capabilities: []string{}}
	reviewer := listenerSession{SessionID: "reviewer-holder", Title: "reviewer", Roles: []string{"reviewer"}, Capabilities: []string{}}

	t.Run("an issue's route", func(t *testing.T) {
		listener := startFakeListener(t, sre)
		pair := startPair(t, rosterEnv(listener))
		project := uniqueProjectKey(t, "RST")
		pair.a.createProject(t, project, "One roster")
		key := pair.a.routeIssue(t, project, "Routed to sre", "role:sre")

		if got := pair.a.issueReach(t, key); got != "live sre-holder" {
			t.Fatalf("A reads %s while the listener lists the holder, want live sre-holder", got)
		}
		listener.setSessions()
		for _, reader := range []*dispatchProcess{pair.b, pair.a} {
			if got := reader.issueReach(t, key); got != "live sre-holder" {
				t.Fatalf("%s reads %s after the listener stopped listing the holder, want the stored live sre-holder",
					pair.name(reader), got)
			}
		}
		if calls := listener.sessionListings(); calls != 1 {
			t.Fatalf("the listener answered %d live-list reads, want 1: both processes judge against one stored answer", calls)
		}
	})

	t.Run("a filtered page", func(t *testing.T) {
		listener := startFakeListener(t, sre)
		pair := startPair(t, rosterEnv(listener))
		project := uniqueProjectKey(t, "RSP")
		pair.a.createProject(t, project, "One roster, paged")
		routedToSRE := make([]string, 0, 6)
		for index := range 6 {
			routedToSRE = append(routedToSRE, pair.a.routeIssue(t, project, fmt.Sprintf("SRE work %d", index), "role:sre"))
			pair.a.routeIssue(t, project, fmt.Sprintf("Review work %d", index), "role:reviewer")
		}

		first, firstTotal := pair.a.liveRoutesPage(t, project, 3, 0)
		listener.setSessions(reviewer)
		second, secondTotal := pair.b.liveRoutesPage(t, project, 3, 3)
		if firstTotal != 6 || secondTotal != 6 {
			t.Errorf("total reads %d on A and %d on B, want 6 on both", firstTotal, secondTotal)
		}
		paged := slices.Concat(first, second)
		slices.Sort(paged)
		want := slices.Clone(routedToSRE)
		slices.Sort(want)
		if !slices.Equal(paged, want) {
			t.Fatalf("page 1 on A holds %v and page 2 on B holds %v, want together exactly the issues routed to sre %v",
				first, second, routedToSRE)
		}
	})
}

// ───── the test hooks ───────────────────────────────────────────────────────

// hookViewerSession is the session whose conversation the test-hook row's viewer watches: any
// UUID, since the in-process relay the publish hook feeds knows no sessions of its own.
const hookViewerSession = "01a0e090-4848-7473-acc5-fc96e6a646d3"

// startHookPair starts both processes as the browser harness starts its server: with the test
// hooks, as every process here runs, and without NATS, so the agent conversation relay each
// serves is the in-process source the publish hook feeds (main.go), one per process.
func startHookPair(t *testing.T) *dispatchPair {
	t.Helper()
	pair := &dispatchPair{database: storetest.Open(t)}
	options := dispatchOptions{SigningKey: multiprocessSigningKey}
	pair.a = startDispatchProcessWith(t, pair.databaseURL(t), options)
	pair.b = startDispatchProcessWith(t, pair.databaseURL(t), options)
	pair.a.waitHealthy(t)
	pair.b.waitHealthy(t)
	return pair
}

// liveSocketFrames reads conn, a live socket, until it ends: each frame of the agent view it
// holds lands on frames, and the read error that ended the socket on ended.
func liveSocketFrames(conn *gws.Conn) (frames <-chan string, ended <-chan streamEnd) {
	agent := make(chan string, 64)
	end := make(chan streamEnd, 1)
	go func() {
		defer close(agent)
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				end <- streamEnd{at: time.Now(), err: err}
				return
			}
			var frame struct {
				Type  string          `json:"type"`
				Event string          `json:"event"`
				Data  json.RawMessage `json:"data"`
			}
			if json.Unmarshal(data, &frame) != nil || frame.Type != "agent" || frame.Event != "frame" {
				continue
			}
			agent <- string(frame.Data)
		}
	}()
	return agent, end
}

// requireSocketClosed waits for a live socket to end with code, failing the row when it is still
// open within or ended another way.
func requireSocketClosed(t *testing.T, ended <-chan streamEnd, code int, within time.Duration, what string) {
	t.Helper()
	select {
	case end := <-ended:
		if !gws.IsCloseError(end.err, code) {
			t.Fatalf("%s ended with %v, want the hook's %d", what, end.err, code)
		}
	case <-time.After(within):
		t.Fatalf("%s was still open %v later", what, within)
	}
}

// Every test hook acts on every process serving the database, whichever process the call landed
// on: a browser suite against two processes disconnects, rotates, quiesces and feeds frames to
// the process its page happens to be connected to, which is not the one its own API call reaches.
// A live socket - one per tab, holding both the event feed and a conversation view - a live
// document room and a second live socket are each held open on B, and each hook is called on A.
// quiesce answers only once B's room has closed too, which is what a harness truncating its
// database right after relies on. Once B is killed its lock is gone with its connection, and a
// hook called on A answers on A's acknowledgement alone rather than waiting out the timeout for a
// process that no longer exists.
func TestEveryTestHookActsOnBothProcesses(t *testing.T) {
	pair := startHookPair(t)
	project := uniqueProjectKey(t, "HKS")
	pair.a.createProject(t, project, "Test hooks")
	_, artifactID := pair.a.createIssueIn(t, project, "Open on B", "before")

	// One socket per tab: B's holds the event feed and the conversation view the publish hook
	// feeds, as a page watching an agent holds them.
	viewer := pair.b.openLiveEvents(t)
	pair.b.watchAgent(t, viewer, hookViewerSession)
	published, viewerEnded := liveSocketFrames(viewer)
	const frame = `{"v":1,"kind":"message","seq":7}`
	pair.a.call(t, http.MethodPost, "/api/v1/agents/"+hookViewerSession+"/stream/_test/publish", frame, http.StatusOK)
	select {
	case got, open := <-published:
		if !open {
			t.Fatal("the conversation view B held closed before the frame published on A arrived")
		}
		if got != frame {
			t.Fatalf("the conversation view B held received %s, want the frame published on A", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the conversation view B held never received the frame published on A")
	}

	pair.a.call(t, http.MethodPost, "/api/v1/live/_test/disconnect", "", http.StatusOK)
	requireSocketClosed(t, viewerEnded, gws.CloseServiceRestart, 10*time.Second,
		"the live socket B held after the disconnect hook was called on A")

	_, rotated := liveSocketFrames(pair.b.openLiveEvents(t))
	pair.a.call(t, http.MethodPost, "/api/v1/live/_test/rotate", "", http.StatusOK)
	requireSocketClosed(t, rotated, contracts.LiveSocketMaxAgeClose, 10*time.Second,
		"the live socket B held after the rotate hook was called on A")

	editor := pair.b.openEditor(t, artifactID, "before\n")
	pair.a.call(t, http.MethodPost, "/api/v1/artifacts/_test/quiesce", "", http.StatusOK)
	// B closed the room before it acknowledged, and A answered only after that acknowledgement,
	// so the close is already on its way over loopback when the answer arrives: the bound is that
	// trip, not a wait for B to get round to it.
	select {
	case <-editor.Ended:
	case <-time.After(2 * time.Second):
		t.Fatalf("the document room B held was still open 2s after quiesce answered on A:\nB:\n%s", pair.b.Output.String())
	}

	_, survivor := liveSocketFrames(pair.a.openLiveEvents(t))
	pair.b.kill(t)
	called := time.Now()
	pair.a.call(t, http.MethodPost, "/api/v1/live/_test/disconnect", "", http.StatusOK)
	waited := time.Since(called)
	t.Logf("the disconnect hook on A answered %v after B was killed", waited)
	// The hook's timeout is ten seconds; an answer inside it is one that did not wait for B.
	if waited >= 10*time.Second {
		t.Fatalf("the disconnect hook on A answered %v after B was killed, want well inside the 10s timeout", waited)
	}
	requireSocketClosed(t, survivor, gws.CloseServiceRestart, 10*time.Second,
		"the live socket A held after the disconnect hook answered with B killed")
}

// ───── two dashboard builds ─────────────────────────────────────────────────

// dashboardBuild is a web dist a test wrote: its directory and every file below its assets
// directory, by URL path.
type dashboardBuild struct {
	dir    string
	assets map[string][]byte
}

// writeDashboardBuild writes a dist as Vite does, for build name: an index.html naming its own
// entry chunk, the entry, chunks of the sizes the dashboard's own run to (its largest is 1,747,955
// bytes) whose names and bytes are this build's alone, and a chunk of the same name and bytes in
// every build, as yjs and the stylesheet are across two builds of one source.
func writeDashboardBuild(t *testing.T, name string) dashboardBuild {
	t.Helper()
	build := dashboardBuild{dir: t.TempDir(), assets: map[string][]byte{}}
	entry := "/assets/index-" + name + ".js"
	build.assets[entry] = []byte("import './chunk-" + name + "-0.js'; console.log('" + name + "')")
	for i, size := range []int{1_747_955, 420_000, 96_000, 12_000, 2_000, 300, 1} {
		build.assets[fmt.Sprintf("/assets/chunk-%s-%d.js", name, i)] = bytes.Repeat([]byte(name), size)
	}
	build.assets["/assets/fonts/inter-"+name+".woff2"] = []byte{0x77, 0x4f, 0x46, 0x32, 0x00, byte(len(name))}
	build.assets["/assets/yjs-shared.js"] = []byte("export const yjs = 'the same in every build'")
	files := map[string][]byte{"index.html": []byte(`<!doctype html><html><head><script type="module" crossorigin src="` + entry + `"></script></head><body><div id="root"></div></body></html>`)}
	for path, content := range build.assets {
		files[strings.TrimPrefix(path, "/")] = content
	}
	for name, content := range files {
		file := filepath.Join(build.dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return build
}

// startOnBuild starts a Dispatch process serving build on database, with no NATS: the dashboard's
// assets need none. It carries multiprocessSigningKey, as every process beside a live peer must.
func startOnBuild(t *testing.T, database string, build dashboardBuild) *dispatchProcess {
	t.Helper()
	process := startDispatchProcessWith(t, database, dispatchOptions{SigningKey: multiprocessSigningKey, Env: map[string]string{"DISPATCH_WEB_DIST": build.dir}})
	process.waitHealthy(t)
	return process
}

// getAsset fetches path from the process and says what is wrong with the answer, or nothing.
func getAsset(client *http.Client, p *dispatchProcess, path string, want []byte) error {
	response, err := client.Get(p.url(path))
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("GET %s: read: %w", path, err)
	}
	if response.StatusCode != http.StatusOK || !bytes.Equal(body, want) {
		return fmt.Errorf("GET %s: status %d with %d bytes, want 200 with the %d bytes on disk", path, response.StatusCode, len(body), len(want))
	}
	if got := response.Header.Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		return fmt.Errorf("GET %s: Cache-Control %q, want the immutable year", path, got)
	}
	return nil
}

// A rolling update runs two builds on one database, each process holding its own build's files
// alone: a page one process served asks the other for its chunks. Every file of either build
// answers 200 with its bytes from both processes.
func TestDashboardServesEveryFileOfEitherBuildFromBothProcesses(t *testing.T) {
	database := (&dispatchPair{database: storetest.Open(t)}).databaseURL(t)
	builds := []dashboardBuild{writeDashboardBuild(t, "a"), writeDashboardBuild(t, "b")}
	processes := []*dispatchProcess{startOnBuild(t, database, builds[0]), startOnBuild(t, database, builds[1])}

	for i, process := range processes {
		for j, build := range builds {
			for path, content := range build.assets {
				if err := getAsset(http.DefaultClient, process, path, content); err != nil {
					t.Errorf("process %s, build %s's file: %v", "AB"[i:i+1], "ab"[j:j+1], err)
				}
			}
		}
	}
}

// Every chunk a stale tab's route imports is asked for at once, by every such tab: a burst of 200
// concurrent requests for build A's files, every one but the shared chunk missing from B's disk, is
// answered 200 with each file's bytes by B.
func TestDashboardServesABurstOfTheOtherBuildsFiles(t *testing.T) {
	database := (&dispatchPair{database: storetest.Open(t)}).databaseURL(t)
	a := writeDashboardBuild(t, "a")
	startOnBuild(t, database, a)
	b := startOnBuild(t, database, writeDashboardBuild(t, "b"))
	paths := slices.Sorted(maps.Keys(a.assets))

	const burst = 200
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: burst}}
	failures := make([]error, burst)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range burst {
		wg.Go(func() {
			<-start
			path := paths[i%len(paths)]
			failures[i] = getAsset(client, b, path, a.assets[path])
		})
	}
	close(start)
	wg.Wait()

	failed := 0
	for _, err := range failures {
		if err != nil {
			failed++
			t.Error(err)
		}
	}
	if failed > 0 {
		t.Fatalf("%d of %d concurrent requests for A's files to B failed:\n%s", failed, burst, b.Output.String())
	}
}

// A pod restarted on the build it ran writes no asset: the restart's publish writes no file's
// bytes and leaves every stored asset row as it was, renewing only the build's last start, and the
// restarted process still serves every file.
func TestDashboardRestartOnTheSameBuildWritesNoAssetBytes(t *testing.T) {
	ctx := context.Background()
	pair := &dispatchPair{database: storetest.Open(t)}
	database := pair.databaseURL(t)
	build := writeDashboardBuild(t, "a")
	first := startOnBuild(t, database, build)
	if !first.logged("dispatch: published dashboard assets", fmt.Sprintf("assets=%d written=%d", len(build.assets), len(build.assets))) {
		t.Fatalf("the first boot did not write every one of the build's %d files:\n%s", len(build.assets), first.Output.String())
	}
	assetRows := func() string {
		var rows string
		if err := pair.database.Pool.QueryRow(ctx, `select string_agg(path || ':' || xmin::text || ':' || created_at::text, ',' order by path) from dashboard_assets`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	lastStarted := func() time.Time {
		var at time.Time
		if err := pair.database.Pool.QueryRow(ctx, `select last_started_at from dashboard_builds`).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	rowsBefore, startedBefore := assetRows(), lastStarted()
	first.Terminate(t)
	first.WaitExit(t, "SIGTERM")

	restarted := startOnBuild(t, database, build)

	if !restarted.logged("dispatch: published dashboard assets", fmt.Sprintf("assets=%d written=0", len(build.assets))) {
		t.Errorf("the restart wrote asset bytes, want none:\n%s", restarted.Output.String())
	}
	if rowsAfter := assetRows(); rowsAfter != rowsBefore {
		t.Errorf("the restart changed asset rows: before %s, after %s", rowsBefore, rowsAfter)
	}
	if startedAfter := lastStarted(); !startedAfter.After(startedBefore) {
		t.Errorf("the build's last start is %s after the restart, want later than %s", startedAfter, startedBefore)
	}
	for path, content := range build.assets {
		if err := getAsset(http.DefaultClient, restarted, path, content); err != nil {
			t.Error(err)
		}
	}
}

// dashboardBuildsOf is GET /api/v1/dashboard/builds from the process, as the test's person.
func dashboardBuildsOf(t *testing.T, p *dispatchProcess) (serving string, live []string) {
	t.Helper()
	var answer struct {
		Serving string   `json:"serving"`
		Live    []string `json:"live"`
	}
	if err := json.Unmarshal(p.call(t, http.MethodGet, "/api/v1/dashboard/builds", "", http.StatusOK), &answer); err != nil {
		t.Fatal(err)
	}
	return answer.Serving, answer.Live
}

// A tab offers "A new version is available" only when one build is live and it is not the tab's,
// so the builds route is what a rolling update shows it. While A and B run both builds are live,
// whichever process answers; once B shuts down gracefully it has deregistered and only A's build
// is; and a process that stopped beating without deregistering drops out once its heartbeat is
// older than the live window, aged here in the database rather than waited for.
func TestDashboardBuildsAnswersTheBuildsTheLiveProcessesServe(t *testing.T) {
	ctx := context.Background()
	pair := &dispatchPair{database: storetest.Open(t)}
	database := pair.databaseURL(t)
	const entryA, entryB = "/assets/index-a.js", "/assets/index-b.js"
	buildB := writeDashboardBuild(t, "b")
	a := startOnBuild(t, database, writeDashboardBuild(t, "a"))
	b := startOnBuild(t, database, buildB)

	for _, process := range []struct {
		name    string
		p       *dispatchProcess
		serving string
	}{{"A", a, entryA}, {"B", b, entryB}} {
		if serving, live := dashboardBuildsOf(t, process.p); serving != process.serving || !slices.Equal(live, []string{entryA, entryB}) {
			t.Errorf("process %s with both live: serving %q, live %q; want %q and both builds", process.name, serving, live, process.serving)
		}
	}

	b.Terminate(t)
	b.WaitExit(t, "SIGTERM")
	if serving, live := dashboardBuildsOf(t, a); serving != entryA || !slices.Equal(live, []string{entryA}) {
		t.Fatalf("after B shut down: serving %q, live %q; want %q alone", serving, live, entryA)
	}

	// A process on B's build that is beating, and then one whose last heartbeat left the window.
	var instance string
	if err := pair.database.Pool.QueryRow(ctx, `
		insert into server_processes (instance, dashboard_build, signing_keys) values (gen_random_uuid(), $1, '{fingerprint}')
		returning instance::text`, routes.DashboardBuild(buildB.dir)).Scan(&instance); err != nil {
		t.Fatal(err)
	}
	if _, live := dashboardBuildsOf(t, a); !slices.Equal(live, []string{entryA, entryB}) {
		t.Fatalf("with a beating process on B's build: live %q, want both builds", live)
	}
	if _, err := pair.database.Pool.Exec(ctx, `
		update server_processes set heartbeat_at = now() - make_interval(secs => $2) where instance = $1::uuid`,
		instance, processes.LiveWindow.Seconds()+1); err != nil {
		t.Fatal(err)
	}
	if _, live := dashboardBuildsOf(t, a); !slices.Equal(live, []string{entryA}) {
		t.Fatalf("after its heartbeat aged past the live window: live %q, want %q alone", live, entryA)
	}
}
