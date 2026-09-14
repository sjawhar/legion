import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";

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
      <PinnedTab events={[messageEvent]} issueKey={undefined} pinnedIds={["1"]} />
    </QueryClientProvider>
  );

  try {
    expect(await screen.findByRole("list")).not.toBeNull();
    expect(screen.getByRole("listitem").textContent).toBe("item");
  } finally {
    view.unmount();
  }
});

test("PinnedTab renders a pinned ask.opened event that carries options: null (retained pre-fix block-ask events)", async () => {
  const askOpened = {
    actor: { id: "session-1", kind: "session" },
    created_at: "2026-09-10T00:00:00Z",
    id: 2,
    issue_key: "CORE-1",
    notify: true,
    payload: {
      anchor: null,
      answer: null,
      author: { id: "session-1", kind: "session" },
      created_at: "2026-09-10T00:00:00Z",
      edited_at: null,
      id: "ask-1",
      issue_key: "CORE-1",
      kind: "question",
      multiple: false,
      opened_event_id: 2,
      options: null,
      question: "Ship without options?",
      state: "open",
      urgency: "med",
    },
    seq: 2,
    type: "ask.opened",
  } as unknown as Event;
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <PinnedTab events={[askOpened]} issueKey={undefined} pinnedIds={["2"]} />
    </QueryClientProvider>
  );

  try {
    expect(await screen.findByText("Ship without options?")).not.toBeNull();
    expect(screen.getByText("Ask opened:")).not.toBeNull();
  } finally {
    view.unmount();
  }
});
