import { beforeAll, expect, test } from "bun:test";
import { readdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { setBlockIdGenerator } from "@legion/proof-editor";

import { loadedEngine, warmMarkdownRenderer } from "../refs/markdown-engine";
import { paintBlock } from "../refs/paint-block";

/**
 * The fixtures the server's own renderer and this page's preview are both held to
 * (dispatch://INFRA-341, R11, acceptance A11).
 *
 * A document's address now carries its opening blocks as HTML, drawn by the Go server from the
 * tree it holds (`internal/dispatch/pmdoc/first_screen.go`), and this page takes those blocks as
 * its preview rather than drawing them again (`DocumentPreview`). So the two renderers must agree
 * block for block: the same `data-block-id` values, the same text, in the same order. These
 * fixtures are where that is proved. This row records what the page's own preview draws for each
 * document of the corpus, and `first_screen_test.go` holds the server's renderer to the same
 * record - whole for a document the first screen carries all of, and as its opening for one the
 * first screen cuts.
 *
 * The corpus is Markdown, beside the record, under the Go package that owns the renderer, so one
 * directory holds what both read:
 * `packages/envoy/internal/dispatch/pmdoc/testdata/first-screen/`.
 *
 * Re-record it with `UPDATE_FIRST_SCREEN=1 bun test web/src/features/doc/first-screen-fixtures`
 * after a deliberate change to what this page draws, and read the diff: every line of it is a
 * block a reader sees.
 */
const corpus = join(
  import.meta.dir,
  "../../../../../envoy/internal/dispatch/pmdoc/testdata/first-screen"
);
const record = join(
  import.meta.dir,
  "../../../../../envoy/internal/dispatch/pmdoc/testdata/first-screen.json"
);

/** One document of the corpus, as both renderers read it. */
interface Fixture {
  readonly name: string;
  readonly markdown: string;
  /** The tree the page's own parse builds, which the server's renderer draws in the Go row. */
  readonly pm_json: unknown;
  /** The text of every top-level block the page's preview draws, in the order it draws them.
   *  Text and order, not ids: this page's preview mints ids of its own as it parses, and the
   *  server draws the ids its live tree holds, so the two renderers are held to what a reader
   *  reads and to where each block stands, which is what must not differ. */
  readonly blocks: readonly string[];
}

/** Ids this corpus mints, in the order the parse asks for them, so one document is one record. */
function sequentialBlockIds(): void {
  let minted = 0;
  setBlockIdGenerator(() => `b-${String(++minted).padStart(6, "0")}`);
}

/** The text of each top-level block under `root`, in the order it is drawn. */
function blocksOf(root: ParentNode): string[] {
  return [...root.children].map((element) => element.textContent ?? "");
}

function corpusDocuments(): { markdown: string; name: string }[] {
  return readdirSync(corpus)
    .filter((file) => file.endsWith(".md"))
    .sort()
    .map((file) => ({
      markdown: readFileSync(join(corpus, file), "utf8"),
      name: file.replace(/\.md$/, ""),
    }));
}

/** What the page's preview draws for `markdown`: the engine's parse, painted exactly as
 *  `DocumentPreview` paints it (`refs/paint-block.ts`). */
function drawnByThePage(markdown: string): { host: HTMLElement; tree: unknown } {
  const engine = loadedEngine();
  if (engine === undefined) {
    throw new Error("The markdown engine did not load.");
  }
  sequentialBlockIds();
  const parsed = engine.parse(markdown);
  if (parsed === undefined) {
    throw new Error("The markdown engine refused a fixture.");
  }
  const host = document.createElement("div");
  paintBlock(host, parsed, engine.serializer);
  return { host, tree: parsed.toJSON() };
}

beforeAll(async () => {
  // The warm-up parses a body of its own and mints its ids, so the corpus's own numbering starts
  // after it rather than inside it.
  await warmMarkdownRenderer();
});

test("the page's preview draws the recorded blocks of every fixture document", () => {
  const drawn: Fixture[] = corpusDocuments().map(({ markdown, name }) => {
    const { host, tree } = drawnByThePage(markdown);
    return { blocks: blocksOf(host), markdown, name, pm_json: tree };
  });
  setBlockIdGenerator(null);

  if (process.env.UPDATE_FIRST_SCREEN !== undefined) {
    writeFileSync(record, `${JSON.stringify(drawn, null, 2)}\n`);
  }

  const recorded = JSON.parse(readFileSync(record, "utf8")) as Fixture[];
  expect(drawn.map(({ name }) => name)).toEqual(recorded.map(({ name }) => name));
  for (const [index, fixture] of drawn.entries()) {
    expect({ blocks: fixture.blocks, name: fixture.name }).toEqual({
      blocks: recorded[index].blocks as string[],
      name: recorded[index].name,
    });
    expect(fixture.pm_json).toEqual(recorded[index].pm_json);
    expect(fixture.markdown).toBe(recorded[index].markdown);
  }
});
