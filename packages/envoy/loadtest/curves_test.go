package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// curveWinStart is the window start of the curves fixtures.
var curveWinStart = time.Date(2026, 10, 28, 14, 0, 0, 0, time.UTC)

// curveFixture writes the step directory root/name as a cluster step leaves it: summary.json from
// sum (its config.step set to name), meta.json with a window of the summary's duration, cgroup.json
// from cg when not nil, and step-failed.json with reasons when there are any.
func curveFixture(t *testing.T, root, name string, sum summary, cg *cgroupFile, reasons ...stepFailedReason) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sum.Config.Step = name
	end := curveWinStart.Add(time.Duration(sum.Config.DurationS * float64(time.Second)))
	files := map[string]any{"summary.json": sum, "meta.json": metaFile{Step: name, WindowStart: curveWinStart, WindowEnd: end}}
	if cg != nil {
		files["cgroup.json"] = cg
	}
	if len(reasons) > 0 {
		files["step-failed.json"] = map[string]any{"reasons": reasons}
	}
	for file, v := range files {
		writeFixtureJSON(t, filepath.Join(dir, file), v)
	}
	return dir
}

func writeFixtureJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// withCopyMeta rewrites the step directory's meta.json with the copy a cluster step records
// (cluster.sh's write_copy_meta): the phase, the reads-only flag and the copy's Postgres tier.
// It returns dir.
func withCopyMeta(t *testing.T, dir, phase string, readsOnly bool, pgCPUs string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m metaFile
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	m.Copy = json.RawMessage(fmt.Sprintf(`{"namespace":"loadtest","phase":%q,"reads_only":%t,"postgres":{"tier":"db-custom-%s","cpu":%q}}`,
		phase, readsOnly, pgCPUs, pgCPUs))
	writeFixtureJSON(t, filepath.Join(dir, "meta.json"), m)
	return dir
}

// withCopyMetaNoPhase rewrites the step directory's meta.json with the copy a run recorded before
// cluster.sh wrote the phase: the copy's Postgres tier and no phase. It returns dir.
func withCopyMetaNoPhase(t *testing.T, dir, pgCPUs string) string {
	t.Helper()
	var m metaFile
	if err := json.Unmarshal([]byte(readText(t, filepath.Join(dir, "meta.json"))), &m); err != nil {
		t.Fatal(err)
	}
	m.Copy = json.RawMessage(fmt.Sprintf(`{"namespace":"loadtest","postgres":{"tier":"db-custom-%s","cpu":%q}}`, pgCPUs, pgCPUs))
	writeFixtureJSON(t, filepath.Join(dir, "meta.json"), m)
	return dir
}

// goodSummary is a 10-minute step at factor f that passes every criterion: all events committed
// and delivered within 180 ms at p99, every request 2xx at its target, no pool waiter.
func goodSummary(f float64) summary {
	const dur = 600.0
	ev, req := 4*f, 60*f
	sent := int(req * dur)
	committed := int(ev * dur)
	return summary{
		Config: summaryConfig{EventsPerS: ev, RequestsPerS: req, DurationS: dur},
		Events: eventsSummary{
			Offered: committed, OfferedPerS: ev, Committed: committed, CommittedPerS: ev,
			DeliveryMsFromArrival: stat{N: committed * 4, P50: 40, P90: 90, P99: 180, Max: 400},
			DeliveryMsFromCommit:  stat{N: committed * 4, P50: 20, P90: 50, P99: 120, Max: 300},
			IssueTopicReceipts:    committed * 4, ExpectedIssueTopicReceipts: committed * 4,
		},
		Requests: requestsSummary{TargetPerS: req, Sent: sent, SentPerS: req, OK2xx: sent, LatencyMs: stat{N: sent, P50: 12, P99: 80, Max: 300}},
		Resources: &resourcesSummary{
			PoolAcquireGoroutines: stat{N: 120},
			HeapLiveMiB:           stat{N: 40, P50: 300, Max: 420},
			PGConnsTotal:          stat{N: 120, P50: 20, Max: 34},
			PGConnsByState: map[string]stat{
				"active|":      {N: 120, P50: 3, Max: 9},
				"active|Lock":  {N: 120, P50: 1, Max: 4},
				"idle|Client":  {N: 120, P50: 15, Max: 25},
				"idle in tx|x": {N: 120, P50: 1, Max: 2},
			},
		},
		CommitOrderLock: lockSummary{Samples: 25000, Waiters: stat{N: 25000, P50: 1, Max: 6}, HeldFraction: 0.12},
	}
}

// collapsed is goodSummary(f) whose committed events fell to a tenth of the offered.
func collapsed(f float64) summary {
	s := goodSummary(f)
	s.Events.CommittedPerS = s.Events.OfferedPerS / 10
	s.Events.Committed = s.Events.Offered / 10
	return s
}

// fixtureCgroup is a cgroup.json at production's limits with Dispatch at dispatchCores.
func fixtureCgroup(dispatchCores float64) *cgroupFile {
	role := func(cores, limit float64) cgroupRole {
		peak, ws, wsMax := cores*1.5, float64(3<<30), int64(4<<30)
		return cgroupRole{CoresMean: &cores, CoresPeak5s: &peak, LimitCores: &limit, WorkingSetBytesMean: &ws, WorkingSetBytesMax: &wsMax}
	}
	return &cgroupFile{WindowS: 600, Roles: map[string]cgroupRole{
		"dispatch": role(dispatchCores, 16), "postgres": role(2, 16), "listener": role(0.1, 0.5),
		"nats-0": role(0.2, 2), "nats-1": role(0.25, 2), "nats-2": role(0.2, 2), "runner": role(1, 8), "pprof": role(0.001, 0.05),
	}}
}

// curves runs loadgen curves on dirs and returns the output directory.
func curves(t *testing.T, dirs ...string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "curves")
	if err := runCurves(append([]string{"-out", out}, dirs...)); err != nil {
		t.Fatalf("curves: %v", err)
	}
	return out
}

func readText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// curvesCSV reads curves.csv: the step names in row order, and each step's cells by column.
func curvesCSV(t *testing.T, out string) ([]string, map[string]map[string]string) {
	t.Helper()
	f, err := os.Open(filepath.Join(out, "curves.csv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	byStep := map[string]map[string]string{}
	for _, row := range rows[1:] {
		cells := map[string]string{}
		for i, h := range rows[0] {
			cells[h] = row[i]
		}
		order = append(order, row[0])
		byStep[row[0]] = cells
	}
	return order, byStep
}

// TestCurvesNameTheKneeWhereCommitsCollapse: 1x passes and 2x commits a tenth of what it offers:
// one knee at 2x, bracket (1, 2), the committed rates at both steps as its limit, and no
// refinement for a bracket 1.0 wide.
func TestCurvesNameTheKneeWhereCommitsCollapse(t *testing.T) {
	root := t.TempDir()
	out := curves(t,
		curveFixture(t, root, "2x", collapsed(2), fixtureCgroup(3)),
		curveFixture(t, root, "1x", goodSummary(1), fixtureCgroup(2)))

	knees := readText(t, filepath.Join(out, "KNEES.md"))
	for _, want := range []string{
		"## Knee at 2x (step 2x)",
		"  - (a) committed events/s ≥ 0.95 × offered",
		"- Bracket: (1, 2): last passing factor 1 (step 1x), failing factor 2.",
		"  - (a) events/s committed: 0.80 at 2x (offered 8.00), 4.00 at 1x.",
		filepath.Join(root, "2x", "cpu.top.txt") + "` (absent)",
	} {
		if !strings.Contains(knees, want) {
			t.Errorf("KNEES.md lacks %q:\n%s", want, knees)
		}
	}
	if n := strings.Count(knees, "## Knee at"); n != 1 {
		t.Errorf("KNEES.md names %d knees, want 1:\n%s", n, knees)
	}
	if !strings.Contains(readText(t, filepath.Join(out, "CURVES.md")), "| 2x | 2 | FAIL | ok | ok | ok | ok | ok | no |") {
		t.Errorf("CURVES.md's Pass table lacks 2x failing (a) alone")
	}
	if refine := readText(t, filepath.Join(out, "refine.txt")); refine != "" {
		t.Errorf("refine.txt = %q, want empty for a bracket 1.0 wide", refine)
	}
	order, rows := curvesCSV(t, out)
	if strings.Join(order, ",") != "1x,2x" || rows["2x"]["events_committed_per_s"] != "0.8" || rows["1x"]["active_connections_max"] != "13" {
		t.Errorf("curves.csv: order %v, 2x committed %q, 1x active max %q; want 1x,2x, 0.8, 13", order, rows["2x"]["events_committed_per_s"], rows["1x"]["active_connections_max"])
	}
}

// TestCurvesNameAKneeAtAFailingFirstStep: when 1x fails, the knee is 1x, with no passing step.
func TestCurvesNameAKneeAtAFailingFirstStep(t *testing.T) {
	root := t.TempDir()
	bad := goodSummary(1)
	bad.Requests.OK2xx = bad.Requests.Sent / 2
	out := curves(t,
		curveFixture(t, root, "1x", bad, fixtureCgroup(2)),
		curveFixture(t, root, "2x", goodSummary(2), fixtureCgroup(2)))
	knees := readText(t, filepath.Join(out, "KNEES.md"))
	for _, want := range []string{
		"## Knee at 1x (step 1x)",
		"- Bracket: (none, 1): the series' first judged step fails, so no step passed before it.",
		"  - (b) requests/s succeeded (2xx): 30.0 at 1x (target 60.0, sent 60.0); no step passed before it.",
	} {
		if !strings.Contains(knees, want) {
			t.Errorf("KNEES.md lacks %q:\n%s", want, knees)
		}
	}
	if n := strings.Count(knees, "## Knee at"); n != 1 {
		t.Errorf("KNEES.md names %d knees, want 1:\n%s", n, knees)
	}
}

// TestCurvesShowAFailedRowWithoutNumbers: a step whose container restarted fails (f), shows its
// failure kind and no number in any series table or in curves.csv, and is no knee: KNEES.md
// lists it as not judged.
func TestCurvesShowAFailedRowWithoutNumbers(t *testing.T) {
	root := t.TempDir()
	out := curves(t,
		curveFixture(t, root, "1x", goodSummary(1), fixtureCgroup(2)),
		curveFixture(t, root, "2x", goodSummary(2), fixtureCgroup(2),
			stepFailedReason{Kind: "restart", Role: "dispatch", Detail: "restartCount 0 -> 1 (OOMKilled)"}))
	md := readText(t, filepath.Join(out, "CURVES.md"))
	series, _, _ := strings.Cut(md, "\n## Pass")
	const label = "| 2x (failed: restart) | 2 |"
	rows := 0
	for line := range strings.Lines(series) {
		rest, ok := strings.CutPrefix(line, label)
		if !ok {
			continue
		}
		rows++
		for cell := range strings.SplitSeq(strings.Trim(strings.TrimSpace(rest), "|"), "|") {
			if strings.TrimSpace(cell) != "-" {
				t.Errorf("the failed row shows a number: %s", line)
				break
			}
		}
	}
	if rows != len(curveTables) {
		t.Errorf("the failed row is in %d series tables, want %d", rows, len(curveTables))
	}
	if !strings.Contains(md, "| 2x (failed: restart) | 2 | n/a | n/a | n/a | n/a | n/a | FAIL | no |") {
		t.Errorf("the Pass table does not fail (f) alone for the failed row:\n%s", md)
	}
	_, csvRows := curvesCSV(t, out)
	r := csvRows["2x"]
	if r["failed"] != "restart" || r["dispatch_cores_mean"] != "" || r["events_offered_per_s"] != "" || r["pass_f_no_failure"] != "FAIL" {
		t.Errorf("curves.csv 2x: failed %q, dispatch cores %q, offered %q, (f) %q; want restart, empty, empty, FAIL",
			r["failed"], r["dispatch_cores_mean"], r["events_offered_per_s"], r["pass_f_no_failure"])
	}
	knees := readText(t, filepath.Join(out, "KNEES.md"))
	if !strings.Contains(knees, "- 2x (step 2x): not judged (failed: restart).") || strings.Contains(knees, "## Knee at") {
		t.Errorf("KNEES.md does not list the failed row as not judged, or makes it a knee:\n%s", knees)
	}
}

// TestCurvesWalkTheKneesPastAFailedRow: 1x, 2x and 3x pass, 5x failed its sampler and 7x has a
// delivery p99 of 5,000 ms: the knee is 7x over the bracket (3, 7), the last judged step that
// passed; 5x is listed as not judged and ends no bracket, so the refinement narrows (3, 7) and
// not (3, 5).
func TestCurvesWalkTheKneesPastAFailedRow(t *testing.T) {
	root := t.TempDir()
	slow := goodSummary(7)
	slow.Events.DeliveryMsFromArrival.P99 = 5000
	dirs := []string{
		curveFixture(t, root, "1x", goodSummary(1), fixtureCgroup(2)),
		curveFixture(t, root, "2x", goodSummary(2), fixtureCgroup(3)),
		curveFixture(t, root, "3x", goodSummary(3), fixtureCgroup(4)),
		curveFixture(t, root, "5x", goodSummary(5), fixtureCgroup(5),
			stepFailedReason{Kind: stepFailedKindSamplerGap, Role: "postgres", Detail: "no good sample for 45 s"}),
		curveFixture(t, root, "7x", slow, fixtureCgroup(6)),
	}
	out := curves(t, dirs...)
	knees := readText(t, filepath.Join(out, "KNEES.md"))
	for _, want := range []string{
		"## Knee at 7x (step 7x)",
		"  - (c) delivery p99 from arrival ≤ 1,000 ms",
		"- Bracket: (3, 7): last passing factor 3 (step 3x), failing factor 7.",
		"- 5x (step 5x): not judged (failed: sampler_gap).",
	} {
		if !strings.Contains(knees, want) {
			t.Errorf("KNEES.md lacks %q:\n%s", want, knees)
		}
	}
	if n := strings.Count(knees, "## Knee at"); n != 1 || strings.Contains(knees, "## Knee at 5x") {
		t.Errorf("KNEES.md names %d knees, want the one at 7x:\n%s", n, knees)
	}
	// The bracket (3, 7) has no refinement step in the grid interval 3 to 7; its midpoint 5 is
	// the failed step's factor, so the factor is 5.5. The old walk refined (3, 5) and asked for 4.
	if got := readText(t, filepath.Join(out, "refine.txt")); got != "5.5\n" {
		t.Errorf("refine.txt = %q, want %q", got, "5.5\n")
	}
	if md := readText(t, filepath.Join(out, "CURVES.md")); !strings.Contains(md, "| 5x (failed: sampler_gap) | 5 | n/a | n/a | n/a | n/a | n/a | FAIL | no |") {
		t.Errorf("the Pass table does not show the failed row failing (f) alone:\n%s", md)
	}
}

// TestCurvesTakeAStepsKindFromTheRun: with meta.json's copy, the run says where a step sits, not
// its name: a step on a 4 vCPU copy is the db4 row however it is named, a step named -db4 on the
// series' 16 vCPU copy is in the knee series, the phase soak is the hour, and a reads-only step
// is the control.
func TestCurvesTakeAStepsKindFromTheRun(t *testing.T) {
	root := t.TempDir()
	out := curves(t,
		withCopyMeta(t, curveFixture(t, root, "1x-other", goodSummary(1), fixtureCgroup(2)), "db4", false, "4"),
		withCopyMeta(t, curveFixture(t, root, "5x-db4", goodSummary(5), fixtureCgroup(2)), "series", false, "16"),
		withCopyMeta(t, curveFixture(t, root, "10x-long", goodSummary(10), fixtureCgroup(2)), "soak", false, "16"),
		withCopyMeta(t, curveFixture(t, root, "2x-quiet", goodSummary(2), fixtureCgroup(2)), "steps", true, "16"))
	order, rows := curvesCSV(t, out)
	for _, c := range []struct{ step, want string }{
		{"1x-other", "db4"}, {"5x-db4", "grid"}, {"10x-long", "hour"}, {"2x-quiet", "control"},
	} {
		if got := rows[c.step]["kind"]; got != c.want {
			t.Errorf("step %s: kind %q, want %q", c.step, got, c.want)
		}
	}
	if got := strings.Join(order, ","); got != "5x-db4,2x-quiet,10x-long,1x-other" {
		t.Errorf("row order %s, want the knee series, the control, the hour, then the 4 vCPU step", got)
	}
	if knees := readText(t, filepath.Join(out, "KNEES.md")); !strings.Contains(knees, "knee series is the grid and refinement steps in factor order: 5x-db4.") {
		t.Errorf("the knee series is not 5x-db4 alone:\n%s", knees)
	}
}

// TestCurvesNameTheKindOfACopyWithoutAPhase: a copy from before cluster.sh recorded the phase
// names the series' 16 vCPU Postgres for every step, so it says nothing about where a step sits;
// the names do, as they always did. A quick 10x-hour is the hour, with TREND.md over it, and not
// a second factor-10 step of the knee series, and 1x-db4 is the 4 vCPU database row.
func TestCurvesNameTheKindOfACopyWithoutAPhase(t *testing.T) {
	root := t.TempDir()
	quickHour := goodSummary(10)
	quickHour.Config.DurationS = 300
	out := curves(t,
		withCopyMetaNoPhase(t, curveFixture(t, root, "10x-hour", quickHour, fixtureCgroup(2)), "16"),
		withCopyMetaNoPhase(t, curveFixture(t, root, "1x-db4", goodSummary(1), fixtureCgroup(2)), "16"),
		withCopyMetaNoPhase(t, curveFixture(t, root, "10x", goodSummary(10), fixtureCgroup(2)), "16"))
	order, rows := curvesCSV(t, out)
	if rows["10x-hour"]["kind"] != "hour" || rows["1x-db4"]["kind"] != "db4" || rows["10x"]["kind"] != "grid" {
		t.Errorf("kinds: 10x-hour %q, 1x-db4 %q, 10x %q; want hour, db4, grid",
			rows["10x-hour"]["kind"], rows["1x-db4"]["kind"], rows["10x"]["kind"])
	}
	if got := strings.Join(order, ","); got != "10x,10x-hour,1x-db4" {
		t.Errorf("row order %s, want the knee series, the hour, then the 4 vCPU step", got)
	}
	if knees := readText(t, filepath.Join(out, "KNEES.md")); !strings.Contains(knees, "knee series is the grid and refinement steps in factor order: 10x.") {
		t.Errorf("the knee series is not 10x alone:\n%s", knees)
	}
	if _, err := os.Stat(filepath.Join(out, "TREND.md")); err != nil {
		t.Errorf("the hour step left no TREND.md: %v", err)
	}
}

// TestCurvesFailARoleAtNinetyFivePercentOfItsLimit: Dispatch at 0.95 of its 16-core limit fails
// (d), and KNEES.md names the role, its cores and its limit; 0.85 of it passes.
func TestCurvesFailARoleAtNinetyFivePercentOfItsLimit(t *testing.T) {
	root := t.TempDir()
	out := curves(t,
		curveFixture(t, root, "1x", goodSummary(1), fixtureCgroup(0.85*16)),
		curveFixture(t, root, "2x", goodSummary(2), fixtureCgroup(0.95*16)))
	_, rows := curvesCSV(t, out)
	if rows["1x"]["pass_d_cpu_limit"] != "ok" || rows["2x"]["pass_d_cpu_limit"] != "FAIL" || rows["2x"]["dispatch_limit_share"] != "0.95" {
		t.Errorf("(d): 1x %q, 2x %q at share %q; want ok, FAIL at 0.95", rows["1x"]["pass_d_cpu_limit"], rows["2x"]["pass_d_cpu_limit"], rows["2x"]["dispatch_limit_share"])
	}
	if knees := readText(t, filepath.Join(out, "KNEES.md")); !strings.Contains(knees, "  - (d) cores mean at 2x: dispatch at 15.20 of its 16.00-core limit.") {
		t.Errorf("KNEES.md lacks the role at its limit:\n%s", knees)
	}
}

// TestCurvesRefineAWideKneeAtMostTwice: a 7-10 knee asks for 8.5; a knee at the 8.5 refinement
// step, bracket (7, 8.5), asks for 8; once two refinement steps lie in the 7-10 interval, a knee
// still 2.0 wide asks for nothing.
func TestCurvesRefineAWideKneeAtMostTwice(t *testing.T) {
	for _, c := range []struct {
		name  string
		pass  []float64
		fail  []float64
		wantR string
	}{
		{"grid knee", []float64{5, 7}, []float64{10}, "8.5\n"},
		{"one refinement inside", []float64{7}, []float64{8.5, 10}, "8\n"},
		{"two refinements inside", []float64{7}, []float64{9, 9.5, 10}, ""},
	} {
		root := t.TempDir()
		var dirs []string
		for _, f := range c.pass {
			dirs = append(dirs, curveFixture(t, root, fmtFloat(f)+"x", goodSummary(f), fixtureCgroup(2)))
		}
		for _, f := range c.fail {
			dirs = append(dirs, curveFixture(t, root, fmtFloat(f)+"x", collapsed(f), fixtureCgroup(2)))
		}
		out := curves(t, dirs...)
		if got := readText(t, filepath.Join(out, "refine.txt")); got != c.wantR {
			t.Errorf("%s: refine.txt = %q, want %q", c.name, got, c.wantR)
		}
	}
}

// TestCurvesJudgeTheReadsOnlyControlAndOrderTheRows: the reads-only control is n/a on (a) and (c);
// rows go in factor order, then the control, the hour and the 4 vCPU database; the Control section
// puts 10x beside 10x-reads and 1x beside 1x-db4.
func TestCurvesJudgeTheReadsOnlyControlAndOrderTheRows(t *testing.T) {
	root := t.TempDir()
	reads := goodSummary(10)
	reads.Config.EventsPerS, reads.Events = 0, eventsSummary{}
	reads.Requests.LatencyMs.P99 = 64
	hour := goodSummary(10)
	hour.Config.DurationS = 3600
	out := curves(t,
		curveFixture(t, root, "1x-db4", goodSummary(1), fixtureCgroup(2)),
		curveFixture(t, root, "10x-hour", hour, fixtureCgroup(2)),
		curveFixture(t, root, "10x-reads", reads, fixtureCgroup(2)),
		curveFixture(t, root, "10x", goodSummary(10), fixtureCgroup(2)),
		curveFixture(t, root, "1x", goodSummary(1), fixtureCgroup(2)))
	order, rows := curvesCSV(t, out)
	if got := strings.Join(order, ","); got != "1x,10x,10x-reads,10x-hour,1x-db4" {
		t.Errorf("row order %s", got)
	}
	r := rows["10x-reads"]
	if r["kind"] != "control" || r["factor"] != "10" || r["pass_a_commits"] != "n/a" || r["pass_c_delivery"] != "n/a" || r["passes"] != "true" {
		t.Errorf("10x-reads: kind %q factor %q (a) %q (c) %q passes %q; want control, 10, n/a, n/a, true",
			r["kind"], r["factor"], r["pass_a_commits"], r["pass_c_delivery"], r["passes"])
	}
	if rows["10x-hour"]["kind"] != "hour" || rows["1x-db4"]["kind"] != "db4" || rows["10x"]["kind"] != "grid" {
		t.Errorf("kinds: hour %q, db4 %q, 10x %q", rows["10x-hour"]["kind"], rows["1x-db4"]["kind"], rows["10x"]["kind"])
	}
	md := readText(t, filepath.Join(out, "CURVES.md"))
	for _, want := range []string{
		"### Requests with and without events at 10x",
		"| 10x-reads | 0.00 | 64 | 600.0 | yes |",
		"With events, request p99 is 25.0% above 10x-reads's.",
		"### 1x on the 4 vCPU database",
		"| 1x-db4 | 180 | 4.00 | 60.0 | 80 | 2.00 | 3.00 | 2.00 | 0 | yes |",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("CURVES.md lacks %q:\n%s", want, md)
		}
	}
	if knees := readText(t, filepath.Join(out, "KNEES.md")); !strings.Contains(knees, "knee series is the grid and refinement steps in factor order: 1x, 10x.") {
		t.Errorf("KNEES.md's series is not 1x, 10x:\n%s", knees)
	}
}

// linearPoints are minutes trendFrom to trendTo of v(hours).
func linearPoints(v func(h float64) float64) []trendPoint {
	var pts []trendPoint
	for m := trendFrom; m < trendTo; m++ {
		pts = append(pts, trendPoint{m, v(float64(m) / 60)})
	}
	return pts
}

// TestTrendRecoversALinearSlopeAndIgnoresFlatNoise: a series rising 0.6 per hour from 2 gives its
// slope within 1% and is upward; a flat series with noise is not; the t quantiles match the
// table's.
func TestTrendRecoversALinearSlopeAndIgnoresFlatNoise(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	fit, ok := fitTrend(linearPoints(func(h float64) float64 { return 2 + 0.6*h + 0.002*rng.NormFloat64() }))
	if !ok || math.Abs(fit.slope-0.6) > 0.006 || !fit.upward() || fit.n != 55 {
		t.Errorf("linear: %+v ok %v; want slope 0.6 within 1%%, upward, 55 minutes", fit, ok)
	}
	fit, ok = fitTrend(linearPoints(func(float64) float64 { return 2 + 0.1*rng.NormFloat64() }))
	if !ok || fit.upward() {
		t.Errorf("flat noisy: %+v ok %v; want a fit that is not upward", fit, ok)
	}
	for df, want := range map[float64]float64{1: 12.7062, 10: 2.2281, 53: 2.0057} {
		if got := studentT975(df); math.Abs(got-want) > 5e-4 {
			t.Errorf("t(0.975, %v) = %v, want %v", df, got, want)
		}
	}
	if _, ok := fitTrend(linearPoints(func(float64) float64 { return 1 })[:2]); ok {
		t.Errorf("two points gave a fit")
	}
}

// TestCurvesWriteTheHoursTrend: the hour step's cgroup minutes and samples.json heap rise, so all
// three TREND.md rows are upward over 55 minutes, less a null minute; a 10-minute grid step writes
// no TREND.md.
func TestCurvesWriteTheHoursTrend(t *testing.T) {
	root := t.TempDir()
	cg := fixtureCgroup(2)
	d := cg.Roles["dispatch"]
	for m := range 60 {
		h := float64(m) / 60
		d.Minutes = append(d.Minutes, &cgroupMinute{Cores: 2 + 1.2*h, WorkingSetBytesMean: float64(2<<30) + h*float64(1<<30)})
	}
	d.Minutes[20] = nil
	cg.Roles["dispatch"] = d
	hour := goodSummary(10)
	hour.Config.DurationS = 3600
	dir := curveFixture(t, root, "10x-hour", hour, cg)
	var ss []sample
	for s := range 3600 / 5 {
		at := curveWinStart.Add(time.Duration(s) * 5 * time.Second)
		smp := sample{At: at, InWindow: true}
		if s%3 == 0 {
			smp.HeapAt, smp.HeapInWindow = at, true
			smp.HeapLiveBytes = float64(300<<20) + at.Sub(curveWinStart).Hours()*float64(200<<20)
		}
		ss = append(ss, smp)
	}
	writeFixtureJSON(t, filepath.Join(dir, "samples.json"), ss)
	out := curves(t, dir)
	trend := readText(t, filepath.Join(out, "TREND.md"))
	for _, row := range []string{"| Dispatch cores | 54 |", "| Dispatch working set MiB | 54 |", "| " + trendHeapLive + " | 55 |"} {
		i := strings.Index(trend, row)
		if i < 0 {
			t.Errorf("TREND.md lacks %q:\n%s", row, trend)
			continue
		}
		line, _, _ := strings.Cut(trend[i:], "\n")
		if !strings.Contains(line, "| yes |") {
			t.Errorf("not upward: %s", line)
		}
	}
	if !strings.Contains(trend, "| Dispatch cores | 54 | 2.644 | 1.200 |") {
		t.Errorf("the cores row does not give slope 1.2 per hour:\n%s", trend)
	}

	grid := curves(t, curveFixture(t, t.TempDir(), "1x", goodSummary(1), fixtureCgroup(2)))
	if _, err := os.Stat(filepath.Join(grid, "TREND.md")); err == nil {
		t.Errorf("a run with no hour step wrote TREND.md")
	}
}

// olderRunStep rewrites dir's summary.json and samples.json as a run did before the sampler read
// the heap profile: resources.heap_inuse_mib (runtime.MemStats HeapInuse) and no heap_live_mib,
// and samples whose heap_inuse (with heap_alloc and heap_sys) each sample read itself, with no
// heap_at. ss gives each sample's time and its heap_inuse in MiB.
func olderRunStep(t *testing.T, dir string, heap stat, ss map[time.Time]float64) {
	t.Helper()
	var sum map[string]any
	if err := json.Unmarshal([]byte(readText(t, filepath.Join(dir, "summary.json"))), &sum); err != nil {
		t.Fatal(err)
	}
	res := sum["resources"].(map[string]any)
	delete(res, "heap_live_mib")
	res["heap_inuse_mib"] = heap
	writeFixtureJSON(t, filepath.Join(dir, "summary.json"), sum)
	var samples []map[string]any
	for at, mibs := range ss {
		samples = append(samples, map[string]any{"at": at, "in_window": true, "heap_inuse": mibs * (1 << 20), "heap_alloc": mibs * (1 << 19), "heap_sys": mibs * (1 << 21)})
	}
	writeFixtureJSON(t, filepath.Join(dir, "samples.json"), samples)
}

// TestHeapOfAnOlderRunRendersUnderItsOwnLabel: a step directory from before the sampler read the
// heap profile (MemStats HeapInuse) and one from after (the profile's in-use bytes) render side by
// side, each under its own label: their own curves.csv and CURVES.md columns, the Resources cell
// marked as MemStats with its note, and the hour's TREND.md series named for its metric.
func TestHeapOfAnOlderRunRendersUnderItsOwnLabel(t *testing.T) {
	root := t.TempDir()
	newDir := curveFixture(t, root, "1x", goodSummary(1), fixtureCgroup(2))
	oldDir := curveFixture(t, root, "2x", goodSummary(2), fixtureCgroup(2))
	olderRunStep(t, oldDir, stat{N: 9, P50: 1165.4, P90: 1381.2, P99: 1381.2, Max: 1464.6}, map[time.Time]float64{curveWinStart: 1165.4})

	out := curves(t, newDir, oldDir)
	_, rows := curvesCSV(t, out)
	for step, want := range map[string][4]string{
		"1x": {"300", "420", "", ""},
		"2x": {"", "", "1165.4", "1464.6"},
	} {
		for i, col := range []string{"heap_live_mib_p50", "heap_live_mib_max", "memstats_heap_inuse_mib_p50", "memstats_heap_inuse_mib_max"} {
			if got := rows[step][col]; got != want[i] {
				t.Errorf("curves.csv %s %s = %q, want %q", step, col, got, want[i])
			}
		}
	}
	memory := section(readText(t, filepath.Join(out, "CURVES.md")), "Memory")
	if !strings.Contains(memory, "| Go heap live p50 MiB (heap profile) | Go heap live max MiB (heap profile) | MemStats HeapInuse p50 MiB (older run) | MemStats HeapInuse max MiB (older run) |") {
		t.Errorf("CURVES.md's Memory table does not keep the two heap metrics in their own labelled columns:\n%s", memory)
	}
	if c := rowCells(memory, "1x"); len(c) != 8 || c[4] != "300" || c[5] != "420" || c[6] != "-" || c[7] != "-" {
		t.Errorf("CURVES.md's new-run row %q, want the heap live columns only", c)
	}
	if c := rowCells(memory, "2x"); len(c) != 8 || c[4] != "-" || c[5] != "-" || c[6] != "1,165" || c[7] != "1,465" {
		t.Errorf("CURVES.md's older-run row %q, want the MemStats columns only", c)
	}

	var steps []stepData
	for _, dir := range []string{newDir, oldDir} {
		st, err := readStep(dir)
		if err != nil {
			t.Fatal(err)
		}
		steps = append(steps, st)
	}
	md := renderTables(steps)
	resources := section(md, "Resources")
	const heap = 11 // after the working-set column
	if c := rowCells(resources, "1x"); c == nil || c[heap] != "300 / 420" {
		t.Errorf("TABLES.md's new-run heap cell %q, want the heap profile's 300 / 420 unmarked:\n%s", c, resources)
	}
	if c := rowCells(resources, "2x"); c == nil || c[heap] != "1,165 / 1,465 "+memStatsHeapMark {
		t.Errorf("TABLES.md's older-run heap cell %q, want MemStats HeapInuse marked %s:\n%s", c, memStatsHeapMark, resources)
	}
	if note := noteLine(resources, memStatsHeapMark+":"); !strings.Contains(note, "runtime.MemStats HeapInuse") || !strings.Contains(note, "`resources.heap_inuse_mib`") {
		t.Errorf("no note defines %s:\n%s", memStatsHeapMark, resources)
	}
	if strings.Contains(renderTables(steps[:1]), memStatsHeapMark) {
		t.Errorf("a new run alone carries the MemStats mark or its note")
	}
	checkRowWidths(t, md)

	// The hour: an older run's samples.json, with heap_inuse in the samples themselves.
	hour := goodSummary(10)
	hour.Config.DurationS = 3600
	hourDir := curveFixture(t, t.TempDir(), "10x-hour", hour, fixtureCgroup(2))
	ss := map[time.Time]float64{}
	for s := range 3600 / 15 {
		at := curveWinStart.Add(time.Duration(s) * 15 * time.Second)
		ss[at] = 300 + at.Sub(curveWinStart).Hours()*200
	}
	olderRunStep(t, hourDir, stat{N: 240, P50: 400, Max: 500}, ss)
	trend := readText(t, filepath.Join(curves(t, hourDir), "TREND.md"))
	if !strings.Contains(trend, "| "+trendHeapMemStats+" | 55 |") || strings.Contains(trend, "| "+trendHeapLive+" |") {
		t.Errorf("TREND.md does not name the older run's heap series %q over its 55 minutes:\n%s", trendHeapMemStats, trend)
	}
}

// TestCgroupMergeGivesEachMinute: a constant 2 cores gives each of the window's 10 minutes 2 cores
// and the working set; a 45 s hole in the samples nulls the minutes the gap reaches.
func TestCgroupMergeGivesEachMinute(t *testing.T) {
	cf, _ := mergeLines(t, defaultSeries("dispatch", 2).lines(nil, nil))
	mins := cf.Roles["dispatch"].Minutes
	if len(mins) != 10 {
		t.Fatalf("%d minutes, want 10", len(mins))
	}
	for i, m := range mins {
		if m == nil || math.Abs(m.Cores-2) > 1e-9 || m.WorkingSetBytesMean != 2<<30 {
			t.Errorf("minute %d: %+v, want 2 cores and 2 GiB", i, m)
		}
	}
	// The samples are at :02, :07, ... of each minute; the hole drops 12:03:02 to 12:03:42, so the
	// pair 12:02:57-12:03:47 is 50 s apart and reaches into minute 2.
	hole := func(at time.Time) bool {
		return at.After(cgWinStart.Add(180*time.Second)) && at.Before(cgWinStart.Add(225*time.Second))
	}
	cf, _ = mergeLines(t, defaultSeries("dispatch", 2).lines(hole, nil))
	mins = cf.Roles["dispatch"].Minutes
	for i, m := range mins {
		if (i == 2 || i == 3) != (m == nil) {
			t.Errorf("minute %d: %+v; want null exactly for minutes 2 and 3", i, m)
		}
	}
}
