/**
 * The ground truth for Dispatch's answer filter (`packages/envoy/going/filter.go`), which clears
 * a model's text so no link the server did not give the model reaches a reader.
 *
 * The Go suite asserts the exact text of each probe and one list of marks no answer may decode to
 * (`noAutolink` in `filter_test.go`). A list names the forms it already knows, and a form it does
 * not name is what slipped two rounds of review; both of those were found by parsing the filter's
 * output with the markdown engine Dispatch renders with, not by reading the Go. So the filter's
 * output for every probe of that suite is committed as a golden
 * (`packages/envoy/going/testdata/filtered-answers.json`, written by
 * `packages/envoy/going/filter_golden_test.go`), and this file parses each answer with the plugin
 * list `src/lib-headless.ts` parses a body with and fails on a `link`, an `image` or an `html`
 * node in the tree.
 *
 * The tree, not HTML: the app never renders markdown to HTML. `createHeadlessProof` hands the
 * parsed mdast tree to Milkdown's `ParserState`, and no rehype package is installed here, so a
 * test that compiled HTML would assert on a pipeline no reader runs.
 *
 * `src/lib-headless.ts` builds its processor inside `createHeadlessProof` and exports neither the
 * processor nor its plugin list, so the list below is written out once more. It cannot drift
 * silently: the first case reads that file and fails when the two lists differ.
 */

import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { remarkProofMarks } from "proof-sdk-upstream/src/formats/remark-proof-marks.js";
import remarkFrontmatter from "remark-frontmatter";
import remarkGfm from "remark-gfm";
import remarkInlineLinks from "remark-inline-links";
import remarkParse from "remark-parse";
import { unified } from "unified";
import { remarkDispatchMarks } from "../src/dispatch-marks.js";
import { remarkSoftBreakAsSpace } from "../src/dispatch-soft-breaks.js";
import { remarkContainerDirectives } from "../src/lib-remark-directive-plugin.js";
import { remarkHoistNestedLinks } from "../src/nested-links.js";

/** The golden the Go suite writes: every probe it holds, and what the filter emitted for it. */
interface FilteredAnswer {
  /** The name the Go suite knows the probe by, which a failure here names back. */
  probe: string;
  /** The answer the model wrote. */
  answer: string;
  /** The whole text of every frame the filter emitted for it: what a reader is given. */
  filtered: string;
}

interface FilteredAnswers {
  generator: string;
  answers: FilteredAnswer[];
}

/** As much of an mdast node as this walk reads. */
interface TreeNode {
  type: string;
  url?: string;
  value?: string;
  children?: TreeNode[];
}

/**
 * The node types a reader follows or a page fetches: a link, an image, and raw HTML, which
 * carries either. Everything else is text to a reader.
 */
const LINKING: Record<string, true> = { link: true, image: true, html: true };

/**
 * The plugins `src/lib-headless.ts`'s `buildParser` installs, in its order, up to the soft-break
 * policy. The two it installs after that policy (`remarkTypedBlocks` with `remarkUnfitTypedBlocks`,
 * or `remarkReadOnlyDirectives`) shape typed blocks against a block schema and a ProseMirror
 * schema; they make no link, and they need an engine, so the parse here ends at the policy.
 *
 * The guard below compares these names, in this order, and not the options each plugin is
 * installed with. That is the whole of what can move this parse away from the app's: the chain
 * in `lib-headless.ts` is one closed list of `.use(...)` calls, the only one of them that takes
 * an option there is `remarkFrontmatter`'s `['yaml']`, and `remarkGfm` is installed with none at
 * all - none of its options (`firstLineBlank`, `stringLength`, `singleTilde`, `tablePipeAlign`,
 * `tableCellPadding`) touches an autolink literal, which is what this file is about.
 */
const APP_PARSE_PLUGINS = [
  "remarkParse",
  "remarkFrontmatter",
  "remarkGfm",
  "remarkContainerDirectives",
  "remarkProofMarks",
  "remarkDispatchMarks",
  "remarkInlineLinks",
  "remarkHoistNestedLinks",
];

/** Where that list starts in `src/lib-headless.ts`, and the line that ends it. */
const CHAIN_OPENS = "const parseProcessor = unified()";
const SOFT_BREAK_LINE =
  "parseProcessor.use(softBreaks === 'line' ? remarkSoftBreakAsLine : remarkSoftBreakAsSpace);";

/**
 * The document default, `'space'`: a single newline inside a paragraph is a space. The live view's
 * `'line'` policy differs only in what it makes of that newline, which links nothing.
 */
const answerParser = unified()
  .use(remarkParse)
  .use(remarkFrontmatter, ["yaml"])
  .use(remarkGfm)
  .use(remarkContainerDirectives)
  .use(remarkProofMarks)
  .use(remarkDispatchMarks)
  .use(remarkInlineLinks)
  .use(remarkHoistNestedLinks)
  .use(remarkSoftBreakAsSpace);

/** The renderer this file is the ground truth of, named from the `remark-gfm` the install
 *  resolved and not from the range `package.json` pins (`^4.0.1`), which names no one version:
 *  a failure here names the renderer that actually read the probe. */
const rendererManifest = JSON.parse(
  readFileSync(join(import.meta.dir, "../node_modules/remark-gfm/package.json"), "utf8")
) as { version: string };
const renderer = `remark-gfm ${rendererManifest.version}`;

const fixturePath = join(import.meta.dir, "../../envoy/going/testdata/filtered-answers.json");
const fixture = JSON.parse(readFileSync(fixturePath, "utf8")) as FilteredAnswers;

/** Every link, image and raw tag the parse of `markdown` holds, each named with its target. */
function linkingNodes(markdown: string): string[] {
  const tree = answerParser.runSync(answerParser.parse(markdown)) as TreeNode;
  const found: string[] = [];
  const walk = (node: TreeNode): void => {
    if (LINKING[node.type]) found.push(`${node.type}:${node.url ?? node.value ?? ""}`);
    for (const child of node.children ?? []) walk(child);
  };
  walk(tree);
  return found;
}

test("the plugin list here is the one src/lib-headless.ts parses a body with", () => {
  const source = readFileSync(join(import.meta.dir, "../src/lib-headless.ts"), "utf8");
  const opens = source.indexOf(CHAIN_OPENS);
  expect(opens).toBeGreaterThanOrEqual(0);
  const closes = source.indexOf(SOFT_BREAK_LINE, opens);
  expect(closes).toBeGreaterThan(opens);
  const used = [...source.slice(opens, closes).matchAll(/\.use\((\w+)/g)].map((match) => match[1]);
  expect(used).toEqual(APP_PARSE_PLUGINS);
});

test("the golden holds the Go suite's probes", () => {
  expect(fixture.answers.length).toBeGreaterThan(0);
  expect(fixture.generator).toContain("filter_golden_test.go");
});

for (const { probe, answer, filtered } of fixture.answers) {
  test(`a reader is given no link: ${probe}`, () => {
    const found = linkingNodes(filtered);
    if (found.length > 0) {
      throw new Error(
        `${renderer} reads ${found.join(", ")} out of the probe ${JSON.stringify(probe)}:\n` +
          `  the model wrote    ${JSON.stringify(answer)}\n` +
          `  the filter emitted ${JSON.stringify(filtered)}\n` +
          "Regenerate the golden with `go test ./going " +
          "-run TestTheFilteredAnswersAreWhatTheFilterEmits -update` in packages/envoy after a " +
          "filter change."
      );
    }
  });
}
