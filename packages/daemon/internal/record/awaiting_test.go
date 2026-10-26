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
		must(t, records.PutRootAsk(ctx, tx, RootAsk{Issue: "LEGION-1", Ask: "ask-q", Kind: AskQuestion, Open: true, WaitingOn: "human",
			ByTree: true, Seq: 11}))

		must(t, records.Declare(ctx, tx, "LEGION-1", 11))
		must(t, records.Declare(ctx, tx, "LEGION-1", 7))
		stored, err := records.Issue(ctx, tx, "LEGION-1")
		must(t, err)
		if stored.DeclaredSeq != 11 {
			t.Fatalf("declared_seq = %d, want the newest hand-back 11", stored.DeclaredSeq)
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

		// The handout takes the highest sequence the record holds of the issue, and clears what the
		// architect handed back; a stale copy written over it moves neither.
		must(t, records.StartHandout(ctx, tx, "LEGION-1"))
		after, err := records.Issue(ctx, tx, "LEGION-1")
		must(t, err)
		if after.HandoutSeq != 11 || after.DeclaredSeq != 0 {
			t.Fatalf("handout_seq = %d, declared_seq = %d, want 11 and nothing declared", after.HandoutSeq, after.DeclaredSeq)
		}
		must(t, records.PutIssue(ctx, tx, waiting))
		kept, err := records.Issue(ctx, tx, "LEGION-1")
		must(t, err)
		if kept.HandoutSeq != 11 || kept.DeclaredSeq != 0 {
			t.Fatalf("handout_seq = %d, declared_seq = %d after a stale copy, want them kept", kept.HandoutSeq, kept.DeclaredSeq)
		}

		must(t, records.Declare(ctx, tx, "LEGION-1", 13))
		must(t, records.ClearDeclared(ctx, tx, "LEGION-1"))
		cleared, err := records.Issue(ctx, tx, "LEGION-1")
		must(t, err)
		if cleared.DeclaredSeq != 0 {
			t.Fatalf("declared_seq = %d, want it cleared", cleared.DeclaredSeq)
		}

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
		if err := records.Declare(ctx, tx, "LEGION-1", 0); err == nil {
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
