import { afterAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { UnifiedInboxPage as InboxPage } from "../../api/types";
import { UnifiedInboxPage } from "./UnifiedInboxPage";

const who = spyOn(api, "whoAmI");
const list = spyOn(api, "listUnifiedInbox");
afterAll(() => {
  who.mockRestore();
  list.mockRestore();
});

test("switching signed-in people never displays the previous person's cached inbox", async () => {
  const alice = { kind: "user" as const, login: "alice", event_head: "0" };
  who.mockResolvedValue(alice);
  const page: InboxPage = {
    person: "alice",
    total: 1,
    limit: 50,
    offset: 0,
    horizon: 0,
    expansions: [],
    rows: [
      {
        cause: "ask",
        kind: "ask",
        id: "one",
        thread_id: "one",
        created_at: "2026-11-04T09:00:00Z",
        body: "Alice private inbox item",
        author: { kind: "user", id: "alice" },
        href: "/issues/CORE-1/asks/one",
        subject: "Alice private inbox item",
        snoozed_until: null,
      },
    ],
  };
  list.mockResolvedValueOnce(page).mockImplementation(() => new Promise<InboxPage>(() => {}));
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["whoami"], alice);
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <UnifiedInboxPage />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    await waitFor(() => expect(view.getByText("Alice private inbox item")).toBeTruthy());
    await act(async () => {
      client.setQueryData(["whoami"], { kind: "user", login: "bob", event_head: "0" });
    });
    expect(view.queryByText("Alice private inbox item")).toBeNull();
  } finally {
    view.unmount();
    client.clear();
  }
});
