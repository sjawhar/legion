import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useEffect, useMemo, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";

import { api } from "../../api/client";
import type { IssueSummary } from "../../api/types";
import { AttentionBadge } from "../../components/Badge";
import { EmptyState } from "../../components/EmptyState";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { LabelPill, Pill } from "../../components/Pill";
import {
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
import { PriorityControl } from "../issue/PriorityControl";
import { buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { userPreferenceStorageKey } from "../shell/userPreference";
import { issueStatuses } from "./board-model";
import { issueIsUnread, UnreadDot } from "./UnreadDot";

function IssueRow({ issue, unread }: { issue: IssueSummary; unread: boolean }): ReactNode {
  return (
    <li aria-label={`${issue.key} ${issue.title}`} className={`border-t py-3 ${borderDefault}`}>
      <div className="flex flex-wrap items-start justify-between gap-2">
        <Link
          className={`min-w-0 flex-1 text-sm ${linkText} ${linkHoverText}`}
          to={buildIssuePath({ key: issue.key, kind: "issue" })}
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
            className={`rounded-full border ${borderDefault} ${linkText} ${cardHoverBorder}`}
            to={buildIssuePath({ key: issue.parent, kind: "issue" })}
          >
            <Pill>{issue.parent}</Pill>
          </Link>
        )}
      </div>
    </li>
  );
}

export function IssueList({ login, project }: { login?: string; project: string }): ReactNode {
  const [status, setStatus] = useState("all");
  const [searchParams, setSearchParams] = useSearchParams();
  const labels = searchParams.getAll("label");
  const [needsYou, setNeedsYou] = useState(false);
  const [unread, setUnread] = useState(false);
  const [search, setSearch] = useState("");
  const activeFilterCount =
    Number(status !== "all") +
    labels.length +
    Number(needsYou) +
    Number(unread) +
    Number(search.trim() !== "");
  const filterPreferenceKey =
    login === undefined ? undefined : userPreferenceStorageKey(login, "project.issue-filters");
  const [filtersExpanded, setFiltersExpanded] = useState(() => labels.length > 0);

  useEffect(() => {
    if (filterPreferenceKey === undefined) return;
    const saved = window.localStorage.getItem(filterPreferenceKey);
    setFiltersExpanded(activeFilterCount > 0 && saved !== "collapsed");
  }, [activeFilterCount, filterPreferenceKey]);

  const setFiltersOpen = (open: boolean) => {
    setFiltersExpanded(open);
    if (filterPreferenceKey !== undefined) {
      window.localStorage.setItem(filterPreferenceKey, open ? "expanded" : "collapsed");
    }
  };
  const setLabels = (nextLabels: string[]) => {
    const next = new URLSearchParams(searchParams.toString());
    next.delete("label");
    for (const label of nextLabels) {
      next.append("label", label);
    }
    setSearchParams(next, { replace: true });
  };
  const allIssues = useQuery({
    queryKey: ["issues", "project", project],
    queryFn: () => api.listIssues({ project }),
  });
  const filteredIssues = useQuery({
    enabled: labels.length > 0,
    queryKey: ["issues", "project", project, "labels", labels],
    queryFn: () => api.listIssues({ labels, project }),
  });
  const issues = labels.length === 0 ? allIssues : filteredIssues;
  const state = useQuery({
    queryKey: ["user-state"],
    queryFn: () => api.getMyState(),
  });
  const availableLabels = useMemo(
    () =>
      [...new Set((allIssues.data ?? []).flatMap((issue) => issue.labels ?? []))].sort(
        (left, right) => left.localeCompare(right)
      ),
    [allIssues.data]
  );
  const activeFilters = [
    ...(status === "all" ? [] : [{ label: `Status: ${status}`, remove: () => setStatus("all") }]),
    ...labels.map((label) => ({
      label: `Label: ${label}`,
      remove: () => setLabels(labels.filter((current) => current !== label)),
    })),
    ...(search.trim() === ""
      ? []
      : [{ label: `Search: ${search.trim()}`, remove: () => setSearch("") }]),
    ...(needsYou ? [{ label: "Needs you", remove: () => setNeedsYou(false) }] : []),
    ...(unread ? [{ label: "Unread", remove: () => setUnread(false) }] : []),
  ];
  const visibleIssues = useMemo(() => {
    const query = search.trim().toLocaleLowerCase();
    return (issues.data ?? []).filter((issue) => {
      const lastReadSequence = state.data?.[issue.key]?.last_read_seq ?? 0;
      return (
        (status === "all" || issue.status === status) &&
        (!needsYou || issue.open_asks > 0) &&
        (!unread || issueIsUnread(issue, lastReadSequence)) &&
        (query === "" ||
          issue.key.toLocaleLowerCase().includes(query) ||
          issue.title.toLocaleLowerCase().includes(query))
      );
    });
  }, [issues.data, needsYou, search, state.data, status, unread]);

  if (issues.isPending || state.isPending) {
    return <LoadingSkeleton label="Loading issues" />;
  }
  if (issues.isError || state.isError) {
    return <p className={dangerText}>Could not load issues.</p>;
  }

  return (
    <section aria-label="Project issues">
      <div className="mb-4 flex flex-wrap items-center gap-2">
        <button
          aria-controls="project-issue-filters"
          aria-expanded={filtersExpanded}
          className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-semibold ${borderDefault} ${surfaceMutedBg} ${textSecondaryOnCanvas}`}
          onClick={() => setFiltersOpen(!filtersExpanded)}
          type="button"
        >
          Filters · {activeFilterCount} active
        </button>
        {activeFilters.map(({ label, remove }) => (
          <button
            aria-label={`Remove ${label} filter`}
            className="min-h-11 rounded-full"
            key={label}
            onClick={remove}
            type="button"
          >
            <LabelPill>
              {label} <span aria-hidden="true">×</span>
            </LabelPill>
          </button>
        ))}
      </div>
      {filtersExpanded ? (
        <div className="mb-4 flex flex-wrap items-end gap-3" id="project-issue-filters">
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
          <fieldset
            aria-label="Filter by labels"
            className="m-0 flex max-h-32 min-w-0 flex-wrap content-start items-center gap-2 overflow-y-auto border-0 p-0"
          >
            <legend className={`text-sm font-medium ${textSecondaryOnCanvas}`}>Labels</legend>
            {availableLabels.map((candidate) => {
              const selected = labels.includes(candidate);
              return (
                <button
                  aria-pressed={selected}
                  className={`min-h-11 rounded-full border ${borderDefault}`}
                  key={candidate}
                  onClick={() =>
                    setLabels(
                      selected
                        ? labels.filter((label) => label !== candidate)
                        : [...labels, candidate]
                    )
                  }
                  type="button"
                >
                  <Pill tone={selected ? "selected-label" : "label"}>{candidate}</Pill>
                </button>
              );
            })}
            {labels.length === 0 ? null : (
              <button
                className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${borderDefault} ${surfaceMutedBg} ${textSecondaryOnCanvas}`}
                onClick={() => setLabels([])}
                type="button"
              >
                Clear labels
              </button>
            )}
          </fieldset>
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
      ) : null}
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
              className={`min-h-11 cursor-pointer py-3 text-base font-semibold ${textPrimaryOnCanvas}`}
            >
              {currentStatus} ({grouped.length})
            </summary>
            <ul aria-label={`${currentStatus} issues`}>
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
