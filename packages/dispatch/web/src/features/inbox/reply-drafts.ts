import { type Dispatch, type SetStateAction, useCallback, useSyncExternalStore } from "react";

/**
 * Unsent ask-reply drafts, by ask id. A draft belongs to the ask, not to the composer showing it:
 * every composer for one ask reads and writes the same draft (the margin's inline composer and a
 * Conversation or decision-block composer can be on one page at once), and a composer that
 * remounts (switching margin tabs away and back, closing and reopening Write a reply) picks the
 * draft back up. A sent reply, an emptied field, or the ask resolving drops it.
 */
const drafts = new Map<string, string>();
const listeners = new Map<string, Set<() => void>>();

function setReplyDraft(askId: string, text: string): void {
  if ((drafts.get(askId) ?? "") === text) return;
  if (text === "") drafts.delete(askId);
  else drafts.set(askId, text);
  for (const listener of listeners.get(askId) ?? []) listener();
}

/** Drops an ask's draft; with `sent`, only while the draft is still the text that was sent. */
export function clearReplyDraft(askId: string, sent?: string): void {
  const draft = drafts.get(askId);
  if (draft === undefined || (sent !== undefined && draft.trim() !== sent)) return;
  setReplyDraft(askId, "");
}

/** The ask's draft and its setter; the component re-renders whenever any composer changes it. An
 *  updater reads the draft as it is now, so a text added when an upload lands never overwrites
 *  what another upload or a keystroke put there since the render that started it. */
export function useReplyDraft(askId: string): [string, Dispatch<SetStateAction<string>>] {
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
      setReplyDraft(askId, typeof next === "function" ? next(drafts.get(askId) ?? "") : next),
    [askId]
  );
  return [draft, setDraft];
}

/** Forgets every draft. The unit-test preload calls it after each test, since the store outlives
 *  any one render. */
export function resetReplyDrafts(): void {
  for (const askId of [...drafts.keys()]) setReplyDraft(askId, "");
}
