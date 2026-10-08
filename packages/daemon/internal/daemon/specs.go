package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"

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
// configured project's; the runtime locates the issue's workspace from it.
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
	// slack is the deployment's `slack` block, nil when it sets none, which the controller is told
	// after its design gate policy (SlackFragment).
	slack *config.Slack
	// reviewWorkflows is the project's review_workflows, which a reviewer is told after its
	// addressing (ReviewWorkflowsFragment).
	reviewWorkflows []string
	// identity is the role's App bot identity every pane commits as; nil for a daemon with no
	// GitHub Apps.
	identity func(ctx context.Context, role claim.Role) (runtime.GitIdentity, error)
}

// rolePromptPath is where a claim's role prompt is kept for every launch of it.
func rolePromptPath(stateDir string, token claim.Token) string {
	return filepath.Join(stateDir, "prompts", string(token)+".md")
}

// SpawnSpec is the launch's secrets (launchSecrets: the Envoy bearer and the NATS nkey seed, each
// when the daemon has one), its prompt — the role prompt parts, the addressing sentence, and the
// deployment instructions — and its repository; for a claim whose workspace was lost with its
// session, the issue's branch the recreated workspace is recovered from. The controller's launch
// (controllerSpawnSpec) is its own.
func (s specs) SpawnSpec(ctx context.Context, c supervise.Claim) (runtime.SpawnSpec, error) {
	if c.Role == claim.RoleController {
		return s.controllerSpawnSpec()
	}
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
	if c.Role == claim.RoleReviewer {
		addressing += " " + ReviewWorkflowsFragment(s.reviewWorkflows)
	}
	env := map[string]string{}
	if value := SlackReportingChannelsEnvValue(s.slack); value != "" {
		env[slackReportingChannelsEnv] = value
	}
	if s.identity != nil {
		id, err := s.identity(ctx, c.Role)
		if err != nil {
			return runtime.SpawnSpec{}, fmt.Errorf("the git identity of %s: %w", c.Token, err)
		}
		maps.Copy(env, id.Env())
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
	if c.WorkspaceLost {
		spec.WorkspaceRecoveredFrom = workspace.Bookmark(c.Issue)
	}
	return spec, nil
}

// controllerSpawnSpec is the daemon's controller's launch (`controller: daemon`): its role part and
// the headless part, the project's design gate policy and its Slack reporting channels as its
// addressing — what `legion controller start` tells the operator's controller — the deployment
// instructions and the launch secrets. It has no repository and no git identity: the controller
// works Dispatch, never a checkout, and commits nothing.
func (s specs) controllerSpawnSpec() (runtime.SpawnSpec, error) {
	if s.prompts == nil {
		return runtime.SpawnSpec{}, errors.New("the daemon prompt bundle was not constructed at boot")
	}
	paths, err := s.prompts.ControllerPromptPaths(true)
	if err != nil {
		return runtime.SpawnSpec{}, err
	}
	env := map[string]string{}
	if value := SlackReportingChannelsEnvValue(s.slack); value != "" {
		env[slackReportingChannelsEnv] = value
	}
	return runtime.SpawnSpec{
		Env:     env,
		Secrets: maps.Clone(s.secrets),
		Prompt: runtime.PromptParts{
			RolePromptPaths:            paths,
			Addressing:                 ControllerAddressing(s.designGate, s.slack),
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
// the model never hand-encodes one. It names no merge queue: the merger publishes nothing — the
// daemon posts the READY packet and publishes it to `projects.<KEY>.merge_queue_role` itself
// (workflow.Engine.ready, prompts/go/merger.md). It is exported for the rigs under
// packages/pi-legion/scripts, which tell a worker what a pane is told.
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
// operator's controller as its launch addressing (`legion controller start`): this project's design
// gate policy, the "Design gate policy" line the shared role prompts read. What each policy asks of
// the architect is in its Go role part (prompts/go/architect-root.md).
func DesignGateFragment(policy config.DesignGate) string {
	return fmt.Sprintf("Design gate policy: `gates.design: %s`.", policy)
}

// ControllerAddressing is the controller's launch addressing under either launcher: the design gate
// policy, then, when the deployment takes Slack reports, its reporting channels (SlackFragment).
func ControllerAddressing(policy config.DesignGate, slack *config.Slack) string {
	if slack == nil {
		return DesignGateFragment(policy)
	}
	return DesignGateFragment(policy) + " " + SlackFragment(*slack)
}

// SlackFragment is the sentence the controller is told after its design gate policy when the
// deployment sets `slack`: the "Slack reporting channels" line its skill reads
// (skill://legion-controller, "Reports from Slack"), naming each channel's mention topic, which the
// controller subscribes to, and the project a report there is filed in. The topic is spelled here
// so the model never hand-builds one.
func SlackFragment(slack config.Slack) string {
	channels := make([]string, 0, len(slack.ReportingChannels))
	for _, c := range slack.ReportingChannels {
		channels = append(channels, fmt.Sprintf("`notifications.slack.%s.%s.mention` (files in %s)", slack.Team, c.Channel, c.Project))
	}
	return fmt.Sprintf("Slack reporting channels: team `%s`; %s.", slack.Team, strings.Join(channels, ", "))
}

// slackReportingChannelsEnv is LEGION_SLACK_REPORTING_CHANNELS (the literal is duplicated in
// packages/daemon/cmd/legion/slack.go, which reads it; the two must agree): the comma-separated
// channel IDs legion slack post and reply may address, carried to every launch this deployment
// configures Slack reporting channels for. A report thread's text reaches a session as untrusted
// data; keeping this allowlist in the launch's Env, rather than only in the controller's free-text
// addressing, lets `legion slack` enforce it in code instead of trusting a prompt.
const slackReportingChannelsEnv = "LEGION_SLACK_REPORTING_CHANNELS"

// SlackReportingChannelsEnvValue is slackReportingChannelsEnv's value for slack: "" when slack is
// nil or names no channels, so SpawnSpec, controllerSpawnSpec and `legion controller start`
// (packages/daemon/cmd/legion/controller.go) set no Env entry for it then. Exported so the
// operator-launched controller's path can carry the same allowlist into its own Env, which the
// daemon-launched controller gets from controllerSpawnSpec above.
func SlackReportingChannelsEnvValue(slack *config.Slack) string {
	if slack == nil {
		return ""
	}
	channels := make([]string, len(slack.ReportingChannels))
	for i, c := range slack.ReportingChannels {
		channels[i] = c.Channel
	}
	return strings.Join(channels, ",")
}

// ReviewWorkflowsFragment is the sentence a reviewer is told after its addressing: the required
// workflows this project declares as review workflows (`projects.<KEY>.review_workflows`), the
// "Review workflows" line its Go role part reads (prompts/go/reviewer.md). A red only they make is
// the reviewer's round's to adjudicate; any other red required workflow is a failing check.
func ReviewWorkflowsFragment(workflows []string) string {
	if len(workflows) == 0 {
		return "Review workflows: this project declares none (`review_workflows`)."
	}
	return "Review workflows: this project declares `" + strings.Join(workflows, "`, `") + "` (`review_workflows`)."
}
