package claudebridge

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// reportingRig is a rig whose pane names a daemon: every turn the bridge ends is reported to the
// stub, which answers as the daemon's route does.
type reportingRig struct {
	*rig
	reports chan claim.TurnOutcomeRequest
	answer  func(claim.TurnOutcomeRequest) claim.TurnOutcomeResponse
}

const testBootToken = "boot-token-of-this-launch"

func newReportingRig(t *testing.T, args ...string) *reportingRig {
	t.Helper()
	r := &reportingRig{
		reports: make(chan claim.TurnOutcomeRequest, 16),
		answer:  func(claim.TurnOutcomeRequest) claim.TurnOutcomeResponse { return claim.TurnOutcomeResponse{} },
	}
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/legion/v1/claims/turn-outcome" {
			t.Errorf("the bridge posted to %s, want the turn-outcome route", request.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read the bridge's report: %v", err)
			return
		}
		var report claim.TurnOutcomeRequest
		if err := json.Unmarshal(body, &report); err != nil {
			t.Errorf("decode the bridge's report %s: %v", body, err)
			return
		}
		r.reports <- report
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(r.answer(report))
	}))
	t.Cleanup(daemon.Close)
	tokenFile := filepath.Join(t.TempDir(), "boot-token")
	if err := os.WriteFile(tokenFile, []byte(testBootToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.rig = newRigWithEnv(t, []string{
		"LEGION_DAEMON_URL=" + daemon.URL,
		"LEGION_BOOT_TOKEN_FILE=" + tokenFile,
		"LEGION_GENERATION=4",
	}, args...)
	return r
}

// reported is the next turn the bridge reported.
func (r *reportingRig) reported() claim.TurnOutcomeRequest {
	r.t.Helper()
	select {
	case report := <-r.reports:
		return report
	case <-time.After(10 * time.Second):
		r.t.Fatal("the bridge reported no turn within 10s")
		return claim.TurnOutcomeRequest{}
	}
}

// untilTurnEnds reads the bridge's frames up to and including the turn's end.
func (r *rig) untilTurnEnds() {
	r.t.Helper()
	for r.next()["type"] != "agent_end" {
	}
}

// The bridge reports every turn it ends, clean and refused alike, with its own sequence counted
// from 1 within the launch generation and the launch's boot token as its name: the daemon's count
// of refused turns is made of these reports and of nothing else. A step Claude Code ended with an
// error is the turn's refusal, carrying what it said.
func TestTheBridgeReportsEveryTurnItEndsCleanAndRefusedAlike(t *testing.T) {
	r := newReportingRig(t, "-p", "--resume", sessionID)

	r.prompt("p1", "say hello")
	r.expect("agent_start", nil)
	r.untilTurnEnds()

	clean := r.reported()
	want := claim.TurnOutcomeRequest{
		BootToken: testBootToken, Generation: 4, Sequence: 1, Outcome: claim.TurnClean,
	}
	if clean != want {
		t.Fatalf("the clean turn was reported %+v, want %+v", clean, want)
	}

	r.prompt("p2", "fail the provider refused")
	r.expect("agent_start", nil)
	r.untilTurnEnds()

	refused := r.reported()
	if refused.Outcome != claim.TurnRefused || refused.Sequence != 2 || refused.Generation != 4 {
		t.Fatalf("the refused turn was reported %+v, want the second turn of generation 4 refused", refused)
	}
	if refused.Message == "" || refused.BootToken != testBootToken {
		t.Fatalf("the refused turn was reported %+v, want the provider's message and the launch's boot token", refused)
	}
}

// A turn the daemon's own abort ended is not the provider refusing it: the interrupt was asked
// for, so the turn is reported clean and nothing counts against the claim.
func TestATurnTheDaemonAbortedIsReportedClean(t *testing.T) {
	r := newReportingRig(t, "-p", "--resume", sessionID)

	r.prompt("p1", "hang")
	r.expect("agent_start", nil)
	r.send(shimwire.Abort{ID: "a1"})
	r.expect("response", map[string]any{"id": "a1", "command": "abort", "success": true})
	r.untilTurnEnds()

	if report := r.reported(); report.Outcome != claim.TurnClean || report.Message != "" {
		t.Fatalf("the aborted turn was reported %+v, want it clean with no message", report)
	}
}

// What the daemon counts is the provider refusing a turn, keyed on the result's subtype: a step
// Claude Code ended with its own cap on the steps of one turn (error_max_turns) is the worker
// having worked, not the provider refusing, and a turn whose newest step ran is the provider
// having answered after all. Counting either would walk a working claim to the launch failure
// limit, fail it, and abandon the session it is working in (dispatch://INFRA-714).
func TestOnlyTheStepsTheProviderRefusedAreReportedRefused(t *testing.T) {
	r := newReportingRig(t, "-p", "--resume", sessionID)

	r.prompt("p1", "maxturns worked to the cap")
	r.expect("agent_start", nil)
	r.untilTurnEnds()

	if report := r.reported(); report.Outcome != claim.TurnClean || report.Message != "" {
		t.Fatalf("the turn Claude Code's own turn cap ended was reported %+v, want it clean", report)
	}

	r.prompt("p2", "fail-then the CI is green")
	r.expect("agent_start", nil)
	r.untilTurnEnds()

	if report := r.reported(); report.Outcome != claim.TurnClean || report.Message != "" {
		t.Fatalf("the turn whose last step ran was reported %+v, want it clean: the provider answered after all", report)
	}
}

// A pane that names no daemon reports nothing and bridges as it always did: a bridge run by hand
// has no claim to report to.
func TestABridgeWithNoDaemonReportsNothingAndStillBridges(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)

	r.prompt("p1", "say hello")
	r.expect("agent_start", nil)
	r.untilTurnEnds()
	r.quiet()
}
