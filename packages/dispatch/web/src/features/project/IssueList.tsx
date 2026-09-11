import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useMemo, useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import type { IssueSummary } from "../../api/types";
import {
  badgeBlocking,
  borderDefault,
  cardHoverBorder,
  dangerText,
  inputClasses,
  linkHoverText,
  linkText,
  surfaceMutedBg,
  surfaceMutedStrongBg,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";

const issueStatuses = [
  "triage",
  "icebox",
  "backlog",
  "todo",
  "in_progress",
  "testing",
  "needs_review",
  "retro",
  "done",
] as const;

function issueIsUnread(issue: IssueSummary, lastReadSequence: number): boolean {
  return issue.last_seq > lastReadSequence;
}

function IssueRow({ issue, unread }: { issue: IssueSummary; unread: boolean }): ReactNode {
  return (
    <li aria-label={`${issue.key} ${issue.title}`} className={`border-t py-3 ${borderDefault}`}>
      <div className="flex flex-wrap items-start justify-between gap-2">
        <Link
          className={`min-w-0 flex-1 ${linkText} ${linkHoverText}`}
          to={buildIssuePath({ key: issue.key, kind: "issue" })}
        >
          <span className="font-semibold">{issue.key}</span>
          <span className={`ml-2 ${textPrimaryOnCanvas}`}>{issue.title}</span>
        </Link>
        <div className={`flex items-center gap-2 text-xs ${textMutedOnCanvas}`}>
          {unread ? (
            <span className={`size-2 rounded-full ${badgeBlocking.bg}`} title="Unread" />
          ) : null}
          {issue.open_asks === 0 ? null : (
            <span
              className={`rounded-full px-1.5 py-0.5 font-medium ${badgeBlocking.bg} ${badgeBlocking.text}`}
            >
              {issue.open_asks}
            </span>
          )}
          <Timestamp at={issue.updated_at} />
        </div>
      </div>
      <div className="mt-2 flex flex-wrap items-center gap-2">
        {(issue.labels ?? []).map((label) => (
          <span
            className={`rounded-full px-2 py-1 text-xs ${surfaceMutedStrongBg} ${textSecondaryOnCanvas}`}
            key={label}
          >
            {label}
          </span>
        ))}
        {issue.parent === null ? null : (
          <Link
            className={`rounded-full border px-2 py-1 text-xs font-medium ${borderDefault} ${surfaceMutedBg} ${linkText} ${cardHoverBorder}`}
            to={buildIssuePath({ key: issue.parent, kind: "issue" })}
          >
            {issue.parent}
          </Link>
        )}
      </div>
    </li>
  );
}

export function IssueList({ project }: { project: string }): ReactNode {
  const [status, setStatus] = useState("all");
  const [label, setLabel] = useState("all");
  const [needsYou, setNeedsYou] = useState(false);
  const [unread, setUnread] = useState(false);
  const [search, setSearch] = useState("");
  const issues = useQuery({
    queryKey: ["issues", "project", project],
    queryFn: () => api.listIssues({ project }),
  });
  const state = useQuery({
    queryKey: ["user-state"],
    queryFn: () => api.getMyState(),
  });
  const labels = useMemo(
    () =>
      [...new Set((issues.data ?? []).flatMap((issue) => issue.labels ?? []))].sort((left, right) =>
        left.localeCompare(right)
      ),
    [issues.data]
  );
  const visibleIssues = useMemo(() => {
    const query = search.trim().toLocaleLowerCase();
    return (issues.data ?? []).filter((issue) => {
      const lastReadSequence = state.data?.[issue.key]?.last_read_seq ?? 0;
      return (
        (status === "all" || issue.status === status) &&
        (label === "all" || (issue.labels ?? []).includes(label)) &&
        (!needsYou || issue.open_asks > 0) &&
        (!unread || issueIsUnread(issue, lastReadSequence)) &&
        (query === "" ||
          issue.key.toLocaleLowerCase().includes(query) ||
          issue.title.toLocaleLowerCase().includes(query))
      );
    });
  }, [issues.data, label, needsYou, search, state.data, status, unread]);

  if (issues.isPending || state.isPending) {
    return <p className={textMutedOnCanvas}>Loading issues…</p>;
  }
  if (issues.isError || state.isError) {
    return <p className={dangerText}>Could not load issues.</p>;
  }

  return (
    <section aria-label="Project issues">
      <div className="mb-4 flex flex-wrap items-end gap-3">
        <label className={`text-sm font-medium ${textSecondaryOnCanvas}`}>
          Status
          <select
            aria-label="Status"
            className={`mt-1 block min-h-11 rounded-lg px-3 py-2 text-sm font-normal ${inputClasses(true)}`}
            onChange={(event) => setStatus(event.target.value)}
            value={status}
          >
            <option value="all">All statuses</option>
            {issueStatuses.map((candidate) => (
              <option key={candidate} value={candidate}>
                {candidate}
              </option>
            ))}
          </select>
        </label>
        <label className={`text-sm font-medium ${textSecondaryOnCanvas}`}>
          Label
          <select
            aria-label="Label"
            className={`mt-1 block min-h-11 rounded-lg px-3 py-2 text-sm font-normal ${inputClasses(true)}`}
            onChange={(event) => setLabel(event.target.value)}
            value={label}
          >
            <option value="all">All labels</option>
            {labels.map((candidate) => (
              <option key={candidate} value={candidate}>
                {candidate}
              </option>
            ))}
          </select>
        </label>
        <label className={`text-sm font-medium ${textSecondaryOnCanvas}`}>
          Search
          <input
            aria-label="Search issues"
            className={`mt-1 block min-h-11 rounded-lg px-3 py-2 text-sm font-normal ${inputClasses(true)}`}
            onChange={(event) => setSearch(event.target.value)}
            type="search"
            value={search}
          />
        </label>
        <button
          aria-pressed={needsYou}
          className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${borderDefault} ${
            needsYou ? surfaceMutedStrongBg : surfaceMutedBg
          } ${textSecondaryOnCanvas}`}
          onClick={() => setNeedsYou((current) => !current)}
          type="button"
        >
          Needs you
        </button>
        <button
          aria-pressed={unread}
          className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${borderDefault} ${
            unread ? surfaceMutedStrongBg : surfaceMutedBg
          } ${textSecondaryOnCanvas}`}
          onClick={() => setUnread((current) => !current)}
          type="button"
        >
          Unread
        </button>
      </div>
      {issueStatuses.map((currentStatus) => {
        const grouped = visibleIssues.filter((issue) => issue.status === currentStatus);
        if (grouped.length === 0) {
          return null;
        }
        return (
          <details
            aria-label={`${currentStatus} (${grouped.length})`}
            className={`mb-3 rounded-xl border px-4 ${borderDefault}`}
            key={currentStatus}
            open={currentStatus !== "done"}
          >
            <summary
              className={`min-h-11 cursor-pointer py-3 text-sm font-semibold ${textPrimaryOnCanvas}`}
            >
              {currentStatus} ({grouped.length})
            </summary>
            <ul>
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
        <p className={textMutedOnCanvas}>No issues match these filters.</p>
      ) : null}
    </section>
  );
}
