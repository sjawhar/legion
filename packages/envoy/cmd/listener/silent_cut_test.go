package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// silentCutBound is how long after the cut each of the listener's clients has to report it.
// bus.WithSilentCutDetection pings once a second and declares the connection stale on the interval
// that would put a third ping out, which is two to three seconds, and the rest of the bound is
// room for a loaded machine.
const silentCutBound = 5 * time.Second

// disconnectedUnderName reports whether output holds internal/bus's dropped-connection line for
// the client named name. The listener leaves internal/bus on the default logger, which writes the
// name as a `name=envoy` field, so the field is matched whole: "envoy" is a prefix of
// "envoy-live-topics".
func disconnectedUnderName(output, name string) bool {
	for line := range strings.SplitSeq(output, "\n") {
		if strings.Contains(line, "envoy nats disconnected") && slices.Contains(strings.Fields(line), "name="+name) {
			return true
		}
	}
	return false
}

// Both NATS clients of a listener declare a connection lost within seconds of a hub that stops
// answering it: the main client, which owns the stream (listenerStreamOptions), and the live
// topics lane's own client (openLiveTopics). A hub whose packets stop closes nothing, so on
// nats.go's default pings both clients would read connected for four to six minutes into the cut.
//
// The test runs the listener binary and freezes the hub under it, so each client is the one
// production builds. A test that connected with bus.WithSilentCutDetection itself would report the
// cut within seconds whether or not those two call sites carry the option, and would prove
// nothing.
func TestBothOfTheListenersNATSClientsReportASilentCutWithinSeconds(t *testing.T) {
	server, uri := testnats.Start(t)
	// The live topics lane opens only under ENVOY_SUBSCRIBE_ALL_TOPICS (liveLaneEnv), and it opens
	// before the listener reports itself healthy, so waitHealthy means both clients have connected.
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), uri, "silent-cut", liveLaneEnv...)
	listener.waitHealthy(t)

	// Only what the listener logs from the freeze is read, so a disconnect line from earlier in the
	// run could not stand in for one the cut caused.
	logged := len(listener.Output.String())
	thaw := server.Freeze(t)
	// Cleanups run last-in first-out, so this thaw precedes the server's shutdown.
	t.Cleanup(thaw)
	cut := time.Now()

	names := []string{"envoy", "envoy-live-topics"}
	reported := make(map[string]time.Duration, len(names))
	for len(reported) < len(names) {
		output := listener.Output.String()[logged:]
		for _, name := range names {
			if _, done := reported[name]; !done && disconnectedUnderName(output, name) {
				reported[name] = time.Since(cut)
			}
		}
		if len(reported) == len(names) {
			break
		}
		if time.Since(cut) > silentCutBound {
			var missing []string
			for _, name := range names {
				if _, done := reported[name]; !done {
					missing = append(missing, name)
				}
			}
			t.Fatalf("no disconnect line for %s %s into a silent cut:\n%s",
				strings.Join(missing, " and "), time.Since(cut).Round(time.Millisecond), output)
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, name := range names {
		t.Logf("%s reported the cut %s after it began", name, reported[name].Round(time.Millisecond))
	}
}
