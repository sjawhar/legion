import { expect, jest, spyOn, test } from "bun:test";
import { onlineManager, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook } from "@testing-library/react";
import type { ReactNode } from "react";

import { keptTab } from "../../__tests__/kept-sends-harness";
import { ApiError, api, NetworkError } from "../../api/client";
import { SEND_DEADLINE_MS, SendStoppedError } from "./keyed-write";
import { useKeyedWrite } from "./useKeyedWrite";
import { writesOf } from "./write-store";

const refusal = new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" });
const verdict = new ApiError(422, { code: "KEY_REUSED", error: "the key names another write" });

/** Moves the fake clock `ms` on in steps, letting each timer's continuation run as it fires. */
async function tick(ms: number): Promise<void> {
  for (let left = ms; left >= 0; left -= 500) {
    await act(async () => {
      jest.advanceTimersByTime(Math.min(500, left));
      for (let turn = 0; turn < 20; turn += 1) await Promise.resolve();
    });
  }
}

/** A surface's write of an answer, as `AskCard`'s form makes it, over one app's queue. */
function renderAnswerWrite() {
  const client = new QueryClient({ defaultOptions: { mutations: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  const hook = renderHook(
    () =>
      useKeyedWrite({
        inline: true,
        label: () => "Answer to “Ship?”",
        write: (input: { selected: string[] }, bounds) =>
          api.answerAsk("ask-1", { expected_edited_at: null, ...input }, bounds),
      }),
    { wrapper }
  );
  return { client, hook };
}

// Ruling 3: a write the reader stopped may have reached Dispatch, so its Retry - the surface making
// the very same press again - goes under its own key, and Dispatch answers it with what the first
// did. After Dispatch's own refusal the Retry is a new press under a new key, since Dispatch would
// answer the old one with that refusal for a day.
test("a surface's Retry keeps the key after Stop trying, and takes a new one after Dispatch refused", async () => {
  jest.useFakeTimers();
  const tab = keptTab();
  const answerAsk = spyOn(api, "answerAsk").mockRejectedValue(refusal);
  const { client, hook } = renderAnswerWrite();
  const input = { selected: ["Ship"] };

  try {
    act(() => hook.result.current.mutate(input));
    await tick(SEND_DEADLINE_MS + 1_000);
    const [late] = writesOf(client).rows();
    expect(late?.status).toBe("late");
    act(() => {
      writesOf(client).stop(late?.id ?? 0);
    });
    await tick(60_000);
    expect(hook.result.current.error).toBeInstanceOf(SendStoppedError);
    const stoppedKey = answerAsk.mock.calls[0]?.[2]?.key;

    // Retry after the stop: the same press, under the same key.
    answerAsk.mockRejectedValueOnce(verdict);
    act(() => hook.result.current.mutate(input));
    await tick(0);
    expect(answerAsk.mock.calls.at(-1)?.[2]?.key).toBe(stoppedKey);
    expect(hook.result.current.error).toBe(verdict);

    // Retry after Dispatch's refusal: a press of its own.
    answerAsk.mockResolvedValueOnce({ id: "ask-1" } as never);
    act(() => hook.result.current.mutate(input));
    await tick(0);
    expect(answerAsk.mock.calls.at(-1)?.[2]?.key).not.toBe(stoppedKey);
  } finally {
    hook.unmount();
    answerAsk.mockRestore();
    tab.close();
    jest.advanceTimersByTime(1);
    jest.useRealTimers();
  }
});

// W27. A write that landed with no answer to give - Dispatch answered its key `409 KEY_COMMITTED` -
// is a success with no data: the surface's `onSuccess` runs with `null`, nothing is rolled back,
// nothing shows as failed, and `mutateAsync` resolves.
test("a write answered KEY_COMMITTED succeeds with null: no error and no rollback", async () => {
  const tab = keptTab();
  const answerAsk = spyOn(api, "answerAsk").mockRejectedValue(
    new ApiError(409, { code: "KEY_COMMITTED", error: "already made" })
  );
  let rolledBack = 0;
  const succeeded: unknown[] = [];
  const client = new QueryClient({ defaultOptions: { mutations: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  const hook = renderHook(
    () =>
      useKeyedWrite({
        inline: true,
        label: () => "Answer to “Ship?”",
        onError: () => {
          rolledBack += 1;
        },
        onSuccess: (data) => succeeded.push(data),
        write: (input: { selected: string[] }, bounds) =>
          api.answerAsk("ask-1", { expected_edited_at: null, ...input }, bounds),
      }),
    { wrapper }
  );

  try {
    let resolved: unknown = "unset";
    await act(async () => {
      resolved = await hook.result.current.mutateAsync({ selected: ["Ship"] });
    });
    expect(resolved).toBeNull();
    expect(answerAsk).toHaveBeenCalledTimes(1);
    expect(hook.result.current.isSuccess).toBe(true);
    expect(hook.result.current.data).toBeNull();
    expect(rolledBack).toBe(0);
    expect(succeeded).toEqual([null]);
    expect(writesOf(client).rows()).toEqual([]);
  } finally {
    hook.unmount();
    answerAsk.mockRestore();
    tab.close();
  }
});

// A write pressed while the browser says it is offline is attempted, not paused: its own retries
// wait out the network, so it goes late at its deadline and can be stopped (`networkMode:
// "always"`).
test("a write pressed offline is attempted and goes late", async () => {
  jest.useFakeTimers();
  const online = onlineManager.isOnline();
  const answerAsk = spyOn(api, "answerAsk").mockRejectedValue(
    new NetworkError("/api/v1/asks/ask-1/answer", new TypeError("Failed to fetch"))
  );
  const { client, hook } = renderAnswerWrite();
  try {
    onlineManager.setOnline(false);
    act(() => hook.result.current.mutate({ selected: ["Ship"] }));
    await tick(0);
    expect(answerAsk).toHaveBeenCalledTimes(1);
    await tick(SEND_DEADLINE_MS + 1_000);
    expect(writesOf(client).rows()[0]?.status).toBe("late");
  } finally {
    onlineManager.setOnline(online);
    hook.unmount();
    answerAsk.mockRestore();
    jest.advanceTimersByTime(1);
    jest.useRealTimers();
  }
});
