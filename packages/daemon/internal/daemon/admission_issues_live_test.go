// The live proof's driver for dispatch://INFRA-980's acceptance line A8: the admission cap counts
// the issues at work rather than the trees, and a parent's own phases come after its children's.
// It boots the whole daemon on a real Postgres, drives one root with two children and one second
// tree through the daemon's own intake and routes, and reads GET /legion/v1/state at each step.
//
// The daemon is the whole daemon: run, the function `legion start` calls, through this package's
// own whole-daemon harness (startDaemonWithin), so the proof exercises the real configuration
// load, the real store and its migrations, the real HTTP API, the real claim supervision, the real
// openWorkflow, the real JetStream consumers and the real outbox lanes. Facts enter only by the
// transports production has — the GitHub envelopes of the ENVOY_NOTIFICATIONS stream and the
// daemon's own HTTP routes, called as the tree's architect with a capability grant its session
// bought — and every step is judged by what GET /legion/v1/state answers, with the Dispatch
// stand-in's record of what the daemon wrote to it and the architect's own notices beside it.
// Nothing reads the store to judge a step, and nothing calls into the workflow package.
//
// Unlike this package's other live proofs it carries no `e2e` build tag and has no script of its
// own: it runs in the ordinary `go test ./internal/daemon/`, over the broker that package's other
// tests take (newLiveProofOn over workflowNATS), which is where the plan this work follows puts
// A8. Everything else is the shared harness: Dispatch is liveProof's board (liveDispatch), Envoy's
// listener its noticeListener, GitHub the stand-in below, and no agent runs — every claim is
// launched by the package's fake runtime and reached over the daemon's own routes. The records
// that put the two trees at the first step are seeded through the store, as every daemon test
// seeds its fixture; what is driven is the change.
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
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

const (
	// admissionIssuesCap is the cap this proof runs under, in issues: the two children of one tree
	// fill it between them, which is the count that used to be one tree's single slot.
	admissionIssuesCap = 2
	// admissionIssuesCheck is the one required check of the second tree's pull request, which goes
	// red under it.
	admissionIssuesCheck = "ci"
	// admissionIssuesHead is that pull request's head, the commit the red settles on.
	admissionIssuesHead = "head-accepting"
)

// admissionIssuesLive is one run of the proof: the booted daemon, the keys of the two trees it
// drives, the architect session of the tree whose children it releases and parks, and the
// stand-ins the steps read the daemon's own writes back from.
type admissionIssuesLive struct {
	liveProof
	rt    *fake.Runtime
	built *built
	// root is the parent whose phases wait for its children; childOne and childTwo are the two
	// children Dispatch shows beneath it, filed and not yet released.
	root, childOne, childTwo string
	// accepting is the second tree's root, in accepting on an open pull request: an issue in the
	// workflow that holds no slot, and the one the red head returns to implementing.
	accepting   string
	acceptingPR int
	// architect is the root's architect session, registered once over the daemon's own routes.
	architect *admissionIssuesArchitect
	// seq numbers the check settlements this run publishes, so each carries a snapshot of its own.
	seq int
}

// admissionIssuesArchitect is the root's architect session as its agent holds it: the session its
// registration named and the capability secret that registration returned, which buys its grants.
type admissionIssuesArchitect struct {
	session string
	secret  string
}

// TestTheCapCountsIssuesAndAParentWaitsForItsChildrenLive drives the four steps of the proof
// against one booted daemon, in the order each step's state follows from the last: two live trees
// holding no slot at all, the two released children holding both slots of a cap that counts
// issues while their parent waits, a red head that returns the second tree's root to implementing
// and leaves its start waiting for a slot the full cap has none of, and the parking of the two
// children, which frees the slots that return and that parent's first phase then take.
func TestTheCapCountsIssuesAndAParentWaitsForItsChildrenLive(t *testing.T) {
	r := newAdmissionIssuesLive(t)
	r.step("step1-two-live-trees-hold-no-slot", r.liveTreesHoldNoSlot)
	r.step("step2-the-released-children-hold-both-slots-and-their-parent-waits", r.childrenHoldTheSlots)
	r.step("step3-a-red-head-returns-a-waiting-issue-and-its-start-waits-for-a-slot", r.redHeadReturnsAWaitingIssue)
	r.step("step4-the-parked-children-free-the-slots-the-return-and-the-parent-take", r.freedSlotsGoToTheReturnAndTheParent)
}

// newAdmissionIssuesLive boots the daemon over the package's broker with the two trees already
// recorded, and registers the root's architect.
func newAdmissionIssuesLive(t *testing.T) *admissionIssuesLive {
	t.Helper()
	proof := newLiveProofOn(t, workflowNATS(t))
	proof.cfg.AdmissionCap = admissionIssuesCap
	number, err := strconv.ParseInt(randomSuffix(t)[:6], 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	rt := fake.NewRuntime()
	// The runtime provisions each claim's workspace itself (fake.Runtime's InPod), so a launch
	// reaches the fake without the daemon cloning acme/widgets from github.com, which the tmux
	// answer would have it try and which no machine can do: that repository does not exist.
	rt.InPod = true
	r := &admissionIssuesLive{
		liveProof: proof, rt: rt, built: &built{},
		root: proof.cfg.Project + "-1", childOne: proof.cfg.Project + "-2", childTwo: proof.cfg.Project + "-3",
		accepting: proof.cfg.Project + "-4", acceptingPR: int(number) + 1,
	}
	r.seedTrees()
	o := fakeRuntime(r.rt, r.built)
	o.workflowTokens = &workflowTokenRecorder{}
	o.githubAPI = admissionIssuesGitHub(t)
	r.d = startDaemonWithin(t, r.cfg, o, r.log(), 60*time.Second)
	r.architect = r.registerArchitect()
	return r
}

// admissionIssuesGitHub is GitHub as this proof's daemon reads it: every repository has default
// branch main at one commit, each issue branch the workflow creates there is taken, and its
// GraphQL root knows no pull request, so each disarm the workflow queues finishes with nothing
// armed to take back. Nothing else is answered, and nothing reaches github.com.
func admissionIssuesGitHub(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest, isRepository := strings.CutPrefix(r.URL.Path, "/repos/")
		parts := strings.Split(rest, "/")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequest":null}}}`)
		case r.Method == http.MethodGet && isRepository && len(parts) == 2:
			_, _ = io.WriteString(w, `{"full_name":"`+parts[0]+"/"+parts[1]+`","default_branch":"main"}`)
		case r.Method == http.MethodGet && isRepository && strings.HasSuffix(rest, "/git/ref/heads/main"):
			_, _ = io.WriteString(w, `{"ref":"refs/heads/main","object":{"sha":"base-main"}}`)
		case r.Method == http.MethodPost && isRepository && strings.HasSuffix(rest, "/git/refs"):
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// seedTrees records the two trees as the daemon would hold them at the moment this proof begins,
// and neither holds a slot: the root at admitted with its design gate approved, so its architect's
// spec is written and its own first phase is the next thing due, and two children filed beneath it
// in Dispatch that no release has run yet; and the second tree's root in accepting on its
// reviewed, open pull request, whose base requires one check. Each tree's lifecycle is open under
// the workflow, and each architect's claim is a launch a previous daemon began and never recorded,
// which this boot relaunches — which is how the proof reaches a live architect session without the
// operator's spawn, which would take the tree's lifecycle from the workflow.
func (r *admissionIssuesLive) seedTrees() {
	t := r.t
	t.Helper()
	ctx := context.Background()
	r.board.show(r.root, "in_progress")
	r.board.show(r.accepting, "needs_review")
	for _, child := range []string{r.childOne, r.childTwo} {
		r.board.place(child, "backlog", r.root, false)
	}
	approved := 1
	r.write("the two trees", func(tx pgx.Tx) error {
		if err := r.records.PutIssue(ctx, tx, record.Issue{
			Key: r.root, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Tree: r.root, Project: r.cfg.Project,
			Title: "the parent whose children go first", Phase: phase.Admitted, Generation: 1, Status: "in_progress",
			DispatchStatus: "in_progress", Rank: "A1", LastDispatchSeq: 1, HandedOver: true,
		}); err != nil {
			return err
		}
		// EverOpened is what tells a root whose architect is still writing its spec, which holds a
		// slot, from one whose gate has opened and whose children now work, which holds none.
		if err := r.records.PutGate(ctx, tx, record.DesignGate{
			Issue: r.root, ArtifactID: "spec-" + r.root, LatestVersion: 1, ApprovedVersion: &approved, EverOpened: true,
		}); err != nil {
			return err
		}
		if err := r.records.PutIssue(ctx, tx, record.Issue{
			Key: r.accepting, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Tree: r.accepting, Project: r.cfg.Project,
			Title: "the second tree, accepting", Phase: phase.Accepting, Generation: 1, Status: "needs_review",
			DispatchStatus: "needs_review", Rank: "A2", LastDispatchSeq: 1, HandedOver: true,
		}); err != nil {
			return err
		}
		if err := r.records.PutPullRequest(ctx, tx, record.PullRequest{
			State: record.PullRequestOpen, Issue: r.accepting, Repo: liveRepo, Number: r.acceptingPR,
			Branch: "legion/" + r.accepting, HeadSHA: admissionIssuesHead, CheckedHead: admissionIssuesHead,
			Required: []string{admissionIssuesCheck}, CheckRuns: []record.AttemptRun{{Name: admissionIssuesCheck, ID: 1}}, Failing: []string{},
		}); err != nil {
			return err
		}
		if err := r.records.PutPhase(ctx, tx, record.PhaseRow{
			Issue: r.accepting, Role: claim.RoleImplementer, Claim: "seeded-implementer", HandoffCommit: "implement-1",
		}); err != nil {
			return err
		}
		return r.records.PutPhase(ctx, tx, record.PhaseRow{
			Issue: r.accepting, Role: claim.RoleReviewer, Claim: "seeded-reviewer", HandoffRecord: "review-1", HandoffVersion: 1,
			Summary: "reviewed", Decision: &record.ReviewDecision{State: "approved", Head: admissionIssuesHead},
		})
	})
	// The mirror of Dispatch's board, as the boot listing of the project leaves it: the daemon
	// reads a parent's children from the mirror rather than from Dispatch, so the two filed
	// children have a row each naming the parent whose first phase they hold.
	r.write("the Dispatch mirror of the board", func(tx pgx.Tx) error {
		for _, row := range []record.DispatchIssueRow{
			{Key: r.root, Project: r.cfg.Project, Status: "in_progress", Seq: 1},
			{Key: r.accepting, Project: r.cfg.Project, Status: "needs_review", Seq: 1},
			{Key: r.childOne, Project: r.cfg.Project, Status: "backlog", Parent: r.root, Seq: 1},
			{Key: r.childTwo, Project: r.cfg.Project, Status: "backlog", Parent: r.root, Seq: 1},
		} {
			if err := r.records.PutDispatchIssue(ctx, tx, row); err != nil {
				return err
			}
		}
		return nil
	})
	for _, key := range []string{r.root, r.accepting} {
		if _, err := r.st.OpenTreeLifecycle(ctx, r.project, key, treelifecycle.AuthorityWorkflow); err != nil {
			t.Fatalf("open the lifecycle of %s: %v", key, err)
		}
		architect := r.token(key, claim.RoleArchitect)
		if _, err := r.st.AdmitClaim(ctx, supervise.Claim{
			Token: architect, Project: r.project, Tree: key, Issue: key, Role: claim.RoleArchitect,
			Generation: 1, State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)),
		}); err != nil {
			t.Fatalf("record the architect's unfinished launch on %s: %v", key, err)
		}
		writePrompt(t, r.cfg, architect)
	}
	r.note("seeded: %s at admitted with its gate approved and %s and %s filed beneath it in backlog; %s accepting on pull request #%d at %s, requiring [%s]; cap %d, no slot taken",
		r.root, r.childOne, r.childTwo, r.accepting, r.acceptingPR, admissionIssuesHead, admissionIssuesCheck, admissionIssuesCap)
}

// write runs body in one transaction against the daemon's own database, failing the test with what
// it was seeding on an error.
func (r *admissionIssuesLive) write(what string, body func(tx pgx.Tx) error) {
	r.t.Helper()
	if err := r.st.Tx(context.Background(), body); err != nil {
		r.t.Fatalf("seed %s: %v", what, err)
	}
}

// token is the claim token of role on issue, as the daemon spells it.
func (r *admissionIssuesLive) token(issue string, role claim.Role) claim.Token {
	r.t.Helper()
	token, err := claim.NewToken(r.project, issue, role)
	if err != nil {
		r.t.Fatal(err)
	}
	return token
}

// registerArchitect brings the root's relaunched architect claim to ready, as its pane's shim and
// its agent do: it waits for the launch, dials the worker stream with the launch's boot token,
// registers the session and reports it ready.
func (r *admissionIssuesLive) registerArchitect() *admissionIssuesArchitect {
	t := r.t
	t.Helper()
	token := r.token(r.root, claim.RoleArchitect)
	testwait.Eventually(t, "the launch of "+string(token), func() bool {
		return slices.ContainsFunc(r.rt.Calls(), func(call fake.Call) bool {
			return (call.Method == "Spawn" || call.Method == "Resume") && call.Spec.Claim == token
		})
	})
	launch := lastLaunch(t, r.rt, token)
	dialShim(t, r.built.address, launch.BootToken)
	const session = "ses_architect"
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
	return &admissionIssuesArchitect{session: session, secret: registered.Secret}
}

// grant is a capability grant minted for the root's architect now, as its `legion` tool mints one
// per operation.
func (r *admissionIssuesLive) grant() string {
	t := r.t
	t.Helper()
	status, body := r.d.request(http.MethodPost, "/legion/v1/grants",
		api.GrantRequest{SessionID: r.architect.session, Secret: r.architect.secret, Tree: r.root, Issue: r.root}, false)
	if status != http.StatusOK {
		t.Fatalf("the architect's grant = %d %s, want 200", status, body)
	}
	var granted api.GrantResponse
	if err := json.Unmarshal(body, &granted); err != nil {
		t.Fatalf("decode the architect's grant: %v", err)
	}
	return granted.GrantID
}

// quoted is GET /legion/v1/state now, with its `admission` member printed under the step as the
// route served it and one line per issue of the run naming the fields the step turns on. Every
// assertion of this proof reads the value it returns, so what the run prints is the answer the
// step was judged on.
func (r *admissionIssuesLive) quoted(what string) api.State {
	t := r.t
	t.Helper()
	status, body := r.d.request(http.MethodGet, "/legion/v1/state", nil, false)
	if status != http.StatusOK {
		t.Fatalf("GET the state = %d %s, want 200", status, body)
	}
	var served struct {
		Admission json.RawMessage `json:"admission"`
	}
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatalf("decode the state %s: %v", body, err)
	}
	var state api.State
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode the state %s: %v", body, err)
	}
	r.note("%s: admission %s", what, served.Admission)
	for _, key := range []string{r.root, r.childOne, r.childTwo, r.accepting} {
		issue, recorded := state.Issues[key]
		if !recorded {
			r.note("   issues.%s: the state records no such issue", key)
			continue
		}
		r.note("   issues.%s: phase %s, status %s, tree %s, slot %s", key, issue.Phase, issue.Status, issue.Tree, slotOf(issue))
	}
	return state
}

// slotOf is an issue's slot as a line of a step's output: its index and the moment it took it, or
// that it holds none.
func slotOf(issue api.Issue) string {
	if issue.Slot == nil {
		return "none"
	}
	return fmt.Sprintf("index %d taken %s", issue.Slot.Index, issue.Slot.AdmittedAt.UTC().Format(time.RFC3339))
}

// waitForState polls GET /legion/v1/state until want holds of it, then prints and returns that
// answer under the step's heading. A wait that gives up prints the answer it last had, since that
// is what the step was judged on either way.
func (r *admissionIssuesLive) waitForState(what string, want func(api.State) bool) api.State {
	r.t.Helper()
	held := false
	defer func() {
		if !held {
			r.quoted("the last answer before the wait for " + what + " gave up")
		}
	}()
	testwait.Eventually(r.t, what, func() bool { return want(r.d.state()) })
	held = true
	return r.quoted(what)
}

// holders is the issues `admission.active` names, sorted, which is the set the cap counts.
func holders(state api.State) []string {
	return slices.Sorted(slices.Values(state.Admission.Active))
}

// blockedBy is what `admission.blocked` says key waits on, nil when it names key at all.
func blockedBy(state api.State, key string) []string {
	for _, row := range state.Admission.Blocked {
		if row.Issue == key {
			return row.BlockedBy
		}
	}
	return nil
}

// Step 1. Two trees are live and neither holds a slot: the root's design gate has opened, so its
// architect's own turns take none, and the second tree's root is in accepting, where no worker
// runs. Before this change a slot was one row per root and both of these would hold one; now
// `admission.trees` is the only member of the state that names them, which is why a reader such as
// the controller reads the running trees there. The root's own first phase does not start, because
// Dispatch shows two children filed beneath it, and `admission.blocked` names them.
func (r *admissionIssuesLive) liveTreesHoldNoSlot() {
	t := r.t
	state := r.waitForState("the two live trees with no slot between them", func(state api.State) bool {
		return len(state.Admission.Trees) == 2 && len(state.Admission.Active) == 0 && blockedBy(state, r.root) != nil
	})
	if trees := state.Admission.Trees; !slices.Equal(trees, []string{r.root, r.accepting}) {
		t.Fatalf("admission.trees = %v, want the two live trees %s and %s", trees, r.root, r.accepting)
	}
	if holders := holders(state); len(holders) != 0 {
		t.Fatalf("admission.active = %v, want no issue holding a slot: the gate of %s has opened and %s is accepting", holders, r.root, r.accepting)
	}
	if want := []string{r.childOne, r.childTwo}; !slices.Equal(blockedBy(state, r.root), want) {
		t.Fatalf("admission.blocked names %s waiting on %v, want its two filed children %v", r.root, blockedBy(state, r.root), want)
	}
	if got := state.Issues[r.root]; got.Phase != phase.Admitted || got.Tree != r.root {
		t.Fatalf("%s is phase %s of tree %s, want admitted in its own tree while its children go first", r.root, got.Phase, got.Tree)
	}
	told := r.waitArchitectNotices(r.root, "children-first", 1)
	r.note("read back: one children-first notice on %s's architect topic — %s", r.root, told[0].Payload)
}

// Step 2. The architect releases the two children. Each takes a slot of its own and starts
// planning, which fills a cap of 2 from one tree — the count that was one slot before this change
// — and their parent still holds none and stays at admitted, because its children are now in the
// workflow it waits for.
func (r *admissionIssuesLive) childrenHoldTheSlots() {
	t := r.t
	status, body := r.d.request(http.MethodPost, "/legion/v1/waves/release",
		api.WaveReleaseRequest{GrantID: r.grant(), Issues: []string{r.childOne, r.childTwo}}, false)
	if status != http.StatusOK {
		t.Fatalf("release_children of %s and %s = %d %s, want 200", r.childOne, r.childTwo, status, body)
	}
	r.note("drove POST /legion/v1/waves/release {%s, %s} by %s's architect: %d %s", r.childOne, r.childTwo, r.root, status, body)
	for _, child := range []string{r.childOne, r.childTwo} {
		r.publishStatus(child, "todo", r.root, true)
	}

	state := r.waitForState("both children holding a slot", func(state api.State) bool {
		return slices.Equal(holders(state), []string{r.childOne, r.childTwo})
	})
	for _, child := range []string{r.childOne, r.childTwo} {
		got := state.Issues[child]
		if got.Phase != phase.Planning || got.Slot == nil || got.Tree != r.root {
			t.Fatalf("%s is phase %s of tree %s holding slot %s, want planning in %s's tree with a slot of its own",
				child, got.Phase, got.Tree, slotOf(got), r.root)
		}
	}
	if got := state.Issues[r.root]; got.Slot != nil || got.Phase != phase.Admitted {
		t.Fatalf("%s is phase %s holding slot %s, want it still admitted and holding none while its children work", r.root, got.Phase, slotOf(got))
	}
	if want := []string{r.childOne, r.childTwo}; !slices.Equal(blockedBy(state, r.root), want) {
		t.Fatalf("admission.blocked names %s waiting on %v, want its two children %v, now in the workflow", r.root, blockedBy(state, r.root), want)
	}
	if !slices.Equal(state.Admission.Trees, []string{r.root, r.accepting}) {
		t.Fatalf("admission.trees = %v, want the same two live trees: a child's slot is no tree of its own", state.Admission.Trees)
	}
	r.note("read back: %d slots of a cap of %d, both of %s's tree, and %s itself holds none",
		len(state.Admission.Active), state.Admission.Cap, r.root, r.root)
}

// Step 3. CI goes red on the second tree's pull request while it stands in accepting. The
// transition applies at once — the issue is back in implementing and Dispatch is told in_progress
// — and the start its implementer is owed waits: the two children hold both slots, so the issue
// takes none and stands in the admission line instead. Nothing reports its claim as one nothing
// will wake.
func (r *admissionIssuesLive) redHeadReturnsAWaitingIssue() {
	t := r.t
	r.publishRed()
	state := r.waitForState("the returned issue waiting for a slot", func(state api.State) bool {
		return state.Issues[r.accepting].Phase == phase.Implementing && slices.Contains(state.Admission.Waiting, r.accepting)
	})
	got := state.Issues[r.accepting]
	if got.Slot != nil {
		t.Fatalf("%s holds slot %s, want none: the cap of %d is full of %s's children", r.accepting, slotOf(got), state.Admission.Cap, r.root)
	}
	if !slices.Equal(holders(state), []string{r.childOne, r.childTwo}) {
		t.Fatalf("admission.active = %v, want the two children still holding the cap", holders(state))
	}
	// The transition writes the status through the daemon's outbox, which delivers it to Dispatch
	// after the record the state above is read from. So the write is waited for, under the shared
	// hang guard, rather than read once: reading it once fails whenever the delivery has not
	// landed in the moment between the two, which is what made this row red in CI.
	testwait.Eventually(t, fmt.Sprintf("the Dispatch stand-in's in_progress write for %s, which the transition must apply whatever the cap says", r.accepting),
		func() bool { return r.board.wrote(r.accepting, "in_progress") })
	for _, stranded := range state.StrandedClaims {
		if stranded.Issue == r.accepting {
			t.Fatalf("%s's %s is reported as a claim nothing will wake: a worker whose start waits for a slot is not stranded", r.accepting, stranded.Role)
		}
	}
	r.note("read back: %s is in %s, in admission.waiting %v, holding no slot, and the Dispatch stand-in took its in_progress write",
		r.accepting, got.Phase, state.Admission.Waiting)
}

// Step 4. The architect parks both children, which takes them out of the workflow and frees the
// two slots they held. The line hands them to the returning issue first, whose work came back, and
// to the parent, whose children have now left: the parent's first phase starts, which is the order
// children-first keeps, and both issues hold slots of the same cap that held the children.
func (r *admissionIssuesLive) freedSlotsGoToTheReturnAndTheParent() {
	t := r.t
	for _, child := range []string{r.childOne, r.childTwo} {
		status, body := r.d.request(http.MethodPost, "/legion/v1/children/park",
			api.ChildRequest{GrantID: r.grant(), Issue: child}, false)
		if status != http.StatusOK {
			t.Fatalf("park_child of %s by %s's architect = %d %s, want 200", child, r.root, status, body)
		}
		r.note("drove POST /legion/v1/children/park of %s by %s's architect: %d", child, r.root, status)
		r.publishStatus(child, "backlog", r.root, true)
	}

	state := r.waitForState("the freed slots taken by the return and by the parent", func(state api.State) bool {
		return slices.Equal(holders(state), []string{r.accepting, r.root}) || slices.Equal(holders(state), []string{r.root, r.accepting})
	})
	returned, parent := state.Issues[r.accepting], state.Issues[r.root]
	if returned.Slot == nil || returned.Phase != phase.Implementing {
		t.Fatalf("%s is phase %s holding slot %s, want implementing with the slot its return waited for", r.accepting, returned.Phase, slotOf(returned))
	}
	if parent.Slot == nil || parent.Phase != phase.Planning {
		t.Fatalf("%s is phase %s holding slot %s, want its first phase started once its children left", r.root, parent.Phase, slotOf(parent))
	}
	if blocked := blockedBy(state, r.root); blocked != nil {
		t.Fatalf("admission.blocked still names %s waiting on %v, want the wait over once both children left", r.root, blocked)
	}
	if waiting := state.Admission.Waiting; slices.Contains(waiting, r.accepting) {
		t.Fatalf("admission.waiting still names %s, want it out of the line with its slot", r.accepting)
	}
	for _, child := range []string{r.childOne, r.childTwo} {
		if got := state.Issues[child]; got.Slot != nil {
			t.Fatalf("the parked %s still holds slot %s, want the slot freed when it left the workflow", child, slotOf(got))
		}
	}
	r.note("read back: %d slots of a cap of %d, held by the returned %s and by %s, which is planning; the two parked children hold none",
		len(state.Admission.Active), state.Admission.Cap, r.accepting, r.root)
}

// publishStatus publishes Dispatch's own event of key's move to status under parent, which the
// daemon's consumer reads as it reads any issue event: the architect's own route writes the status
// to Dispatch, and this is the event Dispatch relays for that write (daemon true) or for a
// person's (daemon false). The board is moved with it, as Dispatch's board moves.
func (r *admissionIssuesLive) publishStatus(key, status, parent string, daemon bool) {
	r.t.Helper()
	r.board.place(key, status, parent, false)
	r.seq++
	actor := map[string]string{"kind": "user", "id": "operator"}
	if daemon {
		actor = map[string]string{"kind": "session", "id": dispatch.DaemonSession(key)}
	}
	payload, err := json.Marshal(map[string]any{
		"id": r.seq, "issue_key": key, "project": r.cfg.Project, "seq": r.seq, "type": "issue.updated",
		"actor": actor, "notify": true, "created_at": time.Now().UTC().Format(time.RFC3339Nano),
		"payload": map[string]any{
			"key": key, "status": status, "title": key, "parent": parent, "rank": "M",
			"labels": liveDispatchLabels(false), "last_seq": r.seq, "project": r.cfg.Project, "resolved_repo": liveRepo,
		},
	})
	if err != nil {
		r.t.Fatalf("encode the Dispatch event of %s: %v", key, err)
	}
	r.publish("notifications.dispatch.issue."+key+".issue.updated", "dispatch",
		fmt.Sprintf("%s-%s-%d", key, status, r.seq), string(payload))
	r.note("drove Dispatch's own event of that write: %s is %s under %s", key, status, parent)
}

// publishRed publishes CI's settlement of the second tree's head with its one required check
// failed, as Envoy relays one (intake's decodeChecks reads it).
func (r *admissionIssuesLive) publishRed() {
	r.t.Helper()
	group := func(checks ...string) map[string]any {
		return map[string]any{"count": len(checks), "checks": append([]string{}, checks...)}
	}
	r.seq++
	payload, err := json.Marshal(map[string]any{
		"kind": "checks", "repo": liveRepo, "number": strconv.Itoa(r.acceptingPR), "sha": admissionIssuesHead,
		"check_runs": []map[string]any{{"name": admissionIssuesCheck, "id": r.seq + 1}}, "generation": 1,
		"snapshot": fmt.Sprintf("%s-red-%d", r.accepting, r.seq), "settled_at": time.Now().UnixMilli(),
		"failed": group(admissionIssuesCheck), "passed": group(), "running": group(), "queued": group(),
		"cancelled": group(), "skipped": group(),
	})
	if err != nil {
		r.t.Fatalf("encode the settlement payload: %v", err)
	}
	subject := fmt.Sprintf("notifications.github.%s.pr.%d.checks", r.subjectRepo(), r.acceptingPR)
	r.publish(subject, "github", fmt.Sprintf("checks-%d-%s-%d", r.acceptingPR, admissionIssuesHead, r.seq), string(payload))
	r.note("drove CI's settlement of pull request #%d at %s with %s failed", r.acceptingPR, admissionIssuesHead, admissionIssuesCheck)
}
