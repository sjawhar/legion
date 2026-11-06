// Package fixtureenv is the environment a test's real git and jj run in. The daemon's tests run
// inside Legion's own issue pods, whose environment names the pod's live workspace, its git
// configuration and its GitHub credential; a test that inherited them could read or push the
// pod's own work. Guard takes them out of a test process's own environment, so every command it
// starts and everything it runs in-process inherits none; Isolated hands one command none of them
// and a home of the test's.
package fixtureenv

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// reaches is whether a variable can lead git or jj to a workspace, repository, remote or
// credential other than the test's own (Isolated's doc says how each does), HOME and
// XDG_CONFIG_HOME aside.
func reaches(key string) bool {
	for _, prefix := range []string{"LEGION_", "GIT_", "JJ_", "GH_", "GITHUB_"} {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return key == "SSH_AUTH_SOCK" || key == "SSH_ASKPASS"
}

// testInput is whether a variable is one a person or CI sets for the tests themselves, which Guard
// keeps: the LEGION_TEST_* and LEGION_E2E* services and binaries (LEGION_TEST_PG_DSN,
// LEGION_TEST_OMP), and GITHUB_ACTIONS, which turns a missing prerequisite's skip into a failure.
func testInput(key string, keep []string) bool {
	return strings.HasPrefix(key, "LEGION_TEST_") || strings.HasPrefix(key, "LEGION_E2E") ||
		key == "GITHUB_ACTIONS" || slices.Contains(keep, key)
}

// guardedKeep is what the last Guard kept beyond the test inputs; nil until a Guard ran.
var guardedKeep []string

// unguarded is the process's own KEY=value entry, before the first Guard, of every variable that
// Guard took out or set, for Live.
var unguarded []string

// Guard takes every variable that reaches (LEGION_*, GIT_*, JJ_*, GH_*, GITHUB_*, SSH_AUTH_SOCK,
// SSH_ASKPASS) out of the test process's own environment, but the test inputs and the names in
// keep, and sets GIT_ALLOW_PROTOCOL=file. A package whose tests run git or jj — as child
// processes, with no Env of their own, or in-process through the code under test — calls it in
// TestMain before m.Run, after the branch where the test binary runs as a helper child: that child
// keeps the variables its test hands it. Code under test that names its own transport
// (workspace's GIT_ALLOW_PROTOCOL=https) still does; a git that names none can reach no network
// remote. Guard does the same in every build, the e2e one included, whose untagged tests are the
// same fixtures; a live proof of that build, which reaches a real cluster and GitHub on purpose,
// takes the process's own environment back for its own run with Live.
func Guard(keep ...string) error {
	first := guardedKeep == nil
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !reaches(key) || testInput(key, keep) {
			continue
		}
		if first {
			unguarded = append(unguarded, entry)
		}
		if key == "GIT_ALLOW_PROTOCOL" {
			continue
		}
		if err := os.Unsetenv(key); err != nil {
			return fmt.Errorf("fixtureenv: unset %s: %w", key, err)
		}
	}
	if err := os.Setenv("GIT_ALLOW_PROTOCOL", "file"); err != nil {
		return fmt.Errorf("fixtureenv: set GIT_ALLOW_PROTOCOL: %w", err)
	}
	guardedKeep = append([]string{}, keep...)
	return nil
}

// Live gives the test the process's own values back of every variable Guard took out or set, and
// leaves GIT_ALLOW_PROTOCOL unset when the process had none; the guarded environment returns when
// the test ends. Only a live proof's entry point calls it: an e2e-tagged test that reaches a real
// cluster and GitHub with the operator's credentials and configuration. Like t.Setenv, it cannot
// run in a parallel test. Without a Guard it changes nothing.
func Live(t testing.TB) {
	t.Helper()
	if guardedKeep == nil {
		return
	}
	t.Setenv("GIT_ALLOW_PROTOCOL", "file")
	if err := os.Unsetenv("GIT_ALLOW_PROTOCOL"); err != nil {
		t.Fatalf("fixtureenv: unset GIT_ALLOW_PROTOCOL: %v", err)
	}
	for _, entry := range unguarded {
		key, value, _ := strings.Cut(entry, "=")
		t.Setenv(key, value)
	}
}

// RequireGuarded fails the test unless the package's TestMain ran Guard and the process holds
// none of the variables it takes out: the only GIT_ variable is GIT_ALLOW_PROTOCOL=file and the
// only JJ_ one is the JJ_CONFIG a TestMain may set after it.
func RequireGuarded(t testing.TB) {
	t.Helper()
	if guardedKeep == nil {
		t.Fatal("the package's TestMain never called fixtureenv.Guard")
	}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if reaches(key) && !testInput(key, guardedKeep) && key != "GIT_ALLOW_PROTOCOL" && key != "JJ_CONFIG" {
			t.Errorf("the test process holds %s, which Guard takes out", key)
		}
	}
	if got := os.Getenv("GIT_ALLOW_PROTOCOL"); got != "file" {
		t.Errorf("GIT_ALLOW_PROTOCOL=%q, want file", got)
	}
}

// Isolated is the process's environment without anything that could lead git or jj to a real
// workspace, repository, remote or credential, and with home as the home, an empty user
// configuration and an identity of the test's. It drops:
//
//   - LEGION_*: LEGION_WORKSPACE names the workspace a shim adopts and checks for unpushed work
//     (shim.rescue, shim.runAdoption); a test that leaves it out to see a refusal would otherwise
//     run on the pod's own.
//   - GIT_*, JJ_*, HOME, XDG_CONFIG_HOME: GIT_DIR and GIT_WORK_TREE name another repository
//     whatever directory a command runs in, a pod hands every process a GIT_CONFIG_* block of its
//     own, and a home carries configuration and an identity.
//   - GH_*, GITHUB_*, SSH_AUTH_SOCK, SSH_ASKPASS: a credential git could push with.
//
// GIT_ALLOW_PROTOCOL=file lets git use no transport but local files, whatever a repository's own
// configuration allows (git-config(1), protocol.allow): a push or fetch that still named a network
// remote is refused before it connects. A caller's own JJ_USER or GIT_AUTHOR_NAME appended after
// these wins, as exec.Cmd takes the last value of a key.
func Isolated(home string) []string {
	env := slices.DeleteFunc(os.Environ(), func(entry string) bool {
		key, _, _ := strings.Cut(entry, "=")
		return reaches(key) || key == "HOME" || key == "XDG_CONFIG_HOME"
	})
	env = append(env, Home(home)...)
	return append(env,
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL=file",
		"GIT_AUTHOR_NAME=Legion test", "GIT_AUTHOR_EMAIL=legion-test@example.invalid",
		"GIT_COMMITTER_NAME=Legion test", "GIT_COMMITTER_EMAIL=legion-test@example.invalid",
		"JJ_USER=Legion test", "JJ_EMAIL=legion-test@example.invalid",
	)
}

// Home is the home Isolated gives a command, as KEY=value entries: HOME, the config home under it
// (XDG_CONFIG_HOME, where jj keeps each repository's own configuration), and a JJ_CONFIG naming a
// file that does not exist, so jj reads no user configuration. A test whose code under test reads
// the process's own environment sets these on the process, so that code and its fixtures under
// Isolated of the same home read the same jj repository configuration.
func Home(home string) []string {
	return []string{
		"HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"JJ_CONFIG=" + filepath.Join(home, "no-jj-config.toml"),
	}
}
