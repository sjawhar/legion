package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

func TestParseSetupMarker(t *testing.T) {
	line := "setup:architect:pod-1/3 cwd=/legion/workspaces/sjawhar/legion-smoke/s4a-1 uid=1000 ppid=41 git_from=/tmp helper=expected helper_count=1 helper_effective=1 helper_lines=2 git=ok"
	got, err := parseSetupMarker(line)
	if err != nil {
		t.Fatal(err)
	}
	if want := (setupMarker{
		role: "architect", incarnation: "pod-1/3", cwd: "/legion/workspaces/sjawhar/legion-smoke/s4a-1",
		uid: "1000", ppid: "41", gitFrom: "/tmp", helper: "expected", helperCount: "1", helperEffective: "1", helperLines: "2", git: "ok",
	}); got != want {
		t.Errorf("parseSetupMarker(%q) = %#v, want %#v", line, got, want)
	}
}

func TestParseSetupMarkerRejectsMalformedRecords(t *testing.T) {
	const fields = "cwd=/legion uid=1000 ppid=1 git_from=/tmp"
	for _, line := range []string{
		"architect:pod-1/3 " + fields + " helper=expected helper_count=1 helper_effective=1 helper_lines=2 git=ok",
		"setup:architect:pod-1/3 " + fields + " helper=expected helper_count=1 helper_effective=1 git=ok",
		"setup:architect:pod-1/3 " + fields + " helper=unknown helper_count=1 helper_effective=1 helper_lines=2 git=ok",
		"setup:architect:pod-1/3 " + fields + " helper=expected helper_count=one helper_effective=1 helper_lines=2 git=ok",
		"setup:architect:pod-1/3 " + fields + " helper=expected helper_count=1 helper_effective=x helper_lines=2 git=ok",
		"setup:architect:pod-1/3 " + fields + " helper=expected helper_count=1 helper_effective=1 helper_lines=2 git=maybe",
		"setup:architect:pod-1/3 " + fields + " helper=expected helper_count=1 helper_effective=1 extra=2 git=ok",
	} {
		t.Run(line, func(t *testing.T) {
			if _, err := parseSetupMarker(line); err == nil {
				t.Fatal("parseSetupMarker succeeded, want a refusal")
			}
		})
	}
}

func TestLiveSetupCarriesTheRepositoryAsAnArgument(t *testing.T) {
	repo, err := ghrepo.Parse("repo", "sjawhar/legion-smoke")
	if err != nil {
		t.Fatal(err)
	}
	argv := liveSetup(repo)
	if len(argv) != 5 || argv[0] != "/bin/sh" || argv[1] != "-c" || argv[3] != "stage4a-setup" || argv[4] != "https://github.com/sjawhar/legion-smoke" {
		t.Fatalf("liveSetup(%s) = %q, want /bin/sh -c <script> stage4a-setup https://github.com/sjawhar/legion-smoke", repo, argv)
	}
	if strings.Contains(argv[2], repo.String()) {
		t.Errorf("liveSetup script embeds %s instead of taking it as an argument: %q", repo, argv[2])
	}
}

func TestLiveSetupRunsBeforeTheAgent(t *testing.T) {
	repo, err := ghrepo.Parse("repo", "sjawhar/legion-smoke")
	if err != nil {
		t.Fatal(err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir, bin := t.TempDir(), t.TempDir()
	marker, agentPID, gitCWD, gitRemoteEnv, global := filepath.Join(dir, "marker"), filepath.Join(dir, "agent-pid"), filepath.Join(dir, "git-cwd"), filepath.Join(dir, "git-remote-env"), filepath.Join(dir, "gitconfig")
	git := `#!/bin/sh
audit() {
  home=nonempty
  [ -z "$(ls -A "$HOME")" ] && home=empty
  keys=missing
  if [ "${GIT_CONFIG_KEY_0-}" = credential.https://github.com.helper ] && [ "${GIT_CONFIG_VALUE_1-}" = '!legion git-credential' ] && [ "${GIT_CONFIG_KEY_2-}" = credential.https://github.com.useHttpPath ] && [ "${GIT_CONFIG_VALUE_2-}" = true ]; then
    keys=preserved
  fi
  printf '%s|%s|%s|%s|%s|%s|%s|%s|%s|%s\n' "${GIT_ASKPASS-unset}" "${SSH_ASKPASS-unset}" "${GH_TOKEN-unset}" "${GITHUB_TOKEN-unset}" "${GIT_CONFIG_GLOBAL-unset}" "${GIT_CONFIG_PARAMETERS-unset}" "$GIT_CONFIG_NOSYSTEM" "$GIT_CONFIG_COUNT" "$home" "$keys" >"$1"
}
if [ "$1" = config ]; then
  exec "$LIVE_SETUP_REAL_GIT" "$@"
fi
audit "$LIVE_SETUP_GIT_REMOTE_ENV"
printf '%s\n' "$PWD" >"$LIVE_SETUP_GIT_CWD"
printf '0123456789012345678901234567890123456789\tHEAD\n'
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(git), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(global, []byte("[credential]\n\thelper = !poisoned-global-helper\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEGION_E2E_MARKER", marker)
	t.Setenv("LEGION_ROLE", "architect")
	t.Setenv("POD_UID", "pod-1")
	t.Setenv("LEGION_GENERATION", "3")
	t.Setenv("LIVE_SETUP_GIT_CWD", gitCWD)
	t.Setenv("GIT_ASKPASS", "must-be-unset")
	t.Setenv("SSH_ASKPASS", "must-be-unset")
	t.Setenv("GH_TOKEN", "must-be-unset")
	t.Setenv("GITHUB_TOKEN", "must-be-unset")
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_PARAMETERS", "'http.extraheader'='Authorization: bearer fake-token'")
	for _, variable := range gitCredentialEnv() {
		t.Setenv(variable.Name, variable.Value)
	}
	t.Setenv("LIVE_SETUP_REAL_GIT", realGit)
	t.Setenv("LIVE_SETUP_GIT_REMOTE_ENV", gitRemoteEnv)
	agent := []string{"/bin/sh", "-c", `printf '%s\n' "$$" >"$1"; printf '%s:%s/%s\n' "$LEGION_ROLE" "$POD_UID" "$LEGION_GENERATION" >>"$LEGION_E2E_MARKER"`, "agent", agentPID}
	if got := runStart(t, dir, bin, "", nil, liveSetup(repo), agent); got.exit != 0 {
		t.Fatalf("the wrapper exited %d: %s", got.exit, got.stderr)
	}
	lines := readLines(t, marker)
	if len(lines) != 2 {
		t.Fatalf("marker lines = %q, want setup then agent", lines)
	}
	setup, err := parseSetupMarker(lines[0])
	if err != nil {
		t.Fatal(err)
	}
	pid, err := os.ReadFile(agentPID)
	if err != nil {
		t.Fatal(err)
	}
	if setup.ppid != strings.TrimSpace(string(pid)) {
		t.Errorf("setup parent pid %q, want agent pid %q", setup.ppid, strings.TrimSpace(string(pid)))
	}
	if setup.cwd != dir {
		t.Errorf("setup cwd %q, want start directory %q", setup.cwd, dir)
	}
	if setup.uid != strconv.Itoa(os.Getuid()) {
		t.Errorf("setup uid %q, want %d", setup.uid, os.Getuid())
	}
	if setup.gitFrom != "/tmp" || setup.git != "ok" {
		t.Errorf("setup git record = from %q, result %q; want /tmp and ok", setup.gitFrom, setup.git)
	}
	if setup.helper != "expected" || setup.helperCount != "1" || setup.helperEffective != "1" || setup.helperLines != "2" {
		t.Errorf("setup helper record = classification %q, count %q, effective %q, lines %q; want expected, one, one and two", setup.helper, setup.helperCount, setup.helperEffective, setup.helperLines)
	}
	if got, err := os.ReadFile(gitCWD); err != nil || strings.TrimSpace(string(got)) != "/tmp" {
		t.Errorf("git cwd = %q, %v; want /tmp", got, err)
	}
	if got, err := os.ReadFile(gitRemoteEnv); err != nil || strings.TrimSpace(string(got)) != "unset|unset|unset|unset|unset|unset|1|3|empty|preserved" {
		t.Errorf("git ls-remote environment = %q, %v; want the same neutralized credential environment", got, err)
	}
	if lines[1] != "architect:pod-1/3" {
		t.Errorf("agent marker = %q, want architect:pod-1/3", lines[1])
	}
}

func TestLiveSetupNeverRecordsAHelperValue(t *testing.T) {
	const poison = "not-a-real-token"
	repo, err := ghrepo.Parse("repo", "sjawhar/legion-smoke")
	if err != nil {
		t.Fatal(err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir, bin := t.TempDir(), t.TempDir()
	marker, agentPID := filepath.Join(dir, "marker"), filepath.Join(dir, "agent-pid")
	git := `#!/bin/sh
if [ "$1" = config ]; then
  exec "$LIVE_SETUP_REAL_GIT" "$@"
fi
printf '0123456789012345678901234567890123456789\tHEAD\n'
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(git), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEGION_E2E_MARKER", marker)
	t.Setenv("LEGION_ROLE", "architect")
	t.Setenv("POD_UID", "pod-1")
	t.Setenv("LEGION_GENERATION", "3")
	for _, variable := range gitCredentialEnv() {
		t.Setenv(variable.Name, variable.Value)
	}
	t.Setenv("GIT_CONFIG_VALUE_1", "!legion git-credential\tfuture="+poison)
	t.Setenv("LIVE_SETUP_REAL_GIT", realGit)
	agent := []string{"/bin/sh", "-c", `printf '%s\n' "$$" >"$1"; printf '%s:%s/%s\n' "$LEGION_ROLE" "$POD_UID" "$LEGION_GENERATION" >>"$LEGION_E2E_MARKER"`, "agent", agentPID}
	got := runStart(t, dir, bin, "", nil, liveSetup(repo), agent)
	if got.exit != 0 {
		t.Fatalf("the wrapper exited %d: %s", got.exit, got.stderr)
	}
	if strings.Contains(got.stderr, poison) {
		t.Fatalf("setup printed helper text %q: %q", poison, got.stderr)
	}
	line := readLines(t, marker)[0]
	if strings.Contains(line, poison) {
		t.Fatalf("setup marker leaks helper text %q: %q", poison, line)
	}
	setup, err := parseSetupMarker(line)
	if err != nil {
		t.Fatal(err)
	}
	if setup.helper != "other" || setup.helperCount != "1" || setup.helperEffective != "1" || setup.helperLines != "2" {
		t.Fatalf("setup classification = %q with count %q, effective %q and lines %q, want other, one, one and two", setup.helper, setup.helperCount, setup.helperEffective, setup.helperLines)
	}
	err = setupHelperProblem(setup)
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

func TestLiveSetupClassifiesGitCredentialEnv(t *testing.T) {
	for name, changed := range map[string]bool{
		"the runtime helper": false,
		"a changed helper":   true,
	} {
		t.Run(name, func(t *testing.T) {
			setup := runSetupWithGitCredentialEnv(t, changed)
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

func runSetupWithGitCredentialEnv(t *testing.T, changed bool) setupMarker {
	t.Helper()
	repo, err := ghrepo.Parse("repo", "sjawhar/legion-smoke")
	if err != nil {
		t.Fatal(err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir, bin := t.TempDir(), t.TempDir()
	marker := filepath.Join(dir, "marker")
	git := `#!/bin/sh
if [ "$1" = config ]; then
  exec "$LIVE_SETUP_REAL_GIT" "$@"
fi
printf '0123456789012345678901234567890123456789\tHEAD\n'
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(git), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEGION_E2E_MARKER", marker)
	t.Setenv("LEGION_ROLE", "architect")
	t.Setenv("POD_UID", "pod-1")
	t.Setenv("LEGION_GENERATION", "3")
	for _, variable := range gitCredentialEnv() {
		t.Setenv(variable.Name, variable.Value)
	}
	if changed {
		t.Setenv("GIT_CONFIG_VALUE_1", "!different git-credential")
	}
	t.Setenv("LIVE_SETUP_REAL_GIT", realGit)
	if got := runStart(t, dir, bin, "", nil, liveSetup(repo), []string{"/bin/true"}); got.exit != 0 {
		t.Fatalf("the wrapper exited %d: %s", got.exit, got.stderr)
	}
	line := readLines(t, marker)
	if len(line) != 1 {
		t.Fatalf("marker lines = %q, want its setup line alone", line)
	}
	setup, err := parseSetupMarker(line[0])
	if err != nil {
		t.Fatal(err)
	}
	return setup
}

func TestLiveSetupClassifiesEffectiveRepositoryHelpers(t *testing.T) {
	for name, tc := range map[string]struct {
		configure                       func(t *testing.T, clone string, realGit string)
		helper, count, effective, lines string
	}{
		"the live repository and runtime environment": {
			configure: func(t *testing.T, _ string, _ string) {
				for _, variable := range gitCredentialEnv() {
					t.Setenv(variable.Name, variable.Value)
				}
			},
			helper: "expected", count: "3", effective: "1", lines: "6",
		},
		"only the repository helper": {
			configure: func(t *testing.T, _ string, _ string) {
				t.Setenv("GIT_CONFIG_COUNT", "0")
			},
			helper: "other", count: "2", effective: "1", lines: "4",
		},
		"an extra command helper": {
			configure: func(t *testing.T, _ string, _ string) {
				for _, variable := range gitCredentialEnv() {
					t.Setenv(variable.Name, variable.Value)
				}
				t.Setenv("GIT_CONFIG_COUNT", "4")
				t.Setenv("GIT_CONFIG_KEY_3", "credential.https://github.com.helper")
				t.Setenv("GIT_CONFIG_VALUE_3", "!extra credential")
			},
			helper: "other", count: "4", effective: "2", lines: "7",
		},
		"a local non-legion helper cleared by command scope": {
			configure: func(t *testing.T, clone, realGit string) {
				for _, variable := range gitCredentialEnv() {
					t.Setenv(variable.Name, variable.Value)
				}
				if output, err := exec.Command(realGit, "-C", clone, "config", "--add", "credential.https://github.com.helper", "!local credential").CombinedOutput(); err != nil {
					t.Fatalf("add local helper: %v: %s", err, output)
				}
			},
			helper: "expected", count: "4", effective: "1", lines: "7",
		},
		"a generic command helper after legion's": {
			configure: func(t *testing.T, _ string, _ string) {
				for _, variable := range gitCredentialEnv() {
					t.Setenv(variable.Name, variable.Value)
				}
				t.Setenv("GIT_CONFIG_COUNT", "4")
				t.Setenv("GIT_CONFIG_KEY_3", "credential.helper")
				t.Setenv("GIT_CONFIG_VALUE_3", "!generic credential")
			},
			helper: "other", count: "4", effective: "2", lines: "7",
		},
	} {
		t.Run(name, func(t *testing.T) {
			setup := parseSetupMarkerOrFail(t, runSetupInConfiguredClone(t, tc.configure))
			if setup.helper != tc.helper || setup.helperCount != tc.count || setup.helperEffective != tc.effective || setup.helperLines != tc.lines {
				t.Errorf("setup classification = %q with count %q, effective %q and lines %q, want %q, %q, %q and %q", setup.helper, setup.helperCount, setup.helperEffective, setup.helperLines, tc.helper, tc.count, tc.effective, tc.lines)
			}
		})
	}
}

func parseSetupMarkerOrFail(t *testing.T, line string) setupMarker {
	t.Helper()
	setup, err := parseSetupMarker(line)
	if err != nil {
		t.Fatal(err)
	}
	return setup
}

// git runs credential.helper and credential.https://github.com.helper as one list for an
// https://github.com request, in its read order, so liveSetupScript must classify both keys
// together: two fake helpers that record only their own name show the order, and an empty
// command-scope url-specific value drops a local generic helper.
func TestGitCombinesBothHelperKeysInReadOrder(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ran := filepath.Join(dir, "ran")
	helper := func(name string) string {
		path := filepath.Join(dir, "helper-"+name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n[ \"$1\" = get ] && echo "+name+" >>"+ran+"\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	generic, specific := helper("generic"), helper("specific")
	fill := func(repo string, extra ...string) []string {
		t.Helper()
		if err := os.WriteFile(ran, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(realGit, "credential", "fill")
		cmd.Dir = repo
		cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\npath=acme/widgets.git\n\n")
		cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}, extra...)
		_ = cmd.Run()
		return readLinesOrNone(t, ran)
	}
	repo := func(name string, keys ...[2]string) string {
		path := filepath.Join(dir, name)
		if output, err := exec.Command(realGit, "init", "--quiet", path).CombinedOutput(); err != nil {
			t.Fatalf("init %s: %v: %s", name, err, output)
		}
		for _, kv := range keys {
			if output, err := exec.Command(realGit, "-C", path, "config", "--add", kv[0], kv[1]).CombinedOutput(); err != nil {
				t.Fatalf("config %s: %v: %s", kv[0], err, output)
			}
		}
		return path
	}
	genericFirst := repo("generic-first", [2]string{"credential.helper", generic}, [2]string{"credential.https://github.com.helper", specific})
	if got := fill(genericFirst); strings.Join(got, " ") != "generic specific" {
		t.Errorf("helpers run = %q, want generic then specific: git combines both keys in read order", got)
	}
	specificFirst := repo("specific-first", [2]string{"credential.https://github.com.helper", specific}, [2]string{"credential.helper", generic})
	if got := fill(specificFirst); strings.Join(got, " ") != "specific generic" {
		t.Errorf("helpers run = %q, want specific then generic", got)
	}
	reset := []string{"GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=credential.https://github.com.helper", "GIT_CONFIG_VALUE_0=", "GIT_CONFIG_KEY_1=credential.https://github.com.helper", "GIT_CONFIG_VALUE_1=" + specific}
	if got := fill(repo("reset", [2]string{"credential.helper", generic}), reset...); strings.Join(got, " ") != "specific" {
		t.Errorf("helpers run = %q, want specific alone: the empty command-scope value drops the local generic helper", got)
	}
}

func readLinesOrNone(t *testing.T, path string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(body))
}

func runSetupInConfiguredClone(t *testing.T, configure func(t *testing.T, clone string, realGit string)) string {
	t.Helper()
	repo, err := ghrepo.Parse("repo", "sjawhar/legion-smoke")
	if err != nil {
		t.Fatal(err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir, bin := t.TempDir(), t.TempDir()
	clone, marker := filepath.Join(dir, "clone"), filepath.Join(dir, "marker")
	if output, err := exec.Command(realGit, "init", "--quiet", clone).CombinedOutput(); err != nil {
		t.Fatalf("init clone: %v: %s", err, output)
	}
	if _, err := workspace.ConfigureRepositoryCredential(context.Background(), workspace.NewRunner(time.Minute, map[string]string{"git": realGit}), clone); err != nil {
		t.Fatalf("configure repository credential: %v", err)
	}
	git := `#!/bin/sh
if [ "$1" = config ]; then
  exec "$LIVE_SETUP_REAL_GIT" "$@"
fi
printf '0123456789012345678901234567890123456789\tHEAD\n'
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(git), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEGION_E2E_MARKER", marker)
	t.Setenv("LEGION_ROLE", "architect")
	t.Setenv("POD_UID", "pod-1")
	t.Setenv("LEGION_GENERATION", "3")
	t.Setenv("LIVE_SETUP_REAL_GIT", realGit)
	configure(t, clone, realGit)
	if got := runStart(t, clone, bin, "", nil, liveSetup(repo), []string{"/bin/true"}); got.exit != 0 {
		t.Fatalf("the wrapper exited %d: %s", got.exit, got.stderr)
	}
	lines := readLines(t, marker)
	if len(lines) != 1 {
		t.Fatalf("marker lines = %q, want its setup line alone", lines)
	}
	return lines[0]
}
