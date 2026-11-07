package daemon

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// The commit this rig is driven with: the merge the check was opened on.
const checkWakeMerge = "a1b2c3d4e5f6a7b8c9d0a1b2c3d4e5f6a7b8c9d0"

// checkWakeMergedAt is when sjawhar/legion#1300 merged, GitHub's merged_at.
var checkWakeMergedAt = time.Date(2026, 10, 19, 5, 0, 0, 0, time.UTC)

// checkWakeGitHub is GitHub's REST API for sjawhar/legion as a daemon over an open production
// check reads it: the repository (every launch reads its default branch), the merged pull request
// with its merge time, and the compare of the check's merge with a build, answered `ahead` for
// every build, so a compare the daemon made would carry the merge. It records every path read.
type checkWakeGitHub struct {
	url   string
	mu    sync.Mutex
	paths []string
}

func newCheckWakeGitHub(t *testing.T) *checkWakeGitHub {
	t.Helper()
	g := &checkWakeGitHub{}
	g.url = githubtest.Server(t, githubtest.Spec{Record: func(r *http.Request) {
		g.mu.Lock()
		g.paths = append(g.paths, r.URL.Path)
		g.mu.Unlock()
	}, Handle: func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/sjawhar/legion":
			_, _ = io.WriteString(w, `{"full_name":"sjawhar/legion","default_branch":"main"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/sjawhar/legion/pulls/1300":
			_, _ = io.WriteString(w, `{"number":1300,"merged":true,"merged_at":"`+checkWakeMergedAt.Format(time.RFC3339)+`","merge_commit_sha":"`+checkWakeMerge+`"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/sjawhar/legion/compare/"):
			_, _ = io.WriteString(w, `{"status":"ahead","ahead_by":1,"behind_by":0,"files":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
		}
	}})
	return g
}

// read is every path GitHub was asked for so far, in order.
func (g *checkWakeGitHub) read() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.paths...)
}

// checkWakeDispatch is a Dispatch that maps sjawhar/legion to project as its default and lists no
// issues; every other call is unavailable, as a Dispatch whose dependencies are down answers, and
// is not what these tests are about.
func checkWakeDispatch(t *testing.T, project string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/settings/repo-projects":
			_, _ = io.WriteString(w, `[{"repo":"sjawhar/legion","project":"`+project+`","default":true}]`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/issues":
			_, _ = io.WriteString(w, `[]`)
		default:
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// checkWakeRig is a daemon over a closed root of its project whose tree lingers for another hour,
// the root's implementer suspended by the daemon as the close left it, and the root's production
// check on sjawhar/legion#1300 at checkWakeMerge, its first look (the merge's) ended as pending.
// It is the state a wake would reach: a check with no look open, its implementer's session kept.
// The daemon's log is kept.
type checkWakeRig struct {
	cfg         config.Config
	root        string
	implementer claim.Token
	github      *checkWakeGitHub
	rt          *fake.Runtime
	records     record.Store
	pool        *pgxpool.Pool
	logs        *syncBuffer
	// built is what the daemon handed the fake runtime: the worker stream a pane's shim dials.
	built *built
}

func newCheckWakeRig(t *testing.T) *checkWakeRig {
	t.Helper()
	pool := isolatedOutboxPool(t)
	natsURL := workflowNATS(t)
	cfg := workflowConfig(t, natsURL)
	cfg.PostgresDSN = pool.Config().ConnString()
	cfg.DispatchURL = checkWakeDispatch(t, cfg.Project)
	// The daemon refuses to boot without Envoy's notification stream; no test of this rig
	// publishes to it.
	workflowJetStream(t, natsURL)
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
	rig := &checkWakeRig{cfg: cfg, root: root, implementer: implementer, github: newCheckWakeGitHub(t),
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
	// A wake resumes the implementer, and a launch provisions the issue's workspace when the host
	// has none, or when its checkout never finished — which reaches GitHub. The workspace is here
	// already, whole, so the launch provisions nothing and no test of this rig clones from GitHub.
	hostWorkspace(t, cfg.StateDir, "sjawhar/legion", root)
	return rig
}

// boot runs the daemon as the release build over the rig until the test ends, returning once it
// answers /healthz and its boot fact has been applied: whatever the boot does for an open
// production check, it has done by then.
func (rig *checkWakeRig) boot(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, rig.cfg, slog.New(slog.NewJSONHandler(rig.logs, nil)), overrides{
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
	testwait.Eventually(t, "the daemon's boot fact applied", func() bool {
		var applied bool
		if err := rig.pool.QueryRow(context.Background(),
			`select exists (select 1 from processed_events where source = 'daemon' and event_id like 'boot:%')`).Scan(&applied); err != nil {
			t.Fatalf("read whether the boot fact was applied: %v", err)
		}
		return applied
	})
}

// check is the root's production check as recorded now.
func (rig *checkWakeRig) check(t *testing.T) record.ProductionCheck {
	t.Helper()
	return readProductionCheck(t, rig.pool, rig.records, rig.root)
}

// A daemon that boots on a build containing an open check's merge commit starts no look: no boot
// reads production any more. GitHub would answer `ahead` for this build, and the check is the one
// a compare would have woken — open, its first look ended, its implementer suspended with its
// session kept — so a boot that still compared would start look 2 here. Nothing is compared, the
// check stays as it was and the implementer is never resumed.
func TestABootOnABuildCarryingTheMergeStartsNoLook(t *testing.T) {
	rig := newCheckWakeRig(t)
	rig.boot(t)

	for _, path := range rig.github.read() {
		if strings.Contains(path, "/compare/") {
			t.Fatalf("the boot read %s, want no compare of its build against the check's merge", path)
		}
	}
	if check := rig.check(t); check.Looks != 1 || check.LookOpen || check.LastWakeKind != "merge" {
		t.Fatalf("check = look %d (open %v) woken by %q; want look 1 still ended by the merge's look",
			check.Looks, check.LookOpen, check.LastWakeKind)
	}
	if resumes := resumesOf(rig.rt, rig.implementer); len(resumes) != 0 {
		t.Fatalf("the implementer was resumed %d times, want never: the boot wakes no check", len(resumes))
	}
}
