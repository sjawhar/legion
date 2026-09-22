package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
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

func TestSpawnRefusesARequestItCannotName(t *testing.T) {
	h := newHarness(t)
	for _, testCase := range []struct {
		name, wantMessage string
		mutate            func(*SpawnRequest)
	}{
		{"no tree", "tree is required", func(r *SpawnRequest) { r.Tree = "" }},
		{"no prompt", "prompt is required", func(r *SpawnRequest) { r.Prompt = "" }},
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
		"deliver refused: the claim is launching (a delivery is already pending)")
	recorder = h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(architectToken)+"/deliver", DeliverRequest{})
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "task is required") {
		t.Fatalf("deliver of no task = %d %s, want 400 naming task", recorder.Code, recorder.Body)
	}
}

// suspend, resume, and stop are the machine's requests: each answers the claim as the request
// left it, the machine's refusal when its state does not allow the request, and 404 for a claim
// the daemon does not hold.
func TestSuspendResumeAndStopDriveTheClaimsMachine(t *testing.T) {
	h := newHarness(t)
	h.operator(http.MethodPost, "/legion/v1/operator/claims", spawnBody())
	route := func(action string) string {
		return "/legion/v1/operator/claims/" + string(architectToken) + "/" + action
	}

	wantRefusal(t, h.operator(http.MethodPost, route("suspend"), nil), http.StatusConflict,
		"suspend refused: the claim is launching (the agent is not ready, so there is nothing to suspend yet)")

	registered := h.registered(h.bootToken(architectToken), "ses_architect")
	if recorder := h.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: architectToken, SessionID: "ses_architect", Secret: registered.Secret, Generation: 1,
	}, nil); recorder.Code != http.StatusNoContent {
		t.Fatalf("ready = %d; body %s", recorder.Code, recorder.Body)
	}

	steps := []struct {
		action string
		want   supervise.ClaimState
		method string
	}{
		{"suspend", supervise.StateSuspended, "Suspend"},
		{"resume", supervise.StateLaunching, "Resume"},
		{"stop", supervise.StateRetired, "Stop"},
	}
	for _, step := range steps {
		recorder := h.operator(http.MethodPost, route(step.action), nil)
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
	if stops := h.runtime.CallsOf("Stop"); stops[0].Grace != testGrace {
		t.Errorf("stop grace %s, want the machine's %s", stops[0].Grace, testGrace)
	}
	wantRefusal(t, h.operator(http.MethodPost, route("resume"), nil), http.StatusConflict,
		"resume refused: the claim is retired (the claim is retired)")

	for _, action := range []string{"deliver", "suspend", "resume", "stop"} {
		wantRefusal(t, h.operator(http.MethodPost, "/legion/v1/operator/claims/legion-legion-legion-1-tester/"+action,
			DeliverRequest{Task: "x"}), http.StatusNotFound, "no claim legion-legion-legion-1-tester")
	}
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

func TestListOfNoClaimsIsAnEmptyArray(t *testing.T) {
	h := newHarness(t)
	recorder := h.operator(http.MethodGet, "/legion/v1/operator/claims", nil)
	if got := strings.TrimSpace(recorder.Body.String()); recorder.Code != http.StatusOK || got != `{"claims":[]}` {
		t.Fatalf("list = %d %s, want 200 {\"claims\":[]}", recorder.Code, got)
	}
}
