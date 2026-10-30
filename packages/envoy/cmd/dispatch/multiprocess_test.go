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
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
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
	options := dispatchOptions{NATSURL: p.nats, SigningKey: multiprocessSigningKey, Env: env}
	p.a = startDispatchProcessWith(t, p.databaseURL(t), options)
	p.b = startDispatchProcessWith(t, p.databaseURL(t), options)
	p.a.waitHealthy(t)
	p.b.waitHealthy(t)
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

// The apply the component cannot refuse and the README therefore states as the procedure's first
// precondition: [0] → [1] in one step, which renders generation 1's key alone while the live
// tasks still sign with generation 0's. Each key set lacks the key the other signs with, so the
// registry refuses the new process at its boot (processes.Register) and the rollout stalls on a
// pod that never serves, rather than the fleet serving under two disjoint keys and signing people
// out of whichever task their next request reaches. The pair carries on serving the person signed
// in before the attempt, which is why a stalled rollout is the better of the two outcomes.
func TestAStagedKeyRotationThatSkipsTheAddApplyIsRefusedAtBoot(t *testing.T) {
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), cookieClientID, cookieClientSecret)
	pair := newPair(t)
	// All three processes run the same unstamped build and differ only in their setting, so the
	// binary is built once.
	options := dispatchOptions{
		NATSURL: pair.nats, SigningKey: stagedGeneration0, Env: cookieSignIn(issuer),
		Binary: cmdtest.Build(t, "envoy-dispatch"),
	}
	pair.a = startDispatchProcessWith(t, pair.databaseURL(t), options)
	pair.b = startDispatchProcessWith(t, pair.databaseURL(t), options)
	pair.a.waitHealthy(t)
	pair.b.waitHealthy(t)
	session := signInThrough(t, pair.a, issuer)

	options.SigningKey = stagedGeneration1
	skipped := startDispatchProcessWith(t, pair.databaseURL(t), options)
	requireRefusedToStart(t, skipped, "the first pod of an apply that moved straight from [0] to [1]")
	requireWhoami(t, pair, pair.a, session, "the cookie A minted, once the skipped apply's pod was refused")
	requireWhoami(t, pair, pair.b, session, "the cookie A minted, once the skipped apply's pod was refused")
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
