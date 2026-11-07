// Package fixturegit runs the git and jj commands a test's fixture is made of. A fixture builds a
// remote, a clone and a workspace out of the real tools, and every one of those commands needs the
// same three things: the directory it runs in, because jj searches upward from the directory it is
// started in for the closest .jj and a command with none would open the workspace the test process
// stands in, which in a Legion issue pod is a real one; an environment of its own, which is
// fixtureenv.Isolated's wherever the fixture has no reason for another, so no command reads the
// repository, configuration or credential of whoever runs the tests; and a failure that names what
// was run, where, and everything it printed. Runner holds the test and that environment, and its
// four ways of running one command are the four a fixture has: Run for the usual one, Raw where
// the output's own whitespace is part of what is read, Out where what is read must hold no stderr,
// and Try where the caller keeps the failure for itself.
//
// It is an ordinary package rather than a _test.go file of one of them because cmd/legion is
// package main, which can import no other package's test-only helpers.
package fixturegit

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
)

// Runner runs a fixture's git or jj commands under one environment. Env is the whole environment
// of each child, as exec.Cmd takes it, so a fixture that hands its commands a handful of variables
// of its own passes them as they are. There is no binary: argv[0] names it, so a fixture that runs
// a pinned git or jj passes that path itself.
type Runner struct {
	TB  testing.TB
	Env []string
}

// Isolated is a Runner whose commands run under fixtureenv.Isolated of home, which is the
// environment most of these fixtures want: one that reaches no workspace, repository, remote,
// credential or session store but the test's own.
func Isolated(tb testing.TB, home string) Runner {
	return Runner{TB: tb, Env: fixtureenv.Isolated(home)}
}

// With is the runner with extra added to its environment, as a copy, so the one it was made from
// is unchanged and two runners of one fixture do not share an array. A key in extra wins over the
// same key in Env, as exec.Cmd takes the last value of a key.
func (r Runner) With(extra ...string) Runner {
	env := make([]string, 0, len(r.Env)+len(extra))
	env = append(append(env, r.Env...), extra...)
	return Runner{TB: r.TB, Env: env}
}

// Run runs argv in dir and returns its combined output, trimmed, failing the test when the command
// fails with what was run, the directory it ran in, and everything it printed.
func (r Runner) Run(dir string, argv ...string) string {
	r.TB.Helper()
	return strings.TrimSpace(r.Raw(dir, argv...))
}

// Raw is Run without the trim, for a caller that reads the output's own whitespace: an empty value
// a trim would take away, or a line count.
func (r Runner) Raw(dir string, argv ...string) string {
	r.TB.Helper()
	output, err := r.command(dir, argv).CombinedOutput()
	if err != nil {
		r.TB.Fatalf("%s in %s: %v\n%s", strings.Join(argv, " "), dir, err, output)
	}
	return string(output)
}

// Out runs argv in dir and returns its standard output alone, untrimmed, for a caller that reads a
// value git or jj printed rather than a transcript: a progress line on stderr would otherwise be
// part of the value. What the command printed on stderr appears in the failure instead.
func (r Runner) Out(dir string, argv ...string) string {
	r.TB.Helper()
	command := r.command(dir, argv)
	var stderr strings.Builder
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		r.TB.Fatalf("%s in %s: %v\n%s", strings.Join(argv, " "), dir, err, stderr.String())
	}
	return string(output)
}

// Try runs argv in dir and returns its combined output, untrimmed, and the command's error,
// failing nothing itself: a caller whose failure text is its own, or one for which a command that
// fails is an answer rather than a failure, takes the error.
func (r Runner) Try(dir string, argv ...string) (string, error) {
	r.TB.Helper()
	output, err := r.command(dir, argv).CombinedOutput()
	return string(output), err
}

// command is argv in dir under the runner's environment.
func (r Runner) command(dir string, argv []string) *exec.Cmd {
	command := exec.Command(argv[0], argv[1:]...)
	command.Dir = dir
	command.Env = r.Env
	return command
}
