/**
 * Which mark a press or a hover in the document names. Two readers' comments, suggestions or asks
 * may cover the same text (the three record marks declare `excludes: ''`), so their spans nest,
 * and which span is innermost follows the order the marks were made in rather than anything a
 * reader can see: a later comment or ask nests inside an earlier one, an earlier suggestion inside
 * a later one. So a press on text several marks cover names the narrowest of them, the record mark
 * whose range is the smallest, and a comment inside a wider one is always reachable from its own
 * text. Of marks the same size, the innermost span's wins. A mark that is not a record mark (a
 * proof review mark) is named only where no record mark covers the text (LEGION-458).
 */

import type { EditorView } from "@milkdown/kit/prose/view";
import { findRecordMark } from "./record-mark-retype";

const MARK_SELECTOR = "span[data-proof][data-id], span[data-dispatch][data-id]";

/** `target` as an element, or null for a target that is not one. */
function asElement(target: EventTarget | null): Element | null {
  return target !== null && typeof (target as Partial<Element>).closest === "function"
    ? (target as Element)
    : null;
}

/** The ids of the mark spans around `target` inside `view`'s editor, innermost first. */
function markIdsAround(view: EditorView, target: EventTarget | null): string[] {
  const ids: string[] = [];
  let span = asElement(target)?.closest<HTMLElement>(MARK_SELECTOR) ?? null;
  while (span !== null && view.dom.contains(span)) {
    if (span.dataset.id) ids.push(span.dataset.id);
    span = span.parentElement?.closest<HTMLElement>(MARK_SELECTOR) ?? null;
  }
  return ids;
}

/** How much of the document the record mark `markId` spans, from the start of its first run to
 *  the end of its last; infinite for a mark that is not a record mark. */
function rangeSize(view: EditorView, markId: string): number {
  const held = findRecordMark(view.state.doc, markId);
  return held === null ? Number.POSITIVE_INFINITY : held.range.to - held.range.from;
}

/** The id of the mark a press or a hover at `target` names: of the mark spans around it, the
 *  record mark whose range is the smallest, the innermost of those the same size. Null when no
 *  mark span inside the editor holds `target`. */
export function markIdAt(view: EditorView, target: EventTarget | null): string | null {
  const [innermost, ...outer] = markIdsAround(view, target);
  if (innermost === undefined) return null;
  if (outer.length === 0) return innermost;
  let narrowest = innermost;
  let smallest = rangeSize(view, innermost);
  for (const id of outer) {
    const size = rangeSize(view, id);
    if (size < smallest) {
      narrowest = id;
      smallest = size;
    }
  }
  return narrowest;
}
