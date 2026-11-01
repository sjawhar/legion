import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Agent, BroadcastSummary } from "../../api/types";

import { BroadcastsPage } from "./BroadcastsPage";

const planner: Agent = {
  capabilities: ["steer"],
  dir: "/workspaces/planner",
  host: "build-host",
  last_activity: null,
  last_seen: Date.now(),
  machine_id: "build-host",
  open_asks: 0,
  roles: [],
  session_id: "planner-session",
  title: "Planner",
};

// INFRA-440. A row is one link to its broadcast, and a session sender is drawn by `AgentName`,
// whose name links to its live conversation: the sender sits beside the row's link, never inside
// it, so neither link holds the other and the row link keeps its broadcast destination.
test("a broadcast row draws its session sender outside the row's link", async () => {
  const sent: BroadcastSummary = {
    author: { id: "planner-session", kind: "session" },
    body: "Report status.",
    created_at: new Date().toISOString(),
    delivery: "steer",
    id: "broadcast-1",
    recipients: 2,
    replies: 1,
  };
  const listBroadcasts = spyOn(api, "listBroadcasts").mockResolvedValue([sent]);
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([planner]);
  const view = render(
    <MemoryRouter>
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <BroadcastsPage />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    const row = await screen.findByRole("listitem");
    const rowLink = within(row).getByRole("link", { name: /1 of 2 answered/ });
    expect(rowLink.getAttribute("href")).toBe("/agents/broadcasts/broadcast-1");
    const sender = await within(row).findByRole("link", { name: "Planner" });
    expect(sender.getAttribute("href")).toBe("/agents/planner-session/live");
    expect(rowLink.contains(sender)).toBe(false);
    expect(sender.contains(rowLink)).toBe(false);
    const agent = row.querySelector('[data-agent-name="planner-session"]');
    await waitFor(() => expect(agent?.getAttribute("data-agent-state")).toBe("running"));
  } finally {
    view.unmount();
    listAgents.mockRestore();
    listBroadcasts.mockRestore();
  }
});
