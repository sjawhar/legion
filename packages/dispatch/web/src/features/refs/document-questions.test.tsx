import { afterEach, beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import type { ReferenceQuestions } from "../../api/types";
import { MarkdownBody } from "./MarkdownBody";
import { warmMarkdownRenderer } from "./markdown-engine";
import type { TextSource } from "./text-references";

/**
 * A `#id` resolves against the question map the server answers for the text's own source
 * (`GET /api/v1/references/questions`), which is the map the reference graph stores its edges
 * from: `refs.ScopeOf` for the document the text belongs to, `refs.QuestionMap` for the
 * questions that document's latest version declares. The three tests below are the three ways
 * the dashboard's own map used to read `#id` differently from the server's - a copied block, a
 * block the latest version no longer declares, and a comment anchored to a document that is not
 * its issue's spec - and the last two tests are what the read costs.
 */

beforeAll(async () => {
  await warmMarkdownRenderer();
});

let restore = () => {};

afterEach(() => {
  restore();
});

/** The route spied, answering `questions` for every source, as the server answers it. */
function spyQuestions(questions: ReferenceQuestions) {
  const getReferenceQuestions = spyOn(api, "getReferenceQuestions").mockResolvedValue(questions);
  restore = () => getReferenceQuestions.mockRestore();
  return getReferenceQuestions;
}

function withQueries(
  children: ReactNode,
  queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
): ReactNode {
  return (
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>{children}</MemoryRouter>
    </QueryClientProvider>
  );
}

/** The `dispatch://` targets a rendered body links, in the order it draws them. */
function linked(container: HTMLElement): string[] {
  return [...container.querySelectorAll("a[data-dispatch-ref]")].map(
    (anchor) => anchor.getAttribute("data-dispatch-ref") ?? ""
  );
}

const documentText: TextSource = { id: "doc-1", kind: "artifact" };

test("a `#id` naming a copied question block names the ask it was copied from", async () => {
  // The server's map: `q2` is declared by this document as a copy, and the ask it carries is
  // the one it was copied from, which lives on another issue. The dashboard's own map had no
  // `copied_from` case, so it left this `#id` as plain text.
  spyQuestions({
    document: "doc-1",
    questions: [{ ask: "ask-9", block_id: "q2", reference: "dispatch://CORE-7/ask/ask-9" }],
  });
  const view = render(
    withQueries(<MarkdownBody markdown="See #q2 for the rollout." source={documentText} />)
  );

  try {
    await waitFor(() => expect(linked(view.container)).toEqual(["dispatch://CORE-7/ask/ask-9"]));
  } finally {
    view.unmount();
  }
});

test("a `#id` whose block the document's latest version no longer declares stays plain text", async () => {
  // The ask still carries `block_id: q2`, so the dashboard's own map linked it; the server
  // reads the blocks the latest version declares, which no longer holds q2, so it answers the
  // one block that is still declared and the `#id` names nothing.
  spyQuestions({
    document: "doc-1",
    questions: [{ ask: "ask-3", block_id: "q5", reference: "dispatch://CORE-1/ask/ask-3" }],
  });
  const view = render(
    withQueries(<MarkdownBody markdown="See #q2 for the rollout." source={documentText} />)
  );

  try {
    await waitFor(() => expect(view.container.textContent).toContain("See #q2 for the rollout."));
    expect(linked(view.container)).toEqual([]);
  } finally {
    view.unmount();
  }
});

test("a comment's `#id` resolves against the document it is anchored to, not its issue's spec", async () => {
  // The comment is anchored to `design.md`, so `refs.ScopeOf` scopes its text to that document
  // and `#q2` names the ask of that document's question block. The dashboard's own map chose
  // the issue's spec (`block_artifact.primary`) and named the spec's ask instead.
  const getReferenceQuestions = spyQuestions({
    document: "design-1",
    questions: [
      { ask: "ask-design", block_id: "q2", reference: "dispatch://CORE-1/ask/ask-design" },
    ],
  });
  const source: TextSource = { id: "comment-1", kind: "comment" };
  const view = render(
    withQueries(<MarkdownBody markdown="Holding on #q2 for now." source={source} />)
  );

  try {
    await waitFor(() =>
      expect(linked(view.container)).toEqual(["dispatch://CORE-1/ask/ask-design"])
    );
    expect(getReferenceQuestions).toHaveBeenCalledWith("comment", "comment-1");
  } finally {
    view.unmount();
  }
});

test("two texts of one source read the question map once", async () => {
  const getReferenceQuestions = spyQuestions({
    document: "doc-1",
    questions: [{ ask: "ask-2", block_id: "q2", reference: "dispatch://CORE-1/ask/ask-2" }],
  });
  const view = render(
    withQueries(
      <>
        <MarkdownBody markdown="See #q2 for the rollout." source={documentText} />
        <MarkdownBody markdown="And #q2 again." source={documentText} />
      </>
    )
  );

  try {
    await waitFor(() =>
      expect(linked(view.container)).toEqual([
        "dispatch://CORE-1/ask/ask-2",
        "dispatch://CORE-1/ask/ask-2",
      ])
    );
    expect(getReferenceQuestions).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
  }
});

test("a body that writes no `#id` reads no question map", async () => {
  const getReferenceQuestions = spyQuestions({ document: "doc-1", questions: [] });
  const view = render(
    withQueries(<MarkdownBody markdown="The rollout starts on Monday." source={documentText} />)
  );

  try {
    await waitFor(() => expect(view.container.textContent).toContain("The rollout starts"));
    expect(getReferenceQuestions).not.toHaveBeenCalled();
  } finally {
    view.unmount();
  }
});

test("a source whose text belongs to no document leaves its `#id` as plain text", async () => {
  const getReferenceQuestions = spyOn(api, "getReferenceQuestions").mockRejectedValue(
    new ApiError(404, { code: "NOT_FOUND", error: "the text of message m belongs to no document" })
  );
  restore = () => getReferenceQuestions.mockRestore();
  const source: TextSource = { id: "message-1", kind: "message" };
  const view = render(
    withQueries(<MarkdownBody markdown="See #q2 for the rollout." source={source} />)
  );

  try {
    await waitFor(() => expect(getReferenceQuestions).toHaveBeenCalledWith("message", "message-1"));
    expect(linked(view.container)).toEqual([]);
  } finally {
    view.unmount();
  }
});
