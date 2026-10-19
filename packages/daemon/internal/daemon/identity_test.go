package daemon

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A pane commits as its role's App, minted for the owner of the repository its issue works: the
// launch carries the six git identity variables every worker pane gets, nothing else of the Apps,
// and a launch whose identity cannot be resolved does not happen.
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
	var owners []string
	s := specs{stateDir: stateDir, project: "s1", repositories: noRepositoriesView,
		repository: func(context.Context, string) (ghrepo.Repository, string, error) {
			return ghrepo.MustParse("zeta/tools"), "main", nil
		},
		identity: func(_ context.Context, role claim.Role, owner string) (runtime.GitIdentity, error) {
			if role != claim.RoleImplementer {
				t.Errorf("identity asked for %s, want the claim's role", role)
			}
			owners = append(owners, owner)
			return bot, nil
		}}
	spec, err := s.SpawnSpec(context.Background(), c)
	if err != nil || spec.Repository != ghrepo.MustParse("zeta/tools") {
		t.Fatalf("SpawnSpec = %q, %v; want zeta/tools, the runtime's to locate the workspace from", spec.Repository, err)
	}
	if want := []string{"zeta"}; !slices.Equal(owners, want) {
		t.Errorf("identity asked for owners %q, want the recorded repository's owner alone, %q", owners, want)
	}
	identity := map[string]string{
		"JJ_USER": bot.Name, "JJ_EMAIL": bot.Email,
		"GIT_AUTHOR_NAME": bot.Name, "GIT_AUTHOR_EMAIL": bot.Email,
		"GIT_COMMITTER_NAME": bot.Name, "GIT_COMMITTER_EMAIL": bot.Email,
	}
	if !maps.Equal(spec.Env, identity) {
		t.Errorf("the launch's Env = %v, want the six git identity variables alone", spec.Env)
	}

	s.identity = func(context.Context, claim.Role, string) (runtime.GitIdentity, error) {
		return runtime.GitIdentity{}, errors.New("github_app_not_installed")
	}
	if _, err := s.SpawnSpec(context.Background(), c); err == nil || !strings.Contains(err.Error(), "github_app_not_installed") {
		t.Fatalf("SpawnSpec with no identity = %v, want the lease failure", err)
	}
}
