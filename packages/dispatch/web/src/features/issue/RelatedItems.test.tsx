import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Suggestions, WriteSuggestion } from "../../api/types";
import { RelatedItems } from "./RelatedItems";
import { RELATED_DEBOUNCE_MS } from "./related-timing";

const answer = { at: "2026-10-01T00:00:00Z", selected: [], text: null, user: "alice" };

const issueItem: WriteSuggestion = {
  href: "/issues/CORE-12",
  id: "CORE-12",
  kind: "issue",
  owner: { key: "CORE-12", kind: "issue", status: "in_progress", title: "Broadcast send order" },
  snippet: "the order broadcasts send in",
};

const documentItem: WriteSuggestion = {
  href: "/projects/CORE/documents/send-order",
  id: "doc-1",
  kind: "document",
  owner: {
    artifact_id: "art-1",
    kind: "document",
    name: "Send order",
    project: "CORE",
    slug: "send-order",
  },
  snippet: "how the send order was settled",
};

const askItem: WriteSuggestion = {
  ask: { answer, state: "answered" },
  href: "/issues/CORE-30/asks/ask-1",
  id: "ask-1",
  kind: "ask",
  owner: { key: "CORE-30", kind: "issue", status: "done", title: "Which order do we send in?" },
  snippet: "which order do we send in",
};

function renderRelated(
  check: () => Promise<Suggestions>,
  project = "CORE",
  title = "Broadcast send order"
) {
  const relatedItems = spyOn(api, "relatedItems").mockImplementation(() => check());
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <RelatedItems project={project} title={title} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    relatedItems,
    unmount: () => {
      view.unmount();
      queryClient.clear();
      relatedItems.mockRestore();
    },
  };
}

test("lists each related item as a link named by its owner, with where an ask stands", async () => {
  const view = renderRelated(async () => ({
    outcome: "found",
    related: [issueItem, documentItem, askItem],
    search: "meaning_and_keywords",
  }));
  try {
    await screen.findByText("Related items (searched by meaning and keywords):", undefined, {
      timeout: 2000,
    });
    const links = screen.getAllByRole("link");
    expect(links).toHaveLength(3);

    const issueLink = screen.getByRole("link", { name: "CORE-12 Broadcast send order" });
    expect(issueLink.getAttribute("href")).toBe("/issues/CORE-12");
    expect(issueLink.getAttribute("target")).toBe("_blank");

    const documentLink = screen.getByRole("link", { name: "Send order" });
    expect(documentLink.getAttribute("href")).toBe("/projects/CORE/documents/send-order");
    expect(documentLink.getAttribute("target")).toBe("_blank");

    const askLink = screen.getByRole("link", { name: "CORE-30 Which order do we send in?" });
    expect(askLink.getAttribute("href")).toBe("/issues/CORE-30/asks/ask-1");
    expect(askLink.getAttribute("target")).toBe("_blank");
    expect(screen.getByText("Answered")).toBeTruthy();
  } finally {
    view.unmount();
  }
});

// A check that never ran must not read as one that found nothing, so the two sentences differ
// and the one for a check that never ran names the words to search by hand.
test("a search that found nothing and a check that was never made read differently", async () => {
  const searched = renderRelated(async () => ({
    outcome: "none",
    related: [],
    search: "keywords_only",
  }));
  let foundNothing: string | null;
  try {
    foundNothing = (
      await screen.findByText(
        "Related-item check searched (keywords only) and found nothing.",
        undefined,
        { timeout: 2000 }
      )
    ).textContent;
  } finally {
    searched.unmount();
  }

  const notMade = renderRelated(async () => ({
    missing: "the search timed out",
    outcome: "not_made",
    related: [],
    search_query: "broadcast send order",
  }));
  try {
    const sentence = await screen.findByText(/No related-item check was made/, undefined, {
      timeout: 2000,
    });
    expect(sentence.textContent).toBe(
      "No related-item check was made: the search timed out. Search Dispatch for “broadcast send order” before you rely on this being new."
    );
    expect(sentence.textContent).not.toBe(foundNothing);
  } finally {
    notMade.unmount();
  }
});

test("a past decision is named with its link and who answered it", async () => {
  const view = renderRelated(async () => ({
    decision: { ...askItem, answered_at: "2026-10-02T00:00:00Z", answered_by: "alice" },
    outcome: "none",
    related: [],
    search: "keywords_only",
  }));
  try {
    await screen.findByText("A past decision may already answer this:", undefined, {
      timeout: 2000,
    });
    // The decision's own list item holds who answered it, so a screen reader's item for the
    // decision names the decision and its answer together.
    const item = screen.getByRole("listitem");
    const link = within(item).getByRole("link", { name: "CORE-30 Which order do we send in?" });
    expect(link.getAttribute("href")).toBe("/issues/CORE-30/asks/ask-1");
    const answered = within(item).getByText(/Answered by alice/);
    // The time is a <time>, so it reads as "4 weeks ago" and carries the exact value for
    // assistive tech; asserting its relative words would make this row age.
    expect(answered.querySelector("time")?.getAttribute("datetime")).toBe("2026-10-02T00:00:00Z");
  } finally {
    view.unmount();
  }
});

test("a failed check says so and offers Retry", async () => {
  const view = renderRelated(async () => {
    throw new Error("network down");
  });
  try {
    await screen.findByText("Could not check for related items.", undefined, { timeout: 2000 });
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
  } finally {
    view.unmount();
  }
});

test("a title below the floor and a dialog with no project ask nothing", async () => {
  const short = renderRelated(async () => ({ outcome: "none", related: [] }), "CORE", "a");
  try {
    // Past the debounce, with real time: the component waits that long before it asks at all.
    await act(() => Bun.sleep(RELATED_DEBOUNCE_MS + 200));
    expect(short.relatedItems).toHaveBeenCalledTimes(0);
  } finally {
    short.unmount();
  }

  const noProject = renderRelated(async () => ({ outcome: "none", related: [] }), "");
  try {
    // Past the debounce, with real time: the component waits that long before it asks at all.
    await act(() => Bun.sleep(RELATED_DEBOUNCE_MS + 200));
    expect(noProject.relatedItems).toHaveBeenCalledTimes(0);
  } finally {
    noProject.unmount();
  }
});
