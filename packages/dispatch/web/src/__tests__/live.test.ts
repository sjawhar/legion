import { expect, test } from "bun:test";
import { act, renderHook } from "@testing-library/react";

import {
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
