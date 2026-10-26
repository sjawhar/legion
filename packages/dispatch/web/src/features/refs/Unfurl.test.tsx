import { afterAll, beforeAll, expect, spyOn, test } from "bun:test";
import { DISPATCH_BODY_MAX } from "@legion/contracts/dispatch-tools";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import type { Window as HappyDOMWindow } from "happy-dom";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { ApiError, api, type ReferenceSource } from "../../api/client";
import type { Artifact, AskRead, CommentRead, IssueDetails } from "../../api/types";
import { warmMarkdownRenderer } from "./markdown-engine";
import { issueQuery, referenceQuestionsQuery } from "./reference-target";
import type { BareReferences } from "./routes";
import { Unfurl } from "./Unfurl";

const origin = "https://dispatch.test";
const testWindow = window as unknown as HappyDOMWindow;
let pageBefore = "about:blank";

beforeAll(async () => {
  pageBefore = window.location.href;
  testWindow.happyDOM.setURL(`${origin}/issues/CORE-1/conversation`);
  await warmMarkdownRenderer();
});

afterAll(() => {
  testWindow.happyDOM.setURL(pageBefore);
});

test("Unfurl resolves a body that is one GitHub reference", async () => {
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
        <Unfurl body={"https://github.com/owner/repository/issues/1"} />
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

test("Unfurl reads the immutable document version named by a reference", async () => {
  const artifact: Artifact = {
    created_at: "2026-09-09T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    id: "artifact-1",
    issue_key: "CORE-1",
    project: "CORE",
    kind: "doc",
    name: "design.md",
    primary: true,
    slug: "design",
    versions: [],
  };
  const issue: IssueDetails = {
    route_status: null,
    route_holder: null,
    artifacts: [artifact],
    children: [],
    closed_at: null,
    created_at: "2026-09-09T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    external_links: [],
    key: "CORE-1",
    labels: [],
    last_seq: 1,
    number: 1,
    parent: null,
    blocked_by: [],
    assignee: null,
    claim: null,
    components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
    primary_artifact_id: artifact.id,
    project: "CORE",
    referenced_by_count: 0,
    route: null,
    repo: null,
    resolved_repo: null,
    status: "testing",
    priority: null,
    rank: "U",
    title: "Design decision",
    open_asks: [],
    progress: { tasks: null, children: null },
    updated_at: "2026-09-09T00:00:00Z",
  };
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
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
    expect(getArtifactVersion).toHaveBeenCalledWith("artifact-1", 3);
    expect(getArtifactText).not.toHaveBeenCalled();
    view.unmount();
  } finally {
    getArtifactVersion.mockRestore();
    getArtifactText.mockRestore();
    getIssue.mockRestore();
  }
});

test("Unfurl unfurls a dispatch project document reference with its name and document link", async () => {
  const artifact: Artifact = {
    created_at: "2026-09-10T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    id: "artifact-1",
    issue_key: null,
    project: "CORE",
    kind: "doc",
    name: "Design notes",
    primary: false,
    slug: "design-notes",
    versions: [],
  };
  const getProjectArtifact = spyOn(api, "getProjectArtifact").mockResolvedValue({
    ...artifact,
  });
  const getArtifactVersion = spyOn(api, "getArtifactVersion").mockResolvedValue({
    authors: [],
    created_at: "2026-09-10T00:00:00Z",
    markdown: "Project version three",
    named: true,
    number: 3,
    summary: "Version three",
  });
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(undefined as never);
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
    expect(getProjectArtifact).toHaveBeenCalledWith("CORE", "design-notes");
    expect(getIssue).not.toHaveBeenCalled();
    expect(screen.getByRole("link", { name: /Design notes/ }).getAttribute("href")).toBe(
      "/projects/CORE/documents/design-notes?version=3"
    );
    view.unmount();
  } finally {
    getIssue.mockRestore();
    getArtifactVersion.mockRestore();
    getProjectArtifact.mockRestore();
  }
});

test("Unfurl titles an issue reference with its key in semibold, then its title", async () => {
  const getIssue = spyOn(api, "getIssue").mockResolvedValue({
    artifacts: [],
    key: "CORE-12",
    status: "todo",
    title: "Fix the thing",
  } as never);
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });

  try {
    const view = render(
      <QueryClientProvider client={queryClient}>
        <Unfurl body="dispatch://CORE-12" />
      </QueryClientProvider>
    );

    const card = await screen.findByRole("link", { name: /^CORE-12 Fix the thing/ });
    expect(card.getAttribute("href")).toBe("/issues/CORE-12");
    expect(card.querySelector(".font-semibold")?.textContent).toBe("CORE-12");
    view.unmount();
  } finally {
    getIssue.mockRestore();
  }
});

test("Unfurl unfurls a dispatch ask reference with the question, not the issue title", async () => {
  const issue: IssueDetails = {
    route_status: null,
    route_holder: null,
    artifacts: [],
    children: [],
    closed_at: null,
    created_at: "2026-09-09T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    external_links: [],
    key: "CORE-1",
    labels: [],
    last_seq: 1,
    number: 1,
    parent: null,
    blocked_by: [],
    assignee: null,
    claim: null,
    components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
    primary_artifact_id: "artifact-none",
    project: "CORE",
    referenced_by_count: 0,
    route: null,
    repo: null,
    resolved_repo: null,
    status: "testing",
    priority: null,
    rank: "U",
    title: "Design decision",
    open_asks: [],
    progress: { tasks: null, children: null },
    updated_at: "2026-09-09T00:00:00Z",
  };
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
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
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
    expect(within(view.container).queryByText("Design decision")).toBeNull();
    expect(getAsk).toHaveBeenCalledWith("ask-1");
  } finally {
    getAsk.mockRestore();
    getIssue.mockRestore();
    view.unmount();
  }
});

test("Unfurl unfurls a dispatch comment reference with its own words, not the issue title", async () => {
  const issue: IssueDetails = {
    route_status: null,
    route_holder: null,
    artifacts: [],
    children: [],
    closed_at: null,
    created_at: "2026-09-09T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    external_links: [],
    key: "CORE-1",
    labels: [],
    last_seq: 1,
    number: 1,
    parent: null,
    blocked_by: [],
    assignee: null,
    claim: null,
    components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
    primary_artifact_id: "artifact-none",
    project: "CORE",
    referenced_by_count: 0,
    route: null,
    repo: null,
    resolved_repo: null,
    status: "testing",
    priority: null,
    rank: "U",
    title: "Design decision",
    open_asks: [],
    progress: { tasks: null, children: null },
    updated_at: "2026-09-09T00:00:00Z",
  };
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
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
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
    expect(within(view.container).queryByText("Design decision")).toBeNull();
    expect(getComment).toHaveBeenCalledWith("comment-1");
  } finally {
    getComment.mockRestore();
    getIssue.mockRestore();
    view.unmount();
  }
});

const resolved: BareReferences = {
  issues: new Set(["CORE-12"]),
  questions: new Map([["q2", "dispatch://CORE-1/ask/ask-2"]]),
};
const nothingResolved: BareReferences = { issues: new Set(), questions: new Map() };

/**
 * The cards `Unfurl` shows for body once it has rendered, each as the address it opens, or `null`
 * when it shows none: `bare`'s keys are the issues that exist and its map is the question map of
 * the body's document, both already read, and no other target exists.
 */
function cardsShown(body: string, bare: BareReferences): (string | null)[] | null {
  const issues = new Map(
    Array.from(
      bare.issues,
      (key) =>
        [key, { artifacts: [], key, status: "todo", title: `Title of ${key}` } as never] as const
    )
  );
  const questionMap = {
    document: "dispatch://CORE-1/spec",
    questions: Array.from(bare.questions, ([block_id, ask]) => ({ ask, block_id })),
  };
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  for (const [key, issue] of issues) {
    queryClient.setQueryData(issueQuery(key).queryKey, issue);
  }
  queryClient.setQueryData(referenceQuestionsQuery(commentSource).queryKey, questionMap);
  const spies = [
    spyOn(api, "getIssue").mockImplementation(async (key: string) => {
      const issue = issues.get(key);
      if (issue === undefined) {
        throw notFound();
      }
      return issue;
    }),
    spyOn(api, "getReferenceQuestions").mockResolvedValue(questionMap),
    spyOn(api, "getAsk").mockRejectedValue(notFound()),
    spyOn(api, "githubRest").mockRejectedValue(notFound()),
  ];
  const view = render(
    <QueryClientProvider client={queryClient}>
      <Unfurl body={body} source={commentSource} />
    </QueryClientProvider>
  );
  try {
    const section = within(view.container).queryByRole("region", { name: "References" });
    return section === null
      ? null
      : within(section)
          .getAllByRole("link")
          .map((card) => card.getAttribute("href"));
  } finally {
    view.unmount();
    for (const spy of spies) {
      spy.mockRestore();
    }
  }
}

test("Unfurl shows a card for a body that is one reference in each form Dispatch reads", () => {
  expect(cardsShown("dispatch://CORE-1", nothingResolved)).toEqual(["/issues/CORE-1"]);
  expect(cardsShown("https://dispatch.test/issues/CORE-2", nothingResolved)).toEqual([
    "/issues/CORE-2",
  ]);
  expect(cardsShown("CORE-12", resolved)).toEqual(["/issues/CORE-12"]);
  expect(cardsShown("#q2", resolved)).toEqual(["/issues/CORE-1/asks/ask-2"]);
  expect(cardsShown("https://github.com/owner/repository/pull/2", resolved)).toEqual([
    "https://github.com/owner/repository/pull/2",
  ]);
  expect(cardsShown("  CORE-12 \n", resolved)).toEqual(["/issues/CORE-12"]);
});

test("Unfurl shows a card per reference for references with only whitespace between them", () => {
  expect(cardsShown("dispatch://CORE-1 dispatch://CORE-2", resolved)).toEqual([
    "/issues/CORE-1",
    "/issues/CORE-2",
  ]);
  expect(cardsShown("CORE-12\n#q2\thttps://dispatch.test/issues/CORE-2", resolved)).toEqual([
    "/issues/CORE-12",
    "/issues/CORE-1/asks/ask-2",
    "/issues/CORE-2",
  ]);
});

test("Unfurl shows no card for a body with a key or #id that names nothing", () => {
  // A key whose issue does not exist, or has not answered yet, is text.
  expect(cardsShown("CORE-999999", resolved)).toBeNull();
  expect(cardsShown("CORE-12", nothingResolved)).toBeNull();
  // A #id no question of the body's document holds.
  expect(cardsShown("#q9", resolved)).toBeNull();
  expect(cardsShown("#q2", nothingResolved)).toBeNull();
  // One unresolved word makes the whole body text.
  expect(cardsShown("dispatch://CORE-1 CORE-999999", resolved)).toBeNull();
});

test("Unfurl shows no card for a body with anything beside its references", () => {
  expect(cardsShown("See dispatch://CORE-1", resolved)).toBeNull();
  expect(cardsShown("dispatch://CORE-1 for details", resolved)).toBeNull();
  expect(cardsShown("Blocked on CORE-12", resolved)).toBeNull();
  expect(cardsShown("CORE-12.", resolved)).toBeNull();
  expect(cardsShown("**CORE-12**", resolved)).toBeNull();
  expect(cardsShown("#q2_", resolved)).toBeNull();
  expect(cardsShown("See **https://github.com/owner/repository/pull/2**.", resolved)).toBeNull();
  // An address that names nothing Dispatch or GitHub has is not a reference.
  expect(cardsShown("https://example.com/notes", resolved)).toBeNull();
  expect(cardsShown("dispatch://", resolved)).toBeNull();
  expect(cardsShown("", resolved)).toBeNull();
  expect(cardsShown("   ", resolved)).toBeNull();
});

// Every comment body and ask question passes this check as it renders, and a comment holds up to
// DISPATCH_BODY_MAX UTF-16 units. A body of repeated schemes splits into references in
// exponentially many ways, and a pattern that tried them ran for minutes in V8 on one of these
// bodies, hanging every reader's tab; JavaScriptCore, which runs these tests, gives up after a
// second or so per body.
test(`Unfurl reads ${DISPATCH_BODY_MAX}-character bodies in one pass`, () => {
  const start = performance.now();
  for (const scheme of ["http://", "https://", "dispatch://"]) {
    // The repeat leaves room for the two units the ` x` and `\nx` cases append, so every body
    // here is at or just under the cap.
    const references = scheme.repeat(Math.floor((DISPATCH_BODY_MAX - 2) / scheme.length));
    expect(cardsShown(`${references} x`, resolved)).toBeNull();
    expect(cardsShown(`${references}\nx`, resolved)).toBeNull();
    // One word that names nothing.
    expect(cardsShown(references, resolved)).toBeNull();
  }
  const word = "CORE-12 ";
  const keys = word.repeat(Math.floor(DISPATCH_BODY_MAX / word.length));
  expect(cardsShown(keys, resolved)).toEqual(["/issues/CORE-12"]);
  expect(cardsShown(`${keys.slice(0, -1)}x`, resolved)).toBeNull();
  expect(performance.now() - start).toBeLessThan(2000);
});

test("Unfurl shows a dashboard address whose issue does not exist as written on its card", async () => {
  const getIssue = spyOn(api, "getIssue").mockRejectedValue(notFound());
  const address = "https://dispatch.test/issues/CORE-999999";
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <Unfurl body={address} />
    </QueryClientProvider>
  );

  try {
    await waitFor(() => expect(getIssue).toHaveBeenCalledWith("CORE-999999"));
    await waitFor(() => expect(queryClient.isFetching()).toBe(0));
    const card = within(view.container).getByRole("link");
    expect(card.getAttribute("href")).toBe("/issues/CORE-999999");
    expect(card.textContent).toBe(address);
  } finally {
    getIssue.mockRestore();
    view.unmount();
  }
});

test("Unfurl shows no card, and asks GitHub nothing, for references with text beside them", async () => {
  const githubRest = spyOn(api, "githubRest");
  const getIssue = spyOn(api, "getIssue").mockResolvedValue({
    artifacts: [],
    key: "CORE-12",
    status: "todo",
    title: "Fix the thing",
  } as never);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <Unfurl body={"See **https://github.com/owner/repository/pull/2**, after CORE-12."} />
    </QueryClientProvider>
  );

  try {
    // Text beside the references means no card whatever the key names, so the card asks nothing:
    // not the key's existence (the body's own MarkdownBody asks that), and not GitHub.
    await Bun.sleep(20);
    expect(view.container.querySelector("section")).toBeNull();
    expect(getIssue).not.toHaveBeenCalled();
    expect(githubRest).not.toHaveBeenCalled();
  } finally {
    githubRest.mockRestore();
    getIssue.mockRestore();
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

function notFound(): ApiError {
  return new ApiError(404, { code: "NOT_FOUND", error: "not found" });
}

const commentSource: ReferenceSource = { id: "comment-1", kind: "comment" };

test("Unfurl shows the issue's card for a body that is one existing bare key", async () => {
  const getIssue = spyOn(api, "getIssue").mockResolvedValue({
    artifacts: [],
    key: "CORE-12",
    status: "todo",
    title: "Fix the thing",
  } as never);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <Unfurl body=" CORE-12 " source={commentSource} />
    </QueryClientProvider>
  );

  try {
    const card = await within(view.container).findByRole("link", {
      name: /^CORE-12 Fix the thing/,
    });
    expect(card.getAttribute("href")).toBe("/issues/CORE-12");
    expect(getIssue).toHaveBeenCalledWith("CORE-12");
  } finally {
    getIssue.mockRestore();
    view.unmount();
  }
});

test("Unfurl shows no card for a bare key whose issue does not exist", async () => {
  const getIssue = spyOn(api, "getIssue").mockRejectedValue(notFound());
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <Unfurl body="CORE-999999" source={commentSource} />
    </QueryClientProvider>
  );

  try {
    await waitFor(() => expect(getIssue).toHaveBeenCalledWith("CORE-999999"));
    await Bun.sleep(20);
    expect(view.container.querySelector("section")).toBeNull();
  } finally {
    getIssue.mockRestore();
    view.unmount();
  }
});

test("Unfurl shows the question's card for a #id its source's document holds", async () => {
  const getReferenceQuestions = spyOn(api, "getReferenceQuestions").mockResolvedValue({
    document: "dispatch://CORE-1/spec",
    questions: [{ ask: "dispatch://CORE-1/ask/ask-2", block_id: "q2" }],
  });
  const getAsk = spyOn(api, "getAsk").mockResolvedValue({
    answers: [],
    ask: { id: "ask-2", issue_key: "CORE-1", question: "Ship the docs too?" },
    edits: [],
    followers: [],
    replies: [],
  } as never);
  // The ask's own issue, which the card's target reads beside the ask.
  const getIssue = spyOn(api, "getIssue").mockRejectedValue(notFound());
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <Unfurl body="#q2" source={commentSource} />
    </QueryClientProvider>
  );

  try {
    const card = await within(view.container).findByRole("link", { name: "Ship the docs too?" });
    expect(card.getAttribute("href")).toBe("/issues/CORE-1/asks/ask-2");
    expect(getReferenceQuestions).toHaveBeenCalledWith(commentSource);
  } finally {
    getAsk.mockRestore();
    getIssue.mockRestore();
    getReferenceQuestions.mockRestore();
    view.unmount();
  }
});

test("Unfurl shows no card for a #id when the body names no source", async () => {
  const getReferenceQuestions = spyOn(api, "getReferenceQuestions");
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <Unfurl body="#q2" />
    </QueryClientProvider>
  );

  try {
    await Bun.sleep(20);
    expect(view.container.querySelector("section")).toBeNull();
    expect(getReferenceQuestions).not.toHaveBeenCalled();
  } finally {
    getReferenceQuestions.mockRestore();
    view.unmount();
  }
});

test("Unfurl shows one card per reference for a body of several references", async () => {
  const getIssue = spyOn(api, "getIssue").mockImplementation(
    async (key: string) =>
      ({ artifacts: [], key, status: "todo", title: `Title of ${key}` }) as never
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <Unfurl body={"CORE-12\ndispatch://CORE-13 CORE-12"} source={commentSource} />
    </QueryClientProvider>
  );

  try {
    const section = await within(view.container).findByRole("region", { name: "References" });
    await within(section).findByRole("link", { name: /^CORE-13 Title of CORE-13/ });
    // CORE-12, written twice, is one card.
    expect(
      within(section)
        .getAllByRole("link")
        .map((card) => card.getAttribute("href"))
    ).toEqual(["/issues/CORE-12", "/issues/CORE-13"]);
  } finally {
    getIssue.mockRestore();
    view.unmount();
  }
});
