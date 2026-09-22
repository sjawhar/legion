package daemon

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// roleTopicPrefix is the Envoy subject a role token is reached on
// (packages/contracts/src/subject.ts:2).
const roleTopicPrefix = "notifications.role."

var _ supervise.Specs = specs{}

// specs builds the part of every launch the claim does not carry. It reads nothing but the claim
// and the state directory, because a relaunch after a restart has nothing else to read: the role
// prompt is the file the spawn wrote under the state directory, and the workspace is the issue's
// directory there.
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
// role prompt, the addressing sentence, and the deployment instructions — and its workspace. The
// workspace is `<state_dir>/workspaces/<issue>`, made if it is not there: the issue's working copy
// is Stage 3's to provision, and until then an agent's cwd is a directory of its issue's own.
func (s specs) SpawnSpec(_ context.Context, c supervise.Claim) (runtime.SpawnSpec, error) {
	prompt := rolePromptPath(s.stateDir, c.Token)
	if _, err := os.Stat(prompt); err != nil {
		return runtime.SpawnSpec{}, fmt.Errorf("the role prompt of %s: %w", c.Token, err)
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
			RolePromptPaths:            []string{prompt},
			Addressing:                 addressing,
			DeploymentInstructionsPath: s.instructions,
		},
		Workspace: workspace,
	}, nil
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
