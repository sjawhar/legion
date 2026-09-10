import { type QueryClient, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef } from "react";

import {
  type AttemptOutcome,
  EventStreamHttpError,
  initialStreamState,
  type QueryKey,
  readEventStream,
  type StreamApplicationEvent,
  type StreamEffect,
  type StreamEvent,
  type StreamState,
  type StreamTimer,
  type StreamTransitionEvent,
  setConnectionState,
  transition,
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
  "ask.resolved": true,
  "comment.created": true,
  "comment.resolved": true,
  "comment.reopened": true,
  "comment.edited": true,
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

  if (
    event.type === "ask.opened" ||
    event.type === "ask.answered" ||
    event.type === "ask.resolved"
  ) {
    keys.push(["asks", event.issue_key]);
    if (event.type === "ask.resolved" && typeof event.payload.id === "string") {
      keys.push(["ask-thread", event.payload.id]);
    }
    return keys;
  }

  if (
    event.type === "comment.created" ||
    event.type === "comment.resolved" ||
    event.type === "comment.reopened" ||
    event.type === "comment.edited" ||
    event.type === "suggestion.accepted" ||
    event.type === "suggestion.rejected"
  ) {
    keys.push(["comments", event.issue_key], ["artifact"]);
    if (typeof event.payload.ask_id === "string") {
      keys.push(["ask-thread", event.payload.ask_id]);
    }
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
  const stateRef = useRef<StreamState>(initialStreamState);
  const resourcesRef = useRef<{
    controller: AbortController | undefined;
    invalidationTimer: number | undefined;
    pendingInvalidations: Map<string, QueryKey>;
    reconnectTimer: number | undefined;
    watchdogTimer: number | undefined;
  }>({
    controller: undefined,
    invalidationTimer: undefined,
    pendingInvalidations: new Map(),
    reconnectTimer: undefined,
    watchdogTimer: undefined,
  });

  useEffect(() => {
    const resources = resourcesRef.current;

    const clearTimer = (timer: StreamTimer) => {
      switch (timer) {
        case "watchdog":
          if (resources.watchdogTimer !== undefined) {
            window.clearTimeout(resources.watchdogTimer);
            resources.watchdogTimer = undefined;
          }
          return;
        case "reconnect":
          if (resources.reconnectTimer !== undefined) {
            window.clearTimeout(resources.reconnectTimer);
            resources.reconnectTimer = undefined;
          }
          return;
        case "invalidation":
          if (resources.invalidationTimer !== undefined) {
            window.clearTimeout(resources.invalidationTimer);
            resources.invalidationTimer = undefined;
          }
      }
    };

    const dispatch = (event: StreamTransitionEvent) => {
      const { effects, state } = transition(stateRef.current, event);
      stateRef.current = state;
      for (const effect of effects) {
        runEffect(effect);
      }
    };

    const receive = (streamId: number, raw: StreamEvent) => {
      let application: StreamApplicationEvent | undefined;
      if (raw.event !== undefined && raw.event in knownEventTypes) {
        const event = JSON.parse(raw.data) as Event;
        application = { event, queryKeys: eventQueryKeys(event) };
      }
      dispatch({ application, id: raw.id, kind: "stream-event", streamId });
    };

    const run = async (
      controller: AbortController,
      since: number,
      streamId: number
    ): Promise<AttemptOutcome> => {
      try {
        const url = since > 0 ? `/api/v1/events?since=${since}` : "/api/v1/events";
        await readEventStream(url, {
          onChunk: () => dispatch({ kind: "chunk", streamId }),
          onEvent: (event) => receive(streamId, event),
          onOpen: () => dispatch({ kind: "opened", streamId }),
          signal: controller.signal,
        });
        return { kind: "closed" };
      } catch (error) {
        if (controller.signal.aborted) {
          return { kind: "closed" };
        }
        if (error instanceof EventStreamHttpError) {
          if (error.status === 401 || error.status === 403) {
            return { kind: "auth" };
          }
          if (isTerminalHttpStatus(error.status)) {
            return { kind: "terminal", status: error.status };
          }
        }
        return { kind: "transient", error };
      }
    };

    const runEffect = (effect: StreamEffect) => {
      switch (effect.kind) {
        case "set-connection-state":
          setConnectionState(effect.state);
          return;
        case "open-stream": {
          const controller = new AbortController();
          resources.controller = controller;
          void run(controller, effect.since, effect.streamId).then((outcome) => {
            dispatch({ kind: "attempt-finished", outcome, streamId: effect.streamId });
          });
          return;
        }
        case "clear-stream":
          resources.controller = undefined;
          return;
        case "start-watchdog": {
          clearTimer("watchdog");
          const streamId = stateRef.current.streamId;
          const timer = window.setTimeout(() => {
            if (resources.watchdogTimer !== timer) {
              return;
            }
            resources.watchdogTimer = undefined;
            dispatch({ kind: "watchdog", streamId });
          }, watchdogMs);
          resources.watchdogTimer = timer;
          return;
        }
        case "clear-timers":
          for (const timer of effect.timers) {
            clearTimer(timer);
          }
          if (effect.timers.includes("invalidation")) {
            resources.pendingInvalidations.clear();
          }
          return;
        case "schedule-reconnect": {
          const timer = window.setTimeout(() => {
            if (resources.reconnectTimer !== timer) {
              return;
            }
            resources.reconnectTimer = undefined;
            dispatch({ kind: "reconnect-timer" });
          }, effect.delayMs);
          resources.reconnectTimer = timer;
          return;
        }
        case "apply-event":
          prependEventToLog(queryClient, effect.event);
          for (const key of effect.queryKeys) {
            resources.pendingInvalidations.set(JSON.stringify(key), key);
          }
          return;
        case "schedule-invalidations": {
          const timer = window.setTimeout(() => {
            if (resources.invalidationTimer !== timer) {
              return;
            }
            resources.invalidationTimer = undefined;
            dispatch({ kind: "flush-invalidations" });
          }, INVALIDATION_DEBOUNCE_MS);
          resources.invalidationTimer = timer;
          return;
        }
        case "flush-invalidations":
          for (const key of resources.pendingInvalidations.values()) {
            queryClient.invalidateQueries({ queryKey: key });
          }
          resources.pendingInvalidations.clear();
          return;
        case "invalidate":
          for (const key of effect.queryKeys) {
            queryClient.invalidateQueries({ queryKey: key });
          }
          return;
        case "abort-stream":
          resources.controller?.abort();
          return;
        case "register-listeners":
          document.addEventListener("visibilitychange", handleVisibilityChange);
          window.addEventListener("online", handleOnline);
          window.addEventListener("offline", handleOffline);
          return;
        case "remove-listeners":
          document.removeEventListener("visibilitychange", handleVisibilityChange);
          window.removeEventListener("online", handleOnline);
          window.removeEventListener("offline", handleOffline);
          return;
      }
    };

    const handleVisibilityChange = () => {
      if (document.visibilityState === "visible") {
        dispatch({ kind: "force-reconnect" });
      }
    };
    const handleOnline = () => dispatch({ kind: "force-reconnect" });
    const handleOffline = () => dispatch({ kind: "offline" });

    dispatch({ kind: "start" });
    return () => dispatch({ kind: "stop" });
  }, [queryClient, watchdogMs]);
}
