import type { ReactNode } from "react";

import { kbdHint, railHoverBg, railHoverText, railSecondaryText } from "../../theme/classes";

export function SearchButton({ onOpen }: { onOpen: () => void }): ReactNode {
  const shortcut = navigator.userAgent.includes("Mac") ? "⌘K" : "Ctrl K";

  return (
    <button
      aria-keyshortcuts="Control+K Meta+K"
      className={`mt-4 flex min-h-11 w-full items-center justify-between rounded-lg px-3 text-left text-sm font-medium ${railSecondaryText} ${railHoverBg} ${railHoverText}`}
      onClick={onOpen}
      type="button"
    >
      <span>Search</span>
      <kbd className={`rounded px-1.5 py-0.5 text-xs font-medium ${kbdHint}`}>{shortcut}</kbd>
    </button>
  );
}
