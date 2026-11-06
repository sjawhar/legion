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

// nearPtr is true when a holds a value within 1e-9 of want.
func nearPtr(a *float64, want float64) bool { return a != nil && near(*a, want) }

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
	if !nearPtr(d.CoresMean, 2) || !nearPtr(d.CPUS, 1200) || !nearPtr(d.CoresPeak5s, 2) {
		t.Fatalf("cores_mean %v, cpu_s %v, cores_peak_5s %v; want 2, 1200, 2", deref(d.CoresMean), deref(d.CPUS), deref(d.CoresPeak5s))
	}
	if !nearPtr(d.ThrottledS, 0) || !nearPtr(d.LimitCores, 16) || d.CPUMax != "1600000 100000" {
		t.Errorf("throttled_s %v, limit_cores %v (cpu_max %q); want 0 and 16", deref(d.ThrottledS), deref(d.LimitCores), d.CPUMax)
	}
	if d.WorkingSetBytesMax == nil || *d.WorkingSetBytesMax != 2<<30 || !nearPtr(d.WorkingSetBytesMean, 2<<30) {
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
		if !nearPtr(d.CoresMean, 2*595.0/600) || d.Restarts != 1 || len(d.Pods) != c.wantPods {
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
	if !nearPtr(d.CoresMean, 2) || !nearPtr(d.CoresPeak5s, 2) || d.WorkingSetBytesMax == nil || !nearPtr(d.WorkingSetBytesMean, 2<<30) {
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
	if !nearPtr(r.CoresPeak5s, 1) || r.Samples == 0 || cf.Failed {
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
	if d := cf.Roles["dispatch"]; !nearPtr(d.CoresMean, 2) {
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

// cgPiecewise is the dispatch role's samples every 5 s from `from` to `to`, on the 5 s grid of
// `from`: over each 5 s the usage rises by coresAt(the interval's start) cores. skip drops samples.
func cgPiecewise(from, to time.Time, coresAt func(time.Time) float64, skip func(time.Time) bool) []string {
	usage := int64(5e9)
	var out []string
	for at := from; !at.After(to); at = at.Add(5 * time.Second) {
		if at.After(from) {
			usage += int64(math.Round(coresAt(at.Add(-5*time.Second)) * 5e6))
		}
		if skip == nil || !skip(at) {
			out = append(out, cgLine(at, "dispatch", "dispatch-0", usage, 1000, "1600000 100000", 3<<30, 1<<30))
		}
	}
	return out
}

// replayCores is 0.5 cores before the window, `in` cores in it and 1 core after it.
func replayCores(in float64) func(time.Time) float64 {
	return func(at time.Time) float64 {
		switch {
		case at.Before(cgWinStart):
			return 0.5
		case at.Before(cgWinEnd):
			return in
		}
		return 1
	}
}

const (
	replayDocHead = "{\n  \"config\": {\n    \"step\": \"r16\",\n    \"source\": \"replay\"\n  },\n  \"replay\": "
	replayDocTail = ",\n  \"sse_bytes\": 7\n}\n"
)

// cgReplayDir is a step directory whose meta.json holds the test window and end, whose
// summary.json holds a replay block as loadgen run -allow-foreign writes it (cpu_source none, the
// CPU fields null) with rate factor 0.5 and the reasons given, and a samples file of lines.
func cgReplayDir(t *testing.T, lines []string, end time.Time, reasons ...string) (dir, samples string) {
	t.Helper()
	dir, samples = cgStepDir(t, lines)
	meta, err := json.Marshal(metaFile{Step: "r16", WindowStart: cgWinStart, WindowEnd: cgWinEnd, End: end})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), meta, 0o600); err != nil {
		t.Fatal(err)
	}
	rec := replayRecord{Inputs: replayInputs{RateFactor: 0.5, ReadMix: map[string]float64{"read": 1}}, WindowS: 600, CPUSource: "none",
		Writes: map[string]replayWriteCount{"plain": {Planned: 3, Sent: 3}}, SkippedMissingDep: map[string]int{},
		Timeouts: 1, InflightAtEnd: 21, Saturated: len(reasons) > 0, SaturatedReasons: append([]string{}, reasons...), Roster: 762}
	value, err := json.MarshalIndent(rec, "  ", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "summary.json"), []byte(replayDocHead+string(value)+replayDocTail), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, samples
}

// mergeReplay runs cgroup-merge on the replay step directory and reads summary.json back: its
// bytes and its replay block.
func mergeReplay(t *testing.T, dir, samples string) (string, replayRecord) {
	t.Helper()
	if err := runCgroupMerge([]string{dir, samples}); err != nil {
		t.Fatalf("cgroup-merge: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var sum summary
	if err := json.Unmarshal(b, &sum); err != nil || sum.Replay == nil {
		t.Fatalf("summary.json: %v, replay %v:\n%s", err, sum.Replay, b)
	}
	return string(b), *sum.Replay
}

// TestCgroupMergeFillsTheReplayBlock: Dispatch at 0.5 cores before the window, 4 in it and 1
// after it, meta.json's end 400 s after the window: dispatch_cpu_s 2,400, cores_full_rate 2400 /
// 600 / 0.5 = 8, idle_cores_before 0.5, idle-corrected (2400 - 0.5 × 600) / 600 / 0.5 = 7, and
// drain_cpu_s over 300 s (the cap, not 400) = 300, from the cgroup. loadgen's timeouts and
// inflight_at_end reasons stay, 4 cores of a 16-core limit adds no dispatch_cpu, every other byte
// of summary.json is kept, the drain's samples change no figure of cgroup.json, and a re-run
// writes the same file.
func TestCgroupMergeFillsTheReplayBlock(t *testing.T) {
	lines := cgPiecewise(cgWinStart.Add(-2*time.Minute), cgWinEnd.Add(400*time.Second), replayCores(4), nil)
	dir, samples := cgReplayDir(t, lines, cgWinEnd.Add(400*time.Second), saturatedTimeouts, saturatedInflight)
	text, rec := mergeReplay(t, dir, samples)
	if rec.CPUSource != "cgroup" || !nearPtr(rec.DispatchCPUS, 2400) || !nearPtr(rec.CoresFullRate, 8) || rec.WindowS != 600 {
		t.Fatalf("cpu_source %q, dispatch_cpu_s %v, cores_full_rate %v, window_s %v; want cgroup, 2400, 8, 600",
			rec.CPUSource, deref(rec.DispatchCPUS), deref(rec.CoresFullRate), rec.WindowS)
	}
	if !nearPtr(rec.IdleCoresBefore, 0.5) || !nearPtr(rec.CoresFullRateIdleCorrected, 7) || !nearPtr(rec.DrainCPUS, 300) {
		t.Errorf("idle_cores_before %v, cores_full_rate_idle_corrected %v, drain_cpu_s %v; want 0.5, 7, 300",
			deref(rec.IdleCoresBefore), deref(rec.CoresFullRateIdleCorrected), deref(rec.DrainCPUS))
	}
	if !rec.Saturated || strings.Join(rec.SaturatedReasons, ",") != "timeouts,inflight_at_end" {
		t.Errorf("saturated %v %v; want true with loadgen's timeouts,inflight_at_end alone", rec.Saturated, rec.SaturatedReasons)
	}
	if rec.Roster != 762 || rec.Writes["plain"].Sent != 3 || rec.Inputs.ReadMix["read"] != 1 {
		t.Errorf("fields cgroup-merge does not fill changed: %+v", rec)
	}
	if !strings.HasPrefix(text, replayDocHead) || !strings.HasSuffix(text, replayDocTail) {
		t.Errorf("summary.json outside the replay block changed:\n%s", text)
	}
	cf, err := readCgroupFile(dir)
	if err != nil || cf == nil {
		t.Fatalf("cgroup.json: %v", err)
	}
	// Samples at 12:00:00, :05, ..., 12:10:00: 121; the drain's are brackets only.
	if d := cf.Roles["dispatch"]; !nearPtr(d.CoresMean, 4) || !nearPtr(d.CoresPeak5s, 4) || d.Samples != 121 || d.MaxGapS > 5 {
		t.Errorf("cgroup.json dispatch: cores_mean %v, peak %v, samples %d, max gap %v; want 4, 4, 121, at most 5",
			deref(d.CoresMean), deref(d.CoresPeak5s), d.Samples, d.MaxGapS)
	}
	again, _ := mergeReplay(t, dir, samples)
	if again != text {
		t.Errorf("a re-run changed summary.json:\n%s\nthen\n%s", text, again)
	}
}

// TestCgroupMergeAddsDispatchCPUSaturation: 15 cores of a 16-core limit in every 5 s interval
// (at or over 0.9 × 16 = 14.4) adds dispatch_cpu before loadgen's own reasons, once on a re-run;
// 15 cores in 5 of the window's 120 intervals (4%, under 10%) adds none, and drops a dispatch_cpu
// reason an earlier merge left, keeping the step's timeouts reason.
func TestCgroupMergeAddsDispatchCPUSaturation(t *testing.T) {
	lines := cgPiecewise(cgWinStart.Add(-2*time.Minute), cgWinEnd.Add(time.Minute), replayCores(15), nil)
	dir, samples := cgReplayDir(t, lines, cgWinEnd.Add(time.Minute), saturatedInflight)
	_, rec := mergeReplay(t, dir, samples)
	if !rec.Saturated || strings.Join(rec.SaturatedReasons, ",") != "dispatch_cpu,inflight_at_end" {
		t.Fatalf("saturated %v %v; want dispatch_cpu,inflight_at_end", rec.Saturated, rec.SaturatedReasons)
	}
	if _, rec = mergeReplay(t, dir, samples); strings.Join(rec.SaturatedReasons, ",") != "dispatch_cpu,inflight_at_end" {
		t.Errorf("a re-run gave %v; want dispatch_cpu,inflight_at_end once", rec.SaturatedReasons)
	}

	burst := func(at time.Time) float64 {
		if !at.Before(cgWinStart) && at.Before(cgWinStart.Add(25*time.Second)) {
			return 15
		}
		return 2
	}
	lines = cgPiecewise(cgWinStart.Add(-2*time.Minute), cgWinEnd.Add(time.Minute), burst, nil)
	dir, samples = cgReplayDir(t, lines, cgWinEnd.Add(time.Minute), saturatedCPU, saturatedTimeouts)
	if _, rec = mergeReplay(t, dir, samples); !rec.Saturated || strings.Join(rec.SaturatedReasons, ",") != "timeouts" {
		t.Errorf("15 cores in 4%% of the intervals: saturated %v %v; want true with timeouts alone", rec.Saturated, rec.SaturatedReasons)
	}
	dir, samples = cgReplayDir(t, lines, cgWinEnd.Add(time.Minute), saturatedCPU)
	if _, rec = mergeReplay(t, dir, samples); rec.Saturated || len(rec.SaturatedReasons) != 0 {
		t.Errorf("15 cores in 4%% of the intervals and no loadgen reason: saturated %v %v; want false with none", rec.Saturated, rec.SaturatedReasons)
	}
}

// TestCgroupMergeLeavesTheReplaysUnmeasurableFieldsNull: a 60 s gap in the 30 s before the window
// leaves idle_cores_before and the idle-corrected cores null; samples that end 20 s before the
// window's end leave dispatch_cpu_s, cores_full_rate and drain_cpu_s null; a meta.json without an
// end leaves drain_cpu_s null; no Dispatch samples at all leave cpu_source none. Each null is
// written as null, never as 0.
func TestCgroupMergeLeavesTheReplaysUnmeasurableFieldsNull(t *testing.T) {
	gap := func(at time.Time) bool { return at.After(cgWinStart.Add(-61*time.Second)) && at.Before(cgWinStart) }
	lines := cgPiecewise(cgWinStart.Add(-2*time.Minute), cgWinEnd.Add(time.Minute), replayCores(4), gap)
	dir, samples := cgReplayDir(t, lines, cgWinEnd.Add(time.Minute))
	text, rec := mergeReplay(t, dir, samples)
	if rec.IdleCoresBefore != nil || rec.CoresFullRateIdleCorrected != nil || !strings.Contains(text, `"idle_cores_before": null`) {
		t.Errorf("a gap before the window: idle_cores_before %v, corrected %v; want both null", deref(rec.IdleCoresBefore), deref(rec.CoresFullRateIdleCorrected))
	}
	if rec.CPUSource != "cgroup" || !nearPtr(rec.DispatchCPUS, 2400) || !nearPtr(rec.DrainCPUS, 60) {
		t.Errorf("a gap before the window: cpu_source %q, dispatch_cpu_s %v, drain_cpu_s %v; want cgroup, 2400, 60", rec.CPUSource, deref(rec.DispatchCPUS), deref(rec.DrainCPUS))
	}

	lines = cgPiecewise(cgWinStart.Add(-2*time.Minute), cgWinEnd.Add(-20*time.Second), replayCores(4), nil)
	dir, samples = cgReplayDir(t, lines, cgWinEnd.Add(time.Minute))
	text, rec = mergeReplay(t, dir, samples)
	if rec.DispatchCPUS != nil || rec.CoresFullRate != nil || rec.DrainCPUS != nil || !nearPtr(rec.IdleCoresBefore, 0.5) {
		t.Errorf("samples that end before the window's end: dispatch_cpu_s %v, cores_full_rate %v, drain_cpu_s %v, idle %v; want null, null, null, 0.5",
			deref(rec.DispatchCPUS), deref(rec.CoresFullRate), deref(rec.DrainCPUS), deref(rec.IdleCoresBefore))
	}
	for _, field := range []string{"dispatch_cpu_s", "cores_full_rate", "drain_cpu_s"} {
		if !strings.Contains(text, `"`+field+`": null`) {
			t.Errorf("summary.json does not write %s as null:\n%s", field, text)
		}
	}

	lines = cgPiecewise(cgWinStart.Add(-2*time.Minute), cgWinEnd.Add(time.Minute), replayCores(4), nil)
	dir, samples = cgReplayDir(t, lines, time.Time{})
	if _, rec = mergeReplay(t, dir, samples); rec.DrainCPUS != nil || !nearPtr(rec.DispatchCPUS, 2400) {
		t.Errorf("meta.json without an end: drain_cpu_s %v, dispatch_cpu_s %v; want null and 2400", deref(rec.DrainCPUS), deref(rec.DispatchCPUS))
	}

	dir, samples = cgReplayDir(t, defaultSeries("postgres", 1).lines(nil, nil), cgWinEnd.Add(time.Minute), saturatedTimeouts)
	if _, rec = mergeReplay(t, dir, samples); rec.CPUSource != "none" || rec.DispatchCPUS != nil || strings.Join(rec.SaturatedReasons, ",") != "timeouts" {
		t.Errorf("no Dispatch samples: cpu_source %q, dispatch_cpu_s %v, reasons %v; want none, null, timeouts", rec.CPUSource, deref(rec.DispatchCPUS), rec.SaturatedReasons)
	}
}

// TestCgroupMergeLeavesProcReplaysAndOtherStepsAlone: a replay block with cpu_source proc (the
// local stack's /proc reads) and a summary.json without a replay block keep their bytes.
func TestCgroupMergeLeavesProcReplaysAndOtherStepsAlone(t *testing.T) {
	lines := cgPiecewise(cgWinStart.Add(-2*time.Minute), cgWinEnd.Add(time.Minute), replayCores(4), nil)
	for name, doc := range map[string]string{
		"proc":      replayDocHead + `{"inputs": {"rate_factor": 1}, "cpu_source": "proc", "dispatch_cpu_s": 12, "saturated": false, "saturated_reasons": []}` + replayDocTail,
		"no replay": `{"config": {"step": "1x"}, "sse_bytes": 3}` + "\n",
	} {
		dir, samples := cgReplayDir(t, lines, cgWinEnd.Add(time.Minute))
		path := filepath.Join(dir, "summary.json")
		if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := runCgroupMerge([]string{dir, samples}); err != nil {
			t.Fatalf("%s: cgroup-merge: %v", name, err)
		}
		if b, _ := os.ReadFile(path); string(b) != doc {
			t.Errorf("%s: summary.json changed:\n%s", name, b)
		}
	}
}

// deref is *p, or NaN for nil, for test messages.
func deref(p *float64) float64 {
	if p == nil {
		return math.NaN()
	}
	return *p
}
