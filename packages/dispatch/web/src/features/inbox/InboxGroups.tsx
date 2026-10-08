import { type ReactNode, type RefObject, useLayoutEffect, useState } from "react";

import type { InboxRow } from "../../api/types";
import { linkHoverText, linkText, textSecondaryOnCanvas } from "../../theme/classes";
import { InboxOwnerLink } from "./AskOwnerLink";
import type { CrossBandCount, InboxGroup } from "./grouping";
import { type InboxSection, SECTION_TITLES } from "./sections";

/** A visual grouping label only: rows stay the flat list's keyboard and viewport targets. */
function InboxGroupHeader({
  group,
  notes,
  onJump,
}: {
  group: InboxGroup;
  notes: readonly CrossBandCount[];
  onJump: (section: InboxSection, ownerKey: string) => void;
}): ReactNode {
  const first = group.rows[0];
  if (first === undefined) return null;
  return (
    <li data-inbox-group-header={group.owner.key} role="presentation">
      <div className="flex min-w-0 flex-wrap items-baseline gap-x-1 gap-y-1 text-sm">
        <InboxOwnerLink ask={first} rowOwner={false} />
        <span className={textSecondaryOnCanvas}>· {group.rows.length} asks</span>
        {notes.map((note) => {
          const title = SECTION_TITLES[note.section];
          return (
            <button
              aria-label={`Jump to ${group.owner.label}'s asks ${title}`}
              className={`text-xs ${linkText} ${linkHoverText}`}
              key={note.section}
              onClick={() => onJump(note.section, group.owner.key)}
              type="button"
            >
              {note.count} more {title.toLowerCase()}
            </button>
          );
        })}
      </div>
    </li>
  );
}

/**
 * One band's groups as items of the Inbox's single list: a group of two or more gets its header
 * first, then every row through `renderRow`. A plain function rather than a component, so each
 * row stays a direct child of the one keyed `<ul>` and a row that changes band moves there
 * instead of remounting under another parent.
 */
export function bandGroupItems({
  groups,
  notes,
  onJump,
  renderRow,
  section,
}: {
  groups: readonly InboxGroup[];
  /** The cross-band counts of this band's owners, by owner key. */
  notes: ReadonlyMap<string, readonly CrossBandCount[]> | undefined;
  onJump: (section: InboxSection, ownerKey: string) => void;
  renderRow: (ask: InboxRow, group: InboxGroup) => ReactNode;
  section: InboxSection;
}): ReactNode[] {
  return groups.flatMap((group) => [
    group.rows.length > 1 ? (
      <InboxGroupHeader
        group={group}
        key={`group-${section}-${group.owner.key}`}
        notes={notes?.get(group.owner.key) ?? []}
        onJump={onJump}
      />
    ) : null,
    ...group.rows.map((ask) => renderRow(ask, group)),
  ]);
}

/**
 * A header's `N more <band>` jump: opens `Later` when that is the band, then scrolls to and
 * focuses the owner's first row there once it has rendered. Every row carries its owner key and
 * section, so a group of one, which has no header, is still a target.
 */
export function useGroupJump(
  listRef: RefObject<HTMLElement | null>,
  openLater: () => void
): (section: InboxSection, ownerKey: string) => void {
  const [jumpTo, setJumpTo] = useState<{ ownerKey: string; section: InboxSection } | null>(null);
  useLayoutEffect(() => {
    if (jumpTo === null) return;
    const target = [
      ...(listRef.current?.querySelectorAll<HTMLElement>("[data-inbox-owner-key]") ?? []),
    ].find(
      (row) =>
        row.dataset.inboxOwnerKey === jumpTo.ownerKey && row.dataset.inboxSection === jumpTo.section
    );
    setJumpTo(null);
    if (target === undefined) return;
    target.scrollIntoView({ block: "center" });
    target.focus();
  }, [jumpTo, listRef]);
  return (section, ownerKey) => {
    setJumpTo({ ownerKey, section });
    if (section === "later") openLater();
  };
}
