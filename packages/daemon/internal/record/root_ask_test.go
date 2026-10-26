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

// An ask's mirror row round-trips, a write older than the stored row changes nothing, a closed
// row keeps the turn it last had, and the batched read groups every issue's rows.
func TestARootAskMirrorKeepsItsNewestState(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	inTx(t, st, func(tx pgx.Tx) {
		putRoot(t, ctx, records, tx, "LEGION-1")
		putRoot(t, ctx, records, tx, "LEGION-2")
		newer := RootAsk{Issue: "LEGION-1", Ask: "ask-q", Kind: AskQuestion, Open: false, WaitingOn: "human", Seq: 12, ByTree: true, PersonSeq: 12,
			ClosedSeq: 12, ClosedBy: ClosedByPerson}
		must(t, records.PutRootAsk(ctx, tx, newer))
		must(t, records.PutRootAsk(ctx, tx, RootAsk{Issue: "LEGION-1", Ask: "ask-q", Kind: AskQuestion, Open: true, WaitingOn: "human", Seq: 10, ByTree: true}))
		got, err := records.RootAsk(ctx, tx, "LEGION-1", "ask-q")
		must(t, err)
		if got == nil || *got != newer {
			t.Fatalf("ask after an older write = %#v, want %#v kept", got, newer)
		}
		unknown := RootAsk{Issue: "LEGION-2", Ask: "ask-r", Open: true, WaitingOn: "agent", Seq: 4}
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

// A gate keeps the approval request its registration bound, through Gate and GatesForArtifact,
// and a gate bound to none reads back with none.
func TestAGateRoundTripsItsApprovalRequest(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	inTx(t, st, func(tx pgx.Tx) {
		putRoot(t, ctx, records, tx, "LEGION-1")
		bound := DesignGate{Issue: "LEGION-1", ArtifactID: "artifact-1", LatestVersion: 3, RequestAsk: "ask-approval"}
		must(t, records.PutGate(ctx, tx, bound))
		got, err := records.Gate(ctx, tx, "LEGION-1")
		must(t, err)
		if !reflect.DeepEqual(got, &bound) {
			t.Fatalf("gate = %#v, want %#v", got, bound)
		}
		forArtifact, err := records.GatesForArtifact(ctx, tx, "artifact-1")
		must(t, err)
		if !reflect.DeepEqual(forArtifact, []DesignGate{bound}) {
			t.Fatalf("gates for artifact = %#v, want %#v", forArtifact, []DesignGate{bound})
		}
		unbound := DesignGate{Issue: "LEGION-1", ArtifactID: "artifact-2", LatestVersion: 1}
		must(t, records.PutGate(ctx, tx, unbound))
		if got, err = records.Gate(ctx, tx, "LEGION-1"); err != nil || !reflect.DeepEqual(got, &unbound) {
			t.Fatalf("gate rebound to none = %#v, %v; want %#v", got, err, unbound)
		}
	})
}
