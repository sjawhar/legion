import { type QueryClient, useQueryClient } from "@tanstack/react-query";
import { useEffect } from "react";

import { reaskHeldCredentialList } from "../features/credentials/pending";
import { markPendingAskThreadInvalidation } from "../features/inbox/ask-thread-freshness";

import { eventRefreshTargets } from "./event-refresh";
import {
  EventStreamHttpError,
  readEventStream,
  reconnectDelayMs,
  type StreamEvent,
  type StreamOpened,
  setConnectionState,
} from "./live";
import { inboxQuery } from "./queries";
import { type RefreshTarget, refreshQueries, trackQueryFetches } from "./query-refresh";
import type { Event, EventType } from "./types";

const knownEventTypes: Record<EventType, true> = {
  "project.created": true,
  "project.updated": true,
  "settings.repo_project.updated": true,
  "settings.project_default_assignee.updated": true,
  "settings.architecture_source.updated": true,
  "architecture.synced": true,
  "architecture.sync_failed": true,
  "user_state.updated": true,
  "user_agent_state.updated": true,
  "user_thread_state.updated": true,
  "issue.created": true,
  "issue.updated": true,
  "issue.closed": true,
  "issue.claimed": true,
  "issue.released": true,
  "issue.deployed": true,
  "artifact.created": true,
  "artifact.version": true,
  "artifact.approved": true,
  "artifact.changes_requested": true,
  "ask.opened": true,
  "ask.anchor_refreshed": true,
  "ask.edited": true,
  "ask.handed_back": true,
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
  "message.accepted": true,
  "message.answered": true,
  "child.status": true,
  "child.added": true,
  "child.removed": true,
  "subscription.remove_requested": true,
  "subscription.removed": true,
};

// One window bounds every stream attempt: armed when the attempt starts, re-armed when its
// response headers arrive and on every chunk (application events or heartbeat comments), and
// cleared when the attempt settles. An attempt that receives nothing for this long is aborted
// and reconnects with the backoff, whether its request never got headers or its open connection
// went silent. The server sends a `: heartbeat` comment every 15s, so 45s is three missed
// heartbeats.
const WATCHDOG_MS = 45_000;

const inboxQueryKey = inboxQuery().queryKey;

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

// Leading window for a burst: a cold-start replay or a flurry of events on one issue is handed to
// the refresh scheduler (`api/query-refresh.ts`) as one batch, 100ms after the first event of the
// burst - later events join the pending batch but do not extend the window.
const BATCH_WINDOW_MS = 100;

// A whole-cache refresh reaches every query the app holds, the GitHub proxy queries included,
// and two callers ask for one: an event this client cannot parse, and a reconnect. Neither is
// rate-limited on its own - a mid-deploy stream of a new event type arrives at the frame rate,
// and `forceReconnect` reopens the stream on every `visibilitychange` - so the throttle belongs
// to "refresh everything" rather than to either caller. Every request schedules the one pending
// refresh: a request a window or more after the last one schedules it for now, and any inside
// the window joins that same pending refresh, so a change no key can be derived from always
// arrives within the window of the request that asked for it.
// The backlink panels' own `["references", "to", …]` queries are part of "everything", so a
// reconnect refreshes them too.
// Exported for the one test that deliberately runs at the production window rather than an
// injected one, so it reads the real number instead of keeping a copy that drifts.
export const WHOLE_CACHE_REFRESH_MS = 5_000;

/**
 * An ask event that arrives while the Inbox is fetching, for a thread the cache does not hold
 * yet, is invisible to the flush: its `["ask-thread", id]` key matches nothing, and a card that
 * mounts from the landing rows seeds that thread from a body composed before the event. The
 * Inbox's own refetch once its load settles does not reach this - a card can mount from the
 * landing rows first - so the marker makes the card read the ask endpoint instead of trusting
 * the row it mounted from.
 */
function markAskThreadsSeededByAnInFlightInbox(
  queryClient: QueryClient,
  targets: readonly RefreshTarget[]
): void {
  if (queryClient.getQueryState(inboxQueryKey)?.fetchStatus !== "fetching") {
    return;
  }
  for (const { queryKey } of targets) {
    if (
      queryKey[0] === "ask-thread" &&
      typeof queryKey[1] === "string" &&
      queryClient.getQueryState(queryKey) === undefined
    ) {
      markPendingAskThreadInvalidation(queryClient, queryKey[1]);
    }
  }
}

/**
 * The batch without the repeats of a target that has no `when`: events on one issue each push
 * the same targets, and every copy past the first is one more full-cache `findAll` at the flush
 * that reaches nothing the first did not. A target with a `when` always stays, since its
 * predicate cannot be compared by key. First-seen order is kept.
 */
function dedupeRefreshTargets(targets: readonly RefreshTarget[]): RefreshTarget[] {
  const seen = new Set<string>();
  return targets.filter((target) => {
    if (target.when !== undefined) {
      return true;
    }
    const key = `${target.exact === true} ${JSON.stringify(target.queryKey)}`;
    if (seen.has(key)) {
      return false;
    }
    seen.add(key);
    return true;
  });
}

/**
 * `watchdogMs` overrides the attempt watchdog's window (`WATCHDOG_MS`) and `wholeCacheRefreshMs`
 * the whole-cache refresh throttle; the only callers that set either are tests that would
 * otherwise have to wait out the real 45 s and 5 s windows. Production calls this with no
 * argument.
 */
export function useEventStream({
  watchdogMs = WATCHDOG_MS,
  wholeCacheRefreshMs = WHOLE_CACHE_REFRESH_MS,
}: {
  watchdogMs?: number;
  wholeCacheRefreshMs?: number;
} = {}): void {
  const queryClient = useQueryClient();

  useEffect(() => {
    let stopped = false;
    let controller: AbortController | null = null;
    let forced = false;
    let attempt = 0;
    // Whether any attempt has ended, by dropping after it opened or by failing before. Only the
    // page's first attempt opens with nothing to refresh, since the page made its own reads
    // alongside it; every later open follows a gap in which no stream carried what committed.
    let anAttemptEnded = false;
    let lastEventId = 0;
    let watchdog: number | undefined;
    let reconnect: number | undefined;
    let flush: number | undefined;
    // `pendingAll` is the whole-cache entry of the pending batch: it means "something changed
    // that no target can be derived from", and it subsumes every target already queued.
    let pendingAll = false;
    // `performance.now()`, not `Date.now()`: a monotonic clock, so a backwards system-clock
    // step cannot park the throttle for the length of the step.
    let lastWholeCacheRefresh = Number.NEGATIVE_INFINITY;
    let wholeCacheTrailing: number | undefined;
    let pending: RefreshTarget[] = [];
    // Record when each query's fetches begin from now on, so the scheduler's one-a-second rule
    // holds for the first batch after the page's own loads too.
    trackQueryFetches(queryClient);

    const armWatchdog = () => {
      window.clearTimeout(watchdog);
      watchdog = window.setTimeout(() => controller?.abort(), watchdogMs);
    };

    const onOpen = ({ replayComplete }: StreamOpened) => {
      if (stopped) {
        return;
      }
      setConnectionState("connected");
      armWatchdog();
      if (anAttemptEnded) {
        // Events may have committed since the prior attempt ended - or, when the first attempt
        // failed, since the page's own reads - so every query the app holds refreshes, unless the
        // server says this stream replays every event after the cursor (a resuming connection
        // whose cursor the log still holds): those events refresh what they changed, as any
        // other does. A cold connect, a reset log and a server older than the header say no such
        // thing. Nothing is excluded from the refresh: the one query that looks expensive,
        // `["block-schema"]` with an infinite `staleTime`, resolves from a module-level
        // per-session cache (`features/doc/schema.ts`), so its refetch issues no request. A
        // genuine reconnect after a gap refreshes on the leading edge; `visibilitychange`
        // reopening the stream repeatedly does not.
        if (!replayComplete) {
          refreshEverything();
        }
        // The one query a refresh skips: a credential list held at `null` (no broker), which a
        // server restarted since with a broker configured would now answer with a list. A
        // restart replays nothing that says so.
        reaskHeldCredentialList(queryClient);
      }
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
        const targets = pendingAll ? undefined : dedupeRefreshTargets(pending);
        pendingAll = false;
        pending = [];
        refreshQueries(queryClient, targets);
      }, BATCH_WINDOW_MS);
    };

    /**
     * Every whole-cache refresh goes through here, whatever asked for one. A request outside the
     * window refreshes at once; one inside it schedules the single trailing refresh, which is
     * what the `setTimeout` of zero or less does for the leading edge too.
     */
    const refreshEverything = () => {
      if (wholeCacheTrailing !== undefined) {
        return;
      }
      // Clamped: any request more than one window after the last refresh - the first of a
      // connection, which has none to measure from, most of all - leaves the delay negative.
      // Browsers clamp a negative timeout to zero; saying so here keeps the intent explicit and
      // the runtime from warning about it.
      const remaining = Math.max(
        0,
        wholeCacheRefreshMs - (performance.now() - lastWholeCacheRefresh)
      );
      wholeCacheTrailing = window.setTimeout(() => {
        wholeCacheTrailing = undefined;
        lastWholeCacheRefresh = performance.now();
        pendingAll = true;
        scheduleFlush();
      }, remaining);
    };

    const queueRefresh = (targets: readonly RefreshTarget[]) => {
      // A latched whole-cache refresh subsumes every target, so there is nothing to record.
      if (!pendingAll) {
        pending.push(...targets);
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
      const targets = eventRefreshTargets(event, signedInLogin);
      markAskThreadsSeededByAnInFlightInbox(queryClient, targets);
      queueRefresh(targets);
    };

    const open = () => {
      window.clearTimeout(reconnect);
      reconnect = undefined;
      const current = new AbortController();
      controller = current;
      armWatchdog();
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
      anAttemptEnded = true;
      if (!current.signal.aborted && error instanceof EventStreamHttpError) {
        if (error.status === 401) {
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
      pending = [];
      controller?.abort();
      controller = null;
      setConnectionState("connected");
    };
  }, [queryClient, watchdogMs, wholeCacheRefreshMs]);
}
