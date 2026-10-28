package main

import (
	"strings"
	"testing"
)

// A step whose loadtest.event_commits could not be read (events.commits_error: a foreign run's
// database without the commit trigger, or an unreachable -pg) knows nothing about its commits,
// receipts or delivery: Committed is 0 because nothing was read, not because nothing committed.
// Its tables must not present that 0 as a measurement. INFRA-589 test round 4: a foreign run
// whose -pg named a database without the trigger wrote 31 writes with 2xx and 35 committed events,
// and TABLES.md showed 0.00 events/s committed, 0 / 0 receipts and (0 / n) for every kind, with
// exit 0.
func TestTablesDoNotShowUnreadCommitsAsZero(t *testing.T) {
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
	}}
	md := renderTables([]stepData{unread})

	var eventsRow string
	for line := range strings.SplitSeq(section(md, "Events, delivery and requests"), "\n") {
		if strings.HasPrefix(line, "| foreign-notrigger") {
			eventsRow = line
		}
	}
	if eventsRow == "" {
		t.Fatalf("no Events row for the step:\n%s", md)
	}
	cells := strings.Split(strings.TrimSuffix(strings.TrimPrefix(eventsRow, "| "), " |"), " | ")
	if len(cells) < 8 {
		t.Fatalf("Events row has %d cells:\n%s", len(cells), eventsRow)
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
}
