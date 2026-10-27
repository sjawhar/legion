package admit

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The grandchild's key sorts before its parent's, so neither the listing's order nor the keys'
// records the parent first.
const bootRoot, bootChild, bootGrandchild = "LEGION-1", "LEGION-20", "LEGION-12"

// seedLiveTree records bootRoot as a running tree: slotted, implementing, its design gate approved.
func seedLiveTree(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedSlotted(t, pool, bootRoot, "A")
	approved := 1
	inTx(t, pool, func(tx pgx.Tx) {
		records, ctx := record.NewStore(), context.Background()
		root, err := records.Issue(ctx, tx, bootRoot)
		if err != nil {
			t.Fatalf("read root: %v", err)
		}
		root.Phase, root.LastDispatchSeq = phase.Implementing, 1
		for _, put := range []error{
			records.PutIssue(ctx, tx, *root),
			records.PutGate(ctx, tx, record.DesignGate{Issue: bootRoot, ArtifactID: "7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26", LatestVersion: 1, ApprovedVersion: &approved}),
		} {
			if put != nil {
				t.Fatalf("seed the live tree: %v", put)
			}
		}
	})
}

// bootListing is Dispatch's listing at boot: the running root, a child its architect released to
// todo, and that child's own todo child, listed before its parent. Neither child has a record.
func bootListing(labelled bool) []dispatch.IssueSummary {
	root, child := bootRoot, bootChild
	return []dispatch.IssueSummary{
		{Key: bootGrandchild, ResolvedRepo: "acme/widgets", Title: "grandchild", Status: "todo", Parent: &child, Rank: "C", LastSeq: 8},
		{Key: bootRoot, ResolvedRepo: "acme/widgets", Title: bootRoot, Status: "in_progress", Rank: "A", HandedOver: true, LastSeq: 1},
		{Key: bootChild, ResolvedRepo: "acme/widgets", Title: "child", Status: "todo", Parent: &root, Rank: "B", HandedOver: labelled, LastSeq: 7},
	}
}

// echoed is the record and outbox the two children's own todo events leave, applied live through
// intake as the Dispatch consumer applies them, from the same tree: what the boot read must leave.
func echoed(t *testing.T, labelled bool) (record.Issue, record.Issue, []effect) {
	t.Helper()
	pool := migratedPool(t)
	admission := newAdmissionWithGate(t, 2, config.DesignGateRootIssues, slog.New(slog.NewTextHandler(io.Discard, nil)))
	seedLiveTree(t, pool)
	apply(t, pool, admission, "child-todo", intake.DispatchIssue{Key: bootChild, ResolvedRepo: "acme/widgets", Seq: 7, Type: "issue.updated",
		Status: "todo", Title: "child", Parent: bootRoot, Rank: "B", HandedOver: labelled}, admission.engine)
	apply(t, pool, admission, "grandchild-todo", intake.DispatchIssue{Key: bootGrandchild, ResolvedRepo: "acme/widgets", Seq: 8, Type: "issue.updated",
		Status: "todo", Title: "grandchild", Parent: bootChild, Rank: "C"}, admission.engine)
	return issue(t, pool, bootChild), issue(t, pool, bootGrandchild), effects(t, pool)
}

// A child its architect released to todo whose own todo event recorded nothing — the daemon stopped
// before it applied the event, or Dispatch linked the child to its parent only after it — is
// recorded into its running tree by the boot read, from the parent the listing shows, exactly as
// that event would have recorded it; so is its own todo child, whichever the listing shows first.
// The boot read holds both while the Dispatch consumer is behind and records them on the release.
// A labelled child of a running tree is recorded into it too, as its event would, not admitted as a
// tree of its own.
func TestTheBootReadRecordsATodoChildOfALiveTreeThatHasNoRecord(t *testing.T) {
	for _, tc := range []struct {
		name     string
		labelled bool
		behind   bool
	}{
		{name: "consumer caught up"},
		{name: "consumer behind", behind: true},
		{name: "labelled child", labelled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantChild, wantGrandchild, wantEffects := echoed(t, tc.labelled)
			if wantChild.Tree != bootRoot || wantChild.Phase != phase.Planning || wantGrandchild.Tree != bootRoot || wantGrandchild.Phase != phase.Planning {
				t.Fatalf("the live events recorded %#v and %#v, want both planning in %s", wantChild, wantGrandchild, bootRoot)
			}

			pool := migratedPool(t)
			admission := newAdmissionWithGate(t, 2, config.DesignGateRootIssues, slog.New(slog.NewTextHandler(io.Discard, nil)))
			seedLiveTree(t, pool)
			if tc.behind {
				reconcileWithPosition(t, pool, admission, bootListing(tc.labelled), 10, 5, false)
				if got := maybeIssue(t, pool, bootChild); got != nil {
					t.Fatalf("%s recorded while the consumer is behind = %#v, want it held for the consumer", bootChild, got)
				}
				if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "position-reached", intake.DispatchConsumerPosition{AckFloorStream: 10}, engineStub{}, admission); err != nil {
					t.Fatalf("ApplyFact position-reached: %v", err)
				}
			} else {
				reconcileWithPosition(t, pool, admission, bootListing(tc.labelled), 10, 10, true)
			}

			for key, want := range map[string]record.Issue{bootChild: wantChild, bootGrandchild: wantGrandchild} {
				got := maybeIssue(t, pool, key)
				if got == nil {
					t.Fatalf("%s after the boot read has no record, want it recorded in %s as its todo event records it: %#v", key, bootRoot, want)
				}
				if !reflect.DeepEqual(*got, want) {
					t.Fatalf("%s after the boot read = %#v, want the record its todo event leaves, %#v", key, *got, want)
				}
			}
			if got := effects(t, pool); !reflect.DeepEqual(got, wantEffects) {
				t.Fatalf("outbox after the boot read = %#v, want what the two todo events queue, %#v", got, wantEffects)
			}
			assertSlots(t, pool, []record.Slot{{Issue: bootRoot, Index: 0, AdmittedAt: fixedNow}})
		})
	}
}

// A labelled todo child whose parent is in no running tree is still an orphan at boot: the
// workflow records nothing of it, and admission records it as a root of its own, as before.
func TestTheBootReadStillAdmitsALabelledOrphanAsItsOwnRoot(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmissionWithGate(t, 2, config.DesignGateRootIssues, slog.New(slog.NewTextHandler(io.Discard, nil)))
	parent := "LEGION-GONE"
	reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{
		{Key: bootChild, ResolvedRepo: "acme/widgets", Title: "orphan", Status: "todo", Parent: &parent, Rank: "B", HandedOver: true, LastSeq: 7},
	}, 10, 10, true)
	if got := issue(t, pool, bootChild); got.Tree != bootChild || got.Parent == nil || *got.Parent != parent {
		t.Fatalf("orphan after the boot read = %#v, want a root of its own under parent %s", got, parent)
	}
	assertSlots(t, pool, []record.Slot{{Issue: bootChild, Index: 0, AdmittedAt: fixedNow}})
}
