import type { ReactNode } from "react";

import { disclosureButtonText, textSecondaryOnCanvas } from "../theme/classes";

/** The fold glyph: a chevron pointing down at rest and up once `expanded`. */
export function ChevronIcon({ expanded }: { expanded: boolean }): ReactNode {
  return (
    <svg
      aria-hidden="true"
      className={`h-4 w-4 shrink-0 transition-transform ${expanded ? "rotate-180" : ""}`}
      fill="none"
      viewBox="0 0 16 16"
    >
      <path
        d="M4 6l4 4 4-4"
        stroke="currentColor"
        strokeLinecap="round"
        strokeLinejoin="round"
        strokeWidth="1.5"
      />
    </svg>
  );
}

/** The one fold control: a labelled chevron button that reveals rows beneath it (the Agents
 * page's `Inactive (N)` sessions and `Show N older` exchanges, a component's `Show N done`
 * issues). `textClassName` is the text tone of the surface it sits on: the canvas by default,
 * `textSecondaryOnSurface` inside a card. */
export function DisclosureToggle({
  expanded,
  label,
  onToggle,
  textClassName = textSecondaryOnCanvas,
}: {
  expanded: boolean;
  label: string;
  onToggle: () => void;
  textClassName?: string;
}): ReactNode {
  return (
    <button
      aria-expanded={expanded}
      className={`flex min-h-11 items-center gap-1 text-sm font-medium ${textClassName}`}
      onClick={onToggle}
      type="button"
    >
      {label}
      <span className={disclosureButtonText}>
        <ChevronIcon expanded={expanded} />
      </span>
    </button>
  );
}
