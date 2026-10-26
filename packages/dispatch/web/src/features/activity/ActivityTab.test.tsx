import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { ApiError, api, type ListEventsOptions } from "../../api/client";
import { prependEventToLog } from "../../api/event-feed";
import type { Actor, Agent, Event, UserIssueState, UserState } from "../../api/types";
import { writesOf } from "../writes/write-store";
import { ActivityTab } from "./ActivityTab";

function message(seq: number, by?: Actor): Event {
  const actor = by ?? { id: "alice", kind: "user" as const };
  return {
    actor,
    created_at: "2026-09-09T00:00:00Z",
    id: seq,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      author: actor,
      body: `Message ${seq}`,
      created_at: "2026-09-09T00:00:00Z",
      deliveries: [],
      id: `message-${seq}`,
      in_reply_to: null,
      issue_key: "CORE-1",
      target: null,
    },
    seq,
    type: "message.created",
  };
}

function newQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
}

function issueState(lastReadSeq = 0): UserIssueState {
  return { dismissed: [], last_read_seq: lastReadSeq, pinned: false, seq: 0 };
}

function renderTab(state: UserState, queryClient: QueryClient = newQueryClient()) {
  return render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <ActivityTab issueKey="CORE-1" state={state} visible />
      </QueryClientProvider>
    </MemoryRouter>
  );
}

// The Activity tab's list is the issue's whole history and nothing else: the Conversation's
// `ol[aria-label="Conversation turns"]` is a different list on the same page. The watermark it
// draws is read-only here - only the Conversation writes `last_read_seq` - so the tab must not
// put one, however long the reader dwells on it.
test("ActivityTab lists the issue's events under its own label and writes no read mark", async () => {
  const getIssueEvents = spyOn(api, "getIssueEvents").mockResolvedValue([
    message(3),
    message(2),
    message(1),
  ]);
  const putIssueState = spyOn(api, "putIssueState");
  const view = renderTab({
    "CORE-1": { dismissed: [], last_read_seq: 2, pinned: false, seq: 0 },
  });

  try {
    const list = await screen.findByRole("list", { name: "Issue activity" });
    expect(within(list).getAllByText("sent a message")).toHaveLength(3);
    expect(within(list).getByLabelText("New since you last read")).toBeDefined();
    expect(putIssueState).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    getIssueEvents.mockRestore();
    putIssueState.mockRestore();
  }
});

test("ActivityTab's Load older fetches the page before the oldest event it holds", async () => {
  const firstPage = Array.from({ length: 200 }, (_, index) => message(200 - index));
  const calls: ListEventsOptions[] = [];
  const getIssueEvents = spyOn(api, "getIssueEvents").mockImplementation(async (_key, options) => {
    calls.push(options ?? {});
    return options?.before === undefined ? firstPage : [message(0)];
  });
  const view = renderTab({});

  try {
    await screen.findByRole("list", { name: "Issue activity" });
    fireEvent.click(await screen.findByRole("button", { name: "Load older" }));

    await waitFor(() => expect(calls).toHaveLength(2));
    expect(calls[1]).toEqual({ before: 1, limit: 200 });
    await waitFor(() =>
      expect(
        within(screen.getByRole("list", { name: "Issue activity" })).getAllByText("sent a message")
      ).toHaveLength(201)
    );
  } finally {
    view.unmount();
    getIssueEvents.mockRestore();
  }
});

/** One live session, as `GET /api/v1/agents` lists it. */
function running(sessionId: string, title: string): Agent {
  return {
    capabilities: [],
    dir: "/w",
    host: "host",
    last_activity: null,
    last_seen: 1,
    machine_id: "host",
    open_asks: 0,
    roles: [],
    session_id: sessionId,
    title,
  };
}

// Every row names its author, and a session's name is its live registry title. The tab read the
// registry only for a history that held a claim, so a row a session wrote on an issue nobody had
// claimed read as the raw `session:<id>` - the registry was never fetched to resolve it.
test("ActivityTab names a session author by its registered title", async () => {
  const getIssueEvents = spyOn(api, "getIssueEvents").mockResolvedValue([
    message(1, { id: "e2e-long-title-session", kind: "session" }),
  ]);
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([
    running("e2e-long-title-session", "Reviewing PR #1478"),
  ]);
  const view = renderTab({});

  try {
    const list = await screen.findByRole("list", { name: "Issue activity" });
    await waitFor(() => expect(within(list).getByText("Reviewing PR #1478")).toBeDefined());
    expect(within(list).queryByText(/^session:/)).toBeNull();
  } finally {
    view.unmount();
    getIssueEvents.mockRestore();
    listAgents.mockRestore();
  }
});

// A pin is a write of the one store (W22): a refusal reverts the optimistic pin and says so in the
// shell's pending-writes indicator, with Retry, which re-sends the pin; nothing else is held while
// it waits, so the Pin buttons stay live.
test("a pin the server refuses shows refused in the store, and Retry re-sends it", async () => {
  const originalGetMyState = api.getMyState;
  const originalPutIssueState = api.putIssueState;
  const getIssueEvents = spyOn(api, "getIssueEvents").mockResolvedValue([message(1)]);
  const writes: string[][] = [];
  let refuse = true;
  let unmount: (() => void) | undefined;
  const queryClient = newQueryClient();
  queryClient.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });

  try {
    api.getMyState = async () => ({ "CORE-1": issueState(9) });
    api.putIssueState = async (_issueKey, input) => {
      const dismissed = input.dismissed ?? [];
      writes.push(dismissed);
      if (refuse) {
        throw new ApiError(409, { code: "PIN_REFUSED", error: "refused" });
      }
      return { ...issueState(9), dismissed, seq: input.seq ?? 1 };
    };
    unmount = renderTab({ "CORE-1": issueState(9) }, queryClient).unmount;
    fireEvent.click(await screen.findByRole("button", { name: "Pin" }));

    await waitFor(() =>
      expect(
        writesOf(queryClient)
          .rows()
          .map((write) => [write.label, write.status])
      ).toEqual([["Pin on CORE-1", "refused"]])
    );
    expect(screen.getByRole("button", { name: "Pin" }).hasAttribute("disabled")).toBe(false);

    refuse = false;
    act(() => writesOf(queryClient).retry(writesOf(queryClient).rows()[0]?.id ?? 0));
    await waitFor(() => expect(writesOf(queryClient).rows()).toEqual([]));
    expect(writes).toEqual([["pinned_items:event:1"], ["pinned_items:event:1"]]);
  } finally {
    unmount?.();
    api.getMyState = originalGetMyState;
    api.putIssueState = originalPutIssueState;
    getIssueEvents.mockRestore();
  }
});

const ROW_HEIGHT = 40;
const LIST_TOP = 100;

function setScrollY(value: number): void {
  Object.defineProperty(window, "scrollY", { configurable: true, value, writable: true });
}

/** The geometry `ViewportAnchor` reads, faked as `ViewportAnchor.test.tsx` fakes it: every row
 *  is `ROW_HEIGHT` tall, stacked from `LIST_TOP` in DOM order, and the page scrolls through them
 *  with `window.scrollY`. Anything that is not a row measures as the list's own box. */
function installRowLayout(): { restore: () => void } {
  const rows = () => [...document.querySelectorAll<HTMLElement>("[data-event-seq]")];
  const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    const all = rows();
    const index = all.indexOf(this as HTMLElement);
    const top = LIST_TOP + (index < 0 ? 0 : index * ROW_HEIGHT) - window.scrollY;
    const height = index < 0 ? all.length * ROW_HEIGHT : ROW_HEIGHT;
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
    } as DOMRect;
  });
  const point = spyOn(document, "elementFromPoint").mockImplementation(
    (_x, y) => rows()[Math.floor((window.scrollY + y - LIST_TOP) / ROW_HEIGHT)] ?? null
  );
  const originalScrollBy = window.scrollBy;
  window.scrollBy = ((x: number | ScrollToOptions, y?: number) => {
    const top = typeof x === "number" ? y : x.top;
    if (top !== undefined) setScrollY(window.scrollY + top);
  }) as typeof window.scrollBy;
  return {
    restore: () => {
      rect.mockRestore();
      point.mockRestore();
      window.scrollBy = originalScrollBy;
    },
  };
}

// The tab reads `["events", <key>]`, the query the stream prepends every published event into at
// the front of page 0 (`prependEventToLog`), and it renders newest first: each arrival pushes a
// reader who is down in the history one row further down unless something compensates. Each row
// carries `data-event-seq`, which is the row identity the anchor follows.
test("an event arriving while the reader is in the history leaves their row where it was", async () => {
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  const getIssueEvents = spyOn(api, "getIssueEvents").mockResolvedValue(
    Array.from({ length: 12 }, (_, index) => message(12 - index))
  );
  const layout = installRowLayout();
  const queryClient = newQueryClient();
  let unmount: (() => void) | undefined;

  try {
    unmount = renderTab({ "CORE-1": issueState(99) }, queryClient).unmount;
    await screen.findByRole("list", { name: "Issue activity" });
    setScrollY(300);
    const reading = document.querySelectorAll<HTMLElement>("[data-event-seq]")[8];
    const seq = reading?.getAttribute("data-event-seq");
    const before = reading?.getBoundingClientRect().top;

    act(() => {
      prependEventToLog(queryClient, message(13));
    });
    await waitFor(() => expect(document.querySelectorAll("[data-event-seq]")).toHaveLength(13));

    expect(document.querySelector(`[data-event-seq="${seq}"]`)?.getBoundingClientRect().top).toBe(
      before
    );
    // The row it held onto moved down one place in the list, so the page scrolled by one row.
    expect(window.scrollY).toBe(300 + ROW_HEIGHT);
  } finally {
    unmount?.();
    layout.restore();
    getIssueEvents.mockRestore();
    if (scrollY === undefined) {
      Reflect.deleteProperty(window, "scrollY");
    } else {
      Object.defineProperty(window, "scrollY", scrollY);
    }
  }
});
