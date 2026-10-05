package natsauth_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/natsauth"
)

// RED (LEGION-580, maintainability and correctness review): the boot gate's predicate must match
// concrete, named shapes, never the net.Error interface — two standard library types satisfy it
// without being network failures at all.
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
		{"nats.ErrTimeout, a plain NATS request/reply that got no answer", nats.ErrTimeout, true},
		{"a dial the network refused, exactly episode 1's shape",
			&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, true},
		{"a dial that timed out, exactly the dial tcp …: i/o timeout crash text",
			&net.OpError{Op: "dial", Net: "tcp", Err: errTimeout{}}, true},
		{"a host that does not resolve, net.Dial's own *net.OpError wrapping a DNS failure",
			&net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "nats.invalid", IsNotFound: true}}, true},
		{"a NATS permission violation — a refused grant, joined from LastError", nats.ErrPermissionViolation, false},
		{"a NATS authorization violation — a misconfigured nkey user", nats.ErrAuthorization, false},
		{"context.DeadlineExceeded alone, with no joined LastError: the exact false positive the review reproduced",
			deadline.Err(), false},
		{"a *url.Error, the exact false positive a Dispatch-shaped error would have produced under the old shared classifier",
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
// right shape; it is never classified by the net.Error interface itself (that is exactly the bug).
type errTimeout struct{}

func (errTimeout) Error() string   { return "i/o timeout" }
func (errTimeout) Timeout() bool   { return true }
func (errTimeout) Temporary() bool { return true }

var _ net.Error = errTimeout{}

// RED (correctness review, blocking finding 1): a refused JetStream API call (a publish or
// subscribe grant the connection's user lacks) is reported to nats.go asynchronously and never
// closes the connection, so the synchronous error a blocked API call returns is only ever its own
// deadline, with nothing in its own text to tell the refusal from NATS merely being slow.
// JoinLastError is what makes the refusal visible in time for Unreachable to refuse it loud. This
// reproduces the mechanism with a raw protocol fake (no Docker, no real nats-server): a real
// permission violation -ERR line, which nats.go parses into conn.LastError() exactly as it would
// from a live server's own refusal.
func TestJoinLastErrorMakesAPermissionViolationVisibleToUnreachable(t *testing.T) {
	refused := make(chan struct{}, 1)
	url := fakeServer(t, "-ERR 'Permissions Violation for Publish to \"$JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS\"'\r\n")
	conn, err := natsauth.Connect([]string{url}, "", nats.Timeout(5*time.Second),
		natsauth.LogEvents(slog.New(slog.NewTextHandler(io.Discard, nil))),
		signalled(refused, make(chan struct{}, 1), make(chan struct{}, 1), make(chan struct{}, 1)))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()
	select {
	case <-refused:
	case <-time.After(5 * time.Second):
		t.Fatal("the server's permission violation was never processed")
	}

	// Exactly what intake.OpenConsumers returns when the blocked API call's own deadline fires:
	// no sign of the permission violation in its own text.
	deadline, cancel := context.WithTimeout(context.Background(), 0)
	cancel()
	<-deadline.Done()
	blocked := fmt.Errorf("open ENVOY_NOTIFICATIONS: %w", deadline.Err())

	// A bare deadline with no join already classifies false (TestUnreachableClassifiesEachErrorShape);
	// what matters here is that the join reveals the real cause.
	joined := natsauth.JoinLastError(blocked, conn)
	if !errors.Is(joined, nats.ErrPermissionViolation) {
		t.Fatalf("JoinLastError(%v, conn) = %v; want it to wrap nats.ErrPermissionViolation", blocked, joined)
	}
	if natsauth.Unreachable(joined) {
		t.Fatalf("Unreachable(%v) = true after the join; a permission violation must refuse the boot loud, not wait forever", joined)
	}
}

// RED (LEGION-580): before this fix, a boot against a NATS address nothing answered on exited at
// once ("legion start: connect Envoy NATS: dial tcp …: i/o timeout"), the first of the 71
// identical crashes the 2026-10-01 06:33–06:52 episode shows (70 of them this exact shape, the
// 71st the prior "workflow intake stopped" crash). Here natsauth.Connect itself — the real boot
// dial — is retried under bootprobe.Run and natsauth.Unreachable against an address a bare TCP
// listener refuses until a real (if minimal) NATS protocol responder opens there late.
func TestBootprobeRunRetriesANATSAddressThatRefusesConnectionsUntilItsServerOpensLate(t *testing.T) {
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := reserved.Addr().String()
	reserved.Close() // nothing answers here until the fake server below opens.

	opened := make(chan struct{})
	go func() {
		time.Sleep(30 * time.Millisecond) // several refused dials happen first.
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			t.Errorf("open the late NATS listener on %s: %v", addr, err)
			return
		}
		close(opened)
		defer listener.Close()
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		io.WriteString(conn, `INFO {"server_id":"fake","version":"2.10.0","proto":1,"max_payload":1048576}`+"\r\n")
		lines := bufio.NewReader(conn)
		for {
			line, err := lines.ReadString('\n')
			if err != nil {
				return
			}
			if strings.HasPrefix(line, "PING") {
				break
			}
		}
		io.WriteString(conn, "PONG\r\n")
		io.Copy(io.Discard, conn)
	}()

	var attempts atomic.Int64
	retry := bootprobe.Retry{Initial: 2 * time.Millisecond, Max: 8 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runErr := bootprobe.Run(ctx, "NATS connect", retry, slog.New(slog.NewTextHandler(io.Discard, nil)), func(ctx context.Context) bootprobe.Outcome {
		attempts.Add(1)
		conn, err := natsauth.Connect([]string{"nats://" + addr}, "", nats.Timeout(200*time.Millisecond))
		switch {
		case err == nil:
			conn.Close()
			return bootprobe.Outcome{Passed: true}
		case natsauth.Unreachable(err):
			return bootprobe.Outcome{Detail: err.Error()}
		default:
			return bootprobe.Outcome{Refusal: err}
		}
	})
	if runErr != nil {
		t.Fatalf("bootprobe.Run returned %v; a late-opening NATS listener must eventually satisfy it, not refuse", runErr)
	}
	select {
	case <-opened:
	default:
		t.Fatal("bootprobe.Run passed before the listener ever opened, so it proves nothing about retrying")
	}
	if n := attempts.Load(); n < 2 {
		t.Fatalf("attempts = %d, want at least 2: the gate must have retried the refused dial, not succeeded on the first try", n)
	}
}
