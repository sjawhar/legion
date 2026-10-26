import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import { createElement, type ReactNode } from "react";

import { ApiError, api } from "../../api/client";
import { userAgentStateQuery } from "../../api/queries";
import type {
  Message,
  UserAgentState,
  UserAgentStateInput,
  UserAgentStates,
} from "../../api/types";
import { writesOf } from "../writes/write-store";
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
  const putAgentState = spyOn(api, "putAgentState").mockImplementation(
    async (session, body, bounds) => {
      // As `api.putAgentState` does, the request is told to `sent` before anything goes out: a
      // mark waiting its turn is planned there, and nothing past it runs (`Writes`).
      bounds.sent?.({ body, method: "PUT", path: `/api/v1/me/agents/${session}/state` });
      sent.push(body);
      return put(body);
    }
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const hook = renderHook(
    ({ replies }: { replies: readonly Message[] }) => useMarkShownRepliesRead(SESSION, replies),
    {
      initialProps: { replies: [reply("a1")] },
      wrapper: ({ children }: { children: ReactNode }) =>
        createElement(QueryClientProvider, { client: queryClient }, children),
    }
  );
  /** Waits until the store holds no mark still out. */
  const settled = () => waitFor(() => expect(writesOf(queryClient).rows()).toEqual([]));
  return {
    hook,
    queryClient,
    sent,
    settled,
    /** The session's unread count moves, as the refetch a `user_agent_state.updated` event
     *  causes brings it in, and every PUT the new count calls for is answered. */
    async countBecomes(unread: number) {
      states = { [SESSION]: { unread_replies: unread } };
      await act(() => queryClient.invalidateQueries({ queryKey: userAgentStateQuery().queryKey }));
      await settled();
    },
    async restore() {
      await settled();
      hook.unmount();
      putAgentState.mockRestore();
      getMyAgentState.mockRestore();
    },
  };
}

const refusal = new ApiError(400, { code: "STATE_INPUT", error: "refused" });

// A reply that arrives while the page's mark is still out changes what the page shows, so the page
// makes a new mark that covers both; the marks of one session go out in order, so the new one
// waits behind the old. When the old one is then refused, the new mark still stands: forgetting it
// would send it once more at the next change in the count, a PUT the server answers by changing
// nothing.
test("a superseded mark that is refused sends nothing more", async () => {
  const held = Promise.withResolvers<void>();
  const page = renderShownReplies(async (body) => {
    if (body.read_replies?.length === 1) {
      await held.promise;
      throw refusal;
    }
    // The session has another unread reply this page does not show, so the count stays up.
    return { unread_replies: 1 };
  });
  try {
    await waitFor(() => expect(page.sent).toEqual([{ read_replies: ["a1"] }]));
    page.hook.rerender({ replies: [reply("a1"), reply("a2")] });
    await waitFor(() => expect(writesOf(page.queryClient).rows()).toHaveLength(2));
    expect(page.sent).toHaveLength(1);
    held.resolve();
    await page.settled();
    expect(page.sent).toEqual([{ read_replies: ["a1"] }, { read_replies: ["a1", "a2"] }]);
    await page.countBecomes(3);
    expect(page.sent).toHaveLength(2);
  } finally {
    held.resolve();
    await page.restore();
  }
});

// The mark the page still stands by is another matter: when it is refused its key is forgotten, so
// the next change in the count sends it again rather than leaving the badge up until the session
// replies once more.
test("the current mark that is refused is sent again when the count changes", async () => {
  let refusals = 1;
  const page = renderShownReplies(async () => {
    refusals -= 1;
    if (refusals >= 0) throw refusal;
    return { unread_replies: 0 };
  });
  try {
    await waitFor(() => expect(page.sent).toHaveLength(1));
    await page.settled();
    await page.countBecomes(3);
    expect(page.sent).toEqual(Array(2).fill({ read_replies: ["a1"] }));
  } finally {
    await page.restore();
  }
});
