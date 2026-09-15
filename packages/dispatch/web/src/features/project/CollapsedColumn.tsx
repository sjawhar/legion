import { useDroppable } from "@dnd-kit/core";
import type { ReactNode } from "react";

import { Pill } from "../../components/Pill";
import {
  borderDefault,
  selectedCardBorder,
  surfaceMutedBg,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { type BoardColumn, statusLabel } from "./board-model";

/**
 * Icebox and Done while the board's edges are hidden: a narrow rail with the status name and
 * its count, no cards. It is the same `status:<s>` droppable a full column is, and like every
 * column it spans the board's height, so a card dragged onto it anywhere along the rail is
 * appended to that status - into Done that closes the issue, exactly as dropping into the full
 * column would.
 */
export function CollapsedColumn({ column }: { column: BoardColumn }): ReactNode {
  const { isOver, setNodeRef } = useDroppable({ id: `status:${column.status}` });
  const label = statusLabel(column.status);

  return (
    <section
      aria-label={`${label} (collapsed)`}
      className="w-10 shrink-0 snap-start"
      ref={setNodeRef}
    >
      <div
        className={`flex h-full min-h-[52px] flex-col items-center gap-3 rounded-xl border px-1 py-3 ${borderDefault} ${surfaceMutedBg} ${
          isOver ? selectedCardBorder : ""
        }`}
        data-testid="board-column-rail"
      >
        <Pill>{column.issues.length}</Pill>
        <span
          className={`text-sm font-semibold tracking-wide ${textPrimaryOnSurface}`}
          style={{ writingMode: "vertical-rl" }}
        >
          {label}
        </span>
      </div>
    </section>
  );
}
