import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Actor, Event, UserIssueState, UserState } from "../../api/types";
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
      id: `message-${id}`,
      issue_key: "CORE-1",
      reply_to: null,
    },
    seq: id,
    type: "message.created",
  };
}

function issueState(dismissed: string[] = [], lastReadSeq = 0): UserIssueState {
  return { dismissed, last_read_seq: lastReadSeq, pinned: false };
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
  isClosed = false
): ReactNode {
  return (
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <ConversationTab isClosed={isClosed} issueKey="CORE-1" state={state} visible={visible} />
      </QueryClientProvider>
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
    const isComposer = this.getAttribute("aria-label") === "Message composer";
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
    const view = render(tab({ "CORE-1": issueState() }, false, queryClient));
    unmount = view.unmount;

    await screen.findByText("A message");
    await new Promise<void>((resolve) => window.setTimeout(resolve, 0));
    expect(observations).toHaveLength(0);

    view.rerender(tab({ "CORE-1": issueState() }, true, queryClient));
    await waitFor(() => expect(observations).toHaveLength(1));
    expect(observations[0]?.getAttribute("data-event-seq")).toBe("1");
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
    for (const button of screen.getAllByRole("button", { name: "Pin" })) {
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
    queryClient.setQueryData<UserState>(["user-state"], { "CORE-1": issueState() });
    queryClient.setQueryData(["events", "CORE-1"], { pageParams: [null], pages: [events] });

    unmount = render(tab({ "CORE-1": issueState() }, true, queryClient)).unmount;
    await screen.findByText("Message 0");

    rectSpy.mockClear();
    window.dispatchEvent(new Event("scroll"));

    // ReaderPosition snapshots only immediately before a content reflow; a scroll alone must do
    // no layout reads. This guards both against O(n) scans over every loaded turn and unnecessary
    // per-scroll layout work.
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

    await screen.findByRole("form", { name: "Message composer" });
    view.rerender(tab({ "CORE-1": issueState() }, true, queryClient, true));

    expect(screen.queryByRole("form", { name: "Message composer" })).toBeNull();
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

test("lists live sessions in the message recipient selector", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  const originalListAgents = api.listAgents;
  const queryClient = newQueryClient();
  let unmount: (() => void) | undefined;

  try {
    api.getIssueEvents = async () => [message(1)];
    api.listAgents = async () => [
      {
        capabilities: [],
        dir: "/w/planner",
        last_seen: 2,
        machine_id: "machine-a",
        roles: [],
        session_id: "planner-session",
        title: "Planner (e2e)",
      },
      {
        capabilities: [],
        dir: "/w/reviewer",
        last_seen: 1,
        machine_id: "machine-b",
        roles: [],
        session_id: "reviewer-session",
        title: "Reviewer (e2e)",
      },
    ];
    unmount = render(tab({ "CORE-1": issueState() }, true, queryClient)).unmount;

    const selector = (await screen.findByRole("combobox", {
      name: "Recipient",
    })) as HTMLSelectElement;
    expect(selector.value).toBe("");
    expect(screen.getByRole("option", { name: "No recipient" })).toBeTruthy();
    const planner = (await screen.findByRole("option", {
      name: "Planner (e2e)",
    })) as HTMLOptionElement;
    expect(planner.value).toBe("planner-session");
    const reviewer = screen.getByRole("option", { name: "Reviewer (e2e)" }) as HTMLOptionElement;
    expect(reviewer.value).toBe("reviewer-session");
  } finally {
    unmount?.();
    api.getIssueEvents = originalGetIssueEvents;
    api.listAgents = originalListAgents;
  }
});
