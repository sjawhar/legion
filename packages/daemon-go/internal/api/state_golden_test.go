package api

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden fixtures this package pins")

// goldenPath is the cross-language pin: Go writes the wire shape, and
// `packages/contracts/src/legion-go-api.test.ts` parses it through the strict zod schema the
// plugin reads with. Go is the source of truth; the fixture is how the schema is held to it.
func goldenPath(name string) string {
	return filepath.Join("..", "..", "..", "contracts", "fixtures", "daemon-api", name)
}

// populatedState carries every optional the state shape has, so the fixture pins the full shape
// rather than Stage 1's empty record.
func populatedState() State {
	approvedVersion := 6
	return State{
		Daemon: DaemonInfo{
			Project:       "legion",
			SchemaVersion: 1,
			Boots:         4,
			FirstBootAt:   time.Date(2026, 9, 18, 14, 3, 27, 0, time.UTC),
			StartedAt:     time.Date(2026, 9, 22, 9, 15, 2, 0, time.UTC),
		},
		Admission: Admission{
			Cap:     2,
			Active:  []string{"LEGION-208", "LEGION-210"},
			Waiting: []string{"LEGION-211"},
		},
		Issues: map[string]Issue{
			"LEGION-208": {
				Key:        "LEGION-208",
				Generation: 3,
				Phase:      PhaseImplementing,
				Architect: &ClaimView{
					Session: "ses_architect_208",
					State:   "ready",
					Locator: json.RawMessage(
						`{"runtime":"tmux","incarnation":"legion-legion:@4:31881","tmuxSession":"legion-legion","tmuxWindowId":"@4","tmuxPaneId":"%12"}`,
					),
				},
				Workers: map[Role]PhaseView{
					RolePlanner: {
						Claim: ClaimView{
							Session: "ses_planner_208",
							State:   "suspended",
							Locator: json.RawMessage(
								`{"runtime":"sandbox","incarnation":"7f0c2f9a-6a4b-4f2e-9a1c-2f0d5a3b7e11","namespace":"legion","podName":"legion-208-planner"}`,
							),
						},
						HandoffCommit: "9f2c1d7a4b6e8c3f5a90d2e14b7c6f8a3d5e0b21",
						Rounds:        0,
					},
					RoleImplementer: {
						Claim: ClaimView{
							Session: "ses_implementer_208",
							State:   "working",
							Locator: json.RawMessage(
								`{"runtime":"sandbox","incarnation":"c41a8d3e-5b62-4f18-9d07-1e3a6c94b2f5","namespace":"legion","podName":"legion-208-implementer"}`,
							),
						},
						HandoffCommit: "",
						Rounds:        2,
					},
				},
				PullRequest: &PullRequestView{
					Number:         1247,
					Head:           "5b3d9e2c8f1a47d06b2e5c9f3a8d1e4b7c0f6a29",
					ChecksVerdict:  "pending",
					ReviewDecision: "changes_requested",
					FixAttempts:    1,
				},
				DesignGate: &GateView{
					ArtifactID:      "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093",
					CurrentVersion:  7,
					ApprovedVersion: &approvedVersion,
				},
				Slot: &SlotView{
					Index:      0,
					AdmittedAt: time.Date(2026, 9, 21, 18, 42, 11, 0, time.UTC),
				},
			},
		},
	}
}

func TestStateGolden(t *testing.T) {
	encoded, err := json.MarshalIndent(populatedState(), "", "  ")
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	encoded = append(encoded, '\n')

	path := goldenPath("state.json")
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create fixture directory: %v", err)
		}
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run `go test ./internal/api/ -update`): %v", err)
	}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("state.json is stale\n got: %s\nwant: %s", encoded, want)
	}
}

// An issue the daemon has admitted but not yet given a worker still has to answer the plugin's
// strict reader: `workers` is an object, never `null`.
func TestIssueWithoutWorkersMarshalsAnEmptyObject(t *testing.T) {
	encoded, err := json.Marshal(Issue{Key: "LEGION-209", Phase: PhaseHeld})
	if err != nil {
		t.Fatalf("marshal issue: %v", err)
	}
	if !bytes.Contains(encoded, []byte(`"workers":{}`)) {
		t.Fatalf("issue = %s, want an empty workers object", encoded)
	}
}
