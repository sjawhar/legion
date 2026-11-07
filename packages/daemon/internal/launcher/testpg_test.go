package launcher

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubTestPG puts a `legion-testpg` on PATH that appends one line per call — the subcommand, the
// DSN it was given and the directory it was told — to a file, and answers the exit code the file
// `exit` holds. It answers what the worker image's script answers, so the launcher's side is what
// the test reads.
func stubTestPG(t *testing.T) (calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	script := "#!/bin/sh\nprintf '%s %s %s\\n' \"$1\" \"$" + TestPGVariable + "\" \"$" + testPGDirVariable + "\" >> " + calls + "\nexit $(cat " + filepath.Join(dir, "exit") + ")\n"
	if err := os.WriteFile(filepath.Join(dir, testPGProgram), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "exit"), []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

// toldDSN starts generation with env as its start command's own Env, answers what TestPGVariable
// held in the child's environment, and stops the generation. A child that held no such variable
// answers "unset": the shell's `${name-unset}` answers the value for a name set to anything,
// including the empty string, and "unset" only for a name its environment does not hold.
func (g *rig) toldDSN(generation uint64, env []string) string {
	g.t.Helper()
	return g.told(generation, "${"+TestPGVariable+"-unset}", env, "")
}

// The launcher that serves the pod's test database names it to every generation it starts, over
// whatever DSN the daemon's start command carried, and a launcher that serves none takes the
// variable out of the generation's environment. The daemon and the worker image are deployed
// from two separately pinned builds and this launcher comes from the image, so the process that
// creates the database is the one that names it: a role is told a database its own pod's server
// holds whichever of the two builds is older. It reads whether this launcher was told to serve a
// database, never whether a server answers: the first `ensure` runs before any role and its
// failure is logged rather than thrown, so neither rig here runs a server at all.
func TestEveryGenerationIsToldTheDSNItsOwnLauncherServes(t *testing.T) {
	// What a daemon of another build sends: the database every image's server serves.
	const fromTheDaemon = "postgres://legion_test@127.0.0.1:5433/postgres?sslmode=disable"
	serving := newSessionRig(t, Config{TestPostgres: true})
	if got := serving.toldDSN(1, []string{TestPGVariable + "=" + fromTheDaemon}); got != TestPGDSN {
		t.Errorf("a generation whose start command carried %s=%q ran with %q, want this launcher's own %q",
			TestPGVariable, fromTheDaemon, got, TestPGDSN)
	}
	if got := serving.toldDSN(2, nil); got != TestPGDSN {
		t.Errorf("a generation whose start command named no DSN ran with %s=%q, want this launcher's own %q",
			TestPGVariable, got, TestPGDSN)
	}
	none := newSessionRig(t, Config{})
	if got := none.toldDSN(1, []string{TestPGVariable + "=" + fromTheDaemon}); got != "unset" {
		t.Errorf("a generation of a launcher that serves no test database ran with %s=%q, want no such variable",
			TestPGVariable, got)
	}
}

// The launcher serves the pod's test database before it accepts a role start: serveTestPG's first
// `ensure` has returned when it does, so a role that starts at once finds the server listening.
// Each tick asks again, which is what starts a postmaster that has died, and every call carries
// the DSN the roles are told and the cluster's directory under the roles directory.
func TestTheLauncherServesTheTestDatabaseBeforeAnyRoleAndKeepsAsking(t *testing.T) {
	calls := stubTestPG(t)
	testPGInterval = 20 * time.Millisecond
	t.Cleanup(func() { testPGInterval = 15 * time.Second })
	rolesDir := t.TempDir()
	var log bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	// The ticker goroutine is waited for before the cleanup above restores the interval, so no
	// read of it outlives this test.
	done := serveTestPG(ctx, Config{RolesDir: rolesDir, TestPostgres: true}, &roleLog{stdout: &log, stderr: &log}, &pidGuard{})
	t.Cleanup(func() { cancel(); <-done })
	first, err := os.ReadFile(calls)
	if err != nil {
		t.Fatalf("serveTestPG returned before its first ensure ran: %v", err)
	}
	want := "ensure " + TestPGDSN + " " + filepath.Join(rolesDir, TestPGDirName) + "\n"
	if string(first) != want {
		t.Errorf("the first call is %q, want %q", first, want)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		again, err := os.ReadFile(calls)
		if err == nil && strings.Count(string(again), "ensure ") > 1 {
			if got := strings.ReplaceAll(string(again), want, ""); got != "" {
				t.Errorf("a later call is %q, want every call to be %q", got, want)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the launcher asked once and never again: %q", again)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A script that fails is logged and nothing else: a pod whose test database cannot start still
// runs its roles, where a launcher that failed its container would cost the issue every role it
// serves.
func TestATestDatabaseThatCannotStartOnlyLogs(t *testing.T) {
	calls := stubTestPG(t)
	if err := os.WriteFile(filepath.Join(filepath.Dir(calls), "exit"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testPGInterval = time.Hour
	t.Cleanup(func() { testPGInterval = 15 * time.Second })
	// The log is read here while the script's output is still being copied into it: cmd.Wait
	// returns at WaitDelay with that copy goroutine still running, so the buffer the roleLog
	// writes through must be one that holds its own lock against this read.
	log := &lockedBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := serveTestPG(ctx, Config{RolesDir: t.TempDir(), TestPostgres: true}, &roleLog{stdout: log, stderr: log}, &pidGuard{})
	t.Cleanup(func() { cancel(); <-done })
	if !strings.Contains(log.String(), "launcher: test database:") {
		t.Errorf("the container log holds %q, want the refusal named", log.String())
	}
}

// The guard holds a running child out of the reaper's pass and forgets it once its own wait has
// read its exit, so nothing of the launcher's own is left in the skip list between calls.
func TestTheGuardForgetsAChildOnceItsWaitHasRead(t *testing.T) {
	calls := stubTestPG(t)
	guard := &pidGuard{}
	if err := runTestPG(context.Background(), Config{RolesDir: t.TempDir(), TestPostgres: true}, &roleLog{}, guard, "ensure"); err != nil {
		t.Fatalf("runTestPG: %v", err)
	}
	if held := guard.held(); len(held) != 0 {
		t.Errorf("the guard still holds %v", held)
	}
	if _, err := os.Stat(calls); err != nil {
		t.Errorf("the script never ran: %v", err)
	}
}

// One run of the script is bounded, so an `ensure` that hangs — a lock a dead holder still owns —
// costs a pod one wait and a log line, not its whole boot: the launcher's first `ensure` runs
// before any role starts, inside the daemon's boot timeout.
func TestATestDatabaseRunThatHangsIsBounded(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nsleep 30\n"
	if err := os.WriteFile(filepath.Join(dir, testPGProgram), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	testPGBound = 200 * time.Millisecond
	t.Cleanup(func() { testPGBound = 60 * time.Second })
	started := time.Now()
	err := runTestPG(context.Background(), Config{RolesDir: t.TempDir(), TestPostgres: true}, &roleLog{}, &pidGuard{}, "ensure")
	if err == nil || !strings.Contains(err.Error(), "did not finish within") {
		t.Fatalf("runTestPG = %v, want the bound named", err)
	}
	if waited := time.Since(started); waited > 10*time.Second {
		t.Errorf("runTestPG waited %s on a script that hangs", waited)
	}
}

// The image probe runs what a pod's launcher runs and then proves the DSN every role is told:
// check, ensure, a database made and dropped through it, and the server stopped, so the probe pod
// leaves nothing running. A step that fails is answered with its output, and the server is stopped
// all the same.
func TestCheckTestPostgresRunsEveryStepAndStopsTheServer(t *testing.T) {
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	for _, program := range []string{testPGProgram, "psql"} {
		script := "#!/bin/sh\nprintf '%s %s\\n' \"$(basename \"$0\")\" \"$*\" >> " + calls + "\nexit $(cat " + filepath.Join(dir, "exit") + ")\n"
		if err := os.WriteFile(filepath.Join(dir, program), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "exit"), []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := CheckTestPostgres(filepath.Join(dir, "cluster")); err != nil {
		t.Fatalf("CheckTestPostgres: %v", err)
	}
	ran, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		testPGProgram + " check\n", testPGProgram + " ensure\n",
		"psql -X -q " + TestPGDSN + " -c create database legion_probe_testpg strategy file_copy\n",
		"psql -X -q " + TestPGDSN + " -c drop database legion_probe_testpg with (force)\n",
		testPGProgram + " stop\n",
	} {
		if !strings.Contains(string(ran), want) {
			t.Errorf("the probe ran %q, want a %q among them", ran, want)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "exit"), []byte("3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(calls); err != nil {
		t.Fatal(err)
	}
	err = CheckTestPostgres(filepath.Join(dir, "cluster"))
	if err == nil || !strings.Contains(err.Error(), "the pod's own test database") {
		t.Fatalf("CheckTestPostgres on a failing step = %v, want the failure named", err)
	}
	if again, readErr := os.ReadFile(calls); readErr != nil || !strings.Contains(string(again), testPGProgram+" stop\n") {
		t.Errorf("a failed probe ran %q (%v), want the server stopped all the same", again, readErr)
	}
}
