package main

import (
	"slices"
	"testing"
	"time"
)

func TestPlanMentionsDrawsDistinctSessionsOtherThanTheAuthor(t *testing.T) {
	if got := planMentions(7, 0, 100, 0, 3); got != nil {
		t.Fatalf("mention-frac 0 mentions %v", got)
	}
	mentioned := 0
	for marker := int64(1); marker <= 1000; marker++ {
		author := int(marker % 4)
		got := planMentions(marker, author, 4, 0.2, 3)
		if !slices.Equal(got, planMentions(marker, author, 4, 0.2, 3)) {
			t.Fatalf("marker %d draws differently twice", marker)
		}
		if got == nil {
			continue
		}
		mentioned++
		if len(got) != 3 || slices.Contains(got, author) {
			t.Fatalf("marker %d mentions %v, want 3 sessions other than %d", marker, got, author)
		}
		sorted := slices.Sorted(slices.Values(got))
		if len(slices.Compact(sorted)) != 3 {
			t.Fatalf("marker %d mentions a session twice: %v", marker, got)
		}
	}
	if mentioned < 150 || mentioned > 250 {
		t.Fatalf("%d of 1,000 comments mention sessions at mention-frac 0.2", mentioned)
	}
	if got := mentionTargets([]int{3, 12}); got[0]["target"] != "session:ses-lg-0003" || got[1]["target"] != "session:ses-lg-0012" {
		t.Fatalf("mention targets %v", got)
	}
}

func TestSummarizeMentionsSplitsTheWindowAndTheMinuteAfterTheRestart(t *testing.T) {
	restart := time.Unix(2000, 0)
	at := func(d time.Duration) int64 { return restart.Add(d).UnixNano() }
	ok := func(sent time.Duration, ms int64, mentions int) writeRec {
		return writeRec{Kind: kindComment, Mentions: mentions, InWindow: true, Sent: at(sent), Arrival: at(sent), Response: at(sent) + ms*msNs, Status: 201}
	}
	writes := []writeRec{
		ok(-30*time.Second, 40, 0),
		ok(-20*time.Second, 900, 3),
		ok(10*time.Second, 700, 3),
		ok(20*time.Second, 50, 0),
		{Kind: kindReply, Mentions: 3, InWindow: true, Sent: at(time.Second), Arrival: at(time.Second), Response: at(6 * time.Second), TransportErr: "connection refused"},
		ok(70*time.Second, 30, 0), // past the minute after the restart
		{Kind: kindEdit, InWindow: true, Sent: at(5 * time.Second), Arrival: at(5 * time.Second), Response: at(6 * time.Second), Status: 201}, // not a comment
		{Kind: kindAnchored, Mentions: 3, InWindow: false, Sent: at(-90 * time.Second), Arrival: at(-90 * time.Second), Response: at(-89 * time.Second), Status: 201},
	}
	got := summarizeMentions(writes, 0.2, 3, restart)

	if w := got.Window.With; w.Offered != 3 || w.OK2xx != 2 || w.WriteMs != (stat{N: 2, P50: 700, P90: 700, P99: 700, Max: 900}) {
		t.Fatalf("window with mentions %+v", w)
	}
	if w := got.Window.Without; w.Offered != 3 || w.OK2xx != 3 || w.WriteMs.P50 != 40 {
		t.Fatalf("window without mentions %+v", w)
	}
	if got.AfterRestart == nil {
		t.Fatal("no split after the restart")
	}
	if a := got.AfterRestart.With; a.Offered != 2 || a.OK2xx != 1 || a.WriteMs.Max != 700 {
		t.Fatalf("after restart with mentions %+v", a)
	}
	if a := got.AfterRestart.Without; a.Offered != 1 || a.WriteMs.P50 != 50 {
		t.Fatalf("after restart without mentions %+v", a)
	}
	if none := summarizeMentions(writes, 0.2, 3, time.Time{}); none.AfterRestart != nil {
		t.Fatal("a step with no restart has a split after it")
	}
}
