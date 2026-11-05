// packages/envoy/internal/broker/helper/enroll_session_held_test.go
//go:build linux

// What enrollLoop does with the one broker refusal that never comes free: 403 SESSION_HELD, the
// broker keeping this session's name for another principal (dispatch://INFRA-1167). Every other
// enroll failure is worth another try — a credential not installed yet, a broker that is down,
// a lapsed row still being revoked — and the loop retries each for as long as the session
// lives. This one is not: the holder is another person, another Google account or another
// service account, and nothing this helper does changes whose the name is. The loop must stop
// and leave the refusal on the session for the launcher to read.
package helper

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/proof"
)

func TestAnEnrollRefusedSessionHeldStopsRetrying(t *testing.T) {
	const heldName = "99999999-8888-4777-8666-555555555555"
	f := newFakeBroker(t)
	f.heldSessions = map[string]bool{heldName: true}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	srv := &Server{
		Registry: NewRegistry(filepath.Join(t.TempDir(), "sessions.json")),
		Broker: &Broker{URL: f.srv.URL, OperatorFile: operatorFile(t, "ada@example.com"),
			MachineFile: machineFilePath(t), HTTP: f.srv.Client(), Log: log},
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
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		// The retry's backoff is a second doubling to a minute, so a loop that treats this as
		// transient is still going here — and would be for as long as the session lives.
		t.Fatal("enrollLoop kept retrying 403 SESSION_HELD; a session name another principal holds never comes free")
	}

	// It asked exactly once. A second POST would mean the refusal was read as transient.
	posts, _, _ := f.snapshot()
	if len(posts) != 1 {
		t.Fatalf("the helper sent %d enrolls for a name another principal holds, want 1: %+v", len(posts), posts)
	}
	// And the session keeps the refusal: `sessions`, a register reply and every sign answer
	// report it, so the launcher learns why this session will never enrol.
	snap := sess.snapshot()
	if snap.State != "enrolling" || snap.EnrollmentID != "" {
		t.Fatalf("a refused session must hold no enrollment: %+v", snap)
	}
	if !strings.Contains(snap.LastError, "SESSION_HELD") || !strings.Contains(snap.LastError, heldName) {
		t.Fatalf("the session's last error is %q; want the broker's own refusal, naming the session", snap.LastError)
	}
}
