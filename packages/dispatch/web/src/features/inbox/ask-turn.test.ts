import { expect, test } from "bun:test";

import { askTurnHolder } from "./ask-turn";

const asker = { id: "session-1", kind: "session" as const, origin: { session_title: "planner" } };

test("askTurnHolder says the human holds the turn", () => {
  expect(askTurnHolder({ author: asker, state: "open", waiting_on: "human" }, undefined)).toBe(
    "you"
  );
});

test("askTurnHolder names the agent holding the turn: the asker, or whoever posted the progress note", () => {
  expect(askTurnHolder({ author: asker, state: "open", waiting_on: "agent" }, undefined)).toBe(
    asker
  );
  expect(
    askTurnHolder(
      { author: asker, state: "open", waiting_on: "agent" },
      { author: { id: "alice", kind: "user" } }
    )
  ).toBe(asker);
  const auditor = {
    id: "session-2",
    kind: "session" as const,
    origin: { session_title: "auditor" },
  };
  expect(
    askTurnHolder({ author: asker, state: "open", waiting_on: "agent" }, { author: auditor })
  ).toBe(auditor);
});

test("askTurnHolder is silent on closed asks and reads that did not carry waiting_on", () => {
  expect(askTurnHolder({ author: asker, state: "answered", waiting_on: "human" }, undefined)).toBe(
    null
  );
  expect(askTurnHolder({ author: asker, state: "open" }, undefined)).toBe(null);
});
