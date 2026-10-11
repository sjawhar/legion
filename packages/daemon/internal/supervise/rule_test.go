package supervise

import (
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// suspendedBy suspends h's idle claim with by asking.
func suspendedBy(h *harness, by SuspendSource) {
	h.t.Helper()
	h.reach(StateIdle)
	h.must(RequestSuspend{Claim: h.token, Reason: "the test suspends it", By: by})
	h.wantState(StateSuspended)
}

// Every suspension records who asked for it, and the store keeps it while the claim is suspended:
// the rule reads it after a restart as before one. A resume clears it.
func TestASuspensionRecordsItsSource(t *testing.T) {
	for _, by := range []SuspendSource{SuspendedByOperator, SuspendedByDaemon} {
		t.Run(string(by), func(t *testing.T) {
			h := newHarness(t)
			suspendedBy(h, by)
			if got := h.store.load(testToken).SuspendedBy; got != by {
				t.Fatalf("stored source %q, want %q", got, by)
			}
			h.restart()
			if got := h.claim().SuspendedBy; got != by {
				t.Fatalf("after a restart the claim records %q, want %q", got, by)
			}
			h.must(RequestResume{Claim: testToken})
			if got := h.store.load(testToken).SuspendedBy; got != "" {
				t.Fatalf("after the resume the store records %q, want nobody", got)
			}
		})
	}

	t.Run("exit", func(t *testing.T) {
		h := newHarnessOf(t, rootClaim())
		h.reach(StateIdle)
		h.exit()
		h.wantState(StateSuspended)
		if got := h.store.load(rootToken).SuspendedBy; got != SuspendedByExit {
			t.Fatalf("the root's exit stored %q, want exit", got)
		}
	})
}

// A suspension that names no source is a wiring error, refused before anything is stopped: the
// source is what decides whether the daemon may resume the claim.
func TestASuspensionNamingNoSourceIsRefused(t *testing.T) {
	for _, by := range []SuspendSource{"", SuspendedByExit, "someone"} {
		t.Run(string(by), func(t *testing.T) {
			h := newHarness(t)
			h.reach(StateIdle)
			if err := h.handle(RequestSuspend{Claim: testToken, By: by}); err == nil || !strings.Contains(err.Error(), "not operator or daemon") {
				t.Fatalf("suspend naming %q returned %v, want it refused", by, err)
			}
			h.wantState(StateIdle)
			h.wantCalls("Suspend", 0)
		})
	}
}

// The rule resumes every suspension but the operator's: the daemon's, a root's own exit, and one
// suspended before migration 0038 recorded its source (unknown, or none), as main resumed those on
// the same triggers. The operator's holds until the operator resumes the claim. (The boot's resume
// by rule, the one trigger main did not have, filters for daemon and exit itself: workflow's booted.)
func TestTheRuleResumesEverySuspensionButTheOperators(t *testing.T) {
	for _, tc := range []struct {
		name    string
		suspend func(h *harness)
		resumed bool
	}{
		{name: "the daemon's", suspend: func(h *harness) { suspendedBy(h, SuspendedByDaemon) }, resumed: true},
		{name: "unknown, before migration 0038", suspend: func(h *harness) {
			suspendedBy(h, SuspendedByDaemon)
			c := h.claim()
			c.SuspendedBy = SuspendedByUnknown
			if err := h.store.PutClaim(h.ctx, c); err != nil {
				h.t.Fatal(err)
			}
			h.restart()
		}, resumed: true},
		{name: "an empty source", suspend: func(h *harness) {
			suspendedBy(h, SuspendedByDaemon)
			c := h.claim()
			c.SuspendedBy = ""
			if err := h.store.PutClaim(h.ctx, c); err != nil {
				h.t.Fatal(err)
			}
			h.restart()
		}, resumed: true},
		{name: "the operator's", suspend: func(h *harness) { suspendedBy(h, SuspendedByOperator) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tc.suspend(h)
			h.must(RequestRuleResume{Claim: testToken, Reason: "it owns its issue's phase"})
			if tc.resumed {
				h.wantState(StateLaunching)
				if resume := h.wantCalls("Resume", 1)[0]; resume.Spec.ResumeSessionFile != sessionFile {
					t.Errorf("resumed %+v, want the claim's own session", resume.Spec)
				}
				if lines := h.logs.lines(`msg="supervise: resuming by rule"`, `rule="it owns its issue's phase"`); len(lines) != 1 {
					t.Errorf("resume lines %q, want one naming the rule", lines)
				}
				return
			}
			h.wantState(StateSuspended)
			h.wantCalls("Resume", 0)
			if lines := h.logs.lines(`msg="supervise: not resumed by rule: its suspension holds until the operator resumes it"`); len(lines) != 1 {
				t.Errorf("hold lines %q, want one saying the suspension holds", lines)
			}
		})
	}
}

// The rule resumes nothing that is not suspended: a claim already running is left to run, and a
// failed claim, whose launch budget is spent, is relaunched only by the workflow's retry. A claim
// the rule resumes whose launch keeps failing therefore fails at its budget and stays failed.
func TestTheRuleResumesNothingButASuspendedClaim(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestRuleResume{Claim: testToken, Reason: "it owns its phase"})
	h.wantState(StateReady)

	suspendedBy(h, SuspendedByDaemon)
	refused := errors.New("the pod was never scheduled")
	for range testLimits().LaunchFailures + 2 {
		h.rt.ScriptResume(fake.SpawnResult{Err: refused})
	}
	if err := h.handle(RequestRuleResume{Claim: testToken, Reason: "it owns its phase"}); !errors.Is(err, refused) {
		t.Fatalf("the rule's resume returned %v, want the runtime's refusal", err)
	}
	h.wantState(StateFailed)
	h.wantCalls("Resume", testLimits().LaunchFailures)
	for range 3 {
		h.must(RequestRuleResume{Claim: testToken, Reason: "it owns its phase"})
	}
	h.wantState(StateFailed)
	h.wantCalls("Resume", testLimits().LaunchFailures)
}

// An operator's suspend of a claim the daemon suspended takes the suspension over, so the rule
// resumes it no more; the daemon's suspend of a claim the operator suspended leaves it the
// operator's.
func TestTheOperatorsSuspensionTakesASuspendedClaimOver(t *testing.T) {
	h := newHarness(t)
	suspendedBy(h, SuspendedByDaemon)
	h.must(RequestSuspend{Claim: testToken, Reason: "the operator suspended it", By: SuspendedByOperator})
	if got := h.store.load(testToken).SuspendedBy; got != SuspendedByOperator {
		t.Fatalf("after the operator's suspend the store records %q, want the operator", got)
	}
	h.must(RequestSuspend{Claim: testToken, Reason: "the tree of LEGION-208 lingers", By: SuspendedByDaemon})
	if got := h.claim().SuspendedBy; got != SuspendedByOperator {
		t.Fatalf("after the daemon's suspend the claim records %q, want the operator still", got)
	}
	h.must(RequestRuleResume{Claim: testToken, Reason: "it owns its phase"})
	h.wantState(StateSuspended)
	h.wantCalls("Suspend", 1)
}

// The workflow's task does not resume a claim an operator suspended: it is not queued, and the
// start that carries it waits (ErrHeldByOperator). The operator's own delivery resumes the claim,
// as the operator's resume does.
func TestTheWorkflowsTaskWaitsOutTheOperatorsSuspension(t *testing.T) {
	h := newHarness(t)
	suspendedBy(h, SuspendedByOperator)
	pendingBefore := h.claim().Pending
	if err := h.handle(RequestDeliver{Claim: testToken, Task: "Test it.", ID: "outbox:7", Phase: "testing", Generation: 1}); !errors.Is(err, ErrHeldByOperator) {
		t.Fatalf("the workflow's task returned %v, want ErrHeldByOperator", err)
	}
	h.wantState(StateSuspended)
	h.wantCalls("Resume", 0)
	if got := h.claim().Pending; (got == nil) != (pendingBefore == nil) || got != nil && got.ID == "outbox:7" {
		t.Fatalf("pending %+v, want the workflow's task not queued", got)
	}

	h.must(RequestDeliver{Claim: testToken, Task: "look at the tree"})
	h.wantState(StateLaunching)
	h.wantCalls("Resume", 1)
	if got := h.claim().SuspendedBy; got != "" {
		t.Fatalf("the resumed claim records %q as who suspended it, want nobody", got)
	}
}
