package api

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

const architectToken = claim.Token("legion-legion-legion-208-architect")

func spawnBody() SpawnRequest {
	return SpawnRequest{Tree: "LEGION-208", Issue: "LEGION-208", Role: claim.RoleArchitect, Prompt: "Reply ready and wait."}
}

// Every operator route compares the bearer in constant time and answers 403 to anything else —
// no bearer, another scheme, the wrong token — before it reads a body or touches a claim.
func TestOperatorRoutesRefuseAnythingButTheOperatorBearer(t *testing.T) {
	h := newHarness(t)
	routes := []struct{ method, target string }{
		{http.MethodPost, "/legion/v1/operator/claims"},
		{http.MethodGet, "/legion/v1/operator/claims"},
		{http.MethodPost, "/legion/v1/operator/claims/" + string(architectToken) + "/deliver"},
		{http.MethodPost, "/legion/v1/operator/claims/" + string(architectToken) + "/suspend"},
		{http.MethodPost, "/legion/v1/operator/claims/" + string(architectToken) + "/resume"},
		{http.MethodPost, "/legion/v1/operator/claims/" + string(architectToken) + "/stop"},
		{http.MethodPost, "/legion/v1/operator/claims/" + string(architectToken) + "/close"},
	}
	for _, route := range routes {
		for name, header := range map[string]http.Header{
			"no bearer":       nil,
			"another scheme":  {"Authorization": {"Basic " + testOperatorToken}},
			"the wrong token": {"Authorization": {"Bearer not-the-operator"}},
			"a prefix of it":  {"Authorization": {"Bearer " + testOperatorToken[:5]}},
			"an empty bearer": {"Authorization": {"Bearer "}},
		} {
			t.Run(route.method+" "+route.target+"/"+name, func(t *testing.T) {
				wantRefusal(t, h.request(route.method, route.target, spawnBody(), header), http.StatusForbidden,
					"Invalid operator token")
			})
		}
	}
	if calls := h.runtime.Calls(); len(calls) != 0 {
		t.Fatalf("refused operator requests reached the runtime: %v", h.runtime.Methods())
	}
}

// A daemon that holds no operator token refuses every operator request, the empty bearer
// included: nothing compares equal to a token that is not there.
func TestOperatorRoutesAreClosedWithoutAnOperatorToken(t *testing.T) {
	h := newHarness(t)
	h.handler = NewServer("127.0.0.1", 8437, Options{Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject}).Handler

	for _, header := range []http.Header{nil, {"Authorization": {"Bearer "}}} {
		wantRefusal(t, h.request(http.MethodGet, "/legion/v1/operator/claims", nil, header), http.StatusForbidden,
			"Invalid operator token")
	}
}

// The spawn surface: a claim named by its issue and role, its role prompt kept by the daemon, its
// first task queued before the agent is ready, and its first launch made before the answer.
func TestSpawnCreatesTheClaimQueuesItsTaskAndLaunchesIt(t *testing.T) {
	h := newHarness(t)
	body := spawnBody()
	body.Task = "Say hello."

	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims", body)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("spawn = %d, want 201; body %s", recorder.Code, recorder.Body)
	}
	var got OperatorClaim
	decodeInto(t, recorder, &got)
	if got.Token != architectToken || got.Tree != "LEGION-208" || got.Issue != "LEGION-208" ||
		got.Role != claim.RoleArchitect || got.Generation != 1 || got.State != string(supervise.StateLaunching) {
		t.Fatalf("spawn answered %+v, want the architect claim launched at generation 1", got)
	}
	if got.Locator == nil || got.Locator.Claim != architectToken {
		t.Fatalf("spawn answered locator %+v, want the launch's", got.Locator)
	}
	if got.Pending == nil || got.Pending.Task != "Say hello." || got.Pending.DeliveredAt != nil {
		t.Fatalf("spawn answered pending %+v, want the task queued and not yet sent", got.Pending)
	}
	spawns := h.runtime.CallsOf("Spawn")
	if len(spawns) != 1 || spawns[0].Spec.Claim != architectToken || spawns[0].Spec.Project != testProject {
		t.Fatalf("spawns = %+v, want one launch of %s in project %s", spawns, architectToken, testProject)
	}
	if h.supervisor.prompts[architectToken] != body.Prompt {
		t.Errorf("the role prompt handed to the supervisor = %q, want %q", h.supervisor.prompts[architectToken], body.Prompt)
	}
	if stored := h.stored(architectToken); stored.State != supervise.StateLaunching || stored.Pending == nil {
		t.Errorf("stored %+v, want the launched claim with its task", stored)
	}
}

// Prompt is optional: its absence leaves the daemon to compose the shared role prompt and Go
// workflow addition for the selected role.
func TestSpawnWithoutPromptLeavesRoleCompositionToTheDaemon(t *testing.T) {
	h := newHarness(t)
	body := spawnBody()
	body.Prompt = ""

	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims", body)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("spawn = %d, want 201; body %s", recorder.Code, recorder.Body)
	}
	if got := h.supervisor.prompts[architectToken]; got != "" {
		t.Errorf("the role prompt override = %q, want none", got)
	}
}

func TestSpawnRefusesARequestItCannotName(t *testing.T) {
	h := newHarness(t)
	for _, testCase := range []struct {
		name, wantMessage string
		mutate            func(*SpawnRequest)
	}{
		{"no tree", "tree is required", func(r *SpawnRequest) { r.Tree = "" }},
		{"an issue that is not a key", `issue "legion-208" is not an issue key`, func(r *SpawnRequest) { r.Issue = "legion-208" }},
		{"a tree that is not a key", `tree "208" is not an issue key`, func(r *SpawnRequest) { r.Tree = "208" }},
		{"a role no agent holds", `role "controller" is not a role`, func(r *SpawnRequest) { r.Role = "controller" }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			body := spawnBody()
			testCase.mutate(&body)
			recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims", body)
			wantRefusal(t, recorder, http.StatusBadRequest, testCase.wantMessage)
		})
	}
	if calls := h.runtime.Calls(); len(calls) != 0 {
		t.Fatalf("a refused spawn reached the runtime: %v", h.runtime.Methods())
	}
}

// A spawn of a claim that exists is the machine's to judge: one already launched refuses, and
// the refusal launches nothing.
func TestSpawnOfALaunchedClaimIsRefused(t *testing.T) {
	h := newHarness(t)
	if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims", spawnBody()); recorder.Code != http.StatusCreated {
		t.Fatalf("first spawn = %d; body %s", recorder.Code, recorder.Body)
	}

	wantRefusal(t, h.operator(http.MethodPost, "/legion/v1/operator/claims", spawnBody()), http.StatusConflict,
		"spawn refused: the claim is launching (the claim is already launched)")
	if spawns := h.runtime.CallsOf("Spawn"); len(spawns) != 1 {
		t.Fatalf("spawns = %d, want the refused one to launch nothing", len(spawns))
	}
}

func TestDeliverQueuesATaskOnTheClaim(t *testing.T) {
	h := newHarness(t)
	h.operator(http.MethodPost, "/legion/v1/operator/claims", spawnBody())

	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(architectToken)+"/deliver",
		DeliverRequest{Task: "Review the plan."})

	if recorder.Code != http.StatusOK {
		t.Fatalf("deliver = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	var got OperatorClaim
	decodeInto(t, recorder, &got)
	if got.Pending == nil || got.Pending.Task != "Review the plan." {
		t.Fatalf("deliver answered pending %+v, want the task", got.Pending)
	}

	wantRefusal(t, h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(architectToken)+"/deliver",
		DeliverRequest{Task: "Another."}), http.StatusConflict,
		"deliver refused: a delivery is already pending")
	recorder = h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(architectToken)+"/deliver", DeliverRequest{})
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "task is required") {
		t.Fatalf("deliver of no task = %d %s, want 400 naming task", recorder.Code, recorder.Body)
	}
}

// suspend, resume, and stop are the machine's requests: each answers the claim as the request
// left it, the machine's refusal when its state does not allow the request, and 404 for a claim
// the daemon does not hold. The tree's root claim ends only when its tree closes, so the operator's
// stop of it is refused and names suspend; a worker's claim it stops.
func TestSuspendResumeAndStopDriveTheClaimsMachine(t *testing.T) {
	h := newHarness(t)
	h.operator(http.MethodPost, "/legion/v1/operator/claims", spawnBody())
	route := func(token claim.Token, action string) string {
		return "/legion/v1/operator/claims/" + string(token) + "/" + action
	}

	wantRefusal(t, h.operator(http.MethodPost, route(architectToken, "suspend"), nil), http.StatusConflict,
		"suspend refused: the claim is launching (the agent has not registered)")

	registered := h.registered(h.bootToken(architectToken), "ses_architect")
	if recorder := h.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: architectToken, SessionID: "ses_architect", Secret: registered.Secret, Generation: 1,
	}, nil); recorder.Code != http.StatusNoContent {
		t.Fatalf("ready = %d; body %s", recorder.Code, recorder.Body)
	}

	worker := spawnBody()
	worker.Issue, worker.Role = "LEGION-209", claim.RoleImplementer
	h.operator(http.MethodPost, "/legion/v1/operator/claims", worker)
	workerToken, err := claim.NewToken("legion", worker.Issue, worker.Role)
	if err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		token  claim.Token
		action string
		want   supervise.ClaimState
		method string
	}{
		{architectToken, "suspend", supervise.StateSuspended, "Suspend"},
		{architectToken, "resume", supervise.StateLaunching, "Resume"},
		{workerToken, "stop", supervise.StateRetired, "Release"},
	}
	for _, step := range steps {
		recorder := h.operator(http.MethodPost, route(step.token, step.action), nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s = %d, want 200; body %s", step.action, recorder.Code, recorder.Body)
		}
		var got OperatorClaim
		decodeInto(t, recorder, &got)
		if got.State != string(step.want) {
			t.Fatalf("%s answered state %s, want %s", step.action, got.State, step.want)
		}
		if len(h.runtime.CallsOf(step.method)) != 1 {
			t.Fatalf("after %s the runtime saw %v, want one %s", step.action, h.runtime.Methods(), step.method)
		}
	}
	if releases := h.runtime.CallsOf("Release"); releases[0].Released.Claim != workerToken {
		t.Errorf("released %s, want %s", releases[0].Released.Claim, workerToken)
	}
	wantRefusal(t, h.operator(http.MethodPost, route(architectToken, "stop"), nil), http.StatusConflict,
		"stop refused: the tree's root claim ends only when its tree closes; suspend it to stop its process once its agent has registered")
	if releases := h.runtime.CallsOf("Release"); len(releases) != 1 {
		t.Errorf("the refused stop of the root reached the runtime: %+v", releases)
	}
	wantRefusal(t, h.operator(http.MethodPost, route(workerToken, "resume"), nil), http.StatusConflict,
		"resume refused: the claim is retired (the claim is retired)")

	for _, action := range []string{"deliver", "suspend", "resume", "stop", "close"} {
		wantRefusal(t, h.operator(http.MethodPost, "/legion/v1/operator/claims/legion-legion-legion-1-tester/"+action,
			DeliverRequest{Task: "x"}), http.StatusNotFound, "no claim legion-legion-legion-1-tester")
	}
}

// A registered agent holds a secret that authenticates its requests, and no timer runs on it until
// it is ready; the operator's suspend is what stops the tree's root there, so it is accepted, and
// the suspension revokes the secret.
func TestTheOperatorSuspendsARegisteredRootAndRevokesItsSecret(t *testing.T) {
	h := newHarness(t)
	h.operator(http.MethodPost, "/legion/v1/operator/claims", spawnBody())
	registered := h.registered(h.bootToken(architectToken), "ses_architect")

	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(architectToken)+"/suspend", nil)

	if recorder.Code != http.StatusOK {
		t.Fatalf("suspend of the registered root = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	var got OperatorClaim
	decodeInto(t, recorder, &got)
	if got.State != string(supervise.StateSuspended) || got.Session != "ses_architect" {
		t.Fatalf("suspend answered %+v, want the root suspended with its session kept", got)
	}
	wantRefusal(t, h.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: architectToken, SessionID: "ses_architect", Secret: registered.Secret, Generation: 1,
	}, nil), http.StatusForbidden, claim.InvalidSecret.Message)
}

// A tree no workflow issue backs — one the operator spawned — has no linger to close it, so the
// operator closes it: close ends the tree's root claim, here its only claim, whatever its state, as
// the workflow's tree_close does. Its Sandbox and tree volume would otherwise outlive every use
// under a sandbox.
func TestTheOperatorClosesATreeNoWorkflowIssueBacks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reach func(h *harness)
	}{
		{"launching", func(*harness) {}},
		{"already retired", func(h *harness) {
			// The operator asked for an outcome that holds; a retry after a timeout is not a
			// failure, and nothing is released twice.
			if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(architectToken)+"/close", nil); recorder.Code != http.StatusOK {
				t.Fatalf("first close = %d; body %s", recorder.Code, recorder.Body)
			}
		}},
		{"suspended", func(h *harness) {
			h.registered(h.bootToken(architectToken), "ses_architect")
			if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(architectToken)+"/suspend", nil); recorder.Code != http.StatusOK {
				t.Fatalf("suspend = %d; body %s", recorder.Code, recorder.Body)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.operator(http.MethodPost, "/legion/v1/operator/claims", spawnBody())
			tc.reach(h)

			recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(architectToken)+"/close", nil)

			if recorder.Code != http.StatusOK {
				t.Fatalf("close = %d, want 200; body %s", recorder.Code, recorder.Body)
			}
			var got OperatorClaim
			decodeInto(t, recorder, &got)
			if got.State != string(supervise.StateRetired) {
				t.Fatalf("close answered state %s, want retired", got.State)
			}
			if releases := h.runtime.CallsOf("Release"); len(releases) != 1 || releases[0].Released.Claim != architectToken {
				t.Fatalf("releases = %+v, want the root released once", releases)
			}
		})
	}
}

// The operator's close is the whole tree's, as the workflow's tree_close is: the root claim first,
// whose close asks whether the tree may be closed, then every other claim of the tree, whatever its
// state. A worker left running would stay on a tree volume that is being deleted. Another tree's
// claims are left alone.
func TestTheOperatorsCloseStopsEveryClaimOfTheTree(t *testing.T) {
	h := newHarness(t)
	h.operator(http.MethodPost, "/legion/v1/operator/claims", spawnBody())
	for _, spawn := range []SpawnRequest{
		{Tree: "LEGION-208", Issue: "LEGION-209", Role: claim.RoleImplementer, Prompt: "Reply ready and wait."},
		{Tree: "LEGION-208", Issue: "LEGION-210", Role: claim.RoleTester, Prompt: "Reply ready and wait."},
		{Tree: "LEGION-300", Issue: "LEGION-300", Role: claim.RoleArchitect, Prompt: "Reply ready and wait."},
		{Tree: "LEGION-300", Issue: "LEGION-301", Role: claim.RoleImplementer, Prompt: "Reply ready and wait."},
	} {
		if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims", spawn); recorder.Code != http.StatusCreated {
			t.Fatalf("spawn %s %s = %d; body %s", spawn.Issue, spawn.Role, recorder.Code, recorder.Body)
		}
	}
	h.registered(h.bootToken("legion-legion-legion-210-tester"), "ses_tester")

	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(architectToken)+"/close", nil)

	if recorder.Code != http.StatusOK {
		t.Fatalf("close = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	var released []claim.Token
	for _, call := range h.runtime.CallsOf("Release") {
		released = append(released, call.Released.Claim)
	}
	if len(released) != 3 || released[0] != architectToken {
		t.Fatalf("released %v, want the root first and then the tree's two workers", released)
	}
	for _, token := range []claim.Token{architectToken, "legion-legion-legion-209-implementer", "legion-legion-legion-210-tester"} {
		if state := h.stored(token).State; state != supervise.StateRetired {
			t.Errorf("%s is %s after its tree closed, want retired", token, state)
		}
	}
	for _, token := range []claim.Token{"legion-legion-legion-300-architect", "legion-legion-legion-301-implementer"} {
		if state := h.stored(token).State; state == supervise.StateRetired {
			t.Errorf("%s, of another tree, was retired by this tree's close", token)
		}
	}
}

// A worker whose stop fails is named with its reason, the root stays closed, and the other workers
// are still stopped: the close does not end at the first failure. Asking again once the fault clears
// is the recovery. The retired root takes its "already retired" row, and the workers' stops go
// through.
func TestTheOperatorsCloseNamesTheWorkersItCouldNotStop(t *testing.T) {
	h := newHarness(t)
	h.operator(http.MethodPost, "/legion/v1/operator/claims", spawnBody())
	workers := []claim.Token{"legion-legion-legion-209-implementer", "legion-legion-legion-210-tester"}
	for _, spawn := range []SpawnRequest{
		{Tree: "LEGION-208", Issue: "LEGION-209", Role: claim.RoleImplementer, Prompt: "Reply ready and wait."},
		{Tree: "LEGION-208", Issue: "LEGION-210", Role: claim.RoleTester, Prompt: "Reply ready and wait."},
	} {
		if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims", spawn); recorder.Code != http.StatusCreated {
			t.Fatalf("spawn %s %s = %d; body %s", spawn.Issue, spawn.Role, recorder.Code, recorder.Body)
		}
	}
	for _, worker := range workers {
		h.runtime.FailReleaseOf(worker, errors.New("the API server timed out"))
	}

	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(architectToken)+"/close", nil)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("close = %d, want 500; body %s", recorder.Code, recorder.Body)
	}
	for _, want := range []string{"closed LEGION-208's root claim " + string(architectToken), string(workers[0]), string(workers[1]), "the API server timed out", "legion claims stop"} {
		if !strings.Contains(recorder.Body.String(), want) {
			t.Errorf("the refusal %s does not say %q", recorder.Body, want)
		}
	}
	if state := h.stored(architectToken).State; state != supervise.StateRetired {
		t.Errorf("the root is %s, want it closed", state)
	}
	for _, worker := range workers {
		if state := h.stored(worker).State; state == supervise.StateRetired {
			t.Errorf("%s retired although its stop failed", worker)
		}
		h.runtime.FailReleaseOf(worker, nil)
	}

	if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(architectToken)+"/close", nil); recorder.Code != http.StatusOK {
		t.Fatalf("the second close = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	for _, worker := range workers {
		if state := h.stored(worker).State; state != supervise.StateRetired {
			t.Errorf("%s is %s after the second close, want retired", worker, state)
		}
	}
}

// A claim of the tree the daemon supervises no machine for cannot be stopped, so the close does not
// answer 200 over it: it is named with that reason, and with the restart that makes it stoppable,
// since the stop route answers 404 for it, beside the closed root.
func TestTheOperatorsCloseNamesAClaimItSupervisesNoMachineFor(t *testing.T) {
	h := newHarness(t)
	h.operator(http.MethodPost, "/legion/v1/operator/claims", spawnBody())
	worker := spawnBody()
	worker.Issue, worker.Role = "LEGION-209", claim.RoleImplementer
	h.operator(http.MethodPost, "/legion/v1/operator/claims", worker)
	h.supervisor.mu.Lock()
	delete(h.supervisor.machines, "legion-legion-legion-209-implementer")
	h.supervisor.mu.Unlock()

	recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(architectToken)+"/close", nil)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("close = %d, want 500; body %s", recorder.Code, recorder.Body)
	}
	for _, want := range []string{"legion-legion-legion-209-implementer (the daemon supervises no machine for it", "until the daemon restarts"} {
		if !strings.Contains(recorder.Body.String(), want) {
			t.Errorf("the refusal %s does not say %q", recorder.Body, want)
		}
	}
	if state := h.stored(architectToken).State; state != supervise.StateRetired {
		t.Errorf("the root is %s, want it closed", state)
	}
}

// The operator closes only a tree no workflow issue backs, and only through its root claim: a
// workflow issue's tree closes when its linger expires, and a worker's claim is stopped. Each
// refusal changes nothing.
func TestTheOperatorsCloseRefusesAWorkflowTreeAndAWorker(t *testing.T) {
	h := newHarness(t)
	h.operator(http.MethodPost, "/legion/v1/operator/claims", spawnBody())
	worker := spawnBody()
	worker.Issue, worker.Role = "LEGION-209", claim.RoleImplementer
	h.operator(http.MethodPost, "/legion/v1/operator/claims", worker)

	wantRefusal(t, h.operator(http.MethodPost, "/legion/v1/operator/claims/legion-legion-legion-209-implementer/close", nil),
		http.StatusConflict, "close refused: legion-legion-legion-209-implementer is not its tree's root claim; stop it instead")

	h.recordIssue(record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Planning, Generation: 1, Status: "in_progress"})
	wantRefusal(t, h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(architectToken)+"/close", nil),
		http.StatusConflict, "close refused: LEGION-208 is a workflow issue's tree, which closes when its linger expires")

	if releases := h.runtime.CallsOf("Release"); len(releases) != 0 {
		t.Errorf("a refused close reached the runtime: %+v", releases)
	}
	for _, token := range []claim.Token{architectToken, "legion-legion-legion-209-implementer"} {
		if state := h.stored(token).State; state != supervise.StateLaunching {
			t.Errorf("%s is %s after a refused close, want it left launching", token, state)
		}
	}
}

// A close names a claim, and only a tree's root claim closes its tree. A worker's claim that has
// already retired is still not one: answering it as the close that succeeded ran the tree's
// fan-out from a claim that never held the tree, stopping the tree's live workers and leaving the
// root alone. The refusal a live worker gets is the one a retired worker gets.
func TestAClosedWorkersRetiredClaimStillRefusesAndStopsNothing(t *testing.T) {
	h := newHarness(t)
	h.operator(http.MethodPost, "/legion/v1/operator/claims", spawnBody())
	worker := spawnBody()
	worker.Issue, worker.Role = "LEGION-209", claim.RoleImplementer
	h.operator(http.MethodPost, "/legion/v1/operator/claims", worker)
	tester := spawnBody()
	tester.Issue, tester.Role = "LEGION-210", claim.RoleTester
	h.operator(http.MethodPost, "/legion/v1/operator/claims", tester)
	const implementer = claim.Token("legion-legion-legion-209-implementer")
	if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(implementer)+"/stop", nil); recorder.Code != http.StatusOK {
		t.Fatalf("stop = %d; body %s", recorder.Code, recorder.Body)
	}
	if state := h.stored(implementer).State; state != supervise.StateRetired {
		t.Fatalf("the stopped implementer is %s, want retired", state)
	}

	wantRefusal(t, h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(implementer)+"/close", nil),
		http.StatusConflict, "close refused: legion-legion-legion-209-implementer is not its tree's root claim; stop it instead")

	if state := h.stored("legion-legion-legion-210-tester").State; state == supervise.StateRetired {
		t.Error("the tree's tester was retired by a close of a retired worker's claim")
	}
	if state := h.stored(architectToken).State; state != supervise.StateLaunching {
		t.Errorf("the root is %s after the refused close, want it left launching", state)
	}
}

// A workflow issue's tree closes when its linger expires, whatever state its root claim is in: a
// root that has already retired is not a way past TreeClosable.
func TestACloseOfARetiredRootStillAsksWhetherAWorkflowIssueBacksTheTree(t *testing.T) {
	h := newHarness(t)
	h.operator(http.MethodPost, "/legion/v1/operator/claims", spawnBody())
	if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(architectToken)+"/close", nil); recorder.Code != http.StatusOK {
		t.Fatalf("first close = %d; body %s", recorder.Code, recorder.Body)
	}
	h.recordIssue(record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "U"})

	wantRefusal(t, h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(architectToken)+"/close", nil),
		http.StatusConflict, "close refused: LEGION-208 is a workflow issue's tree, which closes when its linger expires")
}

func TestListAnswersEveryClaim(t *testing.T) {
	h := newHarness(t)
	h.operator(http.MethodPost, "/legion/v1/operator/claims", spawnBody())
	worker := spawnBody()
	worker.Issue, worker.Role = "LEGION-209", claim.RoleImplementer
	h.operator(http.MethodPost, "/legion/v1/operator/claims", worker)

	recorder := h.operator(http.MethodGet, "/legion/v1/operator/claims", nil)

	if recorder.Code != http.StatusOK {
		t.Fatalf("list = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	var got OperatorClaims
	decodeInto(t, recorder, &got)
	want := []string{string(architectToken), "legion-legion-legion-209-implementer"}
	if tokens := sortedTokens(got.Claims); !slices.Equal(tokens, want) {
		t.Fatalf("list answered %v, want %v", tokens, want)
	}
}

// The list shows what supervision has counted against a claim and how its task will be sent: the
// deaths its agent has had with work outstanding, and that a death interrupted its task, which is
// sent behind the sentence saying so.
func TestListShowsAClaimsDeathsAndItsInterruptedTask(t *testing.T) {
	h := newHarness(t)
	token := claim.Token("legion-legion-legion-209-implementer")
	if err := h.store.PutClaim(h.ctx, supervise.Claim{Token: token, Project: testProject, Tree: "LEGION-208", Issue: "LEGION-209",
		Role: claim.RoleImplementer, State: supervise.StateLaunching, Budgets: supervise.Budgets{Deaths: 2}}); err != nil {
		t.Fatalf("put the claim: %v", err)
	}
	if err := h.store.PutDelivery(h.ctx, token, supervise.Delivery{ID: "delivery-1", Task: "implement the plan",
		QueuedAt: time.Date(2026, 9, 26, 7, 0, 0, 0, time.UTC), Interrupted: true}); err != nil {
		t.Fatalf("put the delivery: %v", err)
	}

	recorder := h.operator(http.MethodGet, "/legion/v1/operator/claims", nil)

	var got OperatorClaims
	decodeInto(t, recorder, &got)
	if len(got.Claims) != 1 || got.Claims[0].Budgets.Deaths != 2 || got.Claims[0].Pending == nil || !got.Claims[0].Pending.Interrupted {
		t.Fatalf("list answered %+v, want the claim with 2 deaths and its task interrupted", got.Claims)
	}
}

func TestListOfNoClaimsIsAnEmptyArray(t *testing.T) {
	h := newHarness(t)
	recorder := h.operator(http.MethodGet, "/legion/v1/operator/claims", nil)
	if got := strings.TrimSpace(recorder.Body.String()); recorder.Code != http.StatusOK || got != `{"claims":[]}` {
		t.Fatalf("list = %d %s, want 200 {\"claims\":[]}", recorder.Code, got)
	}
}
