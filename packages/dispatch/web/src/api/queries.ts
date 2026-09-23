import { type QueryFunctionContext, queryOptions } from "@tanstack/react-query";

import { beginInboxFetch, finishInboxFetch } from "../features/inbox/ask-thread-freshness";
import { projectIssuesQueryKey } from "../features/project/issue-filters";

import { api } from "./client";
import type { InboxRow } from "./types";

/** The signed-in principal (`/auth/whoami`). One key shared by every surface that reads it. */
export const whoAmIQuery = () =>
  queryOptions({
    queryKey: ["whoami"],
    queryFn: () => api.whoAmI(),
  });

/** Every ask the viewer can see, as the Inbox lists them. */
export const inboxQuery = () =>
  queryOptions({
    queryKey: ["inbox"],
    queryFn: async ({ client }) => {
      const fetch = beginInboxFetch(client);
      let asks: InboxRow[] | undefined;
      try {
        asks = await api.getInbox();
        return asks;
      } finally {
        finishInboxFetch(client, fetch, asks);
      }
    },
  });

/** The viewer's per-issue read/pin/dismiss state. */
export const userStateQuery = () =>
  queryOptions({
    queryKey: ["user-state"],
    queryFn: () => api.getMyState(),
  });

/** Every native project, shared by the shell, project routes, and settings. */
export const projectsQuery = () =>
  queryOptions({
    queryKey: ["projects"],
    queryFn: () => api.listProjects(),
  });

/** Every configured architecture source, shared by its settings table and invalidations. */
export const architectureSourcesQuery = () =>
  queryOptions({
    queryKey: ["architecture-sources"],
    queryFn: () => api.listArchitectureSources(),
  });

/** The mutation key every board move on `project` carries; `projectIssuesQuery` waits on it. */
export const boardMoveMutationKey = (project: string): readonly unknown[] => [
  "board-move",
  project,
];

/**
 * A project's issue list - plain, or server-filtered by labels - shared by the Board, the List,
 * the filter strip and the issue header's label picker. Every observer sets the query's function,
 * so each one takes it from here. A fetch that starts while a board move on the project is
 * unanswered waits for the answer: until then the server may not have applied the move, and its
 * list would put the card back where it came from.
 */
export const projectIssuesQuery = (project: string, labels: readonly string[]) =>
  queryOptions({
    queryKey: projectIssuesQueryKey(project, labels),
    queryFn: async (context) => {
      await boardMovesSettled(context, project);
      return api.listIssues(labels.length === 0 ? { project } : { labels, project });
    },
  });

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
