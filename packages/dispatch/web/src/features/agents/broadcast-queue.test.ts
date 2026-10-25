import { expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { waitFor } from "@testing-library/react";

import { type KeptTab, keptTab, UnloadTarget } from "../../__tests__/kept-sends-harness";
import { ApiError, api } from "../../api/client";
import type { BroadcastCreated, CreateBroadcastInput } from "../../api/types";
import { SendStoppedError } from "../conversation/send-request";
import { BroadcastQueue, type BroadcastSend } from "./broadcast-queue";

/** A press of Send: `body` to the Planner, under `key`. */
function press(body: string, key: string): BroadcastSend {
  return {
    input: { body, delivery: "btw", idempotency_key: key, session_ids: ["planner-session"] },
    picked: false,
    selected: ["planner-session"],
  };
}

/** Dispatch's answer to `input`: the broadcast it made. */
function created(input: CreateBroadcastInput): BroadcastCreated {
  return {
    author: { id: "alice", kind: "user" },
    body: input.body,
    created_at: "2026-10-25T00:00:00Z",
    delivery: input.delivery,
    excluded: [],
    id: `broadcast-${input.idempotency_key}`,
    recipients: [],
  };
}

/** A queue of a page of `tab`, as the app makes one. */
function queueOf(tab: KeptTab, unload?: EventTarget): BroadcastQueue {
  return new BroadcastQueue(new QueryClient(), tab.page(unload));
}

/** What `tab` keeps of the send under `key`: its state, or nothing. */
function keptState(tab: KeptTab, key: string): string | undefined {
  return (tab.kept(key) as { state: string } | undefined)?.state;
}

// W1. A broadcast goes under its key, so a reload that interrupts the queue loses nothing: the page
// after it takes up every send not yet sent, in press order and under the same keys - Dispatch
// answers a repeat with the broadcast the first request made, if it made one - and a send
// pressed before the reload stays one of the earlier visit's, which opens no broadcast page. Once
// a send lands, nothing of it is kept.
test("broadcasts out or queued survive a reload and are sent again under their keys, in press order", async () => {
  const tab = keptTab();
  const createBroadcast = spyOn(api, "createBroadcast").mockReturnValueOnce(
    Promise.withResolvers<BroadcastCreated>().promise
  );
  const before = queueOf(tab);

  try {
    before.enqueue(press("First.", "key-1"));
    before.enqueue(press("Second.", "key-2"));
    await waitFor(() => expect(createBroadcast).toHaveBeenCalledTimes(1));
    expect(keptState(tab, "key-1")).toBe("sending");
    expect(keptState(tab, "key-2")).toBe("queued");
    const pressedAt = before.rows().map((row) => row.submittedAt);

    const answers = [Promise.withResolvers<BroadcastCreated>()];
    createBroadcast
      .mockReturnValueOnce(answers[0]?.promise ?? Promise.reject())
      .mockImplementation(async (input) => created(input));
    const after = queueOf(tab);
    after.resume();
    expect(after.rows().map((row) => [row.send.input.body, row.status])).toEqual([
      ["Second.", "queued"],
      ["First.", "queued"],
    ]);
    expect(after.rows().map((row) => row.submittedAt)).toEqual(pressedAt);
    await waitFor(() => expect(createBroadcast).toHaveBeenCalledTimes(2));
    expect(after.rows().map((row) => row.status)).toEqual(["queued", "sending"]);

    answers[0]?.resolve(created(press("First.", "key-1").input));
    await waitFor(() => expect(after.rows().map((row) => row.status)).toEqual(["sent", "sent"]));
    expect(createBroadcast.mock.calls.map(([input]) => input.idempotency_key)).toEqual([
      "key-1",
      "key-1",
      "key-2",
    ]);
    expect(tab.kept("key-1")).toBeUndefined();
    expect(tab.kept("key-2")).toBeUndefined();

    // A press after the reload takes the next place in the strip.
    after.enqueue(press("Third.", "key-3"));
    expect(after.rows().map((row) => row.send.input.body)).toEqual(["Third.", "Second.", "First."]);
  } finally {
    createBroadcast.mockRestore();
    tab.close();
  }
});

// A refused send's row is the only copy of a message Send cleared from the composer, so a reload
// shows it again, Retry and Restore draft with it, and sends nothing. Restore draft's dismissal
// drops it from what is kept; Retry keeps it until it lands.
test("a refused broadcast survives a reload, and leaves what is kept once dismissed or sent", async () => {
  const tab = keptTab();
  const refusal = new ApiError(409, {
    code: "BROADCAST_KEY_REUSED",
    error: "the key names another broadcast",
  });
  const createBroadcast = spyOn(api, "createBroadcast")
    .mockRejectedValueOnce(refusal)
    .mockRejectedValueOnce(refusal);
  const before = queueOf(tab);

  try {
    before.enqueue(press("First.", "key-1"));
    before.enqueue(press("Second.", "key-2"));
    await waitFor(() =>
      expect(before.rows().map((row) => row.status)).toEqual(["failed", "failed"])
    );
    expect(keptState(tab, "key-1")).toBe("refused");

    const after = queueOf(tab);
    after.resume();
    const [second, first] = after.rows();
    expect(first?.send).toEqual({ ...press("First.", "key-1"), pressed: 1 });
    expect(first?.status).toBe("failed");
    const error = first?.status === "failed" ? first.error : undefined;
    expect(error).toBeInstanceOf(ApiError);
    expect(error).toMatchObject({
      code: "BROADCAST_KEY_REUSED",
      message: "the key names another broadcast",
      status: 409,
    });
    expect(createBroadcast).toHaveBeenCalledTimes(2);

    if (first === undefined || second === undefined) throw new Error("a refused row went missing");
    after.dismiss(first);
    expect(tab.kept("key-1")).toBeUndefined();

    createBroadcast.mockImplementation(async (input) => created(input));
    after.retry(second);
    expect(keptState(tab, "key-2")).toBe("queued");
    await waitFor(() => expect(after.rows().map((row) => row.status)).toEqual(["sent"]));
    expect(createBroadcast.mock.calls.at(-1)?.[0].idempotency_key).toBe("key-2");
    expect(tab.kept("key-2")).toBeUndefined();
  } finally {
    createBroadcast.mockRestore();
    tab.close();
  }
});

// A send the reader was stopping lost its last attempt with the page: it comes back stopped, with
// Retry under the same key, and nothing is sent until the reader asks.
test("a broadcast being stopped resumes stopped", async () => {
  const tab = keptTab();
  const createBroadcast = spyOn(api, "createBroadcast");
  const after = queueOf(tab);

  try {
    tab.storage.setItem(
      "dispatch.keyed-send:key-1",
      JSON.stringify({
        key: "key-1",
        kind: "broadcast",
        refusal: null,
        send: { send: { ...press("First.", "key-1"), pressed: 4 }, submittedAt: 1 },
        state: "stopping",
        version: 1,
      })
    );
    after.resume();
    const [row] = after.rows();
    expect(row?.status).toBe("failed");
    expect(row?.status === "failed" ? row.error : undefined).toBeInstanceOf(SendStoppedError);
    expect(keptState(tab, "key-1")).toBe("refused");
    expect(createBroadcast).not.toHaveBeenCalled();
  } finally {
    createBroadcast.mockRestore();
    tab.close();
  }
});

// A closed tab loses what sessionStorage keeps, so while a broadcast is queued or out the page asks
// to confirm leaving, and once every one has its answer it stops asking.
test("leaving asks for confirmation only while a broadcast is unconfirmed", async () => {
  const tab = keptTab();
  const unload = new UnloadTarget();
  const answer = Promise.withResolvers<BroadcastCreated>();
  const createBroadcast = spyOn(api, "createBroadcast")
    .mockReturnValueOnce(answer.promise)
    .mockRejectedValueOnce(new ApiError(400, { code: "BROADCAST_EMPTY", error: "nobody" }));
  const queue = queueOf(tab, unload);

  try {
    expect(unload.asks()).toBe(false);
    queue.enqueue(press("First.", "key-1"));
    queue.enqueue(press("Second.", "key-2"));
    expect(unload.listeners).toBe(1);
    expect(unload.asks()).toBe(true);

    answer.resolve(created(press("First.", "key-1").input));
    await waitFor(() => expect(queue.rows().map((row) => row.status)).toEqual(["failed", "sent"]));
    expect(unload.listeners).toBe(0);
    expect(unload.asks()).toBe(false);
  } finally {
    createBroadcast.mockRestore();
    tab.close();
  }
});
