package delivery

import (
	"fmt"
	"testing"
	"time"
)

// One claim per pair per CommitPullReadStaleAfter, and the decision and the record are one step:
// the second claim of a pair is refused however soon it asks, and the claim one span later is
// granted. Another commit of the same repository, and the same commit of another, are their own
// pairs.
func TestACommitPullReadIsClaimedOncePerSpan(t *testing.T) {
	var reads CommitPullReads
	start := time.Date(2026, 11, 5, 7, 15, 8, 0, time.UTC)
	if !reads.Claim("acme/widgets", "abc", start) {
		t.Fatal("the first claim of a pair was refused, want it granted")
	}
	if reads.Claim("acme/widgets", "abc", start.Add(CommitPullReadStaleAfter-time.Nanosecond)) {
		t.Fatal("a claim inside the span was granted, want it refused")
	}
	if !reads.Claim("acme/widgets", "def", start) || !reads.Claim("acme/gadgets", "abc", start) {
		t.Fatal("a claim of another pair was refused, want each pair bounded on its own")
	}
	if !reads.Claim("acme/widgets", "abc", start.Add(CommitPullReadStaleAfter)) {
		t.Fatal("the claim one span later was refused, want it granted")
	}
}

// The sweep: a claim granted once the map has passed commitPullReadSweepAt drops every entry no
// claim can be refused by any more, so a process reading results for weeks holds the commits of
// the last CommitPullReadStaleAfter and not every commit it ever asked about. The entries inside
// the span stay, bound and all.
func TestAClaimSweepsTheEntriesNoBoundCanRefuse(t *testing.T) {
	var reads CommitPullReads
	start := time.Date(2026, 11, 5, 7, 15, 8, 0, time.UTC)
	for number := range commitPullReadSweepAt {
		if !reads.Claim("acme/widgets", fmt.Sprintf("%040x", number), start) {
			t.Fatalf("claim %d of a pair of its own was refused, want it granted", number)
		}
	}
	fresh := start.Add(CommitPullReadStaleAfter)
	if !reads.Claim("acme/widgets", "fresh", fresh) {
		t.Fatal("a claim of a pair nothing is held for was refused, want it granted")
	}
	if held := len(reads.reads); held != 1 {
		t.Fatalf("the sweep left %d entries, want the one claim inside the span", held)
	}
	if reads.Claim("acme/widgets", "fresh", fresh.Add(time.Minute)) {
		t.Fatal("the entry the sweep kept refused nothing, want the bound to stand")
	}
}
