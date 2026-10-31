package main

import (
	"cmp"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// gridFactors are the load factors of the cluster grid; a knee-series step at another
// factor is a refinement step.
var gridFactors = []float64{1, 2, 3, 5, 7, 10}

// curveKind is where a step sits in the curves: the knee series (the grid and refinement steps),
// the reads-only control, the hour, or the 4 vCPU database. Rows go in that order.
type curveKind int

const (
	kindSeries curveKind = iota
	kindControl
	kindHour
	kindDB4
)

// verdict is a step's result on one pass criterion.
type verdict string

const (
	verdictOK   verdict = "ok"
	verdictFail verdict = "FAIL"
	verdictNA   verdict = "n/a"
)

// The pass criteria, as indices into passCriteria and curveStep.verdicts.
const (
	critCommits = iota
	critRequests
	critDelivery
	critCPULimit
	critPool
	critFailed
)

// passCriteria are the pass rule, one per column of the Pass table: a step passes when none
// is FAIL. when says when a criterion is n/a, or FAIL for want of a number.
var passCriteria = []struct{ id, csv, text, when string }{
	{"a", "pass_a_commits", "committed events/s ≥ 0.95 × offered", "n/a when offered is 0; FAIL when the commit rows were not read"},
	{"b", "pass_b_requests", "requests 2xx ≥ 0.99 × sent, and sent/s ≥ 0.95 × target", "n/a when the target is 0"},
	{"c", "pass_c_delivery", "delivery p99 from arrival ≤ 1,000 ms", "n/a with no events; FAIL when it was not measured"},
	{"d", "pass_d_cpu_limit", "no container's cores mean ≥ 0.9 × its CPU limit, from cgroup.json", "n/a without cgroup.json, which fails (f)"},
	{"e", "pass_e_pool", "Dispatch's pool acquire waiters p50 = 0", "FAIL when the sampler measured none"},
	{"f", "pass_f_no_failure", "no failed row: no step-failed.json, and cgroup.json present and not failed", ""},
}

// The failure kinds that loadgen curves adds to step-failed.json's: a step directory without
// summary.json, and a cgroup.json marked failed with no sampler_gap reason in step-failed.json.
const (
	failedKindNoSummary = "no summary.json"
	failedKindCgroup    = "cgroup.json failed"
)

// curveStep is one step directory as loadgen curves reads it: the tables' step data, its factor
// and kind, and its verdicts on passCriteria. A failed row (stepData.failed) holds its failure
// kinds in failedKinds and gives no number.
type curveStep struct {
	stepData
	dir       string
	noSummary bool
	factor    float64
	kind      curveKind
	verdicts  []verdict
	// overLimit are the roles whose cores_mean reached 0.9 × limit_cores (criterion d).
	overLimit []string
}

// readCurveStep reads the step directory dir. A directory without summary.json is a failed row
// whose factor its name gives ("<f>x…"); any other unreadable input is an error.
func readCurveStep(dir string) (*curveStep, error) {
	cs := &curveStep{dir: dir}
	_, err := os.Stat(filepath.Join(dir, "summary.json"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		cs.noSummary = true
		cs.name = filepath.Base(filepath.Clean(dir))
		cs.meta = readMetaFile(dir)
		if cs.cgroup, err = readCgroupFile(dir); err != nil {
			return nil, err
		}
		cs.failedKinds, _ = readFailedKinds(dir)
		cs.failedKinds, cs.failed = append(cs.failedKinds, failedKindNoSummary), true
		f, ok := nameFactor(cs.name)
		if !ok {
			return nil, fmt.Errorf("step %s has no summary.json, and its name gives no factor (<f>x)", dir)
		}
		cs.factor = f
	case err != nil:
		return nil, fmt.Errorf("step %s: %w", dir, err)
	default:
		if cs.stepData, err = readStep(dir); err != nil {
			return nil, err
		}
		cs.factor = configFactor(cs.sum.Config)
	}
	if cs.cgroup != nil && cs.cgroup.Failed && !slices.Contains(cs.failedKinds, stepFailedKindSamplerGap) {
		cs.failedKinds, cs.failed = append(cs.failedKinds, failedKindCgroup), true
	}
	cs.kind = cs.classify()
	cs.judge()
	return cs, nil
}

// configFactor is a step's load factor from its configuration: requests/s ÷ 60, or events/s ÷ 4
// for a step with no requests.
func configFactor(c summaryConfig) float64 {
	if c.RequestsPerS > 0 {
		return c.RequestsPerS / 60
	}
	return c.EventsPerS / 4
}

// nameFactor is the factor a step name starts with, "<f>x", as the cluster script names steps.
func nameFactor(name string) (float64, bool) {
	head, _, ok := strings.Cut(name, "x")
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(head, 64)
	return f, err == nil && finite(f) && f > 0
}

// seriesPGCPUs is the Postgres tier the knee series runs on, 16 vCPU (cluster.sh up's default, the
// size of production's instance). A step whose copy has fewer is the 4 vCPU database step.
const seriesPGCPUs = 16

// classify is the step's kind. For a cluster step whose copy names the phase, meta.json's copy
// (cluster.sh's write_copy_meta) says what the run did, so the kind comes from the run and not
// from the step's name: the hour is the phase soak, or a window of an hour or more; the 4 vCPU
// database is a copy whose Postgres has fewer than seriesPGCPUs vCPUs; the reads-only control is
// a step the run marked reads-only, or one that offered no event; every other step is in the knee
// series. Without such a copy (a local stack run, or a run from before cluster.sh recorded the
// phase, whose copy names the series' Postgres tier for every step) the name says it, as it
// always did: a window of an hour or more or -hour, -db4, -reads. The name is the row's label
// either way.
func (s *curveStep) classify() curveKind {
	if c, ok := readStepCopy(s.meta); ok && c.Phase != "" {
		cpus, haveCPUs := c.pgCPUs()
		switch {
		case c.Phase == phaseSoak || s.sum.Config.DurationS >= 3600:
			return kindHour
		case haveCPUs && cpus < seriesPGCPUs:
			return kindDB4
		case c.ReadsOnly || (!s.noSummary && s.sum.Config.EventsPerS == 0):
			return kindControl
		}
		return kindSeries
	}
	switch {
	case s.sum.Config.DurationS >= 3600 || strings.HasSuffix(s.name, "-hour"):
		return kindHour
	case strings.HasSuffix(s.name, "-db4"):
		return kindDB4
	case strings.HasSuffix(s.name, "-reads") || (!s.noSummary && s.sum.Config.EventsPerS == 0):
		return kindControl
	}
	return kindSeries
}

// kindName is the step's kind as curves.csv names it.
func (s *curveStep) kindName() string {
	switch s.kind {
	case kindControl:
		return "control"
	case kindHour:
		return "hour"
	case kindDB4:
		return "db4"
	}
	if slices.Contains(gridFactors, s.factor) {
		return "grid"
	}
	return "refinement"
}

// judge sets the step's verdicts on passCriteria. A failed row is judged by (f) alone: the curves
// take no number from it, so its other criteria are n/a.
func (s *curveStep) judge() {
	v := make([]verdict, len(passCriteria))
	for i := range v {
		v[i] = verdictNA
	}
	s.verdicts = v
	if s.failed {
		v[critFailed] = verdictFail
		return
	}
	check := func(ok bool) verdict {
		if ok {
			return verdictOK
		}
		return verdictFail
	}
	e, q, c := s.sum.Events, s.sum.Requests, s.sum.Config
	if e.OfferedPerS > 0 {
		v[critCommits] = check(!e.commitsUnread() && e.CommittedPerS >= 0.95*e.OfferedPerS)
	}
	if q.TargetPerS > 0 {
		v[critRequests] = check(q.Sent > 0 && float64(q.OK2xx) >= 0.99*float64(q.Sent) && q.SentPerS >= 0.95*q.TargetPerS)
	}
	if c.EventsPerS > 0 || e.Offered > 0 {
		d := e.DeliveryMsFromArrival
		v[critDelivery] = check(!e.commitsUnread() && d.N > 0 && d.P99 <= 1000)
	}
	if s.cgroup != nil {
		for _, name := range slices.Sorted(maps.Keys(s.cgroup.Roles)) {
			r := s.cgroup.Roles[name]
			if r.CoresMean != nil && r.LimitCores != nil && *r.CoresMean >= 0.9**r.LimitCores {
				s.overLimit = append(s.overLimit, name)
			}
		}
		v[critCPULimit] = check(len(s.overLimit) == 0)
	}
	r := s.sum.Resources
	v[critPool] = check(r != nil && r.PoolAcquireGoroutines.N > 0 && r.PoolAcquireGoroutines.P50 == 0)
	v[critFailed] = check(s.cgroup != nil)
}

// passes is true when no criterion is FAIL.
func (s *curveStep) passes() bool { return !slices.Contains(s.verdicts, verdictFail) }

// tripped are the indices of the criteria the step fails.
func (s *curveStep) tripped() []int {
	var out []int
	for i, v := range s.verdicts {
		if v == verdictFail {
			out = append(out, i)
		}
	}
	return out
}

// sortCurveSteps orders the rows: the knee series in factor order, then the controls, the hours
// and the 4 vCPU database steps, each in factor order.
func sortCurveSteps(steps []*curveStep) {
	slices.SortStableFunc(steps, func(a, b *curveStep) int {
		return cmp.Or(cmp.Compare(a.kind, b.kind), cmp.Compare(a.factor, b.factor), strings.Compare(a.name, b.name))
	})
}

// --- the numbers of a row -------------------------------------------------------------------

// curveCol is one number of a step's row: its curves.csv header, its CURVES.md header and
// decimals (pct shows a share as a percentage), and how it is read; ok is false when the step did
// not measure it or it could not be read. value runs only on a row that is not failed, which has a
// summary.json.
type curveCol struct {
	csv, md string
	d       int
	pct     bool
	value   func(s *curveStep) (v float64, ok bool)
}

// cell is the column's CURVES.md cell for s: "-" for a failed row or a missing number.
func (c curveCol) cell(s *curveStep) string {
	if s.failed {
		return "-"
	}
	v, ok := c.value(s)
	if !ok {
		return "-"
	}
	if c.pct {
		return num(100*v, 1) + "%"
	}
	return num(v, c.d)
}

// csvCell is the column's curves.csv cell for s: empty for a failed row or a missing number.
func (c curveCol) csvCell(s *curveStep) string {
	if s.failed {
		return ""
	}
	v, ok := c.value(s)
	if !ok || !finite(v) {
		return ""
	}
	return fmtFloat(v)
}

// curveTable is one table of CURVES.md. commits is set when a cell comes from the run's commit
// rows, cgroup when one comes from cgroup.json, sampler when one comes from summary.json's
// resources: their rows carry the marks of a missing source.
type curveTable struct {
	title, about             string
	cols                     []curveCol
	commits, cgroup, sampler bool
}

// The marks of a CURVES.md row whose source is missing.
const (
	noCgroupMark  = "(no cgroup.json)"
	noSamplerMark = "(no sampler window)"
)

// fromCommits reads a number that the run's commit rows give: none when they were not read.
func fromCommits(get func(eventsSummary) (float64, bool)) func(*curveStep) (float64, bool) {
	return func(s *curveStep) (float64, bool) {
		if s.sum.Events.commitsUnread() {
			return 0, false
		}
		return get(s.sum.Events)
	}
}

// statOf reads one figure of a stat; none when the stat holds no value.
func statOf(st stat, f func(stat) float64) (float64, bool) {
	if st.N == 0 {
		return 0, false
	}
	return f(st), true
}

// perS is n over the step's window; none without a window.
func perS(s *curveStep, n int) (float64, bool) {
	if d := s.sum.Config.DurationS; d > 0 {
		return float64(n) / d, true
	}
	return 0, false
}

// role is the cgroup.json role name of the step, nil without cgroup.json or the role.
func (s *curveStep) role(name string) *cgroupRole {
	if s.cgroup == nil {
		return nil
	}
	r, ok := s.cgroup.Roles[name]
	if !ok {
		return nil
	}
	return &r
}

// natsRoles are the step's cgroup.json roles of the NATS pods (nats-*), sorted.
func (s *curveStep) natsRoles() []cgroupRole {
	if s.cgroup == nil {
		return nil
	}
	names := s.cgroup.natsRoleNames()
	out := make([]cgroupRole, len(names))
	for i, name := range names {
		out[i] = s.cgroup.Roles[name]
	}
	return out
}

// coresMean is a role's cores_mean, none when null or absent.
func coresMean(name string) func(*curveStep) (float64, bool) {
	return func(s *curveStep) (float64, bool) {
		if r := s.role(name); r != nil && r.CoresMean != nil {
			return *r.CoresMean, true
		}
		return 0, false
	}
}

// coresPeak is a role's cores_peak_5s, none when null or absent.
func coresPeak(name string) func(*curveStep) (float64, bool) {
	return func(s *curveStep) (float64, bool) {
		if r := s.role(name); r != nil && r.CoresPeak5s != nil {
			return *r.CoresPeak5s, true
		}
		return 0, false
	}
}

// limitShare is a role's cores_mean as a share of its limit_cores, none when either is null.
func limitShare(r *cgroupRole) (float64, bool) {
	if r == nil || r.CoresMean == nil || r.LimitCores == nil || *r.LimitCores <= 0 {
		return 0, false
	}
	return *r.CoresMean / *r.LimitCores, true
}

// roleShare is limitShare of the named role.
func roleShare(name string) func(*curveStep) (float64, bool) {
	return func(s *curveStep) (float64, bool) { return limitShare(s.role(name)) }
}

// mib converts bytes to MiB.
func mib(b float64) float64 { return b / (1 << 20) }

// activeConns sums, over pg_conns_by_state's states whose name starts with "active", one figure of
// each state's stat; none when the sampler read no pg_stat_activity.
func activeConns(f func(stat) float64) func(*curveStep) (float64, bool) {
	return func(s *curveStep) (float64, bool) {
		r := s.sum.Resources
		if r == nil || r.PGConnsTotal.N == 0 {
			return 0, false
		}
		sum := 0.0
		for k, st := range r.PGConnsByState {
			if strings.HasPrefix(k, "active") {
				sum += f(st)
			}
		}
		return sum, true
	}
}

// resStat reads one figure of a resources stat; none without a sampler window or a value.
func resStat(get func(*resourcesSummary) stat, f func(stat) float64) func(*curveStep) (float64, bool) {
	return func(s *curveStep) (float64, bool) {
		if s.sum.Resources == nil {
			return 0, false
		}
		return statOf(get(s.sum.Resources), f)
	}
}

// memStatsHeap is an older run's MemStats HeapInuse stat, empty for a run that read the heap
// profile.
func memStatsHeap(r *resourcesSummary) stat {
	if r.MemStatsHeapInuseMiB == nil {
		return stat{}
	}
	return *r.MemStatsHeapInuseMiB
}

// Columns that several tables and KNEES.md share.
var (
	colCommitted = curveCol{"events_committed_per_s", "events/s committed", 2, false,
		fromCommits(func(e eventsSummary) (float64, bool) { return e.CommittedPerS, true })}
	colSucceeded = curveCol{"requests_succeeded_per_s", "requests/s succeeded (2xx)", 1, false,
		func(s *curveStep) (float64, bool) { return perS(s, s.sum.Requests.OK2xx) }}
	colSent = curveCol{"requests_sent_per_s", "requests/s sent", 1, false,
		func(s *curveStep) (float64, bool) { return s.sum.Requests.SentPerS, true }}
	colReqP99 = curveCol{"request_ms_p99", "request ms p99", 0, false,
		func(s *curveStep) (float64, bool) { return statOf(s.sum.Requests.LatencyMs, p99) }}
	colArrivalP99 = curveCol{"delivery_ms_from_arrival_p99", "from arrival p99", 0, false,
		fromCommits(func(e eventsSummary) (float64, bool) { return statOf(e.DeliveryMsFromArrival, p99) })}
	colPoolP50 = curveCol{"pool_acquire_waiters_p50", "pool acquire waiters p50", 0, false,
		resStat(func(r *resourcesSummary) stat { return r.PoolAcquireGoroutines }, p50)}
	colDispatchCores = curveCol{"dispatch_cores_mean", "Dispatch cores mean", 2, false, coresMean("dispatch")}
	colPGCores       = curveCol{"postgres_cores_mean", "Postgres cores mean", 2, false, coresMean("postgres")}
	colPGPeak        = curveCol{"postgres_cores_peak_5s", "Postgres cores peak 5 s", 2, false, coresPeak("postgres")}
)

// curveTables are the tables of CURVES.md before the Pass table: the root's six series (INFRA-514
// acceptance 3), then the pool and commit-order lock waiters.
var curveTables = []curveTable{
	{
		title:   "Delivery latency",
		about:   "Milliseconds over the issue-topic receipts of the window's committed writes: from the write's arrival at Dispatch, and from its transaction's commit.",
		commits: true,
		cols: []curveCol{
			{"delivery_ms_from_arrival_p50", "from arrival p50", 0, false,
				fromCommits(func(e eventsSummary) (float64, bool) { return statOf(e.DeliveryMsFromArrival, p50) })},
			colArrivalP99,
			{"delivery_ms_from_commit_p99", "from commit p99", 0, false,
				fromCommits(func(e eventsSummary) (float64, bool) { return statOf(e.DeliveryMsFromCommit, p99) })},
		},
	},
	{
		title:   "Throughput",
		about:   "Per second of the window. Delivered is the issue-topic receipts over the window, and their share of the expected receipts (committed writes × the issue's subscribed sessions). Succeeded is the 2xx responses.",
		commits: true,
		cols: []curveCol{
			{"events_offered_per_s", "events/s offered", 2, false,
				func(s *curveStep) (float64, bool) { return s.sum.Events.OfferedPerS, true }},
			colCommitted,
			{"events_delivered_per_s", "receipts/s delivered", 1, false, func(s *curveStep) (float64, bool) {
				if s.sum.Events.commitsUnread() {
					return 0, false
				}
				return perS(s, s.sum.Events.IssueTopicReceipts)
			}},
			{"delivered_share_of_expected", "delivered share of expected", 0, true,
				fromCommits(func(e eventsSummary) (float64, bool) {
					if e.ExpectedIssueTopicReceipts == 0 {
						return 0, false
					}
					return float64(e.IssueTopicReceipts) / float64(e.ExpectedIssueTopicReceipts), true
				})},
			{"requests_target_per_s", "requests/s target", 1, false,
				func(s *curveStep) (float64, bool) { return s.sum.Requests.TargetPerS, true }},
			colSent,
			colSucceeded,
			colReqP99,
		},
	},
	{
		title:  "CPU",
		about:  "Cores (CPU-seconds per second) over the window from cgroup.json, and each container's mean as a share of its CPU limit. NATS is the sum of the three pods, and its share the highest pod's.",
		cgroup: true,
		cols: []curveCol{
			colDispatchCores,
			{"dispatch_cores_peak_5s", "Dispatch cores peak 5 s", 2, false, coresPeak("dispatch")},
			{"dispatch_limit_share", "Dispatch % of limit", 0, true, roleShare("dispatch")},
			colPGCores,
			{"postgres_limit_share", "Postgres % of limit", 0, true, roleShare("postgres")},
			{"listener_cores_mean", "listener cores mean", 3, false, coresMean("listener")},
			{"listener_limit_share", "listener % of limit", 0, true, roleShare("listener")},
			{"nats_cores_mean", "NATS cores mean (sum)", 3, false, func(s *curveStep) (float64, bool) {
				roles := s.natsRoles()
				sum := 0.0
				for _, r := range roles {
					if r.CoresMean == nil {
						return 0, false
					}
					sum += *r.CoresMean
				}
				return sum, len(roles) > 0
			}},
			{"nats_limit_share_max", "NATS highest pod % of limit", 0, true, func(s *curveStep) (float64, bool) {
				roles := s.natsRoles()
				top := 0.0
				for i := range roles {
					v, ok := limitShare(&roles[i])
					if !ok {
						return 0, false
					}
					top = max(top, v)
				}
				return top, len(roles) > 0
			}},
			{"runner_cores_mean", "runner cores mean", 2, false, coresMean("runner")},
			{"runner_limit_share", "runner % of limit", 0, true, roleShare("runner")},
		},
	},
	{
		title:   "Memory",
		about:   "Dispatch's working set (memory.current less inactive_file) over the window's cgroup samples, and its Go heap over the 5 s sampler's heap reads (every 15 s). Heap live is the in-use bytes of the heap profile (`/debug/pprof/heap`), a sampled estimate of the live heap. MemStats HeapInuse is an older run's heap read, runtime.MemStats HeapInuse from `/debug/pprof/heap?debug=1`: a different metric, in its own columns, so a step shows one pair or the other and the two are never one series.",
		cgroup:  true,
		sampler: true,
		cols: []curveCol{
			{"dispatch_working_set_mib_mean", "Dispatch working set mean MiB", 0, false, func(s *curveStep) (float64, bool) {
				if r := s.role("dispatch"); r != nil && r.WorkingSetBytesMean != nil {
					return mib(*r.WorkingSetBytesMean), true
				}
				return 0, false
			}},
			{"dispatch_working_set_mib_max", "Dispatch working set max MiB", 0, false, func(s *curveStep) (float64, bool) {
				if r := s.role("dispatch"); r != nil && r.WorkingSetBytesMax != nil {
					return mib(float64(*r.WorkingSetBytesMax)), true
				}
				return 0, false
			}},
			{"heap_live_mib_p50", "Go heap live p50 MiB (heap profile)", 0, false, resStat(func(r *resourcesSummary) stat { return r.HeapLiveMiB }, p50)},
			{"heap_live_mib_max", "Go heap live max MiB (heap profile)", 0, false, resStat(func(r *resourcesSummary) stat { return r.HeapLiveMiB }, smax)},
			{"memstats_heap_inuse_mib_p50", "MemStats HeapInuse p50 MiB (older run)", 0, false, resStat(memStatsHeap, p50)},
			{"memstats_heap_inuse_mib_max", "MemStats HeapInuse max MiB (older run)", 0, false, resStat(memStatsHeap, smax)},
		},
	},
	{
		title:   "Database connections",
		about:   "Connections to database dispatch in pg_stat_activity, every 5 s. Active is the sum, over the states whose name starts with `active` (`active|<wait event type>`), of each state's p50 and of each state's max: a bound on the p50 and max of their total, which summary.json does not hold.",
		sampler: true,
		cols: []curveCol{
			{"postgres_connections_max", "Postgres connections max", 0, false,
				resStat(func(r *resourcesSummary) stat { return r.PGConnsTotal }, smax)},
			{"active_connections_p50", "active connections p50", 0, false, activeConns(p50)},
			{"active_connections_max", "active connections max", 0, false, activeConns(smax)},
		},
	},
	{
		title:  "Database CPU",
		about:  "Postgres's cores over the window from cgroup.json.",
		cgroup: true,
		cols:   []curveCol{colPGCores, colPGPeak},
	},
	{
		title:   "Pool acquire waiters",
		about:   "Dispatch's goroutines waiting in pgx's pool Acquire, every 5 s.",
		sampler: true,
		cols: []curveCol{
			colPoolP50,
			{"pool_acquire_waiters_max", "pool acquire waiters max", 0, false,
				resStat(func(r *resourcesSummary) stat { return r.PoolAcquireGoroutines }, smax)},
		},
	},
	{
		title: "Commit-order lock",
		about: "The commit-order lock probe (summary.json `commit_order_lock`), every 20 ms from 20 s into the window.",
		cols: []curveCol{
			{"commit_order_lock_waiters_p50", "lock waiters p50", 0, false, lockFigure(func(l lockSummary) float64 { return l.Waiters.P50 })},
			{"commit_order_lock_waiters_max", "lock waiters max", 0, false, lockFigure(func(l lockSummary) float64 { return l.Waiters.Max })},
			{"commit_order_lock_held_share", "time held", 0, true, lockFigure(func(l lockSummary) float64 { return l.HeldFraction })},
		},
	},
}

// lockFigure reads one figure of the commit-order lock probe; none when it took no sample.
func lockFigure(f func(lockSummary) float64) func(*curveStep) (float64, bool) {
	return func(s *curveStep) (float64, bool) {
		l := s.sum.CommitOrderLock
		if l.Samples == 0 {
			return 0, false
		}
		return f(l), true
	}
}

// --- knees and refinement -------------------------------------------------------------------

// knee is a step of the knee series that fails a criterion after a step that passed (pass), or
// the series' first judged step when it fails (pass nil). Only steps that are not failed are
// judged, so both are steps with numbers.
type knee struct {
	pass, fail *curveStep
}

// findKnees are the knees of series, the knee-series steps in factor order. The walk takes the
// steps that are not failed: a failed row gives no number, so it is judged by (f) alone (judge),
// and is neither a knee nor the end of a bracket. A knee is a step that fails after the previous
// judged step passed, or the first judged step when it fails.
func findKnees(series []*curveStep) []knee {
	var out []knee
	var prev *curveStep
	for _, s := range series {
		if s.failed {
			continue
		}
		if !s.passes() {
			switch {
			case prev == nil:
				out = append(out, knee{fail: s})
			case prev.passes():
				out = append(out, knee{pass: prev, fail: s})
			}
		}
		prev = s
	}
	return out
}

// refineFactors is one factor per knee whose bracket is wider than 1.0 and whose grid interval
// (the grid factors at or around the bracket) holds fewer than two refinement steps: the bracket's
// midpoint rounded to 0.5, moved by 0.5 steps inside the bracket when a step already has that
// factor. A knee at the series' first judged step has no bracket to narrow. A failed step ends no
// bracket (findKnees), but its factor is taken: the cluster script refuses to run a step whose
// directory exists, so a refinement never names it again.
func refineFactors(knees []knee, steps []*curveStep) []float64 {
	var out []float64
	taken := func(f float64) bool {
		return slices.Contains(out, f) || slices.ContainsFunc(steps, func(s *curveStep) bool { return s.factor == f })
	}
	for _, k := range knees {
		if k.pass == nil {
			continue
		}
		lo, hi := k.pass.factor, k.fail.factor
		if hi-lo <= 1.0 {
			continue
		}
		gLo, gHi := lo, hi
		for _, g := range gridFactors {
			if g <= lo {
				gLo = g
			}
			if g >= hi {
				gHi = g
				break
			}
		}
		inside := 0
		for _, s := range steps {
			if s.kindName() == "refinement" && s.factor > gLo && s.factor < gHi {
				inside++
			}
		}
		if inside >= 2 {
			continue
		}
		// The midpoint (lo+hi)/2 rounded to 0.5 is round(lo+hi)/2.
		mid := math.Round(lo+hi) / 2
		for d := 0.0; mid-d > lo || mid+d < hi; d += 0.5 {
			if f := mid + d; f < hi && !taken(f) {
				out = append(out, f)
				break
			}
			if f := mid - d; f > lo && !taken(f) {
				out = append(out, f)
				break
			}
		}
	}
	return out
}

// fmtFloat is f in the fewest digits that read back as f: a factor as the step names and refine.txt
// write it (8.5, 10), and a curves.csv number.
func fmtFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// --- the command ----------------------------------------------------------------------------

func runCurves(args []string) error {
	fs := flag.NewFlagSet("curves", flag.ExitOnError)
	out := fs.String("out", "", "the directory that receives CURVES.md, curves.csv, KNEES.md, refine.txt and, with an hour step, TREND.md (required)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: loadgen curves -out <dir> <step dir>...")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if *out == "" || fs.NArg() == 0 {
		fs.Usage()
		return fmt.Errorf("name -out and at least one step directory")
	}
	var steps []*curveStep
	for _, dir := range fs.Args() {
		s, err := readCurveStep(dir)
		if err != nil {
			return err
		}
		steps = append(steps, s)
	}
	sortCurveSteps(steps)
	var series []*curveStep
	for _, s := range steps {
		if s.kind == kindSeries {
			series = append(series, s)
		}
	}
	knees := findKnees(series)
	refine := refineFactors(knees, steps)

	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	files := map[string]string{
		"CURVES.md":  renderCurves(steps),
		"KNEES.md":   renderKnees(series, knees),
		"refine.txt": renderRefine(refine),
	}
	csvText, err := renderCurvesCSV(steps)
	if err != nil {
		return err
	}
	files["curves.csv"] = csvText
	if trend := renderTrend(steps); trend != "" {
		files["TREND.md"] = trend
	}
	names := slices.Sorted(maps.Keys(files))
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(*out, name), []byte(files[name]), 0o644); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "loadgen curves: wrote %s for %d steps: %d knees, %d refinement steps in refine.txt\n",
		strings.Join(names, ", "), len(steps), len(knees), len(refine))
	return nil
}

// renderRefine is refine.txt: one factor per line, empty with none.
func renderRefine(factors []float64) string {
	var b strings.Builder
	for _, f := range factors {
		b.WriteString(fmtFloat(f) + "\n")
	}
	return b.String()
}

// renderCurvesCSV is curves.csv: one row per step, every number of the CURVES.md tables (a
// column shared by two tables once), then the verdicts; an empty cell is a number not measured or
// a failed row's.
func renderCurvesCSV(steps []*curveStep) (string, error) {
	var cols []curveCol
	for _, t := range curveTables {
		for _, c := range t.cols {
			if !slices.ContainsFunc(cols, func(x curveCol) bool { return x.csv == c.csv }) {
				cols = append(cols, c)
			}
		}
	}
	header := []string{"step", "factor", "kind", "failed"}
	for _, c := range cols {
		header = append(header, c.csv)
	}
	for _, p := range passCriteria {
		header = append(header, p.csv)
	}
	var b strings.Builder
	w := csv.NewWriter(&b)
	if err := w.Write(append(header, "passes")); err != nil {
		return "", err
	}
	for _, s := range steps {
		row := []string{s.name, fmtFloat(s.factor), s.kindName(), strings.Join(s.failedKinds, ";")}
		for _, c := range cols {
			row = append(row, c.csvCell(s))
		}
		for _, v := range s.verdicts {
			row = append(row, string(v))
		}
		if err := w.Write(append(row, strconv.FormatBool(s.passes()))); err != nil {
			return "", err
		}
	}
	w.Flush()
	return b.String(), w.Error()
}

// renderCurves is CURVES.md.
func renderCurves(steps []*curveStep) string {
	var md markdown
	md.line("# Scaling curves\n")
	md.line("One row per step: the knee series (the grid and refinement steps) in factor order, then the reads-only control, the hour and the 4 vCPU database. A step's factor is its requests/s target ÷ 60 (events/s ÷ 4 without requests). `-` is a number the step did not measure or that could not be read, never 0.")
	if slices.ContainsFunc(steps, func(s *curveStep) bool { return s.failed }) {
		md.line("\n(failed: <kinds>), in every table: the step failed, so its row shows no number. The kinds are step-failed.json's `reasons[].kind` ("+failedKindsText+"; `sampler_gap`: a role went over %s without a good cgroup sample in the window), `%s` (cgroup.json is marked failed) and `%s` (the step directory holds none).",
			seconds(cgroupMaxGap), failedKindCgroup, failedKindNoSummary)
	}
	for _, t := range curveTables {
		curveTableMD(&md, t, steps)
	}
	passTableMD(&md, steps)
	controlMD(&md, steps)
	return md.String()
}

// rowLabel is the step's label in a CURVES.md table, with the marks of the table's missing
// sources; unread, noCgroup and noSampler say which of the marks it carries.
func rowLabel(s *curveStep, t curveTable) (label string, unread, noCgroup, noSampler bool) {
	label = s.label()
	if s.failed {
		return label, false, false, false
	}
	unread = t.commits && s.sum.Events.commitsUnread()
	noCgroup = t.cgroup && s.cgroup == nil
	noSampler = t.sampler && s.sum.Resources == nil
	if unread {
		label += " " + commitsUnreadMark
	}
	if noCgroup {
		label += " " + noCgroupMark
	}
	if noSampler {
		label += " " + noSamplerMark
	}
	return label, unread, noCgroup, noSampler
}

// curveTableMD writes one series table: every step a row.
func curveTableMD(md *markdown, t curveTable, steps []*curveStep) {
	md.line("\n## %s\n", t.title)
	md.line("%s\n", t.about)
	header := []string{"step", "factor"}
	for _, c := range t.cols {
		header = append(header, c.md)
	}
	md.header(header...)
	var unread, noCgroup, noSampler bool
	for _, s := range steps {
		label, u, c, sm := rowLabel(s, t)
		unread, noCgroup, noSampler = unread || u, noCgroup || c, noSampler || sm
		cells := []string{label, fmtFloat(s.factor)}
		for _, c := range t.cols {
			cells = append(cells, c.cell(s))
		}
		md.row(cells...)
	}
	commitsUnreadNote(md, unread, "its cells from the commit rows", "show")
	if noCgroup {
		md.line("\n%s: the step directory holds no cgroup.json (loadgen cgroup-merge), so its cells from cgroup.json show `-`.", noCgroupMark)
	}
	if noSampler {
		md.line("\n%s: summary.json has no `resources` (the 5 s sampler took fewer than two samples in the window), so its sampler cells show `-`.", noSamplerMark)
	}
}

// passTableMD writes the Pass table and its criteria.
func passTableMD(md *markdown, steps []*curveStep) {
	md.line("\n## Pass\n")
	md.line("The pass rule, each criterion per step. A step passes when no criterion is FAIL. A failed row is judged by (f) alone: the curves take no number from it, so its other criteria are n/a.\n")
	header := []string{"step", "factor"}
	for _, p := range passCriteria {
		header = append(header, "("+p.id+")")
	}
	md.header(append(header, "passes")...)
	for _, s := range steps {
		cells := []string{s.label(), fmtFloat(s.factor)}
		for _, v := range s.verdicts {
			cells = append(cells, string(v))
		}
		md.row(append(cells, yesNo(s.passes()))...)
	}
	md.line("")
	for _, p := range passCriteria {
		if p.when == "" {
			md.line("- (%s) %s", p.id, p.text)
		} else {
			md.line("- (%s) %s (%s)", p.id, p.text, p.when)
		}
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// beside is the knee-series step at the factor of s, nil when there is none.
func beside(steps []*curveStep, s *curveStep) *curveStep {
	for _, x := range steps {
		if x.kind == kindSeries && x.factor == s.factor {
			return x
		}
	}
	return nil
}

// controlMD writes the Control section: each reads-only control beside the knee-series step at its
// factor, and each 4 vCPU database step beside the step at its factor.
func controlMD(md *markdown, steps []*curveStep) {
	md.line("\n## Control\n")
	var reads, db4 [][2]*curveStep
	for _, s := range steps {
		if b := beside(steps, s); b != nil && (s.kind == kindControl || s.kind == kindDB4) {
			if s.kind == kindControl {
				reads = append(reads, [2]*curveStep{b, s})
			} else {
				db4 = append(db4, [2]*curveStep{b, s})
			}
		}
	}
	if len(reads) == 0 {
		md.line("No reads-only control with a knee-series step at its factor.")
	}
	for _, p := range reads {
		md.line("### Requests with and without events at %sx\n", fmtFloat(p[0].factor))
		cols := []curveCol{{md: "events/s offered", d: 2, value: func(s *curveStep) (float64, bool) { return s.sum.Events.OfferedPerS, true }}, colReqP99, colSucceeded}
		pairTableMD(md, p, cols)
		with, okW := colReqP99.value(p[0])
		without, okWo := colReqP99.value(p[1])
		if !p[0].failed && !p[1].failed && okW && okWo && without > 0 {
			md.line("\nWith events, request p99 is %s%% above %s's.", num(100*(with-without)/without, 1), p[1].name)
		}
		md.line("")
	}
	if len(db4) == 0 {
		md.line("No 4 vCPU database step with a knee-series step at its factor.")
	}
	for _, p := range db4 {
		md.line("### %sx on the 4 vCPU database\n", fmtFloat(p[0].factor))
		pairTableMD(md, p, []curveCol{colArrivalP99, colCommitted, colSucceeded, colReqP99, colPGCores, colPGPeak, colDispatchCores, colPoolP50})
		md.line("")
	}
}

// pairTableMD writes the two steps of p as rows of cols, with their pass result.
func pairTableMD(md *markdown, p [2]*curveStep, cols []curveCol) {
	header := []string{"step"}
	for _, c := range cols {
		header = append(header, c.md)
	}
	md.header(append(header, "passes")...)
	for _, s := range p {
		cells := []string{s.label()}
		for _, c := range cols {
			cells = append(cells, c.cell(s))
		}
		md.row(append(cells, yesNo(s.passes()))...)
	}
}

// renderKnees is KNEES.md: the series' failed rows, which no knee can be, then each knee's
// tripped criteria, bracket, measured limit and evidence.
func renderKnees(series []*curveStep, knees []knee) string {
	var md markdown
	md.line("# Knees\n")
	names := make([]string, len(series))
	var failed []*curveStep
	for i, s := range series {
		names[i] = s.name
		if s.failed {
			failed = append(failed, s)
		}
	}
	md.line("The knee series is the grid and refinement steps in factor order: %s. A knee is each step that fails a criterion of CURVES.md's Pass table after the previous judged step passed, and the series' first judged step when it fails. A failed step is not judged: it gives no number, so it is neither a knee nor the end of a bracket. The control, hour and 4 vCPU database steps are judged in the Pass table but are not in the series.", mdCell(strings.Join(names, ", ")))
	if len(failed) > 0 {
		md.line("")
		for _, s := range failed {
			md.line("- %sx (step %s): not judged %s.", fmtFloat(s.factor), s.name, failedMark(s.failedKinds))
		}
	}
	if len(knees) == 0 {
		md.line("\nNo knee: every judged step of the knee series passes.")
	}
	for _, k := range knees {
		f := k.fail
		md.line("\n## Knee at %sx (step %s)\n", fmtFloat(f.factor), f.label())
		md.line("- Tripped:")
		for _, c := range f.tripped() {
			md.line("  - (%s) %s", passCriteria[c].id, passCriteria[c].text)
		}
		if k.pass == nil {
			md.line("- Bracket: (none, %s): the series' first judged step fails, so no step passed before it.", fmtFloat(f.factor))
		} else {
			md.line("- Bracket: (%s, %s): last passing factor %s (step %s), failing factor %s.", fmtFloat(k.pass.factor), fmtFloat(f.factor),
				fmtFloat(k.pass.factor), k.pass.name, fmtFloat(f.factor))
		}
		md.line("- Measured limit:")
		for _, c := range f.tripped() {
			md.line("  - (%s) %s", passCriteria[c].id, kneeLimit(k, c))
		}
		md.line("- Evidence: %s.", strings.Join(evidence(f.dir), ", "))
	}
	return md.String()
}

// noPassBefore stands in a knee's limit for the passing step's number when no step passed before
// the knee.
const noPassBefore = "no step passed before it"

// kneeLimit is the measured limit of criterion c at knee k, read from the two steps' numbers.
func kneeLimit(k knee, c int) string {
	f, p := k.fail, k.pass
	// both is col at the failing step, with extra after it, then at the passing step.
	both := func(col curveCol, extra string) string {
		s := col.cell(f) + " at " + f.name + extra
		if p == nil {
			return s + "; " + noPassBefore
		}
		return s + ", " + col.cell(p) + " at " + p.name
	}
	switch c {
	case critCommits:
		return "events/s committed: " + both(colCommitted, " (offered "+num(f.sum.Events.OfferedPerS, 2)+")") + "."
	case critRequests:
		return "requests/s succeeded (2xx): " + both(colSucceeded,
			fmt.Sprintf(" (target %s, sent %s)", num(f.sum.Requests.TargetPerS, 1), colSent.cell(f))) + "."
	case critDelivery:
		committed := noPassBefore
		if p != nil {
			committed = colCommitted.cell(p) + " at " + p.name
		}
		return fmt.Sprintf("events/s committed at the last passing step: %s; delivery p99 from arrival at %s: %s ms.", committed, f.name, colArrivalP99.cell(f))
	case critCPULimit:
		var roles []string
		for _, name := range f.overLimit {
			r := f.cgroup.Roles[name]
			roles = append(roles, fmt.Sprintf("%s at %s of its %s-core limit", name, num(*r.CoresMean, 2), num(*r.LimitCores, 2)))
		}
		return fmt.Sprintf("cores mean at %s: %s.", f.name, strings.Join(roles, "; "))
	case critPool:
		return "requests/s sent: " + both(colSent, "") + "; pool acquire waiters p50 at " + f.name + ": " + colPoolP50.cell(f) + "."
	}
	// (f) at a knee is a step that is not failed and holds no cgroup.json: a failed step is no
	// knee (findKnees).
	return "the step directory holds no cgroup.json."
}

// evidence are the failing step's files to read, each marked "(absent)" when the directory lacks
// it.
func evidence(dir string) []string {
	var out []string
	for _, name := range []string{"cpu.top.txt", "goroutines.summary.txt", "TABLES.md"} {
		path := filepath.Join(dir, name)
		cell := "`" + path + "`"
		if _, err := os.Stat(path); err != nil {
			cell += " (absent)"
		}
		out = append(out, cell)
	}
	return out
}
