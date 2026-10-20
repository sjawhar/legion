package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

func TestParseSetupMarker(t *testing.T) {
	line := "setup:architect:pod-1/3 cwd=/legion/workspaces/sjawhar/legion-smoke/s4a-1 uid=1000 ppid=41 git_from=/tmp helper=expected helper_count=1 helper_lines=2 git=ok"
	got, err := parseSetupMarker(line)
	if err != nil {
		t.Fatal(err)
	}
	if want := (setupMarker{
		role: "architect", incarnation: "pod-1/3", cwd: "/legion/workspaces/sjawhar/legion-smoke/s4a-1",
		uid: "1000", ppid: "41", gitFrom: "/tmp", helper: "expected", helperCount: "1", helperLines: "2", git: "ok",
	}); got != want {
		t.Errorf("parseSetupMarker(%q) = %#v, want %#v", line, got, want)
	}
}

func TestParseSetupMarkerRejectsMalformedRecords(t *testing.T) {
	for _, line := range []string{
		"architect:pod-1/3 cwd=/legion uid=1000 ppid=1 git_from=/tmp helper=expected helper_count=1 git=ok",
		"setup:architect:pod-1/3 cwd=/legion uid=1000 ppid=1 git_from=/tmp helper=expected",
		"setup:architect:pod-1/3 cwd=/legion uid=1000 ppid=1 git_from=/tmp helper=unknown helper_count=1 git=ok",
		"setup:architect:pod-1/3 cwd=/legion uid=1000 ppid=1 git_from=/tmp helper=expected helper_count=one git=ok",
		"setup:architect:pod-1/3 cwd=/legion uid=1000 ppid=1 git_from=/tmp helper=expected helper_count=1 git=ok extra=value",
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
	if setup.helper != "expected" || setup.helperCount != "1" || setup.helperLines != "2" {
		t.Errorf("setup helper record = classification %q, count %q, lines %q; want expected, one and two", setup.helper, setup.helperCount, setup.helperLines)
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
	if setup.helper != "other" || setup.helperCount != "1" || setup.helperLines != "2" {
		t.Fatalf("setup classification = %q with count %q and lines %q, want other, one and two", setup.helper, setup.helperCount, setup.helperLines)
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
			if setup.helper != want || setup.helperCount != "1" || setup.helperLines != "2" {
				t.Errorf("setup classification = %q with count %q and lines %q, want %q, one and two", setup.helper, setup.helperCount, setup.helperLines, want)
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
