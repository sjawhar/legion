import { afterEach, beforeEach, expect, type Mock, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import { whoAmIQuery } from "../../api/queries";
import type { MyThread } from "../../api/types";
import { MyThreadsPage } from "./MyThreadsPage";

let list: Mock<typeof api.listMyThreads>;
beforeEach(() => {
  list = spyOn(api, "listMyThreads");
});
afterEach(() => {
  list.mockRestore();
});
function show(client = new QueryClient({ defaultOptions: { queries: { retry: false } } })) {
  client.setQueryDefaults(whoAmIQuery().queryKey, { staleTime: Number.POSITIVE_INFINITY });
  client.setQueryData(whoAmIQuery().queryKey, { kind: "user", login: "alice", event_head: "0" });
  return render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <MyThreadsPage />
      </QueryClientProvider>
    </MemoryRouter>
  );
}
const row: MyThread = {
  kind: "comment",
  thread_id: "one",
  causes: ["authored"],
  subject: "First thread",
  activity_at: "2026-11-04T07:00:00Z",
  issue_key: "CORE-1",
  project: null,
  document: null,
  href: "/issues/CORE-1/comments/one",
  unread: false,
};
test("pages all participation without dropping read threads or earlier roots", async () => {
  list.mockImplementation(async (page) => ({
    person: "alice",
    limit: page.limit,
    offset: page.offset,
    total: 2,
    rows:
      page.offset === 0
        ? [row]
        : [
            {
              ...row,
              thread_id: "two",
              subject: "Mentioned thread",
              causes: ["mentioned", "addressed"],
            },
          ],
  }));
  const view = show();
  await waitFor(() => expect(view.getByText("First thread")).toBeTruthy());
  const rows = view.getByRole("list", { name: "My threads" });
  expect(within(rows).getByText("Wrote in")).toBeTruthy();
  expect(within(rows).queryByText("Unread")).toBeNull();
  fireEvent.click(view.getByRole("button", { name: "Load older" }));
  await waitFor(() => expect(within(rows).getAllByRole("listitem")).toHaveLength(2));
  expect(within(rows).getByText("First thread")).toBeTruthy();
  expect(within(rows).getByText("Mentioned")).toBeTruthy();
  expect(within(rows).getByText("Addressed")).toBeTruthy();
  expect(view.queryByRole("button", { name: "Load older" })).toBeNull();
});
test("shows a real empty result", async () => {
  list.mockResolvedValue({ person: "alice", rows: [], total: 0, limit: 50, offset: 0 });
  const view = show();
  await waitFor(() =>
    expect(view.getByText("No participation in threads is recorded yet.")).toBeTruthy()
  );
});
test("does not label a refusal as an empty result", async () => {
  list.mockRejectedValue(new Error("read refused"));
  const view = show();
  await waitFor(() =>
    expect(view.getByText("Could not load my threads: read refused")).toBeTruthy()
  );
  expect(view.queryByText("No participation in threads is recorded yet.")).toBeNull();
});

test("switching the authenticated caller never renders the previous person's cached threads", async () => {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  list.mockResolvedValue({ person: "alice", rows: [row], total: 1, limit: 50, offset: 0 });
  const view = show(client);
  await waitFor(() => expect(view.getByText("First thread")).toBeTruthy());
  list.mockResolvedValue({
    person: "bob",
    rows: [{ ...row, thread_id: "bob", subject: "Bob's thread" }],
    total: 1,
    limit: 50,
    offset: 0,
  });
  await act(() => {
    client.setQueryData(whoAmIQuery().queryKey, { kind: "user", login: "bob", event_head: "0" });
  });
  await waitFor(() => expect(view.getByText("Bob's thread")).toBeTruthy());
  expect(view.queryByText("First thread")).toBeNull();
  client.clear();
});
