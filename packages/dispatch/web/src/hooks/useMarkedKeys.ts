import { type Dispatch, type SetStateAction, useCallback, useEffect, useState } from "react";

/** The marks a list holds, as the Inbox's rows and a project's issues both hold them. */
export interface MarkedKeys {
  /** The keys the reader has marked. Its identity changes exactly when its contents do, so it is
   *  itself a sound effect dependency. */
  readonly marked: ReadonlySet<string>;
  /** Marks the key, or unmarks it when it is already marked: `x` and a row's own checkbox are the
   *  two ways in. Stable, so a memoized row is not re-rendered by it. */
  readonly onMark: (key: string) => void;
  /** The marks themselves, for a caller that clears or rolls them back. */
  readonly setMarked: Dispatch<SetStateAction<ReadonlySet<string>>>;
}

/**
 * The keys of the rows a reader has marked. A key the list no longer holds drops out, so a row
 * that is listed again comes back unmarked; the mark itself outlives a refetch that lists it
 * still.
 *
 * `listedKeys` is the listed keys joined by a space - a string, so the pruning runs when the keys
 * change rather than on every render that rebuilds the list. `undefined` means the list has not
 * arrived yet, and prunes nothing.
 */
export function useMarkedKeys(listedKeys: string | undefined): MarkedKeys {
  const [marked, setMarked] = useState<ReadonlySet<string>>(() => new Set());
  const onMark = useCallback((key: string) => {
    setMarked((current) => {
      const next = new Set(current);
      if (next.has(key)) {
        next.delete(key);
      } else {
        next.add(key);
      }
      return next;
    });
  }, []);
  useEffect(() => {
    if (listedKeys === undefined) return;
    const listed = new Set(listedKeys === "" ? [] : listedKeys.split(" "));
    // The same set when nothing was pruned, so a reader of `marked` is not woken by a refetch
    // that listed the very same rows.
    setMarked((current) =>
      [...current].every((key) => listed.has(key))
        ? current
        : new Set([...current].filter((key) => listed.has(key)))
    );
  }, [listedKeys]);
  return { marked, onMark, setMarked };
}
