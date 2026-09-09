import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";

import { api } from "../api/client";
import type { Event, UserIssueState, UserState } from "../api/types";
import { LogTab } from "../features/issue/LogTab";

function event(): Event {
  return {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-09-09T00:00:00Z",
    id: 1,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      author: { id: "alice", kind: "user" },
      body: "A message",
      created_at: "2026-09-09T00:00:00Z",
      id: "message-1",
      issue_key: "CORE-1",
    },
    seq: 1,
    type: "message.created",
  };
}

function issueState(dismissed: string[] = []): UserIssueState {
  return { dismissed, last_read_seq: 1, pinned: false };
}

test("retry replays every operation rejected by the state-write queue", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  const originalGetMyState = api.getMyState;
  const originalPutIssueState = api.putIssueState;
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const writes: string[][] = [];
  let failures = 2;

  try {
    api.getIssueEvents = async () => [event()];
    api.getMyState = async () => ({ "CORE-1": issueState() });
    api.putIssueState = async (_issueKey, input) => {
      const dismissed = input.dismissed ?? [];
      writes.push(dismissed);
      if (failures > 0) {
        failures -= 1;
        throw new Error("offline");
      }
      return issueState(dismissed);
    };
    queryClient.setQueryData<UserState>(["user-state"], { "CORE-1": issueState() });
    queryClient.setQueryData(["events", "CORE-1"], {
      pageParams: [null],
      pages: [[event()]],
    });

    render(
      <QueryClientProvider client={queryClient}>
        <LogTab issueKey="CORE-1" state={{ "CORE-1": issueState() }} />
      </QueryClientProvider>
    );
    fireEvent.click(screen.getByRole("button", { name: "Pin" }));
    fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));

    await waitFor(() =>
      expect(screen.getByRole("alert").textContent).toContain("Couldn't save pin/dismiss")
    );
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));

    await waitFor(() => expect(writes.at(-1)).toEqual(["pinned_items:event:1", "event:1"]));
  } finally {
    api.getIssueEvents = originalGetIssueEvents;
    api.getMyState = originalGetMyState;
    api.putIssueState = originalPutIssueState;
  }
});
