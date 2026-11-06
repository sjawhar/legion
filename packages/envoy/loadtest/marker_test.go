package main

import (
	"regexp"
	"testing"
	"time"
)

// triggerPattern is the pattern that commit-trigger.sql extracts each new event's marker with:
// the argument of `substring(new.payload::text from '...')`.
func triggerPattern(t *testing.T) *regexp.Regexp {
	t.Helper()
	m := regexp.MustCompile(`substring\(new\.payload::text from '([^']+)'\)`).FindStringSubmatch(commitTriggerSQL)
	if m == nil {
		t.Fatal("commit-trigger.sql extracts no marker with substring(new.payload::text from '...')")
	}
	return regexp.MustCompile(m[1])
}

func TestMarkerRoundTripsOnlyForItsRun(t *testing.T) {
	tag := markerTag(42, 17)
	if tag != "lgm00000004217x" {
		t.Fatalf("markerTag(42, 17) = %q, want lgm00000004217x", tag)
	}
	if n, ok := parseMarker(tag, 42); !ok || n != 17 {
		t.Fatalf("parseMarker(%q, 42) = %d, %t; want 17", tag, n, ok)
	}
	for _, s := range []string{
		markerTag(43, 17),  // another run
		"lgm17x",           // a marker without a nonce
		"lgm000000042x",    // the nonce alone
		"lgm000000042017x", // a counter with a leading zero, which markerTag never writes
		"lgm000000042abcx", // not digits
		"lgm00000004217",   // no closing x
	} {
		if _, ok := parseMarker(s, 42); ok {
			t.Errorf("parseMarker(%q, 42) accepted it", s)
		}
	}
	// The trigger extracts the first match of its pattern in the payload: the whole marker.
	trigger := triggerPattern(t)
	if got := trigger.FindString(`{"body":"` + tag + ` reply: text"}`); got != tag {
		t.Fatalf("the trigger's pattern %s extracts %q, want %q", trigger, got, tag)
	}
	if n, ok := findMarker([]byte(markerTag(7, 5)+" "+tag), 42); !ok || n != 17 {
		t.Fatalf("findMarker = %d, %t; want 17, past the other run's marker", n, ok)
	}
}

// Two runs on one database without a reset: each wrote counters 1 and 2. The second run counts
// only its own commit rows.
func TestARunCountsOnlyItsOwnCommitRows(t *testing.T) {
	const first, second = 111111111, 222222222
	at := time.Unix(0, 1500*msNs)
	read := []commitTagRow{
		{EventID: 10, Type: "comment.created", Tag: markerTag(first, 1), At: &at},
		{EventID: 11, Type: "comment.created", Tag: markerTag(first, 2), At: &at},
		{EventID: 20, Type: "comment.created", Tag: markerTag(second, 1), At: &at},
	}
	commits := runCommits(read, second)
	if len(commits) != 1 || commits[0].EventID != 20 || commits[0].Marker != 1 || commits[0].CommitNs != at.UnixNano() {
		t.Fatalf("commits %+v, want only event 20, the second run's marker 1", commits)
	}
	writes := []writeRec{
		{Marker: 1, Kind: kindComment, Issue: "LGA-1", InWindow: true, Arrival: 1000 * msNs, Response: 1100 * msNs, Status: 201},
		{Marker: 2, Kind: kindComment, Issue: "LGA-1", InWindow: true, Arrival: 2000 * msNs, Response: 2100 * msNs, Status: 201},
	}
	got := joinDelivery(writes, commits, nil, map[string]int{"LGA-1": 1}, 10)
	if got.Offered != 2 || got.Committed != 1 {
		t.Fatalf("offered %d committed %d, want 2 and 1: the first run's marker 2 is not the second run's", got.Offered, got.Committed)
	}
}
