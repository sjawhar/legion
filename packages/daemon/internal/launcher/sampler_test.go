package launcher

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/sampler"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// samplerStub stands in for this binary's own `sampler` command, which the launcher runs as a
// child of its own: a script that appends one line per start — its process id and the whole
// command line it was given — to a file, and then either lingers until it is killed or exits at
// once with the code it was left. It answers what the sampler answers, so the launcher's side of
// a sampler that runs, one that is killed and one that cannot stay up is what a test reads.
type samplerStub struct {
	t     *testing.T
	dir   string
	calls string
}

// samplerStart is one start the stub recorded: the process it ran as, which a test kills, and
// the arguments the launcher gave it, which carry no space, so the record is read back by words.
type samplerStart struct {
	pid  int
	args []string
}

// stubSampler puts that script where the launcher looks for this binary (samplerExecutable), so
// the launcher's own command line is what the stub records, and the test binary, which is not
// `legion`, is never run with a sampler's flags.
func stubSampler(t *testing.T) *samplerStub {
	t.Helper()
	stub := &samplerStub{t: t, dir: t.TempDir()}
	stub.calls = filepath.Join(stub.dir, "calls")
	program := filepath.Join(stub.dir, samplerProgram)
	script := "#!/bin/sh\nprintf '%s %s\\n' \"$$\" \"$*\" >> " + stub.calls + "\n" +
		"if [ \"$(cat " + filepath.Join(stub.dir, "linger") + ")\" = yes ]; then exec sleep 600; fi\n" +
		"exit $(cat " + filepath.Join(stub.dir, "exit") + ")\n"
	for _, file := range []struct {
		name string
		body string
		mode os.FileMode
	}{{program, script, 0o755}, {filepath.Join(stub.dir, "linger"), "yes\n", 0o644}, {filepath.Join(stub.dir, "exit"), "0\n", 0o644}} {
		if err := os.WriteFile(file.name, []byte(file.body), file.mode); err != nil {
			t.Fatal(err)
		}
	}
	samplerExecutable = func() (string, error) { return program, nil }
	t.Cleanup(func() { samplerExecutable = os.Executable })
	return stub
}

// exitsAtOnce leaves the stub a sampler that cannot stay up: every start exits with code the
// moment it has recorded itself.
func (s *samplerStub) exitsAtOnce(code int) {
	s.t.Helper()
	if err := os.WriteFile(filepath.Join(s.dir, "linger"), []byte("no\n"), 0o644); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.dir, "exit"), []byte(strconv.Itoa(code)+"\n"), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

// starts are the starts recorded so far.
func (s *samplerStub) starts() []samplerStart {
	s.t.Helper()
	raw, err := os.ReadFile(s.calls)
	if err != nil {
		return nil
	}
	var starts []samplerStart
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		words := strings.Fields(line)
		if len(words) == 0 {
			continue
		}
		pid, err := strconv.Atoi(words[0])
		if err != nil {
			s.t.Fatalf("the stub recorded %q, want its process id first", line)
		}
		starts = append(starts, samplerStart{pid: pid, args: words[1:]})
	}
	return starts
}

// await waits until the stub has recorded count starts and answers every start it holds.
func (s *samplerStub) await(count int) []samplerStart {
	s.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		starts := s.starts()
		if len(starts) >= count {
			return starts
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("the launcher started the sampler %d times, want %d", len(starts), count)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// samplerTestConfig is a pod's sampler as the daemon passes it: a port of its own, the pod's
// project and issue, one volume and one rule of the deployment's.
func samplerTestConfig() *SamplerConfig {
	return &SamplerConfig{
		Port: 9500, Project: "INFRA", Issue: "INFRA-1208",
		Volumes: []sampler.Volume{
			{Name: "workspace-tester", Paths: []string{"/home/legion/workspace"}, Medium: "disk", Role: "tester"},
			{Name: "cache", Paths: []string{"/var/cache/legion"}, Medium: "memory"},
		},
		Rules: []sampler.Rule{{Program: "bun", Arguments: []string{"test"}, Kind: sampler.KindTest}},
	}
}

// shortenSamplerInterval has the launcher start the sampler again at a pace a test can wait for.
func shortenSamplerInterval(t *testing.T) {
	t.Helper()
	samplerInterval = 50 * time.Millisecond
	t.Cleanup(func() { samplerInterval = 5 * time.Second })
}

// readSamplerRoles is the roles file as the sampler reads it.
func readSamplerRoles(t *testing.T, rolesDir string) sampler.Roles {
	t.Helper()
	raw, err := os.ReadFile(samplerRolesPath(rolesDir))
	if err != nil {
		t.Fatalf("the launcher wrote no roles file: %v", err)
	}
	var roles sampler.Roles
	if err := json.Unmarshal(raw, &roles); err != nil {
		t.Fatalf("the roles file %q is no map of role to process id: %v", raw, err)
	}
	return roles
}

// The launcher runs the pod's sampler as this binary's own command, with everything that pod's
// series need and the launcher alone knows: the port the daemon chose, the pod's project and
// issue, the two files the launcher writes, the deployment's own kind rules and the number of
// times the sampler has been started again, which is none on the first start.
func TestTheLauncherStartsTheSamplerWithThePodsOwnFlags(t *testing.T) {
	stub := stubSampler(t)
	// The sampler lingers, so this is the one start, and the interval is never reached.
	samplerInterval = time.Hour
	t.Cleanup(func() { samplerInterval = 5 * time.Second })
	rolesDir := t.TempDir()
	var log lockedBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := serveSampler(ctx, Config{RolesDir: rolesDir, Sampler: samplerTestConfig()}, &roleLog{stdout: &log, stderr: &log}, &pidGuard{})
	t.Cleanup(func() { cancel(); <-done })
	want := []string{
		"sampler",
		"--listen", "0.0.0.0:9500",
		"--project", "INFRA",
		"--issue", "INFRA-1208",
		"--roles-file", filepath.Join(rolesDir, SamplerDirName, samplerRolesName),
		"--volumes-file", filepath.Join(rolesDir, SamplerDirName, samplerVolumesName),
		"--rules", `[{"program":"bun","arguments":["test"],"kind":"test"}]`,
		"--restarts", "0",
	}
	if got := stub.await(1)[0].args; !slices.Equal(got, want) {
		t.Fatalf("the launcher ran the sampler as %q, want %q; its log: %s", got, want, log.String())
	}
}

// A sampler that dies is replaced: the launcher waits its interval and starts another, telling
// it how often it has had to, so the pod's own series say that its readings were interrupted.
func TestTheLauncherStartsTheSamplerAgainAfterItIsKilled(t *testing.T) {
	stub := stubSampler(t)
	shortenSamplerInterval(t)
	var log lockedBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := serveSampler(ctx, Config{RolesDir: t.TempDir(), Sampler: samplerTestConfig()}, &roleLog{stdout: &log, stderr: &log}, &pidGuard{})
	t.Cleanup(func() { cancel(); <-done })
	first := stub.await(1)[0]
	killed := time.Now()
	if err := syscall.Kill(first.pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill the sampler %d: %v", first.pid, err)
	}
	second := stub.await(2)[1]
	if waited := time.Since(killed); waited > 2*time.Second {
		t.Errorf("the launcher waited %s to start the sampler again, want about %s", waited, samplerInterval)
	}
	if second.pid == first.pid {
		t.Errorf("the second start is the first process %d again", first.pid)
	}
	if restarts := second.args[len(second.args)-2:]; !slices.Equal(restarts, []string{"--restarts", "1"}) {
		t.Errorf("the second sampler was told %q, want --restarts 1", restarts)
	}
	if !strings.Contains(log.String(), "launcher: sampler:") {
		t.Errorf("the container log holds %q, want the sampler's death named", log.String())
	}
}

// A sampler that cannot stay up costs the pod its readings and nothing else: every role of the
// container keeps running while the launcher starts another sampler and logs each failure, where
// a launcher that failed its container would cost the issue every role it serves.
func TestASamplerThatExitsAtOnceLeavesEveryRoleRunning(t *testing.T) {
	requireRoleNamespaces(t)
	stub := stubSampler(t)
	stub.exitsAtOnce(1)
	shortenSamplerInterval(t)
	f := newSampledFleet(t, 10*time.Second, samplerTestConfig(), "tester", "reviewer")
	const beating = `while :; do /bin/date +%s%N > /tmp/beat.tmp && /bin/mv /tmp/beat.tmp /tmp/beat; /bin/sleep 0.05; done`
	for i, role := range []string{"tester", "reviewer"} {
		if got := f.role(role).start(shimwire.LauncherStart{
			ID: "start-" + role, Generation: uint64(i + 1), Argv: []string{"/bin/sh", "-c", beating},
		}); !got.OK {
			t.Fatalf("start of %s = %#v; the launcher's log: %s", role, got, f.stderr.String())
		}
		f.waitForRoleFile(f.roleTmp(role, "beat"))
	}

	// Three starts is a sampler that failed, was waited for and was started again, twice over.
	stub.await(3)
	beats := map[string]string{}
	for _, role := range []string{"tester", "reviewer"} {
		beats[role] = f.waitForRoleFile(f.roleTmp(role, "beat"))
	}
	time.Sleep(500 * time.Millisecond)
	for _, role := range []string{"tester", "reviewer"} {
		if again := f.waitForRoleFile(f.roleTmp(role, "beat")); again == beats[role] {
			t.Fatalf("role %s stopped beating while the sampler failed: its beat stayed at %s", role, beats[role])
		}
	}
	if !strings.Contains(f.stderr.String(), "launcher: sampler:") {
		t.Errorf("the container log holds %q, want each sampler failure named", f.stderr.String())
	}
	f.role("tester").stop(1)
	f.role("reviewer").stop(2)
}

// The roles file is what the sampler reads every role's processes from: the role-init process id
// of each role whose generation is running, written where that process id changes, so a
// generation that has ended is gone from the file before the kernel can give its number away.
func TestTheRolesFileNamesEveryRolesRunningGeneration(t *testing.T) {
	requireRoleNamespaces(t)
	stubSampler(t)
	f := newSampledFleet(t, 10*time.Second, samplerTestConfig(), "tester", "reviewer")
	if roles := readSamplerRoles(t, f.rolesDir); len(roles) != 0 {
		t.Fatalf("a launcher that has started no generation wrote %v, want no role", roles)
	}

	running := map[string]int{}
	for i, role := range []string{"tester", "reviewer"} {
		if got := f.role(role).start(shimwire.LauncherStart{
			ID: "start-" + role, Generation: uint64(i + 1), Argv: []string{"/bin/sh", "-c", "exec /bin/sleep 600"},
		}); !got.OK {
			t.Fatalf("start of %s = %#v; the launcher's log: %s", role, got, f.stderr.String())
		}
		child := f.role(role).latest.Child
		if child == nil {
			t.Fatalf("role %s reported no child after its start; the launcher's log: %s", role, f.stderr.String())
		}
		running[role] = child.PID
	}
	if roles := readSamplerRoles(t, f.rolesDir); !maps.Equal(roles, running) {
		t.Fatalf("the roles file is %v, want each role's own role-init %v", roles, running)
	}

	f.role("tester").stop(1)
	f.role("tester").awaitExit(1)
	if roles := readSamplerRoles(t, f.rolesDir); !maps.Equal(roles, map[string]int{"reviewer": running["reviewer"]}) {
		t.Fatalf("the roles file is %v after the tester's generation ended, want the reviewer's alone", roles)
	}
	f.role("reviewer").stop(2)
}

// The volumes file is the pod's volumes as the daemon declared them, written once before any
// role starts: it is what the pod mounts and never changes while the pod lives, and a sampler
// started again reads the file that is there rather than one the launcher wrote over.
func TestTheVolumesFileIsWrittenOnceWithWhatTheConfigurationCarried(t *testing.T) {
	stub := stubSampler(t)
	stub.exitsAtOnce(0)
	shortenSamplerInterval(t)
	rolesDir := t.TempDir()
	cfg := Config{RolesDir: rolesDir, Sampler: samplerTestConfig()}
	var log lockedBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := serveSampler(ctx, cfg, &roleLog{stdout: &log, stderr: &log}, &pidGuard{})
	t.Cleanup(func() { cancel(); <-done })
	stub.await(1)
	path := samplerVolumesPath(rolesDir)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the launcher wrote no volumes file: %v", err)
	}
	var volumes []sampler.Volume
	if err := json.Unmarshal(raw, &volumes); err != nil {
		t.Fatalf("the volumes file %q is no list of volumes: %v", raw, err)
	}
	if !slices.EqualFunc(volumes, cfg.Sampler.Volumes, func(got, want sampler.Volume) bool {
		return got.Name == want.Name && got.Medium == want.Medium && got.Role == want.Role && slices.Equal(got.Paths, want.Paths)
	}) {
		t.Fatalf("the volumes file holds %#v, want %#v", volumes, cfg.Sampler.Volumes)
	}

	mark := []byte(`[{"name":"written-by-this-test","paths":["/marker"],"medium":"disk","role":""}]` + "\n")
	if err := os.WriteFile(path, mark, 0o644); err != nil {
		t.Fatal(err)
	}
	stub.await(3)
	if again, err := os.ReadFile(path); err != nil || string(again) != string(mark) {
		t.Fatalf("the volumes file is %q, %v after two more sampler starts; want it written once at the start", again, err)
	}
}

// The controller pod's launcher serves one role in a container with no roles directory, where
// there is no roles file to write and no pod of roles to measure, and runs no sampler whatever
// it was passed. Its absence is also how a pod rendered before the flag existed runs none.
func TestTheControllerPodsLauncherRunsNoSampler(t *testing.T) {
	stub := stubSampler(t)
	shortenSamplerInterval(t)
	var log lockedBuffer
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- Run(ctx, Config{
			// Port 1 is nothing's: the launcher dials it, is refused and dials again until its
			// context ends, which is the controller pod's launcher with no daemon up.
			Connect: "tcp://127.0.0.1:1", Sandbox: "legion-legion-legion-208", PodUID: "pod-1",
			Token: "token", Role: "controller", PrivateDir: t.TempDir(), Sampler: samplerTestConfig(),
			Stdout: &log, Stderr: &log,
		})
	}()
	time.Sleep(20 * samplerInterval)
	cancel()
	select {
	case <-result:
	case <-time.After(30 * time.Second):
		t.Fatalf("the launcher did not leave after its context ended; its log: %s", log.String())
	}
	if starts := stub.starts(); len(starts) != 0 {
		t.Fatalf("the controller pod's launcher ran the sampler %d times: %v", len(starts), starts)
	}
}
