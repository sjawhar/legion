import type { Anchor, AnchorBox, Comment } from "../../api/types";
import { compareTimestamps } from "../../lib/timestamps";

/** A pointer position, in the page's CSS pixels (a pointer event's `clientX`/`clientY`). */
export interface Point {
  readonly x: number;
  readonly y: number;
}

/** Where the picture is drawn, as `getBoundingClientRect` gives it. */
export interface PictureRect {
  readonly left: number;
  readonly top: number;
  readonly width: number;
  readonly height: number;
}

/** The shortest drag, in CSS pixels across and down, that draws a box; a shorter one is a click. */
export const MIN_BOX_PIXELS = 4;

/** Fractions are kept to four places: a ten-thousandth of the picture is finer than any pixel of
 *  a picture Dispatch stores, and the stored box reads as the numbers `dispatch comment --box`
 *  takes. */
const PLACES = 10_000;

function round(value: number): number {
  return Math.round(value * PLACES) / PLACES;
}

function clamp(value: number, low: number, high: number): number {
  return Math.min(Math.max(value, low), high);
}

/**
 * The part of the picture a drag from `start` to `end` covers, as fractions of the picture's width
 * and height from its top-left corner, whichever corner the drag started from. A drag that leaves
 * the picture stops at its edge; one shorter than MIN_BOX_PIXELS across or down draws no box.
 */
export function boxFromDrag(start: Point, end: Point, rect: PictureRect): AnchorBox | undefined {
  if (rect.width <= 0 || rect.height <= 0) return undefined;
  const right = rect.left + rect.width;
  const bottom = rect.top + rect.height;
  const x0 = clamp(Math.min(start.x, end.x), rect.left, right);
  const x1 = clamp(Math.max(start.x, end.x), rect.left, right);
  const y0 = clamp(Math.min(start.y, end.y), rect.top, bottom);
  const y1 = clamp(Math.max(start.y, end.y), rect.top, bottom);
  if (x1 - x0 < MIN_BOX_PIXELS || y1 - y0 < MIN_BOX_PIXELS) return undefined;
  const left = round((x0 - rect.left) / rect.width);
  const top = round((y0 - rect.top) / rect.height);
  // The far sides are rounded on their own and the size taken between them, so a box drawn to the
  // edge ends at 1 rather than a rounding error past it.
  const farRight = round((x1 - rect.left) / rect.width);
  const farBottom = round((y1 - rect.top) / rect.height);
  return { left, top, width: round(farRight - left), height: round(farBottom - top) };
}

/** A box's place over the picture as CSS percentages, so it follows the picture at any size. */
export function boxStyle(box: AnchorBox): {
  left: string;
  top: string;
  width: string;
  height: string;
} {
  const percent = (value: number) => `${round(value * 100)}%`;
  return {
    left: percent(box.left),
    top: percent(box.top),
    width: percent(box.width),
    height: percent(box.height),
  };
}

/** Whether `anchor` names an uploaded file, or part of a picture, rather than a passage of a
 *  document. */
export function isFileAnchor(
  anchor: Anchor | null | undefined
): anchor is Anchor & { readonly kind: "file" } {
  return anchor?.kind === "file";
}

/**
 * The number each box thread on `artifactId` shows over the picture and on its card: the threads
 * whose anchor holds a box, numbered from 1 in the order they were written, resolved ones and those
 * on other versions included, so resolving or uploading never renumbers a box.
 */
export function fileBoxNumbers(
  comments: readonly Comment[],
  artifactId: string
): ReadonlyMap<string, number> {
  const roots = comments
    .filter(
      (comment) =>
        comment.reply_to === null &&
        comment.ask_id === null &&
        isFileAnchor(comment.anchor) &&
        comment.anchor.artifact_id === artifactId &&
        comment.anchor.box !== undefined
    )
    .sort(
      (left, right) =>
        compareTimestamps(left.created_at, right.created_at) || left.id.localeCompare(right.id)
    );
  return new Map(roots.map((comment, index) => [comment.id, index + 1]));
}

/** What a file thread's card says it is about, in place of a passage's quote. */
export function fileThreadLabel(anchor: Anchor, boxNumber: number | undefined): string {
  const version = `version ${anchor.version}`;
  if (anchor.box === undefined) return `Whole file · ${version}`;
  return `Box${boxNumber === undefined ? "" : ` ${boxNumber}`} · ${version}`;
}
