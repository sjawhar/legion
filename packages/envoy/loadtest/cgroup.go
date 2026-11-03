package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// cgroupIdleBefore is how far before window_start cgroup-merge keeps samples, so the replay's
// idle_cores_before, over the replayIdleSpan before window_start, has samples that bracket it.
const cgroupIdleBefore = 60 * time.Second

// replayIdleSpan is the span before window_start whose Dispatch cores are the replay's
// idle_cores_before (the warm-up's heartbeats and streams, no replay request).
const replayIdleSpan = 30 * time.Second

// replayDrainMax is the longest span after window_end that the replay's drain_cpu_s covers: up to
// meta.json's end, at most this long.
const replayDrainMax = 300 * time.Second

// cgroupMaxGap is the longest time in the window a role may go without a good sample; a longer
// gap marks the step failed (kind sampler_gap in step-failed.json).
const cgroupMaxGap = 30 * time.Second

// cgroupSample is one line of the cluster sampler's cgroup.jsonl: one container's cgroup v2
// counters at one 5 s sample, on the sampler's clock. A read that failed is a line with Error set
// and no number.
type cgroupSample struct {
	At            time.Time `json:"at"`
	Role          string    `json:"role"`
	Pod           string    `json:"pod"`
	Container     string    `json:"container"`
	UsageUsec     *int64    `json:"usage_usec"`
	ThrottledUsec *int64    `json:"throttled_usec"`
	CPUMax        string    `json:"cpu_max"`
	MemoryCurrent *int64    `json:"memory_current"`
	InactiveFile  *int64    `json:"inactive_file"`
	Error         string    `json:"error"`
}

// good is true for a sample that holds every number and no error; any other line is a failed read,
// never a zero.
func (s *cgroupSample) good() bool {
	return s.Error == "" && s.UsageUsec != nil && s.ThrottledUsec != nil && s.MemoryCurrent != nil && s.InactiveFile != nil
}

// workingSet is memory.current less memory.stat inactive_file, as the kubelet counts it, never
// below 0.
func (s *cgroupSample) workingSet() int64 { return max(0, *s.MemoryCurrent-*s.InactiveFile) }

func usageUsec(s *cgroupSample) int64     { return *s.UsageUsec }
func throttledUsec(s *cgroupSample) int64 { return *s.ThrottledUsec }

// roleSamples are one role's samples that a step needs, out of the whole run's file.
type roleSamples struct {
	// seen is true when the role has any line at or before the window end: it was sampled during
	// the step, so it must have good samples in its window.
	seen bool
	// before is the latest good sample before the kept span, after the earliest good sample after
	// it; in is every sample in the span, good or failed.
	before, after *cgroupSample
	in            []cgroupSample
}

// series is the role's kept samples in time order (in), its good samples with the bracketing ones
// (good), and the restart cuts of good (restartCuts).
func (rs *roleSamples) series() (in, good []cgroupSample, cut []bool) {
	in = slices.Clone(rs.in)
	slices.SortStableFunc(in, func(a, b cgroupSample) int { return a.At.Compare(b.At) })
	if rs.before != nil {
		good = append(good, *rs.before)
	}
	for i := range in {
		if in[i].good() {
			good = append(good, in[i])
		}
	}
	if rs.after != nil {
		good = append(good, *rs.after)
	}
	return in, good, restartCuts(good)
}

// roleSeries is a role's samples as its figures read them over a step's window: in, good and cut
// (series) and the 5 s cores (intervalCores).
type roleSeries struct {
	in, good []cgroupSample
	cut      []bool
	cores    []float64
}

// window is the role's roleSeries over the window from ws to we.
func (rs *roleSamples) window(ws, we time.Time) roleSeries {
	in, good, cut := rs.series()
	return roleSeries{in: in, good: good, cut: cut, cores: intervalCores(good, cut, ws, we)}
}

// readCgroupSamples streams the samples file at path and keeps, per role, the samples from `from`
// to `to` and the good samples that bracket that span; a role is seen when it has a line at or
// before windowEnd. A line that is not a sample is an error naming the file and line, except an
// unterminated last line, which the sampler may still be writing: it is skipped.
func readCgroupSamples(path string, from, windowEnd, to time.Time) (map[string]*roleSamples, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	roles := map[string]*roleSamples{}
	br := bufio.NewReaderSize(f, 1<<16)
	for n := 1; ; n++ {
		line, readErr := br.ReadBytes('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, fmt.Errorf("%s: %w", path, readErr)
		}
		if text := bytes.TrimSpace(line); len(text) > 0 {
			var s cgroupSample
			err := json.Unmarshal(text, &s)
			switch {
			case err != nil && readErr != nil:
				fmt.Fprintf(os.Stderr, "loadgen cgroup-merge: %s line %d is incomplete and not JSON; skipped (the sampler may be writing it)\n", path, n)
			case err != nil:
				return nil, fmt.Errorf("%s line %d: %w", path, n, err)
			case s.Role == "" || s.At.IsZero():
				return nil, fmt.Errorf("%s line %d: a sample needs a role and an at time", path, n)
			default:
				keepCgroupSample(roles, s, from, windowEnd, to)
			}
		}
		if readErr != nil {
			return roles, nil
		}
	}
}

// keepCgroupSample files s under its role when the step needs it: in the span from `from` to `to`,
// or as the nearest good sample on either side of it.
func keepCgroupSample(roles map[string]*roleSamples, s cgroupSample, from, windowEnd, to time.Time) {
	rs := roles[s.Role]
	if rs == nil {
		rs = &roleSamples{}
		roles[s.Role] = rs
	}
	rs.seen = rs.seen || !s.At.After(windowEnd)
	switch {
	case s.At.Before(from):
		if s.good() && (rs.before == nil || s.At.After(rs.before.At)) {
			rs.before = &s
		}
	case s.At.After(to):
		if s.good() && (rs.after == nil || s.At.Before(rs.after.At)) {
			rs.after = &s
		}
	default:
		rs.in = append(rs.in, s)
	}
}

// cgroupFile is a step's cgroup.json: each role's CPU and memory over the step's window, from the
// cluster sampler's cgroup v2 counters. Failed is set when a role went over cgroupMaxGap without a
// good sample in the window, or had none; Failures says which and why.
type cgroupFile struct {
	WindowStart time.Time             `json:"window_start"`
	WindowEnd   time.Time             `json:"window_end"`
	WindowS     float64               `json:"window_s"`
	Failed      bool                  `json:"failed"`
	Failures    []cgroupFailure       `json:"failures,omitempty"`
	Roles       map[string]cgroupRole `json:"roles"`
}

// cgroupFailure is why a role marks the step failed.
type cgroupFailure struct {
	Role   string `json:"role"`
	Reason string `json:"reason"`
}

// cgroupRole is one role's figures over the window. A figure that the samples cannot give is null,
// never 0: cores_mean, cpu_s and throttled_s need a good sample at or before window_start and one
// at or after window_end; cores_peak_5s two good samples in the window; the working set one.
type cgroupRole struct {
	Pods        []string `json:"pods"`
	Container   string   `json:"container"`
	Samples     int      `json:"samples"`
	FailedReads int      `json:"failed_reads"`
	// Restarts are the container restarts the series shows in the window: usage or throttled time
	// going down, or another pod or container. A pair of samples across one adds nothing, so the
	// figures undercount the time around it and never go negative.
	Restarts            int      `json:"restarts"`
	CPUMax              string   `json:"cpu_max"`
	LimitCores          *float64 `json:"limit_cores"`
	CoresMean           *float64 `json:"cores_mean"`
	CoresPeak5s         *float64 `json:"cores_peak_5s"`
	CPUS                *float64 `json:"cpu_s"`
	ThrottledS          *float64 `json:"throttled_s"`
	WorkingSetBytesMean *float64 `json:"working_set_bytes_mean"`
	WorkingSetBytesMax  *int64   `json:"working_set_bytes_max"`
	MaxGapS             float64  `json:"max_gap_s"`
	// Minutes are the role's figures for each whole minute from window_start, in order (cgroupMinutes);
	// a minute is null when it has a gap over cgroupMaxGap.
	Minutes []*cgroupMinute `json:"minutes"`
}

// cgroupMinute is one whole minute of a role's window: its cores (the usage counter's rise over the
// minute, its ends interpolated as cores_mean's are, over 60 s) and the mean working set of its good
// samples.
type cgroupMinute struct {
	Cores               float64 `json:"cores"`
	WorkingSetBytesMean float64 `json:"working_set_bytes_mean"`
}

// mergeCgroup reduces every sampled role's samples to its figures over the window from ws to we.
// dispatch is the dispatch role's series, which the replay block's CPU fields read too; nil when
// the role has no samples.
func mergeCgroup(roles map[string]*roleSamples, ws, we time.Time) (cf cgroupFile, dispatch *roleSeries) {
	cf = cgroupFile{WindowStart: ws, WindowEnd: we, WindowS: we.Sub(ws).Seconds(), Roles: map[string]cgroupRole{}}
	names := make([]string, 0, len(roles))
	for name, rs := range roles {
		if rs.seen {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		s := roles[name].window(ws, we)
		if name == "dispatch" {
			dispatch = &s
		}
		r := summarizeCgroupRole(roles[name], s, ws, we)
		cf.Roles[name] = r
		switch {
		case r.Samples == 0:
			cf.Failures = append(cf.Failures, cgroupFailure{Role: name, Reason: "no good sample in the window"})
		case r.MaxGapS > cgroupMaxGap.Seconds():
			cf.Failures = append(cf.Failures, cgroupFailure{Role: name, Reason: fmt.Sprintf(
				"the longest time in the window without a good sample is %.1f s, over %.0f s", r.MaxGapS, cgroupMaxGap.Seconds())})
		}
	}
	cf.Failed = len(cf.Failures) > 0
	if rs := roles["dispatch"]; rs != nil && dispatch == nil {
		// Not seen in the window, so not summarized; its samples still bracket the replay's spans.
		s := rs.window(ws, we)
		dispatch = &s
	}
	return cf, dispatch
}

// summarizeCgroupRole is one role's figures over the window from ws to we, from its series w;
// samples kept outside the window count only as the counters' brackets.
func summarizeCgroupRole(rs *roleSamples, w roleSeries, ws, we time.Time) cgroupRole {
	in, good, cut := w.in, w.good, w.cut

	r := cgroupRole{Pods: []string{}}
	var last *cgroupSample
	var wsSum float64
	var wsMax int64
	gapFrom, maxGap := ws, time.Duration(0)
	for i := range in {
		s := &in[i]
		if s.At.Before(ws) || s.At.After(we) {
			continue
		}
		if !slices.Contains(r.Pods, s.Pod) {
			r.Pods = append(r.Pods, s.Pod)
		}
		if r.Container == "" {
			r.Container = s.Container
		}
		if !s.good() {
			r.FailedReads++
			continue
		}
		r.Samples++
		maxGap, gapFrom = max(maxGap, s.At.Sub(gapFrom)), s.At
		wsSum += float64(s.workingSet())
		wsMax = max(wsMax, s.workingSet())
		last = s
	}
	r.MaxGapS = max(maxGap, we.Sub(gapFrom)).Seconds()
	if r.Samples > 0 {
		mean := wsSum / float64(r.Samples)
		r.WorkingSetBytesMean, r.WorkingSetBytesMax = &mean, &wsMax
	}
	if last == nil {
		last = rs.before
	}
	if last != nil {
		r.CPUMax = last.CPUMax
		r.LimitCores = cpuMaxCores(last.CPUMax)
	}

	windowS := we.Sub(ws).Seconds()
	if usec, ok := counterBetween(good, cut, ws, we, usageUsec); ok && windowS > 0 {
		cpuS := usec / 1e6
		cores := cpuS / windowS
		r.CPUS, r.CoresMean = &cpuS, &cores
	}
	if usec, ok := counterBetween(good, cut, ws, we, throttledUsec); ok {
		throttled := usec / 1e6
		r.ThrottledS = &throttled
	}
	for i := 1; i < len(good); i++ {
		if cut[i] && !good[i].At.Before(ws) && !good[i-1].At.After(we) {
			r.Restarts++
		}
	}
	if len(w.cores) > 0 {
		peak := slices.Max(w.cores)
		r.CoresPeak5s = &peak
	}
	r.Minutes = cgroupMinutes(good, cut, ws, we)
	return r
}

// intervalCores is the cores of each interval between consecutive good samples that both lie in
// the window from ws to we and have no restart between them: the 5 s cores.
func intervalCores(good []cgroupSample, cut []bool, ws, we time.Time) []float64 {
	var out []float64
	for i := 1; i < len(good); i++ {
		a, b := &good[i-1], &good[i]
		dt := b.At.Sub(a.At).Seconds()
		if cut[i] || dt <= 0 || a.At.Before(ws) || b.At.After(we) {
			continue
		}
		out = append(out, float64(*b.UsageUsec-*a.UsageUsec)/1e6/dt)
	}
	return out
}

// gapFreeSpan is the indexes lo and hi of the good samples that span from `from` to `to`: the
// last one at or before from and the first at or after to. ok is false when no good sample
// brackets one of the ends, or when two consecutive ones in the span are over cgroupMaxGap apart:
// a figure over the span would rest on the samples around a gap, not on samples in it.
func gapFreeSpan(good []cgroupSample, from, to time.Time) (lo, hi int, ok bool) {
	lo = sort.Search(len(good), func(i int) bool { return good[i].At.After(from) }) - 1
	hi = sort.Search(len(good), func(i int) bool { return !good[i].At.Before(to) })
	if lo < 0 || hi == len(good) {
		return 0, 0, false
	}
	for i := lo + 1; i <= hi; i++ {
		if good[i].At.Sub(good[i-1].At) > cgroupMaxGap {
			return 0, 0, false
		}
	}
	return lo, hi, true
}

// spanCPUSeconds is the CPU-seconds of the usage counter's rise from `from` to `to`, over a
// gap-free span of the good samples (gapFreeSpan); ok is false without one.
func spanCPUSeconds(good []cgroupSample, cut []bool, from, to time.Time) (cpuS float64, ok bool) {
	lo, hi, ok := gapFreeSpan(good, from, to)
	if !ok {
		return 0, false
	}
	usec, _ := counterBetween(good[lo:hi+1], cut[lo:hi+1], from, to, usageUsec)
	return usec / 1e6, true
}

// cgroupMinutes is the figures of each whole minute from ws to we over the good samples (cut marks
// the restarts, as restartCuts does). A minute is null when the good samples do not span it
// without a gap (gapFreeSpan).
func cgroupMinutes(good []cgroupSample, cut []bool, ws, we time.Time) []*cgroupMinute {
	out := make([]*cgroupMinute, int(we.Sub(ws)/time.Minute))
	for m := range out {
		from := ws.Add(time.Duration(m) * time.Minute)
		to := from.Add(time.Minute)
		lo, hi, ok := gapFreeSpan(good, from, to)
		if !ok {
			continue
		}
		// With both ends bracketed and no gap over cgroupMaxGap, at least one good sample lies in the
		// minute (from its start, inclusive, to its end), so the working-set mean has a sample.
		var wsSum float64
		n := 0
		for _, s := range good[lo : hi+1] {
			if !s.At.Before(from) && s.At.Before(to) {
				wsSum += float64(s.workingSet())
				n++
			}
		}
		usec, _ := counterBetween(good[lo:hi+1], cut[lo:hi+1], from, to, usageUsec)
		out[m] = &cgroupMinute{Cores: usec / 1e6 / time.Minute.Seconds(), WorkingSetBytesMean: wsSum / float64(n)}
	}
	return out
}

// restartCuts marks, for each good sample after the first, whether a restart lies between it and
// the one before: usage or throttled time went down, or the pod or container changed.
func restartCuts(good []cgroupSample) []bool {
	cut := make([]bool, len(good))
	for i := 1; i < len(good); i++ {
		a, b := &good[i-1], &good[i]
		cut[i] = a.Pod != b.Pod || a.Container != b.Container || *b.UsageUsec < *a.UsageUsec || *b.ThrottledUsec < *a.ThrottledUsec
	}
	return cut
}

// counterBetween is how much a counter (value) rose from `from` to `to` over the good samples, each
// pair of consecutive samples interpolated linearly; a pair across a restart (cut) adds nothing.
// ok is false unless a good sample at or before from and one at or after to bracket the span.
func counterBetween(good []cgroupSample, cut []bool, from, to time.Time, value func(*cgroupSample) int64) (rise float64, ok bool) {
	if len(good) == 0 || good[0].At.After(from) || good[len(good)-1].At.Before(to) {
		return 0, false
	}
	for i := 1; i < len(good); i++ {
		a, b := &good[i-1], &good[i]
		dt := b.At.Sub(a.At)
		if cut[i] || dt <= 0 {
			continue
		}
		lo, hi := a.At, b.At
		if lo.Before(from) {
			lo = from
		}
		if hi.After(to) {
			hi = to
		}
		if hi.After(lo) {
			rise += float64(value(b)-value(a)) * float64(hi.Sub(lo)) / float64(dt)
		}
	}
	return rise, true
}

// cpuMaxCores is the CPU limit that cgroup v2 cpu.max ("<quota> <period>") sets, in cores; nil
// for "max" (no limit) or a text that is not a quota and a period.
func cpuMaxCores(text string) *float64 {
	f := strings.Fields(text)
	if len(f) != 2 || f[0] == "max" {
		return nil
	}
	quota, err1 := strconv.ParseFloat(f[0], 64)
	period, err2 := strconv.ParseFloat(f[1], 64)
	if err1 != nil || err2 != nil || quota <= 0 || period <= 0 {
		return nil
	}
	cores := quota / period
	return &cores
}

// stepFailedKindSamplerGap is the kind of the step-failed.json reasons that cgroup-merge writes;
// the shell writes the others (restart, for example).
const stepFailedKindSamplerGap = "sampler_gap"

// stepFailedReason is one reason in step-failed.json.
type stepFailedReason struct {
	Kind   string `json:"kind"`
	Role   string `json:"role,omitempty"`
	Detail string `json:"detail,omitempty"`
}

func runCgroupMerge(args []string) error {
	fs := flag.NewFlagSet("cgroup-merge", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: loadgen cgroup-merge <step dir> <cgroup.jsonl>")
		fmt.Fprintln(fs.Output(), "writes <step dir>/cgroup.json from the samples in the window of <step dir>/meta.json, and fills")
		fmt.Fprintln(fs.Output(), "the CPU fields of <step dir>/summary.json's replay block from the dispatch role's samples")
	}
	_ = fs.Parse(args)
	if fs.NArg() != 2 {
		fs.Usage()
		return fmt.Errorf("name a step directory and a samples file")
	}
	dir, samplesPath := fs.Arg(0), fs.Arg(1)
	ws, we, end, err := readStepWindow(dir)
	if err != nil {
		return err
	}
	// The replay's drain runs from window_end to meta.json's end, at most replayDrainMax; samples
	// past window_end bracket the counters only, so they change no figure of cgroup.json.
	var drainEnd time.Time
	to := we
	if end.After(we) {
		drainEnd = we.Add(replayDrainMax)
		if end.Before(drainEnd) {
			drainEnd = end
		}
		to = drainEnd
	}
	roles, err := readCgroupSamples(samplesPath, ws.Add(-cgroupIdleBefore), we, to)
	if err != nil {
		return err
	}
	cf, dispatch := mergeCgroup(roles, ws, we)
	b, err := json.MarshalIndent(cf, "", "  ")
	if err != nil {
		return err
	}
	out := filepath.Join(dir, "cgroup.json")
	if err := os.WriteFile(out, append(b, '\n'), 0o644); err != nil {
		return err
	}
	if err := recordSamplerGaps(dir, cf.Failures); err != nil {
		return err
	}
	verdict := "the step is not marked failed"
	if cf.Failed {
		verdict = fmt.Sprintf("the step is marked failed (%d sampler gaps, in step-failed.json)", len(cf.Failures))
	}
	fmt.Fprintf(os.Stderr, "loadgen cgroup-merge: wrote %s: %d roles over %.0f s; %s\n", out, len(cf.Roles), cf.WindowS, verdict)
	filled, err := fillReplayCPU(dir, dispatch, cf.Roles["dispatch"], ws, we, drainEnd)
	if err != nil {
		return err
	}
	if filled != "" {
		fmt.Fprintf(os.Stderr, "loadgen cgroup-merge: %s\n", filled)
	}
	return nil
}

// readStepWindow is the window of the step directory's meta.json, and its end (zero when meta.json
// has none).
func readStepWindow(dir string) (ws, we, end time.Time, err error) {
	path := filepath.Join(dir, "meta.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return ws, we, end, err
	}
	var m metaFile
	if err := json.Unmarshal(b, &m); err != nil {
		return ws, we, end, fmt.Errorf("%s: %w", path, err)
	}
	if m.WindowStart.IsZero() || !m.WindowEnd.After(m.WindowStart) {
		return ws, we, end, fmt.Errorf("%s has no window: window_start %s, window_end %s", path,
			m.WindowStart.UTC().Format(time.RFC3339), m.WindowEnd.UTC().Format(time.RFC3339))
	}
	return m.WindowStart, m.WindowEnd, m.End, nil
}

// recordSamplerGaps makes the step directory's step-failed.json hold one sampler_gap reason per
// failure, and keeps every reason of another kind and every other key the file holds. A re-run
// replaces the sampler_gap reasons, so none is duplicated; when that leaves no reason, a file that
// held only sampler_gap reasons is removed. With no failure and no file it writes nothing.
func recordSamplerGaps(dir string, failures []cgroupFailure) error {
	path := filepath.Join(dir, "step-failed.json")
	doc := map[string]json.RawMessage{}
	var reasons []json.RawMessage
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if len(failures) == 0 {
			return nil
		}
	case err != nil:
		return err
	default:
		if err := json.Unmarshal(b, &doc); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if raw, ok := doc["reasons"]; ok {
			if err := json.Unmarshal(raw, &reasons); err != nil {
				return fmt.Errorf("%s: reasons: %w", path, err)
			}
		}
	}
	kept := make([]json.RawMessage, 0, len(reasons)+len(failures))
	for _, raw := range reasons {
		var r stepFailedReason
		if err := json.Unmarshal(raw, &r); err != nil {
			return fmt.Errorf("%s: reason %s: %w", path, raw, err)
		}
		if r.Kind != stepFailedKindSamplerGap {
			kept = append(kept, raw)
		}
	}
	if len(kept) == len(reasons) && len(failures) == 0 {
		return nil
	}
	if len(kept) == 0 && len(failures) == 0 && len(doc) == 1 {
		return os.Remove(path)
	}
	for _, f := range failures {
		raw, err := json.Marshal(stepFailedReason{Kind: stepFailedKindSamplerGap, Role: f.Role, Detail: f.Reason})
		if err != nil {
			return err
		}
		kept = append(kept, raw)
	}
	raw, err := json.Marshal(kept)
	if err != nil {
		return err
	}
	doc["reasons"] = raw
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

// fillReplayCPU fills the CPU fields of the step's summary.json replay block from the Dispatch
// role's cgroup samples (w, their series, nil without samples; role, its cgroup.json figures), for
// a run that read no /proc (cpu_source "none", -allow-foreign): cpu_source "cgroup", dispatch_cpu_s
// (the window's, role's cpu_s), idle_cores_before (the replayIdleSpan before ws), cores_full_rate
// and cores_full_rate_idle_corrected (replayCoresFullRate and replayIdleCorrected with the inputs'
// rate factor, over the window ws to we, which window_s then holds), and drain_cpu_s (we to
// drainEnd; none when drainEnd is zero). A figure the samples cannot give stays null, never 0:
// the window's needs the role's cpu_s, the idle and drain spans a gap-free span (gapFreeSpan).
// saturated_reasons keep the timeouts and inflight_at_end reasons loadgen gave, and gain
// dispatch_cpu when the 5 s cores (intervalCores) reach 0.9 × the container's CPU limit
// (role.limit_cores) in at least 10% of the window's intervals. Every field is computed from the
// samples again, so a re-run gives the same block. Everything else in summary.json keeps its bytes.
// It returns what it did, "" when summary.json has no replay block.
func fillReplayCPU(dir string, w *roleSeries, role cgroupRole, ws, we, drainEnd time.Time) (string, error) {
	path := filepath.Join(dir, "summary.json")
	doc, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	start, end, found, err := topLevelValue(doc, "replay")
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	if !found || string(doc[start:end]) == "null" {
		return "", nil
	}
	var rec replayRecord
	if err := json.Unmarshal(doc[start:end], &rec); err != nil {
		return "", fmt.Errorf("%s: replay: %w", path, err)
	}
	if rec.CPUSource == "proc" {
		return "summary.json's replay block has cpu_source proc (Dispatch's /proc reads); left as it is", nil
	}
	fillReplayRecord(&rec, w, role, ws, we, drainEnd)
	value, err := json.MarshalIndent(rec, "  ", "  ")
	if err != nil {
		return "", err
	}
	out := make([]byte, 0, len(doc)-(end-start)+len(value))
	out = append(append(append(out, doc[:start]...), value...), doc[end:]...)
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("filled %s's replay block from the dispatch role's samples: cpu_source %s, dispatch_cpu_s %s, idle_cores_before %s, drain_cpu_s %s, saturated %t %v",
		path, rec.CPUSource, fmtFloatPtr(rec.DispatchCPUS), fmtFloatPtr(rec.IdleCoresBefore), fmtFloatPtr(rec.DrainCPUS), rec.Saturated, rec.SaturatedReasons), nil
}

// fillReplayRecord is fillReplayCPU's computation on the decoded block.
func fillReplayRecord(rec *replayRecord, w *roleSeries, role cgroupRole, ws, we, drainEnd time.Time) {
	rec.CPUSource = "none"
	rec.DispatchCPUS, rec.CoresFullRate, rec.IdleCoresBefore, rec.CoresFullRateIdleCorrected, rec.DrainCPUS = nil, nil, nil, nil, nil
	reasons := []string{}
	for _, r := range rec.SaturatedReasons {
		if r == saturatedTimeouts || r == saturatedInflight {
			reasons = append(reasons, r)
		}
	}
	if w != nil {
		good, cut := w.good, w.cut
		windowS := we.Sub(ws).Seconds()
		rate := rec.Inputs.RateFactor
		if role.CPUS != nil {
			cpu := *role.CPUS
			rec.DispatchCPUS, rec.WindowS = &cpu, windowS
			if rate > 0 {
				cores := replayCoresFullRate(cpu, windowS, rate)
				rec.CoresFullRate = &cores
			}
		}
		if idleS, ok := spanCPUSeconds(good, cut, ws.Add(-replayIdleSpan), ws); ok {
			idle := idleS / replayIdleSpan.Seconds()
			rec.IdleCoresBefore = &idle
			if rec.DispatchCPUS != nil && rate > 0 {
				corrected := replayIdleCorrected(*rec.DispatchCPUS, idle, windowS, rate)
				rec.CoresFullRateIdleCorrected = &corrected
			}
		}
		if !drainEnd.IsZero() {
			if drain, ok := spanCPUSeconds(good, cut, we, drainEnd); ok {
				rec.DrainCPUS = &drain
			}
		}
		if rec.DispatchCPUS != nil || rec.IdleCoresBefore != nil || rec.DrainCPUS != nil {
			rec.CPUSource = "cgroup"
		}
		if cores := w.cores; role.LimitCores != nil && len(cores) > 0 {
			high := 0
			for _, c := range cores {
				if c >= 0.9**role.LimitCores {
					high++
				}
			}
			if float64(high) >= 0.1*float64(len(cores)) {
				reasons = append([]string{saturatedCPU}, reasons...)
			}
		}
	}
	rec.Saturated, rec.SaturatedReasons = len(reasons) > 0, reasons
}

// topLevelValue is where key's value lies in the JSON object doc, as the byte offsets [start, end);
// found is false when the object has no such key.
func topLevelValue(doc []byte, key string) (start, end int, found bool, err error) {
	dec := json.NewDecoder(bytes.NewReader(doc))
	tok, err := dec.Token()
	if err != nil {
		return 0, 0, false, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return 0, 0, false, errors.New("not a JSON object")
	}
	for dec.More() {
		if tok, err = dec.Token(); err != nil {
			return 0, 0, false, err
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return 0, 0, false, err
		}
		if k, _ := tok.(string); k == key {
			end = int(dec.InputOffset())
			return end - len(raw), end, true, nil
		}
	}
	return 0, 0, false, nil
}

// fmtFloatPtr is *p with three decimals, or null.
func fmtFloatPtr(p *float64) string {
	if p == nil {
		return "null"
	}
	return strconv.FormatFloat(*p, 'f', 3, 64)
}
