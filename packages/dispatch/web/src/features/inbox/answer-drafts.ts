import { type Dispatch, type SetStateAction, useCallback, useSyncExternalStore } from "react";

/**
 * Unsent ask-answer drafts, by ask id, for the reason the reply drafts beside them live in one
 * (`reply-drafts.ts`): a draft belongs to the ask, not to the form showing it, so every card for
 * one ask reads and writes the same draft and a card that remounts picks it back up.
 *
 * It outlives the ask's Inbox row, which is what the Inbox reads it for. A question the server
 * withdraws, resolves or answers elsewhere leaves the list at once, except where the reader had
 * typed an answer to it: the reading column then keeps that question - its closed state, with
 * their text under it - until they select another row (`Inbox.tsx`, `paneAsk`; `AskCard.tsx`,
 * `unsentAnswer`). An answer the server records drops the draft, since it is no longer unsent.
 */
const drafts = new Map<string, string>();
const listeners = new Map<string, Set<() => void>>();

function setAnswerDraft(askId: string, text: string): void {
  if ((drafts.get(askId) ?? "") === text) return;
  if (text === "") drafts.delete(askId);
  else drafts.set(askId, text);
  for (const listener of listeners.get(askId) ?? []) listener();
}

/** What the reader has typed into the ask's answer box and not sent, read without subscribing to
 *  it: the Inbox asks at the commit that drops the row, not at every keystroke in the column. */
export function answerDraft(askId: string): string {
  return drafts.get(askId) ?? "";
}

/** The ask's draft and its setter; the component re-renders whenever any form changes it. An
 *  updater reads the draft as it is now, so a text added when an upload lands never overwrites
 *  what another upload or a keystroke put there since the render that started it. */
export function useAnswerDraft(askId: string): [string, Dispatch<SetStateAction<string>>] {
  const subscribe = useCallback(
    (listener: () => void) => {
      const askListeners = listeners.get(askId) ?? new Set<() => void>();
      listeners.set(askId, askListeners);
      askListeners.add(listener);
      return () => {
        askListeners.delete(listener);
        if (askListeners.size === 0) listeners.delete(askId);
      };
    },
    [askId]
  );
  const draft = useSyncExternalStore(subscribe, () => drafts.get(askId) ?? "");
  const setDraft = useCallback(
    (next: SetStateAction<string>) =>
      setAnswerDraft(askId, typeof next === "function" ? next(drafts.get(askId) ?? "") : next),
    [askId]
  );
  return [draft, setDraft];
}

/** Forgets every draft. The unit-test preload calls it after each test, since the store outlives
 *  any one render. */
export function resetAnswerDrafts(): void {
  for (const askId of [...drafts.keys()]) setAnswerDraft(askId, "");
}
