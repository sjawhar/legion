package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
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

// checkWakeGitHub is GitHub's REST API for sjawhar/legion as a daemon's production check wakes
// read it: the repository (every launch reads its default branch), the merged pull request, and
// the compare of the check's merge with a build, answered `ahead` for the build that carries it,
// `behind` for the daemon's own and the one that does not, and 404 for a commit it does not know.
// It records every compare's build, in order.
type checkWakeGitHub struct {
	url      string
	mu       sync.Mutex
	compared []string
}

func newCheckWakeGitHub(t *testing.T) *checkWakeGitHub {
	t.Helper()
	g := &checkWakeGitHub{}
	status := map[string]string{checkWakeDaemonBuild: "behind", checkWakeBehindBuild: "behind", checkWakeAheadBuild: "ahead"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/sjawhar/legion":
			_, _ = io.WriteString(w, `{"full_name":"sjawhar/legion","default_branch":"main"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/sjawhar/legion/pulls/1300":
			_, _ = io.WriteString(w, `{"number":1300,"merged":true,"merge_commit_sha":"`+checkWakeMerge+`"}`)
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
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
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
// state a wake reaches: a check with no look open, its implementer's session kept.
type checkWakeRig struct {
	cfg         config.Config
	root        string
	implementer claim.Token
	js          jetstream.JetStream
	github      *checkWakeGitHub
	rt          *fake.Runtime
	records     record.Store
	pool        *pgxpool.Pool
}

func newCheckWakeRig(t *testing.T) *checkWakeRig {
	t.Helper()
	pool := isolatedOutboxPool(t)
	natsURL := workflowNATS(t)
	cfg := workflowConfig(t, natsURL)
	cfg.PostgresDSN = pool.Config().ConnString()
	cfg.DispatchURL = checkWakeDispatch(t, cfg.Project)
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
		rt: fake.NewRuntime(), records: record.NewStore(), pool: pool}

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
		done <- run(ctx, rig.cfg, Build{Revision: checkWakeDaemonBuild, Repository: "sjawhar/legion"}, quietLogger(), overrides{
			listen: heldListen, runtime: fakeRuntime(rig.rt, &built{}).runtime, clock: stillClock{},
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
	data, err := json.Marshal(map[string]any{
		"event_id": eventID, "source": "dispatch", "source_event_id": eventID + "-source", "topic": subject,
		"dedupe_key": eventID, "issued_at": 1, "payload_summary": "dispatch event", "payload": payload, "trace_id": eventID + "-trace",
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
	testwait.Eventually(t, "look 2 of the check woken by "+kind, func() bool {
		check := rig.check(t)
		return check.Looks == 2 && check.LookOpen && check.LastWakeKind == kind && check.LastWakeDetail == detail
	})
	testwait.Eventually(t, "the look's start row resuming the implementer", func() bool {
		return len(resumesOf(rig.rt, rig.implementer)) == 1
	})
	if resume := resumesOf(rig.rt, rig.implementer)[0]; resume.Spec.ResumeSessionFile != filepath.Join(rig.cfg.StateDir, "implementer.jsonl") {
		t.Fatalf("resumed %+v, want the implementer's own kept session", resume.Spec)
	}
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
