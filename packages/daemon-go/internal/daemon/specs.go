package daemon

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// roleTopicPrefix is the Envoy subject a role token is reached on
// (packages/contracts/src/subject.ts:2).
const roleTopicPrefix = "notifications.role."

var _ supervise.Specs = specs{}

// specs builds the part of every launch the claim does not carry. An operator may retain a
// one-off prompt under the state directory for the Stage 2 proof; otherwise the Go daemon
// composes the shipped role parts plus its own role-specific instructions. The workspace is the
// issue's directory there.
type specs struct {
	stateDir     string
	project      string
	instructions string
	secrets      map[string]string
}

// rolePromptPath is where a claim's role prompt is kept for every launch of it.
func rolePromptPath(stateDir string, token claim.Token) string {
	return filepath.Join(stateDir, "prompts", string(token)+".md")
}

// SpawnSpec is the launch's secrets (the Envoy bearer, when the daemon has one), its prompt — the
// role prompt parts, the addressing sentence, and the deployment instructions — and its workspace.
// The workspace is `<state_dir>/workspaces/<issue>`, made if it is not there: the issue's working
// copy is Stage 3's to provision, and until then an agent's cwd is a directory of its issue's own.
func (s specs) SpawnSpec(_ context.Context, c supervise.Claim) (runtime.SpawnSpec, error) {
	promptPaths, err := s.rolePromptPaths(c)
	if err != nil {
		return runtime.SpawnSpec{}, err
	}
	addressing, err := addressingFragment(s.project, c)
	if err != nil {
		return runtime.SpawnSpec{}, err
	}
	workspace := filepath.Join(s.stateDir, "workspaces", c.Issue)
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return runtime.SpawnSpec{}, fmt.Errorf("the workspace of %s: %w", c.Token, err)
	}
	return runtime.SpawnSpec{
		Secrets: maps.Clone(s.secrets),
		Prompt: runtime.PromptParts{
			RolePromptPaths:            promptPaths,
			Addressing:                 addressing,
			DeploymentInstructionsPath: s.instructions,
		},
		Workspace: workspace,
	}, nil
}

// rolePromptPaths keeps an explicit operator prompt as a narrow test override. Every ordinary
// claim has no such file, so it gets the shared role sequence followed by the generated Go part.
// Task 3.12 constructs prompts.New at boot; Stage 2 creates it here so this path remains runnable
// before that wiring lands.
func (s specs) rolePromptPaths(c supervise.Claim) ([]string, error) {
	override := rolePromptPath(s.stateDir, c.Token)
	if _, err := os.Stat(override); err == nil {
		return []string{override}, nil
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("the role prompt of %s: %w", c.Token, err)
	}

	rolesDir, err := prompts.ResolveRolePromptsDir(os.LookupEnv)
	if err != nil {
		return nil, fmt.Errorf("resolve role prompts for %s: %w", c.Token, err)
	}
	composer, err := prompts.New(rolesDir, s.stateDir)
	if err != nil {
		return nil, fmt.Errorf("compose role prompt for %s: %w", c.Token, err)
	}
	parts, err := composer.Compose(c.Role, c.Issue == c.Tree)
	if err != nil {
		return nil, err
	}
	return parts.RolePromptPaths, nil
}

// addressingFragment is the sentence that tells an agent where it and its peers are reached: its
// own role topic, the tree architect's, and the project controller's, spelled from the tokens so
// the model never hand-encodes one (packages/daemon/src/daemon/processes.ts:317-343). The shipped
// sentence's merge-queue clause is left out: the merge queue is `projects.<KEY>.merge_queue_role`,
// which this daemon does not read until Stage 3, and a clause saying there is none would be a claim
// about a setting the daemon never looked at.
func addressingFragment(project string, c supervise.Claim) (string, error) {
	architect, err := claim.NewToken(project, c.Tree, claim.RoleArchitect)
	if err != nil {
		return "", fmt.Errorf("the addressing of %s: %w", c.Token, err)
	}
	return fmt.Sprintf("Legion addressing: your role topic is `%s%s`; the architect that owns your issue is `%s%s`; "+
		"the project's controller is `%s%s`; a sibling role on your issue is your topic with the trailing `-<role>` replaced.",
		roleTopicPrefix, c.Token, roleTopicPrefix, architect, roleTopicPrefix, claim.ControllerToken(project)), nil
}
