package main

import (
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
// name as a `name=...` field, and each client's name carries the pod's instance after a space,
// which slog's text handler quotes, so the field is matched whole: "envoy-listener <instance>" is
// a prefix of "envoy-listener-lease <instance>", the delivery lease's own connection.
func disconnectedUnderName(output, name string) bool {
	for line := range strings.SplitSeq(output, "\n") {
		if strings.Contains(line, "envoy nats disconnected") && strings.Contains(line, ` name="`+name+`"`) {
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
	const instance = "silent-cut-pod"
	server, uri := testnats.Start(t)
	// The live topics lane opens only under ENVOY_SUBSCRIBE_ALL_TOPICS (liveLaneEnv), and it opens
	// before the listener reports itself healthy, so waitHealthy means both clients have connected.
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), uri, "silent-cut",
		append([]string{"ENVOY_INSTANCE_ID=" + instance}, liveLaneEnv...)...)
	listener.waitHealthy(t)

	freezeUntilBothClientsReportTheCut(t, server, listener, instance)
}

// freezeUntilBothClientsReportTheCut freezes the hub under a healthy listener running as instance
// and returns once each of the listener's two clients has logged the dropped connection, within
// silentCutBound. It fails the test naming every client whose line is missing, with what the
// listener logged. It returns the thaw, which the caller runs itself when it needs the traffic
// back, and a reader of everything the listener logged from the freeze on: a line from the
// listener's start, or from before the cut, could otherwise stand in for one the cut caused.
func freezeUntilBothClientsReportTheCut(t *testing.T, server *testnats.Server, listener *listenerProcess, instance string) (thaw func(), output func() string) {
	t.Helper()
	logged := len(listener.Output.String())
	output = func() string { return listener.Output.String()[logged:] }
	thaw = server.Freeze(t)
	// Cleanups run last-in first-out, so this thaw precedes the server's shutdown. Thawing twice
	// is harmless, so a caller may run it itself.
	t.Cleanup(thaw)
	cut := time.Now()

	names := []string{"envoy-listener " + instance, liveTopicsClientName + " " + instance}
	reported := make(map[string]time.Duration, len(names))
	for {
		for _, name := range names {
			if _, done := reported[name]; !done && disconnectedUnderName(output(), name) {
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
				strings.Join(missing, " and "), time.Since(cut).Round(time.Millisecond), output())
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, name := range names {
		t.Logf("%s reported the cut %s after it began", name, reported[name].Round(time.Millisecond))
	}
	return thaw, output
}
