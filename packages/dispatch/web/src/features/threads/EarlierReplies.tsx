import { type ReactNode, useId } from "react";

import { linkHoverText, linkText } from "../../theme/classes";
import { earlierRepliesLabel } from "./thread-layout";

/**
 * The control a thread's folded replies sit behind: **Show N earlier replies** while they are
 * folded, **Show fewer replies** once they are not, and the replies themselves beneath it.
 *
 * One control for every surface that folds - an ask's thread and the Inbox's own thread body -
 * so the two cannot drift; what folds at all is `threadLayout`'s (`thread-layout.ts`), and the
 * fold is drawn furthest from the newest message, under the root in Oldest first and at the
 * foot of the thread in Newest first. Each host lists its replies its own way, so it is handed
 * the id the button names and gives back the list wearing it. Nothing folded, no control.
 */
export function EarlierReplies({
  count,
  onToggle,
  open,
  replies,
}: {
  /** How many replies the fold holds. */
  count: number;
  onToggle: () => void;
  open: boolean;
  /** The folded replies, drawn only while the fold is open, in the element the host lists them
   *  in: it carries `id`, which the button points at. */
  replies: (id: string) => ReactNode;
}): ReactNode {
  const foldId = useId();
  if (count === 0) {
    return null;
  }
  return (
    <>
      <button
        aria-controls={foldId}
        aria-expanded={open}
        className={`min-h-11 text-sm font-medium ${linkText} ${linkHoverText}`}
        onClick={onToggle}
        type="button"
      >
        {open ? "Show fewer replies" : earlierRepliesLabel(count)}
      </button>
      {open ? replies(foldId) : null}
    </>
  );
}
