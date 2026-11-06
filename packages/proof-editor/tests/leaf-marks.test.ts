import { expect, test } from "bun:test";
import type { Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import { createHeadlessProof } from "../src/lib-headless.js";
import { withMarksEditor } from "./marks-editor";
import { pastePlainText, withHTMLElementGlobal } from "./paste";

/**
 * Two readings this editor's engine shares with Go's reader of the same document, asserted at
 * each of the engine's three parse seams. A row whose reading this rule changed names the reading
 * it had before this rule beside it, as the history it is.
 *
 * - An inline *leaf* node - `html`, `image`, `hardbreak`, `footnote_reference` - carries no
 *   mark, whatever emphasis, strong, strikethrough or link is wrapped around it, as Go's schema
 *   holds marks on text alone and this editor's own collaborative store drops them on the way to
 *   a peer. Every text node keeps exactly the marks it always had.
 * - An autolink inside a link label carries its own link alone, and the label's link covers both
 *   ordinary runs: in `[a <https://u.test> b](u)` that label's link covers `a ` and ` b`, and the
 *   autolink's own link covers its text.
 *
 * Each seam builds its own parser - the headless one, the editor's shared `parserCtx` that every
 * text/plain paste runs through, and the `ParserState` `setMarkdown` builds for a markdown import
 * - so each is asserted separately, and a failing test's name says which one reads a row wrong.
 */

/** The attribute that identifies an inline leaf node, which holds no text of its own. */
const LEAF_IDENTITY: Record<string, string | undefined> = {
  footnote_reference: "label",
  html: "value",
  image: "src",
};

interface InlineReading {
  /** Mark types in schema order; a link mark as `link(<href>)`, since the href is what moves. */
  readonly marks: readonly string[];
  /** A text node's text, or what identifies the leaf (an image's `src`, and so on). */
  readonly text: string;
  readonly type: string;
}

/** What a reading holds for one text run. */
function textRun(value: string, ...marks: string[]): InlineReading {
  return { marks, text: value, type: "text" };
}

/** What a reading holds for one inline leaf: the rule leaves it no mark, whatever wraps it. */
function leaf(type: string, identity: string): InlineReading {
  return { marks: [], text: identity, type };
}

function link(href: string): string {
  return `link(${href})`;
}

/** A node's own content: its text, or the attribute that identifies the leaf. */
function contentOf(node: ProseMirrorNode): string {
  if (node.isText) return node.text ?? "";
  const identity = LEAF_IDENTITY[node.type.name];
  return identity === undefined ? "" : String(node.attrs[identity]);
}

/** Every inline node of `doc`, in document order, as the plain values asserted below. */
function inlineReading(doc: ProseMirrorNode): InlineReading[] {
  const reading: InlineReading[] = [];
  doc.descendants((node) => {
    if (!node.isInline) return true;
    reading.push({
      marks: node.marks.map((mark) =>
        mark.type.name === "link" ? link(String(mark.attrs.href)) : mark.type.name
      ),
      text: contentOf(node),
      type: node.type.name,
    });
    return false;
  });
  return reading;
}

interface Row {
  readonly markdown: string;
  readonly name: string;
  readonly reading: readonly InlineReading[];
}

/** The rows every seam reads this way; each of them read otherwise before this rule. */
const CHANGED_ROWS: readonly Row[] = [
  {
    // Before this rule: text `a` [strong], html `<b>` [strong], text `c` [strong].
    markdown: "**a<b>c**",
    name: "an inline html node inside strong",
    reading: [textRun("a", "strong"), leaf("html", "<b>"), textRun("c", "strong")],
  },
  {
    // The image is the paragraph's only inline node, and carries neither of the two marks
    // wrapped around it: before this rule it read with both `strong` and the label's link.
    markdown: "**[![x](i.png)](https://u.test)**",
    name: "an image inside a link inside strong",
    reading: [leaf("image", "i.png")],
  },
  {
    // The second run is the definition's own text, which this rule leaves alone.
    markdown: "*[^d]*\n\n[^d]: note",
    name: "a footnote reference inside emphasis",
    reading: [leaf("footnote_reference", "d"), textRun("note")],
  },
  {
    // `a`, a backslash, a line ending, `b`, all wrapped in `**`: a backslash hard break.
    markdown: "**a\\\nb**",
    name: "a hard break inside strong",
    reading: [textRun("a", "strong"), leaf("hardbreak", ""), textRun("b", "strong")],
  },
  {
    // Not a rule about `strong`: strikethrough and a link mark leave an inline leaf too.
    markdown: "~~<b>~~",
    name: "an inline html node inside strikethrough",
    reading: [leaf("html", "<b>")],
  },
  {
    markdown: "[<b>](u)",
    name: "an inline html node inside a link",
    reading: [leaf("html", "<b>")],
  },
  {
    // Before this rule the last run carried no mark at all: the label's link ended at the
    // autolink.
    markdown: "[a <https://u.test> b](u)",
    name: "an autolink inside a link label",
    reading: [
      textRun("a ", link("u")),
      textRun("https://u.test", link("https://u.test")),
      textRun(" b", link("u")),
    ],
  },
  {
    // The whole label is the autolink: one run, carrying the autolink's own link alone, before
    // this rule as after it.
    markdown: "[<mailto:a@b.test>](u)",
    name: "an autolink alone in a link label",
    reading: [textRun("mailto:a@b.test", link("mailto:a@b.test"))],
  },
  {
    // The same three runs, each also inside the `**`: before this rule the last one kept
    // `strong` only.
    markdown: "**[a <https://u.test> b](u)**",
    name: "an autolink inside a link label inside strong",
    reading: [
      textRun("a ", "strong", link("u")),
      textRun("https://u.test", "strong", link("https://u.test")),
      textRun(" b", "strong", link("u")),
    ],
  },
];

/** Readings neither change touches: marks on text, and a link label holding no autolink. */
const CONTROL_ROWS: readonly Row[] = [
  {
    markdown: "**a `b` c**",
    name: "a code span inside strong",
    reading: [
      textRun("a ", "strong"),
      textRun("b", "strong", "inlineCode"),
      textRun(" c", "strong"),
    ],
  },
  {
    markdown: "[plain](u)",
    name: "a link over plain text",
    reading: [textRun("plain", link("u"))],
  },
];

/** Seam 1: every headless consumer - comments, previews, the fixture generator. */
async function readHeadless(markdown: string): Promise<InlineReading[]> {
  const { parseMarkdown } = await createHeadlessProof();
  return inlineReading(parseMarkdown(markdown));
}

/**
 * Seam 2: the editor's shared `parserCtx`, reached the way a reader reaches it - a text/plain
 * paste into an empty document, through the real clipboard plugin's `handlePaste`. The paste's
 * DOM round trip hits `parseDOM` rules that test `dom instanceof HTMLElement` (a link's and a
 * footnote reference's among them), so every one of these pastes needs `withHTMLElementGlobal`.
 */
async function readPastedPlainText(markdown: string): Promise<InlineReading[]> {
  let reading: InlineReading[] = [];
  await withMarksEditor("", ({ view }) => {
    withHTMLElementGlobal(() => pastePlainText(view, markdown));
    reading = inlineReading(view.state.doc);
  });
  return reading;
}

/** Seam 3: the `ParserState` `setMarkdown` builds for an import - the past-version view's. */
async function readSetMarkdown(markdown: string): Promise<InlineReading[]> {
  let reading: InlineReading[] = [];
  await withMarksEditor("", ({ handle, view }) => {
    handle.setMarkdown(markdown);
    reading = inlineReading(view.state.doc);
  });
  return reading;
}

/** Asserts each row's whole reading, so a wrong mark anywhere in a row fails it by name. */
async function expectRows(
  rows: readonly Row[],
  read: (markdown: string) => Promise<readonly InlineReading[]>
): Promise<void> {
  for (const row of rows) {
    expect(await read(row.markdown), row.name).toEqual(row.reading);
  }
}

test("seam 1 (headless parseMarkdown): no inline leaf carries a mark, and a label's link survives an autolink", async () => {
  await expectRows(CHANGED_ROWS, readHeadless);
});

test("seam 1 (headless parseMarkdown) control: a code span inside strong and a plain link read unchanged", async () => {
  await expectRows(CONTROL_ROWS, readHeadless);
});

test("seam 2 (shared parserCtx, text/plain paste): no inline leaf carries a mark, and a label's link survives an autolink", async () => {
  await expectRows(CHANGED_ROWS, readPastedPlainText);
});

test("seam 2 (shared parserCtx, text/plain paste) control: a code span inside strong and a plain link read unchanged", async () => {
  await expectRows(CONTROL_ROWS, readPastedPlainText);
});

test("seam 3 (ProofEditorHandle.setMarkdown): no inline leaf carries a mark, and a label's link survives an autolink", async () => {
  await expectRows(CHANGED_ROWS, readSetMarkdown);
});

test("seam 3 (ProofEditorHandle.setMarkdown) control: a code span inside strong and a plain link read unchanged", async () => {
  await expectRows(CONTROL_ROWS, readSetMarkdown);
});
