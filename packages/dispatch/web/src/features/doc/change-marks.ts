import type { BlockWidget, TextDecoration } from "@legion/proof-editor";
import type { Node as ProseMirrorNode } from "prosemirror-model";

import { loadedEngine } from "../refs/markdown-engine";
import { changedWords, pairBlocks } from "./change-set";

/** The whole block a change touched. It marks the block without hiding any of its text. */
export const CHANGED_BLOCK_CLASS = "dispatch-change-block";
/** A run of words a changed block gained since the baseline. */
export const ADDED_WORDS_CLASS = "dispatch-change-added";
/** The text of a block removed since the baseline, drawn struck through where it stood. */
export const REMOVED_BLOCK_CLASS = "dispatch-change-removed";
/** A run of words a changed block lost since the baseline, drawn struck through where it stood
 *  inside the block's surviving text. */
export const REMOVED_WORDS_CLASS = "dispatch-change-removed-words";

/** What a reader sees of a change: the marks drawn over the rendered document's own text, and the
 *  text it lost - a whole block, or a run of words inside one - drawn beside it. */
export interface ChangeMarks {
  text: TextDecoration[];
  widgets: BlockWidget[];
}

/** Nothing marked: each caller owns the lists it is handed. */
function noMarks(): ChangeMarks {
  return { text: [], widgets: [] };
}

/** The text of each top-level block of `doc`, in the units the editor counts its own text in. */
function blockTexts(doc: ProseMirrorNode): string[] {
  const texts: string[] = [];
  doc.forEach((block) => {
    texts.push(block.textContent);
  });
  return texts;
}

/**
 * What changed in `doc`, the editor's own rendered document, since the version `baselineMarkdown`
 * holds, as the two decoration lists `ProofEditorHandle` draws: the words a changed block gained
 * marked inside its rendered text, the words it lost drawn beside them, and each block the change
 * removed drawn where it stood.
 *
 * Both sides are read as rendered text rather than markdown source - `doc` is the editor's own
 * document, and the baseline is parsed by the same headless engine every Markdown surface reads
 * with - so an offset here is an offset into the text the editor counts, which is what the
 * decorations name. An index the rendered side returns always names a block `doc` holds.
 *
 * A removed word run of a changed block is drawn as a widget rather than a mark: the rendered
 * block no longer holds those characters, so there is no range in it to draw them over, and a
 * widget is how the editor draws text a document does not hold. It stands at the offset the run
 * was removed at, which is where the surviving text closed over it, and carries
 * `REMOVED_WORDS_CLASS`; the whole block carries `CHANGED_BLOCK_CLASS` as well, which is what
 * tells the reader to compare it.
 *
 * Nothing is marked for an absent or equal baseline, or for one the engine refuses: a reader who
 * cannot be told what changed is shown the document itself rather than a guess.
 */
export function changeMarksAgainst(baselineMarkdown: string, doc: ProseMirrorNode): ChangeMarks {
  if (baselineMarkdown === "") return noMarks();
  const baseline = loadedEngine()?.parse(baselineMarkdown);
  if (baseline === undefined) return noMarks();

  const before = blockTexts(baseline);
  const rendered = blockTexts(doc);
  const pairs = pairBlocks(before, rendered);
  const text: TextDecoration[] = [];
  const widgets: BlockWidget[] = [];
  /** The text of each block removed since the baseline that no surviving block follows yet. */
  let pendingRemovals: string[] = [];
  const drawRemovals = (blockIndex: number): void => {
    for (const removed of pendingRemovals) {
      widgets.push({ blockIndex, className: REMOVED_BLOCK_CLASS, text: removed });
    }
    pendingRemovals = [];
  };

  for (const pair of pairs) {
    if (pair.after !== undefined) drawRemovals(pair.after);
    if (pair.kind === "removed" && pair.before !== undefined) {
      pendingRemovals.push(before[pair.before]);
      continue;
    }
    if (pair.kind !== "changed" || pair.before === undefined || pair.after === undefined) continue;
    const blockIndex = pair.after;
    text.push({ blockIndex, className: CHANGED_BLOCK_CLASS });
    let offset = 0;
    for (const part of changedWords(before[pair.before], rendered[blockIndex])) {
      if (part.kind === "removed") {
        widgets.push({ blockIndex, className: REMOVED_WORDS_CLASS, offset, text: part.value });
        continue;
      }
      if (part.kind === "added") {
        text.push({
          blockIndex,
          className: ADDED_WORDS_CLASS,
          range: { from: offset, to: offset + part.value.length },
        });
      }
      offset += part.value.length;
    }
  }
  // A block removed after the last surviving one stood at the end of the document.
  drawRemovals(doc.childCount);
  return { text, widgets };
}
