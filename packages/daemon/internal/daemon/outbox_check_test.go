package daemon

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// checkRoot is a closed root whose tree lingers for another hour: the state a production check's
// look runs in, where a phase start would finish without acting.
func checkRoot() record.Issue {
	until := time.Now().Add(time.Hour)
	return record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-208", Title: "root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "U", LingerUntil: &until}
}

// openCheck is issue's production check on acme/widgets#42 at merge, opened an hour ago with look 1
// open since, woken by the merge, and bound two weeks out.
func openCheck(issue record.Issue, merge string) record.ProductionCheck {
	opened := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	return record.ProductionCheck{
		Issue: issue.Key, Project: issue.Project, Tree: issue.Tree, Repo: "acme/widgets", Number: 42, MergeSHA: merge,
		OpenedAt: opened, BoundAt: opened.Add(336 * time.Hour), Looks: 1, LookOpen: true, LookWakeAt: &opened,
		LastWakeKind: "merge", LastWakeDetail: merge, LastWakeAt: &opened,
	}
}

func putProductionCheck(t *testing.T, pool *pgxpool.Pool, records record.Store, check record.ProductionCheck) {
	t.Helper()
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return records.PutProductionCheck(context.Background(), tx, check)
	}); err != nil {
		t.Fatalf("put production check: %v", err)
	}
}

func readProductionCheck(t *testing.T, pool *pgxpool.Pool, records record.Store, issue string) record.ProductionCheck {
	t.Helper()
	var check *record.ProductionCheck
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		var err error
		check, err = records.ProductionCheck(context.Background(), tx, issue)
		return err
	}); err != nil || check == nil {
		t.Fatalf("read production check of %s = %v, %v", issue, check, err)
	}
	return *check
}

// suspendedImplementer is issue's implementer as the close or the linger left it: suspended by the
// rule, its session kept, in the issue's tree.
func suspendedImplementer(t *testing.T, sup *supervisor, issue record.Issue, by supervise.SuspendSource) (*supervise.Machine, claim.Token) {
	t.Helper()
	token, err := claim.NewToken("legion", issue.Key, claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	machine, _, err := sup.Create(context.Background(), supervise.Claim{Token: token, Project: "legion", Tree: issue.Tree, Issue: issue.Key, Role: claim.RoleImplementer, Generation: 1,
		State: supervise.StateSuspended, SuspendedBy: by, Session: "ses_implementer", SessionFile: "/sessions/implementer.jsonl"}, "")
	if err != nil {
		t.Fatal(err)
	}
	return machine, token
}

// checkStart is the start row of look of issue's production check: the implementer, no phase, the
// issue's generation, fenced on the check (record.SuperviseRequest.CheckLook), naming no instance
// (a row queued before starts carried one).
func checkStart(t *testing.T, issue record.Issue, look int) record.OutboxRow {
	t.Helper()
	row := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleImplementer, Generation: issue.Generation,
		CheckLook: look, Task: "Production check of " + issue.Key}, time.Now())
	row.ID = 57
	return row
}

// sameCheckFact says whether got is want, a fact of check: a check fact's OpenedAt is check's as
// an instant (the executor reads it from Postgres, in a location of pgx's choosing, where want
// names none) and every other field want's.
func sameCheckFact(got, want intake.Fact, check record.ProductionCheck) bool {
	switch got := got.(type) {
	case intake.CheckLookEnded:
		want, ok := want.(intake.CheckLookEnded)
		if !ok || !got.OpenedAt.Equal(check.OpenedAt) {
			return false
		}
		got.OpenedAt = want.OpenedAt
		return got == want
	case intake.CheckWake:
		want, ok := want.(intake.CheckWake)
		if !ok || !got.OpenedAt.Equal(check.OpenedAt) {
			return false
		}
		got.OpenedAt = want.OpenedAt
		return got == want
	}
	return got == want
}

// A check start is the one start a lingering tree runs: it reaches the implementer the linger
// suspended, resumes it, and delivers the look's task, where a phase start on the same tree finishes
// without acting. The delivery names no phase, so nothing drops it for the phase the issue has left,
// and the row's id is its delivery's.
func TestACheckStartResumesTheSuspendedImplementerUnderALingeringRoot(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	root := checkRoot()
	putOutboxIssue(t, pool, records, root)
	putProductionCheck(t, pool, records, openCheck(root, "a1b2c3d4"))
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	machine, _ := suspendedImplementer(t, sup, root, supervise.SuspendedByDaemon)
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), log: quietLogger(), now: time.Now}

	if err := runner.execute(ctx, checkStart(t, root, 1)); err != nil {
		t.Fatalf("check start under the lingering root = %v, want the implementer resumed", err)
	}
	got := machine.Claim()
	if got.LastStartRow != 57 || len(rt.CallsOf("Resume")) != 1 {
		t.Fatalf("implementer after the check start = %s with last start %d and %d resumes, want resumed by row 57", got.State, got.LastStartRow, len(rt.CallsOf("Resume")))
	}
	if got.Pending == nil || got.Pending.ID != "outbox:57" || got.Pending.Phase != "" || got.Pending.Generation != 1 || !strings.HasPrefix(got.Pending.Task, "Production check of ") {
		t.Fatalf("implementer's pending delivery = %+v, want the look's task under outbox:57 with no phase at generation 1", got.Pending)
	}
	// The same row again is the same delivery, not a second one.
	if err := runner.execute(ctx, checkStart(t, root, 1)); err != nil {
		t.Fatalf("the check start run again = %v, want its delivery accepted as the same", err)
	}
}

// A check start finishes without acting once its look is no longer the open one: the look ended (its
// bound, a report), a later look started, or the check closed. The implementer stays as it was, and
// no start is recorded against it.
func TestACheckStartFinishesWithoutActingWhenItsLookIsNotOpen(t *testing.T) {
	closed := time.Now()
	for _, tc := range []struct {
		name  string
		check func(record.ProductionCheck) record.ProductionCheck
	}{
		{name: "the look ended", check: func(c record.ProductionCheck) record.ProductionCheck { c.LookOpen = false; return c }},
		{name: "a later look is open", check: func(c record.ProductionCheck) record.ProductionCheck { c.Looks = 2; return c }},
		{name: "the check closed", check: func(c record.ProductionCheck) record.ProductionCheck {
			c.LookOpen, c.ClosedAt, c.Outcome = false, &closed, record.CheckRecorded
			return c
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			root := checkRoot()
			putOutboxIssue(t, pool, records, root)
			putProductionCheck(t, pool, records, tc.check(openCheck(root, "a1b2c3d4")))
			sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
			machine, _ := suspendedImplementer(t, sup, root, supervise.SuspendedByDaemon)
			runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), log: quietLogger(), now: time.Now}

			if err := runner.execute(context.Background(), checkStart(t, root, 1)); err != nil {
				t.Fatalf("check start of a look that is not open = %v, want finished without acting", err)
			}
			if got := machine.Claim(); got.State != supervise.StateSuspended || got.LastStartRow != 0 || got.Pending != nil || len(rt.CallsOf("Resume")) != 0 {
				t.Fatalf("implementer = %s with last start %d, pending %+v and %d resumes, want left suspended with nothing started", got.State, got.LastStartRow, got.Pending, len(rt.CallsOf("Resume")))
			}
		})
	}
}

// A check start of an issue that runs again finishes without acting: a root reopened inside its
// linger (admit's readmit) stands in `admitted` with the status of the reopen, and the look its
// row would start would resume the implementer and relaunch the Sandbox under the run. The row's
// check is still open at its look, so the check alone would let it start.
func TestACheckStartOfAnIssueRunningAgainFinishesWithoutActing(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	root := checkRoot()
	root.Phase, root.Status, root.LingerUntil = phase.Admitted, "todo", nil
	putOutboxIssue(t, pool, records, root)
	putProductionCheck(t, pool, records, openCheck(root, "a1b2c3d4"))
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	machine, _ := suspendedImplementer(t, sup, root, supervise.SuspendedByDaemon)
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), log: quietLogger(), now: time.Now}

	if err := runner.execute(context.Background(), checkStart(t, root, 1)); err != nil {
		t.Fatalf("check start of an issue running again = %v, want finished without acting", err)
	}
	if got := machine.Claim(); got.State != supervise.StateSuspended || got.LastStartRow != 0 || got.Pending != nil || len(rt.CallsOf("Resume")) != 0 {
		t.Fatalf("implementer = %s with last start %d, pending %+v and %d resumes, want left suspended with nothing started", got.State, got.LastStartRow, got.Pending, len(rt.CallsOf("Resume")))
	}
	if check := readProductionCheck(t, pool, records, root.Key); !check.Open() || !check.LookOpen {
		t.Fatalf("the check after the refused start = %+v, want untouched, its look ending at its own bound", check)
	}
}

// An operator's suspension of the implementer holds a phase start on the row's backoff until the
// operator resumes it; a look cannot wait that long, so the check start finishes without acting and
// ends the look as one nobody reported, with the hold as its cause, of the check the row's issue
// holds.
func TestACheckStartOnAnOperatorHeldImplementerEndsTheLook(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	root := checkRoot()
	putOutboxIssue(t, pool, records, root)
	check := openCheck(root, "a1b2c3d4")
	putProductionCheck(t, pool, records, check)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	machine, _ := suspendedImplementer(t, sup, root, supervise.SuspendedByOperator)
	facts := &outboxFactHandler{}
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
		handlers: []intake.Handler{facts}, log: quietLogger(), now: time.Now}

	if err := runner.execute(context.Background(), checkStart(t, root, 1)); err != nil {
		t.Fatalf("check start on the operator's hold = %v, want finished without acting", err)
	}
	if got := machine.Claim(); got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByOperator || got.LastStartRow != 0 || len(rt.CallsOf("Resume")) != 0 {
		t.Fatalf("implementer = %s by %s with last start %d and %d resumes, want left to the operator", got.State, got.SuspendedBy, got.LastStartRow, len(rt.CallsOf("Resume")))
	}
	want := intake.CheckLookEnded{Issue: root.Key, Look: 1, Cause: "operator hold"}
	if got := facts.facts(); len(got) != 1 || !sameCheckFact(got[0], want, check) {
		t.Fatalf("facts applied = %+v, want %+v of the check opened %s", got, want, check.OpenedAt)
	}
}

// A check start of a check a later merge replaced — its row names the first check's OpenedAt, and
// the issue's check is the second, at a look 1 of its own — finishes without acting: the
// implementer stays as it was, with no start recorded against it, since the look the row would
// start is the first check's, ended long before, and the second's look 1 has a start row of its own.
func TestACheckStartOfAReplacedCheckFinishesWithoutActing(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	root := checkRoot()
	putOutboxIssue(t, pool, records, root)
	second := openCheck(root, "f00dfeed")
	putProductionCheck(t, pool, records, second)
	first := openCheck(root, "a1b2c3d4")
	first.OpenedAt = second.OpenedAt.Add(-24 * time.Hour)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	machine, _ := suspendedImplementer(t, sup, root, supervise.SuspendedByDaemon)
	facts := &outboxFactHandler{}
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
		handlers: []intake.Handler{facts}, log: quietLogger(), now: time.Now}

	stale := mustOutboxRow(t, root.Key, record.SuperviseRequest{Op: "start", Tree: root.Tree, Role: claim.RoleImplementer, Generation: root.Generation,
		CheckLook: 1, CheckOpenedAt: first.OpenedAt, Task: "Production check of " + root.Key}, time.Now())
	stale.ID = 58
	if err := runner.execute(context.Background(), stale); err != nil {
		t.Fatalf("the first check's start against the second = %v, want finished without acting", err)
	}
	if got := machine.Claim(); got.State != supervise.StateSuspended || got.LastStartRow != 0 || got.Pending != nil || len(rt.CallsOf("Resume")) != 0 {
		t.Fatalf("implementer = %s with last start %d, pending %+v and %d resumes, want left suspended with nothing started", got.State, got.LastStartRow, got.Pending, len(rt.CallsOf("Resume")))
	}
	if got := facts.facts(); len(got) != 0 {
		t.Fatalf("facts applied = %+v, want none", got)
	}
	if got := readProductionCheck(t, pool, records, root.Key); !got.LookOpen || got.Looks != 1 {
		t.Fatalf("the second check after the first check's start = look %d open %t, want its look 1 left open", got.Looks, got.LookOpen)
	}

	// The second check's own start row, naming it, resumes the implementer.
	own := mustOutboxRow(t, root.Key, record.SuperviseRequest{Op: "start", Tree: root.Tree, Role: claim.RoleImplementer, Generation: root.Generation,
		CheckLook: 1, CheckOpenedAt: second.OpenedAt, Task: "Production check of " + root.Key}, time.Now())
	own.ID = 59
	if err := runner.execute(context.Background(), own); err != nil {
		t.Fatalf("the second check's own start = %v, want the implementer resumed", err)
	}
	if got := machine.Claim(); got.LastStartRow != 59 || len(rt.CallsOf("Resume")) != 1 {
		t.Fatalf("implementer after the second check's start = %s with last start %d and %d resumes, want resumed by row 59", got.State, got.LastStartRow, len(rt.CallsOf("Resume")))
	}
}

// A check_look row ends the look it names as its bound; a check_bound row brings the check's bound
// due. Each is applied through the workflow's handlers: the look's under the event of the check
// and the look, so the row run again is a duplicate, and the bound's under the row's, so a bound
// the workflow deferred as a new row applies afresh.
func TestCheckLookAndCheckBoundRowsApplyTheirFacts(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	root := checkRoot()
	putOutboxIssue(t, pool, records, root)
	check := openCheck(root, "a1b2c3d4")
	putProductionCheck(t, pool, records, check)
	facts := &outboxFactHandler{}
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, handlers: []intake.Handler{facts}, log: quietLogger(), now: time.Now}

	look := mustOutboxRow(t, root.Key, record.CheckLook{Look: 1, OpenedAt: check.OpenedAt}, time.Now())
	look.ID = 61
	for range 2 {
		if err := runner.execute(ctx, look); err != nil {
			t.Fatalf("check_look row = %v", err)
		}
	}
	first := mustOutboxRow(t, root.Key, record.CheckBound{}, time.Now())
	first.ID = 62
	deferred := mustOutboxRow(t, root.Key, record.CheckBound{}, time.Now())
	deferred.ID = 63
	for _, row := range []record.OutboxRow{first, deferred} {
		if err := runner.execute(ctx, row); err != nil {
			t.Fatalf("check_bound row %d = %v", row.ID, err)
		}
	}
	want := []intake.Fact{
		intake.CheckLookEnded{Issue: root.Key, Look: 1, Cause: "look bound"},
		intake.CheckBound{Issue: root.Key},
		intake.CheckBound{Issue: root.Key},
	}
	got := facts.facts()
	if len(got) != len(want) {
		t.Fatalf("facts applied = %+v, want %+v", got, want)
	}
	for i := range want {
		if !sameCheckFact(got[i], want[i], check) {
			t.Fatalf("fact %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A check_look row of no open check — none recorded, or one closed since — and one of a check a
// later merge replaced (its OpenedAt another than the issue's check's) each finish without acting:
// the look they would end is not the issue's check's.
func TestACheckLookRowOfNoCheckOrAReplacedOneAppliesNothing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		recorded bool
		closed   bool
		replaced bool
	}{
		{name: "no check"},
		{name: "a closed check", recorded: true, closed: true},
		{name: "a replaced check", recorded: true, replaced: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			root := checkRoot()
			putOutboxIssue(t, pool, records, root)
			check := openCheck(root, "a1b2c3d4")
			if tc.closed {
				closed := time.Now().UTC().Truncate(time.Microsecond)
				check.LookOpen, check.ClosedAt, check.Outcome = false, &closed, record.CheckRecorded
			}
			if tc.recorded {
				putProductionCheck(t, pool, records, check)
			}
			named := check.OpenedAt
			if tc.replaced {
				named = named.Add(-24 * time.Hour)
			}
			facts := &outboxFactHandler{}
			runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, handlers: []intake.Handler{facts}, log: quietLogger(), now: time.Now}
			row := mustOutboxRow(t, root.Key, record.CheckLook{Look: 1, OpenedAt: named}, time.Now())
			row.ID = 61
			if err := runner.execute(context.Background(), row); err != nil {
				t.Fatalf("check_look row = %v, want finished without acting", err)
			}
			if got := facts.facts(); len(got) != 0 {
				t.Fatalf("facts applied = %+v, want none", got)
			}
		})
	}
}

// A second production check of the same issue — a rerun's, after the first closed and its merge
// opened another — counts its looks from one again, and its look 1 has a bound row of its own
// (workflow.startLook). That row must end that look: the first check's look 1 ended at its own
// bound long before, and nothing clears processed_events between the two checks, so the event the
// row applies under must tell the two checks apart. Otherwise a silent implementer keeps its pod
// for the whole of the rerun's check, the cost the look bound exists to remove.
func TestACheckLookRowOfALaterCheckEndsThatChecksLook(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	root := checkRoot()
	putOutboxIssue(t, pool, records, root)
	engine := workflow.New(records, workflow.Config{Project: "legion", Linger: 24 * time.Hour}, quietLogger())
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, handlers: []intake.Handler{engine}, log: quietLogger(), now: time.Now}

	first := openCheck(root, "a1b2c3d4")
	putProductionCheck(t, pool, records, first)
	look := mustOutboxRow(t, root.Key, record.CheckLook{Look: 1}, time.Now())
	look.ID = 61
	if err := runner.execute(ctx, look); err != nil {
		t.Fatalf("the first check's look bound = %v", err)
	}
	if got := readProductionCheck(t, pool, records, root.Key); got.LookOpen || !got.Open() {
		t.Fatalf("the first check after its look bound = look open %t, closed %t; want the look ended and the check open", got.LookOpen, !got.Open())
	}

	// The first check closed at its bound as not run; the rerun's merge opened the second a day
	// later, with its own look 1 and the look's own bound row.
	closed := time.Now().UTC().Truncate(time.Microsecond)
	first.ClosedAt, first.Outcome = &closed, record.CheckNotRun
	putProductionCheck(t, pool, records, first)
	second := openCheck(root, "f00dfeed")
	reopened := closed.Add(24 * time.Hour)
	second.OpenedAt, second.BoundAt, second.LookWakeAt, second.LastWakeAt, second.LastWakeDetail = reopened, reopened.Add(336*time.Hour), &reopened, &reopened, "f00dfeed"
	putProductionCheck(t, pool, records, second)
	later := mustOutboxRow(t, root.Key, record.CheckLook{Look: 1}, time.Now())
	later.ID = 64
	if err := runner.execute(ctx, later); err != nil {
		t.Fatalf("the second check's look bound = %v", err)
	}
	if got := readProductionCheck(t, pool, records, root.Key); got.LookOpen {
		t.Fatalf("the second check's look 1 is still open after its own bound row ran: the row's fact was taken as the first check's look 1 bound; want the look ended")
	}
}

// checkGitHub is GitHub's REST API for acme/widgets as a check_compare row calls it: the pull
// request, naming its merge commit, and the compare of a merge with a build, answered with status
// (`ahead`, `behind`, …) or the HTTP code when one is set. It records each read's path, in order.
type checkGitHub struct {
	url    string
	mu     sync.Mutex
	merge  string
	status string
	code   int
	reads  []string
}

func newCheckGitHub(t *testing.T, merge, status string, code int) *checkGitHub {
	t.Helper()
	g := &checkGitHub{merge: merge, status: status, code: code}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.reads = append(g.reads, r.URL.Path+"?"+r.URL.RawQuery)
		g.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer outbox-token" {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/42":
			_, _ = io.WriteString(w, `{"number":42,"merged":true,"merge_commit_sha":"`+g.merge+`"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/acme/widgets/compare/"):
			if g.code != 0 {
				w.WriteHeader(g.code)
				_, _ = io.WriteString(w, `{"message":"Not Found"}`)
				return
			}
			_, _ = io.WriteString(w, `{"status":"`+g.status+`","ahead_by":1,"behind_by":0,"files":[]}`)
		default:
			t.Errorf("unexpected GitHub request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	g.url = server.URL
	return g
}

func (g *checkGitHub) read() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.reads...)
}

// A check_compare row reads GitHub as the implement App for whether the build carries the check's
// merge: a compare that answers ahead (or identical) wakes the check, under an event keyed by the
// check, the build and what asked for it, the fact naming the check; behind or diverged wakes
// nothing; a commit GitHub does not know wakes nothing either; any other failure is the row's to
// retry. A check opened without its merge commit has it read from the pull request and recorded
// before the compare, and a check closed since, or one a later merge replaced (the row names an
// earlier check's OpenedAt), reads nothing.
func TestACheckCompareWakesTheCheckOnlyWhenTheBuildCarriesTheMerge(t *testing.T) {
	const merge, build = "a1b2c3d4e5f6a7b8c9d0a1b2c3d4e5f6a7b8c9d0", "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c"
	compare := "/repos/acme/widgets/compare/" + merge + "..." + build + "?per_page=1"
	pull := "/repos/acme/widgets/pulls/42?"
	for _, tc := range []struct {
		name     string
		recorded string
		status   string
		code     int
		closed   bool
		replaced bool
		wantErr  string
		reads    []string
		facts    []intake.Fact
	}{
		{name: "ahead wakes", recorded: merge, status: "ahead", reads: []string{compare},
			facts: []intake.Fact{intake.CheckWake{Issue: "LEGION-208", Look: 1, Cause: "daemon boot", Detail: build}}},
		{name: "identical wakes", recorded: merge, status: "identical", reads: []string{compare},
			facts: []intake.Fact{intake.CheckWake{Issue: "LEGION-208", Look: 1, Cause: "daemon boot", Detail: build}}},
		{name: "behind wakes nothing", recorded: merge, status: "behind", reads: []string{compare}},
		{name: "diverged wakes nothing", recorded: merge, status: "diverged", reads: []string{compare}},
		{name: "an unknown commit wakes nothing", recorded: merge, code: http.StatusNotFound, reads: []string{compare}},
		{name: "another failure retries", recorded: merge, code: http.StatusBadGateway, reads: []string{compare}, wantErr: "502"},
		{name: "an empty merge commit is read from the pull request first", status: "ahead", reads: []string{pull, compare},
			facts: []intake.Fact{intake.CheckWake{Issue: "LEGION-208", Look: 1, Cause: "daemon boot", Detail: build}}},
		{name: "a closed check reads nothing", recorded: merge, status: "ahead", closed: true},
		{name: "a replaced check reads nothing", recorded: merge, status: "ahead", replaced: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			root := checkRoot()
			putOutboxIssue(t, pool, records, root)
			check := openCheck(root, tc.recorded)
			if tc.closed {
				closed := time.Now()
				check.LookOpen, check.ClosedAt, check.Outcome = false, &closed, record.CheckRecorded
			}
			putProductionCheck(t, pool, records, check)
			github := newCheckGitHub(t, merge, tc.status, tc.code)
			facts := &outboxFactHandler{}
			runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, tokens: outboxTokens{}, githubAPI: github.url, project: "legion",
				handlers: []intake.Handler{facts}, log: quietLogger(), now: time.Now}
			named := check.OpenedAt
			if tc.replaced {
				named = named.Add(-24 * time.Hour)
			}
			row := mustOutboxRow(t, root.Key, record.CheckCompare{Look: 1, Cause: "daemon boot", Build: build, OpenedAt: named}, time.Now())
			row.ID = 71

			err := runner.execute(context.Background(), row)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("check compare = %v, want it finished", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("check compare = %v, want a failure naming %s", err, tc.wantErr)
			}
			if got := github.read(); strings.Join(got, " ") != strings.Join(tc.reads, " ") {
				t.Fatalf("GitHub reads = %v, want %v", got, tc.reads)
			}
			got := facts.facts()
			if len(got) != len(tc.facts) {
				t.Fatalf("facts applied = %+v, want %+v", got, tc.facts)
			}
			for i := range tc.facts {
				if !sameCheckFact(got[i], tc.facts[i], check) {
					t.Fatalf("fact %d = %+v, want %+v", i, got[i], tc.facts[i])
				}
			}
			if !tc.closed {
				if recorded := readProductionCheck(t, pool, records, root.Key); recorded.MergeSHA != merge {
					t.Fatalf("the check's merge commit = %q, want %s recorded", recorded.MergeSHA, merge)
				}
			}
		})
	}
}

// A wake of a build that already woke the check applies once: the second compare row of the same
// build and cause is a duplicate of the first's event, so the workflow is not asked again.
func TestACheckCompareOfTheSameBuildWakesOnce(t *testing.T) {
	const merge, build = "a1b2c3d4e5f6a7b8c9d0a1b2c3d4e5f6a7b8c9d0", "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c"
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	root := checkRoot()
	putOutboxIssue(t, pool, records, root)
	putProductionCheck(t, pool, records, openCheck(root, merge))
	github := newCheckGitHub(t, merge, "ahead", 0)
	facts := &outboxFactHandler{}
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, tokens: outboxTokens{}, githubAPI: github.url, project: "legion",
		handlers: []intake.Handler{facts}, log: quietLogger(), now: time.Now}
	for id := int64(71); id <= 72; id++ {
		row := mustOutboxRow(t, root.Key, record.CheckCompare{Look: 1, Cause: "daemon boot", Build: build}, time.Now())
		row.ID = id
		if err := runner.execute(context.Background(), row); err != nil {
			t.Fatalf("check compare row %d = %v", id, err)
		}
	}
	if got := facts.facts(); len(got) != 1 {
		t.Fatalf("facts applied = %+v, want the one wake", got)
	}
}

// A row whose effect waits for a known moment (wait.Until: a lingering tree's close waiting for a
// member's production check, bounded at a time the record holds) runs again at that moment rather
// than on its backoff; a moment already past is the backoff's.
func TestSettleRunsARowAgainAtTheMomentItsWaitNames(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, tc := range []struct {
		name string
		at   time.Time
		want time.Time
	}{
		{name: "a moment ahead", at: now.Add(3 * time.Hour), want: now.Add(3 * time.Hour)},
		{name: "a moment past", at: now.Add(-time.Minute), want: now.Add(time.Second)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.LingerClose{Generation: 1}, now))
			waiting := &outboxFactHandler{err: wait.Until(tc.at, "tree LEGION-208 lingers past its deadline; member LEGION-209's production check is open")}
			runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, handlers: []intake.Handler{waiting}, log: quietLogger(), now: func() time.Time { return now }}

			if err := runner.RunOnce(context.Background()); err != nil {
				t.Fatalf("run once: %v", err)
			}
			var attempts int
			var nextAt time.Time
			var lastError string
			if err := pool.QueryRow(context.Background(), "select attempts, next_at, last_error from outbox").Scan(&attempts, &nextAt, &lastError); err != nil {
				t.Fatalf("read the waiting row: %v", err)
			}
			if attempts != 1 || !nextAt.Equal(tc.want) || !strings.Contains(lastError, "production check is open") {
				t.Fatalf("retry = attempts=%d next_at=%s error=%q, want one attempt due at %s", attempts, nextAt, lastError, tc.want)
			}
		})
	}
}

// The boot fact carries the binary's revision and, for a release build with a Dispatch client, the
// build Dispatch reports on /healthz, which the workflow compares against the open production
// checks as it compares the revision. A build with no revision — every non-release build — carries
// none and asks Dispatch nothing; a Dispatch that cannot be asked is a warning and a boot that
// compares none of its build, never a failed boot. The engine's BuiltFrom follows the same guard:
// the built-from repository reaches it only with a revision to compare.
func TestTheBootFactCarriesTheBuildsAndAsksDispatchOnlyForAReleaseBuild(t *testing.T) {
	release := Build{Revision: "a1b2c3d4e5f6a7b8c9d0a1b2c3d4e5f6a7b8c9d0", Repository: "sjawhar/legion"}
	for _, tc := range []struct {
		name      string
		build     Build
		dispatch  *outboxDispatch
		want      intake.DaemonBooted
		asked     int
		builtFrom string
	}{
		{name: "a release build", build: release, dispatch: &outboxDispatch{build: "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c"},
			want: intake.DaemonBooted{Boot: 7, Revision: release.Revision, DispatchBuild: "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c"}, asked: 1, builtFrom: "sjawhar/legion"},
		{name: "a release build, Dispatch reporting no commit", build: release, dispatch: &outboxDispatch{},
			want: intake.DaemonBooted{Boot: 7, Revision: release.Revision}, asked: 1, builtFrom: "sjawhar/legion"},
		{name: "a release build, Dispatch unreachable", build: release, dispatch: &outboxDispatch{buildErr: errors.New("dial tcp: connection refused")},
			want: intake.DaemonBooted{Boot: 7, Revision: release.Revision}, asked: 1, builtFrom: "sjawhar/legion"},
		{name: "a release build with no Dispatch client", build: release,
			want: intake.DaemonBooted{Boot: 7, Revision: release.Revision}, builtFrom: "sjawhar/legion"},
		{name: "a non-release build", build: Build{Repository: "sjawhar/legion"}, dispatch: &outboxDispatch{build: "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c"},
			want: intake.DaemonBooted{Boot: 7}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			facts := &outboxFactHandler{}
			w := &workflowRuntime{pool: pool, dispatchProject: "LEGION", build: tc.build, handlers: []intake.Handler{facts}, log: quietLogger()}
			if tc.dispatch != nil {
				w.dispatch = tc.dispatch
			}
			if err := w.booted(context.Background(), 7); err != nil {
				t.Fatalf("booted = %v", err)
			}
			if got := facts.facts(); len(got) != 1 || got[0] != tc.want {
				t.Fatalf("the boot applied %+v, want %+v", got, tc.want)
			}
			if tc.dispatch != nil && tc.dispatch.buildAsked != tc.asked {
				t.Fatalf("Dispatch's build commit was asked %d times, want %d", tc.dispatch.buildAsked, tc.asked)
			}
			if got := engineConfig(config.Config{Project: "LEGION"}, "legion-reviewer[bot]", tc.build).BuiltFrom; got != tc.builtFrom {
				t.Fatalf("the engine's BuiltFrom = %q, want %q", got, tc.builtFrom)
			}
		})
	}
}
