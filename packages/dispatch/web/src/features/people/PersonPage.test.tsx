import { afterEach, beforeEach, expect, type Mock, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { api } from "../../api/client";
import type { PersonNode } from "../../api/types";
import { PersonPage } from "./PersonPage";

let list: Mock<typeof api.listPersonNodes>;
beforeEach(() => {
  list = spyOn(api, "listPersonNodes");
});
afterEach(() => {
  cleanup();
  list.mockRestore();
});

function renderPerson() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <MemoryRouter initialEntries={["/people/alice"]}>
      <QueryClientProvider client={client}>
        <Routes>
          <Route element={<PersonPage />} path="/people/:login" />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

const authored: PersonNode = {
  kind: "comment",
  id: "one",
  relations: ["authored_by"],
  title: "First node",
  created_at: "2026-11-04T07:00:00Z",
  ref: "dispatch://CORE-1/comment/one",
  href: "/issues/CORE-1/comments/one",
};

test("lists graph relations and loads the next offset without dropping prior rows", async () => {
  list.mockImplementation(async (login, page) => {
    expect(login).toBe("alice");
    return {
      person: "alice",
      limit: page.limit,
      offset: page.offset,
      total: 2,
      rows:
        page.offset === 0
          ? [authored]
          : [
              {
                ...authored,
                id: "two",
                relations: ["delivered_to"],
                title: "Second node",
                ref: null,
                href: "/agents/reader/live",
              },
            ],
    };
  });
  const view = renderPerson();
  await waitFor(() => expect(view.getByText("First node")).toBeTruthy());
  const rows = view.getByRole("list", { name: "Person nodes" });
  expect(within(rows).getByText("Authored")).toBeTruthy();
  fireEvent.click(view.getByRole("button", { name: "Load older" }));
  await waitFor(() => expect(within(rows).getAllByRole("listitem").length).toBe(2));
  expect(within(rows).getByText("First node")).toBeTruthy();
  expect(within(rows).getByText("Received")).toBeTruthy();
  expect(within(rows).getAllByRole("link", { name: "Open" })[1]?.getAttribute("href")).toBe(
    "/agents/reader/live"
  );
  expect(view.queryByRole("button", { name: "Load older" })).toBeNull();
});

test("shows an empty graph without inventing received nodes", async () => {
  list.mockResolvedValue({ person: "alice", limit: 50, offset: 0, total: 0, rows: [] });
  const view = renderPerson();
  await waitFor(() =>
    expect(view.getByText("No authored or received nodes are recorded.")).toBeTruthy()
  );
  expect(
    within(view.getByRole("list", { name: "Person nodes" })).queryAllByRole("listitem")
  ).toHaveLength(0);
});

test("shows a refused person read rather than an empty success", async () => {
  list.mockRejectedValue(new Error("person not found"));
  const view = renderPerson();
  await waitFor(() =>
    expect(view.getByText("Could not load person nodes: person not found")).toBeTruthy()
  );
  expect(view.queryByText("No authored or received nodes are recorded.")).toBeNull();
});
