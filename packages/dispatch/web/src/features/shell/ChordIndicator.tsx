import type { ReactNode } from "react";

import { card, kbdHint, textMutedOnSurface } from "../../theme/classes";
import { usePendingChord } from "./keymap";

/** Shows a half-typed chord (`g …`) so the pending key is visible until it completes or times out. */
export function ChordIndicator(): ReactNode {
  const pending = usePendingChord();
  if (pending === "") {
    return null;
  }
  return (
    <output
      className={`fixed right-4 bottom-32 z-40 flex items-center gap-1.5 rounded-full border px-3 py-1.5 text-sm shadow-lg xl:bottom-16 ${card} ${textMutedOnSurface}`}
      data-testid="chord-indicator"
    >
      <kbd className={`rounded px-1.5 py-0.5 font-mono text-xs font-medium ${kbdHint}`}>
        {pending}
      </kbd>
      <span aria-hidden="true">…</span>
      <span className="sr-only">then…</span>
    </output>
  );
}
