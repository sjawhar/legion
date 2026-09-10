import { useQueries, useQuery } from "@tanstack/react-query";
import { type ReactNode, useRef } from "react";
import { Link, useLocation } from "react-router-dom";

import { api } from "../../api/client";
import type { IssueSummary, UserState } from "../../api/types";
import { buildIssuePath, parseIssuePath } from "../refs/routes";

export type SidebarEntry = string | { key: string; children: SidebarEntry[] };

export interface SidebarGroup {
  count: number;
  items: SidebarEntry[];
  label: "Pinned" | "Needs you" | "Unread" | "Everything else";
}

const groups: SidebarGroup["label"][] = ["Pinned", "Needs you", "Unread", "Everything else"];

function baseRank(
  issue: IssueSummary,
  state: UserState,
  lastSeqByIssue: Record<string, number>
): number {
  if (state[issue.key]?.pinned) {
    return 0;
  }
  if (issue.open_asks > 0) {
    return 1;
  }
  if ((lastSeqByIssue[issue.key] ?? 0) > (state[issue.key]?.last_read_seq ?? 0)) {
    return 2;
  }
  return 3;
}

export function arrangeIssues(
  issues: IssueSummary[],
  state: UserState,
  lastSeqByIssue: Record<string, number>
): SidebarGroup[] {
  const issueByKey = new Map(issues.map((issue) => [issue.key, issue]));
  const childrenByParent = new Map<string, IssueSummary[]>();
  const roots: IssueSummary[] = [];

  for (const issue of issues) {
    if (issue.parent !== null && issueByKey.has(issue.parent)) {
      const children = childrenByParent.get(issue.parent) ?? [];
      children.push(issue);
      childrenByParent.set(issue.parent, children);
    } else {
      roots.push(issue);
    }
  }

  const rankForTree = (issue: IssueSummary): number => {
    let rank = baseRank(issue, state, lastSeqByIssue);
    for (const child of childrenByParent.get(issue.key) ?? []) {
      rank = Math.min(rank, rankForTree(child));
    }
    return rank;
  };
  const entryForIssue = (issue: IssueSummary): SidebarEntry => {
    const children = [...(childrenByParent.get(issue.key) ?? [])]
      .sort((left, right) => right.updated_at.localeCompare(left.updated_at))
      .map(entryForIssue);
    return children.length === 0 ? issue.key : { children, key: issue.key };
  };
  const groupItems = groups.map((_label, rank) =>
    roots
      .filter((issue) => rankForTree(issue) === rank)
      .sort((left, right) => right.updated_at.localeCompare(left.updated_at))
      .map(entryForIssue)
  );

  return groups
    .map((label, rank) => {
      const rootKeys = new Set<string>();
      const visit = (entry: SidebarEntry) => {
        if (typeof entry === "string") {
          rootKeys.add(entry);
          return;
        }
        rootKeys.add(entry.key);
        for (const child of entry.children) {
          visit(child);
        }
      };
      for (const entry of groupItems[rank]) {
        visit(entry);
      }
      const count =
        label === "Needs you"
          ? issues
              .filter((issue) => rootKeys.has(issue.key))
              .reduce((total, issue) => total + issue.open_asks, 0)
          : groupItems[rank].length;
      return { count, items: groupItems[rank], label };
    })
    .filter((group) => group.items.length > 0);
}

export interface FrozenSidebar {
  groups: SidebarGroup[];
  issueKey: string;
}

export function selectSidebarView(
  currentIssue: string | undefined,
  frozen: FrozenSidebar | undefined,
  arranged: SidebarGroup[],
  detailsPending: boolean
): { displayed: SidebarGroup[] | undefined; frozen: FrozenSidebar | undefined } {
  if (currentIssue === undefined) {
    return { displayed: detailsPending ? undefined : arranged, frozen: undefined };
  }
  if (detailsPending && frozen?.issueKey !== currentIssue) {
    return { displayed: undefined, frozen };
  }
  if (frozen?.issueKey === currentIssue) {
    return { displayed: frozen.groups, frozen };
  }
  const next = { groups: arranged, issueKey: currentIssue };
  return { displayed: arranged, frozen: next };
}

function activeIssueKey(pathname: string): string | undefined {
  return parseIssuePath(pathname)?.key;
}

function IssueLink({
  currentIssue,
  entry,
  issues,
  onNavigate,
}: {
  currentIssue: string | undefined;
  entry: SidebarEntry;
  issues: Map<string, IssueSummary>;
  onNavigate?: () => void;
}): ReactNode {
  const key = typeof entry === "string" ? entry : entry.key;
  const issue = issues.get(key);
  if (issue === undefined) {
    return null;
  }
  const isActive = key === currentIssue;
  const children = typeof entry === "string" ? [] : entry.children;
  return (
    <li>
      <Link
        aria-current={isActive ? "page" : undefined}
        className={
          isActive
            ? "flex items-baseline gap-2 rounded bg-slate-800 px-2 py-1.5 text-sm font-medium text-sky-300"
            : "flex items-baseline gap-2 rounded px-2 py-1.5 text-sm hover:bg-slate-800"
        }
        onClick={onNavigate}
        to={buildIssuePath({ key: issue.key, kind: "issue" })}
      >
        <span className="shrink-0 font-medium whitespace-nowrap">{issue.key}</span>
        <span className="min-w-0 flex-1 text-slate-300">{issue.title}</span>
        {issue.open_asks === 0 ? null : (
          <span className="shrink-0 rounded-full bg-sky-500 px-1.5 py-0.5 text-xs text-slate-950">
            {issue.open_asks}
          </span>
        )}
      </Link>
      {children.length === 0 ? null : (
        <ul className="ml-3 border-l border-slate-700 pl-2">
          {children.map((child) => (
            <IssueLink
              currentIssue={currentIssue}
              entry={child}
              issues={issues}
              key={typeof child === "string" ? child : child.key}
              onNavigate={onNavigate}
            />
          ))}
        </ul>
      )}
    </li>
  );
}

export function Sidebar({ onNavigate }: { onNavigate?: () => void }): ReactNode {
  const location = useLocation();
  const issues = useQuery({ queryKey: ["issues"], queryFn: () => api.listIssues() });
  const state = useQuery({ queryKey: ["user-state"], queryFn: () => api.getMyState() });
  const issueDetails = useQueries({
    queries: (issues.data ?? []).map((issue) => ({
      queryKey: ["issue", issue.key],
      queryFn: () => api.getIssue(issue.key),
    })),
  });
  const currentIssue = activeIssueKey(location.pathname);
  const frozen = useRef<FrozenSidebar | undefined>(undefined);
  const lastSeqByIssue = Object.fromEntries(
    issueDetails.flatMap((detail) =>
      detail.data === undefined ? [] : [[detail.data.key, detail.data.last_seq] as const]
    )
  );
  if (issues.isError || state.isError) {
    return <p className="mt-8 text-sm text-rose-300">Could not load issues.</p>;
  }
  if (issues.isPending || state.isPending) {
    return <p className="mt-8 text-sm text-slate-400">Loading issues…</p>;
  }

  const arranged = arrangeIssues(issues.data ?? [], state.data ?? {}, lastSeqByIssue);
  const view = selectSidebarView(
    currentIssue,
    frozen.current,
    arranged,
    issueDetails.some((detail) => detail.isPending)
  );
  frozen.current = view.frozen;

  if (view.displayed === undefined) {
    return <p className="mt-8 text-sm text-slate-400">Loading issues…</p>;
  }

  const displayed = view.displayed;

  const issueByKey = new Map((issues.data ?? []).map((issue) => [issue.key, issue]));
  return (
    <nav aria-label="Issues" className="mt-8 space-y-5">
      {displayed.map((group) => (
        <section key={group.label}>
          <h2 className="px-2 text-xs font-semibold tracking-wide text-slate-400 uppercase">
            {group.label}
            {group.count === 0 ? null : ` (${group.count})`}
          </h2>
          <ul className="mt-1 space-y-0.5">
            {group.items.map((entry) => (
              <IssueLink
                currentIssue={currentIssue}
                entry={entry}
                issues={issueByKey}
                key={typeof entry === "string" ? entry : entry.key}
                onNavigate={onNavigate}
              />
            ))}
          </ul>
        </section>
      ))}
    </nav>
  );
}
