import { afterEach, expect, type Mock, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation, useNavigate } from "react-router-dom";

import { answerAgentStates } from "../../__tests__/send-answers";
import { api } from "../../api/client";
import { whoAmIQuery } from "../../api/queries";
import type { Agent, UserAgentStates } from "../../api/types";
import { liveViewsOf } from "../agent-view/live-views";
import { Sidebar } from "./Sidebar";

/**
 * The rail's Agents list (INFRA-812, R1): the heading that still opens the Agents page, the
 * person's pinned agents under it, oldest pin first, and the controls that fold the list and
 * unpin a row.
 */

const PLANNER = "planner-session";
const REVIEWER = "reviewer-session";
const TESTER = "tester-session";

function agent(sessionId: string, title: string): Agent {
  return {
    capabilities: ["aside"],
    dir: "/workspaces/legion",
    host: "build-host",
    last_activity: null,
    last_seen: Date.now(),
    machine_id: "build-host",
    open_asks: 0,
    roles: [],
    session_id: sessionId,
    title,
  };
}

const spies: { mockRestore: () => void }[] = [];

afterEach(() => {
  cleanup();
  for (const spy of spies.splice(0)) spy.mockRestore();
});

/** The address the rail moved to, and the Back the browser's own would do. */
function Probe(): ReactNode {
  const location = useLocation();
  const navigate = useNavigate();
  return (
    <div>
      <span data-testid="address">{location.pathname}</span>
      <button onClick={() => navigate(-1)} type="button">
        Back
      </button>
    </div>
  );
}

/** The rail, signed in as alice, with `agentState` her per-agent state and `listed` the sessions
 *  Dispatch lists. `entries` is the history it is rendered over, newest last. */
function renderRail({
  agentState = {},
  entries = ["/"],
  listed = [agent(PLANNER, "Planner"), agent(REVIEWER, "Reviewer"), agent(TESTER, "Tester")],
}: {
  agentState?: UserAgentStates;
  entries?: string[];
  listed?: Agent[];
} = {}): { putAgentState: Mock<typeof api.putAgentState>; queryClient: QueryClient } {
  spies.push(
    spyOn(api, "whoAmI").mockResolvedValue({ event_head: "0", kind: "user", login: "alice" }),
    spyOn(api, "getInbox").mockResolvedValue([]),
    spyOn(api, "getCredentialPending").mockResolvedValue(null),
    spyOn(api, "listIssues").mockResolvedValue([]),
    spyOn(api, "listProjects").mockResolvedValue([]),
    spyOn(api, "listAgents").mockResolvedValue(listed),
    spyOn(api, "getMyAgentState").mockResolvedValue(agentState)
  );
  // Dispatch answers a state write with the session's whole state, `pinned_at` gone once the
  // viewer unpins it.
  const putAgentState = answerAgentStates().mockImplementation(async (session, input) => {
    const { pinned_at: held, ...rest } = agentState[session] ?? {};
    return input.pinned === true ? { ...rest, pinned_at: "2026-09-14T12:00:00Z" } : rest;
  });
  spies.push(putAgentState);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(whoAmIQuery().queryKey, {
    event_head: "0",
    kind: "user",
    login: "alice",
  });
  render(
    <MemoryRouter initialEntries={entries} initialIndex={entries.length - 1}>
      <QueryClientProvider client={queryClient}>
        <Sidebar user={{ event_head: "0", kind: "user", login: "alice" }} />
        <Probe />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { putAgentState, queryClient };
}

/** The rows under the Agents heading, as the rail draws them. */
function pinnedRows(): HTMLElement[] {
  const list = document.getElementById("rail-pinned-agents");
  if (list === null) throw new Error("the rail has no pinned-agents list");
  return [...list.querySelectorAll("li")];
}

/** Each pinned row's text, in the order the rail draws them. */
function rowTexts(): (string | null)[] {
  return pinnedRows().map((row) => row.textContent);
}

const pins: UserAgentStates = {
  // Keyed in neither pin order nor name order: the rail orders by `pinned_at`.
  [TESTER]: { pinned_at: "2026-09-14T00:00:03Z" },
  [PLANNER]: { pinned_at: "2026-09-14T00:00:01Z", unread_replies: 2 },
  [REVIEWER]: { pinned_at: "2026-09-14T00:00:02Z" },
};

// Exactly the person's pins, in the order they pinned them, each carrying what R1 asks of a row:
// its running state, its name and its unread count. A session Dispatch lists but they have not
// pinned is in no rail row at all.
test("the rail lists the pinned agents, oldest pin first, with their state and unread count", async () => {
  const unpinned = agent("other-session", "Nobody pinned me");
  renderRail({
    agentState: { ...pins, "other-session": { unread_replies: 5 } },
    listed: [
      agent(PLANNER, "Planner"),
      agent(REVIEWER, "Reviewer"),
      agent(TESTER, "Tester"),
      unpinned,
    ],
  });

  await screen.findByRole("link", { name: "Planner" });
  await waitFor(() => expect(rowTexts()).toHaveLength(3));
  expect(rowTexts()).toEqual(["Planner2asleep", "Reviewerasleep", "Testerasleep"]);
  expect(screen.queryByRole("link", { name: "Nobody pinned me" })).toBeNull();
  const rows = pinnedRows();
  expect(
    within(rows[0] as HTMLElement)
      .getByRole("img")
      .getAttribute("aria-label")
  ).toBe("Running");
});

// The row's state mark is the registry's answer, not a pin's: a session the person pinned that
// is no longer running says so rather than reading as live.
test("a pinned session Dispatch no longer lists reads as not running", async () => {
  renderRail({
    agentState: { [PLANNER]: { pinned_at: "2026-09-14T00:00:01Z" } },
    listed: [agent(REVIEWER, "Reviewer")],
  });

  await waitFor(() => expect(rowTexts()).toHaveLength(1));
  const rows = pinnedRows();
  await waitFor(() =>
    expect(
      within(rows[0] as HTMLElement)
        .getByRole("img")
        .getAttribute("aria-label")
    ).toBe("Not running")
  );
});

// The word each row shows is the registry's (`live-views.ts`): the view in front is live, the
// one left lingers, and the rail computes none of it.
test("each row says what its live view is doing", async () => {
  const { queryClient } = renderRail({ agentState: pins });

  await waitFor(() => expect(rowTexts()).toHaveLength(3));
  const views = liveViewsOf(queryClient);
  views.show(PLANNER);
  views.show(REVIEWER);
  await waitFor(() => expect(rowTexts()[1]).toBe("Reviewerlive"));
  expect(rowTexts()[0]).toBe("Planner2live 30s");
  expect(rowTexts()[2]).toBe("Testerasleep");
});

// A person who has pinned nothing is told where the pin is, rather than left with a heading over
// nothing.
test("the rail says where the pin is when nothing is pinned", async () => {
  renderRail();

  expect(
    await screen.findByText(
      "Pin an agent on the Agents page, or in its live view, to keep it here."
    )
  ).toBeTruthy();
  expect(document.getElementById("rail-pinned-agents")?.tagName).toBe("P");
});

// The heading is still the link to the Agents page; the chevron beside it folds the rows.
test("the Agents heading folds its rows and still opens the Agents page", async () => {
  renderRail({ agentState: pins });

  await waitFor(() => expect(rowTexts()).toHaveLength(3));
  // The heading keeps its unread pill beside its name, so it reads `Agents New replies 2`.
  expect(screen.getByRole("link", { name: "Agents New replies 2" }).getAttribute("href")).toBe(
    "/agents"
  );
  const fold = screen.getByRole("button", { name: "Hide pinned agents" });
  expect(fold.getAttribute("aria-expanded")).toBe("true");
  expect(fold.getAttribute("aria-controls")).toBe("rail-pinned-agents");

  fireEvent.click(fold);
  expect(document.getElementById("rail-pinned-agents")).toBeNull();
  expect(screen.getByRole("link", { name: "Agents New replies 2" }).getAttribute("href")).toBe(
    "/agents"
  );

  fireEvent.click(screen.getByRole("button", { name: "Show pinned agents" }));
  expect(rowTexts()).toHaveLength(3);
});

// Unpinning from the rail writes the person's state, as the Agents row and the live view do, and
// the row goes with the pin.
test("a rail row unpins its agent", async () => {
  const { putAgentState } = renderRail({ agentState: pins });

  await waitFor(() => expect(rowTexts()).toHaveLength(3));
  fireEvent.click(screen.getByRole("button", { name: "Unpin Reviewer from the sidebar" }));

  await waitFor(() => expect(rowTexts()).toEqual(["Planner2asleep", "Testerasleep"]));
  expect(putAgentState.mock.calls.map(([session, input]) => [session, input])).toEqual([
    [REVIEWER, { pinned: false }],
  ]);
});

// Moving between pinned agents from the rail is one step of the reader's: Back leaves the live
// views for the page they came from, rather than walking back through every agent they looked
// at. A rail row followed from another page pushes, so Back returns to that page.
test("a move between pinned agents from the rail replaces the address, and one from another page pushes", async () => {
  renderRail({ agentState: pins, entries: ["/", `/agents/${PLANNER}/live`] });

  await waitFor(() => expect(rowTexts()).toHaveLength(3));
  fireEvent.click(screen.getByRole("link", { name: "Reviewer" }));
  expect(screen.getByTestId("address").textContent).toBe(`/agents/${REVIEWER}/live`);

  fireEvent.click(screen.getByRole("button", { name: "Back" }));
  expect(screen.getByTestId("address").textContent).toBe("/");

  fireEvent.click(screen.getByRole("link", { name: "Tester" }));
  expect(screen.getByTestId("address").textContent).toBe(`/agents/${TESTER}/live`);
  fireEvent.click(screen.getByRole("button", { name: "Back" }));
  expect(screen.getByTestId("address").textContent).toBe("/");
});
