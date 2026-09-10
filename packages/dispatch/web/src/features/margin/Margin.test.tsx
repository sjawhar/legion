import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter, useNavigate } from "react-router-dom";

import { api } from "../../api/client";
import type { Artifact, Ask, Comment, IssueDetails } from "../../api/types";
import { buildIssuePath } from "../refs/routes";
import { Margin, MarginProvider, useMargin } from "./Margin";

const specArtifact: Artifact = {
  created_at: "2026-09-09T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-1",
  issue_key: "CORE-1",
  kind: "doc",
  name: "spec.md",
  primary: true,
  slug: "spec",
  versions: [],
};

const issue: IssueDetails = {
  artifacts: [specArtifact],
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
  primary_artifact_id: "artifact-1",
  project: "CORE",
  route: null,
  status: "open",
  title: "Review the spec",
  open_asks: [],
  updated_at: "2026-09-09T00:00:00Z",
};

const secondIssue: IssueDetails = {
  ...issue,
  artifacts: [
    {
      ...specArtifact,
      id: "artifact-2",
      issue_key: "CORE-2",
    },
  ],
  key: "CORE-2",
  number: 2,
  primary_artifact_id: "artifact-2",
  title: "Second issue",
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
  ask_id: null,
  issue_key: "CORE-1",
  reply_to: null,
  resolved: false,
  suggestion: null,
};

const anchoredAsk: Ask = {
  anchor: {
    artifact_id: "artifact-1",
    from: 0,
    orphaned: false,
    quote: "Review",
    to: 6,
    version: 1,
  },
  answer: null,
  author: { id: "session-1", kind: "session" },
  created_at: "2026-09-09T00:00:00Z",
  id: "ask-1",
  issue_key: "CORE-1",
  multiple: false,
  options: [{ label: "Ship" }],
  question: "Should this ship?",
  state: "open",
  urgency: "med",
};

function CommentLink(): ReactNode {
  const navigate = useNavigate();

  return (
    <button
      onClick={() => navigate(buildIssuePath({ id: "comment-1", key: "CORE-1", kind: "comment" }))}
      type="button"
    >
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

function NavigateToIssue(): ReactNode {
  const navigate = useNavigate();

  return (
    <button
      onClick={() => navigate(buildIssuePath({ key: "CORE-1", kind: "issue" }))}
      type="button"
    >
      Open issue
    </button>
  );
}

function NavigateToSecondIssue(): ReactNode {
  const navigate = useNavigate();

  return (
    <button
      onClick={() => navigate(buildIssuePath({ key: "CORE-2", kind: "issue" }))}
      type="button"
    >
      Open second issue
    </button>
  );
}

function SelectedItemLabel(): ReactNode {
  const { selectedItemId } = useMargin();

  return <output aria-label="Selected margin item">{selectedItemId ?? "none"}</output>;
}

test("Margin hides an open composer when its issue closes", async () => {
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], []);

  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: "CORE-1", kind: "issue" })]}>
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

    act(() => {
      queryClient.setQueryData(["issue", issue.key], {
        ...issue,
        closed_at: "2026-09-09T01:00:00Z",
      });
    });

    await waitFor(() => expect(screen.queryByLabelText("Ask composer")).toBeNull());
  } finally {
    view.unmount();
  }
});

test("Margin hides an open composer when navigating to a different artifact", async () => {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["issue", secondIssue.key], secondIssue);
  const getIssue = spyOn(api, "getIssue").mockImplementation(async (key) =>
    key === "CORE-2" ? secondIssue : issue
  );
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const listComments = spyOn(api, "listComments").mockResolvedValue([]);

  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: "CORE-1", kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <SelectionButton />
          <NavigateToSecondIssue />
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
    fireEvent.click(screen.getByRole("button", { name: "Open second issue" }));

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
    <MemoryRouter initialEntries={[buildIssuePath({ key: "CORE-1", kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <CommentLink />
          <SelectedItemLabel />
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
    const card = screen.getByTestId("margin-comment-comment-1");
    fireEvent.click(card);
    expect(screen.getByLabelText("Selected margin item").textContent).toBe("comment-1");
  } finally {
    view.unmount();
    getIssue.mockRestore();
    getInbox.mockRestore();
    getMyState.mockRestore();
    listComments.mockRestore();
    scrollTo.mockRestore();
  }
});

test("margin item listeners reattach after the comments tab remounts", async () => {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const listComments = spyOn(api, "listComments").mockResolvedValue([comment]);

  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: "CORE-1", kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <SelectedItemLabel />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByTestId("margin-comment-comment-1");
    fireEvent.click(screen.getByRole("tab", { name: "Pinned" }));
    fireEvent.click(screen.getByRole("tab", { name: "Comments" }));
    const card = await screen.findByTestId("margin-comment-comment-1");
    fireEvent.click(card);

    expect(screen.getByLabelText("Selected margin item").textContent).toBe("comment-1");
  } finally {
    view.unmount();
    getIssue.mockRestore();
    getInbox.mockRestore();
    getMyState.mockRestore();
    listComments.mockRestore();
  }
});

test("margin item listeners attach after navigating from a route with no review list", async () => {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const listComments = spyOn(api, "listComments").mockResolvedValue([comment]);

  const view = render(
    <MemoryRouter initialEntries={["/"]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <NavigateToIssue />
          <SelectedItemLabel />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    expect(screen.getByLabelText("Selected margin item").textContent).toBe("none");
    fireEvent.click(screen.getByRole("button", { name: "Open issue" }));
    const card = await screen.findByTestId("margin-comment-comment-1");
    fireEvent.click(card);

    expect(screen.getByLabelText("Selected margin item").textContent).toBe("comment-1");
  } finally {
    view.unmount();
    getIssue.mockRestore();
    getInbox.mockRestore();
    getMyState.mockRestore();
    listComments.mockRestore();
  }
});

test("Margin surfaces and retries a failed fetch for an answered anchored ask", async () => {
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], [anchoredAsk]);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], [comment]);
  // AskCard's own reply-thread query (`["ask-thread", id]`) also calls `api.getAsk` as soon as
  // the card mounts, so the initial resolved value must cover that fetch before the
  // reject/retry sequence below exercises `useAnsweredAsks`'s missing-ask fallback query.
  const getAsk = spyOn(api, "getAsk").mockResolvedValue({ ask: anchoredAsk, replies: [] });
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: "CORE-1", kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByTestId("ask-ask-1");
    await waitFor(() => expect(getAsk).toHaveBeenCalledTimes(1));
    getAsk.mockRejectedValueOnce(new Error("offline")).mockResolvedValue({
      ask: {
        ...anchoredAsk,
        answer: { at: "2026-09-09T00:05:00Z", selected: ["Ship"], text: null, user: "alice" },
        state: "answered",
      },
      replies: [],
    });
    act(() => {
      queryClient.setQueryData(["inbox"], []);
    });

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("Could not load an answered ask.");
    expect(screen.getByTestId("margin-comment-comment-1")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(getAsk).toHaveBeenCalledTimes(3));
    expect((await screen.findByTestId("ask-ask-1")).textContent).toContain("alice answered");
  } finally {
    view.unmount();
    getAsk.mockRestore();
  }
});

test("a reply to an ask renders exactly once in the margin, not also as a standalone comment", async () => {
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  const askReply: Comment = {
    anchor: null,
    ask_id: anchoredAsk.id,
    author: { id: "alice", kind: "user" },
    body: "Any blockers first?",
    created_at: "2026-09-09T00:01:00Z",
    id: "ask-reply-1",
    issue_key: "CORE-1",
    reply_to: null,
    resolved: false,
    suggestion: null,
  };
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], [anchoredAsk]);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], [askReply]);
  const getAsk = spyOn(api, "getAsk").mockResolvedValue({ ask: anchoredAsk, replies: [askReply] });
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: "CORE-1", kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByTestId("ask-ask-1");
    await screen.findByTestId(`thread-${anchoredAsk.id}`);
    expect(screen.getAllByText("Any blockers first?")).toHaveLength(1);
    expect(screen.queryByTestId(`margin-comment-${askReply.id}`)).toBeNull();
  } finally {
    view.unmount();
    getAsk.mockRestore();
  }
});
