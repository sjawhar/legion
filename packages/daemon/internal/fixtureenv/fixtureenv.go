// Package fixtureenv is the environment a test's real git and jj run in. The daemon's tests run
// inside Legion's own issue pods, whose environment names the pod's live workspace, its git
// configuration, its GitHub credential and the deployment's own session store; a test that
// inherited them could read or push the pod's own work, or read and write the conversation of
// every agent of the deployment. Guard takes them out of a test process's own environment, so
// every command it starts and everything it runs in-process inherits none; Isolated hands one
// command none of them and a home of the test's.
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

// placesSession is whether a variable places an Oh My Pi session. OMP_SESSION_SQL_DSN_FILE and
// OMP_SESSION_STORAGE are the two Oh My Pi reads to do that; in a Legion pod they name the
// deployment's live session database, which holds every agent's conversation, so a test that
// inherited them would read and write that store. OMP_SESSION_SQL_DSN carries the same URL by
// value, where a deployment exports the file's contents rather than its name. The three are named
// rather than an OMP_ or PI_ prefix taken because a test may set a variable of either prefix for
// the code under test to read: OMP_PROFILE (cmd/legion's probe-image, worker-shim and controller
// tests) and PI_CONFIG_DIR and PI_CODING_AGENT_DIR (cmd/legion's controller test).
func placesSession(key string) bool {
	return key == "OMP_SESSION_SQL_DSN_FILE" || key == "OMP_SESSION_STORAGE" ||
		key == "OMP_SESSION_SQL_DSN"
}

// guarded is every variable the guard takes out: the ones git and jj reach through, and the ones
// that place the session store.
func guarded(key string) bool {
	return reaches(key) || placesSession(key)
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
// SSH_ASKPASS) or places a session (OMP_SESSION_SQL_DSN_FILE, OMP_SESSION_STORAGE,
// OMP_SESSION_SQL_DSN) out of the test process's own environment, but the test inputs and the
// names in keep, and sets GIT_ALLOW_PROTOCOL=file. A package whose tests run git or jj — as child
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
		if !guarded(key) || testInput(key, keep) {
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
		if guarded(key) && !testInput(key, guardedKeep) && key != "GIT_ALLOW_PROTOCOL" && key != "JJ_CONFIG" {
			t.Errorf("the test process holds %s, which Guard takes out", key)
		}
	}
	if got := os.Getenv("GIT_ALLOW_PROTOCOL"); got != "file" {
		t.Errorf("GIT_ALLOW_PROTOCOL=%q, want file", got)
	}
}

// Isolated is the process's environment without anything that could lead git or jj to a real
// workspace, repository, remote or credential, or Oh My Pi to a real session store, and with home
// as the home, an empty user configuration and an identity of the test's. It keeps no test input
// either, so a fixture that needs one appends it after. It drops:
//
//   - LEGION_*: LEGION_WORKSPACE names the workspace a shim adopts and checks for unpushed work
//     (shim.rescue, shim.runAdoption); a test that leaves it out to see a refusal would otherwise
//     run on the pod's own.
//   - GIT_*, JJ_*, HOME, XDG_CONFIG_HOME: GIT_DIR and GIT_WORK_TREE name another repository
//     whatever directory a command runs in, a pod hands every process a GIT_CONFIG_* block of its
//     own, and a home carries configuration and an identity.
//   - GH_*, GITHUB_*, SSH_AUTH_SOCK, SSH_ASKPASS: a credential git could push with.
//   - OMP_SESSION_SQL_DSN_FILE, OMP_SESSION_STORAGE, OMP_SESSION_SQL_DSN: the session store Oh My
//     Pi places a session in, which in a Legion pod is the deployment's own, holding every agent's
//     conversation; a fixture that carried them would have whatever it starts read and write it.
//
// GIT_ALLOW_PROTOCOL=file lets git use no transport but local files, whatever a repository's own
// configuration allows (git-config(1), protocol.allow): a push or fetch that still named a network
// remote is refused before it connects. A caller's own JJ_USER or GIT_AUTHOR_NAME appended after
// these wins, as exec.Cmd takes the last value of a key.
func Isolated(home string) []string {
	env := slices.DeleteFunc(os.Environ(), func(entry string) bool {
		key, _, _ := strings.Cut(entry, "=")
		return guarded(key) || key == "HOME" || key == "XDG_CONFIG_HOME"
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
