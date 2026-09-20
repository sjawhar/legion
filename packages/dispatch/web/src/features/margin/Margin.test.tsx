import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { type ReactNode, useState } from "react";
import { MemoryRouter, useNavigate } from "react-router-dom";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { api } from "../../api/client";
import type { Artifact, Ask, Comment, IssueDetails } from "../../api/types";
import { buildIssuePath, buildProjectPath } from "../refs/routes";
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
  assignee: null,
  components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
  primary_artifact_id: "artifact-1",
  project: "CORE",
  route: null,
  status: "open",
  priority: null,
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
    block_id: null,
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
  turn: null,
  issue_key: "CORE-1",
  reply_to: null,
  resolved: false,
  resolved_by: null,
  resolved_at: null,
  edited_at: null,
  suggestion: null,
  ...commentDeliveryFields(),
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
    block_id: null,
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
    await screen.findByText("No pinned items.");
    fireEvent.click(screen.getByRole("button", { name: "Open ask composer" }));
    await screen.findByRole("form", { name: "Comment composer" });

    act(() => {
      queryClient.setQueryData(["issue", issue.key], {
        ...issue,
        closed_at: "2026-09-09T01:00:00Z",
      });
    });

    await waitFor(() =>
      expect(screen.queryByRole("form", { name: "Comment composer" })).toBeNull()
    );
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
    await screen.findByText("No pinned items.");
    fireEvent.click(screen.getByRole("button", { name: "Open ask composer" }));
    await screen.findByRole("form", { name: "Comment composer" });
    fireEvent.click(screen.getByRole("button", { name: "Open second issue" }));

    await waitFor(() =>
      expect(screen.queryByRole("form", { name: "Comment composer" })).toBeNull()
    );
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
    expect(within(needsYou).getAllByRole("time")).toHaveLength(1);
    expect(within(needsYou).queryByLabelText("Your answer")).toBeNull();
    expect(
      within(needsYou).getByText("Add a note or answer in your own words", { exact: true })
    ).toBeTruthy();
    const shipOption = await within(needsYou).findByRole("radio", { name: "Ship" });
    fireEvent.click(shipOption);
    expect((shipOption as HTMLInputElement).checked).toBe(true);
    fireEvent.click(within(needsYou).getByRole("button", { name: "Answer" }));
    await waitFor(() =>
      expect(answerAsk).toHaveBeenCalledWith(unanchoredAsk.id, {
        selected: ["Ship"],
        expected_edited_at: null,
      })
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
      expect(answerAsk).toHaveBeenCalledWith(anchoredAsk.id, {
        selected: ["Ship"],
        expected_edited_at: null,
      })
    );
    await waitFor(() => expect(screen.queryByRole("region", { name: "Needs you" })).toBeNull());
    expect(screen.getByRole("button", { name: "Open review panel (0 open asks)" })).toBeTruthy();
    expect(answerAsk).toHaveBeenCalledWith(anchoredAsk.id, {
      selected: ["Ship"],
      expected_edited_at: null,
    });
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
      block_id: null,
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
      block_id: null,
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
        screen.getByLabelText("Margin asks").querySelectorAll<HTMLElement>("[data-margin-item]")
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
    turn: "agent",
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
    ...commentDeliveryFields(),
  };
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["asks", issue.key], [anchoredAsk]);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], [askReply]);
  const getAsk = spyOn(api, "getAsk").mockResolvedValue({
    ask: anchoredAsk,
    edits: [],
    followers: [],
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
    fireEvent.click(within(composer).getByRole("button", { name: "Send" }));

    await waitFor(() => {
      expect(screen.getByLabelText("First composer outcome").textContent).toBe("saved");
      expect(screen.queryByRole("form", { name: "Comment composer" })).toBeNull();
    });
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

test("Margin links an orphaned ask to its original document version", async () => {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const orphanedAsk: Ask = {
    ...anchoredAsk,
    anchor: {
      artifact_id: "artifact-1",
      block_id: null,
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

test("desktop margin resize handle supports keyboard adjustments and reset", async () => {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["asks", issue.key], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], []);
  const originalMatchMedia = window.matchMedia;
  const originalInnerWidth = window.innerWidth;
  const innerWidthDescriptor = Object.getOwnPropertyDescriptor(window, "innerWidth");
  Object.defineProperty(window, "innerWidth", { configurable: true, value: 1280 });
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
  function ResizableMargin(): ReactNode {
    const [width, setWidth] = useState(384);
    return <Margin onWidthChange={setWidth} width={width} />;
  }
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <ResizableMargin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const handle = await screen.findByRole("separator", { name: "Resize margin" });
    expect(screen.getByTestId("desktop-margin-shell").getAttribute("style")).toContain(
      "width: 384px"
    );

    fireEvent.keyDown(handle, { key: "ArrowLeft" });
    await waitFor(() =>
      expect(screen.getByTestId("desktop-margin-shell").getAttribute("style")).toContain(
        "width: 408px"
      )
    );

    fireEvent.doubleClick(handle);
    await waitFor(() =>
      expect(screen.getByTestId("desktop-margin-shell").getAttribute("style")).toContain(
        "width: 384px"
      )
    );
  } finally {
    view.unmount();
    window.matchMedia = originalMatchMedia;
    if (innerWidthDescriptor === undefined) {
      Object.defineProperty(window, "innerWidth", {
        configurable: true,
        value: originalInnerWidth,
      });
    } else {
      Object.defineProperty(window, "innerWidth", innerWidthDescriptor);
    }
  }
});

test("margin keeps an ask draft through parent, width, and viewport layout updates", async () => {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["asks", issue.key], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], []);
  const originalMatchMedia = window.matchMedia;
  const originalInnerWidth = window.innerWidth;
  const innerWidthDescriptor = Object.getOwnPropertyDescriptor(window, "innerWidth");
  Object.defineProperty(window, "innerWidth", { configurable: true, value: 1280 });
  const changeListeners: Array<(event: Event) => void> = [];
  let compact = false;
  window.matchMedia = ((query: string) =>
    ({
      addEventListener: (type: string, listener: EventListenerOrEventListenerObject | null) => {
        if (type === "change" && typeof listener === "function") {
          changeListeners.push(listener);
        }
      },
      addListener: () => {},
      dispatchEvent: () => true,
      get matches() {
        return query === "(max-width: 1279px)" && compact;
      },
      media: query,
      onchange: null,
      removeEventListener: () => {},
      removeListener: () => {},
    }) as MediaQueryList) as typeof window.matchMedia;
  function MarginHarness(): ReactNode {
    const [parentRender, setParentRender] = useState(0);
    const [width, setWidth] = useState(384);
    return (
      <>
        <button onClick={() => setParentRender((current) => current + 1)} type="button">
          Rerender parent
        </button>
        <output aria-label="Parent render">{parentRender}</output>
        <Margin onWidthChange={setWidth} width={width} />
      </>
    );
  }
  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <OpenAskComposerButton />
          <MarginHarness />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByText("No pinned items.");
    fireEvent.click(screen.getByRole("button", { name: "Open ask composer" }));
    const question = await screen.findByLabelText("Question");
    fireEvent.change(question, { target: { value: "Keep this draft" } });

    fireEvent.click(screen.getByRole("button", { name: "Rerender parent" }));
    expect(screen.getByLabelText("Parent render").textContent).toBe("1");
    expect((screen.getByLabelText("Question") as HTMLTextAreaElement).value).toBe(
      "Keep this draft"
    );

    fireEvent.keyDown(screen.getByRole("separator", { name: "Resize margin" }), {
      key: "ArrowLeft",
    });
    await waitFor(() =>
      expect(screen.getByTestId("desktop-margin-shell").getAttribute("style")).toContain(
        "width: 408px"
      )
    );
    expect((screen.getByLabelText("Question") as HTMLTextAreaElement).value).toBe(
      "Keep this draft"
    );

    act(() => {
      compact = true;
      for (const listener of changeListeners) {
        listener(new Event("change"));
      }
    });
    await waitFor(() => expect(screen.queryByTestId("desktop-margin-shell")).toBeNull());

    act(() => {
      compact = false;
      for (const listener of changeListeners) {
        listener(new Event("change"));
      }
    });
    await waitFor(() =>
      expect((screen.getByLabelText("Question") as HTMLTextAreaElement).value).toBe(
        "Keep this draft"
      )
    );
  } finally {
    view.unmount();
    window.matchMedia = originalMatchMedia;
    if (innerWidthDescriptor === undefined) {
      Object.defineProperty(window, "innerWidth", {
        configurable: true,
        value: originalInnerWidth,
      });
    } else {
      Object.defineProperty(window, "innerWidth", innerWidthDescriptor);
    }
  }
});

const documentArtifact = {
  ...specArtifact,
  issue_key: null,
  primary: false,
  referenced_by: [],
};

function documentComment(id: string, body: string, replyTo: string | null = null): Comment {
  return {
    ...comment,
    anchor: {
      artifact_id: documentArtifact.id,
      block_id: null,
      mark_id: `${id}-mark`,
      orphaned: false,
      quote: body,
      version: 1,
    },
    body,
    id,
    issue_key: null,
    reply_to: replyTo,
  };
}

function renderDocumentMargin(comments: Comment[]) {
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  queryClient.setQueryData(
    ["artifact-ref", `${documentArtifact.project}/${documentArtifact.slug}`],
    documentArtifact
  );
  queryClient.setQueryData(["artifact", documentArtifact.id, "asks"], []);
  queryClient.setQueryData(["artifact", documentArtifact.id, "comments"], comments);
  const listArtifactComments = spyOn(api, "listArtifactComments").mockResolvedValue(comments);
  const view = render(
    <MemoryRouter
      initialEntries={[
        buildProjectPath({
          kind: "document",
          project: documentArtifact.project,
          slug: documentArtifact.slug,
        }),
      ]}
    >
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { listArtifactComments, view };
}

function expandThreadCard(card: HTMLElement): void {
  const collapsed = card.querySelector<HTMLButtonElement>('button[aria-expanded="false"]');
  if (collapsed === null) {
    throw new Error("Expected a collapsed thread card.");
  }
  fireEvent.click(collapsed);
}

async function saveMarginEdit(card: HTMLElement, body: string): Promise<void> {
  expandThreadCard(card);
  await waitFor(() =>
    expect(within(card).getAllByRole("button", { name: "Edit" })).toHaveLength(1)
  );
  fireEvent.click(within(card).getByRole("button", { name: "Edit" }));
  fireEvent.change(screen.getByLabelText("Edit comment"), { target: { value: body } });
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await Promise.resolve();
  });
}

test("Margin keeps sibling edit controls hidden while an owner save is pending", async () => {
  const saving = documentComment("saving-comment", "Saving comment");
  const sibling = documentComment("sibling-comment", "Sibling comment");
  const save = Promise.withResolvers<Comment>();
  const editComment = spyOn(api, "editComment").mockImplementation(() => save.promise);
  const { listArtifactComments, view } = renderDocumentMargin([saving, sibling]);

  try {
    const savingCard = await screen.findByTestId(`margin-comment-${saving.id}`);
    await saveMarginEdit(savingCard, "Updated saving comment");
    await waitFor(() =>
      expect(editComment).toHaveBeenCalledWith(saving.id, { body: "Updated saving comment" })
    );

    const siblingCard = screen.getByTestId(`margin-comment-${sibling.id}`);
    expandThreadCard(siblingCard);
    await waitFor(() =>
      expect(within(siblingCard).queryAllByRole("button", { name: "Edit" })).toHaveLength(0)
    );
    expect((within(siblingCard).getByLabelText("Reply") as HTMLTextAreaElement).disabled).toBe(
      false
    );

    await act(async () => {
      save.resolve(saving);
      await save.promise;
    });
    await waitFor(() =>
      expect(within(siblingCard).getAllByRole("button", { name: "Edit" })).toHaveLength(1)
    );
  } finally {
    save.resolve(saving);
    view.unmount();
    editComment.mockRestore();
    listArtifactComments.mockRestore();
  }
});

test("Margin restores edit controls and the draft when an owner save rejects", async () => {
  const root = documentComment("root-comment", "Root comment");
  const reply = documentComment("reply-comment", "Reply comment", root.id);
  const save = Promise.withResolvers<Comment>();
  const editComment = spyOn(api, "editComment").mockImplementation(() => save.promise);
  const { listArtifactComments, view } = renderDocumentMargin([root, reply]);

  try {
    const card = await screen.findByTestId(`margin-comment-${root.id}`);
    expandThreadCard(card);
    await waitFor(() =>
      expect(within(card).getAllByRole("button", { name: "Edit" })).toHaveLength(2)
    );
    fireEvent.click(within(card).getAllByRole("button", { name: "Edit" })[0] as HTMLElement);
    fireEvent.change(screen.getByLabelText("Edit comment"), {
      target: { value: "Draft survives" },
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Save" }));
      await Promise.resolve();
    });
    await waitFor(() =>
      expect(editComment).toHaveBeenCalledWith(root.id, { body: "Draft survives" })
    );
    expect(within(card).queryAllByRole("button", { name: "Edit" })).toHaveLength(0);
    expect((screen.getByLabelText("Reply") as HTMLTextAreaElement).disabled).toBe(false);

    await act(async () => {
      save.reject(new Error("offline"));
      await Promise.resolve();
    });
    await screen.findByRole("alert");
    await waitFor(() =>
      expect(within(card).getAllByRole("button", { name: "Edit" })).toHaveLength(2)
    );
    expect((screen.getByLabelText("Edit comment") as HTMLTextAreaElement).value).toBe(
      "Draft survives"
    );
  } finally {
    save.resolve(root);
    view.unmount();
    editComment.mockRestore();
    listArtifactComments.mockRestore();
  }
});
