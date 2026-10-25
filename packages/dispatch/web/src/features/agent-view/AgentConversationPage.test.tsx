import { afterEach, beforeEach, expect, jest, type Mock, spyOn, test } from "bun:test";
import {
  type AgentStreamCommand,
  type AgentStreamFrame,
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
import { MemoryRouter, Route, Routes } from "react-router-dom";

import {
  errorFrame,
  type FakeLiveSocket,
  type FakeLiveSockets,
  installFakeLiveSockets,
} from "../../__tests__/live-socket-harness";
import { ApiError, api } from "../../api/client";
import { liveSocketFor } from "../../api/live-socket";
import type { Agent, Message, MessageRead, UserAgentStates } from "../../api/types";
import { KEYED_SEND_ATTEMPT_MS, SEND_DEADLINE_MS } from "../conversation/send-request";
import { Sidebar } from "../sidebar/Sidebar";
import { AgentConversationPage } from "./AgentConversationPage";

/** What each attempt of the live view's send is held to, beside the message it carries: its own
 *  deadline, and nothing that would cut it short once it is out. */
const keyedAttempt = { deadlineMs: KEYED_SEND_ATTEMPT_MS };

const SESSION = "01a0e090-4848-7473-acc5-fc96e6a646d3";

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
  messages: MessageRead[];
  putAgentState: Mock<typeof api.putAgentState>;
}): void {
  spies.push(
    spyOn(api, "whoAmI").mockResolvedValue({ kind: "user", login: "sami" }),
    spyOn(api, "listAgents").mockResolvedValue(agents),
    spyOn(api, "listAgentMessages").mockResolvedValue(messages),
    spyOn(api, "getMyAgentState").mockResolvedValue(agentState),
    spyOn(api, "getInbox").mockResolvedValue([]),
    spyOn(api, "listIssues").mockResolvedValue([]),
    spyOn(api, "listProjects").mockResolvedValue([]),
    putAgentState
  );
}

/** Session `session`'s live view on `queryClient`, beside the navigation when `sidebar`. */
function renderPage(queryClient: QueryClient, session: string, sidebar = false): RenderResult {
  return render(
    <MemoryRouter initialEntries={[`/agents/${session}/live`]}>
      <QueryClientProvider client={queryClient}>
        {sidebar ? <Sidebar user={{ kind: "user", login: "sami" }} /> : null}
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
  session = SESSION
): void {
  const sub = agentSubscription(socket, session);
  act(() => {
    socket.receive({ type: "agent", sub, event: "responder", data: { responding: true, v: 1 } });
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
 * badge, as the app lays them out. With `replay` the tab's socket opens and Dispatch answers the
 * view with the session's replay; with none the socket never opens, so everything shown comes from
 * Dispatch's stored messages.
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
  putAgentState: Mock<typeof api.putAgentState>;
  replay?: AgentStreamFrame[];
}): QueryClient {
  mockDispatch({ agentState, agents: [{ ...agent, capabilities }], messages, putAgentState });
  const queryClient = newClient();
  renderPage(queryClient, SESSION, true);
  if (replay !== undefined) {
    const socket = sockets.latest();
    act(() => socket.open());
    answer(socket, replay);
  }
  return queryClient;
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
    expect(createAgentMessage).toHaveBeenCalledWith(
      SESSION,
      { body: "try the other branch", delivery: "steer", idempotency_key: expect.any(String) },
      keyedAttempt
    )
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
    .mockRejectedValueOnce(new TypeError("Failed to fetch"))
    .mockRejectedValueOnce(new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" }))
    .mockImplementation(async (_session, input) => message("sent", input.body));
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
  const [first, ...later] = createAgentMessage.mock.calls.map(([, input]) => input.idempotency_key);
  expect(first).toMatch(/^[0-9a-f-]{36}$/);
  expect(later.slice(0, 2)).toEqual([first, first]);
  expect(later[2]).not.toBe(first);
});

// A send's retries are the view's: leaving the live view stops them between attempts. The
// attempt out when the reader leaves may already have reached Dispatch, so it is let finish, and
// nothing tries again for a view no longer on screen.
test("leaving the live view stops its send's retries, letting the attempt out finish", async () => {
  const inFlight = Promise.withResolvers<Message>();
  const createAgentMessage = spyOn(api, "createAgentMessage")
    .mockRejectedValueOnce(new TypeError("Failed to fetch"))
    .mockReturnValueOnce(inFlight.promise)
    .mockRejectedValue(new TypeError("Failed to fetch"));
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

  await onFakeClock(async () => {
    await sendFromLiveView("try the other branch");
    await tick(1_000);
    expect(createAgentMessage).toHaveBeenCalledTimes(2);
    expect(createAgentMessage.mock.calls[1]?.[2]).toEqual(keyedAttempt);

    cleanup();
    inFlight.reject(new TypeError("Failed to fetch"));
    await tick(10 * 60_000);
    expect(createAgentMessage).toHaveBeenCalledTimes(2);
  });
});

/** Moves the fake clock `ms` on and lets every promise the move released run. */
async function tick(ms: number): Promise<void> {
  await act(async () => {
    jest.advanceTimersByTime(ms);
    for (let turn = 0; turn < 20; turn += 1) await Promise.resolve();
  });
}

/** The live view's Send. */
function sendButton(): HTMLElement {
  return within(screen.getByTestId("agent-composer")).getByRole("button", { name: "Send" });
}

const stillTrying =
  "Still sending — Dispatch has not confirmed it yet. It keeps trying, and the message is delivered once.";

/** The live view for SESSION, its mode settled on Send. */
async function renderSendingView(): Promise<void> {
  renderLiveView({
    agentState: {},
    messages: [],
    putAgentState: spyOn(api, "putAgentState").mockResolvedValue({ unread_replies: 0 }),
  });
  const mode = (await screen.findByRole("combobox", {
    name: "Delivery mode",
  })) as HTMLSelectElement;
  await waitFor(() => expect(mode.value).toBe("steer"));
}

// A pressed message is held on screen until Dispatch confirms it. Send waits for it, saying why,
// so the reader drafts the next message rather than sending a second one beside it - which would
// carry a key of its own and reach the session twice - and past the send's deadline it says the
// message is still being tried.
test("the live view holds a pressed message until Dispatch confirms it, and Send waits for it", async () => {
  const answer = Promise.withResolvers<Message>();
  const createAgentMessage = spyOn(api, "createAgentMessage").mockReturnValueOnce(answer.promise);
  spies.push(createAgentMessage);
  await renderSendingView();

  await onFakeClock(async () => {
    await sendFromLiveView("try the other branch");
    const held = screen.getByTestId("agent-send-held");
    expect(within(held).getByText("Your message: “try the other branch”")).toBeTruthy();
    expect(within(held).getByRole("status").textContent).toBe("Sending…");
    expect(sendButton().getAttribute("aria-disabled")).toBe("true");
    expect(sendButton().getAttribute("title")).toBe(
      "Dispatch has not confirmed your last message yet."
    );

    // The next message can be written, and a press sends nothing beside the first.
    const input = screen.getByPlaceholderText(/delivered as Send/) as HTMLTextAreaElement;
    fireEvent.change(input, { target: { value: "and this" } });
    fireEvent.click(sendButton());
    fireEvent.keyDown(input, { key: "Enter" });
    await tick(0);
    expect(createAgentMessage).toHaveBeenCalledTimes(1);
    expect(input.value).toContain("and this");

    await tick(SEND_DEADLINE_MS);
    expect(within(held).getByRole("status").textContent).toBe(stillTrying);
    expect(within(held).getByRole("button", { name: "Stop trying" })).toBeTruthy();

    await act(async () => {
      answer.resolve(message("sent", "try the other branch"));
    });
    await tick(0);
  });
  expect(screen.queryByTestId("agent-send-held")).toBeNull();
  expect(sendButton().getAttribute("aria-disabled")).toBeNull();
});

// Dispatch's own refusal ends the send: the message stays on screen with the reason, Retry sends
// it again under the key it was pressed with, and Discard drops it.
test("a message Dispatch refuses offers Retry under its key, and Discard", async () => {
  const refused = new ApiError(400, { code: "MESSAGE_INPUT", error: "Dispatch refused it" });
  const createAgentMessage = spyOn(api, "createAgentMessage").mockRejectedValue(refused);
  spies.push(createAgentMessage);
  await renderSendingView();

  await sendFromLiveView("try the other branch");
  const error = await screen.findByTestId("agent-send-error");
  expect(error.textContent).toBe("Could not send: Dispatch refused it");
  expect(sendButton().getAttribute("title")).toBe(
    "Retry or discard the message Dispatch did not take first."
  );

  const held = screen.getByTestId("agent-send-held");
  fireEvent.click(within(held).getByRole("button", { name: "Retry" }));
  await waitFor(() => expect(createAgentMessage).toHaveBeenCalledTimes(2));
  const [first, retried] = createAgentMessage.mock.calls.map(([, input]) => input.idempotency_key);
  expect(retried).toBe(first);

  await screen.findByTestId("agent-send-error");
  fireEvent.click(
    within(screen.getByTestId("agent-send-held")).getByRole("button", { name: "Discard" })
  );
  expect(screen.queryByTestId("agent-send-held")).toBeNull();
  expect(sendButton().getAttribute("aria-disabled")).toBeNull();
  expect(createAgentMessage).toHaveBeenCalledTimes(2);
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
    expect(screen.getByTestId("agent-send-error").textContent).toBe(
      "Dispatch has not confirmed this message. Retry sends it again under the same key, so it is delivered at most once."
    );
    await tick(30 * 60_000);
    expect(createAgentMessage).toHaveBeenCalledTimes(5);

    createAgentMessage.mockResolvedValueOnce(message("sent", "try the other branch"));
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    });
    await tick(0);
  });
  expect(createAgentMessage).toHaveBeenCalledTimes(6);
  expect(
    new Set(createAgentMessage.mock.calls.map(([, input]) => input.idempotency_key)).size
  ).toBe(1);
  expect(screen.queryByTestId("agent-send-held")).toBeNull();
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

// The header says what the relay is doing: connecting until the session's first answer arrives on
// the tab's socket, live from then on, and reconnecting while that socket is lost.
test("the live view connects, goes live on the session's answer, and reconnects with the socket", async () => {
  renderLiveView({
    agentState: {},
    messages: [],
    putAgentState: spyOn(api, "putAgentState").mockResolvedValue({ unread_replies: 0 }),
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
    putAgentState: spyOn(api, "putAgentState").mockResolvedValue({ unread_replies: 0 }),
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
    putAgentState: spyOn(api, "putAgentState").mockResolvedValue({ unread_replies: 0 }),
  });
  // The app's event feed rides the same socket, so the socket outlives the refused view.
  liveSocketFor(queryClient).subscribeEvents({
    onEvent: () => undefined,
    onStart: () => undefined,
    onSubscribed: () => undefined,
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
    messages: [],
    putAgentState: spyOn(api, "putAgentState").mockResolvedValue({ unread_replies: 0 }),
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
    expect(createAgentMessage).toHaveBeenCalledWith(
      SESSION,
      { body: "/compact", delivery: "steer", idempotency_key: expect.any(String) },
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
      { body: "/rec", delivery: "steer", idempotency_key: expect.any(String) },
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
