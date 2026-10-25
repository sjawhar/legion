import { expect, jest, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { waitFor } from "@testing-library/react";

import { type KeptTab, keptTab, UnloadTarget } from "../../__tests__/kept-sends-harness";
import { ApiError, api } from "../../api/client";
import type {
  BroadcastCreated,
  CreateBroadcastInput,
  Message,
  MessageDelivery,
} from "../../api/types";
import { KEPT_SEND_RESUME_MS } from "../conversation/keyed-send-storage";
import { SendInterruptedError, SendStoppedError } from "../conversation/send-request";
import { BroadcastQueue, type BroadcastSend } from "./broadcast-queue";

/** A press of Send: `body` to the Planner, under `key`. */
function press(body: string, key: string): BroadcastSend {
  return {
    input: { body, delivery: "btw", idempotency_key: key, session_ids: ["planner-session"] },
    picked: false,
    selected: ["planner-session"],
  };
}

/** One recipient of `input`, its message's one attempt in `state`. */
function recipient(input: CreateBroadcastInput, state: MessageDelivery["state"]) {
  return {
    message: {
      author: { id: "alice", kind: "user" },
      body: input.body,
      created_at: "2026-10-25T00:00:00Z",
      deliveries: [
        {
          attempt: 1,
          created_at: "2026-10-25T00:00:00Z",
          delivery: input.delivery,
          envelope_id: null,
          error: null,
          message_id: "message-1",
          reply_id: null,
          session_id: "planner-session",
          state,
        },
      ],
      id: "message-1",
      in_reply_to: null,
      issue_key: null,
      target: "session:planner-session",
    } as Message,
    replies: [],
    session_id: "planner-session",
  };
}

/** Dispatch's answer to `input`: the broadcast it made, its one recipient's attempt `state`
 *  ("sent" unless a test says otherwise; no recipient at all where `state` is null). */
function created(
  input: CreateBroadcastInput,
  state: MessageDelivery["state"] | null = null
): BroadcastCreated {
  return {
    author: { id: "alice", kind: "user" },
    body: input.body,
    created_at: "2026-10-25T00:00:00Z",
    delivery: input.delivery,
    excluded: [],
    id: `broadcast-${input.idempotency_key}`,
    recipients: state === null ? [] : [recipient(input, state)],
  };
}

/** A queue of a page of `tab`, signed in as `login`, as the app makes one. */
function queueOf(tab: KeptTab, unload?: EventTarget, login = "alice"): BroadcastQueue {
  const client = new QueryClient();
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login });
  return new BroadcastQueue(client, tab.page(unload));
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
    after.resume("alice");
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
    after.resume("alice");
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

// A send the reader was stopping lost its last attempt with the page: the page after it asks
// Dispatch once more under the key (W9) and, Dispatch not taking it, leaves it stopped, with
// Retry under the same key and nothing sent until the reader asks.
test("a broadcast being stopped asks once more and resumes stopped", async () => {
  const tab = keptTab();
  const createBroadcast = spyOn(api, "createBroadcast").mockRejectedValue(
    new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" })
  );
  const after = queueOf(tab);

  try {
    tab.storage.setItem(
      "dispatch.keyed-send:key-1",
      JSON.stringify({
        key: "key-1",
        kind: "broadcast",
        login: "alice",
        refusal: null,
        send: { send: { ...press("First.", "key-1"), pressed: 4 }, submittedAt: Date.now() },
        state: "stopping",
        version: 2,
      })
    );
    after.resume("alice");
    await waitFor(() => expect(after.rows()[0]?.status).toBe("failed"));
    const [row] = after.rows();
    expect(row?.status === "failed" ? row.error : undefined).toBeInstanceOf(SendStoppedError);
    expect(keptState(tab, "key-1")).toBe("refused");
    expect(createBroadcast).toHaveBeenCalledTimes(1);
    expect(createBroadcast.mock.calls[0]?.[0].idempotency_key).toBe("key-1");
  } finally {
    createBroadcast.mockRestore();
    tab.close();
  }
});

// W11. A broadcast past its deadline when the page went is past it after the reload: its row
// says so and offers Stop trying at once, rather than starting its deadline over.
test("a late broadcast resumes late", async () => {
  const tab = keptTab();
  const createBroadcast = spyOn(api, "createBroadcast").mockReturnValue(
    Promise.withResolvers<BroadcastCreated>().promise
  );
  const after = queueOf(tab);

  try {
    tab.storage.setItem(
      "dispatch.keyed-send:key-1",
      JSON.stringify({
        key: "key-1",
        kind: "broadcast",
        login: "alice",
        refusal: null,
        send: { send: { ...press("First.", "key-1"), pressed: 1 }, submittedAt: Date.now() },
        state: "late",
        version: 2,
      })
    );
    after.resume("alice");
    await waitFor(() => expect(createBroadcast).toHaveBeenCalledTimes(1));
    const [row] = after.rows();
    expect(row?.status).toBe("late");
    expect(row?.status === "late" ? row.stopping : undefined).toBe(false);
    expect(keptState(tab, "key-1")).toBe("late");
  } finally {
    createBroadcast.mockRestore();
    tab.close();
  }
});

// W6. A broadcast belongs to the person who pressed it: the page another person signs in to
// sends none of it and drops what was kept.
test("a kept broadcast is another person's to resume, and is dropped unsent", async () => {
  const tab = keptTab();
  const createBroadcast = spyOn(api, "createBroadcast").mockReturnValueOnce(
    Promise.withResolvers<BroadcastCreated>().promise
  );
  const alice = queueOf(tab);

  try {
    alice.enqueue(press("First.", "key-1"));
    await waitFor(() => expect(createBroadcast).toHaveBeenCalledTimes(1));
    expect(tab.kept("key-1")).toMatchObject({ login: "alice" });

    const bob = queueOf(tab, undefined, "bob");
    bob.resume("bob");
    expect(bob.rows()).toEqual([]);
    expect(createBroadcast).toHaveBeenCalledTimes(1);
    expect(tab.kept("key-1")).toBeUndefined();
  } finally {
    createBroadcast.mockRestore();
    tab.close();
  }
});

// W7. A broadcast kept longer than five minutes is not sent on its own: the row comes back
// failed, saying the page was closed before Dispatch confirmed it, with Retry under its key.
test("a broadcast kept over five minutes comes back failed rather than sent", async () => {
  const tab = keptTab();
  const createBroadcast = spyOn(api, "createBroadcast");
  const after = queueOf(tab);

  try {
    tab.storage.setItem(
      "dispatch.keyed-send:key-1",
      JSON.stringify({
        key: "key-1",
        kind: "broadcast",
        login: "alice",
        refusal: null,
        send: {
          send: { ...press("First.", "key-1"), pressed: 1 },
          submittedAt: Date.now() - KEPT_SEND_RESUME_MS - 1_000,
        },
        state: "sending",
        version: 2,
      })
    );
    after.resume("alice");
    const [row] = after.rows();
    expect(row?.status).toBe("failed");
    const error = row?.status === "failed" ? row.error : undefined;
    expect(error).toBeInstanceOf(SendInterruptedError);
    expect(error?.message).toBe(
      "Not sent: this page was closed before Dispatch confirmed it. Retry sends it under the same key, so it is delivered at most once."
    );
    expect(createBroadcast).not.toHaveBeenCalled();

    // Retry sends the very request again, under the key Dispatch answers it with.
    createBroadcast.mockImplementation(async (input) => created(input, "sent"));
    if (row === undefined) throw new Error("the kept row went missing");
    after.retry(row);
    await waitFor(() => expect(after.rows()[0]?.status).toBe("sent"));
    expect(createBroadcast.mock.calls[0]?.[0].idempotency_key).toBe("key-1");
  } finally {
    createBroadcast.mockRestore();
    tab.close();
  }
});

/** Lets every promise the queue chained run as far as it goes without the clock moving. */
async function settle(): Promise<void> {
  for (let turn = 0; turn < 20; turn += 1) await Promise.resolve();
}

// W12. Dispatch answers a broadcast before it has delivered it, so a 2xx whose recipient still
// has one pending attempt is not the send's outcome: the queue keeps asking under the key, which
// is what lets Dispatch resume a recipient whose sender died, and settles when every recipient
// is decided.
test("a broadcast with a pending recipient is asked again under its key until it is decided", async () => {
  const tab = keptTab();
  const createBroadcast = spyOn(api, "createBroadcast")
    .mockImplementationOnce(async (input) => created(input, "pending"))
    .mockImplementationOnce(async (input) => created(input, "pending"))
    .mockImplementationOnce(async (input) => created(input, "sent"));
  const queue = queueOf(tab);
  jest.useFakeTimers();

  try {
    queue.enqueue(press("First.", "key-1"));
    await settle();
    expect(createBroadcast).toHaveBeenCalledTimes(1);
    expect(queue.rows()[0]?.status).toBe("sending");

    for (const [index, wait] of [1_000, 2_000].entries()) {
      jest.advanceTimersByTime(wait);
      await settle();
      expect(createBroadcast).toHaveBeenCalledTimes(index + 2);
    }
    expect(queue.rows()[0]?.status).toBe("sent");
    expect(new Set(createBroadcast.mock.calls.map(([input]) => input.idempotency_key)).size).toBe(
      1
    );
  } finally {
    jest.advanceTimersByTime(1);
    jest.useRealTimers();
    createBroadcast.mockRestore();
    tab.close();
  }
});

// W12. The repeat a pending recipient asks for is answered with the broadcast as it stands, but
// Dispatch stores no exclusions, so the repeat names every excluded session only as "excluded by
// the original send". The row keeps what the answer that made the broadcast said - the reason
// the sender needs - and the recipients of the latest answer.
test("a repeat under the key keeps the first answer's reasons for an excluded session", async () => {
  const tab = keptTab();
  const excludedAs = (reason: string) => [{ reason, session_id: "observer", title: "Observer" }];
  const createBroadcast = spyOn(api, "createBroadcast")
    .mockImplementationOnce(async (input) => ({
      ...created(input, "pending"),
      excluded: excludedAs("no live session"),
    }))
    .mockImplementationOnce(async (input) => ({
      ...created(input, "sent"),
      excluded: excludedAs("excluded by the original send"),
    }));
  const queue = queueOf(tab);
  jest.useFakeTimers();

  try {
    queue.enqueue(press("First.", "key-1"));
    await settle();
    jest.advanceTimersByTime(1_000);
    await settle();
    expect(createBroadcast).toHaveBeenCalledTimes(2);
    const [row] = queue.rows();
    expect(row?.status).toBe("sent");
    const made = row?.status === "sent" ? row.created : undefined;
    expect(made?.excluded).toEqual(excludedAs("no live session"));
    expect(made?.recipients.map(({ message }) => message.deliveries[0]?.state)).toEqual(["sent"]);
  } finally {
    jest.advanceTimersByTime(1);
    jest.useRealTimers();
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
