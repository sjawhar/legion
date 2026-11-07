import { expect, test } from "bun:test";

import type { Agent } from "../../api/types";
import { ACTIVE_WITHIN_MS, agentGroup, QUIET_AFTER_MS } from "./agent-groups";

const now = Date.parse("2026-09-14T12:00:00Z");

function minutesAgo(minutes: number): string {
  return new Date(now - minutes * 60_000).toISOString();
}

function session(overrides: Partial<Agent> = {}): Agent {
  return {
    capabilities: ["btw"],
    dir: "/workspaces/planner",
    host: "build-host",
    last_activity: null,
    last_seen: now,
    machine_id: "build-host",
    open_asks: 0,
    roles: [],
    session_id: "planner-session",
    title: "Planner",
    ...overrides,
  };
}

test("a session with an open ask waiting on the person is Needs you", () => {
  expect(agentGroup(session({ last_activity: minutesAgo(1) }), 1, 0, now)).toBe("needs-you");
});

test("a session with replies the person has not read is Needs you", () => {
  expect(agentGroup(session({ last_activity: minutesAgo(1) }), 0, 2, now)).toBe("needs-you");
});

test("Dispatch activity inside two minutes is Active, and at the boundary it still is", () => {
  expect(agentGroup(session({ last_activity: minutesAgo(1) }), 0, 0, now)).toBe("active");
  const atBound = session({ last_activity: new Date(now - ACTIVE_WITHIN_MS).toISOString() });
  expect(agentGroup(atBound, 0, 0, now)).toBe("active");
  // A hundredth of a second older is not: the microseconds the server writes decide it, which
  // is why the bound is compared as a timestamp and not parsed to a number.
  const justOlder = session({
    last_activity: new Date(now - ACTIVE_WITHIN_MS - 10).toISOString(),
  });
  expect(agentGroup(justOlder, 0, 0, now)).toBe("idle");
});

test("a session heard from inside ten minutes with older Dispatch activity is Idle", () => {
  const seenRecently = session({
    last_activity: minutesAgo(5),
    last_seen: now - 5 * 60_000,
  });
  expect(agentGroup(seenRecently, 0, 0, now)).toBe("idle");
});

test("a session Dispatch has never heard from is Idle, not Quiet", () => {
  expect(agentGroup(session({ last_activity: null }), 0, 0, now)).toBe("idle");
});

test("a session the listener has not heard from for ten minutes is Quiet", () => {
  const unseen = session({ last_activity: minutesAgo(30), last_seen: now - QUIET_AFTER_MS });
  expect(agentGroup(unseen, 0, 0, now)).toBe("quiet");
  const silentAndUnseen = session({ last_activity: null, last_seen: now - 60 * 60_000 });
  expect(agentGroup(silentAndUnseen, 0, 0, now)).toBe("quiet");
});

// D2: the four tests overlap and the first match wins, so the one question a person must answer
// never hides in the group of what they can ignore.
test("an open ask outranks silence: a session unheard for two hours with an ask is Needs you", () => {
  const silentForHours = session({ last_activity: null, last_seen: now - 2 * 60 * 60_000 });
  expect(agentGroup(silentForHours, 1, 0, now)).toBe("needs-you");
  expect(agentGroup(silentForHours, 0, 1, now)).toBe("needs-you");
  // Without either, the same row is Quiet.
  expect(agentGroup(silentForHours, 0, 0, now)).toBe("quiet");
});

// D3: the rule reads the row alone. The Agents page used to lift a pinned session into its open
// list whatever its age; nothing here can, because no pin reaches this function.
test("Dispatch activity outranks an old heartbeat: an hour unseen, a minute of activity is Active", () => {
  const spokeButUnseen = session({ last_activity: minutesAgo(1), last_seen: now - 60 * 60_000 });
  expect(agentGroup(spokeButUnseen, 0, 0, now)).toBe("active");
});

test("the clock defaults to now, so a caller with no clock of its own reads a live row", () => {
  const live = session({ last_activity: new Date().toISOString(), last_seen: Date.now() });
  expect(agentGroup(live, 0, 0)).toBe("active");
});
