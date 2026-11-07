import { expect, test } from "bun:test";

import type { UserAgentStates } from "../../api/types";
import { legacyPinnedIds, pinnedAgentSessions } from "./pins";

// The rail lists the person's pins oldest first (INFRA-812, R1), so the agent they pinned first
// keeps the top row however their agent state is keyed, and a session they have only read or
// cleared is not in the rail at all.
test("the pins are the sessions with a pinned_at, oldest pin first", () => {
  const states: UserAgentStates = {
    "tester-session": { pinned_at: "2026-09-14T00:00:03Z" },
    "planner-session": { pinned_at: "2026-09-14T00:00:01Z", unread_replies: 2 },
    "reviewer-session": { pinned_at: "2026-09-14T00:00:02Z" },
    "quiet-session": { read_through: "2026-09-14T00:00:00Z", unread_replies: 1 },
  };

  expect(pinnedAgentSessions(states)).toEqual([
    "planner-session",
    "reviewer-session",
    "tester-session",
  ]);
});

// The server writes RFC 3339 with its trailing fractional zeros dropped, which does not sort as
// text (`lib/timestamps.ts`): a pin made at `…00.12Z` is older than one at `…00.123456Z`.
test("two pins in the same second are ordered by their fractions, not as text", () => {
  const states: UserAgentStates = {
    "second-session": { pinned_at: "2026-09-14T00:00:00.123456Z" },
    "first-session": { pinned_at: "2026-09-14T00:00:00.12Z" },
  };

  expect(pinnedAgentSessions(states)).toEqual(["first-session", "second-session"]);
});

test("nothing is pinned when the viewer has no agent state yet", () => {
  expect(pinnedAgentSessions(undefined)).toEqual([]);
  expect(pinnedAgentSessions({})).toEqual([]);
});

// A `pinned_at` this page cannot read orders nothing, and throwing on it would take the whole
// rail - every other pin with it - off the page.
test("a pin whose time cannot be read is listed last, not thrown on", () => {
  const states: UserAgentStates = {
    "broken-session": { pinned_at: "yesterday" },
    "planner-session": { pinned_at: "2026-09-14T00:00:01Z" },
  };

  expect(pinnedAgentSessions(states)).toEqual(["planner-session", "broken-session"]);
});

// What the shell migrates once: the ids this browser kept under the old preference key.
test("the old key's session ids are read in the order it kept them", () => {
  expect(legacyPinnedIds(JSON.stringify(["planner-session", "reviewer-session"]))).toEqual([
    "planner-session",
    "reviewer-session",
  ]);
});

test("an old key holding no list of ids carries no pins", () => {
  expect(legacyPinnedIds(null)).toEqual([]);
  expect(legacyPinnedIds("")).toEqual([]);
  expect(legacyPinnedIds("not json")).toEqual([]);
  expect(legacyPinnedIds(JSON.stringify({ "planner-session": true }))).toEqual([]);
  expect(legacyPinnedIds(JSON.stringify(["planner-session", 7, null]))).toEqual([
    "planner-session",
  ]);
});
