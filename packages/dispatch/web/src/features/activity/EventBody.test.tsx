import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import type { ReactNode } from "react";

import type { Event } from "../../api/types";
import { EventBody } from "./EventBody";

/** An event's text renders through `MarkdownBody`, which reads whether a key written in it names
 *  an issue; one client per test, kept across a rerender. */
function queries(): { wrapper: (props: { children: ReactNode }) => ReactNode } {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return {
    wrapper: ({ children }) => (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    ),
  };
}

const markdownMessage: Event = {
  actor: { id: "alice", kind: "user" },
  created_at: "2026-09-10T00:00:00Z",
  id: 1,
  issue_key: "CORE-1",
  notify: true,
  payload: {
    author: { id: "alice", kind: "user" },
    body: "# Status\n\nThe **document** is [ready](https://example.com).",
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

test("EventBody renders Markdown event content", async () => {
  const view = render(<EventBody event={markdownMessage} />, queries());

  try {
    expect(await screen.findByRole("heading", { name: "Status" })).toBeDefined();
    expect(screen.getByText("document", { selector: "strong" })).toBeDefined();
    expect(screen.getByRole("link", { name: "ready" }).getAttribute("href")).toBe(
      "https://example.com"
    );
  } finally {
    view.unmount();
  }
});

test("EventBody renders unsupported raw HTML as literal text", async () => {
  const view = render(
    <EventBody
      event={{
        ...markdownMessage,
        payload: { ...markdownMessage.payload, body: '<img src="x">' },
      }}
    />,
    queries()
  );

  try {
    expect(await screen.findByText('<img src="x">')).not.toBeNull();
    expect(view.container.querySelector("img")).toBeNull();
  } finally {
    view.unmount();
  }
});

test("EventBody shows the answer a change replaced, struck through", async () => {
  const changed: Extract<Event, { type: "ask.answered" }> = {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-09-11T00:00:00Z",
    id: 3,
    issue_key: "CORE-1",
    notify: true,
    payload: {
      anchor: null,
      answer: { at: "2026-09-11T00:00:00Z", selected: ["Hold"], text: null, user: "alice" },
      author: { id: "session-1", kind: "session" },
      created_at: "2026-09-10T00:00:00Z",
      edited_at: null,
      id: "ask-1",
      issue_key: "CORE-1",
      kind: "question",
      multiple: false,
      opened_event_id: 1,
      options: [{ label: "Ship" }, { label: "Hold" }],
      previous_answer: {
        at: "2026-09-10T12:00:00Z",
        selected: ["Ship"],
        text: "Go now.",
        user: "alice",
      },
      question: "Publish?",
      state: "answered",
      urgency: "med",
    },
    seq: 3,
    type: "ask.answered",
  };
  const view = render(<EventBody event={changed} />, queries());

  try {
    const was = await screen.findByText("was: Ship - Go now.");
    expect(was.closest("s")).not.toBeNull();
    const { previous_answer: _dropped, ...first } = changed.payload;
    view.rerender(<EventBody event={{ ...changed, payload: first }} />);
    expect(screen.queryByText(/^was:/)).toBeNull();
  } finally {
    view.unmount();
  }
});
