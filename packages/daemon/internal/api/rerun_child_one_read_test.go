package api

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// countingRecords counts what a route reads of the records: the issues it reads one at a time
// (Store.Issue) and the trees it reads whole (Store.TreeIssues).
type countingRecords struct {
	record.Store
	issues atomic.Int64
	trees  atomic.Int64
}

func (c *countingRecords) Issue(ctx context.Context, tx pgx.Tx, key string) (*record.Issue, error) {
	c.issues.Add(1)
	return c.Store.Issue(ctx, tx, key)
}

func (c *countingRecords) TreeIssues(ctx context.Context, tx pgx.Tx, tree string) ([]record.Issue, error) {
	c.trees.Add(1)
	return c.Store.TreeIssues(ctx, tx, tree)
}

// rerun_child judges the tree from one read of it (treeMembers), as release_children judges a
// whole wave: those records carry the root whose linger stops a rerun and the chain the accepted
// ancestor is found on (record.AcceptedAncestorIn). It read the root by itself and then the tree
// again through a helper of its own, so one request's two judgements stood on two snapshots of
// the tree and a helper no other route had a use for.
func TestRerunChildJudgesTheTreeFromOneRead(t *testing.T) {
	h, handlers, statuses := statusHarness(t)
	counting := &countingRecords{Store: record.NewStore()}
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, DispatchProject: testDispatchProject,
		Settings: operatorSettings(), Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(),
		Record: counting, Handlers: handlers, Dispatch: statuses,
	}).Handler
	architect := newLiveClaim(t, h, statusRoot, claim.RoleArchitect)
	if recorder := h.request(http.MethodPost, "/legion/v1/children/park", map[string]any{"grantId": architect.grant(t), "issue": statusChild}, nil); recorder.Code != http.StatusOK {
		t.Fatalf("park = %d %s", recorder.Code, recorder.Body)
	}
	acceptPhase(t, h, statusRoot, phase.AwaitingMerge)

	issues, trees := counting.issues.Load(), counting.trees.Load()
	recorder := h.request(http.MethodPost, "/legion/v1/children/rerun", map[string]any{"grantId": architect.grant(t), "issue": statusChild}, nil)
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "CHILD_UNDER_ACCEPTED_PARENT") {
		t.Fatalf("rerun under a root awaiting its merge = %d %s, want 409 CHILD_UNDER_ACCEPTED_PARENT", recorder.Code, recorder.Body)
	}
	// The child's own record, which authorizes the request, and the one read of its tree.
	if read, treeReads := counting.issues.Load()-issues, counting.trees.Load()-trees; read != 1 || treeReads != 1 {
		t.Fatalf("the refused rerun read %d issues one at a time and %d whole trees, want 1 and 1: the tree it is refused by is read once", read, treeReads)
	}
}
