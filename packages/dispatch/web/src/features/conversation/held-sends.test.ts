import { expect, spyOn, test } from "bun:test";
import { hashKey, type MutationKey, QueryClient } from "@tanstack/react-query";

import { ApiError, api } from "../../api/client";
import type { Comment } from "../../api/types";
import { HeldSends } from "./held-sends";
import type { SentRequest } from "./send-request";

const request: SentRequest = {
  anchor: undefined,
  ask: { multiple: false, options: [], urgency: "med" },
  draft: { body: "Held", mentions: [], replacement: "" },
  edit: undefined,
  kind: "comment",
  owner: { issueKey: "CORE-1", kind: "issue" },
  replyTo: null,
};

const refusal = new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" });

/** Resolves once the store tells what became of the send under `mutationKey`. */
function outcomeOf(store: HeldSends, mutationKey: MutationKey): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  const stop = store.onOutcome(({ send }) => {
    if (hashKey(send.mutationKey) !== hashKey(mutationKey)) return;
    stop();
    resolve();
  });
  return promise;
}

// `under` is what `useHeldSendsUnder` hands React as its snapshot, so the array for a prefix is
// the same one until a send beneath that prefix changes - a send elsewhere re-renders nothing that
// watches it - and a new one as soon as one beneath it does, or the watcher shows a stale send.
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

    store.send(cardKey, request);
    const sending = store.under(prefix);
    expect(sending.map((held) => held.status)).toEqual(["sending"]);

    store.send(dockedKey, request);
    expect(store.under(prefix)).toBe(sending);
    const dockedRefused = outcomeOf(store, dockedKey);
    docked.reject(refusal);
    await dockedRefused;
    expect(store.get(dockedKey)?.status).toBe("refused");
    expect(store.under(prefix)).toBe(sending);

    const cardRefused = outcomeOf(store, cardKey);
    cardReply.reject(refusal);
    await cardRefused;
    expect(store.under(prefix).map((held) => held.status)).toEqual(["refused"]);

    store.discard(cardKey);
    expect(store.under(prefix)).toEqual([]);
  } finally {
    createComment.mockRestore();
  }
});
