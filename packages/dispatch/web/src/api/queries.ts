import { keepPreviousData, type QueryFunctionContext, queryOptions } from "@tanstack/react-query";

import { beginInboxFetch, finishInboxFetch } from "../features/inbox/ask-thread-freshness";
import { projectIssuesQueryKey } from "../features/project/issue-filters";

import { api } from "./client";
import { deliveryLiveFor } from "./delivery-live";
import type {
  DeliveryLiveView,
  DeliveryMeasuresResponse,
  DeliveryPipelineResponse,
  DeliveryTimelineResponse,
  InboxRow,
} from "./types";

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

/** The viewer's per-agent conversation state: each session's Clear, read mark, and count of
 *  unread replies to the viewer's direct messages. The navigation, the Agents page, and the
 *  live view all read this one key. */
export const userAgentStateQuery = () =>
  queryOptions({
    queryKey: ["user-agent-state"],
    queryFn: () => api.getMyAgentState(),
  });

/** The prefix every Replies query shares: the page, its count, and each owner's unread set. One
 *  invalidation of it refreshes them all. */
export const myRepliesKey = ["me", "replies"] as const;

/** The viewer's count of unread threads, from the one-row page's `total`: the header's and the
 *  sidebar's `Replies N`. */
export const myRepliesCountQuery = () =>
  queryOptions({
    queryKey: [...myRepliesKey, "count"],
    queryFn: async () => (await api.listMyReplies({ limit: 1 })).total,
  });

/** One owner's unread threads, which every thread mounted on that owner's page reads to learn
 *  whether it is unread: one request however many threads the page shows. */
export const myRepliesOwnerQuery = (
  owner: { readonly issue: string } | { readonly artifact: string }
) =>
  queryOptions({
    queryKey: [...myRepliesKey, owner],
    queryFn: () => api.listMyReplies({ ...owner, limit: 200 }),
  });

/** One session's stored conversations: the direct messages and issue messages sent to it, with
 *  their replies. The Agents page row and the live view read this one key. */
export const agentMessagesQuery = (sessionId: string) =>
  queryOptions({
    queryKey: ["agents", sessionId, "messages"],
    queryFn: () => api.listAgentMessages(sessionId),
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

/** What every live-owned delivery query shares (LEGION-690): its first load subscribes its view
 *  over the page's one delivery socket (`api/delivery-live.ts`), which writes the snapshot and every
 *  later change into the cache, so nothing here reads again. `meta.live` keeps the event stream's
 *  refresh paths away from it (`api/query-refresh.ts`); `gcTime` 0 drops the subscription with the
 *  last observer; the page keeps the previous answer on screen while a new view's snapshot is out,
 *  as a filter change in the prototype it ports never blanks it. */
const LIVE = {
  gcTime: 0,
  meta: { live: true },
  refetchOnReconnect: false,
  refetchOnWindowFocus: false,
  staleTime: Number.POSITIVE_INFINITY,
} as const;

/** The live view's answer, which is the body of the GET route its kind mirrors. */
function subscribeLive<B>(context: QueryFunctionContext, view: DeliveryLiveView): Promise<B> {
  // The socket answers each view with its own route's body (packages/contracts DeliveryLiveView),
  // which the kind the caller passes names; the frame carries the union.
  return deliveryLiveFor(context.client).subscribe(context.queryKey, view) as Promise<B>;
}

/** The delivery timeline, live: the window, the search and every facet, applied by Dispatch
 *  (LEGION-567's plan, "API"), keyed by the view so a filter change subscribes its own entry. */
export const deliveryTimelineQuery = (view: DeliveryLiveView & { kind: "timeline" }) =>
  queryOptions({
    ...LIVE,
    placeholderData: keepPreviousData,
    queryKey: ["delivery", "timeline", view],
    queryFn: (context) => subscribeLive<DeliveryTimelineResponse>(context, view),
  });

/** The measures panel's own read for a brushed window, live, under `["delivery"]` beside the
 *  timeline and by the same search and facets. */
export const deliveryMeasuresQuery = (view: DeliveryLiveView & { kind: "measures" }) =>
  queryOptions({
    ...LIVE,
    placeholderData: keepPreviousData,
    queryKey: ["delivery", "measures", view],
    queryFn: (context) => subscribeLive<DeliveryMeasuresResponse>(context, view),
  });

/** One deploy-repository run with every job it ran, read by GET. A live delta that changes the
 *  run invalidates this key, so an open drill-down reads it again. */
export const deliveryRunQuery = (id: number) =>
  queryOptions({
    queryKey: ["delivery", "run", id],
    queryFn: () => api.getDeliveryRun(id),
  });

/** The Pipeline page, live, by `at` so a pinned day is its own subscription. */
export const deliveryPipelineQuery = (view: DeliveryLiveView & { kind: "pipeline" }) =>
  queryOptions({
    ...LIVE,
    queryKey: ["delivery", "pipeline", view],
    queryFn: (context) => subscribeLive<DeliveryPipelineResponse>(context, view),
  });

/** The delivery timeline's configuration record, or `null` until someone sets it: the Settings
 *  section's form reads it, while the Delivery page's setup form starts from the `null` the
 *  timeline's `DELIVERY_NOT_CONFIGURED` already implies and reads nothing. */
export const deliverySettingsQuery = () =>
  queryOptions({
    queryKey: ["delivery-settings"],
    queryFn: () => api.getDeliverySettings(),
  });
