package main

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// bootProbeDialDelay is how late slowPostgres lets each of the process's connections reach
// Postgres in the row below. It is half again store.healthProbeTimeout's two seconds, so the
// boot's own cold dial cannot fit inside that warm bound however the machine is loaded, and the
// boot pays it three times over: measured through a counting proxy, exactly one connection (the
// shared pool's, which carries every sequential boot step) completes before the probe's own dial,
// the probe dials the second and processes.Register the third, and the whole boot to a 200 from
// /healthz took 11.2 s at this delay. publishDashboard, the one step bounded tightly enough to
// care (dashboardPublishTimeout, 60 s, and it exits 1), opens no connection of its own and took
// 22 and 36 ms in those runs, so a delay charged from the first connection never reaches it.
const bootProbeDialDelay = 3 * time.Second

// bootProbeDeadline is the failure CI showed when the boot's probe ran out of the warm probe's
// bound - "register this server process: probe the database: context deadline exceeded"
// (dispatch://INFRA-985, 1 run in 13 at a Postgres with max_connections 400 and 4 in 16 at 100)
// - written as the binary writes it, which is slog's message and the error it wraps as two
// fields rather than one joined string.
const bootProbeDeadline = `msg="dispatch: register this server process" ` +
	`error="probe the database: context deadline exceeded"`

// The boot's one probe of the database outlasts the bound /healthz's probe reads within. Why that
// read is the one that is always cold, and why the warm bound is no bound for it, is on
// store.Pool.HealthyWithin; the boot's own bound is bootProbeTimeout. Here every connection
// reaches Postgres bootProbeDialDelay late, which is past the warm bound, and the process must
// still register and serve.
//
// What holds the row is healthyWithin: the process serves and answers /healthz 200, which comes
// after joinRegistry, so a boot that died at the probe - or at any step before it, every one of
// which dials through the same slow proxy - never reaches it. The registration line is read after
// that for the log it names, so a reader of a later failure sees which step the boot got past.
func TestTheBootProbeOutlastsTheWarmProbesBound(t *testing.T) {
	migrated := storetest.Open(t)
	dsn, err := url.Parse(migrated.Pool.Config().ConnString())
	if err != nil {
		t.Fatalf("parse the test database URL: %v", err)
	}
	dsn.Host = slowPostgres(t, dsn.Host, bootProbeDialDelay)

	process := startDispatchProcessWith(t, dsn.String(), dispatchOptions{})
	// waitHealthy's own 30 s is a budget this boot has no reason to meet: it pays
	// bootProbeDialDelay on each of the three connections it opens one after another, and every
	// other step it takes is as slow as the machine is. So the row waits on a hang guard instead,
	// far from the 11 s the boot took here, which fails only a process that never serves at all.
	if err := process.healthyWithin(2 * time.Minute); err != nil {
		// A probe that ran out of its bound exits the process with 1 before it serves anything, so
		// this is where that failure arrives: name it, rather than leaving the reader to find the
		// line in the output.
		if strings.Contains(process.Output.String(), bootProbeDeadline) {
			t.Fatalf("the boot probe ran out of its bound at a dial of %s: %v", bootProbeDialDelay, err)
		}
		t.Fatal(err)
	}
	process.WaitForOutput(t, "dispatch: registered this server process")
}

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
