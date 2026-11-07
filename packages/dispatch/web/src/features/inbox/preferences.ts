/**
 * The Inbox's browser-kept choices: the ones that belong to the person's browser rather than to
 * the list they linked. The page and the drawer read the same values, so the two agree, and a
 * reload keeps them. Everything the address can carry - the filters, the sort, the grouping and
 * the switch between the Inbox and what the reader has dismissed out of it - stays in the
 * address on the page and in `useState` in the drawer.
 *
 * The thread order is a browser-kept choice too, but it is no Inbox setting: every thread in
 * Dispatch reads in it, so it lives with the thread modules (`features/threads/thread-order.ts`).
 */

import { useCallback } from "react";

import type { InboxGroupBy } from "../refs/routes";
import { useUserPreference } from "../shell/userPreference";
import { DEFAULT_FOLDED_GROUPS } from "./grouping";

/** The folded bands of every grouping, as the preference stores them: one list of band keys per
 *  Group by value, so folding `P1` under Priority says nothing about the Project bands. */
type FoldedByGrouping = Partial<Record<InboxGroupBy, readonly string[]>>;

function readFolded(stored: string | null): FoldedByGrouping {
  if (stored === null) return {};
  try {
    const parsed: unknown = JSON.parse(stored);
    if (parsed === null || typeof parsed !== "object") return {};
    const folded: FoldedByGrouping = {};
    for (const [grouping, keys] of Object.entries(parsed)) {
      if (Array.isArray(keys)) {
        folded[grouping as InboxGroupBy] = keys.filter((key) => typeof key === "string");
      }
    }
    return folded;
  } catch {
    // A value another build wrote, or a person edited: the reader's folds are not worth a
    // broken Inbox, so the list opens with the default fold instead.
    return {};
  }
}

/**
 * Which bands of `by` are folded shut, and the toggle that folds one. **Later** starts folded -
 * its rows are the ones the reader already dealt with by deferring - and from the moment they
 * fold or open anything under this grouping, the set they left is the whole answer.
 */
export function useFoldedGroups(by: InboxGroupBy): [ReadonlySet<string>, (key: string) => void] {
  const [stored, setStored] = useUserPreference<FoldedByGrouping>(
    "inbox.folded-groups",
    readFolded,
    (value) => JSON.stringify(value)
  );
  const folded = new Set(stored[by] ?? DEFAULT_FOLDED_GROUPS);
  const toggle = useCallback(
    (key: string) => {
      const next = new Set(stored[by] ?? DEFAULT_FOLDED_GROUPS);
      if (!next.delete(key)) next.add(key);
      setStored({ ...stored, [by]: [...next] });
    },
    [by, setStored, stored]
  );
  return [folded, toggle];
}
