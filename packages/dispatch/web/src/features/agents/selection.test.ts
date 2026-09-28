import { expect, test } from "bun:test";

import type { Agent } from "../../api/types";
import { matchingSelection, selectionSummary, toggleMatching } from "./selection";

function agent(sessionID: string): Agent {
  return {
    capabilities: ["btw"],
    dir: `/workspaces/${sessionID}`,
    last_activity: null,
    last_seen: 0,
    machine_id: "build-host",
    open_asks: 0,
    roles: [],
    session_id: sessionID,
    title: sessionID,
  };
}

const matching = [agent("planner"), agent("reviewer")];

function summary(selected: string[]): string {
  return selectionSummary(matching.length, matchingSelection(matching, new Set(selected)));
}

test("the header state reads none, some or all of the matching agents, never those outside the filter", () => {
  expect(matchingSelection(matching, new Set()).state).toBe("none");
  // A selection outside the filter cannot make the header mixed.
  expect(matchingSelection(matching, new Set(["hidden"])).state).toBe("none");
  expect(matchingSelection(matching, new Set(["planner", "hidden"])).state).toBe("some");
  expect(matchingSelection(matching, new Set(["planner", "reviewer"])).state).toBe("all");
  expect(matchingSelection([], new Set(["planner"])).state).toBe("none");
});

test("the header count scopes the selected count to the matching rows and names the selection outside the filter", () => {
  // Nothing selected anywhere.
  expect(summary([])).toBe("2 matching");
  // Some or all of the matching rows, and nothing else.
  expect(summary(["planner"])).toBe("1 of 2 matching selected");
  expect(summary(["planner", "reviewer"])).toBe("2 of 2 matching selected");
  // Matching rows selected, plus selected rows outside the filter.
  expect(summary(["planner", "hidden-a", "hidden-b"])).toBe(
    "1 of 2 matching selected · 2 more selected outside the filter"
  );
  // A selection wholly outside the filter never reads like an empty one.
  expect(summary(["hidden-a", "hidden-b"])).toBe("2 matching · 2 selected outside the filter");
});

test("the header toggle adds every matching agent unless all are selected, then removes them, and leaves the selection outside the filter alone", () => {
  const all = toggleMatching(matching, new Set(["planner", "hidden"]));
  expect([...all].sort()).toEqual(["hidden", "planner", "reviewer"]);
  expect([...toggleMatching(matching, all)]).toEqual(["hidden"]);
});
