package record

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// Beneath is the members of issue's tree that lie under it. For a tree root that is every other
// member, whatever each one hangs under: a root's acceptance and its close decide the whole
// tree's order, and its close lingers every member of it, as closeMerged's own wait already
// reads the flat tree rather than the parent relation. For a child it is the members whose chain
// of recorded parents reaches that child, which is what "beneath" means for an issue that is not
// the tree's own root.
func Beneath(issue Issue, members []Issue) []Issue {
	records := byKey(members)
	under := make([]Issue, 0, len(members))
	for _, member := range members {
		if member.Key == issue.Key {
			continue
		}
		if claim.IsTreeRoot(issue.Key, issue.Tree) || descends(member, issue.Key, records) {
			under = append(under, member)
		}
	}
	return under
}

// Above is the members of issue's tree that issue lies beneath, nearest first and the tree's own
// root last: the mirror of Beneath, so a member is in Above(issue) exactly where issue is in
// Beneath(member). It is the one parent-chain walk (Ancestors) over the tree's own members, so
// a gap in the chain is handled here exactly as every other reader of "above" handles it.
//
// It is what a departure reads to find whose wait it may have ended: a merged issue stands in
// awaiting_merge until the last issue beneath it leaves (OpenBeneath, workflow's closeMerged),
// and that wait is no longer a tree root's alone, so the leave of one issue re-reads the wait of
// every merged ancestor it stood beneath (workflow's leave).
func Above(issue Issue, members []Issue) []Issue {
	return Ancestors(issue, byKey(members))
}

// descends says whether member hangs under key, through the one parent-chain walk (Ancestors)
// over byKey.
func descends(member Issue, key string, byKey map[string]Issue) bool {
	return slices.ContainsFunc(Ancestors(member, byKey), func(above Issue) bool { return above.Key == key })
}

// byKey indexes members by their key, which every rule here reads the parent chain over.
func byKey(members []Issue) map[string]Issue {
	indexed := make(map[string]Issue, len(members))
	for _, member := range members {
		indexed[member.Key] = member
	}
	return indexed
}

// OpenBeneath is the sorted keys of the issues Beneath issue that are still in the workflow
// (InWorkflow), and nothing at all when none is. It is the one set the two acceptance rules read:
// an architect's sign-off of a reviewed head (signOff) and its close of a tree that never started
// a phase (closeRoot) are both refused while it is not empty, and a root whose pull request
// merged waits in awaiting_merge until it empties (closeMerged). A parent's work is proved by a
// head that carries its children's merges, so a parent that is accepted or closed first would
// merge work its own head does not carry.
//
// One kind of issue beneath is left out: one the daemon is itself holding at phase.Admitted,
// because an ancestor of it has already been accepted (HeldUnderAccepted). Without that
// exclusion the three rules deadlock. A child held at admitted under a root in awaiting_merge is
// InWorkflow — a recorded member at admitted with status todo is — so it keeps that root open
// through this set for ever, while the child itself is pinned at admitted until the root closes;
// a child held under an ancestor above an issue in accepting deadlocks that issue's sign-off the
// same way. Neither moves, and no act of a person short of parking the child breaks it.
// Excluding exactly the issues the daemon's own hold has stopped costs nothing else: every other
// issue beneath still refuses the sign-off and still holds the merged root open, and the held
// issue still holds every ancestor that has not been accepted, because the hold is what the
// exclusion is read from.
//
// The close this exclusion allows does not strand the issue it excluded: the linger of the
// closed tree hands every member the daemon held under an accepted ancestor to Legion as a root
// of its own (workflow's unfinishedHandOvers), exactly as it hands over a never-started child a
// blocker holds, so the work a person asked for runs once the ancestor that held it has closed,
// which is what the architect's notice says it does. This is the one recorded departure from
// the spec of dispatch://INFRA-980, decided by that tree's architect
// (dispatch://INFRA-980/comment/d642164a-31d2-4d6c-ae5e-4da5dd0b9104).
func OpenBeneath(issue Issue, members []Issue) []string {
	records := byKey(members)
	open := []string{}
	for _, member := range Beneath(issue, members) {
		if !InWorkflow(member) || HeldUnderAccepted(member, records) {
			continue
		}
		open = append(open, member.Key)
	}
	slices.Sort(open)
	return open
}

// AcceptedAncestorOf is the issue above key, in key's own tree, that the daemon has already
// accepted (AcceptedAncestor), and "" when none has been or key is not recorded. It is
// what the two doors into the workflow ask before they let work in beneath such an ancestor —
// release_children and rerun_child, which refuse CHILD_UNDER_ACCEPTED_PARENT — read over the
// tree's records in one query.
//
// parent is the Dispatch parent the caller read for a child that has no record of its own yet,
// which is every child release_children releases from backlog or triage: the chain starts there
// instead, so the rule reaches an unrecorded child exactly as it reaches a recorded one. tree is
// the architect's own tree, whose records the chain is walked over.
func AcceptedAncestorOf(ctx context.Context, store Store, tx pgx.Tx, tree, key, parent string) (string, error) {
	members, err := store.TreeIssues(ctx, tx, tree)
	if err != nil {
		return "", err
	}
	return AcceptedAncestorIn(members, tree, key, parent), nil
}

// AcceptedAncestorIn is AcceptedAncestorOf over members the caller has already read: a handler
// that answers for a whole wave of children reads the tree once and asks this of each of them,
// rather than reading every record of the tree again per child (api's release_children).
func AcceptedAncestorIn(members []Issue, tree, key, parent string) string {
	records := byKey(members)
	issue, recorded := records[key]
	if !recorded {
		if parent == "" {
			return ""
		}
		issue = Issue{Key: key, Tree: tree, Parent: &parent}
	}
	return AcceptedAncestor(issue, records)
}
