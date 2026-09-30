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
import { TruncatedText } from "./TruncatedText";

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
 * host names the action in `aria-label` (`Remove Label: docs filter`). The chip is never wider
 * than its row: text that does not fit ends in an ellipsis, and a host whose text can be that
 * long passes the whole of it as `title`.
 */
export function Chip({
  "aria-label": ariaLabel,
  children,
  onClick,
  removable = false,
  selected,
  title,
  tone = "neutral",
}: {
  "aria-label"?: string;
  children: ReactNode;
  onClick: () => void;
  removable?: boolean;
  selected?: boolean;
  title?: string;
  tone?: ChipTone;
}): ReactNode {
  return (
    <button
      aria-label={ariaLabel}
      aria-pressed={selected}
      className={`inline-flex min-h-11 max-w-full shrink-0 items-center gap-1 rounded-full border px-3 text-xs font-medium whitespace-nowrap ${controlHoverBorder} ${
        selected === true ? selectedChip[tone] : restingChip
      }`}
      onClick={onClick}
      type="button"
    >
      <TruncatedText title={title}>{children}</TruncatedText>
      {removable ? <span aria-hidden="true">×</span> : null}
    </button>
  );
}
