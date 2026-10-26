import { expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import type { CredentialPendingRow, InboxRow } from "../../api/types";
import { BlockedOnYou } from "./BlockedOnYou";

function request(record_id: string, requested_at: string): CredentialPendingRow {
  return { identifiers: ["DEMO_API_KEY"], kind: "agent_secret", record_id, requested_at };
}

function ask(id: string, overrides: Partial<InboxRow> = {}): InboxRow {
  return {
    anchor: null,
    answer: null,
    author: { id: "session-1", kind: "session" },
    created_at: "2026-09-11T00:00:00Z",
    edited_at: null,
    id,
    issue: { assignee: "alice", key: "CORE-1", title: "Fix the thing" },
    issue_key: "CORE-1",
    kind: "question",
    multiple: false,
    opened_event_id: 1,
    options: [],
    thread: { answers: [], claimant: null, edits: [], followers: [], replies: [], route: null },
    priority: null,
    snoozed_until: null,
    question: "Which approach?",
    state: "open",
    waiting_on: "human",
    urgency: "med",
    ...overrides,
  };
}

// The broker's `requested_at` reaches the SPA verbatim, and the count this rule gives also drives
// the shell's `Needs you N` badges, outside every page's error boundary.
test("a request whose time cannot be read still counts, and the banner's age comes from the rest", () => {
  const unreadable = request("record-1", "yesterday");
  const threeHoursAgo = request(
    "record-2",
    new Date(Date.now() - 3 * 60 * 60 * 1000).toISOString()
  );

  const view = render(
    <MemoryRouter>
      <BlockedOnYou asks={[]} credentialRequests={[unreadable, threeHoursAgo]} viewer="alice" />
    </MemoryRouter>
  );
  try {
    expect(screen.getByRole("link").textContent).toBe("Blocked on you: 2 items, oldest 3h");

    view.rerender(
      <MemoryRouter>
        <BlockedOnYou asks={[]} credentialRequests={[unreadable]} viewer="alice" />
      </MemoryRouter>
    );
    expect(screen.getByRole("link").textContent).toBe("Blocked on you: 1 item");
  } finally {
    view.unmount();
  }
});

// One meaning for every count of what waits on the reader: whose turn it is AND whose issue it
// is, whatever the list on screen is filtered to. A credential request is nobody's issue and
// anyone signed in may decide one, so it is counted whether or not the login has resolved.
test("the banner counts the reader's own questions, and no question before the login resolves", () => {
  const asks = [
    ask("ask-mine"),
    ask("ask-bob", { issue: { assignee: "bob", key: "CORE-2", title: "Bob's" } }),
    ask("ask-nobody", { issue: { assignee: null, key: "CORE-3", title: "Nobody's" } }),
    // A document question belongs to no issue, so it is nobody's.
    ask("ask-doc", { issue: undefined, issue_key: null }),
    // Waiting on the agent, on the reader's own issue: not the reader's move.
    ask("ask-agent", { waiting_on: "agent" }),
  ];

  const loading = render(
    <MemoryRouter>
      <BlockedOnYou
        asks={asks}
        credentialRequests={[request("record-1", "yesterday")]}
        viewer={undefined}
      />
    </MemoryRouter>
  );
  try {
    // No question is the reader's yet; the credential request waiting on them still counts.
    expect(screen.getByRole("link").textContent).toContain("Blocked on you: 1 item");
  } finally {
    loading.unmount();
  }

  const view = render(
    <MemoryRouter>
      <BlockedOnYou asks={asks} viewer="alice" />
    </MemoryRouter>
  );
  try {
    expect(screen.getByRole("link").textContent).toContain("Blocked on you: 1 item");
  } finally {
    view.unmount();
  }
});
