import { useCallback, useMemo } from "react";
import { useSearchParams } from "react-router-dom";

import type { IssueSummary } from "../../api/types";
import { issueIsUnread } from "./UnreadDot";

/** One removable chip in the filter strip. */
export interface ActiveFilter {
  readonly label: string;
  readonly remove: () => void;
}

export interface IssueFiltersState {
  readonly labels: string[];
  readonly search: string;
  readonly needsYou: boolean;
  readonly unread: boolean;
  readonly activeFilterCount: number;
  readonly activeFilters: ActiveFilter[];
  readonly setLabels: (next: string[]) => void;
  readonly setSearch: (next: string) => void;
  readonly setNeedsYou: (next: boolean) => void;
  readonly setUnread: (next: boolean) => void;
  /** Whether `issue` passes every active filter. Status is not here: it is the List's own
   *  view-local state, since on the Board the columns are the statuses. */
  readonly matches: (issue: IssueSummary, lastReadSequence: number) => boolean;
}

/** The one query key for a project's issue list: the plain list, or the server-filtered list
 *  when labels are active. List and Board build their keys here so the caches are shared. */
export function projectIssuesQueryKey(
  project: string,
  labels: readonly string[]
): readonly unknown[] {
  return labels.length === 0
    ? ["issues", "project", project]
    : ["issues", "project", project, "labels", labels];
}

/**
 * The project page's issue filters, read from and written to the URL - `?label=` (repeatable),
 * `?q=`, `?needs-you=1`, `?unread=1` - so List and Board share one filter state and a filtered
 * view survives a reload. Every write replaces the history entry, as the label filter always has.
 */
export function useIssueFilters(): IssueFiltersState {
  const [searchParams, setSearchParams] = useSearchParams();
  const labels = useMemo(() => searchParams.getAll("label"), [searchParams]);
  const search = searchParams.get("q") ?? "";
  const needsYou = searchParams.get("needs-you") === "1";
  const unread = searchParams.get("unread") === "1";
  const update = useCallback(
    (mutate: (params: URLSearchParams) => void) => {
      setSearchParams(
        (current) => {
          const next = new URLSearchParams(current.toString());
          mutate(next);
          return next;
        },
        { replace: true }
      );
    },
    [setSearchParams]
  );
  const setLabels = useCallback(
    (next: string[]) =>
      update((params) => {
        params.delete("label");
        for (const label of next) {
          params.append("label", label);
        }
      }),
    [update]
  );
  const setSearch = useCallback(
    (next: string) =>
      update((params) => {
        if (next === "") {
          params.delete("q");
        } else {
          params.set("q", next);
        }
      }),
    [update]
  );
  const setFlag = useCallback(
    (name: "needs-you" | "unread", next: boolean) =>
      update((params) => {
        if (next) {
          params.set(name, "1");
        } else {
          params.delete(name);
        }
      }),
    [update]
  );
  const setNeedsYou = useCallback((next: boolean) => setFlag("needs-you", next), [setFlag]);
  const setUnread = useCallback((next: boolean) => setFlag("unread", next), [setFlag]);
  const matches = useCallback(
    (issue: IssueSummary, lastReadSequence: number): boolean => {
      const query = search.trim().toLocaleLowerCase();
      return (
        (!needsYou || issue.open_asks > 0) &&
        (!unread || issueIsUnread(issue, lastReadSequence)) &&
        (query === "" ||
          issue.key.toLocaleLowerCase().includes(query) ||
          issue.title.toLocaleLowerCase().includes(query))
      );
    },
    [needsYou, search, unread]
  );
  const activeFilterCount =
    labels.length + Number(search.trim() !== "") + Number(needsYou) + Number(unread);
  const activeFilters: ActiveFilter[] = [
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
  return {
    activeFilterCount,
    activeFilters,
    labels,
    matches,
    needsYou,
    search,
    setLabels,
    setNeedsYou,
    setSearch,
    setUnread,
    unread,
  };
}
