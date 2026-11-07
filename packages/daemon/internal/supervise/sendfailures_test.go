package supervise

// A send that never reaches a turn leaves two marks: the claim's prompt budget, which only the
// far side's own answer about this workspace spends, and the pending task's record of every
// failed send, which is kept whether the failure was charged or not. These tests pin both
// (INFRA-1480).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// staleLockRefusal is what the shim answers an adoption with when jj is wedged on a git lock
// another role's container left: the shim's own words, which name the lock and its age.
const staleLockRefusal = "Command failed (exit 1): jj metaedit: the git lock " +
	"/legion/workspaces/acme/widgets/legion-480/.git/index.lock was last written 31m ago"

// adoptingHarness is a machine whose role has an App identity to adopt its working copy with, at
// ready with nothing pending: without an Identity the daemon adopts nothing at all (Machine.adopt)
// and no adoption could fail.
func adoptingHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.deps.Identity = func(context.Context, claim.Role, string) (runtime.GitIdentity, error) {
		return runtime.GitIdentity{Name: "legion-implementer[bot]", Email: "7+legion-implementer[bot]@users.noreply.github.com"}, nil
	}
	h.restart()
	h.reach(StateReady)
	return h
}

// The whole split a failed send is charged on, in one table: the prompt budget pays for the two
// answers the far side gave about this very workspace — a prompt the agent refused and an
// adoption the shim refused — and for nothing else. A frame the transport lost, either frame, and
// an App identity the daemon could not mint before any frame left it, say nothing about the
// workspace or the agent, cost nothing, and are retried for as long as they keep failing. Charging
// them would retire a healthy worker for its cluster's weather; charging neither of the first two
// leaves a claim re-sending for ever, which is what held two planning tasks 31 and 33 hours
// (INFRA-1480).
func TestDeliveryBudgets(t *testing.T) {
	for _, tc := range []struct {
		name string
		// fail arranges the one failure the send meets.
		fail func(h *harness)
		// logged is the message of the log line the failure writes, which carries the failure's
		// own text beside it; the charge writes a line of its own, so the message is matched
		// whole rather than by a fragment both lines hold.
		logged  string
		text    string
		budgets Budgets
	}{
		{name: "the agent refuses the prompt",
			fail:    func(h *harness) { h.conn.RefusePrompt("the model provider refused the request") },
			logged:  `msg="supervise: the agent refused the prompt"`,
			text:    "the model provider refused the request",
			budgets: Budgets{PromptFailures: 1}},
		{name: "the transport loses the prompt",
			fail:   func(h *harness) { h.conn.FailPrompt(errBoom) },
			logged: `msg="supervise: the prompt was lost to the transport; the delivery waits"`, text: errBoom.Error()},
		{name: "the shim refuses the adoption",
			fail:    func(h *harness) { h.rt.FailAdoptWorkingCopy(adoptRefusal(staleLockRefusal)) },
			logged:  `msg="supervise: the shim refused to adopt the working copy; the prompt was never sent"`,
			text:    staleLockRefusal,
			budgets: Budgets{PromptFailures: 1}},
		{name: "the transport loses the adoption",
			fail:   func(h *harness) { h.rt.FailAdoptWorkingCopy(errBoom) },
			logged: `msg="supervise: the prompt was lost to the transport; the delivery waits"`, text: errBoom.Error()},
		{name: "the role's App identity cannot be minted",
			fail: func(h *harness) {
				h.deps.Identity = func(context.Context, claim.Role, string) (runtime.GitIdentity, error) {
					return runtime.GitIdentity{}, errBoom
				}
				h.restart()
				h.reach(StateReady)
			},
			logged: `msg="supervise: the prompt was lost to the transport; the delivery waits"`, text: errBoom.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := adoptingHarness(t)
			tc.fail(h)

			h.must(RequestDeliver{Claim: testToken, Task: "the task"})

			h.wantBudgets(tc.budgets)
			if p := h.pending(); p.Task != "the task" || !p.ConfirmedAt.IsZero() || !p.DeliveredAt.IsZero() {
				t.Fatalf("pending %+v, want the task waiting unread for the next send", p)
			}
			if lines := h.logs.lines("level=WARN", tc.logged, tc.text); len(lines) != 1 {
				t.Fatalf("logged %v, want one warning naming %q and carrying %q", lines, tc.logged, tc.text)
			}
		})
	}

	// Refused adoptions spend that budget to its end rather than being re-sent for ever: with the
	// shipped limits (prompt_failure_limit 3, prompt_retire_limit 2) three charges retire the
	// process and relaunch the session, and the sixth consecutive charge fails the claim, which
	// `legion state` and the stranded report then name. A failed claim adopts nothing more.
	t.Run("consecutive refused adoptions end in a failed claim", func(t *testing.T) {
		h := adoptingHarness(t)
		h.rt.FailAdoptWorkingCopy(adoptRefusal(staleLockRefusal))

		h.must(RequestDeliver{Claim: testToken, Task: "the task"}) // the first charge
		h.observe(runtime.Alive)                                   // the second
		h.wantBudgets(Budgets{PromptFailures: 2})
		h.observe(runtime.Alive) // the third: the retirement

		h.wantState(StateLaunching)
		h.wantBudgets(Budgets{PromptRetires: 1})
		h.wantCalls("Suspend", 1)
		h.relaunched()           // the fourth charge, on the relaunched session's ready
		h.observe(runtime.Alive) // the fifth
		h.observe(runtime.Alive) // the sixth: the retirement that has none left

		h.wantState(StateFailed)
		h.wantBudgets(Budgets{PromptFailures: 3, PromptRetires: 2})
		// One adoption per send and no seventh: the claim failed where it would otherwise have
		// asked again, and no prompt ever reached the agent.
		h.wantCalls("AdoptWorkingCopy", 6)
		h.wantPrompts(0)
		if lines := h.logs.lines("level=WARN",
			`msg="supervise: the shim refused to adopt the working copy; the prompt was never sent"`,
			staleLockRefusal); len(lines) != 6 {
			t.Fatalf("logged %d adoption refusals, want one per send naming the shim's own words:\n%s",
				len(lines), strings.Join(lines, "\n"))
		}
		if p := h.pending(); p.Task != "the task" {
			t.Fatalf("pending %+v, want the undelivered task kept on the failed claim", p)
		}
	})
}

// Every send that fails is recorded on the pending task itself — how many have failed, when the
// last of them failed by the machine's clock, and the error it failed with — and the record is
// kept for a failure the budget charged and for one it did not. The uncharged failures are the
// ones it exists for: a transport loss costs nothing and is retried for ever, so without this the
// only trace of a claim waiting days on a send that never lands is a log line inside its pod. A
// send that reaches a turn clears all three, so what an operator reads on a pending task is the
// run of failures standing in the way of the send it is still waiting for.
func TestDeliveryRecordsItsFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		// fail arranges the failure, and succeed takes it away so the next send lands.
		fail    func(h *harness)
		succeed func(h *harness)
		text    string
		charged Budgets
	}{
		{name: "an adoption the shim refuses, which is charged",
			fail:    func(h *harness) { h.rt.FailAdoptWorkingCopy(adoptRefusal(staleLockRefusal)) },
			succeed: func(h *harness) { h.rt.FailAdoptWorkingCopy(nil) },
			text:    staleLockRefusal,
			charged: Budgets{PromptFailures: 2}},
		{name: "a prompt the transport loses, which is not",
			fail:    func(h *harness) { h.conn.FailPrompt(errBoom) },
			succeed: func(h *harness) { h.conn.FailPrompt(nil) },
			text:    errBoom.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := adoptingHarness(t)
			tc.fail(h)

			h.must(RequestDeliver{Claim: testToken, Task: "the task"})

			first := h.clock.Now()
			p := h.pending()
			if p.SendFailures != 1 || !p.LastSendFailureAt.Equal(first) || !strings.Contains(p.LastSendError, tc.text) {
				t.Fatalf("pending after one failed send = %+v, want one failure at %v carrying %q", p, first, tc.text)
			}
			if !p.ConfirmedAt.IsZero() || !p.DeliveredAt.IsZero() {
				t.Fatalf("pending %+v, want the task left queued by the failed send", p)
			}
			// The record rides out on the write the taken-back send already makes, so a restart
			// reads it rather than starting the count again.
			if stored, ok := h.store.delivery(testToken); !ok || stored.SendFailures != 1 || !stored.LastSendFailureAt.Equal(first) {
				t.Fatalf("stored delivery = %+v (held %t), want the failure persisted with it", stored, ok)
			}

			h.advance(time.Second)
			second := h.clock.Now()
			h.observe(runtime.Alive)

			if p := h.pending(); p.SendFailures != 2 || !p.LastSendFailureAt.Equal(second) {
				t.Fatalf("pending after two failed sends = %+v, want two failures, the last at %v", p, second)
			}
			h.wantBudgets(tc.charged)

			tc.succeed(h)
			h.observe(runtime.Alive)
			h.must(StreamTurnStart{Claim: testToken})

			p = h.pending()
			if p.ConfirmedAt.IsZero() {
				t.Fatalf("pending %+v, want the turn that started to have confirmed it", p)
			}
			if p.SendFailures != 0 || !p.LastSendFailureAt.IsZero() || p.LastSendError != "" {
				t.Fatalf("pending after the send that reached a turn = %+v, want its failure record cleared", p)
			}
			if stored, ok := h.store.delivery(testToken); !ok || stored.SendFailures != 0 ||
				!stored.LastSendFailureAt.IsZero() || stored.LastSendError != "" {
				t.Fatalf("stored delivery = %+v (held %t), want the cleared record persisted", stored, ok)
			}
		})
	}
}
