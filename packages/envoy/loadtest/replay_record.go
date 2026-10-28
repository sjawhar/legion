package main

// The replay's per-run record: summary.json's `replay` block and meta.json's `replay` record. The
// field names and types of replayRecord and replayInputs are a contract another team's tool writes
// too: never rename them.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// replayInputs are the inputs a replay run's result depends on.
type replayInputs struct {
	RateFactor    float64            `json:"rate_factor"`
	ReadsPerWrite float64            `json:"reads_per_write"`
	ReadMix       map[string]float64 `json:"read_mix"`
	UploadShare   float64            `json:"upload_share"`
	RNGSeed       uint64             `json:"rng_seed"`
	FromS         float64            `json:"from_s"`
	ToS           float64            `json:"to_s"`
	// BundleSHA256 is the sha256 of the bundle's bundle.meta.json, which holds every other
	// file's sha256.
	BundleSHA256 string `json:"bundle_sha256"`
	// SeedDumpSHA256 is the restored seed dump's, from $LOADTEST_ROOT/replay/last-reset.json; ""
	// without that file.
	SeedDumpSHA256 string `json:"seed_dump_sha256"`
	// GOMAXPROCS is Dispatch's: from its environment, else the stack's setting.
	GOMAXPROCS int `json:"gomaxprocs"`
	// DispatchCommit is the commit $LOADTEST_ROOT/bin/envoy-dispatch.json says Dispatch was built
	// from.
	DispatchCommit string `json:"dispatch_commit"`
}

type replayWriteCount struct {
	Planned int `json:"planned"`
	Sent    int `json:"sent"`
	Failed  int `json:"failed"`
}

type replaySubstitutes struct {
	Quote int `json:"quote"`
	Edit  int `json:"edit"`
}

type replayEdits struct {
	Diff       int `json:"diff"`
	Substitute int `json:"substitute"`
}

// replayRecord is summary.json's replay block.
type replayRecord struct {
	Inputs  replayInputs `json:"inputs"`
	WindowS float64      `json:"window_s"`
	// CPUSource is "proc" (Dispatch's /proc/<pid>/stat), or "none" when no pid was readable
	// (-allow-foreign); the CPU fields are null then.
	CPUSource                  string   `json:"cpu_source"`
	DispatchCPUS               *float64 `json:"dispatch_cpu_s"`
	CoresFullRate              *float64 `json:"cores_full_rate"`
	IdleCoresBefore            *float64 `json:"idle_cores_before"`
	CoresFullRateIdleCorrected *float64 `json:"cores_full_rate_idle_corrected"`
	DrainCPUS                  *float64 `json:"drain_cpu_s"`
	// Writes are the write actions by kind; SkippedMissingDep the actions (reads included) not
	// sent because an object they name was never created.
	Writes            map[string]replayWriteCount `json:"writes"`
	SkippedMissingDep map[string]int              `json:"skipped_missing_dep"`
	Substitutes       replaySubstitutes           `json:"substitutes"`
	Edits             replayEdits                 `json:"edits"`
	InflightAtEnd     int                         `json:"inflight_at_end"`
	Timeouts          int                         `json:"timeouts"`
	Saturated         bool                        `json:"saturated"`
	SaturatedReasons  []string                    `json:"saturated_reasons"`
	Roster            int                         `json:"roster"`
}

// replayMetaRecord is meta.json's replay record.
type replayMetaRecord struct {
	Inputs replayInputs `json:"inputs"`
	// CgroupCPUMax is /sys/fs/cgroup/cpu.max; "" when absent.
	CgroupCPUMax string `json:"cgroup_cpu_max"`
	// HeapStartAt and HeapEndAt are when heap-start.pprof and heap-end.pprof were requested.
	HeapStartAt time.Time `json:"heap_start_at,omitzero"`
	HeapEndAt   time.Time `json:"heap_end_at,omitzero"`
	// DispatchBuild is $LOADTEST_ROOT/bin/envoy-dispatch.json (dispatch_commit, dispatch_tree,
	// dispatch_pprof_overlay, dispatch_binary_sha256, go_version, ...).
	DispatchBuild json.RawMessage `json:"dispatch_build,omitempty"`
}

// cpuReading is one read of Dispatch's utime+stime ticks.
type cpuReading struct {
	at    time.Time
	ticks float64
	ok    bool
}

// replayCPU are the run's reads: before the idle interval, at the window start and end, and once
// the drain ends.
type replayCPU struct {
	idle, start, end, drain cpuReading
}

// Saturation reasons.
const (
	saturatedCPU      = "dispatch_cpu"
	saturatedTimeouts = "timeouts"
	saturatedInflight = "inflight_at_end"
)

// collectReplayMeta records the inputs only the stack knows: the seed dump, Dispatch's GOMAXPROCS
// and build, the cgroup's CPU limit.
func collectReplayMeta(in replayInputs, pid int, root string) replayMetaRecord {
	m := replayMetaRecord{}
	if b, err := os.ReadFile(filepath.Join(root, "replay", "last-reset.json")); err == nil {
		var r struct {
			SeedDump string `json:"seed_dump_sha256"`
			Dump     string `json:"dump_sha256"`
		}
		if json.Unmarshal(b, &r) == nil {
			in.SeedDumpSHA256 = r.SeedDump
			if in.SeedDumpSHA256 == "" {
				in.SeedDumpSHA256 = r.Dump
			}
		}
	}
	in.GOMAXPROCS = dispatchGOMAXPROCS(pid)
	if b, err := os.ReadFile(filepath.Join(root, "bin", "envoy-dispatch.json")); err == nil && json.Valid(b) {
		m.DispatchBuild = b
		var r struct {
			Commit string `json:"dispatch_commit"`
		}
		_ = json.Unmarshal(b, &r)
		in.DispatchCommit = r.Commit
	}
	if b, err := os.ReadFile("/sys/fs/cgroup/cpu.max"); err == nil {
		m.CgroupCPUMax = strings.TrimSpace(string(b))
	}
	m.Inputs = in
	return m
}

// dispatchGOMAXPROCS is GOMAXPROCS from Dispatch's environment, else the stack's setting
// (LOADTEST_GOMAXPROCS, default nproc, as stack.sh starts Dispatch).
func dispatchGOMAXPROCS(pid int) int {
	if pid > 0 {
		if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ"); err == nil {
			for kv := range strings.SplitSeq(string(b), "\x00") {
				if v, ok := strings.CutPrefix(kv, "GOMAXPROCS="); ok {
					if n, err := strconv.Atoi(v); err == nil && n > 0 {
						return n
					}
				}
			}
		}
	}
	if n, err := strconv.Atoi(os.Getenv("LOADTEST_GOMAXPROCS")); err == nil && n > 0 {
		return n
	}
	return runtime.NumCPU()
}

// replayCoresFullRate is Dispatch's cores at the full rate: CPU-seconds over the window, divided
// by the rate factor.
func replayCoresFullRate(cpuS, windowS, rate float64) float64 {
	return cpuS / windowS / rate
}

// replayIdleCorrected is replayCoresFullRate with the idle cores before the window taken off the
// window's CPU first.
func replayIdleCorrected(cpuS, idleCores, windowS, rate float64) float64 {
	return (cpuS - idleCores*windowS) / windowS / rate
}

// dispatchCoreSamples are Dispatch's cores over each interval between consecutive in-window 5 s
// samples.
func dispatchCoreSamples(ss []sample) []float64 {
	var out []float64
	var prev *sample
	for i := range ss {
		s := &ss[i]
		if !s.InWindow {
			continue
		}
		if _, ok := s.CPUTicks["dispatch"]; !ok {
			continue
		}
		if prev != nil {
			if dt := s.At.Sub(prev.At).Seconds(); dt > 0 {
				out = append(out, (s.CPUTicks["dispatch"]-prev.CPUTicks["dispatch"])/clkTck/dt)
			}
		}
		prev = s
	}
	return out
}

// replaySaturation says whether the run saturated and why: Dispatch's 5 s cores reached 0.9 x
// GOMAXPROCS in at least 10% of the window's samples, an action hit its deadline, or more than 20
// actions were in flight at the window end.
func replaySaturation(cores []float64, gomaxprocs, timeouts, inflightAtEnd int) (bool, []string) {
	reasons := []string{}
	if len(cores) > 0 && gomaxprocs > 0 {
		high := 0
		for _, c := range cores {
			if c >= 0.9*float64(gomaxprocs) {
				high++
			}
		}
		if float64(high) >= 0.1*float64(len(cores)) {
			reasons = append(reasons, saturatedCPU)
		}
	}
	if timeouts > 0 {
		reasons = append(reasons, saturatedTimeouts)
	}
	if inflightAtEnd > 20 {
		reasons = append(reasons, saturatedInflight)
	}
	return len(reasons) > 0, reasons
}

// record is the run's replay block, from its CPU reads, counters and the sampler's samples;
// windowS is the configured window, used when the /proc reads are missing.
func (rr *replayRun) record(ss []sample, windowS float64) *replayRecord {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	rec := &replayRecord{
		Inputs: rr.meta.Inputs, WindowS: windowS, CPUSource: "none", Writes: map[string]replayWriteCount{}, SkippedMissingDep: map[string]int{},
		Substitutes: rr.subs, Edits: rr.edits, InflightAtEnd: rr.inflightAtEnd, Timeouts: int(rr.timeouts.Load()), Roster: len(rr.setup.roster),
	}
	for k, w := range rr.writes {
		rec.Writes[k] = *w
	}
	for k, n := range rr.skipped {
		rec.SkippedMissingDep[k] = n
	}
	rate := rr.setup.opt.rate
	if c := rr.cpu; c.start.ok && c.end.ok {
		rec.CPUSource = "proc"
		if dt := c.end.at.Sub(c.start.at).Seconds(); dt > 0 {
			rec.WindowS = dt
		}
		cpu := (c.end.ticks - c.start.ticks) / clkTck
		cores := replayCoresFullRate(cpu, rec.WindowS, rate)
		rec.DispatchCPUS, rec.CoresFullRate = &cpu, &cores
		if dt := c.start.at.Sub(c.idle.at).Seconds(); c.idle.ok && dt > 0 {
			idle := (c.start.ticks - c.idle.ticks) / clkTck / dt
			corrected := replayIdleCorrected(cpu, idle, rec.WindowS, rate)
			rec.IdleCoresBefore, rec.CoresFullRateIdleCorrected = &idle, &corrected
		}
		if c.drain.ok {
			drain := (c.drain.ticks - c.end.ticks) / clkTck
			rec.DrainCPUS = &drain
		}
	}
	rec.Saturated, rec.SaturatedReasons = replaySaturation(dispatchCoreSamples(ss), rec.Inputs.GOMAXPROCS, rec.Timeouts, rec.InflightAtEnd)
	return rec
}
