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
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/redeliver"
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

// multiprocessSigningKey is the one DISPATCH_SIGNING_KEY every process of a pair carries: the
// deployment rule INFRA-66 wrote down, since a session cookie minted by one process is presented
// to the other on the next request and nothing can tell a key the other process does not hold
// from a session that has ended.
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

// start starts both processes on the pair's database and NATS, each on a free port and in a home
// of its own, both carrying multiprocessSigningKey, and returns once each answers /healthz. The
// two are started together rather than one after the other: several rows are about what the two
// do when their loops fire at the same moment, which a staggered start would hide.
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

// ───── the processes' own logs ──────────────────────────────────────────────

// logLines is every line of the process's output that holds all of parts, which is how a slog
// text line is matched: the message and the attribute that narrows it.
func (p *dispatchProcess) logLines(parts ...string) []string {
	var matched []string
	for line := range strings.Lines(p.Output.String()) {
		if holdsAll(line, parts...) {
			matched = append(matched, strings.TrimRight(line, "\n"))
		}
	}
	return matched
}

func holdsAll(line string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(line, part) {
			return false
		}
	}
	return true
}

// logged reports whether the process has written a line holding all of parts.
func (p *dispatchProcess) logged(parts ...string) bool {
	return len(p.logLines(parts...)) > 0
}

// waitForLog waits up to within for a line holding all of parts, polling rather than sleeping a
// fixed time, and fails the test with the process's whole output when none arrives, at once when
// the process exits without it: a process that has exited writes no more lines.
func (p *dispatchProcess) waitForLog(t *testing.T, within time.Duration, parts ...string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !p.logged(parts...) {
		select {
		case <-p.Exited:
			// Exited closes only once the output is whole, so this read is final.
			if p.logged(parts...) {
				return
			}
			t.Fatalf("the process exited without a line holding %v:\n%s", parts, p.Output.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no line holding %v within %v:\n%s", parts, within, p.Output.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ───── server-sent events ───────────────────────────────────────────────────

// sseFrame is one server-sent event the stream handler wrote: its type and its data line. Its
// `id:` line names the event, which the data carries too, so nothing here keeps it.
type sseFrame struct {
	event string
	data  string
}

// openEventStream holds GET /api/v1/events open as a dashboard tab does and reports every frame
// it reads on the returned channel, until the test ends or the connection does. One request, read
// to the end: a row about a live stream carrying the other process's event has to show that event
// arriving on the connection the reader already had, never on a reconnect's catch-up.
func (p *dispatchProcess) openEventStream(t *testing.T) <-chan sseFrame {
	t.Helper()
	return p.openServerSentEvents(t, "/api/v1/events")
}

// openServerSentEvents holds the server-sent stream at path open as a signed-in tab does and
// reports every frame it reads on the returned channel, which closes once the connection ends.
func (p *dispatchProcess) openServerSentEvents(t *testing.T, path string) <-chan sseFrame {
	t.Helper()
	response, err := p.send(http.MethodGet, path, "")
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		t.Fatalf("open %s: status %d: %s", path, response.StatusCode, body)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	frames := make(chan sseFrame, 256)
	go func() {
		defer close(frames)
		reader := bufio.NewReader(response.Body)
		var frame sseFrame
		for {
			line, err := reader.ReadString('\n')
			switch trimmed := strings.TrimRight(line, "\r\n"); {
			case trimmed == "":
				if frame.data != "" {
					frames <- frame
				}
				frame = sseFrame{}
			case strings.HasPrefix(trimmed, "event: "):
				frame.event = strings.TrimPrefix(trimmed, "event: ")
			case strings.HasPrefix(trimmed, "data: "):
				frame.data = strings.TrimPrefix(trimmed, "data: ")
			}
			if err != nil {
				return
			}
		}
	}()
	return frames
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

// ───── a live stream carries the other process's events ─────────────────────

// A dashboard tab is open on one process and the work happens on the other: the event of an issue
// created through B's API has to reach the stream A is holding, on the connection it already has.
// The live source of an open stream is no longer this process's in-memory broker, which hears
// only its own writes, but a relay over `listen dispatch_events`, so what either process commits
// every process publishes.
func TestALiveStreamOnOneProcessCarriesTheOtherProcessesEvent(t *testing.T) {
	pair := startPair(t, nil)
	project := uniqueProjectKey(t, "XPR")
	pair.a.createProject(t, project, "Cross process")

	frames := pair.a.openEventStream(t)
	key, _ := pair.b.createIssueIn(t, project, "Created on the other process", "spec")

	deadline := time.After(30 * time.Second)
	for {
		select {
		case frame, open := <-frames:
			if !open {
				t.Fatalf("the stream A held closed before B's event arrived:\n%s", pair.a.Output.String())
			}
			if frame.event == "issue.created" && strings.Contains(frame.data, `"`+key+`"`) {
				return
			}
		case <-deadline:
			t.Fatalf("the stream A held never carried the issue B created (%s):\nA:\n%s\nB:\n%s",
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

// readStreamFrom is every envelope the stream retains past head, read by sequence rather than
// through a consumer so the read creates nothing on the server and cannot miss a message a
// consumer's filter would skip. A sequence the stream no longer holds is passed over.
func readStreamFrom(t *testing.T, js natsgo.JetStreamContext, head uint64) []contracts.Envelope {
	t.Helper()
	info, err := js.StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the notification stream: %v", err)
	}
	var items []contracts.Envelope
	for seq := head + 1; seq <= info.State.LastSeq; seq++ {
		msg, err := js.GetMsg(bus.Stream, seq)
		if err != nil {
			if errors.Is(err, natsgo.ErrMsgNotFound) {
				continue
			}
			t.Fatalf("read message %d of the notification stream: %v", seq, err)
		}
		var item contracts.Envelope
		if err := json.Unmarshal(msg.Data, &item); err != nil {
			t.Fatalf("decode message %d of the notification stream: %v", seq, err)
		}
		items = append(items, item)
	}
	return items
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

// deliveryDurables are the two push durables the delivery intakes bind, one subscriber each.
var deliveryDurables = []string{"delivery-events", "delivery-jobs"}

// seedDeliverySettings writes the one delivery_settings row, the same write
// PUT /api/v1/settings/delivery makes (delivery.PutSettings). The intakes read it on their first
// poll and bind nothing until it is there, so a row about the durables seeds it before the
// processes start rather than driving the route, which would first have the GitHub App prove it
// can read the repository.
func seedDeliverySettings(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := delivery.PutSettings(context.Background(), database.Pool, delivery.DeliverySettings{
		DeployRepo:           "acme/deploy",
		DeployWorkflowPath:   ".github/workflows/deploy.yaml",
		PRChecksWorkflowPath: ".github/workflows/pr.yaml",
		ProductionJobNames:   []string{"production"},
	}, model.Actor{Kind: "user", ID: dispatchTestLogin}); err != nil {
		t.Fatalf("seed the delivery settings: %v", err)
	}
}

// githubAppEnv is a GitHub App a Dispatch process boots with: a throwaway RSA key generated for
// this test, and apiBase as the only origin its App JWT is ever sent to. Without an App the
// delivery intakes never connect and the webhook redelivery sweep never runs, so a row about
// either needs these rows.
func githubAppEnv(t *testing.T, apiBase string) map[string]string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate the test App key: %v", err)
	}
	encoded := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return map[string]string{
		"DISPATCH_APP_CLIENT_ID":   "Iv1.multiprocess",
		"DISPATCH_APP_ID":          "4242",
		"DISPATCH_APP_SLUG":        "multiprocess",
		"DISPATCH_APP_PEM_B64":     base64.StdEncoding.EncodeToString(encoded),
		"DISPATCH_GITHUB_API_BASE": apiBase,
	}
}

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

// lockTables holds every table named in exclusive mode in a transaction of the test's own, as a
// writer that is taking its time would, and rolls it back when the test ends if the test has not.
// A maintenance pass whose first statement writes one of them waits there, inside its lease, which
// is the moment the lease is about: both processes' loops firing while one is still in its pass.
func lockTables(t *testing.T, database *store.Store, tables ...string) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	gate, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("open the transaction holding %v: %v", tables, err)
	}
	t.Cleanup(func() { _ = gate.Rollback(context.Background()) })
	for _, table := range tables {
		if _, err := gate.Exec(ctx, "lock table "+table+" in exclusive mode"); err != nil {
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
// Three of the six leases are observable inside a window a test can wait out:
// reference-excerpts and webhook-redelivery, whose first pass is immediate, and
// suggestion-outcomes, whose first is one SuggestionSweepInterval (a minute) in. The other three
// are not, and this row says so rather than pretending: document-compaction's ticker is 24 h;
// architecture-import's and delivery-reconcile's first pass is jittered uniformly over
// SyncInterval/ReconcileInterval (5 min) and each process draws its own, so whether their passes
// ever overlap is a coin toss no test should turn into a flake.
func TestEachLeasedMaintenanceLoopRunsOnceAcrossBothProcesses(t *testing.T) {
	pair := newPair(t)
	fake := startGitHubFake(t)
	// refs is where the reference-excerpt reconciliation's first statement writes
	// (refs.MarkStaleExcerpts), write_suggestions where the suggestion-outcome sweep's does.
	gate := lockTables(t, pair.database, "refs", "write_suggestions")
	pair.start(t, githubAppEnv(t, fake.URL()))

	if waiting := storetest.WaitForLockWaiters(t, gate, 1, 60*time.Second); waiting < 1 {
		t.Fatalf("no reference-excerpt pass reached the refs lock, so neither process took the lease:\nA:\n%s\nB:\n%s",
			pair.a.Output.String(), pair.b.Output.String())
	}
	waitForLease(t, pair, leaseSkipped, "reference-excerpts", 60*time.Second)
	if ran := countLease(pair.a, leaseRan, "reference-excerpts") + countLease(pair.b, leaseRan, "reference-excerpts"); ran != 0 {
		t.Errorf("a reference-excerpt pass reported running %d times while the one holding the lease is still inside the refs lock", ran)
	}

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
	requireLeasedOnce(t, pair, "reference-excerpts")
	requireLeasedOnce(t, pair, "suggestion-outcomes")
	for _, unobserved := range []struct{ lease, why string }{
		{"document-compaction", "its ticker is 24 h, so no pass is due inside any window a test can wait out"},
		{"architecture-import", "its first pass is jittered over SyncInterval (5 min) and each process draws its own"},
		{"delivery-reconcile", "its first pass is jittered over ReconcileInterval (5 min) and each process draws its own"},
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
// replicas each process keeps a ticker of its own: two ticks of one leased loop almost never
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
// A period is a bucket of the database's clock (duty.Exclusive, migration 0115), so both
// processes are started inside one bucket on purpose: a second process whose first tick lands
// past a bucket boundary is entitled to sweep, and this row is about the one that lands inside
// the bucket its peer has already served. waitOutABucketBoundary is what keeps which of the two
// this run gets from being luck.
func TestALeasedLoopRunsOnceAcrossAStaggeredStart(t *testing.T) {
	pair := newPair(t)
	waitOutABucketBoundary(t, pair, redeliver.Interval)
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
// both processes and read their first ticks inside it. A period is floor(epoch / period) on the
// database's clock, so a row that straddles a boundary watches a second process legitimately
// sweep rather than skip; this waits the boundary out instead, on the same clock the gate reads,
// and waits at most the margin it needs.
func waitOutABucketBoundary(t *testing.T, pair *dispatchPair, period time.Duration) {
	t.Helper()
	// What one run of the row needs inside the bucket: two process starts and their first ticks.
	const margin = 45 * time.Second
	var left time.Duration
	if err := pair.database.Pool.QueryRow(context.Background(),
		`select make_interval(secs => $1 - mod(extract(epoch from now()), $1))`, period.Seconds(),
	).Scan(&left); err != nil {
		t.Fatalf("read how much of the period's bucket is left: %v", err)
	}
	if left >= margin {
		return
	}
	t.Logf("waiting %v for the next bucket of the %v period, so both processes tick inside one", left, period)
	time.Sleep(left + time.Second)
}

// ───── one signing key across the processes ────────────────────────────────

// The sign-in client the fake issuer serves, and the group it admits.
const (
	cookieClientID     = "dispatch-multiprocess-client"
	cookieClientSecret = "dispatch-multiprocess-secret"
	cookieGroup        = "dispatch-members"
	cookiePerson       = "alice@example.test"
)

// A person signs in on one process and their next request lands on the other, which is what a
// load balancer in front of two tasks does on every page load. The session itself is in Postgres,
// which both read; what each process needs of its own is DISPATCH_SIGNING_KEY, and nothing in
// Dispatch can tell a cookie signed with another key from a session that has ended - the third
// process here, carrying a key of its own, answers the same 401 either way.
func TestASessionMintedOnOneProcessAuthenticatesOnTheOtherUnderOneSigningKey(t *testing.T) {
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), cookieClientID, cookieClientSecret)
	signIn := map[string]string{
		"DISPATCH_IDENTITY":             "cookie",
		"DISPATCH_SIGNIN_ISSUER":        issuer.URL(),
		"DISPATCH_SIGNIN_CLIENT_ID":     cookieClientID,
		"DISPATCH_SIGNIN_CLIENT_SECRET": cookieClientSecret,
		"DISPATCH_SIGNIN_GROUP":         cookieGroup,
		"DISPATCH_INSECURE_COOKIE":      "1",
	}
	pair := newPair(t)
	pair.start(t, signIn)
	// The negative control: a third process on the same database and the same sign-in issuer,
	// whose signing key is its own, as a deployment that rolled one task with a new secret has.
	odd := startDispatchProcessWith(t, pair.databaseURL(t), dispatchOptions{
		NATSURL: pair.nats, SigningKey: "a-key-of-its-own", Env: signIn,
	})
	odd.waitHealthy(t)

	session := signInThrough(t, pair.a, issuer)
	for _, target := range []struct {
		name    string
		process *dispatchProcess
		status  int
	}{
		{"the process it was minted on", pair.a, http.StatusOK},
		{"the other process of the pair", pair.b, http.StatusOK},
		{"a process carrying a key of its own", odd, http.StatusUnauthorized},
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

// signInThrough signs cookiePerson in on the process, the whole code flow a browser walks:
// /auth/start for the issuer's authorization URL and the state cookie, the issuer's own sign-in,
// then /auth/callback, which answers the session cookie. It returns that cookie.
func signInThrough(t *testing.T, p *dispatchProcess, issuer *oidctest.Issuer) *http.Cookie {
	t.Helper()
	issuer.SignInAs(map[string]any{
		"sub":              "alice-subject",
		"cognito:username": "ExampleIdP_" + cookiePerson,
		"cognito:groups":   []string{cookieGroup},
		"identities":       []map[string]any{{"providerName": "ExampleIdP"}},
	})
	started := browserGet(t, p.url("/auth/start"), nil)
	defer started.Body.Close()
	if started.StatusCode != http.StatusFound {
		t.Fatalf("/auth/start answered %d, want a redirect to the issuer", started.StatusCode)
	}
	var state *http.Cookie
	for _, cookie := range started.Cookies() {
		if cookie.Name == "dispatch_oauth_state" {
			state = cookie
		}
	}
	if state == nil {
		t.Fatalf("/auth/start set no sign-in state cookie: %v", started.Cookies())
	}
	code, returned := issuer.Authorize(t, started.Header.Get("Location"))
	callback := browserGet(t, p.url("/auth/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(returned)), state)
	defer callback.Body.Close()
	for _, cookie := range callback.Cookies() {
		if cookie.Name == "dsession" && cookie.Value != "" {
			return cookie
		}
	}
	body, _ := io.ReadAll(callback.Body)
	t.Fatalf("/auth/callback answered %d and set no session cookie: %s", callback.StatusCode, body)
	return nil
}

// browserGet is one GET a browser makes during a sign-in: it sends cookie when there is one and
// never follows the redirect, since the redirect's own response is where the cookie is set.
func browserGet(t *testing.T, target string, cookie *http.Cookie) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("build %s: %v", target, err)
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	return response
}

// whoami asks the process who the session cookie names, and answers its body and status.
func whoami(t *testing.T, p *dispatchProcess, session *http.Cookie) (string, int) {
	t.Helper()
	response := browserGet(t, p.url("/auth/whoami"), session)
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return strings.TrimSpace(string(body)), response.StatusCode
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
// listener's URL, and the interval its resume loop sends stranded attempts on.
func (f *fakeListener) processEnv(resumeInterval time.Duration) map[string]string {
	return map[string]string{
		"ENVOY_URL":                              f.server.URL,
		"DISPATCH_TEST_DELIVERY_RESUME_INTERVAL": resumeInterval.String(),
	}
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
	go func() {
		if response, err := p.send(method, path, body); err == nil {
			_ = response.Body.Close()
		}
	}()
}

// kill ends the process the way a task replaced mid-deploy or killed for memory ends, with no
// chance to finish anything, and returns once it has exited.
func (p *dispatchProcess) kill(t *testing.T) {
	t.Helper()
	if err := p.Cmd.Process.Kill(); err != nil {
		t.Fatalf("kill the process: %v", err)
	}
	<-p.Exited
}

// backDatePendingAttempts moves every pending attempt's claim and creation two minutes back, past
// the one-minute claim lease the resume loop waits out, so a row does not wait a real minute.
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

// A process killed while it delivers leaves its attempts pending: a direct message and a comment
// mention whose own request was sending them, and a broadcast recipient a background worker was
// sending. The other process's resume loop sends each one once its claim has lapsed, under the
// attempt and the idempotency key the killed process used, so the session gets each once. The
// subtest kills the process earlier, after the message is stored and before anything is resolved
// or claimed: the attempt the storing transaction wrote is what the survivor sends.
func TestADeliveryAKilledProcessLeftPendingIsSentByTheOther(t *testing.T) {
	t.Run("killed while sending", func(t *testing.T) {
		listener := startFakeListener(t, resumeSession)
		sends := listener.hold(listenerSend)
		pair := startPair(t, listener.processEnv(time.Second))
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
			`{"body":"A broadcast","delivery":"steer","session_ids":[%q],"idempotency_key":"killed-while-sending"}`,
			resumeSession.SessionID), http.StatusCreated)
		broadcast := sends.next(t, 30*time.Second, "the broadcast recipient's send")

		pair.a.kill(t)
		requireAbandoned(t, direct, mention, broadcast)
		sends.Release()
		backDatePendingAttempts(t, pair.database)
		requireDeliveredOnce(t, pair, listener, 30*time.Second,
			messageDelivery("the direct message", direct.subject),
			mentionDelivery("the mention", mention.subject),
			messageDelivery("the broadcast recipient's message", broadcast.subject),
		)
	})

	t.Run("killed before the claim", func(t *testing.T) {
		listener := startFakeListener(t, resumeSession)
		lookups := listener.hold(listenerRegistry)
		pair := startPair(t, listener.processEnv(time.Second))
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
		requireDeliveredOnce(t, pair, listener, 30*time.Second,
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
// and read sent, with no Retry. The resume loop is an hour out, so nothing but the request's own
// send can deliver them.
func TestASendWhoseRequestIsCancelledAfterItsCommitReachesItsRecipient(t *testing.T) {
	listener := startFakeListener(t, resumeSession)
	pair := startPair(t, listener.processEnv(time.Hour))
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
// process's resume loop sends.
func TestASendDuringAListenerTakeOverReachesItsSession(t *testing.T) {
	t.Run("sent during the take-over", func(t *testing.T) {
		listener := startFakeListener(t, resumeSession)
		// The resume loop is an hour out, so only each request's own sender can deliver.
		pair := startPair(t, listener.processEnv(time.Hour))
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
		pair := startPair(t, listener.processEnv(time.Hour))
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
		pair := startPair(t, listener.processEnv(time.Second))
		project := uniqueProjectKey(t, "TKK")
		pair.a.createProject(t, project, "Killed during a take-over")
		issue, _ := pair.a.createIssueIn(t, project, "Mentioned, then killed", "spec")

		listener.takeOver(time.Hour, 0, 0, 0)
		direct, mention := sendDuringTakeOver(t, pair.a, pair.database, issue, "killed during a take-over")
		requireWaitingOutTheTakeOver(t, pair.database, listener, direct, mention)
		pair.a.kill(t)
		listener.endTakeOver()
		backDatePendingAttempts(t, pair.database)
		requireDeliveredOnce(t, pair, listener, 30*time.Second, direct, mention)
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
	state, err := redeliver.OpenState(js)
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
	response, err := p.send(http.MethodGet, path, "")
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer response.Body.Close()
	var body struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(response.Body).Decode(&body)
	return response.StatusCode, response.Header.Get("Retry-After"), body.Code
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
		response, err := pair.a.send(http.MethodPost, path, "")
		if err != nil {
			held <- answer{err: err}
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		held <- answer{status: response.StatusCode, body: body, err: err}
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
// ENVOY_URL, the resume loop an hour out (no row here sends anything), and a listener answer that
// serves issue reads for a minute (DISPATCH_TEST_ROUTE_ROSTER_TTL, under the test hooks every
// process here runs with), so the row reads both processes inside one answer's life however slow
// the machine, with no timing window.
func rosterEnv(listener *fakeListener) map[string]string {
	env := listener.processEnv(time.Hour)
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

// openDeliverySocket opens GET /ws/delivery as a signed-in Delivery page does and reports the
// read error that ended it on the returned channel.
func (p *dispatchProcess) openDeliverySocket(t *testing.T) <-chan error {
	t.Helper()
	conn, response, err := gws.DefaultDialer.Dial("ws"+strings.TrimPrefix(p.url("/ws/delivery"), "http"),
		http.Header{"X-Dispatch-User": {dispatchTestLogin}})
	if err != nil {
		t.Fatalf("open the delivery socket: %v (%v)", err, response)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ended := make(chan error, 1)
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				ended <- err
				return
			}
		}
	}()
	return ended
}

// requireStreamEnded drains frames until the stream closes, failing the row when it is still
// open within.
func requireStreamEnded(t *testing.T, frames <-chan sseFrame, within time.Duration, what string) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case _, open := <-frames:
			if !open {
				return
			}
		case <-deadline:
			t.Fatalf("%s was still open %v later", what, within)
		}
	}
}

// Every test hook acts on every process serving the database, whichever process the call landed
// on: a browser suite against two processes disconnects, quiesces and feeds frames to the process
// its page happens to be connected to, which is not the one its own API call reaches. A stream, a
// delivery socket, a live document room and a conversation viewer are each held open on B, and
// each hook is called on A. quiesce answers only once B's room has closed too, which is what a
// harness truncating its database right after relies on. Once B is killed its lock is gone with
// its connection, and a hook called on A answers on A's acknowledgement alone rather than waiting
// out the timeout for a process that no longer exists.
func TestEveryTestHookActsOnBothProcesses(t *testing.T) {
	pair := startHookPair(t)
	project := uniqueProjectKey(t, "HKS")
	pair.a.createProject(t, project, "Test hooks")
	_, artifactID := pair.a.createIssueIn(t, project, "Open on B", "before")

	stream := pair.b.openEventStream(t)
	pair.a.call(t, http.MethodPost, "/api/v1/events/_test/disconnect", "", http.StatusOK)
	requireStreamEnded(t, stream, 10*time.Second, "the event stream B held after the disconnect hook was called on A")

	socket := pair.b.openDeliverySocket(t)
	pair.a.call(t, http.MethodPost, "/api/v1/delivery/_test/disconnect", "", http.StatusOK)
	select {
	case err := <-socket:
		if !gws.IsCloseError(err, gws.CloseServiceRestart) {
			t.Fatalf("the delivery socket B held ended with %v, want the hook's 1012", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the delivery socket B held was still open 10s after the delivery disconnect hook was called on A")
	}

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

	viewer := pair.b.openServerSentEvents(t, "/api/v1/agents/"+hookViewerSession+"/stream")
	const published = `{"v":1,"kind":"message","seq":7}`
	pair.a.call(t, http.MethodPost, "/api/v1/agents/"+hookViewerSession+"/stream/_test/publish", published, http.StatusOK)
	deadline := time.After(10 * time.Second)
	for received := false; !received; {
		select {
		case frame, open := <-viewer:
			if !open {
				t.Fatal("the conversation viewer B held closed before the frame published on A arrived")
			}
			received = frame.event == "frame" && frame.data == published
		case <-deadline:
			t.Fatal("the conversation viewer B held never received the frame published on A")
		}
	}

	survivor := pair.a.openEventStream(t)
	pair.b.kill(t)
	called := time.Now()
	pair.a.call(t, http.MethodPost, "/api/v1/events/_test/disconnect", "", http.StatusOK)
	waited := time.Since(called)
	t.Logf("the disconnect hook on A answered %v after B was killed", waited)
	// The hook's timeout is ten seconds; an answer inside it is one that did not wait for B.
	if waited >= 10*time.Second {
		t.Fatalf("the disconnect hook on A answered %v after B was killed, want well inside the 10s timeout", waited)
	}
	requireStreamEnded(t, survivor, 10*time.Second, "the event stream A held after the disconnect hook answered with B killed")
}
