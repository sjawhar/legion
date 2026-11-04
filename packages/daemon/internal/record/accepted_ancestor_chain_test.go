package record

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// chainIssue is one member of the tree the chain tests are read over, on the repository every
// record carries.
func chainIssue(key, tree, parent string, stands phase.Phase, status string) Issue {
	issue := Issue{Key: key, Tree: tree, Project: "LEGION", Title: key, Phase: stands, Generation: 1,
		Status: status, DispatchStatus: status, Rank: key, HandedOver: true,
		Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
	if parent != "" {
		issue.Parent = ParentOf(parent)
	}
	return issue
}

// The hold under an accepted ancestor, where that ancestor is not the tree's root and a done
// parent stands between it and the held issue. The doors into the workflow walk the whole tree
// (AcceptedAncestorOf over Store.TreeIssues) and find it; the waiting line is read over the
// project's live records, which leave out an issue already done, so without the records above
// the pass (LineFacts.Parents) the walk stopped at the done parent, jumped to the root — which
// is at admitted and has accepted nothing — and offered the held issue a first-phase slot. The
// architect is told that issue waits until the merging ancestor closes, and OpenBeneath leaves
// it out of that ancestor's own wait, so starting it would merge a head without its work.
//
// The whole path is read here, from the records as the store holds them, because the defect was
// in which records the line's own pass holds and not in the walk over them.
func TestTheLineHoldsAnIssueUnderAnAcceptedAncestorAboveADoneParent(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	const (
		root        = "LEGION-900"
		merging     = "LEGION-901"
		doneParent  = "LEGION-902"
		heldAtAdmit = "LEGION-903"
	)
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, chainIssue(root, root, "", phase.Admitted, "in_progress")))
		must(t, records.PutIssue(ctx, tx, chainIssue(merging, root, root, phase.Merging, "needs_review")))
		must(t, records.PutIssue(ctx, tx, chainIssue(doneParent, root, merging, phase.Done, "done")))
		must(t, records.PutIssue(ctx, tx, chainIssue(heldAtAdmit, root, doneParent, phase.Admitted, "todo")))
		approved := 1
		must(t, records.PutGate(ctx, tx, DesignGate{Issue: root, ArtifactID: "spec", LatestVersion: 1, ApprovedVersion: &approved}))
		if _, err := treelifecycle.Open(ctx, tx, "legion", root, treelifecycle.AuthorityWorkflow); err != nil {
			t.Fatalf("open the tree of %s: %v", root, err)
		}
	})

	inTx(t, st, func(tx pgx.Tx) {
		issues, err := records.LiveIssues(ctx, tx, "LEGION")
		if err != nil {
			t.Fatalf("list the live issues: %v", err)
		}
		facts, err := ReadLineFacts(ctx, tx, records, "LEGION", issues)
		if err != nil {
			t.Fatalf("read the line's facts: %v", err)
		}
		if _, carried := facts.Parents[doneParent]; !carried {
			t.Fatalf("the pass's facts hold no record of %s, the done parent on the chain: the line cannot walk past it", doneParent)
		}
		// The control: with the records above the pass taken away, which is what the line read
		// before, the walk stops at the done parent and the line offers the held issue a slot.
		bare := facts
		bare.Parents = nil
		if !inLine(Waiting(issues, nil, bare), heldAtAdmit) {
			t.Fatalf("%s is out of the line even with no record of %s: this test proves nothing", heldAtAdmit, doneParent)
		}
		for _, entry := range Waiting(issues, nil, facts) {
			if entry.Issue.Key == heldAtAdmit {
				t.Fatalf("%s stands in the waiting line, though %s above it is merging: the daemon holds it until that ancestor closes", heldAtAdmit, merging)
			}
		}
		members, err := records.TreeIssues(ctx, tx, root)
		if err != nil {
			t.Fatalf("read the tree of %s: %v", root, err)
		}
		// The two readers answer alike, which is what keeps the hold and the merge from
		// deadlocking: the issue the line holds is one the merging ancestor does not wait for.
		if ancestor := AcceptedAncestorIn(members, root, heldAtAdmit, ""); ancestor != merging {
			t.Fatalf("the doors read the accepted ancestor of %s as %q, want %s", heldAtAdmit, ancestor, merging)
		}
		if open := OpenBeneath(mergingOf(members, merging), members); len(open) != 0 {
			t.Fatalf("the issues beneath %s that are still in the workflow = %v, want none: the one the daemon holds is left out", merging, open)
		}
	})
}

// mergingOf is the record of key among members.
func mergingOf(members []Issue, key string) Issue {
	for _, member := range members {
		if member.Key == key {
			return member
		}
	}
	return Issue{}
}

// inLine says whether key stands in the waiting line.
func inLine(line []LineEntry, key string) bool {
	for _, entry := range line {
		if entry.Issue.Key == key {
			return true
		}
	}
	return false
}

// A lane whose lease lapsed may be slow rather than dead, and every move it has left is matched
// on the token it holds: it renews by the token alone, and finishes the row by the id and the
// token. So the rewrite of a lapsed row takes the token with it. Left in place, that lane could
// renew, deliver the earlier task it had already read, and then finish the row now carrying the
// later one — deleting the later task with nothing to re-place it.
func TestReplacingALapsedOwedStartTakesItsLeaseToken(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	const issue = "LEGION-910"
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, issueFixture(issue)))
		must(t, records.Enqueue(ctx, tx, owedStartRow(t, issue, "Implement the change.")))
		if _, err := tx.Exec(ctx, `update outbox set lease_token = 'lane', lease_until = now() - interval '1 hour'
			where issue = $1 and kind = 'supervise'`, issue); err != nil {
			t.Fatalf("lapse the lease: %v", err)
		}
		replaced, err := records.ReplaceOwedStart(ctx, tx, owedStartRow(t, issue, "Implement the change again."))
		if err != nil {
			t.Fatalf("replace the owed start: %v", err)
		}
		if !replaced {
			t.Fatalf("ReplaceOwedStart left the lapsed row alone")
		}
		var token string
		if err := tx.QueryRow(ctx, `select lease_token from outbox where issue = $1 and kind = 'supervise'`, issue).Scan(&token); err != nil {
			t.Fatalf("read the rewritten row's lease token: %v", err)
		}
		if token == "lane" {
			t.Fatalf("the rewritten row still carries the lapsed lane's token: that lane can renew its lease, deliver the earlier task and delete the later one")
		}
		if token == "" {
			t.Fatalf("the rewritten row carries no token at all: a row that was leased keeps one, rotated, as the ready_merge conflict does")
		}
	})
}
