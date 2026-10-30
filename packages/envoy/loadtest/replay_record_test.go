package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestReplayCoresArithmetic(t *testing.T) {
	// 468 CPU-s over a 2,340 s window at F=0.1: 0.2 cores here, 2 at the full rate.
	if got := replayCoresFullRate(468, 2340, 0.1); !near(got, 2) {
		t.Fatalf("cores at full rate %v, want 2", got)
	}
	// Idle 0.05 cores takes 117 CPU-s off first: (468-117)/2340/0.1 = 1.5.
	if got := replayIdleCorrected(468, 0.05, 2340, 0.1); !near(got, 1.5) {
		t.Fatalf("idle-corrected %v, want 1.5", got)
	}
}

func TestReplayRecordFromReads(t *testing.T) {
	start := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	rr := &replayRun{
		setup:   &replaySetup{opt: replayOptions{rate: 0.5}, roster: make([]string, 7)},
		writes:  map[string]*replayWriteCount{rkReply: {Planned: 3, Sent: 2, Failed: 1}},
		skipped: map[string]int{rkReply: 1},
		meta:    replayMetaRecord{Inputs: replayInputs{RateFactor: 0.5, GOMAXPROCS: 2}},
		cpu: replayCPU{
			idle:  cpuReading{at: start.Add(-30 * time.Second), ticks: 1000, ok: true},
			start: cpuReading{at: start, ticks: 1300, ok: true},                        // 3 CPU-s over 30 s: 0.1 idle cores
			end:   cpuReading{at: start.Add(100 * time.Second), ticks: 6300, ok: true}, // 50 CPU-s over 100 s
			drain: cpuReading{at: start.Add(110 * time.Second), ticks: 6500, ok: true}, // 2 CPU-s
		},
	}
	rec := rr.record(nil, 99)
	check := func(name string, p *float64, want float64) {
		t.Helper()
		if p == nil || !near(*p, want) {
			t.Fatalf("%s %v, want %v", name, p, want)
		}
	}
	if rec.CPUSource != "proc" || !near(rec.WindowS, 100) || rec.Roster != 7 {
		t.Fatalf("source %s window %v roster %d", rec.CPUSource, rec.WindowS, rec.Roster)
	}
	check("dispatch_cpu_s", rec.DispatchCPUS, 50)
	check("cores_full_rate", rec.CoresFullRate, 1)
	check("idle_cores_before", rec.IdleCoresBefore, 0.1)
	check("cores_full_rate_idle_corrected", rec.CoresFullRateIdleCorrected, (50-10)/100.0/0.5)
	check("drain_cpu_s", rec.DrainCPUS, 2)
	if rec.Writes[rkReply] != (replayWriteCount{3, 2, 1}) || rec.SkippedMissingDep[rkReply] != 1 || rec.Saturated {
		t.Fatalf("writes %v skipped %v saturated %v", rec.Writes, rec.SkippedMissingDep, rec.Saturated)
	}

	// No pid: no CPU fields, the configured window.
	rr.cpu = replayCPU{}
	rec = rr.record(nil, 99)
	if rec.CPUSource != "none" || rec.DispatchCPUS != nil || rec.CoresFullRate != nil || rec.WindowS != 99 {
		t.Fatalf("without /proc: %+v", rec)
	}
	b, _ := json.Marshal(rec)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"inputs", "window_s", "cpu_source", "dispatch_cpu_s", "cores_full_rate", "idle_cores_before", "cores_full_rate_idle_corrected",
		"drain_cpu_s", "writes", "skipped_missing_dep", "substitutes", "edits", "inflight_at_end", "timeouts", "saturated", "saturated_reasons",
		"saturation_cores", "saturation_cores_from", "roster"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("summary.json replay block lacks %s", k)
		}
	}
}

func TestReplaySaturationRules(t *testing.T) {
	cores := func(high, low int) []float64 {
		var out []float64
		for range high {
			out = append(out, 1.8) // 0.9 x GOMAXPROCS 2
		}
		for range low {
			out = append(out, 0.5)
		}
		return out
	}
	for _, c := range []struct {
		name               string
		cores              []float64
		timeouts, inflight int
		want               []string
	}{
		{"idle", cores(0, 20), 0, 0, []string{}},
		{"9% of samples high", cores(9, 91), 0, 0, []string{}},
		{"10% of samples high", cores(10, 90), 0, 0, []string{saturatedCPU}},
		{"a timeout", cores(0, 20), 1, 0, []string{saturatedTimeouts}},
		{"20 in flight", cores(0, 20), 0, 20, []string{}},
		{"21 in flight", cores(0, 20), 0, 21, []string{saturatedInflight}},
		{"all three", cores(5, 5), 2, 30, []string{saturatedCPU, saturatedTimeouts, saturatedInflight}},
	} {
		sat, reasons := replaySaturation(c.cores, 2, c.timeouts, c.inflight)
		if sat != (len(c.want) > 0) || !slices.Equal(reasons, c.want) {
			t.Fatalf("%s: saturated %v %v, want %v", c.name, sat, reasons, c.want)
		}
	}
	// The threshold's base is the least of GOMAXPROCS, nproc and the cgroup quota in cores.
	for _, c := range []struct {
		name       string
		gomaxprocs int
		nproc      int
		cpuMax     string
		wantCores  float64
		wantFrom   string
	}{
		{"GOMAXPROCS 3 on a 2-core box", 3, 2, "max 100000", 2, "nproc"},
		{"no cgroup file", 3, 2, "", 2, "nproc"},
		{"a 1.5-core quota below both", 3, 2, "150000 100000", 1.5, "cgroup_cpu_max"},
		{"a quota above both", 2, 2, "400000 100000", 2, "gomaxprocs"},
		{"GOMAXPROCS 1 on a 2-core box", 1, 2, "200000 100000", 1, "gomaxprocs"},
	} {
		cores, from := saturationCores(c.gomaxprocs, c.nproc, c.cpuMax)
		if !near(cores, c.wantCores) || from != c.wantFrom {
			t.Fatalf("%s: %v cores from %s, want %v from %s", c.name, cores, from, c.wantCores, c.wantFrom)
		}
	}
	// GOMAXPROCS 3 with nproc 2: a window at 1.9 cores in 10% of the samples is saturated, which
	// 0.9 x 3 = 2.7 cores never is on a 2-core box.
	satCores, _ := saturationCores(3, 2, "max 100000")
	at19 := func(high, low int) []float64 {
		var out []float64
		for range high {
			out = append(out, 1.9)
		}
		for range low {
			out = append(out, 0.5)
		}
		return out
	}
	if sat, reasons := replaySaturation(at19(10, 90), satCores, 0, 0); !sat || !slices.Equal(reasons, []string{saturatedCPU}) {
		t.Fatalf("1.9 cores in 10%% of samples at nproc 2: %v %v", sat, reasons)
	}
	if sat, _ := replaySaturation(at19(9, 91), satCores, 0, 0); sat {
		t.Fatalf("1.9 cores in 9%% of samples: saturated")
	}
	if sat, _ := replaySaturation(at19(10, 90), 3, 0, 0); sat {
		t.Fatalf("1.9 cores against GOMAXPROCS 3: saturated")
	}
	// The 5 s samples: Dispatch's cores between consecutive in-window samples.
	t0 := time.Now()
	ss := []sample{
		{At: t0, CPUTicks: map[string]float64{"dispatch": 0}},
		{At: t0.Add(5 * time.Second), CPUTicks: map[string]float64{"dispatch": 100}, InWindow: true},
		{At: t0.Add(10 * time.Second), CPUTicks: map[string]float64{"dispatch": 1000}, InWindow: true},
		{At: t0.Add(15 * time.Second), CPUTicks: map[string]float64{"dispatch": 1500}, InWindow: true},
	}
	if got := dispatchCoreSamples(ss); !slices.Equal(got, []float64{1.8, 1}) {
		t.Fatalf("core samples %v", got)
	}
}

func TestCollectReplayMeta(t *testing.T) {
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, "replay"), 0o755)
	_ = os.MkdirAll(filepath.Join(root, "bin"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "replay", "last-reset.json"), []byte(`{"seed_dump_sha256":"abc","checksum":{"md5":"def"}}`), 0o644)
	_ = os.WriteFile(filepath.Join(root, "bin", "envoy-dispatch.json"), []byte(`{"dispatch_commit":"35efd326","go_version":"go1.27.1"}`), 0o644)
	t.Setenv("LOADTEST_GOMAXPROCS", "3")
	m := collectReplayMeta(replayInputs{RateFactor: 0.1}, 0, root)
	if m.Inputs.SeedDumpSHA256 != "abc" || m.Inputs.DispatchCommit != "35efd326" || m.Inputs.GOMAXPROCS != 3 || m.Inputs.RateFactor != 0.1 || len(m.DispatchBuild) == 0 || !strings.Contains(string(m.LastReset), `"def"`) {
		t.Fatalf("meta %+v", m)
	}
	m = collectReplayMeta(replayInputs{}, 0, t.TempDir())
	if m.Inputs.SeedDumpSHA256 != "" || m.Inputs.DispatchCommit != "" || m.LastReset != nil {
		t.Fatalf("meta without files %+v", m.Inputs)
	}
}

func TestReplayTable(t *testing.T) {
	cpu, cores, corrected := 468.0, 2.0, 1.5
	steps := []stepData{
		{name: "routes"},
		{name: "replay-f0.1", sum: summary{Replay: &replayRecord{
			Inputs: replayInputs{RateFactor: 0.1, ReadsPerWrite: 1, UploadShare: 0.25}, WindowS: 2340, DispatchCPUS: &cpu, CoresFullRate: &cores,
			CoresFullRateIdleCorrected: &corrected, Saturated: true, SaturatedReasons: []string{saturatedTimeouts},
			Writes:      map[string]replayWriteCount{rkReply: {Planned: 10, Sent: 9, Failed: 1}, rkEdit: {Planned: 5, Sent: 5}},
			Substitutes: replaySubstitutes{Quote: 3, Edit: 2},
		}}},
	}
	md := renderTables(steps)
	want := "| replay-f0.1 | 0.100 | 1.00 | 0.25 | 2,340 | 468.0 | 2.00 | 1.50 | yes (timeouts) | 14 / 15 | 1 | 3 / 2 |"
	if !strings.Contains(md, "## Replay") || !strings.Contains(md, want) || strings.Contains(md, "| routes | 0") {
		t.Fatalf("Replay table:\n%s", md[strings.Index(md, "## Replay"):])
	}
	if strings.Contains(renderTables(steps[:1]), "## Replay") {
		t.Fatal("a Replay table without a replay step")
	}
}
