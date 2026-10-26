//go:build e2e

// The live proof's driver for the two rules this change adds: an acceptance waits for the issues
// beneath the issue being accepted (signOff's openBeneath), and a merged root's close waits for
// the rest of its tree (closeMerged's openBeside, closeAwaitingRoot, and the boot's sweepMerged).
// scripts/e2e/merged-root-waits.sh builds this driver with `-tags e2e` and owns the nats-server and
// the PostgreSQL it runs against.
//
// The daemon is the whole daemon: run, the function `legion start` calls, through this package's
// own whole-daemon harness (startDaemon), so the proof exercises the real configuration load, the
// real store and its migrations, the real HTTP API, the real claim supervision, the real
// openWorkflow, the real JetStream consumers and the real outbox lanes. Facts enter only by the
// transports production has — the GitHub and Dispatch envelopes of the ENVOY_NOTIFICATIONS stream,
// and the daemon's own HTTP routes — and every assertion is a read of GET /legion/v1/state, of what
// the daemon posted to the Dispatch stand-in or published to the Envoy listener stand-in, or of what
// the fake worker runtime was asked to launch and suspend. Nothing reads the store to judge a step,
// and nothing calls into the workflow package. The one read of the store outside the seeding is
// step 7's wait for the daemon's record that it took the event the step drove, which no route
// shows; what the step holds is still read from GET /legion/v1/state.
//
// What the proof stands in for lies outside the two rules. The broker is a native nats-server the
// script starts and names in LEGION_E2E_NATS_URL, rather than the testcontainers one workflowNATS
// starts. Dispatch is this package's own stand-in (workflowConfig's) extended to record what the
// daemon writes to it, liveDispatch, from the harness this driver shares with the package's other
// whole-daemon live proof (live_proof_test.go). Envoy's listener is the package's noticeListener,
// which takes every notice the daemon publishes to a role topic. GitHub is the package's
// repositoriesGitHub, and the two Apps' tokens are workflowTokenRecorder's placeholders. No agent
// runs: every claim is launched by the package's fake runtime and driven over the daemon's own
// worker stream and HTTP routes, as mergedRootWaitsClaimant does. The issue, child, pull-request
// and slot records that put the tree at each step are seeded through the store, as every daemon
// test seeds its fixture; what is driven is the change.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// The commits the two merges carry.
const (
	mergedRootWaitsParentMerge = "merge-parent"
	mergedRootWaitsChildMerge  = "merge-child"
)

// mergedRootWaitsLive is one run of the proof: the booted daemon, the tree it runs, and the three
// stand-ins the steps read the daemon's own writes back from.
type mergedRootWaitsLive struct {
	liveProof
	rt    *fake.Runtime
	built *built
	// parent is the tree's root, the issue that is accepted and whose merge waits; child is the
	// one issue beneath it.
	parent, child string
	// parentPR and childPR are their pull requests' numbers, and openedPR the pull request step 7
	// opens from the root's branch, all this run's own: the store a pod re-runs the proof against
	// holds every earlier run's pull requests, and a fact names a pull request by its repository
	// and number.
	parentPR, childPR, openedPR int
	// github is the REST and GraphQL root the daemon's workflow reads (mergedRootWaitsGitHub).
	github string
	// arch is the root's architect, the one session more than one step acts as: steps 1 and 6
	// share its registration (architect).
	arch *mergedRootWaitsClaimant
	// seq is the Dispatch sequence the next envelope the driver publishes carries.
	seq int64
}

// TestAMergedRootsCloseWaitsForTheRestOfItsTreeLive drives the seven steps of the proof against one
// booted daemon, in the order each step's state follows from the last: the acceptance refused
// while the child runs and accepted once it is out (the plan's steps 1 and 6, the second the
// first's control, with the park the refusal itself names between them), then the root's merge
// and its wait, the restart that must leave that wait as it was, the child's completion in the
// wait — as the session the restart's own relaunch gave it — then a pull request opened from the
// waiting root's branch, which must leave the merge the root waits on, and last the child's merge,
// which closes the root on that merge.
func TestAMergedRootsCloseWaitsForTheRestOfItsTreeLive(t *testing.T) {
	r := newMergedRootWaitsLive(t)
	r.step("step1-the-acceptance-waits-for-the-child", r.acceptanceRefused)
	r.step("step6-the-acceptance-lands-once-the-child-is-out", r.acceptanceAccepted)
	r.step("step2-the-merged-root-waits-for-its-child", r.rootMergeWaits)
	r.step("step5-a-restart-leaves-the-wait-and-tells-no-one-again", r.restartLeavesTheWait)
	r.step("step3-the-waiting-roots-child-completes-its-phase", r.childCompletesItsPhase)
	r.step("step7-a-pull-request-opened-from-the-waiting-roots-branch-leaves-its-merge", r.openedLeavesTheMerge)
	r.step("step4-the-childs-merge-closes-the-root", r.childMergeClosesTheRoot)
}

// newMergedRootWaitsLive boots the daemon over the script's broker with the tree already recorded:
// the root in accepting on its reviewed, open pull request, holding its tree's admission slot, and
// the one child beneath it implementing on an open pull request of its own.
func newMergedRootWaitsLive(t *testing.T) *mergedRootWaitsLive {
	t.Helper()
	proof := newLiveProof(t, "scripts/e2e/merged-root-waits.sh")
	number, err := strconv.ParseInt(randomSuffix(t)[:6], 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	r := &mergedRootWaitsLive{
		liveProof: proof, rt: fake.NewRuntime(), built: &built{},
		parent: proof.cfg.Project + "-1", child: proof.cfg.Project + "-2",
		parentPR: int(number) + 1, childPR: int(number) + 2, openedPR: int(number) + 3,
		github: mergedRootWaitsGitHub(t), seq: 1,
	}
	r.seedTree()
	r.boot()
	return r
}

// mergedRootWaitsGitHub is GitHub as this proof's daemon reads it: every repository of the
// mapping has default branch main, and its GraphQL root answers that it knows no pull request, so
// each disarm the workflow queues finishes with nothing armed to take back. Nothing else is
// answered, and nothing reaches github.com.
func mergedRootWaitsGitHub(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest, isRepository := strings.CutPrefix(r.URL.Path, "/repos/")
		switch parts := strings.Split(rest, "/"); {
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequest":null}}}`)
		case r.Method == http.MethodGet && isRepository && len(parts) == 2:
			_, _ = io.WriteString(w, `{"full_name":"`+parts[0]+"/"+parts[1]+`","default_branch":"main"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// seedTree records the tree as the daemon would hold it at the moment the reviewer approved the
// root: the root in accepting with its reviewer's handoff and its open pull request, the slot its
// tree holds, its lifecycle open under the workflow, the child implementing beneath it on a pull
// request of its own, and the root's architect claim as a launch a previous daemon began and never
// recorded, which this boot relaunches. That last one is how the proof reaches a live architect
// session without the operator's spawn, which would take the tree's lifecycle from the workflow.
func (r *mergedRootWaitsLive) seedTree() {
	t := r.t
	t.Helper()
	ctx := context.Background()
	r.board.show(r.parent, "needs_review")
	r.board.show(r.child, "in_progress")
	r.write("the tree", func(tx pgx.Tx) error {
		if err := r.records.PutIssue(ctx, tx, record.Issue{
			Key: r.parent, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Tree: r.parent, Project: r.cfg.Project,
			Title: "the tree's root", Phase: phase.Accepting, Generation: 1, Status: "needs_review", DispatchStatus: "needs_review", Rank: "U",
			LastDispatchSeq: r.seq, HandedOver: true,
		}); err != nil {
			return err
		}
		parent := r.parent
		if err := r.records.PutIssue(ctx, tx, record.Issue{
			Key: r.child, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Tree: r.parent, Project: r.cfg.Project,
			Title: "the child beneath it", Parent: &parent, Phase: phase.Implementing, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress",
			Rank: "V", LastDispatchSeq: r.seq,
		}); err != nil {
			return err
		}
		if err := r.records.PutPhase(ctx, tx, record.PhaseRow{Issue: r.parent, Role: claim.RoleReviewer, Claim: "seeded-reviewer", HandoffCommit: "head-parent"}); err != nil {
			return err
		}
		for _, pr := range []record.PullRequest{
			{State: record.PullRequestOpen, Issue: r.parent, Repo: liveRepo, Number: r.parentPR, Branch: "legion/" + r.parent, HeadSHA: "head-parent", Failing: []string{}},
			{State: record.PullRequestOpen, Issue: r.child, Repo: liveRepo, Number: r.childPR, Branch: "legion/" + r.child, HeadSHA: "head-child", Failing: []string{}},
		} {
			if err := r.records.PutPullRequest(ctx, tx, pr); err != nil {
				return err
			}
		}
		return r.admit(ctx, tx, r.parent)
	})
	if _, err := r.st.OpenTreeLifecycle(ctx, r.project, r.parent, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open the lifecycle of %s: %v", r.parent, err)
	}
	architect, err := claim.NewToken(r.project, r.parent, claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.AdmitClaim(ctx, supervise.Claim{
		Token: architect, Project: r.project, Tree: r.parent, Issue: r.parent, Role: claim.RoleArchitect,
		Generation: 1, State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)),
	}); err != nil {
		t.Fatalf("record the architect's unfinished launch: %v", err)
	}
	writePrompt(t, r.cfg, architect)
}

// boot runs the daemon over the seeded store until the test ends, and returns once it answers
// /healthz.
func (r *mergedRootWaitsLive) boot() {
	r.t.Helper()
	o := fakeRuntime(r.rt, r.built)
	o.workflowTokens = &workflowTokenRecorder{}
	o.githubAPI = r.github
	r.d = startDaemonWithin(r.t, r.cfg, o, r.log(), 60*time.Second)
}

// restart stops the daemon and boots another one on the same store, as an operator's `legion
// restart` does: a fresh fake runtime, whose claims the boot re-adopts, and the same stand-ins.
func (r *mergedRootWaitsLive) restart() {
	r.t.Helper()
	r.d.stop()
	rebindHeldPorts(r.t, &r.cfg)
	r.rt, r.built = fake.NewRuntime(), &built{}
	r.boot()
}

// mergedRootWaitsClaimant is one role's claim as its agent reaches it over the daemon's own
// routes, and the one place the driver holds that dance: the boot token of the launch the fake
// runtime recorded, the shim that dials the worker stream with it, the registration that
// exchanges it for the session's capability secret, the ready its session reports, and the grants
// that secret buys.
type mergedRootWaitsClaimant struct {
	r       *mergedRootWaitsLive
	token   claim.Token
	issue   string
	session string
	secret  string
}

// architect is the root's architect, registered the first time a step asks for it: the boot
// relaunched the claim seedTree left unfinished, so the session the steps act as is the tree's own
// architect claim, reached over the daemon's own routes.
func (r *mergedRootWaitsLive) architect() *mergedRootWaitsClaimant {
	r.t.Helper()
	if r.arch == nil {
		r.arch = r.claimant(claim.RoleArchitect, r.parent, "ses_architect")
	}
	return r.arch
}

// claimant brings the launched claim of role on issue to ready, as its pane's shim and its agent
// do: it waits for the launch, dials the worker stream with the launch's boot token, registers
// the session and reports it ready.
func (r *mergedRootWaitsLive) claimant(role claim.Role, issue, session string) *mergedRootWaitsClaimant {
	t := r.t
	t.Helper()
	token := r.token(issue, role)
	launch := r.awaitLaunch(token)
	dialShim(t, r.built.address, launch.BootToken)
	status, body := r.d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: launch.BootToken, SessionID: session, OmpSessionFile: "/sessions/" + session + ".jsonl",
		AgentID: session, PluginContract: api.DaemonAPIVersion,
	}, false)
	if status != http.StatusOK {
		t.Fatalf("register %s = %d %s, want 200", token, status, body)
	}
	var registered claim.RegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil {
		t.Fatalf("decode the registration of %s: %v", token, err)
	}
	if status, body := r.d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: session, Secret: registered.Secret, Generation: launch.Generation,
	}, false); status != http.StatusNoContent {
		t.Fatalf("ready %s = %d %s, want 204", token, status, body)
	}
	return &mergedRootWaitsClaimant{r: r, token: token, issue: issue, session: session, secret: registered.Secret}
}

// token is the claim token of role on issue, as the daemon spells it.
func (r *mergedRootWaitsLive) token(issue string, role claim.Role) claim.Token {
	r.t.Helper()
	token, err := claim.NewToken(r.project, issue, role)
	if err != nil {
		r.t.Fatal(err)
	}
	return token
}

// awaitLaunch waits for the running daemon to launch token's claim and returns that launch.
func (r *mergedRootWaitsLive) awaitLaunch(token claim.Token) runtime.SpawnSpec {
	r.t.Helper()
	testwait.Eventually(r.t, "the launch of "+string(token), func() bool {
		for _, call := range r.rt.Calls() {
			if (call.Method == "Spawn" || call.Method == "Resume") && call.Spec.Claim == token {
				return true
			}
		}
		return false
	})
	return lastLaunch(r.t, r.rt, token)
}

// grant is a capability grant minted for this session now, as its `legion` tool mints one per
// operation.
func (c *mergedRootWaitsClaimant) grant() string {
	t := c.r.t
	t.Helper()
	status, body := c.r.d.request(http.MethodPost, "/legion/v1/grants",
		api.GrantRequest{SessionID: c.session, Secret: c.secret, Tree: c.r.parent, Issue: c.issue}, false)
	if status != http.StatusOK {
		t.Fatalf("the grant of %s = %d %s, want 200", c.token, status, body)
	}
	var granted api.GrantResponse
	if err := json.Unmarshal(body, &granted); err != nil {
		t.Fatalf("decode the grant of %s: %v", c.token, err)
	}
	return granted.GrantID
}

// write runs body in one transaction against the daemon's own database, failing the test with
// what it was seeding on an error.
func (r *mergedRootWaitsLive) write(what string, body func(tx pgx.Tx) error) {
	r.t.Helper()
	if err := r.st.Tx(context.Background(), body); err != nil {
		r.t.Fatalf("seed %s: %v", what, err)
	}
}

// seedIssue reads key's record and writes it back as change left it: the fixture advance that puts
// the tree at the next step, which no fact of this driver drives.
func (r *mergedRootWaitsLive) seedIssue(what, key string, change func(issue *record.Issue)) {
	r.t.Helper()
	ctx := context.Background()
	r.write(what, func(tx pgx.Tx) error {
		issue, err := r.records.Issue(ctx, tx, key)
		if err != nil {
			return err
		}
		if issue == nil {
			return fmt.Errorf("%s is not recorded", key)
		}
		change(issue)
		return r.records.PutIssue(ctx, tx, *issue)
	})
	r.note("seeded: %s", what)
}

// waitFor polls the state until want is true of key, failing with what it last saw.
func (r *mergedRootWaitsLive) waitFor(what, key string, want func(api.Issue) bool) api.Issue {
	r.t.Helper()
	var last api.Issue
	testwait.Eventually(r.t, what, func() bool {
		last = r.issue(key)
		return want(last)
	})
	return last
}

// Step 1. The architect accepts the root while the child is still in the workflow: the daemon
// refuses 409 SIGNOFF_CHILD_IN_WORKFLOW, names the child and the way out, and writes nothing — the
// root is still in accepting.
func (r *mergedRootWaitsLive) acceptanceRefused() {
	t := r.t
	architect := r.architect()
	status, body := r.d.request(http.MethodPost, "/legion/v1/signoff", api.SignOffRequest{GrantID: architect.grant(), Issue: r.parent}, false)
	r.note("drove POST /legion/v1/signoff of %s with the architect's grant while %s implements", r.parent, r.child)
	var failure api.Failure
	if err := json.Unmarshal(body, &failure); err != nil {
		t.Fatalf("decode the sign-off's answer %s: %v", body, err)
	}
	if status != http.StatusConflict || failure.Code != "SIGNOFF_CHILD_IN_WORKFLOW" {
		t.Fatalf("the sign-off = %d %s, want 409 SIGNOFF_CHILD_IN_WORKFLOW", status, body)
	}
	for _, want := range []string{r.child, "park_child"} {
		if !strings.Contains(failure.Error, want) {
			t.Fatalf("the refusal %q does not name %q", failure.Error, want)
		}
	}
	r.note("read back: %d %s — %s", status, failure.Code, failure.Error)
	if got := r.issue(r.parent); got.Phase != phase.Accepting {
		t.Fatalf("%s is in %s after the refused sign-off, want accepting", r.parent, got.Phase)
	}
	r.note("read back: %s is still in accepting in GET /legion/v1/state", r.parent)
}

// Step 6. The architect takes the child out of the workflow the way the refusal names — the park a
// person makes from the Dispatch dashboard, relayed as the issue's own Dispatch event — and the
// same sign-off is the acceptance it always was: the root reaches retro.
func (r *mergedRootWaitsLive) acceptanceAccepted() {
	t := r.t
	r.publishChildPark("backlog")
	r.note("drove the child's park: a notifications.dispatch.issue.%s.issue.updated envelope moving it to backlog", r.child)
	r.waitFor("the child out of the workflow", r.child, func(got api.Issue) bool { return got.Status == "backlog" })
	r.note("read back: %s is backlog in GET /legion/v1/state", r.child)

	architect := r.architect()
	status, body := r.d.request(http.MethodPost, "/legion/v1/signoff", api.SignOffRequest{GrantID: architect.grant(), Issue: r.parent}, false)
	if status != http.StatusOK {
		t.Fatalf("the sign-off with the child out = %d %s, want 200", status, body)
	}
	r.note("drove POST /legion/v1/signoff of %s again: %d", r.parent, status)
	got := r.waitFor("the accepted root in retro", r.parent, func(got api.Issue) bool { return got.Phase == phase.Retro })
	// The move's own Dispatch write is waited for here, so step 2's baseline of what the board has
	// taken is not raced by the write this sign-off enqueued.
	testwait.Eventually(t, "the sign-off's own retro status on the Dispatch stand-in", func() bool { return r.board.wrote(r.parent, "retro") })
	r.note("read back: %s is in %s, status %s, and the Dispatch stand-in took its retro write", r.parent, got.Phase, got.Status)
}

// Step 2. The root's pull request merges while the child runs: the merge is recorded, the close
// waits, and the architect is told once, by name, which issue it waits for. Nothing of the close
// happened — no Dispatch done, no suspension, and the tree keeps its slot.
func (r *mergedRootWaitsLive) rootMergeWaits() {
	t := r.t
	r.seedArcTwo()
	suspendsBefore := len(r.rt.CallsOf("Suspend"))
	writesBefore := len(r.board.writes())

	r.publishMerge(r.parent, r.parentPR, mergedRootWaitsParentMerge)
	r.note("drove the root's merge: a notifications.github.%s.pr.%d envelope, merged at %s",
		r.subjectRepo(), r.parentPR, mergedRootWaitsParentMerge)
	got := r.waitFor("the merge recorded on the root's pull request", r.parent, func(got api.Issue) bool {
		return got.PullRequest != nil && got.PullRequest.State == string(record.PullRequestMerged)
	})
	if got.Phase != phase.AwaitingMerge {
		t.Fatalf("%s is in %s after its merge, want awaiting_merge: its close waits for %s", r.parent, got.Phase, r.child)
	}
	if got.Status == "done" {
		t.Fatalf("%s is done on Dispatch after its merge, want the status its wait leaves", r.parent)
	}
	if got.Slot == nil {
		t.Fatalf("%s holds no admission slot after its merge, want the slot its tree still holds", r.parent)
	}
	if got.Check != nil {
		t.Fatalf("%s has a production check after its merge, want none until it closes", r.parent)
	}
	r.note("read back: %s is in %s, pullRequest.state %s, status %s, slot %d, no production check",
		r.parent, got.Phase, got.PullRequest.State, got.Status, got.Slot.Index)
	if suspends := r.rt.CallsOf("Suspend"); len(suspends) != suspendsBefore {
		t.Fatalf("the merge suspended %d claims, want none: no member of the tree is stopped while the close waits", len(suspends)-suspendsBefore)
	}
	r.note("read back: the fake runtime was asked to suspend nothing (%d suspensions, as before the merge)", suspendsBefore)
	for _, write := range r.board.writes()[writesBefore:] {
		if write.Issue == r.parent {
			t.Fatalf("the daemon wrote %+v to Dispatch, want nothing of the root while the close waits", write)
		}
	}
	r.note("read back: the Dispatch stand-in took no status write and no message for %s", r.parent)
	told := r.waitArchitectNotices(r.parent, "pr-merged", 1)
	if !strings.Contains(told[0].Payload, r.child) || !strings.Contains(told[0].Payload, fmt.Sprint(r.parentPR)) {
		t.Fatalf("the pr-merged notice reads %q, want it naming pull request #%d and %s", told[0].Payload, r.parentPR, r.child)
	}
	r.note("read back: one pr-merged notice on the architect's topic — %s", told[0].Payload)
}

// seedArcTwo puts the tree at the step the merge is driven from: the root through retro and
// merging to awaiting_merge on its open pull request, the child back in the workflow under it and
// implementing, as its architect's release left it, and the child's implementer claim as a launch
// a previous daemon began and never recorded, serving the run of the task it took. The restart of
// step 5 relaunches that claim, which is where step 3's session comes from.
func (r *mergedRootWaitsLive) seedArcTwo() {
	t := r.t
	t.Helper()
	r.seedIssue("the root in awaiting_merge, as its retro and its merger's READY left it", r.parent, func(issue *record.Issue) {
		issue.Phase = phase.AwaitingMerge
	})
	r.seedIssue("the child implementing and back in the workflow", r.child, func(issue *record.Issue) {
		issue.Phase, issue.Status = phase.Implementing, "in_progress"
	})
	r.board.show(r.child, "in_progress")
	implementer, err := claim.NewToken(r.project, r.child, claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.AdmitClaim(context.Background(), supervise.Claim{
		Token: implementer, Project: r.project, Tree: r.parent, Issue: r.child, Role: claim.RoleImplementer,
		Generation: 1, State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)),
		ServingGeneration: 1,
	}); err != nil {
		t.Fatalf("record the child implementer's unfinished launch: %v", err)
	}
	writePrompt(t, r.cfg, implementer)
	r.note("seeded: the child's implementer serving run 1, its launch unfinished")
}

// Step 3. The child's implementer completes its phase while its root's close waits. The tree is
// not lingering, so the completion is accepted and the child moves on; before this change the
// root's merge lingered the tree mid-turn and that completion was refused 409 TREE_LINGERING.
func (r *mergedRootWaitsLive) childCompletesItsPhase() {
	t := r.t
	implementer := r.claimant(claim.RoleImplementer, r.child, "ses_implementer")
	r.note("the child's implementer is registered and ready on the launch the restart made, serving run 1")
	status, body := r.d.request(http.MethodPost, "/legion/v1/handoff/complete",
		api.HandoffCompleteRequest{GrantID: implementer.grant(), Summary: "implemented the child", Commit: "head-child"}, false)
	if status != http.StatusOK {
		t.Fatalf("the child's implementer completion = %d %s, want 200", status, body)
	}
	r.note("drove POST /legion/v1/handoff/complete for %s's implementer: %d", r.child, status)
	got := r.waitFor("the child past implementing", r.child, func(got api.Issue) bool { return got.Phase != phase.Implementing })
	if got.Phase != phase.Testing {
		t.Fatalf("%s is in %s after its implementer's completion, want testing", r.child, got.Phase)
	}
	r.note("read back: %s is in %s", r.child, got.Phase)
	parent := r.issue(r.parent)
	if parent.Phase != phase.AwaitingMerge {
		t.Fatalf("%s is in %s, want awaiting_merge while its child runs on", r.parent, parent.Phase)
	}
	r.note("read back: %s still waits in %s", r.parent, parent.Phase)
}

// Step 7. A pull request opens from the waiting root's branch: GitHub's opened, for a pull request
// of this run's own that is neither the root's nor the child's, from legion/<root>. GitHub never
// sends the root's merge again, so the opened must leave the merge the root waits on standing: the
// root stays in awaiting_merge on its merged pull request, and step 4 closes it on that merge.
// Before this change the opened replaced the merged pull request and the root never closed. The
// workflow only logs the opened it leaves, so the state shows nothing new to wait for; the step
// instead waits for the daemon's processed_events row for the envelope, which the intake writes in
// the transaction that applies the event, so the window that follows starts with the event applied.
func (r *mergedRootWaitsLive) openedLeavesTheMerge() {
	t := r.t
	eventID := r.publishOpened(r.parent, r.openedPR)
	r.note("drove a pull request opened from %s's branch: a notifications.github.%s.pr.%d envelope, action opened, head_ref legion/%s",
		r.parent, r.subjectRepo(), r.openedPR, r.parent)
	r.waitProcessed(eventID)
	r.note("read back: the daemon took the event — its processed_events row for github %s", eventID)
	deadline := time.Now().Add(liveSettle)
	for time.Now().Before(deadline) {
		got := r.issue(r.parent)
		if got.Phase != phase.AwaitingMerge || got.PullRequest == nil ||
			got.PullRequest.Number != r.parentPR || got.PullRequest.State != string(record.PullRequestMerged) {
			t.Fatalf("after the opened of #%d %s is in %s on pull request %+v, want it still awaiting its merge of #%d",
				r.openedPR, r.parent, got.Phase, got.PullRequest, r.parentPR)
		}
		time.Sleep(100 * time.Millisecond)
	}
	got := r.issue(r.parent)
	r.note("read back over %s: %s is in %s on pull request #%d, pullRequest.state %s — not #%d",
		liveSettle, r.parent, got.Phase, got.PullRequest.Number, got.PullRequest.State, r.openedPR)
}

// Step 5. The daemon is restarted at the waiting root's state. The boot's sweep of merged pull
// requests finds the root awaiting a merge GitHub already reported and leaves it there, and tells
// nobody: a restart must not repeat the architect's notice, and a root can wait across many boots.
func (r *mergedRootWaitsLive) restartLeavesTheWait() {
	t := r.t
	before := len(r.waitArchitectNotices(r.parent, "pr-merged", 1))
	r.restart()
	// The boot's work is under way: this boot relaunched the claim the fixture left unfinished,
	// which its reconciliation does. The window below then watches the wait, and step 4 reads the
	// notice count once more at the end of the run, past every retry a slow boot fact can take.
	r.awaitLaunch(r.token(r.child, claim.RoleImplementer))
	r.note("drove a restart of the daemon at the waiting root's state; boot %d, its reconciliation run", r.d.state().Daemon.Boots)
	deadline := time.Now().Add(liveSettle)
	for time.Now().Before(deadline) {
		if got := r.issue(r.parent); got.Phase != phase.AwaitingMerge || got.PullRequest.State != string(record.PullRequestMerged) {
			t.Fatalf("after the restart %s is in %s on a %s pull request, want it still awaiting its merge", r.parent, got.Phase, got.PullRequest.State)
		}
		if told := r.architectNotices(r.parent, "pr-merged"); len(told) != before {
			t.Fatalf("the boot told the architect %d pr-merged notices, want the %d the wait began with", len(told), before)
		}
		time.Sleep(100 * time.Millisecond)
	}
	got := r.issue(r.parent)
	r.note("read back over %s: %s is in %s, pullRequest.state %s, and the architect's pr-merged notices are still %d",
		liveSettle, r.parent, got.Phase, got.PullRequest.State, before)
}

// Step 4. The child's own pull request merges, which closes it and takes the last open member out
// of the tree: the root's close, waiting since step 2, runs as the merge's own close would have —
// done on Dispatch, the tree suspended by its linger, the slot freed and the production check open
// on the root's merged pull request, the one step 7's opened left standing.
func (r *mergedRootWaitsLive) childMergeClosesTheRoot() {
	t := r.t
	r.seedIssue("the child in awaiting_merge, as its testing, review, acceptance, retro and merger left it", r.child, func(issue *record.Issue) {
		issue.Phase, issue.Status = phase.AwaitingMerge, "retro"
	})
	suspendsBefore := len(r.rt.CallsOf("Suspend"))
	r.publishMerge(r.child, r.childPR, mergedRootWaitsChildMerge)
	r.note("drove the child's merge: a notifications.github.%s.pr.%d envelope, merged at %s",
		r.subjectRepo(), r.childPR, mergedRootWaitsChildMerge)

	child := r.waitFor("the child closed", r.child, func(got api.Issue) bool { return got.Phase == phase.Done })
	r.note("read back: %s is in %s, status %s", r.child, child.Phase, child.Status)
	parent := r.waitFor("the root closed behind it", r.parent, func(got api.Issue) bool {
		return got.Phase == phase.Done && got.Status == "done" && got.Slot == nil && got.Check != nil
	})
	r.note("read back: %s is in %s, status %s, no slot, production check open on #%d at %s",
		r.parent, parent.Phase, parent.Status, parent.Check.PullRequest, parent.Check.MergeCommit)
	if parent.Check.PullRequest != r.parentPR {
		t.Fatalf("%s's production check is on #%d, want its merged pull request #%d", r.parent, parent.Check.PullRequest, r.parentPR)
	}
	// The state carries no linger deadline, so the linger is read through what it does: it suspends
	// every claim of the tree, keeping each session, which the root's own architect shows.
	suspended := r.waitFor("the tree suspended by the root's linger", r.parent, func(got api.Issue) bool {
		return got.Architect != nil && got.Architect.State == "suspended"
	})
	r.note("read back: the root's architect is %s with its session %s kept — the linger's own doing",
		suspended.Architect.State, suspended.Architect.Session)
	suspends := r.rt.CallsOf("Suspend")
	if len(suspends) <= suspendsBefore {
		t.Fatalf("the close suspended %d claims, want the tree's", len(suspends)-suspendsBefore)
	}
	r.note("read back: the fake runtime was asked to suspend %d of the tree's processes", len(suspends)-suspendsBefore)
	if told := r.architectNotices(r.parent, "pr-merged"); len(told) != 1 {
		t.Fatalf("the architect was told %d pr-merged notices over the whole run, want the one the wait began with", len(told))
	}
	r.note("read back: one pr-merged notice over the whole run")
	testwait.Eventually(t, "the close's own done status on the Dispatch stand-in", func() bool { return r.board.wrote(r.parent, "done") })
	r.note("read back: the Dispatch stand-in took the close's done write for %s", r.parent)
}

// publishMerge publishes GitHub's merge of number on the tree's repository, as Envoy relays a
// GitHub pull-request event (packages/daemon/internal/intake/decode.go is the only producer of a
// merge fact from one).
func (r *mergedRootWaitsLive) publishMerge(issue string, number int, mergeSHA string) {
	r.t.Helper()
	payload, err := json.Marshal(map[string]string{
		"action": "closed", "kind": "pr", "repo": liveRepo, "number": fmt.Sprint(number),
		"merged": "true", "merge_commit_sha": mergeSHA, "head_sha": "head-" + fmt.Sprint(number),
		"base_ref": "main", "head_ref": "legion/" + issue, "title": "the merge the proof drives",
		"url":        "https://github.com/" + liveRepo + "/pull/" + fmt.Sprint(number),
		"updated_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		r.t.Fatalf("encode the merge payload: %v", err)
	}
	subject := "notifications.github." + r.subjectRepo() + ".pr." + fmt.Sprint(number)
	r.publish(subject, "github", fmt.Sprintf("pr-%d-merged", number), string(payload))
}

// publishOpened publishes GitHub's opened of number from issue's branch on the tree's repository,
// as Envoy relays a GitHub pull-request event (decode.go's decodePullRequest reads it), and
// returns the event id the daemon records it under.
func (r *mergedRootWaitsLive) publishOpened(issue string, number int) string {
	r.t.Helper()
	payload, err := json.Marshal(map[string]string{
		"action": "opened", "kind": "pr", "repo": liveRepo, "number": fmt.Sprint(number),
		"head_sha": "head-" + fmt.Sprint(number), "base_ref": "main", "head_ref": "legion/" + issue,
		"title":      "a pull request opened from the waiting root's branch",
		"url":        "https://github.com/" + liveRepo + "/pull/" + fmt.Sprint(number),
		"updated_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		r.t.Fatalf("encode the opened payload: %v", err)
	}
	subject := "notifications.github." + r.subjectRepo() + ".pr." + fmt.Sprint(number)
	return r.publish(subject, "github", fmt.Sprintf("pr-%d-opened", number), string(payload))
}

// publishChildPark publishes Dispatch's own issue.updated event moving the child to status, as a
// person's move from the Dispatch dashboard makes one, on the next sequence the issue has seen.
// Only the child is ever moved this way: the root's own moves are the daemon's.
func (r *mergedRootWaitsLive) publishChildPark(status string) {
	r.t.Helper()
	key := r.child
	// The event stands for a write already on Dispatch's board, so the board shows it first.
	r.board.show(key, status)
	r.seq++
	parent := any(r.parent)
	payload, err := json.Marshal(map[string]any{
		"id": r.seq, "issue_key": key, "project": r.cfg.Project, "seq": r.seq, "type": "issue.updated",
		"actor": map[string]string{"kind": "user", "id": "operator"}, "notify": true,
		"created_at": time.Now().UTC().Format(time.RFC3339Nano),
		"payload": map[string]any{
			"key": key, "status": status, "title": "the child beneath it", "parent": parent, "rank": "V",
			"labels": []string{}, "last_seq": r.seq, "project": r.cfg.Project, "resolved_repo": liveRepo,
		},
	})
	if err != nil {
		r.t.Fatalf("encode the Dispatch event: %v", err)
	}
	r.publish("notifications.dispatch.issue."+key+"."+"issue.updated", "dispatch", fmt.Sprintf("%s-%s-%d", key, status, r.seq), string(payload))
}
