import { expect, spyOn, test } from "bun:test";
import { type MutationKey, QueryClient } from "@tanstack/react-query";
import { waitFor } from "@testing-library/react";

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
