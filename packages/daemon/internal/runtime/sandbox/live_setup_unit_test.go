package sandbox

import (
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

func TestParseSetupMarker(t *testing.T) {
	line := "setup:architect:pod-1/3 cwd=/legion/workspaces/sjawhar/legion-smoke/s4a-1 uid=1000 ppid=41 git_from=/tmp helper=expected helper_count=1 helper_effective=1 helper_lines=2 git=ok git_without_helper=failed"
	got, err := parseSetupMarker(line)
	if err != nil {
		t.Fatal(err)
	}
	if want := (setupMarker{
		role: "architect", incarnation: "pod-1/3", cwd: "/legion/workspaces/sjawhar/legion-smoke/s4a-1",
		uid: "1000", ppid: "41", gitFrom: "/tmp", helper: "expected", helperCount: "1", helperEffective: "1", helperLines: "2",
		git: "ok", gitWithoutHelper: "failed",
	}); got != want {
		t.Errorf("parseSetupMarker(%q) = %#v, want %#v", line, got, want)
	}
}

func TestParseSetupMarkerRejectsMalformedRecords(t *testing.T) {
	const fields = "cwd=/legion uid=1000 ppid=1 git_from=/tmp"
	for _, line := range []string{
		"architect:pod-1/3 " + fields + " helper=expected helper_count=1 helper_effective=1 helper_lines=2 git=ok git_without_helper=failed",
		"setup:architect:pod-1/3 " + fields + " helper=expected helper_count=1 helper_effective=1 git=ok git_without_helper=failed",
		"setup:architect:pod-1/3 " + fields + " helper=unknown helper_count=1 helper_effective=1 helper_lines=2 git=ok git_without_helper=failed",
		"setup:architect:pod-1/3 " + fields + " helper=expected helper_count=one helper_effective=1 helper_lines=2 git=ok git_without_helper=failed",
		"setup:architect:pod-1/3 " + fields + " helper=expected helper_count=1 helper_effective=x helper_lines=2 git=ok git_without_helper=failed",
		"setup:architect:pod-1/3 " + fields + " helper=expected helper_count=1 helper_effective=1 helper_lines=2 git=maybe git_without_helper=failed",
		"setup:architect:pod-1/3 " + fields + " helper=expected helper_count=1 helper_effective=1 helper_lines=2 git=ok git_without_helper=maybe",
		"setup:architect:pod-1/3 " + fields + " helper=expected helper_count=1 helper_effective=1 helper_lines=2 git=ok",
		"setup:architect:pod-1/3 " + fields + " helper=expected helper_count=1 helper_effective=1 extra=2 git=ok git_without_helper=failed",
	} {
		t.Run(line, func(t *testing.T) {
			if _, err := parseSetupMarker(line); err == nil {
				t.Fatal("parseSetupMarker succeeded, want a refusal")
			}
		})
	}
}

func TestLiveSetupCarriesTheRepositoryAsAnArgument(t *testing.T) {
	argv := liveSetup(setupRigRepository(t))
	if len(argv) != 5 || argv[0] != "/bin/sh" || argv[1] != "-c" || argv[3] != "stage4a-setup" || argv[4] != "https://github.com/sjawhar/legion-smoke" {
		t.Fatalf("liveSetup = %q, want /bin/sh -c <script> stage4a-setup https://github.com/sjawhar/legion-smoke", argv)
	}
	if strings.Contains(argv[2], "sjawhar/legion-smoke") {
		t.Errorf("liveSetup script embeds the repository instead of taking it as an argument: %q", argv[2])
	}
}

// The setup runs before the agent, in its directory and as its uid, and every git it runs gets the
// allowlist liveSetupScript names and nothing else: the role's GIT_CONFIG_* entries, its PATH and
// LEGION_GH_OWNERS_DIR, an empty HOME and the three fixed git variables, whatever else the role's
// environment holds.
func TestLiveSetupRunsBeforeTheAgent(t *testing.T) {
	r := newSetupRig(t, false)
	r.roleEnv()
	global := filepath.Join(r.root, "gitconfig")
	if err := os.WriteFile(global, []byte("[credential]\n\thelper = !poisoned-global-helper\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"GIT_ASKPASS": "must-not-reach-git", "SSH_ASKPASS": "must-not-reach-git",
		"GH_TOKEN": "must-not-reach-git", "GITHUB_TOKEN": "must-not-reach-git",
		"GIT_CONFIG_GLOBAL": global, "GIT_CONFIG_PARAMETERS": "'http.extraheader'='Authorization: bearer fake-token'",
		"HTTPS_PROXY": "http://127.0.0.1:9",
	} {
		t.Setenv(name, value)
	}
	agentPID := filepath.Join(r.root, "agent-pid")
	agent := []string{"/bin/sh", "-c", `printf '%s\n' "$$" >"$1"; printf '%s:%s/%s\n' "$LEGION_ROLE" "$POD_UID" "$LEGION_GENERATION" >>"$LEGION_E2E_MARKER"`, "agent", agentPID}
	stderr, lines := r.run(r.start, agent)
	if len(lines) != 2 {
		t.Fatalf("marker lines = %q, want setup then agent", lines)
	}
	setup := parseSetupMarkerOrFail(t, lines[0])
	pid, err := os.ReadFile(agentPID)
	if err != nil {
		t.Fatal(err)
	}
	if setup.ppid != strings.TrimSpace(string(pid)) {
		t.Errorf("setup parent pid %q, want agent pid %q", setup.ppid, strings.TrimSpace(string(pid)))
	}
	if setup.cwd != r.start {
		t.Errorf("setup cwd %q, want start directory %q", setup.cwd, r.start)
	}
	if setup.uid != strconv.Itoa(os.Getuid()) {
		t.Errorf("setup uid %q, want %d", setup.uid, os.Getuid())
	}
	if setup.gitFrom != "/tmp" || setup.git != "ok" || setup.gitWithoutHelper != "failed" {
		t.Errorf("setup git record = from %q, git %q, git_without_helper %q; want /tmp, ok and failed", setup.gitFrom, setup.git, setup.gitWithoutHelper)
	}
	if err := setupGitProblem(setup); err != nil {
		t.Errorf("setupGitProblem = %v, want nil", err)
	}
	if setup.helper != "expected" || setup.helperCount != "1" || setup.helperEffective != "1" || setup.helperLines != "2" {
		t.Errorf("setup helper record = classification %q, count %q, effective %q, lines %q; want expected, one, one and two", setup.helper, setup.helperCount, setup.helperEffective, setup.helperLines)
	}
	if lines[1] != "architect:pod-1/3" {
		t.Errorf("agent marker = %q, want architect:pod-1/3", lines[1])
	}
	if strings.Contains(stderr+strings.Join(lines, "\n"), setupRigToken) {
		t.Errorf("the helper's password reached the marker or stderr: %q %q", lines, stderr)
	}

	withHelper := r.wantEnvironment(false)
	audits := map[string][]string{"config": withHelper, "ls-remote-1": withHelper, "ls-remote-2": r.wantEnvironment(true)}
	for call, want := range audits {
		if got := r.audited(call + "-env"); !slices.Equal(got, want) {
			t.Errorf("git %s environment =\n%s\nwant exactly\n%s", call, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		if got := r.audited(call + "-home"); !slices.Equal(got, []string{"empty"}) {
			t.Errorf("git %s HOME = %q, want an empty temporary directory", call, got)
		}
	}
	for _, call := range []string{"ls-remote-1", "ls-remote-2"} {
		if got := r.audited(call + "-cwd"); !slices.Equal(got, []string{"/tmp"}) {
			t.Errorf("git %s ran in %q, want /tmp", call, got)
		}
	}
}

// The control ls-remote runs with every command-scope helper emptied, so it succeeds only when
// something other than the role's helper authenticates git. setup-ran requires git=ok and
// git_without_helper=failed: with no helper both fail, and against a repository that needs no
// credential both succeed, and each is refused.
func TestLiveSetupNegativeControl(t *testing.T) {
	for name, tc := range map[string]struct {
		public, helper        bool
		git, gitWithoutHelper string
		accepted              bool
	}{
		"the role's helper":   {helper: true, git: "ok", gitWithoutHelper: "failed", accepted: true},
		"no helper":           {git: "failed", gitWithoutHelper: "failed"},
		"a public repository": {public: true, helper: true, git: "ok", gitWithoutHelper: "ok"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newSetupRig(t, tc.public)
			if tc.helper {
				r.roleEnv()
			}
			setup := r.probe(r.start)
			if setup.git != tc.git || setup.gitWithoutHelper != tc.gitWithoutHelper {
				t.Errorf("git = %q, git_without_helper = %q; want %q and %q", setup.git, setup.gitWithoutHelper, tc.git, tc.gitWithoutHelper)
			}
			if err := setupGitProblem(setup); (err == nil) != tc.accepted {
				t.Errorf("setupGitProblem = %v, want accepted %t", err, tc.accepted)
			}
		})
	}
}

// runtime.kubernetes.pod.env may set any git variable runtimeOwned does not hold (manifest.go), and
// the setup command inherits it. None may change what the probe records or the environment any git
// of it receives (poisons names what each would do), with the role's helper and with another: each
// one's record and environments are the ones the probe gives without it.
func TestLiveSetupIgnoresTheRoleEnvironmentGitVariables(t *testing.T) {
	for name, tc := range map[string]struct {
		helper, class, git string
	}{
		"the role's helper": {helper: workspace.GitHubCredentialHelper, class: "expected", git: "ok"},
		"another helper":    {helper: "!different git-credential", class: "other", git: "failed"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newSetupRig(t, false)
			clone := r.configuredClone()
			r.roleEnv()
			t.Setenv("GIT_CONFIG_VALUE_1", tc.helper)
			clean := r.probe(clone)
			if clean.helper != tc.class || clean.git != tc.git || clean.gitWithoutHelper != "failed" {
				t.Fatalf("clean record = helper %q, git %q, git_without_helper %q; want %q, %q and failed", clean.helper, clean.git, clean.gitWithoutHelper, tc.class, tc.git)
			}
			cleanEnv := r.environments()
			for poison, variables := range r.poisons() {
				t.Run(poison, func(t *testing.T) {
					for variable, value := range variables {
						t.Setenv(variable, value)
					}
					poisoned := r.probe(clone)
					if got, want := setupOutcome(poisoned), setupOutcome(clean); got != want {
						t.Errorf("record = %s, want the record without %s, %s", got, poison, want)
					}
					if got := r.environments(); !maps.EqualFunc(got, cleanEnv, slices.Equal) {
						t.Errorf("git environments =\n%v\nwant the ones without %s\n%v", got, poison, cleanEnv)
					}
				})
			}
		})
	}
}

// `git config -z` ends each record with a NUL, so a value's newlines stay inside it: a local helper
// whose value forges two command-scope records, an empty reset and Legion's helper, reads as the one
// local record it is.
func TestLiveSetupReadsEachConfigRecordWhole(t *testing.T) {
	r := newSetupRig(t, false)
	clone := r.plainClone()
	forged := "x\ncommand\tcredential.https://github.com.helper \ncommand\tcredential.https://github.com.helper " + workspace.GitHubCredentialHelper
	r.git("-C", clone, "config", "credential.helper", forged)
	setup := r.probe(clone)
	if setup.helper != "other" || setup.helperCount != "1" || setup.helperEffective != "1" || setup.helperLines != "1" {
		t.Errorf("setup classification = %q with count %q, effective %q and lines %q; want other, one, one and one", setup.helper, setup.helperCount, setup.helperEffective, setup.helperLines)
	}
}

func TestLiveSetupNeverRecordsAHelperValue(t *testing.T) {
	const poison = "not-a-real-token"
	r := newSetupRig(t, false)
	r.roleEnv()
	t.Setenv("GIT_CONFIG_VALUE_1", workspace.GitHubCredentialHelper+"\tfuture="+poison)
	stderr, lines := r.run(r.start, []string{"/bin/true"})
	for _, text := range append([]string{stderr}, lines...) {
		if strings.Contains(text, poison) {
			t.Fatalf("setup leaks helper text %q: marker %q, stderr %q", poison, lines, stderr)
		}
	}
	setup := parseSetupMarkerOrFail(t, lines[0])
	if setup.helper != "other" || setup.helperCount != "1" || setup.helperEffective != "1" || setup.helperLines != "2" {
		t.Fatalf("setup classification = %q with count %q, effective %q and lines %q, want other, one, one and two", setup.helper, setup.helperCount, setup.helperEffective, setup.helperLines)
	}
	err := setupHelperProblem(setup)
	if err == nil {
		t.Fatal("setupHelperProblem accepted a poisoned helper")
	}
	if strings.Contains(err.Error(), poison) {
		t.Fatalf("setup helper error leaks helper text %q: %q", poison, err)
	}
	if !strings.Contains(err.Error(), "lines 2") {
		t.Fatalf("setup helper error %q does not name helper_lines", err)
	}
}

// Every role container sets XDG_CONFIG_HOME, and git reads $XDG_CONFIG_HOME/git/config as its
// global scope when GIT_CONFIG_GLOBAL is unset, so a helper written there must not count.
func TestLiveSetupIgnoresTheXDGGlobalConfig(t *testing.T) {
	r := newSetupRig(t, false)
	xdg := filepath.Join(r.root, "xdg")
	if err := os.MkdirAll(filepath.Join(xdg, "git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(xdg, "git", "config"), []byte("[credential \"https://github.com\"]\n\thelper = !xdg-global-helper\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", xdg)
	r.roleEnv()
	setup := r.probe(r.start)
	if setup.helper != "expected" || setup.helperLines != "2" {
		t.Errorf("setup helper record = classification %q, lines %q; want expected and two, with the XDG global config unread", setup.helper, setup.helperLines)
	}
}

func TestLiveSetupClassifiesGitCredentialEnv(t *testing.T) {
	for name, changed := range map[string]bool{
		"the runtime helper": false,
		"a changed helper":   true,
	} {
		t.Run(name, func(t *testing.T) {
			r := newSetupRig(t, false)
			r.roleEnv()
			if changed {
				t.Setenv("GIT_CONFIG_VALUE_1", "!different git-credential")
			}
			setup := r.probe(r.start)
			want := "expected"
			if changed {
				want = "other"
			}
			if setup.helper != want || setup.helperCount != "1" || setup.helperEffective != "1" || setup.helperLines != "2" {
				t.Errorf("setup classification = %q with count %q, effective %q and lines %q, want %q, one, one and two", setup.helper, setup.helperCount, setup.helperEffective, setup.helperLines, want)
			}
		})
	}
}

func TestLiveSetupClassifiesEffectiveRepositoryHelpers(t *testing.T) {
	extra := func(key, value string) func(t *testing.T, r *setupRig, clone string) {
		return func(t *testing.T, r *setupRig, _ string) {
			r.roleEnv()
			t.Setenv("GIT_CONFIG_COUNT", "4")
			t.Setenv("GIT_CONFIG_KEY_3", key)
			t.Setenv("GIT_CONFIG_VALUE_3", value)
		}
	}
	for name, tc := range map[string]struct {
		configure                       func(t *testing.T, r *setupRig, clone string)
		helper, count, effective, lines string
	}{
		"the live repository and runtime environment": {
			configure: func(_ *testing.T, r *setupRig, _ string) { r.roleEnv() },
			helper:    "expected", count: "3", effective: "1", lines: "6",
		},
		"only the repository helper": {
			configure: func(*testing.T, *setupRig, string) {},
			helper:    "other", count: "2", effective: "1", lines: "4",
		},
		"an extra command helper": {
			configure: extra("credential.https://github.com.helper", "!extra credential"),
			helper:    "other", count: "4", effective: "2", lines: "7",
		},
		"a local non-legion helper cleared by command scope": {
			configure: func(_ *testing.T, r *setupRig, clone string) {
				r.roleEnv()
				r.git("-C", clone, "config", "--add", "credential.https://github.com.helper", "!local credential")
			},
			helper: "expected", count: "4", effective: "1", lines: "7",
		},
		"a generic command helper after legion's": {
			configure: extra("credential.helper", "!generic credential"),
			helper:    "other", count: "4", effective: "2", lines: "7",
		},
		"a command helper under another URL form after legion's": {
			configure: extra("credential.https://github.com/.helper", "!slash credential"),
			helper:    "other", count: "4", effective: "2", lines: "7",
		},
		"an empty command helper under another URL form after legion's": {
			configure: extra("credential.https://github.com/.helper", ""),
			helper:    "other", count: "3", effective: "2", lines: "7",
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newSetupRig(t, false)
			clone := r.configuredClone()
			tc.configure(t, r, clone)
			setup := r.probe(clone)
			if setup.helper != tc.helper || setup.helperCount != tc.count || setup.helperEffective != tc.effective || setup.helperLines != tc.lines {
				t.Errorf("setup classification = %q with count %q, effective %q and lines %q, want %q, %q, %q and %q", setup.helper, setup.helperCount, setup.helperEffective, setup.helperLines, tc.helper, tc.count, tc.effective, tc.lines)
			}
		})
	}
}

// git runs credential.helper and credential.https://github.com.helper as one list for an
// https://github.com request, in its read order, so liveSetupScript must classify both keys
// together: two fake helpers that record only their own name show the order, and an empty
// command-scope url-specific value drops a local generic helper.
func TestGitCombinesBothHelperKeysInReadOrder(t *testing.T) {
	h := newHelperLog(t)
	generic, specific := h.helper("generic"), h.helper("specific")
	genericFirst := h.repo("generic-first", [2]string{"credential.helper", generic}, [2]string{"credential.https://github.com.helper", specific})
	if got := h.fill(genericFirst); strings.Join(got, " ") != "generic specific" {
		t.Errorf("helpers run = %q, want generic then specific: git combines both keys in read order", got)
	}
	specificFirst := h.repo("specific-first", [2]string{"credential.https://github.com.helper", specific}, [2]string{"credential.helper", generic})
	if got := h.fill(specificFirst); strings.Join(got, " ") != "specific generic" {
		t.Errorf("helpers run = %q, want specific then generic", got)
	}
	reset := commandConfig([2]string{"credential.https://github.com.helper", ""}, [2]string{"credential.https://github.com.helper", specific})
	if got := h.fill(h.repo("reset", [2]string{"credential.helper", generic}), reset...); strings.Join(got, " ") != "specific" {
		t.Errorf("helpers run = %q, want specific alone: the empty command-scope value drops the local generic helper", got)
	}
}

// For a request for https://github.com/acme/widgets.git with credential.useHttpPath on, git runs a
// helper set under any URL form that matches the request, not only credential.https://github.com:
// one with a trailing slash, a path prefix, no scheme, a wildcard host or the default port. It
// ignores a form for another path, another scheme or another host. An empty value under a matching
// form empties the list as credential.https://github.com's does. So liveSetupScript counts a record
// under any other helper key as an entry, never as a reset.
func TestGitCredentialHelperURLForms(t *testing.T) {
	h := newHelperLog(t)
	ran := h.helper("ran")
	outside := t.TempDir()
	for key, applies := range map[string]bool{
		"credential.https://github.com.helper":                  true,
		"credential.https://github.com/.helper":                 true,
		"credential.https://github.com/acme.helper":             true,
		"credential.https://github.com/acme/widgets.git.helper": true,
		"credential.github.com.helper":                          true,
		"credential.https://*.com.helper":                       true,
		"credential.https://github.com:443.helper":              true,
		"credential.https://github.com/other.helper":            false,
		"credential.http://github.com.helper":                   false,
		"credential.https://gist.github.com.helper":             false,
	} {
		got := h.fill(outside, commandConfig([2]string{key, ran}, [2]string{"credential.useHttpPath", "true"})...)
		if want := map[bool]string{true: "ran", false: ""}[applies]; strings.Join(got, " ") != want {
			t.Errorf("%s: helpers run = %q, want %q", key, got, want)
		}
	}
	generic := h.helper("generic")
	reset := commandConfig([2]string{"credential.helper", generic}, [2]string{"credential.https://github.com/.helper", ""})
	if got := h.fill(outside, reset...); len(got) != 0 {
		t.Errorf("helpers run = %q, want none: an empty value under credential.https://github.com/ empties the list", got)
	}
}

// setupRig runs liveSetupScript through the real start wrapper from a directory of its own, with a
// fake git first on PATH that hands `git config` to the real git and stands in for ls-remote, and a
// fake `legion` whose `git-credential get` answers a password only under the owners directory the
// rig names. The fake ls-remote succeeds when the configuration it reads, as a real ls-remote does
// (never through GIT_CONFIG), names an http.extraHeader for github.com, or when the real git's
// `credential fill` for sjawhar/legion-smoke, run with the environment the probe gave it, returns
// a password; otherwise it fails as git does against a private repository. Each fake call records
// its environment, its HOME's emptiness and its directory under the rig's audit directory.
type setupRig struct {
	t                               *testing.T
	root, start, bin, audit, owners string
	marker, realGit                 string
}

const setupRigToken = "fake-installation-token"

const setupRigGit = `#!/bin/sh
real=%s
audit=%s
public=%s
environment() {
  env | grep -v -E '^(PWD|SHLVL|OLDPWD|_|HOME)=' | LC_ALL=C sort
}
home() {
  if [ -z "$(ls -A "$HOME")" ]; then echo empty; else echo nonempty; fi
}
case $1 in
config)
  environment >"$audit/config-env"
  home >"$audit/config-home"
  exec "$real" "$@"
  ;;
ls-remote)
  n=1
  while [ -e "$audit/ls-remote-$n-env" ]; do n=$((n + 1)); done
  environment >"$audit/ls-remote-$n-env"
  home >"$audit/ls-remote-$n-home"
  pwd >"$audit/ls-remote-$n-cwd"
  if [ -z "$public" ]; then
    header=$(env -u GIT_CONFIG "$real" config --get-urlmatch http.extraheader https://github.com/ 2>/dev/null) || header=
    if [ -z "$header" ]; then
      answer=$(printf 'protocol=https\nhost=github.com\npath=sjawhar/legion-smoke.git\n\n' | "$real" credential fill 2>/dev/null) || exit 128
      case $answer in *password=?*) ;; *) exit 128 ;; esac
    fi
  fi
  printf '0123456789012345678901234567890123456789\tHEAD\n'
  ;;
*)
  echo "fake git: unexpected arguments: $*" >&2
  exit 2
  ;;
esac
`

const setupRigLegion = `#!/bin/sh
[ "$1" = git-credential ] && [ "$2" = get ] || exit 0
cat >/dev/null
[ "${LEGION_GH_OWNERS_DIR-}" = %s ] || exit 1
printf 'username=x-access-token\npassword=%s\n'
`

func newSetupRig(t *testing.T, public bool) *setupRig {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	r := &setupRig{
		t: t, root: root, start: filepath.Join(root, "start"), bin: filepath.Join(root, "bin"),
		audit: filepath.Join(root, "audit"), owners: filepath.Join(root, "owners"), marker: filepath.Join(root, "marker"),
		realGit: realGit,
	}
	for _, dir := range []string{r.start, r.bin, r.owners} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	publicFlag := ""
	if public {
		publicFlag = "1"
	}
	scripts := map[string]string{
		"git":    fmt.Sprintf(setupRigGit, shellQuote(realGit), shellQuote(r.audit), publicFlag),
		"legion": fmt.Sprintf(setupRigLegion, shellQuote(r.owners), setupRigToken),
	}
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(r.bin, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// The test process may inherit command-scope git entries of its own; the rig starts with none.
	for name, value := range map[string]string{
		"GIT_CONFIG_COUNT": "0", "LEGION_E2E_MARKER": r.marker, "LEGION_ROLE": "architect", "POD_UID": "pod-1",
		"LEGION_GENERATION": "3", "LEGION_GH_OWNERS_DIR": r.owners,
	} {
		t.Setenv(name, value)
	}
	return r
}

// roleEnv sets the role's command-scope git entries, gitCredentialEnv's as the runtime sets them.
func (r *setupRig) roleEnv() {
	for _, variable := range gitCredentialEnv() {
		r.t.Setenv(variable.Name, variable.Value)
	}
}

// poisons are the git variables the role's environment may carry, each set that would change what
// the probe reads or how its ls-remote authenticates were it to reach the probe's git: GIT_CONFIG
// makes `git config` alone read a forged file holding Legion's helper, GIT_DIR and GIT_COMMON_DIR
// hand git another repository's configuration, whose http.extraHeader authenticates ls-remote with
// no helper, an askpass answers git's prompt with no helper, and GIT_CONFIG_PARAMETERS adds a helper
// at command scope. gh reads GH_TOKEN and GITHUB_TOKEN; git ignores them, but they must not reach it.
func (r *setupRig) poisons() map[string]map[string]string {
	r.t.Helper()
	forged := filepath.Join(r.root, "forged-config")
	if err := os.WriteFile(forged, []byte("[credential \"https://github.com\"]\n\thelper =\n\thelper = "+workspace.GitHubCredentialHelper+"\n"), 0o600); err != nil {
		r.t.Fatal(err)
	}
	headers := filepath.Join(r.root, "headers")
	r.git("init", "--quiet", headers)
	r.git("-C", headers, "config", "http.https://github.com/.extraheader", "Authorization: basic Zm9yZ2Vk")
	askpass := filepath.Join(r.root, "askpass")
	if err := os.WriteFile(askpass, []byte("#!/bin/sh\necho forged\n"), 0o700); err != nil {
		r.t.Fatal(err)
	}
	poisons := map[string]map[string]string{
		"GIT_CONFIG":            {"GIT_CONFIG": forged},
		"GIT_DIR":               {"GIT_DIR": filepath.Join(headers, ".git")},
		"GIT_COMMON_DIR":        {"GIT_COMMON_DIR": filepath.Join(headers, ".git")},
		"an askpass":            {"GIT_ASKPASS": askpass, "SSH_ASKPASS": askpass},
		"GIT_CONFIG_PARAMETERS": {"GIT_CONFIG_PARAMETERS": "'credential.https://github.com.helper'='" + workspace.GitHubCredentialHelper + "'"},
		"gh tokens":             {"GH_TOKEN": "forged", "GITHUB_TOKEN": "forged"},
	}
	all := map[string]string{}
	for _, poison := range poisons {
		maps.Copy(all, poison)
	}
	poisons["all of them"] = all
	return poisons
}

// configuredClone is a repository whose credential configuration the daemon wrote, as the issue
// clone's is.
func (r *setupRig) configuredClone() string {
	r.t.Helper()
	clone := r.plainClone()
	if _, err := workspace.ConfigureRepositoryCredential(context.Background(), workspace.NewRunner(time.Minute, map[string]string{"git": r.realGit}), clone); err != nil {
		r.t.Fatalf("configure repository credential: %v", err)
	}
	return clone
}

func (r *setupRig) plainClone() string {
	r.t.Helper()
	clone := filepath.Join(r.root, "clone")
	r.git("init", "--quiet", clone)
	return clone
}

func (r *setupRig) git(args ...string) {
	r.t.Helper()
	if output, err := exec.Command(r.realGit, args...).CombinedOutput(); err != nil {
		r.t.Fatalf("git %q: %v: %s", args, err, output)
	}
}

// run runs the start wrapper with liveSetup and agent from start, and returns the wrapper's stderr
// and the marker's lines, with the marker and the audit directory emptied first.
func (r *setupRig) run(start string, agent []string) (string, []string) {
	r.t.Helper()
	if err := os.RemoveAll(r.audit); err != nil {
		r.t.Fatal(err)
	}
	if err := os.Mkdir(r.audit, 0o700); err != nil {
		r.t.Fatal(err)
	}
	if err := os.Remove(r.marker); err != nil && !os.IsNotExist(err) {
		r.t.Fatal(err)
	}
	got := runStart(r.t, start, r.bin, "", nil, liveSetup(setupRigRepository(r.t)), agent)
	if got.exit != 0 {
		r.t.Fatalf("the wrapper exited %d: %s", got.exit, got.stderr)
	}
	return got.stderr, readLines(r.t, r.marker)
}

// probe runs the setup alone from start and returns its one record.
func (r *setupRig) probe(start string) setupMarker {
	r.t.Helper()
	_, lines := r.run(start, []string{"/bin/true"})
	if len(lines) != 1 {
		r.t.Fatalf("marker lines = %q, want its setup line alone", lines)
	}
	return parseSetupMarkerOrFail(r.t, lines[0])
}

// environments is every environment the fake git recorded in the last run, by call.
func (r *setupRig) environments() map[string][]string {
	r.t.Helper()
	entries, err := os.ReadDir(r.audit)
	if err != nil {
		r.t.Fatal(err)
	}
	env := map[string][]string{}
	for _, entry := range entries {
		if call, ok := strings.CutSuffix(entry.Name(), "-env"); ok {
			env[call] = r.audited(entry.Name())
		}
	}
	return env
}

// audited is the lines the fake git recorded under name in the last run.
func (r *setupRig) audited(name string) []string {
	r.t.Helper()
	body, err := os.ReadFile(filepath.Join(r.audit, name))
	if err != nil {
		r.t.Fatalf("the fake git recorded no %s: %v", name, err)
	}
	return strings.Split(strings.TrimRight(string(body), "\n"), "\n")
}

// wantEnvironment is the whole environment, HOME aside, that liveSetupScript's allowlist gives
// every git under roleEnv: gitCredentialPairs as command-scope entries, with each helper key's
// value emptied for the negative control, the role's PATH (runStart's) and owners directory, and
// the three fixed git variables.
func (r *setupRig) wantEnvironment(withoutHelper bool) []string {
	env := []string{
		"PATH=" + r.bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"LEGION_GH_OWNERS_DIR=" + r.owners,
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=" + strconv.Itoa(len(gitCredentialPairs)),
	}
	for i, pair := range gitCredentialPairs {
		value := pair[1]
		if withoutHelper && strings.HasSuffix(strings.ToLower(pair[0]), "helper") {
			value = ""
		}
		env = append(env, "GIT_CONFIG_KEY_"+strconv.Itoa(i)+"="+pair[0], "GIT_CONFIG_VALUE_"+strconv.Itoa(i)+"="+value)
	}
	slices.Sort(env)
	return env
}

// setupOutcome is what a record says about git, every field but the run's own directory and pid.
func setupOutcome(m setupMarker) string {
	return fmt.Sprintf("helper=%s helper_count=%s helper_effective=%s helper_lines=%s git=%s git_without_helper=%s",
		m.helper, m.helperCount, m.helperEffective, m.helperLines, m.git, m.gitWithoutHelper)
}

func setupRigRepository(t *testing.T) ghrepo.Repository {
	t.Helper()
	repo, err := ghrepo.Parse("repo", "sjawhar/legion-smoke")
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

func parseSetupMarkerOrFail(t *testing.T, line string) setupMarker {
	t.Helper()
	setup, err := parseSetupMarker(line)
	if err != nil {
		t.Fatal(err)
	}
	return setup
}

// helperLog runs the real git's `credential fill` for https://github.com/acme/widgets.git in a
// clean environment, with fake helpers that record only their own name when git asks them.
type helperLog struct {
	t                 *testing.T
	dir, ran, realGit string
}

func newHelperLog(t *testing.T) *helperLog {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	return &helperLog{t: t, dir: dir, ran: filepath.Join(dir, "ran"), realGit: realGit}
}

func (h *helperLog) helper(name string) string {
	h.t.Helper()
	path := filepath.Join(h.dir, "helper-"+name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n[ \"$1\" = get ] && echo "+name+" >>"+h.ran+"\nexit 0\n"), 0o700); err != nil {
		h.t.Fatal(err)
	}
	return path
}

func (h *helperLog) repo(name string, keys ...[2]string) string {
	h.t.Helper()
	path := filepath.Join(h.dir, name)
	if output, err := exec.Command(h.realGit, "init", "--quiet", path).CombinedOutput(); err != nil {
		h.t.Fatalf("init %s: %v: %s", name, err, output)
	}
	for _, kv := range keys {
		if output, err := exec.Command(h.realGit, "-C", path, "config", "--add", kv[0], kv[1]).CombinedOutput(); err != nil {
			h.t.Fatalf("config %s: %v: %s", kv[0], err, output)
		}
	}
	return path
}

// fill returns the helpers git ran, in order, for one request from dir under extra.
func (h *helperLog) fill(dir string, extra ...string) []string {
	h.t.Helper()
	if err := os.WriteFile(h.ran, nil, 0o600); err != nil {
		h.t.Fatal(err)
	}
	cmd := exec.Command(h.realGit, "credential", "fill")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\npath=acme/widgets.git\n\n")
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + h.dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}, extra...)
	_ = cmd.Run()
	body, err := os.ReadFile(h.ran)
	if err != nil {
		h.t.Fatal(err)
	}
	return strings.Fields(string(body))
}

// commandConfig is pairs as GIT_CONFIG_COUNT, GIT_CONFIG_KEY_<n> and GIT_CONFIG_VALUE_<n>.
func commandConfig(pairs ...[2]string) []string {
	env := []string{"GIT_CONFIG_COUNT=" + strconv.Itoa(len(pairs))}
	for i, pair := range pairs {
		env = append(env, "GIT_CONFIG_KEY_"+strconv.Itoa(i)+"="+pair[0], "GIT_CONFIG_VALUE_"+strconv.Itoa(i)+"="+pair[1])
	}
	return env
}
