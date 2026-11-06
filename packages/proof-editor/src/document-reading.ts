/**
 * What a reader sees of a parsed document: each paragraph's text with the text of the emphasis,
 * strong and strikethrough elements in it, and each table's cells. One definition, read by the Go
 * reader's fixture table (`packages/envoy/internal/dispatch/pmdoc/testdata/fixtures.json`, whose
 * generator stores a `reading` per corpus file), by Dispatch's web unit tests and by one browser
 * row per parity family, so the three cannot drift apart on what a document reads as.
 *
 * The reading is taken from the DOM the schema draws, through ProseMirror's own `DOMSerializer`,
 * and never from a walk over the document's nodes. Two things a walk cannot know decide it: the
 * element a mark draws is that mark's own `toDOM` answer, which only the schema holds, and where
 * one run of a mark is cut into several elements follows the serializer's mark ordering, not the
 * node's mark set. Measured over the corpus, a walk disagrees with the serializer three ways: on
 * `marks-on-hard-breaks.md`, `paragraphs.md` and `softbreak.md`, where a `<br>` adds nothing to
 * `textContent` while the `hardbreak` node's `leafText` is a line feed; on
 * `links-holding-an-autolink.md`, where the serializer draws one `<strong>` over text the walk
 * reads as three runs, and on `writer-spells-a-flanked-letter-after-a-backslash.md`, where it
 * draws one run of strikethrough as three `<del>` elements because a link mark ranks between
 * them; and on every document holding a footnote reference, whose `<sup>` label the schema draws
 * and no node carries.
 */

import { DOMSerializer, type Node as ProseMirrorNode } from "prosemirror-model";

/** One paragraph of a reading: its whole text, with runs of whitespace collapsed, and the text of
 *  each `<em>`, `<strong>` and `<del>` in it, in document order. */
export interface ParagraphReading {
  emphasized: string[];
  strong: string[];
  struck: string[];
  text: string;
}

export interface DocumentReading {
  /** Each table's rows, as the text of their cells. */
  tables: string[][][];
  paragraphs: ParagraphReading[];
}

/** The text of every `selector` element inside `element`, in document order. */
function texts(element: Element, selector: string): string[] {
  return [...element.querySelectorAll(selector)].map((found) => found.textContent ?? "");
}

/**
 * `doc`'s reading, drawn into `document`: the caller's document, so a page reads its own DOM and a
 * command-line reader reads a happy-dom one. Nothing is mounted — the fragment is serialized into
 * a detached element of that document and read back out of it. A caller outside a page installs
 * that document as `globalThis.document` as well, because the commonmark preset's inline `html`
 * node draws its element from the global one rather than from the document its `toDOM` is handed.
 */
export function readDocument(doc: ProseMirrorNode, document: Document): DocumentReading {
  const root = document.createElement("div");
  root.appendChild(
    DOMSerializer.fromSchema(doc.type.schema).serializeFragment(doc.content, { document })
  );
  return {
    tables: [...root.querySelectorAll("table")].map((table) =>
      [...table.querySelectorAll("tr")].map((row) => texts(row, "th, td"))
    ),
    paragraphs: [...root.querySelectorAll("p")].map((paragraph) => ({
      emphasized: texts(paragraph, "em"),
      strong: texts(paragraph, "strong"),
      struck: texts(paragraph, "del"),
      text: (paragraph.textContent ?? "").replace(/\s+/g, " ").trim(),
    })),
  };
}
