package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A pane commits as its role's App: the launch carries the six variables the shipped daemon sets
// on every worker pane (packages/daemon/src/daemon/github-app-env.ts:46-58), and a launch whose
// identity cannot be resolved does not happen.
func TestSpawnSpecCarriesTheRoleAppIdentity(t *testing.T) {
	stateDir := t.TempDir()
	token, err := claim.NewToken("s1", "S1-1", claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	c := supervise.Claim{Token: token, Issue: "S1-1", Tree: "S1-1", Role: claim.RoleImplementer}
	if err := os.MkdirAll(filepath.Join(stateDir, "prompts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rolePromptPath(stateDir, token), []byte("implement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bot := runtime.GitIdentity{Name: "legion-implementer[bot]", Email: "7+legion-implementer[bot]@users.noreply.github.com"}
	s := specs{stateDir: stateDir, project: "s1", identity: func(_ context.Context, role claim.Role) (runtime.GitIdentity, error) {
		if role != claim.RoleImplementer {
			t.Errorf("identity asked for %s, want the claim's role", role)
		}
		return bot, nil
	}}
	spec, err := s.SpawnSpec(context.Background(), c)
	if err != nil {
		t.Fatalf("SpawnSpec: %v", err)
	}
	for name, want := range map[string]string{
		"JJ_USER": bot.Name, "JJ_EMAIL": bot.Email,
		"GIT_AUTHOR_NAME": bot.Name, "GIT_AUTHOR_EMAIL": bot.Email,
		"GIT_COMMITTER_NAME": bot.Name, "GIT_COMMITTER_EMAIL": bot.Email,
	} {
		if spec.Env[name] != want {
			t.Errorf("the launch's %s = %q, want %q", name, spec.Env[name], want)
		}
	}

	s.identity = func(context.Context, claim.Role) (runtime.GitIdentity, error) {
		return runtime.GitIdentity{}, errors.New("github_app_not_installed")
	}
	if _, err := s.SpawnSpec(context.Background(), c); err == nil || !strings.Contains(err.Error(), "github_app_not_installed") {
		t.Fatalf("SpawnSpec with no identity = %v, want the lease failure", err)
	}
}
