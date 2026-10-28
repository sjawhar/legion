import { expect, test } from "bun:test";
import { act, renderHook } from "@testing-library/react";

import {
  RECONNECT_SPREAD_MS,
  readEventStream,
  reconnectDelayMs,
  type StreamEvent,
  setConnectionState,
  useConnectionState,
} from "../api/live";

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

test("the first reconnect is spread over [0, 2 s); later ones back off within [d/2, d), capped at 30 s", () => {
  expect(reconnectDelayMs(0, () => 0)).toBe(0);
  expect(reconnectDelayMs(0, () => 0.5)).toBe(1_000);
  expect(reconnectDelayMs(0, () => 0.999)).toBeCloseTo(1_998);
  // d = min(1 s × 2^attempt, 30 s): the lowest draw waits d/2, the highest just under d.
  for (const [attempt, d] of [
    [1, 2_000],
    [2, 4_000],
    [3, 8_000],
    [4, 16_000],
    [5, 30_000],
    [10, 30_000],
  ] as const) {
    expect(reconnectDelayMs(attempt, () => 0)).toBe(d / 2);
    expect(reconnectDelayMs(attempt, () => 0.999)).toBeCloseTo(d / 2 + 0.999 * (d / 2));
    expect(reconnectDelayMs(attempt, () => 0.999)).toBeLessThan(d);
  }
});

test("first reconnects drawn from a seeded source spread evenly over the 2 s window", () => {
  // mulberry32: a small seeded generator, so the draw is the same on every run.
  let state = 0x5eed;
  const seeded = () => {
    state = (state + 0x6d2b79f5) | 0;
    let t = Math.imul(state ^ (state >>> 15), 1 | state);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4_294_967_296;
  };
  const buckets = new Array<number>(RECONNECT_SPREAD_MS / 100).fill(0);
  for (let draw = 0; draw < 1_000; draw += 1) {
    const delay = reconnectDelayMs(0, seeded);
    expect(delay).toBeGreaterThanOrEqual(0);
    expect(delay).toBeLessThan(RECONNECT_SPREAD_MS);
    const bucket = Math.floor(delay / 100);
    buckets[bucket] = (buckets[bucket] ?? 0) + 1;
  }
  // A uniform spread puts about 50 of the 1,000 in each 100 ms bucket; a fixed delay, or one
  // bunched near any moment, puts far more than 100 in one.
  expect(Math.max(...buckets)).toBeLessThanOrEqual(100);
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
