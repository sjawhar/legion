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
	"unicode/utf8"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// staleLockRefusal is what the shim answers an adoption with when jj is wedged on a git lock
// another role's container left: the shim's own words, which name the lock and its age.
const staleLockRefusal = "Command failed (exit 1): jj metaedit: the git lock " +
	"/legion/workspaces/acme/widgets/legion-480/.git/index.lock was last written 31m ago"

// staleLockRefusalWithNUL is that same answer with a NUL byte in it, which is what reaches the
// daemon when the far side's JSON holds a `\u0000`: Oh My Pi decodes it into the `error` string,
// and the shim relays the wedged command's own bytes whatever they are. No Postgres encoding
// stores a NUL ("invalid byte sequence for encoding \"UTF8\": 0x00"), so a daemon that recorded
// it raw could not write the record of the failed send at all.
const staleLockRefusalWithNUL = "Command failed (exit 1): jj metaedit: the git lock " +
	"/legion/workspaces/acme/widgets/legion-480/.git/index.lock\x00 was last written 31m ago"

// staleLockRefusalWithRawByte is that answer carrying a byte that begins no UTF-8 sequence, as a
// command whose output is not text at all gives it.
const staleLockRefusalWithRawByte = "Command failed (exit 1): jj metaedit: the git lock " +
	"/legion/workspaces/acme/widgets/legion-480/.git/index.lock\xff was last written 31m ago"

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
//
// Each row names its own failure in the log, the uncharged ones included: an operator reading a
// claim that waits is told whether the prompt was lost on the way or the GitHub App would mint
// no identity for it, which are looked for in different places.
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
			logged: `msg="supervise: the role's GitHub App identity could not be minted; the prompt was never sent and the delivery waits"`,
			text:   errBoom.Error()},
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

	// The charge is what bounds the re-sends, and the record of the failed send beside it is only
	// a diagnosis, so a record the store will not take decides nothing about the charge. The write
	// of that record used to return before the split that charges, and a refused adoption whose
	// text the store refused was then charged nothing and re-sent with no bound — the very stall
	// the split exists to end (INFRA-1480). The store refuses the write itself here, rather than
	// the text being one a UTF8 server rejects, so the claim holds for every reason a write can
	// fail and not for one encoding's quirk.
	t.Run("a refused adoption is charged though its record is not written", func(t *testing.T) {
		h := adoptingHarness(t)
		h.rt.FailAdoptWorkingCopy(adoptRefusal(staleLockRefusalWithNUL))

		// The first send writes its record: this same call queues the task, and a store refusing
		// every delivery write would refuse the queue's own write and leave no task to re-send.
		h.must(RequestDeliver{Claim: testToken, Task: "the task"})
		h.wantBudgets(Budgets{PromptFailures: 1})

		h.store.fail("PutDelivery", errBoom)
		h.observe(runtime.Alive) // the sweep's re-send, whose record the store refuses

		h.wantBudgets(Budgets{PromptFailures: 2})
		if p := h.pending(); p.SendFailures != 2 {
			t.Fatalf("pending %+v, want the second failed send counted in memory", p)
		}
		if lines := h.logs.lines("level=WARN",
			`msg="supervise: the failed send was not recorded on the delivery; settle writes it again and the charge goes on"`,
			errBoom.Error()); len(lines) != 1 {
			t.Fatalf("logged %v, want the unwritten record named once", lines)
		}
	})

	// The far side's text never reaches the store as it arrived (scrubSendError). It is the
	// wedged command's own output, relayed whole, and the store takes only some of what a command
	// can write: the rule is valid UTF-8, with no NUL, and the column holds a diagnosis rather
	// than a log. What the scrub cuts still goes to the log line beside it, which has no store to
	// refuse it, so nothing is lost and only moved.
	t.Run("the text a failed send records is scrubbed and bounded", func(t *testing.T) {
		// About a megabyte of relayed stderr, as a jj wedged on a lock writes it, ending in a
		// line the log must still carry and the record must not.
		hugeRefusal := "Command failed (exit 1): jj metaedit: " +
			strings.Repeat("the git lock is held by another container; ", 24000) +
			"and this is the last line of the megabyte"
		for _, tc := range []struct {
			name string
			// reason is what the shim's `ok: false` answer carries.
			reason string
			// want is the whole text the delivery must hold once the scrub has run.
			want string
			// logged is a fragment of the far side's text the log line must still carry.
			logged string
		}{
			{name: "a NUL byte, which no Postgres encoding stores",
				reason: staleLockRefusalWithNUL,
				want:   adoptRefusal(strings.ReplaceAll(staleLockRefusalWithNUL, "\x00", "")).Error(),
				logged: "index.lock"},
			// Not the row above again, and not to be deleted as a duplicate of it: a NUL is valid
			// UTF-8 and a lone 0xff begins no UTF-8 sequence at all, so the two are refused by
			// different servers. An SQL_ASCII server takes the 0xff, a UTF8 server — production's
			// encoding — refuses it ("invalid byte sequence for encoding "UTF8": 0xff"), and every
			// encoding refuses the NUL. The rule the scrub holds is valid UTF-8, and this is the
			// row that reads it.
			{name: "a byte that begins no UTF-8 sequence",
				reason: staleLockRefusalWithRawByte,
				want:   adoptRefusal(strings.ReplaceAll(staleLockRefusalWithRawByte, "\xff", "\uFFFD")).Error(),
				logged: "index.lock"},
			{name: "about a megabyte of the wedged command's stderr",
				reason: hugeRefusal,
				want:   string([]rune(adoptRefusal(hugeRefusal).Error())[:maxSendErrorRunes]) + "…",
				logged: "and this is the last line of the megabyte"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				h := adoptingHarness(t)
				h.rt.FailAdoptWorkingCopy(adoptRefusal(tc.reason))

				h.must(RequestDeliver{Claim: testToken, Task: "the task"})

				got := h.pending().LastSendError
				if got != tc.want {
					t.Fatalf("recorded %.300q, want %.300q", got, tc.want)
				}
				if !utf8.ValidString(got) || strings.ContainsRune(got, 0) || len([]rune(got)) > maxSendErrorRunes+1 {
					t.Fatalf("recorded %.300q of %d runes, want valid UTF-8, no NUL, and at most %d runes",
						got, len([]rune(got)), maxSendErrorRunes)
				}
				if stored, ok := h.store.delivery(testToken); !ok || stored.LastSendError != got {
					t.Fatalf("stored delivery's text = %.300q (held %t), want the scrubbed text persisted with it",
						stored.LastSendError, ok)
				}
				if lines := h.logs.lines("level=WARN",
					`msg="supervise: the shim refused to adopt the working copy; the prompt was never sent"`,
					tc.logged); len(lines) != 1 {
					t.Fatalf("logged %d warnings carrying %q, want the one refusal with the far side's whole text",
						len(lines), tc.logged)
				}
			})
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
