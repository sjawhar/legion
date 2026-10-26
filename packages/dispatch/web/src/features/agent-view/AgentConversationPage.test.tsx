import { afterEach, beforeEach, expect, jest, type Mock, spyOn, test } from "bun:test";
import {
  type AgentStreamCommand,
  type AgentStreamFrame,
  type AgentStreamSettingsReported,
  LIVE_SOCKET_MAX_AGE_CLOSE,
} from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  act,
  cleanup,
  fireEvent,
  type RenderResult,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import type { ReactNode } from "react";
import { Link, MemoryRouter, Route, Routes } from "react-router-dom";

import {
  errorFrame,
  type FakeLiveSocket,
  type FakeLiveSockets,
  installFakeLiveSockets,
} from "../../__tests__/live-socket-harness";
import { answerAgentMessages, answerAgentStates } from "../../__tests__/send-answers";
import { ApiError, api, NetworkError } from "../../api/client";
import { liveSocketFor } from "../../api/live-socket";
import type { Agent, Message, MessageRead, UserAgentStates } from "../../api/types";
import { KEYED_SEND_LATE_TEXT } from "../conversation/send-request";
import { Sidebar } from "../sidebar/Sidebar";
import { KEYED_SEND_ATTEMPT_MS, SEND_DEADLINE_MS, SendStoppedError } from "../writes/keyed-write";
import { AgentConversationPage } from "./AgentConversationPage";

/** What each attempt of the live view's send is held to, beside the message it carries: its own
 *  deadline, nothing that would cut it short once it is out, and the key that names the press. */
const keyedAttempt = expect.objectContaining({
  deadlineMs: KEYED_SEND_ATTEMPT_MS,
  key: expect.any(String),
});

/** The key each send carried (`Idempotency-Key`), in order. */
function sentKeys(createAgentMessage: Mock<typeof api.createAgentMessage>): string[] {
  return createAgentMessage.mock.calls.map(([, , bounds]) => bounds.key);
}

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
const clients: QueryClient[] = [];
let sockets: FakeLiveSockets;

beforeEach(() => {
  sockets = installFakeLiveSockets();
});

afterEach(() => {
  cleanup();
  for (const spy of spies.splice(0)) spy.mockRestore();
  for (const client of clients.splice(0)) liveSocketFor(client).close();
  sockets.restore();
});

/** A query client whose live socket the teardown closes. */
function newClient(): QueryClient {
  const client = new QueryClient({
    defaultOptions: { queries: { gcTime: 0, retry: false, staleTime: 0 } },
  });
  clients.push(client);
  return client;
}

/** Dispatch's reads, signed in as sami, with `agents` the live sessions. */
function mockDispatch({
  agentState,
  agents,
  messages,
  putAgentState,
}: {
  agentState: UserAgentStates;
  agents: Agent[];
  messages: () => MessageRead[];
  putAgentState: Mock<typeof api.putAgentState>;
}): void {
  spies.push(
    spyOn(api, "whoAmI").mockResolvedValue({ event_head: "0", kind: "user", login: "sami" }),
    spyOn(api, "listAgents").mockResolvedValue(agents),
    spyOn(api, "listAgentMessages").mockImplementation(async () => messages()),
    spyOn(api, "getMyAgentState").mockResolvedValue(agentState),
    spyOn(api, "getInbox").mockResolvedValue([]),
    spyOn(api, "listIssues").mockResolvedValue([]),
    spyOn(api, "listProjects").mockResolvedValue([]),
    putAgentState
  );
}

/** Session `session`'s live view on `queryClient`, beside the navigation when `sidebar`. */
function renderPage(
  queryClient: QueryClient,
  session: string,
  sidebar = false,
  links = false
): RenderResult {
  return render(
    <MemoryRouter initialEntries={[`/agents/${session}/live`]}>
      <QueryClientProvider client={queryClient}>
        {sidebar ? <Sidebar user={{ event_head: "0", kind: "user", login: "sami" }} /> : null}
        {links ? <SessionLinks /> : null}
        <Routes>
          <Route element={<AgentConversationPage />} path="/agents/:sessionId/live" />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

/** The id of session `session`'s live view subscription, as the page last sent it on `socket`. */
function agentSubscription(socket: FakeLiveSocket, session = SESSION): string {
  const sent = socket
    .subscribes()
    .filter((frame) => frame.view.kind === "agent" && frame.view.session_id === session)
    .at(-1);
  if (sent === undefined) throw new Error(`no live view of ${session} was subscribed`);
  return sent.sub;
}

/** Dispatch answering a live view's subscription on `socket`, as it does each time the page sends
 *  it: whether the session answers the relay, then the session's replay when it holds any. */
function answer(
  socket: FakeLiveSocket,
  frames: readonly AgentStreamFrame[],
  session = SESSION,
  responding = true
): void {
  const sub = agentSubscription(socket, session);
  act(() => {
    socket.receive({ type: "agent", sub, event: "responder", data: { responding, v: 1 } });
    if (frames.length > 0) {
      socket.receive({
        type: "agent",
        sub,
        event: "replay",
        data: { frames, session_id: session, v: 1 },
      });
    }
  });
}

/**
 * The live view for SESSION, signed in as sami, beside the navigation that carries the unread
 * badge, as the app lays them out. The relayed stream says whether the session answers when
 * `responding` is given, answers with `replay`, and then stays open; with no replay the session
 * sends none, so everything shown comes from Dispatch. With `other`, the session list holds a
 * second session, OTHER ("Reviewer"), whose stream answers with its own replay, and
 * `SessionLinks` moves the one mounted live view between the two as the app's links do.
 */
function renderLiveView({
  agentState,
  capabilities = agent.capabilities,
  messages,
  other,
  putAgentState,
  replay,
  responding,
}: {
  agentState: UserAgentStates;
  capabilities?: string[];
  messages: MessageRead[];
  other?: { replay?: AgentStreamFrame[] };
  putAgentState: Mock<typeof api.putAgentState>;
  replay?: AgentStreamFrame[];
  responding?: boolean;
}): QueryClient & LiveView {
  let thread = messages;
  mockDispatch({
    agentState,
    agents: [
      { ...agent, capabilities },
      ...(other === undefined ? [] : [{ ...agent, session_id: OTHER, title: "Reviewer" }]),
    ],
    messages: () => thread,
    putAgentState,
  });
  const queryClient = newClient();
  renderPage(queryClient, SESSION, true, other !== undefined);
  let socket: FakeLiveSocket | undefined;
  if (replay !== undefined || responding !== undefined) {
    socket = sockets.latest();
    act(() => socket?.open());
    answer(socket, replay ?? [], SESSION, responding ?? true);
  }
  return Object.assign(queryClient, {
    frame: (frame: AgentStreamFrame) => {
      const open = socket ?? sockets.latest();
      const subscribed = open
        .subscribes()
        .filter((entry) => entry.view.kind === "agent")
        .at(-1);
      if (subscribed === undefined) throw new Error("no live agent subscription");
      act(() => {
        open.receive({ type: "agent", sub: subscribed.sub, event: "frame", data: frame });
      });
    },
    storeThread: async (next: MessageRead[]) => {
      thread = next;
      await act(async () => {
        await queryClient.invalidateQueries({ queryKey: ["agents", SESSION, "messages"] });
      });
    },
  });
}

/** What a test does to a rendered live view after it opened. */
interface LiveView {
  frame: (frame: AgentStreamFrame) => void;
  storeThread: (messages: MessageRead[]) => Promise<void>;
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

/** Every way the live view's header names the relay. */
const RELAY_LABELS = [
  "Connecting to the session",
  "Live",
  "Reconnecting",
  "This session's conversation is unavailable",
];

/** The relay labels `view`'s header shows; exactly one in any coherent state. */
function relayStatus(view: HTMLElement = screen.getByTestId("agent-conversation")): string[] {
  return RELAY_LABELS.filter((label) => within(view).queryByText(label) !== null);
}

// A human's direct message and the session's replies to it are stored in Dispatch, not in the
// relayed stream: the session answers through a tool call, which the stream shows only as that
// call. The live view is where the human talks to the session, so it has to show those replies,
// and seeing them there is reading them.
test("the live view shows the session's Dispatch replies to direct messages and marks them read", async () => {
  const putAgentState = answerAgentStates().mockImplementation(async (_session, input) => ({
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
    expect(putAgentState).toHaveBeenCalledWith(
      SESSION,
      { read_through: "2026-09-27T21:20:28Z" },
      keyedAttempt
    )
  );
  await waitFor(() => expect(agentsLink().textContent).toBe("Agents"));
});

// A read mark that fails short of Dispatch's verdict is tried again under its key, so one dropped
// request does not leave the badge up until the session replies once more.
test("the live view retries a read mark that failed to save", async () => {
  const putAgentState = answerAgentStates()
    .mockRejectedValueOnce(new ApiError(503, { code: "UNAVAILABLE", error: "restarting" }))
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
    putAgentState: answerAgentStates().mockResolvedValue({ unread_replies: 0 }),
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
    putAgentState: answerAgentStates().mockResolvedValue({ unread_replies: 0 }),
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
    putAgentState: answerAgentStates().mockResolvedValue({ unread_replies: 0 }),
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
    putAgentState: answerAgentStates().mockResolvedValue({ unread_replies: 0 }),
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
    putAgentState: answerAgentStates().mockResolvedValue({ unread_replies: 0 }),
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
    expect(createAgentMessage).toHaveBeenCalledWith(
      SESSION,
      { body: "try the other branch", delivery: "steer" },
      keyedAttempt
    )
  );

  cleanup();
  for (const spy of spies.splice(0)) spy.mockRestore();
  renderLiveView({
    agentState: {},
    capabilities: ["aside"],
    messages: [],
    putAgentState: answerAgentStates().mockResolvedValue({ unread_replies: 0 }),
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

/** A fetch that failed, as the client hands it on: no answer from Dispatch at all. */
const dropped = () =>
  new NetworkError(`/api/v1/agents/${SESSION}/messages`, new TypeError("Failed to fetch"));

/** Types `body` into the live view's composer and presses its Send. */
async function sendFromLiveView(body: string): Promise<void> {
  fireEvent.change(screen.getByPlaceholderText(/delivered as Send/), { target: { value: body } });
  await act(async () => {
    fireEvent.click(
      within(screen.getByTestId("agent-composer")).getByRole("button", { name: "Send" })
    );
  });
}

/** Runs `body` on the fake clock, which moves a millisecond before the real one comes back: a
 *  timer left at zero delay would otherwise hang a later `waitFor`. */
async function onFakeClock(body: () => Promise<void>): Promise<void> {
  jest.useFakeTimers();
  try {
    await body();
  } finally {
    act(() => {
      jest.advanceTimersByTime(1);
    });
    jest.useRealTimers();
  }
}

// A message sent from the live view is one key, minted when the person presses Send: a fetch that
// fails, or a Dispatch that cannot answer yet, is tried again under it, so the session gets the
// message once. The next press is another message, with a key of its own.
test("the live view's send retries under one key", async () => {
  const createAgentMessage = spyOn(api, "createAgentMessage")
    .mockRejectedValueOnce(dropped())
    .mockRejectedValueOnce(new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" }))
    .mockImplementation(async (_session, input) => message("sent", input.body));
  spies.push(createAgentMessage);
  renderLiveView({
    agentState: {},
    messages: [],
    putAgentState: answerAgentStates().mockResolvedValue({ unread_replies: 0 }),
  });
  const mode = (await screen.findByRole("combobox", {
    name: "Delivery mode",
  })) as HTMLSelectElement;
  await waitFor(() => expect(mode.value).toBe("steer"));

  await onFakeClock(async () => {
    await sendFromLiveView("try the other branch");
    expect(createAgentMessage).toHaveBeenCalledTimes(1);
    await act(async () => {
      jest.advanceTimersByTime(1_000);
    });
    expect(createAgentMessage).toHaveBeenCalledTimes(2);
    await act(async () => {
      jest.advanceTimersByTime(2_000);
    });
    expect(createAgentMessage).toHaveBeenCalledTimes(3);
  });
  expect(screen.queryByTestId("agent-send-error")).toBeNull();

  await sendFromLiveView("and this");
  await waitFor(() => expect(createAgentMessage).toHaveBeenCalledTimes(4));
  const [first, ...later] = sentKeys(createAgentMessage);
  expect(first).toMatch(/^[0-9a-f-]{36}$/);
  expect(later.slice(0, 2)).toEqual([first, first]);
  expect(later[2]).not.toBe(first);
});

// A send belongs to its session, not to the view: the live view going away - here unmounted with
// its page - leaves its send being tried under the key it was pressed with, and every attempt is
// held to its own deadline, nothing cutting it short.
test("leaving the live view leaves its send being tried, under its key", async () => {
  const createAgentMessage = spyOn(api, "createAgentMessage").mockRejectedValue(dropped());
  spies.push(createAgentMessage);
  renderLiveView({
    agentState: {},
    messages: [],
    putAgentState: answerAgentStates().mockResolvedValue({ unread_replies: 0 }),
  });
  const mode = (await screen.findByRole("combobox", {
    name: "Delivery mode",
  })) as HTMLSelectElement;
  await waitFor(() => expect(mode.value).toBe("steer"));

  await onFakeClock(async () => {
    await sendFromLiveView("try the other branch");
    await tick(1_000);
    expect(createAgentMessage).toHaveBeenCalledTimes(2);

    cleanup();
    await tick(2_000);
    await tick(4_000);
    expect(createAgentMessage).toHaveBeenCalledTimes(4);
    // Dispatch is back: the send lands, and nothing is tried after it.
    createAgentMessage.mockResolvedValueOnce(message("sent", "try the other branch"));
    await tick(8_000);
    await tick(30 * 60_000);
    expect(createAgentMessage).toHaveBeenCalledTimes(5);
  });
  expect(createAgentMessage.mock.calls.map(([, , bounds]) => bounds)).toEqual(
    Array.from({ length: 5 }, () => keyedAttempt)
  );
  expect(new Set(sentKeys(createAgentMessage)).size).toBe(1);
});

/** Moves the fake clock `ms` on and lets every promise the move released run. */
async function tick(ms: number): Promise<void> {
  await act(async () => {
    jest.advanceTimersByTime(ms);
    for (let turn = 0; turn < 20; turn += 1) await Promise.resolve();
  });
}

/** The live view for SESSION, its mode settled on Send. */
async function renderSendingView(): Promise<void> {
  renderLiveView({
    agentState: {},
    messages: [],
    putAgentState: answerAgentStates().mockResolvedValue({ unread_replies: 0 }),
  });
  const mode = (await screen.findByRole("combobox", {
    name: "Delivery mode",
  })) as HTMLSelectElement;
  await waitFor(() => expect(mode.value).toBe("steer"));
}

// W25 / INFRA-442. Send is pressable whenever the box holds text: each press is its own write under
// its own key, shown as its own pending turn until Dispatch confirms it, and a later press waits
// only behind an earlier one still being tried - so they reach the session in press order, each
// once. Past a send's deadline its turn says it is still being tried, with Stop trying, and Send
// stays open.
test("Send stays open with text while an earlier send is out or late, and each press is its own turn", async () => {
  const answers: ReturnType<typeof Promise.withResolvers<Message>>[] = [];
  const createAgentMessage = spyOn(api, "createAgentMessage").mockImplementation(() => {
    const answer = Promise.withResolvers<Message>();
    answers.push(answer);
    return answer.promise;
  });
  spies.push(createAgentMessage);
  await renderSendingView();

  await onFakeClock(async () => {
    await sendFromLiveView("try the other branch");
    const [held] = screen.getAllByTestId("agent-send-held");
    // The pending turn names the message as the reader's own, at a glance and to a screen reader.
    expect(
      within(held as HTMLElement).getByText("Your message: “try the other branch”")
    ).toBeTruthy();
    expect(within(held as HTMLElement).getByRole("status").textContent).toBe("Sending…");

    // The field cleared on the press, and the next message goes out as its own press.
    const input = screen.getByPlaceholderText(/delivered as Send/) as HTMLTextAreaElement;
    expect(input.value).toBe("");
    fireEvent.change(input, { target: { value: "and this" } });
    expect(sendButton().disabled).toBe(false);
    expect(sendButton().getAttribute("aria-disabled")).toBeNull();
    await act(async () => {
      fireEvent.click(sendButton());
    });
    await tick(0);
    expect(screen.getAllByTestId("agent-send-held")).toHaveLength(2);
    // In press order: the second waits while the first is being tried.
    expect(createAgentMessage).toHaveBeenCalledTimes(1);

    await tick(SEND_DEADLINE_MS);
    const [late] = screen.getAllByTestId("agent-send-held");
    expect(within(late as HTMLElement).getByRole("status").textContent).toBe(KEYED_SEND_LATE_TEXT);
    expect(within(late as HTMLElement).getByRole("button", { name: "Stop trying" })).toBeTruthy();
    fireEvent.change(input, { target: { value: "a third" } });
    expect(sendButton().getAttribute("aria-disabled")).toBeNull();
    fireEvent.change(input, { target: { value: "" } });

    await act(async () => {
      answers[0]?.resolve(message("sent", "try the other branch"));
    });
    await tick(0);
    expect(createAgentMessage.mock.calls.map(([, sent]) => sent.body)).toEqual([
      "try the other branch",
      "and this",
    ]);
    await act(async () => {
      answers[1]?.resolve(message("sent", "and this"));
    });
    await tick(0);
  });
  expect(screen.queryByTestId("agent-send-held")).toBeNull();
  const [first, second] = sentKeys(createAgentMessage);
  expect(second).not.toBe(first);
});

// Dispatch's own refusal ends the send: the message stays on screen in its turn with the reason,
// Retry sends it again under a key of its own - Dispatch would answer the refused key with its
// refusal - and Discard drops it. A refused message holds nothing: Send stays open with text, and
// the next message is not held behind it.
test("a message Dispatch refuses offers Retry under a new key, Discard, and holds nothing", async () => {
  const refused = new ApiError(400, { code: "MESSAGE_INPUT", error: "Dispatch refused it" });
  const createAgentMessage = spyOn(api, "createAgentMessage")
    .mockRejectedValueOnce(refused)
    .mockImplementationOnce(async (_session, input) => message("sent", input.body))
    .mockRejectedValue(refused);
  spies.push(createAgentMessage);
  await renderSendingView();

  await sendFromLiveView("try the other branch");
  const error = await screen.findByTestId("agent-send-error");
  expect(error.textContent).toBe("Could not send: Dispatch refused it");
  await sendFromLiveView("the next one");
  await waitFor(() => expect(createAgentMessage).toHaveBeenCalledTimes(2));
  expect(createAgentMessage.mock.calls[1]?.[1].body).toBe("the next one");

  const held = screen.getByTestId("agent-send-held");
  fireEvent.click(within(held).getByRole("button", { name: "Retry" }));
  await waitFor(() => expect(createAgentMessage).toHaveBeenCalledTimes(3));
  const [first, , retried] = sentKeys(createAgentMessage);
  expect(retried).not.toBe(first);

  await screen.findByTestId("agent-send-error");
  fireEvent.click(
    within(screen.getByTestId("agent-send-held")).getByRole("button", { name: "Discard" })
  );
  expect(screen.queryByTestId("agent-send-held")).toBeNull();
  expect(sendButton().getAttribute("title")).toBe("Type a message first.");
  expect(createAgentMessage).toHaveBeenCalledTimes(3);
});

// A lasting outage would hold the message for good. Once it is late, Stop trying ends it after the
// attempt out: nothing more is sent, the message stays with what a Retry does, and a Retry goes
// under the same key, so it is delivered at most once.
test("Stop trying ends a late live view send, and its Retry goes under the same key", async () => {
  const createAgentMessage = spyOn(api, "createAgentMessage").mockRejectedValue(
    new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" })
  );
  spies.push(createAgentMessage);
  await renderSendingView();

  await onFakeClock(async () => {
    await sendFromLiveView("try the other branch");
    for (const wait of [1_000, 2_000, 4_000, 8_000]) await tick(wait);
    expect(screen.queryByRole("button", { name: "Stop trying" })).toBeNull();
    await tick(15_000);
    expect(createAgentMessage).toHaveBeenCalledTimes(5);

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Stop trying" }));
    });
    await tick(0);
    expect(screen.getByTestId("agent-send-error").textContent).toBe(new SendStoppedError().message);
    await tick(30 * 60_000);
    expect(createAgentMessage).toHaveBeenCalledTimes(5);

    createAgentMessage.mockResolvedValueOnce(message("sent", "try the other branch"));
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    });
    await tick(0);
  });
  expect(createAgentMessage).toHaveBeenCalledTimes(6);
  expect(new Set(sentKeys(createAgentMessage)).size).toBe(1);
  expect(screen.queryByTestId("agent-send-held")).toBeNull();
});

// A Claude Code session whose Envoy channel Claude Code did not load advertises no mode: the
// live view says a message sent here records a failed attempt, before the person sends one.
test("the live view says a session advertising no delivery mode takes no messages", async () => {
  renderLiveView({
    agentState: {},
    capabilities: [],
    messages: [],
    putAgentState: answerAgentStates().mockResolvedValue({ unread_replies: 0 }),
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
    putAgentState: answerAgentStates().mockResolvedValue({ unread_replies: 0 }),
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
    putAgentState: answerAgentStates().mockResolvedValue({ unread_replies: 0 }),
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
    putAgentState: answerAgentStates().mockResolvedValue({ unread_replies: 0 }),
    replay: [assistantFrame(1, Date.parse("2026-09-27T21:16:16Z"), "Switching to it now.")],
  });

  const thread = await screen.findByTestId("agent-thread");
  await within(thread).findByText("Switching to it now.");
  expect(screen.queryByTestId("agent-session-model")).toBeNull();
  expect(within(thread).queryByTestId("agent-message-model")).toBeNull();
});

// The header says what the relay is doing: connecting until the session's first answer arrives on
// the tab's socket, live from then on, and reconnecting while that socket is lost.
test("the live view connects, goes live on the session's answer, and reconnects with the socket", async () => {
  renderLiveView({
    agentState: {},
    messages: [],
    putAgentState: answerAgentStates().mockResolvedValue({ unread_replies: 0 }),
  });
  expect(relayStatus()).toEqual(["Connecting to the session"]);

  // An open socket is not yet an answer: the view goes live on its first frame.
  const first = sockets.latest();
  act(() => first.open());
  expect(first.subscribed("agent").view).toEqual({ kind: "agent", session_id: SESSION });
  expect(relayStatus()).toEqual(["Connecting to the session"]);
  answer(first, [assistantFrame(1, Date.parse("2026-09-27T21:16:16Z"), "Before the restart.")]);
  await within(screen.getByTestId("agent-thread")).findByText("Before the restart.");
  expect(relayStatus()).toEqual(["Live"]);

  // Dispatch restarting (1012) is a gap the page shows until the next socket opens.
  act(() => first.drop(1012));
  expect(relayStatus()).toEqual(["Reconnecting"]);
  const second = sockets.latest();
  expect(second).not.toBe(first);
  act(() => second.open());
  expect(relayStatus()).toEqual(["Live"]);
  expect(agentSubscription(second)).toBe(agentSubscription(first));
});

// Dispatch rotates every socket once it reaches its age (4001): the page reopens at once and sends
// the live view again, and the session answers with its replay, everything it still holds. The
// transcript is rebuilt from that replay rather than layered over the old one, whose oldest turns
// may be gone from the session's ring, and nothing about the relay reads as lost.
test("a rotation rebuilds from the replay and keeps status live", async () => {
  renderLiveView({
    agentState: {},
    messages: [],
    putAgentState: answerAgentStates().mockResolvedValue({ unread_replies: 0 }),
    replay: [assistantFrame(1, Date.parse("2026-09-27T21:16:16Z"), "Before the rotation.")],
  });
  const thread = await screen.findByTestId("agent-thread");
  await within(thread).findByText("Before the rotation.");
  expect(relayStatus()).toEqual(["Live"]);

  const first = sockets.latest();
  act(() => first.drop(LIVE_SOCKET_MAX_AGE_CLOSE));
  expect(sockets.sockets).toHaveLength(2);
  expect(relayStatus()).toEqual(["Live"]);
  const second = sockets.latest();
  act(() => second.open());
  expect(agentSubscription(second)).toBe(agentSubscription(first));
  expect(relayStatus()).toEqual(["Live"]);

  answer(second, [assistantFrame(2, Date.parse("2026-09-27T21:17:16Z"), "After the rotation.")]);
  await within(thread).findByText("After the rotation.");
  await waitFor(() => expect(within(thread).queryByText("Before the rotation.")).toBeNull());
  expect(relayStatus()).toEqual(["Live"]);
});

// Dispatch relays a session's conversation to a person only. Refused, the view says it cannot
// reach the conversation, and that holds for this session: no reconnect of the tab's socket sends
// the view again or shows it reconnecting.
test("a human-only refusal shows the view unavailable", async () => {
  const queryClient = renderLiveView({
    agentState: {},
    messages: [],
    putAgentState: answerAgentStates().mockResolvedValue({ unread_replies: 0 }),
  });
  // The app's event feed rides the same socket, so the socket outlives the refused view.
  liveSocketFor(queryClient).subscribeEvents({
    onEvent: () => undefined,
    onStart: () => undefined,
    since: () => null,
  });
  const first = sockets.latest();
  act(() => first.open());
  act(() =>
    first.receive(
      errorFrame(agentSubscription(first), "HUMAN_ONLY", "only users may perform this action")
    )
  );
  expect((await screen.findByTestId("agent-stream-unavailable")).textContent).toBe(
    "This Dispatch cannot reach the session's conversation."
  );
  expect(relayStatus()).toEqual(["This session's conversation is unavailable"]);

  act(() => first.drop(1012));
  expect(relayStatus()).toEqual(["This session's conversation is unavailable"]);
  const second = sockets.latest();
  expect(second).not.toBe(first);
  act(() => second.open());
  expect(second.subscribes().map((frame) => frame.view.kind)).toEqual(["events"]);
  expect(relayStatus()).toEqual(["This session's conversation is unavailable"]);
  expect(screen.getByTestId("agent-stream-unavailable")).toBeTruthy();
});

// Two live views in one tab are two subscriptions on the tab's one socket, each answered on its
// own; leaving one lets only its subscription go.
test("two agent views share the tab's socket", async () => {
  const REVIEWER = "01a0e090-4848-7473-acc5-fc96e6a646d4";
  mockDispatch({
    agentState: {},
    agents: [agent, { ...agent, session_id: REVIEWER, title: "Reviewer" }],
    messages: () => [],
    putAgentState: answerAgentStates().mockResolvedValue({ unread_replies: 0 }),
  });
  const queryClient = newClient();
  const planner = renderPage(queryClient, SESSION);
  const reviewer = renderPage(queryClient, REVIEWER);
  expect(sockets.sockets).toHaveLength(1);
  const socket = sockets.latest();
  act(() => socket.open());
  expect(socket.subscribes().map((frame) => frame.view)).toEqual([
    { kind: "agent", session_id: SESSION },
    { kind: "agent", session_id: REVIEWER },
  ]);
  expect(agentSubscription(socket, SESSION)).not.toBe(agentSubscription(socket, REVIEWER));

  const at = Date.parse("2026-09-27T21:16:16Z");
  answer(socket, [assistantFrame(1, at, "Planning the change.")], SESSION);
  answer(socket, [assistantFrame(1, at, "Reviewing the change.")], REVIEWER);
  await within(planner.container).findByText("Planning the change.");
  await within(reviewer.container).findByText("Reviewing the change.");
  expect(within(planner.container).queryByText("Reviewing the change.")).toBeNull();
  expect(within(reviewer.container).queryByText("Planning the change.")).toBeNull();
  expect(relayStatus(planner.container)).toEqual(["Live"]);
  expect(relayStatus(reviewer.container)).toEqual(["Live"]);

  const plannerSub = agentSubscription(socket, SESSION);
  planner.unmount();
  expect(socket.unsubscribes()).toEqual([{ type: "unsubscribe", sub: plannerSub }]);
  await Promise.resolve();
  expect(socket.closedWith).toBeUndefined();
  act(() =>
    socket.receive({
      type: "agent",
      sub: agentSubscription(socket, REVIEWER),
      event: "frame",
      data: assistantFrame(2, at + 1_000, "Still reviewing."),
    })
  );
  await within(reviewer.container).findByText("Still reviewing.");
  expect(sockets.sockets).toHaveLength(1);
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
    putAgentState: answerAgentStates().mockResolvedValue({ unread_replies: 0 }),
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
    expect(createAgentMessage).toHaveBeenCalledWith(
      SESSION,
      { body: "/compact", delivery: "steer" },
      keyedAttempt
    )
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
    expect(createAgentMessage).toHaveBeenCalledWith(
      SESSION,
      { body: "/rec", delivery: "steer" },
      keyedAttempt
    )
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

/** The models a session lists, in its own order: two providers interleaved, and a custom
 *  provider's id holding an `@` and a trailing `:<level>`, which a pick must send unchanged. */
const MODELS = ["anthropic/model-a", "openai/model-c", "anthropic/model-b", "custom/model@v2:high"];

/** A reported settings frame (INFRA-195): model-a at thinking `auto`, unless `fields` say
 *  otherwise. */
function settingsFrame(
  seq: number,
  fields: Partial<Omit<AgentStreamSettingsReported, "kind" | "reported" | "seq" | "v">> = {}
): AgentStreamFrame {
  return {
    kind: "settings",
    model: "anthropic/model-a",
    models: MODELS,
    reported: true,
    seq,
    thinking: { level: "auto", levels: ["auto", "low", "high"] },
    v: 1,
    ...fields,
  };
}

/** The commands of a session that runs `/model` and `/effort` from Dispatch. */
const SWITCHES: AgentStreamCommand[] = [
  ...COMMANDS,
  { name: "model", source: "builtin" },
  { name: "effort", source: "builtin" },
];

/** The live view of a session reporting `replay`, once its model picker is on screen. */
async function renderWithPickers(replay: AgentStreamFrame[]): Promise<LiveView> {
  const view = renderLiveView({
    agentState: {},
    messages: [],
    putAgentState: answerAgentStates().mockResolvedValue({ unread_replies: 0 }),
    replay,
  });
  await screen.findByRole("combobox", { name: "Model" });
  return view;
}

function picker(name: "Model" | "Thinking level"): HTMLSelectElement {
  return screen.getByRole("combobox", { name }) as HTMLSelectElement;
}

/** Whether a pending switch holds the picker: `aria-disabled`, so it keeps focus and its keys. */
function held(select: HTMLSelectElement): boolean {
  return select.getAttribute("aria-disabled") === "true";
}

/** Answers each switch's send as Dispatch stores it: the person's message, its attempt sent. */
function spyOnSwitchSends() {
  let count = 0;
  const createAgentMessage = answerAgentMessages().mockImplementation(async (_session, input) => {
    count += 1;
    return message(`switch-${count}`, input.body);
  });
  spies.push(createAgentMessage);
  return createAgentMessage;
}

/** The session's reply to the stored switch `id`. */
function sessionReply(id: string, body: string): Message {
  return message(`reply-${id}`, body, {
    author: { id: SESSION, kind: "session" },
    created_at: "2026-09-27T21:17:16Z",
    in_reply_to: id,
  });
}

// The header shows the model and thinking level the session's newest settings frame reports
// (INFRA-195), over the last turn's model, before any turn runs on them, and a change made
// anywhere else (the terminal, a fallback) as its frame arrives. What the header renders of a
// view, the pickers' options and keys included, is `SessionSettings.test.tsx`'s.
test("the header shows the model and thinking level each settings frame reports, as it arrives", async () => {
  const view = await renderWithPickers([
    assistantFrame(1, Date.parse("2026-09-27T21:16:16Z"), "Done.", "anthropic/old-model"),
    settingsFrame(2),
    commandsFrame(3, SWITCHES),
  ]);

  expect(picker("Model").value).toBe("anthropic/model-a");
  expect(screen.getByTestId("agent-session-model").textContent).toBe("anthropic/model-a");
  expect(picker("Thinking level").value).toBe("auto");
  expect(screen.queryByTestId("agent-settings-notice")).toBeNull();

  view.frame(
    settingsFrame(4, {
      model: "openai/fallback",
      thinking: { level: null, levels: ["low", "high"] },
    })
  );
  await waitFor(() => expect(picker("Model").value).toBe("openai/fallback"));
  expect(screen.getByTestId("agent-session-model").textContent).toBe("openai/fallback");
  expect(picker("Thinking level").value).toBe("");
  expect(screen.getByTestId("agent-session-thinking").textContent).toBe("model default");
});

// A session that does not answer the live view at all gets no settings notice: the empty
// transcript's own explanation says why.
test("a session not answering the live view keeps the empty transcript's explanation, no settings notice", async () => {
  renderLiveView({
    agentState: {},
    messages: [],
    putAgentState: answerAgentStates().mockResolvedValue({ unread_replies: 0 }),
    responding: false,
  });
  expect((await screen.findByTestId("agent-thread-empty")).textContent).toContain(
    "This session is not answering the live view"
  );
  expect(screen.queryByTestId("agent-settings-notice")).toBeNull();
});

// A pick is the person's own Send of what they would type at the session's terminal: always a
// Send, since a BTW is a side question the session does not run, and the id exactly as the
// session listed it, even one `/model` could read a role or a level into.
test("a pick sends /model or /effort as a Send whatever Send as says, the id unchanged", async () => {
  const createAgentMessage = spyOnSwitchSends();
  const view = await renderWithPickers([settingsFrame(1), commandsFrame(2, SWITCHES)]);
  fireEvent.change(screen.getByRole("combobox", { name: "Delivery mode" }), {
    target: { value: "btw" },
  });

  fireEvent.change(picker("Model"), { target: { value: "custom/model@v2:high" } });
  await waitFor(() =>
    expect(createAgentMessage).toHaveBeenCalledWith(
      SESSION,
      { body: "/model custom/model@v2:high", delivery: "steer" },
      keyedAttempt
    )
  );
  await view.storeThread([
    {
      message: message("switch-1", "/model custom/model@v2:high"),
      replies: [sessionReply("switch-1", "Model set to custom/model@v2:high.")],
    },
  ]);
  await waitFor(() => expect(held(picker("Thinking level"))).toBe(false));

  fireEvent.change(picker("Thinking level"), { target: { value: "high" } });
  await waitFor(() =>
    expect(createAgentMessage).toHaveBeenLastCalledWith(
      SESSION,
      { body: "/effort high", delivery: "steer" },
      keyedAttempt
    )
  );
  expect(createAgentMessage).toHaveBeenCalledTimes(2);
});

// One switch at a time, and the person's next prompt from this page reaches the session only
// after the switch answered. The header's values come from the session's frames alone, so the
// reply and the frame may arrive in either order. What a held picker does with a pick is
// `SessionSettings.test.tsx`'s.
test("while a switch waits the pickers and Send hold, and its reply and frame land in either order", async () => {
  const createAgentMessage = spyOnSwitchSends();
  const view = await renderWithPickers([settingsFrame(1), commandsFrame(2, SWITCHES)]);
  const input = within(screen.getByTestId("agent-composer")).getByRole(
    "textbox"
  ) as HTMLTextAreaElement;
  type(input, "and then run the tests");
  await waitFor(() => expect(sendButton().disabled).toBe(false));

  // The reply before the frame.
  fireEvent.change(picker("Model"), { target: { value: "anthropic/model-b" } });
  expect((await screen.findByTestId("agent-switch-status")).textContent).toBe(
    "Switching model to anthropic/model-b…"
  );
  await waitFor(() => expect(held(picker("Model"))).toBe(true));
  expect(held(picker("Thinking level"))).toBe(true);
  expect(sendButton().disabled).toBe(true);
  expect(createAgentMessage).toHaveBeenCalledTimes(1);
  expect(screen.getByTestId("agent-switch-status").textContent).toBe(
    "Switching model to anthropic/model-b…"
  );
  expect(input.placeholder).toBe("Waiting for the session to answer the switch…");
  expect(input.value).toBe("and then run the tests");
  // Nothing the session has not reported shows.
  expect(picker("Model").value).toBe("anthropic/model-a");

  const first = message("switch-1", "/model anthropic/model-b");
  await view.storeThread([
    { message: first, replies: [sessionReply("switch-1", "Model set to anthropic/model-b.")] },
  ]);
  await waitFor(() =>
    expect(screen.getByTestId("agent-switch-status").textContent).toBe(
      "The session answered: Model set to anthropic/model-b."
    )
  );
  expect(picker("Model").value).toBe("anthropic/model-a");
  expect(held(picker("Model"))).toBe(false);
  expect(sendButton().disabled).toBe(false);
  view.frame(settingsFrame(3, { model: "anthropic/model-b" }));
  await waitFor(() => expect(picker("Model").value).toBe("anthropic/model-b"));

  // The frame before the reply.
  fireEvent.change(picker("Thinking level"), { target: { value: "high" } });
  await waitFor(() => expect(sendButton().disabled).toBe(true));
  view.frame(
    settingsFrame(4, {
      model: "anthropic/model-b",
      thinking: { level: "high", levels: ["auto", "low", "high"] },
    })
  );
  await waitFor(() => expect(picker("Thinking level").value).toBe("high"));
  expect(screen.getByTestId("agent-switch-status").textContent).toBe(
    "Switching thinking level to high…"
  );
  expect(sendButton().disabled).toBe(true);
  expect(held(picker("Thinking level"))).toBe(true);
  await view.storeThread([
    { message: first, replies: [sessionReply("switch-1", "Model set to anthropic/model-b.")] },
    {
      message: message("switch-2", "/effort high"),
      replies: [sessionReply("switch-2", "Thinking set to high.")],
    },
  ]);
  await waitFor(() =>
    expect(screen.getByTestId("agent-switch-status").textContent).toBe(
      "The session answered: Thinking set to high."
    )
  );
  expect(sendButton().disabled).toBe(false);
  expect(picker("Thinking level").value).toBe("high");
});

// A switch the session refuses publishes no new frame: the header keeps what the session last
// reported, and the line under it is the session's own reason.
test("a refused switch shows the session's reason and the header keeps its values", async () => {
  spyOnSwitchSends();
  const view = await renderWithPickers([settingsFrame(1), commandsFrame(2, SWITCHES)]);

  fireEvent.change(picker("Model"), { target: { value: "openai/model-c" } });
  await screen.findByTestId("agent-switch-status");
  await view.storeThread([
    {
      message: message("switch-1", "/model openai/model-c"),
      replies: [sessionReply("switch-1", "Failed to set model: No API key for openai/model-c")],
    },
  ]);
  await waitFor(() =>
    expect(screen.getByTestId("agent-switch-status").textContent).toBe(
      "The session answered: Failed to set model: No API key for openai/model-c"
    )
  );
  expect(picker("Model").value).toBe("anthropic/model-a");
  expect(screen.getByTestId("agent-session-model").textContent).toBe("anthropic/model-a");
  expect(picker("Thinking level").value).toBe("auto");
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
  const createAgentMessage = answerAgentMessages().mockImplementation(() => {
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
    putAgentState: answerAgentStates().mockResolvedValue({ unread_replies: 0 }),
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
    expect(createAgentMessage).toHaveBeenCalledWith(
      SESSION,
      { body: "try the other branch", delivery: "steer" },
      keyedAttempt
    )
  );
});

// Enter is the keyboard's Send, and a turn still streaming does not swallow it.
test("Enter sends while the session is mid-turn", async () => {
  const createAgentMessage = acceptSends();
  renderLiveView({
    agentState: {},
    messages: [],
    putAgentState: answerAgentStates().mockResolvedValue({ unread_replies: 0 }),
    replay: MID_TURN,
  });
  const input = await modeSettled("steer");
  await midTurnShown();

  type(input, "stop after this file");
  fireEvent.keyDown(input, { key: "Enter" });
  await waitFor(() =>
    expect(createAgentMessage).toHaveBeenCalledWith(
      SESSION,
      { body: "stop after this file", delivery: "steer" },
      keyedAttempt
    )
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
    putAgentState: answerAgentStates().mockResolvedValue({ unread_replies: 0 }),
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

// A send Dispatch refuses is not lost: the message is held on screen with the reason, for Retry or
// Discard, and the box is free for the next one.
test("a refused send keeps the message on screen and says why", async () => {
  spies.push(
    spyOn(api, "createAgentMessage").mockRejectedValue(
      new ApiError(400, { code: "MESSAGE_INPUT", error: "Dispatch refused it" })
    )
  );
  renderLiveView({
    agentState: {},
    messages: [],
    putAgentState: answerAgentStates().mockResolvedValue({ unread_replies: 0 }),
  });
  const input = await modeSettled("steer");

  type(input, "try the other branch");
  fireEvent.click(sendButton());
  expect((await screen.findByTestId("agent-send-error")).textContent).toBe(
    "Could not send: Dispatch refused it"
  );
  expect(
    within(screen.getByTestId("agent-send-held")).getByText("Your message: “try the other branch”")
  ).toBeTruthy();
  expect(composerInput().value).toBe("");
});

// A refusal belongs to the session that sent the message. One landing while the reader is on
// another session leaves that session's box and screen alone, and is waiting, message and reason,
// when they go back. A send out on one session does not hold Send on another.
test("a refusal that lands after switching sessions stays with the session that sent it, not the one on screen", async () => {
  const { createAgentMessage, held } = holdSends();
  renderLiveView({
    agentState: {},
    messages: [],
    other: {},
    putAgentState: answerAgentStates().mockResolvedValue({ unread_replies: 0 }),
  });
  const input = await modeSettled("steer");

  type(input, "hold this");
  fireEvent.click(sendButton());
  await waitFor(() =>
    expect(createAgentMessage).toHaveBeenCalledWith(
      SESSION,
      { body: "hold this", delivery: "steer" },
      keyedAttempt
    )
  );

  const other = await switchTo("Reviewer");
  type(other, "Reviewer's own words");
  expect(sendButton().hasAttribute("disabled")).toBe(false);
  await act(async () => {
    held[0]?.reject(new ApiError(400, { code: "MESSAGE_INPUT", error: "Dispatch refused it" }));
    await Bun.sleep(50);
  });
  expect(composerInput().value).toBe("Reviewer's own words");
  expect(screen.queryByTestId("agent-send-error")).toBeNull();
  expect(screen.queryByTestId("agent-send-held")).toBeNull();

  await switchTo("Planner");
  expect((await screen.findByTestId("agent-send-error")).textContent).toBe(
    "Could not send: Dispatch refused it"
  );
  expect(
    within(screen.getByTestId("agent-send-held")).getByText("Your message: “hold this”")
  ).toBeTruthy();
});

// Messages to one session go out one after another, in press order: Send stays open while one is
// out, and a press - a click or Enter - while one is out is its own message, which waits its turn.
// A refused send is one turn, however often a Retry of it is refused, and it no longer holds the
// line: its Retry joins behind what was pressed since.
test("a send pressed while another is out waits its turn, and a refused one is retried at the back", async () => {
  const { createAgentMessage, held } = holdSends();
  renderLiveView({
    agentState: {},
    messages: [],
    putAgentState: answerAgentStates().mockResolvedValue({ unread_replies: 0 }),
  });
  const input = await modeSettled("steer");
  const refusal = new ApiError(400, { code: "MESSAGE_INPUT", error: "Dispatch refused it" });

  type(input, "first");
  fireEvent.click(sendButton());
  await waitFor(() =>
    expect(createAgentMessage).toHaveBeenCalledWith(
      SESSION,
      { body: "first", delivery: "steer" },
      keyedAttempt
    )
  );
  type(composerInput(), "second");
  expect(sendButton().hasAttribute("aria-disabled")).toBe(false);
  expect(within(composer()).getByText("Enter to send · Shift+Enter for a new line")).toBeTruthy();
  fireEvent.keyDown(composerInput(), { key: "Enter" });
  await waitFor(() => expect(screen.getAllByTestId("agent-send-held")).toHaveLength(2));
  expect(createAgentMessage).toHaveBeenCalledTimes(1);

  await act(async () => {
    held[0]?.reject(refusal);
  });
  await screen.findByTestId("agent-send-error");
  await waitFor(() => expect(createAgentMessage).toHaveBeenCalledTimes(2));
  expect(createAgentMessage).toHaveBeenLastCalledWith(
    SESSION,
    { body: "second", delivery: "steer" },
    keyedAttempt
  );

  fireEvent.click(
    within(screen.getAllByTestId("agent-send-held")[0] as HTMLElement).getByRole("button", {
      name: "Retry",
    })
  );
  await act(async () => {
    held[1]?.resolve(message("sent", "second"));
  });
  await waitFor(() => expect(createAgentMessage).toHaveBeenCalledTimes(3));
  expect(createAgentMessage).toHaveBeenLastCalledWith(
    SESSION,
    { body: "first", delivery: "steer" },
    keyedAttempt
  );
  await act(async () => {
    held[2]?.reject(refusal);
  });
  await screen.findByTestId("agent-send-error");
  expect(screen.getAllByTestId("agent-send-held")).toHaveLength(1);
  fireEvent.click(
    within(screen.getByTestId("agent-send-held")).getByRole("button", { name: "Retry" })
  );
  await waitFor(() => expect(createAgentMessage).toHaveBeenCalledTimes(4));
  await act(async () => {
    held[3]?.resolve(message("sent", "first"));
  });
  await waitFor(() => expect(screen.queryByTestId("agent-send-held")).toBeNull());
});

// What a person is writing to one session stays with it: switching to another shows that
// session's own box, and switching back finds the draft where it was left.
test("a draft stays with its session across a switch to another", async () => {
  renderLiveView({
    agentState: {},
    messages: [],
    other: {},
    putAgentState: answerAgentStates().mockResolvedValue({ unread_replies: 0 }),
  });
  const input = await modeSettled("steer");

  type(input, "draft for the planner");
  const other = await switchTo("Reviewer");
  expect(other.value).toBe("");
  const back = await switchTo("Planner");
  expect(back.value).toBe("draft for the planner");
});

// A model or thinking switch pending on a session holds that session's Send (it keeps reading
// "Send", and the line under the box says what its placeholder says) and holds no other's; a
// message out on another session holds nothing there either, and Enter sends nothing while a
// switch holds.
test("a pending switch holds its own session's Send, and a send out on another holds nothing there", async () => {
  const { createAgentMessage, held: answers } = holdSends();
  const view = renderLiveView({
    agentState: {},
    messages: [],
    other: {},
    putAgentState: answerAgentStates().mockResolvedValue({ unread_replies: 0 }),
    replay: [settingsFrame(1), commandsFrame(2, SWITCHES)],
  });
  await screen.findByRole("combobox", { name: "Model" });
  await modeSettled("steer");

  fireEvent.change(picker("Model"), { target: { value: "anthropic/model-b" } });
  await waitFor(() =>
    expect(createAgentMessage).toHaveBeenCalledWith(
      SESSION,
      { body: "/model anthropic/model-b", delivery: "steer" },
      keyedAttempt
    )
  );
  await waitFor(() => expect(sendButton().disabled).toBe(true));
  const wait = "Waiting for the session to answer the switch…";
  const describedBy = (sendButton().getAttribute("aria-describedby") ?? "")
    .split(" ")
    .map((id) => document.getElementById(id)?.textContent ?? "")
    .join(" ");
  expect(describedBy).toBe(wait);
  expect(within(composer()).queryByText("Type a message first.")).toBeNull();
  type(composerInput(), "and then run the tests");
  expect(sendButton().disabled).toBe(true);
  expect(within(composer()).getByText(wait)).toBeTruthy();
  expect(within(composer()).queryByText("Enter to send · Shift+Enter for a new line")).toBeNull();
  fireEvent.keyDown(composerInput(), { key: "Enter" });
  await Bun.sleep(50);
  expect(createAgentMessage).toHaveBeenCalledTimes(1);

  const other = await switchTo("Reviewer");
  type(other, "review the diff");
  await waitFor(() => expect(sendButton().disabled).toBe(false));
  fireEvent.click(sendButton());
  await waitFor(() =>
    expect(createAgentMessage).toHaveBeenLastCalledWith(
      OTHER,
      { body: "review the diff", delivery: "steer" },
      keyedAttempt
    )
  );
  await waitFor(() => expect(screen.getAllByTestId("agent-send-held")).toHaveLength(1));
  expect(sendButton().getAttribute("title")).toBe("Type a message first.");

  const back = await switchTo("Planner");
  expect(back.value).toBe("and then run the tests");
  expect(sendButton().disabled).toBe(true);
  expect(screen.queryByTestId("agent-send-held")).toBeNull();
  await act(async () => {
    answers[0]?.resolve(message("switch-1", "/model anthropic/model-b"));
  });
  await waitFor(() =>
    expect(screen.getByTestId("agent-switch-status").textContent).toBe(
      "Switching model to anthropic/model-b…"
    )
  );
  expect(sendButton().disabled).toBe(true);
  await act(async () => {
    answers[1]?.resolve(message("sent", "review the diff"));
  });
  expect(sendButton().disabled).toBe(true);

  await view.storeThread([
    {
      message: message("switch-1", "/model anthropic/model-b"),
      replies: [sessionReply("switch-1", "Model set to anthropic/model-b.")],
    },
  ]);
  await waitFor(() =>
    expect(screen.getByTestId("agent-switch-status").textContent).toBe(
      "The session answered: Model set to anthropic/model-b."
    )
  );
  expect(sendButton().disabled).toBe(false);
  expect(within(composer()).getByText("Enter to send · Shift+Enter for a new line")).toBeTruthy();
  await switchTo("Reviewer");
  expect(sendButton().disabled).toBe(false);
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
    putAgentState: answerAgentStates().mockResolvedValue({ unread_replies: 0 }),
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
    expect(createAgentMessage).toHaveBeenCalledWith(
      SESSION,
      { body: "are you there?", delivery: "aside" },
      keyedAttempt
    )
  );
  expect(screen.getByTestId("agent-takes-no-messages")).toBeTruthy();
});
