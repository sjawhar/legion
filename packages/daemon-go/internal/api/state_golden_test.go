package api

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/phase"
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
				Phase:      phase.Implementing,
				Status:     "in_progress",
				Architect: &ClaimView{
					Session: "ses_architect_208",
					State:   "ready",
					Locator: &runtime.Locator{
						Runtime:     runtime.RuntimeTmux,
						Claim:       "legion-legion-legion-208-architect",
						Incarnation: "31881:9442101",
						Tmux:        &runtime.TmuxLocator{Window: "@4", Pane: "%12"},
					},
				},
				Workers: map[claim.Role]PhaseView{
					claim.RolePlanner: {
						Claim: ClaimView{
							Session: "ses_planner_208",
							State:   "suspended",
							Locator: &runtime.Locator{
								Runtime:     runtime.RuntimeSandbox,
								Claim:       "legion-legion-legion-208-planner",
								Incarnation: "7f0c2f9a-6a4b-4f2e-9a1c-2f0d5a3b7e11",
								Sandbox:     &runtime.SandboxLocator{Namespace: "legion", Name: "legion-208-planner"},
							},
						},
						HandoffCommit: "9f2c1d7a4b6e8c3f5a90d2e14b7c6f8a3d5e0b21",
						Rounds:        0,
					},
					claim.RoleImplementer: {
						Claim: ClaimView{
							Session: "ses_implementer_208",
							State:   "working",
							Locator: &runtime.Locator{
								Runtime:     runtime.RuntimeSandbox,
								Claim:       "legion-legion-legion-208-implementer",
								Incarnation: "c41a8d3e-5b62-4f18-9d07-1e3a6c94b2f5",
								Sandbox:     &runtime.SandboxLocator{Namespace: "legion", Name: "legion-208-implementer"},
							},
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
	golden(t, "state.json", populatedState())
}

func TestStateStage3Golden(t *testing.T) {
	golden(t, "state-stage3.json", State{
		Daemon: DaemonInfo{
			Project:       "LEGION",
			SchemaVersion: 3,
			Boots:         5,
			FirstBootAt:   time.Date(2026, 9, 18, 14, 3, 27, 0, time.UTC),
			StartedAt:     time.Date(2026, 9, 22, 17, 30, 0, 0, time.UTC),
		},
		Admission: Admission{Cap: 2, Active: []string{"LEGION-208"}, Waiting: []string{"LEGION-209"}},
		Issues: map[string]Issue{
			"LEGION-208": {
				Key:        "LEGION-208",
				Generation: 4,
				Phase:      phase.Reviewing,
				Status:     "needs_review",
				Workers:    map[claim.Role]PhaseView{},
			},
		},
		PendingStatusWrites: []PendingStatusWrite{{
			Issue:     "LEGION-208",
			Payload:   json.RawMessage(`{"status":"needs_review"}`),
			Attempts:  2,
			NextAt:    time.Date(2026, 9, 22, 17, 32, 0, 0, time.UTC),
			LastError: "Dispatch unavailable",
		}},
	})
}

// golden pins one response's wire shape: Go writes it (`-update`), and
// `packages/contracts/src/legion-go-api.test.ts` parses it through the strict schema of the
// response it is. A fixture that differs from what Go now writes is stale.
func golden(t *testing.T, name string, value any) {
	t.Helper()
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	encoded = append(encoded, '\n')

	path := goldenPath(name)
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
		t.Fatalf("%s is stale\n got: %s\nwant: %s", name, encoded, want)
	}
}

// The register response is what the plugin reads its claim and its secret from.
func TestRegisterResponseGolden(t *testing.T) {
	golden(t, "register.json", claim.RegisterResponse{
		ClaimToken: "legion-legion-legion-208-architect",
		Tree:       "LEGION-208",
		Issue:      "LEGION-208",
		Role:       claim.RoleArchitect,
		Generation: 3,
		Secret:     "U3VwZXJ2aXNlZEJ5TGVnaW9u",
	})
}

// Every refusal a route answers is one sentence under `error`.
func TestErrorResponseGolden(t *testing.T) {
	golden(t, "error.json", claim.SameAgentRefusal)
}

// operatorClaim carries every optional an operator's view of a claim has.
func operatorClaim() OperatorClaim {
	delivered := time.Date(2026, 9, 22, 9, 31, 4, 0, time.UTC)
	return OperatorClaim{
		Token:       "legion-legion-legion-209-implementer",
		Tree:        "LEGION-208",
		Issue:       "LEGION-209",
		Role:        claim.RoleImplementer,
		Generation:  2,
		State:       "working",
		Session:     "ses_implementer_209",
		SessionFile: "/srv/legion/state/home/.omp/agent/sessions/ses_implementer_209.jsonl",
		Locator: &runtime.Locator{
			Runtime:     runtime.RuntimeTmux,
			Claim:       "legion-legion-legion-209-implementer",
			Incarnation: "40217:9551230",
			Tmux:        &runtime.TmuxLocator{Window: "@7", Pane: "%23"},
		},
		Budgets:         BudgetsView{LaunchFailures: 1, PromptFailures: 0, PromptRetires: 0},
		UncertainStreak: 0,
		Pending: &DeliveryView{
			ID:          "OVXA3ZC6BPIKTOQHKXHRZ7TYVB",
			Task:        "Implement the plan in .legion/plan.json.",
			QueuedAt:    time.Date(2026, 9, 22, 9, 30, 58, 0, time.UTC),
			DeliveredAt: &delivered,
			ConfirmedAt: &delivered,
		},
	}
}

// What the spawn, deliver, suspend, resume, and stop routes answer.
func TestOperatorClaimGolden(t *testing.T) {
	golden(t, "operator-claim.json", operatorClaim())
}

// What the list route answers: a queued claim carries no locator and no delivery yet.
func TestOperatorClaimsGolden(t *testing.T) {
	golden(t, "operator-claims.json", OperatorClaims{Claims: []OperatorClaim{
		{
			Token:      "legion-legion-legion-208-architect",
			Tree:       "LEGION-208",
			Issue:      "LEGION-208",
			Role:       claim.RoleArchitect,
			Generation: 0,
			State:      "queued",
		},
		operatorClaim(),
	}})
}

// An issue the daemon has admitted but not yet given a worker still has to answer the plugin's
// strict reader: `workers` is an object, never `null`.
func TestIssueWithoutWorkersMarshalsAnEmptyObject(t *testing.T) {
	encoded, err := json.Marshal(Issue{Key: "LEGION-209", Phase: phase.Held})
	if err != nil {
		t.Fatalf("marshal issue: %v", err)
	}
	if !bytes.Contains(encoded, []byte(`"workers":{}`)) {
		t.Fatalf("issue = %s, want an empty workers object", encoded)
	}
}

// Every Task 3.10 route has a fixture. The TypeScript Go-daemon client parses these exact
// responses, so adding a route cannot quietly leave its wire shape undocumented.
func TestTask310RouteGoldens(t *testing.T) {
	golden(t, "grant.json", GrantResponse{
		GrantID: "grant-for-one-command", ExpiresAt: "2026-09-23T12:01:00Z",
	})
	golden(t, "github-token.json", GitHubTokenResponse{
		Token: "installation-token", AppLogin: "legion-implementer[bot]",
	})
	golden(t, "git-credential.json", GitCredentialResponse{
		Username: "x-access-token", Password: "installation-token",
	})
	golden(t, "provisioning-credential.json", GitHubTokenResponse{
		Token: "installation-token", AppLogin: "legion-implementer[bot]",
	})
	golden(t, "handoff-complete.json", HandoffCompleteResponse{})
	golden(t, "issue-status.json", IssueStatusResponse{})
	golden(t, "gate-register.json", GateRegisterResponse{})
	golden(t, "wave-release.json", WaveReleaseResponse{Released: []string{"LEGION-209"}})
	golden(t, "phase-backward.json", PhaseBackwardResponse{})
	golden(t, "phase-retry.json", PhaseRetryResponse{})
	golden(t, "signoff.json", SignOffResponse{})
}
