package api

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
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
// drives: each role's App token, rendered as the gh files the daemon writes (internal/ghconfig),
// is whose login a plain gh reports, and a git clone through gh's own credential helper reads
// those files. It is the lower-level proof of the rendered files against real GitHub; the helper a
// shared clone runs, `legion git-credential`, and the `legion gh` wrapper are proven against the
// real gh and git in cmd/legion (TestRealGhAndGitSendTheTargetOwnersToken). It is intentionally
// gated: CI has no Legion App keys, whereas the devbox command is a release gate.
func TestRealGitHubCredentialSurface(t *testing.T) {
	if os.Getenv(realGitHubProofEnabled) != "1" {
		t.Skip("set LEGION_REAL_GITHUB=1 to run the real GitHub App credential proof")
	}
	apps := realGitHubApps(t)

	h := newHarness(t)
	tokens := appauth.New(apps, appauth.Options{})
	h.handler = NewServer("127.0.0.1", 0, Options{
		Supervisor:   h.supervisor,
		BootTokens:   h.tokens,
		Project:      testProject,
		Tokens:       tokens,
		GitHubOwners: func() []string { return []string{"sjawhar"} },
		Grants:       credential.New(nil),
		Pool:         h.store.Pool(),
		Record:       record.NewStore(),
	}).Handler
	gh := realGh(t)

	// Each App's lease, rendered as the role's gh files, is whose login the real gh reports.
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
}

// githubAppsLib is the live proofs' App lib, scripts/e2e/lib/github-apps.sh: the one place this
// deployment's App pair, the checks on the four inputs that override it, and the private-key
// command are written.
var githubAppsLib = filepath.Join("..", "..", "..", "..", "scripts", "e2e", "lib", "github-apps.sh")

// liveProofApp is one role's App as githubAppsLib resolves it.
type liveProofApp struct{ id, keyName, keyCommand string }

// liveProofApps resolves the implement and review Apps by running githubAppsLib's own
// require_app_inputs and app_key_command in an environment of environ, so the gated proofs here
// take the lib's defaults, its input checks and its key command, and no second copy of any of
// them exists to drift. A refusal returns the lib's message, which names the input and never its
// value.
func liveProofApps(environ []string) (implement, review liveProofApp, err error) {
	const script = `set -euo pipefail
fail() { printf '%s\n' "$*" >&2; exit 1; }
source "$1"
require_app_inputs implement review
for role in implement review; do
  id=${role}_app_id key=${role}_app_key_name
  printf '%s\0%s\0%s\0' "${!id}" "${!key}" "$(app_key_command "${!key}")"
done`
	command := exec.Command("bash", "-c", script, "bash", githubAppsLib)
	command.Env = environ
	var stderr bytes.Buffer
	command.Stderr = &stderr
	stdout, runErr := command.Output()
	if runErr != nil {
		return liveProofApp{}, liveProofApp{}, fmt.Errorf("%s refused the App inputs (%v): %s", githubAppsLib, runErr, strings.TrimSpace(stderr.String()))
	}
	fields := strings.Split(strings.TrimSuffix(string(stdout), "\x00"), "\x00")
	if len(fields) != 6 {
		return liveProofApp{}, liveProofApp{}, fmt.Errorf("%s printed %d fields for the two Apps, want 6", githubAppsLib, len(fields))
	}
	return liveProofApp{fields[0], fields[1], fields[2]}, liveProofApp{fields[3], fields[4], fields[5]}, nil
}

// realGitHubApps is Legion's implement and review Apps: this repository's own deployment by
// default, another deployment's through the same four inputs the stage proofs read, each App's id
// and the secrets-store key the devbox's `secrets` CLI resolves to its private key, all as
// liveProofApps reads them from the lib.
func realGitHubApps(t *testing.T) config.GitHubApps {
	t.Helper()
	implement, review, err := liveProofApps(os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	apps, err := config.ResolveGitHubApps(config.GitHubApps{
		Implement: config.GitHubApp{AppID: implement.id, PrivateKeyCommand: implement.keyCommand},
		Review:    config.GitHubApp{AppID: review.id, PrivateKeyCommand: review.keyCommand},
	})
	if err != nil {
		t.Fatalf("resolve the GitHub App private-key commands: %v", err)
	}
	return apps
}

// TestLiveProofAppsReadTheLib proves liveProofApps, ungated: with no input set it answers exactly
// the pair the lib's default_<role>_app_* assignments hold, each set input replaces its value, and
// a malformed or empty input is refused naming the input and never its value.
func TestLiveProofAppsReadTheLib(t *testing.T) {
	inputs := []string{
		"LEGION_E2E_IMPLEMENT_APP_ID", "LEGION_E2E_IMPLEMENT_APP_KEY_NAME",
		"LEGION_E2E_REVIEW_APP_ID", "LEGION_E2E_REVIEW_APP_KEY_NAME",
	}
	base := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !slices.Contains(inputs, name) {
			base = append(base, entry)
		}
	}
	with := func(set ...string) []string { return append(slices.Clone(base), set...) }

	lib, err := os.ReadFile(githubAppsLib)
	if err != nil {
		t.Fatalf("read the live proofs' App lib: %v", err)
	}
	defaults := map[string]string{}
	for _, match := range regexp.MustCompile(`(?m)^default_((?:implement|review)_app_(?:id|key_name))=(.*)$`).FindAllStringSubmatch(string(lib), -1) {
		if _, twice := defaults[match[1]]; twice {
			t.Fatalf("%s assigns default_%s twice", githubAppsLib, match[1])
		}
		defaults[match[1]] = match[2]
	}
	if len(defaults) != 4 {
		t.Fatalf("%s assigns %d of the four default_<role>_app_* values, want all four: %v", githubAppsLib, len(defaults), defaults)
	}

	implement, review, err := liveProofApps(with())
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		role string
		app  liveProofApp
	}{{"implement", implement}, {"review", review}} {
		if check.app.id != defaults[check.role+"_app_id"] || check.app.keyName != defaults[check.role+"_app_key_name"] {
			t.Errorf("with no input the %s App is %s/%s, want the lib's default %s/%s", check.role,
				check.app.id, check.app.keyName, defaults[check.role+"_app_id"], defaults[check.role+"_app_key_name"])
		}
		if !strings.HasPrefix(check.app.keyCommand, "secrets "+check.app.keyName+" -- sh -c ") {
			t.Errorf("the %s App's key command reads %q, want `secrets %s -- sh -c …`", check.role, check.app.keyCommand, check.app.keyName)
		}
	}

	implement, review, err = liveProofApps(with(
		"LEGION_E2E_IMPLEMENT_APP_ID=101", "LEGION_E2E_IMPLEMENT_APP_KEY_NAME=EXAMPLE_IMPLEMENT_KEY",
		"LEGION_E2E_REVIEW_APP_ID=202", "LEGION_E2E_REVIEW_APP_KEY_NAME=EXAMPLE_REVIEW_KEY",
	))
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{implement.id, implement.keyName, review.id, review.keyName}; !slices.Equal(got, []string{"101", "EXAMPLE_IMPLEMENT_KEY", "202", "EXAMPLE_REVIEW_KEY"}) {
		t.Errorf("with every input set the Apps are %v, want the inputs", got)
	}

	pasted := strings.Repeat("Zm9vYmFy", 9) // 72 base64 characters: a value, not a store key's name
	for _, refused := range []struct{ input, value string }{
		{"LEGION_E2E_REVIEW_APP_KEY_NAME", pasted},
		{"LEGION_E2E_IMPLEMENT_APP_ID", ""},
		{"LEGION_E2E_IMPLEMENT_APP_ID", "0123"},
	} {
		_, _, err := liveProofApps(with(refused.input + "=" + refused.value))
		if err == nil || !strings.Contains(err.Error(), refused.input) || (refused.value != "" && strings.Contains(err.Error(), refused.value)) {
			t.Errorf("%s=%q: err = %v, want a refusal naming the input and not its value", refused.input, refused.value, err)
		}
	}
}

type realCommandResult struct {
	stdout string
	stderr string
	code   int
}

// realGh is the real GitHub CLI the proof runs: LEGION_GH_PATH when the test runs in a Legion pane
// or pod, whose `gh` on PATH is the `legion gh` wrapper in front of it, and otherwise the gh on
// PATH, as internal/ghconfig's own gh-backed test picks it.
func realGh(t *testing.T) string {
	t.Helper()
	if path := os.Getenv("LEGION_GH_PATH"); path != "" {
		return path
	}
	if path, err := exec.LookPath("gh"); err == nil {
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

// realCLIEnvironment is a pane's environment as far as gh and git read it: the real gh's directory
// first on PATH, so git's `gh auth git-credential` helper is that gh, the gh files configDir holds
// as GH_CONFIG_DIR, and GH_TOKEN, GITHUB_TOKEN and GH_HOST empty so nothing inherited outranks the
// files.
func realCLIEnvironment(gh, configDir string) []string {
	return append(os.Environ(),
		"PATH="+filepath.Dir(gh)+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GH_CONFIG_DIR="+configDir, "GH_TOKEN=", "GITHUB_TOKEN=", "GH_HOST=")
}
