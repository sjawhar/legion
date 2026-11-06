package fixtureenv_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
)

func TestMain(m *testing.M) {
	if err := fixtureenv.Guard(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// The package's own TestMain ran Guard, as every package whose tests run git or jj does.
func TestTheTestProcessIsGuarded(t *testing.T) {
	fixtureenv.RequireGuarded(t)
}

// Guard takes out what a Legion issue pod hands every process — its workspace, git configuration,
// jj identity, GitHub token and SSH agent — and keeps the tests' own inputs and the names a package
// keeps. A command started with no Env of its own then inherits none of them, and is held to the
// file transport.
func TestGuardTakesOutWhatAPodSetsAndKeepsTheTestsInputs(t *testing.T) {
	t.Cleanup(func() {
		if err := fixtureenv.Guard(); err != nil {
			t.Error(err)
		}
	})
	pod := map[string]string{
		"LEGION_WORKSPACE": "/legion/workspaces/acme/widgets/widgets-42", "GIT_DIR": "/legion/repos/x/.git",
		"GIT_CONFIG_COUNT": "1", "JJ_USER": "a role", "GH_TOKEN": "a pod's token", "GITHUB_TOKEN": "a pod's token",
		"SSH_AUTH_SOCK":            "/run/agent.sock",
		"OMP_SESSION_SQL_DSN_FILE": "/legion/secrets/session-dsn", "OMP_SESSION_STORAGE": "sql",
		"OMP_SESSION_SQL_DSN": "postgres://legion@postgres.legion:5432/sessions",
	}
	inputs := map[string]string{
		"LEGION_TEST_PG_DSN": "postgres://test", "LEGION_E2E_REPO": "acme/widgets", "GITHUB_ACTIONS": "true",
		"LEGION_KEPT_BY_THE_PACKAGE": "1",
	}
	for name, value := range pod {
		t.Setenv(name, value)
	}
	for name, value := range inputs {
		t.Setenv(name, value)
	}
	t.Setenv("GIT_ALLOW_PROTOCOL", "https")
	if err := fixtureenv.Guard("LEGION_KEPT_BY_THE_PACKAGE"); err != nil {
		t.Fatal(err)
	}
	child, err := exec.Command("env").Output()
	if err != nil {
		t.Fatal(err)
	}
	inherited := strings.Split(string(child), "\n")
	for name := range pod {
		if _, set := os.LookupEnv(name); set {
			t.Errorf("the process still holds %s after Guard", name)
		}
		if slices.ContainsFunc(inherited, func(line string) bool { return strings.HasPrefix(line, name+"=") }) {
			t.Errorf("a command with no Env inherited %s", name)
		}
	}
	for name, value := range inputs {
		if got := os.Getenv(name); got != value {
			t.Errorf("%s=%q after Guard, want the test input %q kept", name, got, value)
		}
	}
	if !slices.Contains(inherited, "GIT_ALLOW_PROTOCOL=file") {
		t.Errorf("a command with no Env inherited no GIT_ALLOW_PROTOCOL=file:\n%s", child)
	}
}

// The three names this package takes out are the names that place a session — the two Oh My Pi
// reads, OMP_SESSION_STORAGE and OMP_SESSION_SQL_DSN_FILE, and OMP_SESSION_SQL_DSN, the projected
// file's own name, which carries the same URL by value — read here from the constants the rest of
// the daemon builds on, so that a rename reaching those constants and not this package fails here
// rather than silently leaving a test free to open the deployment's store. Guard, RequireGuarded
// and Isolated all read one predicate, so Isolated's slice holds for the three of them.
// internal/runtime/paneenv_test.go pins its own list the same way, for the same reason.
func TestTheSessionNamesArePinnedToTheirConstants(t *testing.T) {
	for _, name := range []string{ompsessions.StorageVariable, ompsessions.DSNFileVariable, ompsessions.DSNFileName} {
		t.Setenv(name, "a store no test may open")
		if slices.ContainsFunc(fixtureenv.Isolated(t.TempDir()), func(entry string) bool {
			key, _, _ := strings.Cut(entry, "=")
			return key == name
		}) {
			t.Errorf("Isolated holds %s, one of the names that place a session", name)
		}
	}
}

// run runs argv in dir under env and returns its trimmed output and error.
func run(dir string, env []string, argv ...string) (string, error) {
	command := exec.Command(argv[0], argv[1:]...)
	command.Dir = dir
	command.Env = env
	out, err := command.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func mustRun(t *testing.T, dir string, env []string, argv ...string) string {
	t.Helper()
	out, err := run(dir, env, argv...)
	if err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(argv, " "), err, out)
	}
	return out
}

// A process environment that names another repository (GIT_DIR, GIT_WORK_TREE), a workspace
// (LEGION_WORKSPACE) and a credential (GH_TOKEN), as a Legion issue pod's can, reaches no command
// run under Isolated: a commit in the test's own repository leaves the decoy repository as it was.
func TestIsolatedDropsTheProcesssRepositoryWorkspaceAndCredential(t *testing.T) {
	decoy := t.TempDir()
	clean := fixtureenv.Isolated(t.TempDir())
	mustRun(t, decoy, clean, "git", "init", "--quiet", "--initial-branch=main")
	mustRun(t, decoy, clean, "git", "commit", "--quiet", "--allow-empty", "-m", "decoy")
	before := mustRun(t, decoy, clean, "git", "rev-parse", "HEAD")

	t.Setenv("GIT_DIR", filepath.Join(decoy, ".git"))
	t.Setenv("GIT_WORK_TREE", decoy)
	t.Setenv("LEGION_WORKSPACE", decoy)
	t.Setenv("GH_TOKEN", "a pod's credential")
	t.Setenv("OMP_SESSION_SQL_DSN_FILE", "/legion/secrets/session-dsn")
	t.Setenv("OMP_SESSION_STORAGE", "sql")
	t.Setenv("OMP_SESSION_SQL_DSN", "postgres://legion@postgres.legion:5432/sessions")
	env := fixtureenv.Isolated(t.TempDir())
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if slices.Contains([]string{
			"GIT_DIR", "GIT_WORK_TREE", "LEGION_WORKSPACE", "GH_TOKEN",
			"OMP_SESSION_SQL_DSN_FILE", "OMP_SESSION_STORAGE", "OMP_SESSION_SQL_DSN",
		}, key) {
			t.Errorf("Isolated holds %s, want none of the process's", entry)
		}
	}
	work := t.TempDir()
	mustRun(t, work, env, "git", "init", "--quiet", "--initial-branch=main")
	mustRun(t, work, env, "git", "commit", "--quiet", "--allow-empty", "-m", "the test's own")
	if after := mustRun(t, decoy, clean, "git", "rev-parse", "HEAD"); after != before {
		t.Errorf("the decoy repository's HEAD moved from %s to %s, want it untouched", before, after)
	}
}

// A repository whose own configuration allows http (protocol.http.allow=always, which outranks
// protocol.allow) still reaches no network remote under Isolated: jj git push to a loopback HTTP
// server fails and the server receives no request. Without GIT_ALLOW_PROTOCOL the same push
// reaches the server, so the refusal is the environment's.
func TestIsolatedReachesNoNetworkRemoteWhateverTheRepositoryAllows(t *testing.T) {
	if _, err := exec.LookPath("jj"); err != nil {
		t.Fatalf("this test drives a real jj: %v", err)
	}
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "no", http.StatusForbidden)
	}))
	t.Cleanup(server.Close)

	env := fixtureenv.Isolated(t.TempDir())
	repo := t.TempDir()
	mustRun(t, repo, env, "jj", "git", "init", "--colocate", repo)
	mustRun(t, repo, env, "jj", "describe", "-m", "work that must not leave the test", "-R", repo)
	mustRun(t, repo, env, "jj", "bookmark", "set", "main", "-r", "@", "-R", repo)
	mustRun(t, repo, env, "jj", "git", "remote", "add", "origin", server.URL+"/repo.git", "-R", repo)
	mustRun(t, repo, env, "git", "config", "protocol.http.allow", "always")

	push := []string{"jj", "git", "push", "--remote", "origin", "--bookmark", "main", "-R", repo}
	if out, err := run(repo, env, push...); err == nil {
		t.Errorf("jj git push under Isolated succeeded, want a refusal:\n%s", out)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("the loopback server received %d requests under Isolated, want none", n)
	}

	open := slices.DeleteFunc(slices.Clone(env), func(entry string) bool { return entry == "GIT_ALLOW_PROTOCOL=file" })
	_, _ = run(repo, open, push...)
	if requests.Load() == 0 {
		t.Fatal("the push without GIT_ALLOW_PROTOCOL never reached the loopback server, so this test proves nothing")
	}
}
