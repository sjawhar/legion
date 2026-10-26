package api

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// unsetIssue is the issue an issue.created's changed list is measured from: no field holds a
// value, and its components inherit from nobody (componentsScan.resolve's unassigned attachment).
var unsetIssue = model.Issue{Components: model.IssueComponents{Mode: "inherit"}}

// issueChangedFields is an issue event's changed list: the name of every field whose value differs
// between before and after, in one fixed order. repo moves with the issue's own repository or the
// one it resolves to (a reparent can move the second alone), and components with the effective
// attachment, so an inherited change counts too. closed_at moves only with a close or a reopen
// (whether the issue is closed, not the time), which a status alone cannot say: a reader learns
// from it that the issue's open asks entered or left the Inbox and the projects' open-ask counts.
func issueChangedFields(before, after model.Issue) []string {
	changed := []string{}
	add := func(field string, moved bool) {
		if moved {
			changed = append(changed, field)
		}
	}
	add("title", before.Title != after.Title)
	add("status", before.Status != after.Status)
	add("closed_at", (before.ClosedAt == nil) != (after.ClosedAt == nil))
	add("rank", before.Rank != after.Rank)
	add("priority", !intPointersEqual(before.Priority, after.Priority))
	add("assignee", !stringPointersEqual(before.Assignee, after.Assignee))
	add("parent", !stringPointersEqual(before.Parent, after.Parent))
	add("labels", !slices.Equal(before.Labels, after.Labels))
	add("route", !stringPointersEqual(before.Route, after.Route))
	add("repo", !stringPointersEqual(before.Repo, after.Repo) || !stringPointersEqual(before.ResolvedRepo, after.ResolvedRepo))
	add("components", !issueComponentsEqual(before.Components, after.Components))
	add("blocked_by", !slices.Equal(before.BlockedBy, after.BlockedBy))
	add("external_links", !slices.Equal(before.ExternalLinks, after.ExternalLinks))
	return changed
}

func intPointersEqual(left, right *int) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func issueComponentsEqual(left, right model.IssueComponents) bool {
	return left.Mode == right.Mode &&
		slices.Equal(left.IDs, right.IDs) &&
		slices.Equal(left.Unknown, right.Unknown) &&
		stringPointersEqual(left.Reason, right.Reason) &&
		stringPointersEqual(left.InheritedFrom, right.InheritedFrom)
}

// issueAncestors is an issue event's ancestors: key's parent chain, parent first up to the root,
// [] for a root. The walk stops at parentDepthCap, as every recursive issue walk does.
func issueAncestors(ctx context.Context, q queryer, key string) ([]string, error) {
	ancestors := []string{}
	if err := q.QueryRow(ctx, `
		with recursive ancestors as (
			select p.key, p.parent_key, 1 as depth
			from issues i join issues p on p.key = i.parent_key where i.key = $1
			union
			select i.key, i.parent_key, a.depth + 1
			from issues i join ancestors a on i.key = a.parent_key
			where a.depth < $2
		)
		select coalesce(array_agg(key order by depth), '{}') from ancestors
	`, key, parentDepthCap).Scan(&ancestors); err != nil {
		return nil, fmt.Errorf("read the ancestors of %s: %w", key, err)
	}
	return ancestors, nil
}

// newIssueEventPayload is the payload of an issue.created, issue.updated or issue.closed: the
// issue after the write, what it moved in the reference graph, the fields it changed, and the
// issue's ancestors read in tx. The caller adds thread_logins when the status moved.
func newIssueEventPayload(ctx context.Context, tx pgx.Tx, issue model.Issue, changes model.ReferenceChanges, changed []string) (model.IssueEventPayload, error) {
	ancestors, err := issueAncestors(ctx, tx, issue.Key)
	if err != nil {
		return model.IssueEventPayload{}, err
	}
	payload := model.NewIssueEventPayload(issue, changes)
	payload.Changed = changed
	payload.Ancestors = ancestors
	return payload, nil
}

// titleExcerptChanges names the issues whose referenced-by panel shows issue's title: its parent
// (the child_of edge) and the issues it is blocked by (its blocked_by edges), each an edge the
// graph keeps across a retitle but whose excerpt (refs.Edges cuts it from an issue's title) moves.
func titleExcerptChanges(ctx context.Context, tx pgx.Tx, issue model.Issue) (model.ReferenceChanges, error) {
	targets := make([]refs.Target, 0, len(issue.BlockedBy)+1)
	if issue.Parent != nil {
		targets = append(targets, refs.Target{Kind: "issue", ID: *issue.Parent})
	}
	for _, blocker := range issue.BlockedBy {
		targets = append(targets, refs.Target{Kind: "issue", ID: blocker})
	}
	return refs.CountedChanges(ctx, tx, targets)
}
