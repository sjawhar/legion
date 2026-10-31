package workflow

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A person who makes a root that already ran a child of a running tree changes nothing in Legion:
// no release of children and no admission sweep takes such a root in, so it stays a tree of its
// own. The daemon says so once per parent — on the issue, where the person reads it, and to the
// architect of the tree it was moved under, which would otherwise wait for a release it cannot
// make (dispatch://INFRA-714/spec R3). A later event under the same parent repeats nothing; a move
// under another live tree is told anew.
func TestARootThatRanIsToldOncePerParentThatItStaysItsOwnTree(t *testing.T) {
	pool := migratedPool(t)
	seedLiveRoot(t, pool, "LEGION-208", 0)
	seedLiveRoot(t, pool, "LEGION-300", 1)
	// The re-parented root: its own tree, finished and set back to todo, carrying the mark of the
	// run it stood in (record.Issue.TakenBack), which is what makes it a root no tree adopts.
	seedIssue(t, pool, record.Issue{Key: "LEGION-500", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-500", Project: "LEGION", Title: "a tree of its own", Phase: phase.Done, Generation: 2, Status: "todo",
		DispatchStatus: "todo", HandedOver: true, TakenBack: true, Rank: "Z", LastDispatchSeq: 4})
	engine := testEngine(config.DesignGateRootIssues, nil)

	reparent := func(id, parent string, seq int64) {
		t.Helper()
		applyMirrored(t, pool, engine, id, intake.DispatchIssue{Key: "LEGION-500", ResolvedRepo: "sjawhar/legion", Seq: seq,
			Type: "issue.updated", Status: "todo", Title: "a tree of its own", Parent: parent, Rank: "Z", HandedOver: true})
	}

	reparent("moved-under-208", "LEGION-208", 5)
	first := dispatchMessages(t, pool, "LEGION-500")
	if len(first) != 1 || !strings.Contains(first[0], "LEGION-208") || !strings.Contains(first[0], "stays a tree of its own") {
		t.Fatalf("the messages on the re-parented root = %q, want one saying it stays its own tree under LEGION-208", first)
	}
	if told := noticeReasons(t, pool, "LEGION-208"); len(told) != 1 || told[0] != first[0] {
		t.Fatalf("the notices to the tree of LEGION-208 = %q, want the one the issue was told", told)
	}
	if got := toldParent(t, pool, "LEGION-500"); got != "LEGION-208" {
		t.Fatalf("the parent recorded as told = %q, want LEGION-208", got)
	}

	// An edit of the issue that leaves it under the same parent says nothing new.
	reparent("edited-under-208", "LEGION-208", 6)
	if again := dispatchMessages(t, pool, "LEGION-500"); len(again) != 1 {
		t.Fatalf("the messages after a second event under the same parent = %q, want the first alone", again)
	}
	if told := noticeReasons(t, pool, "LEGION-208"); len(told) != 1 {
		t.Fatalf("the notices after a second event under the same parent = %q, want the first alone", told)
	}

	// A move under another live tree is another parent, and that tree's architect has not heard it.
	reparent("moved-under-300", "LEGION-300", 7)
	if moved := dispatchMessages(t, pool, "LEGION-500"); len(moved) != 2 || !strings.Contains(moved[1], "LEGION-300") {
		t.Fatalf("the messages after the move under LEGION-300 = %q, want a second naming LEGION-300", moved)
	}
	if told := noticeReasons(t, pool, "LEGION-300"); len(told) != 1 {
		t.Fatalf("the notices to the tree of LEGION-300 = %q, want one", told)
	}
	if got := toldParent(t, pool, "LEGION-500"); got != "LEGION-300" {
		t.Fatalf("the parent recorded as told = %q, want LEGION-300", got)
	}
	if got := treeOf(t, pool, "LEGION-500"); got != "LEGION-500" {
		t.Fatalf("the re-parented root is recorded in the tree of %s, want its own", got)
	}
}

// A root that never ran is adopted rather than refused, so it is told nothing: the release of
// children takes it into the tree, and a message saying it stays its own would be false.
func TestARootThatNeverRanIsToldNothingWhenItIsReparented(t *testing.T) {
	pool := migratedPool(t)
	seedLiveRoot(t, pool, "LEGION-208", 0)
	seedIssue(t, pool, record.Issue{Key: "LEGION-501", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-501", Project: "LEGION", Title: "a root waiting for a slot", Phase: phase.Admitted, Generation: 1,
		Status: "todo", DispatchStatus: "todo", HandedOver: true, Rank: "Z", LastDispatchSeq: 4})

	applyMirrored(t, pool, testEngine(config.DesignGateRootIssues, nil), "moved", intake.DispatchIssue{
		Key: "LEGION-501", ResolvedRepo: "sjawhar/legion", Seq: 5, Type: "issue.updated", Status: "todo",
		Title: "a root waiting for a slot", Parent: "LEGION-208", Rank: "Z", HandedOver: true})

	if got := dispatchMessages(t, pool, "LEGION-501"); len(got) != 0 {
		t.Fatalf("the messages on a queued root a person re-parented = %q, want none: the release adopts it", got)
	}
	if got := toldParent(t, pool, "LEGION-501"); got != "" {
		t.Fatalf("the parent recorded as told = %q, want none", got)
	}
}

// seedLiveRoot records a root of a live tree: admitted in its slot, its gate open, planning.
func seedLiveRoot(t *testing.T, pool *pgxpool.Pool, key string, slot int) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: key, Project: "LEGION", Title: key, Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "U"})
	seedGate(t, pool, record.DesignGate{Issue: key, ArtifactID: "artifact-" + key, LatestVersion: 1, ApprovedVersion: new(1)})
	seedSlot(t, pool, record.Slot{Issue: key, Index: slot, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
}

// dispatchMessages is the body of every message the daemon has queued for issue, oldest first.
func dispatchMessages(t *testing.T, pool *pgxpool.Pool, issue string) []string {
	t.Helper()
	return outboxStrings(t, pool, "select payload->>'body' from outbox where kind = 'dispatch_message' and issue = $1 order by id", issue)
}

// noticeReasons is the reason of every notice the daemon has queued for issue's architect.
func noticeReasons(t *testing.T, pool *pgxpool.Pool, issue string) []string {
	t.Helper()
	return outboxStrings(t, pool, "select payload->>'reason' from outbox where kind = 'notice' and issue = $1 order by id", issue)
}

func outboxStrings(t *testing.T, pool *pgxpool.Pool, query, issue string) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), query, issue)
	if err != nil {
		t.Fatalf("read the outbox of %s: %v", issue, err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatalf("scan an outbox row of %s: %v", issue, err)
		}
		got = append(got, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the outbox of %s: %v", issue, err)
	}
	return got
}

// toldParent is the parent the issue's record says it was told it stays its own tree under.
func toldParent(t *testing.T, pool *pgxpool.Pool, key string) string {
	t.Helper()
	var issue *record.Issue
	seedRecord(t, pool, func(tx pgx.Tx) error {
		var err error
		issue, err = record.NewStore().Issue(t.Context(), tx, key)
		return err
	})
	if issue == nil {
		t.Fatalf("%s is not recorded", key)
	}
	return issue.ToldParent
}

// treeOf is the tree the record puts key in.
func treeOf(t *testing.T, pool *pgxpool.Pool, key string) string {
	t.Helper()
	var issue *record.Issue
	seedRecord(t, pool, func(tx pgx.Tx) error {
		var err error
		issue, err = record.NewStore().Issue(t.Context(), tx, key)
		return err
	})
	if issue == nil {
		t.Fatalf("%s is not recorded", key)
	}
	return issue.Tree
}
