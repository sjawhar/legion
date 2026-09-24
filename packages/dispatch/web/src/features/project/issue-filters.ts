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
  /** The List's `?status=` values (OR across them). Not folded into `activeFilters` or
   *  `matches`: the Board ignores it - its columns are the statuses - so the strip counts and
   *  chips it only while the List is showing. */
  readonly statuses: string[];
  readonly search: string;
  readonly needsYou: boolean;
  readonly unread: boolean;
  /** Only issues nobody has claimed: what an agent looking for work should see. */
  readonly unclaimed: boolean;
  readonly activeFilterCount: number;
  readonly activeFilters: ActiveFilter[];
  readonly setLabels: (next: string[]) => void;
  readonly setStatuses: (next: string[]) => void;
  readonly setSearch: (next: string) => void;
  readonly setNeedsYou: (next: boolean) => void;
  readonly setUnclaimed: (next: boolean) => void;
  readonly setUnread: (next: boolean) => void;
  /** Whether `issue` passes every active filter but status (see `statuses`). */
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
 * The project page's issue filters, read from and written to the URL - `?label=` and `?status=`
 * (both repeatable), `?q=`, `?needs-you=1`, `?unread=1` - so List and Board share one filter
 * state and a filtered view survives a reload. Every write replaces the history entry, as the
 * label filter always has.
 */
export function useIssueFilters(): IssueFiltersState {
  const [searchParams, setSearchParams] = useSearchParams();
  const labels = useMemo(() => searchParams.getAll("label"), [searchParams]);
  const statuses = useMemo(() => searchParams.getAll("status"), [searchParams]);
  const search = searchParams.get("q") ?? "";
  const needsYou = searchParams.get("needs-you") === "1";
  const unread = searchParams.get("unread") === "1";
  const unclaimed = searchParams.get("unclaimed") === "1";
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
  const setAll = useCallback(
    (name: "label" | "status", next: string[]) =>
      update((params) => {
        params.delete(name);
        for (const value of next) {
          params.append(name, value);
        }
      }),
    [update]
  );
  const setLabels = useCallback((next: string[]) => setAll("label", next), [setAll]);
  const setStatuses = useCallback((next: string[]) => setAll("status", next), [setAll]);
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
    (name: "needs-you" | "unread" | "unclaimed", next: boolean) =>
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
  const setUnclaimed = useCallback((next: boolean) => setFlag("unclaimed", next), [setFlag]);
  const matches = useCallback(
    (issue: IssueSummary, lastReadSequence: number): boolean => {
      const query = search.trim().toLocaleLowerCase();
      return (
        (!needsYou || issue.open_asks > 0) &&
        (!unread || issueIsUnread(issue, lastReadSequence)) &&
        (!unclaimed || issue.claim === null) &&
        (query === "" ||
          issue.key.toLocaleLowerCase().includes(query) ||
          issue.title.toLocaleLowerCase().includes(query))
      );
    },
    [needsYou, search, unclaimed, unread]
  );
  const activeFilterCount =
    labels.length +
    Number(search.trim() !== "") +
    Number(needsYou) +
    Number(unread) +
    Number(unclaimed);
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
    ...(unclaimed ? [{ label: "Unclaimed", remove: () => setUnclaimed(false) }] : []),
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
    setStatuses,
    setUnclaimed,
    setUnread,
    statuses,
    unclaimed,
    unread,
  };
}
