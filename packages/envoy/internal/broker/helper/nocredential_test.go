// packages/envoy/internal/broker/helper/nocredential_test.go
//go:build linux

package helper

import (
	"testing"
	"time"
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
