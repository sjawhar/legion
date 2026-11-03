import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, waitFor } from "@testing-library/react";
import type { Window as HappyDOMWindow } from "happy-dom";
import type { ComponentProps, ReactNode } from "react";

import { api } from "../../api/client";
import type { DisplayTree, IssueReferenceSummary } from "../../api/types";
import fixtures from "./__fixtures__/display-trees.json";
import { MarkdownPreview } from "./MarkdownPreview";
import { installFakeParseWorker, restoreParseWorker } from "./parse-worker-fixture";

/**
 * `MarkdownPreview` on its own: the inputs a real surface hands it that the one-fixture surface
 * test (`markdown-surfaces.test.tsx`) never does.
 *
 * The highlight rows below are a deliberate behaviour change of this task, not tests bent to
 * pass. They used to hand the preview the matched phrases and assert that the browser found
 * those words in its own rendered text; the server now reads the hit's snippet and marks the
 * runs it matched in the tree it sends (`pmdoc.DisplaySnippetJSON`), and the preview draws
 * those marks. So each row hands it the snippet tree Go made - from the fixture `pmdoc`'s
 * `TestWebDisplayFixture` writes and CI checks - and asserts the same thing of the result:
 * which words are marked, and that no syntax and no `dispatch://` is left in what a reader
 * sees.
 */

/** One marked snippet of the display-tree fixture, by name. */
function stored(name: string): { markdown: string; snippet: DisplayTree } {
  const found = (fixtures as { name: string; markdown: string; snippet?: DisplayTree }[]).find(
    (fixture) => fixture.name === name
  );
  if (found?.snippet === undefined) {
    throw new Error(`no marked snippet named ${name} in the display-tree fixture`);
  }
  return { markdown: found.markdown, snippet: found.snippet };
}

/** CORE-1's summary read, which a reference to it shows. */
const issue: IssueReferenceSummary = {
  documents: [],
  key: "CORE-1",
  project: "CORE",
  status: "in_progress",
  title: "Design decision",
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
  const hit = stored("marks-inside-formatting");
  const { preview, view } = renderPreview({
    highlightClassName: "hit",
    lines: 2,
    markdown: hit.markdown,
    tree: hit.snippet,
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
  const getIssueSummary = spyOn(api, "getIssueSummary").mockResolvedValue(issue);
  // q=CORE-1: `Tracked as <mark>dispatch://CORE-1</mark> since Monday` - the match is inside
  // the reference the preview replaces with the issue's title.
  const bare = stored("a-marked-reference");
  const { preview, rerender, view } = renderPreview({
    highlightClassName: "hit",
    lines: 2,
    markdown: bare.markdown,
    tree: bare.snippet,
  });
  try {
    await waitFor(() => expect(preview().textContent).toContain("Design decision"));
    const reference = preview().querySelector<HTMLElement>("[data-dispatch-ref]");
    if (reference === null) throw new Error("the reference did not render as one");
    expect(reference.textContent).toBe("Design decision");
    expect(reference.closest("mark") !== null || reference.querySelector("mark") !== null).toBe(
      true
    );
    for (const mark of preview().querySelectorAll("mark")) {
      expect(mark.textContent?.trim()).not.toBe("");
    }
    expect(preview().textContent).not.toContain("dispatch://");

    // q=design on a hand-written link to the same target: the hit is in the words its author
    // gave the link, which the resolved title replaces.
    const linkHit = stored("a-marked-link-to-a-reference");
    rerender({
      highlightClassName: "hit",
      lines: 2,
      markdown: linkHit.markdown,
      tree: linkHit.snippet,
    });
    await waitFor(() =>
      expect(preview().querySelector("[data-dispatch-ref]")?.textContent).toBe("Design decision")
    );
    const linked = preview().querySelector<HTMLElement>("[data-dispatch-ref]");
    expect(linked?.closest("mark") !== null || linked?.querySelector("mark") !== null).toBe(true);
    expect(preview().textContent).not.toContain("](");
  } finally {
    view.unmount();
    getIssueSummary.mockRestore();
  }
});

test("a hit marks the word the server matched, not the same letters inside other words", async () => {
  // q=port: `Report the <mark>port</mark> number to the support team before we export`. The
  // server marks whole lexemes, and the preview draws the marks of the tree and adds none of
  // its own, so `support` and `export` stay plain.
  const hit = stored("a-mark-on-one-word-alone");
  const { preview, view } = renderPreview({
    highlightClassName: "hit",
    lines: 2,
    markdown: hit.markdown,
    tree: hit.snippet,
  });
  try {
    await waitFor(() => expect(preview().textContent).toContain("export"));
    expect(marks(preview())).toEqual(["port"]);
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

test("a Dispatch picture shows as its thumbnail beside its caption, a link only where links stay live", async () => {
  // The dashboard is served from an origin, where a picture's same-origin bytes route resolves;
  // at happy-dom's `about:blank` every picture would fail at once (`MarkdownBody.test.tsx`).
  const testWindow = window as unknown as HappyDOMWindow;
  const pageBefore = window.location.href;
  testWindow.happyDOM.setURL("https://dispatch.test/issues/CORE-1/conversation");
  const markdown = "The broken layout: ![shot.png](dispatch://CORE-1/artifact/shot-png@v1)";
  const { preview, rerender, view } = renderPreview({ lines: 2, markdown });
  try {
    await waitFor(() => expect(preview().querySelector("img")).not.toBeNull());
    const thumbnail = preview().querySelector("img");
    expect(thumbnail?.getAttribute("src")).toBe(
      "/api/v1/issues/CORE-1/artifacts/shot-png/versions/1"
    );
    expect(thumbnail?.className.split(" ")).toEqual(
      expect.arrayContaining(["h-10", "w-10", "object-cover"])
    );
    expect(words(preview())).toBe("The broken layout: shot.png");
    // Inert by default: the host is the one link, so the picture sits in link-styled text.
    expect(preview().querySelector("a")).toBeNull();
    expect(thumbnail?.closest("[data-dispatch-picture]")?.tagName).toBe("SPAN");

    // A host whose own control is a toggle keeps the picture's link to its page.
    rerender({ lines: 2, links: "live", markdown });
    await waitFor(() => expect(preview().querySelector("a img")).not.toBeNull());
    expect(preview().querySelector("a")?.getAttribute("href")).toBe(
      "/issues/CORE-1/artifacts/shot-png?v=1"
    );
  } finally {
    view.unmount();
    testWindow.happyDOM.setURL(pageBefore);
  }
});

test("a lead is the app's own words: syntax and a reference in it stay literal while the body formats", async () => {
  const getIssueSummary = spyOn(api, "getIssueSummary").mockResolvedValue(issue);
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
    getIssueSummary.mockRestore();
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

// Past `markdownWorkerThreshold` the preview's body is parsed off the page, so this row drives
// the faked worker (`parse-worker-fixture.ts`): the clamp it proves - the whole body parsed,
// never the two lines the reader can see - is the one the dashboard applies to a body that size.
test("a long body is parsed whole in the worker, so formatting past any visible cut renders and leaves no syntax", async () => {
  const worker = installFakeParseWorker();
  const { preview, view } = renderPreview({
    lines: 2,
    markdown: `${"word ".repeat(400)}**held** and [docs](https://example.com/docs)`,
  });
  try {
    await act(() => worker.answer(0));
    await waitFor(() => expect(preview().querySelector("strong")?.textContent).toBe("held"));
    expect(preview().textContent).not.toContain("**");
    expect(preview().textContent).not.toContain("](");
    expect(preview().textContent?.endsWith("held and docs")).toBe(true);
  } finally {
    restoreParseWorker();
    view.unmount();
  }
});

test("a body the engine cannot parse shows its literal text, led, and never as HTML", async () => {
  // Proof's schema has no node for raw HTML, so the parse throws. This is a text Dispatch does
  // not store - the only kind the browser still parses - so it carries no tree and no mark:
  // the marks a search hit shows now ride in the snippet's own tree, and a body with no tree
  // has none. That is the deliberate change; what this row holds still is the fallback itself.
  const source = "<script>alert(1)</script> astrolabe <mark>x</mark>";
  const { preview, rerender, view } = renderPreview({
    lead: "Replying to Planner — ",
    lines: 2,
    markdown: source,
  });
  try {
    await waitFor(() => expect(preview().getAttribute("data-markdown-fallback")).toBe("true"));
    expect(preview().textContent).toBe(`Replying to Planner — ${source}`);
    expect(preview().querySelector("script")).toBeNull();
    // The author's own `<mark>` is text, never an element.
    expect(marks(preview())).toEqual([]);

    // Edited into a body that parses, it renders formatted and stops being the fallback.
    rerender({ lines: 2, markdown: "**fixed** astrolabe" });
    await waitFor(() => expect(preview().querySelector("strong")?.textContent).toBe("fixed"));
    expect(preview().getAttribute("data-markdown-fallback")).toBeNull();
    expect(preview().textContent).toBe("fixed astrolabe");
  } finally {
    view.unmount();
  }
});

test("the hover text is the whole text as plain words, never its source, and a preview not asked for one has none", async () => {
  const body = (fixtures as { name: string; markdown: string; latest: DisplayTree }[]).find(
    (fixture) => fixture.name === "a-two-block-title"
  );
  if (body === undefined) {
    throw new Error("no stored text named a-two-block-title in the display-tree fixture");
  }
  const { preview, rerender, view } = renderPreview({
    fullTitle: true,
    lines: 1,
    markdown: body.markdown,
    tree: body.latest,
  });
  try {
    await waitFor(() => expect(preview().querySelector("strong")?.textContent).toBe("Stop"));
    await waitFor(() => expect(preview().getAttribute("title")).toBe("Stop the deploy one two"));
    rerender({ lines: 1, markdown: body.markdown, tree: body.latest });
    await waitFor(() => expect(preview().hasAttribute("title")).toBe(false));
  } finally {
    view.unmount();
  }
});
