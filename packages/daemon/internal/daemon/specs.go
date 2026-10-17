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
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

var _ supervise.Specs = specs{}

// specs builds the part of every launch the claim does not carry. An operator may retain a
// one-off prompt under the state directory for the Stage 2 proof; otherwise the Go daemon
// composes the shipped role parts plus its own role-specific instructions. The repository is the
// configured project's, with its default branch as GitHub names it; the runtime locates the
// issue's workspace from the repository and provisions it at the default branch.
type specs struct {
	stateDir     string
	project      string
	instructions string
	secrets      map[string]string
	repo         ghrepo.Repository
	prompts      *prompts.Composer
	// designGate is the project's design gate policy (gates.design), which a tree's root architect
	// is told after its addressing (DesignGateFragment).
	designGate config.DesignGate
	// slackChannels is the project's slack_channels, which the controller is told after the design
	// gate policy (SlackChannelsFragment); its LEGION_SLACK_CHANNELS is the runtime's to set.
	slackChannels []string
	// mergeRules is the project's merge policy, which a merger is told after its addressing.
	mergeRules config.MergeRules
	// identity is the role's App bot identity every pane commits as; nil for a daemon with no
	// GitHub Apps.
	identity func(ctx context.Context, role claim.Role) (runtime.GitIdentity, error)
	// defaultBranch is a repository's default branch as GitHub names it (defaultBranches.Of); nil
	// for a daemon with no GitHub Apps, which can launch no claim on a repository.
	defaultBranch func(ctx context.Context, repo ghrepo.Repository) (string, error)
	// runtimeName is cfg.Runtime.Name: a stale-plugin install (stalePluginInstall) is Kubernetes'
	// alone, since a tmux pane always resumes on the plugin installed on the daemon's host.
	runtimeName string
	// plugins is this daemon's resolved plugin pair under Kubernetes — the file's
	// `runtime.kubernetes.plugins`, or the binary's linked pair when the file names none — with
	// Contract already set to api.DaemonAPIVersion; nil when the daemon knows none
	// (LEGION-164's degraded mode). Never read under any other runtime.
	plugins *runtime.PluginInstall
}

// rolePromptPath is where a claim's role prompt is kept for every launch of it.
func rolePromptPath(stateDir string, token claim.Token) string {
	return filepath.Join(stateDir, "prompts", string(token)+".md")
}

// SpawnSpec is the launch's secrets (launchSecrets: the Envoy bearer and the NATS nkey seed, each
// when the daemon has one), its prompt — the role prompt parts, the addressing sentence, and the
// deployment instructions — and its repository with that repository's default branch; for a claim
// whose workspace was lost with its session, the issue's branch the recreated workspace is
// recovered from. The controller's launch (controllerSpawnSpec) is its own. Plugins carries the
// plugin pair a stale-plugin relaunch installs (stalePluginInstall), set the same way for both —
// the controller the daemon launches (`controller: daemon`) is covered like any claim.
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
	spec.Plugins = s.stalePluginInstall(c)
	return spec, nil
}

// workerSpawnSpec is SpawnSpec's own build for every claim but the controller's.
func (s specs) workerSpawnSpec(ctx context.Context, c supervise.Claim) (runtime.SpawnSpec, error) {
	promptPaths, err := s.rolePromptPaths(c)
	if err != nil {
		return runtime.SpawnSpec{}, err
	}
	addressing, err := AddressingFragment(s.project, c)
	if err != nil {
		return runtime.SpawnSpec{}, err
	}
	if claim.IsTreeArchitect(c.Role, c.Issue, c.Tree) {
		addressing += " " + DesignGateFragment(s.designGate)
	}
	if c.Role == claim.RoleMerger {
		addressing += " " + MergeRulesFragment(s.mergeRules)
	}
	env := map[string]string{}
	if s.identity != nil {
		id, err := s.identity(ctx, c.Role)
		if err != nil {
			return runtime.SpawnSpec{}, fmt.Errorf("the git identity of %s: %w", c.Token, err)
		}
		env = id.Env()
	}
	spec := runtime.SpawnSpec{
		Env:     env,
		Secrets: maps.Clone(s.secrets),
		Prompt: runtime.PromptParts{
			RolePromptPaths:            promptPaths,
			Addressing:                 addressing,
			DeploymentInstructionsPath: s.instructions,
		},
		Repository: s.repo,
	}
	if !s.repo.IsZero() {
		if s.defaultBranch == nil {
			return runtime.SpawnSpec{}, fmt.Errorf("the default branch of %s for %s: the daemon reads it from GitHub, and has no GitHub Apps", s.repo, c.Token)
		}
		if spec.DefaultBranch, err = s.defaultBranch(ctx, s.repo); err != nil {
			return runtime.SpawnSpec{}, fmt.Errorf("the default branch of %s for %s: %w", s.repo, c.Token, err)
		}
	}
	if c.WorkspaceLost {
		spec.WorkspaceRecoveredFrom = workspace.Bookmark(c.Issue)
	}
	return spec, nil
}

// stalePluginInstall is spec.Plugins for c's launch (LEGION-164): the daemon's resolved pair,
// with Contract already api.DaemonAPIVersion, iff the runtime is Kubernetes, c has registered a
// session, and its recorded contract differs from this daemon's — never for a launch that is not
// replacing a process on another plugin contract, and never when the daemon knows no pair
// (s.plugins nil, the degraded mode). A fresh copy, so a caller mutating one launch's spec never
// reaches another's.
func (s specs) stalePluginInstall(c supervise.Claim) *runtime.PluginInstall {
	if s.runtimeName != "kubernetes" || c.Session == "" || c.PluginContract == api.DaemonAPIVersion || s.plugins == nil {
		return nil
	}
	install := *s.plugins
	return &install
}

// controllerSpawnSpec is the daemon's controller's launch (`controller: daemon`): its role part and
// the headless part, the project's design gate policy and, when it names any, its Slack channels
// as its addressing, the deployment instructions and the launch secrets. The operator's controller
// is told the policy alone (`legion controller start`), since only the daemon's may have channels
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
	addressing := DesignGateFragment(s.designGate)
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
// the model never hand-encodes one. The daemon posts every READY packet and publishes it to
// `projects.<KEY>.merge_queue_role` itself (workflow.Engine.ready). It is exported for the rigs
// under packages/pi-legion/scripts, which tell a worker what a pane is told.
func AddressingFragment(project string, c supervise.Claim) (string, error) {
	architect, err := claim.NewToken(project, c.Tree, claim.RoleArchitect)
	if err != nil {
		return "", fmt.Errorf("the addressing of %s: %w", c.Token, err)
	}
	return fmt.Sprintf("Legion addressing: your role topic is `%s%s`; the architect that owns your issue is `%s%s`; "+
		"the project's controller is `%s%s`; a sibling role on your issue is your topic with the trailing `-<role>` replaced.",
		notify.RoleTopicPrefix, c.Token, notify.RoleTopicPrefix, architect, notify.RoleTopicPrefix, claim.ControllerToken(project)), nil
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
