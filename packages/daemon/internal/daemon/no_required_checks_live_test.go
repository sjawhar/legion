//go:build e2e

// The live proof's driver for the rule this change makes: a pull request whose base branch requires
// no check and no workflow has no CI verdict (classify.HeadVerdict's "", so no checksVerdict on GET
// /legion/v1/state), where it read green before, so a failed check there sends nothing back; and a
// code head pushed onto such a settled head leaves the attempt the fix-attempt count last counted,
// as a push onto green does (classify.AdvancePullRequestHead's nothingRequired), so a red once the
// base requires the check again is new and sends the work back. scripts/e2e/no-required-checks.sh
// builds this driver with `-tags e2e` and owns the nats-server and the PostgreSQL it runs against.
//
// The daemon is the whole daemon: run, the function `legion start` calls, through this package's
// own whole-daemon harness (startDaemonWithin), so the proof exercises the real configuration, the
// real store and its migrations, the real HTTP API, the real openWorkflow and its required-checks
// pass (watchRequiredChecks), the real JetStream consumers and the real outbox lanes. Facts enter
// only as production's do: GitHub envelopes on the ENVOY_NOTIFICATIONS stream for the settlements,
// the push and the synchronize, and the daemon's own reads of GitHub's REST API for what each base
// branch requires. Every assertion is a read of GET /legion/v1/state - raw, where the step holds
// that a key is absent - or of the notices the daemon published to the Envoy listener stand-in.
// The store is read only to know that the daemon took what a step drove, which no route shows: the
// settlement it recorded (checked_head), the required set its pass read (required), and the
// processed_events rows of the push and the synchronize.
//
// What the proof stands in for lies outside the rule. The broker is a native nats-server the script
// starts and names in LEGION_E2E_NATS_URL. GitHub is noRequiredChecksGitHub, a REST stand-in that
// answers every read the required-checks pass makes with a 2xx - a non-2xx would keep the set last
// read, so a switch would never reach the daemon - and whose base branches the driver switches
// between requiring nothing and requiring the check `ci`. Dispatch is the board the package's
// whole-daemon live proofs share (liveDispatch, in live_proof_test.go, the harness this driver
// shares with merged_root_waits_live_test.go), which takes the status writes and messages a
// send-back and a blocked count post. Envoy's listener is the package's noticeListener, and the two
// Apps' tokens are workflowTokenRecorder's placeholders. No agent runs: an implementer a send-back
// starts is launched by the package's fake runtime, which says it provisions each workspace in the
// claim's own pod, as the Agent Sandbox runtime does, so the daemon clones nothing from GitHub, and
// the launch never registers. The three issues and their pull requests are seeded through the
// store, as every daemon test seeds its fixture.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// noRequiredChecksCheck is the one check a base branch requires when it requires anything.
const noRequiredChecksCheck = "ci"

// noRequiredChecksRead bounds the wait for the daemon's read of a base branch the driver switched:
// the required-checks pass reads every open pull request at boot and every requiredChecksInterval
// after, so a switch reaches the store within one interval and a pass, and a minute more is room
// for a slow pass, not a second chance.
const noRequiredChecksRead = requiredChecksInterval + time.Minute

// noRequiredChecksLive is one run of the proof: the booted daemon, the stand-ins it reads and
// writes, and the three issues the steps drive.
type noRequiredChecksLive struct {
	liveProof
	github *noRequiredChecksGitHub
	// nothing is the issue on a base that requires nothing (steps 1 and 2), green the one on a base
	// that requires ci (step 3), and pushed the one at an exhausted fix-attempt count whose base
	// stops requiring anything under it (step 4). Each is a root of its own tree, and each pull
	// request is on a base branch of its own, so each has its own required set.
	nothing, green, pushed noRequiredChecksIssue
	// seq numbers the envelopes the driver publishes, so each event id is the run's own.
	seq int
}

// noRequiredChecksIssue is one issue the proof drives: its key, its pull request's number - the
// run's own, since the store a pod re-runs the proof against holds every earlier run's pull
// requests and a fact names a pull request by its repository and number - and its base branch.
type noRequiredChecksIssue struct {
	key    string
	number int
	base   string
}

// branch is the issue's pull request's head branch, as Legion names it.
func (i noRequiredChecksIssue) branch() string { return "legion/" + i.key }

// TestABaseThatRequiresNothingGivesNoVerdictLive drives the four steps of the proof against one
// booted daemon: a failed check on a base that requires nothing gives no verdict and sends nothing
// back; the same failure once the base requires the check is red and sends the work back, which is
// step 1's control that its absence came from the empty set and not from an unread set or a lost
// settlement; a passing required check is still green; and at an exhausted fix-attempt count, a
// push onto the counted head once its base requires nothing leaves the counted attempt, so the new
// head's red, once the base requires the check again, sends the work back.
func TestABaseThatRequiresNothingGivesNoVerdictLive(t *testing.T) {
	r := newNoRequiredChecksLive(t)
	r.step("nothing-required-failed-check", r.nothingRequiredFailedCheck)
	r.step("the-same-failure-once-required-is-red", r.theSameFailureOnceRequiredIsRed)
	r.step("a-passing-required-check-is-green", r.aPassingRequiredCheckIsGreen)
	r.step("a-push-onto-a-base-that-requires-nothing-leaves-the-attempt", r.aPushOntoABaseThatRequiresNothingLeavesTheAttempt)
}

// newNoRequiredChecksLive boots the daemon over the script's broker, with max_fix_attempts 1, and
// the three issues already recorded: the first in testing on a base that requires nothing, its
// required set not yet read; the second in reviewing on a base that requires ci; and the third in
// testing on a base that requires ci, its one fix attempt counted at its head.
func newNoRequiredChecksLive(t *testing.T) *noRequiredChecksLive {
	t.Helper()
	proof := newLiveProof(t, "scripts/e2e/no-required-checks.sh")
	// One counted attempt exhausts the count, so step 4's first red is held and reported, and its
	// last is held too unless the push onto the base that required nothing left that attempt.
	proof.cfg.MaxFixAttempts = 1
	number, err := strconv.ParseInt(randomSuffix(t)[:6], 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	r := &noRequiredChecksLive{
		liveProof: proof,
		nothing:   noRequiredChecksIssue{key: proof.cfg.Project + "-1", number: int(number) + 1, base: "base-nothing"},
		green:     noRequiredChecksIssue{key: proof.cfg.Project + "-2", number: int(number) + 2, base: "base-green"},
		pushed:    noRequiredChecksIssue{key: proof.cfg.Project + "-3", number: int(number) + 3, base: "base-pushed"},
	}
	r.github = newNoRequiredChecksGitHub(t)
	r.github.pull(r.nothing, "a1")
	r.github.pull(r.green, "c1", noRequiredChecksCheck)
	r.github.pull(r.pushed, "b1", noRequiredChecksCheck)
	r.showOnBoard()
	r.seed()
	// The runtime provisions each claim's workspace itself (fake.Runtime's InPod), so a send-back's
	// implementer start reaches the fake's launch without the daemon cloning acme/widgets from
	// github.com, which the tmux answer would have it try.
	rt := fake.NewRuntime()
	rt.InPod = true
	o := fakeRuntime(rt, &built{})
	o.workflowTokens = &workflowTokenRecorder{}
	o.githubAPI = r.github.url
	r.d = startDaemonWithin(t, r.cfg, o, r.log(), 60*time.Second)
	return r
}

// showOnBoard puts the three issues on the Dispatch stand-in at the statuses they are seeded with.
func (r *noRequiredChecksLive) showOnBoard() {
	r.board.show(r.nothing.key, "testing")
	r.board.show(r.green.key, "needs_review")
	r.board.show(r.pushed.key, "testing")
}

// seed records the three issues as the daemon would hold them: each a root handed to Legion,
// holding its tree's admission slot, its lifecycle open under the workflow, on an open pull request
// at its first head. The first's pull request has no required set yet, which the boot's pass reads
// as the empty set its base requires; the other two record the [ci] their bases require, and the
// third its one fix attempt, counted at that head and not yet reported.
func (r *noRequiredChecksLive) seed() {
	t := r.t
	t.Helper()
	ctx := context.Background()
	issues := []struct {
		issue  noRequiredChecksIssue
		phase  phase.Phase
		status string
		rank   string
		pr     record.PullRequest
	}{
		{r.nothing, phase.Testing, "testing", "U", record.PullRequest{HeadSHA: "a1"}},
		{r.green, phase.Reviewing, "needs_review", "V", record.PullRequest{HeadSHA: "c1", Required: []string{noRequiredChecksCheck}}},
		{r.pushed, phase.Testing, "testing", "W", record.PullRequest{HeadSHA: "b1", Required: []string{noRequiredChecksCheck}, FixAttempts: 1, HeadCounted: "b1"}},
	}
	if err := r.st.Tx(ctx, func(tx pgx.Tx) error {
		for _, seeded := range issues {
			key := seeded.issue.key
			if err := r.records.PutIssue(ctx, tx, record.Issue{
				Key: key, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Tree: key, Project: r.cfg.Project,
				Title: "an issue on " + seeded.issue.base, Phase: seeded.phase, Generation: 1, Status: seeded.status, DispatchStatus: seeded.status,
				Rank: seeded.rank, LastDispatchSeq: 1, HandedOver: true,
			}); err != nil {
				return err
			}
			pr := seeded.pr
			pr.State, pr.Issue, pr.Repo, pr.Number, pr.Branch, pr.Failing = record.PullRequestOpen, key, liveRepo, seeded.issue.number, seeded.issue.branch(), []string{}
			if err := r.records.PutPullRequest(ctx, tx, pr); err != nil {
				return err
			}
		}
		return r.admit(ctx, tx, r.nothing.key, r.green.key, r.pushed.key)
	}); err != nil {
		t.Fatalf("seed the three issues: %v", err)
	}
	for _, seeded := range issues {
		if _, err := r.st.OpenTreeLifecycle(ctx, r.project, seeded.issue.key, treelifecycle.AuthorityWorkflow); err != nil {
			t.Fatalf("open the lifecycle of %s: %v", seeded.issue.key, err)
		}
		required := "unread"
		if seeded.pr.Required != nil {
			required = fmt.Sprint(seeded.pr.Required)
		}
		r.note("seeded: %s in %s on pull request #%d at %s, base %s, required set %s, fix attempts %d",
			seeded.issue.key, seeded.phase, seeded.issue.number, seeded.pr.HeadSHA, seeded.issue.base, required, seeded.pr.FixAttempts)
	}
}

// Step 1. The base requires nothing, and CI settles the head with ci failed: the head has no
// verdict, so the state's pull request carries no checksVerdict at all, the issue stays in testing,
// and its architect is told no checks-red. Before this change the empty set read green.
func (r *noRequiredChecksLive) nothingRequiredFailedCheck() {
	t := r.t
	issue := r.nothing
	r.publishSettlement(issue, "a1", true)
	r.note("drove CI's settlement of %s at a1 with %s failed: a notifications.github.%s.pr.%d.checks envelope", issue.key, noRequiredChecksCheck, r.subjectRepo(), issue.number)
	r.waitSettled(issue, "a1")
	r.waitRequired(issue, []string{})
	r.note("read back: the daemon recorded the settlement (checked_head a1) and its boot pass read %s as requiring nothing (required [])", issue.base)
	pullRequest := r.rawPullRequest(issue.key)
	if verdict, ok := pullRequest["checksVerdict"]; ok {
		t.Fatalf("issues[%s].pullRequest carries checksVerdict %s on a base that requires nothing, want no checksVerdict key: %s", issue.key, verdict, r.rawJSON(pullRequest))
	}
	r.note("read back: issues[%s].pullRequest in GET /legion/v1/state has no checksVerdict key: %s", issue.key, r.rawJSON(pullRequest))
	r.holdsWithoutNotice(issue, phase.Testing, "checks-red")
	r.note("read back over %s: %s still in testing, and no checks-red notice on its architect's topic", liveSettle, issue.key)
}

// Step 2. The same base now requires ci. The daemon's next pass reads it, and the same settlement,
// still standing for the head, is red on the check it names: the state shows checksVerdict red, the
// work goes back to implementing, and the architect is told once by a checks-red naming ci. This
// is step 1's control: its absent verdict was the empty set's, not a set left unread or a
// settlement the daemon never took.
func (r *noRequiredChecksLive) theSameFailureOnceRequiredIsRed() {
	t := r.t
	issue := r.nothing
	r.github.require(issue.base, noRequiredChecksCheck)
	r.note("switched the GitHub stand-in: %s's rulesets now require %s", issue.base, noRequiredChecksCheck)
	r.waitRequired(issue, []string{noRequiredChecksCheck})
	r.note("read back: the daemon's pass read %s as requiring [%s]", issue.base, noRequiredChecksCheck)
	got := r.issue(issue.key)
	if got.PullRequest == nil || got.PullRequest.ChecksVerdict != "red" {
		t.Fatalf("issues[%s].pullRequest is %+v once its base requires %s, want checksVerdict red", issue.key, got.PullRequest, noRequiredChecksCheck)
	}
	if got.Phase != phase.Implementing {
		t.Fatalf("%s is in %s on the red at a1, want implementing", issue.key, got.Phase)
	}
	r.note("read back: checksVerdict %s, %s in %s", got.PullRequest.ChecksVerdict, issue.key, got.Phase)
	told := r.waitArchitectNotices(issue.key, "checks-red", 1)
	r.wantRedAt(issue, told[0], "a1")
	r.note("read back: one checks-red notice on %s's architect's topic: %s", issue.key, told[0].Payload)
}

// Step 3. A base that requires ci, and CI settles the head with ci passed: green still means every
// required check passed, so the state shows checksVerdict green and the issue stays in reviewing.
func (r *noRequiredChecksLive) aPassingRequiredCheckIsGreen() {
	t := r.t
	issue := r.green
	r.publishSettlement(issue, "c1", false)
	r.note("drove CI's settlement of %s at c1 with %s passed", issue.key, noRequiredChecksCheck)
	r.waitSettled(issue, "c1")
	r.note("read back: the daemon recorded the settlement (checked_head c1)")
	got := r.issue(issue.key)
	if got.PullRequest == nil || got.PullRequest.ChecksVerdict != "green" {
		t.Fatalf("issues[%s].pullRequest is %+v after %s passed on a base requiring it, want checksVerdict green", issue.key, got.PullRequest, noRequiredChecksCheck)
	}
	if got.Phase != phase.Reviewing {
		t.Fatalf("%s is in %s after a green settlement, want reviewing", issue.key, got.Phase)
	}
	r.note("read back: checksVerdict %s, %s in %s", got.PullRequest.ChecksVerdict, issue.key, got.Phase)
}

// Step 4. At an exhausted count (max_fix_attempts 1, one attempt counted at b1), the counted head's
// red is held: the issue stays in testing and the count is reported once. The base then stops
// requiring anything, so b1 has no verdict, and the implementer pushes b2 onto it: the push raises
// no count and leaves the counted attempt, as a push onto green does. Once the base requires ci
// again, b2's red is new, not the held attempt's: it sends the work back with a checks-red naming
// ci, and the count is still 1. Had HeadVerdict's change dropped this reset (AdvancePullRequestHead
// without nothingRequired), the push would have stayed in the attempt, and b2's red would have been
// held with the issue left in testing.
func (r *noRequiredChecksLive) aPushOntoABaseThatRequiresNothingLeavesTheAttempt() {
	t := r.t
	issue := r.pushed
	r.publishSettlement(issue, "b1", true)
	r.note("drove CI's settlement of %s at b1 with %s failed", issue.key, noRequiredChecksCheck)
	r.waitSettled(issue, "b1")
	got := r.issue(issue.key)
	if got.Phase != phase.Testing || got.PullRequest == nil || got.PullRequest.ChecksVerdict != "red" || got.PullRequest.FixAttempts != 1 {
		t.Fatalf("after the red at the counted head b1 %s is in %s on %+v, want testing, checksVerdict red, fixAttempts 1: the cap holds the attempt's red", issue.key, got.Phase, got.PullRequest)
	}
	blocked := r.waitArchitectNotices(issue.key, "pr-blocked", 1)
	r.note("read back: %s held in %s, checksVerdict %s, fixAttempts %d; one pr-blocked notice: %s",
		issue.key, got.Phase, got.PullRequest.ChecksVerdict, got.PullRequest.FixAttempts, blocked[0].Payload)
	if red := r.architectNotices(issue.key, "checks-red"); len(red) != 0 {
		t.Fatalf("the held red told %s's architect %d checks-red notices, want none", issue.key, len(red))
	}

	r.github.require(issue.base)
	r.note("switched the GitHub stand-in: %s's rulesets now require nothing", issue.base)
	r.waitRequired(issue, []string{})
	r.note("read back: the daemon's pass read %s as requiring nothing (required [])", issue.base)
	pullRequest := r.rawPullRequest(issue.key)
	if verdict, ok := pullRequest["checksVerdict"]; ok {
		t.Fatalf("issues[%s].pullRequest carries checksVerdict %s once its base requires nothing, want no checksVerdict key: %s", issue.key, verdict, r.rawJSON(pullRequest))
	}
	if got := r.issue(issue.key); got.Phase != phase.Testing {
		t.Fatalf("%s is in %s once its base requires nothing, want testing", issue.key, got.Phase)
	}
	r.note("read back: issues[%s].pullRequest has no checksVerdict key, %s still in testing: %s", issue.key, issue.key, r.rawJSON(pullRequest))

	r.github.head(issue, "b2")
	pushed := r.publishPush(issue, "b1", "b2")
	synchronized := r.publishSynchronize(issue, "b2")
	r.note("drove the implementer's push of b2 onto b1 (src/widget.go, pusher legion-implement[bot]) and GitHub's synchronize of #%d to b2", issue.number)
	r.waitProcessed(pushed)
	r.waitProcessed(synchronized)
	got = r.issue(issue.key)
	if got.PullRequest == nil || got.PullRequest.Head != "b2" || got.PullRequest.FixAttempts != 1 {
		t.Fatalf("after the push of b2 issues[%s].pullRequest is %+v, want head b2 and fixAttempts still 1: a push onto a head with no verdict counts nothing", issue.key, got.PullRequest)
	}
	r.note("read back: the daemon took both envelopes (processed_events); head %s, fixAttempts %d", got.PullRequest.Head, got.PullRequest.FixAttempts)

	r.github.require(issue.base, noRequiredChecksCheck)
	r.note("switched the GitHub stand-in: %s's rulesets require %s again", issue.base, noRequiredChecksCheck)
	r.waitRequired(issue, []string{noRequiredChecksCheck})
	r.note("read back: the daemon's pass read %s as requiring [%s]", issue.base, noRequiredChecksCheck)

	r.publishSettlement(issue, "b2", true)
	r.note("drove CI's settlement of %s at b2 with %s failed", issue.key, noRequiredChecksCheck)
	r.waitSettled(issue, "b2")
	got = r.issue(issue.key)
	if got.Phase != phase.Implementing {
		t.Fatalf("after the red at b2 %s is in %s on %+v, want implementing: the push onto b1 while its base required nothing must leave the counted attempt, so b2's red is new", issue.key, got.Phase, got.PullRequest)
	}
	if got.PullRequest == nil || got.PullRequest.ChecksVerdict != "red" || got.PullRequest.FixAttempts != 1 {
		t.Fatalf("after the red at b2 issues[%s].pullRequest is %+v, want checksVerdict red and fixAttempts 1", issue.key, got.PullRequest)
	}
	r.note("read back: %s in %s, checksVerdict %s, fixAttempts %d", issue.key, got.Phase, got.PullRequest.ChecksVerdict, got.PullRequest.FixAttempts)
	told := r.waitArchitectNotices(issue.key, "checks-red", 1)
	r.wantRedAt(issue, told[0], "b2")
	if blocked := r.architectNotices(issue.key, "pr-blocked"); len(blocked) != 1 {
		t.Fatalf("%s's architect was told %d pr-blocked notices over the step, want the one the held red reported", issue.key, len(blocked))
	}
	r.note("read back: one checks-red notice on %s's architect's topic: %s; still the one pr-blocked", issue.key, told[0].Payload)
}

// rawPullRequest is issues[key].pullRequest as GET /legion/v1/state sends it, each key as it is on
// the wire, so a step can hold that a key is absent rather than decoded to its zero value.
func (r *noRequiredChecksLive) rawPullRequest(key string) map[string]json.RawMessage {
	t := r.t
	t.Helper()
	state := bootState(t, r.d)
	var issues map[string]struct {
		PullRequest map[string]json.RawMessage `json:"pullRequest"`
	}
	if raw, ok := state["issues"]; ok {
		if err := json.Unmarshal(raw, &issues); err != nil {
			t.Fatalf("decode the state's issues %s: %v", raw, err)
		}
	}
	issue, ok := issues[key]
	if !ok || issue.PullRequest == nil {
		t.Fatalf("the state shows no pull request for %s: %s", key, r.rawJSON(state))
	}
	return issue.PullRequest
}

// rawJSON is value encoded back to the JSON the state sent, for a step's note or failure.
func (r *noRequiredChecksLive) rawJSON(value map[string]json.RawMessage) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		r.t.Fatalf("encode %v: %v", value, err)
	}
	return string(encoded)
}

// stored is issue's pull request as the daemon's store holds it now.
func (r *noRequiredChecksLive) stored(issue noRequiredChecksIssue) record.PullRequest {
	r.t.Helper()
	ctx := context.Background()
	var pr *record.PullRequest
	if err := r.st.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		pr, err = r.records.PullRequest(ctx, tx, issue.key)
		return err
	}); err != nil {
		r.t.Fatalf("read the pull request of %s: %v", issue.key, err)
	}
	if pr == nil {
		r.t.Fatalf("the store holds no pull request for %s", issue.key)
	}
	return *pr
}

// waitSettled waits for the daemon's record that it took CI's settlement of head on issue's pull
// request: the settlement and the decision it makes commit together, so what the state shows next
// is that decision.
func (r *noRequiredChecksLive) waitSettled(issue noRequiredChecksIssue, head string) {
	r.t.Helper()
	testwait.Eventually(r.t, fmt.Sprintf("the daemon's record of the settlement of %s at %s", issue.key, head), func() bool {
		return r.stored(issue).CheckedHead == head
	})
}

// waitRequired waits for the daemon's required-checks pass to record want as the set issue's base
// requires, polling the store for up to noRequiredChecksRead, past testwait's minute: the pass
// reads at most every requiredChecksInterval. A read and the decision it makes commit together.
func (r *noRequiredChecksLive) waitRequired(issue noRequiredChecksIssue, want []string) {
	r.t.Helper()
	deadline := time.Now().Add(noRequiredChecksRead)
	var last record.PullRequest
	for time.Now().Before(deadline) {
		last = r.stored(issue)
		if last.Required != nil && slices.Equal(last.Required, want) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	r.t.Fatalf("the daemon's pass did not record %s as requiring %v within %s: the store holds %v (nil is a set never read); GitHub stand-in reads it left unanswered: %v",
		issue.base, want, noRequiredChecksRead, last.Required, r.github.unanswered())
}

// holdsWithoutNotice watches issue for liveSettle, failing if it leaves want or its
// architect is told a notice of kind.
func (r *noRequiredChecksLive) holdsWithoutNotice(issue noRequiredChecksIssue, want phase.Phase, kind string) {
	r.t.Helper()
	deadline := time.Now().Add(liveSettle)
	for time.Now().Before(deadline) {
		if got := r.issue(issue.key); got.Phase != want {
			r.t.Fatalf("%s is in %s, want %s", issue.key, got.Phase, want)
		}
		if told := r.architectNotices(issue.key, kind); len(told) != 0 {
			r.t.Fatalf("%s's architect was told %d %s notices, want none: %s", issue.key, len(told), kind, told[0].Payload)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// wantRedAt fails unless the checks-red notice told names head and the required check, as the
// workflow writes its reason: "CI is red at <head>: <check>, ...".
func (r *noRequiredChecksLive) wantRedAt(issue noRequiredChecksIssue, told listenerPublish, head string) {
	r.t.Helper()
	if want := "CI is red at " + head + ": " + noRequiredChecksCheck; told.Payload != want {
		r.t.Fatalf("%s's checks-red notice reads %q, want %q", issue.key, told.Payload, want)
	}
}

// publishSettlement publishes CI's settlement of head on issue's pull request with the one check
// ci, failed or passed, as Envoy relays one (intake's decodeChecks reads it).
func (r *noRequiredChecksLive) publishSettlement(issue noRequiredChecksIssue, head string, failed bool) {
	r.t.Helper()
	group := func(checks ...string) map[string]any {
		return map[string]any{"count": len(checks), "checks": append([]string{}, checks...)}
	}
	failedGroup, passedGroup := group(), group(noRequiredChecksCheck)
	if failed {
		failedGroup, passedGroup = passedGroup, failedGroup
	}
	r.seq++
	payload, err := json.Marshal(map[string]any{
		"kind": "checks", "repo": liveRepo, "number": strconv.Itoa(issue.number), "sha": head,
		"check_runs": []map[string]any{{"name": noRequiredChecksCheck, "id": r.seq}}, "generation": 1,
		"snapshot": fmt.Sprintf("%s-%s-%d", issue.key, head, r.seq), "settled_at": time.Now().UnixMilli(),
		"failed": failedGroup, "passed": passedGroup, "running": group(), "queued": group(), "cancelled": group(), "skipped": group(),
	})
	if err != nil {
		r.t.Fatalf("encode the settlement payload: %v", err)
	}
	subject := fmt.Sprintf("notifications.github.%s.pr.%d.checks", r.subjectRepo(), issue.number)
	r.publish(subject, "github", fmt.Sprintf("checks-%d-%s-%d", issue.number, head, r.seq), string(payload))
}

// publishPush publishes the implementer's push of to onto from on issue's branch, changing code, as
// Envoy relays a GitHub push (intake's decodePush reads it), and returns its event id.
func (r *noRequiredChecksLive) publishPush(issue noRequiredChecksIssue, from, to string) string {
	r.t.Helper()
	payload, err := json.Marshal(map[string]string{
		"kind": "push", "repo": liveRepo, "ref": "refs/heads/" + issue.branch(), "before": from, "after": to,
		"changed_paths": "src/widget.go", "changed_paths_truncated": "false", "forced": "false",
		"pusher": "legion-implement[bot]", "commit_count": "1", "head_subject": "fix the widget",
	})
	if err != nil {
		r.t.Fatalf("encode the push payload: %v", err)
	}
	subject := fmt.Sprintf("notifications.github.%s.push.branch.%s", r.subjectRepo(), issue.branch())
	return r.publish(subject, "github", fmt.Sprintf("push-%d-%s", issue.number, to), string(payload))
}

// publishSynchronize publishes GitHub's synchronize of issue's pull request to head, as Envoy relays
// a GitHub pull-request event (intake's decodePullRequest reads it), and returns its event id.
func (r *noRequiredChecksLive) publishSynchronize(issue noRequiredChecksIssue, head string) string {
	r.t.Helper()
	payload, err := json.Marshal(map[string]string{
		"action": "synchronize", "kind": "pr", "repo": liveRepo, "number": strconv.Itoa(issue.number),
		"head_sha": head, "base_ref": issue.base, "head_ref": issue.branch(), "title": "an issue on " + issue.base,
		"url":        "https://github.com/" + liveRepo + "/pull/" + strconv.Itoa(issue.number),
		"updated_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		r.t.Fatalf("encode the synchronize payload: %v", err)
	}
	subject := fmt.Sprintf("notifications.github.%s.pr.%d", r.subjectRepo(), issue.number)
	return r.publish(subject, "github", fmt.Sprintf("pr-%d-synchronize-%s", issue.number, head), string(payload))
}

// noRequiredChecksGitHub is GitHub's REST API for acme/widgets as this proof's daemon reads it,
// answering every read of the required-checks pass (readRequiredChecks) with a 2xx: each pull
// request with its base, head and `mergeable: true` (readPullRequest), its reviews as none
// (readReviews), each base branch's rulesets as the driver last set them (requiredchecks.Required's
// /rules/branches/<base>) and the branch itself as unprotected (its /branches/<base>, which a 404
// would make an error that keeps the set last read). It also answers what a launch and a disarm
// read: the repository, with default branch main, and a GraphQL root that knows no pull request.
// Anything else is answered 404 and kept, for a failure to name, and nothing reaches github.com.
type noRequiredChecksGitHub struct {
	t   *testing.T
	url string
	mu  sync.Mutex
	// pulls is each pull request by number; requires is the check each base branch's rulesets
	// require, none for a base that requires nothing.
	pulls    map[int]noRequiredChecksPull
	requires map[string][]string
	missed   []string
}

// noRequiredChecksPull is one pull request as the stand-in answers it.
type noRequiredChecksPull struct {
	issue noRequiredChecksIssue
	head  string
}

func newNoRequiredChecksGitHub(t *testing.T) *noRequiredChecksGitHub {
	t.Helper()
	g := &noRequiredChecksGitHub{t: t, pulls: map[int]noRequiredChecksPull{}, requires: map[string][]string{}}
	server := httptest.NewServer(g)
	t.Cleanup(server.Close)
	g.url = server.URL
	return g
}

// pull answers issue's pull request at head, its base requiring checks.
func (g *noRequiredChecksGitHub) pull(issue noRequiredChecksIssue, head string, checks ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pulls[issue.number] = noRequiredChecksPull{issue: issue, head: head}
	g.requires[issue.base] = checks
}

// head moves issue's pull request to head, as a push does on GitHub.
func (g *noRequiredChecksGitHub) head(issue noRequiredChecksIssue, head string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	pull := g.pulls[issue.number]
	pull.head = head
	g.pulls[issue.number] = pull
}

// require switches base's rulesets to require checks, and nothing when none is named.
func (g *noRequiredChecksGitHub) require(base string, checks ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requires[base] = checks
}

// unanswered is every request the stand-in answered 404, oldest first.
func (g *noRequiredChecksGitHub) unanswered() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.missed...)
}

func (g *noRequiredChecksGitHub) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if req.Method == http.MethodPost && req.URL.Path == "/graphql" {
		_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequest":null}}}`)
		return
	}
	rest, ours := strings.CutPrefix(req.URL.Path, "/repos/"+liveRepo)
	if req.Method == http.MethodGet && ours {
		if g.answer(w, rest) {
			return
		}
	}
	g.missed = append(g.missed, req.Method+" "+req.URL.Path)
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, `{"message":"Not Found"}`)
}

// answer writes the stand-in's answer to a GET of /repos/acme/widgets<rest>, and says whether it
// had one.
func (g *noRequiredChecksGitHub) answer(w http.ResponseWriter, rest string) bool {
	if rest == "" {
		g.write(w, map[string]any{"full_name": liveRepo, "default_branch": "main"})
		return true
	}
	if base, ok := strings.CutPrefix(rest, "/rules/branches/"); ok {
		checks, known := g.requires[base]
		if !known {
			return false
		}
		rules := []map[string]any{{"type": "pull_request"}}
		if len(checks) > 0 {
			contexts := []map[string]string{}
			for _, check := range checks {
				contexts = append(contexts, map[string]string{"context": check})
			}
			rules = append(rules, map[string]any{"type": "required_status_checks", "parameters": map[string]any{"required_status_checks": contexts}})
		}
		g.write(w, rules)
		return true
	}
	if base, ok := strings.CutPrefix(rest, "/branches/"); ok {
		if _, known := g.requires[base]; !known {
			return false
		}
		g.write(w, map[string]any{"name": base, "protected": false})
		return true
	}
	pulled, ok := strings.CutPrefix(rest, "/pulls/")
	if !ok {
		return false
	}
	number, reviews := strings.CutSuffix(pulled, "/reviews")
	n, err := strconv.Atoi(number)
	if err != nil {
		return false
	}
	pull, known := g.pulls[n]
	if !known {
		return false
	}
	if reviews {
		g.write(w, []any{})
		return true
	}
	g.write(w, map[string]any{
		"number": n, "state": "open", "mergeable": true,
		"base": map[string]string{"ref": pull.issue.base},
		"head": map[string]string{"ref": pull.issue.branch(), "sha": pull.head},
	})
	return true
}

// write encodes value as the answer.
func (g *noRequiredChecksGitHub) write(w http.ResponseWriter, value any) {
	if err := json.NewEncoder(w).Encode(value); err != nil {
		g.t.Errorf("write the GitHub stand-in's answer: %v", err)
	}
}
