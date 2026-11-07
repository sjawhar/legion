import { expect, spyOn, test } from "bun:test";
import { DISPATCH_BLOCK_UNITS } from "@legion/contracts/dispatch-tools";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import type { Window as HappyDOMWindow } from "happy-dom";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { ApiError, api } from "../../api/client";
import type {
  AskRead,
  CommentRead,
  IssueAskRow,
  IssueReferenceSummary,
  ReferenceQuestions,
} from "../../api/types";
import { Unfurl } from "./Unfurl";

/** The test DOM is happy-dom's window (`__tests__/setup.ts` registers it), which TypeScript
 *  knows only as the DOM's; a dashboard address is read against the page's own origin, so the
 *  test that writes one puts the page on an origin first. */
const testWindow = window as unknown as HappyDOMWindow;

/** A wall-clock budget, not one of the machine-sensitive waits this package's defaults cover
 *  (`web/src/__tests__/setup.ts`): the regression the rows that read it guard, catastrophic
 *  backtracking, costs minutes rather than milliseconds, so a budget far above what any loaded
 *  pod spends on a single pass fails a rewrite that backtracks and nothing else. */
const BACKTRACKING_BUDGET_MS = 2_000;

function renderUnfurl(body: string, source?: Parameters<typeof Unfurl>[0]["source"]) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <Unfurl body={body} source={source} />
    </QueryClientProvider>
  );
}

test("Unfurl trims terminal punctuation before resolving a GitHub reference", async () => {
  const githubRest = spyOn(api, "githubRest").mockResolvedValue(
    new Response(JSON.stringify({ title: "Issue title" }), {
      headers: { "Content-Type": "application/json" },
    })
  );

  const view = renderUnfurl("https://github.com/owner/repository/issues/1.");
  try {
    await waitFor(() => {
      expect(githubRest).toHaveBeenCalledWith("/repos/owner/repository/issues/1");
    });
    await within(view.container).findByRole("link", { name: "Issue title" });
  } finally {
    view.unmount();
    githubRest.mockRestore();
  }
});

/** CORE-1's summary read, with its spec `design`. */
const coreSummary: IssueReferenceSummary = {
  documents: [
    {
      id: "artifact-1",
      kind: "doc",
      latest_version: 3,
      name: "design.md",
      primary: true,
      slug: "design",
    },
  ],
  key: "CORE-1",
  project: "CORE",
  status: "testing",
  title: "Design decision",
};

test("Unfurl shows an issue's title and status from its summary, without the full issue read", async () => {
  const getIssueSummary = spyOn(api, "getIssueSummary").mockResolvedValue({
    ...coreSummary,
    documents: [],
  });
  const getIssue = spyOn(api, "getIssue");
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  const view = render(
    <QueryClientProvider client={queryClient}>
      <Unfurl body="dispatch://CORE-1" />
    </QueryClientProvider>
  );

  try {
    await within(view.container).findByText("Design decision");
    await within(view.container).findByText("testing");
    expect(getIssueSummary).toHaveBeenCalledWith("CORE-1");
    expect(getIssue).not.toHaveBeenCalled();
  } finally {
    getIssue.mockRestore();
    getIssueSummary.mockRestore();
    view.unmount();
  }
});

test("Unfurl reads the immutable document version named by a reference", async () => {
  const getIssueSummary = spyOn(api, "getIssueSummary").mockResolvedValue(coreSummary);
  const getIssue = spyOn(api, "getIssue");
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "Live document",
    version: null,
    token: "sha256:live-document",
  });
  const getArtifactVersion = spyOn(api, "getArtifactVersion").mockResolvedValue({
    authors: [],
    created_at: "2026-09-09T00:00:00Z",
    markdown: "Version three",
    named: true,
    number: 3,
    summary: "Version three",
  });
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });

  try {
    const view = render(
      <QueryClientProvider client={queryClient}>
        <Unfurl body="dispatch://CORE-1/artifact/design@v3" />
      </QueryClientProvider>
    );

    await screen.findByText("Version three");
    expect(screen.getByText("design.md")).toBeDefined();
    expect(getArtifactVersion).toHaveBeenCalledWith("artifact-1", 3);
    expect(getArtifactText).not.toHaveBeenCalled();
    expect(getIssue).not.toHaveBeenCalled();
    view.unmount();
  } finally {
    getArtifactVersion.mockRestore();
    getArtifactText.mockRestore();
    getIssue.mockRestore();
    getIssueSummary.mockRestore();
  }
});

test("Unfurl unfurls a dispatch project document reference with its name and document link", async () => {
  const getProjectDocumentSummary = spyOn(api, "getProjectDocumentSummary").mockResolvedValue({
    id: "artifact-1",
    kind: "doc",
    latest_version: 3,
    name: "Design notes",
    project: "CORE",
    slug: "design-notes",
  });
  const getProjectArtifact = spyOn(api, "getProjectArtifact");
  const getArtifactVersion = spyOn(api, "getArtifactVersion").mockResolvedValue({
    authors: [],
    created_at: "2026-09-10T00:00:00Z",
    markdown: "Project version three",
    named: true,
    number: 3,
    summary: "Version three",
  });
  const getIssue = spyOn(api, "getIssue");
  const getIssueSummary = spyOn(api, "getIssueSummary");
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });

  try {
    const view = render(
      <QueryClientProvider client={queryClient}>
        <Unfurl body="dispatch://CORE/artifact/design-notes@v3" />
      </QueryClientProvider>
    );

    await screen.findByText("Project version three");
    expect(getProjectDocumentSummary).toHaveBeenCalledWith("CORE", "design-notes");
    expect(getProjectArtifact).not.toHaveBeenCalled();
    expect(getIssue).not.toHaveBeenCalled();
    expect(getIssueSummary).not.toHaveBeenCalled();
    expect(screen.getByRole("link", { name: /Design notes/ }).getAttribute("href")).toBe(
      "/projects/CORE/documents/design-notes?version=3"
    );
    view.unmount();
  } finally {
    getIssueSummary.mockRestore();
    getIssue.mockRestore();
    getArtifactVersion.mockRestore();
    getProjectArtifact.mockRestore();
    getProjectDocumentSummary.mockRestore();
  }
});

test("Unfurl unfurls a dispatch ask reference with the question, reading no issue", async () => {
  const askRead: AskRead = {
    ask: {
      id: "ask-1",
      issue_key: "CORE-1",
      kind: "question",
      author: { id: "alice", kind: "user" },
      question: "Should we ship this on Friday or wait for the review to land first?",
      options: [],
      multiple: false,
      urgency: "med",
      anchor: null,
      state: "open",
      answer: null,
      opened_event_id: 1,
      created_at: "2026-09-09T00:00:00Z",
      edited_at: null,
    },
    answers: [],
    edits: [],
    claimant: null,
    followers: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
    replies: [],
  };
  const getIssue = spyOn(api, "getIssue");
  const getIssueSummary = spyOn(api, "getIssueSummary");
  const getAsk = spyOn(api, "getAsk").mockResolvedValue(askRead);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  const view = render(
    <QueryClientProvider client={queryClient}>
      <Unfurl body="dispatch://CORE-1/ask/ask-1" />
    </QueryClientProvider>
  );

  try {
    await within(view.container).findByText(
      "Should we ship this on Friday or wait for the review to land…"
    );
    expect(getAsk).toHaveBeenCalledWith("ask-1");
    expect(getIssue).not.toHaveBeenCalled();
    expect(getIssueSummary).not.toHaveBeenCalled();
  } finally {
    getAsk.mockRestore();
    getIssueSummary.mockRestore();
    getIssue.mockRestore();
    view.unmount();
  }
});

test("Unfurl unfurls a dispatch comment reference with its own words, reading no issue", async () => {
  const commentRead: CommentRead = {
    comment: {
      id: "comment-1",
      issue_key: "CORE-1",
      author: { id: "alice", kind: "user" },
      body: "**Looks** good overall.\nOne nit below.",
      anchor: null,
      reply_to: null,
      ask_id: null,
      turn: null,
      resolved: false,
      resolved_by: null,
      resolved_at: null,
      edited_at: null,
      suggestion: null,
      created_at: "2026-09-09T00:00:00Z",
      ...commentDeliveryFields(),
    },
    replies: [],
  };
  const getIssue = spyOn(api, "getIssue");
  const getIssueSummary = spyOn(api, "getIssueSummary");
  const getComment = spyOn(api, "getComment").mockResolvedValue(commentRead);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  const view = render(
    <QueryClientProvider client={queryClient}>
      <Unfurl body="dispatch://CORE-1/comment/comment-1" />
    </QueryClientProvider>
  );

  try {
    // The title is the comment projected to plain words: the soft-wrapped line is one
    // paragraph (as the editor reads it), and the bold is dropped, not shown as `**`.
    await within(view.container).findByText("Looks good overall. One nit below.");
    expect(getComment).toHaveBeenCalledWith("comment-1");
    expect(getIssue).not.toHaveBeenCalled();
    expect(getIssueSummary).not.toHaveBeenCalled();
  } finally {
    getComment.mockRestore();
    getIssueSummary.mockRestore();
    getIssue.mockRestore();
    view.unmount();
  }
});

/** The question map the route answers for the body's source: `#q1` is the question block of
 *  CORE-1's spec, settled into `ask-1`. */
const specQuestions: ReferenceQuestions = {
  document: "artifact-1",
  questions: [{ ask: "ask-1", block_id: "q1", reference: "dispatch://CORE-1/ask/ask-1" }],
};

/** The ask `#q1` names, as its own read carries it. */
const blockAsk: IssueAskRow = {
  anchor: null,
  answer: null,
  author: { id: "alice", kind: "user" },
  block_artifact: { id: "artifact-1", primary: true, slug: "design" },
  block_id: "q1",
  created_at: "2026-09-09T00:00:00Z",
  edited_at: null,
  id: "ask-1",
  issue_key: "CORE-1",
  kind: "question",
  last_reply: null,
  multiple: false,
  opened_event_id: 1,
  options: [],
  question: "Which region do we deploy to?",
  state: "open",
  urgency: "med",
};

/** `blockAsk`'s own read, which the card behind `#q1` shows the question from. */
const blockAskRead: AskRead = {
  answers: [],
  ask: blockAsk,
  claimant: null,
  edits: [],
  followers: [],
  replies: [],
  route: null,
  route_holder: null,
  route_status: null,
  route_status_reason: null,
};

test("Unfurl cards an issue key written bare, once its issue exists", async () => {
  const getIssueSummary = spyOn(api, "getIssueSummary").mockResolvedValue(coreSummary);
  const view = renderUnfurl("CORE-1");

  try {
    const card = await within(view.container).findByRole("link", { name: /Design decision/ });
    expect(card.getAttribute("href")).toBe("/issues/CORE-1");
    expect(getIssueSummary).toHaveBeenCalledWith("CORE-1");
  } finally {
    getIssueSummary.mockRestore();
    view.unmount();
  }
});

// A reference may end a sentence. The stop, the comma and the rest of what the reference
// grammar trims (`trimReference`) belong to the word that holds the reference, so each of
// these bodies is one reference and cards as it did before the predicate read them.
test("Unfurl cards a reference that ends in the punctuation the grammar trims", async () => {
  const getIssueSummary = spyOn(api, "getIssueSummary").mockResolvedValue(coreSummary);
  const getReferenceQuestions = spyOn(api, "getReferenceQuestions").mockResolvedValue(
    specQuestions
  );
  const getAsk = spyOn(api, "getAsk").mockResolvedValue(blockAskRead);
  const address = renderUnfurl("dispatch://CORE-1.");
  const key = renderUnfurl("CORE-1,");
  const question = renderUnfurl("#q1.", { id: "message-1", kind: "message" });

  try {
    expect(
      (
        await within(address.container).findByRole("link", { name: /Design decision/ })
      ).getAttribute("href")
    ).toBe("/issues/CORE-1");
    await within(key.container).findByRole("link", { name: /Design decision/ });
    await within(question.container).findByRole("link", {
      name: "Which region do we deploy to?",
    });
  } finally {
    getAsk.mockRestore();
    getReferenceQuestions.mockRestore();
    getIssueSummary.mockRestore();
    address.unmount();
    key.unmount();
    question.unmount();
  }
});

test("Unfurl cards a dashboard address of this dashboard", async () => {
  const pageBefore = window.location.href;
  testWindow.happyDOM.setURL("https://dispatch.test/issues/CORE-2");
  const getIssueSummary = spyOn(api, "getIssueSummary").mockResolvedValue(coreSummary);
  const view = renderUnfurl("https://dispatch.test/issues/CORE-1");

  try {
    const card = await within(view.container).findByRole("link", { name: /Design decision/ });
    expect(card.getAttribute("href")).toBe("/issues/CORE-1");
  } finally {
    getIssueSummary.mockRestore();
    view.unmount();
    testWindow.happyDOM.setURL(pageBefore);
  }
});

test("Unfurl cards a #id against the document the body's text belongs to", async () => {
  const getReferenceQuestions = spyOn(api, "getReferenceQuestions").mockResolvedValue(
    specQuestions
  );
  const getAsk = spyOn(api, "getAsk").mockResolvedValue(blockAskRead);
  const view = renderUnfurl("#q1", { id: "message-1", kind: "message" });

  try {
    const card = await within(view.container).findByRole("link", {
      name: "Which region do we deploy to?",
    });
    expect(card.getAttribute("href")).toBe("/issues/CORE-1/asks/ask-1");
    expect(getReferenceQuestions).toHaveBeenCalledWith("message", "message-1");
  } finally {
    getAsk.mockRestore();
    getReferenceQuestions.mockRestore();
    view.unmount();
  }
});

test("Unfurl cards every reference of a body that holds several, in order", async () => {
  const getIssueSummary = spyOn(api, "getIssueSummary").mockResolvedValue(coreSummary);
  const getAsk = spyOn(api, "getAsk").mockResolvedValue(blockAskRead);
  const view = renderUnfurl("CORE-1 dispatch://CORE-1/ask/ask-1");

  try {
    await within(view.container).findByRole("link", { name: /Design decision/ });
    await within(view.container).findByRole("link", { name: "Which region do we deploy to?" });
    expect(within(view.container).getAllByRole("link")).toHaveLength(2);
  } finally {
    getAsk.mockRestore();
    getIssueSummary.mockRestore();
    view.unmount();
  }
});

test("Unfurl shows no card for a bare key whose issue does not exist", async () => {
  const getIssueSummary = spyOn(api, "getIssueSummary").mockRejectedValue(
    new ApiError(404, { code: "NOT_FOUND", error: "no issue CORE-9" })
  );
  const view = renderUnfurl("CORE-9");

  try {
    await waitFor(() => expect(getIssueSummary).toHaveBeenCalledWith("CORE-9"));
    await waitFor(() => expect(view.container.querySelector("section")).toBeNull());
    expect(within(view.container).queryAllByRole("link")).toHaveLength(0);
  } finally {
    getIssueSummary.mockRestore();
    view.unmount();
  }
});

test("Unfurl shows no card, and asks nothing, for a body with prose beside its reference", async () => {
  const getIssueSummary = spyOn(api, "getIssueSummary");
  const githubRest = spyOn(api, "githubRest");
  const views = [
    renderUnfurl("See dispatch://CORE-1"),
    renderUnfurl("CORE-1 for details"),
    // Emphasis opened before a reference is a character no reference covers: the `**` stands
    // outside the word's reference, so the body holds more than its reference.
    renderUnfurl("**https://github.com/owner/repository/pull/2**"),
    // Two references with no whitespace between them are one run to the span reader, and that
    // run names nothing: a word is one reference or none.
    renderUnfurl("dispatch://CORE-1https://dispatch.test/issues/CORE-2"),
    renderUnfurl(""),
  ];

  try {
    for (const view of views) {
      expect(view.container.querySelector("section")).toBeNull();
    }
    expect(getIssueSummary).not.toHaveBeenCalled();
    expect(githubRest).not.toHaveBeenCalled();
  } finally {
    githubRest.mockRestore();
    getIssueSummary.mockRestore();
    for (const view of views) {
      view.unmount();
    }
  }
});

// Every comment body and ask question passes this check as it renders, and one block of a
// comment holds up to DISPATCH_BLOCK_UNITS UTF-16 units. A body of repeated schemes splits into
// references in exponentially many ways, and a pattern that tried them ran for minutes in V8 on
// one of these bodies, hanging every reader's tab; JavaScriptCore, which runs these tests, gives
// up after a second or so per body.
test(`Unfurl reads ${DISPATCH_BLOCK_UNITS}-character bodies of repeated schemes in one pass`, () => {
  const start = performance.now();
  const views = [];
  for (const scheme of ["http://", "https://", "dispatch://"]) {
    // The repeat leaves room for the two units the ` x` and `\nx` cases append, so every body
    // here is at or just under the bound.
    const references = scheme.repeat(Math.floor((DISPATCH_BLOCK_UNITS - 2) / scheme.length));
    views.push(renderUnfurl(`${references} x`), renderUnfurl(`${references}\nx`));
    // The run itself names nothing - no scheme repeated is a reference - so it cards nothing
    // either; what this holds is that reading it costs one pass.
    views.push(renderUnfurl(references));
  }
  try {
    for (const view of views) {
      expect(view.container.querySelector("section")).toBeNull();
    }
    // This row reads 0.03 s idle and 0.16 s loaded on a 2-core pod, and the budget is 12 times
    // that slowest loaded reading, so no loaded pod can fail it by being slow.
    expect(performance.now() - start).toBeLessThan(BACKTRACKING_BUDGET_MS);
  } finally {
    for (const view of views) {
      view.unmount();
    }
  }
});

test("Unfurl links a GitHub pull request's card to the URL the body wrote", async () => {
  const githubRest = spyOn(api, "githubRest").mockResolvedValue(
    new Response(JSON.stringify({ title: "Pull title" }), {
      headers: { "Content-Type": "application/json" },
    })
  );
  const view = renderUnfurl("https://github.com/owner/repository/pull/2");

  try {
    await waitFor(() => {
      expect(githubRest).toHaveBeenCalledWith("/repos/owner/repository/issues/2");
    });
    expect(within(view.container).getByRole("link").getAttribute("href")).toBe(
      "https://github.com/owner/repository/pull/2"
    );
  } finally {
    githubRest.mockRestore();
    view.unmount();
  }
});

// The trim once rescanned the closing run from every position in it, taking seconds on 64 KiB.
test("Unfurl reads a GitHub link trailed by a long closing run in one pass", () => {
  const githubRest = spyOn(api, "githubRest");
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const start = performance.now();
  const view = render(
    <QueryClientProvider client={queryClient}>
      <Unfurl body={`https://github.com/owner/repository/pull/3${")".repeat(1 << 16)}x`} />
    </QueryClientProvider>
  );

  try {
    // This row reads 0.01 s idle and 0.07 s loaded on a 2-core pod.
    expect(performance.now() - start).toBeLessThan(BACKTRACKING_BUDGET_MS);
    expect(view.container.querySelector("section")).toBeNull();
    expect(githubRest).not.toHaveBeenCalled();
  } finally {
    githubRest.mockRestore();
    view.unmount();
  }
});

// LEGION-540. A GitHub issue's body is Markdown its author wrote: the card renders it formatted
// and cut to its lines after rendering, never the raw source with its `**`, backticks and `-`.
test("Unfurl renders a GitHub issue's body formatted, on one run of text, with no syntax", async () => {
  const githubRest = spyOn(api, "githubRest").mockResolvedValue(
    new Response(
      JSON.stringify({
        body: "## Steps\n\n**Crash** on `start` after [upgrading](https://example.com/notes)\n\n- open it\n- wait",
        title: "Crash on start",
      }),
      { headers: { "Content-Type": "application/json" } }
    )
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <Unfurl body="https://github.com/owner/repository/issues/4" />
    </QueryClientProvider>
  );

  try {
    await within(view.container).findByText("Crash on start");
    const body = await waitFor(() => {
      const found = view.container.querySelector<HTMLElement>("[data-markdown-preview]");
      if (found === null || found.querySelector("strong") === null) {
        throw new Error("the issue body has not rendered");
      }
      return found;
    });
    expect(body.querySelector("strong")?.textContent).toBe("Crash");
    expect(body.querySelector("code")?.textContent).toBe("start");
    expect(body.textContent?.replace(/\s+/g, " ").trim()).toBe(
      "Steps Crash on start after upgrading open it wait"
    );
    expect(body.querySelector("h2, ul, li, p")).toBeNull();
  } finally {
    githubRest.mockRestore();
    view.unmount();
  }
});
