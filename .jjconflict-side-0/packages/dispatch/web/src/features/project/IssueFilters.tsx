import { SEARCH_QUERY_MAX } from "@legion/contracts/dispatch-tools";
import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useEffect, useMemo, useState } from "react";

import { projectIssuesQuery } from "../../api/queries";
import { Chip } from "../../components/Chip";
import { MultiSelect } from "../../components/MultiSelect";
import {
  borderDefault,
  inputClasses,
  surfaceMutedBg,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { useUserPreference } from "../shell/userPreference";
import { issueStatuses, statusText } from "./board-model";
import { useIssueFilters } from "./issue-filters";

/**
 * The one filter strip both issue views share: the disclosure trigger, the removable
 * active-filter chips, and the expanded Status / Labels / Search / Needs you / Unread controls,
 * all URL-backed through `useIssueFilters`. Status and Labels are the shared `MultiSelect`
 * (Status one or many, OR'd), and the strip shows Status only for the List (`showStatus`) - the
 * Board's columns are the statuses - counting and chipping it like every other filter.
 */
export function IssueFilters({
  project,
  showStatus,
}: {
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
      label: `Status: ${statusText(status)}`,
      remove: () => setStatuses(statuses.filter((current) => current !== status)),
    })),
    ...filters.activeFilters,
  ];
  const noIdentityFiltersExpanded = labels.length > 0;
  // The saved preference is the only owner of the open state: the strip reads it when it mounts
  // and when the signed-in identity resolves, never because the filter count moved. Arriving from
  // another page, tab or project mounts a new strip, so that read covers the filters the route
  // brings. On a strip that is up the count moves when the reader filters in it, when the view
  // toggle folds `?status=` in or out (the List counts it, the Board does not), and when Back or
  // Forward crosses entries of the Issues tab. A re-read would collapse the strip the moment the
  // last filter goes (clearing the search box), taking the control being typed in with it, or
  // open or close it against the reader's choice on the others, while its trigger and chips
  // already show every filter. So it stays as they left it until they close it or leave the
  // project or its Issues tab.
  const [filtersExpanded, setFiltersExpanded] = useUserPreference(
    "project.issue-filters",
    (stored) => activeFilterCount > 0 && stored !== "collapsed",
    (open) => (open ? "expanded" : "collapsed"),
    { failed: noIdentityFiltersExpanded, pending: noIdentityFiltersExpanded }
  );
  const [openPicker, setOpenPicker] = useState<"labels" | "status" | undefined>(undefined);

  useEffect(() => {
    if (!filtersExpanded) setOpenPicker(undefined);
  }, [filtersExpanded]);
  const setFiltersOpen = (open: boolean) => {
    setFiltersExpanded(open);
    if (!open) setOpenPicker(undefined);
  };
  const allIssues = useQuery(projectIssuesQuery(project, []));
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
          <Chip
            aria-label={`Remove ${label} filter`}
            key={label}
            onClick={remove}
            removable
            title={label}
          >
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
              optionLabel={statusText}
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
              maxLength={SEARCH_QUERY_MAX}
              onChange={(event) => filters.setSearch(event.target.value)}
              title={`At most ${SEARCH_QUERY_MAX} characters`}
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
          <Chip
            onClick={() => filters.setUnclaimed(!filters.unclaimed)}
            selected={filters.unclaimed}
          >
            Unclaimed
          </Chip>
        </div>
      ) : null}
    </>
  );
}
