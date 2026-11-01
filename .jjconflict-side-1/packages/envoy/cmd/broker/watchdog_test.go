// packages/envoy/cmd/broker/watchdog_test.go
//
// The watchdog at the top of main decides who a second signal belongs to - the drain, or an exit
// that skips it - and secondSignalFindsServing is that whole decision, extracted so these rows
// drive it rather than a replica of it.
package main

import (
	"os"
	"syscall"
	"testing"
)

// secondSignalRepetitions is how many times the serving row asks. The shape this row exists to
// refuse - a select over serveStarted and the second signal - answers exit about half the time
// when both are ready, so it passes this many asks with probability 2^-500: never.
const secondSignalRepetitions = 500

// twoSignals answers a channel already holding the two signals the decision takes, as again
// holds them once an operator has sent SIGTERM twice.
func twoSignals() chan os.Signal {
	again := make(chan os.Signal, 2)
	again <- syscall.SIGTERM
	again <- syscall.SIGTERM
	return again
}

// A second signal reaching a broker that is already serving belongs to the drain, every time: an
// exit there would skip the drain and srv.Shutdown, cut the in-flight requests, and log that the
// broker had served nothing. Both inputs are ready before the decision starts - serveStarted
// closed and both signals buffered - which is exactly the moment a select would choose at random.
func TestASecondSignalAtAServingBrokerAlwaysDrains(t *testing.T) {
	serveStarted := make(chan struct{})
	close(serveStarted)
	exits := 0
	for range secondSignalRepetitions {
		if !secondSignalFindsServing(twoSignals(), serveStarted) {
			exits++
		}
	}
	if exits != 0 {
		t.Fatalf("a second signal at a serving broker answered exit %d times in %d; want drain every time", exits, secondSignalRepetitions)
	}
}

// The control: a second signal before serveStarted closes belongs to the watchdog's exit, since
// there is no server to drain.
func TestASecondSignalBeforeServingExits(t *testing.T) {
	if secondSignalFindsServing(twoSignals(), make(chan struct{})) {
		t.Fatal("a second signal before the broker was serving answered drain; want exit")
	}
}
