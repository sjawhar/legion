package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// checkoutDelay is how long the jj the cases below start sleeps before a checkout, and
// checkoutBudget the runner's slow-command budget under them: the delay outlasts that budget by
// three seconds, so neither case turns on how fast this machine is. Every other provisioning
// command of the fixture's one-commit repository answers in milliseconds, well inside whichever
// bound it carries.
const (
	checkoutDelay  = 8 * time.Second
	checkoutBudget = 5 * time.Second
)

// slowCheckoutTools is testTools with a jj that sleeps delay whenever its arguments hold `sparse
// reset`, so the checkout alone outlasts a runner budget shorter than delay. The sleeping script
// is first on the runner's jj entry and execs testTools' own jj wrapper with every argument it was
// given, so whatever that wrapper does for GIT_ALLOW_PROTOCOL still happens: the fixture's remote
// is a local path, which git reaches over its file transport alone. It reads the two words
// wherever they stand rather than as "$1 $2", because the runner puts its own
// --config=git.executable-path first on every jj it starts (execRunner.Run); no other jj
// provisioning runs holds them side by side (`jj sparse list`, `jj sparse set --clear`).
func slowCheckoutTools(t *testing.T, delay time.Duration) map[string]string {
	t.Helper()
	tools := testTools(t)
	wrapper := filepath.Join(t.TempDir(), "jj")
	script := "#!/bin/sh\ncase \" $* \" in *' sparse reset '*) sleep " + strconv.FormatFloat(delay.Seconds(), 'f', -1, 64) + " ;; esac\nexec '" + tools["jj"] + "' \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	tools["jj"] = wrapper
	return tools
}

// slowCheckoutRunner is newLocalRunner whose jj sleeps delay before a checkout and whose
// slow-command budget is budget, what the runner gives a command carrying no bound of its own.
func slowCheckoutRunner(t *testing.T, delay, budget time.Duration) *recordingRunner {
	t.Helper()
	run := newLocalRunner(t)
	run.timeout = budget
	run.runner = NewRunner(budget, slowCheckoutTools(t, delay))
	return run
}

// Each provisioning command is given the bound its own class carries: the checkout, the one that
// writes the issue's whole working copy, is given CheckoutTimeout, and a command carrying no
// bound of its own — `jj workspace update-stale`, which only a workspace that already exists runs,
// among them — the runner's slow-command budget. The shared clone's size-following steps share one
// CloneTimeout and are TestTheSharedClonesCloneIsBoundedByCloneTimeout's, so this case passes
// over them (isSharedCloneStep). A fresh workspace and one provisioned a second time are read
// together, so both paths through Provision are covered.
func TestProvisionBoundsEachCommandByItsOwnBudget(t *testing.T) {
	run := newLocalRunner(t)
	req := feedRequest(t, run, provisionRequest(t))
	checkouts, stale := 0, 0
	for _, round := range []string{"a fresh workspace", "a workspace that already exists"} {
		before := len(run.Calls())
		if _, err := Provision(context.Background(), run, req); err != nil {
			t.Fatalf("provision %s: %v", round, err)
		}
		for _, call := range run.Calls()[before:] {
			// The shared clone's own size-following steps carry what remains of CloneTimeout, and
			// are TestTheSharedClonesCloneIsBoundedByCloneTimeout's: they are recognised here the
			// way that case recognises them, and left to it.
			if isSharedCloneStep(call.Argv) {
				continue
			}
			if isCheckout(call.Argv) {
				checkouts++
				if call.Timeout != CheckoutTimeout {
					t.Errorf("%s: the checkout %q was given %s, want CheckoutTimeout %s", round, call.Argv, call.Timeout, CheckoutTimeout)
				}
				continue
			}
			if commandWith(call.Argv, "jj", "workspace", "update-stale") {
				stale++
			}
			if call.Timeout != testTimeout {
				t.Errorf("%s: %q was given %s, want the runner's budget %s", round, call.Argv, call.Timeout, testTimeout)
			}
		}
	}
	// The fresh round checks out; the second finds the workspace checked out already (CheckedOut)
	// and runs `jj workspace update-stale` instead, so each is seen once over the two rounds.
	if checkouts != 1 {
		t.Errorf("provisioning ran %d checkouts over the two rounds, want the fresh workspace's one", checkouts)
	}
	if stale != 1 {
		t.Errorf("provisioning ran %d `jj workspace update-stale` over the two rounds, want the existing workspace's one", stale)
	}
}

// A checkout that outlasts the runner's slow-command budget finishes, and the workspace it wrote
// holds the whole tree: the checkout is bounded by CheckoutTimeout, an hour, and not by that
// budget.
func TestProvisionChecksOutPastTheRunnersBudget(t *testing.T) {
	run := slowCheckoutRunner(t, checkoutDelay, checkoutBudget)
	pushRemoteBranch(t, run.remote, "main", "history.txt")
	working, err := Provision(context.Background(), run, provisionRequest(t))
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	requireCheckedOut(t, working)
	// The file list requireCheckedOut read is read again here: what this case proves beyond it is
	// that the checkout wrote each of those files to disk, and the list is unchanged between the
	// two reads — nothing writes to the workspace in between.
	files := strings.Fields(runSetup(t, working.Dir, "jj", "file", "list", "-R", working.Dir))
	for _, file := range files {
		if _, err := os.Stat(filepath.Join(working.Dir, file)); err != nil {
			t.Errorf("the checkout left %s unwritten: %v", file, err)
		}
	}
}

// A checkout that outlives its own bound fails the provision, naming the checkout: the checkout
// is bounded by that bound and by no other, so a request that gives it one second (CheckoutBound)
// ends the checkout there, inside the five seconds the runner's own budget would have allowed it.
func TestProvisionFailsWhenTheCheckoutOutlivesItsBound(t *testing.T) {
	run := slowCheckoutRunner(t, checkoutDelay, checkoutBudget)
	req := provisionRequest(t)
	req.CheckoutBound = time.Second
	working, err := Location(req.StateDir, req.Repo, req.Issue)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Provision(context.Background(), run, req)
	want := "check out workspace " + working.Dir + ": command timed out: jj sparse reset"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Provision whose checkout outlived its one-second bound = %v, want an error holding %q", err, want)
	}
}
