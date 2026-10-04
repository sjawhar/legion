import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import { createElement, type ReactNode } from "react";

import { api } from "../../api/client";
import { userAgentStateQuery } from "../../api/queries";
import type {
  Message,
  UserAgentState,
  UserAgentStateInput,
  UserAgentStates,
} from "../../api/types";
import { totalUnreadReplies, useMarkShownRepliesRead } from "./unread";

// An API older than the unread count answers a session's state with its Clear alone. The badge
// then counts that session as nothing unread rather than reading "New replies NaN".
test("the unread total counts a state with no unread count as none", () => {
  const states: UserAgentStates = {
    old: { cleared_before: "2026-09-27T20:00:00Z" },
    s1: { unread_replies: 2 },
  };
  expect(totalUnreadReplies(states)).toBe(2);
});

const SESSION = "planner-session";

function reply(id: string): Message {
  return {
    author: { id: SESSION, kind: "session" },
    body: "Done.",
    created_at: "2026-10-03T12:00:00Z",
    deliveries: [],
    id,
    in_reply_to: "message-1",
    issue_key: null,
    target: null,
  };
}

/** Mounts the broadcast page's read mark for `SESSION`, showing reply `a1`, against a server that
 *  counts 2 of the session's replies unread and answers every PUT with `put`. Every PUT body is
 *  recorded in `sent`, in the order it was sent. */
function renderShownReplies(put: (body: UserAgentStateInput) => Promise<UserAgentState>) {
  let states: UserAgentStates = { [SESSION]: { unread_replies: 2 } };
  const sent: UserAgentStateInput[] = [];
  const getMyAgentState = spyOn(api, "getMyAgentState").mockImplementation(async () => states);
  const putAgentState = spyOn(api, "putAgentState").mockImplementation(async (_session, body) => {
    sent.push(body);
    return put(body);
  });
  // No backoff between a write's retries, so its final failure lands within the test.
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retryDelay: 0 }, queries: { retry: false } },
  });
  const hook = renderHook(
    ({ replies }: { replies: readonly Message[] }) => useMarkShownRepliesRead(SESSION, replies),
    {
      initialProps: { replies: [reply("a1")] },
      wrapper: ({ children }: { children: ReactNode }) =>
        createElement(QueryClientProvider, { client: queryClient }, children),
    }
  );
  return {
    hook,
    queryClient,
    sent,
    /** The session's unread count moves, as the refetch a `user_agent_state.updated` event
     *  causes brings it in, and every PUT the new count calls for is answered. A PUT the view
     *  sends for it is pending by the time `act` has run the view's effects, so waiting for no
     *  pending write waits for it. */
    async countBecomes(unread: number) {
      states = { [SESSION]: { unread_replies: unread } };
      await act(() => queryClient.invalidateQueries({ queryKey: userAgentStateQuery().queryKey }));
      await waitFor(() => expect(queryClient.isMutating()).toBe(0));
    },
    async restore() {
      await waitFor(() => expect(queryClient.isMutating()).toBe(0));
      hook.unmount();
      putAgentState.mockRestore();
      getMyAgentState.mockRestore();
    },
  };
}

// A reply that arrives while the page's mark is still retrying changes what the page shows, so
// the page sends a new mark that covers both, and the old one is superseded. When the old one then
// fails for good, the new mark still stands: forgetting it would send it once more at the next
// change in the count, a PUT the server answers by changing nothing.
test("a superseded mark that fails for good sends nothing more", async () => {
  const held = Promise.withResolvers<void>();
  const page = renderShownReplies(async (body) => {
    if (body.read_replies?.length === 1) {
      await held.promise;
      throw new Error("Dispatch is restarting");
    }
    // The session has another unread reply this page does not show, so the count stays up.
    return { unread_replies: 1 };
  });
  try {
    await waitFor(() => expect(page.sent).toEqual([{ read_replies: ["a1"] }]));
    page.hook.rerender({ replies: [reply("a1"), reply("a2")] });
    await waitFor(() => expect(page.sent).toHaveLength(2));
    // The covering mark is answered while the first is still on its first try.
    await waitFor(() => expect(page.queryClient.isMutating()).toBe(1));
    held.resolve();
    await waitFor(() => expect(page.queryClient.isMutating()).toBe(0));
    expect(page.sent).toEqual([
      { read_replies: ["a1"] },
      { read_replies: ["a1", "a2"] },
      { read_replies: ["a1"] },
      { read_replies: ["a1"] },
    ]);
    await page.countBecomes(3);
    expect(page.sent).toHaveLength(4);
  } finally {
    held.resolve();
    await page.restore();
  }
});

// The mark the page still stands by is another matter: when it fails for good its key is
// forgotten, so the next change in the count sends it again rather than leaving the badge up
// until the session replies once more.
test("the current mark that fails for good is sent again when the count changes", async () => {
  let refusals = 3;
  const page = renderShownReplies(async () => {
    refusals -= 1;
    if (refusals >= 0) throw new Error("Dispatch is restarting");
    return { unread_replies: 0 };
  });
  try {
    await waitFor(() => expect(page.sent).toHaveLength(3));
    await waitFor(() => expect(page.queryClient.isMutating()).toBe(0));
    await page.countBecomes(3);
    expect(page.sent).toEqual(Array(4).fill({ read_replies: ["a1"] }));
  } finally {
    await page.restore();
  }
});
