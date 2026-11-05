import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { api } from "../../api/client";
import type { GraphReferences } from "../../api/types";
import { IssueLinks } from "./IssueLinks";

function edge(kind: string, target: string): GraphReferences["edges"][number] {
  return {
    kind,
    direction: "out",
    node: { kind: "issue", id: target, ref: `dispatch://${target}` },
    created_at: "2026-11-04T00:00:00Z",
    source_seq: null,
  };
}

function answer(target?: string): GraphReferences {
  return {
    node: { kind: "issue", id: "CORE-1" },
    edges: target === undefined ? [] : [edge("duplicate_of", target)],
  };
}
let restore: (() => void) | undefined;
afterEach(() => {
  restore?.();
  restore = undefined;
});

function panel() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<IssueLinks issueKey="CORE-1" version={1} />, {
    wrapper: ({ children }) => (
      <MemoryRouter>
        <QueryClientProvider client={client}>{children}</QueryClientProvider>
      </MemoryRouter>
    ),
  });
}

test("Links uses the named read and refreshes after the issue sequence changes", async () => {
  const read = spyOn(api, "getIssueLinks").mockResolvedValue(answer("CORE-2"));
  const view = panel();
  restore = () => {
    view.unmount();
    read.mockRestore();
  };
  const region = within(await screen.findByRole("region", { name: "Links" }));
  expect((await region.findByRole("link", { name: "CORE-2" })).getAttribute("href")).toBe(
    "/issues/CORE-2"
  );
  expect(read.mock.calls[0]).toEqual(["CORE-1"]);
  read.mockResolvedValue(answer("CORE-3"));
  view.rerender(<IssueLinks issueKey="CORE-1" version={2} />);
  await region.findByRole("link", { name: "CORE-3" });
  expect(region.queryByRole("link", { name: "CORE-2" })).toBeNull();
});

test("Links refusal is visible and Retry reads again", async () => {
  const read = spyOn(api, "getIssueLinks")
    .mockRejectedValueOnce(new Error("unavailable"))
    .mockResolvedValue(answer("CORE-2"));
  const view = panel();
  restore = () => {
    view.unmount();
    read.mockRestore();
  };
  const region = within(await screen.findByRole("region", { name: "Links" }));
  await region.findByText("Could not read links.");
  fireEvent.click(region.getByRole("button", { name: "Retry" }));
  await screen.findByRole("link", { name: "CORE-2" });
  expect(screen.queryByText("Could not read links.")).toBeNull();
  expect(read.mock.calls).toHaveLength(2);
});

test("Links leaves out the header's own parent and components, and an issue with only those shows no Links", async () => {
  const parent = edge("child_of", "CORE-9");
  const component = { ...edge("affects", "api"), node: { kind: "component" as const, id: "api" } };
  const read = spyOn(api, "getIssueLinks").mockResolvedValue({
    node: { kind: "issue", id: "CORE-1" },
    edges: [parent, component, edge("duplicate_of", "CORE-2")],
  });
  const view = panel();
  restore = () => {
    view.unmount();
    read.mockRestore();
  };
  const region = within(await screen.findByRole("region", { name: "Links" }));
  await region.findByRole("link", { name: "CORE-2" });
  expect(region.queryByText("CORE-9")).toBeNull();
  expect(region.queryByText(/api/)).toBeNull();

  read.mockResolvedValue({ node: { kind: "issue", id: "CORE-1" }, edges: [parent, component] });
  view.rerender(<IssueLinks issueKey="CORE-1" version={2} />);
  await waitFor(() => expect(read.mock.calls).toHaveLength(2));
  await read.mock.results[1]?.value;
  await waitFor(() => expect(screen.queryByRole("region", { name: "Links" })).toBeNull());
});
