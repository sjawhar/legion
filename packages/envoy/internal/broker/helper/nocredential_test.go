// packages/envoy/internal/broker/helper/nocredential_test.go
//go:build linux

package helper

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/proof"
)

// TestNoCredentialReasonNamesWhyTheHelperHoldsNone: every state login-status can report with no
// credential held has its own reason, which the helper's journal and `machine login-status` both
// print: a login in flight outranks a dropped credential, a dropped credential names the cause the
// helper recorded, and a helper from before credential_dropped, which sets login_refused only for
// a broker refusal, reads as that refusal.
func TestNoCredentialReasonNamesWhyTheHelperHoldsNone(t *testing.T) {
	const refusal = "the broker refused the launcher credential (expired or revoked, or a proof it could not verify, such as clock skew or an AGENT_SECRETS_URL mismatch)"
	for _, tc := range []struct {
		name, state string
		refused     bool
		dropped     string
		want        string
	}{
		{"never logged in since the start", "", false, "", "no machine login since the helper started, and none kept on this machine"},
		{"a login waiting for approval", "pending", false, "", "a machine login is waiting for a human to approve it"},
		{"a login waiting for approval beside a dropped credential", "pending", true, dropExpired, "a machine login is waiting for a human to approve it"},
		{"a denied login", "denied", false, "", "the most recent machine login was denied"},
		{"a login nobody approved in time", "expired", false, "", "the most recent machine login expired before anyone approved it"},
		{"dropped at its expiry", "expired", true, dropExpired, "the launcher credential reached its expiry"},
		{"dropped when the broker refused its refresh", "expired", true, dropRefreshRefused, dropRefreshRefused},
		{"dropped when the broker refused it", "expired", true, dropRefused, refusal},
		{"refused, from a helper before credential_dropped", "expired", true, "", refusal},
		{"a state from a newer helper", "revoked", false, "", "the most recent machine login is revoked"},
	} {
		if got := NoCredentialReason(tc.state, tc.refused, tc.dropped); got != tc.want {
			t.Errorf("%s: NoCredentialReason(%q, %v, %q) = %q; want %q", tc.name, tc.state, tc.refused, tc.dropped, got, tc.want)
		}
	}
}

// waitForRecord waits up to 5 s for a JSON log record in out whose msg is msg, and returns it.
func waitForRecord(t *testing.T, out *syncBuffer, msg string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		records := logRecords(t, out)
		if i := slices.IndexFunc(records, func(rec map[string]any) bool { return rec["msg"] == msg }); i >= 0 {
			return records[i]
		}
		if time.Now().After(deadline) {
			t.Fatalf("no line %q within 5 s; log:\n%s", msg, out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestARestartWithNoLoginSaysWhyTheSessionCannotEnroll: a restarted helper with no machine login
// kept on disk holds no launcher credential, and a session it re-pins must revoke its prior
// enrollment before it enrolls again. That revoke cannot run, and the helper says so at ERROR,
// naming the session, the prior enrollment and why.
func TestARestartWithNoLoginSaysWhyTheSessionCannotEnroll(t *testing.T) {
	state := filepath.Join(t.TempDir(), "sessions.json")
	pid := recordPrior(t, state, "enr-prior")
	var out syncBuffer
	r := newLoggedRig(t, state, slog.New(slog.NewJSONHandler(&out, nil))) // Recover re-pins the child; no login
	sess := r.srv.Registry.Get(pid)
	if sess == nil {
		t.Fatal("the live process must be re-pinned")
	}
	line := waitForRecord(t, &out, "session cannot enroll: revoking its lapsed enrollment first needs a launcher credential, and the helper holds none; run: agent-secrets machine login, and have a human approve it")
	for k, v := range map[string]any{
		"level": "ERROR", "runtime_id": sess.RuntimeID(), "enrollment_id": "enr-prior",
		"why": "no machine login since the helper started, and none kept on this machine",
	} {
		if line[k] != v {
			t.Fatalf("the line for the re-pinned session: %s = %v; want %v (line %v)", k, line[k], v, line)
		}
	}
}

// TestASessionKeyTheHelperCannotEnrollIsAnError: an enroll-session-key the helper cannot attempt
// for want of a launcher credential fails, as before, and logs the same ERROR and reason as the
// helper's own session's enrollment does.
func TestASessionKeyTheHelperCannotEnrollIsAnError(t *testing.T) {
	var out syncBuffer
	r := newLoggedRig(t, "", slog.New(slog.NewJSONHandler(&out, nil)))
	if key := r.call(t, Request{Op: "enroll-session-key", RuntimeID: "box-1", Thumbprint: "tp-1"}); key.OK || key.Code != CodeEnrollFailed || key.Error != noCredentialMsg {
		t.Fatalf("enroll-session-key with no credential: %+v; want %s naming %q", key, CodeEnrollFailed, noCredentialMsg)
	}
	line := waitForRecord(t, &out, "session cannot enroll: the helper holds no launcher credential; run: agent-secrets machine login, and have a human approve it")
	for k, v := range map[string]any{
		"level": "ERROR", "runtime_id": "box-1",
		"why": "no machine login since the helper started, and none kept on this machine",
	} {
		if line[k] != v {
			t.Fatalf("the line for the session key: %s = %v; want %v (line %v)", k, line[k], v, line)
		}
	}
}

// noErrors fails t if out holds a record at ERROR other than the drop line itself.
func noErrors(t *testing.T, out *syncBuffer, dropMsg string) {
	t.Helper()
	for _, rec := range logRecords(t, out) {
		if rec["level"] == "ERROR" && rec["msg"] != dropMsg {
			t.Fatalf("an ERROR while the helper held a credential: %v (log:\n%s)", rec, out.String())
		}
	}
}

// TestARefusalOfAReplacedCredentialIsAnOrdinaryRetry: a session's enroll is in flight, signed with
// the credential the helper holds, when a newer login installs another; the broker then refuses
// the old one. The helper drops nothing, since it no longer holds the refused credential, and no
// session is short of one: the failure is the ordinary retry WARN naming the broker's refusal, and
// the retry the login woke enrolls the session with the new credential at once.
func TestARefusalOfAReplacedCredentialIsAnOrdinaryRetry(t *testing.T) {
	var out syncBuffer
	r := newLoggedRig(t, "", slog.New(slog.NewJSONHandler(&out, nil)))
	r.login(t)
	b := r.srv.Broker
	old := b.cred.Load()
	gate, open := testGate(t)
	r.fake.mu.Lock()
	r.fake.enrollGate, r.fake.enrollUnauthorizedNext = gate, 1
	r.fake.mu.Unlock()
	if reg := r.call(t, Request{Op: "register"}); !reg.OK {
		t.Fatalf("register: %+v", reg)
	}
	waitFor(t, func() bool {
		r.fake.mu.Lock()
		defer r.fake.mu.Unlock()
		return r.fake.enrollArrivals == 1
	})
	if _, err := b.Login(context.Background(), r.srv.Hostname); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return b.cred.Load() != old })
	fresh := b.cred.Load()
	open() // the broker refuses the old credential's enroll

	sess := r.srv.Registry.Get(os.Getpid())
	waitFor(t, func() bool { return sess.identity().EnrollmentID != "" })
	if b.cred.Load() != fresh {
		t.Fatal("a refusal of the replaced credential must leave the new one in place")
	}
	retry := waitForRecord(t, &out, "enroll failed; retrying")
	if retry["level"] != "WARN" || !strings.HasPrefix(retry["error"].(string), "broker 401 LAUNCHER_INVALID") {
		t.Fatalf("the failed enroll: %v; want a WARN naming the broker's refusal", retry)
	}
	noErrors(t, &out, "")
}

// onDrop is a slog handler that runs do, once, as the helper logs that it dropped its launcher
// credential, before the drop's caller goes on.
type onDrop struct {
	slog.Handler
	do atomic.Pointer[func()]
}

func (h *onDrop) Handle(ctx context.Context, rec slog.Record) error {
	if strings.HasPrefix(rec.Message, dropRefused) {
		if do := h.do.Swap(nil); do != nil {
			(*do)()
		}
	}
	return h.Handler.Handle(ctx, rec)
}

// TestALoginBetweenARefusalAndItsLineIsAnOrdinaryRetry: the broker refuses the credential a
// session's enroll was signed with, and a login installs a new one before the enroll loop logs
// the failure. The helper holds a credential by then, so the line is the ordinary retry WARN, not
// an ERROR that the session cannot enroll, and the retry the login woke enrolls it at once.
func TestALoginBetweenARefusalAndItsLineIsAnOrdinaryRetry(t *testing.T) {
	var out syncBuffer
	hook := &onDrop{Handler: slog.NewJSONHandler(&out, nil)}
	r := newLoggedRig(t, "", slog.New(hook))
	r.login(t)
	b := r.srv.Broker
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	fresh := &machineCredential{key: key, id: "lcred-fresh", expiresAt: time.Now().Add(7 * 24 * time.Hour), gone: make(chan struct{})}
	install := func() { b.installCredential(fresh, &loginState{State: "issued"}) }
	hook.do.Store(&install)
	r.fake.mu.Lock()
	r.fake.enrollUnauthorizedNext = 1
	r.fake.mu.Unlock()
	if reg := r.call(t, Request{Op: "register"}); !reg.OK {
		t.Fatalf("register: %+v", reg)
	}

	sess := r.srv.Registry.Get(os.Getpid())
	waitFor(t, func() bool { return sess.identity().EnrollmentID != "" })
	if b.cred.Load() != fresh {
		t.Fatal("the session enrolled, but not with the credential the login installed")
	}
	if retry := waitForRecord(t, &out, "enroll failed; retrying"); retry["level"] != "WARN" {
		t.Fatalf("the failed enroll: %v; want a WARN", retry)
	}
	noErrors(t, &out, dropRefused+"; cleared: no session can enroll until a human approves a new machine login (run: agent-secrets machine login)")
}

// TestALoginWakesTheEnrollmentOfASessionRegisteredWithoutACredential: a session registered while
// the helper holds no credential keeps retrying its enrollment with a backoff that grows to a
// minute. The login that installs a credential must wake that retry, so the session can sign
// within about a second of the login instead of whenever its backoff comes round.
func TestALoginWakesTheEnrollmentOfASessionRegisteredWithoutACredential(t *testing.T) {
	r := newRig(t, "")
	if reg := r.call(t, Request{Op: "register"}); !reg.OK || reg.Code != CodeNoCredential {
		t.Fatalf("register with no credential: %+v", reg)
	}
	// Without a credential each attempt fails at once: at 0 s, 1 s and 3 s. The next is due at 7 s,
	// well over the bound below, so only a wake can enroll the session in time.
	time.Sleep(3500 * time.Millisecond)
	r.login(t)
	loggedIn := time.Now()
	for {
		signed := r.call(t, Request{Op: "sign", Method: "GET", URL: "https://secrets.test/v1/enrollments/self"})
		if signed.OK {
			t.Logf("the session signed %s after the login", time.Since(loggedIn))
			return
		}
		if time.Since(loggedIn) > time.Second {
			t.Fatalf("sign %s after the login: %+v; the login must wake the session's enrollment", time.Since(loggedIn), signed)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSignAnswersNoCredentialWhileTheHelperHoldsNone: sign and sign-request for a registered
// session with no enrollment answer NO_CREDENTIAL while the helper holds no launcher credential
// (never logged in, or its credential refused), and NOT_ENROLLED only while it holds one and the
// session is still enrolling; a register reply for that session is OK either way and carries
// NO_CREDENTIAL in the same case. Every enrollment this fake broker sees fails, so the session
// never enrolls and the credential alone decides the answer.
func TestSignAnswersNoCredentialWhileTheHelperHoldsNone(t *testing.T) {
	r := newRig(t, "")
	r.fake.mu.Lock()
	r.fake.failFirst = failAlways
	r.fake.mu.Unlock()
	if reg := r.call(t, Request{Op: "register"}); !reg.OK {
		t.Fatalf("register: %+v", reg)
	}
	signs := []Request{
		{Op: "sign", Method: "GET", URL: "https://secrets.test/v1/enrollments/self"},
		{Op: "sign-request", Secrets: []string{"DEEL_API_KEY"}, Reason: "x"},
	}
	expect := func(when, code string) {
		t.Helper()
		for _, req := range signs {
			resp := r.call(t, req)
			if resp.OK || resp.Code != code {
				t.Fatalf("%s, %s: %+v; want %s", when, req.Op, resp, code)
			}
			if code == CodeNoCredential && resp.Error != noCredentialMsg {
				t.Fatalf("%s, %s: error %q; want %q", when, req.Op, resp.Error, noCredentialMsg)
			}
		}
		regCode := ""
		if code == CodeNoCredential {
			regCode = CodeNoCredential
		}
		reg := r.call(t, Request{Op: "register"})
		if !reg.OK || reg.State != "enrolling" || reg.Code != regCode || (regCode != "" && reg.Error != noCredentialMsg) {
			t.Fatalf("%s, register: %+v; want OK, enrolling, code %q", when, reg, regCode)
		}
	}

	expect("before any login", CodeNoCredential)
	r.login(t)
	expect("logged in, still enrolling", CodeNotEnrolled)

	// The broker refuses the credential (401 LAUNCHER_INVALID) on the next enroll, the session
	// key's or the session's own loop's, whichever comes first; either way the helper holds none
	// afterwards.
	r.fake.mu.Lock()
	r.fake.enrollUnauthorizedNext = 1
	r.fake.mu.Unlock()
	if key := r.call(t, Request{Op: "enroll-session-key", RuntimeID: "box-1", Thumbprint: "tp-1"}); key.OK {
		t.Fatalf("enroll-session-key with a refused credential: %+v", key)
	}
	if r.srv.Broker.HasCredential() {
		t.Fatal("a refused credential must be cleared")
	}
	expect("after the broker refused the credential", CodeNoCredential)
}

// TestARegisterWaitWaitsForAConfirmationThatNeedsNoCredential: a renew of an inherited enrollment
// is signed by the session's own key alone, so a session that comes back for a parked identity
// still becomes enrolled on a helper holding no launcher credential. A caller that asked to wait
// must be given that answer, not the snapshot from before it: a register that does not wait here
// answers NO_CREDENTIAL and `agent-secrets register --wait N` exits 1 for a session the broker
// confirms milliseconds later. The fake holds the confirming renew at its renew gate until the
// waiting call is blocked in registerReply, so what the row reads is its own ordering and not how
// fast the host is.
func TestARegisterWaitWaitsForAConfirmationThatNeedsNoCredential(t *testing.T) {
	r, _, _, release := reclaimedIdentityRig(t)

	// The register that admits the session and starts its enroll loop, whose renew is then held
	// at the gate. With no wait asked for it answers at once, on the identity as it stands.
	id := testSessionID
	first := r.call(t, Request{Op: "register", SessionID: &id})
	if first.Code != CodeNoCredential || first.State == "enrolled" {
		t.Fatalf("register with no wait: %+v; want NO_CREDENTIAL and a state that is not enrolled", first)
	}

	// The same session again, this time asking to wait: it must wait for the confirmation the
	// gate holds, although the helper holds no launcher credential. A wait taken only while a
	// credential is held would answer at once here.
	replies := registerWaitBlocked(t, r, &id)
	release()
	select {
	case got := <-replies:
		if got.err != nil || !got.resp.OK || got.resp.Code != "" || got.resp.State != "enrolled" || got.resp.EnrollmentID != "enr-before" {
			t.Fatalf("register --wait after the confirmation: %+v (%v); want OK, enrolled, enr-before and no code", got.resp, got.err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("register --wait never answered after the confirming renew was released")
	}
}

// TestARegisterWaitAnswersAtOnceOnceTheBrokerRefusesTheInheritedEnrollment: the broker answers
// that it will not honour the enrollment the session took from its record, so that confirmation
// is over, and the enroll the session falls through to wants a launcher credential nobody has
// installed. NO_CREDENTIAL is then the whole answer, and no wait can change it: `register --wait
// N` must give it at once, or the launcher gate's unconditional `--wait 10`
// (scripts/agent-secrets-session) stalls every launch ten seconds for the rest of the session's
// life.
func TestARegisterWaitAnswersAtOnceOnceTheBrokerRefusesTheInheritedEnrollment(t *testing.T) {
	r := reclaimedIdentityRigRefusingRenew(t)

	// The register that admits the session and starts its enroll loop, whose confirming renew the
	// fake refuses. With no wait asked for it answers at once, on the identity as it stands.
	id := testSessionID
	if first := r.call(t, Request{Op: "register", SessionID: &id}); first.Code != CodeNoCredential {
		t.Fatalf("register with no wait: %+v; want NO_CREDENTIAL", first)
	}
	// The refusal is that one confirmation attempt's answer, and from it the session reads no
	// confirmation in flight (Session.confirmationInFlight), which is what ends the wait.
	waitFor(t, func() bool {
		sess := r.srv.Registry.Get(os.Getpid())
		return sess != nil && sess.confirmationInFlight() == nil
	})

	// The same session again, this time asking to wait. Nothing can confirm the refused id, so the
	// answer is the one the helper already has and the caller must not pay the wait for it.
	started := time.Now()
	resp := r.call(t, Request{Op: "register", WaitSeconds: 5, SessionID: &id})
	if elapsed := time.Since(started); elapsed > time.Second || resp.Code != CodeNoCredential {
		t.Fatalf("register --wait 5 answered %+v after %s; want NO_CREDENTIAL well inside the wait, since the broker has refused the inherited enrollment and no confirmation can come", resp, elapsed)
	}
}

// TestARegisterWaitAnswersAtOnceWhenNoConfirmationIsInFlight: the wait a register takes on a
// helper holding no launcher credential is for ONE confirmation attempt, the one in flight when
// it arrives, and a register that finds none waits for nothing. Two rows hold that line, the
// cases either side of a wait that is earned: a session whose confirmation keeps failing to
// reach the broker, whose attempts the enroll loop retries for as long as the outage lasts, and
// a session that never had a record to inherit an enrollment from at all.
func TestARegisterWaitAnswersAtOnceWhenNoConfirmationIsInFlight(t *testing.T) {
	// The broker cannot ANSWER the confirming renew: a 503, not a refusal, so confirmInherited
	// keeps retrying it and the session stays unconfirmed for as long as the outage lasts. A
	// wait that covered those retries would cost every launch its whole N for all that time.
	t.Run("the broker cannot answer the confirming renew", func(t *testing.T) {
		r := reclaimedIdentityRigFailingRenew(t)

		// The register that admits the session and starts its enroll loop, whose confirming
		// renew the fake answers 503. With no wait asked for it answers at once.
		id := testSessionID
		if first := r.call(t, Request{Op: "register", SessionID: &id}); first.Code != CodeNoCredential {
			t.Fatalf("register with no wait: %+v; want NO_CREDENTIAL", first)
		}
		// The first attempt's answer, read from the BROKER's side: a second renew arriving is
		// the enroll loop having taken the first one's 503 and retried it a second later.
		// Reading it there rather than from the session leaves the register below the only
		// thing the settle this row pins can change, and it puts a retry of the enroll loop's
		// own in flight while that register runs — the very attempt a register that did not
		// see it start must not wait on.
		waitFor(t, func() bool {
			r.fake.mu.Lock()
			defer r.fake.mu.Unlock()
			return r.fake.renewAttempts >= 2
		})

		// The same session again, asking to wait. The attempt this register could have waited
		// on has been answered; the next one is the enroll loop's own, and nothing waits on a
		// retry it did not see start.
		started := time.Now()
		resp := r.call(t, Request{Op: "register", WaitSeconds: 2, SessionID: &id})
		if elapsed := time.Since(started); elapsed > time.Second || resp.Code != CodeNoCredential {
			t.Fatalf("register --wait 2 answered %+v after %s; want NO_CREDENTIAL well inside the wait, since the confirmation attempt in flight when it arrived has been answered and the enroll loop's retries are not waited on", resp, elapsed)
		}
	})

	// A session with no parked record at all, the headline promise: it has no enrollment to
	// confirm, so there is nothing a wait can shorten and every launch gets its answer at once.
	t.Run("a session with no parked record at all", func(t *testing.T) {
		r := newRig(t, "")
		r.callerPID.Store(int64(os.Getpid()))

		started := time.Now()
		resp := r.call(t, Request{Op: "register", WaitSeconds: 2})
		if elapsed := time.Since(started); elapsed > time.Second || resp.Code != CodeNoCredential {
			t.Fatalf("register --wait 2 answered %+v after %s; want NO_CREDENTIAL at once, since the helper holds no launcher credential and this session inherited no enrollment anything could confirm", resp, elapsed)
		}
	})
}

// TestARegisterWaitAnswersAtOnceWhenALapsedIDBlocksTheConfirmation: a session can come back
// holding BOTH an enrollment to reclaim and a lapsed one to revoke (reclaimIdentity's Revoke,
// and Recover's re-pin). enrollLoop revokes that lapsed id BEFORE it reaches confirmInherited,
// so the lapsed id is revoked before the confirmation is ever sent — and Broker.Revoke signs
// through signingAuthority, which tries a person's sign-in before the machine credential,
// while this rig holds neither. So nothing lets that revoke through, no confirming renew is
// ever sent, and nothing can ever answer a wait taken for one. The register
// must say so at once: waiting would spend the caller's whole N and then answer NO_CREDENTIAL
// all the same, which the launcher gate's unconditional `--wait 10`
// (scripts/agent-secrets-session) would pay on every launch for the rest of the session's life.
func TestARegisterWaitAnswersAtOnceWhenALapsedIDBlocksTheConfirmation(t *testing.T) {
	r := reclaimedIdentityRigWithLapsedID(t)

	// The register that admits the session and starts its enroll loop, which is stuck on the
	// revoke of enr-lapsed. With no wait asked for it answers at once, on the identity as it
	// stands: the reclaimed id is inherited, so the session is not enrolled.
	id := testSessionID
	if first := r.call(t, Request{Op: "register", SessionID: &id}); first.Code != CodeNoCredential {
		t.Fatalf("register with no wait: %+v; want NO_CREDENTIAL", first)
	}

	// The same session again, this time asking to wait. The confirmation it would wait for is
	// behind a revoke nobody can make, so the answer is the one the helper already has.
	started := time.Now()
	resp := r.call(t, Request{Op: "register", WaitSeconds: 3, SessionID: &id})
	if elapsed := time.Since(started); elapsed > time.Second || resp.Code != CodeNoCredential {
		t.Fatalf("register --wait 3 answered %+v after %s; want NO_CREDENTIAL well inside the wait, since the lapsed id is revoked before the confirmation is ever sent and the revoke goes through on a sign-in or a machine credential, neither of which this rig holds, so no renew can be sent", resp, elapsed)
	}
}

// TestARegisterWaitAnswersWithinTheBoundWhenTheBrokerNeverAnswers: the outage a 503 does not
// cover. A broker that ACCEPTS the connection and never answers holds the confirming renew
// for as long as the helper lets it, and unbounded that is the helper's whole client timeout
// (leasetime.RenewClientTimeout, 30 s): every launch of this session's process tree is a new
// pid that reclaims afresh, gets its own first renew, and pays its whole `--wait` — 10 s at
// the dotfiles launcher gate (scripts/agent-secrets-session) — for as long as the outage
// lasts. So the first confirming renew is bounded (Server.FirstConfirmBound): when the bound
// expires that attempt has answered as one that did not reach the broker, the wait ends, and
// the enroll loop retries on its own schedule, exactly as it does for a 503.
func TestARegisterWaitAnswersWithinTheBoundWhenTheBrokerNeverAnswers(t *testing.T) {
	// The production bound is DefaultFirstConfirmBound, 3 s; this row sets a short one of its
	// own so it is quick and deterministic. What it pins is the mechanism — a first renew
	// nothing answers ends the wait at the bound rather than at the client timeout — and not
	// that number.
	const bound = 500 * time.Millisecond
	r := reclaimedIdentityRigNeverAnswering(t, bound)

	// The register that admits the session and starts its enroll loop, whose confirming renew
	// the gate then swallows. With no wait asked for it answers at once.
	id := testSessionID
	if first := r.call(t, Request{Op: "register", SessionID: &id}); first.Code != CodeNoCredential {
		t.Fatalf("register with no wait: %+v; want NO_CREDENTIAL", first)
	}
	// Read from the BROKER's side that the renew is out and waiting at the gate, so the
	// register below arrives with that first confirmation in flight and takes the wait.
	waitFor(t, func() bool {
		r.fake.mu.Lock()
		defer r.fake.mu.Unlock()
		return r.fake.renewAttempts >= 1
	})

	// The same session again, asking for the launcher gate's own wait, through the helper that
	// returns once the call is blocked in registerReply: the register has taken the wait, so
	// what ends it below is the bound and not a wait never taken. Its client deadline is 20 s,
	// past the 10 s asked for, so an unbounded first renew is read here as this row's own
	// failure and not as a client that gave up.
	started := time.Now()
	replies := registerWaitBlocked(t, r, &id)
	got := <-replies
	elapsed := time.Since(started)
	// The floor is half the bound, not the whole of it: the bound runs from the renew leaving
	// the helper, which is a poll of the loop above before this call starts, so a reply at
	// exactly the bound can be measured a little short here. Half of it still parts a reply
	// that waited for the bound from one that waited for nothing (those answer in ~0 s).
	if got.err != nil || got.resp.Code != CodeNoCredential || elapsed > 5*time.Second || elapsed < bound/2 {
		t.Fatalf("register --wait 10 answered %+v (%v) after %s; want NO_CREDENTIAL at the bound (%s): well inside the 10 s wait, since a broker that never answers must not hold a launcher's gate for the helper's whole client timeout, and no sooner than the bound, since this register waits for the confirmation in flight", got.resp, got.err, elapsed, bound)
	}
}

// TestARegisterWaitAnswersWithinTheBoundWhenTheCredentialWentAwayUnderTheRenew: the bound above
// is the register's promise, but the register and the renew read the helper's machine
// credential at different moments, so no gate on that credential can keep it — whichever of
// them reads first can be right about a state the other no longer sees. Here the first
// confirming renew BEGINS while a credential is held (a re-pin or a reclaim on a logged-in
// helper), the credential is then refused and cleared, and a register on that same session
// arrives reading none: it waits on the renew in flight, and only a renew bounded whatever the
// credential was when it started can answer it. Unbounded, that wait is the launcher gate's
// whole `--wait 10` (scripts/agent-secrets-session) on every launch for as long as the outage
// lasts, which is what the bound exists to prevent.
func TestARegisterWaitAnswersWithinTheBoundWhenTheCredentialWentAwayUnderTheRenew(t *testing.T) {
	// Short, for the same reason as the row above: what this pins is the mechanism, not the
	// number. The bound may well expire before the register below arrives — the credential
	// drop takes a round trip of its own — and that is the same answer by the same means: the
	// first attempt ended at the bound instead of running to the client timeout.
	const bound = 300 * time.Millisecond
	r := reclaimedIdentityRigLoggedInNeverAnswering(t, bound)

	// The register that admits the session and starts its enroll loop, whose confirming renew
	// the gate then swallows. The helper is logged in, so this answers OK with no code.
	id := testSessionID
	if first := r.call(t, Request{Op: "register", SessionID: &id}); !first.OK || first.Code != "" {
		t.Fatalf("register with no wait: %+v; want OK and no code while the helper holds a credential", first)
	}
	// Read from the BROKER's side that the renew is out and waiting at the gate, so the
	// credential is dropped AFTER that first attempt began and the interleaving this row is
	// about is read rather than assumed.
	waitFor(t, func() bool {
		r.fake.mu.Lock()
		defer r.fake.mu.Unlock()
		return r.fake.renewAttempts >= 1
	})

	// The broker refuses the credential (401 LAUNCHER_INVALID) on the next enroll, as
	// TestSignAnswersNoCredentialWhileTheHelperHoldsNone does: afterwards the helper holds
	// none, and the renew already at the gate was started by a helper that did.
	r.fake.mu.Lock()
	r.fake.enrollUnauthorizedNext = 1
	r.fake.mu.Unlock()
	if key := r.call(t, Request{Op: "enroll-session-key", RuntimeID: "box-1", Thumbprint: "tp-1"}); key.OK {
		t.Fatalf("enroll-session-key with a refused credential: %+v", key)
	}
	if r.srv.Broker.HasCredential() {
		t.Fatal("a refused credential must be cleared")
	}

	// The same session again, asking for the launcher gate's own wait. It reads no credential,
	// so it waits for the confirmation in flight — the one started under a credential. The call
	// is made with a 20 s deadline, past the 10 s asked for, so a wait that runs its whole
	// course is read here as this row's own failure and not as a client that gave up.
	started := time.Now()
	resp, err := Call(r.sock, Request{Op: "register", WaitSeconds: 10, SessionID: &id}, 20*time.Second)
	elapsed := time.Since(started)
	if err != nil || resp.Code != CodeNoCredential || elapsed > 5*time.Second {
		t.Fatalf("register --wait 10 answered %+v (%v) after %s; want NO_CREDENTIAL at the bound (%s), well inside the 10 s wait: the renew it waits on began while the helper held a credential, and a bound that is skipped for that renew leaves this register waiting the whole of a launcher gate's wait", resp, err, elapsed, bound)
	}
}

// TestANameTakenDuringAnEnrollRetryConfirmsItsInheritedEnrollmentAtOnce: a launcher cannot name
// a fresh session before its exec, so a resumed one registers unnamed first and is named
// seconds later. That first registration starts an enroll the helper cannot make for want of a
// launcher credential, and its retry backs off towards a minute; the name then brings a parked
// identity with it, whose inherited enrollment a renew signed by the session's own key confirms
// with no credential at all. The `register --wait N` carrying the name is already waiting for
// that renew (Session.confirmSettled), so the rename's wake must start it at once: a retry
// woken by a login alone leaves the renew unsent until the backoff comes round, and the
// launcher gate spends its wait on a backoff this confirmation has no part in.
func TestANameTakenDuringAnEnrollRetryConfirmsItsInheritedEnrollmentAtOnce(t *testing.T) {
	r, _, _, release := reclaimedIdentityRig(t)
	release() // nothing holds the confirming renew here: this fake answers it as a healthy broker does

	// The unnamed register, which starts the enroll loop. Every enroll fails at once for want
	// of a credential, so from here on the loop is in its backoff.
	if first := r.call(t, Request{Op: "register"}); first.Code != CodeNoCredential {
		t.Fatalf("register with no name: %+v; want NO_CREDENTIAL", first)
	}

	// Far enough into that backoff that nothing but the rename's own wake can answer the
	// register below inside a second. The attempts fall at 0 s, 1 s, 3 s and 7 s — one second
	// doubling after each failure — so four seconds in sits inside the 4 s between the third
	// and the fourth with about three of them left to run, past the second this row allows and
	// a second clear of each end of that window on a loaded host. The delay is the whole of
	// what the row costs, so it is as short as proving the point leaves it. A mistimed delay
	// could only weaken the negative control, never make this row flaky: with the rename's
	// wake in place the answer is immediate wherever in the ladder the register lands.
	time.Sleep(4 * time.Second)

	// The name, with the wait the dotfiles launcher gate asks for
	// (scripts/agent-secrets-session). The rename installs the record's enrollment and wakes
	// the enroll retry; the woken attempt reads the new generation, ends that retry
	// (errRenamed), and the pass that follows renews the inherited enrollment straight away.
	id := testSessionID
	started := time.Now()
	resp := r.call(t, Request{Op: "register", WaitSeconds: 10, SessionID: &id})
	if elapsed := time.Since(started); elapsed > time.Second || !resp.OK || resp.State != "enrolled" || resp.EnrollmentID != "enr-before" {
		t.Fatalf("register --session-id --wait 10 answered %+v after %s; want OK, enrolled and enr-before within a second: the renew that confirms an inherited enrollment needs no launcher credential, so the rename must start it rather than leave it to the enroll retry's backoff", resp, elapsed)
	}
}
