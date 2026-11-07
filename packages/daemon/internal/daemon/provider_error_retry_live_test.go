//go:build e2e

// The live proof of what a provider error does to a worker's task (dispatch://INFRA-956 R2) and
// of the operator's hand on the claims it leaves (R3), driven against real Oh My Pi panes under
// the real daemon. scripts/e2e/provider-error-retry.sh builds this driver with `-tags e2e` and
// owns the nats-server and the PostgreSQL it runs against.
//
// The daemon is the whole daemon: run, the function `legion start` calls, through this package's
// own whole-daemon harness (startDaemonWithin) over the live-proof fixture (live_proof_test.go),
// so the proof exercises the real configuration, the real store and its migrations, the real HTTP
// API, the real claim supervision and its worker stream, and the real workflow. Each worker is a
// real `omp --mode rpc` under the real `legion worker-shim`, started by localruntime_live_test.go
// — this pod carries no tmux, so the pane is a child process group instead of a window — with this
// checkout's `@sjawhar/pi-envoy` and `@sjawhar/pi-legion` installed into an Oh My Pi profile of the
// run's own. The agents register, report ready and take their tasks over the daemon's own routes
// and stream, as they do in production.
//
// What stands in is the model endpoint and nothing else. Three HTTP stand-ins of the test's own
// answer the panes' provider requests, over the stand-in and the overlay writers this proof
// shares with the compaction proof (live_agent_proof_test.go):
//
//   - overloaded: every request 529, with `retry-after: 3600`. The header is over the Oh My Pi
//     client's `maxRetryDelayMs` cap (60 s), which declines the retry and surfaces the original
//     status untouched (@oh-my-pi/pi-ai src/providers/anthropic-client.ts), so one send is one
//     request. Without it, the pane retries the same request eleven times over about fifty
//     seconds whatever `retry.enabled` and `retry.maxRetries` say, which would hide the ladder's
//     own gaps inside the host's.
//   - filtered: an OpenAI chat completion that streams text and then ends `finish_reason:
//     content_filter`, which Oh My Pi reports as the turn error `Provider finish_reason:
//     content_filter` with no HTTP status at all. The role's chain names one model, so no
//     fallback is left, and an error that carries no status is one the daemon sends the task
//     again for.
//   - malformed: every request 400 with an `invalid_request_error` body, which Oh My Pi reports
//     with status 400 — a client error that is none of 401, 403, 408 and 429 — so the claim fails and the
//     workflow holds the issue for its architect.
//
// Each pane's Oh My Pi overlay sets `retry.enabled: false` (PI_CONFIG_FILES, the key an operator's
// `runtime.tmux.pi_config_files` carries), so the host walks no fallback chain of its own and
// every failed turn reaches the daemon.
//
// How long it takes. The ladder's first ladderWaitedOut rungs are waited out where they are real:
// 30 s, 60 s and 120 s of wall clock, measured between the daemon's sends. Every rung above them
// is driven on the daemon's own clock (ladderDriven, ladderClock), because waiting the fourth and
// fifth out would add twelve minutes and measure the same ladder twice. With the pod's Oh My Pi
// the whole driver runs in about eight minutes, inside the twenty-minute bound
// scripts/e2e/lib/live-daemon-proof.sh gives it.
//
// What the daemon already holds when it boots. Each issue is seeded holding its tree's admission
// slot, in a worker phase, with that worker's claim mid-launch and the task that phase's start
// carried pending on the claim — because those two are what a start writes together (workflow's
// StartOrOwe, the relaunch row and the task row), so a slotted worker holding no task at all is
// not a state a start leaves. It is the state admission repairs: resumeSlottedWorkers
// (internal/admit/admit.go) gives any such claim its phase's resume task on the first pass after
// boot, and a seed with no pending delivery meets that repair before this driver delivers
// anything of its own, which answers every deliver 409 "a delivery is already pending". A
// pending delivery of the issue's own generation and phase is what the repair passes over
// (workflow.StartFor, the delivery's StillWorked), so the seed holds one.
//
// The ladder's claim is seeded with the task step 1 measures, and step 1 reads the daemon's own
// first send of it rather than making one. That also keeps the repair off the planner for the
// whole run: a provider error takes the task back unconfirmed (supervise's takeBackPending), so
// it is still worked at every rung and while step 5 leaves the claim stopped. The other two
// claims are seeded with a warm-up task their stand-in answers (providerStandin.arm), so each is
// idle and live — a state the repair passes over too — by the time its own step delivers the
// task that step drives, exactly as those steps always have.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strconv"
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
	"github.com/sjawhar/legion/daemon/internal/testbin"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// liveOmpPathEnv names the Oh My Pi every pane of this proof runs. It is the pod's own build,
// which is the fleet's pin rather than this branch's fetch, so the proof prints which build it
// ran. Unset, the driver skips rather than guess one, as the script refuses to start without it.
const liveOmpPathEnv = "LEGION_OMP_PATH"

// The three model stand-ins, by the provider name each is given in the run's models.yml.
const (
	overloadedProvider = "overloaded"
	filteredProvider   = "filtered"
	malformedProvider  = "malformed"
)

// ladderSends is how many times the daemon sends the ladder's task: the first send and five
// re-sends, so the sixth turn's error is the one that reaches providerErrorRunTold and tells the
// controller.
const ladderSends = 6

// ladderDelays are the waits the ladder arms before each re-send, in order (supervise's
// promptRetryDelay from attempt 1). Each send of step 1 reads the daemon's own armed wait back
// against the rung here, so a ladder that changed and left this list behind fails the proof
// rather than passing on the old numbers.
var ladderDelays = []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute}

// ladderWaitedOut is how many of the ladder's rungs the proof waits out where they are real: its
// first three. Every rung above them is driven (ladderDriven).
const ladderWaitedOut = 3

// ladderDrivenSpan is what a driven rung is armed for instead: short enough that the two rungs
// cost seconds rather than twelve minutes, long enough that the send is the timer's and not a
// race with the turn that armed it.
const ladderDrivenSpan = 2 * time.Second

// ladderDriven says whether wait is a rung of the ladder the proof drives on the daemon's clock
// rather than waits out, and the span it is armed for instead. The rungs are read off
// ladderDelays, so a ladder that grows a step or changes one is still shortened above
// ladderWaitedOut rather than leaving a literal duration behind that matches nothing.
func ladderDriven(wait time.Duration) (time.Duration, bool) {
	if slices.Contains(ladderDelays[ladderWaitedOut:], wait) {
		return ladderDrivenSpan, true
	}
	return 0, false
}

// ladderTolerance is how much longer than its armed wait a send-to-send gap may be: the turn whose
// error armed the wait runs inside the gap, and so does the agent's own start of the next one.
const ladderTolerance = 45 * time.Second

// providerErroredMessage is the daemon's own line for a turn that ended in a provider error
// (supervise's providerErrored), the record a step reads the reported fields off.
const providerErroredMessage = "supervise: the turn this task's prompt ran ended in a provider error; the task is kept and sent again"

// paneReady is how long a real pane is given to start Oh My Pi, load the two plugins, register and
// report ready.
const paneReady = 3 * time.Minute

// liveAgentProof is what a live proof that drives real Oh My Pi panes holds beyond liveProof, and
// the reads such a proof makes: the runtime that started each pane, the daemon's own log as a
// step reads it back, and the `legion` binary this checkout built, which is both the panes' shim
// and the operator's command line. providerErrorLive and claimCompactionLive
// (claim_compaction_live_test.go) are both one, so the two proofs read a claim, wait for a pane
// and run `legion claims` the same way.
type liveAgentProof struct {
	liveProof
	rt     *localRuntime
	daemon *recordingLog
	legion string
	omp    string
}

// providerErrorLive is one run of the proof: the booted daemon, the local runtime that started
// each pane, the daemon's own log as this driver reads it back, and the three issues it drives.
type providerErrorLive struct {
	liveAgentProof
	built *built
	clock *ladderClock
	// ladder, refused and failing are the three issues, each in the phase its one worker works.
	ladder, refused, failing string
	// stands is each stand-in by its provider name.
	stands map[string]*providerStandin
	// seeded is the three issues as the seed wrote them, built once in newProviderErrorLive so
	// the task each claim holds pending at boot and the marker that task asks for are the same
	// in the seed and in the step that reads them back (seededIssue).
	seeded []seededIssue
	// ladderTask is the task the planner holds pending from the moment the daemon boots, which
	// step 1 measures the ladder's sends of, the ladder keeps on the claim, and the operator's
	// retry in step 5 sends to the fresh session.
	ladderTask string
}

// TestAProviderErrorKeepsTheTaskAndTheOperatorRetriesWhatItLeavesLive drives the five steps in the
// order each one's state follows from the last: the ladder a 529 walks, the controller's notice at
// the sixth turn, a content filter's refusal that carries no status, an error that cannot clear,
// and the operator's commands over the claims the first and the fourth leave.
func TestAProviderErrorKeepsTheTaskAndTheOperatorRetriesWhatItLeavesLive(t *testing.T) {
	fixtureenv.Live(t)
	r := newProviderErrorLive(t)
	r.step("step1-a-529-keeps-the-task-and-walks-the-30-60-120-ladder", r.ladderWalks)
	r.step("step2-the-sixth-turn-in-a-row-tells-the-controller", r.controllerIsTold)
	r.step("step3-a-content-filter-carries-no-status-and-the-task-goes-again", r.refusalGoesAgain)
	r.step("step4-an-error-that-cannot-clear-fails-the-claim-and-holds-the-issue", r.cannotClearFails)
	r.step("step5-the-operator-stops-retries-and-reads-the-claims", r.operatorDrivesTheClaims)
}

// newProviderErrorLive readies everything a pane needs before the daemon boots: the `legion`
// binary and the two plugins built from this checkout, an Oh My Pi profile of the run's own routed
// to the three stand-ins, the three issues seeded with their phase worker mid-launch and that
// worker's task pending on its claim (seed), and the daemon booted over the local runtime.
func newProviderErrorLive(t *testing.T) *providerErrorLive {
	t.Helper()
	omp := os.Getenv(liveOmpPathEnv)
	if omp == "" {
		t.Skipf("%s is unset, so there is no Oh My Pi for a pane to run; scripts/e2e/provider-error-retry.sh requires it", liveOmpPathEnv)
	}
	takeOwnDatabase(t)
	proof := newLiveProof(t, "scripts/e2e/provider-error-retry.sh")
	r := &providerErrorLive{
		liveAgentProof: liveAgentProof{liveProof: proof, omp: omp},
		built:          &built{}, clock: &ladderClock{},
		ladder: proof.cfg.Project + "-1", refused: proof.cfg.Project + "-2", failing: proof.cfg.Project + "-3",
		stands: map[string]*providerStandin{},
	}
	r.seeded = []seededIssue{
		{key: r.ladder, title: "the issue whose provider is overloaded", phase: phase.Planning,
			role: claim.RolePlanner, provider: overloadedProvider, marker: "LADDER-" + randomSuffix(t)},
		{key: r.refused, title: "the issue whose turn a content filter refuses", phase: phase.Implementing,
			role: claim.RoleImplementer, provider: filteredProvider, marker: "WARM-IMPLEMENTER-" + randomSuffix(t)},
		{key: r.failing, title: "the issue whose provider request is malformed", phase: phase.Testing,
			role: claim.RoleTester, provider: malformedProvider, marker: "WARM-TESTER-" + randomSuffix(t)},
	}
	for i, seeded := range r.seeded {
		r.seeded[i].task = "Say the word " + seeded.marker + " and nothing else."
	}
	r.ladderTask = r.seededFor(r.ladder).task
	version, err := exec.Command(omp, "--version").Output()
	if err != nil {
		t.Fatalf("read the Oh My Pi version of %s: %v", omp, err)
	}
	r.note("Oh My Pi %s at %s (the pod's build, which is the fleet's pin and not this branch's fetch)",
		strings.TrimSpace(string(version)), omp)

	root := proofCheckoutRoot(t)
	work := t.TempDir()
	r.legion = buildProofLegion(t, root, work)
	home := filepath.Join(work, "omp-home")
	profile := "legion-provider-error-" + strings.ToLower(r.cfg.Project)
	installProofPlugins(t, root, work, home, profile, omp)
	for _, name := range []string{overloadedProvider, filteredProvider, malformedProvider} {
		r.stands[name] = newProviderStandin(t, name)
	}
	r.writeOmpProfile(home, profile)
	overlays := r.writeOverlays(work)

	r.rt = newLocalRuntime(t, r.cfg.StateDir, r.legion, omp, home, profile)
	r.rt.daemonURL, r.rt.envoyURL, r.rt.natsURLs = r.cfg.DaemonURL, newPaneListener(t), r.cfg.NatsURLs
	r.rt.overlays = func(token claim.Token) []string { return overlays[token] }

	r.seed()
	r.daemon = newRecordingLog(r.log())
	r.d = startDaemonWithin(t, r.cfg, r.overrides(), slog.New(r.daemon), 60*time.Second)
	r.registerController()
	return r
}

// registerController registers a controller session for the project. The daemon tells a
// controller nothing while none is registered (admit's wakeController), and the notice a run of
// provider errors earns is the one thing step 2 reads, so a session is registered here as `legion
// controller start` registers the operator's own (ControllerLaunchOperator). No controller process
// runs: the proof reads what the daemon published to the listener for it, never what an agent did
// with it.
func (r *providerErrorLive) registerController() {
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
	session := "ses_controller_" + randomSuffix(t)
	if status, body := r.d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: capability.Secret, SessionID: session, OmpSessionFile: "/sessions/" + session + ".jsonl",
		AgentID: session, PluginContract: api.DaemonAPIVersion,
	}, false); status != http.StatusOK {
		t.Fatalf("register the controller = %d %s, want 200", status, body)
	}
	r.note("the project's controller is registered as %s, so a notice for it is published rather than dropped", session)
}

// overrides is the daemon this proof boots: the local runtime over the real worker stream, the
// ladder's clock, the API listener the fixture holds, and the workflow's App tokens stood in for.
func (r *providerErrorLive) overrides() overrides {
	return overrides{
		runtime: func(_ context.Context, listener *stream.Listener, address string, _ appauth.Tokens,
			_ func(context.Context, string) (ghrepo.Repository, string, error), _ *store.Store, _ func() []ghrepo.Repository) (runtime.Runtime, error) {
			r.built.mu.Lock()
			defer r.built.mu.Unlock()
			r.built.conns, r.built.address = listener, address
			r.rt.conns, r.rt.stream = listener, address
			return r.rt, nil
		},
		clock:          r.clock,
		listen:         heldListen,
		workflowTokens: &workflowTokenRecorder{},
	}
}

// seed records the three issues as the daemon would hold them at the moment each one's worker
// began: a root handed to Legion, in the phase its worker works, holding its tree's admission slot
// with its lifecycle open, that worker's claim mid-launch, which this boot relaunches, and the
// task that phase's start left pending on the claim. The claim is how the proof reaches three
// live panes without an operator's spawn, and the pending task is what keeps admission's repair
// of a slot whose worker has no task off them: resumeSlottedWorkers (internal/admit/admit.go)
// would otherwise give each claim its phase's resume task on the first pass after boot, and
// every deliver this driver makes would be refused "a delivery is already pending". A delivery
// of the issue's own generation and phase, unconfirmed, is one the repair passes over
// (workflow.StartFor, the delivery's StillWorked), which is also what a real start leaves
// behind: the relaunch and the task are written together (workflow's StartOrOwe).
func (r *providerErrorLive) seed() {
	t := r.t
	t.Helper()
	ctx := context.Background()
	for _, seeded := range r.issues() {
		r.board.show(seeded.key, "in_progress")
	}
	if err := r.st.Tx(ctx, func(tx pgx.Tx) error {
		for _, seeded := range r.issues() {
			if err := r.records.PutIssue(ctx, tx, record.Issue{
				Key: seeded.key, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Tree: seeded.key,
				Project: r.cfg.Project, Title: seeded.title, Phase: seeded.phase, Generation: 1, Status: "in_progress",
				DispatchStatus: "in_progress", Rank: "U", LastDispatchSeq: 1, HandedOver: true,
			}); err != nil {
				return err
			}
		}
		return r.admit(ctx, tx, r.ladder, r.refused, r.failing)
	}); err != nil {
		t.Fatalf("seed the issues: %v", err)
	}
	for _, seeded := range r.issues() {
		if _, err := r.st.OpenTreeLifecycle(ctx, r.project, seeded.key, treelifecycle.AuthorityWorkflow); err != nil {
			t.Fatalf("open the lifecycle of %s: %v", seeded.key, err)
		}
		token := r.token(seeded.key, seeded.role)
		if _, err := r.st.AdmitClaim(ctx, supervise.Claim{
			Token: token, Project: r.project, Tree: seeded.key, Issue: seeded.key, Role: seeded.role,
			Generation: 1, State: supervise.StateLaunching,
			BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)), ServingGeneration: 1,
		}); err != nil {
			t.Fatalf("record the unfinished launch of %s: %v", token, err)
		}
		// Unconfirmed and unread: the launch this seed interrupts never put the task in front of
		// an agent, so the relaunched pane is sent it once it reports ready.
		if err := r.st.PutDelivery(ctx, token, supervise.Delivery{
			ID: "seeded-start-" + randomSuffix(t), Task: seeded.task, Generation: 1,
			Phase: seeded.phase, QueuedAt: time.Now(),
		}); err != nil {
			t.Fatalf("record the task %s's start left pending: %v", token, err)
		}
		writePrompt(t, r.cfg, token)
	}
	r.note("seeded %s (%s, %s), %s (%s, %s) and %s (%s, %s), each with its worker mid-launch holding the task "+
		"its phase's start left pending, which is the state admission's repair of a slotted worker passes over",
		r.ladder, phase.Planning, overloadedProvider, r.refused, phase.Implementing, filteredProvider,
		r.failing, phase.Testing, malformedProvider)
}

// seededIssue is one of the proof's three issues: its key, the phase it stands in, the role that
// works that phase, the stand-in that role's pane talks to, the task the seed leaves pending on
// that role's claim, and the word that task asks for, which tells this claim's turns from every
// other request its stand-in answers.
type seededIssue struct {
	key      string
	title    string
	phase    phase.Phase
	role     claim.Role
	provider string
	task     string
	marker   string
}

// issues is the three issues as newProviderErrorLive built them.
func (r *providerErrorLive) issues() []seededIssue { return r.seeded }

// seededFor is the seeded issue of key, and fails the run when the key is none of the three.
func (r *providerErrorLive) seededFor(key string) seededIssue {
	r.t.Helper()
	for _, seeded := range r.issues() {
		if seeded.key == key {
			return seeded
		}
	}
	r.t.Fatalf("this proof seeds no issue %s", key)
	return seededIssue{}
}

// token is the claim token of role on key, as the daemon spells it.
func (r *providerErrorLive) token(key string, role claim.Role) claim.Token {
	r.t.Helper()
	token, err := claim.NewToken(r.project, key, role)
	if err != nil {
		r.t.Fatal(err)
	}
	return token
}

// ---- the steps ---------------------------------------------------------------------------------

// ladderWalks drives the first half of R2: the task a 529 takes back stays on the claim, goes to
// the same session again after 30 s, 60 s and 120 s, and the claim neither fails nor loses it.
//
// The task is the one the planner's claim has held pending since the daemon booted, which is
// what its phase's start left there (seed), so the first send is the daemon's own and this step
// waits for it rather than delivering a task of its own. It is read off the delivery's read
// mark, the instant the shim acknowledged that first prompt, which a provider error keeps
// (supervise's takeBackPending) — so every gap below is measured from the send that began the
// ladder and not from a later rung.
func (r *providerErrorLive) ladderWalks() {
	t := r.t
	token := r.token(r.ladder, claim.RolePlanner)
	r.awaitReady(token)
	marker, task := r.seededFor(r.ladder).marker, r.ladderTask
	sent := []time.Time{r.awaitFirstSend(token, task)}
	r.note("the daemon held the task %q for %s from the moment it booted and sent it at %s",
		task, token, sent[0].Format(time.RFC3339))

	for attempt := 1; attempt < ladderSends; attempt++ {
		delay := ladderDelays[attempt-1]
		within := delay + ladderTolerance
		if driven, ok := ladderDriven(delay); ok {
			within = driven + ladderTolerance
		}
		r.awaitLog("the ladder's send "+strconv.Itoa(attempt+1), within+time.Minute, func(record loggedRecord) bool {
			return record.Message == "supervise: the wait after a provider error is over; the task goes again" &&
				record.at.After(sent[attempt-1])
		})
		armed := r.lastLog(func(record loggedRecord) bool {
			return record.Message == "supervise: this provider error can clear; the task goes to the same session again after a wait" &&
				record.Attrs["attempt"] == float64(attempt)
		})
		if armed == nil {
			t.Fatalf("the daemon armed no wait for send %d of the ladder", attempt+1)
		}
		if wait, want := armed.Attrs["wait"], delay.String(); wait != want {
			t.Fatalf("the ladder armed %v before send %d, want %s", wait, attempt+1, want)
		}
		now := time.Now()
		gap := now.Sub(sent[attempt-1])
		sent = append(sent, now)
		if driven, ok := ladderDriven(delay); ok {
			r.note("send %d came %s after send %d; its %s wait was driven on the daemon's clock to %s",
				attempt+1, gap.Round(time.Second), attempt, delay, driven)
			continue
		}
		if gap < delay || gap > delay+ladderTolerance {
			t.Fatalf("send %d of the ladder came %s after send %d, want between %s and %s",
				attempt+1, gap.Round(time.Second), attempt, delay, delay+ladderTolerance)
		}
		r.note("send %d came %s after send %d, and the daemon had armed %s (cause %q)",
			attempt+1, gap.Round(time.Second), attempt, delay, armed.Attrs["cause"])
	}

	errors := r.logs(func(record loggedRecord) bool {
		return record.Message == providerErroredMessage &&
			record.Attrs["httpStatus"] == float64(529)
	})
	if len(errors) < ladderSends-1 {
		t.Fatalf("the daemon read %d turns ending in a 529, want at least %d", len(errors), ladderSends-1)
	}
	r.note("every turn ended in a 529 the pane reported retryable=%v on model %v",
		errors[0].Attrs["retryable"], errors[0].Attrs["model"])

	held := r.claim(token)
	if held.Pending == nil || held.Pending.Task != task {
		t.Fatalf("%s holds pending %+v, want the task it was given", token, held.Pending)
	}
	if held.State == string(supervise.StateFailed) {
		t.Fatalf("%s failed while the ladder ran: %+v", token, held)
	}
	if issue := r.issue(r.ladder); issue.Phase == phase.Held {
		t.Fatalf("%s was held while the ladder ran: %+v", r.ladder, issue)
	}
	r.note("%s is %s, still holding its task (%d re-sends counted on the delivery), and %s is %s",
		token, held.State, ladderSends-1, r.ladder, r.issue(r.ladder).Phase)
	if got := r.stands[overloadedProvider].requestsHolding(marker); got < ladderSends {
		t.Fatalf("the overloaded stand-in was asked %d times for a request carrying the task, want %d", got, ladderSends)
	}
	r.note("the overloaded stand-in answered 529 to %d requests, %d of them carrying the task itself",
		r.stands[overloadedProvider].count(), r.stands[overloadedProvider].requestsHolding(marker))
}

// controllerIsTold reads the one notice the run of provider errors earns: at the sixth turn in a
// row, the project's controller is told, once, and nothing is held.
func (r *providerErrorLive) controllerIsTold() {
	t := r.t
	topic := notify.ControllerTopic(r.project)
	var told []listenerPublish
	r.await("the controller's notice of a run of provider errors", 3*time.Minute, func() bool {
		told = nil
		for _, published := range r.notices.acceptedOn(topic) {
			var payload struct {
				Kind   string `json:"kind"`
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal([]byte(published.Payload), &payload); err != nil {
				t.Fatalf("decode the controller notice %q: %v", published.Payload, err)
			}
			if payload.Kind == string(record.ProviderErrorsNotice) {
				published.Payload = payload.Reason
				told = append(told, published)
			}
		}
		return len(told) == 1
	})
	r.note("the controller was told once on %s: %q", topic, told[0].Payload)
	if !strings.Contains(told[0].Payload, strconv.Itoa(ladderSends)+" turns in a row") {
		t.Fatalf("the controller's notice does not name the run of %d turns: %q", ladderSends, told[0].Payload)
	}
	if issue := r.issue(r.ladder); issue.Phase == phase.Held {
		t.Fatalf("%s was held by the run of provider errors: %+v", r.ladder, issue)
	}
	r.note("%s is still %s: the notice is the one exit from a run, and no attempt limit holds the issue",
		r.ladder, r.issue(r.ladder).Phase)
}

// refusalGoesAgain drives the second half of R2: a turn a content filter ended carries no HTTP
// status, so the task goes to the same session again after 30 s and nobody is told to hold the
// issue.
//
// The claim answers the warm-up task its seed left pending first, so it is idle and holds
// nothing when this step gives it the task whose turns the filter refuses, and the stand-in is
// armed only once that is true: the refusals this step reads are this task's and no other's.
func (r *providerErrorLive) refusalGoesAgain() {
	t := r.t
	token := r.token(r.refused, claim.RoleImplementer)
	r.awaitReady(token)
	r.awaitWarmedUp(r.refused)
	r.stands[filteredProvider].arm()
	marker := "FILTERED-" + randomSuffix(t)
	task := "Say the word " + marker + " and nothing else."
	// The log is marked before the task is given: the daemon errors and decides inside a few
	// milliseconds of each other, so a mark taken after the first record would miss the decision
	// that belongs to it and read the ladder's next step instead.
	from := r.daemon.count()
	status, body := r.d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/deliver",
		api.DeliverRequest{Task: task}, true)
	if status != http.StatusOK {
		t.Fatalf("deliver the refused task = %d %s, want 200", status, body)
	}
	sent := time.Now()
	errored := r.awaitLogFrom("the turn a content filter ended", from, 4*time.Minute, func(record loggedRecord) bool {
		return record.Message == providerErroredMessage &&
			record.Attrs["claim"] == string(token)
	})
	// A refusal carries no HTTP status at all, and an error with no status is one the daemon
	// sends the task again for.
	if errored.Attrs["retryable"] != false || errored.Attrs["httpStatus"] != float64(0) {
		t.Fatalf("the refused turn was reported retryable=%v httpStatus=%v, want a plain refusal",
			errored.Attrs["retryable"], errored.Attrs["httpStatus"])
	}
	armed := r.awaitLogFrom("the refusal's wait", from, time.Minute, func(record loggedRecord) bool {
		return record.Message == "supervise: this provider error can clear; the task goes to the same session again after a wait" &&
			record.Attrs["claim"] == string(token)
	})
	if armed.Attrs["wait"] != ladderDelays[0].String() {
		t.Fatalf("the refusal armed %v, want %s", armed.Attrs["wait"], ladderDelays[0])
	}
	// The cause names the status or nothing: the daemon builds it from structured fields alone,
	// never from a provider's text, and this turn carries no status.
	cause, _ := armed.Attrs["cause"].(string)
	if cause != "a provider error" {
		t.Fatalf("the refusal's cause is %q, want the words an error with no status gives", cause)
	}
	r.note("the content filter ended the turn with no HTTP status and the daemon armed %v: %q",
		armed.Attrs["wait"], cause)

	r.awaitLogFrom("the refused task sent again", from, ladderDelays[0]+ladderTolerance+time.Minute, func(record loggedRecord) bool {
		return record.Message == "supervise: the wait after a provider error is over; the task goes again" &&
			record.Attrs["claim"] == string(token)
	})
	gap := time.Since(sent)
	if gap < ladderDelays[0] {
		t.Fatalf("the refused task went again %s after it was first sent, want at least %s", gap.Round(time.Second), ladderDelays[0])
	}
	held := r.claim(token)
	if held.Pending == nil || held.Pending.Task != task {
		t.Fatalf("%s holds pending %+v, want the task it was given", token, held.Pending)
	}
	issue := r.issue(r.refused)
	if issue.Phase != phase.Implementing {
		t.Fatalf("%s is %s, want it still implementing and held for nobody", r.refused, issue.Phase)
	}
	if notices := r.architectNotices(r.refused, "held"); len(notices) != 0 {
		t.Fatalf("%s's architect was told %d held notices, want none", r.refused, len(notices))
	}
	r.note("the task went again %s after the first send, %s still holds it, %s is %s and its architect was told nothing",
		gap.Round(time.Second), token, r.refused, issue.Phase)
}

// cannotClearFails drives the last of R2: an HTTP 400, a client error that is none of 401, 403,
// 408 and 429, fails the claim, and the workflow holds the issue for its architect.
//
// As in step 3, the claim answers the warm-up task its seed left pending before the stand-in is
// armed, so the malformed request this step reads is the one its own task ran into.
func (r *providerErrorLive) cannotClearFails() {
	t := r.t
	token := r.token(r.failing, claim.RoleTester)
	r.awaitReady(token)
	r.awaitWarmedUp(r.failing)
	r.stands[malformedProvider].arm()
	marker := "MALFORMED-" + randomSuffix(t)
	task := "Say the word " + marker + " and nothing else."
	from := r.daemon.count()
	status, body := r.d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/deliver",
		api.DeliverRequest{Task: task}, true)
	if status != http.StatusOK {
		t.Fatalf("deliver the malformed task = %d %s, want 200", status, body)
	}
	errored := r.awaitLogFrom("the turn a malformed request ended", from, 4*time.Minute, func(record loggedRecord) bool {
		return record.Message == providerErroredMessage &&
			record.Attrs["claim"] == string(token)
	})
	if errored.Attrs["retryable"] != false || errored.Attrs["httpStatus"] != float64(400) {
		t.Fatalf("the malformed turn was reported retryable=%v httpStatus=%v, want retryable=false and 400",
			errored.Attrs["retryable"], errored.Attrs["httpStatus"])
	}
	var failed api.OperatorClaim
	r.await("the claim to fail", 4*time.Minute, func() bool {
		failed = r.claim(token)
		return failed.State == string(supervise.StateFailed)
	})
	r.note("the turn ended in HTTP 400 and %s is %s", token, failed.State)
	var issue api.Issue
	r.await(r.failing+" to be held for its architect", 2*time.Minute, func() bool {
		issue = r.issue(r.failing)
		return issue.Phase == phase.Held
	})
	notices := r.waitArchitectNotices(r.failing, "held", 1)
	r.note("%s is %s and its architect was told once: %q", r.failing, issue.Phase, notices[0].Message)
}

// operatorDrivesTheClaims drives R3 with the `legion` binary this checkout built: the stop of a
// phase worker that is still working, its retry in a fresh session, and the same retry of the
// claim the malformed provider failed.
func (r *providerErrorLive) operatorDrivesTheClaims() {
	t := r.t
	planner := r.token(r.ladder, claim.RolePlanner)
	before := r.claim(planner)
	r.note("`legion claims stop` on %s, which is %s in session %s", planner, before.State, before.Session)
	r.note("%s", r.legionClaims("stop", "--claim", string(planner)))
	r.await(string(planner)+" to retire", 2*time.Minute, func() bool {
		return r.claim(planner).State == string(supervise.StateRetired)
	})
	r.note("`legion claims list` now reads:\n%s", indentOutput(r.legionClaims("list")))

	r.note("%s", r.legionClaims("retry", "--claim", string(planner), "--fresh-session"))
	// The stopped planner still holds the task its ladder kept, so the fresh session is sent that
	// task, and the restart's own phase task waits in the outbox behind it ("a delivery is already
	// pending") until the kept one is retired. That is R2 and R3 together: a provider error takes
	// the task away from nobody, and neither does a stop and a retry.
	sent := r.awaitFreshSession(planner, before.Session)
	if sent != r.ladderTask {
		t.Fatalf("the planner's fresh session was sent %q, want the task its ladder kept, %q", sent, r.ladderTask)
	}

	failing := r.token(r.failing, claim.RoleTester)
	stale := r.claim(failing)
	r.note("`legion claims retry --fresh-session` on %s, which is %s in session %s", failing, stale.State, stale.Session)
	r.note("%s", r.legionClaims("retry", "--claim", string(failing), "--fresh-session"))
	r.awaitFreshSession(failing, stale.Session)
	if issue := r.issue(r.failing); issue.Phase != phase.Testing {
		t.Fatalf("%s is %s after its worker's retry, want it back in testing", r.failing, issue.Phase)
	}
	r.note("%s left its hold and is %s again", r.failing, r.issue(r.failing).Phase)
	r.note("`legion claims list` at the end reads:\n%s", indentOutput(r.legionClaims("list")))
}

// awaitFreshSession waits until token runs in a session other than old, having been relaunched
// with no recorded session to resume, and answers the task that new session was sent.
func (r *providerErrorLive) awaitFreshSession(token claim.Token, old string) string {
	t := r.t
	t.Helper()
	var now api.OperatorClaim
	r.await(string(token)+" to run in a session of its own", paneReady, func() bool {
		now = r.claim(token)
		return now.Session != "" && now.Session != old &&
			(now.State == string(supervise.StateReady) || now.State == string(supervise.StateIdle) ||
				now.State == string(supervise.StateWorking))
	})
	launches := r.rt.LaunchesOf(token)
	last := launches[len(launches)-1]
	if last.ResumeSessionFile != "" {
		t.Fatalf("the retry of %s resumed %s, and --fresh-session asked for a new session", token, last.ResumeSessionFile)
	}
	var sent string
	r.await("the task "+string(token)+"'s new session was sent", 2*time.Minute, func() bool {
		held := r.claim(token)
		if held.Pending == nil {
			return false
		}
		sent = held.Pending.Task
		return sent != ""
	})
	r.note("%s was relaunched with no session to resume, registered %s (was %s), and that session was sent: %q",
		token, now.Session, old, sent)
	return sent
}

// awaitFirstSend waits until the claim's pending task is task and the daemon has put it in front
// of its agent, and answers the instant it did: the delivery's read mark, which the shim's
// acknowledgement of that first prompt set. A provider error keeps the mark while it takes the
// task back (supervise's takeBackPending), so the instant answered here stays the first send's
// however far the ladder climbs afterwards.
func (r *providerErrorLive) awaitFirstSend(token claim.Token, task string) time.Time {
	r.t.Helper()
	var at time.Time
	r.await(string(token)+" to be sent the task its start left pending", paneReady, func() bool {
		held := r.claim(token)
		if held.Pending == nil || held.Pending.Task != task || held.Pending.DeliveredAt == nil {
			return false
		}
		at = *held.Pending.DeliveredAt
		return true
	})
	return at
}

// awaitWarmedUp waits until the claim has answered the warm-up task its seed left pending and
// holds nothing (seed). Only then is the claim's next task this step's own, and only then is a
// deliver of it taken: the daemon refuses a second task while one is pending. A claim idle with
// no task is also one admission's repair of a slotted worker passes over, since the repair
// starts no phase whose claim is in a live state (workflow.StartFor).
func (r *providerErrorLive) awaitWarmedUp(issue string) {
	r.t.Helper()
	seeded := r.seededFor(issue)
	token := r.token(seeded.key, seeded.role)
	r.await(string(token)+" to answer the warm-up task its start left pending", paneReady, func() bool {
		held := r.claim(token)
		return held.Pending == nil && held.State != string(supervise.StateWorking)
	})
	if got := r.stands[seeded.provider].requestsHolding(seeded.marker); got == 0 {
		r.t.Fatalf("the %s stand-in answered no request carrying %s's warm-up task", seeded.provider, token)
	}
	r.note("%s answered the warm-up task its start left pending (%q) and holds nothing, so what it is given next is this step's",
		token, seeded.task)
}

// ---- reads a proof that drives real panes makes ---------------------------------------------------

// claim is token as the operator's claim listing shows it now.
func (r *liveAgentProof) claim(token claim.Token) api.OperatorClaim {
	r.t.Helper()
	status, body := r.d.request(http.MethodGet, "/legion/v1/operator/claims", nil, true)
	if status != http.StatusOK {
		r.t.Fatalf("the operator's claim listing = %d %s", status, body)
	}
	var list api.OperatorClaims
	if err := json.Unmarshal(body, &list); err != nil {
		r.t.Fatalf("decode the claim listing %s: %v", body, err)
	}
	for _, held := range list.Claims {
		if held.Token == token {
			return held
		}
	}
	r.t.Fatalf("the daemon holds no claim %s; it holds %d", token, len(list.Claims))
	return api.OperatorClaim{}
}

// awaitReady waits for the claim's real pane to start Oh My Pi, register and report ready. A claim
// that fails meanwhile ends the wait at once, naming the log the worker's own output is in: a pane
// that cannot start is read from there and nowhere else.
func (r *liveAgentProof) awaitReady(token claim.Token) {
	r.t.Helper()
	var held api.OperatorClaim
	r.await(string(token)+"'s pane to register and report ready", paneReady, func() bool {
		held = r.claim(token)
		if held.State == string(supervise.StateFailed) {
			r.t.Fatalf("%s failed before its pane reported ready; its workers' output is under %s",
				token, r.rt.paneLogs)
		}
		return held.State == string(supervise.StateReady) || held.State == string(supervise.StateIdle)
	})
	r.note("%s is %s in session %s, plugin contract %d, process %s",
		token, held.State, held.Session, held.PluginContract, held.Locator.Label())
}

// await polls until want holds, failing with what it waited for. testwait.Eventually bounds its
// own wait at a minute, which is shorter than the ladder's own gaps.
func (r *liveAgentProof) await(what string, within time.Duration, want func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(within)
	for {
		if want() {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("timed out after %s waiting for %s", within, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// awaitLog waits for the daemon to write a log record matching want, and answers the first one it
// wrote after the wait began.
func (r *liveAgentProof) awaitLog(what string, within time.Duration, want func(loggedRecord) bool) *loggedRecord {
	r.t.Helper()
	return r.awaitLogFrom(what, r.daemon.count(), within, want)
}

// awaitLogFrom is awaitLog from a mark the step took itself, before whatever it drove: the daemon
// errors and decides inside milliseconds of each other, so a step that must read both takes one
// mark ahead of the act rather than a mark per read.
func (r *liveAgentProof) awaitLogFrom(what string, from int, within time.Duration, want func(loggedRecord) bool) *loggedRecord {
	r.t.Helper()
	var found *loggedRecord
	r.await(what, within, func() bool {
		found = r.daemon.firstAfter(from, want)
		return found != nil
	})
	return found
}

// logs is every record the daemon has written that matches want, oldest first.
func (r *liveAgentProof) logs(want func(loggedRecord) bool) []loggedRecord {
	return r.daemon.matching(want)
}

// lastLog is the newest record matching want, and nil when there is none.
func (r *liveAgentProof) lastLog(want func(loggedRecord) bool) *loggedRecord {
	matched := r.daemon.matching(want)
	if len(matched) == 0 {
		return nil
	}
	return &matched[len(matched)-1]
}

// legionClaims runs `legion claims <args>` — the binary this checkout built — against the booted
// daemon, and answers its output. A non-zero exit fails the step with both streams.
func (r *liveAgentProof) legionClaims(args ...string) string {
	r.t.Helper()
	out, err := r.legionClaimsTry(args...)
	if err != nil {
		r.t.Fatalf("legion claims %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// legionClaimsTry is legionClaims for a command a step expects the daemon to refuse: it answers
// both streams and the exit error rather than ending the step, since the refusal is what the
// step reads (claim_compaction_live_test.go).
func (r *liveAgentProof) legionClaimsTry(args ...string) (string, error) {
	r.t.Helper()
	full := append([]string{"claims", args[0], "--port", strconv.Itoa(r.cfg.Port),
		"--operator-token-file", r.cfg.OperatorTokenFile}, args[1:]...)
	cmd := exec.Command(r.legion, full...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + r.t.TempDir()}
	out, err := cmd.CombinedOutput()
	return strings.TrimRight(string(out), "\n"), err
}

// indent puts each line of a command's output under a step's own note.
func indentOutput(text string) string {
	return "   " + strings.ReplaceAll(text, "\n", "\n   ")
}

// ---- what the run builds before the daemon boots -------------------------------------------------

// takeOwnDatabase points LEGION_TEST_PG_DSN at a database this run makes for itself on the server
// that variable names, so the fixture migrates into a database of its own rather than into the one
// the DSN names. The daemon's live proofs share the named database on a CI service, whose role
// owns it; in a Legion worker pod the server's `legion_test` role may create databases and may not
// write the `public` schema of `postgres`, which a migration answers `permission denied for schema
// public` (SQLSTATE 42501). The database is dropped at the end of the run; that role also cannot
// terminate another backend, so a drop a connection outlives is reported and not a failure, and the
// database goes with the pod.
func takeOwnDatabase(t *testing.T) {
	t.Helper()
	named := os.Getenv("LEGION_TEST_PG_DSN")
	if named == "" {
		return // newLiveProof's own testDSN skips the test, naming the variable.
	}
	ctx := context.Background()
	server, err := pgx.Connect(ctx, named)
	if err != nil {
		t.Fatalf("connect the PostgreSQL LEGION_TEST_PG_DSN names: %v", err)
	}
	database := "legion_provider_error_" + strings.ToLower(randomSuffix(t))
	if _, err := server.Exec(ctx, "create database "+database+" strategy file_copy"); err != nil {
		server.Close(ctx)
		t.Fatalf("make this run's own database %s: %v", database, err)
	}
	if err := server.Close(ctx); err != nil {
		t.Fatalf("close the connection the database was made on: %v", err)
	}
	own, err := url.Parse(named)
	if err != nil {
		t.Fatalf("read LEGION_TEST_PG_DSN as a URL: %v", err)
	}
	own.Path = "/" + database
	t.Setenv("LEGION_TEST_PG_DSN", own.String())
	fmt.Printf("   this run migrates into its own database %s on the server LEGION_TEST_PG_DSN names\n", database)
	t.Cleanup(func() {
		drop, err := pgx.Connect(context.Background(), named)
		if err != nil {
			t.Logf("the run's own database %s was left behind: %v", database, err)
			return
		}
		defer drop.Close(context.Background())
		if _, err := drop.Exec(context.Background(), "drop database "+database); err != nil {
			t.Logf("the run's own database %s was left behind: %v", database, err)
		}
	})
}

// proofCheckoutRoot is the repository this driver was built from, found from this file's own
// compiled path: the driver runs from a temporary directory, so nothing about its working
// directory names the checkout.
func proofCheckoutRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("locate this driver's source")
	}
	root, err := filepath.Abs(filepath.Join(filepath.Dir(source), "..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.work")); err != nil {
		t.Fatalf("%s is not the checkout root: %v", root, err)
	}
	return root
}

// buildLegion builds the `legion` every pane's shim and every operator command in this proof is,
// from this checkout.
func buildProofLegion(t *testing.T, root, work string) string {
	t.Helper()
	binary := filepath.Join(work, "legion")
	build := exec.Command("go", "build", "-o", binary, "./cmd/legion")
	build.Dir = filepath.Join(root, "packages", "daemon")
	build.Env = append(os.Environ(), "GOFLAGS=")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build legion from %s: %v\n%s", build.Dir, err, out)
	}
	return binary
}

// installPlugins packs this checkout's two Oh My Pi plugins as the release packs them
// (scripts/e2e/lib/pack-plugin.sh) and installs each into the run's own profile under its own
// HOME, as scripts/e2e/lib/install-plugin-profile.sh does for a stage proof — through the Oh My Pi
// this proof's panes run rather than a mise pin, since a pod has no mise. The profile's natives are
// linked from the shared cache, so no pane downloads a native module.
func installProofPlugins(t *testing.T, root, work, home, profile, omp string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, ".omp"), 0o700); err != nil {
		t.Fatalf("make the run's Oh My Pi home %s: %v", home, err)
	}
	testbin.OMPHome(t, omp, home)
	for _, pkg := range []string{"pi-envoy", "pi-legion"} {
		packed := filepath.Join(work, "pack-"+pkg)
		pack := exec.Command("bash", filepath.Join(root, "scripts", "e2e", "lib", "pack-plugin.sh"), pkg, packed)
		pack.Dir = root
		tarball, err := pack.Output()
		if err != nil {
			t.Fatalf("pack %s: %v", pkg, err)
		}
		dest := filepath.Join(work, pkg)
		if err := os.MkdirAll(dest, 0o700); err != nil {
			t.Fatal(err)
		}
		unpack := exec.Command("tar", "xzf", strings.TrimSpace(string(tarball)), "-C", dest, "--strip-components=1")
		if out, err := unpack.CombinedOutput(); err != nil {
			t.Fatalf("unpack %s: %v\n%s", pkg, err, out)
		}
		install := exec.Command(omp, "plugin", "install", dest)
		install.Dir = dest
		install.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "OMP_PROFILE=" + profile}
		if out, err := install.CombinedOutput(); err != nil {
			t.Fatalf("install %s into the profile %s: %v\n%s", pkg, profile, err, out)
		}
	}
	list := exec.Command(omp, "plugin", "list", "--json")
	list.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "OMP_PROFILE=" + profile}
	out, err := list.Output()
	if err != nil {
		t.Fatalf("read the profile's plugins: %v", err)
	}
	var listing struct {
		NPM []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
			Enabled bool   `json:"enabled"`
		} `json:"npm"`
	}
	if err := json.Unmarshal(out, &listing); err != nil {
		t.Fatalf("decode the plugin listing %s: %v", out, err)
	}
	var installed []string
	for _, plugin := range listing.NPM {
		if plugin.Enabled {
			installed = append(installed, plugin.Name+"@"+plugin.Version)
		}
	}
	if len(installed) != 2 {
		t.Fatalf("the profile %s has %v enabled, want this checkout's pi-envoy and pi-legion", profile, installed)
	}
	fmt.Printf("   plugins %s in Oh My Pi profile %s under %s\n", strings.Join(installed, " and "), profile, home)
}

// writeOmpProfile writes the profile every pane shares: the three stand-in providers, the models
// they serve, and the settings a pane needs to run unattended. The model each role uses is its own
// overlay's (writeOverlays).
func (r *providerErrorLive) writeOmpProfile(home, profile string) {
	t := r.t
	t.Helper()
	agent := filepath.Join(home, ".omp", "profiles", profile, "agent")
	if err := os.MkdirAll(agent, 0o700); err != nil {
		t.Fatalf("make the profile's agent directory %s: %v", agent, err)
	}
	var models, enabled strings.Builder
	models.WriteString("# Written by packages/daemon/internal/daemon/provider_error_retry_live_test.go.\nproviders:\n")
	for _, name := range []string{overloadedProvider, filteredProvider, malformedProvider} {
		api := "anthropic-messages"
		base := r.stands[name].url
		if name == filteredProvider {
			api, base = "openai-completions", base+"/v1"
		}
		fmt.Fprintf(&models, "  %s:\n    baseUrl: %s\n    api: %s\n    apiKey: a-literal-key-no-endpoint-checks\n"+
			"    models:\n      - id: %s\n        name: %s\n", name, base, api, name+"-model", name)
		fmt.Fprintf(&enabled, "  - %s\n", standinModel(name))
	}
	writeProofFile(t, filepath.Join(agent, "models.yml"), models.String())
	writeProofFile(t, filepath.Join(agent, "config.yml"),
		"# Written by packages/daemon/internal/daemon/provider_error_retry_live_test.go.\n"+
			"enabledModels:\n"+enabled.String()+
			// A fresh profile opens Oh My Pi's setup wizard, and a pane waiting on it never starts.
			"startup:\n  setupWizard: false\n")
}

// writeOverlays writes the Oh My Pi overlays each pane is told through PI_CONFIG_FILES: the
// operator's own, shared by every pane, which turns `retry.enabled` off so the host walks no
// fallback chain and every failed turn reaches the daemon; and one per claim naming the single
// model that claim's stand-in serves (writeRoleOverlay), so each claim's chain has no fallback
// below it.
func (r *providerErrorLive) writeOverlays(work string) map[claim.Token][]string {
	t := r.t
	t.Helper()
	dir := filepath.Join(work, "overlays")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	operator := filepath.Join(dir, "operator.yml")
	writeProofFile(t, operator, "# The operator's pane overlay (runtime.tmux.pi_config_files).\nretry:\n  enabled: false\n")
	overlays := map[claim.Token][]string{}
	for _, seeded := range r.issues() {
		path := filepath.Join(dir, string(seeded.role)+".yml")
		writeRoleOverlay(t, path, standinModel(seeded.provider))
		overlays[r.token(seeded.key, seeded.role)] = []string{operator, path}
	}
	return overlays
}

// newPaneListener is the Envoy listener a pane is told through ENVOY_URL, and answers the URL of
// it. A pane's plugin registers its session with the listener and claims its role topic
// (pi-shared's legion-claim-core, between claims/register and claims/ready), and a claim boot
// whose role claim is refused stops the agent, so the pane needs a listener that answers those
// calls — the fixture's noticeListener, which the daemon itself publishes its notices to, serves
// the publish route alone and 404s the rest. It answers as the plugins' own pane harness does
// (packages/pi-shared/test/omp-harness.ts, runLegionPane's stand-in): the session, a machine and
// no topics, for every path.
//
// Nothing the proof reads comes from here. What an architect or the controller is told is read
// from the daemon's own listener, which is the fixture's and another server.
func newPaneListener(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var sent struct {
			SessionID string `json:"session_id"`
			Dir       string `json:"dir"`
		}
		if body, err := io.ReadAll(req.Body); err == nil {
			_ = json.Unmarshal(body, &sent)
		}
		// The plugin reads an answer whose dir is no string as a refusal, so every answer carries
		// one: the directory the caller named, and the root for a call that named none.
		dir := sent.Dir
		if dir == "" {
			dir = "/"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"session_id": sent.SessionID,
			"machine_id": "provider-error-retry",
			"dir":        dir,
			"topics":     []string{},
		})
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// ---- the model stand-ins -------------------------------------------------------------------------

// providerStandin is one provider endpoint of this proof's own: it answers every model request
// the way its provider name says, over the stand-in both proofs share (live_agent_proof_test.go),
// which records each request so a step can count the ones a task reached it in.
//
// Until it is armed it answers a turn instead, which is how the claim it serves answers the
// warm-up task its seed left pending and reaches the step that drives it idle, holding nothing
// (seed, awaitWarmedUp). The overloaded one is armed from the start, because the task its claim
// holds at boot is the ladder step 1 measures and every turn of it must end in the 529.
type providerStandin struct {
	*modelStandin

	mu    sync.Mutex
	armed bool
}

func newProviderStandin(t *testing.T, name string) *providerStandin {
	t.Helper()
	s := &providerStandin{modelStandin: &modelStandin{name: name}, armed: name == overloadedProvider}
	s.serve(t, s)
	return s
}

// arm is the step's word that every turn from now on is answered the way this stand-in's
// provider name says.
func (s *providerStandin) arm() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armed = true
}

func (s *providerStandin) refusing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.armed
}

func (s *providerStandin) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if _, ok := s.take(w, req); !ok {
		return
	}
	if !s.refusing() {
		s.warmAnswer(w, req)
		return
	}
	switch s.name {
	case overloadedProvider:
		writeOverloaded(w)
	case malformedProvider:
		// No word of this body is one Oh My Pi reads as transient, so the turn error is reported
		// `retryable: false` with status 400.
		writeInvalidRequest(w, `{"type":"invalid_request_error","message":"messages: unexpected role"}`)
	case filteredProvider:
		s.streamContentFilter(w)
	}
}

// warmAnswer answers one request of an unarmed stand-in with a turn that ends: the Messages
// stream for the two `anthropic-messages` providers, a chat completion that stops for the
// `openai-completions` one, and a token count for Oh My Pi's own count request, which is no turn
// and which an Anthropic stream would not answer.
func (s *providerStandin) warmAnswer(w http.ResponseWriter, req *http.Request) {
	if s.name == filteredProvider {
		s.streamCompletion(w, "stop")
		return
	}
	if !strings.HasSuffix(req.URL.Path, "/v1/messages") {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"input_tokens":1000}`)
		return
	}
	streamAnthropicText(w, s.name, "WARMED")
}

// streamContentFilter answers one OpenAI chat completion: two chunks of text, then a chunk whose
// `finish_reason` is `content_filter`, which is what a provider's own filter leaves behind.
func (s *providerStandin) streamContentFilter(w http.ResponseWriter) {
	s.streamCompletion(w, "content_filter")
}

// streamCompletion answers one OpenAI chat completion of two text chunks and a last chunk
// carrying finish: `content_filter` is the refusal step 3 reads, and `stop` the warm-up turn a
// stand-in answers before it is armed.
func (s *providerStandin) streamCompletion(w http.ResponseWriter, finish string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	chunk := func(delta, finish string) {
		reason := "null"
		if finish != "" {
			reason = `"` + finish + `"`
		}
		fmt.Fprintf(w, "data: {\"id\":\"filtered\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,"+
			"\"choices\":[{\"index\":0,\"delta\":{%s},\"finish_reason\":%s}]}\n\n",
			filteredProvider+standinModelID, delta, reason)
		if flusher != nil {
			flusher.Flush()
		}
	}
	chunk(`"role":"assistant","content":"I can "`, "")
	chunk(`"content":"start on that"`, "")
	chunk("", finish)
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// ---- the daemon's clock and its log ----------------------------------------------------------------

// ladderClock is the wall clock with the ladder's driven rungs shortened: a timer armed for one
// of them (ladderDriven, read off ladderDelays) fires after ladderDrivenSpan instead, because
// waiting the fourth and fifth rungs out would add twelve minutes and prove the same doubling the
// first three do. Every other duration the daemon arms — its probe interval, its registration
// deadline, and the ladder's own waited-out rungs — passes through untouched, so what the proof
// measures is measured where it is real.
type ladderClock struct {
	supervise.RealClock
}

func (c *ladderClock) AfterFunc(d time.Duration, f func()) supervise.Cancel {
	if driven, ok := ladderDriven(d); ok {
		d = driven
	}
	return c.RealClock.AfterFunc(d, f)
}

// loggedRecord is one record the daemon wrote, as a step reads it back: the message, its
// attributes decoded from JSON, and the moment it was written.
type loggedRecord struct {
	Message string
	Attrs   map[string]any
	at      time.Time
}

// recordingLog is the daemon's own log handler with every record also kept in memory, so a step
// can read what the daemon decided and when. The records still go wherever liveProof.log() sends
// them, which is the script's evidence file. A handler the daemon derives with WithAttrs or
// WithGroup is another recordingLog over the derived inner handler, keeping its records on the
// same keptRecords and carrying the derivation's attributes, so a step reads a claim's `claim`
// attribute whether the daemon set it per record or on the logger.
type recordingLog struct {
	*keptRecords
	inner slog.Handler
	attrs []slog.Attr
}

// keptRecords is the records the root handler and every handler derived from it share.
type keptRecords struct {
	mu      sync.Mutex
	records []loggedRecord
}

func newRecordingLog(logger *slog.Logger) *recordingLog {
	return &recordingLog{keptRecords: &keptRecords{}, inner: logger.Handler()}
}

func (l *recordingLog) Enabled(context.Context, slog.Level) bool { return true }

func (l *recordingLog) Handle(ctx context.Context, record slog.Record) error {
	kept := loggedRecord{Message: record.Message, Attrs: map[string]any{}, at: time.Now()}
	for _, attr := range l.attrs {
		kept.Attrs[attr.Key] = decodeAttr(attr.Value)
	}
	record.Attrs(func(attr slog.Attr) bool {
		kept.Attrs[attr.Key] = decodeAttr(attr.Value)
		return true
	})
	l.mu.Lock()
	l.records = append(l.records, kept)
	l.mu.Unlock()
	if l.inner.Enabled(ctx, record.Level) {
		return l.inner.Handle(ctx, record)
	}
	return nil
}

// decodeAttr is one attribute as a step compares it: a number as float64, whatever its Go type, so
// a step's own literal need not match the daemon's; a duration and every other value as the text
// the log shows.
func decodeAttr(value slog.Value) any {
	switch value.Kind() {
	case slog.KindInt64:
		return float64(value.Int64())
	case slog.KindUint64:
		return float64(value.Uint64())
	case slog.KindFloat64:
		return value.Float64()
	case slog.KindBool:
		return value.Bool()
	case slog.KindString:
		return value.String()
	default:
		return value.String()
	}
}

func (l *recordingLog) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &recordingLog{keptRecords: l.keptRecords, inner: l.inner.WithAttrs(attrs),
		attrs: append(append([]slog.Attr{}, l.attrs...), attrs...)}
}

func (l *recordingLog) WithGroup(name string) slog.Handler {
	return &recordingLog{keptRecords: l.keptRecords, inner: l.inner.WithGroup(name), attrs: l.attrs}
}

func (l *recordingLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.records)
}

func (l *recordingLog) matching(want func(loggedRecord) bool) []loggedRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	var matched []loggedRecord
	for _, kept := range l.records {
		if want(kept) {
			matched = append(matched, kept)
		}
	}
	return matched
}

// firstAfter is the first record written after the first from, matching want.
func (l *recordingLog) firstAfter(from int, want func(loggedRecord) bool) *loggedRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := from; i < len(l.records); i++ {
		if want(l.records[i]) {
			kept := l.records[i]
			return &kept
		}
	}
	return nil
}
