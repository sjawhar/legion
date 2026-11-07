package intake

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
)

// decidingHandler answers every fact with refusal, nil for none, and counts the facts it was
// given: the workflow as intake sees it.
type decidingHandler struct {
	refusal *Refusal
	applied int
}

func (h *decidingHandler) Apply(context.Context, pgx.Tx, Fact) (Result, error) {
	h.applied++
	return Result{Refusal: h.refusal}, nil
}

// A production check report is one fact id — the issue, the check, the look and the verdict — so a
// refusal recorded under it would answer the implementer's next send of the same report as one
// already received, and the report of the deploy it drove would never be recorded. A refused
// report leaves its event unmarked instead, and applies when it is sent again
// (dispatch://INFRA-714/spec R12). Every other refusal stands: an unmarked one would make a
// refused handoff apply on its next delivery.
func TestARefusedCheckReportAppliesWhenItIsSentAgain(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	const reportID = "check:LEGION-209:2026-10-30T00:00:00Z:1:pass"
	report := CheckReport{Issue: "LEGION-209", Claim: "legion-legion-legion-209-implementer", Look: 1, Verdict: "pass", Summary: "driven in production"}
	refusing := &decidingHandler{refusal: &Refusal{Status: http.StatusConflict, Code: "CHECK_LOOK_NOT_OPEN", Message: "the look has ended"}}

	for attempt := 1; attempt <= 2; attempt++ {
		result, err := ApplyFact(ctx, pool, "api", reportID, report, refusing)
		if err != nil {
			t.Fatalf("apply the report, attempt %d: %v", attempt, err)
		}
		if result.Duplicate || result.Refusal == nil {
			t.Fatalf("the report's attempt %d = %+v, want the handler's refusal and no duplicate", attempt, result)
		}
		if refusing.applied != attempt {
			t.Fatalf("the handler was given %d facts after attempt %d, want %d: a refused report is unmarked", refusing.applied, attempt, attempt)
		}
	}

	taking := &decidingHandler{}
	result, err := ApplyFact(ctx, pool, "api", reportID, report, taking)
	if err != nil || result.Duplicate || result.Refusal != nil {
		t.Fatalf("the report once the workflow takes it = %+v, %v; want it applied", result, err)
	}
	again, err := ApplyFact(ctx, pool, "api", reportID, report, taking)
	if err != nil || !again.Duplicate {
		t.Fatalf("the report sent once more = %+v, %v; want it dropped as a duplicate of the one recorded", again, err)
	}
	if taking.applied != 1 {
		t.Fatalf("the handler was given %d facts, want the one it took", taking.applied)
	}
}

// A refusal of any other fact is recorded under its id, so the fact is not applied again: the
// handoff refusals exist to stop a completion of an ended run from being taken on its redelivery.
func TestARefusedFactThatIsNoCheckReportStaysRecorded(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	refusing := &decidingHandler{refusal: &Refusal{Status: http.StatusConflict, Code: "HANDOFF_STALE_GENERATION", Message: "the run it reports is over"}}
	handoff := HandoffComplete{Issue: "LEGION-209", Role: "implementer", Generation: 3}

	if result, err := ApplyFact(ctx, pool, "api", "handoff:LEGION-209:3", handoff, refusing); err != nil || result.Refusal == nil {
		t.Fatalf("the completion = %+v, %v; want the handler's refusal", result, err)
	}
	result, err := ApplyFact(ctx, pool, "api", "handoff:LEGION-209:3", handoff, refusing)
	if err != nil || !result.Duplicate {
		t.Fatalf("the completion sent again = %+v, %v; want it dropped as a duplicate", result, err)
	}
	if refusing.applied != 1 {
		t.Fatalf("the handler was given %d facts, want the one it refused", refusing.applied)
	}
}
