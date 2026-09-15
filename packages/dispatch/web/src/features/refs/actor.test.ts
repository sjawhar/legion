import { expect, test } from "bun:test";

import { actorLabel, sessionLabel, shortSessionId } from "./actor";

const titles = new Map([["abcdef1234567890", "Planner"]]);

test("a session is labelled by its live registry title, else its stamped title, else its short id", () => {
  expect(
    actorLabel(
      { id: "abcdef1234567890", kind: "session", origin: { session_title: "old title" } },
      titles
    )
  ).toBe("Planner");
  expect(
    actorLabel({ id: "zzz", kind: "session", origin: { session_title: "Fix Margin Log" } }, titles)
  ).toBe("Fix Margin Log");
  expect(actorLabel({ id: "0123456789abcdef", kind: "session" }, titles)).toBe("session:01234567…");
  expect(
    actorLabel({ id: "0123456789abcdef", kind: "session", origin: { session_title: "  " } })
  ).toBe(shortSessionId("0123456789abcdef"));
  expect(actorLabel({ id: "alice", kind: "user" }, titles)).toBe("alice");
});

test("a personally attributed session keeps its human owner after the same label", () => {
  expect(
    actorLabel({
      id: "architect-session",
      kind: "session",
      origin: { session_title: "Architect" },
      owner: "alice",
    })
  ).toBe("Architect (for alice)");
  expect(
    actorLabel(
      {
        id: "abcdef1234567890",
        kind: "session",
        origin: { session_title: "old title" },
        owner: "alice",
      },
      titles
    )
  ).toBe("Planner (for alice)");
  expect(actorLabel({ id: "0123456789abcdef", kind: "session", owner: "bob" })).toBe(
    "session:01234567… (for bob)"
  );
});

test("sessionLabel and actorLabel agree for the same session id and title", () => {
  expect(sessionLabel("0123456789abcdef", "Reviewer")).toBe("Reviewer");
  expect(sessionLabel("0123456789abcdef", "")).toBe("session:01234567…");
  expect(sessionLabel("0123456789abcdef", undefined)).toBe(
    actorLabel({ id: "0123456789abcdef", kind: "session" })
  );
  expect(sessionLabel("abcdef1234567890", "Planner")).toBe(
    actorLabel({ id: "abcdef1234567890", kind: "session" }, titles)
  );
});
