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
		Live:      liveSummary{Frames: 1200, OrderViolations: 2, FrameMsFromCreatedAt: stat{N: 1200, P50: 40, P99: 300}, FrameMsFromWriteResponse: stat{N: 400, P50: 5, P99: 60}},
		UngrantedLocks: ungrantedSummary{Samples: 100, ShareWithAny: 0.97, Groups: []ungrantedGroup{{
			LockType: "advisory", Target: "commit_order_lock", Mode: "ExclusiveLock", WaitersP50: 12, WaitersMax: 40, ShareWithWaiter: 0.96,
			TopPairs: []waitPair{{WaiterQuery: "select a | b", BlockerQuery: "", Count: 800}},
		}}},
		XIDHolds: &xactSummary{IntervalMs: 10, Samples: 17000, SamplesPerS: 94.4, XIDHolders: 640, CommittedEvents: 700,
			XIDHoldMs: stat{N: 640, P50: 180, P99: 1900, Max: 2400}, XactMs: stat{N: 640, P50: 400, P99: 2600, Max: 3100},
			IdleInXactWithXIDMs: stat{N: 640, P50: 120, P99: 1500, Max: 2000}, IdleInXactNoXIDMs: stat{N: 640, P50: 0, P99: 300, Max: 450},
			LongestXIDHolders: []xidHolder{{HoldMs: 2400, XactMs: 3100, Query: "update a | b"}, {HoldMs: 2100, XactMs: 2500, Query: ""}}},
	}
	// Samplers that failed: the lock probe and the transaction-id hold sampler never connected, the
	// ungranted-lock sampler's reads all failed, the transaction-id probe has no rate.
	failed := summary{
		Config:          summaryConfig{Step: "failed"},
		Resources:       &resourcesSummary{},
		CommitOrderLock: lockSummary{Errors: 1},
		XIDHolds:        &xactSummary{IntervalMs: 10, Errors: 1},
		UngrantedLocks:  ungrantedSummary{Errors: 160},
	}
	// The transaction-id hold sampler turned off (-xact-interval 0).
	off := summary{Config: summaryConfig{Step: "off"}, XIDHolds: &xactSummary{}}
	md := renderTables([]stepData{{name: "failed", sum: failed}, {name: "new", sum: cur}, {name: "off", sum: off}})

	for _, c := range []struct{ title, failed, cur string }{
		{"Delivery from sending, and live socket frames", "| failed | - | - | - | 0 | 0 |", "| new | 120 / 900 | 40 / 300 | 5 / 60 | 1,200 | 2 |"},
		{"Commit-order lock", "| failed (1 failed read) | - | - | - | - | - |", "| new | - | - | - | - | - |"},
		{"Ungranted locks", "| failed (160 failed reads) | - | - | - |", `| new | 97% | advisory commit_order_lock ExclusiveLock 96%, 12 / 40 | select a \| b → (none) (800) |`},
		{"Transaction ids", "| failed | - |", "| new | 412.3 |"},
		{"Transaction-id holds", "| failed (1 failed read) | - | - | - | - | - | - | - |",
			`| new | 10 (94) | 640 / 700 | 180 / 1,900 / 2,400 | 400 / 2,600 / 3,100 | 120 / 1,500 / 2,000 | 0 / 300 / 450 | 2,400 update a \| b; 2,100 (none) |`},
		{"Transaction-id holds", "| off | off | - | - | - | - | - | - |", "| off | off | - | - | - | - | - | - |"},
	} {
		text := section(md, c.title)
		if !strings.Contains(text, c.failed+"\n") || !strings.Contains(text, c.cur+"\n") {
			t.Errorf("section %q lacks %q or %q:\n%s", c.title, c.failed, c.cur, text)
		}
	}
	for _, c := range []struct{ title, view, key string }{
		{"Commit-order lock", "pg_locks", "commit_order_lock.errors"},
		{"Ungranted locks", "pg_locks", "ungranted_locks.errors"},
		{"Transaction-id holds", "pg_stat_activity", "xid_holds.errors"},
	} {
		if note := noteLine(section(md, c.title), "(<n> failed reads)"); !strings.Contains(note, "read of "+c.view) || !strings.Contains(note, "`"+c.key+"`") {
			t.Errorf("section %q has no note of the failed-reads mark that names the read of %s and summary.json `%s`:\n%s", c.title, c.view, c.key, md)
		}
	}
	if strings.Contains(md, "| new (") {
		t.Errorf("a step whose probes read every time is marked:\n%s", md)
	}
	checkRowWidths(t, md)
}

// noteLine is the line of text that defines mark, "" when none does.
func noteLine(text, mark string) string {
	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(line, mark) {
			return line
		}
	}
	return ""
}

// rowCells is the cells of the table row in text whose step cell starts with step, nil when no row
// does.
func rowCells(text, step string) []string {
	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(line, "| "+step) {
			return strings.Split(strings.TrimSuffix(strings.TrimPrefix(line, "| "), " |"), " | ")
		}
	}
	return nil
}

func TestTablesResourcesShowFailedSamplerReadsAsMissing(t *testing.T) {
	step := func(name string, ss ...sample) stepData {
		return stepData{name: name, sum: summary{Config: summaryConfig{Step: name}, Resources: summarizeResources(ss, 0, 15)}}
	}
	md := renderTables([]stepData{
		step("unread", fakeSample(0, 0, 0, 0, readPostgres, readGoroutines, readHeap), fakeSample(5, 0, 0, 0, readPostgres, readGoroutines),
			fakeSample(10, 0, 0, 0, readPostgres, readGoroutines)),
		step("partial", fakeSample(0, 0, 0, 0, readPostgres, readGoroutines), fakeSample(5, 10, 100, 0), fakeSample(10, 12, 120, 0)),
		step("clean", fakeSample(0, 8, 90, 20), fakeSample(5, 9, 95, 0), fakeSample(10, 9, 95, 0)),
	})
	text := section(md, "Resources")
	// The columns: heap in use, goroutines, pool waiters, Postgres connections max, connections
	// waiting on a lock.
	const heap, goroutines, pool, pgMax, lockWait = 10, 11, 12, 13, 14

	unread := rowCells(text, "unread")
	if unread == nil || unread[0] != "unread (7 failed reads)" {
		t.Fatalf("the row whose reads all failed is not marked with its 7 failed reads:\n%s", text)
	}
	for _, col := range []int{heap, goroutines, pool, pgMax, lockWait} {
		if unread[col] != "-" {
			t.Errorf("column %d of the row whose reads all failed reads %q, want -:\n%s", col, unread[col], text)
		}
	}
	if unread[1] == "-" {
		t.Errorf("the /proc columns of the row whose reads failed are empty too:\n%s", text)
	}

	partial := rowCells(text, "partial")
	if partial == nil || partial[0] != "partial (2 failed reads)" {
		t.Fatalf("the row with two failed reads is not marked with them:\n%s", text)
	}
	if partial[pgMax] != "12" || partial[goroutines] != "100 / 120" {
		t.Errorf("the row with two failed reads: Postgres max %q, goroutines %q; want 12 and 100 / 120", partial[pgMax], partial[goroutines])
	}

	if clean := rowCells(text, "clean"); clean == nil || clean[0] != "clean" {
		t.Errorf("a row whose reads all succeeded is marked:\n%s", text)
	}
	note := noteLine(text, "(<n> failed reads)")
	for _, key := range []string{"resources.pg_read_errors", "resources.pprof_read_errors"} {
		if !strings.Contains(note, "`"+key+"`") {
			t.Errorf("the failed-reads note of the Resources table does not name summary.json `%s`:\n%s", key, text)
		}
	}
	checkRowWidths(t, md)
}

// TestTablesDoNotShowUnreadCommitsAsZero: a step whose loadtest.event_commits could not be read
// (events.commits_error: a foreign run's database without the commit trigger, or an unreachable
// -pg) knows nothing about its commits, receipts or delivery: Committed is 0 because nothing was
// read, not because nothing committed. Its tables must not present that 0 as a measurement: the
// committed rate, the issue-topic receipts and each kind's committed count are not 0; each table
// with cells that depend on the commit rows marks the row, shows "-" in those cells and names
// events.commits_error in a note; the other cells keep their values; and neither a step whose
// commit rows were read nor a table that no commit row feeds carries the mark.
func TestTablesDoNotShowUnreadCommitsAsZero(t *testing.T) {
	perOp := 0.0123
	read := stepData{name: "read", sum: summary{
		Config:    summaryConfig{Step: "read", EventsPerS: 1},
		Events:    eventsSummary{Committed: 30, CommittedPerS: 1, DeliveryMsFromSent: stat{N: 60, P50: 120, P99: 900}},
		Resources: &resourcesSummary{DispatchCPUSPerOp: &perOp},
		Live:      liveSummary{Frames: 40, FrameMsFromWriteResponse: stat{N: 30, P50: 5, P99: 60}},
	}}
	// What run.go writes for a step whose commit rows were not read: no delivery latency, no CPU
	// per operation, and its live frames from created_at, which need no commit row.
	unread := stepData{name: "foreign-notrigger", sum: summary{
		Config: summaryConfig{Step: "foreign-notrigger", EventsPerS: 1, Sessions: 10},
		Events: eventsSummary{
			Offered:     31,
			OfferedPerS: 1.03,
			ByKind: map[string]kindSummary{
				kindComment:  {Offered: 1, WriteMs: stat{N: 1, P50: 118, P99: 118}},
				kindAnchored: {Offered: 8, WriteMs: stat{N: 8, P50: 670, P99: 720}},
			},
			CommitsError: `read loadtest.event_commits (is commit-trigger.sql installed?): ERROR: relation "loadtest.event_commits" does not exist (SQLSTATE 42P01)`,
		},
		Resources: &resourcesSummary{},
		Live:      liveSummary{Frames: 40, FrameMsFromCreatedAt: stat{N: 40, P50: 30, P99: 80}},
	}}
	md := renderTables([]stepData{read, unread})

	cells := rowCells(section(md, "Events, delivery and requests"), "foreign-notrigger")
	if cells == nil {
		t.Fatalf("no Events row for the step:\n%s", md)
	}
	if len(cells) < 8 {
		t.Fatalf("Events row has %d cells:\n%s", len(cells), md)
	}
	if rate := cells[3]; strings.HasSuffix(rate, "/ 0.00") {
		t.Errorf("the committed rate of a step whose commit rows were not read reads %q, a measured zero:\n%s", rate, md)
	}
	if receipts := cells[7]; receipts == "0 / 0" {
		t.Errorf("the issue-topic receipts of a step whose commit rows were not read read %q, a measured zero:\n%s", receipts, md)
	}
	for line := range strings.SplitSeq(section(md, "Write latency by kind"), "\n") {
		if strings.HasPrefix(line, "| foreign-notrigger") && strings.Contains(line, "(0 / ") {
			t.Errorf("a kind's committed count of a step whose commit rows were not read reads 0:\n%s", line)
		}
	}

	const mark = "foreign-notrigger " + commitsUnreadMark
	for _, c := range []struct {
		title string
		cols  []int // the cells that depend on the commit rows
	}{
		{"Events, delivery and requests", []int{5, 6, 7}},
		{"Write latency by kind", nil},
		{"Resources", []int{15}},
		{"Delivery from sending, and live socket frames", []int{1, 3}},
	} {
		text := section(md, c.title)
		cells := rowCells(text, "foreign-notrigger")
		if cells == nil || cells[0] != mark {
			t.Errorf("section %q: the unread step's row is not marked %q:\n%s", c.title, commitsUnreadMark, text)
			continue
		}
		for _, col := range c.cols {
			if cells[col] != "-" {
				t.Errorf("section %q: cell %d of the unread step reads %q, want -:\n%s", c.title, col, cells[col], text)
			}
		}
		if note := noteLine(text, commitsUnreadMark); !strings.Contains(note, "`events.commits_error`") {
			t.Errorf("section %q has no note of the mark that names summary.json `events.commits_error`:\n%s", c.title, text)
		}
		if r := rowCells(text, "read"); r == nil || r[0] != "read" {
			t.Errorf("section %q: the step whose commit rows were read is marked:\n%s", c.title, text)
		}
	}
	// The cells that do not depend on the commit rows keep their values.
	if cells := rowCells(section(md, "Delivery from sending, and live socket frames"), "foreign-notrigger"); cells[2] != "30 / 80" || cells[4] != "40" {
		t.Errorf("the unread step's live frames and their latency from created_at: %q", cells)
	}
	if cells := rowCells(section(md, "Events, delivery and requests"), "foreign-notrigger"); cells[3] != "1.00 / 1.03 / -" {
		t.Errorf("the unread step's events/s: %q, want the target and offered rates and - committed", cells[3])
	}
	// The tables that no commit row feeds carry no mark.
	for _, title := range []string{"Commit-order lock", "Ungranted locks", "Transaction ids"} {
		if strings.Contains(section(md, title), commitsUnreadMark) {
			t.Errorf("section %q, which no commit row feeds, carries the mark:\n%s", title, md)
		}
	}
	checkRowWidths(t, md)
}

// TestTablesMarkAFailedTransactionIDRead: a step whose transaction-id probe failed shows "-" with
// the mark "(1 failed read)", and the note names resources.xid_read_error; a step with no
// resources shows "-" unmarked, as every cell of its Resources row.
func TestTablesMarkAFailedTransactionIDRead(t *testing.T) {
	md := renderTables([]stepData{
		{name: "xid-failed", sum: summary{Config: summaryConfig{Step: "xid-failed"}, Resources: &resourcesSummary{XIDReadError: "xid probe: failed to connect"}}},
		{name: "no-window", sum: summary{Config: summaryConfig{Step: "no-window"}}},
	})
	text := section(md, "Transaction ids")
	if cells := rowCells(text, "xid-failed"); cells == nil || cells[0] != "xid-failed (1 failed read)" || cells[1] != "-" {
		t.Errorf("the failed transaction-id read is not '-' with the mark: %q\n%s", cells, text)
	}
	if cells := rowCells(text, "no-window"); cells == nil || cells[0] != "no-window" || cells[1] != "-" {
		t.Errorf("the step with no resources: %q\n%s", cells, text)
	}
	if note := noteLine(text, "(1 failed read)"); !strings.Contains(note, "`resources.xid_read_error`") {
		t.Errorf("no note of the failed-reads mark names summary.json `resources.xid_read_error`:\n%s", text)
	}
	checkRowWidths(t, md)
}
