import type { Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import { type EditorState, Plugin, PluginKey } from "@milkdown/kit/prose/state";
import { Decoration, DecorationSet, type EditorView } from "@milkdown/kit/prose/view";
import { $prose } from "@milkdown/kit/utils";

/** One run of a document's text drawn with a class. */
export interface TextDecoration {
  /** Index of the top-level block of the document the decoration sits in. */
  blockIndex: number;
  /** Character offsets into that block's own text; with neither, the whole block carries the class
   *  as a node decoration. */
  from?: number;
  to?: number;
  className: string;
}

/** A piece of text drawn beside a document without being part of it. */
export interface BlockWidget {
  /** The widget is drawn before the top-level block at this index; `doc.childCount` draws it at
   *  the end of the document. */
  blockIndex: number;
  /** Character offset into that block's own text to draw the widget at. With none, the widget is
   *  drawn before the whole block. */
  offset?: number;
  text: string;
  className: string;
}

interface ChangeDecorations {
  text: readonly TextDecoration[];
  widgets: readonly BlockWidget[];
  decorations: DecorationSet;
}

type ChangeDecorationsChange =
  | { kind: "text"; text: readonly TextDecoration[] }
  | { kind: "widgets"; widgets: readonly BlockWidget[] };

const changeDecorationsKey = new PluginKey<ChangeDecorations>("dispatch-editor-change-decorations");
const noChangeDecorations: ChangeDecorations = {
  text: [],
  widgets: [],
  decorations: DecorationSet.empty,
};

/** One top-level block of the document and the position it starts at. */
interface TopLevelBlock {
  node: ProseMirrorNode;
  start: number;
}

function topLevelBlocks(doc: ProseMirrorNode): TopLevelBlock[] {
  const blocks: TopLevelBlock[] = [];
  doc.forEach((node, offset) => {
    blocks.push({ node, start: offset });
  });
  return blocks;
}

/** Where one run of a block's own text sits: the offset into that text the run starts at, its
 *  length, and the document position of its first character. A block's text is what ProseMirror
 *  counts as its text content, so an inline atom standing between two runs takes positions without
 *  taking offsets, and so does the boundary between two of the block's own text blocks. */
interface TextRun {
  offset: number;
  length: number;
  position: number;
}

function textRuns(block: TopLevelBlock): TextRun[] {
  const runs: TextRun[] = [];
  let offset = 0;
  block.node.descendants((node, pos) => {
    if (!node.isText) return true;
    const length = node.text?.length ?? 0;
    // `descendants` counts from the start of the block's content, one position inside the block.
    runs.push({ offset, length, position: block.start + 1 + pos });
    offset += length;
    return false;
  });
  return runs;
}

/** The document position `offset` characters into the block's text, or `undefined` for an offset
 *  past its end. The position where one run ends and the next begins is two positions in the
 *  document whenever anything stands between them, so an offset there belongs to the run that
 *  begins at it while a decoration opens (`"from"`) and to the one that ends at it while a
 *  decoration closes (`"to"`): a mark over a word never reaches over the atom or the block
 *  boundary beside it. */
function positionAt(
  runs: readonly TextRun[],
  offset: number,
  side: "from" | "to"
): number | undefined {
  for (const run of runs) {
    const end = run.offset + run.length;
    if (side === "to" ? offset <= end : offset < end) return run.position + (offset - run.offset);
  }
  const last = runs.at(-1);
  // A `"from"` at the very end of the text: the last run's end is the only position it names.
  if (last !== undefined && offset === last.offset + last.length)
    return last.position + last.length;
  return undefined;
}

/** The position a widget is drawn at: the one its offset names inside the block's text, or the
 *  start of the whole block where it names none. `runsOf` is the walk of the block's inline
 *  content, shared with the rest of the set. */
function widgetPosition(
  doc: ProseMirrorNode,
  blocks: readonly TopLevelBlock[],
  widget: BlockWidget,
  runsOf: (blockIndex: number) => readonly TextRun[]
): number | undefined {
  const block = blocks[widget.blockIndex];
  if (widget.offset !== undefined) {
    if (block === undefined) return undefined;
    return positionAt(runsOf(widget.blockIndex), widget.offset, "from");
  }
  if (widget.blockIndex === blocks.length) return doc.content.size;
  return block?.start;
}

function widgetElement(view: EditorView, widget: BlockWidget): HTMLElement {
  // A widget drawn inside a block's text stands among that text, where only an inline element
  // belongs; one drawn before a whole block stands among the blocks.
  const tag = widget.offset === undefined ? "div" : "span";
  const element = view.dom.ownerDocument.createElement(tag);
  element.className = widget.className;
  element.textContent = widget.text;
  // The widget is drawn beside the document, not in it: a caret must not walk into it and a
  // redraw must not read it back as content.
  element.setAttribute("contenteditable", "false");
  return element;
}

/** The set the entries draw over `doc`. An entry naming a block the document does not hold, or
 *  offsets its text does not reach, names nothing to draw and is dropped.
 *
 *  Each block's inline content is walked once for the whole set, however many entries and
 *  widgets name it: a rewritten paragraph carries an entry for every run of words it gained and
 *  a widget for every run it lost, and the set is rebuilt on every change of the document. */
function changeDecorations(doc: ProseMirrorNode, value: ChangeDecorations): DecorationSet {
  if (value.text.length === 0 && value.widgets.length === 0) return DecorationSet.empty;
  const blocks = topLevelBlocks(doc);
  const walked = new Map<number, readonly TextRun[]>();
  /** The runs of the block at `blockIndex`, which the caller has already found in `blocks`. */
  const runsOf = (blockIndex: number): readonly TextRun[] => {
    const cached = walked.get(blockIndex);
    if (cached !== undefined) return cached;
    const runs = textRuns(blocks[blockIndex]);
    walked.set(blockIndex, runs);
    return runs;
  };
  const decorations: Decoration[] = [];
  for (const entry of value.text) {
    const block = blocks[entry.blockIndex];
    if (block === undefined) continue;
    if (entry.from === undefined || entry.to === undefined) {
      // Neither end is the whole block; one end alone names no range at all.
      if (entry.from === undefined && entry.to === undefined) {
        const to = block.start + block.node.nodeSize;
        decorations.push(Decoration.node(block.start, to, { class: entry.className }));
      }
      continue;
    }
    const runs = runsOf(entry.blockIndex);
    const from = positionAt(runs, entry.from, "from");
    const to = positionAt(runs, entry.to, "to");
    if (from === undefined || to === undefined || from >= to) continue;
    decorations.push(Decoration.inline(from, to, { class: entry.className }));
  }
  for (const widget of value.widgets) {
    const position = widgetPosition(doc, blocks, widget, runsOf);
    if (position === undefined) continue;
    decorations.push(
      Decoration.widget(position, (view) => widgetElement(view, widget), {
        ignoreSelection: true,
        side: -1,
      })
    );
  }
  return DecorationSet.create(doc, decorations);
}

function dispatchChange(view: EditorView, change: ChangeDecorationsChange): void {
  view.dispatch(view.state.tr.setMeta(changeDecorationsKey, change).setMeta("addToHistory", false));
}

/**
 * Presentation-only marks a host draws over a read-only document: the runs of text a change
 * touched, and the text it removed - a whole block, or a run of words inside one - drawn where it
 * stood. A host that writes classes into mark or block DOM makes ProseMirror read that DOM back
 * as a document edit, so these classes are decorations computed from editor state.
 *
 * The entries name a block by its index among the document's top-level blocks, a run by character
 * offsets into that block's text and a widget by the offset it stands at, so a host computes them
 * from text alone and never from document positions. The editor this serves is read-only and
 * `setMarkdown` replaces its whole document, so a document change recomputes the set from the
 * entries rather than mapping positions that no longer name the same text.
 */
export const editorChangeDecorationsPlugin = $prose(
  () =>
    new Plugin<ChangeDecorations>({
      key: changeDecorationsKey,
      state: {
        init: () => noChangeDecorations,
        apply(transaction, value) {
          const change = transaction.getMeta(changeDecorationsKey) as
            | ChangeDecorationsChange
            | undefined;
          if (change === undefined) {
            if (!transaction.docChanged) return value;
            if (value.text.length === 0 && value.widgets.length === 0) return value;
            return { ...value, decorations: changeDecorations(transaction.doc, value) };
          }
          const next: ChangeDecorations =
            change.kind === "text"
              ? { ...value, text: change.text }
              : { ...value, widgets: change.widgets };
          return { ...next, decorations: changeDecorations(transaction.doc, next) };
        },
      },
      props: {
        decorations: (state: EditorState) =>
          (changeDecorationsKey.getState(state) ?? noChangeDecorations).decorations,
      },
    })
);

/** Draws `decorations` over the document, in place of whatever was drawn before. */
export function setTextDecorations(view: EditorView, decorations: readonly TextDecoration[]): void {
  dispatchChange(view, { kind: "text", text: decorations });
}

/** Draws `widgets` beside the document, in place of whatever was drawn before. */
export function setBlockWidgets(view: EditorView, widgets: readonly BlockWidget[]): void {
  dispatchChange(view, { kind: "widgets", widgets });
}
