import { afterEach, beforeAll, beforeEach, expect, spyOn, test } from "bun:test";
import { DISPATCH_TEXT_REFERENCES } from "@legion/contracts";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, waitFor } from "@testing-library/react";
import type { Window as HappyDOMWindow } from "happy-dom";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { graphNode } from "../../__tests__/graph-fixture";
import { ApiError, api } from "../../api/client";
import type { GraphReferences, IssueReferenceSummary, ReferenceQuestions } from "../../api/types";
import { MarkdownBody } from "./MarkdownBody";
import { MarkdownPreview } from "./MarkdownPreview";
import { warmMarkdownRenderer } from "./markdown-engine";
import { readTextReferences, type TextSource } from "./text-references";

const origin = "https://dispatch.test";

/** The record every row's text is: a message of CORE-1, whose `#id`s resolve against the
 *  question map the server answers for that source, which the row's `questions` is. */
const source = { id: "message-1", kind: "message" } as const satisfies TextSource;

const testWindow = window as unknown as HappyDOMWindow;
let pageBefore = "about:blank";

beforeAll(async () => {
  await warmMarkdownRenderer();
});

beforeEach(() => {
  pageBefore = window.location.href;
});

afterEach(() => {
  testWindow.happyDOM.setURL(pageBefore);
});

function summaryOf(key: string): IssueReferenceSummary {
  return {
    documents: [],
    key,
    project: key.slice(0, key.lastIndexOf("-")),
    status: "in_progress",
    title: `Title of ${key}`,
  };
}

/** The question map the route answers for that source, as `refs.QuestionMap` writes it: each
 *  question block of CORE-1's spec with the ask settled for it and that ask's address. */
function questionsOf(questions: Readonly<Record<string, string>>): ReferenceQuestions {
  return {
    document: "spec-1",
    questions: Object.entries(questions).map(([block, reference]) => ({
      ask: reference.slice(reference.lastIndexOf("/") + 1),
      block_id: block,
      reference,
    })),
  };
}

/** The graph read of a reference whose id is written short, as the server answers it: the one
 *  item of `items` whose own reference the written one is a prefix of. A prefix two items carry,
 *  and one no item carries, is a 404. */
function prefixAnswer(
  reference: string,
  items: Readonly<Record<string, string>>
): GraphReferences | undefined {
  const matches = Object.entries(items).filter(([, whole]) =>
    whole.toLowerCase().startsWith(reference.toLowerCase())
  );
  const found = matches.length === 1 ? matches[0] : undefined;
  if (found === undefined) {
    return undefined;
  }
  const [id, whole] = found;
  const block = whole.includes("#b-");
  return {
    edges: [],
    node: block
      ? graphNode({ block_id: id, id: "spec-1", kind: "block", ref: whole })
      : graphNode({ id, kind: "ask", ref: whole }),
  };
}

/** A query cache holding what the row says exists: each issue of `issues`, the asks that declare
 *  `questions`, and the graph read that resolves each id the body writes short. */
function cacheOf(row: (typeof DISPATCH_TEXT_REFERENCES)[number], server: string): QueryClient {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  for (const key of row.issues ?? []) {
    queryClient.setQueryData(["issue-summary", key], summaryOf(key));
  }
  if (row.questions !== undefined) {
    queryClient.setQueryData(
      ["references", "questions", source.kind, source.id],
      questionsOf(row.questions)
    );
  }
  for (const reference of readTextReferences(row.body, { origin: server }).needs.prefixes) {
    const answer = prefixAnswer(reference, row.items ?? {});
    if (answer !== undefined) {
      queryClient.setQueryData(["references", "to", reference], answer);
    }
  }
  return queryClient;
}

function withQueries(queryClient: QueryClient, children: ReactNode): ReactNode {
  return (
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>{children}</MemoryRouter>
    </QueryClientProvider>
  );
}

/** The references a rendered element draws a link for, in the order it draws them: an inline
 *  reference anchor, and the placeholder a Dispatch picture links its artifact's page from.
 *  `MarkdownPreview` makes each link an inert span carrying the same `data-dispatch-ref`, so
 *  both surfaces are read the same way. */
function linked(element: HTMLElement): string[] {
  return [...element.querySelectorAll("[data-dispatch-ref],[data-dispatch-picture]")].map(
    (anchor) =>
      anchor.getAttribute("data-dispatch-ref") ?? anchor.getAttribute("data-dispatch-picture") ?? ""
  );
}

// The renderers' half of the shared text table: every row, rendered against a cache holding what
// the row says exists, links exactly what the row's `links` names - which is its `refs` without
// the ones inside a code span, a code block or a link, where the server indexes a reference and
// no renderer draws one.
test.each(
  DISPATCH_TEXT_REFERENCES.map(
    (row) => [row.body, row.origin ?? origin, row, [...(row.links ?? row.refs)]] as const
  )
)("%s links, rendered against %s, its row's links", async (body, server, row, links) => {
  testWindow.happyDOM.setURL(`${server}/issues/CORE-1`);
  const getIssueSummary = spyOn(api, "getIssueSummary").mockRejectedValue(
    new ApiError(404, { code: "NOT_FOUND", error: "no issue" })
  );
  const getReferences = spyOn(api, "getReferences").mockRejectedValue(
    new ApiError(404, { code: "NOT_FOUND", error: "no node" })
  );
  // The seeded reads are stale at once, so the answer behind each is the row's own: a refetch
  // of the question map must not replace the questions the row declares.
  const getReferenceQuestions = spyOn(api, "getReferenceQuestions").mockResolvedValue(
    questionsOf(row.questions ?? {})
  );
  const bodyView = render(
    withQueries(cacheOf(row, server), <MarkdownBody markdown={body} source={source} />)
  );
  const previewView = render(
    withQueries(cacheOf(row, server), <MarkdownPreview lines={3} markdown={body} source={source} />)
  );

  try {
    await waitFor(() => expect(linked(bodyView.container)).toEqual([...links]));
    await waitFor(() => expect(linked(previewView.container)).toEqual([...links]));
  } finally {
    bodyView.unmount();
    previewView.unmount();
    getIssueSummary.mockRestore();
    getReferences.mockRestore();
    getReferenceQuestions.mockRestore();
  }
});
