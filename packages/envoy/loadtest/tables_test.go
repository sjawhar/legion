package main

import (
	"encoding/json"
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
	for _, c := range []struct{ title, key string }{{"Commit-order lock", "commit_order_lock.errors"}, {"Ungranted locks", "ungranted_locks.errors"}} {
		if note := noteLine(section(md, c.title), "(<n> failed reads)"); !strings.Contains(note, "`"+c.key+"`") {
			t.Errorf("section %q has no note of the failed-reads mark that names summary.json `%s`:\n%s", c.title, c.key, md)
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

func TestTablesRenderEditorsAndRoomsPerStep(t *testing.T) {
	var old summary
	if err := json.Unmarshal([]byte(`{"config":{"step":"old"}}`), &old); err != nil {
		t.Fatal(err)
	}
	perUpdate, share, bytesPerS := 1.75, 0.25, 2048.0
	count, written, total := int64(4), int64(4), 31.2
	edited := summary{
		Config: summaryConfig{Step: "editors-1x"},
		Editors: &editorsSummary{
			Editors: 3, Docs: 2, SingleEditorDocs: []string{"LGA-0002"}, TargetPerS: 6, Sent: 1000, SentPerS: 5.6, Acknowledged: 990, Refused: 2,
			NotSent: map[string]int{"disconnected": 4}, Connects: 3, ConnectFailures: map[string]int{"http 503": 2}, Closes: map[string]int{"1006": 1},
			RelayMs: stat{N: 600, P50: 12, P99: 80}, Markers: 20, MarkersFound: 19, DurableMs: stat{N: 19, P50: 40, P99: 150},
		},
		Rooms: &roomsSummary{
			RoundTrips:  &roundTripsSummary{All: 9000, Room: 1750, RoomPerUpdate: &perUpdate},
			Connections: &roomConnections{Samples: 1800, Held: stat{N: 1800, P50: 2, P99: 9, Max: 12}, SharedPoolShare: &share},
			Batches:     &dispatchRooms{AppendBatches: 400, AppendedUpdates: 1000, AppendRoundTrips: 800, BatchSizes: map[string]uint64{"1": 100, "3-4": 50, "2": 250, "5-8": 0}},
			Versions:    versionsSummary{BytesPerS: &bytesPerS, Written: &written, InsertCount: &count, InsertTotalMs: &total, InsertMs: stat{N: 4, P50: 6.5, P99: 12}},
		},
	}
	restOnly := summary{
		Config: summaryConfig{Step: "rest-only"},
		Rooms:  &roomsSummary{RoundTripsNote: "stack.json names no proxy_stats port", ConnectionsNote: "the room sampler runs only in a step with editors", BatchesNote: notServed},
	}
	// A build whose /debug/vars answered 404: the note carries the response body, newline and "|"
	// included; the room sampler's one read failed.
	notFound := summary{
		Config: summaryConfig{Step: "rooms-404"},
		Rooms: &roomsSummary{RoundTripsNote: "stack.json names no proxy_stats port", Connections: &roomConnections{Errors: 1},
			BatchesNote: "not served by this build (http://127.0.0.1:16061/debug/vars: 404 404 page | not found\n)"},
	}
	md := renderTables([]stepData{{name: "old", sum: old}, {name: "editors-1x", sum: edited}, {name: "rest-only", sum: restOnly}, {name: "rooms-404", sum: notFound}})
	const notFoundCell = `not served by this build (http://127.0.0.1:16061/debug/vars: 404 404 page \| not found )`
	for _, c := range []struct{ title, want string }{
		{"Editors", "| old | - | - | - | - | - | - | - | - |\n"},
		{"Editors", "| editors-1x | 3 on 2 (1) | 6.0 / 5.6 | 1,000 / 990 / 2 | 4 (disconnected 4) | 12 / 80 | 40 / 150 (19 / 20) | 3 / 2 (http 503 2) | 1006: 1 |\n"},
		{"Editors", "| rest-only | - | - | - | - | - | - | - | - |\n"},
		{"Editors", "Documents with one editor, which have no relay latency: editors-1x: LGA-0002."},
		{"Document rooms", "| old | - | - | - | - | - | - | - | - | - |\n"},
		{"Document rooms", "| editors-1x | 1,750 / 9,000 | 1.75 | 2 / 9 / 12 | 25% | 400 / 1,000 / 800 | 1: 100, 2: 250, 3-4: 50 | 2.0 | 4 | 6.5 / 12.0 (4, 31) |\n"},
		{"Document rooms", "| rest-only | - | - | - | - | not served by this build | not served by this build | - | - | - |\n"},
		{"Document rooms", "rest-only: round trips not measured: stack.json names no proxy_stats port."},
		{"Document rooms", "\n| rooms-404 (1 failed reads) | - | - | - | - | " + notFoundCell + " | " + notFoundCell + " | - | - | - |\n"},
	} {
		if text := section(md, c.title); !strings.Contains(text, c.want) {
			t.Errorf("section %q lacks %q:\n%s", c.title, c.want, text)
		}
	}
	if note := noteLine(section(md, "Document rooms"), "(<n> failed reads)"); !strings.Contains(note, "`rooms.connections.errors`") {
		t.Errorf("the Document rooms table has no note of the failed-reads mark that names summary.json `rooms.connections.errors`:\n%s", md)
	}
	for _, step := range []string{"editors-1x (", "rest-only ("} {
		if strings.Contains(section(md, "Document rooms"), "| "+step) {
			t.Errorf("a step whose room sampler read every time, or never ran, is marked:\n%s", md)
		}
	}
	checkRowWidths(t, md)
}
