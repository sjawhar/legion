package claudebridge

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// bootToken is the launch's own token the rig's boot token file holds, which every report below
// must carry: the bridge holds no registration secret, so this is the whole of how the daemon
// knows whose turn ended (claim.BootCaller).
const bootToken = "boot-token-of-the-launch"

// generation is the launch generation the rig's panes run, which every report counts its sequence
// within.
const generation = 7

// daemonStandin is a stand-in for POST /legion/v1/claims/turn-outcome: it records each report
// and answers 204, or the status a case set.
type daemonStandin struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	reports  []claim.TurnOutcomeRequest
	refusing int
}

// newReportingRig is a bridge whose reports reach a stand-in daemon of the test's own.
func newReportingRig(t *testing.T, args ...string) (*rig, *daemonStandin) {
	t.Helper()
	d := &daemonStandin{t: t}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+turnOutcomePath, d.take)
	d.server = httptest.NewServer(mux)
	t.Cleanup(d.server.Close)
	tokenFile := filepath.Join(t.TempDir(), "boot-token")
	if err := os.WriteFile(tokenFile, []byte(bootToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{
		"LEGION_DAEMON_URL=" + d.server.URL,
		"LEGION_BOOT_TOKEN_FILE=" + tokenFile,
		fmt.Sprintf("LEGION_GENERATION=%d", generation),
	}
	return newRigEnv(t, env, args...), d
}

func (d *daemonStandin) take(w http.ResponseWriter, r *http.Request) {
	var report claim.TurnOutcomeRequest
	if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
		d.t.Errorf("the bridge posted a body that is not a turn outcome: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	d.reports = append(d.reports, report)
	status := d.refusing
	d.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"Invalid boot token"}`))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// refuse makes the stand-in answer every later report with status.
func (d *daemonStandin) refuse(status int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.refusing = status
}

// reported is the reports the stand-in has taken, in the order they arrived.
func (d *daemonStandin) reported() []claim.TurnOutcomeRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]claim.TurnOutcomeRequest(nil), d.reports...)
}

// awaitReports waits for the stand-in to have taken n reports.
func (d *daemonStandin) awaitReports(r *rig, n int) []claim.TurnOutcomeRequest {
	r.t.Helper()
	r.waitFor(fmt.Sprintf("%d turn outcome reports", n), func() bool { return len(d.reported()) >= n })
	return d.reported()
}

// The class each turn is reported under, driven through the bridge by the measured stream-json of
// the pinned Claude Code (2.1.310): a refusal's own lines, a 529's and a 429's, as that binary
// wrote them (replay's comment names the measurement). A content filter's refusal is `refused`,
// because the same session often takes that turn on a weaker model; every other failure the
// provider marked final is `provider-error`, which may not clear that way; and Claude Code's own
// `error_max_turns`, where the provider took every request, is `clean` and never
// `provider-error`.
func TestEachTurnIsReportedUnderItsClass(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
		want   claim.TurnOutcome
	}{
		{"a content filter's refusal", "replay refusal-turn.jsonl", claim.TurnRefused},
		{"a 529 the provider marked final", "replay api-error-529.jsonl", claim.TurnProviderError},
		{"a 429 the provider marked final", "replay api-error-429.jsonl", claim.TurnProviderError},
		{"a turn that ended well", "say done", claim.TurnClean},
		{"Claude Code's own turn limit", "max-turns", claim.TurnClean},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, d := newReportingRig(t, "-p", "--resume", sessionID)
			r.prompt("p1", tc.script)
			if strings.HasPrefix(tc.script, "replay ") {
				// The recording's last line is its result, which the gate holds back.
				r.open()
			}
			reports := d.awaitReports(r, 1)
			want := claim.TurnOutcomeRequest{
				BootCaller: claim.BootCaller{BootToken: bootToken},
				Generation: generation,
				Sequence:   1,
				Outcome:    tc.want,
			}
			if reports[0] != want {
				t.Errorf("the bridge reported %+v, want %+v", reports[0], want)
			}
			// One report per turn, and no second one behind the turn's end.
			r.waitFor("the turn's end", func() bool { return strings.Contains(r.log.String(), "reported turn 1") })
			if got := d.reported(); len(got) != 1 {
				t.Errorf("the bridge sent %d reports for one turn: %+v", len(got), got)
			}
		})
	}
}

// The sequence is the bridge's own count of the turns it has ended at this generation, from 1:
// the daemon keeps the newest report under (generation, sequence), so two turns may not take the
// same number, and the number rises in the order the turns ended.
func TestTheSequenceRisesWithEachTurnOfTheGeneration(t *testing.T) {
	r, d := newReportingRig(t, "-p", "--resume", sessionID)
	for turn := 1; turn <= 3; turn++ {
		r.prompt(fmt.Sprintf("p%d", turn), "say done")
		r.expect("agent_start", nil)
		r.expect("agent_end", nil)
		r.expect("prompt_result", map[string]any{"id": fmt.Sprintf("p%d", turn)})
	}
	reports := d.awaitReports(r, 3)
	for turn, report := range reports {
		want := claim.TurnOutcomeRequest{
			BootCaller: claim.BootCaller{BootToken: bootToken},
			Generation: generation,
			Sequence:   uint64(turn + 1),
			Outcome:    claim.TurnClean,
		}
		if report != want {
			t.Errorf("report %d is %+v, want %+v", turn+1, report, want)
		}
	}
}

// A report the daemon refuses is logged and ends nothing: the turn is over, nothing the bridge
// can do fixes a refusal, and the frames that end the turn reach the shim either way.
func TestADaemonsRefusalOfAReportIsLoggedAndEndsNothing(t *testing.T) {
	r, d := newReportingRig(t, "-p", "--resume", sessionID)
	d.refuse(http.StatusForbidden)
	r.prompt("p1", "say done")
	r.expect("agent_start", nil)
	r.expect("agent_end", nil)
	r.expect("prompt_result", map[string]any{"id": "p1", "agentInvoked": true})
	r.logged("the daemon refused the clean outcome of turn 1 with 403: {\"error\":\"Invalid boot token\"}")
	// The next turn is reported as any other: the refusal held back no turn and spent no number
	// twice.
	d.refuse(0)
	r.prompt("p2", "say done")
	r.expect("agent_start", nil)
	r.expect("agent_end", nil)
	r.expect("prompt_result", map[string]any{"id": "p2"})
	reports := d.awaitReports(r, 2)
	if reports[1].Sequence != 2 || reports[1].Outcome != claim.TurnClean {
		t.Errorf("the turn after a refused report is %+v, want sequence 2 reported clean", reports[1])
	}
}

// A bridge the environment told nothing to report with — one an operator runs by hand, and the
// rig of every other case here — reports nothing and says so once, naming each variable it was
// not given.
func TestABridgeToldNoDaemonReportsNothing(t *testing.T) {
	r := newRig(t, "-p", "--resume", sessionID)
	r.logged("reporting no turn outcome: the environment names no LEGION_DAEMON_URL and no LEGION_BOOT_TOKEN_FILE and no LEGION_GENERATION")
	r.prompt("p1", "say done")
	r.expect("agent_start", nil)
	r.expect("agent_end", nil)
	r.expect("prompt_result", map[string]any{"id": "p1"})
}

// classify reads the two members the measured result lines carry, and the subtype for the one
// end Claude Code makes itself.
func TestClassifyReadsTheResultsOwnMembers(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  resultLine
		want claim.TurnOutcome
	}{
		{"a refusal", resultLine{subtype: "success", isError: true, stopReason: "refusal", terminalReason: "api_error"}, claim.TurnRefused},
		{"a 529", resultLine{subtype: "success", isError: true, stopReason: "stop_sequence", terminalReason: "api_error", httpStatus: 529}, claim.TurnProviderError},
		{"max turns, whatever it says it ended on", resultLine{subtype: "error_max_turns", isError: true, terminalReason: "api_error"}, claim.TurnClean},
		{"a failure the provider did not make", resultLine{subtype: "error_during_execution", isError: true}, claim.TurnClean},
		{"a turn that ended well", resultLine{subtype: "success"}, claim.TurnClean},
	} {
		if got := classify(tc.res); got != tc.want {
			t.Errorf("%s is %q, want %q", tc.name, got, tc.want)
		}
	}
}
