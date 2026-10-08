import type { InboxRow } from "../../api/types";
import { askOwner, type AskOwner } from "./ask-name";
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
 * Later rows, so a header can open that fold before it hands focus to a target row.
 */
export function crossBandCounts(
  bands: readonly InboxBand[]
): ReadonlyMap<InboxSection, ReadonlyMap<string, readonly CrossBandCount[]>> {
  const notes = new Map<InboxSection, ReadonlyMap<string, readonly CrossBandCount[]>>();

  for (const band of bands) {
    const byOwner = new Map<string, readonly CrossBandCount[]>();
    for (const group of groupRows(band.rows)) {
      if (group.owner.key === "none") continue;
      const counts: CrossBandCount[] = [];
      for (const other of bands) {
        if (other.section === band.section) continue;
        const count = other.rows.filter((row) => askOwner(row).key === group.owner.key).length;
        if (count > 0) counts.push({ count, section: other.section });
      }
      if (counts.length > 0) byOwner.set(group.owner.key, counts);
    }
    notes.set(band.section, byOwner);
  }

  return notes;
}
