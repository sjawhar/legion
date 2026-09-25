import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";
import { api } from "../../api/client";
import { prependEventToLog } from "../../api/sse";
import type { Actor, Artifact, Comment, Event, UserIssueState, UserState } from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { ConversationTab } from "./ConversationTab";

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
    <MemoryRouter>
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
      created_at: "2026-09-12T00:01:00Z",
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

    expect(screen.queryByRole("button", { name: "Ask BTW again" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Send normally" })).toBeNull();
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
      created_at: "2026-09-12T00:00:01Z",
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
            created_at: "2026-09-14T00:00:00Z",
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
    expect(
      screen.getByText(/session:worker-session no longer supports\s+normal delivery\./)
    ).toBeTruthy();
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
            created_at: "2026-09-14T00:00:00Z",
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
    expect(screen.getByText(/role:reviewer no longer supports\s+normal delivery\./)).toBeTruthy();
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
          created_at: "2026-09-12T00:00:00Z",
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
        created_at: "2026-09-12T00:01:00Z",
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
      created_at: "2026-09-12T00:01:00Z",
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
    const sendNormally = await screen.findByRole("button", { name: "Send normally" });
    expect(sendNormally.hasAttribute("disabled")).toBe(true);
    expect(screen.getByRole("button", { name: "Ask BTW again" }).hasAttribute("disabled")).toBe(
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
      created_at: "2026-09-12T00:00:30Z",
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
      created_at: "2026-09-12T00:01:30Z",
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
    const sendNormally = await screen.findByRole("button", { name: "Send normally" });
    expect(sendNormally.hasAttribute("disabled")).toBe(true);
    expect(screen.getByRole("button", { name: "Ask BTW again" }).hasAttribute("disabled")).toBe(
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
          created_at: "2026-09-12T00:00:00Z",
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
        created_at: "2026-09-12T00:01:00Z",
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
