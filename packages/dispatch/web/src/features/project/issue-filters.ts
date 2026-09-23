import { type QueryFunctionContext, queryOptions } from "@tanstack/react-query";
import { useCallback, useMemo } from "react";
import { useSearchParams } from "react-router-dom";

import { api } from "../../api/client";
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
  readonly activeFilterCount: number;
  readonly activeFilters: ActiveFilter[];
  readonly setLabels: (next: string[]) => void;
  readonly setStatuses: (next: string[]) => void;
  readonly setSearch: (next: string) => void;
  readonly setNeedsYou: (next: boolean) => void;
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

/** The mutation key every board move on `project` carries; `projectIssuesQuery` waits on it. */
export function boardMoveMutationKey(project: string): readonly unknown[] {
  return ["board-move", project];
}

/**
 * A project's issue list - the one query the Board, the List and the filter strip observe. Every
 * observer sets the query's function, so each one takes it from here. A fetch that starts while a
 * board move on the project is unanswered waits for the answer: until then the server may not have
 * applied the move, and its list would put the card back where it came from.
 */
export function projectIssuesQuery(project: string, labels: readonly string[]) {
  return queryOptions({
    queryKey: projectIssuesQueryKey(project, labels),
    queryFn: async (context) => {
      await boardMovesSettled(context, project);
      return api.listIssues(labels.length === 0 ? { project } : { labels, project });
    },
  });
}

/** Resolves once no board move on `project` is pending; rejects if the fetch is cancelled first. */
function boardMovesSettled(context: QueryFunctionContext, project: string): Promise<void> {
  const { client } = context;
  const filters = { mutationKey: boardMoveMutationKey(project) };
  if (client.isMutating(filters) === 0) {
    return Promise.resolve();
  }
  // Read only here: once a fetch reads its signal, query-core cancels it when the last observer
  // unmounts, which a fetch that is not waiting has no reason to invite.
  const { signal } = context;
  const settled = Promise.withResolvers<void>();
  const stop = () => {
    unsubscribe();
    signal.removeEventListener("abort", abort);
  };
  const abort = () => {
    stop();
    settled.reject(signal.reason);
  };
  const unsubscribe = client.getMutationCache().subscribe(() => {
    if (client.isMutating(filters) === 0) {
      stop();
      settled.resolve();
    }
  });
  signal.addEventListener("abort", abort);
  return settled.promise;
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
    setStatuses,
    setUnread,
    statuses,
    unread,
  };
}
