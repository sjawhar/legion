import { describe, expect, test } from "bun:test";
import { act, renderHook } from "@testing-library/react";

import {
  initialStreamState,
  readEventStream,
  reconnectDelayMs,
  type StreamEvent,
  type StreamState,
  setConnectionState,
  transition,
  useConnectionState,
} from "../api/live";
import type { Event as DispatchEvent } from "../api/types";

function streamResponse(chunks: string[], init: { ok?: boolean; status?: number } = {}): Response {
  const encoder = new TextEncoder();
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      for (const chunk of chunks) {
        controller.enqueue(encoder.encode(chunk));
      }
      controller.close();
    },
  });
  return new Response(body, { status: init.ok === false ? (init.status ?? 500) : 200 });
}

test("readEventStream parses events split across chunk boundaries and skips comments", async () => {
  const originalFetch = globalThis.fetch;
  const received: StreamEvent[] = [];
  const chunks: number[] = [];

  try {
    globalThis.fetch = (async () =>
      streamResponse([
        ": heartbeat\n\n",
        "id: 1\nevent: iss",
        'ue.updated\ndata: {"a":1}\n\n',
        "id: 2\nevent: message.created\ndata: line one\ndata: line two\n\n",
      ])) as unknown as typeof fetch;

    let opened = false;
    await readEventStream("/api/v1/events?since=0", {
      onChunk: () => chunks.push(chunks.length),
      onEvent: (event) => received.push(event),
      onOpen: () => {
        opened = true;
      },
      signal: new AbortController().signal,
    });

    expect(opened).toBe(true);
    expect(chunks.length).toBe(4);
    expect(received).toEqual([
      { data: '{"a":1}', event: "issue.updated", id: "1" },
      { data: "line one\nline two", event: "message.created", id: "2" },
    ]);
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test("readEventStream rejects on a non-OK response before calling onOpen", async () => {
  const originalFetch = globalThis.fetch;

  try {
    globalThis.fetch = (async () =>
      streamResponse([], { ok: false, status: 503 })) as unknown as typeof fetch;

    let opened = false;
    await expect(
      readEventStream("/api/v1/events?since=0", {
        onChunk: () => {},
        onEvent: () => {},
        onOpen: () => {
          opened = true;
        },
        signal: new AbortController().signal,
      })
    ).rejects.toThrow("503");
    expect(opened).toBe(false);
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test("reconnectDelayMs doubles from a 1s base and caps at 30s", () => {
  expect(reconnectDelayMs(0)).toBe(1_000);
  expect(reconnectDelayMs(1)).toBe(2_000);
  expect(reconnectDelayMs(2)).toBe(4_000);
  expect(reconnectDelayMs(3)).toBe(8_000);
  expect(reconnectDelayMs(10)).toBe(30_000);
});

test("readEventStream persists the last event id across frames that omit their own id", async () => {
  const originalFetch = globalThis.fetch;
  const received: StreamEvent[] = [];

  try {
    globalThis.fetch = (async () =>
      streamResponse([
        "id: 5\nevent: issue.updated\ndata: first\n\n",
        // No id: field on this frame — per the SSE spec, the last event id persists
        // until a later frame sets a new one, it is not cleared on every dispatch.
        "event: issue.updated\ndata: second\n\n",
      ])) as unknown as typeof fetch;

    await readEventStream("/api/v1/events?since=0", {
      onChunk: () => {},
      onEvent: (event) => received.push(event),
      onOpen: () => {},
      signal: new AbortController().signal,
    });

    expect(received).toEqual([
      { data: "first", event: "issue.updated", id: "5" },
      { data: "second", event: "issue.updated", id: "5" },
    ]);
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test("readEventStream throws a typed EventStreamHttpError carrying the HTTP status", async () => {
  const originalFetch = globalThis.fetch;

  try {
    globalThis.fetch = (async () =>
      streamResponse([], { ok: false, status: 401 })) as unknown as typeof fetch;

    await expect(
      readEventStream("/api/v1/events?since=0", {
        onChunk: () => {},
        onEvent: () => {},
        onOpen: () => {},
        signal: new AbortController().signal,
      })
    ).rejects.toMatchObject({ name: "EventStreamHttpError", status: 401 });
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test("useConnectionState re-renders only on an actual transition", () => {
  setConnectionState("connected");
  const { result } = renderHook(() => useConnectionState());
  expect(result.current).toBe("connected");

  act(() => setConnectionState("connected"));
  expect(result.current).toBe("connected");

  act(() => setConnectionState("reconnecting"));
  expect(result.current).toBe("reconnecting");

  act(() => setConnectionState("connected"));
  expect(result.current).toBe("connected");
});

function streamState(overrides: Partial<StreamState> = {}): StreamState {
  return { ...initialStreamState, ...overrides };
}

describe("event-stream transition", () => {
  test("starts a connecting stream and registers browser listeners", () => {
    expect(transition(initialStreamState, { kind: "start" })).toEqual({
      effects: [
        { kind: "set-connection-state", state: "connecting" },
        { kind: "register-listeners" },
        { kind: "open-stream", since: 0, streamId: 1 },
      ],
      state: streamState({ active: true, listenersRegistered: true, streamId: 1 }),
    });
  });

  test("opens a reconnecting stream, arms its watchdog, and refreshes missed data", () => {
    expect(
      transition(
        streamState({ active: true, attempt: 2, connection: "reconnecting", hasOpenedOnce: true }),
        { kind: "opened", streamId: 0 }
      )
    ).toEqual({
      effects: [
        { kind: "set-connection-state", state: "connected" },
        { kind: "start-watchdog" },
        {
          kind: "invalidate",
          queryKeys: [
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
          ],
        },
      ],
      state: streamState({ active: true, connection: "connected", hasOpenedOnce: true }),
    });
  });

  test("turns an authentication outcome into signed-out and stops listening", () => {
    expect(
      transition(streamState({ active: true, listenersRegistered: true }), {
        kind: "attempt-finished",
        outcome: { kind: "auth" },
        streamId: 0,
      })
    ).toEqual({
      effects: [
        { kind: "clear-stream" },
        { kind: "clear-timers", timers: ["watchdog"] },
        { kind: "set-connection-state", state: "signed-out" },
        { kind: "invalidate", queryKeys: [["whoami"]] },
        { kind: "remove-listeners" },
      ],
      state: streamState({ connection: "signed-out" }),
    });
  });

  test("turns a terminal HTTP outcome into unavailable without a retry", () => {
    expect(
      transition(streamState({ active: true, listenersRegistered: true }), {
        kind: "attempt-finished",
        outcome: { kind: "terminal", status: 404 },
        streamId: 0,
      })
    ).toEqual({
      effects: [
        { kind: "clear-stream" },
        { kind: "clear-timers", timers: ["watchdog"] },
        { kind: "set-connection-state", state: "unavailable" },
        { kind: "remove-listeners" },
      ],
      state: streamState({ connection: "unavailable" }),
    });
  });

  test("schedules exponential reconnect after closed and transient attempts", () => {
    for (const outcome of [
      { kind: "closed" } as const,
      { error: new Error("offline"), kind: "transient" } as const,
    ]) {
      expect(
        transition(streamState({ active: true, attempt: 1, connection: "connected" }), {
          kind: "attempt-finished",
          outcome,
          streamId: 0,
        })
      ).toEqual({
        effects: [
          { kind: "clear-stream" },
          { kind: "clear-timers", timers: ["watchdog"] },
          { kind: "set-connection-state", state: "reconnecting" },
          { delayMs: 2_000, kind: "schedule-reconnect" },
        ],
        state: streamState({ attempt: 2, connection: "reconnecting" }),
      });
    }
  });

  test("opens the forced replacement as soon as its active stream closes", () => {
    expect(
      transition(
        streamState({
          active: true,
          connection: "connected",
          forceReconnect: true,
          lastEventId: 101,
          streamId: 7,
        }),
        { kind: "attempt-finished", outcome: { kind: "closed" }, streamId: 7 }
      )
    ).toEqual({
      effects: [
        { kind: "clear-stream" },
        { kind: "clear-timers", timers: ["watchdog"] },
        { kind: "open-stream", since: 101, streamId: 8 },
      ],
      state: streamState({ active: true, connection: "connected", lastEventId: 101, streamId: 8 }),
    });
  });

  test("forces an active stream to abort before opening its replacement", () => {
    expect(
      transition(streamState({ active: true, attempt: 3, connection: "connected" }), {
        kind: "force-reconnect",
      })
    ).toEqual({
      effects: [{ kind: "clear-timers", timers: ["reconnect"] }, { kind: "abort-stream" }],
      state: streamState({
        active: true,
        attempt: 0,
        connection: "connected",
        forceReconnect: true,
      }),
    });
  });

  test("opens a forced reconnect immediately when backoff is pending", () => {
    expect(
      transition(streamState({ attempt: 3, connection: "reconnecting", lastEventId: 5 }), {
        kind: "force-reconnect",
      })
    ).toEqual({
      effects: [
        { kind: "clear-timers", timers: ["reconnect"] },
        { kind: "open-stream", since: 5, streamId: 1 },
      ],
      state: streamState({ active: true, connection: "reconnecting", lastEventId: 5, streamId: 1 }),
    });
  });

  test("aborts the active stream when the browser goes offline", () => {
    expect(
      transition(streamState({ active: true, connection: "connected" }), { kind: "offline" })
    ).toEqual({
      effects: [{ kind: "abort-stream" }],
      state: streamState({ active: true, connection: "connected" }),
    });
  });

  test("resets the watchdog on every received stream chunk", () => {
    expect(
      transition(streamState({ active: true, connection: "connected" }), {
        kind: "chunk",
        streamId: 0,
      })
    ).toEqual({
      effects: [{ kind: "start-watchdog" }],
      state: streamState({ active: true, connection: "connected" }),
    });
  });

  test("aborts a silent active stream when its watchdog fires", () => {
    expect(
      transition(streamState({ active: true, connection: "connected" }), {
        kind: "watchdog",
        streamId: 0,
      })
    ).toEqual({
      effects: [{ kind: "abort-stream" }],
      state: streamState({ active: true, connection: "connected" }),
    });
  });

  test("records the greatest stream event id before the next reconnect", () => {
    expect(
      transition(streamState({ active: true, connection: "connected", lastEventId: 41 }), {
        application: undefined,
        id: "42",
        kind: "stream-event",
        streamId: 0,
      })
    ).toEqual({
      effects: [],
      state: streamState({ active: true, connection: "connected", lastEventId: 42 }),
    });
  });

  test("applies a received event and starts one invalidation debounce", () => {
    const event = {
      actor: { id: "alice", kind: "user" },
      created_at: "2026-09-10T00:00:00Z",
      id: 42,
      issue_key: "CORE-1",
      notify: true,
      payload: {},
      seq: 7,
      type: "issue.updated",
    } as DispatchEvent;

    expect(
      transition(streamState({ active: true, connection: "connected" }), {
        application: { event, queryKeys: [["issue", "CORE-1"]] },
        id: "42",
        kind: "stream-event",
        streamId: 0,
      })
    ).toEqual({
      effects: [
        { event, kind: "apply-event", queryKeys: [["issue", "CORE-1"]] },
        { kind: "schedule-invalidations" },
      ],
      state: streamState({
        active: true,
        connection: "connected",
        invalidationScheduled: true,
        lastEventId: 42,
      }),
    });
  });

  test("flushes one pending invalidation debounce", () => {
    expect(
      transition(streamState({ invalidationScheduled: true, lastEventId: 42 }), {
        kind: "flush-invalidations",
      })
    ).toEqual({
      effects: [{ kind: "flush-invalidations" }],
      state: streamState({ lastEventId: 42 }),
    });
  });

  test("stops mid-backoff without aborting a stream that has already closed", () => {
    expect(
      transition(
        streamState({
          attempt: 2,
          connection: "reconnecting",
          invalidationScheduled: true,
          listenersRegistered: true,
        }),
        { kind: "stop" }
      )
    ).toEqual({
      effects: [
        { kind: "remove-listeners" },
        { kind: "clear-timers", timers: ["watchdog", "reconnect", "invalidation"] },
        { kind: "set-connection-state", state: "connected" },
      ],
      state: streamState({ attempt: 2, connection: "connected", stopped: true }),
    });
  });

  test("opens the pending reconnect from the greatest observed event id", () => {
    expect(
      transition(streamState({ connection: "reconnecting", lastEventId: 101 }), {
        kind: "reconnect-timer",
      })
    ).toEqual({
      effects: [{ kind: "open-stream", since: 101, streamId: 1 }],
      state: streamState({
        active: true,
        connection: "reconnecting",
        lastEventId: 101,
        streamId: 1,
      }),
    });
  });
});
