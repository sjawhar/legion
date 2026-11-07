package main

// loadgen step-check: one step directory judged against acceptance criterion A3 of
// dispatch://INFRA-988 - "In each step's result file, every offered rate of R5, the unit starts
// and the concurrent unit sessions are within 5% of target, no proposal has more than 30 unit
// sessions at once, and every measurement of R6 and a pass or fail for each criterion of R8 is
// present."
//
// The directory is the one a step leaves (vm/vm.sh run_step): summary.json, meta.json,
// reset.json, the CPU and heap profiles, cgroup.json merged from the run's cgroup.jsonl and
// cloudsql-cpu.json. Every clause is read from those files alone, so a step's conformance is read
// from what the step recorded and never asserted by a person; each clause prints the numbers it
// read, and the command exits non-zero when one fails, so a role puts the output straight into
// the check's result.
//
// Every target comes from the step's own configuration, never from a rate typed again here: a
// step is judged against what its run was told to send, so the baseline step, the 1x step and the
// 2x step are each judged against their own targets, and a two-population step against the
// portion each population was given as well as the totals.
//
// The pass rule is not copied here either. The step is read by readCurveStep under the spec
// loop's series (curves.go), which holds R8's criteria, and this command asks of each criterion
// only what A3 asks: that the step's own files give it a pass or a fail rather than nothing.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// stepCheckTolerance is how far a measured rate may stand from its target: A3's 5%, two-sided,
// since a step that sends half its reads and one that sends twice them both measure something
// else than the load the step stands for. The pass rule's own request criterion is one-sided
// (curves.go, criterion (b)), which is a different question: whether the generator kept up.
const stepCheckTolerance = 0.05

// The rates of R5, at 1x: production's own traffic, which every step carries as it is whatever
// its factor, and the loop's own, which a step at factor F carries F times (R5 and R7; vm/vm.sh
// step_rates sends the same numbers, and a deployment file that changes one of them drives
// something other than R5's workload, which is what this clause is here to say).
const (
	r5BaselineEventsPerS   = 4.0
	r5BaselineRequestsPerS = 60.0
	r5LoopWritesPerS       = 3.5
	r5LoopReadsPerS        = 38.5
	r5LoopUnitStartsPerS   = 1.75
)

// The Postgres readings R6 names that no command of this package writes today: the slowest
// statements and the lock waits over the step's window (one file, since one read of Postgres's
// statistics takes both), the database's size at its start and its end, and the instance's
// memory beside the CPU that loadgen cloudsql-cpu writes. The check names the file each is read
// from, so a step that records them passes the clause and a step that does not is told what it
// lacks by name rather than passing in silence.
const (
	pgStatementsFileName = "pg-statements.json"
	pgSizeFileName       = "pg-size.json"
)

// cloudsqlCPUFileName is the step's Cloud SQL reading, which loadgen cloudsql-cpu writes and the
// pass rule reads (curves.go readCloudSQLCPUFile).
const cloudsqlCPUFileName = "cloudsql-cpu.json"

// cpuProfileFileName is Dispatch's CPU profile over the window, which R6 names and vm.sh fetches
// from the machine Dispatch runs on (fetch_profiles).
const cpuProfileFileName = "cpu.pprof"

// listenerRoleMark is what a listener's cgroup.json role name holds: the deployment runs
// Dispatch's listener, the hub's and the load machine's four, each a container of its own named
// in the deployment file (vm/vm.sh sampler_targets, "containers"). R6 asks for each listener, so
// the check takes every role whose name carries this mark rather than a list of names of its own
// that another deployment file would outgrow.
const listenerRoleMark = "listener"

// The cgroup.json roles of the components R6 names one by one, as the deployment file names its
// containers.
const (
	dispatchRoleName  = "dispatch"
	authProxyRoleName = "auth-proxy"
	natsRoleName      = "nats"
)

// clauseVerdict is one clause's result: n/a is a clause the step's own load leaves nothing to
// judge (the baseline step drives no unit session), which is no failure.
type clauseVerdict string

const (
	clauseOK   clauseVerdict = "ok"
	clauseFail clauseVerdict = "FAIL"
	clauseNA   clauseVerdict = "n/a"
)

// stepClause is one named clause of the check: what it read, and its verdict.
type stepClause struct {
	name    string
	reading string
	verdict clauseVerdict
}

func okClause(name, reading string) stepClause {
	return stepClause{name: name, reading: reading, verdict: clauseOK}
}
func failClause(name, reading string) stepClause {
	return stepClause{name: name, reading: reading, verdict: clauseFail}
}
func naClause(name, reading string) stepClause {
	return stepClause{name: name, reading: reading, verdict: clauseNA}
}

// recordedClause is a measurement clause: ok when the step's files hold a reading of it, FAIL when
// they hold none, with the reading, or what is missing, printed either way.
func recordedClause(name string, ok bool, reading string) stepClause {
	if ok {
		return okClause(name, reading)
	}
	return failClause(name, reading)
}

// relDelta is how far a measured number stands from the one it is judged against, as a fraction
// of it. A target of 0 is met by 0 alone, so a number beside it is infinitely far off rather
// than a division by zero (loopseed_check.go holds the same rule for a seed's counts).
func relDelta(want, got float64) float64 {
	if want == 0 {
		if got == 0 {
			return 0
		}
		return math.Inf(1)
	}
	return (got - want) / want
}

// pctDelta is relDelta as a clause prints it.
func pctDelta(d float64) string {
	if math.IsInf(d, 0) {
		return "against nothing"
	}
	return fmt.Sprintf("%+.2f%%", d*100)
}

// round2 is x to two decimals, which a factor read back from a rate is printed at: 7.00 ÷ 3.5
// is 2 and not 2.0000000000000004.
func round2(x float64) float64 { return math.Round(x*100) / 100 }

// plural is n of thing, with the s a count of one does not take: "1 lock wait", "3 lock waits".
func plural(n int, thing string) string {
	if n == 1 {
		return "1 " + thing
	}
	return num(float64(n), 0) + " " + thing + "s"
}

// rateClause judges one measured rate against the target the step's own configuration holds. A
// part of R5 the step was given no target for is n/a: the baseline step offers no loop write, no
// loop read and no unit session, and a clause that read 0 against a target of 0 as a failure
// would fail a step for running exactly what it was told to run. A step that measured load it was
// given no target for is a failure, since then the rate stands for nothing.
func rateClause(name, unit string, target, measured float64, note string) stepClause {
	c := stepClause{name: name}
	d := relDelta(target, measured)
	switch {
	case target == 0 && measured == 0:
		c.verdict, c.reading = clauseNA, "no target"
	case target == 0:
		c.verdict, c.reading = clauseFail, fmt.Sprintf("no target, measured %s%s", num(measured, 2), unit)
	default:
		c.verdict = clauseFail
		if math.Abs(d) <= stepCheckTolerance {
			c.verdict = clauseOK
		}
		c.reading = fmt.Sprintf("target %s%s, measured %s%s (%s)", num(target, 2), unit, num(measured, 2), unit, pctDelta(d))
	}
	if note != "" {
		c.reading += "; " + note
	}
	return c
}

// statReading is a statistic as a clause prints it, and whether it holds a value: a stat of no
// samples is the zero stat (mkStat), so its p50 and p99 are not a measured 0 but nothing measured.
func statReading(s stat, unit string, decimals int) (string, bool) {
	if s.N == 0 {
		return "no sample", false
	}
	return fmt.Sprintf("p50 %s%s, p99 %s%s, max %s%s over %s samples",
		num(s.P50, decimals), unit, num(s.P99, decimals), unit, num(s.Max, decimals), unit, num(float64(s.N), 0)), true
}

// --- the step's own files ---------------------------------------------------------------------

// pgStatement is one of the window's slowest statements: Postgres's own text for it and the time
// it took, as pg_stat_statements reports them.
type pgStatement struct {
	Query   string  `json:"query"`
	Calls   int64   `json:"calls"`
	MeanMs  float64 `json:"mean_ms"`
	TotalMs float64 `json:"total_ms"`
}

// pgLockWait is one lock wait of the step's window as loadgen pg-statements records it: the
// lock, what waited for it, what held it, how many samples saw it, and how long it waited where
// the server times a wait (pg_locks.waitstart, Postgres 14 and after). WaitMsMax and WaitMsMean
// are null and never 0 on a server that does not time one, so a wait the sampler saw and could
// not time is a wait all the same: the clause counts every entry and times the ones it can.
type pgLockWait struct {
	LockType     string   `json:"lock_type"`
	Target       string   `json:"target"`
	Mode         string   `json:"mode"`
	WaiterQuery  string   `json:"waiter_query"`
	BlockerQuery string   `json:"blocker_query"`
	Observations int64    `json:"observations"`
	SeenForS     float64  `json:"seen_for_s"`
	WaitMsMax    *float64 `json:"wait_ms_max"`
	WaitMsMean   *float64 `json:"wait_ms_mean"`
}

// longestLockWait is the longest wait the step timed, nil when it timed none: a server before
// Postgres 14 has no pg_locks.waitstart, and its waits are counted and not timed.
func longestLockWait(waits []pgLockWait) *float64 {
	var longest *float64
	for _, w := range waits {
		if w.WaitMsMax != nil && (longest == nil || *w.WaitMsMax > *longest) {
			longest = w.WaitMsMax
		}
	}
	return longest
}

// pgStatementsFile is the step's pg-statements.json: Postgres's slowest statements over the
// step's window and the lock waits beside them, which R6 names together. Statements and
// LockWaits are pointers so that a collector that ran and found nothing reads differently from
// one that never ran: an absent key is no reading, and an empty array is a reading of none,
// which a step under load may legitimately have of lock waits and never of statements.
type pgStatementsFile struct {
	Statements *[]pgStatement `json:"statements"`
	LockWaits  *[]pgLockWait  `json:"lock_waits"`
}

// pgSizeFile is the step's pg-size.json: the database's size in bytes when the step started and
// when it ended, which R6 names. A size of 0 is no reading: the seeded database is never empty,
// and a step only adds to it, so an end under the start is a read of another database or at
// another time, not a measurement.
type pgSizeFile struct {
	StartBytes int64 `json:"start_bytes"`
	EndBytes   int64 `json:"end_bytes"`
}

// cloudsqlMemory is the Cloud SQL instance's memory in cloudsql-cpu.json. R6 asks for the memory
// of Postgres beside its CPU, and the file loadgen cloudsql-cpu writes holds the CPU alone
// (cloudsqlcpu.go), so the memory is read here on its own rather than through cloudsqlCPUFile,
// which the pass rule reads and which judges CPU: a step whose file carries the memory reads as
// recorded, and one whose file does not is named by its clause.
type cloudsqlMemory struct {
	MemoryBytesMean       *float64 `json:"memory_bytes_mean"`
	MemoryUtilizationMean *float64 `json:"memory_utilization_mean"`
}

// readStepJSON reads the step directory's file into v; ok is false when the directory holds none.
// A file that is there and cannot be read is an error and never a silent absence, so a broken
// read is never taken for a measurement the step did not make (curves.go readCloudSQLCPUFile).
func readStepJSON(dir, name string, v any) (bool, error) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return false, fmt.Errorf("step %s: %s: %w", dir, name, err)
	}
	return true, nil
}

// stepCheck is one step directory as the check reads it: the step the pass rule reads
// (readCurveStep, which carries summary.json, meta.json, cgroup.json, cloudsql-cpu.json, the
// step's failure and its verdicts), and the Postgres readings R6 names that no other command of
// this package reads.
type stepCheck struct {
	dir  string
	step *curveStep
	// mem is cloudsql-cpu.json's memory, statements pg-statements.json and size pg-size.json,
	// each zero when the step holds no such file or field; hasStatements and hasSize say whether
	// the directory holds the file at all, so a collector that ran and found nothing is told
	// apart from one that never ran.
	mem           cloudsqlMemory
	statements    pgStatementsFile
	hasStatements bool
	size          pgSizeFile
	hasSize       bool
	profileB      int64
}

// readStepCheck reads every file the clauses are judged from. A directory that is unreadable, or
// whose files are not what they say they are, is an error: it is a step nothing can be read from,
// which is not the same as a step that measured nothing.
func readStepCheck(dir string) (*stepCheck, error) {
	cs, err := readCurveStep(loopSeries, dir)
	if err != nil {
		return nil, err
	}
	sc := &stepCheck{dir: dir, step: cs}
	if _, err := readStepJSON(dir, cloudsqlCPUFileName, &sc.mem); err != nil {
		return nil, err
	}
	if sc.hasStatements, err = readStepJSON(dir, pgStatementsFileName, &sc.statements); err != nil {
		return nil, err
	}
	if sc.hasSize, err = readStepJSON(dir, pgSizeFileName, &sc.size); err != nil {
		return nil, err
	}
	if st, err := os.Stat(filepath.Join(dir, cpuProfileFileName)); err == nil {
		sc.profileB = st.Size()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return sc, nil
}

// --- the clauses --------------------------------------------------------------------------------

// clauses are every clause of the check, in the order they are printed: the rates and the
// placement of R5, the measurements of R6, and a pass or a fail for each criterion of R8.
func (sc *stepCheck) clauses() []stepClause {
	var out []stepClause
	out = append(out, sc.rateClauses()...)
	out = append(out, sc.capClause())
	out = append(out, sc.r6Clauses()...)
	out = append(out, sc.r8Clauses()...)
	return out
}

// rateClauses judge the rates of R5: first that the step's own targets are R5's rates for the
// step it stands for, then that what it offered met those targets. Both halves are needed. A
// step run at half its read rate is a step whose generator met its target exactly, and only the
// first half sees that the target was not the step's (A3's negative control); a step whose
// generator fell behind has R5's target and misses it, which only the second half sees.
func (sc *stepCheck) rateClauses() []stepClause {
	s := sc.step.sum
	out := []stepClause{sc.baselineTargetClause(), sc.loopTargetClause(),
		rateClause("R5 writes offered", "/s", s.Config.EventsPerS, s.Events.OfferedPerS,
			fmt.Sprintf("%s writes over the %s s window", num(float64(s.Events.Offered), 0), num(s.Config.DurationS, 0))),
		rateClause("R5 reads sent", "/s", s.Requests.TargetPerS, s.Requests.SentPerS,
			fmt.Sprintf("%s reads sent, %s dropped at the in-flight cap", num(float64(s.Requests.Sent), 0), num(float64(s.Requests.DroppedInflightCap), 0))),
	}
	for _, p := range s.Populations {
		out = append(out,
			rateClause("R5 writes offered ("+p.Name+")", "/s", p.EventsPerS, p.Events.OfferedPerS, ""),
			rateClause("R5 reads sent ("+p.Name+")", "/s", p.RequestsPerS, p.Requests.SentPerS, ""))
	}
	return append(out, sc.unitStartClause(), sc.concurrentClause())
}

// population is the step's named population, nil for a step that did not drive it.
func (sc *stepCheck) population(name string) *populationSummary {
	for i, p := range sc.step.sum.Populations {
		if p.Name == name {
			return &sc.step.sum.Populations[i]
		}
	}
	return nil
}

// baselineTargetClause: production's own traffic under the loop, which R5 fixes at 4 writes and
// 60 requests a second and which every step carries as it is, whatever its factor (R5, R7).
func (sc *stepCheck) baselineTargetClause() stepClause {
	const name = "R5 baseline target"
	p := sc.population(populationBaseline)
	if p == nil {
		return failClause(name, "the step drove no "+populationBaseline+" population, so it carried none of production's own traffic")
	}
	dw, dr := relDelta(r5BaselineEventsPerS, p.EventsPerS), relDelta(r5BaselineRequestsPerS, p.RequestsPerS)
	reading := fmt.Sprintf("writes %s/s of R5's %s/s (%s), reads %s/s of R5's %s/s (%s)",
		num(p.EventsPerS, 2), num(r5BaselineEventsPerS, 2), pctDelta(dw),
		num(p.RequestsPerS, 2), num(r5BaselineRequestsPerS, 2), pctDelta(dr))
	if math.Abs(dw) > stepCheckTolerance || math.Abs(dr) > stepCheckTolerance {
		return failClause(name, reading)
	}
	return okClause(name, reading)
}

// loopTargetClause: the loop's own rates, which R5 gives at 1x and a step at factor F carries F
// times. The three rates are read as the factor each one stands for, so a step whose reads were
// set to half their target reads as one factor for its writes and another for its reads, which
// is what A3's negative control is; the step's own config.loop_factor is the factor they are
// judged against when the step carries one.
func (sc *stepCheck) loopTargetClause() stepClause {
	const name = "R5 loop target"
	p := sc.population(populationLoop)
	if p == nil {
		return failClause(name, "the step drove no "+populationLoop+" population, so it carried none of the loop's own load")
	}
	var starts float64
	if lp := sc.step.sum.LoopSessions; lp != nil {
		starts = lp.UnitStartsPerS
	}
	rates := []struct {
		what   string
		target float64
		r5At1x float64
	}{
		{"writes", p.EventsPerS, r5LoopWritesPerS},
		{"reads", p.RequestsPerS, r5LoopReadsPerS},
		{"unit starts", starts, r5LoopUnitStartsPerS},
	}
	if p.EventsPerS == 0 && p.RequestsPerS == 0 && starts == 0 {
		return naClause(name, "the step carried no loop load: it is the baseline alone, factor 0")
	}
	factor := sc.step.sum.Config.LoopFactor
	from := "config.loop_factor"
	if factor <= 0 {
		factor, from = p.RequestsPerS/r5LoopReadsPerS, "the step's own read target"
	}
	var read []string
	bad := false
	for _, r := range rates {
		at := r.target / r.r5At1x
		d := relDelta(factor, at)
		bad = bad || math.Abs(d) > stepCheckTolerance
		read = append(read, fmt.Sprintf("%s %s/s = %sx (%s)", r.what, num(r.target, 2), fmtFloat(round2(at)), pctDelta(d)))
	}
	reading := fmt.Sprintf("factor %s from %s: %s", fmtFloat(factor), from, strings.Join(read, ", "))
	if bad {
		return failClause(name, reading)
	}
	return okClause(name, reading)
}

// unitStartClause judges the unit sessions started against -unit-starts. The schedule runs from
// the step's start to the end of its window (loopsessions.go runUnits), warm-up included, so the
// rate is the unit sessions the delivery join counted over that whole span and not over the
// window alone: a start in the warm-up is a start the step made.
func (sc *stepCheck) unitStartClause() stepClause {
	const name = "R5 unit starts"
	lp := sc.step.sum.LoopSessions
	if lp == nil {
		return naClause(name, "the step ran no unit session")
	}
	span := sc.step.sum.Config.WarmupS + sc.step.sum.Config.DurationS
	if span <= 0 {
		return failClause(name, "the step records no warm-up and no window to read a rate over")
	}
	started := sc.step.sum.Events.ByClass[deliveryClassUnit].Sessions
	return rateClause(name, "/s", lp.UnitStartsPerS, float64(started)/span,
		fmt.Sprintf("%s unit sessions over the %s s the schedule ran, %s refused by their listener",
			num(float64(started), 0), num(span, 0), num(float64(lp.StartErrors), 0)))
}

// concurrentClause judges the unit sessions live at one time against the step's own target, the
// start rate times a session's life (R5: 1.75 starts a second of sessions that live 5 minutes, so
// about 525 at once). The reading is the median of the once-a-second samples, which the ramp of
// the first -unit-life does not move: a step of an hour samples the plateau for most of its
// length, and a mean would read the ramp as a shortfall the deployment never had.
func (sc *stepCheck) concurrentClause() stepClause {
	const name = "R5 concurrent unit sessions"
	lp := sc.step.sum.LoopSessions
	if lp == nil {
		return naClause(name, "the step ran no unit session")
	}
	if lp.Concurrent.N == 0 && lp.UnitStartsPerS > 0 {
		return failClause(name, "the step sampled no live unit session")
	}
	reading, _ := statReading(lp.Concurrent, "", 0)
	return rateClause(name, "", lp.UnitStartsPerS*lp.UnitLifeS, lp.Concurrent.P50,
		fmt.Sprintf("%s starts a second of sessions living %s s; %s", num(lp.UnitStartsPerS, 2), num(lp.UnitLifeS, 0), reading))
}

// capClause judges R5's bound on one proposal: a proposal never holds more than 30 unit sessions
// at the same time, which the placement keeps by sending a start that would pass it to the next
// proposal by share (loopsessions.go unitPlacer.take). The spilled starts are printed with it,
// since they say how often the bound bound.
func (sc *stepCheck) capClause() stepClause {
	const name = "R5 unit sessions on one proposal"
	lp := sc.step.sum.LoopSessions
	if lp == nil {
		return naClause(name, "the step ran no unit session")
	}
	reading := fmt.Sprintf("most on one proposal %d, bound %d; %s starts went to the next proposal by share",
		lp.MostOnOneProposal, unitCapPerProposal, num(float64(lp.Spilled), 0))
	if lp.MostOnOneProposal > unitCapPerProposal {
		return failClause(name, reading)
	}
	return okClause(name, reading)
}

// r6Clauses are the measurements R6 names, each as its own clause with its reading: a clause is
// ok when the step's files hold the measurement, n/a when the step's own load leaves nothing to
// measure, and FAIL when the step should hold it and holds none.
func (sc *stepCheck) r6Clauses() []stepClause {
	s := sc.step.sum
	loop := s.LoopSessions != nil
	out := []stepClause{
		sc.kindLatencyClause(), sc.routeLatencyClause(),
		deliveryLatencyClause("R6 delivery latency from arrival", s.Events.DeliveryMsFromArrival, s.Events.commitsUnread()),
		deliveryLatencyClause("R6 delivery latency from commit", s.Events.DeliveryMsFromCommit, s.Events.commitsUnread()),
		sc.sessionClassClause(), sc.notificationCountsClause(),
	}
	for _, c := range []struct {
		name string
		st   stat
	}{
		{"R6 session start latency", loopStat(s.LoopSessions, func(l *loopSessionsSummary) stat { return l.StartMs })},
		{"R6 subscribe latency", loopStat(s.LoopSessions, func(l *loopSessionsSummary) stat { return l.SubscribeMs })},
		{"R6 unsubscribe latency", loopStat(s.LoopSessions, func(l *loopSessionsSummary) stat { return l.UnsubscribeMs })},
	} {
		if !loop {
			out = append(out, naClause(c.name, "the step ran no loop session"))
			continue
		}
		reading, ok := statReading(c.st, " ms", 0)
		out = append(out, recordedClause(c.name, ok, reading))
	}
	out = append(out, sc.rateReadingsClause(), sc.concurrentReadingClause())
	if loop {
		out = append(out,
			okClause("R6 most unit sessions on one proposal", num(float64(s.LoopSessions.MostOnOneProposal), 0)))
	} else {
		out = append(out, naClause("R6 most unit sessions on one proposal", "the step ran no unit session"))
	}
	out = append(out, sc.capRefusedClause())
	out = append(out,
		sc.componentClause("Dispatch", dispatchRoleName),
		sc.componentClause("the Auth Proxy", authProxyRoleName),
		sc.listenerClause(),
		sc.componentClause("NATS", natsRoleName),
		sc.generatorClause(),
		sc.postgresCPUMemoryClause(),
		sc.poolWaitersClause(),
		sc.statementsAndLocksClause(),
		sc.profileClause(),
		sc.sizeClause())
	return out
}

// loopStat reads one statistic of the loop sessions; the zero stat without them.
func loopStat(lp *loopSessionsSummary, f func(*loopSessionsSummary) stat) stat {
	if lp == nil {
		return stat{}
	}
	return f(lp)
}

// kindLatencyClause: p50 and p99 for each write kind the step offered (events.by_kind). A kind
// whose writes all failed has no latency, which is a measurement the step lacks and names.
func (sc *stepCheck) kindLatencyClause() stepClause {
	const name = "R6 write latency by kind"
	byKind := sc.step.sum.Events.ByKind
	if len(byKind) == 0 {
		return failClause(name, "summary.json holds no write kind")
	}
	var read, missing []string
	for _, kind := range slices.Sorted(maps.Keys(byKind)) {
		k := byKind[kind]
		if k.WriteMs.N == 0 {
			missing = append(missing, kind)
			continue
		}
		read = append(read, fmt.Sprintf("%s %s/%s ms", kind, num(k.WriteMs.P50, 0), num(k.WriteMs.P99, 0)))
	}
	reading := fmt.Sprintf("%d kinds, p50/p99: %s", len(byKind), strings.Join(read, ", "))
	if len(missing) > 0 {
		return failClause(name, reading+"; no latency for "+strings.Join(missing, ", "))
	}
	return okClause(name, reading)
}

// routeLatencyClause: p50 and p99 for each read kind the step sent (requests.routes, one entry
// per route of the mix, which is how R5's read kinds reach Dispatch).
func (sc *stepCheck) routeLatencyClause() stepClause {
	const name = "R6 read latency by route"
	routes := sc.step.sum.Requests.Routes
	if len(routes) == 0 {
		if sc.step.sum.Requests.TargetPerS == 0 {
			return naClause(name, "the step sent no read")
		}
		return failClause(name, "summary.json holds no route")
	}
	var missing []string
	for _, route := range slices.Sorted(maps.Keys(routes)) {
		if routes[route].LatencyMs.N == 0 {
			missing = append(missing, route)
		}
	}
	reading := fmt.Sprintf("%d routes, each with p50 and p99 (requests p50 %s ms, p99 %s ms)",
		len(routes), num(sc.step.sum.Requests.LatencyMs.P50, 0), num(sc.step.sum.Requests.LatencyMs.P99, 0))
	if len(missing) > 0 {
		return failClause(name, fmt.Sprintf("%d routes; no latency for %s", len(routes), strings.Join(missing, ", ")))
	}
	return okClause(name, reading)
}

// deliveryLatencyClause is one of the two notification delivery latencies R6 names. A step whose
// commit rows could not be read measured neither: every count and latency that comes from them is
// 0 because nothing was read (delivery.go, commits_error).
func deliveryLatencyClause(name string, st stat, commitsUnread bool) stepClause {
	if commitsUnread {
		return failClause(name, "the run's commit rows were not read, so the step measured none")
	}
	reading, ok := statReading(st, " ms", 0)
	return recordedClause(name, ok, reading)
}

// sessionClassClause: R6 asks for the delivery of the driver and of the unit sessions apart, which
// the delivery join records per class (events.by_class): each class's sessions, what it was owed,
// what it got, what it lost and what it took twice.
func (sc *stepCheck) sessionClassClause() stepClause {
	const name = "R6 delivery by session class"
	byClass := sc.step.sum.Events.ByClass
	if sc.step.sum.LoopSessions == nil {
		return naClause(name, "the step ran no loop session")
	}
	var read, missing []string
	for _, class := range []string{deliveryClassUnit, deliveryClassDriver} {
		c, ok := byClass[class]
		if !ok {
			missing = append(missing, class)
			continue
		}
		read = append(read, fmt.Sprintf("%s %s sessions, expected %s, received %s, lost %s, duplicate %s",
			class, num(float64(c.Sessions), 0), num(float64(c.Expected), 0), num(float64(c.Received), 0),
			num(float64(c.Lost), 0), num(float64(c.Duplicate), 0)))
	}
	if len(missing) > 0 {
		return failClause(name, "no class "+strings.Join(missing, " and ")+" in events.by_class")
	}
	return okClause(name, strings.Join(read, "; "))
}

// notificationCountsClause: the notifications expected, received, lost and received more than
// once. The step's own totals are the join's (events), and the duplicates are counted per class,
// since a second receipt is a session's (delivery.go).
func (sc *stepCheck) notificationCountsClause() stepClause {
	const name = "R6 notifications expected, received, lost and duplicate"
	e := sc.step.sum.Events
	if e.commitsUnread() {
		return failClause(name, "the run's commit rows were not read, so the step counted none")
	}
	duplicate := 0
	for _, c := range e.ByClass {
		duplicate += c.Duplicate
	}
	return okClause(name, fmt.Sprintf("expected %s, received %s, lost %s, duplicate %s",
		num(float64(e.ExpectedIssueTopicReceipts), 0), num(float64(e.IssueTopicReceipts), 0),
		num(float64(e.Missing), 0), num(float64(duplicate), 0)))
}

// rateReadingsClause: the offered and the committed rate of each part of R5. The rates are judged
// against their targets by the R5 clauses above; this clause is A3's other half, that the file
// holds a reading of each of them.
func (sc *stepCheck) rateReadingsClause() stepClause {
	const name = "R6 offered and committed rates"
	s := sc.step.sum
	if s.Config.DurationS <= 0 {
		return failClause(name, "the step records no window to read a rate over")
	}
	reading := fmt.Sprintf("writes offered %s/s, committed %s/s; reads sent %s/s",
		num(s.Events.OfferedPerS, 2), num(s.Events.CommittedPerS, 2), num(s.Requests.SentPerS, 2))
	if lp := s.LoopSessions; lp != nil {
		reading += fmt.Sprintf("; unit starts %s/s", num(lp.UnitStartsPerS, 2))
	}
	if s.Events.commitsUnread() {
		return failClause(name, reading+"; the run's commit rows were not read, so the committed rate is no reading")
	}
	return okClause(name, reading)
}

// concurrentReadingClause: the count of unit sessions at the same time, sampled once a second.
func (sc *stepCheck) concurrentReadingClause() stepClause {
	const name = "R6 unit sessions at the same time"
	lp := sc.step.sum.LoopSessions
	if lp == nil {
		return naClause(name, "the step ran no unit session")
	}
	reading, ok := statReading(lp.Concurrent, "", 0)
	return recordedClause(name, ok, reading)
}

// capRefusedClause: the writes a document cap refused, which the loop's writer counts with the
// part documents those refusals opened (events.go loopRun).
func (sc *stepCheck) capRefusedClause() stepClause {
	const name = "R6 writes a document cap refused"
	lr := sc.step.sum.Loop
	if lr == nil {
		return naClause(name, "the step drove no loop write")
	}
	return okClause(name, fmt.Sprintf("%s refused, %s new part documents opened",
		num(float64(lr.CapExceeded), 0), num(float64(lr.NewParts), 0)))
}

// componentClause is the CPU and memory of one component R6 names, from its cgroup.json role. A
// figure the samples could not give is null and never 0 (cgroup.go), so a role with a null
// cores_mean or a null working set measured nothing and the clause says so.
func (sc *stepCheck) componentClause(label, role string) stepClause {
	name := "R6 " + label + " CPU and memory"
	if sc.step.cgroup == nil {
		return failClause(name, "the step has no cgroup.json")
	}
	reading, ok := cgroupReading(sc.step.role(role))
	return recordedClause(name, ok, role+": "+reading)
}

// cgroupReading is a role's CPU and memory as a clause prints it, and whether cgroup.json gives
// both.
func cgroupReading(r *cgroupRole) (string, bool) {
	if r == nil {
		return "no role of that name in cgroup.json", false
	}
	cores, ws := "none", "none"
	if r.CoresMean != nil {
		cores = num(*r.CoresMean, 3)
	}
	if r.WorkingSetBytesMax != nil {
		ws = num(mib(float64(*r.WorkingSetBytesMax)), 0) + " MiB"
	}
	return fmt.Sprintf("cores mean %s, working set max %s", cores, ws), r.CoresMean != nil && r.WorkingSetBytesMax != nil
}

// listenerClause is the CPU and memory of each listener: every cgroup.json role whose name
// carries listenerRoleMark. A step that sampled none has no listener reading at all.
func (sc *stepCheck) listenerClause() stepClause {
	const name = "R6 listener CPU and memory"
	if sc.step.cgroup == nil {
		return failClause(name, "the step has no cgroup.json")
	}
	var roles []string
	for _, role := range slices.Sorted(maps.Keys(sc.step.cgroup.Roles)) {
		if strings.Contains(role, listenerRoleMark) {
			roles = append(roles, role)
		}
	}
	if len(roles) == 0 {
		return failClause(name, "cgroup.json holds no role whose name carries "+listenerRoleMark)
	}
	var read, missing []string
	for _, role := range roles {
		r := sc.step.cgroup.Roles[role]
		reading, ok := cgroupReading(&r)
		if !ok {
			missing = append(missing, role+" ("+reading+")")
			continue
		}
		read = append(read, role+" "+reading)
	}
	if len(missing) > 0 {
		return failClause(name, fmt.Sprintf("%d listeners; no reading of %s", len(roles), strings.Join(missing, ", ")))
	}
	return okClause(name, fmt.Sprintf("%d listeners: %s", len(roles), strings.Join(read, "; ")))
}

// generatorClause is the load generator's CPU and memory. The machine that sent the load is the
// step's own config.generator_role, which the pass rule's void rule reads too (curves.go
// markVoid): only the run that sent the load knows which machine sent it.
func (sc *stepCheck) generatorClause() stepClause {
	const name = "R6 the load generator CPU and memory"
	role := sc.step.sum.Config.GeneratorRole
	if role == "" {
		return failClause(name, "the step records no config.generator_role, so no role is its generator's")
	}
	if sc.step.cgroup == nil {
		return failClause(name, "the step has no cgroup.json")
	}
	reading, ok := cgroupReading(sc.step.role(role))
	return recordedClause(name, ok, role+": "+reading)
}

// postgresCPUMemoryClause is Postgres's CPU and memory. The deployment's database is a Cloud SQL
// instance, whose CPU loadgen cloudsql-cpu reads from Cloud Monitoring into cloudsql-cpu.json;
// its memory is the same read of the instance's memory metric, which that command does not take
// today, so a step holds the memory when its cloudsql-cpu.json carries memory_bytes_mean. A
// memory of 0 bytes is no reading: a running instance always holds some.
func (sc *stepCheck) postgresCPUMemoryClause() stepClause {
	const name = "R6 Postgres CPU and memory"
	cpu := sc.step.cloudsql
	if cpu == nil {
		return failClause(name, "the step holds no "+cloudsqlCPUFileName)
	}
	cpuReading := fmt.Sprintf("cores mean %s of %s vCPUs", num(cpu.CoresMean, 2), num(cpu.LimitCores, 0))
	switch {
	case sc.mem.MemoryBytesMean == nil:
		return failClause(name, cpuReading+", and no memory: "+cloudsqlCPUFileName+" holds no memory_bytes_mean")
	case *sc.mem.MemoryBytesMean <= 0:
		return failClause(name, fmt.Sprintf("%s, and %s holds memory_bytes_mean %s, which a running instance never has",
			cpuReading, cloudsqlCPUFileName, num(*sc.mem.MemoryBytesMean, 0)))
	}
	return okClause(name, fmt.Sprintf("%s, memory mean %s MiB", cpuReading, num(mib(*sc.mem.MemoryBytesMean), 0)))
}

// poolWaitersClause: the median and the maximum count of requests waiting for a database
// connection, which the 5 s sampler reads from Dispatch's pprof port.
func (sc *stepCheck) poolWaitersClause() stepClause {
	const name = "R6 requests waiting for a database connection"
	r := sc.step.sum.Resources
	if r == nil {
		return failClause(name, "the step holds no resources")
	}
	if r.PoolAcquireGoroutines.N == 0 {
		return failClause(name, "the sampler measured no pool acquire waiter")
	}
	return okClause(name, fmt.Sprintf("median %s, maximum %s over %s samples",
		num(r.PoolAcquireGoroutines.P50, 0), num(r.PoolAcquireGoroutines.Max, 0), num(float64(r.PoolAcquireGoroutines.N), 0)))
}

// statementsAndLocksClause: Postgres's slowest statements and its lock waits, which R6 names
// together and which one read of Postgres's own statistics takes together. No command of this
// package writes them today, so the clause names the file it reads them from and fails until the
// step holds it.
//
// A collector that never ran and one that ran and found nothing read differently. A step with no
// file at all measured neither. A step whose file holds no statement found none over an hour
// under load, which is a failed read of pg_stat_statements and not a quiet database. An empty
// lock_waits array, on the other hand, is a reading: a step where nothing waited for a lock is
// the step this test hopes for, so the clause asks only that the array be there.
func (sc *stepCheck) statementsAndLocksClause() stepClause {
	const name = "R6 Postgres's slowest statements and lock waits"
	lock := sc.step.sum.CommitOrderLock
	switch {
	case !sc.hasStatements:
		return failClause(name, "the step holds no "+pgStatementsFileName+", so nothing read them")
	case sc.statements.Statements == nil:
		return failClause(name, pgStatementsFileName+" holds no statements array")
	case len(*sc.statements.Statements) == 0:
		return failClause(name, pgStatementsFileName+" holds statements: [], so the read found no statement over a window under load")
	case sc.statements.LockWaits == nil:
		return failClause(name, pgStatementsFileName+" holds no lock_waits array")
	}
	statements, waits := *sc.statements.Statements, *sc.statements.LockWaits
	// The array is the step's own order, the statement that took the most time first, so the
	// first entry is the one a reader of the step looks at.
	top := statements[0]
	reading := fmt.Sprintf("%s, the first taking %s ms over %s calls (mean %s ms); %s",
		plural(len(statements), "statement"), num(top.TotalMs, 0), num(float64(top.Calls), 0), num(top.MeanMs, 1),
		plural(len(waits), "lock wait"))
	if longest := longestLockWait(waits); longest != nil {
		reading += fmt.Sprintf(", the longest %s ms", num(*longest, 1))
	}
	if lock.Samples > 0 {
		reading += fmt.Sprintf("; the commit-order lock's own waiters p50 %s, max %s over %s samples",
			num(lock.Waiters.P50, 0), num(lock.Waiters.Max, 0), num(float64(lock.Samples), 0))
	}
	return okClause(name, reading)
}

// profileClause: a CPU profile of Dispatch over the window, which vm.sh fetches from the machine
// Dispatch runs on. An empty file is no profile.
func (sc *stepCheck) profileClause() stepClause {
	const name = "R6 Dispatch's CPU profile"
	if sc.profileB == 0 {
		return failClause(name, "the step holds no "+cpuProfileFileName)
	}
	return okClause(name, fmt.Sprintf("%s, %s MiB", cpuProfileFileName, num(mib(float64(sc.profileB)), 2)))
}

// sizeClause: the database's size at the start of the step and at its end. No command of this
// package writes it today, so the clause names the file it reads it from and fails until the
// step holds it. A step with no file at all and a file whose figures are no measurement read
// differently: the seeded database is never empty, and a step adds rows and never removes a
// table, so a size of 0 or an end under the start is a read of another database or at another
// time rather than a size the step had.
func (sc *stepCheck) sizeClause() stepClause {
	const name = "R6 the database's size at the start and the end"
	start, end := sc.size.StartBytes, sc.size.EndBytes
	switch {
	case !sc.hasSize:
		return failClause(name, "the step holds no "+pgSizeFileName+", so nothing read it")
	case start <= 0 || end <= 0:
		return failClause(name, fmt.Sprintf("%s holds start_bytes %d and end_bytes %d, and a seeded database is neither empty nor unread",
			pgSizeFileName, start, end))
	case end < start:
		return failClause(name, fmt.Sprintf("%s holds end_bytes %s MiB under start_bytes %s MiB, which no step's writes can do",
			pgSizeFileName, num(mib(float64(end)), 0), num(mib(float64(start)), 0)))
	}
	return okClause(name, fmt.Sprintf("%s MiB at the start, %s MiB at the end, %s MiB added",
		num(mib(float64(start)), 0), num(mib(float64(end)), 0), num(mib(float64(end-start)), 0)))
}

// r8Clauses: a pass or a fail for each criterion of R8, which is the spec loop's series rule
// (curves.go loopSeries). A criterion the step's own load makes meaningless - no write offered,
// no read sent, no loop session - is n/a and no failure; a criterion that is n/a for want of a
// file the step should hold is a measurement the step lacks, so it fails here even though the
// curves would draw the step without it.
func (sc *stepCheck) r8Clauses() []stepClause {
	out := make([]stepClause, 0, len(loopSeries.crits)+1)
	for _, c := range loopSeries.crits {
		pc := passCriteria[c]
		name := fmt.Sprintf("R8 (%s) %s", pc.id, pc.text)
		switch sc.step.verdicts[c] {
		case verdictOK:
			out = append(out, okClause(name, "pass: "+sc.critReading(c)))
		case verdictFail:
			out = append(out, okClause(name, "fail: "+sc.critReading(c)))
		default:
			if why, ok := sc.naByLoad(c); ok {
				out = append(out, naClause(name, why))
				continue
			}
			out = append(out, failClause(name, "no pass and no fail: "+sc.critReading(c)))
		}
	}
	return append(out, sc.voidClause())
}

// naByLoad says why a criterion has no verdict because of what the step was told to drive, and
// false when the step should have given it one. A step that drove no write cannot be judged on
// its committed writes, and a step with no loop session cannot lose a notification one was owed;
// a missing cgroup.json or cloudsql-cpu.json is not such a reason.
func (sc *stepCheck) naByLoad(c int) (string, bool) {
	s := sc.step.sum
	switch c {
	case critCommits:
		return "the step offered no write", s.Events.OfferedPerS == 0
	case critRequests:
		return "the step was given no read target", s.Requests.TargetPerS == 0
	case critDelivery:
		return "the step offered no write", s.Config.EventsPerS == 0 && s.Events.Offered == 0
	case critLost:
		return "the step ran no loop session", s.LoopSessions == nil
	}
	return "", false
}

// critReading is the numbers one criterion was judged on, so a verdict is read with what it came
// from rather than on its own.
func (sc *stepCheck) critReading(c int) string {
	s := sc.step.sum
	switch c {
	case critCommits:
		if s.Events.commitsUnread() {
			return "the run's commit rows were not read"
		}
		return fmt.Sprintf("committed %s/s of %s/s offered", num(s.Events.CommittedPerS, 2), num(s.Events.OfferedPerS, 2))
	case critRequests:
		ok2xx := 0.0
		if s.Requests.Sent > 0 {
			ok2xx = 100 * float64(s.Requests.OK2xx) / float64(s.Requests.Sent)
		}
		return fmt.Sprintf("sent %s/s of %s/s target, %s%% of them 2xx",
			num(s.Requests.SentPerS, 2), num(s.Requests.TargetPerS, 2), num(ok2xx, 2))
	case critDelivery:
		if s.Events.DeliveryMsFromArrival.N == 0 {
			return "no delivery latency measured"
		}
		return fmt.Sprintf("p99 from arrival %s ms over %s receipts",
			num(s.Events.DeliveryMsFromArrival.P99, 0), num(float64(s.Events.DeliveryMsFromArrival.N), 0))
	case critCPULimit:
		if sc.step.cgroup == nil {
			return "the step has no cgroup.json"
		}
		if len(sc.step.overLimit) > 0 {
			return "over the limit: " + strings.Join(sc.step.overLimit, ", ")
		}
		return fmt.Sprintf("no container of the %d in cgroup.json reached %s of its limit",
			len(sc.step.cgroup.Roles), num(cpuLimitShare, 2))
	case critPool:
		r := s.Resources
		if r == nil || r.PoolAcquireGoroutines.N == 0 {
			return "the sampler measured no pool acquire waiter"
		}
		return fmt.Sprintf("pool acquire waiters p50 %s over %s samples",
			num(r.PoolAcquireGoroutines.P50, 0), num(float64(r.PoolAcquireGoroutines.N), 0))
	case critFailed:
		if sc.step.failed {
			return "the step failed: " + strings.Join(sc.step.failedKinds, ", ")
		}
		return "no step-failed.json, and cgroup.json is there and not failed"
	case critLost:
		if len(sc.step.lostClasses) > 0 {
			return "classes that lost a notification: " + strings.Join(sc.step.lostClasses, ", ")
		}
		if len(s.Events.ByClass) == 0 {
			return "the step records no session class"
		}
		return fmt.Sprintf("no class of the %d in events.by_class lost one", len(s.Events.ByClass))
	case critCloudSQLCPU:
		if sc.step.cloudsql == nil {
			return "the step has no " + cloudsqlCPUFileName
		}
		return fmt.Sprintf("cores mean %s of %s vCPUs",
			num(sc.step.cloudsql.CoresMean, 2), num(sc.step.cloudsql.LimitCores, 0))
	}
	return ""
}

// voidClause is R8's void rule: a step whose load generator ran at 0.7 or more of its machine's
// processors offered what the machine could send and not what the step asked for, so it measures
// the generator and not Dispatch and runs again with a larger one.
func (sc *stepCheck) voidClause() stepClause {
	const name = "R8 void rule: the load generator under 0.7 of its machine"
	if sc.step.void {
		return failClause(name, "void: "+sc.step.voidReason())
	}
	role := sc.step.sum.Config.GeneratorRole
	if role == "" || sc.step.cgroup == nil {
		return failClause(name, "no reading: the step has no config.generator_role with a cgroup.json role")
	}
	share, ok := limitShare(sc.step.role(role))
	if !ok {
		return failClause(name, "no reading: cgroup.json gives "+role+" no cores mean against its processors")
	}
	return okClause(name, fmt.Sprintf("%s at %s of its processors", role, num(share, 2)))
}

// --- the command ----------------------------------------------------------------------------

// printStepCheck writes the step's line, one line per clause and the verdict, and answers how
// many clauses failed.
func printStepCheck(w io.Writer, sc *stepCheck, clauses []stepClause) int {
	s := sc.step.sum
	fmt.Fprintf(w, "step %s: factor %s, %s s window after a %s s warm-up (%s)\n",
		sc.step.name, fmtFloat(sc.step.factor), num(s.Config.DurationS, 0), num(s.Config.WarmupS, 0), sc.dir)
	failed, na := 0, 0
	for _, c := range clauses {
		switch c.verdict {
		case clauseFail:
			failed++
		case clauseNA:
			na++
		}
		fmt.Fprintf(w, "%s: %s — %s\n", c.name, c.reading, c.verdict)
	}
	judged := len(clauses) - na
	if failed == 0 {
		fmt.Fprintf(w, "step-check %s: no clause failed, %d passed, %d n/a\n", sc.step.name, judged, na)
	} else {
		fmt.Fprintf(w, "step-check %s: %d of %d clauses failed, %d n/a\n", sc.step.name, failed, judged, na)
	}
	return failed
}

// runStepCheck judges one step directory against A3 and exits non-zero when a clause fails.
func runStepCheck(args []string) error {
	fs := flag.NewFlagSet("step-check", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: loadgen step-check <step dir>")
		fmt.Fprintln(fs.Output(), "  One step directory judged against acceptance A3 of dispatch://INFRA-988: every offered")
		fmt.Fprintln(fs.Output(), "  rate of R5, the unit starts and the unit sessions at once within 5% of the step's own")
		fmt.Fprintln(fs.Output(), "  targets, no proposal past 30 unit sessions, every measurement of R6 present, and a pass")
		fmt.Fprintln(fs.Output(), "  or a fail for each criterion of R8.")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		return fmt.Errorf("name one step directory")
	}
	sc, err := readStepCheck(fs.Arg(0))
	if err != nil {
		return err
	}
	clauses := sc.clauses()
	if failed := printStepCheck(os.Stdout, sc, clauses); failed > 0 {
		return fmt.Errorf("%d of %d clauses failed", failed, len(clauses))
	}
	return nil
}
