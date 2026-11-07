import { afterEach, describe, expect, spyOn, test } from "bun:test";

import serverBlockSchema from "../../../../../envoy/internal/dispatch/pmdoc/schema/blocks.json";
import { fixture, type PmdocFixture } from "../../__tests__/pmdoc-fixtures";
import type { BlockSchema } from "../../api/types";
import * as schemaModule from "../doc/schema";
import { type MarkdownRendering, renderWithEngine, warmMarkdownRenderer } from "./markdown-engine";
import { markdownWorkerThreshold } from "./markdown-worker-threshold";
import { flushTasks, installFakeParseWorker, restoreParseWorker } from "./parse-worker-fixture";

const blockSchema = serverBlockSchema as unknown as BlockSchema;

/** Renders `markdown` through the shared engine as a surface would, and answers the parsed
 *  document's plain text, or `undefined` where the engine answered nothing for it. */
function parseThroughSharedEngine(markdown: string): Promise<string | undefined> {
  const { promise, resolve } = Promise.withResolvers<string | undefined>();
  renderWithEngine(markdown, "space", ({ engine }) => {
    const doc = engine?.parse(markdown);
    resolve(doc === undefined ? undefined : doc.textBetween(0, doc.content.size, " ", " "));
  });
  return promise;
}

/**
 * What `parseThroughSharedEngine` must answer for `entry`, derived from the table's own reading
 * rather than written out again here. The helper joins the document's blocks with a space and
 * writes a space for every leaf; a paragraph's stored reading is that paragraph's own text with
 * runs of whitespace collapsed. For a document whose every block is a paragraph - a list item's
 * and a table cell's paragraph among them - and which holds no inline leaf, the two are the same
 * string, which is why the shapes below are the families' paragraph-only documents. The shapes
 * that hold a heading, a code block or a leaf are read against the table in
 * `fixture-table-readings.test.ts`, where a DOM says what each paragraph reads as.
 */
function wholeText(entry: PmdocFixture): string {
  return entry.reading.paragraphs.map((paragraph) => paragraph.text).join(" ");
}

/** The bodies the parses after a refused one carry: eight documents of the fixture table, one
 *  for each markdown shape this engine must keep reading, and two for the addresses - a list
 *  (`lists`), the tilde-beside-emphasis shapes (`emphasis-beside-a-tilde`), the spelling a
 *  writer's backslash forces (`writer-spells-a-flanked-letter-after-a-backslash`), the
 *  addresses two linkifiers argue over (`email-autolinks-in-a-spec`,
 *  `bare-addresses-each-with-its-own-host`), text shaped like a directive this schema has no
 *  block for (`directive-like-text`), the mark spans a document carries (`marks`) and text
 *  outside ASCII (`unicode`). */
const unrelatedBodies = [
  "lists",
  "emphasis-beside-a-tilde",
  "writer-spells-a-flanked-letter-after-a-backslash",
  "email-autolinks-in-a-spec",
  "bare-addresses-each-with-its-own-host",
  "directive-like-text",
  "marks",
  "unicode",
].map(fixture);

// LEGION-540. A body the schema has no node for throws, and that body falls back to its literal
// text. The throw must stop there: the next bodies through the same engine, on the same page,
// are unrelated text someone else wrote, and they render formatted. Before the headless editor
// dropped its refused parser, the one or two parses after a throw threw too, so a numbered list
// under a raw-HTML or mark-span turn in the live transcript read as `1. **Stop** …`. A span of a
// mark kind this schema does not carry still throws (reference-style links now resolve, so they
// no longer do), so it drives the recovery this test guards; removing the drop-on-throw in
// `@legion/proof-editor`'s headless parser fails the assertions after the refused parse.
test("a parse the engine refuses leaves the next parses of unrelated text intact", async () => {
  const load = spyOn(schemaModule, "loadBlockSchema").mockResolvedValue(blockSchema);
  try {
    const warm = fixture("emphasis");
    expect(await parseThroughSharedEngine(warm.markdown)).toBe(wholeText(warm));
    // A span of a mark kind this schema has no parser for: the parser throws mid-paragraph.
    expect(
      await parseThroughSharedEngine('see <span data-dispatch="bogus">this</span> now')
    ).toBeUndefined();
    for (const entry of unrelatedBodies) {
      expect(await parseThroughSharedEngine(entry.markdown), entry.name).toBe(wholeText(entry));
    }
  } finally {
    load.mockRestore();
  }
});

// The boundary the browser rows measure (`e2e/heavy-bodies.e2e.ts`, whose two shortest rows sit
// one character under it): a body of exactly `markdownWorkerThreshold` characters is parsed
// where every body always was, on the page's own thread, and one character more is parsed in the
// worker (sjawhar/legion#2519).
describe("the parse worker's threshold", () => {
  afterEach(restoreParseWorker);

  test("a body of exactly the threshold parses on the page, one character more in the worker", async () => {
    await warmMarkdownRenderer();
    const worker = installFakeParseWorker();
    // One paragraph of one word, so each body is exactly as long as the count under test.
    const atThreshold = "x".repeat(markdownWorkerThreshold);
    const overThreshold = "x".repeat(markdownWorkerThreshold + 1);

    const onThePage: MarkdownRendering[] = [];
    const cancelOnThePage = renderWithEngine(atThreshold, "space", (rendering) => {
      onThePage.push(rendering);
    });
    try {
      // The engine is loaded, so this body is answered in the caller's own commit, parsed, the
      // way every body was before the worker: nothing about it is pending.
      expect(onThePage).toHaveLength(1);
      expect(onThePage[0].pending).toBe(false);
      expect(onThePage[0].engine).toBeDefined();
      expect(onThePage[0].parsed?.textContent).toBe(atThreshold);
      await flushTasks();
      expect(worker.created).toBe(0);
      expect(worker.requests).toHaveLength(0);
    } finally {
      cancelOnThePage();
    }

    const inTheWorker: MarkdownRendering[] = [];
    const cancelInTheWorker = renderWithEngine(overThreshold, "space", (rendering) => {
      inTheWorker.push(rendering);
    });
    try {
      // One character more, and the body is answered at once as pending, with no document: the
      // literal text is what a surface paints until the worker answers.
      expect(inTheWorker).toHaveLength(1);
      expect(inTheWorker[0].pending).toBe(true);
      expect(inTheWorker[0].parsed).toBeUndefined();
      expect(inTheWorker[0].engine).toBeDefined();
      await worker.answer(0);
      await flushTasks();
      expect(worker.created).toBe(1);
      expect(worker.requests).toHaveLength(1);
      expect(worker.requests[0].markdown).toBe(overThreshold);
      const answered = inTheWorker.at(-1) as MarkdownRendering;
      expect(inTheWorker.length).toBeGreaterThan(1);
      expect(answered.pending).toBe(false);
      expect(answered.parsed?.textContent).toBe(overThreshold);
    } finally {
      cancelInTheWorker();
    }
  });
});
