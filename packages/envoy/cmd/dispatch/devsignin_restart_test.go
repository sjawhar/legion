package main

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A dev sign-in server signs its cookies with a key it mints for its own process
// (sessionSigningKey), so a restart carries a key set the process before it never held. Killed
// (SIGKILL, as Playwright stops packages/dispatch/e2e/run-server.sh's server), that process never
// deregisters, and its registry row counts as live until its heartbeat lapses. A server started
// on the same database at once must still come up: its registration waits for the dead row to
// lapse (processes.Register) rather than refusing beside a process that no longer runs.
func TestADevSignInServerRestartedAfterAKillStarts(t *testing.T) {
	database := storetest.Open(t)
	options := dispatchOptions{Env: map[string]string{
		"DISPATCH_DEV_SIGNIN":              "1",
		"DISPATCH_IDENTITY":                "cookie",
		"DISPATCH_IDENTITY_HEADER_TRUSTED": "",
		"DISPATCH_INSECURE_COOKIE":         "1",
		// The dev sign-in fence takes a loopback dashboard origin only; /healthz, all this test
		// asks, is served whatever Host a request names.
		"DISPATCH_SERVER_URL": "http://127.0.0.1",
	}}
	first := startDispatchProcessWith(t, database.Pool.Config().ConnString(), options)
	first.waitHealthy(t)
	if err := first.Cmd.Process.Kill(); err != nil {
		t.Fatalf("kill the first server: %v", err)
	}
	first.WaitExit(t, "SIGKILL")

	second := startDispatchProcessWith(t, database.Pool.Config().ConnString(), options)
	second.waitHealthy(t)
}
