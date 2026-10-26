package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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

// liveProofAppInputs is the five inputs githubAppsLib reads, each overriding one default: the two
// Apps' ids and key names, and the key runner both key commands run.
var liveProofAppInputs = []string{
	"LEGION_E2E_IMPLEMENT_APP_ID", "LEGION_E2E_IMPLEMENT_APP_KEY_NAME",
	"LEGION_E2E_REVIEW_APP_ID", "LEGION_E2E_REVIEW_APP_KEY_NAME",
	"LEGION_E2E_APP_KEY_RUNNER",
}

// liveProofApp is one role's App as githubAppsLib resolves it.
type liveProofApp struct{ id, keyName, keyCommand string }

// runGitHubAppsLib runs script in bash with lib as $1 and args after it, in environ less the
// variables that make bash run other code or trace its own: an exported SHELLOPTS with xtrace
// would print a refused input's value on stderr, BASHOPTS sets shell options, a BASH_ENV or ENV
// file could write to the stdout the caller parses, BASH_XTRACEFD and PS4 steer a trace, and an
// exported function replaces a builtin the lib calls. --norc keeps out the rc files a
// non-interactive bash built with SSH_SOURCE_BASHRC (Debian's and Ubuntu's) reads when SSH_CLIENT
// is set at a shell level below 2, as under a plain `ssh host go test …`.
func runGitHubAppsLib(lib string, environ []string, script string, args ...string) (stdout []byte, stderr string, err error) {
	command := exec.Command("bash", append([]string{"--norc", "-c", script, "bash", lib}, args...)...)
	command.Env = slices.DeleteFunc(slices.Clone(environ), func(entry string) bool {
		name, _, _ := strings.Cut(entry, "=")
		return strings.HasPrefix(name, "BASH_FUNC_") || slices.Contains(
			[]string{"SHELLOPTS", "BASHOPTS", "BASH_ENV", "ENV", "BASH_XTRACEFD", "PS4"}, name)
	})
	var errOut bytes.Buffer
	command.Stderr = &errOut
	stdout, err = command.Output()
	return stdout, strings.TrimSpace(errOut.String()), err
}

// liveProofApps resolves the implement and review Apps by running githubAppsLib's own
// require_app_inputs and app_key_command in an environment of environ, so the gated proofs here
// take the lib's defaults, its input checks and its key command, and no second copy of any of
// them exists to drift. A refusal returns the lib's message, which names the input and never its
// value.
func liveProofApps(environ []string) (implement, review liveProofApp, err error) {
	return liveProofAppsFrom(githubAppsLib, environ)
}

// liveProofAppsFrom is liveProofApps reading the lib at lib. It refuses anything but exactly six
// fields, each non-empty and on one line, and a key command that failed. Stray output on a line
// of its own is refused with them; stray output with no line break joins a field, which the
// gated path's GET /app check then refuses, since no key signs for that id.
func liveProofAppsFrom(lib string, environ []string) (implement, review liveProofApp, err error) {
	const script = `set -euo pipefail
fail() { printf '%s\n' "$*" >&2; exit 1; }
source "$1"
require_app_inputs implement review
for role in implement review; do
  id=${role}_app_id key=${role}_app_key_name
  command=$(app_key_command "${!key}")
  printf '%s\0%s\0%s\0' "${!id}" "${!key}" "$command"
done`
	stdout, stderr, runErr := runGitHubAppsLib(lib, environ, script)
	if runErr != nil {
		return liveProofApp{}, liveProofApp{}, fmt.Errorf("%s refused the App inputs (%v): %s", lib, runErr, stderr)
	}
	fields := strings.Split(strings.TrimSuffix(string(stdout), "\x00"), "\x00")
	if len(fields) != 6 {
		return liveProofApp{}, liveProofApp{}, fmt.Errorf("%s printed %d fields for the two Apps, want 6", lib, len(fields))
	}
	for index, field := range fields {
		if field == "" || strings.ContainsAny(field, "\r\n") {
			return liveProofApp{}, liveProofApp{}, fmt.Errorf("%s printed field %d of the two Apps empty or on more than one line", lib, index+1)
		}
	}
	return liveProofApp{fields[0], fields[1], fields[2]}, liveProofApp{fields[3], fields[4], fields[5]}, nil
}

// proveLiveProofApps proves apps, the very block the gated proofs act with, against GitHub as every
// stage proof does: it writes apps as legion.yaml's `github_apps` block to work/github-apps.yaml and
// runs the lib's own check_github_apps on that file, so each key signs an App JWT and GitHub's
// GET /app must answer its configured id and the write permission its role needs, which also
// refuses the two Apps configured the other way round. It returns the lib's notes, and on a
// refusal the lib's reason, which names the App by role and id and never the key.
func proveLiveProofApps(environ []string, work string, apps config.GitHubApps) (notes string, err error) {
	block := "github_apps:\n"
	for _, role := range []struct {
		name string
		app  config.GitHubApp
	}{{"implement", apps.Implement}, {"review", apps.Review}} {
		// A JSON string is a YAML double-quoted scalar, as the lib's own write_github_apps quotes it.
		id, _ := json.Marshal(role.app.AppID)
		command, _ := json.Marshal(role.app.PrivateKeyCommand)
		block += fmt.Sprintf("  %s:\n    app_id: %s\n    private_key_command: %s\n", role.name, id, command)
	}
	if err := os.WriteFile(filepath.Join(work, "github-apps.yaml"), []byte(block), 0o600); err != nil {
		return "", err
	}
	const script = `set -euo pipefail
fail() { printf '%s\n' "$*" >&2; exit 1; }
note() { printf '%s\n' "$*" >&2; }
work=$2
source "$1"
check_github_apps implement review`
	_, stderr, runErr := runGitHubAppsLib(githubAppsLib, environ, script, work)
	if runErr != nil {
		return stderr, fmt.Errorf("%s could not prove the Apps against GitHub (%v): %s", githubAppsLib, runErr, stderr)
	}
	return stderr, nil
}

// realGitHubApps is Legion's implement and review Apps: the lib's default pair, another
// deployment's through the same four inputs the stage proofs read, each App's id
// and the secrets-store key the devbox's `secrets` CLI resolves to its private key, all as
// liveProofApps reads them from the lib, and the block it returns proved against GitHub before a
// proof acts as it.
func realGitHubApps(t *testing.T) config.GitHubApps {
	t.Helper()
	// go test's result cache keys a test on the variables it looks up and the files it opens, and
	// sees none of them through the bash child, so name them here: a changed input, lib or GET /app
	// checker runs the gated proof again rather than replaying a pass made as other Apps.
	for _, name := range liveProofAppInputs {
		os.LookupEnv(name)
	}
	for _, file := range []string{githubAppsLib, filepath.Join(filepath.Dir(githubAppsLib), "app-logins.ts")} {
		if _, err := os.ReadFile(file); err != nil {
			t.Fatalf("read the live proofs' App lib: %v", err)
		}
	}
	implement, review, err := liveProofApps(os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	apps := config.GitHubApps{
		Implement: config.GitHubApp{AppID: implement.id, PrivateKeyCommand: implement.keyCommand},
		Review:    config.GitHubApp{AppID: review.id, PrivateKeyCommand: review.keyCommand},
	}
	notes, err := proveLiveProofApps(os.Environ(), t.TempDir(), apps)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(notes)
	resolved, err := config.ResolveGitHubApps(apps)
	if err != nil {
		t.Fatalf("resolve the GitHub App private-key commands: %v", err)
	}
	return resolved
}

// TestLiveProofAppsReadTheLib proves liveProofApps, ungated: with no input set it answers exactly
// the pair the lib's default_<role>_app_* assignments hold, each set input replaces its value, and
// a malformed or empty input is refused naming the input and never its value.
func TestLiveProofAppsReadTheLib(t *testing.T) {
	inputs := liveProofAppInputs
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
	// The key command is what the proofs sign with, so it must read the input's key, not a default's,
	// and decode it as the store holds it: Node-style base64, URL-safe and unpadded, perhaps with one
	// dangling sextet. A stand-in `secrets` puts each value in the command's environment as the
	// real one does, and the command's output must be the value's bytes.
	stub := t.TempDir()
	if err := os.WriteFile(filepath.Join(stub, "secrets"), []byte("#!/bin/sh\nname=$1\nshift 2\nexport \"$name=$STUB_VALUE\"\nexec \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for role, app := range map[string]liveProofApp{"implement": implement, "review": review} {
		want := "secrets EXAMPLE_" + strings.ToUpper(role) + "_KEY -- sh -c "
		if !strings.HasPrefix(app.keyCommand, want) || !strings.Contains(app.keyCommand, "${EXAMPLE_"+strings.ToUpper(role)+"_KEY}") {
			t.Errorf("with every input set the %s App's key command reads %q, want `%s…` reading that key", role, app.keyCommand, want)
		}
		for _, stored := range []struct {
			text  string
			value []byte
		}{
			{base64.RawURLEncoding.EncodeToString([]byte{0xfb, 0xef, 0xbe, 0xff, 0xfe}), []byte{0xfb, 0xef, 0xbe, 0xff, 0xfe}}, // 7 characters, '-' and '_'
			{base64.RawURLEncoding.EncodeToString([]byte{0xfb, 0xff, 0x3e, 0x01}), []byte{0xfb, 0xff, 0x3e, 0x01}},             // 6 characters
			{base64.RawURLEncoding.EncodeToString([]byte{0xfb, 0xff, 0x3e}), []byte{0xfb, 0xff, 0x3e}},                         // 4 characters
			// A dangling sextet after a whole quantum, 5 characters, which a Node-style decode drops.
			{base64.RawURLEncoding.EncodeToString([]byte{0xfb, 0xff, 0x3e}) + "A", []byte{0xfb, 0xff, 0x3e}},
		} {
			run := exec.Command("sh", "-c", app.keyCommand)
			run.Env = append(with(), "PATH="+stub+string(os.PathListSeparator)+os.Getenv("PATH"), "STUB_VALUE="+stored.text)
			got, err := run.Output()
			if err != nil || !bytes.Equal(got, stored.value) {
				t.Errorf("the %s App's key command decodes stored %q to %x (err %v), want %x", role, stored.text, got, err, stored.value)
			}
		}
	}

	pasted := strings.Repeat("Zm9vYmFy", 9) // 72 base64 characters: a value, not a store key's name
	refuses := func(t *testing.T, input, value string, extra ...string) {
		t.Helper()
		_, _, err := liveProofApps(append(with(input+"="+value), extra...))
		if err == nil || !strings.Contains(err.Error(), input) || (value != "" && strings.Contains(err.Error(), value)) {
			t.Errorf("%s=%q with %v: err = %v, want a refusal naming the input and not its value", input, value, extra, err)
		}
	}
	refuses(t, "LEGION_E2E_REVIEW_APP_KEY_NAME", pasted)
	refuses(t, "LEGION_E2E_IMPLEMENT_APP_ID", "")
	refuses(t, "LEGION_E2E_IMPLEMENT_APP_ID", "0123")
	refuses(t, "LEGION_E2E_REVIEW_APP_KEY_NAME", pasted, "SHELLOPTS=braceexpand:hashall:interactive-comments:xtrace")

	t.Run("a UTF-8 locale admits nothing the C locale refuses", func(t *testing.T) {
		// Under en_US.UTF-8 bash's [0-9] matches fullwidth digits, which is what the lib's local
		// LC_ALL=C guards against. Without that locale bash falls back to C, where the case
		// proves nothing, so it is skipped rather than passed.
		probe := exec.Command("bash", "--norc", "-c", `[[ ３ =~ ^[0-9]$ ]]`)
		probe.Env = append(with(), "LC_ALL=en_US.UTF-8")
		if probe.Run() != nil {
			t.Skip("bash under LC_ALL=en_US.UTF-8 does not match a fullwidth digit with [0-9] here: the en_US.UTF-8 locale is absent, so this case cannot show the guard")
		}
		refuses(t, "LEGION_E2E_IMPLEMENT_APP_ID", "３２０２６３６", "LC_ALL=en_US.UTF-8")
		refuses(t, "LEGION_E2E_REVIEW_APP_KEY_NAME", "K\u00e9Y", "LC_ALL=en_US.UTF-8")
	})

	startup := filepath.Join(t.TempDir(), "bash_env")
	if err := os.WriteFile(startup, []byte("printf 'noise from BASH_ENV\\n'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, inherited := range [][]string{
		{"BASH_ENV=" + startup},
		{"BASH_FUNC_printf%%=() { builtin printf 'HIJACK|'; builtin printf \"$@\"; }"},
	} {
		implement, _, err = liveProofApps(append(with(), inherited...))
		if err != nil || implement.id != defaults["implement_app_id"] {
			t.Errorf("with %v inherited the implement App is %q (err %v), want the lib's default %s", inherited, implement.id, err, defaults["implement_app_id"])
		}
	}

	t.Run("an ssh environment's rc file does not reach the lib's child", func(t *testing.T) {
		// A non-interactive bash built with SSH_SOURCE_BASHRC reads ~/.bashrc when SSH_CLIENT is set
		// at a shell level below 2; this one writes to stdout and exports a function. Where bash is
		// built without it, no rc file is read with or without --norc, so the case is skipped.
		home := t.TempDir()
		if err := os.WriteFile(filepath.Join(home, ".bashrc"), []byte("echo 'bashrc ran'\nhijack() { :; }\nexport -f hijack\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		ssh := append(with(), "SSH_CLIENT=192.0.2.1 1 22", "SHLVL=0", "HOME="+home)
		probe := exec.Command("bash", "-c", ":")
		probe.Env = ssh
		if out, _ := probe.Output(); !strings.Contains(string(out), "bashrc ran") {
			t.Skip("this bash reads no ~/.bashrc for a non-interactive shell under SSH_CLIENT (built without SSH_SOURCE_BASHRC), so this case cannot show --norc")
		}
		implement, _, err := liveProofApps(ssh)
		if err != nil || implement.id != defaults["implement_app_id"] {
			t.Errorf("under an ssh environment the implement App is %q (err %v), want the lib's default %s", implement.id, err, defaults["implement_app_id"])
		}
	})

	t.Run("the GET /app check reads the block the proofs act with", func(t *testing.T) {
		// A stand-in bun takes app-logins.ts's place: it keeps the block it was given and answers as
		// GitHub agreed, or refuses as a GitHub that answered another id. The block must be the one
		// the lib's own write_github_apps writes for the same Apps.
		stub, work, want := t.TempDir(), t.TempDir(), t.TempDir()
		bun := "#!/bin/sh\ncp \"$4\" \"$STUB_KEEP\"\n[ -z \"$STUB_REFUSE\" ] || { echo 'app-logins: GET /app as the implement App answered another id' >&2; exit 1; }\necho '{\"implement\":\"implement[bot]\",\"review\":\"review[bot]\"}'\n"
		if err := os.WriteFile(filepath.Join(stub, "bun"), []byte(bun), 0o700); err != nil {
			t.Fatal(err)
		}
		path := "PATH=" + stub + string(os.PathListSeparator) + os.Getenv("PATH")
		kept := filepath.Join(stub, "kept.yaml")
		implement, review, err := liveProofApps(with())
		if err != nil {
			t.Fatal(err)
		}
		apps := config.GitHubApps{
			Implement: config.GitHubApp{AppID: implement.id, PrivateKeyCommand: implement.keyCommand},
			Review:    config.GitHubApp{AppID: review.id, PrivateKeyCommand: review.keyCommand},
		}
		if _, err := proveLiveProofApps(append(with(), path, "STUB_KEEP="+kept), work, apps); err != nil {
			t.Fatalf("a GitHub that agrees: %v", err)
		}
		if _, stderr, err := runGitHubAppsLib(githubAppsLib, with(), `set -euo pipefail
fail() { printf '%s\n' "$*" >&2; exit 1; }
work=$2
source "$1"
require_app_inputs implement review
write_github_apps implement review`, want); err != nil {
			t.Fatalf("the lib's write_github_apps: %v: %s", err, stderr)
		}
		got, _ := os.ReadFile(kept)
		lib, _ := os.ReadFile(filepath.Join(want, "github-apps.yaml"))
		if len(got) == 0 || !bytes.Equal(got, lib) {
			t.Errorf("the GET /app check read\n%s\nwant the block the lib writes for the same Apps\n%s", got, lib)
		}
		if _, err := proveLiveProofApps(append(with(), path, "STUB_KEEP="+kept, "STUB_REFUSE=1"), t.TempDir(), apps); err == nil || !strings.Contains(err.Error(), "answered another id") {
			t.Errorf("a GitHub that refuses: err = %v, want the checker's refusal", err)
		}
	})

	t.Run("the lib's child inherits none of bash's startup or trace variables", func(t *testing.T) {
		stdout, stderr, err := runGitHubAppsLib(githubAppsLib, append(with(),
			"SHELLOPTS=braceexpand:hashall:interactive-comments", "BASHOPTS=extglob", "BASH_ENV="+startup,
			"ENV="+startup, "BASH_XTRACEFD=2", "PS4=+ ", "BASH_FUNC_hijack%%=() { :; }",
		), `exec env -0`)
		if err != nil {
			t.Fatalf("env in the lib's child: %v: %s", err, stderr)
		}
		for _, entry := range strings.Split(string(stdout), "\x00") {
			name, _, _ := strings.Cut(entry, "=")
			if strings.HasPrefix(name, "BASH_FUNC_") || slices.Contains([]string{"SHELLOPTS", "BASHOPTS", "BASH_ENV", "ENV", "BASH_XTRACEFD", "PS4"}, name) {
				t.Errorf("the lib's child inherits %s", name)
			}
		}
	})

	t.Run("a lib whose output is malformed yields no App", func(t *testing.T) {
		// Each scratch lib sources the real one, then breaks one thing about what it prints. The
		// control breaks nothing, so a scratch lib that fails to load cannot pass as a refusal.
		real, err := filepath.Abs(githubAppsLib)
		if err != nil {
			t.Fatal(err)
		}
		wrap := `eval "$(declare -f require_app_inputs | sed '1s/^require_app_inputs/real_require_app_inputs/')"` + "\n"
		for _, scratch := range []struct{ name, body, refusal string }{
			{"the control", wrap + `require_app_inputs() { real_require_app_inputs "$@"; }`, ""},
			{"a seventh field", wrap + `require_app_inputs() { real_require_app_inputs "$@"; printf 'extra\0'; }`, "printed 7 fields"},
			{"stray output on a line of its own", wrap + `require_app_inputs() { real_require_app_inputs "$@"; echo debug; }`, "field 1 of the two Apps empty or on more than one line"},
			{"an empty key command", `app_key_command() { :; }`, "field 3 of the two Apps empty or on more than one line"},
			{"a key command that failed", `app_key_command() { printf 'secrets %s' "$1"; return 3; }`, "exit status 3"},
			{"a key command on two lines", `app_key_command() { printf 'secrets %s\nmore' "$1"; }`, "field 3 of the two Apps empty or on more than one line"},
		} {
			lib := filepath.Join(t.TempDir(), "github-apps.sh")
			if err := os.WriteFile(lib, []byte("source "+strconv.Quote(real)+"\n"+scratch.body+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			implement, review, err := liveProofAppsFrom(lib, with())
			switch {
			case scratch.refusal == "" && (err != nil || implement.id != defaults["implement_app_id"] || review.id != defaults["review_app_id"]):
				t.Errorf("%s: got Apps %+v and %+v (err %v), want the lib's default pair", scratch.name, implement, review, err)
			case scratch.refusal != "" && (err == nil || !strings.Contains(err.Error(), scratch.refusal)):
				t.Errorf("%s: got Apps %+v and %+v (err %v), want a refusal saying %q", scratch.name, implement, review, err, scratch.refusal)
			}
		}
	})
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
