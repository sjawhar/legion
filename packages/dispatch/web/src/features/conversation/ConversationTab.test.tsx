import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";
import { ApiError, api } from "../../api/client";
import { prependEventToLog } from "../../api/sse";
import type {
  Actor,
  Artifact,
  Comment,
  Event,
  Message,
  UserIssueState,
  UserState,
} from "../../api/types";
import { stubMatchMedia } from "../margin/margin-fixture";
import { KeymapProvider } from "../shell/KeymapProvider";
import { ConversationTab } from "./ConversationTab";
import { answeredWithErrorGuidance, safeRetryGuidance } from "./delivery";

function message(
  id: number,
  body = "A message",
  actor: Actor = { id: "alice", kind: "user" }
): Event {
  return {
    actor,
    created_at: "2026-09-09T00:00:00Z",
    id,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      author: actor,
      body,
      created_at: "2026-09-09T00:00:00Z",
      deliveries: [],
      id: `message-${id}`,
      in_reply_to: null,
      issue_key: "CORE-1",
      target: null,
    },
    seq: id,
    type: "message.created",
  };
}

function issueState(dismissed: string[] = [], lastReadSeq = 0): UserIssueState {
  return { dismissed, last_read_seq: lastReadSeq, pinned: false, seq: 0 };
}

// Delivery attempts are dated relative to the run: the dashboard only offers a
// same-mode Retry while an attempt is inside the stream's duplicate window, so a
// fixture frozen at an absolute date would age out of every retry assertion.
const recentAttemptAt = new Date(Date.now() - 60_000).toISOString();

const primaryDocument: Artifact = {
  created_at: "2026-09-09T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
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
  created_at: "2026-09-09T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-secondary",
  issue_key: "CORE-1",
  kind: "doc",
  name: "supporting-document.md",
  primary: false,
  project: "CORE",
  slug: "supporting-document",
  versions: [],
};

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
  issueArtifacts: ReadonlyMap<string, Artifact> = new Map()
): ReactNode {
  return (
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <KeymapProvider>
        <QueryClientProvider client={queryClient}>
          <ConversationTab
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

// The sticky composer sits at document top 0 and the newest turn below it. `useFollowLatest`
// reads their bounding rectangles only to place the newest turn beneath the composer, so these
// tests fake that minimal layout rather than exercising a real browser layout engine.
const COMPOSER_HEIGHT = 140;
const NEWEST_TURN_DOC_TOP = 300;
const RESTING_SCROLL_Y = NEWEST_TURN_DOC_TOP - COMPOSER_HEIGHT;

function installScrollLayoutMocks(): { restore: () => void } {
  const rectSpy = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    const isComposer = this.getAttribute("aria-label") === "Comment composer";
    const isTurn = this.hasAttribute("data-event-seq");
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

test("observes message rows only while the Conversation panel is visible", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  const originalListAgents = api.listAgents;
  const originalIntersectionObserver = globalThis.IntersectionObserver;
  const observations: Element[] = [];
  class IntersectionObserverStub {
    disconnect(): void {}

    observe(target: Element): void {
      observations.push(target);
    }

    takeRecords(): IntersectionObserverEntry[] {
      return [];
    }

    unobserve(): void {}
  }

  globalThis.IntersectionObserver =
    IntersectionObserverStub as unknown as typeof IntersectionObserver;
  const queryClient = newQueryClient();
  let unmount: (() => void) | undefined;

  try {
    api.getIssueEvents = async () => [message(1)];
    api.listAgents = async () => [];
    queryClient.setQueryData(["agents"], []);
    queryClient.setQueryData(["events", "CORE-1"], {
      pageParams: [null],
      pages: [[message(1)]],
    });
    const view = render(tab({ "CORE-1": issueState() }, false, queryClient));
    unmount = view.unmount;

    if (view.container.querySelector('[data-event-seq="1"]') === null) {
      throw new Error("expected the cached message turn");
    }
    expect(observations.filter((element) => view.container.contains(element))).toHaveLength(0);

    view.rerender(tab({ "CORE-1": issueState() }, true, queryClient));
    await waitFor(() =>
      expect(observations.filter((element) => view.container.contains(element))).toHaveLength(1)
    );
    expect(
      observations
        .filter((element) => view.container.contains(element))[0]
        ?.getAttribute("data-event-seq")
    ).toBe("1");
  } finally {
    unmount?.();
    api.getIssueEvents = originalGetIssueEvents;
    api.listAgents = originalListAgents;
    globalThis.IntersectionObserver = originalIntersectionObserver;
  }
});

test("Conversation holds Reply while its composer sends", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  const originalListAgents = api.listAgents;
  const sent = Promise.withResolvers<Comment>();
  const createComment = spyOn(api, "createComment").mockReturnValueOnce(sent.promise);
  const queryClient = newQueryClient();
  let unmount: (() => void) | undefined;

  try {
    const event = message(1, "Earlier message");
    api.getIssueEvents = async () => [event];
    api.listAgents = async () => [];
    queryClient.setQueryData(["events", "CORE-1"], { pageParams: [null], pages: [[event]] });
    unmount = render(tab({ "CORE-1": issueState() }, true, queryClient)).unmount;

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
    unmount?.();
    createComment.mockRestore();
    api.getIssueEvents = originalGetIssueEvents;
    api.listAgents = originalListAgents;
  }
});

test("retry replays every pin operation rejected by the state-write queue", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  const originalGetMyState = api.getMyState;
  const originalListAgents = api.listAgents;
  const originalPutIssueState = api.putIssueState;
  const queryClient = newQueryClient();
  const writes: string[][] = [];
  let failures = 2;
  let unmount: (() => void) | undefined;

  try {
    api.getIssueEvents = async () => [message(1), message(2, "Another message")];
    api.getMyState = async () => ({ "CORE-1": issueState() });
    api.listAgents = async () => [];
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
    await screen.findByText("Another message");
    const pins = screen.getAllByRole("button", { name: "Pin" });
    expect(pins.map((button) => button.getAttribute("aria-pressed"))).toEqual(["false", "false"]);
    for (const button of pins) {
      fireEvent.click(button);
    }

    await waitFor(() =>
      expect(screen.getByRole("alert").textContent).toContain("Couldn't save pin — retry")
    );
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));

    await waitFor(() =>
      expect(writes.at(-1)).toEqual(["pinned_items:event:2", "pinned_items:event:1"])
    );
  } finally {
    unmount?.();
    api.getIssueEvents = originalGetIssueEvents;
    api.getMyState = originalGetMyState;
    api.listAgents = originalListAgents;
    api.putIssueState = originalPutIssueState;
  }
});

test("a scroll event measures the reader position in O(1) rect reads", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  const originalListAgents = api.listAgents;
  const queryClient = newQueryClient();
  const events = Array.from({ length: 250 }, (_, index) => message(index + 1, `Message ${index}`));
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
    document.querySelector("[data-event-seq]")
  );
  let unmount: (() => void) | undefined;

  try {
    api.getIssueEvents = async () => events;
    api.listAgents = async () => [];
    queryClient.setQueryData(["agents"], []);
    queryClient.setQueryData<UserState>(["user-state"], { "CORE-1": issueState() });
    queryClient.setQueryData(["events", "CORE-1"], { pageParams: [null], pages: [events] });

    unmount = render(tab({ "CORE-1": issueState() }, true, queryClient)).unmount;
    await screen.findByText("Message 0");

    rectSpy.mockClear();
    window.dispatchEvent(new Event("scroll"));

    // ViewportAnchor measures only in the commit phase, immediately before a content reflow; a
    // scroll alone must do no layout reads. This guards both against O(n) scans over every loaded
    // turn and unnecessary per-scroll layout work.
    expect(rectSpy.mock.calls.length).toBe(0);
  } finally {
    unmount?.();
    api.getIssueEvents = originalGetIssueEvents;
    api.listAgents = originalListAgents;
    rectSpy.mockRestore();
    elementFromPointSpy.mockRestore();
  }
});

test("the activity toggle hides system lines without remounting messages and persists its choice", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  const originalListAgents = api.listAgents;
  const queryClient = newQueryClient();
  let unmount: (() => void) | undefined;

  try {
    window.localStorage.removeItem("dispatch.conversation.showActivity");
    api.getIssueEvents = async () => [
      {
        ...message(1),
        payload: {},
        type: "issue.created",
      } as Event,
      message(2, "Update"),
    ];
    api.listAgents = async () => [];

    unmount = render(tab({ "CORE-1": issueState() }, true, queryClient)).unmount;
    const update = await screen.findByText("Update");
    const toggle = screen.getByRole("checkbox", { name: "Show activity" });
    expect(screen.getByText("created the issue")).toBeTruthy();

    fireEvent.click(toggle);

    expect(screen.queryByText("created the issue")).toBeNull();
    expect(screen.getByText("Update")).toBe(update);
    expect(window.localStorage.getItem("dispatch.conversation.showActivity")).toBe("false");
  } finally {
    unmount?.();
    window.localStorage.removeItem("dispatch.conversation.showActivity");
    api.getIssueEvents = originalGetIssueEvents;
    api.listAgents = originalListAgents;
  }
});

test("agent authors render the registry title and fall back to a short id", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  const originalListAgents = api.listAgents;
  const queryClient = newQueryClient();
  let unmount: (() => void) | undefined;

  try {
    api.getIssueEvents = async () => [
      message(1, "Planned", { id: "0123456789abcdef", kind: "session" }),
      message(2, "Unplanned", { id: "fedcba9876543210", kind: "session" }),
    ];
    api.listAgents = async () => [
      {
        capabilities: [],
        dir: "/w",
        last_seen: 1,
        last_activity: null,
        open_asks: 0,
        machine_id: "m",
        roles: [],
        session_id: "0123456789abcdef",
        title: "Planner",
      },
    ];

    unmount = render(tab({ "CORE-1": issueState() }, true, queryClient)).unmount;
    const turns = await screen.findByRole("list", { name: "Conversation turns" });
    await within(turns).findByText("Planner");
    expect(within(turns).getByText("session:fedcba98…")).toBeTruthy();
  } finally {
    unmount?.();
    api.getIssueEvents = originalGetIssueEvents;
    api.listAgents = originalListAgents;
  }
});

test("hides targeted-message retries on a closed issue", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  const originalListAgents = api.listAgents;
  const queryClient = newQueryClient();
  let unmount: (() => void) | undefined;

  try {
    const question: Event = {
      actor: { id: "alice", kind: "user" },
      created_at: "2026-09-12T00:00:00Z",
      id: 1,
      issue_key: "CORE-1",
      notify: false,
      payload: {
        author: { id: "alice", kind: "user" },
        body: "Can this ship?",
        created_at: "2026-09-12T00:00:00Z",
        deliveries: [],
        id: "message-1",
        in_reply_to: null,
        issue_key: "CORE-1",
        target: "session:s1",
      },
      seq: 1,
      type: "message.created",
    };
    const failedDelivery: Event = {
      actor: { id: "alice", kind: "user" },
      created_at: recentAttemptAt,
      id: 2,
      issue_key: "CORE-1",
      notify: false,
      payload: {
        attempt: 1,
        delivery: "btw",
        error: "no live session s1",
        message_id: "message-1",
        session_id: "s1",
        state: "failed",
        title: "planner",
      },
      seq: 2,
      type: "message.delivery",
    };
    api.getIssueEvents = async () => [question, failedDelivery];
    api.listAgents = async () => [];

    unmount = render(tab({ "CORE-1": issueState() }, true, queryClient, true)).unmount;
    await screen.findByText("Failed: no live session s1");

    expect(screen.queryByRole("button", { name: "Use BTW instead" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Use Send instead" })).toBeNull();
  } finally {
    unmount?.();
    api.getIssueEvents = originalGetIssueEvents;
    api.listAgents = originalListAgents;
  }
});

// The issue feed reads an attempt from its receipts. The session's error reply appends the
// attempt's second receipt as that session, after Dispatch's own `sent` one; that attempt gets a
// mode change, not a same-mode Retry whose repeated key the stream has already stored.
test("a targeted message its session answered with an error offers a mode change, not Retry", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  const originalListAgents = api.listAgents;
  const queryClient = newQueryClient();
  let unmount: (() => void) | undefined;

  try {
    const question: Event = {
      actor: { id: "alice", kind: "user" },
      created_at: "2026-09-12T00:00:00Z",
      id: 1,
      issue_key: "CORE-1",
      notify: false,
      payload: {
        author: { id: "alice", kind: "user" },
        body: "Can this ship?",
        created_at: "2026-09-12T00:00:00Z",
        deliveries: [],
        id: "message-1",
        in_reply_to: null,
        issue_key: "CORE-1",
        target: "session:s1",
      },
      seq: 1,
      type: "message.created",
    };
    const receipt = (seq: number, actor: Actor, state: "sent" | "failed"): Event => ({
      actor,
      created_at: recentAttemptAt,
      id: seq,
      issue_key: "CORE-1",
      notify: false,
      payload: {
        attempt: 1,
        delivery: "btw",
        ...(state === "failed" ? { error: "side turn failed" } : {}),
        message_id: "message-1",
        session_id: "s1",
        state,
        title: actor.kind === "user" ? "planner" : "",
      },
      seq,
      type: "message.delivery",
    });
    const sent = receipt(2, { id: "alice", kind: "user" }, "sent");
    const answeredError = receipt(3, { id: "s1", kind: "session" }, "failed");
    api.getIssueEvents = async () => [question, sent, answeredError];
    api.listAgents = async () => [];

    unmount = render(tab({ "CORE-1": issueState() }, true, queryClient)).unmount;
    await screen.findByText(`Failed: side turn failed. ${answeredWithErrorGuidance("card")}`);
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
    expect(screen.getByRole("button", { name: "Use Send instead" })).toBeTruthy();
  } finally {
    unmount?.();
    api.getIssueEvents = originalGetIssueEvents;
    api.listAgents = originalListAgents;
  }
});

// A mention's receipt carries the same mark. The mention list has no mode-change action, so the
// attempt gets no Retry and is pointed at a new comment; the failure Dispatch recorded for the
// same attempt keeps its Retry.
test("a mention its session answered with an error is pointed at a new comment, not Retry", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  const originalListAgents = api.listAgents;
  const queryClient = newQueryClient();
  let unmount: (() => void) | undefined;

  try {
    const comment = (target: string, session: string, seq: number): Event =>
      ({
        actor: { id: "alice", kind: "user" },
        created_at: "2026-09-14T00:00:00Z",
        id: seq,
        issue_key: "CORE-1",
        notify: false,
        payload: {
          anchor: null,
          artifact_name: "",
          ask_id: null,
          author: { id: "alice", kind: "user" },
          body: `@${session} take a look`,
          created_at: "2026-09-14T00:00:00Z",
          deliveries: [],
          edited_at: null,
          id: `comment-${seq}`,
          issue_key: "CORE-1",
          mentions: [{ delivery: "btw", session_id: session, target }],
          reply_to: null,
          resolved: false,
          resolved_at: null,
          resolved_by: null,
          suggestion: null,
          turn: null,
        },
        seq,
        type: "comment.created",
      }) as Event;
    const failed = (seq: number, commentSeq: number, session: string, actor: Actor): Event =>
      ({
        actor,
        created_at: recentAttemptAt,
        id: seq,
        issue_key: "CORE-1",
        notify: false,
        payload: {
          attempt: 1,
          comment_id: `comment-${commentSeq}`,
          delivery: "btw",
          error: `${session} failed`,
          reply_id: null,
          session_id: session,
          state: "failed",
          target: `session:${session}`,
        },
        seq,
        type: "comment.delivery",
      }) as Event;
    api.getIssueEvents = async () => [
      comment("session:worker", "worker", 1),
      failed(2, 1, "worker", { id: "worker", kind: "session" }),
      comment("session:reviewer", "reviewer", 3),
      failed(4, 3, "reviewer", { id: "alice", kind: "user" }),
    ];
    api.listAgents = async () => [];

    unmount = render(tab({ "CORE-1": issueState() }, true, queryClient)).unmount;
    await screen.findByText(
      `session:worker · failed · worker failed. ${answeredWithErrorGuidance("mention")}`
    );
    screen.getByText(
      `session:reviewer · failed · reviewer failed. ${safeRetryGuidance("mention", "reviewer failed")}`
    );
    expect(screen.getAllByRole("button", { name: "Retry" })).toHaveLength(1);
    unmount();

    // A closed issue shows no Retry and takes no new comment, so neither sentence is said: the
    // same rule the card follows, where each sentence names a control in the retry row.
    unmount = render(tab({ "CORE-1": issueState() }, true, queryClient, true)).unmount;
    await screen.findByText("session:worker · failed · worker failed");
    screen.getByText("session:reviewer · failed · reviewer failed");
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
  } finally {
    unmount?.();
    api.getIssueEvents = originalGetIssueEvents;
    api.listAgents = originalListAgents;
  }
});

test("a targeted message asked by a session names that session as the asker", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  const originalListAgents = api.listAgents;
  const queryClient = newQueryClient();
  let unmount: (() => void) | undefined;

  try {
    const asker = { id: "s2", kind: "session", origin: { session_title: "reviewer" } } as const;
    const question: Event = {
      actor: asker,
      created_at: "2026-09-12T00:00:00Z",
      id: 1,
      issue_key: "CORE-1",
      notify: false,
      payload: {
        author: asker,
        body: "Is the build green?",
        created_at: "2026-09-12T00:00:00Z",
        deliveries: [],
        id: "message-2",
        in_reply_to: null,
        issue_key: "CORE-1",
        target: "session:s1",
      },
      seq: 1,
      type: "message.created",
    };
    const sent: Event = {
      actor: asker,
      created_at: recentAttemptAt,
      id: 2,
      issue_key: "CORE-1",
      notify: false,
      payload: {
        attempt: 1,
        delivery: "btw",
        message_id: "message-2",
        session_id: "s1",
        state: "sent",
        title: "planner",
      },
      seq: 2,
      type: "message.delivery",
    };
    api.getIssueEvents = async () => [question, sent];
    api.listAgents = async () => [];

    unmount = render(tab({ "CORE-1": issueState() }, true, queryClient)).unmount;
    await screen.findByText("Asking planner (BTW) ·");

    expect(screen.getByText("reviewer")).toBeTruthy();
    expect(screen.queryByText("alice")).toBeNull();
  } finally {
    unmount?.();
    api.getIssueEvents = originalGetIssueEvents;
    api.listAgents = originalListAgents;
  }
});

test("renders a message composer for an open issue but not a closed one", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  const originalListAgents = api.listAgents;
  const queryClient = newQueryClient();
  let unmount: (() => void) | undefined;

  try {
    api.getIssueEvents = async () => [message(1)];
    api.listAgents = async () => [];
    const view = render(tab({ "CORE-1": issueState() }, true, queryClient));
    unmount = view.unmount;

    await screen.findByRole("form", { name: "Comment composer" });
    view.rerender(tab({ "CORE-1": issueState() }, true, queryClient, true));

    expect(screen.queryByRole("form", { name: "Comment composer" })).toBeNull();
  } finally {
    unmount?.();
    api.getIssueEvents = originalGetIssueEvents;
    api.listAgents = originalListAgents;
  }
});

test("renders an anchored comment with the owner controls in Conversation", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  const originalListAgents = api.listAgents;
  const queryClient = newQueryClient();
  let unmount: (() => void) | undefined;
  const comment: Event = {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-09-12T00:00:00Z",
    id: 1,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      anchor: {
        artifact_id: "artifact-secondary",
        block_id: null,
        mark_id: "mark-1",
        orphaned: true,
        quote: "Anchored source",
        version: 1,
      },
      artifact_name: "secondary",
      ask_id: null,
      author: { id: "alice", kind: "user" },
      body: "Please revise this.",
      created_at: "2026-09-12T00:00:00Z",
      deliveries: [],
      edited_at: null,
      id: "comment-1",
      issue_key: "CORE-1",
      mentions: [],
      reply_to: null,
      resolved: false,
      resolved_at: null,
      resolved_by: null,
      suggestion: null,
      turn: null,
    },
    seq: 1,
    type: "comment.created",
  };

  try {
    api.getIssueEvents = async () => [comment];
    api.listAgents = async () => [];
    queryClient.setQueryData(["whoami"], { login: "alice" });
    unmount = render(
      tab(
        { "CORE-1": issueState() },
        true,
        queryClient,
        false,
        new Map([["artifact-secondary", secondaryDocument]])
      )
    ).unmount;
    await screen.findByText("Please revise this.");
    const timelineComment = screen.getByTestId("margin-comment-comment-1");
    expect(timelineComment.textContent).toContain("Anchored source");
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
    unmount?.();
    api.getIssueEvents = originalGetIssueEvents;
    api.listAgents = originalListAgents;
  }
});

test("hides resolved comment turns behind their disclosure", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  const originalListAgents = api.listAgents;
  const queryClient = newQueryClient();
  let unmount: (() => void) | undefined;
  const resolvedComment: Extract<Event, { type: "comment.resolved" }> = {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-09-12T00:01:00Z",
    id: 2,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      anchor: null,
      artifact_name: "spec",
      ask_id: null,
      author: { id: "alice", kind: "user" },
      body: "Resolved timeline comment",
      created_at: "2026-09-12T00:00:00Z",
      deliveries: [],
      edited_at: null,
      id: "comment-resolved",
      issue_key: "CORE-1",
      mentions: [],
      reply_to: null,
      resolved: true,
      resolved_at: "2026-09-12T00:01:00Z",
      resolved_by: { id: "alice", kind: "user" },
      suggestion: null,
      turn: null,
    },
    seq: 2,
    type: "comment.resolved",
  };

  try {
    api.getIssueEvents = async () => [resolvedComment];
    api.listAgents = async () => [];
    unmount = render(tab({ "CORE-1": issueState() }, true, queryClient)).unmount;

    await screen.findByRole("button", { name: "Resolved (1)" });
    expect(screen.queryByText("Resolved timeline comment")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Resolved (1)" }));
    expect(await screen.findByText("Resolved timeline comment")).toBeTruthy();
  } finally {
    unmount?.();
    api.getIssueEvents = originalGetIssueEvents;
    api.listAgents = originalListAgents;
  }
});

// A thread the reader has open is in their hand: resolving its comment - anyone's resolve, here
// arriving on the stream - leaves the turn, its open thread and the reply they are writing where
// they are, and the turn takes the resolved filter only once they collapse the thread.
test("a comment resolved while its thread is open stays, with its reply, until the thread closes", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  const originalListAgents = api.listAgents;
  const queryClient = newQueryClient();
  const root = commentEvent(1, "root-comment", "Root comment");
  const resolved: Event = {
    ...root,
    id: 2,
    payload: {
      ...root.payload,
      resolved: true,
      resolved_at: "2026-09-20T00:01:00Z",
      resolved_by: { id: "bob", kind: "user" },
    },
    seq: 2,
    type: "comment.resolved",
  };
  let unmount: (() => void) | undefined;

  try {
    api.getIssueEvents = async () => [root];
    api.listAgents = async () => [];
    unmount = render(tab({ "CORE-1": issueState() }, true, queryClient)).unmount;
    await screen.findByText("Root comment");
    fireEvent.click(screen.getByRole("button", { name: "Expand thread" }));
    const field = await screen.findByRole<HTMLTextAreaElement>("textbox", { name: "Reply" });
    fireEvent.change(field, { target: { value: "Half a reply" } });

    act(() => {
      prependEventToLog(queryClient, resolved);
    });
    await screen.findByRole("button", { name: "Resolved (1)" });
    expect(screen.getByText("Root comment")).toBeTruthy();
    expect(screen.getByRole<HTMLTextAreaElement>("textbox", { name: "Reply" })).toBe(field);
    expect(field.value).toBe("Half a reply");

    fireEvent.click(screen.getByRole("button", { name: "Collapse thread" }));
    await waitFor(() => expect(screen.queryByText("Root comment")).toBeNull());
  } finally {
    unmount?.();
    api.getIssueEvents = originalGetIssueEvents;
    api.listAgents = originalListAgents;
  }
});

// Resolve inside a thread closes it once the server takes it, never before: a refusal leaves the
// thread open with the reply the reader was writing, and the Retry that lands closes it.
test("a refused Resolve keeps the thread and its reply, and the one that lands closes it", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  const originalListAgents = api.listAgents;
  const originalResolveComment = api.resolveComment;
  const queryClient = newQueryClient();
  const root = commentEvent(1, "root-comment", "Root comment");
  let refuse = true;
  const resolves: string[] = [];
  let unmount: (() => void) | undefined;

  try {
    api.getIssueEvents = async () => [root];
    api.listAgents = async () => [];
    api.resolveComment = async (id) => {
      resolves.push(id);
      if (refuse) throw new ApiError(503, { error: "the server is down" });
      return { ...root.payload, deliveries: [], mentions: [], resolved: true };
    };
    unmount = render(tab({ "CORE-1": issueState() }, true, queryClient)).unmount;
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
    unmount?.();
    api.getIssueEvents = originalGetIssueEvents;
    api.listAgents = originalListAgents;
    api.resolveComment = originalResolveComment;
  }
});

// With resolved comments shown, a resolved comment stays in the list, so its thread does too.
test("with resolved comments shown, a Resolve that lands keeps its thread open", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  const originalListAgents = api.listAgents;
  const originalResolveComment = api.resolveComment;
  const queryClient = newQueryClient();
  const earlier = commentEvent(1, "earlier-comment", "Earlier comment");
  const earlierResolved: Event = {
    ...earlier,
    id: 2,
    payload: {
      ...earlier.payload,
      resolved: true,
      resolved_at: "2026-09-20T00:01:00Z",
      resolved_by: { id: "bob", kind: "user" },
    },
    seq: 2,
    type: "comment.resolved",
  };
  const root = commentEvent(3, "root-comment", "Root comment");
  const resolved = Promise.withResolvers<Comment>();
  let unmount: (() => void) | undefined;

  try {
    api.getIssueEvents = async () => [earlier, earlierResolved, root];
    api.listAgents = async () => [];
    api.resolveComment = () => resolved.promise;
    unmount = render(tab({ "CORE-1": issueState() }, true, queryClient)).unmount;
    fireEvent.click(await screen.findByRole("button", { name: "Resolved (1)" }));
    const turn = (await screen.findByText("Root comment")).closest("li");
    if (turn === null) throw new Error("expected the root comment's turn");
    fireEvent.click(within(turn).getByRole("button", { name: "Expand thread" }));
    const field = await within(turn).findByRole<HTMLTextAreaElement>("textbox", { name: "Reply" });
    fireEvent.change(field, { target: { value: "Half a reply" } });

    fireEvent.click(within(turn).getByRole("button", { name: "Resolve" }));
    await act(async () => {
      resolved.resolve({ ...root.payload, deliveries: [], mentions: [], resolved: true });
      await resolved.promise;
    });
    await waitFor(() => expect(within(turn).queryByText("Saving…")).toBeNull());
    expect(within(turn).getByRole<HTMLTextAreaElement>("textbox", { name: "Reply" })).toBe(field);
    expect(field.value).toBe("Half a reply");
  } finally {
    unmount?.();
    api.getIssueEvents = originalGetIssueEvents;
    api.listAgents = originalListAgents;
    api.resolveComment = originalResolveComment;
  }
});

/** A tab whose one comment the reader resolves: `land` lets the server take the Resolve, after
 *  which the events read lists the comment resolved, and `refuse` refuses the reply sent. */
function ownResolveOverAReply() {
  const root = commentEvent(1, "root-comment", "Root comment");
  const resolvedRoot: Event = {
    ...root,
    id: 2,
    payload: {
      ...root.payload,
      resolved: true,
      resolved_at: "2026-09-20T00:01:00Z",
      resolved_by: { id: "alice", kind: "user" },
    },
    seq: 2,
    type: "comment.resolved",
  };
  const resolution = Promise.withResolvers<void>();
  const reply = Promise.withResolvers<Comment>();
  let resolvedOnServer = false;
  const spies = [
    spyOn(api, "getIssueEvents").mockImplementation(async () =>
      resolvedOnServer ? [resolvedRoot, root] : [root]
    ),
    spyOn(api, "listAgents").mockImplementation(async () => []),
    spyOn(api, "createComment").mockImplementation(() => reply.promise),
    spyOn(api, "resolveComment").mockImplementation(async () => {
      await resolution.promise;
      resolvedOnServer = true;
      return { ...root.payload, deliveries: [], mentions: [], resolved: true };
    }),
  ];
  const queryClient = newQueryClient();
  const view = render(tab({ "CORE-1": issueState() }, true, queryClient));
  const turn = () => view.container.querySelector('[data-turn="comment:root-comment"]');
  return {
    land: async () => {
      await act(async () => {
        resolution.resolve();
        await resolution.promise;
      });
      // The read after the Resolve lists the comment resolved, so only a hold keeps it.
      await screen.findByRole("button", { name: "Resolved (1)" });
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
    },
    turn,
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
  const a = commentEvent(1, "comment-a", "Comment A");
  const b = commentEvent(2, "comment-b", "Comment B");
  const reply = Promise.withResolvers<Comment>();
  const getIssueEvents = spyOn(api, "getIssueEvents").mockImplementation(async () => [b, a]);
  const listAgents = spyOn(api, "listAgents").mockImplementation(async () => []);
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
    getIssueEvents.mockRestore();
    listAgents.mockRestore();
    createComment.mockRestore();
    restoreViewport();
  }
});

// At a desktop width a comment's Reply answers the same docked composer and send name the phone
// thread composer uses on a phone (`ConversationTab.tsx:1456`), so the same hazard applies: a
// refusal the composer holds for one comment must redirect, not be silently overwritten, when
// the reader taps Reply on a different one.
test("at a desktop width, another comment's Reply redirects onto the one whose refusal it holds", async () => {
  const a = commentEvent(1, "comment-a", "Comment A");
  const b = commentEvent(2, "comment-b", "Comment B");
  const reply = Promise.withResolvers<Comment>();
  const getIssueEvents = spyOn(api, "getIssueEvents").mockImplementation(async () => [b, a]);
  const listAgents = spyOn(api, "listAgents").mockImplementation(async () => []);
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
    getIssueEvents.mockRestore();
    listAgents.mockRestore();
    createComment.mockRestore();
  }
});

// A message's Reply answers the same docked composer at every width (`ConversationTab.tsx:1422`,
// unconditional - not gated on phone viewport the way a comment's is), so the hazard, and the
// redirect that closes it, hold here too.
test("another message's Reply redirects onto the one whose refusal the docked composer holds", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  const originalListAgents = api.listAgents;
  const reply = Promise.withResolvers<Message>();
  const createMessage = spyOn(api, "createMessage").mockImplementation(() => reply.promise);
  const queryClient = newQueryClient();
  let unmount: (() => void) | undefined;

  try {
    const a = message(1, "Message A");
    const b = message(2, "Message B");
    api.getIssueEvents = async () => [b, a];
    api.listAgents = async () => [];
    const view = render(tab({ "CORE-1": issueState() }, true, queryClient));
    unmount = view.unmount;
    const turn = (id: number) => {
      const element = view.container.querySelector<HTMLElement>(`[data-turn="message:${id}"]`);
      if (element === null) throw new Error(`expected message ${id}'s turn`);
      return element;
    };

    await screen.findByText("Message A");
    fireEvent.click(within(turn(1)).getByRole("button", { name: "Reply" }));
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

    fireEvent.click(within(turn(2)).getByRole("button", { name: "Reply" }));
    expect(within(composer).getByText(/Message A/)).toBeTruthy();
    expect(within(composer).queryByText(/Message B/)).toBeNull();
    expect(within(composer).getByText(refusedText)).toBeTruthy();
    expect(field.value).toBe("Reply meant for A");
    expect(createMessage).toHaveBeenCalledTimes(1);
  } finally {
    unmount?.();
    createMessage.mockRestore();
    api.getIssueEvents = originalGetIssueEvents;
    api.listAgents = originalListAgents;
  }
});

test("shows Jump to latest until the reader returns to the top", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  const originalListAgents = api.listAgents;
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  const layout = installScrollLayoutMocks();
  const queryClient = newQueryClient();
  let unmount: (() => void) | undefined;

  try {
    api.getIssueEvents = async () => [message(1)];
    api.listAgents = async () => [];
    unmount = render(tab({ "CORE-1": issueState() }, true, queryClient)).unmount;

    await screen.findByText("A message");
    await waitFor(() => expect(window.scrollY).toBe(RESTING_SCROLL_Y));
    expect(screen.queryByTestId("jump-to-latest")).toBeNull();

    Object.defineProperty(window, "scrollY", { configurable: true, value: 4_500, writable: true });
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
    unmount?.();
    api.getIssueEvents = originalGetIssueEvents;
    api.listAgents = originalListAgents;
    layout.restore();
    if (scrollY === undefined) {
      Reflect.deleteProperty(window, "scrollY");
    } else {
      Object.defineProperty(window, "scrollY", scrollY);
    }
  }
});

test("a reader pinned to the top stays pinned when a new turn arrives", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  const originalListAgents = api.listAgents;
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  const layout = installScrollLayoutMocks();
  const queryClient = newQueryClient();
  let unmount: (() => void) | undefined;

  try {
    Object.defineProperty(window, "scrollY", { configurable: true, value: 0, writable: true });
    api.getIssueEvents = async () => [message(1)];
    api.listAgents = async () => [];
    unmount = render(tab({ "CORE-1": issueState() }, true, queryClient)).unmount;

    await screen.findByText("A message");
    await waitFor(() => expect(window.scrollY).toBe(RESTING_SCROLL_Y));
    expect(screen.queryByTestId("jump-to-latest")).toBeNull();

    act(() => {
      queryClient.setQueryData(["events", "CORE-1"], {
        pageParams: [null],
        pages: [[message(1), message(2, "Incoming message")]],
      });
    });

    await screen.findByText("Incoming message");
    const turns = document.querySelectorAll("[data-turn]");
    expect(Array.from(turns).map((turn) => turn.getAttribute("data-turn"))).toEqual([
      "message:2",
      "message:1",
    ]);
    expect(screen.queryByTestId("jump-to-latest")).toBeNull();
    expect(window.scrollY).toBe(RESTING_SCROLL_Y);
  } finally {
    unmount?.();
    api.getIssueEvents = originalGetIssueEvents;
    api.listAgents = originalListAgents;
    layout.restore();
    if (scrollY === undefined) {
      Reflect.deleteProperty(window, "scrollY");
    } else {
      Object.defineProperty(window, "scrollY", scrollY);
    }
  }
});

test("Jump to latest keeps the reader at the newest turn", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  const originalListAgents = api.listAgents;
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  const layout = installScrollLayoutMocks();
  const queryClient = newQueryClient();
  let unmount: (() => void) | undefined;

  try {
    api.getIssueEvents = async () => [message(1)];
    api.listAgents = async () => [];
    unmount = render(tab({ "CORE-1": issueState() }, true, queryClient)).unmount;

    await screen.findByText("A message");
    await waitFor(() => expect(window.scrollY).toBe(RESTING_SCROLL_Y));

    Object.defineProperty(window, "scrollY", { configurable: true, value: 4_500, writable: true });
    act(() => {
      window.dispatchEvent(new Event("scroll"));
    });
    await screen.findByTestId("jump-to-latest");
    fireEvent.click(screen.getByTestId("jump-to-latest"));

    await waitFor(() => expect(window.scrollY).toBe(RESTING_SCROLL_Y));
  } finally {
    unmount?.();
    api.getIssueEvents = originalGetIssueEvents;
    api.listAgents = originalListAgents;
    layout.restore();
    if (scrollY === undefined) {
      Reflect.deleteProperty(window, "scrollY");
    } else {
      Object.defineProperty(window, "scrollY", scrollY);
    }
  }
});

test("a refetch that predates streamed turns neither removes them nor loses their count", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  const originalListAgents = api.listAgents;
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  const layout = installScrollLayoutMocks();
  const queryClient = newQueryClient();
  let unmount: (() => void) | undefined;
  // Every fetch after the first is answered by the test, so a response can land late.
  const pendingFetches: ((events: Event[]) => void)[] = [];
  let fetches = 0;

  try {
    api.getIssueEvents = () => {
      fetches += 1;
      if (fetches === 1) {
        return Promise.resolve([message(1)]);
      }
      const { promise, resolve } = Promise.withResolvers<Event[]>();
      pendingFetches.push(resolve);
      return promise;
    };
    api.listAgents = async () => [];
    unmount = render(tab({ "CORE-1": issueState() }, true, queryClient)).unmount;
    await screen.findByText("A message");
    await waitFor(() => expect(window.scrollY).toBe(RESTING_SCROLL_Y));

    // The reader scrolls into history.
    Object.defineProperty(window, "scrollY", { configurable: true, value: 4_500, writable: true });
    act(() => {
      window.dispatchEvent(new Event("scroll"));
    });
    await screen.findByTestId("jump-to-latest");

    // The stream delivers a turn and the burst invalidation refetches the log; the server read
    // behind that refetch happened before the turn was committed.
    act(() => {
      prependEventToLog(queryClient, message(2, "Tail arrives"));
    });
    await screen.findByText("Tail arrives");
    expect(screen.getByTestId("jump-to-latest").textContent).toBe("Jump to latest · 1 new");
    await act(async () => {
      await queryClient.invalidateQueries({ queryKey: ["events", "CORE-1"], refetchType: "none" });
      void queryClient.refetchQueries({ queryKey: ["events", "CORE-1"] });
    });
    await waitFor(() => expect(pendingFetches).toHaveLength(1));
    await act(async () => {
      pendingFetches[0]?.([message(1)]);
      await Promise.resolve();
    });
    expect(screen.getByText("Tail arrives")).toBeTruthy();
    expect(screen.getByTestId("jump-to-latest").textContent).toBe("Jump to latest · 1 new");

    // The next turn streams in, and the refetch its burst started returns the whole log.
    act(() => {
      prependEventToLog(queryClient, message(3, "Another tail arrives"));
    });
    await screen.findByText("Another tail arrives");
    expect(screen.getByTestId("jump-to-latest").textContent).toBe("Jump to latest · 2 new");
    await act(async () => {
      void queryClient.refetchQueries({ queryKey: ["events", "CORE-1"] });
    });
    await waitFor(() => expect(pendingFetches).toHaveLength(2));
    await act(async () => {
      pendingFetches[1]?.([
        message(3, "Another tail arrives"),
        message(2, "Tail arrives"),
        message(1),
      ]);
      await Promise.resolve();
    });
    expect(
      Array.from(document.querySelectorAll("[data-turn]")).map((turn) =>
        turn.getAttribute("data-turn")
      )
    ).toEqual(["message:3", "message:2", "message:1"]);
    expect(screen.getByTestId("jump-to-latest").textContent).toBe("Jump to latest · 2 new");
  } finally {
    unmount?.();
    api.getIssueEvents = originalGetIssueEvents;
    api.listAgents = originalListAgents;
    layout.restore();
    if (scrollY === undefined) {
      Reflect.deleteProperty(window, "scrollY");
    } else {
      Object.defineProperty(window, "scrollY", scrollY);
    }
  }
});

test("the unread count follows the log when a refetch fills in a turn below the newest", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  const originalListAgents = api.listAgents;
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  const layout = installScrollLayoutMocks();
  const queryClient = newQueryClient();
  let unmount: (() => void) | undefined;

  try {
    api.getIssueEvents = async () => [message(1)];
    api.listAgents = async () => [];
    unmount = render(tab({ "CORE-1": issueState() }, true, queryClient)).unmount;
    await screen.findByText("A message");
    await waitFor(() => expect(window.scrollY).toBe(RESTING_SCROLL_Y));
    Object.defineProperty(window, "scrollY", { configurable: true, value: 4_500, writable: true });
    act(() => {
      window.dispatchEvent(new Event("scroll"));
    });
    await screen.findByTestId("jump-to-latest");

    // The stream skipped turn 2 (an unparseable frame is skipped, not replayed) and delivered 3.
    act(() => {
      prependEventToLog(queryClient, message(3, "Another tail arrives"));
    });
    await screen.findByText("Another tail arrives");
    expect(screen.getByTestId("jump-to-latest").textContent).toBe("Jump to latest · 1 new");

    // The refetch brings turn 2 in beneath the newest one.
    act(() => {
      queryClient.setQueryData(["events", "CORE-1"], {
        pageParams: [null],
        pages: [[message(3, "Another tail arrives"), message(2, "Tail arrives"), message(1)]],
      });
    });
    await screen.findByText("Tail arrives");
    expect(screen.getByTestId("jump-to-latest").textContent).toBe("Jump to latest · 2 new");
  } finally {
    unmount?.();
    api.getIssueEvents = originalGetIssueEvents;
    api.listAgents = originalListAgents;
    layout.restore();
    if (scrollY === undefined) {
      Reflect.deleteProperty(window, "scrollY");
    } else {
      Object.defineProperty(window, "scrollY", scrollY);
    }
  }
});

test("a full page padded by streamed turns still offers Load older", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  const originalListAgents = api.listAgents;
  const queryClient = newQueryClient();
  let unmount: (() => void) | undefined;

  try {
    // The server's page is exactly 200; the stream then puts one more turn above it.
    const fullPage = Array.from({ length: 200 }, (_, index) => message(200 - index));
    api.getIssueEvents = async () => fullPage;
    api.listAgents = async () => [];
    unmount = render(tab({ "CORE-1": issueState() }, true, queryClient)).unmount;
    await screen.findByRole("button", { name: "Load older" });

    act(() => {
      prependEventToLog(queryClient, message(201, "Streamed above a full page"));
    });
    await screen.findByText("Streamed above a full page");
    expect(screen.getByRole("button", { name: "Load older" })).toBeTruthy();
  } finally {
    unmount?.();
    api.getIssueEvents = originalGetIssueEvents;
    api.listAgents = originalListAgents;
  }
});
test("a comment-delivery retry disables when the target no longer advertises the failed mode", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  const originalListAgents = api.listAgents;
  const queryClient = newQueryClient();
  let unmount: (() => void) | undefined;

  try {
    const comment: Event = {
      actor: { id: "alice", kind: "user" },
      created_at: "2026-09-14T00:00:00Z",
      id: 1,
      issue_key: "CORE-1",
      notify: false,
      payload: {
        anchor: null,
        ask_id: null,
        author: { id: "alice", kind: "user" },
        body: "@Worker take a look",
        created_at: "2026-09-14T00:00:00Z",
        deliveries: [
          {
            attempt: 1,
            comment_id: "comment-1",
            created_at: recentAttemptAt,
            delivery: "steer",
            envelope_id: null,
            error: "no live session worker",
            reply_id: null,
            resolve_error: null,
            session_id: "worker-session",
            state: "failed",
            target: "session:worker-session",
          },
        ],
        edited_at: null,
        id: "comment-1",
        issue_key: "CORE-1",
        mentions: [
          { delivery: "steer", session_id: "worker-session", target: "session:worker-session" },
        ],
        reply_to: null,
        resolved: false,
        resolved_at: null,
        resolved_by: null,
        suggestion: null,
        turn: null,
      },
      seq: 1,
      type: "comment.created",
    } as Event;
    api.getIssueEvents = async () => [comment];
    api.listAgents = async () => [
      {
        capabilities: [],
        dir: "/w",
        last_seen: 1,
        last_activity: null,
        open_asks: 0,
        machine_id: "m",
        roles: [],
        session_id: "worker-session",
        title: "Worker",
      },
    ];

    unmount = render(tab({ "CORE-1": issueState() }, true, queryClient)).unmount;
    await screen.findByText(/no live session worker/);
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
    expect(screen.getByText(/session:worker-session no longer supports Send\./)).toBeTruthy();
  } finally {
    unmount?.();
    api.getIssueEvents = originalGetIssueEvents;
    api.listAgents = originalListAgents;
  }
});

test("a comment-delivery retry to a role target checks the role's current live holder, not the old one", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  const originalListAgents = api.listAgents;
  const queryClient = newQueryClient();
  let unmount: (() => void) | undefined;

  try {
    const comment: Event = {
      actor: { id: "alice", kind: "user" },
      created_at: "2026-09-14T00:00:00Z",
      id: 1,
      issue_key: "CORE-1",
      notify: false,
      payload: {
        anchor: null,
        ask_id: null,
        author: { id: "alice", kind: "user" },
        body: "@reviewer take a look",
        created_at: "2026-09-14T00:00:00Z",
        deliveries: [
          {
            attempt: 1,
            comment_id: "comment-1",
            created_at: recentAttemptAt,
            delivery: "steer",
            envelope_id: null,
            error: "no live session reviewer",
            reply_id: null,
            resolve_error: null,
            session_id: "old-reviewer-session",
            state: "failed",
            target: "role:reviewer",
          },
        ],
        edited_at: null,
        id: "comment-1",
        issue_key: "CORE-1",
        mentions: [{ delivery: "steer", session_id: null, target: "role:reviewer" }],
        reply_to: null,
        resolved: false,
        resolved_at: null,
        resolved_by: null,
        suggestion: null,
        turn: null,
      },
      seq: 1,
      type: "comment.created",
    } as Event;
    api.getIssueEvents = async () => [comment];
    // The role's live holder changed since the failed attempt and does not advertise steer,
    // even though the delivery's own recorded session_id (the old holder) is a stale reference.
    api.listAgents = async () => [
      {
        capabilities: [],
        dir: "/w",
        last_seen: 1,
        last_activity: null,
        open_asks: 0,
        machine_id: "m",
        roles: ["reviewer"],
        session_id: "new-reviewer-session",
        title: "New reviewer",
      },
    ];

    unmount = render(tab({ "CORE-1": issueState() }, true, queryClient)).unmount;
    await screen.findByText(/no live session reviewer/);
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
    expect(screen.getByText(/role:reviewer no longer supports Send\./)).toBeTruthy();
  } finally {
    unmount?.();
    api.getIssueEvents = originalGetIssueEvents;
    api.listAgents = originalListAgents;
  }
});

test("a comment-delivery retry guards a same-tick double click to exactly one attempt", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  const originalListAgents = api.listAgents;
  const originalCreateCommentDelivery = api.createCommentDelivery;
  const queryClient = newQueryClient();
  let unmount: (() => void) | undefined;
  const comment: Event = {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-09-12T00:00:00Z",
    id: 1,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      anchor: null,
      artifact_name: "",
      ask_id: null,
      author: { id: "alice", kind: "user" },
      body: "@Worker take a look",
      created_at: "2026-09-12T00:00:00Z",
      deliveries: [
        {
          attempt: 1,
          comment_id: "comment-1",
          created_at: recentAttemptAt,
          delivery: "steer",
          envelope_id: null,
          error: "no live session worker",
          reply_id: null,
          resolve_error: null,
          session_id: "worker-session",
          state: "failed",
          target: "session:worker-session",
        },
      ],
      edited_at: null,
      id: "comment-1",
      issue_key: "CORE-1",
      mentions: [],
      reply_to: null,
      resolved: false,
      resolved_at: null,
      resolved_by: null,
      suggestion: null,
      turn: null,
    },
    seq: 1,
    type: "comment.created",
  };

  try {
    api.getIssueEvents = async () => [comment];
    api.listAgents = async () => [
      {
        capabilities: ["steer"],
        dir: "/w",
        last_seen: 1,
        last_activity: null,
        open_asks: 0,
        machine_id: "m",
        roles: [],
        session_id: "worker-session",
        title: "Worker",
      },
    ];
    let sends = 0;
    const { promise, resolve } = Promise.withResolvers<void>();
    api.createCommentDelivery = async (id, target, delivery) => {
      sends += 1;
      await promise;
      return {
        attempt: 2,
        comment_id: id,
        created_at: recentAttemptAt,
        delivery,
        envelope_id: null,
        error: null,
        reply_id: null,
        resolve_error: null,
        session_id: "worker-session",
        state: "sent",
        target: target ?? "session:worker-session",
      };
    };
    unmount = render(tab({ "CORE-1": issueState() }, true, queryClient)).unmount;
    const retry = await screen.findByRole("button", { name: "Retry" });
    // Both clicks fire in the same task, before any re-render could disable the button - the
    // window a disabled-prop check (which lags on React state) cannot catch.
    fireEvent.click(retry);
    fireEvent.click(retry);
    resolve();
    await waitFor(() => expect(sends).toBe(1));
  } finally {
    unmount?.();
    api.getIssueEvents = originalGetIssueEvents;
    api.listAgents = originalListAgents;
    api.createCommentDelivery = originalCreateCommentDelivery;
  }
});

test("a root targeted-message retry checks the role's current holder after a handoff, not the failed attempt's session", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  const originalListAgents = api.listAgents;
  const queryClient = newQueryClient();
  let unmount: (() => void) | undefined;

  try {
    const question: Event = {
      actor: { id: "alice", kind: "user" },
      created_at: "2026-09-12T00:00:00Z",
      id: 1,
      issue_key: "CORE-1",
      notify: false,
      payload: {
        author: { id: "alice", kind: "user" },
        body: "Can this ship?",
        created_at: "2026-09-12T00:00:00Z",
        deliveries: [],
        id: "message-1",
        in_reply_to: null,
        issue_key: "CORE-1",
        target: "role:reviewer",
      },
      seq: 1,
      type: "message.created",
    };
    // The failed attempt was recorded against the role's OLD holder.
    const failedDelivery: Event = {
      actor: { id: "alice", kind: "user" },
      created_at: recentAttemptAt,
      id: 2,
      issue_key: "CORE-1",
      notify: false,
      payload: {
        attempt: 1,
        delivery: "steer",
        error: "no live session old-reviewer-session",
        message_id: "message-1",
        session_id: "old-reviewer-session",
        state: "failed",
        title: "Old reviewer",
      },
      seq: 2,
      type: "message.delivery",
    };
    api.getIssueEvents = async () => [question, failedDelivery];
    // The role has since handed off; the live holder does not advertise steer.
    api.listAgents = async () => [
      {
        capabilities: ["btw"],
        dir: "/w",
        last_seen: 1,
        last_activity: null,
        open_asks: 0,
        machine_id: "m",
        roles: ["reviewer"],
        session_id: "new-reviewer-session",
        title: "New reviewer",
      },
    ];

    unmount = render(tab({ "CORE-1": issueState() }, true, queryClient)).unmount;
    // The new holder dropped steer, so the same-mode Retry of a steer attempt is refused.
    const sendNormally = await screen.findByRole("button", { name: "Retry" });
    expect(sendNormally.hasAttribute("disabled")).toBe(true);
    expect(screen.getByRole("button", { name: "Use BTW instead" }).hasAttribute("disabled")).toBe(
      false
    );
  } finally {
    unmount?.();
    api.getIssueEvents = originalGetIssueEvents;
    api.listAgents = originalListAgents;
  }
});

test("a reply's targeted-message retry checks the thread's current role holder after a handoff", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  const originalListAgents = api.listAgents;
  const queryClient = newQueryClient();
  let unmount: (() => void) | undefined;

  try {
    const question: Event = {
      actor: { id: "alice", kind: "user" },
      created_at: "2026-09-12T00:00:00Z",
      id: 1,
      issue_key: "CORE-1",
      notify: false,
      payload: {
        author: { id: "alice", kind: "user" },
        body: "Can this ship?",
        created_at: "2026-09-12T00:00:00Z",
        deliveries: [],
        id: "message-1",
        in_reply_to: null,
        issue_key: "CORE-1",
        target: "role:reviewer",
      },
      seq: 1,
      type: "message.created",
    };
    const sent: Event = {
      actor: { id: "alice", kind: "user" },
      created_at: recentAttemptAt,
      id: 2,
      issue_key: "CORE-1",
      notify: false,
      payload: {
        attempt: 1,
        delivery: "steer",
        message_id: "message-1",
        session_id: "old-reviewer-session",
        state: "sent",
        title: "Old reviewer",
      },
      seq: 2,
      type: "message.delivery",
    };
    // A session answer marks the root "answered," suppressing its own retry row so only the
    // reply's retry row is under test below.
    const sessionAnswer: Event = {
      actor: { id: "old-reviewer-session", kind: "session" },
      created_at: "2026-09-12T00:00:45Z",
      id: 5,
      issue_key: "CORE-1",
      notify: false,
      payload: {
        author: { id: "old-reviewer-session", kind: "session" },
        body: "Looking into it.",
        created_at: "2026-09-12T00:00:45Z",
        deliveries: [],
        id: "message-3",
        in_reply_to: "message-1",
        issue_key: "CORE-1",
        target: null,
      },
      seq: 5,
      type: "message.created",
    };
    const reply: Event = {
      actor: { id: "alice", kind: "user" },
      created_at: "2026-09-12T00:01:00Z",
      id: 3,
      issue_key: "CORE-1",
      notify: false,
      payload: {
        author: { id: "alice", kind: "user" },
        body: "Any update?",
        created_at: "2026-09-12T00:01:00Z",
        deliveries: [],
        id: "message-2",
        in_reply_to: "message-1",
        issue_key: "CORE-1",
        target: null,
      },
      seq: 3,
      type: "message.created",
    };
    // The reply's own failed delivery is recorded against the role's OLD holder too.
    const failedReplyDelivery: Event = {
      actor: { id: "alice", kind: "user" },
      created_at: recentAttemptAt,
      id: 4,
      issue_key: "CORE-1",
      notify: false,
      payload: {
        attempt: 1,
        delivery: "steer",
        error: "no live session old-reviewer-session",
        message_id: "message-2",
        session_id: "old-reviewer-session",
        state: "failed",
        title: "Old reviewer",
      },
      seq: 4,
      type: "message.delivery",
    };
    api.getIssueEvents = async () => [question, sent, sessionAnswer, reply, failedReplyDelivery];
    // The role has since handed off; the live holder does not advertise steer.
    api.listAgents = async () => [
      {
        capabilities: ["btw"],
        dir: "/w",
        last_seen: 1,
        last_activity: null,
        open_asks: 0,
        machine_id: "m",
        roles: ["reviewer"],
        session_id: "new-reviewer-session",
        title: "New reviewer",
      },
    ];

    unmount = render(tab({ "CORE-1": issueState() }, true, queryClient)).unmount;
    await screen.findByText("Any update?");
    // The new holder dropped steer, so the same-mode Retry of a steer attempt is refused.
    const sendNormally = await screen.findByRole("button", { name: "Retry" });
    expect(sendNormally.hasAttribute("disabled")).toBe(true);
    expect(screen.getByRole("button", { name: "Use BTW instead" }).hasAttribute("disabled")).toBe(
      false
    );
  } finally {
    unmount?.();
    api.getIssueEvents = originalGetIssueEvents;
    api.listAgents = originalListAgents;
  }
});
test("a comment-delivery retry guards a same-tick double click to exactly one attempt", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  const originalListAgents = api.listAgents;
  const originalCreateCommentDelivery = api.createCommentDelivery;
  const queryClient = newQueryClient();
  let unmount: (() => void) | undefined;
  const comment: Event = {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-09-12T00:00:00Z",
    id: 1,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      anchor: null,
      artifact_name: "",
      ask_id: null,
      author: { id: "alice", kind: "user" },
      body: "@Worker take a look",
      created_at: "2026-09-12T00:00:00Z",
      deliveries: [
        {
          attempt: 1,
          comment_id: "comment-1",
          created_at: recentAttemptAt,
          delivery: "steer",
          envelope_id: null,
          error: "no live session worker",
          reply_id: null,
          resolve_error: null,
          session_id: "worker-session",
          state: "failed",
          target: "session:worker-session",
        },
      ],
      edited_at: null,
      id: "comment-1",
      issue_key: "CORE-1",
      mentions: [],
      reply_to: null,
      resolved: false,
      resolved_at: null,
      resolved_by: null,
      suggestion: null,
      turn: null,
    },
    seq: 1,
    type: "comment.created",
  };

  try {
    api.getIssueEvents = async () => [comment];
    api.listAgents = async () => [];
    let sends = 0;
    const { promise, resolve } = Promise.withResolvers<void>();
    api.createCommentDelivery = async (id, target, delivery) => {
      sends += 1;
      await promise;
      return {
        attempt: 2,
        comment_id: id,
        created_at: recentAttemptAt,
        delivery,
        envelope_id: null,
        error: null,
        reply_id: null,
        resolve_error: null,
        session_id: "worker-session",
        state: "sent",
        target: target ?? "session:worker-session",
      };
    };
    unmount = render(tab({ "CORE-1": issueState() }, true, queryClient)).unmount;
    const retry = await screen.findByRole("button", { name: "Retry" });
    // Both clicks fire in the same task, before any re-render could disable the button - the
    // window a disabled-prop check (which lags on React state) cannot catch.
    fireEvent.click(retry);
    fireEvent.click(retry);
    resolve();
    await waitFor(() => expect(sends).toBe(1));
  } finally {
    unmount?.();
    api.getIssueEvents = originalGetIssueEvents;
    api.listAgents = originalListAgents;
    api.createCommentDelivery = originalCreateCommentDelivery;
  }
});

function commentEvent(
  sequence: number,
  id: string,
  body: string,
  replyTo: string | null = null
): Extract<Event, { type: "comment.created" }> {
  return {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-09-20T00:00:00Z",
    id: sequence,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      anchor: null,
      artifact_name: "spec",
      ask_id: null,
      author: { id: "alice", kind: "user" },
      body,
      created_at: "2026-09-20T00:00:00Z",
      deliveries: [],
      edited_at: null,
      id,
      issue_key: "CORE-1",
      mentions: [],
      reply_to: replyTo,
      resolved: false,
      resolved_at: null,
      resolved_by: null,
      suggestion: null,
      turn: null,
    },
    seq: sequence,
    type: "comment.created",
  };
}

test("Conversation hides reply editing while its owner saves a comment", async () => {
  const root = commentEvent(1, "root-comment", "Root comment");
  const reply = commentEvent(2, "reply-comment", "Reply comment", root.payload.id);
  const save = Promise.withResolvers<Comment>();
  const editComment = spyOn(api, "editComment").mockImplementation(() => save.promise);
  const originalGetIssueEvents = api.getIssueEvents;
  const originalListAgents = api.listAgents;
  const queryClient = newQueryClient();
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  let unmount: (() => void) | undefined;

  try {
    api.getIssueEvents = async () => [root, reply];
    api.listAgents = async () => [];
    unmount = render(tab({ "CORE-1": issueState() }, true, queryClient)).unmount;
    const card = await screen.findByTestId(`margin-comment-${root.payload.id}`);
    fireEvent.click(screen.getByRole("button", { name: "Expand thread" }));
    await waitFor(() =>
      expect(within(card).getAllByRole("button", { name: "Edit" })).toHaveLength(2)
    );
    fireEvent.click(within(card).getAllByRole("button", { name: "Edit" })[0] as HTMLElement);
    fireEvent.change(screen.getByLabelText("Edit comment"), { target: { value: "Updated root" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() =>
      expect(editComment).toHaveBeenCalledWith(root.payload.id, { body: "Updated root" })
    );
    expect(within(card).queryAllByRole("button", { name: "Edit" })).toHaveLength(0);
    expect((screen.getByRole("textbox", { name: "Reply" }) as HTMLTextAreaElement).disabled).toBe(
      false
    );

    await act(async () => {
      save.resolve({ ...root.payload, deliveries: [], mentions: [] });
      await save.promise;
    });
    await waitFor(() =>
      expect(within(card).getAllByRole("button", { name: "Edit" })).toHaveLength(2)
    );
  } finally {
    save.resolve({ ...root.payload, deliveries: [], mentions: [] });
    unmount?.();
    editComment.mockRestore();
    api.getIssueEvents = originalGetIssueEvents;
    api.listAgents = originalListAgents;
  }
});

test("Conversation restores edit controls and the draft when its owner save rejects", async () => {
  const root = commentEvent(1, "root-comment", "Root comment");
  const reply = commentEvent(2, "reply-comment", "Reply comment", root.payload.id);
  const save = Promise.withResolvers<Comment>();
  const editComment = spyOn(api, "editComment").mockImplementation(() => save.promise);
  const originalGetIssueEvents = api.getIssueEvents;
  const originalListAgents = api.listAgents;
  const queryClient = newQueryClient();
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  let unmount: (() => void) | undefined;

  try {
    api.getIssueEvents = async () => [root, reply];
    api.listAgents = async () => [];
    unmount = render(tab({ "CORE-1": issueState() }, true, queryClient)).unmount;
    const card = await screen.findByTestId(`margin-comment-${root.payload.id}`);
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
      expect(editComment).toHaveBeenCalledWith(root.payload.id, { body: "Draft survives" })
    );
    expect(within(card).queryAllByRole("button", { name: "Edit" })).toHaveLength(0);
    expect((screen.getByRole("textbox", { name: "Reply" }) as HTMLTextAreaElement).disabled).toBe(
      false
    );

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
    save.resolve({ ...root.payload, deliveries: [], mentions: [] });
    unmount?.();
    editComment.mockRestore();
    api.getIssueEvents = originalGetIssueEvents;
    api.listAgents = originalListAgents;
  }
});

test("a comment on the primary document links to the Spec route, not its artifact route", async () => {
  // The link is built by `documentItemPath`, the one builder that knows a primary document is
  // the issue's Spec tab; the hand-assembled href it replaced always named the artifact route.
  const originalGetIssueEvents = api.getIssueEvents;
  const originalListAgents = api.listAgents;
  const queryClient = newQueryClient();
  let unmount: (() => void) | undefined;
  const comment: Event = {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-09-12T00:00:00Z",
    id: 1,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      anchor: {
        artifact_id: "artifact-primary",
        block_id: null,
        mark_id: "mark-1",
        orphaned: false,
        quote: "Anchored source",
        version: 1,
      },
      artifact_name: "spec.md",
      ask_id: null,
      author: { id: "alice", kind: "user" },
      body: "Please revise this.",
      created_at: "2026-09-12T00:00:00Z",
      deliveries: [],
      edited_at: null,
      id: "comment-1",
      issue_key: "CORE-1",
      mentions: [],
      reply_to: null,
      resolved: false,
      resolved_at: null,
      resolved_by: null,
      suggestion: null,
      turn: null,
    },
    seq: 1,
    type: "comment.created",
  };

  try {
    api.getIssueEvents = async () => [comment];
    api.listAgents = async () => [];
    queryClient.setQueryData(["whoami"], { login: "alice" });
    unmount = render(
      tab(
        { "CORE-1": issueState() },
        true,
        queryClient,
        false,
        new Map([["artifact-primary", primaryDocument]])
      )
    ).unmount;
    await screen.findByText("Please revise this.");

    expect(screen.getByRole("link", { name: "View in document" }).getAttribute("href")).toBe(
      "/issues/CORE-1/spec?comment=comment-1"
    );
  } finally {
    unmount?.();
    api.getIssueEvents = originalGetIssueEvents;
    api.listAgents = originalListAgents;
  }
});
