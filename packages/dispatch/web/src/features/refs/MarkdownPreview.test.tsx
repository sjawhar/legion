import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, waitFor } from "@testing-library/react";
import type { ComponentProps, ReactNode } from "react";

import { api } from "../../api/client";
import type { IssueDetails } from "../../api/types";
import { MarkdownPreview } from "./MarkdownPreview";

/**
 * `MarkdownPreview` on its own: the inputs a real surface hands it that the one-fixture surface
 * test (`markdown-surfaces.test.tsx`) never does. Highlights are what the server's `ts_headline`
 * returns for the query each case names (Postgres, the search route's own headline options), run
 * through `snippetSegments` the way `SearchPalette` does: the marked runs become `highlight`, the
 * rest of the text the `markdown`.
 */

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

type PreviewProps = ComponentProps<typeof MarkdownPreview>;

/** Renders one preview under a query client (a reference inside resolves its title through
 *  one), and hands back its root and a rerender with new props. */
function renderPreview(props: PreviewProps) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrap = (next: PreviewProps): ReactNode => (
    <QueryClientProvider client={client}>
      <MarkdownPreview {...next} />
    </QueryClientProvider>
  );
  const view = render(wrap(props));
  const preview = (): HTMLElement => {
    const element = view.container.querySelector<HTMLElement>("[data-markdown-preview]");
    if (element === null) throw new Error("no MarkdownPreview rendered");
    return element;
  };
  return { preview, rerender: (next: PreviewProps) => view.rerender(wrap(next)), view };
}

function marks(element: HTMLElement): (string | null)[] {
  return Array.from(element.querySelectorAll("mark"), (mark) => mark.textContent);
}

function words(element: HTMLElement): string {
  return (element.textContent ?? "").replace(/\s+/g, " ").trim();
}

afterEach(cleanup);

test("a hit inside bold or code text is marked inside the formatting, with no syntax left", async () => {
  // q="bold word" and q=code: `**<mark>Bold</mark>** <mark>word</mark>, see `<mark>code</mark>``.
  const { preview, view } = renderPreview({
    highlight: ["Bold", "word", "code"],
    lines: 2,
    markdown: "**Bold** word, see `code` here",
  });
  try {
    await waitFor(() => expect(preview().querySelector("strong")).not.toBeNull());
    expect(preview().querySelector("strong mark")?.textContent).toBe("Bold");
    expect(preview().querySelector("code mark")?.textContent).toBe("code");
    expect(marks(preview())).toEqual(["Bold", "word", "code"]);
    expect(preview().textContent).not.toContain("**");
    expect(preview().textContent).not.toContain("`");
  } finally {
    view.unmount();
  }
});

test("a hit inside a dispatch:// reference keeps its highlight on the reference's resolved title", async () => {
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  // q=CORE-1: `Report on dispatch://<mark>CORE</mark><mark>-1</mark> today` - two lexemes, both
  // inside the reference the preview replaces with the issue's title.
  const { preview, rerender, view } = renderPreview({
    highlight: ["CORE", "-1", "today"],
    lines: 2,
    markdown: "Report on dispatch://CORE-1 today",
  });
  try {
    await waitFor(() => expect(preview().textContent).toContain("Design decision"));
    const reference = preview().querySelector<HTMLElement>("[data-dispatch-ref]");
    if (reference === null) throw new Error("the reference did not render as one");
    expect(reference.textContent).toBe("Design decision");
    expect(reference.closest("mark") !== null || reference.querySelector("mark") !== null).toBe(
      true
    );
    expect(marks(preview())).toContain("today");
    for (const mark of preview().querySelectorAll("mark")) {
      expect(mark.textContent?.trim()).not.toBe("");
    }
    expect(preview().textContent).not.toContain("dispatch://");

    // q=design on a hand-written link to the same target: the hit is in the words its author
    // gave the link, which the resolved title replaces.
    rerender({ highlight: ["design"], lines: 2, markdown: "See [the design](dispatch://CORE-1)." });
    await waitFor(() =>
      expect(preview().querySelector("[data-dispatch-ref]")?.textContent).toBe("Design decision")
    );
    const linked = preview().querySelector<HTMLElement>("[data-dispatch-ref]");
    expect(linked?.closest("mark") !== null || linked?.querySelector("mark") !== null).toBe(true);
    expect(preview().textContent).not.toContain("](");
  } finally {
    view.unmount();
    getIssue.mockRestore();
  }
});

test("a hit marks the word the server matched, not the same letters inside other words", async () => {
  // q=port: `Report the <mark>port</mark> number to the support team before we export`.
  const { preview, rerender, view } = renderPreview({
    highlight: ["port"],
    lines: 2,
    markdown: "Report the port number to the support team before we export",
  });
  try {
    await waitFor(() => expect(preview().textContent).toContain("export"));
    expect(marks(preview())).toEqual(["port"]);

    // q=deploy stems: `redeploy after the <mark>deployment</mark>; <mark>deploy</mark>`.
    rerender({
      highlight: ["deployment", "deploy"],
      lines: 2,
      markdown: "redeploy after the deployment; deploy",
    });
    await waitFor(() => expect(marks(preview())).toEqual(["deployment", "deploy"]));

    // q=CORE-1 on plain text: `Tracked as <mark>CORE</mark><mark>-1</mark> since Monday`. A
    // hit that starts at a non-word character still marks where it stands.
    rerender({ highlight: ["CORE", "-1"], lines: 2, markdown: "Tracked as CORE-1 since Monday" });
    await waitFor(() => expect(marks(preview())).toEqual(["CORE", "-1"]));
  } finally {
    view.unmount();
  }
});

test("a lead with no body shows the lead alone", async () => {
  // A reply quote whose parent is not loaded: `Replying to a message`, and nothing after it.
  const { preview, view } = renderPreview({
    lead: "Replying to a message",
    lines: 1,
    markdown: "",
  });
  try {
    await waitFor(() => expect(preview().textContent).toBe("Replying to a message"));
    expect(preview().getAttribute("data-markdown-fallback")).toBeNull();
  } finally {
    view.unmount();
  }
});

test("a lead is the app's own words: syntax and a reference in it stay literal while the body formats", async () => {
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  // A session titles itself; its title is the lead of every quote of its messages.
  const lead = "Replying to **Planner** (dispatch://CORE-1) — ";
  const { preview, view } = renderPreview({ lead, lines: 1, markdown: "*body*" });
  try {
    await waitFor(() => expect(preview().querySelector("em")?.textContent).toBe("body"));
    expect(preview().textContent).toBe(`${lead}body`);
    expect(preview().querySelector("strong")).toBeNull();
    expect(preview().querySelector("[data-dispatch-ref]")).toBeNull();
  } finally {
    view.unmount();
    getIssue.mockRestore();
  }
});

test("a body that is only a code fence shows its code on the line, with no fence and no block", async () => {
  const { preview, view } = renderPreview({
    lines: 1,
    markdown: "```ts\nconst plan = 1;\nsee dispatch://CORE-1\n```",
  });
  try {
    await waitFor(() => expect(preview().textContent).toContain("const plan = 1;"));
    const code = preview().querySelector("code");
    expect(code?.textContent?.startsWith("const plan = 1;")).toBe(true);
    expect(code?.textContent).toContain("see dispatch://CORE-1");
    expect(preview().querySelector("pre")).toBeNull();
    expect(preview().textContent).not.toContain("```");
    // Code is code: a reference written inside the fence is not resolved.
    expect(preview().querySelector("[data-dispatch-ref]")).toBeNull();
    expect(preview().getAttribute("data-markdown-fallback")).toBeNull();
  } finally {
    view.unmount();
  }
});

test("a body whose first block is a list keeps every item's formatting on one line, without its markers", async () => {
  const { preview, view } = renderPreview({
    lines: 1,
    markdown: "1. **Stop** the deploy\n2. Check `deploy.yml`\n   - nested *note*\n\nThen report.",
  });
  try {
    await waitFor(() => expect(preview().textContent).toContain("Then report."));
    expect(words(preview())).toBe("Stop the deploy Check deploy.yml nested note Then report.");
    expect(preview().querySelector("ol, ul, li, p")).toBeNull();
    expect(preview().querySelector("strong")?.textContent).toBe("Stop");
    // The second item's code and the nested item's emphasis: a preview that formats only its
    // first textblock and flattens the rest to plain words loses both.
    expect(preview().querySelector("code")?.textContent).toBe("deploy.yml");
    expect(preview().querySelector("em")?.textContent).toBe("note");
  } finally {
    view.unmount();
  }
});

test("a line break inside a paragraph reads as a space, so nothing after it falls below the clamp", async () => {
  const { preview, view } = renderPreview({
    lines: 1,
    markdown: "first line  \nsecond line\\\nthird line",
  });
  try {
    await waitFor(() => expect(preview().textContent).toContain("third line"));
    expect(preview().querySelector("br")).toBeNull();
    expect(words(preview())).toBe("first line second line third line");
  } finally {
    view.unmount();
  }
});

test("a long body is parsed whole, so formatting past any visible cut renders and leaves no syntax", async () => {
  const { preview, view } = renderPreview({
    lines: 2,
    markdown: `${"word ".repeat(400)}**held** and [docs](https://example.com/docs)`,
  });
  try {
    await waitFor(() => expect(preview().querySelector("strong")?.textContent).toBe("held"));
    expect(preview().textContent).not.toContain("**");
    expect(preview().textContent).not.toContain("](");
    expect(preview().textContent?.endsWith("held and docs")).toBe(true);
  } finally {
    view.unmount();
  }
});

test("a body the engine cannot parse shows its literal text, led and highlighted, and never as HTML", async () => {
  // Proof's schema has no node for raw HTML, so the parse throws. A comment holding
  // `<script>alert(1)</script> astrolabe <mark>x</mark>`, found by q=astrolabe, comes back escaped
  // with one server mark, which `snippetSegments` decodes into exactly this source and one hit.
  const source = "<script>alert(1)</script> astrolabe <mark>x</mark>";
  const { preview, rerender, view } = renderPreview({
    highlight: ["astrolabe"],
    lead: "Replying to Planner — ",
    lines: 2,
    markdown: source,
  });
  try {
    await waitFor(() => expect(preview().getAttribute("data-markdown-fallback")).toBe("true"));
    expect(preview().textContent).toBe(`Replying to Planner — ${source}`);
    expect(preview().querySelector("script")).toBeNull();
    // The author's own `<mark>` is text; only the server's hit is a mark.
    expect(marks(preview())).toEqual(["astrolabe"]);

    // Edited into a body that parses, it renders formatted and stops being the fallback.
    rerender({ highlight: ["astrolabe"], lines: 2, markdown: "**fixed** astrolabe" });
    await waitFor(() => expect(preview().querySelector("strong")?.textContent).toBe("fixed"));
    expect(preview().getAttribute("data-markdown-fallback")).toBeNull();
    expect(preview().textContent).toBe("fixed astrolabe");
    expect(marks(preview())).toEqual(["astrolabe"]);
  } finally {
    view.unmount();
  }
});

test("the hover text is the whole text as plain words, never its source, and a preview not asked for one has none", async () => {
  const source = "**Stop** the deploy\n\n1. one\n2. two";
  const { preview, rerender, view } = renderPreview({
    fullTitle: true,
    lines: 1,
    markdown: source,
  });
  try {
    await waitFor(() => expect(preview().querySelector("strong")?.textContent).toBe("Stop"));
    await waitFor(() => expect(preview().getAttribute("title")).toBe("Stop the deploy one two"));
    rerender({ lines: 1, markdown: source });
    await waitFor(() => expect(preview().hasAttribute("title")).toBe(false));
  } finally {
    view.unmount();
  }
});
