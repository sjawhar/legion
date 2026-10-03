// packages/envoy/internal/broker/helper/nocredential_test.go
//go:build linux

package helper

import (
	"context"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/proof"
)

// TestNoCredentialReasonNamesWhyTheHelperHoldsNone: every state login-status can report with no
// credential held has its own reason, which the helper's journal and `launcher login-status` both
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
		{"never logged in since the start", "", false, "", "no machine login since the helper started; a restart discards the launcher credential"},
		{"a login waiting for approval", "pending", false, "", "a machine login is waiting for a human to approve it"},
		{"a login waiting for approval beside a dropped credential", "pending", true, dropExpired, "a machine login is waiting for a human to approve it"},
		{"a denied login", "denied", false, "", "the most recent machine login was denied"},
		{"a login nobody approved in time", "expired", false, "", "the most recent machine login expired before anyone approved it"},
		{"dropped at its expiry", "expired", true, dropExpired, "the launcher credential reached its expiry"},
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

// TestABoxTheHelperCannotEnrollIsAnError: an enroll-box the helper cannot attempt for want of a
// launcher credential fails, as before, and logs the same ERROR and reason as a host session's
// enrollment does.
func TestABoxTheHelperCannotEnrollIsAnError(t *testing.T) {
	var out syncBuffer
	r := newLoggedRig(t, "", slog.New(slog.NewJSONHandler(&out, nil)))
	if box := r.call(t, Request{Op: "enroll-box", RuntimeID: "box-1", Thumbprint: "tp-1"}); box.OK || box.Code != CodeEnrollFailed || box.Error != noCredentialMsg {
		t.Fatalf("enroll-box with no credential: %+v; want %s naming %q", box, CodeEnrollFailed, noCredentialMsg)
	}
	line := waitForRecord(t, &out, "session cannot enroll: the helper holds no launcher credential; run: agent-secrets launcher login, and have a human approve it")
	for k, v := range map[string]any{
		"level": "ERROR", "runtime_id": "box-1",
		"why": "no machine login since the helper started; a restart discards the launcher credential",
	} {
		if line[k] != v {
			t.Fatalf("the line for the box: %s = %v; want %v (line %v)", k, line[k], v, line)
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
	waitFor(t, func() bool { return sess.EnrollmentID() != "" })
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
	fresh := &machineCredential{key: key, id: "lcred-fresh", gone: make(chan struct{})}
	install := func() { b.installCredential(fresh, &loginState{State: "issued"}) }
	hook.do.Store(&install)
	r.fake.mu.Lock()
	r.fake.enrollUnauthorizedNext = 1
	r.fake.mu.Unlock()
	if reg := r.call(t, Request{Op: "register"}); !reg.OK {
		t.Fatalf("register: %+v", reg)
	}

	sess := r.srv.Registry.Get(os.Getpid())
	waitFor(t, func() bool { return sess.EnrollmentID() != "" })
	if b.cred.Load() != fresh {
		t.Fatal("the session enrolled, but not with the credential the login installed")
	}
	if retry := waitForRecord(t, &out, "enroll failed; retrying"); retry["level"] != "WARN" {
		t.Fatalf("the failed enroll: %v; want a WARN", retry)
	}
	noErrors(t, &out, dropRefused+"; cleared: no session can enroll until a human approves a new machine login (run: agent-secrets launcher login)")
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

	// The broker refuses the credential (401 LAUNCHER_INVALID) on the next enroll, the box's or the
	// session's own loop's, whichever comes first; either way the helper holds none afterwards.
	r.fake.mu.Lock()
	r.fake.enrollUnauthorizedNext = 1
	r.fake.mu.Unlock()
	if box := r.call(t, Request{Op: "enroll-box", RuntimeID: "box-1", Thumbprint: "tp-1"}); box.OK {
		t.Fatalf("enroll-box with a refused credential: %+v", box)
	}
	if r.srv.Broker.HasCredential() {
		t.Fatal("a refused credential must be cleared")
	}
	expect("after the broker refused the credential", CodeNoCredential)
}
