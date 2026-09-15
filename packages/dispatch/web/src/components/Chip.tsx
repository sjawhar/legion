import type { ReactNode } from "react";

import {
  askBlockPill,
  borderDefault,
  borderStrong,
  borderTransparent,
  controlHoverBorder,
  surfaceMutedBg,
  surfaceMutedStrongBg,
  textMutedOnSurfaceMuted,
  textSecondaryOnSurface,
} from "../theme/classes";

/** The fill a chip takes while selected: the neutral strong surface, or an ask urgency's badge
 *  (the margin composer's Urgency row). At rest every tone is the same quiet outlined pill. */
export type ChipTone = "neutral" | keyof typeof askBlockPill;

const restingChip = `${borderDefault} ${surfaceMutedBg} ${textMutedOnSurfaceMuted}`;
const selectedChip: Record<ChipTone, string> = {
  neutral: `${borderStrong} ${surfaceMutedStrongBg} ${textSecondaryOnSurface}`,
  blocking: `${borderTransparent} ${askBlockPill.blocking.bg} ${askBlockPill.blocking.text}`,
  high: `${borderTransparent} ${askBlockPill.high.bg} ${askBlockPill.high.text}`,
  low: `${borderTransparent} ${askBlockPill.low.bg} ${askBlockPill.low.text}`,
  med: `${borderTransparent} ${askBlockPill.med.bg} ${askBlockPill.med.text}`,
};

/**
 * The one interactive chip: a single element carrying the border, the fill and the 44 px hit
 * area, so the outline is always the shaded shape (a border on a tall button wrapped around a
 * small `Pill` draws a bigger ring than the fill). `selected` renders `aria-pressed` and the
 * selected fill; `removable` appends a `×` glyph and the whole chip is the remove control, so the
 * host names the action in `aria-label` (`Remove Label: docs filter`).
 */
export function Chip({
  "aria-label": ariaLabel,
  children,
  onClick,
  removable = false,
  selected,
  tone = "neutral",
}: {
  "aria-label"?: string;
  children: ReactNode;
  onClick: () => void;
  removable?: boolean;
  selected?: boolean;
  tone?: ChipTone;
}): ReactNode {
  return (
    <button
      aria-label={ariaLabel}
      aria-pressed={selected}
      className={`inline-flex min-h-11 shrink-0 items-center gap-1 rounded-full border px-3 text-xs font-medium whitespace-nowrap ${controlHoverBorder} ${
        selected === true ? selectedChip[tone] : restingChip
      }`}
      onClick={onClick}
      type="button"
    >
      {children}
      {removable ? <span aria-hidden="true">×</span> : null}
    </button>
  );
}
