package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// The commits a production check wake is driven with: the merge the check looks for, the build the
// daemon under test is built from (behind the merge, so its own boot wakes nothing), a build
// Dispatch starts on that carries the merge, and one that does not.
const (
	checkWakeMerge       = "a1b2c3d4e5f6a7b8c9d0a1b2c3d4e5f6a7b8c9d0"
	checkWakeDaemonBuild = "0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d"
	checkWakeAheadBuild  = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c"
	checkWakeBehindBuild = "0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b"
)

// The deploy the rig's project configures — sjawhar/legion's envoy.yaml, its step Apply — and
// another workflow of the repository, which is no deploy.
const (
	checkWakeDeployWorkflow = ".github/workflows/envoy.yaml"
	checkWakeOtherWorkflow  = ".github/workflows/ci.yaml"
)

// checkWakeMergedAt is when sjawhar/legion#1300 merged, GitHub's merged_at: a deploy run's Apply
// wakes the check only when it finished after it.
var checkWakeMergedAt = time.Date(2026, 10, 19, 5, 0, 0, 0, time.UTC)

// checkWakeGitHub is GitHub's REST API for sjawhar/legion as a daemon's production check wakes
// read it: the repository (every launch reads its default branch), the merged pull request with its
// merge time, the compare of the check's merge with a build, answered `ahead` for the build that
// carries it, `behind` for the daemon's own and the one that does not, and 404 for a commit it does
// not know, and attempt 1 of each workflow run a test serves (serveRun) with that attempt's jobs,
// 404 for a run it was not given. It records every compare's build, and every run read, in order.
type checkWakeGitHub struct {
	url      string
	mu       sync.Mutex
	compared []string
	runs     map[int64]checkWakeRun
	runReads []int64
}

// checkWakeRun is attempt 1 of a push run of a sjawhar/legion workflow on main, as GitHub answers
// it: the workflow file it ran, the commit it ran on, and the attempt's jobs (checkWakeJob).
type checkWakeRun struct {
	workflow string
	headSHA  string
	jobs     []string
}

// name is the run's workflow name, its file's name without the extension.
func (run checkWakeRun) name() string {
	return strings.TrimSuffix(path.Base(run.workflow), path.Ext(run.workflow))
}

func newCheckWakeGitHub(t *testing.T) *checkWakeGitHub {
	t.Helper()
	g := &checkWakeGitHub{runs: map[int64]checkWakeRun{}}
	status := map[string]string{checkWakeDaemonBuild: "behind", checkWakeBehindBuild: "behind", checkWakeAheadBuild: "ahead"}
	notFound := func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/sjawhar/legion":
			_, _ = io.WriteString(w, `{"full_name":"sjawhar/legion","default_branch":"main"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/sjawhar/legion/pulls/1300":
			_, _ = io.WriteString(w, `{"number":1300,"merged":true,"merged_at":"`+checkWakeMergedAt.Format(time.RFC3339)+`","merge_commit_sha":"`+checkWakeMerge+`"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/sjawhar/legion/compare/"+checkWakeMerge+"..."):
			build := strings.TrimPrefix(r.URL.Path, "/repos/sjawhar/legion/compare/"+checkWakeMerge+"...")
			g.mu.Lock()
			g.compared = append(g.compared, build)
			g.mu.Unlock()
			answer, known := status[build]
			if !known {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"message":"Not Found"}`)
				return
			}
			_, _ = io.WriteString(w, `{"status":"`+answer+`","ahead_by":1,"behind_by":0,"files":[]}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/sjawhar/legion/actions/runs/"):
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/sjawhar/legion/actions/runs/"), "/")
			id, err := strconv.ParseInt(parts[0], 10, 64)
			g.mu.Lock()
			run, known := g.runs[id]
			g.runReads = append(g.runReads, id)
			g.mu.Unlock()
			switch {
			case err != nil || !known || len(parts) < 3 || parts[1] != "attempts" || parts[2] != "1":
				notFound(w)
			case len(parts) == 3:
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": id, "run_attempt": 1, "name": run.name(), "path": run.workflow, "head_branch": "main", "head_sha": run.headSHA, "event": "push",
					"status": "completed", "conclusion": "success", "html_url": fmt.Sprintf("https://github.com/sjawhar/legion/actions/runs/%d/attempts/1", id),
					"head_repository": map[string]any{"full_name": "sjawhar/legion"},
				})
			case len(parts) == 4 && parts[3] == "jobs":
				_, _ = fmt.Fprintf(w, `{"total_count":%d,"jobs":[%s]}`, len(run.jobs), strings.Join(run.jobs, ","))
			default:
				notFound(w)
			}
		default:
			notFound(w)
		}
	}))
	t.Cleanup(server.Close)
	g.url = server.URL
	return g
}

// compares is each build GitHub was asked to compare the merge with so far.
func (g *checkWakeGitHub) compares() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.compared...)
}

// serveRun has GitHub answer attempt 1 of run id as run.
func (g *checkWakeGitHub) serveRun(id int64, run checkWakeRun) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.runs[id] = run
}

// runRead is how many times GitHub was asked about run id, its attempt or the attempt's jobs.
func (g *checkWakeGitHub) runRead(id int64) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	reads := 0
	for _, read := range g.runReads {
		if read == id {
			reads++
		}
	}
	return reads
}

// checkWakeJob is job id of an attempt-1 run, named name, as GitHub lists it among the attempt's
// jobs, with its steps (checkWakeStep).
func checkWakeJob(id int64, name string, steps ...string) string {
	return fmt.Sprintf(`{"id":%d,"run_attempt":1,"name":%q,"status":"completed","conclusion":"success","steps":[%s]}`, id, name, strings.Join(steps, ","))
}

// checkWakeStep is a job's step as GitHub lists it: its name, its conclusion, and when it
// completed, after the pull request's merge (a negative duration before it).
func checkWakeStep(name, conclusion string, after time.Duration) string {
	completed := checkWakeMergedAt.Add(after)
	return fmt.Sprintf(`{"name":%q,"status":"completed","conclusion":%q,"number":4,"started_at":%q,"completed_at":%q}`,
		name, conclusion, completed.Add(-time.Minute).Format(time.RFC3339), completed.Format(time.RFC3339))
}

// checkWakeDeployDetail is the wake detail of run's job whose Apply succeeded after after the merge.
func checkWakeDeployDetail(run int64, job string, after time.Duration) string {
	return fmt.Sprintf("sjawhar/legion %s run %d attempt 1, job %s: Apply succeeded at %s", checkWakeDeployWorkflow, run, job, checkWakeMergedAt.Add(after).Format(time.RFC3339))
}

// checkWakeDispatch is a Dispatch that maps sjawhar/legion to project as its default, lists no
// issues and reports no build commit on /healthz, so the boot compares only the daemon's own build;
// every other call is unavailable, as a Dispatch whose dependencies are down answers, and is not
// what these tests are about.
func checkWakeDispatch(t *testing.T, project string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/settings/repo-projects":
			_, _ = io.WriteString(w, `[{"repo":"sjawhar/legion","project":"`+project+`","default":true}]`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/issues":
			_, _ = io.WriteString(w, `[]`)
		case r.Method == http.MethodGet && r.URL.Path == "/healthz":
			_, _ = io.WriteString(w, `{"status":"ok","commit":null}`)
		default:
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// checkWakeRig is a release build of the daemon, built from sjawhar/legion at checkWakeDaemonBuild,
// over a closed root of its project whose tree lingers for another hour, the root's implementer
// suspended by the daemon as the close left it, and the root's production check on
// sjawhar/legion#1300 at checkWakeMerge, its first look (the merge's) ended as pending. It is the
// state a wake reaches: a check with no look open, its implementer's session kept. The project
// configures one deploy, sjawhar/legion's envoy.yaml and its step Apply. The daemon's log is kept.
type checkWakeRig struct {
	cfg         config.Config
	root        string
	implementer claim.Token
	js          jetstream.JetStream
	github      *checkWakeGitHub
	rt          *fake.Runtime
	records     record.Store
	pool        *pgxpool.Pool
	logs        *syncBuffer
	// built is what the daemon handed the fake runtime: the worker stream a pane's shim dials.
	built *built
	// api calls the booted daemon's HTTP API.
	api *daemon
}

func newCheckWakeRig(t *testing.T) *checkWakeRig {
	t.Helper()
	pool := isolatedOutboxPool(t)
	natsURL := workflowNATS(t)
	cfg := workflowConfig(t, natsURL)
	cfg.PostgresDSN = pool.Config().ConnString()
	cfg.DispatchURL = checkWakeDispatch(t, cfg.Project)
	cfg.Projects = map[string]config.Project{cfg.Project: {Deploys: []config.Deploy{
		{Repo: ghrepo.MustParse("sjawhar/legion"), Workflow: checkWakeDeployWorkflow, Step: "Apply"},
	}}}
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	root := cfg.Project + "-1"
	implementer, err := claim.NewToken(project, root, claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Hour)
	opened := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	rig := &checkWakeRig{cfg: cfg, root: root, implementer: implementer, js: workflowJetStream(t, natsURL), github: newCheckWakeGitHub(t),
		rt: fake.NewRuntime(), built: &built{}, records: record.NewStore(), pool: pool, logs: &syncBuffer{}}

	ctx := context.Background()
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	if err := pgx.BeginFunc(ctx, st.Pool(), func(tx pgx.Tx) error {
		if err := rig.records.PutIssue(ctx, tx, record.Issue{Key: root, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Project: cfg.Project, Tree: root, Title: "Closed root", Phase: phase.Done,
			Generation: 1, Status: "done", HandedOver: true, Rank: "U", LingerUntil: &until}); err != nil {
			return err
		}
		return rig.records.PutProductionCheck(ctx, tx, record.ProductionCheck{
			Issue: root, Project: cfg.Project, Tree: root, Repo: "sjawhar/legion", Number: 1300, MergeSHA: checkWakeMerge,
			OpenedAt: opened, BoundAt: opened.Add(336 * time.Hour), Looks: 1, LookOpen: false, LookWakeAt: &opened,
			LastWakeKind: "merge", LastWakeDetail: checkWakeMerge, LastWakeAt: &opened,
		})
	}); err != nil {
		t.Fatalf("record the closed root and its check: %v", err)
	}
	if _, err := st.OpenTreeLifecycle(ctx, project, root, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open the lifecycle of %s: %v", root, err)
	}
	if _, err := st.AdmitClaim(ctx, supervise.Claim{Token: implementer, Project: project, Tree: root, Issue: root, Role: claim.RoleImplementer, Generation: 1,
		State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByDaemon, Session: "ses_implementer", SessionFile: filepath.Join(cfg.StateDir, "implementer.jsonl")}); err != nil {
		t.Fatalf("record the suspended implementer: %v", err)
	}
	writePrompt(t, cfg, implementer)
	return rig
}

// boot runs the daemon as the release build over the rig until the test ends, returning once it
// answers /healthz and its boot has compared its own build against the check: the compare that
// wakes nothing, since that build is behind the merge.
func (rig *checkWakeRig) boot(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, rig.cfg, Build{Revision: checkWakeDaemonBuild, Repository: "sjawhar/legion"}, slog.New(slog.NewJSONHandler(rig.logs, nil)), overrides{
			listen: heldListen, runtime: fakeRuntime(rig.rt, rig.built).runtime, clock: stillClock{},
			workflowTokens: &workflowTokenRecorder{}, githubAPI: rig.github.url,
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("run: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	awaitHealthz(t, rig.cfg, done)
	testwait.Eventually(t, "the boot comparing the daemon's own build against the check", func() bool {
		return len(rig.github.compares()) >= 1
	})
	// The API's client closes its connections before the daemon is stopped (cleanups run last
	// first), as startDaemon's does, so the stop waits on none of them.
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	rig.api = &daemon{t: t, cfg: rig.cfg, transport: transport, client: &http.Client{Transport: transport, Timeout: 10 * time.Second},
		base: "http://127.0.0.1:" + strconv.Itoa(rig.cfg.Port), stopped: true}
	if compared := rig.github.compares(); compared[0] != checkWakeDaemonBuild {
		t.Fatalf("the boot compared %v, want the daemon's own build %s first", compared, checkWakeDaemonBuild)
	}
	if check := rig.check(t); check.Looks != 1 || check.LookOpen {
		t.Fatalf("after the boot's compare the check is at look %d (open %v), want look 1 still ended: the daemon's build is behind the merge", check.Looks, check.LookOpen)
	}
}

// check is the root's production check as recorded now.
func (rig *checkWakeRig) check(t *testing.T) record.ProductionCheck {
	t.Helper()
	return readProductionCheck(t, rig.pool, rig.records, rig.root)
}

// publish publishes an Envoy envelope from Dispatch on subject: eventID its id and payload its
// payload, as Envoy relays Dispatch's events.
func (rig *checkWakeRig) publish(t *testing.T, subject, eventID, payload string) {
	t.Helper()
	rig.publishFrom(t, "dispatch", subject, eventID, "dispatch event", payload)
}

// publishFrom publishes an Envoy envelope from source on subject: eventID its id, summary its
// payload summary and payload its payload.
func (rig *checkWakeRig) publishFrom(t *testing.T, source, subject, eventID, summary, payload string) {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"event_id": eventID, "source": source, "source_event_id": eventID + "-source", "topic": subject,
		"dedupe_key": eventID, "issued_at": 1, "payload_summary": summary, "payload": payload, "trace_id": eventID + "-trace",
	})
	if err != nil {
		t.Fatalf("encode the %s envelope: %v", subject, err)
	}
	if _, err := rig.js.Publish(context.Background(), subject, data); err != nil {
		t.Fatalf("publish %s: %v", subject, err)
	}
}

// awaitLook waits for the check's look 2 to open, woken by kind with detail, and for its start row
// to have resumed the implementer the close left suspended: the one resume the fake runtime sees.
func (rig *checkWakeRig) awaitLook(t *testing.T, kind, detail string) {
	t.Helper()
	rig.awaitWake(t, 2, kind, detail)
	testwait.Eventually(t, "the look's start row resuming the implementer", func() bool {
		return len(resumesOf(rig.rt, rig.implementer)) == 1
	})
	if resume := resumesOf(rig.rt, rig.implementer)[0]; resume.Spec.ResumeSessionFile != filepath.Join(rig.cfg.StateDir, "implementer.jsonl") {
		t.Fatalf("resumed %+v, want the implementer's own kept session", resume.Spec)
	}
}

// awaitWake waits for the check's look to be open, woken by kind with detail.
func (rig *checkWakeRig) awaitWake(t *testing.T, look int, kind, detail string) {
	t.Helper()
	testwait.Eventually(t, fmt.Sprintf("look %d of the check woken by %s", look, kind), func() bool {
		check := rig.check(t)
		return check.Looks == look && check.LookOpen && check.LastWakeKind == kind && check.LastWakeDetail == detail
	})
}

// Dispatch's start, relayed on notifications.dispatch.server.started, names the build Dispatch
// started on; the daemon, a release build from sjawhar/legion, compares it on GitHub against each
// open production check of that repository, and a build that carries the check's merge (`ahead`)
// wakes the check: its next look starts, recorded as woken by the Dispatch start on that build,
// and the look's start row resumes the implementer the close left suspended. The daemon's own
// build, compared at its boot and behind the merge, wakes nothing first.
func TestADispatchStartOnABuildCarryingTheMergeWakesTheCheck(t *testing.T) {
	rig := newCheckWakeRig(t)
	rig.boot(t)

	rig.publish(t, "notifications.dispatch.server.started", "dispatch-server-started-ahead", `{"commit":"`+checkWakeAheadBuild+`","started_at":"2026-10-19T06:00:00Z"}`)

	rig.awaitLook(t, "dispatch start", checkWakeAheadBuild)
	if compared := rig.github.compares(); len(compared) != 2 || compared[1] != checkWakeAheadBuild {
		t.Fatalf("GitHub compared %v, want the daemon's build then Dispatch's", compared)
	}
}

// A Dispatch start on a build that does not carry the merge (`behind`) is compared and wakes
// nothing: the check stays at its ended look, and no start row reaches the implementer.
func TestADispatchStartOnABuildBehindTheMergeWakesNothing(t *testing.T) {
	rig := newCheckWakeRig(t)
	rig.boot(t)

	rig.publish(t, "notifications.dispatch.server.started", "dispatch-server-started-behind", `{"commit":"`+checkWakeBehindBuild+`","started_at":"2026-10-19T06:00:00Z"}`)

	testwait.Eventually(t, "GitHub asked to compare Dispatch's build", func() bool {
		return len(rig.github.compares()) == 2
	})
	if compared := rig.github.compares(); compared[1] != checkWakeBehindBuild {
		t.Fatalf("GitHub compared %v, want Dispatch's build second", compared)
	}
	if check := rig.check(t); check.Looks != 1 || check.LookOpen || check.LastWakeKind != "merge" {
		t.Fatalf("check = look %d (open %v) woken by %q, want look 1 still ended by the merge's look", check.Looks, check.LookOpen, check.LastWakeKind)
	}
	if resumes := resumesOf(rig.rt, rig.implementer); len(resumes) != 0 {
		t.Fatalf("the implementer was resumed %d times, want never: the build does not carry the merge", len(resumes))
	}
}

// Dispatch's issue.deployed for the check's pull request, relayed on the issue's own subject, wakes
// the check with no GitHub read: Dispatch's delivery timeline already knows the run shipped it. The
// next look starts, recorded as woken by the deploy with the run and its completion, and its start
// row resumes the implementer.
func TestAnIssueDeployedEventWakesTheCheck(t *testing.T) {
	rig := newCheckWakeRig(t)
	rig.boot(t)

	event := `{"id":31,"issue_key":"` + rig.root + `","seq":7,"type":"issue.deployed","actor":{"kind":"system","id":"delivery-timeline"},"notify":true,` +
		`"payload":{"repo":"sjawhar/legion","number":1300,"run_id":38230245571,"completed_at":"2026-10-19T06:12:00Z"}}`
	rig.publish(t, "notifications.dispatch.issue."+rig.root+".issue.deployed", "dispatch-31-"+rig.cfg.Project, event)

	rig.awaitLook(t, "issue.deployed", "run 38230245571 completed 2026-10-19T06:12:00Z")
	if compared := rig.github.compares(); len(compared) != 1 {
		t.Fatalf("GitHub compared %v, want only the boot's compare of the daemon's own build", compared)
	}
}

// checkWakeDeploy is a run of the deploy workflow on commit sha whose job `deploy` (id 2) applied
// after after the merge, beside a job `preview` (id 1) that applies nothing.
func checkWakeDeploy(sha string, after time.Duration) checkWakeRun {
	return checkWakeRun{workflow: checkWakeDeployWorkflow, headSHA: sha, jobs: []string{
		checkWakeJob(1, "preview", checkWakeStep("Preview", "success", after-5*time.Minute)),
		checkWakeJob(2, "deploy", checkWakeStep("Plan", "success", after-time.Minute), checkWakeStep("Apply", "success", after)),
	}}
}

// publishGitHub has GitHub answer attempt 1 of run id as run and publishes that attempt's
// completion as Envoy relays GitHub's workflow_run webhook (the golden
// packages/contracts/fixtures/github-envelopes/workflow-run-completed.json): from source github
// under eventID, on the subject of the run's workflow file, its payload naming the run, its
// attempt, its workflow file, its branch and its commit. It goes through the daemon's durable
// GitHub intake consumer, as a real webhook's envelope does.
func (rig *checkWakeRig) publishGitHub(t *testing.T, eventID string, id int64, run checkWakeRun) {
	t.Helper()
	rig.github.serveRun(id, run)
	payload, err := json.Marshal(map[string]any{
		"action": "completed", "branch": "main", "conclusion": "success", "head_branch": "main", "head_sha": run.headSHA, "kind": "workflow",
		"path": run.workflow, "repo": "sjawhar/legion", "run_attempt": "1", "run_id": strconv.FormatInt(id, 10), "run_started_at": "2026-10-19T05:40:04Z",
		"status": "completed", "updated_at": "2026-10-19T06:06:28Z", "url": fmt.Sprintf("https://github.com/sjawhar/legion/actions/runs/%d", id), "workflow": run.name(),
	})
	if err != nil {
		t.Fatalf("encode the payload of run %d: %v", id, err)
	}
	subject := "notifications.github.sjawhar.legion.workflow." + strings.ReplaceAll(path.Base(run.workflow), ".", "_") + ".completed"
	rig.publishFrom(t, "github", subject, eventID, fmt.Sprintf("workflow %s main run %d completed/success", run.name(), id), string(payload))
}

// awaitCompared waits for the daemon to have taken in each GitHub envelope published under
// eventIDs — its intake fact processed, which queues a deploy's compare in the same transaction
// when it queues one — and then for no check_compare row to be left in the outbox: every compare
// those runs queued has read GitHub and finished, so what they woke, or did not, is recorded.
func (rig *checkWakeRig) awaitCompared(t *testing.T, eventIDs ...string) {
	t.Helper()
	for _, eventID := range eventIDs {
		testwait.Eventually(t, "the daemon taking in GitHub event "+eventID, func() bool {
			var processed bool
			if err := rig.pool.QueryRow(context.Background(), `select exists (select 1 from processed_events where source = 'github' and event_id = $1)`, eventID).
				Scan(&processed); err != nil {
				t.Fatalf("read whether GitHub event %s was processed: %v", eventID, err)
			}
			return processed
		})
	}
	testwait.Eventually(t, "every check compare finished", func() bool {
		var rows int
		if err := rig.pool.QueryRow(context.Background(), `select count(*) from outbox where kind = $1`, string(record.OutboxKindCheckCompare)).Scan(&rows); err != nil {
			t.Fatalf("count the check compares: %v", err)
		}
		return rows == 0
	})
}

// awaitTask waits for the task pending on the implementer's claim — the one the latest look's
// start row handed it — to say want.
func (rig *checkWakeRig) awaitTask(t *testing.T, want string) {
	t.Helper()
	var task string
	testwait.Eventually(t, "the implementer's pending task saying "+want, func() bool {
		if err := rig.pool.QueryRow(context.Background(), `select coalesce((select task from pending_task_deliveries where claim_token = $1), '')`,
			string(rig.implementer)).Scan(&task); err != nil {
			t.Fatalf("read the implementer's pending task: %v", err)
		}
		return strings.Contains(task, want)
	})
}

// reportPending is the implementer's turn on the open look's task, as its agent takes it: the
// launch the look's start row made — a resume of the kept session — connects its shim, registers
// on that session and reports ready, is prompted the look's task and starts its turn, reports the
// look pending through the API with a grant it mints, and ends the turn, which retires the task.
func (rig *checkWakeRig) reportPending(t *testing.T) {
	t.Helper()
	launch := lastLaunch(t, rig.rt, rig.implementer)
	rig.built.mu.Lock()
	address := rig.built.address
	rig.built.mu.Unlock()
	sh := dialShim(t, address, launch.BootToken)
	testwait.Eventually(t, "the implementer's shim connected", func() bool {
		return rig.api.claim(rig.implementer).State == string(supervise.StateShimConnected)
	})
	status, body := rig.api.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: launch.BootToken, SessionID: "ses_implementer", OmpSessionFile: launch.ResumeSessionFile, AgentID: "agent", PluginContract: api.DaemonAPIVersion,
	}, false)
	if status != http.StatusOK {
		t.Fatalf("register the implementer = %d %s, want 200", status, body)
	}
	var registered claim.RegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil {
		t.Fatalf("decode the implementer's registration: %v", err)
	}
	if status, body := rig.api.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: rig.implementer, SessionID: "ses_implementer", Secret: registered.Secret, Generation: launch.Generation,
	}, false); status != http.StatusNoContent {
		t.Fatalf("ready the implementer = %d %s, want 204", status, body)
	}
	look := rig.check(t).Looks
	prompt := sh.prompt()
	if want := fmt.Sprintf("Look %d, woken by", look); !strings.Contains(prompt.Message, want) {
		t.Fatalf("the implementer was prompted %q, want look %d's task", prompt.Message, look)
	}
	sh.send(shimwire.Response{ID: prompt.ID, Command: shimwire.TypePrompt, Success: true})
	sh.send(shimwire.AgentStart{})
	testwait.Eventually(t, "the implementer's turn starting", func() bool {
		return rig.api.claim(rig.implementer).State == string(supervise.StateWorking)
	})
	status, body = rig.api.request(http.MethodPost, "/legion/v1/grants",
		api.GrantRequest{SessionID: "ses_implementer", Secret: registered.Secret, Tree: rig.root, Issue: rig.root}, false)
	if status != http.StatusOK {
		t.Fatalf("the implementer's grant = %d %s, want 200", status, body)
	}
	var granted api.GrantResponse
	if err := json.Unmarshal(body, &granted); err != nil {
		t.Fatalf("decode the implementer's grant: %v", err)
	}
	status, body = rig.api.request(http.MethodPost, "/legion/v1/handoff/complete",
		api.HandoffCompleteRequest{GrantID: granted.GrantID, Summary: "production carries no deploy of the merge yet", Verdict: "pending"}, false)
	if status != http.StatusOK {
		t.Fatalf("the implementer's pending report = %d %s, want 200", status, body)
	}
	sh.send(shimwire.AgentEnd{})
}

// logged is how many of the daemon's log lines say msg with each of attributes, a value compared
// as the log wrote it.
func (rig *checkWakeRig) logged(t *testing.T, msg string, attributes map[string]string) int {
	t.Helper()
	count := 0
	for _, line := range strings.Split(rig.logs.String(), "\n") {
		if line == "" {
			continue
		}
		decoder := json.NewDecoder(strings.NewReader(line))
		decoder.UseNumber()
		var entry map[string]any
		if err := decoder.Decode(&entry); err != nil {
			t.Fatalf("decode the daemon's log line %q: %v", line, err)
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

// awaitLogged waits for the daemon to log msg with each of attributes times times.
func (rig *checkWakeRig) awaitLogged(t *testing.T, times int, msg string, attributes map[string]string) {
	t.Helper()
	testwait.Eventually(t, fmt.Sprintf("the daemon logging %q with %v %d times", msg, attributes, times), func() bool {
		return rig.logged(t, msg, attributes) == times
	})
}

// A completed run of the project's deploy workflow on the default branch, relayed by Envoy on the
// workflow's own subject, reaches the daemon through its GitHub intake: the daemon reads the run
// attempt and its jobs from GitHub, and the job whose Apply succeeded after the pull request merged
// wakes the check. Look 2 starts, recorded as woken by the deploy run with the run, the job and the
// Apply's completion; its start row resumes the implementer on the check task naming that run; and
// the daemon logs the wake and the deploy it found, naming the run.
func TestADeployRunApplyingAfterTheMergeWakesTheCheck(t *testing.T) {
	rig := newCheckWakeRig(t)
	rig.boot(t)

	rig.publishGitHub(t, "github-run-40203304600", 40203304600, checkWakeDeploy(checkWakeAheadBuild, 8*time.Minute))

	detail := checkWakeDeployDetail(40203304600, "deploy", 8*time.Minute)
	rig.awaitLook(t, "deploy run", detail)
	rig.awaitTask(t, "Look 2, woken by the deploy workflow's run: "+detail+".")
	rig.awaitLogged(t, 1, "workflow: production check woken", map[string]string{"issue": rig.root, "kind": "deploy run", "detail": detail})
	rig.awaitLogged(t, 1, "outbox check compare found a deploy after the merge; the check is woken",
		map[string]string{"issue": rig.root, "run": "40203304600", "attempt": "1", "job": "2", "url": "https://github.com/sjawhar/legion/actions/runs/40203304600/attempts/1"})
	if compared := rig.github.compares(); len(compared) != 1 {
		t.Fatalf("GitHub compared %v, want only the boot's compare of the daemon's own build", compared)
	}
}

// Two deploy runs that each applied after the merge give two looks: the first run's wake starts
// look 2, the implementer reports it pending, which suspends the implementer again, and the second
// run's wake starts look 3, resuming the implementer on the task naming the second run.
func TestTwoDeployRunsApplyingAfterTheMergeWakeTwoLooks(t *testing.T) {
	rig := newCheckWakeRig(t)
	rig.boot(t)

	rig.publishGitHub(t, "github-run-501", 501, checkWakeDeploy(checkWakeAheadBuild, 10*time.Minute))
	rig.awaitLook(t, "deploy run", checkWakeDeployDetail(501, "deploy", 10*time.Minute))
	rig.reportPending(t)
	testwait.Eventually(t, "look 2 ended pending and the implementer suspended", func() bool {
		check := rig.check(t)
		return check.Looks == 2 && !check.LookOpen && rig.api.claim(rig.implementer).State == string(supervise.StateSuspended)
	})

	rig.publishGitHub(t, "github-run-502", 502, checkWakeDeploy(checkWakeAheadBuild, 40*time.Minute))

	second := checkWakeDeployDetail(502, "deploy", 40*time.Minute)
	rig.awaitWake(t, 3, "deploy run", second)
	testwait.Eventually(t, "look 3's start row resuming the implementer again", func() bool {
		return len(resumesOf(rig.rt, rig.implementer)) == 2
	})
	rig.awaitTask(t, "Look 3, woken by the deploy workflow's run: "+second+".")
	rig.awaitLogged(t, 2, "workflow: production check woken", map[string]string{"issue": rig.root, "kind": "deploy run"})
}

// The same run's completion delivered twice, under two envelope ids, is compared twice and wakes
// the check once: both compares read the run from GitHub, and the second one's wake is the same
// job's, a duplicate that starts no look and is not kept for the next one.
func TestTheSameDeployRunDeliveredTwiceWakesOneLook(t *testing.T) {
	rig := newCheckWakeRig(t)
	rig.boot(t)

	run := checkWakeDeploy(checkWakeAheadBuild, 12*time.Minute)
	rig.publishGitHub(t, "github-delivery-1", 601, run)
	rig.publishGitHub(t, "github-delivery-2", 601, run)
	rig.awaitCompared(t, "github-delivery-1", "github-delivery-2")

	rig.awaitLook(t, "deploy run", checkWakeDeployDetail(601, "deploy", 12*time.Minute))
	if reads := rig.github.runRead(601); reads != 4 {
		t.Fatalf("GitHub was asked about run 601 %d times, want 4: the attempt and its jobs, once per delivery", reads)
	}
	if check := rig.check(t); check.Looks != 2 || check.PendingCauseKind != "" {
		t.Fatalf("check = look %d, pending cause %q, want look 2 and none kept: the second delivery is the same deploy", check.Looks, check.PendingCauseKind)
	}
	if resumes := resumesOf(rig.rt, rig.implementer); len(resumes) != 1 {
		t.Fatalf("the implementer was resumed %d times, want once", len(resumes))
	}
	rig.awaitLogged(t, 1, "workflow: production check woken", map[string]string{"issue": rig.root, "kind": "deploy run"})
}

// A deploy run completing while look 2 is open is kept as the check's pending cause, and look 2
// proceeds; when the implementer reports look 2 pending, look 3 starts at once, woken by the kept
// run, on a task naming it.
func TestADeployRunDuringAnOpenLookIsKeptForTheNextLook(t *testing.T) {
	rig := newCheckWakeRig(t)
	rig.boot(t)

	first := checkWakeDeployDetail(701, "deploy", 10*time.Minute)
	rig.publishGitHub(t, "github-run-701", 701, checkWakeDeploy(checkWakeAheadBuild, 10*time.Minute))
	rig.awaitLook(t, "deploy run", first)

	kept := checkWakeDeployDetail(702, "deploy", 30*time.Minute)
	rig.publishGitHub(t, "github-run-702", 702, checkWakeDeploy(checkWakeAheadBuild, 30*time.Minute))
	rig.awaitCompared(t, "github-run-702")
	if check := rig.check(t); check.Looks != 2 || !check.LookOpen || check.LastWakeDetail != first || check.PendingCauseKind != "deploy run" || check.PendingCauseDetail != kept {
		t.Fatalf("check = look %d (open %v) woken by %q, pending %q %q; want look 2 still open on the first run, the second kept",
			check.Looks, check.LookOpen, check.LastWakeDetail, check.PendingCauseKind, check.PendingCauseDetail)
	}
	rig.awaitLogged(t, 1, "workflow: production check wake kept for the next look", map[string]string{"issue": rig.root, "kind": "deploy run", "detail": kept})

	rig.reportPending(t)

	rig.awaitWake(t, 3, "deploy run", kept)
	rig.awaitTask(t, "Look 3, woken by the deploy workflow's run: "+kept+".")
	if check := rig.check(t); check.PendingCauseKind != "" {
		t.Fatalf("look 3 started with %q still kept, want the kept cause spent", check.PendingCauseKind)
	}
}

// A run whose commit predates the merge — one that took its branch's tip before the merge and
// applied after it — wakes the check: what counts is that its Apply finished after the merge, and
// the daemon compares no commit for it.
func TestAnOlderDeployRunApplyingAfterTheMergeWakesTheCheck(t *testing.T) {
	rig := newCheckWakeRig(t)
	rig.boot(t)

	rig.publishGitHub(t, "github-run-older", 801, checkWakeDeploy(checkWakeBehindBuild, 2*time.Hour))

	rig.awaitLook(t, "deploy run", checkWakeDeployDetail(801, "deploy", 2*time.Hour))
	if compared := rig.github.compares(); len(compared) != 1 {
		t.Fatalf("GitHub compared %v, want only the boot's compare of the daemon's own build", compared)
	}
}

// A run of the deploy workflow whose Apply was skipped (a preview), one whose Apply finished before
// the merge, and a run of another workflow of the repository wake nothing: the first two are read
// from GitHub and found no deploy after the merge, the third is no deploy and is not read at all.
// The check stays at its ended look 1, nothing is kept for a next look, and the implementer stays
// suspended.
func TestDeployRunsThatDidNotApplyAfterTheMergeWakeNothing(t *testing.T) {
	rig := newCheckWakeRig(t)
	rig.boot(t)

	rig.publishGitHub(t, "github-run-preview", 901, checkWakeRun{workflow: checkWakeDeployWorkflow, headSHA: checkWakeAheadBuild, jobs: []string{
		checkWakeJob(1, "deploy", checkWakeStep("Plan", "success", 5*time.Minute), checkWakeStep("Apply", "skipped", 6*time.Minute)),
	}})
	rig.publishGitHub(t, "github-run-before", 902, checkWakeDeploy(checkWakeBehindBuild, -10*time.Minute))
	rig.publishGitHub(t, "github-run-ci", 903, checkWakeRun{workflow: checkWakeOtherWorkflow, headSHA: checkWakeAheadBuild, jobs: []string{
		checkWakeJob(1, "deploy", checkWakeStep("Apply", "success", 10*time.Minute)),
	}})
	rig.awaitCompared(t, "github-run-preview", "github-run-before", "github-run-ci")

	if check := rig.check(t); check.Looks != 1 || check.LookOpen || check.LastWakeKind != "merge" || check.PendingCauseKind != "" {
		t.Fatalf("check = look %d (open %v) woken by %q, pending %q; want look 1 still ended by the merge's look and nothing kept",
			check.Looks, check.LookOpen, check.LastWakeKind, check.PendingCauseKind)
	}
	if resumes := resumesOf(rig.rt, rig.implementer); len(resumes) != 0 {
		t.Fatalf("the implementer was resumed %d times, want never", len(resumes))
	}
	for run, why := range map[string]string{"901": `step "Apply" succeeded in no job of the attempt`, "902": `step "Apply" succeeded only at or before the merge`} {
		if n := rig.logged(t, "outbox check compare finished without acting: the run did not deploy after the merge", map[string]string{"run": run, "why": why}); n != 1 {
			t.Fatalf("the daemon said run %s did not deploy after the merge (%s) %d times, want once", run, why, n)
		}
	}
	if reads := rig.github.runRead(903); reads != 0 {
		t.Fatalf("GitHub was asked about the ci.yaml run %d times, want never: it is no deploy", reads)
	}
	if n := rig.logged(t, "workflow: a deploy run is compared against the open production checks", map[string]string{"run": "903"}); n != 0 {
		t.Fatalf("the daemon compared the ci.yaml run %d times, want never", n)
	}
}

// One run with two jobs that each applied after the merge is two deploys: the job that applied
// first wakes look 2 and the other is kept for the next look, which starts once look 2 is reported
// pending.
func TestOneDeployRunWithTwoApplyingJobsWakesTwoLooks(t *testing.T) {
	rig := newCheckWakeRig(t)
	rig.boot(t)

	rig.publishGitHub(t, "github-run-two-jobs", 1001, checkWakeRun{workflow: checkWakeDeployWorkflow, headSHA: checkWakeAheadBuild, jobs: []string{
		checkWakeJob(11, "deploy-east", checkWakeStep("Apply", "success", 9*time.Minute)),
		checkWakeJob(12, "deploy-west", checkWakeStep("Apply", "success", 4*time.Minute)),
	}})

	west, east := checkWakeDeployDetail(1001, "deploy-west", 4*time.Minute), checkWakeDeployDetail(1001, "deploy-east", 9*time.Minute)
	rig.awaitLook(t, "deploy run", west)
	testwait.Eventually(t, "the later job's deploy kept for the next look", func() bool {
		check := rig.check(t)
		return check.PendingCauseKind == "deploy run" && check.PendingCauseDetail == east
	})

	rig.reportPending(t)

	rig.awaitWake(t, 3, "deploy run", east)
	rig.awaitTask(t, "Look 3, woken by the deploy workflow's run: "+east+".")
}
