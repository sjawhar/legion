import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Ask } from "../../api/types";
import { Inbox } from "./Inbox";

function artifactAsk(): Ask {
  return {
    anchor: null,
    answer: null,
    artifact_id: "artifact-1",
    author: { id: "session-1", kind: "session" },
    created_at: "2026-09-11T00:00:00Z",
    document: { name: "Design notes", project: "CORE", slug: "design-notes" },
    edited_at: null,
    id: "ask-1",
    issue_key: null,
    multiple: false,
    opened_event_id: 1,
    options: [],
    question: "Does this design need review?",
    state: "open",
    urgency: "med",
  };
}

test("Inbox labels an artifact-owned ask with its project and document name", async () => {
  const ask = artifactAsk();
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([ask]);
  const getAsk = spyOn(api, "getAsk").mockResolvedValue({ ask, replies: [] });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <Inbox />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByText("CORE / Design notes");
    expect(screen.queryByRole("link", { name: "CORE / Design notes" })).toBeNull();
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getInbox.mockRestore();
  }
});
