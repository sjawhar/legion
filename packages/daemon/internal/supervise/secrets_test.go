package supervise

import (
	"errors"
	"fmt"
	"testing"
	"time"

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

// TestARevocationTheBrokerRefusesIsNotRetried: a daemon restarted under a new machine login cannot
// end an enrollment its earlier login made (the broker answers 403 NOT_ENROLLING_CREDENTIAL, a
// refusal no retry changes), so the machine makes one attempt, retries nothing, and logs that the
// enrollment's lease ends it once the pod stops renewing.
func TestARevocationTheBrokerRefusesIsNotRetried(t *testing.T) {
	h := newHarness(t)
	broker := h.withSecrets()
	broker.revokeErr = permanentErr{"broker answered 403 NOT_ENROLLING_CREDENTIAL: only the machine login that enrolled a pod may end its enrollment"}
	h.launch()
	h.helloWithIdentity()
	h.register()
	h.observe(runtime.Gone)
	for range revokeAttempts {
		h.advance(revokeRetryDelay)
	}
	if broker.revokeCalls() != 1 {
		t.Fatalf("%d revocation attempts for a refusal no retry changes, want 1", broker.revokeCalls())
	}
	if lines := h.logs.lines("agent-secrets: enrollment revocation refused", "NOT_ENROLLING_CREDENTIAL", "its lease ends it"); len(lines) != 1 {
		t.Fatalf("log %q, want the refusal line once", h.logs.lines("agent-secrets"))
	}
	if lines := h.logs.lines("agent-secrets: enrollment not revoked after every attempt"); len(lines) != 0 {
		t.Fatalf("log %q: a refusal is not a run of failed attempts", lines)
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

// A projected token is valid for an hour at most, and the shim reads it once per hello. An
// enrollment held up past the hello token's expiry (the daemon's machine login waiting for a
// person to approve it) presents a token the shim reads again then, never the expired one, which
// the broker refuses as POD_TOKEN_EXPIRED.
func TestAnEnrollmentHeldPastTheHelloTokensExpiryPresentsAFreshToken(t *testing.T) {
	h := newHarness(t)
	broker := h.withSecrets()
	broker.enrollErr = errors.New("broker answered 0 NO_MACHINE_CREDENTIAL: machine login pending; code ABCD-1234")
	h.launch()
	h.helloWithToken(podToken(h.clock.Now().Add(time.Hour)))
	h.register()
	h.ready()
	if c := h.claim(); c.Enrollment != nil {
		t.Fatalf("enrolled %+v while the login was pending", c.Enrollment)
	}

	h.clock.Advance(80 * time.Minute) // the login is approved long after the hello
	broker.enrollErr = nil
	fresh := podToken(h.clock.Now().Add(time.Hour))
	h.conn.SetAgentSecretsToken(fresh)
	h.observe(runtime.Alive)

	if refused := h.logs.lines("agent-secrets: enrollment refused"); len(refused) != 0 {
		t.Fatalf("the enrollment was refused: %q", refused)
	}
	enrollments := broker.enrolled()
	if len(enrollments) != 1 || enrollments[0].PodToken != fresh {
		t.Fatalf("enrolled %+v, want one enrollment presenting the token the shim read after the hello's expired", enrollments)
	}
	if c := h.claim(); c.Enrollment == nil || c.Enrollment.ID != "enr-1" {
		t.Fatalf("stored enrollment %+v", c.Enrollment)
	}
	if h.m.identity.PodToken != "" {
		t.Fatal("the machine kept the pod token past the enrollment")
	}
}

// The machine asks the shim for its token again only when the token it holds could expire before
// the broker verifies it: within the Enroll call's RPC bound plus a minute of clock skew. A token
// that outlives that is presented as the hello carried it.
func TestAHeldTokenIsReadAgainOnlyWhenItCouldExpireBeforeTheBrokerVerifiesIt(t *testing.T) {
	for name, tc := range map[string]struct {
		left  time.Duration // the hello token's life left when the agent registers
		reads int
	}{
		"expired":                    {-time.Minute, 1},
		"expiring within the margin": {testRPC + 30*time.Second, 1},
		"outliving the margin":       {testRPC + 2*time.Minute, 0},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			broker := h.withSecrets()
			fresh := podToken(h.clock.Now().Add(time.Hour))
			h.conn.SetAgentSecretsToken(fresh)
			hello := podToken(h.clock.Now().Add(tc.left))
			h.launch()
			h.helloWithToken(hello)
			h.register()
			want := hello
			if tc.reads > 0 {
				want = fresh
			}
			if enrollments := broker.enrolled(); len(enrollments) != 1 || enrollments[0].PodToken != want {
				t.Fatalf("enrolled %+v, want one enrollment presenting %s", enrollments, want)
			}
			if reads := h.conn.TokenReads(); reads != tc.reads {
				t.Fatalf("the shim was asked for its token %d times, want %d", reads, tc.reads)
			}
		})
	}
}

// A shim built before the token request forwards it to OMP, which refuses it as an unknown
// command. The machine presents the broker nothing it would refuse: it drops the identity and
// waits for the shim's next hello, whose token is read then, as it does after a refusal.
func TestAShimThatCannotReadItsTokenAgainWaitsForItsNextHello(t *testing.T) {
	h := newHarness(t)
	broker := h.withSecrets()
	h.conn.FailAgentSecretsToken(fmt.Errorf("worker-stream: %s refused agent-secrets-token: Unknown command: agent-secrets-token: %w", h.token, runtime.ErrTokenRefused))
	h.launch()
	h.helloWithToken(podToken(h.clock.Now().Add(-time.Minute)))
	h.register()
	h.ready()
	h.observe(runtime.Alive)
	if c := h.claim(); c.Enrollment != nil || len(broker.enrolled()) != 0 {
		t.Fatalf("enrolled %+v with no token to present", c.Enrollment)
	}
	if reads := h.conn.TokenReads(); reads != 1 {
		t.Fatalf("the shim was asked for its token %d times, want once: its refusal stands until its next hello", reads)
	}
	if lines := h.logs.lines("agent-secrets: the shim did not read its projected token again", "Unknown command"); len(lines) != 1 {
		t.Fatalf("log %q", h.logs.lines("agent-secrets"))
	}
	if refused := h.logs.lines("agent-secrets: enrollment refused"); len(refused) != 0 {
		t.Fatalf("the expired token reached the broker: %q", refused)
	}
	h.helloWithToken(podToken(h.clock.Now().Add(time.Hour))) // the shim reconnected
	if c := h.claim(); c.Enrollment == nil {
		t.Fatal("the fresh hello did not enroll")
	}
}

func TestATokenRequestTheTransportLosesIsRetriedOnTheNextAliveObservation(t *testing.T) {
	h := newHarness(t)
	broker := h.withSecrets()
	h.conn.FailAgentSecretsToken(errors.New("worker-stream: agent-secrets-token: no answer on the worker stream within the RPC timeout"))
	h.launch()
	h.helloWithToken(podToken(h.clock.Now().Add(-time.Minute)))
	h.register()
	h.ready()
	if c := h.claim(); c.Enrollment != nil {
		t.Fatalf("enrolled %+v with no token to present", c.Enrollment)
	}
	h.conn.FailAgentSecretsToken(nil)
	fresh := podToken(h.clock.Now().Add(time.Hour))
	h.conn.SetAgentSecretsToken(fresh)
	h.observe(runtime.Alive)
	if enrollments := broker.enrolled(); len(enrollments) != 1 || enrollments[0].PodToken != fresh {
		t.Fatalf("enrolled %+v, want the alive observation to ask again and present what the shim read", enrollments)
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

// expiredTokenErr is the broker refusing the presented pod token as expired (POD_TOKEN_EXPIRED).
type expiredTokenErr struct{ msg string }

func (e expiredTokenErr) Error() string      { return e.msg }
func (e expiredTokenErr) TokenExpired() bool { return true }

// A broker whose clock runs further ahead of the daemon's than podTokenSkew refuses as expired a
// token the daemon still holds good. The key stands: the machine drops the token, keeps the
// identity, and at its next observation asks the shim for the pod's current token and enrolls the
// same key with it, rather than waiting for a hello.
func TestAnExpiredTokenRefusalReadsTheTokenAgainAtTheNextObservation(t *testing.T) {
	h := newHarness(t)
	broker := h.withSecrets()
	broker.enrollErr = expiredTokenErr{"broker answered 403 POD_TOKEN_EXPIRED: the projected token has expired (oidc: token expired at 2026-10-11T04:54:28Z)"}
	h.launch()
	h.helloWithIdentity() // a token the daemon's clock holds good for an hour
	h.register()
	h.ready()
	if lines := h.logs.lines("agent-secrets: the broker found the pod token expired", "POD_TOKEN_EXPIRED"); len(lines) != 1 {
		t.Fatalf("log %q", h.logs.lines("agent-secrets"))
	}
	if refused := h.logs.lines("agent-secrets: enrollment refused"); len(refused) != 0 {
		t.Fatalf("an expired token was read as a refusal of the key: %q", refused)
	}
	if h.m.identity == nil || h.m.identity.Thumbprint != testIdentity.Thumbprint || h.m.identity.PodToken != "" {
		t.Fatalf("held identity %+v, want the key kept and the expired token dropped", h.m.identity)
	}
	if reads := h.conn.TokenReads(); reads != 0 {
		t.Fatalf("the shim was asked for its token %d times before the refusal, want none: the daemon held it good", reads)
	}

	broker.enrollErr = nil
	fresh := podToken(h.clock.Now().Add(time.Hour + time.Minute))
	h.conn.SetAgentSecretsToken(fresh)
	h.observe(runtime.Alive)
	if reads := h.conn.TokenReads(); reads != 1 {
		t.Fatalf("the shim was asked for its token %d times at the next observation, want once", reads)
	}
	enrollments := broker.enrolled()
	if len(enrollments) != 1 || enrollments[0].PodToken != fresh || enrollments[0].Thumbprint != testIdentity.Thumbprint {
		t.Fatalf("enrolled %+v, want the same key enrolled once, with the token the shim read", enrollments)
	}
	if c := h.claim(); c.Enrollment == nil {
		t.Fatal("the claim records no enrollment")
	}
}
