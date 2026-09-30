// packages/envoy/internal/broker/helper/server_test.go
//go:build linux

package helper

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
)

type rig struct {
	fake   *fakeBroker
	srv    *Server
	sock   string
	cancel context.CancelFunc
	// callerPID is the pid PeerOf presents for the next connection; 0 means the real peer.
	// Atomic: the server goroutine reads it while the test goroutine writes it.
	callerPID atomic.Int64
}

// newRig builds and serves a rig whose Broker holds no machine credential yet: the caller must
// log it in (see rig.login) before any Enroll — host session or box — can succeed, since Task
// 1's Broker fails closed until a human has run `agent-secrets launcher login`. startRig below
// is newRig plus that login, for every test that doesn't care about the pre-login state itself.
func newRig(t *testing.T, statePath string) *rig {
	t.Helper()
	f := newFakeBroker(t)
	of := operatorFile(t, "sjawhar")
	r := &rig{fake: f}
	if statePath == "" {
		statePath = filepath.Join(t.TempDir(), "sessions.json")
	}
	r.srv = &Server{
		Registry: NewRegistry(statePath),
		Broker:   &Broker{URL: f.srv.URL, OperatorFile: of, HTTP: f.srv.Client()},
		Hostname: "testhost",
		Log:      slog.New(slog.NewTextHandler(os.Stderr, nil)),
		MinRenew: 50 * time.Millisecond,
	}
	r.srv.PeerOf = func(conn *net.UnixConn) (*Peer, error) {
		if pid := r.callerPID.Load(); pid != 0 {
			return PinPID(int(pid))
		}
		return PeerOf(conn)
	}
	r.sock = filepath.Join(t.TempDir(), "helper.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: r.sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	served := make(chan struct{})
	t.Cleanup(func() {
		// A registered session's watchExit goroutine only ends when Registry.Remove closes its
		// stop channel — cancel(ctx) alone stops enrollLoop/renewLoop (they select on
		// ctx.Done()) but never watchExit (see retire's doc comment). A test whose
		// sleeper(t)-spawned child is killed by ITS OWN t.Cleanup (which runs before this one,
		// since t.Cleanup is LIFO and sleeper is called after startRig returns) has usually
		// already died by the time we get here, so retire() has often already removed the
		// session (and started an async Registry.Save()) before this sweep even runs. Close a
		// session's peer only when our own Remove call is the one that actually found and
		// removed it — otherwise retire() already owns (or will own) that Close, and racing it
		// here would double-close the pidfd.
		for _, sess := range r.srv.Registry.List() {
			if r.srv.Registry.Remove(sess) && sess.peer != nil {
				sess.peer.Close()
			}
		}
		// Registry.Save holds saveMu for its whole WriteFile+Rename. Taking it here — and
		// never releasing it — blocks any Save already in flight (or about to start: a
		// retire() that removed its session above before this sweep ever saw it, or an
		// enrollLoop between setEnrolled and its own save() a few lines later) until that
		// write finishes, before the context is canceled and TempDir's own cleanup can run. A
		// later Save() attempt simply blocks forever on this held lock, harmlessly, until the
		// test binary itself exits — nothing else in this test needs the registry again.
		r.srv.Registry.saveMu.Lock()
		cancel()
		// Serve's own ctx.Done() watcher goroutine closes (and so unlinks) the unix socket
		// asynchronously; wait for Serve itself to return — which only happens after that
		// Close — so the socket is gone before this test's t.TempDir() cleanup removes the
		// directory it lives in.
		<-served
	})
	r.srv.Recover(ctx)
	go func() { defer close(served); _ = r.srv.Serve(ctx, ln) }()
	return r
}

// startRig is newRig plus an immediate machine login, so the returned rig's Broker already
// holds a credential and ordinary session enrollment (Enroll) can succeed. Tests exercising the
// pre-login state itself (TestLoginOverTheSocketReturnsTheCodeAndEnrollBoxWorksAfterIssue,
// TestSignStillRequiresDescendancyButEnrollBoxDoesNot) call newRig directly instead.
func startRig(t *testing.T, statePath string) *rig {
	t.Helper()
	r := newRig(t, statePath)
	r.login(t)
	return r
}

// login logs r's Broker in against its own fake and waits for the credential to install.
func (r *rig) login(t *testing.T) {
	t.Helper()
	if _, err := r.srv.Broker.Login(context.Background(), r.srv.Hostname); err != nil {
		t.Fatalf("rig login: %v", err)
	}
	waitFor(t, func() bool { return r.srv.Broker.LoginStatus().State == "issued" })
}

func (r *rig) call(t *testing.T, req Request) Response {
	t.Helper()
	resp, err := Call(r.sock, req, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestRegisterPinsCallerEnrollsAndSigns(t *testing.T) {
	r := startRig(t, "")
	resp := r.call(t, Request{Op: "register", WaitSeconds: 5})
	if !resp.OK || resp.State != "enrolled" || resp.EnrollmentID == "" || resp.Operator != "sjawhar" {
		t.Fatalf("register: %+v", resp)
	}
	want := "testhost:" + strconv.Itoa(os.Getpid()) + ":"
	if len(resp.RuntimeID) <= len(want) || resp.RuntimeID[:len(want)] != want {
		t.Fatalf("runtime_id %q must be <hostname>:<pid>:<ticks>", resp.RuntimeID)
	}
	posts, _, _ := r.fake.snapshot()
	if posts[0]["kind"] != "host" {
		t.Fatalf("enrolled as %v", posts[0]["kind"])
	}
	url := "https://secrets.test/v1/requests"
	signed := r.call(t, Request{Op: "sign", Method: "POST", URL: url})
	if !signed.OK || signed.EnrollmentID != resp.EnrollmentID {
		t.Fatalf("sign: %+v", signed)
	}
	sess := r.srv.Registry.Get(os.Getpid())
	v := &proof.Verifier{Skew: time.Minute,
		Lookup: func(context.Context, string) (string, bool, error) { return sess.Thumbprint, true, nil },
		Replay: func(context.Context, string, time.Time) (bool, error) { return true, nil }}
	if sub, err := v.Verify(context.Background(), signed.Proof, "POST", url, time.Now()); err != nil || sub.EnrollmentID != resp.EnrollmentID {
		t.Fatalf("the proof must verify with the session key for its enrollment: %q %v", sub.EnrollmentID, err)
	}
	again := r.call(t, Request{Op: "register"})
	posts, _, _ = r.fake.snapshot()
	if !again.OK || again.RuntimeID != resp.RuntimeID || len(posts) != 1 {
		t.Fatalf("a second register from the same pid is the same session and no new enrollment: %+v posts=%d", again, len(posts))
	}
}

func TestSignOnlyForDescendantsOfARoot(t *testing.T) {
	r := startRig(t, "")
	child := sleeper(t)
	r.callerPID.Store(int64(child.Process.Pid))
	reg := r.call(t, Request{Op: "register", WaitSeconds: 5})
	if !reg.OK || reg.State != "enrolled" {
		t.Fatalf("register child: %+v", reg)
	}
	// This test process is the child's PARENT: not a descendant, no proof.
	r.callerPID.Store(int64(0))
	resp := r.call(t, Request{Op: "sign", Method: "GET", URL: "https://secrets.test/v1/enrollments/self"})
	if resp.OK || resp.Code != CodeNotASession {
		t.Fatalf("a non-descendant must get NOT_A_SESSION: %+v", resp)
	}
	// pid 1 is nobody's descendant either.
	r.callerPID.Store(int64(1))
	resp = r.call(t, Request{Op: "sign", Method: "GET", URL: "https://secrets.test/v1/enrollments/self"})
	if resp.OK || resp.Code != CodeNotASession {
		t.Fatalf("pid 1 must get NOT_A_SESSION: %+v", resp)
	}
	// A register from a process INSIDE the child's session (the child itself again) returns it.
	r.callerPID.Store(int64(child.Process.Pid))
	again := r.call(t, Request{Op: "register"})
	if !again.OK || again.RuntimeID != reg.RuntimeID {
		t.Fatalf("register inside a session returns that session: %+v", again)
	}
}

// TestSignRequestBuildsAValidRequestObject covers signRequest's direct output: the compact JWS
// it returns must verify with Plan A's own request-object verifier (record.VerifyRequestObject),
// carry the calling session's own key as iss (never some other value), embed exactly the
// requested agent_secret details and reason, and — since RequestTyp is disjoint from proof's own
// typ header — must NOT verify as a per-call proof (record_test.go's
// TestVerifyRequestObjectAcceptsItsOwnSignAndRefusesTheProofTyp proves the converse: a proof
// refused as a request object).
func TestSignRequestBuildsAValidRequestObject(t *testing.T) {
	r := startRig(t, "")
	reg := r.call(t, Request{Op: "register", WaitSeconds: 5})
	if !reg.OK || reg.State != "enrolled" {
		t.Fatalf("register: %+v", reg)
	}
	resp := r.call(t, Request{Op: "sign-request", Secrets: []string{"DEEL_API_KEY"}, Reason: "deel sync for AGENTC-1"})
	if !resp.OK || resp.RequestObject == "" {
		t.Fatalf("sign-request: %+v", resp)
	}
	ro, err := record.VerifyRequestObject(resp.RequestObject, r.srv.Broker.URL, time.Minute, time.Now())
	if err != nil {
		t.Fatalf("the request object must verify with record.VerifyRequestObject: %v", err)
	}
	sess := r.srv.Registry.Get(os.Getpid())
	if ro.Thumbprint != sess.Thumbprint {
		t.Fatalf("iss must be the calling session's own key thumbprint: got %q, want %q", ro.Thumbprint, sess.Thumbprint)
	}
	if len(ro.Details) != 1 || ro.Details[0].Type != "agent_secret" || ro.Details[0].Identifier != "DEEL_API_KEY" || ro.Reason != "deel sync for AGENTC-1" {
		t.Fatalf("claims: %+v", ro.Details)
	}
	v := &proof.Verifier{Skew: time.Minute}
	if _, err := v.Verify(context.Background(), resp.RequestObject, "POST", r.srv.Broker.URL+"/v1/requests", time.Now()); err == nil {
		t.Fatal("a request object must not verify as a per-call proof (typ agent-secrets-request+jwt must be refused)")
	}
}

// TestSignRequestOnlyForDescendantsOfARoot mirrors TestSignOnlyForDescendantsOfARoot: sign-request
// uses the exact same descendancy gate as sign (both now go through resolveDescendant), so a peer
// outside the session's process tree and a peer with no registered session at all must both be
// refused, never handed a signed request object.
func TestSignRequestOnlyForDescendantsOfARoot(t *testing.T) {
	r := startRig(t, "")
	child := sleeper(t)
	r.callerPID.Store(int64(child.Process.Pid))
	reg := r.call(t, Request{Op: "register", WaitSeconds: 5})
	if !reg.OK || reg.State != "enrolled" {
		t.Fatalf("register child: %+v", reg)
	}
	// This test process is the child's PARENT: not a descendant, no request object.
	r.callerPID.Store(int64(0))
	resp := r.call(t, Request{Op: "sign-request", Secrets: []string{"DEEL_API_KEY"}, Reason: "x"})
	if resp.OK || resp.Code != CodeNotASession {
		t.Fatalf("a non-descendant must get NOT_A_SESSION: %+v", resp)
	}
	// pid 1 is nobody's descendant either: unregistered, no session to sign for.
	r.callerPID.Store(int64(1))
	resp = r.call(t, Request{Op: "sign-request", Secrets: []string{"DEEL_API_KEY"}, Reason: "x"})
	if resp.OK || resp.Code != CodeNotASession {
		t.Fatalf("pid 1 must get NOT_A_SESSION: %+v", resp)
	}
}

func TestRegisterFromDescendantReturnsTheAncestorSession(t *testing.T) {
	r := startRig(t, "")
	root := r.call(t, Request{Op: "register", WaitSeconds: 5}) // this test process
	child := sleeper(t)
	r.callerPID.Store(int64(child.Process.Pid))
	got := r.call(t, Request{Op: "register"})
	if !got.OK || got.RuntimeID != root.RuntimeID || len(r.srv.Registry.List()) != 1 {
		t.Fatalf("a descendant registering must join the ancestor's session, not open its own: %+v", got)
	}
	signed := r.call(t, Request{Op: "sign", Method: "GET", URL: "https://x/v1/enrollments/self"})
	if !signed.OK || signed.EnrollmentID != root.EnrollmentID {
		t.Fatalf("the descendant signs as the root session: %+v", signed)
	}
}

func TestExitRevokesAndUnregisterRevokes(t *testing.T) {
	r := startRig(t, "")
	child := sleeper(t)
	r.callerPID.Store(int64(child.Process.Pid))
	reg := r.call(t, Request{Op: "register", WaitSeconds: 5})
	_ = child.Process.Kill()
	_ = child.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.fake.mu.Lock()
		n := len(r.fake.deletes)
		r.fake.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	_, deletes, _ := r.fake.snapshot()
	if len(deletes) != 1 || deletes[0] != reg.EnrollmentID {
		t.Fatalf("the exit must revoke the enrollment: %v", deletes)
	}
	if len(r.srv.Registry.List()) != 0 {
		t.Fatal("a dead session is forgotten")
	}
	second := sleeper(t)
	r.callerPID.Store(int64(second.Process.Pid))
	reg2 := r.call(t, Request{Op: "register", WaitSeconds: 5})
	un := r.call(t, Request{Op: "unregister"})
	_, deletes, _ = r.fake.snapshot()
	if !un.OK || len(deletes) != 2 || deletes[1] != reg2.EnrollmentID {
		t.Fatalf("unregister must revoke: %+v %v", un, deletes)
	}
	r.callerPID.Store(int64(0))
	list := r.call(t, Request{Op: "sessions"})
	if !list.OK || len(list.Sessions) != 0 {
		t.Fatalf("sessions after both ended: %+v", list)
	}
}

func TestEnrollRetriesUntilTheBrokerAnswers(t *testing.T) {
	r := startRig(t, "")
	r.fake.mu.Lock()
	r.fake.failFirst = 2
	r.fake.mu.Unlock()
	reg := r.call(t, Request{Op: "register"})
	if !reg.OK || reg.State != "enrolling" {
		t.Fatalf("register returns at once while enrolling: %+v", reg)
	}
	signed := r.call(t, Request{Op: "sign", Method: "GET", URL: "https://x/v1/enrollments/self"})
	if signed.OK || signed.Code != CodeNotEnrolled {
		t.Fatalf("before enrollment: %+v", signed)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && r.srv.Registry.Get(os.Getpid()).State() != "enrolled" {
		time.Sleep(100 * time.Millisecond)
	}
	if r.srv.Registry.Get(os.Getpid()).State() != "enrolled" {
		t.Fatal("two 503s then a 201 must end enrolled within 10 s (1 s + 2 s backoff)")
	}
}

func TestRenewRunsAndARefusedRenewReenrolls(t *testing.T) {
	r := startRig(t, "")
	// A 150 ms lease: the helper renews at a third of it (floored by MinRenew = 50 ms). Poll
	// for the first renew instead of guessing how long that takes under load — enrollment
	// alone has been observed to take longer than a fixed 300 ms wait under a starved
	// scheduler. Production leases are 900 s → one renew per 300 s.
	r.fake.mu.Lock()
	r.fake.lease = 150 * time.Millisecond
	r.fake.mu.Unlock()
	reg := r.call(t, Request{Op: "register", WaitSeconds: 5})
	renewDeadline := time.Now().Add(5 * time.Second)
	var renews int
	for time.Now().Before(renewDeadline) {
		r.fake.mu.Lock()
		renews = r.fake.renews
		r.fake.mu.Unlock()
		if renews > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if renews == 0 {
		t.Fatal("the helper renews the lease")
	}
	// The broker forgets the enrollment (lease lapsed during an outage): renew is 401, the
	// helper enrolls again and gets a new enrollment for the same runtime_id.
	r.fake.mu.Lock()
	delete(r.fake.enrolled, reg.EnrollmentID)
	delete(r.fake.byTP, r.srv.Registry.Get(os.Getpid()).Thumbprint)
	r.fake.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if id := r.srv.Registry.Get(os.Getpid()).EnrollmentID(); id != "" && id != reg.EnrollmentID {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("a refused renew must re-enroll")
}

func TestRecoverRepinsLiveSessionsWithFreshKeys(t *testing.T) {
	state := filepath.Join(t.TempDir(), "sessions.json")
	r := startRig(t, state)
	child := sleeper(t)
	r.callerPID.Store(int64(child.Process.Pid))
	first := r.call(t, Request{Op: "register", WaitSeconds: 5})
	oldSess := r.srv.Registry.Get(child.Process.Pid)
	oldTP := oldSess.Thumbprint
	// register()'s WaitSeconds only waits on sess.ready, which enrollLoop's setEnrolled closes
	// BEFORE the s.save() a few lines later in the same goroutine — so the call above returning
	// does not mean the enrolled record has actually reached disk yet. Wait for it explicitly:
	// srv2.Recover below reads only what's on disk, and quiescing the old session first (next)
	// removes it from the in-memory registry, so if s.save() runs after that Remove, it snapshots
	// an already-empty registry and overwrites the state file with nothing to recover.
	saveDeadline := time.Now().Add(5 * time.Second)
	for {
		recs, err := LoadRecords(state)
		if err != nil {
			t.Fatal(err)
		}
		saved := false
		for _, rec := range recs {
			if rec.PID == child.Process.Pid && rec.EnrollmentID == first.EnrollmentID {
				saved = true
				break
			}
		}
		if saved {
			break
		}
		if time.Now().After(saveDeadline) {
			t.Fatal("the enrolled record must be saved to disk before the old session is quiesced")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Quiesce the old session's watchExit before simulating the helper's death: r.cancel() only
	// stops enrollLoop/renewLoop (they select on ctx.Done()), never watchExit (which only ends
	// via Registry.Remove closing sess.stop, by design — see retire's doc comment). Left running,
	// it would still be pinning the same child pid when srv2 recovers below, and both would race
	// retire/Save against each other — and against this test's own t.TempDir() cleanup — once the
	// child process is killed during test teardown. Remove (not retire): retire would also
	// delete/revoke the enrollment this test is about to recover from disk.
	r.srv.Registry.Remove(oldSess)
	if oldSess.peer != nil {
		oldSess.peer.Close()
	}
	r.cancel() // the helper dies; keys are gone with it
	time.Sleep(100 * time.Millisecond)
	// A second helper on the same state and the same fake broker.
	of := operatorFile(t, "sjawhar")
	srv2 := &Server{Registry: NewRegistry(state), Broker: &Broker{URL: r.fake.srv.URL, OperatorFile: of, HTTP: r.fake.srv.Client()},
		Hostname: "testhost", PeerOf: PeerOf, Log: slog.Default(), MinRenew: time.Second}
	if _, err := srv2.Broker.Login(context.Background(), "testhost"); err != nil {
		t.Fatalf("srv2 login: %v", err)
	}
	waitFor(t, func() bool { return srv2.Broker.LoginStatus().State == "issued" })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv2.Recover(ctx)
	deadline := time.Now().Add(5 * time.Second)
	var sess *Session
	for time.Now().Before(deadline) {
		if sess = srv2.Registry.Get(child.Process.Pid); sess != nil && sess.State() == "enrolled" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if sess == nil || sess.State() != "enrolled" {
		t.Fatal("the live session must be re-pinned and enrolled")
	}
	if sess.Thumbprint == oldTP || sess.EnrollmentID() == first.EnrollmentID || sess.RuntimeID != first.RuntimeID {
		t.Fatalf("recovery uses a fresh key and a new enrollment for the same runtime_id: %+v", sess.Info())
	}
	// The recovered session's own goroutine revokes the old enrollment before its first enroll
	// call (AGENTC-834 thermonuclear review, Recover's ordering; enrollLoop revokes the session's
	// lapsed id first): by the time sess reads "enrolled" above, the old id must already be
	// revoked, so this poll should already find it on the first check.
	revokeDeadline := time.Now().Add(5 * time.Second)
	for {
		r.fake.mu.Lock()
		revoked := false
		for _, id := range r.fake.deletes {
			if id == first.EnrollmentID {
				revoked = true
				break
			}
		}
		deletes := append([]string(nil), r.fake.deletes...)
		r.fake.mu.Unlock()
		if revoked {
			break
		}
		if time.Now().After(revokeDeadline) {
			t.Fatalf("the old enrollment must be revoked: %v", deletes)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Quiesce srv2's own recovered session too, synchronously, before this test returns: its
	// watchExit is still pinning the same child pid and would otherwise still be live when
	// t.Cleanup kills the child and t.TempDir() removes this test's directories — an async
	// retire/Save racing the state directory's own removal, exactly like the old rig's session
	// above. All assertions are already made; nothing further needs srv2's registry.
	if sess != nil {
		srv2.Registry.Remove(sess)
		if sess.peer != nil {
			sess.peer.Close()
		}
	}
	// srv2 isn't fenced by startRig's own t.Cleanup (it's a second, manually-constructed
	// server); apply the same fence directly here. Registry.Save holds saveMu for its whole
	// WriteFile+Rename, so taking it here — and never releasing it — blocks any of srv2's own
	// Save calls (still possibly in flight or not yet started, e.g. an enrollLoop between
	// setEnrolled and its own save() a few lines later) until they finish, before this test
	// returns and t.TempDir() removes the state directory those writes go into.
	srv2.Registry.saveMu.Lock()
}

// TestRecoverRevokesThePriorEnrollmentBeforeReenrolling is the regression for the Important
// finding on Recover's ordering (AGENTC-834 thermonuclear review, second pass): against the
// real broker, Recover's old enrollment id must be gone before the re-pinned session's first
// enroll call, because the real broker's idempotent-enroll conflict is keyed on
// (launcher_credential_id, runtime_id), not thumbprint — a fresh key for a runtime that still
// has a live row is refused outright (fakeBroker's runtimeIDToID models exactly that; byTP
// alone, keyed on the recovered session's brand-new thumbprint, cannot). fakeBroker's
// revokeFirstDelay holds the old row "live" for a window so a concurrent (buggy) enroll would
// hit that conflict if one were ever attempted; the fixed enrollLoop revokes synchronously
// before its first Broker.Enroll call, so no enroll is ever attempted during that window and
// conflicts stays zero however long the delay runs.
func TestRecoverRevokesThePriorEnrollmentBeforeReenrolling(t *testing.T) {
	state := filepath.Join(t.TempDir(), "sessions.json")
	r := startRig(t, state)
	child := sleeper(t)
	r.callerPID.Store(int64(child.Process.Pid))
	first := r.call(t, Request{Op: "register", WaitSeconds: 5})
	// Wait for the enrolled record to actually reach disk before quiescing the old session —
	// see TestRecoverRepinsLiveSessionsWithFreshKeys's identical wait for why this is needed.
	saveDeadline := time.Now().Add(5 * time.Second)
	for {
		recs, err := LoadRecords(state)
		if err != nil {
			t.Fatal(err)
		}
		saved := false
		for _, rec := range recs {
			if rec.PID == child.Process.Pid && rec.EnrollmentID == first.EnrollmentID {
				saved = true
				break
			}
		}
		if saved {
			break
		}
		if time.Now().After(saveDeadline) {
			t.Fatal("the enrolled record must be saved to disk before the old session is quiesced")
		}
		time.Sleep(20 * time.Millisecond)
	}
	oldSess := r.srv.Registry.Get(child.Process.Pid)
	r.srv.Registry.Remove(oldSess)
	if oldSess.peer != nil {
		oldSess.peer.Close()
	}
	r.cancel() // the helper dies; keys are gone with it
	time.Sleep(100 * time.Millisecond)
	// Hold the old row "live" for 300ms once srv2.Recover asks to revoke it: nothing has issued
	// a DELETE for this broker yet, so this is that first (and only) one.
	r.fake.mu.Lock()
	r.fake.revokeFirstDelay = 300 * time.Millisecond
	r.fake.mu.Unlock()
	of := operatorFile(t, "sjawhar")
	srv2 := &Server{Registry: NewRegistry(state), Broker: &Broker{URL: r.fake.srv.URL, OperatorFile: of, HTTP: r.fake.srv.Client()},
		Hostname: "testhost", PeerOf: PeerOf, Log: slog.Default(), MinRenew: time.Second}
	if _, err := srv2.Broker.Login(context.Background(), "testhost"); err != nil {
		t.Fatalf("srv2 login: %v", err)
	}
	waitFor(t, func() bool { return srv2.Broker.LoginStatus().State == "issued" })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv2.Recover(ctx)
	deadline := time.Now().Add(5 * time.Second)
	var sess *Session
	for time.Now().Before(deadline) {
		if sess = srv2.Registry.Get(child.Process.Pid); sess != nil && sess.State() == "enrolled" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if sess == nil || sess.State() != "enrolled" {
		t.Fatal("the live session must be re-pinned and enrolled despite the delayed revoke")
	}
	r.fake.mu.Lock()
	conflicts := r.fake.conflicts
	deletes := append([]string(nil), r.fake.deletes...)
	r.fake.mu.Unlock()
	if conflicts != 0 {
		t.Fatalf("enrollLoop must revoke the prior enrollment before its first enroll call, never race it: %d runtime_id-keyed conflicts", conflicts)
	}
	revoked := false
	for _, id := range deletes {
		if id == first.EnrollmentID {
			revoked = true
			break
		}
	}
	if !revoked {
		t.Fatalf("the old enrollment must be revoked: %v", deletes)
	}
	// Quiesce srv2's own recovered session, synchronously, before this test returns — see
	// TestRecoverRepinsLiveSessionsWithFreshKeys's identical teardown for why this is needed.
	if sess != nil {
		srv2.Registry.Remove(sess)
		if sess.peer != nil {
			sess.peer.Close()
		}
	}
	srv2.Registry.saveMu.Lock()
}

// TestRecoverFallsBackToIndependentRevokeIfTheRepinnedSessionEndsMidBackoff is the regression for
// AGENTC-834: enrollLoop's revoke-before-enroll guard calls revokeLapsed synchronously, and
// revokeLapsed retries indefinitely with backoff — so if the re-pinned session ends
// (Registry.Remove, exactly what retire/unregister would do) while that revoke is genuinely stuck
// retrying against a failing broker, the prior id would be abandoned: retire's own revoke only
// ever touches sess.EnrollmentID(), still empty at this point since this session never reached its
// first successful Enroll, and once Registry.Remove drops the record a later restart's Recover has
// no way to find the prior id either. revokeLapsed therefore hands the id to the same bounded,
// independent s.revoke used elsewhere whenever it gives up because the session ended rather than
// because ctx was canceled. fakeBroker's revokeFailFirst holds the first revoke attempt at a 503
// (an unreachable broker, not merely a slow one) so revokeLapsed is genuinely retrying with
// backoff — its next attempt is roughly a second away — and revokeAttempts lets this test poll
// for exactly when that first attempt has landed instead of guessing with a raw sleep, so ending
// the session lands reliably inside the backoff window rather than racing it.
func TestRecoverFallsBackToIndependentRevokeIfTheRepinnedSessionEndsMidBackoff(t *testing.T) {
	state := filepath.Join(t.TempDir(), "sessions.json")
	r := startRig(t, state)
	child := sleeper(t)
	r.callerPID.Store(int64(child.Process.Pid))
	first := r.call(t, Request{Op: "register", WaitSeconds: 5})
	// Wait for the enrolled record to actually reach disk before quiescing the old session —
	// see TestRecoverRepinsLiveSessionsWithFreshKeys's identical wait for why this is needed.
	saveDeadline := time.Now().Add(5 * time.Second)
	for {
		recs, err := LoadRecords(state)
		if err != nil {
			t.Fatal(err)
		}
		saved := false
		for _, rec := range recs {
			if rec.PID == child.Process.Pid && rec.EnrollmentID == first.EnrollmentID {
				saved = true
				break
			}
		}
		if saved {
			break
		}
		if time.Now().After(saveDeadline) {
			t.Fatal("the enrolled record must be saved to disk before the old session is quiesced")
		}
		time.Sleep(20 * time.Millisecond)
	}
	oldSess := r.srv.Registry.Get(child.Process.Pid)
	r.srv.Registry.Remove(oldSess)
	if oldSess.peer != nil {
		oldSess.peer.Close()
	}
	r.cancel() // the helper dies; keys are gone with it
	time.Sleep(100 * time.Millisecond)
	// Fail only the first revoke of the old enrollment: revokeLapsed's own loop (or the
	// fallback's) succeeds on whichever attempt comes next, so the enrollment is never
	// permanently stuck — only genuinely retrying with backoff for one round.
	r.fake.mu.Lock()
	r.fake.revokeFailFirst = 1
	r.fake.mu.Unlock()
	of := operatorFile(t, "sjawhar")
	srv2 := &Server{Registry: NewRegistry(state), Broker: &Broker{URL: r.fake.srv.URL, OperatorFile: of, HTTP: r.fake.srv.Client()},
		Hostname: "testhost", PeerOf: PeerOf, Log: slog.Default(), MinRenew: time.Second}
	if _, err := srv2.Broker.Login(context.Background(), "testhost"); err != nil {
		t.Fatalf("srv2 login: %v", err)
	}
	waitFor(t, func() bool { return srv2.Broker.LoginStatus().State == "issued" })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv2.Recover(ctx)
	sess := srv2.Registry.Get(child.Process.Pid)
	if sess == nil {
		t.Fatal("the live process must be re-pinned")
	}
	// Wait for the first (failing) revoke attempt to land, then end the re-pinned session —
	// Registry.Remove, exactly what retire/unregister would do — while revokeLapsed is still
	// backed off waiting for its next attempt (roughly a second away).
	attemptDeadline := time.Now().Add(5 * time.Second)
	for {
		r.fake.mu.Lock()
		attempts := r.fake.revokeAttempts
		r.fake.mu.Unlock()
		if attempts >= 1 {
			break
		}
		if time.Now().After(attemptDeadline) {
			t.Fatal("the first revoke attempt never landed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if id := sess.EnrollmentID(); id != "" {
		t.Fatalf("the re-pinned session must not have reached its own first enroll yet: %q", id)
	}
	r.fake.mu.Lock()
	alreadyRevoked := false
	for _, id := range r.fake.deletes {
		if id == first.EnrollmentID {
			alreadyRevoked = true
			break
		}
	}
	r.fake.mu.Unlock()
	if alreadyRevoked {
		t.Fatal("the old enrollment was already revoked before the session ended; this run never reached the mid-backoff window the fix covers")
	}
	srv2.Registry.Remove(sess)
	if sess.peer != nil {
		sess.peer.Close()
	}
	// The independent fallback must still revoke the old enrollment even though the session that
	// was supposed to revoke it is gone.
	revokeDeadline := time.Now().Add(5 * time.Second)
	for {
		r.fake.mu.Lock()
		revoked := false
		for _, id := range r.fake.deletes {
			if id == first.EnrollmentID {
				revoked = true
				break
			}
		}
		deletes := append([]string(nil), r.fake.deletes...)
		r.fake.mu.Unlock()
		if revoked {
			break
		}
		if time.Now().After(revokeDeadline) {
			t.Fatalf("the old enrollment must be revoked by the fallback, not abandoned: %v", deletes)
		}
		time.Sleep(20 * time.Millisecond)
	}
	srv2.Registry.saveMu.Lock()
}

func TestRecoverDropsDeadRecords(t *testing.T) {
	state := filepath.Join(t.TempDir(), "sessions.json")
	dead := exec.Command("true")
	_ = dead.Run()
	reg := NewRegistry(state)
	sess, _ := newSession(dead.ProcessState.Pid(), 1, "testhost:dead", nil)
	sess.setEnrolled("enr-dead")
	reg.Add(sess)
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	r := startRig(t, state)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.fake.mu.Lock()
		n := len(r.fake.deletes)
		r.fake.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	_, deletes, _ := r.fake.snapshot()
	if len(deletes) != 1 || deletes[0] != "enr-dead" || len(r.srv.Registry.List()) != 0 {
		t.Fatalf("a recorded session whose process is gone is revoked and dropped: %v %d", deletes, len(r.srv.Registry.List()))
	}
}

// TestRenewRefusedRevokesTheLapsedEnrollmentBeforeReenrolling is the regression for the review's
// finding C2: the real broker's idempotent-enroll endpoint can answer a renew-refused ("your
// lease lapsed") enrollment id with itself forever, since a repeat enroll for the same
// thumbprint matches on the still-live row rather than minting a fresh one. The fake here models
// that faithfully — it keeps the enrollment row and its stale lease past expiry instead of
// deleting anything, refuses renew only because the lease has passed, and repeats the SAME id
// and SAME stale lease on the next enroll — so a fixed renewLoop must revoke the lapsed id
// before enrolling again, and this proves it does: within a bounded time the session ends up
// enrolled under a genuinely NEW id, having explicitly revoked the old one, never spinning.
func TestRenewRefusedRevokesTheLapsedEnrollmentBeforeReenrolling(t *testing.T) {
	r := startRig(t, "")
	r.fake.mu.Lock()
	r.fake.lease = 60 * time.Millisecond
	// Three failed renew attempts (150ms of retrying at the 50ms MinRenew floor) comfortably
	// outlasts the 60ms lease, so by the time renewFail hits zero the lease has genuinely
	// lapsed and the next renew is refused for that reason, not a transient outage.
	r.fake.renewFail = 3
	r.fake.mu.Unlock()
	reg := r.call(t, Request{Op: "register", WaitSeconds: 5})
	if !reg.OK || reg.State != "enrolled" {
		t.Fatalf("register: %+v", reg)
	}
	deadline := time.Now().Add(5 * time.Second)
	var newID string
	for time.Now().Before(deadline) {
		if id := r.srv.Registry.Get(os.Getpid()).EnrollmentID(); id != "" && id != reg.EnrollmentID {
			newID = id
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if newID == "" {
		t.Fatal("a renew refused because the lease lapsed must eventually revoke the old id and re-enroll with a new one, within a reasonable bounded time")
	}
	_, deletes, _ := r.fake.snapshot()
	revoked := false
	for _, id := range deletes {
		if id == reg.EnrollmentID {
			revoked = true
		}
	}
	if !revoked {
		t.Fatalf("the lapsed enrollment must be explicitly revoked before re-enrolling: deletes=%v old=%q", deletes, reg.EnrollmentID)
	}
	r.fake.mu.Lock()
	_, oldRowStillLive := r.fake.enrolled[reg.EnrollmentID]
	r.fake.mu.Unlock()
	if oldRowStillLive {
		t.Fatalf("the old enrollment row must be gone once it is revoked: %q", reg.EnrollmentID)
	}
}

// TestRegisterRejectsAPeerWhosePIDChangedDuringTheAncestryWalk is the regression for the
// review's finding I1 on register's "existing session" path: Registry.Root's ancestry walk
// resolves pid to a session while the peer's pidfd stays open, but the pid number itself can be
// reused by the kernel meanwhile (Peer's doc comment), so the walk's result must be trusted only
// once peer.PID() is re-read and still names pid. Calling register() directly with a peer pinned
// to a DIFFERENT live process than the pid argument reproduces exactly that mismatch without
// needing to win a real kernel race.
func TestRegisterRejectsAPeerWhosePIDChangedDuringTheAncestryWalk(t *testing.T) {
	r := startRig(t, "")
	root := r.call(t, Request{Op: "register", WaitSeconds: 5})
	if !root.OK {
		t.Fatalf("register root: %+v", root)
	}
	impostor := sleeper(t)
	peer, err := PinPID(impostor.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	resp := r.srv.register(context.Background(), peer, os.Getpid(), 0)
	if resp.OK || resp.Code != CodeUnidentified {
		t.Fatalf("a peer no longer matching the pid the ancestry walk resolved must be refused, not handed the root session: %+v", resp)
	}
}

// TestRegisterRejectsAPeerWhosePIDChangedBeforeAdopting covers the same finding on the "new
// session" path: a pid the ancestry walk finds unclaimed must not be adopted for a peer that no
// longer names it, and the registry must not keep a half-adopted entry for the pid either.
func TestRegisterRejectsAPeerWhosePIDChangedBeforeAdopting(t *testing.T) {
	r := startRig(t, "")
	victim := sleeper(t)
	impostor := sleeper(t)
	peer, err := PinPID(impostor.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	resp := r.srv.register(context.Background(), peer, victim.Process.Pid, 0)
	if resp.OK || resp.Code != CodeUnidentified {
		t.Fatalf("a peer no longer matching an unclaimed pid must be refused, not adopted: %+v", resp)
	}
	if r.srv.Registry.Get(victim.Process.Pid) != nil {
		t.Fatal("a refused mismatch must not leave a ghost session in the registry")
	}
}

// TestSignRejectsAPeerWhosePIDChangedDuringTheAncestryWalk is the regression for finding 1 (HIGH,
// AGENTC-834 thermonuclear review): sign() must keep the connecting peer's pidfd open through
// Registry.Root's ancestry walk and re-verify peer.PID() against pid before trusting a resolved
// session, exactly like register's "existing session" path — otherwise a pid the kernel reuses
// mid-walk could get a signed proof for another session's enrollment (full impersonation).
func TestSignRejectsAPeerWhosePIDChangedDuringTheAncestryWalk(t *testing.T) {
	r := startRig(t, "")
	root := r.call(t, Request{Op: "register", WaitSeconds: 5})
	if !root.OK || root.State != "enrolled" {
		t.Fatalf("register root: %+v", root)
	}
	impostor := sleeper(t)
	peer, err := PinPID(impostor.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	resp := r.srv.sign(peer, os.Getpid(), "GET", "https://secrets.test/v1/enrollments/self")
	if resp.OK || resp.Code != CodeUnidentified {
		t.Fatalf("a peer no longer matching the pid the ancestry walk resolved must be refused a proof, not signed as the root session: %+v", resp)
	}
}

// TestUnregisterRejectsAPeerWhosePIDChangedDuringTheAncestryWalk is the same regression for
// unregister(): a peer that no longer names the resolved pid must not be trusted to force-revoke
// that session's enrollment (denial of service).
func TestUnregisterRejectsAPeerWhosePIDChangedDuringTheAncestryWalk(t *testing.T) {
	r := startRig(t, "")
	root := r.call(t, Request{Op: "register", WaitSeconds: 5})
	if !root.OK {
		t.Fatalf("register root: %+v", root)
	}
	impostor := sleeper(t)
	peer, err := PinPID(impostor.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	resp := r.srv.unregister(context.Background(), peer, os.Getpid())
	if resp.OK || resp.Code != CodeUnidentified {
		t.Fatalf("a peer no longer matching the pid the ancestry walk resolved must be refused, not allowed to revoke that session: %+v", resp)
	}
	if r.srv.Registry.Get(os.Getpid()) == nil {
		t.Fatal("a refused unregister must not remove the real session")
	}
}

// delayingTransport delays a request whose path contains match, letting a test hold the fake's
// GET /v1/launcher-credentials/{pending} poll open long enough to observe a machine login sit
// "pending" — the fake otherwise answers "issued" on its very first poll, closing that window
// before a second socket round trip could ever land inside it.
type delayingTransport struct {
	http.RoundTripper
	delay time.Duration
	match string
}

func (d *delayingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.Contains(req.URL.Path, d.match) {
		time.Sleep(d.delay)
	}
	return d.RoundTripper.RoundTrip(req)
}

// TestLoginOverTheSocketReturnsTheCodeAndEnrollBoxWorksAfterIssue drives the socket protocol
// end to end for the new pass-through ops (AGENTC-393 Plan B, Task 2): enroll-box before any
// machine login names the login command (the same fail-closed error Broker.EnrollBox's
// launcherProof call produces); login returns a human-facing code and a repeat while pending
// returns the SAME code; once the fake issues it, login-status reports "issued" and enroll-box
// succeeds; unenroll-box is idempotent (a second call for the same id is still OK, matching
// Broker.Revoke's 204/404-both-succeed contract). newRig, not startRig: this test needs the
// rig's OWN pre-login window, which startRig's automatic login would already have closed.
func TestLoginOverTheSocketReturnsTheCodeAndEnrollBoxWorksAfterIssue(t *testing.T) {
	r := newRig(t, "")
	r.srv.Broker.HTTP = &http.Client{Transport: &delayingTransport{
		RoundTripper: r.fake.srv.Client().Transport,
		delay:        300 * time.Millisecond,
		match:        "/v1/launcher-credentials/",
	}}
	before := r.call(t, Request{Op: "enroll-box", RuntimeID: "box-1", Thumbprint: "tp-1"})
	if before.OK || before.Code != CodeEnrollFailed || !strings.Contains(before.Error, "agent-secrets launcher login") {
		t.Fatalf("enroll-box before any login must name the login command: %+v", before)
	}
	login1 := r.call(t, Request{Op: "login"})
	if !login1.OK || login1.Code == "" || login1.LoginState != "pending" {
		t.Fatalf("login: %+v", login1)
	}
	login2 := r.call(t, Request{Op: "login"})
	if !login2.OK || login2.Code != login1.Code || login2.LoginState != "pending" {
		t.Fatalf("a repeat login while pending must return the same code: %+v vs %+v", login1, login2)
	}
	waitFor(t, func() bool { return r.srv.Broker.LoginStatus().State == "issued" })
	status := r.call(t, Request{Op: "login-status"})
	if !status.OK || status.LoginState != "issued" {
		t.Fatalf("login-status: %+v", status)
	}
	enroll := r.call(t, Request{Op: "enroll-box", RuntimeID: "box-1", Thumbprint: "tp-1"})
	if !enroll.OK || enroll.EnrollmentID == "" || enroll.LeaseExpires == "" {
		t.Fatalf("enroll-box after issue: %+v", enroll)
	}
	un1 := r.call(t, Request{Op: "unenroll-box", EnrollmentID: enroll.EnrollmentID})
	if !un1.OK {
		t.Fatalf("unenroll-box: %+v", un1)
	}
	un2 := r.call(t, Request{Op: "unenroll-box", EnrollmentID: enroll.EnrollmentID})
	if !un2.OK {
		t.Fatalf("unenroll-box must be idempotent, like Revoke's 204/404-both-succeed contract: %+v", un2)
	}
}

// TestSignStillRequiresDescendancyButEnrollBoxDoesNot pins the stated boundary: enroll-box is a
// pass-through broker call available to ANY peer, while sign still requires the caller to be a
// descendant of a registered session root. A peer that is refused sign as NOT_A_SESSION must
// still be able to log in and enroll-box, proving the delta did not widen sign's own check.
// newRig, not startRig: register (WaitSeconds omitted) only needs the child to be a registered
// session for the descendancy check below, never for it to finish enrolling.
func TestSignStillRequiresDescendancyButEnrollBoxDoesNot(t *testing.T) {
	r := newRig(t, "")
	child := sleeper(t)
	r.callerPID.Store(int64(child.Process.Pid))
	reg := r.call(t, Request{Op: "register"})
	if !reg.OK {
		t.Fatalf("register child: %+v", reg)
	}
	// This test process is the child's PARENT: not a descendant, refused sign.
	r.callerPID.Store(int64(0))
	signed := r.call(t, Request{Op: "sign", Method: "GET", URL: "https://secrets.test/v1/enrollments/self"})
	if signed.OK || signed.Code != CodeNotASession {
		t.Fatalf("a non-descendant must still be refused sign: %+v", signed)
	}
	// The SAME non-descendant peer may enroll-box: it is not a registry-session op.
	login := r.call(t, Request{Op: "login"})
	if !login.OK {
		t.Fatalf("login: %+v", login)
	}
	waitFor(t, func() bool { return r.srv.Broker.LoginStatus().State == "issued" })
	enroll := r.call(t, Request{Op: "enroll-box", RuntimeID: "box-2", Thumbprint: "tp-2"})
	if !enroll.OK || enroll.EnrollmentID == "" {
		t.Fatalf("a non-descendant peer must still be able to enroll-box: %+v", enroll)
	}
}
