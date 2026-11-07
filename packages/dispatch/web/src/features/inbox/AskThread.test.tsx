import { beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { api } from "../../api/client";
import type { Ask, AskRead, Comment, IssueAskRow } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { AskThread, type AskThreadQuery } from "./AskThread";

beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** The question block of CORE-1's spec, as the issue's ask list carries it: what a map built
 *  from that list would have named `#q2`, and what the reply below does not mean. */
const specQuestionAsk: IssueAskRow = {
  anchor: null,
  answer: null,
  author: { id: "planner", kind: "session" },
  block_artifact: { id: "artifact-1", primary: true, slug: "spec" },
  block_id: "q2",
  created_at: "2026-09-09T00:00:00Z",
  edited_at: null,
  id: "ask-2",
  issue_key: "CORE-1",
  kind: "question",
  last_reply: null,
  multiple: false,
  opened_event_id: 1,
  options: [],
  question: "Which way round?",
  state: "open",
  urgency: "med",
};

const answeredAsk: Ask = {
  anchor: null,
  answer: { at: "2026-09-09T01:00:00Z", selected: [], text: "Done.", user: "alice" },
  author: { id: "session-1", kind: "session" },
  created_at: "2026-09-09T00:00:00Z",
  edited_at: null,
  id: "ask-1",
  issue_key: "CORE-1",
  kind: "question",
  multiple: false,
  opened_event_id: 1,
  options: [],
  question: "Should this ship?",
  state: "answered",
  urgency: "med",
};

const reply: Comment = {
  anchor: null,
  ask_id: answeredAsk.id,
  author: { id: "alice", kind: "user" },
  body: "Holding on #q2 for now.",
  created_at: "2026-09-09T02:00:00Z",
  edited_at: null,
  id: "comment-1",
  issue_key: "CORE-1",
  reply_to: null,
  resolved: false,
  resolved_at: null,
  resolved_by: null,
  suggestion: null,
  turn: null,
  ...commentDeliveryFields(),
};

const read: AskRead = {
  answers: [],
  ask: answeredAsk,
  claimant: null,
  edits: [],
  followers: [],
  replies: [reply],
  route: null,
  route_holder: null,
  route_status: null,
  route_status_reason: null,
};

/** The ask `#q2` names in the reply, read by the reference the reply draws to title it. */
const designAsk: Ask = {
  ...answeredAsk,
  answer: null,
  id: "ask-design",
  question: "Which way round?",
  state: "open",
};

test("an Inbox reply's `#q2` names the question of the document its comment is anchored to", async () => {
  // The reply is anchored to `design.md`, which is no primary document of the issue: the server
  // scopes the comment's text to that document (`refs.ScopeOf`), so `#q2` is that document's
  // question. A map built from the issue's ask list would have chosen the spec's `q2` instead.
  const listIssueAsks = spyOn(api, "listIssueAsks").mockResolvedValue([specQuestionAsk]);
  const getAsk = spyOn(api, "getAsk").mockResolvedValue({ ...read, ask: designAsk, replies: [] });
  const getReferenceQuestions = spyOn(api, "getReferenceQuestions").mockResolvedValue({
    document: "artifact-design",
    questions: [
      { ask: "ask-design", block_id: "q2", reference: "dispatch://CORE-1/ask/ask-design" },
    ],
  });
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const thread = { data: read } as unknown as AskThreadQuery;
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <AskThread ask={answeredAsk} order="oldest" showComposer={false} thread={thread} />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const linked = () =>
      [...view.container.querySelectorAll("[data-reply] [data-dispatch-ref]")].map((anchor) =>
        anchor.getAttribute("data-dispatch-ref")
      );
    await waitFor(() => expect(linked()).toEqual(["dispatch://CORE-1/ask/ask-design"]));
    expect(getReferenceQuestions).toHaveBeenCalledWith("comment", "comment-1");
    // One map: the page reads the server's, never the issue's whole ask list.
    expect(listIssueAsks).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getReferenceQuestions.mockRestore();
    listIssueAsks.mockRestore();
  }
});
