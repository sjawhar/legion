import { expect, test } from "bun:test";

import type { Agent } from "../../api/types";
import { agentGroup } from "./agent-groups";

const NOW = 1_762_000_000_000;

function session(overrides: Partial<Agent> & { session_id: string }): Agent {
  return {
    capabilities: [],
    dir: "/src/legion",
    host: "devbox",
    last_activity: null,
    last_seen: NOW - 30_000,
    machine_id: "listener",
    open_asks: 0,
    roles: [],
    title: overrides.session_id,
    ...overrides,
  };
}

function minutesAgo(minutes: number): string {
  return new Date(NOW - minutes * 60_000).toISOString();
}

test("a session with an open ask whose turn is the viewer's needs them", () => {
  const agent = session({ last_activity: minutesAgo(1), session_id: "asked" });

  expect(agentGroup(agent, 1, 0, NOW)).toBe("needs-you");
});

test("a session whose replies the viewer has not read needs them", () => {
  const agent = session({ session_id: "replied" });

  expect(agentGroup(agent, 0, 2, NOW)).toBe("needs-you");
});

test("a session that acted inside two minutes is active", () => {
  const agent = session({ last_activity: minutesAgo(1), session_id: "working" });

  expect(agentGroup(agent, 0, 0, NOW)).toBe("active");
});

test("a session heard from inside ten minutes that acted longer ago is idle", () => {
  const agent = session({
    last_activity: minutesAgo(5),
    last_seen: NOW - 60_000,
    session_id: "waiting",
  });

  expect(agentGroup(agent, 0, 0, NOW)).toBe("idle");
});

test("a session Dispatch has never heard from is idle while the registry still sees it", () => {
  const agent = session({ last_activity: null, last_seen: NOW - 9 * 60_000, session_id: "silent" });

  expect(agentGroup(agent, 0, 0, NOW)).toBe("idle");
});

test("a session the registry has not seen for ten minutes is quiet", () => {
  const agent = session({ last_seen: NOW - 11 * 60_000, session_id: "gone-quiet" });

  expect(agentGroup(agent, 0, 0, NOW)).toBe("quiet");
});

test("an open ask wins over every other test: a session silent for two hours still needs you", () => {
  const agent = session({
    last_activity: minutesAgo(120),
    last_seen: NOW - 120 * 60_000,
    session_id: "owed",
  });

  expect(agentGroup(agent, 1, 0, NOW)).toBe("needs-you");
});

test("activity wins over silence: a session that acted a minute ago is active, unseen or not", () => {
  const agent = session({
    last_activity: minutesAgo(1),
    last_seen: NOW - 60 * 60_000,
    session_id: "acted-unseen",
  });

  expect(agentGroup(agent, 0, 0, NOW)).toBe("active");
});

test("the two bounds are the ages themselves: two minutes is still active, ten still quiet", () => {
  const acted = session({ last_activity: minutesAgo(2), session_id: "two-minutes" });
  const unseen = session({ last_seen: NOW - 10 * 60_000, session_id: "ten-minutes" });

  expect(agentGroup(acted, 0, 0, NOW)).toBe("active");
  expect(agentGroup(unseen, 0, 0, NOW)).toBe("quiet");
});

test("an activity timestamp Dispatch did not write counts as no activity, never as a throw", () => {
  const agent = session({ last_activity: "yesterday", last_seen: NOW - 60_000, session_id: "odd" });

  expect(agentGroup(agent, 0, 0, NOW)).toBe("idle");
});
