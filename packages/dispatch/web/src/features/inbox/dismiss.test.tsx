/**
 * The Inbox's **Dismiss**: what one press sends, what it takes off the page before the server
 * answers, what it puts back when the server refuses, and the `e` and the bulk bar that press it
 * (INFRA-450).
 */

import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { answerInboxDismisses } from "../../__tests__/send-answers";
import { ApiError, api } from "../../api/client";
import {
  dismissedThreadsQuery,
  inboxQuery,
  inboxThreadsQuery,
  myRepliesCountQuery,
  whoAmIQuery,
} from "../../api/queries";
import type { InboxRow, MyRepliesResponse, MyReplyRow } from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { plannerAgentRow, replyRow } from "../threads/replies-fixture";
import { Inbox } from "./Inbox";
import { issueAsk } from "./inbox-fixture";
import {
  dismissesThread,
  dismissRefusalText,
  dismissThreadRow,
  inboxDismissBody,
} from "./useInboxDismiss";

/** What a press carries for one of this file's Reply rows: the version the fixture draws them
 *  at, which every row of it shares. */
const SEEN = { version: replyRow("c1").version };

const AS_OF = "2026-10-12T02:00:00Z";

/** Every API mock a test installed, restored after it. */
const restores: { mockRestore: () => void }[] = [];

afterEach(() => {
  cleanup();
  for (const mock of restores.splice(0)) mock.mockRestore();
});

function threadsRead(rows: readonly MyReplyRow[]): MyRepliesResponse {
  return { as_of: AS_OF, limit: 0, offset: 0, rows: [...rows], total: rows.length };
}

/** A client holding everything the Inbox reads, with `asks` open and `rows` in the Inbox's own
 *  thread read. The refetch a landed press triggers is answered from the same rows. */
function seeded(rows: readonly MyReplyRow[], asks: readonly InboxRow[] = []): QueryClient {
  restores.push(spyOn(api, "getCredentialPending").mockResolvedValue({ pending: [] }));
  restores.push(spyOn(api, "listMyReplies").mockResolvedValue(threadsRead(rows)));
  restores.push(spyOn(api, "getMyAgentState").mockResolvedValue({}));
  const client = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  client.setQueryData(inboxQuery().queryKey, [...asks]);
  client.setQueryData(inboxThreadsQuery().queryKey, threadsRead(rows));
  client.setQueryData(myRepliesCountQuery().queryKey, rows.length);
  return client;
}

function renderInbox(client: QueryClient, address = "/"): void {
  render(
    <MemoryRouter initialEntries={[address]}>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <Inbox />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

function rowIds(): string[] {
  return [...document.querySelectorAll("[data-inbox-row]")].map(
    (row) => row.getAttribute("data-inbox-row") ?? ""
  );
}

test("the body names either the rows with what the page drew in each or the list's narrowing with its read mark", () => {
  // A dismissal names every row it changes with what the page drew in it, per kind: a Reply
  // row's own version, and an Agent row's newest session reply with the ids it drew as unread,
  // which is what keeps the press from burying a write the page never drew.
  const reply = replyRow("c1");
  const agent = plannerAgentRow();
  expect(inboxDismissBody({ rows: [dismissThreadRow(reply), dismissThreadRow(agent)] })).toEqual({
    seen_replies: {
      "agent:message-1": { newest_reply: "reply-1", unread_replies: ["reply-1"] },
      "reply:c1": { version: reply.version },
    },
    threads: ["reply:c1", "agent:message-1"],
  });
  // A restore draws the rows back rather than burying anything, so it carries nothing: the
  // route refuses a restore that names any.
  expect(inboxDismissBody({ restore: true, rows: [dismissThreadRow(reply)] })).toEqual({
    restore: true,
    threads: ["reply:c1"],
  });
  // **Select all** of the paged view: the ids are not all loaded, so the narrowing and the list's
  // own `as_of` go instead - never both, which the route refuses - and it carries nothing drawn
  // either, the server drawing the rows itself there.
  expect(inboxDismissBody({ filters: { type: ["reply"] }, through: AS_OF })).toEqual({
    dismiss_through: AS_OF,
    kind: ["reply"],
  });
});

test("a press under the narrowing changes the threads the list answers at or before its mark", () => {
  const body = inboxDismissBody({ filters: {}, through: AS_OF });
  const row = replyRow("c1");
  expect(dismissesThread(row, body)).toBe(true);
  // Written in since the mark: the reader has not seen that write, so the press leaves it.
  expect(
    dismissesThread({ ...row, reply: { ...row.reply, created_at: "2026-10-12T03:00:00Z" } }, body)
  ).toBe(false);
  // By id, the narrowing says nothing: the ids alone decide.
  expect(dismissesThread(row, inboxDismissBody({ rows: [dismissThreadRow(row)] }))).toBe(true);
  expect(dismissesThread(row, inboxDismissBody({ rows: [dismissThreadRow(replyRow("c9"))] }))).toBe(
    false
  );
});

test("the row's Dismiss sends the row's own id and drawn version and takes it out of the list at once", async () => {
  const dismissed = answerInboxDismisses();
  const settle = Promise.withResolvers<{ dismissed: number }>();
  dismissed.mockReturnValue(settle.promise);
  renderInbox(seeded([replyRow("c1"), replyRow("c9")]));

  await waitFor(() => expect(rowIds()).toEqual(["reply:c1", "reply:c9"]));
  const control = document.querySelector<HTMLElement>('[data-inbox-dismiss="reply:c1"]');
  fireEvent.click(control as HTMLElement);

  // At once, with the request still out: the row is gone and the shared unread count has dropped
  // by the one unread row it took.
  await waitFor(() => expect(rowIds()).toEqual(["reply:c9"]));
  expect(dismissed.mock.calls[0]?.[0]).toEqual({
    seen_replies: { "reply:c1": SEEN },
    threads: ["reply:c1"],
  });
  settle.resolve({ dismissed: 1 });
  await waitFor(() => expect(dismissed).toHaveBeenCalledTimes(1));
});

test("a refused dismiss puts the row back where it stood and says so", async () => {
  const dismissed = answerInboxDismisses();
  dismissed.mockRejectedValue(new ApiError(404, { error: "not found" }));
  renderInbox(seeded([replyRow("c1"), replyRow("c9")]));

  await waitFor(() => expect(rowIds()).toEqual(["reply:c1", "reply:c9"]));
  fireEvent.click(document.querySelector('[data-inbox-dismiss="reply:c1"]') as HTMLElement);

  const alert = await screen.findByRole("alert");
  expect(alert.textContent).toBe(dismissRefusalText(1, false, "not found"));
  expect(dismissRefusalText(1, false, "not found")).toBe("Could not dismiss 1 thread: not found.");
  // Back in the list, in the place it held.
  expect(rowIds()).toEqual(["reply:c1", "reply:c9"]);
});

test("a row a refused dismiss put back is the reader's hand's again, held under the next read", async () => {
  const dismissed = answerInboxDismisses();
  dismissed.mockRejectedValue(new ApiError(404, { error: "not found" }));
  const client = seeded([replyRow("c1"), replyRow("c9")]);
  renderInbox(client);

  await waitFor(() => expect(rowIds()).toEqual(["reply:c1", "reply:c9"]));
  fireEvent.click(document.querySelector('[data-inbox-dismiss="reply:c1"]') as HTMLElement);
  await screen.findByRole("alert");

  // The reader's hand is on the row the refusal put back, and the next read of the list no longer
  // holds it - somebody else dealt with the thread. It stays where they can see it, as any row
  // under their hand does: the press that took it out of the hold has settled.
  const row = document.querySelector<HTMLElement>('[data-inbox-row="reply:c1"]');
  act(() => row?.focus());
  act(() => {
    client.setQueryData(inboxThreadsQuery().queryKey, threadsRead([replyRow("c9")]));
  });
  expect(rowIds()).toEqual(["reply:c1", "reply:c9"]);
});

test("`e` dismisses the thread row in hand and does nothing on an ask", async () => {
  const dismissed = answerInboxDismisses();
  // Held open, so the list under test is the one the key leaves behind rather than the read the
  // landed press asks for.
  const settle = Promise.withResolvers<{ dismissed: number }>();
  dismissed.mockReturnValue(settle.promise);
  const ask = issueAsk({ id: "ask-1", question: "Which approach?" });
  renderInbox(seeded([replyRow("c1")], [ask]));

  await waitFor(() => expect(rowIds()).toContain("reply:c1"));
  // The focused row is the key's hand below the reading column's width.
  const askRow = document.querySelector<HTMLElement>('[data-inbox-row="ask-1"]');
  act(() => askRow?.focus());
  fireEvent.keyDown(document.body, { key: "e" });
  expect(dismissed).not.toHaveBeenCalled();

  const threadRow = document.querySelector<HTMLElement>('[data-inbox-row="reply:c1"]');
  act(() => threadRow?.focus());
  fireEvent.keyDown(document.body, { key: "e" });
  await waitFor(() => expect(dismissed).toHaveBeenCalledTimes(1));
  expect(dismissed.mock.calls[0]?.[0]).toEqual({
    seen_replies: { "reply:c1": SEEN },
    threads: ["reply:c1"],
  });
  expect(rowIds()).not.toContain("reply:c1");
  settle.resolve({ dismissed: 1 });
});

test("the bulk bar offers Snooze for the asks among the selection and Dismiss for the threads", async () => {
  const dismissed = answerInboxDismisses();
  dismissed.mockResolvedValue({ dismissed: 1 });
  const ask = issueAsk({ id: "ask-1", question: "Which approach?" });
  renderInbox(seeded([replyRow("c1")], [ask]));

  await waitFor(() => expect(rowIds()).toContain("reply:c1"));
  // A thread row takes the mark `x` makes, as an ask does.
  fireEvent.click(screen.getAllByLabelText("Select Root")[0] as HTMLElement);
  await screen.findByText("1 selected");
  expect(screen.queryByLabelText("Snooze selected asks")).toBeNull();
  expect(screen.getByRole("button", { name: "Dismiss selected threads" })).toBeTruthy();

  const asksMark = [...document.querySelectorAll<HTMLElement>('input[type="checkbox"]')].find(
    (box) => box.getAttribute("aria-label")?.startsWith("Select Root") !== true
  );
  fireEvent.click(asksMark as HTMLElement);
  await screen.findByText("2 selected");
  expect(screen.getByLabelText("Snooze selected asks")).toBeTruthy();

  fireEvent.click(screen.getByRole("button", { name: "Dismiss selected threads" }));
  await waitFor(() => expect(rowIds()).not.toContain("reply:c1"));
  expect(dismissed.mock.calls[0]?.[0]).toEqual({
    seen_replies: { "reply:c1": SEEN },
    threads: ["reply:c1"],
  });
});

test("Select all over the Inbox's own read names every row of the group by id and drawn version", async () => {
  const dismissed = answerInboxDismisses();
  dismissed.mockResolvedValue({ dismissed: 2 });
  renderInbox(seeded([replyRow("c1"), replyRow("c9")]));

  await waitFor(() => expect(rowIds()).toEqual(["reply:c1", "reply:c9"]));
  fireEvent.click(screen.getAllByLabelText("Select Root")[0] as HTMLElement);
  await screen.findByText("1 selected");
  fireEvent.click(screen.getByRole("button", { name: "Select all" }));
  await screen.findByText("Every row selected");

  // The Inbox's thread read is unpaged, so every id of the group is in hand - the rows behind
  // **Show more** and inside a folded band with them - and one request names them all, each
  // under the version its own row is showing.
  fireEvent.click(screen.getByRole("button", { name: "Dismiss selected threads" }));
  await waitFor(() => expect(dismissed).toHaveBeenCalledTimes(1));
  expect(dismissed.mock.calls[0]?.[0]).toEqual({
    seen_replies: { "reply:c1": SEEN, "reply:c9": SEEN },
    threads: ["reply:c1", "reply:c9"],
  });
});

test("Select all in the paged Dismissed view sends the narrowing with that list's read mark", async () => {
  const dismissed = answerInboxDismisses();
  dismissed.mockResolvedValue({ dismissed: 2 });
  const client = seeded([]);
  client.setQueryData(dismissedThreadsQuery().queryKey, {
    pageParams: [undefined],
    pages: [threadsRead([replyRow("c1"), replyRow("c9")])],
  });
  renderInbox(client, "/?show=dismissed");

  await waitFor(() => expect(rowIds()).toEqual(["reply:c1", "reply:c9"]));
  fireEvent.click(screen.getAllByLabelText("Select Root")[0] as HTMLElement);
  await screen.findByText("1 selected");
  fireEvent.click(screen.getByRole("button", { name: "Select all" }));
  await screen.findByText("Every row selected");

  // This list is the one the server pages, so the ids behind **Show more** are not loaded: the
  // press names the narrowing and the mark the list was read at instead, never both.
  fireEvent.click(screen.getByRole("button", { name: "Move to Inbox selected threads" }));
  await waitFor(() => expect(dismissed).toHaveBeenCalledTimes(1));
  expect(dismissed.mock.calls[0]?.[0]).toEqual({
    dismiss_through: AS_OF,
    kind: ["reply", "agent"],
    restore: true,
  });
});
