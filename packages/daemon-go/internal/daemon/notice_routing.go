package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// errNoticeWaits is a notice held for its architect: the architect holds no role while its claim
// can hold it again, or an earlier notice held for the same architect has not gone yet, and a later
// one must not overtake it. The row is tried again on the outbox's backoff, logged once.
var errNoticeWaits = errors.New("the notice waits for its architect")

// errNoticeUnroutable is a notice whose architect cannot be resolved from the record: its issue is
// not recorded, has no tree or no recorded root, or its keys make no claim token. readNoticeTree
// decides it, once, when it reads the tree. Retrying cannot change the record and the outbox has
// no dead-letter path, so the row finishes undelivered with one log line.
var errNoticeUnroutable = errors.New("the notice has no architect to go to")

// treeSnapshot is one read of a notice's tree: its project token, its root, every issue of the tree
// whose key makes a claim token, by key, and the tree's unfinished notices written before the row,
// oldest first. The owner walk, the fence, and the linger check all see this one state, so a
// reparent committing meanwhile cannot show them half of itself. An issue whose key makes no claim
// token owns nothing, so it is left out: a walk that meets it ends at the root, and an earlier
// notice of it holds nothing back.
type treeSnapshot struct {
	project string
	root    record.Issue
	issues  map[string]record.Issue
	earlier []record.OutboxRow
}

// readNoticeTree reads row's tree in one repeatable-read transaction, and returns row's issue as
// the tree's own issue set holds it. A notice it cannot route is errNoticeUnroutable.
func (r *outbox) readNoticeTree(ctx context.Context, row record.OutboxRow) (record.Issue, treeSnapshot, error) {
	var issue record.Issue
	tree := treeSnapshot{issues: map[string]record.Issue{}}
	if err := pgx.BeginTxFunc(ctx, r.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if !claim.IsIssueKey(row.Issue) {
			return fmt.Errorf("%w: %q is not an issue key", errNoticeUnroutable, row.Issue)
		}
		recorded, err := r.records.Issue(ctx, tx, row.Issue)
		if err != nil {
			return err
		}
		if recorded == nil {
			return fmt.Errorf("%w: workflow issue %s is not recorded", errNoticeUnroutable, row.Issue)
		}
		if recorded.Tree == "" {
			return fmt.Errorf("%w: issue %s has no tree root", errNoticeUnroutable, row.Issue)
		}
		if tree.project, err = claim.ProjectToken(recorded.Project); err != nil {
			return fmt.Errorf("%w: %w", errNoticeUnroutable, err)
		}
		members, err := r.records.TreeIssues(ctx, tx, recorded.Tree)
		if err != nil {
			return err
		}
		for _, member := range members {
			if claim.IsIssueKey(member.Key) {
				tree.issues[member.Key] = member
			}
		}
		var ok bool
		if tree.root, ok = tree.issues[recorded.Tree]; !ok {
			return fmt.Errorf("%w: the root %s of %s is not recorded", errNoticeUnroutable, recorded.Tree, row.Issue)
		}
		issue = tree.issues[row.Issue]
		tree.earlier, err = r.records.EarlierNotices(ctx, tx, recorded.Tree, row.ID)
		return err
	}); err != nil {
		return record.Issue{}, treeSnapshot{}, fmt.Errorf("read the tree of notice row %d: %w", row.ID, err)
	}
	return issue, tree, nil
}

// owningArchitect is the architect claim that owns a notice of kind about issue, by the TypeScript
// daemon's rule (owningArchitect, packages/daemon/src/daemon/legion-state.ts): the nearest issue at
// or above it, through its parents, whose sub-architect claim runs (runs; one the operator started,
// and not suspended: the workflow suspends a child's sub-architect when the child leaves, and
// nothing starts it again for a notice), else the tree root, whose architect is the tree's own
// claim. A child's close or status change starts at its parent (reduceIssueClosed and
// reduceChildStatus, packages/daemon/src/daemon/reducers.ts): the child's own sub-architect is
// suspended with it (workflow's leave), and the architect above it decides what the rest of the
// tree does. The walk stays inside the issue's tree: admission records a Dispatch re-parent as it
// comes and leaves the issue's tree as it was (admit's recordObservation), so a parent that is not
// an issue of tree — not recorded, or recorded in another tree — ends the walk at the tree root.
func owningArchitect(project string, tree map[string]record.Issue, issue record.Issue, kind record.NoticeKind, runs func(claim.Token) bool) (claim.Token, error) {
	root := func() (claim.Token, error) { return claim.NewToken(project, issue.Tree, claim.RoleArchitect) }
	current := issue
	if (kind == "child-closed" || kind == "child-status") && issue.Parent != nil && !claim.IsTreeRoot(issue.Key, issue.Tree) {
		parent, ok := tree[*issue.Parent]
		if !ok {
			return root()
		}
		current = parent
	}
	seen := map[string]bool{}
	for {
		if claim.IsTreeRoot(current.Key, current.Tree) {
			return root()
		}
		token, err := claim.NewToken(project, current.Key, claim.RoleArchitect)
		if err != nil {
			return "", err
		}
		if runs(token) {
			return token, nil
		}
		seen[current.Key] = true
		if current.Parent == nil || seen[*current.Parent] {
			return root()
		}
		parent, ok := tree[*current.Parent]
		if !ok {
			return root()
		}
		current = parent
	}
}

// earlierNoticeFor is the id of the oldest of tree's earlier notices that goes to architect as the
// claims run now, or 0 when there is none. Each earlier notice's architect is resolved again rather
// than recorded when it was held, since a sub-architect can start or end while it waits. An earlier
// row whose architect cannot be resolved is no architect's, so it holds back no later notice; it
// finishes on its own attempt (notice).
func earlierNoticeFor(tree treeSnapshot, architect claim.Token, runs func(claim.Token) bool) int64 {
	for _, other := range tree.earlier {
		payload, err := record.DecodeOutboxPayload(other)
		if err != nil {
			continue
		}
		notice, ok := payload.(record.Notice)
		if !ok {
			continue
		}
		issue, ok := tree.issues[other.Issue]
		if !ok {
			continue
		}
		if owner, err := owningArchitect(tree.project, tree.issues, issue, notice.Kind, runs); err == nil && owner == architect {
			return other.ID
		}
	}
	return 0
}

// claimRuns is whether a claim in state runs, or is on its way back to running: it has not ended,
// and it is not suspended.
func claimRuns(state supervise.ClaimState) bool {
	return !claimEnded(state) && state != supervise.StateSuspended
}

// claimEnded is whether a claim in state will not hold its role again: this daemon does not
// supervise it (""), or it failed or retired. A suspended claim has not ended: the operator's
// resume or deliver starts it again (`legion claims resume`).
func claimEnded(state supervise.ClaimState) bool {
	return state == "" || state == supervise.StateFailed || state == supervise.StateRetired
}

// claimState is the state of token's claim, or "" when this daemon does not supervise it.
func (r *outbox) claimState(token claim.Token) supervise.ClaimState {
	machine, ok := r.supervisor.Machine(token)
	if !ok {
		return ""
	}
	return machine.Claim().State
}

// architectEnded says why nobody will hold the role of the owning architect, in state, for a
// notice of tree, or "" when its claim can hold the role again: a lingering or closed tree's
// architect is suspended until re-admission, which starts it with the tree's record rather than the
// notices of its close.
func architectEnded(tree treeSnapshot, state supervise.ClaimState) string {
	if tree.root.Lingers() {
		return "its tree lingers or has closed"
	}
	if claimEnded(state) {
		return "its claim has failed, retired, or is not supervised"
	}
	return ""
}
