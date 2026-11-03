import type { ReactNode } from "react";

import { DisclosureToggle } from "../../components/DisclosureToggle";
import { borderDefault, linkHoverText, linkText, textMutedOnCanvas } from "../../theme/classes";
import type { InboxGroup } from "./grouping";
import type { InboxListRow } from "./list-row";

/** How many rows a band draws at once, and how many more each **Show more** adds. A band of a
 *  thousand threads is a scroll nobody asked for, and the rows behind the first page are one
 *  press away. */
export const GROUP_PAGE = 50;

/**
 * One band as items of the Inbox's single list: its heading, the rows it is drawing, and
 * **Show more** while it is holding any back. A plain function rather than a component, so
 * every row stays a direct child of the one keyed `<ul>` and a row that changes band moves
 * there instead of remounting under another parent.
 *
 * The caller decides which rows are drawn - a folded band draws almost none, and a long one
 * draws its first page - because the same rule decides what the selection steps through and
 * what the marks count.
 */
export function groupItems({
  folded,
  group,
  held,
  onShowMore,
  onToggleFold,
  renderRow,
  rows,
}: {
  /** Whether the reader has folded this band shut. */
  folded: boolean;
  group: InboxGroup;
  /** How many of the band's rows are behind **Show more**; none draws no button. */
  held: number;
  onShowMore: (key: string) => void;
  onToggleFold: (key: string) => void;
  renderRow: (row: InboxListRow) => ReactNode;
  /** The rows to draw, in order. */
  rows: readonly InboxListRow[];
}): ReactNode[] {
  const heading =
    group.label === undefined ? null : (
      <li className="pt-2" key={`heading-${group.key}`} role="presentation">
        {/* A band divider, not a heading that competes with the rows under it. */}
        <h2 className={`text-xs font-semibold tracking-wide uppercase ${textMutedOnCanvas}`}>
          <DisclosureToggle
            expanded={!folded}
            label={`${group.label} (${group.rows.length})`}
            onToggle={() => onToggleFold(group.key)}
            textClassName={textMutedOnCanvas}
          />
        </h2>
      </li>
    );
  return [
    heading,
    ...rows.map((row) => renderRow(row)),
    held > 0 && !folded ? (
      <li key={`more-${group.key}`} role="presentation">
        <button
          className={`min-h-11 rounded-lg border px-3 py-1.5 text-sm font-medium ${borderDefault} ${linkText} ${linkHoverText}`}
          data-inbox-show-more={group.key}
          onClick={() => onShowMore(group.key)}
          type="button"
        >
          Show {Math.min(held, GROUP_PAGE)} more
          {group.label === undefined ? "" : ` in ${group.label}`}
        </button>
      </li>
    ) : null,
  ];
}
