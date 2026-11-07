package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// A new version of an approved spec closes the tree's design gate, and what may merge is what the
// person's review of that version decides: the merger's start waits while the gate is closed and
// runs when it opens again (dispatch://INFRA-714/spec R5). The start is held, not dropped: the row
// stays on its backoff with nobody spawned, and the same row starts the merger once the approval
// lands.
func TestTheMergersStartWaitsWhileItsTreesDesignGateIsClosed(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: "LEGION-208", Title: "a reviewed root", Phase: phase.Merging, Generation: 1, Status: "retro"}
	putOutboxIssue(t, pool, records, issue)
	// The gate as a new version leaves it: version 4 with the person's approval still at 3.
	approved := 3
	putOutboxGate(t, pool, records, record.DesignGate{Issue: issue.Key, ArtifactID: "artifact-208", LatestVersion: 4, ApprovedVersion: &approved})
	sup, runtime := newOutboxSupervisor(t, "legion", t.TempDir())
	runner := &outbox{log: quietLogger(), dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup,
		trees: outboxTreeStore(t, pool, issue.Tree), tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
		},
	}
	row := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleMerger,
		Generation: issue.Generation, Phase: phase.Merging, Task: "Merge it."}, time.Now())

	err := runner.execute(context.Background(), row)
	if !errors.Is(err, wait.ErrWaiting) {
		t.Fatalf("the merger's start under a closed gate = %v, want it held (wait.ErrWaiting)", err)
	}
	if spawns := len(runtime.CallsOf("Spawn")); spawns != 0 {
		t.Fatalf("the runtime spawned %d sessions while the gate was closed, want none", spawns)
	}

	// The person approves version 4, which opens the gate.
	approvedNow := 4
	putOutboxGate(t, pool, records, record.DesignGate{Issue: issue.Key, ArtifactID: "artifact-208", LatestVersion: 4, ApprovedVersion: &approvedNow})

	if err := runner.execute(context.Background(), row); err != nil {
		t.Fatalf("the merger's start once the gate opened = %v, want it run", err)
	}
	if spawns := len(runtime.CallsOf("Spawn")); spawns != 1 {
		t.Fatalf("the runtime spawned %d sessions once the gate opened, want the merger's one", spawns)
	}
}

// putOutboxGate records a tree's design gate.
func putOutboxGate(t *testing.T, pool *pgxpool.Pool, records record.Store, gate record.DesignGate) {
	t.Helper()
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return records.PutGate(context.Background(), tx, gate)
	}); err != nil {
		t.Fatalf("put the design gate of %s: %v", gate.Issue, err)
	}
}
