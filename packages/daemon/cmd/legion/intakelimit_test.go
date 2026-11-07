package main

import (
	"bytes"
	"context"
	"testing"
)

// derivedIntakeHandlerLimitLine is the sentence `legion start --check-config` prints for a file
// whose `worker_rpc_timeout_seconds` is 200, written out here rather than built from
// daemon.IntakeHandlerLimitLine: a row that composed the expected line from the code that writes
// it would pass whatever that code said, and what an operator reads is the whole point of the
// line. The numbers in it are the derivation's — four windows of 200 s — so a change to the
// derivation or to the wording fails this row and is read by a person before it ships.
const derivedIntakeHandlerLimitLine = "intake: one fact's handling is cut after 800s: " +
	"worker_rpc_timeout_seconds (200) bounds each read a review makes — whether its pull request is recorded, " +
	"its truncated body and its author's repository permission — and one window more is left for the part nothing bounds, " +
	"the fact's transaction and its wait on the apply lock\n"

// Two configurations that differ in one line, `worker_rpc_timeout_seconds`, run through the same
// check: the check names the intake handler limit when the window derives one above the floor,
// and says nothing about it when the floor still decides it. Both halves matter, and the quiet
// one matters most. The line is a report an operator reads back, so a check that printed it for
// every file would add a fifth line to the output of every deployment on the default window —
// including the ones this package's other rows assert whole, character for character
// (TestStartCheckConfigValidatesAndStartsNothing and its neighbours) — and would tell an operator
// nothing they could not read in the daemon's own default. The derived half is asserted verbatim
// for the same reason: the sentence is the deliverable.
func TestStartCheckConfigNamesTheIntakeHandlerLimitOnlyWhenTheWindowDerivesOne(t *testing.T) {
	for _, row := range []struct {
		name     string
		projects string
		want     string
	}{
		{
			"a window the ten-minute floor covers names no limit",
			"worker_rpc_timeout_seconds: 5\nprojects:\n  DEMO: {}\n",
			demoCheckConfigOK + tmuxCapabilityGaps,
		},
		{
			"a window that derives a limit above the floor names it",
			"worker_rpc_timeout_seconds: 200\nprojects:\n  DEMO: {}\n",
			demoCheckConfigOK + tmuxCapabilityGaps + derivedIntakeHandlerLimitLine,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			legionState(t)
			config := projectsConfig(t, row.projects)

			var out, errb bytes.Buffer
			code := run(context.Background(), []string{"legion", "start", "--config", config, "--check-config"}, &out, &errb)

			if code != 0 || out.String() != row.want || errb.Len() != 0 {
				t.Fatalf("exit code = %d, stdout %q, stderr %q; want 0 and %q", code, out.String(), errb.String(), row.want)
			}
		})
	}
}
