import { type Dispatch, type SetStateAction, useCallback, useSyncExternalStore } from "react";

/** One keyed store of unsent text, and the four ways its callers reach it. */
export interface DraftStore {
  /** The key's draft, read without subscribing to it. */
  read(key: string): string;
  /** Forgets every draft. The unit-test preload calls it after each test, since the store
   *  outlives any one render. */
  reset(): void;
  /** Writes the key's draft and tells every field holding that key; `""` forgets the draft. */
  set(key: string, text: string): void;
  /** The key's draft and its setter; the component re-renders whenever any field for that key
   *  changes it. An updater reads the draft as it is now, so a text added when an upload lands
   *  never overwrites what another upload or a keystroke put there since the render that
   *  started it. */
  useDraft(key: string): [string, Dispatch<SetStateAction<string>>];
}

/**
 * A store of unsent text, keyed by what the text belongs to (an ask id): the draft belongs to
 * that thing rather than to the field showing it, so every field for one key reads and writes
 * the same draft, a field that remounts picks it back up, and the draft outlives every render.
 *
 * Each of the app's draft stores is one of these (`answer-drafts.ts`, `reply-drafts.ts`), so a
 * fix to the subscription, the listener cleanup or the updater reaches both.
 */
export function createDraftStore(): DraftStore {
  const drafts = new Map<string, string>();
  const listeners = new Map<string, Set<() => void>>();

  const set = (key: string, text: string): void => {
    if ((drafts.get(key) ?? "") === text) return;
    if (text === "") drafts.delete(key);
    else drafts.set(key, text);
    for (const listener of listeners.get(key) ?? []) listener();
  };

  function useDraft(key: string): [string, Dispatch<SetStateAction<string>>] {
    const subscribe = useCallback(
      (listener: () => void) => {
        const keyListeners = listeners.get(key) ?? new Set<() => void>();
        listeners.set(key, keyListeners);
        keyListeners.add(listener);
        return () => {
          keyListeners.delete(listener);
          if (keyListeners.size === 0) listeners.delete(key);
        };
      },
      [key]
    );
    const draft = useSyncExternalStore(subscribe, () => drafts.get(key) ?? "");
    const setDraft = useCallback(
      (next: SetStateAction<string>) =>
        set(key, typeof next === "function" ? next(drafts.get(key) ?? "") : next),
      [key]
    );
    return [draft, setDraft];
  }

  return {
    read: (key) => drafts.get(key) ?? "",
    reset: () => {
      for (const key of [...drafts.keys()]) set(key, "");
    },
    set,
    useDraft,
  };
}
