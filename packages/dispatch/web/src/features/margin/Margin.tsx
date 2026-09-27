import { type ReactNode, useRef } from "react";
import { useLocation } from "react-router-dom";

import {
  borderDefault,
  focusVisibleRing,
  railBg,
  railBorder,
  railHoverBg,
  railText,
} from "../../theme/classes";
import { routeHasMargin } from "../refs/routes";
import { COMPACT_VIEWPORT_QUERY, useMediaQuery } from "../shell/useDialog";
import { MarginSheet } from "./MarginSheet";
import { useMarginSheet } from "./useMarginSheet";

export const DEFAULT_MARGIN_WIDTH = 384;
const MIN_MARGIN_WIDTH = 280;
const MARGIN_WIDTH_STEP = 24;

/** The widest the margin may be dragged: 60 % of the viewport, never below the minimum. */
function maxMarginWidth(): number {
  return Math.max(MIN_MARGIN_WIDTH, Math.floor(window.innerWidth * 0.6));
}

function clampMarginWidth(width: number): number {
  return Math.min(Math.max(width, MIN_MARGIN_WIDTH), maxMarginWidth());
}

interface MarginProps {
  collapsed?: boolean;
  onCollapsedChange?(collapsed: boolean): void;
  onWidthChange?(width: number): void;
  width?: number;
}

export function Margin({
  collapsed = false,
  onCollapsedChange,
  onWidthChange,
  width,
}: MarginProps): ReactNode {
  const model = useMarginSheet();
  const isCompactViewport = useMediaQuery(COMPACT_VIEWPORT_QUERY);
  const { pathname, search } = useLocation();
  const resizePointer = useRef<
    { pointerId: number; startWidth: number; startX: number } | undefined
  >(undefined);
  const marginWidth = clampMarginWidth(width ?? DEFAULT_MARGIN_WIDTH);
  const maxWidth = maxMarginWidth();
  const setClampedMarginWidth = (nextWidth: number) => {
    onWidthChange?.(clampMarginWidth(nextWidth));
  };

  // A margin is a property of a document surface. On a route that has none - the Inbox, a
  // project, Settings, Agents - the desktop column used to spend 384 px, a third of a 1280 px
  // viewport, saying so in a sentence. It now takes no width at all, and the reader's own
  // collapse preference is untouched, so it returns as they left it on the next issue or
  // document. `routeHasMargin` is the one answer the shell reserves its gutter from too.
  if (!isCompactViewport && !routeHasMargin(pathname, search)) {
    return null;
  }

  if (!isCompactViewport && collapsed) {
    return (
      <aside
        aria-label="Collapsed margin"
        className={`fixed inset-y-0 right-0 z-10 flex w-14 justify-center border-l p-2 ${railBorder} ${railBg} ${railText}`}
        data-testid="margin-rail"
      >
        <button
          aria-label="Show margin"
          className={`min-h-11 min-w-11 rounded-lg text-sm font-medium ${railHoverBg}`}
          onClick={() => onCollapsedChange?.(false)}
          type="button"
        >
          <span aria-hidden="true">‹</span>
        </button>
      </aside>
    );
  }

  return (
    <div
      className={isCompactViewport ? "contents" : "relative order-3 shrink-0"}
      data-testid={isCompactViewport ? undefined : "desktop-margin-shell"}
      style={isCompactViewport ? undefined : { width: `${marginWidth}px` }}
    >
      <hr
        aria-controls="review-margin"
        aria-label="Resize margin"
        aria-orientation="vertical"
        aria-valuemax={maxWidth}
        aria-valuemin={MIN_MARGIN_WIDTH}
        aria-valuenow={marginWidth}
        className={
          isCompactViewport
            ? "hidden"
            : `absolute top-0 -left-1 z-20 h-full w-2 border-0 border-l cursor-col-resize focus-visible:outline-none focus-visible:ring-2 ${borderDefault} ${focusVisibleRing}`
        }
        onDoubleClick={() => setClampedMarginWidth(DEFAULT_MARGIN_WIDTH)}
        onKeyDown={(event) => {
          if (event.key !== "ArrowLeft" && event.key !== "ArrowRight") {
            return;
          }
          event.preventDefault();
          setClampedMarginWidth(
            marginWidth + (event.key === "ArrowLeft" ? MARGIN_WIDTH_STEP : -MARGIN_WIDTH_STEP)
          );
        }}
        onPointerCancel={(event) => {
          if (resizePointer.current?.pointerId === event.pointerId) {
            resizePointer.current = undefined;
          }
        }}
        onPointerDown={(event) => {
          if (event.button !== 0) {
            return;
          }
          event.preventDefault();
          event.currentTarget.setPointerCapture(event.pointerId);
          resizePointer.current = {
            pointerId: event.pointerId,
            startWidth: marginWidth,
            startX: event.clientX,
          };
        }}
        onPointerMove={(event) => {
          const pointer = resizePointer.current;
          if (pointer?.pointerId !== event.pointerId) {
            return;
          }
          setClampedMarginWidth(pointer.startWidth + pointer.startX - event.clientX);
        }}
        onPointerUp={(event) => {
          if (resizePointer.current?.pointerId !== event.pointerId) {
            return;
          }
          resizePointer.current = undefined;
          event.currentTarget.releasePointerCapture(event.pointerId);
        }}
        tabIndex={0}
      />
      <MarginSheet
        desktopControl={
          isCompactViewport || onCollapsedChange === undefined ? undefined : (
            <button
              aria-label="Hide margin"
              className={`min-h-11 min-w-11 rounded-lg text-sm font-medium ${railHoverBg}`}
              onClick={() => onCollapsedChange(true)}
              type="button"
            >
              <span aria-hidden="true">›</span>
            </button>
          )
        }
        model={model}
      />
    </div>
  );
}
