import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen } from "@testing-library/react";

import type { Event } from "../../api/types";
import { PinnedTab } from "./PinnedTab";

const messageEvent: Event = {
  actor: { id: "alice", kind: "user" },
  created_at: "2026-09-10T00:00:00Z",
  id: 1,
  issue_key: "CORE-1",
  notify: false,
  payload: {
    author: { id: "alice", kind: "user" },
    body: "Pinned message\n\n- item",
    created_at: "2026-09-10T00:00:00Z",
    deliveries: [],
    id: "message-1",
    in_reply_to: null,
    issue_key: "CORE-1",
    target: null,
  },
  seq: 1,
  type: "message.created",
};

test("PinnedTab renders pinned event bodies as Markdown", async () => {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <PinnedTab events={[messageEvent]} onUnpin={() => {}} pinnedIds={["1"]} />
    </QueryClientProvider>
  );
  try {
    expect(await screen.findByRole("list")).not.toBeNull();
    expect(screen.getByRole("listitem").textContent).toBe("item");
  } finally {
    view.unmount();
  }
});

test("a pinned lifecycle event remains removable", async () => {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const lifecycleEvent: Extract<Event, { type: "comment.resolved" }> = {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-09-10T00:01:00Z",
    id: 2,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      anchor: null,
      artifact_name: "spec",
      ask_id: null,
      author: { id: "alice", kind: "user" },
      body: "Pinned lifecycle comment",
      created_at: "2026-09-10T00:00:00Z",
      deliveries: [],
      edited_at: null,
      id: "comment-1",
      issue_key: "CORE-1",
      mentions: [],
      reply_to: null,
      resolved: true,
      resolved_at: "2026-09-10T00:01:00Z",
      resolved_by: { id: "alice", kind: "user" },
      suggestion: null,
      turn: null,
    },
    seq: 2,
    type: "comment.resolved",
  };
  const removed: number[] = [];
  const view = render(
    <QueryClientProvider client={queryClient}>
      <PinnedTab
        events={[lifecycleEvent]}
        onUnpin={(eventId) => removed.push(eventId)}
        pinnedIds={["2"]}
      />
    </QueryClientProvider>
  );

  try {
    fireEvent.click(await screen.findByRole("button", { name: "Unpin" }));
    expect(removed).toEqual([2]);
  } finally {
    view.unmount();
  }
});
