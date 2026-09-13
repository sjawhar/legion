import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Agent, InboxRow } from "../../api/types";
import { AuthGate } from "../../app";

const now = Date.now();
const agents = [
  {
    capabilities: ["aside", "btw"],
    dir: "/workspaces/planner",
    last_activity: new Date(now - 60_000).toISOString(),
    last_seen: now - 30_000,
    machine_id: "build-host",
    open_asks: 2,
    roles: ["planner"],
    session_id: "planner-session",
    title: "Planner",
  },
  {
    capabilities: ["aside"],
    dir: "/workspaces/reviewer",
    last_activity: null,
    last_seen: now - 5 * 60_000,
    machine_id: "review-host",
    open_asks: 0,
    roles: [],
    session_id: "reviewer-session",
    title: "Reviewer",
  },
] satisfies Agent[];

const inbox: InboxRow[] = [1, 2].map((number) => ({
  anchor: null,
  answer: null,
  author: { id: "planner-session", kind: "session" },
  created_at: new Date(now - 60_000).toISOString(),
  edited_at: null,
  id: `ask-${number}`,
  issue: { key: "CORE-1", title: "Core work" },
  issue_key: "CORE-1",
  kind: "question",
  multiple: false,
  opened_event_id: number,
  options: [],
  priority: null,
  question: "What should happen next?",
  state: "open",
  urgency: "med",
}));

function renderAgents(listedAgents: Agent[] = agents) {
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({ kind: "user", login: "alice" });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(inbox);
  const listAgents = spyOn(api, "listAgents").mockResolvedValue(listedAgents);
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([]);
  const view = render(
    <MemoryRouter initialEntries={["/agents"]}>
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <AuthGate />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return Object.assign(view, { getInbox, listAgents, listIssues, listProjects, whoAmI });
}

test("Agents lists live session activity and capability-aware actions", async () => {
  const page = renderAgents();

  try {
    await screen.findByRole("heading", { name: "Agents" });
    const pageRegion = screen.getByRole("region", { name: "Agents" });
    expect(within(pageRegion).getByText("Planner", { exact: true })).toBeTruthy();
    expect(within(pageRegion).getByText("Needs you 2", { exact: true })).toBeTruthy();
    expect(within(pageRegion).getByText("Open asks 2", { exact: true })).toBeTruthy();
    expect(
      within(pageRegion).getByRole("status", { name: "Seen less than 2 minutes ago" })
    ).toBeTruthy();
    expect(within(pageRegion).getByRole("button", { name: "Steer Planner" })).toBeTruthy();

    fireEvent.click(within(pageRegion).getByRole("button", { name: "BTW Reviewer" }));
    expect(
      (within(pageRegion).getByRole("button", { name: "BTW Reviewer" }) as HTMLButtonElement)
        .disabled
    ).toBe(true);
    expect(
      within(pageRegion).getByRole("button", { name: "BTW Reviewer" }).getAttribute("title")
    ).toBe("Reviewer does not advertise BTW");
  } finally {
    page.getInbox.mockRestore();
    page.listAgents.mockRestore();
    page.listIssues.mockRestore();
    page.listProjects.mockRestore();
    page.unmount();
    page.whoAmI.mockRestore();
  }
});

test("Agents shows the shared empty state when Envoy has no live sessions", async () => {
  const page = renderAgents([]);

  try {
    const emptyState = await screen.findByRole("region", { name: "Agents empty state" });
    expect(within(emptyState).getByText("No agents are connected.")).toBeTruthy();
  } finally {
    page.getInbox.mockRestore();
    page.listAgents.mockRestore();
    page.listIssues.mockRestore();
    page.listProjects.mockRestore();
    page.unmount();
    page.whoAmI.mockRestore();
  }
});
