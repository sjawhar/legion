//go:build e2e

// The live proof's driver for the handoff records of INFRA-177: a phase's handoff is a record on
// the Dispatch issue, written with `dispatch handoff-write` under the worker's own session, and a
// completion names it. scripts/e2e/handoff-records.sh builds this driver with `-tags e2e` and owns
// the nats-server and the PostgreSQL it runs against.
//
// Nothing of the record path is stood in for. Dispatch is the real server: this driver builds
// packages/envoy's cmd/dispatch and runs it, as packages/dispatch/e2e/run-server.sh does, on a
// database of its own on LEGION_TEST_PG_DSN's server, with NATS off and the agent token the daemon
// and the CLI present; it stops it and starts it again on the same database and port for A10. The
// daemon is the whole daemon (run, the function `legion start` calls, through startDaemonWithin),
// booted with dispatch_url at that server, so every Dispatch read and write it makes - the
// repository mapping, the issue list, its status writes and messages, and the read of the record a
// completion names - goes to the real routes. The worker side is the real `dispatch` CLI
// (packages/envoy-client/bin/dispatch.ts under bun) run as each claim's registered session, and
// the daemon's own routes: registration, grants and POST /legion/v1/handoff/complete. The import's
// records are written by `legion handoffs import` itself (handoffimport.Run) against the running
// daemon's state and the real Dispatch.
//
// What is stood in for lies outside the records. The broker is a native nats-server the script
// starts and names in LEGION_E2E_NATS_URL. GitHub is two REST stand-ins: the daemon's
// (mergedRootWaitsGitHub, which answers each repository with default branch main and nothing
// else), and the import's (handoffRecordsGitHub), one branch carrying `.legion/<KEY>/`, which the
// import reads and removes through GitHub's contents and Git Data APIs. The Apps' tokens are
// workflowTokenRecorder's placeholders, Envoy's listener is the package's noticeListener, and no
// agent runs: each claim is launched by the package's fake runtime, which provisions its workspace
// in its own pod, and is driven over the daemon's worker stream as its pane's shim drives it
// (handoffClaimant). The issues, their pull requests, their trees' slots and the claims the boot
// relaunches are seeded through the store, as every daemon test seeds its fixture; every
// assertion is a read of GET /legion/v1/state, of a route's answer, of what the CLI printed, of
// Dispatch's own routes, or of the task a started worker's shim was prompted with.
package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/handoffimport"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// handoffRecordsRootEnv names the checkout the script runs from, whose packages/envoy the driver
// builds Dispatch from and whose packages/envoy-client holds the CLI. The script exports it: the
// driver binary runs from the caller's directory, not the package's. Unset (a plain `go test
// -tags e2e`, which runs in the package directory), the checkout is four directories up.
const handoffRecordsRootEnv = "LEGION_E2E_REPO_ROOT"

// handoffRecordsUser is the person the driver sets Dispatch up as: the project, its repository
// mapping and the issues, through the trusted identity header the scratch server takes.
const handoffRecordsUser = "legion-e2e"

// handoffBodyMax is Dispatch's handoff body cap (contracts DISPATCH_HANDOFF_BODY_MAX).
const handoffBodyMax = 262144

// handoffRecordsLive is one run of the proof: the booted daemon over the real Dispatch, the
// stand-ins it reads, and the issues the steps drive.
type handoffRecordsLive struct {
	liveProof
	rt       *fake.Runtime
	built    *built
	root     string
	dispatch *handoffDispatch
	github   string
	importGH *handoffRecordsGitHub
	cliHome  string
	// a6 is the issue A6 writes to, on Dispatch alone; a3 and a10 are in planning, a7 in testing
	// with its branch carrying `.legion/`, and renew implementing, the HANDOFF_NOT_NEW step's.
	a6, a3, a7, renew, a10 string
	// claimants is each registered claim, by token.
	claimants map[claim.Token]*handoffClaimant
	// a10Existing is the record the A10 completion names while Dispatch is stopped.
	a10Existing handoffWritten
}

// TestHandoffRecordsLive drives the proof's steps against one booted daemon and one Dispatch.
// A10 runs last, since it stops Dispatch under the daemon.
func TestHandoffRecordsLive(t *testing.T) {
	r := newHandoffRecordsLive(t)
	r.step("a6-a-40kb-handoff-reads-back-byte-identical-two-writes-are-versions-1-and-2", r.a6RoundTrip)
	r.step("a6-control-a-body-one-byte-over-the-cap-is-refused", r.a6OverCap)
	r.step("a3-a-record-another-session-wrote-is-refused-at-completion", r.a3Refused)
	r.step("a3-control-the-planners-own-record-moves-the-issue-to-implementing", r.a3Accepted)
	r.step("a7-the-imports-record-is-refused-the-tester-writes-it-again-and-the-reviewer-is-told-it", r.a7Import)
	r.step("handoff-not-new-the-implementers-round-1-record-is-refused-in-round-2", r.notNew)
	r.step("a10-control-with-dispatch-up-the-first-write-succeeds", r.a10Up)
	r.step("a10-with-dispatch-stopped-the-write-fails-and-the-completion-is-refused", r.a10Down)
	r.step("a10-dispatch-restarted-the-retried-write-and-the-completion-land", r.a10Restarted)
}

// newHandoffRecordsLive starts Dispatch, sets up its project, mapping and issues, seeds the
// daemon's store with those issues where each step starts them, and boots the daemon at Dispatch.
func newHandoffRecordsLive(t *testing.T) *handoffRecordsLive {
	t.Helper()
	proof := newLiveProof(t, "scripts/e2e/handoff-records.sh")
	// A Dispatch project key is at most ten characters (contracts ProjectKeyPattern), and the
	// daemon's project is its Dispatch project, so the run's project is a short one of its own.
	proof.cfg.Project = "HR" + randomSuffix(t)[:6]
	proof.cfg.Projects = map[string]config.Project{proof.cfg.Project: {}}
	proof.cfg.AdmissionCap = 8
	var err error
	if proof.project, err = claim.ProjectToken(proof.cfg.Project); err != nil {
		t.Fatal(err)
	}
	root := os.Getenv(handoffRecordsRootEnv)
	if root == "" {
		root = filepath.Join("..", "..", "..", "..")
	}
	if root, err = filepath.Abs(root); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("bun"); err != nil {
		t.Fatalf("bun is not on PATH, and the dispatch CLI runs under it: %v", err)
	}
	r := &handoffRecordsLive{
		liveProof: proof, rt: fake.NewRuntime(), built: &built{}, root: root,
		github: mergedRootWaitsGitHub(t), cliHome: t.TempDir(), claimants: map[claim.Token]*handoffClaimant{},
	}
	r.rt.InPod = true
	r.dispatch = startHandoffDispatch(t, root)
	if err := os.WriteFile(r.cfg.DispatchTokenFile, []byte(r.dispatch.token+"\n"), 0o600); err != nil {
		t.Fatalf("write the Dispatch token file: %v", err)
	}
	r.cfg.DispatchURL = r.dispatch.base
	r.setUpDispatch()
	r.importGH = newHandoffRecordsGitHub(t, r.a7)
	r.seed()
	o := fakeRuntime(r.rt, r.built)
	o.workflowTokens = &workflowTokenRecorder{}
	o.githubAPI = r.github
	r.d = startDaemonWithin(t, r.cfg, o, r.log(), 2*time.Minute)
	r.note("Dispatch %s (database of its own, NATS off), project %s mapped to %s; daemon at dispatch_url %s",
		r.dispatch.base, r.cfg.Project, liveRepo, r.cfg.DispatchURL)
	return r
}

// setUpDispatch creates the run's project on Dispatch, maps liveRepo to it as its default, and
// creates the issues. The workflow's issues are in progress and carry the legion label, as a
// running daemon leaves them; A6's issue is no Legion issue and sits in the backlog.
func (r *handoffRecordsLive) setUpDispatch() {
	t := r.t
	t.Helper()
	r.dispatch.user(http.MethodPost, "/api/v1/projects", map[string]string{"key": r.cfg.Project, "name": "Handoff records live proof"}, http.StatusCreated, nil)
	r.dispatch.user(http.MethodPut, "/api/v1/settings/repo-projects/"+liveRepo, map[string]any{"project": r.cfg.Project, "default": true}, http.StatusOK, nil)
	create := func(title, status string, labels []string) string {
		var created struct {
			Key string `json:"key"`
		}
		r.dispatch.user(http.MethodPost, "/api/v1/issues", map[string]string{"project": r.cfg.Project, "title": title}, http.StatusCreated, &created)
		if created.Key == "" {
			t.Fatalf("Dispatch created the issue %q with no key", title)
		}
		r.dispatch.user(http.MethodPatch, "/api/v1/issues/"+created.Key, map[string]any{"status": status, "labels": labels}, http.StatusOK, nil)
		return created.Key
	}
	r.a6 = create("A6: a 40 KB handoff round trip", "backlog", []string{})
	r.a3 = create("A3: the planner's handoff under another session", "in_progress", []string{"legion"})
	r.a7 = create("A7: a tester mid-phase at the import", "in_progress", []string{"legion"})
	r.renew = create("HANDOFF_NOT_NEW: the implementer's second round", "in_progress", []string{"legion"})
	r.a10 = create("A10: Dispatch stops under a completion", "in_progress", []string{"legion"})
}

// seed records the four workflow issues as the daemon would hold them: each the root of its own
// tree with its admission slot and its lifecycle under the workflow; a3 and a10 in planning, a7 in
// testing and renew implementing, the last two on open pull requests; and each phase's worker as
// a launch a previous daemon began and never recorded, serving run 1, which this boot relaunches.
func (r *handoffRecordsLive) seed() {
	t := r.t
	t.Helper()
	ctx := context.Background()
	number, err := strconv.ParseInt(randomSuffix(t)[:6], 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	type seeded struct {
		key   string
		phase phase.Phase
		role  claim.Role
		pr    int
	}
	issues := []seeded{
		{r.a3, phase.Planning, claim.RolePlanner, 0},
		{r.a7, phase.Testing, claim.RoleTester, int(number) + 1},
		{r.renew, phase.Implementing, claim.RoleImplementer, int(number) + 2},
		{r.a10, phase.Planning, claim.RolePlanner, 0},
	}
	if err := r.st.Tx(ctx, func(tx pgx.Tx) error {
		for _, s := range issues {
			listed := r.dispatch.issue(s.key)
			if err := r.records.PutIssue(ctx, tx, record.Issue{
				Key: s.key, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Tree: s.key, Project: r.cfg.Project,
				Title: listed.Title, Phase: s.phase, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", Rank: listed.Rank,
				LastDispatchSeq: listed.LastSeq,
			}); err != nil {
				return err
			}
			if s.pr != 0 {
				if err := r.records.PutPullRequest(ctx, tx, record.PullRequest{
					State: record.PullRequestOpen, Issue: s.key, Repo: liveRepo, Number: s.pr, Branch: "legion/" + s.key, HeadSHA: "head-" + s.key, Failing: []string{},
				}); err != nil {
					return err
				}
			}
			if err := r.admit(ctx, tx, s.key); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the issues: %v", err)
	}
	for _, s := range issues {
		if _, err := r.st.OpenTreeLifecycle(ctx, r.project, s.key, treelifecycle.AuthorityWorkflow); err != nil {
			t.Fatalf("open the lifecycle of %s: %v", s.key, err)
		}
		token := r.token(s.key, s.role)
		if _, err := r.st.AdmitClaim(ctx, supervise.Claim{
			Token: token, Project: r.project, Tree: s.key, Issue: s.key, Role: s.role, Generation: 1, State: supervise.StateLaunching,
			BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)), ServingGeneration: 1,
		}); err != nil {
			t.Fatalf("record the %s's unfinished launch on %s: %v", s.role, s.key, err)
		}
		writePrompt(t, r.cfg, token)
	}
}

// Step A6. A 40 KB handoff written with `dispatch handoff-write` reads back with `dispatch
// handoff-read` byte for byte, and two writes are versions 1 and 2.
func (r *handoffRecordsLive) a6RoundTrip() {
	t := r.t
	const session = "ses_a6_writer"
	first, second := r.bigBody(r.a6, 1), r.bigBody(r.a6, 2)
	r.note("bodies of %d and %d bytes, each a JSON object with a proof list, non-ASCII text and literal escapes, ending in a newline", len(first), len(second))
	one := r.write(session, r.a6, "implement", first)
	two := r.write(session, r.a6, "implement", second)
	if one.version != 1 || two.version != 2 {
		t.Fatalf("the two writes are versions %d and %d, want 1 and 2", one.version, two.version)
	}
	r.note("handoff-write: %s", one.line)
	r.note("handoff-write: %s", two.line)
	for _, c := range []struct {
		version string
		body    []byte
		args    []string
	}{
		{"2 (newest)", second, nil},
		{"1", first, []string{"--version", "1"}},
	} {
		meta, body := r.read(session, r.a6, "implement", c.args...)
		if !bytes.Equal(body, c.body) {
			t.Fatalf("handoff-read of version %s gave %d bytes (sha256 %s), want the %d written (sha256 %s)",
				c.version, len(body), digest(body), len(c.body), digest(c.body))
		}
		r.note("handoff-read version %s: %s", c.version, meta)
		r.note("   body %d bytes, sha256 %s, byte-identical to the file written", len(body), digest(body))
	}
}

// Step A6's control. A body of the cap plus one byte is refused: the CLI refuses it before it
// sends anything, and Dispatch itself, sent it raw, answers 413 CAP_EXCEEDED. The issue still
// holds two versions.
func (r *handoffRecordsLive) a6OverCap() {
	t := r.t
	const session = "ses_a6_writer"
	body := capBody(handoffBodyMax + 1)
	file := r.bodyFile("over-cap", body)
	out, code := r.cli(session, "handoff-write", "--issue", r.a6, "--phase", "implement", "--body-file", file)
	over := fmt.Sprintf("1 bytes over the %d-byte limit", handoffBodyMax)
	if code == 0 || !strings.Contains(out, "was not called") || !strings.Contains(out, over) {
		t.Fatalf("handoff-write of %d bytes exited %d: %q, want a refusal before the call naming %q", len(body), code, out, over)
	}
	r.note("handoff-write of a %d-byte body exited %d: %s", len(body), code, strings.Join(strings.Fields(out), " "))
	status, answer := r.dispatch.agentPost("/api/v1/issues/"+r.a6+"/handoffs/implement", map[string]any{
		"actor": map[string]string{"kind": "session", "id": session}, "body": string(body),
	})
	var refusal struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(answer, &refusal); err != nil {
		t.Fatalf("decode Dispatch's answer %s: %v", answer, err)
	}
	if status != http.StatusRequestEntityTooLarge || refusal.Code != "CAP_EXCEEDED" {
		t.Fatalf("Dispatch answered the raw %d-byte write %d %s, want 413 CAP_EXCEEDED", len(body), status, answer)
	}
	r.note("POST /api/v1/issues/%s/handoffs/implement with the same body, raw: %d %s: %s", r.a6, status, refusal.Code, refusal.Error)
	listed := r.dispatch.handoffs(r.a6)
	if len(listed) != 2 || listed[0].Version != 2 {
		t.Fatalf("after the refusals %s lists %+v, want versions 2 and 1 only", r.a6, listed)
	}
	r.note("GET /api/v1/issues/%s/handoffs still lists versions %d and %d", r.a6, listed[0].Version, listed[1].Version)
}

// Step A3. Another session writes the planner's handoff; the planner's completion naming that
// record is refused 403 HANDOFF_SESSION_MISMATCH naming both sessions, and the issue stays in
// planning.
func (r *handoffRecordsLive) a3Refused() {
	t := r.t
	planner := r.claimant(claim.RolePlanner, r.a3, "ses_a3_planner")
	const other = "ses_a3_another_session"
	written := r.write(other, r.a3, "plan", r.smallBody(r.a3, "plan", "written by another session"))
	r.note("handoff-write as %s: %s", other, written.line)
	status, failure := planner.complete(api.HandoffCompleteRequest{Summary: "planned", HandoffID: written.id, HandoffVersion: written.version})
	if status != http.StatusForbidden || failure.Code != "HANDOFF_SESSION_MISMATCH" ||
		!strings.Contains(failure.Error, other) || !strings.Contains(failure.Error, planner.session) {
		t.Fatalf("the planner's completion naming %s's record = %d %+v, want 403 HANDOFF_SESSION_MISMATCH naming %s and %s",
			other, status, failure, other, planner.session)
	}
	r.note("POST /legion/v1/handoff/complete as %s naming %s: %d %s: %s", planner.session, written.id, status, failure.Code, failure.Error)
	r.holdPhase(r.a3, phase.Planning)
}

// Step A3's control. The planner writes the handoff under its own session, and the same
// completion naming that record moves the issue to implementing.
func (r *handoffRecordsLive) a3Accepted() {
	t := r.t
	planner := r.claimants[r.token(r.a3, claim.RolePlanner)]
	written := r.write(planner.session, r.a3, "plan", r.smallBody(r.a3, "plan", "written by the planner"))
	r.note("handoff-write as %s: %s", planner.session, written.line)
	status, failure := planner.complete(api.HandoffCompleteRequest{Summary: "planned", HandoffID: written.id, HandoffVersion: written.version})
	if status != http.StatusOK {
		t.Fatalf("the planner's completion naming its own record = %d %+v, want 200", status, failure)
	}
	r.note("POST /legion/v1/handoff/complete as %s naming %s: %d", planner.session, written.id, status)
	got := r.waitPhase(r.a3, phase.Implementing)
	r.note("read back: %s is in %s", r.a3, got.Phase)
}

// Step A7. `legion handoffs import` writes the files on a7's branch as its records under the
// daemon's session and removes `.legion/` from the branch; the tester's completion naming the
// imported test record is refused 403 HANDOFF_SESSION_MISMATCH; the tester reads it with `dispatch
// handoff-read`, writes it again under its own session, and completes; and the reviewer the pass
// starts is told that record in its task.
func (r *handoffRecordsLive) a7Import() {
	t := r.t
	tester := r.claimant(claim.RoleTester, r.a7, "ses_a7_tester")
	var stdout, stderr bytes.Buffer
	code := handoffimport.Run(context.Background(), handoffimport.Options{
		Project: r.cfg.Project, DaemonURL: r.cfg.DaemonURL, GitHubAPI: r.importGH.server.URL,
		Tokens: &workflowTokenRecorder{}, Dispatch: dispatch.New(r.dispatch.base, r.dispatch.token),
	}, &stdout, &stderr)
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		r.note("legion handoffs import: %s", line)
	}
	if code != 0 {
		t.Fatalf("legion handoffs import exited %d: %s%s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{r.a7 + " plan: imported v1", r.a7 + " implement: imported v1", r.a7 + " test: imported v1", r.a7 + ": removed .legion/ at "} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("the import printed %q, want a line %q", stdout.String(), want)
		}
	}
	r.importGH.heldRemoval(t, r)
	imported := r.dispatch.latest(r.a7, "test")
	if imported.Author.Kind != "session" || imported.Author.ID != dispatch.DaemonSession(r.a7) || imported.Body != r.importGH.files["test.json"] {
		t.Fatalf("the imported test record = %+v, want the branch's test.json under %s", imported, dispatch.DaemonSession(r.a7))
	}
	r.note("Dispatch's test record of %s: version %d (id %s) by session %s, the branch's test.json byte for byte", r.a7, imported.Version, imported.ID, imported.Author.ID)

	status, failure := tester.complete(api.HandoffCompleteRequest{Summary: "tested", Verdict: "pass", HandoffID: imported.ID, HandoffVersion: imported.Version})
	if status != http.StatusForbidden || failure.Code != "HANDOFF_SESSION_MISMATCH" ||
		!strings.Contains(failure.Error, dispatch.DaemonSession(r.a7)) || !strings.Contains(failure.Error, tester.session) {
		t.Fatalf("the tester's completion naming the imported record = %d %+v, want 403 HANDOFF_SESSION_MISMATCH naming %s and %s",
			status, failure, dispatch.DaemonSession(r.a7), tester.session)
	}
	r.note("POST /legion/v1/handoff/complete as %s naming the imported record: %d %s: %s", tester.session, status, failure.Code, failure.Error)
	r.holdPhase(r.a7, phase.Testing)

	meta, body := r.read(tester.session, r.a7, "test")
	r.note("handoff-read as %s: %s", tester.session, meta)
	rewritten := r.write(tester.session, r.a7, "test", body)
	r.note("handoff-write as %s of the body it read: %s", tester.session, rewritten.line)
	if again := r.dispatch.latest(r.a7, "test"); again.Body != imported.Body || again.Author.ID != tester.session {
		t.Fatalf("the rewritten test record = %+v, want the imported body under %s", again, tester.session)
	}
	status, failure = tester.complete(api.HandoffCompleteRequest{Summary: "tested", Verdict: "pass", HandoffID: rewritten.id, HandoffVersion: rewritten.version})
	if status != http.StatusOK {
		t.Fatalf("the tester's completion naming its own record = %d %+v, want 200", status, failure)
	}
	r.note("POST /legion/v1/handoff/complete as %s naming %s (verdict pass): %d", tester.session, rewritten.id, status)
	got := r.waitPhase(r.a7, phase.Reviewing)
	r.note("read back: %s is in %s", r.a7, got.Phase)

	reviewer := r.claimant(claim.RoleReviewer, r.a7, "ses_a7_reviewer")
	task := reviewer.awaitPrompt()
	want := fmt.Sprintf("Handoff: test version %d (dispatch handoff-read --issue %s --phase test).", rewritten.version, r.a7)
	if !strings.Contains(task.Message, want) {
		t.Fatalf("the reviewer was prompted %q, want it to name %q", task.Message, want)
	}
	r.note("the reviewer's shim was prompted: %q", task.Message)
	meta, _ = r.read(reviewer.session, r.a7, "test")
	if !strings.Contains(meta, fmt.Sprintf("version %d (id %s)", rewritten.version, rewritten.id)) || !strings.Contains(meta, tester.session) {
		t.Fatalf("the reviewer's handoff-read says %q, want version %d (id %s) by %s", meta, rewritten.version, rewritten.id, tester.session)
	}
	r.note("handoff-read as %s: %s", reviewer.session, meta)
}

// Step HANDOFF_NOT_NEW. The implementer completes round 1 naming its implement record; the tester
// the daemon starts fails the round, which sends the issue back to implementing; the implementer's
// round-2 completion naming its round-1 record is refused 409 HANDOFF_NOT_NEW, and naming a new
// record moves the issue to testing again.
func (r *handoffRecordsLive) notNew() {
	t := r.t
	implementer := r.claimant(claim.RoleImplementer, r.renew, "ses_renew_implementer")
	first := r.write(implementer.session, r.renew, "implement", r.smallBody(r.renew, "implement", "round 1"))
	r.note("handoff-write as %s: %s", implementer.session, first.line)
	status, failure := implementer.complete(api.HandoffCompleteRequest{Summary: "implemented round 1", Commit: "round-1-" + r.renew, HandoffID: first.id, HandoffVersion: first.version})
	if status != http.StatusOK {
		t.Fatalf("the implementer's round-1 completion = %d %+v, want 200", status, failure)
	}
	r.note("POST /legion/v1/handoff/complete as %s naming %s: %d", implementer.session, first.id, status)
	r.waitPhase(r.renew, phase.Testing)
	r.note("read back: %s is in testing", r.renew)

	tester := r.claimant(claim.RoleTester, r.renew, "ses_renew_tester")
	task := tester.awaitPrompt()
	r.note("the tester's shim was prompted: %q", task.Message)
	test := r.write(tester.session, r.renew, "test", r.smallBody(r.renew, "test", "round 1 fails"))
	r.note("handoff-write as %s: %s", tester.session, test.line)
	status, failure = tester.complete(api.HandoffCompleteRequest{Summary: "a criterion fails", Verdict: "fail", HandoffID: test.id, HandoffVersion: test.version})
	if status != http.StatusOK {
		t.Fatalf("the tester's failing completion = %d %+v, want 200", status, failure)
	}
	tester.endTurn()
	r.note("POST /legion/v1/handoff/complete as %s naming %s (verdict fail): %d", tester.session, test.id, status)
	r.waitPhase(r.renew, phase.Implementing)
	r.note("read back: %s is back in implementing", r.renew)
	task = implementer.awaitPrompt()
	r.note("the implementer's shim was prompted: %q", task.Message)

	status, failure = implementer.complete(api.HandoffCompleteRequest{Summary: "implemented round 2", Commit: "round-1-" + r.renew, HandoffID: first.id, HandoffVersion: first.version})
	if status != http.StatusConflict || failure.Code != "HANDOFF_NOT_NEW" || !strings.Contains(failure.Error, first.id) {
		t.Fatalf("the implementer's round-2 completion naming its round-1 record = %d %+v, want 409 HANDOFF_NOT_NEW naming %s", status, failure, first.id)
	}
	r.note("POST /legion/v1/handoff/complete as %s naming the round-1 record %s: %d %s: %s", implementer.session, first.id, status, failure.Code, failure.Error)
	r.holdPhase(r.renew, phase.Implementing)

	second := r.write(implementer.session, r.renew, "implement", r.smallBody(r.renew, "implement", "round 2"))
	r.note("handoff-write as %s: %s", implementer.session, second.line)
	status, failure = implementer.complete(api.HandoffCompleteRequest{Summary: "implemented round 2", Commit: "round-2-" + r.renew, HandoffID: second.id, HandoffVersion: second.version})
	if status != http.StatusOK {
		t.Fatalf("the implementer's round-2 completion naming its new record = %d %+v, want 200", status, failure)
	}
	implementer.endTurn()
	r.note("control: POST /legion/v1/handoff/complete as %s naming %s: %d", implementer.session, second.id, status)
	r.waitPhase(r.renew, phase.Testing)
	r.note("read back: %s is in testing again", r.renew)
}

// Step A10's control. With Dispatch up, the planner's first write succeeds.
func (r *handoffRecordsLive) a10Up() {
	planner := r.claimant(claim.RolePlanner, r.a10, "ses_a10_planner")
	r.a10Existing = r.write(planner.session, r.a10, "plan", r.smallBody(r.a10, "plan", "written with Dispatch up"))
	r.note("handoff-write as %s with Dispatch up: %s", planner.session, r.a10Existing.line)
}

// Step A10. With the Dispatch process stopped, `dispatch handoff-write` exits non-zero naming the
// connection failure, and the planner's completion naming the record that exists is refused 502
// DISPATCH_READ_FAILED; the issue stays in planning.
func (r *handoffRecordsLive) a10Down() {
	t := r.t
	planner := r.claimants[r.token(r.a10, claim.RolePlanner)]
	r.dispatch.stop()
	r.note("stopped the Dispatch process (SIGINT); %s refuses connections", r.dispatch.base)
	file := r.bodyFile("a10-retry", r.smallBody(r.a10, "plan", "written once Dispatch answers"))
	out, code := r.cli(planner.session, "handoff-write", "--issue", r.a10, "--phase", "plan", "--body-file", file)
	lower := strings.ToLower(out)
	if code == 0 || !(strings.Contains(lower, "connect") || strings.Contains(lower, "refused")) {
		t.Fatalf("handoff-write with Dispatch stopped exited %d: %q, want a non-zero exit naming the connection failure", code, out)
	}
	r.note("handoff-write with Dispatch stopped exited %d: %s", code, strings.TrimSpace(out))
	status, failure := planner.complete(api.HandoffCompleteRequest{Summary: "planned", HandoffID: r.a10Existing.id, HandoffVersion: r.a10Existing.version})
	if status != http.StatusBadGateway || failure.Code != "DISPATCH_READ_FAILED" {
		t.Fatalf("the completion with Dispatch stopped = %d %+v, want 502 DISPATCH_READ_FAILED", status, failure)
	}
	r.note("POST /legion/v1/handoff/complete naming %s: %d %s: %s", r.a10Existing.id, status, failure.Code, failure.Error)
	r.holdPhase(r.a10, phase.Planning)
}

// Step A10, Dispatch back. Dispatch restarted on the same database and port, the retried write
// succeeds as version 2, the completion naming it is accepted and moves the issue to implementing,
// and the issue's handoff list - what Conversation reads - holds it.
func (r *handoffRecordsLive) a10Restarted() {
	t := r.t
	planner := r.claimants[r.token(r.a10, claim.RolePlanner)]
	r.dispatch.start()
	r.note("started Dispatch again on the same database at %s", r.dispatch.base)
	out, code := r.cli(planner.session, "handoff-write", "--issue", r.a10, "--phase", "plan", "--body-file", r.bodyFile("a10-retry", nil))
	retried := parseWritten(t, out, code)
	if retried.version != 2 {
		t.Fatalf("the retried write is version %d, want 2", retried.version)
	}
	r.note("retried handoff-write as %s: %s", planner.session, retried.line)
	status, failure := planner.complete(api.HandoffCompleteRequest{Summary: "planned", HandoffID: retried.id, HandoffVersion: retried.version})
	if status != http.StatusOK {
		t.Fatalf("the retried completion = %d %+v, want 200", status, failure)
	}
	r.note("POST /legion/v1/handoff/complete naming %s: %d", retried.id, status)
	got := r.waitPhase(r.a10, phase.Implementing)
	r.note("read back: %s is in %s", r.a10, got.Phase)
	listed := r.dispatch.handoffs(r.a10)
	if len(listed) != 2 || listed[0].ID != retried.id || listed[0].Author.ID != planner.session || listed[1].ID != r.a10Existing.id {
		t.Fatalf("GET /api/v1/issues/%s/handoffs = %+v, want the retried record %s newest, then %s", r.a10, listed, retried.id, r.a10Existing.id)
	}
	for _, h := range listed {
		r.note("GET /api/v1/issues/%s/handoffs: %s version %d (id %s) by %s %s at %s", r.a10, h.Phase, h.Version, h.ID, h.Author.Kind, h.Author.ID, h.CreatedAt)
	}
}

// holdPhase holds that key is still in want, for liveSettle, as the refusal left it.
func (r *handoffRecordsLive) holdPhase(key string, want phase.Phase) {
	r.t.Helper()
	deadline := time.Now().Add(liveSettle)
	for time.Now().Before(deadline) {
		if got := r.issue(key).Phase; got != want {
			r.t.Fatalf("%s moved to %s after the refusal, want it to stay in %s", key, got, want)
		}
		time.Sleep(100 * time.Millisecond)
	}
	r.note("read back: %s is still in %s", key, want)
}

// waitPhase waits for key to reach want.
func (r *handoffRecordsLive) waitPhase(key string, want phase.Phase) api.Issue {
	r.t.Helper()
	var last api.Issue
	testwait.Eventually(r.t, fmt.Sprintf("%s in %s", key, want), func() bool {
		last = r.issue(key)
		return last.Phase == want
	})
	return last
}

// token is the claim token of role on issue, as the daemon spells it.
func (r *handoffRecordsLive) token(issue string, role claim.Role) claim.Token {
	r.t.Helper()
	token, err := claim.NewToken(r.project, issue, role)
	if err != nil {
		r.t.Fatal(err)
	}
	return token
}

// handoffWritten is one record `dispatch handoff-write` reported.
type handoffWritten struct {
	id      string
	version int
	line    string
}

// write runs `dispatch handoff-write` as session with body, and returns the record it reported.
func (r *handoffRecordsLive) write(session, issue, word string, body []byte) handoffWritten {
	r.t.Helper()
	out, code := r.cli(session, "handoff-write", "--issue", issue, "--phase", word, "--body-file", r.bodyFile(session+"-"+word, body))
	return parseWritten(r.t, out, code)
}

// parseWritten reads the record `dispatch handoff-write` printed: `Recorded the <phase> handoff of
// <KEY>, version <n> (id <uuid>)`.
func parseWritten(t *testing.T, out string, code int) handoffWritten {
	t.Helper()
	line := firstLine(out)
	var word, key, id string
	var version int
	if _, err := fmt.Sscanf(line, "Recorded the %s handoff of %s version %d (id %s", &word, &key, &version, &id); code != 0 || err != nil {
		t.Fatalf("handoff-write exited %d printing %q (%v), want `Recorded the <phase> handoff of <KEY>, version <n> (id <uuid>)`", code, out, err)
	}
	return handoffWritten{id: strings.TrimSuffix(id, ")"), version: version, line: line}
}

// read runs `dispatch handoff-read` as session and returns its metadata line and the body below it.
func (r *handoffRecordsLive) read(session, issue, word string, extra ...string) (string, []byte) {
	r.t.Helper()
	out, code := r.cliOut(session, append([]string{"handoff-read", "--issue", issue, "--phase", word}, extra...)...)
	meta, body, found := bytes.Cut(out.stdout, []byte("\n"))
	if code != 0 || !found {
		r.t.Fatalf("handoff-read exited %d: %s%s", code, out.stdout, out.stderr)
	}
	return string(meta), body
}

// bodyFile writes body (when not nil) to the run's own file name, outside any checkout, and
// returns its path.
func (r *handoffRecordsLive) bodyFile(name string, body []byte) string {
	r.t.Helper()
	path := filepath.Join(r.cliHome, "bodies", name+".json")
	if body == nil {
		return path
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		r.t.Fatal(err)
	}
	return path
}

// cliOutput is what one CLI run printed on each stream.
type cliOutput struct {
	stdout, stderr []byte
}

// cli runs the dispatch CLI as session and returns both streams together and its exit code.
func (r *handoffRecordsLive) cli(session string, args ...string) (string, int) {
	r.t.Helper()
	out, code := r.cliOut(session, args...)
	return string(out.stdout) + string(out.stderr), code
}

// cliOut runs `bun packages/envoy-client/bin/dispatch.ts args...` as an omp session's shell runs
// `dispatch`: DISPATCH_HOST omp, DISPATCH_SESSION_ID the session, and Dispatch named by
// DISPATCH_URL and DISPATCH_TOKEN. Nothing else of the caller's environment reaches it but PATH:
// its home, its configuration file (absent, so the variables are the whole of its Dispatch
// settings) and its session state are the run's own, so no operator's file or session is read.
func (r *handoffRecordsLive) cliOut(session string, args ...string) (cliOutput, int) {
	r.t.Helper()
	home := filepath.Join(r.cliHome, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		r.t.Fatal(err)
	}
	command := exec.Command("bun", append([]string{filepath.Join(r.root, "packages", "envoy-client", "bin", "dispatch.ts")}, args...)...)
	command.Dir = r.cliHome
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"TMPDIR=" + r.cliHome,
		"XDG_CONFIG_HOME=" + filepath.Join(r.cliHome, "config"),
		"XDG_STATE_HOME=" + filepath.Join(r.cliHome, "state"),
		"XDG_CACHE_HOME=" + filepath.Join(r.cliHome, "cache"),
		"ENVOY_CONFIG_FILE=" + filepath.Join(r.cliHome, "envoy.json"),
		"DISPATCH_STATE_DIR=" + filepath.Join(r.cliHome, "dispatch-state"),
		"DISPATCH_HOST=omp",
		"DISPATCH_SESSION_ID=" + session,
		"DISPATCH_URL=" + r.dispatch.base,
		"DISPATCH_TOKEN=" + r.dispatch.token,
	}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	code := 0
	if err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			r.t.Fatalf("run the dispatch CLI: %v", err)
		}
		code = exit.ExitCode()
	}
	return cliOutput{stdout: stdout.Bytes(), stderr: stderr.Bytes()}, code
}

// bigBody is a handoff of at least 40 KB for issue, distinct per n: a JSON object with today's
// fields and a proof list long enough to reach the size, holding non-ASCII text and escapes a
// re-encoding would change, indented, and ending in a newline.
func (r *handoffRecordsLive) bigBody(issue string, n int) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "{\n  \"schemaVersion\": 1,\n  \"phase\": \"implement\",\n  \"issue\": %q,\n  \"completed\": \"2026-10-27T00:00:0%dZ\",\n", issue, n)
	b.WriteString("  \"note\": \"naïve café — 𝄞 <tag> & \\u003c\\u00e9\\/ kept as written\",\n  \"proof\": [\n")
	for i := 0; b.Len() < 40*1024; i++ {
		if i > 0 {
			b.WriteString(",\n")
		}
		fmt.Fprintf(&b, "    {\"criterion\": \"A6.%d.%d\", \"command\": \"go test ./... -run TestCase%d\", \"observed\": \"ok %s\"}", n, i, i, strings.Repeat("x", 40))
	}
	b.WriteString("\n  ]\n}\n")
	return []byte(b.String())
}

// smallBody is a short handoff of issue's phase word, its summary what.
func (r *handoffRecordsLive) smallBody(issue, word, what string) []byte {
	return []byte(fmt.Sprintf("{\"schemaVersion\": 1, \"phase\": %q, \"issue\": %q, \"summary\": %q, \"proof\": [{\"criterion\": \"live\", \"observed\": %q}]}\n", word, issue, what, what))
}

// capBody is a JSON object of exactly size bytes.
func capBody(size int) []byte {
	const opening, closing = `{"pad":"`, `"}`
	return []byte(opening + strings.Repeat("a", size-len(opening)-len(closing)) + closing)
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// handoffClaimant is one role's claim as its pane reaches it: the shim connected with the launch's
// boot token, the session registered and ready, and a reader that answers what the daemon asks of
// the shim and hands each prompt to the step, starting its turn as the agent would.
type handoffClaimant struct {
	r       *handoffRecordsLive
	token   claim.Token
	issue   string
	session string
	secret  string
	sh      *shim
	mu      sync.Mutex
	turn    bool
	prompts chan shimwire.Prompt
}

// claimant waits for the daemon to launch role's claim on issue and brings it to ready as session.
func (r *handoffRecordsLive) claimant(role claim.Role, issue, session string) *handoffClaimant {
	t := r.t
	t.Helper()
	token := r.token(issue, role)
	testwait.Eventually(t, "the launch of "+string(token), func() bool {
		for _, call := range r.rt.Calls() {
			if (call.Method == "Spawn" || call.Method == "Resume") && call.Spec.Claim == token {
				return true
			}
		}
		return false
	})
	launch := lastLaunch(t, r.rt, token)
	r.built.mu.Lock()
	address := r.built.address
	r.built.mu.Unlock()
	c := &handoffClaimant{r: r, token: token, issue: issue, session: session, sh: dialShim(t, address, launch.BootToken), prompts: make(chan shimwire.Prompt, 8)}
	testwait.Eventually(t, "the shim of "+string(token)+" connected", func() bool {
		return r.d.claim(token).State == string(supervise.StateShimConnected)
	})
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
	c.secret = registered.Secret
	go c.serve()
	if status, body := r.d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: session, Secret: registered.Secret, Generation: launch.Generation,
	}, false); status != http.StatusNoContent {
		t.Fatalf("ready %s = %d %s, want 204", token, status, body)
	}
	r.claimants[token] = c
	r.note("%s's %s is registered and ready as session %s", issue, role, session)
	return c
}

// serve reads every frame the daemon sends the shim until the connection closes: it answers the
// protocol negotiation, the state probe (idle, or streaming while a turn runs) and an abort, and
// takes each prompt - answering it and starting the turn, as Oh My Pi does - for the step to read.
func (c *handoffClaimant) serve() {
	// The reader dialShim read the hello_ack through, which may already hold the frames after it.
	reader := c.sh.lines
	_ = c.sh.conn.SetReadDeadline(time.Time{})
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		frame, err := shimwire.Decode(line)
		if err != nil {
			continue
		}
		switch f := frame.(type) {
		case shimwire.NegotiateProtocol:
			c.send(shimwire.Response{ID: f.ID, Command: shimwire.TypeNegotiateProtocol, Success: true})
		case shimwire.GetState:
			c.mu.Lock()
			streaming := c.turn
			c.mu.Unlock()
			data, _ := json.Marshal(shimwire.StateData{IsStreaming: &streaming})
			c.send(shimwire.Response{ID: f.ID, Command: shimwire.TypeGetState, Success: true, Data: data})
		case shimwire.Abort:
			c.send(shimwire.Response{ID: f.ID, Command: shimwire.TypeAbort, Success: true})
			c.endTurn()
		case shimwire.Prompt:
			c.send(shimwire.Response{ID: f.ID, Command: shimwire.TypePrompt, Success: true})
			c.mu.Lock()
			c.turn = true
			c.mu.Unlock()
			c.send(shimwire.AgentStart{DeliveryID: f.DeliveryID})
			c.prompts <- f
		}
	}
}

// send writes one frame to the daemon; the reader and the step both write.
func (c *handoffClaimant) send(frame shimwire.Frame) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.sh.writer.WriteFrame(frame); err != nil {
		c.r.t.Errorf("send %s for %s: %v", frame.FrameType(), c.token, err)
	}
}

// endTurn ends the turn the last prompt started, when one is running, as the agent's turn ends.
func (c *handoffClaimant) endTurn() {
	c.mu.Lock()
	running := c.turn
	c.turn = false
	c.mu.Unlock()
	if running {
		c.send(shimwire.AgentEnd{})
	}
}

// awaitPrompt waits for the next task the daemon prompts this claim with.
func (c *handoffClaimant) awaitPrompt() shimwire.Prompt {
	c.r.t.Helper()
	select {
	case prompt := <-c.prompts:
		return prompt
	case <-time.After(2 * time.Minute):
		c.r.t.Fatalf("%s was prompted with no task within 2m", c.token)
		return shimwire.Prompt{}
	}
}

// complete posts a completion for this claim with a grant minted for it now, as the `legion`
// tool's handoff_complete does, and returns the status and the refusal, if any.
func (c *handoffClaimant) complete(req api.HandoffCompleteRequest) (int, api.Failure) {
	t := c.r.t
	t.Helper()
	status, body := c.r.d.request(http.MethodPost, "/legion/v1/grants",
		api.GrantRequest{SessionID: c.session, Secret: c.secret, Tree: c.issue, Issue: c.issue}, false)
	if status != http.StatusOK {
		t.Fatalf("the grant of %s = %d %s, want 200", c.token, status, body)
	}
	var granted api.GrantResponse
	if err := json.Unmarshal(body, &granted); err != nil {
		t.Fatalf("decode the grant of %s: %v", c.token, err)
	}
	req.GrantID = granted.GrantID
	status, body = c.r.d.request(http.MethodPost, "/legion/v1/handoff/complete", req, false)
	var failure api.Failure
	if status != http.StatusOK {
		if err := json.Unmarshal(body, &failure); err != nil {
			t.Fatalf("decode the refusal %s: %v", body, err)
		}
	}
	return status, failure
}

// handoffDispatch is the real Dispatch server this run builds and runs: its binary, its own
// database, the port it serves on (kept across a restart, since the daemon's dispatch_url and the
// CLI's DISPATCH_URL name it), and the running process.
type handoffDispatch struct {
	t        *testing.T
	binary   string
	dir      string
	webDist  string
	database string
	token    string
	home     string
	log      io.Writer
	port     int
	base     string
	command  *exec.Cmd
	exited   chan error
	output   *syncBuffer
	client   *http.Client
}

// startHandoffDispatch creates the run's Dispatch database on LEGION_TEST_PG_DSN's server, builds
// cmd/dispatch from root's packages/envoy, and starts it. The database is dropped and the process
// stopped when the test ends. Dispatch's own log goes beside the daemon's in the evidence
// directory when the script names one.
func startHandoffDispatch(t *testing.T, root string) *handoffDispatch {
	t.Helper()
	ctx := context.Background()
	base, err := url.Parse(testDSN(t))
	if err != nil {
		t.Fatalf("parse LEGION_TEST_PG_DSN: %v", err)
	}
	adminURL := *base
	adminURL.Path = "/postgres"
	admin, err := pgxpool.New(ctx, adminURL.String())
	if err != nil {
		t.Fatalf("open the Postgres admin pool: %v", err)
	}
	t.Cleanup(admin.Close)
	name := "dispatch_handoff_records_" + strings.ToLower(randomSuffix(t))
	if _, err := admin.Exec(ctx, "create database "+name); err != nil {
		t.Fatalf("create the Dispatch database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "drop database "+name+" with (force)"); err != nil {
			t.Errorf("drop the Dispatch database %s: %v", name, err)
		}
	})
	serverURL := *base
	serverURL.Path = "/" + name
	d := &handoffDispatch{
		t: t, binary: filepath.Join(t.TempDir(), "dispatch"), dir: filepath.Join(root, "packages", "envoy"),
		webDist: filepath.Join(root, "packages", "dispatch", "web", "dist"), database: serverURL.String(),
		token: "handoff-records-agent-" + strings.ToLower(randomSuffix(t)), home: t.TempDir(), log: io.Discard,
		client: &http.Client{Timeout: 10 * time.Second},
	}
	if daemonLog := os.Getenv(liveDaemonLogEnv); daemonLog != "" {
		file, err := os.OpenFile(filepath.Join(filepath.Dir(daemonLog), "dispatch.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatalf("open Dispatch's log: %v", err)
		}
		t.Cleanup(func() { file.Close() })
		d.log = file
	}
	build := exec.Command("go", "build", "-o", d.binary, "./cmd/dispatch")
	build.Dir = d.dir
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Dispatch: %v\n%s", err, output)
	}
	t.Cleanup(d.stop)
	d.start()
	return d
}

// start runs the server and returns once GET /api/v1 answers 200. The first start takes a free
// port; a restart takes the same one again, retrying while the kernel still holds it.
func (d *handoffDispatch) start() {
	t := d.t
	t.Helper()
	restart := d.port != 0
	for attempt := 1; ; attempt++ {
		if d.port == 0 {
			d.port = boundPort(t)
			d.base = fmt.Sprintf("http://127.0.0.1:%d", d.port)
		}
		command := exec.Command(d.binary)
		command.Dir = d.dir
		// Nothing of the caller's Dispatch, Envoy or NATS settings reaches the scratch server, as
		// run-server.sh drops them: a Legion pane exports DISPATCH_URL and friends.
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "DISPATCH_") && !strings.HasPrefix(entry, "ENVOY_") && !strings.HasPrefix(entry, "NATS_") && !strings.HasPrefix(entry, "HOME=") {
				command.Env = append(command.Env, entry)
			}
		}
		command.Env = append(command.Env,
			"DATABASE_URL="+d.database,
			"DISPATCH_AGENT_TOKEN="+d.token,
			"DISPATCH_IDENTITY=header:X-Dispatch-User",
			"DISPATCH_IDENTITY_HEADER_TRUSTED=1",
			"DISPATCH_NATS_DISABLED=1",
			"DISPATCH_LISTEN_HOST=127.0.0.1",
			fmt.Sprintf("DISPATCH_PORT=%d", d.port),
			"DISPATCH_SERVER_URL="+d.base,
			"DISPATCH_WEB_DIST="+d.webDist,
			"HOME="+d.home,
		)
		output := &syncBuffer{}
		command.Stdout = io.MultiWriter(output, d.log)
		command.Stderr = command.Stdout
		if err := command.Start(); err != nil {
			t.Fatalf("start Dispatch: %v", err)
		}
		exited := make(chan error, 1)
		go func() { exited <- command.Wait() }()
		d.command, d.exited, d.output = command, exited, output
		if d.ready() {
			return
		}
		d.command = nil
		if attempt < 10 && strings.Contains(output.String(), "address already in use") {
			if !restart {
				d.port = 0
			}
			time.Sleep(500 * time.Millisecond)
			continue
		}
		t.Fatalf("Dispatch did not become ready:\n%s", output.String())
	}
}

// ready waits for the started process to answer GET /api/v1, and says whether it did before it
// exited or two minutes passed.
func (d *handoffDispatch) ready() bool {
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-d.exited:
			return false
		default:
		}
		if response, err := pollClient.Get(d.base + "/api/v1"); err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = d.command.Process.Kill()
	<-d.exited
	return false
}

// stop interrupts the running server and waits for it to exit, killing it after ten seconds.
func (d *handoffDispatch) stop() {
	if d.command == nil {
		return
	}
	_ = d.command.Process.Signal(syscall.SIGINT)
	select {
	case <-d.exited:
	case <-time.After(10 * time.Second):
		_ = d.command.Process.Kill()
		<-d.exited
	}
	d.command = nil
}

// user sends one request as the person handoffRecordsUser and requires want, decoding the answer
// into into when it is not nil.
func (d *handoffDispatch) user(method, path string, body any, want int, into any) {
	d.t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		d.t.Fatal(err)
	}
	request, err := http.NewRequest(method, d.base+path, bytes.NewReader(encoded))
	if err != nil {
		d.t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", handoffRecordsUser)
	status, answer := d.do(request)
	if status != want {
		d.t.Fatalf("Dispatch %s %s = %d %s, want %d", method, path, status, answer, want)
	}
	if into != nil {
		if err := json.Unmarshal(answer, into); err != nil {
			d.t.Fatalf("decode Dispatch's answer to %s %s: %v", method, path, err)
		}
	}
}

// agentPost posts body with the agent's bearer token and returns Dispatch's answer as it is.
func (d *handoffDispatch) agentPost(path string, body any) (int, []byte) {
	d.t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		d.t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, d.base+path, bytes.NewReader(encoded))
	if err != nil {
		d.t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+d.token)
	return d.do(request)
}

// agentGet reads path with the agent's bearer token into into, requiring 200.
func (d *handoffDispatch) agentGet(path string, into any) {
	d.t.Helper()
	request, err := http.NewRequest(http.MethodGet, d.base+path, nil)
	if err != nil {
		d.t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+d.token)
	status, answer := d.do(request)
	if status != http.StatusOK {
		d.t.Fatalf("Dispatch GET %s = %d %s, want 200", path, status, answer)
	}
	if err := json.Unmarshal(answer, into); err != nil {
		d.t.Fatalf("decode Dispatch's answer to GET %s: %v", path, err)
	}
}

func (d *handoffDispatch) do(request *http.Request) (int, []byte) {
	d.t.Helper()
	response, err := d.client.Do(request)
	if err != nil {
		d.t.Fatalf("Dispatch %s %s: %v", request.Method, request.URL.Path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		d.t.Fatalf("read Dispatch's answer to %s %s: %v", request.Method, request.URL.Path, err)
	}
	return response.StatusCode, answer
}

// handoffDispatchIssue is the part of a Dispatch issue the seed copies into the daemon's record.
type handoffDispatchIssue struct {
	Title   string `json:"title"`
	Rank    string `json:"rank"`
	LastSeq int64  `json:"last_seq"`
}

func (d *handoffDispatch) issue(key string) handoffDispatchIssue {
	var issue handoffDispatchIssue
	d.agentGet("/api/v1/issues/"+key, &issue)
	return issue
}

// handoffRecord is a handoff as Dispatch's routes answer it.
type handoffRecord struct {
	ID        string `json:"id"`
	IssueKey  string `json:"issue_key"`
	Phase     string `json:"phase"`
	Version   int    `json:"version"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	Author    struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	} `json:"author"`
}

// handoffs is GET /api/v1/issues/<key>/handoffs, newest first: what Conversation lists.
func (d *handoffDispatch) handoffs(key string) []handoffRecord {
	var listed []handoffRecord
	d.agentGet("/api/v1/issues/"+key+"/handoffs", &listed)
	return listed
}

// latest is GET /api/v1/issues/<key>/handoffs/<word>/latest, with its body.
func (d *handoffDispatch) latest(key, word string) handoffRecord {
	var latest handoffRecord
	d.agentGet("/api/v1/issues/"+key+"/handoffs/"+word+"/latest", &latest)
	return latest
}

// handoffRecordsGitHub is GitHub's REST API as `legion handoffs import` reads and writes it for one
// branch, legion/<key> on liveRepo, whose head carries `.legion/<key>/{plan,implement,test}.json`
// and a README beside `.legion/`: the ref, the contents API's listings and files, the commit and
// its trees, and the tree, commit and non-forced ref update the removal writes, each recorded.
// Every other branch is 404, which the import reports as no branch.
type handoffRecordsGitHub struct {
	server *httptest.Server
	key    string
	files  map[string]string
	mu     sync.Mutex
	head   string
	// tree, commit and ref are the bodies of the removal's three writes, as GitHub received them.
	tree, commit, ref map[string]any
}

// The commits and trees the stand-in branch is made of, and what the removal writes.
const (
	handoffGitHead      = "1111111111111111111111111111111111111111"
	handoffGitRootTree  = "2222222222222222222222222222222222222222"
	handoffGitLegion    = "3333333333333333333333333333333333333333"
	handoffGitNewTree   = "4444444444444444444444444444444444444444"
	handoffGitNewCommit = "5555555555555555555555555555555555555555"
)

func newHandoffRecordsGitHub(t *testing.T, key string) *handoffRecordsGitHub {
	t.Helper()
	g := &handoffRecordsGitHub{key: key, head: handoffGitHead, files: map[string]string{
		"plan.json":      fmt.Sprintf("{\n  \"schemaVersion\": 1,\n  \"phase\": \"plan\",\n  \"issue\": %q,\n  \"plan\": \"committed before the cutover\"\n}\n", key),
		"implement.json": fmt.Sprintf("{\n  \"schemaVersion\": 1,\n  \"phase\": \"implement\",\n  \"issue\": %q,\n  \"proof\": [{\"criterion\": \"A7\", \"observed\": \"committed before the cutover\"}]\n}\n", key),
		"test.json":      fmt.Sprintf("{\n  \"schemaVersion\": 1,\n  \"phase\": \"test\",\n  \"issue\": %q,\n  \"verdict\": \"pass\",\n  \"implementerProof\": [\"A7\"]\n}\n", key),
	}}
	g.server = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.server.Close)
	return g
}

func (g *handoffRecordsGitHub) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	repo := "/repos/" + liveRepo
	path := strings.TrimPrefix(r.URL.Path, repo)
	answer := func(value any) { _ = json.NewEncoder(w).Encode(value) }
	notFound := func() { w.WriteHeader(http.StatusNotFound); _, _ = io.WriteString(w, `{"message":"Not Found"}`) }
	write := func(into *map[string]any) bool {
		if err := json.NewDecoder(r.Body).Decode(into); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return false
		}
		return true
	}
	ref := r.URL.Query().Get("ref")
	legion := ".legion/" + g.key
	switch {
	case !strings.HasPrefix(r.URL.Path, repo+"/"):
		notFound()
	case r.Method == http.MethodGet && path == "/git/ref/heads/legion/"+g.key:
		answer(map[string]any{"object": map[string]string{"sha": g.head}})
	case r.Method == http.MethodGet && path == "/contents/.legion" && ref == handoffGitHead:
		answer([]map[string]string{{"name": g.key, "type": "dir"}})
	case r.Method == http.MethodGet && path == "/contents/"+legion && ref == handoffGitHead:
		var entries []map[string]string
		for name := range g.files {
			entries = append(entries, map[string]string{"name": name, "type": "file"})
		}
		answer(entries)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/contents/"+legion+"/") && ref == handoffGitHead:
		text, ok := g.files[strings.TrimPrefix(path, "/contents/"+legion+"/")]
		if !ok {
			notFound()
			return
		}
		answer(map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(text))})
	case r.Method == http.MethodGet && path == "/git/commits/"+handoffGitHead:
		answer(map[string]any{"tree": map[string]string{"sha": handoffGitRootTree}})
	case r.Method == http.MethodGet && path == "/git/trees/"+handoffGitRootTree:
		answer(map[string]any{"sha": handoffGitRootTree, "tree": []map[string]string{
			{"path": ".legion", "mode": "040000", "type": "tree", "sha": handoffGitLegion},
			{"path": "README.md", "mode": "100644", "type": "blob", "sha": "6666666666666666666666666666666666666666"},
		}})
	case r.Method == http.MethodGet && path == "/git/trees/"+handoffGitLegion && r.URL.Query().Get("recursive") == "1":
		entries := []map[string]string{{"path": g.key, "mode": "040000", "type": "tree", "sha": "7777777777777777777777777777777777777777"}}
		for name := range g.files {
			entries = append(entries, map[string]string{"path": g.key + "/" + name, "mode": "100644", "type": "blob", "sha": "8888888888888888888888888888888888888888"})
		}
		answer(map[string]any{"sha": handoffGitLegion, "tree": entries, "truncated": false})
	case r.Method == http.MethodPost && path == "/git/trees":
		if write(&g.tree) {
			w.WriteHeader(http.StatusCreated)
			answer(map[string]string{"sha": handoffGitNewTree})
		}
	case r.Method == http.MethodPost && path == "/git/commits":
		if write(&g.commit) {
			w.WriteHeader(http.StatusCreated)
			answer(map[string]string{"sha": handoffGitNewCommit})
		}
	case r.Method == http.MethodPatch && path == "/git/refs/heads/legion/"+g.key:
		if write(&g.ref) {
			if sha, _ := g.ref["sha"].(string); sha != "" {
				g.head = sha
			}
			answer(map[string]any{"object": map[string]string{"sha": g.head}})
		}
	default:
		notFound()
	}
}

// heldRemoval holds what the import wrote to the branch: one tree on the head's tree deleting each
// `.legion/` file (a nil sha), one commit of it on the head with no trailer and no bracket keyword,
// and a non-forced ref update to that commit.
func (g *handoffRecordsGitHub) heldRemoval(t *testing.T, r *handoffRecordsLive) {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	entries, _ := g.tree["tree"].([]any)
	if g.tree["base_tree"] != handoffGitRootTree || len(entries) != len(g.files) {
		t.Fatalf("the removal's tree = %v, want %d deletions on base_tree %s", g.tree, len(g.files), handoffGitRootTree)
	}
	var deleted []string
	for _, entry := range entries {
		e, _ := entry.(map[string]any)
		if sha, present := e["sha"]; !present || sha != nil {
			t.Fatalf("the removal's tree entry %v does not delete its path (sha null)", e)
		}
		deleted = append(deleted, e["path"].(string))
	}
	message, _ := g.commit["message"].(string)
	parents, _ := g.commit["parents"].([]any)
	if len(parents) != 1 || parents[0] != handoffGitHead || g.commit["tree"] != handoffGitNewTree ||
		strings.Contains(message, "skip-checks") || strings.Contains(message, "[skip") {
		t.Fatalf("the removal's commit = %v, want the new tree on parent %s with no trailer or bracket keyword", g.commit, handoffGitHead)
	}
	if g.ref["sha"] != handoffGitNewCommit || g.ref["force"] != false {
		t.Fatalf("the removal's ref update = %v, want %s with force false", g.ref, handoffGitNewCommit)
	}
	r.note("GitHub stand-in received: POST git/trees base_tree %s deleting %v (sha null); POST git/commits %q on parent %s; PATCH refs/heads/legion/%s sha %s force false",
		handoffGitRootTree, deleted, message, handoffGitHead, g.key, handoffGitNewCommit)
}
