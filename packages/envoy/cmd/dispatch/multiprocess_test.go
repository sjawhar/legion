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
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
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
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/docs/crafted"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/redeliver"
	"github.com/sjawhar/envoy/internal/dispatch/routes"
	"github.com/sjawhar/envoy/internal/dispatch/startup"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
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

// dispatchPair is two envoy-dispatch processes on one database and one NATS server.
type dispatchPair struct {
	a, b     *dispatchProcess
	database *store.Store
	nats     string
}

// newPair opens the database and takes the NATS server the pair will share, and starts nothing:
// a row that must seed durable state the processes read at boot (the delivery settings their
// intakes wait for) or hold a lock their first pass will wait on does that between this and
// start.
func newPair(t *testing.T) *dispatchPair {
	t.Helper()
	return &dispatchPair{database: storetest.Open(t), nats: multiprocessNATS(t)}
}

// start starts both processes on the pair's database and NATS, each on a port held for it from the
// moment the kernel chose it (startDispatchProcessWith) and in a home of its own, both carrying
// multiprocessSigningKey, and returns once each answers /healthz. The two are started together
// rather than one after the other: several rows are about what the two do when their loops fire at
// the same moment, which a staggered start would hide.
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
	options := dispatchOptions{NATSURL: p.nats, SigningKey: multiprocessSigningKey, Env: env}
	optionsA, optionsB := options, options
	optionsA.SettleDelay, optionsB.SettleDelay = settleA, settleB
	p.a = startDispatchProcessWith(t, p.databaseURL(t), optionsA)
	p.b = startDispatchProcessWith(t, p.databaseURL(t), optionsB)
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

// ───── the outbox ───────────────────────────────────────────────────────────

// outboxRoleLane is the role the outbox row's issue is routed to: its events are published on
// notifications.role.<lane>, which travels over core NATS with no message id, so the stream's
// duplicate window hides nothing there. It is the lane a second publish of one event shows up on.
const outboxRoleLane = "multiprocess-outbox"

// Both processes' outboxes scan one events table. Each claims its batch under
// `for update skip locked` before it publishes, so one event is published once however many
// processes are scanning: every row reaches published_at, the stream holds one retained message
// per (dedupe key, topic), and the role lane - core NATS, no message id, no deduplication
// anywhere - carries exactly one message per event.
func TestTheOutboxPublishesEachEventOnceAcrossBothProcesses(t *testing.T) {
	pair := startPair(t, nil)
	conn := testnats.Connect(t, pair.nats)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("JetStream context: %v", err)
	}
	// This row counts only what it publishes itself: readStreamFrom starts past this head.
	head := streamHead(t, js)

	lane := make(chan contracts.Envelope, 256)
	laneSub, err := conn.Subscribe(contracts.RoleTopicPrefix+outboxRoleLane, func(msg *natsgo.Msg) {
		var item contracts.Envelope
		if err := json.Unmarshal(msg.Data, &item); err == nil {
			lane <- item
		}
	})
	if err != nil {
		t.Fatalf("subscribe to the role lane: %v", err)
	}
	t.Cleanup(func() { _ = laneSub.Unsubscribe() })

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

	// The role lane's messages are read after every event is marked published, plus a settling
	// read: a second publish of an already published event would arrive after the mark, and
	// this is where it would be seen.
	received := drainEnvelopes(t, lane, 3*time.Second)
	perEvent := map[string]int{}
	for _, item := range received {
		perEvent[item.SourceEventID]++
	}
	if len(perEvent) < comments {
		t.Errorf("the role lane carried %d events, want at least the %d comments:\n%v", len(perEvent), comments, perEvent)
	}
	for eventID, count := range perEvent {
		if count != 1 {
			t.Errorf("event %s reached the role lane %d times; two processes publishing one event is what the claim prevents:\n%v",
				eventID, count, perEvent)
		}
	}

	perMessage := map[string]int{}
	for _, item := range readStreamFrom(t, js, head) {
		perMessage[item.DedupeKey+" on "+item.Topic]++
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

// waitForEveryEventPublished waits for every event row the outbox takes to carry published_at.
// A row whose payload says `pending` is not one the outbox claims (claimPendingEvents' own
// predicate), so it is not waited for.
func waitForEveryEventPublished(t *testing.T, database *store.Store, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		var unpublished int
		if err := database.Pool.QueryRow(context.Background(), `
			select count(*) from events
			where published_at is null and coalesce(payload ->> 'pending', 'false') <> 'true'
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
// Five of the twelve leases are observable inside a window a test can wait out:
// reference-excerpts, block-index, search-term-stats and webhook-redelivery, whose first pass is
// immediate, and suggestion-outcomes, whose first is one SuggestionSweepInterval (a minute) in.
// The other seven are not, and this row says so rather than pretending: document-compaction's
// interval is 24 h; issue-ask-repair's first leased pass is one IssueAskRepairInterval (10 min) in,
// the boot's repair taking no lease; architecture-import's and delivery-reconcile's first pass is
// jittered uniformly over SyncInterval/ReconcileInterval (5 min) and each process draws its own,
// so whether their passes ever overlap is a coin toss no test should turn into a flake;
// edit-notices' period is five seconds and settlement-resumption's a minute, and each pass is over
// in milliseconds, so this window holds many of their periods, each served once
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
		{"settlement-resumption", "its period is SettlementResumeInterval (1 min), so the window holds several periods, each served once"},
		{"server-processes", "its period is processes.PruneInterval (1 min), so the window holds several periods, each served once"},
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
// come through the agent token (dispatchAgentToken).
const multiprocessAgentSession = "multiprocess-agent"

// asAgent is the header an agent's request carries: the agent token and no person.
func asAgent() http.Header {
	return http.Header{"Authorization": {"Bearer " + dispatchAgentToken}}
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
	deadline := time.Now().Add(within)
	for storedHead(t, p, artifactID) <= head {
		if time.Now().After(deadline) {
			t.Fatalf("%s was not stored within %v: the document still stands at update %d", what, within, head)
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
			editor, err := typing.process.reconnectEditor(t, typing.login, artifactID)
			if err != nil {
				return fmt.Errorf("%s: %w", typing.who, err)
			}
			typing.editor = editor
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
		typists[0].editor = pair.a.openEditorAs(t, "alice", artifactID, "x\n")
		typists[1].editor = pair.b.openEditorAs(t, "bob", artifactID, "x\n")
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
		typists[0].editor = pair.a.openEditorAs(t, "alice", artifactID, "x\n")
		typists[1].editor = pair.b.openEditorAs(t, "bob", artifactID, "x\n")
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
	}{{"alice on A", alice, "a"}, {"bob on B", bob, "b"}} {
		head := storedHead(t, pair, artifactID)
		if err := typeAtEnd(edit.editor, edit.text); err != nil {
			t.Fatalf("%s's keystroke on the reopened issue: %v", edit.who, err)
		}
		pair.waitStoredPast(t, artifactID, head, 5*time.Second, edit.who+"'s keystroke on the reopened issue")
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
	head := storedHead(t, pair, artifactID)
	typed := time.Now()
	if err := typeAtEnd(bob, " late"); err != nil {
		t.Fatalf("bob's keystroke before the close: %v", err)
	}
	pair.waitStoredPast(t, artifactID, head, 5*time.Second, "bob's keystroke before the close")
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
	head = storedHead(t, pair, artifactID)
	if err := typeAtEnd(bob, " again"); err != nil {
		t.Fatalf("bob's keystroke on the reopened issue: %v", err)
	}
	pair.waitStoredPast(t, artifactID, head, 5*time.Second, "bob's keystroke on the reopened issue")
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
