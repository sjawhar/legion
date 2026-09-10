import { type QueryClient, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef } from "react";

import {
  EventStreamHttpError,
  readEventStream,
  reconnectDelayMs,
  type StreamEvent,
  setConnectionState,
} from "./live";
import type { Event, EventType } from "./types";

const knownEventTypes: Record<EventType, true> = {
  "issue.created": true,
  "issue.updated": true,
  "issue.closed": true,
  "artifact.created": true,
  "artifact.version": true,
  "ask.opened": true,
  "ask.answered": true,
  "comment.created": true,
  "comment.resolved": true,
  "suggestion.accepted": true,
  "suggestion.rejected": true,
  "message.created": true,
  "child.status": true,
};

// A dead connection reveals no client-visible signal until this much time passes with
// no bytes at all (application events or heartbeat comments): the server sends a `:
// heartbeat` comment every 15s, so 45s is three missed heartbeats.
const WATCHDOG_MS = 45_000;

export interface QueryInvalidator {
  invalidateQueries(filters: { queryKey: readonly unknown[] }): unknown;
}

function artifactId(event: Event): string | undefined {
  return event.type === "artifact.version" ? event.payload.artifact_id : undefined;
}

// 408 (timeout) and 429 (rate limit) are transient — worth retrying. Every other
// 4xx (404, 409, 422, ...) means the request itself can never succeed unchanged, so
// backing off and reconnecting forever would just spin instead of ever recovering.
function isTerminalHttpStatus(status: number): boolean {
  return status >= 400 && status < 500 && status !== 408 && status !== 429;
}

interface EventPages {
  pageParams: unknown[];
  pages: Event[][];
}

export function prependEventToLog(queryClient: QueryClient, event: Event): void {
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

function eventQueryKeys(event: Event): (readonly unknown[])[] {
  const keys: (readonly unknown[])[] = [
    ["issue", event.issue_key],
    ["events", event.issue_key],
    ["issues"],
    // Unread badges and group membership read last_read_seq from here; every inbound
    // event can change what counts as unread, and answering an ask or reading the log
    // updates this out from under any other open tab watching the same user.
    ["user-state"],
    ["inbox"],
  ];

  if (event.type.startsWith("issue.")) {
    return keys;
  }

  if (event.type === "artifact.created" || event.type === "artifact.version") {
    keys.push(["artifacts", event.issue_key]);
    const id = artifactId(event);
    if (id !== undefined) {
      keys.push(["artifact", id]);
    }
    keys.push(["comments", event.issue_key]);
    return keys;
  }

  if (event.type === "ask.opened" || event.type === "ask.answered") {
    keys.push(["asks", event.issue_key]);
    if (typeof event.payload.id === "string") {
      keys.push(["ask", event.payload.id]);
    }
    return keys;
  }

  if (event.type.startsWith("comment.") || event.type.startsWith("suggestion.")) {
    keys.push(["comments", event.issue_key], ["artifact"]);
    return keys;
  }

  if (event.type === "message.created") {
    keys.push(["messages", event.issue_key], ["artifact"]);
    return keys;
  }

  keys.push(["children", event.issue_key]);
  return keys;
}

export function applyEventInvalidations(queryClient: QueryInvalidator, event: Event): void {
  for (const key of eventQueryKeys(event)) {
    queryClient.invalidateQueries({ queryKey: key });
  }
}

function invalidateAfterReconnect(queryClient: QueryInvalidator): void {
  // A gap in the stream (dropped connection, tab asleep, server restart, the replay
  // cap closing the stream) can hide events. Refetch everything a live event would
  // have touched so nothing missed while disconnected stays stale.
  queryClient.invalidateQueries({ queryKey: ["issues"] });
  queryClient.invalidateQueries({ queryKey: ["inbox"] });
  queryClient.invalidateQueries({ queryKey: ["user-state"] });
  queryClient.invalidateQueries({ queryKey: ["issue"] });
  queryClient.invalidateQueries({ queryKey: ["events"] });
}

// Trailing debounce for burst invalidation: a cold-start replay or a flurry of events
// on one issue invalidates each affected key once after 100ms of quiet, not once per
// event.
const INVALIDATION_DEBOUNCE_MS = 100;

// `watchdogMs` overrides the no-chunk watchdog window (default WATCHDOG_MS); the
// only caller that ever sets it is a test proving the watchdog reconnects a
// connection that goes silent without erroring — production code always uses the
// default 45s.
export function useEventStream(watchdogMs: number = WATCHDOG_MS): void {
  const queryClient = useQueryClient();
  const lastId = useRef(0);

  useEffect(() => {
    let cancelled = false;
    let controller: AbortController | undefined;
    let watchdogTimer: number | undefined;
    let reconnectTimer: number | undefined;
    let invalidationTimer: number | undefined;
    let attempt = 0;
    let forceImmediateReconnect = false;
    // True once a terminal (non-auth, non-transient) 4xx has been seen: the
    // connection is done for good until the user reloads, so both a future
    // connect() call and every visibility/online/offline handler must no-op.
    let terminalFailure = false;
    // True once the stream has opened successfully at least once: distinct from
    // `attempt`, which forceReconnect() resets to 0 before every forced retry —
    // using `attempt > 0` here would miss a genuine reconnect whenever online or
    // visibilitychange fires the retry, since that reset always runs first.
    let hasOpenedOnce = false;

    const pendingInvalidations = new Map<string, readonly unknown[]>();

    const clearWatchdog = () => {
      if (watchdogTimer !== undefined) {
        window.clearTimeout(watchdogTimer);
        watchdogTimer = undefined;
      }
    };
    const armWatchdog = () => {
      clearWatchdog();
      watchdogTimer = window.setTimeout(() => controller?.abort(), watchdogMs);
    };

    const flushInvalidations = () => {
      invalidationTimer = undefined;
      const keys = [...pendingInvalidations.values()];
      pendingInvalidations.clear();
      for (const key of keys) {
        queryClient.invalidateQueries({ queryKey: key });
      }
    };
    const scheduleInvalidations = (keys: readonly (readonly unknown[])[]) => {
      for (const key of keys) {
        pendingInvalidations.set(JSON.stringify(key), key);
      }
      if (invalidationTimer !== undefined) {
        window.clearTimeout(invalidationTimer);
      }
      invalidationTimer = window.setTimeout(flushInvalidations, INVALIDATION_DEBOUNCE_MS);
    };

    const receive = (raw: StreamEvent) => {
      if (raw.id !== undefined) {
        const numericId = Number(raw.id);
        // The server now forwards live events regardless of id (a lower id can
        // commit after a higher one is already visible), so an out-of-order
        // delivery must never move the reconnect cursor backward — take the max.
        if (Number.isFinite(numericId) && numericId > lastId.current) {
          lastId.current = numericId;
        }
      }
      if (raw.event === undefined || !(raw.event in knownEventTypes)) {
        return;
      }
      const event = JSON.parse(raw.data) as Event;
      prependEventToLog(queryClient, event);
      scheduleInvalidations(eventQueryKeys(event));
    };

    const connect = () => {
      // The invariant this whole hook relies on: at most one of {an active
      // controller, a pending reconnect timer} exists at any moment. Refusing to
      // start a second stream here — on top of clearing any pending timer below —
      // means a stale timer firing after a forced reconnect already started a new
      // attempt can never open a second, duplicate subscription.
      if (cancelled || controller !== undefined || terminalFailure) {
        return;
      }
      if (reconnectTimer !== undefined) {
        window.clearTimeout(reconnectTimer);
        reconnectTimer = undefined;
      }
      const attemptController = new AbortController();
      controller = attemptController;
      let authFailureStatus: number | undefined;
      let terminalStatus: number | undefined;

      const run = async () => {
        // The very first connection (lastId still at its initial 0) omits since
        // entirely: the server now subscribes to the broker before resolving its
        // own current head, so opening cold here can never race a separate
        // head-lookup request the way a client-computed since= used to. Any
        // later reconnect that already observed a real event id resumes from it.
        const url =
          lastId.current > 0 ? `/api/v1/events?since=${lastId.current}` : "/api/v1/events";
        await readEventStream(url, {
          onChunk: armWatchdog,
          onEvent: receive,
          onOpen: () => {
            if (cancelled) {
              return;
            }
            const wasReconnecting = hasOpenedOnce;
            hasOpenedOnce = true;
            attempt = 0;
            setConnectionState("connected");
            armWatchdog();
            if (wasReconnecting) {
              invalidateAfterReconnect(queryClient);
            }
          },
          signal: attemptController.signal,
        });
      };

      run()
        .catch((error: unknown) => {
          if (error instanceof EventStreamHttpError) {
            if (error.status === 401 || error.status === 403) {
              authFailureStatus = error.status;
            } else if (isTerminalHttpStatus(error.status)) {
              terminalStatus = error.status;
            }
          }
          // Anything else (network error, aborted, 5xx, 408/429, or the server
          // simply closing the stream) reconnects below.
        })
        .finally(() => {
          controller = undefined;
          clearWatchdog();
          if (cancelled) {
            return;
          }
          if (authFailureStatus !== undefined) {
            // The session is gone, not the connection — "Reconnecting" would be
            // misleading, and retrying cannot succeed until the user signs back in.
            setConnectionState("connected");
            queryClient.invalidateQueries({ queryKey: ["whoami"] });
            return;
          }
          if (terminalStatus !== undefined) {
            // The request itself can never succeed unchanged; backing off and
            // retrying forever would just spin instead of ever recovering. This
            // is terminal until the user reloads — detach every handler so a
            // later visibilitychange/online/offline can't reopen the stream.
            terminalFailure = true;
            setConnectionState("unavailable");
            document.removeEventListener("visibilitychange", handleVisibilityChange);
            window.removeEventListener("online", handleOnline);
            window.removeEventListener("offline", handleOffline);
            return;
          }
          if (forceImmediateReconnect) {
            forceImmediateReconnect = false;
            connect();
            return;
          }
          setConnectionState("reconnecting");
          const delay = reconnectDelayMs(attempt);
          attempt += 1;
          reconnectTimer = window.setTimeout(connect, delay);
        });
    };

    // Forces a fresh attempt outside the normal backoff schedule: used when the tab
    // regains visibility or the OS reports the network is back, so the user does not
    // wait out an in-progress backoff delay to recover.
    const forceReconnect = () => {
      if (cancelled || terminalFailure) {
        return;
      }
      attempt = 0;
      if (controller === undefined) {
        if (reconnectTimer !== undefined) {
          window.clearTimeout(reconnectTimer);
          reconnectTimer = undefined;
        }
        connect();
        return;
      }
      forceImmediateReconnect = true;
      controller.abort();
    };
    const handleVisibilityChange = () => {
      if (document.visibilityState === "visible") {
        forceReconnect();
      }
    };
    const handleOnline = () => forceReconnect();
    const handleOffline = () => {
      // The browser's own signal that the network is down: tear down a connection
      // that would otherwise sit open (still receiving heartbeats from a proxy or
      // local buffer) without ever telling the app it lost its route to the server.
      controller?.abort();
    };

    connect();
    document.addEventListener("visibilitychange", handleVisibilityChange);
    window.addEventListener("online", handleOnline);
    window.addEventListener("offline", handleOffline);

    return () => {
      cancelled = true;
      document.removeEventListener("visibilitychange", handleVisibilityChange);
      window.removeEventListener("online", handleOnline);
      window.removeEventListener("offline", handleOffline);
      clearWatchdog();
      if (reconnectTimer !== undefined) {
        window.clearTimeout(reconnectTimer);
      }
      if (invalidationTimer !== undefined) {
        window.clearTimeout(invalidationTimer);
      }
      controller?.abort();
      setConnectionState("connected");
    };
  }, [queryClient, watchdogMs]);
}
