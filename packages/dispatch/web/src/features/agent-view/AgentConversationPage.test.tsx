import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { api } from "../../api/client";
import * as live from "../../api/live";
import type { Agent, Message, MessageRead } from "../../api/types";
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

// A human's direct message and the session's replies to it are stored in Dispatch, not in the
// relayed stream: the session answers through a tool call, which the stream shows only as that
// call. The live view is where the human talks to the session, so it has to show those replies,
// and seeing them there is reading them.
test("the live view shows the session's Dispatch replies to direct messages and marks them read", async () => {
  spies.push(
    spyOn(api, "whoAmI").mockResolvedValue({ kind: "user", login: "sami" }),
    spyOn(api, "listAgents").mockResolvedValue([agent]),
    // The stream never opens: the replies come from Dispatch, whatever the relay does.
    spyOn(live, "readEventStream").mockReturnValue(new Promise<void>(() => undefined)),
    spyOn(api, "listAgentMessages").mockResolvedValue(conversation),
    spyOn(api, "getMyAgentState").mockResolvedValue({ [SESSION]: { unread_replies: 2 } })
  );
  const putAgentState = spyOn(api, "putAgentState").mockImplementation(async (_session, input) => ({
    ...input,
    unread_replies: 0,
  }));
  spies.push(putAgentState);
  const queryClient = new QueryClient({
    defaultOptions: { queries: { gcTime: 0, retry: false, staleTime: 0 } },
  });
  render(
    <MemoryRouter initialEntries={[`/agents/${SESSION}/live`]}>
      <QueryClientProvider client={queryClient}>
        <Routes>
          <Route element={<AgentConversationPage />} path="/agents/:sessionId/live" />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>
  );

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
});

// A read mark that fails to save is sent again, so one failed request does not leave the badge
// up until the session replies once more.
test("the live view retries a read mark that failed to save", async () => {
  spies.push(
    spyOn(api, "whoAmI").mockResolvedValue({ kind: "user", login: "sami" }),
    spyOn(api, "listAgents").mockResolvedValue([agent]),
    spyOn(live, "readEventStream").mockReturnValue(new Promise<void>(() => undefined)),
    spyOn(api, "listAgentMessages").mockResolvedValue(conversation),
    spyOn(api, "getMyAgentState").mockResolvedValue({ [SESSION]: { unread_replies: 2 } })
  );
  const putAgentState = spyOn(api, "putAgentState")
    .mockRejectedValueOnce(new Error("Dispatch is restarting"))
    .mockImplementation(async (_session, input) => ({ ...input, unread_replies: 0 }));
  spies.push(putAgentState);
  const queryClient = new QueryClient({
    defaultOptions: { queries: { gcTime: 0, retry: false, staleTime: 0 } },
  });
  render(
    <MemoryRouter initialEntries={[`/agents/${SESSION}/live`]}>
      <QueryClientProvider client={queryClient}>
        <Routes>
          <Route element={<AgentConversationPage />} path="/agents/:sessionId/live" />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>
  );

  await waitFor(() => expect(putAgentState).toHaveBeenCalledTimes(2), { timeout: 4000 });
  await waitFor(() =>
    expect(
      queryClient.getQueryData<Record<string, { unread_replies: number }>>(["user-agent-state"])
    ).toMatchObject({ [SESSION]: { unread_replies: 0 } })
  );
});

// A session's stored conversation holds every message sent to it: the viewer's direct messages,
// other humans' direct messages, and issue messages targeted at it. Only the viewer's own read as
// "you"; every other one names who wrote it, and an issue message names its issue.
test("the live view attributes other people's messages to the session to their authors", async () => {
  spies.push(
    spyOn(api, "whoAmI").mockResolvedValue({ kind: "user", login: "sami" }),
    spyOn(api, "listAgents").mockResolvedValue([agent]),
    spyOn(live, "readEventStream").mockReturnValue(new Promise<void>(() => undefined)),
    spyOn(api, "listAgentMessages").mockResolvedValue([
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
    ]),
    spyOn(api, "getMyAgentState").mockResolvedValue({}),
    spyOn(api, "putAgentState").mockResolvedValue({ unread_replies: 0 })
  );
  const queryClient = new QueryClient({
    defaultOptions: { queries: { gcTime: 0, retry: false, staleTime: 0 } },
  });
  render(
    <MemoryRouter initialEntries={[`/agents/${SESSION}/live`]}>
      <QueryClientProvider client={queryClient}>
        <Routes>
          <Route element={<AgentConversationPage />} path="/agents/:sessionId/live" />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>
  );

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
