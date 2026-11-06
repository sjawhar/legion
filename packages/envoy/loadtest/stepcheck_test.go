package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The conforming step the fixtures are built from: the 2x step of the spec loop's series as A3
// asks for it, an hour's window after a five-minute warm-up, carrying production's own traffic
// (R5's 4 writes and 60 requests a second) and twice the loop's (7 writes, 77 reads and 3.5 unit
// starts a second, each session living 5 minutes, so 1,050 of them at once).
const (
	fixWarmupS   = 300.0
	fixDurationS = 3600.0
	fixFactor    = 2.0
	fixLoopWrite = r5LoopWritesPerS * fixFactor
	fixLoopRead  = r5LoopReadsPerS * fixFactor
	fixStarts    = r5LoopUnitStartsPerS * fixFactor
	fixUnitLifeS = 300.0
	fixGenerator = "vm-10.80.0.4"
)

// stepFixture is a step directory before it is written: the summary, the merged cgroup file, the
// Cloud SQL reading and the two Postgres files, each as the collectors leave them. A test changes
// what its own clause reads and writes the rest as it is, so one change is one failed clause.
// cloudsql, statements and size are written as JSON objects rather than structs, since a test
// takes a key out of them to make the file a collector that ran and found nothing.
type stepFixture struct {
	sum      summary
	cgroup   *cgroupFile
	cloudsql map[string]any
	// A nil map is a file the step does not hold at all.
	statements map[string]any
	size       map[string]any
	profile    []byte
}

// newStepFixture is a step that meets every clause of the check.
func newStepFixture() *stepFixture {
	return &stepFixture{
		sum:      fixtureLoopSummary(),
		cgroup:   fixtureLoopCgroup(),
		cloudsql: map[string]any{"limit_cores": 16.0, "cores_mean": 6.2, "cores_peak_60s": 9.1, "utilization_mean": 0.3875, "memory_bytes_mean": 21.0 * (1 << 30)},
		statements: map[string]any{
			"statements": []any{
				map[string]any{"query": "select * from comments where issue_id = $1", "calls": 412_000, "mean_ms": 18.4, "total_ms": 7_580_800},
				map[string]any{"query": "insert into comments ...", "calls": 190_000, "mean_ms": 9.1, "total_ms": 1_729_000},
			},
			"lock_waits": []any{
				map[string]any{"lock_type": "transactionid", "target": "xid 912", "mode": "ShareLock",
					"waiter_query": "insert into events ...", "blocker_query": "update issues ...",
					"observations": 12, "seen_for_s": 4.5, "wait_ms_max": 96, "wait_ms_mean": 40},
			},
		},
		size:    map[string]any{"start_bytes": 61 << 30, "end_bytes": 63 << 30},
		profile: []byte("a CPU profile of Dispatch"),
	}
}

// fixtureLoopSummary is the conforming step's summary.json: both populations at R5's rates for
// factor 2, every rate met, every latency measured, nothing lost and no container near its limit.
func fixtureLoopSummary() summary {
	ev, req := r5BaselineEventsPerS+fixLoopWrite, r5BaselineRequestsPerS+fixLoopRead
	offered, sent := int(ev*fixDurationS), int(req*fixDurationS)
	units := int(fixStarts * (fixWarmupS + fixDurationS))
	receipts := 120_000
	writeMs := stat{N: offered, P50: 22, P90: 60, P99: 140, Max: 900}
	readMs := stat{N: sent, P50: 9, P90: 30, P99: 85, Max: 700}
	kind := func(share float64) kindSummary {
		n := int(float64(offered) * share)
		return kindSummary{PrimaryType: "comment.created", Offered: n, Committed: n, WriteMs: writeMs,
			DeliveryMsFromArrival: stat{N: n, P50: 40, P99: 180, Max: 500},
			DeliveryMsFromCommit:  stat{N: n, P50: 20, P99: 120, Max: 400}}
	}
	route := func() routeSummary {
		return routeSummary{Count: sent / 7, LatencyMs: readMs, Status: map[string]int{"200": sent / 7}}
	}
	events := eventsSummary{
		Offered: offered, OfferedPerS: ev, Committed: offered, CommittedPerS: ev,
		ByKind: map[string]kindSummary{
			kindVerdict: kind(0.2), kindRequirement: kind(0.1), kindStatus: kind(0.1),
			kindComment: kind(0.3), kindAnchored: kind(0.3),
		},
		DeliveryMsFromArrival: stat{N: receipts, P50: 40, P90: 120, P99: 180, Max: 900},
		DeliveryMsFromCommit:  stat{N: receipts, P50: 20, P90: 80, P99: 120, Max: 800},
		DeliveryMsFromPublish: stat{N: receipts, P50: 12, P90: 40, P99: 70, Max: 400},
		IssueTopicReceipts:    receipts, ExpectedIssueTopicReceipts: receipts,
		ByClass: map[string]classDelivery{
			deliveryClassUnit:   {Sessions: units, Expected: receipts - 4_000, Received: receipts - 4_000, Duplicate: 9},
			deliveryClassDriver: {Sessions: 1, Expected: 4_000, Received: 4_000, Boundary: 3},
		},
	}
	requests := requestsSummary{TargetPerS: req, Sent: sent, SentPerS: req, OK2xx: sent, LatencyMs: readMs,
		Routes: map[string]routeSummary{
			"loop GET /api/v1/issues/{key}/comments":                    route(),
			"loop GET /api/v1/artifacts/{review_page_artifact_id}/text": route(),
			"loop GET /api/v1/search?q={search_term}&project={project}": route(),
			"browser GET /api/v1/issues":                                route(),
		}}
	population := func(name string, evs, reqs float64) populationSummary {
		off, st := int(evs*fixDurationS), int(reqs*fixDurationS)
		return populationSummary{Name: name, EventsPerS: evs, RequestsPerS: reqs,
			Events:   eventsSummary{Offered: off, OfferedPerS: evs, Committed: off, CommittedPerS: evs},
			Requests: requestsSummary{TargetPerS: reqs, Sent: st, SentPerS: reqs, OK2xx: st}}
	}
	return summary{
		Config: summaryConfig{EventsPerS: ev, RequestsPerS: req, Sessions: 400, LoopFactor: fixFactor,
			WarmupS: fixWarmupS, DurationS: fixDurationS, GraceS: 20, GeneratorRole: fixGenerator,
			LoopSeed: "loopseed.seed.json", SessionDelivery: "consumer"},
		Events:   events,
		Requests: requests,
		Populations: []populationSummary{
			population(populationBaseline, r5BaselineEventsPerS, r5BaselineRequestsPerS),
			population(populationLoop, fixLoopWrite, fixLoopRead),
		},
		Resources: &resourcesSummary{WindowS: fixDurationS,
			CPUCoresMean: map[string]float64{"loadgen": 1.4}, RSSMiBMax: map[string]float64{"loadgen": 420},
			PoolAcquireGoroutines: stat{N: 720, P50: 0, P90: 1, P99: 2, Max: 3},
			PGConnsTotal:          stat{N: 720, P50: 120, Max: 240},
			HeapLiveMiB:           stat{N: 240, P50: 900, Max: 1400}},
		CommitOrderLock: lockSummary{Samples: 25_000, Waiters: stat{N: 25_000, P50: 0, P90: 2, Max: 7}, HeldFraction: 0.18},
		UngrantedLocks:  ungrantedSummary{Samples: 900, ShareWithAny: 0.02},
		Sessions:        sessionsSummary{Registered: 400, NATSSubscriptions: 800},
		LoopSessions: &loopSessionsSummary{
			UnitStartsPerS: fixStarts, UnitLifeS: fixUnitLifeS, ReceiptDeadlineMs: 1000, Listeners: 4,
			Started: int64(units + 1), Stopped: int64(units), ExitMessages: int64(units),
			StartMs:       stat{N: units, P50: 30, P90: 70, P99: 140, Max: 900},
			SubscribeMs:   stat{N: units, P50: 12, P90: 30, P99: 80, Max: 400},
			UnsubscribeMs: stat{N: units, P50: 11, P90: 28, P99: 75, Max: 380},
			Concurrent: stat{N: int(fixWarmupS + fixDurationS), P50: fixStarts * fixUnitLifeS,
				P90: 1080, P99: 1100, Max: 1120},
			MostOnOneProposal: 28, Spilled: 140, DriverTopics: 614, DriverSubscribed: true},
		Loop: &loopRun{Requests: []loopRequests{{Class: loopClassVerdict, Requests: 2000}},
			Reopens: 240, CapExceeded: 12, NewParts: 3, PageReads: 800},
	}
}

// fixtureLoopCgroup is the deployment's cgroup.json: the three application containers, the four
// listeners on the load machine, the hub's, NATS and the machines themselves, each under its
// limit.
func fixtureLoopCgroup() *cgroupFile {
	role := func(cores, limit float64) cgroupRole {
		peak, ws := cores*1.4, float64(2<<30)
		wsMax := int64(3 << 30)
		return cgroupRole{Samples: 720, CoresMean: &cores, CoresPeak5s: &peak, LimitCores: &limit,
			WorkingSetBytesMean: &ws, WorkingSetBytesMax: &wsMax}
	}
	return &cgroupFile{WindowS: fixDurationS, Roles: map[string]cgroupRole{
		dispatchRoleName: role(7.4, 16), authProxyRoleName: role(0.08, 0.2), "dispatch-listener": role(0.12, 0.5),
		"hub-listener": role(0.14, 0.5), "load-listener-1": role(0.2, 0.5), "load-listener-2": role(0.2, 0.5),
		"load-listener-3": role(0.19, 0.5), "load-listener-4": role(0.05, 0.5), natsRoleName: role(0.9, 2),
		"vm-10.80.0.2": role(8.1, 32), "vm-10.80.0.3": role(1.1, 2), fixGenerator: role(4.2, 16),
	}}
}

// write leaves the fixture in a step directory of its own and answers it.
func (f *stepFixture) write(t *testing.T, name string) string {
	t.Helper()
	dir := curveFixture(t, t.TempDir(), name, f.sum, f.cgroup)
	for file, v := range map[string]map[string]any{
		cloudsqlCPUFileName: f.cloudsql, pgStatementsFileName: f.statements, pgSizeFileName: f.size,
	} {
		if v == nil {
			continue
		}
		writeFixtureJSON(t, filepath.Join(dir, file), v)
	}
	if len(f.profile) > 0 {
		if err := os.WriteFile(filepath.Join(dir, cpuProfileFileName), f.profile, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// check runs the check over the written step, as the command does, and answers its output and
// the clauses that failed.
func (f *stepFixture) check(t *testing.T, name string) (string, int) {
	t.Helper()
	sc, err := readStepCheck(f.write(t, name))
	if err != nil {
		t.Fatalf("step-check: %v", err)
	}
	var b strings.Builder
	failed := printStepCheck(&b, sc, sc.clauses())
	return b.String(), failed
}

// clauseLine is the output's line for the named clause, which must be there: a clause the check
// stopped printing is a clause nothing judges.
func clauseLine(t *testing.T, out, name string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, name+": ") {
			return line
		}
	}
	t.Fatalf("the check printed no clause %q:\n%s", name, out)
	return ""
}

// wantClause is the named clause's line, which must hold each of want and end in verdict.
func wantClause(t *testing.T, out, name, verdict string, want ...string) {
	t.Helper()
	line := clauseLine(t, out, name)
	if !strings.HasSuffix(line, " — "+verdict) {
		t.Errorf("clause %q reads %q, want it to end %q", name, line, verdict)
	}
	for _, w := range want {
		if !strings.Contains(line, w) {
			t.Errorf("clause %q reads %q, want %q in it", name, line, w)
		}
	}
}

// TestStepCheckPassesAConformingStep: the 2x step as A3 asks for it - every offered rate within
// 5% of its target, no proposal past the bound, every measurement of R6 recorded and a pass or a
// fail for each criterion of R8 - fails no clause, and the command exits 0 on it.
func TestStepCheckPassesAConformingStep(t *testing.T) {
	f := newStepFixture()
	dir := f.write(t, "2x")
	sc, err := readStepCheck(dir)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	clauses := sc.clauses()
	if failed := printStepCheck(&b, sc, clauses); failed != 0 {
		t.Errorf("%d clauses failed on a conforming step:\n%s", failed, b.String())
	}
	out := b.String()
	for _, c := range clauses {
		if c.verdict != clauseOK {
			t.Errorf("clause %q is %s on a conforming step: %s", c.name, c.verdict, c.reading)
		}
	}
	for _, want := range []string{
		"step 2x: factor 2, 3,600 s window after a 300 s warm-up",
		"R5 baseline target: writes 4.00/s of R5's 4.00/s (+0.00%), reads 60.00/s of R5's 60.00/s (+0.00%) — ok",
		"R5 loop target: factor 2 from config.loop_factor: writes 7.00/s = 2x (+0.00%), reads 77.00/s = 2x (+0.00%), unit starts 3.50/s = 2x (+0.00%) — ok",
		"R5 unit sessions on one proposal: most on one proposal 28, bound 30",
		"R8 (c) delivery p99 from arrival ≤ 1,000 ms: pass: p99 from arrival 180 ms over 120,000 receipts — ok",
		"no clause failed, ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the check's output lacks %q:\n%s", want, out)
		}
	}
	if err := runStepCheck([]string{dir}); err != nil {
		t.Errorf("the command refused a conforming step: %v", err)
	}
}

// TestStepCheckFlagsAHalfReadRate is A3's own negative control: a step run with the read rate set
// to half its target. Its generator met the target it was given, so only the target itself says
// what the step was: the loop's reads read as factor 1 where its writes and its unit starts read
// as 2, and the clause names the rate and the factor it stood at.
func TestStepCheckFlagsAHalfReadRate(t *testing.T) {
	f := newStepFixture()
	half := fixLoopRead / 2
	req := r5BaselineRequestsPerS + half
	f.sum.Config.RequestsPerS = req
	f.sum.Requests.TargetPerS = req
	f.sum.Requests.Sent = int(req * fixDurationS)
	f.sum.Requests.SentPerS = req
	f.sum.Requests.OK2xx = f.sum.Requests.Sent
	loop := &f.sum.Populations[1]
	loop.RequestsPerS = half
	loop.Requests = requestsSummary{TargetPerS: half, Sent: int(half * fixDurationS), SentPerS: half, OK2xx: int(half * fixDurationS)}

	out, failed := f.check(t, "2x-half-reads")
	if failed != 1 {
		t.Errorf("%d clauses failed, want the loop target alone:\n%s", failed, out)
	}
	wantClause(t, out, "R5 loop target", "FAIL",
		"factor 2 from config.loop_factor", "reads 38.50/s = 1x (-50.00%)", "writes 7.00/s = 2x (+0.00%)")
	// The step's own generator met what it was asked for, so the measured clauses stand: the
	// control is caught by the target and not by a shortfall that never happened.
	wantClause(t, out, "R5 reads sent", "ok")
	wantClause(t, out, "R5 reads sent (loop)", "ok")
	if err := runStepCheck([]string{f.write(t, "2x-half-reads-again")}); err == nil {
		t.Error("the command passed a step whose read rate is half its target")
	}
}

// TestStepCheckNamesAMissingR6Measurement: one measurement of R6 left out of the step's files
// fails its own clause and names what is missing, the other clauses standing. A collector that
// ran and found nothing and one that never ran are told apart.
func TestStepCheckNamesAMissingR6Measurement(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(*stepFixture)
		clause string
		want   []string
	}{
		{"no unsubscribe latency", func(f *stepFixture) { f.sum.LoopSessions.UnsubscribeMs = stat{} },
			"R6 unsubscribe latency", []string{"no sample"}},
		{"no write latency for one kind", func(f *stepFixture) {
			k := f.sum.Events.ByKind[kindStatus]
			k.WriteMs = stat{}
			f.sum.Events.ByKind[kindStatus] = k
		}, "R6 write latency by kind", []string{"no latency for status"}},
		{"no statements file", func(f *stepFixture) { f.statements = nil },
			"R6 Postgres's slowest statements and lock waits", []string{"holds no pg-statements.json", "nothing read them"}},
		{"a statements file of no statement", func(f *stepFixture) { f.statements["statements"] = []any{} },
			"R6 Postgres's slowest statements and lock waits", []string{"statements: []", "found no statement"}},
		{"no lock waits in it", func(f *stepFixture) { delete(f.statements, "lock_waits") },
			"R6 Postgres's slowest statements and lock waits", []string{"holds no lock_waits array"}},
		{"no Cloud SQL memory", func(f *stepFixture) { delete(f.cloudsql, "memory_bytes_mean") },
			"R6 Postgres CPU and memory", []string{"cores mean 6.20 of 16 vCPUs", "holds no memory_bytes_mean"}},
		{"no database size", func(f *stepFixture) { f.size = nil },
			"R6 the database's size at the start and the end", []string{"holds no pg-size.json"}},
		{"a database that shrank", func(f *stepFixture) { f.size["end_bytes"] = 60 << 30 },
			"R6 the database's size at the start and the end", []string{"under start_bytes", "61,440 MiB"}},
		{"no CPU profile", func(f *stepFixture) { f.profile = nil },
			"R6 Dispatch's CPU profile", []string{"holds no cpu.pprof"}},
		{"no pool waiter sample", func(f *stepFixture) { f.sum.Resources.PoolAcquireGoroutines = stat{} },
			"R6 requests waiting for a database connection", []string{"measured no pool acquire waiter"}},
		{"no listener role", func(f *stepFixture) {
			for role := range f.cgroup.Roles {
				if strings.Contains(role, listenerRoleMark) {
					delete(f.cgroup.Roles, role)
				}
			}
		}, "R6 listener CPU and memory", []string{"no role whose name carries listener"}},
		{"no memory for the Auth Proxy", func(f *stepFixture) {
			r := f.cgroup.Roles[authProxyRoleName]
			r.WorkingSetBytesMax = nil
			f.cgroup.Roles[authProxyRoleName] = r
		}, "R6 the Auth Proxy CPU and memory", []string{"working set max none"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newStepFixture()
			c.change(f)
			out, failed := f.check(t, "2x")
			wantClause(t, out, c.clause, "FAIL", c.want...)
			if failed != 1 {
				t.Errorf("%d clauses failed, want %q alone:\n%s", failed, c.clause, out)
			}
		})
	}
}

// TestStepCheckFlagsAProposalOverTheBound: R5 holds a proposal to 30 unit sessions at once, so a
// step whose placement went past it fails that clause and no other.
func TestStepCheckFlagsAProposalOverTheBound(t *testing.T) {
	f := newStepFixture()
	f.sum.LoopSessions.MostOnOneProposal = unitCapPerProposal + 1
	out, failed := f.check(t, "2x-over-the-bound")
	wantClause(t, out, "R5 unit sessions on one proposal", "FAIL", "most on one proposal 31, bound 30")
	if failed != 1 {
		t.Errorf("%d clauses failed, want the bound alone:\n%s", failed, out)
	}
	if !strings.Contains(out, "1 of ") || !strings.Contains(out, "clauses failed") {
		t.Errorf("the verdict line does not count the failure:\n%s", out)
	}
}

// TestStepCheckFlagsAnR8CriterionWithNoVerdict: A3 asks for a pass or a fail for each criterion
// of R8, so a step that holds nothing to judge one by fails, where the curves would simply draw
// it as n/a. A step with no cloudsql-cpu.json gives criterion (h) nothing, and the Cloud SQL
// reading R6 asks for goes with it.
func TestStepCheckFlagsAnR8CriterionWithNoVerdict(t *testing.T) {
	f := newStepFixture()
	f.cloudsql = nil
	out, failed := f.check(t, "2x-no-cloudsql")
	wantClause(t, out, "R8 (h) the Cloud SQL instance's cores mean < 0.9 × its vCPUs, from cloudsql-cpu.json",
		"FAIL", "no pass and no fail", "the step has no cloudsql-cpu.json")
	wantClause(t, out, "R6 Postgres CPU and memory", "FAIL", "holds no cloudsql-cpu.json")
	if failed != 2 {
		t.Errorf("%d clauses failed, want the Cloud SQL reading and criterion (h):\n%s", failed, out)
	}
}

// TestStepCheckJudgesTheBaselineStepByWhatItDrove: the series' first step carries production's
// own traffic alone, so the loop's rates, its sessions and the notifications they were owed are
// n/a rather than failures, and the step passes.
func TestStepCheckJudgesTheBaselineStepByWhatItDrove(t *testing.T) {
	f := newStepFixture()
	s := &f.sum
	s.Config.EventsPerS, s.Config.RequestsPerS, s.Config.LoopFactor = r5BaselineEventsPerS, r5BaselineRequestsPerS, 0
	offered, sent := int(r5BaselineEventsPerS*fixDurationS), int(r5BaselineRequestsPerS*fixDurationS)
	s.Events.Offered, s.Events.OfferedPerS = offered, r5BaselineEventsPerS
	s.Events.Committed, s.Events.CommittedPerS = offered, r5BaselineEventsPerS
	s.Events.ByClass = nil
	s.Requests.TargetPerS, s.Requests.SentPerS = r5BaselineRequestsPerS, r5BaselineRequestsPerS
	s.Requests.Sent, s.Requests.OK2xx = sent, sent
	s.Populations = []populationSummary{
		{Name: populationBaseline, EventsPerS: r5BaselineEventsPerS, RequestsPerS: r5BaselineRequestsPerS,
			Events:   eventsSummary{Offered: offered, OfferedPerS: r5BaselineEventsPerS, Committed: offered, CommittedPerS: r5BaselineEventsPerS},
			Requests: requestsSummary{TargetPerS: r5BaselineRequestsPerS, Sent: sent, SentPerS: r5BaselineRequestsPerS, OK2xx: sent}},
		{Name: populationLoop},
	}
	s.LoopSessions = nil

	out, failed := f.check(t, "baseline")
	if failed != 0 {
		t.Errorf("%d clauses failed on the baseline step:\n%s", failed, out)
	}
	for _, name := range []string{"R5 loop target", "R5 unit starts", "R5 concurrent unit sessions",
		"R5 unit sessions on one proposal", "R6 delivery by session class", "R6 session start latency",
		"R6 unit sessions at the same time",
		"R8 (g) no notification a loop session was owed is lost: every class of events.by_class lost 0"} {
		wantClause(t, out, name, "n/a")
	}
	wantClause(t, out, "R5 baseline target", "ok", "writes 4.00/s of R5's 4.00/s")
}
