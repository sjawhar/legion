import { expect, test } from "bun:test";

import { askTurnLabel } from "./ask-turn";

const asker = { id: "session-1", kind: "session" as const, origin: { session_title: "planner" } };

test("askTurnLabel says the human holds the turn", () => {
  expect(askTurnLabel({ author: asker, state: "open", waiting_on: "human" }, undefined)).toBe(
    "Waiting on you"
  );
});

test("askTurnLabel names the agent holding the turn: the asker, or whoever posted the progress note", () => {
  expect(askTurnLabel({ author: asker, state: "open", waiting_on: "agent" }, undefined)).toBe(
    "Waiting on planner"
  );
  expect(
    askTurnLabel(
      { author: asker, state: "open", waiting_on: "agent" },
      { author: { id: "alice", kind: "user" } }
    )
  ).toBe("Waiting on planner");
  expect(
    askTurnLabel(
      { author: asker, state: "open", waiting_on: "agent" },
      { author: { id: "session-2", kind: "session", origin: { session_title: "auditor" } } }
    )
  ).toBe("Waiting on auditor");
});

test("askTurnLabel is silent on closed asks and reads that did not carry waiting_on", () => {
  expect(askTurnLabel({ author: asker, state: "answered", waiting_on: "human" }, undefined)).toBe(
    null
  );
  expect(askTurnLabel({ author: asker, state: "open" }, undefined)).toBe(null);
});
