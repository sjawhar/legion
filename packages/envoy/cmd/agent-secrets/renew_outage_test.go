package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/leasetime"
)

// outageRig is a broker that stops serving for a while, on the renewer's own clock: renewLoop's
// injected wait advances that clock by exactly the wait it asked for and returns at once, and
// every answer this transport gives is decided by where the renewal falls on it. Nothing here
// sleeps, so the whole outage runs in microseconds of real time.
//
// The clock's zero is the renewal that granted the lease. The renewer reads its lease from that
// answer, so the lease it is working to is three times its own first wait — the microseconds
// the answer took to reach it are already inside that reading — and the rig takes that, rather
// than the lease it granted, as the instant the enrollment ends.
//
// The outage is the worst one the model found, and this rig reproduces the whole of it: it
// begins gapOffset from the renewer's first renewal (which is at or before that renewal, since
// a gap that began later would let it through and start the lease again), and fails each
// renewal it refuses after the delay delays names for it, from at once to the whole client
// timeout. That pattern is the half of the worst case a gap start alone does not carry: a
// renewal that fails part of the way through its timeout puts the next one just before the
// broker returns, where holding that one costs the whole timeout.
type outageRig struct {
	mu        sync.Mutex
	now       time.Duration
	firstWait time.Duration
	gapOffset time.Duration
	gap       time.Duration
	delays    []time.Duration
	timeout   time.Duration
	lease     time.Duration
	attempts  []leasetime.Attempt
	renewed   time.Duration
	renewedOK bool
}

func (r *outageRig) gapStart() time.Duration { return r.firstWait + r.gapOffset }
func (r *outageRig) gapEnd() time.Duration   { return r.gapStart() + r.gap }

// expires is when the enrollment ends on the renewer's clock: the lease the renewer itself read
// out of the answer that granted it, which is three times the wait it then kept.
func (r *outageRig) expires() time.Duration { return leasetime.RenewDivisor * r.firstWait }

func (r *outageRig) RoundTrip(*http.Request) (*http.Response, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	at := r.now
	if r.firstWait == 0 {
		// The renewal that grants the lease, before the outage and before any wait.
		return r.granted(), nil
	}
	i := len(r.attempts)
	r.attempts = append(r.attempts, leasetime.Attempt{At: at, Failed: at})
	if at >= r.gapStart() && at < r.gapEnd() {
		// No broker serves, and this renewal takes the delay the model's worst outage gave it
		// to fail: nothing when the connection is refused, the whole client timeout when the
		// outage black-holes it, and anything between when a proxy gives up on it or a
		// terminating pod resets it.
		var delay time.Duration
		if i < len(r.delays) {
			delay = min(max(r.delays[i], 0), r.timeout)
		}
		r.attempts[i].Failed = at + delay
		r.now = at + delay
		if delay >= r.timeout {
			return nil, &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
		}
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	}
	// A broker serves. The renewal is the one the model's walk ends on either way, so it failed
	// at no instant; whether the broker renews it or answers ENROLLMENT_NOT_LIVE is what the
	// lease, rather than the outage, decides.
	r.attempts[i].Failed = 0
	if at >= r.expires() {
		// Its first sweep ended this enrollment: the gap was short enough to count as a broker
		// serving, so none of it was given back.
		return r.notLive(), nil
	}
	r.renewed, r.renewedOK = at, true
	return r.granted(), nil
}

func (r *outageRig) granted() *http.Response {
	return jsonResponse(http.StatusOK, map[string]any{"lease_expires_at": time.Now().Add(r.lease)})
}

func (r *outageRig) notLive() *http.Response {
	return jsonResponse(http.StatusUnauthorized, map[string]string{"code": "ENROLLMENT_NOT_LIVE", "error": "enrollment is not live"})
}

func jsonResponse(status int, body any) *http.Response {
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

// after is renewLoop's wait: it advances the renewer's clock and returns at once. The first one
// is the wait a third of the lease asks for, which the outage is placed against; the one after
// a renewal that got through ends the row, since the loop would otherwise renew forever.
func (r *outageRig) after(cancel context.CancelFunc, t *testing.T) func(time.Duration) <-chan time.Time {
	return func(d time.Duration) <-chan time.Time {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.firstWait == 0 {
			r.firstWait = d
		}
		r.now += d
		if r.renewedOK || len(r.attempts) > 64 {
			if !r.renewedOK {
				t.Errorf("the renewer launched %d renewals without one getting through", len(r.attempts))
			}
			cancel()
			return make(chan time.Time)
		}
		ready := make(chan time.Time, 1)
		ready <- time.Now()
		return ready
	}
}

// TestRenewLoopOutlivesAnOutageOnlyAtTheLeaseBound drives the real renewLoop through the worst
// outage of leasetime.NotServingSweeps sweeps the model found for this renewer, at one lease
// below its floor and one above it, each far enough from the boundary that a real-time loop
// cannot carry it across (minMargin). Below the floor, the renewal that gets through comes after
// the enrollment ended and the renewer exits 76, the exit that told INFRA-487's pods their
// session was gone; above it, it renews. The walk it
// takes is asserted instant by instant against the model config.Load refuses by, so the two can
// never say different things about the same renewer.
func TestRenewLoopOutlivesAnOutageOnlyAtTheLeaseBound(t *testing.T) {
	const sweep = 10 * time.Second
	model := leasetime.Production()
	floor := model.ShortestFor(leasetime.RenewerAgentSecrets, sweep)
	// A lease exactly at the floor renews one instant inside it, which no rig that spends
	// milliseconds signing a proof can tell from a lapse. Each lease below is far enough from
	// the floor that the renewer's own clock cannot carry it across, and minMargin checks that
	// against the model rather than assuming it.
	const minMargin = time.Second
	for _, c := range []struct {
		what     string
		lease    time.Duration
		wantExit int
	}{
		{"a lease the outage runs out", floor - 5*time.Second, exitRenewRefused},
		{"a lease well above the floor", floor + 15*time.Second, 0},
	} {
		t.Run(c.what, func(t *testing.T) {
			boundary := model.WorstFor(leasetime.RenewerAgentSecrets, c.lease, sweep)
			if boundary.Survived() != (c.wantExit == 0) {
				t.Fatalf("the model says a lease of %v renews at %v and survives %v; this row expects exit %d", c.lease, boundary.Renewed, boundary.Survived(), c.wantExit)
			}
			if margin := boundary.Lease - boundary.Renewed; margin < minMargin && -margin < minMargin {
				t.Fatalf("a lease of %v renews %v from its end, closer to the boundary than the %v this row needs to decide in real time", c.lease, margin, minMargin)
			}
			delays := make([]time.Duration, 0, len(boundary.Attempts))
			partial := false
			for _, a := range boundary.Attempts {
				if !a.Refused() {
					continue
				}
				delays = append(delays, a.Delay())
				partial = partial || (a.Delay() > 0 && a.Delay() < leasetime.RenewClientTimeout)
			}
			if !partial {
				t.Fatalf("the model's worst outage for a lease of %v fails every renewal at one end of its timeout; this row exists to drive one that fails part of the way through", c.lease)
			}
			rig := &outageRig{
				gapOffset: boundary.GapStart - boundary.Attempts[0].At,
				gap:       leasetime.NotServingSweeps * sweep,
				delays:    delays,
				timeout:   leasetime.RenewClientTimeout,
				lease:     c.lease,
			}
			client, signer, enrollmentID, _ := renewalSession(t, "http://broker.invalid")
			if client.http.Timeout != rig.timeout {
				t.Fatalf("the renewer's client times out after %v, but the lease bound counts %v", client.http.Timeout, rig.timeout)
			}
			client.http.Transport = rig
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if got := renewLoop(ctx, client, signer, enrollmentID, boxRenewal, io.Discard, rig.after(cancel, t), leasetime.RenewBackoffMax); got != c.wantExit {
				t.Fatalf("renew loop exit = %d, want %d", got, c.wantExit)
			}
			// The renewer reads its lease a few milliseconds after the broker granted it —
			// signing the proof and the round trip are inside that reading — so its own walk
			// is the model's at the lease it read, three times the wait it kept. Each lease
			// above is at least minMargin from the boundary, far more than that reading costs,
			// so which side of it this lease falls on is the same either way.
			if short := leasetime.RenewAfter(c.lease) - rig.firstWait; short < 0 || short > 100*time.Millisecond {
				t.Fatalf("the renewer waited %v after a lease of %v, want a third of it", rig.firstWait, c.lease)
			}
			walked := model.Walk(leasetime.RenewerAgentSecrets, leasetime.RenewDivisor*rig.firstWait, rig.gap, rig.gapStart(), delays...)
			if !slices.Equal(rig.attempts, walked.Attempts) {
				t.Errorf("the renewer launched renewals at %v, the model walked %v", rig.attempts, walked.Attempts)
			}
			if rig.renewedOK != walked.Survived() || (walked.Survived() && rig.renewed != walked.Renewed) {
				t.Errorf("the renewal that got through was at %v (any: %v), the model's at %v (survived: %v)", rig.renewed, rig.renewedOK, walked.Renewed, walked.Survived())
			}
		})
	}
}
