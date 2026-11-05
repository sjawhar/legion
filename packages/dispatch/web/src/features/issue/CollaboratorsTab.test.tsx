import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Agent, IssueCollaboratorAgent, IssueCollaborators } from "../../api/types";
import { CollaboratorsTab, collaboratorSections } from "./CollaboratorsTab";

function legionAgent(
  sessionId: string,
  role: "implementer" | "planner" | "reviewer" | "tester",
  startedAt: string,
  replacedBy: string | null = null
): IssueCollaboratorAgent {
  return {
    kind: "session",
    last_at: null,
    legion: {
      replaced_at: replacedBy === null ? null : "2026-10-26T05:00:00Z",
      replaced_by: replacedBy,
      role,
      role_token: `legion-p-core-1-${role}`,
      started_at: startedAt,
    },
    reasons: [],
    session_id: sessionId,
    title: `Legion ${role} · ${sessionId}`,
  };
}

function otherAgent(sessionId: string, lastAt: string | null): IssueCollaboratorAgent {
  return {
    kind: "session",
    last_at: lastAt,
    legion: null,
    reasons: ["posted"],
    session_id: sessionId,
    title: sessionId,
  };
}

function live(sessionId: string): Agent {
  return {
    capabilities: ["steer"],
    dir: "/w",
    host: "host",
    last_activity: null,
    last_seen: Date.now(),
    machine_id: "host",
    open_asks: 0,
    roles: [],
    session_id: sessionId,
    title: sessionId,
  };
}

function renderTab(collaborators: IssueCollaborators, agents: readonly Agent[] = []) {
  const read = spyOn(api, "getIssueCollaborators").mockResolvedValue(collaborators);
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([...agents]);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <CollaboratorsTab issueKey="CORE-1" />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    read,
    view,
    restore: () => {
      view.unmount();
      read.mockRestore();
      listAgents.mockRestore();
    },
  };
}

function sessionsIn(container: HTMLElement): string[] {
  return [...container.querySelectorAll("li > div:first-child [data-agent-name]")].map(
    (node) => node.getAttribute("data-agent-name") ?? ""
  );
}

// Legion's agents by role in workflow order, each role's newest first with a replaced one naming
// its successor; then every other agent by its last activity; then people with their reasons.
test("the Collaborators tab orders Legion agents by role, other agents by activity, and people as sent", async () => {
  const tab = renderTab(
    {
      agents: [
        // The server's order is not the tab's: reviewer before planner, oldest reviewer first.
        legionAgent("old-reviewer", "reviewer", "2026-10-26T01:00:00Z", "new-reviewer"),
        legionAgent("new-reviewer", "reviewer", "2026-10-26T05:00:00Z"),
        legionAgent("tester", "tester", "2026-10-26T03:00:00Z"),
        legionAgent("planner", "planner", "2026-10-26T00:00:00Z"),
        otherAgent("quiet", null),
        otherAgent("older", "2026-10-26T01:00:00Z"),
        otherAgent("newer", "2026-10-26T02:00:00Z"),
      ],
      issue_key: "CORE-1",
      legion_registry: "ok",
      people: [
        { kind: "user", last_at: "2026-10-26T02:00:00Z", login: "bob", reasons: ["posted"] },
        { kind: "user", last_at: null, login: "alice", reasons: ["assignee", "claimed"] },
      ],
    },
    [live("tester")]
  );

  try {
    const legion = await screen.findByRole("region", { name: "Legion agents" });
    expect(
      within(legion)
        .getAllByRole("heading", { level: 3 })
        .map((heading) => heading.textContent)
    ).toEqual(["planner", "tester", "reviewer"]);
    expect(sessionsIn(legion)).toEqual(["planner", "tester", "new-reviewer", "old-reviewer"]);
    const replaced = legion.querySelector('[data-replaced-by="new-reviewer"]');
    expect(replaced?.textContent).toBe("replaced by Legion reviewer · new-reviewer");
    expect(replaced?.querySelector("a")?.getAttribute("href")).toBe("/agents/new-reviewer/live");
    // Each agent is the live link, with the registry's state: the tester runs, the rest do not.
    expect(
      within(legion)
        .getByRole("link", { name: "Legion reviewer · old-reviewer" })
        .getAttribute("href")
    ).toBe("/agents/old-reviewer/live");
    await waitFor(() =>
      expect(
        legion.querySelector('[data-agent-name="tester"]')?.getAttribute("data-agent-state")
      ).toBe("running")
    );
    expect(
      legion
        .querySelector('li > div:first-child [data-agent-name="old-reviewer"]')
        ?.getAttribute("data-agent-state")
    ).toBe("not-running");

    const others = screen.getByRole("region", { name: "Other agents" });
    expect(sessionsIn(others)).toEqual(["newer", "older", "quiet"]);

    const people = screen.getByRole("region", { name: "People" });
    expect(
      within(people)
        .getAllByRole("listitem")
        .map((row) => row.textContent)
    ).toEqual([expect.stringContaining("bobposted"), "aliceassignee, claimed"]);
    expect(within(people).queryByRole("link")).toBeNull();
    expect(screen.queryByText("No one has worked this issue yet.")).toBeNull();
  } finally {
    tab.restore();
  }
});

test("the Collaborators tab says when Legion's record could not be read, and when nobody worked the issue", async () => {
  const unavailable = renderTab({
    agents: [otherAgent("writer", "2026-10-26T01:00:00Z")],
    issue_key: "CORE-1",
    legion_registry: "unavailable",
    people: [],
  });
  try {
    await screen.findByText("Legion's record of this issue's agents could not be read.");
    expect(screen.queryByRole("region", { name: "Legion agents" })).toBeNull();
    expect(sessionsIn(screen.getByRole("region", { name: "Other agents" }))).toEqual(["writer"]);
  } finally {
    unavailable.restore();
  }

  const empty = renderTab({ agents: [], issue_key: "CORE-1", legion_registry: "ok", people: [] });
  try {
    await screen.findByText("No one has worked this issue yet.");
    expect(
      screen.queryByText("Legion's record of this issue's agents could not be read.")
    ).toBeNull();
    expect(empty.read).toHaveBeenCalledWith("CORE-1");
  } finally {
    empty.restore();
  }
});

// The ordering rules themselves, without a DOM: a role nobody worked is left out, and a session
// with no activity at all sorts after every session that has some.
test("collaboratorSections orders the three sections the tab draws", () => {
  const sections = collaboratorSections({
    agents: [
      legionAgent("tester", "tester", "2026-10-26T03:00:00Z"),
      legionAgent("old-planner", "planner", "2026-10-26T00:00:00Z", "new-planner"),
      legionAgent("new-planner", "planner", "2026-10-26T02:00:00Z"),
      otherAgent("quiet", null),
      otherAgent("loud", "2026-10-26T04:00:00Z"),
    ],
    issue_key: "CORE-1",
    legion_registry: "ok",
    people: [{ kind: "user", last_at: null, login: "alice", reasons: ["assignee"] }],
  });

  expect(
    sections.legion.map(({ agents, role }) => [role, agents.map((agent) => agent.session_id)])
  ).toEqual([
    ["planner", ["new-planner", "old-planner"]],
    ["tester", ["tester"]],
  ]);
  expect(sections.others.map((agent) => agent.session_id)).toEqual(["loud", "quiet"]);
  expect(sections.people.map((person) => person.login)).toEqual(["alice"]);
});
