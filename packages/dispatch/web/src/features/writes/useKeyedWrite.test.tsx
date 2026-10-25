import { expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook } from "@testing-library/react";
import type { ReactNode } from "react";

import { keptTab } from "../../__tests__/kept-sends-harness";
import { ApiError, api } from "../../api/client";
import { SendStoppedError } from "./keyed-write";
import { pendingWrites } from "./pending-writes";
import { useKeyedWrite } from "./useKeyedWrite";

const refusal = new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" });
const verdict = new ApiError(422, { code: "KEY_REUSED", error: "the key names another write" });

async function tick(ms: number): Promise<void> {
  await act(async () => {
    jest.advanceTimersByTime(ms);
    for (let turn = 0; turn < 20; turn += 1) await Promise.resolve();
  });
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
    for (const wait of [1_000, 2_000, 4_000, 8_000, 16_000]) await tick(wait);
    const [late] = pendingWrites(client).rows();
    expect(late?.status).toBe("late");
    act(() => {
      pendingWrites(client).stop(late?.id ?? 0);
    });
    await tick(30 * 60_000);
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

// A write that landed with no answer to give - Dispatch answered its key `409 KEY_COMMITTED` -
// is no refusal on the surface either: nothing is rolled back, nothing shows as failed, and the
// surface settles as on a landing.
test("a write answered KEY_COMMITTED leaves the surface with no error and no rollback", async () => {
  const tab = keptTab();
  const answerAsk = spyOn(api, "answerAsk").mockRejectedValue(
    new ApiError(409, { code: "KEY_COMMITTED", error: "already made" })
  );
  let rolledBack = 0;
  const settled: unknown[] = [];
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
        onSettled: (_data, error) => settled.push(error),
        write: (input: { selected: string[] }, bounds) =>
          api.answerAsk("ask-1", { expected_edited_at: null, ...input }, bounds),
      }),
    { wrapper }
  );

  try {
    await act(async () => {
      await hook.result.current.mutateAsync({ selected: ["Ship"] }).catch(() => undefined);
    });
    expect(answerAsk).toHaveBeenCalledTimes(1);
    expect(hook.result.current.error).toBeNull();
    expect(hook.result.current.isError).toBe(false);
    expect(rolledBack).toBe(0);
    expect(settled).toEqual([null]);
    expect(pendingWrites(client).rows()).toEqual([]);
  } finally {
    hook.unmount();
    answerAsk.mockRestore();
    tab.close();
  }
});
