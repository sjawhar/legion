import type { ReactNode } from "react";

import { kbdHint, railHoverBg, railHoverText, railSecondaryText } from "../../theme/classes";

/** A rail control that stands for a keyboard binding: its label, then the keys that also run it,
 *  so a person who has not learned the binding can still reach what it opens. `className` carries
 *  the control's place in the rail (its top margin), which differs per call site. */
export function RailButton({
  ariaKeyshortcuts,
  className,
  label,
  onOpen,
  shortcut,
}: {
  ariaKeyshortcuts: string;
  className: string;
  label: string;
  onOpen: () => void;
  shortcut: string;
}): ReactNode {
  return (
    <button
      aria-keyshortcuts={ariaKeyshortcuts}
      className={`${className} flex min-h-11 w-full items-center justify-between rounded-lg px-3 text-left text-sm font-medium ${railSecondaryText} ${railHoverBg} ${railHoverText}`}
      onClick={onOpen}
      type="button"
    >
      <span>{label}</span>
      <kbd className={`rounded px-1.5 py-0.5 text-xs font-medium ${kbdHint}`}>{shortcut}</kbd>
    </button>
  );
}
