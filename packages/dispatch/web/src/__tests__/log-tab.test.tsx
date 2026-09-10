import { expect, spyOn, test } from "bun:test";
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

    const view = render(
      <QueryClientProvider client={queryClient}>
        <LogTab
          isClosed={false}
          issueKey="CORE-1"
          route={null}
          state={{ "CORE-1": issueState() }}
        />
      </QueryClientProvider>
    );
    fireEvent.click(screen.getByRole("button", { name: "Pin" }));
    fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));

    await waitFor(() =>
      expect(screen.getByRole("alert").textContent).toContain("Couldn't save pin/dismiss")
    );
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));

    await waitFor(() => expect(writes.at(-1)).toEqual(["pinned_items:event:1", "event:1"]));
    view.unmount();
  } finally {
    api.getIssueEvents = originalGetIssueEvents;
    api.getMyState = originalGetMyState;
    api.putIssueState = originalPutIssueState;
  }
});

test("a scroll event measures the reader's position in O(1) rect reads, not a scan over every article", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  const originalGetMyState = api.getMyState;
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  // Enough articles that a per-article getBoundingClientRect scan would blow well past any
  // reasonable per-scroll-event budget, while an O(1) hit-test stays flat regardless of count.
  const events: Event[] = Array.from({ length: 250 }, (_, index) => ({
    actor: { id: "alice", kind: "user" },
    created_at: "2026-09-09T00:00:00Z",
    id: index + 1,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      author: { id: "alice", kind: "user" },
      body: `Message ${index}`,
      created_at: "2026-09-09T00:00:00Z",
      id: `message-${index}`,
      issue_key: "CORE-1",
    },
    seq: index + 1,
    type: "message.created",
  }));
  const rectSpy = spyOn(Element.prototype, "getBoundingClientRect").mockReturnValue({
    bottom: 10,
    height: 10,
    left: 0,
    right: 10,
    toJSON: () => ({}),
    top: 0,
    width: 10,
    x: 0,
    y: 0,
  });
  const elementFromPointSpy = spyOn(document, "elementFromPoint").mockImplementation(() =>
    document.querySelector("[data-event-seq]")
  );

  try {
    api.getIssueEvents = async () => events;
    api.getMyState = async () => ({ "CORE-1": issueState() });
    queryClient.setQueryData<UserState>(["user-state"], { "CORE-1": issueState() });
    queryClient.setQueryData(["events", "CORE-1"], { pageParams: [null], pages: [events] });

    const view = render(
      <QueryClientProvider client={queryClient}>
        <LogTab
          isClosed={false}
          issueKey="CORE-1"
          route={null}
          state={{ "CORE-1": issueState() }}
        />
      </QueryClientProvider>
    );
    await waitFor(() => expect(screen.getByText("Message 0")).toBeDefined());

    rectSpy.mockClear();
    window.dispatchEvent(new Event("scroll"));

    expect(rectSpy.mock.calls.length).toBeLessThanOrEqual(3);
    view.unmount();
  } finally {
    api.getIssueEvents = originalGetIssueEvents;
    api.getMyState = originalGetMyState;
    rectSpy.mockRestore();
    elementFromPointSpy.mockRestore();
  }
});
