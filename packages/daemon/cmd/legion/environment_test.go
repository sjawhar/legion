package main

import (
	"os"
	"testing"
)

// startupRestored are the variables TestControllerStartOmitsTheOptionalPointersTheFileDoesNotSet
// takes out of this binary's own environment for its own run. Each is a pointer a pane's run of
// `legion controller start` reads, so a test that left one of them gone would hand every test
// after it an environment the process never had. That test names this list rather than spelling
// the variables again, both where it clears them and where it reads the child's environment
// back, so a name added here is cleared, asserted at the call site, and restored below.
var startupRestored = []string{"DISPATCH_URL", "DISPATCH_TOKEN_FILE", "ENVOY_TOKEN_FILE", "NATS_NKEY_SEED_FILE"}

// startupValue is what the process held of one variable: its value, and whether it held the
// variable at all, so an empty value is told from none (os.LookupEnv).
type startupValue struct {
	value string
	set   bool
}

// startupEnvironment is what the process held of startupRestored when the test binary started.
var startupEnvironment map[string]startupValue

// snapshotStartupEnvironment records what the process holds of startupRestored. TestMain calls it
// before fixtureenv.Guard, the first thing in this binary that changes the environment, as
// fixtureenv takes its own unguarded snapshot there.
func snapshotStartupEnvironment() {
	startupEnvironment = make(map[string]startupValue, len(startupRestored))
	for _, name := range startupRestored {
		value, set := os.LookupEnv(name)
		startupEnvironment[name] = startupValue{value: value, set: set}
	}
}

// A variable a test takes out of this binary's own environment is back once that test ends.
// TestControllerStartOmitsTheOptionalPointersTheFileDoesNotSet clears the four of startupRestored
// through fixtureenv.Unset, which leaves putting them back to Go's own cleanup; this test reads
// them again and compares each with what the process held when the binary started, whether it
// held it included. The file is named so that it sorts after controller_test.go: go test runs a
// package's tests in the order their files declare them, files taken in name order, and a reader
// that ran before the unsetter would prove nothing.
//
// Three of the four are provable this way, not all four. internal/testnats's init unsets
// NATS_NKEY_SEED_FILE in every binary that imports that package, this one among them
// (main_test.go), and an init runs before TestMain: the snapshot already records that name as
// absent, and it stays absent whatever any test here does. The call at the site covers all four;
// what this test proves is DISPATCH_URL, DISPATCH_TOKEN_FILE and ENVOY_TOKEN_FILE.
func TestTheVariablesAControllerTestClearsAreBackOnceItEnds(t *testing.T) {
	if startupEnvironment == nil {
		t.Fatal("TestMain took no snapshot of the environment this binary started with")
	}
	for _, name := range startupRestored {
		want := startupEnvironment[name]
		value, set := os.LookupEnv(name)
		switch {
		case want.set && !set:
			t.Errorf("%s is unset; the binary started with it %q", name, want.value)
		case !want.set && set:
			t.Errorf("%s is %q; the binary started without it", name, value)
		case value != want.value:
			t.Errorf("%s is %q; the binary started with %q", name, value, want.value)
		}
	}
}
