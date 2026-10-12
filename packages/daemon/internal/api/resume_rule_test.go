package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// suspendedBy is who the store records suspended token's claim: the column the daemon's resume by
// rule reads after a restart.
func (h *harness) suspendedBy(token claim.Token) string {
	h.t.Helper()
	var by string
	if err := h.store.Pool().QueryRow(h.ctx, "select suspended_by from claims where token = $1", string(token)).Scan(&by); err != nil {
		h.t.Fatalf("read who suspended %s: %v", token, err)
	}
	return by
}

// Who suspended a claim is stored with it: the operator's suspend route records the operator, and a
// tree root's own exit, which suspends the root, records the exit. The daemon resumes a suspended
// claim by rule unless an operator suspended it, so the record is what that rule reads, after a
// restart as before one. A resume clears it: a claim that runs again is suspended by nobody.
func TestTheStoreRecordsWhoSuspendedAClaim(t *testing.T) {
	h := newHarness(t)
	suspended, boot := h.launch("LEGION-208", claim.RoleArchitect)
	h.registered(boot, "ses_architect")
	if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(suspended)+"/suspend", nil); recorder.Code != http.StatusOK {
		t.Fatalf("suspend = %d; body %s", recorder.Code, recorder.Body)
	}
	if got := h.suspendedBy(suspended); got != "operator" {
		t.Fatalf("the operator's suspend recorded %q, want operator", got)
	}

	exited, boot := h.launch("LEGION-300", claim.RoleArchitect)
	registered := h.registered(boot, "ses_exited")
	if recorder := h.request(http.MethodPost, "/legion/v1/claims/exit", claim.ExitRequest{
		ClaimToken: exited, SessionID: "ses_exited", Secret: registered.Secret, Generation: 1, Reason: "the tree waits on its workers",
	}, nil); recorder.Code != http.StatusNoContent {
		t.Fatalf("exit = %d; body %s", recorder.Code, recorder.Body)
	}
	if got := h.suspendedBy(exited); got != "exit" {
		t.Fatalf("the root's own exit recorded %q, want exit", got)
	}

	if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(suspended)+"/resume", nil); recorder.Code != http.StatusOK {
		t.Fatalf("resume = %d; body %s", recorder.Code, recorder.Body)
	}
	if got := h.suspendedBy(suspended); got != "" {
		t.Fatalf("the resumed claim records %q, want nobody", got)
	}
}

// Each operator request that changes a claim and succeeds says so in one Info line naming its
// route, the claim and the caller, so a day's operator requests can be counted from the log alone.
// A request the claim refuses logs no such line.
func TestEachSuccessfulOperatorClaimRequestLogsOneInfoLine(t *testing.T) {
	h := newHarness(t)
	var logged bytes.Buffer
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken, Controller: h.store,
		Log: slog.New(slog.NewTextHandler(&logged, nil)), Pool: h.store.Pool(), Record: record.NewStore(), Releaser: h.runtime, Trees: h.store,
	}).Handler
	route := func(token claim.Token, action string) string {
		return "/legion/v1/operator/claims/" + string(token) + "/" + action
	}
	worker, err := claim.NewToken(testProject, "LEGION-209", claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	type step struct {
		method, target string
		body           any
		pattern        string
		claim          claim.Token
	}
	run := func(s step, want int) {
		t.Helper()
		if recorder := h.operator(s.method, s.target, s.body); recorder.Code != want {
			t.Fatalf("%s %s = %d, want %d; body %s", s.method, s.target, recorder.Code, want, recorder.Body)
		}
	}
	// operatorLines is every Info line an operator request logged, in order.
	operatorLines := func() []string {
		var lines []string
		for _, line := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
			if strings.Contains(line, "level=INFO") && strings.Contains(line, `msg="api: operator request"`) {
				lines = append(lines, line)
			}
		}
		return lines
	}

	run(step{method: http.MethodPost, target: route(architectToken, "suspend")}, http.StatusNotFound)
	run(step{method: http.MethodPost, target: "/legion/v1/operator/claims", body: spawnBody()}, http.StatusCreated)
	run(step{method: http.MethodPost, target: route(architectToken, "resume")}, http.StatusConflict)
	h.registered(h.bootToken(architectToken), "ses_architect")
	workerSpawn := spawnBody()
	workerSpawn.Issue, workerSpawn.Role = "LEGION-209", claim.RoleImplementer
	succeeded := []step{
		{method: http.MethodPost, target: route(architectToken, "suspend"), pattern: "POST /legion/v1/operator/claims/{token}/suspend", claim: architectToken},
		{method: http.MethodPost, target: route(architectToken, "resume"), pattern: "POST /legion/v1/operator/claims/{token}/resume", claim: architectToken},
		{method: http.MethodPost, target: route(architectToken, "deliver"), body: DeliverRequest{Task: "look at the tree"}, pattern: "POST /legion/v1/operator/claims/{token}/deliver", claim: architectToken},
		{method: http.MethodPost, target: "/legion/v1/operator/claims", body: workerSpawn, pattern: "POST /legion/v1/operator/claims", claim: worker},
		{method: http.MethodPost, target: route(worker, "stop"), pattern: "POST /legion/v1/operator/claims/{token}/stop", claim: worker},
		{method: http.MethodPost, target: route(architectToken, "close"), pattern: "POST /legion/v1/operator/claims/{token}/close", claim: architectToken},
	}
	for _, s := range succeeded {
		want := http.StatusOK
		if s.target == "/legion/v1/operator/claims" {
			want = http.StatusCreated
		}
		run(s, want)
	}
	run(step{method: http.MethodGet, target: "/legion/v1/operator/claims"}, http.StatusOK)

	lines := operatorLines()
	// The first spawn succeeded too, before the refused suspend.
	if len(lines) != len(succeeded)+1 {
		t.Fatalf("logged %d operator request lines, want one per successful request (%d):\n%s", len(lines), len(succeeded)+1, logged.String())
	}
	for i, s := range append([]step{{pattern: "POST /legion/v1/operator/claims", claim: architectToken}}, succeeded...) {
		for _, want := range []string{`route="` + s.pattern + `"`, "claim=" + string(s.claim), "caller=192.0.2.1:1234"} {
			if !strings.Contains(lines[i], want) {
				t.Errorf("operator request line %d is %q, want it to carry %s", i, lines[i], want)
			}
		}
	}
}
