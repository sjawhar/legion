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
// than the lease it granted, as the instant the enrollment ends. The outage begins at the first
// renewal, the harshest instant a gap can begin at, since a gap that began later would let that
// renewal through and start the lease again.
type outageRig struct {
	mu        sync.Mutex
	now       time.Duration
	firstWait time.Duration
	gap       time.Duration
	timeout   time.Duration
	lease     time.Duration
	attempts  []time.Duration
	renewed   time.Duration
	renewedOK bool
}

func (r *outageRig) gapStart() time.Duration { return r.firstWait }
func (r *outageRig) gapEnd() time.Duration   { return r.firstWait + r.gap }

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
	r.attempts = append(r.attempts, at)
	switch {
	case at < r.gapEnd():
		// No broker serves. A connection the outage refuses fails at once; one it black-holes
		// holds the renewer to its whole client timeout, which only costs it anything when that
		// lands past the instant a broker serves again.
		if at+r.timeout > r.gapEnd() {
			r.now = at + r.timeout
			return nil, &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
		}
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	case at >= r.expires():
		// A broker serves again, and its first sweep ended this enrollment: the gap was short
		// enough to count as a broker serving, so none of it was given back.
		return r.notLive(), nil
	default:
		r.renewed, r.renewedOK = at, true
		return r.granted(), nil
	}
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
// is the wait a third of the lease asks for, which is where the outage begins; the one after a
// renewal that got through ends the row, since the loop would otherwise renew forever.
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

// TestRenewLoopOutlivesAnOutageOnlyAtTheLeaseBound drives the real renewLoop through an outage
// of leasetime.NotServingSweeps sweeps, beginning at the worst instant for it, at the two
// leases either side of its own boundary. At the largest lease the model refuses, the renewal
// that gets through comes after the enrollment ended and the renewer exits 76, the exit that
// told INFRA-487's pods their session was gone; one second more and it renews. The walk it
// takes is asserted instant by instant against the model config.Load refuses by, so the two can
// never say different things about the same renewer.
func TestRenewLoopOutlivesAnOutageOnlyAtTheLeaseBound(t *testing.T) {
	const sweep = 5 * time.Second
	model := leasetime.Production()
	floor := model.ShortestFor(leasetime.RenewerAgentSecrets, sweep)
	for _, c := range []struct {
		what     string
		lease    time.Duration
		wantExit int
	}{
		{"the largest lease that cannot outlive the outage", floor - time.Second, exitRenewRefused},
		{"one second more", floor, 0},
	} {
		t.Run(c.what, func(t *testing.T) {
			boundary := model.WorstFor(leasetime.RenewerAgentSecrets, c.lease, sweep)
			if boundary.GapStart != boundary.Attempts[0] {
				t.Fatalf("the model's worst gap for a lease of %v begins at %v, not at the first renewal (%v); this row drives the first-renewal phase", c.lease, boundary.GapStart, boundary.Attempts[0])
			}
			if boundary.Survived() != (c.wantExit == 0) {
				t.Fatalf("the model says a lease of %v renews at %v and survives %v; this row expects exit %d", c.lease, boundary.Renewed, boundary.Survived(), c.wantExit)
			}
			rig := &outageRig{gap: leasetime.NotServingSweeps * sweep, timeout: leasetime.RenewClientTimeout, lease: c.lease}
			client, signer, enrollmentID, _ := renewalSession(t, "http://broker.invalid")
			if client.http.Timeout != rig.timeout {
				t.Fatalf("the renewer's client times out after %v, but the lease bound counts %v", client.http.Timeout, rig.timeout)
			}
			client.http.Transport = rig
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if got := renewLoop(ctx, client, signer, enrollmentID, boxRenewal, io.Discard, rig.after(cancel, t)); got != c.wantExit {
				t.Fatalf("renew loop exit = %d, want %d", got, c.wantExit)
			}
			// The renewer reads its lease a few milliseconds after the broker granted it —
			// signing the proof and the round trip are inside that reading — so its own walk
			// is the model's at the lease it read, three times the wait it kept. The boundary
			// above is two thirds of a second wide, far wider than that reading costs, so
			// which side of it this lease falls on is the same either way.
			if short := c.lease/leasetime.RenewDivisor - rig.firstWait; short < 0 || short > 100*time.Millisecond {
				t.Fatalf("the renewer waited %v after a lease of %v, want a third of it", rig.firstWait, c.lease)
			}
			walked := model.Walk(leasetime.RenewerAgentSecrets, leasetime.RenewDivisor*rig.firstWait, rig.gap, rig.firstWait)
			if !slices.Equal(rig.attempts, walked.Attempts) {
				t.Errorf("the renewer launched renewals at %v, the model walked %v", rig.attempts, walked.Attempts)
			}
			if rig.renewedOK != walked.Survived() || (walked.Survived() && rig.renewed != walked.Renewed) {
				t.Errorf("the renewal that got through was at %v (any: %v), the model's at %v (survived: %v)", rig.renewed, rig.renewedOK, walked.Renewed, walked.Survived())
			}
		})
	}
}
