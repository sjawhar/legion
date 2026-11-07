package supervise

import (
	"context"
	"log/slog"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// The two turn messages as an operator and a production check read them: whole, and not as the
// constants that carry them. A reword would move the constant and leave a test that read the
// constant green, while every search that counts a turn's start against its end went quiet.
const (
	startedLine = "supervise: a turn started"
	endedLine   = "supervise: a turn ended"
)

// wantTurnLine is the one record of line, read from the records the handler kept rather than from
// the text the handler printed: what this holds is the attributes' own values, and one of them —
// the claim — is an attribute of the machine's logger rather than of the call site. delivery is
// the turn's task where it has one; empty means the line must carry no delivery key at all,
// which is how a turn that is no task's is told from one whose task is unnamed.
func wantTurnLine(t *testing.T, h *harness, line string, generation uint64, delivery string) {
	t.Helper()
	kept := h.records.of(line)
	if len(kept) != 1 {
		t.Fatalf("%q was written %d times, want once", line, len(kept))
	}
	r := kept[0]
	if r.level != slog.LevelInfo {
		t.Fatalf("%q is %s, want INFO", line, r.level)
	}
	if claim, ok := r.attr("claim"); !ok || claim.String() != string(h.token) {
		t.Fatalf("%q carries claim %v (present %v), want %s", line, claim, ok, h.token)
	}
	if got, ok := r.attr("generation"); !ok || got.Uint64() != generation {
		t.Fatalf("%q carries generation %v (present %v), want %d", line, got, ok, generation)
	}
	got, ok := r.attr("delivery")
	switch {
	case delivery == "" && ok:
		t.Fatalf("%q carries delivery %v; a turn that is no task's names none", line, got)
	case delivery != "" && !ok:
		t.Fatalf("%q carries no delivery, want %s", line, delivery)
	case delivery != "" && got.String() != delivery:
		t.Fatalf("%q carries delivery %v, want %s", line, got, delivery)
	}
}

// A turn's start and its end are the pair an operator counts against each other to read what the
// daemon did between them, so each of the three paths that put a claim in working writes the
// start, under the one message, and the turn's end writes the close. The line goes before the
// write that records the turn, never after it: the decision these lines exist for is a turn start
// whose claim write failed, and a line written after the write would leave exactly that decision
// silent.
func TestTheTurnLogLinesAreWrittenOnEveryPathIntoATurn(t *testing.T) {
	// The first path: an agent_start this daemon read in a ready claim, which is the task's own
	// turn, so the line names the delivery. Its end closes the pair under the same task.
	t.Run("a turn of the task's own in a ready claim", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		h.must(RequestDeliver{Claim: testToken, Task: "the task"})
		delivery := h.pending().ID

		h.must(StreamTurnStart{Claim: testToken})

		h.wantState(StateWorking)
		wantTurnLine(t, h, startedLine, h.generation(), delivery)

		h.must(StreamTurnEnd{Claim: testToken})

		wantTurnLine(t, h, endedLine, h.generation(), delivery)
	})

	// A turn the daemon prompted nothing for — an Envoy notice, a person's steer — is no task's,
	// and the line names none rather than naming an empty one.
	t.Run("a turn that is no task's names no delivery", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)

		h.must(StreamTurnStart{Claim: testToken})

		h.wantState(StateWorking)
		wantTurnLine(t, h, startedLine, h.generation(), "")
	})

	// The second path: a restored claim asks the agent whether it is in a turn, and the agent
	// answers that it is. That turn is the delivery an earlier daemon sent, so the claim moves to
	// working with no agent_start of this daemon's own ever read, and the line is the only record
	// of a turn that started before this process existed.
	t.Run("a restored claim whose agent answers that it is streaming", func(t *testing.T) {
		restored := fixture(StateReady)
		h := newHarnessOf(t, restored)
		if err := h.store.PutDelivery(h.ctx, testToken, *restored.Pending); err != nil {
			t.Fatalf("seed the restored delivery: %v", err)
		}
		h.conns.Register(testToken, h.conn)
		h.conn.SetStreaming(true)
		h.start(restored)

		h.must(RequestReady{Claim: testToken, Generation: restored.Generation, Session: session})

		h.wantState(StateWorking)
		wantTurnLine(t, h, startedLine, restored.Generation, restored.Pending.ID)
	})

	// The third path: the restored claim's task is dropped for its phase, and the same ask finds
	// the agent in a turn of its own. Nothing is pending, so the turn is no task's and the line
	// names no delivery; without it the claim's move to working would be the one such move the
	// log never recorded.
	t.Run("a claim whose task was dropped and whose agent is in a turn of its own", func(t *testing.T) {
		restored := fixture(StateReady)
		restored.Pending.Phase = phase.Implementing
		h := newHarnessOf(t, restored)
		if err := h.store.PutDelivery(h.ctx, testToken, *restored.Pending); err != nil {
			t.Fatalf("seed the restored delivery: %v", err)
		}
		h.conns.Register(testToken, h.conn)
		h.conn.SetStreaming(true)
		h.deps.PhaseHolds = func(context.Context, string, phase.Phase, uint64) (bool, error) { return false, nil }
		h.start(restored)

		h.must(RequestReady{Claim: testToken, Generation: restored.Generation, Session: session})

		h.wantState(StateWorking)
		if p := h.claim().Pending; p != nil {
			t.Fatalf("pending = %+v, want the finished phase's task dropped", p)
		}
		wantTurnLine(t, h, startedLine, restored.Generation, "")
	})

	// A turn start whose claim write the database did not take writes its line all the same, with
	// the owed line beside it: the pair is what a reader of the log follows from the failure to
	// the repair, and the start line is exactly what the measured incident was missing. When the
	// write lands, the repair's line closes that story.
	t.Run("a start whose claim write failed, and the lines that follow it", func(t *testing.T) {
		h, _ := owedTurnStart(t)

		wantTurnLine(t, h, startedLine, h.generation(), "")
		owed := h.records.of("supervise: a claim write the database did not take")
		if len(owed) != 1 {
			t.Fatalf("the owed line was written %d times, want once", len(owed))
		}
		if owed[0].level != slog.LevelWarn {
			t.Fatalf("the owed line is %s, want WARN", owed[0].level)
		}
		if store, ok := owed[0].attr("store"); !ok || store.String() != "PutClaim" {
			t.Fatalf("the owed line carries store %v (present %v), want PutClaim", store, ok)
		}
		if _, ok := owed[0].attr("error"); !ok {
			t.Fatal("the owed line carries no error; the reason the write was refused is the whole of what it adds")
		}

		h.store.fail("PutClaim", nil)
		h.observe(runtime.Alive)

		repaired := h.records.of("supervise: a claim write the database did not take was made")
		if len(repaired) != 1 {
			t.Fatalf("the repaired line was written %d times, want once", len(repaired))
		}
		if repaired[0].level != slog.LevelInfo {
			t.Fatalf("the repaired line is %s, want INFO", repaired[0].level)
		}
		if store, ok := repaired[0].attr("store"); !ok || store.String() != "PutClaim" {
			t.Fatalf("the repaired line carries store %v (present %v), want PutClaim", store, ok)
		}
	})
}
