package workflow

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// checkTaskWakes are the four wakes a look can have, each written as its own caller writes it:
// the merge that opened the check (openCheck, the merge commit as the detail), the wake the
// checking agent named on a pending report (checkReported, the report's summary), the retry of a
// look nobody reported (endLook, its sentence and the cause it was given), and the check's bound
// (checkBound). The last three are the ones a reader of the task must tell apart, so each case
// names the words the task has to carry.
var checkTaskWakes = []struct {
	name   string
	cause  string
	detail string
	woke   string
}{
	{"the merge", wakeMerge, "merge-43", "woken by the merge."},
	{"the agent's own wake", wakeNamed, "production runs yesterday's build; the release train runs at 09:00",
		"woken by the wake named at the last look — production runs yesterday's build; the release train runs at 09:00."},
	{"the retry of a look nobody reported", wakeNamed, "the retry of a look nobody reported: look bound",
		"woken by the wake named at the last look — the retry of a look nobody reported: look bound."},
	{"the bound", wakeBound, "production did not carry the merge within " + boundText(14*24*time.Hour),
		"woken by the bound: production did not carry the merge within 14 days."},
}

// checkTaskFor is the task of a look woken by cause with detail, last marking the bound's look.
// One issue and one check for every case, so two tasks differ only in their wake.
func checkTaskFor(cause, detail string, last bool) string {
	issue := record.Issue{Key: "LEGION-208", Title: "A production check"}
	check := record.ProductionCheck{Issue: issue.Key, Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43",
		OpenedAt: checkOpened, Looks: 2}
	return checkTask(issue, check, cause, detail, last)
}

// The task tells the checking agent what woke this look, and nothing else does: the session may be
// a fresh one on a woken pod, with no memory of the report it sent. A look the agent's own wake
// started names the summary that report gave, and one the retry started names the retry, so a
// reader can tell the look it asked for from the look it was given after saying nothing.
func TestTheCheckTaskNamesWhatWokeItsOwnLook(t *testing.T) {
	for _, tc := range checkTaskWakes {
		t.Run(tc.name, func(t *testing.T) {
			task := checkTaskFor(tc.cause, tc.detail, false)
			if !strings.Contains(task, tc.woke) {
				t.Errorf("the task of a look woken by %s = %q, want it to say %q", tc.name, task, tc.woke)
			}
			// The first line is the one a resumed session tells the check from its last phase task
			// by, and the skills and the legion tool's description quote it.
			if want := "Production check of LEGION-208: A production check.\n"; !strings.HasPrefix(task, want) {
				t.Errorf("the task of a look woken by %s opens %q, want it to open %q", tc.name, task, want)
			}
		})
	}
}

// Nothing the daemon reads from a deployment wakes a check any more: no Dispatch `issue.deployed`,
// no Dispatch build, no boot of the daemon's own build and no deploy workflow run. So no look's
// task may name one of them or tell its reader to wait in the session for a deploy, which would
// hold the pod for a wake that never comes. The look ends with the report, and the agent's own
// `wake_at` starts the next one.
func TestNoCheckTaskNamesADeployWakeOrTellsItsReaderToWait(t *testing.T) {
	retired := []string{"issue.deployed", "deploy workflow", "daemon's boot", "Dispatch serving build",
		"Dispatch's build", "the deploy's event", "wakes the check on", "Wait for no deploy",
		"await no reply", "deploy role"}
	for _, tc := range checkTaskWakes {
		for _, last := range []bool{false, true} {
			task := checkTaskFor(tc.cause, tc.detail, last)
			for _, phrase := range retired {
				if strings.Contains(task, phrase) {
					t.Errorf("the task of a look woken by %s (last %t) still says %q", tc.name, last, phrase)
				}
			}
			if want := "Wait for nothing in this session: end the turn with the report."; !strings.Contains(task, want) {
				t.Errorf("the task of a look woken by %s (last %t) = %q, want it to say %q", tc.name, last, task, want)
			}
		}
	}
}

// A pending on a look that is not the check's last names when the next look is due: nothing else
// starts one, and the route refuses a pending without it (CHECK_WAKE_REQUIRED). On the bound's
// look no look follows, the pending closes the check as not run, and a time there is refused
// (CHECK_WAKE_UNEXPECTED), so that task asks for none and says what to write before reporting it.
func TestTheCheckTaskAsksForAWakeOnEveryPendingButTheLastLooks(t *testing.T) {
	const asks = "A pending report also carries wake_at, an RFC 3339 time: when you want your next look."
	task := checkTaskFor(wakeMerge, "merge-43", false)
	if !strings.Contains(task, asks) {
		t.Errorf("the task of a look that is not the last = %q, want it to ask %q", task, asks)
	}
	for _, want := range []string{
		"the daemon stops this process and keeps this session",
		"a task naming the summary you gave as what woke it",
	} {
		if !strings.Contains(task, want) {
			t.Errorf("the task of a look that is not the last = %q, want it to say %q", task, want)
		}
	}
	last := checkTaskFor(wakeBound, "production did not carry the merge within "+boundText(14*24*time.Hour), true)
	if strings.Contains(last, asks) {
		t.Errorf("the last look's task asks for a wake: %q", last)
	}
	for _, want := range []string{
		"This is the last look: no look follows.",
		"This last look's pending carries no wake_at and closes the check as not run",
	} {
		if !strings.Contains(last, want) {
			t.Errorf("the last look's task = %q, want it to say %q", last, want)
		}
	}
}
