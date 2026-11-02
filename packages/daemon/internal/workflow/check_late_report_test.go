package workflow

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The implementer's verdict on the check's current look counts however that look ended: its bound
// came due while the implementer was still driving the change, an operator's hold ended it, or its
// claim failed and was restarted in it. The look is over either way; what the implementer saw in
// production is not, and refusing it threw the only report of that deploy away and left the check
// to close as not run (dispatch://INFRA-714/spec R12).
func TestALateReportOfTheCurrentLookIsRecorded(t *testing.T) {
	for _, tc := range []struct {
		name    string
		verdict string
		outcome record.CheckOutcome
		open    bool
	}{
		{name: "a pass after the look ended closes the check as recorded", verdict: "pass", outcome: record.CheckRecorded},
		{name: "a fail after the look ended closes it as a defect", verdict: "fail", outcome: record.CheckDefect},
		// A late pending is recorded as the other two are: the look it reports is over, and the
		// check waits for its next wake, open.
		{name: "a pending after the look ended leaves the check open", verdict: "pending", open: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := childCheckRig(t, "")
			// The look ends with nobody reporting it, as its bound does (intake.CheckLookEnded).
			check := productionCheck(t, rig.pool, "LEGION-209")
			if check == nil || !check.LookOpen {
				t.Fatalf("the check before its look ended = %+v, want look 1 open", check)
			}
			rig.apply("bound", intake.CheckLookEnded{Issue: "LEGION-209", OpenedAt: check.OpenedAt, Look: 1, Cause: "look bound"})
			if ended := productionCheck(t, rig.pool, "LEGION-209"); ended == nil || ended.LookOpen || !ended.Open() {
				t.Fatalf("the check after its look's bound = %+v, want the look ended and the check open", ended)
			}

			if got := refusalCode(rig.apply("late", intake.CheckReport{Issue: "LEGION-209", Look: 1, Verdict: tc.verdict, Summary: "drove it"})); got != "" {
				t.Fatalf("the late %s report = %q, want it recorded", tc.verdict, got)
			}

			after := productionCheck(t, rig.pool, "LEGION-209")
			if after == nil {
				t.Fatal("the check is gone")
			}
			if tc.open {
				if !after.Open() || after.Outcome != "" {
					t.Fatalf("the check after the late pending = %+v, want it still open", after)
				}
				return
			}
			if after.Open() || after.Outcome != tc.outcome {
				t.Fatalf("the check after the late %s = %+v, want it closed as %s", tc.verdict, after, tc.outcome)
			}
		})
	}
}
