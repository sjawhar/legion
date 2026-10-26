import { afterEach, expect, jest, type Mock, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";
import {
  type IntersectionControl,
  installIntersectionObserver,
  type Placement,
} from "../../__tests__/intersection-observer";
import { ApiError, api } from "../../api/client";
import type {
  Actor,
  Agent,
  Artifact,
  Ask,
  Comment,
  CommentDelivery,
  Event,
  IssueAskThreadRow,
  Message,
  MessageDelivery,
  MessageRead,
  UserIssueState,
  UserState,
} from "../../api/types";
import { stubMatchMedia } from "../margin/margin-fixture";
import { seedUnread } from "../replies/replies-fixture";
import { KeymapProvider } from "../shell/KeymapProvider";
import { ConversationTab } from "./ConversationTab";
import { answeredWithErrorGuidance, safeRetryGuidance } from "./delivery";

const alice: Actor = { id: "alice", kind: "user" };

// Delivery attempts are dated relative to the run: the dashboard only offers a
// same-mode Retry while an attempt is inside the stream's duplicate window, so a
// fixture frozen at an absolute date would age out of every retry assertion.
const recentAttemptAt = new Date(Date.now() - 60_000).toISOString();

function issueState(dismissed: string[] = [], lastReadSeq = 0): UserIssueState {
  return { dismissed, last_read_seq: lastReadSeq, pinned: false, seq: 0 };
}

function comment(id: string, overrides: Partial<Comment> = {}): Comment {
  return {
    anchor: null,
    ask_id: null,
    author: alice,
    body: `Body of ${id}`,
    created_at: "2026-09-20T00:00:00Z",
    deliveries: [],
    edited_at: null,
    id,
    issue_key: "CORE-1",
    mentions: [],
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
    ...overrides,
  };
}

function message(id: string, overrides: Partial<Message> = {}): Message {
  return {
    author: alice,
    body: `Body of ${id}`,
    created_at: "2026-09-09T00:00:00Z",
    deliveries: [],
    id,
    in_reply_to: null,
    issue_key: "CORE-1",
    target: null,
    ...overrides,
  };
}

function thread(root: Message, replies: Message[] = []): MessageRead {
  return { message: root, replies };
}

function attempt(overrides: Partial<MessageDelivery> = {}): MessageDelivery {
  return {
    attempt: 1,
    created_at: recentAttemptAt,
    delivery: "btw",
    envelope_id: null,
    error: null,
    message_id: "message-1",
    reply_id: null,
    session_id: "s1",
    state: "sent",
    ...overrides,
  };
}

function mentionAttempt(overrides: Partial<CommentDelivery> = {}): CommentDelivery {
  return {
    attempt: 1,
    comment_id: "comment-1",
    created_at: recentAttemptAt,
    delivery: "steer",
    envelope_id: null,
    error: null,
    reply_id: null,
    resolve_error: null,
    session_id: "worker-session",
    state: "failed",
    target: "session:worker-session",
    ...overrides,
  };
}

const baseAsk: Ask = {
  anchor: null,
  answer: null,
  author: { id: "s1", kind: "session" },
  created_at: "2026-09-10T09:00:00Z",
  edited_at: null,
  id: "ask-1",
  issue_key: "CORE-1",
  kind: "question",
  multiple: false,
  opened_event_id: 4,
  options: [{ label: "Ship" }, { label: "Hold" }],
  question: "Ship it?",
  state: "open",
  urgency: "med",
};

function askRow(overrides: Partial<Ask> = {}, replies: Comment[] = []): IssueAskThreadRow {
  return {
    ...baseAsk,
    ...overrides,
    last_reply: null,
    thread: { answers: [], edits: [], followers: [], replies },
  };
}

function headEvent(seq: number, at = "2026-09-20T00:00:00Z"): Event {
  return {
    actor: alice,
    created_at: at,
    id: seq,
    issue_key: "CORE-1",
    notify: false,
    payload: {},
    seq,
    type: "issue.created",
  } as Event;
}

function agent(sessionId: string, title: string, capabilities: string[] = []): Agent {
  return {
    capabilities,
    dir: "/w",
    host: "m",
    last_activity: null,
    last_seen: 1,
    machine_id: "m",
    open_asks: 0,
    roles: [],
    session_id: sessionId,
    title,
  };
}

const primaryDocument: Artifact = {
  created_at: "2026-09-09T00:00:00Z",
  created_by: alice,
  id: "artifact-primary",
  issue_key: "CORE-1",
  kind: "doc",
  name: "spec.md",
  primary: true,
  project: "CORE",
  slug: "spec",
  versions: [],
};

const secondaryDocument: Artifact = {
  ...primaryDocument,
  id: "artifact-secondary",
  name: "supporting-document.md",
  primary: false,
  slug: "supporting-document",
};

interface Lists {
  agents?: Agent[];
  asks?: IssueAskThreadRow[];
  comments?: Comment[];
  /** The issue's newest event, which a dwell on the newest card marks read through. */
  head?: Event[];
  messages?: MessageRead[];
  /** The first event the reader has not read: its moment is the unread boundary. */
  unread?: Event[];
}

/** The three reads the Conversation builds its list from, plus the two one-row event reads that
 *  carry the unread boundary and the issue's head. The returned `lists` is live: a test that
 *  changes it changes what the next refetch answers. */
function stubLists(initial: Lists = {}): { lists: Lists; restore: () => void } {
  const lists: Lists = { ...initial };
  const spies = [
    spyOn(api, "listIssueAskThreads").mockImplementation(async () => lists.asks ?? []),
    spyOn(api, "listComments").mockImplementation(async () => lists.comments ?? []),
    spyOn(api, "listIssueMessages").mockImplementation(async () => lists.messages ?? []),
    spyOn(api, "listAgents").mockImplementation(async () => lists.agents ?? []),
    spyOn(api, "getIssueEvents").mockImplementation(async (_key, options = {}) =>
      options.order === "desc" ? (lists.head ?? []) : (lists.unread ?? [])
    ),
  ];
  return {
    lists,
    restore: () => {
      for (const spy of spies) spy.mockRestore();
    },
  };
}

function newQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
}

function tab(
  state: UserState,
  visible: boolean,
  queryClient: QueryClient,
  isClosed = false,
  issueArtifacts: ReadonlyMap<string, Artifact> = new Map(),
  focusItemId?: string
): ReactNode {
  return (
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <KeymapProvider>
        <QueryClientProvider client={queryClient}>
          <ConversationTab
            focusItemId={focusItemId}
            issueArtifacts={issueArtifacts}
            isClosed={isClosed}
            issueKey="CORE-1"
            state={state}
            visible={visible}
          />
        </QueryClientProvider>
      </KeymapProvider>
    </MemoryRouter>
  );
}

// The sticky composer sits at document top 0 and the newest card below it. `useFollowLatest`
// reads their bounding rectangles only to place the newest card beneath the composer, so these
// tests fake that minimal layout rather than exercising a real browser layout engine.
const COMPOSER_HEIGHT = 140;
const NEWEST_TURN_DOC_TOP = 300;
const RESTING_SCROLL_Y = NEWEST_TURN_DOC_TOP - COMPOSER_HEIGHT;

function installScrollLayoutMocks(): { restore: () => void } {
  const rectSpy = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    const isComposer = this.getAttribute("aria-label") === "Comment composer";
    const isTurn = this.hasAttribute("data-turn");
    const docTop = isComposer ? 0 : isTurn ? NEWEST_TURN_DOC_TOP : 0;
    const height = isComposer ? COMPOSER_HEIGHT : isTurn ? 50 : 0;
    const top = docTop - window.scrollY;
    return {
      bottom: top + height,
      height,
      left: 0,
      right: 100,
      toJSON: () => ({}),
      top,
      width: 100,
      x: 0,
      y: top,
    };
  });
  const originalScrollTo = window.scrollTo;
  window.scrollTo = (options?: ScrollToOptions | number, y?: number) => {
    const top = typeof options === "number" ? y : options?.top;
    if (top !== undefined) {
      Object.defineProperty(window, "scrollY", { configurable: true, value: top, writable: true });
    }
  };
  return {
    restore: () => {
      rectSpy.mockRestore();
      window.scrollTo = originalScrollTo;
    },
  };
}

function withScrollY(run: () => Promise<void>): () => Promise<void> {
  return async () => {
    const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
    try {
      await run();
    } finally {
      if (scrollY === undefined) {
        Reflect.deleteProperty(window, "scrollY");
      } else {
        Object.defineProperty(window, "scrollY", scrollY);
      }
    }
  };
}

afterEach(() => {
  jest.useRealTimers();
});

test("the read mark watches the newest conversation alone, and only while the tab is visible", async () => {
  const viewport = installIntersectionObserver();
  const stub = stubLists({
    head: [headEvent(7)],
    messages: [
      thread(message("message-1", { created_at: "2026-09-09T00:00:00Z" })),
      thread(message("message-2", { created_at: "2026-09-09T00:05:00Z" })),
    ],
  });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, false, queryClient));

  try {
    await screen.findByText("Body of message-2");
    expect(viewport.observed().filter((element) => view.container.contains(element))).toHaveLength(
      0
    );

    view.rerender(tab({ "CORE-1": issueState() }, true, queryClient));
    const newest = view.container.querySelector('[data-turn="message:message-2"]');
    const older = view.container.querySelector('[data-turn="message:message-1"]');
    await waitFor(() => expect(viewport.observed()).toContain(newest as Element));
    expect(viewport.observed()).not.toContain(older as Element);
  } finally {
    view.unmount();
    stub.restore();
    viewport.restore();
  }
});

test("Conversation holds Reply while its composer sends", async () => {
  const sent = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment").mockReturnValueOnce(sent.promise);
  const stub = stubLists({ messages: [thread(message("message-1"))] });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    const field = await screen.findByLabelText<HTMLTextAreaElement>("Comment");
    fireEvent.change(field, { target: { value: "Status please" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(field.closest("fieldset")?.disabled).toBe(true));
    const reply = screen.getByRole("button", { name: "Reply" }) as HTMLButtonElement;
    expect(reply.disabled).toBe(true);
    fireEvent.click(reply);
    expect(screen.queryByRole("button", { name: "Cancel reply" })).toBeNull();

    sent.reject(new Error("the server is down"));
    await screen.findByText("Couldn't send — network error");
    expect(createComment.mock.calls[0]).toEqual(["CORE-1", { body: "Status please" }]);
  } finally {
    view.unmount();
    createComment.mockRestore();
    stub.restore();
  }
});

test("retry replays every pin operation rejected by the state-write queue", async () => {
  const originalGetMyState = api.getMyState;
  const originalPutIssueState = api.putIssueState;
  const stub = stubLists({
    messages: [
      thread(message("message-1", { created_at: "2026-09-09T00:00:00Z" })),
      thread(message("message-2", { created_at: "2026-09-09T00:05:00Z" })),
    ],
  });
  const queryClient = newQueryClient();
  const writes: string[][] = [];
  let failures = 2;
  let unmount: (() => void) | undefined;

  try {
    api.getMyState = async () => ({ "CORE-1": issueState() });
    api.putIssueState = async (_issueKey, input) => {
      const dismissed = input.dismissed ?? [];
      writes.push(dismissed);
      if (failures > 0) {
        failures -= 1;
        throw new Error("offline");
      }
      return issueState(dismissed);
    };

    unmount = render(tab({ "CORE-1": issueState() }, true, queryClient)).unmount;
    await screen.findByText("Body of message-2");
    const pins = screen.getAllByRole("button", { name: "Pin" });
    expect(pins.map((button) => button.getAttribute("aria-pressed"))).toEqual(["false", "false"]);
    for (const button of pins) {
      fireEvent.click(button);
    }

    await waitFor(() =>
      expect(screen.getByRole("alert").textContent).toContain("Couldn't save pin — retry")
    );
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));

    // A card built from `/messages` has no event id at all: it pins under its own id.
    await waitFor(() =>
      expect(writes.at(-1)).toEqual([
        "pinned_items:message:message-2",
        "pinned_items:message:message-1",
      ])
    );
  } finally {
    unmount?.();
    api.getMyState = originalGetMyState;
    api.putIssueState = originalPutIssueState;
    stub.restore();
  }
});

test("a scroll event measures the reader position in O(1) rect reads", async () => {
  const stub = stubLists({
    messages: Array.from({ length: 250 }, (_, index) =>
      thread(
        message(`message-${index}`, {
          body: `Message ${index}`,
          created_at: `2026-09-09T00:00:${String(index % 60).padStart(2, "0")}Z`,
        })
      )
    ),
  });
  const rectSpy = spyOn(Element.prototype, "getBoundingClientRect").mockReturnValue({
    bottom: 10,
    height: 10,
    left: 0,
    right: 10,
    toJSON: () => ({}),
    top: 0,
    width: 10,
    x: 0,
    y: 0,
  });
  const elementFromPointSpy = spyOn(document, "elementFromPoint").mockImplementation(() =>
    document.querySelector("[data-turn]")
  );
  const queryClient = newQueryClient();
  queryClient.setQueryData<UserState>(["user-state"], { "CORE-1": issueState() });
  let unmount: (() => void) | undefined;

  try {
    unmount = render(tab({ "CORE-1": issueState() }, true, queryClient)).unmount;
    await screen.findByText("Message 0");

    rectSpy.mockClear();
    window.dispatchEvent(new Event("scroll"));

    // ViewportAnchor measures only in the commit phase, immediately before a content reflow; a
    // scroll alone must do no layout reads. This guards both against O(n) scans over every card
    // and unnecessary per-scroll layout work.
    expect(rectSpy.mock.calls.length).toBe(0);
  } finally {
    unmount?.();
    rectSpy.mockRestore();
    elementFromPointSpy.mockRestore();
    stub.restore();
  }
});

test("agent authors render the registry title and fall back to a short id", async () => {
  const stub = stubLists({
    agents: [agent("0123456789abcdef", "Planner")],
    messages: [
      thread(
        message("message-1", {
          author: { id: "0123456789abcdef", kind: "session" },
          created_at: "2026-09-09T00:00:00Z",
        })
      ),
      thread(
        message("message-2", {
          author: { id: "fedcba9876543210", kind: "session" },
          created_at: "2026-09-09T00:05:00Z",
        })
      ),
    ],
  });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    const turns = await screen.findByRole("list", { name: "Conversation turns" });
    await within(turns).findByText("Planner");
    expect(within(turns).getByText("session:fedcba98…")).toBeTruthy();
  } finally {
    view.unmount();
    stub.restore();
  }
});

// A closed issue takes a message's retry and replies, so its card offers them as an open one does.
test("offers targeted-message retries and Reply on a closed issue", async () => {
  const stub = stubLists({
    messages: [
      thread(
        message("message-1", {
          deliveries: [attempt({ error: "no live session s1", state: "failed" })],
          target: "session:s1",
        })
      ),
    ],
  });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient, true));

  try {
    await screen.findByText(/Failed: no live session s1/);
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
    const turns = screen.getByRole("list", { name: "Conversation turns" });
    expect(within(turns).getByRole("button", { name: "Reply" })).toBeTruthy();
  } finally {
    view.unmount();
    stub.restore();
  }
});

// A failed attempt that carries an envelope id was failed by the session's own error reply
// after Dispatch recorded the send (`rowAnsweredWithError`); that attempt gets a mode change,
// not a same-mode Retry whose repeated key the stream has already stored.
test("a targeted message its session answered with an error offers a mode change, not Retry", async () => {
  const stub = stubLists({
    messages: [
      thread(
        message("message-1", {
          deliveries: [
            attempt({ envelope_id: "envelope-1", error: "side turn failed", state: "failed" }),
          ],
          target: "session:s1",
        })
      ),
    ],
  });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    await screen.findByText(`Failed: side turn failed. ${answeredWithErrorGuidance("card")}`);
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
    expect(screen.getByRole("button", { name: "Use Send instead" })).toBeTruthy();
  } finally {
    view.unmount();
    stub.restore();
  }
});

// A mention's row carries the same mark. The mention list has no mode-change action, so the
// attempt gets no Retry and is pointed at a new comment; the failure Dispatch recorded for the
// same attempt keeps its Retry.
test("a mention its session answered with an error is pointed at a new comment, not Retry", async () => {
  const mentioned = (id: string, session: string, deliveries: CommentDelivery[]): Comment =>
    comment(id, {
      body: `@${session} take a look`,
      deliveries,
      mentions: [{ delivery: "btw", session_id: session, target: `session:${session}` }],
    });
  const stub = stubLists({
    comments: [
      mentioned("comment-1", "worker", [
        mentionAttempt({
          comment_id: "comment-1",
          envelope_id: "envelope-1",
          error: "worker failed",
          session_id: "worker",
          target: "session:worker",
        }),
      ]),
      mentioned("comment-2", "reviewer", [
        mentionAttempt({
          comment_id: "comment-2",
          error: "reviewer failed",
          session_id: "reviewer",
          target: "session:reviewer",
        }),
      ]),
    ],
  });
  const queryClient = newQueryClient();
  let view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    await screen.findByText(
      `session:worker · failed · worker failed. ${answeredWithErrorGuidance("mention")}`
    );
    screen.getByText(
      `session:reviewer · failed · reviewer failed. ${safeRetryGuidance("mention", "reviewer failed")}`
    );
    expect(screen.getAllByRole("button", { name: "Retry" })).toHaveLength(1);
    view.unmount();

    // A closed issue takes a retry and a new comment, so it says both sentences, with the Retry,
    // as an open one does.
    view = render(tab({ "CORE-1": issueState() }, true, queryClient, true));
    await screen.findByText(
      `session:worker · failed · worker failed. ${answeredWithErrorGuidance("mention")}`
    );
    screen.getByText(
      `session:reviewer · failed · reviewer failed. ${safeRetryGuidance("mention", "reviewer failed")}`
    );
    expect(screen.getAllByRole("button", { name: "Retry" })).toHaveLength(1);
  } finally {
    view.unmount();
    stub.restore();
  }
});

test("a targeted message asked by a session names that session as the asker", async () => {
  const asker = { id: "s2", kind: "session", origin: { session_title: "reviewer" } } as const;
  const stub = stubLists({
    agents: [agent("s1", "planner")],
    messages: [
      thread(
        message("message-2", {
          author: asker,
          body: "Is the build green?",
          deliveries: [attempt({ message_id: "message-2" })],
          target: "session:s1",
        })
      ),
    ],
  });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    await screen.findByText("Asking planner (BTW) ·");
    expect(screen.getByText("reviewer")).toBeTruthy();
    expect(screen.queryByText("alice")).toBeNull();
  } finally {
    view.unmount();
    stub.restore();
  }
});

// A closed issue takes comments and message replies, so its Conversation keeps the composer.
test("renders the message composer on an open issue and a closed one alike", async () => {
  const stub = stubLists({ messages: [thread(message("message-1"))] });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    await screen.findByRole("form", { name: "Comment composer" });
    view.rerender(tab({ "CORE-1": issueState() }, true, queryClient, true));
    expect(screen.getByRole("form", { name: "Comment composer" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Send" })).toBeTruthy();
  } finally {
    view.unmount();
    stub.restore();
  }
});

test("renders an anchored comment with the owner controls in Conversation", async () => {
  const stub = stubLists({
    comments: [
      comment("comment-1", {
        anchor: {
          artifact_id: "artifact-secondary",
          block_id: null,
          mark_id: "mark-1",
          orphaned: true,
          quote: "Anchored source",
          version: 1,
        },
        body: "Please revise this.",
      }),
    ],
  });
  const queryClient = newQueryClient();
  queryClient.setQueryData(["whoami"], { login: "alice" });
  const view = render(
    tab(
      { "CORE-1": issueState() },
      true,
      queryClient,
      false,
      new Map([["artifact-secondary", secondaryDocument]])
    )
  );

  try {
    await screen.findByText("Please revise this.");
    const card = screen.getByTestId("margin-comment-comment-1");
    expect(card.textContent).toContain("Anchored source");
    fireEvent.click(screen.getByRole("button", { name: "Expand thread" }));
    expect(screen.getByRole("button", { name: "Edit" })).toBeTruthy();
    expect(
      screen.getAllByRole("button", {
        name: "Copy reference dispatch://CORE-1/comment/comment-1",
      })
    ).toHaveLength(1);
    expect(screen.getByRole("link", { name: "View in document" }).getAttribute("href")).toBe(
      "/issues/CORE-1/artifacts/supporting-document?comment=comment-1"
    );
    expect(screen.getByRole("link", { name: "View original text" }).getAttribute("href")).toBe(
      "/issues/CORE-1/artifacts/supporting-document?v=1&comment=comment-1"
    );
  } finally {
    view.unmount();
    stub.restore();
  }
});

test("a comment on the primary document links to the Spec route, not its artifact route", async () => {
  // The link is built by `documentItemPath`, the one builder that knows a primary document is
  // the issue's Spec tab; the hand-assembled href it replaced always named the artifact route.
  const stub = stubLists({
    comments: [
      comment("comment-1", {
        anchor: {
          artifact_id: "artifact-primary",
          block_id: null,
          mark_id: "mark-1",
          orphaned: false,
          quote: "Anchored source",
          version: 1,
        },
        body: "Please revise this.",
      }),
    ],
  });
  const queryClient = newQueryClient();
  queryClient.setQueryData(["whoami"], { login: "alice" });
  const view = render(
    tab(
      { "CORE-1": issueState() },
      true,
      queryClient,
      false,
      new Map([["artifact-primary", primaryDocument]])
    )
  );

  try {
    await screen.findByText("Please revise this.");
    expect(screen.getByRole("link", { name: "View in document" }).getAttribute("href")).toBe(
      "/issues/CORE-1/spec?comment=comment-1"
    );
  } finally {
    view.unmount();
    stub.restore();
  }
});

test("the filter opens on All, and Open and Closed partition the list", async () => {
  const stub = stubLists({
    asks: [
      askRow({
        answer: { at: "2026-09-10T10:00:00Z", selected: ["Ship"], text: null, user: "bob" },
        state: "answered",
      }),
    ],
    comments: [comment("comment-1", { body: "An open thread" })],
  });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    await screen.findByText("An open thread");
    expect(screen.getByRole("button", { name: "All" }).getAttribute("aria-pressed")).toBe("true");
    expect(screen.getByText("Ship it?")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Open" }));
    expect(screen.queryByText("Ship it?")).toBeNull();
    expect(screen.getByText("An open thread")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Closed" }));
    expect(screen.getByText("Ship it?")).toBeTruthy();
    expect(screen.queryByText("An open thread")).toBeNull();

    // Not persisted: a reader who filtered on an earlier visit meets the whole conversation.
    view.unmount();
    const revisit = render(tab({ "CORE-1": issueState() }, true, queryClient));
    try {
      await screen.findByText("An open thread");
      expect(screen.getByRole("button", { name: "All" }).getAttribute("aria-pressed")).toBe("true");
      expect(screen.getByText("Ship it?")).toBeTruthy();
    } finally {
      revisit.unmount();
    }
  } finally {
    stub.restore();
  }
});

test("a retracted ask is listed, and landing on one resets a filter that hides it", async () => {
  const retracted = askRow({
    id: "ask-retracted",
    resolution: {
      actor: { id: "s1", kind: "session" },
      at: "2026-09-10T09:10:00Z",
      kind: "retracted",
      reason: "Duplicate of the spec's own decision.",
    },
    state: "resolved",
  });
  const stub = stubLists({
    asks: [retracted],
    comments: [comment("comment-1", { body: "An open thread" })],
  });
  const queryClient = newQueryClient();
  const scrollIntoView = HTMLElement.prototype.scrollIntoView;
  HTMLElement.prototype.scrollIntoView = () => {};
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    await screen.findByText("Ship it?");
    fireEvent.click(screen.getByRole("button", { name: "Open" }));
    expect(screen.queryByText("Ship it?")).toBeNull();

    view.rerender(
      tab({ "CORE-1": issueState() }, true, queryClient, false, new Map(), "ask-retracted")
    );
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "All" }).getAttribute("aria-pressed")).toBe("true")
    );
    const card = view.container.querySelector('[data-turn="ask:ask-retracted"]');
    expect(card?.getAttribute("aria-current")).toBe("true");
  } finally {
    view.unmount();
    HTMLElement.prototype.scrollIntoView = scrollIntoView;
    stub.restore();
  }
});

test("landing on a reply under an ask marks that ask's card and the reply's own row", async () => {
  const replies = [
    comment("reply-1", {
      ask_id: "ask-1",
      body: "The oldest reply",
      created_at: "2026-09-10T09:01:00Z",
    }),
    comment("reply-2", {
      ask_id: "ask-1",
      body: "A later reply",
      created_at: "2026-09-10T09:02:00Z",
    }),
    comment("reply-3", {
      ask_id: "ask-1",
      body: "The newest reply",
      created_at: "2026-09-10T09:03:00Z",
    }),
  ];
  const getAsk = spyOn(api, "getAsk");
  const stub = stubLists({ asks: [askRow({}, replies)], comments: replies });
  const queryClient = newQueryClient();
  const scrollIntoView = HTMLElement.prototype.scrollIntoView;
  HTMLElement.prototype.scrollIntoView = () => {};
  const view = render(
    tab({ "CORE-1": issueState() }, true, queryClient, false, new Map(), "reply-1")
  );

  try {
    // The oldest reply sits behind the card's fold, which the landing opens.
    const marked = await screen.findByText("The oldest reply");
    const row = marked.closest("[data-reply]");
    expect(row?.getAttribute("aria-current")).toBe("true");
    expect(
      view.container.querySelector('[data-turn="ask:ask-1"]')?.getAttribute("aria-current")
    ).toBe("true");
    // The row's own thread seeds the card, so the page makes no per-ask thread request.
    expect(getAsk).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    getAsk.mockRestore();
    HTMLElement.prototype.scrollIntoView = scrollIntoView;
    stub.restore();
  }
});

// A thread the reader has open is in their hand: resolving its comment leaves the card, its open
// thread and the reply they are writing where they are, whatever the filter would do with it,
// until they close the thread.
test("under Open, a comment resolved while its thread is open stays until the thread closes", async () => {
  const root = comment("root-comment", { body: "Root comment" });
  const stub = stubLists({ comments: [root] });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    await screen.findByText("Root comment");
    fireEvent.click(screen.getByRole("button", { name: "Open" }));
    fireEvent.click(screen.getByRole("button", { name: "Expand thread" }));
    const field = await screen.findByRole<HTMLTextAreaElement>("textbox", { name: "Reply" });
    fireEvent.change(field, { target: { value: "Half a reply" } });

    stub.lists.comments = [{ ...root, resolved: true }];
    await act(async () => {
      await queryClient.invalidateQueries({ queryKey: ["comments", "CORE-1"] });
    });
    expect(screen.getByText("Root comment")).toBeTruthy();
    expect(screen.getByRole<HTMLTextAreaElement>("textbox", { name: "Reply" })).toBe(field);
    expect(field.value).toBe("Half a reply");

    fireEvent.click(screen.getByRole("button", { name: "Collapse thread" }));
    await waitFor(() => expect(screen.queryByText("Root comment")).toBeNull());
  } finally {
    view.unmount();
    stub.restore();
  }
});

// Resolve inside a thread closes it once the server takes it, never before: a refusal leaves the
// thread open with the reply the reader was writing, and the Retry that lands closes it.
test("a refused Resolve keeps the thread and its reply, and the one that lands closes it", async () => {
  const root = comment("root-comment", { body: "Root comment" });
  const stub = stubLists({ comments: [root] });
  const queryClient = newQueryClient();
  let refuse = true;
  const resolves: string[] = [];
  const resolveComment = spyOn(api, "resolveComment").mockImplementation(async (id) => {
    resolves.push(id);
    if (refuse) throw new ApiError(503, { error: "the server is down" });
    return { ...root, resolved: true };
  });
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    await screen.findByText("Root comment");
    fireEvent.click(screen.getByRole("button", { name: "Expand thread" }));
    const field = await screen.findByRole<HTMLTextAreaElement>("textbox", { name: "Reply" });
    fireEvent.change(field, { target: { value: "Half a reply" } });

    fireEvent.click(screen.getByRole("button", { name: "Resolve" }));
    await screen.findByText("the server is down");
    expect(screen.getByRole<HTMLTextAreaElement>("textbox", { name: "Reply" })).toBe(field);
    expect(field.value).toBe("Half a reply");

    refuse = false;
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(screen.queryByRole("textbox", { name: "Reply" })).toBeNull());
    expect(resolves).toEqual(["root-comment", "root-comment"]);
  } finally {
    view.unmount();
    resolveComment.mockRestore();
    stub.restore();
  }
});

/** A tab whose one comment the reader resolves: `land` lets the server take the Resolve, after
 *  which the comments read lists the comment resolved, and `refuse` refuses the reply sent. */
function ownResolveOverAReply() {
  const root = comment("root-comment", { body: "Root comment" });
  const stub = stubLists({ comments: [root] });
  const resolution = Promise.withResolvers<void>();
  const reply = Promise.withResolvers<Comment>();
  const listComments = api.listComments as Mock<typeof api.listComments>;
  const spies = [
    spyOn(api, "createComment").mockImplementation(() => reply.promise),
    spyOn(api, "resolveComment").mockImplementation(async () => {
      await resolution.promise;
      stub.lists.comments = [{ ...root, resolved: true }];
      return { ...root, resolved: true };
    }),
  ];
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));
  return {
    land: async () => {
      const reads = listComments.mock.calls.length;
      await act(async () => {
        resolution.resolve();
        await resolution.promise;
      });
      // The read after the Resolve lists the comment resolved, so only a hold keeps its thread.
      await waitFor(() => expect(listComments.mock.calls.length).toBeGreaterThan(reads));
    },
    refuse: async () => {
      await act(async () => {
        reply.reject(new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" }));
        await reply.promise.catch(() => {});
      });
    },
    restore: () => {
      view.unmount();
      for (const spy of spies) spy.mockRestore();
      stub.restore();
    },
    turn: () => view.container.querySelector('[data-turn="comment:root-comment"]'),
  };
}

const refusedText = "Couldn't send — the server is down";

// A decision closes its thread once the server takes it, and the thread's reply stays the
// reader's: the refusal it holds, and the comment that reply answers, outlive the reader's own
// Resolve on a phone as they do at a desktop width.
test("on a phone, the reader's own Resolve keeps the thread composer's refused reply and its comment", async () => {
  const restoreViewport = stubMatchMedia(true);
  const resolve = ownResolveOverAReply();

  try {
    await screen.findByText("Root comment");
    fireEvent.click(screen.getByRole("button", { name: "Reply" }));
    const dialog = await screen.findByRole("dialog", { name: "Thread" });
    const field = within(dialog).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });
    fireEvent.change(field, { target: { value: "Refused reply" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Send" }));
    await resolve.refuse();
    await within(dialog).findByText(refusedText);

    fireEvent.click(within(dialog).getByRole("button", { name: "Resolve" }));
    await resolve.land();
    expect(screen.queryByRole("dialog", { name: "Thread" })).toBeNull();
    const turn = resolve.turn();
    if (turn === null) throw new Error("expected the resolved comment to stay listed");

    fireEvent.click(within(turn as HTMLElement).getByRole("button", { name: "Expand thread" }));
    const reopened = await screen.findByRole("dialog", { name: "Thread" });
    expect(within(reopened).getByText(refusedText)).toBeTruthy();
    expect(
      within(reopened).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" }).value
    ).toBe("Refused reply");
  } finally {
    resolve.restore();
    restoreViewport();
  }
});

// The same, with the reply refused while the Resolve is still out: the decision lands on a thread
// that already shows the refusal.
test("on a phone, a thread reply refused while the reader's Resolve is out outlives the Resolve", async () => {
  const restoreViewport = stubMatchMedia(true);
  const resolve = ownResolveOverAReply();

  try {
    await screen.findByText("Root comment");
    fireEvent.click(screen.getByRole("button", { name: "Reply" }));
    const dialog = await screen.findByRole("dialog", { name: "Thread" });
    const field = within(dialog).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });
    fireEvent.change(field, { target: { value: "Refused reply" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Send" }));
    await waitFor(() => expect(api.createComment).toHaveBeenCalledTimes(1));
    fireEvent.click(within(dialog).getByRole("button", { name: "Resolve" }));
    await waitFor(() => expect(api.resolveComment).toHaveBeenCalledTimes(1));
    await resolve.refuse();
    await within(dialog).findByText(refusedText);

    await resolve.land();
    const turn = resolve.turn();
    if (turn === null) throw new Error("expected the resolved comment to stay listed");
    fireEvent.click(within(turn as HTMLElement).getByRole("button", { name: "Expand thread" }));
    const reopened = await screen.findByRole("dialog", { name: "Thread" });
    expect(within(reopened).getByText(refusedText)).toBeTruthy();
    expect(
      within(reopened).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" }).value
    ).toBe("Refused reply");
  } finally {
    resolve.restore();
    restoreViewport();
  }
});

// The thread card's own reply, at a phone width and at a desktop one: the reader's own Resolve
// closes the thread and keeps the reply's refusal for the thread's return.
for (const width of ["phone", "desktop"] as const) {
  test(`at a ${width} width, the reader's own Resolve keeps the card's refused reply and its comment`, async () => {
    const restoreViewport = width === "phone" ? stubMatchMedia(true) : () => {};
    const resolve = ownResolveOverAReply();

    try {
      await screen.findByText("Root comment");
      fireEvent.click(screen.getByRole("button", { name: "Expand thread" }));
      const field = await screen.findByRole<HTMLTextAreaElement>("textbox", { name: "Reply" });
      const form = field.closest("form");
      if (form === null) throw new Error("expected the card's reply form");
      fireEvent.change(field, { target: { value: "Refused reply" } });
      fireEvent.click(within(form).getByRole("button", { name: "Send" }));
      await resolve.refuse();
      await within(form).findByText(refusedText);

      fireEvent.click(screen.getByRole("button", { name: "Resolve" }));
      await resolve.land();
      await waitFor(() => expect(screen.queryByRole("textbox", { name: "Reply" })).toBeNull());
      const turn = resolve.turn();
      if (turn === null) throw new Error("expected the resolved comment to stay listed");

      fireEvent.click(within(turn as HTMLElement).getByRole("button", { name: "Expand thread" }));
      const reopened = await screen.findByRole<HTMLTextAreaElement>("textbox", { name: "Reply" });
      expect(reopened.value).toBe("Refused reply");
      expect(screen.getByText(refusedText)).toBeTruthy();
    } finally {
      resolve.restore();
      restoreViewport();
    }
  });
}

// The thread composer's refusal is its comment's. Left with Back while its send was out, it comes
// back in that comment's thread: another comment's Reply opens it there, rather than readdressing
// the composer, and its draft, to the other comment.
test("on a phone, another comment's Reply opens the thread whose reply was refused after Back", async () => {
  const restoreViewport = stubMatchMedia(true);
  const stub = stubLists({
    comments: [
      comment("comment-a", { body: "Comment A", created_at: "2026-09-20T00:00:00Z" }),
      comment("comment-b", { body: "Comment B", created_at: "2026-09-20T00:01:00Z" }),
    ],
  });
  const reply = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment").mockImplementation(() => reply.promise);
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));
  const turn = (id: string) => {
    const element = view.container.querySelector<HTMLElement>(`[data-turn="comment:${id}"]`);
    if (element === null) throw new Error(`expected ${id}'s turn`);
    return element;
  };

  try {
    await screen.findByText("Comment A");
    fireEvent.click(within(turn("comment-a")).getByRole("button", { name: "Reply" }));
    let dialog = await screen.findByRole("dialog", { name: "Thread" });
    const field = within(dialog).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });
    fireEvent.change(field, { target: { value: "Reply meant for A" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Send" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    fireEvent.click(within(dialog).getByRole("button", { name: "Back" }));
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Thread" })).toBeNull());
    await act(async () => {
      reply.reject(new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" }));
      await reply.promise.catch(() => {});
    });

    fireEvent.click(within(turn("comment-b")).getByRole("button", { name: "Reply" }));
    dialog = await screen.findByRole("dialog", { name: "Thread" });
    expect(within(dialog).getByText("Comment A")).toBeTruthy();
    expect(within(dialog).queryByText("Comment B")).toBeNull();
    expect(within(dialog).getByText(refusedText)).toBeTruthy();
    expect(
      within(dialog).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" }).value
    ).toBe("Reply meant for A");
    expect(createComment).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    createComment.mockRestore();
    stub.restore();
    restoreViewport();
  }
});

// At a desktop width a comment's Reply answers the same docked composer and send name the phone
// thread composer uses on a phone (`CommentTurn`'s `onReply` prop in `ConversationTab.tsx`), so
// the same hazard applies: a refusal the composer holds for one comment must redirect, not be
// silently overwritten, when the reader taps Reply on a different one.
test("at a desktop width, another comment's Reply redirects onto the one whose refusal it holds", async () => {
  const stub = stubLists({
    comments: [
      comment("comment-a", { body: "Comment A", created_at: "2026-09-20T00:00:00Z" }),
      comment("comment-b", { body: "Comment B", created_at: "2026-09-20T00:01:00Z" }),
    ],
  });
  const reply = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment").mockImplementation(() => reply.promise);
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));
  const turn = (id: string) => {
    const element = view.container.querySelector<HTMLElement>(`[data-turn="comment:${id}"]`);
    if (element === null) throw new Error(`expected ${id}'s turn`);
    return element;
  };

  try {
    await screen.findByText("Comment A");
    fireEvent.click(within(turn("comment-a")).getByRole("button", { name: "Reply" }));
    const composer = screen.getByRole("form", { name: "Comment composer" });
    const field = within(composer).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });
    fireEvent.change(field, { target: { value: "Reply meant for A" } });
    fireEvent.click(within(composer).getByRole("button", { name: "Send" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    await act(async () => {
      reply.reject(new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" }));
      await reply.promise.catch(() => {});
    });
    await within(composer).findByText(refusedText);

    fireEvent.click(within(turn("comment-b")).getByRole("button", { name: "Reply" }));
    expect(within(composer).getByText(/Comment A/)).toBeTruthy();
    expect(within(composer).queryByText(/Comment B/)).toBeNull();
    expect(within(composer).getByText(refusedText)).toBeTruthy();
    expect(field.value).toBe("Reply meant for A");
    expect(createComment).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    createComment.mockRestore();
    stub.restore();
  }
});

// A message's Reply answers the same docked composer at every width (`MessageTurn`'s `onReply`
// prop in `ConversationTab.tsx`, unconditional - not gated on phone viewport the way a
// comment's is), so the hazard, and the redirect that closes it, hold here too.
test("another message's Reply redirects onto the one whose refusal the docked composer holds", async () => {
  const stub = stubLists({
    messages: [
      thread(message("message-1", { body: "Message A", created_at: "2026-09-09T00:00:00Z" })),
      thread(message("message-2", { body: "Message B", created_at: "2026-09-09T00:05:00Z" })),
    ],
  });
  const reply = Promise.withResolvers<Message>();
  const createMessage = spyOn(api, "createMessage").mockImplementation(() => reply.promise);
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));
  const turn = (id: string) => {
    const element = view.container.querySelector<HTMLElement>(`[data-turn="message:${id}"]`);
    if (element === null) throw new Error(`expected ${id}'s turn`);
    return element;
  };

  try {
    await screen.findByText("Message A");
    fireEvent.click(within(turn("message-1")).getByRole("button", { name: "Reply" }));
    const composer = screen.getByRole("form", { name: "Comment composer" });
    const field = within(composer).getByRole<HTMLTextAreaElement>("textbox", { name: "Comment" });
    fireEvent.change(field, { target: { value: "Reply meant for A" } });
    fireEvent.click(within(composer).getByRole("button", { name: "Send" }));
    await waitFor(() => expect(createMessage).toHaveBeenCalledTimes(1));
    await act(async () => {
      reply.reject(new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" }));
      await reply.promise.catch(() => {});
    });
    await within(composer).findByText(refusedText);

    fireEvent.click(within(turn("message-2")).getByRole("button", { name: "Reply" }));
    expect(within(composer).getByText(/Message A/)).toBeTruthy();
    expect(within(composer).queryByText(/Message B/)).toBeNull();
    expect(within(composer).getByText(refusedText)).toBeTruthy();
    expect(field.value).toBe("Reply meant for A");
    expect(createMessage).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    createMessage.mockRestore();
    stub.restore();
  }
});

test(
  "shows Jump to latest until the reader returns to the top",
  withScrollY(async () => {
    const layout = installScrollLayoutMocks();
    const stub = stubLists({ messages: [thread(message("message-1", { body: "A message" }))] });
    const queryClient = newQueryClient();
    const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

    try {
      await screen.findByText("A message");
      await waitFor(() => expect(window.scrollY).toBe(RESTING_SCROLL_Y));
      expect(screen.queryByTestId("jump-to-latest")).toBeNull();

      Object.defineProperty(window, "scrollY", {
        configurable: true,
        value: 4_500,
        writable: true,
      });
      act(() => {
        window.dispatchEvent(new Event("scroll"));
      });
      expect(screen.getByTestId("jump-to-latest")).toBeTruthy();

      Object.defineProperty(window, "scrollY", {
        configurable: true,
        value: RESTING_SCROLL_Y,
        writable: true,
      });
      act(() => {
        window.dispatchEvent(new Event("scroll"));
      });
      await waitFor(() => expect(screen.queryByTestId("jump-to-latest")).toBeNull());
    } finally {
      view.unmount();
      layout.restore();
      stub.restore();
    }
  })
);

test(
  "Jump to latest counts the unread conversations and returns to the newest",
  withScrollY(async () => {
    const layout = installScrollLayoutMocks();
    const stub = stubLists({
      comments: [
        comment("comment-1", { body: "A read comment", created_at: "2026-09-20T00:00:00Z" }),
        comment("comment-2", { body: "An unread comment", created_at: "2026-09-20T00:05:00Z" }),
      ],
      unread: [headEvent(9, "2026-09-20T00:05:00Z")],
    });
    const queryClient = newQueryClient();
    const view = render(tab({ "CORE-1": issueState([], 3) }, true, queryClient));

    try {
      await screen.findByText("An unread comment");
      await waitFor(() => expect(window.scrollY).toBe(RESTING_SCROLL_Y));
      Object.defineProperty(window, "scrollY", {
        configurable: true,
        value: 4_500,
        writable: true,
      });
      act(() => {
        window.dispatchEvent(new Event("scroll"));
      });

      const pill = await screen.findByTestId("jump-to-latest");
      expect(pill.textContent).toBe("Jump to latest · 1 new");
      fireEvent.click(pill);
      await waitFor(() => expect(window.scrollY).toBe(RESTING_SCROLL_Y));
    } finally {
      view.unmount();
      layout.restore();
      stub.restore();
    }
  })
);

// The pill scrolls to the first card on the page and labels itself with a count: both have to
// read the same list. With the filter on Open and the only unread conversation resolved, a count
// taken over the whole set would promise `· 1 new` and land the reader on a card carrying no
// `New` mark, with the conversation it counted not on the page at all.
test(
  "Jump to latest counts the unread conversations the filter leaves on the page",
  withScrollY(async () => {
    const layout = installScrollLayoutMocks();
    const stub = stubLists({
      comments: [
        comment("comment-1", { body: "An open comment", created_at: "2026-09-20T00:00:00Z" }),
        comment("comment-2", {
          body: "A resolved comment",
          created_at: "2026-09-20T00:05:00Z",
          resolved: true,
          resolved_at: "2026-09-20T00:06:00Z",
          resolved_by: alice,
        }),
      ],
      unread: [headEvent(9, "2026-09-20T00:05:00Z")],
    });
    const queryClient = newQueryClient();
    const view = render(tab({ "CORE-1": issueState([], 3) }, true, queryClient));

    try {
      await screen.findByText("A resolved comment");
      await waitFor(() => expect(window.scrollY).toBe(RESTING_SCROLL_Y));
      Object.defineProperty(window, "scrollY", {
        configurable: true,
        value: 4_500,
        writable: true,
      });
      act(() => {
        window.dispatchEvent(new Event("scroll"));
      });
      expect((await screen.findByTestId("jump-to-latest")).textContent).toBe(
        "Jump to latest · 1 new"
      );

      const filters = screen.getByRole("group", { name: "Conversations" });
      fireEvent.click(within(filters).getByRole("button", { name: "Open" }));
      await waitFor(() => expect(screen.queryByText("A resolved comment")).toBeNull());

      expect(screen.getByTestId("jump-to-latest").textContent).toBe("Jump to latest");
    } finally {
      view.unmount();
      layout.restore();
      stub.restore();
    }
  })
);

test(
  "a reader pinned to the top stays pinned when a conversation arrives",
  withScrollY(async () => {
    const layout = installScrollLayoutMocks();
    const stub = stubLists({
      messages: [thread(message("message-1", { body: "A message" }))],
    });
    const queryClient = newQueryClient();
    const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

    try {
      Object.defineProperty(window, "scrollY", { configurable: true, value: 0, writable: true });
      await screen.findByText("A message");
      await waitFor(() => expect(window.scrollY).toBe(RESTING_SCROLL_Y));
      expect(screen.queryByTestId("jump-to-latest")).toBeNull();

      stub.lists.messages = [
        thread(message("message-1", { body: "A message" })),
        thread(
          message("message-2", { body: "Incoming message", created_at: "2026-09-09T00:05:00Z" })
        ),
      ];
      await act(async () => {
        await queryClient.invalidateQueries({ queryKey: ["issue-messages", "CORE-1"] });
      });

      await screen.findByText("Incoming message");
      expect(
        Array.from(view.container.querySelectorAll("[data-turn]")).map((turn) =>
          turn.getAttribute("data-turn")
        )
      ).toEqual(["message:message-2", "message:message-1"]);
      expect(screen.queryByTestId("jump-to-latest")).toBeNull();
      expect(window.scrollY).toBe(RESTING_SCROLL_Y);
    } finally {
      view.unmount();
      layout.restore();
      stub.restore();
    }
  })
);

test("a comment-delivery retry disables when the target no longer advertises the failed mode", async () => {
  const stub = stubLists({
    agents: [agent("worker-session", "Worker")],
    comments: [
      comment("comment-1", {
        body: "@Worker take a look",
        deliveries: [mentionAttempt({ error: "no live session worker" })],
        mentions: [
          { delivery: "steer", session_id: "worker-session", target: "session:worker-session" },
        ],
      }),
    ],
  });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    await screen.findByText(/no live session worker/);
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
    expect(screen.getByText(/session:worker-session no longer supports Send\./)).toBeTruthy();
  } finally {
    view.unmount();
    stub.restore();
  }
});

test("a comment-delivery retry to a role target checks the role's current live holder, not the old one", async () => {
  const holder = agent("new-reviewer-session", "New reviewer");
  const stub = stubLists({
    // The role's live holder changed since the failed attempt and does not advertise steer,
    // even though the delivery's own recorded session_id (the old holder) is a stale reference.
    agents: [{ ...holder, roles: ["reviewer"] }],
    comments: [
      comment("comment-1", {
        body: "@reviewer take a look",
        deliveries: [
          mentionAttempt({
            error: "no live session reviewer",
            session_id: "old-reviewer-session",
            target: "role:reviewer",
          }),
        ],
        mentions: [{ delivery: "steer", session_id: null, target: "role:reviewer" }],
      }),
    ],
  });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    await screen.findByText(/no live session reviewer/);
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
    expect(screen.getByText(/role:reviewer no longer supports Send\./)).toBeTruthy();
  } finally {
    view.unmount();
    stub.restore();
  }
});

test("a comment-delivery retry guards a same-tick double click to exactly one attempt", async () => {
  const stub = stubLists({
    agents: [agent("worker-session", "Worker", ["steer"])],
    comments: [
      comment("comment-1", {
        body: "@Worker take a look",
        deliveries: [mentionAttempt({ error: "no live session worker" })],
      }),
    ],
  });
  const queryClient = newQueryClient();
  let sends = 0;
  const { promise, resolve } = Promise.withResolvers<void>();
  const createCommentDelivery = spyOn(api, "createCommentDelivery").mockImplementation(
    async (id, target, delivery) => {
      sends += 1;
      await promise;
      return mentionAttempt({
        attempt: 2,
        comment_id: id,
        delivery,
        error: null,
        state: "sent",
        target: target ?? "session:worker-session",
      });
    }
  );
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    const retry = await screen.findByRole("button", { name: "Retry" });
    // Both clicks fire in the same task, before any re-render could disable the button - the
    // window a disabled-prop check (which lags on React state) cannot catch.
    fireEvent.click(retry);
    fireEvent.click(retry);
    resolve();
    await waitFor(() => expect(sends).toBe(1));
  } finally {
    view.unmount();
    createCommentDelivery.mockRestore();
    stub.restore();
  }
});

test("a root targeted-message retry checks the role's current holder after a handoff, not the failed attempt's session", async () => {
  const stub = stubLists({
    // The role has since handed off; the live holder does not advertise steer.
    agents: [{ ...agent("new-reviewer-session", "New reviewer", ["btw"]), roles: ["reviewer"] }],
    messages: [
      thread(
        message("message-1", {
          // The failed attempt was recorded against the role's OLD holder.
          deliveries: [
            attempt({
              delivery: "steer",
              error: "no live session old-reviewer-session",
              session_id: "old-reviewer-session",
              state: "failed",
            }),
          ],
          target: "role:reviewer",
        })
      ),
    ],
  });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    // The new holder dropped steer, so the same-mode Retry of a steer attempt is refused.
    const sendNormally = await screen.findByRole("button", { name: "Retry" });
    expect(sendNormally.hasAttribute("disabled")).toBe(true);
    expect(screen.getByRole("button", { name: "Use BTW instead" }).hasAttribute("disabled")).toBe(
      false
    );
  } finally {
    view.unmount();
    stub.restore();
  }
});

test("a reply's targeted-message retry checks the thread's current role holder after a handoff", async () => {
  const stub = stubLists({
    agents: [{ ...agent("new-reviewer-session", "New reviewer", ["btw"]), roles: ["reviewer"] }],
    messages: [
      thread(
        message("message-1", {
          deliveries: [attempt({ delivery: "steer", session_id: "old-reviewer-session" })],
          target: "role:reviewer",
        }),
        [
          // A session answer marks the root "answered," suppressing its own retry row so only
          // the reply's retry row is under test below.
          message("message-3", {
            author: { id: "old-reviewer-session", kind: "session" },
            body: "Looking into it.",
            created_at: "2026-09-09T00:00:45Z",
            in_reply_to: "message-1",
          }),
          message("message-2", {
            body: "Any update?",
            created_at: "2026-09-09T00:01:00Z",
            // The reply's own failed delivery is recorded against the role's OLD holder too.
            deliveries: [
              attempt({
                delivery: "steer",
                error: "no live session old-reviewer-session",
                message_id: "message-2",
                session_id: "old-reviewer-session",
                state: "failed",
              }),
            ],
            in_reply_to: "message-1",
          }),
        ]
      ),
    ],
  });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    await screen.findByText("Any update?");
    // The new holder dropped steer, so the same-mode Retry of a steer attempt is refused.
    const sendNormally = await screen.findByRole("button", { name: "Retry" });
    expect(sendNormally.hasAttribute("disabled")).toBe(true);
    expect(screen.getByRole("button", { name: "Use BTW instead" }).hasAttribute("disabled")).toBe(
      false
    );
  } finally {
    view.unmount();
    stub.restore();
  }
});

test("Conversation hides reply editing while its owner saves a comment", async () => {
  const root = comment("root-comment", { body: "Root comment" });
  const reply = comment("reply-comment", { body: "Reply comment", reply_to: "root-comment" });
  const stub = stubLists({ comments: [root, reply] });
  const save = Promise.withResolvers<Comment>();
  const editComment = spyOn(api, "editComment").mockImplementation(() => save.promise);
  const queryClient = newQueryClient();
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    const card = await screen.findByTestId("margin-comment-root-comment");
    fireEvent.click(screen.getByRole("button", { name: "Expand thread" }));
    await waitFor(() =>
      expect(within(card).getAllByRole("button", { name: "Edit" })).toHaveLength(2)
    );
    fireEvent.click(within(card).getAllByRole("button", { name: "Edit" })[0] as HTMLElement);
    fireEvent.change(screen.getByLabelText("Edit comment"), { target: { value: "Updated root" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() =>
      expect(editComment).toHaveBeenCalledWith("root-comment", { body: "Updated root" })
    );
    expect(within(card).queryAllByRole("button", { name: "Edit" })).toHaveLength(0);
    expect((screen.getByRole("textbox", { name: "Reply" }) as HTMLTextAreaElement).disabled).toBe(
      false
    );

    await act(async () => {
      save.resolve(root);
      await save.promise;
    });
    await waitFor(() =>
      expect(within(card).getAllByRole("button", { name: "Edit" })).toHaveLength(2)
    );
  } finally {
    save.resolve(root);
    view.unmount();
    editComment.mockRestore();
    stub.restore();
  }
});

test("Conversation restores edit controls and the draft when its owner save rejects", async () => {
  const root = comment("root-comment", { body: "Root comment" });
  const reply = comment("reply-comment", { body: "Reply comment", reply_to: "root-comment" });
  const stub = stubLists({ comments: [root, reply] });
  const save = Promise.withResolvers<Comment>();
  const editComment = spyOn(api, "editComment").mockImplementation(() => save.promise);
  const queryClient = newQueryClient();
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));

  try {
    const card = await screen.findByTestId("margin-comment-root-comment");
    fireEvent.click(screen.getByRole("button", { name: "Expand thread" }));
    await waitFor(() =>
      expect(within(card).getAllByRole("button", { name: "Edit" })).toHaveLength(2)
    );
    fireEvent.click(within(card).getAllByRole("button", { name: "Edit" })[0] as HTMLElement);
    fireEvent.change(screen.getByLabelText("Edit comment"), {
      target: { value: "Draft survives" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() =>
      expect(editComment).toHaveBeenCalledWith("root-comment", { body: "Draft survives" })
    );
    expect(within(card).queryAllByRole("button", { name: "Edit" })).toHaveLength(0);

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
    view.unmount();
    editComment.mockRestore();
    stub.restore();
  }
});

interface ThreadsPage {
  readonly cleanup: () => void;
  readonly mark: Mock<typeof api.markThreadRead>;
  readonly turn: (id: string) => HTMLElement;
  readonly viewport: IntersectionControl;
}

/**
 * Renders the Conversation on `lists` with `unread` thread ids unread for alice, the screen in
 * the test's hands, and every write it can make stubbed. `focusItemId` is the item a deep link
 * names.
 */
function renderThreads(lists: Lists, unread: readonly string[], focusItemId?: string): ThreadsPage {
  const viewport = installIntersectionObserver();
  const stub = stubLists(lists);
  const putIssueState = spyOn(api, "putIssueState").mockImplementation(async () => issueState());
  const mark = spyOn(api, "markThreadRead").mockResolvedValue(undefined);
  const scrollIntoView = HTMLElement.prototype.scrollIntoView;
  HTMLElement.prototype.scrollIntoView = () => {};
  const queryClient = newQueryClient();
  seedUnread(queryClient, { issue: "CORE-1" }, unread);
  const view = render(
    tab({ "CORE-1": issueState() }, true, queryClient, false, new Map(), focusItemId)
  );
  return {
    cleanup: () => {
      view.unmount();
      putIssueState.mockRestore();
      mark.mockRestore();
      stub.restore();
      HTMLElement.prototype.scrollIntoView = scrollIntoView;
      viewport.restore();
    },
    mark,
    turn: (id) => {
      const element = view.container.querySelector<HTMLElement>(`[data-turn="${id}"]`);
      if (element === null) throw new Error(`expected the ${id} turn`);
      return element;
    },
    viewport,
  };
}

/** `element` stays on screen as `placement` says for a second, and every write it starts lands. */
async function dwellOn(viewport: IntersectionControl, element: Element, placement?: Placement) {
  jest.useFakeTimers();
  act(() => viewport.show(element, placement));
  act(() => {
    jest.advanceTimersByTime(1_000);
  });
  for (let flush = 0; flush < 5; flush += 1) await act(async () => {});
}

/** A session's reply in a message thread. */
function sessionReply(id: string, parent: string, at: string): Message {
  return message(id, {
    author: { id: "s1", kind: "session" },
    body: `Session reply ${id}`,
    created_at: at,
    in_reply_to: parent,
  });
}

// A link to a message (`/issues/K/messages/<root>`) shows its thread's replies, which a message
// thread always renders, and they mark the thread read like any other view of them.
test("landing on a message thread shows its replies and marks the thread read through the newest", async () => {
  const reply = sessionReply("message-2", "message-1", "2026-09-09T00:05:00.25Z");
  const page = renderThreads(
    { messages: [thread(message("message-1", { body: "Status?" }), [reply])] },
    ["message-1"],
    "message-1"
  );
  try {
    await screen.findByText("Session reply message-2");
    const newest = page.turn("message:message-2");
    expect(page.viewport.observed()).toContain(newest);
    await dwellOn(page.viewport, newest);
    expect(page.mark.mock.calls).toEqual([["message-1", "2026-09-09T00:05:00.25Z"]]);
  } finally {
    page.cleanup();
  }
});

// The root card wraps every reply, so a long exchange can be taller than the screen and never
// three-quarters on it; the mark watches the newest reply instead.
test("a message thread taller than the screen is marked read from its newest reply alone", async () => {
  const first = sessionReply("message-2", "message-1", "2026-09-09T00:05:00Z");
  const second = sessionReply("message-3", "message-1", "2026-09-09T00:06:00Z");
  const page = renderThreads(
    { messages: [thread(message("message-1", { body: "Status?" }), [first, second])] },
    ["message-1"]
  );
  try {
    await screen.findByText("Session reply message-3");
    act(() => page.viewport.show(page.turn("message:message-1"), { height: 3_000, visible: 300 }));
    await dwellOn(page.viewport, page.turn("message:message-3"));
    expect(page.mark.mock.calls).toEqual([["message-1", "2026-09-09T00:06:00Z"]]);
    // The older reply is not what the mark watches.
    expect(page.viewport.observed()).not.toContain(page.turn("message:message-2"));
  } finally {
    page.cleanup();
  }
});

test("a targeted message's thread is marked read once its newest reply is on screen", async () => {
  const reply = sessionReply("message-2", "message-1", "2026-09-09T00:07:00Z");
  const page = renderThreads(
    {
      messages: [
        thread(message("message-1", { body: "Please rebase", target: "session:s1" }), [reply]),
      ],
    },
    ["message-1"]
  );
  try {
    await screen.findByText("Session reply message-2");
    await dwellOn(page.viewport, page.turn("message:message-2"));
    expect(page.mark.mock.calls).toEqual([["message-1", "2026-09-09T00:07:00Z"]]);
  } finally {
    page.cleanup();
  }
});

test("a message thread the viewer has read is never watched for the Replies mark", async () => {
  const reply = sessionReply("message-2", "message-1", "2026-09-09T00:05:00Z");
  const page = renderThreads(
    { messages: [thread(message("message-1", { body: "Status?" }), [reply])] },
    []
  );
  try {
    await screen.findByText("Session reply message-2");
    expect(page.viewport.observed()).not.toContain(page.turn("message:message-2"));
  } finally {
    page.cleanup();
  }
});

// Every comment link opens the comment's thread on landing - a Replies row's, and equally the
// Inbox's, search's or a copied reference's - so its replies are on screen to read.
test("at a desktop width, landing on a comment opens its thread inline and reading it marks it", async () => {
  const root = comment("root-comment", { body: "Root comment" });
  const reply = comment("reply-comment", {
    body: "Reply comment",
    created_at: "2026-09-20T00:01:00Z",
    reply_to: "root-comment",
  });
  const page = renderThreads({ comments: [root, reply] }, ["root-comment"], "root-comment");
  try {
    const replyItem = (await screen.findByText("Reply comment")).closest("li");
    if (replyItem === null) throw new Error("expected the reply's item");
    expect(
      page.turn("comment:root-comment").querySelector("article")?.getAttribute("aria-expanded")
    ).toBe("true");
    await dwellOn(page.viewport, replyItem);
    expect(page.mark.mock.calls).toEqual([["root-comment", reply.created_at]]);
  } finally {
    page.cleanup();
  }
});

test("landing on a comment's reply opens the thread it belongs to and marks that reply", async () => {
  const root = comment("root-comment", { body: "Root comment" });
  const reply = comment("reply-comment", {
    body: "Reply comment",
    created_at: "2026-09-20T00:01:00Z",
    reply_to: "root-comment",
  });
  const page = renderThreads({ comments: [root, reply] }, [], "reply-comment");
  try {
    const marked = await screen.findByText("Reply comment");
    expect(marked.closest("[data-reply]")?.getAttribute("aria-current")).toBe("true");
    expect(page.turn("comment:root-comment").getAttribute("aria-current")).toBe("true");
  } finally {
    page.cleanup();
  }
});

test("on a phone, landing on a comment opens its thread dialog and reading it marks it", async () => {
  const restoreViewport = stubMatchMedia(true);
  const root = comment("root-comment", { body: "Root comment" });
  const reply = comment("reply-comment", {
    body: "Reply comment",
    created_at: "2026-09-20T00:01:00Z",
    reply_to: "root-comment",
  });
  const page = renderThreads({ comments: [root, reply] }, ["root-comment"], "root-comment");
  try {
    const dialog = await screen.findByRole("dialog", { name: "Thread" });
    const replyItem = within(dialog).getByText("Reply comment").closest("li");
    if (replyItem === null) throw new Error("expected the reply's item");
    await dwellOn(page.viewport, replyItem);
    expect(page.mark.mock.calls).toEqual([["root-comment", reply.created_at]]);
  } finally {
    page.cleanup();
    restoreViewport();
  }
});

test("without a link, a comment's thread stays collapsed", async () => {
  const root = comment("root-comment", { body: "Root comment" });
  const reply = comment("reply-comment", { body: "Reply comment", reply_to: "root-comment" });
  const page = renderThreads({ comments: [root, reply] }, ["root-comment"]);
  try {
    await screen.findByText("Root comment");
    expect(screen.queryByText("Reply comment")).toBeNull();
  } finally {
    page.cleanup();
  }
});

// The issue's watermark is the Conversation's to write, and only from the newest conversation:
// a single mark cannot say "I read the third card only". A write the server refuses re-reads the
// stored mark and is sent again while the card stays on screen.
test("the read mark writes the issue's head seq, and retries a refused write", async () => {
  const viewport = installIntersectionObserver();
  const stub = stubLists({
    comments: [comment("comment-1", { body: "Newest" })],
    head: [headEvent(7)],
  });
  const getMyState = spyOn(api, "getMyState").mockImplementation(async () => ({
    "CORE-1": issueState([], 1),
  }));
  let writes = 0;
  const putIssueState = spyOn(api, "putIssueState").mockImplementation(async (_key, input) => {
    writes += 1;
    if (writes === 1) throw new ApiError(503, { code: "UNAVAILABLE", error: "down" });
    return issueState([], input.last_read_seq ?? 0);
  });
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState([], 1) }, true, queryClient));

  try {
    await screen.findByText("Newest");
    const card = view.container.querySelector('[data-turn="comment:comment-1"]');
    if (card === null) throw new Error("expected the newest card");
    await dwellOn(viewport, card);
    expect(putIssueState.mock.calls).toEqual([["CORE-1", { last_read_seq: 7 }]]);
    expect(getMyState).toHaveBeenCalledTimes(1);
    act(() => {
      jest.advanceTimersByTime(1_000);
    });
    for (let flush = 0; flush < 5; flush += 1) await act(async () => {});
    expect(putIssueState.mock.calls).toEqual([
      ["CORE-1", { last_read_seq: 7 }],
      ["CORE-1", { last_read_seq: 7 }],
    ]);
  } finally {
    view.unmount();
    getMyState.mockRestore();
    putIssueState.mockRestore();
    stub.restore();
    viewport.restore();
  }
});
