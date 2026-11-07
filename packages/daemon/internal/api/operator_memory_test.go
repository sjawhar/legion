package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/memorygroup"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The operator's spawn of a root draws its tree's memory group, in the transaction that creates
// the tree's lifecycle row, under the project's own mode and coin and at the instant its clock
// reads (Options.Now). The draw reaches the tree's root record where the daemon has one — a root
// the workflow recorded, which an operator spawned a session on — and writes nothing at all where
// it has none, the ordinary operator case: a tree the operator opens for work Dispatch never
// filed is not refused, and no issue record is invented for it.
func TestTheOperatorsSpawnDrawsTheMemoryGroupOfTheTreeItOpens(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	records := record.NewStore()
	drawnAt := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Steps:      config.DesignGateRootIssues.Steps(),
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
		Controller: h.store, Pool: h.store.Pool(), Record: records, Releaser: h.runtime, Trees: h.store,
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Memory: config.Memory{Mode: config.MemoryRandom}, MemoryCoin: func() (bool, error) { return true, nil },
		Now: func() time.Time { return drawnAt },
	}).Handler
	recorded := spawnBody()
	if err := h.store.Tx(ctx, func(tx pgx.Tx) error {
		return records.PutIssue(ctx, tx, record.Issue{
			Key: recorded.Tree, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Tree: recorded.Tree, Project: testDispatchProject, Title: recorded.Tree, Phase: phase.Admitted,
			Generation: 1, Status: "todo", Rank: "A", HandedOver: true,
		})
	}); err != nil {
		t.Fatalf("seed the root's record: %v", err)
	}

	if code := h.operator(http.MethodPost, "/legion/v1/operator/claims", recorded).Code; code != http.StatusCreated {
		t.Fatalf("spawn of a recorded root = %d, want 201", code)
	}

	drawn := readIssue(t, h, recorded.Tree)
	if drawn == nil {
		t.Fatalf("the spawn left no record of %s at all", recorded.Tree)
	}
	if drawn.MemoryGroup != string(memorygroup.GroupMemory) || drawn.MemoryDrawMode != string(config.MemoryRandom) ||
		drawn.MemoryDrawnAt == nil || !drawn.MemoryDrawnAt.Equal(drawnAt) {
		t.Fatalf("the spawned root carries group %q drawn as %q at %v, want the coin's %q drawn as %q at %s",
			drawn.MemoryGroup, drawn.MemoryDrawMode, drawn.MemoryDrawnAt, memorygroup.GroupMemory, config.MemoryRandom, drawnAt)
	}

	// A tree the daemon has no record of: the spawn opens it, draws nothing, and answers as it
	// does for any other tree.
	unrecorded := SpawnRequest{Tree: "LEGION-404", Issue: "LEGION-404", Role: claim.RoleArchitect, Prompt: "Reply ready and wait."}
	if code := h.operator(http.MethodPost, "/legion/v1/operator/claims", unrecorded).Code; code != http.StatusCreated {
		t.Fatalf("spawn of a tree with no record = %d, want 201", code)
	}
	if stored := readIssue(t, h, unrecorded.Tree); stored != nil {
		t.Fatalf("the spawn of an unrecorded tree wrote the record %+v, want none invented", *stored)
	}
	if started, _ := h.treeCleanup(unrecorded.Tree); started {
		t.Fatalf("the lifecycle of %s reads a reserved cleanup, want the tree opened", unrecorded.Tree)
	}
}

// readIssue is the record of key as the store holds it now, nil when the daemon has none.
func readIssue(t *testing.T, h *harness, key string) *record.Issue {
	t.Helper()
	var stored *record.Issue
	if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		var err error
		stored, err = record.NewStore().Issue(context.Background(), tx, key)
		return err
	}); err != nil {
		t.Fatalf("read the record of %s: %v", key, err)
	}
	return stored
}
