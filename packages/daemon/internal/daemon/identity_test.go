package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A pane commits as its role's App: the launch carries the six git identity variables every worker
// pane gets, and a launch whose identity cannot be resolved does not happen. Beside them, a daemon
// with GitHub Apps tells every tree role both Apps' bot logins as LEGION_IMPLEMENT_APP_LOGIN and
// LEGION_REVIEW_APP_LOGIN, plain values `legion threads resolve` builds its bot-thread rule from; a
// daemon without Apps sets neither, and the rule then has no Legion App to spare.
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
	if spec.Repository != (ghrepo.Repository{}) {
		t.Errorf("the launch names repository %q with none configured", spec.Repository)
	}
	s.repo = ghrepo.MustParse("acme/widgets")
	if spec, err := s.SpawnSpec(context.Background(), c); err != nil || spec.Repository != (ghrepo.MustParse("acme/widgets")) {
		t.Errorf("SpawnSpec with a configured repository = %q, %v; want acme/widgets, the runtime's to locate the workspace from", spec.Repository, err)
	}
	s.repo = ghrepo.Repository{}
	for name, want := range map[string]string{
		"JJ_USER": bot.Name, "JJ_EMAIL": bot.Email,
		"GIT_AUTHOR_NAME": bot.Name, "GIT_AUTHOR_EMAIL": bot.Email,
		"GIT_COMMITTER_NAME": bot.Name, "GIT_COMMITTER_EMAIL": bot.Email,
	} {
		if spec.Env[name] != want {
			t.Errorf("the launch's %s = %q, want %q", name, spec.Env[name], want)
		}
	}
	for _, name := range appauth.LoginEnv {
		if value, set := spec.Env[name]; set {
			t.Errorf("a daemon with no GitHub Apps told the launch %s=%q", name, value)
		}
	}
	s.appLogins = map[appauth.AppRole]string{appauth.Implement: "legion-implementer[bot]", appauth.Review: "legion-reviewer[bot]"}
	withApps, err := s.SpawnSpec(context.Background(), c)
	if err != nil {
		t.Fatalf("SpawnSpec with App logins: %v", err)
	}
	for name, want := range map[string]string{"LEGION_IMPLEMENT_APP_LOGIN": "legion-implementer[bot]", "LEGION_REVIEW_APP_LOGIN": "legion-reviewer[bot]"} {
		if withApps.Env[name] != want {
			t.Errorf("the launch's %s = %q, want %q", name, withApps.Env[name], want)
		}
	}
	if withApps.Env["JJ_USER"] != bot.Name {
		t.Errorf("the launch with App logins lost its git identity: JJ_USER = %q", withApps.Env["JJ_USER"])
	}
	s.appLogins = nil

	s.identity = func(context.Context, claim.Role) (runtime.GitIdentity, error) {
		return runtime.GitIdentity{}, errors.New("github_app_not_installed")
	}
	if _, err := s.SpawnSpec(context.Background(), c); err == nil || !strings.Contains(err.Error(), "github_app_not_installed") {
		t.Fatalf("SpawnSpec with no identity = %v, want the lease failure", err)
	}
}
