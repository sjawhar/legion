package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

const branchMainCommit = "c0ffee0123456789abcdef0123456789abcdef01"

// branchCreated, branchExists and branchRefused are GitHub's answers to a create of a ref.
var (
	branchCreated = func() (int, string) {
		return http.StatusCreated, `{"ref":"refs/heads/legion/LEGION-208","object":{"sha":"` + branchMainCommit + `"}}`
	}
	branchExists = func() (int, string) {
		return http.StatusUnprocessableEntity, `{"message":"Reference already exists","status":"422"}`
	}
	branchRefused = func() (int, string) {
		return http.StatusForbidden, `{"message":"Resource not accessible by integration","status":"403"}`
	}
)

// branchGitHub is GitHub's REST API for acme/widgets as an issue_branch row calls it: main's ref, and
// each create of a ref answered by answer. It records each create, with the launches the runtime had
// made when it came.
type branchGitHub struct {
	url     string
	mu      sync.Mutex
	answer  func() (int, string)
	creates []branchCreate
}

type branchCreate struct {
	ref, sha, authorization string
	launched                int
}

func newBranchGitHub(t *testing.T, rt *fake.Runtime, answer func() (int, string)) *branchGitHub {
	t.Helper()
	g := &branchGitHub{answer: answer}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/git/ref/heads/main":
			_, _ = io.WriteString(w, `{"ref":"refs/heads/main","object":{"sha":"`+branchMainCommit+`","type":"commit"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widgets/git/refs":
			var body struct{ Ref, SHA string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode the create: %v", err)
			}
			launched := 0
			if rt != nil {
				launched = len(rt.CallsOf("Spawn")) + len(rt.CallsOf("Resume"))
			}
			g.mu.Lock()
			g.creates = append(g.creates, branchCreate{ref: body.Ref, sha: body.SHA, authorization: r.Header.Get("Authorization"), launched: launched})
			status, answer := g.answer()
			g.mu.Unlock()
			w.WriteHeader(status)
			_, _ = io.WriteString(w, answer)
		default:
			t.Errorf("unexpected GitHub request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	g.url = server.URL
	return g
}

func (g *branchGitHub) created() []branchCreate {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]branchCreate(nil), g.creates...)
}

func (g *branchGitHub) answerWith(answer func() (int, string)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.answer = answer
}

// roleTokens mints a token named for the App it is minted as, so a request shows which App made it.
type roleTokens struct{}

func (roleTokens) Token(_ context.Context, role appauth.AppRole, owner string) (appauth.Lease, error) {
	return appauth.Lease{Token: string(role) + "-app-token-for-" + owner, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

// branchAdmission is an outbox over a fresh database whose admission admits LEGION-208 when the
// test applies its todo, with GitHub answered by github and the runner's clock the test's own.
type branchAdmission struct {
	runner *outbox
	rt     *fake.Runtime
	clock  *time.Time
	logs   *bytes.Buffer
}

func newBranchAdmission(t *testing.T, answer func() (int, string)) (*branchAdmission, *branchGitHub) {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	github := newBranchGitHub(t, rt, answer)
	engine := workflow.New(records, workflow.Config{Project: "legion"}, quietLogger())
	admission := admit.New(records, engine, 2, "legion", quietLogger())
	clock := time.Now()
	var logs bytes.Buffer
	runner := &outbox{
		dispatchProject: "LEGION",
		pool:            pool, records: records, supervisor: sup, tokens: roleTokens{}, project: "legion", stateDir: t.TempDir(),
		repo: ghrepo.MustParse("acme/widgets"), githubAPI: github.url,
		dispatch: &outboxDispatch{issue: dispatch.Issue{Key: "LEGION-208", Status: "todo"}}, notices: &outboxPublisher{}, handlers: []intake.Handler{engine, admission},
		now: func() time.Time { return clock }, log: slog.New(slog.NewJSONHandler(&logs, nil)),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
		},
	}
	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "todo", intake.DispatchIssue{
		Key: "LEGION-208", Seq: 1, Type: "issue.created", Status: "todo", Title: "Workflow", Rank: "U", HandedOver: true,
	}, engine, admission); err != nil {
		t.Fatalf("admit LEGION-208: %v", err)
	}
	clock = time.Now().Add(time.Minute)
	return &branchAdmission{runner: runner, rt: rt, clock: &clock, logs: &logs}, github
}

// run runs the outbox's due rows once, a minute of the runner's clock after its previous run.
func (b *branchAdmission) run(t *testing.T) {
	t.Helper()
	*b.clock = b.clock.Add(time.Minute)
	if err := b.runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the outbox: %v", err)
	}
}

func (b *branchAdmission) launches() int {
	return len(b.rt.CallsOf("Spawn")) + len(b.rt.CallsOf("Resume"))
}

// Admitting a root creates its branch on GitHub at main, as the implement App, before its
// architect starts: the architect's start waits for the branch's row. A branch GitHub already has
// (its create answered 422 "Reference already exists") is the same success.
func TestAdmissionCreatesTheRootsBranchAtMainBeforeItsArchitectStarts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func() (int, string)
	}{
		{name: "created", answer: branchCreated},
		{name: "already exists", answer: branchExists},
	} {
		t.Run(tc.name, func(t *testing.T) {
			admitted, github := newBranchAdmission(t, tc.answer)

			admitted.run(t)
			admitted.run(t)

			want := branchCreate{ref: "refs/heads/legion/LEGION-208", sha: branchMainCommit, authorization: "Bearer implement-app-token-for-acme"}
			if got := github.created(); len(got) != 1 || got[0] != want {
				t.Fatalf("GitHub creates = %+v, want one, %+v, before any launch", got, want)
			}
			if got := admitted.launches(); got != 1 {
				t.Fatalf("the runtime saw %d launches, want the architect's one", got)
			}
		})
	}
}

// A create GitHub refuses leaves the root's architect unstarted: the branch's row retries on the
// outbox's backoff, each attempt logged with the repository, the ref, GitHub's status and its body,
// and the start behind it waits. Once a create passes, the architect starts.
func TestARefusedBranchCreateHoldsTheArchitectBackUntilACreatePasses(t *testing.T) {
	admitted, github := newBranchAdmission(t, branchRefused)

	for range 3 {
		admitted.run(t)
	}

	if got := admitted.launches(); got != 0 {
		t.Fatalf("the runtime saw %d launches while GitHub refused the branch, want none", got)
	}
	if got := len(github.created()); got != 3 {
		t.Fatalf("GitHub saw %d creates, want one per run", got)
	}
	want := "create the branch of LEGION-208, which its roles' starts wait for: create refs/heads/legion/LEGION-208 on acme/widgets at " +
		branchMainCommit + `: GitHub answered POST /git/refs with 403: {"message":"Resource not accessible by integration","status":"403"}`
	var lastError string
	var attempts int
	if err := admitted.runner.pool.QueryRow(context.Background(), "select last_error, attempts from outbox where kind = 'issue_branch'").Scan(&lastError, &attempts); err != nil {
		t.Fatalf("read the branch's row: %v", err)
	}
	if lastError != want || attempts != 3 {
		t.Fatalf("the branch's row has %d attempts, last error %q; want 3 and %q", attempts, lastError, want)
	}
	logged := 0
	for line := range strings.Lines(admitted.logs.String()) {
		var entry struct{ Msg, Kind, Error string }
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		if entry.Msg == "outbox row failed" && entry.Kind == string(record.OutboxKindIssueBranch) && entry.Error == want {
			logged++
		}
	}
	if logged != 3 {
		t.Fatalf("the log has %d failed branch rows naming GitHub's answer, want 3:\n%s", logged, admitted.logs)
	}

	github.answerWith(branchCreated)
	admitted.run(t)
	admitted.run(t)

	if got := admitted.launches(); got != 1 {
		t.Fatalf("the runtime saw %d launches once GitHub created the branch, want the architect's one", got)
	}
}

// A branch row of a generation the issue has left, or of a tree that lingers, asks GitHub for
// nothing and finishes: a refused create of a parked tree never retries forever, and the starts it
// held back meet their own fences.
func TestAnIssueBranchRowOfALeftRunFinishesWithoutACreate(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	github := newBranchGitHub(t, nil, branchRefused)
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, tokens: roleTokens{}, project: "legion",
		repo: ghrepo.MustParse("acme/widgets"), githubAPI: github.url}
	until := time.Now().Add(time.Hour)
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Admitted,
		Generation: 2, Status: "in_progress", Rank: "U"})
	parent := "LEGION-210"
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-210", Project: "LEGION", Tree: "LEGION-210", Title: "Closed", Phase: phase.Done,
		Generation: 1, Status: "backlog", Rank: "V", LingerUntil: &until})
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-211", Project: "LEGION", Tree: "LEGION-210", Parent: &parent, Title: "Child",
		Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "W"})

	for _, row := range []record.OutboxRow{
		mustOutboxRow(t, "LEGION-208", record.IssueBranch{Generation: 1}, time.Now()),
		mustOutboxRow(t, "LEGION-211", record.IssueBranch{Generation: 1}, time.Now()),
	} {
		if err := runner.execute(context.Background(), row); err != nil {
			t.Fatalf("execute the branch row of %s: %v", row.Issue, err)
		}
	}
	if got := github.created(); len(got) != 0 {
		t.Fatalf("GitHub creates = %+v, want none", got)
	}
}
