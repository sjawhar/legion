package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// acceptPhase moves the root of the status harness's tree into at, the phase that makes it an
// ancestor the daemon has already accepted.
func acceptPhase(t *testing.T, h *harness, key string, at phase.Phase) {
	t.Helper()
	if _, err := h.store.Pool().Exec(context.Background(), "update issues set phase = $2 where key = $1", key, string(at)); err != nil {
		t.Fatalf("move %s into %s: %v", key, at, err)
	}
}

// A6. rerun_child is refused while an ancestor of the child has already been accepted: an issue
// entering the workflow beneath a head on its way to the merge queue would let that head merge
// without carrying it. The refusal names the ancestor, and is lifted once that ancestor closes.
func TestRerunChildIsRefusedUnderAnAcceptedAncestor(t *testing.T) {
	for _, at := range []phase.Phase{phase.Retro, phase.Merging, phase.AwaitingMerge} {
		t.Run(string(at), func(t *testing.T) {
			h, _, _ := statusHarness(t)
			architect := newLiveClaim(t, h, statusRoot, claim.RoleArchitect)
			if recorder := h.request(http.MethodPost, "/legion/v1/children/park", map[string]any{"grantId": architect.grant(t), "issue": statusChild}, nil); recorder.Code != http.StatusOK {
				t.Fatalf("park = %d %s", recorder.Code, recorder.Body)
			}
			acceptPhase(t, h, statusRoot, at)

			recorder := h.request(http.MethodPost, "/legion/v1/children/rerun", map[string]any{"grantId": architect.grant(t), "issue": statusChild}, nil)
			if recorder.Code != http.StatusConflict {
				t.Fatalf("rerun under a root in %s = %d %s, want 409", at, recorder.Code, recorder.Body)
			}
			body := recorder.Body.String()
			if !strings.Contains(body, "CHILD_UNDER_ACCEPTED_PARENT") || !strings.Contains(body, statusRoot) {
				t.Fatalf("rerun refusal = %s, want CHILD_UNDER_ACCEPTED_PARENT naming %s", body, statusRoot)
			}

			// Once the ancestor has closed there is no order left to keep, and the rerun runs.
			acceptPhase(t, h, statusRoot, phase.Done)
			if recorder := h.request(http.MethodPost, "/legion/v1/children/rerun", map[string]any{"grantId": architect.grant(t), "issue": statusChild}, nil); recorder.Code != http.StatusOK {
				t.Fatalf("rerun once the ancestor closed = %d %s, want 200", recorder.Code, recorder.Body)
			}
		})
	}
}

// A6. release_children is refused the same way, for a child Dispatch holds in backlog that the
// daemon has never recorded: the chain is read from the Dispatch parent the release itself read,
// and it is walked past a parent already done, so a grandchild under a done parent is refused by
// whichever issue above it is still merging.
func TestWaveReleaseIsRefusedForAGrandchildUnderADoneParent(t *testing.T) {
	h, _, statuses := statusHarness(t)
	architect := newLiveClaim(t, h, statusRoot, claim.RoleArchitect)
	acceptPhase(t, h, statusRoot, phase.AwaitingMerge)
	acceptPhase(t, h, statusChild, phase.Done)
	const grandchild = "LEGION-300"
	parent := statusChild
	statuses.issues[grandchild] = dispatch.Issue{Key: grandchild, ResolvedRepo: "acme/widgets", Title: "grandchild",
		Status: "backlog", Parent: &parent, Rank: "C", LastSeq: 7}

	recorder := h.request(http.MethodPost, "/legion/v1/waves/release", map[string]any{"grantId": architect.grant(t), "issues": []string{grandchild}}, nil)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("release of a grandchild under a merged root = %d %s, want 409", recorder.Code, recorder.Body)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "CHILD_UNDER_ACCEPTED_PARENT") || !strings.Contains(body, statusRoot) || !strings.Contains(body, grandchild) {
		t.Fatalf("release refusal = %s, want CHILD_UNDER_ACCEPTED_PARENT naming %s and %s", body, grandchild, statusRoot)
	}
	if len(statuses.writes) != 0 {
		t.Fatalf("Dispatch writes = %#v, want none: the whole wave is refused before anything is written", statuses.writes)
	}
	if recorded := recordedChild(t, h, grandchild); recorded != nil {
		t.Fatalf("the refused grandchild = %#v, want it unrecorded", recorded)
	}

	// With the root back before acceptance the same release is taken.
	acceptPhase(t, h, statusRoot, phase.Implementing)
	if recorder := h.request(http.MethodPost, "/legion/v1/waves/release", map[string]any{"grantId": architect.grant(t), "issues": []string{grandchild}}, nil); recorder.Code != http.StatusOK {
		t.Fatalf("release once the root is back in implementing = %d %s, want 200", recorder.Code, recorder.Body)
	}
}

// A6. The refusal is the daemon's own reading of its records, so the ancestor it names is the
// one record.AcceptedAncestor finds over the tree: the nearest issue above the child in retro,
// merging or awaiting_merge, found by walking past a parent that is already done.
func TestTheAcceptedAncestorIsFoundByWalkingThePartlyDoneChain(t *testing.T) {
	h, _, _ := statusHarness(t)
	acceptPhase(t, h, statusRoot, phase.Merging)
	acceptPhase(t, h, statusChild, phase.Done)
	parent := statusChild
	h.recordIssue(record.Issue{Key: "LEGION-300", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Tree: statusRoot, Parent: &parent, Project: testDispatchProject, Title: "grandchild", Phase: phase.Admitted,
		Generation: 1, Status: "todo", DispatchStatus: "todo", Rank: "C", LastDispatchSeq: 7})

	var ancestor string
	if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		var err error
		ancestor, err = record.AcceptedAncestorOf(context.Background(), record.NewStore(), tx, statusRoot, "LEGION-300", "")
		return err
	}); err != nil {
		t.Fatalf("read the accepted ancestor: %v", err)
	}
	if ancestor != statusRoot {
		t.Fatalf("the accepted ancestor of the grandchild = %q, want %s", ancestor, statusRoot)
	}
}
