import { type Dispatch, type SetStateAction, useCallback, useSyncExternalStore } from "react";

import type { AcceptedMention } from "../conversation/composer-model";

/** One unsent draft: what the reader typed, and the mentions picked in it, whose offsets are into
 *  that exact text (`features/conversation/MentionField.tsx`). The two travel together because
 *  only the records reach the wire and only the text reaches the reader, so a store that kept the
 *  text alone would hand a reopened box prose where a mention stood. */
export interface Draft {
  readonly mentions: readonly AcceptedMention[];
  readonly text: string;
}

/** The empty draft, and the sentinel a store forgets a key by. One value for every empty key, so
 *  a box whose draft was dropped reads the same object each time and re-renders for none of it. */
export const EMPTY_DRAFT: Draft = { mentions: [], text: "" };

/** Whether a draft holds nothing: no text and no picked mention. */
export function draftEmpty(draft: Draft): boolean {
  return draft.text === "" && draft.mentions.length === 0;
}

/** Whether two drafts say the same thing, so a write that changes nothing tells nobody. */
function sameDraft(left: Draft, right: Draft): boolean {
  return (
    left.text === right.text &&
    left.mentions.length === right.mentions.length &&
    left.mentions.every((mention, index) => {
      const other = right.mentions[index];
      return (
        other !== undefined &&
        mention.target === other.target &&
        mention.text === other.text &&
        mention.start === other.start &&
        mention.end === other.end
      );
    })
  );
}

/** One keyed store of unsent drafts, and the four ways its callers reach it. */
export interface DraftStore {
  /** The key's draft, read without subscribing to it. */
  read(key: string): Draft;
  /** Forgets every draft. The unit-test preload calls it after each test, since the store
   *  outlives any one render. */
  reset(): void;
  /** Writes the key's draft and tells every field holding that key; an empty draft
   *  (`draftEmpty`) forgets it. */
  set(key: string, draft: Draft): void;
  /** The key's draft and its setter; the component re-renders whenever any field for that key
   *  changes it. An updater reads the draft as it is now, so a text added when an upload lands
   *  never overwrites what another upload or a keystroke put there since the render that
   *  started it. */
  useDraft(key: string): [Draft, Dispatch<SetStateAction<Draft>>];
}

/**
 * A store of unsent drafts, keyed by what the draft belongs to (an ask id): the draft belongs to
 * that thing rather than to the field showing it, so every field for one key reads and writes
 * the same draft, a field that remounts picks it back up - its mentions with its text - and the
 * draft outlives every render.
 *
 * Each of the app's draft stores is one of these (`answer-drafts.ts`, `reply-drafts.ts`), so a
 * fix to the subscription, the listener cleanup or the updater reaches both. The map is in
 * memory, so no draft an older build wrote is ever read and nothing has to be migrated.
 */
export function createDraftStore(): DraftStore {
  const drafts = new Map<string, Draft>();
  const listeners = new Map<string, Set<() => void>>();

  const read = (key: string): Draft => drafts.get(key) ?? EMPTY_DRAFT;

  const set = (key: string, draft: Draft): void => {
    if (sameDraft(read(key), draft)) return;
    if (draftEmpty(draft)) drafts.delete(key);
    else drafts.set(key, draft);
    for (const listener of listeners.get(key) ?? []) listener();
  };

  function useDraft(key: string): [Draft, Dispatch<SetStateAction<Draft>>] {
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
    const draft = useSyncExternalStore(subscribe, () => read(key));
    const setDraft = useCallback(
      (next: SetStateAction<Draft>) =>
        set(key, typeof next === "function" ? next(read(key)) : next),
      [key]
    );
    return [draft, setDraft];
  }

  return {
    read,
    reset: () => {
      for (const key of [...drafts.keys()]) set(key, EMPTY_DRAFT);
    },
    set,
    useDraft,
  };
}
