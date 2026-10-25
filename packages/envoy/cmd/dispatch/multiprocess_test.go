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
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
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
	response, err := p.send(http.MethodGet, "/api/v1/events", "")
	if err != nil {
		t.Fatalf("open the event stream: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		t.Fatalf("open the event stream: status %d: %s", response.StatusCode, body)
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

	editor := pair.a.openEditor(t, artifactID, "before\n")
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

// writeThroughRetrying503s makes one edit through the process's own API, repeating a 503 as a
// client is told to. A write whose own room is failed or reloading is answered
// 503 DOC_SERVICE_UNAVAILABLE, and the contract is that the caller repeats the same call
// (skills/dispatch/references/documents.md, and the pull request's contract census). A rig that
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
func burstLeavingTheRoomBehind(t *testing.T, pair *dispatchPair, artifactID string, beforeReload func()) (*docstest.Peer, time.Time) {
	t.Helper()
	editor := pair.a.openEditor(t, artifactID, "before\n")
	writeThroughRetrying503s(t, pair.b, artifactID, "before", "middle")
	var evicted time.Time
	select {
	case <-editor.Ended:
		// Where the interval's window starts, as closely as this side can see it: Evict closes
		// the room - which is what ends this socket - and the stamp the interval reads is taken
		// as that call returns (recordEviction, docs/stale_room.go). This stamp is on the test's
		// side of that close, so it is the later of the two, and the gap measured at the end of
		// this helper reads shorter than the window the write really spent, by the close's trip
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
	// The eviction's own line is written after the room has closed, so it is waited for rather
	// than read the moment the socket went.
	pair.a.waitForLog(t, 10*time.Second,
		`msg="dispatch: closed a document room another process's write left behind"`, "room="+artifactID)
	if evictions := evictionsOf(pair.a, artifactID); evictions != 1 {
		t.Fatalf("want one eviction after the first write, got %d", evictions)
	}
	if beforeReload != nil {
		beforeReload()
	}
	reconnected := pair.a.openEditor(t, artifactID, "middle\n")
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
// socket went, not who ended it, and both rows below have another path that ends it - the
// settlement rescue's failRoomLocked for the room that owes one, an unload for either - so the
// close is the window's only if the evictor closed that room a second time. The first eviction
// is asserted in burstLeavingTheRoomBehind, so the count here is two. The line is written after
// the room has closed, so it is waited for rather than counted the moment the socket went; ten
// seconds is the budget the eviction's other waits in this file use, and it is spent only by a
// row that is already failing.
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
			t.Fatalf("%s: the room closed but the evictor has closed it %d time(s) in %v, not the two this burst leaves, so what closed it is some other path - the settlement rescue's failRoomLocked, or an unload - and not the window of the notification the interval refused.\nThis room's evictions, then the closes and the head reads that failed:\n%v\n%v\n%v\nThe process's output:\n%s",
				what, evictions, budget,
				p.logLines("closed a document room another process's write left behind", "room="+artifactID),
				p.logLines("close a document room another process's write left behind", "room="+artifactID),
				p.logLines("read the head of a document that changed", "room="+artifactID),
				p.Output.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// refusedWindowClosesWithin bounds the row below between the two paths that could close its
// room, since a bound above both would be green on either. The window's own re-examination
// closes it a little under two seconds after the trailing write - what the write left of the
// interval, plus the close - which was 1.9486 s in this row's green run here and 1.88 s when it
// was built. The settlement rescue - three refusals two seconds apart, maxSettleFailures of
// them, ending in failRoomLocked - took 6.018 s in the control run with the wake unwired.
//
// Both of those figures are taken from the trailing write; this budget is not counted from
// there. It is spent by requireTheRoomClosed's own time.After, which starts when that helper is
// entered: after burstLeavingTheRoomBehind returned, so after the trailing write's answer came
// back, and after the row's settlementOwedFor query. The wrote stamp the helper is also passed
// only labels its log line. That origin is later than wrote by the trailing write's own
// duration plus that one query, which is inside the 91-164 ms the helper logs for the whole
// stretch from the first eviction, the write included. So in the terms the two figures above
// use, the deadline falls at about 4.1 s: some 2.2 s past the window's close and some 1.9 s
// short of the rescue. This row still goes red when the wake is gone instead of green on the
// rescue's clock.
const refusedWindowClosesWithin = 4 * time.Second

// The deferred re-examination, in the branch where something else could also have rescued the
// room. A notification the eviction interval refuses is remembered and re-examined when that
// room's window ends; here the room also owes a settlement, so the durable gate would fail it in
// about six seconds anyway - three refusals two seconds apart, maxSettleFailures of them, ending
// in failRoomLocked. The window is the shorter of the two and is what closes it, which is why
// this row is bounded below the rescue's own time (refusedWindowClosesWithin) and why the close
// must be the evictor's second eviction of this room rather than any close at all.
func TestTheRefusedNotificationClosesARoomThatOwesASettlement(t *testing.T) {
	pair := startPair(t, nil)
	project := uniqueProjectKey(t, "OWD")
	pair.a.createProject(t, project, "Owes a settlement")
	_, artifactID := pair.a.createIssueIn(t, project, "Written in a burst elsewhere", "before")

	reconnected, wrote := burstLeavingTheRoomBehind(t, pair, artifactID, nil)
	if !settlementOwedFor(t, pair, artifactID) {
		t.Fatalf("this document owes no settlement, so this row is not the branch it is named for")
	}
	requireTheRoomClosed(t, pair.a, reconnected, artifactID, wrote, refusedWindowClosesWithin, "owes a settlement")
}

// The branch the deferred re-examination was built for, and the one nothing else covers: a room
// no settlement of this process's will ever fail. A settlement is what rescued the room in the
// row above - refused by the durable gate three times two seconds apart until failRoomLocked -
// and a reader with the tab open who has typed nothing has no such rescuer, so the window's
// re-examination is the only path that closes its room, and without it that reader holds text
// the store replaced for as long as the tab stays open (roomIdleTimeout is how long a room stays
// resident *after its last peer leaves*, which a room with a browser in it never reaches).
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
// close this row measures is Evict, which stops the room's armed settle timer before it closes
// the room, and that the row asserts afterwards that A wrote no "dispatch: settle document" line
// for this document - a settlement that fired on any of those paths would be in A's output.
//
// The condition itself was tried first and did not hold: B was given a settle delay short enough
// to clear the pending row before A's reconnect loaded the room, and the row asserted that row
// absent at the load - which passed, and the room still settled, because A's own reconnecting
// peer appends as it syncs and arms a settlement of its own. With the wake wired that assertion
// passed only because the window (1.88 s) beat the settle delay (2 s), which is a vacuous pass;
// the control with the wake unwired is what exposed it. Hence this shape, and hence the
// assertion below that no settlement of A's ran, which under a 30-minute delay is a real
// condition rather than a race the window happens to win.
//
// Its budget stays at thirty seconds, which already discriminates: with the wake unwired nothing
// closes this room at all, so no bound is needed to tell the window from a rescuer it has none
// of.
func TestTheRefusedNotificationClosesARoomNoSettlementWillFail(t *testing.T) {
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
	pair.b.createProject(t, project, "No settlement will fail it")
	_, artifactID := pair.b.createIssueIn(t, project, "Open here, typed into never", "before")

	reconnected, wrote := burstLeavingTheRoomBehind(t, pair, artifactID, nil)
	requireTheRoomClosed(t, pair.a, reconnected, artifactID, wrote, 30*time.Second, "no settlement will fail it")
	if settled := pair.a.logLines("dispatch: settle document", artifactID); len(settled) > 0 {
		t.Errorf("A settled this room after all, so something other than the window could have closed it:\n%v", settled)
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
