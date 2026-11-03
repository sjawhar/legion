package main

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A process refused by a live peer waits, before it serves anything, for the peer's heartbeat to
// lapse (processes.Register), up to a live window and a heartbeat. A deploy that stops it in that
// wait stops it cleanly: SIGTERM ends the wait, the process logs the stop at INFO and exits 0,
// and no supervisor reads the signal it sent as a failed start. The peer here is a running
// process with a key set of its own, so its row keeps heartbeating and the wait does not end on
// its own before the signal.
func TestAProcessStoppedWhileWaitingForAdmissionExitsCleanly(t *testing.T) {
	database := storetest.Open(t)
	peer := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{SigningKey: "the-live-peers-key"})
	peer.waitHealthy(t)

	waiting := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{SigningKey: "a-key-the-peer-does-not-share"})
	waiting.WaitForOutput(t, "a live server process refuses this one's signing keys; waiting for its heartbeat to lapse")
	waiting.Terminate(t)
	waiting.WaitExit(t, "SIGTERM")

	output := waiting.Output.String()
	if code := waiting.Cmd.ProcessState.ExitCode(); code != 0 {
		t.Fatalf("a process stopped while waiting for admission exited %d, want 0:\n%s", code, output)
	}
	if !strings.Contains(output, `level=INFO msg="dispatch: stopped while waiting for admission to the registry of live server processes"`) {
		t.Fatalf("a process stopped while waiting for admission did not log the stop at INFO:\n%s", output)
	}
}
