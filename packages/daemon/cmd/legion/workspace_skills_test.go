package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
)

// `workspace-init skills` checks everything before it touches the skills volume or its own TMPDIR:
// --repo, --dir, and a readable provisioning token. A refusal leaves no directory behind, which is
// what lets the kubelet retry the container on the same volume.
func TestWorkspaceInitSkillsRefusesBeforeItTouchesTheVolume(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills")
	for name, tc := range map[string]struct {
		args  []string
		token string
		says  string
	}{
		"no repository": {
			args: []string{"skills", "--dir", dir}, token: "set",
			says: `--repo must be "owner/name" (got "")`,
		},
		"a repository that is no owner/name": {
			args: []string{"skills", "--repo", "verification-skills", "--dir", dir}, token: "set",
			says: `--repo must be "owner/name"`,
		},
		"a relative directory": {
			args: []string{"skills", "--repo", winitRepo, "--dir", "skills"}, token: "set",
			says: `--dir must be an absolute path (got "skills")`,
		},
		"no provisioning token": {
			args: []string{"skills", "--repo", winitRepo, "--dir", dir},
			says: "LEGION_PROVISION_TOKEN_FILE is not set",
		},
		"a positional argument": {
			args: []string{"skills", "--repo", winitRepo, "--dir", dir, "extra"}, token: "set",
			says: `unexpected argument "extra"`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.token != "" {
				t.Setenv("LEGION_PROVISION_TOKEN_FILE", writeFile(t, "LEGION_PROVISION_TOKEN", "test-installation-token"))
			} else {
				fixtureenv.Unset(t, "LEGION_PROVISION_TOKEN_FILE")
			}
			code, stdout, stderr := runWorkspaceInitHere(tc.args)
			if code == 0 || !strings.Contains(stderr, tc.says) {
				t.Fatalf("exit %d, stdout %q, stderr %q; want a refusal naming %q", code, stdout, stderr, tc.says)
			}
			if _, err := os.Stat(dir); err == nil {
				t.Errorf("the refusal made the skills directory %s", dir)
			}
		})
	}
}

// `legion workspace-init --help` names skills beside the containers it runs with, so an operator
// reading the command list sees the third init container.
func TestWorkspaceInitNamesSkillsAmongItsSubcommands(t *testing.T) {
	code, stdout, stderr := runWorkspaceInitHere([]string{"--help"})
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout+stderr, "skills") {
		t.Errorf("the command list does not name skills:\n%s%s", stdout, stderr)
	}
}
