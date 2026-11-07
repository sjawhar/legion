import { expect, jest, type Mock, spyOn, test } from "bun:test";
import { RECEIPT_TIMEOUT_CAUSE } from "@legion/contracts";
import {
  focusManager,
  onlineManager,
  QueryClient,
  QueryClientProvider,
} from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router-dom";
import { answerAgentStates } from "../../__tests__/send-answers";
import { wholeText } from "../../__tests__/whole-text";
import { ApiError, api, NetworkError, RequestTimeoutError } from "../../api/client";
import type {
  Agent,
  BroadcastCreated,
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
import { KEYED_SEND_LATE_TEXT } from "../conversation/send-request";
import type { AgentPart } from "../refs/AgentName";
import { KEYED_SEND_ATTEMPT_MS, SendStoppedError } from "../writes/keyed-write";
import { resetWriteStorage } from "../writes/write-storage";
import { orderAgents } from "./AgentsPage";
import type { AgentGroup } from "./agent-groups";
import { type AgentFilters, matchesFilters, searchWords } from "./agent-search";
import { broadcastPlan, broadcastSendState, composedBroadcast } from "./broadcast-plan";

/** What each attempt of a message to one agent, or of a broadcast, is held to, beside what it
 *  carries: its deadline and the key that names the press (`Idempotency-Key`). */
const keyedAttempt = expect.objectContaining({
  deadlineMs: KEYED_SEND_ATTEMPT_MS,
  key: expect.any(String),
});

/** The bounds a read mark's write carries: the one write path's key and deadline. */
const markAttempt = keyedAttempt;

/** The key each message to one agent went under, in order. */
function messageKeys(createAgentMessage: Mock<typeof api.createAgentMessage>): string[] {
  return createAgentMessage.mock.calls.map(([, , bounds]) => bounds.key);
}

/** Dispatch's refusal of a broadcast. A 4xx is the one failure a broadcast is not tried again
 *  after under its key: a failed fetch, a 5xx or a 429 is. */
const BROADCAST_REFUSED = "no selected session takes this broadcast";

async function refuseBroadcast(): Promise<never> {
  throw new ApiError(400, { code: "BROADCAST_EMPTY", error: BROADCAST_REFUSED });
}

// Delivery attempts are dated relative to the run: the dashboard only offers a
// same-mode Retry while an attempt is inside the stream's duplicate window, so a
// fixture frozen at an absolute date would age out of every retry assertion.
const recentAttemptAt = new Date(Date.now() - 60_000).toISOString();

const now = Date.now();
const agents = [
  {
    capabilities: ["aside", "btw"],
    dir: "/workspaces/planner",
    host: "build-host",
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
    host: "review-host",
    last_activity: new Date(now - 10 * 60_000).toISOString(),
    last_seen: now - 5 * 60_000,
    machine_id: "review-host",
    open_asks: 0,
    roles: [],
    session_id: "reviewer-session",
    title: "Reviewer",
  },
] satisfies Agent[];

// The signed-in reader is `alice`, and a card's first number is their own work: these rows are
// on an issue assigned to them.
const inbox: InboxRow[] = [1, 2].map((number) => ({
  anchor: null,
  answer: null,
  author: { id: "planner-session", kind: "session" },
  created_at: new Date(now - 60_000).toISOString(),
  edited_at: null,
  id: `ask-${number}`,
  issue: { assignee: "alice", key: "CORE-1", title: "Core work" },
  issue_key: "CORE-1",
  kind: "question",
  multiple: false,
  opened_event_id: number,
  options: [],
  thread: {
    answers: [],
    claimant: null,
    edits: [],
    followers: [],
    replies: [],
    route: null,
    route_status: null,
    route_holder: null,
  },
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

/** When Dispatch says it took a pin, in the answer the fake route gives. */
const ANSWERED_PIN_AT = "2026-09-14T12:00:00Z";

/** Exposes the router's current query string, for the one test that needs to see a typed search
 *  land on the page's own address rather than only in its rendered rows. */
function LocationProbe() {
  const location = useLocation();
  return <div data-testid="location-search">{location.search}</div>;
}

function renderAgents({
  agentState = {},
  inboxRows = inbox,
  initialEntry = "/agents",
  listedAgents = agents,
  issues = [],
  messages = [],
}: {
  agentState?: UserAgentStates;
  inboxRows?: InboxRow[];
  initialEntry?: string;
  listedAgents?: Agent[];
  issues?: IssueSummary[];
  messages?: MessageRead[];
} = {}) {
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({
    event_head: "0",
    kind: "user",
    login: "alice",
  });
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
  // Dispatch answers a state write with the session's whole state: the marks the write set over
  // the state it held, with `pinned_at` added by a pin and dropped by an unpin, and nothing left
  // unread.
  const putAgentState = answerAgentStates().mockImplementation(async (session, input) => {
    const { pinned, read_replies: _read, ...marks } = input;
    const { pinned_at: held, ...rest } = agentState[session] ?? {};
    const pinnedAt = pinned === undefined ? held : pinned ? ANSWERED_PIN_AT : undefined;
    return {
      ...rest,
      ...marks,
      ...(pinnedAt === undefined ? {} : { pinned_at: pinnedAt }),
      unread_replies: 0,
    };
  });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={[initialEntry]}>
      <QueryClientProvider client={queryClient}>
        <AuthGate />
        <LocationProbe />
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

/** A row a closed fold or the filters hide is off the page: the list is windowed, so a row it
 *  does not draw is not mounted at all, hidden or otherwise. The one exception is a row the
 *  reader has opened (`expectKeptHidden`). */
function expectFolded(region: HTMLElement, name: string): void {
  expect(within(region).queryByRole("heading", { hidden: true, name })).toBeNull();
}

/** A row the reader has opened that a closed fold or the filters hide: mounted and hidden
 *  wherever the window has it, so the draft, the upload and the unread set it holds survive. */
function expectKeptHidden(region: HTMLElement, name: string): void {
  expect(within(region).queryByRole("heading", { name })).toBeNull();
  const heading = within(region).getByRole("heading", { hidden: true, name });
  expect(heading.closest("article")?.hidden).toBe(true);
}

/** Cards collapse by default; the title button toggles the conversation and composer. */
function expand(agentCard: HTMLElement, name: string): void {
  fireEvent.click(within(agentCard).getByRole("button", { name }));
}

/** The four group headings the page draws, in the order it draws them. */
function groupNames(region: HTMLElement): string[] {
  return [...region.querySelectorAll<HTMLElement>('[data-testid^="agents-group-name-"]')].map(
    (name) => name.textContent ?? ""
  );
}

/** A group's collapse control, which is also its heading. */
function groupToggle(region: HTMLElement, group: AgentGroup): HTMLElement {
  const toggle = region.querySelector<HTMLElement>(`[data-testid="agents-group-toggle-${group}"]`);
  if (toggle === null) throw new Error(`no ${group} group on the page`);
  return toggle;
}

/** What a group's heading says it holds: `(2)`, or `(2, 1 selected)`. */
function groupCount(region: HTMLElement, group: AgentGroup): string {
  const count = region.querySelector<HTMLElement>(`[data-testid="agents-group-count-${group}"]`);
  if (count === null) throw new Error(`no ${group} group on the page`);
  return count.textContent ?? "";
}

test("Agents lists live session activity and capability-aware actions", async () => {
  const page = renderAgents();

  try {
    await screen.findByRole("heading", { name: "Agents" });
    const pageRegion = screen.getByRole("region", { name: "Agents" });
    expect(within(pageRegion).getByText("Planner", { exact: true })).toBeTruthy();
    expect(within(pageRegion).getByText("Needs you 2", { exact: true })).toBeTruthy();
    expect(within(pageRegion).queryByText(/^Open asks/)).toBeNull();
    // The row's state is `AgentName`'s: a listed session is running, and its mark's `title` says
    // when the listener last heard it.
    const plannerState = within(card(pageRegion, "Planner")).getByRole("img", { name: "Running" });
    expect(plannerState.getAttribute("title")).toBe("Running · seen just now");
    expect(
      within(card(pageRegion, "Planner"))
        .getByRole("link", { name: "Open live conversation with Planner" })
        .getAttribute("href")
    ).toBe("/agents/planner-session/live");
    const reviewer = card(pageRegion, "Reviewer");
    expand(reviewer, "Reviewer");
    expect(within(reviewer).queryByRole("button", { name: /^BTW$|^Aside$|^Steer$/ })).toBeNull();
    expect(within(reviewer).getByRole("textbox", { name: "Comment" })).toBeTruthy();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// A Claude Code session takes Aside but not Send: the card's plain message goes as an Aside and
// says so, rather than warning that the Send it would have been records a failed attempt. A
// session that advertises no mode at all says on its card that it takes no messages.
test("an agent card sends a plain message in the mode the session takes", async () => {
  const deaf = { ...agents[1], capabilities: [], session_id: "deaf-session", title: "Deaf" };
  const page = renderAgents({ listedAgents: [...agents, deaf] });

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    const reviewer = card(region, "Reviewer");
    expand(reviewer, "Reviewer");
    fireEvent.change(within(reviewer).getByRole("textbox", { name: "Comment" }), {
      target: { value: "Is the review done?" },
    });
    expect(
      within(reviewer).getByText("Sending as Aside: Reviewer does not advertise Send.")
    ).toBeTruthy();
    fireEvent.submit(within(reviewer).getByRole("form", { name: "Comment composer" }));
    await waitFor(() =>
      expect(page.createAgentMessage).toHaveBeenCalledWith(
        "reviewer-session",
        { body: "Is the review done?", delivery: "aside" },
        keyedAttempt
      )
    );

    const silent = card(region, "Deaf");
    expand(silent, "Deaf");
    expect(within(silent).getByText("Takes no messages")).toBeTruthy();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("Agents collapses every card by default and expands each one independently", async () => {
  const page = renderAgents();

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    // Nothing is mounted before a card's first open, hidden or not.
    expect(within(region).queryByRole("textbox", { hidden: true, name: "Comment" })).toBeNull();
    expect(page.listAgentMessages).not.toHaveBeenCalled();
    const planner = card(region, "Planner");
    const toggle = within(planner).getByRole("button", { name: "Planner" });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");

    expand(planner, "Planner");
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    expect(within(planner).getByRole("textbox", { name: "Comment" })).toBeTruthy();
    await waitFor(() => expect(page.listAgentMessages).toHaveBeenCalledWith("planner-session"));
    const reviewer = card(region, "Reviewer");
    expect(within(reviewer).queryByRole("textbox", { hidden: true, name: "Comment" })).toBeNull();

    expand(reviewer, "Reviewer");
    expect(within(region).getAllByRole("textbox", { name: "Comment" })).toHaveLength(2);
    expect(toggle.getAttribute("aria-expanded")).toBe("true");

    expand(planner, "Planner");
    // A card opened once keeps its composer - and its draft - when it collapses, hidden.
    expect(within(planner).queryByRole("textbox", { name: "Comment" })).toBeNull();
    const kept = within(planner).getByRole("textbox", { hidden: true, name: "Comment" });
    expect(kept.closest("[hidden]")).not.toBeNull();
    expect(within(reviewer).getByRole("textbox", { name: "Comment" })).toBeTruthy();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("Agents counts a card's two numbers in their own right: the reader's own, and the session's", async () => {
  // Planner: both of its open asks wait on the reader, on their own issue. Builder: six open
  // asks - two the reader's, one on Bob's issue, one of the reader's they snoozed, and two whose
  // next move is Builder's, one of them snoozed too. Reviewer: nothing open.
  const builder: Agent = {
    ...agents[0],
    open_asks: 6,
    session_id: "builder-session",
    title: "Builder",
  };
  const builderAsk = (overrides: Partial<InboxRow>): InboxRow => ({
    ...inbox[0],
    author: { id: "builder-session", kind: "session" },
    ...overrides,
  });
  const page = renderAgents({
    inboxRows: [
      ...inbox,
      builderAsk({ id: "builder-mine-1" }),
      builderAsk({ id: "builder-mine-2" }),
      builderAsk({
        id: "builder-bobs",
        issue: { assignee: "bob", key: "CORE-2", title: "Bob's work" },
        issue_key: "CORE-2",
      }),
      builderAsk({
        id: "builder-snoozed",
        snoozed_until: new Date(now + 3 * 60 * 60 * 1000).toISOString(),
      }),
      builderAsk({ id: "builder-agents", waiting_on: "agent" }),
      builderAsk({
        id: "builder-agents-snoozed",
        snoozed_until: new Date(now + 3 * 60 * 60 * 1000).toISOString(),
        waiting_on: "agent",
      }),
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

    // Bob's question is in neither number, and neither is the reader's own that they snoozed -
    // which is what subtracting from `open_asks` used to charge to the agent. A snooze is the
    // reader deferring a question, not a change of whose move it is, so the snoozed agent-turn
    // question IS this session's second number, reached by opening `Later` in the list it links.
    const builderCard = card(region, "Builder");
    expect(
      (await within(builderCard).findByRole("link", { name: "Needs you 2" })).getAttribute("href")
    ).toBe("/?agent=builder-session&section=needs-you");
    const waiting = within(builderCard).getByRole("link", { name: "Waiting on agent 2" });
    // The filter cleared, so the list it opens holds the question it counted whoever holds it.
    expect(waiting.getAttribute("href")).toBe("/?agent=builder-session&assignee=everyone");
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

// The four groups are `agent-groups.ts`'s own rule, with its own tests; what the page does with
// them - the headings, the counts, the collapse and who lands where - is tested below.

test("matchesFilters keeps a session whose title, directory, machine, session id or issue key together hold every searched word, in any case", () => {
  const alpha = session({
    dir: "/workspaces/alpha",
    host: "alpha-host",
    session_id: "alpha-session",
    title: "Scrum planning",
  });
  const beta = session({
    dir: "/workspaces/beta",
    host: "beta-host",
    session_id: "beta-session",
    title: "Beta",
  });
  const matches = (agent: Agent, search: string, issueKeys: readonly string[] = []) =>
    matchesFilters(
      agent,
      "active",
      { groups: [], machines: [], roles: [] },
      searchWords(search),
      issueKeys
    );

  // One word, found in the title alone.
  expect(matches(alpha, "scrum")).toBe(true);
  expect(matches(beta, "scrum")).toBe(false);

  // Two words found in two different fields of the one session - its directory and an issue key
  // one of its asks names - still match: "wherever they appear" is not "in the same field".
  expect(matches(alpha, "alpha core-9", ["CORE-9"])).toBe(true);
  expect(matches(beta, "alpha core-9", ["CORE-9"])).toBe(false);

  // Case-insensitive, by host; a word absent from every field excludes the session even
  // when the others match.
  expect(matches(alpha, "ALPHA-HOST")).toBe(true);
  expect(matches(alpha, "alpha gamma")).toBe(false);

  // The session id itself is searchable too.
  expect(matches(alpha, "alpha-session")).toBe(true);
  expect(matches(beta, "alpha-session")).toBe(false);
});

test("matchesFilters keeps a session any picked group, machine or role holds, free text besides", () => {
  const planner = session({
    host: "build-host",
    roles: ["planner", "reviewer"],
    session_id: "planner-session",
    title: "Planner",
  });
  const narrow = (
    filters: Partial<Pick<AgentFilters, "groups" | "machines" | "roles">>,
    group: AgentGroup = "active",
    search = ""
  ) =>
    matchesFilters(
      planner,
      group,
      { groups: [], machines: [], roles: [], ...filters },
      searchWords(search),
      []
    );

  // An empty list is no filter at all; a list keeps a session any one of its values holds.
  expect(narrow({})).toBe(true);
  expect(narrow({ machines: ["build-host"] }, "active", "planner")).toBe(true);
  expect(narrow({ machines: ["other-host"] })).toBe(false);
  expect(narrow({ machines: ["other-host", "build-host"] })).toBe(true);
  // A role list matches a session holding any of them, not all of them.
  expect(narrow({ roles: ["planner"] })).toBe(true);
  expect(narrow({ roles: ["tester"] })).toBe(false);
  expect(narrow({ roles: ["tester", "reviewer"] })).toBe(true);
  // The group is the row's own state, which the page works out before it asks.
  expect(narrow({ groups: ["active"] }, "active")).toBe(true);
  expect(narrow({ groups: ["active"] }, "quiet")).toBe(false);
  expect(narrow({ groups: ["needs-you", "quiet"] }, "quiet")).toBe(true);
  // Every field holds at once: the machine matches and the free text does not.
  expect(narrow({ machines: ["build-host"] }, "active", "reviewer-host")).toBe(false);
});

test("Agents lists each session in its group, orders the rows inside one, and keeps a re-poll stable", async () => {
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
        .getAllByRole("heading", { hidden: false, level: 2 })
        .map((heading) => heading.textContent);
    await within(region).findByText("Needs you 2", { exact: true });
    // Planner has an ask waiting on the reader, so it heads the page whatever its age; Alpha
    // spoke a minute ago; Zulu's activity is five minutes old and None's is nothing at all, so
    // both are Idle, newest activity first.
    expect(titles()).toEqual(["Planner", "Alpha", "Zulu", "None"]);
    expect(groupNames(region)).toEqual(["Needs you", "Active", "Idle", "Quiet"]);
    expect(groupCount(region, "needs-you")).toBe("(1)");
    expect(groupCount(region, "active")).toBe("(1)");
    expect(groupCount(region, "idle")).toBe("(2)");
    expect(groupCount(region, "quiet")).toBe("(0)");
    await page.queryClient.refetchQueries({ queryKey: ["agents"] });
    expect(titles()).toEqual(["Planner", "Alpha", "Zulu", "None"]);

    // Collapsing a group hides its rows and no others, and opening it puts them back.
    const idle = groupToggle(region, "idle");
    expect(idle.getAttribute("aria-expanded")).toBe("true");
    fireEvent.click(idle);
    expect(idle.getAttribute("aria-expanded")).toBe("false");
    expectFolded(region, "None");
    expectFolded(region, "Zulu");
    expect(titles()).toEqual(["Planner", "Alpha"]);

    fireEvent.click(idle);
    expect(idle.getAttribute("aria-expanded")).toBe("true");
    const noneCard = card(region, "None");
    expect(within(noneCard).getByText("No Dispatch activity", { exact: true })).toBeTruthy();
    expect(titles()).toEqual(["Planner", "Alpha", "Zulu", "None"]);
    // A row a group gave back is the row it was: its composer is still there to open.
    expand(noneCard, "None");
    expect(within(noneCard).getByRole("textbox", { name: "Comment" })).toBeTruthy();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// R7: the page draws its four groups whether or not each holds a session, so a reader can see
// at a glance that nothing waits on them and the count beside each heading always means
// something.
test("Agents draws four counted groups, the empty ones among them", async () => {
  const page = renderAgents();

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    expect(groupNames(region)).toEqual(["Needs you", "Active", "Idle", "Quiet"]);
    // The Planner's two asks wait on the reader; the Reviewer was heard from five minutes ago
    // with its last Dispatch activity ten minutes old.
    expect(groupCount(region, "needs-you")).toBe("(1)");
    expect(groupCount(region, "active")).toBe("(0)");
    expect(groupCount(region, "idle")).toBe("(1)");
    expect(groupCount(region, "quiet")).toBe("(0)");
    for (const group of ["needs-you", "active", "idle", "quiet"] as const) {
      expect(groupToggle(region, group).getAttribute("aria-expanded")).toBe("true");
    }
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// D3: the pin no longer lifts a row out of its group - the rail is the pin's own home now - so
// a pinned session is in the group its row puts it in, first among that group's rows.
test("Agents keeps a pinned session in its own group, first among its rows", async () => {
  const silent: Agent = {
    ...agents[1],
    last_activity: null,
    session_id: "silent-session",
    title: "Silent",
  };
  // The pin is the person's, held by Dispatch (INFRA-812): this page reads it from their agent
  // state, where it was `dispatch.agents.pinned:<login>` in this browser before.
  const page = renderAgents({
    agentState: { "silent-session": { pinned_at: "2026-09-14T00:00:00Z" } },
    listedAgents: [...agents, silent],
  });
  const titles = (region: HTMLElement) =>
    within(region)
      .getAllByRole("heading", { hidden: false, level: 2 })
      .map((heading) => heading.textContent);

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    await screen.findByRole("button", { name: "Unpin Silent" });
    // Dispatch has never heard from Silent, so it is Idle, not Active: the pin only puts it
    // ahead of the Reviewer, which is Idle too.
    expect(titles(region)).toEqual(["Planner", "Silent", "Reviewer"]);
    expect(groupCount(region, "idle")).toBe("(2)");

    fireEvent.click(within(region).getByRole("button", { name: "Unpin Silent" }));
    // Unpinned it stays in Idle, behind the Reviewer, whose activity is the newer of the two.
    await waitFor(() => expect(titles(region)).toEqual(["Planner", "Reviewer", "Silent"]));
    expect(groupCount(region, "idle")).toBe("(2)");
    expect(page.putAgentState.mock.calls.map(([session, input]) => [session, input])).toEqual([
      ["silent-session", { pinned: false }],
    ]);
  } finally {
    page.view.unmount();
    page.restore();
  }
});

/** Heard from eleven minutes ago, its last Dispatch activity half an hour old: Quiet. */
const stale: Agent = {
  ...agents[1],
  last_activity: new Date(now - 30 * 60_000).toISOString(),
  last_seen: now - 11 * 60_000,
  session_id: "stale-session",
  title: "Stale",
};

test("Agents puts a session unseen for ten minutes in Quiet, which collapses and opens", async () => {
  const page = renderAgents({ listedAgents: [stale, ...agents] });

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    const titles = () =>
      within(region)
        .getAllByRole("heading", { hidden: false, level: 2 })
        .map((heading) => heading.textContent);
    expect(titles()).toEqual(["Planner", "Reviewer", "Stale"]);
    expect(groupCount(region, "quiet")).toBe("(1)");
    const quiet = groupToggle(region, "quiet");
    expect(quiet.getAttribute("aria-expanded")).toBe("true");

    fireEvent.click(quiet);
    expect(quiet.getAttribute("aria-expanded")).toBe("false");
    expectFolded(region, "Stale");
    expect(titles()).toEqual(["Planner", "Reviewer"]);

    fireEvent.click(quiet);
    expect(quiet.getAttribute("aria-expanded")).toBe("true");
    const staleCard = card(region, "Stale");
    // Still listed, so still running: the group is what says it has been quiet, and the mark's
    // `title` says for how long.
    expect(within(staleCard).getByRole("img", { name: "Running" }).getAttribute("title")).toBe(
      "Running · seen 11 minutes ago"
    );
    expect(titles()).toEqual(["Planner", "Reviewer", "Stale"]);
    // A collapsed group's rows keep their composer.
    expand(staleCard, "Stale");
    expect(within(staleCard).getByRole("textbox", { name: "Comment" })).toBeTruthy();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// D3, in the case the page used to lift out: the pin is a place in the rail, not a place in
// another group, so a pinned session nobody has heard from for ten minutes is still Quiet.
test("Agents keeps a pinned session unseen for ten minutes in Quiet", async () => {
  const page = renderAgents({
    agentState: { "stale-session": { pinned_at: "2026-09-14T00:00:00Z" } },
    listedAgents: [...agents, stale],
  });

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    await screen.findByRole("button", { name: "Unpin Stale" });
    expect(groupCount(region, "quiet")).toBe("(1)");
    expect(groupCount(region, "active")).toBe("(0)");
    // Collapsing Quiet takes the pinned row with it, which is where it is listed.
    fireEvent.click(groupToggle(region, "quiet"));
    expectFolded(region, "Stale");
    expect(
      within(region)
        .getAllByRole("heading", { hidden: false, level: 2 })
        .map((heading) => heading.textContent)
    ).toEqual(["Planner", "Reviewer"]);
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// D2: the four tests overlap and the first match wins. A session with a question for the reader
// is where they will look for it, however long it has been silent - and in that group alone.
test("Agents lists a session with an open ask under Needs you alone, however long it has been silent", async () => {
  const silentForHours: Agent = {
    ...stale,
    last_activity: null,
    last_seen: now - 2 * 60 * 60_000,
  };
  const page = renderAgents({
    inboxRows: [
      ...inbox,
      { ...inbox[0], author: { id: "stale-session", kind: "session" }, id: "ask-stale" },
    ],
    listedAgents: [...agents, silentForHours],
  });

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    await within(region).findByText("Needs you 1", { exact: true });
    expect(groupCount(region, "needs-you")).toBe("(2)");
    expect(groupCount(region, "quiet")).toBe("(0)");
    // Collapsing Needs you takes the row with it: it is in that group and in no other.
    fireEvent.click(groupToggle(region, "needs-you"));
    expectFolded(region, "Stale");
    expectFolded(region, "Planner");
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// A session with replies the reader has not read is waiting on them as surely as an ask is.
test("Agents lists a session with unread replies under Needs you", async () => {
  const page = renderAgents({
    agentState: { "reviewer-session": { unread_replies: 2 } },
    listedAgents: agents,
  });

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    await within(region).findByRole("button", { name: "Reviewer replied: 2 unread" });
    await waitFor(() => expect(groupCount(region, "needs-you")).toBe("(2)"));
    expect(groupCount(region, "idle")).toBe("(0)");
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// R7: the page lists the sessions the listener lists, so a session that has ended is in none of
// the four groups - it keeps its row under `Replied, no longer connected`, where its unread
// replies can still be reached.
test("Agents lists an ended session under Replied, no longer connected and in no group", async () => {
  const page = renderAgents({
    agentState: { "gone-session": { unread_replies: 1 } },
    listedAgents: agents,
  });

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    const ended = await screen.findByRole("region", { name: "Replied, no longer connected" });
    expect(within(ended).getByText("New reply 1")).toBeTruthy();
    expect(groupNames(region)).toEqual(["Needs you", "Active", "Idle", "Quiet"]);
    expect(groupCount(region, "needs-you")).toBe("(1)");
    expect(groupCount(region, "active")).toBe("(0)");
    expect(groupCount(region, "idle")).toBe("(1)");
    expect(groupCount(region, "quiet")).toBe("(0)");
    // The ended session's row is outside the list the groups draw.
    expect(region.querySelector('[data-agent-row="gone-session"]')).toBeNull();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// The pin belongs to the person, not to the browser (INFRA-812, R1): pressing it writes the
// session's state to Dispatch, so their other browsers and the rail show it, and nothing of it
// is kept in this browser's own storage.
test("Agents pins a card by writing the person's agent state, not a browser preference", async () => {
  const first = renderAgents();
  let firstActive = true;

  try {
    const pin = await screen.findByRole("button", { name: "Pin Planner" });
    expect(pin.getAttribute("aria-pressed")).toBe("false");
    fireEvent.click(pin);

    const unpin = await screen.findByRole("button", { name: "Unpin Planner" });
    expect(unpin.getAttribute("aria-pressed")).toBe("true");
    expect(within(unpin.closest("article") ?? unpin).getByRole("heading").textContent).toBe(
      "Planner"
    );
    expect(first.putAgentState.mock.calls.map(([session, input]) => [session, input])).toEqual([
      ["planner-session", { pinned: true }],
    ]);
    expect(window.localStorage.getItem("dispatch.agents.pinned:alice")).toBeNull();

    // The person's second browser reads the same pin from Dispatch, with nothing of this one's.
    first.view.unmount();
    first.restore();
    firstActive = false;
    const second = renderAgents({
      agentState: { "planner-session": { pinned_at: "2026-09-14T00:00:00Z" } },
    });
    try {
      const elsewhere = await screen.findByRole("button", { name: "Unpin Planner" });
      expect(elsewhere.getAttribute("aria-pressed")).toBe("true");
    } finally {
      second.view.unmount();
      second.restore();
    }
  } finally {
    if (firstActive) {
      first.view.unmount();
      first.restore();
    }
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
      expect(page.createAgentMessage).toHaveBeenCalledWith(
        "planner-session",
        { body: "Check the deployment", delivery: "btw" },
        keyedAttempt
      )
    );
  } finally {
    page.view.unmount();
    page.restore();
  }
});

const coreIssue: IssueSummary = {
  assignee: null,
  claim: null,
  components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
  key: "CORE-1",
  last_seq: 0,
  open_asks: 0,
  progress: { tasks: null, children: null },
  parent: null,
  blocked_by: [],
  primary_artifact_id: "artifact-1",
  priority: null,
  rank: "U",
  route: null,
  repo: null,
  resolved_repo: null,
  route_holder: null,
  route_status: null,
  status: "todo",
  title: "Core work",
  updated_at: "2026-09-14T00:00:00Z",
};

test("Agents keeps selected-issue sends on the issue message route", async () => {
  const page = renderAgents({ issues: [coreIssue] });

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
      // Planner takes Aside and BTW but not Send, so the plain message is an Aside.
      expect(page.createComment).toHaveBeenCalledWith(
        "CORE-1",
        {
          body: "@Planner Track this under Core",
          delivery: "aside",
          mentions: [{ target: "session:planner-session" }],
        },
        keyedAttempt
      )
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
    expect(within(conversation).getByText(wholeText("Answered by Planner"))).toBeTruthy();
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
      expect(page.createMessage).toHaveBeenCalledWith(
        "CORE-1",
        {
          body: "Follow up",
          delivery: "btw",
          in_reply_to: "message-1",
          target: "session:planner-session",
        },
        keyedAttempt
      )
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
      expect(page.createMessageDelivery).toHaveBeenCalledWith("message-1", "steer", keyedAttempt)
    );
    // The same-mode Retry re-sends the attempt's own mode, the only retry that cannot deliver
    // the message twice.
    fireEvent.click(sameMode);
    await waitFor(() =>
      expect(page.createMessageDelivery).toHaveBeenCalledWith("message-1", "btw", keyedAttempt)
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
    await within(planner).findByText(wholeText("Planner does not support Send — use BTW."));
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
      await within(target).findByText(wholeText(headline));
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
    // Newest first, the fold's rows beneath the newest exchange in the same list. A body is a
    // full Markdown block (its text in a `<p>` under the root); the reply's quote of its
    // parent is a one-line preview with no paragraph, so only paragraphs are counted.
    await waitFor(() =>
      expect(
        within(conversation)
          .getAllByText(/question$/, { selector: ".dispatch-markdown > p" })
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
      expect(page.putAgentState).toHaveBeenCalledWith(
        "planner-session",
        { read_through: "2026-09-14T01:05:00Z" },
        markAttempt
      )
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
// exits. A reply from a session no longer in the live list still has a row, whose name links to
// its live view where the reply is read, so the badge is always one the viewer can clear.
test("a reply from a session that has ended keeps a row that opens its conversation", async () => {
  const ended = "01a0e52e-0000-7000-8000-00000000abcd";
  const page = renderAgents({
    agentState: { [ended]: { unread_replies: 1 } },
    listedAgents: [],
  });

  try {
    await expect(screen.findByRole("link", { name: "New reply 1" })).resolves.toBeTruthy();
    const region = await screen.findByRole("region", { name: "Replied, no longer connected" });
    const open = within(region).getByRole("link", { name: "session:01a0e52e…" });
    expect(open.getAttribute("href")).toBe(`/agents/${ended}/live`);
    // The registry answered without it: not running.
    expect(within(region).getByRole("img", { name: "Not running" })).toBeTruthy();
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
      expect(page.putAgentState).toHaveBeenCalledWith(
        "planner-session",
        { read_through: "2026-09-14T00:30:00Z" },
        markAttempt
      )
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
      expect(page.putAgentState).toHaveBeenCalledWith(
        "planner-session",
        { cleared_before: "2026-09-14T02:00:00Z" },
        keyedAttempt
      )
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

// A Claude Code session takes Aside alone: a reply into an exchange whose last attempt was a
// Send it refused goes as an Aside, not as a BTW it refuses too.
test("a reply to an Aside-only agent after a refused Send goes as an Aside", async () => {
  const root = message("Can this ship?", {
    deliveries: [
      {
        attempt: 1,
        created_at: recentAttemptAt,
        delivery: "steer",
        envelope_id: null,
        error: "session reviewer-session () does not advertise steer",
        message_id: "message-1",
        reply_id: null,
        session_id: "reviewer-session",
        state: "failed",
      },
    ],
    target: "session:reviewer-session",
  });
  const page = renderAgents({ messages: [{ message: root, replies: [] }] });

  try {
    const reviewer = card(await screen.findByRole("region", { name: "Agents" }), "Reviewer");
    expand(reviewer, "Reviewer");
    await within(reviewer).findByText("Can this ship?");
    fireEvent.click(within(reviewer).getByRole("button", { name: "Reply" }));
    fireEvent.change(within(reviewer).getByRole("textbox", { name: "Comment" }), {
      target: { value: "Can this ship now?" },
    });
    fireEvent.submit(within(reviewer).getByRole("form", { name: "Comment composer" }));
    await waitFor(() =>
      expect(page.createAgentMessage).toHaveBeenCalledWith(
        "reviewer-session",
        {
          body: "Can this ship now?",
          delivery: "aside",
          in_reply_to: "message-1",
        },
        keyedAttempt
      )
    );
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
    expect(
      within(composer).getByText(wholeText("Replying to Planner — Yes, it can."))
    ).toBeTruthy();
    // A reply lives in its parent's direct-session conversation; it hides the issue picker.
    expect(within(planner).queryByRole("button", { name: "Choose issue" })).toBeNull();
    fireEvent.change(within(planner).getByRole("textbox", { name: "Comment" }), {
      target: { value: "Ship it." },
    });
    fireEvent.submit(composer);
    await waitFor(() =>
      expect(page.createAgentMessage).toHaveBeenCalledWith(
        "planner-session",
        {
          body: "Ship it.",
          delivery: "btw",
          in_reply_to: "message-2",
        },
        keyedAttempt
      )
    );
    await waitFor(() =>
      expect(
        within(planner).queryByText(wholeText("Replying to Planner — Yes, it can."))
      ).toBeNull()
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
      expect(page.putAgentState).toHaveBeenCalledWith(
        "planner-session",
        { read_through: "2026-09-14T03:00:00.1235Z" },
        markAttempt
      )
    );

    fireEvent.click(within(planner).getByRole("button", { name: "Clear conversation" }));
    await waitFor(() =>
      expect(page.putAgentState).toHaveBeenCalledWith(
        "planner-session",
        { cleared_before: "2026-09-14T03:00:00.1235Z" },
        keyedAttempt
      )
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
      expect(page.createAgentMessage).toHaveBeenCalledWith(
        "planner-session",
        {
          body: "Ship it.",
          delivery: "btw",
          in_reply_to: "message-2",
        },
        keyedAttempt
      )
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
    host: "review-host",
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
    host: "review-host",
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
    const excludedLine = within(broadcast).getByText(
      wholeText(
        "Excluded: Reviewer (does not advertise BTW). Nothing is sent to them, and no other mode is substituted."
      )
    );
    // The excluded agent is drawn by `AgentName`: its live link, inside the line.
    expect(within(excludedLine).getByRole("link", { name: "Reviewer" }).getAttribute("href")).toBe(
      "/agents/reviewer-session/live"
    );
    fireEvent.change(within(broadcast).getByRole("textbox", { name: "Broadcast message" }), {
      target: { value: "Stand down and report status." },
    });
    fireEvent.click(within(broadcast).getByRole("button", { name: "Send to 1" }));
    await waitFor(() =>
      expect(page.createBroadcast).toHaveBeenCalledWith(
        {
          body: "Stand down and report status.",
          delivery: "btw",
          session_ids: ["planner-session"],
        },
        keyedAttempt
      )
    );
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("a broadcast whose selection holds an Aside-only session goes as an Aside until the sender picks a mode, and says so", async () => {
  const page = renderAgents();

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    fireEvent.click(within(region).getByRole("checkbox", { name: "Select Planner for broadcast" }));
    let broadcast = within(region).getByRole("region", { name: "Broadcast" });
    const mode = () =>
      within(broadcast).getByRole<HTMLSelectElement>("combobox", { name: "Delivery mode" });
    // The Planner takes BTW: the broadcast's usual default, named by nothing.
    expect(mode().value).toBe("btw");
    expect(within(broadcast).queryByText(/Sending as Aside/)).toBeNull();
    // The Reviewer advertises `aside` only, so the selection's mode becomes Aside and reaches both.
    fireEvent.click(
      within(region).getByRole("checkbox", { name: "Select Reviewer for broadcast" })
    );
    broadcast = within(region).getByRole("region", { name: "Broadcast" });
    expect(mode().value).toBe("aside");
    const asideLine = within(broadcast).getByText(
      wholeText("Sending as Aside: Reviewer does not advertise BTW.")
    );
    expect(within(asideLine).getByRole("link", { name: "Reviewer" }).getAttribute("href")).toBe(
      "/agents/reviewer-session/live"
    );
    expect(within(broadcast).queryByText(/Excluded:/)).toBeNull();
    fireEvent.change(within(broadcast).getByRole("textbox", { name: "Broadcast message" }), {
      target: { value: "Report status." },
    });
    fireEvent.click(within(broadcast).getByRole("button", { name: "Send to 2" }));
    await waitFor(() =>
      expect(page.createBroadcast).toHaveBeenCalledWith(
        {
          body: "Report status.",
          delivery: "aside",
          session_ids: ["planner-session", "reviewer-session"],
        },
        keyedAttempt
      )
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
  page.createBroadcast.mockImplementationOnce(refuseBroadcast);
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
    await within(sends).findByText(`Could not send to 1 agent: ${BROADCAST_REFUSED}`);
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
    readonly mock: { readonly calls: readonly Parameters<typeof api.createBroadcast>[] };
  };
}

/** The i-th request the page handed `api.createBroadcast`. */
function posted(page: BroadcastSpy, index: number): CreateBroadcastInput {
  const call = page.createBroadcast.mock.calls[index];
  if (call === undefined) throw new Error(`no broadcast request ${index}`);
  return call[0];
}

/** The key the i-th broadcast request went under (`Idempotency-Key`). */
function postedKey(page: BroadcastSpy, index: number): string {
  const call = page.createBroadcast.mock.calls[index];
  if (call === undefined) throw new Error(`no broadcast request ${index}`);
  return call[1].key;
}

// LEGION-446. A refused send's row is the only copy of its message, and Restore draft is how the
// human sends it again. Until something is edited the request goes out word for word; Dispatch
// refused it, so it goes under a key of its own, since Dispatch would answer the refused key with
// its stored refusal. The first edit makes it a new composition, under a new key too.
test("Restore draft re-sends the refused request word for word, and the first edit drops it for a new send", async () => {
  const page = renderAgents();
  page.createBroadcast
    .mockImplementationOnce(refuseBroadcast)
    .mockImplementationOnce(refuseBroadcast);

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
          within(sends).getAllByText(`Could not send to 1 agent: ${BROADCAST_REFUSED}`)
        ).toHaveLength(failures)
      );
      fireEvent.click(within(sends).getByRole("button", { name: "Restore draft" }));
    };
    fireEvent.click(within(region).getByRole("checkbox", { name: "Select Planner for broadcast" }));
    fireEvent.change(message(), { target: { value: "Keep this." } });
    fireEvent.click(screen.getByRole("button", { name: "Send to 1" }));
    await waitFor(() => expect(page.createBroadcast).toHaveBeenCalledTimes(1));
    const first = posted(page, 0);
    const firstKey = postedKey(page, 0);
    expect(firstKey).not.toBe("");

    await restoreDraft(1);
    fireEvent.click(screen.getByRole("button", { name: "Send to 1" }));
    await waitFor(() => expect(page.createBroadcast).toHaveBeenCalledTimes(2));
    expect(posted(page, 1)).toEqual(first);
    expect(postedKey(page, 1)).not.toBe(firstKey);

    await restoreDraft(1);
    fireEvent.change(message(), { target: { value: "Keep this!" } });
    fireEvent.click(screen.getByRole("button", { name: "Send to 1" }));
    await waitFor(() => expect(page.createBroadcast).toHaveBeenCalledTimes(3));
    expect(postedKey(page, 2)).not.toBe(firstKey);
    expect(postedKey(page, 2)).not.toBe(postedKey(page, 1));
    expect(posted(page, 2).body).toBe("Keep this!");
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("Restore draft keeps a mode the sender did not pick unpicked, so it still follows the selection", async () => {
  const page = renderAgents();
  page.createBroadcast.mockImplementationOnce(refuseBroadcast);

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    const broadcast = () => within(region).getByRole("region", { name: "Broadcast" });
    const mode = () =>
      within(broadcast()).getByRole<HTMLSelectElement>("combobox", { name: "Delivery mode" });
    // The Reviewer takes Aside alone, so the selection's own mode is Aside.
    fireEvent.click(
      within(region).getByRole("checkbox", { name: "Select Reviewer for broadcast" })
    );
    fireEvent.change(within(broadcast()).getByRole("textbox", { name: "Broadcast message" }), {
      target: { value: "Report status." },
    });
    expect(mode().value).toBe("aside");
    fireEvent.click(screen.getByRole("button", { name: "Send to 1" }));
    const sends = await screen.findByRole("region", { name: "Sends" });
    await within(sends).findByText(`Could not send to 1 agent: ${BROADCAST_REFUSED}`);
    fireEvent.click(within(sends).getByRole("button", { name: "Restore draft" }));

    expect(mode().value).toBe("aside");
    expect(
      within(broadcast()).getByText(wholeText("Sending as Aside: Reviewer does not advertise BTW."))
    ).toBeTruthy();
    // Still the selection's: with only the Planner, which takes BTW, it is BTW again.
    fireEvent.click(within(region).getByRole("checkbox", { name: "Select Planner for broadcast" }));
    fireEvent.click(
      within(region).getByRole("checkbox", { name: "Select Reviewer for broadcast" })
    );
    expect(mode().value).toBe("btw");
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("a restored broadcast shows the mode it re-sends, even after the selection's own mode changed", async () => {
  const [planner, reviewer] = agents;
  const page = renderAgents({ listedAgents: [planner, reviewer] });
  page.createBroadcast.mockImplementationOnce(refuseBroadcast);

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    const composer = () => within(region).getByRole("region", { name: "Broadcast" });
    const mode = () =>
      within(composer()).getByRole<HTMLSelectElement>("combobox", { name: "Delivery mode" });
    fireEvent.click(
      within(region).getByRole("checkbox", { name: "Select Reviewer for broadcast" })
    );
    fireEvent.change(within(composer()).getByRole("textbox", { name: "Broadcast message" }), {
      target: { value: "Report status." },
    });
    expect(mode().value).toBe("aside");
    fireEvent.click(within(composer()).getByRole("button", { name: "Send to 1" }));
    await waitFor(() => expect(page.createBroadcast).toHaveBeenCalledTimes(1));

    // The Reviewer now takes BTW too, so the selection's own mode would be BTW.
    page.listAgents.mockResolvedValue([planner, { ...reviewer, capabilities: ["aside", "btw"] }]);
    await page.queryClient.refetchQueries({ queryKey: ["agents"] });
    const sends = await screen.findByRole("region", { name: "Sends" });
    await within(sends).findByText(`Could not send to 1 agent: ${BROADCAST_REFUSED}`);
    fireEvent.click(within(sends).getByRole("button", { name: "Restore draft" }));

    // Held word for word, the request shows its own mode and goes in it.
    expect(mode().value).toBe("aside");
    fireEvent.click(within(composer()).getByRole("button", { name: "Send to 1" }));
    await waitFor(() => expect(page.createBroadcast).toHaveBeenCalledTimes(2));
    expect(posted(page, 1)).toEqual(posted(page, 0));
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("a selected agent that went away does not change what Restore draft re-sends, and the composer says so", async () => {
  const [planner, reviewer] = agents;
  const listening = { ...reviewer, capabilities: ["aside", "btw"] };
  const page = renderAgents({ listedAgents: [planner, listening] });
  page.createBroadcast.mockImplementationOnce(refuseBroadcast);

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
    await within(sends).findByText(`Could not send to 2 agents: ${BROADCAST_REFUSED}`);
    fireEvent.click(within(sends).getByRole("button", { name: "Restore draft" }));

    // The restored request names both, so the composer counts both; the Reviewer's chip falls
    // back to its session id, with no reason, since the request asks for it.
    expect(within(composer()).getByRole("heading", { level: 2 }).textContent).toBe(
      "Broadcast to 2 of 2 selected"
    );
    const chip = within(composer()).getByTitle("Remove session:reviewer… from this broadcast");
    expect(chip.textContent).toBe("session:reviewer… ✕");
    // The deselect button keeps the name; the agent's live link and its state sit beside it, the
    // link outside the button.
    const chipAgent = chip.closest("[data-agent-name]");
    expect(chipAgent?.getAttribute("data-agent-state")).toBe("not-running");
    const chipLive = within(chipAgent as HTMLElement).getByRole("link", {
      name: "Open live conversation with session:reviewer…",
    });
    expect(chipLive.getAttribute("href")).toBe("/agents/reviewer-session/live");
    expect(chip.contains(chipLive)).toBe(false);
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
  page.createBroadcast
    .mockImplementationOnce(refuseBroadcast)
    .mockImplementationOnce(refuseBroadcast);
  const notInTheSend =
    "Excluded: Reviewer (not in the refused send; edit to include it). Nothing is sent to them, and no other mode is substituted.";

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    const composer = () => within(region).getByRole("region", { name: "Broadcast" });
    const restoreDraft = async (failures: number) => {
      const sends = await screen.findByRole("region", { name: "Sends" });
      await waitFor(() =>
        expect(
          within(sends).getAllByText(`Could not send to 1 agent: ${BROADCAST_REFUSED}`)
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
      within(composer()).getByText(
        wholeText(
          "Excluded: Reviewer (does not advertise BTW). Nothing is sent to them, and no other mode is substituted."
        )
      )
    ).toBeTruthy();
    fireEvent.click(within(composer()).getByRole("button", { name: "Send to 1" }));
    await waitFor(() => expect(page.createBroadcast).toHaveBeenCalledTimes(1));
    const first = posted(page, 0);
    const firstKey = postedKey(page, 0);
    expect(first.session_ids).toEqual(["planner-session"]);

    // The Reviewer now takes BTW: the live plan would reach it, the refused request does not.
    page.listAgents.mockResolvedValue([planner, { ...reviewer, capabilities: ["aside", "btw"] }]);
    await page.queryClient.refetchQueries({ queryKey: ["agents"] });
    await restoreDraft(1);
    expect(within(composer()).getByRole("heading", { level: 2 }).textContent).toBe(
      "Broadcast to 1 of 2 selected"
    );
    expect(within(composer()).getByText(wholeText(notInTheSend))).toBeTruthy();
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
    expect(postedKey(page, 2)).not.toBe(firstKey);
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
    input: {
      body: "Still here?",
      delivery: "btw",
      session_ids: ["planner-session"],
    } satisfies CreateBroadcastInput,
    key: "refused-key",
  };
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
  expect(composedBroadcast(composition, [planner, reviewer]).input).toBe(restored.input);
  expect(composedBroadcast(composition, [planner, reviewer]).key).toBe("refused-key");
});

// Two deliberate sends of the same words are two broadcasts: each composition carries a key of its
// own, so the server never answers the second with the first.
test("each composed send carries a key of its own, even when it says the same thing", async () => {
  const page = renderAgents();
  // Refused, so each row stays on the strip and the page does not open a broadcast in between.
  page.createBroadcast.mockImplementation(refuseBroadcast);

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
    const keys = [0, 1, 2].map((index) => postedKey(page, index));
    expect(new Set(keys).size).toBe(3);
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// TanStack resumes a paused mutation only while the tab is visible (`focusManager`), so a queue
// built on a mutation `scope` would hold the second send until the reader came back to the tab;
// the queue's own line waits on nothing but the send ahead.
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
    // A request left unanswered would keep its send out after the test, its line waiting behind
    // it.
    held.resolve();
    focusManager.setFocused(undefined);
    page.view.unmount();
    page.restore();
  }
});

/** What a broadcast's row says once it is past its deadline and still being tried. */
const broadcastLate =
  "Still sending to 1 agent — Dispatch has not confirmed it yet. It keeps trying, and the broadcast is made once.";

/** Sends `body` to the Planner from the broadcast composer, then ticks the Planner again: a
 *  broadcast begun since keeps the page on the strip once this one lands. */
async function broadcastToPlanner(region: HTMLElement, body: string): Promise<void> {
  const planner = within(region).getByRole("checkbox", { name: "Select Planner for broadcast" });
  fireEvent.click(planner);
  fireEvent.change(within(region).getByRole("textbox", { name: "Broadcast message" }), {
    target: { value: body },
  });
  fireEvent.click(within(region).getByRole("button", { name: "Send to 1" }));
  await tick(0);
  fireEvent.click(planner);
}

// W2. A broadcast goes under its key as a message to one session does, and is tried as one is: a
// failed fetch, a timeout or a 5xx is no verdict, so the same request is made again after 1 s, 2 s,
// 4 s … never more than 30 s apart, each attempt held to 15 s of silence. Past the send's 30 s
// deadline the strip says Dispatch has not confirmed it yet, and it goes on until Dispatch does.
test("a broadcast Dispatch has not confirmed is tried again under its key until it is", async () => {
  const page = renderAgents();
  const region = await screen.findByRole("region", { name: "Agents" });
  page.createBroadcast
    .mockRejectedValueOnce(new NetworkError("/api/v1/broadcasts", new TypeError("Failed to fetch")))
    .mockRejectedValueOnce(new RequestTimeoutError("/api/v1/broadcasts", KEYED_SEND_ATTEMPT_MS))
    .mockRejectedValueOnce(new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" }))
    .mockRejectedValueOnce(new ApiError(504, { error: "gateway timeout" }))
    .mockRejectedValueOnce(new ApiError(502, { error: "bad gateway" }))
    .mockRejectedValueOnce(
      new NetworkError("/api/v1/broadcasts", new TypeError("Failed to fetch"))
    );
  jest.useFakeTimers();

  try {
    await broadcastToPlanner(region, "Status?");
    const sends = screen.getByRole("region", { name: "Sends" });
    expect(within(sends).getByText("Sending to 1 agent…")).toBeTruthy();
    expect(page.createBroadcast).toHaveBeenCalledTimes(1);

    for (const [index, wait] of [1_000, 2_000, 4_000, 8_000, 16_000, 30_000].entries()) {
      await tick(wait - 1);
      expect(page.createBroadcast).toHaveBeenCalledTimes(index + 1);
      await tick(1);
      expect(page.createBroadcast).toHaveBeenCalledTimes(index + 2);
      // 1 + 2 + 4 + 8 + 16 s is past the send's 30 s deadline: still the server's, not refused.
      if (index === 4) {
        expect(within(sends).getByText(broadcastLate)).toBeTruthy();
        expect(within(sends).getByRole("button", { name: "Stop trying" })).toBeTruthy();
      }
    }
    expect(within(sends).getByRole("link", { name: "Sent to 1 agent" })).toBeTruthy();
    expect(within(sends).queryByRole("button", { name: "Stop trying" })).toBeNull();
    const request = posted(page, 0);
    expect(page.createBroadcast.mock.calls).toEqual(
      Array.from({ length: 7 }, () => [request, keyedAttempt])
    );
    expect(new Set(page.createBroadcast.mock.calls.map(([, bounds]) => bounds.key)).size).toBe(1);
  } finally {
    jest.advanceTimersByTime(1);
    jest.useRealTimers();
    page.view.unmount();
    page.restore();
  }
});

// An outage would hold a late broadcast, and every send behind it, for good. Stop trying ends it
// between attempts: none follows, the row says it was stopped and offers Retry and Restore draft as
// a refusal does, and Retry sends the very request again, key included.
test("Stop trying ends a late broadcast, and Retry sends it again under the same key", async () => {
  const page = renderAgents();
  const region = await screen.findByRole("region", { name: "Agents" });
  page.createBroadcast.mockRejectedValue(
    new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" })
  );
  jest.useFakeTimers();

  try {
    await broadcastToPlanner(region, "Status?");
    const sends = screen.getByRole("region", { name: "Sends" });
    expect(within(sends).queryByRole("button", { name: "Stop trying" })).toBeNull();
    for (const wait of [1_000, 2_000, 4_000, 8_000, 15_000]) await tick(wait);
    expect(page.createBroadcast).toHaveBeenCalledTimes(5);
    expect(within(sends).getByText(broadcastLate)).toBeTruthy();

    await act(async () => {
      fireEvent.click(within(sends).getByRole("button", { name: "Stop trying" }));
    });
    await tick(0);
    expect(
      within(sends).getByText(`Stopped sending to 1 agent: ${new SendStoppedError().message}`)
    ).toBeTruthy();
    expect(within(sends).getByRole("button", { name: "Restore draft" })).toBeTruthy();
    await tick(30 * 60_000);
    expect(page.createBroadcast).toHaveBeenCalledTimes(5);

    page.createBroadcast.mockImplementation(async (input) => ({
      author: { id: "alice", kind: "user" },
      body: input.body,
      created_at: "2026-10-25T00:00:00Z",
      delivery: input.delivery,
      excluded: [],
      id: "broadcast-retried",
      recipients: [{ message: message(input.body), replies: [], session_id: "planner-session" }],
    }));
    await act(async () => {
      fireEvent.click(within(sends).getByRole("button", { name: "Retry" }));
    });
    await tick(0);
    expect(page.createBroadcast).toHaveBeenCalledTimes(6);
    expect(within(sends).getByRole("link", { name: "Sent to 1 agent" })).toBeTruthy();
    expect(new Set(page.createBroadcast.mock.calls.map(([, bounds]) => bounds.key)).size).toBe(1);
  } finally {
    jest.advanceTimersByTime(1);
    jest.useRealTimers();
    page.view.unmount();
    page.restore();
  }
});

/** A reload, as far as kept sends go: the page's code stops, so nothing it still runs keeps
 *  anything more, and what it kept is there for the page after it. */
function reloadTab(): void {
  const tab = window.sessionStorage;
  const kept = Array.from({ length: tab.length }, (_, index) => tab.key(index)).flatMap((name) =>
    name === null ? [] : [[name, tab.getItem(name) ?? ""] as const]
  );
  resetWriteStorage();
  for (const [name, value] of kept) tab.setItem(name, value);
}

// W1. A broadcast out when the page reloads goes on after it under its key, and a refused one
// shows again with Retry and Restore draft: the reloaded app takes both up as it signs in.
test("a broadcast out or refused when the page reloads is there again after it", async () => {
  const before = renderAgents();
  before.createBroadcast
    .mockImplementationOnce(refuseBroadcast)
    .mockReturnValueOnce(Promise.withResolvers<BroadcastCreated>().promise);
  let out: CreateBroadcastInput | undefined;
  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    for (const body of ["Refused.", "Out."]) {
      fireEvent.click(
        within(region).getByRole("checkbox", { name: "Select Planner for broadcast" })
      );
      fireEvent.change(within(region).getByRole("textbox", { name: "Broadcast message" }), {
        target: { value: body },
      });
      fireEvent.click(within(region).getByRole("button", { name: "Send to 1" }));
      await screen.findByTitle(body);
    }
    await waitFor(() => expect(before.createBroadcast).toHaveBeenCalledTimes(2));
    out = posted(before, 1);
  } finally {
    before.view.unmount();
    before.restore();
  }

  reloadTab();
  const after = renderAgents();
  try {
    const sends = await screen.findByRole("region", { name: "Sends" });
    await within(sends).findByRole("link", { name: "Sent to 1 agent" });
    expect(within(sends).getByText(`Could not send to 1 agent: ${BROADCAST_REFUSED}`)).toBeTruthy();
    expect(after.createBroadcast).toHaveBeenCalledTimes(1);
    expect(posted(after, 0)).toEqual(out ?? {});

    fireEvent.click(within(sends).getByRole("button", { name: "Restore draft" }));
    const region = screen.getByRole("region", { name: "Agents" });
    expect(
      within(region).getByRole<HTMLTextAreaElement>("textbox", { name: "Broadcast message" }).value
    ).toBe("Refused.");
  } finally {
    after.view.unmount();
    after.restore();
  }
});

// W1. A message to one session out when the page reloads is made again after it under the key it
// was pressed with, and its live view shows it again as its own turn, Sending….
test("a live-view send out when the page reloads is sent again under its key and shown again", async () => {
  const before = await renderPlannerLiveView();
  before.createAgentMessage.mockReturnValueOnce(Promise.withResolvers<Message>().promise);
  let pressed: (typeof before.createAgentMessage.mock.calls)[number] | undefined;
  try {
    await sendFromLiveView("Status please");
    expect(before.createAgentMessage).toHaveBeenCalledTimes(1);
    pressed = before.createAgentMessage.mock.calls[0];
  } finally {
    before.view.unmount();
    before.restore();
  }

  reloadTab();
  const answer = Promise.withResolvers<Message>();
  const after = renderAgents({ initialEntry: "/agents/planner-session/live" });
  after.createAgentMessage.mockReturnValueOnce(answer.promise);
  try {
    const held = await screen.findByTestId("agent-send-held");
    expect(within(held).getByText("Your message: “Status please”")).toBeTruthy();
    expect(within(held).getByRole("status").textContent).toBe("Sending…");
    expect(after.createAgentMessage.mock.calls).toEqual([
      [
        pressed?.[0],
        pressed?.[1],
        expect.objectContaining({ deadlineMs: KEYED_SEND_ATTEMPT_MS, key: pressed?.[2].key }),
      ],
    ]);

    await act(async () => answer.resolve(message("Status please")));
    await waitFor(() => expect(screen.queryByTestId("agent-send-held")).toBeNull());
    expect(window.sessionStorage.getItem(`dispatch.write:${pressed?.[2].key}`)).toBeNull();
  } finally {
    after.view.unmount();
    after.restore();
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
  const tail = ". Nothing is sent to them, and no other mode is substituted.";
  // The line keeps each excluded session as an agent part, for `AgentName` to draw: by its
  // registry title while it is listed, else by its ID.
  const gone = (sessionId: string) => ({ sessionId, storedTitle: undefined });
  const listed = (agent: Agent) => ({ sessionId: agent.session_id, storedTitle: agent.title });
  // Nobody reached: the label stops counting, and the Excluded line is the notice and the reason.
  const nobody = (line: AgentPart[]) => ({
    label: "No recipient",
    notice: line,
    refusal: line,
    refusalOnNotice: true,
  });

  // Every selected session has left the registry: the line names each by its ID, and no mode
  // would help.
  expect(state(both, [], "btw")).toEqual(
    nobody([
      "Excluded: ",
      gone(planner.session_id),
      " (no live session)",
      ", ",
      gone(reviewer.session_id),
      " (no live session)",
      tail,
    ])
  );
  // One gone, one without the mode: each with its own cause, then the mode that reaches the
  // live one.
  expect(state(both, [reviewer], "btw")).toEqual(
    nobody([
      "Excluded: ",
      gone(planner.session_id),
      " (no live session)",
      ", ",
      listed(reviewer),
      " (does not advertise BTW)",
      `${tail} Sending as Aside would reach 1 of them.`,
    ])
  );
  // The hint picks the mode that reaches the most: Aside reaches both, BTW only the Planner.
  expect(state(both, agents, "steer").refusal?.at(-1)).toMatch(
    / Sending as Aside would reach 2 of them\.$/
  );
  // One session alone is "it"; a session advertising nothing gets no hint at all.
  expect(state([reviewer.session_id], [reviewer], "btw").refusal?.at(-1)).toMatch(
    / Sending as Aside would reach it\.$/
  );
  expect(state([deaf.session_id], [deaf], "btw")).toEqual(
    nobody(["Excluded: ", listed(deaf), " (does not advertise BTW)", tail])
  );

  // Someone reached: the label counts, and an Excluded line is context beside Send, never its
  // reason, with no hint since the mode reaches someone.
  const reviewerLeftOut = ["Excluded: ", listed(reviewer), " (does not advertise BTW)", tail];
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
    refusal: ["Type a message first."],
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
    notice: [limit],
    refusal: [limit],
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
    const search = within(region).getByRole("searchbox", { name: "Search agents" });
    expect(within(region).getByText("2 matching")).toBeTruthy();

    // Everything, then narrow: the header counts the matching row and names the other one.
    fireEvent.click(header);
    expect(within(region).getByText("2 of 2 matching selected")).toBeTruthy();
    // Picked, so the Aside-only Reviewer is left out of the count below.
    fireEvent.change(
      within(within(region).getByRole("region", { name: "Broadcast" })).getByRole("combobox", {
        name: "Delivery mode",
      }),
      { target: { value: "btw" } }
    );
    fireEvent.change(search, { target: { value: "PLANNER" } });
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
    fireEvent.change(search, { target: { value: "" } });
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

test("the header checkbox selects a row a collapsed group hides, and the group says how many", async () => {
  // Seen half a minute ago and never heard from by Dispatch: Idle.
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
    fireEvent.click(groupToggle(region, "idle"));
    expect(within(region).queryByRole("heading", { level: 2, name: "Silent" })).toBeNull();
    expect(groupCount(region, "idle")).toBe("(1)");
    fireEvent.click(within(region).getByRole("checkbox", { name: "Select all matching agents" }));
    expect(within(region).getByText("2 of 2 matching selected")).toBeTruthy();
    expect(groupCount(region, "idle")).toBe("(1, 1 selected)");
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

// LEGION-540. A send's row in the Sends strip names the message it carries, which is Markdown
// its author wrote: formatted on the row's one line, with all of its words on hover - the row is
// the only copy of a refused message, so the hover keeps every word, and never shows its syntax.
test("a refused send's row shows its message formatted on one line, with its whole text on hover", async () => {
  const page = renderAgents();
  page.createBroadcast.mockImplementationOnce(refuseBroadcast);
  const body = "**Stop** the deploy\n\n1. check `main`\n2. report back";
  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    fireEvent.click(within(region).getByRole("checkbox", { name: "Select Planner for broadcast" }));
    fireEvent.change(
      within(within(region).getByRole("region", { name: "Broadcast" })).getByRole("textbox", {
        name: "Broadcast message",
      }),
      { target: { value: body } }
    );
    fireEvent.click(screen.getByRole("button", { name: "Send to 1" }));
    const sends = await screen.findByRole("region", { name: "Sends" });
    await within(sends).findByText(`Could not send to 1 agent: ${BROADCAST_REFUSED}`);
    await waitFor(() => expect(sends.querySelector("strong")?.textContent).toBe("Stop"));
    const preview = sends.querySelector<HTMLElement>("[data-markdown-preview]");
    if (preview === null) throw new Error("the send's row shows no preview of its message");
    expect(preview.querySelector("code")?.textContent).toBe("main");
    expect(preview.textContent?.replace(/\s+/g, " ").trim()).toBe(
      "Stop the deploy check main report back"
    );
    expect(preview.querySelector("ol, li, p")).toBeNull();
    await waitFor(() =>
      expect(preview.getAttribute("title")).toBe("Stop the deploy check main report back")
    );
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// A collapsed group keeps its rows mounted, hidden, so a row the reader had open is still
// expanded in there. Nobody can see it, so a reply that arrives then must stay unread - in the
// navigation and on the row - until the group opens and puts the conversation back on screen.
test("an open row a collapsed group hides marks nothing read, and opening the group reads it", async () => {
  const asked = exchange("m1", "Still there?", "2026-09-14T01:00:00Z");
  // The session's own question waits on the reader, so it is under Needs you before the reply
  // arrives and after it: the group holding it does not change under the hidden row.
  const page = renderAgents({
    inboxRows: [{ ...inbox[0], author: { id: "stale-session", kind: "session" }, id: "ask-stale" }],
    listedAgents: [stale, ...agents],
    messages: [asked],
  });

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    const staleCard = card(region, "Stale");
    expand(staleCard, "Stale");
    await within(staleCard).findByText("Still there?");
    const fold = groupToggle(region, "needs-you");
    fireEvent.click(fold);
    expectKeptHidden(region, "Stale");

    const answered = exchange("m1", "Still there?", "2026-09-14T01:00:00Z", {
      body: "Yes, still here.",
      createdAt: "2026-09-14T01:05:00Z",
      unread: true,
    });
    page.listAgentMessages.mockResolvedValue([
      {
        ...answered,
        replies: answered.replies.map((reply) => ({
          ...reply,
          author: { id: "stale-session", kind: "session" as const },
        })),
      },
    ]);
    page.getMyAgentState.mockResolvedValue({ "stale-session": { unread_replies: 1 } });
    await page.queryClient.invalidateQueries();
    await within(staleCard).findByText("Yes, still here.");
    // A mark the hidden row sent would go out from an effect a few microtasks on; give it a
    // task, so the assertion below is not merely early.
    const settled = Promise.withResolvers<void>();
    setTimeout(settled.resolve, 20);
    await settled.promise;
    expect(page.putAgentState).not.toHaveBeenCalled();
    expect(screen.getByRole("link", { name: "New reply 1" })).toBeTruthy();

    fireEvent.click(fold);
    await waitFor(() =>
      expect(page.putAgentState).toHaveBeenCalledWith(
        "stale-session",
        { read_through: "2026-09-14T01:05:00Z" },
        markAttempt
      )
    );
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// LEGION-540. A session's answer on the Agents page is Markdown it wrote, shown whole: a list in
// it reads as a list (the `inline` rendering this row used before ran it onto one line and
// dropped every item's formatting after the first block).
test("an agent's answer on its row renders as a Markdown block, lists included", async () => {
  const page = renderAgents({
    messages: [
      exchange("m1", "Status?", "2026-09-14T01:00:00Z", {
        body: "Two things:\n\n1. **Stopped** the deploy\n2. `main` is green",
        createdAt: "2026-09-14T01:05:00Z",
      }),
    ],
  });
  try {
    const planner = card(await screen.findByRole("region", { name: "Agents" }), "Planner");
    expand(planner, "Planner");
    const conversation = await within(planner).findByRole("list", {
      name: "Conversation with Planner",
    });
    await waitFor(() =>
      expect(
        Array.from(
          conversation.querySelectorAll(".dispatch-markdown ol > li"),
          (item) => item.textContent
        )
      ).toEqual(["Stopped the deploy", "main is green"])
    );
    // Scoped to the rendered body: the conversation is itself a list of turns.
    expect(conversation.querySelector(".dispatch-markdown ol > li > strong")?.textContent).toBe(
      "Stopped"
    );
    expect(conversation.querySelector(".dispatch-markdown ol > li > code")?.textContent).toBe(
      "main"
    );
    expect(conversation.textContent).not.toContain("**");
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// A collapse keeps the row's picker as the reader left it, open included, so `i` opens the
// picker rather than toggling it: on a picker already open it goes into the select instead of
// clicking it shut.
test("i on a row collapsed with its picker open goes back into the picker's select", async () => {
  const page = renderAgents({ issues: [coreIssue] });

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    const planner = card(region, "Planner");
    expand(planner, "Planner");
    const toggle = within(planner).getByRole("button", { name: "Choose issue" });
    fireEvent.click(toggle);
    await within(planner).findByRole("combobox", { name: "Issue" });
    expand(planner, "Planner");
    expect(toggle.getAttribute("aria-expanded")).toBe("true");

    planner.focus();
    fireEvent.keyDown(planner, { key: "i" });
    const select = within(planner).getByRole("combobox", { hidden: true, name: "Issue" });
    await waitFor(() => expect(document.activeElement).toBe(select));
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// One failed poll of the registry is not the agents leaving: each row keeps what it holds - here
// a draft - and the page says the list could not be refreshed.
test("a registry poll that fails keeps every row and what it holds, and says so", async () => {
  const page = renderAgents();

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    const planner = card(region, "Planner");
    expand(planner, "Planner");
    const field = within(planner).getByRole("textbox", { name: "Comment" }) as HTMLTextAreaElement;
    fireEvent.change(field, { target: { value: "Half a thought" } });

    page.listAgents.mockRejectedValue(new Error("Envoy listener unreachable"));
    await page.queryClient.refetchQueries({ queryKey: ["agents"] });
    expect((await screen.findByRole("alert")).textContent).toBe(
      "Could not refresh agents: Envoy listener unreachable"
    );
    expect(card(region, "Planner")).toBe(planner);
    expect(field.isConnected).toBe(true);
    expect(field.value).toBe("Half a thought");
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// A row whose session leaves the registry unmounts; the send it had out is the held-send store's,
// which holds it for the session. The row that comes back shows that send, held - its picker and
// its draft - until it lands, and then they are the reader's.
test("a row back after its session left mid-send holds the send it left until it lands", async () => {
  const sent = Promise.withResolvers<Message>();
  const page = renderAgents({ issues: [coreIssue] });
  page.createAgentMessage.mockImplementationOnce(() => sent.promise);

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    const planner = card(region, "Planner");
    expand(planner, "Planner");
    fireEvent.change(within(planner).getByRole("textbox", { name: "Comment" }), {
      target: { value: "Status please" },
    });
    fireEvent.submit(within(planner).getByRole("form", { name: "Comment composer" }));
    await waitFor(() =>
      expect(
        (within(planner).getByRole("button", { name: "Choose issue" }) as HTMLButtonElement)
          .disabled
      ).toBe(true)
    );

    page.listAgents.mockResolvedValue([agents[1]]);
    await page.queryClient.refetchQueries({ queryKey: ["agents"] });
    await waitFor(() =>
      expect(within(region).queryByRole("heading", { name: "Planner" })).toBeNull()
    );
    page.listAgents.mockResolvedValue(agents);
    await page.queryClient.refetchQueries({ queryKey: ["agents"] });
    const back = card(await screen.findByRole("region", { name: "Agents" }), "Planner");
    expect(back).not.toBe(planner);
    // A new row for the same session, open where the reader left it: the page holds which rows
    // are open, not the row, because the window decides which rows are mounted.
    expect(
      within(back).getByRole("button", { name: "Planner" }).getAttribute("aria-expanded")
    ).toBe("true");
    const choose = () =>
      within(back).getByRole("button", { name: "Choose issue" }) as HTMLButtonElement;
    const field = () =>
      within(back).getByRole("textbox", { name: "Comment" }) as HTMLTextAreaElement;
    // The composer holds its draft through its control fieldset.
    const held = () => field().closest("fieldset")?.disabled;
    expect(choose().disabled).toBe(true);
    expect(held()).toBe(true);
    expect(field().value).toBe("Status please");

    await act(async () => {
      sent.resolve(message("Status please"));
      await sent.promise;
    });
    await waitFor(() => expect(choose().disabled).toBe(false));
    expect(held()).toBe(false);
    expect(field().value).toBe("");
  } finally {
    sent.resolve(message("Status please"));
    page.view.unmount();
    page.restore();
  }
});

// A filter hides the rows it excludes rather than dropping them, as a closed fold does, so
// narrowing the list and widening it again loses nothing a row holds.
test("a row a filter hides keeps its draft and comes back as the reader left it", async () => {
  const page = renderAgents();

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    const planner = card(region, "Planner");
    expand(planner, "Planner");
    const field = within(planner).getByRole("textbox", { name: "Comment" }) as HTMLTextAreaElement;
    fireEvent.change(field, { target: { value: "Half a thought" } });
    const search = within(region).getByRole("searchbox", { name: "Search agents" });

    fireEvent.change(search, { target: { value: "REVIEWER" } });
    expectKeptHidden(region, "Planner");
    fireEvent.change(search, { target: { value: "" } });

    expect(card(region, "Planner")).toBe(planner);
    expect(
      within(planner).getByRole("button", { name: "Planner" }).getAttribute("aria-expanded")
    ).toBe("true");
    expect(field.value).toBe("Half a thought");
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("the search box reads its query from the page's address and writes back to it as the reader types", async () => {
  const page = renderAgents({ initialEntry: "/agents?q=planner" });

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    const search = within(region).getByRole("searchbox", {
      name: "Search agents",
    }) as HTMLInputElement;
    // A reload lands on this address, so the box opens already narrowed to what it last held.
    expect(search.value).toBe("planner");
    expect(within(region).queryByRole("heading", { level: 2, name: "Reviewer" })).toBeNull();

    fireEvent.change(search, { target: { value: "reviewer" } });
    await waitFor(() =>
      expect(screen.getByTestId("location-search").textContent).toBe("?q=reviewer")
    );
    await waitFor(() =>
      expect(within(region).queryByRole("heading", { level: 2, name: "Planner" })).toBeNull()
    );

    fireEvent.change(search, { target: { value: "" } });
    await waitFor(() => expect(screen.getByTestId("location-search").textContent).toBe(""));
    await screen.findByRole("heading", { level: 2, name: "Planner" });
  } finally {
    page.view.unmount();
    page.restore();
  }
});

/** A session on its own machine, so the machine filter has more than two rows to narrow. */
const deployer: Agent = {
  ...agents[1],
  host: "deploy-host",
  last_activity: new Date(now - 60_000).toISOString(),
  last_seen: now - 10_000,
  roles: ["deployer"],
  session_id: "deploy-session",
  title: "Deployer",
};

/** The three filters of the row, by the concept each picks from. */
const FILTER_LABELS = { group: "Group", machine: "Machine", role: "Role" } as const;

/** A filter's trigger, which says what is picked. */
function filterTrigger(region: HTMLElement, filter: keyof typeof FILTER_LABELS): HTMLElement {
  return within(within(region).getByTestId(`agents-filter-${filter}`)).getByRole("button", {
    name: `Filter by ${filter}`,
  });
}

/** The options of one filter's open panel, which is portalled out of the page, so it is found
 *  by its own listbox rather than among every option in the document. */
function optionTexts(filter: keyof typeof FILTER_LABELS): (string | null)[] {
  const panel = screen.getByRole("listbox", { name: `${FILTER_LABELS[filter]} options` });
  return within(panel)
    .getAllByRole("option")
    .map((option) => option.textContent);
}

/** One option of a filter's open panel, by its text. */
function filterOption(filter: keyof typeof FILTER_LABELS, name: string): HTMLElement {
  const panel = screen.getByRole("listbox", { name: `${FILTER_LABELS[filter]} options` });
  return within(panel).getByRole("option", { name });
}

// R7: never a dropdown. Each of the three filters opens a panel with a text box that narrows
// its own options, takes more than one, says how many are picked, and clears them.
test("the machine filter is a searchable multi-select that keeps the rows of every machine picked", async () => {
  const page = renderAgents({ listedAgents: [...agents, deployer] });

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    const titles = () =>
      within(region)
        .getAllByRole("heading", { hidden: false, level: 2 })
        .map((heading) => heading.textContent);
    // No dropdown in the filter row: the three filters are buttons over panels.
    expect(within(within(region).getByTestId("agents-filters")).queryAllByRole("combobox")).toEqual(
      []
    );

    const machine = filterTrigger(region, "machine");
    expect(machine.textContent).toBe("Machine");
    fireEvent.click(machine);
    // Only the machines the live sessions occupy, in order.
    expect(optionTexts("machine")).toEqual(["build-host", "deploy-host", "review-host"]);

    // The text box narrows this panel's own options as the reader types.
    const search = screen.getByRole("combobox", { name: "Search machines" });
    fireEvent.change(search, { target: { value: "DEPLOY" } });
    expect(optionTexts("machine")).toEqual(["deploy-host"]);
    fireEvent.change(search, { target: { value: "" } });

    // Two picked, and the list keeps the rows of either.
    fireEvent.click(filterOption("machine", "build-host"));
    fireEvent.click(filterOption("machine", "deploy-host"));
    expect(machine.textContent).toBe("Machine · 2");
    expect(titles()).toEqual(["Planner", "Deployer"]);

    // And one control empties them.
    fireEvent.click(screen.getByRole("button", { name: "Clear machine" }));
    expect(machine.textContent).toBe("Machine");
    expect(titles()).toEqual(["Planner", "Deployer", "Reviewer"]);
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("the group and role filters are searchable multi-selects of their own", async () => {
  const page = renderAgents({ listedAgents: [...agents, deployer] });

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    const titles = () =>
      within(region)
        .getAllByRole("heading", { hidden: false, level: 2 })
        .map((heading) => heading.textContent);

    const group = filterTrigger(region, "group");
    fireEvent.click(group);
    expect(optionTexts("group")).toEqual(["Needs you", "Active", "Idle", "Quiet"]);
    fireEvent.change(screen.getByRole("combobox", { name: "Search groups" }), {
      target: { value: "act" },
    });
    expect(optionTexts("group")).toEqual(["Active"]);
    fireEvent.click(filterOption("group", "Active"));
    expect(group.textContent).toBe("Group · 1");
    // The Deployer spoke a minute ago; the Planner's asks wait on the reader and the Reviewer
    // was last heard from ten minutes ago.
    expect(titles()).toEqual(["Deployer"]);
    expect(groupCount(region, "needs-you")).toBe("(0)");
    expect(groupCount(region, "active")).toBe("(1)");
    fireEvent.click(screen.getByRole("button", { name: "Clear group" }));
    fireEvent.keyDown(screen.getByRole("combobox", { name: "Search groups" }), { key: "Escape" });

    const role = filterTrigger(region, "role");
    fireEvent.click(role);
    expect(optionTexts("role")).toEqual(["deployer", "planner"]);
    fireEvent.change(screen.getByRole("combobox", { name: "Search roles" }), {
      target: { value: "plan" },
    });
    expect(optionTexts("role")).toEqual(["planner"]);
    fireEvent.click(filterOption("role", "planner"));
    expect(role.textContent).toBe("Role · 1");
    expect(titles()).toEqual(["Planner"]);
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("/ focuses the Agents search box instead of opening the Dispatch-wide search dialog", async () => {
  const page = renderAgents();

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    const search = within(region).getByRole("searchbox", { name: "Search agents" });
    expect(search).not.toBe(document.activeElement);

    fireEvent.keyDown(document.body, { key: "/" });

    expect(document.activeElement).toBe(search);
    expect(screen.queryByRole("dialog", { name: "Search" })).toBeNull();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("/ falls through to the Dispatch-wide search dialog when no agents are connected, since no box is there to focus", async () => {
  const page = renderAgents({ listedAgents: [] });

  try {
    await screen.findByText("No agents are connected.");
    expect(screen.queryByRole("searchbox", { name: "Search agents" })).toBeNull();

    fireEvent.keyDown(document.body, { key: "/" });

    expect(screen.getByRole("dialog", { name: "Search" })).toBeTruthy();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

/** Moves the fake clock `ms` on and lets every promise the move released run. */
async function tick(ms: number): Promise<void> {
  await act(async () => {
    jest.advanceTimersByTime(ms);
    for (let turn = 0; turn < 20; turn += 1) await Promise.resolve();
  });
}

const keyedStopped = new SendStoppedError().message;

/** Opens `name`'s live view from its Agents row, by the row's own live link (`AgentName`'s). */
function openLiveView(name: string): void {
  const region = screen.getByRole("region", { name: "Agents" });
  fireEvent.click(
    within(card(region, name)).getByRole("link", { name: `Open live conversation with ${name}` })
  );
}

/** Leaves a live view for the Agents page, by the view's own link. */
function backToAgents(): void {
  fireEvent.click(screen.getByRole("link", { name: "← Agents" }));
}

/** The app with Planner's live view open, both lazy pages loaded once on the real clock, so every
 *  move between them on the fake clock that follows renders at once. A message to a session is
 *  answered 503 until a test says otherwise: Dispatch is down. `listedAgents` are the live
 *  sessions, Planner among them. */
async function renderPlannerLiveView(listedAgents: Agent[] = agents) {
  const page = renderAgents({ listedAgents });
  page.createAgentMessage.mockRejectedValue(
    new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" })
  );
  await screen.findByRole("region", { name: "Agents" });
  openLiveView("Planner");
  await screen.findByRole("combobox", { name: "Delivery mode" });
  return page;
}

/** Sends `body` from the open live view. */
async function sendFromLiveView(body: string): Promise<void> {
  fireEvent.change(screen.getByPlaceholderText(/delivered as Aside/), { target: { value: body } });
  await act(async () => {
    fireEvent.click(
      within(screen.getByTestId("agent-composer")).getByRole("button", { name: "Send" })
    );
  });
}

// A send belongs to its session, not to the view that made it: one the live view made during an
// outage is still being tried while the reader looks at another session and at the Agents page,
// under the key it was pressed with, and the view shows it again on return. It ends when the
// reader stops it.
test("a live-view send that keeps failing outlives the view, under one key, until Stop trying", async () => {
  const page = await renderPlannerLiveView();
  jest.useFakeTimers();

  try {
    await sendFromLiveView("Status please");
    expect(page.createAgentMessage).toHaveBeenCalledTimes(1);
    await tick(1_000);
    expect(page.createAgentMessage).toHaveBeenCalledTimes(2);

    // Another session's live view holds nothing of Planner's: only its own empty box refuses Send.
    await act(async () => backToAgents());
    await act(async () => openLiveView("Reviewer"));
    expect(screen.getByPlaceholderText(/^Message Reviewer/)).toBeTruthy();
    expect(screen.queryByTestId("agent-send-held")).toBeNull();
    expect(
      within(screen.getByTestId("agent-composer"))
        .getByRole("button", { name: "Send" })
        .getAttribute("title")
    ).toBe("Type a message first.");
    await tick(2_000);
    expect(page.createAgentMessage).toHaveBeenCalledTimes(3);

    // Back on Planner: the same send, still out.
    await act(async () => backToAgents());
    await act(async () => openLiveView("Planner"));
    const held = screen.getByTestId("agent-send-held");
    expect(within(held).getByText("Your message: “Status please”")).toBeTruthy();
    expect(within(held).getByRole("status").textContent).toBe("Sending…");
    await tick(4_000);
    await tick(8_000);
    expect(page.createAgentMessage).toHaveBeenCalledTimes(5);
    await tick(15_000);
    expect(within(held).getByRole("status").textContent).toBe(KEYED_SEND_LATE_TEXT);

    await act(async () => {
      fireEvent.click(within(held).getByRole("button", { name: "Stop trying" }));
    });
    await tick(0);
    expect(screen.getByTestId("agent-send-error").textContent).toBe(keyedStopped);
    await tick(30 * 60_000);
    expect(page.createAgentMessage).toHaveBeenCalledTimes(5);
    expect(
      new Set(
        page.createAgentMessage.mock.calls.map(
          ([session, input, bounds]) => `${session} ${input.body} ${bounds.key}`
        )
      ).size
    ).toBe(1);
    expect(page.createAgentMessage.mock.calls[0]?.[0]).toBe("planner-session");

    fireEvent.click(within(held).getByRole("button", { name: "Discard" }));
    expect(screen.queryByTestId("agent-send-held")).toBeNull();
  } finally {
    jest.advanceTimersByTime(1);
    jest.useRealTimers();
    page.view.unmount();
    page.restore();
  }
});

// A send to a session belongs to the session, the write's target: the session's Agents row shows
// the send its live view made as a turn of its own, in the session's one press-order line, while
// the row's own composer stays the reader's - empty and free. The row's Stop trying ends that
// write, and the row's Retry sends it again under the same key, where it lands, and it leaves both
// surfaces.
test("the session's Agents row shows a live-view send as its own turn and can stop and retry it", async () => {
  const page = await renderPlannerLiveView();
  jest.useFakeTimers();

  try {
    await sendFromLiveView("Status please");
    await tick(1_000);
    expect(page.createAgentMessage).toHaveBeenCalledTimes(2);

    await act(async () => backToAgents());
    const region = screen.getByRole("region", { name: "Agents" });
    const planner = card(region, "Planner");
    await act(async () => expand(planner, "Planner"));
    const held = within(planner).getByTestId("agent-send-held");
    expect(within(held).getByText("Your message: “Status please”")).toBeTruthy();
    expect(within(held).getByRole("status").textContent).toBe("Sending…");
    const field = within(planner).getByRole("textbox", { name: "Comment" }) as HTMLTextAreaElement;
    expect(field.value).toBe("");
    expect(field.closest("fieldset")?.disabled).toBe(false);

    for (const wait of [2_000, 4_000, 8_000, 15_000]) await tick(wait);
    expect(page.createAgentMessage).toHaveBeenCalledTimes(5);
    expect(within(held).getByRole("status").textContent).toBe(KEYED_SEND_LATE_TEXT);
    await act(async () => {
      fireEvent.click(within(held).getByRole("button", { name: "Stop trying" }));
    });
    await tick(0);
    expect(within(planner).getByTestId("agent-send-error").textContent).toBe(keyedStopped);
    await tick(30 * 60_000);
    expect(page.createAgentMessage).toHaveBeenCalledTimes(5);

    // Retry from the row is the same message, so it goes under the same key, and lands.
    page.createAgentMessage.mockResolvedValueOnce(message("Status please"));
    await act(async () => {
      fireEvent.click(
        within(within(planner).getByTestId("agent-send-held")).getByRole("button", {
          name: "Retry",
        })
      );
    });
    await tick(0);
    expect(page.createAgentMessage).toHaveBeenCalledTimes(6);
    expect(new Set(messageKeys(page.createAgentMessage)).size).toBe(1);
    expect(within(planner).queryByTestId("agent-send-held")).toBeNull();
  } finally {
    jest.advanceTimersByTime(1);
    jest.useRealTimers();
    page.view.unmount();
    page.restore();
  }
});

// A message the live view sent in a mode the reader picked is that message, whoever retries it:
// the Agents row's Retry of a stopped live-view send re-sends the held request as it was - its
// body, the picked mode and its key - and not the row's own plain mode under a key of its own,
// which would reach the session a second time as a different message.
test("the Agents row's Retry of a live-view send re-sends it as it was", async () => {
  const both: Agent = { ...agents[0], capabilities: ["aside", "steer"] };
  const page = await renderPlannerLiveView([both, agents[1]]);
  const mode = screen.getByRole("combobox", { name: "Delivery mode" }) as HTMLSelectElement;
  await waitFor(() => expect(mode.value).toBe("steer"));
  fireEvent.change(mode, { target: { value: "aside" } });
  await screen.findByPlaceholderText(/delivered as Aside/);
  jest.useFakeTimers();

  try {
    await sendFromLiveView("Status please");
    for (const wait of [1_000, 2_000, 4_000, 8_000, 15_000]) await tick(wait);
    expect(page.createAgentMessage).toHaveBeenCalledTimes(5);
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Stop trying" }));
    });
    await tick(0);
    const pressed = page.createAgentMessage.mock.calls[0]?.[1];
    const pressedKey = page.createAgentMessage.mock.calls[0]?.[2].key;
    expect(pressed).toEqual({ body: "Status please", delivery: "aside" });

    await act(async () => backToAgents());
    const planner = card(screen.getByRole("region", { name: "Agents" }), "Planner");
    await act(async () => expand(planner, "Planner"));
    page.createAgentMessage.mockResolvedValueOnce(message("Status please"));
    await act(async () => {
      fireEvent.click(
        within(within(planner).getByTestId("agent-send-held")).getByRole("button", {
          name: "Retry",
        })
      );
    });
    await tick(0);
    expect(page.createAgentMessage).toHaveBeenCalledTimes(6);
    expect(page.createAgentMessage.mock.calls[5]?.[1]).toEqual(pressed);
    // Stopped, so Dispatch gave no verdict: the Retry goes under the key the press had.
    expect(page.createAgentMessage.mock.calls[5]?.[2].key).toBe(pressedKey);
    expect(within(planner).queryByTestId("agent-send-held")).toBeNull();
  } finally {
    jest.advanceTimersByTime(1);
    jest.useRealTimers();
    page.view.unmount();
    page.restore();
  }
});

// The session is the write's target: a refused live-view message shows on the Agents row too, and
// the row's Discard drops the one write, so the live view shows it no longer.
test("a refused live-view send shows on the Agents row, and Discard there drops it from both", async () => {
  const page = await renderPlannerLiveView();
  try {
    page.createAgentMessage.mockRejectedValueOnce(
      new ApiError(400, { code: "MESSAGE_INPUT", error: "Dispatch refused the message" })
    );
    await sendFromLiveView("Status please");
    await screen.findByTestId("agent-send-error");

    await act(async () => backToAgents());
    const planner = card(screen.getByRole("region", { name: "Agents" }), "Planner");
    await act(async () => expand(planner, "Planner"));
    const held = within(planner).getByTestId("agent-send-held");
    expect(within(held).getByText("Your message: “Status please”")).toBeTruthy();
    expect(within(held).getByTestId("agent-send-error").textContent).toBe(
      "Could not send: Dispatch refused the message"
    );
    const field = within(planner).getByRole("textbox", { name: "Comment" }) as HTMLTextAreaElement;
    expect(field.value).toBe("");

    fireEvent.click(within(held).getByRole("button", { name: "Discard" }));
    expect(within(planner).queryByTestId("agent-send-held")).toBeNull();
    await act(async () => openLiveView("Planner"));
    expect(screen.queryByTestId("agent-send-held")).toBeNull();
    expect(page.createAgentMessage).toHaveBeenCalledTimes(1);
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// A live-view send's mode is that send's own: the Agents row shows a BTW the live view sent as a
// turn of its own, and its composer warns only about the row's own message, in the row's own
// plain mode, before and after the reader writes there.
test("the Agents row's composer warns about its own message, not a live-view send", async () => {
  const page = await renderPlannerLiveView();
  const plainNotice = /does not advertise Send/;
  try {
    const mode = screen.getByRole("combobox", { name: "Delivery mode" }) as HTMLSelectElement;
    fireEvent.change(mode, { target: { value: "btw" } });
    await screen.findByPlaceholderText(/delivered as BTW/);
    page.createAgentMessage.mockRejectedValueOnce(
      new ApiError(400, { code: "MESSAGE_INPUT", error: "Dispatch refused the message" })
    );
    fireEvent.change(screen.getByPlaceholderText(/delivered as BTW/), {
      target: { value: "Status please" },
    });
    await act(async () => {
      fireEvent.click(
        within(screen.getByTestId("agent-composer")).getByRole("button", { name: "Send" })
      );
    });
    await screen.findByTestId("agent-send-error");
    expect(page.createAgentMessage.mock.calls[0]?.[1].delivery).toBe("btw");

    await act(async () => backToAgents());
    const planner = card(screen.getByRole("region", { name: "Agents" }), "Planner");
    await act(async () => expand(planner, "Planner"));
    expect(within(planner).getByTestId("agent-send-held")).toBeTruthy();
    expect(within(planner).getByText(plainNotice)).toBeTruthy();

    fireEvent.change(within(planner).getByRole("textbox", { name: "Comment" }), {
      target: { value: "Status please, now" },
    });
    expect(within(planner).getByText(plainNotice)).toBeTruthy();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// W8. What the reader writes over a refusal is the only copy of that message, and a reload must
// keep it as last typed: the composer writes it back to the held send as the page goes, so the
// page after the reload shows the edit rather than the words the refusal was made with.
test("an edit typed over a refused row survives a reload", async () => {
  const before = renderAgents();
  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    const planner = card(region, "Planner");
    await act(async () => expand(planner, "Planner"));
    before.createAgentMessage.mockRejectedValueOnce(
      new ApiError(400, { code: "MESSAGE_INPUT", error: "Dispatch refused the message" })
    );
    const field = within(planner).getByRole("textbox", { name: "Comment" });
    fireEvent.change(field, { target: { value: "Status please" } });
    await act(async () => {
      fireEvent.submit(within(planner).getByRole("form", { name: "Comment composer" }));
    });
    await within(planner).findByText("Couldn't send — Dispatch refused the message");

    fireEvent.change(field, { target: { value: "Status please, now" } });
    act(() => {
      window.dispatchEvent(new Event("pagehide"));
    });
  } finally {
    before.view.unmount();
    before.restore();
  }

  reloadTab();
  const after = renderAgents();
  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    const planner = card(region, "Planner");
    await act(async () => expand(planner, "Planner"));
    const field = within(planner).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });
    expect(field.value).toBe("Status please, now");
    expect(within(planner).getByText("Couldn't send — Dispatch refused the message")).toBeTruthy();
    expect(after.createAgentMessage).not.toHaveBeenCalled();
  } finally {
    after.view.unmount();
    after.restore();
  }
});

// W6. A kept send belongs to the person who pressed it: a tab alice left a send in, that bob
// signs into, sends nothing of hers, shows no row for it, and drops what was kept.
test("a send alice left behind is not resumed for bob, and is dropped", async () => {
  const alice = renderAgents();
  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    const planner = card(region, "Planner");
    await act(async () => expand(planner, "Planner"));
    alice.createAgentMessage.mockReturnValueOnce(Promise.withResolvers<Message>().promise);
    fireEvent.change(within(planner).getByRole("textbox", { name: "Comment" }), {
      target: { value: "Status please" },
    });
    await act(async () => {
      fireEvent.submit(within(planner).getByRole("form", { name: "Comment composer" }));
    });
    await waitFor(() => expect(alice.createAgentMessage).toHaveBeenCalledTimes(1));
    // Her send is kept in the tab, so the check after the reload is of something that was there.
    expect(
      Array.from({ length: window.sessionStorage.length }, (_, index) =>
        window.sessionStorage.key(index)
      ).filter((name) => name?.startsWith("dispatch.write:"))
    ).toHaveLength(1);
  } finally {
    alice.view.unmount();
    alice.restore();
  }

  reloadTab();
  const bob = renderAgents();
  bob.whoAmI.mockResolvedValue({ event_head: "0", kind: "user", login: "bob" });
  bob.queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "bob" });
  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    const planner = card(region, "Planner");
    await act(async () => expand(planner, "Planner"));
    expect(bob.createAgentMessage).not.toHaveBeenCalled();
    expect(screen.queryByTestId("agent-send-held")).toBeNull();
    expect(screen.queryByText("Status please")).toBeNull();
    expect(
      within(planner).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" }).value
    ).toBe("");
    // Alice's kept write is gone from the tab, not merely left unshown.
    await waitFor(() =>
      expect(
        Array.from({ length: window.sessionStorage.length }, (_, index) =>
          window.sessionStorage.key(index)
        ).filter((name) => name?.startsWith("dispatch.write:"))
      ).toEqual([])
    );
  } finally {
    bob.view.unmount();
    bob.restore();
  }
});

// W21. A send pressed while the browser is offline is tried, not parked: the attempt is made,
// the composer says Dispatch has not confirmed it once the deadline passes and offers Stop
// trying, and the message lands once the network is back. A mutation TanStack paused offline
// would make no attempt, start no deadline and offer no way out.
test("a send pressed offline goes late with Stop trying, and lands when the network returns", async () => {
  const online = onlineManager.isOnline();
  const page = await renderPlannerLiveView();
  page.createAgentMessage.mockRejectedValue(
    new NetworkError("/api/v1/agents/planner-session/messages", new TypeError("Failed to fetch"))
  );
  jest.useFakeTimers();

  try {
    onlineManager.setOnline(false);
    await sendFromLiveView("Status please");
    expect(page.createAgentMessage).toHaveBeenCalledTimes(1);
    const held = screen.getByTestId("agent-send-held");
    expect(within(held).getByRole("status").textContent).toBe("Sending…");

    for (const wait of [1_000, 2_000, 4_000, 8_000, 15_000]) await tick(wait);
    expect(within(held).getByRole("status").textContent).toBe(KEYED_SEND_LATE_TEXT);
    expect(within(held).getByRole("button", { name: "Stop trying" })).toBeTruthy();
    expect(page.createAgentMessage.mock.calls.length).toBeGreaterThan(1);

    page.createAgentMessage.mockResolvedValueOnce(message("Status please"));
    onlineManager.setOnline(true);
    await tick(30_000);
    expect(screen.queryByTestId("agent-send-held")).toBeNull();
    expect(new Set(messageKeys(page.createAgentMessage)).size).toBe(1);
  } finally {
    jest.advanceTimersByTime(1);
    jest.useRealTimers();
    onlineManager.setOnline(online);
    page.view.unmount();
    page.restore();
  }
});

/** A long list of listed sessions, every one Idle and named in order, so the page's order is the
 *  fixture's own and what the window drew can be read off it. */
function manySessions(count: number): Agent[] {
  return Array.from({ length: count }, (_, index) => ({
    ...agents[1],
    last_activity: null,
    last_seen: now,
    session_id: `session-${String(index).padStart(2, "0")}`,
    title: `Session ${String(index).padStart(2, "0")}`,
  }));
}

/**
 * happy-dom has no layout engine: every box is zero and nothing scrolls. The windowed list reads
 * one box, its own top edge against the viewport, so this answers that one and leaves every
 * other at zero, where each row stands at the window's estimate (44 px) under a 768 px viewport.
 */
function scrollingList(): { restore: () => void; scrollTo: (offset: number) => void } {
  let listTop = 0;
  const boxes = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function box(
    this: Element
  ) {
    const top = this.matches("[data-agents-list]") ? listTop : 0;
    return {
      bottom: top,
      height: 0,
      left: 0,
      right: 0,
      toJSON: () => ({}),
      top,
      width: 0,
      x: 0,
      y: top,
    } as DOMRect;
  });
  return {
    restore: () => boxes.mockRestore(),
    scrollTo: (offset: number) => {
      listTop = -offset;
      act(() => {
        fireEvent.scroll(window);
      });
    },
  };
}

/** The rows the window has drawn, in the order the page draws them. */
function drawnRows(region: HTMLElement): HTMLElement[] {
  return [...region.querySelectorAll<HTMLElement>("[data-agent-row]:not([hidden])")];
}

// R7: the list draws the rows the screen holds and twelve either side, so a page of 600 sessions
// is the same work as one of 20. A row the window has not drawn is in no part of the page, which
// is what makes the saving; the scrollbar still measures the whole list.
test("the Agents page draws part of a long list, with its scrollbar measuring the whole", async () => {
  const listed = manySessions(60);
  const layout = scrollingList();
  const page = renderAgents({ listedAgents: listed });

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    await within(region).findByText("Session 00", { exact: true });
    expect(drawnRows(region).length).toBeGreaterThan(10);
    expect(drawnRows(region).length).toBeLessThan(listed.length);
    expect(region.querySelector('[data-agent-row="session-59"]')).toBeNull();
    // Sixty rows and four group headings at the estimate: the box the rows sit in is as tall as
    // the list, so the scrollbar reaches the last session rather than the last drawn row.
    expect(region.querySelector<HTMLElement>("[data-agents-list]")?.style.height).toBe(
      `${64 * 44}px`
    );
  } finally {
    layout.restore();
    page.view.unmount();
    page.restore();
  }
});

// D1: an open row holds the reply target, the unread set and the composer's draft, so the window
// draws it wherever it has scrolled to. A plain window would unmount it with the scroll and the
// draft would be gone when the reader scrolled back.
test("a row the reader opened keeps its draft when the window scrolls past it", async () => {
  const layout = scrollingList();
  const page = renderAgents({ listedAgents: manySessions(60) });

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    await within(region).findByText("Session 00", { exact: true });
    const opened = card(region, "Session 00");
    expand(opened, "Session 00");
    const field = within(opened).getByRole("textbox", { name: "Comment" }) as HTMLTextAreaElement;
    fireEvent.change(field, { target: { value: "Half a thought" } });

    layout.scrollTo(2_000);
    // A row nobody opened is gone once it scrolls out; the opened one is still there.
    expect(region.querySelector('[data-agent-row="session-05"]')).toBeNull();
    expect(region.querySelector('[data-agent-row="session-00"]')).not.toBeNull();

    layout.scrollTo(0);
    expect(card(region, "Session 00")).toBe(opened);
    expect(field.value).toBe("Half a thought");
  } finally {
    layout.restore();
    page.view.unmount();
    page.restore();
  }
});

// The keys walk the page's list of sessions, not the rows in the DOM: at the bottom of the drawn
// range a walk over `[data-agent-row]` would stop dead, with no row left to step to.
test("j from the bottom of the drawn range reaches the next session and focuses its row", async () => {
  const listed = manySessions(60);
  const layout = scrollingList();
  const page = renderAgents({ listedAgents: listed });

  try {
    const region = await screen.findByRole("region", { name: "Agents" });
    await within(region).findByText("Session 00", { exact: true });
    const bottom = drawnRows(region).at(-1);
    const at = listed.findIndex((agent) => agent.session_id === bottom?.dataset.agentRow);
    const next = listed[at + 1]?.session_id;
    expect(next).toBeDefined();
    expect(region.querySelector(`[data-agent-row="${next}"]`)).toBeNull();

    act(() => bottom?.focus());
    expect(document.activeElement).toBe(bottom ?? null);
    act(() => {
      fireEvent.keyDown(bottom ?? document.body, { key: "j" });
    });

    const landed = region.querySelector(`[data-agent-row="${next}"]`);
    expect(landed).not.toBeNull();
    expect(document.activeElement).toBe(landed);
  } finally {
    layout.restore();
    page.view.unmount();
    page.restore();
  }
});
