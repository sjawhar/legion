/**
 * The line between the Inbox's list and its reading pane, and how wide the reader has made them.
 *
 * The pane is the wider of the two: it starts at `DEFAULT_PANE_WIDTH` of the width the two
 * columns share, the arrow keys move the line a step at a time while it has the focus, a
 * double-click puts it back, and whatever the reader settles on is kept for their browser
 * (`inbox.pane-width`), so a reload opens where they left it. The document margin's resizer is
 * the same control for the same job (`features/margin/Margin.tsx`), and this follows it: an `hr`
 * that carries the slider's own attributes, pointer capture while it is dragged, and the reset on
 * a double-click.
 *
 * The width is a share of the two columns rather than a number of pixels, because the Inbox lays
 * them out as a grid of two fractions: the share is exactly what the grid takes, at any viewport
 * width.
 */

import { type ReactNode, type RefObject, useRef } from "react";

import { borderDefault, focusVisibleRing } from "../../theme/classes";
import { useUserPreference } from "../shell/userPreference";

/** How much of the two columns the reading pane takes to begin with. */
export const DEFAULT_PANE_WIDTH = 0.6;

/** The narrowest and widest the pane may be dragged: the list keeps a fifth of the row, and the
 *  pane never shrinks below a third of it. */
const MIN_PANE_WIDTH = 0.3;
const MAX_PANE_WIDTH = 0.8;

/** What one press of an arrow key moves the line, as a share of the two columns: about 26 px at
 *  the 1280 px the pane appears from, which is the margin resizer's own step. */
const PANE_WIDTH_STEP = 0.02;

export function clampPaneWidth(width: number): number {
  if (!Number.isFinite(width)) return DEFAULT_PANE_WIDTH;
  return Math.min(Math.max(width, MIN_PANE_WIDTH), MAX_PANE_WIDTH);
}

/** The share of the two columns the reading pane takes, kept for the reader's browser. The page
 *  and the drawer read the same value, as the thread order and the folded bands do. */
export function usePaneWidth(): [number, (next: number) => void] {
  return useUserPreference<number>(
    "inbox.pane-width",
    (stored) => (stored === null ? DEFAULT_PANE_WIDTH : clampPaneWidth(Number.parseFloat(stored))),
    (value) => String(value)
  );
}

export interface PaneDividerProps {
  /** The grid the two columns are laid out in, whose width a drag is measured against. */
  columns: RefObject<HTMLElement | null>;
  onWidthChange: (width: number) => void;
  /** The share the pane holds now, as `usePaneWidth` keeps it. */
  width: number;
}

export function PaneDivider({ columns, onWidthChange, width }: PaneDividerProps): ReactNode {
  const drag = useRef<{ pointerId: number; startWidth: number; startX: number } | undefined>(
    undefined
  );
  const setWidth = (next: number) => onWidthChange(clampPaneWidth(next));
  return (
    <hr
      aria-controls="inbox-pane"
      aria-label="Resize reading pane"
      aria-orientation="vertical"
      aria-valuemax={Math.round(MAX_PANE_WIDTH * 100)}
      aria-valuemin={Math.round(MIN_PANE_WIDTH * 100)}
      aria-valuenow={Math.round(width * 100)}
      className={`mx-3 h-full w-0.5 shrink-0 cursor-col-resize border-0 border-l focus-visible:outline-none focus-visible:ring-2 ${borderDefault} ${focusVisibleRing}`}
      data-testid="inbox-pane-divider"
      onDoubleClick={() => setWidth(DEFAULT_PANE_WIDTH)}
      onKeyDown={(event) => {
        if (event.key !== "ArrowLeft" && event.key !== "ArrowRight") {
          return;
        }
        event.preventDefault();
        // The pane is the right-hand column, so the line moving left widens it.
        setWidth(width + (event.key === "ArrowLeft" ? PANE_WIDTH_STEP : -PANE_WIDTH_STEP));
      }}
      onPointerCancel={(event) => {
        if (drag.current?.pointerId === event.pointerId) {
          drag.current = undefined;
        }
      }}
      onPointerDown={(event) => {
        if (event.button !== 0) {
          return;
        }
        event.preventDefault();
        event.currentTarget.setPointerCapture(event.pointerId);
        drag.current = { pointerId: event.pointerId, startWidth: width, startX: event.clientX };
      }}
      onPointerMove={(event) => {
        const pointer = drag.current;
        const row = columns.current;
        if (pointer?.pointerId !== event.pointerId || row === null) {
          return;
        }
        // A row with no measured width yet would make every drag one division by zero.
        const span = row.clientWidth;
        if (span <= 0) {
          return;
        }
        setWidth(pointer.startWidth + (pointer.startX - event.clientX) / span);
      }}
      onPointerUp={(event) => {
        if (drag.current?.pointerId !== event.pointerId) {
          return;
        }
        drag.current = undefined;
        event.currentTarget.releasePointerCapture(event.pointerId);
      }}
      tabIndex={0}
    />
  );
}
