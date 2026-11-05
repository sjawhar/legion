package supervise

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// subArchitectClaim is an architect of a child issue of the tree: an architect whose claim a stop
// may end, since only the tree's root ends with its tree.
func subArchitectClaim() Claim {
	c := rootClaim()
	c.Token, c.Issue, c.Role = "legion-LEGION-209-architect", "LEGION-209", claim.RoleArchitect
	return c
}

// `legion claims stop` retires a claim, and until dispatch://INFRA-956 R3 the operator's retry
// refused that state, so a stopped claim could be started again only by editing the claims store
// with the daemon stopped. The retry now relaunches it on the session it recorded, with fresh
// budgets, for a phase worker and for an architect alike.
func TestTheOperatorsRetryRelaunchesAStoppedClaim(t *testing.T) {
	for _, tc := range []struct {
		name  string
		claim Claim
	}{
		{"a phase worker", queuedClaim()},
		{"an architect", subArchitectClaim()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarnessOf(t, tc.claim)
			h.reach(StateIdle)
			resumes := len(h.calls("Resume"))

			h.must(RequestStop{Claim: h.token})
			h.wantState(StateRetired)

			h.must(RequestOperatorRetry{Claim: h.token})

			h.wantState(StateLaunching)
			h.wantBudgets(Budgets{})
			calls := h.wantCalls("Resume", resumes+1)
			if got := calls[len(calls)-1].Spec.ResumeSessionFile; got != sessionFile {
				t.Fatalf("the retry of the stopped claim resumed session file %q, want the claim's %q", got, sessionFile)
			}
			h.relaunched()
			h.wantState(StateReady)
		})
	}
}

// --fresh-session is the one thing that changes which session the operator's retry starts: with
// the flag the claim forgets the recorded session before the relaunch, so the runtime spawns a
// new agent — what a claim whose provider refused its session needs, since resuming it would be
// refused again — and without it the recorded session is resumed.
func TestTheOperatorsRetryStartsANewSessionOnlyWhenItAsksForOne(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fresh bool
	}{
		{"a retry that asks for nothing", false},
		{"--fresh-session", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarnessOf(t, rootClaim())
			h.reach(StateReady)
			for range 3 {
				h.observe(runtime.Gone)
			}
			h.wantState(StateFailed)
			spawns, resumes := len(h.calls("Spawn")), len(h.calls("Resume"))

			h.must(RequestOperatorRetry{Claim: h.token, FreshSession: tc.fresh})

			h.wantState(StateLaunching)
			if !tc.fresh {
				h.wantCalls("Spawn", spawns)
				calls := h.wantCalls("Resume", resumes+1)
				if got := calls[len(calls)-1].Spec.ResumeSessionFile; got != sessionFile {
					t.Fatalf("the retry resumed session file %q, want the claim's %q", got, sessionFile)
				}
				if c := h.claim(); c.Session != session || c.SessionFile != sessionFile {
					t.Fatalf("the retried claim records session %q (%q), want the recorded %q (%q)", c.Session, c.SessionFile, session, sessionFile)
				}
				return
			}
			h.wantCalls("Resume", resumes)
			calls := h.wantCalls("Spawn", spawns+1)
			if got := calls[len(calls)-1].Spec.ResumeSessionFile; got != "" {
				t.Fatalf("the fresh-session retry spawned with resume session file %q, want none", got)
			}
			if c := h.claim(); c.Session != "" || c.SessionFile != "" {
				t.Fatalf("the fresh-session retry left session %q (%q) on the claim, want both forgotten", c.Session, c.SessionFile)
			}
		})
	}
}
