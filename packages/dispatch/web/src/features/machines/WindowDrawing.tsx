import { type ReactNode, useEffect, useMemo, useState } from "react";

import {
  inlineWarningText,
  surfaceMutedBg,
  surfaceMutedStrongBg,
  surfaceRecessedBg,
  textMutedOnSurface,
  textMutedOnSurfaceMuted,
  textPrimaryOnSurfaceMuted,
} from "../../theme/classes";
import { type AgentGroup, agentGroup } from "../agents/agent-groups";
import type { NeedsYouBySession } from "../agents/ask-tally";
import { AgentName } from "../refs/AgentName";
import type { MachinePane, MachineWindow } from "./machine-model";
import { parseTmuxLayout, type TmuxLayout, type TmuxLayoutPane } from "./tmux-layout";

/**
 * One tmux window, drawn: a box holding one absolutely positioned rectangle per pane, at the
 * percentages the window's own layout string gives. The rectangles carry no border, padding or
 * margin of their own, so the drawing's proportions are the layout's and nothing else; the
 * visible separation between two panes is the one-cell divider the layout already encodes.
 *
 * Where a pane's rectangle would be too small to read, and for a window whose layout is absent or
 * which the parser refuses, the window is drawn as a list of its panes instead, with the reason.
 */

/** The width a short session name needs beside its state dot. */
const MIN_PANE_WIDTH_PX = 96;

/** The height dispatch://INFRA-812 gives an Agents row: under it a pane holds no row of text. */
const MIN_PANE_HEIGHT_PX = 42;

/** A terminal cell is a character, about twice as tall as it is wide, so a window of `C` columns
 *  and `R` rows is drawn at `width × 2R / C`. */
const CELL_ASPECT = 2;

/**
 * The width the drawing lays out at until it has measured itself: a laptop, the width most
 * readers are at. happy-dom lays nothing out and has no `ResizeObserver`, so a unit test drives
 * the component through the `width` prop rather than through a measurement.
 */
const ASSUMED_WIDTH_PX = 1440;

/** Why a window is a list of panes rather than a drawing. */
type ListReason = "no-layout" | "panes-too-small" | "unreadable-layout";

const LIST_REASON_TEXT: Record<ListReason, string> = {
  "no-layout": "This window reported no layout, so its panes are listed by pane index instead.",
  "panes-too-small":
    "A pane of this window would be too small to read at this width, so its panes are listed in layout order instead.",
  "unreadable-layout":
    "This window's layout could not be read, so its panes are listed by pane index instead.",
};

/**
 * What a window comes to at the width it is drawn at: the layout to draw it from, or the reason
 * its panes are listed instead. A listed window still carries its layout where the parser read
 * one, since the list is then in layout order rather than pane-index order.
 */
type WindowShape =
  | { readonly layout: TmuxLayout; readonly reason: undefined }
  | { readonly layout: TmuxLayout | undefined; readonly reason: ListReason };

function windowShape(
  reported: string | undefined,
  layout: TmuxLayout | undefined,
  width: number
): WindowShape {
  if (reported === undefined) return { layout: undefined, reason: "no-layout" };
  if (layout === undefined) return { layout: undefined, reason: "unreadable-layout" };
  if (panesTooSmall(layout, width)) return { layout, reason: "panes-too-small" };
  return { layout, reason: undefined };
}

const STATE_LABEL: Record<AgentGroup, string> = {
  active: "Active",
  idle: "Idle",
  "needs-you": "Needs you",
  quiet: "Quiet",
};

/**
 * The box's own width in pixels, or `undefined` while nothing has measured it. The observer is
 * the only measurement: a window drawn at a laptop width and one drawn on a phone differ by
 * whether their panes can be read, which is the viewport's answer and not the model's.
 */
function useMeasuredWidth(node: HTMLElement | null): number | undefined {
  const [width, setWidth] = useState<number | undefined>(undefined);
  useEffect(() => {
    if (node === null) {
      return;
    }
    const measure = (): void => {
      const measured = node.getBoundingClientRect().width;
      setWidth(measured > 0 ? measured : undefined);
    };
    measure();
    // happy-dom has none; a unit test passes `width` instead.
    if (typeof ResizeObserver === "undefined") {
      return;
    }
    const observer = new ResizeObserver(measure);
    observer.observe(node);
    return () => {
      observer.disconnect();
    };
  }, [node]);
  return width;
}

/** The drawing's height: the window's own aspect ratio, in cells of one by two. */
function drawingHeight(layout: TmuxLayout, width: number): number {
  return (width * CELL_ASPECT * layout.root.height) / layout.root.width;
}

/** Whether any pane of the window would be drawn too small to hold a name and a state. */
function panesTooSmall(layout: TmuxLayout, width: number): boolean {
  const height = drawingHeight(layout, width);
  return layout.panes.some(
    (pane) =>
      (pane.width / layout.root.width) * width < MIN_PANE_WIDTH_PX ||
      (pane.height / layout.root.height) * height < MIN_PANE_HEIGHT_PX
  );
}

/** One pane as the window draws it: its box in the layout, the session in it, and its index. */
interface DrawnPane {
  readonly cell: TmuxLayoutPane;
  readonly pane: MachinePane | undefined;
  readonly paneIndex: number;
}

/**
 * The window's panes in the layout's own order, each with the index to draw it by. A layout
 * string names its panes in index order and carries no index of its own, so a pane no session
 * named takes its place in that order, shifted by the window's own base index: the first pane
 * the model knows says what the shift is (tmux's `pane-base-index` is 0 by default and 1 in
 * many configurations).
 */
function drawnPanes(layout: TmuxLayout, panes: ReadonlyMap<string, MachinePane>): DrawnPane[] {
  let shift = 0;
  for (const [position, cell] of layout.panes.entries()) {
    const known = panes.get(cell.paneId);
    if (known !== undefined) {
      shift = known.paneIndex - position;
      break;
    }
  }
  return layout.panes.map((cell, position) => {
    const pane = panes.get(cell.paneId);
    return { cell, pane, paneIndex: pane?.paneIndex ?? position + shift };
  });
}

/** What the viewer is owed, by session id: the open asks whose turn is theirs and the replies
 *  they have not read. The page reads both once from the queries the shell already makes and
 *  hands them in, so the drawing itself fetches nothing and a unit test drives it with two
 *  plain maps. */
export interface PaneCounts {
  readonly asksForYou: NeedsYouBySession;
  readonly unreadReplies: ReadonlyMap<string, number>;
}

/** What one pane says: its session, or that it holds none. */
function PaneBody({
  compact,
  counts,
  now,
  pane,
  paneIndex,
}: {
  compact: boolean;
  counts: PaneCounts;
  now: number;
  pane: MachinePane | undefined;
  paneIndex: number;
}): ReactNode {
  if (pane === undefined) {
    return (
      <>
        <span className="font-medium">Pane {paneIndex}</span>
        <span>No agent</span>
      </>
    );
  }
  const sessionId = pane.session.session_id;
  const group = agentGroup(
    pane.session,
    counts.asksForYou.get(sessionId) ?? 0,
    counts.unreadReplies.get(sessionId) ?? 0,
    now
  );
  return (
    <>
      <AgentName sessionId={pane.session.session_id} storedTitle={pane.session.title} />
      <span data-state={group} data-testid="window-pane-state">
        {STATE_LABEL[group]}
      </span>
      {pane.zoomed ? <span data-testid="window-pane-zoomed">Zoomed</span> : null}
      {pane.otherSessions === 0 ? null : (
        <span data-testid="window-pane-others">
          {pane.otherSessions === 1
            ? "1 other session names this pane"
            : `${pane.otherSessions} other sessions name this pane`}
        </span>
      )}
      {compact ? <span>Pane {paneIndex}</span> : null}
    </>
  );
}

export interface WindowDrawingProps {
  /** What the viewer is owed, by session id, for the first of the four states. */
  readonly counts: PaneCounts;
  /** The clock each pane's state is read against; the page's own. */
  readonly now: number;
  /** The width to lay out at, in pixels, in place of the measured one: the seam a unit test
   *  drives the component through, since happy-dom measures nothing. */
  readonly width?: number;
  readonly window: MachineWindow;
}

export function WindowDrawing({
  counts,
  now,
  width,
  window: shown,
}: WindowDrawingProps): ReactNode {
  const [box, setBox] = useState<HTMLElement | null>(null);
  const measured = useMeasuredWidth(box);
  const drawnWidth = width ?? measured ?? ASSUMED_WIDTH_PX;

  // Parsed once per layout string, not once per measured pixel: the observer re-renders the
  // component for every width the reader drags it through, and the layout is the same string.
  const layout = useMemo(() => {
    const parsed = shown.layout === undefined ? undefined : parseTmuxLayout(shown.layout);
    // A layout whose root has no size divides to nothing: it is read as one the parser refused,
    // since a drawing made from it would be a box of NaNs.
    return parsed !== undefined && parsed.root.width > 0 && parsed.root.height > 0
      ? parsed
      : undefined;
  }, [shown.layout]);
  const panes = useMemo(
    () => new Map(shown.panes.map((pane) => [pane.paneId, pane])),
    [shown.panes]
  );
  // Whether the panes fit is the width's answer, so this is read on every render.
  const shape = windowShape(shown.layout, layout, drawnWidth);

  return (
    <section
      aria-label={`Window ${shown.windowIndex}: ${shown.windowName}`}
      data-drawn={shape.reason === undefined ? "layout" : "list"}
      data-testid="window-drawing"
      data-window={shown.windowId}
      data-window-index={shown.windowIndex}
      data-window-name={shown.windowName}
      ref={setBox}
    >
      {shape.reason !== undefined ? (
        <PaneList
          counts={counts}
          layout={shape.layout}
          now={now}
          panes={panes}
          reason={shape.reason}
          window={shown}
        />
      ) : (
        <PaneBoxes
          counts={counts}
          height={drawingHeight(shape.layout, drawnWidth)}
          layout={shape.layout}
          now={now}
          panes={panes}
        />
      )}
    </section>
  );
}

/** The drawing itself: one filled rectangle per pane, each at the layout's own percentages. */
function PaneBoxes({
  counts,
  height,
  layout,
  now,
  panes,
}: {
  counts: PaneCounts;
  height: number;
  layout: TmuxLayout;
  now: number;
  panes: ReadonlyMap<string, MachinePane>;
}): ReactNode {
  const rows = drawnPanes(layout, panes);
  return (
    <div
      className={`relative w-full overflow-hidden rounded-lg ${surfaceMutedStrongBg}`}
      data-columns={layout.root.width}
      data-rows={layout.root.height}
      data-testid="window-layout"
      style={{ height: `${height}px` }}
    >
      {rows.map(({ cell, pane, paneIndex }) => (
        <div
          className="absolute overflow-hidden"
          data-agent={pane?.session.session_id}
          data-pane={cell.paneId}
          data-pane-index={paneIndex}
          data-testid="window-pane"
          data-zoomed={pane?.zoomed === true ? "true" : "false"}
          key={cell.paneId}
          style={{
            height: `${(cell.height / layout.root.height) * 100}%`,
            left: `${(cell.x / layout.root.width) * 100}%`,
            top: `${(cell.y / layout.root.height) * 100}%`,
            width: `${(cell.width / layout.root.width) * 100}%`,
          }}
        >
          <div
            className={`flex h-full flex-col gap-1 overflow-hidden p-2 text-xs ${
              pane === undefined
                ? `${surfaceRecessedBg} ${textMutedOnSurface}`
                : `${surfaceMutedBg} ${textPrimaryOnSurfaceMuted}`
            }`}
          >
            <PaneBody compact={false} counts={counts} now={now} pane={pane} paneIndex={paneIndex} />
          </div>
        </div>
      ))}
    </div>
  );
}

/** The window as a list of its panes, and why it is not a drawing. */
function PaneList({
  counts,
  layout,
  now,
  panes,
  reason,
  window: shown,
}: {
  counts: PaneCounts;
  layout: TmuxLayout | undefined;
  now: number;
  panes: ReadonlyMap<string, MachinePane>;
  reason: ListReason;
  window: MachineWindow;
}): ReactNode {
  const rows =
    layout === undefined
      ? shown.panes.map((pane) => ({ pane, paneId: pane.paneId, paneIndex: pane.paneIndex }))
      : drawnPanes(layout, panes).map((row) => ({ ...row, paneId: row.cell.paneId }));
  return (
    <div data-reason={reason} data-testid="window-pane-list">
      <p className={`mb-2 text-xs ${inlineWarningText}`} data-testid="window-pane-list-reason">
        {LIST_REASON_TEXT[reason]}
      </p>
      <ol className="flex flex-col gap-1">
        {rows.map((row) => (
          <li
            className={`flex flex-wrap items-center gap-2 rounded-lg p-2 text-xs ${
              row.pane === undefined
                ? `${surfaceRecessedBg} ${textMutedOnSurface}`
                : `${surfaceMutedBg} ${textMutedOnSurfaceMuted}`
            }`}
            data-agent={row.pane?.session.session_id}
            data-pane={row.paneId}
            data-pane-index={row.paneIndex}
            data-testid="window-pane-row"
            key={row.paneId}
          >
            <PaneBody
              compact={true}
              counts={counts}
              now={now}
              pane={row.pane}
              paneIndex={row.paneIndex}
            />
          </li>
        ))}
      </ol>
    </div>
  );
}
