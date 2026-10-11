import type { InboxRow } from "../../api/types";
import { type AskOwner, askOwner } from "./ask-name";
import type { InboxSection } from "./sections";

/** Rows of one Inbox owner that stay adjacent within one band. */
export interface InboxGroup {
  owner: AskOwner;
  rows: InboxRow[];
}

/** The rows of one band before its fold hides any of them from the DOM. */
export interface InboxBand {
  section: InboxSection;
  rows: readonly InboxRow[];
}

/** How many asks one owner has in another visible Inbox band. */
export interface CrossBandCount {
  count: number;
  section: InboxSection;
}

/**
 * Groups one band's rows in the server order. The first occurrence fixes a group's place; later
 * asks for that owner join it without changing their relative order. Unowned rows have no
 * meaningful common owner, so each remains a separate one-row group.
 */
export function groupRows(rows: readonly InboxRow[]): InboxGroup[] {
  const groups: InboxGroup[] = [];
  const byOwner = new Map<string, InboxGroup>();

  for (const row of rows) {
    const owner = askOwner(row);
    if (owner.key === "none") {
      groups.push({ owner, rows: [row] });
      continue;
    }
    const existing = byOwner.get(owner.key);
    if (existing === undefined) {
      const group = { owner, rows: [row] };
      byOwner.set(owner.key, group);
      groups.push(group);
    } else {
      existing.rows.push(row);
    }
  }

  return groups;
}

/**
 * Counts an owner's asks in every other band for each band it appears in. The input keeps folded
 * Later rows, so a header can open that fold before it hands focus to a target row. One pass
 * counts each band's owners; the groups themselves are the render's, so nothing groups twice.
 */
export function crossBandCounts(
  bands: readonly InboxBand[]
): ReadonlyMap<InboxSection, ReadonlyMap<string, readonly CrossBandCount[]>> {
  const owners = bands.map((band) => {
    const counts = new Map<string, number>();
    for (const row of band.rows) {
      const key = askOwner(row).key;
      if (key !== "none") counts.set(key, (counts.get(key) ?? 0) + 1);
    }
    return { counts, section: band.section };
  });
  const notes = new Map<InboxSection, ReadonlyMap<string, readonly CrossBandCount[]>>();
  for (const band of owners) {
    const byOwner = new Map<string, readonly CrossBandCount[]>();
    for (const key of band.counts.keys()) {
      const counts: CrossBandCount[] = [];
      for (const other of owners) {
        const count = other.section === band.section ? undefined : other.counts.get(key);
        if (count !== undefined) counts.push({ count, section: other.section });
      }
      if (counts.length > 0) byOwner.set(key, counts);
    }
    notes.set(band.section, byOwner);
  }
  return notes;
}
