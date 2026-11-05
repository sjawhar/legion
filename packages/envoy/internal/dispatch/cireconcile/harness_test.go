package cireconcile

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/ratelimit"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/webhook"
)

// The repository every test below reconciles. The row is what repo_projects holds, lower-cased
// as every mapping is stored; gitHubOwner and gitHubName are what GitHub itself answers with, in
// its own casing, and every record and every settlement is keyed by those rather than the row's.
const (
	rowRepository    = "acme/widgets"
	gitHubOwner      = "Acme"
	gitHubName       = "Widgets"
	gitHubRepository = gitHubOwner + "/" + gitHubName
	repositoryID     = 4242

	pullNumber = "7"
	headSHA    = "abcdef1234567890abcdef1234567890abcdef12"

	// The review App's id, and another App's, as GitHub's own JSON carries them: a number.
	reviewerApp = 15368
	otherApp    = 98765

	webhookSecret = "cireconcile-secret"
)

// checksSubject is where the listener publishes the head's settlement: the owner and name GitHub
// gave, which is why a pass keyed from the lower-cased row would settle somewhere else.
const checksSubject = "notifications.github." + gitHubOwner + "." + gitHubName + ".pr." + pullNumber + ".checks"

// checkRunsPath and checkSuitesPath are the paths of the head's two listings, which a pass reads
// page by page and a test counts the reads of.
const (
	checkRunsPath   = "/repos/" + gitHubRepository + "/commits/" + headSHA + "/check-runs"
	checkSuitesPath = "/repos/" + gitHubRepository + "/commits/" + headSHA + "/check-suites"
)

// reviewerAppID is the review App's id as a Reconciler and the listener's webhook take it: the
// text of the number GitHub's own objects carry.
var reviewerAppID = strconv.Itoa(reviewerApp)

// harness is one test's whole stack: the GitHub stand-in, a Postgres of this test's own with no
// delivery_settings row, a NATS of its own carrying the listener's real CI store and its real
// summary loop, the listener's real webhook handler over them, and the reconcile pass.
type harness struct {
	t          *testing.T
	ctx        context.Context
	github     *fakeGitHub
	pool       *store.Pool
	bus        *bus.Client
	listener   *cistore.Store
	webhook    http.HandlerFunc
	logs       *syncBuffer
	reconciler *Reconciler
	// reviewer is the review App's id this test's listener and pass were built with, which
	// startListener needs for the webhook handler it mounts.
	reviewer string
}

// newHarness is the whole stack with the listener already running, which every test wants but
// the one about a pass that runs before the listener has made the CI bucket.
func newHarness(t *testing.T, reviewer string) *harness {
	t.Helper()
	h := newHarnessBeforeTheListener(t, reviewer)
	h.startListener()
	return h
}

// newHarnessBeforeTheListener is the stack with no CI bucket: NATS is up, so a pass runs, but
// nothing has made envoy_ci_state yet, which is where a Dispatch process booted ahead of the
// listener stands. startListener then makes the bucket, as the listener itself does.
func newHarnessBeforeTheListener(t *testing.T, reviewer string) *harness {
	t.Helper()
	ctx := t.Context()
	database := storetest.Open(t)
	_, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	t.Cleanup(client.Close)

	logs := &syncBuffer{}
	limits := ratelimit.New(database.Pool)
	github := newFakeGitHub(t)
	return &harness{
		t:        t,
		ctx:      ctx,
		github:   github,
		pool:     database.Pool,
		bus:      client,
		logs:     logs,
		reviewer: reviewer,
		reconciler: &Reconciler{
			Pool:   database.Pool,
			GitHub: github.client(limits),
			Limits: limits,
			// The writer is bound by the first pass that finds NATS connected and the bucket
			// made, as the running pass binds it, rather than here.
			Open:          openWriter(client),
			ReviewerAppID: reviewer,
			Logger:        slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
			// A test waits for no pace.
			Pace: 0,
		},
	}
}

// openWriter is the Open a Reconciler of this harness binds through: cistore's own writer, which
// creates no bucket and so fails until the listener has made one.
func openWriter(client *bus.Client) func() (Writer, error) {
	return func() (Writer, error) {
		writer, err := cistore.OpenWriter(client.Conn, logging.New("writer"))
		if err != nil {
			return nil, err
		}
		return writer, nil
	}
}

// startListener opens the listener's own store, which makes the bucket and watches it, and its
// real summary loop, and mounts the listener's real webhook handler over them: the pass's job is
// to make this listener settle, and a record alone does not prove that.
func (h *harness) startListener() {
	h.t.Helper()
	listener, err := cistore.Open(h.bus.Conn, logging.New("listener"), cistore.WithReplicas(1), cistore.WithTTL(time.Hour))
	if err != nil {
		h.t.Fatalf("open the listener's CI store: %v", err)
	}
	ready, cancelReady := context.WithTimeout(h.ctx, 10*time.Second)
	defer cancelReady()
	if err := listener.WaitForCacheReady(ready); err != nil {
		h.t.Fatalf("the listener's cache is not ready: %v", err)
	}
	loop, endLoop := context.WithCancel(h.ctx)
	h.t.Cleanup(endLoop)
	cistore.StartSummaryLoop(loop, listener, h.bus, 100*time.Millisecond, 20*time.Millisecond,
		metrics.New().NewGauge("held", ""), logging.New("loop"))
	h.listener = listener
	h.webhook = webhook.GitHubHandler(webhookSecret, "@legion", h.reviewer, h.bus, listener)
}

// replica is a second Reconciler standing for the other Dispatch process of a deployment: a
// GitHub client of its own over the one stand-in, and a log of its own, over the database, the
// NATS and the shared rate limits this harness's own Reconciler uses.
func (h *harness) replica() *Reconciler {
	h.t.Helper()
	limits := ratelimit.New(h.pool)
	return &Reconciler{
		Pool:          h.pool,
		GitHub:        h.github.client(limits),
		Limits:        limits,
		Open:          openWriter(h.bus),
		ReviewerAppID: h.reviewer,
		Logger:        slog.New(slog.NewJSONHandler(&syncBuffer{}, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Pace:          0,
	}
}

// mapRepository maps one repository to a project of its own, as Settings → Repositories does.
func (h *harness) mapRepository(repository string) {
	h.t.Helper()
	if _, err := h.pool.Exec(h.ctx, `
		insert into projects (key, name) values ('CIR', 'CI reconcile') on conflict (key) do nothing
	`); err != nil {
		h.t.Fatalf("seed the project: %v", err)
	}
	if _, err := h.pool.Exec(h.ctx, `
		insert into repo_projects (repo, project, created_by)
		values ($1, 'CIR', '{"kind":"system","id":"test"}'::jsonb)
		on conflict (repo) do nothing
	`, repository); err != nil {
		h.t.Fatalf("map %s to a project: %v", repository, err)
	}
}

// listHead makes the stand-in answer with one open pull request at one head, of the repository
// the row names and of the name GitHub gives it.
func (h *harness) listHead() {
	h.github.setRepository(rowRepository, gitHubRepository, repositoryID)
	h.github.setPullRequests(gitHubRepository, fakePull{number: 7, sha: headSHA})
}

// pass runs one reconcile pass.
func (h *harness) pass() {
	h.t.Helper()
	h.reconciler.Pass(h.ctx)
}

// subscribe listens for the head's settlements before anything can publish one.
func (h *harness) subscribe() *nats.Subscription {
	h.t.Helper()
	sub, err := h.bus.Conn.SubscribeSync(checksSubject)
	if err != nil {
		h.t.Fatalf("subscribe to %s: %v", checksSubject, err)
	}
	h.t.Cleanup(func() { _ = sub.Unsubscribe() })
	return sub
}

// settlement is the next settlement published for the head, which must arrive.
func (h *harness) settlement(sub *nats.Subscription, why string) cistore.Summary {
	h.t.Helper()
	message, err := sub.NextMsg(15 * time.Second)
	if err != nil {
		h.t.Fatalf("%s: no settlement was published: %v", why, err)
	}
	var envelope contracts.Envelope
	if err := json.Unmarshal(message.Data, &envelope); err != nil {
		h.t.Fatalf("%s: the settlement is not an envelope: %v", why, err)
	}
	var summary cistore.Summary
	if err := json.Unmarshal([]byte(envelope.Payload), &summary); err != nil {
		h.t.Fatalf("%s: the settlement's payload is not JSON: %v\n%s", why, err, envelope.Payload)
	}
	return summary
}

// noSettlement fails when another settlement is published within the summary loop's own debounce
// several times over.
func (h *harness) noSettlement(sub *nats.Subscription, why string) {
	h.t.Helper()
	message, err := sub.NextMsg(2 * time.Second)
	if err == nil {
		h.t.Fatalf("%s: a settlement was published: %s", why, message.Data)
	}
}

// settledRecord is the head's record once the summary loop has finished with it. Publishing a
// settlement is itself two writes of the record -- the claim, and the mark that says it was
// emitted -- so a revision read the moment a settlement arrives still moves afterwards, and a
// test comparing revisions across a pass has to start from one that has stopped.
func (h *harness) settledRecord() (cistore.State, uint64) {
	h.t.Helper()
	var state cistore.State
	var revision uint64
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		next, nextRevision, ok := h.record()
		if ok && next.SettledEmitted && next.Claim == nil && nextRevision == revision {
			return next, revision
		}
		state, revision = next, nextRevision
		time.Sleep(50 * time.Millisecond)
	}
	h.t.Fatalf("the head's record did not settle: %+v", state)
	return state, revision
}

// record is the head's record as the bucket holds it, with its revision, and ok false when the
// bucket holds none.
func (h *harness) record() (state cistore.State, revision uint64, ok bool) {
	h.t.Helper()
	kv, err := h.bus.JS().KeyValue(bus.CIStateBucket)
	if err != nil {
		h.t.Fatalf("open the CI bucket: %v", err)
	}
	entry, err := kv.Get(cistore.Key(gitHubOwner, gitHubName, pullNumber, headSHA))
	if err != nil {
		return cistore.State{}, 0, false
	}
	if err := json.Unmarshal(entry.Value(), &state); err != nil {
		h.t.Fatalf("the record is not JSON: %v", err)
	}
	return state, entry.Revision(), true
}

// post sends the listener's real webhook handler one GitHub event, as GitHub sends it.
func (h *harness) post(event, body, delivery string) {
	h.t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/webhook/github", strings.NewReader(body))
	request.Header.Set("X-GitHub-Delivery", delivery)
	request.Header.Set("X-GitHub-Event", event)
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write([]byte(body))
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	recorder := httptest.NewRecorder()
	h.webhook.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		h.t.Fatalf("the webhook answered %d: %s", recorder.Code, recorder.Body.String())
	}
}

// lines is the fields of every line the pass logged under msg, oldest first.
func (h *harness) lines(msg string) []map[string]any {
	h.t.Helper()
	var found []map[string]any
	for _, text := range strings.Split(strings.TrimSpace(h.logs.String()), "\n") {
		if text == "" {
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal([]byte(text), &fields); err != nil {
			h.t.Fatalf("a log line is not JSON: %v\n%s", err, text)
		}
		if fields["msg"] == msg {
			found = append(found, fields)
		}
	}
	return found
}

// passLine is the fields of the last "ci reconcile pass" line the pass logged.
func (h *harness) passLine() map[string]any {
	h.t.Helper()
	lines := h.lines("ci reconcile pass")
	if len(lines) == 0 {
		h.t.Fatalf("no pass line was logged:\n%s", h.logs.String())
	}
	return lines[len(lines)-1]
}

// logged reports whether any line the pass logged carries text.
func (h *harness) logged(text string) bool {
	return strings.Contains(h.logs.String(), text)
}

// number is one field of the pass line, which JSON decodes as a float.
func number(t *testing.T, line map[string]any, field string) int {
	t.Helper()
	value, ok := line[field].(float64)
	if !ok {
		t.Fatalf("the pass line has no %s: %v", field, line)
	}
	return int(value)
}

// syncBuffer is a log buffer two goroutines may write, since a pass and the test read and write
// it from their own.
type syncBuffer struct {
	mu   sync.Mutex
	text bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.text.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.text.String()
}

// checkRun is one check run object as GitHub's listing answers it, and as its webhook carries it.
func checkRun(id int64, name, status, conclusion string, appID int, observedAt string) object {
	return object{
		"id":           id,
		"name":         name,
		"status":       status,
		"conclusion":   conclusion,
		"head_sha":     headSHA,
		"completed_at": observedAt,
		"started_at":   observedAt,
		"html_url":     fmt.Sprintf("https://github.com/%s/runs/%d", gitHubRepository, id),
		"app":          object{"id": appID},
		"check_suite":  object{"id": 900, "head_branch": "feature/x"},
	}
}

// checkSuite is one check suite object as GitHub's listing answers it.
func checkSuite(id int64, status, conclusion string, appID int, observedAt string) object {
	return object{
		"id":         id,
		"status":     status,
		"conclusion": conclusion,
		"head_sha":   headSHA,
		"app":        object{"id": appID},
		"updated_at": observedAt,
	}
}
