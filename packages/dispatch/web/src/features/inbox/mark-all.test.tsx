import { afterEach, expect, test } from "bun:test";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { answerInboxReads } from "../../__tests__/send-answers";
import { ApiError } from "../../api/client";
import { inboxThreadsQuery, myRepliesCountQuery, whoAmIQuery } from "../../api/queries";
import type { MyRepliesResponse, MyReplyRow } from "../../api/types";
import { plannerAgentRow, replyRow } from "../threads/replies-fixture";
import { InboxBar, type InboxFilters } from "./InboxBar";
import type { InboxListRow } from "./list-row";
import { MARK_ALL_REFUSED_TEXT, markAllReadBody, marksThread, useMarkAllRead } from "./mark-all";
import { useUnreadThreadCount } from "./useInboxCount";

afterEach(cleanup);

const AS_OF = "2026-10-12T02:00:00Z";

function threadsRead(rows: readonly MyReplyRow[]): MyRepliesResponse {
  return { as_of: AS_OF, limit: 0, offset: 0, rows: [...rows], total: rows.length };
}

function noop(): void {}

/**
 * The bar over one list, with the one number the sidebar's Inbox count and the compact header's
 * Inbox link both read beside it (`useUnreadThreadCount` over `myRepliesCountQuery`, which is
 * what `Sidebar.tsx` and `app.tsx` each add their Needs-you count to). The bar is the surface
 * the button and its refusal live on; the press itself is the Inbox's own hook.
 */
function Bar({ filters = {} }: { filters?: InboxFilters }): ReactNode {
  const read = useQuery(inboxThreadsQuery());
  const rows: readonly InboxListRow[] = (read.data?.rows ?? []).map((thread) => ({
    kind: "thread",
    thread,
  }));
  const markAll = useMarkAllRead({ asOf: AS_OF, filters, rows });
  const count = useUnreadThreadCount();
  return (
    <>
      <output data-testid="inbox-count">{count}</output>
      <output data-testid="listed">{rows.length}</output>
      <output data-testid="unread">{(read.data?.rows ?? []).filter((t) => t.unread).length}</output>
      <InboxBar
        agentToken={undefined}
        assignees={undefined}
        filters={filters}
        markAll={markAll}
        onAssigneesChange={noop}
        onClearAgent={noop}
        onFiltersChange={noop}
        onPopoverOpenChange={noop}
        rows={rows}
        selection={["alice"]}
        view={{
          group: "priority",
          onGroupChange: noop,
          onShowChange: noop,
          onSortChange: noop,
          onThreadOrderChange: noop,
          show: "inbox",
          sort: "newest",
          threadOrder: "oldest",
        }}
        viewer="alice"
      />
    </>
  );
}

function renderBar(rows: readonly MyReplyRow[], count: number, filters?: InboxFilters) {
  const client = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  client.setQueryData(inboxThreadsQuery().queryKey, threadsRead(rows));
  client.setQueryData(myRepliesCountQuery().queryKey, count);
  render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <Bar filters={filters} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return client;
}

function inboxCount(): string {
  return screen.getByTestId("inbox-count").textContent ?? "";
}

function listed(): string {
  return screen.getByTestId("listed").textContent ?? "";
}

function unread(): string {
  return screen.getByTestId("unread").textContent ?? "";
}

test("the body carries the list's read mark and the filters the list is under", () => {
  // Nothing narrowed: both kinds, and no list for the three fields, which the route reads as
  // every value.
  expect(markAllReadBody({}, AS_OF)).toEqual({ kind: ["reply", "agent"], read_through: AS_OF });
  expect(
    markAllReadBody(
      { priority: ["P0", "none"], project: ["CORE"], type: ["reply"], urgency: ["none"] },
      AS_OF
    )
  ).toEqual({
    kind: ["reply"],
    priority: ["P0", "none"],
    project: ["CORE"],
    read_through: AS_OF,
    urgency: ["none"],
  });
});

test("a thread written in since the read mark, or already read, is not one this press marks", () => {
  const body = markAllReadBody({}, AS_OF);
  const row = replyRow("c1");
  expect(marksThread(row, body)).toBe(true);
  expect(
    marksThread({ ...row, reply: { ...row.reply, created_at: "2026-10-12T03:00:00Z" } }, body)
  ).toBe(false);
  expect(marksThread({ ...row, unread: false }, body)).toBe(false);
  // An Agent row carries no project, no priority and no urgency, so a filter naming a value for
  // one of them leaves it out and a filter naming `none` keeps it.
  expect(marksThread(plannerAgentRow(), markAllReadBody({ project: ["CORE"] }, AS_OF))).toBe(false);
  expect(marksThread(plannerAgentRow(), markAllReadBody({ project: ["none"] }, AS_OF))).toBe(true);
});

test("the button shows only while the list holds an unread Reply or Agent row", () => {
  renderBar([replyRow("c1", { unread: false })], 0);
  expect(screen.queryByRole("button", { name: "Mark all read" })).not.toBeInTheDocument();
  cleanup();
  renderBar([replyRow("c1")], 1);
  expect(screen.getByRole("button", { name: "Mark all read" })).toBeTruthy();
});

test("one press zeroes the count and draws the rows read, without taking one out of the list", async () => {
  const user = userEvent.setup();
  const marked = answerInboxReads();
  const settle = Promise.withResolvers<{ marked: number }>();
  marked.mockReturnValue(settle.promise);
  renderBar([replyRow("c1", { priority: 1 }), replyRow("c2", { priority: 1 })], 2, {
    priority: ["P1"],
  });

  expect(inboxCount()).toBe("2");
  await user.click(screen.getByRole("button", { name: "Mark all read" }));

  // At once, with the request still out: every badge reading the shared count is at zero and no
  // row is unread any more - and both rows are still in the list, where a thread stays until it
  // is dismissed.
  await waitFor(() => expect(unread()).toBe("0"));
  expect(listed()).toBe("2");
  expect(inboxCount()).toBe("0");
  expect(screen.queryByRole("button", { name: "Mark all read" })).not.toBeInTheDocument();
  expect(screen.queryByRole("alert")).not.toBeInTheDocument();

  expect(marked.mock.calls[0]?.[0]).toEqual({
    kind: ["reply", "agent"],
    priority: ["P1"],
    read_through: AS_OF,
  });
  settle.resolve({ marked: 2 });
  await waitFor(() => expect(marked).toHaveBeenCalledTimes(1));
});

test("a refused press brings the rows and the counts back and says nothing was marked", async () => {
  const user = userEvent.setup();
  const marked = answerInboxReads();
  // An old server mid-deploy has no such route, which is a failed request like any other.
  marked.mockRejectedValue(new ApiError(404, { error: "not found" }));
  renderBar([replyRow("c1"), replyRow("c2")], 2);

  await user.click(screen.getByRole("button", { name: "Mark all read" }));

  const alert = await screen.findByRole("alert");
  expect(alert.textContent).toBe(MARK_ALL_REFUSED_TEXT);
  expect(MARK_ALL_REFUSED_TEXT).toBe("Nothing was marked read.");
  // The sidebar's Inbox count and the compact header's Inbox link hold their pre-click number
  // again, and the rows are back in the list with the button over them.
  expect(inboxCount()).toBe("2");
  expect(unread()).toBe("2");
  expect(listed()).toBe("2");
});
