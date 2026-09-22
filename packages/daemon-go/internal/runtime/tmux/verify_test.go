package tmux

import (
	"os"
	"strings"
	"testing"
	"time"
)

// Fields 3..52 of a real `/proc/<pid>/stat` line for a shell; field 22 (starttime) is 1234567
// (proc-stat.test.ts:4-6).
const statTail = "S 1 4242 4242 0 -1 4194560 812 0 0 0 3 1 0 0 20 0 1 0 1234567 8912896 486 18446744073709551615 1 1 0 0 0 0 0 0 65536 1 0 0 17 3 0 0 0 0 0 0 0 0 0 0 0 0 0"

// Field 22, counted only after the last `)`, since comm may hold spaces and parentheses
// (proc-stat.ts:1-28, proc-stat.test.ts:8-28).
func TestParseProcStatStartTicks(t *testing.T) {
	for _, line := range []string{
		"4242 (sh) " + statTail + "\n",
		"4242 (tmux: server) " + statTail,
		"4242 (a) b (c)) " + statTail,
	} {
		ticks, err := parseProcStatStartTicks(line)
		if err != nil || ticks != 1234567 {
			t.Errorf("parseProcStatStartTicks(%q) = (%d, %v), want 1234567", line, ticks, err)
		}
	}
	for line, want := range map[string]string{
		"4242 sh S 1 2":     "comm terminator",
		"4242 (sh) S 1 2 3": "starttime",
	} {
		if _, err := parseProcStatStartTicks(line); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("parseProcStatStartTicks(%q) error = %v, want one naming %s", line, err, want)
		}
	}
	self, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		t.Skipf("no /proc here: %v", err)
	}
	if ticks, err := parseProcStatStartTicks(string(self)); err != nil || ticks == 0 {
		t.Errorf("this process's own stat line: (%d, %v)", ticks, err)
	}
}

// One window, three panes: the architect's first, then two split-in workers (tmux.test.ts:18).
const listing = "%1531 2363427\n%1533 3003090\n%1534 446716\n"

// lookupPane's three verdicts over a `list-panes -t` listing (tmux.ts:386-433,
// tmux.test.ts:62-126): the pane's own row, absent only on proof, and failed on anything else.
func TestReadPaneLookup(t *testing.T) {
	for _, tc := range []struct {
		name   string
		paneID string
		result result
		want   paneLookup
	}{
		{"the pane's own row", "%1533", result{stdout: listing}, paneLookup{status: panePresent, pid: 3003090}},
		{"absent from a successful listing", "%1535", result{stdout: listing}, paneLookup{status: paneAbsent}},
		{"can't find pane", "%1533", result{stderr: "can't find pane: %1533", exitCode: 1}, paneLookup{status: paneAbsent}},
		{"no server behind the socket", "%1533", result{stderr: "no server running on /tmp/tmux-1000/legion-omp", exitCode: 1}, paneLookup{status: paneAbsent}},
		{"a socket never created", "%1533", result{stderr: "error connecting to /tmp/tmux-1000/legion-omp (No such file or directory)", exitCode: 1}, paneLookup{status: paneAbsent}},
		{
			"any other exit proves nothing", "%1533",
			result{stderr: "server not responding", exitCode: 1},
			paneLookup{status: paneLookupFailed, detail: "list-panes -t %1533 exited 1: server not responding"},
		},
		{
			"a permission refusal proves nothing", "%1533",
			result{stderr: "error connecting to /tmp/tmux-1000/legion-omp (Permission denied)", exitCode: 1},
			paneLookup{status: paneLookupFailed, detail: "list-panes -t %1533 exited 1: error connecting to /tmp/tmux-1000/legion-omp (Permission denied)"},
		},
		{
			"a client killed by its own timeout proves nothing", "%1533",
			result{stdout: listing, exitCode: -1, timedOut: 2 * time.Second},
			paneLookup{status: paneLookupFailed, detail: "list-panes -t %1533 timed out after 2s"},
		},
		{
			"a row whose pid is not a pid", "%1533",
			result{stdout: "%1533 nope\n"},
			paneLookup{status: paneLookupFailed, detail: "list-panes -t %1533 reported an unparseable pid for %1533: %1533 nope"},
		},
		{"exact ids, never a prefix: %15", "%15", result{stdout: "%150 446716\n%1 3715931\n%15 4141285\n"}, paneLookup{status: panePresent, pid: 4141285}},
		{"exact ids, never a prefix: %1", "%1", result{stdout: "%150 446716\n%1 3715931\n%15 4141285\n"}, paneLookup{status: panePresent, pid: 3715931}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := readPaneLookup(tc.paneID, tc.result); got != tc.want {
				t.Errorf("readPaneLookup = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// What an operator reads for each way a pane fails verification (runtime-tmux.ts:189-211).
func TestDescribeVerdict(t *testing.T) {
	recorded := incarnation{pid: 4242, ticks: 1234567}
	for _, tc := range []struct {
		verdict verdict
		want    string
	}{
		{verdict{reason: reasonPaneGone}, "pane %41 is gone"},
		{verdict{reason: reasonListingFailed, detail: "list-panes -t %41 exited 1: server not responding"}, "cannot verify pane %41: list-panes -t %41 exited 1: server not responding"},
		{verdict{reason: reasonPidMismatch, observedPid: 5151}, "pane %41 now runs pid 5151 (recorded pid 4242 start 1234567)"},
		{verdict{reason: reasonStatUnreadable, observedPid: 4242}, "pane %41 pid 4242 has no readable /proc stat (recorded pid 4242 start 1234567)"},
		{verdict{reason: reasonStartMismatch, observedPid: 4242, observedTicks: 99}, "pane %41 runs pid 4242 started at 99 (recorded pid 4242 start 1234567)"},
		{verdict{reason: reasonNotOmp, observedPid: 4242}, "pane %41 pid 4242 is not running OMP (recorded pid 4242 start 1234567)"},
	} {
		if got := describeVerdict("%41", recorded, tc.verdict); got != tc.want {
			t.Errorf("describeVerdict(%v) = %q, want %q", tc.verdict.reason, got, tc.want)
		}
	}
}

// An incarnation is "<pane pid>:<start ticks>" (runtime.TmuxLocator); anything else names a
// locator nothing can be acted on.
func TestIncarnationRoundTrip(t *testing.T) {
	inc, err := parseIncarnation("4242:1234567")
	if err != nil || inc != (incarnation{pid: 4242, ticks: 1234567}) {
		t.Fatalf("parseIncarnation = (%+v, %v)", inc, err)
	}
	if inc.String() != "4242:1234567" {
		t.Errorf("String() = %q", inc.String())
	}
	for _, bad := range []string{"", "4242", "4242:", ":1", "0:1", "-1:1", "4242:x", "4242:1:2"} {
		if _, err := parseIncarnation(bad); err == nil {
			t.Errorf("parseIncarnation(%q) succeeded", bad)
		}
	}
}
