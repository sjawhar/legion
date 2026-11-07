//go:build e2e

// The live proof of the two operations dispatch://INFRA-956 R3 gives an operator and a project's
// controller over a claim whose provider keeps refusing its session: `legion claims compact` and
// the controller's `compact`, and `legion claims retry --fresh-session` and the controller's
// `fresh_session`. Each is driven through both of its doors against real `omp --mode rpc` panes
// under the real daemon. scripts/e2e/claim-compaction.sh builds this driver with `-tags e2e` and
// owns the nats-server and the PostgreSQL it runs against.
//
// It is a sibling of provider_error_retry_live_test.go, and what the two share is the live-proof
// base that sits beside them (live_agent_proof_test.go): liveAgentProof's reads of a claim and
// of the daemon's own log, the recording log handler, the model stand-in, the overlay writers,
// the run's own database and the two plugins packed from this checkout. The local runtime that
// starts a pane where there is no tmux is localruntime_live_test.go's. What it proves is the
// other half of R3, so it is a script of its own rather than more steps of that one: the two
// runs would otherwise share a wall clock, and this one starts four panes and waits out a
// compaction the endpoint holds for over half a minute.
//
// The overlays are this deployment's rendered shape, which is what makes the compaction's form
// provable (paneOverlays): an image-capable model, Oh My Pi's own default compaction method order
// — which puts `snapcompact` above the summary methods — and `compaction.keepRecentTokens` small
// enough that a session of a few turns has anything to compact at all. Under those settings the
// only thing keeping the compaction off snapcompact's images is the custom instructions the
// daemon sends (api's compactInstructions, shimwire.Compact), and step 2 reads the compaction
// entry back out of the pane's own session file to show the summary is text.
//
// What stands in is the model endpoint and nothing else. Each pane talks to one HTTP stand-in of
// the driver's own (paneStandin), which answers a turn's request and a compaction's summary
// request separately, so one pane can walk the provider-error ladder while its compaction
// succeeds, is refused, or is held past the daemon's RPC timeout.
//
// Each phase worker is seeded holding the task its phase's start left pending, and the root
// architect's issue stands in accepting, because admission repairs a slotted worker that holds
// no task by starting its phase again (internal/admit/admit.go's resumeSlottedWorkers): the
// seed's own comment says what each of those two agrees with. Every pane answers that task
// before the step that drives it, so what a step delivers is still the first task it reads.
package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// compactKeepRecentTokens is `compaction.keepRecentTokens` on every pane of this proof. Oh My Pi
// keeps that many tokens of the newest turns out of the summary and summarizes the rest, so at
// the shipped 20,000 a session of a few turns has nothing left to summarize and every compaction
// answers "Nothing to compact (session too small)". One token is the smallest honest value: it
// keeps the cut point at the newest turn and leaves everything before it to the summary. Step 1
// reads the value back out of the pane's own Oh My Pi rather than trusting this literal.
const compactKeepRecentTokens = 1

// compactSummaryMarker is how a stand-in tells a compaction's summary request from a turn's: the
// summarization system prompt Oh My Pi sends such a request under, which the plugins' own pane
// harness recognises the same way (packages/pi-shared/test/omp-harness.ts:586-592,
// `isSummarization`). A release that rewords that prompt makes a summary request look like a
// turn, which this proof's first compaction then fails on rather than passing quietly.
const compactSummaryMarker = "Summarize user"

// compactHeldSummary is how long the held-summary stand-in sits on a compaction's answer in step
// 5: past `worker_rpc_timeout_seconds`, which bounds every other request on the worker stream,
// and past 30 s, which is the ladder's first rung. The step reads both of those back and refuses
// to run on a bound this no longer outlasts, so a configuration or a ladder that grew past it
// fails the proof rather than passing on a stale literal.
const compactHeldSummary = 35 * time.Second

// compactSnapcompactMethod is the compaction method this proof must never see on a session the
// daemon compacted: the image-based archive (omp://compaction.md, "Snapcompact method"), which a
// worker resumed on it needs a vision model to read back.
const compactSnapcompactMethod = "snapcompact"

// compactNamesFreshSession is what a compaction's refusal of a claim running no Oh My Pi session
// names as the operation that reaches it instead, word for word through both doors (api's
// freshSessionName, which is that package's own unexported constant).
const compactNamesFreshSession = "fresh_session (`legion claims retry --fresh-session`)"

// The four panes, by the provider name each one's stand-in is given in the run's models.yml. Each
// is one pane's endpoint; the behaviour differs by what a step sets on it, not by the name.
const (
	ladderPaneProvider   = "ladderpane"
	noticePaneProvider   = "noticepane"
	rootPaneProvider     = "rootpane"
	refusingPaneProvider = "refusingpane"
)

// claimCompactionLive is one run of the proof: the booted daemon, the four panes it drives, and
// the controller session whose grant is the controller's own door onto both operations.
type claimCompactionLive struct {
	liveAgentProof
	built *built
	// panes is each live claim's own pane: its issue, its role and the stand-in it talks to.
	panes map[claim.Token]*compactPane
	// ladder, notice, root and refusing are the four issues, one pane each.
	ladder, notice, root, refusing string
	// onClaude is the role worker_backends puts on the Claude Code backend, whose claim is seeded
	// stopped and never launched: the compaction refuses it before anything is asked.
	onClaude claim.Role
	// backends is the map cfg.BackendFor reads, which the API server was handed at boot.
	backends map[claim.Role]runtime.Backend
	// controllerSession and controllerSecret are the registered controller's, which mint the
	// grant the controller's own door is authenticated by.
	controllerSession, controllerSecret string
	// home and profile are the Oh My Pi home and profile every pane runs under, which step 1
	// reads the settings back from.
	home, profile string
	// overlays is each claim's PI_CONFIG_FILES, in order.
	overlays map[claim.Token][]string
}

// compactPane is one live claim of the proof: the issue it works, the role it holds and the model
// stand-in its Oh My Pi talks to.
type compactPane struct {
	issue string
	role  claim.Role
	stand *paneStandin
}

// TestTheOperatorAndTheControllerCompactAClaimsSessionAndStartItFreshLive drives the seven steps
// in the order each one's state follows from the last: the overlays every pane runs under, the
// operator's compaction of a phase worker the ladder holds, the controller's own compaction and
// the second notice its count reset makes possible, a root architect that owes nothing, a
// compaction the endpoint holds past the RPC timeout, every refusal through both doors, and the
// fresh sessions both doors start.
func TestTheOperatorAndTheControllerCompactAClaimsSessionAndStartItFreshLive(t *testing.T) {
	fixtureenv.Live(t)
	r := newClaimCompactionLive(t)
	r.step("step1-the-panes-run-the-deployments-overlays-and-a-short-session-can-be-compacted", r.overlaysAreTheDeployments)
	r.step("step2-the-operator-compacts-a-phase-worker-the-ladder-holds", r.operatorCompactsAPhaseWorker)
	r.step("step3-the-controller-compacts-and-the-run-of-errors-is-told-a-second-time", r.controllerCompactsAndIsToldTwice)
	r.step("step4-a-root-architect-that-owes-no-task-is-sent-nothing-and-says-already-compacted", r.architectIsSentNothing)
	r.step("step5-a-held-summary-outlasts-the-rpc-timeout-and-refuses-a-fresh-session", r.heldSummaryOutlastsTheTimeout)
	r.step("step6-every-refusal-gets-its-own-message-through-both-doors", r.everyRefusalGetsItsMessage)
	r.step("step7-a-fresh-session-starts-each-worker-anew-with-what-its-issue-owes-it", r.freshSessionsStartEachWorker)
}

// newClaimCompactionLive readies everything a pane needs before the daemon boots: the `legion`
// binary and the two plugins built from this checkout, an Oh My Pi profile of the run's own
// routed to the four stand-ins, the issues seeded with their worker mid-launch, the daemon booted
// over the local runtime, and the project's controller registered.
func newClaimCompactionLive(t *testing.T) *claimCompactionLive {
	t.Helper()
	omp := os.Getenv(liveOmpPathEnv)
	if omp == "" {
		t.Skipf("%s is unset, so there is no Oh My Pi for a pane to run; scripts/e2e/claim-compaction.sh requires it", liveOmpPathEnv)
	}
	takeOwnDatabase(t)
	proof := newLiveProof(t, "scripts/e2e/claim-compaction.sh")
	r := &claimCompactionLive{
		liveAgentProof: liveAgentProof{liveProof: proof, omp: omp},
		built:          &built{},
		panes:          map[claim.Token]*compactPane{},
		ladder:         proof.cfg.Project + "-1", notice: proof.cfg.Project + "-2",
		root: proof.cfg.Project + "-3", refusing: proof.cfg.Project + "-4",
		onClaude: claim.RoleReviewer,
		backends: map[claim.Role]runtime.Backend{},
	}
	// The backends map is the one the API server reads through cfg.BackendFor, and it is empty
	// while the daemon boots: boot resolves Claude Code's binary, its bun and its plugin
	// directory for every backend the configuration names (the daemon's resolveClaude), and this
	// pod has no Claude Code to resolve. The role is put on that backend once the daemon is up,
	// because the refusal under test reads that map and nothing else (api's compact, over
	// Options.Backend, which the daemon fills from config.Config.BackendFor); the alternative
	// would put some other binary on a real resolver's path and claim it is Claude Code. The
	// map is written after the boot that read it because a Config's WorkerBackends is a map and
	// every copy of the Config shares it, which is how internal/daemon's own pod-update tests
	// put a role on this backend too (podupdate_test.go:1986).
	r.cfg.WorkerBackends = r.backends
	version, err := exec.Command(omp, "--version").Output()
	if err != nil {
		t.Fatalf("read the Oh My Pi version of %s: %v", omp, err)
	}
	r.note("Oh My Pi %s at %s (the pod's build, which is the fleet's pin and not this branch's fetch)",
		strings.TrimSpace(string(version)), omp)

	root := proofCheckoutRoot(t)
	work := t.TempDir()
	r.legion = buildProofLegion(t, root, work)
	r.home = filepath.Join(work, "omp-home")
	r.profile = "legion-claim-compaction-" + strings.ToLower(r.cfg.Project)
	installProofPlugins(t, root, work, r.home, r.profile, omp)
	for _, seeded := range r.seededPanes() {
		r.panes[r.token(seeded.issue, seeded.role)] = &compactPane{
			issue: seeded.issue, role: seeded.role, stand: newPaneStandin(t, seeded.provider),
		}
	}
	r.writeOmpProfile()
	r.overlays = r.writeOverlays(work)

	r.rt = newLocalRuntime(t, r.cfg.StateDir, r.legion, omp, r.home, r.profile)
	r.rt.daemonURL, r.rt.envoyURL, r.rt.natsURLs = r.cfg.DaemonURL, newPaneListener(t), r.cfg.NatsURLs
	r.rt.overlays = func(token claim.Token) []string { return r.overlays[token] }

	r.seed()
	r.daemon = newRecordingLog(r.log())
	r.d = startDaemonWithin(t, r.cfg, r.overrides(), slog.New(r.daemon), 60*time.Second)
	r.registerController()
	r.backends[r.onClaude] = runtime.BackendClaude
	r.note("worker_backends now runs %s on %s, and the %s claim of %s is stopped and never launched",
		r.onClaude, runtime.Backends[runtime.BackendClaude].Name, r.onClaude, r.ladder)
	return r
}

// seededPane is one of the proof's live claims: the issue it works, the phase that issue stands
// in, the role that works it, and the provider name of the stand-in its pane talks to.
type seededPane struct {
	issue    string
	title    string
	at       phase.Phase
	role     claim.Role
	provider string
}

// seededPanes is the four claims that run a real pane, each on an issue and a tree of its own.
// The root architect's issue is its own tree's root, as every issue here is, so its claim is the
// root architect claim.phase workers and architects both reach. That one stands in accepting,
// the phase whose role is the architect and in which no phase worker runs: it is the only issue
// here with no phase worker's claim, and in a worker's phase admission's repair of a slotted
// worker would start the worker it names (seed).
func (r *claimCompactionLive) seededPanes() []seededPane {
	return []seededPane{
		{r.ladder, "the issue whose planner the ladder holds", phase.Planning, claim.RolePlanner, ladderPaneProvider},
		{r.notice, "the issue whose implementer keeps failing its turns", phase.Implementing, claim.RoleImplementer, noticePaneProvider},
		{r.root, "the issue whose architect owes no task", phase.Accepting, claim.RoleArchitect, rootPaneProvider},
		{r.refusing, "the issue whose summary the endpoint holds and then refuses", phase.Testing, claim.RoleTester, refusingPaneProvider},
	}
}

// pane is the live claim of token, and fails the step when token names none.
func (r *claimCompactionLive) pane(token claim.Token) *compactPane {
	r.t.Helper()
	held, ok := r.panes[token]
	if !ok {
		r.t.Fatalf("this proof drives no pane for %s", token)
	}
	return held
}

// token is the claim token of role on key, as the daemon spells it.
func (r *claimCompactionLive) token(key string, role claim.Role) claim.Token {
	r.t.Helper()
	token, err := claim.NewToken(r.project, key, role)
	if err != nil {
		r.t.Fatal(err)
	}
	return token
}

// registerController registers a controller session for the project and keeps its registration
// secret, which is what mints the grant the controller's own door is authenticated by
// (controllerGrant). It registers as `legion controller start` registers the operator's own:
// the operator's bearer buys a capability, and a session registers with it. No controller
// process runs — the controller's door is driven as its plugin drives it, over the daemon's own
// routes, and what the daemon publishes for the controller is read from the fixture's listener.
func (r *claimCompactionLive) registerController() {
	t := r.t
	t.Helper()
	status, body := r.d.request(http.MethodPost, "/legion/v1/controller/secret",
		api.ControllerSecretRequest{PluginContract: api.DaemonAPIVersion}, true)
	if status != http.StatusOK {
		t.Fatalf("the controller's boot secret = %d %s, want 200", status, body)
	}
	var capability api.ControllerSecretResponse
	if err := json.Unmarshal(body, &capability); err != nil {
		t.Fatalf("decode the controller's boot secret %s: %v", body, err)
	}
	r.controllerSession = "ses_controller_" + randomSuffix(t)
	status, body = r.d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: capability.Secret, SessionID: r.controllerSession,
		OmpSessionFile: "/sessions/" + r.controllerSession + ".jsonl",
		AgentID:        r.controllerSession, PluginContract: api.DaemonAPIVersion,
	}, false)
	if status != http.StatusOK {
		t.Fatalf("register the controller = %d %s, want 200", status, body)
	}
	var registered api.ControllerRegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil {
		t.Fatalf("decode the controller's registration %s: %v", body, err)
	}
	r.controllerSecret = registered.Secret
	r.note("the project's controller is registered as %s (claim %s), and its registration secret mints the grant its own door takes",
		r.controllerSession, registered.ClaimToken)
}

// overrides is the daemon this proof boots: the local runtime over the real worker stream, the
// API listener the fixture holds, and the workflow's App tokens stood in for. The clock is the
// real one: what this proof measures — the RPC timeout a held summary outlasts, and the ladder's
// first rung a compaction's own send comes inside of — is measured where it is real.
func (r *claimCompactionLive) overrides() overrides {
	return overrides{
		runtime: func(_ context.Context, listener *stream.Listener, address string, _ appauth.Tokens,
			_ func(context.Context, string) (ghrepo.Repository, string, error), _ *store.Store, _ func() []ghrepo.Repository) (runtime.Runtime, error) {
			r.built.mu.Lock()
			defer r.built.mu.Unlock()
			r.built.conns, r.built.address = listener, address
			r.rt.conns, r.rt.stream = listener, address
			return r.rt, nil
		},
		listen:         heldListen,
		workflowTokens: &workflowTokenRecorder{},
	}
}

// seed records the issues as the daemon would hold them at the moment each one's worker began: a
// root handed to Legion, in the phase its worker works, holding its tree's admission slot with
// its lifecycle open, and that worker's claim mid-launch, which this boot relaunches. The one
// claim that runs no pane is the Claude Code role's, seeded stopped so nothing ever launches it:
// the compaction refuses such a claim on its backend before it reads any state at all.
//
// Each phase worker's claim also holds the task its phase's start left pending, because a start
// writes the relaunch and the task together (workflow's StartOrOwe) and a slotted worker holding
// no task at all is the state admission repairs: resumeSlottedWorkers (internal/admit/admit.go)
// gives such a claim its phase's resume task on the first pass after boot, which would answer
// every deliver of this driver's 409 "a delivery is already pending". A delivery of the issue's
// own generation and phase, unconfirmed, is what that repair passes over (workflow.StartFor, the
// delivery's StillWorked). Each pane answers that task before its step drives the claim
// (awaitStartTaskAnswered), which leaves the claim idle and live — another state the repair
// passes over — and gives the session the turns a compaction needs to have anything to summarize.
//
// The root architect's issue stands in accepting rather than in a worker phase, for the same
// repair: it has an architect's claim and no phase worker's, and the repair would read the
// planner a planning issue names, find no claim at all and start one. Accepting is the phase
// whose role is the architect (workflow's RoleFor), where no phase worker runs, so the repair
// passes over it as it passes over every architect.
func (r *claimCompactionLive) seed() {
	t := r.t
	t.Helper()
	ctx := context.Background()
	for _, seeded := range r.seededPanes() {
		r.board.show(seeded.issue, "in_progress")
	}
	if err := r.st.Tx(ctx, func(tx pgx.Tx) error {
		for _, seeded := range r.seededPanes() {
			if err := r.records.PutIssue(ctx, tx, record.Issue{
				Key: seeded.issue, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Tree: seeded.issue,
				Project: r.cfg.Project, Title: seeded.title, Phase: seeded.at, Generation: 1, Status: "in_progress",
				DispatchStatus: "in_progress", Rank: "U", LastDispatchSeq: 1, HandedOver: true,
			}); err != nil {
				return err
			}
		}
		return r.admit(ctx, tx, r.ladder, r.notice, r.root, r.refusing)
	}); err != nil {
		t.Fatalf("seed the issues: %v", err)
	}
	for _, seeded := range r.seededPanes() {
		if _, err := r.st.OpenTreeLifecycle(ctx, r.project, seeded.issue, treelifecycle.AuthorityWorkflow); err != nil {
			t.Fatalf("open the lifecycle of %s: %v", seeded.issue, err)
		}
		r.seedClaim(ctx, seeded.issue, seeded.role, supervise.StateLaunching, "")
		if seeded.role == claim.RoleArchitect {
			continue
		}
		// Unconfirmed and unread: the launch this seed interrupts never put the task in front of
		// an agent, so the relaunched pane is sent it once it reports ready.
		token := r.token(seeded.issue, seeded.role)
		if err := r.st.PutDelivery(ctx, token, supervise.Delivery{
			ID: "seeded-start-" + randomSuffix(t), Task: startTask(seeded.role), Generation: 1,
			Phase: seeded.at, QueuedAt: time.Now(),
		}); err != nil {
			t.Fatalf("record the task %s's start left pending: %v", token, err)
		}
	}
	r.seedClaim(ctx, r.ladder, r.onClaude, supervise.StateRetired, "")
	r.note("seeded %s (%s, %s), %s (%s, %s), %s (%s, %s) and %s (%s, %s), each with its worker mid-launch — every phase "+
		"worker holding the task its start left pending, which is what admission's repair of a slotted worker passes "+
		"over — and %s's %s stopped",
		r.ladder, phase.Planning, claim.RolePlanner, r.notice, phase.Implementing, claim.RoleImplementer,
		r.root, phase.Accepting, claim.RoleArchitect, r.refusing, phase.Testing, claim.RoleTester,
		r.ladder, r.onClaude)
}

// startTask is the task a phase worker's claim holds pending at boot: one its pane's stand-in
// answers, since every stand-in answers a turn until a step sets it otherwise (paneStandin).
func startTask(role claim.Role) string {
	return "Say the word START-" + strings.ToUpper(string(role)) + " and nothing else."
}

// seedClaim records one claim of issue in state, with the prompt file every launch reads.
func (r *claimCompactionLive) seedClaim(ctx context.Context, issue string, role claim.Role, state supervise.ClaimState, by supervise.SuspendSource) {
	t := r.t
	t.Helper()
	token := r.token(issue, role)
	if _, err := r.st.AdmitClaim(ctx, supervise.Claim{
		Token: token, Project: r.project, Tree: issue, Issue: issue, Role: role,
		Generation: 1, State: state, SuspendedBy: by,
		BootTokenHash: supervise.HashBootToken("seeded-" + randomSuffix(t)), ServingGeneration: 1,
	}); err != nil {
		t.Fatalf("record the claim %s: %v", token, err)
	}
	writePrompt(t, r.cfg, token)
}

// ---- the steps -----------------------------------------------------------------------------------

// overlaysAreTheDeployments reads back, from the Oh My Pi each pane runs and under the overlays
// each pane is told, the three settings the compaction's form rests on: the method order, which
// is Oh My Pi's own default and puts snapcompact above every summary method; the model's input
// modalities, which include images, so snapcompact is a method this pane could run; and
// keepRecentTokens, which is small enough that a session of a few turns has something to
// summarize. Nothing of the three is asserted against a literal of this file's: each is read with
// the same binary the panes run, under the same overlays, so a changed default fails the proof.
func (r *claimCompactionLive) overlaysAreTheDeployments() {
	t := r.t
	pane := r.seededPanes()[0]
	token := r.token(pane.issue, pane.role)
	order := r.paneSetting(token, "compaction.methodOrder")
	var methods []string
	if err := json.Unmarshal(order, &methods); err != nil {
		t.Fatalf("read %s's compaction.methodOrder %s: %v", token, order, err)
	}
	if !slices.Contains(methods, compactSnapcompactMethod) {
		t.Fatalf("the pane's compaction.methodOrder is %v, which names no %s: the proof's proposition is that the "+
			"custom instructions alone keep the compaction off it", methods, compactSnapcompactMethod)
	}
	r.note("compaction.methodOrder on the pane is %v — Oh My Pi's own default, which this proof overrides nowhere", methods)

	keep := r.paneSetting(token, "compaction.keepRecentTokens")
	var kept int
	if err := json.Unmarshal(keep, &kept); err != nil {
		t.Fatalf("read %s's compaction.keepRecentTokens %s: %v", token, keep, err)
	}
	if kept != compactKeepRecentTokens {
		t.Fatalf("the pane reads compaction.keepRecentTokens %d, want the %d the operator's overlay sets", kept, compactKeepRecentTokens)
	}
	r.note("compaction.keepRecentTokens on the pane is %d, from the operator's own overlay: at the shipped default a session "+
		"of a few turns answers every compaction \"Nothing to compact (session too small)\"", kept)

	// Every model role of the claim's own overlay names one selector, so that is the model the
	// pane's session runs on; the profile's roster carries each pane's.
	selector := standinModel(pane.provider)
	listed := r.paneModels(token)
	served := slices.IndexFunc(listed, func(model paneModel) bool { return model.Selector == selector })
	if served < 0 {
		t.Fatalf("the pane's Oh My Pi serves no %s; it serves %+v", selector, listed)
	}
	if !slices.Contains(listed[served].Input, "image") {
		t.Fatalf("the pane's model %s takes %v, and snapcompact needs one that reads images", selector, listed[served].Input)
	}
	r.note("the model every role of %s names is %s, which takes %v: snapcompact is a method it could run, and the instructions "+
		"the daemon sends are what keep the summary text (api's compactInstructions)", token, selector, listed[served].Input)
	r.note("every pane's PI_CONFIG_FILES is %v: the operator's own overlay, then the claim's own (role_config_files), "+
		"and nothing of Legion's after them", r.overlays[token])
}

// operatorCompactsAPhaseWorker drives `legion claims compact` — the built binary against the
// booted daemon — at an idle phase worker the provider-error ladder holds, and reads back what
// the operation promises: the worker keeps its session, that session holds a text compaction
// entry rather than snapcompact's images, and the task the claim owes is sent once and only after
// the compaction, inside the ladder's own first rung and so by the compaction and not the wait.
func (r *claimCompactionLive) operatorCompactsAPhaseWorker() {
	t := r.t
	token := r.token(r.ladder, claim.RolePlanner)
	r.awaitReady(token)
	r.awaitStartTaskAnswered(token)
	r.answeredTurn(token, "WARM")
	before := r.claim(token)
	if before.Session == "" || before.SessionFile == "" {
		t.Fatalf("%s records no session to compact: %+v", token, before)
	}

	// The mark is taken before the turn: the daemon writes the arming after the error record
	// erroredTurn waits for, with a store write between them (supervise's providerErrorClears),
	// so a single read of the arming the moment that record lands can fall between the two.
	turn := r.daemon.count()
	marker, errored := r.erroredTurn(token, 1)
	armed := r.awaitLogFrom("the ladder's first rung to be armed", turn, time.Minute, func(record loggedRecord) bool {
		return record.Message == "supervise: this provider error can clear; the task goes to the same session again after a wait" &&
			record.Attrs["claim"] == string(token)
	})
	if armed.Attrs["wait"] != ladderDelays[0].String() {
		t.Fatalf("%s is on no rung of the ladder: the daemon armed %+v", token, armed)
	}
	r.note("%s is idle on the ladder's first rung (%v armed) holding %q", token, armed.Attrs["wait"], r.claim(token).Pending.Task)

	from := r.daemon.count()
	compacted := r.legionClaims("compact", "--claim", string(token))
	if compacted != "compacted; whatever task the claim owed has gone again" {
		t.Fatalf("`legion claims compact` printed %q, want the line a compacted session prints", compacted)
	}
	r.note("`legion claims compact --claim %s` printed %q", token, compacted)
	door := r.awaitLogFrom("the daemon's record of which door asked", from, time.Minute, func(record loggedRecord) bool {
		return record.Message == "api: the operator compacted a claim's session" && record.Attrs["claim"] == string(token)
	})
	if door.Attrs["compacted"] != true {
		t.Fatalf("the daemon recorded the operator's compaction as %v, want it compacted", door.Attrs["compacted"])
	}
	r.note("the daemon holds that the operator asked: %q (compacted %v)", door.Message, door.Attrs["compacted"])

	ended := r.awaitLogFrom("the compaction to end", from, runtime.CompactTimeout, func(record loggedRecord) bool {
		return record.Message == "supervise: the compaction of the claim's session is over" && record.Attrs["claim"] == string(token)
	})
	if ended.Attrs["compacted"] != true || ended.Attrs["providerErrorRun"] != float64(0) {
		t.Fatalf("the compaction ended compacted=%v providerErrorRun=%v, want true and 0",
			ended.Attrs["compacted"], ended.Attrs["providerErrorRun"])
	}
	after := r.claim(token)
	if after.Session != before.Session || after.SessionFile != before.SessionFile {
		t.Fatalf("%s runs in session %s (%s) after the compaction, want the one it had, %s (%s)",
			token, after.Session, after.SessionFile, before.Session, before.SessionFile)
	}
	r.note("%s kept its session %s and its transcript %s, and its run of provider-error turns is back to %v",
		token, after.Session, after.SessionFile, ended.Attrs["providerErrorRun"])

	entry := r.awaitCompactionEntry(after.SessionFile, 1)
	r.note("the session's compaction entry is method %q with a %d-character text summary %q and no snapcompact archive (preserveData %v)",
		entry.Method, len(entry.Summary), firstLine(entry.Summary), entry.preserveKeys())

	again := r.awaitLogFrom("the owed task's next turn", from, ladderDelays[0]+time.Minute, func(record loggedRecord) bool {
		return record.Message == providerErroredMessage && record.Attrs["claim"] == string(token)
	})
	if !again.at.After(ended.at) {
		t.Fatalf("the owed task ran a turn at %s, before the compaction ended at %s", again.at, ended.at)
	}
	// Only after the compaction, and only once: the ladder's wait is what would otherwise have
	// sent this task, and it sent nothing between the turn that armed it and this one. That is
	// read from the daemon's own record of every such send rather than from the clock, so a
	// compaction slower than the rung does not fail the proof for the wrong reason.
	if sends := r.logs(func(record loggedRecord) bool {
		return record.Message == "supervise: the wait after a provider error is over; the task goes again" &&
			record.Attrs["claim"] == string(token) && record.at.After(errored.at) && record.at.Before(again.at)
	}); len(sends) != 0 {
		t.Fatalf("the ladder's wait sent the task %d times before the compaction's own send did", len(sends))
	}
	if again.Attrs["providerErrorRun"] != float64(1) {
		t.Fatalf("the turn after the compaction counts providerErrorRun %v, want 1: the compaction put the run back to zero",
			again.Attrs["providerErrorRun"])
	}
	// Once: the daemon's own record of every turn this task's prompt ran. The stand-in's request
	// count is the host's behaviour — a turn carries side requests of Oh My Pi's own, and a later
	// release can carry others — where the send is the daemon's act and the daemon writes it down.
	if turns := r.erroredTurns(token); len(turns) != 2 {
		t.Fatalf("the daemon ran %d turns of the owed task, want 2: the first send and the compaction's own", len(turns))
	}
	r.note("the owed task went again %s after the turn that armed the ladder's %s rung and %s after the compaction ended, "+
		"once by the daemon's own count of the turns it ran, with the ladder's own wait having sent nothing in between; "+
		"the stand-in answered %d requests carrying the task across those two turns",
		again.at.Sub(errored.at).Round(time.Millisecond), ladderDelays[0], again.at.Sub(ended.at).Round(time.Millisecond),
		r.pane(token).stand.turnsHolding(marker))
}

// controllerCompactsAndIsToldTwice drives the controller's own door — a controller grant minted
// from its registration, then `POST /legion/v1/claims/compact` naming the issue and the role —
// and the one thing only a compaction that ran makes possible: a second notice. The run of
// provider-error turns reaches six, the project's controller is told once, the compaction puts
// that run back to zero, and six more such turns tell it again.
func (r *claimCompactionLive) controllerCompactsAndIsToldTwice() {
	t := r.t
	token := r.token(r.notice, claim.RoleImplementer)
	r.awaitReady(token)
	r.awaitStartTaskAnswered(token)
	r.answeredTurn(token, "WARM")
	for run := 1; run <= 6; run++ {
		r.erroredTurn(token, run)
	}
	r.awaitControllerNotices(r.notice, 1)
	told := r.controllerNotices(r.notice)
	r.note("the project's controller was told once on %s: %q", notify.ControllerTopic(r.project), told[0])

	from := r.daemon.count()
	answer := r.compactedByController(r.notice, claim.RoleImplementer)
	if !answer.Compacted {
		t.Fatalf("the controller's compaction answered %+v, want a session compacted", answer)
	}
	door := r.awaitLogFrom("the daemon's record of which door asked", from, time.Minute, func(record loggedRecord) bool {
		return record.Message == "api: the controller compacted a claim's session" && record.Attrs["claim"] == string(token)
	})
	r.note("the daemon holds that the controller asked: %q (issue %v, role %v, compacted %v)",
		door.Message, door.Attrs["issue"], door.Attrs["role"], door.Attrs["compacted"])
	ended := r.awaitLogFrom("the compaction to end", from, runtime.CompactTimeout, func(record loggedRecord) bool {
		return record.Message == "supervise: the compaction of the claim's session is over" && record.Attrs["claim"] == string(token)
	})
	if ended.Attrs["providerErrorRun"] != float64(0) {
		t.Fatalf("the compaction left providerErrorRun %v, want it back to zero", ended.Attrs["providerErrorRun"])
	}
	// The owed task the compaction sent again is the first of the six turns that earn the second
	// notice: it ends in the same 529, so the run starts over at one.
	r.awaitErroredTurnFrom(token, 1, from, ladderDelays[0]+time.Minute)
	r.note("the compaction put the run back to zero and sent the owed task again, whose turn starts the next run at 1")
	for run := 2; run <= 6; run++ {
		r.erroredTurn(token, run)
	}
	r.awaitControllerNotices(r.notice, 2)
	told = r.controllerNotices(r.notice)
	if told[1] != told[0] {
		r.note("the second notice reads %q", told[1])
	}
	r.note("after six more refused turns the controller was told a second time, which only the compaction's count reset allows")
}

// architectIsSentNothing compacts the one claim that owes no task — an idle root architect — and
// holds that nothing is sent to it, through both doors; and that a second compaction of a session
// nothing has changed since is reported to the caller in Oh My Pi's own words, with the operation
// still answering 200 rather than a refusal.
func (r *claimCompactionLive) architectIsSentNothing() {
	t := r.t
	token := r.token(r.root, claim.RoleArchitect)
	r.awaitReady(token)
	r.answeredTurn(token, "WARM")
	stand := r.pane(token).stand
	if held := r.claim(token); held.Pending != nil {
		t.Fatalf("%s owes %+v, and this step's claim is the one that owes nothing", token, held.Pending)
	}
	turnsBefore := stand.turns()

	from := r.daemon.count()
	first := r.compactedByOperator(token)
	if !first.Compacted {
		t.Fatalf("the operator's compaction of the root architect answered %+v, want a session compacted", first)
	}
	r.awaitLogFrom("the daemon's record that the operator asked", from, time.Minute, func(record loggedRecord) bool {
		return record.Message == "api: the operator compacted a claim's session" && record.Attrs["claim"] == string(token)
	})
	r.awaitLogFrom("the compaction to end", from, runtime.CompactTimeout, func(record loggedRecord) bool {
		return record.Message == "supervise: the compaction of the claim's session is over" && record.Attrs["claim"] == string(token)
	})
	entry := r.awaitCompactionEntry(r.claim(token).SessionFile, 1)
	r.note("the root architect's session was compacted (method %q, summary %q) and it kept session %s",
		entry.Method, firstLine(entry.Summary), r.claim(token).Session)

	asked := r.daemon.count()
	second := r.compactedByController(r.root, claim.RoleArchitect)
	if second.Compacted || second.Reason == "" {
		t.Fatalf("the second compaction answered %+v, want it not compacted with Oh My Pi's own reason", second)
	}
	r.awaitLogFrom("the daemon's record that the controller asked", asked, time.Minute, func(record loggedRecord) bool {
		return record.Message == "api: the controller compacted a claim's session" &&
			record.Attrs["claim"] == string(token) && record.Attrs["compacted"] == false
	})
	r.note("the controller's compaction of the same session answered 200 with compacted=false, reason %q", second.Reason)

	// Nothing is owed, so nothing goes: watched for liveSettle, which is longer than the send a
	// compaction's end makes takes to reach the stand-in.
	time.Sleep(liveSettle)
	if held := r.claim(token); held.Pending != nil {
		t.Fatalf("%s was sent %+v after its compactions, and it owed nothing", token, held.Pending)
	}
	if turns := stand.turns(); turns != turnsBefore {
		t.Fatalf("the root architect's stand-in answered %d turns across its two compactions, want the %d it had: an architect that owes nothing is sent nothing",
			turns, turnsBefore)
	}
	r.note("%s still owes nothing and ran no turn in %s after either compaction: the end of a compaction sends what the claim owes, and it owes none",
		token, liveSettle)
}

// heldSummaryOutlastsTheTimeout drives a compaction whose endpoint sits on the summary past
// `worker_rpc_timeout_seconds` and past the ladder's first rung: the caller still gets the real
// answer when it arrives, a fresh session asked for while it runs is refused through both doors,
// and the task the claim owes goes at the compaction's end rather than at the ladder's.
func (r *claimCompactionLive) heldSummaryOutlastsTheTimeout() {
	t := r.t
	token := r.token(r.refusing, claim.RoleTester)
	r.awaitReady(token)
	r.awaitStartTaskAnswered(token)
	r.answeredTurn(token, "WARM")
	if compactHeldSummary <= r.cfg.WorkerRPCTimeout || compactHeldSummary <= ladderDelays[0] {
		t.Fatalf("this step holds the summary for %s, which no longer outlasts worker_rpc_timeout_seconds (%s) or the ladder's first rung (%s)",
			compactHeldSummary, r.cfg.WorkerRPCTimeout, ladderDelays[0])
	}
	_, errored := r.erroredTurn(token, 1)
	stand := r.pane(token).stand
	stand.holdSummary(compactHeldSummary)
	r.note("the stand-in holds every summary answer for %s: past worker_rpc_timeout_seconds (%s), which bounds every other request on the worker stream, and past the ladder's first rung (%s)",
		compactHeldSummary, r.cfg.WorkerRPCTimeout, ladderDelays[0])

	from := r.daemon.count()
	started := time.Now()
	answered := make(chan compactRun, 1)
	go func() {
		out, err := r.legionClaimsTry("compact", "--claim", string(token))
		answered <- compactRun{out: out, err: err}
	}()
	r.awaitLogFrom("the claim to be taken for its compaction", from, time.Minute, func(record loggedRecord) bool {
		return record.Message == "supervise: the claim's session is being compacted; it is busy until the compaction ends" &&
			record.Attrs["claim"] == string(token)
	})
	// Well past the RPC timeout, and while the compaction is still in flight: the fresh session
	// is refused through both doors, and so is a second compaction, each in its own words.
	time.Sleep(2 * r.cfg.WorkerRPCTimeout)
	r.refusedThroughBothDoors("a fresh session asked for while the compaction runs", freshSessionDoors, token, r.refusing, claim.RoleTester,
		http.StatusConflict, "CLAIM_IN_A_TURN", "a fresh session would cut that turn off, so ask again after the turn ends")
	r.refusedThroughBothDoors("a second compaction while the first runs", compactDoors, token, r.refusing, claim.RoleTester,
		http.StatusConflict, "CLAIM_IN_A_TURN", "is being compacted already")

	run := <-answered
	waited := time.Since(started)
	if run.err != nil {
		t.Fatalf("the held compaction failed after %s: %v\n%s", waited.Round(time.Second), run.err, run.out)
	}
	if run.out != "compacted; whatever task the claim owed has gone again" {
		t.Fatalf("the held compaction printed %q, want the line a compacted session prints", run.out)
	}
	r.awaitLogFrom("the daemon's record that the operator asked", from, time.Minute, func(record loggedRecord) bool {
		return record.Message == "api: the operator compacted a claim's session" &&
			record.Attrs["claim"] == string(token) && record.Attrs["compacted"] == true
	})
	if waited <= r.cfg.WorkerRPCTimeout || waited <= 30*time.Second {
		t.Fatalf("the held compaction answered after %s, which is not past worker_rpc_timeout_seconds (%s) and 30 s",
			waited.Round(time.Millisecond), r.cfg.WorkerRPCTimeout)
	}
	r.note("the caller got the real answer %q after %s, past the RPC timeout %s and past 30 s",
		run.out, waited.Round(time.Second), r.cfg.WorkerRPCTimeout)
	stand.answerSummary()

	again := r.awaitLogFrom("the owed task's next turn", from, 2*time.Minute, func(record loggedRecord) bool {
		return record.Message == providerErroredMessage && record.Attrs["claim"] == string(token) && record.at.After(started)
	})
	// Once, and by the daemon's own count of the turns it ran: the ladder's own first rung came
	// due while the compaction held the claim, and a claim being compacted is handed nothing.
	if turns := r.erroredTurns(token); len(turns) != 2 {
		t.Fatalf("the daemon ran %d turns of the owed task, want 2: the first send and the compaction's own", len(turns))
	}
	if sends := r.logs(func(record loggedRecord) bool {
		return record.Message == "supervise: the wait after a provider error is over; the task goes again" &&
			record.Attrs["claim"] == string(token) && record.at.After(errored.at) && record.at.Before(again.at)
	}); len(sends) == 0 {
		t.Fatalf("the ladder's %s rung never came due inside the %s the summary was held for", ladderDelays[0], compactHeldSummary)
	}
	r.note("the ladder's own %s rung came due while the compaction held the claim and sent nothing; the owed task went at the "+
		"compaction's end, %s after the turn that armed the ladder, once by the daemon's own count of the turns it ran",
		ladderDelays[0], again.at.Sub(errored.at).Round(time.Second))
}

// everyRefusalGetsItsMessage drives, through both doors, every case the compaction refuses before
// it asks anything, and the two the operation reports to its caller instead. Each refusal is the
// daemon's own, made from the claim's state, its role's backend or the role the request names,
// and both doors give it word for word.
func (r *claimCompactionLive) everyRefusalGetsItsMessage() {
	t := r.t
	tester := r.token(r.refusing, claim.RoleTester)
	stand := r.pane(tester).stand

	// A summary the endpoint refuses every time: reported to the caller, with the owed task sent
	// again all the same, which is the operation's whole promise.
	stand.refuseSummary()
	from := r.daemon.count()
	refused := r.compactedByController(r.refusing, claim.RoleTester)
	// Oh My Pi names the pass that failed — the history summary's, or a split turn's prefix — so
	// the words are matched rather than the whole sentence.
	if refused.Compacted || !strings.Contains(strings.ToLower(refused.Reason), "summarization failed") {
		t.Fatalf("a refused summary answered %+v, want compacted=false carrying Oh My Pi's own reason", refused)
	}
	r.note("the endpoint refused every summary request and the caller was told: 200, compacted=false, reason %q", refused.Reason)
	ended := r.awaitLogFrom("the refused compaction to end", from, runtime.CompactTimeout, func(record loggedRecord) bool {
		return record.Message == "supervise: the compaction of the claim's session is over" && record.Attrs["claim"] == string(tester)
	})
	if ended.Attrs["compacted"] != false || ended.Attrs["providerErrorRun"] == float64(0) {
		t.Fatalf("the refused compaction ended compacted=%v providerErrorRun=%v, want false and a run it did not reset",
			ended.Attrs["compacted"], ended.Attrs["providerErrorRun"])
	}
	r.awaitErroredTurnFrom(tester, 0, from, ladderDelays[0]+time.Minute)
	r.note("the owed task still went again, and the refused compaction reset no run of provider-error turns (providerErrorRun %v)",
		ended.Attrs["providerErrorRun"])
	stand.answerSummary()

	// A worker in a turn of its own: the stand-in holds the turn open, so the claim is working
	// when the request arrives.
	stand.holdTurn()
	r.deliver(tester, "Say the word HELD-"+randomSuffix(t)+" and nothing else.")
	r.await(string(tester)+" to be in a turn", 2*time.Minute, func() bool {
		return r.claim(tester).State == string(supervise.StateWorking)
	})
	r.refusedThroughBothDoors("a worker in a turn of its own", compactDoors, tester, r.refusing, claim.RoleTester,
		http.StatusConflict, "CLAIM_IN_A_TURN", "a compaction would abort that turn, so ask again after the turn ends")
	stand.releaseTurn()
	r.await(string(tester)+"'s turn to end", 2*time.Minute, func() bool {
		return r.claim(tester).State != string(supervise.StateWorking)
	})

	// A role the configuration runs on Claude Code, which keeps no Oh My Pi session at all. The
	// claim runs nothing and never has: the refusal comes before the backend is used for anything.
	r.refusedThroughBothDoors("a role worker_backends runs on Claude Code", compactDoors,
		r.token(r.ladder, r.onClaude), r.ladder, r.onClaude,
		http.StatusConflict, "CLAIM_NOT_ON_OMP", runtime.Backends[runtime.BackendClaude].Name, compactNamesFreshSession)

	// The project's controller is no role a claim token is built from, so the controller's own
	// door — the only one that names a role at all — refuses it by name, for both operations.
	r.refusedByController("compact naming role controller", "/legion/v1/claims/compact",
		api.ClaimRequest{Issue: r.refusing, Role: string(claim.RoleController)},
		http.StatusBadRequest, "INVALID_ROLE", "the project's controller is none of them")
	r.refusedByController("fresh_session naming role controller", "/legion/v1/claims/fresh-session",
		api.ClaimRequest{Issue: r.refusing, Role: string(claim.RoleController)},
		http.StatusBadRequest, "INVALID_ROLE", "the project's controller is none of them")
	// The operator's door names a claim by its token, and this deployment launches no controller
	// of its own (`controller: operator`, config.ControllerLaunchOperator, which is what the
	// fixture boots with), so the daemon supervises no claim by the controller's token and the
	// route answers that rather than freshSession's NOT_THE_CONTROLLER. Both ways the controller
	// takes no fresh session.
	status, body := r.d.request(http.MethodPost,
		"/legion/v1/operator/claims/"+string(claim.ControllerToken(r.project))+"/retry",
		api.RetryRequest{FreshSession: true}, true)
	if status != http.StatusNotFound || !strings.Contains(string(body), "no claim "+string(claim.ControllerToken(r.project))) {
		t.Fatalf("the operator's fresh session of the controller's own claim = %d %s, want 404 naming no such claim", status, body)
	}
	r.note("the operator's door answers the controller's own token %d %s: this deployment launches no controller claim, and the "+
		"controller's own door cannot name the role at all", status, strings.TrimSpace(string(body)))

	// A suspended, a stopped and a failed claim: each runs no Oh My Pi session, so each is told
	// that a fresh session is what reaches it instead.
	implementer := r.token(r.notice, claim.RoleImplementer)
	r.note("%s", r.legionClaims("suspend", "--claim", string(implementer)))
	r.await(string(implementer)+" to be suspended", 2*time.Minute, func() bool {
		return r.claim(implementer).State == string(supervise.StateSuspended)
	})
	r.refusedThroughBothDoors("a suspended worker", compactDoors, implementer, r.notice, claim.RoleImplementer,
		http.StatusConflict, "CLAIM_NOT_RUNNING", string(supervise.StateSuspended), compactNamesFreshSession)

	// The ladder's planner is finished with: its kept task is answered first, so step 7 can read
	// what its issue's phase owes a fresh session rather than the task the ladder held.
	planner := r.token(r.ladder, claim.RolePlanner)
	r.answeredTurn(planner, "CLEAR")
	r.note("%s", r.legionClaims("stop", "--claim", string(planner)))
	r.await(string(planner)+" to retire", 2*time.Minute, func() bool {
		return r.claim(planner).State == string(supervise.StateRetired)
	})
	r.refusedThroughBothDoors("a worker an operator stopped", compactDoors, planner, r.ladder, claim.RolePlanner,
		http.StatusConflict, "CLAIM_NOT_RUNNING", string(supervise.StateRetired), compactNamesFreshSession)

	// A failed claim: an HTTP 400 is a client error that is none of 401, 403, 408 and 429, so the
	// turn fails the claim rather than going again (supervise's holdsTheIssue).
	stand.failTurn()
	r.deliver(tester, "Say the word FAILING-"+randomSuffix(t)+" and nothing else.")
	r.await(string(tester)+" to fail", 4*time.Minute, func() bool {
		return r.claim(tester).State == string(supervise.StateFailed)
	})
	stand.answerTurn()
	r.refusedThroughBothDoors("a failed worker", compactDoors, tester, r.refusing, claim.RoleTester,
		http.StatusConflict, "CLAIM_NOT_RUNNING", string(supervise.StateFailed), compactNamesFreshSession)
}

// freshSessionsStartEachWorker drives `legion claims retry --fresh-session` and the controller's
// own `fresh_session`, and reads back what each worker is given in the session it starts in: a
// phase worker its issue's current phase task, and the root architect its catch-up.
//
// The phase worker is the one step 6 stopped, and the daemon starts such a worker again on its
// own: its issue holds a slot in a worker phase and its claim owes no task, which is the state
// admission repairs (internal/admit/admit.go's resumeSlottedWorkers). So this step asks for the
// fresh session through retryFreshSession, which waits out a claim the daemon is in the middle
// of starting, and what it reads back afterwards is the same either way: the fresh session is
// the operator's, and the task in it is the one the issue's phase owes (api's
// restartWorkedPhase).
func (r *claimCompactionLive) freshSessionsStartEachWorker() {
	t := r.t
	planner := r.token(r.ladder, claim.RolePlanner)
	from := r.daemon.count()
	stopped, retried := r.retryFreshSession(planner)
	r.note("`legion claims retry --claim %s --fresh-session` on a claim that was %s: %s", planner, stopped.State, retried)
	r.awaitLogFrom("the daemon's record of which door asked", from, time.Minute, func(record loggedRecord) bool {
		return record.Message == "api: operator request" && record.Attrs["claim"] == string(planner) &&
			strings.HasSuffix(fmt.Sprint(record.Attrs["route"]), "/retry")
	})
	fresh := r.awaitFreshPane(planner, stopped.Session)
	task := r.awaitPendingTask(planner)
	at := r.issue(r.ladder).Phase
	if task.Phase != at {
		t.Fatalf("%s's fresh session was sent a task of phase %q, want its issue's current phase %q: %q",
			planner, task.Phase, at, task.Task)
	}
	r.note("%s runs in the new session %s (was %s, relaunched with no session to resume) and was sent %s's %s task: %q",
		planner, fresh.Session, stopped.Session, r.ladder, task.Phase, firstLine(task.Task))

	architect := r.token(r.root, claim.RoleArchitect)
	was := r.claim(architect)
	caughtUp := len(r.architectNotices(r.root, "catch-up"))
	from = r.daemon.count()
	status, body := r.controllerRequest("/legion/v1/claims/fresh-session",
		api.ClaimRequest{Issue: r.root, Role: string(claim.RoleArchitect)})
	if status != http.StatusOK {
		t.Fatalf("the controller's fresh session of %s = %d %s, want 200", architect, status, body)
	}
	r.awaitLogFrom("the daemon's record of which door asked", from, time.Minute, func(record loggedRecord) bool {
		return record.Message == "api: the controller started a claim in a fresh session" && record.Attrs["claim"] == string(architect)
	})
	r.note("the daemon holds that the controller asked for %s's fresh session", architect)
	started := r.awaitFreshPane(architect, was.Session)
	told := r.waitArchitectNotices(r.root, "catch-up", caughtUp+1)
	r.note("%s runs in the new session %s (was %s) and its new launch was told its tree's catch-up: %q",
		architect, started.Session, was.Session, told[len(told)-1].Payload)
	r.note("`legion claims list` at the end reads:\n%s", indentOutput(r.legionClaims("list")))
}

// ---- what a step drives --------------------------------------------------------------------------

// compactRun is one `legion claims compact` a step ran off the test's own goroutine.
type compactRun struct {
	out string
	err error
}

// deliver gives the claim one task of the operator's own, which carries no phase.
func (r *claimCompactionLive) deliver(token claim.Token, task string) {
	r.t.Helper()
	status, body := r.d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/deliver",
		api.DeliverRequest{Task: task}, true)
	if status != http.StatusOK {
		r.t.Fatalf("deliver %q to %s = %d %s, want 200", task, token, status, body)
	}
}

// answeredTurn gives the claim one task its stand-in answers, and waits for the turn to end with
// the task retired. It is how a step puts turns in a session before compacting it — a session of
// nothing has nothing to summarize — and how it clears a task a provider error kept.
func (r *claimCompactionLive) answeredTurn(token claim.Token, what string) {
	r.t.Helper()
	marker := what + "-" + randomSuffix(r.t)
	r.pane(token).stand.answerTurn()
	r.deliver(token, "Say the word "+marker+" and nothing else.")
	r.await(string(token)+" to answer the task "+marker, 4*time.Minute, func() bool {
		held := r.claim(token)
		return held.Pending == nil && held.State != string(supervise.StateWorking)
	})
}

// awaitStartTaskAnswered waits until the pane has answered the task its phase's start left
// pending on the claim (seed) and the claim holds nothing. Only then is a task this step
// delivers taken — the daemon refuses a second task while one is pending — and only then is the
// claim idle and live, which is a state admission's repair of a slotted worker passes over
// (workflow.StartFor).
func (r *claimCompactionLive) awaitStartTaskAnswered(token claim.Token) {
	r.t.Helper()
	pane := r.pane(token)
	task := startTask(pane.role)
	r.await(string(token)+" to answer the task its start left pending", 4*time.Minute, func() bool {
		held := r.claim(token)
		return held.Pending == nil && held.State != string(supervise.StateWorking)
	})
	if got := pane.stand.turnsHolding(task); got == 0 {
		r.t.Fatalf("%s's stand-in answered no turn carrying the task its start left pending, %q", token, task)
	}
	r.note("%s answered the task its start left pending (%q) and holds nothing, so what it is given next is this step's",
		token, task)
}

// erroredTurn gives the claim one task its stand-in answers 529, waits for the daemon to read the
// turn's provider error, and holds that the claim's run of such turns is now run (0 asks for
// none). It answers the task's marker and the record the daemon wrote.
func (r *claimCompactionLive) erroredTurn(token claim.Token, run int) (string, *loggedRecord) {
	r.t.Helper()
	marker := "REFUSED-" + randomSuffix(r.t)
	r.pane(token).stand.errorTurn()
	from := r.daemon.count()
	r.deliver(token, "Say the word "+marker+" and nothing else.")
	return marker, r.awaitErroredTurnFrom(token, run, from, 4*time.Minute)
}

// erroredTurns is every turn of the claim the daemon has read a provider error from, oldest
// first: the daemon's own count of the times it put this claim's task in front of its agent and
// the provider refused, which is what "the task was sent once" is a statement about.
func (r *claimCompactionLive) erroredTurns(token claim.Token) []loggedRecord {
	return r.logs(func(record loggedRecord) bool {
		return record.Message == providerErroredMessage && record.Attrs["claim"] == string(token)
	})
}

func (r *claimCompactionLive) awaitErroredTurnFrom(token claim.Token, run, from int, within time.Duration) *loggedRecord {
	r.t.Helper()
	errored := r.awaitLogFrom("a turn of "+string(token)+" to end in a provider error", from, within, func(record loggedRecord) bool {
		return record.Message == providerErroredMessage && record.Attrs["claim"] == string(token)
	})
	if errored.Attrs["httpStatus"] != float64(529) {
		r.t.Fatalf("%s's turn ended with httpStatus %v, want the 529 its stand-in answers", token, errored.Attrs["httpStatus"])
	}
	if run > 0 && errored.Attrs["providerErrorRun"] != float64(run) {
		r.t.Fatalf("%s's turn counts providerErrorRun %v, want %d", token, errored.Attrs["providerErrorRun"], run)
	}
	return errored
}

// retryFreshSession runs `legion claims retry --claim <token> --fresh-session` — the built
// binary against the booted daemon — and answers the claim as it stood when the command the
// daemon accepted ran, together with what that command printed.
//
// It asks again while the daemon refuses: the operation reaches a claim whose agent is ready or
// idle and one that is suspended, stopped or failed, and refuses one it is in the middle of
// starting or one in a turn. A worker an operator stopped is one the daemon starts again by
// itself — its issue holds a slot in a worker phase and its claim owes no task, the state
// admission repairs (internal/admit/admit.go's resumeSlottedWorkers) — so the claim this step
// asks about may be launching, and the refusal is the daemon's own word that it is not there
// yet. Every refusal is printed with the failure, so a refusal for any other reason is read
// rather than hidden behind the wait.
func (r *claimCompactionLive) retryFreshSession(token claim.Token) (api.OperatorClaim, string) {
	r.t.Helper()
	deadline := time.Now().Add(paneReady)
	for {
		before := r.claim(token)
		out, err := r.legionClaimsTry("retry", "--claim", string(token), "--fresh-session")
		if err == nil {
			return before, out
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("`legion claims retry --fresh-session` on %s was refused for %s; it is %s and the last answer was %v:\n%s",
				token, paneReady, before.State, err, out)
		}
		time.Sleep(time.Second)
	}
}

// ---- the two doors -------------------------------------------------------------------------------

// doorPair is one operation's two doors, each named as its caller knows it: `operator` is the
// route `legion claims` posts to with the operator's bearer, `controller` the route the
// controller's plugin posts to with a controller grant, naming the issue and the role.
type doorPair struct {
	operation  string
	operator   func(r *claimCompactionLive, token claim.Token) (int, []byte)
	controller func(r *claimCompactionLive, issue string, role claim.Role) (int, []byte)
}

// compactDoors and freshSessionDoors are the two operations' door pairs.
var (
	compactDoors = doorPair{
		operation: "compact",
		operator: func(r *claimCompactionLive, token claim.Token) (int, []byte) {
			return r.d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/compact", nil, true)
		},
		controller: func(r *claimCompactionLive, issue string, role claim.Role) (int, []byte) {
			return r.controllerRequest("/legion/v1/claims/compact", api.ClaimRequest{Issue: issue, Role: string(role)})
		},
	}
	freshSessionDoors = doorPair{
		operation: "fresh_session",
		operator: func(r *claimCompactionLive, token claim.Token) (int, []byte) {
			return r.d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/retry",
				api.RetryRequest{FreshSession: true}, true)
		},
		controller: func(r *claimCompactionLive, issue string, role claim.Role) (int, []byte) {
			return r.controllerRequest("/legion/v1/claims/fresh-session", api.ClaimRequest{Issue: issue, Role: string(role)})
		},
	}
)

// controllerRequest posts one of the controller's own requests: a grant minted from the
// registered controller's session and secret, as its plugin mints one per operation
// (packages/pi-shared/src/daemon-client.ts), then the route with that grant in the body.
func (r *claimCompactionLive) controllerRequest(path string, body api.ClaimRequest) (int, []byte) {
	r.t.Helper()
	body.GrantID = r.controllerGrant()
	return r.d.request(http.MethodPost, path, body, false)
}

// controllerGrant mints one controller grant over the daemon's own route, the controller-session
// form: the session the controller registered, proving the secret that registration issued.
func (r *claimCompactionLive) controllerGrant() string {
	r.t.Helper()
	status, body := r.d.request(http.MethodPost, "/legion/v1/grants",
		api.GrantRequest{SessionID: r.controllerSession, Secret: r.controllerSecret}, false)
	if status != http.StatusOK {
		r.t.Fatalf("mint the controller's grant = %d %s, want 200", status, body)
	}
	var minted api.GrantResponse
	if err := json.Unmarshal(body, &minted); err != nil {
		r.t.Fatalf("decode the controller's grant %s: %v", body, err)
	}
	return minted.GrantID
}

// compactedByOperator and compactedByController run one door's compaction and hold that it
// answered 200, returning what became of the compaction.
func (r *claimCompactionLive) compactedByOperator(token claim.Token) api.CompactResponse {
	r.t.Helper()
	status, body := compactDoors.operator(r, token)
	return r.compactAnswer("the operator's", status, body)
}

func (r *claimCompactionLive) compactedByController(issue string, role claim.Role) api.CompactResponse {
	r.t.Helper()
	status, body := compactDoors.controller(r, issue, role)
	return r.compactAnswer("the controller's", status, body)
}

func (r *claimCompactionLive) compactAnswer(whose string, status int, body []byte) api.CompactResponse {
	r.t.Helper()
	if status != http.StatusOK {
		r.t.Fatalf("%s compaction = %d %s, want 200", whose, status, body)
	}
	var answer api.CompactResponse
	if err := json.Unmarshal(body, &answer); err != nil {
		r.t.Fatalf("decode %s compaction %s: %v", whose, body, err)
	}
	return answer
}

// refusedThroughBothDoors drives one operation at one claim through both of its doors and holds
// that each is refused with the same status, the same code and the same sentence: the effect and
// its refusals are one piece of the daemon, and only the authentication differs (api's compact
// and freshSession).
func (r *claimCompactionLive) refusedThroughBothDoors(what string, doors doorPair, token claim.Token,
	issue string, role claim.Role, status int, code string, phrases ...string) {
	r.t.Helper()
	operatorStatus, operatorBody := doors.operator(r, token)
	operator := r.heldRefusal(doors.operation+" at "+what+", through the operator's door", operatorStatus, operatorBody, status, code, phrases...)
	controllerStatus, controllerBody := doors.controller(r, issue, role)
	controller := r.heldRefusal(doors.operation+" at "+what+", through the controller's door", controllerStatus, controllerBody, status, code, phrases...)
	if operator != controller {
		r.t.Fatalf("%s at %s: the operator's door said %q and the controller's %q; both doors give one refusal word for word",
			doors.operation, what, operator, controller)
	}
	r.note("%s at %s is refused %d %s through both doors, word for word: %q", doors.operation, what, status, code, operator)
}

// refusedByController drives one request only the controller's door can make and holds its
// refusal. The operator's door names a claim by its token, so it can name no role at all.
func (r *claimCompactionLive) refusedByController(what, path string, body api.ClaimRequest, status int, code string, phrases ...string) {
	r.t.Helper()
	got, answered := r.controllerRequest(path, body)
	refusal := r.heldRefusal(what, got, answered, status, code, phrases...)
	r.note("%s is refused %d %s: %q", what, status, code, refusal)
}

// heldRefusal holds that a route refused with status, code and every phrase, and answers the
// refusal's own sentence.
func (r *claimCompactionLive) heldRefusal(what string, got int, body []byte, status int, code string, phrases ...string) string {
	r.t.Helper()
	if got != status {
		r.t.Fatalf("%s = %d %s, want %d", what, got, body, status)
	}
	var failure api.Failure
	if err := json.Unmarshal(body, &failure); err != nil {
		r.t.Fatalf("decode the refusal of %s, %s: %v", what, body, err)
	}
	if failure.Code != code {
		r.t.Fatalf("%s is refused %q, want %q: %s", what, failure.Code, code, body)
	}
	for _, phrase := range phrases {
		if !strings.Contains(failure.Error, phrase) {
			r.t.Fatalf("the refusal of %s does not say %q: %q", what, phrase, failure.Error)
		}
	}
	return failure.Error
}

// ---- reads the steps make --------------------------------------------------------------------------

// awaitPendingTask waits until the claim holds a task, and answers it.
func (r *claimCompactionLive) awaitPendingTask(token claim.Token) api.DeliveryView {
	r.t.Helper()
	var task api.DeliveryView
	r.await(string(token)+" to be sent a task", 2*time.Minute, func() bool {
		held := r.claim(token)
		if held.Pending == nil {
			return false
		}
		task = *held.Pending
		return true
	})
	return task
}

// awaitFreshPane waits until the claim runs in a session other than old, having been relaunched
// with no recorded session to resume, and answers the claim as it stands then.
func (r *claimCompactionLive) awaitFreshPane(token claim.Token, old string) api.OperatorClaim {
	r.t.Helper()
	var now api.OperatorClaim
	r.await(string(token)+" to run in a session of its own", paneReady, func() bool {
		now = r.claim(token)
		return now.Session != "" && now.Session != old &&
			(now.State == string(supervise.StateReady) || now.State == string(supervise.StateIdle) ||
				now.State == string(supervise.StateWorking))
	})
	launches := r.rt.LaunchesOf(token)
	if last := launches[len(launches)-1]; last.ResumeSessionFile != "" {
		r.t.Fatalf("the fresh session of %s resumed %s, and a fresh session keeps no session at all", token, last.ResumeSessionFile)
	}
	return now
}

// controllerNotices is the reason of every provider-errors notice the daemon has published to the
// project's controller topic for issue, oldest first. The topic carries every claim's notice, so
// the issue is read off the notice's own summary line (noticeSummary).
func (r *claimCompactionLive) controllerNotices(issue string) []string {
	r.t.Helper()
	want := noticeSummary(record.ProviderErrorsNotice, issue)
	var reasons []string
	for _, published := range r.notices.acceptedOn(notify.ControllerTopic(r.project)) {
		if published.Message != want {
			continue
		}
		var payload struct {
			Kind   string `json:"kind"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(published.Payload), &payload); err != nil {
			r.t.Fatalf("decode the controller notice %q: %v", published.Payload, err)
		}
		reasons = append(reasons, payload.Reason)
	}
	return reasons
}

func (r *claimCompactionLive) awaitControllerNotices(issue string, count int) {
	r.t.Helper()
	r.await(fmt.Sprintf("%s's provider-errors notices to number %d", issue, count), 3*time.Minute, func() bool {
		return len(r.controllerNotices(issue)) >= count
	})
	if got := r.controllerNotices(issue); len(got) != count {
		r.t.Fatalf("the controller was told %d times about %s, want %d: %v", len(got), issue, count, got)
	}
}

// compactionEntry is a session's compaction record as this proof reads it back out of the pane's
// own transcript (omp://session.md, "compaction"): the summary text, the method Oh My Pi used,
// and whatever it preserved, which carries a snapcompact archive for a compaction that ran that
// method and nothing of the sort for one written as text.
type compactionEntry struct {
	Type         string                     `json:"type"`
	Summary      string                     `json:"summary"`
	Method       string                     `json:"method"`
	PreserveData map[string]json.RawMessage `json:"preserveData"`
}

func (e compactionEntry) preserveKeys() []string {
	keys := make([]string, 0, len(e.PreserveData))
	for key := range e.PreserveData {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// awaitCompactionEntry waits until the pane's transcript holds count compaction entries and
// answers the newest, holding that it is a text summary rather than snapcompact's images. The
// transcript is the session file the claim records, one JSON entry per line, which is how the
// plugins' own pane harness reads a session back too
// (packages/pi-shared/test/omp-harness.ts:915-926).
func (r *claimCompactionLive) awaitCompactionEntry(path string, count int) compactionEntry {
	t := r.t
	t.Helper()
	var entries []compactionEntry
	r.await(fmt.Sprintf("%s to hold %d compaction entries", path, count), time.Minute, func() bool {
		entries = readCompactionEntries(t, path)
		return len(entries) >= count
	})
	entry := entries[len(entries)-1]
	switch {
	case strings.TrimSpace(entry.Summary) == "":
		t.Fatalf("the compaction entry in %s carries no summary text: %+v", path, entry)
	case entry.Method == compactSnapcompactMethod:
		t.Fatalf("the compaction in %s ran %s, and the daemon's instructions are meant to keep it off that method", path, entry.Method)
	}
	if _, archived := entry.PreserveData[compactSnapcompactMethod]; archived {
		t.Fatalf("the compaction entry in %s preserved a snapcompact archive: %v", path, entry.preserveKeys())
	}
	return entry
}

// readCompactionEntries is every compaction entry of the transcript at path, oldest first.
func readCompactionEntries(t *testing.T, path string) []compactionEntry {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open the pane's transcript %s: %v", path, err)
	}
	defer file.Close()
	var entries []compactionEntry
	lines := bufio.NewScanner(file)
	lines.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for lines.Scan() {
		if !strings.Contains(lines.Text(), `"compaction"`) {
			continue
		}
		var entry compactionEntry
		if err := json.Unmarshal(lines.Bytes(), &entry); err != nil {
			continue // Not an entry of this shape; every line of the transcript is read here.
		}
		if entry.Type == "compaction" {
			entries = append(entries, entry)
		}
	}
	if err := lines.Err(); err != nil {
		t.Fatalf("read the pane's transcript %s: %v", path, err)
	}
	return entries
}

// paneSetting is what the Oh My Pi a pane runs answers for key, under that pane's own home,
// profile and PI_CONFIG_FILES: the setting as the pane itself resolves it, not as this file
// wrote it.
func (r *claimCompactionLive) paneSetting(token claim.Token, key string) json.RawMessage {
	t := r.t
	t.Helper()
	out, err := r.paneOmp(token, "config", "get", key, "--json").Output()
	if err != nil {
		t.Fatalf("read %s under %s's overlays: %v", key, token, err)
	}
	var answer struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(out, &answer); err != nil {
		t.Fatalf("decode `omp config get %s` %s: %v", key, out, err)
	}
	return answer.Value
}

// paneModel is one model the pane's Oh My Pi serves, as `omp models --json` lists it.
type paneModel struct {
	Selector string   `json:"selector"`
	Input    []string `json:"input"`
}

// paneModels is every model the pane's Oh My Pi has, which is the one its stand-in serves.
func (r *claimCompactionLive) paneModels(token claim.Token) []paneModel {
	t := r.t
	t.Helper()
	out, err := r.paneOmp(token, "models", "--json").Output()
	if err != nil {
		t.Fatalf("list the models under %s's overlays: %v", token, err)
	}
	var listing struct {
		Models []paneModel `json:"models"`
	}
	if err := json.Unmarshal(out, &listing); err != nil {
		t.Fatalf("decode `omp models --json` %s: %v", out, err)
	}
	return listing.Models
}

// paneOmp is one Oh My Pi command under a pane's own environment: the run's home and profile, and
// the claim's own PI_CONFIG_FILES in the order the runtime composes it.
func (r *claimCompactionLive) paneOmp(token claim.Token, args ...string) *exec.Cmd {
	cmd := exec.Command(r.omp, args...)
	cmd.Dir = r.t.TempDir()
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + r.home, "OMP_PROFILE=" + r.profile,
		"PI_CONFIG_FILES=" + strings.Join(r.overlays[token], ":"), "TMPDIR=" + os.Getenv("TMPDIR"),
	}
	return cmd
}

// ---- the Oh My Pi profile and the overlays -----------------------------------------------------------

// writeOmpProfile writes the profile every pane of this proof shares: the four stand-in
// providers, one model each, and the settings a pane needs to run unattended. Every model takes
// images, because snapcompact needs a model that reads them and the proof's proposition is that
// the compaction stays text all the same.
func (r *claimCompactionLive) writeOmpProfile() {
	t := r.t
	t.Helper()
	agent := filepath.Join(r.home, ".omp", "profiles", r.profile, "agent")
	if err := os.MkdirAll(agent, 0o700); err != nil {
		t.Fatalf("make the profile's agent directory %s: %v", agent, err)
	}
	var models, enabled strings.Builder
	models.WriteString("# Written by packages/daemon/internal/daemon/claim_compaction_live_test.go.\nproviders:\n")
	for _, seeded := range r.seededPanes() {
		stand := r.panes[r.token(seeded.issue, seeded.role)].stand
		// anthropic-messages, as provider_error_retry_live_test.go's overloaded stand-in is: a
		// `retry-after` over the client's own cap for a retry delay declines the retry there, so
		// one of the daemon's sends is one request and a turn that ends in a provider error ends
		// at once rather than after the host's own round of retries.
		fmt.Fprintf(&models, "  %s:\n    baseUrl: %s\n    api: anthropic-messages\n    apiKey: a-literal-key-no-endpoint-checks\n"+
			"    models:\n      - id: %s\n        name: %s\n        input: [text, image]\n        contextWindow: 200000\n        maxTokens: 8192\n",
			seeded.provider, stand.url, seeded.provider+standinModelID, seeded.provider)
		fmt.Fprintf(&enabled, "  - %s\n", standinModel(seeded.provider))
	}
	writeProofFile(t, filepath.Join(agent, "models.yml"), models.String())
	writeProofFile(t, filepath.Join(agent, "config.yml"),
		"# Written by packages/daemon/internal/daemon/claim_compaction_live_test.go.\n"+
			"enabledModels:\n"+enabled.String()+
			// A fresh profile opens Oh My Pi's setup wizard, and a pane waiting on it never starts.
			"startup:\n  setupWizard: false\n")
}

// writeOverlays writes the Oh My Pi overlays each pane is told through PI_CONFIG_FILES, in the
// shape a deployment renders them (docs/kubernetes.md, "Operator configuration"): the operator's
// own, which every pane names first (`runtime.tmux.pi_config_files`, or the pod environment's
// PI_CONFIG_FILES), then the claim's own (`role_config_files`), and nothing of Legion's after
// them — Legion writes no overlay at all.
//
// The operator's overlay carries two keys and names no compaction method:
//
//   - `retry.enabled: false`, so the host walks no fallback chain of its own and every failed
//     turn reaches the daemon, as provider_error_retry_live_test.go's does;
//   - `compaction.keepRecentTokens: 1`, because Oh My Pi keeps that many tokens of the newest
//     turns out of every summary and a session of a few turns has nothing left to summarize at
//     the shipped 20,000 (compactKeepRecentTokens).
//
// `compaction.methodOrder` is deliberately absent, so each pane runs Oh My Pi's own default,
// which puts the image-based `snapcompact` above every summary method. The claim's own overlay
// names the single model its stand-in serves for every model role (writeRoleOverlay), so no role
// of the session reaches a model this run does not answer for.
func (r *claimCompactionLive) writeOverlays(work string) map[claim.Token][]string {
	t := r.t
	t.Helper()
	dir := filepath.Join(work, "overlays")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	operator := filepath.Join(dir, "operator.yml")
	writeProofFile(t, operator, fmt.Sprintf(
		"# The operator's pane overlay (runtime.tmux.pi_config_files), which every pane names first.\n"+
			"retry:\n  enabled: false\ncompaction:\n  keepRecentTokens: %d\n", compactKeepRecentTokens))
	overlays := map[claim.Token][]string{}
	for _, seeded := range r.seededPanes() {
		path := filepath.Join(dir, string(seeded.role)+".yml")
		writeRoleOverlay(t, path, standinModel(seeded.provider))
		overlays[r.token(seeded.issue, seeded.role)] = []string{operator, path}
	}
	return overlays
}

// ---- the pane stand-ins ------------------------------------------------------------------------------

// turnAnswer is what a stand-in does with a turn's model request.
type turnAnswer int

const (
	// turnAnswers streams one short assistant message and ends the turn.
	turnAnswers turnAnswer = iota
	// turnErrors answers 529 with a `retry-after` over Oh My Pi's own cap for a retry delay, so
	// the host declines the retry and the status reaches the daemon untouched: one send is one
	// request, and the turn ends in a provider error the ladder waits out.
	turnErrors
	// turnFails answers 400 with an `invalid_request_error` body: a client error that is none of
	// 401, 403, 408 and 429, which fails the claim rather than going again.
	turnFails
	// turnHolds keeps the turn open until the step releases it, so the claim is working.
	turnHolds
)

// summaryAnswer is what a stand-in does with a compaction's summary request.
type summaryAnswer int

const (
	// summaryAnswers writes the summary at once.
	summaryAnswers summaryAnswer = iota
	// summaryHolds writes it after a delay of the step's choosing.
	summaryHolds
	// summaryRefuses answers 400, which Oh My Pi reports as `Summarization failed: 400 …` and
	// does not retry: a 5xx it retries until the daemon's own ten-minute bound runs out.
	summaryRefuses
)

// paneStandin is one pane's provider endpoint: it answers a turn's request and a compaction's
// summary request apart, over the stand-in both proofs share (live_agent_proof_test.go), which
// records each request. A step sets each behaviour before it drives the claim, and the two
// counts of its own are what a step reads back.
type paneStandin struct {
	*modelStandin

	mu         sync.Mutex
	turn       turnAnswer
	summary    summaryAnswer
	held       time.Duration
	release    chan struct{}
	turnBodies []string
	summaries  int
}

func newPaneStandin(t *testing.T, name string) *paneStandin {
	t.Helper()
	s := &paneStandin{modelStandin: &modelStandin{name: name}}
	s.serve(t, s)
	return s
}

func (s *paneStandin) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, ok := s.take(w, req)
	if !ok {
		return
	}
	// Only a Messages request is a turn or a summary. Oh My Pi asks the same base for a token
	// count of its own, which is neither, and is answered a count rather than counted here.
	if !strings.HasSuffix(req.URL.Path, "/v1/messages") {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"input_tokens":1000}`)
		return
	}
	if strings.Contains(body, compactSummaryMarker) {
		s.mu.Lock()
		s.summaries++
		mode, held := s.summary, s.held
		s.mu.Unlock()
		switch mode {
		case summaryHolds:
			time.Sleep(held)
		case summaryRefuses:
			writeInvalidRequest(w, anthropicRefusal("the summarizer refuses this request"))
			return
		case summaryAnswers:
		}
		streamAnthropicText(w, s.name, "The session so far, written as text: the task, the decisions and what is still open.")
		return
	}
	if isSideQuestion(body) {
		// The plugin's own `<btw>` question, asked beside the task's turn: it is answered as the
		// plugins' own pane harness answers it and counted as no turn of the task.
		streamAnthropicText(w, s.name, "PROCEEDING")
		return
	}
	s.mu.Lock()
	s.turnBodies = append(s.turnBodies, body)
	mode, release := s.turn, s.release
	s.mu.Unlock()
	switch mode {
	case turnErrors:
		writeOverloaded(w)
	case turnFails:
		writeInvalidRequest(w, anthropicRefusal("messages: unexpected role"))
	case turnHolds:
		if release != nil {
			<-release
		}
		streamAnthropicText(w, s.name, "HELD-TURN-OVER")
	case turnAnswers:
		streamAnthropicText(w, s.name, "ANSWERED")
	}
}

func (s *paneStandin) answerTurn() { s.setTurn(turnAnswers, nil) }
func (s *paneStandin) errorTurn()  { s.setTurn(turnErrors, nil) }
func (s *paneStandin) failTurn()   { s.setTurn(turnFails, nil) }
func (s *paneStandin) holdTurn()   { s.setTurn(turnHolds, make(chan struct{})) }

func (s *paneStandin) setTurn(turn turnAnswer, release chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turn, s.release = turn, release
}

// releaseTurn lets every held turn finish.
func (s *paneStandin) releaseTurn() {
	s.mu.Lock()
	release := s.release
	s.turn, s.release = turnAnswers, nil
	s.mu.Unlock()
	if release != nil {
		close(release)
	}
}

func (s *paneStandin) answerSummary() { s.setSummary(summaryAnswers, 0) }
func (s *paneStandin) refuseSummary() { s.setSummary(summaryRefuses, 0) }

// holdSummary sits on every summary answer for held before writing it.
func (s *paneStandin) holdSummary(held time.Duration) { s.setSummary(summaryHolds, held) }

func (s *paneStandin) setSummary(summary summaryAnswer, held time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.summary, s.held = summary, held
}

// turns is how many of the pane's turns the stand-in has answered, a compaction's summary
// requests excluded.
func (s *paneStandin) turns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.turnBodies)
}

// turnsHolding is how many of those turns carried marker, which is how many times the task that
// names it was put in front of the agent.
func (s *paneStandin) turnsHolding(marker string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	held := 0
	for _, body := range s.turnBodies {
		if strings.Contains(body, marker) {
			held++
		}
	}
	return held
}

// isSideQuestion is whether a Messages request is the plugin's own side question rather than a
// turn of the task: its last message carries the `<btw>` tag, which is how the plugins' own pane
// harness tells the two apart (packages/pi-shared/test/omp-harness.ts:528-531, `isSelfCheck`).
// Such a question rides beside the task's turns, so a proof that counts how often a task was put
// in front of the agent counts turns and not requests.
func isSideQuestion(body string) bool {
	var request struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal([]byte(body), &request) != nil || len(request.Messages) == 0 {
		return false
	}
	return strings.Contains(string(request.Messages[len(request.Messages)-1]), "<btw>")
}
