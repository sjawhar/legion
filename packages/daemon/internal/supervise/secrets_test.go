package supervise

import (
	"errors"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

func TestAFreshLaunchEnrollsAtRegistrationWithTheSession(t *testing.T) {
	h := newHarness(t)
	broker := h.withSecrets()
	h.launch()
	h.helloWithIdentity()
	h.wantState(StateShimConnected)
	if n := len(broker.enrolled()); n != 0 {
		t.Fatalf("%d enrollments before the agent registered; the session id is the wake address", n)
	}
	h.register()
	enrollments := broker.enrolled()
	if len(enrollments) != 1 {
		t.Fatalf("enrollments %+v, want one at registration", enrollments)
	}
	loc := h.locator()
	want := PodEnrollment{PodUID: loc.Incarnation, Thumbprint: testIdentity.Thumbprint, PodToken: testIdentity.PodToken, Session: session}
	if enrollments[0] != want {
		t.Fatalf("enrolled %+v, want %+v", enrollments[0], want)
	}
	stored := h.store.load(testToken)
	if stored.Enrollment == nil || stored.Enrollment.ID != "enr-1" || stored.Enrollment.Incarnation != loc.Incarnation {
		t.Fatalf("stored enrollment %+v", stored.Enrollment)
	}
	if got := h.conn.Enrollments(); len(got) != 1 || got[0] != "enr-1" {
		t.Fatalf("the shim was told %v, want [enr-1]", got)
	}
}

// In an issue pod six role processes share one pod uid, so the broker tells their enrollments
// apart by slot: the pod's real uid stays the identity the broker verifies against the projected
// token, and the slot is the daemon's own `<role>-g<generation>`, never the composed incarnation.
func TestAnIssuePodProcessEnrollsWithItsPodUIDAndRoleSlot(t *testing.T) {
	h := newHarness(t)
	broker := h.withSecrets()
	h.rt.ScriptSpawn(fake.SpawnResult{Locator: runtime.Locator{
		Runtime: runtime.RuntimeSandbox, Claim: testToken, Incarnation: runtime.SandboxIncarnation("pod-uid-9", 1),
		Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: "legion-legion-209", PodUID: "pod-uid-9", Container: "implementer", Generation: 1},
	}})
	h.launch()
	h.helloWithIdentity()
	h.register()
	enrollments := broker.enrolled()
	want := PodEnrollment{PodUID: "pod-uid-9", Slot: "implementer-g1", Thumbprint: testIdentity.Thumbprint, PodToken: testIdentity.PodToken, Session: session}
	if len(enrollments) != 1 || enrollments[0] != want {
		t.Fatalf("enrolled %+v, want %+v", enrollments, want)
	}
	if stored := h.store.load(testToken); stored.Enrollment == nil || stored.Enrollment.Incarnation != "pod-uid-9/1" {
		t.Fatalf("stored enrollment %+v, want it bound to the process incarnation", stored.Enrollment)
	}
}

func TestAResumedClaimEnrollsOnItsHello(t *testing.T) {
	h := newHarness(t)
	broker := h.withSecrets()
	h.launch()
	h.helloWithIdentity()
	h.register()
	h.ready()
	first := h.claim().Enrollment.ID
	h.must(RequestSuspend{Claim: h.token})
	h.wantState(StateSuspended)
	if got := broker.revocations(); len(got) != 1 || got[0] != first {
		t.Fatalf("the suspension revoked %v, want [%s]", got, first)
	}
	h.must(RequestResume{Claim: h.token})
	h.wantState(StateLaunching)
	h.helloWithIdentity() // the resumed pod's hello: the claim already records its session
	enrollments := broker.enrolled()
	if len(enrollments) != 2 || enrollments[1].Session != session || enrollments[1].PodUID != h.locator().Incarnation ||
		enrollments[1].PodUID == enrollments[0].PodUID {
		t.Fatalf("enrollments %+v, want the resumed pod enrolled on its hello with the recorded session and its own uid", enrollments)
	}
	if c := h.claim(); c.Enrollment == nil || c.Enrollment.ID == first || c.Enrollment.Incarnation != h.locator().Incarnation {
		t.Fatalf("the resumed claim holds %+v", c.Enrollment)
	}
	if got := broker.revocations(); len(got) != 1 {
		t.Fatalf("the resume revoked %v", got[1:])
	}
}

func TestEveryEndOfAProcessRevokesItsEnrollment(t *testing.T) {
	for name, end := range map[string]func(h *harness){
		"found gone":           func(h *harness) { h.observe(runtime.Gone) },
		"not the recorded pod": func(h *harness) { h.observe(runtime.NotRecordedProcess) },
		"suspended":            func(h *harness) { h.must(RequestSuspend{Claim: h.token}) },
		"stopped":              func(h *harness) { h.must(RequestStop{Claim: h.token}) },
		"the agent exited":     func(h *harness) { h.exit() },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			broker := h.withSecrets()
			h.launch()
			h.helloWithIdentity()
			h.register()
			h.ready()
			id := h.claim().Enrollment.ID
			end(h)
			if got := broker.revocations(); len(got) != 1 || got[0] != id {
				t.Fatalf("revocations %v, want [%s]", got, id)
			}
			if c := h.claim(); c.Enrollment != nil {
				t.Fatalf("the claim still records enrollment %+v", c.Enrollment)
			}
		})
	}
}

func TestTheRegistrationDeadlineRevokesAnUnregisteredPodsEnrollment(t *testing.T) {
	// A pod whose agent never registers is enrolled only if its claim already had a session (a
	// resume); a fresh launch holds the identity and enrolls nothing, so the deadline revokes
	// nothing and discards the identity with the process.
	h := newHarness(t)
	broker := h.withSecrets()
	h.launch()
	h.helloWithIdentity()
	h.advance(deadline)
	if got := broker.revocations(); len(got) != 0 {
		t.Fatalf("revocations %v for a pod that was never enrolled", got)
	}
	if n := len(broker.enrolled()); n != 0 {
		t.Fatalf("%d enrollments", n)
	}
	// The relaunched pod's hello carries its own identity; registration enrolls it, not the old one.
	h.helloWithIdentity()
	h.register()
	if e := broker.enrolled(); len(e) != 1 || e[0].PodUID != h.locator().Incarnation {
		t.Fatalf("enrolled %+v, want the relaunched pod", e)
	}
}

func TestAFailedRevocationIsRetriedThenLeftToTheLease(t *testing.T) {
	h := newHarness(t)
	broker := h.withSecrets()
	broker.revokeFails = 2
	h.launch()
	h.helloWithIdentity()
	h.register()
	id := h.claim().Enrollment.ID
	h.observe(runtime.Gone) // the first attempt runs on the machine's goroutine and fails
	if got := broker.revocations(); len(got) != 0 {
		t.Fatalf("revoked %v before any retry", got)
	}
	h.advance(revokeRetryDelay) // the second attempt, from the clock; fails
	h.advance(revokeRetryDelay) // the third succeeds
	if got := broker.revocations(); len(got) != 1 || got[0] != id {
		t.Fatalf("revocations %v after two failures, want [%s] on the third attempt", got, id)
	}
	h = newHarness(t)
	broker = h.withSecrets()
	broker.revokeFails = revokeAttempts
	h.launch()
	h.helloWithIdentity()
	h.register()
	h.observe(runtime.Gone)
	for i := 0; i < revokeAttempts; i++ {
		h.advance(revokeRetryDelay)
	}
	if lines := h.logs.lines("agent-secrets: enrollment not revoked", "the lease ends it"); len(lines) != 1 {
		t.Fatalf("log %q, want the give-up line once", h.logs.lines("agent-secrets"))
	}
}

func TestATransientEnrollmentFailureIsRetriedOnTheNextAliveObservation(t *testing.T) {
	h := newHarness(t)
	broker := h.withSecrets()
	broker.enrollErr = errors.New("broker unreachable")
	h.launch()
	h.helloWithIdentity()
	h.register()
	h.wantState(StateRegistered)
	if c := h.claim(); c.Enrollment != nil {
		t.Fatalf("enrolled %+v through a failure", c.Enrollment)
	}
	broker.enrollErr = nil
	h.observe(runtime.Alive)
	if c := h.claim(); c.Enrollment == nil {
		t.Fatal("the alive observation did not retry the enrollment")
	}
	if got := h.conn.Enrollments(); len(got) != 1 {
		t.Fatalf("the shim was told %v", got)
	}
}

func TestAPermanentEnrollmentRefusalWaitsForTheNextHello(t *testing.T) {
	h := newHarness(t)
	broker := h.withSecrets()
	broker.enrollErr = permanentErr{"broker answered 403 POD_IDENTITY_MISMATCH: the token names another pod"}
	h.launch()
	h.helloWithIdentity()
	h.register()
	broker.enrollErr = nil
	h.observe(runtime.Alive)
	if c := h.claim(); c.Enrollment != nil {
		t.Fatal("a permanent refusal was retried with the same identity")
	}
	if lines := h.logs.lines("agent-secrets: enrollment refused", "POD_IDENTITY_MISMATCH"); len(lines) != 1 {
		t.Fatalf("log %q", h.logs.lines("agent-secrets"))
	}
	h.helloWithIdentity() // the shim reconnected with a fresh token
	if c := h.claim(); c.Enrollment == nil {
		t.Fatal("the fresh hello did not enroll")
	}
}

func TestTheEnrollmentIsResentToAReconnectedShim(t *testing.T) {
	h := newHarness(t)
	h.withSecrets()
	h.launch()
	h.helloWithIdentity()
	h.register()
	h.ready()
	h.must(StreamClosed{Claim: h.token})
	h.observe(runtime.Alive)
	h.helloWithIdentity()
	if got := h.conn.Enrollments(); len(got) != 2 || got[0] != got[1] {
		t.Fatalf("the shim was told %v, want the same id twice", got)
	}
	// The claim was already enrolled for this incarnation when the reconnected shim's hello
	// arrived, so the fresh identity the hello carried must not keep a live pod token in memory.
	if h.m.identity == nil || h.m.identity.PodToken != "" {
		t.Fatalf("the held identity kept a live pod token after an already-enrolled hello: %+v", h.m.identity)
	}
}

func TestARestartKeepsTheEnrollmentAndTellsTheReconnectedShim(t *testing.T) {
	h := newHarness(t)
	broker := h.withSecrets()
	h.launch()
	h.helloWithIdentity()
	h.register()
	h.ready()
	id := h.claim().Enrollment.ID
	h.restart()
	h.helloWithIdentity()
	if e := broker.enrolled(); len(e) != 1 {
		t.Fatalf("the restarted daemon enrolled again: %+v", e)
	}
	if c := h.claim(); c.Enrollment == nil || c.Enrollment.ID != id {
		t.Fatalf("the restarted claim holds %+v, want %s", c.Enrollment, id)
	}
	if got := h.conn.Enrollments(); len(got) != 2 || got[1] != id {
		t.Fatalf("the shim was told %v", got)
	}
}

func TestWithoutABrokerNothingIsEnrolledOrRevoked(t *testing.T) {
	h := newHarness(t)
	h.launch()
	h.helloWithIdentity()
	h.register()
	h.ready()
	if c := h.claim(); c.Enrollment != nil {
		t.Fatalf("enrolled %+v with no broker", c.Enrollment)
	}
	h.observe(runtime.Gone)
	if got := h.conn.Enrollments(); len(got) != 0 {
		t.Fatalf("the shim was told %v", got)
	}
}
