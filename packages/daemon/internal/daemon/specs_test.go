package daemon

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
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

// A merger is told after its addressing which login the reviewer's App posts as, the one account
// whose approval its READY may name; no other claim is told it. A daemon that cannot read that
// App's identity starts no merger rather than one with no way to tell the reviewer's review.
func TestAMergerIsToldTheReviewAppsLogin(t *testing.T) {
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	logins := map[claim.Role]string{claim.RoleReviewer: "acme-review[bot]", claim.RoleImplementer: "acme-implement[bot]", claim.RoleMerger: "acme-implement[bot]"}
	identity := func(_ context.Context, role claim.Role) (runtime.GitIdentity, error) {
		return runtime.GitIdentity{Name: logins[role], Email: logins[role] + "@users.noreply.github.com"}, nil
	}
	const told = " Review App: the reviewer posts as `acme-review[bot]`."
	for _, role := range []claim.Role{claim.RoleMerger, claim.RoleReviewer, claim.RoleImplementer} {
		t.Run(string(role), func(t *testing.T) {
			token, err := claim.NewToken("s1", "S1-2", role)
			if err != nil {
				t.Fatal(err)
			}
			s := specs{stateDir: t.TempDir(), project: "s1", prompts: composer, identity: identity}
			spec, err := s.SpawnSpec(context.Background(), supervise.Claim{Token: token, Issue: "S1-2", Tree: "S1-1", Role: role})
			if err != nil {
				t.Fatalf("SpawnSpec: %v", err)
			}
			if got, want := strings.HasSuffix(spec.Prompt.Addressing, told), role == claim.RoleMerger; got != want || !want && strings.Contains(spec.Prompt.Addressing, "Review App:") {
				t.Fatalf("addressing %q; want it to end with %q: %t", spec.Prompt.Addressing, told, want)
			}
		})
	}

	token, err := claim.NewToken("s1", "S1-2", claim.RoleMerger)
	if err != nil {
		t.Fatal(err)
	}
	s := specs{stateDir: t.TempDir(), project: "s1", prompts: composer, identity: func(_ context.Context, role claim.Role) (runtime.GitIdentity, error) {
		if role == claim.RoleReviewer {
			return runtime.GitIdentity{}, errors.New("github_app_not_installed")
		}
		return identity(context.Background(), role)
	}}
	if _, err := s.SpawnSpec(context.Background(), supervise.Claim{Token: token, Issue: "S1-2", Tree: "S1-1", Role: claim.RoleMerger}); err == nil || !strings.Contains(err.Error(), "the review App's login") {
		t.Fatalf("SpawnSpec with no review App identity = %v, want a refusal naming the review App's login", err)
	}
}
