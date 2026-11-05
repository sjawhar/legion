import { createDraftStore } from "./draft-store";

/**
 * Unsent ask-answer drafts, by ask id, for the reason the reply drafts beside them live in one
 * (`reply-drafts.ts`): a draft belongs to the ask, not to the form showing it, so every card for
 * one ask reads and writes the same draft and a card that remounts picks it back up.
 *
 * It outlives the ask's Inbox row, which is what the Inbox reads it for. A question the server
 * withdraws, resolves or answers elsewhere leaves the list at once, except where the reader had
 * typed an answer to it: the reading column then keeps that question - its closed state, with
 * their text under it - until they select another row (`pane-selection.ts`, `paneAsk`;
 * `AskCard.tsx`, `unsentAnswer`). An answer the server records drops the draft, since it is no
 * longer unsent.
 */
const store = createDraftStore();

/** What the reader has typed into the ask's answer box and not sent, read without subscribing to
 *  it: the Inbox asks at the commit that drops the row, not at every keystroke in the column. */
export const answerDraft = store.read;

/** The ask's draft and its setter; the component re-renders whenever any form changes it. */
export const useAnswerDraft = store.useDraft;

/** Forgets every answer draft, for the unit-test preload. */
export const resetAnswerDrafts = store.reset;
