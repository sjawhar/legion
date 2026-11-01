package admit

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The operator can run a tree of its own under the key of a root waiting for its slot: the root's
// promotion waits on the operator's lifetime (TestAnOperatorTreeHoldsOnlyItsOwnWorkflowRootsAdmission).
// That tree's claims and pod carry the root's tokens and names, so when the waiting root leaves the
// workflow — a person parks it, or someone moves it to a running tree's status and admission releases
// it — the workflow's record goes to done and nothing acts on the operator's tree: no suspend, no
// IssueSuspend, no linger close. Set back to todo with the label, the root waits again behind the
// operator's lifetime.
func TestAWaitingRootLeavingTheWorkflowLeavesTheOperatorsTreeUnderItsKeyAlone(t *testing.T) {
	for _, tc := range []struct{ name, actor, status string }{
		{name: "a person's backlog", status: "backlog"},
		{name: "an outside session's in_progress", actor: "ses-outsider", status: "in_progress"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
			engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
			seedSlotted(t, pool, "LEGION-1", "A")
			seedWaiting(t, pool, "LEGION-OPS", "B")
			if _, err := pool.Exec(context.Background(), `insert into tree_lifecycles (project, tree, epoch, authority) values ('legion', 'LEGION-OPS', 1, 'operator')`); err != nil {
				t.Fatal(err)
			}

			apply(t, pool, admission, "moved", intake.DispatchIssue{Key: "LEGION-OPS", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: tc.status, Title: "LEGION-OPS", Rank: "B", HandedOver: true, ActorSession: tc.actor}, engine)
			if got := issue(t, pool, "LEGION-OPS"); got.Phase != phase.Done || got.LingerUntil != nil || got.Status != tc.status {
				t.Fatalf("LEGION-OPS after the %s = %#v, want it out of the workflow at phase done, not lingering", tc.status, got)
			}
			for _, effect := range effects(t, pool) {
				if effect.issue != "LEGION-OPS" {
					continue
				}
				switch payload := effect.payload.(type) {
				case record.SuperviseRequest, record.IssueSuspend, record.LingerClose:
					t.Fatalf("queued %T %#v for LEGION-OPS, want nothing that acts on the operator's tree", payload, payload)
				}
			}
			var authority string
			var cleanup bool
			if err := pool.QueryRow(context.Background(), `select authority, cleanup_started from tree_lifecycles where project = 'legion' and tree = 'LEGION-OPS'`).Scan(&authority, &cleanup); err != nil {
				t.Fatal(err)
			}
			if authority != "operator" || cleanup {
				t.Fatalf("LEGION-OPS's lifecycle = %s, cleanup started %t; want the operator's, untouched", authority, cleanup)
			}

			// The running tree finishes, freeing its slot; then the root is set back to todo with the
			// label: it waits again, behind the operator's lifetime, and starts nothing.
			apply(t, pool, admission, "first-done", intake.DispatchIssue{Key: "LEGION-1", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "done", Title: "LEGION-1", Rank: "A", HandedOver: true}, engine)
			apply(t, pool, admission, "queued-again", intake.DispatchIssue{Key: "LEGION-OPS", ResolvedRepo: "acme/widgets", Seq: 3, Type: "issue.updated", Status: "todo", Title: "LEGION-OPS", Rank: "B", HandedOver: true}, engine)
			if got := issue(t, pool, "LEGION-OPS"); got.Phase != phase.Admitted || got.Generation != 2 {
				t.Fatalf("LEGION-OPS set back to todo = %#v, want it admitted at generation 2", got)
			}
			assertSlots(t, pool, nil)
			assertWaiting(t, pool, []string{"LEGION-OPS"})
			if started := architectStarts(t, pool, "LEGION-OPS"); started != 0 {
				t.Fatalf("architect starts for LEGION-OPS = %d, want none while the operator holds its tree", started)
			}
		})
	}
}
