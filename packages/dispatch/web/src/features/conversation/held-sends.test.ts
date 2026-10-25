import { expect, jest, spyOn, test } from "bun:test";
import { type MutationKey, QueryClient } from "@tanstack/react-query";
import { waitFor } from "@testing-library/react";

import { type KeptTab, keptTab, UnloadTarget } from "../../__tests__/kept-sends-harness";
import { ApiError, api, RequestTimeoutError } from "../../api/client";
import type { Comment, Message, MessageDelivery } from "../../api/types";
import { HeldSends, stoppable } from "./held-sends";
import { KEYED_SEND_ATTEMPT_MS, SendStoppedError, type SentRequest } from "./send-request";

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

/** Attempt `number` of the planner's message, in `state`. */
function plannerAttempt(number: number, state: "failed" | "pending" | "sent"): MessageDelivery {
  return {
    attempt: number,
    created_at: "2026-10-25T00:00:00Z",
    delivery: "steer",
    envelope_id: null,
    error: null,
    message_id: "message-1",
    reply_id: null,
    session_id: "planner-session",
    state,
  };
}

/** Dispatch's answer to the planner's message, sent. */
const sentMessage = {
  body: "Status please",
  deliveries: [plannerAttempt(1, "sent")],
  id: "message-1",
} as Message;

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

// Dispatch answers a repeat of a keyed send whose message's one attempt is still pending with
// that attempt pending: another request is sending it, or a sender that died - a crashed process
// - still holds its claim, and Dispatch resumes the attempt for the first repeat after the claim
// lapses (a minute). Taken as landed, that answer would leave a message stored and never on its
// way, so the send keeps asking under its key until the attempt settles. A message with more than
// one attempt was retried from its card, which Dispatch never resumes, so its answer lands as is.
test("a 2xx whose one attempt is still pending is asked again under the key until it settles", async () => {
  const answer = (...deliveries: MessageDelivery[]) => ({ ...sentMessage, deliveries });
  const createAgentMessage = spyOn(api, "createAgentMessage")
    .mockRejectedValueOnce(new TypeError("Failed to fetch"))
    .mockResolvedValueOnce(answer(plannerAttempt(1, "pending")))
    .mockResolvedValueOnce(answer(plannerAttempt(1, "pending")))
    .mockResolvedValueOnce(answer(plannerAttempt(1, "sent")))
    .mockResolvedValueOnce(answer(plannerAttempt(1, "failed"), plannerAttempt(2, "pending")));
  const store = new HeldSends(new QueryClient({ defaultOptions: { mutations: { retry: false } } }));

  try {
    await onFakeClock(async () => {
      store.send(agentKey, agentSend);
      await settle();
      for (const [index, wait] of [1_000, 2_000, 4_000].entries()) {
        expect(store.get(agentKey)?.status).toBe("sending");
        jest.advanceTimersByTime(wait);
        await settle();
        expect(createAgentMessage).toHaveBeenCalledTimes(index + 2);
      }
      expect(store.get(agentKey)).toBeUndefined();

      store.send(agentKey, { ...agentSend, sendKey: "send-2" });
      await settle();
      expect(store.get(agentKey)).toBeUndefined();
      jest.advanceTimersByTime(30 * 60_000);
      await settle();
      expect(createAgentMessage).toHaveBeenCalledTimes(5);
    });
    expect(createAgentMessage.mock.calls.map(([, input]) => input.idempotency_key)).toEqual([
      "send-1",
      "send-1",
      "send-1",
      "send-1",
      "send-2",
    ]);
  } finally {
    createAgentMessage.mockRestore();
  }
});

/** Fails the send out under `agentKey` through 1 + 2 + 4 + 8 s of attempts and the 15 s after
 *  them, to the send's 30 s deadline: five attempts made, the sixth 16 s away. */
async function failUntilLate(): Promise<void> {
  for (const wait of [1_000, 2_000, 4_000, 8_000, 15_000]) {
    jest.advanceTimersByTime(wait);
    await settle();
  }
}

// A lasting outage would hold a late send to one session for good. Stop trying ends it: no attempt
// follows, the send is handed back refused with what a Retry does, and a Retry goes under the same
// key, so Dispatch makes the message at most once. Until it is late there is nothing to stop.
test("a late send to one session can be stopped, and Retry goes under its key", async () => {
  const createAgentMessage = spyOn(api, "createAgentMessage").mockRejectedValue(refusal);
  const store = new HeldSends(new QueryClient({ defaultOptions: { mutations: { retry: false } } }));

  try {
    await onFakeClock(async () => {
      store.send(agentKey, agentSend);
      await settle();
      expect(stoppable(store.get(agentKey))).toBe(false);
      expect(store.stop(agentKey)).toBe(false);

      await failUntilLate();
      expect(store.get(agentKey)?.status).toBe("late");
      expect(stoppable(store.get(agentKey))).toBe(true);
      expect(createAgentMessage).toHaveBeenCalledTimes(5);

      expect(store.stop(agentKey)).toBe(true);
      await settle();
      const stopped = store.get(agentKey);
      expect(stopped?.status).toBe("refused");
      expect(stopped?.refusal).toBeInstanceOf(SendStoppedError);
      expect(stopped?.refusal?.message).toBe(new SendStoppedError().message);
      jest.advanceTimersByTime(30 * 60_000);
      await settle();
      expect(createAgentMessage).toHaveBeenCalledTimes(5);

      createAgentMessage.mockResolvedValueOnce(sentMessage);
      expect(store.send(agentKey, stopped?.request ?? agentSend)).toBe(true);
      await settle();
      expect(store.get(agentKey)).toBeUndefined();
    });
    expect(createAgentMessage.mock.calls.map(([, input]) => input.idempotency_key)).toEqual(
      Array.from({ length: 6 }, () => "send-1")
    );
  } finally {
    createAgentMessage.mockRestore();
  }
});

// Stopping ends the trying between attempts, never the attempt in flight: that attempt may already
// have reached Dispatch, so its answer is still the send's - a 2xx lands, a 5xx ends it stopped -
// and no attempt is made after it.
test("stopping lets the attempt in flight answer, and makes none after it", async () => {
  const createAgentMessage = spyOn(api, "createAgentMessage");

  try {
    for (const answer of ["lands", "fails"] as const) {
      createAgentMessage.mockReset().mockRejectedValue(refusal);
      const inFlight = Promise.withResolvers<Message>();
      const store = new HeldSends(
        new QueryClient({ defaultOptions: { mutations: { retry: false } } })
      );
      await onFakeClock(async () => {
        store.send(agentKey, agentSend);
        await settle();
        await failUntilLate();
        createAgentMessage.mockReturnValueOnce(inFlight.promise);
        // The sixth attempt goes out 16 s on, and the reader stops while it is out.
        jest.advanceTimersByTime(16_000);
        await settle();
        expect(createAgentMessage).toHaveBeenCalledTimes(6);
        expect(createAgentMessage.mock.calls[5]?.[2]).toEqual({
          deadlineMs: KEYED_SEND_ATTEMPT_MS,
        });
        expect(store.stop(agentKey)).toBe(true);
        await settle();
        expect(store.get(agentKey)).toMatchObject({ status: "late", stopping: true });
        expect(store.stop(agentKey)).toBe(false);

        if (answer === "lands") inFlight.resolve(sentMessage);
        else inFlight.reject(refusal);
        await settle();
        if (answer === "lands") expect(store.get(agentKey)).toBeUndefined();
        else expect(store.get(agentKey)?.refusal).toBeInstanceOf(SendStoppedError);
        jest.advanceTimersByTime(30 * 60_000);
        await settle();
        expect(createAgentMessage).toHaveBeenCalledTimes(6);
      });
    }
  } finally {
    createAgentMessage.mockRestore();
  }
});

/** The kept record of the send under `key` in `tab`, as far as its state and refusal go. */
function keptState(tab: KeptTab, key: string): unknown {
  const kept = tab.kept(key) as { refusal: unknown; state: string } | undefined;
  return kept === undefined ? undefined : { refusal: kept.refusal, state: kept.state };
}

// W1. A send to one session goes under its key, so a reload that interrupts it loses nothing: the
// page after it makes the send again under the same key - Dispatch answers that repeat with the
// message the first request made, if it made one - and holds it as the composer left it. Once it
// lands, nothing of it is kept.
test("a send in flight survives a reload and is tried again under the same key", async () => {
  const tab = keptTab();
  const createAgentMessage = spyOn(api, "createAgentMessage").mockReturnValueOnce(
    Promise.withResolvers<Message>().promise
  );
  const before = new HeldSends(
    new QueryClient({ defaultOptions: { mutations: { retry: false } } }),
    tab.page()
  );

  try {
    before.send(agentKey, agentSend);
    expect(keptState(tab, "send-1")).toEqual({ refusal: null, state: "sending" });

    // The reload: a page of the same tab, which takes up what the one before it kept.
    createAgentMessage.mockResolvedValueOnce(sentMessage);
    const after = new HeldSends(
      new QueryClient({ defaultOptions: { mutations: { retry: false } } }),
      tab.page()
    );
    after.resume();
    expect(after.get(agentKey)).toMatchObject({
      draft: { ask: agentSend.ask, draft: agentSend.draft },
      status: "sending",
    });
    expect(after.get(agentKey)?.request).toEqual(agentSend);
    await waitFor(() => expect(after.get(agentKey)).toBeUndefined());

    expect(createAgentMessage.mock.calls.map(([session, input]) => [session, input])).toEqual([
      ["planner-session", { body: "Status please", delivery: "steer", idempotency_key: "send-1" }],
      ["planner-session", { body: "Status please", delivery: "steer", idempotency_key: "send-1" }],
    ]);
    expect(tab.kept("send-1")).toBeUndefined();
    // Taken up once: a second call finds nothing more to send.
    after.resume();
    expect(createAgentMessage).toHaveBeenCalledTimes(2);
  } finally {
    createAgentMessage.mockRestore();
    tab.close();
  }
});

// A send past its deadline before the reload is past it after: Stop trying is there at once. One
// the reader was stopping lost its last attempt with the page, so it comes back stopped, its
// Retry still under the same key.
test("a late send resumes late, and one being stopped resumes stopped", async () => {
  const tab = keptTab();
  const createAgentMessage = spyOn(api, "createAgentMessage").mockRejectedValue(refusal);
  const before = new HeldSends(
    new QueryClient({ defaultOptions: { mutations: { retry: false } } }),
    tab.page()
  );

  try {
    await onFakeClock(async () => {
      before.send(agentKey, agentSend);
      await settle();
      await failUntilLate();
      expect(keptState(tab, "send-1")).toEqual({ refusal: null, state: "late" });

      const late = new HeldSends(
        new QueryClient({ defaultOptions: { mutations: { retry: false } } }),
        tab.page()
      );
      late.resume();
      expect(late.get(agentKey)?.status).toBe("late");
      expect(stoppable(late.get(agentKey))).toBe(true);
      await settle();
      expect(createAgentMessage.mock.calls.at(-1)?.[1].idempotency_key).toBe("send-1");

      // Stopped while its attempt is out, and the page reloads before that attempt answers.
      createAgentMessage.mockReturnValueOnce(Promise.withResolvers<Message>().promise);
      jest.advanceTimersByTime(1_000);
      await settle();
      expect(late.stop(agentKey)).toBe(true);
      expect(keptState(tab, "send-1")).toEqual({ refusal: null, state: "stopping" });
      const calls = createAgentMessage.mock.calls.length;

      const stopped = new HeldSends(
        new QueryClient({ defaultOptions: { mutations: { retry: false } } }),
        tab.page()
      );
      stopped.resume();
      expect(stopped.get(agentKey)?.status).toBe("refused");
      expect(stopped.get(agentKey)?.refusal).toBeInstanceOf(SendStoppedError);
      expect(keptState(tab, "send-1")).toEqual({ refusal: { kind: "stopped" }, state: "refused" });
      jest.advanceTimersByTime(30 * 60_000);
      await settle();
      expect(createAgentMessage).toHaveBeenCalledTimes(calls);
    });
  } finally {
    createAgentMessage.mockRestore();
    tab.close();
  }
});

// A refusal is the only copy of what the reader wrote: a reload shows it again, with what they
// wrote over it since, and sends nothing. Discard drops it from what is kept.
test("a refusal survives a reload with the draft written over it, until it is discarded", async () => {
  const tab = keptTab();
  const verdict = new ApiError(409, {
    code: "MESSAGE_KEY_REUSED",
    error: "the key names another message",
  });
  const createAgentMessage = spyOn(api, "createAgentMessage").mockRejectedValueOnce(verdict);
  const before = new HeldSends(
    new QueryClient({ defaultOptions: { mutations: { retry: false } } }),
    tab.page()
  );

  try {
    before.send(agentKey, agentSend);
    await waitFor(() => expect(before.get(agentKey)?.status).toBe("refused"));
    const rewritten = { ask: agentSend.ask, draft: { ...agentSend.draft, body: "Status, now" } };
    before.keep(agentKey, rewritten);

    const after = new HeldSends(
      new QueryClient({ defaultOptions: { mutations: { retry: false } } }),
      tab.page()
    );
    after.resume();
    const held = after.get(agentKey);
    expect(held).toMatchObject({ draft: rewritten, status: "refused", stopping: false });
    expect(held?.request).toEqual(agentSend);
    expect(held?.refusal).toBeInstanceOf(ApiError);
    expect(held?.refusal).toMatchObject({
      code: "MESSAGE_KEY_REUSED",
      message: "the key names another message",
      status: 409,
    });
    expect(createAgentMessage).toHaveBeenCalledTimes(1);

    expect(after.discard(agentKey)).toBe(true);
    expect(tab.kept("send-1")).toBeUndefined();
  } finally {
    createAgentMessage.mockRestore();
    tab.close();
  }
});

// A closed tab loses what sessionStorage keeps, so while a keyed send has no verdict the page asks
// to confirm leaving, and only then: a send that landed or was refused has its answer, and a
// comment carries no key and is not kept at all.
test("leaving asks for confirmation only while a keyed send is unconfirmed", async () => {
  const tab = keptTab();
  const unload = new UnloadTarget();
  const answer = Promise.withResolvers<Message>();
  const createAgentMessage = spyOn(api, "createAgentMessage").mockReturnValueOnce(answer.promise);
  const createComment = spyOn(api, "createComment").mockReturnValueOnce(
    Promise.withResolvers<Comment>().promise
  );
  const store = new HeldSends(
    new QueryClient({ defaultOptions: { mutations: { retry: false } } }),
    tab.page(unload)
  );

  try {
    store.send(["conversation-composer", "CORE-1"], request);
    expect(unload.listeners).toBe(0);
    expect(unload.asks()).toBe(false);
    expect(tab.storage.length).toBe(0);

    store.send(agentKey, agentSend);
    expect(unload.listeners).toBe(1);
    expect(unload.asks()).toBe(true);

    answer.resolve(sentMessage);
    await waitFor(() => expect(store.get(agentKey)).toBeUndefined());
    expect(unload.listeners).toBe(0);
    expect(unload.asks()).toBe(false);
    expect(tab.storage.length).toBe(0);

    createAgentMessage.mockRejectedValueOnce(
      new ApiError(400, { code: "MESSAGE_INPUT", error: "delivery must be steer, aside or btw" })
    );
    store.send(agentKey, { ...agentSend, sendKey: "send-2" });
    expect(unload.asks()).toBe(true);
    await waitFor(() => expect(store.get(agentKey)?.status).toBe("refused"));
    expect(unload.listeners).toBe(0);
    expect(unload.asks()).toBe(false);
    expect(keptState(tab, "send-2")).toMatchObject({ state: "refused" });
  } finally {
    createAgentMessage.mockRestore();
    createComment.mockRestore();
    tab.close();
  }
});
