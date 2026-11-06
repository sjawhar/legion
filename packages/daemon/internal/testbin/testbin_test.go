package testbin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A Legion issue pod hands the test process its own repository (GIT_DIR, GIT_WORK_TREE), its git
// configuration (the GIT_CONFIG_* block, which carries a credential helper) and a JJ_CONFIG; a
// probe that inherited them would answer for that configuration rather than for the pinned jj's
// own defaults, which is what RequireJJFilters asks about. The test plants those variables and
// reads the command RequireJJFilters starts, which carries its own environment and its own
// working directory. It starts no jj at all, so it needs neither the pinned binary nor a
// fixtureenv.Guard in a TestMain of this package: reading what a command would inherit proves
// the same thing as running it, wherever the test runs.
func TestTheJJProbeTakesNoVariableOfTheProcess(t *testing.T) {
	planted := map[string]string{
		"GIT_DIR":            "/legion/repos/legion/.git",
		"GIT_WORK_TREE":      "/legion/workspaces/sjawhar/legion",
		"GIT_CONFIG_COUNT":   "1",
		"GIT_CONFIG_KEY_0":   "credential.helper",
		"GIT_CONFIG_VALUE_0": "!gh auth git-credential",
		"JJ_CONFIG":          "/legion/config/jj.toml",
	}
	for key, value := range planted {
		t.Setenv(key, value)
	}

	home := t.TempDir()
	probe := probeCommand(home)
	env := probe.Env
	if probe.Dir != home {
		t.Errorf("the probe runs in %s, want the empty home %s, since jj opens the closest .jj above the directory it runs in", probe.Dir, home)
	}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		if want, ok := planted[key]; ok && want == value {
			t.Errorf("the probe's environment holds the process's %s, want none of the process's", entry)
		}
	}
	if got := envValue(t, env, "HOME"); got != home {
		t.Errorf("the probe's HOME is %s, want the home it was given, %s", got, home)
	}
	config := envValue(t, env, "XDG_CONFIG_HOME")
	if !strings.HasPrefix(config, home+string(filepath.Separator)) {
		t.Errorf("the probe's XDG_CONFIG_HOME is %s, want a directory under %s", config, home)
	}
	jj := envValue(t, env, "JJ_CONFIG")
	if _, err := os.Stat(jj); !os.IsNotExist(err) {
		t.Errorf("the probe's JJ_CONFIG %s exists (%v), want a file that does not, so jj reads no user configuration", jj, err)
	}
}

// envValue is name's value among env, the last entry naming it, as exec.Cmd takes a key's last
// value; a name env does not hold fails the test, since every name this test reads is one the
// probe's environment must supply.
func envValue(t *testing.T, env []string, name string) string {
	t.Helper()
	for i := len(env) - 1; i >= 0; i-- {
		if value, ok := strings.CutPrefix(env[i], name+"="); ok {
			return value
		}
	}
	t.Fatalf("the probe's environment names no %s", name)
	return ""
}
