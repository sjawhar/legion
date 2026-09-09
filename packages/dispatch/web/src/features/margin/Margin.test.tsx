import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter, useNavigate } from "react-router-dom";

import { api } from "../../api/client";
import type { Comment, Issue } from "../../api/types";
import { Margin, MarginProvider, useMargin } from "./Margin";

const issue: Issue = {
  artifacts: [
    {
      created_at: "2026-09-09T00:00:00Z",
      created_by: { id: "alice", kind: "user" },
      id: "artifact-1",
      issue_key: "CORE-1",
      kind: "doc",
      name: "spec.md",
      primary: true,
      slug: "spec",
      versions: [],
    },
  ],
  closed_at: null,
  created_at: "2026-09-09T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  external_links: [],
  key: "CORE-1",
  labels: [],
  last_seq: 1,
  number: 1,
  parent: null,
  primary_artifact_id: "artifact-1",
  project: "CORE",
  route: null,
  status: "open",
  title: "Review the spec",
  updated_at: "2026-09-09T00:00:00Z",
};

const comment: Comment = {
  anchor: {
    artifact_id: "artifact-1",
    from: 10,
    orphaned: false,
    quote: "brown",
    to: 15,
    version: 1,
  },
  author: { id: "alice", kind: "user" },
  body: "why?",
  created_at: "2026-09-09T00:00:00Z",
  id: "comment-1",
  issue_key: "CORE-1",
  reply_to: null,
  resolved: false,
  suggestion: null,
};

function CommentLink(): ReactNode {
  const navigate = useNavigate();

  return (
    <button onClick={() => navigate("/issues/CORE-1/comments/comment-1")} type="button">
      Open comment
    </button>
  );
}

function SelectionButton(): ReactNode {
  const { setSelection } = useMargin();

  return (
    <button
      onClick={() =>
        setSelection({
          artifact: "artifact-1",
          artifactId: "artifact-1",
          canSuggest: true,
          from: 0,
          quote: "Review",
          rect: { bottom: 0, left: 0, right: 0, top: 0 },
          to: 6,
        })
      }
      type="button"
    >
      Select text
    </button>
  );
}

test("Margin hides an open composer when its issue closes", async () => {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const listComments = spyOn(api, "listComments").mockResolvedValue([]);

  const view = render(
    <MemoryRouter initialEntries={["/issues/CORE-1"]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <SelectionButton />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByText("No comments, asks, or suggestions on this document.");
    fireEvent.click(screen.getByRole("button", { name: "Select text" }));
    fireEvent.click(screen.getByRole("button", { name: "Ask" }));
    await screen.findByLabelText("Ask composer");

    queryClient.setQueryData(["issue", issue.key], {
      ...issue,
      closed_at: "2026-09-09T01:00:00Z",
    });

    await waitFor(() => expect(screen.queryByLabelText("Ask composer")).toBeNull());
  } finally {
    view.unmount();
    getIssue.mockRestore();
    getInbox.mockRestore();
    getMyState.mockRestore();
    listComments.mockRestore();
  }
});

test("a comment deep link activates Comments and scrolls its card from Pinned", async () => {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const listComments = spyOn(api, "listComments").mockResolvedValue([comment]);
  const scrollTo = spyOn(HTMLElement.prototype, "scrollTo").mockImplementation(() => {});

  const view = render(
    <MemoryRouter initialEntries={["/issues/CORE-1"]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <CommentLink />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByTestId("margin-comment-comment-1");
    fireEvent.click(screen.getByRole("tab", { name: "Pinned" }));
    expect(screen.getByRole("tab", { name: "Pinned" }).getAttribute("aria-selected")).toBe("true");

    fireEvent.click(screen.getByRole("button", { name: "Open comment" }));

    await waitFor(() => {
      expect(screen.getByRole("tab", { name: "Comments" }).getAttribute("aria-selected")).toBe(
        "true"
      );
      expect(scrollTo).toHaveBeenCalledTimes(1);
    });
  } finally {
    view.unmount();
    getIssue.mockRestore();
    getInbox.mockRestore();
    getMyState.mockRestore();
    listComments.mockRestore();
    scrollTo.mockRestore();
  }
});
