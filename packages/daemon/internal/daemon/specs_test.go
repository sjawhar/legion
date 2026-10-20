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

// noRepositoriesView is the supplier of a daemon whose project maps and refuses no repository.
func noRepositoriesView() api.RepositoriesView { return api.RepositoriesView{} }

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
				s := specs{stateDir: t.TempDir(), project: "s1", prompts: composer, designGate: policy, repositories: noRepositoriesView}
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
			s := specs{stateDir: t.TempDir(), project: "s1", prompts: composer, mergeRules: rules, repositories: noRepositoriesView}
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

// A role worker_backends puts on claude is launched on Claude Code and told the Claude Code part
// after its role's parts; every other role, and every role when the daemon names no backend, on Oh
// My Pi with its parts alone.
func TestAClaimsBackendIsItsRolesAndAClaudeRoleIsToldClaudeCodesTools(t *testing.T) {
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	onClaude := func(role claim.Role) runtime.Backend {
		if role == claim.RoleImplementer {
			return runtime.BackendClaude
		}
		return runtime.BackendOmp
	}
	for _, tc := range []struct {
		name    string
		role    claim.Role
		backend func(claim.Role) runtime.Backend
		want    runtime.Backend
	}{
		{"an implementer on claude", claim.RoleImplementer, onClaude, runtime.BackendClaude},
		{"a tester on omp", claim.RoleTester, onClaude, runtime.BackendOmp},
		{"a daemon that names no backend", claim.RoleImplementer, nil, runtime.BackendOmp},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token, err := claim.NewToken("s1", "S1-2", tc.role)
			if err != nil {
				t.Fatal(err)
			}
			s := specs{stateDir: t.TempDir(), project: "s1", prompts: composer, backend: tc.backend, repositories: noRepositoriesView}
			spec, err := s.SpawnSpec(context.Background(), supervise.Claim{Token: token, Issue: "S1-2", Tree: "S1-1", Role: tc.role})
			if err != nil {
				t.Fatalf("SpawnSpec: %v", err)
			}
			if spec.Backend != tc.want {
				t.Errorf("backend = %q, want %q", spec.Backend, tc.want)
			}
			parts, err := composer.Compose(tc.role, false)
			if err != nil {
				t.Fatal(err)
			}
			want := parts.RolePromptPaths
			if tc.want == runtime.BackendClaude {
				want = append(want, composer.Part(runtime.Backends[runtime.BackendClaude].PromptPart))
			}
			if !slices.Equal(spec.Prompt.RolePromptPaths, want) {
				t.Errorf("role prompts = %q, want %q", spec.Prompt.RolePromptPaths, want)
			}
		})
	}
	body, err := os.ReadFile(composer.Part(runtime.Backends[runtime.BackendClaude].PromptPart))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"`Read`, `Write` and `Edit`", "There is no `grep`, `glob` or `find` tool", "`Agent` tool", "`mcp__legion__legion`", "`Skill` tool"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the Claude Code part does not say %q", want)
		}
	}
}

// A worker's addressing ends naming its issue's repository and default branch, then the project's
// repositories in the order the daemon serves them. A daemon that runs no workflow resolves no
// repository for its claims: the whole sentence is left off, not just the issue's own repository.
// A recorded issue keeps its repository after a refresh unmaps or refuses
// every one, so the clause then says the daemon maps none, as the controller's does, and never
// ends in "are .".
func TestAWorkersAddressingNamesItsRepositoryAndTheProjects(t *testing.T) {
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	view := api.RepositoriesView{Mapped: []api.MappedRepositoryView{
		{Repo: "acme/gadgets"}, {Repo: "acme/widgets"}, {Repo: "sjawhar/legion-smoke"},
	}}
	s := specs{
		stateDir: t.TempDir(), project: "s1", prompts: composer,
		repository: func(context.Context, string) (ghrepo.Repository, string, error) {
			return ghrepo.MustParse("acme/widgets"), "release/v7.1", nil
		},
		repositories: func() api.RepositoriesView { return view },
	}
	token, err := claim.NewToken("s1", "S1-2", claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	launch := func() string {
		t.Helper()
		spec, err := s.SpawnSpec(context.Background(), supervise.Claim{Token: token, Issue: "S1-2", Tree: "S1-1", Role: claim.RoleImplementer})
		if err != nil {
			t.Fatalf("SpawnSpec: %v", err)
		}
		return spec.Prompt.Addressing
	}
	const own = "Your issue's repository is acme/widgets (default branch release/v7.1); "
	for _, tc := range []struct {
		name string
		view api.RepositoriesView
		want string
	}{
		{"the mapping", view, own + "the project's repositories are acme/gadgets, acme/widgets, sjawhar/legion-smoke."},
		{"an empty mapping", api.RepositoriesView{}, own + "the daemon maps no repository to this project now."},
		{
			"every repository refused",
			api.RepositoriesView{Refused: []api.RefusedRepositoryView{
				{Repo: "acme/widgets", Reason: "the review App is not installed on acme/widgets"},
				{Repo: "acme/gadgets", Reason: "the implement App is not installed on acme/gadgets"},
			}},
			own + "the daemon maps no repository to this project now: " +
				"acme/widgets (the review App is not installed on acme/widgets); acme/gadgets (the implement App is not installed on acme/gadgets).",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s.repositories = func() api.RepositoriesView { return tc.view }
			if got := launch(); !strings.HasSuffix(got, tc.want) {
				t.Fatalf("addressing = %q, want it to end with %q", got, tc.want)
			}
		})
	}

	s.repositories = func() api.RepositoriesView { return view }
	s.repository = nil
	if got := launch(); strings.Contains(got, "repositories are") || strings.Contains(got, "repository is") {
		t.Fatalf("addressing with no repository resolved (a daemon with no workflow) = %q, want no repository sentence", got)
	}
}

// The daemon's controller (`controller: daemon`) is launched with its role part and the headless
// part, told the project's design gate policy and repositories as the operator's controller is,
// with the deployment's instructions and launch secrets, and with no repository, no git identity
// and neither App login: it works Dispatch, never a checkout or GitHub, and commits nothing.
func TestTheControllersLaunchIsItsHeadlessPromptAndNoCheckout(t *testing.T) {
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	s := specs{
		stateDir: t.TempDir(), project: "s1", prompts: composer, designGate: config.DesignGateOff, repositories: noRepositoriesView,
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
	if want := "Design gate policy: `gates.design: off`. The daemon maps no repository to this project now."; spec.Prompt.Addressing != want {
		t.Errorf("addressing = %q, want %q", spec.Prompt.Addressing, want)
	}
	if spec.Prompt.DeploymentInstructionsPath != s.instructions {
		t.Errorf("instructions = %q, want %q", spec.Prompt.DeploymentInstructionsPath, s.instructions)
	}
	if !spec.Repository.IsZero() || spec.DefaultBranch != "" || len(spec.Env) != 0 || spec.WorkspaceRecoveredFrom != "" {
		t.Errorf("repository %q, default branch %q, env %v, recovered from %q; want none of them", spec.Repository, spec.DefaultBranch, spec.Env, spec.WorkspaceRecoveredFrom)
	}
	if spec.Backend != runtime.BackendOmp {
		t.Errorf("backend = %q, want omp, named", spec.Backend)
	}
	if !maps.Equal(spec.Secrets, s.secrets) {
		t.Errorf("secrets = %v, want the launch secrets %v", spec.Secrets, s.secrets)
	}
}

// The daemon's controller is told the project's Slack channels after its repositories, as the
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
			slackChannels: channels, repositories: noRepositoriesView,
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
	want := "Design gate policy: `gates.design: root-issues`. The daemon maps no repository to this project now. Slack channels: `C00000000`, `G00000001`: " +
		"your session hears each one's app mentions while it holds the controller role; answer each in its thread, " +
		"and nothing from any other channel (skill://legion-controller/references/slack-mentions.md)."
	if spec.Prompt.Addressing != want {
		t.Errorf("addressing = %q, want %q", spec.Prompt.Addressing, want)
	}
	if none := launch(nil); none.Prompt.Addressing != "Design gate policy: `gates.design: root-issues`. The daemon maps no repository to this project now." {
		t.Errorf("with no channel: addressing %q, want no Slack line", none.Prompt.Addressing)
	}
}

// The controller's addressing gains the project's repositories after its design gate policy, as a
// sentence of its own, so `legion controller start`'s operator and the daemon-launched controller
// both read where the project's issues live; a worker's addressing carries the same clause after
// a `; `.
func TestTheControllersAddressingGainsTheProjectsRepositories(t *testing.T) {
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	for _, tc := range []struct {
		name string
		view api.RepositoriesView
		want string
	}{
		{
			"the mapping",
			api.RepositoriesView{Mapped: []api.MappedRepositoryView{{Repo: "acme/widgets"}, {Repo: "sjawhar/legion-smoke"}}},
			"The project's repositories are acme/widgets, sjawhar/legion-smoke.",
		},
		// An empty mapping is a state a running project reaches, never a refusal to launch.
		{"an empty mapping", api.RepositoriesView{}, "The daemon maps no repository to this project now."},
		{
			"every repository refused",
			api.RepositoriesView{Refused: []api.RefusedRepositoryView{{Repo: "acme/widgets", Reason: "the review App is not installed on acme/widgets"}}},
			"The daemon maps no repository to this project now: acme/widgets (the review App is not installed on acme/widgets).",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := specs{
				stateDir: t.TempDir(), project: "s1", prompts: composer, designGate: config.DesignGateOff,
				repositories: func() api.RepositoriesView { return tc.view },
			}
			spec, err := s.SpawnSpec(context.Background(), supervise.Claim{Token: claim.ControllerToken("s1"), Project: "s1", Role: claim.RoleController})
			if err != nil {
				t.Fatalf("SpawnSpec: %v", err)
			}
			if want := DesignGateFragment(config.DesignGateOff) + " " + tc.want; spec.Prompt.Addressing != want {
				t.Errorf("addressing = %q, want %q", spec.Prompt.Addressing, want)
			}
		})
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
	s := specs{stateDir: t.TempDir(), project: "s1", prompts: composer, repositories: noRepositoriesView,
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
			s := specs{stateDir: t.TempDir(), project: "s1", prompts: composer, runtimeName: tc.runtimeName, plugins: tc.plugins, repositories: noRepositoriesView}
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
	s := specs{stateDir: t.TempDir(), project: "s1", prompts: composer, runtimeName: "kubernetes", plugins: pair, repositories: noRepositoriesView}
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
