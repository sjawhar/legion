import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { MyRepliesResponse, MyReplyRow } from "../../api/types";
import { RepliesPage } from "./RepliesPage";
import { replyRow } from "./replies-fixture";

const restores: Array<() => void> = [];

afterEach(() => {
  for (const restore of restores.splice(0)) restore();
});

function page(rows: MyReplyRow[], total: number, offset = 0): MyRepliesResponse {
  return { limit: 50, offset, rows, total };
}

function renderPage(list: (offset: number) => MyRepliesResponse) {
  const listMyReplies = spyOn(api, "listMyReplies").mockImplementation(async (query) =>
    list(query.offset ?? 0)
  );
  const markThreadRead = spyOn(api, "markThreadRead").mockResolvedValue(undefined);
  restores.push(
    () => listMyReplies.mockRestore(),
    () => markThreadRead.mockRestore()
  );
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <RepliesPage />
      </QueryClientProvider>
    </MemoryRouter>
  );
  restores.push(() => view.unmount());
  return { listMyReplies, markThreadRead };
}

const commentRow = replyRow("comment-root", {
  reply: {
    author: { id: "s1", kind: "session" },
    created_at: "2026-10-12T02:00:00.123456Z",
    first_line: "Fixed in **abc123**.",
    id: "comment-reply",
    kind: "reply",
  },
  subject: "Please fix the flake",
});

const askRow = replyRow("ask-1", {
  document: { name: "Notes", project: "CORE", slug: "notes" },
  issue: undefined,
  kind: "ask",
  ref: "/projects/CORE/documents/notes?ask=ask-1",
  reply: {
    author: { id: "s2", kind: "session" },
    created_at: "2026-10-12T01:00:00Z",
    first_line: "Thanks, done.",
    id: "ask-reply",
    kind: "reply",
  },
  subject: "Which release?",
});

test("rows read newest first, each naming the replier, the reply, where it is and the thread", async () => {
  renderPage(() => page([commentRow, askRow], 2));
  const rows = await screen.findAllByRole("listitem");
  expect(rows.map((row) => row.getAttribute("data-replies-row"))).toEqual([
    "comment-root",
    "ask-1",
  ]);
  const first = within(rows[0] as HTMLElement);
  await waitFor(() => expect(first.getByText("abc123").tagName).toBe("STRONG"));
  expect(first.getByText(/Please fix the flake/)).toBeTruthy();
  expect(first.getByRole("link", { name: /CORE-1/ }).getAttribute("href")).toBe("/issues/CORE-1");
  // Open follows the server's ref, which names the thread's root.
  expect(first.getByRole("link", { name: "Open" }).getAttribute("href")).toBe(
    "/issues/CORE-1/comments/comment-root"
  );
  const second = within(rows[1] as HTMLElement);
  expect(second.getByRole("link", { name: "Open" }).getAttribute("href")).toBe(
    "/projects/CORE/documents/notes?ask=ask-1"
  );
  expect(second.getByRole("link", { name: /CORE · Notes/ }).getAttribute("href")).toBe(
    "/projects/CORE/documents/notes"
  );
});

// An answer someone else gave is a write in the thread like a reply: the row names who answered,
// says it is an answer, and shows the chosen option and the answer's text.
test("an answer reads as who answered, the word answered, and the option and text", async () => {
  renderPage(() =>
    page(
      [
        replyRow("ask-2", {
          kind: "ask",
          ref: "/issues/CORE-1/asks/ask-2",
          reply: {
            author: { id: "bob@example.com", kind: "user" },
            created_at: "2026-10-12T03:00:00Z",
            first_line: "eu-west-1 - closest to the users",
            id: "ask-2",
            kind: "answer",
          },
          subject: "Which region?",
        }),
      ],
      1
    )
  );
  const row = within((await screen.findAllByRole("listitem"))[0] as HTMLElement);
  expect(row.getByText("bob@example.com")).toBeTruthy();
  expect(row.getByText("answered")).toBeTruthy();
  expect(row.getByText(/eu-west-1 - closest to the users/)).toBeTruthy();
});

test("Mark read marks the thread through the reply the row shows, and the list refreshes", async () => {
  let read = false;
  const { listMyReplies, markThreadRead } = renderPage(() =>
    read ? page([], 0) : page([commentRow], 1)
  );
  const row = await screen.findByText("Open");
  const before = listMyReplies.mock.calls.length;
  markThreadRead.mockImplementation(async () => {
    read = true;
  });
  fireEvent.click(
    within(row.closest("li") as HTMLElement).getByRole("button", { name: "Mark read" })
  );
  await waitFor(() =>
    expect(markThreadRead.mock.calls).toEqual([["comment-root", "2026-10-12T02:00:00.123456Z"]])
  );
  expect(await screen.findByText("No new replies in threads you wrote in.")).toBeTruthy();
  expect(listMyReplies.mock.calls.length).toBeGreaterThan(before);
});

test("Show more reads the next page after the rows already shown", async () => {
  const firstPage = Array.from({ length: 50 }, (_, index) => replyRow(`thread-${index}`));
  const { listMyReplies } = renderPage((offset) =>
    offset === 0 ? page(firstPage, 51) : page([replyRow("thread-50")], 51, 50)
  );
  fireEvent.click(await screen.findByRole("button", { name: "Show more" }));
  await waitFor(() => expect(screen.getAllByRole("listitem")).toHaveLength(51));
  expect(listMyReplies.mock.calls.map((call) => call[0])).toEqual([
    { limit: 50, offset: 0 },
    { limit: 50, offset: 50 },
  ]);
  expect(screen.queryByRole("button", { name: "Show more" })).toBeNull();
});

// A reply landing between two page reads moves a thread up, so the next page can repeat the last
// row of the one before; the thread shows once, where it was first listed.
test("a row a shifted later page repeats shows once", async () => {
  const firstPage = Array.from({ length: 50 }, (_, index) => replyRow(`thread-${index}`));
  renderPage((offset) =>
    offset === 0
      ? page(firstPage, 52)
      : page([replyRow("thread-49"), replyRow("thread-50"), replyRow("thread-51")], 52, 50)
  );
  fireEvent.click(await screen.findByRole("button", { name: "Show more" }));
  await waitFor(() =>
    expect(screen.getAllByRole("listitem").at(-1)?.getAttribute("data-replies-row")).toBe(
      "thread-51"
    )
  );
  const ids = screen.getAllByRole("listitem").map((row) => row.getAttribute("data-replies-row"));
  expect([ids.length, new Set(ids).size, ids.indexOf("thread-49")]).toEqual([52, 52, 49]);
});

test("with nothing unread, the page says so", async () => {
  renderPage(() => page([], 0));
  expect(await screen.findByText("No new replies in threads you wrote in.")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Show more" })).toBeNull();
});
