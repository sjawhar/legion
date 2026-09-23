import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useMemo } from "react";
import { Link } from "react-router-dom";

import { userStateQuery } from "../../api/queries";
import type { IssueSummary } from "../../api/types";
import { AttentionBadge } from "../../components/Badge";
import { EmptyState } from "../../components/EmptyState";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { LabelPill, pillClassName } from "../../components/Pill";
import {
  borderDefault,
  cardHoverBorder,
  dangerText,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { PriorityControl } from "../issue/PriorityControl";
import { referenceTriggerProps } from "../refs/RefPreview";
import { buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { issueStatuses, statusLabel } from "./board-model";
import { projectIssuesQuery, useIssueFilters } from "./issue-filters";
import { issueIsUnread, UnreadDot } from "./UnreadDot";

function IssueRow({ issue, unread }: { issue: IssueSummary; unread: boolean }): ReactNode {
  return (
    <li aria-label={`${issue.key} ${issue.title}`} className={`border-t py-3 ${borderDefault}`}>
      <div className="flex flex-wrap items-start justify-between gap-2">
        <Link
          className={`min-w-0 flex-1 text-sm ${linkText} ${linkHoverText}`}
          to={buildIssuePath({ key: issue.key, kind: "issue" })}
          {...referenceTriggerProps({ key: issue.key, kind: "issue" })}
        >
          <span className="font-semibold">{issue.key}</span>
          <span className={`ml-2 ${textPrimaryOnCanvas}`}>{issue.title}</span>
        </Link>
        <div className={`flex items-center gap-2 text-xs ${textMutedOnCanvas}`}>
          {unread ? <UnreadDot /> : null}
          {issue.open_asks === 0 ? null : <AttentionBadge count={issue.open_asks} />}
          <Timestamp at={issue.updated_at} />
        </div>
      </div>
      <div className="mt-2 flex flex-wrap items-center gap-2">
        <PriorityControl
          disabled={issue.status === "done"}
          issueKey={issue.key}
          priority={issue.priority}
        />
        {(issue.labels ?? []).map((label) => (
          <LabelPill key={label}>{label}</LabelPill>
        ))}
        {issue.parent === null ? null : (
          <Link
            className={`${pillClassName("label")} border ${borderDefault} ${cardHoverBorder}`}
            to={buildIssuePath({ key: issue.parent, kind: "issue" })}
            {...referenceTriggerProps({ key: issue.parent, kind: "issue" })}
          >
            {issue.parent}
          </Link>
        )}
      </div>
    </li>
  );
}

export function IssueList({ project }: { project: string }): ReactNode {
  const { labels, matches, statuses } = useIssueFilters();
  const allIssues = useQuery(projectIssuesQuery(project, []));
  const filteredIssues = useQuery({
    ...projectIssuesQuery(project, labels),
    enabled: labels.length > 0,
  });
  const issues = labels.length === 0 ? allIssues : filteredIssues;
  const state = useQuery(userStateQuery());
  const visibleIssues = useMemo(
    () =>
      (issues.data ?? []).filter(
        (issue) =>
          (statuses.length === 0 || statuses.includes(issue.status)) &&
          matches(issue, state.data?.[issue.key]?.last_read_seq ?? 0)
      ),
    [issues.data, matches, state.data, statuses]
  );

  if (issues.isPending || state.isPending) {
    return <LoadingSkeleton label="Loading issues" />;
  }
  if (issues.isError || state.isError) {
    return <p className={dangerText}>Could not load issues.</p>;
  }

  return (
    <section aria-label="Project issues">
      {issueStatuses.map((currentStatus) => {
        const grouped = visibleIssues.filter((issue) => issue.status === currentStatus);
        if (grouped.length === 0) {
          return null;
        }
        return (
          <details
            aria-label={`${statusLabel(currentStatus)} (${grouped.length})`}
            className={`mb-3 rounded-xl border px-4 ${borderDefault}`}
            key={currentStatus}
            open={currentStatus !== "done"}
          >
            <summary
              className={`min-h-11 cursor-pointer py-3 text-base font-semibold ${textPrimaryOnCanvas}`}
            >
              {statusLabel(currentStatus)} ({grouped.length})
            </summary>
            <ul aria-label={`${statusLabel(currentStatus)} issues`}>
              {grouped.map((issue) => (
                <IssueRow
                  issue={issue}
                  key={issue.key}
                  unread={issueIsUnread(issue, state.data?.[issue.key]?.last_read_seq ?? 0)}
                />
              ))}
            </ul>
          </details>
        );
      })}
      {visibleIssues.length === 0 ? (
        <EmptyState label="No matching project issues" message="No issues match these filters." />
      ) : null}
    </section>
  );
}
