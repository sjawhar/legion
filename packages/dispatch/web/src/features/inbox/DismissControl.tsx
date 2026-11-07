import { createContext, type ReactNode, useContext } from "react";

import type { MyReplyRow } from "../../api/types";
import { linkHoverText, linkText } from "../../theme/classes";
import type { InboxShow } from "../refs/routes";
import { questionLine } from "./question-line";
import { dismissThreadRow, type InboxDismissRow } from "./useInboxDismiss";

/** What a thread row's Dismiss does, as the Inbox hands it to the three surfaces that draw one:
 *  the short row, the reading column's head and the card. */
export interface RowDismiss {
  /** Takes the row out of the view it is drawn in: out of the Inbox, or - in the Dismissed view -
   *  back into it. The row carries what the page drew in it, which the control reads off the row
   *  it is drawn on, so a press names what the reader was shown. */
  dismiss: (row: InboxDismissRow) => void;
  /** The row ids whose own press is still out. */
  pending: ReadonlySet<string>;
  /** Which view the row is drawn in, which is what its control is called. */
  show: InboxShow;
}

/**
 * The dismiss the Inbox hands to the rows it draws, as a context rather than a prop: a thread is
 * drawn by `ThreadRowCard` and by the reading column, which reaches its head through four bodies
 * of its own, and neither route carries a prop of the list's. Each Inbox provides its own - the
 * page's and the drawer's are separate - and with no provider a thread draws no Dismiss at all,
 * which is what a thread drawn outside the list does.
 */
export const RowDismissContext = createContext<RowDismiss | undefined>(undefined);

/**
 * A thread row's **Dismiss**, which takes it out of the Inbox - or, in the Dismissed view, the
 * **Move to Inbox** that puts it back. An Ask or an Approval row draws none: a question leaves the
 * Inbox by being answered or snoozed.
 *
 * On the short row it shows while the pointer is on the row or something in it holds the focus,
 * and always on the row the reading column is showing (`shown`); the column's own head and the
 * card draw it beside their other controls, where there is room for it.
 */
export function DismissControl({
  shown = false,
  thread,
}: {
  /** Whether the control is drawn whatever the reader's hand is doing: the head's and the card's
   *  always are, and so is the selected row's. */
  shown?: boolean;
  thread: MyReplyRow;
}): ReactNode {
  const dismiss = useContext(RowDismissContext);
  if (dismiss === undefined) return null;
  const named = dismissThreadRow(thread);
  const restoring = dismiss.show === "dismissed";
  const label = restoring ? "Move to Inbox" : "Dismiss";
  const live = dismiss.pending.has(named.id);
  return (
    <button
      aria-label={`${label}: ${questionLine(thread.subject)}`}
      className={`min-h-11 shrink-0 text-sm font-medium md:min-h-8 ${linkText} ${linkHoverText} ${
        shown ? "" : "opacity-0 group-hover:opacity-100 group-focus-within:opacity-100"
      }`}
      data-inbox-dismiss={named.id}
      disabled={live}
      onClick={() => dismiss.dismiss(named)}
      type="button"
    >
      {live ? (restoring ? "Moving…" : "Dismissing…") : label}
    </button>
  );
}
