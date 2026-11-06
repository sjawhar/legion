package supervise

import (
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// sessionStops is a harness whose Envoy clean stops are recorded rather than sent, over a claim
// walked to ready: its registration is the session the stop must name.
func sessionStops(t *testing.T) (*harness, *[]string) {
	t.Helper()
	h := newBareHarness(t)
	stopped := &[]string{}
	h.deps.SessionGone = func(session string) { *stopped = append(*stopped, session) }
	c := queuedClaim()
	if err := h.store.PutClaim(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	h.start(c)
	return h, stopped
}

// A closed stream says nothing about the process that opened it: a daemon restart closes every
// one of them while the agents run on, and stopping those sessions would unseat live roles. So the
// clean stop of a claim's Envoy session follows the probe's verdict, never the close, and it
// carries the session the claim registered.
func TestTheSessionStopFollowsTheProbeNotTheClosedStream(t *testing.T) {
	for _, tc := range []struct {
		name  string
		probe runtime.ObservationKind
		want  []string
	}{
		{"the probe finds the process gone", runtime.Gone, []string{session}},
		{"the probe finds the process alive", runtime.Alive, nil},
		{"the probe cannot tell", runtime.Uncertain, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, stopped := sessionStops(t)
			h.reach(StateReady)
			h.rt.ScriptProbe(fake.ProbeResult{Kind: tc.probe})

			h.must(StreamClosed{Claim: testToken})

			if !slices.Equal(*stopped, tc.want) {
				t.Fatalf("stopped sessions %v, want %v", *stopped, tc.want)
			}
		})
	}
}

// The other runtime reaches the same judgement without a stream at all: under Kubernetes the
// launcher reports its child's exit (shimwire.LauncherState.LastExit), which the sandbox runtime
// observes as gone, so that claim's session is stopped too.
func TestALauncherReportedExitStopsTheSession(t *testing.T) {
	h, stopped := sessionStops(t)
	h.reach(StateReady)

	h.observe(runtime.Gone)

	if !slices.Equal(*stopped, []string{session}) {
		t.Fatalf("stopped sessions %v, want the claim's own session once", *stopped)
	}
}

// A claim whose agent never registered holds no session, so its death stops nothing: there is no
// registration in the listener to end.
func TestADeathBeforeAnyRegistrationStopsNoSession(t *testing.T) {
	h, stopped := sessionStops(t)
	h.launch()
	h.connect()
	h.rt.ScriptProbe(fake.ProbeResult{Kind: runtime.Gone})

	h.must(StreamClosed{Claim: testToken})

	if len(*stopped) != 0 {
		t.Fatalf("stopped sessions %v, want none for a claim that registered none", *stopped)
	}
}
