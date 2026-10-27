package main

import (
	"strings"
	"testing"
	"time"
)

func TestTablesMarkAStepWhoseStackProcessExited(t *testing.T) {
	busy := 0.42
	ok := stepData{name: "step-1x", sum: summary{Config: summaryConfig{Step: "step-1x"}, Resources: &resourcesSummary{}},
		meta: &metaFile{BoxBusyBefore: &busy}}
	killed := stepData{name: "killed", sum: summary{
		Config:     summaryConfig{Step: "killed"},
		StackExits: []stackExit{{Name: "dispatch", PID: 4242, FirstSeenGone: time.Date(2026, 10, 27, 9, 0, 15, 0, time.UTC)}},
	}}
	md := renderTables([]stepData{ok, killed})
	if !strings.Contains(md, "| killed (dispatch exited) |") {
		t.Fatalf("the killed step's row is not marked:\n%s", md)
	}
	if strings.Contains(md, "step-1x (") {
		t.Fatalf("a step with no stack exit is marked:\n%s", md)
	}
	if !strings.Contains(md, "\n(<process> exited): ") {
		t.Fatalf("no note defines the mark:\n%s", md)
	}
	if !strings.Contains(md, "| box busy | box busy before |") || !strings.Contains(md, "| 0% | 42% |") {
		t.Fatalf("the Resources table lacks box busy before 42%%:\n%s", md)
	}
	// The killed step has no resources: its row keeps the box busy before column, empty here.
	if !strings.Contains(section(md, "Resources"), "| killed (dispatch exited) | - | - | - | - | - | - | - | - | - | - | - | - | - | - | - |\n") {
		t.Fatalf("the killed step's Resources row is not all -:\n%s", md)
	}
	checkRowWidths(t, md)
}

// section is the text of the markdown section that title opens, up to the next section.
func section(md, title string) string {
	_, rest, _ := strings.Cut(md, "## "+title+"\n")
	text, _, _ := strings.Cut(rest, "\n## ")
	return text
}

// checkRowWidths fails when a table row has more or fewer cells than its table's header.
func checkRowWidths(t *testing.T, md string) {
	t.Helper()
	width := 0
	for line := range strings.SplitSeq(md, "\n") {
		if strings.HasPrefix(line, "|---") {
			continue
		}
		if !strings.HasPrefix(line, "| ") {
			width = 0
			continue
		}
		cells := len(strings.Split(strings.TrimSuffix(strings.TrimPrefix(line, "| "), " |"), " | "))
		if width == 0 {
			width = cells
			continue
		}
		if cells != width {
			t.Errorf("row has %d cells, its header %d: %s", cells, width, line)
		}
	}
}

func TestTablesRenderTheSamplersAndTheirFailedReads(t *testing.T) {
	xids := 412.3
	cur := summary{
		Config:    summaryConfig{Step: "new"},
		Events:    eventsSummary{DeliveryMsFromSent: stat{N: 3, P50: 120, P99: 900}},
		Resources: &resourcesSummary{XIDsPerS: &xids},
		SSE:       sseSummary{Frames: 1200, OrderViolations: 2, FrameMsFromCreatedAt: stat{N: 1200, P50: 40, P99: 300}, FrameMsFromWriteResponse: stat{N: 400, P50: 5, P99: 60}},
		UngrantedLocks: ungrantedSummary{Samples: 100, ShareWithAny: 0.97, Groups: []ungrantedGroup{{
			LockType: "advisory", Target: "commit_order_lock", Mode: "ExclusiveLock", WaitersP50: 12, WaitersMax: 40, ShareWithWaiter: 0.96,
			TopPairs: []waitPair{{WaiterQuery: "select a | b", BlockerQuery: "", Count: 800}},
		}}},
	}
	// Samplers that failed: the lock probe never connected, the ungranted-lock sampler's reads all
	// failed, the transaction-id probe has no rate.
	failed := summary{
		Config:          summaryConfig{Step: "failed"},
		Resources:       &resourcesSummary{},
		CommitOrderLock: lockSummary{Errors: 1},
		UngrantedLocks:  ungrantedSummary{Errors: 160},
	}
	md := renderTables([]stepData{{name: "failed", sum: failed}, {name: "new", sum: cur}})

	for _, c := range []struct{ title, failed, cur string }{
		{"Delivery from sending, and SSE frames", "| failed | - | - | - | 0 | 0 |", "| new | 120 / 900 | 40 / 300 | 5 / 60 | 1,200 | 2 |"},
		{"Commit-order lock", "| failed (1 failed reads) | - | - | - | - | - |", "| new | - | - | - | - | - |"},
		{"Ungranted locks", "| failed (160 failed reads) | - | - | - |", `| new | 97% | advisory commit_order_lock ExclusiveLock 96%, 12 / 40 | select a \| b → (none) (800) |`},
		{"Transaction ids", "| failed | - |", "| new | 412.3 |"},
	} {
		text := section(md, c.title)
		if !strings.Contains(text, c.failed+"\n") || !strings.Contains(text, c.cur+"\n") {
			t.Errorf("section %q lacks %q or %q:\n%s", c.title, c.failed, c.cur, text)
		}
	}
	for _, key := range []string{"commit_order_lock", "ungranted_locks"} {
		if !strings.Contains(md, "\n(<n> failed reads): the probe's read of pg_locks failed n times, a failed connect included (summary.json `"+key+".errors`)") {
			t.Errorf("no note defines the failed-reads mark for %s:\n%s", key, md)
		}
	}
	if strings.Contains(md, "| new (") {
		t.Errorf("a step whose probes read every time is marked:\n%s", md)
	}
	checkRowWidths(t, md)
}
