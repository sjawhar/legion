package registry

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func registryFile(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "legion", FileName)
}

func entry(team string, pid int) Entry {
	return Entry{
		Team:       team,
		ConfigPath: "/srv/" + team + "/legion.yaml",
		PID:        pid,
		Port:       13370,
		Bind:       "127.0.0.1",
		StartedAt:  time.Date(2026, 9, 21, 2, 18, 0, 0, time.UTC),
	}
}

// mustClaim records an entry the way `legion start` does, and fails the test if the team is held:
// a fixture that could not be written is not a premise a test may carry on from.
func mustClaim(t *testing.T, path string, e Entry) {
	t.Helper()
	held, ok, err := Claim(path, e)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !ok {
		t.Fatalf("Claim refused %s: pid %d holds the team", e.Team, held.PID)
	}
}

// The Go daemon keeps its own registry file until Stage 7: legions.json is the shipped
// TypeScript daemon's, and a Go entry written there is a file of another shape to its reader.
func TestPathIsTheGoDaemonsOwnFileUnderTheStateHome(t *testing.T) {
	env := func(key string) string {
		if key == "XDG_STATE_HOME" {
			return "/var/lib/state"
		}
		return ""
	}
	if got, want := Path(env, "/home/legion"), "/var/lib/state/legion/legions-go.json"; got != want {
		t.Fatalf("Path = %q, want %q", got, want)
	}
}

// The shipped resolver takes XDG_STATE_HOME only when it is absolute (paths.ts:26-31).
func TestPathFallsBackToTheHomeStateDirectoryWhenTheStateHomeIsNotAbsolute(t *testing.T) {
	for _, stateHome := range []string{"", "relative/state"} {
		env := func(string) string { return stateHome }
		want := "/home/legion/.local/state/legion/legions-go.json"
		if got := Path(env, "/home/legion"); got != want {
			t.Errorf("Path with XDG_STATE_HOME=%q = %q, want %q", stateHome, got, want)
		}
	}
}

func TestReadIsEmptyWhereNoLegionEverStarted(t *testing.T) {
	entries, err := Read(registryFile(t))
	if err != nil {
		t.Fatalf("Read of a registry that does not exist yet: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("Read = %v, want no entries", entries)
	}
}

func TestClaimRecordsTheStartedLegion(t *testing.T) {
	path := registryFile(t)
	mustClaim(t, path, entry("LEGION", 4321))

	entries, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("Read = %v, want one entry", entries)
	}
	if entries[0] != entry("LEGION", 4321) {
		t.Fatalf("Read[0] = %+v, want %+v", entries[0], entry("LEGION", 4321))
	}
}

func TestClaimKeepsOneEntryPerTeam(t *testing.T) {
	path := registryFile(t)
	mustClaim(t, path, entry("LEGION", 4321))
	mustClaim(t, path, entry("WIDGETS", 99))
	mustClaim(t, path, entry("LEGION", 5555))

	found, ok, err := Find(path, "LEGION")
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if !ok {
		t.Fatal("Find did not find the restarted legion")
	}
	if found.PID != 5555 {
		t.Fatalf("Find pid = %d, want the restarted daemon's 5555", found.PID)
	}
	entries, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("Read = %v, want the two teams", entries)
	}
}

func TestRemoveIfTakesTheStoppedLegionOut(t *testing.T) {
	path := registryFile(t)
	mustClaim(t, path, entry("LEGION", 4321))
	mustClaim(t, path, entry("WIDGETS", 99))
	if err := RemoveIf(path, "LEGION", 4321); err != nil {
		t.Fatalf("RemoveIf: %v", err)
	}

	if _, ok, err := Find(path, "LEGION"); err != nil || ok {
		t.Fatalf("Find after RemoveIf = %v, %v, want not found", ok, err)
	}
	entries, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) != 1 || entries[0].Team != "WIDGETS" {
		t.Fatalf("Read = %v, want the other team alone", entries)
	}
}

func TestRemoveIfOfATeamThatIsNotThereLeavesTheRegistryAlone(t *testing.T) {
	path := registryFile(t)
	mustClaim(t, path, entry("WIDGETS", 99))
	if err := RemoveIf(path, "LEGION", 4321); err != nil {
		t.Fatalf("RemoveIf of an unregistered team: %v", err)
	}
	entries, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("Read = %v, want the untouched entry", entries)
	}
}

// The entry is a claim on the team, and the claim belongs to the process the entry names: a
// daemon that failed to start must not release the claim of the one that is serving. This is the
// hole that orphaned a running daemon from stop, status and legions.
func TestRemoveIfLeavesTheEntryAnotherProcessOwns(t *testing.T) {
	path := registryFile(t)
	mustClaim(t, path, entry("LEGION", 4321))
	if err := RemoveIf(path, "LEGION", 9999); err != nil {
		t.Fatalf("RemoveIf of an entry another pid owns: %v", err)
	}

	found, ok, err := Find(path, "LEGION")
	if err != nil || !ok {
		t.Fatalf("Find = %v, %v, want the entry still there", ok, err)
	}
	if found.PID != 4321 {
		t.Fatalf("entry pid = %d, want the running daemon's 4321", found.PID)
	}
}

// A half-written registry is a registry a `legion legions` reads as truth, so the file is
// replaced by rename and the temporary never left behind.
func TestClaimLeavesNoTemporaryFileBehind(t *testing.T) {
	path := registryFile(t)
	mustClaim(t, path, entry("LEGION", 4321))
	names, err := filepath.Glob(filepath.Join(filepath.Dir(path), "*"))
	if err != nil {
		t.Fatalf("glob the state directory: %v", err)
	}
	for _, name := range names {
		if strings.HasPrefix(filepath.Base(name), ".legions-go-") {
			t.Errorf("a temporary registry file survived the write: %s", name)
		}
	}
}

// The file is this daemon's own record: another shape is a bug to read out loud, not a file to
// silently reset.
func TestReadRefusesAFileOfAnotherShape(t *testing.T) {
	path := registryFile(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("make the state directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"LEGION":{"pid":1,"port":2}}`), 0o600); err != nil {
		t.Fatalf("write the registry: %v", err)
	}
	if _, err := Read(path); err == nil {
		t.Fatal("Read accepted a registry of the TypeScript daemon's shape")
	} else if !strings.Contains(err.Error(), path) {
		t.Fatalf("Read error does not name the file: %v", err)
	}
}

func TestClaimWritesTheEntryFieldsTheRegistryPromises(t *testing.T) {
	path := registryFile(t)
	mustClaim(t, path, entry("LEGION", 4321))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the registry file: %v", err)
	}
	var wire []map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("the registry is not a JSON array: %v\n%s", err, raw)
	}
	if len(wire) != 1 {
		t.Fatalf("registry = %v, want one entry", wire)
	}
	for _, key := range []string{"team", "configPath", "pid", "port", "bind", "startedAt"} {
		if _, ok := wire[0][key]; !ok {
			t.Errorf("entry has no %q field: %s", key, raw)
		}
	}
}

func TestAliveIsFalseForAReapedPid(t *testing.T) {
	done := exec.Command("/bin/true")
	if err := done.Run(); err != nil {
		t.Fatalf("run /bin/true: %v", err)
	}
	if Alive(done.Process.Pid) {
		t.Fatalf("pid %d is reported alive after it exited and was reaped", done.Process.Pid)
	}
}

func TestAliveIsTrueForARunningLegion(t *testing.T) {
	// A process named legion, which is what the registry's pids point at: the binary the proof
	// script builds to `$work/legion` and the one `legion restart` re-execs.
	pid := startProbe(t, filepath.Join(t.TempDir(), "legion"))
	if !Alive(pid) {
		t.Fatalf("pid %d is reported dead while the legion runs", pid)
	}
}

// Linux hands a pid out again, so a stale entry must not make an unrelated process look like a
// legion — `legion stop` and `legion restart` signal that pid.
func TestAliveIsFalseForAPidAnotherProcessReused(t *testing.T) {
	pid := startProbe(t, filepath.Join(t.TempDir(), "other-probe"))
	if Alive(pid) {
		t.Fatalf("pid %d belongs to another process and is reported as a live legion", pid)
	}
}

// The guard is the process's own name, not its arguments: this checkout lives at
// /home/ubuntu/src/legion, so `go build`, an editor and a jj invocation all carry "legion" in
// their command line, and a recycled pid belonging to one of them would take the SIGTERM
// `legion stop` sends.
func TestAliveIsFalseForAProcessWhosePathMerelyContainsLegion(t *testing.T) {
	pid := startProbe(t, filepath.Join(t.TempDir(), "src", "legion", "other-probe"))
	if Alive(pid) {
		t.Fatalf("pid %d is reported as a live legion because %q is in its path", pid, "legion")
	}
}

// startProbe runs a long-lived process at exactly the path given — a copy of `sleep`, because
// what Alive reads is argv[0], and a `#!/bin/sh` script's argv[0] is the shell, not the script.
// It returns the pid once /proc shows the exec has happened: os/exec hands back a pid before
// the child has finished exec'ing, and a process mid-exec has no command line yet.
func startProbe(t *testing.T, path string) int {
	t.Helper()
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatalf("find sleep to copy as the probe: %v", err)
	}
	body, err := os.ReadFile(sleep)
	if err != nil {
		t.Fatalf("read %s: %v", sleep, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("make the probe directory: %v", err)
	}
	if err := os.WriteFile(path, body, 0o700); err != nil {
		t.Fatalf("write the probe: %v", err)
	}

	probe := exec.Command(path, "300")
	if err := probe.Start(); err != nil {
		t.Fatalf("start the probe: %v", err)
	}
	t.Cleanup(func() {
		_ = probe.Process.Kill()
		_, _ = probe.Process.Wait()
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		cmdline, err := os.ReadFile("/proc/" + strconv.Itoa(probe.Process.Pid) + "/cmdline")
		if err == nil && len(cmdline) > 0 {
			return probe.Process.Pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("the probe at %s never exec'd", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The claim is one critical section or it is nothing. Two starts released together both read an
// empty registry, both write, and the loser's release then deletes the winner's entry — the
// serving daemon left with no record, invisible to stop, status and legions. Contenders are real
// processes named `legion`, because Alive is what decides who holds a team and it reads argv[0].
func TestClaimAdmitsOneLegionPerTeamUnderContention(t *testing.T) {
	const contenders = 8
	const rounds = 25

	pids := make([]int, contenders)
	for i := range pids {
		pids[i] = startProbe(t, filepath.Join(t.TempDir(), "legion"))
	}

	for round := range rounds {
		path := registryFile(t)
		release := make(chan struct{})
		held := make([]Entry, contenders)
		won := make([]bool, contenders)
		failed := make([]error, contenders)

		var racing sync.WaitGroup
		for i := range contenders {
			racing.Add(1)
			go func(i int) {
				defer racing.Done()
				<-release
				held[i], won[i], failed[i] = Claim(path, entry("LEGION", pids[i]))
			}(i)
		}
		close(release)
		racing.Wait()

		winners := 0
		winner := 0
		for i := range contenders {
			if failed[i] != nil {
				t.Fatalf("round %d: Claim by pid %d: %v", round, pids[i], failed[i])
			}
			if won[i] {
				winners++
				winner = pids[i]
			}
		}
		if winners != 1 {
			t.Fatalf("round %d: %d of %d contenders claimed the team, want exactly 1", round, winners, contenders)
		}

		entries, err := Read(path)
		if err != nil {
			t.Fatalf("round %d: Read: %v", round, err)
		}
		if len(entries) != 1 || entries[0].PID != winner {
			t.Fatalf("round %d: registry = %v, want the one entry of pid %d", round, entries, winner)
		}
		for i := range contenders {
			if !won[i] && held[i].PID != winner {
				t.Fatalf("round %d: pid %d was refused naming pid %d, want the winner %d",
					round, pids[i], held[i].PID, winner)
			}
		}
	}
}
