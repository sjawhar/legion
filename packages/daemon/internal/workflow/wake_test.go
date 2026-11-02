package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// wakeAt is when every wake of these tests reaches the daemon, and wakeIssueGeneration the run
// the released pod was serving.
var wakeAt = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

const wakeIssueGeneration = 7

// wakeRig is a released pod the tests wake: a root LEGION-208 implementing at generation 7, its
// pull request sjawhar/legion#42 open at head1, and its implementer and architect claims both
// suspended with the source `released`, which is the state a release leaves a pod in.
type wakeRig struct {
	pool   *pgxpool.Pool
	engine *Engine
	logged *bytes.Buffer
	apply  func(id string, fact intake.Fact) intake.Result
}

func newWakeRig(t *testing.T) wakeRig {
	t.Helper()
	pool := migratedPool(t)
	logged := &bytes.Buffer{}
	clock := &checkClock{now: wakeAt}
	engine := New(record.NewStore(), Config{Project: "LEGION", Pods: wakePods(t, pool), Linger: time.Hour, Clock: clock.Now},
		slog.New(slog.NewTextHandler(logged, nil)))
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: wakeIssueGeneration,
		Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
		Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head1", Failing: []string{}})
	seedReleasedClaim(t, pool, claim.RoleImplementer, "session-implementer")
	seedReleasedClaim(t, pool, claim.RoleArchitect, "session-architect")
	return wakeRig{pool: pool, engine: engine, logged: logged, apply: applyFacts(t, pool, engine)}
}

// wakePods is the project's pod records as the daemon hands them to the engine
// (store.Store.PodRecordsOf, over the project key alone): the engine carries no spelling of the
// project of its own, and the store derives the one its claim-keyed tables hold.
func wakePods(t *testing.T, pool *pgxpool.Pool) Pods {
	t.Helper()
	st, err := store.Open(context.Background(), pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store over the test pool: %v", err)
	}
	t.Cleanup(st.Close)
	records, err := st.PodRecordsOf("LEGION")
	if err != nil {
		t.Fatalf("read the project's pod records: %v", err)
	}
	return records
}

// wakeProjectToken is the project as a claim token spells it, which is what `claims.project` and
// the pod tables of the release hold — never the key "LEGION", which only the record's own tables
// hold. The two differ (claim.ProjectToken lower-cases), and a fixture that seeded the key on both
// sides is what hid the engine reading those tables under the key.
func wakeProjectToken(t *testing.T) string {
	t.Helper()
	project, err := claim.ProjectToken("LEGION")
	if err != nil {
		t.Fatalf("the project token: %v", err)
	}
	return project
}

// wakeToken is the claim token of LEGION-208's role, which a role topic is named by.
func wakeToken(t *testing.T, role claim.Role) claim.Token {
	t.Helper()
	token, err := claim.NewToken(wakeProjectToken(t), "LEGION-208", role)
	if err != nil {
		t.Fatalf("derive the %s token: %v", role, err)
	}
	return token
}

// seedReleasedClaim records one claim of LEGION-208's pod as a release leaves it: suspended, with
// the source `released`, its session kept and the generation it was serving recorded.
func seedReleasedClaim(t *testing.T, pool *pgxpool.Pool, role claim.Role, session string) {
	t.Helper()
	token := wakeToken(t, role)
	if _, err := pool.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation,
		session, session_file, state, suspended_by, serving_generation, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ($1, $5, 'LEGION-208', 'LEGION-208', $2, 1, $3, '', 'suspended', 'released', $4, 0, 0, 0, 0)`,
		string(token), string(role), session, int64(wakeIssueGeneration), wakeProjectToken(t)); err != nil {
		t.Fatalf("seed the released %s claim: %v", role, err)
	}
}

// claimSuspendedBy re-stamps a released claim's suspension source, which is what every path that
// takes a released claim over does (supervise's suspendedAgain).
func claimSuspendedBy(t *testing.T, pool *pgxpool.Pool, role claim.Role, source string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `update claims set suspended_by = $2 where token = $1`,
		string(wakeToken(t, role)), source); err != nil {
		t.Fatalf("take the %s claim over: %v", role, err)
	}
}

// wakeStart is one supervise start row a wake wrote: the role it starts and the task it carries.
type wakeStart struct {
	Role claim.Role
	Task string
}

// wakeStarts is every supervise `start` row in the outbox, oldest first.
func wakeStarts(t *testing.T, pool *pgxpool.Pool) []wakeStart {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select payload from outbox where kind = 'supervise' order by id`)
	if err != nil {
		t.Fatalf("read the supervise rows: %v", err)
	}
	defer rows.Close()
	var starts []wakeStart
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			t.Fatalf("read a supervise row: %v", err)
		}
		var request record.SuperviseRequest
		if err := json.Unmarshal(payload, &request); err != nil {
			t.Fatalf("decode a supervise row: %v", err)
		}
		if request.Op != "start" {
			continue
		}
		starts = append(starts, wakeStart{Role: request.Role, Task: request.Task})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the supervise rows: %v", err)
	}
	return starts
}

// wakeResumes is every pod_resumes row of LEGION-208, oldest first, as (role, kind) pairs.
func wakeResumes(t *testing.T, pool *pgxpool.Pool) [][2]string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select role, kind from pod_resumes where issue = 'LEGION-208' order by id`)
	if err != nil {
		t.Fatalf("read the pod resumes: %v", err)
	}
	defer rows.Close()
	var resumes [][2]string
	for rows.Next() {
		var role, kind string
		if err := rows.Scan(&role, &kind); err != nil {
			t.Fatalf("read a pod resume: %v", err)
		}
		resumes = append(resumes, [2]string{role, kind})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the pod resumes: %v", err)
	}
	return resumes
}

// oneWake fails the test unless exactly one start row woke role with a task naming each of names.
func oneWake(t *testing.T, pool *pgxpool.Pool, role claim.Role, kind string, names ...string) {
	t.Helper()
	starts := wakeStarts(t, pool)
	if len(starts) != 1 {
		t.Fatalf("supervise start rows = %+v, want the one wake of the %s", starts, role)
	}
	if starts[0].Role != role {
		t.Fatalf("the wake started %s, want %s", starts[0].Role, role)
	}
	for _, name := range names {
		if !strings.Contains(starts[0].Task, name) {
			t.Fatalf("the woken role's task = %q, want it to name %q", starts[0].Task, name)
		}
	}
	resumes := wakeResumes(t, pool)
	if len(resumes) != 1 || resumes[0] != [2]string{string(role), kind} {
		t.Fatalf("pod resumes = %v, want the one %s wake of the %s", resumes, kind, role)
	}
}

// settlement is one CI settlement of LEGION-208's pull request at head.
func settlement(head string, failing ...string) intake.PullRequestChecks {
	return intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: head, Generation: 1,
		Snapshot: head + "-snapshot", CheckRuns: []record.AttemptRun{{Name: "build", ID: 1}}, Failing: failing}
}

// R2: a settlement on the head of the issue's pull request wakes the released phase worker whose
// phase is open, and the woken role's task names the head and what the settlement came to.
func TestACISettlementWakesTheReleasedPhaseWorker(t *testing.T) {
	rig := newWakeRig(t)
	rig.apply("settled", settlement("head1", "build"))
	oneWake(t, rig.pool, claim.RoleImplementer, pods.WakeChecks, "head1", "build")
}

// R2: one head settles again as each of its checks finishes, and each settlement of that head
// wakes the pod at most once.
func TestASecondSettlementOfTheSameHeadWakesThePodNoSecondTime(t *testing.T) {
	rig := newWakeRig(t)
	rig.apply("settled", settlement("head1", "build"))
	rig.apply("settled-again", settlement("head1"))
	oneWake(t, rig.pool, claim.RoleImplementer, pods.WakeChecks, "head1")
	if resumes := wakeResumes(t, rig.pool); len(resumes) != 1 {
		t.Fatalf("pod resumes after a second settlement of the same head = %v, want the one", resumes)
	}
}

// R3: a message published to the role topic of a role of the pod wakes that role, whatever the
// publish kept, and the woken role's task says a message was sent to its topic.
func TestARoleMessageWakesTheReleasedRole(t *testing.T) {
	rig := newWakeRig(t)
	token := wakeToken(t, claim.RoleImplementer)
	rig.apply("role-message", intake.ListenerWake{Kind: intake.ListenerWakeRole, Role: string(token), At: wakeAt, Report: "report-1"})
	oneWake(t, rig.pool, claim.RoleImplementer, pods.WakeRoleMessage, string(token), "role topic")
}

// R3: the same report delivered twice wakes the pod once; the mark is keyed on the listener's own
// report id.
func TestARoleMessageReportedTwiceWakesThePodOnce(t *testing.T) {
	rig := newWakeRig(t)
	token := wakeToken(t, claim.RoleImplementer)
	fact := intake.ListenerWake{Kind: intake.ListenerWakeRole, Role: string(token), At: wakeAt, Report: "report-1"}
	rig.apply("role-message", fact)
	rig.apply("role-message-redelivered", fact)
	oneWake(t, rig.pool, claim.RoleImplementer, pods.WakeRoleMessage, string(token))
}

// R3: a role message for a released sub-architect wakes the architect, which stays the message's
// owner.
func TestARoleMessageWakesAReleasedArchitect(t *testing.T) {
	rig := newWakeRig(t)
	token := wakeToken(t, claim.RoleArchitect)
	rig.apply("role-message", intake.ListenerWake{Kind: intake.ListenerWakeRole, Role: string(token), At: wakeAt, Report: "report-1"})
	oneWake(t, rig.pool, claim.RoleArchitect, pods.WakeRoleMessage, string(token))
}

// R4: a message sent to the agent subject of a released session wakes the role that kept that
// session, and the woken role's task names the session.
func TestAnAgentMessageWakesTheReleasedSession(t *testing.T) {
	rig := newWakeRig(t)
	rig.apply("agent-message", intake.ListenerWake{Kind: intake.ListenerWakeAgent, Session: "session-implementer", At: wakeAt, Report: "report-2"})
	oneWake(t, rig.pool, claim.RoleImplementer, pods.WakeAgentMessage, "session-implementer", "sent to your session")
}

// R4: a report naming a session no released claim of this daemon's project kept wakes nothing.
// Every daemon consumes the subject, and only the one that owns the session acts.
func TestAnAgentMessageForAnotherDaemonsSessionWakesNothing(t *testing.T) {
	rig := newWakeRig(t)
	rig.apply("agent-message", intake.ListenerWake{Kind: intake.ListenerWakeAgent, Session: "somebody-elses-session", At: wakeAt, Report: "report-2"})
	if starts := wakeStarts(t, rig.pool); len(starts) != 0 {
		t.Fatalf("supervise start rows = %+v, want none for another daemon's session", starts)
	}
}

// R7: the listener's liveness report is recorded and wakes nothing.
func TestAListenerReadyReportIsRecordedAndWakesNothing(t *testing.T) {
	rig := newWakeRig(t)
	rig.apply("ready", intake.ListenerWake{Kind: intake.ListenerWakeReady, At: wakeAt, Report: "report-3"})
	if starts := wakeStarts(t, rig.pool); len(starts) != 0 {
		t.Fatalf("supervise start rows = %+v, want none for a liveness report", starts)
	}
	var at time.Time
	if err := rig.pool.QueryRow(context.Background(), `select ready_at from listener_reports where project = $1`, wakeProjectToken(t)).Scan(&at); err != nil {
		t.Fatalf("read the listener's report: %v", err)
	}
	if !at.UTC().Equal(wakeAt) {
		t.Fatalf("the listener's report is stamped %s, want the listener's own %s", at.UTC(), wakeAt)
	}
}

// armBackstop arms one backstop of LEGION-208's role, due at due, and answers its row id.
func armBackstop(t *testing.T, pool *pgxpool.Pool, role claim.Role, due time.Time) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `insert into pod_wake_backstops (project, issue, role, release_id, due_at)
		values ($3, 'LEGION-208', $1, 0, $2) returning id`, string(role), due, wakeProjectToken(t)).Scan(&id); err != nil {
		t.Fatalf("arm the %s backstop: %v", role, err)
	}
	return id
}

// R5: the backstop wakes the phase worker it was armed for, and its task says the backstop ran
// out.
func TestTheBackstopWakesThePhaseWorker(t *testing.T) {
	rig := newWakeRig(t)
	id := armBackstop(t, rig.pool, claim.RoleImplementer, wakeAt.Add(-time.Minute))
	rig.apply("backstop", intake.PodBackstopDue{ID: id, Issue: "LEGION-208", Role: claim.RoleImplementer})
	oneWake(t, rig.pool, claim.RoleImplementer, pods.WakeBackstop, "backstop")
}

// R5: a backstop two passes read fires once. The row's conditional fire is what decides it, so
// the second fact changes nothing even under a different event id.
func TestABackstopReadTwiceFiresOnce(t *testing.T) {
	rig := newWakeRig(t)
	id := armBackstop(t, rig.pool, claim.RoleImplementer, wakeAt.Add(-time.Minute))
	fact := intake.PodBackstopDue{ID: id, Issue: "LEGION-208", Role: claim.RoleImplementer}
	rig.apply("backstop", fact)
	rig.apply("backstop-again", fact)
	oneWake(t, rig.pool, claim.RoleImplementer, pods.WakeBackstop, "backstop")
	var fired bool
	if err := rig.pool.QueryRow(context.Background(), `select fired from pod_wake_backstops where id = $1`, id).Scan(&fired); err != nil {
		t.Fatalf("read the backstop: %v", err)
	}
	if !fired {
		t.Fatalf("the backstop is not marked fired")
	}
}

// R5: an architect gets no backstop. OpenPhaseRole is the one statement of "the worker of the
// open phase" — which role a release arms a backstop for, which role a CI settlement wakes, and
// which live claim has a phase reminder armed in it — and it answers none for a phase the
// architect works and none for a phase no role works.
func TestAnArchitectGetsNoBackstop(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    phase.Phase
		want claim.Role
	}{
		{name: "implementing", p: phase.Implementing, want: claim.RoleImplementer},
		{name: "testing", p: phase.Testing, want: claim.RoleTester},
		{name: "accepting is the architect's", p: phase.Accepting},
		{name: "awaiting a person", p: phase.AwaitingPerson},
		{name: "done", p: phase.Done},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := OpenPhaseRole(tc.p); got != tc.want {
				t.Fatalf("OpenPhaseRole(%s) = %q, want %q", tc.p, got, tc.want)
			}
		})
	}
}

// R8: a linger, a park and a close each take the issue out of the workflow or stop its tree, and
// a wake of any kind then does nothing.
func TestAWakeDoesNothingOnceTheIssueHasLeftTheWorkflow(t *testing.T) {
	lingerUntil := wakeAt.Add(time.Hour)
	for _, tc := range []struct {
		name  string
		issue record.Issue
	}{
		{name: "a park", issue: record.Issue{Status: "backlog", Phase: phase.Implementing}},
		{name: "a close", issue: record.Issue{Status: "done", Phase: phase.Done}},
		{name: "a linger", issue: record.Issue{Status: "in_progress", Phase: phase.Done, LingerUntil: &lingerUntil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newWakeRig(t)
			issue := tc.issue
			issue.Key, issue.Tree, issue.Project, issue.Title = "LEGION-208", "LEGION-208", "LEGION", "root"
			issue.Repo, issue.GitHubRepo, issue.DefaultBranch = "sjawhar/legion", "sjawhar/legion", "main"
			issue.Generation, issue.Rank, issue.LastDispatchSeq = wakeIssueGeneration, "U", 2
			seedIssue(t, rig.pool, issue)
			token := wakeToken(t, claim.RoleImplementer)
			rig.apply("role-message", intake.ListenerWake{Kind: intake.ListenerWakeRole, Role: string(token), At: wakeAt, Report: "report-1"})
			rig.apply("agent-message", intake.ListenerWake{Kind: intake.ListenerWakeAgent, Session: "session-implementer", At: wakeAt, Report: "report-2"})
			id := armBackstop(t, rig.pool, claim.RoleImplementer, wakeAt.Add(-time.Minute))
			rig.apply("backstop", intake.PodBackstopDue{ID: id, Issue: "LEGION-208", Role: claim.RoleImplementer})
			if starts := wakeStarts(t, rig.pool); len(starts) != 0 {
				t.Fatalf("supervise start rows after %s = %+v, want none", tc.name, starts)
			}
			if resumes := wakeResumes(t, rig.pool); len(resumes) != 0 {
				t.Fatalf("pod resumes after %s = %v, want none", tc.name, resumes)
			}
		})
	}
}

// R8: an operator's suspend of a released claim re-stamps its suspension source, and a wake then
// finds no released claim to act on.
func TestAWakeDoesNothingOnceAnOperatorTookTheClaimOver(t *testing.T) {
	rig := newWakeRig(t)
	claimSuspendedBy(t, rig.pool, claim.RoleImplementer, "operator")
	token := wakeToken(t, claim.RoleImplementer)
	rig.apply("role-message", intake.ListenerWake{Kind: intake.ListenerWakeRole, Role: string(token), At: wakeAt, Report: "report-1"})
	rig.apply("settled", settlement("head1", "build"))
	if starts := wakeStarts(t, rig.pool); len(starts) != 0 {
		t.Fatalf("supervise start rows after an operator suspend = %+v, want none", starts)
	}
}

// R8: a daemon suspension — a linger, a park, a close or a hand-over — takes a released claim
// over the same way, and a wake then does nothing either.
func TestAWakeDoesNothingOnceTheDaemonTookTheClaimOver(t *testing.T) {
	rig := newWakeRig(t)
	claimSuspendedBy(t, rig.pool, claim.RoleImplementer, "daemon")
	rig.apply("settled", settlement("head1", "build"))
	if starts := wakeStarts(t, rig.pool); len(starts) != 0 {
		t.Fatalf("supervise start rows after a daemon suspend = %+v, want none", starts)
	}
}

// R6: every wake's task names the wake, so the woken agent's first turn reads why its pod came
// back without reading anything else.
func TestWakeTaskNamesItsWake(t *testing.T) {
	for _, tc := range []struct {
		kind, detail, want string
	}{
		{kind: pods.WakeChecks, detail: "head1 failed build", want: "head1 failed build"},
		{kind: pods.WakeRoleMessage, detail: "legion-LEGION-208-implementer", want: "role topic"},
		{kind: pods.WakeAgentMessage, detail: "session-1", want: "sent to your session"},
		{kind: pods.WakeBackstop, detail: "the backstop came due", want: "backstop"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			task := WakeTask(tc.kind, tc.detail)
			if !strings.Contains(task, tc.want) || !strings.Contains(task, "released") {
				t.Fatalf("WakeTask(%s) = %q, want it to name the release and %q", tc.kind, task, tc.want)
			}
		})
	}
}
