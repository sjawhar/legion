import { afterEach, expect, spyOn, test } from "bun:test";
import type { AgentStreamCommand, AgentStreamFrame } from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { api } from "../../api/client";
import * as live from "../../api/live";
import type { Agent, Message, MessageRead, UserAgentStates } from "../../api/types";
import { Sidebar } from "../sidebar/Sidebar";
import { AgentConversationPage } from "./AgentConversationPage";

const SESSION = "01a0e090-4848-7473-acc5-fc96e6a646d3";

const agent: Agent = {
  capabilities: ["aside", "btw", "steer"],
  dir: "/workspaces/planner",
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
 * with none it never opens, so everything shown comes from Dispatch.
 */
function renderLiveView({
  agentState,
  capabilities = agent.capabilities,
  messages,
  putAgentState,
  replay,
}: {
  agentState: UserAgentStates;
  capabilities?: string[];
  messages: MessageRead[];
  putAgentState: ReturnType<typeof spyOn<typeof api, "putAgentState">>;
  replay?: AgentStreamFrame[];
}): void {
  spies.push(
    spyOn(api, "whoAmI").mockResolvedValue({ kind: "user", login: "sami" }),
    spyOn(api, "listAgents").mockResolvedValue([{ ...agent, capabilities }]),
    spyOn(live, "readEventStream").mockImplementation((url, handlers) => {
      if (replay !== undefined && url.endsWith("/stream")) {
        handlers.onEvent({
          data: JSON.stringify({ frames: replay, session_id: SESSION, v: 1 }),
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
        <Routes>
          <Route element={<AgentConversationPage />} path="/agents/:sessionId/live" />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>
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
  ]);
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

/** An assistant message the session streamed, optionally naming the model that produced it
 *  (LEGION-548). */
function assistantFrame(seq: number, at: number, text: string, model?: string) {
  return {
    kind: "message",
    message: {
      at,
      id: `a${at}`,
      parts: [{ text, type: "text" }],
      role: "assistant",
      streaming: false,
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
