import { useSyncExternalStore } from "react";

export type ConnectionState = "connected" | "reconnecting" | "unavailable";

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
