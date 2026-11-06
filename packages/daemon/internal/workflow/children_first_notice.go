package workflow

import (
	"fmt"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// ChildrenFirstNoticeKind is the kind of the notice below, and the one kind that says a parent's
// own work waits for the work under it.
const ChildrenFirstNoticeKind = record.NoticeKind("children-first")

// ChildCycleReason is what the daemon says of a cycle the children-first judgement found
// (record.ChildWait's Cycle): an open child held by a Dispatch blocked_by link that names an
// issue above it on its own parent chain, so the child waits for an issue that is waiting for
// the child. It is one fixed sentence rather than one built per case, so that a person reading
// two notices reads one rule and a test can hold the daemon to it word for word.
//
// The daemon names it and acts no further. Taking the link off decides what the work depends on,
// which is a person's decision; dropping a Dispatch link of its own accord would lose a fact
// nobody asked it to lose.
func ChildCycleReason(child, ancestor string) string {
	return fmt.Sprintf("%s is blocked_by %s, which stands above it on its own Dispatch parent chain, so neither can start until a person takes that link off", child, ancestor)
}

// ChildrenFirstNotice is what the tree's architect is told when a parent's own first phase begins
// to wait for its children: the parent, each open child with the Dispatch status that holds it,
// what ends the wait, and each cycle among them. It is told once per issue and run of its tree —
// the mark's key is the issue, its tree and that tree's root generation (admit's
// tellChildrenFirst) — so a child filed later during one wait tells nobody again, while a
// re-admitted tree, whose architect writes its spec anew, and a sub-parent handed over to a tree
// of its own are told anew.
func ChildrenFirstNotice(wait record.ChildWait) record.Notice {
	named := make([]string, 0, len(wait.Open))
	var cycles []string
	for _, child := range wait.Open {
		named = append(named, fmt.Sprintf("%s (%s)", child.Key, child.Status))
		if child.Cycle != "" {
			cycles = append(cycles, ChildCycleReason(child.Key, child.Cycle))
		}
	}
	reason := fmt.Sprintf("%s starts no phase of its own while Dispatch shows an open child under it: %s. "+
		"Each one must be released and run until it leaves the workflow, closed, moved to icebox, or moved out from under %s, to another parent or to none; %s starts its own first phase when the last of them has.",
		wait.Parent, strings.Join(named, ", "), wait.Parent, wait.Parent)
	if len(cycles) > 0 {
		reason += " " + strings.Join(cycles, " ") + "."
	}
	return record.Notice{Kind: ChildrenFirstNoticeKind, Role: claim.RoleArchitect, Reason: reason}
}
