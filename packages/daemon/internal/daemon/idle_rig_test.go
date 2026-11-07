package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	legionruntime "github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/sessionproxy"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testtmp"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// idleRigImage is the worker image the rig's configuration names, pinned by digest as the runtime
// requires. Every pod the rig's controller makes runs it, so no pod is ever held back for an old
// image.
const idleRigImage = "ghcr.io/example/worker@sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// idleRigIdleMinutes and idleRigBackstopMinutes are the rig's `runtime.kubernetes.idle_release`.
// The idle period is long enough that a pod a wake brought back is not released again while the
// test still reads it, and the release wave costs nothing for it: the rule measures the period
// against the claim rows, which the rig back-dates half an hour (idleWakeRig.quiet), so the first
// release comes at the first tick either way. The backstop is long enough that no pod is woken by one it did not ask
// for: the subtest that drives the backstop brings its own pod's due time forward instead
// (idleWakeRig.dueBackstop), as the stuck-resume pass's ten minutes is driven by the recorded
// wake time (idleWakeRig.ageResume). Both are the clock those passes read, and nothing else.
const (
	idleRigIdleMinutes     = 5
	idleRigBackstopMinutes = 10
)

// idleRigTick is how often the rig's daemon decides every pod and fires the backstops that came
// due (overrides.idleTick, the daemon's own minute in production). Every moment the rig drives is
// a row it back-dates — the quiet period, a backstop's due time, a wake's age — so the tick's
// period buys nothing but the wait for it, and a quarter of a second takes minutes off the run
// while leaving each pass as far apart as the work it does.
const idleRigTick = 250 * time.Millisecond

// idleWakeRig is the acceptance rig of INFRA-945's A3: the Go daemon on a real Postgres, a real
// nats-server, a real Envoy listener and the Sandbox runtime over a fake Kubernetes API whose
// controller stand-in starts a real `legion launcher` for every pod it reports Ready, each
// launcher running a scripted stand-in for Oh My Pi that registers, claims its role and records
// every message it is given.
//
// Where each part comes from:
//
//   - the whole-daemon boot, its Dispatch and GitHub stand-ins, its store seeding and its shim
//     protocol: packages/daemon/internal/daemon/check_wake_test.go's checkWakeRig;
//   - the fake Kubernetes API, its Sandbox-controller stand-in and its pods:
//     packages/daemon/internal/runtime/sandbox/rig_test.go, rebuilt over HTTP
//     (idle_rig_kube_test.go);
//   - the real launcher per Ready pod: packages/daemon/cmd/legion/launcher_test.go's in-process
//     launcher (idle_rig_pod_test.go);
//   - the scripted Oh My Pi: packages/daemon/internal/capabilities/image_test.go's fakeOmp, grown
//     a registration, a role claim and a record of every delivery (idle_rig_agent_test.go);
//   - Postgres through internal/testpg's database (isolatedOutboxPool) and NATS through
//     internal/testnats. This pod has no Docker, so testnats serves the nats-server
//     LEGION_TEST_NATS_URL and LEGION_TEST_NATS_MONITOR_URL name rather than a container, which is
//     what that package does when both are set;
//   - the real Envoy listener, built from packages/envoy/cmd/listener and run as its own process
//     (idle_rig_listener_test.go).
type idleWakeRig struct {
	t        *testing.T
	cfg      config.Config
	pool     *pgxpool.Pool
	natsURL  string
	js       jetstream.JetStream
	board    *liveDispatch
	kube     *issueSandboxAPI
	pods     *idleRigPods
	agents   *idleRigAgents
	listener *idleRigListener
	records  record.Store
	project  string
	logs     *syncBuffer
	built    *built
	api      *daemon
	// issues are the rig's issues, in the order they were seeded.
	issues []idleRigIssue
	// daemonLog is where a copy of the daemon's log is kept.
	daemonLog *os.File
	// baseline is how many tasks each role had been given when markPrompts was called, so a
	// subtest counts the tasks a wake gave and not the phase start the daemon's boot gave.
	baseline map[string]int
	// toolsPath holds the rig's `legion` and `omp` stand-ins, made once.
	toolsPath string

	// stop ends the running daemon, and done answers when it has.
	stopMu sync.Mutex
	stop   context.CancelFunc
	done   chan error
}

// idleRigIssue is one issue of the rig: its key, the pod its claims run in, and the role that pod
// runs.
type idleRigIssue struct {
	// name is what the test calls the issue; key is the issue key its project gives it.
	name string
	key  string
	pod  string
	role claim.Role
}

// newIdleWakeRig readies every part and boots the daemon, with one issue per name given. Each
// issue is its own tree root, in the workflow at `implementing` with its implementer's claim
// suspended, which the rig then resumes: that launch is what makes the pod, its launcher and its
// role process.
func newIdleWakeRig(t *testing.T, issues ...string) *idleWakeRig {
	t.Helper()
	pool := isolatedOutboxPool(t)
	if _, err := pool.Exec(context.Background(), idleRigSessionTable()); err != nil {
		t.Fatalf("make the session table the launchers look a resume up in: %v", err)
	}
	natsURL := testnats.URL(t)
	// The listener owns the notification stream: it creates it with its own subjects before the
	// daemon binds a consumer on it, which is the order a deployment rolls them in.
	listener := startIdleRigListener(t, natsURL, "idlerig")
	cfg := workflowConfig(t, natsURL)
	cfg.PostgresDSN = pool.Config().ConnString()
	cfg.EnvoyURL = listener.url
	// One admission slot per issue of the rig, and a cap with room for them all: an issue the
	// daemon has not admitted is one its boot lingers closed.
	cfg.AdmissionCap = len(issues) + 4
	// Every launcher of the rig reaches that database through its generation's proxy, which
	// carries the launcher's own startup packet to it unchanged, so the password the rig's DSN
	// names has to be in the environment before the first launcher starts
	// (idleRigSessionPassword).
	idleRigSessionPassword(t, pool)
	cfg.Runtime = config.Runtime{Name: "kubernetes", Kubernetes: &config.Kubernetes{
		Namespace: sandboxNamespace, Image: idleRigImage, StorageClass: "standard", IssueVolume: "1Gi",
		Resources: config.DefaultResources(), RepositoryCache: config.DefaultRepositoryCache(),
		SessionStore: ompsessions.Store{Kind: ompsessions.StoreCloudSQLIAM,
			Database: ompsessions.Database{Instance: "legion-test:us-central1:sessions", Name: idleRigDatabase(t, pool), User: idleRigUser(t, pool)}},
		IdleRelease: config.IdleReleaseConfig{IdleMinutes: idleRigIdleMinutes, BackstopMinutes: idleRigBackstopMinutes},
	}}
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	// The rig's board lists the issues it holds: the daemon's boot lingers closed every recorded
	// issue Dispatch's listing does not name.
	board := newLiveDispatch(t, cfg.Project)
	board.lists = true
	cfg.DispatchURL = board.url
	rig := &idleWakeRig{t: t, cfg: cfg, board: board, pool: pool, natsURL: natsURL, js: workflowJetStream(t, natsURL),
		kube: newIssueSandboxAPI(), agents: newIdleRigAgents(t, pool), listener: listener,
		records: record.NewStore(), project: project, logs: &syncBuffer{}, built: &built{}}
	rig.pods = newIdleRigPods(t, rig.address, cfg.Runtime.Kubernetes.SessionStore.Database,
		func(context.Context) (sessionproxy.Dialer, error) {
			return idleRigDialer{address: idleRigHostPort(t, pool)}, nil
		})
	rig.kube.serve(t)
	// Boot reads runtime.kubernetes' kubeconfig whatever runtime it is given (readSandbox,
	// kubeClient), and with none it reads the pod's in-cluster credentials, so the rig names its
	// own stand-in API: it runs the same way outside a cluster as in one, and never holds a
	// credential of the cluster it runs in. Nothing dials this client, since the runtime the
	// boot is given is the rig's own (prepareSandbox returns before it builds one).
	rig.cfg.Runtime.Kubernetes.Kubeconfig = writeKubeconfig(t, rig.kube.url, "test")
	rig.kube.onPodReady = rig.podReady
	rig.kube.onPodGone = rig.pods.podGone
	rig.kube.run(t)
	t.Setenv(idleRigURLVariable, rig.agents.url)

	for i, name := range issues {
		rig.issues = append(rig.issues, rig.seed(name, cfg.Project+"-"+strconv.Itoa(i+1), i))
	}
	rig.boot()
	rig.awaitListenerReady()
	return rig
}

// listenerReports is every row of listener_reports, by the project each is filed under.
func (r *idleWakeRig) listenerReports() map[string]string {
	rows, err := r.pool.Query(context.Background(), "select project, ready_at from listener_reports")
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	defer rows.Close()
	held := map[string]string{}
	for rows.Next() {
		var project string
		var at time.Time
		if err := rows.Scan(&project, &at); err != nil {
			return map[string]string{"error": err.Error()}
		}
		held[project] = at.UTC().Format(time.RFC3339Nano)
	}
	return held
}

// awaitListenerReady waits until the daemon has read a `listener_ready` report of the Envoy
// listener, which is R7's gate: it releases nothing until it has. The listener publishes one at
// its start and every five minutes after, so a listener that came up before this daemon's durable
// consumer existed is started again rather than waited out.
func (r *idleWakeRig) awaitListenerReady() {
	r.t.Helper()
	defer func() {
		if r.t.Failed() {
			r.t.Logf("the Envoy listener's log:\n%s", r.listener.log())
			r.t.Logf("listener_reports holds %v; this daemon's project token is %q and its project key is %q",
				r.listenerReports(), r.project, r.cfg.Project)
		}
	}()
	restarted := false
	began := time.Now()
	idleRigEventually(r.t, 90*time.Second, "the daemon reading the Envoy listener's ready report", func() bool {
		var found bool
		if err := r.pool.QueryRow(context.Background(),
			"select exists(select 1 from listener_reports where project = $1)", r.project).Scan(&found); err != nil {
			r.t.Fatalf("read the listener's report: %v", err)
		}
		if found {
			return true
		}
		// A durable consumer this daemon has just created starts at the next message
		// (intake's createConsumer), so the report the listener published at its own start is
		// behind it. The listener is started again on the same port, which publishes a fresh
		// one that the consumer does see and leaves every URL already handed out valid.
		if !restarted && time.Since(began) > 2*time.Second {
			restarted = true
			port := r.listener.port
			r.listener.stop()
			r.listener = startIdleRigListenerOn(r.t, r.natsURL, "idlerig", port)
		}
		return false
	})
}

// issue is the seeded issue the test calls name.
func (r *idleWakeRig) issue(name string) idleRigIssue {
	r.t.Helper()
	for _, issue := range r.issues {
		if issue.name == name {
			return issue
		}
	}
	r.t.Fatalf("the rig seeded no issue called %s", name)
	return idleRigIssue{}
}

// launchAll launches every seeded issue's pod, in order. It is apart from the rig's construction
// so a test can tell a role process what to do — the topics it takes with envoy_subscribe, whether
// it takes a turn — before the process starts.
func (r *idleWakeRig) launchAll() {
	r.t.Helper()
	for _, issue := range r.issues {
		r.launch(issue)
	}
}

// idleRigDatabase and idleRigUser are the rig's own database and login, which its launchers'
// session proxies carry the generation's session lookup to.
func idleRigDatabase(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	return pool.Config().ConnConfig.Database
}

func idleRigUser(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	return pool.Config().ConnConfig.User
}

// idleRigSessionPassword puts the rig's database password where a launcher's own session client
// finds it, libpq's PGPASSWORD, which is the only place left for one: that client builds its URL
// from the endpoint alone (ompsessions.Endpoint.ClientURL), which carries no password, and the
// generation's proxy is a byte copy that hands the client's startup packet to the server
// unchanged, so a server that asks for one refuses the login (`failed SASL auth`, SQLSTATE
// 28P01) however the proxy's upstream was dialed. A pod's Cloud SQL asks for none — the
// connector mints the login as it dials and no password is ever written into a pod
// (internal/sessionproxy) — so this is the rig's stand-in for that identity and no production
// path reads it, as internal/launcher's sessionDatabaseAt is for that package's tests. A
// trust-authenticated server, which this repository's own pod serves, names no password, and an
// empty PGPASSWORD is no setting at all (pgx's parseEnvSettings keeps only what is non-empty),
// so the rig runs the same against either.
func idleRigSessionPassword(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	t.Setenv("PGPASSWORD", pool.Config().ConnConfig.Password)
}

// idleRigHostPort is the rig's Postgres as a dialable address, which the session proxy's upstream
// dialer connects to.
func idleRigHostPort(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	conn := pool.Config().ConnConfig
	return conn.Host + ":" + strconv.Itoa(int(conn.Port))
}

// address is the worker stream the daemon bound, which every launcher dials.
func (r *idleWakeRig) address() string {
	r.built.mu.Lock()
	defer r.built.mu.Unlock()
	return r.built.address
}

// podReady starts the pod's launchers, for the roles the rig gave the issue a claim, and records
// where each launcher writes its generations so the role process can be handed its boot token.
func (r *idleWakeRig) podReady(pod, uid string, roles map[string]string) {
	key := pod
	for _, issue := range r.issues {
		if issue.pod == pod {
			key = issue.key
		}
	}
	wanted := map[string]string{}
	for role, token := range roles {
		if role == string(claim.RoleImplementer) {
			wanted[role] = token
		}
	}
	r.pods.podReady(pod, uid, wanted, func(role, dir string) { r.agents.setPrivate(key, role, dir) })
}

// seed records one issue, its tree's lifecycle and its implementer's suspended claim, as
// check_wake_test.go's rig seeds a claim the daemon will resume. The issue is its own tree root,
// in the workflow at `implementing`, so the idle rule reads a live tree and the backstop has a
// phase worker to arm for (workflow.BackstopRole).
func (r *idleWakeRig) seed(name, key string, index int) idleRigIssue {
	r.t.Helper()
	r.board.show(key, "in_progress")
	token, err := claim.NewToken(r.project, key, claim.RoleImplementer)
	if err != nil {
		r.t.Fatal(err)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, r.cfg.PostgresDSN)
	if err != nil {
		r.t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	if err := pgx.BeginFunc(ctx, st.Pool(), func(tx pgx.Tx) error {
		if err := r.records.PutIssue(ctx, tx, record.Issue{
			Key: key, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Project: r.cfg.Project,
			Tree: key, Title: "An idle pod", Phase: phase.Implementing, Generation: 1, Status: "in_progress",
			HandedOver: true, Rank: "U",
		}); err != nil {
			return err
		}
		if err := r.records.PutSlot(ctx, tx, record.Slot{Issue: key, Index: index, AdmittedAt: time.Now()}); err != nil {
			return err
		}
		if name != idleRigChecks {
			return nil
		}
		return r.records.PutPullRequest(ctx, tx, record.PullRequest{
			Issue: key, Repo: liveRepo, Number: idleRigPullNumber(key), Branch: "legion/" + key,
			HeadSHA: idleRigHead(key), HeadUpdatedAt: time.Now().Add(-time.Hour).UTC(), State: record.PullRequestOpen,
		})
	}); err != nil {
		r.t.Fatalf("record issue %s: %v", key, err)
	}
	if _, err := st.OpenTreeLifecycle(ctx, r.project, key, treelifecycle.AuthorityWorkflow); err != nil {
		r.t.Fatalf("open the lifecycle of %s: %v", key, err)
	}
	if _, err := st.AdmitClaim(ctx, supervise.Claim{
		Token: token, Project: r.project, Tree: key, Issue: key, Role: claim.RoleImplementer, Generation: 1,
		State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByDaemon,
	}); err != nil {
		r.t.Fatalf("record the suspended implementer of %s: %v", key, err)
	}
	writePrompt(r.t, r.cfg, token)
	return idleRigIssue{name: name, key: key, pod: sandbox.SandboxName(token), role: claim.RoleImplementer}
}

// idleRigPullNumber is the pull request number of an issue, derived from its key so each issue has
// one of its own.
func idleRigPullNumber(key string) int {
	sum := 0
	for _, b := range []byte(key) {
		sum = (sum*31 + int(b)) % 9000
	}
	return 1000 + sum
}

// idleRigHead is the commit at the head of an issue's pull request, which its CI settlement names.
func idleRigHead(key string) string {
	head := ""
	for len(head) < 40 {
		head += fmt.Sprintf("%x", key)
	}
	return head[:40]
}

// boot runs the daemon over the rig until the test ends, returning once it answers /healthz. The
// runtime it is given is the real Sandbox runtime over the rig's Kubernetes API.
func (r *idleWakeRig) boot() {
	r.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	// The log's writer is taken here, on the test's own goroutine, and not in the goroutine
	// below. Taking it registers the cleanup that prints the log's tail, and cleanups run in the
	// reverse of the order they were registered in: registered before r.stopDaemon below, the
	// tail is printed after the daemon has stopped, so it carries the shutdown's own lines and it
	// sees a failure the stop itself raised. Taken in the goroutine, it would usually be
	// registered after r.stopDaemon and so run before it, printing a log that ends where the
	// shutdown begins, or none at all when the stop is what failed.
	logger := slog.New(slog.NewJSONHandler(io.MultiWriter(r.logs, r.logFile()), &slog.HandlerOptions{Level: slog.LevelDebug}))
	go func() {
		done <- run(ctx, r.cfg, logger, overrides{
			listen: heldListen, runtime: r.newRuntime, clock: stillClock{}, idleTick: idleRigTick,
			workflowTokens: idleRigAppTokens{workflowTokenRecorder: &workflowTokenRecorder{}}, githubAPI: repositoriesGitHub(r.t),
		})
	}()
	r.stopMu.Lock()
	r.stop, r.done = cancel, done
	r.stopMu.Unlock()
	r.t.Cleanup(r.stopDaemon)
	awaitHealthz(r.t, r.cfg, done)
	transport := &http.Transport{}
	r.t.Cleanup(transport.CloseIdleConnections)
	r.api = &daemon{t: r.t, cfg: r.cfg, transport: transport, client: &http.Client{Transport: transport, Timeout: 150 * time.Second},
		base: "http://127.0.0.1:" + strconv.Itoa(r.cfg.Port), stopped: true}
}

// logFile keeps a copy of the daemon's log beside the test, under the test's own temporary
// directory, which Go removes when the test ends whether it passed or failed: a finished run
// leaves no log behind. The file is still worth naming while the test runs, and the cleanup
// closes it and, when the test failed, prints the end of it into the test's own output, which Go
// shows for a failing test. That is where a failure's diagnosis is read now, the file itself
// being gone by the time anyone looks. It is called on the test's own goroutine, because the
// order its cleanup runs in is the order that call is made in (boot).
func (r *idleWakeRig) logFile() io.Writer {
	r.t.Helper()
	if r.daemonLog == nil {
		// TempDir registers the directory's removal the first time it is called, and cleanups run
		// in the reverse of the order they were registered in, so the cleanup below is registered
		// after this call: it reads the file while the directory holding it is still there.
		file, err := os.CreateTemp(r.t.TempDir(), "lgdaemon-*.log")
		if err != nil {
			r.t.Fatal(err)
		}
		r.t.Cleanup(func() {
			_ = file.Close()
			if !r.t.Failed() {
				return
			}
			tail, omitted, err := idleRigLogTail(file.Name())
			switch {
			case err != nil:
				r.t.Logf("the daemon's log %s could not be read back: %v", file.Name(), err)
			case tail == "":
				r.t.Logf("the daemon's log %s is empty: the daemon wrote nothing to it", file.Name())
			case omitted > 0:
				r.t.Logf("the daemon's log, its last %d bytes, %d earlier bytes left out; the rig's own diagnosis prints the last 40 lines of this same log, which are the end of what follows:\n%s",
					len(tail), omitted, tail)
			default:
				r.t.Logf("the daemon's log, whole, %d bytes:\n%s", len(tail), tail)
			}
		})
		r.t.Logf("the daemon's log: %s, a file the run removes when the test ends; a test that fails prints its last %d bytes instead",
			file.Name(), idleRigLogTailBytes)
		r.daemonLog = file
	}
	return r.daemonLog
}

// idleRigLogTailBytes is how much of the daemon's log a failing test prints. The whole log of a
// long run is a quarter of a megabyte, which nobody reads in CI output, while the last 32 KiB is
// the stretch the failure happened in and is still several times the 40 lines diagnose prints of
// the same log, so the two read as a wide window and the close one inside it.
const idleRigLogTailBytes = 32 << 10

// idleRigLogTail reads the last idleRigLogTailBytes of the named file, returning them and how
// many bytes it left out before them. It starts after the first newline in that window, so the
// text begins with a whole log line rather than half of one, and the count it returns says how
// much of the log the reader is not being shown.
func idleRigLogTail(name string) (tail string, omitted int64, err error) {
	file, err := os.Open(name)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = file.Close() }()
	size, err := file.Seek(0, io.SeekEnd)
	if err != nil {
		return "", 0, err
	}
	at := size - idleRigLogTailBytes
	if at < 0 {
		at = 0
	}
	window := make([]byte, size-at)
	if _, err := file.ReadAt(window, at); err != nil {
		return "", 0, err
	}
	text := string(window)
	if at > 0 {
		if newline := strings.IndexByte(text, '\n'); newline >= 0 {
			at += int64(newline) + 1
			text = text[newline+1:]
		}
	}
	return text, at, nil
}

// stopDaemon stops the running daemon, if one runs, and waits for it.
func (r *idleWakeRig) stopDaemon() {
	r.stopMu.Lock()
	stop, done := r.stop, r.done
	r.stop, r.done = nil, nil
	r.stopMu.Unlock()
	if stop == nil {
		return
	}
	stop()
	select {
	case err := <-done:
		if err != nil {
			r.t.Errorf("run: %v", err)
		}
	case <-time.After(30 * time.Second):
		r.t.Error("the daemon did not stop")
	}
}

// restart stops the daemon and boots it again on the same store, as a deployment's restart does.
// The wakes the listener reported while it was down are on its durable consumer, which is what
// makes them wait for it rather than be lost.
func (r *idleWakeRig) restart() {
	r.t.Helper()
	r.stopDaemon()
	rebindHeldPorts(r.t, &r.cfg)
	r.boot()
}

// newRuntime is the runtime factory the daemon boots with: the real Sandbox runtime
// (sandbox.New) over the rig's Kubernetes API, with the worker stream, store and connection
// directory the boot hands it, and the rig's own tools — a `legion` that is the role process
// stand-in, and an agent command it never has to run.
func (r *idleWakeRig) newRuntime(ctx context.Context, listener *stream.Listener, address string, tokens appauth.Tokens,
	_ func(context.Context, string) (ghrepo.Repository, string, error), st *store.Store, _ func() []ghrepo.Repository,
) (legionruntime.Runtime, error) {
	r.built.mu.Lock()
	r.built.address = address
	r.built.mu.Unlock()
	k := *r.cfg.Runtime.Kubernetes
	opts, err := sandboxOptions(r.cfg, k, r.project, "placeholder-dispatch-token", lookup(nil), slog.New(slog.NewJSONHandler(r.logs, nil)))
	if err != nil {
		return nil, fmt.Errorf("sandboxOptions: %w", err)
	}
	tools := r.toolsDir()
	opts.Conns, opts.StreamURL, opts.Store = listener, address, st
	opts.Tokens = idleRigTokens{}
	opts.GitHubCredential = func(_ context.Context, role claim.Role, repo ghrepo.Repository) (ghconfig.Bundle, error) {
		rendered := ghconfig.Render("ghs_idle_rig", string(appauth.AppRoleFor(role)), time.Now().Add(time.Hour))
		owner := strings.ToLower(repo.Owner())
		return ghconfig.Bundle{Default: owner, Dirs: map[string]ghconfig.Rendered{owner: rendered}}, nil
	}
	// The controller's own bundle, which its launch and its refresh both ask for by owner
	// (sandbox.Options.ControllerCredential, appauth.ControllerCredential): the review App's, over
	// liveRepo's owner, the rig's one.
	opts.ControllerCredential = func(_ context.Context, defaultOwner string) (ghconfig.Bundle, error) {
		rendered := ghconfig.Render("ghs_idle_rig", string(appauth.Review), time.Now().Add(time.Hour))
		owner := strings.Split(liveRepo, "/")[0]
		if defaultOwner != "" {
			owner = defaultOwner
		}
		return ghconfig.Bundle{Default: owner, Dirs: map[string]ghconfig.Rendered{owner: rendered}}, nil
	}
	// The directories the pods' gh is built from, which sandbox.New now requires
	// (checkSupervision) and the runtime reads a running pod's gh projection against: the rig's
	// issues are all on liveRepo, so its owner's directory is the whole set.
	opts.GitHubDirs = func(ghrepo.Repository, []appauth.AppRole) []string {
		return []string{strings.Split(liveRepo, "/")[0]}
	}
	opts.Mapped = func() []ghrepo.Repository { return nil }
	opts.Tools.Legion = idleRigLegionTool(r.t, tools)
	opts.Agent = idleRigAgentTool(r.t, tools)
	opts.DaemonURL = r.cfg.DaemonURL
	opts.EnvoyURL = r.cfg.EnvoyURL
	rt, err := sandbox.New(ctx, sandboxRESTConfig(r.kube.url), opts)
	if err != nil {
		return nil, fmt.Errorf("sandbox.New: %w", err)
	}
	listener.SetLauncherResolver(rt.LauncherResolver())
	_ = tokens
	return rt, nil
}

// toolsDir holds the rig's `legion` and `omp` stand-ins, made once for the test. The longest
// path written under it is the `legion` stand-in (idleRigLegionTool).
func (r *idleWakeRig) toolsDir() string {
	r.t.Helper()
	r.stopMu.Lock()
	defer r.stopMu.Unlock()
	if r.toolsPath == "" {
		r.toolsPath = testtmp.Dir(r.t, "legion")
	}
	return r.toolsPath
}

// idleRigAppTokens is the package's workflow token source with one thing filled in: each lease's
// git identity carries an email as well as a login. The daemon asks a role's shim to adopt the
// working copy under that identity before it prompts it, and shimwire refuses an
// `adopt-working-copy` whose identity has no email ("adopt-working-copy carries no jj identity"),
// which loses every prompt. A real App's lease carries `<id>+<login>@users.noreply.github.com`
// (appauth.GitIdentity).
type idleRigAppTokens struct{ *workflowTokenRecorder }

func (t idleRigAppTokens) Token(ctx context.Context, role appauth.AppRole, repo ghrepo.Repository) (appauth.Lease, error) {
	lease, err := t.workflowTokenRecorder.Token(ctx, role, repo)
	if err != nil {
		return lease, err
	}
	lease.Identity.Email = "1+" + lease.Identity.Name + "@users.noreply.github.com"
	return lease, nil
}

// idleRigTokens mints the provisioning token a pod's workspace fetch clones with; a public
// repository needs none, and no pod of this rig clones anything.
type idleRigTokens struct{}

func (idleRigTokens) Token(context.Context, ghrepo.Repository) (string, error) {
	return "idle-rig-provision", nil
}

// launch waits until the issue's pod runs its role, registered, ready and having told the daemon
// it runs no background work: the state the idle rule releases. The daemon starts the implementer
// of an admitted issue whose phase is its own, so nothing here asks for the launch; a claim still
// suspended is nudged with the operator's resume, which is what a boot that left it suspended
// needs.
func (r *idleWakeRig) launch(issue idleRigIssue) {
	r.t.Helper()
	defer func() {
		if r.t.Failed() {
			r.t.Log(r.diagnose())
		}
	}()
	token := r.token(issue)
	var nudged time.Time
	idleRigEventually(r.t, 150*time.Second, "the pod of "+issue.key+" running its role, registered and ready", func() bool {
		state := r.api.claim(token)
		if idleRigWorking(state.State) && r.agents.role(issue.key, string(issue.role)).Registered > 0 {
			return true
		}
		if state.State == string(supervise.StateSuspended) && time.Since(nudged) > 3*time.Second {
			nudged = time.Now()
			r.api.request(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/resume", nil, true)
		}
		return false
	})
}

// diagnose is what a failure of the rig prints: the launchers' own log and the last of the
// daemon's, which is where a pod that never came up says why.
func (r *idleWakeRig) diagnose() string {
	daemonLog := strings.Split(strings.TrimSpace(r.logs.String()), "\n")
	if len(daemonLog) > 40 {
		daemonLog = daemonLog[len(daemonLog)-40:]
	}
	return "launchers:\n" + r.pods.log() + "\ndaemon:\n" + strings.Join(daemonLog, "\n")
}

// idleRigWorking is whether a claim's state is one a live role process is behind: waiting for
// work, in a turn, or between turns. A woken role passes through all three, and its pod becomes
// idle again a minute later, so a wait on one of them alone would miss it.
func idleRigWorking(state string) bool {
	switch supervise.ClaimState(state) {
	case supervise.StateReady, supervise.StateWorking, supervise.StateIdle:
		return true
	}
	return false
}

// token is the claim token of the issue's role.
func (r *idleWakeRig) token(issue idleRigIssue) claim.Token {
	r.t.Helper()
	token, err := claim.NewToken(r.project, issue.key, issue.role)
	if err != nil {
		r.t.Fatal(err)
	}
	return token
}

// markPrompts records how many tasks each role has been given, which newPrompts counts from: the
// daemon's boot starts the implementer of an admitted issue whose phase is its own and gives it
// that phase's task, so a role of the rig has already been prompted once before its pod is ever
// released.
func (r *idleWakeRig) markPrompts() {
	r.baseline = map[string]int{}
	for _, issue := range r.issues {
		r.baseline[issue.key] = len(r.agents.prompts(issue.key, string(issue.role)))
	}
}

// newPrompts is every task the issue's role has been given since markPrompts.
func (r *idleWakeRig) newPrompts(issue idleRigIssue) []idleRigMessage {
	prompts := r.agents.prompts(issue.key, string(issue.role))
	from := r.baseline[issue.key]
	if from > len(prompts) {
		return nil
	}
	return prompts[from:]
}

// quiet back-dates when every claim of the project was last written, so the next pass of the
// daemon's idle tick reads a pod that has done nothing for longer than the idle period. The
// session's own report is left as the session wrote it, because the rule reads it against that
// session's last turn and not against the clock. The rule reads the claim rows and nothing else
// (pods.IdleView), so the rows are the clock it is measured against.
func (r *idleWakeRig) quiet() {
	r.t.Helper()
	if _, err := r.pool.Exec(context.Background(),
		`update claims set updated_at = now() - interval '30 minutes'
		 where project = $1 and activity_at is not null`, r.project); err != nil {
		r.t.Fatalf("back-date the project's claims: %v", err)
	}
}

// awaitReleased waits for every issue's claim to be suspended with the source `released` and its
// pod, and the pod's Sandbox, to be gone. One pass of the daemon's own idle tick decides them
// all, so they are waited for together.
func (r *idleWakeRig) awaitReleased(issues ...idleRigIssue) {
	r.t.Helper()
	defer func() {
		if r.t.Failed() {
			r.t.Log(r.diagnose())
		}
	}()
	var quieted time.Time
	idleRigEventually(r.t, 240*time.Second, "every pod of the rig released", func() bool {
		// The pods are back-dated rather than waited out: the idle rule reads the claim rows and
		// nothing else (pods.IdleView), so those rows are the clock it is measured against.
		if time.Since(quieted) > 2*time.Second {
			quieted = time.Now()
			r.quiet()
		}
		for _, issue := range issues {
			state := r.api.claim(r.token(issue))
			if state.State != string(supervise.StateSuspended) || state.SuspendedBy != string(supervise.SuspendedByReleased) ||
				r.kube.pod(issue.pod) || r.kube.sandbox(issue.pod) != nil {
				return false
			}
		}
		return true
	})
}

// awaitResumed waits for the issue's role to be back: its claim live again, a second pod started
// for it, and its role process registered again.
func (r *idleWakeRig) awaitResumed(issue idleRigIssue, registrations int) {
	r.t.Helper()
	token := r.token(issue)
	idleRigEventually(r.t, 150*time.Second, "the pod of "+issue.key+" back with its role registered again", func() bool {
		return r.agents.role(issue.key, string(issue.role)).Registered >= registrations &&
			idleRigWorking(r.api.claim(token).State)
	})
}

// publish puts one Envoy envelope from source on the stream the daemon consumes.
func (r *idleWakeRig) publish(subject, source, eventID, payload string) {
	r.t.Helper()
	data, err := json.Marshal(map[string]any{
		"event_id": eventID, "source": source, "source_event_id": eventID + "-source", "topic": subject,
		"dedupe_key": eventID, "issued_at": time.Now().UnixMilli(), "payload_summary": "idle rig event",
		"payload": payload, "trace_id": eventID + "-trace",
	})
	if err != nil {
		r.t.Fatalf("encode the %s envelope: %v", subject, err)
	}
	if _, err := r.js.Publish(context.Background(), subject, data); err != nil {
		r.t.Fatalf("publish %s: %v", subject, err)
	}
}

// publishSettlement publishes the CI settlement of the issue's pull request head, as Envoy relays
// GitHub's check report (ci_red_no_takeover_live_test.go's publishSettlement builds the same
// payload).
func (r *idleWakeRig) publishSettlement(issue idleRigIssue) {
	r.t.Helper()
	group := func() map[string]any { return map[string]any{"names": []string{}, "truncated": false} }
	payload, err := json.Marshal(map[string]any{
		"kind": "checks", "repo": liveRepo, "number": strconv.Itoa(idleRigPullNumber(issue.key)), "sha": idleRigHead(issue.key),
		"check_runs": []map[string]any{{"name": "build", "id": 1}}, "generation": 1,
		"snapshot": issue.key + "-settled", "settled_at": time.Now().UnixMilli(),
		"failed": group(), "passed": group(), "running": group(), "queued": group(), "cancelled": group(), "skipped": group(),
	})
	if err != nil {
		r.t.Fatalf("encode the settlement payload: %v", err)
	}
	subject := fmt.Sprintf("notifications.github.%s.pr.%d.checks",
		strings.ReplaceAll(liveRepo, "/", "."), idleRigPullNumber(issue.key))
	r.publish(subject, "github", "checks-"+issue.key, string(payload))
}

// releaseRecords is the deployment's record of every release, resume and hold, read through the
// route a claim of the pod reads them on (POST /legion/v1/claims/release-records).
func (r *idleWakeRig) releaseRecords(issue idleRigIssue) api.ReleaseRecordsResponse {
	r.t.Helper()
	seen := r.agents.role(issue.key, string(issue.role))
	status, body := r.api.request(http.MethodPost, "/legion/v1/claims/release-records",
		api.ReleaseRecordsRequest{ClaimToken: claim.Token(seen.Claim), Secret: seen.Secret,
			Since: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}, false)
	if status != http.StatusOK {
		r.t.Fatalf("read the release records = %d %s", status, body)
	}
	var records api.ReleaseRecordsResponse
	if err := json.Unmarshal(body, &records); err != nil {
		r.t.Fatalf("decode the release records: %v", err)
	}
	return records
}

// logged is how many of the daemon's log lines say msg with each of attributes, as
// check_wake_test.go's rig reads them.
func (r *idleWakeRig) logged(msg string, attributes map[string]string) int {
	count := 0
	for _, line := range strings.Split(r.logs.String(), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		matches := entry["msg"] == msg
		for key, want := range attributes {
			matches = matches && fmt.Sprint(entry[key]) == want
		}
		if matches {
			count++
		}
	}
	return count
}

// idleRigEventually waits for condition within the given time, failing with what it waited for
// and the launchers' own log, which is where a pod that never came back says why. It is
// testwait.Eventually with a deadline of its own: a release waits out the daemon's idle tick,
// which is a minute, so the package helper's one-minute cap is too short here.
func idleRigEventually(t *testing.T, within time.Duration, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	if testDeadline, ok := t.Deadline(); ok && testDeadline.Add(-5*time.Second).Before(deadline) {
		deadline = testDeadline.Add(-5 * time.Second)
	}
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
