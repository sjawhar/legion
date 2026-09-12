import { expect, test } from "bun:test";

import { resolveAuthor } from "./authors";

test("session authors prefer the live registry title, then the stamped title, then a short id", () => {
  const titles = new Map([["abcdef1234567890", "Planner"]]);

  expect(
    resolveAuthor(
      { id: "abcdef1234567890", kind: "session", origin: { session_title: "old title" } },
      titles
    )
  ).toEqual({ initials: "P", label: "Planner", shape: "square" });
  expect(
    resolveAuthor(
      { id: "zzz", kind: "session", origin: { session_title: "Fix Margin Log" } },
      titles
    ).label
  ).toBe("Fix Margin Log");
  expect(
    resolveAuthor(
      { id: "zzz", kind: "session", origin: { session_title: "Fix Margin Log" } },
      titles
    ).initials
  ).toBe("FM");
  expect(resolveAuthor({ id: "0123456789abcdef", kind: "session" }, titles)).toEqual({
    initials: "S",
    label: "session:01234567…",
    shape: "square",
  });
  expect(
    resolveAuthor(
      {
        id: "abcdef1234567890",
        kind: "session",
        origin: { session_title: "old title" },
        owner: "alice",
      },
      titles
    )
  ).toEqual({ initials: "P", label: "Planner (for alice)", shape: "square" });
  expect(resolveAuthor({ id: "alice", kind: "user" }, titles)).toEqual({
    initials: "A",
    label: "alice",
    shape: "round",
  });
});
