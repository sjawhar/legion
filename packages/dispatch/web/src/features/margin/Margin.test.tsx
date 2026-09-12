import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { type ReactNode, useState } from "react";
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
  project: "CORE",
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
  rank: "U",
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
    mark_id: "m-1",
    orphaned: false,
    quote: "brown",
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
  resolved_by: null,
  resolved_at: null,
  edited_at: null,
  suggestion: null,
};

const replyToAnchoredComment: Comment = {
  ...comment,
  anchor: null,
  body: "Can you clarify?",
  id: "comment-reply-1",
  reply_to: comment.id,
};

const unanchoredRootComment: Comment = {
  ...comment,
  anchor: null,
  body: "Issue-level comment.",
  id: "comment-unanchored-root",
};

const anchoredAsk: Ask = {
  anchor: {
    artifact_id: "artifact-1",
    mark_id: "m-2",
    orphaned: false,
    quote: "Review",
    version: 1,
  },
  answer: null,
  author: { id: "session-1", kind: "session" },
  created_at: "2026-09-09T00:00:00Z",
  edited_at: null,
  id: "ask-1",
  issue_key: "CORE-1",
  kind: "question",
  multiple: false,
  opened_event_id: 1,
  options: [{ label: "Ship" }],
  question: "Should this ship?",
  state: "open",
  urgency: "med",
};

const unanchoredAsk: Ask = {
  ...anchoredAsk,
  anchor: null,
  id: "ask-unanchored",
  question: "Approve the release?",
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

const markComposerAnchor = { artifact: "artifact-1", mark_id: "m-1", quote: "selected" };

function MarkComposerProbe(): ReactNode {
  const { composeForMark } = useMargin();
  const [firstOutcome, setFirstOutcome] = useState("idle");
  const [secondOutcome, setSecondOutcome] = useState("idle");

  const compose = (anchor: typeof markComposerAnchor, setOutcome: (outcome: string) => void) => {
    void composeForMark({ anchor, kind: "comment" }).then(
      () => setOutcome("saved"),
      (error: unknown) => setOutcome(error instanceof Error ? error.message : String(error))
    );
  };

  return (
    <>
      <button onClick={() => compose(markComposerAnchor, setFirstOutcome)} type="button">
        Compose first
      </button>
      <button
        onClick={() =>
          compose({ artifact: "artifact-1", mark_id: "m-2", quote: "second" }, setSecondOutcome)
        }
        type="button"
      >
        Compose second
      </button>
      <output aria-label="First composer outcome">{firstOutcome}</output>
      <output aria-label="Second composer outcome">{secondOutcome}</output>
    </>
  );
}

function OpenAskComposerButton(): ReactNode {
  const { composeForMark } = useMargin();

  return (
    <button
      onClick={() => {
        void composeForMark({ anchor: markComposerAnchor, kind: "ask" }).catch(() => {});
      }}
      type="button"
    >
      Open ask composer
    </button>
  );
}

function FocusMarkButton(): ReactNode {
  const { focusItemForMark } = useMargin();

  return (
    <button onClick={() => focusItemForMark("m-1")} type="button">
      Focus mark
    </button>
  );
}

function DocumentBridgeButton({
  focusMark,
  setActiveMarks,
}: {
  focusMark: (markId: string) => void;
  setActiveMarks: (markIds: readonly string[]) => void;
}): ReactNode {
  const { registerDocument } = useMargin();

  return (
    <button onClick={() => registerDocument({ focusMark, setActiveMarks })} type="button">
      Register document bridge
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
  queryClient.setQueryData(["asks", issue.key], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], []);

  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: "CORE-1", kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <OpenAskComposerButton />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByText("No comments, asks, or suggestions on this document.");
    fireEvent.click(screen.getByRole("button", { name: "Open ask composer" }));
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
  const listIssueAsks = spyOn(api, "listIssueAsks").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const listComments = spyOn(api, "listComments").mockResolvedValue([]);

  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: "CORE-1", kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <OpenAskComposerButton />
          <NavigateToSecondIssue />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByText("No comments, asks, or suggestions on this document.");
    fireEvent.click(screen.getByRole("button", { name: "Open ask composer" }));
    await screen.findByLabelText("Ask composer");
    fireEvent.click(screen.getByRole("button", { name: "Open second issue" }));

    await waitFor(() => expect(screen.queryByLabelText("Ask composer")).toBeNull());
  } finally {
    view.unmount();
    getIssue.mockRestore();
    getInbox.mockRestore();
    listIssueAsks.mockRestore();
    getMyState.mockRestore();
    listComments.mockRestore();
  }
});

test("a desktop comment deep link activates Comments and scrolls its card from Pinned", async () => {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const listIssueAsks = spyOn(api, "listIssueAsks").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const listComments = spyOn(api, "listComments").mockResolvedValue([comment]);
  const scrollTo = spyOn(HTMLElement.prototype, "scrollTo").mockImplementation(() => {});
  const originalMatchMedia = window.matchMedia;
  window.matchMedia = (() =>
    ({
      addEventListener: () => {},
      addListener: () => {},
      dispatchEvent: () => true,
      matches: false,
      media: "",
      onchange: null,
      removeEventListener: () => {},
      removeListener: () => {},
    }) as MediaQueryList) as typeof window.matchMedia;

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
    window.matchMedia = originalMatchMedia;
    getIssue.mockRestore();
    getInbox.mockRestore();
    listIssueAsks.mockRestore();
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
  const listIssueAsks = spyOn(api, "listIssueAsks").mockResolvedValue([]);
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
    listIssueAsks.mockRestore();
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
  const listIssueAsks = spyOn(api, "listIssueAsks").mockResolvedValue([]);
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
    listIssueAsks.mockRestore();
    getMyState.mockRestore();
    listComments.mockRestore();
  }
});

test("Margin surfaces and retries a failed fetch for the issue's asks", async () => {
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], [comment]);
  const listIssueAsks = spyOn(api, "listIssueAsks")
    .mockRejectedValueOnce(new Error("offline"))
    .mockResolvedValue([anchoredAsk]);
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
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("Could not load this document's asks.");
    expect(screen.getByTestId("margin-comment-comment-1")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await screen.findByTestId("ask-ask-1");
    expect(listIssueAsks).toHaveBeenCalledTimes(2);
  } finally {
    view.unmount();
    listIssueAsks.mockRestore();
  }
});

test("Margin puts an unanchored open ask under Needs you and sends its answer", async () => {
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  const answeredAsk: Ask = {
    ...unanchoredAsk,
    answer: {
      at: "2026-09-10T00:01:00Z",
      selected: ["Ship"],
      text: null,
      user: "alice",
    },
    state: "answered",
  };
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], [unanchoredAsk]);
  queryClient.setQueryData(["asks", issue.key], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], []);
  const answerAsk = spyOn(api, "answerAsk").mockResolvedValue(answeredAsk);
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const needsYou = await screen.findByRole("region", { name: "Needs you" });
    expect(await within(needsYou).findByText(unanchoredAsk.question)).toBeTruthy();
    const shipOption = await within(needsYou).findByRole("radio", { name: "Ship" });
    fireEvent.click(shipOption);
    expect((shipOption as HTMLInputElement).checked).toBe(true);
    fireEvent.click(within(needsYou).getByRole("button", { name: "Answer" }));
    await waitFor(() =>
      expect(answerAsk).toHaveBeenCalledWith(unanchoredAsk.id, { selected: ["Ship"] })
    );
  } finally {
    view.unmount();
    answerAsk.mockRestore();
  }
});

test("Margin clears an answered anchored ask from Needs you without an event stream", async () => {
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  const answeredAsk: Ask = {
    ...anchoredAsk,
    answer: {
      at: "2026-09-10T00:01:00Z",
      selected: ["Ship"],
      text: null,
      user: "alice",
    },
    state: "answered",
  };
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], [anchoredAsk]);
  queryClient.setQueryData(["asks", issue.key], [anchoredAsk]);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], []);
  const answerAsk = spyOn(api, "answerAsk").mockResolvedValue(answeredAsk);
  const listIssueAsks = spyOn(api, "listIssueAsks").mockResolvedValue([answeredAsk]);
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const needsYou = await screen.findByRole("region", { name: "Needs you" });
    fireEvent.click(await within(needsYou).findByRole("radio", { name: "Ship" }));
    fireEvent.click(within(needsYou).getByRole("button", { name: "Answer" }));
    await waitFor(() =>
      expect(answerAsk).toHaveBeenCalledWith(anchoredAsk.id, { selected: ["Ship"] })
    );
    await waitFor(() => expect(screen.queryByRole("region", { name: "Needs you" })).toBeNull());
    expect(screen.getByRole("button", { name: "Open review panel (0 open asks)" })).toBeTruthy();
    expect(answerAsk).toHaveBeenCalledWith(anchoredAsk.id, { selected: ["Ship"] });
  } finally {
    view.unmount();
    answerAsk.mockRestore();
    listIssueAsks.mockRestore();
  }
});

test("Margin leaves an unanchored root out of document review", async () => {
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["asks", issue.key], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], [unanchoredRootComment]);
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await waitFor(() =>
      expect(screen.queryByTestId(`margin-comment-${unanchoredRootComment.id}`)).toBeNull()
    );
  } finally {
    view.unmount();
  }
});

test("Margin replies to an agent's anchored reply with the thread root id and no anchor", async () => {
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  const agentRootComment: Comment = {
    ...comment,
    author: { id: "s1", kind: "session" },
  };
  const agentReply: Comment = {
    ...replyToAnchoredComment,
    author: { id: "s1", kind: "session" },
    reply_to: agentRootComment.id,
  };
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["asks", issue.key], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], [agentRootComment, agentReply]);
  const createComment = spyOn(api, "createComment").mockResolvedValue({
    ...agentReply,
    body: "Nested reply.",
    id: "comment-reply-2",
    reply_to: agentRootComment.id,
  });
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const threadCard = await screen.findByTestId(`margin-comment-${agentRootComment.id}`);
    fireEvent.click(within(threadCard).getByRole("button"));
    const composer = await screen.findByRole("form", { name: "Reply composer" });
    expect(composer.querySelector("blockquote")).toBeNull();
    fireEvent.change(within(composer).getByLabelText("Reply"), {
      target: { value: "Nested reply." },
    });
    fireEvent.click(within(composer).getByRole("button", { name: "Reply" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    const payload = createComment.mock.calls[0]?.[1];
    expect(payload).toEqual({
      body: "Nested reply.",
      reply_to: agentRootComment.id,
    });
    expect(Object.keys(payload ?? {}).sort()).toEqual(["body", "reply_to"]);
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

test("a viewer who mounts after the answer sees the answered anchored ask", async () => {
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  const answeredAsk: Ask = {
    anchor: {
      artifact_id: "artifact-1",
      mark_id: "m-3",
      orphaned: false,
      quote: "brown",
      version: 1,
    },
    answer: {
      at: "2026-09-10T00:01:00Z",
      selected: [],
      text: "Because it is precise.",
      user: "alice",
    },
    author: { id: "session-1", kind: "session" },
    created_at: "2026-09-10T00:00:00Z",
    edited_at: null,
    id: "ask-answered",
    issue_key: issue.key,
    kind: "question",
    opened_event_id: 2,
    multiple: false,
    options: [],
    question: "Why brown?",
    state: "answered",
    urgency: "med",
  };
  // Open asks remain in the Needs you group before the historical answered ask, regardless of
  // their document positions.
  const openAsk: Ask = {
    anchor: {
      artifact_id: "artifact-1",
      mark_id: "m-4",
      orphaned: false,
      quote: "The",
      version: 1,
    },
    answer: null,
    author: { id: "session-1", kind: "session" },
    created_at: "2026-09-10T00:02:00Z",
    edited_at: null,
    id: "ask-open",
    issue_key: issue.key,
    opened_event_id: 3,
    multiple: false,
    options: [],
    question: "Why the?",
    state: "open",
    kind: "question",
    urgency: "med",
  };
  // This viewer's tab never had either ask open in its inbox or query cache - unlike a tab
  // that was present when they were created, both queries start empty.
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], []);
  const listIssueAsks = spyOn(api, "listIssueAsks").mockResolvedValue([answeredAsk, openAsk]);
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const card = await screen.findByTestId(`ask-${answeredAsk.id}`);
    expect(listIssueAsks).toHaveBeenCalledWith(issue.key, "all");
    expect(card.textContent).toContain("Answered by alice");
    await waitFor(() => expect(card.textContent).toContain("Because it is precise."));
    expect(
      Array.from(
        screen
          .getByLabelText("Margin review items")
          .querySelectorAll<HTMLElement>("[data-margin-item]")
      ).map((item) => item.dataset.marginItem)
    ).toEqual([openAsk.id, answeredAsk.id]);
  } finally {
    view.unmount();
    listIssueAsks.mockRestore();
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
    resolved_by: null,
    resolved_at: null,
    edited_at: null,
    suggestion: null,
  };
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["asks", issue.key], [anchoredAsk]);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], [askReply]);
  const getAsk = spyOn(api, "getAsk").mockResolvedValue({
    ask: anchoredAsk,
    edits: [],
    replies: [askReply],
  });
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
    await waitFor(() => expect(screen.getAllByText("Any blockers first?")).toHaveLength(1));
    expect(screen.queryByTestId(`margin-comment-${askReply.id}`)).toBeNull();
  } finally {
    view.unmount();
    getAsk.mockRestore();
  }
});

test("composeForMark opens the composer with the mark anchor and resolves when it saves", async () => {
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["asks", issue.key], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], []);
  const createComment = spyOn(api, "createComment").mockResolvedValue(undefined as never);
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <MarkComposerProbe />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    fireEvent.click(screen.getByRole("button", { name: "Compose first" }));
    const composer = await screen.findByRole("form", { name: "Comment composer" });
    expect(within(composer).getByText("selected")).toBeTruthy();
    fireEvent.change(within(composer).getByLabelText("Comment"), { target: { value: "why?" } });
    fireEvent.click(within(composer).getByRole("button", { name: "Comment" }));

    await waitFor(() => {
      expect(screen.getByLabelText("First composer outcome").textContent).toBe("saved");
      expect(screen.queryByRole("form", { name: "Comment composer" })).toBeNull();
    });
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

test("replying inside a thread leaves a pending mark composer open", async () => {
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["asks", issue.key], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], [comment]);
  const createComment = spyOn(api, "createComment").mockResolvedValue(undefined as never);
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <MarkComposerProbe />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    fireEvent.click(screen.getByRole("button", { name: "Compose first" }));
    await screen.findByRole("form", { name: "Comment composer" });
    const threadCard = await screen.findByTestId(`margin-comment-${comment.id}`);
    fireEvent.click(within(threadCard).getByRole("button"));
    const replyComposer = await screen.findByRole("form", { name: "Reply composer" });
    expect(screen.getByLabelText("First composer outcome").textContent).toBe("idle");
    fireEvent.change(within(replyComposer).getByLabelText("Reply"), {
      target: { value: "reply" },
    });
    fireEvent.click(within(replyComposer).getByRole("button", { name: "Reply" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    expect(screen.getByLabelText("First composer outcome").textContent).toBe("idle");
    const phoneThread = screen.queryByRole("dialog", { name: "Thread" });
    if (phoneThread !== null) {
      fireEvent.click(within(phoneThread).getByRole("button", { name: "Back" }));
    }
    expect(screen.getByRole("form", { name: "Comment composer" })).not.toBeNull();
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

test("composeForMark rejects when the composer is dismissed unsaved", async () => {
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["asks", issue.key], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], []);
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <MarkComposerProbe />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    fireEvent.click(screen.getByRole("button", { name: "Compose first" }));
    await screen.findByRole("form", { name: "Comment composer" });
    fireEvent.click(screen.getByRole("button", { name: "Compose second" }));
    await waitFor(() =>
      expect(screen.getByLabelText("First composer outcome").textContent).toBe(
        "replaced by a newer composer"
      )
    );
    const composer = screen.getByRole("form", { name: "Comment composer" });
    expect(within(composer).getByText("second")).toBeTruthy();
    fireEvent.keyDown(within(composer).getByLabelText("Comment"), { key: "Escape" });
    await waitFor(() => {
      expect(screen.getByLabelText("Second composer outcome").textContent).toBe("composer closed");
      expect(screen.queryByRole("form", { name: "Comment composer" })).toBeNull();
    });
  } finally {
    view.unmount();
  }
});

test("focusItemForMark opens the matching thread in the compact sheet", async () => {
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["asks", issue.key], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], [comment]);
  const originalMatchMedia = window.matchMedia;
  window.matchMedia = (() =>
    ({
      addEventListener: () => {},
      addListener: () => {},
      dispatchEvent: () => true,
      matches: true,
      media: "",
      onchange: null,
      removeEventListener: () => {},
      removeListener: () => {},
    }) as MediaQueryList) as typeof window.matchMedia;
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <FocusMarkButton />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const card = await screen.findByTestId(`margin-comment-${comment.id}`);
    fireEvent.click(screen.getByRole("button", { name: "Focus mark" }));
    await waitFor(() => {
      const phoneThread = screen.queryByRole("dialog", { name: "Thread" });
      const focusedCard =
        phoneThread === null
          ? card
          : within(phoneThread).getByTestId(`margin-comment-${comment.id}`);
      expect(focusedCard.getAttribute("aria-current")).toBe("true");
      expect(screen.getByTestId("margin-sheet").getAttribute("data-expanded")).toBe("true");
    });
  } finally {
    view.unmount();
    window.matchMedia = originalMatchMedia;
  }
});

test("selecting or hovering a card drives the document bridge", async () => {
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["asks", issue.key], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], [comment]);
  const focusedMarks: string[] = [];
  const activeMarkCalls: Array<readonly string[]> = [];
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <DocumentBridgeButton
            focusMark={(markId) => focusedMarks.push(markId)}
            setActiveMarks={(markIds) => activeMarkCalls.push(markIds)}
          />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const card = await screen.findByTestId(`margin-comment-${comment.id}`);
    fireEvent.click(screen.getByRole("button", { name: "Register document bridge" }));
    fireEvent.click(within(card).getByRole("button"));
    await waitFor(() => expect(focusedMarks).toEqual(["m-1"]));
    fireEvent.mouseOver(card);
    await waitFor(() => expect(activeMarkCalls).toContainEqual(["m-1"]));
  } finally {
    view.unmount();
  }
});

test("Margin links an orphaned ask to its original document version", async () => {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const orphanedAsk: Ask = {
    ...anchoredAsk,
    anchor: {
      artifact_id: "artifact-1",
      mark_id: "m-2",
      orphaned: true,
      quote: "Review",
      version: 1,
    },
  };
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], [orphanedAsk]);
  queryClient.setQueryData(["asks", issue.key], [orphanedAsk]);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], []);
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const link = await screen.findByRole("link", { name: "View original text" });
    expect(link.getAttribute("href")).toBe("/issues/CORE-1/artifacts/spec?v=1&ask=ask-1");
  } finally {
    view.unmount();
  }
});
