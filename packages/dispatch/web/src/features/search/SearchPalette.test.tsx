import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation, useNavigate } from "react-router-dom";

import { api } from "../../api/client";
import type { SearchResult } from "../../api/types";
import { SearchButton } from "./SearchButton";
import { SearchPalette } from "./SearchPalette";
import { useSearchShortcut } from "./useSearchShortcut";

const results: SearchResult[] = [
  {
    artifact: { name: "spec.md", slug: "spec" },
    href: "/issues/LEGION-2/spec?q=astrolabe",
    id: "document-2",
    issue: { key: "LEGION-2", status: "done", title: "Navigation instruments" },
    kind: "document",
    rank: 3,
    snippet: "The <mark>astrolabe</mark> measures altitude.",
  },
  {
    href: "/issues/LEGION-3",
    id: "LEGION-3",
    issue: { key: "LEGION-3", status: "todo", title: "Instrument research" },
    kind: "issue",
    rank: 2,
    snippet: "Research <mark>astrolabe</mark> history.",
  },
  {
    href: "/issues/LEGION-2/comments/comment-2",
    id: "comment-2",
    issue: { key: "LEGION-2", status: "done", title: "Navigation instruments" },
    kind: "comment",
    rank: 1,
    snippet: "Discuss the <mark>astrolabe</mark> diagram.",
  },
];

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
        <CurrentRoute />
        <SearchPalette onClose={onClose} open />
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
  const search = spyOn(api, "search").mockResolvedValue({ results, took_ms: 1 });
  const view = renderPalette();

  try {
    await searchFor("astrolabe");

    const doneGroup = screen
      .getAllByRole("presentation")
      .find((group) => group.getAttribute("aria-label") === "LEGION-2: Navigation instruments");
    expect(screen.getAllByRole("presentation")).toHaveLength(2);
    expect(screen.getAllByRole("option")).toHaveLength(3);
    expect(screen.getAllByText("astrolabe", { selector: "mark" })).not.toHaveLength(0);
    expect(doneGroup?.getAttribute("data-status")).toBe("done");
  } finally {
    search.mockRestore();
    view.unmount();
    view.queryClient.clear();
  }
});

test("does not query below two characters", () => {
  const search = spyOn(api, "search").mockResolvedValue({ results: [], took_ms: 1 });
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

test("arrow keys move aria-activedescendant and Enter navigates to the active href", async () => {
  const search = spyOn(api, "search").mockResolvedValue({ results, took_ms: 1 });
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

test("closes the palette when navigation changes", async () => {
  let closed = 0;
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <RouteChanger />
        <SearchPalette
          onClose={() => {
            closed += 1;
          }}
          open
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

test("shows QueryError when the search request fails", async () => {
  const search = spyOn(api, "search").mockRejectedValue(new Error("network unavailable"));
  const view = renderPalette();

  try {
    fireEvent.change(screen.getByRole("combobox", { name: "Search" }), {
      target: { value: "astrolabe" },
    });
    expect((await screen.findByRole("alert")).textContent).toContain("Search failed.");
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

function ShortcutProbe({ onToggle }: { onToggle: () => void }): ReactNode {
  useSearchShortcut(onToggle);
  return <textarea aria-label="Editor" />;
}

test("global search shortcut toggles outside text editing targets", () => {
  let toggled = 0;
  const view = render(<ShortcutProbe onToggle={() => (toggled += 1)} />);

  try {
    fireEvent.keyDown(window, { ctrlKey: true, key: "k" });
    expect(toggled).toBe(1);

    fireEvent.keyDown(screen.getByRole("textbox", { name: "Editor" }), { ctrlKey: true, key: "k" });
    expect(toggled).toBe(1);

    fireEvent.keyDown(window, { key: "k", metaKey: true });
    expect(toggled).toBe(2);
  } finally {
    view.unmount();
  }
});
