import type { ItemBlock } from "../../api/types";

/**
 * How the comment editor carries each block's id through an edit. The blocks route gives each
 * block's span in the stored body (`spansFromBlocks` moves it into the textarea's value), every
 * edit of the value moves the spans (`mapSpans`), and Save sends where each span starts in the
 * body it writes (`carriedBlocks`). The editor never splits the body: the server splits the body
 * it gets, and the block holding a carried start keeps that start's id. Offsets throughout are
 * UTF-16 code units, a JavaScript string index.
 */

/** The part of the textarea's value one edit replaced, as offsets into the value before it
 *  (the body field's `beforeinput` listener, `bodyFieldRef` in `MentionComposer`, records it from
 *  the selection). */
export interface TextEditRange {
  readonly end: number;
  readonly start: number;
}

/** A block of the comment as the editor follows it: its id and where its text lies in the
 *  textarea's value. */
export interface BlockSpan {
  readonly end: number;
  readonly id: string;
  readonly start: number;
}

/** A block whose whole text a cut or a drag took out of the value: its id and text, and the whole
 *  text that cut or drag removed (`cut`) with where the block's text sits in it (`offset`), so a
 *  paste or a drop of that text puts the block back. */
export interface HeldSpan {
  readonly cut: string;
  readonly id: string;
  readonly offset: number;
  readonly text: string;
}

/** The spans of the blocks still in the value, in value order, and those the last cut or drag
 *  holds. */
export interface BlockSpans {
  readonly held: readonly HeldSpan[];
  readonly live: readonly BlockSpan[];
}

/** An edit's spans as the composer keeps them: the comment's revision the blocks route answered,
 *  the spans, and the value they are offsets into. */
export interface FollowedBlocks {
  readonly revision: number;
  readonly spans: BlockSpans;
  readonly value: string;
}

/** One carried id, with where its block's span starts in the body Save sends. */
export interface CarriedBlock {
  readonly from: number;
  readonly id: string;
}

/** The value a textarea holds for `body`: the textarea reads each CR LF, and each lone CR, as LF. */
export function textareaValue(body: string): string {
  return body.replace(/\r\n?/g, "\n");
}

/**
 * Each block's span, moved from the stored body into `value`, the textarea's value for it
 * (`textareaValue`): a CR LF before an offset is one character fewer in the value, and a lone CR
 * is LF, the same length. Undefined when the blocks are not this body's - `value` is not the
 * textarea's value for `storedBody`, or a block's span does not hold its text, as when the comment
 * changed between the editor's body and the blocks read - so the edit carries no ids.
 */
export function spansFromBlocks(
  storedBody: string,
  value: string,
  blocks: readonly Pick<ItemBlock, "from" | "id" | "text" | "to">[]
): BlockSpans | undefined {
  if (textareaValue(storedBody) !== value) return undefined;
  // A CR LF pair the stored body holds before an offset is one character fewer in the value; a CR
  // just before the offset, its LF past it, is LF there, the same length.
  const valueOffset = (offset: number) => textareaValue(storedBody.slice(0, offset)).length;
  const live: BlockSpan[] = [];
  for (const block of blocks) {
    if (
      block.from < 0 ||
      block.from >= block.to ||
      block.to > storedBody.length ||
      storedBody.slice(block.from, block.to) !== block.text
    ) {
      return undefined;
    }
    live.push({ end: valueOffset(block.to), id: block.id, start: valueOffset(block.from) });
  }
  return { held: [], live };
}

/** `range` when it explains how `before` became `after` (only the text in it changed); else the
 *  one range a diff finds, the longest common prefix and then the longest common suffix. A range
 *  recorded from a collapsed caret can miss what the browser deleted - a whole surrogate pair, a
 *  word - and an edit with no `beforeinput` records none. */
function editRange(before: string, after: string, range: TextEditRange | undefined): TextEditRange {
  if (
    range !== undefined &&
    range.start >= 0 &&
    range.start <= range.end &&
    range.end <= before.length &&
    after.length >= range.start + before.length - range.end &&
    after.startsWith(before.slice(0, range.start)) &&
    after.endsWith(before.slice(range.end))
  ) {
    return range;
  }
  const common = Math.min(before.length, after.length);
  let start = 0;
  while (start < common && before[start] === after[start]) start += 1;
  let kept = 0;
  while (
    kept < common - start &&
    before[before.length - 1 - kept] === after[after.length - 1 - kept]
  ) {
    kept += 1;
  }
  return { end: before.length - kept, start };
}

/**
 * The spans after one edit of the textarea's value, `before` to `after`, which replaced `range`
 * of `before` (a diff finds it when `range` is missing or does not explain the edit):
 *
 * - A span after the edit moves with it, as does a span the edit inserts text right before: the
 *   inserted text goes ahead of the block's start, so a paragraph typed above a block is not it.
 * - Text inserted inside a span, or at its end with no line break, grows it. Enter in the middle
 *   splits the text, and the span keeps its start: the server gives the id to the part holding it.
 * - An edit over a span's start but not its end leaves the span starting where the edit ends; one
 *   over its end leaves it ending there. Joining two blocks (Backspace across the boundary) leaves
 *   both starts in one block, and the server keeps the first.
 * - An edit that replaces the whole text of one span, and of no other, with new words keeps the
 *   span over them: a block whose text is selected and typed over is reworded, as the server's
 *   text rules read it too. Any other edit over a span's whole text removes it, except a cut
 *   (`deleteByCut`) or a drag (`deleteByDrag`), which holds it - the newest cut or drag holds the
 *   only held spans, as the clipboard holds only the newest cut. A paste (`insertFromPaste`) or a
 *   drop (`insertFromDrop`) holding a held span's text puts the span back where the text lands.
 */
export function mapSpans(
  spans: BlockSpans,
  before: string,
  after: string,
  range: TextEditRange | undefined,
  inputType: string | undefined
): BlockSpans {
  const { end, start } = editRange(before, after, range);
  const delta = after.length - before.length;
  const insertedEnd = end + delta;
  const inserted = after.slice(start, insertedEnd);
  const holding = inputType === "deleteByCut" || inputType === "deleteByDrag";
  const taken: HeldSpan[] = [];
  const live: BlockSpan[] = [];
  const covered = spans.live.filter((span) => start <= span.start && end >= span.end);
  const words = inserted.trim();
  const reworded = !holding && covered.length === 1 && words !== "" ? covered[0] : undefined;
  for (const span of spans.live) {
    if (start === end && start === span.end && !inserted.includes("\n")) {
      live.push({ ...span, end: span.end + delta });
    } else if (end <= span.start) {
      live.push({ ...span, end: span.end + delta, start: span.start + delta });
    } else if (start >= span.end) {
      live.push(span);
    } else if (start <= span.start && end >= span.end) {
      if (span === reworded) {
        const wordsStart = start + inserted.indexOf(words);
        live.push({ ...span, end: wordsStart + words.length, start: wordsStart });
      } else if (holding) {
        taken.push({
          cut: before.slice(start, end),
          id: span.id,
          offset: span.start - start,
          text: before.slice(span.start, span.end),
        });
      }
    } else if (start <= span.start) {
      live.push({ ...span, end: span.end + delta, start: insertedEnd });
    } else {
      live.push({ ...span, end: end <= span.end ? span.end + delta : insertedEnd });
    }
  }
  let held = holding ? taken : spans.held;
  if ((inputType === "insertFromPaste" || inputType === "insertFromDrop") && inserted !== "") {
    const stillHeld: HeldSpan[] = [];
    for (const span of held) {
      const at = inserted === span.cut ? span.offset : inserted.indexOf(span.text);
      if (at < 0 || live.some((other) => other.id === span.id)) {
        stillHeld.push(span);
        continue;
      }
      live.push({ end: start + at + span.text.length, id: span.id, start: start + at });
    }
    held = stillHeld;
  }
  live.sort((left, right) => left.start - right.start);
  return { held, live };
}

/** The composer's followed blocks once the field holds `body` (`mapSpans` from the value they
 *  were offsets into); none while there are none to follow. */
export function followEdit(
  followed: FollowedBlocks | undefined,
  body: string,
  range: TextEditRange | undefined,
  inputType: string | undefined
): FollowedBlocks | undefined {
  if (followed === undefined) return undefined;
  const value = textareaValue(body);
  return {
    ...followed,
    spans: mapSpans(followed.spans, followed.value, value, range, inputType),
    value,
  };
}

/**
 * The ids Save carries, each with where its span starts in `trimmed`, the body it sends: `value`
 * (the textarea's value the spans are offsets into) with its whitespace trimmed, so each start
 * moves back by the leading whitespace. A span starting outside `trimmed` is dropped, as is a
 * span starting where an earlier one does (the server would keep the first); the rest go in
 * increasing order, each id once. A start inside a surrogate pair moves to the pair's start, since
 * the server refuses an offset inside a character. Undefined when `trimmed` is not `value`
 * trimmed: the spans are not offsets into what Save sends, so it carries no ids.
 */
export function carriedBlocks(
  spans: BlockSpans,
  value: string,
  trimmed: string
): CarriedBlock[] | undefined {
  if (value.trim() !== trimmed) return undefined;
  const lead = value.length - value.trimStart().length;
  const named = new Set<string>();
  const carried: CarriedBlock[] = [];
  for (const span of [...spans.live].sort((left, right) => left.start - right.start)) {
    let from = span.start - lead;
    // A character outside the Basic Multilingual Plane starts at from - 1: from is inside it.
    if (from > 0 && (trimmed.codePointAt(from - 1) ?? 0) > 0xffff) from -= 1;
    const last = carried.at(-1);
    if (
      from < 0 ||
      from >= trimmed.length ||
      named.has(span.id) ||
      (last !== undefined && from <= last.from)
    ) {
      continue;
    }
    named.add(span.id);
    carried.push({ from, id: span.id });
  }
  return carried;
}
