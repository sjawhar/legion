import { expect, spyOn, test } from "bun:test";
import { SEARCH_QUERY_MAX } from "@legion/contracts/dispatch-tools";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { type ReactNode, useState } from "react";
import { MemoryRouter, useLocation, useNavigate } from "react-router-dom";

import { api } from "../../api/client";
import type { SearchResponse, SearchResult, SearchResultsPage } from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
import { appKeymap } from "../shell/keymap";
import { SearchButton } from "./SearchButton";
import { type PaletteMode, SearchPalette } from "./SearchPalette";
import { optionId } from "./search-model";

const results: SearchResult[] = [
  {
    artifact: { name: "spec.md", slug: "spec" },
    href: "/issues/LEGION-2/spec?q=astrolabe",
    id: "document-2",
    owner: { key: "LEGION-2", kind: "issue", status: "done", title: "Navigation instruments" },
    kind: "document",
    rank: 3,
    snippet: "The <mark>astrolabe</mark> measures altitude.",
  },
  {
    href: "/issues/LEGION-3",
    id: "LEGION-3",
    owner: { key: "LEGION-3", kind: "issue", status: "todo", title: "Instrument research" },
    kind: "issue",
    rank: 2,
    snippet: "Research <mark>astrolabe</mark> history.",
  },
  {
    href: "/issues/LEGION-2/comments/comment-2",
    id: "comment-2",
    owner: { key: "LEGION-2", kind: "issue", status: "done", title: "Navigation instruments" },
    kind: "comment",
    rank: 1,
    snippet: "Discuss the <mark>astrolabe</mark> diagram.",
  },
];

/** The server's answer when these hits are every match: one page holding the whole list. */
function searchAnswer(hits: SearchResult[]): SearchResponse & SearchResultsPage {
  return {
    limit: 20,
    offset: 0,
    reachable: hits.length,
    results: hits,
    took_ms: 1,
    total: hits.length,
  };
}

function CurrentRoute(): ReactNode {
  const location = useLocation();
  return <output data-testid="current-route">{`${location.pathname}${location.search}`}</output>;
}

function RouteChanger(): ReactNode {
  const navigate = useNavigate();
  return (
    <button onClick={() => navigate("/issues/LEGION-9")} type="button">
      Navigate elsewhere
    </button>
  );
}

function renderPalette(onClose = () => {}): { queryClient: QueryClient; unmount: () => void } {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <CurrentRoute />
          <SearchPalette mode="all" onClose={onClose} />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { queryClient, unmount: view.unmount };
}

async function searchFor(query: string): Promise<HTMLInputElement> {
  const input = screen.getByRole<HTMLInputElement>("combobox", { name: "Search" });
  fireEvent.change(input, { target: { value: query } });
  await waitFor(() => expect(screen.getAllByRole("option").length).toBeGreaterThan(0));
  return input;
}

test("renders grouped results with marked snippets and dims a done issue", async () => {
  const search = spyOn(api, "search").mockResolvedValue(searchAnswer(results));
  const view = renderPalette();

  try {
    await searchFor("astrolabe");

    // The owner rows name their groups and are `aria-hidden`, so a reader hears each owner
    // once; they are found by their label attribute rather than by role.
    const doneGroup = document.querySelector('[aria-label="LEGION-2: Navigation instruments"]');
    expect(screen.getAllByRole("group")).toHaveLength(2);
    expect(screen.getAllByRole("option")).toHaveLength(3);
    expect(screen.getAllByText("astrolabe", { selector: "mark" })).not.toHaveLength(0);
    expect(doneGroup?.getAttribute("data-status")).toBe("done");
    expect(screen.queryByText(/^Showing the best/)).toBeNull();
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

test("says how many matches a query has when the page holds fewer", async () => {
  const search = spyOn(api, "search").mockResolvedValue({
    ...searchAnswer(results),
    reachable: 300,
    total: 1328,
  });
  const view = renderPalette();

  try {
    await searchFor("astrolabe");
    expect(screen.getAllByRole("option")).toHaveLength(3);
    expect(screen.getByText("Showing the best 3 of 1328 matches")).toBeDefined();
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

test("renders document-owned results under the document and opens their discussion", async () => {
  const search = spyOn(api, "search").mockResolvedValue(
    searchAnswer([
      {
        artifact: { name: "Navigation design", slug: "navigation-design" },
        href: "/projects/CORE/documents/navigation-design?comment=comment-2",
        id: "comment-2",
        kind: "comment",
        owner: {
          artifact_id: "document-2",
          kind: "document",
          name: "Navigation design",
          project: "CORE",
          slug: "navigation-design",
        },
        rank: 1,
        snippet: "Discuss the <mark>astrolabe</mark> diagram.",
      },
    ])
  );
  const view = renderPalette();

  try {
    await searchFor("astrolabe");

    const documentGroup = document.querySelector('[aria-label="CORE: Navigation design"]');
    expect(documentGroup).not.toBeNull();
    expect(screen.getByRole("group", { name: "CORE: Navigation design" })).toBeTruthy();
    fireEvent.click(screen.getByRole("option"));
    expect(screen.getByTestId("current-route").textContent).toBe(
      "/projects/CORE/documents/navigation-design?comment=comment-2"
    );
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

test("does not query below two characters", () => {
  const search = spyOn(api, "search").mockResolvedValue(searchAnswer([]));
  const view = renderPalette();

  try {
    fireEvent.change(screen.getByRole("combobox", { name: "Search" }), { target: { value: "a" } });

    expect(search).not.toHaveBeenCalled();
    expect(screen.getByText("Type at least 2 characters")).not.toBeNull();
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

test("names the limit instead of querying over it, and queries at it", async () => {
  const search = spyOn(api, "search").mockResolvedValue(searchAnswer([]));
  const view = renderPalette();

  try {
    const input = screen.getByRole("combobox", { name: "Search" });
    fireEvent.change(input, { target: { value: "x".repeat(SEARCH_QUERY_MAX + 1) } });

    expect(
      screen.getByText(
        `Search with a short phrase: ${SEARCH_QUERY_MAX + 1} characters is over the ${SEARCH_QUERY_MAX}-character limit`
      )
    ).not.toBeNull();
    // Past the palette's 150 ms debounce, so a request would have gone out by now.
    const settled = Promise.withResolvers<void>();
    setTimeout(settled.resolve, 300);
    await act(() => settled.promise);
    expect(search).not.toHaveBeenCalled();

    fireEvent.change(input, { target: { value: "x".repeat(SEARCH_QUERY_MAX) } });
    await waitFor(() => expect(search).toHaveBeenCalledWith("x".repeat(SEARCH_QUERY_MAX)));
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

test("arrow keys move aria-activedescendant and Enter navigates to the active href", async () => {
  const search = spyOn(api, "search").mockResolvedValue(searchAnswer(results));
  const view = renderPalette();

  try {
    const input = await searchFor("astrolabe");
    const options = screen.getAllByRole("option");
    expect(input.getAttribute("aria-activedescendant")).toBe(options[0]?.id);

    fireEvent.keyDown(input, { key: "ArrowDown" });
    expect(input.getAttribute("aria-activedescendant")).toBe(options[1]?.id);

    fireEvent.keyDown(input, { key: "ArrowUp" });
    fireEvent.keyDown(input, { key: "Enter" });

    expect(screen.getByTestId("current-route").textContent).toBe(
      "/issues/LEGION-2/spec?q=astrolabe"
    );
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

function issueHit(key: string): SearchResult {
  return {
    href: `/issues/${key}`,
    id: key,
    kind: "issue",
    owner: { key, kind: "issue", status: "todo", title: `Instrument ${key}` },
    rank: 1,
    snippet: `The <mark>astrolabe</mark> of ${key}.`,
  };
}

test("a refetch that re-ranks the hits leaves the highlight on the hit the reader sees", async () => {
  const [first, second] = [issueHit("LEGION-3"), issueHit("LEGION-4")];
  const search = spyOn(api, "search").mockResolvedValue(searchAnswer([first, second]));
  const view = renderPalette();

  try {
    const input = await searchFor("astrolabe");
    const shown = input.getAttribute("aria-activedescendant");
    expect(shown).toBe(screen.getAllByRole("option")[0]?.id ?? "");

    // A focus refetch, or the event stream's reconnect, answers the same query re-ranked.
    search.mockResolvedValue(searchAnswer([second, first]));
    await view.queryClient.refetchQueries();
    await waitFor(() => expect(screen.getAllByRole("option")[0]?.id).not.toBe(shown));
    expect(input.getAttribute("aria-activedescendant")).toBe(shown);
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

test("a highlighted hit that drops out hands the highlight to the head, which keeps it", async () => {
  const [first, second] = [issueHit("LEGION-3"), issueHit("LEGION-4")];
  const search = spyOn(api, "search").mockResolvedValue(searchAnswer([first, second]));
  const view = renderPalette();

  try {
    const input = await searchFor("astrolabe");
    fireEvent.keyDown(input, { key: "ArrowDown" });
    const [head, arrowed] = screen.getAllByRole("option").map((option) => option.id);
    expect(input.getAttribute("aria-activedescendant")).toBe(arrowed ?? "");

    search.mockResolvedValue(searchAnswer([first]));
    await view.queryClient.refetchQueries();
    await waitFor(() => expect(screen.getAllByRole("option")).toHaveLength(1));
    expect(input.getAttribute("aria-activedescendant")).toBe(head ?? "");

    // The row coming back does not take the highlight back from the one the reader now sees.
    search.mockResolvedValue(searchAnswer([first, second]));
    await view.queryClient.refetchQueries();
    await waitFor(() => expect(screen.getAllByRole("option")).toHaveLength(2));
    expect(input.getAttribute("aria-activedescendant")).toBe(head ?? "");
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

test("a background refresh that fails keeps the hits the query has, and the highlight on its hit", async () => {
  const [first, second] = [issueHit("LEGION-3"), issueHit("LEGION-4")];
  const search = spyOn(api, "search").mockResolvedValue(searchAnswer([first, second]));
  const view = renderPalette();

  try {
    const input = await searchFor("astrolabe");
    fireEvent.keyDown(input, { key: "ArrowDown" });
    const arrowed = input.getAttribute("aria-activedescendant");
    expect(arrowed).toBe(screen.getAllByRole("option")[1]?.id ?? "");

    // The tab-return refresh fails; TanStack keeps the answer it already has.
    search.mockRejectedValue(new Error("503"));
    await view.queryClient.refetchQueries();
    await waitFor(() =>
      expect(view.queryClient.getQueryState(["search", "astrolabe"])?.status).toBe("error")
    );
    expect(screen.getAllByRole("option")).toHaveLength(2);
    expect(screen.queryByText("Search failed.")).toBeNull();
    expect(input.getAttribute("aria-activedescendant")).toBe(arrowed);
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

// The shell's part in a chosen action: the palette closes, and the row runs once it has.
function PaletteHost(): ReactNode {
  const [mode, setMode] = useState<PaletteMode | null>("all");
  return <SearchPalette mode={mode} onClose={() => setMode(null)} />;
}

function renderPaletteHost(): { queryClient: QueryClient; unmount: () => void } {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <CurrentRoute />
          <PaletteHost />
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { queryClient, unmount: view.unmount };
}

const CLOSE_ROW = "search-option-action-global-close";

// `Close issue` stands for any row whose label holds a search word somewhere past its start.
function registerClose(ran: string[]): () => void {
  return appKeymap.register("global", [
    { id: "close", keys: [], label: "Close issue", run: () => ran.push("close") },
  ]);
}

function optionIds(): string[] {
  return screen.queryAllByRole("option").map((option) => option.id);
}

test("a query found inside an action's label lists the action below the hits, and Enter opens the first hit", async () => {
  const ran: string[] = [];
  const unregister = registerClose(ran);
  const hit = issueHit("LEGION-3");
  const search = spyOn(api, "search").mockResolvedValue(searchAnswer([hit]));
  const view = renderPaletteHost();

  try {
    const input = screen.getByRole<HTMLInputElement>("combobox", { name: "Search" });
    fireEvent.change(input, { target: { value: "issue" } });
    await waitFor(() => expect(optionIds()).toEqual([optionId(hit), CLOSE_ROW]));
    expect(input.getAttribute("aria-activedescendant")).toBe(optionId(hit));

    fireEvent.keyDown(input, { key: "Enter" });
    expect(screen.getByTestId("current-route").textContent).toBe("/issues/LEGION-3");
    expect(ran).toEqual([]);
  } finally {
    search.mockRestore();
    unregister();
    view.unmount();
    view.queryClient.clear();
  }
});

test("while the search is in flight no action found inside its label is highlighted, so Enter runs nothing", async () => {
  const ran: string[] = [];
  const unregister = registerClose(ran);
  const hit = issueHit("LEGION-3");
  const answer = Promise.withResolvers<SearchResponse>();
  const search = spyOn(api, "search").mockReturnValue(answer.promise);
  const view = renderPaletteHost();

  try {
    const input = screen.getByRole<HTMLInputElement>("combobox", { name: "Search" });
    fireEvent.change(input, { target: { value: "issue" } });
    await waitFor(() => expect(search).toHaveBeenCalled());
    expect(optionIds()).toEqual([CLOSE_ROW]);
    expect(input.getAttribute("aria-activedescendant")).toBeNull();
    fireEvent.keyDown(input, { key: "Enter" });
    expect(screen.getByRole("dialog", { name: "Search" })).toBeTruthy();
    expect(ran).toEqual([]);

    // The hits arriving take the head; the action row was never adopted while they were out.
    await act(async () => answer.resolve(searchAnswer([hit])));
    await waitFor(() => expect(input.getAttribute("aria-activedescendant")).toBe(optionId(hit)));
    expect(optionIds()).toEqual([optionId(hit), CLOSE_ROW]);
  } finally {
    search.mockRestore();
    unregister();
    view.unmount();
    view.queryClient.clear();
  }
});

test("a search that finds nothing still leaves an action found inside its label unhighlighted", async () => {
  const ran: string[] = [];
  const unregister = registerClose(ran);
  const search = spyOn(api, "search").mockResolvedValue(searchAnswer([]));
  const view = renderPaletteHost();

  try {
    const input = screen.getByRole<HTMLInputElement>("combobox", { name: "Search" });
    fireEvent.change(input, { target: { value: "issue" } });
    await screen.findByText('No results for "issue"');
    expect(optionIds()).toEqual([CLOSE_ROW]);
    expect(input.getAttribute("aria-activedescendant")).toBeNull();
    fireEvent.keyDown(input, { key: "Enter" });
    expect(screen.getByRole("dialog", { name: "Search" })).toBeTruthy();
    expect(ran).toEqual([]);
  } finally {
    search.mockRestore();
    unregister();
    view.unmount();
    view.queryClient.clear();
  }
});

test("an arrow pressed before the search answers reaches no action found inside its label", async () => {
  const ran: string[] = [];
  const unregister = registerClose(ran);
  const hit = issueHit("LEGION-3");
  const answer = Promise.withResolvers<SearchResponse>();
  const search = spyOn(api, "search").mockReturnValue(answer.promise);
  const view = renderPaletteHost();

  try {
    const input = screen.getByRole<HTMLInputElement>("combobox", { name: "Search" });
    fireEvent.change(input, { target: { value: "issue" } });
    // Down and Enter in one burst, inside the debounce and again once the request is out.
    fireEvent.keyDown(input, { key: "ArrowDown" });
    fireEvent.keyDown(input, { key: "Enter" });
    expect(input.getAttribute("aria-activedescendant")).toBeNull();
    await waitFor(() => expect(search).toHaveBeenCalled());
    fireEvent.keyDown(input, { key: "ArrowDown" });
    fireEvent.keyDown(input, { key: "ArrowUp" });
    expect(input.getAttribute("aria-activedescendant")).toBeNull();
    fireEvent.keyDown(input, { key: "Enter" });
    expect(ran).toEqual([]);

    // The hits arrive above it and take the head; Enter opens the first one.
    await act(async () => answer.resolve(searchAnswer([hit])));
    await waitFor(() => expect(optionIds()).toEqual([optionId(hit), CLOSE_ROW]));
    expect(input.getAttribute("aria-activedescendant")).toBe(optionId(hit));
    fireEvent.keyDown(input, { key: "Enter" });
    expect(screen.getByTestId("current-route").textContent).toBe("/issues/LEGION-3");
    expect(ran).toEqual([]);
  } finally {
    search.mockRestore();
    unregister();
    view.unmount();
    view.queryClient.clear();
  }
});

test("once the search has answered with no hit, Down takes the first action found inside its label", async () => {
  const ran: string[] = [];
  const unregister = registerClose(ran);
  const search = spyOn(api, "search").mockResolvedValue(searchAnswer([]));
  const view = renderPaletteHost();

  try {
    const input = screen.getByRole<HTMLInputElement>("combobox", { name: "Search" });
    fireEvent.change(input, { target: { value: "issue" } });
    await screen.findByText('No results for "issue"');
    fireEvent.keyDown(input, { key: "ArrowDown" });
    expect(input.getAttribute("aria-activedescendant")).toBe(CLOSE_ROW);
    fireEvent.keyDown(input, { key: "Enter" });
    await waitFor(() => expect(ran).toEqual(["close"]));
  } finally {
    search.mockRestore();
    unregister();
    view.unmount();
    view.queryClient.clear();
  }
});

test("while the search is out the arrows walk only the actions the query starts, which keep the highlight as hits arrive", async () => {
  const ran: string[] = [];
  const unregister = appKeymap.register("global", [
    { id: "close", keys: [], label: "Close issue", run: () => ran.push("close") },
    { id: "report", keys: [], label: "Issue the report", run: () => ran.push("report") },
    { id: "triage", keys: [], label: "Issue triage", run: () => ran.push("triage") },
  ]);
  const hit = issueHit("LEGION-3");
  const answer = Promise.withResolvers<SearchResponse>();
  const search = spyOn(api, "search").mockReturnValue(answer.promise);
  const view = renderPaletteHost();
  const [report, triage] = [
    "search-option-action-global-report",
    "search-option-action-global-triage",
  ];

  try {
    const input = screen.getByRole<HTMLInputElement>("combobox", { name: "Search" });
    fireEvent.change(input, { target: { value: "issue" } });
    await waitFor(() => expect(search).toHaveBeenCalled());
    expect(optionIds()).toEqual([report, triage, CLOSE_ROW]);
    expect(input.getAttribute("aria-activedescendant")).toBe(report);
    fireEvent.keyDown(input, { key: "ArrowDown" });
    expect(input.getAttribute("aria-activedescendant")).toBe(triage);
    fireEvent.keyDown(input, { key: "ArrowDown" });
    expect(input.getAttribute("aria-activedescendant")).toBe(report);
    fireEvent.keyDown(input, { key: "ArrowUp" });
    expect(input.getAttribute("aria-activedescendant")).toBe(triage);

    await act(async () => answer.resolve(searchAnswer([hit])));
    await waitFor(() => expect(optionIds()).toEqual([report, triage, optionId(hit), CLOSE_ROW]));
    expect(input.getAttribute("aria-activedescendant")).toBe(triage);
    fireEvent.keyDown(input, { key: "Enter" });
    await waitFor(() => expect(ran).toEqual(["triage"]));
  } finally {
    search.mockRestore();
    unregister();
    view.unmount();
    view.queryClient.clear();
  }
});

test("a refetch behind a stale cached answer is still out, so Down and Enter reach no action found inside its label", async () => {
  const ran: string[] = [];
  const unregister = registerClose(ran);
  const hit = issueHit("LEGION-3");
  const answer = Promise.withResolvers<SearchResponse>();
  const search = spyOn(api, "search").mockReturnValue(answer.promise);
  const view = renderPaletteHost();
  // The reader searched `issue` earlier and found nothing; that answer is stale now.
  view.queryClient.setQueryData(["search", "issue"], searchAnswer([]));

  try {
    const input = screen.getByRole<HTMLInputElement>("combobox", { name: "Search" });
    fireEvent.change(input, { target: { value: "issue" } });
    await waitFor(() => expect(search).toHaveBeenCalled());
    expect(optionIds()).toEqual([CLOSE_ROW]);
    fireEvent.keyDown(input, { key: "ArrowDown" });
    expect(input.getAttribute("aria-activedescendant")).toBeNull();
    fireEvent.keyDown(input, { key: "Enter" });
    expect(ran).toEqual([]);

    await act(async () => answer.resolve(searchAnswer([hit])));
    await waitFor(() => expect(optionIds()).toEqual([optionId(hit), CLOSE_ROW]));
    expect(input.getAttribute("aria-activedescendant")).toBe(optionId(hit));
    fireEvent.keyDown(input, { key: "Enter" });
    expect(screen.getByTestId("current-route").textContent).toBe("/issues/LEGION-3");
    expect(ran).toEqual([]);
  } finally {
    search.mockRestore();
    unregister();
    view.unmount();
    view.queryClient.clear();
  }
});

test("once the search has failed, Down takes the first action found inside its label", async () => {
  const ran: string[] = [];
  const unregister = registerClose(ran);
  const search = spyOn(api, "search").mockRejectedValue(new Error("503"));
  const view = renderPaletteHost();

  try {
    const input = screen.getByRole<HTMLInputElement>("combobox", { name: "Search" });
    fireEvent.change(input, { target: { value: "issue" } });
    expect((await screen.findByRole("alert")).textContent).toContain("Search failed.");
    fireEvent.keyDown(input, { key: "ArrowDown" });
    expect(input.getAttribute("aria-activedescendant")).toBe(CLOSE_ROW);
    fireEvent.keyDown(input, { key: "Enter" });
    await waitFor(() => expect(ran).toEqual(["close"]));
  } finally {
    search.mockRestore();
    unregister();
    view.unmount();
    view.queryClient.clear();
  }
});

test("a background refetch keeps the actions the query starts and the hits walkable, and a highlight already among the More actions moving", async () => {
  const ran: string[] = [];
  const unregister = appKeymap.register("global", [
    { id: "close", keys: [], label: "Close issue", run: () => ran.push("close") },
    { id: "report", keys: [], label: "Issue the report", run: () => ran.push("report") },
  ]);
  const hit = issueHit("LEGION-3");
  const search = spyOn(api, "search").mockResolvedValue(searchAnswer([hit]));
  const view = renderPaletteHost();
  const report = "search-option-action-global-report";
  const refetchHeld = async (calls: number) => {
    const answer = Promise.withResolvers<SearchResponse>();
    search.mockReturnValue(answer.promise);
    act(() => {
      void view.queryClient.refetchQueries();
    });
    await waitFor(() => expect(search).toHaveBeenCalledTimes(calls));
    return answer;
  };

  try {
    const input = screen.getByRole<HTMLInputElement>("combobox", { name: "Search" });
    fireEvent.change(input, { target: { value: "issue" } });
    await waitFor(() => expect(optionIds()).toEqual([report, optionId(hit), CLOSE_ROW]));
    expect(input.getAttribute("aria-activedescendant")).toBe(report);

    // A focus or staleTime refetch is out with the hits still shown: the arrows walk the
    // leading row and the hits, and stop short of the More actions.
    const first = await refetchHeld(2);
    fireEvent.keyDown(input, { key: "ArrowDown" });
    expect(input.getAttribute("aria-activedescendant")).toBe(optionId(hit));
    fireEvent.keyDown(input, { key: "ArrowDown" });
    expect(input.getAttribute("aria-activedescendant")).toBe(report);
    fireEvent.keyDown(input, { key: "ArrowUp" });
    expect(input.getAttribute("aria-activedescendant")).toBe(optionId(hit));
    await act(async () => first.resolve(searchAnswer([hit])));
    await waitFor(() => expect(view.queryClient.isFetching()).toBe(0));
    fireEvent.keyDown(input, { key: "ArrowDown" });
    expect(input.getAttribute("aria-activedescendant")).toBe(CLOSE_ROW);

    // Moved there after an answer, a highlight among the More actions keeps moving over the
    // whole list through the next refetch.
    const second = await refetchHeld(3);
    fireEvent.keyDown(input, { key: "ArrowDown" });
    expect(input.getAttribute("aria-activedescendant")).toBe(report);
    await act(async () => second.resolve(searchAnswer([hit])));
    expect(ran).toEqual([]);
  } finally {
    search.mockRestore();
    unregister();
    view.unmount();
    view.queryClient.clear();
  }
});

test("a query that starts an action's label lists it above the hits, and Enter runs it", async () => {
  const ran: string[] = [];
  const unregister = registerClose(ran);
  const hit = issueHit("LEGION-3");
  const search = spyOn(api, "search").mockResolvedValue(searchAnswer([hit]));
  const view = renderPaletteHost();

  try {
    const input = screen.getByRole<HTMLInputElement>("combobox", { name: "Search" });
    fireEvent.change(input, { target: { value: "clo" } });
    await waitFor(() => expect(optionIds()).toEqual([CLOSE_ROW, optionId(hit)]));
    expect(input.getAttribute("aria-activedescendant")).toBe(CLOSE_ROW);

    fireEvent.keyDown(input, { key: "Enter" });
    await waitFor(() => expect(ran).toEqual(["close"]));
    expect(screen.getByTestId("current-route").textContent).toBe("/");
  } finally {
    search.mockRestore();
    unregister();
    view.unmount();
    view.queryClient.clear();
  }
});

test("Escape calls onClose", () => {
  let closed = 0;
  const view = renderPalette(() => {
    closed += 1;
  });

  try {
    fireEvent.keyDown(screen.getByRole("dialog", { name: "Search" }), { key: "Escape" });
    expect(closed).toBe(1);
  } finally {
    view.unmount();
    view.queryClient.clear();
  }
});

test("Escape closes the palette even when pressed before focus has moved into it", () => {
  let closed = 0;
  const view = renderPalette(() => {
    closed += 1;
  });

  try {
    // The dialog moves focus inside itself one animation frame after opening; a key pressed
    // in that frame lands on whatever was focused before (the document editor, the body).
    fireEvent.keyDown(document.body, { key: "Escape" });
    expect(closed).toBe(1);
  } finally {
    view.unmount();
    view.queryClient.clear();
  }
});

test("closes the palette when navigation changes", async () => {
  let closed = 0;
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <RouteChanger />
        <SearchPalette
          mode="all"
          onClose={() => {
            closed += 1;
          }}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    fireEvent.click(screen.getByRole("button", { name: "Navigate elsewhere" }));
    await waitFor(() => expect(closed).toBe(1));
  } finally {
    view.unmount();
    queryClient.clear();
  }
});

test("shows a retryable failure when the search request fails", async () => {
  const search = spyOn(api, "search").mockRejectedValue(new Error("500"));
  const view = renderPalette();

  try {
    fireEvent.change(screen.getByRole("combobox", { name: "Search" }), {
      target: { value: "astrolabe" },
    });
    expect((await screen.findByRole("alert")).textContent).toContain("Search failed.");
    expect(screen.getByRole("button", { name: "Retry" })).not.toBeNull();
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

test("SearchButton announces its shortcut and opens the palette", () => {
  let opened = 0;
  const view = render(<SearchButton onOpen={() => (opened += 1)} />);

  try {
    const button = screen.getByRole("button", { name: /search/i });
    expect(button.getAttribute("aria-keyshortcuts")).toBe("Control+K Meta+K");
    fireEvent.click(button);
    expect(opened).toBe(1);
  } finally {
    view.unmount();
  }
});
