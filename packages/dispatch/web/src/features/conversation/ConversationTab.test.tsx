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
      expect(writes.at(-1)).toEqual(["pinned_items:event:1", "pinned_items:event:2"])
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

    expect(rectSpy.mock.calls.length).toBeLessThanOrEqual(3);
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

test("shows Jump to latest until the reader returns to the bottom", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  const originalListAgents = api.listAgents;
  const innerHeight = Object.getOwnPropertyDescriptor(window, "innerHeight");
  const scrollHeight = Object.getOwnPropertyDescriptor(document.documentElement, "scrollHeight");
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  const originalScrollTo = window.scrollTo;
  const queryClient = newQueryClient();
  let unmount: (() => void) | undefined;

  try {
    Object.defineProperty(window, "innerHeight", { configurable: true, value: 500 });
    Object.defineProperty(document.documentElement, "scrollHeight", {
      configurable: true,
      value: 5_000,
    });
    Object.defineProperty(window, "scrollY", { configurable: true, value: 0, writable: true });
    window.scrollTo = () => {};
    api.getIssueEvents = async () => [message(1)];
    api.listAgents = async () => [];
    unmount = render(tab({ "CORE-1": issueState() }, true, queryClient)).unmount;

    await screen.findByText("A message");
    expect(screen.getByTestId("jump-to-latest")).toBeTruthy();

    Object.defineProperty(window, "scrollY", { configurable: true, value: 4_500, writable: true });
    act(() => {
      window.dispatchEvent(new Event("scroll"));
    });

    await waitFor(() => expect(screen.queryByTestId("jump-to-latest")).toBeNull());
  } finally {
    unmount?.();
    api.getIssueEvents = originalGetIssueEvents;
    api.listAgents = originalListAgents;
    window.scrollTo = originalScrollTo;
    if (innerHeight === undefined) {
      Reflect.deleteProperty(window, "innerHeight");
    } else {
      Object.defineProperty(window, "innerHeight", innerHeight);
    }
    if (scrollHeight === undefined) {
      Reflect.deleteProperty(document.documentElement, "scrollHeight");
    } else {
      Object.defineProperty(document.documentElement, "scrollHeight", scrollHeight);
    }
    if (scrollY === undefined) {
      Reflect.deleteProperty(window, "scrollY");
    } else {
      Object.defineProperty(window, "scrollY", scrollY);
    }
  }
});

test("follows an incoming turn when the reader was pinned to the bottom", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  const originalListAgents = api.listAgents;
  const innerHeight = Object.getOwnPropertyDescriptor(window, "innerHeight");
  const scrollHeight = Object.getOwnPropertyDescriptor(document.documentElement, "scrollHeight");
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  const originalScrollTo = window.scrollTo;
  const queryClient = newQueryClient();
  let unmount: (() => void) | undefined;

  try {
    Object.defineProperty(window, "innerHeight", { configurable: true, value: 500 });
    Object.defineProperty(document.documentElement, "scrollHeight", {
      configurable: true,
      value: 500,
    });
    Object.defineProperty(window, "scrollY", { configurable: true, value: 0, writable: true });
    window.scrollTo = (options?: ScrollToOptions | number, y?: number) => {
      const top = typeof options === "number" ? y : options?.top;
      if (top !== undefined) {
        Object.defineProperty(window, "scrollY", {
          configurable: true,
          value: top,
          writable: true,
        });
      }
    };
    api.getIssueEvents = async () => [message(1)];
    api.listAgents = async () => [];
    unmount = render(tab({ "CORE-1": issueState() }, true, queryClient)).unmount;

    await screen.findByText("A message");
    Object.defineProperty(window, "scrollY", { configurable: true, value: 0, writable: true });
    Object.defineProperty(document.documentElement, "scrollHeight", {
      configurable: true,
      value: 5_000,
    });
    act(() => {
      queryClient.setQueryData(["events", "CORE-1"], {
        pageParams: [null],
        pages: [[message(1), message(2, "Incoming message")]],
      });
    });

    await screen.findByText("Incoming message");
    await waitFor(() => expect(window.scrollY).toBe(5_000));
  } finally {
    unmount?.();
    api.getIssueEvents = originalGetIssueEvents;
    api.listAgents = originalListAgents;
    window.scrollTo = originalScrollTo;
    if (innerHeight === undefined) {
      Reflect.deleteProperty(window, "innerHeight");
    } else {
      Object.defineProperty(window, "innerHeight", innerHeight);
    }
    if (scrollHeight === undefined) {
      Reflect.deleteProperty(document.documentElement, "scrollHeight");
    } else {
      Object.defineProperty(document.documentElement, "scrollHeight", scrollHeight);
    }
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
  const innerHeight = Object.getOwnPropertyDescriptor(window, "innerHeight");
  const scrollHeight = Object.getOwnPropertyDescriptor(document.documentElement, "scrollHeight");
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  const originalScrollBy = window.scrollBy;
  const originalScrollTo = window.scrollTo;
  const rectSpy = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(() => ({
    bottom: 500 - window.scrollY,
    height: 500,
    left: 0,
    right: 100,
    toJSON: () => ({}),
    top: -window.scrollY,
    width: 100,
    x: 0,
    y: -window.scrollY,
  }));
  const elementFromPointSpy = spyOn(document, "elementFromPoint").mockImplementation(() =>
    document.querySelector("[data-event-seq]")
  );
  const queryClient = newQueryClient();
  let allowScroll = false;
  let unmount: (() => void) | undefined;

  try {
    Object.defineProperty(window, "innerHeight", { configurable: true, value: 500 });
    Object.defineProperty(document.documentElement, "scrollHeight", {
      configurable: true,
      value: 5_000,
    });
    Object.defineProperty(window, "scrollY", { configurable: true, value: 0, writable: true });
    window.scrollTo = (options?: ScrollToOptions | number, y?: number) => {
      const top = typeof options === "number" ? y : options?.top;
      if (allowScroll && top !== undefined) {
        Object.defineProperty(window, "scrollY", {
          configurable: true,
          value: top,
          writable: true,
        });
      }
    };
    window.scrollBy = (options?: ScrollToOptions | number, y?: number) => {
      const top = typeof options === "number" ? y : options?.top;
      if (top !== undefined) {
        Object.defineProperty(window, "scrollY", {
          configurable: true,
          value: window.scrollY + top,
          writable: true,
        });
      }
    };
    api.getIssueEvents = async () => [message(1)];
    api.listAgents = async () => [];
    unmount = render(tab({ "CORE-1": issueState() }, true, queryClient)).unmount;

    await screen.findByTestId("jump-to-latest");
    allowScroll = true;
    fireEvent.click(screen.getByTestId("jump-to-latest"));

    await waitFor(() => expect(window.scrollY).toBe(5_000));
  } finally {
    unmount?.();
    api.getIssueEvents = originalGetIssueEvents;
    api.listAgents = originalListAgents;
    window.scrollBy = originalScrollBy;
    window.scrollTo = originalScrollTo;
    rectSpy.mockRestore();
    elementFromPointSpy.mockRestore();
    if (innerHeight === undefined) {
      Reflect.deleteProperty(window, "innerHeight");
    } else {
      Object.defineProperty(window, "innerHeight", innerHeight);
    }
    if (scrollHeight === undefined) {
      Reflect.deleteProperty(document.documentElement, "scrollHeight");
    } else {
      Object.defineProperty(document.documentElement, "scrollHeight", scrollHeight);
    }
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
