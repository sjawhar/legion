package natsauth_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/legion/daemon/internal/natsauth"
)

// Unreachable's table: every shape the audit found, plus the boundary cases (an authorization or
// permission violation, the two standard library types that satisfy net.Error without being
// network failures at all, and the shapes this package's own callers never actually produce but a
// future one might).
func TestUnreachableClassifiesEachErrorShape(t *testing.T) {
	deadline, cancel := context.WithTimeout(context.Background(), 0)
	cancel()
	<-deadline.Done()

	for _, testCase := range []struct {
		name string
		err  error
		want bool
	}{
		{"nats.ErrNoServers, connection refused reduces to this", nats.ErrNoServers, true},
		{"a dial the network refused",
			&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, true},
		{"a dial that timed out",
			&net.OpError{Op: "dial", Net: "tcp", Err: errTimeout{}}, true},
		{"a host that does not resolve, net.Dial's own *net.OpError wrapping a DNS failure",
			&net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "nats.invalid", IsNotFound: true}}, true},
		{"a NATS permission violation — a refused grant", nats.ErrPermissionViolation, false},
		{"a NATS authorization violation — a misconfigured nkey user", nats.ErrAuthorization, false},
		{"a bare context.DeadlineExceeded: refused by design, since an innocent slow JetStream call " +
			"and a silently refused one produce the same shape and this package cannot tell them apart",
			deadline.Err(), false},
		{"a *url.Error satisfying net.Error only through its wrapped Timeout()/Temporary(), never matched by type",
			&url.Error{Op: "Get", URL: "https://dispatch.invalid", Err: errTimeout{}}, false},
		{"a bare EOF during the handshake: ambiguous, not evidenced, exits by default", io.EOF, false},
		{"no responders for a JetStream request: ambiguous (disabled vs. still starting), exits by default",
			errors.New("nats: no responders available for request"), false},
		{"a plain error with no recognized shape at all", errors.New("boom"), false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := natsauth.Unreachable(testCase.err); got != testCase.want {
				t.Fatalf("Unreachable(%v) = %t, want %t", testCase.err, got, testCase.want)
			}
		})
	}
}

// errTimeout is a net.Error that times out, used only to build an *net.OpError/*url.Error of the
// right shape; it is never classified by the net.Error interface itself (that is exactly the bug
// Unreachable's typed matching avoids).
type errTimeout struct{}

func (errTimeout) Error() string   { return "i/o timeout" }
func (errTimeout) Timeout() bool   { return true }
func (errTimeout) Temporary() bool { return true }

var _ net.Error = errTimeout{}

// A connection already closed answers WithLastError with its own last asynchronous error, not the
// err it is given: a refused JetStream API call (a publish or subscribe grant the connection's
// user lacks) is reported to nats.go asynchronously and never closes the connection on its own, so
// workflow.connect's own Close, then WithLastError, is what makes the refusal visible — the join
// survives the close, and the %w wrapping lets errors.Is reach nats.ErrPermissionViolation through
// it. This reproduces the mechanism with a raw protocol fake (no Docker, no real nats-server): a
// real permission-violation -ERR line, which nats.go parses into conn.LastError() exactly as it
// would from a live server's own refusal.
func TestWithLastErrorNamesAPermissionViolationOnceTheConnectionIsClosed(t *testing.T) {
	refused := make(chan struct{}, 1)
	url := fakeServer(t, "-ERR 'Permissions Violation for Publish to \"$JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS\"'\r\n")
	conn, err := natsauth.Connect([]string{url}, "", nats.Timeout(5*time.Second),
		natsauth.LogEvents(slog.New(slog.NewTextHandler(io.Discard, nil))),
		signalled(refused, make(chan struct{}, 1), make(chan struct{}, 1), make(chan struct{}, 1)))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	select {
	case <-refused:
	case <-time.After(5 * time.Second):
		t.Fatal("the server's permission violation was never processed")
	}

	// Exactly what intake.OpenConsumers returns when the blocked API call's own deadline fires:
	// no sign of the permission violation in its own text, until the connection is closed and
	// WithLastError is asked.
	deadline, cancel := context.WithTimeout(context.Background(), 0)
	cancel()
	<-deadline.Done()
	blocked := deadline.Err()

	conn.Close()
	named := natsauth.WithLastError(blocked, conn)
	if !errors.Is(named, nats.ErrPermissionViolation) {
		t.Fatalf("WithLastError(%v, conn) = %v; want it to wrap nats.ErrPermissionViolation once conn is closed", blocked, named)
	}
	if natsauth.Unreachable(named) {
		t.Fatalf("Unreachable(%v) = true; a permission violation must refuse the boot loud, not wait forever", named)
	}
}

// natsauth.Connect against a closed port is the real shape episode 2's 70 crashes share (a dial
// tcp …: i/o timeout, or whatever the OS answers first) — the one assertion this package owes the
// boot gate; the retry loop that rides it out lives in bootprobe and is tested there
// (bootprobe.TestRunWithoutABoundWaitsOutEveryTransientFailure).
func TestConnectAgainstAClosedPortReturnsAnErrorUnreachableAccepts(t *testing.T) {
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := reserved.Addr().String()
	reserved.Close() // nothing answers here.

	_, err = natsauth.Connect([]string{"nats://" + addr}, "", nats.Timeout(500*time.Millisecond))
	if err == nil {
		t.Fatal("Connect against a closed port must fail")
	}
	if !natsauth.Unreachable(err) {
		t.Fatalf("Unreachable(%v) = false; a closed port is exactly the shape the boot gate must wait out", err)
	}
}
