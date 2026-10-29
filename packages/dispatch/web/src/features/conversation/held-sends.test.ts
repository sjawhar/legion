import { expect, jest, spyOn, test } from "bun:test";
import {
  type MutationKey,
  onlineManager,
  QueryClient,
  QueryClientProvider,
} from "@tanstack/react-query";
import { act, render, renderHook, waitFor } from "@testing-library/react";
import { createElement, type ReactNode } from "react";

import { keptTab, memoryFiles, UnloadTarget } from "../../__tests__/kept-sends-harness";
import { ApiError, api, NetworkError } from "../../api/client";
import { agentMessagesQuery } from "../../api/queries";
import type { Message } from "../../api/types";
import { useSending } from "../../hooks/useSending";
import { SEND_DEADLINE_MS, SendStoppedError } from "../writes/keyed-write";
import { Writes } from "../writes/write-store";
import { type HeldSend, HeldSends, heldSends, stoppable } from "./held-sends";
import { PendingSendTurn } from "./PendingSendTurn";
import type { SentRequest } from "./send-request";
import { useSessionSends } from "./useSessionSends";

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

/** A message to one session, Send's own: the request the Agents page's composer holds. */
const agentSend: SentRequest = {
  ...request,
  draft: { body: "Status please", mentions: [], replacement: "" },
  owner: { kind: "session", sessionId: "planner-session" },
  sendKey: "send-1",
};

const agentKey: MutationKey = ["agent-composer", "planner-session"];

const sentMessage = {
  body: "Status please",
  deliveries: [],
  id: "message-1",
} as unknown as Message;

/** Dispatch's verdict on a send: it is not tried again, and the composer shows it. */
const verdict = new ApiError(400, { code: "COMMENT_INPUT", error: "the body is empty" });

/** A page's query client, signed in: the store stamps every send with this login at its press
 *  and resumes only this login's, as the app's does. */
function signedIn(login = "alice"): QueryClient {
  const client = new QueryClient({ defaultOptions: { mutations: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login });
  return client;
}

/** The held sends of a page of `tab`, over a store of their own. */
function heldOf(tab = keptTab(), client = signedIn(), unload?: EventTarget) {
  const writes = new Writes(client, tab.page(unload), memoryFiles());
  return { held: new HeldSends(writes), writes };
}

async function settle(): Promise<void> {
  for (let turn = 0; turn < 30; turn += 1) await Promise.resolve();
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

// A composer holds one draft: a send under a name that is out refuses another, a refused one gives
// way to the next, and Discard drops a refusal but never a send still out.
test("one send per composer name: out refuses another, refused gives way", async () => {
  const first = Promise.withResolvers<unknown>();
  const createComment = spyOn(api, "createComment")
    .mockReturnValueOnce(first.promise as never)
    .mockResolvedValueOnce({} as never);
  const { held } = heldOf();
  const name: MutationKey = ["conversation-composer", "CORE-1"];
  try {
    expect(held.send(name, request)).toBe(true);
    expect(held.send(name, { ...request, sendKey: "send-x" })).toBe(false);
    expect(held.discard(name)).toBe(false);
    first.reject(verdict);
    await waitFor(() => expect(held.get(name)?.status).toBe("refused"));
    expect(held.send(name, { ...request, sendKey: "send-2" })).toBe(true);
    await waitFor(() => expect(held.get(name)).toBeUndefined());
    expect(createComment).toHaveBeenCalledTimes(2);
  } finally {
    createComment.mockRestore();
  }
});

// A send that lands refreshes what it wrote whether or not a composer is mounted: for a message to
// one session, that session's conversation. One Dispatch made with no answer to give lands the
// same way, with no refusal.
test("a send that lands, with or without its answer, refreshes its session", async () => {
  for (const answer of [
    () => Promise.resolve(sentMessage),
    () => Promise.reject(new ApiError(409, { code: "KEY_COMMITTED", error: "made" })),
  ]) {
    const client = signedIn();
    const invalidate = spyOn(client, "invalidateQueries");
    const createAgentMessage = spyOn(api, "createAgentMessage").mockImplementationOnce(answer);
    const { held } = heldOf(keptTab(), client);
    const outcomes: string[] = [];
    const stop = held.onOutcome((outcome) => outcomes.push(outcome.kind));
    try {
      held.send(agentKey, agentSend);
      await waitFor(() => expect(held.get(agentKey)).toBeUndefined());
      expect(outcomes).toEqual(["landed"]);
      expect(invalidate).toHaveBeenCalledWith({
        queryKey: agentMessagesQuery("planner-session").queryKey,
      });
    } finally {
      stop();
      createAgentMessage.mockRestore();
    }
  }
});

// A refusal survives a reload with what the reader wrote over it, its request whole, until it is
// discarded.
test("a refusal survives a reload with the draft written over it, until it is discarded", async () => {
  const tab = keptTab();
  const refused = new ApiError(422, { code: "KEY_REUSED", error: "the key names another write" });
  const createAgentMessage = spyOn(api, "createAgentMessage").mockRejectedValueOnce(refused);
  const before = heldOf(tab).held;
  try {
    before.send(agentKey, agentSend);
    await waitFor(() => expect(before.get(agentKey)?.status).toBe("refused"));
    const rewritten = { ask: agentSend.ask, draft: { ...agentSend.draft, body: "Status, now" } };
    before.keep(agentKey, rewritten);

    const { held: after, writes } = heldOf(tab);
    writes.resume("alice");
    await settle();
    const held = after.get(agentKey);
    expect(held).toMatchObject({ draft: rewritten, status: "refused" });
    expect(held?.request).toEqual(agentSend);
    expect(held?.status === "refused" && held.refusal).toMatchObject({
      code: "KEY_REUSED",
      status: 422,
    });
    expect(createAgentMessage).toHaveBeenCalledTimes(1);
    expect(after.discard(agentKey)).toBe(true);
    expect(tab.kept("send-1")).toBeUndefined();
  } finally {
    createAgentMessage.mockRestore();
    tab.close();
  }
});

// A reply to a thread is resumed whole: the reply it answers and the mode it was picked in, so a
// reload sends the same message to the same place.
test("a reply to a thread is kept and resumed whole", async () => {
  const tab = keptTab();
  const threaded: SentRequest = {
    ...agentSend,
    pickedDelivery: "aside",
    replyTo: {
      actor: { id: "planner-session", kind: "session", origin: { session_title: "Planner" } },
      excerpt: "Can this ship?",
      id: "message-9",
      mentions: [{ target: "session:planner-session", title: "Planner" }],
      parentKind: "message",
      thread: { delivery: "btw", target: "session:planner-session" },
      to: "alice",
    },
    sendKey: "send-9",
  };
  const createAgentMessage = spyOn(api, "createAgentMessage").mockReturnValue(
    Promise.withResolvers<Message>().promise
  );
  try {
    heldOf(tab).held.send(agentKey, threaded);
    const { held: after, writes } = heldOf(tab);
    writes.resume("alice");
    await settle();
    expect(after.get(agentKey)?.request).toEqual({
      ...threaded,
      anchor: undefined,
      edit: undefined,
    });
    expect(createAgentMessage.mock.calls.at(-1)?.[1]).toEqual({
      body: "Status please",
      delivery: "aside",
      in_reply_to: "message-9",
    });
    expect(createAgentMessage.mock.calls.at(-1)?.[2]?.key).toBe("send-9");
  } finally {
    createAgentMessage.mockRestore();
    tab.close();
  }
});

// A closed tab loses what sessionStorage keeps, so while a send has no verdict the page asks to
// confirm leaving, and only then.
test("leaving asks for confirmation only while a send is unconfirmed", async () => {
  const unload = new UnloadTarget();
  const answer = Promise.withResolvers<Message>();
  const createAgentMessage = spyOn(api, "createAgentMessage").mockReturnValueOnce(answer.promise);
  const { held } = heldOf(keptTab(), signedIn(), unload);
  try {
    expect(unload.asks()).toBe(false);
    held.send(agentKey, agentSend);
    expect(unload.asks()).toBe(true);
    answer.resolve(sentMessage);
    await waitFor(() => expect(unload.asks()).toBe(false));
  } finally {
    createAgentMessage.mockRestore();
  }
});

// A send pressed while the browser is offline is tried, not parked: its own retries wait out the
// network, it goes late at its deadline - where Stop trying is offered - and a host's hold on its
// name counts it the whole time (`useSending`), until it lands once the network is back.
test("a send pressed offline is tried, goes late, holds its name, and lands", async () => {
  const online = onlineManager.isOnline();
  const createAgentMessage = spyOn(api, "createAgentMessage").mockRejectedValue(
    new NetworkError("/api/v1/agents/planner-session/messages", new TypeError("Failed to fetch"))
  );
  const client = signedIn();
  const { held } = heldOf(keptTab(), client);
  try {
    onlineManager.setOnline(false);
    await onFakeClock(async () => {
      held.send(agentKey, agentSend);
      await settle();
      expect(createAgentMessage).toHaveBeenCalledTimes(1);
      expect(client.isMutating({ mutationKey: agentKey })).toBe(1);
      for (let left = SEND_DEADLINE_MS; left > 0; left -= 500) {
        jest.advanceTimersByTime(500);
        await settle();
      }
      expect(held.get(agentKey)?.status).toBe("late");
      expect(stoppable(held.get(agentKey))).toBe(true);
      expect(client.isMutating({ mutationKey: agentKey })).toBe(1);

      createAgentMessage.mockResolvedValueOnce(sentMessage);
      onlineManager.setOnline(true);
      for (let left = 31_000; left > 0; left -= 500) {
        jest.advanceTimersByTime(500);
        await settle();
      }
      expect(held.get(agentKey)).toBeUndefined();
      expect(client.isMutating({ mutationKey: agentKey })).toBe(0);
    });
    expect(new Set(createAgentMessage.mock.calls.map(([, , bounds]) => bounds?.key)).size).toBe(1);
  } finally {
    onlineManager.setOnline(online);
    createAgentMessage.mockRestore();
  }
});

// A send to a session belongs to that session, not the surface that made it: its live view and its
// Agents row read the session's sends from the one store, share its press-order line, and Retry or
// Discard from either acts on the same write. Two presses on the live view and one on the row go
// out in press order, each once, and the row's Retry of a refused live-view message is that
// message's.
test("a session's sends are one line, shown and acted on from every surface of it", async () => {
  await onFakeClock(async () => {
    const client = signedIn();
    const bodies: string[] = [];
    const first = Promise.withResolvers<Message>();
    const createAgentMessage = spyOn(api, "createAgentMessage").mockImplementation(
      (_session, input) => {
        bodies.push(input.body);
        return input.body === "one" ? first.promise : Promise.withResolvers<Message>().promise;
      }
    );
    const wrapper = ({ children }: { children: ReactNode }) =>
      createElement(QueryClientProvider, { client }, children);
    const live = renderHook(() => useSessionSends("planner-session"), { wrapper });
    const row = renderHook(() => useSessionSends("planner-session"), { wrapper });
    const rowHold = renderHook(() => useSending(agentKey), { wrapper });
    try {
      act(() => {
        live.result.current.send(["agent-live", "planner-session"], "one", "steer");
        live.result.current.send(["agent-live", "planner-session"], "two", "steer");
      });
      act(() => {
        heldSends(client).send(agentKey, { ...agentSend, sendKey: "row-1" });
      });
      await act(settle);
      // One out, the rest waiting their turn behind it, in press order, on both surfaces.
      expect(bodies).toEqual(["one"]);
      expect(live.result.current.pending.map((send) => send.draft.draft.body)).toEqual([
        "one",
        "two",
        "Status please",
      ]);
      expect(row.result.current.pending).toEqual(live.result.current.pending);
      // The row's own composer is held for its own send, not for the live view's.
      expect(rowHold.result.current.sending).toBe(true);

      first.reject(verdict);
      await act(settle);
      expect(bodies).toEqual(["one", "two"]);
      const refused = row.result.current.pending.find((send) => send.status === "refused");
      expect(refused?.draft.draft.body).toBe("one");

      // Retry from the row sends the live view's message, at the back of the line.
      act(() => row.result.current.retry(refused as never));
      await act(settle);
      expect(live.result.current.pending.map((send) => send.draft.draft.body)).toEqual([
        "two",
        "Status please",
        "one",
      ]);
      expect(
        createAgentMessage.mock.calls.every(([session]) => session === "planner-session")
      ).toBe(true);
    } finally {
      live.unmount();
      row.unmount();
      rowHold.unmount();
      createAgentMessage.mockRestore();
    }
  });
});

// Stop trying on a late send lets the attempt in flight answer and makes none after it; the send
// is then refused stopped, and its Retry goes under the same key.
test("a late send can be stopped, and its Retry goes under its key", async () => {
  await onFakeClock(async () => {
    const createAgentMessage = spyOn(api, "createAgentMessage").mockRejectedValue(
      new ApiError(503, { code: "UNAVAILABLE", error: "down" })
    );
    const { held } = heldOf();
    try {
      held.send(agentKey, agentSend);
      for (let left = SEND_DEADLINE_MS + 1_000; left > 0; left -= 500) {
        jest.advanceTimersByTime(500);
        await settle();
      }
      expect(held.stop(agentKey)).toBe(true);
      await settle();
      jest.advanceTimersByTime(60_000);
      await settle();
      const stopped = held.get(agentKey);
      expect(stopped?.status === "refused" && stopped.refusal).toBeInstanceOf(SendStoppedError);
      const calls = createAgentMessage.mock.calls.length;
      jest.advanceTimersByTime(30 * 60_000);
      await settle();
      expect(createAgentMessage).toHaveBeenCalledTimes(calls);
    } finally {
      createAgentMessage.mockRestore();
    }
  });
});

// W51. A message the person's sign-out in another tab holds says so on its pending turn, and its
// trying goes on once that tab's hold lifts.
test("a send held for a sign-out in another tab says so, and goes on once released", async () => {
  await onFakeClock(async () => {
    const createAgentMessage = spyOn(api, "createAgentMessage").mockRejectedValue(
      new NetworkError("/api/v1/agents/planner-session/messages", new TypeError("Failed to fetch"))
    );
    const { held, writes } = heldOf();
    const sends = { discard: () => {}, retry: () => {}, stop: () => {} };
    try {
      held.send(agentKey, agentSend);
      await settle();
      writes.holdFor("alice");
      await settle();
      const waiting = held.get(agentKey);
      expect(waiting?.signingOutElsewhere).toBe(true);
      const turn = render(createElement(PendingSendTurn, { held: waiting as HeldSend, sends }));
      expect(turn.getByRole("status").textContent).toBe("Waiting for a sign-out in another tab");
      turn.unmount();
      const calls = createAgentMessage.mock.calls.length;
      jest.advanceTimersByTime(10_000);
      await settle();
      expect(createAgentMessage).toHaveBeenCalledTimes(calls);

      writes.release("alice");
      await settle();
      const going = held.get(agentKey);
      expect(going?.signingOutElsewhere).toBe(false);
      expect(createAgentMessage.mock.calls.length).toBeGreaterThan(calls);
      const again = render(createElement(PendingSendTurn, { held: going as HeldSend, sends }));
      expect(again.getByRole("status").textContent).toBe("Sending…");
      again.unmount();
    } finally {
      createAgentMessage.mockRestore();
    }
  });
});
