// Package fixtureenv is what a test takes out of its own process's environment and what puts it
// back: the module's one place for a helper that changes a variable the test process reads, so a
// test that needs a variable gone leaves the process as it found it. It is an ordinary package
// rather than a _test.go file because a command is package main and can import no other package's
// test helpers, the reason packages/envoy/internal/cmdtest gives for itself.
//
// The daemon module's package of the same name
// (packages/daemon/internal/fixtureenv) holds Unset too, besides the guards that keep that
// module's git and jj off a Legion pod's own workspace. The two are separate packages because
// Go's internal rule reads a path prefix, so neither module can import the other's.
package fixtureenv

import (
	"os"
	"testing"
)

// Unset takes every variable in names out of the test process's own environment: t.Setenv of an
// empty value, so Go puts back whatever the process held when the test ends, and then
// os.Unsetenv, so code that tells an empty value from an absent one (os.LookupEnv) finds none.
// It is variadic because a test that clears one of these usually clears a set of them together.
// Like t.Setenv, which it uses, it cannot be used in a parallel test. It is for a test and takes
// the test for that reason: a TestMain has no *testing.T, so nothing there would put the
// process's own values back, and a TestMain that wants a variable gone calls os.Unsetenv itself.
func Unset(t testing.TB, names ...string) {
	t.Helper()
	for _, name := range names {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
	}
}
