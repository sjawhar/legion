import { expect, test } from "bun:test";

import type { UserAgentStates } from "../../api/types";
import { totalUnreadReplies } from "./unread";

// An API older than the unread count answers a session's state with its Clear alone. The badge
// then counts that session as nothing unread rather than reading "New replies NaN".
test("the unread total counts a state with no unread count as none", () => {
  const states: UserAgentStates = {
    old: { cleared_before: "2026-09-27T20:00:00Z" },
    s1: { unread_replies: 2 },
  };
  expect(totalUnreadReplies(states)).toBe(2);
});
