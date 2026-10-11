package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// Once the daemon's stop has begun, every operator request that would change a claim — spawn,
// deliver, suspend, resume, stop, close — is refused with 503 before it decides anything: one
// decided then would run without its claim's worker stream connection, which the stop closes, and
// under tmux its stop would end the agent without its shutdown frame. Nothing reaches the runtime,
// nothing stays held, and the operator's listing, which changes nothing, still answers
// (LEGION-650).
func TestTheOperatorsRequestsThatWouldChangeAClaimAreRefusedOnceTheStopHasBegun(t *testing.T) {
	h := newHarness(t)
	h.operator(http.MethodPost, "/legion/v1/operator/claims", spawnBody())
	worker := SpawnRequest{Tree: "LEGION-208", Issue: "LEGION-209", Role: claim.RoleImplementer, Prompt: "Reply ready and wait."}
	if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims", worker); recorder.Code != http.StatusCreated {
		t.Fatalf("spawn the worker = %d; body %s", recorder.Code, recorder.Body)
	}
	stopping, beginStop := context.WithCancel(context.Background())
	beginStop()
	decisions := NewRouteDecisions()
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken, Controller: h.store,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Pool: h.store.Pool(), Record: record.NewStore(), Releaser: h.runtime,
		Trees: h.store, Stopping: stopping, Decisions: decisions,
	}).Handler
	calls := len(h.runtime.Calls())
	before := map[claim.Token]string{}
	for _, token := range []claim.Token{architectToken, "legion-legion-legion-209-implementer"} {
		before[token] = string(h.stored(token).State)
	}

	const claims = "/legion/v1/operator/claims/"
	for _, request := range []struct {
		name, target string
		body         any
	}{
		{"spawn", "/legion/v1/operator/claims", SpawnRequest{Tree: "LEGION-300", Issue: "LEGION-300", Role: claim.RoleArchitect, Prompt: "Reply ready and wait."}},
		{"deliver", claims + "legion-legion-legion-209-implementer/deliver", DeliverRequest{Task: "the task"}},
		{"suspend", claims + "legion-legion-legion-209-implementer/suspend", nil},
		{"resume", claims + "legion-legion-legion-209-implementer/resume", nil},
		{"stop", claims + "legion-legion-legion-209-implementer/stop", nil},
		{"close", claims + string(architectToken) + "/close", nil},
	} {
		recorder := h.operator(http.MethodPost, request.target, request.body)
		if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "the daemon is stopping") {
			t.Errorf("%s once the stop began = %d %s, want 503 saying the daemon is stopping", request.name, recorder.Code, recorder.Body)
		}
	}

	if got := h.runtime.Calls()[calls:]; len(got) != 0 {
		t.Errorf("the runtime was asked %+v once the stop began, want nothing", got)
	}
	for token, state := range before {
		if got := string(h.stored(token).State); got != state {
			t.Errorf("%s is stored %s, was %s before the refused requests", token, got, state)
		}
		if decisions.Holds(token) {
			t.Errorf("%s is still held once its refused requests returned", token)
		}
	}
	if _, ok := h.supervisor.Machine("legion-legion-legion-300-architect"); ok {
		t.Error("the refused spawn created its claim")
	}
	if recorder := h.operator(http.MethodGet, "/legion/v1/operator/claims", nil); recorder.Code != http.StatusOK {
		t.Errorf("the listing once the stop began = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
}

// A claim stays held while any route decides it: a suspension and a close can decide one claim at
// once, and the first to return must not end the other's hold (LEGION-650).
func TestAClaimStaysHeldWhileAnyRouteDecidesIt(t *testing.T) {
	d := NewRouteDecisions()
	const a, b claim.Token = "legion-legion-legion-208-architect", "legion-legion-legion-209-implementer"
	first := d.begin([]claim.Token{a})
	second := d.begin([]claim.Token{a, b})
	first()
	if !d.Holds(a) || !d.Holds(b) {
		t.Fatalf("after one of two routes deciding %s returned, held %s: %t, %s: %t; want both", a, a, d.Holds(a), b, d.Holds(b))
	}
	second()
	if d.Holds(a) || d.Holds(b) {
		t.Fatalf("after every route returned, held %s: %t, %s: %t; want neither", a, d.Holds(a), b, d.Holds(b))
	}
}

// heldAtRelease is the fake runtime, recording at each release which of the watched claims the
// API's route set holds then.
type heldAtRelease struct {
	*fake.Runtime
	decisions *RouteDecisions
	watched   []claim.Token
	mu        sync.Mutex
	held      map[claim.Token][]claim.Token
}

func (r *heldAtRelease) Release(ctx context.Context, k runtime.Known) error {
	var held []claim.Token
	for _, token := range r.watched {
		if r.decisions.Holds(token) {
			held = append(held, token)
		}
	}
	r.mu.Lock()
	r.held[k.Claim] = held
	r.mu.Unlock()
	return r.Runtime.Release(ctx, k)
}

// The operator's close holds every claim of the tree as its own from before the root's close, which
// waits out the root's exit, to the end of the route: a daemon's stop that begins while the root
// exits keeps those claims' worker stream connections, and each one's stop sends its shutdown frame
// over its own. Another tree's claims are not held, and nothing is once the route returns
// (LEGION-650).
func TestTheOperatorsCloseHoldsTheTreesClaimsFromBeforeTheRootsClose(t *testing.T) {
	h := newHarness(t)
	workers := []claim.Token{"legion-legion-legion-209-implementer", "legion-legion-legion-210-tester"}
	elsewhere := claim.Token("legion-legion-legion-301-implementer")
	decisions := NewRouteDecisions()
	rt := &heldAtRelease{Runtime: h.runtime, decisions: decisions, watched: append([]claim.Token{architectToken, elsewhere}, workers...),
		held: map[claim.Token][]claim.Token{}}
	h.supervisor.deps.Runtime = rt
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken, Controller: h.store,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Pool: h.store.Pool(), Record: record.NewStore(), Releaser: h.runtime,
		Trees: h.store, Decisions: decisions,
	}).Handler
	for _, spawn := range []SpawnRequest{
		spawnBody(),
		{Tree: "LEGION-208", Issue: "LEGION-209", Role: claim.RoleImplementer, Prompt: "Reply ready and wait."},
		{Tree: "LEGION-208", Issue: "LEGION-210", Role: claim.RoleTester, Prompt: "Reply ready and wait."},
		{Tree: "LEGION-300", Issue: "LEGION-300", Role: claim.RoleArchitect, Prompt: "Reply ready and wait."},
		{Tree: "LEGION-300", Issue: "LEGION-301", Role: claim.RoleImplementer, Prompt: "Reply ready and wait."},
	} {
		if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims", spawn); recorder.Code != http.StatusCreated {
			t.Fatalf("spawn %s %s = %d; body %s", spawn.Issue, spawn.Role, recorder.Code, recorder.Body)
		}
	}

	if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(architectToken)+"/close", nil); recorder.Code != http.StatusOK {
		t.Fatalf("close = %d, want 200; body %s", recorder.Code, recorder.Body)
	}

	want := append([]claim.Token{architectToken}, workers...)
	slices.Sort(want)
	for _, released := range want {
		held, ok := rt.held[released]
		if !ok {
			t.Errorf("%s was never released", released)
			continue
		}
		slices.Sort(held)
		if !slices.Equal(held, want) {
			t.Errorf("while %s was released the route held %v, want the tree's claims %v and nothing else", released, held, want)
		}
	}
	for _, token := range append(want, elsewhere) {
		if decisions.Holds(token) {
			t.Errorf("%s is still held once the close returned", token)
		}
	}
}
