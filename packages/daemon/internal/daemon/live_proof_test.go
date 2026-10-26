//go:build e2e

// The harness this package's whole-daemon live proofs share: merged_root_waits_live_test.go and
// no_required_checks_live_test.go, whose scripts (scripts/e2e/merged-root-waits.sh and
// scripts/e2e/no-required-checks.sh) make the same run around them through
// scripts/e2e/lib/live-daemon-proof.sh. A driver embeds liveProof, which holds what every such
// proof needs before and around the daemon it boots: the configuration it boots with (newLiveProof
// reads the broker LEGION_E2E_NATS_URL names, skips when it names none, and builds workflowConfig
// around it), the log file the script names, the stand-ins for Dispatch and Envoy's listener, the
// store the daemon runs on, the seeding of a tree into the next free admission slots (admit), and
// the reads and writes a step makes of them: the envelopes it publishes, the events it waits for
// the daemon to take, and the notices an issue's architect is told (architectNotices). What the
// driver keeps is what its own proof drives.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// liveNATSURLEnv names the running nats-server the script hands the driver. Unset, a driver skips
// rather than starting a broker of its own, so `go vet -tags e2e ./...` and a plain `go test -tags
// e2e ./internal/daemon` still compile and run it.
const liveNATSURLEnv = "LEGION_E2E_NATS_URL"

// liveDaemonLogEnv names the file the booted daemon's own log is written to, at debug, for the
// run's evidence. Unset, the daemon's log is dropped, as every other test in this package drops
// it.
const liveDaemonLogEnv = "LEGION_E2E_DAEMON_LOG"

// liveRepo is the repository the project's Dispatch mapping serves (workflowConfig's
// servedMapping), which every issue a live proof seeds is on.
const liveRepo = "acme/widgets"

// liveSettle is how long a step that must see nothing happen watches for it: what it must not see
// — the boot's sweep of merged pull requests, which runs behind the API, or a notice the outbox
// publishes after the fact that enqueued it commits — lands after the read that would otherwise
// end the step, so the step watches this long for it.
const liveSettle = 3 * time.Second

// liveProof is what a run of a live proof holds whatever it proves: the booted daemon, the
// configuration it was booted with, a JetStream client of the script's broker, the Dispatch and
// Envoy listener stand-ins the steps read the daemon's own writes back from, and the daemon's own
// store, which seeds the fixture.
type liveProof struct {
	t       *testing.T
	cfg     config.Config
	d       *daemon
	js      jetstream.JetStream
	board   *liveDispatch
	notices *noticeListener
	st      *store.Store
	records record.Store
	project string
}

// newLiveProof readies workflowConfig for a driver to seed and boot: Dispatch and Envoy's listener
// answered by the run's own stand-ins, the stream the daemon's intake consumes on the broker
// liveNATSURLEnv names, which boot refuses to run without, and the store opened and migrated. With
// no broker named, the test skips, naming script, the driver's own script that starts one. A
// driver whose proof needs more of the configuration sets it on cfg before it boots: nothing here
// reads more of it than the project and the store.
func newLiveProof(t *testing.T, script string) liveProof {
	t.Helper()
	natsURL := os.Getenv(liveNATSURLEnv)
	if natsURL == "" {
		t.Skipf("%s is unset, so there is no broker to drive the daemon's intake through; %s starts a nats-server and sets it", liveNATSURLEnv, script)
	}
	cfg := workflowConfig(t, natsURL)
	p := liveProof{t: t, cfg: cfg, board: newLiveDispatch(t, cfg.Project), notices: newNoticeListener(), records: record.NewStore()}
	dispatchServer := httptest.NewServer(p.board)
	t.Cleanup(dispatchServer.Close)
	p.cfg.DispatchURL = dispatchServer.URL
	envoyServer := httptest.NewServer(p.notices)
	t.Cleanup(envoyServer.Close)
	p.cfg.EnvoyURL = envoyServer.URL

	var err error
	if p.project, err = claim.ProjectToken(cfg.Project); err != nil {
		t.Fatal(err)
	}
	p.js = workflowJetStream(t, natsURL)
	testnats.CreateStream(t, p.js, jetstream.StreamConfig{Name: "ENVOY_NOTIFICATIONS", Subjects: []string{"notifications.>"}})
	ctx := context.Background()
	if p.st, err = store.Open(ctx, p.cfg.PostgresDSN); err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(p.st.Close)
	if _, err := p.st.Migrate(ctx); err != nil {
		t.Fatalf("migrate the store: %v", err)
	}
	return p
}

// log is where the booted daemon writes its own log: the file liveDaemonLogEnv names, appended to
// across a restart, and nowhere when it names none.
func (p *liveProof) log() *slog.Logger {
	p.t.Helper()
	path := os.Getenv(liveDaemonLogEnv)
	if path == "" {
		return quietLogger()
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		p.t.Fatalf("open the daemon's log %s: %v", path, err)
	}
	p.t.Cleanup(func() { file.Close() })
	return slog.New(slog.NewTextHandler(file, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// step runs one of the proof's steps between its `== name` and `ok name (<s>s)` lines, as the
// repository's live proofs print them.
func (p *liveProof) step(name string, body func()) {
	p.t.Helper()
	started := time.Now()
	fmt.Printf("== %s\n", name)
	body()
	fmt.Printf("ok %s (%ds)\n", name, int(time.Since(started).Seconds()))
}

// note prints what a step drove or read, under its heading.
func (p *liveProof) note(format string, args ...any) {
	fmt.Printf("   "+format+"\n", args...)
}

// issue is key as GET /legion/v1/state shows it now.
func (p *liveProof) issue(key string) api.Issue {
	p.t.Helper()
	issue, ok := p.d.state().Issues[key]
	if !ok {
		p.t.Fatalf("the state records no issue %s", key)
	}
	return issue
}

// subjectRepo is liveRepo as a GitHub envelope's subject spells it.
func (p *liveProof) subjectRepo() string {
	return strings.ReplaceAll(liveRepo, "/", ".")
}

// publish puts one Envoy envelope from source on the stream the daemon consumes, and returns its
// event id. The event id carries the run's own project, since a processed event is deduplicated by
// its id across the shared database, and a proof's runs share one.
func (p *liveProof) publish(subject, source, eventID, payload string) string {
	p.t.Helper()
	eventID = p.cfg.Project + "-" + eventID
	envelope, err := json.Marshal(map[string]any{
		"event_id": eventID, "source": source, "source_event_id": eventID, "topic": subject, "dedupe_key": eventID,
		"issued_at": time.Now().UnixMilli(), "payload_summary": subject, "payload": payload, "trace_id": eventID,
	})
	if err != nil {
		p.t.Fatalf("encode the %s envelope: %v", subject, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := p.js.Publish(ctx, subject, envelope); err != nil {
		p.t.Fatalf("publish %s: %v", subject, err)
	}
	return eventID
}

// admit seeds issues, in order, into the next free admission slots in tx. Slot indexes are unique
// across the shared database, which a pod re-runs every live proof against, so each tree takes the
// next index after the highest any earlier run left taken.
func (p *liveProof) admit(ctx context.Context, tx pgx.Tx, issues ...string) error {
	taken, err := p.records.Slots(ctx, tx)
	if err != nil {
		return err
	}
	index := 0
	for _, slot := range taken {
		index = max(index, slot.Index+1)
	}
	for _, issue := range issues {
		if err := p.records.PutSlot(ctx, tx, record.Slot{Issue: issue, Index: index, AdmittedAt: time.Now()}); err != nil {
			return err
		}
		index++
	}
	return nil
}

// waitProcessed waits for the daemon's processed_events row for the GitHub envelope eventID, which
// the intake writes in the transaction that applies the event, so what a step reads next follows
// that event. No route shows the row.
func (p *liveProof) waitProcessed(eventID string) {
	p.t.Helper()
	testwait.Eventually(p.t, "the daemon's processed_events row for github event "+eventID, func() bool {
		var taken bool
		if err := p.st.Pool().QueryRow(context.Background(),
			"select exists(select 1 from processed_events where source = 'github' and event_id = $1)", eventID).Scan(&taken); err != nil {
			p.t.Fatalf("read the processed event %s: %v", eventID, err)
		}
		return taken
	})
}

// architectNotices is every notice of kind the daemon has published to the role topic of key's
// architect, each payload replaced by the notice's reason. Every notice a live proof reads is one
// an architect is told, so the topic is derived here, from the run's own project, and a step names
// only the issue it drove.
func (p *liveProof) architectNotices(key, kind string) []listenerPublish {
	p.t.Helper()
	architect, err := claim.NewToken(p.project, key, claim.RoleArchitect)
	if err != nil {
		p.t.Fatal(err)
	}
	var got []listenerPublish
	for _, published := range p.notices.acceptedOn(notify.RoleTopicPrefix + string(architect)) {
		var payload struct {
			Kind   string `json:"kind"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(published.Payload), &payload); err != nil {
			p.t.Fatalf("decode the notice payload %q: %v", published.Payload, err)
		}
		if payload.Kind == kind {
			published.Payload = payload.Reason
			got = append(got, published)
		}
	}
	return got
}

// waitArchitectNotices polls until the daemon has told key's architect count notices of kind, and
// returns them as architectNotices does. The wait's failure names the count, the kind and the
// issue, which is all a step would say of it.
func (p *liveProof) waitArchitectNotices(key, kind string, count int) []listenerPublish {
	p.t.Helper()
	var got []listenerPublish
	testwait.Eventually(p.t, fmt.Sprintf("%s's architect's %s notices to number %d", key, kind, count), func() bool {
		got = p.architectNotices(key, kind)
		return len(got) == count
	})
	return got
}

// liveDispatch is workflowConfig's Dispatch stand-in — the repository mapping and an empty issue
// list — grown into the board the daemon writes to: it holds each issue's status, as Dispatch's own
// board does, answers the issue read the outbox makes before a status write, and records every
// status and message the daemon posted, so a step can read back what the daemon told Dispatch and
// tell a write that never came from one it cannot see. Anything else is a call a live proof does
// not expect, and fails the test as workflowConfig's stand-in does.
type liveDispatch struct {
	t       *testing.T
	project string
	mu      sync.Mutex
	// status is each issue's status as this board holds it, the daemon's writes applied.
	status map[string]string
	posted []liveDispatchWrite
}

// liveDispatchWrite is one write the daemon made to Dispatch: a status it set on an issue, or a
// message it posted on one.
type liveDispatchWrite struct {
	Issue  string
	Status string
	Body   string
}

func newLiveDispatch(t *testing.T, project string) *liveDispatch {
	return &liveDispatch{t: t, project: project, status: map[string]string{}}
}

// show puts key on the board at status, as the seeded fixture stands on Dispatch before the daemon
// writes anything.
func (b *liveDispatch) show(key, status string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.status[key] = status
}

func (b *liveDispatch) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if servedMapping(b.t, w, req, b.project) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	path := req.URL.Path
	key := issueOfDispatchPath(path)
	switch {
	case req.Method == http.MethodGet && path == "/api/v1/issues":
		_, _ = io.WriteString(w, `[]`)
	case req.Method == http.MethodGet && path == "/healthz":
		_, _ = io.WriteString(w, `{"status":"ok","commit":null}`)
	case req.Method == http.MethodGet && strings.HasSuffix(path, "/events"):
		// The outbox reads an issue's events to see whether its message is already posted; this
		// board keeps none, so every message the daemon decides to post is posted.
		_, _ = io.WriteString(w, `[]`)
	case req.Method == http.MethodGet && strings.HasPrefix(path, "/api/v1/issues/") && key != "":
		b.showIssue(key, w)
	case req.Method == http.MethodPost && strings.HasSuffix(path, "/messages"):
		b.record(liveDispatchWrite{Issue: key, Body: b.decode(req, w)}, w)
	case req.Method == http.MethodPatch && strings.HasPrefix(path, "/api/v1/issues/"):
		write := liveDispatchWrite{Issue: key, Status: b.decode(req, w)}
		b.show(key, write.Status)
		b.record(write, w)
	default:
		b.t.Errorf("Dispatch request = %s %s", req.Method, path)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}
}

// showIssue answers the issue read the outbox makes before a status write: the board's own status
// for it, with the Legion label the tree runs under.
func (b *liveDispatch) showIssue(key string, w http.ResponseWriter) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := json.NewEncoder(w).Encode(map[string]any{
		"key": key, "project": b.project, "title": key, "status": b.status[key], "rank": "U",
		"labels": []string{"legion"}, "last_seq": 1, "resolved_repo": liveRepo,
	}); err != nil {
		b.t.Errorf("write the Dispatch issue %s: %v", key, err)
	}
}

// decode is the write's status or body, whichever the daemon sent.
func (b *liveDispatch) decode(req *http.Request, w http.ResponseWriter) string {
	var write struct {
		Status string `json:"status"`
		Body   string `json:"body"`
	}
	if err := json.NewDecoder(req.Body).Decode(&write); err != nil {
		b.t.Errorf("decode the Dispatch write to %s: %v", req.URL.Path, err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return ""
	}
	if write.Status != "" {
		return write.Status
	}
	return write.Body
}

func (b *liveDispatch) record(write liveDispatchWrite, w http.ResponseWriter) {
	b.mu.Lock()
	b.posted = append(b.posted, write)
	b.mu.Unlock()
	_, _ = io.WriteString(w, `{}`)
}

// writes is every status the daemon has set and every message it has posted, oldest first.
func (b *liveDispatch) writes() []liveDispatchWrite {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]liveDispatchWrite(nil), b.posted...)
}

// wrote says whether the daemon set issue to status.
func (b *liveDispatch) wrote(issue, status string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, write := range b.posted {
		if write.Issue == issue && write.Status == status {
			return true
		}
	}
	return false
}

// issueOfDispatchPath is the issue key in /api/v1/issues/<key>[/...].
func issueOfDispatchPath(path string) string {
	rest := strings.TrimPrefix(path, "/api/v1/issues/")
	key, _, _ := strings.Cut(rest, "/")
	return key
}
