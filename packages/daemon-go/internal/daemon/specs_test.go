package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A tree's root architect is told the project's design gate policy after its addressing, the
// "Design gate policy" line its shared role prompt reads, and its composed role prompt carries what
// that policy asks of it under this daemon: register_gate under either policy, with approval first
// only when the gate is armed. No other claim is told the policy: a child's spec is never gated.
func TestARootArchitectIsToldTheDesignGatePolicyAndWhatItAsks(t *testing.T) {
	composer, err := prompts.New(filepath.Join("..", "..", "..", "pi-envoy", "roles"), t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	instructions := []string{
		"the tree's work starts only once you register your spec with `register_gate`, under either design gate policy",
		"With `root-issues`, request the spec's approval with `dispatch_request_approval`",
		"With `off`, request no approval and wait for no `design-approved`",
	}
	for _, policy := range []config.DesignGate{config.DesignGateRootIssues, config.DesignGateOff} {
		for _, tc := range []struct {
			name, issue string
			role        claim.Role
			root        bool
		}{
			{"the root architect", "S1-1", claim.RoleArchitect, true},
			{"a sub-architect", "S1-2", claim.RoleArchitect, false},
			{"a planner on the root", "S1-1", claim.RolePlanner, false},
		} {
			t.Run(string(policy)+"/"+tc.name, func(t *testing.T) {
				token, err := claim.NewToken("s1", tc.issue, tc.role)
				if err != nil {
					t.Fatal(err)
				}
				s := specs{stateDir: t.TempDir(), project: "s1", prompts: composer, designGate: policy}
				spec, err := s.SpawnSpec(context.Background(), supervise.Claim{Token: token, Issue: tc.issue, Tree: "S1-1", Role: tc.role})
				if err != nil {
					t.Fatalf("SpawnSpec: %v", err)
				}
				line := "Design gate policy: `gates.design: " + string(policy) + "`."
				if told := strings.HasSuffix(spec.Prompt.Addressing, " "+line); told != tc.root || !tc.root && strings.Contains(spec.Prompt.Addressing, "Design gate policy") {
					t.Fatalf("addressing %q; want it to end with %q: %t", spec.Prompt.Addressing, line, tc.root)
				}
				if !tc.root {
					return
				}
				var prompt strings.Builder
				for _, path := range spec.Prompt.RolePromptPaths {
					part, err := os.ReadFile(path)
					if err != nil {
						t.Fatalf("read role prompt part %s: %v", path, err)
					}
					prompt.Write(part)
				}
				for _, want := range instructions {
					if !strings.Contains(prompt.String(), want) {
						t.Errorf("the root architect's composed prompt does not say %q", want)
					}
				}
			})
		}
	}
}
