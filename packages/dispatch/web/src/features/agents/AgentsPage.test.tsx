import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Agent, InboxRow, IssueSummary, Message, MessageRead } from "../../api/types";
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
  waiting_on: "human",
  urgency: "med",
}));

function message(body: string, overrides: Partial<Message> = {}): Message {
  return {
    author: { id: "alice", kind: "user" },
    body,
    created_at: "2026-09-14T00:00:00Z",
    deliveries: [],
    id: "message-1",
    in_reply_to: null,
    issue_key: null,
    target: "session:planner-session",
    ...overrides,
  };
}

function renderAgents({
  listedAgents = agents,
  issues = [],
  messages = [],
}: {
  listedAgents?: Agent[];
  issues?: IssueSummary[];
  messages?: MessageRead[];
} = {}) {
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({ kind: "user", login: "alice" });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(inbox);
  const listAgents = spyOn(api, "listAgents").mockResolvedValue(listedAgents);
  const listAgentMessages = spyOn(api, "listAgentMessages").mockResolvedValue(messages);
  const listIssues = spyOn(api, "listIssues").mockResolvedValue(issues);
  const listProjects = spyOn(api, "listProjects").mockResolvedValue([]);
  const createAgentMessage = spyOn(api, "createAgentMessage").mockImplementation(
    async (_session, input) => message(input.body)
  );
  const createMessage = spyOn(api, "createMessage").mockImplementation(async (issue, input) =>
    message(input.body, { issue_key: issue, target: input.target ?? null })
  );
  const getBlockSchema = spyOn(api, "getBlockSchema").mockResolvedValue({ types: [], version: 1 });
  const createMessageDelivery = spyOn(api, "createMessageDelivery").mockResolvedValue({
    attempt: 2,
    created_at: "2026-09-14T00:00:01Z",
    delivery: "steer",
    envelope_id: "envelope-2",
    error: null,
    message_id: "message-1",
    reply_id: null,
    session_id: "planner-session",
    state: "sent",
  });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={["/agents"]}>
      <QueryClientProvider client={queryClient}>
        <AuthGate />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    createAgentMessage,
    createMessageDelivery,
    createMessage,
    getBlockSchema,
    getInbox,
    listAgentMessages,
    listAgents,
    listIssues,
    listProjects,
    queryClient,
    restore: () => {
      getBlockSchema.mockRestore();
      createMessage.mockRestore();
      createAgentMessage.mockRestore();
      createMessageDelivery.mockRestore();
      listProjects.mockRestore();
      listIssues.mockRestore();
      listAgentMessages.mockRestore();
      listAgents.mockRestore();
      getInbox.mockRestore();
      whoAmI.mockRestore();
    },
    view,
    whoAmI,
  };
}

function card(region: HTMLElement, name: string): HTMLElement {
  const result = within(region).getByRole("heading", { name }).closest("article");
  if (result === null) throw new Error(`${name} card missing`);
  return result;
}

/** Cards collapse by default; the title button toggles the conversation and composer. */
function expand(agentCard: HTMLElement, name: string): void {
  fireEvent.click(within(agentCard).getByRole("button", { name }));
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
    const reviewer = card(pageRegion, "Reviewer");
    expand(reviewer, "Reviewer");
    expect(
      (within(reviewer).getByRole("button", { name: /^BTW$/ }) as HTMLButtonElement).disabled
    ).toBe(true);
    expect(
      (within(reviewer).getByRole("button", { name: /^Aside$/ }) as HTMLButtonElement).disabled
    ).toBe(false);
    expect(
      (within(reviewer).getByRole("button", { name: /^Steer$/ }) as HTMLButtonElement).disabled
    ).toBe(false);
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("Agents collapses every card by default and expands each one independently", async () => {
  const page = renderAgents();

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    expect(within(region).queryByRole("textbox", { name: "Message" })).toBeNull();
    expect(page.listAgentMessages).not.toHaveBeenCalled();
    const planner = card(region, "Planner");
    const toggle = within(planner).getByRole("button", { name: "Planner" });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");

    expand(planner, "Planner");
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    expect(within(planner).getByRole("textbox", { name: "Message" })).toBeTruthy();
    await waitFor(() => expect(page.listAgentMessages).toHaveBeenCalledWith("planner-session"));
    const reviewer = card(region, "Reviewer");
    expect(within(reviewer).queryByRole("textbox", { name: "Message" })).toBeNull();

    expand(reviewer, "Reviewer");
    expect(within(region).getAllByRole("textbox", { name: "Message" })).toHaveLength(2);
    expect(toggle.getAttribute("aria-expanded")).toBe("true");

    expand(planner, "Planner");
    expect(within(planner).queryByRole("textbox", { name: "Message" })).toBeNull();
    expect(within(reviewer).getByRole("textbox", { name: "Message" })).toBeTruthy();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("Agents ask pills open the Inbox narrowed to that agent; zero counts stay text", async () => {
  const page = renderAgents();

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    const planner = card(region, "Planner");
    expect(within(planner).getByRole("link", { name: "Needs you 2" }).getAttribute("href")).toBe(
      "/?agent=planner-session&section=needs-you"
    );
    expect(within(planner).getByRole("link", { name: "Open asks 2" }).getAttribute("href")).toBe(
      "/?agent=planner-session"
    );
    const reviewer = card(region, "Reviewer");
    expect(within(reviewer).getByText("Open asks 0", { exact: true })).toBeTruthy();
    expect(within(reviewer).queryByRole("link")).toBeNull();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("Agents cards copy the session ID and title", async () => {
  const originalClipboard = navigator.clipboard;
  const writeText = spyOn({ writeText: async () => undefined }, "writeText");
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
  const page = renderAgents();

  try {
    const planner = card(await screen.findByRole("region", { name: "Agents" }), "Planner");
    fireEvent.click(
      within(planner).getByRole("button", { name: "Copy session ID planner-session" })
    );
    await waitFor(() => expect(writeText).toHaveBeenCalledWith("planner-session"));
    expect(await within(planner).findByText("Copied", { exact: true })).toBeTruthy();
    fireEvent.click(within(planner).getByRole("button", { name: "Copy session title Planner" }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith("Planner"));
  } finally {
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: originalClipboard });
    page.view.unmount();
    page.restore();
  }
});

test("Agents shows the shared empty state when Envoy has no live sessions", async () => {
  const page = renderAgents({ listedAgents: [] });

  try {
    const emptyState = await screen.findByRole("region", { name: "Agents empty state" });
    expect(within(emptyState).getByText("No agents are connected.")).toBeTruthy();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("Agents labels an untitled session the way every other surface does", async () => {
  const untitled: Agent = { ...agents[1], session_id: "0123456789abcdef", title: "" };
  const page = renderAgents({ listedAgents: [untitled] });

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    expect(within(region).getByRole("heading", { name: "session:01234567…" })).toBeTruthy();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("Agents orders dispatch activity before liveness and keeps a re-poll stable", async () => {
  const olderActivityButNewerHeartbeat: Agent = {
    ...agents[0],
    last_activity: new Date(now - 5 * 60_000).toISOString(),
    last_seen: now - 1_000,
    session_id: "z-session",
    title: "Zulu",
  };
  const newerActivityButOlderHeartbeat: Agent = {
    ...agents[1],
    last_activity: new Date(now - 60_000).toISOString(),
    last_seen: now - 10 * 60_000,
    session_id: "a-session",
    title: "Alpha",
  };
  const noActivity: Agent = {
    ...agents[1],
    last_activity: null,
    last_seen: now,
    session_id: "none-session",
    title: "None",
  };
  const page = renderAgents({
    listedAgents: [olderActivityButNewerHeartbeat, noActivity, newerActivityButOlderHeartbeat],
  });

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    const titles = () =>
      within(region)
        .getAllByRole("heading", { level: 2 })
        .map((heading) => heading.textContent);
    expect(titles()).toEqual(["Alpha", "Zulu", "None"]);
    await page.queryClient.refetchQueries({ queryKey: ["agents"] });
    expect(titles()).toEqual(["Alpha", "Zulu", "None"]);
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("Agents pins cards in a per-login preference across a reload", async () => {
  window.localStorage.removeItem("dispatch.agents.pinned:alice");
  const first = renderAgents();
  let firstActive = true;

  try {
    const pin = await screen.findByRole("button", { name: "Pin Planner" });
    expect(pin.getAttribute("aria-pressed")).toBe("false");
    fireEvent.click(pin);
    expect(pin.getAttribute("aria-pressed")).toBe("true");
    expect(window.localStorage.getItem("dispatch.agents.pinned:alice")).toBe(
      JSON.stringify(["planner-session"])
    );
    first.view.unmount();
    first.restore();
    firstActive = false;

    const second = renderAgents();
    try {
      const persistedPin = await screen.findByRole("button", { name: "Unpin Planner" });
      expect(persistedPin.getAttribute("aria-pressed")).toBe("true");
    } finally {
      second.view.unmount();
      second.restore();
    }
  } finally {
    if (firstActive) {
      first.view.unmount();
      first.restore();
    }
    window.localStorage.removeItem("dispatch.agents.pinned:alice");
  }
});

test("Agents sends without an issue through the agent message route", async () => {
  const page = renderAgents();

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    const planner = card(region, "Planner");
    expand(planner, "Planner");
    await waitFor(() =>
      expect(
        (within(planner).getByRole("button", { name: "BTW" }) as HTMLButtonElement).disabled
      ).toBe(false)
    );
    fireEvent.change(within(planner).getByRole("textbox", { name: "Message" }), {
      target: { value: "Check the deployment" },
    });
    fireEvent.submit(within(planner).getByRole("form", { name: "Message composer" }));
    await waitFor(() =>
      expect(page.createAgentMessage).toHaveBeenCalledWith("planner-session", {
        body: "Check the deployment",
        delivery: "btw",
      })
    );
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("Agents keeps selected-issue sends on the issue message route", async () => {
  const page = renderAgents({
    issues: [
      {
        key: "CORE-1",
        last_seq: 0,
        open_asks: 0,
        parent: null,
        priority: null,
        rank: "U",
        status: "todo",
        title: "Core work",
        updated_at: "2026-09-14T00:00:00Z",
      },
    ],
  });

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    const planner = card(region, "Planner");
    expand(planner, "Planner");
    fireEvent.click(within(planner).getByRole("button", { name: "Choose issue" }));
    await waitFor(() =>
      expect(within(planner).getByRole("combobox", { name: "Issue" })).toBeTruthy()
    );
    fireEvent.change(within(planner).getByRole("combobox", { name: "Issue" }), {
      target: { value: "CORE-1" },
    });
    await waitFor(() =>
      expect(
        (within(planner).getByRole("button", { name: "BTW" }) as HTMLButtonElement).disabled
      ).toBe(false)
    );
    fireEvent.change(within(planner).getByRole("textbox", { name: "Message" }), {
      target: { value: "Track this under Core" },
    });
    fireEvent.submit(within(planner).getByRole("form", { name: "Message composer" }));
    await waitFor(() =>
      expect(page.createMessage).toHaveBeenCalledWith("CORE-1", {
        body: "Track this under Core",
        delivery: "btw",
        target: "session:planner-session",
      })
    );
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("Agents loads only open issues when an optional picker is opened", async () => {
  const page = renderAgents();

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    const planner = card(region, "Planner");
    expand(planner, "Planner");
    expect(page.listIssues).not.toHaveBeenCalled();
    fireEvent.click(within(planner).getByRole("button", { name: "Choose issue" }));
    await waitFor(() => expect(page.listIssues).toHaveBeenCalledWith({ open: true }));
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("Agents renders each targeted message and its reply beneath the matching card", async () => {
  const root = message("Can this ship?", {
    deliveries: [
      {
        attempt: 1,
        created_at: "2026-09-14T00:00:00Z",
        delivery: "btw",
        envelope_id: "envelope-1",
        error: null,
        message_id: "message-1",
        reply_id: "message-2",
        session_id: "planner-session",
        state: "sent",
      },
    ],
  });
  const page = renderAgents({
    messages: [
      {
        message: root,
        replies: [
          message("Yes, it can.", {
            author: { id: "planner-session", kind: "session" },
            id: "message-2",
            in_reply_to: root.id,
          }),
        ],
      },
    ],
  });

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    const planner = card(region, "Planner");
    expand(planner, "Planner");
    const conversation = await within(planner).findByRole("list", {
      name: "Conversation with Planner",
    });
    await expect(within(conversation).findByText("Can this ship?")).resolves.toBeTruthy();
    await expect(within(conversation).findByText("Yes, it can.")).resolves.toBeTruthy();
    expect(within(conversation).getByText("Answered by Planner")).toBeTruthy();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("Agents retain targeted-message retries and attempt history", async () => {
  const root = message("Can this ship?", {
    deliveries: [
      {
        attempt: 1,
        created_at: "2026-09-14T00:00:00Z",
        delivery: "btw",
        envelope_id: null,
        error: "no live session planner-session",
        message_id: "message-1",
        reply_id: null,
        session_id: "planner-session",
        state: "failed",
      },
    ],
  });
  const page = renderAgents({ messages: [{ message: root, replies: [] }] });

  try {
    const planner = card(await screen.findByRole("region", { name: "Agents" }), "Planner");
    expand(planner, "Planner");
    const retry = await within(planner).findByRole("button", { name: "Send normally" });
    expect(within(planner).getByRole("button", { name: "Ask BTW again" })).toBeTruthy();
    fireEvent.click(retry);
    await waitFor(() =>
      expect(page.createMessageDelivery).toHaveBeenCalledWith("message-1", "steer")
    );
  } finally {
    page.view.unmount();
    page.restore();
  }
});
