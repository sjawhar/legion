package record

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// A root's wait round-trips whole, and leaving awaiting_person takes it with the phase. The two
// sequences the decision rests on are StartHandout's, Declare's and ClearDeclared's alone: an
// issue written from a copy read before any of them keeps what the record holds.
func TestAWaitRoundTripsAndOnlyItsOwnWritersMoveTheSequences(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	at := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	inTx(t, st, func(tx pgx.Tx) {
		putRoot(t, ctx, records, tx, "LEGION-1")
		must(t, records.PutDispatchIssue(ctx, tx, DispatchIssueRow{Key: "LEGION-1", Project: "LEGION", Status: "in_progress", Seq: 3,
			ClaimedBy: "session:ses_architect", ClaimSeq: 9}))
		must(t, records.PutRootAsk(ctx, tx, RootAsk{Issue: "LEGION-1", Ask: "ask-q", Kind: AskQuestion, Open: true, Seq: 11,
			WaitingOn: "human", WaitingOnSeq: 11, ByTree: true}))
		// A gate's request only a version's turn reached holds its highest sequence in its turn.
		must(t, records.PutRootAsk(ctx, tx, RootAsk{Issue: "LEGION-1", Ask: "ask-approval", Kind: AskApproval, Artifact: "artifact-1",
			WaitingOn: "agent", WaitingOnSeq: 12}))

		// The newest hand-back's time is kept: an older one, or the same one read again with no
		// time, leaves it.
		must(t, records.Declare(ctx, tx, "LEGION-1", EventMark{Seq: 11, At: at}))
		must(t, records.Declare(ctx, tx, "LEGION-1", EventMark{Seq: 7, At: at.Add(-time.Hour)}))
		must(t, records.Declare(ctx, tx, "LEGION-1", EventMark{Seq: 11}))
		stored, err := records.Issue(ctx, tx, "LEGION-1")
		must(t, err)
		if stored.DeclaredSeq != 11 || !stored.DeclaredAt.Equal(at) {
			t.Fatalf("declared = %d at %s, want the newest hand-back 11 at %s", stored.DeclaredSeq, stored.DeclaredAt, at)
		}

		waiting := *stored
		waiting.Phase = phase.AwaitingPerson
		waiting.Awaiting = &Awaiting{Ask: "ask-q", AnsweredAt: &at, AnsweredBy: AnsweredByAsk, AnsweredDetail: "ask-q",
			Since: EventMark{Seq: 11, At: at}}
		must(t, records.PutIssue(ctx, tx, waiting))
		read, err := records.Issue(ctx, tx, "LEGION-1")
		must(t, err)
		if read.Awaiting == nil || read.Awaiting.Ask != "ask-q" || read.Awaiting.AnsweredBy != AnsweredByAsk ||
			read.Awaiting.AnsweredDetail != "ask-q" || read.Awaiting.Since.Seq != 11 {
			t.Fatalf("awaiting = %+v, want the wait as it was written", read.Awaiting)
		}
		if read.Awaiting.AnsweredAt == nil || !read.Awaiting.AnsweredAt.Equal(at) || !read.Awaiting.Since.At.Equal(at) {
			t.Fatalf("awaiting times = %+v, want %s", read.Awaiting, at)
		}
		if !read.AwaitingPerson() || !read.Answered() {
			t.Fatalf("issue %+v, want it an answered awaiting root", read)
		}

		// The handout takes the highest sequence the record holds of the issue, every fence of its
		// asks included, and clears what the architect handed back; a stale copy written over it
		// moves neither.
		must(t, records.StartHandout(ctx, tx, "LEGION-1"))
		after, err := records.Issue(ctx, tx, "LEGION-1")
		must(t, err)
		if after.HandoutSeq != 12 || after.DeclaredSeq != 0 {
			t.Fatalf("handout_seq = %d, declared_seq = %d, want 12 and nothing declared", after.HandoutSeq, after.DeclaredSeq)
		}
		must(t, records.PutIssue(ctx, tx, waiting))
		kept, err := records.Issue(ctx, tx, "LEGION-1")
		must(t, err)
		if kept.HandoutSeq != 12 || kept.DeclaredSeq != 0 {
			t.Fatalf("handout_seq = %d, declared_seq = %d after a stale copy, want them kept", kept.HandoutSeq, kept.DeclaredSeq)
		}
		// A newer claim the claims mirror holds is the highest the record holds at the next handout.
		must(t, records.PutDispatchClaim(ctx, tx, "LEGION-1", "user:alice", 15))
		must(t, records.StartHandout(ctx, tx, "LEGION-1"))
		if claimed, err := records.Issue(ctx, tx, "LEGION-1"); err != nil || claimed.HandoutSeq != 15 {
			t.Fatalf("handout after a claim at 15 = %+v, %v; want 15", claimed, err)
		}
		// The claim Unresumable reads is the claims mirror's, a claim seen before the issue's first
		// mirrored row included.
		must(t, records.PutDispatchClaim(ctx, tx, "LEGION-2", "user:alice", 4))
		if holder, err := records.DispatchClaim(ctx, tx, "LEGION-2"); err != nil || holder != "user:alice" {
			t.Fatalf("claim of an issue with no mirrored row = %q, %v; want user:alice", holder, err)
		}

		must(t, records.Declare(ctx, tx, "LEGION-1", EventMark{Seq: 13, At: at}))
		must(t, records.ClearDeclared(ctx, tx, "LEGION-1"))
		cleared, err := records.Issue(ctx, tx, "LEGION-1")
		must(t, err)
		if cleared.DeclaredSeq != 0 || !cleared.DeclaredAt.IsZero() {
			t.Fatalf("declared = %d at %s, want it cleared", cleared.DeclaredSeq, cleared.DeclaredAt)
		}

		// A design-changes notice takes in the hand-backs at or before its review, and keeps a
		// newer one applied first.
		must(t, records.Declare(ctx, tx, "LEGION-1", EventMark{Seq: 20, At: at}))
		must(t, records.TakeInThrough(ctx, tx, "LEGION-1", 18))
		kept20, err := records.Issue(ctx, tx, "LEGION-1")
		must(t, err)
		if kept20.DeclaredSeq != 20 || !kept20.DeclaredAt.Equal(at) || kept20.HandoutSeq != 18 {
			t.Fatalf("after a notice of a review at 18: declared %d at %s, handout %d; want 20 kept, handout 18",
				kept20.DeclaredSeq, kept20.DeclaredAt, kept20.HandoutSeq)
		}
		must(t, records.TakeInThrough(ctx, tx, "LEGION-1", 20))
		if taken, err := records.Issue(ctx, tx, "LEGION-1"); err != nil || taken.DeclaredSeq != 0 || !taken.DeclaredAt.IsZero() || taken.HandoutSeq != 20 {
			t.Fatalf("after a notice of a review at 20: %+v, %v; want nothing declared and handout 20", taken, err)
		}
		must(t, records.ClearDeclared(ctx, tx, "LEGION-1"))

		// Leaving the phase takes the wait with it.
		admitted := *cleared
		admitted.Phase, admitted.Awaiting = phase.Admitted, nil
		must(t, records.PutIssue(ctx, tx, admitted))
		gone, err := records.Issue(ctx, tx, "LEGION-1")
		must(t, err)
		if gone.Awaiting != nil || gone.AwaitingPerson() {
			t.Fatalf("awaiting = %+v, want it gone with the phase", gone.Awaiting)
		}
	})
}

// A hand-back with no sequence cannot be placed, and is refused rather than recorded.
func TestDeclareRefusesAHandBackWithNoSequence(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	inTx(t, st, func(tx pgx.Tx) {
		putRoot(t, ctx, records, tx, "LEGION-1")
		if err := records.Declare(ctx, tx, "LEGION-1", EventMark{At: time.Now()}); err == nil {
			t.Fatal("a hand-back with no sequence was recorded, want it refused")
		}
	})
}

// The waiting line puts every root a person answered ahead of every root that never ran, oldest
// answer first, and leaves out a root still waiting on a person, whatever Dispatch shows it as.
func TestAnAnsweredRootWaitsAheadOfEveryRootThatNeverRan(t *testing.T) {
	early := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	late := early.Add(time.Hour)
	waiting := func(key, rank string, answered *time.Time, awaiting bool) Issue {
		issue := Issue{Key: key, Tree: key, Project: "LEGION", Title: key, Phase: phase.Admitted, Generation: 1,
			Status: "todo", Rank: rank, HandedOver: true}
		if awaiting {
			issue.Phase, issue.Status = phase.AwaitingPerson, "in_progress"
			issue.Awaiting = &Awaiting{Ask: "ask-q", AnsweredAt: answered, Since: EventMark{Seq: 5}}
		}
		return issue
	}
	issues := []Issue{
		waiting("LEGION-1", "A", nil, false),
		waiting("LEGION-2", "B", &late, true),
		waiting("LEGION-3", "C", &early, true),
		waiting("LEGION-4", "D", nil, true),
	}
	var got []string
	for _, issue := range Waiting(issues, nil) {
		got = append(got, issue.Key)
	}
	want := []string{"LEGION-3", "LEGION-2", "LEGION-1"}
	if len(got) != len(want) {
		t.Fatalf("waiting = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("waiting = %v, want %v", got, want)
		}
	}
}

// A root waiting on a person returned its slot and is in no line, yet its tree stays live
// (TreeLive), so LiveRoots names it: a todo root whose Dispatch chain reaches it stays held under
// that tree's unreleased child (the ancestry hold) rather than taking the slot the wait returned.
func TestARootWaitingOnAPersonKeepsTheAncestryHold(t *testing.T) {
	awaiting := Issue{Key: "LEGION-4", Tree: "LEGION-4", Project: "LEGION", Title: "LEGION-4", Phase: phase.AwaitingPerson, Generation: 1,
		Status: "in_progress", Rank: "A", HandedOver: true, Awaiting: &Awaiting{Ask: "ask-q", Since: EventMark{Seq: 5}}}
	parent := "LEGION-5"
	held := Issue{Key: "LEGION-6", Tree: "LEGION-6", Project: "LEGION", Title: "LEGION-6", Phase: phase.Admitted, Generation: 1,
		Status: "todo", Rank: "B", HandedOver: true, Parent: &parent, Ancestry: []string{parent, awaiting.Key}}
	issues := []Issue{awaiting, held}
	if _, live := LiveRoots(issues, nil)[awaiting.Key]; !live {
		t.Fatalf("live roots = %v, want %s's tree live while it waits on a person", LiveRoots(issues, nil), awaiting.Key)
	}
	if waiting := Waiting(issues, nil); len(waiting) != 0 {
		t.Fatalf("waiting = %v, want %s held under the tree of %s", waiting, held.Key, awaiting.Key)
	}
	until := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	awaiting.LingerUntil = &until
	if _, live := LiveRoots([]Issue{awaiting}, nil)[awaiting.Key]; live {
		t.Fatalf("a lingering root waiting on a person is live, want it not live, as TreeLive reads it")
	}
}
