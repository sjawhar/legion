package projection

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden fixtures this package pins")

// The daemon's own controller (`controller: daemon`) holds a claim on no issue and no tree. It is
// the project's controller, which the state shows as controllerLocator, and no issue's worker: an
// issue keyed "" with a `controller` worker refuses the whole document at every strict client,
// among them the daemon-launched controller's own claim, which reads the state before it
// registers, and every agent's read_record. The document is what Project emits for that claim
// beside a tree's architect, completed as the state route completes it (internal/daemon's
// source.State), and it is packages/contracts' state-controller-claim fixture: the contract's own
// schema parses it (packages/contracts/src/legion-api.test.ts), and the plugin's daemon-launched
// controller reads it as its daemon's state (packages/pi-legion/extensions/legion.test.ts).
func TestTheStateWithTheControllersClaimGolden(t *testing.T) {
	const issue = "LEGION-208"
	architect, err := claim.NewToken("legion", issue, claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	controller := claim.ControllerToken("legion")
	admitted := time.Date(2026, 10, 6, 18, 2, 11, 0, time.UTC)
	store := projectionStore{
		issues: []record.Issue{{Key: issue, Tree: issue, Status: "in_progress", Phase: phase.Admitted, Generation: 1, HandedOver: true}},
		phases: map[string][]record.PhaseRow{issue: {{Issue: issue, Role: claim.RoleArchitect, Claim: architect}}},
		slots:  []record.Slot{{Issue: issue, Index: 0, AdmittedAt: admitted}},
	}
	architectLocator := sandboxLocator(architect, "legion-legion-legion-208", "4b7e1c2a-0d93-4f6e-8a51-3c2f9e7d1b04", 1)
	// The architect's session reported its live rows with its ready (LEGION-663), from the process
	// the locator names, so the fixture carries a claim's capability report; the controller's ready
	// carries none, and its claim is on no issue besides.
	reports := map[claim.Token]capabilities.Report{
		architect: {
			Claim: architect, Generation: 1, Locator: *architectLocator,
			MeasuredAt: admitted.Add(45 * time.Second), ReportedAt: admitted.Add(46 * time.Second), ElapsedMs: 1200,
			Rows: []capabilities.Row{
				{Name: capabilities.Subagents, OK: true, Detail: "6 task agents discovered: deep-worker, oracle, plan-gap-analyst, plan-reviewer, thermonuclear-code-quality, thermonuclear-deep-review"},
				{Name: capabilities.WebSearch, OK: true, Detail: "web_search is active and a query answered"},
				{Name: capabilities.MCP, OK: true, Detail: "no MCP server is configured"},
				{Name: capabilities.RepositoryExtensions, OK: true, Detail: "2 extension paths discovered"},
				{Name: capabilities.DispatchEnvoyTools, OK: true, Detail: "dispatch read LEGION-208 answered; envoy_whoami is active"},
				{Name: capabilities.GitHub, Detail: "gh api user: HTTP 401: Bad credentials"},
			},
		},
	}
	state, err := Project(context.Background(), nil, store, "LEGION", []supervise.Claim{
		{
			Token: architect, Project: "legion", Tree: issue, Issue: issue, Role: claim.RoleArchitect,
			State: supervise.StateReady, Session: "ses_architect_208", Locator: architectLocator,
		},
		{
			Token: controller, Project: "legion", Role: claim.RoleController,
			State: supervise.StateReady, Session: "ses_controller", Locator: sandboxLocator(controller, "legion-legion-controller", "9d2a6f13-7c48-4e0b-b5a9-1f8e3d6c2a57", 1),
		},
	}, reports)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	for key, view := range state.Issues {
		if key != issue {
			t.Errorf("the state lists issue %q (%+v); the controller's claim is on no issue", key, view)
		}
		if worker, listed := view.Workers[claim.RoleController]; listed {
			t.Errorf("issue %q lists a controller worker %+v", key, worker)
		}
	}
	if view := state.Issues[issue]; view.Architect == nil || view.Architect.Session != "ses_architect_208" {
		t.Errorf("%s's architect = %+v, want its ready claim", issue, view.Architect)
	} else if view.Architect.Capabilities == nil || view.Architect.Capabilities.Incarnation != architectLocator.Incarnation {
		t.Errorf("%s's architect capabilities = %+v, want its session's report from the process the locator names", issue, view.Architect.Capabilities)
	}

	state.Daemon = api.DaemonInfo{Project: "LEGION", SchemaVersion: 35, Boots: 2, FirstBootAt: admitted.Add(-time.Hour), StartedAt: admitted.Add(-time.Minute)}
	state.Admission.Cap = 2
	state.ControllerLocator = &api.ControllerLocator{Runtime: "kubernetes", External: true, SessionID: "ses_controller", RegisteredAt: admitted.Add(-30 * time.Second)}
	golden(t, "state-controller-claim.json", state)
}

// sandboxLocator is one role process in a Sandbox (contract 15): the Sandbox's name, the pod's uid,
// the role's container (the role the token names, the controller's included) and the process
// generation, its incarnation `<pod uid>/<generation>`.
func sandboxLocator(token claim.Token, sandbox, podUID string, generation uint64) *runtime.Locator {
	role, _ := token.Role()
	return &runtime.Locator{
		Runtime: runtime.RuntimeSandbox, Claim: token, Incarnation: runtime.SandboxIncarnation(podUID, generation),
		Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: sandbox, PodUID: podUID, Container: string(role), Generation: generation},
	}
}

// golden pins the state document Project's caller serves: Go writes it (`-update`), and
// `packages/contracts/src/legion-api.test.ts` parses it through the strict schema the plugin reads
// the state with. A fixture that differs from what Go now writes is stale. It is internal/api's
// golden helper with this package's command in its message: the daemon-api fixtures are written
// from two packages, so a state-shape change takes both `-update` runs whether or not the helper is
// shared. Once a third package writes one, the helper moves into an internal/testgolden package.
func golden(t *testing.T, name string, value any) {
	t.Helper()
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	encoded = append(encoded, '\n')
	path := filepath.Join("..", "..", "..", "contracts", "fixtures", "daemon-api", name)
	if *updateGolden {
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run `go test ./internal/projection/ -update`): %v", err)
	}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("%s is stale\n got: %s\nwant: %s", name, encoded, want)
	}
}
