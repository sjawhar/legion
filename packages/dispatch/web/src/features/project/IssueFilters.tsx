import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useEffect, useMemo, useState } from "react";

import { api } from "../../api/client";
import { Chip } from "../../components/Chip";
import { MultiSelect } from "../../components/MultiSelect";
import {
  borderDefault,
  inputClasses,
  surfaceMutedBg,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { userPreferenceStorageKey } from "../shell/userPreference";
import { isIssueStatus, issueStatuses, statusLabel } from "./board-model";
import { projectIssuesQueryKey, useIssueFilters } from "./issue-filters";

/**
 * The one filter strip both issue views share: the disclosure trigger, the removable
 * active-filter chips, and the expanded Status / Labels / Search / Needs you / Unread controls,
 * all URL-backed through `useIssueFilters`. Status and Labels are the shared `MultiSelect`
 * (Status one or many, OR'd), and the strip shows Status only for the List (`showStatus`) - the
 * Board's columns are the statuses - counting and chipping it like every other filter.
 */
export function IssueFilters({
  login,
  project,
  showStatus,
}: {
  login?: string;
  project: string;
  showStatus: boolean;
}): ReactNode {
  const filters = useIssueFilters();
  const { labels, setLabels, setStatuses, statuses } = filters;
  const shownStatuses = showStatus ? statuses : [];
  const activeFilterCount = filters.activeFilterCount + shownStatuses.length;
  const activeFilters = [
    ...shownStatuses.map((status) => ({
      // A `?status=` the app does not know stays visible under its own key: the filter still
      // applies (and matches nothing), so the chip must name what the URL says.
      label: `Status: ${isIssueStatus(status) ? statusLabel(status) : status}`,
      remove: () => setStatuses(statuses.filter((current) => current !== status)),
    })),
    ...filters.activeFilters,
  ];
  const filterPreferenceKey =
    login === undefined ? undefined : userPreferenceStorageKey(login, "project.issue-filters");
  const [filtersExpanded, setFiltersExpanded] = useState(() => labels.length > 0);
  const [openPicker, setOpenPicker] = useState<"labels" | "status" | undefined>(undefined);

  useEffect(() => {
    if (filterPreferenceKey === undefined) return;
    const saved = window.localStorage.getItem(filterPreferenceKey);
    const expanded = activeFilterCount > 0 && saved !== "collapsed";
    setFiltersExpanded(expanded);
    if (!expanded) setOpenPicker(undefined);
  }, [activeFilterCount, filterPreferenceKey]);

  const setFiltersOpen = (open: boolean) => {
    setFiltersExpanded(open);
    if (!open) setOpenPicker(undefined);
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
          <Chip aria-label={`Remove ${label} filter`} key={label} onClick={remove} removable>
            {label}
          </Chip>
        ))}
      </div>
      {filtersExpanded ? (
        <div className="mb-4 flex flex-wrap items-end gap-3" id="project-issue-filters">
          {showStatus ? (
            <MultiSelect
              emptyMessage="No matching status."
              label="Status"
              onChange={setStatuses}
              onOpenChange={(open) => setOpenPicker(open ? "status" : undefined)}
              open={openPicker === "status"}
              optionLabel={(status) => (isIssueStatus(status) ? statusLabel(status) : status)}
              options={issueStatuses}
              searchLabel="Search statuses"
              selected={statuses}
            />
          ) : null}
          <MultiSelect
            emptyMessage="No labels yet."
            label="Labels"
            onChange={setLabels}
            onOpenChange={(open) => setOpenPicker(open ? "labels" : undefined)}
            open={openPicker === "labels"}
            options={availableLabels}
            searchLabel="Search labels"
            selected={labels}
          />
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
          <Chip onClick={() => filters.setNeedsYou(!filters.needsYou)} selected={filters.needsYou}>
            Needs you
          </Chip>
          <Chip onClick={() => filters.setUnread(!filters.unread)} selected={filters.unread}>
            Unread
          </Chip>
        </div>
      ) : null}
    </>
  );
}
