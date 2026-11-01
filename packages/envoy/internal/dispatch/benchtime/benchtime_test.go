package benchtime

import (
	"strings"
	"testing"
	"time"
)

// samplePlan is an EXPLAIN (ANALYZE, BUFFERS) answer as Postgres writes one, with the two time
// lines this package reads indented as they are under a plan node's summary.
const samplePlan = `Index Scan using asks_issue_key on asks a  (cost=0.43..8.45 rows=1 width=16) (actual time=0.021..0.023 rows=2 loops=1)
  Index Cond: (issue_key = 'SEED1-1'::text)
  Buffers: shared hit=4
Planning Time: 0.184 ms
Execution Time: 1.250 ms`

func TestPlanTimeIsPlanningPlusExecution(t *testing.T) {
	took, err := planTime(samplePlan)
	if err != nil {
		t.Fatalf("read the plan's times: %v", err)
	}
	if want := 1434 * time.Microsecond; took != want {
		t.Fatalf("planning plus execution = %v, want %v", took, want)
	}
}

// A plan with no time at all is what EXPLAIN without ANALYZE answers, and one with only a
// planning time is what a cancelled run leaves. Either must fail the measurement: a missing time
// read as zero would be the fastest run of the eleven and would decide the median.
func TestPlanTimeRefusesAPlanMissingATime(t *testing.T) {
	for _, plan := range []struct{ name, text string }{
		{"no times", "Seq Scan on issues i  (cost=0.00..18.10 rows=810 width=36)"},
		{"planning alone", "Seq Scan on issues i\nPlanning Time: 0.100 ms"},
		{"execution alone", "Seq Scan on issues i\nExecution Time: 0.100 ms"},
	} {
		t.Run(plan.name, func(t *testing.T) {
			if took, err := planTime(plan.text); err == nil {
				t.Fatalf("read %v from a plan carrying %s", took, plan.name)
			}
		})
	}
}

// The median is a whole reading, so the plan a run prints beside a number is the run that gave
// that number rather than whichever run finished last.
func TestMedianIsTheMiddleReadingWithItsPlan(t *testing.T) {
	readings := []reading{
		{took: 5 * time.Millisecond, plan: "fifth"},
		{took: 1 * time.Millisecond, plan: "first"},
		{took: 3 * time.Millisecond, plan: "third"},
		{took: 2 * time.Millisecond, plan: "second"},
		{took: 4 * time.Millisecond, plan: "fourth"},
	}
	middle := median(readings)
	if middle.took != 3*time.Millisecond || middle.plan != "third" {
		t.Fatalf("the median reading is %v/%q, want 3ms/\"third\"", middle.took, middle.plan)
	}
	if readings[0].plan != "fifth" {
		t.Fatalf("median reordered its caller's readings: %q leads them", readings[0].plan)
	}
}

// The runs the median is taken over are eleven: runs 2 to 12, the first left out.
func TestElevenRunsDecideTheMedian(t *testing.T) {
	if counted := runs - discarded; counted != 11 {
		t.Fatalf("%d runs decide the median, want 11", counted)
	}
}

// Every statement-timing test is gated on this one variable, and no other variable turns a
// measurement on: a CI job that sets none runs none.
func TestTheGateIsTheBenchDatabaseURL(t *testing.T) {
	if !strings.HasPrefix(DatabaseURLEnv, "DISPATCH_BENCH_") {
		t.Fatalf("the gate is %q, which is not the bench database variable CI leaves unset", DatabaseURLEnv)
	}
}
