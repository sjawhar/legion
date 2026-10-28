package intake_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

const (
	redeliveredProject  = "ACME"
	redeliveredIssue    = "ACME-7"
	redeliveredRepo     = "acme/widgets"
	redeliveredNumber   = 7
	redeliveredGUID     = "8a4f2c10-b5e6-11f0-9d3e-0c7f1e2a4b6d"
	redeliveredSecret   = "redelivered-webhook-secret"
	redeliveredClientID = "Iv1.redelivered"
	githubSubjects      = "notifications.github.>"
)

// redeliveredRepositories serves acme/widgets at main, as the daemon's mapping would.
type redeliveredRepositories struct{}

func (redeliveredRepositories) ResolveRepository(resolved string) (workflow.Resolution, error) {
	if resolved == redeliveredRepo {
		return workflow.Resolution{Repo: redeliveredRepo, DefaultBranch: "main", GitHubRepo: redeliveredRepo}, nil
	}
	return workflow.Resolution{Refusal: resolved + " is not mapped to project " + redeliveredProject + " in Dispatch"}, nil
}

// GitHub sends a pull request opened on an issue's branch while the stream does not capture
// GitHub's subjects, so Envoy's listener cannot store it and answers 503, which GitHub records as
// a failed attempt and never retries on its own. Once the stream captures the subjects again,
// Dispatch's redelivery sweep (`envoy-dispatch redeliver-webhooks`) finds the failed attempt and
// asks GitHub to redeliver it; GitHub re-sends the original body under the same delivery GUID, the
// listener stores it, and the daemon's consumer applies it with the workflow's handler: the pull
// request is recorded on its issue, once, under the delivery's fact id. The listener and the
// sweep are the real binaries; GitHub is a fake that keeps the App webhook's delivery log.
func TestADeliveryNATSRefusedIsRedeliveredByTheSweepAndApplied(t *testing.T) {
	ctx := context.Background()
	binaries := buildEnvoyBinaries(t)

	dsn := testpg.Database(t, "legion_intake_redelivered_test")
	pool := drainPool(t, dsn)
	seedRedeliveredIssue(t, pool)

	js := testnats.JetStream(t)
	stream := testnats.CreateStream(t, js, jetstream.StreamConfig{Name: "ENVOY_NOTIFICATIONS", Subjects: []string{"notifications.>"}})
	natsURL := js.Conn().ConnectedUrl()

	listener := startListener(t, binaries.listener, natsURL)

	logs := &warnings{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	repository := ghrepo.MustParse(redeliveredRepo)
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: redeliveredProject, Repositories: redeliveredRepositories{}}, log)
	admission := admit.New(records, engine, 10, redeliveredProject, log)
	consumers, err := intake.OpenConsumers(ctx, js, pool, intake.ConsumerSpec{
		Project: redeliveredProject, Repositories: []ghrepo.Repository{repository},
		AckWait: 30 * time.Second, NakDelay: time.Second, Logger: log,
	})
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	run, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- consumers.Run(run, blockers.NewMirror(records, redeliveredProject), engine, admission) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	})

	// The stream stops capturing GitHub's subjects: the listener reconciled it when it started,
	// so its subjects are the listener's own, and every one but GitHub's stays.
	captured := streamSubjects(t, stream)
	if !slices.Contains(captured, githubSubjects) {
		t.Fatalf("the stream the listener reconciled captures %v, want %s among them", captured, githubSubjects)
	}
	setStreamSubjects(t, js, stream, slices.DeleteFunc(slices.Clone(captured), func(subject string) bool { return subject == githubSubjects }))
	t.Logf("1. the stream ENVOY_NOTIFICATIONS captures %v: no %s", streamSubjects(t, stream), githubSubjects)

	key, pemText := redeliveredAppKey(t)
	github := newFakeGitHub(t, &key.PublicKey, redeliveredClientID, "http://127.0.0.1:"+strconv.Itoa(listener.port)+"/webhook/github")
	body := redeliveredPullRequestOpened()
	original := github.deliver(redeliveredGUID, "pull_request", "opened", body)
	if original.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("the listener answered the delivery %d, want 503 while the stream does not capture it:\n%s", original.StatusCode, listener.output.String())
	}
	t.Logf("2. GitHub delivered pull_request.opened %s (%s#%d on legion/%s): the listener answered %d (%s); GitHub's log lists attempt %d, status_code %d, status %q",
		redeliveredGUID, redeliveredRepo, redeliveredNumber, redeliveredIssue, original.StatusCode,
		listener.line(t, "github publish failed"), original.ID, original.StatusCode, original.Status)
	subject := "notifications.github.acme.widgets.pr." + strconv.Itoa(redeliveredNumber)
	if n := storedOn(t, stream, subject); n != 0 {
		t.Fatalf("the stream holds %d messages on %s after the refused publish, want none", n, subject)
	}

	setStreamSubjects(t, js, stream, captured)
	t.Logf("3. the stream captures %s again", githubSubjects)

	sweep := exec.Command(binaries.dispatch, "redeliver-webhooks", "--since", "1h")
	home := t.TempDir()
	sweep.Dir = home
	sweep.Env = []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"NATS_URLS=" + natsURL,
		"DISPATCH_APP_CLIENT_ID=" + redeliveredClientID,
		"DISPATCH_APP_PEM_B64=" + base64.StdEncoding.EncodeToString([]byte(pemText)),
		"DISPATCH_GITHUB_API_BASE=" + github.server.URL,
	}
	output, err := sweep.CombinedOutput()
	if err != nil {
		t.Fatalf("envoy-dispatch redeliver-webhooks --since 1h: %v\n%s", err, output)
	}
	t.Logf("4. envoy-dispatch redeliver-webhooks --since 1h exited 0:\n%s", strings.TrimSpace(string(output)))

	requests, attempts := github.requestsAndLog()
	if !slices.Equal(requests, []int64{original.ID}) {
		t.Fatalf("redelivery requests %v, want one, for attempt %d", requests, original.ID)
	}
	if len(attempts) != 2 || attempts[1].GUID != redeliveredGUID || !attempts[1].Redelivery || attempts[1].StatusCode != http.StatusOK {
		t.Fatalf("GitHub's delivery log %+v, want the 503 and one redelivery of %s answered 200", attempts, redeliveredGUID)
	}
	t.Logf("5. the sweep asked GitHub once (POST /app/hook/deliveries/%d/attempts); GitHub re-sent %s and the listener answered %d (attempt %d, redelivery)",
		original.ID, redeliveredGUID, attempts[1].StatusCode, attempts[1].ID)

	factID := "github." + redeliveredGUID
	testwait.Eventually(t, "the daemon to apply "+factID, func() bool {
		return slices.Contains(processedGitHubEvents(t, pool), factID)
	})
	if got := processedGitHubEvents(t, pool); !slices.Equal(got, []string{factID}) {
		t.Errorf("processed events %v, want exactly %s", got, factID)
	}
	if n := storedOn(t, stream, subject); n != 1 {
		t.Errorf("the stream holds %d messages on %s, want the redelivered one", n, subject)
	}
	var pr *record.PullRequest
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		pr, err = records.PullRequestByNumber(ctx, tx, redeliveredRepo, redeliveredNumber)
		return err
	}); err != nil {
		t.Fatalf("read the recorded pull request: %v", err)
	}
	if pr == nil || pr.Issue != redeliveredIssue || pr.Branch != "legion/"+redeliveredIssue || pr.HeadSHA != redeliveredHead || pr.State != record.PullRequestOpen {
		t.Fatalf("%s#%d recorded as %+v, want open on %s at %s", redeliveredRepo, redeliveredNumber, pr, redeliveredIssue, redeliveredHead)
	}
	var recorded int
	if err := pool.QueryRow(ctx, `select count(*) from pull_requests`).Scan(&recorded); err != nil {
		t.Fatalf("count the recorded pull requests: %v", err)
	}
	if recorded != 1 {
		t.Errorf("%d pull requests recorded, want %s's one", recorded, redeliveredIssue)
	}
	if logged := logs.String(); logged != "" {
		t.Errorf("the consumer and the handlers logged, want nothing at warn or above:\n%s", logged)
	}
	t.Logf("6. the daemon applied %s once: processed_events holds %v, and %s#%d is recorded open on %s at %s",
		factID, processedGitHubEvents(t, pool), redeliveredRepo, redeliveredNumber, pr.Issue, pr.HeadSHA)
}

const redeliveredHead = "4c1e9a7b2d5f8e0a3c6b9d2f5e8a1c4b7d0e3f6a"

// redeliveredPullRequestOpened is the body GitHub sends for a pull request opened on the issue's
// branch.
func redeliveredPullRequestOpened() []byte {
	return drainJSON(map[string]any{
		"action": "opened",
		"number": redeliveredNumber,
		"pull_request": map[string]any{
			"number": redeliveredNumber, "title": "Widgets", "body": "Dispatch: " + redeliveredIssue,
			"user":     map[string]any{"login": "implementer", "type": "User"},
			"html_url": "https://github.com/" + redeliveredRepo + "/pull/" + strconv.Itoa(redeliveredNumber),
			"head":     map[string]any{"sha": redeliveredHead, "ref": "legion/" + redeliveredIssue},
			"base":     map[string]any{"sha": strings.Repeat("0", 40), "ref": "main"},
			"merged":   false, "merge_commit_sha": nil, "merged_by": nil, "updated_at": "2026-10-28T09:00:00Z",
		},
		"repository":   map[string]any{"id": 4242, "name": "widgets", "full_name": redeliveredRepo, "owner": map[string]any{"login": "acme"}},
		"sender":       map[string]any{"login": "implementer", "type": "User"},
		"installation": map[string]any{"id": 135459902},
	})
}

// seedRedeliveredIssue records the issue implementing, slotted, with its Dispatch mirror row.
func seedRedeliveredIssue(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `insert into issues (key, tree, project, title, parent, phase, generation, status, dispatch_status, rank,
			handed_over, last_dispatch_seq, repo, default_branch, github_repo)
			values ($1, $1, $2, $1, null, $3, 1, 'in_progress', 'in_progress', 'M', true, 1, $4, 'main', $4)`,
			redeliveredIssue, redeliveredProject, string(phase.Implementing), redeliveredRepo); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `insert into dispatch_issues (key, project, status, blockers, dispatch_seq) values ($1, $2, 'in_progress', '{}', 1)`,
			redeliveredIssue, redeliveredProject); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `insert into slots (issue, index, admitted_at) values ($1, 0, now())`, redeliveredIssue)
		return err
	}); err != nil {
		t.Fatalf("seed %s: %v", redeliveredIssue, err)
	}
}

// envoyBinaries are Envoy's listener and Dispatch, built from this checkout.
type envoyBinaries struct{ listener, dispatch string }

func buildEnvoyBinaries(t *testing.T) envoyBinaries {
	t.Helper()
	envoy, err := filepath.Abs(filepath.Join("..", "..", "..", "envoy"))
	if err != nil {
		t.Fatalf("resolve the envoy module: %v", err)
	}
	out := t.TempDir()
	build := exec.Command("go", "build", "-C", envoy, "-o", out+string(filepath.Separator), "./cmd/listener", "./cmd/dispatch")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Envoy's listener and Dispatch: %v\n%s", err, output)
	}
	return envoyBinaries{listener: filepath.Join(out, "listener"), dispatch: filepath.Join(out, "dispatch")}
}

// listenerProcess is Envoy's listener binary serving on a loopback port the kernel chose.
type listenerProcess struct {
	port   int
	output *processOutput
}

// processOutput keeps everything a process writes to its stdout and stderr.
type processOutput struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (o *processOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.Write(p)
}

func (o *processOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

// line is the listener's first output line holding text, or "" when none does.
func (l *listenerProcess) line(t *testing.T, text string) string {
	t.Helper()
	for line := range strings.SplitSeq(l.output.String(), "\n") {
		if strings.Contains(line, text) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// startListener runs the listener against the NATS at natsURL with the GitHub webhook mounted,
// on a socket held here (ENVOY_LISTEN_FD) so no other process takes the port first, and waits for
// /healthz to say healthy: NATS connected, the stream reconciled and the listener's durable bound.
// It is stopped when t ends.
func startListener(t *testing.T, binary, natsURL string) *listenerProcess {
	t.Helper()
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold a loopback port: %v", err)
	}
	socket, err := held.(*net.TCPListener).File()
	_ = held.Close()
	if err != nil {
		t.Fatalf("take the held socket's descriptor: %v", err)
	}
	port := held.Addr().(*net.TCPAddr).Port
	cmd := exec.Command(binary)
	cmd.Env = []string{
		"PORT=" + strconv.Itoa(port),
		"ENVOY_LISTEN_HOST=127.0.0.1",
		"ENVOY_LISTEN_FD=3",
		"ENVOY_MACHINE_ID=redelivered",
		"NATS_URLS=" + natsURL,
		"ENVOY_API_TOKEN=redelivered-listener-token",
		"ENVOY_WEBHOOKS=github",
		"ENVOY_GITHUB_WEBHOOK_SECRET=" + redeliveredSecret,
		"ENVOY_REVIEWER_APP_ID=1",
	}
	cmd.ExtraFiles = []*os.File{socket}
	output := &processOutput{}
	cmd.Stdout, cmd.Stderr = output, output
	err = cmd.Start()
	_ = socket.Close()
	if err != nil {
		t.Fatalf("start the listener: %v", err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
			t.Errorf("the listener did not stop after SIGINT:\n%s", output.String())
		}
	})
	client := &http.Client{Timeout: 5 * time.Second}
	url := "http://127.0.0.1:" + strconv.Itoa(port) + "/healthz"
	testwait.Eventually(t, "the listener to report healthy", func() bool {
		select {
		case <-exited:
			t.Fatalf("the listener exited before it was healthy:\n%s", output.String())
		default:
		}
		response, err := client.Get(url)
		if err != nil {
			return false
		}
		defer response.Body.Close()
		var health struct {
			Status string `json:"status"`
		}
		return json.NewDecoder(response.Body).Decode(&health) == nil && health.Status == "healthy"
	})
	return &listenerProcess{port: port, output: output}
}

func streamSubjects(t *testing.T, stream jetstream.Stream) []string {
	t.Helper()
	info, err := stream.Info(context.Background())
	if err != nil {
		t.Fatalf("read the stream's info: %v", err)
	}
	return info.Config.Subjects
}

func setStreamSubjects(t *testing.T, js jetstream.JetStream, stream jetstream.Stream, subjects []string) {
	t.Helper()
	info, err := stream.Info(context.Background())
	if err != nil {
		t.Fatalf("read the stream's info: %v", err)
	}
	config := info.Config
	config.Subjects = subjects
	if _, err := js.UpdateStream(context.Background(), config); err != nil {
		t.Fatalf("set the stream's subjects to %v: %v", subjects, err)
	}
}

// storedOn counts the stream's messages on subject.
func storedOn(t *testing.T, stream jetstream.Stream, subject string) uint64 {
	t.Helper()
	info, err := stream.Info(context.Background(), jetstream.WithSubjectFilter(subject))
	if err != nil {
		t.Fatalf("read the stream's messages on %s: %v", subject, err)
	}
	return info.State.Subjects[subject]
}

func redeliveredAppKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate the App key: %v", err)
	}
	return key, string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

// hookDelivery is one attempt in the App webhook's delivery log, in the shape GitHub's
// `GET /app/hook/deliveries` serves it.
type hookDelivery struct {
	ID             int64     `json:"id"`
	GUID           string    `json:"guid"`
	DeliveredAt    time.Time `json:"delivered_at"`
	Redelivery     bool      `json:"redelivery"`
	Duration       float64   `json:"duration"`
	Status         string    `json:"status"`
	StatusCode     int       `json:"status_code"`
	Event          string    `json:"event"`
	Action         *string   `json:"action"`
	InstallationID int64     `json:"installation_id"`
	RepositoryID   int64     `json:"repository_id"`
}

// fakeGitHub keeps the App webhook's delivery log and serves its listing and redelivery request,
// each only to an App JWT signed by the App's key for its client id. A delivery is GitHub posting
// the body to the webhook URL, signed with the webhook secret, under its delivery GUID and event;
// a redelivery re-posts the recorded attempt's body under the same GUID.
type fakeGitHub struct {
	t         *testing.T
	server    *httptest.Server
	publicKey *rsa.PublicKey
	clientID  string
	webhook   string
	client    *http.Client

	mu       sync.Mutex
	log      []hookDelivery
	bodies   map[int64][]byte
	nextID   int64
	requests []int64
}

func newFakeGitHub(t *testing.T, publicKey *rsa.PublicKey, clientID, webhook string) *fakeGitHub {
	t.Helper()
	g := &fakeGitHub{t: t, publicKey: publicKey, clientID: clientID, webhook: webhook,
		client: &http.Client{Timeout: 10 * time.Second}, bodies: map[int64][]byte{}, nextID: 3_844_000_000_000_000_000}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /app/hook/deliveries", g.list)
	mux.HandleFunc("POST /app/hook/deliveries/{id}/attempts", g.attempt)
	g.server = httptest.NewServer(mux)
	t.Cleanup(g.server.Close)
	return g
}

// deliver posts body to the webhook as GitHub does and records the attempt with the status the
// webhook answered (0 when it gave no HTTP answer).
func (g *fakeGitHub) deliver(guid, event, action string, body []byte) hookDelivery {
	return g.send(guid, event, action, body, false)
}

func (g *fakeGitHub) send(guid, event, action string, body []byte, redelivery bool) hookDelivery {
	request, err := http.NewRequest(http.MethodPost, g.webhook, bytes.NewReader(body))
	if err != nil {
		g.t.Errorf("webhook request: %v", err)
		return hookDelivery{}
	}
	mac := hmac.New(sha256.New, []byte(redeliveredSecret))
	mac.Write(body)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "GitHub-Hookshot/redelivered")
	request.Header.Set("X-GitHub-Delivery", guid)
	request.Header.Set("X-GitHub-Event", event)
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	started := time.Now()
	code := 0
	if response, err := g.client.Do(request); err == nil {
		code = response.StatusCode
		_ = response.Body.Close()
	}
	status := "OK"
	if code < 200 || code > 299 {
		status = fmt.Sprintf("Invalid HTTP Response: %d", code)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nextID += 17
	recorded := hookDelivery{ID: g.nextID, GUID: guid, DeliveredAt: started.UTC(), Redelivery: redelivery,
		Duration: time.Since(started).Seconds(), Status: status, StatusCode: code, Event: event,
		Action: &action, InstallationID: 135459902, RepositoryID: 4242}
	g.log = append(g.log, recorded)
	g.bodies[recorded.ID] = body
	return recorded
}

// requestsAndLog is the delivery ids a redelivery was requested for, in order, and every attempt
// in the order GitHub made them.
func (g *fakeGitHub) requestsAndLog() ([]int64, []hookDelivery) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.requests), slices.Clone(g.log)
}

// list serves the whole log newest first on one page.
func (g *fakeGitHub) list(w http.ResponseWriter, r *http.Request) {
	if !g.verifyAppJWT(w, r) {
		return
	}
	g.mu.Lock()
	rows := slices.Clone(g.log)
	g.mu.Unlock()
	slices.Reverse(rows)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(rows); err != nil {
		g.t.Errorf("encode the delivery log: %v", err)
	}
}

// attempt answers a redelivery request as GitHub does: it re-sends the recorded attempt, records
// the redelivery's outcome under the original GUID, and answers 202.
func (g *fakeGitHub) attempt(w http.ResponseWriter, r *http.Request) {
	if !g.verifyAppJWT(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	g.mu.Lock()
	g.requests = append(g.requests, id)
	body, found := g.bodies[id]
	var original hookDelivery
	for _, row := range g.log {
		if row.ID == id {
			original = row
		}
	}
	g.mu.Unlock()
	if !found {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	g.send(original.GUID, original.Event, *original.Action, body, true)
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte("{}"))
}

// verifyAppJWT refuses a request whose bearer is not an RS256 JWT signed by the App's key with the
// App's client id as its issuer and an expiry at most ten minutes ahead, as GitHub does, and fails
// the test for it.
func (g *fakeGitHub) verifyAppJWT(w http.ResponseWriter, r *http.Request) bool {
	refuse := func(format string, args ...any) bool {
		g.t.Errorf("%s %s: "+format, append([]any{r.Method, r.URL.Path}, args...)...)
		http.Error(w, `{"message":"A JSON web token could not be decoded"}`, http.StatusUnauthorized)
		return false
	}
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	parts := strings.Split(raw, ".")
	if !ok || len(parts) != 3 {
		return refuse("Authorization %q is no App JWT", r.Header.Get("Authorization"))
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return refuse("decode the JWT signature: %v", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(g.publicKey, crypto.SHA256, digest[:], signature); err != nil {
		return refuse("the JWT does not verify against the App's key: %v", err)
	}
	encoded, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return refuse("decode the JWT claims: %v", err)
	}
	var claims struct {
		Iss string `json:"iss"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(encoded, &claims); err != nil {
		return refuse("parse the JWT claims: %v", err)
	}
	if now := time.Now().Unix(); claims.Iss != g.clientID || claims.Exp <= now || claims.Exp > now+10*60 {
		return refuse("JWT iss %q exp %d, want %q and an expiry within ten minutes", claims.Iss, claims.Exp, g.clientID)
	}
	return true
}
