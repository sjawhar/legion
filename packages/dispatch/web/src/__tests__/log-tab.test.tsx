import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";

import { api } from "../api/client";
import type { Event, UserIssueState, UserState } from "../api/types";
import { LogTab } from "../features/issue/LogTab";

function event(body = "A message"): Event {
  return {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-09-09T00:00:00Z",
    id: 1,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      author: { id: "alice", kind: "user" },
      body,
      created_at: "2026-09-09T00:00:00Z",
      id: "message-1",
      issue_key: "CORE-1",
    },
    seq: 1,
    type: "message.created",
  };
}

function askEvent(type: "ask.opened" | "ask.answered", question: string): Event {
  return {
    ...event(),
    payload: {
      anchor: null,
      answer: null,
      author: { id: "alice", kind: "user" },
      created_at: "2026-09-09T00:00:00Z",
      id: "ask-1",
      issue_key: "CORE-1",
      multiple: false,
      options: [],
      question,
      state: type === "ask.opened" ? "open" : "answered",
      urgency: "med",
    },
    type,
  };
}

function renderLog(events: Event[]) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  api.getIssueEvents = async () => events;
  return render(
    <QueryClientProvider client={queryClient}>
      <LogTab
        isClosed={false}
        issueKey="CORE-1"
        route={null}
        state={{ "CORE-1": issueState() }}
        visible={false}
      />
    </QueryClientProvider>
  );
}

function issueState(dismissed: string[] = []): UserIssueState {
  return { dismissed, last_read_seq: 1, pinned: false };
}

test("LogTab renders message bodies as Markdown", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  let unmount: (() => void) | undefined;

  try {
    unmount = renderLog([event("line one\n\n- item\n- item")]).unmount;

    const article = (await screen.findByText("line one")).closest("article");
    if (article === null) {
      throw new Error("Markdown message did not render in a log row");
    }
    expect(within(article).getByRole("list")).not.toBeNull();
    expect(
      within(article)
        .getAllByRole("listitem")
        .map((item) => item.textContent)
    ).toEqual(["item", "item"]);
    expect(article.querySelectorAll("p")).toHaveLength(2);
  } finally {
    unmount?.();
    api.getIssueEvents = originalGetIssueEvents;
  }
});

test("LogTab keeps a folded event as a plain-text preview", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  let unmount: (() => void) | undefined;

  try {
    unmount = renderLog([{ ...event(), type: "comment.resolved" } as Event]).unmount;

    const article = (await screen.findByText("Comment resolved")).closest("article");
    if (article === null) {
      throw new Error("Folded event did not render in a log row");
    }
    expect(article.querySelector(".prose")).toBeNull();
    expect(article.querySelector("ul")).toBeNull();
  } finally {
    unmount?.();
    api.getIssueEvents = originalGetIssueEvents;
  }
});

test("LogTab renders an ask label above its Markdown body", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  let unmount: (() => void) | undefined;

  try {
    unmount = renderLog([askEvent("ask.opened", "Review **this**")]).unmount;

    expect(await screen.findByText("Ask opened:")).not.toBeNull();
    expect(screen.getByText("Review")).not.toBeNull();
  } finally {
    unmount?.();
    api.getIssueEvents = originalGetIssueEvents;
  }
});

test("LogTab coalesces an answered ask with its options and timestamps", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  let unmount: (() => void) | undefined;

  try {
    const opened = askEvent("ask.opened", "Choose a path");
    const answerEvent = askEvent("ask.answered", "Choose a path");
    const answered = {
      ...answerEvent,
      id: 2,
      payload: {
        ...answerEvent.payload,
        answer: {
          at: "2026-09-09T00:05:00Z",
          selected: ["Ship"],
          text: "Proceed.",
          user: "alice",
        },
        options: [{ description: "Ship immediately", label: "Ship" }, { label: "Hold" }],
        state: "answered",
      },
      seq: 2,
    } as Event;
    unmount = renderLog([opened, answered]).unmount;

    const article = (await screen.findByText("Ask:")).closest("article");
    if (article === null) {
      throw new Error("Answered ask did not render in a log row");
    }
    expect(document.querySelectorAll("[data-event-seq]")).toHaveLength(1);
    const options = within(article).getByRole("list", { name: "Answer options" });
    expect(options.textContent).toContain("✓");
    expect(options.textContent).toContain("Ship immediately");
    expect(article.textContent).toContain("Opened");
    expect(article.textContent).toContain("Answered by alice");
    expect(article.textContent).toContain("Proceed.");
  } finally {
    unmount?.();
    api.getIssueEvents = originalGetIssueEvents;
  }
});

test("LogTab observes read state only while its panel is visible", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
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
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  let unmount: (() => void) | undefined;

  try {
    api.getIssueEvents = async () => [event()];
    unmount = render(
      <QueryClientProvider client={queryClient}>
        <LogTab
          isClosed={false}
          issueKey="CORE-1"
          route={null}
          state={{ "CORE-1": issueState() }}
          visible={false}
        />
      </QueryClientProvider>
    ).unmount;

    await screen.findByText("A message");
    await new Promise<void>((resolve) => window.setTimeout(resolve, 0));
    expect(observations).toHaveLength(0);
  } finally {
    unmount?.();
    api.getIssueEvents = originalGetIssueEvents;
    globalThis.IntersectionObserver = originalIntersectionObserver;
  }
});

test("retry replays every operation rejected by the state-write queue", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  const originalGetMyState = api.getMyState;
  const originalPutIssueState = api.putIssueState;
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const writes: string[][] = [];
  let failures = 2;

  try {
    api.getIssueEvents = async () => [event()];
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
    queryClient.setQueryData(["events", "CORE-1"], {
      pageParams: [null],
      pages: [[event()]],
    });

    const view = render(
      <QueryClientProvider client={queryClient}>
        <LogTab
          isClosed={false}
          issueKey="CORE-1"
          route={null}
          state={{ "CORE-1": issueState() }}
          visible={true}
        />
      </QueryClientProvider>
    );
    fireEvent.click(screen.getByRole("button", { name: "Pin" }));
    fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));

    await waitFor(() =>
      expect(screen.getByRole("alert").textContent).toContain("Couldn't save pin/dismiss")
    );
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));

    await waitFor(() => expect(writes.at(-1)).toEqual(["pinned_items:event:1", "event:1"]));
    view.unmount();
  } finally {
    api.getIssueEvents = originalGetIssueEvents;
    api.getMyState = originalGetMyState;
    api.putIssueState = originalPutIssueState;
  }
});

test("a scroll event measures the reader's position in O(1) rect reads, not a scan over every article", async () => {
  const originalGetIssueEvents = api.getIssueEvents;
  const originalGetMyState = api.getMyState;
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  // Enough articles that a per-article getBoundingClientRect scan would blow well past any
  // reasonable per-scroll-event budget, while an O(1) hit-test stays flat regardless of count.
  const events: Event[] = Array.from({ length: 250 }, (_, index) => ({
    actor: { id: "alice", kind: "user" },
    created_at: "2026-09-09T00:00:00Z",
    id: index + 1,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      author: { id: "alice", kind: "user" },
      body: `Message ${index}`,
      created_at: "2026-09-09T00:00:00Z",
      id: `message-${index}`,
      issue_key: "CORE-1",
    },
    seq: index + 1,
    type: "message.created",
  }));
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

  try {
    api.getIssueEvents = async () => events;
    api.getMyState = async () => ({ "CORE-1": issueState() });
    queryClient.setQueryData<UserState>(["user-state"], { "CORE-1": issueState() });
    queryClient.setQueryData(["events", "CORE-1"], { pageParams: [null], pages: [events] });

    const view = render(
      <QueryClientProvider client={queryClient}>
        <LogTab
          isClosed={false}
          issueKey="CORE-1"
          route={null}
          state={{ "CORE-1": issueState() }}
          visible={true}
        />
      </QueryClientProvider>
    );
    await waitFor(() => expect(screen.getByText("Message 0")).toBeDefined());

    rectSpy.mockClear();
    window.dispatchEvent(new Event("scroll"));

    expect(rectSpy.mock.calls.length).toBeLessThanOrEqual(3);
    view.unmount();
  } finally {
    api.getIssueEvents = originalGetIssueEvents;
    api.getMyState = originalGetMyState;
    rectSpy.mockRestore();
    elementFromPointSpy.mockRestore();
  }
});
