import { expect, spyOn, test } from "bun:test";
import { DELIVERY_DUPLICATE_WINDOW_MS } from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { api } from "../../api/client";
import type {
  Agent,
  BroadcastExclusion,
  BroadcastRead,
  Message,
  MessageDelivery,
  UserAgentStates,
} from "../../api/types";

import { BroadcastPage } from "./BroadcastPage";

const agents: Agent[] = [
  {
    capabilities: ["aside", "btw", "steer"],
    dir: "/workspaces/planner",
    last_activity: null,
    last_seen: Date.now(),
    machine_id: "build-host",
    open_asks: 0,
    roles: [],
    session_id: "planner-session",
    title: "Planner",
  },
];

function attempt(overrides: Partial<MessageDelivery>): MessageDelivery {
  return {
    attempt: 1,
    created_at: new Date().toISOString(),
    delivery: "steer",
    duplicate: false,
    envelope_id: null,
    error: null,
    message_id: "message-1",
    reply_id: null,
    session_id: "planner-session",
    state: "pending",
    ...overrides,
  };
}

function broadcast(deliveries: MessageDelivery[]): BroadcastRead {
  return {
    author: { id: "alice", kind: "user" },
    body: "Report status.",
    created_at: new Date().toISOString(),
    delivery: "steer",
    id: "broadcast-1",
    recipients: [
      {
        message: {
          author: { id: "alice", kind: "user" },
          body: "Report status.",
          created_at: new Date().toISOString(),
          deliveries,
          id: "message-1",
          in_reply_to: null,
          issue_key: null,
          target: "session:planner-session",
        },
        replies: [],
        session_id: "planner-session",
      },
    ],
  };
}

function renderBroadcast(
  read: BroadcastRead,
  state?: { excluded: readonly BroadcastExclusion[] },
  agentState: UserAgentStates = {},
  markRefused = false
) {
  const getBroadcast = spyOn(api, "getBroadcast").mockResolvedValue(read);
  const listAgents = spyOn(api, "listAgents").mockResolvedValue(agents);
  const createMessageDelivery = spyOn(api, "createMessageDelivery").mockResolvedValue(
    attempt({ attempt: 2, state: "sent" })
  );
  const getMyAgentState = spyOn(api, "getMyAgentState").mockResolvedValue(agentState);
  const putAgentState = markRefused
    ? spyOn(api, "putAgentState").mockRejectedValue(new Error("Dispatch is restarting"))
    : spyOn(api, "putAgentState").mockResolvedValue({ unread_replies: 0 });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={[{ pathname: "/agents/broadcasts/broadcast-1", state }]}>
      <QueryClientProvider client={queryClient}>
        <Routes>
          <Route element={<BroadcastPage />} path="/agents/broadcasts/:id" />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    createMessageDelivery,
    putAgentState,
    queryClient,
    restore: () => {
      putAgentState.mockRestore();
      getMyAgentState.mockRestore();
      createMessageDelivery.mockRestore();
      listAgents.mockRestore();
      getBroadcast.mockRestore();
    },
    view,
  };
}

function threadMessage(id: string, author: Message["author"], body: string): Message {
  return {
    author,
    body,
    created_at: new Date().toISOString(),
    deliveries: [],
    id,
    in_reply_to: "message-1",
    issue_key: null,
    target: null,
  };
}

/** A broadcast whose one recipient's thread holds two answers from the session and the viewer's
 *  own follow-up between them. */
function answeredBroadcast(): BroadcastRead {
  const read = broadcast([attempt({ envelope_id: "e1", state: "sent" })]);
  const [recipient] = read.recipients;
  if (recipient === undefined) throw new Error("the fixture has no recipient");
  const session = { id: "planner-session", kind: "session" } as const;
  return {
    ...read,
    recipients: [
      {
        ...recipient,
        replies: [
          threadMessage("answer-1", session, "Standing down."),
          threadMessage("follow-up", { id: "alice", kind: "user" }, "And the build?"),
          threadMessage("answer-2", session, "Green."),
        ],
      },
    ],
  };
}

// The page shows a recipient's whole thread, which holds the viewer's own follow-ups beside the
// session's answers. The read marks the session's messages alone: the server refuses an id the
// session did not write, and one such id in the list would leave every answer on the page unread.
test("opening a broadcast marks the recipient's answers read by id, and only the session's own", async () => {
  const page = renderBroadcast(answeredBroadcast(), undefined, {
    "planner-session": { unread_replies: 2 },
  });
  try {
    await waitFor(() =>
      expect(page.putAgentState).toHaveBeenCalledWith("planner-session", {
        read_replies: ["answer-1", "answer-2"],
      })
    );
    expect(page.putAgentState).toHaveBeenCalledTimes(1);
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// A mark the server keeps refusing - a 5xx, an expired session, an old server's 400 during a
// rolling deploy - is sent once and retried twice, and then the row waits for the unread count or
// the replies it shows to change. A refetch of the broadcast renders the same replies in a new
// array, as every render does, and sends nothing more.
test("a refused mark is sent three times and then left alone, however often the row re-renders", async () => {
  const page = renderBroadcast(
    answeredBroadcast(),
    undefined,
    { "planner-session": { unread_replies: 2 } },
    true
  );
  try {
    await waitFor(() => expect(page.putAgentState).toHaveBeenCalledTimes(3), { timeout: 8000 });
    await page.queryClient.invalidateQueries({ queryKey: ["broadcast"] });
    const quiet = Promise.withResolvers<void>();
    setTimeout(quiet.resolve, 2500);
    await quiet.promise;
    expect(page.putAgentState).toHaveBeenCalledTimes(3);
  } finally {
    page.view.unmount();
    page.restore();
  }
}, 20_000);

test("a broadcast page keeps the server exclusion reason as written", async () => {
  const serverExcluded = [
    {
      reason: "does not advertise btw",
      session_id: "reviewer-session",
      title: "Reviewer",
    },
  ] satisfies BroadcastExclusion[];
  const page = renderBroadcast(broadcast([attempt({ state: "sent" })]), {
    excluded: serverExcluded,
  });

  try {
    expect(
      await screen.findByText(
        (_, element) =>
          element?.textContent ===
          "Excluded: Reviewer (does not advertise btw). Nothing was sent to them."
      )
    ).toBeTruthy();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// LEGION-233 review, P2. A send a worker is still carrying must offer nothing: the mode-change
// buttons are a genuine second frame, and taking one while the worker was sending delivered the
// message twice.
test("a recipient whose send is still outstanding offers no retry at all", async () => {
  const page = renderBroadcast(broadcast([attempt({})]));
  try {
    const row = await screen.findByRole("article", { name: "Planner" });
    expect(within(row).getByText("Sending to Planner (Send)")).toBeTruthy();
    // textContent, never the elements: a failed toEqual on DOM nodes serialises the whole tree
    // and can take minutes to report.
    expect(
      within(row)
        .queryAllByRole("button")
        .map((button) => button.textContent)
    ).toEqual([]);
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// The other half: a process that died mid-delivery leaves a pending attempt nobody holds. Moving
// that recipient only by a delivery in a DIFFERENT mode would put a second frame wherever the
// first landed, so its own mode is the only one offered, and it is offered.
test("a pending attempt nobody is carrying offers a same-mode retry and no mode change", async () => {
  const stranded = attempt({ created_at: new Date(Date.now() - 5 * 60_000).toISOString() });
  const page = renderBroadcast(broadcast([stranded]));
  try {
    const row = await screen.findByRole("article", { name: "Planner" });
    const buttons = within(row)
      .getAllByRole("button")
      .map((button) => button.textContent);
    expect(buttons).toEqual(["Retry"]);
    expect(
      within(row).getByText(
        (_, element) =>
          element?.textContent ===
          "Nobody is carrying this send. Retry uses Send again, which cannot deliver it twice unless the session restarted since it was sent."
      )
    ).toBeTruthy();
    fireEvent.click(within(row).getByRole("button", { name: "Retry" }));
    await waitFor(() =>
      expect(page.createMessageDelivery).toHaveBeenCalledWith("message-1", "steer")
    );
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// The row's promise is the dashboard's one promise, and it expires with the same window: past it
// nothing recognises the resumed frame as a repeat (`DELIVERY_DUPLICATE_WINDOW_MS`).
test("a stranded send older than the duplicate window is not promised a single delivery", async () => {
  const aged = attempt({
    created_at: new Date(Date.now() - DELIVERY_DUPLICATE_WINDOW_MS - 60_000).toISOString(),
  });
  const page = renderBroadcast(broadcast([aged]));
  try {
    const row = await screen.findByRole("article", { name: "Planner" });
    expect(within(row).queryByText(/cannot deliver it twice/)).toBeNull();
    expect(within(row).getByText(/may deliver it twice/)).toBeTruthy();
    expect(within(row).getByRole("button", { name: "Retry" })).toBeTruthy();
  } finally {
    page.view.unmount();
    page.restore();
  }
});

// A settled failure is different: nothing is outstanding, so a mode change sends rather than
// duplicates, and the agent card's full retry row applies.
test("a failed attempt offers the same-mode retry and the mode changes", async () => {
  const failed = attempt({ error: "no live session", state: "failed" });
  const page = renderBroadcast(broadcast([failed]));
  try {
    const row = await screen.findByRole("article", { name: "Planner" });
    const buttons = within(row)
      .getAllByRole("button")
      .map((button) => button.textContent);
    expect(buttons).toEqual(["Retry", "Use BTW instead"]);
  } finally {
    page.view.unmount();
    page.restore();
  }
});

test("a delivered recipient offers nothing", async () => {
  const page = renderBroadcast(broadcast([attempt({ envelope_id: "e1", state: "sent" })]));
  try {
    const row = await screen.findByRole("article", { name: "Planner" });
    expect(within(row).getByText("Sent to Planner (Send)")).toBeTruthy();
    // textContent, never the elements: a failed toEqual on DOM nodes serialises the whole tree
    // and can take minutes to report.
    expect(
      within(row)
        .queryAllByRole("button")
        .map((button) => button.textContent)
    ).toEqual([]);
  } finally {
    page.view.unmount();
    page.restore();
  }
});
