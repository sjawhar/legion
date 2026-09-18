import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type {
  Agent,
  InboxRow,
  IssueSummary,
  Message,
  MessageRead,
  UserAgentStates,
} from "../../api/types";
import { AuthGate } from "../../app";
import { orderAgents, partitionAgents } from "./AgentsPage";

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
  const getMyAgentState = spyOn(api, "getMyAgentState").mockResolvedValue(agentState);
  const putAgentState = spyOn(api, "putAgentState").mockImplementation(async (_session, input) => ({
    cleared_before: input.cleared_before,
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
    expect(within(reviewer).queryByRole("link")).toBeNull();
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
        key: "CORE-1",
        last_seq: 0,
        open_asks: 0,
        parent: null,
        assignee: null,
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

test("Agents keeps an issue-attached legacy reply on its issue message route", async () => {
  const root = message("Can this ship?", {
    issue_key: "CORE-1",
    target: "session:planner-session",
    deliveries: [
      {
        attempt: 1,
        created_at: "2026-09-14T00:00:00Z",
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

/** One exchange: a root from Alice at `createdAt`, optionally answered by the planner. */
function exchange(
  id: string,
  body: string,
  createdAt: string,
  reply?: { body: string; createdAt: string }
): MessageRead {
  const root = message(body, { created_at: createdAt, id });
  return {
    message: root,
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
    agentState: { "planner-session": { cleared_before: "2026-09-14T12:00:00Z" } },
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

test("Agents replies to an issue-less exchange through the agent route, threaded under the answer", async () => {
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
        delivery: "steer",
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
