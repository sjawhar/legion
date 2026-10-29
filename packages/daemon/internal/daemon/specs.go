package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

var _ supervise.Specs = specs{}

// specs builds the part of every launch the claim does not carry. An operator may retain a
// one-off prompt under the state directory for the Stage 2 proof; otherwise the Go daemon
// composes the shipped role parts plus its own role-specific instructions. The repository is the
// one the claim's issue's record names, at the default branch the record names; a runtime that
// provisions workspaces itself (a Sandbox pod's init containers) provisions the issue's workspace
// from them, and under any other the daemon makes sure the workspace is on its own host first.
type specs struct {
	stateDir     string
	project      string
	instructions string
	secrets      map[string]string
	prompts      *prompts.Composer
	// designGate is the project's design gate policy (gates.design), which a tree's root architect
	// is told after its addressing (DesignGateFragment).
	designGate config.DesignGate
	// slackChannels is the project's slack_channels, which the controller is told after the design
	// gate policy (SlackChannelsFragment); its LEGION_SLACK_CHANNELS is the runtime's to set.
	slackChannels []string
	// mergeRules is the project's merge policy, which a merger is told after its addressing.
	mergeRules config.MergeRules
	// identity is the role's App bot identity every pane commits as, minted for the owner of the
	// repository the launch works; nil for a daemon with no GitHub Apps.
	identity func(ctx context.Context, role claim.Role, owner string) (runtime.GitIdentity, error)
	// repository is the repository an issue works and its default branch
	// (workflowRuntime.issueRepository: its record's, or the project's default for an issue the
	// workflow does not record); nil for a daemon with no workflow, whose launches work no
	// repository.
	repository func(ctx context.Context, issue string) (ghrepo.Repository, string, error)
	// repositories is the project's repository mapping as the daemon serves it (repositories.view),
	// in the daemon's own order: what a worker's addressing names beside its own issue's
	// repository and the controller's names after its design gate policy (RepositoriesClause). The
	// daemon sets it at boot whether or not a workflow is configured.
	repositories func() api.RepositoriesView
	// runtimeName is cfg.Runtime.Name: a stale-plugin install (stalePluginInstall) is Kubernetes'
	// alone, since a tmux pane always resumes on the plugin installed on the daemon's host.
	runtimeName string
	// plugins is this daemon's resolved plugin pair under Kubernetes — the file's
	// `runtime.kubernetes.plugins`, or the binary's linked pair when the file names none — with
	// Contract already set to api.DaemonAPIVersion; nil when the daemon knows none
	// (LEGION-164's degraded mode). Never read under any other runtime.
	plugins *runtime.PluginInstall
	// backend is the backend a role runs on (config.Config.BackendFor); nil runs every role on Oh
	// My Pi.
	backend func(claim.Role) runtime.Backend
	// hostWorkspaces is the runtime leaving each claim's workspace to the daemon's own host
	// (runtime.Runtime.ProvisionsWorkspaces false: tmux). ensureWorkspace then provisions an
	// issue's workspace a launch finds missing or not checked out there (outbox.ensureWorkspace,
	// through the workflow); nil for a daemon with no workflow, whose launches work no repository.
	hostWorkspaces  bool
	ensureWorkspace func(ctx context.Context, issue string, repo ghrepo.Repository, defaultBranch string) error
}

// rolePromptPath is where a claim's role prompt is kept for every launch of it.
func rolePromptPath(stateDir string, token claim.Token) string {
	return filepath.Join(stateDir, "prompts", string(token)+".md")
}

// SpawnSpec is the launch's secrets (launchSecrets: the Envoy bearer and the NATS nkey seed, each
// when the daemon has one), its prompt — the role prompt parts, the addressing sentence, and the
// deployment instructions — and its repository with that repository's default branch, whose
// workspace is on the daemon's host by then when the runtime leaves workspaces to the daemon
// (ensureWorkspace). The controller's launch (controllerSpawnSpec) is its own. Both name the
// backend the role runs on and are told its prompt part after the role's parts (launchBackend).
// Plugins carries the plugin pair a stale-plugin relaunch installs (stalePluginInstall), set the
// same way for both — the controller the daemon launches (`controller: daemon`) is covered like
// any claim.
func (s specs) SpawnSpec(ctx context.Context, c supervise.Claim) (runtime.SpawnSpec, error) {
	var spec runtime.SpawnSpec
	var err error
	if c.Role == claim.RoleController {
		spec, err = s.controllerSpawnSpec()
	} else {
		spec, err = s.workerSpawnSpec(ctx, c)
	}
	if err != nil {
		return runtime.SpawnSpec{}, err
	}
	var part string
	if spec.Backend, part, err = s.launchBackend(c.Role); err != nil {
		return runtime.SpawnSpec{}, err
	}
	if part != "" {
		spec.Prompt.RolePromptPaths = append(spec.Prompt.RolePromptPaths, part)
	}
	spec.Plugins = s.stalePluginInstall(c, spec.Backend)
	return spec, nil
}

// launchBackend is the backend role runs on (backend; Oh My Pi when nil) and the path of the
// daemon prompt part its entry names (runtime.Backends), "" when it names none.
func (s specs) launchBackend(role claim.Role) (runtime.Backend, string, error) {
	backend := runtime.BackendOmp
	if s.backend != nil {
		backend = s.backend(role)
	}
	info, err := runtime.LookupBackend(backend)
	if err != nil {
		return "", "", fmt.Errorf("the %s's backend: %w", role, err)
	}
	if info.PromptPart == "" {
		return backend, "", nil
	}
	if s.prompts == nil {
		return "", "", errors.New("the daemon prompt bundle was not constructed at boot")
	}
	return backend, s.prompts.Part(info.PromptPart), nil
}

// workerSpawnSpec is SpawnSpec's own build for every claim but the controller's.
func (s specs) workerSpawnSpec(ctx context.Context, c supervise.Claim) (runtime.SpawnSpec, error) {
	promptPaths, err := s.rolePromptPaths(c)
	if err != nil {
		return runtime.SpawnSpec{}, err
	}
	spec := runtime.SpawnSpec{
		Env:     map[string]string{},
		Secrets: maps.Clone(s.secrets),
	}
	if s.repository != nil {
		if spec.Repository, spec.DefaultBranch, err = s.repository(ctx, c.Issue); err != nil {
			return runtime.SpawnSpec{}, fmt.Errorf("the repository of %s: %w", c.Token, err)
		}
	}
	// Every launch, not only a claim's first: a claim resumed, relaunched or recovered on this host
	// may have no workspace here — its issue ran in another runtime's pods before, or its tree's
	// linger removed it — and a pane cannot start without one. Provisioning checks out the issue's
	// branch legion/<KEY> when GitHub has it, so whatever the claim pushed is there.
	if s.hostWorkspaces && s.ensureWorkspace != nil && !spec.Repository.IsZero() {
		if err := s.ensureWorkspace(ctx, c.Issue, spec.Repository, spec.DefaultBranch); err != nil {
			return runtime.SpawnSpec{}, fmt.Errorf("the workspace of %s: %w", c.Token, err)
		}
	}
	addressing, err := AddressingFragment(s.project, c, spec.Repository, spec.DefaultBranch, s.repositories())
	if err != nil {
		return runtime.SpawnSpec{}, err
	}
	if claim.IsTreeArchitect(c.Role, c.Issue, c.Tree) {
		addressing += " " + DesignGateFragment(s.designGate)
	}
	if c.Role == claim.RoleMerger {
		addressing += " " + MergeRulesFragment(s.mergeRules)
	}
	spec.Prompt = runtime.PromptParts{
		RolePromptPaths:            promptPaths,
		Addressing:                 addressing,
		DeploymentInstructionsPath: s.instructions,
	}
	if s.identity != nil {
		id, err := s.identity(ctx, c.Role, spec.Repository.Owner())
		if err != nil {
			return runtime.SpawnSpec{}, fmt.Errorf("the git identity of %s: %w", c.Token, err)
		}
		spec.Env = id.Env()
	}
	// After the git identity, which replaces spec.Env rather than merging into it: the stall
	// limits are merged into whatever map the launch holds by then (stallLimitVariables).
	for _, name := range stallLimitVariables {
		if value, ok := os.LookupEnv(name); ok {
			spec.Env[name] = value
		}
	}
	return spec, nil
}

// stallLimitVariables are the four numbers a phase worker's end-of-turn reminder runs on, in name
// order: how many delivered reminders in a row change nothing before the architect is told, how
// long the worker waits for a reminder after a settle, how long after a resume on an open phase,
// and how long a reply that said what it waits on buys. A worker told none of them runs on the
// defaults its plugin holds (packages/pi-shared/src/stall-limits.ts), so the daemon passes exactly
// the ones its own environment holds and no more. Both runtimes forward spec.Env verbatim — a
// pane's -e pairs (runtime/tmux's panePairs) and a pod launcher's start command (runtime/sandbox's
// mainEnvironment) — so workerSpawnSpec is the one seam that serves both.
var stallLimitVariables = []string{
	"LEGION_STALL_ARCHITECT_AFTER",
	"LEGION_STALL_REMINDER_MS",
	"LEGION_STALL_RESUME_MS",
	"LEGION_STALL_WAITING_MS",
}

// stalePluginInstall is spec.Plugins for c's launch (LEGION-164): the daemon's resolved pair,
// with Contract already api.DaemonAPIVersion, iff the launch runs on Oh My Pi, the runtime is
// Kubernetes, c has registered a session, and its recorded contract differs from this daemon's —
// never for a launch that is not replacing a process on another plugin contract, and never when
// the daemon knows no pair (s.plugins nil, the degraded mode). A fresh copy, so a caller mutating
// one launch's spec never reaches another's.
//
// backend is the launch's, as SpawnSpec resolved it (launchBackend). A Claude Code launch gets
// nothing: that install is `@sjawhar/pi-legion` and `@sjawhar/pi-envoy` from npm
// (runtime/sandbox's plugins.go), no workflow releases claude-envoy, and a Claude worker's plugin
// is the image's — so a plugin-contract change for such a role is answered by a new image, not by
// an install. Such a claim whose recorded contract differs still relaunches: it is the image's
// plugin that decides, and the image probe is what refuses a wrong one. The daemon-launched
// controller runs on Oh My Pi (runtime.Backends runs Claude Code for phase workers alone), so its
// launch is covered here exactly as before.
func (s specs) stalePluginInstall(c supervise.Claim, backend runtime.Backend) *runtime.PluginInstall {
	if runtime.Of(backend) == runtime.BackendClaude {
		return nil
	}
	if s.runtimeName != "kubernetes" || c.Session == "" || c.PluginContract == api.DaemonAPIVersion || s.plugins == nil {
		return nil
	}
	install := *s.plugins
	return &install
}

// controllerSpawnSpec is the daemon's controller's launch (`controller: daemon`): its role part and
// the headless part; as its addressing the project's design gate policy, then the project's
// repositories as a sentence of their own (RepositoriesSentence, which also says when the daemon
// maps none) and, when it names any, its Slack channels; then the deployment instructions and the
// launch secrets. The operator's controller (`legion controller start`) is told the policy and
// the repositories the same way, never channels, since only the daemon's may have channels
// (config.checkSlackChannels). The channels reach its environment as LEGION_SLACK_CHANNELS from the
// runtime (sandbox.Options.ControllerSlackChannels), which compares them with the list a running
// controller was launched under and relaunches it when they change; a spec cannot set that variable.
// It has no repository and no git identity: the controller works Dispatch, never a checkout, and
// commits nothing.
func (s specs) controllerSpawnSpec() (runtime.SpawnSpec, error) {
	if s.prompts == nil {
		return runtime.SpawnSpec{}, errors.New("the daemon prompt bundle was not constructed at boot")
	}
	paths, err := s.prompts.ControllerPromptPaths(true)
	if err != nil {
		return runtime.SpawnSpec{}, err
	}
	addressing := DesignGateFragment(s.designGate) + " " + RepositoriesSentence(s.repositories())
	if len(s.slackChannels) > 0 {
		addressing += " " + SlackChannelsFragment(s.slackChannels)
	}
	return runtime.SpawnSpec{
		Env:     map[string]string{},
		Secrets: maps.Clone(s.secrets),
		Prompt: runtime.PromptParts{
			RolePromptPaths:            paths,
			Addressing:                 addressing,
			DeploymentInstructionsPath: s.instructions,
		},
	}, nil
}

// rolePromptPaths keeps an explicit operator prompt as a narrow test override. Every ordinary
// claim uses the role prompts daemon boot snapshotted below its state directory (prompts.New).
func (s specs) rolePromptPaths(c supervise.Claim) ([]string, error) {
	override := rolePromptPath(s.stateDir, c.Token)
	if _, err := os.Stat(override); err == nil {
		return []string{override}, nil
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("the role prompt of %s: %w", c.Token, err)
	}
	if s.prompts == nil {
		return nil, errors.New("the daemon prompt bundle was not constructed at boot")
	}
	parts, err := s.prompts.Compose(c.Role, claim.IsTreeRoot(c.Issue, c.Tree))
	if err != nil {
		return nil, err
	}
	return parts.RolePromptPaths, nil
}

// AddressingFragment is the sentence that tells an agent where it and its peers are reached: its
// own role topic, the tree architect's, and the project controller's, spelled from the tokens so
// the model never hand-encodes one; then, when repo is not the zero repository, the issue's own
// repository and default branch and the project's repositories (RepositoriesClause) — left off
// entirely for a daemon that runs no workflow, whose claims name no repository. The clause names
// the project's mapping as it is now, which may hold none: a recorded issue keeps its repository
// after a refresh unmaps or refuses it. The daemon posts every READY packet and publishes it to
// `projects.<KEY>.merge_queue_role` itself (workflow.Engine.ready). It is exported for the rigs
// under packages/pi-legion/scripts, which tell a worker what a pane is told.
func AddressingFragment(project string, c supervise.Claim, repo ghrepo.Repository, defaultBranch string, repositories api.RepositoriesView) (string, error) {
	architect, err := claim.NewToken(project, c.Tree, claim.RoleArchitect)
	if err != nil {
		return "", fmt.Errorf("the addressing of %s: %w", c.Token, err)
	}
	fragment := fmt.Sprintf("Legion addressing: your role topic is `%s%s`; the architect that owns your issue is `%s%s`; "+
		"the project's controller is `%s%s`; a sibling role on your issue is your topic with the trailing `-<role>` replaced.",
		notify.RoleTopicPrefix, c.Token, notify.RoleTopicPrefix, architect, notify.RoleTopicPrefix, claim.ControllerToken(project))
	if repo.IsZero() {
		return fragment, nil
	}
	return fragment + " " + fmt.Sprintf("Your issue's repository is %s (default branch %s); %s",
		repo.String(), defaultBranch, RepositoriesClause(repositories)), nil
}

// RepositoriesClause tells an agent which repositories the project maps, as a clause that ends in
// a full stop and starts in lower case, to follow a worker's `; `: "the project's repositories are
// a, b, c." in the order the daemon serves them (repositories.Mapped sorts them by name, whatever
// the case), or, when the daemon maps none, "the daemon maps no repository to this project now."
// with each repository it leaves out and why. An empty mapping is a state a running project
// reaches (an App uninstalled, a refresh that unmapped every repository), never a refusal to
// launch, so every launch path names it.
func RepositoriesClause(view api.RepositoriesView) string {
	if len(view.Mapped) > 0 {
		names := make([]string, len(view.Mapped))
		for i, mapped := range view.Mapped {
			names[i] = mapped.Repo
		}
		return "the project's repositories are " + strings.Join(names, ", ") + "."
	}
	if len(view.Refused) == 0 {
		return "the daemon maps no repository to this project now."
	}
	refused := make([]string, len(view.Refused))
	for i, r := range view.Refused {
		refused[i] = fmt.Sprintf("%s (%s)", r.Repo, r.Reason)
	}
	return "the daemon maps no repository to this project now: " + strings.Join(refused, "; ") + "."
}

// RepositoriesSentence is RepositoriesClause as a sentence of its own, for the controller's
// addressing, which follows the design gate policy's sentence.
func RepositoriesSentence(view api.RepositoriesView) string {
	clause := RepositoriesClause(view)
	return strings.ToUpper(clause[:1]) + clause[1:]
}

// DesignGateFragment is the sentence a tree's root architect is told after its addressing, and the
// controller as its launch addressing: this project's design gate policy, the "Design gate policy"
// line the shared role prompts read. What each policy asks of the architect is in its Go role part
// (prompts/go/architect-root.md).
func DesignGateFragment(policy config.DesignGate) string {
	return fmt.Sprintf("Design gate policy: `gates.design: %s`.", policy)
}

// SlackChannelsFragment names the project's Slack channels, the list the controller answers
// mentions from: its plugin subscribes to each one's mention topic itself, while the session holds
// the controller role, so the model subscribes to nothing. What the controller does with a mention,
// and with a delivery from a channel the list does not name, is the skill's.
func SlackChannelsFragment(channels []string) string {
	named := make([]string, 0, len(channels))
	for _, channel := range channels {
		named = append(named, "`"+channel+"`")
	}
	return "Slack channels: " + strings.Join(named, ", ") +
		": your session hears each one's app mentions while it holds the controller role; answer each in its thread, " +
		"and nothing from any other channel (skill://legion-controller/references/slack-mentions.md)."
}

// MergeRulesFragment is the merger's project policy. The daemon checks these rules again at READY.
func MergeRulesFragment(rules config.MergeRules) string {
	render := func(patterns []config.PathPattern) string {
		expressions := make([]string, 0, len(patterns))
		for _, pattern := range patterns {
			expressions = append(expressions, "`"+pattern.Expression+"`")
		}
		if len(expressions) == 0 {
			return "none"
		}
		return strings.Join(expressions, ", ")
	}
	return "Merge policy: by_hand " + render(rules.ByHand) + "."
}
