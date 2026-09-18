import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  fireEvent,
  render,
  screen,
  waitFor,
  waitForElementToBeRemoved,
  within,
} from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import type { ArchitectureSource, InboxRow } from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { ProjectPage } from "./ProjectPage";

function LocationProbe() {
  const location = useLocation();
  return (
    <output data-testid="location">
      {location.pathname}
      {location.search}
    </output>
  );
}

function inboxRow(overrides: Partial<InboxRow> = {}): InboxRow {
  return {
    anchor: null,
    answer: null,
    author: { id: "session-1", kind: "session" },
    created_at: "2026-09-11T00:00:00Z",
    edited_at: null,
    id: "ask-1",
    issue: { assignee: null, key: "CORE-1", title: "Fix the thing" },
    issue_key: "CORE-1",
    kind: "question",
    multiple: false,
    opened_event_id: 1,
    options: [],
    thread: { edits: [], followers: [], replies: [] },
    priority: null,
    question: "Which approach?",
    state: "open",
    waiting_on: "human",
    urgency: "med",
    ...overrides,
  };
}

/** `source` is what `GET /projects/CORE/architecture-source` answers: the source (the project
 *  opens on Architecture), `undefined` for 404 SOURCE_NOT_FOUND (it opens on Issues, as every
 *  render here expects unless it says otherwise), an `ApiError` for any other failure, or
 *  `"pending"` for a lookup that never settles. */
function renderPage(
  path: string,
  projects = [{ created_at: "2026-09-10T00:00:00Z", key: "CORE", name: "Core", open_asks: 0 }],
  login: string | null = "alice",
  inboxRows: InboxRow[] = [],
  source: ArchitectureSource | ApiError | "pending" | undefined = undefined
) {
  const getInbox = spyOn(api, "getInbox").mockResolvedValue(inboxRows);
  const whoAmI = spyOn(api, "whoAmI");
  if (login === null) {
    whoAmI.mockImplementation(() => Promise.withResolvers<never>().promise);
  } else {
    whoAmI.mockResolvedValue({ kind: "user", login });
  }

  const listProjects = spyOn(api, "listProjects").mockResolvedValue(projects);
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const listProjectArtifacts = spyOn(api, "listProjectArtifacts").mockResolvedValue([]);
  const getArchitectureSource = spyOn(api, "getArchitectureSource");
  if (source === undefined) {
    getArchitectureSource.mockRejectedValue(new ApiError(404, { code: "SOURCE_NOT_FOUND" }));
  } else if (source === "pending") {
    getArchitectureSource.mockImplementation(() => Promise.withResolvers<never>().promise);
  } else if (source instanceof ApiError) {
    getArchitectureSource.mockRejectedValue(source);
  } else {
    getArchitectureSource.mockResolvedValue(source);
  }
  // The Architecture pane's own tree fetch is not under test here; it never settles.
  const getArchitecture = spyOn(api, "getArchitecture").mockImplementation(
    () => Promise.withResolvers<never>().promise
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={[path]}>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <ProjectPage />
          <LocationProbe />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const restore = () => {
    view.unmount();
    getArchitecture.mockRestore();
    getArchitectureSource.mockRestore();
    getInbox.mockRestore();
    getMyState.mockRestore();
    listIssues.mockRestore();
    listProjectArtifacts.mockRestore();
    listProjects.mockRestore();
    whoAmI.mockRestore();
  };
  return {
    getArchitectureSource,
    getInbox,
    getMyState,
    listIssues,
    listProjectArtifacts,
    listProjects,
    restore,
    view,
    whoAmI,
  };
}

const coreSource: ArchitectureSource = {
  branch: "main",
  created_at: "2026-09-10T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  enabled: true,
  last_commit: null,
  last_error: null,
  last_sync_at: null,
  project: "CORE",
  repo: "legion/legion",
};

test("the bare project path opens on Architecture when the project has a source, forwarding nothing", async () => {
  const page = renderPage("/projects/CORE", undefined, "alice", [], coreSource);
  try {
    await screen.findByRole("heading", { name: "Core" });
    expect(screen.getByTestId("location").textContent).toBe("/projects/CORE/architecture");
    expect(screen.getByRole("tab", { name: "Architecture" }).getAttribute("aria-selected")).toBe(
      "true"
    );
    expect(screen.getByRole("tab", { name: "Issues" })).toBeTruthy();
    // The List / Board control belongs to Issues only.
    expect(screen.queryByRole("button", { name: "Board" })).toBeNull();
    // `v` is inert here.
    fireEvent.keyDown(document.body, { key: "v" });
    expect(window.localStorage.getItem("dispatch.project.issue-view:alice")).toBeNull();
  } finally {
    page.restore();
  }
});

test("the bare project path opens on Issues with the filter parameters forwarded when there is no source, and the tab is absent", async () => {
  const page = renderPage("/projects/CORE?label=frontend&q=core");
  try {
    await screen.findByRole("heading", { name: "Core" });
    await waitFor(() =>
      expect(screen.getByTestId("location").textContent).toBe(
        "/projects/CORE/issues?label=frontend&q=core"
      )
    );
    expect(screen.queryByRole("tab", { name: "Architecture" })).toBeNull();
    expect(screen.getByRole("tab", { name: "Issues" }).getAttribute("aria-selected")).toBe("true");
    expect(screen.getByRole("button", { name: "Filters · 2 active" })).toBeTruthy();
  } finally {
    page.restore();
  }
});

test("any other failure of the source lookup is shown, never silently turned into Issues", async () => {
  const page = renderPage("/projects/CORE", undefined, "alice", [], new ApiError(500, {}));
  try {
    expect(await screen.findByRole("alert")).toHaveProperty(
      "textContent",
      expect.stringContaining("Could not load this project's architecture source.")
    );
    expect(screen.getByTestId("location").textContent).toBe("/projects/CORE");
    expect(screen.queryByRole("tab", { name: "Issues" })).toBeNull();
  } finally {
    page.restore();
  }
});

test("/architecture renders nothing while the source lookup is pending, like the bare path, so the tab strip never shows without its active tab", async () => {
  const page = renderPage("/projects/CORE/architecture", undefined, "alice", [], "pending");
  try {
    // Past the projects query (the `Loading project…` placeholder), with the source lookup
    // still open: no header, no tabs. Scoped to this render: bun runs every test file in one
    // document, and a sibling's leftover placeholder would never go away.
    await waitForElementToBeRemoved(() =>
      within(page.view.container).queryByText("Loading project…")
    );
    expect(page.getArchitectureSource).toHaveBeenCalledTimes(1);
    expect(page.view.container.innerHTML).toBe(
      '<output data-testid="location">/projects/CORE/architecture</output>'
    );
  } finally {
    page.restore();
  }
});

test("renders header, tabs, and the Issues panel; /documents selects Documents", async () => {
  const page = renderPage("/projects/CORE");

  try {
    await screen.findByRole("heading", { name: "Core" });
    expect(screen.getByText("CORE", { exact: true })).toBeTruthy();
    expect(screen.getByRole("tab", { name: "Issues" }).getAttribute("aria-selected")).toBe("true");
    expect(screen.getByRole("tabpanel", { name: "Issues" })).toBeTruthy();

    fireEvent.click(screen.getByRole("tab", { name: "Documents" }));
    expect(screen.getByRole("tab", { name: "Documents" }).getAttribute("aria-selected")).toBe(
      "true"
    );
    expect(screen.getByRole("tabpanel", { name: "Documents" })).toBeTruthy();
  } finally {
    page.restore();
  }

  const documents = renderPage("/projects/CORE/documents");
  try {
    await screen.findByRole("heading", { name: "Core" });
    expect(screen.getByRole("tab", { name: "Documents" }).getAttribute("aria-selected")).toBe(
      "true"
    );
  } finally {
    documents.restore();
  }
});

test("shows a compact blocker link only when asks await the viewer", async () => {
  const waiting = renderPage("/projects/CORE", undefined, "alice", [inboxRow()]);
  try {
    await screen.findByRole("heading", { name: "Core" });
    expect(screen.getByRole("link", { name: "Blocked on you · 1" }).getAttribute("href")).toBe("/");
    expect(screen.queryByText(/oldest/)).toBeNull();
  } finally {
    waiting.restore();
  }

  const clear = renderPage("/projects/CORE", undefined, "alice", [
    inboxRow({
      last_reply: { author: { id: "alice", kind: "user" }, created_at: "2026-09-11T01:00:00Z" },

      waiting_on: "agent",
    }),
  ]);
  try {
    await screen.findByRole("heading", { name: "Core" });
    expect(screen.queryByRole("link", { name: /Blocked on you/ })).toBeNull();
  } finally {
    clear.restore();
  }
});

test("unknown project shows the not-found view", async () => {
  const page = renderPage("/projects/MISSING", []);

  try {
    expect(await screen.findByRole("heading", { name: "Page not found" })).toBeTruthy();
  } finally {
    page.restore();
  }
});

test("keeps the issue view preference separate for each signed-in user", async () => {
  const aliceStorageKey = "dispatch.project.issue-view:alice";
  const bobStorageKey = "dispatch.project.issue-view:bob";
  window.localStorage.setItem(aliceStorageKey, "board");
  window.localStorage.setItem(bobStorageKey, "list");

  const alice = renderPage("/projects/CORE", undefined, "alice");
  try {
    await screen.findByRole("heading", { name: "Core" });
    expect(screen.getByRole("button", { name: "Board" }).getAttribute("aria-pressed")).toBe("true");
  } finally {
    alice.restore();
  }

  const bob = renderPage("/projects/CORE", undefined, "bob");
  try {
    await screen.findByRole("heading", { name: "Core" });
    expect(screen.getByRole("button", { name: "List" }).getAttribute("aria-pressed")).toBe("true");
  } finally {
    bob.restore();
    window.localStorage.removeItem(aliceStorageKey);
    window.localStorage.removeItem(bobStorageKey);
  }
});

test("the Icebox & Done toggle lives in Board view only and persists per signed-in user", async () => {
  const viewKey = "dispatch.project.issue-view:alice";
  const edgesKey = "dispatch.project.board-edges:alice";
  window.localStorage.setItem(viewKey, "board");

  const first = renderPage("/projects/CORE", undefined, "alice");
  try {
    await screen.findByRole("heading", { name: "Core" });
    const toggle = await screen.findByRole("button", { name: "Show Icebox & Done" });
    expect(toggle.getAttribute("aria-pressed")).toBe("false");
    expect(toggle.classList.contains("min-h-11")).toBe(true);
    fireEvent.click(toggle);
    expect(
      screen.getByRole("button", { name: "Hide Icebox & Done" }).getAttribute("aria-pressed")
    ).toBe("true");
    expect(window.localStorage.getItem(edgesKey)).toBe("shown");

    fireEvent.click(screen.getByRole("button", { name: "List" }));
    expect(screen.queryByRole("button", { name: /Icebox & Done/ })).toBeNull();
  } finally {
    first.restore();
  }

  window.localStorage.setItem(viewKey, "board");
  const second = renderPage("/projects/CORE", undefined, "alice");
  try {
    await screen.findByRole("heading", { name: "Core" });
    expect(
      (await screen.findByRole("button", { name: "Hide Icebox & Done" })).getAttribute(
        "aria-pressed"
      )
    ).toBe("true");
  } finally {
    second.restore();
  }

  const bob = renderPage("/projects/CORE", undefined, "bob");
  try {
    window.localStorage.setItem("dispatch.project.issue-view:bob", "board");
    await screen.findByRole("heading", { name: "Core" });
    fireEvent.click(screen.getByRole("button", { name: "Board" }));
    expect(
      (await screen.findByRole("button", { name: "Show Icebox & Done" })).getAttribute(
        "aria-pressed"
      )
    ).toBe("false");
  } finally {
    bob.restore();
    window.localStorage.removeItem(viewKey);
    window.localStorage.removeItem(edgesKey);
    window.localStorage.removeItem("dispatch.project.issue-view:bob");
    window.localStorage.removeItem("dispatch.project.board-edges:bob");
  }
});

test("v toggles List and Board on the issues tab and writes the preference; it is inert on Documents", async () => {
  const viewKey = "dispatch.project.issue-view:alice";
  const page = renderPage("/projects/CORE");
  try {
    await screen.findByRole("heading", { name: "Core" });
    expect(screen.getByRole("button", { name: "List" }).getAttribute("aria-pressed")).toBe("true");

    fireEvent.keyDown(document.body, { key: "v" });
    expect(screen.getByRole("button", { name: "Board" }).getAttribute("aria-pressed")).toBe("true");
    expect(window.localStorage.getItem(viewKey)).toBe("board");

    fireEvent.keyDown(document.body, { key: "v" });
    expect(screen.getByRole("button", { name: "List" }).getAttribute("aria-pressed")).toBe("true");
    expect(window.localStorage.getItem(viewKey)).toBe("list");
  } finally {
    page.restore();
    window.localStorage.removeItem(viewKey);
  }

  const documents = renderPage("/projects/CORE/documents");
  try {
    await screen.findByRole("heading", { name: "Core" });
    fireEvent.keyDown(document.body, { key: "v" });
    expect(screen.queryByRole("button", { name: "Board" })).toBeNull();
    expect(window.localStorage.getItem(viewKey)).toBeNull();
  } finally {
    documents.restore();
  }
});

test("keeps an unpersisted List or Board choice while identity is pending", async () => {
  const pendingKey = "dispatch.project.issue-view:undefined";
  window.localStorage.removeItem(pendingKey);
  const page = renderPage("/projects/CORE", undefined, null);

  try {
    await screen.findByRole("heading", { name: "Core" });
    fireEvent.click(screen.getByRole("button", { name: "Board" }));
    expect(screen.getByRole("button", { name: "Board" }).getAttribute("aria-pressed")).toBe("true");
    expect(window.localStorage.getItem(pendingKey)).toBeNull();
  } finally {
    page.restore();
    window.localStorage.removeItem(pendingKey);
  }
});

test("one filter strip serves both views: chips survive the toggle and only List folds in Status", async () => {
  const viewKey = "dispatch.project.issue-view:alice";
  const page = renderPage("/projects/CORE?label=frontend&q=core");
  try {
    await screen.findByRole("heading", { name: "Core" });
    // One strip instance on the List, holding the URL-backed chips.
    expect(screen.getAllByRole("button", { name: /Filters · \d+ active/ })).toHaveLength(1);
    expect(screen.getByRole("button", { name: "Filters · 2 active" })).toBeTruthy();
    // The List folds the URL's Status filter into the strip's count and chips.
    fireEvent.click(await screen.findByRole("button", { name: "Status" }));
    fireEvent.click(screen.getByRole("option", { name: "Todo" }));
    expect(screen.getByRole("button", { name: "Filters · 3 active" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Remove Status: Todo filter" })).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Board" }));
    // Still one strip; the URL chips survive the toggle; the Board has no Status picker or chip.
    expect(screen.getAllByRole("button", { name: /Filters · \d+ active/ })).toHaveLength(1);
    expect(screen.getByRole("button", { name: "Filters · 2 active" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Remove Label: frontend filter" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Remove Search: core filter" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^Status( · \d+)?$/ })).toBeNull();
    expect(screen.queryByRole("button", { name: "Remove Status: Todo filter" })).toBeNull();

    // Back on the List the held Status filter counts again.
    fireEvent.click(screen.getByRole("button", { name: "List" }));
    expect(screen.getByRole("button", { name: "Filters · 3 active" })).toBeTruthy();
  } finally {
    page.restore();
    window.localStorage.removeItem(viewKey);
    window.localStorage.removeItem("dispatch.project.issue-filters:alice");
  }
});

test("typing v in the strip's search input never toggles the view", async () => {
  const viewKey = "dispatch.project.issue-view:alice";
  const page = renderPage("/projects/CORE?label=frontend");
  try {
    await screen.findByRole("heading", { name: "Core" });
    const search = await screen.findByRole("searchbox", { name: "Search issues" });
    fireEvent.keyDown(search, { key: "v" });
    expect(screen.getByRole("button", { name: "List" }).getAttribute("aria-pressed")).toBe("true");
    expect(window.localStorage.getItem(viewKey)).toBeNull();
    // Outside an editable target the same key still toggles.
    fireEvent.keyDown(document.body, { key: "v" });
    expect(screen.getByRole("button", { name: "Board" }).getAttribute("aria-pressed")).toBe("true");
  } finally {
    page.restore();
    window.localStorage.removeItem(viewKey);
  }
});
