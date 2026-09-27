package admit

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A root its architect closes before its first phase frees its admission slot in the same fact,
// and the next waiting root takes it: no human status write is needed to end the tree. Each slot
// change is one journal line naming the issue, the slots the project holds after it, and the cap.
func TestAClosedRootFreesItsSlotForTheNextWaitingRoot(t *testing.T) {
	pool := migratedPool(t)
	var logged bytes.Buffer
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(&logged, nil)))
	seedSlotted(t, pool, "LEGION-ACTIVE", "A")
	seedWaiting(t, pool, "LEGION-NEXT", "B")

	result, err := intake.ApplyFact(context.Background(), pool, "api", "close", intake.CloseRoot{Issue: "LEGION-ACTIVE", Reason: "no change"}, admission.engine, admission)
	if err != nil || result.Refusal != nil {
		t.Fatalf("close = %+v, %v", result.Refusal, err)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-NEXT", Index: 0, AdmittedAt: fixedNow}})
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
		if strings.Contains(line, `msg="admission: slot `) {
			got = append(got, line[strings.Index(line, "msg="):])
		}
	}
	want := []string{
		`msg="admission: slot released" issue=LEGION-ACTIVE slots=0 cap=1`,
		`msg="admission: slot taken" issue=LEGION-NEXT slots=1 cap=1`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("slot lines\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
