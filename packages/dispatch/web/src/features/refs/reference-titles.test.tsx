import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import type { AskRead, IssueReferenceSummary, MessageRead } from "../../api/types";
import { MarkdownBody } from "./MarkdownBody";

const ASK_ID = "2a7f1c90-1f4e-4a2b-9c3d-5e6f70819a2b";

const summary: IssueReferenceSummary = {
  documents: [],
  key: "CORE-1",
  project: "CORE",
  status: "in_progress",
  title: "Fix the thing",
};

function askRead(id: string, question: string): AskRead {
  return {
    answers: [],
    ask: {
      anchor: null,
      answer: null,
      author: { id: "planner", kind: "session" },
      created_at: "2026-01-01T00:00:00Z",
      edited_at: null,
      id,
      issue_key: "CORE-1",
      kind: "question",
      multiple: false,
      opened_event_id: 1,
      options: [],
      question,
      state: "open",
      urgency: "med",
    },
    claimant: null,
    edits: [],
    followers: [],
    replies: [],
    route: null,
    route_holder: null,
    route_status: null,
    route_status_reason: null,
  };
}

function withQueries(queryClient: QueryClient, children: ReactNode): ReactNode {
  return (
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>{children}</MemoryRouter>
    </QueryClientProvider>
  );
}

function freshClient(): QueryClient {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } });
}

const spies: { mockRestore(): void }[] = [];

afterEach(() => {
  for (const spy of spies.splice(0)) {
    spy.mockRestore();
  }
});

test("an issue reference reads as its key then its title", async () => {
  const queryClient = freshClient();
  queryClient.setQueryData(["issue-summary", "CORE-1"], summary);
  const view = render(
    withQueries(queryClient, <MarkdownBody markdown="Tied to dispatch://CORE-1 here." />)
  );

  try {
    const link = await within(view.container).findByRole("link", { name: "CORE-1 Fix the thing" });
    expect(link.getAttribute("href")).toBe("/issues/CORE-1");
  } finally {
    view.unmount();
  }
});

test("a message reference reads as the headline of its body, as a comment's does", async () => {
  const message: MessageRead = {
    message: {
      author: { id: "planner", kind: "session" },
      body: "## Shipped it\n\nThe deploy went out at noon.",
      created_at: "2026-01-01T00:00:00Z",
      deliveries: [],
      id: "4c9f3e12-3b60-4c4d-9e5f-70819a2b3c4d",
      in_reply_to: null,
      issue_key: "CORE-1",
    },
    replies: [],
  } as unknown as MessageRead;
  spies.push(spyOn(api, "getMessage").mockResolvedValue(message));
  const view = render(
    withQueries(
      freshClient(),
      <MarkdownBody markdown="see dispatch://CORE-1/message/4c9f3e12-3b60-4c4d-9e5f-70819a2b3c4d" />
    )
  );

  try {
    const link = await within(view.container).findByRole("link");
    // The author is no title: the headline of what the message says is.
    await waitFor(() => expect(link.textContent).toBe("Shipped it The deploy went out at noon."));
  } finally {
    view.unmount();
  }
});

test("a reference whose target is missing reads as the words its author wrote", async () => {
  spies.push(
    spyOn(api, "getAsk").mockRejectedValue(
      new ApiError(404, { code: "NOT_FOUND", error: "no ask" })
    )
  );
  const written = `dispatch://CORE-1/ask/${ASK_ID}`;
  const view = render(withQueries(freshClient(), <MarkdownBody markdown={`see ${written}`} />));

  try {
    const link = await within(view.container).findByRole("link", { name: written });
    // Never the short form `CORE-1 ask`: a target that never resolves reads as the page's words.
    await waitFor(() => expect(link.textContent).toBe(written));
  } finally {
    view.unmount();
  }
});

test("a reference whose id is a prefix reads the item under the whole id, caching nothing under the prefix", async () => {
  const whole = `dispatch://CORE-1/ask/${ASK_ID}`;
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: { id: ASK_ID, kind: "ask", ref: whole },
  });
  const getAsk = spyOn(api, "getAsk").mockResolvedValue(askRead(ASK_ID, "Ship it?"));
  spies.push(getReferences, getAsk);
  const queryClient = freshClient();
  const view = render(
    withQueries(queryClient, <MarkdownBody markdown="See dispatch://CORE-1/ask/2a7f1c90 here." />)
  );

  try {
    // The link appears once the prefix resolves, and the paint that draws it replaces the
    // element, so the anchor is read from the container each time rather than held.
    const link = () => view.container.querySelector("a");
    await waitFor(() => expect(link()?.textContent).toBe("Ship it?"));
    expect(link()?.getAttribute("data-dispatch-ref")).toBe(whole);
    expect(getReferences).toHaveBeenCalledWith("dispatch://CORE-1/ask/2a7f1c90");
    expect(getAsk).toHaveBeenCalledWith(ASK_ID);
    expect(getAsk).not.toHaveBeenCalledWith("2a7f1c90");
    // One cache entry per record, under the whole id: nothing is kept under the prefix.
    expect(queryClient.getQueryData(["ask", ASK_ID])).toBeDefined();
    expect(queryClient.getQueryData(["ask", "2a7f1c90"])).toBeUndefined();
  } finally {
    view.unmount();
  }
});
