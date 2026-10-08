package api

import (
	"bytes"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/record"
)

const realGitHubProofEnabled = "LEGION_REAL_GITHUB"

// TestRealGitHubCredentialSurface keeps the Stage 3 credential proof beside the real Apps it
// drives: each role's App token reaches a plain gh and git as the gh files the daemon renders
// (internal/ghconfig), and the built `legion threads resolve` runs as the implement App from them
// and the two App logins the daemon names on a pane. It is intentionally gated: CI has no Legion
// App keys, whereas the devbox command is a release gate and Task 3.14 reuses the same proof
// rather than rebuilding an ad-hoc rig.
func TestRealGitHubCredentialSurface(t *testing.T) {
	if os.Getenv(realGitHubProofEnabled) != "1" {
		t.Skip("set LEGION_REAL_GITHUB=1 to run the real GitHub App credential proof")
	}
	apps, err := config.ResolveGitHubApps(config.GitHubApps{
		Implement: config.GitHubApp{
			AppID: "3202636", PrivateKeyCommand: privateKeyCommand("LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64"),
		},
		Review: config.GitHubApp{
			AppID: "3202653", PrivateKeyCommand: privateKeyCommand("GH_REVIEW_APP_PRIVATE_KEY_B64"),
		},
	})
	if err != nil {
		t.Fatalf("resolve the GitHub App private-key commands: %v", err)
	}

	h := newHarness(t)
	tokens := appauth.New(apps, appauth.Options{})
	h.handler = NewServer("127.0.0.1", 0, Options{
		Supervisor:  h.supervisor,
		BootTokens:  h.tokens,
		Project:     testProject,
		Tokens:      tokens,
		GitHubOwner: "sjawhar",
		Grants:      credential.New(nil),
		Pool:        h.store.Pool(),
		Record:      record.NewStore(),
	}).Handler
	server := httptest.NewServer(h.handler)
	defer server.Close()

	binary := buildLegionForRealProof(t)
	gh := realGh(t)

	// Each App's lease, rendered as the role's gh files, is whose login the real gh reports.
	logins := map[appauth.AppRole]string{}
	dirs := map[appauth.AppRole]string{}
	for _, role := range appauth.Roles {
		lease, err := tokens.Token(h.ctx, role, "sjawhar")
		if err != nil {
			t.Fatalf("mint the %s App's lease: %v", role, err)
		}
		dirs[role] = writeRealGhFiles(t, lease.Token)
		viewer := runRealGh(t, gh, dirs[role], "api", "graphql", "-f", "query={viewer{login}}", "--jq", ".data.viewer.login")
		if viewer.code != 0 || strings.TrimSpace(viewer.stdout) != lease.Identity.Name {
			t.Fatalf("%s App viewer login through gh = %q (exit %d, stderr %q), want %q", role, viewer.stdout, viewer.code, viewer.stderr, lease.Identity.Name)
		}
		t.Logf("%s App GraphQL login through gh: %s", role, strings.TrimSpace(viewer.stdout))
		logins[role] = lease.Identity.Name
	}

	// The negative control: a GH_CONFIG_DIR with no gh files authenticates nobody.
	denied := runRealGh(t, gh, t.TempDir(), "api", "graphql", "-f", "query={viewer{login}}")
	if denied.code == 0 {
		t.Fatalf("gh with an empty GH_CONFIG_DIR exit 0, stdout %q; want it refused", denied.stdout)
	}
	t.Logf("gh with an empty GH_CONFIG_DIR: exit=%d %s", denied.code, strings.TrimSpace(denied.stderr))

	clone := exec.Command("git", "-c", "credential.helper=", "-c", "credential.helper=!gh auth git-credential",
		"clone", "https://github.com/sjawhar/legion-smoke", filepath.Join(t.TempDir(), "legion-smoke"))
	clone.Env = append(realCLIEnvironment(gh, dirs[appauth.Implement]), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
	output, err := clone.CombinedOutput()
	if err != nil {
		t.Fatalf("clone through gh auth git-credential: %v: %s", err, strings.TrimSpace(string(output)))
	}
	t.Log("clone through gh auth git-credential: succeeded")

	// The built binary resolves as the implement App from the same files and the logins the daemon
	// names on a pane; it prints each thread's outcome, or nothing when none is unresolved.
	resolve := exec.Command(binary, "threads", "resolve", "--pr", "1", "--repo", "sjawhar/legion-smoke")
	resolve.Env = append(realCLIEnvironment(gh, dirs[appauth.Implement]),
		"LEGION_DAEMON_URL="+server.URL, "LEGION_ROLE=implementer",
		"LEGION_IMPLEMENT_APP_LOGIN="+logins[appauth.Implement], "LEGION_REVIEW_APP_LOGIN="+logins[appauth.Review])
	var stdout, stderr bytes.Buffer
	resolve.Stdout, resolve.Stderr = &stdout, &stderr
	if err := resolve.Run(); err != nil {
		t.Fatalf("legion threads resolve as the implement App: %v; stdout %q, stderr %q", err, stdout.String(), stderr.String())
	}
	t.Logf("legion threads resolve: %s", strings.TrimSpace(stdout.String()))
}

func privateKeyCommand(key string) string {
	// secretsd stores the reviewer key in Node-compatible base64: unpadded and with one trailing
	// dangling sextet. Normalize exactly as config.decodeBase64LikeNode before POSIX base64 decodes
	// it; the key remains in this daemon-owned child process.
	return "secrets " + key + ` -- sh -c 'value=$(printf %s "${` + key + `}" | tr "_-" "/+"); case $((${#value} % 4)) in 1) value=${value%?};; esac; case $((${#value} % 4)) in 2) value="${value}==";; 3) value="${value}=";; esac; printf %s "$value" | base64 -d'`
}

type realCommandResult struct {
	stdout string
	stderr string
	code   int
}

// realGh is the real GitHub CLI the proof runs: the gh on PATH, unless it is an older Legion pane's
// shim (`<state_dir>/worker-bin/gh`), in which case the CLI it stood in front of at
// /usr/local/bin/gh, as internal/ghconfig's own gh-backed test picks it.
func realGh(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("gh")
	if err == nil && filepath.Base(filepath.Dir(path)) != "worker-bin" {
		return path
	}
	if path, err := exec.LookPath("/usr/local/bin/gh"); err == nil {
		return path
	}
	t.Fatal("no real gh on this machine: the proof needs one to read the rendered files")
	return ""
}

// runRealGh runs the real gh with the gh files under configDir as its GH_CONFIG_DIR and nothing in
// the environment outranking them.
func runRealGh(t *testing.T, gh, configDir string, args ...string) realCommandResult {
	t.Helper()
	command := exec.Command(gh, args...)
	command.Env = realCLIEnvironment(gh, configDir)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	result := realCommandResult{stdout: stdout.String(), stderr: stderr.String()}
	if err == nil {
		return result
	}
	if exit, ok := err.(*exec.ExitError); ok {
		result.code = exit.ExitCode()
		return result
	}
	t.Fatalf("run gh %s: %v", strings.Join(args, " "), err)
	return realCommandResult{}
}

// writeRealGhFiles writes a role's two gh files for token into a fresh directory, read-only as a
// pod's Secret volume is, and returns the directory.
func writeRealGhFiles(t *testing.T, token string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "gh")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// TempDir's cleanup removes the directory, which it cannot do while it is read-only; cleanups
	// run last-registered first, so this one runs before TempDir's.
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	for name, content := range map[string]string{ghconfig.HostsFile: ghconfig.Hosts(token), ghconfig.ConfigFile: ghconfig.Config} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o400); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	return dir
}

// realCLIEnvironment is a pane's environment as far as gh, git and the built legion read it: the
// real gh's directory first on PATH, so git's `gh auth git-credential` helper is that gh, the gh
// files configDir holds as GH_CONFIG_DIR, and GH_TOKEN, GITHUB_TOKEN and GH_HOST empty so nothing
// inherited outranks the files.
func realCLIEnvironment(gh, configDir string) []string {
	return append(os.Environ(),
		"PATH="+filepath.Dir(gh)+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GH_CONFIG_DIR="+configDir, "GH_TOKEN=", "GITHUB_TOKEN=", "GH_HOST=")
}

func buildLegionForRealProof(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "legion")
	command := exec.Command("go", "build", "-o", binary, "./cmd/legion")
	command.Dir = filepath.Join(root, "packages", "daemon")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build legion binary: %v: %s", err, strings.TrimSpace(string(output)))
	}
	return binary
}
