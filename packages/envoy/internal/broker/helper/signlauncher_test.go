// packages/envoy/internal/broker/helper/signlauncher_test.go
//go:build linux

package helper

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/proof"
)

// TestSignLauncherRefusedInsideASessionAndSignsOutside: sign-launcher answers a process inside a
// registered session IN_SESSION, since a session acts on itself alone, and the operator's own shell
// NO_CREDENTIAL until a login: the refusal order proves the session gate ran first.
func TestSignLauncherRefusedInsideASessionAndSignsOutside(t *testing.T) {
	state := filepath.Join(t.TempDir(), "sessions.json")
	r := newRig(t, state)
	child := sleeper(t) // a real process registered as a session root
	r.callerPID.Store(int64(child.Process.Pid))
	if resp := r.call(t, Request{Op: "register"}); !resp.OK && resp.Code != CodeNoCredential {
		t.Fatalf("register: %+v", resp)
	}
	if resp := r.call(t, Request{Op: "sign-launcher", Method: "GET", URL: r.srv.Broker.URL + "/v1/operator/machines"}); resp.Code != CodeInSession {
		t.Fatalf("inside a session: %+v, want IN_SESSION", resp)
	}
	r.callerPID.Store(0) // the operator's own shell: not a registered session
	resp := r.call(t, Request{Op: "sign-launcher", Method: "GET", URL: r.srv.Broker.URL + "/v1/operator/machines"})
	if resp.Code != CodeNoCredential || resp.Error != noCredentialMsg {
		t.Fatalf("outside: %+v, want NO_CREDENTIAL until a login", resp)
	}
}

// TestSignLauncherSignsOnlyTheOperatorRoutesOfThisHelpersBroker: once logged in, sign-launcher
// answers the operator's shell a launcher proof for the held machine credential, which verifies
// with that credential's key for exactly the method and URL asked, for each of the broker's four
// operator routes. Every other call gets no proof: another broker, a host that only begins like
// this broker's, another route of this broker (an enrollment, which a launcher proof also
// authenticates), an operator route under the other method or with its id missing, split or a
// dot segment, and a request missing its method or URL.
func TestSignLauncherSignsOnlyTheOperatorRoutesOfThisHelpersBroker(t *testing.T) {
	r := startRig(t, "")
	cred := r.srv.Broker.cred.Load()
	tp, err := proof.Thumbprint(&cred.key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	v := &proof.Verifier{Skew: time.Minute,
		LookupLauncher: func(_ context.Context, id string) (string, bool, error) { return tp, id == cred.id, nil },
		Replay:         func(context.Context, string, time.Time) (bool, error) { return true, nil }}
	broker := r.srv.Broker.URL
	id := "0f9a6c1e-2b7d-4c3a-9e5f-8a1b2c3d4e5f"
	for _, call := range []struct{ method, url string }{
		{http.MethodGet, broker + "/v1/operator/machines"},
		{http.MethodPost, broker + "/v1/operator/machines/" + id + "/revoke"},
		{http.MethodGet, broker + "/v1/operator/grants"},
		{http.MethodPost, broker + "/v1/operator/grants/" + id + "/revoke"},
	} {
		resp := r.call(t, Request{Op: "sign-launcher", Method: call.method, URL: call.url})
		if !resp.OK || resp.Proof == "" {
			t.Fatalf("sign-launcher %s %s with a credential: %+v", call.method, call.url, resp)
		}
		if sub, err := v.Verify(context.Background(), resp.Proof, call.method, call.url, time.Now()); err != nil || sub.LauncherID != cred.id || resp.CredentialID != cred.id {
			t.Fatalf("%s %s: the proof must verify as a launcher proof of the held credential %s, and the answer name it: %+v %+v %v", call.method, call.url, cred.id, resp, sub, err)
		}
	}
	for name, req := range map[string]Request{
		"another broker":                   {Op: "sign-launcher", Method: "GET", URL: "https://elsewhere.test/v1/operator/grants"},
		"a host beginning like the broker": {Op: "sign-launcher", Method: "GET", URL: broker + "0/v1/operator/grants"},
		"the broker's bare origin":         {Op: "sign-launcher", Method: "GET", URL: broker},
		"an enrollment":                    {Op: "sign-launcher", Method: "POST", URL: broker + "/v1/enrollments"},
		"an unenrollment":                  {Op: "sign-launcher", Method: "DELETE", URL: broker + "/v1/enrollments/" + id},
		"the list under POST":              {Op: "sign-launcher", Method: "POST", URL: broker + "/v1/operator/machines"},
		"a revoke under GET":               {Op: "sign-launcher", Method: "GET", URL: broker + "/v1/operator/grants/" + id + "/revoke"},
		"a revoke with no id":              {Op: "sign-launcher", Method: "POST", URL: broker + "/v1/operator/machines//revoke"},
		"a revoke of a split id":           {Op: "sign-launcher", Method: "POST", URL: broker + "/v1/operator/machines/a/b/revoke"},
		"a revoke of a dot segment":        {Op: "sign-launcher", Method: "POST", URL: broker + "/v1/operator/machines/../revoke"},
		"a revoke of an encoded slash":     {Op: "sign-launcher", Method: "POST", URL: broker + "/v1/operator/machines/a%2Fb/revoke"},
		"a revoke of encoded dots":         {Op: "sign-launcher", Method: "POST", URL: broker + "/v1/operator/machines/%2e%2e/revoke"},
		"a revoke with a query":            {Op: "sign-launcher", Method: "POST", URL: broker + "/v1/operator/grants/" + id + "/revoke?x"},
		"a list with a query":              {Op: "sign-launcher", Method: "GET", URL: broker + "/v1/operator/grants?approver=x"},
		"no method":                        {Op: "sign-launcher", URL: broker + "/v1/operator/grants"},
		"no url":                           {Op: "sign-launcher", Method: "GET"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := r.call(t, req); got.OK || got.Code != CodeBadRequest || got.Proof != "" {
				t.Fatalf("%+v, want BAD_REQUEST and no proof", got)
			}
		})
	}
}

// TestSignLauncherRefusesACallerItCannotTraceOutOfEverySession: sign-launcher signs only for a
// process it has shown to be outside every registered session, so a walk from the caller that
// ends short of init, at a parent it cannot read, at the hop limit or in a loop, is answered
// IN_SESSION naming why. A walk that does reach init signs, so the refusals are the walk's.
func TestSignLauncherRefusesACallerItCannotTraceOutOfEverySession(t *testing.T) {
	r := startRig(t, "")
	child := sleeper(t) // a real process registered as a session root, so there is a walk to make
	r.callerPID.Store(int64(child.Process.Pid))
	if reg := r.call(t, Request{Op: "register"}); !reg.OK {
		t.Fatalf("register: %+v", reg)
	}
	r.callerPID.Store(0) // the caller is this test process, which is not in the child's tree
	self := os.Getpid()
	// Every walk leaves self for pids past any kernel's pid_max, so none meets the session root.
	const far = 10_000_000
	url := r.srv.Broker.URL + "/v1/operator/machines"
	for _, tc := range []struct {
		name   string
		parent func(int) (int, error)
		want   string // "" signs; otherwise the reason IN_SESSION must name
	}{
		{"a walk that reaches init", func(pid int) (int, error) {
			if pid == self {
				return far, nil
			}
			return 1, nil
		}, ""},
		{"a parent it cannot read", func(pid int) (int, error) {
			if pid == self {
				return far, nil
			}
			return 0, errors.New("open /proc/10000000/stat: no such file or directory")
		}, "reading the parent of pid 10000000: open /proc/10000000/stat: no such file or directory"},
		{"a walk past the hop limit", func(pid int) (int, error) {
			if pid == self {
				return far, nil
			}
			return pid + 1, nil
		}, "ancestry runs past 64 processes"},
		{"a loop", func(pid int) (int, error) {
			if pid == self || pid == far+1 {
				return far, nil
			}
			return far + 1, nil
		}, "ancestry loops at pid 10000000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r.parentOf.Store(&tc.parent)
			resp := r.call(t, Request{Op: "sign-launcher", Method: http.MethodGet, URL: url})
			switch {
			case tc.want == "" && (!resp.OK || resp.Proof == ""):
				t.Fatalf("%+v, want a proof", resp)
			case tc.want != "" && (resp.OK || resp.Code != CodeInSession || resp.Proof != "" || !strings.Contains(resp.Error, tc.want)):
				t.Fatalf("OK=%v code=%q error=%q proof=%t, want IN_SESSION naming %q and no proof", resp.OK, resp.Code, resp.Error, resp.Proof != "", tc.want)
			}
		})
	}
}

// TestSignLauncherRejectsAPeerWhosePIDChangedDuringTheAncestryWalk: sign-launcher trusts the
// ancestry walk's "not in a session" only while the peer still names the pid it walked from, as
// sign does; otherwise a session process the kernel replaced mid-walk could be handed a launcher
// proof.
func TestSignLauncherRejectsAPeerWhosePIDChangedDuringTheAncestryWalk(t *testing.T) {
	r := startRig(t, "")
	impostor := sleeper(t)
	peer, err := PinPID(impostor.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	resp := r.srv.signLauncher(peer, os.Getpid(), "GET", r.srv.Broker.URL+"/v1/operator/machines")
	if resp.OK || resp.Code != CodeUnidentified || resp.Proof != "" {
		t.Fatalf("a peer no longer matching the walked pid must be refused a launcher proof: %+v", resp)
	}
}
