import { expect, jest, spyOn, test } from "bun:test";
import { type MutationKey, QueryClient } from "@tanstack/react-query";
import { waitFor } from "@testing-library/react";

import { ApiError, api, RequestTimeoutError } from "../../api/client";
import type { Comment, Message } from "../../api/types";
import { HeldSends } from "./held-sends";
import { KEYED_SEND_ATTEMPT_MS, type SentRequest } from "./send-request";

const request: SentRequest = {
  anchor: undefined,
  ask: { multiple: false, options: [], urgency: "med" },
  draft: { body: "Held", mentions: [], replacement: "" },
  edit: undefined,
  kind: "comment",
  owner: { issueKey: "CORE-1", kind: "issue" },
  plainDelivery: "steer",
  replyTo: null,
  sendKey: "send-0",
};

const refusal = new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" });

// `under` is what `useHeldSendsUnder` hands React as its snapshot, so the array for a prefix is
// the same one until a send beneath that prefix changes - empty included, where a new array per
// read would re-render its watcher for ever - and a new one as soon as one beneath it does, or the
// watcher shows a stale send.
test("the held sends under a prefix are one array until a send beneath it changes", async () => {
  const cardReply = Promise.withResolvers<Comment>();
  const docked = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment")
    .mockReturnValueOnce(cardReply.promise)
    .mockReturnValueOnce(docked.promise);
  const store = new HeldSends(new QueryClient({ defaultOptions: { mutations: { retry: false } } }));
  const prefix: MutationKey = ["conversation-composer", "CORE-1", "thread-card"];
  const cardKey: MutationKey = [...prefix, "comment-1"];
  const dockedKey: MutationKey = ["conversation-composer", "CORE-2"];

  try {
    const empty = store.under(prefix);
    expect(empty).toEqual([]);
    expect(store.under(prefix)).toBe(empty);

    store.send(cardKey, request);
    const sending = store.under(prefix);
    expect(sending.map((held) => held.status)).toEqual(["sending"]);

    store.send(dockedKey, request);
    expect(store.under(prefix)).toBe(sending);
    docked.reject(refusal);
    await waitFor(() => expect(store.get(dockedKey)?.status).toBe("refused"));
    expect(store.under(prefix)).toBe(sending);

    cardReply.reject(refusal);
    await waitFor(() => expect(store.get(cardKey)?.status).toBe("refused"));
    expect(store.under(prefix).map((held) => held.status)).toEqual(["refused"]);

    expect(store.discard(cardKey)).toBe(true);
    expect(store.under(prefix)).toBe(empty);
    expect(store.discard(cardKey)).toBe(false);
  } finally {
    createComment.mockRestore();
  }
});

/** A message to one session, Send's own: the request the Agents page's composer holds. */
const agentSend: SentRequest = {
  ...request,
  draft: { body: "Status please", mentions: [], replacement: "" },
  owner: { kind: "session", sessionId: "planner-session" },
  sendKey: "send-1",
};

const agentKey: MutationKey = ["agent-send", "planner-session"];

const sentMessage = { body: "Status please", id: "message-1" } as Message;

/** Lets every promise the store chained run as far as it goes without the clock moving. */
async function settle(): Promise<void> {
  for (let turn = 0; turn < 20; turn += 1) await Promise.resolve();
}

/** Runs `body` on the fake clock, which moves a millisecond before the real one comes back: a
 *  timer left at zero delay would otherwise hang a later test's `waitFor`. */
async function onFakeClock(body: () => Promise<void>): Promise<void> {
  jest.useFakeTimers();
  try {
    await body();
  } finally {
    jest.advanceTimersByTime(1);
    jest.useRealTimers();
  }
}

// A send to one session carries its key on every attempt, so Dispatch makes the message once
// however many of them reach it. Anything short of Dispatch's verdict - a fetch that failed, an
// attempt that timed out, a 5xx, a 429 - is tried again after 1 s, 2 s, 4 s … never more than
// 30 s apart, and past the send's deadline the composer shows it still sending, not refused.
test("a failed fetch is retried under the same key until Dispatch answers", async () => {
  const createAgentMessage = spyOn(api, "createAgentMessage")
    .mockRejectedValueOnce(new TypeError("Failed to fetch"))
    .mockRejectedValueOnce(
      new RequestTimeoutError("/api/v1/agents/planner-session/messages", KEYED_SEND_ATTEMPT_MS)
    )
    .mockRejectedValueOnce(refusal)
    .mockRejectedValueOnce(new ApiError(429, { code: "RATE_LIMITED", error: "slow down" }))
    .mockRejectedValueOnce(new ApiError(502, { error: "bad gateway" }))
    .mockRejectedValueOnce(new TypeError("Failed to fetch"))
    .mockRejectedValueOnce(new TypeError("Failed to fetch"))
    .mockResolvedValueOnce(sentMessage);
  const store = new HeldSends(new QueryClient({ defaultOptions: { mutations: { retry: false } } }));

  try {
    await onFakeClock(async () => {
      store.send(agentKey, agentSend);
      await settle();
      expect(createAgentMessage).toHaveBeenCalledTimes(1);

      for (const [index, wait] of [1_000, 2_000, 4_000, 8_000, 16_000, 30_000, 30_000].entries()) {
        jest.advanceTimersByTime(wait - 1);
        await settle();
        expect(createAgentMessage).toHaveBeenCalledTimes(index + 1);
        jest.advanceTimersByTime(1);
        await settle();
        expect(createAgentMessage).toHaveBeenCalledTimes(index + 2);
        // 1 + 2 + 4 + 8 + 16 s is past the send's 30 s deadline: still the server's, not refused.
        if (index === 4) expect(store.get(agentKey)?.status).toBe("late");
      }
      expect(store.get(agentKey)).toBeUndefined();
    });

    expect(createAgentMessage.mock.calls).toEqual(
      Array.from({ length: 8 }, () => [
        "planner-session",
        { body: "Status please", delivery: "steer", idempotency_key: "send-1" },
        { deadlineMs: KEYED_SEND_ATTEMPT_MS },
      ])
    );
  } finally {
    createAgentMessage.mockRestore();
  }
});

// Any other 4xx is Dispatch's verdict on the request: the same key and the same message would meet
// it again, so the send stops trying and the composer hands the draft back with the refusal.
test("a 409 or 400 stops the retries and refuses", async () => {
  const refusals = [
    new ApiError(409, { code: "MESSAGE_KEY_REUSED", error: "the key names another message" }),
    new ApiError(400, { code: "MESSAGE_INPUT", error: "delivery must be steer, aside or btw" }),
  ];
  const createAgentMessage = spyOn(api, "createAgentMessage");
  const store = new HeldSends(new QueryClient({ defaultOptions: { mutations: { retry: false } } }));

  try {
    await onFakeClock(async () => {
      for (const verdict of refusals) {
        createAgentMessage
          .mockClear()
          .mockRejectedValueOnce(new TypeError("Failed to fetch"))
          .mockRejectedValueOnce(verdict);
        store.send(agentKey, agentSend);
        await settle();
        jest.advanceTimersByTime(1_000);
        await settle();

        expect(store.get(agentKey)).toMatchObject({ refusal: verdict, status: "refused" });
        jest.advanceTimersByTime(10 * 60_000);
        await settle();
        expect(createAgentMessage).toHaveBeenCalledTimes(2);
        expect(store.discard(agentKey)).toBe(true);
      }
    });
  } finally {
    createAgentMessage.mockRestore();
  }
});
