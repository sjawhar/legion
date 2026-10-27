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
}

// section is the text of the markdown section that title opens, up to the next section.
func section(md, title string) string {
	_, rest, _ := strings.Cut(md, "## "+title+"\n")
	text, _, _ := strings.Cut(rest, "\n## ")
	return text
}

func TestTablesRenderTheSamplersAndOldSummaries(t *testing.T) {
	var old summary
	if err := json.Unmarshal([]byte(`{"config":{"step":"old"},"resources":{"window_s":180}}`), &old); err != nil {
		t.Fatal(err)
	}
	xids := 412.3
	cur := summary{
		Config:    summaryConfig{Step: "new"},
		Events:    eventsSummary{DeliveryMsFromSent: stat{N: 3, P50: 120, P99: 900}},
		Resources: &resourcesSummary{XIDsPerS: &xids},
		SSE:       &sseSummary{Frames: 1200, OrderViolations: 2, FrameMsFromCreatedAt: stat{N: 1200, P50: 40, P99: 300}, FrameMsFromWriteResponse: stat{N: 400, P50: 5, P99: 60}},
		UngrantedLocks: ungrantedSummary{Samples: 100, ShareWithAny: 0.97, Groups: []ungrantedGroup{{
			LockType: "advisory", Target: "commit_order_lock", Mode: "ExclusiveLock", WaitersP50: 12, WaitersMax: 40, ShareWithWaiter: 0.96,
			TopPairs: []waitPair{{WaiterQuery: "select a | b", BlockerQuery: "", Count: 800}},
		}}},
	}
	md := renderTables([]stepData{{name: "old", sum: old}, {name: "new", sum: cur}})

	for _, c := range []struct{ title, old, cur string }{
		{"Delivery from sending, and SSE frames", "| old | - | - | - | - | - |", "| new | 120 / 900 | 40 / 300 | 5 / 60 | 1,200 | 2 |"},
		{"Ungranted locks", "| old | - | - | - |", `| new | 97% | advisory commit_order_lock ExclusiveLock 96%, 12 / 40 | select a \| b → (none) (800) |`},
		{"Transaction ids", "| old | - |", "| new | 412.3 |"},
	} {
		text := section(md, c.title)
		if !strings.Contains(text, c.old+"\n") || !strings.Contains(text, c.cur+"\n") {
			t.Errorf("section %q lacks %q or %q:\n%s", c.title, c.old, c.cur, text)
		}
	}
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
			Batches:     &roomBatches{AppendBatches: 400, AppendedUpdates: 1000, AppendRoundTrips: 800, BatchSizes: map[string]uint64{"1": 100, "3-4": 50, "2": 250, "5-8": 0}},
			Versions:    versionsSummary{BytesPerS: &bytesPerS, Written: &written, InsertCount: &count, InsertTotalMs: &total, InsertMs: stat{N: 4, P50: 6.5, P99: 12}},
		},
	}
	restOnly := summary{
		Config: summaryConfig{Step: "rest-only"},
		Rooms:  &roomsSummary{RoundTripsNote: "stack.json names no proxy_stats port", ConnectionsNote: "the room sampler runs only in a step with editors", BatchesNote: notServed},
	}
	md := renderTables([]stepData{{name: "old", sum: old}, {name: "editors-1x", sum: edited}, {name: "rest-only", sum: restOnly}})
	for _, c := range []struct{ title, want string }{
		{"Editors", "| old | - | - | - | - | - | - | - | - |\n"},
		{"Editors", "| editors-1x | 3 on 2 (1) | 6.0 / 5.6 | 1,000 / 990 / 2 | 4 (disconnected 4) | 12 / 80 | 40 / 150 (19 / 20) | 3 / 2 (http 503 2) | 1006: 1 |\n"},
		{"Editors", "| rest-only | - | - | - | - | - | - | - | - |\n"},
		{"Editors", "Documents with one editor, which have no relay latency: editors-1x: LGA-0002."},
		{"Document rooms", "| old | - | - | - | - | - | - | - | - | - |\n"},
		{"Document rooms", "| editors-1x | 1,750 / 9,000 | 1.75 | 2 / 9 / 12 | 25% | 400 / 1,000 / 800 | 1: 100, 2: 250, 3-4: 50 | 2.0 | 4 | 6.5 / 12.0 (4, 31) |\n"},
		{"Document rooms", "| rest-only | - | - | - | - | not served by this build | not served by this build | - | - | - |\n"},
		{"Document rooms", "rest-only: round trips not measured: stack.json names no proxy_stats port."},
	} {
		if text := section(md, c.title); !strings.Contains(text, c.want) {
			t.Errorf("section %q lacks %q:\n%s", c.title, c.want, text)
		}
	}
}
