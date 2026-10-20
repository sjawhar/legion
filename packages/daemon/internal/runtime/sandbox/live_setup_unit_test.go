package sandbox

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

func TestParseSetupMarker(t *testing.T) {
	line := "setup:architect:pod-1/3 cwd=/legion/workspaces/sjawhar/legion-smoke/s4a-1 uid=1000 ppid=41 git_from=/tmp helper_scope=command helper_b64=IWxlZ2lvbiBnaXQtY3JlZGVudGlhbA== helper_count=1 git=ok"
	got, err := parseSetupMarker(line)
	if err != nil {
		t.Fatal(err)
	}
	if want := (setupMarker{
		role: "architect", incarnation: "pod-1/3", cwd: "/legion/workspaces/sjawhar/legion-smoke/s4a-1",
		uid: "1000", ppid: "41", gitFrom: "/tmp", helperScope: "command",
		helperBase64: "IWxlZ2lvbiBnaXQtY3JlZGVudGlhbA==", helperCount: "1", git: "ok",
	}); got != want {
		t.Errorf("parseSetupMarker(%q) = %#v, want %#v", line, got, want)
	}
}

func TestParseSetupMarkerRejectsMalformedRecords(t *testing.T) {
	for _, line := range []string{
		"architect:pod-1/3 cwd=/legion uid=1000 ppid=1 git_from=/tmp git=ok",
		"setup:architect:pod-1/3 cwd=/legion uid=1000 ppid=1 git_from=/tmp",
		"setup:architect:pod-1/3 cwd=/legion uid=1000 ppid=1 git_from=/tmp git=maybe",
		"setup:architect:pod-1/3 cwd=/legion uid=1000 ppid=1 git_from=/tmp git=ok extra=value",
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
	dir, bin := t.TempDir(), t.TempDir()
	marker, agentPID, gitCWD, gitEnv := filepath.Join(dir, "marker"), filepath.Join(dir, "agent-pid"), filepath.Join(dir, "git-cwd"), filepath.Join(dir, "git-env")
	git := `#!/bin/sh
if [ "$1" = config ]; then
  home=nonempty
  [ -z "$(ls -A "$HOME")" ] && home=empty
  printf '%s|%s|%s|%s|%s|%s|%s\n' "${GIT_ASKPASS-unset}" "${SSH_ASKPASS-unset}" "${GH_TOKEN-unset}" "${GITHUB_TOKEN-unset}" "$GIT_CONFIG_NOSYSTEM" "$GIT_CONFIG_COUNT" "$home" >"$LIVE_SETUP_GIT_ENV"
  printf 'command\tcredential.https://github.com.helper\t\n'
  printf 'command\tcredential.https://github.com.helper\t!legion git-credential\n'
  exit 0
fi
printf '%s\n' "$PWD" >"$LIVE_SETUP_GIT_CWD"
printf '0123456789012345678901234567890123456789\tHEAD\n'
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(git), 0o700); err != nil {
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
	t.Setenv("GIT_CONFIG_COUNT", "3")
	t.Setenv("LIVE_SETUP_GIT_ENV", gitEnv)
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
	if setup.helperScope != "command" || setup.helperBase64 != "IWxlZ2lvbiBnaXQtY3JlZGVudGlhbA==" || setup.helperCount != "1" {
		t.Errorf("setup helper record = scope %q, helper %q, count %q; want command, !legion git-credential and one helper", setup.helperScope, setup.helperBase64, setup.helperCount)
	}
	if got, err := os.ReadFile(gitCWD); err != nil || strings.TrimSpace(string(got)) != "/tmp" {
		t.Errorf("git cwd = %q, %v; want /tmp", got, err)
	}
	if got, err := os.ReadFile(gitEnv); err != nil || strings.TrimSpace(string(got)) != "unset|unset|unset|unset|1|3|empty" {
		t.Errorf("git environment = %q, %v; want the neutralized askpass/token environment and empty temporary HOME", got, err)
	}
	if lines[1] != "architect:pod-1/3" {
		t.Errorf("agent marker = %q, want architect:pod-1/3", lines[1])
	}
}
