import { useSyncExternalStore } from "react";
import type { Event } from "./types";

export type ConnectionState =
  | "connecting"
  | "connected"
  | "reconnecting"
  | "unavailable"
  | "signed-out";

let connectionState: ConnectionState = "connected";
const listeners = new Set<() => void>();

export function getConnectionState(): ConnectionState {
  return connectionState;
}

export function setConnectionState(next: ConnectionState): void {
  if (connectionState === next) {
    return;
  }
  connectionState = next;
  for (const listener of listeners) {
    listener();
  }
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

/** Reads the live event-stream connection state from the shared store. */
export function useConnectionState(): ConnectionState {
  return useSyncExternalStore(subscribe, getConnectionState);
}

const BASE_RECONNECT_DELAY_MS = 1_000;
const MAX_RECONNECT_DELAY_MS = 30_000;

/** Exponential backoff for stream reconnect attempts, capped at 30s. */
export function reconnectDelayMs(attempt: number): number {
  return Math.min(BASE_RECONNECT_DELAY_MS * 2 ** attempt, MAX_RECONNECT_DELAY_MS);
}

export type QueryKey = readonly unknown[];

export interface StreamState {
  active: boolean;
  attempt: number;
  connection: ConnectionState;
  forceReconnect: boolean;
  hasOpenedOnce: boolean;
  invalidationScheduled: boolean;
  lastEventId: number;
  listenersRegistered: boolean;
  stopped: boolean;
  streamId: number;
}

export type StreamTimer = "watchdog" | "reconnect" | "invalidation";

export type AttemptOutcome =
  | { kind: "auth" }
  | { kind: "terminal"; status: number }
  | { kind: "transient"; error: unknown }
  | { kind: "closed" };

export type StreamEffect =
  | { kind: "set-connection-state"; state: ConnectionState }
  | { kind: "open-stream"; since: number; streamId: number }
  | { kind: "clear-stream" }
  | { kind: "start-watchdog" }
  | { kind: "clear-timers"; timers: readonly StreamTimer[] }
  | { kind: "schedule-reconnect"; delayMs: number }
  | { kind: "schedule-invalidations" }
  | { kind: "flush-invalidations" }
  | { kind: "apply-event"; event: Event; queryKeys: readonly QueryKey[] }
  | { kind: "invalidate"; queryKeys: readonly QueryKey[] }
  | { kind: "abort-stream" }
  | { kind: "register-listeners" }
  | { kind: "remove-listeners" };

export type StreamTransitionEvent =
  | { kind: "start" }
  | { kind: "opened"; streamId: number }
  | { kind: "chunk"; streamId: number }
  | { kind: "watchdog"; streamId: number }
  | { kind: "reconnect-timer" }
  | {
      kind: "stream-event";
      id: string | undefined;
      application: StreamApplicationEvent | undefined;
      streamId: number;
    }
  | { kind: "flush-invalidations" }
  | { kind: "force-reconnect" }
  | { kind: "offline" }
  | { kind: "attempt-finished"; outcome: AttemptOutcome; streamId: number }
  | { kind: "stop" };

export interface StreamApplicationEvent {
  event: Event;
  queryKeys: readonly QueryKey[];
}

export interface StreamTransition {
  effects: StreamEffect[];
  state: StreamState;
}

export const initialStreamState: StreamState = {
  active: false,
  attempt: 0,
  connection: "connecting",
  forceReconnect: false,
  hasOpenedOnce: false,
  invalidationScheduled: false,
  lastEventId: 0,
  listenersRegistered: false,
  stopped: false,
  streamId: 0,
};

const reconnectInvalidationKeys: readonly QueryKey[] = [
  ["issues"],
  ["inbox"],
  ["user-state"],
  ["issue"],
  ["events"],
  ["projects"],
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
];

function isCurrentStream(state: StreamState, streamId: number): boolean {
  return !state.stopped && state.active && state.streamId === streamId;
}

/** Pure state machine for one event-stream connection attempt. */
export function transition(state: StreamState, event: StreamTransitionEvent): StreamTransition {
  switch (event.kind) {
    case "start": {
      if (state.active || state.listenersRegistered) {
        return { effects: [], state };
      }
      const previous = state.stopped ? { ...initialStreamState, streamId: state.streamId } : state;
      const streamId = previous.streamId + 1;
      return {
        effects: [
          { kind: "set-connection-state", state: "connecting" },
          { kind: "register-listeners" },
          { kind: "open-stream", since: previous.lastEventId, streamId },
        ],
        state: {
          ...previous,
          active: true,
          connection: "connecting",
          listenersRegistered: true,
          streamId,
        },
      };
    }
    case "opened":
      if (!isCurrentStream(state, event.streamId)) {
        return { effects: [], state };
      }
      return {
        effects: [
          { kind: "set-connection-state", state: "connected" },
          { kind: "start-watchdog" },
          ...(state.hasOpenedOnce
            ? [{ kind: "invalidate", queryKeys: reconnectInvalidationKeys } as const]
            : []),
        ],
        state: { ...state, attempt: 0, connection: "connected", hasOpenedOnce: true },
      };
    case "chunk":
      if (!isCurrentStream(state, event.streamId)) {
        return { effects: [], state };
      }
      return { effects: [{ kind: "start-watchdog" }], state };
    case "watchdog":
      if (!isCurrentStream(state, event.streamId)) {
        return { effects: [], state };
      }
      return { effects: [{ kind: "abort-stream" }], state };
    case "stream-event": {
      if (!isCurrentStream(state, event.streamId)) {
        return { effects: [], state };
      }
      const numericId = Number(event.id);
      const lastEventId =
        Number.isFinite(numericId) && numericId > state.lastEventId ? numericId : state.lastEventId;
      if (event.application === undefined) {
        return { effects: [], state: { ...state, lastEventId } };
      }
      return {
        effects: [
          {
            event: event.application.event,
            kind: "apply-event",
            queryKeys: event.application.queryKeys,
          },
          ...(state.invalidationScheduled ? [] : [{ kind: "schedule-invalidations" } as const]),
        ],
        state: { ...state, invalidationScheduled: true, lastEventId },
      };
    }
    case "flush-invalidations":
      if (state.stopped || !state.invalidationScheduled) {
        return { effects: [], state };
      }
      return {
        effects: [{ kind: "flush-invalidations" }],
        state: { ...state, invalidationScheduled: false },
      };
    case "force-reconnect": {
      if (
        state.stopped ||
        state.connection === "signed-out" ||
        state.connection === "unavailable"
      ) {
        return { effects: [], state };
      }
      if (state.active) {
        return {
          effects: [{ kind: "clear-timers", timers: ["reconnect"] }, { kind: "abort-stream" }],
          state: { ...state, attempt: 0, forceReconnect: true },
        };
      }
      const streamId = state.streamId + 1;
      return {
        effects: [
          { kind: "clear-timers", timers: ["reconnect"] },
          { kind: "open-stream", since: state.lastEventId, streamId },
        ],
        state: { ...state, active: true, attempt: 0, streamId },
      };
    }
    case "offline":
      if (state.stopped || !state.active) {
        return { effects: [], state };
      }
      return { effects: [{ kind: "abort-stream" }], state };
    case "attempt-finished":
      if (!isCurrentStream(state, event.streamId)) {
        return { effects: [], state };
      }
      if (event.outcome.kind === "auth") {
        return {
          effects: [
            { kind: "clear-stream" },
            { kind: "clear-timers", timers: ["watchdog"] },
            { kind: "set-connection-state", state: "signed-out" },
            { kind: "invalidate", queryKeys: [["whoami"]] },
            { kind: "remove-listeners" },
          ],
          state: {
            ...state,
            active: false,
            connection: "signed-out",
            forceReconnect: false,
            listenersRegistered: false,
          },
        };
      }
      if (event.outcome.kind === "terminal") {
        return {
          effects: [
            { kind: "clear-stream" },
            { kind: "clear-timers", timers: ["watchdog"] },
            { kind: "set-connection-state", state: "unavailable" },
            { kind: "remove-listeners" },
          ],
          state: {
            ...state,
            active: false,
            connection: "unavailable",
            forceReconnect: false,
            listenersRegistered: false,
          },
        };
      }
      if (state.forceReconnect) {
        const streamId = state.streamId + 1;
        return {
          effects: [
            { kind: "clear-stream" },
            { kind: "clear-timers", timers: ["watchdog"] },
            { kind: "open-stream", since: state.lastEventId, streamId },
          ],
          state: { ...state, forceReconnect: false, streamId },
        };
      }
      return {
        effects: [
          { kind: "clear-stream" },
          { kind: "clear-timers", timers: ["watchdog"] },
          { kind: "set-connection-state", state: "reconnecting" },
          { delayMs: reconnectDelayMs(state.attempt), kind: "schedule-reconnect" },
        ],
        state: {
          ...state,
          active: false,
          attempt: state.attempt + 1,
          connection: "reconnecting",
          forceReconnect: false,
        },
      };
    case "reconnect-timer": {
      if (state.stopped || state.active || state.connection !== "reconnecting") {
        return { effects: [], state };
      }
      const streamId = state.streamId + 1;
      return {
        effects: [{ kind: "open-stream", since: state.lastEventId, streamId }],
        state: { ...state, active: true, streamId },
      };
    }
    case "stop":
      if (state.stopped) {
        return { effects: [], state };
      }
      return {
        effects: [
          ...(state.listenersRegistered ? [{ kind: "remove-listeners" } as const] : []),
          { kind: "clear-timers", timers: ["watchdog", "reconnect", "invalidation"] },
          ...(state.active
            ? [{ kind: "abort-stream" } as const, { kind: "clear-stream" } as const]
            : []),
          { kind: "set-connection-state", state: "connected" },
        ],
        state: {
          ...state,
          active: false,
          connection: "connected",
          forceReconnect: false,
          invalidationScheduled: false,
          listenersRegistered: false,
          stopped: true,
        },
      };
  }
}

/**
 * Thrown by `readEventStream` when the connection request itself fails (as opposed to
 * a mid-stream network drop, which resolves the promise normally via `done`). Callers
 * distinguish `status` 401/403 (the session is gone — stop retrying, prompt sign-in)
 * from everything else (transient — keep backing off and retrying).
 */
export class EventStreamHttpError extends Error {
  constructor(public readonly status: number) {
    super(`event stream request failed: ${status}`);
    this.name = "EventStreamHttpError";
  }
}

export interface StreamEvent {
  id: string | undefined;
  event: string | undefined;
  data: string;
}

/**
 * Incremental parser for the `text/event-stream` wire format. Unlike the native
 * EventSource, callers see every line as it arrives — including `:`-prefixed comment
 * lines — so a caller can treat any received byte (not just a named event) as proof
 * the connection is alive.
 */
class ServerSentEventParser {
  private buffer = "";
  private id: string | undefined;
  private eventName: string | undefined;
  private dataLines: string[] = [];

  push(chunk: string, onEvent: (event: StreamEvent) => void): void {
    this.buffer += chunk;
    let newlineIndex = this.buffer.indexOf("\n");
    while (newlineIndex !== -1) {
      const line = this.buffer.slice(0, newlineIndex).replace(/\r$/, "");
      this.buffer = this.buffer.slice(newlineIndex + 1);
      this.consumeLine(line, onEvent);
      newlineIndex = this.buffer.indexOf("\n");
    }
  }

  private consumeLine(line: string, onEvent: (event: StreamEvent) => void): void {
    if (line === "") {
      if (this.dataLines.length > 0) {
        onEvent({ data: this.dataLines.join("\n"), event: this.eventName, id: this.id });
      }
      // Per WHATWG SSE semantics, the last event id persists across dispatches —
      // only the event-type and data buffers reset here.
      this.eventName = undefined;
      this.dataLines = [];
      return;
    }
    if (line.startsWith(":")) {
      return;
    }
    const colonIndex = line.indexOf(":");
    const field = colonIndex === -1 ? line : line.slice(0, colonIndex);
    const rawValue = colonIndex === -1 ? "" : line.slice(colonIndex + 1);
    const value = rawValue.startsWith(" ") ? rawValue.slice(1) : rawValue;
    if (field === "id") {
      this.id = value;
    } else if (field === "event") {
      this.eventName = value;
    } else if (field === "data") {
      this.dataLines.push(value);
    }
  }
}

export interface StreamHandlers {
  signal: AbortSignal;
  onOpen: () => void;
  /**
   * Called for every chunk received over the wire, including heartbeat-only comment
   * frames. This is the only client-visible signal that a quiet connection (no
   * application events) is still alive, since heartbeat comments never reach a native
   * EventSource's message listeners.
   */
  onChunk: () => void;
  onEvent: (event: StreamEvent) => void;
}

/**
 * Reads a `text/event-stream` response until the server closes it, `signal` aborts, or
 * the request fails. Built on `fetch` + a manual reader (rather than `EventSource`) so
 * callers can observe heartbeat traffic and drive their own reconnect/backoff policy —
 * `EventSource`'s automatic retry always resumes from `since=0` unless the browser
 * itself performed the retry, which this app does not want to depend on.
 */
export async function readEventStream(url: string, handlers: StreamHandlers): Promise<void> {
  const response = await fetch(url, { credentials: "same-origin", signal: handlers.signal });
  if (!response.ok || response.body === null) {
    throw new EventStreamHttpError(response.status);
  }
  handlers.onOpen();
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  const parser = new ServerSentEventParser();
  while (true) {
    const { done, value } = await reader.read();
    if (done) {
      return;
    }
    handlers.onChunk();
    parser.push(decoder.decode(value, { stream: true }), handlers.onEvent);
  }
}
