package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// vmTestTarget is the target of these tests: two containers on one VM, so its samples carry the
// pod app-vm and the machine's own series the role vm-app-vm.
func vmTestTarget(t *testing.T) *vmTarget {
	t.Helper()
	target, err := parseVMTarget("loadtest@app-vm:dispatch=/sys/fs/cgroup/system.slice/dispatch.scope,postgres")
	if err != nil {
		t.Fatalf("parseVMTarget: %v", err)
	}
	return target
}

// vmReadingText is one of vm/sample.sh's readings, its lines in the order the script prints them.
func vmReadingText(role, container string, usage, throttled int64, cpuMax string, memory, inactive int64) string {
	return fmt.Sprintf("role %s\ncontainer %s\nusage_usec %d\nthrottled_usec %d\ncpu_max %s\nmemory_current %d\ninactive_file %d\nend\n",
		role, container, usage, throttled, cpuMax, memory, inactive)
}

// vmCollect parses text as one target's stream, stamping each reading with the next of ats.
func vmCollect(t *testing.T, target *vmTarget, text string, ats ...time.Time) []cgroupSample {
	t.Helper()
	i := 0
	now := func() time.Time {
		if i >= len(ats) {
			t.Fatalf("the stream has more readings than the %d clock readings the test gives", len(ats))
		}
		i++
		return ats[i-1]
	}
	var got []cgroupSample
	if err := parseVMReadings(strings.NewReader(text), target, now, func(s cgroupSample) { got = append(got, s) }); err != nil {
		t.Fatalf("parseVMReadings: %v", err)
	}
	return got
}

// TestVMSampleReadingIsACgroupSampleLine: a reading of a container and a reading of the machine
// become the exact cgroup.jsonl lines the cluster sampler writes, on the collector's clock.
func TestVMSampleReadingIsACgroupSampleLine(t *testing.T) {
	target := vmTestTarget(t)
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	text := vmReadingText("dispatch", "dispatch.scope", 123456789, 4321, "1600000 100000", 8_000_000, 500_000) +
		vmReadingText("vm-app-vm", "vm", 987654321, 0, "1600000 100000", 30_000_000, 2_000_000)
	got := vmCollect(t, target, text, at, at.Add(10*time.Millisecond))
	want := []string{
		`{"at":"2026-10-28T12:00:00Z","role":"dispatch","pod":"app-vm","container":"dispatch.scope","usage_usec":123456789,"throttled_usec":4321,"cpu_max":"1600000 100000","memory_current":8000000,"inactive_file":500000,"error":""}`,
		`{"at":"2026-10-28T12:00:00.01Z","role":"vm-app-vm","pod":"app-vm","container":"vm","usage_usec":987654321,"throttled_usec":0,"cpu_max":"1600000 100000","memory_current":30000000,"inactive_file":2000000,"error":""}`,
	}
	if len(got) != len(want) {
		t.Fatalf("the stream gave %d samples, want %d", len(got), len(want))
	}
	for i, s := range got {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != want[i] {
			t.Errorf("sample %d:\n got %s\nwant %s", i, b, want[i])
		}
		if !s.good() {
			t.Errorf("sample %d is not a good sample: %s", i, b)
		}
	}
}

// TestVMSampleUnreadableFileIsTheReadingsError: a cgroup file the VM cannot read is that
// reading's error and no number, as the cluster sampler writes one, and it is never dropped.
func TestVMSampleUnreadableFileIsTheReadingsError(t *testing.T) {
	target := vmTestTarget(t)
	at := time.Date(2026, 10, 28, 12, 0, 5, 0, time.UTC)
	text := "role postgres\ncontainer postgres\n" +
		"error /sys/fs/cgroup/postgres/memory.stat is not readable\n" +
		"usage_usec 5\nthrottled_usec 0\ncpu_max 400000 100000\nmemory_current 7\nend\n"
	got := vmCollect(t, target, text, at)
	if len(got) != 1 {
		t.Fatalf("the stream gave %d samples, want 1", len(got))
	}
	s := got[0]
	if s.good() {
		t.Fatalf("a reading whose file could not be read is a good sample: %+v", s)
	}
	if !strings.Contains(s.Error, "/sys/fs/cgroup/postgres/memory.stat is not readable") {
		t.Errorf("the error does not say which file could not be read: %q", s.Error)
	}
	if s.Role != "postgres" || s.Pod != "app-vm" || s.Container != "postgres" || !s.At.Equal(at) {
		t.Errorf("the failed read names the wrong series: %+v", s)
	}
	if s.UsageUsec != nil || s.ThrottledUsec != nil || s.MemoryCurrent != nil || s.InactiveFile != nil {
		t.Errorf("a failed read carries a number: %+v", s)
	}
}

// TestVMSampleIncompleteReadingIsAFailedRead: a reading the script ended without every number is
// a failed read naming what was missing, never a zero.
func TestVMSampleIncompleteReadingIsAFailedRead(t *testing.T) {
	target := vmTestTarget(t)
	at := time.Date(2026, 10, 28, 12, 0, 10, 0, time.UTC)
	got := vmCollect(t, target, "role postgres\ncontainer postgres\nusage_usec 5\nthrottled_usec 0\nend\n", at)
	if len(got) != 1 || got[0].good() {
		t.Fatalf("want one failed read, got %+v", got)
	}
	for _, want := range []string{"incomplete read", "memory_current=missing", "inactive_file=missing"} {
		if !strings.Contains(got[0].Error, want) {
			t.Errorf("the error %q does not name %q", got[0].Error, want)
		}
	}
}

// TestVMSampleRefusesAReadingItCannotPlace: a block that names no role, or a role this VM was
// never asked to sample, is a stream the collector does not understand, so it ends it (and its
// caller starts another ssh) rather than writing a line under a role no one asked for.
func TestVMSampleRefusesAReadingItCannotPlace(t *testing.T) {
	target := vmTestTarget(t)
	for _, tc := range []struct{ name, text, want string }{
		{"no role", "container x\nusage_usec 1\nend\n", "names no role"},
		{"another role", vmReadingText("nats", "nats", 1, 0, "max 100000", 2, 1), "was not asked to sample"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := parseVMReadings(strings.NewReader(tc.text), target, time.Now, func(cgroupSample) {
				t.Error("a reading the collector cannot place was written")
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error naming %q, got %v", tc.want, err)
			}
		})
	}
}

// TestVMSampleRefusesABadTarget: every -vm value the collector cannot sample is refused by name,
// before it opens any ssh.
func TestVMSampleRefusesABadTarget(t *testing.T) {
	for _, tc := range []struct{ spec, want string }{
		{"loadtest@app-vm", "names no role"},
		{":dispatch", "names no ssh target"},
		{"loadtest@:dispatch", "names no host"},
		{"app-vm:dispatch,", "has an empty role"},
		{"app-vm:dispatch=", "empty cgroup directory"},
		{"app-vm:dispatch,dispatch", "names the role dispatch twice"},
		{"app-vm:vm-app-vm", "role of the machine's own series"},
	} {
		if _, err := parseVMTarget(tc.spec); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parseVMTarget(%q): want an error naming %q, got %v", tc.spec, tc.want, err)
		}
	}
	var targets vmTargets
	if err := targets.Set("app-vm:dispatch"); err != nil {
		t.Fatal(err)
	}
	if err := targets.Set("other-vm:dispatch"); err == nil || !strings.Contains(err.Error(), "already samples") {
		t.Errorf("want a refusal of the same role on two VMs, got %v", err)
	}
	if err := targets.Set("app-vm:postgres"); err == nil || !strings.Contains(err.Error(), "names host app-vm twice") {
		t.Errorf("want a refusal of the same host twice, got %v", err)
	}
}

// TestVMSampleFileMergesIntoCgroupJSON: loadgen cgroup-merge reads the file this collector
// writes. The readings are fake, the file is the collector's own, and the merge gives the roles
// and the cores the pass rule judges: Dispatch's container at 2 cores of its 16, the VM itself at
// 4 cores of its 16 processors.
func TestVMSampleFileMergesIntoCgroupJSON(t *testing.T) {
	target := vmTestTarget(t)
	dir := t.TempDir()
	meta, err := json.Marshal(metaFile{Step: "vm", WindowStart: cgWinStart, WindowEnd: cgWinEnd})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), meta, 0o600); err != nil {
		t.Fatal(err)
	}
	samples := filepath.Join(dir, "cgroup.jsonl")
	f, err := os.OpenFile(samples, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s := &vmSampler{interval: 5 * time.Second, lg: log.New(io.Discard, "", 0), out: f}

	// The samples run from a minute before the window to half a minute after it, as the run's
	// sampler does, so good samples bracket both ends of the window.
	const dispatchCores, machineCores = 2.0, 4.0
	var text strings.Builder
	var ats []time.Time
	for at := cgWinStart.Add(-time.Minute); !at.After(cgWinEnd.Add(30 * time.Second)); at = at.Add(5 * time.Second) {
		elapsed := at.Sub(cgWinStart.Add(-time.Minute)).Seconds()
		text.WriteString(vmReadingText("dispatch", "dispatch.scope",
			int64(math.Round(dispatchCores*1e6*elapsed)), 1000, "1600000 100000", 8_000_000, 500_000))
		text.WriteString(vmReadingText(target.machineRole, "vm",
			int64(math.Round(machineCores*1e6*elapsed)), 0, "1600000 100000", 30_000_000, 2_000_000))
		ats = append(ats, at, at)
	}
	i := 0
	if err := parseVMReadings(strings.NewReader(text.String()), target, func() time.Time {
		i++
		return ats[i-1]
	}, s.write); err != nil {
		t.Fatalf("parseVMReadings: %v", err)
	}
	if s.written != len(ats) || s.failed != 0 {
		t.Fatalf("the collector wrote %d samples (%d failed), want %d and 0", s.written, s.failed, len(ats))
	}

	if err := runCgroupMerge([]string{dir, samples}); err != nil {
		t.Fatalf("cgroup-merge: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "cgroup.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cf cgroupFile
	if err := json.Unmarshal(b, &cf); err != nil {
		t.Fatal(err)
	}
	if cf.Failed {
		t.Fatalf("cgroup-merge marked the step failed: %v", cf.Failures)
	}
	for role, wantCores := range map[string]float64{"dispatch": dispatchCores, target.machineRole: machineCores} {
		got, ok := cf.Roles[role]
		if !ok {
			t.Fatalf("cgroup.json has no role %s: %s", role, b)
		}
		if got.CoresMean == nil || math.Abs(*got.CoresMean-wantCores) > 0.001 {
			t.Errorf("role %s cores_mean %s, want %.3f", role, fmtFloatPtr(got.CoresMean), wantCores)
		}
		if got.LimitCores == nil || *got.LimitCores != 16 {
			t.Errorf("role %s limit_cores %s, want 16", role, fmtFloatPtr(got.LimitCores))
		}
		if got.Pods[0] != "app-vm" {
			t.Errorf("role %s pods %v, want the VM's host", role, got.Pods)
		}
	}
}

// TestVMSampleRunsTheEmbeddedScript: the collector sends vm/sample.sh itself on the shell's
// standard input and reads back what that script prints. The ssh here is a shim that drops the
// target and runs the shell it was given on this machine, so the script, the arguments the
// collector gives it and the parser are proved together, against a prepared cgroup tree.
func TestVMSampleRunsTheEmbeddedScript(t *testing.T) {
	dir := t.TempDir()
	cgroup := filepath.Join(dir, "dispatch")
	if err := os.MkdirAll(cgroup, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{
		"cpu.stat":       "usage_usec 123456789\nthrottled_usec 4321\n",
		"cpu.max":        "1600000 100000\n",
		"memory.current": "8000000\n",
		"memory.stat":    "anon 5000000\ninactive_file 500000\n",
	} {
		if err := os.WriteFile(filepath.Join(cgroup, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	shim := filepath.Join(dir, "ssh-shim")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nshift\nexec \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "cgroup.jsonl")
	if err := runVMSample([]string{
		"-out", out, "-ssh", shim, "-interval", "200ms", "-until", "700ms",
		"-vm", "loadtest@app-vm:dispatch=" + cgroup,
	}); err != nil {
		t.Fatalf("vm-sample: %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var s cgroupSample
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			t.Fatalf("the collector wrote a line that is not a sample: %q: %v", line, err)
		}
		roles[s.Role]++
		if s.Role != "dispatch" { // the machine's reading needs this box's /proc, which is not every box's
			continue
		}
		if !s.good() {
			t.Fatalf("the script's reading of the prepared cgroup tree is a failed read: %q", s.Error)
		}
		if *s.UsageUsec != 123456789 || *s.ThrottledUsec != 4321 || s.CPUMax != "1600000 100000" ||
			*s.MemoryCurrent != 8000000 || *s.InactiveFile != 500000 || s.Pod != "app-vm" || s.Container != "dispatch" {
			t.Fatalf("the script's reading is not the prepared tree's counters: %s", line)
		}
	}
	if roles["dispatch"] < 2 || roles["vm-app-vm"] < 2 {
		t.Errorf("700 ms at a 200 ms interval gave %d container readings and %d machine readings, want at least two of each", roles["dispatch"], roles["vm-app-vm"])
	}
}

// TestVMSampleRestartsADeadSSH: an ssh that ends is started again, and the gap it left is an
// error reading of every series on the VM, never a series that closes in silence. The fake ssh
// prints one reading and exits, as a dropped connection does.
func TestVMSampleRestartsADeadSSH(t *testing.T) {
	dir := t.TempDir()
	calls := filepath.Join(dir, "ssh-calls")
	fake := filepath.Join(dir, "fake-ssh")
	script := "#!/bin/sh\ncat >/dev/null\necho \"$@\" >>" + calls + "\n" +
		"cat <<'EOF'\n" + vmReadingText("dispatch", "dispatch", 7, 0, "1600000 100000", 11, 1) + "EOF\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "cgroup.jsonl")
	if err := runVMSample([]string{
		"-out", out, "-ssh", fake, "-interval", "100ms", "-until", "1s",
		"-vm", "loadtest@app-vm:dispatch=/sys/fs/cgroup/dispatch",
	}); err != nil {
		t.Fatalf("vm-sample: %v", err)
	}

	callLog, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	ran := strings.Count(string(callLog), "\n")
	if ran < 2 {
		t.Fatalf("the ssh ran %d times in 1 s, so a dead one was not started again: %s", ran, callLog)
	}
	wantArgs := "loadtest@app-vm sh -s -- -i 0.1 -m vm-app-vm dispatch=/sys/fs/cgroup/dispatch"
	if first, _, _ := strings.Cut(string(callLog), "\n"); first != wantArgs {
		t.Errorf("the ssh ran %q, want %q", first, wantArgs)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	good, gaps := 0, map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var s cgroupSample
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			t.Fatalf("the collector wrote a line that is not a sample: %q: %v", line, err)
		}
		if s.good() {
			good++
			continue
		}
		if !strings.Contains(s.Error, "ended, so this series has a gap") {
			t.Fatalf("a failed read does not say the ssh ended: %q", s.Error)
		}
		gaps[s.Role+" "+s.Container]++
	}
	if good < 2 {
		t.Errorf("the file holds %d good samples of %d ssh runs, want one each", good, ran)
	}
	for _, series := range []string{"dispatch dispatch", "vm-app-vm vm"} {
		if gaps[series] < 2 {
			t.Errorf("series %q has %d error readings, want one per ended ssh (%d)", series, gaps[series], ran)
		}
	}
}
