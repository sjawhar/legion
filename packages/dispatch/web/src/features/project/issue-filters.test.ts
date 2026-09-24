import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import { createElement, type ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Agent, IssueClaim, IssueSummary } from "../../api/types";
import { projectIssuesQueryKey, useIssueFilters } from "./issue-filters";

function agent(sessionID: string): Agent {
  return {
    session_id: sessionID,
    title: "Implementer",
    dir: "/w",
    machine_id: "host",
    roles: [],
    capabilities: [],
    last_seen: 1,
    open_asks: 0,
    last_activity: null,
  };
}

function heldBy(actor: IssueClaim["actor"]): IssueSummary {
  return issue({ claim: { actor, at: "2026-09-24T06:00:00Z" } });
}

function issue(overrides: Partial<IssueSummary> = {}): IssueSummary {
  return {
    key: "CORE-1",
    labels: [],
    last_seq: 0,
    open_asks: 0,
    parent: null,
    assignee: null,
    claim: null,
    components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
    status: "todo",
    priority: null,
    rank: "U",
    title: "Core work",
    updated_at: "2026-09-10T00:00:00Z",
    ...overrides,
  };
}

function renderFilters(initialEntry = "/projects/CORE") {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderHook(() => useIssueFilters(), {
    wrapper: ({ children }: { children: ReactNode }) =>
      createElement(
        MemoryRouter,
        { initialEntries: [initialEntry] },
        createElement(QueryClientProvider, { client }, children)
      ),
  });
}

test("parses every filter from the URL and counts them", () => {
  const hook = renderFilters(
    "/projects/CORE?label=a&label=b&q=ship&needs-you=1&unread=1&status=todo&status=done"
  );
  try {
    expect(hook.result.current.labels).toEqual(["a", "b"]);
    // Status is the List's alone: parsed here, but the Board renders the strip without it, so
    // neither the count nor the chips include it.
    expect(hook.result.current.statuses).toEqual(["todo", "done"]);
    expect(hook.result.current.search).toBe("ship");
    expect(hook.result.current.needsYou).toBe(true);
    expect(hook.result.current.unread).toBe(true);
    expect(hook.result.current.activeFilterCount).toBe(5);
    expect(hook.result.current.activeFilters.map((filter) => filter.label)).toEqual([
      "Label: a",
      "Label: b",
      "Search: ship",
      "Needs you",
      "Unread",
    ]);
  } finally {
    hook.unmount();
  }
});

test("setters round-trip through the URL and clearing removes the parameter", () => {
  const hook = renderFilters();
  try {
    act(() => hook.result.current.setSearch("plan"));
    expect(hook.result.current.search).toBe("plan");
    act(() => hook.result.current.setNeedsYou(true));
    act(() => hook.result.current.setUnread(true));
    act(() => hook.result.current.setLabels(["frontend"]));
    act(() => hook.result.current.setStatuses(["todo", "testing"]));
    expect(hook.result.current.statuses).toEqual(["todo", "testing"]);
    act(() => hook.result.current.setStatuses([]));
    expect(hook.result.current.statuses).toEqual([]);
    expect(hook.result.current.activeFilterCount).toBe(4);
    // One chip per click, a render between them - as the strip drives it.
    while (hook.result.current.activeFilters.length > 0) {
      act(() => hook.result.current.activeFilters[0]?.remove());
    }
    expect(hook.result.current.activeFilterCount).toBe(0);
    expect(hook.result.current.search).toBe("");
    expect(hook.result.current.labels).toEqual([]);
  } finally {
    hook.unmount();
  }
});

test("matches applies needs-you, unread and search but never status", () => {
  const hook = renderFilters("/projects/CORE?q=ship&needs-you=1&unread=1");
  try {
    const { matches } = hook.result.current;
    const match = issue({ key: "CORE-2", last_seq: 4, open_asks: 1, title: "Ship the work" });
    expect(matches(match, 3)).toBe(true);
    // Status never enters the predicate: the List applies it separately, the Board's columns
    // are the statuses.
    expect(matches({ ...match, status: "done" }, 3)).toBe(true);
    expect(matches({ ...match, open_asks: 0 }, 3)).toBe(false);
    expect(matches(match, 4)).toBe(false);
    expect(matches({ ...match, key: "OTHER-1", title: "Quiet work" }, 3)).toBe(false);
    // The key matches the query too.
    expect(matches({ ...match, key: "SHIP-1", title: "Quiet work" }, 3)).toBe(true);
  } finally {
    hook.unmount();
  }
});

// An agent looking for work asks for the issues nobody is working. A claim held by a running
// session hides the issue; a claim whose session the registry no longer lists does not, because
// that is exactly the claim the server hands to the next agent that asks for it.
test("unclaimed keeps the issues nobody is working, a lapsed holder's included", async () => {
  const agents = spyOn(api, "listAgents").mockResolvedValue([agent("session-one")]);
  const hook = renderFilters("/projects/CORE?unclaimed=1");
  const running: IssueClaim["actor"] = { kind: "session", id: "session-one" };
  const ended: IssueClaim["actor"] = { kind: "session", id: "session-gone" };
  try {
    expect(hook.result.current.unclaimed).toBe(true);
    expect(hook.result.current.activeFilterCount).toBe(1);
    expect(hook.result.current.activeFilters.map((filter) => filter.label)).toEqual(["Unclaimed"]);
    expect(hook.result.current.matches(issue(), 0)).toBe(true);
    // Until the registry answers, every claim is somebody's: a list that has not loaded it must
    // not advertise held work as free.
    expect(hook.result.current.matches(heldBy(ended), 0)).toBe(false);
    await waitFor(() => expect(hook.result.current.matches(heldBy(ended), 0)).toBe(true));
    expect(hook.result.current.matches(heldBy(running), 0)).toBe(false);
    // A human's claim has no session to end, so it never lapses.
    expect(hook.result.current.matches(heldBy({ kind: "user", id: "alice" }), 0)).toBe(false);
    act(() => hook.result.current.setUnclaimed(false));
    expect(hook.result.current.matches(heldBy(running), 0)).toBe(true);
  } finally {
    hook.unmount();
    agents.mockRestore();
  }
});

test("the query key is the plain list without labels and the labelled list with them", () => {
  expect(projectIssuesQueryKey("CORE", [])).toEqual(["issues", "project", "CORE"]);
  expect(projectIssuesQueryKey("CORE", ["a"])).toEqual([
    "issues",
    "project",
    "CORE",
    "labels",
    ["a"],
  ]);
});
