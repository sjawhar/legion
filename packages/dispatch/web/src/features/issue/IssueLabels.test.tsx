import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen } from "@testing-library/react";

import { api } from "../../api/client";
import type { IssueSummary } from "../../api/types";
import { IssueLabels } from "./IssueLabels";

function summary(key: string, labels: string[]): IssueSummary {
  return {
    key,
    labels,
    last_seq: 1,
    open_asks: 0,
    parent: null,
    assignee: null,
    priority: null,
    rank: "U",
    status: "todo",
    title: key,
    updated_at: "2026-09-09T00:00:00Z",
  };
}

test("the label cloud reads the project's loaded issue list instead of fetching its own copy", async () => {
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 30_000 } },
  });
  queryClient.setQueryData(
    ["issues", "project", "CORE"],
    [summary("CORE-1", ["bug"]), summary("CORE-2", ["frontend", "docs"])]
  );
  const view = render(
    <QueryClientProvider client={queryClient}>
      <IssueLabels
        disabled={false}
        labels={["bug"]}
        onSave={() => Promise.resolve()}
        project="CORE"
        saveError={false}
        saving={false}
      />
    </QueryClientProvider>
  );

  try {
    fireEvent.click(screen.getByRole("button", { name: "Edit labels" }));
    await screen.findByRole("combobox", { name: "Search or create label" });

    expect(await screen.findByRole("option", { name: "frontend" })).toBeDefined();
    expect(screen.getByRole("option", { name: "docs" })).toBeDefined();
    expect(listIssues).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    listIssues.mockRestore();
  }
});

test("a rejected create shows its error until the next keystroke clears it", async () => {
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <IssueLabels
        disabled={false}
        labels={[]}
        onSave={() => Promise.resolve()}
        project="CORE"
        saveError={false}
        saving={false}
      />
    </QueryClientProvider>
  );

  try {
    fireEvent.click(screen.getByRole("button", { name: "Edit labels" }));
    const search = await screen.findByRole("combobox", { name: "Search or create label" });
    const tooLong = "x".repeat(41);
    fireEvent.change(search, { target: { value: tooLong } });
    fireEvent.click(screen.getByRole("option", { name: `Create "${tooLong}"` }));
    expect(screen.getByText("Labels must be 1–40 characters.")).toBeTruthy();
    expect(screen.queryByRole("option", { name: tooLong })).toBeNull();

    fireEvent.change(search, { target: { value: "x" } });
    expect(screen.queryByText("Labels must be 1–40 characters.")).toBeNull();
  } finally {
    view.unmount();
    listIssues.mockRestore();
  }
});
