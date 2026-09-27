package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A tree's root architect is told the project's design gate policy after its addressing, the
// "Design gate policy" line its shared role prompt reads, so an architect that starts on its own
// knows whether to ask for approval. No other claim is told it: a child's spec is never gated.
func TestOnlyATreesRootArchitectIsToldTheDesignGatePolicy(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stateDir, "prompts"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, policy := range []config.DesignGate{config.DesignGateRootIssues, config.DesignGateOff} {
		for _, tc := range []struct {
			name, issue string
			role        claim.Role
			told        bool
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
				if err := os.WriteFile(rolePromptPath(stateDir, token), []byte("role\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				s := specs{stateDir: stateDir, project: "s1", designGate: policy}
				spec, err := s.SpawnSpec(context.Background(), supervise.Claim{Token: token, Issue: tc.issue, Tree: "S1-1", Role: tc.role})
				if err != nil {
					t.Fatalf("SpawnSpec: %v", err)
				}
				line := "Design gate policy: `gates.design: " + string(policy) + "`"
				if told := strings.Contains(spec.Prompt.Addressing, "Design gate policy"); told != tc.told || tc.told && !strings.Contains(spec.Prompt.Addressing, line) {
					t.Fatalf("addressing %q; want the policy line %q: %t", spec.Prompt.Addressing, line, tc.told)
				}
			})
		}
	}
}
