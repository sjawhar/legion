import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderHook, waitFor } from "@testing-library/react";
import { createElement, type ReactNode } from "react";

import { useSendMarkOnce } from "./useSendMarkOnce";

interface Props {
  readonly key: string | undefined;
  readonly shouldSend: boolean;
  readonly wake?: number;
}

/** Mounts the hook against `send`, recording every value it sends, in order; a mark's value is its
 *  key. No backoff between a write's retries, so a final failure lands within the test. */
function renderMark(send: (value: string) => Promise<void>, initial: Props) {
  const sent: string[] = [];
  let succeeded = 0;
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retryDelay: 0 }, queries: { retry: false } },
  });
  const hook = renderHook(
    (props: Props) =>
      useSendMarkOnce({
        mark: props.key === undefined ? undefined : { key: props.key, value: props.key },
        onSuccess: () => {
          succeeded += 1;
        },
        send: (value: string) => {
          sent.push(value);
          return send(value);
        },
        shouldSend: props.shouldSend,
        wake: props.wake,
      }),
    {
      initialProps: initial,
      wrapper: ({ children }: { children: ReactNode }) =>
        createElement(QueryClientProvider, { client: queryClient }, children),
    }
  );
  return { hook, sent, succeeded: () => succeeded };
}

test("sends once per key, and only while it should", async () => {
  const { hook, sent, succeeded } = renderMark(async () => {}, { key: "a", shouldSend: false });
  try {
    expect(sent).toEqual([]);
    hook.rerender({ key: "a", shouldSend: true });
    await waitFor(() => expect(succeeded()).toBe(1));
    hook.rerender({ key: "a", shouldSend: true, wake: 1 });
    hook.rerender({ key: "a", shouldSend: false });
    hook.rerender({ key: "a", shouldSend: true });
    expect(sent).toEqual(["a"]);
    hook.rerender({ key: "b", shouldSend: true });
    await waitFor(() => expect(succeeded()).toBe(2));
    expect(sent).toEqual(["a", "b"]);
  } finally {
    hook.unmount();
  }
});

test("retries a failed write twice, with the value the mark was made with", async () => {
  let calls = 0;
  const { hook, sent, succeeded } = renderMark(
    async () => {
      calls += 1;
      if (calls < 3) throw new Error("refused");
    },
    { key: "a", shouldSend: true }
  );
  try {
    await waitFor(() => expect(succeeded()).toBe(1));
    expect(sent).toEqual(["a", "a", "a"]);
  } finally {
    hook.unmount();
  }
});

// A write that still fails after its retries, while its key is the view's latest, forgets the key:
// the next wake sends that key again rather than leaving it unsent.
test("a final failure of the latest key is sent again at the next wake", async () => {
  let fail = true;
  const { hook, sent } = renderMark(
    async () => {
      if (fail) throw new Error("refused");
    },
    { key: "a", shouldSend: true, wake: 1 }
  );
  try {
    await waitFor(() => expect(sent).toEqual(["a", "a", "a"]));
    fail = false;
    hook.rerender({ key: "a", shouldSend: true, wake: 2 });
    await waitFor(() => expect(sent).toEqual(["a", "a", "a", "a"]));
  } finally {
    hook.unmount();
  }
});

// A write that a newer key replaced while it was retrying forgets nothing when it fails: the newer
// write stands for the view.
test("a superseded key that fails for good is not sent again", async () => {
  const first = Promise.withResolvers<void>();
  let firstCalls = 0;
  const { hook, sent } = renderMark(
    (value) => {
      if (value === "a") {
        firstCalls += 1;
        return firstCalls === 1 ? first.promise : Promise.reject(new Error("refused"));
      }
      return Promise.resolve();
    },
    { key: "a", shouldSend: true, wake: 1 }
  );
  try {
    hook.rerender({ key: "b", shouldSend: true, wake: 1 });
    await waitFor(() => expect(sent).toEqual(["a", "b"]));
    first.reject(new Error("refused"));
    await waitFor(() => expect(firstCalls).toBe(3));
    hook.rerender({ key: "b", shouldSend: true, wake: 2 });
    // A later key's send is the signal: any resend of "b" the wake caused goes out before it.
    hook.rerender({ key: "c", shouldSend: true, wake: 2 });
    await waitFor(() => expect(sent.at(-1)).toBe("c"));
    expect(sent.filter((value) => value === "b")).toEqual(["b"]);
  } finally {
    hook.unmount();
  }
});
