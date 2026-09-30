// packages/envoy/internal/broker/helper/nocredential_test.go
//go:build linux

package helper

import "testing"

// TestSignAnswersNoCredentialWhileTheHelperHoldsNone: sign and sign-request for a registered
// session with no enrollment answer NO_CREDENTIAL while the helper holds no launcher credential
// (never logged in, or its credential refused), and NOT_ENROLLED only while it holds one and the
// session is still enrolling; a register reply for that session is OK either way and carries
// NO_CREDENTIAL in the same case. Every enrollment this fake broker sees fails, so the session
// never enrolls and the credential alone decides the answer.
func TestSignAnswersNoCredentialWhileTheHelperHoldsNone(t *testing.T) {
	r := newRig(t, "")
	r.fake.mu.Lock()
	r.fake.failFirst = 1 << 30
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
