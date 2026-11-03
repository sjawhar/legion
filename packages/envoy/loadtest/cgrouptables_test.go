package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cgroupStep is a step directory as a cluster step leaves it: summary.json (with resources but no
// /proc figures, as under -allow-foreign), meta.json, and cgroup.json merged from series.
func cgroupStep(t *testing.T, name string, sum summary, series ...[]string) string {
	t.Helper()
	var lines []string
	for _, s := range series {
		lines = append(lines, s...)
	}
	dir, samples := cgStepDir(t, lines)
	sum.Config.Step = name
	b, err := json.Marshal(sum)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "summary.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runCgroupMerge([]string{dir, samples}); err != nil {
		t.Fatal(err)
	}
	return dir
}

// emptyResources is the resources of a foreign run: the sampler ran, but read no /proc.
func emptyResources() *resourcesSummary {
	return &resourcesSummary{CPUCoresMean: map[string]float64{}, CPUCoresPeak5s: map[string]float64{}, RSSMiBMax: map[string]float64{}}
}

// TestTablesTakeTheResourcesCellsFromCgroupJSON: with cgroup.json, the Resources row takes
// Dispatch's cores and peak, Postgres, listener, the sum of the NATS pods and the runner from it, the
// Dispatch working set max, and the CPU-s per operation from Dispatch's cpu_s; an unbracketed role
// is "-" with its mark, and a step marked failed by cgroup.json is "-" in every such cell.
func TestTablesTakeTheResourcesCellsFromCgroupJSON(t *testing.T) {
	late := defaultSeries("runner", 1)
	late.from = cgWinStart.Add(4 * time.Second)
	good := cgroupStep(t, "c-1x", summary{Resources: emptyResources(), Events: eventsSummary{Committed: 100}, Requests: requestsSummary{Sent: 1100}},
		defaultSeries("dispatch", 2).lines(nil, nil), defaultSeries("postgres", 1.5).lines(nil, nil), defaultSeries("listener", 0.25).lines(nil, nil),
		defaultSeries("nats-0", 0.1).lines(nil, nil), defaultSeries("nats-1", 0.2).lines(nil, nil), defaultSeries("nats-2", 0.3).lines(nil, nil),
		late.lines(nil, nil))
	gap := func(at time.Time) bool {
		return at.After(cgWinStart.Add(100*time.Second)) && at.Before(cgWinStart.Add(200*time.Second))
	}
	failed := cgroupStep(t, "c-gap", summary{Resources: emptyResources(), Events: eventsSummary{Committed: 100}, Requests: requestsSummary{Sent: 1100}},
		defaultSeries("dispatch", 2).lines(gap, nil), defaultSeries("postgres", 1.5).lines(nil, nil))
	unread := cgroupStep(t, "c-unread", summary{Resources: emptyResources(), Events: eventsSummary{Committed: 100, CommitsError: "read loadtest.event_commits: failed"}, Requests: requestsSummary{Sent: 1100}},
		defaultSeries("dispatch", 2).lines(nil, nil))

	var steps []stepData
	for _, dir := range []string{good, failed, unread} {
		st, err := readStep(dir)
		if err != nil {
			t.Fatal(err)
		}
		steps = append(steps, st)
	}
	md := renderTables(steps)
	text := section(md, "Resources")
	if !strings.Contains(text, "| Dispatch RSS max MiB | "+cgroupWorkingSetH+" | heap live MiB p50 / max |") {
		t.Fatalf("the Resources header lacks the working-set column after the RSS one:\n%s", text)
	}
	// The columns: Dispatch cores, Postgres, listener, NATS, loadgen, proxy, ..., working set (10),
	// ..., CPU-s per operation (16).
	const dispatch, postgres, listener, nats, loadgen, workingSet, perOp = 1, 2, 3, 4, 5, 10, 16
	c := rowCells(text, "c-1x")
	if c == nil {
		t.Fatalf("no c-1x row:\n%s", text)
	}
	for col, want := range map[int]string{dispatch: "2.00 / 2.00", postgres: "1.50", listener: "0.250", nats: "0.600", loadgen: "-", workingSet: "2,048", perOp: "1.0000"} {
		if c[col] != want {
			t.Errorf("c-1x column %d reads %q, want %q:\n%s", col, c[col], want, text)
		}
	}
	if c[0] != "c-1x (no cgroup cores: runner)" {
		t.Errorf("c-1x is labelled %q, want the unmeasured runner marked", c[0])
	}

	f := rowCells(text, "c-gap")
	if f == nil || !strings.Contains(f[0], cgroupFailedMark) || !strings.Contains(f[0], "(failed: sampler_gap)") {
		t.Fatalf("the step cgroup.json failed is not marked: %q\n%s", f, text)
	}
	for _, col := range []int{dispatch, postgres, listener, nats, loadgen, workingSet, perOp} {
		if f[col] != "-" {
			t.Errorf("c-gap column %d reads %q, want -", col, f[col])
		}
	}
	if u := rowCells(text, "c-unread"); u == nil || u[perOp] != "-" || u[dispatch] != "2.00 / 2.00" {
		t.Errorf("the step whose commit rows were not read: %q; want its cores and - per operation", u)
	}
	for _, mark := range []string{cgroupFailedMark, cgroupUnmeasuredMark, "(failed: <kinds>)"} {
		if noteLine(md, mark) == "" {
			t.Errorf("no note defines %s:\n%s", mark, md)
		}
	}
	checkRowWidths(t, md)
}

// TestTablesWithoutCgroupJSONAreUnchanged: a step directory without cgroup.json renders no
// working-set column and no cgroup mark or note.
func TestTablesWithoutCgroupJSONAreUnchanged(t *testing.T) {
	dir := t.TempDir()
	b, _ := json.Marshal(summary{Config: summaryConfig{Step: "local"}, Resources: &resourcesSummary{CPUCoresMean: map[string]float64{"dispatch": 1.5}, CPUCoresPeak5s: map[string]float64{"dispatch": 1.9}}})
	if err := os.WriteFile(filepath.Join(dir, "summary.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := readStep(dir)
	if err != nil {
		t.Fatal(err)
	}
	md := renderTables([]stepData{st})
	if st.cgroup != nil || st.failed || strings.Contains(md, cgroupWorkingSetH) || strings.Contains(md, "cgroup") || strings.Contains(md, "(failed") {
		t.Fatalf("a step without cgroup.json or step-failed.json renders cgroup or failed text:\n%s", md)
	}
	if c := rowCells(section(md, "Resources"), "local"); len(c) != 16 || c[1] != "1.50 / 1.90" {
		t.Errorf("the Resources row %q, want 16 cells with Dispatch 1.50 / 1.90", c)
	}
}

// TestTablesMarkAStepWithStepFailedJSONInEveryTable: the kinds of step-failed.json's reasons mark
// the step's label in every table, each kind once.
func TestTablesMarkAStepWithStepFailedJSONInEveryTable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "killed")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(summary{Config: summaryConfig{Step: "killed"}, Requests: requestsSummary{Routes: map[string]routeSummary{"GET /x": {Count: 1}}}})
	if err := os.WriteFile(filepath.Join(dir, "summary.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	reasons := `{"reasons":[{"kind":"restart","container":"envoy-dispatch"},{"kind":"sampler_gap","role":"postgres"},{"kind":"restart","container":"nats"}]}`
	if err := os.WriteFile(filepath.Join(dir, "step-failed.json"), []byte(reasons), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := readStep(dir)
	if err != nil {
		t.Fatal(err)
	}
	md := renderTables([]stepData{st})
	const label = "killed (failed: restart, sampler_gap)"
	for _, title := range []string{"Events, delivery and requests", "Write latency by kind", "Resources", "Commit-order lock", "Delivery from sending, and live socket frames", "Ungranted locks", "Transaction ids"} {
		if c := rowCells(section(md, title), "killed"); c == nil || c[0] != label {
			t.Errorf("section %q: the row is %q, want the label %q", title, c, label)
		}
	}
	if !strings.Contains(section(md, "Requests by route"), "| route | "+label+" |") {
		t.Errorf("the route table's column is not marked:\n%s", section(md, "Requests by route"))
	}
	if note := noteLine(section(md, "Events, delivery and requests"), "(failed: <kinds>)"); !strings.Contains(note, "step-failed.json") {
		t.Errorf("no note defines the failed mark:\n%s", md)
	}
	checkRowWidths(t, md)
}
