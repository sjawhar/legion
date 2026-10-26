import { expect, spyOn, test } from "bun:test";
import { DISPATCH_BODY_MAX } from "@legion/contracts/dispatch-tools";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { api } from "../../api/client";
import type { AskRead, CommentRead, IssueReferenceSummary } from "../../api/types";
import { isBareReferenceBody, Unfurl } from "./Unfurl";

test("Unfurl trims terminal punctuation before resolving a GitHub reference", async () => {
  const githubRest = spyOn(api, "githubRest").mockResolvedValue(
    new Response(JSON.stringify({ title: "Issue title" }), {
      headers: { "Content-Type": "application/json" },
    })
  );
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });

  try {
    const view = render(
      <QueryClientProvider client={queryClient}>
        <Unfurl body={"See https://github.com/owner/repository/issues/1."} />
      </QueryClientProvider>
    );

    await waitFor(() => {
      expect(githubRest).toHaveBeenCalledWith("/repos/owner/repository/issues/1");
    });
    view.unmount();
  } finally {
    githubRest.mockRestore();
  }
});

/** CORE-1's summary read, with its spec `design`. */
const coreSummary: IssueReferenceSummary = {
  documents: [
    { id: "artifact-1", kind: "doc", latest_version: 3, name: "design.md", slug: "design" },
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
      <Unfurl body="See dispatch://CORE-1" />
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
        <Unfurl body="See dispatch://CORE-1/artifact/design@v3" />
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
        <Unfurl body="See dispatch://CORE/artifact/design-notes@v3" />
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
    followers: [],
    replies: [],
  };
  const getIssue = spyOn(api, "getIssue");
  const getIssueSummary = spyOn(api, "getIssueSummary");
  const getAsk = spyOn(api, "getAsk").mockResolvedValue(askRead);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  const view = render(
    <QueryClientProvider client={queryClient}>
      <Unfurl body="See dispatch://CORE-1/ask/ask-1" />
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
      <Unfurl body="See dispatch://CORE-1/comment/comment-1" />
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

test("isBareReferenceBody accepts only whitespace-joined bare references", () => {
  expect(isBareReferenceBody("dispatch://CORE-1")).toBe(true);
  expect(isBareReferenceBody("  dispatch://CORE-1  ")).toBe(true);
  expect(isBareReferenceBody("dispatch://CORE-1 dispatch://CORE-2")).toBe(true);
  expect(isBareReferenceBody("dispatch://CORE-1\nhttps://dispatch.test/issues/CORE-2")).toBe(true);
  expect(isBareReferenceBody("dispatch://CORE-1https://dispatch.test/issues/CORE-2")).toBe(true);
  expect(isBareReferenceBody("See dispatch://CORE-1")).toBe(false);
  expect(isBareReferenceBody("dispatch://CORE-1 for details")).toBe(false);
  expect(isBareReferenceBody("dispatch://")).toBe(false);
  expect(isBareReferenceBody("")).toBe(false);
});

// Every comment body and ask question passes this check as it renders, and a comment holds up to
// DISPATCH_BODY_MAX UTF-16 units. A body of repeated schemes splits into references in
// exponentially many ways, and a pattern that tried them ran for minutes in V8 on one of these
// bodies, hanging every reader's tab; JavaScriptCore, which runs these tests, gives up after a
// second or so per body.
test(`isBareReferenceBody reads ${DISPATCH_BODY_MAX}-character bodies of repeated schemes in one pass`, () => {
  const start = performance.now();
  for (const scheme of ["http://", "https://", "dispatch://"]) {
    // The repeat leaves room for the two units the ` x` and `\nx` cases append, so every body
    // here is at or just under the cap.
    const references = scheme.repeat(Math.floor((DISPATCH_BODY_MAX - 2) / scheme.length));
    expect(isBareReferenceBody(`${references} x`)).toBe(false);
    expect(isBareReferenceBody(`${references}\nx`)).toBe(false);
    expect(isBareReferenceBody(references)).toBe(true);
  }
  expect(performance.now() - start).toBeLessThan(2000);
});

test("Unfurl resolves a bold GitHub link without its emphasis", async () => {
  const githubRest = spyOn(api, "githubRest").mockResolvedValue(
    new Response(JSON.stringify({ title: "Pull title" }), {
      headers: { "Content-Type": "application/json" },
    })
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <Unfurl body={"See **https://github.com/owner/repository/pull/2**."} />
    </QueryClientProvider>
  );

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
    expect(performance.now() - start).toBeLessThan(2000);
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
      <Unfurl body="See https://github.com/owner/repository/issues/4" />
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
