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
import { architectureSourcesQuery, inboxQuery, projectsQuery, userStateQuery } from "./queries";
import { coalescePrefixKeys, refreshQueries } from "./query-refresh";
import type { Event, EventType } from "./types";

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

const inboxQueryKey = inboxQuery().queryKey;
const [inboxKey] = inboxQueryKey;

/** The one documented subtraction, named once so every call site shares it. */
function withoutInbox(keys: (readonly unknown[])[]): (readonly unknown[])[] {
  return keys.filter((key) => key[0] !== inboxKey);
}

function artifactId(event: Event): string | undefined {
  return event.type === "artifact.version" ? event.payload.artifact_id : undefined;
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

// Follower events carry the ask under `ask_id`; every other ask event is the ask itself
// and carries its `id`. Both refresh the ask and its thread (which lists followers).
function appendAskDetailKeys(keys: (readonly unknown[])[], event: Event): void {
  const id =
    event.type === "ask.follower_added" || event.type === "ask.follower_removed"
      ? payloadString(event, "ask_id")
      : payloadString(event, "id");
  if (id !== undefined) {
    keys.push(["ask", id], ["ask-thread", id]);
  }
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

// The ask lifecycle events, listed rather than matched on the `ask.` prefix so a new one falls
// to the default branch instead of silently routing here.
function isAskEvent(event: Event): boolean {
  return (
    event.type === "ask.opened" ||
    event.type === "ask.anchor_refreshed" ||
    event.type === "ask.edited" ||
    event.type === "ask.answered" ||
    event.type === "ask.resolved" ||
    event.type === "ask.follower_added" ||
    event.type === "ask.follower_removed"
  );
}

// The events that change a comment thread: creation, deliveries, lifecycle, suggestion
// verdicts, and a cascaded anchor refresh (a document version orphaned or re-anchored it).
function isCommentLikeEvent(event: Event): boolean {
  return (
    event.type === "comment.created" ||
    event.type === "comment.delivery" ||
    event.type === "comment.answered" ||
    event.type === "comment.resolved" ||
    event.type === "comment.reopened" ||
    event.type === "comment.edited" ||
    event.type === "comment.anchor_refreshed" ||
    event.type === "suggestion.accepted" ||
    event.type === "suggestion.rejected"
  );
}

// The Agents page holds one conversation per session; `session:<id>` with an id after the prefix
// is the only target shape that names one. Every other target (a role, a name that never
// resolved) belongs to no conversation on that page.
function agentConversationKey(target: string | undefined): readonly unknown[] | undefined {
  if (target?.startsWith("session:") && target.length > "session:".length) {
    return ["agents", target.slice("session:".length), "messages"];
  }
  return undefined;
}

function appendAgentConversationKey(
  keys: (readonly unknown[])[],
  target: string | undefined
): void {
  const key = agentConversationKey(target);
  if (key !== undefined) {
    keys.push(key);
  }
}

function isMessageEvent(event: Event): boolean {
  return (
    event.type === "message.created" ||
    event.type === "message.delivery" ||
    event.type === "message.answered"
  );
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

export function eventQueryKeys(event: Event, signedInLogin?: string): (readonly unknown[])[] {
  if (
    event.type === "project.created" ||
    event.type === "project.updated" ||
    event.type === "settings.repo_project.updated"
  ) {
    if (event.project === undefined) {
      throw new Error("project event is missing its project");
    }
    return [
      projectsQuery().queryKey,
      ["project", event.project],
      ["issues", "project", event.project],
      ["repo-projects"],
    ];
  }
  if (
    event.type === "settings.architecture_source.updated" ||
    event.type === "architecture.synced" ||
    event.type === "architecture.sync_failed"
  ) {
    if (event.project === undefined) {
      throw new Error("project event is missing its project");
    }
    return [
      architectureSourcesQuery().queryKey,
      ["architecture-source", event.project],
      ["components", event.project],
      // A re-import changes which components exist, so every count in the tree.
      ["architecture", event.project],
    ];
  }
  if (event.type === "user_state.updated") {
    return payloadString(event, "login") === signedInLogin
      ? [userStateQuery().queryKey, inboxQuery().queryKey]
      : [];
  }
  if (event.type === "subscription.remove_requested") {
    return [];
  }
  if (event.issue_key === null && isMessageEvent(event)) {
    const key = agentConversationKey(payloadString(event, "target"));
    if (key === undefined) {
      throw new Error("issue-less message event is missing its session target");
    }
    return [key];
  }
  if (event.issue_key === null) {
    if (event.artifact_id === null || event.artifact_id === undefined) {
      throw new Error("document event is missing its artifact id");
    }
    if (event.project === undefined) {
      throw new Error("document event is missing its project");
    }
    const keys: (readonly unknown[])[] = [
      ["artifact", event.artifact_id],
      ["artifact-ref"],
      ["project", event.project, "artifacts"],
      projectsQuery().queryKey,
    ];
    if (isAskEvent(event)) {
      appendAskDetailKeys(keys, event);
      keys.push(inboxQuery().queryKey);
    }
    if (isCommentLikeEvent(event)) {
      appendCommentDetailKeys(keys, event);
    }
    if (event.type === "artifact.approved" || event.type === "artifact.changes_requested") {
      keys.push(inboxQuery().queryKey);
    }
    if (event.type === "subscription.removed") {
      keys.push(["subscribers", event.artifact_id]);
    }
    return keys;
  }

  const keys: (readonly unknown[])[] = [
    ["issue", event.issue_key],
    ["events", event.issue_key],
    ["issues"],
    // Unread badges and group membership read last_read_seq from here; every inbound
    // event can change what counts as unread, and answering an ask or reading the log
    // updates this out from under any other open tab watching the same user.
    userStateQuery().queryKey,
    inboxQuery().queryKey,
  ];

  if (
    event.type === "issue.created" ||
    event.type === "issue.updated" ||
    event.type === "issue.closed"
  ) {
    if (event.project !== undefined) {
      keys.push(
        ["issues", "project", event.project],
        // A status change or a new attachment moves a count in the component tree.
        ["components", event.project],
        ["architecture", event.project]
      );
    }
    // Components, parents and subtree rollups are resolved on read up and down the parent
    // chain, so an issue's own event alters ancestors and descendants that get no event of
    // their own: refetch every open issue. The sidebar's per-project open-ask badge moves
    // with a close, a reopen and an assignment, so it refetches here too.
    keys.push(["issue"], projectsQuery().queryKey);
    return keys;
  }
  // The branch above names every `issue.*` type there is, so a type added later falls to the
  // default branch below instead of returning the bare base keys.
  if (event.type === "artifact.created" || event.type === "artifact.version") {
    keys.push(["artifacts", event.issue_key]);
    const id = artifactId(event);
    if (id !== undefined) {
      keys.push(["artifact", id]);
    }
    keys.push(["comments", event.issue_key]);
    return keys;
  }

  if (event.type === "artifact.approved" || event.type === "artifact.changes_requested") {
    keys.push(
      ["artifacts", event.issue_key],
      ["artifact", event.payload.artifact_id],
      ["asks", event.issue_key]
    );
    if (event.payload.ask_id !== null) {
      keys.push(["ask-thread", event.payload.ask_id]);
    }
    return keys;
  }

  if (
    event.type === "ask.opened" ||
    event.type === "ask.answered" ||
    event.type === "ask.resolved" ||
    event.type === "ask.edited" ||
    event.type === "ask.anchor_refreshed"
  ) {
    keys.push(["asks", event.issue_key]);
    // The sidebar's per-project open-ask counts come from GET /projects; an edit or an
    // anchor refresh changes neither whether the ask is open nor which issue owns it.
    if (event.type !== "ask.edited" && event.type !== "ask.anchor_refreshed") {
      keys.push(projectsQuery().queryKey);
    }
    appendDocumentKey(keys, event);
    appendAskDetailKeys(keys, event);
    return keys;
  }
  if (event.type === "ask.follower_added" || event.type === "ask.follower_removed") {
    appendAskDetailKeys(keys, event);
    return keys;
  }
  if (event.type === "block.repaired" || event.type === "block.invalid") {
    keys.push(["asks", event.issue_key], projectsQuery().queryKey);
    return keys;
  }

  if (isCommentLikeEvent(event)) {
    const askID = payloadString(event, "ask_id");
    keys.push(["comments", event.issue_key]);
    // A reply on an ask moves its `waiting_on`; the issue's ask list carries it, and a decision
    // block reads its turn from that list rather than from the Inbox.
    if (askID !== undefined) {
      keys.push(["asks", event.issue_key]);
    }
    appendDocumentKey(keys, event);
    appendCommentDetailKeys(keys, event);
    // The one subtraction from the conservative baseline. An Inbox row is an open ask plus its
    // thread's `last_reply` and `waiting_on` and its issue's priority, assignee and status.
    // Every comment in an ask's thread carries that ask's `ask_id`: both write paths normalise a
    // reply up to the ask at the head of its thread, a `comment.delivery` receipt names the ask
    // its comment belongs to, and migration 0043 moved the reply rows written before that. So a
    // payload with no `ask_id` is a comment outside every ask thread. The row's remaining
    // fields are all ask-scoped, so nothing else it renders can move either - until a field
    // that a non-ask comment does change is added to the row, at which point the payload has
    // to say so and this condition has to read that flag too (LEGION-227 #1251 adds
    // `references_changed` for the backlink count).
    return askID === undefined ? withoutInbox(keys) : keys;
  }

  if (isMessageEvent(event)) {
    const target = payloadString(event, "target");
    appendAgentConversationKey(keys, target);
    keys.push(["messages", event.issue_key]);
    // The Agents page groups a conversation under its thread root, so a reply belongs to that
    // root's conversation and not to a target of its own: a session answering a message aimed
    // at itself names no target at all, and the event carries the root's as `thread_target`.
    const threadTarget = payloadString(event, "thread_target");
    if (threadTarget !== target) {
      appendAgentConversationKey(keys, threadTarget);
    }
    // Same subtraction: a message, a delivery attempt and a message reply change no ask, no
    // ask thread and no issue field an Inbox row reads, whatever session they target.
    return withoutInbox(keys);
  }

  if (event.type === "subscription.removed") {
    keys.push(["subscribers", event.issue_key]);
    return keys;
  }

  // A child's status or parentage changes the `N/M done` rollup in every loaded ancestor.
  keys.push(["children", event.issue_key], ["issue"]);
  return keys;
}

// Leading debounce for burst invalidation: a cold-start replay or a flurry of events
// on one issue invalidates each affected key once, 100ms after the first event of the
// burst — later events join the pending set but do not extend the window.
const INVALIDATION_DEBOUNCE_MS = 100;

// A whole-cache refresh reaches every query the app holds, the GitHub proxy queries included,
// and two callers ask for one: an event this client cannot parse, and a reconnect. Neither is
// rate-limited on its own - a mid-deploy stream of a new event type arrives at the frame rate,
// and `forceReconnect` reopens the stream on every `visibilitychange` - so the throttle belongs
// to "refresh everything" rather than to either caller. Leading and trailing: the first request
// refreshes at once, any inside the window are answered by one trailing refresh, and a change
// no key can be derived from still arrives within the window.
export const WHOLE_CACHE_REFRESH_MS = 5_000;

// `watchdogMs` overrides the no-chunk watchdog window (default WATCHDOG_MS); the
// only caller that ever sets it is a test proving the watchdog reconnects a
// connection that goes silent without erroring — production code always uses the
// default 45s.

/**
 * An ask event that arrives while the Inbox is fetching, for a thread the cache does not hold
 * yet, is invisible to the flush: its `["ask-thread", id]` key matches nothing, and a card that
 * mounts from the landing rows seeds that thread from a body composed before the event.
 * Cancelling a first load does not reach this - a body that lands inside the debounce window is
 * already committed - so the marker makes the card read the ask endpoint instead of trusting the
 * row it mounted from.
 */
function markAskThreadsSeededByAnInFlightInbox(
  queryClient: QueryClient,
  keys: readonly (readonly unknown[])[]
): void {
  if (queryClient.getQueryState(inboxQueryKey)?.fetchStatus !== "fetching") {
    return;
  }
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
    // `pendingAll` is the whole-cache entry of the pending set: it means "something changed
    // that no key can be derived from", and it subsumes every individual key already queued.
    let pendingAll = false;
    // `performance.now()`, not `Date.now()`: a monotonic clock, so a backwards system-clock
    // step cannot park the throttle for the length of the step.
    let lastWholeCacheRefresh = Number.NEGATIVE_INFINITY;
    let wholeCacheTrailing: number | undefined;
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
        // A reconnect may have missed events the stream never saw, so every query the app holds
        // refreshes; TanStack matches all of them when no filter is given. Nothing is excluded:
        // the one query that looks expensive to refresh, `["block-schema"]` with an infinite
        // `staleTime`, resolves from a module-level per-session cache (`features/doc/schema.ts`),
        // so its refetch issues no request. A genuine reconnect after a gap refreshes on the
        // leading edge; `visibilitychange` reopening the stream repeatedly does not.
        refreshEverything();
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

    const scheduleFlush = () => {
      if (flush !== undefined) {
        return;
      }
      flush = window.setTimeout(() => {
        flush = undefined;
        const keys = pendingAll ? undefined : coalescePrefixKeys([...pending.values()]);
        pendingAll = false;
        pending.clear();
        refreshQueries(queryClient, keys);
      }, INVALIDATION_DEBOUNCE_MS);
    };

    const refreshEverythingNow = () => {
      lastWholeCacheRefresh = performance.now();
      pendingAll = true;
      scheduleFlush();
    };

    /** Every whole-cache refresh goes through here, whatever asked for one. */
    const refreshEverything = () => {
      const since = performance.now() - lastWholeCacheRefresh;
      if (since >= WHOLE_CACHE_REFRESH_MS) {
        refreshEverythingNow();
        return;
      }
      if (wholeCacheTrailing !== undefined) {
        return;
      }
      wholeCacheTrailing = window.setTimeout(() => {
        wholeCacheTrailing = undefined;
        refreshEverythingNow();
      }, WHOLE_CACHE_REFRESH_MS - since);
    };

    const queueInvalidations = (keys: readonly (readonly unknown[])[]) => {
      // A latched whole-cache refresh subsumes every key, so there is nothing to record.
      if (!pendingAll) {
        for (const key of keys) {
          pending.set(JSON.stringify(key), key);
        }
      }
      scheduleFlush();
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
      // `Object.hasOwn`, not `in`: `knownEventTypes` inherits `Object.prototype`, so `in` reads
      // a frame named `constructor` or `toString` as a known type and parses it as an event.
      if (raw.event === undefined || !Object.hasOwn(knownEventTypes, raw.event)) {
        // The server may add an event this client cannot parse. Refresh everything rather than
        // leaving a rendered field stale until the next reconnect.
        refreshEverything();
        return;
      }
      const event = JSON.parse(raw.data) as Event;
      prependEventToLog(queryClient, event);
      const signedInLogin = queryClient.getQueryData<{ login?: string }>(["whoami"])?.login;
      const keys = eventQueryKeys(event, signedInLogin);
      markAskThreadsSeededByAnInFlightInbox(queryClient, keys);
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
      window.clearTimeout(wholeCacheTrailing);
      pending.clear();
      controller?.abort();
      controller = null;
      setConnectionState("connected");
    };
  }, [queryClient, watchdogMs]);
}
