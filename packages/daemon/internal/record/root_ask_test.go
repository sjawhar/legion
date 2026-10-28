package record

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

func putRoot(t *testing.T, ctx context.Context, records Store, tx pgx.Tx, key string) {
	t.Helper()
	must(t, records.PutIssue(ctx, tx, Issue{Key: key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: key, Project: "LEGION", Title: key, Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "U"}))
}

// An ask's mirror row round-trips, a write older than the stored row on any of its fences or
// person's acts changes nothing, a closed row keeps the turn it last had, and the batched read
// groups every issue's rows.
func TestARootAskMirrorKeepsItsNewestState(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	inTx(t, st, func(tx pgx.Tx) {
		putRoot(t, ctx, records, tx, "LEGION-1")
		putRoot(t, ctx, records, tx, "LEGION-2")
		newer := RootAsk{Issue: "LEGION-1", Ask: "ask-q", Kind: AskApproval, Artifact: "artifact-1", Open: false, Seq: 12, WaitingOn: "human",
			WaitingOnSeq: 11, ClosedBy: ClosedByPerson, ClosedSeq: 12, ByTree: true, PersonSeq: 9, PersonAnswerSeq: 12}
		must(t, records.PutRootAsk(ctx, tx, newer))
		for _, older := range []RootAsk{
			{Issue: "LEGION-1", Ask: "ask-q", Kind: AskQuestion, Open: true, Seq: 10, WaitingOn: "human", WaitingOnSeq: 10, ByTree: true},
			{Issue: "LEGION-1", Ask: "ask-q", Seq: 13, WaitingOn: "agent", WaitingOnSeq: 10, ClosedBy: ClosedByPerson, ClosedSeq: 12, PersonSeq: 9, PersonAnswerSeq: 12},
			{Issue: "LEGION-1", Ask: "ask-q", Seq: 13, WaitingOn: "human", WaitingOnSeq: 11, ClosedBy: ClosedByOther, ClosedSeq: 12, PersonSeq: 8, PersonAnswerSeq: 12},
		} {
			must(t, records.PutRootAsk(ctx, tx, older))
		}
		got, err := records.RootAsk(ctx, tx, "LEGION-1", "ask-q")
		must(t, err)
		if got == nil || *got != newer {
			t.Fatalf("ask after older writes = %#v, want %#v kept", got, newer)
		}
		unknown := RootAsk{Issue: "LEGION-2", Ask: "ask-r", Open: true, Seq: 4, WaitingOn: "agent", WaitingOnSeq: 4}
		must(t, records.PutRootAsk(ctx, tx, unknown))
		all, err := records.RootAsks(ctx, tx, []string{"LEGION-1", "LEGION-2", "LEGION-3"})
		must(t, err)
		if want := map[string][]RootAsk{"LEGION-1": {newer}, "LEGION-2": {unknown}}; !reflect.DeepEqual(all, want) {
			t.Fatalf("asks = %#v, want %#v", all, want)
		}
		missing, err := records.RootAsk(ctx, tx, "LEGION-2", "ask-none")
		must(t, err)
		if missing != nil {
			t.Fatalf("an ask never mirrored = %#v, want none", missing)
		}
	})
}

// An approval request's answer is a review of its document: it answers no root waiting on the
// person when that document is the root's design gate's, and does for any other document or for a
// question. PersonSeq's acts count whatever the document.
func TestAnAnswerOfTheGatesDocumentIsNoPersonsAct(t *testing.T) {
	request := RootAsk{Kind: AskApproval, Artifact: "artifact-1", PersonSeq: 4, PersonAnswerSeq: 9}
	if !request.GateRequestOf("artifact-1") || request.PersonActSeq("artifact-1") != 4 {
		t.Fatalf("the gate's request = %t, act %d; want its answer left out, its other act at 4", request.GateRequestOf("artifact-1"), request.PersonActSeq("artifact-1"))
	}
	if request.GateRequestOf("artifact-2") || request.PersonActSeq("artifact-2") != 9 || request.PersonActSeq("") != 9 {
		t.Fatalf("another document's request acts at %d (no gate: %d), want its answer at 9", request.PersonActSeq("artifact-2"), request.PersonActSeq(""))
	}
	question := RootAsk{Kind: AskQuestion, PersonAnswerSeq: 9}
	if question.GateRequestOf("") || question.PersonActSeq("") != 9 {
		t.Fatalf("a question's answer acts at %d, want 9", question.PersonActSeq(""))
	}
}
