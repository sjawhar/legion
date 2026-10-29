import { diffArrays, diffWordsWithSpace } from "diff";
import { type ReactNode, useMemo, useState } from "react";

import { DisclosureToggle } from "../../components/DisclosureToggle";
import {
  card,
  diffAddedBg,
  diffAddedText,
  diffRemovedBg,
  diffRemovedText,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { type DocumentBlock, splitBlocks } from "./document-blocks";

/** How a block stands between the two versions. */
type ChangeKind = "unchanged" | "changed" | "removed" | "added";

/**
 * One block of the change: the block as it stands in `before`, in `after`, or in both, and the
 * heading of the section a change sits in (the nearest heading before it, which may be folded
 * away or outside the view).
 */
interface ChangeEntry {
  kind: ChangeKind;
  before: DocumentBlock | undefined;
  after: DocumentBlock | undefined;
  heading: string | undefined;
}

/** A word run of a changed block: `same` is unmarked, `removed` a `del`, `added` an `ins`. */
interface WordPart {
  key: string;
  kind: "same" | "removed" | "added";
  value: string;
}

/** One rendered row: a block, a folded run's control, or a heading drawn for context. */
type ChangeRow =
  | { key: string; kind: "block"; change: ChangeKind; source: string; words: WordPart[] }
  | { key: string; kind: "fold"; run: number; count: number; open: boolean }
  | { key: string; kind: "heading"; source: string };

/** The source of the heading whose section the block at `index` sits in, when it has one. */
function sectionHeading(blocks: readonly DocumentBlock[], index: number): string | undefined {
  for (let i = Math.min(index, blocks.length) - 1; i >= 0; i -= 1) {
    if (blocks[i].kind === "heading") return blocks[i].source;
  }
  return undefined;
}

/**
 * The two block lists paired up, one entry per block of the change. `diffArrays` pairs them on
 * the block's whole source, and a run it reports as removed and added at one place is read as
 * blocks that changed there: the first removed block pairs with the first added one and so on,
 * and whichever side is longer leaves its remainder removed or added on its own.
 */
function pairBlocks(
  beforeBlocks: readonly DocumentBlock[],
  afterBlocks: readonly DocumentBlock[]
): ChangeEntry[] {
  const parts = diffArrays(
    beforeBlocks.map((block) => block.source),
    afterBlocks.map((block) => block.source)
  );
  const entries: ChangeEntry[] = [];
  let beforeIndex = 0;
  let afterIndex = 0;
  let part = 0;
  while (part < parts.length) {
    const current = parts[part];
    if (!current.added && !current.removed) {
      for (let i = 0; i < current.value.length; i += 1) {
        entries.push({
          kind: "unchanged",
          before: beforeBlocks[beforeIndex],
          after: afterBlocks[afterIndex],
          heading: undefined,
        });
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
      entries.push({
        kind: "changed",
        before: beforeBlocks[beforeIndex],
        after: afterBlocks[afterIndex],
        heading: sectionHeading(afterBlocks, afterIndex),
      });
      beforeIndex += 1;
      afterIndex += 1;
    }
    for (let i = paired; i < removedCount; i += 1) {
      entries.push({
        kind: "removed",
        before: beforeBlocks[beforeIndex],
        after: undefined,
        heading: sectionHeading(beforeBlocks, beforeIndex),
      });
      beforeIndex += 1;
    }
    for (let i = paired; i < addedCount; i += 1) {
      entries.push({
        kind: "added",
        before: undefined,
        after: afterBlocks[afterIndex],
        heading: sectionHeading(afterBlocks, afterIndex),
      });
      afterIndex += 1;
    }
  }
  return entries;
}

/**
 * Which entries fold away: every run of unchanged blocks longer than two, less one block of
 * context on each side of the changes it stands between. A run that opens the view has no
 * change above it and one that closes it none below, so that side keeps no context. Nothing
 * folds while nothing changed: a change view of two equal texts is the document itself.
 */
function foldedEntries(entries: readonly ChangeEntry[]): boolean[] {
  const folded = entries.map(() => false);
  if (entries.every((entry) => entry.kind === "unchanged")) return folded;
  let i = 0;
  while (i < entries.length) {
    if (entries[i].kind !== "unchanged") {
      i += 1;
      continue;
    }
    let end = i;
    while (end < entries.length && entries[end].kind === "unchanged") end += 1;
    const leading = i > 0 ? 1 : 0;
    const trailing = end < entries.length ? 1 : 0;
    if (end - i > 2) {
      for (let k = i + leading; k < end - trailing; k += 1) folded[k] = true;
    }
    i = end;
  }
  return folded;
}

/** A changed block's words: what `before` lost in a `del`, what `after` gained in an `ins`. */
function changedWords(before: string, after: string, key: string): WordPart[] {
  return diffWordsWithSpace(before, after).map((part, index) => ({
    key: `${key}:word:${index}`,
    kind: part.added ? "added" : part.removed ? "removed" : "same",
    value: part.value,
  }));
}

/** The entry's rendered row: the text it shows and, for a changed block, its marked words. */
function blockRow(entry: ChangeEntry, key: string): ChangeRow {
  if (entry.kind === "changed" && entry.before !== undefined && entry.after !== undefined) {
    return {
      key,
      kind: "block",
      change: "changed",
      source: entry.after.source,
      words: changedWords(entry.before.source, entry.after.source, key),
    };
  }
  const block = entry.after ?? entry.before;
  return {
    key,
    kind: "block",
    change: entry.kind,
    source: block === undefined ? "" : block.source,
    words: [],
  };
}

/**
 * The rows to render, in the document's order: every entry that is not folded away, one control
 * per folded run (holding its blocks when the reader opened it), and, above a change whose
 * section heading no visible block above it already shows, that heading.
 */
function buildRows(
  entries: readonly ChangeEntry[],
  fold: boolean,
  opened: ReadonlySet<number>
): ChangeRow[] {
  const folded = fold ? foldedEntries(entries) : entries.map(() => false);
  const rows: ChangeRow[] = [];
  let shownHeading: string | undefined;
  let run = 0;
  let i = 0;
  const show = (entry: ChangeEntry, index: number): void => {
    if (
      entry.kind !== "unchanged" &&
      entry.heading !== undefined &&
      entry.heading !== shownHeading
    ) {
      rows.push({ key: `heading:${index}`, kind: "heading", source: entry.heading });
      shownHeading = entry.heading;
    }
    rows.push(blockRow(entry, `block:${index}`));
    const block = entry.after ?? entry.before;
    if (block?.kind === "heading") shownHeading = block.source;
  };
  while (i < entries.length) {
    if (!folded[i]) {
      show(entries[i], i);
      i += 1;
      continue;
    }
    let end = i;
    while (end < entries.length && folded[end]) end += 1;
    const index = run;
    run += 1;
    const open = opened.has(index);
    rows.push({ key: `fold:${index}`, kind: "fold", run: index, count: end - i, open });
    if (open) {
      for (let k = i; k < end; k += 1) show(entries[k], k);
    }
    i = end;
  }
  return rows;
}

export interface ChangeViewProps {
  before: string;
  after: string;
  fold?: boolean;
}

const INSERTED = `no-underline ${diffAddedBg} ${diffAddedText}`;
const DELETED = `line-through ${diffRemovedBg} ${diffRemovedText}`;

/**
 * What changed between two versions of a document, as the document's own source with marks: a
 * block both versions hold stands unmarked, a block that changed shows the words it lost in a
 * `del` and the words it gained in an `ins` with the rest of it unmarked, a block only `before`
 * holds is struck through where it stood, and a block only `after` holds is marked as added.
 *
 * Each block carries its standing as `data-change`, so a reader of the DOM - a test, a style -
 * names a change without reading its marks. With `fold` (the default) every run of unchanged
 * blocks longer than two hides behind one control, one block of context on each side of each
 * change, and the heading of the section a change sits in is drawn above it when the text above
 * does not already show it; `fold={false}` shows every block, in order.
 */
export function ChangeView({ after, before, fold = true }: ChangeViewProps): ReactNode {
  const [opened, setOpened] = useState<ReadonlySet<number>>(() => new Set<number>());
  const entries = useMemo(
    () => pairBlocks(splitBlocks(before), splitBlocks(after)),
    [after, before]
  );
  const rows = useMemo(() => buildRows(entries, fold, opened), [entries, fold, opened]);
  const toggle = (run: number): void =>
    setOpened((current) => {
      const next = new Set(current);
      if (!next.delete(run)) next.add(run);
      return next;
    });
  return (
    <div
      className={`[overflow-wrap:anywhere] space-y-2 rounded-lg p-4 text-sm leading-6 ${card}`}
      data-testid="change-view"
    >
      {rows.map((row) => {
        if (row.kind === "fold") {
          return (
            <DisclosureToggle
              expanded={row.open}
              key={row.key}
              label={`${row.open ? "Hide" : "Show"} ${row.count} unchanged blocks`}
              onToggle={() => toggle(row.run)}
              textClassName={textSecondaryOnSurface}
            />
          );
        }
        if (row.kind === "heading") {
          return (
            <pre
              className={`whitespace-pre-wrap ${textSecondaryOnSurface}`}
              data-testid="change-section"
              key={row.key}
            >
              {row.source}
            </pre>
          );
        }
        if (row.change === "changed") {
          return (
            <pre className="whitespace-pre-wrap" data-change="changed" key={row.key}>
              {row.words.map((word) => {
                if (word.kind === "added") {
                  return (
                    <ins className={INSERTED} key={word.key}>
                      {word.value}
                    </ins>
                  );
                }
                if (word.kind === "removed") {
                  return (
                    <del className={DELETED} key={word.key}>
                      {word.value}
                    </del>
                  );
                }
                return <span key={word.key}>{word.value}</span>;
              })}
            </pre>
          );
        }
        if (row.change === "added") {
          return (
            <pre className="whitespace-pre-wrap" data-change="added" key={row.key}>
              <ins className={`block ${INSERTED}`}>{row.source}</ins>
            </pre>
          );
        }
        if (row.change === "removed") {
          return (
            <pre className="whitespace-pre-wrap" data-change="removed" key={row.key}>
              <del className={`block ${DELETED}`}>{row.source}</del>
            </pre>
          );
        }
        return (
          <pre className="whitespace-pre-wrap" data-change="unchanged" key={row.key}>
            {row.source}
          </pre>
        );
      })}
    </div>
  );
}
