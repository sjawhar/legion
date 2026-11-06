import { expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";

import { api, NetworkError } from "../../api/client";
import type { UserIssueState } from "../../api/types";
import { writesOf } from "../writes/write-store";
import { writePin } from "./pin-write";
import { pinnedItemMarker } from "./pins";

/** A page's query client, signed in as `login`. */
function signedIn(login: string): QueryClient {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login });
  return client;
}

async function settle(): Promise<void> {
  for (let turn = 0; turn < 30; turn += 1) await Promise.resolve();
}

const fresh: UserIssueState = { dismissed: [], last_read_seq: 0, pinned: false, seq: 0 };

/**
 * Alice's first pin of CORE-1 lands while her second is still out, so the state its answer gave
 * is what the second writes over; then `end` ends her session on the page. Answers with what the
 * next pin of CORE-1 sends.
 */
async function nextPinAfter(end: (client: QueryClient) => void) {
  const client = signedIn("alice");
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({ "CORE-1": fresh });
  const alicesAnswer: UserIssueState = { ...fresh, dismissed: [pinnedItemMarker("ask:a")], seq: 1 };
  const put = spyOn(api, "putIssueState")
    .mockResolvedValueOnce(alicesAnswer)
    .mockRejectedValueOnce(new NetworkError("/api/v1/me/issues/CORE-1/state", new TypeError()));
  try {
    writePin(client, "CORE-1", { id: "ask:a", op: "pin" });
    writePin(client, "CORE-1", { id: "ask:b", op: "pin" });
    await settle();
    expect(put).toHaveBeenCalledTimes(2);
    end(client);
    client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "bob" });
    put.mockResolvedValue({ ...fresh, seq: 1 });
    writePin(client, "CORE-1", { id: "ask:c", op: "pin" });
    await settle();
    return put.mock.calls.at(-1)?.[1];
  } finally {
    writesOf(client).clear();
    getMyState.mockRestore();
    put.mockRestore();
  }
}

// W36. The issue state a pin's answer left is what the next pin of that issue writes over, so it
// belongs to the person signed in: once someone else signs in, or the person signs out, it goes,
// and so does a pin of theirs dropped unmade. The next pin - bob's - reads his own state afresh
// rather than writing over alice's.
test("another sign-in lets go of the last pin's answer", async () => {
  let rolledBack = false;
  const sent = await nextPinAfter((client) => {
    const invalidate = spyOn(client, "invalidateQueries");
    writesOf(client).signedIn("bob");
    // The dropped pin is undone as a refused one is: the page reads its state again.
    rolledBack = invalidate.mock.calls.some(
      ([filters]) => JSON.stringify(filters?.queryKey) === '["user-state"]'
    );
    invalidate.mockRestore();
  });
  expect(rolledBack).toBe(true);
  expect(sent).toEqual({ dismissed: [pinnedItemMarker("ask:c")], seq: 1 });
});

test("a sign-out lets go of the last pin's answer", async () => {
  const sent = await nextPinAfter((client) => writesOf(client).clear());
  expect(sent).toEqual({ dismissed: [pinnedItemMarker("ask:c")], seq: 1 });
});
