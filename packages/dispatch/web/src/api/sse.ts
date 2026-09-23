import { type QueryClient, useQueryClient } from "@tanstack/react-query";
import { useEffect } from "react";

import { markPendingAskThreadInvalidation } from "../features/inbox/ask-thread-freshness";
import {
  EventStreamHttpError,
  readEventStream,
  reconnectDelayMs,
  type StreamEvent,
  setConnectionState,
} from "./live";
import { inboxQuery, projectsQuery, userStateQuery } from "./queries";
import type { Event, EventType, IssueSummary } from "./types";

const knownEventTypes: Record<EventType, true> = {
  "project.created": true,
  "project.updated": true,
  "settings.repo_project.updated": true,
  "settings.architecture_source.updated": true,
  "architecture.synced": true,
  "architecture.sync_failed": true,
  "user_state.updated": true,
  "issue.created": true,
  "issue.updated": true,
  "issue.closed": true,
  "artifact.created": true,
  "artifact.version": true,
  "artifact.approved": true,
  "artifact.changes_requested": true,
  "ask.opened": true,
  "ask.anchor_refreshed": true,
  "ask.edited": true,
  "ask.answered": true,
  "ask.resolved": true,
  "ask.follower_added": true,
  "ask.follower_removed": true,
  "block.repaired": true,
  "block.invalid": true,
  "comment.created": true,
  "comment.anchor_refreshed": true,
  "comment.delivery": true,
  "comment.answered": true,
  "comment.resolved": true,
  "comment.reopened": true,
  "comment.edited": true,
  "suggestion.accepted": true,
  "suggestion.rejected": true,
  "message.created": true,
  "message.delivery": true,
  "message.answered": true,
  "child.status": true,
  "child.added": true,
  "child.removed": true,
  "subscription.remove_requested": true,
  "subscription.removed": true,
};

// A dead connection reveals no client-visible signal until this much time passes with
// no bytes at all (application events or heartbeat comments): the server sends a `:
// heartbeat` comment every 15s, so 45s is three missed heartbeats.
const WATCHDOG_MS = 45_000;

export interface QueryInvalidator {
  invalidateQueries(filters: { queryKey: readonly unknown[] }): unknown;
}

// The document an ask or comment event touches: a block ask names the document that holds its
// block, an anchored ask or comment names the document its mark lives in, and an ask or comment
// with neither touches no document at all.
function appendDocumentKey(keys: (readonly unknown[])[], event: Event): void {
  let id: string | undefined;
  switch (event.type) {
    case "ask.opened":
    case "ask.edited":
    case "ask.answered":
    case "ask.resolved":
    case "ask.anchor_refreshed":
      id = event.payload.block_artifact?.id ?? event.payload.anchor?.artifact_id;
      break;
    case "comment.created":
    case "comment.delivery":
    case "comment.answered":
    case "comment.resolved":
    case "comment.reopened":
    case "comment.edited":
    case "comment.anchor_refreshed":
    case "suggestion.accepted":
    case "suggestion.rejected":
      id = "anchor" in event.payload ? event.payload.anchor?.artifact_id : undefined;
      break;
    default:
      throw new Error(`${event.type} events do not name a document`);
  }
  if (id !== undefined) {
    keys.push(["artifact", id]);
  }
}

function payloadString(event: Event, key: string): string | undefined {
  const payload = event.payload;
  if (typeof payload !== "object" || payload === null || !(key in payload)) {
    return undefined;
  }
  const value = Reflect.get(payload, key);
  return typeof value === "string" ? value : undefined;
}

function appendCommentDetailKeys(keys: (readonly unknown[])[], event: Event): void {
  const id =
    event.type === "comment.delivery" ? event.payload.comment_id : payloadString(event, "id");
  if (id !== undefined) {
    keys.push(["comment", id]);
  }
  const askID = payloadString(event, "ask_id");
  if (askID !== undefined) {
    keys.push(["ask", askID], ["ask-thread", askID]);
  }
}

// 408 (timeout) and 429 (rate limit) are transient — worth retrying. Every other
// 4xx (404, 409, 422, ...) means the request itself can never succeed unchanged, so
// backing off and reconnecting forever would just spin instead of ever recovering.
function isTerminalHttpStatus(status: number): boolean {
  return status >= 400 && status < 500 && status !== 408 && status !== 429;
}

export interface EventPages {
  pageParams: unknown[];
  pages: Event[][];
}

/**
 * Reconciles a fetched log with the cached one. The stream prepends each event the moment it is
 * published, while the refetch its burst starts reads the log a little later - or, under load, a
 * little *earlier*: a response whose head is older than the cached head predates turns the
 * stream already delivered, and replacing the page with it would make those turns vanish until
 * the next refetch. The response is authoritative for the range it covers; the cached events
 * above its head stay in front of it. A response at or beyond the cached head replaces the page.
 */
export function mergeEventPages(current: EventPages | undefined, incoming: EventPages): EventPages {
  const cached = current?.pages[0];
  const fetched = incoming.pages[0];
  if (cached === undefined || fetched === undefined) {
    return incoming;
  }
  const fetchedHead = fetched[0]?.seq ?? 0;
  const newer = cached.filter((event) => event.seq > fetchedHead);
  if (newer.length === 0) {
    return incoming;
  }
  return { ...incoming, pages: [[...newer, ...fetched], ...incoming.pages.slice(1)] };
}

export function prependEventToLog(queryClient: QueryClient, event: Event): void {
  if (event.issue_key === null) {
    return;
  }
  queryClient.setQueryData<EventPages>(["events", event.issue_key], (current) => {
    if (current === undefined) {
      return current;
    }
    const first = current.pages[0] ?? [];
    if (event.seq <= (first[0]?.seq ?? 0)) {
      return current;
    }
    return {
      ...current,
      pages: [[event, ...first], ...current.pages.slice(1)],
    };
  });
}

type QueryKey = readonly unknown[];

function issueDetailKeys(event: Event): QueryKey[] {
  if (event.issue_key === null) {
    throw new Error(`${event.type} event is missing its issue key`);
  }
  return [
    ["issue", event.issue_key],
    ["events", event.issue_key],
  ];
}

function documentOwnerKeys(event: Event): QueryKey[] {
  if (event.artifact_id === null || event.artifact_id === undefined) {
    throw new Error(`${event.type} document event is missing its artifact id`);
  }
  if (event.project === undefined) {
    throw new Error(`${event.type} document event is missing its project`);
  }
  return [
    ["artifact", event.artifact_id],
    ["artifact-ref"],
    ["project", event.project, "artifacts"],
    projectsQuery().queryKey,
  ];
}
function messageKeys(
  event: Extract<Event, { type: "message.created" | "message.delivery" | "message.answered" }>
): QueryKey[] {
  if (event.issue_key === null) {
    const target = payloadString(event, "target");
    if (
      target === undefined ||
      !target.startsWith("session:") ||
      target.length === "session:".length
    ) {
      throw new Error("issue-less message event is missing its session target");
    }
    return [["agents", target.slice("session:".length), "messages"]];
  }

  const keys = issueDetailKeys(event);
  const target = payloadString(event, "target");
  if (target?.startsWith("session:") && target.length > "session:".length) {
    keys.push(["agents", target.slice("session:".length), "messages"]);
  }
  keys.push(["messages", event.issue_key]);
  return keys;
}

function nonAskCommentKeys(
  event: Extract<
    Event,
    {
      type:
        | "comment.created"
        | "comment.anchor_refreshed"
        | "comment.delivery"
        | "comment.answered"
        | "comment.resolved"
        | "comment.reopened"
        | "comment.edited"
        | "suggestion.accepted"
        | "suggestion.rejected";
    }
  >
): QueryKey[] {
  const keys = event.issue_key === null ? documentOwnerKeys(event) : issueDetailKeys(event);
  if (event.issue_key !== null) {
    keys.push(["comments", event.issue_key]);
  }
  appendDocumentKey(keys, event);
  appendCommentDetailKeys(keys, event);
  if (
    event.type === "comment.created" ||
    event.type === "comment.edited" ||
    event.type === "comment.answered" ||
    event.type === "comment.anchor_refreshed"
  ) {
    keys.push(["references"]);
  }
  return keys;
}

function eventQueryKeys(event: Event): QueryKey[] {
  if (
    event.type === "message.created" ||
    event.type === "message.delivery" ||
    event.type === "message.answered"
  ) {
    const keys = messageKeys(event);
    if (event.type === "message.created" || event.type === "message.answered") {
      keys.push(["references"]);
    }
    return keys;
  }
  if (
    (event.type === "comment.created" ||
      event.type === "comment.anchor_refreshed" ||
      event.type === "comment.delivery" ||
      event.type === "comment.answered" ||
      event.type === "comment.resolved" ||
      event.type === "comment.reopened" ||
      event.type === "comment.edited" ||
      event.type === "suggestion.accepted" ||
      event.type === "suggestion.rejected") &&
    payloadString(event, "ask_id") === undefined
  ) {
    return nonAskCommentKeys(event);
  }
  return [...reconnectInvalidationKeys];
}

export function applyEventInvalidations(queryClient: QueryInvalidator, event: Event): void {
  for (const key of eventQueryKeys(event)) {
    queryClient.invalidateQueries({ queryKey: key });
  }
}

// Leading debounce for burst invalidation: a cold-start replay or a flurry of events
// on one issue invalidates each affected key once, 100ms after the first event of the
// burst — later events join the pending set but do not extend the window.
const INVALIDATION_DEBOUNCE_MS = 100;

// Every list and detail query the app holds: a reconnect may have missed events the
// stream never saw, so all of them refresh when a stream reopens after a live one.
const reconnectInvalidationKeys: readonly (readonly unknown[])[] = [
  ["issues"],
  inboxQuery().queryKey,
  userStateQuery().queryKey,
  ["issue"],
  ["events"],
  projectsQuery().queryKey,
  ["project"],
  ["artifacts"],
  ["artifact"],
  ["artifact-ref"],
  ["asks"],
  ["ask"],
  ["ask-thread"],
  ["comments"],
  ["comment"],
  ["messages"],
  ["subscribers"],
  ["children"],
  ["artifact-reviews"],
  ["components"],
  ["architecture"],
  ["architecture-sources"],
  ["architecture-source"],
  ["repo-projects"],
  ["agents"],
  ["references"],
];

function patchCachedIssueSequence(queryClient: QueryClient, event: Event): void {
  if (event.issue_key === null) {
    return;
  }
  queryClient.setQueriesData<IssueSummary[]>({ queryKey: ["issues"] }, (current) =>
    current?.map((issue) =>
      issue.key === event.issue_key
        ? { ...issue, last_seq: Math.max(issue.last_seq, event.seq) }
        : issue
    )
  );
}

// `watchdogMs` overrides the no-chunk watchdog window (default WATCHDOG_MS); the
// only caller that ever sets it is a test proving the watchdog reconnects a
// connection that goes silent without erroring — production code always uses the
// default 45s.

export function useEventStream(watchdogMs: number = WATCHDOG_MS): void {
  const queryClient = useQueryClient();

  useEffect(() => {
    let stopped = false;
    let controller: AbortController | null = null;
    let forced = false;
    let attempt = 0;
    let hasOpenedOnce = false;
    let lastEventId = 0;
    let watchdog: number | undefined;
    let reconnect: number | undefined;
    let flush: number | undefined;
    const pending = new Map<string, readonly unknown[]>();

    const armWatchdog = () => {
      window.clearTimeout(watchdog);
      watchdog = window.setTimeout(() => controller?.abort(), watchdogMs);
    };

    const onOpen = () => {
      if (stopped) {
        return;
      }
      setConnectionState("connected");
      armWatchdog();
      if (hasOpenedOnce) {
        for (const key of reconnectInvalidationKeys) {
          queryClient.invalidateQueries({ queryKey: key });
        }
      }
      hasOpenedOnce = true;
      attempt = 0;
    };

    const onChunk = () => {
      if (stopped) {
        return;
      }
      armWatchdog();
    };

    const queueInvalidations = (keys: readonly QueryKey[]) => {
      for (const key of keys) {
        pending.set(JSON.stringify(key), key);
      }
      if (flush !== undefined) {
        return;
      }
      flush = window.setTimeout(() => {
        flush = undefined;
        for (const key of pending.values()) {
          queryClient.invalidateQueries({ queryKey: key });
        }
        pending.clear();
      }, INVALIDATION_DEBOUNCE_MS);
    };

    const onEvent = (raw: StreamEvent) => {
      if (stopped) {
        return;
      }
      // The cursor advances before the frame is parsed on purpose: a frame the client
      // cannot parse rejects `readEventStream` and is skipped on the reconnect instead
      // of being replayed forever.
      const id = Number(raw.id);
      if (Number.isFinite(id) && id > lastEventId) {
        lastEventId = id;
      }
      if (raw.event === undefined || !(raw.event in knownEventTypes)) {
        // The server may add an event before this client understands its payload. Refresh the
        // complete reconnect surface rather than silently leaving a rendered field stale.
        queueInvalidations(reconnectInvalidationKeys);
        return;
      }
      const event = JSON.parse(raw.data) as Event;
      patchCachedIssueSequence(queryClient, event);
      prependEventToLog(queryClient, event);
      const keys = eventQueryKeys(event);
      if (queryClient.getQueryState(["inbox"])?.fetchStatus === "fetching") {
        for (const key of keys) {
          if (
            key[0] === "ask-thread" &&
            typeof key[1] === "string" &&
            queryClient.getQueryState(key) === undefined
          ) {
            markPendingAskThreadInvalidation(queryClient, key[1]);
          }
        }
      }
      queueInvalidations(keys);
    };

    const open = () => {
      window.clearTimeout(reconnect);
      reconnect = undefined;
      const current = new AbortController();
      controller = current;
      const url = lastEventId > 0 ? `/api/v1/events?since=${lastEventId}` : "/api/v1/events";
      void readEventStream(url, { onChunk, onEvent, onOpen, signal: current.signal }).then(
        () => settle(current, undefined),
        (error: unknown) => settle(current, error)
      );
    };

    const settle = (current: AbortController, error: unknown) => {
      if (stopped) {
        return;
      }
      window.clearTimeout(watchdog);
      watchdog = undefined;
      controller = null;
      if (!current.signal.aborted && error instanceof EventStreamHttpError) {
        if (error.status === 401 || error.status === 403) {
          setConnectionState("signed-out");
          queryClient.invalidateQueries({ queryKey: ["whoami"] });
          removeListeners();
          return;
        }
        if (isTerminalHttpStatus(error.status)) {
          setConnectionState("unavailable");
          removeListeners();
          return;
        }
      }
      if (forced) {
        forced = false;
        open();
        return;
      }
      setConnectionState("reconnecting");
      reconnect = window.setTimeout(open, reconnectDelayMs(attempt));
      attempt += 1;
    };

    const forceReconnect = () => {
      if (stopped) {
        return;
      }
      attempt = 0;
      if (controller !== null) {
        forced = true;
        controller.abort();
        return;
      }
      open();
    };

    const handleVisibilityChange = () => {
      if (document.visibilityState === "visible") {
        forceReconnect();
      }
    };
    const handleOffline = () => controller?.abort();

    const removeListeners = () => {
      document.removeEventListener("visibilitychange", handleVisibilityChange);
      window.removeEventListener("online", forceReconnect);
      window.removeEventListener("offline", handleOffline);
    };

    setConnectionState("connecting");
    document.addEventListener("visibilitychange", handleVisibilityChange);
    window.addEventListener("online", forceReconnect);
    window.addEventListener("offline", handleOffline);
    open();

    return () => {
      stopped = true;
      removeListeners();
      window.clearTimeout(watchdog);
      window.clearTimeout(reconnect);
      window.clearTimeout(flush);
      pending.clear();
      controller?.abort();
      controller = null;
      setConnectionState("connected");
    };
  }, [queryClient, watchdogMs]);
}
