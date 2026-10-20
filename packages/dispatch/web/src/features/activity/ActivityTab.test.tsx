import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api, type ListEventsOptions } from "../../api/client";
import type { Actor, Agent, Event, UserState } from "../../api/types";
import { ActivityTab } from "./ActivityTab";

function message(seq: number, by?: Actor): Event {
  const actor = by ?? { id: "alice", kind: "user" as const };
  return {
    actor,
    created_at: "2026-09-09T00:00:00Z",
    id: seq,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      author: actor,
      body: `Message ${seq}`,
      created_at: "2026-09-09T00:00:00Z",
      deliveries: [],
      id: `message-${seq}`,
      in_reply_to: null,
      issue_key: "CORE-1",
      target: null,
    },
    seq,
    type: "message.created",
  };
}

function renderTab(state: UserState) {
  return render(
    <MemoryRouter>
      <QueryClientProvider
        client={
          new QueryClient({
            defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
          })
        }
      >
        <ActivityTab issueKey="CORE-1" state={state} visible />
      </QueryClientProvider>
    </MemoryRouter>
  );
}

// The Activity tab's list is the issue's whole history and nothing else: the Conversation's
// `ol[aria-label="Conversation turns"]` is a different list on the same page. The watermark it
// draws is read-only here - only the Conversation writes `last_read_seq` - so the tab must not
// put one, however long the reader dwells on it.
test("ActivityTab lists the issue's events under its own label and writes no read mark", async () => {
  const getIssueEvents = spyOn(api, "getIssueEvents").mockResolvedValue([
    message(3),
    message(2),
    message(1),
  ]);
  const putIssueState = spyOn(api, "putIssueState");
  const view = renderTab({
    "CORE-1": { dismissed: [], last_read_seq: 2, pinned: false, seq: 0 },
  });

  try {
    const list = await screen.findByRole("list", { name: "Issue activity" });
    expect(within(list).getAllByText("sent a message")).toHaveLength(3);
    expect(within(list).getByLabelText("New since you last read")).toBeDefined();
    expect(putIssueState).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    getIssueEvents.mockRestore();
    putIssueState.mockRestore();
  }
});

test("ActivityTab's Load older fetches the page before the oldest event it holds", async () => {
  const firstPage = Array.from({ length: 200 }, (_, index) => message(200 - index));
  const calls: ListEventsOptions[] = [];
  const getIssueEvents = spyOn(api, "getIssueEvents").mockImplementation(async (_key, options) => {
    calls.push(options ?? {});
    return options?.before === undefined ? firstPage : [message(0)];
  });
  const view = renderTab({});

  try {
    await screen.findByRole("list", { name: "Issue activity" });
    fireEvent.click(await screen.findByRole("button", { name: "Load older" }));

    await waitFor(() => expect(calls).toHaveLength(2));
    expect(calls[1]).toEqual({ before: 1, limit: 200 });
    await waitFor(() =>
      expect(
        within(screen.getByRole("list", { name: "Issue activity" })).getAllByText("sent a message")
      ).toHaveLength(201)
    );
  } finally {
    view.unmount();
    getIssueEvents.mockRestore();
  }
});

/** One live session, as `GET /api/v1/agents` lists it. */
function running(sessionId: string, title: string): Agent {
  return {
    capabilities: [],
    dir: "/w",
    host: "host",
    last_activity: null,
    last_seen: 1,
    machine_id: "host",
    open_asks: 0,
    roles: [],
    session_id: sessionId,
    title,
  };
}

// Every row names its author, and a session's name is its live registry title. The tab read the
// registry only for a history that held a claim, so a row a session wrote on an issue nobody had
// claimed read as the raw `session:<id>` - the registry was never fetched to resolve it.
test("ActivityTab names a session author by its registered title", async () => {
  const getIssueEvents = spyOn(api, "getIssueEvents").mockResolvedValue([
    message(1, { id: "e2e-long-title-session", kind: "session" }),
  ]);
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([
    running("e2e-long-title-session", "Reviewing PR #1478"),
  ]);
  const view = renderTab({});

  try {
    const list = await screen.findByRole("list", { name: "Issue activity" });
    await waitFor(() => expect(within(list).getByText("Reviewing PR #1478")).toBeDefined());
    expect(within(list).queryByText(/^session:/)).toBeNull();
  } finally {
    view.unmount();
    getIssueEvents.mockRestore();
    listAgents.mockRestore();
  }
});
