package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The window of the cgroup tests: 10 minutes from 12:00:00.
var (
	cgWinStart = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	cgWinEnd   = cgWinStart.Add(10 * time.Minute)
)

// cgSeries is a role's samples as the cluster sampler writes them: every 5 s from `from` to `to`
// (offset from the window's grid, so the window's ends fall between samples), usage at a constant
// cores rate from usage0, and a constant working set.
type cgSeries struct {
	role, pod     string
	from, to      time.Time
	cores         float64
	usage0        int64
	cpuMax        string
	memory, inact int64
}

// lines is the series as cgroup.jsonl lines, in the sampler's exact shape; skip drops the samples
// for which it is true and fail turns them into failed reads.
func (c cgSeries) lines(skip, fail func(time.Time) bool) []string {
	var out []string
	for at := c.from; !at.After(c.to); at = at.Add(5 * time.Second) {
		switch {
		case skip != nil && skip(at):
		case fail != nil && fail(at):
			out = append(out, fmt.Sprintf(`{"at":"%s","role":"%s","pod":"%s","container":"%s","error":"exec: container not found"}`,
				at.Format(time.RFC3339Nano), c.role, c.pod, c.role))
		default:
			usage := c.usage0 + int64(math.Round(c.cores*1e6*at.Sub(c.from).Seconds()))
			out = append(out, cgLine(at, c.role, c.pod, usage, 1000, c.cpuMax, c.memory, c.inact))
		}
	}
	return out
}

// cgLine is one good sample line.
func cgLine(at time.Time, role, pod string, usage, throttled int64, cpuMax string, memory, inactive int64) string {
	return fmt.Sprintf(`{"at":"%s","role":"%s","pod":"%s","container":"%s","usage_usec":%d,"throttled_usec":%d,"cpu_max":"%s","memory_current":%d,"inactive_file":%d}`,
		at.Format(time.RFC3339Nano), role, pod, role, usage, throttled, cpuMax, memory, inactive)
}

// cgStepDir is a step directory whose meta.json holds the test window, and a samples file of lines.
func cgStepDir(t *testing.T, lines []string) (dir, samples string) {
	t.Helper()
	dir = t.TempDir()
	meta, err := json.Marshal(metaFile{Step: "cg", WindowStart: cgWinStart, WindowEnd: cgWinEnd})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), meta, 0o600); err != nil {
		t.Fatal(err)
	}
	samples = filepath.Join(dir, "cgroup.jsonl")
	if err := os.WriteFile(samples, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, samples
}

// mergeLines runs loadgen cgroup-merge on lines and reads back cgroup.json.
func mergeLines(t *testing.T, lines []string) (cgroupFile, string) {
	t.Helper()
	dir, samples := cgStepDir(t, lines)
	if err := runCgroupMerge([]string{dir, samples}); err != nil {
		t.Fatalf("cgroup-merge: %v", err)
	}
	cf, err := readCgroupFile(dir)
	if err != nil || cf == nil {
		t.Fatalf("cgroup.json: %v", err)
	}
	return *cf, dir
}

// near is true when a holds a value within 1e-9 of want.
func near(a *float64, want float64) bool { return a != nil && math.Abs(*a-want) < 1e-9 }

// The default series: from 2 minutes before the window, 3 s off its grid, to 7 s after it.
func defaultSeries(role string, cores float64) cgSeries {
	return cgSeries{role: role, pod: role + "-0", from: cgWinStart.Add(-123 * time.Second), to: cgWinEnd.Add(7 * time.Second),
		cores: cores, usage0: 5e9, cpuMax: "1600000 100000", memory: 3 << 30, inact: 1 << 30}
}

// TestCgroupMergeInterpolatesTheWindowsEnds: 2 cores constant, sampled 3 s off the window's grid,
// gives 2.00 cores and 1,200 CPU-s over 600 s. The delta between the two bracketing samples alone
// would give 2.0167.
func TestCgroupMergeInterpolatesTheWindowsEnds(t *testing.T) {
	cf, _ := mergeLines(t, defaultSeries("dispatch", 2).lines(nil, nil))
	d, ok := cf.Roles["dispatch"]
	if !ok {
		t.Fatalf("no dispatch role: %+v", cf)
	}
	if !near(d.CoresMean, 2) || !near(d.CPUS, 1200) || !near(d.CoresPeak5s, 2) {
		t.Fatalf("cores_mean %v, cpu_s %v, cores_peak_5s %v; want 2, 1200, 2", deref(d.CoresMean), deref(d.CPUS), deref(d.CoresPeak5s))
	}
	if !near(d.ThrottledS, 0) || !near(d.LimitCores, 16) || d.CPUMax != "1600000 100000" {
		t.Errorf("throttled_s %v, limit_cores %v (cpu_max %q); want 0 and 16", deref(d.ThrottledS), deref(d.LimitCores), d.CPUMax)
	}
	if d.WorkingSetBytesMax == nil || *d.WorkingSetBytesMax != 2<<30 || !near(d.WorkingSetBytesMean, 2<<30) {
		t.Errorf("working set mean %v, max %v; want 2 GiB", deref(d.WorkingSetBytesMean), d.WorkingSetBytesMax)
	}
	// Samples at 12:00:02, :07, ..., 12:09:57 in the window: 120, never more than 5 s apart.
	if d.Samples != 120 || d.FailedReads != 0 || d.MaxGapS > 5 || d.Restarts != 0 {
		t.Errorf("samples %d, failed reads %d, max gap %v s, restarts %d; want 120, 0, at most 5, 0", d.Samples, d.FailedReads, d.MaxGapS, d.Restarts)
	}
	if cf.Failed || len(cf.Failures) != 0 || cf.WindowS != 600 {
		t.Errorf("failed %v %+v over %v s; want a 600 s window that did not fail", cf.Failed, cf.Failures, cf.WindowS)
	}
}

// TestCgroupMergeNeverTakesANegativeOrInflatedDeltaAcrossARestart: the container restarts mid
// window, its usage falling to near 0 (same pod) or a new pod starting with a larger counter; the
// series is split there, so the cores stay between 0 and the true 2.
func TestCgroupMergeNeverTakesANegativeOrInflatedDeltaAcrossARestart(t *testing.T) {
	restart := cgWinStart.Add(300 * time.Second)
	for _, c := range []struct {
		name     string
		pod      string
		usage0   int64
		wantPods int
	}{{"usage falls", "dispatch-0", 1000, 1}, {"new pod", "dispatch-1", 9e12, 2}} {
		before := defaultSeries("dispatch", 2)
		before.to = restart.Add(-3 * time.Second)
		after := defaultSeries("dispatch", 2)
		after.pod, after.usage0, after.from = c.pod, c.usage0, restart.Add(2*time.Second)
		cf, _ := mergeLines(t, append(before.lines(nil, nil), after.lines(nil, nil)...))
		d := cf.Roles["dispatch"]
		if d.CoresMean == nil || *d.CoresMean < 0 || *d.CoresMean > 2+1e-9 || d.CoresPeak5s == nil || *d.CoresPeak5s > 2+1e-9 {
			t.Errorf("%s: cores_mean %v, peak %v; want between 0 and 2", c.name, deref(d.CoresMean), deref(d.CoresPeak5s))
		}
		// Only the 5 s across the restart is lost: 595 of 600 s at 2 cores.
		if !near(d.CoresMean, 2*595.0/600) || d.Restarts != 1 || len(d.Pods) != c.wantPods {
			t.Errorf("%s: cores_mean %v, restarts %d, pods %q; want %.4f, 1 and %d pods", c.name, deref(d.CoresMean), d.Restarts, d.Pods, 2*595.0/600, c.wantPods)
		}
	}
}

// TestCgroupMergeMarksASixtySecondGapFailedAndKeepsOtherReasons: a 60 s gap in Postgres's samples
// fails the step; step-failed.json keeps the shell's restart reason, gains one sampler_gap reason,
// and a re-run does not add it again. The merge still exits 0.
func TestCgroupMergeMarksASixtySecondGapFailedAndKeepsOtherReasons(t *testing.T) {
	gap := func(at time.Time) bool {
		return at.After(cgWinStart.Add(180*time.Second)) && at.Before(cgWinStart.Add(242*time.Second))
	}
	lines := append(defaultSeries("dispatch", 2).lines(nil, nil), defaultSeries("postgres", 1).lines(gap, nil)...)
	dir, samples := cgStepDir(t, lines)
	restart := `{"reasons": [{"kind": "restart", "container": "envoy-dispatch", "reason": "OOMKilled"}], "step": "cg"}`
	if err := os.WriteFile(filepath.Join(dir, "step-failed.json"), []byte(restart), 0o644); err != nil {
		t.Fatal(err)
	}
	for run := 1; run <= 2; run++ {
		if err := runCgroupMerge([]string{dir, samples}); err != nil {
			t.Fatalf("run %d: a failed step exits non-zero: %v", run, err)
		}
	}
	cf, err := readCgroupFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !cf.Failed || len(cf.Failures) != 1 || cf.Failures[0].Role != "postgres" || cf.Roles["postgres"].MaxGapS < 60 {
		t.Fatalf("failed %v, failures %+v, postgres max gap %v s; want postgres failed over a 60 s gap", cf.Failed, cf.Failures, cf.Roles["postgres"].MaxGapS)
	}
	b, err := os.ReadFile(filepath.Join(dir, "step-failed.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Reasons []map[string]string `json:"reasons"`
		Step    string              `json:"step"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("step-failed.json: %v\n%s", err, b)
	}
	if len(doc.Reasons) != 2 || doc.Step != "cg" {
		t.Fatalf("step-failed.json holds %d reasons, step %q; want the restart and one sampler gap, step kept:\n%s", len(doc.Reasons), doc.Step, b)
	}
	if r := doc.Reasons[0]; r["kind"] != "restart" || r["reason"] != "OOMKilled" || r["container"] != "envoy-dispatch" {
		t.Errorf("the shell's restart reason changed: %v", r)
	}
	if r := doc.Reasons[1]; r["kind"] != "sampler_gap" || r["role"] != "postgres" || !strings.Contains(r["detail"], "over 30 s") {
		t.Errorf("the sampler gap reason: %v", r)
	}
}

// TestCgroupMergeDoesNotCountAFailedReadAsZero: failed reads in the middle of the window are counted
// as failed reads and leave the cores, the working set and the series unbroken; a run of them over
// 30 s is a gap.
func TestCgroupMergeDoesNotCountAFailedReadAsZero(t *testing.T) {
	short := func(at time.Time) bool {
		return !at.Before(cgWinStart.Add(100*time.Second)) && at.Before(cgWinStart.Add(115*time.Second))
	}
	cf, _ := mergeLines(t, defaultSeries("dispatch", 2).lines(nil, short))
	d := cf.Roles["dispatch"]
	if d.FailedReads != 3 || d.Samples != 117 || d.Restarts != 0 {
		t.Fatalf("failed reads %d, samples %d, restarts %d; want 3, 117, 0", d.FailedReads, d.Samples, d.Restarts)
	}
	if !near(d.CoresMean, 2) || !near(d.CoresPeak5s, 2) || d.WorkingSetBytesMax == nil || !near(d.WorkingSetBytesMean, 2<<30) {
		t.Errorf("cores_mean %v, peak %v, working set mean %v: a failed read entered them", deref(d.CoresMean), deref(d.CoresPeak5s), deref(d.WorkingSetBytesMean))
	}
	if cf.Failed {
		t.Errorf("a 20 s run of failed reads failed the step: %+v", cf.Failures)
	}

	long := func(at time.Time) bool {
		return !at.Before(cgWinStart.Add(100*time.Second)) && at.Before(cgWinStart.Add(140*time.Second))
	}
	if cf, _ := mergeLines(t, defaultSeries("dispatch", 2).lines(nil, long)); !cf.Failed {
		t.Errorf("a 45 s run of failed reads did not fail the step: max gap %v s", cf.Roles["dispatch"].MaxGapS)
	}
	allFailed := func(at time.Time) bool { return !at.Before(cgWinStart) }
	cf, _ = mergeLines(t, defaultSeries("dispatch", 2).lines(nil, allFailed))
	if d := cf.Roles["dispatch"]; !cf.Failed || d.Samples != 0 || d.CoresMean != nil || d.WorkingSetBytesMax != nil {
		t.Errorf("a role with only failed reads in the window: failed %v, samples %d, cores %v, working set max %v; want failed and no number",
			cf.Failed, d.Samples, deref(d.CoresMean), d.WorkingSetBytesMax)
	}
}

// TestCgroupMergeLeavesAnUnbracketedRoleNull: a role whose first sample is after window_start has
// no cores_mean, cpu_s or throttled_s; cpu.max "max" has no limit.
func TestCgroupMergeLeavesAnUnbracketedRoleNull(t *testing.T) {
	late := defaultSeries("runner", 1)
	late.from, late.cpuMax = cgWinStart.Add(4*time.Second), "max 100000"
	cf, _ := mergeLines(t, append(defaultSeries("dispatch", 2).lines(nil, nil), late.lines(nil, nil)...))
	r := cf.Roles["runner"]
	if r.CoresMean != nil || r.CPUS != nil || r.ThrottledS != nil || r.LimitCores != nil {
		t.Fatalf("runner: cores_mean %v, cpu_s %v, throttled_s %v, limit %v; want all null", deref(r.CoresMean), deref(r.CPUS), deref(r.ThrottledS), deref(r.LimitCores))
	}
	if !near(r.CoresPeak5s, 1) || r.Samples == 0 || cf.Failed {
		t.Errorf("runner: peak %v, samples %d, failed %v; want 1 core, samples, not failed", deref(r.CoresPeak5s), r.Samples, cf.Failed)
	}
}

// TestCgroupMergeReadsOnlyWhatTheStepNeedsAndRefusesUnreadableInput: samples of another step, hours
// before, change nothing; an unterminated last line (the sampler mid-write) is skipped; a line that
// is not a sample in the middle of the file is an error naming the file and line.
func TestCgroupMergeReadsOnlyWhatTheStepNeedsAndRefusesUnreadableInput(t *testing.T) {
	early := defaultSeries("dispatch", 7)
	early.from, early.to, early.usage0 = cgWinStart.Add(-3*time.Hour), cgWinStart.Add(-2*time.Hour), 1
	lines := append(early.lines(nil, nil), defaultSeries("dispatch", 2).lines(nil, nil)...)
	dir, samples := cgStepDir(t, lines)
	f, err := os.OpenFile(samples, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"at":"2026-10-28T12:10:12Z","role":"dispa`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := runCgroupMerge([]string{dir, samples}); err != nil {
		t.Fatalf("cgroup-merge: %v", err)
	}
	cf, _ := readCgroupFile(dir)
	if d := cf.Roles["dispatch"]; !near(d.CoresMean, 2) {
		t.Errorf("cores_mean %v, want 2: samples outside the step entered it", deref(d.CoresMean))
	}

	bad := append(defaultSeries("dispatch", 2).lines(nil, nil)[:5], "not json")
	dir, samples = cgStepDir(t, append(bad, defaultSeries("postgres", 1).lines(nil, nil)...))
	if err := runCgroupMerge([]string{dir, samples}); err == nil || !strings.Contains(err.Error(), samples+" line 6") {
		t.Errorf("a line that is not JSON: error %v, want one naming %s line 6", err, samples)
	}
	if err := runCgroupMerge([]string{dir, filepath.Join(dir, "absent.jsonl")}); err == nil {
		t.Errorf("an absent samples file did not fail")
	}
	if err := runCgroupMerge([]string{t.TempDir(), samples}); err == nil || !strings.Contains(err.Error(), "meta.json") {
		t.Errorf("a step directory without meta.json: error %v, want one naming meta.json", err)
	}
}

// deref is *p, or NaN for nil, for test messages.
func deref(p *float64) float64 {
	if p == nil {
		return math.NaN()
	}
	return *p
}
