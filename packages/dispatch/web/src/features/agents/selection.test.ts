import { expect, test } from "bun:test";

import type { Agent } from "../../api/types";
import { foldLabel, matchingSelection, selectionSummary, toggleMatching } from "./selection";

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

// Two sessions the filter matches and one it hides; "gone" is selected but no longer listed.
const matching = [agent("planner"), agent("reviewer")];
const listed = [...matching, agent("tester")];

function summary(selected: string[], rows: readonly Agent[] = listed): string {
  return selectionSummary(matchingSelection(rows, matching, new Set(selected)));
}

test("the header state reads none, some or all of the matching agents, never the rest of the selection", () => {
  const state = (selected: string[]) =>
    matchingSelection(listed, matching, new Set(selected)).state;
  expect(state([])).toBe("none");
  // A selection the filter hides, or one gone from the list, cannot make the header mixed.
  expect(state(["tester", "gone"])).toBe("none");
  expect(state(["planner", "tester"])).toBe("some");
  expect(state(["planner", "reviewer"])).toBe("all");
  expect(matchingSelection(listed, [], new Set(["planner"])).state).toBe("none");
});

test("the header count scopes the selected count to the matching rows and names every other selected session by why it is not matching", () => {
  // Nothing selected anywhere.
  expect(summary([])).toBe("2 matching");
  // Some or all of the matching rows, and nothing else.
  expect(summary(["planner"])).toBe("1 of 2 matching selected");
  expect(summary(["planner", "reviewer"])).toBe("2 of 2 matching selected");
  // Matching rows selected, plus a selected row the filter hides.
  expect(summary(["planner", "tester"])).toBe(
    "1 of 2 matching selected · 1 more selected outside the filter"
  );
  // A selection wholly outside the filter never reads like an empty one.
  expect(summary(["tester"])).toBe("2 matching · 1 selected outside the filter");
  // No filter set: every listed row matches, and a selected session that left the registry is
  // no longer listed, not outside a filter.
  expect(summary(["planner", "reviewer", "gone"], matching)).toBe(
    "2 of 2 matching selected · 1 more selected, no longer listed"
  );
  expect(summary(["gone"], matching)).toBe("2 matching · 1 selected, no longer listed");
  // Both kinds at once name both.
  expect(summary(["planner", "tester", "gone"])).toBe(
    "1 of 2 matching selected · 1 more selected outside the filter · 1 more selected, no longer listed"
  );
});

test("a folded section's label counts its rows and, once any is selected, how many", () => {
  expect(foldLabel("Inactive", matching, new Set(["tester"]))).toBe("Inactive (2)");
  expect(foldLabel("Inactive", matching, new Set(["reviewer", "tester"]))).toBe(
    "Inactive (2, 1 selected)"
  );
});

test("the header toggle adds every matching agent unless all are selected, then removes them, and leaves the rest of the selection alone", () => {
  const all = toggleMatching(matching, new Set(["planner", "tester"]));
  expect([...all].sort()).toEqual(["planner", "reviewer", "tester"]);
  expect([...toggleMatching(matching, all)]).toEqual(["tester"]);
});
