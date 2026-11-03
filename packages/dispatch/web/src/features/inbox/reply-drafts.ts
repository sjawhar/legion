import { createDraftStore, draftEmpty, EMPTY_DRAFT } from "./draft-store";

/**
 * Unsent ask-reply drafts, by ask id. A draft belongs to the ask, not to the composer showing it:
 * every composer for one ask reads and writes the same draft (the margin's inline composer and a
 * Conversation or decision-block composer can be on one page at once), and a composer that
 * remounts (switching margin tabs away and back, closing and reopening Write a reply) picks the
 * draft back up - the mentions it holds with its text (`draft-store.ts`). A sent reply, an
 * emptied field, or the ask resolving drops it.
 */
const store = createDraftStore();

/** Drops an ask's draft; with `sent`, only while the draft's text is still what was sent. */
export function clearReplyDraft(askId: string, sent?: string): void {
  const draft = store.read(askId);
  if (draftEmpty(draft) || (sent !== undefined && draft.text.trim() !== sent)) return;
  store.set(askId, EMPTY_DRAFT);
}

/** The ask's draft and its setter; the component re-renders whenever any composer changes it. */
export const useReplyDraft = store.useDraft;

/** Forgets every reply draft, for the unit-test preload. */
export const resetReplyDrafts = store.reset;
