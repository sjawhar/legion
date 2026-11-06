package seed

import (
	"os"
	"testing"
)

// startupOAuthToken is what the process held of GOOGLE_OAUTH_ACCESS_TOKEN when the test binary
// started: its value, and whether it held the variable at all, so an empty value is told from
// none (os.LookupEnv). runTrustingTheFake records it before m.Run.
var startupOAuthToken struct {
	value string
	set   bool
}

// snapshotStartupOAuthToken records what the process holds of GOOGLE_OAUTH_ACCESS_TOKEN, which
// the Secret Manager store reads as an operator's access token.
func snapshotStartupOAuthToken() {
	startupOAuthToken.value, startupOAuthToken.set = os.LookupEnv("GOOGLE_OAUTH_ACCESS_TOKEN")
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
func TestTheAccessTokenTheBinaryStartedWithIsBackOnceTheTestsThatClearItEnd(t *testing.T) {
	value, set := os.LookupEnv("GOOGLE_OAUTH_ACCESS_TOKEN")
	switch {
	case startupOAuthToken.set && !set:
		t.Errorf("GOOGLE_OAUTH_ACCESS_TOKEN is unset; the binary started with it %q", startupOAuthToken.value)
	case !startupOAuthToken.set && set:
		t.Errorf("GOOGLE_OAUTH_ACCESS_TOKEN is %q; the binary started without it", value)
	case value != startupOAuthToken.value:
		t.Errorf("GOOGLE_OAUTH_ACCESS_TOKEN is %q; the binary started with %q", value, startupOAuthToken.value)
	}
}
