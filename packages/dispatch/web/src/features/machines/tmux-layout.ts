import { TMUX_LAYOUT_MAX } from "@legion/contracts";

/**
 * tmux's `window_layout` string, read into the boxes it names.
 *
 * The form, measured from tmux 3.5a: four hex digits of checksum, a comma, then the root cell as
 * `<width>x<height>,<x>,<y>`. A cell followed by `[` is split top to bottom and one followed by
 * `{` is split left to right, each holding its children as a comma-separated list inside the
 * bracket; a cell with no bracket is a pane, and the number after it is the pane's id without its
 * leading `%`.
 *
 * Every number is in terminal cells, as tmux states them. tmux spends one cell on the divider
 * between two panes and gives it to neither, so two panes beside each other in an 80-cell window
 * are 40 and 39 wide, and a drawing adds that cell back itself: this parser reports what tmux
 * wrote and invents no gap.
 *
 * The checksum is read for its shape and never recomputed. tmux's own checksum guards a layout a
 * person hands to `select-layout`; here the string came from tmux through a session's
 * registration, so a mismatch would say nothing a reader could act on, while a re-implementation
 * of it that was wrong in any corner would hide windows that draw perfectly well.
 */

/** A box in terminal cells, as the layout string states it. */
export interface TmuxBox {
  readonly width: number;
  readonly height: number;
  readonly x: number;
  readonly y: number;
}

/**
 * One pane of the window. `paneId` carries the leading `%` that the layout string drops (`,5` is
 * `%5`), because every other side of the hop writes it that way — `TmuxPlace.pane_id`, the pane's
 * own `$TMUX_PANE`, `tmux list-panes` — so a reader matching a session to a pane never has to
 * know which side stripped it.
 */
export interface TmuxLayoutPane extends TmuxBox {
  readonly kind: "pane";
  readonly paneId: string;
}

export interface TmuxLayoutSplit extends TmuxBox {
  readonly kind: "split";
  /**
   * `rows` for `[`, whose children sit one above the next; `columns` for `{`, whose children sit
   * side by side. tmux's own words for these ("vertical", "horizontal") name opposite things in
   * `split-window` and in its documentation of the layout, so neither is used here.
   */
  readonly into: "rows" | "columns";
  readonly children: readonly TmuxLayoutCell[];
}

export type TmuxLayoutCell = TmuxLayoutPane | TmuxLayoutSplit;

export interface TmuxLayout {
  readonly root: TmuxLayoutCell;
  /** Every pane in the window, in the order the layout string names them. */
  readonly panes: readonly TmuxLayoutPane[];
}

/** Where the read has reached in the string; the only mutable state of the parse. */
interface Cursor {
  readonly text: string;
  at: number;
}

const CHECKSUM = /^[0-9a-f]{4},/;

function isDigit(code: number): boolean {
  // `charCodeAt` past the end is NaN, which fails both bounds, so this also ends the string.
  return code >= 48 && code <= 57;
}

function readDigits(cursor: Cursor): number | undefined {
  const start = cursor.at;
  while (isDigit(cursor.text.charCodeAt(cursor.at))) {
    cursor.at += 1;
  }
  return cursor.at === start ? undefined : Number(cursor.text.slice(start, cursor.at));
}

function readLiteral(cursor: Cursor, literal: string): boolean {
  if (cursor.text[cursor.at] !== literal) {
    return false;
  }
  cursor.at += 1;
  return true;
}

function readChildren(cursor: Cursor, closer: "]" | "}"): TmuxLayoutCell[] | undefined {
  cursor.at += 1; // the opening bracket, which the caller has already read
  const children: TmuxLayoutCell[] = [];
  for (;;) {
    const child = readCell(cursor);
    if (child === undefined) {
      return undefined;
    }
    children.push(child);
    if (readLiteral(cursor, closer)) {
      return children;
    }
    if (!readLiteral(cursor, ",")) {
      return undefined;
    }
  }
}

function readCell(cursor: Cursor): TmuxLayoutCell | undefined {
  const width = readDigits(cursor);
  if (width === undefined || !readLiteral(cursor, "x")) {
    return undefined;
  }
  const height = readDigits(cursor);
  if (height === undefined || !readLiteral(cursor, ",")) {
    return undefined;
  }
  const x = readDigits(cursor);
  if (x === undefined || !readLiteral(cursor, ",")) {
    return undefined;
  }
  const y = readDigits(cursor);
  if (y === undefined) {
    return undefined;
  }
  // What follows the box says what the cell is: a bracket opens a split, a comma introduces the
  // pane id, and a cell that is neither is a cell tmux did not write.
  const opener = cursor.text[cursor.at];
  if (opener === "[" || opener === "{") {
    const children = readChildren(cursor, opener === "[" ? "]" : "}");
    return children === undefined
      ? undefined
      : {
          children,
          height,
          into: opener === "[" ? "rows" : "columns",
          kind: "split",
          width,
          x,
          y,
        };
  }
  if (!readLiteral(cursor, ",")) {
    return undefined;
  }
  const paneNumber = readDigits(cursor);
  return paneNumber === undefined
    ? undefined
    : { height, kind: "pane", paneId: `%${paneNumber}`, width, x, y };
}

function collectPanes(cell: TmuxLayoutCell, into: TmuxLayoutPane[]): void {
  if (cell.kind === "pane") {
    into.push(cell);
    return;
  }
  for (const child of cell.children) {
    collectPanes(child, into);
  }
}

/**
 * Reads a `window_layout` string into its root cell and its panes, or answers undefined for a
 * string this grammar does not cover whole — a truncated or unbalanced one, a cell that is
 * neither a pane nor a split, anything after the root cell — and for one longer than
 * `TMUX_LAYOUT_MAX`, whatever it holds. A caller then has a window it cannot draw rather than
 * one it would draw wrongly.
 */
export function parseTmuxLayout(layout: string): TmuxLayout | undefined {
  // The cap is read here and not taken on trust from the sender. A session's client drops a
  // longer layout before it registers one, but the listener stores whatever a session sends and
  // Dispatch answers it unchanged, so a client with a session bearer could register a string
  // nested about one level per eight characters - and `readCell`/`readChildren` recurse once per
  // level, which would overflow the stack of every reader of that machine's page. A refused
  // layout costs that window its drawing and nothing else: it falls back to its list of panes.
  if (layout.length > TMUX_LAYOUT_MAX || !CHECKSUM.test(layout)) {
    return undefined;
  }
  const cursor: Cursor = { at: 5, text: layout };
  const root = readCell(cursor);
  if (root === undefined || cursor.at !== layout.length) {
    return undefined;
  }
  const panes: TmuxLayoutPane[] = [];
  collectPanes(root, panes);
  return { panes, root };
}
