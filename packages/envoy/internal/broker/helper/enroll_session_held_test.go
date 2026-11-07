// packages/envoy/internal/broker/helper/enroll_session_held_test.go
//go:build linux

// What enrollLoop does with 403 SESSION_HELD, the broker keeping this session's name for
// another principal (dispatch://INFRA-1167).
//
// The refusal is of the principal THIS attempt enrolled under, and not of the name for ever:
// the helper chooses that principal again at every attempt (Broker.signingAuthority), so the
// person who holds the name signing in on this box makes the next attempt theirs, and that one
// enrols and adopts what their session left on the box it came from — the carry this whole
// change exists for. Nothing else starts an enrollment of a session that is already live, so a
// loop that gave up here would leave the session signing nothing until its process restarted.
//
// So the loop keeps asking, on a ladder of its own up to heldRetryCap rather than the minute
// every other enroll failure retries on; it keeps the refusal on the session as its last error,
// which `sessions`, a register reply and every sign answer report; and it enrols the moment the
// principal changes.
package helper

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/brokerclient"
	"github.com/sjawhar/envoy/internal/testwait"
)

// holderSignIns is the box's sign-ins, and with them the fake broker's rule about the name.
// Current answers no sign-in until signIn, and that machine's own credential is what the
// helper enrols under; from signIn on it answers the holder's own credential and frees the
// name in the fake at the same moment, because the fake refuses a held name to whatever
// authority asks (heldSessions is keyed on the name alone) while the real broker refuses it to
// every principal but the one holding it. So the first attempt made under the holder's
// credential is the one the broker accepts, and every attempt before it is refused.
type holderSignIns struct {
	mu        sync.Mutex
	authority *brokerclient.Authority
	free      func()
}

func (h *holderSignIns) Current(context.Context) (*brokerclient.Authority, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.authority != nil && h.free != nil {
		h.free()
		h.free = nil
	}
	return h.authority, nil
}

func (h *holderSignIns) Refused(string) {}

func (h *holderSignIns) signIn(authority *brokerclient.Authority) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.authority = authority
}

// waitUpTo polls cond every 10 ms for as long as d, and fails with why when it never holds. It
// is waitFor with a deadline of the test's own, because the attempt that enrols here waits out
// the held ladder first and that outlasts waitFor's five seconds.
func waitUpTo(t *testing.T, d time.Duration, cond func() bool, why string) {
	t.Helper()
	if !testwait.Holds(d, 10*time.Millisecond, cond) {
		t.Fatal(why)
	}
}

func TestAnEnrollRefusedSessionHeldBacksOffAndEnrolsWhenThePrincipalChanges(t *testing.T) {
	const heldName = "99999999-8888-4777-8666-555555555555"
	f := newFakeBroker(t)
	f.heldSessions = map[string]bool{heldName: true}
	// The log goes to a buffer, because how OFTEN the refusal is written is part of what
	// this measures.
	var out syncBuffer
	log := slog.New(slog.NewJSONHandler(&out, nil))
	// The machine's login was approved by ada, so an enrolment under it is ada's; the name is
	// grace's, and grace is not signed in on this box yet.
	signIns := &holderSignIns{free: func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.heldSessions, heldName)
	}}
	srv := &Server{
		Registry: NewRegistry(filepath.Join(t.TempDir(), "sessions.json")),
		Broker: &Broker{URL: f.srv.URL, OperatorFile: operatorFile(t, "ada@example.com"),
			MachineFile: machineFilePath(t), HTTP: f.srv.Client(), Log: log, SignIns: signIns},
		Hostname: "testhost", PeerOf: PeerOf, Log: log, MinRenew: time.Second,
	}
	t.Cleanup(srv.Broker.Close)
	if _, err := srv.Broker.Login(context.Background(), "testhost"); err != nil {
		t.Fatalf("login: %v", err)
	}
	waitFor(t, func() bool { return srv.Broker.LoginStatus().State == "issued" })

	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	// No peer: this drives the enrollment loop alone, which needs the session's identity and
	// nothing of the process behind it.
	sess, err := newSession(os.Getpid(), 1, SessionRuntimeID(heldName), heldName, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.Registry.Add(sess)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		srv.enrollLoop(ctx, sess)
		close(done)
	}()

	// It asks again: a loop that read the refusal as the name's final answer would send one
	// POST and return, and this session would sign nothing ever again.
	posts := func() int {
		sent, _, _ := f.snapshot()
		return len(sent)
	}
	waitUpTo(t, 10*time.Second, func() bool { return posts() > 1 },
		"the helper sent one enroll for a name another principal holds and stopped; the refusal is of the principal that attempt enrolled under, which a sign-in changes with no restart")
	select {
	case <-done:
		t.Fatal("enrollLoop returned while the session was refused SESSION_HELD; nothing would start it again for a live session")
	default:
	}

	// And it backs off rather than spinning: the ladder is 1 s doubling, so a couple of
	// seconds hold a handful of attempts, where a loop with no wait at all would hold
	// thousands.
	before := posts()
	time.Sleep(2500 * time.Millisecond)
	if spun := posts() - before; spun > 3 {
		t.Fatalf("the helper sent %d enrolls in 2.5 s for a held name, want the backoff's handful", spun)
	}

	// The session keeps the refusal meanwhile: `sessions`, a register reply and every sign
	// answer report it, so the launcher learns why this session is not signing yet.
	snap := sess.snapshot()
	if snap.State != "enrolling" || snap.EnrollmentID != "" {
		t.Fatalf("a refused session must hold no enrollment: %+v", snap)
	}
	if !strings.Contains(snap.LastError, "SESSION_HELD") || !strings.Contains(snap.LastError, heldName) {
		t.Fatalf("the session's last error is %q; want the broker's own refusal, naming the session", snap.LastError)
	}

	// grace, who holds the name, signs in on this box, with a box key of her own as a real
	// sign-in has. The helper chooses the credential again at the next attempt, so that
	// attempt is grace's and the broker takes it.
	graceKey, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	signIns.signIn(&brokerclient.Authority{Key: graceKey, CredentialID: "credential-of-graces-sign-in", Operator: "grace@example.com"})
	waitUpTo(t, 60*time.Second, func() bool { return sess.snapshot().EnrollmentID != "" },
		"the session never enrolled after the person holding its name signed in on this box")

	sent, _, _ := f.snapshot()
	if got := sent[0]["operator"]; got != "ada@example.com" {
		t.Errorf("the refused enrolment stated operator %v, want this machine's login's approver", got)
	}
	if got := sent[len(sent)-1]["operator"]; got != "grace@example.com" {
		t.Errorf("the enrolment that was accepted stated operator %v, want the person who signed in: the principal must be chosen afresh at every attempt, never kept from the first", got)
	}

	// One line for the whole stretch, not one per attempt: a name held for hours must not
	// fill the journal. The line is found by the field it carries rather than by its
	// wording, so rewording it costs this test nothing; it is written when the principal the
	// attempt enrolled under changes, nothing changed it until grace signed in, and that
	// attempt enrolled rather than being refused — so one line is all of them.
	lines := 0
	for _, rec := range logRecords(t, &out) {
		if rec["held_by_other"] != true {
			continue
		}
		lines++
		if got := rec["enrolling_as"]; got != "the person ada@example.com" {
			t.Errorf("the line says the session enrolled as %v, want the principal of the credential THAT attempt enrolled under, carried on the refusal itself", got)
		}
	}
	if lines != 1 {
		t.Errorf("the helper wrote %d lines about the held name under one principal, want one: %d enrolments were refused", lines, len(sent)-1)
	}
}
