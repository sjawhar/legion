package daemon

import (
	"context"
	"errors"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
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
		"With `root-issues`, once the spec's decision blocks are settled (the dispatch skill's \"Approval of a spec\"), request its approval with `dispatch request-approval` and a `--summary`",
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

func TestAMergerIsToldItsProjectsMergeRules(t *testing.T) {
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	rules := config.MergeRules{
		ByHand: []config.PathPattern{{Expression: "^migrations/versions/", Regexp: regexp.MustCompile("^migrations/versions/")}},
	}
	for _, tc := range []struct {
		name string
		role claim.Role
		want bool
	}{
		{"merger", claim.RoleMerger, true},
		{"implementer", claim.RoleImplementer, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token, err := claim.NewToken("s1", "S1-2", tc.role)
			if err != nil {
				t.Fatal(err)
			}
			s := specs{stateDir: t.TempDir(), project: "s1", prompts: composer, mergeRules: rules}
			spec, err := s.SpawnSpec(context.Background(), supervise.Claim{Token: token, Issue: "S1-2", Tree: "S1-1", Role: tc.role})
			if err != nil {
				t.Fatalf("SpawnSpec: %v", err)
			}
			told := strings.Contains(spec.Prompt.Addressing, "Merge policy: by_hand `^migrations/versions/`.")
			if told != tc.want {
				t.Fatalf("addressing %q; merge policy present = %t, want %t", spec.Prompt.Addressing, told, tc.want)
			}
		})
	}
}

// The daemon's controller (`controller: daemon`) is launched with its role part and the headless
// part, told the project's design gate policy as the operator's controller is, with the
// deployment's instructions and launch secrets, and with no repository, no git identity and
// neither App login: it works Dispatch, never a checkout or GitHub, and commits nothing.
func TestTheControllersLaunchIsItsHeadlessPromptAndNoCheckout(t *testing.T) {
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	s := specs{
		stateDir: t.TempDir(), project: "s1", prompts: composer, designGate: config.DesignGateOff,
		instructions: "/state/deployment-instructions.md", secrets: map[string]string{"ENVOY_TOKEN": "envoy-bearer"},
		repository: func(context.Context, string) (ghrepo.Repository, string, error) {
			t.Error("the controller's launch asked for a repository")
			return ghrepo.MustParse("acme/widgets"), "main", nil
		},
		identity: func(context.Context, claim.Role, string) (runtime.GitIdentity, error) {
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
	if !spec.Repository.IsZero() || spec.DefaultBranch != "" || len(spec.Env) != 0 || spec.WorkspaceRecoveredFrom != "" {
		t.Errorf("repository %q, default branch %q, env %v, recovered from %q; want none of them", spec.Repository, spec.DefaultBranch, spec.Env, spec.WorkspaceRecoveredFrom)
	}
	if !maps.Equal(spec.Secrets, s.secrets) {
		t.Errorf("secrets = %v, want the launch secrets %v", spec.Secrets, s.secrets)
	}
}

// The daemon's controller is told the project's Slack channels after its design gate policy, as the
// list its skill answers mentions from; a project with no channel gets no prompt line. Its
// environment's LEGION_SLACK_CHANNELS is the runtime's to set (sandbox.Options.ControllerSlackChannels),
// never the spec's, which the runtime would refuse.
func TestTheControllersLaunchNamesTheProjectsSlackChannels(t *testing.T) {
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	launch := func(channels []string) runtime.SpawnSpec {
		t.Helper()
		s := specs{
			stateDir: t.TempDir(), project: "s1", prompts: composer, designGate: config.DesignGateRootIssues,
			slackChannels: channels,
		}
		spec, err := s.SpawnSpec(context.Background(), supervise.Claim{Token: claim.ControllerToken("s1"), Project: "s1", Role: claim.RoleController})
		if err != nil {
			t.Fatalf("SpawnSpec: %v", err)
		}
		return spec
	}
	spec := launch([]string{"C00000000", "G00000001"})
	if _, set := spec.Env["LEGION_SLACK_CHANNELS"]; set {
		t.Errorf("the spec sets LEGION_SLACK_CHANNELS (%v), the runtime's variable", spec.Env)
	}
	want := "Design gate policy: `gates.design: root-issues`. Slack channels: `C00000000`, `G00000001`: " +
		"your session hears each one's app mentions while it holds the controller role; answer each in its thread, " +
		"and nothing from any other channel (skill://legion-controller/references/slack-mentions.md)."
	if spec.Prompt.Addressing != want {
		t.Errorf("addressing = %q, want %q", spec.Prompt.Addressing, want)
	}
	if none := launch(nil); none.Prompt.Addressing != DesignGateFragment(config.DesignGateRootIssues) {
		t.Errorf("with no channel: addressing %q, want the design gate policy alone", none.Prompt.Addressing)
	}
}

// A worker's launch is on the repository its issue works, at the default branch the runtime
// answers, whatever its name, which each runtime provisions the issue's workspace at; two issues on
// two owners' repositories each launch on their own. A repository that cannot be read refuses the
// launch, naming the issue. What an issue works, its record's repository or, unrecorded, the
// project's default, is the workflow's (TestAnUnrecordedIssueWorksTheProjectsDefaultRepository).
func TestAWorkerLaunchIsOnItsIssuesRecordedRepository(t *testing.T) {
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	recorded := map[string][2]string{"S1-2": {"acme/widgets", "main"}, "S1-3": {"zeta/tools", "release/v7.1"}}
	var asked []string
	s := specs{stateDir: t.TempDir(), project: "s1", prompts: composer,
		repository: func(_ context.Context, issue string) (ghrepo.Repository, string, error) {
			asked = append(asked, issue)
			if issue == "S1-9" {
				return ghrepo.Repository{}, "", errors.New("read the record of S1-9: the database refused")
			}
			named := recorded[issue]
			return ghrepo.MustParse(named[0]), named[1], nil
		}}
	for issue, want := range recorded {
		token, err := claim.NewToken("s1", issue, claim.RoleImplementer)
		if err != nil {
			t.Fatal(err)
		}
		spec, err := s.SpawnSpec(context.Background(), supervise.Claim{Token: token, Issue: issue, Tree: "S1-1", Role: claim.RoleImplementer})
		if err != nil || spec.Repository.String() != want[0] || spec.DefaultBranch != want[1] {
			t.Fatalf("the launch of %s names %s at %q, %v; want %s at %s", issue, spec.Repository, spec.DefaultBranch, err, want[0], want[1])
		}
	}

	refused, err := claim.NewToken("s1", "S1-9", claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SpawnSpec(context.Background(), supervise.Claim{Token: refused, Issue: "S1-9", Tree: "S1-1", Role: claim.RoleImplementer}); err == nil ||
		!strings.Contains(err.Error(), "the repository of "+string(refused)) || !strings.Contains(err.Error(), "the database refused") {
		t.Fatalf("SpawnSpec with an unreadable record = %v, want the refusal naming the claim and the read", err)
	}
	if len(asked) != 3 {
		t.Fatalf("asked for the repositories of %v, want each launch's issue once", asked)
	}
}

// spec.Plugins is set iff the runtime is Kubernetes, the claim has registered a session, and its
// recorded contract differs from this daemon's — never for tmux, a claim with no session, a claim
// already at the daemon's contract, or a daemon that knows no plugin pair (LEGION-164's degraded
// mode) — and always with Contract already api.DaemonAPIVersion.
func TestSpawnSpecPluginsIsSetOnlyForAStaleKubernetesClaimWithASession(t *testing.T) {
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	pair := &runtime.PluginInstall{PiLegion: "9.7.0", PiEnvoy: "9.6.1", Contract: api.DaemonAPIVersion}
	token, err := claim.NewToken("s1", "S1-2", claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	base := supervise.Claim{Token: token, Issue: "S1-2", Tree: "S1-1", Role: claim.RoleImplementer, Session: "sess-1"}
	for _, tc := range []struct {
		name        string
		runtimeName string
		plugins     *runtime.PluginInstall
		claim       supervise.Claim
		want        bool
	}{
		{"kubernetes, session, recorded 0", "kubernetes", pair, base, true},
		{"kubernetes, session, recorded a stale number", "kubernetes", pair, withContract(base, 23), true},
		{"kubernetes, no session", "kubernetes", pair, withSession(base, ""), false},
		{"kubernetes, at the daemon's contract", "kubernetes", pair, withContract(base, api.DaemonAPIVersion), false},
		{"kubernetes, no known pair", "kubernetes", nil, base, false},
		{"tmux", "tmux", pair, base, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := specs{stateDir: t.TempDir(), project: "s1", prompts: composer, runtimeName: tc.runtimeName, plugins: tc.plugins}
			spec, err := s.SpawnSpec(context.Background(), tc.claim)
			if err != nil {
				t.Fatalf("SpawnSpec: %v", err)
			}
			if tc.want {
				if spec.Plugins == nil || *spec.Plugins != *pair {
					t.Fatalf("Plugins = %+v, want %+v", spec.Plugins, pair)
				}
				return
			}
			if spec.Plugins != nil {
				t.Fatalf("Plugins = %+v, want nil", spec.Plugins)
			}
		})
	}
}

// The daemon's own controller (`controller: daemon`) is covered the same way as any claim.
func TestSpawnSpecPluginsCoversTheDaemonsControllerToo(t *testing.T) {
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	pair := &runtime.PluginInstall{PiLegion: "9.7.0", PiEnvoy: "9.6.1", Contract: api.DaemonAPIVersion}
	s := specs{stateDir: t.TempDir(), project: "s1", prompts: composer, runtimeName: "kubernetes", plugins: pair}
	c := supervise.Claim{Token: claim.ControllerToken("s1"), Project: "s1", Role: claim.RoleController, Session: "sess-1", PluginContract: 23}
	spec, err := s.SpawnSpec(context.Background(), c)
	if err != nil {
		t.Fatalf("SpawnSpec: %v", err)
	}
	if spec.Plugins == nil || *spec.Plugins != *pair {
		t.Fatalf("the controller's Plugins = %+v, want %+v", spec.Plugins, pair)
	}
}

func withSession(c supervise.Claim, session string) supervise.Claim {
	c.Session = session
	return c
}

func withContract(c supervise.Claim, contract int) supervise.Claim {
	c.PluginContract = contract
	return c
}
