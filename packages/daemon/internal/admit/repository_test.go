package admit

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

func rootTodo(key string, seq int64, resolved string) intake.DispatchIssue {
	return intake.DispatchIssue{Key: key, Seq: seq, Type: "issue.updated", Status: "todo", Title: key, Rank: "A", HandedOver: handed, ResolvedRepo: resolved}
}

// Admission never records a root from a retained event whose repository nobody read
// (ResolvedRepoMissing): the event is refused, so it is retried once the consumer's preparation
// reads it, and the root is not admitted on a guess. A recorded root's retained event is applied
// with the record's repository.
func TestARetainedRootEventIsNeverAdmittedWithoutItsRepository(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	retained := rootTodo("LEGION-208", 1, "")
	retained.ResolvedRepoMissing = true
	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "retained", retained, admission.engine, admission); err == nil ||
		!strings.Contains(err.Error(), "predates Dispatch's resolved_repo") {
		t.Fatalf("applying a retained root event = %v, want it refused for retry", err)
	}
	if got := maybeIssue(t, pool, "LEGION-208"); got != nil {
		t.Fatalf("LEGION-208 recorded as %#v, want it not admitted", got)
	}
	apply(t, pool, admission, "root-todo", rootTodo("LEGION-208", 2, "acme/widgets"), admission.engine)
	edited := rootTodo("LEGION-208", 3, "")
	edited.Title, edited.ResolvedRepoMissing = "renamed", true
	apply(t, pool, admission, "retained-rename", edited, admission.engine)
	if got := issue(t, pool, "LEGION-208"); got.Repo != "acme/widgets" || got.Title != "renamed" {
		t.Fatalf("LEGION-208 = %#v, want the retained rename applied on acme/widgets", got)
	}
	for _, effect := range effects(t, pool) {
		if post, ok := effect.payload.(record.MessagePost); ok {
			t.Fatalf("a repository note was posted: %q", post.Body)
		}
	}
}

// A root handed to Legion in todo is recorded on the repository it resolves to, at that
// repository's default branch, whatever the branch is named; a live event and the boot listing
// record it the same way.
func TestARootIsRecordedOnItsResolvedRepositoryAndItsDefaultBranch(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	apply(t, pool, admission, "root-todo", rootTodo("LEGION-208", 1, "acme/tools"), admission.engine)
	reconcile(t, pool, admission, []dispatch.IssueSummary{
		{Key: "LEGION-209", Title: "LEGION-209", Status: "todo", Rank: "B", HandedOver: handed, ResolvedRepo: "acme/widgets"},
	})
	for key, want := range map[string][2]string{"LEGION-208": {"acme/tools", "release/v7.1"}, "LEGION-209": {"acme/widgets", "main"}} {
		got := issue(t, pool, key)
		if got.Repo != want[0] || got.DefaultBranch != want[1] || got.Phase != phase.Admitted {
			t.Fatalf("%s = %#v, want it admitted on %s at %s", key, got, want[0], want[1])
		}
	}
}

// A root whose repository resolves to none, or to one Legion does not serve, is not admitted, and
// the daemon says why on the issue once for that resolution: a repeated event and the boot listing
// say nothing again, and once its repository resolves to one Legion serves, it is admitted.
func TestARootOnARepositoryLegionDoesNotServeIsNotAdmittedAndSaysWhy(t *testing.T) {
	for _, tc := range []struct {
		name, resolved, want string
	}{
		{"none", "", "Legion did not admit this issue: its repository resolves to none. Set its repository (the issue header's Repository picker, or dispatch issue-update --repo owner/name) and move it to todo again."},
		{"left out", "acme/gadgets", "Legion did not admit this issue: the review App is not installed on acme/gadgets. Install it on acme/gadgets, or set another repository (the issue header's Repository picker, or dispatch issue-update --repo owner/name), and move the issue to todo again."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
			apply(t, pool, admission, "root-todo", rootTodo("LEGION-208", 1, tc.resolved), admission.engine)
			apply(t, pool, admission, "root-edited", rootTodo("LEGION-208", 2, tc.resolved), admission.engine)
			reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{
				{Key: "LEGION-208", Title: "LEGION-208", Status: "todo", Rank: "A", HandedOver: handed, LastSeq: 2, ResolvedRepo: tc.resolved},
			}, 10, 10, true)
			if got := maybeIssue(t, pool, "LEGION-208"); got != nil {
				t.Fatalf("LEGION-208 recorded as %#v, want it not admitted", got)
			}
			assertEffects(t, pool, []effect{{kind: record.OutboxKindDispatchMessage, issue: "LEGION-208", payload: record.MessagePost{Body: tc.want}}})

			apply(t, pool, admission, "root-repository-set", rootTodo("LEGION-208", 3, "acme/widgets"), admission.engine)
			if got := issue(t, pool, "LEGION-208"); got.Repo != "acme/widgets" || got.Status != "in_progress" {
				t.Fatalf("LEGION-208 once its repository is set = %#v, want it admitted on acme/widgets", got)
			}
		})
	}
}

// An admitted root keeps the repository it was recorded with: a later event, or the boot listing,
// resolving it to another is not applied, which the daemon says once for that resolution, and a
// re-admission of the root keeps the recorded repository and default branch.
func TestARecordedRootKeepsItsRepositoryAcrossAnEditAndAReadmission(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Linger: time.Hour, Repositories: testRepositories, Clock: func() time.Time { return fixedNow }}, nil)
	until := fixedNow.Add(time.Hour)
	putIssue(t, pool, record.Issue{Key: "LEGION-LINGER", Repo: "acme/tools", DefaultBranch: "release/v7.1", GitHubRepo: "acme/tools", Project: testProject, Title: "lingering", Tree: "LEGION-LINGER",
		Phase: phase.Done, Generation: 3, Status: "done", DispatchStatus: "done", Rank: "A", LingerUntil: &until, LastDispatchSeq: 1, HandedOver: true})

	moved := rootTodo("LEGION-LINGER", 2, "acme/widgets")
	moved.Status = "done"
	apply(t, pool, admission, "repository-edited", moved, engine)
	reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{
		{Key: "LEGION-LINGER", Title: "LEGION-LINGER", Status: "done", Rank: "A", HandedOver: handed, LastSeq: 2, ResolvedRepo: "acme/widgets"},
	}, 10, 10, true)
	apply(t, pool, admission, "readmitted", rootTodo("LEGION-LINGER", 3, "acme/widgets"), engine)

	got := issue(t, pool, "LEGION-LINGER")
	if got.Generation != 4 || got.Repo != "acme/tools" || got.DefaultBranch != "release/v7.1" {
		t.Fatalf("re-admitted root = %#v, want generation 4 on acme/tools at release/v7.1", got)
	}
	var notes []string
	for _, effect := range effects(t, pool) {
		if post, ok := effect.payload.(record.MessagePost); ok && strings.HasPrefix(post.Body, "Legion keeps") {
			notes = append(notes, post.Body)
		}
	}
	want := "Legion keeps acme/tools as this issue's repository; the change to acme/widgets in Dispatch is not applied — an issue's repository is fixed when Legion records it. Create a new issue for acme/widgets instead."
	if len(notes) != 1 || notes[0] != want {
		t.Fatalf("repository notes = %q, want one saying %q", notes, want)
	}
}
