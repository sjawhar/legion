package daemon

import (
	"context"
	"errors"
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
// composes the shipped role parts plus its own role-specific instructions. The repository is the
// configured project's; the runtime locates the issue's workspace from it.
type specs struct {
	stateDir     string
	project      string
	instructions string
	secrets      map[string]string
	repo         string
	prompts      *prompts.Composer
	// identity is the role's App bot identity every pane commits as; nil for a daemon with no
	// GitHub Apps.
	identity func(ctx context.Context, role claim.Role) (runtime.GitIdentity, error)
}

// rolePromptPath is where a claim's role prompt is kept for every launch of it.
func rolePromptPath(stateDir string, token claim.Token) string {
	return filepath.Join(stateDir, "prompts", string(token)+".md")
}

// SpawnSpec is the launch's secrets (the Envoy bearer, when the daemon has one), its prompt — the
// role prompt parts, the addressing sentence, and the deployment instructions — and its
// repository.
func (s specs) SpawnSpec(ctx context.Context, c supervise.Claim) (runtime.SpawnSpec, error) {
	promptPaths, err := s.rolePromptPaths(c)
	if err != nil {
		return runtime.SpawnSpec{}, err
	}
	addressing, err := addressingFragment(s.project, c)
	if err != nil {
		return runtime.SpawnSpec{}, err
	}
	env := map[string]string{}
	if s.identity != nil {
		id, err := s.identity(ctx, c.Role)
		if err != nil {
			return runtime.SpawnSpec{}, fmt.Errorf("the git identity of %s: %w", c.Token, err)
		}
		env = gitIdentityEnv(id)
	}
	return runtime.SpawnSpec{
		Env:     env,
		Secrets: maps.Clone(s.secrets),
		Prompt: runtime.PromptParts{
			RolePromptPaths:            promptPaths,
			Addressing:                 addressing,
			DeploymentInstructionsPath: s.instructions,
		},
		Repository: s.repo,
	}, nil
}

// gitIdentityEnv is the six variables that make a process commit as id: JJ_USER/JJ_EMAIL, which
// jj reads over every config scope, and the Git author and committer pairs for plain git
// (packages/daemon/src/daemon/github-app-env.ts:46-58).
func gitIdentityEnv(id runtime.GitIdentity) map[string]string {
	return map[string]string{
		"JJ_USER": id.Name, "JJ_EMAIL": id.Email,
		"GIT_AUTHOR_NAME": id.Name, "GIT_AUTHOR_EMAIL": id.Email,
		"GIT_COMMITTER_NAME": id.Name, "GIT_COMMITTER_EMAIL": id.Email,
	}
}

// rolePromptPaths keeps an explicit operator prompt as a narrow test override. Every ordinary
// claim uses the prompt bundle daemon boot already validated and materialized.
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
