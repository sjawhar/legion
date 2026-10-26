package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// INFRA-225's proof that the daemon itself, and not only its handlers under a unit test, passes a
// blocked issue over: the real Dispatch server with its events on, a real Postgres, a real NATS,
// and a real daemon process-in-process reading Dispatch's events off that bus. Everything the
// test does to Dispatch it does through Dispatch's own API — the issues, their `blocked_by`
// links, and every close — so every decision below is reached from an event the daemon consumed,
// never from a fact or a row the test wrote.
//
// It drives the ten things the feature promises, in one run over one tree of issues, because
// each step is the state the next one starts from:
//
//  1. a blocked root is passed over while a slot stands free, and the roots nothing blocks take
//     the slots instead;
//  2. that root is admitted when its blocker closes — and, as the negative control the step
//     carries, not when the blocker is merely moved to backlog, which stops an issue without
//     finishing it;
//  3. a blocker linked to a root already in its slot does not stop it: the opening of its design
//     gate starts it, and nothing ever names it blocked;
//  4. a child released through release_children is held, the answer naming the issue it waits on,
//     and starts when that issue closes, its tree's architect told so by a child-status notice
//     naming the issue it waited on; and a child released out of backlog is recorded by the
//     release's own todo write, held at admitted and named blocked on that same issue as the
//     answer says, while an unblocked one released beside it is started by its release, and the
//     todo events of those writes, when the daemon's consumer delivers them, change nothing;
//  5. a waiting root a person moves to backlog and back to todo is still checked, since only a tree
//     that ran starts again when Legion takes it back, and is admitted when its blocker closes;
//  6. a root waiting on an issue Legion keeps no record of and nobody handed to it is admitted by
//     that issue's closing event alone, not by the controller tick that follows;
//  7. a held child whose blocked-by links a person takes off starts, and its tree's architect is
//     told by a child-status notice that the links were taken off;
//  8. a child that ran in its tree's earlier run and comes to Legion as a root of its own once
//     that tree finishes is checked like any other root — one set back to todo and held in its
//     tree before the tree closed, and one closed and set back to todo after;
//  9. a tree that ran, taken back after its linger, is admitted at once whatever its links say,
//     beside the two roots of step 8 its blocker still holds: the check exempts the tree Legion
//     takes back and nothing else, so step 8 cannot pass by checking everything;
//  10. the roots of step 8 are admitted when that blocker closes.
func TestBlockedIssuesAreNotStartedAgainstScratchDispatch(t *testing.T) {
	// The NATS server is taken first: testnats.URL empties it of every stream, and Dispatch's own
	// start reconciles ENVOY_NOTIFICATIONS on it, which the daemon then consumes from.
	natsURL := testnats.URL(t)
	baseURL, token := startScratchDispatchWithNATS(t, natsURL)

	cfg := workflowConfig(t, natsURL)
	// Dispatch takes a project key of at most ten characters; workflowConfig's is longer.
	cfg.Project = "B" + randomSuffix(t)[:9]
	cfg.Projects = map[string]config.Project{cfg.Project: {}}
	cfg.PostgresDSN = blockersDatabase(t)
	// Three slots for three roots, so the slot the blocked root does not take stands visibly free.
	cfg.AdmissionCap = 3
	// The controller tick admits a root whose blocker is done whatever the event did, so it must
	// not be what step 6 sees: an hour before the runtime's first tick (o.controllerFirstWake,
	// below) and an hour between ticks mean this run applies no tick at all.
	cfg.ControllerWakeInterval = time.Hour
	// With the design gate off, the architect's own registration opens its tree, which is what
	// makes starting a released child possible at all.
	cfg.Gates.Design = config.DesignGateOff
	cfg.DispatchURL = baseURL
	if err := os.WriteFile(cfg.DispatchTokenFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatalf("write the scratch Dispatch bearer: %v", err)
	}
	dispatchCall(t, baseURL, http.MethodPost, "/api/v1/projects",
		map[string]string{"key": cfg.Project, "name": "Blocked issues"}, nil)
	dispatchCall(t, baseURL, http.MethodPut, "/api/v1/settings/repo-projects/acme/widgets",
		map[string]any{"project": cfg.Project, "default": true}, nil)

	rt := fake.NewRuntime()
	// A pods runtime provisions each claim's workspace itself, so the daemon clones nothing: these
	// issues name a repository that exists only in this test's GitHub stand-in.
	rt.InPod = true
	o := fakeRuntime(rt, &built{})
	o.workflowTokens = &workflowTokenRecorder{}
	o.githubAPI = blockersGitHub(t)
	o.controllerFirstWake = time.Hour
	d := startDaemon(t, cfg, o)

	// The blocker is itself a root Legion works, which is what a blocked_by link between two
	// Legion issues looks like. Dispatch ranks a new issue after the last, so the waiting line is
	// the order they are created in: the blocked root stands ahead of the root admitted in its
	// place.
	blocker := createBlockersIssue(t, baseURL, cfg.Project, "The blocker", "", nil)
	blocked := createBlockersIssue(t, baseURL, cfg.Project, "The blocked root", "", []string{blocker})
	free := createBlockersIssue(t, baseURL, cfg.Project, "The root behind it", "", nil)
	for _, key := range []string{blocker, blocked, free} {
		setDispatchStatus(t, baseURL, key, "todo")
	}

	// 1. The blocked root is passed over; the root behind it takes a slot, and one slot is left.
	state, body := settled(t, d, "admit the roots nothing blocks", func(s api.State) bool {
		return slices.Contains(s.Admission.Active, blocker) && slices.Contains(s.Admission.Active, free)
	})
	t.Logf("a blocked root passed over while a slot stood free:\n%s", body)
	if slices.Contains(state.Admission.Active, blocked) {
		t.Fatalf("Legion admitted %s although %s, the issue it waits on, is not done; active = %v\nstate %s",
			blocked, blocker, state.Admission.Active, body)
	}
	if !slices.Contains(state.Admission.Waiting, blocked) {
		t.Fatalf("Legion dropped %s out of the waiting line %v; a blocked root keeps its place\nstate %s",
			blocked, state.Admission.Waiting, body)
	}
	if waits, ok := blockedOn(state, blocked); !ok || !slices.Equal(waits, []string{blocker}) {
		t.Fatalf("the state's admission.blocked is %v; want %s named as waiting on %s\nstate %s",
			state.Admission.Blocked, blocked, blocker, body)
	}
	if len(state.Admission.Active) >= state.Admission.Cap {
		t.Fatalf("Legion took all %d slots with %v, so no slot stood free while %s was passed over\nstate %s",
			state.Admission.Cap, state.Admission.Active, blocked, body)
	}

	// 2a. The negative control: a blocker moved to backlog has stopped, not finished. Admission
	// demonstrably ran on that very event — it released the blocker's own slot — and still left
	// the blocked root where it stood.
	awaitDispatchStatus(t, baseURL, blocker, "in_progress")
	setDispatchStatus(t, baseURL, blocker, "backlog")
	state, body = settled(t, d, "release the slot of the blocker moved to backlog", func(s api.State) bool {
		return !slices.Contains(s.Admission.Active, blocker)
	})
	t.Logf("the blocker moved to backlog is not finished, so the root is still passed over:\n%s", body)
	if slices.Contains(state.Admission.Active, blocked) {
		t.Fatalf("Legion admitted %s when %s went to backlog; only done finishes a blocker\nstate %s", blocked, blocker, body)
	}
	if waits, ok := blockedOn(state, blocked); !ok || !slices.Equal(waits, []string{blocker}) {
		t.Fatalf("the state's admission.blocked is %v after %s went to backlog; want %s still waiting on it\nstate %s",
			state.Admission.Blocked, blocker, blocked, body)
	}

	// 2b. The close is a real Dispatch write, and the daemon admits the root off its event.
	setDispatchStatus(t, baseURL, blocker, "done")
	state, body = settled(t, d, "admit the blocked root once its blocker closed", func(s api.State) bool {
		return slices.Contains(s.Admission.Active, blocked)
	})
	t.Logf("the blocker closed through Dispatch, so the root is admitted:\n%s", body)
	if waits, ok := blockedOn(state, blocked); ok {
		t.Fatalf("%s is admitted and the state still lists it blocked by %v\nstate %s", blocked, waits, body)
	}

	// 3. A blocker linked to a root already in its slot does not stop it. A root's links are read
	// once, when promotion admits it into a slot, and the root behind the blocked one is past that
	// moment: its architect is launched and its tree runs. A person now links it to an issue nobody
	// has finished — another root Legion works, admitted into the slot left free and running — and
	// the opening of the root's design gate starts it all the same: a tree already running is not
	// stopped, paused or re-checked. The grant that registers the gate is the real one its launched
	// claim mints.
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatalf("the project token of %s: %v", cfg.Project, err)
	}
	architect, err := claim.NewToken(project, free, claim.RoleArchitect)
	if err != nil {
		t.Fatalf("the architect claim token of %s: %v", free, err)
	}
	testwait.Eventually(t, "the daemon to launch the architect of "+free, func() bool {
		for _, call := range rt.Calls() {
			if (call.Method == "Spawn" || call.Method == "Resume") && call.Spec.Claim == architect {
				return true
			}
		}
		return false
	})
	grant := architectGrants(t, d, lastLaunch(t, rt, architect), free)

	// Dispatch refuses a new issue whose title shares a word with one already in its project as a
	// possible duplicate, so the issues this step and the last add share no word with those before.
	late := createBlockersIssue(t, baseURL, cfg.Project, "Prerequisite work in flight", "", nil)
	setDispatchStatus(t, baseURL, late, "todo")
	settled(t, d, "admit "+late+" into the slot left free", func(s api.State) bool {
		return slices.Contains(s.Admission.Active, late)
	})
	awaitDispatchStatus(t, baseURL, late, "in_progress")
	awaitDispatchStatus(t, baseURL, free, "in_progress")
	var linked struct {
		BlockedBy []string `json:"blocked_by"`
		LastSeq   int64    `json:"last_seq"`
	}
	dispatchCall(t, baseURL, http.MethodPatch, "/api/v1/issues/"+free, map[string]any{"blocked_by": []string{late}}, &linked)
	if !slices.Equal(linked.BlockedBy, []string{late}) {
		t.Fatalf("Dispatch answered the link of %s to %s with blocked_by %v", free, late, linked.BlockedBy)
	}
	// The state names no slotted root in `blocked`, so the link's arrival is read off the daemon's
	// own mirror of Dispatch, at the sequence Dispatch gave the link. Every state read on the way to
	// the root's first phase is judged as well: nothing holds a root in its slot, so none may name it.
	pool, err := pgxpool.New(t.Context(), cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the daemon's database: %v", err)
	}
	t.Cleanup(pool.Close)
	var namedSlotted []api.BlockedIssue
	judgeSlotted := func(s api.State) {
		if _, ok := blockedOn(s, free); ok && namedSlotted == nil {
			namedSlotted = s.Admission.Blocked
		}
	}
	var mirrored []string
	var mirroredSeq int64
	settled(t, d, "consume the event of "+free+"'s link to "+late, func(s api.State) bool {
		judgeSlotted(s)
		if err := pool.QueryRow(t.Context(), `select blockers, dispatch_seq from dispatch_issues where key = $1`, free).
			Scan(&mirrored, &mirroredSeq); err != nil {
			t.Fatalf("read the daemon's mirror of %s: %v", free, err)
		}
		return mirroredSeq >= linked.LastSeq
	})
	state, body = settled(t, d, "answer its own state", func(api.State) bool { return true })
	judgeSlotted(state)
	t.Logf("the daemon consumed the link of %s, in its slot, to %s, mirroring it at Dispatch's sequence %d as waiting on %v:\n%s",
		free, late, mirroredSeq, mirrored, body)
	if !slices.Equal(mirrored, []string{late}) {
		t.Fatalf("the daemon mirrors %s at Dispatch's sequence %d as waiting on %v; want %s, the link Dispatch made at sequence %d\nstate %s",
			free, mirroredSeq, mirrored, late, linked.LastSeq, body)
	}
	if !slices.Contains(state.Admission.Active, free) || state.Issues[free].Phase != phase.Admitted {
		t.Fatalf("Legion shows %s at %s with active = %v once it consumed the link; want it in its slot at %s, its gate not yet registered\nstate %s",
			free, state.Issues[free].Phase, state.Admission.Active, phase.Admitted, body)
	}

	// The tree's design gate is what lets any child of it start; with the policy off, registering
	// the root's own document opens it.
	if status, answer := d.request(http.MethodPost, "/legion/v1/gates/register", api.GateRegisterRequest{
		GrantID: grant(), Issue: free, ArtifactID: dispatchPrimaryArtifact(t, baseURL, free), Version: 1,
	}, false); status != http.StatusOK {
		t.Fatalf("the architect's design-gate registration on %s = %d; body %s", free, status, answer)
	}
	state, body = settled(t, d, "record the design gate of "+free+" open", func(s api.State) bool {
		judgeSlotted(s)
		gate := s.Issues[free].DesignGate
		return gate != nil && gate.ApprovedVersion != nil
	})
	t.Logf("the state once the design gate of %s, in its slot and linked to %s after its admission, opened:\n%s", free, late, body)
	if got := state.Issues[free].Phase; got != phase.Planning {
		t.Fatalf("Legion opened the design gate of %s and left the root at %s rather than starting %s: it judged a root already in "+
			"its slot by %s, linked after its admission, so the root holds its slot with nothing left to start it\nstate %s",
			free, got, phase.Planning, late, body)
	}
	if namedSlotted != nil {
		t.Fatalf("Legion named %s, a root in its slot, in admission.blocked = %v; a root in its slot is held by nothing\nstate %s",
			free, namedSlotted, body)
	}

	// 4. The architect of the root behind the blocked one releases a child that waits on an issue
	// Dispatch has not finished.
	child := createBlockersIssue(t, baseURL, cfg.Project, "The held child", free, []string{blocked})
	setDispatchStatus(t, baseURL, child, "todo")
	_, body = settled(t, d, "record the child "+child+" under "+free, func(s api.State) bool {
		recorded, ok := s.Issues[child]
		return ok && recorded.Phase == phase.Admitted
	})
	t.Logf("the child is recorded and held at %s before the release:\n%s", phase.Admitted, body)

	status, answer := d.request(http.MethodPost, "/legion/v1/waves/release",
		api.WaveReleaseRequest{GrantID: grant(), Issues: []string{child}}, false)
	if status != http.StatusOK {
		t.Fatalf("release_children of %s = %d; body %s", child, status, answer)
	}
	var released api.WaveReleaseResponse
	if err := json.Unmarshal(answer, &released); err != nil {
		t.Fatalf("decode the release of %s from %s: %v", child, answer, err)
	}
	want := api.WaveReleaseResponse{
		Released: []string{child}, Started: []string{},
		Held: []api.HeldChild{{Issue: child, BlockedBy: []string{blocked}}},
	}
	if !reflect.DeepEqual(released, want) {
		state, body = settled(t, d, "answer its own state", func(api.State) bool { return true })
		t.Fatalf("release_children answered %s; want %s held on %s and nothing started. The daemon's own state names that "+
			"same wait at the same moment, admission.blocked = %v\nstate %s", answer, child, blocked, state.Admission.Blocked, body)
	}

	// The same release out of backlog: Dispatch shows these two children in backlog, so the release
	// writes them todo, and records each from its own write, in the request, at the sequence
	// Dispatch answered the write with: the blocked child held at admitted and named in
	// admission.blocked on the issue the answer named, the free one started, as the answer says.
	// Before, each was recorded only once its write's todo event came back through the daemon's
	// Dispatch consumer, after the answer, so a consumer running half an hour behind left both
	// unrecorded, and the free one unstarted, for that half hour.
	stalled := createBlockersIssue(t, baseURL, cfg.Project, "Parked schema cleanup", free, []string{blocked})
	ready := createBlockersIssue(t, baseURL, cfg.Project, "Queued logging refactor", free, nil)
	for _, key := range []string{stalled, ready} {
		setDispatchStatus(t, baseURL, key, "backlog")
	}
	state, body = settled(t, d, "mirror "+stalled+" and "+ready+" in backlog", func(api.State) bool {
		for _, key := range []string{stalled, ready} {
			var status string
			if err := pool.QueryRow(t.Context(), `select status from dispatch_issues where key = $1`, key).Scan(&status); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return false
				}
				t.Fatalf("read the daemon's mirror of %s: %v", key, err)
			}
			if status != "backlog" {
				return false
			}
		}
		return true
	})
	for _, key := range []string{stalled, ready} {
		if recorded, ok := state.Issues[key]; ok {
			t.Fatalf("Legion keeps a record of %s at %s while Dispatch shows it in backlog; want none before its release\nstate %s",
				key, recorded.Phase, body)
		}
	}
	t.Logf("the daemon mirrors %s and %s in backlog and keeps no record of either before the release:\n%s", stalled, ready, body)

	// On this bench the release's todo events can reach the daemon before its answer, and a record
	// one of them wrote would look the same as one the release wrote. What tells them apart is the
	// fact path's own ledger of every fact it applied (processed_events): the release's own writes
	// are applied as facts of the API under the own-write id, which no Dispatch event ever is.
	ownWrites := func() int {
		t.Helper()
		var count int
		if err := pool.QueryRow(t.Context(), `select count(*) from processed_events where source = 'api' and event_id like 'api:status/own-write:%'`).
			Scan(&count); err != nil {
			t.Fatalf("read the facts the API applied for its own status writes: %v", err)
		}
		return count
	}
	ownWritesBefore := ownWrites()
	status, answer = d.request(http.MethodPost, "/legion/v1/waves/release",
		api.WaveReleaseRequest{GrantID: grant(), Issues: []string{stalled, ready}}, false)
	appliedByRelease := ownWrites() - ownWritesBefore
	var recordedAtAnswer int
	if err := pool.QueryRow(t.Context(), `select count(*) from issues where key = any($1)`, []string{stalled, ready}).
		Scan(&recordedAtAnswer); err != nil {
		t.Fatalf("read the daemon's records of %s and %s as the release answered: %v", stalled, ready, err)
	}
	if status != http.StatusOK {
		t.Fatalf("release_children of %s and %s = %d; body %s", stalled, ready, status, answer)
	}
	if appliedByRelease != 2 || recordedAtAnswer != 2 {
		t.Fatalf("release_children of %s and %s out of backlog applied %d of its own todo writes and the daemon recorded %d of the two "+
			"as it answered; want both writes applied by the release itself and both children recorded, not left to their todo events "+
			"coming back through the Dispatch consumer", stalled, ready, appliedByRelease, recordedAtAnswer)
	}
	released = api.WaveReleaseResponse{}
	if err := json.Unmarshal(answer, &released); err != nil {
		t.Fatalf("decode the release of %s and %s from %s: %v", stalled, ready, answer, err)
	}
	t.Logf("release_children of %s and %s out of backlog answered %s", stalled, ready, answer)
	want = api.WaveReleaseResponse{
		Released: []string{stalled, ready}, Started: []string{ready},
		Held: []api.HeldChild{{Issue: stalled, BlockedBy: []string{blocked}}},
	}
	if !reflect.DeepEqual(released, want) {
		t.Fatalf("release_children answered %s; want %s held on %s and %s started, both by the release's own writes", answer, stalled, blocked, ready)
	}
	state, body = settled(t, d, "read "+stalled+" and "+ready+" as the release recorded them", func(api.State) bool { return true })
	t.Logf("the state once the release of %s and %s answered:\n%s", stalled, ready, body)
	if waits, ok := blockedOn(state, stalled); state.Issues[stalled].Phase != phase.Admitted || !ok || !slices.Equal(waits, []string{blocked}) {
		t.Fatalf("Legion took %s, answered held on %s, to %s with admission.blocked = %v; want it held at %s waiting on %s, "+
			"as the answer said\nstate %s", stalled, blocked, state.Issues[stalled].Phase, state.Admission.Blocked, phase.Admitted, blocked, body)
	}
	if got := state.Issues[ready].Phase; got != phase.Planning {
		t.Fatalf("the free child %s is at %s after its release; want its first phase, %s\nstate %s", ready, got, phase.Planning, body)
	}
	if waits, ok := blockedOn(state, ready); ok {
		t.Fatalf("%s started and the state lists it blocked by %v\nstate %s", ready, waits, body)
	}
	releasedRun := map[string]uint64{stalled: state.Issues[stalled].Generation, ready: state.Issues[ready].Generation}

	awaitDispatchStatus(t, baseURL, blocked, "in_progress")
	setDispatchStatus(t, baseURL, blocked, "done")
	state, body = settled(t, d, "start the held child once its blocker closed", func(s api.State) bool {
		recorded, ok := s.Issues[child]
		return ok && recorded.Phase != phase.Admitted
	})
	t.Logf("the released child started once its blocker closed:\n%s", body)
	if got := state.Issues[child].Phase; got != phase.Planning {
		t.Fatalf("the child %s left %s into %s; want its first phase, %s\nstate %s", child, phase.Admitted, got, phase.Planning, body)
	}
	// The tree's architect is told the child it held starts, and why: the child-status notice the
	// daemon queued on the child, in the very transaction that started it, naming the issue it
	// waited on.
	notices := childStatusNotices(t, pool, child)
	t.Logf("the child-status notices the daemon queued on %s for its tree's architect: %q", child, notices)
	wantNotice := fmt.Sprintf("%s starts planning under %s: every issue it waited on (%s) is done", child, free, blocked)
	if !slices.Equal(notices, []string{wantNotice}) {
		t.Fatalf("Legion started the held child %s once %s closed and queued its tree's architect the child-status notices %q; "+
			"want the one reading %q\nstate %s", child, blocked, notices, wantNotice, body)
	}

	// The child released out of backlog was held on that same issue, so its close starts it too.
	// Both children of that release then close, as a person would, so that no child of theirs is
	// still running when the tree's root closes in step 8.
	state, body = settled(t, d, "start the held child "+stalled+" once its blocker closed", func(s api.State) bool {
		return s.Issues[stalled].Phase != phase.Admitted
	})
	if got := state.Issues[stalled].Phase; got != phase.Planning {
		t.Fatalf("the child %s left %s into %s once %s closed; want its first phase, %s\nstate %s",
			stalled, phase.Admitted, got, blocked, phase.Planning, body)
	}
	// The release's own todo events reached the daemon before the close of the issue they waited
	// on, which Dispatch published after them, and changed nothing: each child is still in the run
	// its release recorded, the free one still planning.
	for key, run := range releasedRun {
		if got := state.Issues[key].Generation; got != run {
			t.Fatalf("the released child %s is in run %d once its todo event landed; want run %d, the one its release recorded\nstate %s",
				key, got, run, body)
		}
	}
	if got := state.Issues[ready].Phase; got != phase.Planning {
		t.Fatalf("the free child %s moved to %s once its todo event landed; want it still planning\nstate %s", ready, got, body)
	}
	for _, key := range []string{stalled, ready} {
		setDispatchStatus(t, baseURL, key, "done")
	}
	settled(t, d, "record "+stalled+" and "+ready+" done once a person closed them", func(s api.State) bool {
		return s.Issues[stalled].Phase == phase.Done && s.Issues[ready].Phase == phase.Done
	})

	// 5. A waiting root a person takes off the board and back is still checked. A root linked to the
	// late blocker waits in line while a slot stands free; moved to backlog it leaves the line, and
	// set back to todo it returns to it, still waiting: it never held a slot and never ran a phase,
	// so there is no tree of its that Legion takes back, and only a tree that ran starts again
	// whatever its links say. The close of its blocker, a real Dispatch write, is what admits it.
	returned := createBlockersIssue(t, baseURL, cfg.Project, "Taken off the board and back", "", []string{late})
	held := func(when string, state api.State, body string) {
		t.Helper()
		if slices.Contains(state.Admission.Active, returned) {
			t.Fatalf("Legion admitted %s %s, though %s, the issue it waits on, is not done and %s never held a slot or ran a "+
				"phase, which is all that would start it whatever its links say; active = %v\nstate %s",
				returned, when, late, returned, state.Admission.Active, body)
		}
		if !slices.Contains(state.Admission.Waiting, returned) {
			t.Fatalf("Legion left %s out of the waiting line %v %s; a blocked root keeps its place\nstate %s",
				returned, state.Admission.Waiting, when, body)
		}
		if waits, ok := blockedOn(state, returned); !ok || !slices.Equal(waits, []string{late}) {
			t.Fatalf("the state's admission.blocked is %v %s; want %s named as waiting on %s\nstate %s",
				state.Admission.Blocked, when, returned, late, body)
		}
		if len(state.Admission.Active) >= state.Admission.Cap {
			t.Fatalf("Legion took all %d slots with %v %s, so its blocker was not all that held %s\nstate %s",
				state.Admission.Cap, state.Admission.Active, when, returned, body)
		}
	}
	setDispatchStatus(t, baseURL, returned, "todo")
	state, body = settled(t, d, "take "+returned+" into its waiting line or a slot", func(s api.State) bool {
		return slices.Contains(s.Admission.Waiting, returned) || slices.Contains(s.Admission.Active, returned)
	})
	t.Logf("the state once %s, waiting on %s, was handed to Legion:\n%s", returned, late, body)
	held("when it was handed to Legion", state, body)

	setDispatchStatus(t, baseURL, returned, "backlog")
	state, body = settled(t, d, "take "+returned+" out of the waiting line once it went to backlog", func(s api.State) bool {
		return !slices.Contains(s.Admission.Waiting, returned)
	})
	t.Logf("the waiting root moved to backlog has left the line:\n%s", body)
	if slices.Contains(state.Admission.Active, returned) {
		t.Fatalf("Legion admitted %s when a person moved it to backlog; active = %v\nstate %s", returned, state.Admission.Active, body)
	}
	if waits, ok := blockedOn(state, returned); ok {
		t.Fatalf("%s left the waiting line for backlog and the state still lists it blocked by %v\nstate %s", returned, waits, body)
	}

	// Back at todo the root is either admitted, which is the defect, or waiting and named blocked
	// again. The wait ends on whichever Legion does, so the assertion judges what it actually did.
	setDispatchStatus(t, baseURL, returned, "todo")
	state, body = settled(t, d, "take "+returned+" back once a person set it to todo again", func(s api.State) bool {
		_, named := blockedOn(s, returned)
		return slices.Contains(s.Admission.Active, returned) || (slices.Contains(s.Admission.Waiting, returned) && named)
	})
	t.Logf("the state once %s, taken off the board, was set back to todo while %s is unfinished:\n%s", returned, late, body)
	held("once a person set it back to todo", state, body)

	setDispatchStatus(t, baseURL, late, "done")
	state, body = settled(t, d, "admit "+returned+" once "+late+" closed", func(s api.State) bool {
		return slices.Contains(s.Admission.Active, returned)
	})
	t.Logf("the late blocker closed through Dispatch, so the root taken off the board and back is admitted:\n%s", body)
	if waits, ok := blockedOn(state, returned); ok {
		t.Fatalf("%s is admitted and the state still lists it blocked by %v\nstate %s", returned, waits, body)
	}

	// 6. The blocker is an issue Legion keeps no record of: created without the legion label, so
	// nobody handed it to Legion, and never in todo, so admission never records it. Someone else
	// works it, and its close, a real Dispatch write, is what admits the root waiting on it.
	outside := createUnhandedIssue(t, baseURL, cfg.Project, "External library patch")
	setDispatchStatus(t, baseURL, outside, "in_progress")
	awaiting := createBlockersIssue(t, baseURL, cfg.Project, "Adopt newer dependency release", "", []string{outside})
	setDispatchStatus(t, baseURL, awaiting, "todo")
	state, body = settled(t, d, "take "+awaiting+" into its waiting line or a slot", func(s api.State) bool {
		return slices.Contains(s.Admission.Waiting, awaiting) || slices.Contains(s.Admission.Active, awaiting)
	})
	t.Logf("the state once %s, waiting on %s, which nobody handed to Legion, was handed to Legion:\n%s", awaiting, outside, body)
	if slices.Contains(state.Admission.Active, awaiting) {
		t.Fatalf("Legion admitted %s though %s, the issue it waits on, is in progress; active = %v\nstate %s",
			awaiting, outside, state.Admission.Active, body)
	}
	if waits, ok := blockedOn(state, awaiting); !ok || !slices.Equal(waits, []string{outside}) {
		t.Fatalf("the state's admission.blocked is %v; want %s named as waiting on %s\nstate %s",
			state.Admission.Blocked, awaiting, outside, body)
	}
	if len(state.Admission.Active) >= state.Admission.Cap {
		t.Fatalf("Legion took all %d slots with %v, so its blocker is not all that holds %s\nstate %s",
			state.Admission.Cap, state.Admission.Active, awaiting, body)
	}
	if _, recorded := state.Issues[outside]; recorded {
		t.Fatalf("Legion keeps a record of %s, which nobody handed to it\nstate %s", outside, body)
	}

	// The tick cannot be what admits the root: the first tick is an hour after the runtime starts
	// and the next an hour after that, so no tick runs in this test at all, and the judgement below
	// that none was applied between the close and the admission holds by construction, not by when
	// the close happens to land.
	ticks := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(t.Context(), `select count(*) from processed_events where source = 'controller' and event_id like 'controller-tick:%'`).
			Scan(&n); err != nil {
			t.Fatalf("read the controller ticks the daemon applied: %v", err)
		}
		return n
	}
	ticked := ticks()
	closed := time.Now()
	setDispatchStatus(t, baseURL, outside, "done")
	state, body = settled(t, d, "admit "+awaiting+" once "+outside+", which Legion keeps no record of, closed", func(s api.State) bool {
		return slices.Contains(s.Admission.Active, awaiting)
	})
	took := time.Since(closed)
	var mirroredStatus string
	if err := pool.QueryRow(t.Context(), `select status from dispatch_issues where key = $1`, outside).Scan(&mirroredStatus); err != nil {
		t.Fatalf("read the daemon's mirror of %s: %v", outside, err)
	}
	t.Logf("%s closed through Dispatch, mirrored %s, and %s was admitted %s after the close:\n%s",
		outside, mirroredStatus, awaiting, took, body)
	if now := ticks(); now != ticked {
		t.Fatalf("%d controller ticks were applied between the close of %s and the admission of %s, %s after it: the tick, not "+
			"the close, may have admitted it\nstate %s", now-ticked, outside, awaiting, took, body)
	}
	if waits, ok := blockedOn(state, awaiting); ok {
		t.Fatalf("%s is admitted and the state still lists it blocked by %v\nstate %s", awaiting, waits, body)
	}
	if _, recorded := state.Issues[outside]; recorded {
		t.Fatalf("Legion recorded %s on its close, though nobody handed it to Legion\nstate %s", outside, body)
	}

	// 7. A held child whose blocked-by links a person takes off starts, and its tree's architect is
	// told why. It waits on an issue someone else works, which stays unfinished until step 10: the
	// edit that takes the link off changes no status and closes nothing, so what starts the child is
	// the look at every held child that each event the daemon consumes runs.
	elsewhere := createUnhandedIssue(t, baseURL, cfg.Project, "Vendor firmware upgrade")
	setDispatchStatus(t, baseURL, elsewhere, "in_progress")
	unlinked := createBlockersIssue(t, baseURL, cfg.Project, "Unlinked sibling task", free, []string{elsewhere})
	setDispatchStatus(t, baseURL, unlinked, "todo")
	settled(t, d, "record the child "+unlinked+" under "+free, func(s api.State) bool {
		recorded, ok := s.Issues[unlinked]
		return ok && recorded.Phase == phase.Admitted
	})
	status, answer = d.request(http.MethodPost, "/legion/v1/waves/release",
		api.WaveReleaseRequest{GrantID: grant(), Issues: []string{unlinked}}, false)
	if status != http.StatusOK {
		t.Fatalf("release_children of %s = %d; body %s", unlinked, status, answer)
	}
	released = api.WaveReleaseResponse{}
	if err := json.Unmarshal(answer, &released); err != nil {
		t.Fatalf("decode the release of %s from %s: %v", unlinked, answer, err)
	}
	want = api.WaveReleaseResponse{
		Released: []string{unlinked}, Started: []string{},
		Held: []api.HeldChild{{Issue: unlinked, BlockedBy: []string{elsewhere}}},
	}
	if !reflect.DeepEqual(released, want) {
		t.Fatalf("release_children answered %s; want %s held on %s and nothing started", answer, unlinked, elsewhere)
	}
	var unlinking struct {
		BlockedBy []string `json:"blocked_by"`
	}
	dispatchCall(t, baseURL, http.MethodPatch, "/api/v1/issues/"+unlinked, map[string]any{"blocked_by": []string{}}, &unlinking)
	if len(unlinking.BlockedBy) != 0 {
		t.Fatalf("Dispatch answered the removal of %s's links with blocked_by %v", unlinked, unlinking.BlockedBy)
	}
	state, body = settled(t, d, "start the held child "+unlinked+" once its links were taken off", func(s api.State) bool {
		recorded, ok := s.Issues[unlinked]
		return ok && recorded.Phase != phase.Admitted
	})
	t.Logf("the held child %s started once a person took its links off, %s still unfinished:\n%s", unlinked, elsewhere, body)
	if got := state.Issues[unlinked].Phase; got != phase.Planning {
		t.Fatalf("the child %s left %s into %s once its links were taken off; want its first phase, %s\nstate %s",
			unlinked, phase.Admitted, got, phase.Planning, body)
	}
	if waits, ok := blockedOn(state, unlinked); ok {
		t.Fatalf("%s started and the state still lists it blocked by %v\nstate %s", unlinked, waits, body)
	}
	notices = childStatusNotices(t, pool, unlinked)
	t.Logf("the child-status notices the daemon queued on %s for its tree's architect: %q", unlinked, notices)
	wantNotice = fmt.Sprintf("%s starts planning under %s: its blocked-by links were taken off, so it waits on nothing", unlinked, free)
	if !slices.Equal(notices, []string{wantNotice}) {
		t.Fatalf("Legion started the held child %s once a person took its links off and queued its tree's architect the child-status "+
			"notices %q; want the one reading %q: a start nobody hears of is the silence the hold's report exists to remove\nstate %s",
			unlinked, notices, wantNotice, body)
	}

	// 8. A child that ran in its tree's earlier run and comes to Legion as a root of its own once
	// that tree finishes is checked like any other root: what it ran was its old tree's work, and
	// the tree it now roots has run nothing, so it is no tree Legion takes back. Two such children
	// are driven, both waiting on the issue of step 7. The child of step 4, which ran planning, is
	// moved on to in_progress, as a later phase's status write would leave it, and set back to todo
	// while it waits on that issue: its tree's next run of it is held at admitted, and the tree's
	// close hands it over, since its blocker outlives the tree. The child of step 7, which ran
	// planning too, is linked to that issue and closed, and once its tree has finished a person sets
	// it back to todo: an orphan of a finished tree. testConfig leaves the linger at zero, so a
	// root's tree finishes as it closes. A child's planning writes no status of its own, so no write
	// of the daemon's races the test's on either child.
	//
	// The roots of steps 5 and 6 finish first, so that the slots those two children and step 9's
	// root later take stand free.
	for _, key := range []string{returned, awaiting} {
		awaitDispatchStatus(t, baseURL, key, "in_progress")
		setDispatchStatus(t, baseURL, key, "done")
	}
	state, body = settled(t, d, "release the slots of "+returned+" and "+awaiting+" once they closed", func(s api.State) bool {
		return !slices.Contains(s.Admission.Active, returned) && !slices.Contains(s.Admission.Active, awaiting)
	})
	t.Logf("the roots of steps 5 and 6 closed through Dispatch, leaving their slots free:\n%s", body)
	checked := func(issue, when string, state api.State, body string) {
		t.Helper()
		if slices.Contains(state.Admission.Active, issue) {
			t.Fatalf("Legion admitted %s %s, though %s, the issue it waits on, is not done: %s ran in its old tree's run, which is "+
				"that tree's work, and the root of its own it became has run nothing; active = %v\nstate %s",
				issue, when, elsewhere, issue, state.Admission.Active, body)
		}
		if !slices.Contains(state.Admission.Waiting, issue) {
			t.Fatalf("Legion left %s out of the waiting line %v %s; a blocked root keeps its place\nstate %s",
				issue, state.Admission.Waiting, when, body)
		}
		if waits, ok := blockedOn(state, issue); !ok || !slices.Equal(waits, []string{elsewhere}) {
			t.Fatalf("the state's admission.blocked is %v %s; want %s named as waiting on %s\nstate %s",
				state.Admission.Blocked, when, issue, elsewhere, body)
		}
		if len(state.Admission.Active) >= state.Admission.Cap {
			t.Fatalf("Legion took all %d slots with %v %s, so its blocker was not all that held %s\nstate %s",
				state.Admission.Cap, state.Admission.Active, when, issue, body)
		}
	}

	// The planner the child's run launched registers, as its plugin does once its agent boots. The
	// re-entry below suspends it, and a suspend reaching an agent still booting is held until the
	// agent is ready; this harness boots no agent, so unregistered it would stay launching under
	// the tree it ran in, and the cleanup of that tree, which step 9's re-admission waits on, would
	// wait on it in turn.
	planner, err := claim.NewToken(project, child, claim.RolePlanner)
	if err != nil {
		t.Fatalf("the planner claim token of %s: %v", child, err)
	}
	testwait.Eventually(t, "the daemon to launch the planner of "+child, func() bool {
		for _, call := range rt.Calls() {
			if (call.Method == "Spawn" || call.Method == "Resume") && call.Spec.Claim == planner {
				return true
			}
		}
		return false
	})
	registerLaunch(t, d, lastLaunch(t, rt, planner))

	ran := state.Issues[child].Generation
	setDispatchStatus(t, baseURL, child, "in_progress")
	var relinked struct {
		BlockedBy []string `json:"blocked_by"`
	}
	dispatchCall(t, baseURL, http.MethodPatch, "/api/v1/issues/"+child, map[string]any{"blocked_by": []string{elsewhere}}, &relinked)
	if !slices.Equal(relinked.BlockedBy, []string{elsewhere}) {
		t.Fatalf("Dispatch answered the link of %s to %s with blocked_by %v", child, elsewhere, relinked.BlockedBy)
	}
	setDispatchStatus(t, baseURL, child, "todo")
	state, body = settled(t, d, "take "+child+" into a new run once a person set it back to todo", func(s api.State) bool {
		return s.Issues[child].Generation > ran
	})
	t.Logf("the state once %s, which ran planning, was set back to todo while it waits on %s:\n%s", child, elsewhere, body)
	if waits, ok := blockedOn(state, child); state.Issues[child].Phase != phase.Admitted || !ok || !slices.Equal(waits, []string{elsewhere}) {
		t.Fatalf("Legion took %s, which ran planning in generation %d, into generation %d at %s with admission.blocked = %v; "+
			"want its new run held at %s waiting on %s\nstate %s", child, ran, state.Issues[child].Generation,
			state.Issues[child].Phase, state.Admission.Blocked, phase.Admitted, elsewhere, body)
	}
	// Dispatch refuses an edit of a closed issue, so the links the closed child of step 7 and the root
	// of step 9 come back with are made while each still runs, which stops neither: a child already
	// started and a root already in its slot are never re-checked (step 3).
	for _, key := range []string{unlinked, free} {
		dispatchCall(t, baseURL, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"blocked_by": []string{elsewhere}}, &relinked)
		if !slices.Equal(relinked.BlockedBy, []string{elsewhere}) {
			t.Fatalf("Dispatch answered the link of %s to %s with blocked_by %v", key, elsewhere, relinked.BlockedBy)
		}
	}
	setDispatchStatus(t, baseURL, unlinked, "done")
	settled(t, d, "record "+unlinked+" done once a person closed it", func(s api.State) bool {
		return s.Issues[unlinked].Phase == phase.Done
	})

	setDispatchStatus(t, baseURL, free, "done")
	state, body = settled(t, d, "hand "+child+" over as a root of its own once "+free+" closed", func(s api.State) bool {
		return slices.Contains(s.Admission.Waiting, child) || slices.Contains(s.Admission.Active, child)
	})
	t.Logf("the state once %s closed and its tree finished, handing over %s, held in it on %s:\n%s", free, child, elsewhere, body)
	checked(child, "once its finished tree handed it over", state, body)

	setDispatchStatus(t, baseURL, unlinked, "todo")
	state, body = settled(t, d, "take "+unlinked+" into its waiting line or a slot", func(s api.State) bool {
		return slices.Contains(s.Admission.Waiting, unlinked) || slices.Contains(s.Admission.Active, unlinked)
	})
	t.Logf("the state once %s, closed in its finished tree, was set back to todo while it waits on %s:\n%s", unlinked, elsewhere, body)
	checked(unlinked, "once a person set it back to todo after its tree finished", state, body)

	// 9. A tree that ran, taken back after its linger, starts again whatever its links say. The root
	// of step 3 reached planning before its tree closed in step 8, linked to the issue that still
	// holds both roots of step 8; once the tree's close has run to its end, a person sets it back to
	// todo. It takes a slot at once and is named in admission.blocked at no point, while the same
	// state still names those two: the check exempts the tree Legion takes back, and nothing else.
	//
	// The tree's end is read off the daemon's own tree_lifecycles row, since the state shows none: a
	// root set back to todo while its tree's cleanup is still running waits for that cleanup, and is
	// promoted at the next fact or controller tick after it, of which this test has neither.
	var cleaned bool
	settled(t, d, "finish the cleanup of "+free+"'s tree", func(api.State) bool {
		if err := pool.QueryRow(t.Context(), `select cleanup_confirmed_at is not null from tree_lifecycles where tree = $1`, free).
			Scan(&cleaned); err != nil {
			t.Fatalf("read the lifecycle of %s's tree: %v", free, err)
		}
		return cleaned
	})
	var namedTakenBack []api.BlockedIssue
	setDispatchStatus(t, baseURL, free, "todo")
	state, body = settled(t, d, "take "+free+" back once a person set it to todo", func(s api.State) bool {
		_, named := blockedOn(s, free)
		if named && namedTakenBack == nil {
			namedTakenBack = s.Admission.Blocked
		}
		return slices.Contains(s.Admission.Active, free) || (slices.Contains(s.Admission.Waiting, free) && named)
	})
	t.Logf("the state once %s, a tree that ran, was taken back after its linger while it waits on %s:\n%s", free, elsewhere, body)
	if err := pool.QueryRow(t.Context(), `select blockers from dispatch_issues where key = $1`, free).Scan(&mirrored); err != nil {
		t.Fatalf("read the daemon's mirror of %s: %v", free, err)
	}
	if !slices.Equal(mirrored, []string{elsewhere}) {
		t.Fatalf("the daemon mirrors %s as waiting on %v; want %s, the link Dispatch made before the todo\nstate %s", free, mirrored, elsewhere, body)
	}
	if !slices.Contains(state.Admission.Active, free) || state.Issues[free].Slot == nil {
		t.Fatalf("Legion left %s out of a slot (active = %v, its slot %v) though it is a tree that ran, taken back after its linger, "+
			"which starts again whatever its links say: it checked the tree it takes back against %s\nstate %s",
			free, state.Admission.Active, state.Issues[free].Slot, elsewhere, body)
	}
	if namedTakenBack != nil {
		t.Fatalf("Legion named %s, a tree that ran and was taken back after its linger, in admission.blocked = %v\nstate %s",
			free, namedTakenBack, body)
	}
	for _, key := range []string{child, unlinked} {
		if waits, ok := blockedOn(state, key); !ok || !slices.Equal(waits, []string{elsewhere}) || slices.Contains(state.Admission.Active, key) {
			t.Fatalf("the state that admitted %s shows %s blocked by %v (named %t) and active = %v; want it still waiting on %s: only the "+
				"tree Legion takes back is exempt\nstate %s", free, key, waits, ok, state.Admission.Active, elsewhere, body)
		}
	}

	// 10. The issue both roots of step 8 wait on closes, a real Dispatch write, and admits them.
	setDispatchStatus(t, baseURL, elsewhere, "done")
	state, body = settled(t, d, "admit "+child+" and "+unlinked+" once "+elsewhere+" closed", func(s api.State) bool {
		return slices.Contains(s.Admission.Active, child) && slices.Contains(s.Admission.Active, unlinked)
	})
	t.Logf("%s closed through Dispatch, so the roots it held are admitted:\n%s", elsewhere, body)
	for _, key := range []string{child, unlinked} {
		if waits, ok := blockedOn(state, key); ok {
			t.Fatalf("%s is admitted and the state still lists it blocked by %v\nstate %s", key, waits, body)
		}
	}
}

// settled polls the daemon's state until want holds and returns it with the JSON it was read
// from, so every assertion over it quotes the very answer it judged. The daemon is event-driven:
// nothing here waits a fixed span, and a condition that never holds fails naming the state the
// last poll read — what Legion was doing instead.
func settled(t *testing.T, d *daemon, what string, want func(api.State) bool) (api.State, string) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	if testDeadline, ok := t.Deadline(); ok && testDeadline.Add(-20*time.Second).Before(deadline) {
		deadline = testDeadline.Add(-20 * time.Second)
	}
	for {
		status, body := d.request(http.MethodGet, "/legion/v1/state", nil, false)
		if status != http.StatusOK {
			t.Fatalf("GET the state = %d; body %s", status, body)
		}
		var state api.State
		if err := json.Unmarshal(body, &state); err != nil {
			t.Fatalf("decode the state %s: %v", body, err)
		}
		if want(state) {
			return state, string(body)
		}
		if time.Now().After(deadline) {
			t.Fatalf("Legion did not %s; its state is %s", what, body)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// blockedOn is what the state says issue waits on, and whether it names it at all.
func blockedOn(state api.State, issue string) ([]string, bool) {
	for _, row := range state.Admission.Blocked {
		if row.Issue == issue {
			return row.BlockedBy, true
		}
	}
	return nil, false
}

// childStatusNotices is the reason of every child-status notice the daemon has queued on issue for
// its tree's architect, oldest first, read off the daemon's own outbox, where a notice stands
// until the architect's role topic takes it. Nothing in this harness answers the Envoy address the
// notice runner publishes to, so no row is finished by a delivery while the test reads it.
func childStatusNotices(t *testing.T, pool *pgxpool.Pool, issue string) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `select payload->>'reason' from outbox
		where kind = 'notice' and issue = $1 and payload->>'kind' = 'child-status' order by id`, issue)
	if err != nil {
		t.Fatalf("read the child-status notices queued on %s: %v", issue, err)
	}
	reasons, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the child-status notices queued on %s: %v", issue, err)
	}
	return reasons
}

// architectGrants registers the session of an architect launch with the daemon and answers a
// fresh grant on every call, as the architect's plugin does: one grant authorises one command.
func architectGrants(t *testing.T, d *daemon, launch runtime.SpawnSpec, tree string) func() string {
	t.Helper()
	session, secret := registerLaunch(t, d, launch)
	return func() string {
		t.Helper()
		status, body := d.request(http.MethodPost, "/legion/v1/grants", api.GrantRequest{
			SessionID: session, Secret: secret, Tree: tree, Issue: tree,
		}, false)
		if status != http.StatusOK {
			t.Fatalf("the architect's grant on %s = %d; body %s", tree, status, body)
		}
		var minted api.GrantResponse
		if err := json.Unmarshal(body, &minted); err != nil {
			t.Fatalf("decode the architect's grant %s: %v", body, err)
		}
		return minted.GrantID
	}
}

// registerLaunch registers the session of a claim's launch with the daemon, as the agent's plugin
// does once the agent has booted, and answers the session and the secret the daemon gave it.
func registerLaunch(t *testing.T, d *daemon, launch runtime.SpawnSpec) (string, string) {
	t.Helper()
	session := "ses_" + string(launch.Claim)
	status, body := d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: launch.BootToken, SessionID: session, OmpSessionFile: "/sessions/" + session + ".jsonl",
		AgentID: "agent", PluginContract: api.DaemonAPIVersion,
	}, false)
	if status != http.StatusOK {
		t.Fatalf("register the session of %s = %d; body %s", launch.Claim, status, body)
	}
	var registered claim.RegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil {
		t.Fatalf("decode the registration of %s from %s: %v", launch.Claim, body, err)
	}
	return session, registered.Secret
}

// blockersDatabase is a Postgres database of this daemon's own, dropped when the test ends. The
// daemon migrates it itself at boot. It matters because of one table: processed_events, keyed by
// (source, event_id) alone. The scratch Dispatch numbers its own events from one in a database of
// its own, so every run publishes `dispatch-1` again, and a daemon sharing the test server's
// database with every other test would take this run's events for ones already processed and skip
// them. The admission slot index, unique across the whole database, is a second reason.
func blockersDatabase(t *testing.T) string {
	t.Helper()
	base, err := url.Parse(testDSN(t))
	if err != nil {
		t.Fatalf("parse the test DSN: %v", err)
	}
	adminURL := *base
	adminURL.Path = "/postgres"
	admin, err := pgxpool.New(context.Background(), adminURL.String())
	if err != nil {
		t.Fatalf("connect the test admin database: %v", err)
	}
	t.Cleanup(admin.Close)
	name := "legion_blockers_test_" + outboxSuffix(t)
	if _, err := admin.Exec(context.Background(), "create database "+name); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "drop database "+name+" with (force)"); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})
	daemonURL := *base
	daemonURL.Path = "/" + name
	return daemonURL.String()
}

// blockersGitHub is GitHub's REST API as this daemon's boot and its outbox call it: a repository's
// default branch, the ref of that branch, and the create of an issue branch, for any repository.
// The issue-branch row admission queues runs before the tree's architect starts, so a daemon whose
// GitHub answers nothing would never launch the architect whose grant this test needs. Any other
// request is a 404, logged rather than failed: nothing this test asserts on reads GitHub.
func blockersGitHub(t *testing.T) string {
	t.Helper()
	const head = "c0ffee0123456789abcdef0123456789abcdef01"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest, isRepository := strings.CutPrefix(r.URL.Path, "/repos/")
		parts := strings.SplitN(rest, "/", 3)
		if !isRepository || len(parts) < 2 {
			t.Logf("the GitHub stand-in answered 404 to %s %s", r.Method, r.URL.Path)
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		owner, name := parts[0], parts[1]
		tail := ""
		if len(parts) == 3 {
			tail = parts[2]
		}
		switch {
		case r.Method == http.MethodGet && tail == "":
			_, _ = io.WriteString(w, `{"full_name":"`+owner+"/"+name+`","default_branch":"main"}`)
		case r.Method == http.MethodGet && tail == "git/ref/heads/main":
			_, _ = io.WriteString(w, `{"ref":"refs/heads/main","object":{"sha":"`+head+`","type":"commit"}}`)
		case r.Method == http.MethodPost && tail == "git/refs":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"ref":"refs/heads/legion","object":{"sha":"`+head+`"}}`)
		default:
			t.Logf("the GitHub stand-in answered 404 to %s %s", r.Method, r.URL.Path)
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// scratchDispatchClient is the test's own client of the scratch Dispatch server; the daemon has
// its own.
var scratchDispatchClient = &http.Client{Timeout: 10 * time.Second}

// dispatchCall sends one request to the scratch Dispatch server as the person `smoke` — the
// trusted identity header the harness starts the server with — and decodes its answer into `into`
// when that is not nil.
func dispatchCall(t *testing.T, baseURL, method, path string, body, into any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode %s %s: %v", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, baseURL+path, reader)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "smoke")
	response, err := scratchDispatchClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the answer to %s %s: %v", method, path, err)
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		t.Fatalf("%s %s = %s: %s", method, path, response.Status, answer)
	}
	if into != nil {
		if err := json.Unmarshal(answer, into); err != nil {
			t.Fatalf("decode the answer to %s %s from %s: %v", method, path, answer, err)
		}
	}
}

// createBlockersIssue creates one issue of project through Dispatch's own API, carrying the legion
// label that hands it to Legion, optionally under parent, and waiting on waits. It is created in
// triage, where every Dispatch issue starts; the test moves it on with its own write.
func createBlockersIssue(t *testing.T, baseURL, project, title, parent string, waits []string) string {
	t.Helper()
	body := map[string]any{"project": project, "title": title, "labels": []string{"legion"}}
	if parent != "" {
		body["parent"] = parent
	}
	if len(waits) > 0 {
		body["blocked_by"] = waits
	}
	return createDispatchIssue(t, baseURL, title, body)
}

// createUnhandedIssue creates one issue of project through Dispatch's own API without the legion
// label: someone else's work, which nobody hands to Legion. It is created in triage.
func createUnhandedIssue(t *testing.T, baseURL, project, title string) string {
	t.Helper()
	return createDispatchIssue(t, baseURL, title, map[string]any{"project": project, "title": title})
}

// createDispatchIssue posts body as a new issue to Dispatch and answers the key Dispatch gave it.
func createDispatchIssue(t *testing.T, baseURL, title string, body map[string]any) string {
	t.Helper()
	var created struct {
		Key string `json:"key"`
	}
	dispatchCall(t, baseURL, http.MethodPost, "/api/v1/issues", body, &created)
	if created.Key == "" {
		t.Fatalf("Dispatch created %q without a key", title)
	}
	return created.Key
}

// setDispatchStatus moves an issue through Dispatch, as a person at the dashboard does. The event
// Dispatch makes of it is the daemon's only news of the move.
func setDispatchStatus(t *testing.T, baseURL, key, status string) {
	t.Helper()
	dispatchCall(t, baseURL, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"status": status}, nil)
}

// awaitDispatchStatus waits for Dispatch itself to show key at status. The test writes a status of
// its own only after the daemon's own write for that issue has landed, so the two never race for
// the last word on it.
func awaitDispatchStatus(t *testing.T, baseURL, key, status string) {
	t.Helper()
	var last string
	testwait.Eventually(t, "Dispatch to show "+key+" "+status, func() bool {
		var read struct {
			Status string `json:"status"`
		}
		dispatchCall(t, baseURL, http.MethodGet, "/api/v1/issues/"+key, nil, &read)
		last = read.Status
		return read.Status == status
	})
	if last != status {
		t.Fatalf("Dispatch shows %s %s, want %s", key, last, status)
	}
}

// dispatchPrimaryArtifact is the document Dispatch created with an issue, which is the artifact
// the architect registers as its tree's design.
func dispatchPrimaryArtifact(t *testing.T, baseURL, key string) string {
	t.Helper()
	var read struct {
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}
	dispatchCall(t, baseURL, http.MethodGet, "/api/v1/issues/"+key, nil, &read)
	if read.PrimaryArtifactID == "" {
		t.Fatalf("Dispatch shows no primary document for %s", key)
	}
	return read.PrimaryArtifactID
}
