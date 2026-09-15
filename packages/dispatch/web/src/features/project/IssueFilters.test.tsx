import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { useState } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { api } from "../../api/client";
import type { IssueSummary, UserState } from "../../api/types";
import { userPreferenceStorageKey } from "../shell/userPreference";
import { IssueFilters } from "./IssueFilters";
import { IssueList } from "./IssueList";

function issue(overrides: Partial<IssueSummary> = {}): IssueSummary {
  return {
    key: "CORE-1",
    labels: [],
    last_seq: 0,
    open_asks: 0,
    parent: null,
    status: "todo",
    priority: null,
    rank: "U",
    title: "Core work",
    updated_at: "2026-09-10T00:00:00Z",
    ...overrides,
  };
}

function LocationSearch() {
  return <output data-testid="location-search">{useLocation().search}</output>;
}

async function openFilters(): Promise<void> {
  const disclosure = await screen.findByRole("button", { name: /Filters · \d+ active/ });
  if (disclosure.getAttribute("aria-expanded") === "false") {
    fireEvent.click(disclosure);
  }
}

/** The strip and the List as `ProjectPage` mounts them: siblings sharing the URL, plus the
 *  page-held view-local Status state the strip's select drives. */
function StripAndList({ login, showStatus }: { login?: string; showStatus: boolean }) {
  const [status, setStatus] = useState("all");
  return (
    <>
      <IssueFilters
        login={login}
        onStatusChange={setStatus}
        project="CORE"
        showStatus={showStatus}
        status={status}
      />
      <IssueList project="CORE" status={status} />
    </>
  );
}

function renderStrip(
  issues: IssueSummary[],
  state: UserState = {},
  initialEntry = "/projects/CORE",
  login = "alice",
  showStatus = true
) {
  const listIssues = spyOn(api, "listIssues").mockImplementation(async (options = {}) => {
    const labels = options.labels ?? [];
    return labels.length === 0
      ? issues
      : issues.filter((issue) => labels.every((label) => (issue.labels ?? []).includes(label)));
  });
  const getMyState = spyOn(api, "getMyState").mockResolvedValue(state);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={[initialEntry]}>
      <QueryClientProvider client={queryClient}>
        <StripAndList login={login} showStatus={showStatus} />
        <LocationSearch />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { getMyState, listIssues, view };
}

test("Needs you keeps only issues with open asks and lives in the URL", async () => {
  const { getMyState, listIssues, view } = renderStrip([
    issue({ key: "CORE-1", open_asks: 1, title: "Answer me" }),
    issue({ key: "CORE-2", title: "Quiet" }),
  ]);

  try {
    await screen.findByText("Answer me");
    await openFilters();
    fireEvent.click(screen.getByRole("button", { name: "Needs you" }));
    expect(screen.getByText("Answer me")).toBeTruthy();
    expect(screen.queryByText("Quiet")).toBeNull();
    expect(screen.getByTestId("location-search").textContent).toBe("?needs-you=1");
    fireEvent.click(screen.getByRole("button", { name: "Remove Needs you filter" }));
    expect(screen.getByTestId("location-search").textContent).toBe("");
    expect(await screen.findByText("Quiet")).toBeTruthy();
  } finally {
    view.unmount();
    getMyState.mockRestore();
    listIssues.mockRestore();
  }
});

test("Unread keeps only issues with events past last_read_seq and lives in the URL", async () => {
  const { getMyState, listIssues, view } = renderStrip(
    [
      issue({ key: "CORE-1", last_seq: 4, title: "Unread" }),
      issue({ key: "CORE-2", last_seq: 3, title: "Read" }),
    ],
    {
      "CORE-1": { dismissed: [], last_read_seq: 3, pinned: false },
      "CORE-2": { dismissed: [], last_read_seq: 3, pinned: false },
    }
  );

  try {
    await screen.findByRole("link", { name: /CORE-1.*Unread/ });
    await openFilters();
    fireEvent.click(screen.getByRole("button", { name: "Unread" }));
    expect(screen.getByRole("link", { name: /CORE-1.*Unread/ })).toBeTruthy();
    expect(screen.queryByText("Read")).toBeNull();
    expect(screen.getByTestId("location-search").textContent).toBe("?unread=1");
  } finally {
    view.unmount();
    getMyState.mockRestore();
    listIssues.mockRestore();
  }
});

test("label chips send every selection to the API and retain only AND matches", async () => {
  const { getMyState, listIssues, view } = renderStrip([
    issue({ key: "CORE-1", labels: ["frontend", "api"], title: "Navigation" }),
    issue({ key: "CORE-2", labels: ["frontend", "docs"], title: "Guidance" }),
    issue({ key: "CORE-3", labels: ["backend", "docs"], title: "Endpoint" }),
  ]);

  try {
    await screen.findByText("Navigation");
    await openFilters();
    fireEvent.click(screen.getByRole("button", { name: "frontend" }));
    await waitFor(() =>
      expect(listIssues).toHaveBeenCalledWith({ labels: ["frontend"], project: "CORE" })
    );
    fireEvent.click(screen.getByRole("button", { name: "docs" }));
    await waitFor(() =>
      expect(listIssues).toHaveBeenCalledWith({ labels: ["frontend", "docs"], project: "CORE" })
    );
    expect(screen.getByText("Guidance")).toBeTruthy();
    expect(screen.queryByText("Navigation")).toBeNull();
    expect(screen.queryByText("Endpoint")).toBeNull();
  } finally {
    view.unmount();
    getMyState.mockRestore();
    listIssues.mockRestore();
  }
});

test("restores every filter from the URL and writes changes back to it", async () => {
  const { getMyState, listIssues, view } = renderStrip(
    [
      issue({ key: "CORE-1", labels: ["docs"], title: "Guidance" }),
      issue({ key: "CORE-2", labels: ["frontend"], title: "Navigation" }),
    ],
    {},
    "/projects/CORE?label=docs"
  );

  try {
    await screen.findByText("Guidance");
    await openFilters();
    await waitFor(() =>
      expect(listIssues).toHaveBeenCalledWith({ labels: ["docs"], project: "CORE" })
    );
    expect(screen.getByRole("button", { name: "docs" }).getAttribute("aria-pressed")).toBe("true");
    expect(screen.getByTestId("location-search").textContent).toBe("?label=docs");

    fireEvent.click(screen.getByRole("button", { name: "frontend" }));
    await waitFor(() =>
      expect(listIssues).toHaveBeenCalledWith({ labels: ["docs", "frontend"], project: "CORE" })
    );
    expect(screen.getByTestId("location-search").textContent).toBe("?label=docs&label=frontend");

    fireEvent.change(screen.getByRole("searchbox", { name: "Search issues" }), {
      target: { value: "guid" },
    });
    expect(screen.getByTestId("location-search").textContent).toBe(
      "?label=docs&label=frontend&q=guid"
    );
    fireEvent.click(screen.getByRole("button", { name: "Remove Search: guid filter" }));

    fireEvent.click(screen.getByRole("button", { name: "Clear labels" }));
    expect(screen.getByTestId("location-search").textContent).toBe("");
  } finally {
    view.unmount();
    getMyState.mockRestore();
    listIssues.mockRestore();
  }
});

test("a search restored from ?q= narrows the list before anyone types", async () => {
  const { getMyState, listIssues, view } = renderStrip(
    [issue({ key: "CORE-1", title: "Guidance" }), issue({ key: "CORE-2", title: "Navigation" })],
    {},
    "/projects/CORE?q=guid"
  );

  try {
    await screen.findByText("Guidance");
    expect(screen.queryByText("Navigation")).toBeNull();
    expect(screen.getByRole("button", { name: "Remove Search: guid filter" })).toBeTruthy();
  } finally {
    view.unmount();
    getMyState.mockRestore();
    listIssues.mockRestore();
  }
});

test("collapses filter controls by default and retains the disclosure preference per login", async () => {
  window.localStorage.clear();
  const { getMyState, listIssues, view } = renderStrip([issue()]);

  try {
    await screen.findByText("Core work");
    const disclosure = screen.getByRole("button", { name: "Filters · 0 active" });
    expect(disclosure.getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByRole("combobox", { name: "Status" })).toBeNull();

    fireEvent.click(disclosure);
    fireEvent.change(await screen.findByRole("combobox", { name: "Status" }), {
      target: { value: "done" },
    });
    expect(
      screen.getByRole("button", { name: "Filters · 1 active" }).getAttribute("aria-expanded")
    ).toBe("true");

    fireEvent.click(screen.getByRole("button", { name: "Filters · 1 active" }));
    fireEvent.click(screen.getByRole("button", { name: "Remove Status: done filter" }));
    expect(screen.getByRole("button", { name: "Filters · 0 active" })).toBeTruthy();
    expect(
      window.localStorage.getItem(userPreferenceStorageKey("alice", "project.issue-filters"))
    ).toBe("collapsed");
  } finally {
    view.unmount();
    getMyState.mockRestore();
    listIssues.mockRestore();
    window.localStorage.clear();
  }
});

test("collapses a saved filter disclosure when no filters are active", async () => {
  window.localStorage.setItem(
    userPreferenceStorageKey("alice", "project.issue-filters"),
    "expanded"
  );
  const { getMyState, listIssues, view } = renderStrip([issue()]);

  try {
    await screen.findByText("Core work");
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: "Filters · 0 active" }).getAttribute("aria-expanded")
      ).toBe("false")
    );
  } finally {
    view.unmount();
    getMyState.mockRestore();
    listIssues.mockRestore();
    window.localStorage.clear();
  }
});

test("without showStatus the strip offers no Status select and no Status chip", async () => {
  const { getMyState, listIssues, view } = renderStrip(
    [issue()],
    {},
    "/projects/CORE?label=x",
    "alice",
    false
  );

  try {
    await screen.findByRole("button", { name: "Filters · 1 active" });
    expect(screen.queryByRole("combobox", { name: "Status" })).toBeNull();
    expect(screen.getByRole("button", { name: "Remove Label: x filter" })).toBeTruthy();
  } finally {
    view.unmount();
    getMyState.mockRestore();
    listIssues.mockRestore();
  }
});
