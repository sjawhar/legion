package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// silentCutBound is how long after the cut the listener's client has to report it.
// bus.WithSilentCutDetection pings once a second and declares the connection stale on the interval
// that would put a third ping out, which is two to three seconds, and the rest of the bound is
// room for a loaded machine.
const silentCutBound = 5 * time.Second

// disconnectedUnderName reports whether output holds internal/bus's dropped-connection line for
// the client named name. The listener leaves internal/bus on the default logger, which writes the
// name as a `name=envoy` field, so the field is matched whole rather than by prefix.
func disconnectedUnderName(output, name string) bool {
	for line := range strings.SplitSeq(output, "\n") {
		if strings.Contains(line, "envoy nats disconnected") && slices.Contains(strings.Fields(line), "name="+name) {
			return true
		}
	}
	return false
}

// The listener's stream client declares a connection lost within seconds of a hub that stops
// answering it (listenerStreamOptions). A hub whose packets stop closes nothing, so on nats.go's
// default pings that client would read connected for four to six minutes into the cut.
//
// The test runs the listener binary and freezes the hub under it, so the client is the one
// production builds. A test that connected with bus.WithSilentCutDetection itself would report
// the cut within seconds whether or not that call site carries the option, and would prove
// nothing.
func TestTheListenersNATSClientReportsASilentCutWithinSeconds(t *testing.T) {
	server, uri := testnats.Start(t)
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), uri, "silent-cut")
	listener.waitHealthy(t)

	freezeUntilTheClientReportsTheCut(t, server, listener)
}

// freezeUntilTheClientReportsTheCut freezes the hub under a healthy listener and returns once the
// listener's stream client has logged the dropped connection, within silentCutBound. It fails the
// test with what the listener logged. It returns the thaw, which the caller runs itself when it
// needs the traffic back, and a reader of everything the listener logged from the freeze on: a
// line from the listener's start, or from before the cut, could otherwise stand in for one the
// cut caused.
func freezeUntilTheClientReportsTheCut(t *testing.T, server *testnats.Server, listener *listenerProcess) (thaw func(), output func() string) {
	t.Helper()
	logged := len(listener.Output.String())
	output = func() string { return listener.Output.String()[logged:] }
	thaw = server.Freeze(t)
	// Cleanups run last-in first-out, so this thaw precedes the server's shutdown. Thawing twice
	// is harmless, so a caller may run it itself.
	t.Cleanup(thaw)
	cut := time.Now()

	for !disconnectedUnderName(output(), "envoy") {
		if time.Since(cut) > silentCutBound {
			t.Fatalf("no disconnect line for the listener's client %s into a silent cut:\n%s",
				time.Since(cut).Round(time.Millisecond), output())
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("the listener's client reported the cut %s after it began", time.Since(cut).Round(time.Millisecond))
	return thaw, output
}
