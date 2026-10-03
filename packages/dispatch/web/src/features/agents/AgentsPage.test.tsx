import { expect, spyOn, test } from "bun:test";
import { RECEIPT_TIMEOUT_CAUSE } from "@legion/contracts";
import { focusManager, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type {
  Agent,
  CreateBroadcastInput,
  InboxRow,
  IssueSummary,
  Message,
  MessageDelivery,
  MessageDeliveryMode,
  MessageRead,
  UserAgentStates,
} from "../../api/types";
import { AuthGate } from "../../app";
import { orderAgents, partitionAgents } from "./AgentsPage";
import { broadcastPlan, broadcastSendState, composedBroadcast } from "./broadcast-plan";

// Delivery attempts are dated relative to the run: the dashboard only offers a
// same-mode Retry while an attempt is inside the stream's duplicate window, so a
// fixture frozen at an absolute date would age out of every retry assertion.
const recentAttemptAt = new Date(Date.now() - 60_000).toISOString();

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
    last_activity: new Date(now - 10 * 60_000).toISOString(),
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
  issue: { assignee: null, key: "CORE-1", title: "Core work" },
  issue_key: "CORE-1",
  kind: "question",
  multiple: false,
  opened_event_id: number,
  options: [],
  thread: { edits: [], followers: [], replies: [] },
  priority: null,
  snoozed_until: null,
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
  agentState = {},
  inboxRows = inbox,
  listedAgents = agents,
  issues = [],
  messages = [],
}: {
  agentState?: UserAgentStates;
  inboxRows?: InboxRow[];
  listedAgents?: Agent[];
  issues?: IssueSummary[];
  messages?: MessageRead[];
} = {}) {
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({ kind: "user", login: "alice" });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(inboxRows);
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
  const createComment = spyOn(api, "createComment").mockResolvedValue(undefined as never);
  const createBroadcast = spyOn(api, "createBroadcast").mockImplementation(async (input) => ({
    author: { id: "alice", kind: "user" },
    body: input.body,
    created_at: "2026-09-14T00:00:00Z",
    delivery: input.delivery,
    excluded: [],
    id: "broadcast-1",
    recipients: input.session_ids.map((sessionID) => ({
      message: message(input.body, { target: `session:${sessionID}` }),
      replies: [],
      session_id: sessionID,
    })),
  }));
  const getBlockSchema = spyOn(api, "getBlockSchema").mockResolvedValue({ types: [], version: 1 });
  const createMessageDelivery = spyOn(api, "createMessageDelivery").mockResolvedValue({
    attempt: 2,
    created_at: recentAttemptAt,
    delivery: "steer",
    envelope_id: "envelope-2",
    error: null,
    message_id: "message-1",
    reply_id: null,
    session_id: "planner-session",
    state: "sent",
  });
  const getMyAgentState = spyOn(api, "getMyAgentState").mockResolvedValue(agentState);
  const putAgentState = spyOn(api, "putAgentState").mockImplementation(async (_session, input) => ({
    ...input,
    unread_replies: 0,
  }));
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
    createComment,
    createBroadcast,
    createMessageDelivery,
    createMessage,
    getBlockSchema,
    getInbox,
    getMyAgentState,
    listAgentMessages,
    listAgents,
    listIssues,
    listProjects,
    putAgentState,
    queryClient,
    restore: () => {
      putAgentState.mockRestore();
      getMyAgentState.mockRestore();
      getBlockSchema.mockRestore();
      createComment.mockRestore();
      createMessage.mockRestore();
      createAgentMessage.mockRestore();
      createBroadcast.mockRestore();
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
    expect(within(pageRegion).queryByText(/^Open asks/)).toBeNull();
    expect(
      within(pageRegion).getByRole("status", { name: "Seen less than 2 minutes ago" })
    ).toBeTruthy();
    const reviewer = card(pageRegion, "Reviewer");
    expand(reviewer, "Reviewer");
    expect(within(reviewer).queryByRole("button", { name: /^BTW$|^Aside$|^Steer$/ })).toBeNull();
    expect(within(reviewer).getByRole("textbox", { name: "Comment" })).toBeTruthy();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("Agents collapses every card by default and expands each one independently", async () => {
  const page = renderAgents();

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    expect(within(region).queryByRole("textbox", { name: "Comment" })).toBeNull();
    expect(page.listAgentMessages).not.toHaveBeenCalled();
    const planner = card(region, "Planner");
    const toggle = within(planner).getByRole("button", { name: "Planner" });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");

    expand(planner, "Planner");
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    expect(within(planner).getByRole("textbox", { name: "Comment" })).toBeTruthy();
    await waitFor(() => expect(page.listAgentMessages).toHaveBeenCalledWith("planner-session"));
    const reviewer = card(region, "Reviewer");
    expect(within(reviewer).queryByRole("textbox", { name: "Comment" })).toBeNull();

    expand(reviewer, "Reviewer");
    expect(within(region).getAllByRole("textbox", { name: "Comment" })).toHaveLength(2);
    expect(toggle.getAttribute("aria-expanded")).toBe("true");

    expand(planner, "Planner");
    expect(within(planner).queryByRole("textbox", { name: "Comment" })).toBeNull();
    expect(within(reviewer).getByRole("textbox", { name: "Comment" })).toBeTruthy();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("Agents shows one whose-turn pill per card: Needs you, plus Waiting on agent only for the asks you have answered, none at zero", async () => {
  // Planner: both of its open asks wait on the viewer. Builder: three open asks, two waiting on
  // the viewer, so the third is the agent's move. Reviewer: nothing open.
  const builder: Agent = {
    ...agents[0],
    open_asks: 3,
    session_id: "builder-session",
    title: "Builder",
  };
  const page = renderAgents({
    inboxRows: [
      ...inbox,
      ...inbox.map((ask, index) => ({
        ...ask,
        author: { id: "builder-session", kind: "session" as const },
        id: `builder-ask-${index}`,
      })),
    ],
    listedAgents: [...agents, builder],
  });

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    const planner = card(region, "Planner");
    const needsYou = await within(planner).findByRole("link", { name: "Needs you 2" });
    expect(needsYou.getAttribute("href")).toBe("/?agent=planner-session&section=needs-you");
    expect(within(planner).queryByText(/^Waiting on agent/)).toBeNull();
    expect(within(planner).queryByText(/^Open asks/)).toBeNull();

    const builderCard = card(region, "Builder");
    expect(
      (await within(builderCard).findByRole("link", { name: "Needs you 2" })).getAttribute("href")
    ).toBe("/?agent=builder-session&section=needs-you");
    const waiting = within(builderCard).getByRole("link", { name: "Waiting on agent 1" });
    expect(waiting.getAttribute("href")).toBe("/?agent=builder-session");
    expect(waiting.getAttribute("title")).toContain("Builder");

    const reviewer = card(region, "Reviewer");
    // Every card carries the Open action; what this card must not carry is a whose-turn pill.
    expect(within(reviewer).queryByRole("link", { name: /Needs you|Waiting on agent/ })).toBeNull();
    expect(within(reviewer).queryByText(/^(Needs you|Waiting on agent|Open asks)/)).toBeNull();
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

/** A live session with no Dispatch signal unless the overrides give it one. */
function session(overrides: Partial<Agent> & Pick<Agent, "session_id">): Agent {
  return {
    ...agents[1],
    last_activity: null,
    open_asks: 0,
    title: overrides.session_id,
    ...overrides,
  };
}

const minutesAgo = (minutes: number) => new Date(now - minutes * 60_000).toISOString();

test("orderAgents puts who needs you first, then open asks, then Dispatch recency with silent sessions last", () => {
  const needsYou = session({ last_activity: minutesAgo(30), open_asks: 1, session_id: "needy" });
  const owed = session({ last_activity: minutesAgo(20), open_asks: 3, session_id: "owed" });
  const recent = session({ last_activity: minutesAgo(1), session_id: "recent" });
  const older = session({ last_activity: minutesAgo(5), session_id: "older" });
  const silentB = session({ session_id: "silent-b", title: "Beta" });
  const silentA = session({ session_id: "silent-a", title: "Alpha" });
  const ordered = orderAgents(
    [silentB, older, recent, owed, silentA, needsYou],
    [],
    new Map([["needy", 1]])
  );
  expect(ordered.map((agent) => agent.session_id)).toEqual([
    "needy",
    "owed",
    "recent",
    "older",
    "silent-a",
    "silent-b",
  ]);
});

test("orderAgents keeps pinned sessions first in pin order, whoever needs you", () => {
  const needsYou = session({ last_activity: minutesAgo(1), open_asks: 1, session_id: "needy" });
  const pinnedSilent = session({ session_id: "pinned-silent" });
  const pinnedRecent = session({ last_activity: minutesAgo(2), session_id: "pinned-recent" });
  const ordered = orderAgents(
    [needsYou, pinnedRecent, pinnedSilent],
    ["pinned-silent", "pinned-recent"],
    new Map([["needy", 1]])
  );
  expect(ordered.map((agent) => agent.session_id)).toEqual([
    "pinned-silent",
    "pinned-recent",
    "needy",
  ]);
});

test("orderAgents orders Dispatch activity within one second by time, not by the timestamp strings", () => {
  // As text `…00.12Z` sorts after `…00.123456Z`, the later time.
  const earlier = session({ last_activity: "2026-10-02T00:00:00.12Z", session_id: "earlier" });
  const later = session({ last_activity: "2026-10-02T00:00:00.123456Z", session_id: "later" });
  expect(orderAgents([earlier, later], [], new Map()).map((agent) => agent.session_id)).toEqual([
    "later",
    "earlier",
  ]);
});

test("partitionAgents splits live sessions with a Dispatch signal, silent live sessions, and unseen sessions", () => {
  const asked = session({ open_asks: 1, session_id: "asked" });
  const spoke = session({ last_activity: minutesAgo(3), session_id: "spoke" });
  const awaited = session({ session_id: "awaited" });
  const silent = session({ session_id: "silent" });
  const pinnedSilent = session({ session_id: "pinned-silent" });
  const unseenSilent = session({ last_seen: now - 11 * 60_000, session_id: "unseen-silent" });
  const unseenSpoke = session({
    last_activity: minutesAgo(1),
    last_seen: now - 10 * 60_000,
    session_id: "unseen-spoke",
  });
  const parts = partitionAgents(
    [silent, unseenSilent, spoke, pinnedSilent, unseenSpoke, awaited, asked],
    ["pinned-silent"],
    new Map([["awaited", 1]]),
    now
  );
  const ids = (agents: readonly Agent[]) => agents.map((agent) => agent.session_id);
  expect(ids(parts.active)).toEqual(["pinned-silent", "awaited", "asked", "spoke"]);
  expect(ids(parts.quiet)).toEqual(["silent"]);
  expect(ids(parts.inactive)).toEqual(["unseen-spoke", "unseen-silent"]);
});

test("Agents orders who needs you before Dispatch recency before liveness, folds silent sessions, and keeps a re-poll stable", async () => {
  const olderActivityButNewerHeartbeat: Agent = {
    ...agents[1],
    last_activity: minutesAgo(5),
    last_seen: now - 1_000,
    session_id: "z-session",
    title: "Zulu",
  };
  const newerActivityButOlderHeartbeat: Agent = {
    ...agents[1],
    last_activity: minutesAgo(1),
    last_seen: now - 9 * 60_000,
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
  const needsYouButOldest: Agent = { ...agents[0], last_activity: minutesAgo(45) };
  const page = renderAgents({
    listedAgents: [
      olderActivityButNewerHeartbeat,
      noActivity,
      newerActivityButOlderHeartbeat,
      needsYouButOldest,
    ],
  });

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    const titles = () =>
      within(region)
        .getAllByRole("heading", { level: 2 })
        .map((heading) => heading.textContent);
    await within(region).findByText("Needs you 2", { exact: true });
    expect(titles()).toEqual(["Planner", "Alpha", "Zulu"]);
    await page.queryClient.refetchQueries({ queryKey: ["agents"] });
    expect(titles()).toEqual(["Planner", "Alpha", "Zulu"]);

    const disclosure = within(region).getByRole("button", { name: "No Dispatch activity (1)" });
    expect(disclosure.getAttribute("aria-expanded")).toBe("false");
    expect(within(region).queryByRole("region", { name: "No Dispatch activity" })).toBeNull();
    fireEvent.click(disclosure);
    expect(disclosure.getAttribute("aria-expanded")).toBe("true");
    const fold = within(region).getByRole("region", { name: "No Dispatch activity" });
    const noneCard = card(fold, "None");
    expect(within(noneCard).getByText("No Dispatch activity", { exact: true })).toBeTruthy();
    expect(titles()).toEqual(["Planner", "Alpha", "Zulu", "None"]);
    expand(noneCard, "None");
    expect(within(noneCard).getByRole("textbox", { name: "Comment" })).toBeTruthy();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("Agents renders no fold when every live session has Dispatch activity", async () => {
  const page = renderAgents();

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    expect(within(region).queryByRole("button", { name: /^No Dispatch activity \(/ })).toBeNull();
    expect(within(region).queryByRole("button", { name: /^Inactive \(/ })).toBeNull();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("Agents lists a pinned silent session among the active rows, and folds it again when unpinned", async () => {
  const silent: Agent = {
    ...agents[1],
    last_activity: null,
    session_id: "silent-session",
    title: "Silent",
  };
  window.localStorage.setItem("dispatch.agents.pinned:alice", JSON.stringify(["silent-session"]));
  const page = renderAgents({ listedAgents: [...agents, silent] });

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    await screen.findByRole("button", { name: "Unpin Silent" });
    expect(
      within(region)
        .getAllByRole("heading", { level: 2 })
        .map((heading) => heading.textContent)
    ).toEqual(["Silent", "Planner", "Reviewer"]);
    expect(within(region).queryByRole("button", { name: /^No Dispatch activity \(/ })).toBeNull();

    fireEvent.click(within(region).getByRole("button", { name: "Unpin Silent" }));
    expect(within(region).getByRole("button", { name: "No Dispatch activity (1)" })).toBeTruthy();
    expect(within(region).queryByRole("heading", { name: "Silent" })).toBeNull();
  } finally {
    page.view.unmount();
    page.restore();
    window.localStorage.removeItem("dispatch.agents.pinned:alice");
  }
});

const stale: Agent = {
  ...agents[1],
  last_activity: new Date(now - 30_000).toISOString(),
  last_seen: now - 11 * 60_000,
  session_id: "stale-session",
  title: "Stale",
};

test("Agents folds sessions unseen for ten minutes under a collapsed Inactive disclosure", async () => {
  const page = renderAgents({ listedAgents: [stale, ...agents] });

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    const titles = () =>
      within(region)
        .getAllByRole("heading", { level: 2 })
        .map((heading) => heading.textContent);
    // Newest Dispatch activity would put Stale first; the grey-dot rule folds it instead.
    expect(titles()).toEqual(["Planner", "Reviewer"]);
    const disclosure = within(region).getByRole("button", { name: "Inactive (1)" });
    expect(disclosure.getAttribute("aria-expanded")).toBe("false");
    expect(within(region).queryByRole("region", { name: "Inactive" })).toBeNull();

    fireEvent.click(disclosure);
    expect(disclosure.getAttribute("aria-expanded")).toBe("true");
    const fold = within(region).getByRole("region", { name: "Inactive" });
    const staleCard = card(fold, "Stale");
    expect(
      within(staleCard).getByRole("status", { name: "Seen 10 minutes ago or longer" })
    ).toBeTruthy();
    expect(titles()).toEqual(["Planner", "Reviewer", "Stale"]);
    // The fold's rows keep the composer.
    expand(staleCard, "Stale");
    expect(within(staleCard).getByRole("textbox", { name: "Comment" })).toBeTruthy();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("Agents folds a session that is both silent and unseen under Inactive, never under No Dispatch activity", async () => {
  const silentStale: Agent = { ...stale, last_activity: null };
  const page = renderAgents({ listedAgents: [...agents, silentStale] });

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    expect(within(region).getByRole("button", { name: "Inactive (1)" })).toBeTruthy();
    expect(within(region).queryByRole("button", { name: /^No Dispatch activity \(/ })).toBeNull();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("Agents keeps a pinned session in the active list however long it has been quiet", async () => {
  window.localStorage.setItem("dispatch.agents.pinned:alice", JSON.stringify(["stale-session"]));
  const page = renderAgents({ listedAgents: [...agents, stale] });

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    await screen.findByRole("button", { name: "Unpin Stale" });
    expect(
      within(region)
        .getAllByRole("heading", { level: 2 })
        .map((heading) => heading.textContent)
    ).toEqual(["Stale", "Planner", "Reviewer"]);
    expect(within(region).queryByRole("button", { name: /^Inactive \(/ })).toBeNull();

    fireEvent.click(within(region).getByRole("button", { name: "Unpin Stale" }));
    expect(within(region).getByRole("button", { name: "Inactive (1)" })).toBeTruthy();
    expect(within(region).queryByRole("heading", { name: "Stale" })).toBeNull();
  } finally {
    page.view.unmount();
    page.restore();
    window.localStorage.removeItem("dispatch.agents.pinned:alice");
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
    fireEvent.change(within(planner).getByRole("textbox", { name: "Comment" }), {
      target: { value: "/btw Check the deployment" },
    });
    fireEvent.submit(within(planner).getByRole("form", { name: "Comment composer" }));
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
        route: null,
        route_status: null,
        route_holder: null,
        key: "CORE-1",
        last_seq: 0,
        open_asks: 0,
        parent: null,
        assignee: null,
        claim: null,
        components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
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
        (within(planner).getByRole("textbox", { name: "Comment" }) as HTMLTextAreaElement).value
      ).toBe("@Planner")
    );
    fireEvent.change(within(planner).getByRole("textbox", { name: "Comment" }), {
      target: { value: "@Planner Track this under Core" },
    });
    fireEvent.submit(within(planner).getByRole("form", { name: "Comment composer" }));
    await waitFor(() =>
      expect(page.createComment).toHaveBeenCalledWith("CORE-1", {
        body: "@Planner Track this under Core",
        delivery: "steer",
        mentions: [{ target: "session:planner-session" }],
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
        created_at: recentAttemptAt,
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

test("Agents keeps an issue-attached legacy reply on its issue message route", async () => {
  const root = message("Can this ship?", {
    issue_key: "CORE-1",
    target: "session:planner-session",
    deliveries: [
      {
        attempt: 1,
        created_at: recentAttemptAt,
        delivery: "btw",
        envelope_id: "envelope-1",
        error: null,
        message_id: "message-1",
        reply_id: null,
        session_id: "planner-session",
        state: "sent",
      },
    ],
  });
  const page = renderAgents({ messages: [{ message: root, replies: [] }] });

  try {
    const planner = card(await screen.findByRole("region", { name: "Agents" }), "Planner");
    expand(planner, "Planner");
    await within(planner).findByText("Can this ship?");
    fireEvent.click(within(planner).getByRole("button", { name: "Reply" }));
    fireEvent.change(within(planner).getByRole("textbox", { name: "Comment" }), {
      target: { value: "Follow up" },
    });
    fireEvent.submit(within(planner).getByRole("form", { name: "Comment composer" }));

    await waitFor(() =>
      expect(page.createMessage).toHaveBeenCalledWith("CORE-1", {
        body: "Follow up",
        delivery: "btw",
        in_reply_to: "message-1",
        target: "session:planner-session",
      })
    );
    expect(page.createAgentMessage).not.toHaveBeenCalled();
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
        created_at: recentAttemptAt,
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
  const steerCapableAgents = agents.map((candidate) =>
    candidate.session_id === "planner-session"
      ? { ...candidate, capabilities: [...candidate.capabilities, "steer"] }
      : candidate
  );
  const page = renderAgents({
    listedAgents: steerCapableAgents,
    messages: [{ message: root, replies: [] }],
  });

  try {
    const planner = card(await screen.findByRole("region", { name: "Agents" }), "Planner");
    expand(planner, "Planner");
    const retry = await within(planner).findByRole("button", { name: "Use Send instead" });
    expect(retry.hasAttribute("disabled")).toBe(false);
    const sameMode = within(planner).getByRole("button", { name: "Retry" });
    expect(sameMode.hasAttribute("disabled")).toBe(false);
    fireEvent.click(retry);
    await waitFor(() =>
      expect(page.createMessageDelivery).toHaveBeenCalledWith("message-1", "steer")
    );
    // The same-mode Retry re-sends the attempt's own mode, the only retry that cannot deliver
    // the message twice.
    fireEvent.click(sameMode);
    await waitFor(() =>
      expect(page.createMessageDelivery).toHaveBeenCalledWith("message-1", "btw")
    );
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("Use Send instead is disabled when the target does not advertise steer", async () => {
  const root = message("Can this ship?", {
    deliveries: [
      {
        attempt: 1,
        created_at: recentAttemptAt,
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
  // The shared "planner" fixture advertises aside and btw, not steer.
  const page = renderAgents({ messages: [{ message: root, replies: [] }] });

  try {
    const planner = card(await screen.findByRole("region", { name: "Agents" }), "Planner");
    expand(planner, "Planner");
    const retry = await within(planner).findByRole("button", { name: "Use Send instead" });
    expect(retry.hasAttribute("disabled")).toBe(true);
    expect(retry.hasAttribute("title")).toBe(false);
    await within(planner).findByText("Planner does not support Send — use BTW.");
    expect(within(planner).getByRole("button", { name: "Retry" }).hasAttribute("disabled")).toBe(
      false
    );
  } finally {
    page.view.unmount();
    page.restore();
  }
});

/** One attempt of `messageID` to `sessionID` in `delivery` mode, sent. */
function sentAttempt(
  messageID: string,
  delivery: "aside" | "btw" | "steer",
  overrides: Partial<MessageDelivery> = {}
): MessageDelivery {
  return {
    attempt: 1,
    created_at: recentAttemptAt,
    delivery,
    envelope_id: "envelope-1",
    error: null,
    message_id: messageID,
    reply_id: null,
    session_id: "planner-session",
    state: "sent",
    ...overrides,
  };
}

const accepted = { accepted_as: "user_turn", accepted_at: recentAttemptAt } as const;

// A person's direct Send or Aside that an Oh My Pi session took as its own user turn is answered
// in the session's conversation, never with a Dispatch reply, so its card waits on no reply and
// offers no other way to send it. The session records that with Dispatch; nothing else says it,
// so every attempt the session did not accept - a Claude Code session's, an older plugin's, an
// accept Dispatch refused - keeps today's card and its mode-change row.
for (const [name, session, read, headline, retries] of [
  [
    "a person's direct Aside the session took as its own turn",
    "Planner",
    {
      message: message("Where is the dashboard?", {
        deliveries: [sentAttempt("message-1", "aside", accepted)],
      }),
      replies: [],
    },
    "Delivered to Planner's conversation (Aside)",
    0,
  ],
  [
    "a person's reply the session took as its own turn",
    "Planner",
    {
      message: message("Where is the dashboard?", {
        deliveries: [sentAttempt("message-1", "aside", accepted)],
      }),
      replies: [
        message("And the logs?", {
          deliveries: [sentAttempt("message-2", "steer", accepted)],
          id: "message-2",
          in_reply_to: "message-1",
        }),
      ],
    },
    "Delivered to Planner's conversation (Send)",
    0,
  ],
  [
    "a turn the session took whose send the listener later recorded as failed",
    "Planner",
    {
      message: message("Where is the dashboard?", {
        deliveries: [
          sentAttempt("message-1", "aside", {
            ...accepted,
            envelope_id: null,
            error: RECEIPT_TIMEOUT_CAUSE,
            state: "failed",
          }),
        ],
      }),
      replies: [],
    },
    "Delivered to Planner's conversation (Aside)",
    0,
  ],
  [
    "a person's direct Aside the session did not accept",
    "Planner",
    {
      message: message("Where is the dashboard?", {
        deliveries: [sentAttempt("message-1", "aside")],
      }),
      replies: [],
    },
    "Sent to Planner (Aside)",
    1,
  ],
  [
    "an Aside an aside-only session got as a card",
    "Reviewer",
    {
      message: message("Where is the dashboard?", {
        deliveries: [sentAttempt("message-1", "aside", { session_id: "reviewer-session" })],
        target: "session:reviewer-session",
      }),
      replies: [],
    },
    "Sent to Reviewer (Aside)",
    1,
  ],
] as const satisfies readonly (readonly [string, string, MessageRead, string, number])[]) {
  test(`Agents shows ${name} as ${retries === 0 ? "delivered to the conversation" : "sent, awaiting a reply"}`, async () => {
    // As a current Dispatch answers: a direct message no broadcast sent reads broadcast_id null.
    const page = renderAgents({
      messages: [
        {
          message: { ...read.message, broadcast_id: null },
          replies: read.replies.map((reply) => ({ ...reply, broadcast_id: null })),
        },
      ],
    });

    try {
      const target = card(await screen.findByRole("region", { name: "Agents" }), session);
      expand(target, session);
      await within(target).findByText(headline);
      expect(within(target).queryAllByRole("button", { name: "Use BTW instead" })).toHaveLength(
        retries
      );
    } finally {
      page.view.unmount();
      page.restore();
    }
  });
}

/** One exchange: a root from Alice at `createdAt`, optionally answered by the planner. */
function exchange(
  id: string,
  body: string,
  createdAt: string,
  reply?: { body: string; createdAt: string; unread?: boolean }
): MessageRead {
  const root = message(body, { created_at: createdAt, id });
  return {
    message: root,
    unread: reply?.unread,
    replies:
      reply === undefined
        ? []
        : [
            message(reply.body, {
              author: { id: "planner-session", kind: "session" },
              created_at: reply.createdAt,
              id: `${id}-reply`,
              in_reply_to: id,
            }),
          ],
  };
}

test("Agents shows only the newest exchange and folds the rest behind Show N older", async () => {
  const page = renderAgents({
    messages: [
      exchange("m3", "Third question", "2026-09-14T03:00:00Z", {
        body: "Third answer",
        createdAt: "2026-09-14T03:01:00Z",
      }),
      exchange("m2", "Second question", "2026-09-14T02:00:00Z"),
      exchange("m1", "First question", "2026-09-14T01:00:00Z"),
    ],
  });

  try {
    const planner = card(await screen.findByRole("region", { name: "Agents" }), "Planner");
    expand(planner, "Planner");
    const conversation = await within(planner).findByRole("list", {
      name: "Conversation with Planner",
    });
    await expect(within(conversation).findByText("Third question")).resolves.toBeTruthy();
    expect(within(conversation).getByText("Third answer")).toBeTruthy();
    expect(within(conversation).queryByText("Second question")).toBeNull();
    expect(within(conversation).queryByText("First question")).toBeNull();
    const older = within(planner).getByRole("button", { name: "Show 2 older" });
    expect(older.getAttribute("aria-expanded")).toBe("false");

    fireEvent.click(older);
    expect(older.getAttribute("aria-expanded")).toBe("true");
    // Newest first, the fold's rows beneath the newest exchange in the same list.
    await waitFor(() =>
      expect(
        within(conversation)
          .getAllByText(/question$/, { selector: ".dispatch-markdown" })
          .map((node) => node.textContent)
      ).toEqual(["Third question", "Second question", "First question"])
    );

    fireEvent.click(older);
    expect(within(conversation).queryByText("Second question")).toBeNull();
    expect(within(conversation).getByText("Third question")).toBeTruthy();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// An agent answering a human's direct message is news the human did not go looking for: the
// count shows in the navigation and on the agent's row until the conversation is opened, and
// opening it records how far it was read, so the count stays gone on every device.
test("an agent's unread reply shows on its row and in the navigation until its conversation is opened", async () => {
  const page = renderAgents({
    agentState: { "planner-session": { unread_replies: 1 } },
    messages: [
      exchange("m1", "Where is the dashboard?", "2026-09-14T01:00:00Z", {
        body: "At /dash.",
        createdAt: "2026-09-14T01:05:00Z",
      }),
    ],
  });

  try {
    const planner = card(await screen.findByRole("region", { name: "Agents" }), "Planner");
    // The test viewport is compact, so the shell's header carries the badge the rail would.
    await expect(screen.findByRole("link", { name: "New reply 1" })).resolves.toBeTruthy();
    expect(within(planner).queryByText("At /dash.")).toBeNull();

    fireEvent.click(
      await within(planner).findByRole("button", { name: "Planner replied: 1 unread" })
    );
    await expect(within(planner).findByText("At /dash.")).resolves.toBeTruthy();
    await waitFor(() =>
      expect(page.putAgentState).toHaveBeenCalledWith("planner-session", {
        read_through: "2026-09-14T01:05:00Z",
      })
    );
    await waitFor(() =>
      expect(within(planner).queryByRole("button", { name: /unread/ })).toBeNull()
    );
    expect(screen.queryByRole("link", { name: /^New repl/ })).toBeNull();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// The count sums every session that answered the viewer, and a session often answers and then
// exits. A reply from a session no longer in the live list still has a row, whose Open reads it,
// so the badge is always one the viewer can clear.
test("a reply from a session that has ended keeps a row that opens its conversation", async () => {
  const ended = "01a0e52e-0000-7000-8000-00000000abcd";
  const page = renderAgents({
    agentState: { [ended]: { unread_replies: 1 } },
    listedAgents: [],
  });

  try {
    await expect(screen.findByRole("link", { name: "New reply 1" })).resolves.toBeTruthy();
    const region = await screen.findByRole("region", { name: "Replied, no longer connected" });
    const open = within(region).getByRole("link", { name: "Open session:01a0e52e…" });
    expect(open.getAttribute("href")).toBe(`/agents/${ended}/live`);
    expect(within(region).getByText("New reply 1")).toBeTruthy();
    expect(screen.getByLabelText("Agents empty state")).toBeTruthy();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// Opening a row marks the conversation read through its newest reply, and a read mark covers
// every older reply too. So each exchange holding an unread reply is shown when the row opens,
// not folded behind "Show N older" where the viewer would never see what was just marked read.
test("opening a row shows an older exchange's unread follow-up instead of folding it", async () => {
  const page = renderAgents({
    agentState: { "planner-session": { unread_replies: 1 } },
    messages: [
      exchange("m3", "Third question", "2026-09-14T03:00:00Z"),
      exchange("m2", "Second question", "2026-09-14T02:00:00Z"),
      // The server's flag, not the timestamps, is what the row renders: this exchange's reply is
      // the oldest one on screen and still the unread one.
      exchange("m1", "First question", "2026-09-14T01:00:00Z", {
        body: "First answer, followed up",
        createdAt: "2026-09-14T00:30:00Z",
        unread: true,
      }),
    ],
  });

  try {
    const planner = card(await screen.findByRole("region", { name: "Agents" }), "Planner");
    fireEvent.click(
      await within(planner).findByRole("button", { name: "Planner replied: 1 unread" })
    );
    await expect(within(planner).findByText("First answer, followed up")).resolves.toBeTruthy();
    expect(within(planner).getByText("Third question")).toBeTruthy();
    expect(within(planner).queryByText("Second question")).toBeNull();
    expect(within(planner).getByRole("button", { name: "Show 1 older" })).toBeTruthy();
    await waitFor(() =>
      expect(page.putAgentState).toHaveBeenCalledWith("planner-session", {
        read_through: "2026-09-14T00:30:00Z",
      })
    );
    await waitFor(() =>
      expect(within(planner).queryByRole("button", { name: /unread/ })).toBeNull()
    );
    // Read now, and still on screen: marking it read does not fold it away again.
    expect(within(planner).getByText("First answer, followed up")).toBeTruthy();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// The server decides which conversations hold an unread reply; the row renders that verdict and
// derives nothing from timestamps. This fixture disagrees with the clock in both directions: the
// flagged exchange's reply is older than the read mark, and the unflagged one's is newer. Both
// directions are load-bearing - a fixture that disagreed one way only would still pass against a
// client that re-derived the rule from read_through and cleared_before.
test("Agents shows the exchanges the server flags unread, whatever their timestamps say", async () => {
  const page = renderAgents({
    agentState: {
      "planner-session": { read_through: "2026-09-14T03:00:00Z", unread_replies: 1 },
    },
    messages: [
      exchange("m3", "Newest question", "2026-09-14T05:00:00Z"),
      exchange("m2", "Answered after the mark", "2026-09-14T02:00:00Z", {
        body: "Reply the server calls read",
        createdAt: "2026-09-14T04:00:00Z",
      }),
      exchange("m1", "Answered before the mark", "2026-09-14T01:00:00Z", {
        body: "Reply the server calls unread",
        createdAt: "2026-09-14T01:30:00Z",
        unread: true,
      }),
    ],
  });

  try {
    const planner = card(await screen.findByRole("region", { name: "Agents" }), "Planner");
    expand(planner, "Planner");
    await expect(within(planner).findByText("Reply the server calls unread")).resolves.toBeTruthy();
    expect(within(planner).queryByText("Reply the server calls read")).toBeNull();
    expect(within(planner).getByRole("button", { name: "Show 1 older" })).toBeTruthy();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("Agents renders no fold for a single exchange", async () => {
  const page = renderAgents({
    messages: [exchange("m1", "Only question", "2026-09-14T01:00:00Z")],
  });

  try {
    const planner = card(await screen.findByRole("region", { name: "Agents" }), "Planner");
    expand(planner, "Planner");
    await expect(within(planner).findByText("Only question")).resolves.toBeTruthy();
    expect(within(planner).queryByRole("button", { name: /older$/ })).toBeNull();
    expect(within(planner).getByRole("button", { name: "Clear conversation" })).toBeTruthy();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("Agents Clear hides every exchange up to now for this viewer and persists the cutoff", async () => {
  const page = renderAgents({
    messages: [
      exchange("m2", "Second question", "2026-09-14T02:00:00Z"),
      exchange("m1", "First question", "2026-09-14T01:00:00Z"),
    ],
  });

  try {
    const planner = card(await screen.findByRole("region", { name: "Agents" }), "Planner");
    expand(planner, "Planner");
    await expect(within(planner).findByText("Second question")).resolves.toBeTruthy();
    fireEvent.click(within(planner).getByRole("button", { name: "Clear conversation" }));
    // The cutoff is the newest visible message's own timestamp, so the Clear hides exactly what
    // was on screen whatever the browser clock says.
    await waitFor(() =>
      expect(page.putAgentState).toHaveBeenCalledWith("planner-session", {
        cleared_before: "2026-09-14T02:00:00Z",
      })
    );

    await waitFor(() =>
      expect(within(planner).queryByRole("list", { name: "Conversation with Planner" })).toBeNull()
    );
    expect(within(planner).queryByText("Second question")).toBeNull();
    expect(within(planner).queryByRole("button", { name: "Clear conversation" })).toBeNull();
    const cleared = within(planner).getByText(/^Cleared/);
    expect(cleared.querySelector("time")?.getAttribute("datetime")).toBe("2026-09-14T02:00:00Z");
    const showAnyway = within(planner).getByRole("button", { name: "Show anyway" });

    // Looking back does not touch the cutoff; the whole history is there, folded as usual.
    fireEvent.click(showAnyway);
    const conversation = within(planner).getByRole("list", { name: "Conversation with Planner" });
    await expect(within(conversation).findByText("Second question")).resolves.toBeTruthy();
    expect(within(planner).getByRole("button", { name: "Show 1 older" })).toBeTruthy();
    expect(page.putAgentState).toHaveBeenCalledTimes(1);
    fireEvent.click(within(planner).getByRole("button", { name: "Hide again" }));
    expect(within(planner).queryByText("Second question")).toBeNull();
    // The composer stays: a Clear is about reading, not sending.
    expect(within(planner).getByRole("textbox", { name: "Comment" })).toBeTruthy();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("Agents keeps exchanges with activity after the persisted cutoff and hides the rest", async () => {
  const page = renderAgents({
    agentState: {
      // The viewer has read through the late answer, so nothing is unread and the fold is the
      // Clear's alone.
      "planner-session": {
        cleared_before: "2026-09-14T12:00:00Z",
        read_through: "2026-09-14T13:00:00Z",
        unread_replies: 0,
      },
    },
    messages: [
      exchange("m3", "New question", "2026-09-15T00:00:00Z"),
      // Asked before the Clear, answered after it: the answer is fresh, so the exchange shows.
      exchange("m2", "Pending question", "2026-09-14T02:00:00Z", {
        body: "Late answer",
        createdAt: "2026-09-14T13:00:00Z",
      }),
      exchange("m1", "Old question", "2026-09-14T01:00:00Z", {
        body: "Old answer",
        createdAt: "2026-09-14T01:01:00Z",
      }),
    ],
  });

  try {
    const planner = card(await screen.findByRole("region", { name: "Agents" }), "Planner");
    expand(planner, "Planner");
    const conversation = await within(planner).findByRole("list", {
      name: "Conversation with Planner",
    });
    await expect(within(conversation).findByText("New question")).resolves.toBeTruthy();
    expect(within(planner).queryByText("Old question")).toBeNull();
    expect(within(planner).getByText(/^Cleared/)).toBeTruthy();
    // The fold counts only what the viewer has not cleared.
    fireEvent.click(within(planner).getByRole("button", { name: "Show 1 older" }));
    await expect(within(conversation).findByText("Late answer")).resolves.toBeTruthy();
    expect(within(planner).queryByText("Old question")).toBeNull();

    fireEvent.click(within(planner).getByRole("button", { name: "Show anyway" }));
    await expect(within(conversation).findByText("Old question")).resolves.toBeTruthy();
    expect(page.putAgentState).not.toHaveBeenCalled();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// The cutoff and the answers carry microseconds: an answer 44 µs after the Clear, in the same
// millisecond, is news and stays, and the answer at the Clear itself is cleared.
test("an exchange answered within the Clear's millisecond but after it stays visible", async () => {
  const page = renderAgents({
    agentState: {
      "planner-session": {
        cleared_before: "2026-09-14T03:00:00.123456Z",
        read_through: "2026-09-14T03:00:00.1235Z",
        unread_replies: 0,
      },
    },
    messages: [
      exchange("m2", "Pending question", "2026-09-14T02:00:00Z", {
        body: "Answer after the Clear",
        createdAt: "2026-09-14T03:00:00.1235Z",
      }),
      exchange("m1", "Cleared question", "2026-09-14T01:00:00Z", {
        body: "Answer at the Clear",
        createdAt: "2026-09-14T03:00:00.123456Z",
      }),
    ],
  });

  try {
    const planner = card(await screen.findByRole("region", { name: "Agents" }), "Planner");
    expand(planner, "Planner");
    const conversation = await within(planner).findByRole("list", {
      name: "Conversation with Planner",
    });
    await expect(within(conversation).findByText("Answer after the Clear")).resolves.toBeTruthy();
    expect(within(planner).queryByText("Cleared question")).toBeNull();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("Agents replies to an issue-less exchange through the agent route, threaded under the answer", async () => {
  const root = message("Can this ship?", {
    deliveries: [
      {
        attempt: 1,
        created_at: recentAttemptAt,
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
    const planner = card(await screen.findByRole("region", { name: "Agents" }), "Planner");
    expand(planner, "Planner");
    const conversation = await within(planner).findByRole("list", {
      name: "Conversation with Planner",
    });
    const replies = await within(conversation).findByRole("list", { name: "Replies" });
    const answer = within(replies).getByText("Yes, it can.").closest("li");
    if (answer === null) throw new Error("answer turn missing");
    expect(within(planner).getByRole("button", { name: "Choose issue" })).toBeTruthy();

    fireEvent.click(within(answer).getByRole("button", { name: "Reply" }));
    const composer = within(planner).getByRole("form", { name: "Comment composer" });
    expect(within(composer).getByText("Replying to Planner — Yes, it can.")).toBeTruthy();
    // A reply lives in its parent's direct-session conversation; it hides the issue picker.
    expect(within(planner).queryByRole("button", { name: "Choose issue" })).toBeNull();
    fireEvent.change(within(planner).getByRole("textbox", { name: "Comment" }), {
      target: { value: "Ship it." },
    });
    fireEvent.submit(composer);
    await waitFor(() =>
      expect(page.createAgentMessage).toHaveBeenCalledWith("planner-session", {
        body: "Ship it.",
        delivery: "btw",
        in_reply_to: "message-2",
      })
    );
    await waitFor(() =>
      expect(within(planner).queryByText("Replying to Planner — Yes, it can.")).toBeNull()
    );
    expect(within(planner).getByRole("button", { name: "Choose issue" })).toBeTruthy();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// The server's times carry microseconds, and `Date.parse` keeps milliseconds: it reads
// `…00.123456Z` and `…00.1235Z` as one time. The read mark and the Clear cutoff still name the
// newer, or a reply 44 µs after the mark would stay unread and a Clear would stop short of it.
test("Agents marks read and clears through the newest reply even within one millisecond", async () => {
  const page = renderAgents({
    agentState: { "planner-session": { unread_replies: 1 } },
    messages: [
      exchange("m2", "Second question", "2026-09-14T02:00:00Z", {
        body: "Second answer",
        createdAt: "2026-09-14T03:00:00.123456Z",
      }),
      exchange("m1", "First question", "2026-09-14T01:00:00Z", {
        body: "First answer",
        createdAt: "2026-09-14T03:00:00.1235Z",
        unread: true,
      }),
    ],
  });

  try {
    const planner = card(await screen.findByRole("region", { name: "Agents" }), "Planner");
    fireEvent.click(
      await within(planner).findByRole("button", { name: "Planner replied: 1 unread" })
    );
    await expect(within(planner).findByText("First answer")).resolves.toBeTruthy();
    await waitFor(() =>
      expect(page.putAgentState).toHaveBeenCalledWith("planner-session", {
        read_through: "2026-09-14T03:00:00.1235Z",
      })
    );

    fireEvent.click(within(planner).getByRole("button", { name: "Clear conversation" }));
    await waitFor(() =>
      expect(page.putAgentState).toHaveBeenCalledWith("planner-session", {
        cleared_before: "2026-09-14T03:00:00.1235Z",
      })
    );
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("a reply inherits the mode of the exchange's newest attempt even within one millisecond", async () => {
  const attempt = (
    delivery: MessageDeliveryMode,
    createdAt: string,
    number: number
  ): MessageDelivery => ({
    attempt: number,
    created_at: createdAt,
    delivery,
    envelope_id: `envelope-${number}`,
    error: null,
    message_id: "message-1",
    reply_id: "message-2",
    session_id: "planner-session",
    state: "sent",
  });
  const root = message("Can this ship?", {
    // The newer attempt is listed first, so an order to the millisecond keeps the older's mode.
    deliveries: [
      attempt("btw", "2026-09-14T00:00:01.1235Z", 2),
      attempt("aside", "2026-09-14T00:00:01.123456Z", 1),
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
    const planner = card(await screen.findByRole("region", { name: "Agents" }), "Planner");
    expand(planner, "Planner");
    const conversation = await within(planner).findByRole("list", {
      name: "Conversation with Planner",
    });
    const replies = await within(conversation).findByRole("list", { name: "Replies" });
    const answer = within(replies).getByText("Yes, it can.").closest("li");
    if (answer === null) throw new Error("answer turn missing");
    fireEvent.click(within(answer).getByRole("button", { name: "Reply" }));
    fireEvent.change(within(planner).getByRole("textbox", { name: "Comment" }), {
      target: { value: "Ship it." },
    });
    fireEvent.submit(within(planner).getByRole("form", { name: "Comment composer" }));
    await waitFor(() =>
      expect(page.createAgentMessage).toHaveBeenCalledWith("planner-session", {
        body: "Ship it.",
        delivery: "btw",
        in_reply_to: "message-2",
      })
    );
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("a root targeted-message retry on the Agents page checks the role's current holder after a handoff", async () => {
  const root = message("Can this ship?", {
    target: "role:reviewer",
    deliveries: [
      {
        attempt: 1,
        created_at: recentAttemptAt,
        delivery: "steer",
        envelope_id: null,
        error: "no live session old-reviewer-session",
        message_id: "message-1",
        reply_id: null,
        session_id: "old-reviewer-session",
        state: "failed",
      },
    ],
  });
  const newReviewer: Agent = {
    capabilities: ["btw"],
    dir: "/workspaces/reviewer",
    last_activity: new Date(now - 60_000).toISOString(),
    last_seen: now - 30_000,
    machine_id: "review-host",
    open_asks: 0,
    roles: ["reviewer"],
    session_id: "new-reviewer-session",
    title: "New reviewer",
  };
  const page = renderAgents({
    listedAgents: [newReviewer],
    messages: [{ message: root, replies: [] }],
  });

  try {
    const reviewerCard = card(
      await screen.findByRole("region", { name: "Agents" }),
      "New reviewer"
    );
    expand(reviewerCard, "New reviewer");
    // The new holder dropped steer, so the same-mode Retry of a steer attempt is refused.
    const sendNormally = await within(reviewerCard).findByRole("button", { name: "Retry" });
    expect(sendNormally.hasAttribute("disabled")).toBe(true);
    expect(
      within(reviewerCard).getByRole("button", { name: "Use BTW instead" }).hasAttribute("disabled")
    ).toBe(false);
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("a reply's targeted-message retry on the Agents page checks the thread's current role holder after a handoff", async () => {
  const root = message("Can this ship?", {
    target: "role:reviewer",
    deliveries: [
      {
        attempt: 1,
        created_at: recentAttemptAt,
        delivery: "steer",
        envelope_id: "envelope-1",
        error: null,
        message_id: "message-1",
        reply_id: null,
        session_id: "old-reviewer-session",
        state: "sent",
      },
    ],
  });
  // A session answer marks the root "answered," suppressing its own retry row so only the
  // reply's retry row is under test below.
  const sessionAnswer = message("Looking into it.", {
    author: { id: "old-reviewer-session", kind: "session" },
    id: "message-2",
    in_reply_to: root.id,
  });
  const userReply = message("Any update?", {
    author: { id: "alice", kind: "user" },
    deliveries: [
      {
        attempt: 1,
        created_at: recentAttemptAt,
        delivery: "steer",
        envelope_id: null,
        error: "no live session old-reviewer-session",
        message_id: "message-3",
        reply_id: null,
        session_id: "old-reviewer-session",
        state: "failed",
      },
    ],
    id: "message-3",
    in_reply_to: root.id,
  });
  const newReviewer: Agent = {
    capabilities: ["btw"],
    dir: "/workspaces/reviewer",
    last_activity: new Date(now - 60_000).toISOString(),
    last_seen: now - 30_000,
    machine_id: "review-host",
    open_asks: 0,
    roles: ["reviewer"],
    session_id: "new-reviewer-session",
    title: "New reviewer",
  };
  const page = renderAgents({
    listedAgents: [newReviewer],
    messages: [{ message: root, replies: [sessionAnswer, userReply] }],
  });

  try {
    const reviewerCard = card(
      await screen.findByRole("region", { name: "Agents" }),
      "New reviewer"
    );
    expand(reviewerCard, "New reviewer");
    // The new holder dropped steer, so the same-mode Retry of a steer attempt is refused.
    const sendNormally = await within(reviewerCard).findByRole("button", { name: "Retry" });
    expect(sendNormally.hasAttribute("disabled")).toBe(true);
    expect(
      within(reviewerCard).getByRole("button", { name: "Use BTW instead" }).hasAttribute("disabled")
    ).toBe(false);
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("a broadcast leaves out a selected agent that does not advertise the chosen mode", async () => {
  const page = renderAgents();

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    fireEvent.click(within(region).getByRole("checkbox", { name: "Select Planner for broadcast" }));
    fireEvent.click(
      within(region).getByRole("checkbox", { name: "Select Reviewer for broadcast" })
    );
    const broadcast = within(region).getByRole("region", { name: "Broadcast" });
    // The Reviewer advertises `aside` only, so BTW reaches the Planner alone.
    fireEvent.change(within(broadcast).getByRole("combobox", { name: "Delivery mode" }), {
      target: { value: "btw" },
    });
    expect(
      within(broadcast).getByText(/Excluded: Reviewer \(does not advertise BTW\)/)
    ).toBeTruthy();
    fireEvent.change(within(broadcast).getByRole("textbox", { name: "Broadcast message" }), {
      target: { value: "Stand down and report status." },
    });
    fireEvent.click(within(broadcast).getByRole("button", { name: "Send to 1" }));
    await waitFor(() =>
      expect(page.createBroadcast).toHaveBeenCalledWith({
        body: "Stand down and report status.",
        delivery: "btw",
        idempotency_key: expect.any(String),
        session_ids: ["planner-session"],
      })
    );
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("a broadcast composer labels a mode-mismatched recipient", () => {
  const [, reviewer] = agents;
  const composer = broadcastPlan(new Set([reviewer.session_id]), [reviewer], "btw");

  // `packages/dispatch/AGENTS.md:36-41` and `features/conversation/delivery.ts:8-12` name this
  // composer-facing label.
  expect(composer.excluded[0]?.reason).toBe("does not advertise BTW");
});

test("a mode both recipients advertise takes the excluded one back in", async () => {
  const page = renderAgents();

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    fireEvent.click(within(region).getByRole("checkbox", { name: "Select Planner for broadcast" }));
    fireEvent.click(
      within(region).getByRole("checkbox", { name: "Select Reviewer for broadcast" })
    );
    const broadcast = within(region).getByRole("region", { name: "Broadcast" });
    fireEvent.change(within(broadcast).getByRole("combobox", { name: "Delivery mode" }), {
      target: { value: "aside" },
    });
    expect(within(broadcast).queryByText(/Excluded:/)).toBeNull();
    expect(within(broadcast).getByRole("button", { name: "Send to 2" })).toBeTruthy();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("Restore draft refuses, and says why on screen, while the composer holds a message or a selection started since the refused send", async () => {
  const page = renderAgents();
  page.createBroadcast.mockImplementationOnce(async () => {
    throw new Error("Envoy listener unreachable");
  });
  const reason =
    "Restore draft would replace the broadcast you have started. Send it, or clear its message and selection, first.";

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    const planner = within(region).getByRole("checkbox", { name: "Select Planner for broadcast" });
    const message = () =>
      within(within(region).getByRole("region", { name: "Broadcast" })).getByRole("textbox", {
        name: "Broadcast message",
      });
    fireEvent.click(planner);
    fireEvent.change(message(), { target: { value: "Keep this." } });
    fireEvent.click(screen.getByRole("button", { name: "Send to 1" }));
    const sends = await screen.findByRole("region", { name: "Sends" });
    await within(sends).findByText("Could not send to 1 agent: Envoy listener unreachable");
    const restore = within(sends).getByRole("button", { name: "Restore draft" });
    expect(restore.getAttribute("aria-disabled")).toBeNull();

    // A message started since the press: Restore draft would overwrite it, so it refuses, and the
    // line under its row, which it is described by, says why.
    fireEvent.click(planner);
    fireEvent.change(message(), { target: { value: "Started since." } });
    expect(restore.getAttribute("aria-disabled")).toBe("true");
    expect(restore.getAttribute("title")).toBe(reason);
    const describedBy = restore.getAttribute("aria-describedby") ?? "";
    expect(document.getElementById(describedBy)?.textContent).toBe(reason);
    expect(within(sends).getByText(reason)).toBeTruthy();
    fireEvent.click(restore);
    expect(message()).toHaveProperty("value", "Started since.");
    expect(within(sends).getByText(/^Could not send to 1 agent/)).toBeTruthy();

    // An empty message with agents ticked since is a broadcast begun too: Restore draft would
    // replace the selection, so it still refuses.
    fireEvent.change(message(), { target: { value: "" } });
    expect(restore.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(restore);
    expect(planner).toHaveProperty("checked", true);
    expect(message()).toHaveProperty("value", "");

    // With nothing begun, the refused send's message and selection come back.
    fireEvent.click(planner);
    expect(restore.getAttribute("aria-disabled")).toBeNull();
    expect(within(sends).queryByText(reason)).toBeNull();
    fireEvent.click(restore);
    expect(message()).toHaveProperty("value", "Keep this.");
    expect(planner).toHaveProperty("checked", true);
    expect(screen.queryByRole("region", { name: "Sends" })).toBeNull();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

/** The page's `api.createBroadcast` spy, as far as reading back what it was handed. */
interface BroadcastSpy {
  readonly createBroadcast: {
    readonly mock: { readonly calls: readonly (readonly [CreateBroadcastInput])[] };
  };
}

/** The i-th request the page handed `api.createBroadcast`. */
function posted(page: BroadcastSpy, index: number): CreateBroadcastInput {
  const call = page.createBroadcast.mock.calls[index];
  if (call === undefined) throw new Error(`no broadcast request ${index}`);
  return call[0];
}

// LEGION-446. A refused send's row is the only copy of its message, and Restore draft is how the
// human sends it again. Until something is edited the request goes out word for word, key
// included, so a send that did land behind the refusal is answered as the repeat it is; the first
// edit makes it a new composition, under a new key.
test("Restore draft re-sends the refused request word for word, and the first edit drops it for a new send", async () => {
  const page = renderAgents();
  const refuse = async () => {
    throw new Error("Envoy listener unreachable");
  };
  page.createBroadcast.mockImplementationOnce(refuse).mockImplementationOnce(refuse);

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    const message = () =>
      within(within(region).getByRole("region", { name: "Broadcast" })).getByRole("textbox", {
        name: "Broadcast message",
      });
    const restoreDraft = async (failures: number) => {
      const sends = await screen.findByRole("region", { name: "Sends" });
      await waitFor(() =>
        expect(
          within(sends).getAllByText("Could not send to 1 agent: Envoy listener unreachable")
        ).toHaveLength(failures)
      );
      fireEvent.click(within(sends).getByRole("button", { name: "Restore draft" }));
    };
    fireEvent.click(within(region).getByRole("checkbox", { name: "Select Planner for broadcast" }));
    fireEvent.change(message(), { target: { value: "Keep this." } });
    fireEvent.click(screen.getByRole("button", { name: "Send to 1" }));
    await waitFor(() => expect(page.createBroadcast).toHaveBeenCalledTimes(1));
    const first = posted(page, 0);
    expect(first.idempotency_key).not.toBe("");

    await restoreDraft(1);
    fireEvent.click(screen.getByRole("button", { name: "Send to 1" }));
    await waitFor(() => expect(page.createBroadcast).toHaveBeenCalledTimes(2));
    expect(posted(page, 1)).toEqual(first);

    await restoreDraft(1);
    fireEvent.change(message(), { target: { value: "Keep this!" } });
    fireEvent.click(screen.getByRole("button", { name: "Send to 1" }));
    await waitFor(() => expect(page.createBroadcast).toHaveBeenCalledTimes(3));
    expect(posted(page, 2).idempotency_key).not.toBe(first.idempotency_key);
    expect(posted(page, 2).body).toBe("Keep this!");
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("a selected agent that went away does not change what Restore draft re-sends, and the composer says so", async () => {
  const [planner, reviewer] = agents;
  const listening = { ...reviewer, capabilities: ["aside", "btw"] };
  const page = renderAgents({ listedAgents: [planner, listening] });
  page.createBroadcast.mockImplementationOnce(async () => {
    throw new Error("Envoy listener unreachable");
  });

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    fireEvent.click(within(region).getByRole("checkbox", { name: "Select Planner for broadcast" }));
    fireEvent.click(
      within(region).getByRole("checkbox", { name: "Select Reviewer for broadcast" })
    );
    const composer = () => within(region).getByRole("region", { name: "Broadcast" });
    fireEvent.change(within(composer()).getByRole("combobox", { name: "Delivery mode" }), {
      target: { value: "btw" },
    });
    fireEvent.change(within(composer()).getByRole("textbox", { name: "Broadcast message" }), {
      target: { value: "Still here?" },
    });
    fireEvent.click(within(composer()).getByRole("button", { name: "Send to 2" }));
    await waitFor(() => expect(page.createBroadcast).toHaveBeenCalledTimes(1));
    expect(posted(page, 0).session_ids).toEqual(["planner-session", "reviewer-session"]);

    page.listAgents.mockResolvedValue([planner]);
    await page.queryClient.refetchQueries({ queryKey: ["agents"] });
    await waitFor(() =>
      expect(
        within(region).queryByRole("checkbox", { name: "Select Reviewer for broadcast" })
      ).toBeNull()
    );
    const sends = await screen.findByRole("region", { name: "Sends" });
    await within(sends).findByText("Could not send to 2 agents: Envoy listener unreachable");
    fireEvent.click(within(sends).getByRole("button", { name: "Restore draft" }));

    // The restored request names both, so the composer counts both; the Reviewer's chip falls
    // back to its session id, with no reason, since the request asks for it.
    expect(within(composer()).getByRole("heading", { level: 2 }).textContent).toBe(
      "Broadcast to 2 of 2 selected"
    );
    expect(
      within(composer()).getByTitle("Remove session:reviewer… from this broadcast").textContent
    ).toBe("session:reviewer… ✕");
    fireEvent.click(within(composer()).getByRole("button", { name: "Send to 2" }));
    await waitFor(() => expect(page.createBroadcast).toHaveBeenCalledTimes(2));
    expect(posted(page, 1)).toEqual(posted(page, 0));
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// The security review's case the other way round: an agent the refused send could not reach is
// back by the restore. The request being re-sent does not name it, so the composer says so rather
// than show it as reached; ticking it back in is an edit, which composes a new send.
test("a selected agent that came back is shown as not in the restored send, and the request stays as refused", async () => {
  const [planner, reviewer] = agents;
  const page = renderAgents();
  const refuse = async () => {
    throw new Error("Envoy listener unreachable");
  };
  page.createBroadcast.mockImplementationOnce(refuse).mockImplementationOnce(refuse);
  const notInTheSend =
    "Excluded: Reviewer (not in the refused send; edit to include it). Nothing is sent to them, and no other mode is substituted.";

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    const composer = () => within(region).getByRole("region", { name: "Broadcast" });
    const restoreDraft = async (failures: number) => {
      const sends = await screen.findByRole("region", { name: "Sends" });
      await waitFor(() =>
        expect(
          within(sends).getAllByText("Could not send to 1 agent: Envoy listener unreachable")
        ).toHaveLength(failures)
      );
      fireEvent.click(within(sends).getByRole("button", { name: "Restore draft" }));
    };
    fireEvent.click(within(region).getByRole("checkbox", { name: "Select Planner for broadcast" }));
    fireEvent.click(
      within(region).getByRole("checkbox", { name: "Select Reviewer for broadcast" })
    );
    fireEvent.change(within(composer()).getByRole("combobox", { name: "Delivery mode" }), {
      target: { value: "btw" },
    });
    fireEvent.change(within(composer()).getByRole("textbox", { name: "Broadcast message" }), {
      target: { value: "Still here?" },
    });
    expect(
      within(composer()).getByText(/Excluded: Reviewer \(does not advertise BTW\)/)
    ).toBeTruthy();
    fireEvent.click(within(composer()).getByRole("button", { name: "Send to 1" }));
    await waitFor(() => expect(page.createBroadcast).toHaveBeenCalledTimes(1));
    const first = posted(page, 0);
    expect(first.session_ids).toEqual(["planner-session"]);

    // The Reviewer now takes BTW: the live plan would reach it, the refused request does not.
    page.listAgents.mockResolvedValue([planner, { ...reviewer, capabilities: ["aside", "btw"] }]);
    await page.queryClient.refetchQueries({ queryKey: ["agents"] });
    await restoreDraft(1);
    expect(within(composer()).getByRole("heading", { level: 2 }).textContent).toBe(
      "Broadcast to 1 of 2 selected"
    );
    expect(within(composer()).getByText(notInTheSend)).toBeTruthy();
    fireEvent.click(within(composer()).getByRole("button", { name: "Send to 1" }));
    await waitFor(() => expect(page.createBroadcast).toHaveBeenCalledTimes(2));
    expect(posted(page, 1)).toEqual(first);

    // Taking the Reviewer out and ticking it back in is an edit: the live plan takes over, and
    // the send that follows is a new composition naming both, under a key of its own.
    await restoreDraft(1);
    fireEvent.click(within(composer()).getByTitle("Remove Reviewer from this broadcast"));
    fireEvent.click(
      within(region).getByRole("checkbox", { name: "Select Reviewer for broadcast" })
    );
    expect(within(composer()).queryByText(/Excluded:/)).toBeNull();
    fireEvent.click(within(composer()).getByRole("button", { name: "Send to 2" }));
    await waitFor(() => expect(page.createBroadcast).toHaveBeenCalledTimes(3));
    expect(posted(page, 2).session_ids).toEqual(["planner-session", "reviewer-session"]);
    expect(posted(page, 2).idempotency_key).not.toBe(first.idempotency_key);
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// The security review's minor: only a session an edit would bring in is "not in the refused send";
// one the live plan still leaves out keeps the reason the live plan gives.
test("a restored send names a selected session it leaves out by the reason an edit would keep, unless it has come back", () => {
  const [planner, reviewer] = agents;
  const restored = {
    body: "Still here?",
    delivery: "btw",
    idempotency_key: "refused-key",
    session_ids: ["planner-session"],
  } satisfies CreateBroadcastInput;
  const composition = {
    delivery: "btw",
    draft: "Still here?",
    restored,
    selected: new Set(["planner-session", "reviewer-session"]),
    sendKey: "next-key",
  } as const;
  const reasons = (live: readonly Agent[]) =>
    composedBroadcast(composition, live).plan.excluded.map((item) => item.reason);

  expect(reasons([planner, reviewer])).toEqual(["does not advertise BTW"]);
  expect(reasons([planner])).toEqual(["no live session"]);
  expect(reasons([planner, { ...reviewer, capabilities: ["aside", "btw"] }])).toEqual([
    "not in the refused send; edit to include it",
  ]);
  expect(composedBroadcast(composition, [planner, reviewer]).input).toBe(restored);
});

// Two deliberate sends of the same words are two broadcasts: each composition carries a key of its
// own, so the server never answers the second with the first.
test("each composed send carries a key of its own, even when it says the same thing", async () => {
  const page = renderAgents();
  // Refused, so each row stays on the strip and the page does not open a broadcast in between.
  page.createBroadcast.mockImplementation(async () => {
    throw new Error("Envoy listener unreachable");
  });

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    for (const count of [1, 2, 3]) {
      fireEvent.click(
        within(region).getByRole("checkbox", { name: "Select Planner for broadcast" })
      );
      fireEvent.change(within(region).getByRole("textbox", { name: "Broadcast message" }), {
        target: { value: "Status?" },
      });
      fireEvent.click(within(region).getByRole("button", { name: "Send to 1" }));
      await waitFor(() => expect(page.createBroadcast).toHaveBeenCalledTimes(count));
    }
    const keys = [0, 1, 2].map((index) => posted(page, index).idempotency_key);
    expect(new Set(keys).size).toBe(3);
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// TanStack resumes a paused mutation only while the tab is visible (`focusManager`), so a queue
// built on a mutation `scope` would hold the second send until the reader came back to the tab.
test("a queued broadcast goes out as soon as the one ahead is answered, with the tab hidden by then", async () => {
  const page = renderAgents();
  const held = Promise.withResolvers<void>();
  page.createBroadcast.mockImplementationOnce(async (input) => {
    await held.promise;
    return {
      author: { id: "alice", kind: "user" },
      body: input.body,
      created_at: "2026-09-14T00:00:00Z",
      delivery: input.delivery,
      excluded: [],
      id: "broadcast-held",
      recipients: [],
    };
  });

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    const planner = within(region).getByRole("checkbox", { name: "Select Planner for broadcast" });
    // Each press in a task of its own: the queue drops a second press in the same task.
    for (const body of ["First.", "Second."]) {
      fireEvent.click(planner);
      fireEvent.change(within(region).getByRole("textbox", { name: "Broadcast message" }), {
        target: { value: body },
      });
      fireEvent.click(within(region).getByRole("button", { name: "Send to 1" }));
      await screen.findByTitle(body);
    }
    const sends = screen.getByRole("region", { name: "Sends" });
    expect(within(sends).getByText("Queued: to 1 agent")).toBeTruthy();
    expect(page.createBroadcast).toHaveBeenCalledTimes(1);

    focusManager.setFocused(false);
    held.resolve();
    await waitFor(() => expect(page.createBroadcast).toHaveBeenCalledTimes(2));
    expect(page.createBroadcast.mock.calls.map(([input]) => input.body)).toEqual([
      "First.",
      "Second.",
    ]);
  } finally {
    // `lastRequest` is module state: a held request left unresolved would hold every later
    // broadcast in this file behind it.
    held.resolve();
    focusManager.setFocused(undefined);
    page.view.unmount();
    page.restore();
  }
});

test("Send says which of its reasons stops it, where that reason is shown, and the mode that would reach a selection this one reaches none of", () => {
  const [planner, reviewer] = agents;
  const deaf = { ...reviewer, capabilities: [], session_id: "deaf-session", title: "Deaf" };
  const state = (
    selected: readonly string[],
    live: readonly Agent[],
    delivery: MessageDeliveryMode,
    body = "Report status."
  ) => broadcastSendState(broadcastPlan(new Set(selected), live, delivery), delivery, body);
  const both = [planner.session_id, reviewer.session_id];
  const tail = "Nothing is sent to them, and no other mode is substituted.";
  // Nobody reached: the label stops counting, and the Excluded line is the notice and the reason.
  const nobody = (line: string) => ({
    label: "No recipient",
    notice: line,
    refusal: line,
    refusalOnNotice: true,
  });

  // Every selected session has left the registry: the line names each by its ID, and no mode
  // would help.
  expect(state(both, [], "btw")).toEqual(
    nobody(
      `Excluded: session:planner-… (no live session), session:reviewer… (no live session). ${tail}`
    )
  );
  // One gone, one without the mode: each with its own cause, then the mode that reaches the
  // live one.
  expect(state(both, [reviewer], "btw")).toEqual(
    nobody(
      `Excluded: session:planner-… (no live session), Reviewer (does not advertise BTW). ${tail} Sending as Aside would reach 1 of them.`
    )
  );
  // The hint picks the mode that reaches the most: Aside reaches both, BTW only the Planner.
  expect(state(both, agents, "steer").refusal).toMatch(
    / Sending as Aside would reach 2 of them\.$/
  );
  // One session alone is "it"; a session advertising nothing gets no hint at all.
  expect(state([reviewer.session_id], [reviewer], "btw").refusal).toMatch(
    / Sending as Aside would reach it\.$/
  );
  expect(state([deaf.session_id], [deaf], "btw")).toEqual(
    nobody(`Excluded: Deaf (does not advertise BTW). ${tail}`)
  );

  // Someone reached: the label counts, and an Excluded line is context beside Send, never its
  // reason, with no hint since the mode reaches someone.
  const reviewerLeftOut = `Excluded: Reviewer (does not advertise BTW). ${tail}`;
  expect(state(both, agents, "btw")).toEqual({
    label: "Send to 1",
    notice: reviewerLeftOut,
    refusal: null,
    refusalOnNotice: false,
  });
  // An empty message is Send's own reason, beside the same line.
  expect(state(both, agents, "btw", "  ")).toEqual({
    label: "Send to 1",
    notice: reviewerLeftOut,
    refusal: "Type a message first.",
    refusalOnNotice: false,
  });
  expect(state([planner.session_id], agents, "btw")).toEqual({
    label: "Send to 1",
    notice: null,
    refusal: null,
    refusalOnNotice: false,
  });

  // The limit outranks an empty message and the Excluded line alike.
  const crowd = Array.from({ length: 101 }, (_, index) => ({
    ...planner,
    session_id: `crowd-${index}`,
  }));
  const limit = "At most 100 recipients per broadcast; this one would reach 101.";
  expect(
    state(
      [...crowd.map((agent) => agent.session_id), reviewer.session_id],
      [...crowd, reviewer],
      "btw",
      ""
    )
  ).toEqual({
    label: "Send to 101",
    notice: limit,
    refusal: limit,
    refusalOnNotice: true,
  });
});

test("the header checkbox follows the filters and its count never hides a selected row the filter hides", async () => {
  const page = renderAgents();

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    expect(within(region).queryByRole("button", { name: /^Select all/ })).toBeNull();
    const header = within(region).getByRole("checkbox", {
      name: "Select all matching agents",
    }) as HTMLInputElement;
    const directory = within(region).getByRole("searchbox", { name: "Directory contains" });
    expect(within(region).getByText("2 matching")).toBeTruthy();

    // Everything, then narrow: the header counts the matching row and names the other one.
    fireEvent.click(header);
    expect(within(region).getByText("2 of 2 matching selected")).toBeTruthy();
    fireEvent.change(directory, { target: { value: "PLANNER" } });
    await waitFor(() =>
      expect(within(region).queryByRole("heading", { level: 2, name: "Reviewer" })).toBeNull()
    );
    expect(
      within(region).getByText("1 of 1 matching selected · 1 more selected outside the filter")
    ).toBeTruthy();
    expect(header.checked).toBe(true);
    const broadcast = within(region).getByRole("region", { name: "Broadcast" });
    expect(
      within(broadcast).getByRole("heading", { name: "Broadcast to 1 of 2 selected" })
    ).toBeTruthy();

    // Clearing the matching row leaves a selection wholly outside the filter, which must not
    // read like an empty one.
    fireEvent.click(header);
    expect(within(region).getByText("1 matching · 1 selected outside the filter")).toBeTruthy();
    expect(header.checked).toBe(false);
    expect(header.indeterminate).toBe(false);
    const chips = within(broadcast).getByRole("list", { name: "Selected agents" });
    expect(
      within(chips)
        .getAllByRole("button")
        .map((chip) => chip.textContent)
    ).toEqual(["Reviewer · does not advertise BTW ✕"]);

    // Widening the filter shows the unticked row beside the ticked one: the header turns mixed.
    fireEvent.change(directory, { target: { value: "" } });
    await waitFor(() => expect(within(region).getByText("1 of 2 matching selected")).toBeTruthy());
    expect(header.checked).toBe(false);
    expect(header.indeterminate).toBe(true);

    // Clear selection empties the whole selection, the header with it.
    fireEvent.click(within(region).getByRole("button", { name: "Clear selection" }));
    expect(within(region).getByText("2 matching")).toBeTruthy();
    expect(header.indeterminate).toBe(false);
    expect(header.checked).toBe(false);
    expect(within(region).queryByRole("region", { name: "Broadcast" })).toBeNull();
    expect(within(region).queryByRole("button", { name: "Clear selection" })).toBeNull();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("the header checkbox selects a folded row and the fold says how many of its rows are selected", async () => {
  // Seen a minute ago with no Dispatch signal: it sits under the collapsed `No Dispatch
  // activity` fold.
  const silent: Agent = {
    ...agents[0],
    last_activity: null,
    open_asks: 0,
    session_id: "silent-session",
    title: "Silent",
  };
  const page = renderAgents({ inboxRows: [], listedAgents: [agents[0], silent] });

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    expect(within(region).getByRole("button", { name: "No Dispatch activity (1)" })).toBeTruthy();
    expect(within(region).queryByRole("heading", { level: 2, name: "Silent" })).toBeNull();
    fireEvent.click(within(region).getByRole("checkbox", { name: "Select all matching agents" }));
    expect(within(region).getByText("2 of 2 matching selected")).toBeTruthy();
    expect(
      within(region).getByRole("button", { name: "No Dispatch activity (1, 1 selected)" })
    ).toBeTruthy();
    const chips = within(within(region).getByRole("region", { name: "Broadcast" })).getByRole(
      "list",
      { name: "Selected agents" }
    );
    expect(
      within(chips)
        .getAllByRole("button")
        .map((chip) => chip.textContent)
    ).toEqual(["Planner ✕", "Silent ✕"]);
  } finally {
    page.view.unmount();
    page.restore();
  }
});
