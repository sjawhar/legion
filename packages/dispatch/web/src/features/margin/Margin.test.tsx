import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { type ReactNode, useEffect, useState } from "react";
import { MemoryRouter, useLocation, useNavigate } from "react-router-dom";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { api } from "../../api/client";
import type { Ask, Comment, IssueDetails } from "../../api/types";
import type { RetypeOutcome } from "../doc/editor";
import { buildIssuePath, buildProjectPath } from "../refs/routes";
import { Margin } from "./Margin";
import { type DocumentBridge, MarginProvider, useMargin } from "./margin-context";
import {
  anchoredAsk,
  comment,
  issue,
  SelectedItemLabel,
  specArtifact,
  stubMatchMedia,
} from "./margin-fixture";

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

const unanchoredRootComment: Comment = {
  ...comment,
  anchor: null,
  body: "Issue-level comment.",
  id: "comment-unanchored-root",
};

const unanchoredAsk: Ask = {
  ...anchoredAsk,
  anchor: null,
  id: "ask-unanchored",
  question: "Approve the release?",
};

const markComposerAnchor = { artifact: "artifact-1", mark_id: "m-1", quote: "selected" };

/** A margin ask card by its ask, in the group it is listed under: open and waiting on the reader,
 *  or decided. Both groups are one keyed list, so the group is the card's own attribute. */
function marginAskCard(id: string, section: "needs-you" | "decided"): Promise<HTMLElement> {
  return waitFor(() => {
    const card = document.querySelector<HTMLElement>(
      `[data-margin-item="${id}"][data-margin-section="${section}"]`
    );
    if (card === null) throw new Error(`no ${section} card for ${id}`);
    return card;
  });
}

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

function FocusMarkButton(): ReactNode {
  const { focusItemForMark } = useMargin();

  return (
    <button onClick={() => focusItemForMark("m-1")} type="button">
      Focus mark
    </button>
  );
}

function LocationLabel(): ReactNode {
  const { pathname } = useLocation();

  return <output aria-label="Current path">{pathname}</output>;
}

test("clicking a document mark opens its thread in the margin and stays on the document", async () => {
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
  const restoreMatchMedia = stubMatchMedia(false);
  const specPath = buildIssuePath({ key: issue.key, kind: "spec" });
  const view = render(
    <MemoryRouter initialEntries={[specPath]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <FocusMarkButton />
          <SelectedItemLabel />
          <LocationLabel />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByTestId(`margin-comment-${comment.id}`);
    fireEvent.click(screen.getByRole("tab", { name: "Pinned" }));
    fireEvent.click(screen.getByRole("button", { name: "Focus mark" }));
    await waitFor(() => {
      expect(screen.getByRole("tab", { name: "Comments" }).getAttribute("aria-selected")).toBe(
        "true"
      );
      // Re-query: expanding the thread remounts the card.
      expect(screen.getByTestId(`margin-comment-${comment.id}`).getAttribute("aria-current")).toBe(
        "true"
      );
    });
    expect(screen.getByLabelText("Selected margin item").textContent).toBe(comment.id);
    expect(screen.getByLabelText("Current path").textContent).toBe(specPath);
  } finally {
    view.unmount();
    restoreMatchMedia();
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
  const restoreMatchMedia = stubMatchMedia(true);
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
    restoreMatchMedia();
  }
});

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
    await screen.findByText("No comments, asks, or suggestions on this document.");
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
    expect(await screen.findByRole("heading", { name: "Needs you" })).toBeTruthy();
    const needsYou = await marginAskCard(unanchoredAsk.id, "needs-you");
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

// The answered card moves from Needs you to the decided asks in place - one keyed list - so its
// card is the one the reader was in, with whatever they had started in it (`threads.e2e.ts` types
// a reply there), rather than a fresh mount of it.
test("Margin moves an answered anchored ask out of Needs you in place, without an event stream", async () => {
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
  const refetched = Promise.withResolvers<Ask[]>();
  const listIssueAsks = spyOn(api, "listIssueAsks").mockImplementation(() => refetched.promise);
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
    const needsYou = await marginAskCard(anchoredAsk.id, "needs-you");
    fireEvent.click(await within(needsYou).findByRole("radio", { name: "Ship" }));
    fireEvent.click(within(needsYou).getByRole("button", { name: "Answer" }));
    await waitFor(() =>
      expect(answerAsk).toHaveBeenCalledWith(anchoredAsk.id, {
        selected: ["Ship"],
        expected_edited_at: null,
      })
    );
    await waitFor(() => expect(listIssueAsks).toHaveBeenCalled());
    const answered = await within(needsYou).findByTestId(`ask-${anchoredAsk.id}`);

    await act(async () => refetched.resolve([answeredAsk]));
    await waitFor(() => expect(screen.queryByRole("heading", { name: "Needs you" })).toBeNull());
    expect(screen.getByRole("button", { name: "Open review panel (0 open asks)" })).toBeTruthy();
    const decided = await marginAskCard(anchoredAsk.id, "decided");
    expect(decided).toBe(needsYou);
    expect(within(decided).getByTestId(`ask-${anchoredAsk.id}`)).toBe(answered);
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

function fakeBridge(retype: (markId: string, kind: string) => RetypeOutcome): {
  bridge: DocumentBridge;
  held: (string | null)[];
  removed: string[];
  retyped: [string, string][];
} {
  const held: (string | null)[] = [];
  const removed: string[] = [];
  const retyped: [string, string][] = [];
  return {
    bridge: {
      focusBlock() {},
      focusMark() {},
      removeMark(markId) {
        removed.push(markId);
      },
      retypeMark(markId, kind) {
        retyped.push([markId, kind]);
        return retype(markId, kind);
      },
      setActiveBlocks() {},
      setActiveMarks() {},
      setComposerMark(markId) {
        held.push(markId);
      },
    },
    held,
    removed,
    retyped,
  };
}

function RegisterBridge({ bridge }: { bridge: DocumentBridge }): ReactNode {
  const { registerDocument } = useMargin();
  useEffect(() => {
    registerDocument(bridge);
    return () => registerDocument(undefined);
  }, [bridge, registerDocument]);
  return null;
}

/** What a remounted `ProofDocument` does (a new artifact or block schema): the old editor
 *  unregisters and a fresh one registers, while the margin and its open composer stay. */
function ReopenDocument({ bridge }: { bridge: DocumentBridge }): ReactNode {
  const { registerDocument } = useMargin();
  return (
    <button
      onClick={() => {
        registerDocument(undefined);
        registerDocument(bridge);
      }}
      type="button"
    >
      Reopen the document
    </button>
  );
}

function renderMarginWithBridge(bridge: DocumentBridge, reopened?: DocumentBridge) {
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
  return render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <RegisterBridge bridge={bridge} />
          {reopened === undefined ? null : <ReopenDocument bridge={reopened} />}
          <MarkComposerProbe />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

test("the composer's kind switch retypes its mark through the document and sends under the new mark", async () => {
  const fake = fakeBridge((markId, kind) =>
    kind === "suggestion"
      ? { refused: "unmarkable" }
      : { markId: `${markId}-${kind}`, quote: "selected" }
  );
  const createAsk = spyOn(api, "createAsk").mockResolvedValue(undefined as never);
  const view = renderMarginWithBridge(fake.bridge);

  try {
    fireEvent.click(screen.getByRole("button", { name: "Compose first" }));
    const composer = await screen.findByRole("form", { name: "Comment composer" });
    const kinds = within(composer).getByRole("group", { name: "Kind" });

    fireEvent.click(within(kinds).getByRole("button", { name: "Suggest" }));
    expect(fake.retyped).toEqual([["m-1", "suggestion"]]);
    expect(within(composer).getByRole("status").textContent).toBe(
      "A suggestion needs whole words inside one table cell. Comment or ask about this selection instead, or close this composer and select again."
    );
    expect(
      within(kinds).getByRole("button", { name: "Comment" }).getAttribute("aria-pressed")
    ).toBe("true");

    fireEvent.click(within(kinds).getByRole("button", { name: "Ask" }));
    expect(fake.retyped).toEqual([
      ["m-1", "suggestion"],
      ["m-1", "ask"],
    ]);
    await waitFor(() =>
      expect(within(kinds).getByRole("button", { name: "Ask" }).getAttribute("aria-pressed")).toBe(
        "true"
      )
    );
    expect(within(composer).queryByRole("status")).toBeNull();

    fireEvent.change(within(composer).getByLabelText("Question"), { target: { value: "Why?" } });
    fireEvent.click(within(composer).getAllByRole("button", { name: "Ask" }).at(-1) as HTMLElement);
    await waitFor(() => expect(createAsk).toHaveBeenCalledTimes(1));
    expect(createAsk.mock.calls[0]?.[1]).toMatchObject({
      anchor: { artifact: "artifact-1", mark_id: "m-1-ask" },
      question: "Why?",
    });
    expect(fake.removed).toEqual([]);
  } finally {
    view.unmount();
    createAsk.mockRestore();
  }
});

test("the margin removes the composer's mark when the composer is cancelled or replaced, and tells the document which mark the composer holds", async () => {
  const fake = fakeBridge((markId, kind) => ({ markId: `${markId}-${kind}`, quote: "selected" }));
  const reopened = fakeBridge((markId, kind) => ({
    markId: `${markId}-${kind}`,
    quote: "selected",
  }));
  const view = renderMarginWithBridge(fake.bridge, reopened.bridge);

  try {
    fireEvent.click(screen.getByRole("button", { name: "Compose first" }));
    const composer = await screen.findByRole("form", { name: "Comment composer" });
    fireEvent.click(
      within(within(composer).getByRole("group", { name: "Kind" })).getByRole("button", {
        name: "Ask",
      })
    );
    await screen.findByLabelText("Question");

    // A newer composer takes the margin: the retyped mark, not the original, is removed.
    fireEvent.click(screen.getByRole("button", { name: "Compose second" }));
    await waitFor(() => expect(fake.removed).toEqual(["m-1-ask"]));
    // Registering told the document no composer was open; each compose since named its mark.
    expect(fake.held).toEqual([null, "m-1", "m-1-ask", "m-2"]);

    // A fresh editor registered under the open composer learns which mark the composer holds.
    fireEvent.click(screen.getByRole("button", { name: "Reopen the document" }));
    expect(reopened.held).toEqual(["m-2"]);

    // Cancelling removes the mark the composer holds, in the document now open.
    fireEvent.keyDown(
      within(screen.getByRole("form", { name: "Comment composer" })).getByLabelText("Comment"),
      { key: "Escape" }
    );
    await waitFor(() => expect(reopened.removed).toEqual(["m-2"]));
    expect(screen.queryByRole("form", { name: "Comment composer" })).toBeNull();
    // The document always knew which mark was the composer's, so a selection-bar action that cut
    // into it counted as the composer's own write.
    expect(reopened.held).toEqual(["m-2", null]);
    // The replaced document's log stays frozen: once the document reopened onto `reopened`, the
    // margin told `fake` nothing more.
    expect(fake.held).toEqual([null, "m-1", "m-1-ask", "m-2"]);
  } finally {
    view.unmount();
  }
});

// A selection-bar action while the open compose's own send is out cannot take the margin, and
// the reader sees why: the open compose comes back on screen - here from under the Pinned tab -
// saying its send is still out, until that send lands.
test("a newer compose while the open compose's send is out brings that compose back, saying why", async () => {
  const fake = fakeBridge((markId, kind) => ({ markId: `${markId}-${kind}`, quote: "selected" }));
  const sent = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment").mockImplementation(() => sent.promise);
  const view = renderMarginWithBridge(fake.bridge);

  try {
    fireEvent.click(screen.getByRole("button", { name: "Compose first" }));
    const composer = await screen.findByRole("form", { name: "Comment composer" });
    fireEvent.change(within(composer).getByLabelText("Comment"), { target: { value: "why?" } });
    fireEvent.click(within(composer).getByRole("button", { name: "Send" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    const pinned = screen.getByRole("tab", { name: "Pinned" });
    fireEvent.click(pinned);
    await waitFor(() => expect(pinned.getAttribute("aria-selected")).toBe("true"));

    fireEvent.click(screen.getByRole("button", { name: "Compose second" }));
    await waitFor(() =>
      expect(screen.getByLabelText("Second composer outcome").textContent).toBe(
        "the open composer's send is out"
      )
    );
    expect(fake.removed).toEqual(["m-2"]);
    await waitFor(() =>
      expect(screen.getByRole("tab", { name: "Comments" }).getAttribute("aria-selected")).toBe(
        "true"
      )
    );
    const shown = screen.getByRole("form", { name: "Comment composer" });
    expect(within(shown).getByText("selected")).toBeTruthy();
    expect(within(shown).getByLabelText<HTMLTextAreaElement>("Comment").value).toBe("why?");
    expect(
      screen.getByText(
        "This one is still sending, so the new selection wasn't kept. Select it again once this one is sent."
      )
    ).toBeTruthy();

    await act(async () => {
      sent.resolve({ ...comment, body: "why?" });
      await sent.promise;
    });
    await waitFor(() => {
      expect(screen.getByLabelText("First composer outcome").textContent).toBe("saved");
      expect(screen.queryByRole("form", { name: "Comment composer" })).toBeNull();
    });
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

function SecondDocumentCompose(): ReactNode {
  const { composeForMark } = useMargin();
  const [outcome, setOutcome] = useState("idle");
  return (
    <>
      <button
        onClick={() => {
          void composeForMark({
            anchor: { artifact: "artifact-2", mark_id: "m-3", quote: "elsewhere" },
            kind: "comment",
          }).then(
            () => setOutcome("saved"),
            (error: unknown) => setOutcome(error instanceof Error ? error.message : String(error))
          );
        }}
        type="button"
      >
        Compose on the second document
      </button>
      <output aria-label="Second document composer outcome">{outcome}</output>
    </>
  );
}

// Each compose names its own send, so a send the reader left behind with its document holds no
// compose on the next one, and when it lands it closes and settles nothing there.
test("a compose after leaving a document mid-send opens, and that send's landing leaves it open", async () => {
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
  const sent = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment").mockImplementation(() => sent.promise);

  const view = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: "CORE-1", kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <MarkComposerProbe />
          <SecondDocumentCompose />
          <NavigateToSecondIssue />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    fireEvent.click(screen.getByRole("button", { name: "Compose first" }));
    const first = await screen.findByRole("form", { name: "Comment composer" });
    fireEvent.change(within(first).getByLabelText("Comment"), { target: { value: "why?" } });
    fireEvent.click(within(first).getByRole("button", { name: "Send" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    fireEvent.click(screen.getByRole("button", { name: "Open second issue" }));
    await waitFor(() =>
      expect(screen.getByLabelText("First composer outcome").textContent).toBe("composer closed")
    );

    fireEvent.click(screen.getByRole("button", { name: "Compose on the second document" }));
    const second = await screen.findByRole("form", { name: "Comment composer" });
    expect(within(second).getByText("elsewhere")).toBeTruthy();
    fireEvent.change(within(second).getByLabelText("Comment"), {
      target: { value: "Second draft" },
    });

    await act(async () => {
      sent.resolve({ ...comment, body: "why?" });
      await sent.promise;
    });
    await waitFor(() => expect(queryClient.isMutating()).toBe(0));
    const still = screen.getByRole("form", { name: "Comment composer" });
    expect(within(still).getByText("elsewhere")).toBeTruthy();
    expect(within(still).getByLabelText<HTMLTextAreaElement>("Comment").value).toBe("Second draft");
    expect(screen.getByLabelText("Second document composer outcome").textContent).toBe("idle");
  } finally {
    view.unmount();
    getIssue.mockRestore();
    getInbox.mockRestore();
    listIssueAsks.mockRestore();
    getMyState.mockRestore();
    listComments.mockRestore();
    createComment.mockRestore();
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

test("the desktop margin takes no width on a route that has no margin", async () => {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["user-state"], {});
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
  const inbox = render(
    <MemoryRouter initialEntries={["/"]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    expect(screen.queryByTestId("desktop-margin-shell")).toBeNull();
    expect(screen.queryByRole("separator", { name: "Resize margin" })).toBeNull();
    expect(screen.queryByText("Open an issue or document to review its margin.")).toBeNull();
  } finally {
    inbox.unmount();
  }

  const onIssue = render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    expect(await screen.findByTestId("desktop-margin-shell")).toBeTruthy();
  } finally {
    onIssue.unmount();
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
    await screen.findByText("No comments, asks, or suggestions on this document.");
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
