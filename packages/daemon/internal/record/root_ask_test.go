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
		newer := RootAsk{Issue: "LEGION-1", Ask: "ask-q", Kind: AskQuestion, Open: false, Seq: 12, WaitingOn: "human", WaitingOnSeq: 11,
			ClosedBy: ClosedByPerson, ClosedSeq: 12, ByTree: true, GateRequest: true, PersonSeq: 9, PersonAnswerSeq: 12}
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

// A gate keeps the approval request its registration bound and that request's document, through
// Gate and GatesForArtifact, and a gate bound to none reads back with none. The request is the
// gate's only while its document is the gate's (BoundRequest).
func TestAGateRoundTripsItsApprovalRequest(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	inTx(t, st, func(tx pgx.Tx) {
		putRoot(t, ctx, records, tx, "LEGION-1")
		bound := DesignGate{Issue: "LEGION-1", ArtifactID: "artifact-1", LatestVersion: 3, RequestAsk: "ask-approval", RequestArtifact: "artifact-1"}
		must(t, records.PutGate(ctx, tx, bound))
		got, err := records.Gate(ctx, tx, "LEGION-1")
		must(t, err)
		if !reflect.DeepEqual(got, &bound) || got.BoundRequest() != "ask-approval" {
			t.Fatalf("gate = %#v, want %#v bound to ask-approval", got, bound)
		}
		forArtifact, err := records.GatesForArtifact(ctx, tx, "artifact-1")
		must(t, err)
		if !reflect.DeepEqual(forArtifact, []DesignGate{bound}) {
			t.Fatalf("gates for artifact = %#v, want %#v", forArtifact, []DesignGate{bound})
		}
		elsewhere := DesignGate{Issue: "LEGION-1", ArtifactID: "artifact-2", LatestVersion: 1, RequestAsk: "ask-approval", RequestArtifact: "artifact-1"}
		if elsewhere.BoundRequest() != "" {
			t.Fatalf("gate of artifact-2 naming artifact-1's request binds %q, want none", elsewhere.BoundRequest())
		}
		unbound := DesignGate{Issue: "LEGION-1", ArtifactID: "artifact-2", LatestVersion: 1}
		must(t, records.PutGate(ctx, tx, unbound))
		if got, err = records.Gate(ctx, tx, "LEGION-1"); err != nil || !reflect.DeepEqual(got, &unbound) {
			t.Fatalf("gate rebound to none = %#v, %v; want %#v", got, err, unbound)
		}
	})
}
