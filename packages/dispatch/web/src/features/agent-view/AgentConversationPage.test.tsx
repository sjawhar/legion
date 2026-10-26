import { afterEach, expect, spyOn, test } from "bun:test";
import type { AgentStreamCommand, AgentStreamFrame } from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { Link, MemoryRouter, Route, Routes } from "react-router-dom";

import { api } from "../../api/client";
import * as live from "../../api/live";
import type { Agent, Message, MessageRead, UserAgentStates } from "../../api/types";
import { Sidebar } from "../sidebar/Sidebar";
import { AgentConversationPage } from "./AgentConversationPage";

const SESSION = "01a0e090-4848-7473-acc5-fc96e6a646d3";

/** A second live session, for the rows that move the one mounted live view between two. */
const OTHER = "01a0e0c1-0000-7000-8000-00000000b442";

const agent: Agent = {
  capabilities: ["aside", "btw", "steer"],
  dir: "/workspaces/planner",
  host: "planner-host",
  last_activity: null,
  last_seen: Date.now(),
  machine_id: "planner-host",
  open_asks: 0,
  roles: [],
  session_id: SESSION,
  title: "Planner",
};

function message(id: string, body: string, overrides: Partial<Message> = {}): Message {
  return {
    author: { id: "sami", kind: "user" },
    body,
    created_at: "2026-09-27T21:16:15Z",
    deliveries: [],
    id,
    in_reply_to: null,
    issue_key: null,
    target: `session:${SESSION}`,
    ...overrides,
  };
}

const conversation: MessageRead[] = [
  {
    message: message("m1", "Where is the dashboard?"),
    replies: [
      message("r1", "Switching to it now.", {
        author: { id: SESSION, kind: "session" },
        created_at: "2026-09-27T21:17:16Z",
        in_reply_to: "m1",
      }),
      message("r2", "Done: it is at /dash.", {
        author: { id: SESSION, kind: "session" },
        created_at: "2026-09-27T21:20:28Z",
        in_reply_to: "r1",
      }),
    ],
  },
];

const spies: { mockRestore: () => void }[] = [];

afterEach(() => {
  cleanup();
  for (const spy of spies.splice(0)) spy.mockRestore();
});

/**
 * The live view for SESSION, signed in as sami, beside the navigation that carries the unread
 * badge, as the app lays them out. The relayed stream answers with `replay` and then stays open;
 * with none it never opens, so everything shown comes from Dispatch. With `other`, the session
 * list holds a second session, OTHER ("Reviewer"), whose stream answers with its own replay, and
 * `SessionLinks` moves the one mounted live view between the two as the app's links do.
 */
function renderLiveView({
  agentState,
  capabilities = agent.capabilities,
  messages,
  other,
  putAgentState,
  replay,
}: {
  agentState: UserAgentStates;
  capabilities?: string[];
  messages: MessageRead[];
  other?: { replay?: AgentStreamFrame[] };
  putAgentState: ReturnType<typeof spyOn<typeof api, "putAgentState">>;
  replay?: AgentStreamFrame[];
}): void {
  spies.push(
    spyOn(api, "whoAmI").mockResolvedValue({ kind: "user", login: "sami" }),
    spyOn(api, "listAgents").mockResolvedValue([
      { ...agent, capabilities },
      ...(other === undefined ? [] : [{ ...agent, session_id: OTHER, title: "Reviewer" }]),
    ]),
    spyOn(live, "readEventStream").mockImplementation((url, handlers) => {
      const session = url.includes(OTHER) ? OTHER : SESSION;
      const frames = session === OTHER ? other?.replay : replay;
      if (frames !== undefined && url.endsWith("/stream")) {
        handlers.onEvent({
          data: JSON.stringify({ frames, session_id: session, v: 1 }),
          event: "replay",
          id: undefined,
        });
      }
      return new Promise<void>(() => undefined);
    }),
    spyOn(api, "listAgentMessages").mockResolvedValue(messages),
    spyOn(api, "getMyAgentState").mockResolvedValue(agentState),
    spyOn(api, "getInbox").mockResolvedValue([]),
    spyOn(api, "listIssues").mockResolvedValue([]),
    spyOn(api, "listProjects").mockResolvedValue([]),
    putAgentState
  );
  const queryClient = new QueryClient({
    defaultOptions: { queries: { gcTime: 0, retry: false, staleTime: 0 } },
  });
  render(
    <MemoryRouter initialEntries={[`/agents/${SESSION}/live`]}>
      <QueryClientProvider client={queryClient}>
        <Sidebar user={{ kind: "user", login: "sami" }} />
        {other === undefined ? null : <SessionLinks />}
        <Routes>
          <Route element={<AgentConversationPage />} path="/agents/:sessionId/live" />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

/** In-app links to SESSION and OTHER: following one keeps the live view's route mounted and
 *  changes only its session. */
function SessionLinks(): ReactNode {
  return (
    <nav aria-label="Test sessions">
      <Link to={`/agents/${SESSION}/live`}>Switch to Planner</Link>
      <Link to={`/agents/${OTHER}/live`}>Switch to Reviewer</Link>
    </nav>
  );
}

/** The navigation's Agents entry, which wears the unread badge. */
function agentsLink(): HTMLElement {
  return screen.getByRole("link", { name: /^Agents/ });
}

// A human's direct message and the session's replies to it are stored in Dispatch, not in the
// relayed stream: the session answers through a tool call, which the stream shows only as that
// call. The live view is where the human talks to the session, so it has to show those replies,
// and seeing them there is reading them.
test("the live view shows the session's Dispatch replies to direct messages and marks them read", async () => {
  const putAgentState = spyOn(api, "putAgentState").mockImplementation(async (_session, input) => ({
    ...input,
    unread_replies: 0,
  }));
  renderLiveView({
    agentState: { [SESSION]: { unread_replies: 2 } },
    messages: conversation,
    putAgentState,
  });

  const thread = await screen.findByTestId("agent-thread");
  await expect(within(thread).findByText("Where is the dashboard?")).resolves.toBeTruthy();
  const replies = await within(thread).findAllByTestId("agent-dispatch-reply");
  expect(replies.map((reply) => reply.textContent)).toEqual([
    expect.stringContaining("Switching to it now."),
    expect.stringContaining("Done: it is at /dash."),
  ]);
  await waitFor(() =>
    expect(putAgentState).toHaveBeenCalledWith(SESSION, { read_through: "2026-09-27T21:20:28Z" })
  );
  await waitFor(() => expect(agentsLink().textContent).toBe("Agents"));
});

// A read mark that fails to save is sent again, so one failed request does not leave the badge
// up until the session replies once more.
test("the live view retries a read mark that failed to save", async () => {
  const putAgentState = spyOn(api, "putAgentState")
    .mockRejectedValueOnce(new Error("Dispatch is restarting"))
    .mockImplementation(async (_session, input) => ({ ...input, unread_replies: 0 }));
  renderLiveView({
    agentState: { [SESSION]: { unread_replies: 2 } },
    messages: conversation,
    putAgentState,
  });

  await waitFor(() => expect(agentsLink().textContent).toContain("New replies 2"));
  await waitFor(() => expect(agentsLink().textContent).toBe("Agents"), { timeout: 4000 });
});

// A session's stored conversation holds every message sent to it: the viewer's direct messages,
// other humans' direct messages, and issue messages targeted at it. Only the viewer's own read as
// "you"; every other one names who wrote it, and an issue message names its issue.
test("the live view attributes other people's messages to the session to their authors", async () => {
  renderLiveView({
    agentState: {},
    messages: [
      { message: message("m1", "Mine: where is the dashboard?"), replies: [] },
      {
        message: message("m2", "Alice here: status?", {
          author: { id: "alice", kind: "user" },
          created_at: "2026-09-27T21:18:00Z",
        }),
        replies: [],
      },
      {
        message: message("m3", "Bob on the issue: ship it?", {
          author: { id: "bob", kind: "user" },
          created_at: "2026-09-27T21:19:00Z",
          issue_key: "CORE-1",
        }),
        replies: [],
      },
      {
        message: message("m4", "Reviewer: merged.", {
          author: {
            id: "reviewer-session-id",
            kind: "session",
            origin: { session_title: "Reviewer (stamped)" },
          },
          created_at: "2026-09-27T21:19:30Z",
          issue_key: "CORE-2",
        }),
        replies: [],
      },
    ],
    putAgentState: spyOn(api, "putAgentState").mockResolvedValue({ unread_replies: 0 }),
  });

  const thread = await screen.findByTestId("agent-thread");
  await expect(within(thread).findByText("Alice here: status?")).resolves.toBeTruthy();
  const own = within(thread).getAllByTestId("agent-message-user");
  expect(own.map((node) => node.textContent)).toEqual(["Mine: where is the dashboard?"]);
  const others = within(thread).getAllByTestId("agent-message-other");
  expect(others.map((node) => node.textContent)).toEqual([
    "aliceAlice here: status?",
    "bob · CORE-1Bob on the issue: ship it?",
    "Reviewer (stamped) · CORE-2Reviewer: merged.",
  ]);
  // Another agent is drawn by `AgentName`: named by the title it stamped (the registry does not
  // list it), marked not running, and linked to its own live conversation.
  const reviewer = others[2]?.querySelector('[data-agent-name="reviewer-session-id"]');
  if (!(reviewer instanceof HTMLElement)) throw new Error("the reviewer is not an AgentName");
  await waitFor(() => expect(reviewer.getAttribute("data-agent-state")).toBe("not-running"));
  expect(
    within(reviewer).getByRole("link", { name: "Reviewer (stamped)" }).getAttribute("href")
  ).toBe("/agents/reviewer-session-id/live");
});

// The header is the agent's own live conversation (exclusion E3): `AgentName` names it by its
// registry title and marks it running, with no link to the page it is on. The state mark adds no
// text, so the heading reads exactly the agent's name, as the tab title does.
test("the live view's header names the agent with its running state and no link", async () => {
  renderLiveView({
    agentState: {},
    messages: [],
    putAgentState: spyOn(api, "putAgentState").mockResolvedValue({ unread_replies: 0 }),
  });

  const heading = await screen.findByRole("heading", { level: 1 });
  await waitFor(() => expect(heading.textContent).toBe("Planner"));
  const name = heading.querySelector(`[data-agent-name="${SESSION}"]`);
  expect(name?.getAttribute("data-agent-state")).toBe("running");
  expect(within(heading).getByRole("img", { name: "Running" })).toBeTruthy();
  expect(heading.querySelector("a")).toBeNull();
  await waitFor(() => expect(document.title).toContain("Planner"));
});

// Stored times carry microseconds, so two messages written within one millisecond still have an
// order, and the view keeps it whatever order the exchanges arrive in.
test("the live view orders two stored messages in one millisecond by their microseconds", async () => {
  renderLiveView({
    agentState: {},
    messages: [
      {
        message: message("m2", "Written second", { created_at: "2026-09-27T21:16:15.1208Z" }),
        replies: [],
      },
      {
        message: message("m1", "Written first", { created_at: "2026-09-27T21:16:15.1201Z" }),
        replies: [],
      },
    ],
    putAgentState: spyOn(api, "putAgentState").mockResolvedValue({ unread_replies: 0 }),
  });

  const thread = await screen.findByTestId("agent-thread");
  await within(thread).findByText("Written second");
  expect(
    within(thread)
      .getAllByTestId("agent-message-user")
      .map((node) => node.textContent)
  ).toEqual(["Written first", "Written second"]);
});

/** A user message the session streamed: the turn a person's Dispatch message became when
 *  `dispatchMessageId` names it, else one typed at the session's own terminal. */
function userFrame(seq: number, at: number, text: string, dispatchMessageId?: string) {
  return {
    kind: "message",
    message: {
      at,
      id: `u${at}`,
      parts: [{ text, type: "text" }],
      role: "user",
      streaming: false,
      ...(dispatchMessageId === undefined ? {} : { dispatchMessageId }),
    },
    seq,
    v: 1,
  } as const satisfies AgentStreamFrame;
}

// A person's direct message becomes the session's own user turn, so the session streams it as a
// user message tagged with its Dispatch id, and Dispatch stores it too. It shows once, where the
// session took it, and a message another person sent still names its author.
test("a person's message the session took as its own turn shows once, still naming its author", async () => {
  const at = (iso: string) => Date.parse(iso);
  renderLiveView({
    agentState: {},
    messages: [
      { message: message("m1", "Mine: where is the dashboard?"), replies: [] },
      {
        message: message("m2", "Alice here: status?", {
          author: { id: "alice", kind: "user" },
          created_at: "2026-09-27T21:18:00Z",
        }),
        replies: [],
      },
      {
        message: message("m3", "Not taken yet", { created_at: "2026-09-27T21:19:00Z" }),
        replies: [],
      },
    ],
    putAgentState: spyOn(api, "putAgentState").mockResolvedValue({ unread_replies: 0 }),
    replay: [
      userFrame(1, at("2026-09-27T21:16:16Z"), "Mine: where is the dashboard?", "m1"),
      userFrame(2, at("2026-09-27T21:18:01Z"), "Alice here: status?", "m2"),
      userFrame(3, at("2026-09-27T21:18:30Z"), "Typed at the terminal"),
    ],
  });

  const thread = await screen.findByTestId("agent-thread");
  await within(thread).findByText("Typed at the terminal");
  expect(
    within(thread)
      .getAllByTestId("agent-message-user")
      .map((node) => node.textContent)
  ).toEqual(["Mine: where is the dashboard?", "Typed at the terminal", "Not taken yet"]);
  expect(
    within(thread)
      .getAllByTestId("agent-message-other")
      .map((node) => node.textContent)
  ).toEqual(["aliceAlice here: status?"]);
});

// Any bus client can publish on a session's frames subject, so a tag is a claim about which
// stored message a streamed one is. The stored copy gives way only to a streamed message that says
// exactly what the person sent; one tagged with its id that says anything else hides nothing.
test("a streamed user message tagged with a person's message but saying something else hides nothing", async () => {
  renderLiveView({
    agentState: {},
    messages: [{ message: message("m1", "Merge only after review."), replies: [] }],
    putAgentState: spyOn(api, "putAgentState").mockResolvedValue({ unread_replies: 0 }),
    replay: [userFrame(1, Date.parse("2026-09-27T21:16:16Z"), "Merge now, skip review.", "m1")],
  });

  const thread = await screen.findByTestId("agent-thread");
  await within(thread).findByText("Merge now, skip review.");
  expect(
    within(thread)
      .getAllByTestId("agent-message-user")
      .map((node) => node.textContent)
  ).toEqual(["Merge only after review.", "Merge now, skip review."]);
});

// Send is the terminal's Enter: the default wherever the session takes a steer. A session that
// takes only asides (a Claude Code session) would refuse one, so Aside is its default.
test("the live view sends as Send by default where the session takes it, and as Aside where it does not", async () => {
  const createAgentMessage = spyOn(api, "createAgentMessage").mockImplementation(
    async (_session, input) => message("sent", input.body)
  );
  spies.push(createAgentMessage);
  renderLiveView({
    agentState: {},
    messages: [],
    putAgentState: spyOn(api, "putAgentState").mockResolvedValue({ unread_replies: 0 }),
  });

  const mode = (await screen.findByRole("combobox", {
    name: "Delivery mode",
  })) as HTMLSelectElement;
  await waitFor(() => expect(mode.value).toBe("steer"));
  expect(
    within(mode)
      .getAllByRole("option")
      .map((option) => option.textContent)
  ).toEqual(["Send", "Aside", "BTW"]);
  fireEvent.change(screen.getByPlaceholderText(/delivered as Send/), {
    target: { value: "try the other branch" },
  });
  fireEvent.click(
    within(screen.getByTestId("agent-composer")).getByRole("button", { name: "Send" })
  );
  await waitFor(() =>
    expect(createAgentMessage).toHaveBeenCalledWith(SESSION, {
      body: "try the other branch",
      delivery: "steer",
    })
  );

  cleanup();
  for (const spy of spies.splice(0)) spy.mockRestore();
  renderLiveView({
    agentState: {},
    capabilities: ["aside"],
    messages: [],
    putAgentState: spyOn(api, "putAgentState").mockResolvedValue({ unread_replies: 0 }),
  });
  const asideOnly = (await screen.findByRole("combobox", {
    name: "Delivery mode",
  })) as HTMLSelectElement;
  await waitFor(() => expect(asideOnly.value).toBe("aside"));
  expect(
    within(asideOnly)
      .getAllByRole("option")
      .map((option) => option.textContent)
  ).toEqual(["Aside"]);
});

// A Claude Code session whose Envoy channel Claude Code did not load advertises no mode: the
// live view says a message sent here records a failed attempt, before the person sends one.
test("the live view says a session advertising no delivery mode takes no messages", async () => {
  renderLiveView({
    agentState: {},
    capabilities: [],
    messages: [],
    putAgentState: spyOn(api, "putAgentState").mockResolvedValue({ unread_replies: 0 }),
  });

  expect((await screen.findByTestId("agent-takes-no-messages")).textContent).toContain(
    "advertises no delivery mode"
  );

  cleanup();
  for (const spy of spies.splice(0)) spy.mockRestore();
  renderLiveView({
    agentState: {},
    capabilities: ["aside"],
    messages: [],
    putAgentState: spyOn(api, "putAgentState").mockResolvedValue({ unread_replies: 0 }),
  });
  await screen.findByRole("combobox", { name: "Delivery mode" });
  expect(screen.queryByTestId("agent-takes-no-messages")).toBeNull();
});

/** An assistant message the session streamed, optionally naming the model that produced it
 *  (LEGION-548); `streaming` leaves the turn mid-way, the session still producing it. */
function assistantFrame(seq: number, at: number, text: string, model?: string, streaming = false) {
  return {
    kind: "message",
    message: {
      at,
      id: `a${at}`,
      parts: [{ text, type: "text" }],
      role: "assistant",
      streaming,
      ...(model === undefined ? {} : { model }),
    },
    seq,
    v: 1,
  } as const satisfies AgentStreamFrame;
}

// The live view names the model next to the session's title, from its most recent turn, and
// marks the turn itself in the transcript (LEGION-548).
test("the live view shows the session's model next to its title and on the turn that produced it", async () => {
  renderLiveView({
    agentState: {},
    messages: [],
    putAgentState: spyOn(api, "putAgentState").mockResolvedValue({ unread_replies: 0 }),
    replay: [
      assistantFrame(
        1,
        Date.parse("2026-09-27T21:16:16Z"),
        "Switching to it now.",
        "anthropic/claude-opus-5"
      ),
    ],
  });

  const header = await screen.findByTestId("agent-session-model");
  expect(header.textContent).toBe("anthropic/claude-opus-5");
  const thread = await screen.findByTestId("agent-thread");
  const turnModel = await within(thread).findByTestId("agent-message-model");
  expect(turnModel.textContent).toBe("anthropic/claude-opus-5");
});

// A session whose client never reports a model (Claude Code, OpenCode, or a build too old) shows
// nothing, in the header or on the turn, rather than a guess.
test("the live view shows nothing for a session whose client reports no model", async () => {
  renderLiveView({
    agentState: {},
    messages: [],
    putAgentState: spyOn(api, "putAgentState").mockResolvedValue({ unread_replies: 0 }),
    replay: [assistantFrame(1, Date.parse("2026-09-27T21:16:16Z"), "Switching to it now.")],
  });

  const thread = await screen.findByTestId("agent-thread");
  await within(thread).findByText("Switching to it now.");
  expect(screen.queryByTestId("agent-session-model")).toBeNull();
  expect(within(thread).queryByTestId("agent-message-model")).toBeNull();
});

/** The slash commands a session advertises (LEGION-394), in the order it lists them: one only its
 *  own terminal runs, listed first, and a prompt whose name holds another's as a substring. */
const COMMANDS: AgentStreamCommand[] = [
  { description: "Start a new session", name: "new", source: "builtin", terminalOnly: true },
  { description: "Compact the session's context", name: "compact", source: "builtin" },
  { description: "Show background jobs", name: "jobs", source: "builtin" },
  { name: "recompile", source: "prompt" },
];

function commandsFrame(seq: number, commands: AgentStreamCommand[]) {
  return { commands, kind: "commands", seq, v: 1 } as const satisfies AgentStreamFrame;
}

/** The live view of a session that sent `commands`, or none, with its composer's input. */
async function renderWithCommands(commands?: AgentStreamCommand[]): Promise<HTMLTextAreaElement> {
  renderLiveView({
    agentState: {},
    messages: [],
    putAgentState: spyOn(api, "putAgentState").mockResolvedValue({ unread_replies: 0 }),
    replay: commands === undefined ? [] : [commandsFrame(1, commands)],
  });
  const mode = (await screen.findByRole("combobox", {
    name: "Delivery mode",
  })) as HTMLSelectElement;
  await waitFor(() => expect(mode.value).toBe("steer"));
  return within(screen.getByTestId("agent-composer")).getByRole("textbox") as HTMLTextAreaElement;
}

function type(input: HTMLTextAreaElement, value: string): void {
  fireEvent.change(input, { target: { value } });
}

function commandOptions(): string[] {
  return within(screen.getByRole("listbox", { name: "Slash commands" }))
    .getAllByRole("option")
    .map((option) => option.textContent ?? "");
}

// A message that starts with `/` is a command line at the session's terminal, so the composer
// completes the session's own commands there: every runnable one first, a command only its
// terminal runs after them (listed, so the person learns it will not run here), and as the
// person types, names that start with what they typed before names that merely hold it.
test("the composer completes the session's slash commands, filtering as the person types", async () => {
  const input = await renderWithCommands(COMMANDS);
  await waitFor(() => expect(input.placeholder).toContain("/"));

  type(input, "/");
  await waitFor(() =>
    expect(commandOptions()).toEqual([
      "/compactCompact the session's context",
      "/jobsShow background jobs",
      "/recompile",
      "/newterminal onlyStart a new session",
    ])
  );

  type(input, "/comp");
  await waitFor(() =>
    expect(commandOptions()).toEqual(["/compactCompact the session's context", "/recompile"])
  );
});

// Picking a command writes what the person would have typed at the terminal, never a directive
// the session would receive as markup, and Send carries it to the session unchanged.
test("picking a command puts /name in the message as plain text, and Enter sends it unchanged", async () => {
  const createAgentMessage = spyOn(api, "createAgentMessage").mockImplementation(
    async (_session, input) => message("sent", input.body)
  );
  spies.push(createAgentMessage);
  const input = await renderWithCommands(COMMANDS);

  type(input, "/co");
  await waitFor(() => expect(commandOptions()).toHaveLength(2));
  fireEvent.keyDown(input, { key: "Enter" });
  await waitFor(() => expect(input.value).toBe("/compact "));
  expect(screen.queryByRole("listbox", { name: "Slash commands" })).toBeNull();

  // With the list closed Enter is the composer's own Send again.
  fireEvent.keyDown(input, { key: "Enter" });
  await waitFor(() =>
    expect(createAgentMessage).toHaveBeenCalledWith(SESSION, {
      body: "/compact",
      delivery: "steer",
    })
  );
});

// The list is the keyboard's while it is open, and Escape hands Enter back to sending.
test("arrow keys move through the commands, and after Escape Enter sends what was typed", async () => {
  const createAgentMessage = spyOn(api, "createAgentMessage").mockImplementation(
    async (_session, input) => message("sent", input.body)
  );
  spies.push(createAgentMessage);
  const input = await renderWithCommands(COMMANDS);

  type(input, "/");
  await waitFor(() => expect(commandOptions()).toHaveLength(4));
  fireEvent.keyDown(input, { key: "ArrowDown" });
  await waitFor(() =>
    expect(
      within(screen.getByRole("listbox", { name: "Slash commands" })).getByRole("option", {
        selected: true,
      }).textContent
    ).toBe("/jobsShow background jobs")
  );
  fireEvent.keyDown(input, { key: "Tab" });
  await waitFor(() => expect(input.value).toBe("/jobs "));

  type(input, "/rec");
  await waitFor(() => expect(commandOptions()).toEqual(["/recompile"]));
  fireEvent.keyDown(input, { key: "Escape" });
  await waitFor(() => expect(screen.queryByRole("listbox", { name: "Slash commands" })).toBeNull());
  fireEvent.keyDown(input, { key: "Enter" });
  await waitFor(() =>
    expect(createAgentMessage).toHaveBeenCalledWith(SESSION, { body: "/rec", delivery: "steer" })
  );
});

// A session with every skill installed lists hundreds of commands; the list shows the best
// fifty rather than a scroller the length of the page.
test("a session with hundreds of commands shows fifty of them", async () => {
  const many = Array.from({ length: 300 }, (_, index) => ({
    name: `skill:s${String(index).padStart(3, "0")}`,
    source: "skill" as const,
  }));
  const input = await renderWithCommands(many);

  type(input, "/");
  await waitFor(() => expect(commandOptions()).toHaveLength(50));
  expect(commandOptions()[0]).toBe("/skill:s000");
});

// Completion is offered only where the session would run the text as a command: a session that
// sent no command list cannot, a `/` inside a message is just text at a terminal too, and a BTW
// is a side question the session answers rather than input it runs. None of them mentions `/`.
test("no completion is offered without a command list, for a / mid-message, or in BTW", async () => {
  const silent = await renderWithCommands();
  expect(silent.placeholder).not.toContain("/");
  type(silent, "/");
  await Bun.sleep(50);
  expect(screen.queryByRole("listbox", { name: "Slash commands" })).toBeNull();

  cleanup();
  for (const spy of spies.splice(0)) spy.mockRestore();
  const input = await renderWithCommands(COMMANDS);
  type(input, "please /co");
  await Bun.sleep(50);
  expect(screen.queryByRole("listbox", { name: "Slash commands" })).toBeNull();

  fireEvent.change(screen.getByRole("combobox", { name: "Delivery mode" }), {
    target: { value: "btw" },
  });
  await waitFor(() => expect(input.placeholder).toContain("delivered as BTW"));
  expect(input.placeholder).not.toContain("/");
  type(input, "/co");
  await Bun.sleep(50);
  expect(screen.queryByRole("listbox", { name: "Slash commands" })).toBeNull();
});

/** A turn the session is still producing: the thread is mid-turn while it lasts. */
const MID_TURN = [
  assistantFrame(1, Date.parse("2026-09-27T21:16:16Z"), "Working", undefined, true),
];

function composer(): HTMLElement {
  return screen.getByTestId("agent-composer");
}

function composerInput(): HTMLTextAreaElement {
  return within(composer()).getByRole("textbox") as HTMLTextAreaElement;
}

/** The composer's Send, by what it reads: "Send", or "Sending…" while a send is out. */
function sendButton(name = "Send"): HTMLButtonElement {
  return within(composer()).getByRole("button", { name }) as HTMLButtonElement;
}

/** Waits for the session list to settle the delivery mode on `expected`, then returns the
 *  composer's input. */
async function modeSettled(expected: string): Promise<HTMLTextAreaElement> {
  const mode = (await screen.findByRole("combobox", {
    name: "Delivery mode",
  })) as HTMLSelectElement;
  await waitFor(() => expect(mode.value).toBe(expected));
  return composerInput();
}

/** Waits for the session's mid-turn text to show in the thread. */
async function midTurnShown(): Promise<void> {
  const thread = await screen.findByTestId("agent-thread");
  await within(thread).findByText("Working");
}

/** Dispatch accepting every send. */
function acceptSends() {
  const createAgentMessage = spyOn(api, "createAgentMessage").mockImplementation(
    async (_session, input) => message("sent", input.body)
  );
  spies.push(createAgentMessage);
  return createAgentMessage;
}

/** How a row answers a send Dispatch is holding. */
interface HeldSend {
  resolve: (sent: Message) => void;
  reject: (reason: Error) => void;
}

/** Dispatch holding every send until the row answers it: `held[n]` answers the nth. */
function holdSends() {
  const held: HeldSend[] = [];
  const createAgentMessage = spyOn(api, "createAgentMessage").mockImplementation(() => {
    const answer = Promise.withResolvers<Message>();
    held.push(answer);
    return answer.promise;
  });
  spies.push(createAgentMessage);
  return { createAgentMessage, held };
}

/** Follows the in-app link to the session titled `title` and returns its composer's input. */
async function switchTo(title: "Planner" | "Reviewer"): Promise<HTMLTextAreaElement> {
  fireEvent.click(screen.getByRole("link", { name: `Switch to ${title}` }));
  await screen.findByRole("heading", { level: 1, name: new RegExp(title) });
  return composerInput();
}

// A person talks to a session while it works, as at its terminal: a turn still streaming does
// not hold Send, and a click sends at once, as Send, the session's default.
test("Send stays pressable while the session is mid-turn, and a click sends at once as Send", async () => {
  const createAgentMessage = acceptSends();
  renderLiveView({
    agentState: {},
    messages: [],
    putAgentState: spyOn(api, "putAgentState").mockResolvedValue({ unread_replies: 0 }),
    replay: MID_TURN,
  });
  const input = await modeSettled("steer");
  await midTurnShown();

  type(input, "try the other branch");
  const send = sendButton();
  expect(send.hasAttribute("disabled")).toBe(false);
  expect(send.hasAttribute("aria-disabled")).toBe(false);
  fireEvent.click(send);
  await waitFor(() =>
    expect(createAgentMessage).toHaveBeenCalledWith(SESSION, {
      body: "try the other branch",
      delivery: "steer",
    })
  );
});

// Enter is the keyboard's Send, and a turn still streaming does not swallow it.
test("Enter sends while the session is mid-turn", async () => {
  const createAgentMessage = acceptSends();
  renderLiveView({
    agentState: {},
    messages: [],
    putAgentState: spyOn(api, "putAgentState").mockResolvedValue({ unread_replies: 0 }),
    replay: MID_TURN,
  });
  const input = await modeSettled("steer");
  await midTurnShown();

  type(input, "stop after this file");
  fireEvent.keyDown(input, { key: "Enter" });
  await waitFor(() =>
    expect(createAgentMessage).toHaveBeenCalledWith(SESSION, {
      body: "stop after this file",
      delivery: "steer",
    })
  );
});

// An empty box is the one thing that refuses Send, and Send says so: on its title, as its
// accessible description, and on a line in the composer a reader without a pointer can see. It
// stays focusable (aria-disabled, not disabled), and neither a click nor Enter sends. With text,
// the line goes back to saying how to send.
test("with an empty box Send is refused, says why on screen, and sends nothing", async () => {
  const createAgentMessage = acceptSends();
  renderLiveView({
    agentState: {},
    messages: [],
    putAgentState: spyOn(api, "putAgentState").mockResolvedValue({ unread_replies: 0 }),
  });
  const input = await modeSettled("steer");

  const send = sendButton();
  expect(send.getAttribute("aria-disabled")).toBe("true");
  expect(send.hasAttribute("disabled")).toBe(false);
  expect(send.title).toBe("Type a message first.");
  const description = (send.getAttribute("aria-describedby") ?? "")
    .split(" ")
    .map((id) => document.getElementById(id)?.textContent ?? "")
    .join(" ");
  expect(description).toBe("Type a message first.");
  const shown = within(composer())
    .getAllByText("Type a message first.")
    .filter((line) => !line.classList.contains("sr-only"));
  expect(shown).toHaveLength(1);
  fireEvent.click(send);
  fireEvent.keyDown(input, { key: "Enter" });
  await Bun.sleep(50);
  expect(createAgentMessage).not.toHaveBeenCalled();

  type(input, "now with words");
  await waitFor(() => expect(sendButton().hasAttribute("aria-disabled")).toBe(false));
  expect(sendButton().hasAttribute("aria-describedby")).toBe(false);
  expect(within(composer()).getByText("Enter to send · Shift+Enter for a new line")).toBeTruthy();
  expect(within(composer()).queryByText("Type a message first.")).toBeNull();
});

// A send Dispatch refuses is not lost: the message comes back into the box to send again, and
// the reason shows.
test("a refused send puts the message back in the box and says why", async () => {
  spies.push(spyOn(api, "createAgentMessage").mockRejectedValue(new Error("the server is down")));
  renderLiveView({
    agentState: {},
    messages: [],
    putAgentState: spyOn(api, "putAgentState").mockResolvedValue({ unread_replies: 0 }),
  });
  const input = await modeSettled("steer");

  type(input, "try the other branch");
  fireEvent.click(sendButton());
  await waitFor(() => expect(composerInput().value).toBe("try the other branch"));
  expect((await screen.findByTestId("agent-send-error")).textContent).toBe(
    "Could not send: the server is down"
  );
});

// A refusal belongs to the session that sent the message. One landing while the reader is on
// another session leaves that session's box and screen alone, and is waiting, text and reason,
// when they go back. A send out on one session does not hold Send on another.
test("a refusal that lands after switching sessions goes back to the session that sent it, not the one on screen", async () => {
  const { createAgentMessage, held } = holdSends();
  renderLiveView({
    agentState: {},
    messages: [],
    other: {},
    putAgentState: spyOn(api, "putAgentState").mockResolvedValue({ unread_replies: 0 }),
  });
  const input = await modeSettled("steer");

  type(input, "hold this");
  fireEvent.click(sendButton());
  await waitFor(() =>
    expect(createAgentMessage).toHaveBeenCalledWith(SESSION, {
      body: "hold this",
      delivery: "steer",
    })
  );

  const other = await switchTo("Reviewer");
  type(other, "Reviewer's own words");
  expect(sendButton().hasAttribute("disabled")).toBe(false);
  await act(async () => {
    held[0]?.reject(new Error("the server is down"));
    await Bun.sleep(50);
  });
  expect(composerInput().value).toBe("Reviewer's own words");
  expect(screen.queryByTestId("agent-send-error")).toBeNull();

  const back = await switchTo("Planner");
  await waitFor(() => expect(back.value).toBe("hold this"));
  expect((await screen.findByTestId("agent-send-error")).textContent).toBe(
    "Could not send: the server is down"
  );
});

// One send is out per session at a time: while it is, Send reads "Sending…" and is disabled,
// Enter sends nothing, and the box takes the next message. A refused send comes back ahead of
// what was typed since, joined by a blank line; sending that and having it refused again brings
// it back once, not doubled; and a send that lands empties the box for good.
test("while a send is out Send is busy, and two refused sends each come back once, in order", async () => {
  const { createAgentMessage, held } = holdSends();
  renderLiveView({
    agentState: {},
    messages: [],
    putAgentState: spyOn(api, "putAgentState").mockResolvedValue({ unread_replies: 0 }),
  });
  const input = await modeSettled("steer");

  type(input, "first");
  fireEvent.click(sendButton());
  await waitFor(() =>
    expect(createAgentMessage).toHaveBeenCalledWith(SESSION, { body: "first", delivery: "steer" })
  );
  await waitFor(() => expect(sendButton("Sending…").hasAttribute("disabled")).toBe(true));
  expect(
    within(composer()).getByText("Sending… Enter sends again once Dispatch answers.")
  ).toBeTruthy();
  expect(within(composer()).queryByText("Enter to send · Shift+Enter for a new line")).toBeNull();
  type(composerInput(), "second");
  fireEvent.keyDown(composerInput(), { key: "Enter" });
  await Bun.sleep(50);
  expect(createAgentMessage).toHaveBeenCalledTimes(1);

  await act(async () => {
    held[0]?.reject(new Error("the server is down"));
  });
  await waitFor(() => expect(composerInput().value).toBe("first\n\nsecond"));
  expect(within(composer()).getByText("Enter to send · Shift+Enter for a new line")).toBeTruthy();

  fireEvent.click(sendButton());
  await waitFor(() => expect(createAgentMessage).toHaveBeenCalledTimes(2));
  expect(createAgentMessage).toHaveBeenLastCalledWith(SESSION, {
    body: "first\n\nsecond",
    delivery: "steer",
  });
  await act(async () => {
    held[1]?.reject(new Error("the server is down"));
  });
  await waitFor(() => expect(composerInput().value).toBe("first\n\nsecond"));
  await Bun.sleep(50);
  expect(composerInput().value).toBe("first\n\nsecond");

  fireEvent.click(sendButton());
  await waitFor(() => expect(createAgentMessage).toHaveBeenCalledTimes(3));
  await act(async () => {
    held[2]?.resolve(message("sent", "first\n\nsecond"));
  });
  await waitFor(() => expect(composerInput().value).toBe(""));
  await Bun.sleep(50);
  expect(composerInput().value).toBe("");
});

// What a person is writing to one session stays with it: switching to another shows that
// session's own box, and switching back finds the draft where it was left.
test("a draft stays with its session across a switch to another", async () => {
  renderLiveView({
    agentState: {},
    messages: [],
    other: {},
    putAgentState: spyOn(api, "putAgentState").mockResolvedValue({ unread_replies: 0 }),
  });
  const input = await modeSettled("steer");

  type(input, "draft for the planner");
  const other = await switchTo("Reviewer");
  expect(other.value).toBe("");
  const back = await switchTo("Planner");
  expect(back.value).toBe("draft for the planner");
});

// A session advertising no delivery mode keeps today's behaviour mid-turn too: Send is
// pressable, the message goes as an aside, and the warning that it records a failed attempt
// stays up.
test("a session advertising no delivery mode still takes a mid-turn send as an aside", async () => {
  const createAgentMessage = acceptSends();
  renderLiveView({
    agentState: {},
    capabilities: [],
    messages: [],
    putAgentState: spyOn(api, "putAgentState").mockResolvedValue({ unread_replies: 0 }),
    replay: MID_TURN,
  });
  await screen.findByTestId("agent-takes-no-messages");
  await midTurnShown();

  type(composerInput(), "are you there?");
  const send = sendButton();
  expect(send.hasAttribute("disabled")).toBe(false);
  expect(send.hasAttribute("aria-disabled")).toBe(false);
  fireEvent.click(send);
  await waitFor(() =>
    expect(createAgentMessage).toHaveBeenCalledWith(SESSION, {
      body: "are you there?",
      delivery: "aside",
    })
  );
  expect(screen.getByTestId("agent-takes-no-messages")).toBeTruthy();
});
