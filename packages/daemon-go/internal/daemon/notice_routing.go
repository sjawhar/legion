package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// noticeSummary is the one-line message a notice is published with, "<kind> on <issue>": the
// summary the listener shows and echoes back in a role-lane exception, which parseNoticeSummary
// reads.
func noticeSummary(kind record.NoticeKind, issue string) string {
	return string(kind) + noticeSummarySeparator + issue
}

// parseNoticeSummary is the kind and issue noticeSummary wrote into summary, and whether it did.
func parseNoticeSummary(summary string) (record.NoticeKind, string, bool) {
	kind, issue, found := strings.Cut(summary, noticeSummarySeparator)
	return record.NoticeKind(kind), issue, found && kind != "" && issue != ""
}

const noticeSummarySeparator = " on "

// errNoticeWaits is a notice held for its architect: the architect holds no role while its claim
// can hold it again, or an earlier notice held for the same architect has not gone yet, and a later
// one must not overtake it. The row is tried again on the outbox's backoff, logged once.
var errNoticeWaits = errors.New("the notice waits for its architect")

// errNoticeUnroutable is a notice whose architect cannot be resolved from the record: its issue is
// not recorded, has no tree or no recorded root, or its keys make no claim token. readNoticeRoute
// decides it, once, when it reads the tree. Retrying cannot change the record and the outbox has
// no dead-letter path, so the row finishes undelivered with one log line.
var errNoticeUnroutable = errors.New("the notice has no architect to go to")

// noticeRoute is what routes a notice of a tree: the tree's project token, its root, and every
// issue of the tree whose key makes a claim token, by key. An issue whose key makes no claim token
// owns nothing, so it is left out: a walk that meets it ends at the root, and an earlier notice of
// it holds nothing back.
type noticeRoute struct {
	project string
	root    record.Issue
	issues  map[string]record.Issue
}

// treeSnapshot is one read of a notice's tree: its route, and the fence, the tree's unfinished
// notices written before the row, oldest first. The owner walk, the fence, and the linger check
// all see this one state, so a reparent committing meanwhile cannot show them half of itself.
type treeSnapshot struct {
	noticeRoute
	earlier []record.OutboxRow
}

// readNoticeTree reads row's tree in one repeatable-read transaction: row's issue and the tree's
// route (readNoticeRoute), then the fence. A notice it cannot route is errNoticeUnroutable.
func (r *outbox) readNoticeTree(ctx context.Context, row record.OutboxRow) (record.Issue, treeSnapshot, error) {
	var issue record.Issue
	var tree treeSnapshot
	if err := pgx.BeginTxFunc(ctx, r.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		if issue, tree.noticeRoute, err = r.readNoticeRoute(ctx, tx, row.Issue, map[string]noticeRoute{}); err != nil {
			return err
		}
		tree.earlier, err = r.records.EarlierNotices(ctx, tx, tree.root.Key, row.ID)
		return err
	}); err != nil {
		return record.Issue{}, treeSnapshot{}, fmt.Errorf("read the tree of notice row %d: %w", row.ID, err)
	}
	return issue, tree, nil
}

// readNoticeRoute reads in tx the tree of the issue key names, and returns the issue as the tree's
// own issue set holds it with the tree's route. routes holds the routes already read by tree root
// and keeps the ones read here, so a caller routing several notices in one transaction reads each
// tree once. A notice is unroutable when its issue is not recorded, has no
// tree or no recorded root, or its keys make no claim token.
func (r *outbox) readNoticeRoute(ctx context.Context, tx pgx.Tx, key string, routes map[string]noticeRoute) (record.Issue, noticeRoute, error) {
	if !claim.IsIssueKey(key) {
		return record.Issue{}, noticeRoute{}, fmt.Errorf("%w: %q is not an issue key", errNoticeUnroutable, key)
	}
	recorded, err := r.records.Issue(ctx, tx, key)
	if err != nil {
		return record.Issue{}, noticeRoute{}, err
	}
	if recorded == nil {
		return record.Issue{}, noticeRoute{}, fmt.Errorf("%w: workflow issue %s is not recorded", errNoticeUnroutable, key)
	}
	if recorded.Tree == "" {
		return record.Issue{}, noticeRoute{}, fmt.Errorf("%w: issue %s has no tree root", errNoticeUnroutable, key)
	}
	if route, ok := routes[recorded.Tree]; ok {
		return route.issues[key], route, nil
	}
	route := noticeRoute{issues: map[string]record.Issue{}}
	if route.project, err = claim.ProjectToken(recorded.Project); err != nil {
		return record.Issue{}, noticeRoute{}, fmt.Errorf("%w: %w", errNoticeUnroutable, err)
	}
	members, err := r.records.TreeIssues(ctx, tx, recorded.Tree)
	if err != nil {
		return record.Issue{}, noticeRoute{}, err
	}
	for _, member := range members {
		if claim.IsIssueKey(member.Key) {
			route.issues[member.Key] = member
		}
	}
	var ok bool
	if route.root, ok = route.issues[recorded.Tree]; !ok {
		return record.Issue{}, noticeRoute{}, fmt.Errorf("%w: the root %s of %s is not recorded", errNoticeUnroutable, recorded.Tree, key)
	}
	routes[recorded.Tree] = route
	return route.issues[key], route, nil
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

// claimTookRole is whether a claim in state has an agent that took its Envoy role and said it can
// be prompted: ready, working or idle. A registered claim has not yet: the daemon records the
// registration before the agent's plugin claims the role, so a publish then still reaches the
// session that held the role before.
func claimTookRole(state supervise.ClaimState) bool {
	return state == supervise.StateReady || state == supervise.StateWorking || state == supervise.StateIdle
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
