import { expect, spyOn, test } from "bun:test";
import { SEARCH_QUERY_MAX } from "@legion/contracts/dispatch-tools";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { api } from "../../api/client";
import type { IssueSummary, UserState } from "../../api/types";
import { userPreferenceStorageKey } from "../shell/userPreference";
import { IssueFilters } from "./IssueFilters";
import { IssueList } from "./IssueList";

function issue(overrides: Partial<IssueSummary> = {}): IssueSummary {
  return {
    route: null,
    route_status: null,
    route_holder: null,
    key: "CORE-1",
    labels: [],
    last_seq: 0,
    open_asks: 0,
    parent: null,
    assignee: null,
    claim: null,
    components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
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

/** Opens the `Labels` (or `Status`) picker if it is closed and toggles one option. */
function pickOption(picker: "Labels" | "Status", option: string): void {
  const trigger = screen.getByRole("button", { name: new RegExp(`^${picker}( · \\d+)?$`) });
  if (trigger.getAttribute("aria-expanded") === "false") {
    fireEvent.click(trigger);
  }
  fireEvent.click(screen.getByRole("option", { name: option }));
}

/** The strip and the List as `ProjectPage` mounts them: siblings sharing the URL. */
function StripAndList({ showStatus }: { showStatus: boolean }) {
  return (
    <>
      <IssueFilters project="CORE" showStatus={showStatus} />
      <IssueList project="CORE" />
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
  queryClient.setQueryData(["whoami"], { kind: "user", login });
  // `ProjectPage` keeps one strip across the List/Board toggle and flips `showStatus`, so
  // re-rendering this tree with the other value is that toggle. A test that restated the tree
  // instead would remount the strip, and a remount re-reads the disclosure preference.
  const tree = (status: boolean) => (
    <MemoryRouter initialEntries={[initialEntry]}>
      <QueryClientProvider client={queryClient}>
        <StripAndList showStatus={status} />
        <LocationSearch />
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(tree(showStatus));
  return { getMyState, listIssues, tree, view };
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
      "CORE-1": { dismissed: [], last_read_seq: 3, pinned: false, seq: 0 },
      "CORE-2": { dismissed: [], last_read_seq: 3, pinned: false, seq: 0 },
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
    pickOption("Labels", "frontend");
    await waitFor(() =>
      expect(listIssues).toHaveBeenCalledWith({ labels: ["frontend"], project: "CORE" })
    );
    pickOption("Labels", "docs");
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
    fireEvent.click(screen.getByRole("button", { name: "Labels · 1" }));
    expect(screen.getByRole("option", { name: "docs" }).getAttribute("aria-selected")).toBe("true");
    expect(screen.getByTestId("location-search").textContent).toBe("?label=docs");

    pickOption("Labels", "frontend");
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

test("the search filter writes at most SEARCH_QUERY_MAX characters into the page URL", async () => {
  const { getMyState, listIssues, view } = renderStrip([
    issue({ key: "CORE-1", title: "Guidance" }),
  ]);

  try {
    await screen.findByText("Guidance");
    await openFilters();
    fireEvent.change(screen.getByRole("searchbox", { name: "Search issues" }), {
      target: { value: "x".repeat(SEARCH_QUERY_MAX + 500) },
    });
    const written = new URLSearchParams(screen.getByTestId("location-search").textContent ?? "");
    expect(written.get("q")).toBe("x".repeat(SEARCH_QUERY_MAX));
  } finally {
    view.unmount();
    getMyState.mockRestore();
    listIssues.mockRestore();
  }
});

test("a ?q= over the cap from a link is cut on a code point, never leaving half an emoji", async () => {
  const long = `${"x".repeat(SEARCH_QUERY_MAX - 1)}😀`;
  const { getMyState, listIssues, view } = renderStrip(
    [issue({ key: "CORE-1", title: "Guidance" })],
    {},
    `/projects/CORE?q=${encodeURIComponent(long)}`
  );

  try {
    await openFilters();
    fireEvent.change(screen.getByRole("searchbox", { name: "Search issues" }), {
      target: { value: `${long}y` },
    });
    const written = new URLSearchParams(screen.getByTestId("location-search").textContent ?? "");
    expect(written.get("q")).toBe("x".repeat(SEARCH_QUERY_MAX - 1));
  } finally {
    view.unmount();
    getMyState.mockRestore();
    listIssues.mockRestore();
  }
});

test("one or many statuses narrow the List as an OR, each in the URL and each a chip", async () => {
  const { getMyState, listIssues, view } = renderStrip([
    issue({ key: "CORE-1", status: "todo", title: "Planned" }),
    issue({ key: "CORE-2", status: "testing", title: "Under test" }),
    issue({ key: "CORE-3", status: "done", title: "Shipped" }),
  ]);

  try {
    await screen.findByText("Planned");
    await openFilters();
    pickOption("Status", "Todo");
    expect(screen.getByTestId("location-search").textContent).toBe("?status=todo");
    expect(screen.queryByText("Under test")).toBeNull();
    pickOption("Status", "Testing");
    expect(screen.getByTestId("location-search").textContent).toBe("?status=todo&status=testing");
    expect(screen.getByText("Planned")).toBeTruthy();
    expect(screen.getByText("Under test")).toBeTruthy();
    expect(screen.queryByText("Shipped")).toBeNull();
    expect(screen.getByRole("button", { name: "Filters · 2 active" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Remove Status: Todo filter" }));
    expect(screen.getByTestId("location-search").textContent).toBe("?status=testing");
    expect(screen.queryByText("Planned")).toBeNull();
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
    expect(screen.queryByRole("button", { name: "Status" })).toBeNull();

    fireEvent.click(disclosure);
    pickOption("Status", "Done");
    expect(
      screen.getByRole("button", { name: "Filters · 1 active" }).getAttribute("aria-expanded")
    ).toBe("true");

    fireEvent.click(screen.getByRole("button", { name: "Filters · 1 active" }));
    fireEvent.click(screen.getByRole("button", { name: "Remove Status: Done filter" }));
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

// The saved preference is the only owner of the open state: the strip reads it when it mounts
// and when identity resolves, never because the filter count moved. The List folding a
// `?status=` into the count is the one way that count rises on a strip already up, and it used
// to reopen one the reader had left closed.
test("the List and Board toggle leaves a closed strip closed; the reader still opens it", async () => {
  window.localStorage.clear();
  const { getMyState, listIssues, tree, view } = renderStrip(
    [issue()],
    {},
    "/projects/CORE?status=todo",
    "alice",
    false
  );

  try {
    await screen.findByText("Core work");
    // The Board ignores `?status=`, so nothing is active and the strip starts collapsed.
    expect(
      screen.getByRole("button", { name: "Filters · 0 active" }).getAttribute("aria-expanded")
    ).toBe("false");

    view.rerender(tree(true));
    const trigger = screen.getByRole("button", { name: "Filters · 1 active" });
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
    expect(screen.getByRole("button", { name: "Remove Status: Todo filter" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^Status( · \d+)?$/ })).toBeNull();

    fireEvent.click(trigger);
    expect(trigger.getAttribute("aria-expanded")).toBe("true");
    expect(screen.getByRole("button", { name: "Status · 1" })).toBeTruthy();
  } finally {
    view.unmount();
    getMyState.mockRestore();
    listIssues.mockRestore();
    window.localStorage.clear();
  }
});

test("a pending viewer keeps a URL-only filter disclosure collapsed without persisting", async () => {
  const pendingKey = "dispatch.project.issue-filters:undefined";
  window.localStorage.removeItem(pendingKey);
  const whoAmI = spyOn(api, "whoAmI").mockImplementation(
    () => Promise.withResolvers<never>().promise
  );
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([issue()]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={["/projects/CORE?q=guid"]}>
      <QueryClientProvider client={queryClient}>
        <StripAndList showStatus />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const disclosure = await screen.findByRole("button", { name: "Filters · 1 active" });
    expect(disclosure.getAttribute("aria-expanded")).toBe("false");
    expect(window.localStorage.getItem(pendingKey)).toBeNull();
  } finally {
    view.unmount();
    getMyState.mockRestore();
    listIssues.mockRestore();
    whoAmI.mockRestore();
    window.localStorage.removeItem(pendingKey);
  }
});

test("without showStatus the strip offers no Status picker and no Status chip", async () => {
  const { getMyState, listIssues, view } = renderStrip(
    [issue()],
    {},
    "/projects/CORE?label=x&status=done",
    "alice",
    false
  );

  try {
    await screen.findByRole("button", { name: "Filters · 1 active" });
    expect(screen.queryByRole("button", { name: "Status" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Remove Status: Done filter" })).toBeNull();
    expect(screen.getByRole("button", { name: "Remove Label: x filter" })).toBeTruthy();
  } finally {
    view.unmount();
    getMyState.mockRestore();
    listIssues.mockRestore();
  }
});
