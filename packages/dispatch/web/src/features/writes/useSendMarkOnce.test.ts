import { expect, jest, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook } from "@testing-library/react";
import { createElement, type ReactNode } from "react";

import { ApiError, NetworkError, type WriteBounds } from "../../api/client";
import { useSendMarkOnce } from "./useSendMarkOnce";
import { writesOf } from "./write-store";

interface Props {
  readonly key: string | undefined;
  readonly shouldSend: boolean;
  readonly wake?: number;
}

const refused = new ApiError(400, { code: "MARK_INPUT", error: "refused" });

/** Mounts the hook against `send`, recording every value it sends and the key each went under;
 *  a mark's value is its key. */
function renderMark(send: (value: string) => Promise<void>, initial: Props) {
  const sent: string[] = [];
  const keys: string[] = [];
  let succeeded = 0;
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const hook = renderHook(
    (props: Props) =>
      useSendMarkOnce({
        mark: props.key === undefined ? undefined : { key: props.key, value: props.key },
        onSuccess: () => {
          succeeded += 1;
        },
        send: (value: string, bounds: WriteBounds) => {
          // As every client write does, the request is told to `sent` before it goes out: a mark
          // waiting its turn is planned there, and nothing past it runs (`Writes`).
          bounds.sent?.({ body: { value }, method: "PUT", path: "/api/v1/me/threads/t/read" });
          sent.push(value);
          keys.push(bounds.key);
          return send(value);
        },
        shouldSend: props.shouldSend,
        target: "read:test",
        wake: props.wake,
      }),
    {
      initialProps: initial,
      wrapper: ({ children }: { children: ReactNode }) =>
        createElement(QueryClientProvider, { client }, children),
    }
  );
  return { client, hook, keys, sent, succeeded: () => succeeded };
}

async function settle(): Promise<void> {
  await act(async () => {
    for (let turn = 0; turn < 30; turn += 1) await Promise.resolve();
  });
}

async function tick(ms: number): Promise<void> {
  for (let left = ms; left > 0; left -= 500) {
    jest.advanceTimersByTime(Math.min(500, left));
    await settle();
  }
}

async function onFakeClock(body: () => Promise<void>): Promise<void> {
  jest.useFakeTimers();
  try {
    await body();
  } finally {
    jest.advanceTimersByTime(1);
    jest.useRealTimers();
  }
}

test("sends once per key, and only while it should", async () => {
  const { hook, sent, succeeded } = renderMark(async () => {}, { key: "a", shouldSend: false });
  try {
    expect(sent).toEqual([]);
    hook.rerender({ key: "a", shouldSend: true });
    await settle();
    expect(succeeded()).toBe(1);
    hook.rerender({ key: "a", shouldSend: true, wake: 1 });
    hook.rerender({ key: "a", shouldSend: false });
    hook.rerender({ key: "a", shouldSend: true });
    expect(sent).toEqual(["a"]);
    hook.rerender({ key: "b", shouldSend: true });
    await settle();
    expect(succeeded()).toBe(2);
    expect(sent).toEqual(["a", "b"]);
  } finally {
    hook.unmount();
  }
});

// A read mark is a write of the one store, quiet: a failure short of Dispatch's verdict is tried
// again under its key, with the value the mark was made with, and the reader is never shown it.
test("a mark whose attempts fail short of a verdict is tried again under its key", async () => {
  await onFakeClock(async () => {
    let calls = 0;
    const { client, hook, keys, sent, succeeded } = renderMark(
      async () => {
        calls += 1;
        if (calls < 3) throw new NetworkError("/api/v1/me/threads/t/read", new TypeError("x"));
      },
      { key: "a", shouldSend: true }
    );
    try {
      await settle();
      expect(
        writesOf(client)
          .rows()
          .map((write) => write.presentation)
      ).toEqual(["quiet"]);
      await tick(4_000);
      expect(succeeded()).toBe(1);
      expect(sent).toEqual(["a", "a", "a"]);
      expect(new Set(keys).size).toBe(1);
    } finally {
      hook.unmount();
    }
  });
});

// A mark Dispatch refuses, while its key is the view's latest, forgets the key: the next wake
// sends that key again rather than leaving it unsent. Nothing of the refusal is shown.
test("a refused mark of the latest key is sent again at the next wake", async () => {
  let fail = true;
  const { client, hook, sent } = renderMark(
    async () => {
      if (fail) throw refused;
    },
    { key: "a", shouldSend: true, wake: 1 }
  );
  try {
    await settle();
    expect(sent).toEqual(["a"]);
    expect(writesOf(client).rows()).toEqual([]);
    fail = false;
    hook.rerender({ key: "a", shouldSend: true, wake: 2 });
    await settle();
    expect(sent).toEqual(["a", "a"]);
  } finally {
    hook.unmount();
  }
});

// The marks of one view go out in order, so an older one never lands after a newer one. One a
// newer key replaced forgets nothing when it is refused: the newer one stands for the view.
test("a superseded mark goes first, and its refusal sends nothing again", async () => {
  const first = Promise.withResolvers<void>();
  const { hook, sent } = renderMark(
    (value) => (value === "a" ? first.promise : Promise.resolve()),
    { key: "a", shouldSend: true, wake: 1 }
  );
  try {
    await settle();
    hook.rerender({ key: "b", shouldSend: true, wake: 1 });
    await settle();
    expect(sent).toEqual(["a"]);
    first.reject(refused);
    await settle();
    expect(sent).toEqual(["a", "b"]);
    hook.rerender({ key: "b", shouldSend: true, wake: 2 });
    hook.rerender({ key: "c", shouldSend: true, wake: 2 });
    await settle();
    expect(sent).toEqual(["a", "b", "c"]);
  } finally {
    hook.unmount();
  }
});
