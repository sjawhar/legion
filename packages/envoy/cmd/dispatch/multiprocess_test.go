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
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	natsgo "github.com/nats-io/nats.go"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/redeliver"
	"github.com/sjawhar/envoy/internal/dispatch/routes"
	"github.com/sjawhar/envoy/internal/dispatch/startup"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
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
	p.a = p.startProcess(t, env)
	p.b = p.startProcess(t, env)
	p.a.waitHealthy(t)
	p.b.waitHealthy(t)
}

// startProcess starts one process of the pair behind a NATS proxy of its own, and returns without
// waiting for it to be healthy.
func (p *dispatchPair) startProcess(t *testing.T, env map[string]string) *dispatchProcess {
	t.Helper()
	proxy := testnats.StartProxy(t, p.nats)
	process := startDispatchProcessWith(t, p.databaseURL(t),
		dispatchOptions{NATSURL: proxy.URL(), SigningKey: multiprocessSigningKey, Env: env})
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
// for this test, as its pool's configuration names it. The credential is written through
// url.UserPassword when the pool carries a password, since CI's Postgres takes one
// (DISPATCH_TEST_DATABASE_URL) where a trust-authenticated development server does not, and a URL
// built without it is refused with SQLSTATE 28P01 before the process can serve anything.
func (p *dispatchPair) databaseURL(t *testing.T) string {
	t.Helper()
	config := p.database.Pool.Config().ConnConfig
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
// leaves the machine. It answers an empty list to everything, and holds the first webhook
// delivery listing - the webhook redelivery sweep's own first request, made at boot - until
// release is closed, which is how a row makes one process hold that sweep's lease long enough
// for the other to find it held. It counts those listings too: the listing is the call the
// webhook-redelivery lease exists to make once however many processes sweep, so a row about
// that lease reads the count rather than the log alone.
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
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "[]")
	}))
	t.Cleanup(func() {
		fake.Release()
		fake.server.Close()
	})
	return fake
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
func TestOneProcessBindsEachDeliveryDurableAndTheOtherStandsBy(t *testing.T) {
	pair := newPair(t)
	seedDeliverySettings(t, pair.database)
	fake := startGitHubFake(t)
	fake.Release()
	pair.start(t, githubAppEnv(t, fake.URL()))

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

	// The holder dies the way a task replaced mid-deploy or killed for memory does, with no
	// chance to unsubscribe, and the process that was standing by takes the durable over.
	dying := holders["delivery-events"]
	survivor := pair.other(dying)
	if err := dying.Cmd.Process.Kill(); err != nil {
		t.Fatalf("kill the holder: %v", err)
	}
	<-dying.Exited
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

// A spec tab is open on one process while the other writes the document. The two do not share a
// live document, so the room A holds would otherwise sit on text the store no longer has: the
// write announces itself over Postgres (dispatch_doc_changed) and A closes the room it holds
// behind the durable head, which closes the editor's socket so a browser reconnects and re-syncs.
func TestADocumentWriteOnOneProcessClosesTheStaleRoomOnTheOther(t *testing.T) {
	pair := startPair(t, nil)
	project := uniqueProjectKey(t, "DOC")
	pair.a.createProject(t, project, "Documents")
	_, artifactID := pair.a.createIssueIn(t, project, "Open here, written there", "before")

	editor := openSyncedEditor(t, pair, artifactID, "before\n")
	wrote := time.Now()
	pair.b.call(t, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits",
		`{"ops":[{"op":"replace","find":"before","with":"after"}]}`, http.StatusOK)

	select {
	case <-editor.Ended:
		t.Logf("the room A held closed %v after B's write", time.Since(wrote))
	case <-time.After(30 * time.Second):
		t.Fatalf("the room A held never closed after B wrote the document:\nA:\n%s", pair.a.Output.String())
	}
	// The eviction's own line is written after the room has closed, which is what ended the
	// editor's socket, so it is waited for rather than read the moment the socket went.
	pair.a.waitForLog(t, 10*time.Second,
		`msg="dispatch: closed a document room another process's write left behind"`, "room="+artifactID)
	// The reader that comes next reads the document the store holds, not what the closed room
	// still had.
	if text := pair.a.documentText(t, artifactID); text != "after\n" {
		t.Errorf("A serves %q after B's write; want the current document, %q", text, "after\n")
	}
}

// evictionsOf is how many rooms this process has closed behind another process's write, for one
// document.
func evictionsOf(p *dispatchProcess, artifactID string) int {
	return len(p.logLines(`msg="dispatch: closed a document room another process's write left behind"`, "room="+artifactID))
}

// settlementOwedFor reports whether the document owes a settlement right now, which is the
// condition the two rows below are told apart by.
func settlementOwedFor(t *testing.T, pair *dispatchPair, artifactID string) bool {
	t.Helper()
	var owed bool
	if err := pair.database.Pool.QueryRow(context.Background(),
		`select exists(select 1 from doc_settlements_pending where artifact_id = $1)`, artifactID).Scan(&owed); err != nil {
		t.Fatalf("read whether %s owes a settlement: %v", artifactID, err)
	}
	return owed
}

// storedHead is the version of the document's latest durable update, which is the version a room
// of it must stand at to append anything (ErrRoomBehindHead).
func storedHead(t *testing.T, pair *dispatchPair, artifactID string) int64 {
	t.Helper()
	var head int64
	if err := pair.database.Pool.QueryRow(context.Background(),
		`select coalesce(max(version), 0) from doc_updates where artifact_id = $1`, artifactID).Scan(&head); err != nil {
		t.Fatalf("read the stored head of %s: %v", artifactID, err)
	}
	return head
}

// openSyncedEditor opens a spec tab on A, as openEditor does, and returns once the update the tab
// sends as it syncs is stored, so that whatever B writes next is the only thing that can leave A's
// room behind the durable head.
//
// A browser's provider answers the room's sync step 1 with its own state (docstest.Peer.read, as
// y-websocket does), and the room appends that update even when it changes nothing, since ygo runs
// the room's update observer for every transaction it commits. An append from a room standing
// behind the head is refused by the durable gate, and the refusal fails the room
// (ErrRoomBehindHead; docs/stale_room.go names this path): the room closes and the browser
// reconnects. That is correct for a browser, but a row timing which path closes A's room reads it
// as some other path. So a tab whose update is still on its way when B's write lands closes its
// own socket within milliseconds of that write, which is how the refused-notification row failed
// 2 of 20 runs on a development machine (dispatch://INFRA-396).
//
// The head is read before the dial, and nothing else writes the document while the tab connects:
// B writes only after this returns, and a settlement is at least the settle delay away (2 s by
// default). So the head moves by exactly one, the tab's own update, and anything more fails the
// row rather than letting it run on with that update still on its way. The wait is openEditor's
// own ten seconds: on a loaded development machine 52 such updates were stored 16-294 ms after
// the dial, and one was not stored within a second. A tab whose update is not stored in ten fails
// the row: either the room stopped storing an update for a sync that changes nothing, and this
// wait has outlived its reason, or the machine is too slow for the row. How much of the eviction
// window a reconnect spent is the gap burstLeavingTheRoomBehind asserts.
func openSyncedEditor(t *testing.T, pair *dispatchPair, artifactID, want string) *docstest.Peer {
	t.Helper()
	const within = 10 * time.Second
	before := storedHead(t, pair, artifactID)
	dialled := time.Now()
	editor := pair.a.openEditor(t, artifactID, want)
	for {
		head := storedHead(t, pair, artifactID)
		switch {
		case head == before+1:
			t.Logf("the tab's own sync update was stored %v after it dialled the room", time.Since(dialled))
			return editor
		case head > before+1:
			t.Fatalf("the document moved from update %d to %d while a tab connected, and only the tab's own sync update was to be stored: something else wrote it, and the tab's update may still be on its way",
				before, head)
		case time.Since(dialled) >= within:
			t.Fatalf("the tab's own sync update was not stored within %v of its dial: the document still stands at update %d. Either the room no longer stores an update for a sync that changes nothing, which makes this wait unnecessary, or the machine is too slow for this row.",
				within, before)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// writeThroughRetrying503s makes one edit through the process's own API, repeating a 503 as a
// client is told to. A write whose own room is failed or reloading is answered
// 503 DOC_SERVICE_UNAVAILABLE, and the contract is that the caller repeats the same call
// (`dispatch doc-edit --help`, and the skills/dispatch/SKILL.md opening list). A rig that
// read that 503 as a failure would report a correct contract as a defect, which one run of this
// row's first draft did.
func writeThroughRetrying503s(t *testing.T, p *dispatchProcess, artifactID, find, with string) int {
	t.Helper()
	body := fmt.Sprintf(`{"ops":[{"op":"replace","find":%q,"with":%q}]}`, find, with)
	for attempts := 1; ; attempts++ {
		response, err := p.send(http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", body)
		if err != nil {
			t.Fatalf("write %q over %q: %v", with, find, err)
		}
		status := response.StatusCode
		_ = response.Body.Close()
		switch {
		case status == http.StatusOK:
			return attempts
		case status != http.StatusServiceUnavailable || attempts >= 20:
			t.Fatalf("write %q over %q answered %d after %d attempts", with, find, status, attempts)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// staleRoomEvictInterval is the length of the window one eviction of a room opens: the least
// time between two evictions of that room, so a notification arriving inside it is one the
// interval refuses. It is a copy of docs/stale_room.go's own staleRoomEvictInterval, because
// that constant is unexported and this is another package. The rows below rest on a trailing
// write whose notification lands inside this window, and burstLeavingTheRoomBehind asserts
// that, so a change to that constant which is not made here shows up as that assertion failing
// rather than as a row quietly measuring something else.
const staleRoomEvictInterval = 2 * time.Second

// burstLeavingTheRoomBehind drives the shape both rows below are about: a first write on B that
// evicts A's room, A's browser reconnecting as a browser does, and a second write landing inside
// the two-second eviction interval the first one started, so its notification is one the interval
// refuses. Afterwards nothing writes that document again, which is the whole point: the last
// write of a burst is one of the refused ones, and nothing comes back for it.
//
// That the second write does land inside the window is the construction, not a given, and it is
// asserted here: everything between the first eviction and that write - the wait for the
// eviction's line, A's reconnect, the write's own 503 retries - spends the window, and a row
// whose sum passed it would be evicted on the spot by a notification the interval allowed, which
// is a different scenario wearing this one's name. This row's first construction went wrong that
// way once.
func burstLeavingTheRoomBehind(t *testing.T, pair *dispatchPair, artifactID string) (*docstest.Peer, time.Time) {
	t.Helper()
	editor := openSyncedEditor(t, pair, artifactID, "before\n")
	writeThroughRetrying503s(t, pair.b, artifactID, "before", "middle")
	var evicted time.Time
	select {
	case <-editor.Ended:
		// Where the interval's window starts, as closely as this side can see it: the evictor's
		// close (closeRoomBehindHead) closes the room - which is what ends this socket - and the
		// stamp the interval reads is taken as that call returns (recordEviction,
		// docs/stale_room.go). This stamp is on the test's side of that close, so it is the
		// later of the two, and the gap measured at the end of this helper reads shorter than
		// the window the write really spent, by the close's trip
		// out to this process. The error runs towards under-reading, which makes the assertion
		// below permissive rather than conservative: a write that landed a hair past the
		// interval could still read under it. That is acceptable for what this guard is for -
		// catching a run that drifted out of the window altogether - because the gaps it reads
		// are 91-164 ms against a 2 s interval, and no trip of a loopback socket close brings a
		// run that really spent the interval back under it. Stamped here rather than after the
		// wait below, which polls at 50 ms, because that stamp would be later still and would
		// hide up to a further 50 ms of the window.
		evicted = time.Now()
	case <-time.After(30 * time.Second):
		t.Fatalf("the room A held never closed after B's first write:\n%s", pair.a.Output.String())
	}
	// The tab reconnects the moment its socket closes, as the dashboard's document provider does
	// (@hocuspocus/provider's initialDelay is 0, features/doc/connection.ts), so the reloaded room's
	// load - which arms the settlement the document owes, the settle delay out - lands with the
	// eviction that starts the window. A reconnect that waited for the eviction's log line first
	// put up to that poll's 50 ms between the two, so the evictor's re-examination at the window's
	// end and that settlement met less often than they do for a browser (dispatch://INFRA-396). Its
	// own sync update is stored before the trailing write (openSyncedEditor), so that write alone
	// leaves the room behind the head.
	//
	// Dialling this early can land in ygo's own window between a close's peer hang-up and its
	// removal of the room, where the dial is refused: a real browser's reconnect has the same
	// window, and the row then fails at its open, naming the refusal, rather than passing on
	// something it did not measure.
	reconnected := openSyncedEditor(t, pair, artifactID, "middle\n")
	// The eviction's own line is written after the room has closed, so it is waited for rather
	// than read the moment the socket went.
	pair.a.waitForLog(t, 10*time.Second,
		`msg="dispatch: closed a document room another process's write left behind"`, "room="+artifactID)
	if evictions := evictionsOf(pair.a, artifactID); evictions != 1 {
		t.Fatalf("want one eviction after the first write, got %d", evictions)
	}
	wrote := time.Now()
	writeThroughRetrying503s(t, pair.b, artifactID, "middle", "after")
	// Measured to the write's own answer, not to the moment it was begun: the notification is
	// sent by the transaction that made the change, so it is committed before that answer comes
	// back, and the retries before it spend the window too. Logged either way, so every run
	// reports how much of the window it had left.
	gap := time.Since(evicted)
	t.Logf("the trailing write landed %v after the first eviction, out of the %v the eviction interval allows it", gap, staleRoomEvictInterval)
	if gap >= staleRoomEvictInterval {
		t.Fatalf("the trailing write landed %v after the first eviction, which is past the %v eviction interval: its notification is not one the interval refuses, so it was allowed, the room was evicted where it stood, and this row measured an immediate eviction rather than the deferred re-examination it is named for. What spends that window is the wait for the eviction's line (polled at 50 ms), A's reconnect and the write's own 503 retries.",
			gap, staleRoomEvictInterval)
	}
	return reconnected, wrote
}

// requireTheRoomClosed waits for the room A holds to close and reports how long after the
// trailing write it took, which is the measurement: the eviction interval's own length means the
// deferred re-examination, since nothing notified this process again. A room that stays open is
// reported with the evictor's own lines, which tell a wake that never reached the room from a
// re-examination that reached no verdict. within is what tells the window's close from the other
// path that could have closed this room, so each row sets its own; the close itself is credited
// to the window only by requireTheSecondEviction below.
func requireTheRoomClosed(t *testing.T, p *dispatchProcess, editor *docstest.Peer, artifactID string, wrote time.Time, within time.Duration, what string) {
	t.Helper()
	select {
	case <-editor.Ended:
		t.Logf("%s: the room closed %v after the trailing write", what, time.Since(wrote))
	case <-time.After(within):
		t.Fatalf("%s: the reader kept a room the trailing write of the burst left behind - no second eviction in %v.\nThe evictor's own lines, which say whether the re-examination ran and reached no verdict or never reached the room:\n%v\n%v\nThe process's output:\n%s",
			what, within,
			p.logLines("close a document room another process's write left behind"),
			p.logLines("read the head of a document that changed"),
			p.Output.String())
	}
	requireTheSecondEviction(t, p, artifactID, what)
}

// requireTheSecondEviction is what credits the close above to the window: editor.Ended says the
// socket went, not who ended it, and both rows below have other paths that end it - a browser
// append the durable gate refused (ErrRoomBehindHead), which fails the room, that settlement's
// own close of a room the durable gate finds behind the head (reloadRoomBehindHead,
// docs/service.go), an unload for either - so the close is the window's only if the evictor
// closed that room a second time. The first eviction is asserted in burstLeavingTheRoomBehind,
// so the count here is two. The line is written after the room has closed, so it is waited for
// rather than counted the moment the socket went; ten seconds is the budget the eviction's other
// waits in this file use, and it is spent only by a row that is already failing.
func requireTheSecondEviction(t *testing.T, p *dispatchProcess, artifactID, what string) {
	t.Helper()
	const budget = 10 * time.Second
	deadline := time.Now().Add(budget)
	for {
		evictions := evictionsOf(p, artifactID)
		switch {
		case evictions == 2:
			return
		case evictions > 2:
			t.Fatalf("%s: the evictor closed this room %d times, and the burst leaves it exactly two - the first write's and the refused notification's window - so something else wrote this document:\n%v",
				what, evictions, p.logLines("closed a document room another process's write left behind", "room="+artifactID))
		case time.Now().After(deadline):
			t.Fatalf("%s: the room closed but the evictor has closed it %d time(s) in %v, not the two this burst leaves, so what closed it is some other path - a browser append the durable gate refused, the owed settlement's close of a room behind the head, or an unload - and not the window of the notification the interval refused.\nThis room's evictions, then the closes and the head reads that failed:\n%v\n%v\n%v\nThe process's output:\n%s",
				what, evictions, budget,
				p.logLines("closed a document room another process's write left behind", "room="+artifactID),
				p.logLines("close a document room another process's write left behind", "room="+artifactID),
				p.logLines("read the head of a document that changed", "room="+artifactID),
				p.Output.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// owedSettlementDelay is process A's settle delay in the row below, which holds its owed
// settlement past the window. That settlement is armed when A's reconnecting browser loads the
// room, because the document owes one (onLoadDocument), and it is the other path that closes this
// room: the durable gate refuses it from a room behind the head, and the refusal closes the room
// (reloadRoomBehindHead, INFRA-218). At the default two seconds it fired 10-50 ms after the
// window's own close, close enough on a loaded runner to come first, and its close is not the
// evictor's, so the row failed on requireTheSecondEviction. Ten seconds keeps the settlement clear
// of the window and of refusedWindowClosesWithin below, so the window is what closes the room
// whenever the window works, and the settlement is what closes it only when it does not.
const owedSettlementDelay = 10 * time.Second

// refusedWindowClosesWithin bounds the row below between the two paths that could close its
// room, since a bound above both would be green on either. The window's own re-examination
// closes it a little under two seconds after the trailing write - what the write left of the
// interval, plus the close - which was 1.9486 s in this row's green run here and 1.88 s when it
// was built. The owed settlement closes it no sooner than owedSettlementDelay after A's reconnect
// (every later arm restarts its timer), which is before the trailing write, so about ten seconds
// or more from that write.
//
// Both of those figures are taken from the trailing write; this budget is not counted from
// there. It is spent by requireTheRoomClosed's own time.After, which starts when that helper is
// entered: after burstLeavingTheRoomBehind returned, so after the trailing write's answer came
// back, and after the row's settlementOwedFor query. The wrote stamp the helper is also passed
// only labels its log line. That origin is later than wrote by the trailing write's own
// duration plus that one query, which is inside the 91-164 ms the helper logs for the whole
// stretch from the first eviction, the write included. So in the terms the two figures above
// use, the deadline falls at about 4.1 s: some 2.2 s past the window's close and some 5.9 s
// short of the settlement's. This row still goes red when the wake is gone instead of green on
// the settlement's clock.
const refusedWindowClosesWithin = 4 * time.Second

// The deferred re-examination, in the branch where something else could also have rescued the
// room. A notification the eviction interval refuses is remembered and re-examined when that
// room's window ends; here the room also owes a settlement, which the durable gate refuses from
// a room behind the head, and that refusal closes the room too (reloadRoomBehindHead). A's settle
// delay (owedSettlementDelay) puts that settlement well after the window, so the window is what
// closes the room, which is why this row is bounded below the settlement's own time
// (refusedWindowClosesWithin) and why the close must be the evictor's second eviction of this
// room rather than any close at all.
func TestTheRefusedNotificationClosesARoomThatOwesASettlement(t *testing.T) {
	pair := newPair(t)
	pair.a = startDispatchProcessWith(t, pair.databaseURL(t), dispatchOptions{
		NATSURL: pair.nats, SigningKey: multiprocessSigningKey, SettleDelay: owedSettlementDelay,
	})
	pair.b = startDispatchProcessWith(t, pair.databaseURL(t), dispatchOptions{
		NATSURL: pair.nats, SigningKey: multiprocessSigningKey,
	})
	pair.a.waitHealthy(t)
	pair.b.waitHealthy(t)
	project := uniqueProjectKey(t, "OWD")
	pair.a.createProject(t, project, "Owes a settlement")
	_, artifactID := pair.a.createIssueIn(t, project, "Written in a burst elsewhere", "before")

	reconnected, wrote := burstLeavingTheRoomBehind(t, pair, artifactID)
	if !settlementOwedFor(t, pair, artifactID) {
		t.Fatalf("this document owes no settlement, so this row is not the branch it is named for")
	}
	requireTheRoomClosed(t, pair.a, reconnected, artifactID, wrote, refusedWindowClosesWithin, "owes a settlement")
}

// The branch the deferred re-examination was built for, and the one nothing else covers: a room
// no settlement of this process's will ever close. A settlement is what could rescue the room in
// the row above - refused by the durable gate from a room behind the head, it closes that room
// (reloadRoomBehindHead) - and a reader with the tab open who has typed nothing has no such
// rescuer, so the window's re-examination is the only path that closes its room, and without it
// that reader holds text the store replaced for as long as the tab stays open (roomIdleTimeout is
// how long a room stays resident *after its last peer leaves*, which a room with a browser in it
// never reaches).
//
// How that room is made, and what the construction is and is not. The document is created and
// written on B, and A only ever reads it, which removes the rescuer rather than the condition -
// the stronger of the two for what this row asks, since it leaves the window as the only
// remaining path whatever else is armed. A's thirty-minute settle delay
// (DISPATCH_TEST_SETTLE_DELAY, under the test hooks) is not by itself that removal, and the
// claim that no settlement of A's can fire under it would be false: s.settle gates only the
// timer-armed path (scheduleSettleLocked), while settleLastPeer - wired as OnLastPeer - settles
// at once when the last browser leaves whatever timer is armed, retrySettleSoon re-arms at a
// fixed 10 ms, and a mark's expiry arms its own. What makes the delay enough here is that the
// close this row measures, the evictor's (closeRoomBehindHead), stops the room's armed settle
// timer as it reserves the room and arms it again at A's own thirty-minute delay, and that the
// row then excludes the settlements that could themselves have closed the room: a settlement the
// durable gate refused from this room behind the head logs a line naming "its settlement found
// behind the durable head" for every outcome that can close it - only a service stopping, a
// shutdown's skip and a room already closed or failed log otherwise or not at all, and none of
// those closes a room - and a failed one logs "dispatch: settle document". A settlement that
// committed logs neither, and it closes no room either, so the evictor's second close
// (requireTheSecondEviction) is still what credits the close to the window.
//
// The condition itself was tried first and did not hold: B was given a settle delay short enough
// to clear the pending row before A's reconnect loaded the room, and the row asserted that row
// absent at the load - which passed, and the room still settled, because A's own reconnecting
// peer appends as it syncs and arms a settlement of its own. With the wake wired that assertion
// passed only because the window (1.88 s) beat the settle delay (2 s), which is a vacuous pass;
// the control with the wake unwired is what exposed it. Hence this shape, and hence the
// assertion below that A logged no settlement that could have closed this room, which under a
// 30-minute delay is a real condition rather than a race the window happens to win.
//
// Its budget stays at thirty seconds, which already discriminates: with the wake unwired nothing
// closes this room at all, so no bound is needed to tell the window from a rescuer it has none
// of.
func TestTheRefusedNotificationClosesARoomNoSettlementWillClose(t *testing.T) {
	pair := newPair(t)
	pair.a = startDispatchProcessWith(t, pair.databaseURL(t), dispatchOptions{
		NATSURL: pair.nats, SigningKey: multiprocessSigningKey, SettleDelay: 30 * time.Minute,
	})
	pair.b = startDispatchProcessWith(t, pair.databaseURL(t), dispatchOptions{
		NATSURL: pair.nats, SigningKey: multiprocessSigningKey,
	})
	pair.a.waitHealthy(t)
	pair.b.waitHealthy(t)

	project := uniqueProjectKey(t, "OWN")
	pair.b.createProject(t, project, "No settlement will close it")
	_, artifactID := pair.b.createIssueIn(t, project, "Open here, typed into never", "before")

	reconnected, wrote := burstLeavingTheRoomBehind(t, pair, artifactID)
	requireTheRoomClosed(t, pair.a, reconnected, artifactID, wrote, 30*time.Second, "no settlement will close it")
	settled := pair.a.logLines("dispatch: settle document", artifactID)
	settled = append(settled, pair.a.logLines("its settlement found behind the durable head", artifactID)...)
	if len(settled) > 0 {
		t.Errorf("A ran a settlement of this room that could have closed it, so something other than the window could have:\n%v", settled)
	}
}

// ───── the asks of a closed issue ──────────────────────────────────────────

// A browser connected to B's room writes a decision block into a document of an issue A has just
// closed. A's close, and the sweep it runs once its rooms are closed, reach only A's rooms
// (SetIssueClosed), so B's room still takes the write: the block is stored, and the document owes a
// settlement no process runs while the issue is closed, so the block would read open, with no ask,
// for as long as the issue stays closed. One repair pass, run as the interval runs it - from
// another process on the same database, bounded to the documents marked owing a settlement since a
// moment before the close - gives the issue the close's final sweep: the block reads resolved in
// the durable document, and in B's room once B has reloaded it.
func TestTheRepairSweepResolvesADecisionBlockWrittenOnTheOtherProcessAfterTheClose(t *testing.T) {
	pair := startPair(t, nil)
	project := uniqueProjectKey(t, "SWP")
	pair.a.createProject(t, project, "Swept after the close")
	issueKey, artifactID := pair.a.createIssueIn(t, project, "Closed on A, written on B", "Context")

	editor := pair.b.openEditor(t, artifactID, "Context\n")
	// The tab's sync and the issue's creation each leave a settlement owed, which B and A settle
	// within their settle delay; the close is to find the document settled.
	waitForSettlementOwed(t, pair.database, artifactID, false)
	var markedSince time.Time
	if err := pair.database.Pool.QueryRow(context.Background(), `select statement_timestamp()`).Scan(&markedSince); err != nil {
		t.Fatalf("read the database's clock: %v", err)
	}
	pair.a.call(t, http.MethodPatch, "/api/v1/issues/"+issueKey, `{"status":"done"}`, http.StatusOK)
	if settlementOwed(t, pair.database, artifactID) {
		t.Fatal("A's close left the document owing a settlement, so this row could not tell B's write from it")
	}

	const block = ":::ask{#written-on-b urgency=\"low\" multiple=\"false\"}\nWritten on B after the close?\n:::\n"
	head := storedHead(t, pair, artifactID)
	appendBlock(t, editor, block)
	// B stores the tab's update, which marks the document owing a settlement again.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if storedHead(t, pair, artifactID) > head && settlementOwed(t, pair.database, artifactID) &&
			settlementMarkedAt(t, pair.database, artifactID).After(markedSince) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("B never stored its tab's block: stored head %d (was %d), settlement owed %v\nB:\n%s",
				storedHead(t, pair, artifactID), head, settlementOwed(t, pair.database, artifactID), pair.b.Output.String())
		}
	}
	const written = `:::ask{#written-on-b urgency="low" multiple="false" state="open"}`
	if text := pair.b.documentText(t, artifactID); !strings.Contains(text, written) {
		t.Fatalf("B's room does not hold the block its tab wrote:\n%s", text)
	}
	if text := durableText(t, pair, artifactID); !strings.Contains(text, written) {
		t.Fatalf("the durable document does not hold the block B's tab wrote:\n%s", text)
	}

	logs := captureDefaultLog(t)
	broker := events.NewBroker()
	documents := docs.New(docs.Deps{Store: pair.database, Events: broker})
	t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
	repair, err := api.RepairIssueAsks(store.WithTransactionTracking(context.Background()),
		api.Deps{Store: pair.database, Docs: documents, Events: broker}, &markedSince)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if repair.Swept != 1 || repair.Withdrawn != 0 || repair.Restored != 0 {
		t.Fatalf("repair = %+v, want the one block B's tab wrote swept", repair)
	}
	logged := logs.String()
	if !slices.ContainsFunc(strings.Split(logged, "\n"), func(line string) bool {
		return holdsAll(line, `msg="dispatch: repaired the asks of an issue"`, "issue="+issueKey, "swept=1")
	}) {
		t.Fatalf("the repair logged no line sweeping one block of %s:\n%s", issueKey, logged)
	}
	const resolved = `:::ask{#written-on-b urgency="low" multiple="false" state="resolved"}`
	if text := durableText(t, pair, artifactID); !strings.Contains(text, resolved) {
		t.Fatalf("the durable document after the repair:\n%s\nwant it to hold %s", text, resolved)
	}
	// The repair's write leaves B's room behind the durable head, which B closes; a reader then
	// reads the document B loads again.
	select {
	case <-editor.Ended:
	case <-time.After(30 * time.Second):
		t.Fatalf("B's room never closed behind the repair's write:\nB:\n%s", pair.b.Output.String())
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		text := pair.b.documentText(t, artifactID)
		if strings.Contains(text, resolved) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("B serves the document without the swept block after reloading it:\n%s", text)
		}
	}
}

// appendBlock appends markdown's blocks to the end of the editor's document in one keystroke's
// transaction, and sends the room the update it made.
func appendBlock(t *testing.T, editor *docstest.Peer, markdown string) {
	t.Helper()
	typed, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatalf("parse %q: %v", markdown, err)
	}
	fragment := editor.Doc.GetXmlFragment("prosemirror")
	var changeErr error
	if _, err := editor.Send(func(txn *crdt.Transaction) {
		tree, err := pmdoc.ReadInTransaction(txn, fragment)
		if err != nil {
			changeErr = err
			return
		}
		tree.Children = append(tree.Children, typed.Children...)
		changeErr = pmdoc.Update(txn, fragment, tree)
	}); err != nil || changeErr != nil {
		t.Fatalf("the editor's edit: %v %v", err, changeErr)
	}
}

// durableText is the document as its stored updates hold it: read by a document service of this
// test's own, which holds no room of it until this read loads one from the store.
func durableText(t *testing.T, pair *dispatchPair, artifactID string) string {
	t.Helper()
	reader := docs.New(docs.Deps{Store: pair.database})
	defer func() { _ = reader.Shutdown(context.Background()) }()
	text, err := reader.Text(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read the durable document %s: %v", artifactID, err)
	}
	return text
}

// captureDefaultLog records what the default logger writes until the test ends.
func captureDefaultLog(t *testing.T) *storetest.LockedLog {
	t.Helper()
	logs := &storetest.LockedLog{}
	logtest.SetDefault(t, slog.NewTextHandler(logs, nil))
	return logs
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

// Every maintenance loop that must run once overall takes a named lease for the length of its
// pass, so the second process's tick finds it held and skips instead of doing the same work
// beside it. Each loop observed here is held inside its own first statement while the other
// process's tick fires - by a table lock this test takes, or by the GitHub listing the fake
// holds - which is the only way two processes' passes are made to overlap on purpose rather than
// by luck.
//
// Five of the eleven leases are observable inside a window a test can wait out:
// reference-excerpts, block-index, search-term-stats and webhook-redelivery, whose first pass is
// immediate, and suggestion-outcomes, whose first is one SuggestionSweepInterval (a minute) in.
// The other six are not, and this row says so rather than pretending: document-compaction's
// interval is 24 h; issue-ask-repair's first leased pass is one IssueAskRepairInterval (10 min) in,
// the boot's repair taking no lease; architecture-import's and delivery-reconcile's first pass is
// jittered uniformly over SyncInterval/ReconcileInterval (5 min) and each process draws its own,
// so whether their passes ever overlap is a coin toss no test should turn into a flake;
// edit-notices' period is five seconds, so this window holds many of its periods, each served once
// (docs.TestEditNoticeSweepOfTwoServicesSendsEachNoticeOnce holds what its lease is for); and
// server-processes' prune is one delete on server_processes, the table every process's heartbeat
// writes every five seconds, so a lock that held the prune would hold the registry itself.
func TestEachLeasedMaintenanceLoopRunsOnceAcrossBothProcesses(t *testing.T) {
	pair := newPair(t)
	fake := startGitHubFake(t)
	// The term-statistics refresh writes nothing for a database with no project, so one exists
	// before either process starts.
	if _, err := pair.database.Pool.Exec(context.Background(), `insert into projects (key, name) values ('LEASE', 'Lease')`); err != nil {
		t.Fatalf("create the project the term-statistics refresh rebuilds: %v", err)
	}
	// refs is where the reference-excerpt reconciliation's first statement writes
	// (refs.MarkStaleExcerpts), write_suggestions where the suggestion-outcome sweep's does, and
	// search_term_totals where the term-statistics refresh's first write for a project goes (its
	// first statement only reads projects, which an exclusive lock does not hold).
	gate := lockTables(t, pair.database, "exclusive", "refs", "write_suggestions", "search_term_totals")
	// The block-index reconciliation's first statements read document_block_versions and
	// block_grammar_owed, which only access exclusive mode holds back. They are held in a gate of
	// their own, released as soon as the other process has found the lease held: that pass's
	// interval is 30 s, so released with the gate above - a minute in, when the suggestion sweep's
	// first tick lands - the other process's own next tick could land beside the release and make
	// the next period's pass before this row counts the first.
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

	// The suggestion-outcome sweep's first tick is a minute after each process started, so this
	// waits that out; its pass then waits on write_suggestions as the first one waits on refs.
	waitForLease(t, pair, leaseSkipped, "suggestion-outcomes", 3*time.Minute)
	if err := gate.Rollback(context.Background()); err != nil {
		t.Fatalf("release the tables the passes are waiting on: %v", err)
	}
	waitForLease(t, pair, leaseRan, "reference-excerpts", 60*time.Second)
	waitForLease(t, pair, leaseRan, "suggestion-outcomes", 60*time.Second)
	waitForLease(t, pair, leaseRan, "search-term-stats", 60*time.Second)
	requireLeasedOnce(t, pair, "reference-excerpts")
	requireLeasedOnce(t, pair, "suggestion-outcomes")
	requireLeasedOnce(t, pair, "search-term-stats")
	for _, unobserved := range []struct{ lease, why string }{
		{"document-compaction", "its interval is 24 h, so no pass is due inside any window a test can wait out"},
		{"issue-ask-repair", "its first leased pass is one IssueAskRepairInterval (10 min) after each process started"},
		{"architecture-import", "its first pass is jittered over SyncInterval (5 min) and each process draws its own"},
		{"delivery-reconcile", "its first pass is jittered over ReconcileInterval (5 min) and each process draws its own"},
		{"edit-notices", "its period is 5 s, so the window holds many periods, each served once"},
	} {
		if ran := countLease(pair.a, leaseRan, unobserved.lease) + countLease(pair.b, leaseRan, unobserved.lease); ran > 0 {
			t.Logf("%s ran %d times in the window, which this row does not hold to a count: %s",
				unobserved.lease, ran, unobserved.why)
			continue
		}
		t.Logf("%s was not observed: %s", unobserved.lease, unobserved.why)
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
