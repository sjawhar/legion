import { afterEach, beforeAll, beforeEach, expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";

import type { DisplayTree } from "../../api/types";
import fixtures from "./__fixtures__/display-trees.json";
import { MarkdownBody } from "./MarkdownBody";
import { MarkdownPreview } from "./MarkdownPreview";
import {
  loadedEngine,
  type MarkdownEngine,
  setParseWorkerFactory,
  useMarkdownHeadline,
  warmMarkdownRenderer,
} from "./markdown-engine";
import { treeHead } from "./tree-fragment";

/**
 * The views draw the reading Go made of a stored text and parse no markdown for it.
 *
 * Every tree here is Go's own, from the fixture `pmdoc`'s `TestWebDisplayFixture` writes and CI
 * checks: a tree the browser's parser made would prove nothing about the trees the server
 * sends.
 *
 * How the parses are counted: the shared engine (`markdown-engine.ts`) is one object the whole
 * page holds, and its `parse` is the only markdown parser any of these three surfaces can
 * reach - `useRenderedMarkdown`, the mermaid pass and `useMarkdownHeadline` all go through it,
 * and the parse worker is asked only for a body the engine was given no tree for. So `parses`
 * below counts every parse these surfaces could make, and the parse-worker factory throws, so a
 * body that reached the worker instead would fail the test rather than slip past the counter.
 * The last row is the control: a text Dispatch does not store is parsed, and the counter sees
 * it.
 */
let parses = 0;

/** One fixture case: the stored text and the readings Go made of it. */
interface Fixture {
  readonly name: string;
  readonly markdown: string;
  readonly latest: DisplayTree;
  readonly version: DisplayTree;
  readonly snippet?: DisplayTree;
}

function stored(name: string): Fixture {
  const found = (fixtures as Fixture[]).find((fixture) => fixture.name === name);
  if (found === undefined) {
    throw new Error(`no stored text named ${name} in the display-tree fixture`);
  }
  return found;
}

function withQueries(children: ReactNode): ReactNode {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
}

beforeAll(async () => {
  await warmMarkdownRenderer();
  const engine = loadedEngine();
  if (engine === undefined) {
    throw new Error("the Markdown engine did not load");
  }
  const own = engine.parse.bind(engine);
  const counted: { parse: MarkdownEngine["parse"] } = engine;
  counted.parse = (markdown, softBreaks) => {
    parses += 1;
    return own(markdown, softBreaks);
  };
});

beforeEach(() => {
  parses = 0;
  setParseWorkerFactory(() => {
    throw new Error("a stored text was sent to the parse worker");
  });
});

afterEach(() => {
  setParseWorkerFactory(undefined);
  cleanup();
});

test("MarkdownBody draws a stored text's tree and parses no markdown for it", () => {
  const body = stored("formatted-body");
  const view = render(withQueries(<MarkdownBody markdown={body.markdown} tree={body.latest} />));
  const root = view.container.querySelector("[data-markdown-fallback]");
  expect(root).toBeNull();
  expect(view.container.querySelector("h1")?.textContent).toBe("Deploy");
  expect(view.container.querySelector("strong")?.textContent).toBe("deploy");
  expect(view.container.querySelector("code")?.textContent).toBe("make check");
  expect(view.container.querySelectorAll("li")).toHaveLength(2);
  expect(parses).toBe(0);
});

test("MarkdownPreview draws a stored text's tree and parses no markdown for it", () => {
  const body = stored("formatted-body");
  const view = render(
    withQueries(<MarkdownPreview lines={2} markdown={body.markdown} tree={body.latest} />)
  );
  expect(view.container.textContent).toContain("Deploy");
  expect(view.container.querySelector("strong")?.textContent).toBe("deploy");
  expect(parses).toBe(0);
});

function Headline({ max, tree }: { max: number; tree: DisplayTree }): ReactNode {
  return <span data-headline="">{useMarkdownHeadline(tree, max)?.text}</span>;
}

test("useMarkdownHeadline takes the plain words of a tree and parses no markdown", async () => {
  const body = stored("formatted-body");
  const view = render(withQueries(<Headline max={100} tree={body.latest} />));
  await waitFor(() => {
    expect(view.container.querySelector("[data-headline]")?.textContent).toBe(
      "Deploy Stop the deploy and run make check first. one item two"
    );
  });
  expect(parses).toBe(0);
});

test("a headline shortens a reference as a rendered body's link reads it", async () => {
  const body = stored("a-reference");
  const view = render(withQueries(<Headline max={100} tree={body.latest} />));
  await waitFor(() => {
    expect(view.container.querySelector("[data-headline]")?.textContent).toBe(
      "Tracked as CORE-1 since Monday."
    );
  });
  expect(parses).toBe(0);
});

test("a mermaid block is drawn from the tree with no second parse of the body", () => {
  const body = stored("a-mermaid-block");
  const view = render(withQueries(<MarkdownBody markdown={body.markdown} tree={body.latest} />));
  expect(view.container.querySelector("pre[data-language='mermaid']")).not.toBeNull();
  // The open-fence check is the one thing that would parse the body again
  // (`mermaid-diagrams.ts`); a stored text's fences are closed, so the drawing asks for none.
  expect(parses).toBe(0);
});

test("a preview marks the runs the server matched, from the snippet's own tree", () => {
  const hit = stored("a-marked-snippet");
  if (hit.snippet === undefined) {
    throw new Error("the marked snippet carries no snippet tree");
  }
  const view = render(
    withQueries(
      <MarkdownPreview
        highlightClassName="hit"
        lines={2}
        markdown={hit.markdown}
        tree={hit.snippet}
      />
    )
  );
  const marks = [...view.container.querySelectorAll("mark")];
  expect(marks.map((mark) => mark.textContent)).toEqual(["deploy", "port"]);
  expect(marks.every((mark) => mark.className === "hit")).toBe(true);
  expect(parses).toBe(0);
});

test("a hit inside a link's own words marks the whole link, whose title replaces them", () => {
  const hit = stored("a-marked-reference");
  if (hit.snippet === undefined) {
    throw new Error("the marked reference carries no snippet tree");
  }
  const view = render(
    withQueries(
      <MarkdownPreview
        highlightClassName="hit"
        lines={2}
        markdown={hit.markdown}
        tree={hit.snippet}
      />
    )
  );
  const mark = view.container.querySelector("mark");
  expect(mark?.className).toBe("hit");
  expect(mark?.querySelector("[data-dispatch-ref]")).not.toBeNull();
  expect(parses).toBe(0);
});

test("a view that shows the head of a text cuts the tree and parses nothing", () => {
  const body = stored("a-long-answer");
  const view = render(
    withQueries(
      <MarkdownPreview lines={3} markdown={body.markdown} tree={treeHead(body.latest, 40)} />
    )
  );
  const shown = view.container.textContent ?? "";
  expect(shown.endsWith("…")).toBe(true);
  expect(shown.length).toBeLessThanOrEqual(42);
  expect(body.markdown.startsWith(shown.slice(0, -1))).toBe(true);
  expect(parses).toBe(0);
});

test("a cut short of the whole text leaves it whole", () => {
  const body = stored("one-paragraph");
  expect(treeHead(body.latest, 400)).toBe(body.latest);
});

test("the version mode keeps the attributes the server wrote, where the latest reading drops them", () => {
  const decision = stored("an-answered-decision");
  expect(JSON.stringify(decision.version)).toContain("answered");
  expect(JSON.stringify(decision.latest)).not.toContain("answered");
});

test("a text Dispatch does not store is parsed, which is what the counter counts", () => {
  const view = render(withQueries(<MarkdownBody markdown="Stop the **deploy**." />));
  expect(view.container.querySelector("strong")?.textContent).toBe("deploy");
  expect(parses).toBeGreaterThan(0);
});
