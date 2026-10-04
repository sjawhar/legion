import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { Link, MemoryRouter, Route, Routes } from "react-router-dom";

import { api } from "../../api/client";
import type { AskRead, IssueDetails } from "../../api/types";
import { MarkdownPreview } from "./MarkdownPreview";
import { useMarkdownHeadline } from "./markdown-engine";
import { Unfurl } from "./Unfurl";

const issue: IssueDetails = {
  route_status: null,
  route_holder: null,
  artifacts: [],
  children: [],
  closed_at: null,
  created_at: "2026-09-09T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  external_links: [],
  key: "CORE-1",
  labels: [],
  last_seq: 1,
  number: 1,
  open_asks: [],
  parent: null,
  assignee: null,
  claim: null,
  components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
  primary_artifact_id: "artifact-none",
  priority: null,
  project: "CORE",
  rank: "U",
  referenced_by_count: 0,
  route: null,
  status: "in_progress",
  title: "Design decision",
  updated_at: "2026-09-09T00:00:00Z",
};

function askRead(question: string): AskRead {
  return {
    ask: {
      id: "ask-1",
      issue_key: "CORE-1",
      kind: "question",
      author: { id: "alice", kind: "user" },
      question,
      options: [],
      multiple: false,
      urgency: "med",
      anchor: null,
      state: "open",
      answer: null,
      opened_event_id: 1,
      created_at: "2026-09-09T00:00:00Z",
      edited_at: null,
    },
    edits: [],
    followers: [],
    replies: [],
  };
}

function Providers({ children }: { children: ReactNode }): ReactNode {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return (
    <MemoryRouter>
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    </MemoryRouter>
  );
}

function preview(container: HTMLElement): HTMLElement {
  const element = container.querySelector<HTMLElement>("[data-markdown-preview]");
  if (element === null) throw new Error("no MarkdownPreview rendered");
  return element;
}

afterEach(() => {
  cleanup();
});

test("a preview's clamp is its own display: no display utility that outranks line-clamp, and no prose width cap", async () => {
  // Tailwind emits `.block` after `.line-clamp-N` in the same layer, so a root carrying both
  // computes `display: block` and never clamps; prose's own `max-width: 65ch` cuts the line
  // short of its host. Neither may be on the root.
  const view = render(<MarkdownPreview lines={1} markdown="**held**" />);
  try {
    const classes = preview(view.container).className.split(/\s+/);
    expect(classes).toContain("line-clamp-1");
    expect(classes).toContain("max-w-none");
    for (const display of ["block", "inline", "inline-block", "flex", "inline-flex", "grid"]) {
      expect(classes).not.toContain(display);
    }
  } finally {
    view.unmount();
  }
});

test("a preview inside a link holds no link of its own, and a click on its reference follows the host", async () => {
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  const view = render(
    <Providers>
      <Routes>
        <Route
          element={
            <Link to="/host">
              <MarkdownPreview
                lines={2}
                markdown="see [the docs](https://example.com/docs) and dispatch://CORE-1"
              />
            </Link>
          }
          path="/"
        />
        <Route element={<p>Host page</p>} path="/host" />
        <Route element={<p>Reference page</p>} path="/issues/:key" />
      </Routes>
    </Providers>
  );
  try {
    const element = preview(view.container);
    await waitFor(() => expect(element.textContent).toContain("Design decision"));
    // One link on the row: the host. A nested `<a>` is a second tab stop inside the host and
    // a second target for the same click.
    expect(element.querySelector("a")).toBeNull();
    expect(within(view.container).getAllByRole("link")).toHaveLength(1);
    const reference = element.querySelector<HTMLElement>("[data-dispatch-ref]");
    expect(reference?.getAttribute("data-dispatch-ref")).toBe("dispatch://CORE-1");
    expect(reference?.hasAttribute("href")).toBe(false);
    expect(reference?.textContent).toBe("Design decision");
    const docs = Array.from(element.querySelectorAll("[data-markdown-link]")).find(
      (link) => link.textContent === "the docs"
    );
    expect(docs).toBeDefined();
    if (reference === null || reference === undefined) throw new Error("no reference rendered");
    fireEvent.click(reference);
    expect(await screen.findByText("Host page")).toBeDefined();
    expect(screen.queryByText("Reference page")).toBeNull();
  } finally {
    view.unmount();
    getIssue.mockRestore();
  }
});

test("search hits mark whole words in any case, and a reference the server matched is marked whole", async () => {
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  // What the server's ts_headline marks for `port CORE-1` over this body: `port` as a word (not
  // the `port` in `Report`, `support` or `export`), and the reference's two lexemes.
  const view = render(
    <Providers>
      <MarkdownPreview
        highlight={["PORT", "CORE", "-1", "", "  "]}
        highlightClassName="hit"
        lines={2}
        markdown="Report the port to support before we export dispatch://CORE-1"
      />
    </Providers>
  );
  try {
    const element = preview(view.container);
    await waitFor(() => expect(element.textContent).toContain("Design decision"));
    const marks = Array.from(element.querySelectorAll("mark"), (mark) => mark.textContent);
    expect(marks).toEqual(["port", "Design decision"]);
    const marked = element.querySelector("mark [data-dispatch-ref]");
    expect(marked?.getAttribute("data-dispatch-ref")).toBe("dispatch://CORE-1");
    expect(element.textContent).not.toContain("dispatch://");
  } finally {
    view.unmount();
    getIssue.mockRestore();
  }
});

test("a hit after a character whose lower case is longer is marked on the right words", async () => {
  // `İ` lower-cases to two code units, so an index found in the lower-cased text lands one
  // character late in the source.
  const view = render(<MarkdownPreview highlight={["port"]} lines={1} markdown="İstanbul port" />);
  try {
    const element = preview(view.container);
    await waitFor(() => expect(element.textContent).toBe("İstanbul port"));
    expect(Array.from(element.querySelectorAll("mark"), (mark) => mark.textContent)).toEqual([
      "port",
    ]);
  } finally {
    view.unmount();
  }
});

function Headline({ markdown, max }: { markdown: string; max: number }): ReactNode {
  const headline = useMarkdownHeadline(markdown, max);
  return headline === undefined ? null : (
    <output data-cut={String(headline.cut)}>{headline.text}</output>
  );
}

test("a headline writes a reference as its short form, keeps one in code as written, and never cuts a character in half", async () => {
  const view = render(
    <>
      <Headline
        markdown="Blocked on **dispatch://CORE-2/ask/a1**, see `dispatch://CORE-3`"
        max={60}
      />
      <Headline markdown={`a${"😀".repeat(60)}`} max={60} />
      <Headline markdown="Use Array<string> here" max={60} />
    </>
  );
  try {
    const [reference, emoji, html] = await waitFor(() => {
      const outputs = Array.from(view.container.querySelectorAll("output"));
      expect(outputs).toHaveLength(3);
      return outputs;
    });
    expect(reference?.textContent).toBe("Blocked on CORE-2 ask, see dispatch://CORE-3");
    expect(reference?.getAttribute("data-cut")).toBe("false");
    const cut = emoji?.textContent ?? "";
    expect(cut).toBe(`a${"😀".repeat(59)}…`);
    expect(/[\uD800-\uDBFF](?![\uDC00-\uDFFF])/.test(cut)).toBe(false);
    expect(emoji?.getAttribute("data-cut")).toBe("true");
    // A tag-shaped run reads as the literal characters a rendered body shows for it.
    expect(html?.textContent).toBe("Use Array<string> here");
  } finally {
    view.unmount();
  }
});

test("an ask's unfurl card shows its question once, even when the question ends in an ellipsis", async () => {
  const getAsk = spyOn(api, "getAsk").mockResolvedValue(askRead("Should we wait for **CORE-2**…"));
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  const view = render(
    <Providers>
      <Unfurl body="dispatch://CORE-1/ask/ask-1" />
    </Providers>
  );
  try {
    const card = await within(view.container).findByRole("link", {
      name: "Should we wait for CORE-2…",
    });
    // The title holds the whole question; a description would repeat it under itself.
    expect(card.querySelector("[data-markdown-preview]")).toBeNull();
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getIssue.mockRestore();
  }
});

test("an ask's unfurl card never borrows its issue's status while the ask loads", async () => {
  const getAsk = spyOn(api, "getAsk").mockReturnValue(Promise.withResolvers<AskRead>().promise);
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  const view = render(
    <Providers>
      <Unfurl body="dispatch://CORE-1/ask/ask-1" />
    </Providers>
  );
  try {
    await waitFor(() => expect(getIssue).toHaveBeenCalledWith("CORE-1"));
    // The issue answered and the ask has not: the card says what it is, not the issue's status.
    await Bun.sleep(20);
    const card = within(view.container).getByRole("link");
    expect(card.textContent).toBe("dispatch://CORE-1/ask/ask-1");
    expect(card.textContent).not.toContain("in_progress");
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getIssue.mockRestore();
  }
});

test("a long ask's unfurl card shows the whole question under its cut title, formatted", async () => {
  const question = `**Which** of these ${"options ".repeat(10)}ships first?`;
  const getAsk = spyOn(api, "getAsk").mockResolvedValue(askRead(question));
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  const view = render(
    <Providers>
      <Unfurl body="dispatch://CORE-1/ask/ask-1" />
    </Providers>
  );
  try {
    const card = await within(view.container).findByRole("link", { name: /^Which of these/ });
    await waitFor(() =>
      expect(card.querySelector("[data-markdown-preview] strong")).not.toBeNull()
    );
    expect(card.firstElementChild?.textContent).toEndWith("…");
    expect(card.querySelector("[data-markdown-preview]")?.textContent).toEndWith("ships first?");
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getIssue.mockRestore();
  }
});
