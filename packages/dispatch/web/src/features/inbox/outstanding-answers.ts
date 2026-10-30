/**
 * The asks whose answer the reader has sent and the server has not decided, by ask id: the write
 * is in flight, or it was refused and the card still holds that refusal under their answer.
 *
 * It lives beside the drafts (`answer-drafts.ts`) for the same reason they do: what the reader
 * has put into an ask belongs to the ask, not to the form holding it, and the reading column
 * reads it from outside every card (`Inbox.tsx`, `paneAsk`). The column's card can be the
 * document's own - a question written as a decision block, drawn by `ProofDocument` - which takes
 * no prop of the Inbox's, so the form registers itself here rather than reporting upward as
 * `SnoozeControl` and `AssignToMe` do.
 *
 * What it is for: `useAskAnswerForm` drops the answered row from the `["inbox"]` read the moment
 * the answer goes out, so without this hold the column would hand the selection on at once and
 * unmount the card that wrote - and that card is the only surface a refused answer is shown on,
 * since `useKeyedWrite({inline: true})` lets a refused write go from the store. The hold is the
 * rule a deferred row already follows (`snoozing` in `Inbox.tsx`): the write's own liveness keeps
 * the question where the reader pressed it, released once the server decides it.
 *
 * No listeners: the column reads this at the commit the row leaves, which is a commit the
 * optimistic removal itself causes, and the write's own state re-renders every surface showing it.
 */
const outstanding = new Set<string>();

/** Holds the ask while the reader's own answer to it is outstanding, or lets it go. */
export function markAnswerOutstanding(askId: string, held: boolean): void {
  if (held) outstanding.add(askId);
  else outstanding.delete(askId);
}

/** Whether the reader's own answer to `askId` is still the server's to decide. */
export function answerOutstanding(askId: string): boolean {
  return outstanding.has(askId);
}

/** Forgets every hold. The unit-test preload calls it after each test, since the store outlives
 *  any one render. */
export function resetOutstandingAnswers(): void {
  outstanding.clear();
}
