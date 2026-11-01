import type { ReactNode } from "react";

import { kbdHint, railHoverBg, railHoverText, railSecondaryText } from "../../theme/classes";

/** The rail's control for the `c` shortcut's create-issue dialog, under Search, so a person who
 *  does not know the shortcut can still file an issue. It shows the key it stands for. */
export function CreateIssueButton({ onOpen }: { onOpen: () => void }): ReactNode {
  return (
    <button
      aria-keyshortcuts="C"
      className={`mt-1 flex min-h-11 w-full items-center justify-between rounded-lg px-3 text-left text-sm font-medium ${railSecondaryText} ${railHoverBg} ${railHoverText}`}
      onClick={onOpen}
      type="button"
    >
      <span>New issue</span>
      <kbd className={`rounded px-1.5 py-0.5 text-xs font-medium ${kbdHint}`}>C</kbd>
    </button>
  );
}
