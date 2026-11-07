//go:build e2e

// What the two live proofs that drive real Oh My Pi panes share below the daemon: the constants
// both read off the daemon, the run each one is (liveAgentProof) with its reads of a claim, of a
// pane and of the daemon's own log, the database, the `legion` binary, the plugins and the pane
// listener a run builds before the daemon boots, the model stand-in each pane talks to, the
// provider answers those stand-ins write, the overlay writers that point a pane at one, and the
// log handler those reads of the daemon's own decisions go through. The two are
// provider_error_retry_live_test.go, the driver of scripts/e2e/provider-error-retry.sh, and
// claim_compaction_live_test.go, the driver of scripts/e2e/claim-compaction.sh. What belongs to
// one proof stays in that proof's own file: its issues and their seed, its steps, its stand-ins'
// handlers, and the Oh My Pi profile and operator overlay its panes run under.
//
// A stand-in's handler is its own proof's, because the two differ in what decides an answer: the
// retry proof's three answer by the provider name each is given in the run's models.yml, and the
// compaction proof's four answer by what the step before them set, a turn's request apart from a
// compaction's summary request. What they hold in common is here: the HTTP server, the bodies
// they were sent, and the three answers both of them write — the overloaded status, a refused
// request, and the Messages stream of a turn that ends.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testbin"
)

// ---- the constants both proofs read --------------------------------------------------------------

// liveOmpPathEnv names the Oh My Pi every pane of either proof runs. It is the pod's own build,
// which is the fleet's pin rather than this branch's fetch, so a proof prints which build it
// ran. Unset, the driver skips rather than guess one, as each proof's script refuses to start
// without it.
const liveOmpPathEnv = "LEGION_OMP_PATH"

// providerErroredMessage is the daemon's own line for a turn that ended in a provider error
// (supervise's providerErrored), the record a step reads the reported fields off.
const providerErroredMessage = "supervise: the turn this task's prompt ran ended in a provider error; the task is kept and sent again"

// paneReady is how long a real pane is given to start Oh My Pi, load the two plugins, register and
// report ready.
const paneReady = 3 * time.Minute

// ladderDelays are the waits the ladder arms before each re-send, in order (supervise's
// promptRetryDelay from attempt 1). The retry proof reads the daemon's own armed wait back
// against the rung here at every send of its ladder, and the compaction proof measures against
// the first rung, so a ladder that changed and left this list behind fails a proof rather than
// passing on the old numbers.
var ladderDelays = []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute}

// ---- what a proof that drives real panes holds, and the reads it makes ---------------------------

// liveAgentProof is what a live proof that drives real Oh My Pi panes holds beyond liveProof, and
// the reads such a proof makes: the runtime that started each pane, the daemon's own log as a
// step reads it back, and the `legion` binary this checkout built, which is both the panes' shim
// and the operator's command line. providerErrorLive and claimCompactionLive
// (claim_compaction_live_test.go) are both one, so the two proofs read a claim, wait for a pane
// and run `legion claims` the same way.
type liveAgentProof struct {
	liveProof
	rt     *localRuntime
	daemon *recordingLog
	legion string
	omp    string
}

// claim is token as the operator's claim listing shows it now.
func (r *liveAgentProof) claim(token claim.Token) api.OperatorClaim {
	r.t.Helper()
	status, body := r.d.request(http.MethodGet, "/legion/v1/operator/claims", nil, true)
	if status != http.StatusOK {
		r.t.Fatalf("the operator's claim listing = %d %s", status, body)
	}
	var list api.OperatorClaims
	if err := json.Unmarshal(body, &list); err != nil {
		r.t.Fatalf("decode the claim listing %s: %v", body, err)
	}
	for _, held := range list.Claims {
		if held.Token == token {
			return held
		}
	}
	r.t.Fatalf("the daemon holds no claim %s; it holds %d", token, len(list.Claims))
	return api.OperatorClaim{}
}

// awaitReady waits for the claim's real pane to start Oh My Pi, register and report ready. A claim
// that fails meanwhile ends the wait at once, naming the log the worker's own output is in: a pane
// that cannot start is read from there and nowhere else.
func (r *liveAgentProof) awaitReady(token claim.Token) {
	r.t.Helper()
	var held api.OperatorClaim
	r.await(string(token)+"'s pane to register and report ready", paneReady, func() bool {
		held = r.claim(token)
		if held.State == string(supervise.StateFailed) {
			r.t.Fatalf("%s failed before its pane reported ready; its workers' output is under %s",
				token, r.rt.paneLogs)
		}
		return held.State == string(supervise.StateReady) || held.State == string(supervise.StateIdle)
	})
	r.note("%s is %s in session %s, plugin contract %d, process %s",
		token, held.State, held.Session, held.PluginContract, held.Locator.Label())
}

// await polls until want holds, failing with what it waited for. testwait.Eventually bounds its
// own wait at a minute, which is shorter than the ladder's own gaps.
func (r *liveAgentProof) await(what string, within time.Duration, want func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(within)
	for {
		if want() {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("timed out after %s waiting for %s", within, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// awaitLog waits for the daemon to write a log record matching want, and answers the first one it
// wrote after the wait began.
func (r *liveAgentProof) awaitLog(what string, within time.Duration, want func(loggedRecord) bool) *loggedRecord {
	r.t.Helper()
	return r.awaitLogFrom(what, r.daemon.count(), within, want)
}

// awaitLogFrom is awaitLog from a mark the step took itself, before whatever it drove: the daemon
// errors and decides inside milliseconds of each other, so a step that must read both takes one
// mark ahead of the act rather than a mark per read.
func (r *liveAgentProof) awaitLogFrom(what string, from int, within time.Duration, want func(loggedRecord) bool) *loggedRecord {
	r.t.Helper()
	var found *loggedRecord
	r.await(what, within, func() bool {
		found = r.daemon.firstAfter(from, want)
		return found != nil
	})
	return found
}

// logs is every record the daemon has written that matches want, oldest first.
func (r *liveAgentProof) logs(want func(loggedRecord) bool) []loggedRecord {
	return r.daemon.matching(want)
}

// lastLog is the newest record matching want, and nil when there is none.
func (r *liveAgentProof) lastLog(want func(loggedRecord) bool) *loggedRecord {
	matched := r.daemon.matching(want)
	if len(matched) == 0 {
		return nil
	}
	return &matched[len(matched)-1]
}

// legionClaims runs `legion claims <args>` — the binary this checkout built — against the booted
// daemon, and answers its output. A non-zero exit fails the step with both streams.
func (r *liveAgentProof) legionClaims(args ...string) string {
	r.t.Helper()
	out, err := r.legionClaimsTry(args...)
	if err != nil {
		r.t.Fatalf("legion claims %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// legionClaimsTry is legionClaims for a command a step expects the daemon to refuse: it answers
// both streams and the exit error rather than ending the step, since the refusal is what the
// step reads (claim_compaction_live_test.go).
func (r *liveAgentProof) legionClaimsTry(args ...string) (string, error) {
	r.t.Helper()
	full := append([]string{"claims", args[0], "--port", strconv.Itoa(r.cfg.Port),
		"--operator-token-file", r.cfg.OperatorTokenFile}, args[1:]...)
	cmd := exec.Command(r.legion, full...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + r.t.TempDir()}
	out, err := cmd.CombinedOutput()
	return strings.TrimRight(string(out), "\n"), err
}

// indentOutput puts each line of a command's output under a step's own note.
func indentOutput(text string) string {
	return "   " + strings.ReplaceAll(text, "\n", "\n   ")
}

// ---- what the run builds before the daemon boots -------------------------------------------------

// takeOwnDatabase points LEGION_TEST_PG_DSN at a database this run makes for itself on the server
// that variable names, so the fixture migrates into a database of its own rather than into the one
// the DSN names. The daemon's live proofs share the named database on a CI service, whose role
// owns it; in a Legion worker pod the server's `legion_test` role may create databases and may not
// write the `public` schema of `postgres`, which a migration answers `permission denied for schema
// public` (SQLSTATE 42501). The database is dropped at the end of the run; that role also cannot
// terminate another backend, so a drop a connection outlives is reported and not a failure, and the
// database goes with the pod.
func takeOwnDatabase(t *testing.T) {
	t.Helper()
	named := os.Getenv("LEGION_TEST_PG_DSN")
	if named == "" {
		return // newLiveProof's own testDSN skips the test, naming the variable.
	}
	ctx := context.Background()
	server, err := pgx.Connect(ctx, named)
	if err != nil {
		t.Fatalf("connect the PostgreSQL LEGION_TEST_PG_DSN names: %v", err)
	}
	database := "legion_live_proof_" + strings.ToLower(randomSuffix(t))
	if _, err := server.Exec(ctx, "create database "+database+" strategy file_copy"); err != nil {
		server.Close(ctx)
		t.Fatalf("make this run's own database %s: %v", database, err)
	}
	if err := server.Close(ctx); err != nil {
		t.Fatalf("close the connection the database was made on: %v", err)
	}
	own, err := url.Parse(named)
	if err != nil {
		t.Fatalf("read LEGION_TEST_PG_DSN as a URL: %v", err)
	}
	own.Path = "/" + database
	t.Setenv("LEGION_TEST_PG_DSN", own.String())
	fmt.Printf("   this run migrates into its own database %s on the server LEGION_TEST_PG_DSN names\n", database)
	t.Cleanup(func() {
		drop, err := pgx.Connect(context.Background(), named)
		if err != nil {
			t.Logf("the run's own database %s was left behind: %v", database, err)
			return
		}
		defer drop.Close(context.Background())
		if _, err := drop.Exec(context.Background(), "drop database "+database); err != nil {
			t.Logf("the run's own database %s was left behind: %v", database, err)
		}
	})
}

// proofCheckoutRoot is the repository this driver was built from, found from this file's own
// compiled path: the driver runs from a temporary directory, so nothing about its working
// directory names the checkout.
func proofCheckoutRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("locate this driver's source")
	}
	root, err := filepath.Abs(filepath.Join(filepath.Dir(source), "..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.work")); err != nil {
		t.Fatalf("%s is not the checkout root: %v", root, err)
	}
	return root
}

// buildProofLegion builds the `legion` every pane's shim and every operator command in either
// proof is, from this checkout.
func buildProofLegion(t *testing.T, root, work string) string {
	t.Helper()
	binary := filepath.Join(work, "legion")
	build := exec.Command("go", "build", "-o", binary, "./cmd/legion")
	build.Dir = filepath.Join(root, "packages", "daemon")
	build.Env = append(os.Environ(), "GOFLAGS=")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build legion from %s: %v\n%s", build.Dir, err, out)
	}
	return binary
}

// installProofPlugins packs this checkout's two Oh My Pi plugins as the release packs them
// (scripts/e2e/lib/pack-plugin.sh) and installs each into the run's own profile under its own
// HOME, as scripts/e2e/lib/install-plugin-profile.sh does for a stage proof — through the Oh My Pi
// the run's panes run rather than a mise pin, since a pod has no mise. The profile's natives are
// linked from the shared cache, so no pane downloads a native module.
func installProofPlugins(t *testing.T, root, work, home, profile, omp string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, ".omp"), 0o700); err != nil {
		t.Fatalf("make the run's Oh My Pi home %s: %v", home, err)
	}
	testbin.OMPHome(t, omp, home)
	for _, pkg := range []string{"pi-envoy", "pi-legion"} {
		packed := filepath.Join(work, "pack-"+pkg)
		pack := exec.Command("bash", filepath.Join(root, "scripts", "e2e", "lib", "pack-plugin.sh"), pkg, packed)
		pack.Dir = root
		tarball, err := pack.Output()
		if err != nil {
			t.Fatalf("pack %s: %v", pkg, err)
		}
		dest := filepath.Join(work, pkg)
		if err := os.MkdirAll(dest, 0o700); err != nil {
			t.Fatal(err)
		}
		unpack := exec.Command("tar", "xzf", strings.TrimSpace(string(tarball)), "-C", dest, "--strip-components=1")
		if out, err := unpack.CombinedOutput(); err != nil {
			t.Fatalf("unpack %s: %v\n%s", pkg, err, out)
		}
		install := exec.Command(omp, "plugin", "install", dest)
		install.Dir = dest
		install.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "OMP_PROFILE=" + profile}
		if out, err := install.CombinedOutput(); err != nil {
			t.Fatalf("install %s into the profile %s: %v\n%s", pkg, profile, err, out)
		}
	}
	list := exec.Command(omp, "plugin", "list", "--json")
	list.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "OMP_PROFILE=" + profile}
	out, err := list.Output()
	if err != nil {
		t.Fatalf("read the profile's plugins: %v", err)
	}
	var listing struct {
		NPM []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
			Enabled bool   `json:"enabled"`
		} `json:"npm"`
	}
	if err := json.Unmarshal(out, &listing); err != nil {
		t.Fatalf("decode the plugin listing %s: %v", out, err)
	}
	var installed []string
	for _, plugin := range listing.NPM {
		if plugin.Enabled {
			installed = append(installed, plugin.Name+"@"+plugin.Version)
		}
	}
	if len(installed) != 2 {
		t.Fatalf("the profile %s has %v enabled, want this checkout's pi-envoy and pi-legion", profile, installed)
	}
	fmt.Printf("   plugins %s in Oh My Pi profile %s under %s\n", strings.Join(installed, " and "), profile, home)
}

// newPaneListener is the Envoy listener a pane is told through ENVOY_URL, and answers the URL of
// it. A pane's plugin registers its session with the listener and claims its role topic
// (pi-shared's legion-claim-core, between claims/register and claims/ready), and a claim boot
// whose role claim is refused stops the agent, so the pane needs a listener that answers those
// calls — the fixture's noticeListener, which the daemon itself publishes its notices to, serves
// the publish route alone and 404s the rest. It answers as the plugins' own pane harness does
// (packages/pi-shared/test/omp-harness.ts, runLegionPane's stand-in): the session, a machine and
// no topics, for every path.
//
// Nothing the proof reads comes from here. What an architect or the controller is told is read
// from the daemon's own listener, which is the fixture's and another server.
func newPaneListener(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var sent struct {
			SessionID string `json:"session_id"`
			Dir       string `json:"dir"`
		}
		if body, err := io.ReadAll(req.Body); err == nil {
			_ = json.Unmarshal(body, &sent)
		}
		// The plugin reads an answer whose dir is no string as a refusal, so every answer carries
		// one: the directory the caller named, and the root for a call that named none.
		dir := sent.Dir
		if dir == "" {
			dir = "/"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"session_id": sent.SessionID,
			"machine_id": "live-agent-proof",
			"dir":        dir,
			"topics":     []string{},
		})
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// ---- the model stand-ins -------------------------------------------------------------------------

// standinModelID is the model id every stand-in serves, under its own provider name.
const standinModelID = "-model"

// standinModel is the one model a stand-in serves, as a selector.
func standinModel(provider string) string { return provider + "/" + provider + standinModelID }

// modelStandin is what both proofs' provider endpoints hold: the provider name the run's
// models.yml gives this one, the URL a pane reaches it at, and the body of every model request
// it has answered, which a step counts. The handler is the proof's own and reads each request
// through take.
type modelStandin struct {
	name string
	url  string

	mu       sync.Mutex
	requests []string
}

// serve starts handler as this stand-in's endpoint, closed when the test ends, and records the
// URL the run's models.yml gives the provider.
func (s *modelStandin) serve(t *testing.T, handler http.Handler) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	s.url = server.URL
}

// take is what a stand-in's handler does with a request before it answers it: a request that is
// no model call at all — Oh My Pi lists a provider's models with a GET — is answered an empty
// listing here and take says there is nothing to answer; every other request has its body read
// and recorded, and the handler answers that body.
func (s *modelStandin) take(w http.ResponseWriter, req *http.Request) (string, bool) {
	if req.Method != http.MethodPost {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[]}`)
		return "", false
	}
	body, _ := io.ReadAll(req.Body)
	s.mu.Lock()
	s.requests = append(s.requests, string(body))
	s.mu.Unlock()
	return string(body), true
}

// count is how many model requests the stand-in has answered.
func (s *modelStandin) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

// requestsHolding is how many of them carried marker, which is how many of them are a turn of the
// task rather than one of Oh My Pi's own side requests.
func (s *modelStandin) requestsHolding(marker string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	held := 0
	for _, body := range s.requests {
		if strings.Contains(body, marker) {
			held++
		}
	}
	return held
}

// ---- the answers both proofs' stand-ins write ------------------------------------------------------

// writeOverloaded answers one model request 529 with `retry-after: 3600`. The header is over the
// Oh My Pi client's own cap for a retry delay (maxRetryDelayMs, 60 s), which declines the retry
// and surfaces the status untouched (@oh-my-pi/pi-ai src/providers/anthropic-client.ts), so one
// of the daemon's sends is one request here and the turn ends in a provider error rather than
// after a round of the host's own retries.
func writeOverloaded(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "3600")
	w.WriteHeader(529)
	_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
}

// writeInvalidRequest answers one model request 400 with body, which carries an
// `invalid_request_error`: no word of such a body is one Oh My Pi reads as transient, so the turn
// error is reported `retryable: false` with status 400 and is retried by nobody. A 5xx is retried
// instead, by the summarizer until the daemon's own ten-minute bound runs out, which is why a
// refused summary is a client error here too. Each proof passes the body its own live run has
// measured: the retry proof's malformed provider the bare object, and a pane of the compaction
// proof Anthropic's error envelope (anthropicRefusal).
func writeInvalidRequest(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = io.WriteString(w, body)
}

// anthropicRefusal is Anthropic's own error envelope around an `invalid_request_error` of
// message, which is the shape a request to an `anthropic-messages` provider is refused in.
func anthropicRefusal(message string) string {
	return fmt.Sprintf(`{"type":"error","error":{"type":"invalid_request_error","message":%q}}`, message)
}

// streamAnthropicText answers one Anthropic Messages request with a single text block, in the
// events the plugins' own pane harness scripts (packages/pi-shared/test/omp-harness.ts:383-448,
// `messageStream`).
func streamAnthropicText(w http.ResponseWriter, provider, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	event := func(name, data string) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
		if flusher != nil {
			flusher.Flush()
		}
	}
	event("message_start", fmt.Sprintf(`{"type":"message_start","message":{"id":"answered","type":"message","role":"assistant",`+
		`"model":%q,"content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1000,"output_tokens":1}}}`,
		provider+standinModelID))
	event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	event("content_block_delta", fmt.Sprintf(
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, text))
	event("content_block_stop", `{"type":"content_block_stop","index":0}`)
	event("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":16}}`)
	event("message_stop", `{"type":"message_stop"}`)
}

// ---- the overlays a pane is told through PI_CONFIG_FILES -------------------------------------------

func writeProofFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// writeRoleOverlay writes one claim's own pane overlay (`role_config_files`): the single model
// that claim's stand-in serves, for every model role, so no role of the session reaches a model
// the run answers nothing for and the claim's chain has no fallback below it. The operator's own
// overlay, which every pane names before this one, is each proof's own, since the two set
// different keys in it.
func writeRoleOverlay(t *testing.T, path, model string) {
	t.Helper()
	var body strings.Builder
	body.WriteString("# The role's own pane overlay (role_config_files): one model, so its chain has no fallback.\nmodelRoles:\n")
	for _, role := range []string{"default", "smol", "slow", "vision", "plan", "commit", "tiny", "task", "advisor", "review", "oracle", "deep"} {
		fmt.Fprintf(&body, "  %s: %s\n", role, model)
	}
	writeProofFile(t, path, body.String())
}

// ---- the daemon's log as a step reads it back ----------------------------------------------------

// loggedRecord is one record the daemon wrote, as a step reads it back: the message, its
// attributes decoded from JSON, and the moment it was written.
type loggedRecord struct {
	Message string
	Attrs   map[string]any
	at      time.Time
}

// recordingLog is the daemon's own log handler with every record also kept in memory, so a step
// can read what the daemon decided and when. The records still go wherever liveProof.log() sends
// them, which is the script's evidence file. A handler the daemon derives with WithAttrs or
// WithGroup is another recordingLog over the derived inner handler, keeping its records on the
// same keptRecords and carrying the derivation's attributes, so a step reads a claim's `claim`
// attribute whether the daemon set it per record or on the logger.
type recordingLog struct {
	*keptRecords
	inner slog.Handler
	attrs []slog.Attr
}

// keptRecords is the records the root handler and every handler derived from it share.
type keptRecords struct {
	mu      sync.Mutex
	records []loggedRecord
}

func newRecordingLog(logger *slog.Logger) *recordingLog {
	return &recordingLog{keptRecords: &keptRecords{}, inner: logger.Handler()}
}

func (l *recordingLog) Enabled(context.Context, slog.Level) bool { return true }

func (l *recordingLog) Handle(ctx context.Context, record slog.Record) error {
	kept := loggedRecord{Message: record.Message, Attrs: map[string]any{}, at: time.Now()}
	for _, attr := range l.attrs {
		kept.Attrs[attr.Key] = decodeAttr(attr.Value)
	}
	record.Attrs(func(attr slog.Attr) bool {
		kept.Attrs[attr.Key] = decodeAttr(attr.Value)
		return true
	})
	l.mu.Lock()
	l.records = append(l.records, kept)
	l.mu.Unlock()
	if l.inner.Enabled(ctx, record.Level) {
		return l.inner.Handle(ctx, record)
	}
	return nil
}

// decodeAttr is one attribute as a step compares it: a number as float64, whatever its Go type, so
// a step's own literal need not match the daemon's; a duration and every other value as the text
// the log shows.
func decodeAttr(value slog.Value) any {
	switch value.Kind() {
	case slog.KindInt64:
		return float64(value.Int64())
	case slog.KindUint64:
		return float64(value.Uint64())
	case slog.KindFloat64:
		return value.Float64()
	case slog.KindBool:
		return value.Bool()
	case slog.KindString:
		return value.String()
	default:
		return value.String()
	}
}

func (l *recordingLog) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &recordingLog{keptRecords: l.keptRecords, inner: l.inner.WithAttrs(attrs),
		attrs: append(append([]slog.Attr{}, l.attrs...), attrs...)}
}

func (l *recordingLog) WithGroup(name string) slog.Handler {
	return &recordingLog{keptRecords: l.keptRecords, inner: l.inner.WithGroup(name), attrs: l.attrs}
}

func (l *recordingLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.records)
}

func (l *recordingLog) matching(want func(loggedRecord) bool) []loggedRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	var matched []loggedRecord
	for _, kept := range l.records {
		if want(kept) {
			matched = append(matched, kept)
		}
	}
	return matched
}

// firstAfter is the first record written after the first from, matching want.
func (l *recordingLog) firstAfter(from int, want func(loggedRecord) bool) *loggedRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := from; i < len(l.records); i++ {
		if want(l.records[i]) {
			kept := l.records[i]
			return &kept
		}
	}
	return nil
}
