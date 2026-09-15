import type { ReactNode } from "react";

import {
  primaryButtonBg,
  primaryButtonEnabledHoverBg,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textMutedHoverToSecondary,
} from "../theme/classes";

/** The one pin glyph: an outline that fills with `currentColor` once pinned. */
function PinIcon({ pinned }: { pinned: boolean }): ReactNode {
  return (
    <svg aria-hidden="true" className="h-4 w-4" fill="none" viewBox="0 0 16 16">
      <path
        d="M5 2.5h6v3l1.5 2v1H9v4.75L8 14.5l-1-1.25V8.5H3.5v-1L5 5.5z"
        fill={pinned ? "currentColor" : "none"}
        stroke="currentColor"
        strokeLinejoin="round"
        strokeWidth="1.25"
      />
    </svg>
  );
}

/** A pin toggle: 44 px tap target that shrinks to 32 px on desktop, `aria-pressed` for the
 *  pinned state, filled primary once pinned. `quiet` is for hosts that show one per row
 *  (conversation turns): no resting border, so a list is not a column of boxes, and the 44 px
 *  target on every breakpoint, so a turn's height never depends on its asynchronously rendered
 *  Markdown body (`ViewportAnchor` compensates React commits, not that later DOM swap). */
export function PinButton({
  className,
  disabled = false,
  label,
  onClick,
  pinned,
  quiet = false,
  title,
}: {
  className?: string;
  disabled?: boolean;
  label: string;
  onClick: () => void;
  pinned: boolean;
  quiet?: boolean;
  title?: string;
}): ReactNode {
  const resting = quiet
    ? textMutedHoverToSecondary
    : `${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`;
  return (
    <button
      aria-label={label}
      aria-pressed={pinned}
      className={`flex min-h-11 min-w-11 shrink-0 items-center justify-center rounded-lg disabled:cursor-not-allowed disabled:opacity-50 ${
        quiet ? "" : "md:min-h-8 md:min-w-8"
      } ${pinned ? `${primaryButtonBg} ${primaryButtonEnabledHoverBg}` : resting} ${className ?? ""}`}
      disabled={disabled}
      onClick={onClick}
      title={title ?? label}
      type="button"
    >
      <PinIcon pinned={pinned} />
    </button>
  );
}
