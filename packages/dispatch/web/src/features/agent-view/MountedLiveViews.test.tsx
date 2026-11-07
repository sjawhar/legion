import { afterEach, beforeEach, expect, type Mock, spyOn, test } from "bun:test";
import type { AgentStreamFrame } from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { Link, MemoryRouter, Route, Routes, useMatch } from "react-router-dom";

import {
  type FakeLiveSocket,
  type FakeLiveSockets,
  installFakeLiveSockets,
} from "../../__tests__/live-socket-harness";
import { answerAgentStates } from "../../__tests__/send-answers";
import { api } from "../../api/client";
import { liveSocketFor } from "../../api/live-socket";
import { userAgentStateQuery } from "../../api/queries";
import type { Agent, MessageRead, UserAgentStates } from "../../api/types";
import * as conversation from "./conversation";
import { LiveViewRoute, MountedLiveViews } from "./MountedLiveViews";
// The host loads the view's own module lazily, and the first row to mount it would otherwise
// wait out that whole module graph inside a `findBy` timeout. Loading it with this file costs
// the rows nothing and leaves the host's own `lazy` where the shell needs it.
import "./AgentConversationPage";

/**
 * The shell's host of the live views (INFRA-812, D5 to D8): what the person finds on the page as
 * they walk into a live view, out of it and into others, and what each view is still doing while
 * they are elsewhere.
 *
 * The registry's own rules - the words, the thirty-second linger, what an entry keeps - are
 * `live-views.test.ts`'s. These rows are about the page: that an address alone opens a view,
 * that leaving one does not take it away, that a third one does, and that a view the person is
 * not looking at neither names the tab nor tells Dispatch they have read anything.
 */

const PLANNER = "01a0e090-4848-7473-acc5-fc96e6a646d3";
const REVIEWER = "01a0e090-4848-7473-acc5-fc96e6a646d4";
const TESTER = "01a0e090-4848-7473-acc5-fc96e6a646d5";

/** The id of the one streamed turn whose render the failing test refuses (below). */
const BAD_TURN = "a-turn-this-build-cannot-read";

function agentOf(sessionId: string, title: string): Agent {
  return {
    capabilities: ["aside", "btw", "steer"],
    dir: "/workspaces/legion",
    host: "planner-host",
    last_activity: null,
    last_seen: Date.now(),
    machine_id: "planner-host",
    open_asks: 0,
    roles: [],
    session_id: sessionId,
    title,
  };
}

/** One of the viewer's messages to `session` and the session's reply to it, written at `at`. */
function exchange(session: string, at: string): MessageRead {
  return {
    message: {
      author: { id: "sami", kind: "user" },
      body: "Where is the dashboard?",
      created_at: "2026-09-27T21:16:15Z",
      deliveries: [],
      id: `m-${session}`,
      in_reply_to: null,
      issue_key: null,
      target: `session:${session}`,
    },
    replies: [
      {
        author: { id: session, kind: "session" },
        body: "Done: it is at /dash.",
        created_at: at,
        deliveries: [],
        id: `r-${session}-${at}`,
        in_reply_to: `m-${session}`,
        issue_key: null,
        target: `session:${session}`,
      },
    ],
  };
}

/** A turn the session streamed, by the key the transcript carries it under. */
function assistantFrame(id: string): AgentStreamFrame {
  return {
    kind: "message",
    message: {
      at: Date.parse("2026-09-27T21:16:16Z"),
      id,
      parts: [{ text: "Switching to it now.", type: "text" }],
      role: "assistant",
      streaming: false,
    },
    seq: 1,
    v: 1,
  };
}

const spies: { mockRestore: () => void }[] = [];
const clients: QueryClient[] = [];
let sockets: FakeLiveSockets;
/** The sessions Dispatch lists, and each one's stored conversation: both change under the page,
 *  as a session renaming itself or answering a message changes them. */
let registry: Agent[] = [];
let threads: Record<string, MessageRead[]> = {};

beforeEach(() => {
  sockets = installFakeLiveSockets();
  registry = [
    agentOf(PLANNER, "Planner"),
    agentOf(REVIEWER, "Reviewer"),
    agentOf(TESTER, "Tester"),
  ];
  threads = {};
});

afterEach(() => {
  cleanup();
  for (const spy of spies.splice(0)) spy.mockRestore();
  for (const client of clients.splice(0)) liveSocketFor(client).close();
  sockets.restore();
  window.__dispatchLiveViews = undefined;
});

/** Dispatch's reads, signed in as sami, and the one write these rows make: the read mark. */
function mockDispatch(agentState: UserAgentStates = {}): Mock<typeof api.putAgentState> {
  const putAgentState = answerAgentStates().mockImplementation(async (_session, input) => ({
    ...input,
    unread_replies: 0,
  }));
  spies.push(
    spyOn(api, "whoAmI").mockResolvedValue({ event_head: "0", kind: "user", login: "sami" }),
    spyOn(api, "listAgents").mockImplementation(async () => registry),
    spyOn(api, "listAgentMessages").mockImplementation(async (session) => threads[session] ?? []),
    spyOn(api, "getMyAgentState").mockResolvedValue(agentState),
    putAgentState
  );
  return putAgentState;
}

/**
 * The shell as `app.tsx` lays it out: the address names the view in front, the live route itself
 * renders nothing, and the host mounts the views beside the routes. The links are the app's own
 * ways into a live view - an `AgentName`, the Inbox, a pasted address - which all push.
 */
function Shell(): ReactNode {
  const front = useMatch("/agents/:sessionId/live")?.params.sessionId ?? null;
  return (
    <>
      <nav aria-label="Test navigation">
        <Link to="/agents">Leave for Agents</Link>
        <Link to={`/agents/${PLANNER}/live`}>Open Planner</Link>
        <Link to={`/agents/${REVIEWER}/live`}>Open Reviewer</Link>
        <Link to={`/agents/${TESTER}/live`}>Open Tester</Link>
      </nav>
      <Routes>
        <Route element={<h1>Agents</h1>} path="/agents" />
        <Route element={<LiveViewRoute />} path="/agents/:sessionId/live" />
      </Routes>
      <MountedLiveViews front={front} />
    </>
  );
}

function renderShell(at: string): QueryClient {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { gcTime: 0, retry: false, staleTime: 0 } },
  });
  clients.push(queryClient);
  render(
    <MemoryRouter initialEntries={[at]}>
      <QueryClientProvider client={queryClient}>
        <Shell />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return queryClient;
}

/** The sessions whose views are on the page, in the order the host renders them. */
function mountedSessions(): string[] {
  return screen
    .queryAllByTestId("agent-live-view")
    .map((view) => view.getAttribute("data-session-id") ?? "");
}

function viewOf(sessionId: string): HTMLElement {
  const view = screen
    .queryAllByTestId("agent-live-view")
    .find((candidate) => candidate.getAttribute("data-session-id") === sessionId);
  if (view === undefined) throw new Error(`no live view of ${sessionId} is on the page`);
  return view;
}

/** The tab's live socket, opened as the browser opens it. */
function openSocket(): FakeLiveSocket {
  const socket = sockets.latest();
  act(() => socket.open());
  return socket;
}

/** The sessions the page asked to watch, in order. */
function watched(socket: FakeLiveSocket): string[] {
  return socket
    .subscribes()
    .flatMap((frame) => (frame.view.kind === "agent" ? [frame.view.session_id] : []));
}

/** The sessions the page stopped watching, in order. */
function dropped(socket: FakeLiveSocket): string[] {
  const sessions = new Map<string, string>();
  for (const frame of socket.subscribes()) {
    if (frame.view.kind === "agent") sessions.set(frame.sub, frame.view.session_id);
  }
  return socket.unsubscribes().flatMap((frame) => {
    const session = sessions.get(frame.sub);
    return session === undefined ? [] : [session];
  });
}

/** The session's own answer to its watcher: its replay, which the view draws its transcript
 *  from. */
function replay(socket: FakeLiveSocket, session: string, frames: readonly AgentStreamFrame[]) {
  const sent = socket
    .subscribes()
    .filter((frame) => frame.view.kind === "agent" && frame.view.session_id === session)
    .at(-1);
  if (sent === undefined) throw new Error(`no live view of ${session} was subscribed`);
  act(() => {
    socket.receive({
      type: "agent",
      sub: sent.sub,
      event: "replay",
      data: { frames, session_id: session, v: 1 },
    });
  });
}

/** Every read mark the page sent, as the session it was for and how far it read. */
function readMarks(putAgentState: Mock<typeof api.putAgentState>): [string, string | undefined][] {
  return putAgentState.mock.calls.map(([session, input]) => [session, input.read_through]);
}

// A live view is reachable from every AgentName in the app, from the Inbox, from an issue page
// and from a pasted address (D8), so the address alone opens it. The route's own element renders
// nothing: without the host, every one of those links would land on an empty page.
test("a live address opens that session's view, in front of everything else", async () => {
  mockDispatch();
  renderShell(`/agents/${PLANNER}/live`);

  await screen.findByTestId("agent-conversation");
  const view = viewOf(PLANNER);
  expect(view.getAttribute("data-front")).toBe("true");
  expect(view.getAttribute("aria-hidden")).toBeNull();
  await waitFor(() => expect(document.title).toContain("Planner"));
  expect(watched(openSocket())).toEqual([PLANNER]);
});

// The view the person leaves lingers: it keeps its subscription and the conversation it has
// drawn, which is what makes coming back to it immediate, and the page they are on is the only
// one they see or a screen reader reads.
test("leaving a live view hides it but keeps it watching, and coming back costs no new watch", async () => {
  mockDispatch();
  renderShell(`/agents/${PLANNER}/live`);
  await screen.findByTestId("agent-conversation");
  const socket = openSocket();

  fireEvent.click(screen.getByRole("link", { name: "Leave for Agents" }));
  await screen.findByRole("heading", { name: "Agents" });

  const lingering = viewOf(PLANNER);
  expect(lingering.getAttribute("data-front")).toBe("false");
  expect(lingering.getAttribute("aria-hidden")).toBe("true");
  expect(within(lingering).getByTestId("agent-conversation")).toBeTruthy();
  expect(dropped(socket)).toEqual([]);

  fireEvent.click(screen.getByRole("link", { name: "Open Planner" }));
  await waitFor(() => expect(viewOf(PLANNER).getAttribute("data-front")).toBe("true"));
  expect(watched(socket)).toEqual([PLANNER]);
});

// At most two views are mounted (D6): the one in front and the one lingering. A third opens over
// the first, which then holds nothing at all - no conversation on the page and no session on the
// wire - which is what keeps eight pinned agents from costing eight subscriptions.
test("opening a third live view puts the first to sleep, where it renders nothing", async () => {
  mockDispatch();
  renderShell(`/agents/${PLANNER}/live`);
  await screen.findByTestId("agent-conversation");
  const socket = openSocket();

  fireEvent.click(screen.getByRole("link", { name: "Open Reviewer" }));
  await waitFor(() => expect(mountedSessions()).toEqual([PLANNER, REVIEWER]));

  fireEvent.click(screen.getByRole("link", { name: "Open Tester" }));
  await waitFor(() => expect(mountedSessions()).toEqual([REVIEWER, TESTER]));
  expect(screen.queryAllByTestId("agent-conversation")).toHaveLength(2);
  await waitFor(() => expect(dropped(socket)).toEqual([PLANNER]));
  expect(watched(socket)).toEqual([PLANNER, REVIEWER, TESTER]);
});

// Seeing a session's replies is reading them, and nobody is seeing the view they left. A
// lingering view that marked its session's replies read would clear a badge the person never
// looked at, for as long as the view lingers.
test("only the view in front marks the session's replies read", async () => {
  threads[PLANNER] = [exchange(PLANNER, "2026-09-27T21:20:28Z")];
  threads[REVIEWER] = [exchange(REVIEWER, "2026-09-27T21:20:28Z")];
  const putAgentState = mockDispatch({
    [PLANNER]: { unread_replies: 1 },
    [REVIEWER]: { unread_replies: 1 },
  });
  const queryClient = renderShell(`/agents/${PLANNER}/live`);
  await waitFor(() =>
    expect(readMarks(putAgentState)).toEqual([[PLANNER, "2026-09-27T21:20:28Z"]])
  );

  fireEvent.click(screen.getByRole("link", { name: "Open Reviewer" }));
  await waitFor(() =>
    expect(readMarks(putAgentState)).toContainEqual([REVIEWER, "2026-09-27T21:20:28Z"])
  );

  // Both sessions reply again. The lingering view reads its session as ever and has the reply on
  // the page it holds, but the person is not looking at it.
  threads[PLANNER] = [exchange(PLANNER, "2026-09-27T21:31:00Z")];
  threads[REVIEWER] = [exchange(REVIEWER, "2026-09-27T21:31:00Z")];
  await act(async () => {
    queryClient.setQueryData<UserAgentStates>(userAgentStateQuery().queryKey, {
      [PLANNER]: { unread_replies: 1 },
      [REVIEWER]: { unread_replies: 1 },
    });
    await queryClient.invalidateQueries({ queryKey: ["agents"] });
  });

  await waitFor(() =>
    expect(readMarks(putAgentState)).toContainEqual([REVIEWER, "2026-09-27T21:31:00Z"])
  );
  expect(readMarks(putAgentState)).not.toContainEqual([PLANNER, "2026-09-27T21:31:00Z"]);
});

// The tab's name is the page the person is looking at. A session that renames itself while its
// view lingers - which is what a session does as it takes up new work - must not take the name
// of the tab from the view in front of it.
test("a lingering view leaves the tab's name to the view in front", async () => {
  mockDispatch();
  const queryClient = renderShell(`/agents/${PLANNER}/live`);
  await screen.findByTestId("agent-conversation");
  await waitFor(() => expect(document.title).toContain("Planner"));

  fireEvent.click(screen.getByRole("link", { name: "Open Reviewer" }));
  await waitFor(() => expect(document.title).toContain("Reviewer"));

  registry = registry.map((agent) =>
    agent.session_id === PLANNER ? { ...agent, title: "Planner on INFRA-9" } : agent
  );
  await act(async () => {
    await queryClient.invalidateQueries({ queryKey: ["agents"] });
  });

  await waitFor(() => expect(within(viewOf(PLANNER)).getByText(/Planner on INFRA-9/)).toBeTruthy());
  expect(document.title).toContain("Reviewer");
  expect(document.title).not.toContain("INFRA-9");
});

// Each view renders inside a boundary of its own, keyed on its session: a session whose frames
// this build cannot render costs the reader that session's view, and neither the view behind it
// - which holds a draft and a reading place - nor the shell around them.
test("a view that throws takes itself down and leaves the other view and the shell standing", async () => {
  mockDispatch();
  const settingsView = conversation.sessionSettingsView;
  spies.push(
    spyOn(conversation, "sessionSettingsView").mockImplementation((state, responding, replays) => {
      if (state.messages.some((turn) => turn.id === BAD_TURN)) {
        throw new Error("a frame this build cannot read");
      }
      return settingsView(state, responding, replays);
    })
  );
  renderShell(`/agents/${PLANNER}/live`);
  await screen.findByTestId("agent-conversation");
  const socket = openSocket();

  fireEvent.click(screen.getByRole("link", { name: "Open Reviewer" }));
  await waitFor(() => expect(mountedSessions()).toEqual([PLANNER, REVIEWER]));
  replay(socket, REVIEWER, [assistantFrame(BAD_TURN)]);

  await waitFor(() =>
    expect(
      within(viewOf(REVIEWER)).getByText(/Something went wrong in this live view/)
    ).toBeTruthy()
  );
  expect(within(viewOf(REVIEWER)).queryByTestId("agent-conversation")).toBeNull();
  expect(within(viewOf(PLANNER)).getByTestId("agent-conversation")).toBeTruthy();
  expect(screen.getByRole("link", { name: "Open Tester" })).toBeTruthy();
});
