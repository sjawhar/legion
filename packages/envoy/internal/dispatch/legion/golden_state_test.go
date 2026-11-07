package legion

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// goldenStatePath is the state document the Legion daemon's own code writes:
// `packages/daemon/internal/api/state_golden_test.go` builds the daemon's populated state and
// writes it here (`golden(t, "state.json", populatedState(t))`), so these bytes are the daemon's
// wire shape rather than a reading of it. dispatch://INFRA-1077's acceptance A4 asks for a test
// on this side that reads such a document, because a Dispatch-side decoder held only to
// hand-written literals cannot tell when the daemon's own shape moves.
func goldenStatePath() string {
	return filepath.Join("..", "..", "..", "..", "contracts", "fixtures", "daemon-api", "state.json")
}

// TestTheDecodeReadsTheStateDocumentTheDaemonWrote: the sampler's decode reads the daemon's own
// golden, and every field the Legion view and the four metrics take from a read survives it — the
// settings the settings control shows with the source of each (the spec's contract 4), the
// admission block the view's header and queue draw, each issue's phase and claims, and the pull
// request's `requiresChecks`, which metric 2's CI-wait segment reads.
func TestTheDecodeReadsTheStateDocumentTheDaemonWrote(t *testing.T) {
	body, err := os.ReadFile(goldenStatePath())
	if err != nil {
		t.Fatalf("read the daemon's state golden: %v", err)
	}
	server := stateServer(t, string(body))

	state, size, err := New(server.URL, "").State(t.Context())
	if err != nil {
		t.Fatalf("decode the daemon's own state document: %v", err)
	}
	if size != len(body) {
		t.Errorf("read %d bytes of a %d-byte document", size, len(body))
	}
	if len(state.Settings) == 0 || len(state.SettingSources) == 0 {
		t.Fatalf("settings = %d keys, settingSources = %d keys, want the daemon's own of each",
			len(state.Settings), len(state.SettingSources))
	}
	for key := range state.Settings {
		if _, named := state.SettingSources[key]; !named {
			t.Errorf("setting %q has no entry in settingSources: contract 4 gives every key its source", key)
		}
	}
	if state.Admission.Cap == nil || *state.Admission.Cap == 0 {
		t.Errorf("admission.cap = 0: the view's header draws the slots in use against it")
	}
	if len(state.Issues) == 0 {
		t.Fatalf("the golden carries no issue, so this test proves nothing")
	}

	// The observations a read of this document writes: the three project rows the view's header
	// and its settings control read, and a phase for each issue.
	fields := map[string]int{}
	for _, observation := range state.Observations() {
		fields[observation.Field]++
	}
	for _, field := range []string{FieldAdmission, FieldSettings, FieldSettingSources} {
		if fields[field] != 1 {
			t.Errorf("observations of %s = %d, want exactly one project row", field, fields[field])
		}
	}
	if fields[FieldPhase] != len(state.Issues) {
		t.Errorf("phase observations = %d, want one per issue (%d)", fields[FieldPhase], len(state.Issues))
	}

	// requiresChecks, which the daemon added for this change, reaches the metrics as the daemon
	// wrote it: at least one pull request of the golden carries the field.
	carried := 0
	for _, issue := range state.Issues {
		if issue.PullRequest != nil && len(issue.PullRequest.RequiresChecks) > 0 {
			var requires bool
			if err := json.Unmarshal(issue.PullRequest.RequiresChecks, &requires); err != nil {
				t.Errorf("requiresChecks of %s is %s, which is not a boolean: %v",
					issue.Key, issue.PullRequest.RequiresChecks, err)
			}
			carried++
		}
	}
	if carried == 0 {
		t.Errorf("no pull request of the daemon's own state carries requiresChecks, which metric 2's CI-wait segment reads")
	}
}
