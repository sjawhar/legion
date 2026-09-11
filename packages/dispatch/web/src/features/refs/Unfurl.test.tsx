import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";

import { api } from "../../api/client";
import type { Artifact, AskRead, CommentRead, IssueDetails } from "../../api/types";
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
    render(
      <QueryClientProvider client={queryClient}>
        <Unfurl body={"See https://github.com/owner/repository/issues/1."} />
      </QueryClientProvider>
    );

    await waitFor(() => {
      expect(githubRest).toHaveBeenCalledWith("/repos/owner/repository/issues/1");
    });
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
    primary_artifact_id: artifact.id,
    project: "CORE",
    route: null,
    status: "testing",
    title: "Design decision",
    open_asks: [],
    updated_at: "2026-09-09T00:00:00Z",
  };
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  const getArtifactText = spyOn(api, "getArtifactText").mockResolvedValue({
    markdown: "Live document",
    version: null,
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
    render(
      <QueryClientProvider client={queryClient}>
        <Unfurl body="See dispatch://CORE-1/artifact/design@v3" />
      </QueryClientProvider>
    );

    await screen.findByText("Version three");
    expect(getArtifactVersion).toHaveBeenCalledWith("artifact-1", 3);
    expect(getArtifactText).not.toHaveBeenCalled();
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
    referenced_by: [],
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
    render(
      <QueryClientProvider client={queryClient}>
        <Unfurl body="See dispatch://CORE/artifact/design-notes@v3" />
      </QueryClientProvider>
    );

    await screen.findByText("Project version three");
    expect(getProjectArtifact).toHaveBeenCalledWith("CORE", "design-notes");
    expect(getIssue).not.toHaveBeenCalled();
    expect(screen.getByRole("link", { name: /Design notes/ }).getAttribute("href")).toBe(
      "/projects/CORE/documents/design-notes?version=3"
    );
  } finally {
    getIssue.mockRestore();
    getArtifactVersion.mockRestore();
    getProjectArtifact.mockRestore();
  }
});

test("Unfurl unfurls a dispatch ask reference with the question, not the issue title", async () => {
  const issue: IssueDetails = {
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
    primary_artifact_id: "artifact-none",
    project: "CORE",
    route: null,
    status: "testing",
    title: "Design decision",
    open_asks: [],
    updated_at: "2026-09-09T00:00:00Z",
  };
  const askRead: AskRead = {
    ask: {
      id: "ask-1",
      issue_key: "CORE-1",
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
    replies: [],
  };
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
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
    expect(within(view.container).queryByText("Design decision")).toBeNull();
    expect(getAsk).toHaveBeenCalledWith("ask-1");
  } finally {
    getAsk.mockRestore();
    getIssue.mockRestore();
    view.unmount();
  }
});

test("Unfurl unfurls a dispatch comment reference with its first line, not the issue title", async () => {
  const issue: IssueDetails = {
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
    primary_artifact_id: "artifact-none",
    project: "CORE",
    route: null,
    status: "testing",
    title: "Design decision",
    open_asks: [],
    updated_at: "2026-09-09T00:00:00Z",
  };
  const commentRead: CommentRead = {
    comment: {
      id: "comment-1",
      issue_key: "CORE-1",
      author: { id: "alice", kind: "user" },
      body: "Looks good overall.\nOne nit below.",
      anchor: null,
      reply_to: null,
      ask_id: null,
      resolved: false,
      resolved_by: null,
      resolved_at: null,
      edited_at: null,
      suggestion: null,
      created_at: "2026-09-09T00:00:00Z",
    },
    replies: [],
  };
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  const getComment = spyOn(api, "getComment").mockResolvedValue(commentRead);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  const view = render(
    <QueryClientProvider client={queryClient}>
      <Unfurl body="See dispatch://CORE-1/comment/comment-1" />
    </QueryClientProvider>
  );

  try {
    await within(view.container).findByText("Looks good overall.");
    expect(within(view.container).queryByText("Design decision")).toBeNull();
    expect(getComment).toHaveBeenCalledWith("comment-1");
  } finally {
    getComment.mockRestore();
    getIssue.mockRestore();
    view.unmount();
  }
});

test("isBareReferenceBody accepts only whitespace-joined bare references", () => {
  expect(isBareReferenceBody("dispatch://CORE-1")).toBe(true);
  expect(isBareReferenceBody("  dispatch://CORE-1  ")).toBe(true);
  expect(isBareReferenceBody("dispatch://CORE-1 dispatch://CORE-2")).toBe(true);
  expect(isBareReferenceBody("See dispatch://CORE-1")).toBe(false);
  expect(isBareReferenceBody("dispatch://CORE-1 for details")).toBe(false);
  expect(isBareReferenceBody("")).toBe(false);
});
