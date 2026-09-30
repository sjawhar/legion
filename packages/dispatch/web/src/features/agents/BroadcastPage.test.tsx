import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { api } from "../../api/client";
import type { Agent, BroadcastRead, MessageDelivery } from "../../api/types";

import { BroadcastPage } from "./BroadcastPage";

const agents: Agent[] = [
  {
    capabilities: ["aside", "btw", "steer"],
    dir: "/workspaces/planner",
    last_activity: null,
    last_seen: Date.now(),
    machine_id: "build-host",
    open_asks: 0,
    roles: [],
    session_id: "planner-session",
    title: "Planner",
  },
];

function attempt(overrides: Partial<MessageDelivery>): MessageDelivery {
  return {
    attempt: 1,
    created_at: new Date().toISOString(),
    delivery: "steer",
    duplicate: false,
    envelope_id: null,
    error: null,
    message_id: "message-1",
    reply_id: null,
    session_id: "planner-session",
    state: "pending",
    ...overrides,
  };
}

function broadcast(deliveries: MessageDelivery[]): BroadcastRead {
  return {
    author: { id: "alice", kind: "user" },
    body: "Report status.",
    created_at: new Date().toISOString(),
    delivery: "steer",
    id: "broadcast-1",
    recipients: [
      {
        message: {
          author: { id: "alice", kind: "user" },
          body: "Report status.",
          created_at: new Date().toISOString(),
          deliveries,
          id: "message-1",
          in_reply_to: null,
          issue_key: null,
          target: "session:planner-session",
        },
        replies: [],
        session_id: "planner-session",
      },
    ],
  };
}

function renderBroadcast(read: BroadcastRead) {
  const getBroadcast = spyOn(api, "getBroadcast").mockResolvedValue(read);
  const listAgents = spyOn(api, "listAgents").mockResolvedValue(agents);
  const createMessageDelivery = spyOn(api, "createMessageDelivery").mockResolvedValue(
    attempt({ attempt: 2, state: "sent" })
  );
  const view = render(
    <MemoryRouter initialEntries={["/agents/broadcasts/broadcast-1"]}>
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <Routes>
          <Route element={<BroadcastPage />} path="/agents/broadcasts/:id" />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    createMessageDelivery,
    restore: () => {
      createMessageDelivery.mockRestore();
      listAgents.mockRestore();
      getBroadcast.mockRestore();
    },
    view,
  };
}

// LEGION-233 review, P2. A send a worker is still carrying must offer nothing: the mode-change
// buttons are a genuine second frame, and taking one while the worker was sending delivered the
// message twice.
test("a recipient whose send is still outstanding offers no retry at all", async () => {
  const page = renderBroadcast(broadcast([attempt({})]));
  try {
    const row = await screen.findByRole("article", { name: "Planner" });
    expect(within(row).getByText("Sending to Planner (Send)")).toBeTruthy();
    // textContent, never the elements: a failed toEqual on DOM nodes serialises the whole tree
    // and can take minutes to report.
    expect(
      within(row)
        .queryAllByRole("button")
        .map((button) => button.textContent)
    ).toEqual([]);
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// The other half: a process that died mid-delivery leaves a pending attempt nobody holds, and
// that recipient could previously only be moved by a delivery in a DIFFERENT mode - a second
// frame wherever the first landed. Its own mode is the only one offered, and it is offered.
test("a pending attempt nobody is carrying offers a same-mode retry and no mode change", async () => {
  const stranded = attempt({ created_at: new Date(Date.now() - 5 * 60_000).toISOString() });
  const page = renderBroadcast(broadcast([stranded]));
  try {
    const row = await screen.findByRole("article", { name: "Planner" });
    const buttons = within(row)
      .getAllByRole("button")
      .map((button) => button.textContent);
    expect(buttons).toEqual(["Retry"]);
    fireEvent.click(within(row).getByRole("button", { name: "Retry" }));
    await waitFor(() =>
      expect(page.createMessageDelivery).toHaveBeenCalledWith("message-1", "steer")
    );
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// A settled failure is different: nothing is outstanding, so a mode change sends rather than
// duplicates, and the agent card's full retry row applies.
test("a failed attempt offers the same-mode retry and the mode changes", async () => {
  const failed = attempt({ error: "no live session", state: "failed" });
  const page = renderBroadcast(broadcast([failed]));
  try {
    const row = await screen.findByRole("article", { name: "Planner" });
    const buttons = within(row)
      .getAllByRole("button")
      .map((button) => button.textContent);
    expect(buttons).toEqual(["Retry", "Use BTW instead"]);
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("a delivered recipient offers nothing", async () => {
  const page = renderBroadcast(broadcast([attempt({ envelope_id: "e1", state: "sent" })]));
  try {
    const row = await screen.findByRole("article", { name: "Planner" });
    expect(within(row).getByText("Sent to Planner (Send)")).toBeTruthy();
    // textContent, never the elements: a failed toEqual on DOM nodes serialises the whole tree
    // and can take minutes to report.
    expect(
      within(row)
        .queryAllByRole("button")
        .map((button) => button.textContent)
    ).toEqual([]);
  } finally {
    page.view.unmount();
    page.restore();
  }
});
