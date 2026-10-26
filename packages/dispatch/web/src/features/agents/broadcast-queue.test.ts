import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import { createElement } from "react";
import { MemoryRouter } from "react-router-dom";

import { keptTab, memoryFiles } from "../../__tests__/kept-sends-harness";
import { ApiError, api } from "../../api/client";
import type {
  BroadcastCreated,
  CreateBroadcastInput,
  Message,
  MessageDelivery,
} from "../../api/types";
import { Writes } from "../writes/write-store";
import { BroadcastSends } from "./BroadcastSends";
import { type BroadcastSend, broadcastRows, enqueueBroadcast } from "./broadcast-queue";

/** A press of Send: `body` to the Planner, under `key`. */
function press(body: string, key: string): BroadcastSend {
  return {
    input: { body, delivery: "btw", session_ids: ["planner-session"] },
    key,
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

/** Dispatch's answer to `input`: the broadcast it made, its one recipient's attempt `state`. */
function created(
  input: CreateBroadcastInput,
  state: MessageDelivery["state"] = "sent"
): BroadcastCreated {
  return {
    author: { id: "alice", kind: "user" },
    body: input.body,
    created_at: "2026-10-25T00:00:00Z",
    delivery: input.delivery,
    excluded: [],
    id: `broadcast-${input.body}`,
    recipients: [recipient(input, state)],
  };
}

function signedIn(login = "alice"): QueryClient {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login });
  return client;
}

async function settle(): Promise<void> {
  for (let turn = 0; turn < 30; turn += 1) await Promise.resolve();
}

// Broadcasts go one request at a time in press order, each under its key, so a reload that
// interrupts them loses nothing: the page after it takes up every one not yet made, in press
// order and under the same keys. A made broadcast stays in the strip, sent, and nothing of it is
// kept.
test("broadcasts go in press order, survive a reload, and a made one is kept no longer", async () => {
  const tab = keptTab();
  const first = Promise.withResolvers<BroadcastCreated>();
  const createBroadcast = spyOn(api, "createBroadcast").mockReturnValueOnce(first.promise);
  const send = spyOn(api, "send").mockImplementation(async (request) => {
    const input = request.body as CreateBroadcastInput;
    return created(input) as never;
  });
  const before = new Writes(signedIn(), tab.page(), memoryFiles());
  try {
    enqueueBroadcast(before, press("First.", "key-1"));
    enqueueBroadcast(before, press("Second.", "key-2"));
    await settle();
    expect(createBroadcast).toHaveBeenCalledTimes(1);
    expect(broadcastRows(before.rows()).map((row) => row.status)).toEqual(["queued", "sending"]);
    expect(tab.kept("key-2")).toMatchObject({ kind: "broadcast", state: { status: "queued" } });

    const after = new Writes(signedIn(), tab.page(), memoryFiles());
    after.resume("alice");
    await settle();
    // Both are made again behind each other, in press order, each under its own key.
    expect(send.mock.calls.map(([, bounds]) => bounds?.key)).toEqual(["key-1", "key-2"]);
    expect(createBroadcast.mock.calls.slice(1).map(([, bounds]) => bounds.key)).toEqual([
      "key-1",
      "key-2",
    ]);
    await waitFor(() =>
      expect(broadcastRows(after.rows()).map((row) => row.status)).toEqual(["sent", "sent"])
    );
    expect(tab.kept("key-1")).toBeUndefined();
  } finally {
    createBroadcast.mockRestore();
    send.mockRestore();
    tab.close();
  }
});

// A broadcast made with no answer to give has nothing to link: it leaves the strip, and every
// recipient's conversation and the broadcasts are read again.
test("a broadcast answered KEY_COMMITTED leaves the strip as made", async () => {
  const client = signedIn();
  const invalidate = spyOn(client, "invalidateQueries");
  const createBroadcast = spyOn(api, "createBroadcast").mockRejectedValueOnce(
    new ApiError(409, { code: "KEY_COMMITTED", error: "made" })
  );
  const writes = new Writes(client, keptTab().page(), memoryFiles());
  try {
    enqueueBroadcast(writes, press("First.", "key-1"));
    await waitFor(() => expect(broadcastRows(writes.rows())).toEqual([]));
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ["broadcast"] });
  } finally {
    createBroadcast.mockRestore();
  }
});

// W31. A broadcast Dispatch answered 201 is made, whatever its deliveries' state: it never reads as
// unconfirmed or offers Stop trying again. A recipient whose delivery is still on its way shows as
// delivering, and as delivered once the live feed's event refreshes the broadcast.
test("a broadcast answered 201 with a recipient still delivering is made and delivering, then delivered", async () => {
  const input = press("Ship it.", "key-1").input;
  const client = signedIn();
  const createBroadcast = spyOn(api, "createBroadcast").mockResolvedValueOnce(
    created(input, "pending")
  );
  const getBroadcast = spyOn(api, "getBroadcast").mockResolvedValue(created(input, "sent"));
  const writes = new Writes(client, keptTab().page(), memoryFiles());
  try {
    enqueueBroadcast(writes, press("Ship it.", "key-1"));
    await waitFor(() =>
      expect(broadcastRows(writes.rows()).map((row) => row.status)).toEqual(["sent"])
    );
    expect(createBroadcast).toHaveBeenCalledTimes(1);
    const view = render(
      createElement(
        QueryClientProvider,
        { client },
        createElement(
          MemoryRouter,
          null,
          createElement(BroadcastSends, {
            onDiscard: () => {},
            onRestore: () => {},
            onRetry: () => {},
            onStop: () => {},
            restoreRefusal: null,
            rows: broadcastRows(writes.rows()),
          })
        )
      )
    );
    try {
      expect(screen.getByRole("status").textContent).toContain("delivering to 1 agent");
      expect(screen.queryByRole("button", { name: "Stop trying" })).toBeNull();
      // The recipient's delivery event refreshes every broadcast view.
      await client.invalidateQueries({ queryKey: ["broadcast"] });
      await waitFor(() => expect(screen.getByRole("status").textContent).toContain("delivered"));
      expect(screen.getByRole("status").textContent).not.toContain("delivering");
    } finally {
      view.unmount();
    }
  } finally {
    createBroadcast.mockRestore();
    getBroadcast.mockRestore();
  }
});

// A refused broadcast stays failed, holding its draft, and no longer holds the line: the next one
// goes; its Retry goes under a new key since Dispatch refused it.
test("a refused broadcast does not hold the line, and its Retry takes a new key", async () => {
  const createBroadcast = spyOn(api, "createBroadcast")
    .mockRejectedValueOnce(new ApiError(400, { code: "BROADCAST_INPUT", error: "too long" }))
    .mockImplementation(async (input) => created(input));
  const writes = new Writes(signedIn(), keptTab().page(), memoryFiles());
  try {
    enqueueBroadcast(writes, press("First.", "key-1"));
    enqueueBroadcast(writes, press("Second.", "key-2"));
    await waitFor(() =>
      expect(broadcastRows(writes.rows()).map((row) => row.status)).toEqual(["sent", "failed"])
    );
    const failed = broadcastRows(writes.rows()).find((row) => row.status === "failed");
    writes.retry(failed?.id ?? 0);
    await waitFor(() =>
      expect(broadcastRows(writes.rows()).map((row) => row.status)).toEqual(["sent", "sent"])
    );
    const keys = createBroadcast.mock.calls.map(([, bounds]) => bounds.key);
    expect(keys.slice(0, 2)).toEqual(["key-1", "key-2"]);
    expect(keys[2]).not.toBe("key-1");
    // A retried send keeps its row's place in the strip.
    expect(broadcastRows(writes.rows()).map((row) => row.send.input.body)).toEqual([
      "Second.",
      "First.",
    ]);
  } finally {
    createBroadcast.mockRestore();
  }
});
