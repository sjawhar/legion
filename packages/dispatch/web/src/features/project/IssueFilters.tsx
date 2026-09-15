import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useEffect, useMemo, useState } from "react";

import { api } from "../../api/client";
import { LabelPill, Pill } from "../../components/Pill";
import {
  borderDefault,
  inputClasses,
  surfaceMutedBg,
  surfaceMutedStrongBg,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { userPreferenceStorageKey } from "../shell/userPreference";
import { issueStatuses } from "./board-model";
import { projectIssuesQueryKey, useIssueFilters } from "./issue-filters";

/**
 * The one filter strip both issue views share: the disclosure trigger, the removable
 * active-filter chips, and the expanded Labels / Search / Needs you / Unread controls, all
 * URL-backed through `useIssueFilters`. The List folds its view-local Status select into the
 * same strip (`showStatus`), so the count and chips name every filter the visible rows obey;
 * the Board renders the strip without it - its columns are the statuses.
 */
export function IssueFilters({
  login,
  onStatusChange,
  project,
  showStatus,
  status,
}: {
  login?: string;
  onStatusChange: (status: string) => void;
  project: string;
  showStatus: boolean;
  status: string;
}): ReactNode {
  const filters = useIssueFilters();
  const { labels, setLabels } = filters;
  const statusActive = showStatus && status !== "all";
  const activeFilterCount = filters.activeFilterCount + Number(statusActive);
  const activeFilters = [
    ...(statusActive ? [{ label: `Status: ${status}`, remove: () => onStatusChange("all") }] : []),
    ...filters.activeFilters,
  ];
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
  const allIssues = useQuery({
    queryKey: projectIssuesQueryKey(project, []),
    queryFn: () => api.listIssues({ project }),
  });
  const availableLabels = useMemo(
    () =>
      [...new Set((allIssues.data ?? []).flatMap((issue) => issue.labels ?? []))].sort(
        (left, right) => left.localeCompare(right)
      ),
    [allIssues.data]
  );

  return (
    <>
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
          {showStatus ? (
            <label className={`text-sm font-medium ${textSecondaryOnCanvas}`}>
              Status
              <select
                aria-label="Status"
                className={`mt-1 block min-h-11 rounded-lg px-3 py-2 text-sm font-normal ${inputClasses(true)}`}
                onChange={(event) => onStatusChange(event.target.value)}
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
          ) : null}
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
              onChange={(event) => filters.setSearch(event.target.value)}
              type="search"
              value={filters.search}
            />
          </label>
          <button
            aria-pressed={filters.needsYou}
            className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${borderDefault} ${
              filters.needsYou ? surfaceMutedStrongBg : surfaceMutedBg
            } ${textSecondaryOnCanvas}`}
            onClick={() => filters.setNeedsYou(!filters.needsYou)}
            type="button"
          >
            Needs you
          </button>
          <button
            aria-pressed={filters.unread}
            className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${borderDefault} ${
              filters.unread ? surfaceMutedStrongBg : surfaceMutedBg
            } ${textSecondaryOnCanvas}`}
            onClick={() => filters.setUnread(!filters.unread)}
            type="button"
          >
            Unread
          </button>
        </div>
      ) : null}
    </>
  );
}
