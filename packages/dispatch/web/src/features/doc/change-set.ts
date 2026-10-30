import { diffArrays, diffWordsWithSpace } from "diff";

/**
 * How two versions of a document are paired up, block by block, and how a block that changed
 * between them differs. The change view (`ChangeView.tsx`) pairs the two versions' markdown block
 * sources; the reading pane's approval marks (`change-marks.ts`) pair the rendered blocks' text.
 * Both read the same pairing and the same word diff, so the two surfaces mark the same words.
 */

/** How a block stands between the two versions. */
export type ChangeKind = "unchanged" | "changed" | "removed" | "added";

/** One block of the change: where it stands in each version, `undefined` on a side that does not
 *  hold it. */
export interface ChangePair {
  kind: ChangeKind;
  before: number | undefined;
  after: number | undefined;
}

/** A word run of a changed block: `same` is unmarked, `removed` what the earlier version held,
 *  `added` what the later one gained. */
export interface WordPart {
  kind: "same" | "removed" | "added";
  value: string;
}

/**
 * The two block lists paired up, one entry per block of the change. `diffArrays` pairs them on the
 * block's whole text, and a run it reports as removed and added at one place is read as blocks
 * that changed there: the first removed block pairs with the first added one and so on, and
 * whichever side is longer leaves its remainder removed or added on its own.
 */
export function pairBlocks(before: readonly string[], after: readonly string[]): ChangePair[] {
  // `diffArrays` declares two mutable arrays and mutates neither of them.
  const parts = diffArrays(before as string[], after as string[]);
  const pairs: ChangePair[] = [];
  let beforeIndex = 0;
  let afterIndex = 0;
  let part = 0;
  while (part < parts.length) {
    const current = parts[part];
    if (!current.added && !current.removed) {
      for (let i = 0; i < current.value.length; i += 1) {
        pairs.push({ kind: "unchanged", before: beforeIndex, after: afterIndex });
        beforeIndex += 1;
        afterIndex += 1;
      }
      part += 1;
      continue;
    }
    let removedCount = 0;
    let addedCount = 0;
    while (part < parts.length && (parts[part].added || parts[part].removed)) {
      if (parts[part].added) addedCount += parts[part].value.length;
      else removedCount += parts[part].value.length;
      part += 1;
    }
    const paired = Math.min(removedCount, addedCount);
    for (let i = 0; i < paired; i += 1) {
      pairs.push({ kind: "changed", before: beforeIndex, after: afterIndex });
      beforeIndex += 1;
      afterIndex += 1;
    }
    for (let i = paired; i < removedCount; i += 1) {
      pairs.push({ kind: "removed", before: beforeIndex, after: undefined });
      beforeIndex += 1;
    }
    for (let i = paired; i < addedCount; i += 1) {
      pairs.push({ kind: "added", before: undefined, after: afterIndex });
      afterIndex += 1;
    }
  }
  return pairs;
}

/** A changed block's words: what `before` lost, what `after` gained, and the rest unmarked. The
 *  runs are in `after`'s order, so their lengths add up to `after` once the removed ones are left
 *  out, which is what puts an added run at an offset into the later version's text. */
export function changedWords(before: string, after: string): WordPart[] {
  return diffWordsWithSpace(before, after).map((part) => ({
    kind: part.added ? "added" : part.removed ? "removed" : "same",
    value: part.value,
  }));
}
