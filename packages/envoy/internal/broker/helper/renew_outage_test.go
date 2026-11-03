// packages/envoy/internal/broker/helper/renew_outage_test.go
//go:build linux

package helper

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/leasetime"
)

// outageUnit is what one second of BROKER_LEASE_SECONDS and BROKER_SWEEP_SECONDS lasts in the
// rows below. renewLoop has no clock of its own to inject — it waits on time.After and reads
// time.Until(lease) — so the row runs the whole outage at a shortened second instead, with
// every wait of the model shortened by the same factor (leasetime.Model.Scale). The helper's
// floor becomes 300 ms and each margin asserted below 400 ms, which is far past the lateness a
// loaded machine adds to a timer, and a whole row still takes under two seconds. It was 2 ms
// once, and at that unit one run of this row in a hundred, taken while every other Postgres
// suite of this module ran beside it, launched one renewal more than the model walked.
const outageUnit = 10 * time.Millisecond

// outageBroker is a broker that stops serving between gapStart and gapEnd, counted from the
// renewal that granted the lease. A connection it refuses fails at once; one the outage
// black-holes holds the renewer for the whole client timeout, which is the cost the lease bound
// counts. Once a broker serves again it answers either a new lease or, if the enrollment's own
// lease ran out while nothing served, ENROLLMENT_NOT_LIVE — the answer the broker's first sweep
// leaves behind, since a gap this short counts as a broker serving and is given back to nobody.
type outageBroker struct {
	start            time.Time
	gapStart, gapEnd time.Duration
	timeout          time.Duration
	lease            time.Duration

	mu        sync.Mutex
	attempts  []time.Duration
	renewedOK bool
}

// renewed answers whether a renewal has got through, for a row to wait on.
func (b *outageBroker) renewed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.renewedOK
}

func (b *outageBroker) RoundTrip(*http.Request) (*http.Response, error) {
	at := time.Since(b.start)
	b.mu.Lock()
	b.attempts = append(b.attempts, at)
	b.mu.Unlock()
	switch {
	case at >= b.gapStart && at < b.gapEnd:
		if at+b.timeout > b.gapEnd {
			time.Sleep(b.timeout)
			return nil, &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
		}
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	case at >= b.lease:
		return reply(http.StatusUnauthorized, map[string]string{"code": "ENROLLMENT_NOT_LIVE", "error": "enrollment is not live"}), nil
	default:
		b.mu.Lock()
		b.renewedOK = true
		b.mu.Unlock()
		return reply(http.StatusOK, map[string]any{"lease_expires_at": time.Now().Add(b.lease)}), nil
	}
}

func reply(status int, body any) *http.Response {
	raw, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(raw))),
	}
}

// TestRenewLoopOutlivesAnOutageOnlyAboveTheLeaseBound drives the real renewLoop through an
// outage of leasetime.NotServingSweeps sweeps, beginning at the worst instant for it, at a
// lease the model says it cannot outlive and one it says it can. At the first the renewal that
// gets through finds the enrollment gone and the loop answers "enroll again"; at the second it
// renews and goes on holding the session. Both run at outageUnit to the second, and both leases
// are far enough from the boundary that no scheduler's lateness can move the verdict.
//
// In a pod this row runs -run-filtered: the package's contract rows need SO_PEERPIDFD, which
// gVisor does not answer, and fail PEER_UNIDENTIFIED. CI's envoy-go job runs the whole package.
func TestRenewLoopOutlivesAnOutageOnlyAboveTheLeaseBound(t *testing.T) {
	model := leasetime.Production().Scale(outageUnit)
	sweep := 5 * outageUnit
	// minMargin is how far from the boundary each lease below has to be: the row decides the
	// same way the model does only while the machine's lateness stays well inside it.
	const minMargin = 40 * outageUnit
	for _, c := range []struct {
		what      string
		lease     time.Duration
		wantLapse bool
	}{
		{"a lease the outage runs out", 45 * outageUnit, true},
		{"a lease that outlives it", 180 * outageUnit, false},
	} {
		t.Run(c.what, func(t *testing.T) {
			want := model.WorstFor(leasetime.RenewerHelper, c.lease, sweep)
			if want.Survived() == c.wantLapse {
				t.Fatalf("the model renews a lease of %v at %v, which survives %v; this row wants a lapse: %v", c.lease, want.Renewed, want.Survived(), c.wantLapse)
			}
			if margin := want.Lease - want.Renewed; margin < minMargin && -margin < minMargin {
				t.Fatalf("a lease of %v renews %v from its end, closer to the boundary than the %v this row needs to decide in real time", c.lease, margin, minMargin)
			}
			sess, err := newSession(os.Getpid(), 0, "runtime-1", nil)
			if err != nil {
				t.Fatal(err)
			}
			sess.setEnrolled("enrollment-1")
			start := time.Now()
			broker := &outageBroker{start: start, gapStart: want.GapStart, gapEnd: want.GapEnd, timeout: model.ClientTimeout, lease: c.lease}
			srv := &Server{
				Broker:   &Broker{URL: "http://broker.invalid", HTTP: &http.Client{Transport: broker}},
				Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
				MinRenew: model.HelperMinRenew,
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			lapsed := make(chan bool, 1)
			go func() { lapsed <- srv.renewLoop(ctx, sess, start.Add(c.lease)) }()
			if c.wantLapse {
				select {
				case got := <-lapsed:
					if !got {
						t.Fatal("the renew loop gave up on the session instead of enrolling it again")
					}
				case <-time.After(renewOutageBound):
					t.Fatalf("the renew loop neither lapsed nor renewed within %v", renewOutageBound)
				}
			} else {
				waitFor(t, broker.renewed)
				cancel()
				if got := <-lapsed; got {
					t.Fatal("the renew loop lapsed the session, want it renewed and held")
				}
			}
			broker.mu.Lock()
			defer broker.mu.Unlock()
			if len(broker.attempts) != len(want.Attempts) {
				t.Errorf("the renewer launched %d renewals, at %v; the model walked %d, at %v", len(broker.attempts), broker.attempts, len(want.Attempts), want.Attempts)
			}
			if broker.renewedOK == c.wantLapse {
				t.Errorf("a renewal got through: %v; the model's lease ran out first: %v", broker.renewedOK, c.wantLapse)
			}
		})
	}
}

// renewOutageBound is how long a row waits for a loop that should have ended long before: the
// longest walk below renews at 120 units, and this is the same five seconds waitFor keeps.
const renewOutageBound = 5 * time.Second
