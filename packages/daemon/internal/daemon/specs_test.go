package daemon

import (
	"context"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A tree's root architect is told the project's design gate policy after its addressing, the
// "Design gate policy" line its shared role prompt reads, and its composed role prompt carries what
// that policy asks of it under this daemon: register_gate under either policy, with approval first
// only when the gate is armed. No other claim is told the policy: a child's spec is never gated.
func TestARootArchitectIsToldTheDesignGatePolicyAndWhatItAsks(t *testing.T) {
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	instructions := []string{
		"the tree's work starts only once you register your spec with `register_gate`, under either design gate policy",
		"With `root-issues`, once the spec's decision blocks are settled (the dispatch skill's \"Approval of a spec\"), request its approval with `dispatch_request_approval` and a `summary`",
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

// A reviewer is told after its addressing which required workflows its project declares as review
// workflows, or that it declares none: the reviewer adjudicates a red only those make, and
// requests changes on any other red required workflow. No other claim is told them.
func TestAReviewerIsToldItsProjectsReviewWorkflows(t *testing.T) {
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	for _, tc := range []struct {
		name     string
		role     claim.Role
		declared []string
		want     string
	}{
		{"a reviewer, two declared", claim.RoleReviewer, []string{".github/workflows/review.yml", ".github/workflows/bot.yml"},
			" Review workflows: this project declares `.github/workflows/review.yml`, `.github/workflows/bot.yml` (`review_workflows`)."},
		{"a reviewer, none declared", claim.RoleReviewer, nil, " Review workflows: this project declares none (`review_workflows`)."},
		{"a tester", claim.RoleTester, []string{".github/workflows/review.yml"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token, err := claim.NewToken("s1", "S1-2", tc.role)
			if err != nil {
				t.Fatal(err)
			}
			s := specs{stateDir: t.TempDir(), project: "s1", prompts: composer, reviewWorkflows: tc.declared}
			spec, err := s.SpawnSpec(context.Background(), supervise.Claim{Token: token, Issue: "S1-2", Tree: "S1-1", Role: tc.role})
			if err != nil {
				t.Fatalf("SpawnSpec: %v", err)
			}
			if told := strings.Contains(spec.Prompt.Addressing, "Review workflows:"); told != (tc.want != "") || !strings.HasSuffix(spec.Prompt.Addressing, tc.want) {
				t.Fatalf("addressing %q; want it to end with %q", spec.Prompt.Addressing, tc.want)
			}
		})
	}
}

// The daemon's controller (`controller: daemon`) is launched with its role part and the headless
// part, told the project's design gate policy as the operator's controller is, with the
// deployment's instructions and launch secrets, and with no repository and no git identity: it
// works Dispatch, never a checkout, and commits nothing.
func TestTheControllersLaunchIsItsHeadlessPromptAndNoCheckout(t *testing.T) {
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	s := specs{
		stateDir: t.TempDir(), project: "s1", prompts: composer, designGate: config.DesignGateOff,
		instructions: "/state/deployment-instructions.md", secrets: map[string]string{"ENVOY_TOKEN": "envoy-bearer"},
		repo: ghrepo.MustParse("acme/widgets"),
		identity: func(context.Context, claim.Role) (runtime.GitIdentity, error) {
			t.Error("the controller's launch asked for a git identity")
			return runtime.GitIdentity{}, nil
		},
	}
	spec, err := s.SpawnSpec(context.Background(), supervise.Claim{Token: claim.ControllerToken("s1"), Project: "s1", Role: claim.RoleController})
	if err != nil {
		t.Fatalf("SpawnSpec: %v", err)
	}
	want, err := composer.ControllerPromptPaths(true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(spec.Prompt.RolePromptPaths, want) {
		t.Errorf("role prompts = %q, want %q", spec.Prompt.RolePromptPaths, want)
	}
	if spec.Prompt.Addressing != DesignGateFragment(config.DesignGateOff) {
		t.Errorf("addressing = %q, want the design gate policy alone", spec.Prompt.Addressing)
	}
	if spec.Prompt.DeploymentInstructionsPath != s.instructions {
		t.Errorf("instructions = %q, want %q", spec.Prompt.DeploymentInstructionsPath, s.instructions)
	}
	if !spec.Repository.IsZero() || len(spec.Env) != 0 || spec.WorkspaceRecoveredFrom != "" {
		t.Errorf("repository %q, env %v, recovered from %q; want none of them", spec.Repository, spec.Env, spec.WorkspaceRecoveredFrom)
	}
	if !maps.Equal(spec.Secrets, s.secrets) {
		t.Errorf("secrets = %v, want the launch secrets %v", spec.Secrets, s.secrets)
	}
}
