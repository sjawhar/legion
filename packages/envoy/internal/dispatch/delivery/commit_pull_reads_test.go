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
	if held := len(reads.notBefore); held != 1 {
		t.Fatalf("the sweep left %d entries, want the one claim inside the span", held)
	}
	if reads.Claim("acme/widgets", "fresh", fresh.Add(time.Minute)) {
		t.Fatal("the entry the sweep kept refused nothing, want the bound to stand")
	}
}

// A settled pair is refused every claim until its span ends, and the sweep keeps it that long.
// GitHub has named a pull request for that commit whose head is another commit
// (ReadThroughCommitPulls' atHead), so it has answered about this commit for now: a claim inside
// CommitPullSettledFor buys the same answer again, and the sweep, which drops the moments no
// claim can be refused by, must not drop one that refuses every claim yet. Once the span has
// passed the pair is a pair like any other — a pull request opened later can have its head at
// that commit — so the claim is granted and the sweep drops what is left.
func TestASettledCommitIsRefusedUntilItsSpanEndsAndTheSweepKeepsItUntilThen(t *testing.T) {
	var reads CommitPullReads
	start := time.Date(2026, 11, 5, 7, 15, 8, 0, time.UTC)
	if !reads.Claim("acme/widgets", "abc", start) || !reads.Claim("acme/widgets", "def", start) {
		t.Fatal("the first claim of a pair was refused, want it granted")
	}
	reads.Settle("acme/widgets", "abc", start)
	if reads.Claim("acme/widgets", "abc", start.Add(CommitPullSettledFor-time.Nanosecond)) {
		t.Fatal("a claim of a settled pair inside its span was granted, want it refused")
	}
	// Enough pairs to pass commitPullReadSweepAt, then one claim to run the sweep over them: the
	// claims' own moments have passed and go, and the settled pair's stands and stays.
	for number := range commitPullReadSweepAt {
		if !reads.Claim("acme/widgets", fmt.Sprintf("%040x", number), start) {
			t.Fatalf("claim %d of a pair of its own was refused, want it granted", number)
		}
	}
	if !reads.Claim("acme/widgets", "fresh", start.Add(CommitPullReadStaleAfter)) {
		t.Fatal("a claim of a pair nothing is held for was refused, want it granted")
	}
	if reads.Claim("acme/widgets", "abc", start.Add(CommitPullReadStaleAfter)) {
		t.Fatal("the swept pair was claimed again, want the settling kept: GitHub has answered about it")
	}
	// A pair nothing settled is bounded by the claim's span alone, so the settling refuses its
	// own pair and no other.
	if !reads.Claim("acme/widgets", "def", start.Add(CommitPullReadStaleAfter)) {
		t.Fatal("a claim of an unsettled pair one span later was refused, want it granted")
	}
	// Past the settled span the pair is read through again, and the sweep drops what the
	// settling left: nothing here is held for the life of the process.
	ended := start.Add(CommitPullSettledFor)
	if !reads.Claim("acme/widgets", "abc", ended) {
		t.Fatal("a claim of the settled pair once its span had passed was refused, want it granted")
	}
	reads.Settle("acme/widgets", "ghi", start)
	for number := range commitPullReadSweepAt {
		if !reads.Claim("acme/widgets", fmt.Sprintf("%040x", number), ended) {
			t.Fatalf("claim %d past the settled span was refused, want it granted", number)
		}
	}
	if _, held := reads.notBefore[commitPullKey{repo: "acme/widgets", sha: "ghi"}]; held {
		t.Fatal("the sweep kept a settled pair whose span had passed, want it dropped with the rest")
	}
}
