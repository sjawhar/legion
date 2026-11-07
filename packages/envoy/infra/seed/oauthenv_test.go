package seed

import (
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/fixtureenv"
)

// startupOAuthToken is what the process held of GOOGLE_OAUTH_ACCESS_TOKEN when the test binary
// started: its value, whether it held the variable at all, so an empty value is told from none
// (os.LookupEnv), and whether the snapshot was taken at all, so that a snapshot never taken is
// told from one of a process that held nothing — which is CI's own case, since no workflow sets
// the variable and the fake supplies it per test. runTrustingTheFake records it before m.Run.
var startupOAuthToken struct {
	value string
	set   bool
	taken bool
}

// snapshotStartupOAuthToken records what the process holds of GOOGLE_OAUTH_ACCESS_TOKEN, which
// the Secret Manager store reads as an operator's access token.
func snapshotStartupOAuthToken() {
	startupOAuthToken.value, startupOAuthToken.set = os.LookupEnv("GOOGLE_OAUTH_ACCESS_TOKEN")
	startupOAuthToken.taken = true
}

// unsetOAuthToken takes GOOGLE_OAUTH_ACCESS_TOKEN out of this binary's own environment for the
// rest of t, as fixtureenv.Unset does, and asserts in a cleanup registered before that call that
// it is back once t ends. Cleanups run last registered first, so this one runs after the restore
// t.Setenv registers inside Unset: the test that takes the variable away is the test that proves
// it comes back, it proves it whatever order the runner picks the package's tests in, and it
// fails where the fault is. The whole-binary reader below stays as the backstop for a future
// test that leaves the variable changed and calls nothing here. As there, no failure prints the
// value or any part of one: it is an operator's access token.
func unsetOAuthToken(t *testing.T) {
	t.Helper()
	want, wanted := os.LookupEnv("GOOGLE_OAUTH_ACCESS_TOKEN")
	t.Cleanup(func() {
		value, set := os.LookupEnv("GOOGLE_OAUTH_ACCESS_TOKEN")
		switch {
		case wanted && !set:
			t.Errorf("GOOGLE_OAUTH_ACCESS_TOKEN is unset once this test ended; the test began with it set, to a value of %d bytes", len(want))
		case !wanted && set:
			t.Errorf("GOOGLE_OAUTH_ACCESS_TOKEN is set, to a value of %d bytes, once this test ended; the test began without it at all", len(value))
		case value != want:
			t.Errorf("GOOGLE_OAUTH_ACCESS_TOKEN holds a different value from the one this test began with: %d bytes now against %d at the start", len(value), len(want))
		}
	})
	fixtureenv.Unset(t, "GOOGLE_OAUTH_ACCESS_TOKEN")
}

// A test that takes GOOGLE_OAUTH_ACCESS_TOKEN out of this binary's own environment leaves it as
// it found it. The Secret Manager tests clear the variable to make the store fall back to
// Application Default Credentials (gcpstore_test.go), through fixtureenv.Unset, which leaves
// putting it back to Go's own cleanup; this test reads it again and compares it with what the
// process held when the binary started, whether it held it included. The file is named so that
// it sorts after gcpstore_test.go: go test runs a package's tests in the order their files
// declare them, files taken in name order, and a reader that ran before the unsetter would prove
// nothing.
//
// The whole of this name is provable here, unlike the daemon's cmd/legion reader: no package
// this one imports unsets GOOGLE_OAUTH_ACCESS_TOKEN in an init, so the snapshot holds whatever
// the operator's shell gave the binary, and only a test could take it away.
//
// No failure below prints the variable's value, nor any part of one: the variable holds an
// operator's access token, and a test's failure message is read in the run's output, in CI's
// logs and in every transcript that carries them. Each message says whether the variable is set
// and how many bytes its value holds, and nothing more. The comparison is still of the values
// themselves, in memory, which is what this test is for.
func TestTheAccessTokenTheBinaryStartedWithIsBackOnceTheTestsThatClearItEnd(t *testing.T) {
	if !startupOAuthToken.taken {
		t.Fatal("TestMain took no snapshot of the access token this binary started with")
	}
	value, set := os.LookupEnv("GOOGLE_OAUTH_ACCESS_TOKEN")
	switch {
	case startupOAuthToken.set && !set:
		t.Errorf("GOOGLE_OAUTH_ACCESS_TOKEN is unset; the binary started with it set, to a value of %d bytes",
			len(startupOAuthToken.value))
	case !startupOAuthToken.set && set:
		t.Errorf("GOOGLE_OAUTH_ACCESS_TOKEN is set, to a value of %d bytes; the binary started without it at all",
			len(value))
	case value != startupOAuthToken.value:
		t.Errorf("GOOGLE_OAUTH_ACCESS_TOKEN holds a different value from the one the binary started with: %d bytes now against %d at the start; where those two counts are equal the value still changed, keeping its length",
			len(value), len(startupOAuthToken.value))
	}
}
